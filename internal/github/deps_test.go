package github

import (
	"fmt"
	"testing"
)

func TestBlockers(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want []int
	}{
		{"plain line", "Blocked by #37\n\nRest of the body.", []int{37}},
		{"banner", "> **Blocked by #37.** The size labels do not exist until #37 lands.", []int{37}},
		{"list", "depends on: #3, #4 and #5", []int{3, 4, 5}},
		{"mixed case", "BLOCKED BY #12 and DePeNdS On #13", []int{12, 13}},
		{"emphasis before colon", "**Depends on**: #3", []int{3}},
		{"mid sentence", "This one is blocked by #9 until the parser lands.", []int{9}},
		{"comma and", "Blocked by #1, #2, and #3", []int{1, 2, 3}},
		{"spaces", "blocked by #7 #8", []int{7, 8}},
		{"prose without a number", "blocked by the missing tests", nil},
		{"duplicates dropped", "Blocked by #4, #4\n\nAlso depends on #4.", []int{4}},
		{"no phrase", "Just a body mentioning #5.", nil},
		{"newline is not a separator", "Blocked by #3\n#4 is unrelated", []int{3}},
		{"empty", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Blockers(c.body)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("Blockers(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}
