package todo

import (
	"strings"
	"testing"
)

const sample = `x (A) file the tax return due:2026-04-15
(B) buy milk

call the plumber due:2026-03-01
`

func TestReadWrite(t *testing.T) {
	l, err := Read(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 3 {
		t.Fatalf("read %d items", len(l))
	}
	var b strings.Builder
	if err := l.Write(&b); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(sample, "\n\n", "\n", 1); b.String() != want {
		t.Fatalf("wrote:\n%s", b.String())
	}
	if _, err := Read(strings.NewReader("ok\n(C)\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("a bad line: %v", err)
	}
}

func TestPending(t *testing.T) {
	l, _ := Read(strings.NewReader(sample))
	p := l.Pending()
	if len(p) != 2 || p[0].Title != "buy milk" || p[1].Title != "call the plumber" {
		t.Fatalf("pending: %v", p)
	}
}

func TestByPriorityRanksLettersInOrder(t *testing.T) {
	l, _ := Read(strings.NewReader("(C) c\n(A) a\n(B) b1\n(B) b2\n"))
	var titles []string
	for _, it := range l.ByPriority() {
		titles = append(titles, it.Title)
	}
	if got := strings.Join(titles, " "); got != "a b1 b2 c" {
		t.Fatalf("by priority: %s", got)
	}
}
