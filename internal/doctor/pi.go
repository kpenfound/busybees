package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// PiPackagesTimeout is how long pi has to install and load a role's
// packages: the first time, pi installs each one with npm.
const PiPackagesTimeout = 5 * time.Minute

// piMCPConfigFlag is the flag pi-mcp-adapter adds to pi. `pi --help` lists
// it only when the adapter loaded.
const piMCPConfigFlag = "--mcp-config"

// piPackageArgs is the part of a pi session's command line that loads its
// packages: no discovered extension, the adapter, then the role's own.
func piPackageArgs(role config.ResolvedRole) []string {
	args := []string{"--no-extensions", "-e", config.PiMCPAdapter}
	for _, pkg := range role.PiPackages {
		args = append(args, "-e", pkg)
	}
	return args
}

// checkRolePiPackages loads what a pi session of the role loads, with
// `pi --help` instead of a prompt: pi resolves every -e source the way a
// session does, installing a missing npm or git package into its cache
// first, and fails when one does not install or load. The adapter loaded
// when its flag is in the help. A session loads from the same cache, so
// doctor warms it the way the skills check warms the skills cache. It runs
// pi on this machine: a role in the container sandbox loads its packages
// inside sandbox_image, which this does not reach.
func (d *Deps) checkRolePiPackages(role config.ResolvedRole) func(context.Context) Result {
	return func(ctx context.Context) Result {
		name := role.Name + " pi packages"
		path, res := d.piPath(name)
		if path == "" {
			res.Group = GroupRoles
			return res
		}
		args := piPackageArgs(role)
		command := "pi " + strings.Join(args, " ") + " --help"
		remedy := fmt.Sprintf("pi installs a missing package with npm on first use: check npm and network access and roles.%s.pi_packages in %s, then run `%s`",
			role.Name, d.Config.Path, command)
		out, err := exec.CommandContext(ctx, path, append(args, "--help")...).CombinedOutput()
		if err != nil {
			return fail(name, GroupRoles, fmt.Sprintf("%s failed: %s", command, oneLine(string(out)+" "+err.Error())), remedy)
		}
		if !strings.Contains(string(out), piMCPConfigFlag) {
			return fail(name, GroupRoles, fmt.Sprintf("%s did not load (with PI_OFFLINE set, pi skips a package it has not installed)", config.PiMCPAdapter),
				remedy)
		}
		loaded := append([]string{config.PiMCPAdapter}, role.PiPackages...)
		detail := "loaded: " + strings.Join(loaded, ", ")
		if roleSandboxes(role, config.SandboxContainer) {
			detail += fmt.Sprintf(" (on this machine; a container session loads them inside roles.%s.sandbox_image)", role.Name)
		}
		return pass(name, GroupRoles, detail)
	}
}

// roleSandboxes reports whether role resolves to sandbox mode at some work
// item size.
func roleSandboxes(role config.ResolvedRole, mode string) bool {
	for _, size := range append([]string{""}, config.Sizes...) {
		if role.ForSize(size).Sandbox == mode {
			return true
		}
	}
	return false
}
