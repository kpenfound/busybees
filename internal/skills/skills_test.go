package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in                     string
		url, ref, subdir, name string
	}{
		{"https://github.com/acme/skills", "https://github.com/acme/skills", "", "", "skills"},
		{"https://github.com/acme/skills.git@v1.2", "https://github.com/acme/skills.git", "v1.2", "", "skills"},
		{"https://github.com/acme/skills#skills/tdd", "https://github.com/acme/skills", "", "skills/tdd", "skills-tdd"},
		{"git@github.com:acme/My_Plugin.git@main#a/b", "git@github.com:acme/My_Plugin.git", "main", "a/b", "my-plugin-b"},
	}
	for _, c := range cases {
		s, err := Parse(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if s.URL != c.url || s.Ref != c.ref || s.Subdir != c.subdir || s.Name != c.name {
			t.Errorf("%s: got %+v", c.in, s)
		}
	}
	if _, err := Parse(""); err == nil {
		t.Fatal("expected error")
	}
}

// fakeGit "clones" by copying a local fixture directory.
func fakeGit(fixture string) func(ctx context.Context, dir string, args ...string) error {
	return func(ctx context.Context, dir string, args ...string) error {
		if args[0] != "clone" {
			return nil
		}
		dest := args[len(args)-1]
		return os.CopyFS(dest, os.DirFS(fixture))
	}
}

func TestPrepareLayouts(t *testing.T) {
	fixtures := t.TempDir()
	// single skill at root
	single := filepath.Join(fixtures, "single")
	mk(t, filepath.Join(single, "SKILL.md"), "---\nname: single\n---\n")
	mk(t, filepath.Join(single, ".git", "HEAD"), "")
	// collection
	coll := filepath.Join(fixtures, "coll")
	mk(t, filepath.Join(coll, "skills", "a", "SKILL.md"), "")
	mk(t, filepath.Join(coll, "skills", "b", "SKILL.md"), "")
	mk(t, filepath.Join(coll, ".git", "HEAD"), "")
	// plugin
	plug := filepath.Join(fixtures, "plug")
	mk(t, filepath.Join(plug, ".claude-plugin", "plugin.json"), `{"name":"plug"}`)
	mk(t, filepath.Join(plug, ".git", "HEAD"), "")

	for _, c := range []struct {
		fixture, ref string
		check        func(t *testing.T, dir string)
	}{
		{single, "https://github.com/x/single", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, ".claude-plugin", "plugin.json"))
			mustExist(t, filepath.Join(dir, "skills", "single", "SKILL.md"))
		}},
		{coll, "https://github.com/x/coll", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, "skills", "a", "SKILL.md"))
			mustExist(t, filepath.Join(dir, "skills", "b", "SKILL.md"))
		}},
		{coll, "https://github.com/x/coll#skills/b", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, "skills", "coll-b", "SKILL.md"))
		}},
		{plug, "https://github.com/x/plug", func(t *testing.T, dir string) {
			if filepath.Base(filepath.Dir(dir)) != "repos" {
				t.Fatalf("plugin should be used in place, got %s", dir)
			}
		}},
	} {
		m := NewManager(t.TempDir())
		m.Git = fakeGit(c.fixture)
		dirs, err := m.Prepare(context.Background(), []string{c.ref})
		if err != nil {
			t.Fatalf("%s: %v", c.ref, err)
		}
		if len(dirs) != 1 {
			t.Fatalf("%s: dirs %v", c.ref, dirs)
		}
		c.check(t, dirs[0])
	}

	// not a skill at all
	bad := filepath.Join(fixtures, "bad")
	mk(t, filepath.Join(bad, "README.md"), "")
	mk(t, filepath.Join(bad, ".git", "HEAD"), "")
	m := NewManager(t.TempDir())
	m.Git = fakeGit(bad)
	if _, err := m.Prepare(context.Background(), []string{"https://github.com/x/bad"}); err == nil {
		t.Fatal("expected error for non-skill repo")
	}
}

func mk(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("missing %s: %v", p, err)
	}
}
