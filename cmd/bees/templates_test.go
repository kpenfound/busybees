package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// runTemplates runs `bees templates ...` from an empty directory with no
// bees.toml, no git repository and a BEES_CONFIG that points nowhere: neither
// subcommand may need any of them. It returns what the command printed to
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
	for _, want := range []string{"list", "show"} {
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
