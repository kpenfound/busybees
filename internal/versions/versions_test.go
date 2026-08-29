package versions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndLess(t *testing.T) {
	cases := map[string]string{
		"gh version 2.69.0 (2025-03-19)\nhttps://github.com/cli/cli/releases/tag/v2.69.0": "2.69.0",
		"2.1.251 (Claude Code)": "2.1.251",
		"v10.0.3":               "10.0.3",
	}
	for in, want := range cases {
		v, err := Parse(in)
		if err != nil || v.String() != want {
			t.Errorf("Parse(%q) = %v, %v; want %s", in, v, err, want)
		}
	}
	if _, err := Parse("gh version unknown"); err == nil {
		t.Error("expected error for missing version")
	}
	older := []struct{ a, b string }{{"2.49.9", "2.50.0"}, {"2.1.9", "2.1.76"}, {"1.99.99", "2.0.0"}}
	for _, c := range older {
		a, _ := Parse(c.a)
		b, _ := Parse(c.b)
		if !a.Less(b) || b.Less(a) || a.Less(a) {
			t.Errorf("Less(%s, %s) wrong", c.a, c.b)
		}
	}
}

func fakeTool(t *testing.T, output string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	if err := Check(ctx, "gh", fakeTool(t, "gh version 2.69.0 (2025-03-19)"), "2.50.0"); err != nil {
		t.Errorf("new enough: %v", err)
	}
	err := Check(ctx, "gh", fakeTool(t, "gh version 2.49.2 (2024-04-01)"), MinGH)
	if err == nil || !strings.Contains(err.Error(), "2.49.2 is too old") || !strings.Contains(err.Error(), EnvSkip) {
		t.Errorf("too old: %v", err)
	}
	if err := Check(ctx, "claude", fakeTool(t, "2.1.76 (Claude Code)"), MinClaude); err != nil {
		t.Errorf("exact minimum: %v", err)
	}
	if err := Check(ctx, "gh", filepath.Join(t.TempDir(), "missing"), MinGH); err == nil {
		t.Error("missing binary should fail")
	}
	if err := Check(ctx, "gh", fakeTool(t, "nonsense"), MinGH); err == nil {
		t.Error("unparseable output should fail")
	}
}

func TestSkip(t *testing.T) {
	t.Setenv(EnvSkip, "1")
	if err := CheckAll(context.Background(), filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("skip: %v", err)
	}
}
