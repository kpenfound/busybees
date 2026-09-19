package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The cases shipped under evals/ load, and each one's test fails on its
// fixture, which is what makes it worth running.
func TestShippedCasesFailOnTheirFixtures(t *testing.T) {
	cases, err := LoadCases("../../evals", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			fx, err := buildFixture(ctx, c, dir)
			if err != nil {
				t.Fatal(err)
			}
			passed, log, err := runTest(ctx, c, fx.origin, filepath.Join(dir, "before"))
			if err != nil {
				t.Fatal(err)
			}
			if passed {
				b, _ := os.ReadFile(log)
				t.Fatalf("the test passes on the fixture:\n%s", b)
			}
		})
	}
}

// The trivial case's test passes once greet.sh is fixed: it can pass.
func TestHelloPassesWhenFixed(t *testing.T) {
	c, err := LoadCase("../../evals/hello")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	fx, err := buildFixture(ctx, c, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.project, "greet.sh"), []byte("#!/bin/sh\necho \"Hello, $1!\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, append(append([]string{}, gitIdentity...), "commit", "-q", "-m", "fix"), {"push", "-q"}} {
		if _, err := git(ctx, fx.project, args...); err != nil {
			t.Fatal(err)
		}
	}
	passed, log, err := runTest(ctx, c, fx.origin, filepath.Join(dir, "after"))
	if err != nil || !passed {
		b, _ := os.ReadFile(log)
		t.Fatalf("the fixed fixture fails its test (%v):\n%s", err, b)
	}
}
