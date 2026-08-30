package scheduler

import (
	"testing"

	"github.com/kpenfound/busybees/internal/session"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name string
		res  session.Result
		want failureKind
	}{
		{
			name: "reported an outcome",
			res:  session.Result{HasOutcome: true, Outcome: session.Outcome{Status: OutcomePROpened}},
			want: failureBehavioural,
		},
		{
			name: "reported failure",
			res:  session.Result{HasOutcome: true, Outcome: session.Outcome{Status: OutcomeFailed, Note: "cannot build"}},
			want: failureBehavioural,
		},
		{
			name: "reported an outcome after an error",
			res:  session.Result{HasOutcome: true, Outcome: session.Outcome{Status: OutcomeQuestion}, IsError: true, ErrorSubtype: "error_max_turns"},
			want: failureBehavioural,
		},
		{
			name: "timed out",
			res:  session.Result{TimedOut: true, IsError: true, ExitCode: -1, ErrorSubtype: "timeout"},
			want: failureInfra,
		},
		{
			name: "out of turns",
			res:  session.Result{IsError: true, ErrorSubtype: "error_max_turns"},
			want: failureInfra,
		},
		{
			name: "api error",
			res:  session.Result{IsError: true, ErrorSubtype: "error_during_execution"},
			want: failureInfra,
		},
		{
			name: "claude crashed without a result event",
			res:  session.Result{IsError: true, ExitCode: 1, ErrorSubtype: "no_result", ResultText: "panic"},
			want: failureInfra,
		},
		{
			name: "non-zero exit without an error flag",
			res:  session.Result{ExitCode: 2},
			want: failureInfra,
		},
		{
			name: "rate limited",
			res:  session.Result{ResultText: "API Error: 429 Rate limit exceeded"},
			want: failureInfra,
		},
		{
			name: "overloaded",
			res:  session.Result{ResultText: "Overloaded"},
			want: failureInfra,
		},
		{
			name: "usage limit",
			res:  session.Result{ResultText: "Claude usage limit reached"},
			want: failureInfra,
		},
		{
			name: "clean exit without an outcome",
			res:  session.Result{ResultText: "all done!"},
			want: failureBehavioural,
		},
		{
			name: "empty outcome status",
			res:  session.Result{HasOutcome: true},
			want: failureBehavioural,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(&tc.res); got != tc.want {
				t.Fatalf("classifyFailure = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestInfraReason(t *testing.T) {
	tests := []struct {
		res  session.Result
		want string
	}{
		{session.Result{TimedOut: true, ErrorSubtype: "timeout"}, "timed out"},
		{session.Result{IsError: true, ErrorSubtype: "error_max_turns"}, "ran out of turns"},
		{session.Result{ResultText: "Overloaded"}, "rate limited or overloaded"},
		{session.Result{IsError: true, ErrorSubtype: "no_result"}, "session error (no_result)"},
		{session.Result{ExitCode: 3}, "claude exited with code 3"},
	}
	for _, tc := range tests {
		if got := infraReason(&tc.res); got != tc.want {
			t.Errorf("infraReason(%+v) = %q, want %q", tc.res, got, tc.want)
		}
	}
}
