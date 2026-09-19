package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvalDoneMarksTheNumberedItem(t *testing.T) {
	file := filepath.Join(t.TempDir(), "todo.txt")
	if err := os.WriteFile(file, []byte("buy milk\ncall the plumber\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-f", file, "done", "1"}, &strings.Builder{}); err != nil {
		t.Fatalf("todo done 1: %v", err)
	}
	b, _ := os.ReadFile(file)
	if string(b) != "x buy milk\ncall the plumber\n" {
		t.Fatalf("after todo done 1:\n%s", b)
	}
}
