package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// archiveStamp is the timestamp format of an archived notes file.
const archiveStamp = "20060102-150405"

// NotesArchiveDir returns the directory archived notes files are moved to.
func (s *Store) NotesArchiveDir() string { return filepath.Join(s.Dir, "notes", "archive") }

// NotesSize returns the size of a role's notes file in bytes, 0 when the file
// does not exist yet.
func (s *Store) NotesSize(role string) (int64, error) {
	fi, err := os.Stat(s.NotesPath(role))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// ArchiveNotes moves a role's notes file to
// notes/archive/<role>-<20060102-150405>.md and leaves a fresh one behind. It
// returns the archive path, or "" when there was nothing to archive: a missing
// notes file is not an error, the file is simply created.
func (s *Store) ArchiveNotes(role string, now time.Time) (string, error) {
	p := s.NotesPath(role)
	_, err := os.Stat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", s.EnsureNotes(role)
	case err != nil:
		return "", err
	}
	dir := s.NotesArchiveDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	archived := filepath.Join(dir, role+"-"+now.Format(archiveStamp)+".md")
	if err := os.Rename(p, archived); err != nil {
		return "", err
	}
	return archived, s.EnsureNotes(role)
}

// notesIndent is the continuation indent of a "- " bullet: every line of a
// note after the first is prefixed with it so the whole note stays inside the
// bullet as markdown.
const notesIndent = "  "

// indentContinuation prefixes every line after the first with notesIndent,
// leaving empty lines empty. A line that is already indented simply nests one
// level deeper; nothing is joined or reflowed.
func indentContinuation(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines[1:] {
		if l == "" {
			continue
		}
		lines[i+1] = notesIndent + l
	}
	return strings.Join(lines, "\n")
}

// AppendNotes appends "\n- <text>\n" to a role's notes, creating the file when
// it does not exist yet. Text spanning several lines keeps its line breaks;
// every line after the first is indented by two spaces so the note reads as
// one bullet.
func (s *Store) AppendNotes(role, text string) error {
	if err := s.EnsureNotes(role); err != nil {
		return err
	}
	f, err := os.OpenFile(s.NotesPath(role), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "\n- %s\n", indentContinuation(text)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
