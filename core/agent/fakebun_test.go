package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/dop251/goja"
	"golang.org/x/sys/unix"
)

// fakeBunEnv set to 1 makes the test binary a stand-in for the JavaScript
// runtime opencode is built on, for the search for custom tools
// (openCodeToolScan): it runs the script given after -e with the part of
// Node's API the search uses, over this machine's filesystem, and prints
// what the script logs. The fake opencode execs it for that search
// (openCodeScanRuns), so the tests exercise the production script where the
// turn would run and with its environment.
const fakeBunEnv = "BEES_TEST_FAKE_BUN"

func TestMain(m *testing.M) {
	if os.Getenv(fakeBunEnv) == "1" {
		os.Exit(runFakeBun(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// openCodeScanRuns is the snippet a fake opencode answers the search with
// when the test is about what the production script finds: the test binary
// as the runtime, given the runner's arguments.
func openCodeScanRuns(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fakeBunEnv + `=1 exec '` + self + `' "$@"`
}

func runFakeBun(args []string) int {
	i := slices.Index(args, "-e")
	if i < 0 || i+1 >= len(args) {
		fmt.Fprintln(os.Stderr, "fake bun: no -e script")
		return 2
	}
	vm := goja.New()
	throw := func(err error) {
		code := err.Error()
		var errno syscall.Errno
		if errors.As(err, &errno) {
			code = unix.ErrnoName(errno)
		}
		e := vm.NewObject()
		_ = e.Set("code", code)
		_ = e.Set("message", err.Error())
		panic(e)
	}
	fs := map[string]any{
		"readdirSync": func(dir string) []any {
			entries, err := os.ReadDir(dir)
			if err != nil {
				throw(err)
			}
			names := make([]any, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			return names
		},
		"statSync": func(file string) map[string]any {
			info, err := os.Stat(file)
			if err != nil {
				throw(err)
			}
			return map[string]any{"isDirectory": func() bool { return info.IsDir() }}
		},
	}
	path := map[string]any{
		"join":    func(parts ...string) string { return filepath.Join(parts...) },
		"dirname": filepath.Dir,
	}
	osModule := map[string]any{
		"homedir": func() string {
			if h := os.Getenv("HOME"); h != "" {
				return h
			}
			h, _ := os.UserHomeDir()
			return h
		},
	}
	modules := map[string]any{"fs": fs, "path": path, "os": osModule}
	env := map[string]any{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	_ = vm.Set("require", func(name string) any {
		m, ok := modules[name]
		if !ok {
			panic(vm.NewTypeError("fake bun: no module " + name))
		}
		return m
	})
	_ = vm.Set("process", map[string]any{
		"env": env,
		"cwd": func() string { wd, _ := os.Getwd(); return wd },
	})
	_ = vm.Set("console", map[string]any{
		"log": func(values ...goja.Value) {
			parts := make([]string, len(values))
			for i, v := range values {
				parts[i] = v.String()
			}
			fmt.Println(strings.Join(parts, " "))
		},
	})
	if _, err := vm.RunString(args[i+1]); err != nil {
		fmt.Fprintln(os.Stderr, "fake bun:", err)
		return 1
	}
	return 0
}
