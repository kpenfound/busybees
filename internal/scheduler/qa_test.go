package scheduler

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
)

// qaMailTOML runs QA on its own, outside work hours, so the first tick is a
// full poll and every tick after it (within the hour) is a local pass — the
// pass on which a singleton's only wake trigger is its mailbox.
const qaMailTOML = baseTOML + `
qa_interval = "30m"
off_hours_poll_interval = "1h"
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "UTC"
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
`

// qaSaturday is off hours for qaMailTOML, so the tick after the first one is
// a local pass rather than a poll.
var qaSaturday = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// seedQAFirstRun gives QA a merged pull request and runs one full pass, so
// the first-run freebie (LastRun.IsZero) is spent and QA is inside its
// interval from then on.
func seedQAFirstRun(t *testing.T) *harness {
	t.Helper()
	h := newHarnessAt(t, qaMailTOML, qaSaturday)
	merged := h.clock.now().Add(-time.Minute)
	h.gh.prs[300] = &github.PR{Number: 300, Title: "Merged", State: "MERGED", HeadRefName: "bees/issue-9",
		Labels: []github.Label{{Name: "bees"}}, MergedAt: &merged}
	runPass(t, h)
	if got := len(h.sessions(config.RoleQA)); got != 1 {
		t.Fatalf("qa sessions after its first run: %d, want 1", got)
	}
	return h
}

// TestMailStartsQARunInsideItsInterval pins the contract behind `qa_interval`
// (#228): it is a floor on the runs QA starts *by itself*, not on the ones
// somebody directs at it. Mail is the only channel into a role, so a message
// addressed to `qa` is a person or the product manager asking for a run now;
// making it wait up to the interval would half-undo the steering channel
// #199 built. The cost is bounded because the run marks the mail read: a mail
// burst buys one extra session, not one per poll. This is the contract, not
// an accident of dispatchSingletons' mail-only override.
func TestMailStartsQARunInsideItsInterval(t *testing.T) {
	h := seedQAFirstRun(t)
	// Well inside qa_interval, and nothing has merged since QA last ran.
	h.clock.advance(5 * time.Minute)
	if _, err := h.box.Send(mail.Message{From: HumanSender, To: config.RoleQA,
		Subject: "Focus", Body: "check the new export path by hand"}); err != nil {
		t.Fatal(err)
	}
	runPass(t, h)
	qa := h.sessions(config.RoleQA)
	if len(qa) != 2 {
		t.Fatalf("mail should have started a qa session inside qa_interval: %d sessions, want 2", len(qa))
	}
	prompt := readFile(t, filepath.Join(qa[1], "prompt.md"))
	if !strings.Contains(prompt, "check the new export path by hand") {
		t.Errorf("the qa session was not handed the message:\n%s", prompt)
	}
	if unread, _ := h.box.List(mail.Filter{To: config.RoleQA, UnreadOnly: true}); len(unread) != 0 {
		t.Errorf("qa mail left unread, so the next pass would start another session: %+v", unread)
	}
}

// TestQAIntervalStillBoundsUnpromptedRuns is the other half of that contract
// (#228): mail lifts the floor, nothing else does. With an empty QA mailbox,
// neither a local pass nor a full poll starts a session while `qa_interval`
// has not elapsed — even with something newly merged to look at — and the
// session that was held back does run once the interval passes, which is what
// shows the interval is what held it.
func TestQAIntervalStillBoundsUnpromptedRuns(t *testing.T) {
	h := seedQAFirstRun(t)
	// Something new to test, but nobody asked QA for anything.
	h.clock.advance(5 * time.Minute)
	merged := h.clock.now()
	h.gh.prs[300].MergedAt = &merged

	runPass(t, h) // a local pass: no mail, so no session
	if got := len(h.sessions(config.RoleQA)); got != 1 {
		t.Fatalf("qa sessions after a local pass with an empty mailbox: %d, want 1", got)
	}
	forcePoll(h)
	runPass(t, h) // a full poll, still inside qa_interval
	if got := len(h.sessions(config.RoleQA)); got != 1 {
		t.Fatalf("qa sessions after a poll inside qa_interval: %d, want 1", got)
	}

	h.clock.advance(time.Hour)
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleQA)); got != 2 {
		t.Fatalf("qa sessions once qa_interval has elapsed: %d, want 2", got)
	}
}
