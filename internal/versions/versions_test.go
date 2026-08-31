package versions

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
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

func TestBees(t *testing.T) {
	build := func(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}, Settings: settings}
	}
	rev := func(v string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.revision", Value: v} }
	mod := func(v string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.modified", Value: v} }

	cases := []struct {
		name     string
		override string
		bi       *debug.BuildInfo
		want     string
	}{
		{"ldflags override wins", "v1.2.3", build("v0.2.0"), "v1.2.3"},
		{"override left at the default is ignored", "dev", build("v0.2.0"), "v0.2.0"},
		{"installed tag", "dev", build("v0.2.0"), "v0.2.0"},
		{"installed pseudo-version", "dev", build("v0.0.0-20260829201307-b24a0605c2a1"), "v0.0.0-20260829201307-b24a0605c2a1"},
		{"local build, clean tree", "dev", build("(devel)", rev("b24a0605c2a1e9f0d3c4"), mod("false")), "dev (b24a0605c2a1)"},
		{"local build, dirty tree", "dev", build("(devel)", rev("b24a0605c2a1e9f0d3c4"), mod("true")), "dev (b24a0605c2a1 modified)"},
		{"short revision is not truncated", "dev", build("(devel)", rev("b24a06"), mod("false")), "dev (b24a06)"},
		{"empty main version with a revision", "", build("", rev("b24a0605c2a1e9f0d3c4")), "dev (b24a0605c2a1)"},
		{"devel without vcs stamps", "dev", build("(devel)"), "dev"},
		{"no build info", "dev", nil, "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Bees(c.override, c.bi); got != c.want {
				t.Errorf("Bees(%q, %+v) = %q, want %q", c.override, c.bi, got, c.want)
			}
		})
	}
}

// Revision is the one parser for the VCS stamps: it returns the commit
// untruncated (Bees is what shortens it for display) and says whether the
// tree was dirty, and answers empty for a binary carrying no stamps at all.
func TestRevision(t *testing.T) {
	build := func(settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings}
	}
	rev := func(v string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.revision", Value: v} }
	mod := func(v string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.modified", Value: v} }

	for _, c := range []struct {
		name    string
		bi      *debug.BuildInfo
		wantRev string
		wantMod bool
	}{
		{"clean revision", build(rev("b24a0605c2a1e9f0d3c4"), mod("false")), "b24a0605c2a1e9f0d3c4", false},
		{"modified revision", build(rev("b24a0605c2a1e9f0d3c4"), mod("true")), "b24a0605c2a1e9f0d3c4", true},
		{"longer than the display length", build(rev("b24a0605c2a1e9f0d3c4b5a6978869d3d1e2f3a4")), "b24a0605c2a1e9f0d3c4b5a6978869d3d1e2f3a4", false},
		{"shorter than the display length", build(rev("b24a06")), "b24a06", false},
		{"no vcs settings", build(), "", false},
		{"other settings only", build(debug.BuildSetting{Key: "GOARCH", Value: "arm64"}), "", false},
		{"no build info", nil, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			gotRev, gotMod := Revision(c.bi)
			if gotRev != c.wantRev || gotMod != c.wantMod {
				t.Errorf("Revision() = %q, %v; want %q, %v", gotRev, gotMod, c.wantRev, c.wantMod)
			}
		})
	}
}
