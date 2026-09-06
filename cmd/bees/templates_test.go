package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// runTemplates runs `bees templates ...` from an empty directory with no
// bees.toml, no git repository and a BEES_CONFIG that points nowhere: `list`
// and `show` may need none of them, and neither may `diff` with a name that
// is not a template. `diff` on a config has runTemplatesDiff. It returns what the command printed to
// stdout, what cobra wrote (help and usage), and the error.
func runTemplates(t *testing.T, args ...string) (stdout, cobraOut string, err error) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("BEES_CONFIG", filepath.Join(dir, "missing.toml"))
	var out bytes.Buffer
	stdout = captureStdout(t, func() {
		root := newRoot()
		root.SetArgs(append([]string{"templates"}, args...))
		root.SetOut(&out)
		root.SetErr(&out)
		err = root.Execute()
	})
	return stdout, out.String(), err
}

func TestTemplatesList(t *testing.T) {
	stdout, _, err := runTemplates(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	tpls := config.Templates()
	if len(lines) != len(tpls) {
		t.Fatalf("list printed %d lines for %d templates:\n%s", len(lines), len(tpls), stdout)
	}
	for i, tpl := range tpls {
		name, summary, ok := strings.Cut(lines[i], " ")
		if !ok || name != tpl.Name || strings.TrimSpace(summary) != tpl.Summary {
			t.Errorf("line %d is %q, want %q then %q", i+1, lines[i], tpl.Name, tpl.Summary)
		}
	}
}

func TestTemplatesShow(t *testing.T) {
	stdout, _, err := runTemplates(t, "show", "slop-factory")
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := config.TemplateByName("slop-factory")
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.RenderTOML(config.RenderOptions{Template: &tpl})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != want {
		t.Errorf("show printed something other than the template's render:\n%s", stdout)
	}
	if !strings.HasPrefix(stdout, "# Template: slop-factory\n#\n# ") {
		t.Errorf("show does not start with the When paragraph as comments:\n%s", stdout[:120])
	}
	for _, line := range []string{"\nauto_merge = true\n", "\nfeature_proposals = false\n", "\nenabled = true\n"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("show output has no active %q line", strings.TrimSpace(line))
		}
	}
	for _, line := range []string{"\n#auto_merge = false\n", "\n#feature_proposals = true\n", "\n#enabled = true\n"} {
		if strings.Contains(stdout, line) {
			t.Errorf("show output still has the commented %q line", strings.TrimSpace(line))
		}
	}
	// The project-specific settings are the placeholders bees init --print
	// writes: a template decides nothing about the project.
	if !strings.Contains(stdout, "\n#repo = \"owner/name\"\n") || !strings.Contains(stdout, "\n#assignee = \"@me\"\n") {
		t.Errorf("show output does not keep the project placeholders:\n%s", stdout)
	}
}

func TestTemplatesShowUnknown(t *testing.T) {
	stdout, _, err := runTemplates(t, "show", "nope")
	if err == nil {
		t.Fatal("show nope: no error")
	}
	for _, tpl := range config.Templates() {
		if !strings.Contains(err.Error(), tpl.Name) {
			t.Errorf("error %q does not name template %q", err, tpl.Name)
		}
	}
	if !strings.Contains(err.Error(), `unknown template "nope"`) {
		t.Errorf("error does not name the template asked for: %v", err)
	}
	if stdout != "" {
		t.Errorf("show nope printed a file:\n%s", stdout)
	}
}

func TestTemplatesGroup(t *testing.T) {
	stdout, help, err := runTemplates(t)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Errorf("bare `bees templates` printed to stdout:\n%s", stdout)
	}
	for _, want := range []string{"list", "show", "diff"} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not list %q:\n%s", want, help)
		}
	}
	if _, _, err := runTemplates(t, "nope"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("`bees templates nope`: got %v, want an unknown command error", err)
	}
	if _, _, err := runTemplates(t, "show"); err == nil {
		t.Error("`bees templates show` with no name: no error")
	}
}

// runTemplatesDiff runs `bees templates diff ...` against a bees.toml written
// from body, and returns that file's path, what the command printed to stdout
// and the error. The fixture sets project.repo and project.default_branch, so
// nothing reaches git.
func runTemplatesDiff(t *testing.T, body string, args ...string) (path, stdout string, err error) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	path = filepath.Join(dir, "bees.toml")
	if writeErr := os.WriteFile(path, []byte(body), 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	t.Setenv("BEES_CONFIG", path)
	var out bytes.Buffer
	stdout = captureStdout(t, func() {
		root := newRoot()
		root.SetArgs(append([]string{"templates", "diff"}, args...))
		root.SetOut(&out)
		root.SetErr(&out)
		err = root.Execute()
	})
	return path, stdout, err
}

// templateTOML is the bees.toml `bees init --template <name>` writes.
func templateTOML(t *testing.T, name string) string {
	t.Helper()
	tpl, err := config.TemplateByName(name)
	if err != nil {
		t.Fatal(err)
	}
	text, err := config.RenderTOML(config.RenderOptions{Repo: "acme/widgets", DefaultBranch: "main", ExplicitRepo: true, ExplicitBranch: true, Template: &tpl})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestTemplatesDiffMatches(t *testing.T) {
	path, stdout, err := runTemplatesDiff(t, templateTOML(t, "issue-driven"), "issue-driven")
	if err != nil {
		t.Fatalf("a matching config is not a failure: %v", err)
	}
	if want := path + " matches issue-driven.\n"; stdout != want {
		t.Errorf("got %q, want %q", stdout, want)
	}
}

func TestTemplatesDiffReportsTheDifferences(t *testing.T) {
	path, stdout, err := runTemplatesDiff(t, templateTOML(t, "issue-driven"), "slop-factory")
	if err != nil {
		t.Fatalf("differences are reported, not gated: %v", err)
	}
	want := path + " vs slop-factory\n\n" +
		"  scheduler.feature_proposals   true   (slop-factory: false)\n" +
		"  roles.reviewer.auto_merge     false  (slop-factory: true)\n"
	if stdout != want {
		t.Errorf("got:\n%s\nwant:\n%s", stdout, want)
	}
}

// With no name the closest template is reported, and the heading says so.
// This config leaves every templated key at its default apart from
// auto_merge, which puts it one setting from issue-driven and one from
// slop-factory; the tie goes to the earlier of the two.
func TestTemplatesDiffClosest(t *testing.T) {
	body := "version = 1\n\n[project]\nrepo = \"acme/widgets\"\ndefault_branch = \"main\"\n\n[roles.reviewer]\nauto_merge = true\n"
	path, stdout, err := runTemplatesDiff(t, body)
	if err != nil {
		t.Fatal(err)
	}
	want := path + " vs issue-driven (closest of 5 templates)\n\n" +
		"  roles.reviewer.auto_merge   true  (issue-driven: false)\n"
	if stdout != want {
		t.Errorf("got:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestTemplatesDiffUnknown(t *testing.T) {
	_, stdout, err := runTemplatesDiff(t, templateTOML(t, "planner"), "nope")
	if err == nil {
		t.Fatal("diff nope: no error")
	}
	if !strings.Contains(err.Error(), `unknown template "nope"`) {
		t.Errorf("error does not name the template asked for: %v", err)
	}
	for _, tpl := range config.Templates() {
		if !strings.Contains(err.Error(), tpl.Name) {
			t.Errorf("error %q does not name template %q", err, tpl.Name)
		}
	}
	if stdout != "" {
		t.Errorf("diff nope printed a report:\n%s", stdout)
	}
	// The name is resolved before the config is read, so the error is the
	// same where there is no bees.toml to read.
	if _, _, err := runTemplates(t, "diff", "nope"); err == nil || !strings.Contains(err.Error(), `unknown template "nope"`) {
		t.Errorf("diff nope with no config: got %v, want the unknown template error", err)
	}
}

// No bees.toml in this directory or any parent: the command fails with the
// error every command that reads the config fails with.
func TestTemplatesDiffWithoutAConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("BEES_CONFIG", "")
	var out bytes.Buffer
	var err error
	stdout := captureStdout(t, func() {
		root := newRoot()
		root.SetArgs([]string{"templates", "diff"})
		root.SetOut(&out)
		root.SetErr(&out)
		err = root.Execute()
	})
	if err == nil || !strings.Contains(err.Error(), "bees.toml not found") {
		t.Fatalf("got %v, want the not-found error", err)
	}
	if stdout != "" {
		t.Errorf("diff without a config printed a report:\n%s", stdout)
	}
}
