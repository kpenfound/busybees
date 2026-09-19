package ops

import (
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/agent"
)

// FailureKind classifies why a session did not produce a usable result.
type FailureKind int

const (
	// FailureNone is reserved for callers that have not classified a session;
	// ClassifyFailure returns FailureInfra or FailureBehavioural.
	FailureNone FailureKind = iota
	// FailureInfra is a failure of the machinery around the model — a
	// timeout, an API error, exhausted turns, a crashed agent process.
	// Retrying it later is likely to work.
	FailureInfra
	// FailureBehavioural is the session itself: it ran and reported any
	// outcome, or chose not to report. Retrying repeats the same decision.
	FailureBehavioural
)

func (k FailureKind) String() string {
	switch k {
	case FailureNone:
		return "none"
	case FailureInfra:
		return "infrastructure"
	case FailureBehavioural:
		return "behavioural"
	}
	return "unknown"
}

// ClassifyFailure decides whether a finished session is worth retrying.
func ClassifyFailure(res *agent.Result) FailureKind {
	switch {
	case res.HasOutcome && res.Outcome.Status != "":
		// The session ran and said what happened; retrying changes nothing.
		return FailureBehavioural
	case res.TimedOut:
		return FailureInfra
	case res.IsError, res.ExitCode != 0:
		return FailureInfra
	case RateLimitedText(res.ResultText):
		return FailureInfra
	default:
		// Clean exit, no error, no outcome: the model chose not to report.
		return FailureBehavioural
	}
}

// InfraReason names an infrastructure failure for logs and escalations.
func InfraReason(res *agent.Result) string {
	switch {
	case res.TimedOut:
		return "timed out"
	case res.ErrorSubtype == "error_max_turns":
		return "ran out of turns"
	case RateLimitedText(res.ResultText):
		return "rate limited or overloaded"
	case res.ErrorSubtype != "":
		return "session error (" + res.ErrorSubtype + ")"
	case res.ExitCode != 0:
		return fmt.Sprintf("the agent exited with code %d", res.ExitCode)
	default:
		return "unknown"
	}
}

// RateLimitedText recognizes capacity and transient service-limit messages.
func RateLimitedText(msg string) bool {
	msg = strings.ToLower(msg)
	for _, p := range []string{"rate limit", "abuse detection", "secondary rate", "overloaded", "usage limit", "session limit"} {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// RetryPolicy counts retries after the initial attempt.
type RetryPolicy struct {
	Retries      int
	Delay        time.Duration
	WithFallback bool
}

// RetryDecision reports whether and when the next attempt should run.
type RetryDecision struct {
	Retry       bool
	Delay       time.Duration
	UseFallback bool
}

// Decide evaluates a completed, one-based attempt. Callers may also pass
// retryable=true for a budget crossing eligible for another attempt.
func (p RetryPolicy) Decide(attempt int, retryable bool) RetryDecision {
	if !retryable || attempt > p.Retries {
		return RetryDecision{}
	}
	return RetryDecision{Retry: true, Delay: p.Delay, UseFallback: p.WithFallback}
}

// SelectProfile walks n steps down a profile's fallback chain, the way a
// caller retrying a session that had no capacity moves on: the first retry
// runs the profile's fallback, the second that one's own, and a chain that
// ends sooner stays on its last profile. It reports whether the profile
// returned is a fallback; n <= 0 is the profile itself.
func SelectProfile(p agent.Profile, n int) (agent.Profile, bool) {
	moved := false
	for ; n > 0 && p.Fallback != nil; n-- {
		p, moved = *p.Fallback, true
	}
	return p, moved
}
