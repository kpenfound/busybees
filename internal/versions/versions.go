// Package versions checks that the external tools bees drives are new enough.
//
// The minimums are the oldest releases that support every flag and JSON field
// bees uses; see docs/configuration.md ("Requirements") for what each one
// needs. Keep both files in sync when bumping a minimum.
package versions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	// MinGH is the oldest GitHub CLI release bees supports:
	// `gh pr checks --json` (2.50.0) and `gh api --slurp` (2.49.0).
	MinGH = "2.50.0"
	// MinClaude is the oldest Claude Code release bees supports:
	// `claude --name` (2.1.76); the other flags bees passes are older.
	MinClaude = "2.1.76"

	// EnvSkip disables the checks when set to a non-empty value.
	EnvSkip = "BEES_SKIP_VERSION_CHECK"
)

// Version is a parsed major.minor.patch.
type Version [3]int

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Parse extracts the first major.minor.patch from s, which may be the whole
// output of `tool --version` ("gh version 2.69.0 (2025-03-19)",
// "2.1.251 (Claude Code)").
func Parse(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("no version number in %q", strings.TrimSpace(s))
	}
	var v Version
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, nil
}

// Check runs `bin --version` and fails when it reports an older version than
// min, or cannot be run at all. name is how the tool is called in messages.
func Check(ctx context.Context, name, bin, min string) error {
	want, err := Parse(min)
	if err != nil {
		return fmt.Errorf("%s: bad minimum version: %w", name, err)
	}
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%s not found on PATH (need %s >= %s)", name, name, min)
		}
		return fmt.Errorf("%s --version: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	got, err := Parse(string(out))
	if err != nil {
		return fmt.Errorf("%s --version: %w", name, err)
	}
	if got.Less(want) {
		return fmt.Errorf("%s %s is too old: bees needs %s >= %s (set %s=1 to override)", name, got, name, min, EnvSkip)
	}
	return nil
}

// CheckGH verifies the gh CLI.
func CheckGH(ctx context.Context) error {
	if skipped() {
		return nil
	}
	return Check(ctx, "gh", "gh", MinGH)
}

// CheckClaude verifies the claude executable at bin.
func CheckClaude(ctx context.Context, bin string) error {
	if skipped() {
		return nil
	}
	return Check(ctx, "claude", bin, MinClaude)
}

// CheckAll verifies gh and claude, reporting every problem at once.
func CheckAll(ctx context.Context, claudeBin string) error {
	return errors.Join(CheckGH(ctx), CheckClaude(ctx, claudeBin))
}

func skipped() bool { return os.Getenv(EnvSkip) != "" }
