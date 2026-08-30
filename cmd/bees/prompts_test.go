package main

import (
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// renderedPrompt has to fill every prompts.Data field the scheduler fills from
// the config, or `bees prompts show --rendered` prints a prompt that does not
// match the one the sessions actually get.
func TestRenderedPromptCarriesConfiguredSettings(t *testing.T) {
	cfg, err := config.Parse(`
[project]
repo = "owner/name"

[roles.developer]
max_size = "s"
commit_flags = "--signoff"
`, t.TempDir()+"/bees.toml")
	if err != nil {
		t.Fatal(err)
	}

	pm, err := renderedPrompt(cfg, config.RoleProjectManager)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pm, "`s`") {
		t.Errorf("project manager prompt does not name the configured max size:\n%s", pm)
	}
	if strings.Contains(pm, "larger than `` ") {
		t.Errorf("project manager prompt rendered an empty max size:\n%s", pm)
	}

	dev, err := renderedPrompt(cfg, config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dev, "--signoff") {
		t.Errorf("developer prompt does not name the configured commit flags:\n%s", dev)
	}
}
