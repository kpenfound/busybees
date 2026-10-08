package text

import "testing"

func TestCount(t *testing.T) {
	cases := []struct {
		name string
		n    int
		noun string
		want string
	}{
		{"zero is plural", 0, "session", "0 sessions"},
		{"one is singular", 1, "session", "1 session"},
		{"two is plural", 2, "session", "2 sessions"},
		{"singular multi-word noun", 1, "open issue", "1 open issue"},
		{"plural multi-word noun", 3, "open issue", "3 open issues"},
		{"negative count still pluralizes", -1, "warning", "-1 warnings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Count(c.n, c.noun); got != c.want {
				t.Errorf("Count(%d, %q) = %q, want %q", c.n, c.noun, got, c.want)
			}
		})
	}
}
