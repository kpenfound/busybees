// Package issues creates issues the way the factory wants them: visible to
// the filter, labelled by kind and state, attached to a parent feature as a
// GitHub sub-issue, and in the milestone of the issue they relate to.
// Milestones themselves are managed by people; the factory only inherits.
package issues

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// Kind of issue to create.
type Kind string

// Issue kinds.
const (
	// KindTask is a work item: it enters triage.
	KindTask Kind = "task"
	// KindBug is a bug work item: it enters triage.
	KindBug Kind = "bug"
	// KindFeature is a feature issue owned by the product manager: no state label.
	KindFeature Kind = "feature"
)

// Options for Create.
type Options struct {
	Title string
	Body  string
	Kind  Kind
	// Parent makes the new issue a sub-issue of Parent and inherits its milestone.
	Parent int
	// Related only inherits the milestone of Related (a bug found while
	// working on it, a feature distilled from a feedback issue, ...).
	Related int
	// Milestone overrides the inherited milestone.
	Milestone string
	// ExtraLabels are added on top of the factory's labels.
	ExtraLabels []string
	// Ready puts a task or bug straight into ready instead of triage.
	Ready bool
	// BlockedBy are issues the new one must not be built before. They are
	// written into the body as a "Blocked by #N" line, which the scheduler
	// honours when it dispatches developers. No GitHub relationship is
	// created.
	BlockedBy []int
}

// Result describes a created issue.
type Result struct {
	Number    int
	Milestone string
	Parent    int
	Labels    []string
}

// String renders a created issue the way both `bees issue create` and the
// issue_create tool report it.
func (r Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "created #%d", r.Number)
	if r.Parent > 0 {
		fmt.Fprintf(&b, " (sub-issue of #%d)", r.Parent)
	}
	if r.Milestone != "" {
		fmt.Fprintf(&b, " milestone %q", r.Milestone)
	}
	return b.String()
}

// Create creates an issue according to opts.
func Create(ctx context.Context, gh *github.Client, filter config.Filter, labels config.Labels, opts Options) (Result, error) {
	if strings.TrimSpace(opts.Title) == "" {
		return Result{}, errors.New("title is required")
	}
	if opts.Parent > 0 && opts.Related > 0 {
		return Result{}, errors.New("use either --parent or --related, not both")
	}
	n := github.NewIssue{Title: opts.Title, Body: blockedByBody(opts.BlockedBy, opts.Body), Labels: []string{labels.Base}}
	switch opts.Kind {
	case KindFeature:
		// A bee proposes; a person approves by removing bees:proposal.
		n.Labels = append(n.Labels, labels.Feature, labels.Proposal)
	case KindBug:
		n.Labels = append(n.Labels, labels.Bug, state(labels, opts.Ready))
	case KindTask, "":
		n.Labels = append(n.Labels, state(labels, opts.Ready))
	default:
		return Result{}, fmt.Errorf("unknown kind %q (want task, bug or feature)", opts.Kind)
	}
	n.Labels = append(n.Labels, opts.ExtraLabels...)
	if filter.Assignee != "" {
		n.Assignees = []string{filter.Assignee}
	}

	// Milestone: explicit, else inherited from the parent/related issue,
	// else the filter's milestone (so the issue stays visible).
	// The parent's details answer two questions in one API call: whether it
	// is a proposal (which must not grow work items yet) and which milestone
	// the new issue inherits. A parent is therefore always fetched, even when
	// the milestone is already known.
	n.Milestone = opts.Milestone
	source := opts.Parent
	if source == 0 {
		source = opts.Related
	}
	if source > 0 && (opts.Parent > 0 || n.Milestone == "") {
		d, err := gh.GetIssueDetails(ctx, source)
		if err != nil {
			return Result{}, fmt.Errorf("issue #%d: %w", source, err)
		}
		if opts.Parent > 0 && github.HasLabel(d.Labels, labels.Proposal) {
			return Result{}, proposalError(labels, opts.Parent)
		}
		if n.Milestone == "" {
			n.Milestone = d.MilestoneTitle()
		}
	}
	if n.Milestone == "" {
		n.Milestone = filter.Milestone
	}

	number, err := gh.CreateIssue(ctx, n)
	if err != nil {
		return Result{}, err
	}
	res := Result{Number: number, Milestone: n.Milestone, Parent: opts.Parent, Labels: n.Labels}
	if opts.Parent > 0 {
		// The parent was already checked above, so link without asking
		// GitHub for its labels a second time.
		if err := link(ctx, gh, opts.Parent, number); err != nil {
			return res, fmt.Errorf("issue #%d created but could not be attached to #%d: %w", number, opts.Parent, err)
		}
	}
	return res, nil
}

// Link attaches child to parent as a sub-issue. It refuses a parent that is
// still a proposal: attaching an existing issue to one is the same hole as
// creating a sub-issue under it.
func Link(ctx context.Context, gh *github.Client, labels config.Labels, parent, child int) error {
	p, err := gh.GetIssueDetails(ctx, parent)
	if err != nil {
		return fmt.Errorf("issue #%d: %w", parent, err)
	}
	if github.HasLabel(p.Labels, labels.Proposal) {
		return proposalError(labels, parent)
	}
	return link(ctx, gh, parent, child)
}

// link attaches child to parent without checking the parent.
func link(ctx context.Context, gh *github.Client, parent, child int) error {
	d, err := gh.GetIssueDetails(ctx, child)
	if err != nil {
		return err
	}
	return gh.AddSubIssue(ctx, parent, d.ID)
}

// proposalError is the refusal every path into a proposal's sub-issues
// shares: a proposal is a bee's own idea, and only a person can turn it into
// work by removing the label.
func proposalError(labels config.Labels, number int) error {
	return fmt.Errorf("#%d is a proposal: a person must approve it (remove the %s label) before it can be broken into work items", number, labels.Proposal)
}

// blockedByBody prefixes body with the "Blocked by #N" line the scheduler
// parses (github.Blockers), followed by a blank line.
func blockedByBody(blockedBy []int, body string) string {
	if len(blockedBy) == 0 {
		return body
	}
	refs := make([]string, 0, len(blockedBy))
	for _, n := range blockedBy {
		refs = append(refs, fmt.Sprintf("#%d", n))
	}
	return fmt.Sprintf("Blocked by %s\n\n%s", strings.Join(refs, ", "), body)
}

func state(labels config.Labels, ready bool) string {
	if ready {
		return labels.Ready
	}
	return labels.Triage
}
