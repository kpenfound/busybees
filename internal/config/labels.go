package config

// Labels are the GitHub labels the factory uses as its workflow state
// machine. Every label is derived from the configured visibility label so
// several factories can coexist in one repository.
type Labels struct {
	// Base is the visibility label ("bees"). Only issues and PRs carrying it
	// are visible to the factory.
	Base string

	// Kind labels.
	// Feature marks a feature issue: owned by the product manager, who makes
	// it detailed enough, breaks it into work items and closes it when they
	// ship. Outside the workflow state machine.
	Feature string // bees:feature
	Bug     string // bees:bug     – a bug work item (developer, reviewer, QA or a human)
	// Feedback marks the product manager's inbox: feature ideas, product
	// feedback and bug reports from humans. Exempt from the workflow state
	// machine; the product manager turns them into feature/bug issues.
	Feedback string // bees:feedback
	// Question marks a feature or feedback issue where the product manager
	// is waiting for a person to answer. Removed when the person replies.
	Question string // bees:question

	// Issue workflow state labels (exactly one at a time).
	Triage     string // bees:triage      – needs project manager refinement
	Ready      string // bees:ready       – detailed enough, waiting for a developer
	InProgress string // bees:in-progress – a developer worker owns it
	Blocked    string // bees:blocked     – waiting on an answer to a question
	Review     string // bees:review      – PR open, reviewer loop running
	Approved   string // bees:approved    – reviewer approved, waiting for merge
	NeedsHuman string // bees:needs-human – the factory gave up; a person must step in
}

// LabelsFor derives the label set from the base visibility label.
func LabelsFor(base string) Labels {
	return Labels{
		Base:       base,
		Feature:    base + ":feature",
		Bug:        base + ":bug",
		Feedback:   base + ":feedback",
		Question:   base + ":question",
		Triage:     base + ":triage",
		Ready:      base + ":ready",
		InProgress: base + ":in-progress",
		Blocked:    base + ":blocked",
		Review:     base + ":review",
		Approved:   base + ":approved",
		NeedsHuman: base + ":needs-human",
	}
}

// Labels returns the label set for this configuration.
func (c *Config) Labels() Labels { return LabelsFor(c.Filter.Label) }

// StateLabels lists the mutually exclusive workflow state labels.
func (l Labels) StateLabels() []string {
	return []string{l.Triage, l.Ready, l.InProgress, l.Blocked, l.Review, l.Approved, l.NeedsHuman}
}

// LabelSpec describes a label for creation in GitHub.
type LabelSpec struct {
	Name, Color, Description string
}

// All returns every label with a colour and description, for `bees init`.
func (l Labels) All() []LabelSpec {
	return []LabelSpec{
		{l.Base, "FBCA04", "Visible to the busybees software factory"},
		{l.Feature, "1D76DB", "Feature issue owned by the product manager; broken into work items"},
		{l.Bug, "D73A4A", "Bug work item"},
		{l.Feedback, "C5DEF5", "Feature idea, product feedback or bug report for the product manager"},
		{l.Question, "BFD4F2", "The product manager is waiting for a person to answer"},
		{l.Triage, "E4E669", "Needs refinement by the project manager"},
		{l.Ready, "0E8A16", "Detailed enough for a developer to pick up"},
		{l.InProgress, "5319E7", "A developer is working on it"},
		{l.Blocked, "B60205", "Waiting on an answer to a question"},
		{l.Review, "FBCA04", "Pull request open and under review"},
		{l.Approved, "0E8A16", "Reviewer approved; waiting for merge"},
		{l.NeedsHuman, "000000", "The factory needs a human to step in"},
	}
}
