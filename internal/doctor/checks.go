package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/versions"
	"github.com/kpenfound/busybees/internal/workspace"
)

// MinClaudeVersion is the Claude Code release doctor expects. It is the
// version bees refuses to start below (versions.MinClaude), but doctor only
// warns about it so the remaining checks still run and report.
const MinClaudeVersion = versions.MinClaude

// Deps is everything the checks run against. New builds it from a bees.toml;
// tests fill it in by hand, replacing GitHub.Exec, ClaudeBin, LookPath and
// Git with fakes.
type Deps struct {
	// ConfigPath is the bees.toml doctor was pointed at ("" when none was found).
	ConfigPath string
	// Config is the loaded configuration, or nil when it could not be loaded.
	Config *config.Config
	// ConfigErr is why Config is nil; ResolveErr is why project.repo or
	// project.default_branch could not be derived from the git remote.
	ConfigErr  error
	ResolveErr error

	// GitHub runs gh. It is set whenever the repository is known; the checks
	// that do not need a repository (gh auth status) work without it.
	GitHub *github.Client
	// Workspaces creates the throwaway worktree of the workspace check.
	Workspaces *workspace.Manager
	// ClaudeBin is the claude executable. Default "claude".
	ClaudeBin string

	// LookPath and Git default to exec.LookPath and workspace.Git.
	LookPath func(file string) (string, error)
	Git      func(ctx context.Context, dir string, args ...string) (string, error)
}

// New loads the bees.toml at configPath and returns the dependencies the
// checks run against. It never fails: a configuration that does not load or
// does not resolve is reported by the config checks instead, so the toolchain
// checks still run on a machine that has no bees.toml yet.
func New(ctx context.Context, configPath, claudeBin string) *Deps {
	d := &Deps{ConfigPath: configPath, ClaudeBin: claudeBin}
	cfg, err := config.Load(configPath)
	if err != nil {
		d.ConfigErr = err
		return d
	}
	d.Config = cfg
	d.ResolveErr = cfg.Resolve(ctx)
	if cfg.Project.Repo != "" {
		d.GitHub = github.New(cfg.Project.Repo)
	}
	// Keep is deliberately left off even when scheduler.keep_workspaces is
	// set: doctor's worktree is a probe and always cleans up after itself.
	ws := workspace.NewManager(cfg.Dir(), cfg.Scheduler.WorkspaceRoot)
	ws.Remote = cfg.Project.Remote
	d.Workspaces = ws
	return d
}

// Checks returns the checks that can be run with these dependencies, in the
// order they are printed. Checks that need a configuration are left out when
// bees.toml did not load, and the GitHub and workspace checks are left out
// when the repository could not be resolved: the config checks say why.
func (d *Deps) Checks() []Check {
	checks := []Check{d.checkGit, d.checkGH, d.checkClaude, d.checkConfigLoads}
	if d.Config == nil {
		return checks
	}
	checks = append(checks, d.checkProject, d.checkRemote, d.checkStateDirIgnored,
		d.checkNotesWritable, d.checkPromptFiles)
	if d.Config.Project.Repo != "" {
		checks = append(checks, d.checkRepoAccess, d.checkLabels, d.checkFilter)
	}
	if d.Workspaces != nil && d.Config.Project.DefaultBranch != "" {
		checks = append(checks, d.checkWorktree)
	}
	return checks
}

func (d *Deps) lookPath(file string) (string, error) {
	if d.LookPath != nil {
		return d.LookPath(file)
	}
	return exec.LookPath(file)
}

func (d *Deps) git(ctx context.Context, dir string, args ...string) (string, error) {
	if d.Git != nil {
		return d.Git(ctx, dir, args...)
	}
	return workspace.Git(ctx, dir, args...)
}

// gh runs a gh command through the client when there is one (so tests can
// fake it) and with a repository-less client otherwise.
func (d *Deps) gh(ctx context.Context, args ...string) ([]byte, error) {
	c := d.GitHub
	if c == nil {
		c = github.New("")
	}
	return c.Exec(ctx, args...)
}

func (d *Deps) claudeBin() string {
	if d.ClaudeBin != "" {
		return d.ClaudeBin
	}
	return "claude"
}

// ---- toolchain -------------------------------------------------------------

func (d *Deps) checkGit(ctx context.Context) Result {
	const name = "git"
	path, err := d.lookPath("git")
	if err != nil {
		return fail(name, GroupToolchain, "not found on PATH",
			"install git and make sure it is on the PATH bees runs with")
	}
	out, err := d.git(ctx, "", "--version")
	if err != nil {
		return fail(name, GroupToolchain, fmt.Sprintf("%s cannot be run: %s", path, oneLine(err.Error())),
			"reinstall git")
	}
	return pass(name, GroupToolchain, fmt.Sprintf("%s (%s)", path, oneLine(string(out))))
}

// scopesLine matches the "- Token scopes: 'repo', 'workflow'" line of
// `gh auth status`; older releases print the names unquoted.
var scopesLine = regexp.MustCompile(`(?m)^.*Token scopes:(.*)$`)

// accountLine matches "✓ Logged in to github.com account kyle (keyring)".
var accountLine = regexp.MustCompile(`account ([^\s(]+)`)

func (d *Deps) checkGH(ctx context.Context) Result {
	const name = "gh authenticated"
	if _, err := d.lookPath("gh"); err != nil {
		return fail(name, GroupToolchain, "gh not found on PATH",
			"install the GitHub CLI (https://cli.github.com) — bees drives GitHub through it")
	}
	// gh >= 2.50 (the version bees requires) prints auth status on stdout;
	// a failure goes to stderr, which the client folds into the error.
	out, err := d.gh(ctx, "auth", "status")
	if err != nil {
		return fail(name, GroupToolchain, oneLine(err.Error()),
			"run `gh auth login` (bees uses your existing gh authentication)")
	}
	text := string(out)
	account := ""
	if m := accountLine.FindStringSubmatch(text); m != nil {
		account = m[1]
	}
	scopes := parseScopes(text)
	if scopes == nil {
		// A fine-grained token or a host gh did not report scopes for:
		// nothing to check, and gh itself accepted the token.
		return pass(name, GroupToolchain, describeAccount(account)+", token scopes not reported")
	}
	if !slices.Contains(scopes, "repo") {
		return fail(name, GroupToolchain,
			fmt.Sprintf("%s, token scopes: %s (no repo scope)", describeAccount(account), strings.Join(scopes, ", ")),
			"run `gh auth refresh -s repo` — without it bees cannot label issues or push branches")
	}
	return pass(name, GroupToolchain, fmt.Sprintf("%s, token scopes: %s", describeAccount(account), strings.Join(scopes, ", ")))
}

func describeAccount(account string) string {
	if account == "" {
		return "logged in"
	}
	return "logged in as " + account
}

// parseScopes returns the token scopes reported by `gh auth status`, or nil
// when it did not report any.
func parseScopes(text string) []string {
	var out []string
	for _, m := range scopesLine.FindAllStringSubmatch(text, -1) {
		for _, s := range strings.Split(m[1], ",") {
			s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "'\""))
			if s != "" && s != "none" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	if out == nil && scopesLine.MatchString(text) {
		// "Token scopes: none": reported, and empty.
		return []string{}
	}
	return out
}

func (d *Deps) checkClaude(ctx context.Context) Result {
	const name = "claude runnable"
	bin := d.claudeBin()
	path := bin
	if !strings.ContainsRune(bin, filepath.Separator) {
		p, err := d.lookPath(bin)
		if err != nil {
			return fail(name, GroupToolchain, fmt.Sprintf("%s not found on PATH", bin),
				"install Claude Code (https://claude.com/claude-code), or set $BEES_CLAUDE_BIN to its path")
		}
		path = p
	}
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return fail(name, GroupToolchain, fmt.Sprintf("%s --version failed: %s", path, oneLine(string(out)+" "+err.Error())),
			"check that "+path+" is a working Claude Code installation")
	}
	got, err := versions.Parse(string(out))
	if err != nil {
		return warn(name, GroupToolchain, fmt.Sprintf("%s: no version number in %q", path, oneLine(string(out))),
			"check that "+path+" is Claude Code; bees needs "+MinClaudeVersion+" or newer")
	}
	if min, perr := versions.Parse(MinClaudeVersion); perr == nil && got.Less(min) {
		return warn(name, GroupToolchain, fmt.Sprintf("%s %s at %s", "claude", got, path),
			fmt.Sprintf("update Claude Code: bees needs %s or newer (run `claude update`)", MinClaudeVersion))
	}
	return pass(name, GroupToolchain, fmt.Sprintf("claude %s at %s", got, path))
}

// ---- config ----------------------------------------------------------------

func (d *Deps) checkConfigLoads(context.Context) Result {
	const name = "bees.toml valid"
	if d.ConfigErr != nil {
		if d.ConfigPath == "" {
			return fail(name, GroupConfig, oneLine(d.ConfigErr.Error()),
				"run `bees init` in the project's git clone, or point --config at an existing bees.toml")
		}
		return fail(name, GroupConfig, d.ConfigPath+": "+oneLine(d.ConfigErr.Error()),
			"run `bees config validate` for the full error, then fix bees.toml")
	}
	return pass(name, GroupConfig, fmt.Sprintf("%s (version %d)", d.Config.Path, d.Config.Version))
}

func (d *Deps) checkProject(ctx context.Context) Result {
	const name = "project repo"
	cfg := d.Config
	if d.ResolveErr != nil {
		return fail(name, GroupConfig, oneLine(d.ResolveErr.Error()),
			`set project.repo = "owner/name" and project.default_branch in bees.toml, or point the remote at a GitHub URL`)
	}
	url, err := d.git(ctx, cfg.Dir(), "remote", "get-url", cfg.Project.Remote)
	if err != nil {
		return fail(name, GroupConfig, fmt.Sprintf("remote %q: %s", cfg.Project.Remote, oneLine(err.Error())),
			fmt.Sprintf("add the remote (`git remote add %s <url>`) or set project.remote in bees.toml", cfg.Project.Remote))
	}
	if _, ok := config.ParseGitHubRepo(url); !ok {
		return fail(name, GroupConfig, fmt.Sprintf("remote %q is %s, not a GitHub URL", cfg.Project.Remote, url),
			"bees drives GitHub through gh: point the remote at github.com, or set project.remote to one that is")
	}
	return pass(name, GroupConfig, fmt.Sprintf("%s, default branch %s (remote %q)",
		cfg.Project.Repo, cfg.Project.DefaultBranch, cfg.Project.Remote))
}

func (d *Deps) checkRemote(ctx context.Context) Result {
	const name = "remote reachable"
	cfg := d.Config
	if _, err := d.git(ctx, cfg.Dir(), "ls-remote", "--exit-code", cfg.Project.Remote, "HEAD"); err != nil {
		return fail(name, GroupConfig, fmt.Sprintf("%s: %s", cfg.Project.Remote, oneLine(err.Error())),
			"check the network and your git credentials (`gh auth setup-git` for https remotes)")
	}
	return pass(name, GroupConfig, fmt.Sprintf("%s answers", cfg.Project.Remote))
}

func (d *Deps) checkStateDirIgnored(ctx context.Context) Result {
	const name = "state dir ignored"
	cfg := d.Config
	rel, err := filepath.Rel(cfg.Dir(), cfg.StateDir())
	if err != nil || strings.HasPrefix(rel, "..") {
		return pass(name, GroupConfig, cfg.StateDir()+" is outside the clone")
	}
	// Ask about a path inside the state dir: a "/.bees/" rule only matches
	// the bare directory once it exists on disk, and doctor should give the
	// same answer before and after the first session.
	if _, err := d.git(ctx, cfg.Dir(), "check-ignore", "-q", filepath.Join(rel, "notes")); err != nil {
		line := "/" + filepath.ToSlash(rel) + "/"
		return warn(name, GroupConfig, fmt.Sprintf("%s is not ignored by git", rel),
			fmt.Sprintf("add %q to .gitignore: notes, mail and session transcripts would be committed otherwise", line))
	}
	return pass(name, GroupConfig, rel+" is ignored")
}

func (d *Deps) checkNotesWritable(context.Context) Result {
	const name = "notes dir writable"
	dir := filepath.Join(d.Config.StateDir(), "notes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(name, GroupConfig, oneLine(err.Error()),
			"create the state directory by hand, or point project.state_dir somewhere writable")
	}
	f, err := os.CreateTemp(dir, ".doctor-")
	if err != nil {
		return fail(name, GroupConfig, oneLine(err.Error()),
			fmt.Sprintf("make %s writable: the roles' notes are their only memory between sessions", dir))
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return pass(name, GroupConfig, dir)
}

func (d *Deps) checkPromptFiles(context.Context) Result {
	const name = "prompt files exist"
	cfg := d.Config
	type entry struct{ scope, path string }
	var entries []entry
	if cfg.Global.PromptFile != "" {
		entries = append(entries, entry{"global", cfg.Global.PromptFile})
	}
	for _, role := range config.Roles {
		if p := cfg.Roles[role].PromptFile; p != "" {
			entries = append(entries, entry{"roles." + role, p})
		}
	}
	if len(entries) == 0 {
		return pass(name, GroupConfig, "no prompt_file configured")
	}
	var missing, found []string
	for _, e := range entries {
		p := e.path
		if !filepath.IsAbs(p) {
			p = filepath.Join(cfg.Dir(), p)
		}
		if _, err := os.Stat(p); err != nil {
			missing = append(missing, fmt.Sprintf("%s.prompt_file %s", e.scope, e.path))
			continue
		}
		found = append(found, e.path)
	}
	if len(missing) > 0 {
		return fail(name, GroupConfig, "missing: "+strings.Join(missing, ", "),
			"create the files, or remove the prompt_file keys from bees.toml")
	}
	return pass(name, GroupConfig, strings.Join(found, ", "))
}

// ---- github ----------------------------------------------------------------

// writePermissions are the viewerPermission values that allow pushing.
var writePermissions = []string{"ADMIN", "MAINTAIN", "WRITE"}

func (d *Deps) checkRepoAccess(ctx context.Context) Result {
	const name = "repo readable and writable"
	repo := d.Config.Project.Repo
	out, err := d.gh(ctx, "repo", "view", repo, "--json", "nameWithOwner,viewerPermission")
	if err != nil {
		return fail(name, GroupGitHub, oneLine(err.Error()),
			fmt.Sprintf("check that project.repo (%s) is right and that your gh account can see it", repo))
	}
	var view struct {
		NameWithOwner    string `json:"nameWithOwner"`
		ViewerPermission string `json:"viewerPermission"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return fail(name, GroupGitHub, "could not read `gh repo view`: "+oneLine(err.Error()),
			"upgrade the GitHub CLI: bees needs gh "+versions.MinGH+" or newer")
	}
	switch {
	case view.ViewerPermission == "":
		return warn(name, GroupGitHub, repo+": GitHub reported no permission level",
			"check with `gh repo view "+repo+" --json viewerPermission` that the account can push")
	case !slices.Contains(writePermissions, strings.ToUpper(view.ViewerPermission)):
		return fail(name, GroupGitHub, fmt.Sprintf("%s: permission %s", repo, view.ViewerPermission),
			"developers push branches and open pull requests: ask for write access to "+repo)
	}
	return pass(name, GroupGitHub, fmt.Sprintf("%s (%s)", repo, view.ViewerPermission))
}

func (d *Deps) checkLabels(ctx context.Context) Result {
	const name = "workflow labels"
	out, err := d.gh(ctx, "label", "list", "-R", d.Config.Project.Repo, "--limit", "200", "--json", "name")
	if err != nil {
		return fail(name, GroupGitHub, oneLine(err.Error()), "run `bees labels sync` to create the workflow labels")
	}
	var have []github.Label
	if err := json.Unmarshal(out, &have); err != nil {
		return fail(name, GroupGitHub, "could not read `gh label list`: "+oneLine(err.Error()),
			"upgrade the GitHub CLI: bees needs gh "+versions.MinGH+" or newer")
	}
	want := d.Config.Labels().All()
	var missing []string
	for _, l := range want {
		if !hasLabel(have, l.Name) {
			missing = append(missing, l.Name)
		}
	}
	if len(missing) > 0 {
		return fail(name, GroupGitHub, fmt.Sprintf("%d of %d missing: %s", len(missing), len(want), strings.Join(missing, ", ")),
			"run `bees labels sync`")
	}
	return pass(name, GroupGitHub, fmt.Sprintf("all %d present", len(want)))
}

func hasLabel(labels []github.Label, name string) bool {
	for _, l := range labels {
		// GitHub label names are case-insensitive for creation, so a label
		// that only differs in case is the same label.
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

func (d *Deps) checkFilter(ctx context.Context) Result {
	const name = "filter matches issues"
	q := Query(d.Config)
	issues, err := d.GitHub.ListOpenIssues(ctx, q)
	if err != nil {
		return fail(name, GroupGitHub, oneLine(err.Error()),
			"check that gh can list issues in "+d.Config.Project.Repo)
	}
	if len(issues) == 0 {
		return warn(name, GroupGitHub, fmt.Sprintf("no open issue matches %s", describeQuery(q)),
			"check filter.label, filter.assignee and filter.milestone in bees.toml, or file the first issue "+
				"(the factory only sees issues that match)")
	}
	return pass(name, GroupGitHub, fmt.Sprintf("%s matching %s", plural(len(issues), "open issue"), describeQuery(q)))
}

// Query is the visibility filter as the scheduler applies it.
func Query(cfg *config.Config) github.Query {
	f := cfg.Filter
	q := github.Query{Assignee: f.Assignee, Milestone: f.Milestone}
	if f.LabelRequired() {
		q.Label = f.Label
	}
	return q
}

func describeQuery(q github.Query) string {
	var parts []string
	if q.Label != "" {
		parts = append(parts, "label "+q.Label)
	}
	if q.Assignee != "" {
		parts = append(parts, "assignee "+q.Assignee)
	}
	if q.Milestone != "" {
		parts = append(parts, "milestone "+q.Milestone)
	}
	if len(parts) == 0 {
		return "the empty filter (every open issue)"
	}
	return strings.Join(parts, " + ")
}

// ---- workspace -------------------------------------------------------------

func (d *Deps) checkWorktree(ctx context.Context) Result {
	const name = "worktree"
	cfg := d.Config
	ws, err := d.Workspaces.Detached(ctx, "doctor", cfg.Project.DefaultBranch)
	if err != nil {
		return fail(name, GroupWorkspace, oneLine(err.Error()),
			fmt.Sprintf("check that %s is writable and that %s/%s exists (run `git fetch %s`)",
				d.Workspaces.Root, cfg.Project.Remote, cfg.Project.DefaultBranch, cfg.Project.Remote))
	}
	if err := d.Workspaces.Remove(ctx, ws); err != nil {
		return warn(name, GroupWorkspace, fmt.Sprintf("created %s but could not remove it: %s", ws.RepoDir, oneLine(err.Error())),
			"remove it by hand and run `git worktree prune`: stale worktrees fill up "+d.Workspaces.Root)
	}
	return pass(name, GroupWorkspace, fmt.Sprintf("created and removed one under %s", d.Workspaces.Root))
}

// ---- helpers ---------------------------------------------------------------

// oneLine collapses a multi-line message (a validation error listing every
// problem, git's stderr) into a single table cell.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
