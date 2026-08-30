package scheduler

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{{
		name: "shorter than the limit is untouched",
		in:   "short",
		n:    80,
		want: "short",
	}, {
		name: "exactly the limit is untouched",
		in:   strings.Repeat("a", 80),
		n:    80,
		want: strings.Repeat("a", 80),
	}, {
		name: "one rune over the limit is cut",
		in:   strings.Repeat("a", 81),
		n:    80,
		want: strings.Repeat("a", 80) + "…",
	}, {
		name: "the cut lands on a rune boundary",
		in:   strings.Repeat("a", 79) + "é" + "tests",
		n:    80,
		want: strings.Repeat("a", 79) + "é" + "…",
	}, {
		name: "the limit counts runes, not bytes",
		in:   strings.Repeat("é", 80),
		n:    80,
		want: strings.Repeat("é", 80),
	}, {
		name: "empty stays empty",
		in:   "",
		n:    80,
		want: "",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("\n got: %q\nwant: %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("not valid UTF-8: %q", got)
			}
		})
	}
}

func TestOneLine(t *testing.T) {
	if got, want := oneLine("line one\r\n\tline two", 80), "line one line two"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got, want := oneLine("  leading and   trailing  ", 80), "leading and trailing"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// The limit applies to the flattened text, and the result is one line.
	got := oneLine(strings.Repeat("word\nword\n", 40), 80)
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("still multi-line: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 81 { // 80 runes + the ellipsis
		t.Errorf("got %d runes, want 80 + \"…\": %q", n, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing the ellipsis: %q", got)
	}
}

// The escalation summary line does not go through formatSummary, so it
// flattens and truncates the reason itself. The GitHub comment, where the
// detail is useful, keeps the reason in full.
func TestEscalationSummaryIsOneLine(t *testing.T) {
	h := newHarness(t, baseTOML)
	h.gh.issues[12] = &github.Issue{Number: 12, Title: "Build the thing", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:in-progress"}}}

	res := &session.Result{IsError: true, ErrorSubtype: "error_during_execution",
		ResultText: "step one failed\nstep two failed"}
	status, note := outcomeOf(res)
	if err := h.sched.escalate(context.Background(), 12, h.sched.sessionFailure(config.RoleDeveloper, res, status, note)); err != nil {
		t.Fatal(err)
	}

	var line string
	for _, l := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(l, "escalated to a human") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no escalation line in:\n%s", h.logs.String())
	}
	if !strings.Contains(line, "step one failed step two failed") {
		t.Errorf("the reason is not flattened onto the escalation line:\n%s", line)
	}
	if strings.Contains(h.logs.String(), "\nstep two failed") {
		t.Errorf("the escalation spilled onto a second console line:\n%s", h.logs.String())
	}

	if len(h.gh.comments[12]) != 1 {
		t.Fatalf("comments: %v", h.gh.comments[12])
	}
	body := h.gh.comments[12][0]
	if !strings.Contains(body, "step one failed") || !strings.Contains(body, "step two failed") {
		t.Errorf("the comment lost part of the reason:\n%s", body)
	}
}
