package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

func loadTestConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bees.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSkillRefs(t *testing.T) {
	cfg := loadTestConfig(t, `version = 1
[project]
repo = "a/b"
[global]
skills = ["https://github.com/x/shared"]
[roles.developer]
skills = ["https://github.com/x/tdd"]
[roles.qa]
enabled = false
skills = ["https://github.com/x/qa-only"]
`)
	refs, err := skillRefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.Ref+"="+strings.Join(r.Roles, ","))
	}
	want := []string{
		"https://github.com/x/shared=product_manager,project_manager,developer,reviewer",
		"https://github.com/x/tdd=developer",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", got)
	}
}

func TestListText(t *testing.T) {
	out := listText("/cache/bees", "24h", []skillRow{
		{Commit: "abc1234", Age: "2h ago", Roles: "developer", Ref: "https://github.com/x/tdd"},
		{Commit: "not cached", Age: "-", Roles: "qa", Ref: "https://github.com/x/qa"},
	})
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines %q", lines)
	}
	if !strings.Contains(lines[0], "/cache/bees") || !strings.Contains(lines[0], "refresh: 24h") {
		t.Errorf("header %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "abc1234    ") || !strings.HasSuffix(lines[1], "https://github.com/x/tdd") {
		t.Errorf("row %q", lines[1])
	}
	if !strings.Contains(lines[2], "not cached") {
		t.Errorf("row %q", lines[2])
	}
	if got := listText("/cache/bees", "never", nil); !strings.Contains(got, "no skills configured") {
		t.Errorf("empty: %q", got)
	}
}

func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		then time.Time
		want string
	}{
		{time.Time{}, "unknown"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-90 * time.Minute), "1h ago"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
		{now.Add(time.Hour), "just now"},
	} {
		if got := humanAge(now, c.then); got != c.want {
			t.Errorf("%v: got %q want %q", c.then, got, c.want)
		}
	}
}

func TestUpdateLine(t *testing.T) {
	ref := "https://github.com/x/tdd"
	for _, c := range []struct {
		before, after string
		err           error
		want          string
	}{
		{"", "abc1234", nil, "cloned " + ref + " abc1234"},
		{"abc1234", "abc1234", nil, "unchanged " + ref + " abc1234"},
		{"abc1234", "def5678", nil, "updated " + ref + " abc1234 → def5678"},
		{"abc1234", "", errors.New("boom"), "failed " + ref + ": boom"},
	} {
		if got := updateLine(ref, c.before, c.after, c.err); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

func TestSelectRefs(t *testing.T) {
	refs := []skillRef{{Ref: "a", Roles: []string{"developer"}}, {Ref: "b"}}
	got, err := selectRefs(refs, []string{"b", " a "})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Ref != "b" || got[1].Ref != "a" {
		t.Fatalf("got %+v", got)
	}
	_, err = selectRefs(refs, []string{"c"})
	if err == nil || !strings.Contains(err.Error(), "not a configured skill") {
		t.Fatalf("err %v", err)
	}
}
