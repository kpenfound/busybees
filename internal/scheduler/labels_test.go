package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

// createdLabels lists the names passed to `gh label create`, in order.
func createdLabels(h *harness) []string {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	var out []string
	for _, c := range h.gh.calls {
		if len(c) >= 3 && c[0] == "label" && c[1] == "create" {
			out = append(out, c[2])
		}
	}
	return out
}

// dropLabel removes a label from the repository, as if it had been
// initialised by a build that did not know about it yet.
func dropLabel(h *harness, name string) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	h.gh.labels = slices.DeleteFunc(h.gh.labels, func(l string) bool { return l == name })
}

func TestMissingLabelsAreCreatedAtStart(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	dropLabel(h, "bees:size/m")
	dropLabel(h, "bees:review")
	// bees:priority is a person's lever; a repository initialised before it
	// existed gets it on the next start like any other label. bees:planning
	// and bees:planned are the same shape and were added later still — a
	// label the code applies but the repository does not have makes every
	// --add-label using it fail, so All() and this path are what stop that.
	dropLabel(h, "bees:priority")
	dropLabel(h, "bees:planning")
	dropLabel(h, "bees:planned")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := createdLabels(h)
	slices.Sort(got)
	if want := []string{"bees:planned", "bees:planning", "bees:priority", "bees:review", "bees:size/m"}; !slices.Equal(got, want) {
		t.Fatalf("labels created: %v, want %v", got, want)
	}
	if !strings.Contains(h.logs.String(), "created missing label") {
		t.Fatalf("no log line about the created labels:\n%s", h.logs.String())
	}
}

func TestExistingLabelIsMatchedCaseInsensitively(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	dropLabel(h, "bees:size/m")
	h.gh.labels = append(h.gh.labels, "Bees:Size/M")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := createdLabels(h); len(got) != 0 {
		t.Fatalf("every label exists, none must be created: %v", got)
	}
}

func TestLabelFailuresDoNotStopThePass(t *testing.T) {
	for _, c := range []struct {
		name, command, want string
	}{
		{"list fails", "label list", "could not ensure labels"},
		{"create fails", "label create", "could not ensure labels"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, noRolesTOML)
			dropLabel(h, "bees:size/m")
			h.gh.errFor[c.command] = errors.New("gh: no write access to labels")
			h.gh.issues[1] = &github.Issue{Number: 1, Title: "Work", State: "OPEN",
				Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			if err := h.sched.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(h.logs.String(), c.want) {
				t.Fatalf("no warning about the labels:\n%s", h.logs.String())
			}
			if n := h.gh.callCount("issue list"); n == 0 {
				t.Fatalf("the pass did not poll GitHub:\n%s", h.logs.String())
			}
			st, err := h.store.LoadStatus()
			if err != nil {
				t.Fatal(err)
			}
			if st.Queues["ready"] != 1 {
				t.Fatalf("queues after the pass: %v", st.Queues)
			}
		})
	}
}

// A single cause (a missing label, an expired token) fails the same edit for
// every issue in a queue. The warning names a few of them; the error keeps
// all of them.
func TestReconcileErrorsAreCappedInTheLog(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	for n := 1; n <= 10; n++ {
		h.gh.issues[n] = &github.Issue{Number: n, Title: fmt.Sprintf("Issue %d", n), State: "OPEN",
			Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}, CreatedAt: time.Now()}
	}
	h.gh.errFor["issue edit"] = errors.New("'bees:size/m' boom")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var warn string
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, "msg=reconcile") {
			if warn != "" {
				t.Fatalf("more than one reconcile warning:\n%s", h.logs.String())
			}
			warn = line
		}
	}
	if warn == "" {
		t.Fatalf("no reconcile warning:\n%s", h.logs.String())
	}
	if n := strings.Count(warn, "boom"); n != maxLoggedErrs {
		t.Fatalf("warning names %d errors, want %d: %s", n, maxLoggedErrs, warn)
	}
	if !strings.Contains(warn, "+7 more") {
		t.Fatalf("warning does not count the rest: %s", warn)
	}

	// The error itself still carries every failure.
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = h.sched.reconcile(ctx, snap)
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("reconcile error is not joined: %v", err)
	}
	if got := len(joined.Unwrap()); got != 10 {
		t.Fatalf("reconcile returned %d errors, want 10", got)
	}
}

func TestCapErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"single", errors.New("one"), "one"},
		{"under the limit", errors.Join(errors.New("one"), errors.New("two")), "one\ntwo"},
		{"over the limit", errors.Join(errors.New("one"), errors.New("two"), errors.New("three"), errors.New("four"), errors.New("five")),
			"one; two; three; +2 more"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := capErrors(c.err); got != c.want {
				t.Fatalf("capErrors = %q, want %q", got, c.want)
			}
		})
	}
}
