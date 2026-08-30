package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
)

// OutcomeFile is the file a session's outcome is written to inside the
// session directory (by `bees done` or the `done` MCP tool).
const OutcomeFile = "outcome.json"

// Outcome is the structured result a session reports when it finishes.
type Outcome struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	PR     int    `json:"pr,omitempty"`
	Issue  int    `json:"issue,omitempty"`
}

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
	valid, ok := validOutcomes[role]
	if !ok {
		return nil // unknown role: accept anything
	}
	if slices.Contains(valid, status) {
		return nil
	}
	return fmt.Errorf("status %q is not valid for %s (want one of %s)", status, role, strings.Join(valid, ", "))
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
	if (o.Status == "pr-opened" || o.Status == "pr-updated") && o.PR == 0 {
		return o, fmt.Errorf("%s requires a pull request number", o.Status)
	}
	return o, WriteOutcome(dir, o)
}

// WriteOutcome stores an outcome in dir without validating it.
func WriteOutcome(dir string, o Outcome) error {
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, OutcomeFile), data, 0o644)
}

// ReadOutcome loads the outcome from dir. ok is false when none was written.
func ReadOutcome(dir string) (o Outcome, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, OutcomeFile))
	if err != nil {
		if os.IsNotExist(err) {
			return Outcome{}, false, nil
		}
		return Outcome{}, false, err
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return Outcome{}, false, fmt.Errorf("corrupt %s: %w", OutcomeFile, err)
	}
	return o, true, nil
}
