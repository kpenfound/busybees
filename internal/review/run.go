package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/text"
)

// One review, end to end. Runner strings the pipeline together for one
// pull request: the context is gathered (context.go), the distiller briefs
// the review (distill.go), the angles review from the brief (angles.go),
// the judge merges what they found (judge.go), the reviewer notes act on
// the list (noise.go), and the review is in its artifact directory
// (artifact.go) for triage to read (triage.go). It is what `bees review
// <pr>` runs before it triages, and what a test runs with every session
// and every gh call faked.
//
// The artifact is written as the review goes: the checkout of the pull
// request's head as the context is gathered, the brief as soon as the
// distiller has produced it, each angle's run as the angles finish, the
// findings once the judge and the notes have made the list. A review that
// stops partway leaves what it reached, which is enough to say where it
// stopped and nothing a later command mistakes for a judged review; one
// that stops before the brief reached nothing worth keeping, and leaves
// no directory at all.

// Runner runs one review.
type Runner struct {
	// Pipeline gathers the context: the gh client, the project's
	// context.toml and the machine's checkout, as Open builds them, and
	// the Checkout that clones the pull request's head under the artifact
	// directory. Angles gets no checkout to attempt of its own when the
	// pipeline has one: it runs in the clone the pipeline made, or where
	// it would without one.
	Pipeline *Pipeline
	// Distiller and Angles run the sessions. Angles reports on Log, and
	// to Progress, when it is given none of its own.
	Distiller *Distiller
	Angles    *Angles
	// Notes are the reviewer notes, whose rules the angles are told and the
	// noise filter applies. A person who has dismissed nothing has empty
	// ones.
	Notes *Notes
	// Storage is the directory the artifact directory is created under,
	// the global configuration's resolved storage path.
	Storage string
	// Now names the review's artifact directory. It is the clock when nil.
	Now func() time.Time
	// Log is where progress is written, one line per step, and nowhere
	// when nil.
	Log io.Writer
	// Progress is told as each angle's session starts and ends
	// (Angles.Progress), for a display that draws the fan-out while it
	// runs. It is nil to be told nothing. Angles gets it when it is given
	// none of its own, as with Log.
	Progress func(angle string, event AngleEvent)
}

// NewRunner is the runner of a review of ref as the global configuration
// says, run from dir: gh authenticated the way cfg says, dir as the
// checkout when it is one of ref's repository (Open), the sessions as cfg's
// provider and model, and the reviewer notes at cfg's notes path.
func NewRunner(ctx context.Context, ref Ref, cfg *Config, dir string) (*Runner, error) {
	pipeline, err := Open(ctx, ref, cfg, dir)
	if err != nil {
		return nil, err
	}
	notes, err := ReadNotes(cfg.ResolvedNotesPath())
	if err != nil {
		return nil, err
	}
	angles := NewAngles(cfg, pipeline.Dir)
	angles.Rules = notes.Rules
	pipeline.Checkout = angles.Checkout
	return &Runner{
		Pipeline:  pipeline,
		Distiller: NewDistiller(cfg, pipeline.Dir),
		Angles:    angles,
		Notes:     notes,
		Storage:   cfg.ResolvedStoragePath(),
	}, nil
}

// Run reviews ref and returns the judged artifact, written under Storage
// and ready for a Queue. An error is a step that could not run: the
// context that could not be gathered, a distiller that produced no brief,
// every angle failing. One angle failing among others is not: the review
// goes on without it, the failure is in the findings' Skipped and in the
// log, and the run is kept with its error for the artifact to say so.
//
// The artifact directory is named before the context is gathered, so the
// pipeline's checkout of the pull request's head has somewhere to go
// (Checkout.Run makes the directory), and removed again when the run stops
// before the brief is written: a directory holding a clone and no brief
// is not a review, and a run that made no checkout wrote nothing.
func (r *Runner) Run(ctx context.Context, ref Ref) (*Artifact, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	a := &Artifact{Dir: ArtifactDir(r.Storage, ref, now())}
	if r.Pipeline.Log == nil {
		r.Pipeline.Log = r.Log
	}
	r.logf("gathering the context of %s", ref)
	bundle, err := r.Pipeline.Gather(ctx, ref.Number, a.Dir)
	if err != nil {
		_ = os.RemoveAll(a.Dir)
		return nil, err
	}
	r.logf("gathered %s from %s", text.Count(len(bundle.Items), "item"), strings.Join(bundle.Sources(), ", "))
	for _, s := range bundle.Skipped {
		r.logf("  not gathered: %s", s)
	}
	r.logf("distilling the brief")
	brief, err := r.Distiller.Distill(ctx, bundle)
	if err != nil {
		_ = os.RemoveAll(a.Dir)
		return nil, err
	}
	a.Brief = brief
	if err := WriteBrief(a.Dir, brief); err != nil {
		_ = os.RemoveAll(a.Dir)
		return nil, err
	}
	r.logf("the review is %s", a.Dir)
	project := r.Pipeline.Project
	if project == nil {
		project = &Project{}
	}
	angles := anglesFor(project, r.Angles.sizedAngles(brief.Size))
	r.logf("reviewing a size %s change from %s: %s", brief.Size, text.Count(len(angles), "angle"), strings.Join(angles, ", "))
	var diff string
	if items := bundle.Of(SourceDiff); len(items) > 0 {
		diff = items[0].Content
	}
	if r.Angles.Log == nil {
		r.Angles.Log = r.Log
	}
	if r.Angles.Progress == nil {
		r.Angles.Progress = r.Progress
	}
	if r.Pipeline.Checkout != nil {
		// The pipeline attempted the checkout as it gathered, and said
		// what came of it: the angles run in the clone it made, which
		// Angles.Run finds under the artifact, and attempt none of their
		// own when it made none.
		r.Angles.Checkout = nil
	}
	runs, err := r.Angles.Run(ctx, a.Dir, project, brief, diff)
	if runs == nil && err != nil {
		return nil, err
	}
	a.Runs = runs
	failed := 0
	for _, run := range runs {
		if run.Failed() {
			failed++
			r.logf("  the %s angle failed: %s", run.Angle, run.Error)
		}
	}
	if len(runs) > 0 && failed == len(runs) {
		return nil, fmt.Errorf("every angle failed, so nobody reviewed %s: %w", ref, err)
	}
	findings := Judge(runs, project)
	findings.Items, findings.Silenced = Filter(findings.Items, r.rules(), ref.Repo)
	if err := WriteFindings(a.Dir, findings); err != nil {
		return nil, err
	}
	a.Findings = findings
	r.logf("%s", text.Count(len(findings.Items), "finding"))
	if len(findings.Silenced) > 0 {
		r.logf("  %s hidden by your reviewer notes", text.Count(len(findings.Silenced), "finding"))
	}
	for _, s := range findings.Skipped {
		r.logf("  not reviewed: %s", s)
	}
	return a, nil
}

// Queue is the triage queue over a review this runner ran: the artifact
// with the project, notes and angles the review used, so an ask reopens
// the session the review ran.
func (r *Runner) Queue(a *Artifact) (*Queue, error) {
	return NewQueue(a, r.Pipeline.Project, r.Notes, r.Angles)
}

// rules are the reviewer notes' rules, and none without notes.
func (r *Runner) rules() []Rule {
	if r.Notes == nil {
		return nil
	}
	return r.Notes.Rules
}

// logf writes one line of progress.
func (r *Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(r.Log, format+"\n", args...)
}
