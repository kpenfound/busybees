package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// IssueFile is the file, inside the session directory, naming the issue the
// session worked on. The scheduler writes it before the session starts, and
// the retention sweep reads it to find every session of an issue that has
// closed: a directory's name carries the issue for some sessions only.
const IssueFile = "issue"

// WriteIssue records the issue a session works on in its directory.
func WriteIssue(dir string, number int) error {
	if dir == "" || number <= 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, IssueFile), []byte(strconv.Itoa(number)+"\n"), 0o644)
}

// ReadIssue returns the issue recorded in dir, or 0 when there is none: a
// session of no issue, or one started before the file existed.
func ReadIssue(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, IssueFile))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
