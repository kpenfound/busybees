package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// colourTokens are the ways a colour can be named in this package, built by
// concatenation so this file does not match its own search, the way
// internal/tui and internal/reviewtui keep theirs. Theirs also sweep for a
// style's Background; this package calls context's Background on most of
// its pages, so that one is swept as the style method on a lipgloss style
// chain only.
var colourTokens = []string{
	"lipgloss." + "Color(",
	"lipgloss." + "AdaptiveColor",
	"." + "Foreground(",
	"NewStyle()." + "Background(",
	"." + "BorderForeground(",
	"." + "BorderBackground(",
}

// TestOnlyTheThemeNamesColours is the palette pin the two TUI packages
// keep: every colour this package names is in theme.go, so a person
// repainting the review progress view has one file to read.
func TestOnlyTheThemeNamesColours(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var swept int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == "theme.go" {
			continue
		}
		swept++
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, tok := range colourTokens {
			if strings.Contains(string(b), tok) {
				t.Errorf("%s names a colour with %q: every colour belongs in theme.go", name, tok)
			}
		}
	}
	if swept == 0 {
		t.Fatal("swept no files: the package's sources were not found")
	}
}
