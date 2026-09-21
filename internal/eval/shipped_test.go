package eval

import (
	"bytes"
	"context"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
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

// Every per-role case shipped under evals/<role>/ loads and builds its
// fixture. There is no test to run it against: a per-role case is graded by
// the checks it declares, and those need a session.
func TestShippedRoleCasesLoad(t *testing.T) {
	ctx := context.Background()
	roles := 0
	for _, role := range config.Roles {
		if _, err := os.Stat(filepath.Join("../../evals", role)); err != nil {
			continue
		}
		roles++
		cases, err := LoadRoleCases("../../evals", role, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			t.Run(role+"/"+c.Name, func(t *testing.T) {
				if _, err := buildFixture(ctx, c, t.TempDir()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	// A run of the whole factory does not take them: no case it loads is
	// named after a role.
	whole, err := LoadCases("../../evals", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range whole {
		if slices.Contains(config.Roles, c.Name) {
			t.Errorf("%s is a role directory, taken as a whole-factory case", c.Name)
		}
	}
	if roles == 0 {
		t.Fatal("no role ships a case; this suite would pin nothing")
	}
}

// shippedCases is every case shipped under evals/: the whole-factory ones
// and each role's, with Case.Role set, so a check over all of them takes a
// new case without being added to a list.
func shippedCases(t *testing.T) []Case {
	t.Helper()
	all, err := LoadCases("../../evals", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range config.Roles {
		if _, err := os.Stat(filepath.Join("../../evals", role)); err != nil {
			continue
		}
		cases, err := LoadRoleCases("../../evals", role, "")
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, cases...)
	}
	return all
}

// caseKey names a case in a subtest: the role's cases under their role.
func caseKey(c Case) string {
	if c.Role == "" {
		return c.Name
	}
	return c.Role + "/" + c.Name
}

// The Go a shipped case hands a session is sound: the assembled fixture,
// and the tree of every pull request the case seeds, build, are
// gofmt-clean and have passing tests. A seeded branch whose own tests fail
// measures whoever runs `go test` rather than the role the case is about,
// and nothing else in the suite compiles either tree.
func TestShippedCaseGoIsSound(t *testing.T) {
	ctx := context.Background()
	for _, c := range shippedCases(t) {
		t.Run(caseKey(c), func(t *testing.T) {
			fx, err := buildFixture(ctx, c, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := checkGo(ctx, fx.project); err != nil {
				t.Errorf("the fixture: %v", err)
			}
			for _, p := range c.PullRequests {
				if _, err := git(ctx, fx.project, "checkout", "-q", p.Head); err != nil {
					t.Fatal(err)
				}
				if err := checkGo(ctx, fx.project); err != nil {
					t.Errorf("pull request #%d's branch %s: %v", p.Number, p.Head, err)
				}
			}
		})
	}
}

// goCheckTimeout bounds one go command over an assembled fixture.
const goCheckTimeout = 5 * time.Minute

// checkGo reports what is wrong with the Go under dir: a file that is not
// gofmt-clean, a module that does not build, a test that fails. A
// directory holding no Go module is nothing to check, and reports nil.
func checkGo(ctx context.Context, dir string) error {
	mods, err := goModules(dir)
	if err != nil {
		return err
	}
	for _, mod := range mods {
		where, err := filepath.Rel(dir, mod)
		if err != nil {
			return err
		}
		if err := gofmtClean(mod, dir); err != nil {
			return err
		}
		for _, args := range [][]string{{"build", "./..."}, {"test", "./..."}} {
			out, err := runGo(ctx, mod, args...)
			if err != nil {
				return fmt.Errorf("go %s in %s: %w\n%s", strings.Join(args, " "), where, err, out)
			}
		}
	}
	return nil
}

// goModules is the directories under root holding a go.mod, git's own
// directory left out.
func goModules(root string) ([]string, error) {
	var mods []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "go.mod" {
			mods = append(mods, filepath.Dir(path))
		}
		return nil
	})
	return mods, err
}

// gofmtClean reports the Go files under mod that gofmt would rewrite,
// named relative to base. It formats in this process: nothing on PATH.
func gofmtClean(mod, base string) error {
	var dirty []string
	err := filepath.WalkDir(mod, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		formatted, err := format.Source(src)
		if err != nil {
			// Unparseable: the build says so, in its own words.
			return nil
		}
		if !bytes.Equal(src, formatted) {
			where, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			dirty = append(dirty, where)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("gofmt would rewrite %s", strings.Join(dirty, ", "))
	}
	return nil
}

// runGo runs one go command in dir, with no module proxy and no toolchain
// download: a fixture has no dependencies, and the check reaches no
// network.
func runGo(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, goCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOPROXY=off", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// checkGo is what the suite has instead of a broken case, so it is checked
// against Go of each kind it is there to catch, and against a directory
// with no Go in it at all.
func TestCheckGoCatchesUnsoundGo(t *testing.T) {
	const clean = "package todo\n\n// Sum adds the numbers.\nfunc Sum(n []int) int {\n\ttotal := 0\n\tfor _, v := range n {\n\t\ttotal += v\n\t}\n\treturn total\n}\n"
	const passes = "package todo\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif Sum([]int{1, 2}) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"sound", map[string]string{"todo/sum.go": clean, "todo/sum_test.go": passes}, ""},
		{"unformatted", map[string]string{"todo/sum.go": "package todo\n\nfunc Sum( n int ) int { return n }\n"}, "gofmt"},
		{"does not build", map[string]string{"todo/sum.go": "package todo\n\nimport \"nosuch/pkg\"\n\nfunc Sum() int { return pkg.N }\n"}, "go build"},
		{"failing test", map[string]string{"todo/sum.go": clean, "todo/sum_test.go": "package todo\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tt.Fatal(\"the branch's own test fails\")\n}\n"}, "go test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "go.mod", "module example.com/todo\n\ngo 1.22\n")
			for name, body := range tc.files {
				write(t, dir, name, body)
			}
			err := checkGo(context.Background(), dir)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("sound Go reported as unsound: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("%s went unreported", tc.name)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error does not say %q: %v", tc.want, err)
			}
		})
	}
	// A case whose fixture is not Go at all is nothing to check.
	if err := checkGo(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("a directory with no Go module: %v", err)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
