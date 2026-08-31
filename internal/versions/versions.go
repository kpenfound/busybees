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
	"runtime/debug"
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

// DevVersion is the value cmd/bees compiles in when no `-ldflags -X
// main.version=...` override was given.
const DevVersion = "dev"

// Bees resolves the version bees reports for itself, in this order:
//
//  1. override, when a release build set it through `-ldflags -X`;
//  2. the module version Go recorded in the binary — a tag ("v0.2.0") or the
//     pseudo-version `go install ...@latest` yields for an untagged module;
//  3. for a local build, "dev (<revision> modified)" from the VCS stamps Go
//     writes by default (`-buildvcs=auto`);
//  4. plain "dev" when the binary carries no build information at all.
func Bees(override string, bi *debug.BuildInfo) string {
	if override != "" && override != DevVersion {
		return override
	}
	if bi == nil {
		return DevVersion
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, modified := Revision(bi)
	if rev == "" {
		return DevVersion
	}
	if len(rev) > RevisionDisplayLen {
		rev = rev[:RevisionDisplayLen]
	}
	if modified {
		return fmt.Sprintf("%s (%s modified)", DevVersion, rev)
	}
	return fmt.Sprintf("%s (%s)", DevVersion, rev)
}

// RevisionDisplayLen is how much of a revision Bees shows: enough to name a
// commit, short enough to read in a status line.
const RevisionDisplayLen = 12

// Revision digs the VCS stamps Go writes into a binary by default
// (`-buildvcs=auto`) out of its build information: the full commit the binary
// was built from, and whether the tree was dirty at the time. Both are zero
// for a binary that carries no VCS settings — one built from a source tarball,
// from a module cache, or with `-buildvcs=false`.
//
// Bees renders these; the revision is recorded untruncated in status.json, so
// the running scheduler's build can be compared against the repository.
func Revision(bi *debug.BuildInfo) (rev string, modified bool) {
	if bi == nil {
		return "", false
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return rev, modified
}
