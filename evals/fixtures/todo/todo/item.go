// Package todo reads and writes a to-do list kept in a plain text file, one
// item per line.
package todo

import (
	"fmt"
	"strings"
	"time"
)

// DateLayout is how a due date is written: due:2026-04-15.
const DateLayout = "2006-01-02"

// Item is one line of a to-do list.
type Item struct {
	Title string
	Done  bool
	// Priority is 'A' (the highest) to 'Z', or 0 for none.
	Priority byte
	// Due is the due date, or the zero time for none.
	Due time.Time
}

// Parse reads one line of a to-do list.
func Parse(line string) (Item, error) {
	var it Item
	rest := strings.TrimSpace(line)
	if rest == "" {
		return it, fmt.Errorf("empty line")
	}
	if strings.HasPrefix(rest, "x ") {
		it.Done = true
		rest = strings.TrimSpace(rest[2:])
	}
	if len(rest) >= 3 && rest[0] == '(' && rest[2] == ')' && rest[1] >= 'A' && rest[1] <= 'Z' {
		it.Priority = rest[1]
		rest = strings.TrimSpace(rest[3:])
	}
	var words []string
	for _, w := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(w, "due:"); ok {
			due, err := time.Parse(DateLayout, v)
			if err != nil {
				return it, fmt.Errorf("due date %q: %w", v, err)
			}
			it.Due = due
			continue
		}
		words = append(words, w)
	}
	it.Title = strings.Join(words, " ")
	if it.Title == "" {
		return it, fmt.Errorf("%q has no title", line)
	}
	return it, nil
}

// String writes the item as one line of a to-do list.
func (it Item) String() string {
	var b strings.Builder
	if it.Done {
		b.WriteString("x ")
	}
	if it.Priority != 0 {
		fmt.Fprintf(&b, "(%c) ", it.Priority)
	}
	b.WriteString(it.Title)
	if !it.Due.IsZero() {
		b.WriteString(" due:" + it.Due.Format(DateLayout))
	}
	return b.String()
}
