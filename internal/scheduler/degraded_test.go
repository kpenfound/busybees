package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/state"
)

// The label backstop assigns everything the factory creates. With a filter
// assignee configured and an item that already carries the base label, the
// assignment is the only GitHub mutation the backstop makes — so failing
// the `assignees` REST call fails exactly one named operation.
const degradedTOML = baseTOML + `
[filter]
assignee = "kpenfound"
`

// brokenAssign seeds an item the backstop wants to assign and makes the
// assignment fail.
func brokenAssign(h *harness, err error) {
	h.gh.issues[7] = &github.Issue{
		Number: 7, Title: "Filed by a session", State: "OPEN",
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:bug"}, {Name: "bees:triage"}},
		CreatedAt: time.Now(),
	}
	h.gh.errFor["assignees"] = err
}

// summaries returns the summary records written to a JSON log buffer.
func summaries(t *testing.T, buf *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec[logging.SummaryKey] == true {
			out = append(out, rec)
		}
	}
	return out
}

// jsonLog swaps the scheduler's logger for one writing JSON records, so a
// test can look at the attributes of a record rather than at its text.
func jsonLog(h *harness) *syncBuffer {
	buf := &syncBuffer{}
	h.sched.log = logging.New(logging.Options{Format: logging.FormatJSON, Console: buf}).Logger
	return buf
}

func degradedOp(t *testing.T, st state.Status, op string) state.OpFailure {
	t.Helper()
	for _, f := range st.Degraded {
		if f.Op == op {
			return f
		}
	}
	t.Fatalf("no degraded entry for %q: %+v", op, st.Degraded)
	return state.OpFailure{}
}

func TestAFailedMutationReachesStatusJSON(t *testing.T) {
	h := newHarness(t, degradedTOML)
	brokenAssign(h, errors.New("GraphQL: Projects (classic) is being deprecated"))

	h.sched.adoptCreated(context.Background(), time.Now().Add(-time.Hour))
	h.sched.writeStatus()

	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	f := degradedOp(t, st, "assign")
	if f.Count != 1 {
		t.Errorf("count = %d, want 1", f.Count)
	}
	if !strings.Contains(f.LastError, "Projects (classic) is being deprecated") {
		t.Errorf("last error = %q", f.LastError)
	}
	if f.First.IsZero() || f.Last.IsZero() {
		t.Errorf("failure times not filled in: %+v", f)
	}
	if f.Escalated {
		t.Errorf("one failure should not have escalated: %+v", f)
	}
	if len(st.Degraded) != 1 {
		t.Errorf("only the assignment failed, degraded = %+v", st.Degraded)
	}
}

func TestADegradedOperationEscalatesOncePerStreak(t *testing.T) {
	h := newHarness(t, degradedTOML)
	buf := jsonLog(h)
	brokenAssign(h, errors.New("GraphQL: Projects (classic) is being deprecated"))
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)

	// Three consecutive failing passes: one summary, on the third.
	for i := 0; i < degradedEscalateAfter; i++ {
		if got := len(summaries(t, buf)); got != 0 && i < degradedEscalateAfter-1 {
			t.Fatalf("escalated after %d failures", i)
		}
		h.sched.adoptCreated(ctx, since)
	}
	recs := summaries(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 summary record, got %d: %v", len(recs), recs)
	}
	if recs[0]["level"] != "ERROR" {
		t.Errorf("summary level = %v, want ERROR", recs[0]["level"])
	}
	if recs[0]["op"] != "assign" || recs[0]["failures"] != float64(degradedEscalateAfter) {
		t.Errorf("summary attrs: %v", recs[0])
	}
	if msg, _ := recs[0]["msg"].(string); !strings.Contains(msg, "assign") || !strings.Contains(msg, "3 times in a row") {
		t.Errorf("summary message: %q", msg)
	}

	// A fourth failing pass adds none.
	h.sched.adoptCreated(ctx, since)
	if got := len(summaries(t, buf)); got != 1 {
		t.Fatalf("a fourth failure escalated again: %d summaries", got)
	}
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if f := degradedOp(t, st, "assign"); f.Count != 4 || !f.Escalated {
		t.Errorf("after four failures: %+v", f)
	}

	// A success clears the streak: nothing degraded, and the next streak
	// escalates again.
	delete(h.gh.errFor, "assignees")
	h.sched.adoptCreated(ctx, since)
	h.sched.writeStatus()
	if st, err = h.store.LoadStatus(); err != nil {
		t.Fatal(err)
	}
	if len(st.Degraded) != 0 {
		t.Fatalf("a success left a degraded entry: %+v", st.Degraded)
	}
	brokenAssign(h, errors.New("GraphQL: Projects (classic) is being deprecated"))
	for i := 0; i < degradedEscalateAfter; i++ {
		h.sched.adoptCreated(ctx, since)
	}
	if got := len(summaries(t, buf)); got != 2 {
		t.Fatalf("the second streak did not escalate: %d summaries", got)
	}
}

func TestADegradedErrorIsOneLinedAndCapped(t *testing.T) {
	h := newHarness(t, baseTOML)
	buf := jsonLog(h)
	long := "gh: request failed\n" + strings.Repeat("verbose ", 100) + "\nend"

	for i := 0; i < degradedEscalateAfter; i++ {
		h.sched.op("assign", errors.New(long), "label backstop: assign", "number", 7)
	}
	h.sched.writeStatus()

	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	got := degradedOp(t, st, "assign").LastError
	if strings.Contains(got, "\n") {
		t.Errorf("last error is not one line: %q", got)
	}
	// truncate cuts at escalationNoteLimit runes and marks the cut with "…".
	if n := len([]rune(got)); n > escalationNoteLimit+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("last error is %d runes and ends %q, cap is %d", n, got, escalationNoteLimit)
	}
	recs := summaries(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 summary, got %d", len(recs))
	}
	if msg, _ := recs[0]["msg"].(string); strings.Contains(msg, "\n") {
		t.Errorf("summary message is not one line: %q", msg)
	}
}

// op is bookkeeping only: a nil error records nothing and reports no
// failure, so a call site can use it on both paths of an operation.
func TestOpWithoutAnErrorRecordsNothing(t *testing.T) {
	h := newHarness(t, baseTOML)
	if h.sched.op("assign", nil, "label backstop: assign", "number", 7) {
		t.Error("op reported a failure for a nil error")
	}
	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	if len(h.sched.degraded) != 0 {
		t.Fatalf("a success created an entry: %v", h.sched.degraded)
	}
	if got := h.logs.String(); got != "" {
		t.Errorf("a success logged: %q", got)
	}
}
