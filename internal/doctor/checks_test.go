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
	gh.install(d.GitHub)
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
	// 13 cheap ones plus one per role: with nothing role-specific configured
	// each role still reports one row.
	if want := 13 + len(config.Roles); len(checks) != want {
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
	d.GitHub = github.New("")
	gh.install(d.GitHub)
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
}

func setRemote(t *testing.T, clone, url string) {
	t.Helper()
	if _, err := workspace.Git(context.Background(), clone, "remote", "set-url", "origin", url); err != nil {
		t.Fatal(err)
	}
}
