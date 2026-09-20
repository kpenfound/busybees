package todo

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// List is a to-do list, in the order of its file.
type List []Item

// Read reads a list, one item per line. Blank lines are skipped.
func Read(r io.Reader) (List, error) {
	var l List
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		it, err := Parse(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		l = append(l, it)
	}
	return l, sc.Err()
}

// Write writes the list, one item per line.
func (l List) Write(w io.Writer) error {
	for _, it := range l {
		if _, err := fmt.Fprintln(w, it); err != nil {
			return err
		}
	}
	return nil
}

// Pending is the items not done yet, in list order.
func (l List) Pending() List {
	var out List
	for _, it := range l {
		if !it.Done {
			out = append(out, it)
		}
	}
	return out
}

// Complete marks item n done. Items are numbered from 1, the way the todo
// command shows them.
func (l List) Complete(n int) error {
	if n < 0 || n >= len(l) {
		return fmt.Errorf("no item %d", n)
	}
	l[n].Done = true
	return nil
}

// ByPriority is the list sorted by priority, (A) first. Items of the same
// priority keep their list order.
func (l List) ByPriority() List {
	out := append(List(nil), l...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority < out[j].Priority
	})
	return out
}

// Remove takes item n out of the list and returns what is left. Items are
// numbered from 1, the way the todo command shows them.
func (l List) Remove(n int) List {
	return append(l[:n-1], l[n:]...)
}
