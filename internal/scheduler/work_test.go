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

func TestOpaqueRuntimeIdentity(t *testing.T) {
	h := newHarness(t, baseTOML)
	ref := work.Ref{Key: "role-qa", Tags: map[string]string{"caller/subject": "opaque", "branch": "work"}}
	spec := sessionSpec{role: config.RoleDeveloper, name: "opaque", work: ref}
	ev := sessionEvent(EventSessionStarted, spec)
	if !reflect.DeepEqual(ev.Work, ref) {
		t.Fatalf("event lost tags: %+v", ev)
	}
	ch := h.sched.Subscribe()
	w := &state.Worker{Work: ref}
	h.sched.updateWorker(w, "develop", 2)
	if got := <-ch; !reflect.DeepEqual(got.Work, ref) {
		t.Fatalf("stage lost work: %+v", got)
	}
	h.sched.record(spec, &session.Result{CostUSD: 2, NumTurns: 1})
	entries, err := h.store.ReadLedger(time.Time{})
	if err != nil || len(entries) != 1 || !reflect.DeepEqual(entries[0].Work, ref) {
		t.Fatalf("ledger: %+v %v", entries, err)
	}
	bk, err := h.store.Work(ref)
	if err != nil || bk.Cost != 2 || bk.Sessions != 1 {
		t.Fatalf("cost: %+v %v", bk, err)
	}
	if err := h.store.RemoveWork(ref.Key); err != nil {
		t.Fatal(err)
	}
	if cost, count := h.sched.workSpend(ref); cost != 2 || count != 1 {
		t.Fatalf("opaque ledger seed: %v/%d", cost, count)
	}
	if budgetKey(spec) == budgetKey(sessionSpec{role: config.RoleQA}) {
		t.Fatal("opaque work key collided with singleton budget")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, session.TranscriptFile), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteWork(dir, ref); err != nil {
		t.Fatal(err)
	}
	h.sched.recordRunningSession(spec, ref, dir)
	bk, err = h.store.Work(ref)
	if err != nil {
		t.Fatal(err)
	}
	in := h.sched.takeInterrupted(h.sched.log, &bk)
	if in == nil {
		t.Fatal("opaque work interruption was lost")
	}
	h.sched.holdInterrupted(ref.Key, in)
	if h.sched.interruptedFor(ref.Key, "reviewer") != nil {
		t.Fatal("delivered interruption to wrong role")
	}
	if got := h.sched.interruptedFor(ref.Key, spec.role); got != in {
		t.Fatal("could not resume opaque work")
	}
	if got := h.sched.interruptedFor(ref.Key, spec.role); got != nil {
		t.Fatal("delivered interruption twice")
	}
	h.sched.clearRunningSession(ref)
	bk, err = h.store.Work(ref)
	if err != nil || bk.Session != nil {
		t.Fatalf("running record survived completion: %+v %v", bk, err)
	}
	// Retention indexes keys without interpreting either the key or the name.
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
		t.Fatalf("retention index lost opaque work: %+v %v", idx, err)
	}
}
