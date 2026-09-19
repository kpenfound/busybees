package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvalCount(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "todo.txt")
	if err := os.WriteFile(file, []byte("buy milk\nx (A) file taxes\n\n(B) call the plumber due:2026-03-01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"-f", file, "count"}, &out); err != nil {
		t.Fatalf("todo count: %v", err)
	}
	if out.String() != "2 pending, 1 done\n" {
		t.Fatalf("todo count printed %q", out.String())
	}

	out.Reset()
	if err := run([]string{"-f", filepath.Join(dir, "missing.txt"), "count"}, &out); err != nil {
		t.Fatalf("todo count on a missing file: %v", err)
	}
	if out.String() != "0 pending, 0 done\n" {
		t.Fatalf("todo count on a missing file printed %q", out.String())
	}

	if err := run([]string{"-f", file, "count", "extra"}, &strings.Builder{}); err == nil {
		t.Fatal("todo count extra succeeded")
	}
}
