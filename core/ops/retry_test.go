package ops

import (
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name string
		res  agent.Result
		want FailureKind
	}{
		{
			name: "reported an outcome",
			res:  agent.Result{HasOutcome: true, Outcome: agent.Outcome{Status: "built"}},
			want: FailureBehavioural,
		},
		{
			name: "reported failure",
			res:  agent.Result{HasOutcome: true, Outcome: agent.Outcome{Status: "failed", Note: "cannot build"}},
			want: FailureBehavioural,
		},
		{
			name: "reported an outcome after an error",
			res:  agent.Result{HasOutcome: true, Outcome: agent.Outcome{Status: "query"}, IsError: true, ErrorSubtype: "error_max_turns"},
			want: FailureBehavioural,
		},
		{
			name: "timed out",
			res:  agent.Result{TimedOut: true, IsError: true, ExitCode: -1, ErrorSubtype: "timeout"},
			want: FailureInfra,
		},
		{
			name: "out of turns",
			res:  agent.Result{IsError: true, ErrorSubtype: "error_max_turns"},
			want: FailureInfra,
		},
		{
			name: "api error",
			res:  agent.Result{IsError: true, ErrorSubtype: "error_during_execution"},
			want: FailureInfra,
		},
		{
			name: "the agent crashed without closing its stream",
			res:  agent.Result{IsError: true, ExitCode: 1, ErrorSubtype: "no_result", ResultText: "panic"},
			want: FailureInfra,
		},
		{
			name: "non-zero exit without an error flag",
			res:  agent.Result{ExitCode: 2},
			want: FailureInfra,
		},
		{
			name: "rate limited",
			res:  agent.Result{ResultText: "API Error: 429 Rate limit exceeded"},
			want: FailureInfra,
		},
		{
			name: "overloaded",
			res:  agent.Result{ResultText: "Overloaded"},
			want: FailureInfra,
		},
		{
			name: "usage limit",
			res:  agent.Result{ResultText: "Claude usage limit reached"},
			want: FailureInfra,
		},
		{
			name: "clean exit without an outcome",
			res:  agent.Result{ResultText: "all done!"},
			want: FailureBehavioural,
		},
		{
			name: "empty outcome status",
			res:  agent.Result{HasOutcome: true},
			want: FailureBehavioural,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFailure(&tc.res); got != tc.want {
				t.Fatalf("ClassifyFailure = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestInfraReason(t *testing.T) {
	tests := []struct {
		res  agent.Result
		want string
	}{
		{agent.Result{TimedOut: true, ErrorSubtype: "timeout"}, "timed out"},
		{agent.Result{IsError: true, ErrorSubtype: "error_max_turns"}, "ran out of turns"},
		{agent.Result{ResultText: "Overloaded"}, "rate limited or overloaded"},
		{agent.Result{IsError: true, ErrorSubtype: "no_result"}, "session error (no_result)"},
		{agent.Result{ExitCode: 3}, "the agent exited with code 3"},
	}
	for _, tc := range tests {
		if got := InfraReason(&tc.res); got != tc.want {
			t.Errorf("InfraReason(%+v) = %q, want %q", tc.res, got, tc.want)
		}
	}
}

func TestRetryExhaustionAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, retries := range []int{0, 1, 3} {
			policy := RetryPolicy{Retries: retries, Delay: 17 * time.Millisecond, WithFallback: fallback}
			for attempt := 1; attempt <= retries+2; attempt++ {
				for _, retryable := range []bool{false, true} {
					got := policy.Decide(attempt, retryable)
					want := retryable && attempt <= retries
					if got.Retry != want {
						t.Fatalf("%+v attempt=%d retryable=%v: %+v", policy, attempt, retryable, got)
					}
					if want && (got.Delay != policy.Delay || got.UseFallback != fallback) {
						t.Fatalf("retry settings: %+v", got)
					}
					if !want && got != (RetryDecision{}) {
						t.Fatalf("exhausted decision: %+v", got)
					}
				}
			}
		}
	}
	for _, tc := range []struct {
		fallback string
		use      bool
		want     string
		selected bool
	}{
		{"cheap", true, "cheap", true}, {"cheap", false, "primary", false}, {"", true, "primary", false},
	} {
		got, selected := SelectModel("primary", tc.fallback, tc.use)
		if got != tc.want || selected != tc.selected {
			t.Fatalf("model=%q fallback=%v", got, selected)
		}
	}
	for _, phrase := range []string{"RATE LIMIT", "Abuse Detection", "Secondary rate", "Overloaded", "Usage limit", "Session limit"} {
		if !RateLimitedText(phrase) {
			t.Errorf("missed %q", phrase)
		}
	}
	if RateLimitedText("ordinary response") {
		t.Fatal("ordinary response classified as limit")
	}
}
