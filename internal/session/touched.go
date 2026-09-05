package session

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// TouchedFile is the file, inside the session directory, in which a session
// records every issue it changed on GitHub. The MCP server (`bees mcp serve`)
// appends to it as the session goes, and the scheduler reads it when the
// session ends: the server runs in its own process, so it cannot write into
// the scheduler's cached issue list, and without the file a label a session
// moved stays invisible until the next poll.
const TouchedFile = "touched-issues.txt"

// RecordTouched appends number to dir's touched-issues file, one decimal
// number per line. A session that changes the same issue twice records it
// twice; TouchedIssues collapses the repeats.
//
// Appending, rather than rewriting a list, is what makes concurrent tool
// handlers safe: each call is one short append to a file opened O_APPEND.
func RecordTouched(dir string, number int) error {
	if dir == "" || number <= 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, TouchedFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, number); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// TouchedIssues returns the issues recorded in dir, in the order they were
// first recorded and without repeats. A session that recorded nothing — the
// common case, and every session of a role whose tools change no issue — has
// no file and returns none, which is not an error.
//
// A line that is not a number is skipped rather than failing the read: the
// file is the scheduler's shortcut, so half of it is worth more than none of
// it, and whatever is missed is picked up by the next poll.
func TouchedIssues(dir string) ([]int, error) {
	if dir == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(dir, TouchedFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []int
	seen := map[int]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}
