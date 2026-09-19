package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddAndList(t *testing.T) {
	file := filepath.Join(t.TempDir(), "todo.txt")
	for _, title := range []string{"buy milk", "(A) call the plumber"} {
		if err := run([]string{"-f", file, "add", title}, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	if err := run([]string{"-f", file, "list"}, &out); err != nil {
		t.Fatal(err)
	}
	if want := "1 buy milk\n2 (A) call the plumber\n"; out.String() != want {
		t.Fatalf("list:\n%s", out.String())
	}
	b, _ := os.ReadFile(file)
	if string(b) != "buy milk\n(A) call the plumber\n" {
		t.Fatalf("file:\n%s", b)
	}
}

func TestUnknownCommand(t *testing.T) {
	if err := run([]string{"-f", filepath.Join(t.TempDir(), "todo.txt"), "frobnicate"}, &strings.Builder{}); err == nil {
		t.Fatal("an unknown command succeeded")
	}
}
