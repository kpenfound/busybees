// Package feedback holds the drafts a role writes when it hits an error the
// factory itself caused: a tool that behaved unexpectedly, a prompt that
// contradicted the code, an orchestrator mislabel it had to work around.
// A draft is what the report_factory_error tool records, and it describes a
// problem with busybees, not with the product the factory is building.
//
// Drafts are JSON files under <state_dir>/feedback/<id>.json, one per draft,
// the same shape as the mailbox (package mail). Nothing here reads GitHub or
// files anything: a draft's title and detail are scrubbed by the role that
// wrote it, and the queue only keeps them. Filing them upstream belongs to
// the consumer of the queue, the scheduler's drainFeedbackQueue, which calls
// Remove once a draft has been filed or added to the issue it duplicates.
package feedback

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Draft is one factory-error report, waiting to be filed.
type Draft struct {
	ID string `json:"id"`
	// Role is the role that reported the error; SessionDir the session it
	// happened in, for whoever reads the transcript.
	Role       string `json:"role"`
	SessionDir string `json:"session_dir,omitempty"`
	// Title is the one-line summary; Detail the error and enough context to
	// act on it. Both are already scrubbed of repository names, tokens,
	// paths and people by the role that wrote them.
	Title     string    `json:"title"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// Queue is the draft queue rooted at a directory.
type Queue struct{ root string }

// Open returns a queue rooted at dir (created on demand).
func Open(dir string) *Queue { return &Queue{root: dir} }

// Root returns the queue directory.
func (q *Queue) Root() string { return q.root }

// Add stores a draft and returns it with ID and timestamp filled in.
func (q *Queue) Add(d Draft) (Draft, error) {
	if d.Role == "" {
		return d, errors.New("feedback: reporting role is required")
	}
	if strings.TrimSpace(d.Title) == "" {
		return d, errors.New("feedback: a draft needs a title")
	}
	if strings.TrimSpace(d.Detail) == "" {
		return d, errors.New("feedback: a draft needs detail")
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.ID == "" {
		d.ID = newID(d.CreatedAt)
	}
	if err := os.MkdirAll(q.root, 0o755); err != nil {
		return d, err
	}
	return d, writeDraft(filepath.Join(q.root, d.ID+".json"), d)
}

// List returns every draft, oldest first. A queue that was never written to
// is empty, not an error.
func (q *Queue) List() ([]Draft, error) {
	entries, err := os.ReadDir(q.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Draft
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		d, err := readDraft(filepath.Join(q.root, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Remove deletes a draft by the ID Add returned. A draft that is not there
// is not an error: the queue is a directory, and a consumer that filed a
// draft and crashed before removing it must be able to remove it again.
func (q *Queue) Remove(id string) error {
	if id == "" {
		return errors.New("feedback: a draft id is required")
	}
	// An ID is a file name, not a path: Add builds one and List reads it
	// back from a file it found in the queue directory. Anything else is a
	// corrupt draft, and deleting whatever it points at is not this
	// package's job.
	if strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("feedback: %q is not a draft id", id)
	}
	if err := os.Remove(filepath.Join(q.root, id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeDraft(path string, d Draft) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readDraft(path string) (Draft, error) {
	var d Draft
	data, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("feedback: corrupt draft %s: %w", path, err)
	}
	return d, nil
}

func newID(t time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return t.Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}
