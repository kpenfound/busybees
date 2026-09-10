package review

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The triage queue. Once the judge has written the findings and the noise
// filter has acted on them, somebody decides what to do with each one: a
// person at a terminal (console.go), or in factory mode an agent. Queue is
// what either of them drives. It holds one review's artifact and the four
// actions triage can take on a finding:
//
//	select   the finding goes into the review's output, as it is written or
//	         with the comment text edited
//	dismiss  the finding is left out, and why is appended to the reviewer
//	         notes (notes.go), which is what makes a rule of it one day
//	defer    the finding is left out of this review's output and nothing is
//	         recorded against it: it is not wrong, it is not for now
//	ask      the angle session that found it is reopened with a question
//	         (Angles.Resume); the answer is recorded, and a finding the
//	         answer turns up that the review did not have joins the queue
//
// Every action is written into the artifact's triage state (triage.json)
// before it returns, and a finding an ask added into its findings
// (findings.json), so a triage that stops can be picked up in another
// process where it left off: the queue reads the decisions back and offers
// what is still undecided.
//
// The decisions are a log, in the order they were taken, and the latest
// select, dismiss or defer on a finding is the one in force: a finding
// deferred and then selected is selected. An ask settles nothing; the
// finding stays in the queue with its answer beside it.

// The triage actions, as Decision.Action records them.
const (
	ActionSelect  = "select"
	ActionDismiss = "dismiss"
	ActionDefer   = "defer"
	ActionAsk     = "ask"
)

// Actions lists the triage actions in the order they are offered.
var Actions = []string{ActionSelect, ActionDismiss, ActionDefer, ActionAsk}

// Queue is one review's triage: the findings, what has been decided about
// them, and the actions still open.
type Queue struct {
	// Artifact is the review, judged: its findings are the queue and its
	// triage state is where the decisions go. Every action writes it back.
	Artifact *Artifact
	// Project is the project's configuration, whose category pins act on a
	// finding an ask adds the way they acted on the rest. nil pins nothing.
	Project *Project
	// Notes are the reviewer notes: a dismissal is appended to their file,
	// and their rules act on a finding an ask adds the way they acted on
	// the rest (Filter). A queue without notes cannot dismiss.
	Notes *Notes
	// Angles is what reopens an angle session for an ask. A queue without
	// one cannot ask.
	Angles *Angles
}

// NewQueue is the triage queue of a judged review. A review that stopped
// before the judge has nothing to triage, which is an error; one that has
// never been triaged gets an empty triage state.
func NewQueue(a *Artifact, project *Project, notes *Notes, angles *Angles) (*Queue, error) {
	if a == nil || a.Findings == nil {
		return nil, errors.New("this review has no findings to triage: it stopped before the judge")
	}
	if a.Triage == nil {
		a.Triage = &Triage{Decisions: []Decision{}}
	}
	return &Queue{Artifact: a, Project: project, Notes: notes, Angles: angles}, nil
}

// Findings are every finding in the review, decided or not, in the judge's
// order.
func (q *Queue) Findings() []Finding { return q.Artifact.Findings.Items }

// Pending are the findings nothing has been decided about yet, in the
// judge's order: most severe first.
func (q *Queue) Pending() []Finding {
	var out []Finding
	for _, f := range q.Findings() {
		if _, ok := q.DecisionOn(f.ID); !ok {
			out = append(out, f)
		}
	}
	return out
}

// Selection is one finding triage selected, with the text it is posted as.
type Selection struct {
	Finding Finding
	// Comment is the text: what triage edited it to, or the finding's own
	// (Finding.Comment) when it was selected as written.
	Comment string
}

// Selected are the findings triage selected, in the judge's order, each
// with the text it goes into the output as.
func (q *Queue) Selected() []Selection {
	var out []Selection
	for _, f := range q.Findings() {
		d, ok := q.DecisionOn(f.ID)
		if !ok || d.Action != ActionSelect {
			continue
		}
		comment := d.Comment
		if comment == "" {
			comment = f.Comment()
		}
		out = append(out, Selection{Finding: f, Comment: comment})
	}
	return out
}

// DecisionOn is the decision in force on a finding: the latest select,
// dismiss or defer taken on it, and false when there is none. An ask is not
// a decision on the finding and is never returned here (see Asked).
func (q *Queue) DecisionOn(id string) (Decision, bool) {
	decisions := q.Artifact.Triage.Decisions
	for i := len(decisions) - 1; i >= 0; i-- {
		if d := decisions[i]; d.Finding == id && d.Action != ActionAsk {
			return d, true
		}
	}
	return Decision{}, false
}

// Asked are the questions triage has asked about a finding and what the
// angle answered, oldest first.
func (q *Queue) Asked(id string) []Decision {
	var out []Decision
	for _, d := range q.Artifact.Triage.Decisions {
		if d.Finding == id && d.Action == ActionAsk {
			out = append(out, d)
		}
	}
	return out
}

// Find is the finding with this id, and an error naming the id when the
// review has none.
func (q *Queue) Find(id string) (*Finding, error) {
	items := q.Findings()
	for i := range items {
		if items[i].ID == id {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("no finding %s in this review", id)
}

// Select puts a finding into the review's output. comment is the text it is
// posted as, and "" for the finding as it is written: only an edit is
// recorded, so a finding selected as written reads back as one.
func (q *Queue) Select(id, comment string) error {
	f, err := q.Find(id)
	if err != nil {
		return err
	}
	comment = strings.TrimSpace(comment)
	if comment == f.Comment() {
		comment = ""
	}
	return q.record(Decision{Finding: id, Action: ActionSelect, Comment: comment})
}

// Dismiss leaves a finding out and records why: the reason is appended to
// the reviewer notes before the decision is written, so a dismissal that
// could not be recorded is not one. A dismissal needs a reason: it is what
// the notes are made of.
func (q *Queue) Dismiss(id, reason string) error {
	f, err := q.Find(id)
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("a dismissal needs a reason: it is what your reviewer notes are made of")
	}
	if q.Notes == nil {
		return errors.New("no reviewer notes to record the dismissal in")
	}
	d := DismissalOf(q.repo(), f, reason)
	if err := AppendDismissal(q.Notes.Path, d); err != nil {
		return err
	}
	q.Notes.Dismissals = append(q.Notes.Dismissals, d)
	return q.record(Decision{Finding: id, Action: ActionDismiss, Reason: reason})
}

// Defer leaves a finding out of this review's output and records nothing
// against it.
func (q *Queue) Defer(id string) error {
	if _, err := q.Find(id); err != nil {
		return err
	}
	return q.record(Decision{Finding: id, Action: ActionDefer})
}

// Answer is what an ask came to.
type Answer struct {
	// Text is the angle's answer, without the findings it may have ended
	// with.
	Text string
	// Added are the findings the answer turned up that the review did not
	// have, now in the queue. A finding the review already had, one a
	// category pin turned off, and one the reviewer notes drop are not
	// added; a note that ranks one down ranks it down.
	Added []Finding
}

// Ask reopens the session of the angle that found a finding with a
// question about it, records the answer beside the finding, and adds to the
// queue any finding the answer turned up that the review did not have. The
// finding stays in the queue: an ask settles nothing.
func (q *Queue) Ask(ctx context.Context, id, question string) (*Answer, error) {
	f, err := q.Find(id)
	if err != nil {
		return nil, err
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("nothing to ask: give the question")
	}
	if q.Angles == nil {
		return nil, errors.New("no agent to ask: the queue was opened without one")
	}
	run := q.runOf(f.Angle)
	if run == nil {
		return nil, fmt.Errorf("the %s angle has no session in this review, so there is nothing to ask", f.Angle)
	}
	res, err := q.Angles.Resume(ctx, *run, question)
	if err != nil {
		return nil, err
	}
	if res.ID != "" && res.ID != run.SessionID {
		// A reopened session that answers under another id is resumed
		// under that one next time: the old one has not heard the
		// question.
		run.SessionID = res.ID
		if err := WriteAngleRun(q.Artifact.Dir, run); err != nil {
			return nil, err
		}
	}
	text, added := q.requeue(f.Angle, run.SessionID, res.Text)
	if len(added) > 0 || len(q.Artifact.Findings.Silenced) > 0 {
		if err := WriteFindings(q.Artifact.Dir, q.Artifact.Findings); err != nil {
			return nil, err
		}
	}
	d := Decision{Finding: id, Action: ActionAsk, Question: question, Answer: text}
	for _, a := range added {
		d.Added = append(d.Added, a.ID)
	}
	if err := q.record(d); err != nil {
		return nil, err
	}
	return &Answer{Text: text, Added: added}, nil
}

// emptyFence is a code fence with nothing left in it once the findings
// object has been taken out of an answer.
var emptyFence = regexp.MustCompile("(?m)^[ \t]*```[a-zA-Z]*[ \t]*\n\\s*```[ \t]*$\n?")

// requeue reads the findings a reopened angle session may have ended its
// answer with, and puts the ones the review did not have into the queue:
// merged and pinned as the judge would (Merge), the reviewer notes acting on
// them (Filter), the ones already in the list dropped, and the list
// re-sorted. It returns the answer without the findings, and what it added.
// An answer with no findings object in it, which is most answers, adds
// nothing.
func (q *Queue) requeue(angle, sessionID, text string) (string, []Finding) {
	fresh, err := ParseFindings(angle, sessionID, text)
	if err != nil || len(fresh) == 0 {
		return strings.TrimSpace(text), nil
	}
	obj, _ := jsonObject(text)
	text = strings.TrimSpace(emptyFence.ReplaceAllString(strings.Replace(text, obj, "", 1), ""))
	fresh, silenced := Filter(Merge(fresh, q.Project), q.rules(), q.repo())
	findings := q.Artifact.Findings
	findings.Silenced = append(findings.Silenced, silenced...)
	var added []Finding
	for _, f := range fresh {
		if q.has(&f) {
			continue
		}
		findings.Items = append(findings.Items, f)
		added = append(added, f)
	}
	slices.SortStableFunc(findings.Items, compareFindings)
	return text, added
}

// has reports whether the review already has a finding: one the judge
// would have merged f into, or one with its id.
func (q *Queue) has(f *Finding) bool {
	for i := range q.Artifact.Findings.Items {
		if have := &q.Artifact.Findings.Items[i]; have.ID == f.ID || sameFinding(have, f) {
			return true
		}
	}
	return false
}

// record appends a decision and writes the triage state.
func (q *Queue) record(d Decision) error {
	q.Artifact.Triage.Decisions = append(q.Artifact.Triage.Decisions, d)
	return WriteTriage(q.Artifact.Dir, q.Artifact.Triage)
}

// runOf is the run of the angle that found a finding, and nil when the
// review has none for it.
func (q *Queue) runOf(angle string) *AngleRun {
	for i := range q.Artifact.Runs {
		if q.Artifact.Runs[i].Angle == angle {
			return &q.Artifact.Runs[i]
		}
	}
	return nil
}

// repo is the repository under review, which a dismissal is filed under and
// a rule is matched against.
func (q *Queue) repo() string { return q.Artifact.Brief.Ref.Repo }

// rules are the reviewer notes' rules, and none without notes.
func (q *Queue) rules() []Rule {
	if q.Notes == nil {
		return nil
	}
	return q.Notes.Rules
}

// Comment is the text a finding is posted as when triage does not edit it:
// its title, then its body. The suggestion is not in it: the output renders
// that as a suggestion of its own, and the text is what a person edits.
func (f *Finding) Comment() string {
	if f.Body == "" {
		return f.Title
	}
	return f.Title + "\n\n" + f.Body
}
