package review

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The angle sessions. Once the distiller has written the brief, one session
// per angle the change's size calls for (sizeAngles) and the project enables
// (Settings.EnabledAngles) reads it and looks for problems from that angle
// alone: a quick general pass or a thorough one, the comments and documents,
// the tests, the acceptance criteria, the side effects. They all run at once,
// and every one of them is a read-only session through Agent, held to the
// restriction agent.go sets: it can read and search the checkout and can do
// nothing else.
//
// What an angle came to is persisted, not only its answer: the triage action
// that asks an angle a follow-up question reopens the session it ran, and
// reopening one takes the agent that ran it, the id it gave the session and
// the directory it ran in. That is an AngleRun, one file per angle in the
// artifact directory (WriteAngleRun, ReadAngleRuns), and Resume is what
// reads one back into a session.
//
// A session is also told what the reviewer notes say has been dismissed from
// its angle before (Angles.Rules, policy.go), so that a review does not report
// what its reviewer has already said no to twice.

// angleFrame is what every angle session is told: how a review session
// behaves and what to answer with. The angle's own instructions, the brief
// and the diff follow it.
//
//go:embed prompts/angle.md
var angleFrame string

// angleInstructionFiles holds one file per angle in BuiltinAngles, named
// after it, with what that angle looks for.
//
//go:embed prompts/angles/*.md
var angleInstructionFiles embed.FS

// angleTitles name the angles in a sentence.
var angleTitles = map[string]string{
	AngleQuickGeneral: "quick general",
	AngleGeneral:      "general",
	AngleDocs:         "documentation accuracy",
	AngleTests:        "test coverage and documentation",
	AngleAcceptance:   "acceptance criteria",
	AngleSideEffects:  "side effects",
}

// Artifact paths shared by pipeline stages and their callers. CheckoutDir is
// reserved for caller-acquired files; core does not create a checkout.
const (
	AnglesDir   = "angles"
	CheckoutDir = "checkout"
	ScratchDir  = "scratch"
	DiffFile    = "diff.patch"
)

// AngleRun is one angle's session: what it came to, and what reopens it.
type AngleRun struct {
	// Angle is the angle the session reviewed from, a name in
	// BuiltinAngles.
	Angle string `json:"angle"`
	// Provider and Model are the agent the session ran as. A session is
	// reopened by the same agent: another provider does not know its id.
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// Dir is where the session ran, retained for caller-owned resumption.
	Dir string `json:"dir"`
	// SessionID is the agent's own id for the session, which Resume gives
	// back to it. It is empty for a session that failed.
	SessionID string `json:"session_id,omitempty"`
	// Answer is what the session said last, as it said it: the findings it
	// was asked for, in the JSON shape the prompt describes. Reading them
	// into findings is the judge's job (judge.go), not this file's.
	Answer string `json:"answer,omitempty"`
	// Turns is how many turns the session took and CostUSD what it cost,
	// when the CLI reported one; CostUnknown says it reported none.
	Turns       int     `json:"turns,omitempty"`
	CostUSD     float64 `json:"cost_usd,omitempty"`
	CostUnknown bool    `json:"cost_unknown,omitempty"`
	// Error is what the session failed with, and "" for one that finished.
	// A failed angle is kept with the rest so the review can say which
	// angle nobody looked from.
	Error string `json:"error,omitempty"`
}

// Failed reports whether the session failed, in which case there is no
// answer and nothing to resume.
func (r *AngleRun) Failed() bool { return r.Error != "" }

// Angles runs the angle sessions of a review.
type Angles[R Reference] struct {
	// Agent runs the sessions; Provider and Model say which agent it is,
	// which is what every run records so it can be reopened by the same
	// one.
	Agent    Agent
	Provider string
	Model    string
	// Sized replaces the built-in angles of each size it names (sizeAngles),
	// a size it leaves out keeps the built-in list.
	Sized map[string][]string
	// Prepare optionally supplies the session directory. The caller owns any
	// acquisition; nil uses Dir or a persistent scratch directory.
	Prepare func(context.Context, string) (string, error)
	// AgentFor optionally selects an agent and recorded model per angle.
	// It is called concurrently; profile construction belongs to the caller.
	AgentFor func(angle string) (Agent, string)
	// ProviderFor optionally selects the provider recorded for an angle. Both
	// callbacks must be safe for concurrent calls; nil retains Provider.
	ProviderFor func(angle string) string
	// Dir is an existing working directory, used when Prepare is nil.
	// Core does not write the diff into this caller-owned directory.
	// With no directory, sessions use a persistent scratch directory.
	Dir string
	// Log receives angle-selection diagnostics, and is silent when nil.
	Log io.Writer
	// Rules are the reviewer notes' rules (policy.go), read by whoever runs
	// the review: the ones that name the repository under review and the
	// angle become the part of that angle's prompt saying what has been
	// dismissed from it before. A person who has dismissed nothing has
	// none, and their sessions are told nothing extra.
	Rules []Rule
	// Progress is told how each angle's session is going, for a display to
	// draw while the angles run: AngleStarted the moment before the
	// session starts, and AngleFinished or AngleFailed the moment it ends.
	// It is called from the goroutine of the angle it is about, so calls
	// arrive at once from several angles and it has to be safe to call
	// concurrently. Nil is told nothing, and costs nothing.
	Progress func(angle string, event AngleEvent)
}

// AngleEvent is what Angles.Progress is told about an angle's session.
type AngleEvent string

const (
	// AngleStarted is told as the session starts.
	AngleStarted AngleEvent = "started"
	// AngleFinished is told as the session ends with an answer.
	AngleFinished AngleEvent = "finished"
	// AngleFailed is told as the session ends with an error instead.
	AngleFailed AngleEvent = "failed"
)

// Run fans out one session per angle the brief's size calls for (Sized, else
// sizeAngles) and project enables (anglesFor), all at once, each given the brief and told where to
// read the diff, and waits for every one of them. The runs come back in
// BuiltinAngles order and are written into artifact, one file per angle,
// before Run returns. A brief whose size is not one of Sizes is reported on
// Log and gets the angles of the largest.
//
// Prepare supplies a working directory when set; otherwise sessions use Dir
// or the artifact's empty ScratchDir. Acquisition stays with the caller.
//
// One angle failing stops none of the others: the runs are complete whether
// or not err is nil, a failed angle is among them with Error set and no
// answer, and err says so once every angle has ended. An error before any
// session ran (no brief, a directory that could not be made) returns no
// runs. diff is the pull request's diff, and "" when it was not gathered; it
// is written once to DiffFile inside dir (writeDiff), not once per angle,
// so every angle's prompt can point at the one file instead of repeating
// the diff's text: dir is what an angle's read-only tools can reach, and a
// path outside it could not be read. It is not written, and the prompt
// falls back to the same message it gets when there was no diff at all,
// when dir is Dir, the checkout the machine already had: a review does not
// write into a working tree it did not make. Progress, when set, is told as
// each session starts and ends.
func (a *Angles[R]) Run(ctx context.Context, artifact string, project *Settings, brief *Brief[R], diff string) ([]AngleRun, error) {
	if brief == nil {
		return nil, errors.New("angles: no brief to review from")
	}
	if project == nil {
		project = &Settings{}
	}
	if !slices.Contains(Sizes, brief.Size) {
		a.logf("the brief sizes the change %q, which is not one of %s: the angles of %s run", brief.Size, strings.Join(Sizes, ", "), largestSize())
	}
	angles := anglesFor(project, a.sizedAngles(brief.Size))
	if len(angles) == 0 {
		return nil, nil
	}
	dir, err := a.dir(ctx, artifact)
	if err != nil {
		return nil, err
	}
	diffPath, err := writeDiff(dir, a.Dir, diff)
	if err != nil {
		return nil, err
	}
	prompts := make([]string, len(angles))
	for i, angle := range angles {
		prompt, err := anglePrompt(angle, brief, diffPath, a.Rules)
		if err != nil {
			return nil, err
		}
		prompts[i] = prompt
	}
	runs := make([]AngleRun, len(angles))
	var wg sync.WaitGroup
	for i, angle := range angles {
		wg.Add(1)
		go func(i int, angle string) {
			defer wg.Done()
			agent, model := a.agentFor(angle)
			provider := a.Provider
			if a.ProviderFor != nil {
				provider = a.ProviderFor(angle)
			}
			run := AngleRun{Angle: angle, Provider: provider, Model: model, Dir: dir}
			a.progress(angle, AngleStarted)
			res, err := agent.Run(ctx, AgentRequest{Name: angle, Prompt: prompts[i], Dir: dir})
			if err != nil {
				run.Error = err.Error()
				a.progress(angle, AngleFailed)
			} else {
				run.SessionID, run.Answer, run.Turns, run.CostUSD, run.CostUnknown = res.ID, res.Text, res.Turns, res.CostUSD, !res.CostKnown
				// The session that answered may not be the one configured:
				// an agent that fell back says so.
				if res.Provider != "" {
					run.Provider = res.Provider
				}
				if res.Model != "" {
					run.Model = res.Model
				}
				a.progress(angle, AngleFinished)
			}
			runs[i] = run
		}(i, angle)
	}
	wg.Wait()
	var errs []error
	for i := range runs {
		if err := WriteAngleRun(artifact, &runs[i]); err != nil {
			return runs, err
		}
		if runs[i].Failed() {
			errs = append(errs, errors.New(runs[i].Error))
		}
	}
	return runs, errors.Join(errs...)
}

// sizeAngles are the angles a change of each of Sizes is reviewed from,
// unless Sized replaces a size's list, before the caller's
// own [angles] switches: a small change gets the quick
// general pass and its documentation read; a larger one the thorough general
// pass instead, its tests and its acceptance criteria; and only the largest
// its side effects, which are what a change breaks far from its diff.
var sizeAngles = map[string][]string{
	"xs": {AngleQuickGeneral, AngleDocs},
	"s":  {AngleQuickGeneral, AngleDocs},
	"m":  {AngleGeneral, AngleDocs, AngleTests, AngleAcceptance},
	"l":  {AngleGeneral, AngleDocs, AngleTests, AngleAcceptance},
	"xl": {AngleGeneral, AngleDocs, AngleTests, AngleAcceptance, AngleSideEffects},
}

// largestSize is the last of Sizes, whose angles a size that is not one of
// them gets.
func largestSize() string { return Sizes[len(Sizes)-1] }

// anglesFor lists the angles a review runs: the ones in sized, the angles
// its change's size calls for (sizedAngles), that project enables, in
// BuiltinAngles order. The size only narrows what the project enables, and
// never runs an angle the project turned off; what is left can be one
// angle, or none.
func anglesFor(project *Settings, sized []string) []string {
	var angles []string
	for _, angle := range project.EnabledAngles() {
		if slices.Contains(sized, angle) {
			angles = append(angles, angle)
		}
	}
	return angles
}

// sizedAngles is the angles a change of size is reviewed from before the
// project's switches: overrides[size] when it is set, else sizeAngles[size].
//
// A size that is not one of Sizes gets the angles of the largest. Distill
// refuses such a brief (Brief.Validate), but a brief read back from an
// artifact, or made by any other caller, has not been through it, and a
// change nobody sized is better reviewed too thoroughly than not at all.
func sizedAngles(overrides map[string][]string, size string) []string {
	if !slices.Contains(Sizes, size) {
		size = largestSize()
	}
	if sized, ok := overrides[size]; ok {
		return sized
	}
	return sizeAngles[size]
}

// sizedAngles is the angles a change of size is reviewed from, with Sized
// laid over the built-in lists.
func (a *Angles[R]) sizedAngles(size string) []string { return sizedAngles(a.Sized, size) }

// agentFor selects a caller-supplied profile, or the default agent and model.
func (a *Angles[R]) agentFor(angle string) (Agent, string) {
	if a.AgentFor != nil {
		return a.AgentFor(angle)
	}
	return a.Agent, a.Model
}

func (a *Angles[R]) dir(ctx context.Context, artifact string) (string, error) {
	if a.Prepare != nil {
		return a.Prepare(ctx, artifact)
	}
	if a.Dir != "" {
		return a.Dir, nil
	}
	dir := filepath.Join(artifact, ScratchDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// progress tells Progress about an angle's session, and nobody when it is
// nil.
func (a *Angles[R]) progress(angle string, event AngleEvent) {
	if a.Progress != nil {
		a.Progress(angle, event)
	}
}

// logf writes one line to Log.
func (a *Angles[R]) logf(format string, args ...any) {
	if a.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(a.Log, format+"\n", args...)
}

// writeDiff writes the pull request's diff once per review run to DiffFile
// inside dir, the directory the angle sessions run in (Angles.dir) and the
// only one their read-only tools can reach: a path outside it, such as the
// artifact directory dir sits under, could not be read back. It returns the
// path an angle prompt points at, and "" without writing anything when
// there was no diff to write, or when dir is hostDir, the checkout of the
// repository under review the machine already had (Angles.Dir): a review
// does not write a stray file into a working tree it did not make, so the
// angle prompts fall back to the same message they get when no diff was
// gathered at all. Writing it once here, rather than once per angle inline
// in the prompt, is what keeps a change reviewed by several angles from
// carrying the same diff text in as many prompts.
func writeDiff(dir, hostDir, diff string) (string, error) {
	if strings.TrimSpace(diff) == "" || (hostDir != "" && dir == hostDir) {
		return "", nil
	}
	path := filepath.Join(dir, DiffFile)
	if err := os.WriteFile(path, []byte(diff), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// anglePrompt is the whole of what an angle session is told: how a review
// session behaves, what this angle looks for, what the reviewer notes say
// has been dismissed from this angle before, the brief, where to read the
// diff when there is one, and what to answer with, in that order. The brief
// can be long, so the instruction it is followed by is repeated in one line
// at the end.
func anglePrompt[R Reference](angle string, brief *Brief[R], diffPath string, rules []Rule) (string, error) {
	instructions, err := angleInstructions(angle)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(angleFrame)
	out.WriteString("\n---\n\n")
	out.WriteString(instructions)
	if dismissed := NoiseSection(rules, brief.Ref.ReviewScope(), angle); dismissed != "" {
		out.WriteString("\n---\n\n")
		out.WriteString(dismissed)
	}
	out.WriteString("\n---\n\n")
	out.WriteString(brief.Text())
	out.WriteString("\n---\n\n")
	if diffPath == "" {
		out.WriteString("The diff was not gathered: read the files the brief's touched areas name.\n")
	} else {
		fmt.Fprintf(&out, "The diff is at %s: read it there.\n", diffPath)
	}
	fmt.Fprintf(&out, "\n---\n\nReview %s from the %s angle. Answer with the JSON object alone.\n", brief.Ref, angleTitles[angle])
	return out.String(), nil
}

// angleInstructions is what one angle looks for, read from its file under
// prompts/angles. An angle with no file is an error rather than a session
// told nothing: every name in BuiltinAngles has one.
func angleInstructions(angle string) (string, error) {
	data, err := angleInstructionFiles.ReadFile("prompts/angles/" + angle + ".md")
	if err != nil {
		return "", fmt.Errorf("angle %q: no instructions", angle)
	}
	return string(data), nil
}

// ResumePrompt is what a reopened angle session is asked. It has the brief,
// the diff and its own findings already: only the question is new.
func ResumePrompt(run AngleRun, question string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "A follow-up question from triage about your review from the %s angle:\n\n%s\n\n", angleTitles[run.Angle], strings.TrimSpace(question))
	out.WriteString("Answer it in a few sentences, reading the checkout again where the answer needs it. ")
	out.WriteString("Your session is still read-only. If answering turns up a finding you did not report, ")
	out.WriteString("add it after your answer as the same JSON object as before, holding the new findings only.\n")
	return out.String()
}

// WriteAngleRun writes one angle's run into a review's artifact directory,
// as AnglesDir/<angle>.json, creating the directories when they are not
// there.
func WriteAngleRun(artifact string, run *AngleRun) error {
	dir := filepath.Join(artifact, AnglesDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, run.Angle+".json"), append(data, '\n'), 0o644)
}

// ReadAngleRuns reads the angle runs back out of a review's artifact
// directory, in BuiltinAngles order: the angles that ran, and none for a
// review whose angles have not run yet. A file that is not a run is an
// error naming it.
func ReadAngleRuns(artifact string) ([]AngleRun, error) {
	var runs []AngleRun
	for _, angle := range BuiltinAngles {
		path := filepath.Join(artifact, AnglesDir, angle+".json")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var run AngleRun
		if err := json.Unmarshal(data, &run); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// SelectedAngles returns the enabled angles for size in catalog order.
func (a *Angles[R]) SelectedAngles(settings *Settings, size string) []string {
	if settings == nil {
		settings = &Settings{}
	}
	return anglesFor(settings, a.sizedAngles(size))
}
