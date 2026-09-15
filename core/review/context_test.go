package review

import (
	"reflect"
	"testing"
)

func TestBundleSourcesRetainsFirstOccurrence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		sources []string
		want    []string
	}{
		{name: "empty"},
		{name: "adjacent", sources: []string{"style", "style", "diff"}, want: []string{"style", "diff"}},
		{name: "interleaved", sources: []string{"style", "diff", "style", "custom", "diff"}, want: []string{"style", "diff", "custom"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var bundle Bundle[testRef]
			for _, source := range tt.sources {
				bundle.Items = append(bundle.Items, Item{Source: source})
			}
			if got := bundle.Sources(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("sources = %q, want %q", got, tt.want)
			}
		})
	}
}
