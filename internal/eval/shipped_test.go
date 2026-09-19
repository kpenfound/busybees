package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// shippedSolution is the directory beside a shipped case's case.toml that
// holds a fix for its issues: files copied over the fixture, as a developer
// would change them. Only this suite reads it.
const shippedSolution = "solution"

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

// Each shipped case's test passes once its solution is committed on the
// fixture: the case can pass, and it fails on the fixture for the reason
// its issue gives, not because the fixture is broken.
func TestShippedCasesPassWithTheirSolutions(t *testing.T) {
	cases, err := LoadCases("../../evals", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			solution := filepath.Join(c.Dir, shippedSolution)
			if _, err := os.Stat(solution); err != nil {
				t.Fatalf("a shipped case needs a %s/ that fixes it: %v", shippedSolution, err)
			}
			dir := t.TempDir()
			fx, err := buildFixture(ctx, c, dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := copyTree(solution, fx.project); err != nil {
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
		})
	}
}
