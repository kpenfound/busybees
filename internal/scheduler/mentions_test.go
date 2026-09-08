package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// mentionLogin is the account the factory acts as in these tests: with
// [github] set, an @mention of it is a person asking for a role by name.
const mentionLogin = "busybees-bot"

// mentionHarness is the harness with a factory account configured, which is
// what makes a mention distinguishable from a person naming themselves.
func mentionHarness(t *testing.T, now time.Time) *harness {
	t.Helper()
	h := newHarnessAt(t, noRolesTOML, now)
	h.sched.gh.ActsAs = mentionLogin
	return h
}

// seedMentionIssue puts an open issue carrying labels in the fake GitHub.
func seedMentionIssue(h *harness, n int, now time.Time, labels ...string) {
	ls := []github.Label{{Name: "bees"}}
	for _, l := range labels {
		ls = append(ls, github.Label{Name: l})
	}
	h.gh.issues[n] = &github.Issue{Number: n, Title: "Seeded", State: "OPEN",
		Labels: ls, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
}

// TestAMentionOnAPreFlightIssueReachesTheProjectManager: triage and ready
// deliver nothing on their own — the session that picks the issue up renders
// its comment history in its own prompt — so an @mention of the factory's
// login is the one signal that gets a comment out of them, and only that
// comment. The project manager owns both states, so it is what a mention
// there wakes.
func TestAMentionOnAPreFlightIssueReachesTheProjectManager(t *testing.T) {
	now := time.Now()
	h := mentionHarness(t, now)
	seedMentionIssue(h, 1, now, "bees:triage")
	seedMentionIssue(h, 2, now, "bees:ready", "bees:size/s")
	seedMentionIssue(h, 3, now, "bees:ready", "bees:size/s")

	// Pass 1 seeds the clocks and delivers nothing.
	deliverIssueCommentsOnce(t, h)
	if msgs := roleMail(t, h, config.RoleProjectManager); len(msgs) != 0 {
		t.Fatalf("the seeding pass delivered %d messages, want none: %+v", len(msgs), msgs)
	}

	h.clock.advance(time.Minute)
	said := now.Add(time.Minute)
	for _, n := range []int{1, 2, 3} {
		h.gh.issues[n].UpdatedAt = said
	}
	seedIssueComments(h, 1, issueComment(901, "kyle", "@busybees-bot this one is urgent", said))
	seedIssueComments(h, 2, issueComment(902, "robin", "hey @BusyBees-Bot, split this in two", said))
	seedIssueComments(h, 3, issueComment(903, "kyle", "while you are in there, rename the flag", said))

	deliverIssueCommentsOnce(t, h)

	msgs := roleMail(t, h, config.RoleProjectManager)
	if len(msgs) != 2 {
		t.Fatalf("%d messages for the project manager, want the two mentions: %+v", len(msgs), msgs)
	}
	if msgs[0].Issue != 1 || msgs[1].Issue != 2 {
		t.Fatalf("mail is about issues %d and %d, want 1 (triage) and 2 (ready)", msgs[0].Issue, msgs[1].Issue)
	}
	for _, m := range msgs {
		if m.From != HumanSender {
			t.Errorf("mail about issue #%d is from %q, want %q", m.Issue, m.From, HumanSender)
		}
	}
	if !strings.Contains(msgs[0].Body, "this one is urgent") {
		t.Errorf("the triage mention's body is missing the comment:\n%s", msgs[0].Body)
	}
	if !strings.Contains(msgs[1].Body, "split this in two") {
		t.Errorf("the ready mention's body is missing the comment:\n%s", msgs[1].Body)
	}
	// The comment that mentions nobody is history the next session reads for
	// itself: nothing is mailed about issue 3, to anyone.
	for _, role := range []string{config.RoleProjectManager, config.RoleProductManager, config.RoleDeveloper} {
		for _, m := range roleMail(t, h, role) {
			if m.Issue == 3 || strings.Contains(m.Body, "rename the flag") {
				t.Errorf("a comment mentioning nobody was delivered to %s: %+v", role, m)
			}
		}
	}
	if got := len(h.sched.wake); got != 1 {
		t.Fatalf("%d wakes pending after the mentions were delivered, want 1", got)
	}
}

// TestAMentionOnAFeatureOrFeedbackIssueReachesTheProductManager: a feature
// and a feedback issue sit outside the workflow state machine altogether,
// and both are the product manager's.
func TestAMentionOnAFeatureOrFeedbackIssueReachesTheProductManager(t *testing.T) {
	now := time.Now()
	h := mentionHarness(t, now)
	seedMentionIssue(h, 1, now, "bees:feature")
	seedMentionIssue(h, 2, now, "bees:feedback")
	seedMentionIssue(h, 3, now, "bees:feature")

	deliverIssueCommentsOnce(t, h)

	h.clock.advance(time.Minute)
	said := now.Add(time.Minute)
	for _, n := range []int{1, 2, 3} {
		h.gh.issues[n].UpdatedAt = said
	}
	seedIssueComments(h, 1, issueComment(901, "kyle", "@busybees-bot break this one up now", said))
	seedIssueComments(h, 2, issueComment(902, "robin", "@busybees-bot worth a feature?", said))
	seedIssueComments(h, 3, issueComment(903, "kyle", "still thinking about this one", said))

	deliverIssueCommentsOnce(t, h)

	msgs := roleMail(t, h, config.RoleProductManager)
	if len(msgs) != 2 {
		t.Fatalf("%d messages for the product manager, want the feature's and the feedback's: %+v", len(msgs), msgs)
	}
	if msgs[0].Issue != 1 || msgs[1].Issue != 2 {
		t.Fatalf("mail is about issues %d and %d, want 1 (feature) and 2 (feedback)", msgs[0].Issue, msgs[1].Issue)
	}
	if !strings.Contains(msgs[0].Body, "break this one up now") || !strings.Contains(msgs[1].Body, "worth a feature?") {
		t.Errorf("mail bodies are missing the comments:\n%s\n%s", msgs[0].Body, msgs[1].Body)
	}
	if got := roleMail(t, h, config.RoleProjectManager); len(got) != 0 {
		t.Errorf("the project manager was mailed a feature or feedback issue: %+v", got)
	}
	if got := len(h.sched.wake); got != 1 {
		t.Fatalf("%d wakes pending after the mentions were delivered, want 1", got)
	}
}

// TestWithoutAConfiguredLoginNoMentionIsDelivered: with [github] unset the
// factory shares its GitHub account with the people it works for, so there is
// no name that is the factory's and not a person's. Delivery from the states
// that have no session to steer is off, and costs no comment fetch either.
func TestWithoutAConfiguredLoginNoMentionIsDelivered(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, noRolesTOML, now) // no ActsAs: the shared account
	if h.sched.gh.ActsAs != "" {
		t.Fatalf("the harness acts as %q, want the shared account", h.sched.gh.ActsAs)
	}
	seedMentionIssue(h, 1, now, "bees:triage")
	seedMentionIssue(h, 2, now, "bees:feature")

	deliverIssueCommentsOnce(t, h)

	h.clock.advance(time.Minute)
	said := now.Add(time.Minute)
	h.gh.issues[1].UpdatedAt, h.gh.issues[2].UpdatedAt = said, said
	seedIssueComments(h, 1, issueComment(901, "kyle", "@busybees-bot and @kyle, look at this", said))
	seedIssueComments(h, 2, issueComment(902, "robin", "@busybees-bot too", said))

	deliverIssueCommentsOnce(t, h)

	for _, role := range []string{config.RoleProjectManager, config.RoleProductManager, config.RoleDeveloper} {
		if msgs := roleMail(t, h, role); len(msgs) != 0 {
			t.Errorf("%s was mailed a mention with no factory account configured: %+v", role, msgs)
		}
	}
	if n := h.gh.callCount("api --paginate"); n != 0 {
		t.Errorf("%d comment fetches with no factory account configured, want none", n)
	}
	if got := len(h.sched.wake); got != 0 {
		t.Errorf("%d wakes pending, want none", got)
	}
}

// TestAnInFlightIssueStillDeliversEveryComment: the mention gate belongs to
// the states nothing is delivered from. An issue the factory is working on
// delivers every comment a person writes, mention or no mention, and
// configuring a factory account must not turn that into a gate.
func TestAnInFlightIssueStillDeliversEveryComment(t *testing.T) {
	now := time.Now()
	h := mentionHarness(t, now)
	seedMentionIssue(h, 1, now, "bees:in-progress", "bees:size/s")

	deliverIssueCommentsOnce(t, h)

	h.clock.advance(time.Minute)
	said := now.Add(time.Minute)
	h.gh.issues[1].UpdatedAt = said
	seedIssueComments(h, 1, issueComment(901, "kyle", "drop the second argument", said))

	deliverIssueCommentsOnce(t, h)

	msgs := developerMail(t, h)
	if len(msgs) != 1 {
		t.Fatalf("%d messages for the developer, want the comment: %+v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0].Body, "drop the second argument") {
		t.Errorf("mail body missing the comment:\n%s", msgs[0].Body)
	}
}

// TestMentionsLogin pins where a mention starts and ends: GitHub logins are
// case-insensitive and may carry hyphens, so a longer login that starts with
// this one is somebody else, and an address is nobody.
func TestMentionsLogin(t *testing.T) {
	for _, tc := range []struct {
		body  string
		login string
		want  bool
	}{
		{"@busybees-bot please look", mentionLogin, true},
		{"ping @BUSYBEES-BOT", mentionLogin, true},
		{"(@busybees-bot)", mentionLogin, true},
		{"@busybees-bot", mentionLogin, true},
		{"cc @kyle and @busybees-bot", mentionLogin, true},
		{"@busybees-bot2 is somebody else", mentionLogin, false},
		{"@busybees-bot-staging is somebody else", mentionLogin, false},
		{"mail bees@busybees-bot instead", mentionLogin, false},
		{"busybees-bot, without the at sign", mentionLogin, false},
		{"nothing here", mentionLogin, false},
		{"@busybees-bot", "", false},
		{"@busybees-bot2 then @busybees-bot", mentionLogin, true},
	} {
		if got := mentionsLogin(tc.body, tc.login); got != tc.want {
			t.Errorf("mentionsLogin(%q, %q) = %v, want %v", tc.body, tc.login, got, tc.want)
		}
	}
}
