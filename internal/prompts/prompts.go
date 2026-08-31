// Package prompts holds the base prompts for every role and renders them
// with project context. System prompts are appended to Claude Code's own
// system prompt; task prompts are the user message that starts a session.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
)

//go:embed system/*.md task/*.md partials/*.md
var files embed.FS

// consolidateTemplate is the name task prompts use to pull in the
// "consolidate your notes this session" paragraph:
//
//	{{template "consolidate" .}}
//
// It renders nothing unless Data.ConsolidateNotes is set, so the paragraph
// costs a session nothing until the scheduler asks for it.
const consolidateTemplate = "consolidate"

// Data is everything a prompt template can reference. Fields that do not
// apply to a role are left zero.
type Data struct {
	Role      string
	RoleTitle string
	Project   config.Project
	Filter    config.Filter
	Labels    config.Labels
	AutoMerge bool
	// CommitFlags are extra flags for the developer's git commits.
	CommitFlags string
	// Notify is the GitHub mention string for the people the factory turns
	// to when it needs one ("@kpenfound @myorg/bees-team"), or empty.
	Notify string
	// CreateFlags are the gh flags that make a new issue/PR visible to the
	// factory (label and, when configured, assignee).
	CreateFlags string

	WorkDir    string
	Branch     string
	StateDir   string
	SessionDir string
	NotesFile  string
	Notes      string
	// ConsolidateNotes asks the session to rewrite its notes file into the
	// standard sections on top of its normal work; ConsolidateReason says
	// why it is being asked now ("every 10 sessions", "file is 40 KB").
	ConsolidateNotes  bool
	ConsolidateReason string

	Inbox          []mail.Message
	PreviousRounds []mail.Message

	// Size is the work item's size ("xs", "s", "m", "l", "xl"), empty when
	// the issue carries no size label. Set for developer and reviewer
	// sessions.
	Size string
	// MaxSize is roles.developer.max_size: the largest size a developer
	// takes. Anything above it is sent back to triage to be split.
	MaxSize string

	Issue        *github.Issue
	PR           *github.PR
	Issues       []github.Issue
	TriageIssues []github.Issue
	PRs          []github.PR
	MergedPRs    []github.PR
	Milestones   []github.Milestone

	Round     int
	MaxRounds int
	LastRun   string
	// Retry is the number of times this session has already been attempted
	// and failed for infrastructure reasons (0 on a first attempt).
	Retry int

	// FailedChecks is set for the reviewer's checks-mode task.
	FailedChecks []github.Check
	// Checks are the pull request's checks as read just before the review,
	// ChecksStatus their summary ("passed", "pending"; empty when the
	// pre-review read was skipped, failed, or already happened for an earlier
	// review round) and ChecksTimeout how long a pending read waited before
	// reviewing anyway.
	Checks        []github.Check
	ChecksStatus  string
	ChecksTimeout string
	// Feedback holds bees:feedback issues awaiting the product manager.
	Feedback []github.Issue
	// Features holds every open feature issue; FreshFeatures the ones a
	// person created or commented on since the product manager last replied.
	Features      []github.Issue
	FreshFeatures []github.Issue
	// Proposals holds the fresh features that are still proposals: the
	// product manager's own ideas, which it may refine but must not break
	// into work items until a person removes bees:proposal. They are never
	// in FreshFeatures.
	Proposals []github.Issue
	// Planning holds the feature and feedback issues a person put in
	// planning mode (bees:planning): the product manager discusses them and
	// breaks nothing down. They are never in Feedback, FreshFeatures or
	// Proposals. Set for the product manager.
	Planning []github.Issue
	// Planned holds the issues a person agreed with the product manager
	// (bees:planned) that still need acting on: a feature with no sub-issues
	// yet, or an open feedback issue. A planned feature that already has
	// sub-issues has been broken down and is not listed again. Set for the
	// product manager.
	Planned []github.Issue
	// Progress maps a feature issue number to its sub-issue summary.
	Progress map[int]github.SubIssueSummary
	// CompletedFeatures holds the open features whose every sub-issue has
	// closed since the product manager last ran: its work is finished and the
	// only decision left is whether to close the feature. Set for the product
	// manager, and only for the run that first notices; a feature it looks at
	// and leaves open is not listed again until it gains a sub-issue and that
	// one closes too.
	CompletedFeatures []github.Issue
	// Stages are the reviewer's review stages (roles.reviewer.stages), in the
	// order to run them. Set for a reviewer review session; empty for every
	// other role and for the reviewer's checks-mode task, which diagnoses one
	// failure rather than reviewing.
	Stages []string
	// Parent is the feature a work item belongs to, when it is a sub-issue.
	// Set for a developer session, and for a reviewer session whose stages
	// include config.StageProductFit — the only stage that reads it.
	Parent *github.Parent
	// Parents maps an issue number to its parent feature, for the issues that
	// have one: the triage items (project manager) and the open work items
	// (product manager).
	Parents map[int]github.Parent
	// Blockers maps an issue number to the prerequisites it declares that
	// are still open (project manager).
	Blockers map[int][]int
}

var titles = map[string]string{
	config.RoleProductManager: "product manager",
	config.RoleProjectManager: "project manager",
	config.RoleDeveloper:      "developer",
	config.RoleReviewer:       "reviewer",
	config.RoleQA:             "QA engineer",
}

// Title returns the human name of a role.
func Title(role string) string {
	if t, ok := titles[role]; ok {
		return t
	}
	return role
}

// System renders the full system prompt for a role: the common preamble,
// the role's base prompt, and any custom text from bees.toml.
func System(role string, d Data, custom string) (string, error) {
	d.Role = role
	d.RoleTitle = Title(role)
	d.CreateFlags = CreateFlags(d.Filter)
	common, err := render("system/common.md", d)
	if err != nil {
		return "", err
	}
	base, err := render("system/"+role+".md", d)
	if err != nil {
		return "", err
	}
	parts := []string{common, base}
	if s := strings.TrimSpace(custom); s != "" {
		parts = append(parts, "## Additional instructions from bees.toml\n\n"+s)
	}
	return strings.Join(parts, "\n\n"), nil
}

// Task renders the task prompt for a role.
func Task(role string, d Data) (string, error) {
	return TaskNamed(role, role, d)
}

// TaskNamed renders the task prompt task/<name>.md for a role (used for
// role variants such as the reviewer's checks-mode task).
func TaskNamed(role, name string, d Data) (string, error) {
	d.Role = role
	d.RoleTitle = Title(role)
	d.CreateFlags = CreateFlags(d.Filter)
	return render("task/"+name+".md", d)
}

// CreateFlags returns the gh flags that make a newly created issue or PR
// visible to the factory.
func CreateFlags(f config.Filter) string {
	label := f.Label
	if label == "" {
		label = config.DefaultLabel
	}
	flags := fmt.Sprintf("--label %q", label)
	if f.Assignee != "" {
		flags += fmt.Sprintf(" --assignee %q", f.Assignee)
	}
	return flags
}

// BaseSystemPrompt returns the raw, unrendered base prompt for a role
// (used by `bees prompts show`).
func BaseSystemPrompt(role string) (string, error) {
	b, err := files.ReadFile("system/" + role + ".md")
	if err != nil {
		return "", fmt.Errorf("no base prompt for role %q", role)
	}
	return string(b), nil
}

func render(name string, d Data) (string, error) {
	src, err := files.ReadFile(name)
	if err != nil {
		return "", err
	}
	labels := d.Labels
	funcs := template.FuncMap{
		"formatMail": mail.Format,
		"labels": func(ls []github.Label) string {
			return strings.Join(github.LabelNames(ls), ", ")
		},
		"hasLabel": github.HasLabel,
		"progress": func(m map[int]github.SubIssueSummary, n int) string {
			p, ok := m[n]
			if !ok || p.Total == 0 {
				return "no work items"
			}
			return fmt.Sprintf("%d/%d done", p.Completed, p.Total)
		},
		"parentOf": func(m map[int]github.Parent, n int) string {
			p, ok := m[n]
			if !ok {
				return "-"
			}
			return fmt.Sprintf("#%d %s", p.Number, p.Title)
		},
		"blockedBy": func(m map[int][]int, n int) string {
			b := m[n]
			if len(b) == 0 {
				return "-"
			}
			refs := make([]string, 0, len(b))
			for _, x := range b {
				refs = append(refs, fmt.Sprintf("#%d", x))
			}
			return strings.Join(refs, ", ")
		},
		"stateLabel": func(ls []github.Label) string {
			for _, s := range labels.StateLabels() {
				if github.HasLabel(ls, s) {
					return strings.TrimPrefix(s, labels.Base+":")
				}
			}
			return "-"
		},
		"kindLabel": func(ls []github.Label) string {
			switch {
			case github.HasLabel(ls, labels.Feature):
				return "feature"
			case github.HasLabel(ls, labels.Bug):
				return "bug"
			}
			return "-"
		},
		"milestone": func(i github.Issue) string {
			if i.Milestone == nil {
				return "-"
			}
			return i.Milestone.Title
		},
		"oneline": func(s string) string {
			s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
			if len(s) > 120 {
				s = s[:117] + "..."
			}
			return s
		},
	}
	t := template.New(name).Funcs(funcs)
	partial, err := files.ReadFile("partials/" + consolidateTemplate + ".md")
	if err != nil {
		return "", err
	}
	if _, err := t.New(consolidateTemplate).Parse(string(partial)); err != nil {
		return "", fmt.Errorf("prompt %s: %w", consolidateTemplate, err)
	}
	if _, err := t.Parse(string(src)); err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	return strings.TrimSpace(buf.String()) + "\n", nil
}
