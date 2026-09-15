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
func TestTagsAreOpaqueAndCopied(t *testing.T) {
	ref := Ref{Key: "role-reviewer", Tags: map[string]string{"arbitrary/key": "✓", "empty": ""}}
	copy := ref.Clone()
	copy.Tags["arbitrary/key"] = "changed"
	if !ref.Matches(map[string]string{"arbitrary/key": "✓", "empty": ""}) || ref.Matches(map[string]string{"missing": ""}) {
		t.Fatal("tag matching lost presence or value")
	}
}
