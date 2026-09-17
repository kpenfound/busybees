// Command seatbelt-check runs one confined host turn under macOS's Seatbelt
// confiner and reports what the turn could reach. No test runs it: the gate
// runs on Linux. CONTRIBUTING.md has the command. The agent is a shell
// script that tries what a confined turn must not do, and a few things it
// must; the program exits 1 when any of them came out otherwise.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
)

const probes = `cat >/dev/null
probe() {
  name="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "$name allowed"; else echo "$name denied"; fi >> {out}/probes.txt
}
probe read-mount cat {work}/readme.txt
probe read-outside cat {outside}/secret.txt
probe list-outside ls {outside}
probe create-in-workdir sh -c 'echo x > {work}/new.txt'
probe overwrite-in-workdir sh -c 'echo x > {work}/readme.txt'
probe remove-in-workdir rm {work}/readme.txt
probe create-in-writable sh -c 'echo x > {out}/new.txt'
probe create-outside sh -c 'echo x > {outside}/new.txt'
probe system-tool /usr/bin/env true
probe git-by-name git --version
probe git-by-path /usr/bin/git --version
probe git-by-shell sh -c 'exec /usr/bin/git --version'
probe git-read cat /usr/bin/git
echo '{"type":"result","subtype":"success"}'
`

func main() {
	sandbox := flag.String("sandbox", agent.SandboxNone, "host sandbox of the turn: none or claude")
	vcsGranted := flag.Bool("vcs", false, "grant VCS, which leaves git reachable")
	flag.Parse()
	if runtime.GOOS != "darwin" {
		fail("seatbelt-check runs on macOS; this is %s", runtime.GOOS)
	}

	tmp, err := os.MkdirTemp("", "seatbelt-check")
	check(err)
	defer func() { _ = os.RemoveAll(tmp) }()
	base, err := filepath.EvalSymlinks(tmp)
	check(err)
	dir := func(name string) string {
		p := filepath.Join(base, name)
		check(os.Mkdir(p, 0o755))
		return p
	}
	work, session, out, outside, bin := dir("work"), dir("session"), dir("out"), dir("outside"), dir("bin")
	check(os.WriteFile(filepath.Join(work, "readme.txt"), []byte("pinned\n"), 0o644))
	check(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644))
	script := strings.NewReplacer("{work}", work, "{out}", out, "{outside}", outside).Replace(probes)
	claude := filepath.Join(bin, "claude")
	check(os.WriteFile(claude, []byte("#!/bin/sh\n"+script), 0o755))

	r := &agent.Runner{ClaudeBin: claude, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	res, err := r.Run(context.Background(), agent.Request{
		Workspace:  vcs.Directory(work),
		SessionDir: session,
		Profile:    agent.Profile{Name: "seatbelt-check", Sandbox: *sandbox, Confine: true, VCSAccess: *vcsGranted},
		Grants: &agent.Grants{
			Env:   []string{"PATH"},
			Tools: []string{agent.ToolsAll},
			VCS:   *vcsGranted,
			Mounts: []agent.Mount{
				{Path: work, Access: agent.ReadOnly},
				{Path: session, Access: agent.ReadWrite},
				{Path: out, Access: agent.ReadWrite},
			},
		},
	})
	if err != nil {
		fail("run: %v", err)
	}
	if res.IsError {
		stderr, _ := os.ReadFile(filepath.Join(session, "stderr.log"))
		fail("the turn failed (%s): %s", res.ErrorSubtype, stderr)
	}

	git := "denied"
	if *vcsGranted {
		git = "allowed"
	}
	want := map[string]string{
		"read-mount":           "allowed",
		"read-outside":         "denied",
		"list-outside":         "denied",
		"create-in-workdir":    "denied",
		"overwrite-in-workdir": "denied",
		"remove-in-workdir":    "denied",
		"create-in-writable":   "allowed",
		"create-outside":       "denied",
		"system-tool":          "allowed",
		"git-by-name":          git,
		"git-by-path":          git,
		"git-by-shell":         git,
		"git-read":             git,
	}
	data, err := os.ReadFile(filepath.Join(out, "probes.txt"))
	check(err)
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if name, verdict, ok := strings.Cut(line, " "); ok {
			got[name] = verdict
		}
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	slices.Sort(names)
	failed := false
	for _, name := range names {
		mark := "ok"
		if got[name] != want[name] {
			mark, failed = "WRONG", true
		}
		fmt.Printf("%-5s %-21s %-8s (want %s)\n", mark, name, got[name], want[name])
	}
	if text, err := os.ReadFile(filepath.Join(work, "readme.txt")); err != nil || string(text) != "pinned\n" {
		fmt.Printf("WRONG the read-only working directory changed: %q, %v\n", text, err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "seatbelt-check: "+format+"\n", args...)
	os.Exit(1)
}
