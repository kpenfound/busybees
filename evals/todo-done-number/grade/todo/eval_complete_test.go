package todo_test

import (
	"testing"

	"example.com/todo/todo"
)

func TestEvalCompleteNumbersFromOne(t *testing.T) {
	l := todo.List{{Title: "a"}, {Title: "b"}, {Title: "c"}}
	if err := l.Complete(1); err != nil {
		t.Fatalf("Complete(1): %v", err)
	}
	if !l[0].Done || l[1].Done || l[2].Done {
		t.Fatalf("Complete(1) marked %+v", l)
	}
	if err := l.Complete(3); err != nil {
		t.Fatalf("Complete(3) on three items: %v", err)
	}
	if !l[2].Done || l[1].Done {
		t.Fatalf("Complete(3) marked %+v", l)
	}
	for _, n := range []int{0, -1, 4} {
		before := append(todo.List(nil), l...)
		if err := l.Complete(n); err == nil {
			t.Errorf("Complete(%d) on three items succeeded", n)
		}
		for i := range l {
			if l[i] != before[i] {
				t.Errorf("Complete(%d) changed item %d", n, i+1)
			}
		}
	}
}
