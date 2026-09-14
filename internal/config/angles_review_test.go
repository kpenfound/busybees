package config_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/review"
)

// config.KnownReviewAngles is a copy of review.BuiltinAngles, since
// internal/review imports internal/config and cannot be imported back. A
// test binary can import both, so the two lists are kept equal here.
func TestKnownReviewAnglesMatchReview(t *testing.T) {
	got, want := slices.Sorted(slices.Values(config.KnownReviewAngles)), slices.Sorted(slices.Values(review.BuiltinAngles))
	if !slices.Equal(got, want) {
		t.Errorf("config.KnownReviewAngles is %v; review.BuiltinAngles is %v", got, want)
	}
}
