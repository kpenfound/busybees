package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

func TestFormatSummary(t *testing.T) {
	const dur = 11*time.Minute + 37*time.Second
	tests := []struct {
		name string
		sum  summary
		want string
	}{{
		name: "developer opened a PR",
		sum:  summary{role: config.RoleDeveloper, issue: 12, pr: 31, outcome: OutcomePROpened, turns: 87, cost: 2.41, dur: dur},
		want: "✓ developer issue #12 → PR #31 opened (87 turns, $2.41, 11m37s)",
	}, {
		name: "developer updated a PR",
		sum:  summary{role: config.RoleDeveloper, issue: 12, pr: 31, outcome: OutcomePRUpdated, turns: 41, cost: 0.98, dur: dur},
		want: "✓ developer issue #12 → PR #31 updated (41 turns, $0.98, 11m37s)",
	}, {
		name: "developer asked a question",
		sum:  summary{role: config.RoleDeveloper, issue: 12, outcome: OutcomeQuestion, note: "which format?", turns: 9, cost: 0.2, dur: dur},
		want: `✓ developer issue #12 asked the project manager: "which format?" (9 turns, $0.20, 11m37s)`,
	}, {
		name: "reviewer approved",
		sum:  summary{role: config.RoleReviewer, issue: 12, pr: 31, outcome: OutcomeApproved, note: "lgtm", turns: 23, cost: 0.47, dur: dur},
		want: `✓ reviewer PR #31 approved: "lgtm" (23 turns, $0.47, 11m37s)`,
	}, {
		name: "reviewer requested changes",
		sum:  summary{role: config.RoleReviewer, issue: 12, pr: 31, outcome: OutcomeChangesRequested, note: "tests missing", turns: 52, cost: 1.18, dur: dur},
		want: `✗ reviewer PR #31 changes requested: "tests missing" (52 turns, $1.18, 11m37s)`,
	}, {
		name: "singleton done with a note",
		sum:  summary{role: config.RoleProductManager, outcome: OutcomeDone, note: "filed 2 work items", turns: 34, cost: 0.61, dur: dur},
		want: `✓ product manager done: "filed 2 work items" (34 turns, $0.61, 11m37s)`,
	}, {
		name: "singleton idle without a note",
		sum:  summary{role: config.RoleQA, outcome: OutcomeIdle, turns: 4, cost: 0.03, dur: dur},
		want: "✓ QA engineer idle (4 turns, $0.03, 11m37s)",
	}, {
		name: "failed session",
		sum:  summary{role: config.RoleDeveloper, issue: 12, outcome: OutcomeFailed, note: "session timed out", turns: 120, cost: 3.5, dur: dur},
		want: `✗ developer issue #12 failed: "session timed out" (120 turns, $3.50, 11m37s)`,
	}, {
		name: "duration is rounded to seconds",
		sum:  summary{role: config.RoleQA, outcome: OutcomeIdle, dur: 2*time.Second + 700*time.Millisecond},
		want: "✓ QA engineer idle (0 turns, $0.00, 3s)",
	}, {
		name: "long notes are truncated",
		sum:  summary{role: config.RoleReviewer, pr: 7, outcome: OutcomeApproved, note: strings.Repeat("a", 100)},
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
