package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/github"
)

// ---- a repository the fake gh can actually mutate ---------------------------

// repoGH is a fake gh that holds a repository: it answers `issue list` and
// `pr list` by filtering its own items against the flags of the call, and
// applies the assignee and milestone writes to them. That is what makes "the
// filter check passes on the re-run" a real assertion rather than a second
// canned reply.
type repoGH struct {
	t          *testing.T
	issues     []*github.Issue
	prs        []*github.PR
	milestones []github.Milestone
	// login is what `gh api user` answers, for filter.assignee = "@me".
	login string
	// failWrites, when set, is the error every mutating call returns.
	failWrites error
	// failAssign holds per-number assign failures.
	failAssign map[int]error
	calls      [][]string
}

// mutating matches the gh invocations that change something. Reads through
// `gh api` are GETs and carry no --method/-X.
var mutating = regexp.MustCompile(`--method|(^| )-X |issue edit|pr edit|label create`)

func (g *repoGH) install(c *github.Client) {
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		g.calls = append(g.calls, args)
		out, err := g.answer(args)
		if err == nil && out == nil {
			g.t.Errorf("unexpected gh call: gh %s", strings.Join(args, " "))
			return nil, fmt.Errorf("unexpected gh call")
		}
		return out, err
	}
}

func (g *repoGH) answer(args []string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if g.failWrites != nil && mutating.MatchString(" "+joined) {
		return nil, g.failWrites
	}
	switch {
	case strings.HasPrefix(joined, "issue list"):
		var out []github.Issue
		for _, i := range g.issues {
			if g.matches(args, i.Labels, i.Assignees, i.MilestoneTitle()) {
				out = append(out, *i)
			}
		}
		return mustJSON(g.t, out), nil
	case strings.HasPrefix(joined, "pr list"):
		var out []github.PR
		for _, p := range g.prs {
			if g.matches(args, p.Labels, p.Assignees, p.MilestoneTitle()) {
				out = append(out, *p)
			}
		}
		return mustJSON(g.t, out), nil
	case joined == "api user --jq .login":
		return []byte(g.login + "\n"), nil
	case strings.Contains(joined, "/milestones?"):
		return mustJSON(g.t, g.milestones), nil
	case len(args) >= 3 && strings.HasSuffix(args[len(args)-3], "/assignees"):
		return g.assign(args)
	case strings.Contains(joined, "--method PATCH"):
		return g.milestone(args)
	}
	return nil, nil
}

func (g *repoGH) assign(args []string) ([]byte, error) {
	n := numberIn(g.t, args[len(args)-3])
	if err := g.failAssign[n]; err != nil {
		return nil, err
	}
	login := strings.TrimPrefix(args[len(args)-1], "assignees[]=")
	if i := g.issue(n); i != nil {
		i.Assignees = append(i.Assignees, github.Author{Login: login})
	} else if p := g.pr(n); p != nil {
		p.Assignees = append(p.Assignees, github.Author{Login: login})
	} else {
		g.t.Errorf("assigned #%d, which is not in the repository", n)
	}
	return []byte("{}"), nil
}

func (g *repoGH) milestone(args []string) ([]byte, error) {
	n := numberIn(g.t, args[len(args)-3])
	num, err := strconv.Atoi(strings.TrimPrefix(args[len(args)-1], "milestone="))
	if err != nil {
		return nil, err
	}
	title := ""
	for _, m := range g.milestones {
		if m.Number == num {
			title = m.Title
		}
	}
	ref := &github.MilestoneRef{Title: title}
	if i := g.issue(n); i != nil {
		i.Milestone = ref
	} else if p := g.pr(n); p != nil {
		p.Milestone = ref
	}
	return []byte("{}"), nil
}

func (g *repoGH) issue(n int) *github.Issue {
	for _, i := range g.issues {
		if i.Number == n {
			return i
		}
	}
	return nil
}

func (g *repoGH) pr(n int) *github.PR {
	for _, p := range g.prs {
		if p.Number == n {
			return p
		}
	}
	return nil
}

// numberIn pulls the item number out of a REST path ("repos/o/n/issues/92").
func numberIn(t *testing.T, path string) int {
	t.Helper()
	parts := strings.Split(strings.TrimSuffix(path, "/assignees"), "/")
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("no item number in %q: %v", path, err)
	}
	return n
}

// matches applies the --label/--assignee/--milestone flags of a gh listing to
// one item, the way GitHub ANDs them. "@me" is resolved to the logged-in user
// here, as gh does server-side - github.Query.Matches cannot, which is exactly
// why the fix has to resolve it before it can compare anything.
func (g *repoGH) matches(args []string, labels []github.Label, assignees []github.Author, milestone string) bool {
	var q github.Query
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch a {
		case "--label":
			q.Label = args[i+1]
		case "--assignee":
			q.Assignee = args[i+1]
		case "--milestone":
			q.Milestone = args[i+1]
		}
	}
	if q.Assignee == "@me" {
		q.Assignee = g.login
	}
	return q.Matches(labels, assignees, milestone)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (g *repoGH) called(substr string) bool {
	for _, c := range g.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

// mentions reports whether any gh call names the given item as an argument or
// in a REST path. It is how "not touched at all" is asserted.
func (g *repoGH) mentions(n int) bool {
	s := strconv.Itoa(n)
	for _, c := range g.calls {
		for _, a := range c {
			if a == s || strings.Contains(a, "/"+s+"/") || strings.HasSuffix(a, "/"+s) {
				return true
			}
		}
	}
	return false
}

func labels(names ...string) []github.Label {
	var out []github.Label
	for _, n := range names {
		out = append(out, github.Label{Name: n})
	}
	return out
}

// ---- the fixture the acceptance criteria describe ---------------------------

// strandedRepo is the repository of issue #112: two open issues and one open
// pull request carrying `bees` with nobody assigned, plus one issue that
// carries no factory label at all and must come out untouched.
//
// #119 is the human-authored case that actually bit us: a person filed it with
// the base label, it carries no `<!-- bees:... -->` marker anywhere, and it is
// adopted exactly like the rest.
func strandedRepo(t *testing.T) *repoGH {
	t.Helper()
	return &repoGH{
		t:     t,
		login: "kyle",
		issues: []*github.Issue{
			{Number: 92, Title: "Work item", Body: "Implement the thing.\n\n<!-- bees:project_manager -->",
				Labels: labels("bees", "bees:ready"), Author: github.Author{Login: "kyle"}},
			{Number: 119, Title: "Guardrails so the factory doesn't run wild",
				Body:   "Filed by a person. No marker anywhere in this body.",
				Labels: labels("bees", "bees:feature"), Author: github.Author{Login: "someone-else"}},
			{Number: 77, Title: "Not the factory's business", Body: "A colleague's issue.",
				Author: github.Author{Login: "someone-else"}},
		},
		prs: []*github.PR{
			{Number: 148, Title: "A pull request nobody assigned", Labels: labels("bees"),
				HeadRefName: "bees/issue-92", Author: github.Author{Login: "kyle"}},
		},
	}
}

// fixFixture builds the doctor Deps for a filter of label=bees plus extra,
// backed by a mutable repository.
func fixFixture(t *testing.T, gh *repoGH, extraFilter string) *fixture {
	t.Helper()
	f := setup(t, "\n[filter]\n"+extraFilter, nil)
	gh.install(f.GitHub)
	f.gh = nil // the mutable fake replaced the table-driven one
	return f
}

// filterCheck is the one check --fix knows how to repair, as Deps.Checks
// wires it.
func (f *fixture) filterCheck() Check { return Check{Run: f.checkFilter, Fix: f.fixFilter} }

// TestFixAdoptsEveryLabelledItem is the acceptance criterion of #112: the two
// labelled issues and the labelled pull request are brought into the filter,
// the unlabelled issue is not touched at all, and the check passes afterwards.
func TestFixAdoptsEveryLabelledItem(t *testing.T) {
	gh := strandedRepo(t)
	f := fixFixture(t, gh, "assignee = \"kyle\"\n")
	checks := []Check{f.filterCheck()}

	before := Run(context.Background(), checks)
	wantResult(t, before[0], Warn, "2 open issues", "1 pull request", "0 match your filter")

	outcomes := ApplyFixes(context.Background(), checks, before)
	if len(outcomes) != 1 {
		t.Fatalf("got %d fix outcomes, want 1: %+v", len(outcomes), outcomes)
	}
	if outcomes[0].Err != nil {
		t.Errorf("fix reported errors: %v", outcomes[0].Err)
	}
	text := FixText(outcomes)
	for _, want := range []string{
		"assigned issue #92 to kyle",
		"assigned issue #119 to kyle",
		"assigned pull request #148 to kyle",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("fix output does not contain %q:\n%s", want, text)
		}
	}

	// The human-authored issue is adopted like the rest, and its body still
	// carries no bee marker: selection was on the label alone.
	if !github.HasAssignee(gh.issue(119).Assignees, "kyle") {
		t.Error("#119 (filed by a person, no bee marker) was not adopted")
	}
	if strings.Contains(gh.issue(119).Body, "bees:") {
		t.Fatal("fixture broken: #119 must carry no bee marker")
	}

	// The unlabelled issue is untouched: no gh call names it at all.
	if gh.mentions(77) {
		t.Errorf("#77 carries no factory label and must never be touched: %v", gh.calls)
	}
	if len(gh.issue(77).Assignees) != 0 {
		t.Error("#77 was assigned")
	}

	// The pull request went through the REST endpoint, never `gh pr edit`.
	if !gh.called("--method POST repos/owner/name/issues/148/assignees") {
		t.Errorf("the PR was not assigned through the REST endpoint: %v", gh.calls)
	}
	if gh.called("pr edit") {
		t.Errorf("`gh pr edit` is dead against GitHub and must not be used: %v", gh.calls)
	}

	// And the check passes on the re-run.
	after := Run(context.Background(), checks)
	wantResult(t, after[0], Pass, "2 open issues")
	if n := Failures(after); n != 0 {
		t.Errorf("%d checks still failing after the fix", n)
	}
}

// The milestone criterion is part of the filter too: an item outside
// filter.milestone stays invisible however it is assigned.
func TestFixSetsTheConfiguredMilestone(t *testing.T) {
	gh := strandedRepo(t)
	gh.milestones = []github.Milestone{{Number: 3, Title: "v0.1.0"}}
	f := fixFixture(t, gh, "assignee = \"kyle\"\nmilestone = \"v0.1.0\"\n")
	checks := []Check{f.filterCheck()}

	outcomes := ApplyFixes(context.Background(), checks, Run(context.Background(), checks))
	if outcomes[0].Err != nil {
		t.Fatalf("fix reported errors: %v", outcomes[0].Err)
	}
	if got := gh.issue(92).MilestoneTitle(); got != "v0.1.0" {
		t.Errorf("#92 milestone = %q, want v0.1.0", got)
	}
	if got := gh.pr(148).MilestoneTitle(); got != "v0.1.0" {
		t.Errorf("PR #148 milestone = %q, want v0.1.0", got)
	}
	if !strings.Contains(FixText(outcomes), "put issue #92 in milestone v0.1.0") {
		t.Errorf("fix output does not report the milestone:\n%s", FixText(outcomes))
	}
	wantResult(t, Run(context.Background(), checks)[0], Pass)
}

// filter.assignee = "@me" is a gh query shorthand, not a login: it has to be
// resolved before anything can be assigned.
func TestFixResolvesAtMe(t *testing.T) {
	gh := strandedRepo(t)
	f := fixFixture(t, gh, "assignee = \"@me\"\n")
	checks := []Check{f.filterCheck()}

	outcomes := ApplyFixes(context.Background(), checks, Run(context.Background(), checks))
	if outcomes[0].Err != nil {
		t.Fatalf("fix reported errors: %v", outcomes[0].Err)
	}
	if !github.HasAssignee(gh.issue(92).Assignees, "kyle") {
		t.Errorf("#92 assignees = %+v, want kyle", gh.issue(92).Assignees)
	}
	if gh.called("assignees[]=@me") {
		t.Errorf("`@me` was sent to the REST endpoint verbatim: %v", gh.calls)
	}
}

// Doctor without --fix changes nothing: a gh that fails every mutating call
// still gives a clean run.
func TestCheckWithoutFixMakesNoWriteCall(t *testing.T) {
	gh := strandedRepo(t)
	gh.failWrites = errors.New("gh: this fake refuses every write")
	f := fixFixture(t, gh, "assignee = \"kyle\"\n")

	results := Run(context.Background(), []Check{f.filterCheck()})
	wantResult(t, results[0], Warn, "0 match your filter")
	for _, c := range gh.calls {
		if mutating.MatchString(" " + strings.Join(c, " ")) {
			t.Errorf("`bees doctor` made a write call: gh %s", strings.Join(c, " "))
		}
	}
}

// The safety rule: without require_label there is no base label to tell the
// factory's items from everyone else's, so --fix refuses and says so.
func TestFixDoesNothingWithoutRequireLabel(t *testing.T) {
	gh := strandedRepo(t)
	f := fixFixture(t, gh, "require_label = false\nassignee = \"kyle\"\n")
	checks := []Check{f.filterCheck()}

	before := Run(context.Background(), checks)
	outcomes := ApplyFixes(context.Background(), checks, before)
	if len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if len(outcomes[0].Actions) != 1 || !strings.Contains(outcomes[0].Actions[0], "require_label") {
		t.Errorf("the refusal must be one line naming require_label: %+v", outcomes[0].Actions)
	}
	for _, c := range gh.calls {
		if mutating.MatchString(" " + strings.Join(c, " ")) {
			t.Errorf("--fix wrote something without a base label: gh %s", strings.Join(c, " "))
		}
	}
	after := Run(context.Background(), checks)
	if Failures(after) != Failures(before) {
		t.Error("the exit code changed although nothing was repaired")
	}
}

// One item that cannot be assigned is reported and skipped; the others are
// still adopted and the command carries on.
func TestFixReportsPerItemFailuresAndCarriesOn(t *testing.T) {
	gh := strandedRepo(t)
	gh.failAssign = map[int]error{119: errors.New("gh: HTTP 403 (forbidden)")}
	f := fixFixture(t, gh, "assignee = \"kyle\"\n")
	checks := []Check{f.filterCheck()}

	outcomes := ApplyFixes(context.Background(), checks, Run(context.Background(), checks))
	if outcomes[0].Err == nil {
		t.Fatal("a failing item must be reported")
	}
	if errs := flatten(outcomes[0].Err); len(errs) != 1 {
		t.Errorf("got %d errors, want 1: %v", len(errs), errs)
	}
	if !github.HasAssignee(gh.issue(92).Assignees, "kyle") || !github.HasAssignee(gh.pr(148).Assignees, "kyle") {
		t.Error("one failing item stopped the others from being adopted")
	}
	text := FixText(outcomes)
	if !strings.Contains(text, "! issue #119: assign to kyle: gh: HTTP 403") {
		t.Errorf("the failure is not reported per item:\n%s", text)
	}
}

// A filter that is the base label alone has nothing outside it to adopt.
func TestFixOnALabelOnlyFilterDoesNothing(t *testing.T) {
	gh := strandedRepo(t)
	f := fixFixture(t, gh, "label = \"bees\"\n")
	actions, err := f.fixFilter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || !strings.Contains(actions[0], "label alone") {
		t.Errorf("actions = %+v", actions)
	}
	if len(gh.calls) != 0 {
		t.Errorf("a label-only filter must not list anything: %v", gh.calls)
	}
}

// Everything already visible: the fix says so instead of printing nothing.
func TestFixWithNothingToAdopt(t *testing.T) {
	gh := strandedRepo(t)
	for _, i := range gh.issues {
		i.Assignees = []github.Author{{Login: "kyle"}}
	}
	gh.prs[0].Assignees = []github.Author{{Login: "kyle"}}
	f := fixFixture(t, gh, "assignee = \"kyle\"\n")

	actions, err := f.fixFilter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || !strings.Contains(actions[0], "nothing to adopt") {
		t.Errorf("actions = %+v", actions)
	}
}

// A listing that fails is one error, not a silent no-op: --fix must not
// report success when it could not even see the items.
func TestFixReportsAFailedListing(t *testing.T) {
	gh := &repoGH{t: t, login: "kyle"}
	f := fixFixture(t, gh, "assignee = \"kyle\"\n")
	gh.install(f.GitHub)
	f.GitHub.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("gh: exit status 1")
	}
	if _, err := f.fixFilter(context.Background()); err == nil {
		t.Fatal("a failing listing must be an error")
	}
}

// ApplyFixes only runs the fixes of the checks that did not pass, in check
// order, and leaves checks without a fix alone.
func TestApplyFixesOnlyRepairsFailingChecks(t *testing.T) {
	var ran []string
	fixFor := func(name string) Fix {
		return func(context.Context) ([]string, error) {
			ran = append(ran, name)
			return []string{name}, nil
		}
	}
	checks := []Check{
		{Run: func(context.Context) Result { return pass("ok", GroupConfig, "") }, Fix: fixFor("ok")},
		{Run: func(context.Context) Result { return warn("warned", GroupConfig, "", "do x") }, Fix: fixFor("warned")},
		{Run: func(context.Context) Result { return fail("failed", GroupConfig, "", "do y") }},
		{Run: func(context.Context) Result { return fail("also failed", GroupConfig, "", "do z") }, Fix: fixFor("also failed")},
	}
	results := Run(context.Background(), checks)
	outcomes := ApplyFixes(context.Background(), checks, results)

	if strings.Join(ran, ",") != "warned,also failed" {
		t.Errorf("fixes run = %v, want the failing ones in check order", ran)
	}
	if len(outcomes) != 2 || outcomes[0].Check != "warned" || outcomes[1].Check != "also failed" {
		t.Errorf("outcomes = %+v", outcomes)
	}
	if got := FixText(outcomes); got != "fixing warned\n  warned\nfixing also failed\n  also failed\n\n" {
		t.Errorf("FixText = %q", got)
	}
	if FixText(nil) != "" {
		t.Error("nothing fixed must print nothing")
	}
}

// FixText prints one line per joined error, not one paragraph.
func TestFixTextSplitsJoinedErrors(t *testing.T) {
	err := errors.Join(errors.New("issue #1: nope"), errors.Join(errors.New("issue #2:\nalso nope")))
	got := FixText([]FixOutcome{{Check: "c", Err: err}})
	want := "fixing c\n  ! issue #1: nope\n  ! issue #2: also nope\n\n"
	if got != want {
		t.Errorf("FixText = %q, want %q", got, want)
	}
}
