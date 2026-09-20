package todo

import "testing"

func TestCountsMixedList(t *testing.T) {
	l := List{{Title: "a"}, {Title: "b", Done: true}, {Title: "c"}}
	p, d := l.Counts()
	if p != 2 {
		t.Errorf("pending = %d, want 2", p)
	}
	if d != 1 {
		t.Errorf("done = %d, want 1", d)
	}
}

func TestCountsEmptyList(t *testing.T) {
	var l List
	p, d := l.Counts()
	if p != 0 || d != 0 {
		t.Errorf("Counts() = %d, %d, want 0, 0", p, d)
	}
}

func TestCountsAllDone(t *testing.T) {
	l := List{{Title: "a", Done: true}, {Title: "b", Done: true}}
	p, d := l.Counts()
	if p != 0 || d != 2 {
		t.Errorf("Counts() = %d, %d, want 0, 2", p, d)
	}
}
