package review

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/text"
)

// The triage queue. Once the judge has written the findings and the noise
// filter has acted on them, somebody decides what to do with each one: a
// person at a terminal, a line at a time (console.go) or on the screen
// internal/reviewtui draws, or in factory mode an agent (factory.go). Queue
// is what each of them drives. It holds one review's artifact and the four
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
//
// An action fails in one of two ways, and whoever drives the queue tells
// them apart with errors.As: a Refusal is an action the queue would not take
// as asked, which the person can answer (a dismissal with no reason, a
// finding the review does not have, an angle that cannot be reopened); any
// other error is something that could not be written, and nothing the
// action would have recorded is kept in memory either, so what the queue
// says and what the artifact holds never disagree.

// Refusal is an error for an action the queue would not take as asked. It
// leaves the queue as it was, and the person can ask again differently.
type Refusal struct {
	// Err says what was refused.
	Err error
}

func (r *Refusal) Error() string { return r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

// refuse is a Refusal from a message.
func refuse(format string, args ...any) error {
	return &Refusal{Err: fmt.Errorf(format, args...)}
}

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
	return nil, refuse("no finding %s in this review", id)
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
		return refuse("a dismissal needs a reason: it is what your reviewer notes are made of")
	}
	if q.Notes == nil {
		return refuse("no reviewer notes to record the dismissal in")
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
		return nil, refuse("nothing to ask: give the question")
	}
	if q.Angles == nil {
		return nil, refuse("no agent to ask: the queue was opened without one")
	}
	run := q.runOf(f.Angle)
	if run == nil {
		return nil, refuse("the %s angle has no session in this review, so there is nothing to ask", f.Angle)
	}
	res, err := q.Angles.Resume(ctx, *run, question)
	if err != nil {
		// A session that could not be reopened, or that failed, lost
		// nothing: the finding is as it was, and the person can ask
		// again or decide without an answer.
		return nil, &Refusal{Err: err}
	}
	// Everything below is written before it is kept: a run file, then the
	// findings, then the decision, and an error at any of them leaves the
	// queue as it was.
	if res.ID != "" && res.ID != run.SessionID {
		// A reopened session that answers under another id is resumed
		// under that one next time: the old one has not heard the
		// question.
		reopened := *run
		reopened.SessionID = res.ID
		if err := WriteAngleRun(q.Artifact.Dir, &reopened); err != nil {
			return nil, err
		}
		*run = reopened
	}
	text, findings, added := q.requeue(f.Angle, run.SessionID, res.Text)
	if findings != nil {
		if err := WriteFindings(q.Artifact.Dir, findings); err != nil {
			return nil, err
		}
		q.Artifact.Findings = findings
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
// answer with, and works out the review's findings with the ones it did not
// have added: merged and pinned as the judge would (Merge), the reviewer
// notes acting on them (Filter), the ones already in the list dropped, and
// the list re-sorted. It returns the answer without the findings, the new
// findings list, and what was added; the queue's own list is not touched,
// so the caller can write the new one before keeping it. An answer with no
// findings object in it, which is most answers, changes nothing and the
// list comes back nil.
func (q *Queue) requeue(angle, sessionID, text string) (string, *Findings, []Finding) {
	fresh, err := ParseFindings(angle, sessionID, text)
	if err != nil || len(fresh) == 0 {
		return strings.TrimSpace(text), nil, nil
	}
	obj, _ := jsonObject(text)
	text = strings.TrimSpace(emptyFence.ReplaceAllString(strings.Replace(text, obj, "", 1), ""))
	fresh, silenced := Filter(Merge(fresh, q.Project), q.rules(), q.repo())
	have := q.Artifact.Findings
	findings := &Findings{
		Items:    slices.Clone(have.Items),
		Skipped:  have.Skipped,
		Silenced: append(slices.Clone(have.Silenced), silenced...),
	}
	var added []Finding
	for _, f := range fresh {
		if q.has(&f) {
			continue
		}
		findings.Items = append(findings.Items, f)
		added = append(added, f)
	}
	if len(added) == 0 && len(silenced) == 0 {
		return text, nil, nil
	}
	slices.SortStableFunc(findings.Items, compareFindings)
	return text, findings, added
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

// record writes the triage state with a decision appended, and keeps the
// decision only once it is written: a decision that did not reach the
// artifact is not one the queue took.
func (q *Queue) record(d Decision) error {
	next := &Triage{Decisions: append(slices.Clone(q.Artifact.Triage.Decisions), d)}
	if err := WriteTriage(q.Artifact.Dir, next); err != nil {
		return err
	}
	q.Artifact.Triage.Decisions = next.Decisions
	return nil
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

// Describe is a finding whole, as whoever triages reads it: the file and
// lines it points at, its text, its suggestion, evidence and sources, the
// other angles that found it, and every question asked about it with the
// answer. Its id, severity, angle and category are left to the heading the
// caller puts above it. Each line ends with a newline.
func (q *Queue) Describe(f Finding) string {
	var out strings.Builder
	if f.Anchored() {
		where := f.File
		if !f.Lines.IsZero() {
			where += ":" + f.Lines.String()
		}
		if f.Side == SideOld {
			where += " (removed)"
		}
		fmt.Fprintf(&out, "%s\n", where)
	}
	fmt.Fprintf(&out, "\n%s\n", f.Comment())
	if f.Suggestion != "" {
		fmt.Fprintf(&out, "\nSuggestion:\n%s\n", indent(f.Suggestion))
	}
	if f.Evidence != "" {
		fmt.Fprintf(&out, "\nEvidence: %s\n", f.Evidence)
	}
	if len(f.Sources) > 0 {
		fmt.Fprintf(&out, "Sources: %s\n", strings.Join(f.Sources, ", "))
	}
	if len(f.AlsoFrom) > 0 {
		fmt.Fprintf(&out, "Also from: %s\n", strings.Join(f.AlsoFrom, ", "))
	}
	for _, d := range q.Asked(f.ID) {
		fmt.Fprintf(&out, "\nQ: %s\nA: %s\n", d.Question, d.Answer)
	}
	return out.String()
}

// Summary says where triage stands: how many findings each decision in
// force covers, and how many are still undecided, of how many.
func (q *Queue) Summary() string {
	counts := map[string]int{}
	for _, f := range q.Findings() {
		if d, ok := q.DecisionOn(f.ID); ok {
			counts[d.Action]++
		} else {
			counts["undecided"]++
		}
	}
	var parts []string
	for _, k := range []struct{ action, past string }{{ActionSelect, "selected"}, {ActionDismiss, "dismissed"}, {ActionDefer, "deferred"}, {"undecided", "undecided"}} {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k.action], k.past))
	}
	return fmt.Sprintf("%s of %s", strings.Join(parts, ", "), text.Count(len(q.Findings()), "finding"))
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
