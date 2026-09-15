package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/kpenfound/busybees/core/review"
	"github.com/kpenfound/busybees/internal/text"
)

// Runner acquires GitHub context and a diff, then delegates all review stages
// and incremental artifact persistence to core/review. Publication and triage
// remain in this package. The same adapter serves the CLI and the scheduler.

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
	// AnglesReady receives the enabled angle set after distillation and
	// before any angle progress callbacks. Nil is told nothing.
	AnglesReady func(angles []string)
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
	if r.Angles.Log == nil {
		r.Angles.Log = r.Log
	}
	if r.Angles.Progress == nil {
		r.Angles.Progress = r.Progress
	}
	if r.Pipeline.Checkout != nil {
		// Gathering already attempted acquisition; angles must not retry it.
		r.Angles.Checkout = nil
	}
	var diff string
	if items := bundle.Of(SourceDiff); len(items) > 0 {
		diff = items[0].Content
	}
	pipeline := &core.Runner[Ref]{
		Distiller: &core.Distiller[Ref]{Agent: r.Distiller.Agent, Dir: r.Distiller.Dir},
		Angles:    r.Angles.core(ref), Settings: r.Pipeline.Project.core(), Rules: coreRules(r.rules()),
		Compare: compareFindingsText, Log: r.Log, Progress: r.Progress, AnglesReady: r.AnglesReady,
	}
	return pipeline.Run(ctx, a.Dir, bundle.core(), diff)
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
