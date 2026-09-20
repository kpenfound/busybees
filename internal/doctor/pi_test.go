package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// fakePi writes a shell script standing in for the pi binary: it records
// its arguments one a line in args.txt beside it, prints output and exits
// with code.
func fakePi(t *testing.T, output string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pi")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$(dirname \"$0\")/args.txt\"\nprintf '%s\\n' '" + output + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckPi(t *testing.T) {
	t.Run("runnable", func(t *testing.T) {
		f := setup(t, "", nil)
		f.PiBin = fakePi(t, "0.85.1", 0)
		wantResult(t, f.run(t, f.checkPi), Pass, "pi 0.85.1")
	})
	t.Run("not on PATH", func(t *testing.T) {
		f := setup(t, "", nil)
		f.PiBin = "pi"
		wantResult(t, f.run(t, f.checkPi), Fail, "not found on PATH", "BEES_PI_BIN", "https://pi.dev")
	})
	t.Run("not runnable", func(t *testing.T) {
		f := setup(t, "", nil)
		f.PiBin = filepath.Join(t.TempDir(), "gone")
		wantResult(t, f.run(t, f.checkPi), Fail, "--version failed")
	})
}

// Checks() carries the pi toolchain check and the session dir writable
// check (a pi session's MCP configuration is written there), and the role
// carries its pi packages check, the moment a role is configured to run pi,
// and none of them while it is disabled.
func TestChecksIncludePiOnlyWhenConfigured(t *testing.T) {
	names := func(f *fixture) (cheap, expensive int, piRole bool) {
		for _, c := range f.Checks() {
			if !c.Expensive {
				cheap++
				continue
			}
			expensive++
			if strings.HasSuffix(c.Run(t.Context()).Name, " pi packages") {
				piRole = true
			}
		}
		return
	}
	baseCheap, _, _ := names(setup(t, "", nil))
	with := setup(t, "[roles.developer]\nagent = \"pi\"\n", nil)
	with.PiBin = fakePi(t, "--mcp-config <value>", 0)
	if cheap, _, piRole := names(with); cheap != baseCheap+2 || !piRole {
		t.Errorf("with a pi role: %d cheap checks (want %d: the toolchain and writable checks), a pi packages check %v", cheap, baseCheap+2, piRole)
	}
	disabled := setup(t, "[roles.developer]\nagent = \"pi\"\nenabled = false\n", nil)
	if cheap, _, piRole := names(disabled); cheap != baseCheap || piRole {
		t.Errorf("with the pi role disabled: %d cheap checks (want %d), a pi packages check %v", cheap, baseCheap, piRole)
	}
}

// The pi packages check loads what a session of the role loads, the adapter
// and then the role's packages with nothing discovered, and passes when the
// adapter's flag is in pi's help.
func TestRolePiPackagesLoad(t *testing.T) {
	f := setupRoles(t, "[roles.developer]\nagent = \"pi\"\npi_packages = [\"npm:@acme/pi-tools\"]\n", nil)
	f.PiBin = fakePi(t, "  --mcp-config <value>   Path to MCP config file", 0)
	c := f.roleCheck(t, "developer pi packages")
	if !c.Expensive || c.Timeout < PiPackagesTimeout {
		t.Errorf("the pi packages check: expensive %v, timeout %s", c.Expensive, c.Timeout)
	}
	wantResult(t, f.run(t, c.Run), Pass, config.PiMCPAdapter, "npm:@acme/pi-tools")
	args, _ := os.ReadFile(filepath.Join(filepath.Dir(f.PiBin), "args.txt"))
	if want := "--no-extensions\n-e\n" + config.PiMCPAdapter + "\n-e\nnpm:@acme/pi-tools\n--help\n"; string(args) != want {
		t.Errorf("pi ran with\n%s\nwant\n%s", args, want)
	}
}

// A package pi could not install or load fails the role with pi's own words
// and the command that installs them by hand; an adapter pi skipped (with
// PI_OFFLINE set) fails it too.
func TestRolePiPackagesThatDoNotLoad(t *testing.T) {
	role := "[roles.developer]\nagent = \"pi\"\npi_packages = [\"npm:@acme/pi-tools\"]\n"
	f := setupRoles(t, role, nil)
	f.PiBin = fakePi(t, "npm error 404 Not Found - @acme/pi-tools", 1)
	r := f.run(t, f.roleCheck(t, "developer pi packages").Run)
	wantResult(t, r, Fail, "404 Not Found", "pi --no-extensions -e "+config.PiMCPAdapter+" -e npm:@acme/pi-tools --help", "roles.developer.pi_packages")

	f = setupRoles(t, role, nil)
	f.PiBin = fakePi(t, "Usage: pi [options]", 0)
	wantResult(t, f.run(t, f.roleCheck(t, "developer pi packages").Run), Fail, config.PiMCPAdapter+" did not load", "PI_OFFLINE")

	f = setupRoles(t, role, nil)
	f.PiBin = "pi"
	wantResult(t, f.run(t, f.roleCheck(t, "developer pi packages").Run), Fail, "not found on PATH")
}

// A container role's packages load inside its image, which the check on
// this machine does not reach, and its row says so.
func TestRolePiPackagesInAContainer(t *testing.T) {
	f := setupRoles(t, "[roles.developer]\nagent = \"pi\"\nsandbox = \"container\"\nsandbox_image = \"ghcr.io/acme/pi:1\"\n", nil)
	f.PiBin = fakePi(t, "--mcp-config", 0)
	wantResult(t, f.run(t, f.roleCheck(t, "developer pi packages").Run), Pass, "roles.developer.sandbox_image")
}
