package todo

import "strings"

// Search is the items whose title holds q, ignoring case, in list order. An
// empty query matches nothing.
func (l List) Search(q string) List {
	if q == "" {
		return nil
	}
	q = strings.ToLower(q)
	var out List
	for _, it := range l {
		if strings.Contains(strings.ToLower(it.Title), q) {
			out = append(out, it)
		}
	}
	return out
}
