package eval

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/fakegh"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/text"
)

// Expect is how a per-role case is graded: what the state has to look like
// once the role's session has run, and the case passes when every check it
// declared passes. A case declares only the keys it is about. Most become
// one Check each; the ones that take a list become one per entry, so that
// each label, each closed issue and each pull request stands or falls on
// its own. The rubrics under Graded are decided by a session of their own
// (grader.go); the rest are read off the end state here.
type Expect struct {
	// Outcome is the status the session had to report with `done`.
	Outcome string `toml:"outcome"`
	// Labels are the labels issues had to end up carrying, and the ones
	// they had to be rid of.
	Labels []ExpectLabels `toml:"labels"`
	// Mail is the messages the role had to send.
	Mail []ExpectMail `toml:"mail"`
	// IssuesCreated is how many issues the role had to open: the issues
	// there are at the end that the case did not seed. Nil is no check.
	IssuesCreated *int `toml:"issues_created"`
	// IssuesClosed are the seeded issues it had to close.
	IssuesClosed []int `toml:"issues_closed"`
	// PullRequests are the seeded issues that had to get a pull request.
	PullRequests []int `toml:"pull_requests"`
	// Graded are the checks a grader session decides.
	Graded []Rubric `toml:"graded"`
}

// ExpectLabels is what one issue's labels had to become.
type ExpectLabels struct {
	Issue int `toml:"issue"`
	// Has are the labels the issue carries at the end, Missing the ones it
	// does not.
	Has     []string `toml:"has"`
	Missing []string `toml:"missing"`
}

// ExpectMail is one message the role had to send: to a role, and about a
// seeded issue when Issue is set.
type ExpectMail struct {
	To    string `toml:"to"`
	Issue int    `toml:"issue"`
}

// empty reports whether the case declared no expectation at all.
func (e Expect) empty() bool {
	return e.Outcome == "" && len(e.Labels) == 0 && len(e.Mail) == 0 && e.IssuesCreated == nil &&
		len(e.IssuesClosed) == 0 && len(e.PullRequests) == 0 && len(e.Graded) == 0
}

// issues lists every seeded issue the expectations name, so the case can
// refuse one that names an issue it does not seed.
func (e Expect) issues() []int {
	var out []int
	for _, l := range e.Labels {
		if l.Issue != 0 {
			out = append(out, l.Issue)
		}
	}
	for _, m := range e.Mail {
		if m.Issue != 0 {
			out = append(out, m.Issue)
		}
	}
	return append(append(out, e.IssuesClosed...), e.PullRequests...)
}

// validate checks the expectations against the role they are declared for.
func (e Expect) validate(role string) []error {
	var errs []error
	if e.Outcome != "" {
		if err := session.ValidateOutcome(role, e.Outcome); err != nil {
			errs = append(errs, fmt.Errorf("expect.outcome: %w", err))
		}
	}
	for _, l := range e.Labels {
		if l.Issue <= 0 {
			errs = append(errs, errors.New("expect.labels: issue: name the issue whose labels to check"))
		}
		if len(l.Has) == 0 && len(l.Missing) == 0 {
			errs = append(errs, fmt.Errorf("expect.labels: #%d names no label to check", l.Issue))
		}
	}
	for _, m := range e.Mail {
		if _, err := config.CanonicalRole(m.To); err != nil {
			errs = append(errs, fmt.Errorf("expect.mail: to: %w", err))
		}
	}
	if e.IssuesCreated != nil && *e.IssuesCreated < 0 {
		errs = append(errs, errors.New("expect.issues_created must not be negative"))
	}
	for _, r := range e.Graded {
		errs = append(errs, r.validate()...)
	}
	return errs
}

// expected is the mechanical part of grading a per-role case: one Check for
// every expectation it declared, read off the fake GitHub, the mailbox and
// the ledger.
func (f *caseFactory) expected(c Case) []Check {
	e := c.Expect
	snap := f.gh.Snapshot()
	var checks []Check
	if e.Outcome != "" {
		checks = append(checks, f.outcomeCheck(c.Role, e.Outcome))
	}
	for _, want := range e.Labels {
		checks = append(checks, labelChecks(snap, want)...)
	}
	for _, want := range e.Mail {
		checks = append(checks, f.mailCheck(c.Role, want))
	}
	if e.IssuesCreated != nil {
		checks = append(checks, createdCheck(snap, c, *e.IssuesCreated))
	}
	for _, n := range e.IssuesClosed {
		checks = append(checks, f.closedCheck(snap, n))
	}
	for _, n := range e.PullRequests {
		checks = append(checks, f.prCheck(snap, n))
	}
	return checks
}

// outcomeCheck compares what the role's last session of this run reported
// with what the case asked for.
func (f *caseFactory) outcomeCheck(role, want string) Check {
	got := f.lastOutcome(role)
	check := Check{
		Name:    fmt.Sprintf("the session reported %q", want),
		Failure: fmt.Sprintf("the session reported %q, not %q", got, want),
		Pass:    got == want,
	}
	if got == "" {
		check.Failure = fmt.Sprintf("no %s session reported an outcome, let alone %q", role, want)
	}
	return check
}

// lastOutcome is what the role's last session of this run reported, and ""
// when no session of the role finished.
func (f *caseFactory) lastOutcome(role string) string {
	entries, err := f.store.ReadLedger(time.Time{})
	if err != nil {
		f.log.Warn("could not read the ledger", "err", err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == role {
			return entries[i].Outcome
		}
	}
	return ""
}

// labelChecks is one check per label the case named, so each requirement
// stands or falls on its own.
func labelChecks(snap fakegh.Snapshot, want ExpectLabels) []Check {
	issue, ok := snap.Issue(want.Issue)
	carries := strings.Join(labelNames(issue), ", ")
	if !ok {
		carries = "the issue is not there"
	}
	var checks []Check
	for _, l := range want.Has {
		checks = append(checks, Check{
			Name:    fmt.Sprintf("#%d carries %s", want.Issue, l),
			Failure: fmt.Sprintf("#%d does not carry %s", want.Issue, l),
			Pass:    ok && github.HasLabel(issue.Labels, l),
			Detail:  carries,
		})
	}
	for _, l := range want.Missing {
		checks = append(checks, Check{
			Name:    fmt.Sprintf("#%d no longer carries %s", want.Issue, l),
			Failure: fmt.Sprintf("#%d still carries %s", want.Issue, l),
			Pass:    ok && !github.HasLabel(issue.Labels, l),
			Detail:  carries,
		})
	}
	return checks
}

func labelNames(i github.Issue) []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// mailCheck looks for a message the role sent to the role the case named,
// about the issue it named when it named one.
func (f *caseFactory) mailCheck(role string, want ExpectMail) Check {
	to, _ := config.CanonicalRole(want.To)
	about := ""
	if want.Issue != 0 {
		about = fmt.Sprintf(" about #%d", want.Issue)
	}
	check := Check{
		Name:    fmt.Sprintf("mail to the %s%s", to, about),
		Failure: fmt.Sprintf("the %s sent the %s no mail%s", role, to, about),
	}
	msgs, err := f.box.List(mail.Filter{From: role, To: to})
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	for _, m := range msgs {
		if want.Issue != 0 && ghwork.Issue(m.Work) != want.Issue {
			continue
		}
		check.Pass, check.Detail = true, m.Subject
		break
	}
	return check
}

// createdCheck counts the issues the run opened: every issue there is now
// that the case did not seed.
func createdCheck(snap fakegh.Snapshot, c Case, want int) Check {
	seeded := map[int]bool{}
	for _, i := range c.Issues {
		seeded[i.Number] = true
	}
	var opened []string
	for _, i := range snap.Issues {
		if !seeded[i.Number] {
			opened = append(opened, fmt.Sprintf("#%d %s", i.Number, i.Title))
		}
	}
	return Check{
		Name:    text.Count(want, "issue") + " created",
		Failure: fmt.Sprintf("the run created %s, not %d", text.Count(len(opened), "issue"), want),
		Pass:    len(opened) == want,
		Detail:  strings.Join(opened, "; "),
	}
}
