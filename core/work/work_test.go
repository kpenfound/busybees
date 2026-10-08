package work

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOpaqueKeyFilenames(t *testing.T) {
	seen := map[string]Key{}
	for _, key := range []Key{"task-A", "task-a", "../outside", "a/b", "a:b", "a\\b", "你好 ☃", Key(strings.Repeat("long", 1000))} {
		name := key.Filename()
		if filepath.Base(name) != name || len(name) > 255 || name != key.Filename() {
			t.Fatalf("unsafe or unstable filename for %q: %s", key, name)
		}
		folded := strings.ToLower(name)
		if other, ok := seen[folded]; ok {
			t.Fatalf("%q and %q collide", key, other)
		}
		seen[folded] = key
	}
}

// Clone copies the tags, so mutating the clone's map, or adding a tag to
// it, never reaches the original.
func TestCloneCopiesTagsIndependentlyOfTheOriginal(t *testing.T) {
	ref := Ref{Key: "role-reviewer", Tags: map[string]string{"arbitrary/key": "✓", "empty": ""}}
	clone := ref.Clone()
	clone.Tags["arbitrary/key"] = "changed"
	clone.Tags["new"] = "added"

	if got, want := ref.Tags["arbitrary/key"], "✓"; got != want {
		t.Errorf("original's tag after mutating the clone: %q, want %q", got, want)
	}
	if _, ok := ref.Tags["new"]; ok {
		t.Errorf("original gained the clone's new tag %q", ref.Tags["new"])
	}
	if clone.Key != ref.Key {
		t.Errorf("clone key %q, want %q", clone.Key, ref.Key)
	}
}

// Matches requires every requested tag to be present with exactly the
// requested value, including an explicitly empty one, and tolerates tags
// the reference carries but the caller did not ask about.
func TestMatchesRequiresEveryRequestedTagWithItsExactValue(t *testing.T) {
	ref := Ref{Key: "role-reviewer", Tags: map[string]string{"arbitrary/key": "✓", "empty": ""}}
	matching := map[string]map[string]string{
		"no tags requested":        {},
		"a subset with its value":  {"arbitrary/key": "✓"},
		"every tag with its value": {"arbitrary/key": "✓", "empty": ""},
	}
	for name, tags := range matching {
		if !ref.Matches(tags) {
			t.Errorf("%s: Matches(%v) = false, want true", name, tags)
		}
	}
	notMatching := map[string]map[string]string{
		"a key the reference lacks":             {"missing": ""},
		"the right key, the wrong value":        {"arbitrary/key": "x"},
		"the empty tag given a non-empty value": {"empty": "nonempty"},
	}
	for name, tags := range notMatching {
		if ref.Matches(tags) {
			t.Errorf("%s: Matches(%v) = true, want false", name, tags)
		}
	}
}
