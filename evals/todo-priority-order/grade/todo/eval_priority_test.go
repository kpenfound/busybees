package todo_test

import (
	"strings"
	"testing"

	"example.com/todo/todo"
)

func evalTitles(l todo.List) string {
	var titles []string
	for _, it := range l {
		titles = append(titles, it.Title)
	}
	return strings.Join(titles, ", ")
}

func TestEvalByPriorityPutsNoPriorityLast(t *testing.T) {
	l, err := todo.Read(strings.NewReader("buy milk\n(B) pay rent\n(Z) sort socks\nwater plants\n(A) file taxes\n(B) book dentist\n"))
	if err != nil {
		t.Fatal(err)
	}
	before := evalTitles(l)
	want := "file taxes, pay rent, book dentist, sort socks, buy milk, water plants"
	if got := evalTitles(l.ByPriority()); got != want {
		t.Fatalf("ByPriority:\n got %s\nwant %s", got, want)
	}
	if evalTitles(l) != before {
		t.Fatalf("ByPriority reordered the list it was called on: %s", evalTitles(l))
	}
}
