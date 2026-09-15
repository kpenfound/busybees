package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// reviewSession is one brief or angle session the fake CLI recorded
// (fakeReviewSession): the kind ("brief", or the angle's title), the
// command line, the directory it ran in and the files there, the BEES_ROLE
// it saw, and its prompt.
type reviewSession struct {
	Kind   string   `json:"kind"`
	Args   []string `json:"args"`
	Dir    string   `json:"dir"`
	Files  []string `json:"files"`
	Role   string   `json:"role"`
	Prompt string   `json:"prompt"`
}

// reviewLogPath points FAKE_REVIEW_LOG at a file for the test, and returns
// its path.
func reviewLogPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "review-sessions.jsonl")
	t.Setenv("FAKE_REVIEW_LOG", p)
	return p
}

// reviewSessions reads the sessions the fake recorded, in the order they
// ended.
func reviewSessions(t *testing.T, path string) []reviewSession {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no review session ran: %v", err)
	}
	var out []reviewSession
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var s reviewSession
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// modelOf is the --model a recorded session ran with, "" for none.
func modelOf(args []string) string {
	if i := slices.Index(args, "--model"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// byKind indexes the recorded sessions by kind.
func byKind(sessions []reviewSession) map[string]reviewSession {
	out := map[string]reviewSession{}
	for _, s := range sessions {
		out[s.Kind] = s
	}
	return out
}

// anglesReviewerTOML configures the reviewer the way #647 lets a person:
// the angles of one size replaced, and a model per step.
const anglesReviewerTOML = devOnlyTOML + `
[roles.reviewer]
model = "opus"
brief_model = "sonnet"
judge_model = "gpt-judge"
angles.m = ["general", "docs"]
angle_models.docs = "haiku"
`

// A developer's pull request dispatched for review goes through the review
// pipeline before the reviewer session: the brief session sizes the change,
// the angles roles.reviewer.angles gives that size run from the brief, and
// the judge session is told what they found and posts every finding as one
// review. Each step runs the model roles.reviewer names for it, and Model
// where it names none.
func TestAReviewRunsBriefAnglesAndJudgeFromTheReviewerRole(t *testing.T) {
	logPath := reviewLogPath(t)
	t.Setenv("FAKE_REVIEW_SIZE", "m")
	h := newHarness(t, anglesReviewerTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	seedCounter(t, h, "review", 1) // the judge session approves its first review
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	sessions := reviewSessions(t, logPath)
	kinds := make([]string, 0, len(sessions))
	for _, s := range sessions {
		kinds = append(kinds, s.Kind)
	}
	slices.Sort(kinds)
	// The brief sized the change m, and m runs general and docs as
	// configured, not the built-in four.
	if want := []string{"brief", "documentation accuracy", "general"}; !slices.Equal(kinds, want) {
		t.Fatalf("review sessions: %v, want %v", kinds, want)
	}
	got := byKind(sessions)
	// The brief runs brief_model, the docs angle its angle_models entry,
	// and the general angle, with no entry of its own, the role's model.
	for kind, want := range map[string]string{"brief": "sonnet", "documentation accuracy": "haiku", "general": "opus"} {
		if m := modelOf(got[kind].Args); m != want {
			t.Errorf("the %s session ran --model %q, want %q:\n%s", kind, m, want, strings.Join(got[kind].Args, " "))
		}
	}
	// The judge session is a factory session running judge_model.
	judge := argsOfNamed(t, h, "reviewer-pr-201-r1")
	if m := modelOf(judge); m != "gpt-judge" {
		t.Errorf("the judge session ran --model %q, want gpt-judge:\n%s", m, strings.Join(judge, "\n"))
	}
	// The brief session runs in the worker's checkout; the angles run in a
	// clone of it under the artifact, with the branch's files and the diff
	// written beside them, and none of them is a factory session: no role,
	// no MCP server, the read-only tool lists, no permission bypass.
	if !strings.Contains(got["brief"].Dir, "/ws/") {
		t.Errorf("the brief ran in %q, want the worker's checkout", got["brief"].Dir)
	}
	// The diff the brief was given is the clone's own, the branch against
	// its merge base with main, and gh was never asked for one.
	if p := got["brief"].Prompt; !strings.Contains(p, "+++ b/work-1.txt") || strings.Contains(p, "func Widget() {}") {
		t.Errorf("the brief was not given the clone's diff:\n%s", p)
	}
	if n := h.gh.callCount("pr diff"); n != 0 {
		t.Errorf("gh pr diff ran %d times, want the diff read from the clone", n)
	}
	for _, kind := range []string{"documentation accuracy", "general"} {
		s := got[kind]
		if !strings.HasSuffix(s.Dir, "/"+review.CheckoutDir) || !strings.Contains(s.Dir, filepath.Join("reviews", "acme", "widgets", "201")) {
			t.Errorf("the %s angle ran in %q, want the clone under the artifact", kind, s.Dir)
		}
		if !slices.Contains(s.Files, "work-1.txt") || !slices.Contains(s.Files, review.DiffFile) {
			t.Errorf("the %s angle's clone holds %v, want the branch's work-1.txt and %s", kind, s.Files, review.DiffFile)
		}
		// The path as the runner spelled it, which on macOS need not be the
		// resolved one the session's own pwd reports.
		if !strings.Contains(s.Prompt, "The diff is at ") || !strings.Contains(s.Prompt, filepath.Join(review.CheckoutDir, review.DiffFile)+": read it there") {
			t.Errorf("the %s angle was not told where the diff is:\n%s", kind, s.Prompt)
		}
	}
	for _, s := range sessions {
		if s.Role != "" {
			t.Errorf("the %s session ran with BEES_ROLE=%q; a review session is no factory session", s.Kind, s.Role)
		}
		line := strings.Join(s.Args, " ")
		for _, want := range []string{`--mcp-config {"mcpServers":{}} --strict-mcp-config`, "--allowedTools " + strings.Join(review.ReadOnlyTools, ","), "--disallowedTools " + strings.Join(review.DeniedTools, ",")} {
			if !strings.Contains(line, want) {
				t.Errorf("the %s session lacks %q:\n%s", s.Kind, want, line)
			}
		}
		if strings.Contains(line, "--dangerously-skip-permissions") || strings.Contains(line, "mcp.json") {
			t.Errorf("the %s session ran with the factory's permissions or MCP server:\n%s", s.Kind, line)
		}
	}
	if line := strings.Join(judge, " "); !strings.Contains(line, "mcp.json") || !strings.Contains(line, "--dangerously-skip-permissions") {
		t.Errorf("the judge session has no MCP server:\n%s", line)
	}
	// The judge session's task carries the brief and every finding, and it
	// posted every one of them as one comment review on the pull request.
	prompt := promptOf(t, h, 1)
	for _, want := range []string{"## Findings", "the change was sized `m` and reviewed from the general, docs angles", "The brief's summary: Adds the widget.",
		"### general: Widget does nothing", "### documentation accuracy: Widget does nothing", "_Found from the general angle._", "_Found from the docs angle._"} {
		if !strings.Contains(flowedPrompt(prompt), want) {
			t.Errorf("the judge's task lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Not reviewed") {
		t.Errorf("the judge's task says an angle reviewed nothing:\n%s", prompt)
	}
	dirs := h.sessions(config.RoleReviewer)
	if len(dirs) != 1 {
		t.Fatalf("reviewer sessions: %d, want the judge session alone", len(dirs))
	}
	rev := reviewOf(t, dirs[0])
	if got, want := strings.Join(rev.args, " "), "pr review 201 -R acme/widgets --comment --body-file -"; got != want {
		t.Errorf("gh call: %q, want %q", got, want)
	}
	for _, want := range []string{"### general: Widget does nothing", "### documentation accuracy: Widget does nothing"} {
		if !strings.Contains(rev.body, want) {
			t.Errorf("the review posted lacks %q:\n%s", want, rev.body)
		}
	}
	// The artifact is kept under the state directory, without the clone.
	dir, err := review.LatestArtifactDir(h.store.ReviewsDir(), review.Ref{Repo: "acme/widgets", Number: 201})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{review.BriefFile, filepath.Join(review.AnglesDir, "general.json"), filepath.Join(review.AnglesDir, "docs.json"), review.FindingsFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("the artifact lacks %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, review.CheckoutDir)); !os.IsNotExist(err) {
		t.Errorf("the clone was kept under the artifact")
	}
	a, err := review.ReadArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Brief.Size != "m" || a.Brief.CostUSD != 0.25 || len(a.Findings.Items) != 2 {
		t.Errorf("artifact: brief %+v, %d findings", a.Brief, len(a.Findings.Items))
	}
	if !strings.Contains(h.logs.String(), "review: reviewing a size m change from 2 angles: general, docs") {
		t.Errorf("the runner's progress is not in the log:\n%s", h.logs.String())
	}
	if strings.Contains(h.logs.String(), "posted no review") {
		t.Errorf("the review the judge posted was not found:\n%s", h.logs.String())
	}
}

// The base branch cloneOf finds the merge base against is the one under the
// remote the factory is configured with, project.remote, not origin: a
// worker's checkout on a project whose team repository is `upstream` has
// no refs/remotes/origin/main at all, and a lookup there would leave the
// clone without a base and send the diff back through gh.
func TestTheFactorysDiffIsReadFromTheCloneUnderTheConfiguredRemote(t *testing.T) {
	logPath := reviewLogPath(t)
	t.Setenv("FAKE_REVIEW_SIZE", "m")
	// default_branch is spelled out because the harness clone has no
	// `upstream` remote for Resolve to detect it from when the config loads.
	toml := strings.Replace(anglesReviewerTOML, "[project]\n", "[project]\nremote = \"upstream\"\ndefault_branch = \"main\"\n", 1)
	h := newHarnessAt(t, toml, time.Time{}, withRemote("upstream"))
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	seedCounter(t, h, "review", 1)
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	if p := byKind(reviewSessions(t, logPath))["brief"].Prompt; !strings.Contains(p, "+++ b/work-1.txt") || strings.Contains(p, "func Widget() {}") {
		t.Errorf("the brief was not given the clone's diff:\n%s", p)
	}
	if n := h.gh.callCount("pr diff"); n != 0 {
		t.Errorf("gh pr diff ran %d times, want the diff read from the clone", n)
	}
}

// withRemote renames the harness clone's one remote, so that the clone has
// no `origin` at all, and points the workspace manager at the new name the
// way cmd/bees does from project.remote. remote.pushDefault sends the fake
// developer's bare `git push` there too.
func withRemote(name string) func(*Deps) {
	return func(d *Deps) {
		ctx := context.Background()
		for _, args := range [][]string{{"remote", "rename", "origin", name}, {"config", "remote.pushDefault", name}} {
			if _, err := workspace.Git(ctx, d.Workspaces.MainRepo, args...); err != nil {
				panic(err)
			}
		}
		d.Workspaces.Remote = name
	}
}

// Without a model per step, every session runs the role's model, and the
// built-in angles of the brief's size run: an s change gets the quick
// general pass and the documentation read.
func TestAReviewFallsBackToTheRolesModelAndAngles(t *testing.T) {
	logPath := reviewLogPath(t)
	h := newHarness(t, devOnlyTOML+"[roles.reviewer]\nmodel = \"opus\"\n")
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	seedCounter(t, h, "review", 1)
	runPass(t, h)

	got := byKind(reviewSessions(t, logPath))
	if len(got) != 3 || got["brief"].Kind == "" || got["quick general"].Kind == "" || got["documentation accuracy"].Kind == "" {
		t.Fatalf("review sessions: %v, want the brief, quick general and documentation accuracy", got)
	}
	for kind, s := range got {
		if m := modelOf(s.Args); m != "opus" {
			t.Errorf("the %s session ran --model %q, want the role's opus", kind, m)
		}
	}
	if m := modelOf(argsOfNamed(t, h, "reviewer-pr-201-r1")); m != "opus" {
		t.Errorf("the judge session ran --model %q, want the role's opus", m)
	}
}

// A codex reviewer's brief and angle sessions run `codex exec` in its
// read-only sandbox, as `bees review` runs them, and the review completes
// from codex's event stream.
func TestACodexReviewerRunsTheReviewSessionsReadOnly(t *testing.T) {
	logPath := reviewLogPath(t)
	h := newHarness(t, devOnlyTOML+"[roles.reviewer]\nagent = \"codex\"\nmodel = \"o3\"\n")
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	seedCounter(t, h, "review", 1)
	runPass(t, h)

	sessions := reviewSessions(t, logPath)
	if len(sessions) != 3 {
		t.Fatalf("review sessions: %d, want 3", len(sessions))
	}
	for _, s := range sessions {
		line := strings.Join(s.Args, " ")
		if !strings.HasPrefix(line, "exec --json --sandbox read-only") || !strings.Contains(line, "--model o3") {
			t.Errorf("the %s session ran %q, want codex exec read-only with the role's model", s.Kind, line)
		}
	}
	if !strings.Contains(promptOf(t, h, 1), "### quick general: Widget does nothing") {
		t.Errorf("the judge's task lacks the findings codex answered with:\n%s", promptOf(t, h, 1))
	}
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Errorf("history: %v", h.gh.history[1])
	}
}

// One angle failing is not a failed review: the rest are judged, the judge
// session is told which angle reviewed nothing, and the loop goes on. Every
// angle failing is: the review could not run, and the issue goes to a person
// with the reason rather than to a session with nothing to post.
func TestAFailedAngleIsSkippedAndAFailedReviewEscalates(t *testing.T) {
	t.Setenv("FAKE_ANGLE_FAIL", "documentation accuracy")
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	seedCounter(t, h, "review", 1)
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	prompt := promptOf(t, h, 1)
	for _, want := range []string{"Not reviewed:", "- docs: the session failed: docs session: exit status 1: fake review session: the model is overloaded", "### quick general: Widget does nothing"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the judge's task lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "### documentation accuracy") {
		t.Errorf("the failed angle contributed a finding:\n%s", prompt)
	}
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Errorf("history: %v", h.gh.history[1])
	}

	t.Setenv("FAKE_ANGLE_FAIL", "all")
	h = newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1")
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:needs-human" {
		t.Errorf("history: %v", h.gh.history[1])
	}
	comments := h.gh.comments[1]
	if len(comments) != 1 || !strings.Contains(comments[0], "The review of pull request #201 could not run: every angle failed, so nobody reviewed acme/widgets#201") || !strings.Contains(comments[0], "`roles.reviewer`") {
		t.Errorf("escalation comment: %v", comments)
	}
	// What the sessions cost before the review failed is in the ledger.
	ledger, err := h.store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var failed int
	for _, e := range ledger {
		if e.Session == "reviewer-pr-201-r1" && e.Outcome == OutcomeFailed && e.CostUSD == 0.25 && ghwork.Issue(e.Work) == 1 {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("ledger holds %d failed review entries with the brief's cost, want 1:\n%+v", failed, ledger)
	}
}

// A review that found nothing is still posted, so the person merging knows
// it ran, and approved.
func TestAReviewWithNoFindingsIsPostedAndApproved(t *testing.T) {
	t.Setenv("FAKE_REVIEW_EMPTY", "1")
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	seedCounter(t, h, "review", 1)
	runPass(t, h)

	prompt := promptOf(t, h, 1)
	if !strings.Contains(prompt, "The judge's list is empty: no angle found anything to report.") || strings.Contains(prompt, "###") {
		t.Errorf("the judge's task for an empty review:\n%s", prompt)
	}
	dirs := h.sessions(config.RoleReviewer)
	if len(dirs) != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", len(dirs))
	}
	if rev := reviewOf(t, dirs[0]); !strings.Contains(rev.body, "The judge's list is empty") {
		t.Errorf("the review posted:\n%s", rev.body)
	}
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Errorf("history: %v", h.gh.history[1])
	}
}

// The judge session's review on a developer's pull request is looked for
// when the session ends, and its absence is a degraded operation rather
// than a failure: the verdict travels by outcome and mail, the review is
// for the person who merges.
func TestAJudgeThatPostedNoReviewIsADegradedOperation(t *testing.T) {
	t.Setenv("FAKE_REVIEW_NO_SUBMIT", "1")
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	seedCounter(t, h, "review", 1)
	runPass(t, h)

	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Errorf("history: %v", h.gh.history[1])
	}
	if !strings.Contains(h.logs.String(), "the reviewer posted no review on the pull request") {
		t.Errorf("no warning about the missing review:\n%s", h.logs.String())
	}
	st, _ := h.store.LoadStatus()
	if !slices.ContainsFunc(st.Degraded, func(f state.OpFailure) bool { return f.Op == "review-post" }) {
		t.Errorf("review-post is not a degraded operation: %+v", st.Degraded)
	}
}

// The worker's own worktree is left as the developer had it: the review
// writes its diff into its clone, never into the checkout the next
// developer session commits from.
func TestAReviewLeavesNoFileInTheWorkersWorktree(t *testing.T) {
	logPath := reviewLogPath(t)
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	runPass(t, h)

	// Two rounds: the second developer session committed from the worktree
	// the first review ran on. Its branch carries the work files and
	// nothing of the review.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1", "developer-issue-1-r2", "reviewer-pr-201-r2")
	brief := byKind(reviewSessions(t, logPath))["brief"]
	if slices.Contains(brief.Files, review.DiffFile) {
		t.Errorf("the worker's worktree holds %s: %v", review.DiffFile, brief.Files)
	}
	files, err := workspace.Git(context.Background(), h.clone, "ls-tree", "-r", "--name-only", "origin/bees/issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, review.DiffFile) || !strings.Contains(files, "work-2.txt") {
		t.Errorf("the branch's files:\n%s", files)
	}
}

// A second review round verifies instead of reviewing again: round 1 runs
// the brief and the angles and posts their findings; round 2, after the
// developer pushed, runs no brief and no angle, and its judge session is told
// round 1's findings and the commit round 1 read, and posts what it made of
// those findings.
func TestALaterReviewRoundVerifiesTheFirstRoundsFindings(t *testing.T) {
	logPath := reviewLogPath(t)
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	// The fake reviewer requests changes on round 1 and approves round 2.
	sub := h.sched.Subscribe()
	runPass(t, h)
	events := drain(sub)
	assertReviewLifecycle(t, events, "reviewer-pr-201-r1", 1, 201, 1, 2, true)
	for _, ev := range events {
		if ev.Kind == EventSessionStarted && ev.Session == "reviewer-pr-201-r2" && ev.Activity != "" {
			t.Errorf("verification round claims a pipeline: %+v", ev)
		}
	}

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1", "developer-issue-1-r2", "reviewer-pr-201-r2")
	kinds := map[string]int{}
	for _, s := range reviewSessions(t, logPath) {
		kinds[s.Kind]++
	}
	if want := map[string]int{"brief": 1, "quick general": 1, "documentation accuracy": 1}; !maps.Equal(kinds, want) {
		t.Fatalf("review sessions: %v, want round 1's brief and two angles once each", kinds)
	}

	round1, round2 := promptOf(t, h, 1), flowedPrompt(promptOf(t, h, 3))
	if strings.Contains(round1, "No review ran for this round") {
		t.Errorf("round 1 was told to verify:\n%s", round1)
	}
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	// Round 1 read the branch's first commit; round 2's head is the one the
	// developer pushed on top of it.
	first, err := workspace.Git(context.Background(), h.clone, "rev-parse", "origin/bees/issue-1^")
	if err != nil {
		t.Fatal(err)
	}
	first = strings.TrimSpace(first)
	if bk.ReviewedHead != first {
		t.Errorf("the bookkeeping records reviewed head %q, want round 1's %q", bk.ReviewedHead, first)
	}
	for _, want := range []string{"No review ran for this round.", "when its head was `" + first + "`", "`git diff " + first + "..HEAD`",
		"### quick general: Widget does nothing", "### documentation accuracy: Widget does nothing", "This is round 2: verify, do not review again."} {
		if !strings.Contains(round2, want) {
			t.Errorf("round 2's task lacks %q:\n%s", want, round2)
		}
	}
	if !strings.Contains(round2, "The review is kept under `"+bk.ReviewArtifact+"`") {
		t.Errorf("round 2 was not pointed at round 1's artifact %s:\n%s", bk.ReviewArtifact, round2)
	}
	dirs := h.sessions(config.RoleReviewer)
	if len(dirs) != 2 {
		t.Fatalf("reviewer sessions: %d, want 2", len(dirs))
	}
	if rev := reviewOf(t, dirs[1]); !strings.Contains(rev.body, "### quick general: Widget does nothing") {
		t.Errorf("round 2 posted:\n%s", rev.body)
	}
	// One review artifact for the pull request, and one review in the ledger.
	entries, err := os.ReadDir(filepath.Dir(bk.ReviewArtifact))
	if err != nil || len(entries) != 1 {
		t.Errorf("artifacts for the pull request: %v (%v), want round 1's alone", entries, err)
	}
	ledger, err := h.store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var reviewed []string
	for _, e := range ledger {
		if e.Outcome == "reviewed" {
			reviewed = append(reviewed, e.Session)
		}
	}
	if !slices.Equal(reviewed, []string{"reviewer-pr-201-r1"}) {
		t.Errorf("reviews in the ledger: %v, want round 1's alone", reviewed)
	}
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Errorf("history: %v", h.gh.history[1])
	}
}

// A later round verifies only a review it can read of the pull request it is
// on: no review recorded, one recorded for another pull request, and one whose
// artifact is gone each send the round to a full review.
func TestVerifyReviewNeedsTheReviewOfThisPullRequest(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	log := slog.New(slog.DiscardHandler)
	ref := review.Ref{Repo: "acme/widgets", Number: 201}
	dir := review.ArtifactDir(h.store.ReviewsDir(), ref, time.Now())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := review.WriteBrief(dir, &review.Brief{Size: "s", Summary: "Adds the widget."}); err != nil {
		t.Fatal(err)
	}
	if err := review.WriteFindings(dir, &review.Findings{Items: []review.Finding{{ID: "f1", Angle: "general", Category: "correctness", Severity: "high", File: "widget.go", Title: "Widget does nothing", Body: "empty"}}}); err != nil {
		t.Fatal(err)
	}

	got, ok := h.sched.verifyReview(log, state.WorkState{ReviewArtifact: dir, ReviewedHead: "abc"}, 201)
	if !ok || !got.Verify || got.ReviewedHead != "abc" || got.Count != 1 || !strings.Contains(got.Findings, "Widget does nothing") || got.Artifact != dir {
		t.Fatalf("verifyReview of this pull request's review = %+v, %v", got, ok)
	}
	for name, c := range map[string]struct {
		artifact string
		pr       int
	}{
		"nothing recorded":     {"", 201},
		"another pull request": {dir, 202},
		"an artifact gone":     {filepath.Join(filepath.Dir(dir), "gone"), 201},
	} {
		if got, ok := h.sched.verifyReview(log, state.WorkState{ReviewArtifact: c.artifact}, c.pr); ok || got != nil {
			t.Errorf("%s: verifyReview = %+v, %v, want a full review", name, got, ok)
		}
	}
}

func TestReviewStatesForEachVerdict(t *testing.T) {
	for status, want := range map[string][]string{
		OutcomeChangesRequested: {"CHANGES_REQUESTED"},
		OutcomeApproved:         {"APPROVED", "COMMENTED"},
		"":                      {"APPROVED", "COMMENTED", "CHANGES_REQUESTED"},
	} {
		if got := reviewStatesFor(status); !slices.Equal(got, want) {
			t.Errorf("reviewStatesFor(%q) = %v, want %v", status, got, want)
		}
	}
}
