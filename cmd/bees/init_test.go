package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// errNoGH is what the injected gh calls return: the real thing must never run.
var errNoGH = errors.New("gh is not available in tests")

// testInitDeps stands in for everything init does through `gh`; tests must
// never run the real thing.
func testInitDeps() (initDeps, *int) {
	labelCalls := 0
	return initDeps{
		checkGH:     func(context.Context) error { return nil },
		currentRepo: func(context.Context, string) (string, error) { return "", errNoGH },
		repoBranch:  func(context.Context, string) (string, error) { return "", errNoGH },
		syncLabels: func(context.Context, *config.Config) error {
			labelCalls++
			return nil
		},
	}, &labelCalls
}

// assertClean checks that a failed init left nothing behind.
func assertClean(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"bees.toml", ".bees", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("init left %s behind (err=%v)", name, err)
		}
	}
}

func TestInitDoesNotWriteWhenResolveFails(t *testing.T) {
	_, clone := testutil.SetupRepos(t)
	// The clone's origin is a local path, not a GitHub URL, so project.repo
	// cannot be resolved.
	deps, labels := testInitDeps()
	err := runInit(context.Background(), initOptions{dir: clone, remote: "origin"}, deps)
	if err == nil {
		t.Fatal("init should fail when the remote is not a GitHub repository")
	}
	if !strings.Contains(err.Error(), "project.repo is not set") {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "--repo owner/name") {
		t.Fatalf("error should say how to fix it: %v", err)
	}
	assertClean(t, clone)
	if *labels != 0 {
		t.Fatalf("labels synced despite the failure")
	}
}

func TestInitRequiresGitClone(t *testing.T) {
	dir := t.TempDir()
	deps, _ := testInitDeps()
	err := runInit(context.Background(), initOptions{dir: dir, remote: "origin"}, deps)
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Fatalf("error: %v", err)
	}
	assertClean(t, dir)
}

func TestInitRefusesExistingConfig(t *testing.T) {
	_, clone := testutil.SetupRepos(t)
	path := filepath.Join(clone, "bees.toml")
	const body = "# hand written\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	deps, _ := testInitDeps()
	err := runInit(context.Background(), initOptions{dir: clone, remote: "origin", repo: "acme/widgets"}, deps)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != body {
		t.Fatalf("bees.toml was rewritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(clone, ".bees")); !os.IsNotExist(err) {
		t.Fatalf("state dir created: %v", err)
	}
}

func TestInitWithExplicitRepo(t *testing.T) {
	_, clone := testutil.SetupRepos(t)
	deps, labels := testInitDeps()
	o := initOptions{dir: clone, remote: "origin", repo: "acme/widgets", label: config.DefaultLabel}
	if err := runInit(context.Background(), o, deps); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg, err := config.Load(filepath.Join(clone, "bees.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Project.Repo != "acme/widgets" {
		t.Fatalf("repo: %q", cfg.Project.Repo)
	}
	// default_branch comes from the clone's own refs/remotes/origin/HEAD.
	if cfg.Project.DefaultBranch != "main" {
		t.Fatalf("default_branch: %q", cfg.Project.DefaultBranch)
	}
	if _, err := os.Stat(cfg.StateDir()); err != nil {
		t.Fatalf("state dir: %v", err)
	}
	ignore, err := os.ReadFile(filepath.Join(clone, ".gitignore"))
	if err != nil {
		t.Fatalf(".gitignore: %v", err)
	}
	if !strings.Contains(string(ignore), "/.bees/") {
		t.Fatalf(".gitignore: %q", ignore)
	}
	if *labels != 1 {
		t.Fatalf("labels synced %d times, want 1", *labels)
	}
}

func TestInitPrintNeedsNoGitClone(t *testing.T) {
	dir := t.TempDir()
	deps, labels := testInitDeps()
	o := initOptions{dir: dir, remote: "origin", label: config.DefaultLabel, print: true}
	if err := runInit(context.Background(), o, deps); err != nil {
		t.Fatalf("init --print: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("--print wrote %v", entries)
	}
	if *labels != 0 {
		t.Fatalf("--print synced labels")
	}
}

func TestInitLabelFailureSaysHowToRetry(t *testing.T) {
	_, clone := testutil.SetupRepos(t)
	deps, _ := testInitDeps()
	deps.syncLabels = func(context.Context, *config.Config) error { return errNoGH }
	o := initOptions{dir: clone, remote: "origin", repo: "acme/widgets", label: config.DefaultLabel}
	err := runInit(context.Background(), o, deps)
	if err == nil || !strings.Contains(err.Error(), "bees labels sync") {
		t.Fatalf("error: %v", err)
	}
	// The local setup is complete even though the labels are not.
	if _, err := os.Stat(filepath.Join(clone, "bees.toml")); err != nil {
		t.Fatalf("bees.toml: %v", err)
	}
}

// undetectableClone returns a clone in which nothing about the project can be
// detected: the origin remote has no HEAD ref and its URL points nowhere, so
// both `git symbolic-ref` and `git ls-remote` fail, and the URL is not a
// GitHub one. That is the boundary #89 is about.
func undetectableClone(t *testing.T) string {
	t.Helper()
	_, clone := testutil.SetupRepos(t)
	for _, args := range [][]string{
		{"remote", "set-head", "origin", "-d"},
		{"remote", "set-url", "origin", "/nonexistent"},
	} {
		if _, err := workspace.Git(context.Background(), clone, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if b, err := workspace.DefaultBranch(context.Background(), clone, "origin"); err == nil {
		t.Fatalf("default branch is still detectable: %q", b)
	}
	return clone
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestInitRepoWithUndetectableBranchFails is the bug of #89: --repo used to
// make init write a guessed default_branch = "main" as an active setting,
// which also made the Resolve check of #41 pass trivially.
func TestInitRepoWithUndetectableBranchFails(t *testing.T) {
	clone := undetectableClone(t)
	deps, labels := testInitDeps()
	o := initOptions{dir: clone, remote: "origin", repo: "acme/widgets", label: config.DefaultLabel}
	err := runInit(context.Background(), o, deps)
	if err == nil {
		t.Fatal("init should fail when the default branch cannot be detected")
	}
	if !strings.Contains(err.Error(), "project.default_branch") {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "--default-branch") {
		t.Fatalf("error should name the flag that fixes it: %v", err)
	}
	assertClean(t, clone)
	if *labels != 0 {
		t.Fatalf("labels synced despite the failure")
	}
}

// TestInitWithExplicitDefaultBranch is the escape hatch: a branch the user
// states is written as given, without detection.
func TestInitWithExplicitDefaultBranch(t *testing.T) {
	clone := undetectableClone(t)
	deps, _ := testInitDeps()
	o := initOptions{dir: clone, remote: "origin", repo: "acme/widgets", defaultBranch: "trunk", label: config.DefaultLabel}
	if err := runInit(context.Background(), o, deps); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Load, not Resolve: the value must be in the file, not derived.
	cfg, err := config.Load(filepath.Join(clone, "bees.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Project.Repo != "acme/widgets" || cfg.Project.DefaultBranch != "trunk" {
		t.Fatalf("project: %+v", cfg.Project)
	}
}

// TestInitPrintKeepsAGuessedBranchCommented: --print has nothing to resolve,
// so it succeeds, but the branch it could not detect stays a placeholder.
func TestInitPrintKeepsAGuessedBranchCommented(t *testing.T) {
	clone := undetectableClone(t)
	deps, _ := testInitDeps()
	o := initOptions{dir: clone, remote: "origin", repo: "acme/widgets", label: config.DefaultLabel, print: true}
	var err error
	out := captureStdout(t, func() { err = runInit(context.Background(), o, deps) })
	if err != nil {
		t.Fatalf("init --print: %v", err)
	}
	if !strings.Contains(out, "\nrepo = \"acme/widgets\"\n") {
		t.Fatalf("--repo was not written as a setting:\n%s", out)
	}
	if !strings.Contains(out, "\n#default_branch = \"main\"\n") {
		t.Fatalf("guessed default_branch is not commented:\n%s", out)
	}

	// --default-branch without --repo writes the branch and nothing else.
	o = initOptions{dir: clone, remote: "origin", defaultBranch: "trunk", label: config.DefaultLabel, print: true}
	out = captureStdout(t, func() { err = runInit(context.Background(), o, deps) })
	if err != nil {
		t.Fatalf("init --print: %v", err)
	}
	if !strings.Contains(out, "\ndefault_branch = \"trunk\"\n") {
		t.Fatalf("--default-branch was not written as a setting:\n%s", out)
	}
	if !strings.Contains(out, "\n#repo = \"owner/name\"\n") {
		t.Fatalf("repo should stay a placeholder:\n%s", out)
	}
}
