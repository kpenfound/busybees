// Package mail implements the local mailbox used for all communication
// between roles. Messages never touch GitHub: they are JSON files under
// <state_dir>/mail/<to-role>/.
//
// Messages are addressed to a role, not to a session. A message may carry an
// issue and/or PR number; the scheduler uses that to deliver it to the
// session that is working on that item (for example a project manager's
// answer is delivered to whichever developer picks the issue back up).
package mail

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

// Message is one mailbox entry.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Issue     int       `json:"issue,omitempty"`
	PR        int       `json:"pr,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// ReadAt is set when the message has been delivered to a session.
	ReadAt *time.Time `json:"read_at,omitempty"`
	// InReplyTo references the message this one answers, when known.
	InReplyTo string `json:"in_reply_to,omitempty"`
}

// Unread reports whether the message has not been delivered yet.
func (m Message) Unread() bool { return m.ReadAt == nil }

// Box is a mailbox rooted at a directory.
type Box struct{ root string }

// Open returns a mailbox rooted at dir (created on demand).
func Open(dir string) *Box { return &Box{root: dir} }

// Root returns the mailbox directory.
func (b *Box) Root() string { return b.root }

// Send stores a message and returns it with ID and timestamp filled in.
func (b *Box) Send(m Message) (Message, error) {
	if m.To == "" {
		return m, errors.New("mail: recipient role is required")
	}
	if m.From == "" {
		return m, errors.New("mail: sender role is required")
	}
	if strings.TrimSpace(m.Body) == "" && strings.TrimSpace(m.Subject) == "" {
		return m, errors.New("mail: message needs a subject or body")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.ID == "" {
		m.ID = newID(m.CreatedAt)
	}
	dir := filepath.Join(b.root, m.To)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return m, err
	}
	return m, writeMessage(filepath.Join(dir, m.ID+".json"), m)
}

// Filter selects messages.
type Filter struct {
	To         string
	From       string
	Issue      int // when > 0, only messages for this issue
	PR         int // when > 0, only messages for this PR
	UnreadOnly bool
	// Unaddressed selects messages with no issue and no PR (broadcasts to the role).
	Unaddressed bool
}

// List returns messages matching f, oldest first.
func (b *Box) List(f Filter) ([]Message, error) {
	var roles []string
	if f.To != "" {
		roles = []string{f.To}
	} else {
		entries, err := os.ReadDir(b.root)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				roles = append(roles, e.Name())
			}
		}
	}
	var out []Message
	for _, role := range roles {
		dir := filepath.Join(b.root, role)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			m, err := readMessage(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			if f.From != "" && m.From != f.From {
				continue
			}
			if f.Issue > 0 && m.Issue != f.Issue {
				continue
			}
			if f.PR > 0 && m.PR != f.PR {
				continue
			}
			if f.Unaddressed && (m.Issue != 0 || m.PR != 0) {
				continue
			}
			if f.UnreadOnly && !m.Unread() {
				continue
			}
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Get returns one message by id (searching every role's box).
func (b *Box) Get(id string) (Message, error) {
	all, err := b.List(Filter{})
	if err != nil {
		return Message{}, err
	}
	for _, m := range all {
		if m.ID == id {
			return m, nil
		}
	}
	return Message{}, fmt.Errorf("mail: message %s not found", id)
}

// MarkRead marks the given messages as delivered.
func (b *Box) MarkRead(msgs ...Message) error {
	now := time.Now().UTC()
	for _, m := range msgs {
		if m.ReadAt != nil {
			continue
		}
		m.ReadAt = &now
		if err := writeMessage(filepath.Join(b.root, m.To, m.ID+".json"), m); err != nil {
			return err
		}
	}
	return nil
}

// Counts returns unread message counts per recipient role.
func (b *Box) Counts() (map[string]int, error) {
	msgs, err := b.List(Filter{UnreadOnly: true})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, m := range msgs {
		counts[m.To]++
	}
	return counts, nil
}

// Format renders a message as Markdown for inclusion in a prompt.
func Format(m Message) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "### %s\n", strings.TrimSpace(m.Subject))
	fmt.Fprintf(&sb, "- id: %s\n- from: %s\n- to: %s\n- sent: %s\n", m.ID, m.From, m.To, m.CreatedAt.Format(time.RFC3339))
	if m.Issue > 0 {
		fmt.Fprintf(&sb, "- issue: #%d\n", m.Issue)
	}
	if m.PR > 0 {
		fmt.Fprintf(&sb, "- pr: #%d\n", m.PR)
	}
	if m.InReplyTo != "" {
		fmt.Fprintf(&sb, "- in reply to: %s\n", m.InReplyTo)
	}
	sb.WriteString("\n")
	sb.WriteString(strings.TrimSpace(m.Body))
	sb.WriteString("\n")
	return sb.String()
}

func writeMessage(path string, m Message) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readMessage(path string) (Message, error) {
	var m Message
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("mail: corrupt message %s: %w", path, err)
	}
	return m, nil
}

func newID(t time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return t.Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}
