package todo

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	it, err := Parse("x (A) file the tax return due:2026-04-15")
	if err != nil {
		t.Fatal(err)
	}
	want := Item{Title: "file the tax return", Done: true, Priority: 'A', Due: time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)}
	if it != want {
		t.Fatalf("got %+v, want %+v", it, want)
	}
	for _, bad := range []string{"", "x due:2026-04-15", "due:2026-04-15", "buy milk due:tomorrow"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestStringRoundTrips(t *testing.T) {
	for _, line := range []string{"buy milk", "(B) buy milk", "x call the plumber due:2026-03-01"} {
		it, err := Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		if got := it.String(); got != line {
			t.Errorf("%q came back as %q", line, got)
		}
	}
}
