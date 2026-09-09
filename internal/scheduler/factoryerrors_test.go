package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/feedback"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/workspace"
)

// scheduler.report_factory_errors reaches a session through its system
// prompt: on, the developer is told about report_factory_error and how to
// scrub; off (the default), the prompt does not name the tool.
func TestReportFactoryErrorsReachesTheSessionPrompt(t *testing.T) {
	on := strings.Replace(devOnlyTOML, "max_review_rounds = 3\n", "max_review_rounds = 3\nreport_factory_errors = true\n", 1)
	if on == devOnlyTOML {
		t.Fatal("fixture: could not switch report_factory_errors on")
	}
	for toml, want := range map[string]bool{devOnlyTOML: false, on: true} {
		h := newHarnessAt(t, toml, time.Now())
		h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
		seedReady(h, 1, "m", time.Now().Add(-time.Hour))
		if err := h.sched.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		sys := systemPromptOf(t, h, 0)
		if got := strings.Contains(sys, "### Reporting an error the factory caused"); got != want {
			t.Errorf("report_factory_errors=%v: prompt names the tool = %v:\n%s", want, got, sys)
		}
	}
}

// factoryErrorsTOML runs the developer alone with
// scheduler.report_factory_errors on, so a queued draft is filed.
var factoryErrorsTOML = strings.Replace(devOnlyTOML, "max_review_rounds = 3\n", "max_review_rounds = 3\nreport_factory_errors = true\n", 1)

// upstreamStub is the busybees repository the factory-error drafts are filed
// against: it answers the duplicate check's listing with issues and records
// every gh call, so a test can assert what was written and where.
type upstreamStub struct {
	mu     sync.Mutex
	issues []github.Issue
	calls  [][]string
	// fail makes one call fail: it is given the arguments of every call.
	fail    func(args []string) error
	created int
}

func (u *upstreamStub) exec(_ context.Context, args ...string) ([]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, append([]string(nil), args...))
	if u.fail != nil {
		if err := u.fail(args); err != nil {
			return nil, err
		}
	}
	switch strings.Join(args[:2], " ") {
	case "issue list":
		return json.Marshal(u.issues)
	case "issue create":
		u.created++
		return []byte(fmt.Sprintf("https://github.com/kpenfound/busybees/issues/%d\n", 900+u.created)), nil
	case "issue comment":
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected upstream gh call: %v", args)
}

// install points the scheduler's upstream client at the stub.
func (u *upstreamStub) install(h *harness) {
	c := github.New("kpenfound/busybees")
	c.Exec = u.exec
	h.sched.upstream = c
}

// of returns the calls whose first two arguments are cmd ("issue create").
func (u *upstreamStub) of(cmd string) [][]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out [][]string
	for _, c := range u.calls {
		if len(c) >= 2 && c[0]+" "+c[1] == cmd {
			out = append(out, c)
		}
	}
	return out
}

// queueDraft puts a draft in the factory's feedback queue, as
// report_factory_error does from a session.
func queueDraft(t *testing.T, h *harness, title, detail string, at time.Time) feedback.Draft {
	t.Helper()
	d, err := feedback.Open(h.store.FeedbackDir()).Add(feedback.Draft{
		Role: config.RoleDeveloper, SessionDir: "/tmp/sessions/20260908-developer",
		Title: title, Detail: detail, CreatedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// queuedDrafts is what is left in the queue.
func queuedDrafts(t *testing.T, h *harness) []feedback.Draft {
	t.Helper()
	got, err := feedback.Open(h.store.FeedbackDir()).List()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A draft nothing upstream looks like is filed as a new issue against the
// busybees repository — with no label and no assignee, so the factory's own
// filter never picks its own bug report up as a work item — and the draft
// leaves the queue. The whole thing runs from a full pass.
func TestFullPassFilesAQueuedFactoryError(t *testing.T) {
	h := newHarnessAt(t, factoryErrorsTOML, time.Now())
	up := &upstreamStub{issues: []github.Issue{{Number: 12, Title: "the live view flickers", Body: "on every redraw", State: "OPEN"}}}
	up.install(h)
	d := queueDraft(t, h, "issue_view returned an issue without its comments",
		"Called issue_view on <issue>; the result carried no comments, though the issue has three.", time.Now())

	if err := h.sched.pass(context.Background()); err != nil {
		t.Fatal(err)
	}

	created := up.of("issue create")
	if len(created) != 1 {
		t.Fatalf("issue create calls: %v", up.calls)
	}
	args := created[0]
	if argValue(args, "-R") != "kpenfound/busybees" {
		t.Errorf("filed against %q, want kpenfound/busybees: %v", argValue(args, "-R"), args)
	}
	if slices.Contains(args, "--label") || slices.Contains(args, "--assignee") {
		t.Errorf("an upstream report must stay outside the factory's filter: %v", args)
	}
	if got := argValue(args, "--title"); got != d.Title {
		t.Errorf("title %q, want %q", got, d.Title)
	}
	body := argValue(args, "--body")
	if !strings.Contains(body, d.Detail) {
		t.Errorf("body lost the draft's detail: %q", body)
	}
	if !strings.Contains(body, "Filed automatically by a bee factory's feedback loop.") {
		t.Errorf("body does not say a bee filed it: %q", body)
	}
	if strings.Contains(body, d.SessionDir) {
		t.Errorf("body carries the session directory: %q", body)
	}
	if n := len(up.of("issue comment")); n != 0 {
		t.Errorf("commented %d times as well as filing", n)
	}
	if got := queuedDrafts(t, h); len(got) != 0 {
		t.Errorf("the filed draft is still queued: %+v", got)
	}
}

// A draft that looks like an issue already there is a comment on that issue,
// not a second report of it.
func TestAFactoryErrorThatDuplicatesAnIssueIsCommented(t *testing.T) {
	h := newHarnessAt(t, factoryErrorsTOML, time.Now())
	up := &upstreamStub{issues: []github.Issue{
		{Number: 7, Title: "the live view flickers", Body: "on every redraw", State: "OPEN"},
		{Number: 41, Title: "issue_view returned an issue without its comments", Body: "the comments are missing", State: "CLOSED"},
	}}
	up.install(h)
	d := queueDraft(t, h, "issue_view returned an issue without its comments",
		"Called issue_view on <issue>; the result carried no comments, though the issue has three.", time.Now())

	h.sched.drainFeedbackQueue(context.Background())

	if n := len(up.of("issue create")); n != 0 {
		t.Errorf("filed %d new issues instead of commenting: %v", n, up.calls)
	}
	commented := up.of("issue comment")
	if len(commented) != 1 {
		t.Fatalf("issue comment calls: %v", up.calls)
	}
	args := commented[0]
	if args[2] != "41" || argValue(args, "-R") != "kpenfound/busybees" {
		t.Errorf("commented on %v, want issue 41 of kpenfound/busybees", args)
	}
	body := argValue(args, "--body")
	if !strings.Contains(body, "Also seen:") || !strings.Contains(body, d.Detail) {
		t.Errorf("comment body %q, want the detail under a line saying it was seen again", body)
	}
	if got := queuedDrafts(t, h); len(got) != 0 {
		t.Errorf("the reported draft is still queued: %+v", got)
	}
}

// A GitHub failure costs one draft, not the pass: the draft stays queued for
// the next one and the drafts behind it are still filed.
func TestAFailedFactoryErrorReportLeavesTheDraftQueued(t *testing.T) {
	h := newHarnessAt(t, factoryErrorsTOML, time.Now())
	up := &upstreamStub{fail: func(args []string) error {
		if argValue(args, "--title") == "first" {
			return errors.New("gh: api rate limit exceeded")
		}
		return nil
	}}
	up.install(h)
	now := time.Now()
	first := queueDraft(t, h, "first", "the first thing that went wrong", now.Add(-time.Hour))
	queueDraft(t, h, "second", "the second thing that went wrong", now)

	h.sched.drainFeedbackQueue(context.Background())

	created := up.of("issue create")
	if len(created) != 2 {
		t.Fatalf("issue create calls: %v", up.calls)
	}
	if got := argValue(created[1], "--title"); got != "second" {
		t.Errorf("the draft behind the failing one was not filed: %v", up.calls)
	}
	got := queuedDrafts(t, h)
	if len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("queue = %+v, want only the draft whose call failed", got)
	}
	if !strings.Contains(h.logs.String(), "could not file a factory-error report") {
		t.Errorf("the failure was not logged:\n%s", h.logs.String())
	}
}

// Off, the queue is not read and nothing is filed: a person who switches the
// feature off stops the filing too, and the drafts stay where they are.
func TestFactoryErrorsAreNotFiledWhenTheKeyIsOff(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	up := &upstreamStub{}
	up.install(h)
	d := queueDraft(t, h, "issue_view returned an issue without its comments", "detail", time.Now())

	if err := h.sched.pass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(up.calls) != 0 {
		t.Errorf("report_factory_errors is off and the pass still called gh upstream: %v", up.calls)
	}
	got := queuedDrafts(t, h)
	if len(got) != 1 || got[0].ID != d.ID {
		t.Errorf("queue = %+v, want the draft left alone", got)
	}
}

// The client the factory-error reports go through is the scheduler's own with
// the repository swapped: it writes to busybees rather than to the repository
// this factory builds, acts as the same [github] account, and inherits the
// gh hook — so a test's fake covers it and no factory-error path can reach
// the real GitHub.
func TestTheUpstreamClientIsBusybeesWithTheFactorysIdentity(t *testing.T) {
	h := newHarness(t, factoryErrorsTOML)
	gh := github.NewAs(h.cfg.Project.Repo, "beebot", "t0ken")
	var calls [][]string
	gh.Exec = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("https://github.com/kpenfound/busybees/issues/900\n"), nil
	}
	s, err := New(Deps{Config: h.cfg, GitHub: gh, Mail: h.box, Runner: &session.Runner{},
		Workspaces: workspace.NewManager(h.clone, t.TempDir()), Store: h.store})
	if err != nil {
		t.Fatal(err)
	}
	if s.upstream.Repo != "kpenfound/busybees" {
		t.Errorf("upstream repo %q, want kpenfound/busybees", s.upstream.Repo)
	}
	if s.gh.Repo != h.cfg.Project.Repo {
		t.Errorf("the factory's own client moved to %q", s.gh.Repo)
	}
	if s.upstream.ActsAs != "beebot" || s.upstream.Token != "t0ken" {
		t.Errorf("upstream identity %q/%q, want the factory's own", s.upstream.ActsAs, s.upstream.Token)
	}
	if _, err := s.upstream.CreateIssue(context.Background(), github.NewIssue{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || argValue(calls[0], "-R") != "kpenfound/busybees" {
		t.Fatalf("upstream call %v, want one through the factory's gh hook against busybees", calls)
	}
}

// A queue that cannot be read is a warning and nothing else: the pass carries
// on and writes nothing upstream on a guess.
func TestAnUnreadableFeedbackQueueFilesNothing(t *testing.T) {
	h := newHarnessAt(t, factoryErrorsTOML, time.Now())
	up := &upstreamStub{}
	up.install(h)
	queueDraft(t, h, "a draft that is fine", "detail", time.Now())
	if err := os.WriteFile(filepath.Join(h.store.FeedbackDir(), "corrupt.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.sched.drainFeedbackQueue(context.Background())

	if len(up.calls) != 0 {
		t.Errorf("a corrupt queue was filed anyway: %v", up.calls)
	}
	if !strings.Contains(h.logs.String(), "could not read the factory-error queue") {
		t.Errorf("the unreadable queue was not reported:\n%s", h.logs.String())
	}
}
