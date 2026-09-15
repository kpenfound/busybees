package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

func TestRunModeInference(t *testing.T) {
	machine := writeMachine(t, `["foo/bees.toml"]`)
	project := filepath.Join(filepath.Dir(machine), "foo", "bees.toml")
	for _, path := range []string{machine, project} {
		for _, source := range []string{"flag", "env", "search"} {
			t.Run(filepath.Base(filepath.Dir(path))+"/"+source, func(t *testing.T) {
				t.Setenv("BEES_CONFIG", "")
				g := &globalFlags{}
				switch source {
				case "flag":
					g.config = path
					t.Setenv("BEES_CONFIG", "/invalid")
				case "env":
					t.Setenv("BEES_CONFIG", path)
				case "search":
					t.Chdir(filepath.Dir(path))
				}
				active, err := resolveRunConfig(g)
				if err != nil {
					t.Fatal(err)
				}
				if (active.machine != nil) != (path == machine) {
					t.Fatalf("wrong mode: %+v", active)
				}
				want := filepath.Join(filepath.Dir(machine), MachinePIDFile)
				if path == project {
					want = filepath.Join(filepath.Dir(project), ".bees", ProjectPIDFile)
				}
				if active.pidPath() != want {
					t.Fatalf("pid %s, want %s", active.pidPath(), want)
				}
			})
		}
	}
}

func TestChildArgsPreserveFlagValues(t *testing.T) {
	g, root := newRootWithFlags()
	cmd, _, err := root.Find([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"-dv", "--config=-d", "--roles=qa", "--skip-doctor", "--once", "--no-tui"}); err != nil {
		t.Fatal(err)
	}
	args := childArgs(cmd, g.config)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--config=-d", "--verbose=true", "--roles=qa", "--skip-doctor=true", "--once=true", "--no-tui=true"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%s missing %s", joined, want)
		}
	}
	if strings.Contains(joined, "--daemon") {
		t.Fatal(joined)
	}
}

// Build the actual CLI and run it against local git clones, with every role
// disabled and gh replaced on PATH. No real agent, GitHub or Docker is used.
func TestDetachedRunProcess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bees")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	stubDir := t.TempDir()
	gh := `#!/bin/sh
case "$*" in
  "--version") echo 'gh version 2.90.0 (fake)';;
  *) echo '[]';;
esac
`
	if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "BEES_") && !strings.HasPrefix(e, "PATH=") {
			env = append(env, e)
		}
	}
	env = append(env, "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, machine := range []bool{false, true} {
		t.Run(fmt.Sprintf("machine=%t", machine), func(t *testing.T) {
			a, b := processProject(t, "a"), processProject(t, "b")
			active := a
			pidPath := filepath.Join(filepath.Dir(a), "state", ProjectPIDFile)
			if machine {
				m := loadMachine(t, a, b)
				active = m.Path
				pidPath = filepath.Join(filepath.Dir(active), MachinePIDFile)
			}
			flag := "-d"
			if machine {
				flag = "--daemon"
			}
			args := []string{"run", "--config", active, "--skip-doctor", flag}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			parent := exec.CommandContext(ctx, bin, args...)
			parent.Env = env
			var stdout, stderr bytes.Buffer
			parent.Stdout, parent.Stderr = &stdout, &stderr
			if err := parent.Run(); err != nil {
				t.Fatalf("parent: %v: %s", err, stderr.String())
			}
			pid, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
			if err != nil || pid <= 0 {
				t.Fatalf("pid %q: %v", stdout.String(), err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			eventually(t, func() bool {
				data, _ := os.ReadFile(pidPath)
				return strings.TrimSpace(string(data)) == strconv.Itoa(pid)
			}, "child pid file")
			status := func(p string) string { return filepath.Join(filepath.Dir(p), "state", "status.json") }
			exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
			eventually(t, func() bool { return exists(status(a)) }, "project a polling")
			if machine {
				eventually(t, func() bool { return exists(status(b)) }, "project b polling")
			}
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatal("child did not survive parent", err)
			}
			if group, err := syscall.Getpgid(pid); err != nil || group != pid {
				t.Fatalf("child process group %d, want %d: %v", group, pid, err)
			}

			duplicate := exec.Command(bin, args...)
			duplicate.Env = env
			if out, err := duplicate.CombinedOutput(); err == nil || !strings.Contains(string(out), "already running") {
				t.Fatalf("duplicate: %v %s", err, out)
			}
			if machine {
				c := processProject(t, "c")
				body := fmt.Sprintf("projects = [%q, %q]\n", a, c)
				if err := os.WriteFile(active, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				reload := exec.CommandContext(ctx, bin, "machine", "reload", "--config", active)
				reload.Env = env
				if out, err := reload.CombinedOutput(); err != nil || !strings.Contains(string(out), "reload requested") {
					t.Fatalf("machine reload: %v %s", err, out)
				}
				eventually(t, func() bool { return exists(status(c)) }, "added project c polling after SIGHUP")
			}
			if machine {
				stop := exec.CommandContext(ctx, bin, "machine", "stop", "--config", active)
				stop.Env = env
				if out, err := stop.CombinedOutput(); err != nil || !strings.Contains(string(out), "machine daemon stopped") {
					t.Fatalf("machine stop: %v %s", err, out)
				}
				if exists(pidPath) {
					t.Fatal("machine stop returned before graceful pid cleanup")
				}
			} else if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return !exists(pidPath) }, "graceful pid cleanup")
			eventually(t, func() bool {
				if syscall.Kill(pid, 0) != nil {
					return true
				}
				// An orphan can remain a zombie until the container's init reaps it.
				out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
				return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
			}, "child exit")
			logPath := strings.TrimSuffix(pidPath, ".pid") + "-daemon.log"
			data, err := os.ReadFile(logPath)
			if err != nil || len(data) == 0 {
				t.Fatalf("child log: %v %q", err, data)
			}
			// Foreground --once remains a normal command in both modes, with no pid.
			foreground := exec.CommandContext(ctx, bin, "run", "--config", active, "--skip-doctor", "--no-tui", "--once")
			foreground.Env = env
			if out, err := foreground.CombinedOutput(); err != nil {
				t.Fatalf("foreground: %v %s", err, out)
			}
			if exists(pidPath) {
				t.Fatal("foreground wrote a daemon pid file")
			}

		})
	}
}

func eventually(t *testing.T, test func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !test() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", description)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processProject(t *testing.T, name string) string {
	t.Helper()
	p := writeProject(t, "acme/"+name, "state_dir = \"state\"\n")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "\n[scheduler]\npoll_interval = \"100ms\"\n"
	for _, r := range config.Roles {
		body += fmt.Sprintf("\n[roles.%s]\nenabled = false\n", r)
	}
	if _, err = f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// Hold the original project's one pass in a fake gh call so a real SIGHUP
// arrives while --once is running, then let that pass finish normally.
func TestMachineOnceIgnoresSIGHUPProcess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bees")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	a, b := processProject(t, "a"), processProject(t, "b")
	m := loadMachine(t, a)
	stubDir := t.TempDir()
	started, release := filepath.Join(stubDir, "started"), filepath.Join(stubDir, "release")
	gh := `#!/bin/sh
case "$*" in
 "--version") echo 'gh version 2.90.0 (fake)';;
 *)
  touch "$TEST_STARTED"
  while [ ! -f "$TEST_RELEASE" ]; do sleep 0.01; done
  echo '[]';;
esac
`
	if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run", "--config", m.Path, "--skip-doctor", "--no-tui", "--once")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "BEES_") && !strings.HasPrefix(e, "PATH=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_STARTED="+started, "TEST_RELEASE="+release)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, "original project in its single pass")
	if err := os.WriteFile(m.Path, []byte(fmt.Sprintf("projects = [%q, %q]\n", a, b)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("SIGHUP stopped --once: %v\n%s", err, out.String())
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("single pass did not finish: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(b), "state")); !os.IsNotExist(err) {
		t.Fatalf("SIGHUP started the added project: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "reloaded machine projects") {
		t.Fatalf("--once reloaded: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(a), "state", "status.json")); err != nil {
		t.Fatalf("original project did not complete its pass: %v", err)
	}
}
