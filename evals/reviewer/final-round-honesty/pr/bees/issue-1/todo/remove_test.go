package todo

import "testing"

func TestRemove(t *testing.T) {
	l := List{{Title: "a"}, {Title: "b"}, {Title: "c"}}
	for _, tc := range []struct {
		name string
		n    int
		want []string
	}{
		{"the first item", 1, []string{"b", "c"}},
		{"an item in the middle", 2, []string{"a", "c"}},
		{"the last item", 3, []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := l.Remove(tc.n)
			if err != nil {
				t.Fatalf("Remove(%d): %v", tc.n, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Remove(%d) = %v, want %v", tc.n, got, tc.want)
			}
			for i, it := range got {
				if it.Title != tc.want[i] {
					t.Errorf("item %d is %q, want %q", i, it.Title, tc.want[i])
				}
			}
		})
	}
}

func TestRemovePastTheEnd(t *testing.T) {
	l := List{{Title: "a"}}
	if _, err := l.Remove(9); err == nil {
		t.Fatal("Remove(9) is not an error")
	}
}

// Remove leaves the list it was called on as it was.
func TestRemoveDoesNotChangeTheList(t *testing.T) {
	l := List{{Title: "a"}, {Title: "b"}}
	if _, err := l.Remove(1); err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || l[0].Title != "a" || l[1].Title != "b" {
		t.Fatalf("the list changed: %v", l)
	}
}
