package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/prompts"
)

// renderedPrompt has to fill every prompts.Data field the scheduler fills from
// the config, or `bees prompts show --rendered` prints a prompt that does not
// match the one the sessions actually get.
func TestRenderedPromptCarriesConfiguredSettings(t *testing.T) {
	cfg, err := config.Parse(`
[project]
repo = "owner/name"

[scheduler]
notify = ["kpenfound"]

[roles.developer]
max_size = "s"
commit_flags = "--signoff"
`, t.TempDir()+"/bees.toml")
	if err != nil {
		t.Fatal(err)
	}

	pm, _, err := renderedPrompt(cfg, config.RoleProjectManager)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pm, "`s`") {
		t.Errorf("project manager prompt does not name the configured max size:\n%s", pm)
	}
	if strings.Contains(pm, "larger than `` ") {
		t.Errorf("project manager prompt rendered an empty max size:\n%s", pm)
	}

	dev, _, err := renderedPrompt(cfg, config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dev, "--signoff") {
		t.Errorf("developer prompt does not name the configured commit flags:\n%s", dev)
	}

	product, _, err := renderedPrompt(cfg, config.RoleProductManager)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(product, "@kpenfound") {
		t.Errorf("product manager prompt does not carry scheduler.notify:\n%s", product)
	}
}

// `bees prompts show --rendered` runs outside any session, so it reads the
// project's own prompt files from the checkout bees.toml sits in. It also has
// to report which ones it found, because the command prints a note saying a
// session reads them from its own branch instead.
func TestRenderedPromptReadsTheProjectsPromptFiles(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse("[project]\nrepo = \"owner/name\"\n", filepath.Join(dir, "bees.toml"))
	if err != nil {
		t.Fatal(err)
	}

	// Before anything is written: no files, and no section for them.
	base, project, err := renderedPrompt(cfg, config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if len(project) != 0 {
		t.Fatalf("a project with no %s/ reported %+v", prompts.ProjectDir, project)
	}
	if strings.Contains(base, "Additional instructions from bees/") {
		t.Errorf("prompt names a project prompt file that does not exist:\n%s", base)
	}

	promptDir := filepath.Join(dir, filepath.FromSlash(prompts.ProjectDir))
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"common.md":    "Every role: speak plainly.",
		"developer.md": "Developers: run make lint.",
		"reviewer.md":  "Reviewers: check the migrations.",
	} {
		if err := os.WriteFile(filepath.Join(promptDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dev, project, err := renderedPrompt(cfg, config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if len(project) != 2 {
		t.Fatalf("project prompt files: %+v", project)
	}
	for _, want := range []string{"Every role: speak plainly.", "Developers: run make lint."} {
		if !strings.Contains(dev, want) {
			t.Errorf("rendered developer prompt is missing %q:\n%s", want, dev)
		}
	}
	if strings.Contains(dev, "check the migrations") {
		t.Errorf("rendered developer prompt carries the reviewer's file:\n%s", dev)
	}

	// A file bees cannot use is an error here, where a person is reading the
	// prompt, rather than the warning a session skips it with.
	if err := os.WriteFile(filepath.Join(promptDir, "common.md"), []byte(strings.Repeat("x", prompts.MaxProjectPromptBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := renderedPrompt(cfg, config.RoleDeveloper); err == nil {
		t.Error("an oversized project prompt file must fail the command")
	}
}
