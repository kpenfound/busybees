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

// An expect.labels block with no issue says which issue it wants.
func TestLoadRoleCaseRejectsLabelsWithNoIssue(t *testing.T) {
	_, err := LoadRoleCase(writeCase(t, t.TempDir(), "bad",
		"\n[[issues]]\nnumber = 1\ntitle = \"x\"\n[[expect.labels]]\nhas = [\"bees:ready\"]\n",
		map[string]string{"repo/README": "x"}), "qa")
	if err == nil || !strings.Contains(err.Error(), "expect.labels: issue: name the issue whose labels to check") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "#0 is not a seeded issue") {
		t.Errorf("it also complains about issue #0: %v", err)
	}
}

// prCase seeds a pull request beside its issue: the branch's tree lives in
// the case directory, and the rest is what GitHub shows for it.
const prCase = `
issue = 1
pr = 2

[[issues]]
number = 1
title = "Fix the answer"
labels = ["bees:in-progress", "bees:size/xs"]

[[pull_requests]]
number = 2
title = "Fix the answer"
body = "Closes #1"
head = "bees/issue-1"
labels = ["bees:wip"]

[[pull_requests.comments]]
body = "it still says broken"

[[pull_requests.reviews]]
author = "kpenfound"
state = "CHANGES_REQUESTED"
body = "one line, lowercase"

[expect]
outcome = "pr-updated"
`

// prFiles is prCase's fixture and the working tree of its head branch.
func prFiles() map[string]string {
	return map[string]string{
		"repo/answer.txt":            "broken\n",
		"repo/README.md":             "the answer\n",
		"pr/bees/issue-1/answer.txt": "half fixed\n",
	}
}

// A case can seed a pull request: the keys it leaves out take their
// defaults, and the head branch's tree is pr/<head>.
func TestLoadRoleCaseSeedsAPullRequest(t *testing.T) {
	dir := writeCase(t, t.TempDir(), "pr", prCase, prFiles())
	c, err := LoadRoleCase(dir, "developer")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.PullRequests) != 1 {
		t.Fatalf("pull requests: %+v", c.PullRequests)
	}
	p := c.PullRequests[0]
	if p.Base != DefaultBranch || p.Author != DefaultAuthor {
		t.Errorf("defaults: %+v", p)
	}
	if p.Comments[0].Author != DefaultAuthor || p.Reviews[0].Author != "kpenfound" || p.Reviews[0].State != "CHANGES_REQUESTED" {
		t.Errorf("conversation: %+v", p)
	}
	if want := filepath.Join(dir, PRDir, "bees/issue-1"); p.FilesDir(c) != want {
		t.Errorf("files directory %q, want %q", p.FilesDir(c), want)
	}
	// A review that names no state is a comment on the pull request.
	quiet, err := LoadRoleCase(writeCase(t, t.TempDir(), "pr",
		strings.Replace(prCase, "state = \"CHANGES_REQUESTED\"\n", "", 1), prFiles()), "developer")
	if err != nil {
		t.Fatal(err)
	}
	if got := quiet.PullRequests[0].Reviews[0].State; got != DefaultReviewState {
		t.Errorf("review state %q, want %q", got, DefaultReviewState)
	}
	// The files directory can be named instead of defaulted.
	files := prFiles()
	files["head/answer.txt"] = "half fixed\n"
	named, err := LoadRoleCase(writeCase(t, t.TempDir(), "pr",
		strings.Replace(prCase, `head = "bees/issue-1"`, "head = \"bees/issue-1\"\nfiles = \"head\"", 1), files), "developer")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(named.Dir, "head"); named.PullRequests[0].FilesDir(named) != want {
		t.Errorf("files directory %q, want %q", named.PullRequests[0].FilesDir(named), want)
	}
}

// What a case cannot say about a pull request it seeds.
func TestLoadCaseRejectsPullRequests(t *testing.T) {
	for _, tc := range []struct {
		name, toml string
		files      map[string]string
		want       string
	}{
		{"a subject that is not seeded", strings.Replace(prCase, "pr = 2", "pr = 9", 1), prFiles(),
			"pr: #9 is not a seeded pull request"},
		{"no tree", prCase, map[string]string{"repo/answer.txt": "broken\n"},
			filepath.Join(PRDir, "bees/issue-1") + " is not a directory"},
		{"a files directory that is not there", strings.Replace(prCase, `head = "bees/issue-1"`, "head = \"bees/issue-1\"\nfiles = \"elsewhere\"", 1), prFiles(),
			"elsewhere is not a directory"},
		{"no number", strings.Replace(prCase, "number = 2", "", 1), prFiles(),
			`pull_requests: "Fix the answer" has no number`},
		{"the number of a seeded issue", strings.Replace(prCase, "number = 2", "number = 1", 1), prFiles(),
			"pull_requests: #1 is already a seeded issue or pull request"},
		{"no title", strings.Replace(prCase, `title = "Fix the answer"
body`, "body", 1), prFiles(), "pull_requests: #2 has no title"},
		{"no head branch", strings.Replace(prCase, `head = "bees/issue-1"`, "", 1), prFiles(),
			"pull_requests: #2 has no head branch"},
		{"a head that is its base", strings.Replace(prCase, `head = "bees/issue-1"`, `head = "main"`, 1), prFiles(),
			"pull_requests: #2 is from main into itself"},
		{"a review state GitHub does not have", strings.Replace(prCase, `state = "CHANGES_REQUESTED"`, `state = "GRUMPY"`, 1), prFiles(),
			`review state "GRUMPY" is not one of APPROVED, CHANGES_REQUESTED, COMMENTED`},
		{"two pull requests from one branch", prCase + `
[[pull_requests]]
number = 3
title = "again"
head = "bees/issue-1"
`, prFiles(), "pull_requests: #2 and #3 are both from bees/issue-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRoleCase(writeCase(t, t.TempDir(), "bad", tc.toml, tc.files), "developer")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
