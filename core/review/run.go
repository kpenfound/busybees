package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Runner reviews supplied context and diff. The caller owns acquisition,
// artifact naming, agent profiles, and publication.
type Runner[R Reference] struct {
	Distiller   *Distiller[R]
	Angles      *Angles[R]
	Settings    *Settings
	Rules       []Rule
	Compare     Comparator
	Log         io.Writer
	Progress    func(string, AngleEvent)
	AnglesReady func([]string)
}

// Run writes each stage into dir. Failures before the brief remove dir,
// including anything acquired there; later failures leave partial artifacts.
// One failed angle is non-fatal when another succeeds.
func (r *Runner[R]) Run(ctx context.Context, dir string, bundle *Bundle[R], diff string) (*Artifact[R], error) {
	a := &Artifact[R]{Dir: dir}
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
	project := r.Settings
	if project == nil {
		project = &Settings{}
	}
	angles := anglesFor(project, r.Angles.sizedAngles(brief.Size))
	if r.AnglesReady != nil {
		r.AnglesReady(angles)
	}
	r.logf("reviewing a size %s change from %s: %s", brief.Size, count(len(angles), "angle"), strings.Join(angles, ", "))
	if r.Angles.Log == nil {
		r.Angles.Log = r.Log
	}
	if r.Angles.Progress == nil {
		r.Angles.Progress = r.Progress
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
		return nil, fmt.Errorf("every angle failed, so nobody reviewed %s: %w", bundle.Ref, err)
	}
	findings := Judge(runs, project, r.Compare)
	var dropped []Finding
	findings.Items, dropped = Exclude(findings.Items, bundle.Excluded)
	if len(dropped) > 0 {
		r.logf("  %s on generated files dropped", count(len(dropped), "finding"))
	}
	findings.Items, findings.Silenced = Filter(findings.Items, r.Rules, bundle.Ref.ReviewScope(), r.Compare)
	if err := WriteFindings(a.Dir, findings); err != nil {
		return nil, err
	}
	a.Findings = findings
	r.logf("%s", count(len(findings.Items), "finding"))
	if len(findings.Silenced) > 0 {
		r.logf("  %s hidden by your reviewer notes", count(len(findings.Silenced), "finding"))
	}
	for _, s := range findings.Skipped {
		r.logf("  not reviewed: %s", s)
	}
	return a, nil
}

// logf writes one line of progress.
func (r *Runner[R]) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(r.Log, format+"\n", args...)
}
