package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/feedback"
	"github.com/kpenfound/busybees/internal/state"
)

// fakeFeedback answers scheduler.report_factory_errors from memory.
type fakeFeedback struct {
	on  bool
	err error
}

func (f fakeFeedback) ReportFactoryErrors(context.Context) (bool, error) { return f.on, f.err }

// drafts reads the queue the harness's server writes to: the one under its
// state dir, opened the way the server opens it.
func (h *harness) drafts() []feedback.Draft {
	h.t.Helper()
	got, err := feedback.Open(state.New(h.env.StateDir).FeedbackDir()).List()
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// With the key on, every role's report lands in the queue as a draft that
// names the role and the session, and the tool says so.
func TestReportFactoryErrorRecordsADraftForEveryRole(t *testing.T) {
	for _, role := range config.Roles {
		h := newHarness(t, role, Deps{Feedback: fakeFeedback{on: true}})
		out := h.call("report_factory_error", map[string]any{
			"title":  "issue_view refused a visible issue",
			"detail": "Calling issue_view on <issue> in the filter answered 'outside the filter'.",
		})
		if !strings.Contains(out, "recorded draft ") {
			t.Errorf("role %q: result %q does not say the draft was recorded", role, out)
		}
		got := h.drafts()
		if len(got) != 1 {
			t.Fatalf("role %q: %d drafts, want 1: %+v", role, len(got), got)
		}
		d := got[0]
		if d.Role != role || d.SessionDir != h.sessionDir || d.Title != "issue_view refused a visible issue" || !strings.HasPrefix(d.Detail, "Calling issue_view") {
			t.Errorf("role %q: draft = %+v", role, d)
		}
		if d.ID == "" || d.CreatedAt.IsZero() || !strings.Contains(out, d.ID) {
			t.Errorf("role %q: draft id %q / time %v; result %q", role, d.ID, d.CreatedAt, out)
		}
	}
}

// With the key off the tool is still there, but it records nothing and says
// so in a plain result, not an error: off is the default, not a failure.
func TestReportFactoryErrorOffRecordsNothingAndSaysSo(t *testing.T) {
	h := newHarness(t, config.RoleDeveloper, Deps{Feedback: fakeFeedback{on: false}})
	res := h.callRaw("report_factory_error", map[string]any{"title": "t", "detail": "d"})
	if res.IsError {
		t.Fatalf("off is an ordinary result, got an error: %s", resultText(res))
	}
	out := resultText(res)
	for _, want := range []string{"not recorded", "scheduler.report_factory_errors", "off"} {
		if !strings.Contains(out, want) {
			t.Errorf("result %q does not say %q", out, want)
		}
	}
	if got := h.drafts(); len(got) != 0 {
		t.Errorf("off, but %d drafts were written: %+v", len(got), got)
	}
}

// Without a backend (bees.toml unreadable) the tool is unavailable, the same
// way the issue and GitHub tools are, and a backend that cannot answer
// passes its error through. Neither writes a draft.
func TestReportFactoryErrorWithoutABackend(t *testing.T) {
	for name, deps := range map[string]Deps{
		"nil":         {},
		"erroring":    {Feedback: fakeFeedback{err: errors.New("bees.toml: no such file")}},
		"empty title": {Feedback: fakeFeedback{on: true}},
	} {
		h := newHarness(t, config.RoleQA, deps)
		args := map[string]any{"title": "t", "detail": "d"}
		if name == "empty title" {
			args["title"] = "  "
		}
		res := h.callRaw("report_factory_error", args)
		if !res.IsError {
			t.Errorf("%s: expected an error, got %q", name, resultText(res))
		}
		if got := h.drafts(); len(got) != 0 {
			t.Errorf("%s: %d drafts written: %+v", name, len(got), got)
		}
	}
}

// The tool is offered whoever the role is, and its description carries the
// scrubbing rule: the description is the guardrail, there is no second pass.
func TestReportFactoryErrorDescriptionCarriesTheScrubbingRule(t *testing.T) {
	h := newHarness(t, "", Deps{})
	for _, tool := range h.tools() {
		if tool.Name != "report_factory_error" {
			continue
		}
		for _, want := range []string{"repository", "path", "token", "person", "Nothing scrubs after you"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("description lacks %q:\n%s", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("no report_factory_error tool")
}
