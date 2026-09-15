package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
)

func TestMachineCommandsRegistered(t *testing.T) {
	for _, name := range []string{"status", "cost", "stop", "reload"} {
		cmd, rest, err := newRoot().Find([]string{"machine", name})
		if err != nil || len(rest) != 0 || cmd.Name() != name {
			t.Fatalf("%s: %v %v", name, rest, err)
		}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Help(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "active machine config") {
			t.Fatalf("%s help: %s", name, out.String())
		}
		machine := writeMachine(t, `["foo/bees.toml"]`)
		err = runRoot(t, "machine", name, "-c", filepath.Join(filepath.Dir(machine), "foo", "bees.toml"))
		if err == nil || !strings.Contains(err.Error(), "requires an active machine config") {
			t.Fatalf("%s accepted project: %v", name, err)
		}
	}
}

func machineOutput(t *testing.T, path, command string, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"machine", command, "--config", path}, args...))
	err := root.Execute()
	return out.String(), err
}

func machineReportFixture(t *testing.T) *config.Machine {
	t.Helper()
	path := writeMachine(t, `["foo/bees.toml", "bar/bees.toml", "baz/bees.toml"]`)
	for _, name := range []string{"bar", "baz"} {
		dir := filepath.Join(filepath.Dir(path), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Explicit, distinct state paths must win over the default .bees path.
		data := "version = 2\n[project]\nrepo = \"acme/" + name + "\"\nstate_dir = \"records\"\n"
		if err := os.WriteFile(filepath.Join(dir, "bees.toml"), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := config.LoadMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMachineStatusSnapshotsAndUnavailable(t *testing.T) {
	m := machineReportFixture(t)
	store := state.New(m.Configs[1].StateDir())
	if err := store.SaveStatus(state.Status{Workers: []state.Worker{{}, {}}, LastError: "poll failed"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	bad := state.New(m.Configs[2].StateDir())
	if err := os.MkdirAll(bad.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad.Dir, "status.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := machineOutput(t, m.Path, "status")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("rows: %s", out)
	}
	for i, cfg := range m.Configs {
		if !strings.HasPrefix(lines[i+1], cfg.Path) {
			t.Fatalf("project order: %s", out)
		}
	}
	if strings.Count(out, "unavailable") != 2 || !strings.Contains(lines[2], "recorded") || !strings.Contains(lines[2], st.UpdatedAt.Format(time.RFC3339)) || !strings.Contains(lines[2], "2  ") || !strings.Contains(lines[2], "poll failed") {
		t.Fatalf("status: %s", out)
	}
	if _, err := os.Stat(m.Configs[0].StateDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created state: %v", err)
	}
}

func TestMachineCostAggregatesAndFilters(t *testing.T) {
	m := machineReportFixture(t)
	for i, cost := range []float64{1.25, 2.50} {
		store := state.New(m.Configs[i].StateDir())
		for _, e := range []state.LedgerEntry{
			{Time: time.Now(), Turns: i + 2, CostUSD: cost},
			{Time: time.Now().Add(-48 * time.Hour), Turns: 10, CostUSD: 10},
		} {
			if err := store.AppendLedger(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		args  []string
		total string
	}{
		{nil, "total 2 5 $3.75"},
		{[]string{"--since", "72h"}, "total 4 25 $23.75"},
	} {
		out, err := machineOutput(t, m.Path, "cost", tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 5 {
			t.Fatalf("rows: %s", out)
		}
		for i, cfg := range m.Configs {
			if !strings.HasPrefix(lines[i+1], cfg.Path) {
				t.Fatalf("project order: %s", out)
			}
		}
		if strings.Join(strings.Fields(lines[4]), " ") != tc.total || strings.Join(strings.Fields(lines[3]), " ") != m.Configs[2].Path+" 0 0 $0.00" {
			t.Fatalf("cost: %s", out)
		}
	}
	if _, err := os.Stat(m.Configs[2].StateDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cost created state: %v", err)
	}
	// An unreadable ledger cannot silently contribute zero to the total.
	if err := os.WriteFile(m.Configs[2].StateDir(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := machineOutput(t, m.Path, "cost")
	if err == nil || !strings.Contains(err.Error(), m.Configs[2].Path) || out != "" {
		t.Fatalf("read failure: %s %v", out, err)
	}
}

func lockedMachine(t *testing.T) (string, string) {
	t.Helper()
	configPath := writeMachine(t, `["foo/bees.toml"]`)
	path := filepath.Join(filepath.Dir(configPath), MachinePIDFile)
	if err := os.WriteFile(path, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return configPath, path
}

func TestMachineControlSignalsAndWaits(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "reload"}[reload], func(t *testing.T) {
			path, pidPath := lockedMachine(t)
			// The daemon owns project reconciliation, including invalid project paths.
			if err := os.Remove(filepath.Join(filepath.Dir(path), "foo", "bees.toml")); err != nil {
				t.Fatal(err)
			}
			var signals []syscall.Signal
			cmd := newMachineControlCmd(&globalFlags{config: path}, reload, func(pid int, sig syscall.Signal) error {
				if pid != 4242 {
					t.Fatalf("pid: %d", pid)
				}
				signals = append(signals, sig)
				if sig == 0 && len(signals) == 3 {
					if err := os.Remove(pidPath); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			var out bytes.Buffer
			cmd.SetOut(&out)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			if reload {
				if len(signals) != 1 || signals[0] != syscall.SIGHUP || !strings.Contains(out.String(), "reload requested") {
					t.Fatalf("reload: %v %s", signals, out.String())
				}
			} else if len(signals) != 3 || signals[0] != syscall.SIGTERM || signals[1] != 0 || !strings.Contains(out.String(), "stopped") {
				t.Fatalf("stop returned before drain: %v %s", signals, out.String())
			}
		})
	}
}

func TestMachineControlNoDaemon(t *testing.T) {
	for _, mode := range []string{"absent", "stale", "exited"} {
		for _, reload := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "stop", true: "reload"}[reload], func(t *testing.T) {
				path := writeMachine(t, `["foo/bees.toml"]`)
				if mode == "exited" {
					path, _ = lockedMachine(t)
				}
				if mode == "stale" {
					pidPath := filepath.Join(filepath.Dir(path), MachinePIDFile)
					if err := os.WriteFile(pidPath, []byte("4242\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(pidPath+".lock", nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				cmd := newMachineControlCmd(&globalFlags{config: path}, reload, func(int, syscall.Signal) error { calls++; return syscall.ESRCH })
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&bytes.Buffer{})
				err := cmd.Execute()
				if (err != nil) != reload {
					t.Fatalf("result: %v", err)
				}
				if reload && !strings.Contains(err.Error(), "no running machine daemon") {
					t.Fatal(err)
				}
				if !reload && !strings.Contains(out.String(), "no running machine daemon") {
					t.Fatal(out.String())
				}
				want := 0
				if mode == "exited" {
					want = 1
				}
				if calls != want {
					t.Fatalf("signal calls: %d want %d", calls, want)
				}
			})
		}
	}
}

func TestMachineControlErrorsAndCancellation(t *testing.T) {
	for _, mode := range []string{"permission", "cancel", "bad-pid"} {
		t.Run(mode, func(t *testing.T) {
			path, pidPath := lockedMachine(t)
			if mode == "bad-pid" {
				if err := os.WriteFile(pidPath, []byte("0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			cmd := newMachineControlCmd(&globalFlags{config: path}, false, func(int, syscall.Signal) error {
				calls++
				if mode == "permission" {
					return syscall.EPERM
				}
				cancel()
				return nil
			})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.ExecuteContext(ctx)
			switch mode {
			case "permission":
				if !errors.Is(err, syscall.EPERM) {
					t.Fatal(err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "bad-pid":
				if err == nil || calls != 0 {
					t.Fatalf("invalid pid signaled: %v, %d", err, calls)
				}
			}
		})
	}
}

func TestMachineActiveConfigResolution(t *testing.T) {
	machine := writeMachine(t, `["foo/bees.toml"]`)
	for _, source := range []string{"flag", "env", "search"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("BEES_CONFIG", "")
			t.Chdir(filepath.Dir(machine))
			var args []string
			switch source {
			case "flag":
				t.Setenv("BEES_CONFIG", "missing.toml")
				args = []string{"-c", machine}
			case "env":
				t.Chdir(t.TempDir())
				t.Setenv("BEES_CONFIG", machine)
			}
			root := newRoot()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs(append([]string{"machine", "status"}, args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "unavailable") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestMachineWaitHandlesExitAndReplacement(t *testing.T) {
	for _, mode := range []string{"exit", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			_, path := lockedMachine(t)
			calls := 0
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := waitMachine(ctx, path, 4242, func(pid int, sig syscall.Signal) error {
				calls++
				if pid != 4242 || sig != 0 {
					t.Fatalf("unexpected signal: %d %v", pid, sig)
				}
				if mode == "exit" {
					return syscall.ESRCH
				}
				return os.WriteFile(path, []byte("4243\n"), 0o600)
			})
			if err != nil || calls != 1 {
				t.Fatalf("wait: %v calls=%d", err, calls)
			}
		})
	}
}
