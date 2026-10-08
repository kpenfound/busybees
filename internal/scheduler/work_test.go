package scheduler

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// opaqueWorkRef is the work.Ref a caller-defined (non-issue) work item carries
// through the scheduler: an opaque key with the tags the MCP caller gave it,
// rather than an issue/PR number.
var opaqueWorkRef = work.Ref{Key: "role-qa", Tags: map[string]string{"caller/subject": "opaque", "branch": "work"}}

// Every subsystem below (events, the live worker map, cost bookkeeping,
// interrupted-session recovery, retention indexing) is already proven for the
// ordinary issue-keyed case by a real dispatch elsewhere (resume_test.go,
// budgets_test.go, interrupted_test.go, retention_test.go). What those tests
// cannot show is whether an opaque key — one an MCP caller supplied, not one
// the scheduler derived from an issue — survives each owner function
// unchanged: driving a caller-defined work item through a full singleton
// session for every one of those subsystems would duplicate that coverage at
// several times the cost. These tests call each owner function directly
// instead, which is a maintained contract of the opaque work.Ref type: the
// key and tags are caller data the scheduler must carry, not reinterpret.

// TestOpaqueWorkKeyCarriesThroughEventsLedgerAndCost: a session-started event,
// a worker's live stage and a ledger entry all have to report the caller's
// own key and tags, not an issue number, and the cost bookkeeping they feed
// has to be addressable by that same key — including reseeding its total
// from the ledger once the bookkeeping record itself has been removed.
func TestOpaqueWorkKeyCarriesThroughEventsLedgerAndCost(t *testing.T) {
	h := newHarness(t, baseTOML)
	ref := opaqueWorkRef
	spec := sessionSpec{role: config.RoleDeveloper, name: "opaque", work: ref}

	ev := sessionEvent(EventSessionStarted, spec)
	if !reflect.DeepEqual(ev.Work, ref) {
		t.Fatalf("session-started event carries %+v, want the opaque work: %+v", ev.Work, ref)
	}

	ch := h.sched.Subscribe()
	w := &state.Worker{Work: ref}
	h.sched.updateWorker(w, "develop", 2)
	if got := <-ch; !reflect.DeepEqual(got.Work, ref) {
		t.Fatalf("stage event carries %+v, want the opaque work: %+v", got.Work, ref)
	}

	h.sched.record(spec, &session.Result{CostUSD: 2, CostKnown: true, NumTurns: 1})
	entries, err := h.store.ReadLedger(time.Time{})
	if err != nil || len(entries) != 1 || !reflect.DeepEqual(entries[0].Work, ref) {
		t.Fatalf("ledger entries: %+v, err %v, want one entry for %+v", entries, err, ref)
	}
	bk, err := h.store.Work(ref)
	if err != nil || bk.Cost != 2 || bk.Sessions != 1 {
		t.Fatalf("cost bookkeeping for the opaque key: %+v, err %v, want cost 2 and 1 session", bk, err)
	}

	if err := h.store.RemoveWork(ref.Key); err != nil {
		t.Fatal(err)
	}
	if cost, count, err := h.sched.workSpend(ref); cost != 2 || count != 1 || err != nil {
		t.Fatalf("workSpend after the bookkeeping record was removed: cost %v count %d err %v, want 2/1/nil", cost, count, err)
	}
}

// TestOpaqueWorkKeyNeverCollidesWithASingletonBudgetKey: budgetKey chooses
// between a work-item key and a role key (role-qa, a singleton budget is
// keyed on the role alone). An opaque work.Ref whose own key happens to read
// like a role name must still be keyed as work, or its spend would be counted
// against every QA session instead of the one caller-defined item it belongs
// to.
func TestOpaqueWorkKeyNeverCollidesWithASingletonBudgetKey(t *testing.T) {
	spec := sessionSpec{role: config.RoleDeveloper, name: "opaque", work: opaqueWorkRef}
	if got, singleton := budgetKey(spec), budgetKey(sessionSpec{role: config.RoleQA}); got == singleton {
		t.Fatalf("opaque work key %+v collided with the QA singleton budget key %+v", got, singleton)
	}
}

// TestOpaqueWorkKeyResumesAnInterruptedSession: the interrupted-session
// mechanism (recordRunningSession, takeInterrupted, holdInterrupted,
// interruptedFor, clearRunningSession) is keyed by work.Ref.Key throughout;
// an opaque key has to flow through all five steps exactly like an issue's
// does, including going to the role that owns it and never twice.
func TestOpaqueWorkKeyResumesAnInterruptedSession(t *testing.T) {
	h := newHarness(t, baseTOML)
	ref := opaqueWorkRef
	spec := sessionSpec{role: config.RoleDeveloper, name: "opaque", work: ref}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, session.TranscriptFile), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteWork(dir, ref); err != nil {
		t.Fatal(err)
	}
	h.sched.recordRunningSession(spec, ref, dir)
	bk, err := h.store.Work(ref)
	if err != nil {
		t.Fatal(err)
	}

	in := h.sched.takeInterrupted(h.sched.log, &bk)
	if in == nil {
		t.Fatal("takeInterrupted lost the running record of the opaque work")
	}
	h.sched.holdInterrupted(ref.Key, in)
	if h.sched.interruptedFor(ref.Key, "reviewer") != nil {
		t.Fatal("the interruption was delivered to a role that does not own this work")
	}
	if got := h.sched.interruptedFor(ref.Key, spec.role); got != in {
		t.Fatal("the owning role could not resume the opaque work's interrupted session")
	}
	if got := h.sched.interruptedFor(ref.Key, spec.role); got != nil {
		t.Fatal("the same interruption was delivered twice")
	}

	h.sched.clearRunningSession(ref)
	if bk, err := h.store.Work(ref); err != nil || bk.Session != nil {
		t.Fatalf("running record after clearRunningSession: %+v, err %v, want no running session", bk, err)
	}
}

// TestRetentionIndexesSessionsByTheirOpaqueWorkKey: indexSessions reads the
// work.Ref a session directory recorded and indexes it under the ref's own
// key, never under the directory's name — a caller-defined key is not an
// issue number or a session name, and retention must find it either way.
func TestRetentionIndexesSessionsByTheirOpaqueWorkKey(t *testing.T) {
	ref := opaqueWorkRef
	sessions := t.TempDir()
	sd := filepath.Join(sessions, "unrelated-name")
	if err := os.Mkdir(sd, 0755); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteWork(sd, ref); err != nil {
		t.Fatal(err)
	}
	idx, err := indexSessions(sessions)
	if err != nil || !reflect.DeepEqual(idx.byWork[ref.Key], []string{sd}) {
		t.Fatalf("retention index: %+v, err %v, want %q indexed under %q", idx, err, sd, ref.Key)
	}
}
