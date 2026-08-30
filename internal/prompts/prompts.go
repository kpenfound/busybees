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

//go:embed system/*.md task/*.md
var files embed.FS

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
	// CreateFlags are the gh flags that make a new issue/PR visible to the
	// factory (label and, when configured, assignee).
	CreateFlags string

	WorkDir    string
	Branch     string
	StateDir   string
	SessionDir string
	NotesFile  string
	Notes      string

	Inbox          []mail.Message
	PreviousRounds []mail.Message

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
	// Checks are the pull request's required checks as read just before the
	// review, ChecksStatus their summary ("passed", "pending"; empty when the
	// pre-review read was skipped) and ChecksTimeout how long a pending read
	// waited before giving up.
	Checks        []github.Check
	ChecksStatus  string
	ChecksTimeout string
	// Feedback holds bees:feedback issues awaiting the product manager.
	Feedback []github.Issue
	// Features holds every open feature issue; FreshFeatures the ones a
	// person created or commented on since the product manager last replied.
	Features      []github.Issue
	FreshFeatures []github.Issue
	// Progress maps a feature issue number to its sub-issue summary.
	Progress map[int]github.SubIssueSummary
	// Parent is the feature a work item belongs to, when it is a sub-issue.
	Parent *github.Parent
	// Parents maps work item numbers to their parent feature (project manager).
	Parents map[int]github.Parent
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
	t, err := template.New(name).Funcs(funcs).Parse(string(src))
	if err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	return strings.TrimSpace(buf.String()) + "\n", nil
}
