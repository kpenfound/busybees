// Package eval is `bees eval`: it runs the whole factory against a fixture
// repository and grades the result mechanically, SWE-bench Lite style.
//
// A case is a directory under evals/ (case.go): a fixture repository, the
// GitHub state and mail to seed, and a test command that fails on the
// fixture and must pass once the factory has worked the seeded issues. The
// runner (run.go) builds the fixture as a local bare origin, seeds an
// in-memory GitHub (internal/fakegh), and runs the scheduler pass after
// pass, merging the pull requests it approved the way a person would, until
// every seeded issue is closed or held for a person, or the case's budget or
// timeout runs out. Sessions reach that GitHub through a gh of their own on
// PATH (shim.go). Grading (run.go's grade) runs the test command on the
// default branch and checks that every seeded issue closed with a pull
// request, and the report (report.go) is a table and a JSON file per run.
//
// Which agent profiles the sessions run on is profile.go's: --profile, or
// the profiles bees.toml selects, or the person's global config.toml.
package eval

import (
	"errors"
	"fmt"
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
)

// Defaults for the keys a case leaves out.
const (
	DefaultTimeout = time.Hour
	DefaultMaxCost = 10.0
	// DefaultAuthor is who a seeded issue, comment or mail is from.
	DefaultAuthor = "human"
)

// Case is one whole-factory eval: evals/<name>/case.toml and the fixture
// beside it.
type Case struct {
	// Name is the case directory's name, and Dir its absolute path.
	Name string `toml:"-"`
	Dir  string `toml:"-"`

	Description string `toml:"description"`
	// Test grades the case: a command run with sh -c in a checkout of the
	// default branch, which must fail on the fixture and pass after the
	// run.
	Test string `toml:"test"`
	// Timeout and MaxCost (USD) stop the run: the factory is stopped, the
	// sessions still running with it, and the case graded as it stands.
	Timeout config.Duration `toml:"timeout"`
	MaxCost float64         `toml:"max_cost"`
	Issues  []Issue         `toml:"issues"`
	Mail    []Mail          `toml:"mail"`
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

// Comment is one comment on a seeded issue.
type Comment struct {
	Author string `toml:"author"`
	Body   string `toml:"body"`
}

// Mail is one message in a role's mailbox when the run starts.
type Mail struct {
	From    string `toml:"from"`
	To      string `toml:"to"`
	Subject string `toml:"subject"`
	Body    string `toml:"body"`
	Issue   int    `toml:"issue"`
}

// LoadCases reads the cases under dir, in name order: every one, or only
// the one called name. A directory named after a role is left out: it holds
// that role's cases, which a whole-factory run does not take.
func LoadCases(dir, name string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("eval cases: %w", err)
	}
	var cases []Case
	var names []string
	for _, e := range entries {
		if !e.IsDir() || slices.Contains(config.Roles, e.Name()) {
			continue
		}
		names = append(names, e.Name())
		if name != "" && e.Name() != name {
			continue
		}
		c, err := LoadCase(filepath.Join(dir, e.Name()))
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

// LoadCase reads the case in dir and checks it.
func LoadCase(dir string) (Case, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Case{}, err
	}
	path := filepath.Join(abs, CaseFile)
	c := Case{Name: filepath.Base(abs), Dir: abs}
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
	if err := c.validate(); err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c Case) validate() error {
	var errs []error
	if strings.TrimSpace(c.Test) == "" {
		errs = append(errs, errors.New("test: the command that grades the case is required"))
	}
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
	seen := map[int]bool{}
	for _, i := range c.Issues {
		if i.Number <= 0 {
			errs = append(errs, fmt.Errorf("issues: %q has no number", i.Title))
		}
		if seen[i.Number] {
			errs = append(errs, fmt.Errorf("issues: #%d is seeded twice", i.Number))
		}
		seen[i.Number] = true
		if strings.TrimSpace(i.Title) == "" {
			errs = append(errs, fmt.Errorf("issues: #%d has no title", i.Number))
		}
	}
	for _, m := range c.Mail {
		if _, err := config.CanonicalRole(m.To); err != nil {
			errs = append(errs, fmt.Errorf("mail: to: %w", err))
		}
		if m.Issue != 0 && !seen[m.Issue] {
			errs = append(errs, fmt.Errorf("mail: issue #%d is not a seeded issue", m.Issue))
		}
	}
	return errors.Join(errs...)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
