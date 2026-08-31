package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// baseTOML resolves without touching the network: repo and default branch are
// explicit, so config.Resolve has nothing to derive.
const baseTOML = `version = 1

[project]
repo = "owner/name"
default_branch = "main"
`

// fakeGH answers gh invocations from a table keyed by a prefix of the
// arguments ("repo view", "label list"). The real gh is never run.
type fakeGH struct {
	t       *testing.T
	replies map[string]ghReply
	// reply, when set, answers first and can look at the whole argument list;
	// returning false falls through to the replies table. It is how a check
	// that lists twice with different --label/--assignee flags is faked, since
	// those flags come last and a prefix cannot tell the two calls apart.
	reply func(args []string) (ghReply, bool)
	calls [][]string
}

type ghReply struct {
	out string
	err error
}

func (f *fakeGH) install(c *github.Client) {
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args)
		if f.reply != nil {
			if r, ok := f.reply(args); ok {
				return []byte(r.out), r.err
			}
		}
		joined := strings.Join(args, " ")
		for prefix, r := range f.replies {
			if strings.HasPrefix(joined, prefix) {
				return []byte(r.out), r.err
			}
		}
		// Checks run in goroutines, so this cannot be t.Fatalf.
		f.t.Errorf("unexpected gh call: gh %s", joined)
		return nil, fmt.Errorf("unexpected gh call: gh %s", joined)
	}
}

// installAll fakes every gh client a Deps can run a command through, filling
// in the ones New left nil. Deps carries more than one - repository questions
// go through GitHub, `gh auth status` through MachineGitHub - and both
// Deps.gh and Deps.machineGH fall back to a fresh real client when their
// field is nil, so a client this helper misses runs the real gh against the
// machine's own credentials. Installing on all of them here gives a future
// third client one place to update.
func (f *fakeGH) installAll(d *Deps) {
	if d.GitHub == nil {
		d.GitHub = github.New("")
	}
	if d.MachineGitHub == nil {
		d.MachineGitHub = github.New("")
	}
	f.install(d.GitHub)
	f.install(d.MachineGitHub)
}

// fakeClaude writes a shell script standing in for the claude binary, as
// internal/session does.
func fakeClaude(t *testing.T, output string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fixture is a clone with a bees.toml, a fake gh and a fake claude.
type fixture struct {
	*Deps
	clone string
	gh    *fakeGH
}

// setup writes baseTOML plus extra into a fresh clone and loads it. extra is
// appended inside the [project] table, so bare project keys come first and any
// new table ([filter], [roles.x]) after them. Nothing in the returned Deps can
// reach the real gh, claude or PATH.
func setup(t *testing.T, extra string, replies map[string]ghReply) *fixture {
	t.Helper()
	_, clone := testutil.SetupRepos(t)
	return setupIn(t, clone, extra, replies)
}

func setupIn(t *testing.T, clone, extra string, replies map[string]ghReply) *fixture {
	t.Helper()
	path := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(path, []byte(baseTOML+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(context.Background(), path, fakeClaude(t, "2.9.0 (Claude Code)"))
	if d.ConfigErr != nil {
		t.Fatalf("load bees.toml: %v", d.ConfigErr)
	}
	gh := &fakeGH{t: t, replies: replies}
	gh.installAll(d)
	// "who is running bees?" is asked through no client at all, so it needs
	// its own fake or the fixture reaches the real gh.
	d.CurrentUser = func(context.Context) (string, error) { return "kyle", nil }
	d.LookPath = func(file string) (string, error) {
		if file == "git" || file == "gh" {
			return "/usr/bin/" + file, nil
		}
		return "", fmt.Errorf("%s: not found", file)
	}
	return &fixture{Deps: d, clone: clone, gh: gh}
}

// run drives one check function directly. Checks are wrapped in a Check value
// by Deps.Checks; the tests hold the method, which is what a Check's Run is.
func (f *fixture) run(t *testing.T, c func(context.Context) Result) Result {
	t.Helper()
	r := c(context.Background())
	if r.Name == "" || r.Group == "" {
		t.Errorf("check returned an unnamed result: %+v", r)
	}
	if r.Status != Pass && r.Remediation == "" {
		t.Errorf("%s: a %s result must say what to do about it: %+v", r.Name, r.Status, r)
	}
	return r
}

func wantResult(t *testing.T, r Result, status Status, detail ...string) {
	t.Helper()
	if r.Status != status {
		t.Errorf("%s: status %s, want %s (detail: %s)", r.Name, r.Status, status, r.Detail)
	}
	for _, s := range detail {
		if !strings.Contains(r.Detail, s) && !strings.Contains(r.Remediation, s) {
			t.Errorf("%s: %q not in detail %q or remediation %q", r.Name, s, r.Detail, r.Remediation)
		}
	}
}

// ---- toolchain -------------------------------------------------------------

func TestCheckGit(t *testing.T) {
	f := setup(t, "", nil)
	wantResult(t, f.run(t, f.checkGit), Pass, "/usr/bin/git", "git version")

	f.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	wantResult(t, f.run(t, f.checkGit), Fail, "not found on PATH")

	f = setup(t, "", nil)
	f.Git = func(context.Context, string, ...string) (string, error) { return "", errors.New("exec format error") }
	wantResult(t, f.run(t, f.checkGit), Fail, "cannot be run", "exec format error")
}

func TestCheckGH(t *testing.T) {
	const status = "github.com\n" +
		"  ✓ Logged in to github.com account kyle (keyring)\n" +
		"  - Active account: true\n" +
		"  - Token: gho_xxx\n"
	// Two logged-in hosts, only the enterprise one carrying `repo`. gh prints
	// one block per host whatever --hostname says on older releases.
	const multiHostStatus = "github.com\n" +
		"  ✓ Logged in to github.com account kyle (keyring)\n" +
		"  - Token scopes: 'gist'\n" +
		"ghe.corp.example\n" +
		"  ✓ Logged in to ghe.corp.example account corp-kyle (keyring)\n" +
		"  - Token scopes: 'repo'\n"

	cases := []struct {
		name   string
		reply  ghReply
		status Status
		detail []string
	}{
		{"authenticated with the repo scope",
			ghReply{out: status + "  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'\n"},
			Pass, []string{"kyle", "repo"}},
		{"unquoted scopes (older gh)",
			ghReply{out: status + "  - Token scopes: gist, repo\n"},
			Pass, []string{"gist, repo"}},
		{"missing repo scope",
			ghReply{out: status + "  - Token scopes: 'gist', 'read:org'\n"},
			Fail, []string{"no repo scope", "gh auth refresh -s repo"}},
		{"no scopes at all",
			ghReply{out: status + "  - Token scopes: none\n"},
			Fail, []string{"no token scopes", "gh auth refresh -s repo"}},
		{"scopes not reported",
			ghReply{out: status},
			Pass, []string{"scopes not reported"}},
		{"not logged in",
			ghReply{err: errors.New("gh auth status: exit status 1: You are not logged into any GitHub hosts")},
			Fail, []string{"not logged into", "gh auth login"}},
		// bees only ever talks to github.com, so an enterprise token carrying
		// `repo` must not cover for a github.com token that lacks it.
		{"another host has the repo scope",
			ghReply{out: multiHostStatus},
			Fail, []string{"no repo scope", "gh auth refresh -s repo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, "", map[string]ghReply{"auth status": c.reply})
			wantResult(t, f.run(t, f.checkGH), c.status, c.detail...)
		})
	}

	t.Run("asks gh about github.com only", func(t *testing.T) {
		f := setup(t, "", map[string]ghReply{"auth status": {out: status + "  - Token scopes: 'repo'\n"}})
		wantResult(t, f.run(t, f.checkGH), Pass, "repo")
		if len(f.gh.calls) != 1 {
			t.Fatalf("gh calls: %v", f.gh.calls)
		}
		if got := strings.Join(f.gh.calls[0], " "); !strings.Contains(got, "--hostname github.com") {
			t.Errorf("gh %s: want --hostname github.com, or an enterprise host's scopes leak into the answer", got)
		}
	})

	t.Run("reports the github.com account", func(t *testing.T) {
		// The enterprise block comes first: the account must be picked by host,
		// not by whichever gh happened to print first.
		const enterpriseFirst = "ghe.corp.example\n" +
			"  ✓ Logged in to ghe.corp.example account corp-kyle (keyring)\n" +
			"  - Token scopes: 'repo'\n" +
			"github.com\n" +
			"  ✓ Logged in to github.com account kyle (keyring)\n" +
			"  - Token scopes: 'repo'\n"
		f := setup(t, "", map[string]ghReply{"auth status": {out: enterpriseFirst}})
		r := f.run(t, f.checkGH)
		if !strings.Contains(r.Detail, "kyle") || strings.Contains(r.Detail, "corp-kyle") {
			t.Errorf("detail %q: want the github.com account, not the enterprise one", r.Detail)
		}
	})

	t.Run("no host header", func(t *testing.T) {
		f := setup(t, "", map[string]ghReply{"auth status": {
			out: "  ✓ Logged in to github.com account kyle (keyring)\n  - Token scopes: 'repo'\n"}})
		wantResult(t, f.run(t, f.checkGH), Pass, "kyle", "repo")
	})

	t.Run("gh not installed", func(t *testing.T) {
		f := setup(t, "", nil)
		f.LookPath = func(string) (string, error) { return "", errors.New("not found") }
		wantResult(t, f.run(t, f.checkGH), Fail, "gh not found on PATH")
	})
}

func TestCheckClaude(t *testing.T) {
	cases := []struct {
		name   string
		output string
		status Status
		detail []string
	}{
		{"new enough", "2.9.0 (Claude Code)", Pass, []string{"claude 2.9.0"}},
		{"exactly the minimum", MinClaudeVersion + " (Claude Code)", Pass, nil},
		{"too old", "2.0.1 (Claude Code)", Warn, []string{"2.0.1", MinClaudeVersion, "claude update"}},
		{"not claude", "GNU bash, version 5", Warn, []string{"no version number"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, "", nil)
			f.ClaudeBin = fakeClaude(t, c.output)
			wantResult(t, f.run(t, f.checkClaude), c.status, c.detail...)
		})
	}

	t.Run("not on PATH", func(t *testing.T) {
		f := setup(t, "", nil)
		f.ClaudeBin = "claude"
		wantResult(t, f.run(t, f.checkClaude), Fail, "not found on PATH", "BEES_CLAUDE_BIN")
	})

	t.Run("not runnable", func(t *testing.T) {
		f := setup(t, "", nil)
		f.ClaudeBin = filepath.Join(t.TempDir(), "gone")
		wantResult(t, f.run(t, f.checkClaude), Fail, "--version failed")
	})
}

// ---- config ----------------------------------------------------------------

func TestCheckConfigLoads(t *testing.T) {
	f := setup(t, "", nil)
	wantResult(t, f.run(t, f.checkConfigLoads), Pass, "bees.toml")

	_, clone := testutil.SetupRepos(t)
	path := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(path, []byte("version = 1\n[project]\nnonsense = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(context.Background(), path, "claude")
	if d.Config != nil {
		t.Fatal("an invalid bees.toml must not load")
	}
	r := d.checkConfigLoads(context.Background())
	wantResult(t, r, Fail, "unknown keys", "bees config validate")
	if r.Status != Fail {
		t.Fatal("an invalid bees.toml is a failure")
	}
	if strings.Contains(r.Detail, "\n") {
		t.Errorf("the detail must be a single line: %q", r.Detail)
	}
	// No bees.toml at all: the remediation is to create one.
	none := &Deps{ConfigErr: errors.New("bees.toml not found in this directory or any parent")}
	wantResult(t, none.checkConfigLoads(context.Background()), Fail, "bees init")

	if got := len(d.Checks()); got != 4 {
		t.Errorf("without a config only the toolchain and config-load checks run, got %d", got)
	}
}

func TestCheckProject(t *testing.T) {
	t.Run("github remote", func(t *testing.T) {
		f := setup(t, "", nil)
		setRemote(t, f.clone, "https://github.com/owner/name.git")
		wantResult(t, f.run(t, f.checkProject), Pass, "owner/name", "main")
	})

	t.Run("remote is not a github url", func(t *testing.T) {
		f := setup(t, "", nil) // testutil's origin is a local path
		wantResult(t, f.run(t, f.checkProject), Fail, "not a GitHub URL")
	})

	t.Run("no such remote", func(t *testing.T) {
		f := setup(t, "remote = \"upstream\"\n", nil)
		wantResult(t, f.run(t, f.checkProject), Fail, "upstream", "git remote add upstream")
	})

	t.Run("repo could not be resolved", func(t *testing.T) {
		f := setup(t, "", nil)
		f.ResolveErr = errors.New(`project.repo is not set and remote "origin" URL "/tmp/x" is not a GitHub repository`)
		wantResult(t, f.run(t, f.checkProject), Fail, "is not a GitHub repository", "project.repo")
	})
}

func TestCheckRemote(t *testing.T) {
	f := setup(t, "", nil)
	wantResult(t, f.run(t, f.checkRemote), Pass, "origin")

	f.Config.Project.Remote = "nope"
	wantResult(t, f.run(t, f.checkRemote), Fail, "nope", "credentials")
}

func TestCheckStateDirIgnored(t *testing.T) {
	f := setup(t, "", nil)
	wantResult(t, f.run(t, f.checkStateDirIgnored), Warn, ".bees is not ignored", "/.bees/")

	if err := os.WriteFile(filepath.Join(f.clone, ".gitignore"), []byte("/.bees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.run(t, f.checkStateDirIgnored), Pass, ".bees is ignored")

	outside := t.TempDir()
	f = setup(t, "state_dir = "+fmt.Sprintf("%q", outside)+"\n", nil)
	wantResult(t, f.run(t, f.checkStateDirIgnored), Pass, "outside the clone")
}

func TestCheckNotesWritable(t *testing.T) {
	f := setup(t, "", nil)
	r := f.run(t, f.checkNotesWritable)
	wantResult(t, r, Pass, "notes")
	if entries, err := os.ReadDir(r.Detail); err != nil || len(entries) != 0 {
		t.Errorf("the probe file should be gone: %v %v", entries, err)
	}

	// A state dir that is a regular file: MkdirAll fails whatever the
	// permissions are, including for root in a CI container.
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f = setup(t, "state_dir = "+fmt.Sprintf("%q", blocked)+"\n", nil)
	wantResult(t, f.run(t, f.checkNotesWritable), Fail, "not a directory")
}

func TestCheckPromptFiles(t *testing.T) {
	f := setup(t, "", nil)
	wantResult(t, f.run(t, f.checkPromptFiles), Pass, "no prompt_file configured")

	_, clone := testutil.SetupRepos(t)
	if err := os.WriteFile(filepath.Join(clone, "extra.md"), []byte("be nice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f = setupIn(t, clone, "\n[roles.developer]\nprompt_file = \"extra.md\"\n", nil)
	wantResult(t, f.run(t, f.checkPromptFiles), Pass, "extra.md")

	// config.Load already refuses a missing prompt_file, so the check only
	// ever sees one that disappeared after the configuration was loaded.
	if err := os.Remove(filepath.Join(f.clone, "extra.md")); err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.run(t, f.checkPromptFiles), Fail, "roles.developer.prompt_file extra.md")
}

// A repository that keeps role instructions in bees/prompts/ has them
// checked here: a file no role will ever read (a misspelled role name) and a
// file bees cannot use both have to fail loudly, because a session only warns
// and carries on. A repository with no such directory - every repository that
// has never heard of the feature - passes.
func TestCheckProjectPrompts(t *testing.T) {
	f := setup(t, "", nil)
	wantResult(t, f.run(t, f.checkProjectPrompts), Pass, "no bees/prompts/ directory")

	write := func(clone, name, body string) {
		t.Helper()
		dir := filepath.Join(clone, "bees", "prompts")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(f.clone, "common.md", "Speak plainly.\n")
	write(f.clone, "developer.md", "Run make lint.\n")
	wantResult(t, f.run(t, f.checkProjectPrompts), Pass, "bees/prompts/common.md", "bees/prompts/developer.md")

	write(f.clone, "develloper.md", "oops\n")
	wantResult(t, f.run(t, f.checkProjectPrompts), Fail, "bees/prompts/develloper.md", "developer.md")
	if err := os.Remove(filepath.Join(f.clone, "bees", "prompts", "develloper.md")); err != nil {
		t.Fatal(err)
	}

	// common.md is read by every role, so the report must name it once.
	write(f.clone, "common.md", strings.Repeat("x", prompts.MaxProjectPromptBytes+1))
	r := f.run(t, f.checkProjectPrompts)
	wantResult(t, r, Fail, "bees/prompts/common.md", "limit")
	if n := strings.Count(r.Detail, "bees/prompts/common.md"); n != 1 {
		t.Errorf("the same broken file is reported %d times: %s", n, r.Detail)
	}
}

// A repository can carry both kinds of problem at once - a misspelled file no
// role reads, and a separately oversized common.md - and one run has to name
// both. Reporting the misspelling alone made discovering the second cost a
// fix and a re-run (#309).
func TestCheckProjectPromptsReportsEveryProblemInOneRun(t *testing.T) {
	f := setup(t, "", nil)
	dir := filepath.Join(f.clone, "bees", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"develloper.md": "oops\n",
		"common.md":     strings.Repeat("x", prompts.MaxProjectPromptBytes+1),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := f.run(t, f.checkProjectPrompts)
	wantResult(t, r, Fail, "bees/prompts/develloper.md", "bees/prompts/common.md")
	// wantResult is satisfied by either field, so say which is which: both
	// problems belong in the detail and both remedies in the remediation.
	for _, s := range []string{"not read by any role: bees/prompts/develloper.md", "bees/prompts/common.md is ", "byte limit"} {
		if !strings.Contains(r.Detail, s) {
			t.Errorf("detail %q does not carry %q", r.Detail, s)
		}
	}
	for _, s := range []string{"move it out of bees/prompts/", "fix the file:"} {
		if !strings.Contains(r.Remediation, s) {
			t.Errorf("remediation %q does not carry %q", r.Remediation, s)
		}
	}
	// The known-file branch still dedupes: common.md is read by every role.
	if n := strings.Count(r.Detail, "bees/prompts/common.md"); n != 1 {
		t.Errorf("the same broken file is reported %d times: %s", n, r.Detail)
	}
}

// ---- github ----------------------------------------------------------------

func TestCheckRepoAccess(t *testing.T) {
	cases := []struct {
		name   string
		reply  ghReply
		status Status
		detail []string
	}{
		{"writable", ghReply{out: `{"nameWithOwner":"owner/name","viewerPermission":"ADMIN"}`}, Pass, []string{"ADMIN"}},
		{"read only", ghReply{out: `{"nameWithOwner":"owner/name","viewerPermission":"READ"}`}, Fail, []string{"permission READ", "write access"}},
		{"no permission reported", ghReply{out: `{"nameWithOwner":"owner/name"}`}, Warn, []string{"no permission level"}},
		{"not readable", ghReply{err: errors.New("gh repo view: exit status 1: Could not resolve to a Repository")}, Fail, []string{"Could not resolve", "project.repo"}},
		{"unreadable json", ghReply{out: "not json"}, Fail, []string{"gh repo view"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, "", map[string]ghReply{"repo view": c.reply})
			wantResult(t, f.run(t, f.checkRepoAccess), c.status, c.detail...)
		})
	}
}

// githubTOML configures [github]. The token is a literal: config.Validate
// rejects a "$VAR" reference that expands to nothing, so a fixture that used
// one would fail to load rather than reach the check.
const githubTOML = `
[github]
login = "beebot"
token = "ghp_fixture"
`

// TestCheckGitHubLogin covers the three answers `gh api user` can give for a
// configured token - the login bees.toml names, a different one, and an error
// - plus the "[bot]" shape somebody configuring a bot account actually
// writes. An error is a failure whatever github.login says (#306): the login
// is compared with the account GitHub reports, so a token that authenticates
// as no account is a token bees cannot run as.
func TestCheckGitHubLogin(t *testing.T) {
	cases := []struct {
		name   string
		toml   string
		reply  ghReply
		status Status
		detail []string
	}{
		{"the token belongs to the configured login", githubTOML,
			ghReply{out: "beebot\n"}, Pass, []string{"beebot"}},
		// GitHub logins are case-insensitive, and so is github.IsBee.
		{"case does not matter", githubTOML,
			ghReply{out: "BeeBot\n"}, Pass, []string{"BeeBot"}},
		{"the token belongs to somebody else", githubTOML,
			ghReply{out: "kyle\n"}, Fail,
			[]string{"belongs to kyle", "github.login says beebot", `set github.login = "kyle"`}},
		// A user token whose login was written with the suffix a GitHub App
		// would carry. An ordinary mismatch, named as such.
		{"a [bot] suffix on a user token", "\n[github]\nlogin = \"beebot[bot]\"\ntoken = \"ghp_fixture\"\n",
			ghReply{out: "beebot\n"}, Fail,
			[]string{"belongs to beebot", "without the [bot] suffix"}},
		{"the token was rejected", githubTOML,
			ghReply{err: errors.New("gh api user: exit status 1: Bad credentials")}, Fail,
			[]string{"was not accepted by GitHub", "Bad credentials"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, c.toml, map[string]ghReply{"api user": c.reply})
			wantResult(t, f.run(t, f.checkGitHubLogin), c.status, c.detail...)
		})
	}
}

// TestGitHubChecksAreSkippedWithoutTheTable pins the other half of "only when
// [github] is configured": with the table unset there is no configured login
// to check and no configured token to probe with, so both checks pass and
// neither spends a gh call. The fake errors on any call it was not given a
// reply for, so an unguarded check fails here rather than merely costing one.
func TestGitHubChecksAreSkippedWithoutTheTable(t *testing.T) {
	f := setup(t, "", nil)
	for _, run := range []func(context.Context) Result{f.checkGitHubLogin, f.checkIssueWrites, f.checkPushes} {
		r := f.run(t, run)
		wantResult(t, r, Pass, "[github] is not configured")
	}
	if len(f.gh.calls) != 0 {
		t.Errorf("asked GitHub %d times with [github] unset: %v", len(f.gh.calls), f.gh.calls)
	}
}

// TestCheckIssueWrites covers the failure #303 was filed for: a token that
// reads the repository as ADMIN and cannot create an issue. checkRepoAccess
// passes on such a token, so the write has to be established rather than
// inferred.
func TestCheckIssueWrites(t *testing.T) {
	cases := []struct {
		name   string
		reply  ghReply
		status Status
		detail []string
	}{
		{"the write is allowed",
			ghReply{out: `{"name":"bees"}`}, Pass, []string{"beebot", "issue comments and labels"}},
		// The reported failure, in GitHub's own words.
		{"readable but not writable",
			ghReply{err: errors.New("gh api: exit status 1: Resource not accessible by personal access token (HTTP 403)")},
			Fail, []string{"cannot write issues", "Issues -> Read and write"}},
		{"forbidden without the sentence",
			ghReply{err: errors.New("gh api: exit status 1: HTTP 403")}, Fail, []string{"cannot write issues"}},
		{"no label to probe with",
			ghReply{err: errors.New("gh api: exit status 1: Not Found (HTTP 404)")},
			Warn, []string{"no `bees` label", "bees labels sync"}},
		{"gh could not reach GitHub",
			ghReply{err: errors.New("gh api: exit status 1: dial tcp: lookup api.github.com: no such host")},
			Warn, []string{"could not check", "no such host"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, githubTOML, map[string]ghReply{"api --method PATCH": c.reply})
			wantResult(t, f.run(t, f.checkIssueWrites), c.status, c.detail...)
		})
	}
}

// TestTheWriteProbeLeavesNothingBehind pins what the probe is, which is the
// half of the check no status can express: renaming the base label to the
// name it already has is a real write - so GitHub's permission gate is what
// answers it - that changes nothing.
//
// The argv is the only seam this can be read from: a fake cannot tell a
// no-op rename from one that renames the label, and the difference is the
// whole reason this probe was chosen over creating an issue.
func TestTheWriteProbeLeavesNothingBehind(t *testing.T) {
	f := setup(t, githubTOML, map[string]ghReply{"api --method PATCH": {out: `{"name":"bees"}`}})
	wantResult(t, f.run(t, f.checkIssueWrites), Pass)
	if len(f.gh.calls) != 1 {
		t.Fatalf("made %d gh calls, want 1: %v", len(f.gh.calls), f.gh.calls)
	}
	got := strings.Join(f.gh.calls[0], " ")
	want := "api --method PATCH repos/owner/name/labels/bees -f new_name=bees"
	if got != want {
		t.Errorf("probe ran `gh %s`, want `gh %s`", got, want)
	}
}

// TestCheckPushes covers the sibling of #303 the issue-write check does not
// reach (#312): a token granted Issues but not Contents passes every other
// GitHub check bees has, and then every developer session's `git push` fails.
// The refusal is GitHub's, so the probe has to be a real write.
func TestCheckPushes(t *testing.T) {
	const sha = "f5093ef8549ae5cb9afeb67e8fcaee9962ff23be"
	cases := []struct {
		name     string
		toml     string
		patch    ghReply
		status   Status
		detail   []string
		absent   []string
		noBranch bool
	}{
		{name: "the push is allowed", toml: githubTOML,
			patch: ghReply{out: `{"ref":"refs/heads/main"}`}, status: Pass,
			detail: []string{"beebot", "main", "owner/name"}},
		// The failure the check exists for: the repository reads as ADMIN
		// and the token was not granted Contents.
		{name: "readable but not pushable", toml: githubTOML,
			patch:  ghReply{err: errors.New("gh api: exit status 1: Resource not accessible by personal access token (HTTP 403)")},
			status: Fail,
			detail: []string{"cannot write branches", "Contents -> Read and write", "Pull requests -> Read and write", "`repo` scope"}},
		// A protected default branch answers 422 whatever the token may do,
		// so it must not be reported as a missing grant.
		{name: "the branch is protected", toml: githubTOML,
			patch:  ghReply{err: errors.New("gh api: exit status 1: HTTP 422: Reference cannot be updated")},
			status: Warn,
			detail: []string{"could not check", "protected branch"},
			absent: []string{"Contents ->", "cannot write branches"}},
		{name: "no such branch", toml: githubTOML,
			patch:  ghReply{err: errors.New("gh api: exit status 1: Not Found (HTTP 404)")},
			status: Warn, detail: []string{"could not check", "no main branch"}},
		{name: "gh could not reach GitHub", toml: githubTOML,
			patch:  ghReply{err: errors.New("gh api: exit status 1: dial tcp: lookup api.github.com: no such host")},
			status: Warn, detail: []string{"could not check", "no such host"}},
		// project.default_branch is reported by the config group; with no
		// ref to probe there is nothing this check can say.
		{name: "no default branch to probe", toml: githubTOML, noBranch: true,
			status: Warn, detail: []string{"could not check", "project.default_branch"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, c.toml, map[string]ghReply{
				"api --method PATCH": c.patch,
				"api repos":          {out: `{"object":{"sha":"` + sha + `"}}`},
			})
			if c.noBranch {
				f.Config.Project.DefaultBranch = ""
			}
			r := f.run(t, f.checkPushes)
			wantResult(t, r, c.status, c.detail...)
			for _, a := range c.absent {
				if strings.Contains(r.Detail+" "+r.Remediation, a) {
					t.Errorf("result mentions %q:\n  detail: %s\n  remedy: %s", a, r.Detail, r.Remediation)
				}
			}
		})
	}
}

// TestThePushProbeChangesNothing pins the half of checkPushes no status can
// express: the update sends the commit the ref already points at, so the
// write GitHub's permission gate decides changes nothing and leaves nothing
// behind. A fake cannot tell a no-op ref update from one that moves the
// branch, and that difference is the whole reason this probe was chosen over
// creating and deleting a branch - so the argv is the only seam it can be
// read from.
func TestThePushProbeChangesNothing(t *testing.T) {
	const sha = "f5093ef8549ae5cb9afeb67e8fcaee9962ff23be"
	f := setup(t, githubTOML, map[string]ghReply{
		"api --method PATCH": {out: `{"ref":"refs/heads/main"}`},
		"api repos":          {out: `{"object":{"sha":"` + sha + `"}}`},
	})
	wantResult(t, f.run(t, f.checkPushes), Pass)
	if len(f.gh.calls) != 2 {
		t.Fatalf("made %d gh calls, want 2 (read the ref, then write it back): %v", len(f.gh.calls), f.gh.calls)
	}
	// git/ref, not git/refs: the plural endpoint prefix-matches, so a
	// default branch whose name is a prefix of another branch's answers
	// with an array of every match and the probe has no sha to write back.
	if got, want := strings.Join(f.gh.calls[0], " "), "api repos/owner/name/git/ref/heads/main"; got != want {
		t.Errorf("read the ref with `gh %s`, want `gh %s`", got, want)
	}
	got := strings.Join(f.gh.calls[1], " ")
	want := "api --method PATCH repos/owner/name/git/refs/heads/main -f sha=" + sha
	if got != want {
		t.Errorf("probe ran `gh %s`, want `gh %s`", got, want)
	}
}

func TestCheckLabels(t *testing.T) {
	f := setup(t, "", nil)
	all := f.Config.Labels().All()

	var names []string
	for _, l := range all {
		names = append(names, fmt.Sprintf("{%q:%q}", "name", l.Name))
	}
	f.gh.replies = map[string]ghReply{"label list": {out: "[" + strings.Join(names, ",") + "]"}}
	wantResult(t, f.run(t, f.checkLabels), Pass, fmt.Sprintf("all %d present", len(all)))

	// Drop the ready label; case differences are the same label to GitHub.
	var kept []string
	for i, l := range all {
		if l.Name == f.Config.Labels().Ready {
			continue
		}
		name := l.Name
		if i == 0 {
			name = strings.ToUpper(name)
		}
		kept = append(kept, fmt.Sprintf("{%q:%q}", "name", name))
	}
	f.gh.replies = map[string]ghReply{"label list": {out: "[" + strings.Join(kept, ",") + "]"}}
	wantResult(t, f.run(t, f.checkLabels), Fail, fmt.Sprintf("1 of %d missing: %s", len(all), f.Config.Labels().Ready), "bees labels sync")

	f.gh.replies = map[string]ghReply{"label list": {err: errors.New("gh label list: exit status 1")}}
	wantResult(t, f.run(t, f.checkLabels), Fail, "bees labels sync")
}

func TestCheckFilter(t *testing.T) {
	f := setup(t, "", map[string]ghReply{"issue list": {out: `[{"number":47},{"number":48}]`}})
	wantResult(t, f.run(t, f.checkFilter), Pass, "2 open issues", "label bees")
	// A satisfied filter asks nothing else: the base-label count only runs
	// when the first listing came back empty.
	if len(f.gh.calls) != 1 {
		t.Errorf("a passing filter check made %d gh calls, want 1: %v", len(f.gh.calls), f.gh.calls)
	}

	f.gh.replies = map[string]ghReply{"issue list": {out: "[]"}}
	r := f.run(t, f.checkFilter)
	wantResult(t, r, Warn, "no open issue matches", "label bees", "filter.label")

	f.gh.replies = map[string]ghReply{"issue list": {err: errors.New("gh issue list: exit status 1")}}
	wantResult(t, f.run(t, f.checkFilter), Fail, "exit status 1")
}

// A filter.assignee that is not a GitHub login makes `gh issue list --assignee X`
// error instead of answering an empty list (it only answers empty when the query
// also carries a label). That is a filter matching nothing, not a broken gh, and
// checkFilter is a Warn and never a Fail - see #130.
func TestCheckFilterUnknownAssignee(t *testing.T) {
	const toml = "\n[filter]\nrequire_label = false\nassignee = \"kylpenfound\"\n"
	graphQL := errors.New("gh issue list: exit status 1: GraphQL: Could not find an assignee " +
		"with the login of 'kylpenfound'. (repository.issues)")

	f := setup(t, toml, map[string]ghReply{"issue list": {err: graphQL}})
	r := f.run(t, f.checkFilter)
	wantResult(t, r, Warn, "filter.assignee", "kylpenfound", "bees.toml")
	if strings.Contains(r.Detail+" "+r.Remediation, "check that gh can list issues") {
		t.Errorf("unknown assignee reported as a broken gh: %+v", r)
	}

	// Any other gh failure is still a broken gh.
	f.gh.replies = map[string]ghReply{"issue list": {err: errors.New("gh issue list: exit status 1")}}
	wantResult(t, f.run(t, f.checkFilter), Fail, "exit status 1", "check that gh can list issues")
}

func TestCheckFilterPrintsTheWholeFilter(t *testing.T) {
	f := setup(t, "\n[filter]\nassignee = \"kyle\"\nmilestone = \"v0.1.0\"\n",
		map[string]ghReply{"issue list": {out: "[]"}, "pr list": {out: "[]"}})
	wantResult(t, f.run(t, f.checkFilter), Warn, "label bees + assignee kyle + milestone v0.1.0")

	// require_label = false is only valid with another criterion.
	f = setup(t, "\n[filter]\nrequire_label = false\nassignee = \"kyle\"\n", map[string]ghReply{"issue list": {out: "[]"}})
	wantResult(t, f.run(t, f.checkFilter), Warn, "assignee kyle")
	if strings.Contains(f.run(t, f.checkFilter).Detail, "label") {
		t.Error("the label is not part of the filter when require_label is false")
	}
	if got := describeQuery(github.Query{}); got != "the empty filter (every open issue)" {
		t.Errorf("describeQuery(empty) = %q", got)
	}
}

// issueList renders n issues (or PRs) as the JSON gh returns.
func issueList(n int) string {
	var items []string
	for i := 1; i <= n; i++ {
		items = append(items, fmt.Sprintf(`{"number":%d}`, i))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// baseLabelGH answers the base-label listings ("gh issue list --label bees"
// with no other criterion) with the given counts, and every filtered listing
// with nothing.
func baseLabelGH(issues, prs int) func([]string) (ghReply, bool) {
	return func(args []string) (ghReply, bool) {
		joined := strings.Join(args, " ")
		if !strings.HasPrefix(joined, "issue list") && !strings.HasPrefix(joined, "pr list") {
			return ghReply{}, false
		}
		if strings.Contains(joined, "--assignee") || strings.Contains(joined, "--milestone") {
			return ghReply{out: "[]"}, true // nothing matches the filter
		}
		if strings.HasPrefix(joined, "pr list") {
			return ghReply{out: issueList(prs)}, true
		}
		return ghReply{out: issueList(issues)}, true
	}
}

// A filter that suddenly matches nothing while the repository is full of
// labelled work is the failure mode of #110: say so, with both counts.
func TestCheckFilterTellsAnEmptyRepoFromAHiddenBacklog(t *testing.T) {
	f := setup(t, "\n[filter]\nassignee = \"kyle\"\n", nil)
	f.gh.reply = baseLabelGH(34, 2)

	r := f.run(t, f.checkFilter)
	wantResult(t, r, Warn, "34 open issues", "2 pull requests", "bees", "label=bees AND assignee=kyle")
	for _, want := range []string{"ANDed", "bees doctor --fix", "bees.toml"} {
		if !strings.Contains(r.Remediation, want) {
			t.Errorf("remediation %q does not name %q", r.Remediation, want)
		}
	}

	// A satisfied filter asks nothing else even when it has more than the
	// label: the base-label count only runs when the first listing was empty.
	// (The label-only assertion in TestCheckFilter cannot see this: there the
	// filter *is* the base-label question, so it short-circuits either way.)
	sat := setup(t, "\n[filter]\nassignee = \"kyle\"\n", map[string]ghReply{"issue list": {out: `[{"number":47}]`}})
	wantResult(t, sat.run(t, sat.checkFilter), Pass, "1 open issue")
	if len(sat.gh.calls) != 1 {
		t.Errorf("a passing filter check made %d gh calls, want 1: %v", len(sat.gh.calls), sat.gh.calls)
	}

	// Nothing carries the base label either: an empty or not-yet-labelled
	// repository, which gets the plain message.
	f.gh.reply = baseLabelGH(0, 0)
	wantResult(t, f.run(t, f.checkFilter), Warn, "no open issue matches", "label bees + assignee kyle", "filter.label")

	// A failing second listing must not turn the check into a failure.
	f.gh.reply = func(args []string) (ghReply, bool) {
		if strings.Contains(strings.Join(args, " "), "--assignee") {
			return ghReply{out: "[]"}, true
		}
		return ghReply{err: errors.New("gh issue list: exit status 1")}, true
	}
	wantResult(t, f.run(t, f.checkFilter), Warn, "no open issue matches")
}

// Without require_label there is no base label the factory's items are
// guaranteed to carry, so there is nothing to count and nothing extra to ask.
func TestCheckFilterWithoutRequireLabelKeepsOneLine(t *testing.T) {
	f := setup(t, "\n[filter]\nrequire_label = false\nassignee = \"kyle\"\n", map[string]ghReply{"issue list": {out: "[]"}})
	r := f.run(t, f.checkFilter)
	wantResult(t, r, Warn, "no open issue matches", "assignee kyle")
	if strings.Contains(r.Detail, "carry") {
		t.Errorf("detail counts base-label items without a base label: %q", r.Detail)
	}
	if len(f.gh.calls) != 1 {
		t.Errorf("made %d gh calls, want 1: %v", len(f.gh.calls), f.gh.calls)
	}
}

// A label-only filter is its own base-label question: asking it twice would
// be one wasted gh call per doctor run.
func TestCheckFilterLabelOnlyListsOnce(t *testing.T) {
	f := setup(t, "", map[string]ghReply{"issue list": {out: "[]"}})
	wantResult(t, f.run(t, f.checkFilter), Warn, "no open issue matches", "label bees")
	if len(f.gh.calls) != 1 {
		t.Errorf("made %d gh calls, want 1: %v", len(f.gh.calls), f.gh.calls)
	}
}

// ---- workspace -------------------------------------------------------------

func TestCheckWorktree(t *testing.T) {
	f := setup(t, "", nil)
	f.Workspaces.Root = t.TempDir()
	r := f.run(t, f.checkWorktree)
	wantResult(t, r, Pass, f.Workspaces.Root)
	entries, err := os.ReadDir(f.Workspaces.Root)
	if err != nil || len(entries) != 0 {
		t.Errorf("the probe worktree should be gone: %v %v", entries, err)
	}

	f.Config.Project.DefaultBranch = "nope"
	wantResult(t, f.run(t, f.checkWorktree), Fail, "git fetch origin")
}

// ---- the set as a whole ----------------------------------------------------

func TestChecksCoverEveryGroup(t *testing.T) {
	f := setup(t, "", nil)
	checks := f.Checks()
	// 19 cheap ones plus one per role: with nothing role-specific configured
	// each role still reports one row.
	if want := 19 + len(config.Roles); len(checks) != want {
		t.Errorf("got %d checks, want %d", len(checks), want)
	}
	f.gh.replies = map[string]ghReply{
		"auth status": {out: "  - Token scopes: 'repo'"},
		"repo view":   {out: `{"viewerPermission":"WRITE"}`},
		"label list":  {out: "[]"},
		"issue list":  {out: "[]"},
	}
	f.Workspaces.Root = t.TempDir()
	seen := map[string]bool{}
	for _, r := range Run(context.Background(), checks) {
		seen[r.Group] = true
		if r.Status != Pass && r.Remediation == "" {
			t.Errorf("%s: a %s result must say what to do about it", r.Name, r.Status)
		}
	}
	for _, g := range Groups {
		if !seen[g] {
			t.Errorf("no check in group %q", g)
		}
	}
}

func TestChecksWithoutAResolvedRepo(t *testing.T) {
	_, clone := testutil.SetupRepos(t)
	path := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(path, []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A local origin: config.Resolve cannot derive a GitHub repository.
	d := New(context.Background(), path, fakeClaude(t, "2.9.0 (Claude Code)"))
	gh := &fakeGH{t: t, replies: map[string]ghReply{"auth status": {out: "- Token scopes: 'repo'"}}}
	gh.installAll(d)
	d.LookPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	if d.ResolveErr == nil {
		t.Fatal("expected resolution to fail for a non-GitHub remote")
	}
	for _, c := range d.Checks() {
		// Nothing here may reach the network; every check must be safe to
		// run with an unresolved configuration.
		r := c.Run(context.Background())
		if r.Group == GroupGitHub {
			t.Errorf("github checks must be skipped without a repository: %+v", r)
		}
	}
	// Every gh call a check makes has to land on the fake. An empty list
	// means a check ran through a client this test did not install one on,
	// and so reached the real gh with the machine's own credentials.
	if len(gh.calls) == 0 {
		t.Error("no gh call reached the fake: a check ran the real gh")
	}
}

func setRemote(t *testing.T, clone, url string) {
	t.Helper()
	if _, err := workspace.Git(context.Background(), clone, "remote", "set-url", "origin", url); err != nil {
		t.Fatal(err)
	}
}

// ---- auto_merge gate -------------------------------------------------------

const autoMergeTOML = "\n[roles.reviewer]\nauto_merge = true\n"

const protectionPath = "api repos/owner/name/branches/main/protection"

func TestCheckAutoMergeIsSilentWhenAutoMergeIsOff(t *testing.T) {
	f := setup(t, "", nil)
	wantResult(t, f.run(t, f.checkAutoMerge), Pass, "auto_merge is off")
	if len(f.gh.calls) != 0 {
		t.Errorf("no gh call is needed when auto_merge is off: %v", f.gh.calls)
	}
}

func TestCheckAutoMergeWithRequiredChecks(t *testing.T) {
	f := setup(t, autoMergeTOML, map[string]ghReply{
		protectionPath: {out: `{"required_status_checks":{"strict":true,"contexts":["go"],"checks":[{"context":"go"},{"context":"dagger"}]}}`},
	})
	wantResult(t, f.run(t, f.checkAutoMerge), Pass, "go, dagger", "main")
}

func TestCheckAutoMergeWarnsWhenTheBranchIsNotProtected(t *testing.T) {
	f := setup(t, autoMergeTOML, map[string]ghReply{
		protectionPath: {err: errors.New("gh api ...: exit status 1: gh: Branch not protected (HTTP 404)")},
	})
	r := f.run(t, f.checkAutoMerge)
	wantResult(t, r, Warn, "no check is required on `main`", "whatever checks a pull request reports",
		"require your CI checks in the branch protection rules")
	if strings.Contains(r.Detail, "404") {
		t.Errorf("a 404 is the answer, not an error to report: %q", r.Detail)
	}
}

func TestCheckAutoMergeWarnsWhenProtectionRequiresNothing(t *testing.T) {
	f := setup(t, autoMergeTOML, map[string]ghReply{
		protectionPath: {out: `{"required_pull_request_reviews":{"required_approving_review_count":1}}`},
	})
	wantResult(t, f.run(t, f.checkAutoMerge), Warn, "protected but requires no check", "no check is required on `main`")

	f = setup(t, autoMergeTOML, map[string]ghReply{
		protectionPath: {out: `{"required_status_checks":{"strict":true,"contexts":[],"checks":[]}}`},
	})
	wantResult(t, f.run(t, f.checkAutoMerge), Warn, "no check is required on `main`")
}

func TestCheckAutoMergeWarnsWithoutPermissionToRead(t *testing.T) {
	f := setup(t, autoMergeTOML, map[string]ghReply{
		protectionPath: {err: errors.New("gh api ...: exit status 1: gh: Must have admin rights to Repository. (HTTP 403)")},
	})
	// A token without admin rights cannot read protection: that is a warning
	// with the error one-lined, never a failure (the `bees run` preflight
	// refuses to start on a failure).
	wantResult(t, f.run(t, f.checkAutoMerge), Warn, "could not be read", "Must have admin rights", "admin rights on the repository")
}

// TestDoctorResolvesAtMeAsThePerson pins that filter.assignee = "@me" means
// the person running bees on doctor's paths too, with [github] set. The
// client doctor's repository checks run through carries github.token, and gh
// answers both `api user` and `--assignee @me` as the account that token
// belongs to - so resolving "@me" through it would make `bees doctor --fix`
// assign the factory's work to the bot, which the orchestrator (which
// resolves "@me" to the person) then cannot see, and would make the filter
// check report on somebody else's issues.
func TestDoctorResolvesAtMeAsThePerson(t *testing.T) {
	t.Setenv("BEES_TEST_TOKEN", "ghp_bot")
	f := setup(t, "\n[filter]\nassignee = \"@me\"\n\n[github]\nlogin = \"busybees-bot\"\ntoken = \"$BEES_TEST_TOKEN\"\n",
		map[string]ghReply{
			// What gh answers when it runs with the bot's GH_TOKEN set.
			"api user":   {out: "busybees-bot\n"},
			"issue list": {out: "[]"},
			"pr list":    {out: "[]"},
		})
	f.CurrentUser = func(context.Context) (string, error) { return "kyle", nil }

	login, err := f.assignee(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if login != "kyle" {
		t.Errorf("bees doctor --fix would assign the factory's work to %q, not to the person", login)
	}

	f.run(t, f.checkFilter)
	for _, c := range f.gh.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "@me") {
			t.Errorf("`@me` was sent to gh, which resolves it as github.token's account: gh %s", joined)
		}
		if strings.Contains(joined, "--assignee") && !strings.Contains(joined, "--assignee kyle") {
			t.Errorf("the filter check asks GitHub for somebody else's issues: gh %s", joined)
		}
	}
}

// TestGHAuthStatusIsAskedOfTheMachineAccount pins that `gh auth status` is
// the one check that does not carry github.token. It reports the machine's
// own authentication, which is what every Claude session uses whatever
// [github] configures; run with the bot's GH_TOKEN, gh reports the token's
// account first and unions both accounts' `Token scopes:` lines, so a
// github.token missing `repo` would pass on the strength of the person's
// scopes - the merge hostBlock exists to prevent.
func TestGHAuthStatusIsAskedOfTheMachineAccount(t *testing.T) {
	t.Setenv("BEES_TEST_TOKEN", "ghp_bot")
	f := setup(t, "\n[github]\nlogin = \"busybees-bot\"\ntoken = \"$BEES_TEST_TOKEN\"\n", nil)

	if got := f.GitHub.Token; got != "ghp_bot" {
		t.Errorf("the repository checks act as %q, want the configured token", got)
	}
	if got := f.MachineGitHub.Token; got != "" {
		t.Errorf("`gh auth status` carries a token (%q), so it reports on the bot instead of the machine", got)
	}

	var used *github.Client
	for _, c := range []*github.Client{f.GitHub, f.MachineGitHub} {
		c.Exec = func(client *github.Client) func(context.Context, ...string) ([]byte, error) {
			return func(context.Context, ...string) ([]byte, error) {
				used = client
				return []byte("github.com\n  ✓ Logged in to github.com account kyle (keyring)\n  - Token scopes: 'repo'\n"), nil
			}
		}(c)
	}
	wantResult(t, f.run(t, f.checkGH), Pass, "kyle")
	if used != f.MachineGitHub {
		t.Error("`gh auth status` ran through the client that carries github.token")
	}
}
