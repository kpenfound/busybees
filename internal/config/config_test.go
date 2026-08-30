package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	if l.Ready != "bees:ready" || l.Base != "bees" || len(l.All()) != 18 {
		t.Fatalf("labels: %+v", l)
	}
}

func TestSizeLabels(t *testing.T) {
	l := LabelsFor("bees")
	want := []string{"bees:size/xs", "bees:size/s", "bees:size/m", "bees:size/l", "bees:size/xl"}
	if got := l.SizeLabels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("size labels: got %v want %v", got, want)
	}
	// Every size label is created by `bees init` / `bees labels sync`.
	all := map[string]bool{}
	for _, spec := range l.All() {
		all[spec.Name] = true
	}
	for _, name := range want {
		if !all[name] {
			t.Errorf("%s missing from All()", name)
		}
	}
	// Sizes are orthogonal to states: the two sets must not overlap, or
	// setting a state would clear the size.
	states := map[string]bool{}
	for _, s := range l.StateLabels() {
		states[s] = true
	}
	for _, s := range l.SizeLabels() {
		if states[s] {
			t.Errorf("%s is both a state and a size label", s)
		}
	}
}

func TestKindLabels(t *testing.T) {
	l := LabelsFor("bees")
	if l.Proposal != "bees:proposal" {
		t.Fatalf("proposal label: %q", l.Proposal)
	}
	// A proposal is a kind label: it sits next to bees:feature and carries
	// neither a state nor a size, so labelling one must never clear it.
	for _, name := range []string{"state", "size"} {
		list := l.StateLabels()
		if name == "size" {
			list = l.SizeLabels()
		}
		if slices.Contains(list, l.Proposal) {
			t.Errorf("%s is a %s label", l.Proposal, name)
		}
	}
	// `bees init` and `bees labels sync` create it.
	var found bool
	for _, spec := range l.All() {
		if spec.Name != l.Proposal {
			continue
		}
		found = true
		if spec.Color == "" || spec.Description == "" {
			t.Errorf("%s: colour %q description %q", spec.Name, spec.Color, spec.Description)
		}
	}
	if !found {
		t.Errorf("%s missing from All()", l.Proposal)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"bad repo":             "version = 1\n[project]\nrepo = \"nope\"\n",
		"unknown role":         "version = 1\n[project]\nrepo = \"a/b\"\n[roles.intern]\nprompt = \"x\"\n",
		"unknown key":          "version = 1\n[project]\nrepo = \"a/b\"\nrepository = \"x\"\n",
		"mcp no command":       "version = 1\n[project]\nrepo = \"a/b\"\n[global.mcp.x]\nargs = [\"a\"]\n",
		"bad effort":           "version = 1\n[project]\nrepo = \"a/b\"\n[global]\neffort = \"extreme\"\n",
		"label with colon":     "version = 1\n[project]\nrepo = \"a/b\"\n[filter]\nlabel = \"a:b\"\n",
		"open filter":          "version = 1\n[project]\nrepo = \"a/b\"\n[filter]\nrequire_label = false\n",
		"max devs zero":        "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nmax_developers = -1\n",
		"negative retries":     "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nretries = -1\n",
		"too many retries":     "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nretries = 6\n",
		"negative delay":       "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nretry_delay = \"-1m\"\n",
		"bad dispatch order":   "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\ndispatch_order = \"random\"\n",
		"negative large cap":   "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nmax_large_in_flight = -1\n",
		"bad max size":         "version = 1\n[project]\nrepo = \"a/b\"\n[roles.developer]\nmax_size = \"huge\"\n",
		"max size on global":   "version = 1\n[project]\nrepo = \"a/b\"\n[global]\nmax_size = \"l\"\n",
		"max size on reviewer": "version = 1\n[project]\nrepo = \"a/b\"\n[roles.reviewer]\nmax_size = \"l\"\n",
	}
	for name, body := range cases {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The built-in server's name is reserved: a bees.toml entry would silently
// replace the tools every session depends on.
func TestReservedMCPServerName(t *testing.T) {
	for _, scope := range []string{"global", "roles.developer"} {
		body := "version = 1\n[project]\nrepo = \"a/b\"\n[" + scope + ".mcp." + BuiltinMCPServer + "]\ncommand = \"mine\"\n"
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatalf("%s: expected an error", scope)
		}
		want := `mcp server name "bees" is reserved for the built-in server`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to mention %s", scope, err, want)
		}
	}
}

func TestRetryPolicy(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Retry()
	if p.Retries != DefaultRetries || p.Delay != DefaultRetryDelay || !p.WithFallback {
		t.Fatalf("retry defaults: %+v", p)
	}
	// An explicit zero disables retrying and is not overwritten by the default.
	cfg, err = Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nretries = 0\nretry_delay = \"0s\"\nretry_with_fallback = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Retry(); p.Retries != 0 || p.Delay != 0 || p.WithFallback {
		t.Fatalf("explicit zeroes: %+v", p)
	}
	// A zero Config (no Load) still reports the defaults.
	if p := (&Config{}).Retry(); p.Retries != DefaultRetries || p.Delay != DefaultRetryDelay || !p.WithFallback {
		t.Fatalf("zero config: %+v", p)
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

func TestDispatchSettings(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler.DispatchOrder != DispatchSmallFirst || cfg.Scheduler.LargeInFlight() != 1 || cfg.MaxSize() != "l" {
		t.Fatalf("defaults: order %q, large in flight %d, max size %q", cfg.Scheduler.DispatchOrder, cfg.Scheduler.LargeInFlight(), cfg.MaxSize())
	}
	cfg, err = Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\ndispatch_order = \"large-first\"\nmax_large_in_flight = 0\n[roles.developer]\nmax_size = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	// 0 is a meaningful value (no cap), so it must survive applyDefaults.
	if cfg.Scheduler.DispatchOrder != DispatchLargeFirst || cfg.Scheduler.LargeInFlight() != 0 || cfg.MaxSize() != "m" {
		t.Fatalf("custom: order %q, large in flight %d, max size %q", cfg.Scheduler.DispatchOrder, cfg.Scheduler.LargeInFlight(), cfg.MaxSize())
	}
}

func TestPRUpdateSettings(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Scheduler.FixConflicts() || cfg.Scheduler.PRKeepUpdated {
		t.Fatalf("defaults: fix conflicts %v, keep updated %v", cfg.Scheduler.FixConflicts(), cfg.Scheduler.PRKeepUpdated)
	}
	// false is a meaningful value for pr_fix_conflicts, so it must survive applyDefaults.
	cfg, err = Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\npr_fix_conflicts = false\npr_keep_updated = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler.FixConflicts() || !cfg.Scheduler.PRKeepUpdated {
		t.Fatalf("custom: fix conflicts %v, keep updated %v", cfg.Scheduler.FixConflicts(), cfg.Scheduler.PRKeepUpdated)
	}
}

// The error a bad value produces has to say what the valid ones are.
func TestDispatchErrorsListTheValidValues(t *testing.T) {
	for body, want := range map[string]string{
		"version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\ndispatch_order = \"random\"\n": "small-first, oldest, large-first",
		"version = 1\n[project]\nrepo = \"a/b\"\n[roles.developer]\nmax_size = \"huge\"\n":   "xs, s, m, l, xl",
		"version = 1\n[project]\nrepo = \"a/b\"\n[global]\nmax_size = \"l\"\n":               "only valid under roles.developer",
	} {
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatalf("%q: expected an error", body)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
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
	cfg, err := Load(writeConfig(t, uncommentTemplate(text)))
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

// exampleTOML is the reference config committed at the repository root and
// linked from the README. It must stay byte-for-byte what the template
// renders, so nobody starts from a config that is missing keys.
const exampleTOML = "../../bees.example.toml"

var update = flag.Bool("update", false, "rewrite bees.example.toml from the template")

func TestExampleTOMLInSync(t *testing.T) {
	want, err := Template(TemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(exampleTOML, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("rewrote " + exampleTOML)
		return
	}
	got, err := os.ReadFile(exampleTOML)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("bees.example.toml is out of date with internal/config/template.go.\n"+
			"Regenerate it with: go test ./internal/config -update\n%s",
			firstDiff(string(got), want))
	}
}

// firstDiff reports the first line where got and want differ, with a little
// context, so the failure says what drifted instead of dumping 250 lines.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		gl, wl := "<end of file>", "<end of file>"
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("first difference at line %d:\n  file:     %q\n  template: %q", i+1, gl, wl)
		}
	}
	return ""
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

func TestSkillsRefresh(t *testing.T) {
	for _, c := range []struct {
		value  string
		always bool
		after  time.Duration
	}{
		{"", false, 24 * time.Hour},
		{"never", false, 0},
		{"always", true, 0},
		{"12h", false, 12 * time.Hour},
		{"0s", false, 0},
	} {
		body := "version = 1\n[project]\nrepo = \"a/b\"\n[global]\n"
		if c.value != "" {
			body += "skills_refresh = \"" + c.value + "\"\n"
		}
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("%q: %v", c.value, err)
		}
		always, after := cfg.SkillsRefresh()
		if always != c.always || after != c.after {
			t.Errorf("%q: got always=%v after=%v", c.value, always, after)
		}
	}

	for _, bad := range []string{"soon", "-1h", "24"} {
		_, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[global]\nskills_refresh = \""+bad+"\"\n"))
		if err == nil {
			t.Fatalf("%q: expected an error", bad)
		}
		if !strings.Contains(err.Error(), `"never"`) || !strings.Contains(err.Error(), `"always"`) || !strings.Contains(err.Error(), "duration") {
			t.Errorf("%q: unhelpful error: %v", bad, err)
		}
	}

	_, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[roles.developer]\nskills_refresh = \"1h\"\n"))
	if err == nil || !strings.Contains(err.Error(), "skills_refresh is only valid under global") {
		t.Fatalf("roles.developer.skills_refresh: %v", err)
	}
}

func TestSkillsRefreshPolicy(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.SkillsRefreshPolicy(); got != DefaultSkillsRefresh {
		t.Fatalf("policy %q", got)
	}
}

func TestWorkHoursValidation(t *testing.T) {
	base := "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\n"
	cases := []struct {
		name, body, want string
	}{
		{"am/pm", base + "work_hours = \"9am-5pm\"\n",
			`scheduler.work_hours: want "HH:MM-HH:MM" on a 24-hour clock (e.g. "09:00-18:00"), got "9am-5pm"`},
		{"not a range", base + "work_hours = \"09:00\"\n", `scheduler.work_hours: want "HH:MM-HH:MM"`},
		{"24 hour clock", base + "work_hours = \"09:00-26:00\"\n", `scheduler.work_hours: want "HH:MM-HH:MM"`},
		{"empty window", base + "work_hours = \"09:00-09:00\"\n", "start and end must differ"},
		{"long day name", base + "work_hours = \"09:00-18:00\"\nwork_days = [\"monday\"]\n",
			`scheduler.work_days: unknown day "monday" (want one of mon tue wed thu fri sat sun)`},
		{"no days", base + "work_hours = \"09:00-18:00\"\nwork_days = []\n",
			"scheduler.work_days must list at least one of mon tue wed thu fri sat sun"},
		{"bad timezone", base + "work_hours = \"09:00-18:00\"\ntimezone = \"Mars/Olympus\"\n", "scheduler.timezone: "},
		{"off hours too short", base + "poll_interval = \"5m\"\noff_hours_poll_interval = \"1m\"\nwork_hours = \"09:00-18:00\"\n",
			"scheduler.off_hours_poll_interval (1m0s) must be >= scheduler.poll_interval (5m0s)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, c.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
	// The keys are ignored (and never rejected) while work_hours is empty.
	cfg, err := Load(writeConfig(t, base+"off_hours_poll_interval = \"1s\"\nwork_days = [\"monday\"]\ntimezone = \"Mars/Olympus\"\n"))
	if err != nil {
		t.Fatalf("work_hours unset should disable the checks: %v", err)
	}
	if cfg.Scheduler.WorkHoursEnabled() {
		t.Fatal("feature should be disabled without work_hours")
	}
	if got := cfg.Scheduler.PollIntervalAt(time.Date(2026, 8, 30, 3, 0, 0, 0, time.UTC)); got != DefaultPollInterval {
		t.Fatalf("disabled: poll interval %s", got)
	}
	// An overnight window is valid.
	if _, err := Load(writeConfig(t, base+"work_hours = \"18:00-09:00\"\n")); err != nil {
		t.Fatalf("overnight window rejected: %v", err)
	}
}

func TestWorkHoursDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\nwork_hours = \"09:00-18:00\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Scheduler
	if !s.WorkHoursEnabled() || s.OffHoursPollInterval.Duration != DefaultOffHoursPollInterval {
		t.Fatalf("defaults: %+v", s)
	}
	if strings.Join(s.WorkDays, ",") != "mon,tue,wed,thu,fri" {
		t.Fatalf("work_days default: %v", s.WorkDays)
	}
	if got := s.WorkHoursDescription(); got != "09:00-18:00 mon-fri, Local" {
		t.Fatalf("description: %q", got)
	}
}

func TestInWorkHours(t *testing.T) {
	load := func(t *testing.T, body string) Scheduler {
		t.Helper()
		cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\npoll_interval = \"5m\"\n"+body))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Scheduler
	}
	utc := func(day, hour, min int) time.Time {
		return time.Date(2026, 8, day, hour, min, 0, 0, time.UTC)
	}
	// 2026-08-31 is a Monday, 2026-09-05 a Saturday.
	weekdays := load(t, "work_hours = \"09:00-18:00\"\ntimezone = \"UTC\"\n")
	overnight := load(t, "work_hours = \"22:00-06:00\"\nwork_days = [\"fri\"]\ntimezone = \"UTC\"\n")
	newYork := load(t, "work_hours = \"09:00-18:00\"\ntimezone = \"America/New_York\"\n")
	for _, c := range []struct {
		name string
		s    Scheduler
		t    time.Time
		want bool
	}{
		{"inside", weekdays, utc(31, 12, 0), true},
		{"window start is inclusive", weekdays, utc(31, 9, 0), true},
		{"window end is exclusive", weekdays, utc(31, 18, 0), false},
		{"before the window", weekdays, utc(31, 8, 59), false},
		{"after the window", weekdays, utc(31, 23, 0), false},
		{"weekend", weekdays, utc(29, 12, 0), false},
		{"overnight, evening of the work day", overnight, utc(28, 23, 0), true},
		{"overnight, morning after the work day", overnight, utc(29, 5, 0), true},
		{"overnight, morning of the work day", overnight, utc(28, 5, 0), false},
		{"overnight, evening of another day", overnight, utc(29, 23, 0), false},
		{"overnight, after the window", overnight, utc(29, 6, 0), false},
		{"new york: 13:00 UTC is 09:00 EDT", newYork, utc(31, 13, 0), true},
		{"new york: 12:00 UTC is 08:00 EDT", newYork, utc(31, 12, 0), false},
		{"new york: 22:00 UTC is 18:00 EDT", newYork, utc(31, 22, 0), false},
		{"new york: a UTC Tuesday that is still Monday there", newYork, utc(30, 23, 0), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.InWorkHours(c.t); got != c.want {
				t.Fatalf("InWorkHours(%s) = %v, want %v", c.t, got, c.want)
			}
			want := 5 * time.Minute
			if !c.want {
				want = DefaultOffHoursPollInterval
			}
			if got := c.s.PollIntervalAt(c.t); got != want {
				t.Fatalf("PollIntervalAt(%s) = %s, want %s", c.t, got, want)
			}
		})
	}
	if got := overnight.WorkHoursDescription(); got != "22:00-06:00 fri, UTC" {
		t.Fatalf("description: %q", got)
	}
}

func TestDescribeDays(t *testing.T) {
	days := func(names ...string) map[time.Weekday]bool {
		m := map[time.Weekday]bool{}
		for _, n := range names {
			for _, w := range weekdayNames {
				if w.name == n {
					m[w.day] = true
				}
			}
		}
		return m
	}
	for want, in := range map[string][]string{
		"mon-fri":     {"mon", "tue", "wed", "thu", "fri"},
		"sat,sun":     {"sat", "sun"},
		"mon,wed,fri": {"mon", "wed", "fri"},
		"mon-wed,sun": {"mon", "tue", "wed", "sun"},
		"mon-sun":     {"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
	} {
		if got := describeDays(days(in...)); got != want {
			t.Errorf("describeDays(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestParseWithoutFile(t *testing.T) {
	// The template init renders before it writes anything: empty repo and
	// branch, no file on disk.
	text, err := Template(TemplateData{Remote: DefaultRemote, Label: DefaultLabel})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bees.toml")
	cfg, err := Parse(text, path)
	if err != nil {
		t.Fatalf("rendered template does not parse: %v", err)
	}
	if cfg.Path != path {
		t.Fatalf("Path = %q, want %q", cfg.Path, path)
	}
	if cfg.Project.Repo != "" || cfg.Project.DefaultBranch != "" {
		t.Fatalf("repo/branch should be unset: %+v", cfg.Project)
	}
	if cfg.Scheduler.MaxDevelopers != 1 {
		t.Fatalf("defaults not applied: %+v", cfg.Scheduler)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Parse must not touch %s: %v", path, err)
	}

	// An unknown key fails the same way it does through Load.
	bad := "version = 1\n[project]\nnope = true\n"
	_, parseErr := Parse(bad, path)
	_, loadErr := Load(writeConfig(t, bad))
	if parseErr == nil || loadErr == nil {
		t.Fatalf("unknown key accepted: parse=%v load=%v", parseErr, loadErr)
	}
	if !strings.Contains(parseErr.Error(), "unknown keys: project.nope") {
		t.Fatalf("Parse error: %v", parseErr)
	}
	if !strings.Contains(loadErr.Error(), "unknown keys: project.nope") {
		t.Fatalf("Load error: %v", loadErr)
	}
}

// uncommentTemplate turns every commented-out option in the bees.toml template
// into a live setting, dropping the prose comments.
func uncommentTemplate(text string) string {
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
	return strings.Join(lines, "\n")
}
