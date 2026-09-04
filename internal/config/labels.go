package config

import "slices"

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
	// Bug marks a bug work item (filed by the developer, reviewer, QA or a
	// human). It says what the issue is, not where it goes: only Feature and
	// Feedback route an issue out of the state machine, so a bug with no
	// state label is read as feedback for the product manager.
	Bug string // bees:bug
	// Feedback marks the product manager's inbox: feature ideas, product
	// feedback and bug reports from humans. Exempt from the workflow state
	// machine; the product manager turns them into feature/bug issues.
	Feedback string // bees:feedback
	// Question marks a feature or feedback issue where the product manager
	// is waiting for a person to answer. Removed when the person replies.
	Question string // bees:question
	// Proposal marks a feature issue a bee wrote rather than a person. It
	// sits *next to* bees:feature rather than replacing it, and it is not a
	// state label. Only a person removes it, and that removal is the
	// approval to break the feature into work items.
	Proposal string // bees:proposal
	// Planning marks a feature or feedback issue a person and the product
	// manager are still agreeing on. While it is there the product manager
	// only discusses the issue: it replies to every fresh comment with
	// questions, options or a draft, and breaks nothing down. Not a state
	// label — an issue in planning keeps whatever state it has. Only a
	// person adds or removes it.
	Planning string // bees:planning
	// Planned marks an issue a person and the product manager have agreed:
	// the person ends planning by swapping Planning for it, and the product
	// manager's next run treats the issue as settled and breaks it down.
	// Not a state label. Only a person adds or removes it.
	Planned string // bees:planned
	// Priority marks work a person wants built next. It is not a state
	// label and nothing in the factory removes it. Two roles may add it:
	// the project manager, to a work item that unblocks the factory
	// itself, and the product manager, carrying a person's from a feedback
	// issue onto the work item it creates from it. Dispatch takes priority
	// issues out of the ready queue before anything else, and is the only
	// thing that reads it.
	Priority string // bees:priority
	// ReviewRequested goes on a pull request, not an issue: a person puts it
	// on any open pull request the factory can see to ask the reviewer for
	// one review pass, whoever opened the pull request. Not a state label.
	// The orchestrator removes it as it starts the session, so one label is
	// one pass, served even when the session fails, and adding it again asks
	// for another.
	ReviewRequested string // bees:review-requested

	// Issue workflow state labels (one at a time, except while a person
	// holds an issue with NeedsHuman on top of one — see StateLabels).
	Triage     string // bees:triage      – needs project manager refinement
	Ready      string // bees:ready       – detailed enough, waiting for a developer
	InProgress string // bees:in-progress – a developer worker owns it
	Blocked    string // bees:blocked     – waiting on an answer to a question
	Review     string // bees:review      – PR open, reviewer loop running
	Approved   string // bees:approved    – reviewer approved, waiting for merge
	NeedsHuman string // bees:needs-human – the factory gave up, or a person holds it

	// Size labels (at most one at a time, orthogonal to the state labels).
	// The project manager sets one when it moves a work item to Ready.
	SizeXS string // bees:size/xs – one file, obvious change, no design
	SizeS  string // bees:size/s  – a few files, clear approach, existing tests cover it
	SizeM  string // bees:size/m  – a feature slice across packages, needs new tests
	SizeL  string // bees:size/l  – crosses subsystems or needs a design decision
	SizeXL string // bees:size/xl – too big for one pull request; split it instead
}

// LabelsFor derives the label set from the base visibility label.
func LabelsFor(base string) Labels {
	return Labels{
		Base:            base,
		Feature:         base + ":feature",
		Bug:             base + ":bug",
		Feedback:        base + ":feedback",
		Question:        base + ":question",
		Proposal:        base + ":proposal",
		Planning:        base + ":planning",
		Planned:         base + ":planned",
		Priority:        base + ":priority",
		ReviewRequested: base + ":review-requested",
		Triage:          base + ":triage",
		Ready:           base + ":ready",
		InProgress:      base + ":in-progress",
		Blocked:         base + ":blocked",
		Review:          base + ":review",
		Approved:        base + ":approved",
		NeedsHuman:      base + ":needs-human",
		SizeXS:          base + ":size/xs",
		SizeS:           base + ":size/s",
		SizeM:           base + ":size/m",
		SizeL:           base + ":size/l",
		SizeXL:          base + ":size/xl",
	}
}

// Labels returns the label set for this configuration.
func (c *Config) Labels() Labels { return LabelsFor(c.Filter.Label) }

// StateLabels lists the mutually exclusive workflow state labels in
// PRECEDENCE order, not in workflow order: every caller derives an issue's
// state by taking the first label in this list that the issue carries.
//
// NeedsHuman comes first so that a person can park an issue by adding
// bees:needs-human from the GitHub issue list without also removing the
// state label underneath it. Adding a label there does not remove another
// one, so an issue held that way carries two state labels, and the hold
// only works while it wins. Removing bees:needs-human hands the issue back
// to whatever state label is still on it.
//
// All and the live view's queueOrder deliberately keep workflow order:
// neither derives a state, and both are read by a person.
func (l Labels) StateLabels() []string {
	return []string{l.NeedsHuman, l.Triage, l.Ready, l.InProgress, l.Blocked, l.Review, l.Approved}
}

// SizeLabels lists the size labels, smallest first. An issue carries at
// most one of them, independently of its state label.
func (l Labels) SizeLabels() []string {
	return []string{l.SizeXS, l.SizeS, l.SizeM, l.SizeL, l.SizeXL}
}

// SizeLabel returns the label for a short size name ("s" -> "bees:size/s"),
// or "" when size is not one of Sizes. It is the inverse of the trimming
// callers do to turn a label back into a size.
func (l Labels) SizeLabel(size string) string {
	i := slices.Index(Sizes, size)
	if i < 0 {
		return ""
	}
	return l.SizeLabels()[i]
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
		{l.Proposal, "D4C5F9", "Feature issue a bee proposed; a person must approve it before breakdown"},
		{l.Planning, "6F42C1", "A person and the product manager are agreeing this issue; discussion only, no breakdown"},
		{l.Planned, "0052CC", "Agreed with a person: the product manager breaks it down on its next run"},
		{l.Priority, "E99695", "A person wants this next: dispatched before the rest of the ready queue"},
		{l.ReviewRequested, "F9D0C4", "On a pull request: a person asks the reviewer for one review pass; removed when it starts"},
		{l.Triage, "E4E669", "Needs refinement by the project manager"},
		{l.Ready, "0E8A16", "Detailed enough for a developer to pick up"},
		{l.InProgress, "5319E7", "A developer is working on it"},
		{l.Blocked, "B60205", "Waiting on an answer to a question"},
		{l.Review, "FBCA04", "Pull request open and under review"},
		{l.Approved, "0E8A16", "Reviewer approved; waiting for merge"},
		{l.NeedsHuman, "000000", "The factory needs a human to step in"},
		{l.SizeXS, "EDEDED", "Size: one file, obvious change, no design"},
		{l.SizeS, "D4D4D4", "Size: a few files, clear approach, existing tests cover it"},
		{l.SizeM, "BABABA", "Size: a feature slice across several packages, needs new tests"},
		{l.SizeL, "A0A0A0", "Size: crosses subsystems or needs a design decision"},
		{l.SizeXL, "868686", "Size: too big for one pull request; split it instead"},
	}
}
