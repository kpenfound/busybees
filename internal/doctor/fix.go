package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/busybees/internal/github"
)

// fixFilter is the repair `bees doctor --fix` applies to checkFilter: it
// brings the open items that carry the base label but fall outside the rest of
// the filter back into it, by adding filter.assignee and, when one is
// configured, filter.milestone.
//
// The safety rule of busybees is the whole design here: **an item that does
// not carry the base label is never touched**. That is what makes bees safe in
// a repository it shares with people, and a repair command is exactly where it
// would get quietly broken - so selection is on the label alone, listed from
// GitHub under the label and checked again per item before any write.
// Selection is never on the author or on the `<!-- bees:<role> -->` marker: a
// feature issue a person filed with the base label and no assignee is the case
// this exists for (#119).
//
// It never adds or removes a label. With `filter.require_label = false` there
// is no base label to trust, so it does nothing at all and says why: "label
// every issue in the repository" is not a repair.
//
// Unlike the scheduler's visibility backstop (Scheduler.ensureVisible), which
// only milestones pull requests, this sets the milestone on issues too. The
// backstop keeps out of a person's decision about an unrelated issue; --fix is
// asked, by a person, to make the configured filter match the labelled work,
// and an issue outside filter.milestone stays invisible until it is in it.
func (d *Deps) fixFilter(ctx context.Context) ([]string, error) {
	f := d.Config.Filter
	if !f.LabelRequired() {
		return []string{"filter.require_label is false: with no base label there is no way to tell the factory's " +
			"items from the rest of the repository, and adopting everything is not a repair — nothing was changed"}, nil
	}
	q := Query(d.Config)
	base := github.Query{Label: f.Label}
	if base == q {
		return []string{fmt.Sprintf("the filter is the `%s` label alone: nothing carrying it can be outside the filter", f.Label)}, nil
	}

	issues, err := d.GitHub.ListOpenIssues(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("list open issues carrying `%s`: %w", f.Label, err)
	}
	prs, err := d.GitHub.ListOpenPRs(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("list open pull requests carrying `%s`: %w", f.Label, err)
	}

	assignee, err := d.assignee(ctx)
	if err != nil {
		return nil, err
	}
	// github.Query.Matches deliberately lets "@me" match any assignee (it
	// cannot know the login); the resolved one is what tells an adopted item
	// from an already-visible one.
	q.Assignee = assignee

	a := &adopter{deps: d, query: q, label: f.Label, assignee: assignee, milestone: f.Milestone}
	for _, i := range issues {
		a.adopt(ctx, i.Number, "issue", i.Labels, i.Assignees, i.MilestoneTitle())
	}
	for _, p := range prs {
		a.adopt(ctx, p.Number, "pull request", p.Labels, p.Assignees, p.MilestoneTitle())
	}
	if len(a.actions) == 0 && len(a.errs) == 0 {
		a.actions = append(a.actions,
			fmt.Sprintf("nothing to adopt: every open item carrying `%s` already matches the filter", f.Label))
	}
	return a.actions, errors.Join(a.errs...)
}

// assignee resolves filter.assignee to the login the REST endpoint wants.
// "@me" is a gh query shorthand, not a login: `bees run` resolves it at
// startup, and doctor has to do the same before it can assign anything - with
// the person's own gh authentication (Deps.me), because "@me" means the
// person whatever account [github] makes the factory act as. Resolving it
// through d.gh would assign the factory's work to the bot, where the
// orchestrator cannot see it.
func (d *Deps) assignee(ctx context.Context) (string, error) {
	login := d.Config.Filter.Assignee
	if login != "@me" {
		return login, nil
	}
	out, err := d.me(ctx)
	if err != nil {
		return "", fmt.Errorf(`resolve filter.assignee="@me": %w`, err)
	}
	login = strings.TrimSpace(out)
	if login == "" {
		return "", errors.New(`resolve filter.assignee="@me": gh reported no login`)
	}
	return login, nil
}

// adopter applies the filter to one item at a time, collecting what it did and
// what it could not do. One item that cannot be assigned must not strand the
// rest, so every failure is recorded and the loop goes on.
type adopter struct {
	deps      *Deps
	query     github.Query
	label     string
	assignee  string
	milestone string
	actions   []string
	errs      []error
}

func (a *adopter) adopt(ctx context.Context, number int, kind string, labels []github.Label, assignees []github.Author, milestone string) {
	// The safety rule, checked against the item itself rather than trusting
	// that the listing honoured --label.
	if !github.HasLabel(labels, a.label) {
		return
	}
	if a.query.Matches(labels, assignees, milestone) {
		return
	}
	if a.assignee != "" && !github.HasAssignee(assignees, a.assignee) {
		if err := a.deps.GitHub.Assign(ctx, number, a.assignee); err != nil {
			a.errs = append(a.errs, fmt.Errorf("%s #%d: assign to %s: %w", kind, number, a.assignee, err))
		} else {
			a.actions = append(a.actions, fmt.Sprintf("assigned %s #%d to %s", kind, number, a.assignee))
		}
	}
	if a.milestone != "" && milestone != a.milestone {
		if err := a.deps.GitHub.SetMilestone(ctx, number, a.milestone); err != nil {
			a.errs = append(a.errs, fmt.Errorf("%s #%d: set milestone %s: %w", kind, number, a.milestone, err))
		} else {
			a.actions = append(a.actions, fmt.Sprintf("put %s #%d in milestone %s", kind, number, a.milestone))
		}
	}
}
