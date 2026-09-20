package todo

import "time"

// Overdue is the pending items whose due date has passed, in list order.
func (l List) Overdue(today time.Time) List {
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	var out List
	for _, it := range l {
		if it.Done || it.Due.IsZero() {
			continue
		}
		if !it.Due.After(day) {
			out = append(out, it)
		}
	}
	return out
}
