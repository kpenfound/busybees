package skills

import (
	"fmt"
	"time"
)

// RefreshPolicy controls when Prepare refreshes an existing clone before
// using it. A clone that does not exist yet is always cloned, whatever the
// policy in force.
type RefreshPolicy struct {
	kind  refreshKind
	every time.Duration
}

type refreshKind int

const (
	refreshNever refreshKind = iota
	refreshAlways
	refreshEveryDuration
)

// RefreshNever never refreshes an existing clone.
var RefreshNever = RefreshPolicy{kind: refreshNever}

// RefreshAlways refreshes an existing clone on every Prepare call.
var RefreshAlways = RefreshPolicy{kind: refreshAlways}

// RefreshEvery returns a policy that refreshes an existing clone only when
// its last refresh is older than d.
func RefreshEvery(d time.Duration) RefreshPolicy {
	return RefreshPolicy{kind: refreshEveryDuration, every: d}
}

// ParseRefresh parses a refresh-policy configuration string: "never",
// "always", or a Go duration such as "1h". Any other string, including the
// empty string, is rejected.
func ParseRefresh(s string) (RefreshPolicy, error) {
	switch s {
	case "never":
		return RefreshNever, nil
	case "always":
		return RefreshAlways, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return RefreshPolicy{}, fmt.Errorf(`skills: invalid refresh policy %q: want "never", "always" or a duration`, s)
	}
	return RefreshEvery(d), nil
}
