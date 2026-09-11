package review

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// reviewAgent answers every session of a review by name: the distiller
// with a brief, each angle with the findings it is given, and a session in
// fail with an error.
type reviewAgent struct {
	answers map[string]string
	fail    map[string]error

	mu   sync.Mutex
	reqs map[string]AgentRequest
}

func (f *reviewAgent) Run(_ context.Context, req AgentRequest) (*AgentResult, error) {
	f.mu.Lock()
	if f.reqs == nil {
		f.reqs = map[string]AgentRequest{}
	}
	f.reqs[req.Name] = req
	f.mu.Unlock()
	if err := f.fail[req.Name]; err != nil {
		return nil, err
	}
	answer, ok := f.answers[req.Name]
	if !ok {
		answer = `{"findings": []}`
	}
	return &AgentResult{ID: "sess-" + req.Name, Text: answer, Turns: 2}, nil
}

// testRunner is a runner over sampleGH with every session faked: the
// distiller briefs and sizes the change m, the general angle finds a naming problem the notes may
// rank down and a docs one, the test angle a missing test. The checkout is
// a git repository so the callers source runs, and the project's
// context.toml turns the quick general, docs and side effects angles off.
func testRunner(t *testing.T, agent *reviewAgent, notes *Notes) (*Runner, *bytes.Buffer) {
	t.Helper()
	if agent.answers == nil {
		agent.answers = map[string]string{}
	}
	agent.answers[DistillerName] = answeredBrief
	if _, ok := agent.answers[AngleGeneral]; !ok {
		agent.answers[AngleGeneral] = sessionAnswer(
			`{"category": "naming", "severity": "medium", "file": "widget.go", "lines": [1, 1], "side": "new", "title": "Receiver names here are short", "body": "w"}`,
			`{"category": "docs", "severity": "low", "title": "The package has no doc comment", "body": "every package here has one"}`,
		)
	}
	if _, ok := agent.answers[AngleTests]; !ok {
		agent.answers[AngleTests] = sessionAnswer(`{"category": "missing test", "severity": "high", "file": "widget.go", "lines": [1, 1], "side": "new", "title": "Widget has no test", "body": "nothing calls it"}`)
	}
	dir := gitRepo(t, "https://github.com/"+testRepo)
	writeFile(t, dir, ProjectFile, "[angles]\nquick_general = false\ndocs = false\nside_effects = false\n")
	project, err := LoadProject(FindProject(dir))
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	angles := &Angles{Agent: agent, Provider: config.AgentClaude, Model: "opus", Dir: dir}
	if notes != nil {
		angles.Rules = notes.Rules
	}
	return &Runner{
		Pipeline:  &Pipeline{Client: sampleGH().client(t), Project: project, Dir: dir},
		Distiller: &Distiller{Agent: agent, Dir: dir},
		Angles:    angles,
		Notes:     notes,
		Storage:   filepath.Join(t.TempDir(), "reviews"),
		Now:       func() time.Time { return time.Date(2026, 9, 10, 15, 4, 5, 0, time.UTC) },
		Log:       &log,
	}, &log
}

func TestARunGathersBriefsReviewsJudgesFiltersAndWritesTheArtifact(t *testing.T) {
	notes := &Notes{Path: filepath.Join(t.TempDir(), "reviewer-notes.md"), Rules: []Rule{shortNames(RuleDownrank)}}
	agent := &reviewAgent{}
	r, log := testRunner(t, agent, notes)
	ref := Ref{Repo: testRepo, Number: 7}
	a, err := r.Run(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// The artifact is where the storage path and the clock put it, and
	// holds the brief, one run per enabled angle and the findings.
	if want := filepath.Join(r.Storage, "acme", "widgets", "7", "20260910-150405"); a.Dir != want {
		t.Errorf("artifact at %s, want %s", a.Dir, want)
	}
	read, err := ReadArtifact(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if read.Brief.Summary != "gathers the context sources a project declares" || read.Brief.Title != "Add the widget" || read.Brief.SessionID != "sess-distiller" {
		t.Errorf("the brief written is %+v", read.Brief)
	}
	// What the artifact says of the angles: the size the change was given,
	// and the angles of that size the project enables, one run each.
	if read.Brief.Size != "m" {
		t.Errorf("the brief sizes the change %q, want m", read.Brief.Size)
	}
	if got := strings.Join(ranAngles(read.Runs), ","); got != "general,test_coverage,acceptance_criteria" {
		t.Errorf("angles run: %s, want the three of size m the project enables", got)
	}
	if read.Findings == nil || read.Triage != nil {
		t.Fatalf("findings %v, triage %v: want judged and not triaged", read.Findings, read.Triage)
	}
	// Judged (most severe first) and filtered (the naming finding one
	// severity lower, and recorded as silenced).
	got := titles(read.Findings.Items)
	if want := []string{"Widget has no test", "Receiver names here are short", "The package has no doc comment"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("findings %v, want %v", got, want)
	}
	if read.Findings.Items[1].Severity != SeverityLow || len(read.Findings.Silenced) != 1 || read.Findings.Silenced[0].Action != RuleDownrank {
		t.Errorf("the notes did not act on the findings: %+v, silenced %+v", read.Findings.Items[1], read.Findings.Silenced)
	}
	// The in-memory artifact is the one written.
	if len(a.Findings.Items) != 3 || a.Findings.Items[1].Severity != SeverityLow || len(a.Runs) != 3 {
		t.Errorf("the artifact returned differs from the one written: %+v", a.Findings)
	}
	// The sessions were told what the pipeline gathered: the distiller
	// the bundle, the angles the brief, the diff and the notes' rules.
	if !strings.Contains(agent.reqs[DistillerName].Prompt, "Closes #12") || !strings.Contains(agent.reqs[DistillerName].Prompt, "func Widget() {}") {
		t.Errorf("the distiller was not given the gathered context")
	}
	// testRunner's Angles has no Checkout, so the angles run in the
	// machine's own checkout (Angles.Dir): a working tree the review does
	// not own, and diff.patch is not written into it (angles_test.go's
	// TestTheDiffIsNotWrittenIntoTheMachinesCheckout covers that
	// directly). The prompt falls back to the same message a session with
	// no diff at all gets.
	if p := agent.reqs[AngleGeneral].Prompt; !strings.Contains(p, "gathers the context sources a project declares") || !strings.Contains(p, "The diff was not gathered") || !strings.Contains(p, "receiver names are short here") {
		t.Errorf("the general angle was not given the brief, the rules and the diff fallback:\n%s", p)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, DiffFile)); !os.IsNotExist(err) {
		t.Errorf("diff.patch written under the artifact directory (stat err: %v)", err)
	}
	if _, ok := agent.reqs[AngleSideEffects]; ok {
		t.Error("the angle the project turned off ran")
	}
	for _, want := range []string{
		"gathering the context of acme/widgets#7\n",
		"gathered 5 items from diff, pr_body, linked_issues\n",
		"distilling the brief\n",
		"the review is " + a.Dir + "\n",
		"reviewing a size m change from 3 angles: general, test_coverage, acceptance_criteria\n",
		"3 findings\n",
		"  1 finding hidden by your reviewer notes\n",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, log.String())
		}
	}
	// The queue over it is wired with what the review used, so an ask
	// reopens the session that reviewed.
	q, err := r.Queue(a)
	if err != nil {
		t.Fatal(err)
	}
	if q.Notes != notes || q.Angles != r.Angles || q.Project != r.Pipeline.Project || len(q.Pending()) != 3 {
		t.Errorf("the queue is not over the review's own parts: %+v", q)
	}
}

// A run reviews from the angles of the brief's size that the project
// enables, and says so, not from every angle the project enables: a small
// change from this project, which turned off both angles a small change
// gets, is reviewed from none, and that is not an error.
func TestARunReviewsFromTheAnglesOfTheBriefsSize(t *testing.T) {
	agent := &reviewAgent{}
	r, log := testRunner(t, agent, nil)
	small := strings.Replace(answeredBrief, `"size": "m"`, `"size": "xs"`, 1)
	if small == answeredBrief {
		t.Fatal("answeredBrief is not size m")
	}
	agent.answers[DistillerName] = small
	a, err := r.Run(context.Background(), Ref{Repo: testRepo, Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	if a.Brief.Size != "xs" || len(a.Runs) != 0 {
		t.Errorf("size %q, runs %+v: want xs and none", a.Brief.Size, a.Runs)
	}
	for _, angle := range BuiltinAngles {
		if _, ran := agent.reqs[angle]; ran {
			t.Errorf("the %s angle ran", angle)
		}
	}
	if want := "reviewing a size xs change from 0 angles: \n"; !strings.Contains(log.String(), want) {
		t.Errorf("the log lacks %q:\n%s", want, log.String())
	}
}

func TestARunGoesOnWithoutAnAngleThatFailedAndStopsWhenEveryOneDid(t *testing.T) {
	agent := &reviewAgent{fail: map[string]error{AngleGeneral: errors.New("general session: exit status 1")}}
	r, log := testRunner(t, agent, nil)
	a, err := r.Run(context.Background(), Ref{Repo: testRepo, Number: 7})
	if err != nil {
		t.Fatalf("one angle failing stopped the review: %v", err)
	}
	if len(a.Runs) != 3 || !a.Runs[0].Failed() {
		t.Errorf("runs %+v, want the failed general run kept", a.Runs)
	}
	if got := titles(a.Findings.Items); len(got) != 1 || got[0] != "Widget has no test" {
		t.Errorf("findings %v, want the test angle's alone", got)
	}
	if len(a.Findings.Skipped) != 1 || !strings.Contains(a.Findings.Skipped[0], "general") {
		t.Errorf("skipped %v, want the general angle named", a.Findings.Skipped)
	}
	for _, want := range []string{"  the general angle failed: general session: exit status 1\n", "  not reviewed: general: the session failed"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, log.String())
		}
	}

	// Every angle failing is an error: nobody reviewed. The brief and the
	// runs are written, the findings are not.
	agent = &reviewAgent{fail: map[string]error{
		AngleAcceptance: errors.New("acceptance_criteria session: no"),
		AngleTests:      errors.New("test_coverage session: no"),
		AngleGeneral:    errors.New("general session: no"),
	}}
	r, _ = testRunner(t, agent, nil)
	a, err = r.Run(context.Background(), Ref{Repo: testRepo, Number: 7})
	if err == nil || !strings.Contains(err.Error(), "every angle failed") || !strings.Contains(err.Error(), "general session: no") {
		t.Fatalf("err = %v, want every angle's failure", err)
	}
	if a != nil {
		t.Errorf("an artifact was returned for a review nobody made: %+v", a)
	}
	dir := filepath.Join(r.Storage, "acme", "widgets", "7", "20260910-150405")
	if _, err := os.Stat(filepath.Join(dir, BriefFile)); err != nil {
		t.Errorf("the brief was not kept: %v", err)
	}
	if runs, err := ReadAngleRuns(dir); err != nil || len(runs) != 3 {
		t.Errorf("the failed runs were not kept: %v, %v", runs, err)
	}
	if _, err := os.Stat(filepath.Join(dir, FindingsFile)); !os.IsNotExist(err) {
		t.Errorf("findings were written for a review nobody made: %v", err)
	}
}

func TestARunThatStopsBeforeTheBriefWritesNothing(t *testing.T) {
	// The distiller produced no brief: no artifact directory at all.
	agent := &reviewAgent{fail: map[string]error{DistillerName: errors.New("distiller session: no")}}
	r, _ := testRunner(t, agent, nil)
	if _, err := r.Run(context.Background(), Ref{Repo: testRepo, Number: 7}); err == nil || !strings.Contains(err.Error(), "distiller session: no") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(r.Storage); !os.IsNotExist(err) {
		t.Errorf("something was written under the storage path: %v", err)
	}
	if _, ok := agent.reqs[AngleGeneral]; ok {
		t.Error("an angle ran without a brief")
	}
	// The context could not be gathered: no session ran.
	agent = &reviewAgent{}
	r, _ = testRunner(t, agent, nil)
	gh := sampleGH()
	gh.errs = map[string]bool{"pr view": true}
	r.Pipeline.Client = gh.client(t)
	if _, err := r.Run(context.Background(), Ref{Repo: testRepo, Number: 7}); err == nil || !strings.Contains(err.Error(), "read acme/widgets#7") {
		t.Errorf("err = %v", err)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("sessions ran without context: %v", agent.reqs)
	}
}

func TestNewRunnerWiresTheConfiguredAgentNotesAndStorage(t *testing.T) {
	home := t.TempDir()
	cfg, err := ParseConfig("provider = \"codex\"\nmodel = \"o3\"\nstorage_path = \"kept\"\nnotes_path = \"notes.md\"\n", filepath.Join(home, ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	dismiss(t, filepath.Join(home, "notes.md"), AngleGeneral, "naming", "receiver names are short here")
	dismiss(t, filepath.Join(home, "notes.md"), AngleGeneral, "naming", "receiver names here are short")
	notes, err := ReadNotes(filepath.Join(home, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	notes.Consolidate()
	if err := notes.Write(); err != nil {
		t.Fatal(err)
	}
	dir := gitRepo(t, "https://github.com/"+testRepo)
	ref := Ref{Repo: testRepo, Number: 7}
	r, err := NewRunner(context.Background(), ref, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Storage != filepath.Join(home, "kept") || r.Notes.Path != filepath.Join(home, "notes.md") || len(r.Notes.Rules) != 1 {
		t.Errorf("storage %s, notes %s with %d rules", r.Storage, r.Notes.Path, len(r.Notes.Rules))
	}
	if r.Angles.Provider != config.AgentCodex || r.Angles.Model != "o3" || len(r.Angles.Rules) != 1 || r.Angles.Dir != dir {
		t.Errorf("angles %+v", r.Angles)
	}
	if agent, ok := r.Distiller.Agent.(*CLIAgent); !ok || agent.Provider != config.AgentCodex || r.Distiller.Dir != dir {
		t.Errorf("distiller %+v", r.Distiller)
	}
	if r.Pipeline.Client.Repo != testRepo || r.Pipeline.Dir != dir {
		t.Errorf("pipeline %+v", r.Pipeline)
	}
	// Run from a directory that is not a checkout of the repository: no
	// checkout, so the sessions run in a directory of their own.
	r, err = NewRunner(context.Background(), ref, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r.Pipeline.Dir != "" || r.Angles.Dir != "" || r.Distiller.Dir != "" {
		t.Errorf("an unrelated directory was taken as the checkout: %q %q %q", r.Pipeline.Dir, r.Angles.Dir, r.Distiller.Dir)
	}
}

func TestARunReportsACheckoutItCouldNotMakeOnItsLog(t *testing.T) {
	agent := &reviewAgent{}
	r, log := testRunner(t, agent, nil)
	missing := filepath.Join(t.TempDir(), "docker")
	r.Angles.Checkout = &Checkout{DockerBin: missing}
	a, err := r.Run(context.Background(), Ref{Repo: testRepo, Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	// The angles report on the runner's log, and ran in the checkout the
	// machine has.
	if !strings.Contains(log.String(), "could not check out acme/widgets#7 in a container: "+missing+" is not installed; the angles run in "+r.Angles.Dir) {
		t.Errorf("log:\n%s\nwant the checkout that could not be made", log.String())
	}
	for _, run := range a.Runs {
		if run.Dir != r.Angles.Dir {
			t.Errorf("the %s angle ran in %q, want the machine's checkout %s", run.Angle, run.Dir, r.Angles.Dir)
		}
	}
	if _, err := os.Stat(filepath.Join(a.Dir, CheckoutDir)); !os.IsNotExist(err) {
		t.Errorf("a checkout directory was left in the artifact: %v", err)
	}
}
