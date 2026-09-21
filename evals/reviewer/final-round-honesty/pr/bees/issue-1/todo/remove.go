package todo

import "fmt"

// Remove returns the list without item n. Items are numbered from 1, the
// way the todo command shows them.
func (l List) Remove(n int) (List, error) {
	if n < 0 || n > len(l) {
		return nil, fmt.Errorf("no item %d", n)
	}
	out := append(List(nil), l[:n-1]...)
	return append(out, l[n:]...), nil
}
