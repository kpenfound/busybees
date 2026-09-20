package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadCasesSkipsRoleAndFixturesDirectories(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "b-case", answerCase, answerFiles())
	writeCase(t, root, "a-case", answerCase, answerFiles())
	// evals/<role>/ holds that role's cases, for a run of that role.
	writeCase(t, root, "developer", "not = a case", map[string]string{})
	// evals/fixtures/ holds the fixtures cases share.
	writeCase(t, root, FixturesDir, "not = a case", map[string]string{})

	cases, err := LoadCases(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[0].Name != "a-case" || cases[1].Name != "b-case" {
		t.Fatalf("cases: %+v", cases)
	}
	c := cases[0]
	if c.Timeout.Duration != 2*time.Minute || c.MaxCost != 3 || c.Issues[0].Author != DefaultAuthor || c.Mail[0].From != DefaultAuthor {
		t.Fatalf("case: %+v", c)
	}

	one, err := LoadCases(root, "b-case")
	if err != nil || len(one) != 1 || one[0].Name != "b-case" {
		t.Fatalf("--case b-case: %+v, %v", one, err)
	}
	if _, err := LoadCases(root, "nope"); err == nil || !strings.Contains(err.Error(), `no case "nope"`) || !strings.Contains(err.Error(), "a-case, b-case") {
		t.Fatalf("--case nope: %v", err)
	}
	if _, err := LoadCases(t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "no cases") {
		t.Fatalf("an empty evals directory: %v", err)
	}
}

// Having no eval cases is the normal state of a project: say where they
// live rather than passing on an errno.
func TestLoadCasesWithoutAnEvalsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evals")
	_, err := LoadCases(dir, "")
	if err == nil {
		t.Fatal("a missing evals directory loaded")
	}
	for _, want := range []string{dir + "/<case>/", "case.toml", "docs/evals.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("the errno is still there: %v", err)
	}
}

// Any other read error keeps its wrapping.
func TestLoadCasesReadError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "evals")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCases(file, "")
	if err == nil || !strings.Contains(err.Error(), "eval cases: ") {
		t.Fatalf("a file where evals/ should be: %v", err)
	}
}

func TestLoadCaseDefaults(t *testing.T) {
	c, err := LoadCase(writeCase(t, t.TempDir(), "min", `test = "true"
[[issues]]
number = 4
title = "Do it"
[[issues.comments]]
body = "please"
`, map[string]string{"repo/README": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout.Duration != DefaultTimeout || c.MaxCost != DefaultMaxCost || c.Issues[0].Comments[0].Author != DefaultAuthor {
		t.Fatalf("defaults: %+v", c)
	}
	free, err := LoadCase(writeCase(t, t.TempDir(), "free", "max_cost = 0\n"+`test = "true"
[[issues]]
number = 4
title = "Do it"
`, map[string]string{"repo/README": "x"}))
	if err != nil || free.MaxCost != 0 {
		t.Fatalf("max_cost = 0 is no limit: %+v, %v", free, err)
	}
}

func TestLoadCaseRejects(t *testing.T) {
	const issue = "\n[[issues]]\nnumber = 1\ntitle = \"x\"\n"
	repo := map[string]string{"repo/README": "x"}
	for _, tc := range []struct {
		name, toml string
		files      map[string]string
		want       string
	}{
		{"unknown key", `test = "true"` + "\ncolour = 1" + issue, repo, "unknown keys: colour"},
		{"no test", issue, repo, "test: the command that grades the case is required"},
		{"no fixture", `test = "true"` + issue, map[string]string{}, "exactly one of repo/ and setup.sh"},
		{"two fixtures", `test = "true"` + issue, map[string]string{"repo/README": "x", SetupScript: "true"}, "exactly one of repo/ and setup.sh"},
		{"no issues", `test = "true"`, repo, "a case seeds at least one issue"},
		{"issue twice", `test = "true"` + issue + issue, repo, "#1 is seeded twice"},
		{"no number", `test = "true"` + "\n[[issues]]\ntitle = \"x\"\n", repo, `"x" has no number`},
		{"no title", `test = "true"` + "\n[[issues]]\nnumber = 2\n", repo, "#2 has no title"},
		{"negative budget", "max_cost = -1\n" + `test = "true"` + issue, repo, "max_cost must not be negative"},
		{"mail to nobody", `test = "true"` + issue + "\n[[mail]]\nto = \"boss\"\n", repo, "mail: to:"},
		{"mail about another issue", `test = "true"` + issue + "\n[[mail]]\nto = \"qa\"\nissue = 9\n", repo, "issue #9 is not a seeded issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadCase(writeCase(t, t.TempDir(), "bad", tc.toml, tc.files))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// A role's cases live under evals/<role>/, where only a run of that role
// takes them, and each one carries the role it runs.
func TestLoadRoleCases(t *testing.T) {
	root := t.TempDir()
	writeCase(t, filepath.Join(root, "developer"), "b-case", developerCase, answerRepo())
	writeCase(t, filepath.Join(root, "developer"), "a-case", developerCase, answerRepo())
	writeCase(t, root, "whole", answerCase, answerFiles())

	cases, err := LoadRoleCases(root, "developer", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[0].Name != "a-case" || cases[1].Name != "b-case" {
		t.Fatalf("cases: %+v", cases)
	}
	if cases[0].Role != "developer" || cases[0].Issue != 1 || cases[0].Expect.Outcome != "pr-opened" {
		t.Fatalf("case: %+v", cases[0])
	}
	one, err := LoadRoleCases(root, "developer", "b-case")
	if err != nil || len(one) != 1 || one[0].Name != "b-case" {
		t.Fatalf("--case b-case: %+v, %v", one, err)
	}
	if _, err := LoadRoleCases(root, "developer", "whole"); err == nil || !strings.Contains(err.Error(), `no case "whole"`) {
		t.Fatalf("a whole-factory case is not the developer's: %v", err)
	}
	// A whole-factory run does not take them either.
	if _, err := LoadCases(root, "developer"); err == nil || !strings.Contains(err.Error(), `no case "developer"`) {
		t.Fatalf("the role directory loaded as a whole-factory case: %v", err)
	}
	_, err = LoadRoleCases(root, "qa", "")
	if err == nil || !strings.Contains(err.Error(), "a qa eval case is a directory "+filepath.Join(root, "qa")+"/<case>/") {
		t.Fatalf("a role with no cases: %v", err)
	}
}

func TestLoadRoleCaseRejects(t *testing.T) {
	const issue = "\n[[issues]]\nnumber = 1\ntitle = \"x\"\n"
	const expect = "\n[expect]\noutcome = \"done\"\n"
	repo := map[string]string{"repo/README": "x"}
	for _, tc := range []struct {
		name, role, toml string
		want             string
	}{
		{"a test", "qa", `test = "true"` + issue + expect, "test belongs to a whole-factory case"},
		{"nothing to grade", "qa", issue, "expect: a per-role case declares at least one check"},
		{"no issue for the developer", "developer", issue + expect, "a developer case names the issue its session works on"},
		{"an issue for a singleton", "qa", "issue = 1" + issue + expect, "a qa session is about the whole repository"},
		{"an unseeded subject", "developer", "issue = 9" + issue + expect, "issue: #9 is not a seeded issue"},
		{"an unseeded expectation", "qa", issue + "\n[expect]\nissues_closed = [9]\n", "expect: #9 is not a seeded issue"},
		{"an outcome the role cannot report", "qa", issue + "\n[expect]\noutcome = \"pr-opened\"\n", "expect.outcome:"},
		{"a rubric with no rubric", "qa", issue + expect + "\n[[expect.graded]]\nname = \"good\"\n", `"good" has nothing for the grader`},
		{"a rubric with no name", "qa", issue + expect + "\n[[expect.graded]]\nrubric = \"is it good\"\n", "expect.graded: name:"},
		{"a pass score off the scale", "qa", issue + expect + "\n[[expect.graded]]\nname = \"n\"\nrubric = \"r\"\npass = 2\n", "which is not a score between 0 and 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRoleCase(writeCase(t, t.TempDir(), "bad", tc.toml, repo), tc.role)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// The per-role keys are refused on a whole-factory case, which is graded by
// its test and works every issue it seeds.
func TestLoadCaseRejectsPerRoleKeys(t *testing.T) {
	const issue = "\n[[issues]]\nnumber = 1\ntitle = \"x\"\n"
	repo := map[string]string{"repo/README": "x"}
	for _, tc := range []struct{ name, toml, want string }{
		{"a subject", `test = "true"` + "\nissue = 1" + issue, "issue and pr belong to a per-role case"},
		{"expectations", `test = "true"` + issue + "\n[expect]\noutcome = \"done\"\n", "expect belongs to a per-role case"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadCase(writeCase(t, t.TempDir(), "bad", tc.toml, repo))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
