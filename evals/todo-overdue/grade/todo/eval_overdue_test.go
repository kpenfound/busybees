package todo_test

import (
	"strings"
	"testing"
	"time"

	"example.com/todo/todo"
)

func TestEvalOverdue(t *testing.T) {
	l, err := todo.Read(strings.NewReader(`call the plumber due:2026-03-01
buy milk
x file taxes due:2026-02-01
(A) pay rent due:2026-03-09
water plants due:2026-03-10
book dentist due:2026-04-01
(C) renew passport due:2025-12-31
`))
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range l.Overdue(time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)) {
		titles = append(titles, it.Title)
	}
	want := "call the plumber, pay rent, renew passport"
	if got := strings.Join(titles, ", "); got != want {
		t.Fatalf("Overdue on 2026-03-10 23:30:\n got %s\nwant %s", got, want)
	}
	if len(l) != 7 || !l[2].Done || l[0].Title != "call the plumber" {
		t.Fatalf("Overdue changed the list: %+v", l)
	}
	if got := todo.List(nil).Overdue(time.Now()); len(got) != 0 {
		t.Fatalf("an empty list: %+v", got)
	}
}
