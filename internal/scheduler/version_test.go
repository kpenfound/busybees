package scheduler

import (
	"strings"
	"testing"
)

// The role prompts are compiled into the binary, so a running factory serves
// the prompts of the build it was started from: the scheduler records that
// build in status.json (and in its startup log line) so a person, and #297's
// staleness check, can tell which one is running.
func TestStatusRecordsTheRunningBuild(t *testing.T) {
	const (
		version  = "dev (b24a0605c2a1 modified)"
		revision = "b24a0605c2a1e9f0d3c4b5a6978869d3d1e2f3a4"
	)
	h := newHarness(t, noRolesTOML)
	h.sched = rebuildWithBuild(t, h, version, revision)
	runPass(t, h)

	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != version {
		t.Errorf("status version: got %q want %q", st.Version, version)
	}
	if st.Revision != revision {
		t.Errorf("status revision: got %q want %q", st.Revision, revision)
	}
	if !strings.Contains(h.logs.String(), "version="+`"`+version+`"`) {
		t.Errorf("the startup line does not name the build:\n%s", h.logs.String())
	}
}

// A scheduler given no build — which is every other test, and any caller that
// does not resolve one — records none and behaves exactly as before.
func TestAnEmptyBuildIsNotRecorded(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	runPass(t, h)

	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "" || st.Revision != "" {
		t.Errorf("got version %q, revision %q; want both empty", st.Version, st.Revision)
	}
}

// rebuildWithBuild returns the harness's scheduler built again from the same
// collaborators, this time with a build in Deps: that is the seam the fields
// arrive through, so a test that set them on the Scheduler directly would not
// exercise it.
func rebuildWithBuild(t *testing.T, h *harness, version, revision string) *Scheduler {
	t.Helper()
	s, err := New(Deps{
		Config:     h.sched.cfg,
		GitHub:     h.sched.gh,
		Mail:       h.sched.mail,
		Runner:     h.sched.runner,
		Workspaces: h.sched.ws,
		Store:      h.sched.store,
		Logger:     h.sched.log,
		Now:        h.sched.now,
		Version:    version,
		Revision:   revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Once = true
	return s
}
