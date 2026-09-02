package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

func TestFormatSummary(t *testing.T) {
	const dur = 11*time.Minute + 37*time.Second
	tests := []struct {
		name string
		sum  summary
		want string
	}{{
		name: "developer opened a PR",
		sum:  summary{costKnown: true, role: config.RoleDeveloper, issue: 12, pr: 31, outcome: OutcomePROpened, turns: 87, cost: 2.41, dur: dur},
		want: "✓ developer issue #12 → PR #31 opened (87 turns, $2.41, 11m37s)",
	}, {
		name: "developer updated a PR",
		sum:  summary{costKnown: true, role: config.RoleDeveloper, issue: 12, pr: 31, outcome: OutcomePRUpdated, turns: 41, cost: 0.98, dur: dur},
		want: "✓ developer issue #12 → PR #31 updated (41 turns, $0.98, 11m37s)",
	}, {
		name: "developer asked a question",
		sum:  summary{costKnown: true, role: config.RoleDeveloper, issue: 12, outcome: OutcomeQuestion, note: "which format?", turns: 9, cost: 0.2, dur: dur},
		want: `✓ developer issue #12 asked the project manager: "which format?" (9 turns, $0.20, 11m37s)`,
	}, {
		name: "reviewer approved",
		sum:  summary{costKnown: true, role: config.RoleReviewer, issue: 12, pr: 31, outcome: OutcomeApproved, note: "lgtm", turns: 23, cost: 0.47, dur: dur},
		want: `✓ reviewer PR #31 approved: "lgtm" (23 turns, $0.47, 11m37s)`,
	}, {
		name: "reviewer requested changes",
		sum:  summary{costKnown: true, role: config.RoleReviewer, issue: 12, pr: 31, outcome: OutcomeChangesRequested, note: "tests missing", turns: 52, cost: 1.18, dur: dur},
		want: `✗ reviewer PR #31 changes requested: "tests missing" (52 turns, $1.18, 11m37s)`,
	}, {
		name: "singleton done with a note",
		sum:  summary{costKnown: true, role: config.RoleProductManager, outcome: OutcomeDone, note: "filed 2 work items", turns: 34, cost: 0.61, dur: dur},
		want: `✓ product manager done: "filed 2 work items" (34 turns, $0.61, 11m37s)`,
	}, {
		name: "singleton idle without a note",
		sum:  summary{costKnown: true, role: config.RoleQA, outcome: OutcomeIdle, turns: 4, cost: 0.03, dur: dur},
		want: "✓ QA engineer idle (4 turns, $0.03, 11m37s)",
	}, {
		name: "failed session",
		sum:  summary{costKnown: true, role: config.RoleDeveloper, issue: 12, outcome: OutcomeFailed, note: "session timed out", turns: 120, cost: 3.5, dur: dur},
		want: `✗ developer issue #12 failed: "session timed out" (120 turns, $3.50, 11m37s)`,
	}, {
		name: "duration is rounded to seconds",
		sum:  summary{costKnown: true, role: config.RoleQA, outcome: OutcomeIdle, dur: 2*time.Second + 700*time.Millisecond},
		want: "✓ QA engineer idle (0 turns, $0.00, 3s)",
	}, {
		name: "a multi-line note stays on one line",
		sum:  summary{costKnown: true, role: config.RoleQA, outcome: OutcomeDone, note: "line one\nline two", turns: 1, cost: 1, dur: time.Second},
		want: `✓ QA engineer done: "line one line two" (1 turn, $1.00, 1s)`,
	}, {
		name: "a one-turn session says turn, not turns",
		sum:  summary{costKnown: true, role: config.RoleQA, outcome: OutcomeIdle, turns: 1, cost: 0.02, dur: dur},
		want: "✓ QA engineer idle (1 turn, $0.02, 11m37s)",
	}, {
		name: "a session that really cost nothing still says so",
		sum:  summary{costKnown: true, role: config.RoleQA, outcome: OutcomeIdle, turns: 2, cost: 0, dur: dur},
		want: "✓ QA engineer idle (2 turns, $0.00, 11m37s)",
	}, {
		// A session killed by a signal never reported a cost, and $0.00
		// would read as a session that spent nothing.
		name: "a session whose cost is not known says so",
		sum:  summary{role: config.RoleDeveloper, issue: 330, outcome: OutcomeFailed, note: "session error (signal_killed)", turns: 27, dur: dur},
		want: `✗ developer issue #330 failed: "session error (signal_killed)" (27 turns, cost unknown, 11m37s)`,
	}, {
		name: "long notes are truncated",
		sum:  summary{costKnown: true, role: config.RoleReviewer, pr: 7, outcome: OutcomeApproved, note: strings.Repeat("a", 100)},
		want: `✓ reviewer PR #7 approved: "` + strings.Repeat("a", 80) + `…" (0 turns, $0.00, 0s)`,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatSummary(tc.sum); got != tc.want {
				t.Errorf("\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// Cutting the note at a byte offset would slice the "é" in half.
func TestFormatSummaryIsValidUTF8(t *testing.T) {
	sum := summary{costKnown: true, role: config.RoleReviewer, pr: 7, outcome: OutcomeApproved,
		note: strings.Repeat("a", 79) + "é" + "tests"}
	got := formatSummary(sum)
	if !utf8.ValidString(got) {
		t.Fatalf("not valid UTF-8: %q", got)
	}
	want := `✓ reviewer PR #7 approved: "` + strings.Repeat("a", 79) + "é" + `…" (0 turns, $0.00, 0s)`
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

// TestASignalledSessionIsReportedWithItsSignal drives a real session that
// dies from SIGKILL through the scheduler and reads the line a person sees
// in bees.log. The formatSummary table above pins the rendering; this pins
// the wiring, which is the half that made the report useless: the summary
// used to say "exit_-1" with 0 turns and $0.00 for a session that had
// worked for minutes.
func TestASignalledSessionIsReportedWithItsSignal(t *testing.T) {
	t.Setenv("FAKE_SIGNAL", "9")
	h := newHarness(t, strings.Replace(devOnlyTOML, "[scheduler]\n", "[scheduler]\nretries = 0\n", 1))
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		CreatedAt: time.Now().Add(-time.Hour),
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}

	h.sched.Once = true
	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	logs := h.logs.String()
	if !strings.Contains(logs, "session error (signal_killed)") {
		t.Errorf("summary does not name the signal:\n%s", logs)
	}
	if !strings.Contains(logs, "cost unknown") {
		t.Errorf("summary reports a cost claude never gave:\n%s", logs)
	}
	if strings.Contains(logs, "(0 turns,") {
		t.Errorf("summary says the session did nothing:\n%s", logs)
	}
	if strings.Contains(logs, "exit_-1") {
		t.Errorf("summary still reports a signal as an exit code:\n%s", logs)
	}
}
