package skills

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pluginFixture is a minimal Claude Code plugin: just a manifest.
func pluginFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name":"fixture-plugin"}`)
	return dir
}

// skillsCollectionFixture is a skills/ directory holding two skills.
func skillsCollectionFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk(t, filepath.Join(dir, "skills", "alpha", "SKILL.md"), "---\nname: alpha\n---\n")
	mk(t, filepath.Join(dir, "skills", "beta", "SKILL.md"), "---\nname: beta\n---\n")
	return dir
}

// fullPluginFixture is a Claude Code plugin with hooks, an MCP server
// config, agents, commands and a skills/ directory: everything skills-only
// mode must leave out of the wrapper it builds.
func fullPluginFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name":"full"}`)
	mk(t, filepath.Join(dir, "hooks", "hooks.json"), `{}`)
	mk(t, filepath.Join(dir, ".mcp.json"), `{}`)
	mk(t, filepath.Join(dir, "agents", "reviewer.md"), "# reviewer\n")
	mk(t, filepath.Join(dir, "commands", "deploy.md"), "# deploy\n")
	mk(t, filepath.Join(dir, "skills", "tdd", "SKILL.md"), "---\nname: tdd\n---\n")
	return dir
}

// fullPluginWithoutSkillsFixture is a Claude Code plugin with hooks, an MCP
// server config and agents, but neither SKILL.md nor a skills/ directory.
func fullPluginWithoutSkillsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name":"full"}`)
	mk(t, filepath.Join(dir, "hooks", "hooks.json"), `{}`)
	mk(t, filepath.Join(dir, ".mcp.json"), `{}`)
	mk(t, filepath.Join(dir, "agents", "reviewer.md"), "# reviewer\n")
	return dir
}

// emptyFixture is a target with none of the three recognised layouts.
func emptyFixture(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func mustParse(t *testing.T, raw string) Ref {
	t.Helper()
	ref, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func topLevelNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestDefaultLayoutPluginReturnedAsIs(t *testing.T) {
	m, _ := testManager(t, pluginFixture(t))
	dir := prepare(t, m, testRef)
	if want := m.cloneDir(mustParse(t, testRef)); dir != want {
		t.Fatalf("got %s, want the target inside the clone (%s)", dir, want)
	}
	mustExist(t, filepath.Join(dir, ".claude-plugin", "plugin.json"))
}

func TestDefaultLayoutWrapsSkillMd(t *testing.T) {
	m, _ := testManager(t, skillFixture(t))
	dir := prepare(t, m, testRef)
	if dir == m.cloneDir(mustParse(t, testRef)) {
		t.Fatalf("expected a generated wrapper, got the clone itself")
	}
	mustExist(t, filepath.Join(dir, ".claude-plugin", "plugin.json"))
	mustExist(t, filepath.Join(dir, "skills", "fix", "SKILL.md"))
}

func TestDefaultLayoutWrapsSkillsCollection(t *testing.T) {
	m, _ := testManager(t, skillsCollectionFixture(t))
	dir := prepare(t, m, testRef)
	if dir == m.cloneDir(mustParse(t, testRef)) {
		t.Fatalf("expected a generated wrapper, got the clone itself")
	}
	mustExist(t, filepath.Join(dir, ".claude-plugin", "plugin.json"))
	mustExist(t, filepath.Join(dir, "skills", "alpha", "SKILL.md"))
	mustExist(t, filepath.Join(dir, "skills", "beta", "SKILL.md"))
}

func TestDefaultLayoutRejectsUnrecognisedTarget(t *testing.T) {
	m, _ := testManager(t, emptyFixture(t))
	_, err := m.Prepare(context.Background(), []string{testRef})
	if err == nil {
		t.Fatal("expected an error for an empty target")
	}
	for _, want := range []string{"plugin.json", "SKILL.md", "skills/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestSkillsOnlyWrapsFullPlugin(t *testing.T) {
	m, _ := testManager(t, fullPluginFixture(t))
	m.SkillsOnly = true
	dir := prepare(t, m, testRef)
	if dir == m.cloneDir(mustParse(t, testRef)) {
		t.Fatalf("skills-only mode returned the repository directory")
	}
	if got, want := topLevelNames(t, dir), []string{".claude-plugin", "skills"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("wrapper tree = %v, want only %v", got, want)
	}
	mustExist(t, filepath.Join(dir, "skills", "tdd", "SKILL.md"))
}

func TestSkillsOnlyWrapsSkillMd(t *testing.T) {
	m, _ := testManager(t, skillFixture(t))
	m.SkillsOnly = true
	dir := prepare(t, m, testRef)
	if dir == m.cloneDir(mustParse(t, testRef)) {
		t.Fatalf("skills-only mode returned the repository directory")
	}
	if got, want := topLevelNames(t, dir), []string{".claude-plugin", "skills"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("wrapper tree = %v, want only %v", got, want)
	}
	mustExist(t, filepath.Join(dir, "skills", "fix", "SKILL.md"))
}

func TestSkillsOnlyRejectsFullPluginWithoutSkills(t *testing.T) {
	m, _ := testManager(t, fullPluginWithoutSkillsFixture(t))
	m.SkillsOnly = true
	_, err := m.Prepare(context.Background(), []string{testRef})
	if err == nil {
		t.Fatal("expected an error for a full plugin with no skills/")
	}
	for _, want := range []string{"SKILL.md", "skills/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "plugin.json") {
		t.Errorf("error %q should not name plugin.json in skills-only mode", err)
	}
}

func TestSkillsOnlyRejectsEmptyTarget(t *testing.T) {
	m, _ := testManager(t, emptyFixture(t))
	m.SkillsOnly = true
	_, err := m.Prepare(context.Background(), []string{testRef})
	if err == nil {
		t.Fatal("expected an error for an empty target")
	}
	for _, want := range []string{"SKILL.md", "skills/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestSymlinkEscapeRefused covers a repository whose sub-directory is a
// symlink pointing outside the clone: Parse cannot see this, since it only
// inspects the reference string, so the layout step catches it once the
// clone exists on disk.
func TestSymlinkEscapeRefused(t *testing.T) {
	m := NewManager(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ref := mustParse(t, "https://github.com/x/escape#sub")
	dir := m.cloneDir(ref)
	mk(t, filepath.Join(dir, ".git", "HEAD"), "")

	outside := t.TempDir()
	mk(t, filepath.Join(outside, "SKILL.md"), "---\nname: outside\n---\n")
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}

	_, err := m.Prepare(context.Background(), []string{ref.String()})
	if err == nil {
		t.Fatal("expected an error for a sub-directory that escapes the clone")
	}
	if !strings.Contains(err.Error(), ref.String()) {
		t.Fatalf("error %q does not name the reference %q", err, ref.String())
	}

	if exists(m.wrapperDir(ref)) {
		t.Fatalf("a wrapper directory was created for a refused reference")
	}
}

// TestDistinctWrappersForSameDerivedName covers two references that derive
// the same skill name from different repositories: each must keep its own
// wrapper directory, and preparing them alternately must never rebuild
// either one.
func TestDistinctWrappersForSameDerivedName(t *testing.T) {
	m, _ := testManager(t, skillFixture(t))

	const acme = "https://github.com/acme/skills"
	const other = "https://github.com/other/skills"

	acmeRef, otherRef := mustParse(t, acme), mustParse(t, other)
	if m.wrapperDir(acmeRef) == m.wrapperDir(otherRef) {
		t.Fatalf("acme/skills and other/skills share a wrapper directory: %s", m.wrapperDir(acmeRef))
	}

	var acmeDir, otherDir string
	for i := 0; i < 3; i++ {
		acmeDir = prepare(t, m, acme)
		otherDir = prepare(t, m, other)
	}
	if acmeDir == otherDir {
		t.Fatalf("expected distinct wrapper directories, got %s for both", acmeDir)
	}

	acmeManifest := filepath.Join(acmeDir, ".claude-plugin", "plugin.json")
	otherManifest := filepath.Join(otherDir, ".claude-plugin", "plugin.json")
	beforeAcme, err := os.Stat(acmeManifest)
	if err != nil {
		t.Fatal(err)
	}
	beforeOther, err := os.Stat(otherManifest)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		prepare(t, m, acme)
		prepare(t, m, other)
	}

	afterAcme, err := os.Stat(acmeManifest)
	if err != nil {
		t.Fatal(err)
	}
	afterOther, err := os.Stat(otherManifest)
	if err != nil {
		t.Fatal(err)
	}
	if !beforeAcme.ModTime().Equal(afterAcme.ModTime()) {
		t.Fatalf("acme wrapper rebuilt: mtime %v -> %v", beforeAcme.ModTime(), afterAcme.ModTime())
	}
	if !beforeOther.ModTime().Equal(afterOther.ModTime()) {
		t.Fatalf("other wrapper rebuilt: mtime %v -> %v", beforeOther.ModTime(), afterOther.ModTime())
	}
}

// TestWrapperReusedAfterSuccessfulRefresh covers a clone refreshed to a new
// commit on the same reference: running sessions hold --plugin-dir paths
// into the wrapper, so it must be reused, not removed or rebuilt.
func TestWrapperReusedAfterSuccessfulRefresh(t *testing.T) {
	m, g := testManager(t, skillFixture(t))
	m.SetRefresh(RefreshAlways)

	dir1 := prepare(t, m, testRef)
	manifest := filepath.Join(dir1, ".claude-plugin", "plugin.json")
	before, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}

	dir2 := prepare(t, m, testRef)
	if dir1 != dir2 {
		t.Fatalf("wrapper directory changed across a refresh: %s -> %s", dir1, dir2)
	}
	after, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("wrapper manifest rebuilt after a successful refresh: mtime %v -> %v", before.ModTime(), after.ModTime())
	}
	if g.count("pull") != 1 {
		t.Fatalf("expected exactly one pull, got %d: %v", g.count("pull"), g.calls)
	}
}
