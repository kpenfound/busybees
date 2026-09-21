package todo

import (
	"testing"
	"time"
)

func date(s string) time.Time {
	d, err := time.Parse(DateLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestOverdue(t *testing.T) {
	today := date("2026-03-10")
	l := List{
		{Title: "call the plumber", Due: date("2026-03-01")},
		{Title: "buy milk"},
		{Title: "file the tax return", Due: date("2026-04-15")},
		{Title: "renew the passport", Due: date("2026-02-20"), Done: true},
	}
	got := l.Overdue(today)
	if len(got) != 1 || got[0].Title != "call the plumber" {
		t.Fatalf("Overdue(%s) = %v", today.Format(DateLayout), got)
	}
}

func TestOverdueIgnoresTheTimeOfDay(t *testing.T) {
	l := List{{Title: "call the plumber", Due: date("2026-03-01")}}
	if got := l.Overdue(date("2026-03-10").Add(23 * time.Hour)); len(got) != 1 {
		t.Fatalf("Overdue = %v", got)
	}
}
