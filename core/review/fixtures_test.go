package review

import "testing"

type testRef struct{ Key, Scope, Link string }

func (r testRef) String() string      { return r.Key }
func (r testRef) URL() string         { return r.Link }
func (r testRef) ReviewScope() string { return r.Scope }

const testRepo = "component"
const SourceDiff = "diff"
const SourceStyleFiles = "style_files"

func onlyAngle(t *testing.T, angle string) *Settings {
	t.Helper()
	s := &Settings{Angles: map[string]bool{}}
	for _, a := range BuiltinAngles {
		s.Angles[a] = a == angle
	}
	return s
}
