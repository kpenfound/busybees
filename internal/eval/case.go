// Package eval is `bees eval`: it runs the factory against a fixture
// repository and grades the result, SWE-bench Lite style.
//
// A whole-factory case is a directory under evals/ (case.go): a fixture
// repository, the GitHub state and mail to seed (issues, and pull requests
// with a branch of their own in the fixture), and a test command that
// fails on the fixture and must pass once the factory has worked the seeded
// issues. The runner (run.go) builds the fixture as a local bare origin,
// seeds an in-memory GitHub (internal/fakegh), and runs the scheduler pass
// after pass, merging the pull requests it approved the way a person would,
// until every seeded issue is closed or held for a person, or the case's
// budget or timeout runs out. Sessions reach that GitHub through a gh of
// their own on PATH (shim.go). Grading (run.go's grade) runs the test
// command on the default branch and checks that every seeded issue closed
// with a pull request, and the report (report.go) is a table and a JSON
// file per run.
//
// `bees eval <role>` runs one role instead, against the cases under
// evals/<role>/: the scheduler is scoped to that role, the seeded GitHub
// state and mailbox stand in for the others, and one session runs the way
// `bees exec` runs it (run.go's runRole). A reviewer case is the review
// loop's review stage, review and all, held to one round (profile.go's
// ReviewRounds) so that the verdict ends the run. Such a case is graded by what it declares
// (expect.go): the outcome the session reported, labels moved, mail sent,
// issues created or closed, a pull request opened, and rubrics a grader
// session of its own scores (grader.go).
//
// Which agent profiles the sessions run on is profile.go's: --profile, or
// the profiles bees.toml selects, or the person's global config.toml,
// which the user defaults file fills below it.
package eval

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/busybees/internal/config"
)

// CaseFile is the file that makes a directory under evals/ a case.
const CaseFile = "case.toml"

// The parts of a case directory beside CaseFile.
const (
	// RepoDir holds the fixture repository's files, committed as they are.
	RepoDir = "repo"
	// SetupScript builds the fixture instead: it is run with sh in an empty
	// directory, which is then committed.
	SetupScript = "setup.sh"
	// GradeDir holds files copied over the checkout the test command runs
	// in, before the run and after it, so a session cannot change the tests
	// that grade it.
	GradeDir = "grade"
	// PRDir holds one directory per seeded pull request, named after its
	// head branch: the working tree of that branch.
	PRDir = "pr"
)

// FixturesDir, directly under evals/, is not a case: it holds fixture
// repositories several cases share, each case's SetupScript copying one in.
const FixturesDir = "fixtures"

// Defaults for the keys a case leaves out.
const (
	DefaultTimeout = time.Hour
	DefaultMaxCost = 10.0
	// DefaultAuthor is who a seeded issue, pull request, comment, review or
	// mail is from.
	DefaultAuthor = "human"
)

// Case is one eval: evals/<name>/case.toml, or evals/<role>/<name>/case.toml
// for a per-role one, and the fixture beside it.
type Case struct {
	// Name is the case directory's name, and Dir its absolute path.
	Name string `toml:"-"`
	Dir  string `toml:"-"`
	// Role is the role a per-role case runs in isolation, taken from the
	// directory it lives in, and "" for a whole-factory case.
	Role string `toml:"-"`

	Description string `toml:"description"`
	// Test grades a whole-factory case: a command run with sh -c in a
	// checkout of the default branch, which must fail on the fixture and
	// pass after the run. A per-role case has none; it is graded by what
	// it declares under Expect.
	Test string `toml:"test"`
	// Issue and PR are what a per-role case's session is about, the way
	// `bees exec --issue`/`--pr` name them. A whole-factory case has
	// neither: it works every issue it seeds.
	Issue int `toml:"issue"`
	PR    int `toml:"pr"`
	// Timeout and MaxCost (USD) stop the run: the factory is stopped, the
	// sessions still running with it, and the case graded as it stands.
	Timeout config.Duration `toml:"timeout"`
	MaxCost float64         `toml:"max_cost"`
	Issues  []Issue         `toml:"issues"`
	Mail    []Mail          `toml:"mail"`
	// PullRequests are the pull requests to seed: a branch of their own in
	// the fixture, and the pull request GitHub shows for it.
	PullRequests []PullRequest `toml:"pull_requests"`
	// Expect is how a per-role case is graded (expect.go).
	Expect Expect `toml:"expect"`
}

// Issue is one seeded issue. The runner adds the factory's label, so the
// labels here are the state and kind labels: "bees:ready" and a size for a
// work item a developer takes straight away, "bees:triage" for one the
// project manager refines first.
type Issue struct {
	Number   int       `toml:"number"`
	Title    string    `toml:"title"`
	Body     string    `toml:"body"`
	Labels   []string  `toml:"labels"`
	Author   string    `toml:"author"`
	Comments []Comment `toml:"comments"`
}

// Comment is one comment on a seeded issue or pull request.
type Comment struct {
	Author string `toml:"author"`
	Body   string `toml:"body"`
}

// PullRequest is one seeded pull request: the change on a branch of the
// fixture, and what the fake GitHub answers about it.
type PullRequest struct {
	Number int    `toml:"number"`
	Title  string `toml:"title"`
	Body   string `toml:"body"`
	// Head is the branch the change is on, and Base the branch it is
	// against (DefaultBranch by default). Head is branched off Base.
	Head string `toml:"head"`
	Base string `toml:"base"`
	// Files is the directory holding the head branch's working tree,
	// relative to the case directory; PRDir/<head> by default. It is
	// copied over the fixture and committed on Head, so a file it leaves
	// out is the one Base has.
	Files string `toml:"files"`
	// Labels are the labels beside the factory's own, and Author who
	// opened the pull request (default DefaultAuthor).
	Labels   []string  `toml:"labels"`
	Author   string    `toml:"author"`
	Comments []Comment `toml:"comments"`
	Reviews  []Review  `toml:"reviews"`
}

// FilesDir is where the case keeps the head branch's working tree.
func (p PullRequest) FilesDir(c Case) string {
	if p.Files != "" {
		return filepath.Join(c.Dir, p.Files)
	}
	return filepath.Join(c.Dir, PRDir, p.Head)
}

// Review is one review left on a seeded pull request.
type Review struct {
	Author string `toml:"author"`
	// State is one of ReviewStates; DefaultReviewState by default.
	State string `toml:"state"`
	Body  string `toml:"body"`
}

// ReviewStates are the states a seeded review can be in, and
// DefaultReviewState the one a review that names none is in.
var ReviewStates = []string{"APPROVED", "CHANGES_REQUESTED", "COMMENTED"}

const DefaultReviewState = "COMMENTED"

// Mail is one message in a role's mailbox when the run starts.
type Mail struct {
	From    string `toml:"from"`
	To      string `toml:"to"`
	Subject string `toml:"subject"`
	Body    string `toml:"body"`
	Issue   int    `toml:"issue"`
}

// LoadCases reads the whole-factory cases under dir, in name order: every
// one, or only the one called name. A directory named after a role is left
// out: it holds that role's cases, which a whole-factory run does not take.
// So is FixturesDir.
func LoadCases(dir, name string) ([]Case, error) {
	return loadCases(dir, name, "", func(e string) bool {
		return e == FixturesDir || slices.Contains(config.Roles, e)
	})
}

// LoadRoleCases reads role's cases under dir/<role>/, in name order: every
// one, or only the one called name.
func LoadRoleCases(dir, role string, name string) ([]Case, error) {
	return loadCases(filepath.Join(dir, role), name, role, func(string) bool { return false })
}

// loadCases reads the cases in the directories under dir that skip does not
// leave out, as cases of role.
func loadCases(dir, name, role string, skip func(string) bool) ([]Case, error) {
	kind := "an eval case"
	if role != "" {
		kind = "a " + role + " eval case"
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no %s directory here: %s is a directory %s/<case>/ with a case.toml in it, described in docs/evals.md", dir, kind, dir)
	}
	if err != nil {
		return nil, fmt.Errorf("eval cases: %w", err)
	}
	var cases []Case
	var names []string
	for _, e := range entries {
		if !e.IsDir() || skip(e.Name()) {
			continue
		}
		names = append(names, e.Name())
		if name != "" && e.Name() != name {
			continue
		}
		c, err := loadCase(filepath.Join(dir, e.Name()), role)
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	switch {
	case name != "" && len(cases) == 0:
		return nil, fmt.Errorf("no case %q under %s (cases: %s)", name, dir, strings.Join(names, ", "))
	case len(cases) == 0:
		return nil, fmt.Errorf("no cases under %s", dir)
	}
	return cases, nil
}

// LoadCase reads the whole-factory case in dir and checks it.
func LoadCase(dir string) (Case, error) { return loadCase(dir, "") }

// LoadRoleCase reads the case in dir as one of role's and checks it.
func LoadRoleCase(dir, role string) (Case, error) { return loadCase(dir, role) }

func loadCase(dir, role string) (Case, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Case{}, err
	}
	path := filepath.Join(abs, CaseFile)
	c := Case{Name: filepath.Base(abs), Dir: abs, Role: role}
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return Case{}, fmt.Errorf("case %s: %w", c.Name, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return Case{}, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if c.Timeout.Duration == 0 {
		c.Timeout.Duration = DefaultTimeout
	}
	if !md.IsDefined("max_cost") {
		c.MaxCost = DefaultMaxCost
	}
	for i := range c.Issues {
		c.Issues[i].Author = firstNonEmpty(c.Issues[i].Author, DefaultAuthor)
		for j := range c.Issues[i].Comments {
			c.Issues[i].Comments[j].Author = firstNonEmpty(c.Issues[i].Comments[j].Author, DefaultAuthor)
		}
	}
	for i := range c.Mail {
		c.Mail[i].From = firstNonEmpty(c.Mail[i].From, DefaultAuthor)
	}
	for i := range c.PullRequests {
		p := &c.PullRequests[i]
		p.Author = firstNonEmpty(p.Author, DefaultAuthor)
		p.Base = firstNonEmpty(p.Base, DefaultBranch)
		for j := range p.Comments {
			p.Comments[j].Author = firstNonEmpty(p.Comments[j].Author, DefaultAuthor)
		}
		for j := range p.Reviews {
			p.Reviews[j].Author = firstNonEmpty(p.Reviews[j].Author, DefaultAuthor)
			p.Reviews[j].State = firstNonEmpty(p.Reviews[j].State, DefaultReviewState)
		}
	}
	if err := c.validate(); err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c Case) validate() error {
	var errs []error
	if c.Timeout.Duration < 0 {
		errs = append(errs, errors.New("timeout must be positive"))
	}
	if c.MaxCost < 0 {
		errs = append(errs, errors.New("max_cost must not be negative (0 is no limit)"))
	}
	_, repoErr := os.Stat(filepath.Join(c.Dir, RepoDir))
	_, setupErr := os.Stat(filepath.Join(c.Dir, SetupScript))
	if (repoErr == nil) == (setupErr == nil) {
		errs = append(errs, fmt.Errorf("the case needs exactly one of %s/ and %s to build its fixture", RepoDir, SetupScript))
	}
	if len(c.Issues) == 0 {
		errs = append(errs, errors.New("issues: a case seeds at least one issue"))
	}
	seeded := map[int]bool{}
	for _, i := range c.Issues {
		if i.Number <= 0 {
			errs = append(errs, fmt.Errorf("issues: %q has no number", i.Title))
		}
		if seeded[i.Number] {
			errs = append(errs, fmt.Errorf("issues: #%d is seeded twice", i.Number))
		}
		seeded[i.Number] = true
		if strings.TrimSpace(i.Title) == "" {
			errs = append(errs, fmt.Errorf("issues: #%d has no title", i.Number))
		}
	}
	seededPR := map[int]bool{}
	for _, p := range c.PullRequests {
		switch {
		case p.Number <= 0:
			errs = append(errs, fmt.Errorf("pull_requests: %q has no number", p.Title))
		case seeded[p.Number] || seededPR[p.Number]:
			// GitHub numbers issues and pull requests together, and so does
			// the GitHub the eval seeds.
			errs = append(errs, fmt.Errorf("pull_requests: #%d is already a seeded issue or pull request", p.Number))
		}
		seededPR[p.Number] = true
		if strings.TrimSpace(p.Title) == "" {
			errs = append(errs, fmt.Errorf("pull_requests: #%d has no title", p.Number))
		}
		if strings.TrimSpace(p.Head) == "" {
			errs = append(errs, fmt.Errorf("pull_requests: #%d has no head branch", p.Number))
		} else if p.Head == p.Base {
			errs = append(errs, fmt.Errorf("pull_requests: #%d is from %s into itself", p.Number, p.Head))
		}
		if p.Head != "" {
			if info, err := os.Stat(p.FilesDir(c)); err != nil || !info.IsDir() {
				errs = append(errs, fmt.Errorf("pull_requests: #%d: %s is not a directory: it holds the working tree of the %s branch",
					p.Number, p.FilesDir(c), p.Head))
			}
		}
		for _, r := range p.Reviews {
			if !slices.Contains(ReviewStates, r.State) {
				errs = append(errs, fmt.Errorf("pull_requests: #%d: review state %q is not one of %s", p.Number, r.State, strings.Join(ReviewStates, ", ")))
			}
		}
	}
	for i, p := range c.PullRequests {
		for _, q := range c.PullRequests[i+1:] {
			if p.Head != "" && p.Head == q.Head {
				errs = append(errs, fmt.Errorf("pull_requests: #%d and #%d are both from %s", p.Number, q.Number, p.Head))
			}
		}
	}
	for _, m := range c.Mail {
		if _, err := config.CanonicalRole(m.To); err != nil {
			errs = append(errs, fmt.Errorf("mail: to: %w", err))
		}
		if m.Issue != 0 && !seeded[m.Issue] {
			errs = append(errs, fmt.Errorf("mail: issue #%d is not a seeded issue", m.Issue))
		}
	}
	if c.Role == "" {
		return errors.Join(append(errs, c.validateFactory()...)...)
	}
	return errors.Join(append(errs, c.validateRole(seeded, seededPR)...)...)
}

// validateFactory checks the keys of a whole-factory case: it is graded by
// its test command, and the per-role keys are not its.
func (c Case) validateFactory() []error {
	var errs []error
	if strings.TrimSpace(c.Test) == "" {
		errs = append(errs, errors.New("test: the command that grades the case is required"))
	}
	if c.Issue != 0 || c.PR != 0 {
		errs = append(errs, errors.New("issue and pr belong to a per-role case: a whole-factory case works every issue it seeds"))
	}
	if !c.Expect.empty() {
		errs = append(errs, errors.New("expect belongs to a per-role case: a whole-factory case is graded by its test"))
	}
	return errs
}

// validateRole checks the keys of a per-role case: it is graded by what it
// declares under expect, and the session it runs has to have a subject the
// role can work on.
func (c Case) validateRole(seeded, seededPR map[int]bool) []error {
	var errs []error
	if strings.TrimSpace(c.Test) != "" {
		errs = append(errs, fmt.Errorf("test belongs to a whole-factory case: a %s case is graded by what it declares under expect", c.Role))
	}
	if c.Expect.empty() {
		errs = append(errs, errors.New("expect: a per-role case declares at least one check, or it grades nothing"))
	}
	switch c.Role {
	case config.RoleDeveloper, config.RoleReviewer:
		if c.Issue == 0 && c.PR == 0 {
			errs = append(errs, fmt.Errorf("issue: a %s case names the issue its session works on", c.Role))
		}
	default:
		if c.Issue != 0 || c.PR != 0 {
			errs = append(errs, fmt.Errorf("issue and pr: a %s session is about the whole repository, not one issue", c.Role))
		}
	}
	if c.Issue != 0 && !seeded[c.Issue] {
		errs = append(errs, fmt.Errorf("issue: #%d is not a seeded issue", c.Issue))
	}
	if c.PR != 0 && !seededPR[c.PR] {
		errs = append(errs, fmt.Errorf("pr: #%d is not a seeded pull request", c.PR))
	}
	for _, n := range c.Expect.issues() {
		if !seeded[n] {
			errs = append(errs, fmt.Errorf("expect: #%d is not a seeded issue", n))
		}
	}
	return append(errs, c.Expect.validate(c.Role)...)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
