package text

import "testing"

func TestCount(t *testing.T) {
	cases := []struct {
		n    int
		noun string
		want string
	}{
		{0, "session", "0 sessions"},
		{1, "session", "1 session"},
		{2, "session", "2 sessions"},
		{1, "open issue", "1 open issue"},
		{3, "open issue", "3 open issues"},
		{-1, "warning", "-1 warnings"},
	}
	for _, c := range cases {
		if got := Count(c.n, c.noun); got != c.want {
			t.Errorf("Count(%d, %q) = %q, want %q", c.n, c.noun, got, c.want)
		}
	}
}
