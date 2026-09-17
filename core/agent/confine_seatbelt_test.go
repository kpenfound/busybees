package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// sbplRule is one line of a profile seatbeltProfile writes.
type sbplRule struct {
	allow bool
	ops   []string
	paths []string // none: every path
}

var sbplSubpath = regexp.MustCompile(`\(subpath "((?:[^"\\]|\\.)*)"\)`)

// parseSBPL reads the profiles seatbeltProfile writes, one rule a line, and
// fails on anything else.
func parseSBPL(t *testing.T, profile string) []sbplRule {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(profile, "\n"), "\n")
	if len(lines) == 0 || lines[0] != "(version 1)" {
		t.Fatalf("profile does not start with its version:\n%s", profile)
	}
	var rules []sbplRule
	for _, line := range lines[1:] {
		body, ok := strings.CutPrefix(line, "(")
		body, ok2 := strings.CutSuffix(body, ")")
		if !ok || !ok2 {
			t.Fatalf("not a rule: %q", line)
		}
		head, _, _ := strings.Cut(body, " (")
		fields := strings.Fields(head)
		if len(fields) < 2 || (fields[0] != "allow" && fields[0] != "deny") {
			t.Fatalf("not a rule: %q", line)
		}
		r := sbplRule{allow: fields[0] == "allow", ops: fields[1:]}
		rest := strings.TrimPrefix(body, head)
		for _, m := range sbplSubpath.FindAllStringSubmatch(rest, -1) {
			r.paths = append(r.paths, strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(m[1]))
			rest = strings.Replace(rest, m[0], "", 1)
		}
		if strings.TrimSpace(rest) != "" {
			t.Fatalf("rule %q has a filter this reader does not know: %q", line, rest)
		}
		rules = append(rules, r)
	}
	return rules
}

// sbplAllows decides op on path the way Seatbelt does: the last rule that
// matches wins.
func sbplAllows(rules []sbplRule, op, path string) bool {
	allowed := false
	for _, r := range rules {
		opMatch := slices.ContainsFunc(r.ops, func(o string) bool {
			if o == "default" || o == op {
				return true
			}
			prefix, wild := strings.CutSuffix(o, "*")
			return wild && strings.HasPrefix(op, prefix)
		})
		pathMatch := len(r.paths) == 0 || slices.ContainsFunc(r.paths, func(p string) bool { return inside(p, path) })
		if opMatch && pathMatch {
			allowed = r.allow
		}
	}
	return allowed
}

// seatbeltLayout is a machine in miniature for a profile: a read-only
// working directory with a writable scratch directory and a read-only
// directory inside that, a writable session directory, a system directory
// holding a denied git and a hard link to it, a writable device, and a
// directory nothing grants.
type seatbeltLayout struct {
	work, scratch, pinned, session, system, git, gitLink, gitCore, device, outside string
}

func newSeatbeltLayout(t *testing.T) (seatbeltLayout, Confinement) {
	t.Helper()
	base := realTemp(t)
	l := seatbeltLayout{
		work:    filepath.Join(base, "work"),
		session: filepath.Join(base, "session"),
		system:  filepath.Join(base, "usr"),
		device:  filepath.Join(base, "dev", "null"),
		outside: filepath.Join(base, "outside"),
	}
	l.scratch = filepath.Join(l.work, "scratch")
	l.pinned = filepath.Join(l.scratch, "pinned")
	l.git = filepath.Join(l.system, "bin", "git")
	l.gitLink = filepath.Join(l.system, "bin", "git-upload-pack")
	l.gitCore = filepath.Join(l.system, "libexec", "git-core")
	for _, dir := range []string{l.pinned, l.session, filepath.Dir(l.git), l.gitCore, filepath.Dir(l.device), l.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{l.git, filepath.Join(l.system, "bin", "cat"), filepath.Join(l.gitCore, "git-fetch"), l.device} {
		writeExecutable(t, f, "true\n")
	}
	if err := os.Link(l.git, l.gitLink); err != nil {
		t.Fatal(err)
	}
	return l, Confinement{
		Sandbox: SandboxNone,
		Mounts: []Mount{
			{Path: l.pinned, Access: ReadOnly},
			{Path: l.work, Access: ReadOnly},
			{Path: l.session, Access: ReadWrite},
			{Path: l.scratch, Access: ReadWrite},
		},
		System: []Mount{{Path: l.system, Access: ReadOnly}, {Path: l.device, Access: ReadWrite}},
		Denied: []string{l.git, l.gitCore},
	}
}

func TestSeatbeltProfileReadsOnlyMountsAndSystemPaths(t *testing.T) {
	l, c := newSeatbeltLayout(t)
	profile, err := seatbeltProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	rules := parseSBPL(t, profile)
	for path, want := range map[string]bool{
		filepath.Join(l.work, "main.go"):               true,
		filepath.Join(l.pinned, "file"):                true,
		filepath.Join(l.session, "prompt.md"):          true,
		filepath.Join(l.system, "bin", "cat"):          true,
		l.device:                                       true,
		filepath.Join(l.outside, "secret"):             false,
		filepath.Dir(l.work):                           false,
		"/":                                            false,
		filepath.Join(filepath.Dir(l.device), "disk0"): false,
	} {
		for _, op := range []string{"file-read-data", "process-exec"} {
			if got := sbplAllows(rules, op, path); got != want {
				t.Errorf("%s %s = %v, want %v\n%s", op, path, got, want, profile)
			}
		}
		// A file's metadata is readable everywhere, as under Landlock.
		if !sbplAllows(rules, "file-read-metadata", path) {
			t.Errorf("file-read-metadata %s refused", path)
		}
	}
	// What the profile leaves alone stays as the process has it.
	if !sbplAllows(rules, "network-outbound", l.outside) || !sbplAllows(rules, "mach-lookup", "") {
		t.Errorf("the profile confines more than the filesystem:\n%s", profile)
	}
}

func TestSeatbeltProfileWritesOnlyReadWriteMounts(t *testing.T) {
	l, c := newSeatbeltLayout(t)
	profile, err := seatbeltProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	rules := parseSBPL(t, profile)
	for path, want := range map[string]bool{
		filepath.Join(l.session, "result.json"): true,
		filepath.Join(l.scratch, "out"):         true, // read-write inside read-only
		l.device:                                true,
		filepath.Join(l.work, "main.go"):        false,
		filepath.Join(l.pinned, "file"):         false, // read-only inside read-write
		filepath.Join(l.system, "bin", "cat"):   false,
		filepath.Join(l.outside, "new"):         false,
	} {
		for _, op := range []string{"file-write-data", "file-write-create", "file-write-unlink", "file-link", "file-clone"} {
			if got := sbplAllows(rules, op, path); got != want {
				t.Errorf("%s %s = %v, want %v\n%s", op, path, got, want, profile)
			}
		}
	}

	// The inner mount decides whatever order the mounts come in.
	slices.Reverse(c.Mounts)
	reversed, err := seatbeltProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	if rules := parseSBPL(t, reversed); sbplAllows(rules, "file-write-data", filepath.Join(l.pinned, "file")) || !sbplAllows(rules, "file-write-data", filepath.Join(l.scratch, "out")) {
		t.Errorf("mounts in reverse order lose the inner mount's access:\n%s", reversed)
	}
}

func TestSeatbeltProfileDeniesVCSExecutables(t *testing.T) {
	l, c := newSeatbeltLayout(t)
	if err := os.Symlink("git", filepath.Join(l.system, "bin", "git-symlink")); err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	rules := parseSBPL(t, profile)
	for _, path := range []string{l.git, l.gitLink, l.gitCore, filepath.Join(l.gitCore, "git-fetch")} {
		for _, op := range []string{"file-read-data", "file-read-metadata", "process-exec", "process-exec-interpreter", "file-write-data"} {
			if sbplAllows(rules, op, path) {
				t.Errorf("%s %s is allowed\n%s", op, path, profile)
			}
		}
	}
	// A symbolic link is matched where it leads, so it needs no rule.
	if strings.Contains(profile, "git-symlink") {
		t.Errorf("the profile names a symbolic link:\n%s", profile)
	}
	if !sbplAllows(rules, "process-exec", filepath.Join(l.system, "bin", "cat")) {
		t.Errorf("the executable beside git is refused:\n%s", profile)
	}

	// With VCS granted nothing is denied.
	c.Denied = nil
	granted, err := seatbeltProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	if rules := parseSBPL(t, granted); !sbplAllows(rules, "process-exec", l.git) || !sbplAllows(rules, "process-exec", l.gitLink) {
		t.Errorf("git is refused with VCS granted:\n%s", granted)
	}
}

func TestSeatbeltProfileText(t *testing.T) {
	base := realTemp(t)
	work := filepath.Join(base, `a "quoted\dir`)
	git := filepath.Join(base, "bin", "git")
	for _, dir := range []string{work, filepath.Dir(git)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, git, "true\n")
	profile, err := seatbeltProfile(Confinement{
		Mounts: []Mount{{Path: work, Access: ReadWrite}},
		System: []Mount{{Path: filepath.Join(base, "bin"), Access: ReadOnly}, {Path: "/dev/null", Access: ReadWrite}},
		Denied: []string{git},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := `"` + base + `/a \"quoted\\dir"`
	want := `(version 1)
(allow default)
(deny file-read* file-write* file-link file-clone process-exec*)
(allow file-read-metadata)
(allow file-read* process-exec* (subpath ` + q + `) (subpath "` + base + `/bin") (subpath "/dev/null"))
(allow file-write* file-link file-clone (subpath ` + q + `))
(allow file-write* file-link file-clone (subpath "/dev/null"))
(deny file-read* file-write* file-link file-clone process-exec* (subpath "` + git + `"))
`
	if profile != want {
		t.Errorf("profile =\n%s\nwant\n%s", profile, want)
	}
	if rules := parseSBPL(t, profile); !slices.Equal(rules[3].paths, []string{work, filepath.Join(base, "bin"), "/dev/null"}) {
		t.Errorf("quoted paths read back as %q", rules[3].paths)
	}
}

func TestSeatbeltRefusesWithoutSandboxExec(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, bin := range map[string]string{
		"missing":        filepath.Join(dir, "sandbox-exec"),
		"a directory":    dir,
		"not executable": notExecutable,
	} {
		t.Run(name, func(t *testing.T) {
			s := seatbeltConfiner{bin: bin}
			if err := s.Check(Confinement{}); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("check = %v, want ErrUnsupported", err)
			}
			marker := filepath.Join(t.TempDir(), "ran")
			cmd := exec.Command(agenttest.Script(t, "claude", "touch "+marker+"\n"))
			if err := s.Start(cmd, Confinement{}); !errors.Is(err, ErrUnsupported) || cmd.Process != nil {
				t.Fatalf("start = %v, process %v: want ErrUnsupported and nothing started", err, cmd.Process)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the command ran without its profile")
			}
		})
	}
}

// fakeSandboxExec stands in for sandbox-exec: it keeps the profile it is
// given and executes the command, or fails the way sandbox-exec does when
// it cannot apply the profile, without executing anything.
func fakeSandboxExec(t *testing.T, profileOut string, apply bool) string {
	t.Helper()
	body := `[ "$1" = -p ] || { echo "sandbox-exec: bad usage" >&2; exit 64; }
printf '%s' "$2" > ` + profileOut + `
shift 2
exec "$@"
`
	if !apply {
		body = "echo 'sandbox-exec: sandbox_apply: Operation not permitted' >&2\nexit 65\n"
	}
	return agenttest.Script(t, "sandbox-exec", body)
}

func TestSeatbeltStartsTheCommandUnderItsProfile(t *testing.T) {
	l := newConfinedLayout(t)
	profileOut := filepath.Join(t.TempDir(), "profile")
	s := seatbeltConfiner{bin: fakeSandboxExec(t, profileOut, true)}
	argsOut := filepath.Join(l.session, "args")
	bin := agenttest.Script(t, "claude", `printf '%s\n' "$0" "$@" > `+argsOut+`
cat >/dev/null
echo '{"type":"result","subtype":"success"}'
`)
	realBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{ClaudeBin: bin, Confiner: s, SystemPaths: []Mount{{Path: l.system, Access: ReadOnly}}}
	req := l.request(SandboxNone)
	res, err := r.Run(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	args, err := os.ReadFile(argsOut)
	if err != nil {
		t.Fatalf("the agent did not run: %v", err)
	}
	if lines := strings.Split(string(args), "\n"); lines[0] != bin {
		t.Errorf("the agent ran as %q, want %s", lines, bin)
	}

	// The profile is the one the turn's confinement becomes.
	turn, err := r.Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	c, err := turn.Confinement.withExecutable(bin)
	if err != nil {
		t.Fatal(err)
	}
	want, err := seatbeltProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(profileOut)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("sandbox-exec was given\n%s\nwant\n%s", got, want)
	}
	rules := parseSBPL(t, string(got))
	if !sbplAllows(rules, "process-exec", realBin) || sbplAllows(rules, "process-exec", filepath.Join(l.tools, "git")) || sbplAllows(rules, "file-write-data", filepath.Join(l.work, "x")) {
		t.Errorf("the profile does not hold the turn to its grants:\n%s", got)
	}
}

func TestSeatbeltRunsNothingWhenTheProfileCannotBeApplied(t *testing.T) {
	l := newConfinedLayout(t)
	marker := filepath.Join(l.session, "ran")
	bin := agenttest.Script(t, "claude", "touch "+marker+"\necho '{\"type\":\"result\",\"subtype\":\"success\"}'\n")
	s := seatbeltConfiner{bin: fakeSandboxExec(t, filepath.Join(t.TempDir(), "profile"), false)}
	r := &Runner{ClaudeBin: bin, Confiner: s, SystemPaths: []Mount{{Path: l.system, Access: ReadOnly}}}
	res, err := r.Run(context.Background(), l.request(SandboxNone))
	if err == nil && !res.IsError {
		t.Fatalf("run = %+v: a turn whose profile was not applied succeeded", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the agent ran without its profile")
	}
}
