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
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/skills"
	"github.com/kpenfound/busybees/internal/text"
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
	// Skills clones the skills of the per-role checks. It uses the cache a
	// session uses, so doctor warms it rather than duplicating it.
	Skills *skills.Manager
	// ClaudeBin is the claude executable. Default "claude".
	ClaudeBin string

	// MachineGitHub runs the one gh command that is about the machine's own
	// authentication rather than the repository: `gh auth status`. It never
	// carries github.token - see machineGH.
	MachineGitHub *github.Client

	// LookPath, Git and CurrentUser default to exec.LookPath, workspace.Git
	// and github.CurrentUser.
	LookPath    func(file string) (string, error)
	Git         func(ctx context.Context, dir string, args ...string) (string, error)
	CurrentUser func(ctx context.Context) (string, error)
}

// New loads the bees.toml at configPath and returns the dependencies the
// checks run against. It never fails: a configuration that does not load or
// does not resolve is reported by the config checks instead, so the toolchain
// checks still run on a machine that has no bees.toml yet.
func New(ctx context.Context, configPath, claudeBin string) *Deps {
	d := &Deps{ConfigPath: configPath, ClaudeBin: claudeBin, MachineGitHub: github.New("")}
	cfg, err := config.Load(configPath)
	if err != nil {
		d.ConfigErr = err
		return d
	}
	d.Config = cfg
	d.ResolveErr = cfg.Resolve(ctx)
	if cfg.Project.Repo != "" {
		// The checks answer for the account the factory acts as, so they
		// carry the same token the orchestrator's own calls do (config's
		// [github] table; empty means the machine's own gh auth).
		d.GitHub = github.NewAs(cfg.Project.Repo, cfg.GitHub.Login, cfg.GitHub.ResolvedToken())
	}
	// Keep is deliberately left off even when scheduler.keep_workspaces is
	// set: doctor's worktree is a probe and always cleans up after itself.
	ws := workspace.NewManager(cfg.Dir(), cfg.Scheduler.WorkspaceRoot)
	ws.Remote = cfg.Project.Remote
	d.Workspaces = ws
	sk := skills.NewManager(skills.CacheDir())
	sk.RefreshAlways, sk.RefreshAfter = cfg.SkillsRefresh()
	d.Skills = sk
	return d
}

// Checks returns the checks that can be run with these dependencies, in the
// order they are printed. Checks that need a configuration are left out when
// bees.toml did not load, and the GitHub and workspace checks are left out
// when the repository could not be resolved: the config checks say why.
func (d *Deps) Checks() []Check {
	checks := []Check{{Run: d.checkGit}, {Run: d.checkGH}, {Run: d.checkClaude}, {Run: d.checkConfigLoads}}
	if d.Config == nil {
		return checks
	}
	checks = append(checks, Check{Run: d.checkProject}, Check{Run: d.checkRemote},
		Check{Run: d.checkStateDirIgnored}, Check{Run: d.checkNotesWritable}, Check{Run: d.checkPromptFiles},
		Check{Run: d.checkProjectPrompts})
	if d.Config.Project.Repo != "" {
		checks = append(checks, Check{Run: d.checkRepoAccess}, Check{Run: d.checkLabels},
			Check{Run: d.checkFilter, Fix: d.fixFilter}, Check{Run: d.checkAutoMerge})
	}
	if d.Workspaces != nil && d.Config.Project.DefaultBranch != "" {
		checks = append(checks, Check{Run: d.checkWorktree})
	}
	// Last, because they are the slow ones: cloning skills and starting MCP
	// servers. CheapChecks drops them for the `bees run` preflight.
	return append(checks, d.roleChecks()...)
}

func (d *Deps) lookPath(file string) (string, error) {
	if d.LookPath != nil {
		return d.LookPath(file)
	}
	return exec.LookPath(file)
}

// me is the login of the person running bees. filter.assignee = "@me" says
// whose work the factory picks up, which is theirs, so it is resolved with
// their own gh authentication - never through d.gh, whose client carries
// github.token and would answer as the account the factory acts as.
func (d *Deps) me(ctx context.Context) (string, error) {
	if d.CurrentUser != nil {
		return d.CurrentUser(ctx)
	}
	return github.CurrentUser(ctx)
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

// machineGH runs a gh command as the machine owner, never as the account
// [github] configures. Sessions authenticate with the machine's own gh
// whatever [github] says, so the check that inspects that authentication has
// to ask about it - and `gh auth status` run with GH_TOKEN set reports the
// token's account first and unions the two accounts' scopes, which is exactly
// the merge hostBlock exists to prevent.
func (d *Deps) machineGH(ctx context.Context, args ...string) ([]byte, error) {
	c := d.MachineGitHub
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

// ghHost is the only GitHub host bees talks to: config.ParseGitHubRepo accepts
// github.com and nothing else. The check is scoped to it because `gh auth
// status` reports every logged-in host, and merging an enterprise token's
// scopes into the answer would report a pass for a github.com token that is
// missing `repo` — the one failure this check exists to catch.
const ghHost = "github.com"

// scopesLine matches the "- Token scopes: 'repo', 'workflow'" line of
// `gh auth status`; older releases print the names unquoted.
var scopesLine = regexp.MustCompile(`(?m)^.*Token scopes:(.*)$`)

// accountLine matches "✓ Logged in to github.com account kyle (keyring)".
var accountLine = regexp.MustCompile(`account ([^\s(]+)`)

// unknownAssignee matches the GraphQL error `gh issue list --assignee X`
// answers with when X is not a GitHub login ("Could not find an assignee with
// the login of 'kylpenfound'."). The client folds stderr into the error, so it
// is matched anywhere in the message.
var unknownAssignee = regexp.MustCompile(`(?i)could not find an assignee with the login`)

func (d *Deps) checkGH(ctx context.Context) Result {
	const name = "gh authenticated"
	if _, err := d.lookPath("gh"); err != nil {
		return fail(name, GroupToolchain, "gh not found on PATH",
			"install the GitHub CLI (https://cli.github.com) — bees drives GitHub through it")
	}
	// gh >= 2.50 (the version bees requires) prints auth status on stdout;
	// a failure goes to stderr, which the client folds into the error.
	// --hostname makes gh exit non-zero when that host is not logged in, which
	// the error branch below already turns into the right answer for bees.
	out, err := d.machineGH(ctx, "auth", "status", "--hostname", ghHost)
	if err != nil {
		return fail(name, GroupToolchain, oneLine(err.Error()),
			"run `gh auth login` (sessions use your own gh authentication, whatever [github] configures)")
	}
	text := hostBlock(string(out), ghHost)
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
		detail := describeAccount(account) + ", " + describeScopes(scopes)
		if len(scopes) > 0 {
			// "no token scopes" already says it; only a list needs the verdict.
			detail += " (no repo scope)"
		}
		return fail(name, GroupToolchain, detail,
			"run `gh auth refresh -s repo` — without it bees cannot label issues or push branches")
	}
	return pass(name, GroupToolchain, describeAccount(account)+", "+describeScopes(scopes))
}

// hostBlock returns the part of `gh auth status` output that describes host.
// gh prints one block per logged-in host: an unindented host name followed by
// indented detail lines. Output with no host header at all is returned whole.
func hostBlock(text, host string) string {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			continue
		}
		if start >= 0 {
			return strings.Join(lines[start:i], "\n")
		}
		if strings.TrimSpace(l) == host {
			start = i
		}
	}
	if start >= 0 {
		return strings.Join(lines[start:], "\n")
	}
	return text
}

// describeScopes renders the scopes parseScopes reported. The empty (but not
// nil) slice is `Token scopes: none`, which has no list to print.
func describeScopes(scopes []string) string {
	if len(scopes) == 0 {
		return "no token scopes"
	}
	return "token scopes: " + strings.Join(scopes, ", ")
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

// checkProjectPrompts inspects bees/prompts/ in the repository bees.toml sits
// in: the role instructions a project versions with its code. A repository
// with no such directory is the normal case and passes silently.
//
// It reads the main clone. A session reads the same files from its own
// worktree, so a branch can carry instructions this check never sees - which
// is the point of the feature, and why the detail names the checkout.
func (d *Deps) checkProjectPrompts(context.Context) Result {
	const name = "project prompt files"
	dir := d.Config.Dir()
	known, unknown, err := prompts.ProjectPromptFiles(dir)
	if err != nil {
		return fail(name, GroupConfig, oneLine(err.Error()),
			fmt.Sprintf("make %s readable, or remove it", filepath.Join(dir, prompts.ProjectDir)))
	}
	if len(known) == 0 && len(unknown) == 0 {
		return pass(name, GroupConfig, "no "+prompts.ProjectDir+"/ directory")
	}
	if len(unknown) > 0 {
		return fail(name, GroupConfig, "not read by any role: "+strings.Join(unknown, ", "),
			fmt.Sprintf("rename to %s or one of %s.md, or delete the file",
				prompts.CommonPromptFile, strings.Join(config.Roles, ".md, ")))
	}
	// Every file that a role would read has to be readable and within the
	// size limit, whichever role it belongs to.
	var broken []string
	for _, role := range config.Roles {
		if _, err := prompts.LoadProject(dir, role); err != nil {
			broken = append(broken, oneLine(err.Error()))
		}
	}
	if len(broken) > 0 {
		return fail(name, GroupConfig, strings.Join(dedupe(broken), "; "),
			"fix the file: every session reading that role's prompt skips it with a warning")
	}
	return pass(name, GroupConfig, strings.Join(known, ", "))
}

// dedupe drops repeated strings, keeping the first of each. common.md is read
// by every role, so a broken one is reported once rather than five times.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
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
			fmt.Sprintf("check that project.repo (%s) is right and that the account bees acts as can see it", repo))
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

// notProtected matches gh's answer for a branch with no protection rules at
// all. It is a 404, but it is the answer to the question, not an error.
var notProtected = regexp.MustCompile(`(?i)branch not protected|HTTP 404`)

// checkAutoMerge reports what auto_merge will actually gate a merge on.
//
// With no branch protection `gh pr checks --required` reports nothing, so
// before #117 an auto-merging factory merged with nothing green at all. It
// now falls back to every check a pull request reports, which is a sound
// default but is not the same promise as "the checks you marked required".
// This check says which of the two is in force, once, in plain words.
//
// It is a Warn and never a Fail: #48 wires doctor into the `bees run`
// preflight, which refuses to start on a failure, and an unprotected default
// branch must not stop the factory. bees never enables or edits branch
// protection - that is a person's setting.
func (d *Deps) checkAutoMerge(ctx context.Context) Result {
	const name = "auto_merge check gate"
	cfg := d.Config
	if !cfg.Merge().AutoMerge {
		return pass(name, GroupGitHub, "auto_merge is off: people merge pull requests themselves")
	}
	branch := cfg.Project.DefaultBranch
	if branch == "" {
		return warn(name, GroupGitHub, "auto_merge is on and project.default_branch is not known, so its protection rules cannot be read",
			"set project.default_branch in bees.toml")
	}
	out, err := d.gh(ctx, "api", fmt.Sprintf("repos/%s/branches/%s/protection", cfg.Project.Repo, branch))
	if err != nil {
		if notProtected.MatchString(err.Error()) {
			return unrequiredGate(name, branch, "`"+branch+"` is not protected")
		}
		// A token without admin rights on the repository cannot read the
		// protection rules. That is not a broken setup, and the fallback gate
		// works either way.
		return warn(name, GroupGitHub,
			fmt.Sprintf("auto_merge is on and the branch protection of `%s` could not be read: %s", branch, oneLine(err.Error())),
			"reading branch protection needs admin rights on the repository; without it bees cannot tell you "+
				"which checks are required, and will gate a merge on whatever checks a pull request reports")
	}
	var prot struct {
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		} `json:"required_status_checks"`
	}
	if err := json.Unmarshal(out, &prot); err != nil {
		return warn(name, GroupGitHub,
			fmt.Sprintf("auto_merge is on and the branch protection of `%s` could not be read: %s", branch, oneLine(err.Error())),
			"upgrade the GitHub CLI: bees needs gh "+versions.MinGH+" or newer")
	}
	var required []string
	if rsc := prot.RequiredStatusChecks; rsc != nil {
		required = append(required, rsc.Contexts...)
		for _, c := range rsc.Checks {
			if c.Context != "" && !slices.Contains(required, c.Context) {
				required = append(required, c.Context)
			}
		}
	}
	if len(required) == 0 {
		return unrequiredGate(name, branch, "`"+branch+"` is protected but requires no check")
	}
	return pass(name, GroupGitHub, fmt.Sprintf("auto_merge gates on the %s required on `%s`: %s",
		text.Count(len(required), "check"), branch, strings.Join(required, ", ")))
}

// unrequiredGate is the one warning checkAutoMerge has, for both ways of
// having no required check: bees will merge on the checks it can see.
func unrequiredGate(name, branch, why string) Result {
	return warn(name, GroupGitHub,
		fmt.Sprintf("auto_merge is on and no check is required on `%s` (%s); bees will gate on whatever checks a pull request reports", branch, why),
		"require your CI checks in the branch protection rules for `"+branch+"`, or leave it as it is and bees "+
			"will honour the checks it can see")
}

// checkFilter reports whether the visibility filter matches anything, and,
// when it does not, which of the two very different reasons it is: an empty or
// not-yet-labelled repository, or a filter that just stopped matching the work
// the factory already owns (adding filter.assignee to an installed factory
// hides every issue nobody ever assigned - see #110).
//
// It is a Warn and never a Fail, in both cases: the `bees run` preflight
// refuses to start on a failure, and a filter that matches nothing on purpose
// must still run. The difference between the two is carried entirely by the
// detail and remediation lines - do not "upgrade" this to a Fail.
func (d *Deps) checkFilter(ctx context.Context) Result {
	const name = "filter matches issues"
	q := Query(d.Config)
	if q.Assignee == "@me" {
		// gh resolves "@me" against whatever token the client carries, which
		// with [github] set is the account the factory acts as: ask who is
		// running bees instead, the same way the orchestrator does.
		login, err := d.assignee(ctx)
		if err != nil {
			return fail(name, GroupGitHub, oneLine(err.Error()),
				"run `gh auth login`: bees resolves filter.assignee = \"@me\" with your own gh account, not with github.token")
		}
		q.Assignee = login
	}
	issues, err := d.GitHub.ListOpenIssues(ctx, q)
	if err != nil {
		// A filter.assignee that is not a real login makes gh error instead of
		// answering an empty list (it does not when the query also carries a
		// label). That is a filter that matches nothing, not a broken gh: the
		// configured value is authoritative, so report it from bees.toml.
		if unknownAssignee.MatchString(err.Error()) {
			return warn(name, GroupGitHub,
				fmt.Sprintf("filter.assignee %q is not a GitHub login, so the filter matches nothing", d.Config.Filter.Assignee),
				"fix filter.assignee in bees.toml (it must be a GitHub login) or unset the criterion")
		}
		return fail(name, GroupGitHub, oneLine(err.Error()),
			"check that gh can list issues in "+d.Config.Project.Repo)
	}
	if len(issues) > 0 {
		return pass(name, GroupGitHub, fmt.Sprintf("%s matching %s", text.Count(len(issues), "open issue"), describeQuery(q)))
	}
	if stranded := d.strandedByFilter(ctx, q); stranded != "" {
		return warn(name, GroupGitHub, stranded,
			"filter criteria are ANDed, so every one of them must hold: run `bees doctor --fix` to bring the items "+
				"carrying `"+d.Config.Filter.Label+"` into the filter, or unset the criterion in bees.toml")
	}
	return warn(name, GroupGitHub, fmt.Sprintf("no open issue matches %s", describeQuery(q)),
		"check filter.label, filter.assignee and filter.milestone in bees.toml, or file the first issue "+
			"(the factory only sees issues that match)")
}

// strandedByFilter asks the second question, once the filter has come back
// empty: how much open work carries the base label alone? Pull requests count
// too - a filter change that hides an open PR strands work mid-flight. It
// returns the detail line for that case, or "" when there is nothing to tell
// apart (no base label to count against, the extra listing failed, or the
// repository really is empty).
//
// `bees doctor --fix` repairs exactly this case; see fixFilter.
func (d *Deps) strandedByFilter(ctx context.Context, q github.Query) string {
	// Without require_label there is no base label the factory's own items are
	// guaranteed to carry, so there is nothing to compare against.
	if !d.Config.Filter.LabelRequired() {
		return ""
	}
	base := github.Query{Label: d.Config.Filter.Label}
	if base == q {
		return "" // the label is the whole filter: the first listing was this question
	}
	issues, err := d.GitHub.ListOpenIssues(ctx, base)
	if err != nil {
		return ""
	}
	prs, err := d.GitHub.ListOpenPRs(ctx, base)
	if err != nil {
		return ""
	}
	if len(issues)+len(prs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s and %s carry `%s`, 0 match your filter (%s)",
		text.Count(len(issues), "open issue"), text.Count(len(prs), "pull request"),
		d.Config.Filter.Label, describeANDed(q))
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

// describeANDed spells the filter out as the conjunction it is
// ("label=bees AND assignee=kyle"), for the message that has to make the
// ANDing itself the point.
func describeANDed(q github.Query) string {
	var parts []string
	if q.Label != "" {
		parts = append(parts, "label="+q.Label)
	}
	if q.Assignee != "" {
		parts = append(parts, "assignee="+q.Assignee)
	}
	if q.Milestone != "" {
		parts = append(parts, "milestone="+q.Milestone)
	}
	if len(parts) == 0 {
		return "no criteria"
	}
	return strings.Join(parts, " AND ")
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
