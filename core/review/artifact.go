package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// TriageFile is the name the triage state is written under inside a
// review's artifact directory.
const TriageFile = "triage.json"

// Artifact is one review's directory read into memory: the parts of it
// that are there, and nil for the ones the review has not reached.
type Artifact[R Reference] struct {
	// Dir is the directory.
	Dir string
	// Brief is the brief, which every artifact has: a directory without one
	// is not a review.
	Brief *Brief[R]
	// Runs are the angle runs, in BuiltinAngles order, and none for a
	// review whose angles have not run. They are the angles Brief.Size
	// called for that the project enabled (anglesFor), each one run whether
	// it finished or failed: an angle with no run is one the size did not
	// call for, or one the project turned off.
	Runs []AngleRun
	// Findings is the judge's list, and nil for a review not judged yet.
	Findings *Findings
	// Triage is what triage decided, and nil for a review not triaged yet.
	Triage *Triage
}

// Triage is the triage state of a review: what a person, or in factory
// mode an agent, decided about its findings. The triage queue (triage.go)
// writes it with every decision, and reads a judged review that has none as
// one never triaged.
type Triage struct {
	// Decisions are the decisions taken, in the order they were taken.
	Decisions []Decision `json:"decisions"`
}

// Decision is what triage decided about one finding: the action taken on
// it, and what went with the action. The actions are the triage queue's
// (triage.go: ActionSelect, ActionDismiss, ActionDefer, ActionAsk), and what
// each records here is its own: a dismissal its reason, a selection the
// comment text when it was edited, an ask its question, the answer and the
// findings the answer added.
type Decision struct {
	// Finding is the id of the finding (Finding.ID).
	Finding string `json:"finding"`
	// Action is the action taken on it.
	Action string `json:"action"`
	// Reason is why, for an action that records one.
	Reason string `json:"reason,omitempty"`
	// Comment is the text the finding is posted as, when triage edited it.
	Comment string `json:"comment,omitempty"`
	// Question and Answer are what an ask asked the angle and what the
	// angle answered, and Added the ids of the findings the answer turned
	// up that the review did not have.
	Question string   `json:"question,omitempty"`
	Answer   string   `json:"answer,omitempty"`
	Added    []string `json:"added,omitempty"`
}

// Write writes every part of the artifact that is there into its
// directory, creating it when it is not. A nil brief is an error: the
// directory would not read back as a review. Runs that are nil are not
// written, so a review's earlier runs stay.
func (a *Artifact[R]) Write() error {
	if a.Brief == nil {
		return errors.New("a review artifact has a brief, and this one has none")
	}
	if err := WriteBrief(a.Dir, a.Brief); err != nil {
		return err
	}
	for i := range a.Runs {
		if err := WriteAngleRun(a.Dir, &a.Runs[i]); err != nil {
			return err
		}
	}
	if a.Findings != nil {
		if err := WriteFindings(a.Dir, a.Findings); err != nil {
			return err
		}
	}
	if a.Triage != nil {
		if err := WriteTriage(a.Dir, a.Triage); err != nil {
			return err
		}
	}
	return nil
}

// ReadArtifact reads a review's directory back: the brief, which must be
// there, and the angle runs, findings and triage state that are. A file
// that is there and does not read is an error naming it.
func ReadArtifact[R Reference](dir string) (*Artifact[R], error) {
	brief, err := ReadBrief[R](dir)
	if err != nil {
		return nil, fmt.Errorf("%s is not a review: %w", dir, err)
	}
	a := &Artifact[R]{Dir: dir, Brief: brief}
	if a.Runs, err = ReadAngleRuns(dir); err != nil {
		return nil, err
	}
	if a.Findings, err = ReadFindings(dir); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if a.Triage, err = ReadTriage(dir); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return a, nil
}

// WriteTriage writes the triage state into a review's artifact directory,
// creating it when it is not there. A state with no decisions is written
// with an empty list, not a null.
func WriteTriage(dir string, t *Triage) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if t.Decisions == nil {
		t.Decisions = []Decision{}
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, TriageFile), append(data, '\n'), 0o644)
}

// ReadTriage reads the triage state back out of a review's artifact
// directory.
func ReadTriage(dir string) (*Triage, error) {
	path := filepath.Join(dir, TriageFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Triage
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if t.Decisions == nil {
		t.Decisions = []Decision{}
	}
	return &t, nil
}
