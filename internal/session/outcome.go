package session

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
)

// OutcomeFile is the file a session's outcome is written to inside the
// session directory (by `bees done` or the `done` MCP tool).
const OutcomeFile = "outcome.json"

type Outcome = agent.Outcome

// validOutcomes lists the statuses each role may report. A role that is not
// listed accepts any status.
var validOutcomes = map[string][]string{
	config.RoleProductManager: {"done", "idle", "failed"},
	config.RoleProjectManager: {"done", "idle", "failed"},
	config.RoleDeveloper:      {"pr-opened", "pr-updated", "question", "failed"},
	config.RoleReviewer:       {"approved", "changes-requested", "failed"},
	config.RoleQA:             {"done", "failed"},
}

// ValidOutcomes returns the statuses role may report, or nil for an unknown
// role (which accepts anything).
func ValidOutcomes(role string) []string { return slices.Clone(validOutcomes[role]) }

// ValidateOutcome checks that status is one of role's valid outcomes.
func ValidateOutcome(role, status string) error {
	return agent.ValidateOutcome(role, status, ValidOutcomes(role))
}

// Report validates o for role and writes it to the session directory dir.
// It is the shared implementation of `bees done` and the `done` MCP tool;
// the returned outcome is the one that was written (status normalised).
func Report(dir, role string, o Outcome) (Outcome, error) {
	if dir == "" {
		return o, errors.New("an outcome can only be reported inside a session ($BEES_SESSION_DIR is not set)")
	}
	o.Status = strings.ToLower(strings.TrimSpace(o.Status))
	if err := ValidateOutcome(role, o.Status); err != nil {
		return o, err
	}
	if (o.Status == "pr-opened" || o.Status == "pr-updated") && ghwork.PR(o.Work) == 0 {
		return o, fmt.Errorf("%s requires a pull request number", o.Status)
	}
	return o, WriteOutcome(dir, o)
}

var WriteOutcome = agent.WriteOutcome
var ReadOutcome = agent.ReadOutcome
