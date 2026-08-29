package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bees.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTemplateLoads(t *testing.T) {
	text, err := Template(TemplateData{Repo: "acme/widgets", Assignee: "@me", Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, text))
	if err != nil {
		t.Fatalf("template does not load: %v", err)
	}
	if cfg.Project.Repo != "acme/widgets" || cfg.Project.Remote != "origin" || cfg.Filter.Label != "bees" || cfg.Filter.Assignee != "@me" {
		t.Fatalf("unexpected config: %+v %+v", cfg.Project, cfg.Filter)
	}
	// Without Explicit, repo and default_branch are commented placeholders.
	text, _ = Template(TemplateData{Repo: "acme/widgets", DefaultBranch: "trunk"})
	cfg, err = Load(writeConfig(t, text))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project.Repo != "" || cfg.Project.DefaultBranch != "" || !strings.Contains(text, "#repo = \"acme/widgets\"") || !strings.Contains(text, "#default_branch = \"trunk\"") {
		t.Fatalf("placeholders: %+v", cfg.Project)
	}
	if cfg.Scheduler.MaxDevelopers != 1 || cfg.Scheduler.PollInterval.Duration != 5*time.Minute || cfg.Scheduler.RateLimitBackoff.Duration != 15*time.Minute {
		t.Fatalf("scheduler defaults not applied: %+v", cfg.Scheduler)
	}
}

func TestMerge(t *testing.T) {
	p := writeConfig(t, `
version = 1
[project]
repo = "acme/widgets"

[global]
prompt = "global text"
skills = ["https://github.com/a/one", "https://github.com/a/two"]
model = "opus"
fallback_model = "sonnet"
max_turns = 50
timeout = "10m"
[global.mcp.shared]
command = "srv"
[global.mcp.overridden]
command = "old"

[roles.developer]
prompt = "dev text"
skills = ["https://github.com/a/two", "https://github.com/a/three"]
model = "sonnet"
max_turns = 99
[roles.developer.mcp.overridden]
command = "new"
[roles.developer.mcp.mine]
url = "https://example.com/mcp"

[roles.qa]
enabled = false

[global.env]
FOO = "global"
BAR = "$HOME/bar"
[roles.developer.env]
FOO = "dev"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := cfg.Role("dev")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Name != RoleDeveloper {
		t.Fatalf("alias not resolved: %s", dev.Name)
	}
	if dev.Prompt != "global text\n\ndev text" {
		t.Fatalf("prompt merge: %q", dev.Prompt)
	}
	if got := strings.Join(dev.Skills, ","); got != "https://github.com/a/one,https://github.com/a/two,https://github.com/a/three" {
		t.Fatalf("skills union: %s", got)
	}
	if dev.Model != "sonnet" || dev.FallbackModel != "sonnet" || dev.MaxTurns != 99 || dev.Timeout != 10*time.Minute {
		t.Fatalf("scalars: %+v", dev)
	}
	if dev.MCP["overridden"].Command != "new" || dev.MCP["shared"].Command != "srv" || dev.MCP["mine"].URL == "" {
		t.Fatalf("mcp merge: %+v", dev.MCP)
	}
	if got := dev.MCPNames(); strings.Join(got, ",") != "mine,overridden,shared" {
		t.Fatalf("mcp names: %v", got)
	}
	if dev.Env["FOO"] != "dev" || dev.Env["BAR"] != "$HOME/bar" {
		t.Fatalf("env merge: %v", dev.Env)
	}
	pm, _ := cfg.Role("pm")
	if pm.Model != "opus" || pm.MaxTurns != 50 || pm.Prompt != "global text" || len(pm.MCP) != 2 {
		t.Fatalf("global fallback: %+v", pm)
	}
	if pm.Env["FOO"] != "global" {
		t.Fatalf("global env: %v", pm.Env)
	}
	qa, _ := cfg.Role("qa")
	if qa.Enabled {
		t.Fatal("qa should be disabled")
	}
	rev, _ := cfg.Role("reviewer")
	if !rev.Enabled || rev.Timeout != 10*time.Minute {
		t.Fatalf("reviewer: %+v", rev)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project.Remote != "origin" {
		t.Fatalf("remote default: %q", cfg.Project.Remote)
	}
	dev, _ := cfg.Role(RoleDeveloper)
	if dev.Model != DefaultModel || dev.FallbackModel != DefaultFallbackModel || dev.MaxTurns != DefaultMaxTurns || dev.Timeout != DefaultTimeout {
		t.Fatalf("defaults: %+v", dev)
	}
	if cfg.StateDir() != filepath.Join(cfg.Dir(), ".bees") {
		t.Fatalf("state dir: %s", cfg.StateDir())
	}
	l := cfg.Labels()
	if l.Ready != "bees:ready" || l.Base != "bees" || len(l.All()) != 12 {
		t.Fatalf("labels: %+v", l)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"bad repo":         "version = 1\n[project]\nrepo = \"nope\"\n",
		"unknown role":     "version = 1\n[project]\nrepo = \"a/b\"\n[roles.intern]\nprompt = \"x\"\n",
		"unknown key":      "version = 1\n[project]\nrepo = \"a/b\"\nrepository = \"x\"\n",
		"mcp no command":   "version = 1\n[project]\nrepo = \"a/b\"\n[global.mcp.x]\nargs = [\"a\"]\n",
		"bad effort":       "version = 1\n[project]\nrepo = \"a/b\"\n[global]\neffort = \"extreme\"\n",
		"label with colon": "version = 1\n[project]\nrepo = \"a/b\"\n[filter]\nlabel = \"a:b\"\n",
		"open filter":      "version = 1\n[project]\nrepo = \"a/b\"\n[filter]\nrequire_label = false\n",
		"max devs zero":    "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nmax_developers = -1\n",
	}
	for name, body := range cases {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFilterLabelRequired(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[filter]\nrequire_label = false\nassignee = \"me\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Filter.LabelRequired() {
		t.Fatal("label should not be required")
	}
	if cfg.Filter.Label != "bees" {
		t.Fatal("label default should still apply")
	}
}

func TestFind(t *testing.T) {
	p := writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n")
	nested := filepath.Join(filepath.Dir(p), "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := Find(nested)
	if err != nil || found != p {
		t.Fatalf("Find: %s %v", found, err)
	}
}

func TestMergePolicy(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Merge()
	if p.AutoMerge || p.Method != "squash" || p.ChecksWait != time.Minute || p.ChecksPollInterval != 2*time.Minute || p.ChecksTimeout != 30*time.Minute || p.MaxCheckFixRounds != 2 {
		t.Fatalf("defaults: %+v", p)
	}
	cfg, err = Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[roles.reviewer]\nauto_merge = true\nmerge_method = \"rebase\"\nchecks_wait = \"5s\"\nmax_check_fix_rounds = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	p = cfg.Merge()
	if !p.AutoMerge || p.Method != "rebase" || p.ChecksWait != 5*time.Second || p.MaxCheckFixRounds != 1 {
		t.Fatalf("custom: %+v", p)
	}
	cfg, err = Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[roles.developer]\ncommit_flags = \" --gpg-sign --signoff \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CommitFlags() != "--gpg-sign --signoff" {
		t.Fatalf("commit flags: %q", cfg.CommitFlags())
	}
}

// TestTemplateUncommented makes sure every commented-out option in the
// template is valid TOML with a value the loader accepts: a user should be
// able to uncomment any line and have it work.
func TestTemplateUncommented(t *testing.T) {
	text, err := Template(TemplateData{Repo: "acme/widgets", Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "#=") || strings.HasPrefix(line, "# ") || line == "#":
			continue // prose comment or separator
		case strings.HasPrefix(line, "#prompt_file"):
			continue // placeholder file does not exist
		case strings.HasPrefix(line, "#"):
			lines = append(lines, strings.TrimPrefix(line, "#"))
		default:
			lines = append(lines, line)
		}
	}
	cfg, err := Load(writeConfig(t, strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("uncommented template does not load: %v", err)
	}
	if cfg.Scheduler.MaxDevelopers != 1 || cfg.Global.Model != "opus" || cfg.Roles[RoleReviewer].MergeMethod != "squash" {
		t.Fatalf("unexpected values: %+v", cfg.Scheduler)
	}
	if cfg.Filter.Assignee != "@me" || len(cfg.Global.MCP) != 2 || cfg.Roles[RoleQA].MCP["example"].Command != "example-mcp" {
		t.Fatalf("filter/mcp: %+v %+v", cfg.Filter, cfg.Global.MCP)
	}
	// And the template as written (defaults commented) yields the same resolved roles.
	base, err := Load(writeConfig(t, text))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Roles {
		a, _ := base.Role(r)
		b, _ := cfg.Role(r)
		if a.Model != b.Model || a.MaxTurns != b.MaxTurns || a.Timeout != b.Timeout || a.FallbackModel != b.FallbackModel {
			t.Errorf("%s: commented defaults %+v differ from explicit %+v", r, a, b)
		}
	}
}

func TestParseGitHubRepo(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/acme/widgets.git":   "acme/widgets",
		"https://github.com/acme/widgets":       "acme/widgets",
		"git@github.com:acme/widgets.git":       "acme/widgets",
		"ssh://git@github.com/acme/widgets.git": "acme/widgets",
		"https://kyle@github.com/acme/widgets/": "acme/widgets",
		"https://gitlab.com/acme/widgets.git":   "",
		"git@github.com:acme":                   "",
	} {
		got, ok := ParseGitHubRepo(url)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q %v want %q", url, got, ok, want)
		}
	}
}

func TestVersion(t *testing.T) {
	bad := map[string]string{
		"newer":    "version = 99\n[project]\nrepo = \"a/b\"\n",
		"negative": "version = -1\n[project]\nrepo = \"a/b\"\n",
		"string":   "version = \"1\"\n[project]\nrepo = \"a/b\"\n",
	}
	for name, body := range bad {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if name == "newer" && !strings.Contains(err.Error(), "upgrade bees") {
			t.Errorf("newer: %v", err)
		}
	}
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil || cfg.Version != CurrentVersion || cfg.NeedsRewrite() {
		t.Fatalf("current: %+v %v", cfg, err)
	}
	if b, err := cfg.Rewrite(); err != nil || b != "" {
		t.Fatalf("rewrite of current file should be a no-op: %q %v", b, err)
	}
}

func TestMigrateUnversionedFile(t *testing.T) {
	orig := "# my factory\n\n[project]\n# keep this comment\nrepo = \"a/b\"\n#branch_prefix = \"bees/\"\n"
	path := writeConfig(t, orig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != CurrentVersion || cfg.MigratedFrom != 0 || !cfg.NeedsRewrite() || cfg.Project.Repo != "a/b" {
		t.Fatalf("migrated in memory: %+v", cfg)
	}
	backup, err := cfg.Rewrite()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	want := "# my factory\n\n# Format version of this file (see docs/configuration.md).\nversion = 1\n\n[project]\n# keep this comment\nrepo = \"a/b\"\n#branch_prefix = \"bees/\"\n"
	if text != want {
		t.Fatalf("rewritten file:\n%s\nwant:\n%s", text, want)
	}
	if b, _ := os.ReadFile(backup); string(b) != orig || filepath.Base(backup) != "bees.toml.v0.bak" {
		t.Fatalf("backup %s: %q", backup, b)
	}
	again, err := Load(path)
	if err != nil || again.NeedsRewrite() {
		t.Fatalf("reload: %+v %v", again, err)
	}
	// version = 0 written explicitly is replaced, not duplicated.
	if got := setVersion("version = 0 # old\n[project]\n", 1); got != "version = 1\n[project]\n" {
		t.Fatalf("setVersion replace: %q", got)
	}
}

func TestMigrateChain(t *testing.T) {
	// A fake breaking change: project.repository was renamed to project.repo.
	steps := map[int]migration{
		0: addVersionKey,
		1: func(text string) (string, error) { return strings.ReplaceAll(text, "repository =", "repo ="), nil },
	}
	text, err := migrate("[project]\nrepository = \"a/b\"\n#repository = \"x/y\"\n", 0, 2, steps)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if _, err := toml.Decode(text, &cfg); err != nil || cfg.Version != 2 || cfg.Project.Repo != "a/b" || !strings.Contains(text, "#repo = \"x/y\"") {
		t.Fatalf("chain: %+v %v\n%s", cfg, err, text)
	}
	if _, err := migrate("[project]\n", 0, 3, steps); err == nil || !strings.Contains(err.Error(), "version 2 to 3") {
		t.Fatalf("missing step: %v", err)
	}
}
