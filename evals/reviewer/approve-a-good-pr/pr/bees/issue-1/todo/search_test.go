package todo

import "testing"

func TestSearch(t *testing.T) {
	l := List{
		{Title: "buy milk"},
		{Title: "Call the plumber", Done: true},
		{Title: "file the tax return"},
	}
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"case insensitive", "CALL", []string{"Call the plumber"}},
		{"part of a word", "ilk", []string{"buy milk"}},
		{"more than one item", "l", []string{"buy milk", "Call the plumber", "file the tax return"}},
		{"nothing matches", "plumbing", nil},
		{"an empty query", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := l.Search(tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("Search(%q) = %v, want %v", tc.query, got, tc.want)
			}
			for i, it := range got {
				if it.Title != tc.want[i] {
					t.Errorf("item %d is %q, want %q", i, it.Title, tc.want[i])
				}
			}
		})
	}
}

// Search leaves the list it was called on as it was.
func TestSearchDoesNotChangeTheList(t *testing.T) {
	l := List{{Title: "buy milk"}, {Title: "call the plumber"}}
	l.Search("milk")
	if len(l) != 2 || l[0].Title != "buy milk" || l[1].Title != "call the plumber" {
		t.Fatalf("the list changed: %v", l)
	}
}
