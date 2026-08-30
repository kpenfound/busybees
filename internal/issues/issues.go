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
		n.Labels = append(n.Labels, labels.Feature)
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
	n.Milestone = opts.Milestone
	source := opts.Parent
	if source == 0 {
		source = opts.Related
	}
	if n.Milestone == "" && source > 0 {
		d, err := gh.GetIssueDetails(ctx, source)
		if err != nil {
			return Result{}, fmt.Errorf("issue #%d: %w", source, err)
		}
		n.Milestone = d.MilestoneTitle()
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
		if err := Link(ctx, gh, opts.Parent, number); err != nil {
			return res, fmt.Errorf("issue #%d created but could not be attached to #%d: %w", number, opts.Parent, err)
		}
	}
	return res, nil
}

// Link attaches child to parent as a sub-issue.
func Link(ctx context.Context, gh *github.Client, parent, child int) error {
	d, err := gh.GetIssueDetails(ctx, child)
	if err != nil {
		return err
	}
	return gh.AddSubIssue(ctx, parent, d.ID)
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
