package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/work"
)

// OutcomeFile is the file a session's outcome is written to inside the
// session directory (by the caller outcome reporter or the `done` MCP tool).
const OutcomeFile = "outcome.json"

// Outcome is the structured result a session reports when it finishes.
type Outcome struct {
	Work   work.Ref `json:"work"`
	Status string   `json:"status"`
	Note   string   `json:"note,omitempty"`
}

// ValidateOutcome checks a caller-supplied status set. Nil accepts any status.
func ValidateOutcome(role, status string, valid []string) error {
	if valid == nil || slices.Contains(valid, status) {
		return nil
	}
	return fmt.Errorf("status %q is not valid for %s (want one of %s)", status, role, strings.Join(valid, ", "))
}

// Report normalizes, validates and writes an outcome using the caller's policy.
func Report(dir, role string, valid []string, o Outcome) (Outcome, error) {
	if dir == "" {
		return o, errors.New("an outcome can only be reported inside a session")
	}
	o.Status = strings.ToLower(strings.TrimSpace(o.Status))
	if err := ValidateOutcome(role, o.Status, valid); err != nil {
		return o, err
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
