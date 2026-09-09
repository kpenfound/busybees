package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// scheduler.report_factory_errors reaches a session through its system
// prompt: on, the developer is told about report_factory_error and how to
// scrub; off (the default), the prompt does not name the tool.
func TestReportFactoryErrorsReachesTheSessionPrompt(t *testing.T) {
	on := strings.Replace(devOnlyTOML, "max_review_rounds = 3\n", "max_review_rounds = 3\nreport_factory_errors = true\n", 1)
	if on == devOnlyTOML {
		t.Fatal("fixture: could not switch report_factory_errors on")
	}
	for toml, want := range map[string]bool{devOnlyTOML: false, on: true} {
		h := newHarnessAt(t, toml, time.Now())
		h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
		seedReady(h, 1, "m", time.Now().Add(-time.Hour))
		if err := h.sched.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		sys := systemPromptOf(t, h, 0)
		if got := strings.Contains(sys, "### Reporting an error the factory caused"); got != want {
			t.Errorf("report_factory_errors=%v: prompt names the tool = %v:\n%s", want, got, sys)
		}
	}
}
