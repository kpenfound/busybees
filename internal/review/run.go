package review

import (
	"context"
	"fmt"
	"io"
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
// The artifact is written as the review goes: the brief as soon as the
// distiller has produced it, each angle's run as the angles finish, the
// findings once the judge and the notes have made the list. A review that
// stops partway leaves what it reached, which is enough to say where it
// stopped and nothing a later command mistakes for a judged review.

// Runner runs one review.
type Runner struct {
	// Pipeline gathers the context: the gh client, the project's
	// context.toml and the checkout, as Open builds them.
	Pipeline *Pipeline
	// Distiller and Angles run the sessions. Angles reports on Log when
	// it is given none of its own.
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
func (r *Runner) Run(ctx context.Context, ref Ref) (*Artifact, error) {
	r.logf("gathering the context of %s", ref)
	bundle, err := r.Pipeline.Gather(ctx, ref.Number)
	if err != nil {
		return nil, err
	}
	r.logf("gathered %s from %s", text.Count(len(bundle.Items), "item"), strings.Join(bundle.Sources(), ", "))
	for _, s := range bundle.Skipped {
		r.logf("  not gathered: %s", s)
	}
	r.logf("distilling the brief")
	brief, err := r.Distiller.Distill(ctx, bundle)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	a := &Artifact{Dir: ArtifactDir(r.Storage, ref, now()), Brief: brief}
	if err := WriteBrief(a.Dir, brief); err != nil {
		return nil, err
	}
	r.logf("the review is %s", a.Dir)
	project := r.Pipeline.Project
	if project == nil {
		project = &Project{}
	}
	angles := anglesFor(project, brief.Size)
	r.logf("reviewing a size %s change from %s: %s", brief.Size, text.Count(len(angles), "angle"), strings.Join(angles, ", "))
	var diff string
	if items := bundle.Of(SourceDiff); len(items) > 0 {
		diff = items[0].Content
	}
	if r.Angles.Log == nil {
		r.Angles.Log = r.Log
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
