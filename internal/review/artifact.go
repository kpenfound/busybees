package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The review artifact. Every review is a directory that holds what the
// review came to and what reopens it, so a review can be triaged after the
// sessions have ended, on another day, and an angle asked a question about
// what it found. The directories live under the storage path of the global
// configuration (Config.ResolvedStoragePath), one per review:
//
//	<storage>/<owner>/<name>/<number>/<started>/   ArtifactDir: one review of
//	                                               one pull request, started
//	                                               at <started> (UTC,
//	                                               20060102-150405)
//	  brief.json                                   the brief (brief.go): what
//	                                               the distiller made of the
//	                                               context, and its session id
//	  angles/<angle>.json                          one file per angle that ran
//	                                               (angles.go): its session
//	                                               id, the directory it ran
//	                                               in and its raw answer,
//	                                               enough to resume it
//	  scratch/                                     where the angles ran when
//	                                               there was no checkout
//	  findings.json                                the judge's list
//	                                               (findings.go, judge.go)
//	  triage.json                                  what triage decided about
//	                                               each finding, empty until
//	                                               it has
//
// The files are written one at a time as the review goes, by WriteBrief,
// Angles.Run, WriteFindings and WriteTriage, so a review that stopped after
// the angles has a brief and angle runs and nothing else; ReadArtifact reads
// whichever are there. LatestArtifactDir finds the newest review of a pull
// request, which is the one a resumed review reopens.

// TriageFile is the name the triage state is written under inside a
// review's artifact directory.
const TriageFile = "triage.json"

// artifactStamp is the layout of the directory that names when a review
// started: sortable, and safe on every filesystem.
const artifactStamp = "20060102-150405"

// Artifact is one review's directory read into memory: the parts of it
// that are there, and nil for the ones the review has not reached.
type Artifact struct {
	// Dir is the directory.
	Dir string
	// Brief is the brief, which every artifact has: a directory without one
	// is not a review.
	Brief *Brief
	// Runs are the angle runs, in BuiltinAngles order, and none for a
	// review whose angles have not run.
	Runs []AngleRun
	// Findings is the judge's list, and nil for a review not judged yet.
	Findings *Findings
	// Triage is what triage decided, and nil for a review not triaged yet.
	Triage *Triage
}

// Triage is the triage state of a review: what a person, or in factory
// mode an agent, decided about its findings. It is written empty when the
// findings are, so every judged review has one, and the triage queue fills
// it in.
type Triage struct {
	// Decisions are the decisions taken, in the order they were taken.
	Decisions []Decision `json:"decisions"`
}

// Decision is what triage decided about one finding: the action taken on
// it, and what went with the action. The actions are the triage queue's
// (select, dismiss, defer, ask), and what each records here is its own.
type Decision struct {
	// Finding is the id of the finding (Finding.ID).
	Finding string `json:"finding"`
	// Action is the action taken on it.
	Action string `json:"action"`
	// Reason is why, for an action that records one.
	Reason string `json:"reason,omitempty"`
	// Comment is the text the finding is posted as, when triage edited it.
	Comment string `json:"comment,omitempty"`
}

// ArtifactDir is the directory of a review of ref started at started,
// under storage, the global configuration's resolved storage path. Nothing
// is created.
func ArtifactDir(storage string, ref Ref, started time.Time) string {
	return filepath.Join(pullRequestDir(storage, ref), started.UTC().Format(artifactStamp))
}

// LatestArtifactDir is the directory of the newest review of ref under
// storage, and an error when there has been none.
func LatestArtifactDir(storage string, ref Ref) (string, error) {
	dir := pullRequestDir(storage, ref)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no review of %s under %s", ref, storage)
	}
	// ReadDir lists by name, and the names are timestamps.
	return filepath.Join(dir, names[len(names)-1]), nil
}

// pullRequestDir is the directory the reviews of one pull request are
// under: the repository's owner and name, then the number.
func pullRequestDir(storage string, ref Ref) string {
	return filepath.Join(storage, filepath.FromSlash(ref.Repo), strconv.Itoa(ref.Number))
}

// Write writes every part of the artifact that is there into its
// directory, creating it when it is not. A nil brief is an error: the
// directory would not read back as a review. Runs that are nil are not
// written, so a review's earlier runs stay.
func (a *Artifact) Write() error {
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
func ReadArtifact(dir string) (*Artifact, error) {
	brief, err := ReadBrief(dir)
	if err != nil {
		return nil, fmt.Errorf("%s is not a review: %w", dir, err)
	}
	a := &Artifact{Dir: dir, Brief: brief}
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
