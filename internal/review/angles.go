package review

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kpenfound/busybees/internal/config"
)

// The angle sessions. Once the distiller has written the brief, one session
// per angle the project enables (Project.EnabledAngles) reads it and looks
// for problems from that angle alone: the acceptance criteria, the tests and
// documentation, the style rules, the side effects. They all run at once,
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
	AngleAcceptance:  "acceptance criteria",
	AngleTests:       "test coverage and documentation",
	AngleStyle:       "style",
	AngleSideEffects: "side effects",
}

// Names inside a review's artifact directory: AnglesDir is the directory
// the angle runs are written in, one JSON file per angle named after it,
// and ScratchDir is the empty directory the sessions run in when there is
// no checkout of the repository under review.
const (
	AnglesDir  = "angles"
	ScratchDir = "scratch"
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
	// Dir is the directory the session ran in, which a reopened session
	// runs in too: claude keeps a session's transcript under the directory
	// it ran in, and finds it again from there.
	Dir string `json:"dir"`
	// SessionID is the agent's own id for the session, which Resume gives
	// back to it. It is empty for a session that failed.
	SessionID string `json:"session_id,omitempty"`
	// Answer is what the session said last, as it said it: the findings it
	// was asked for, in the JSON shape the prompt describes. Reading them
	// into findings is the judge's job (judge.go), not this file's.
	Answer string `json:"answer,omitempty"`
	// Turns is how many turns the session took and CostUSD what it cost,
	// when the CLI reported one.
	Turns   int     `json:"turns,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Error is what the session failed with, and "" for one that finished.
	// A failed angle is kept with the rest so the review can say which
	// angle nobody looked from.
	Error string `json:"error,omitempty"`
}

// Failed reports whether the session failed, in which case there is no
// answer and nothing to resume.
func (r *AngleRun) Failed() bool { return r.Error != "" }

// Angles runs the angle sessions of a review.
type Angles struct {
	// Agent runs the sessions; Provider and Model say which agent it is,
	// which is what every run records so it can be reopened by the same
	// one.
	Agent    Agent
	Provider string
	Model    string
	// Dir is the checkout of the repository under review, which the
	// sessions' read-only tools can reach. It is "" on a machine that has
	// none (see CheckoutOf), and the sessions then run in an empty
	// directory inside the artifact directory: a review must not read
	// whichever repository the command happened to be run in, and the
	// directory has to outlive the run for the sessions to be reopened.
	Dir string
}

// NewAngles is the angle runner of a review, as the global configuration
// says: cfg's provider and model, and dir the checkout Open was given.
func NewAngles(cfg *Config, dir string) *Angles {
	agent := NewAgent(cfg)
	return &Angles{Agent: agent, Provider: agent.Provider, Model: agent.Model, Dir: dir}
}

// Run fans out one session per angle project enables, all at once, each
// given the brief and the diff, and waits for every one of them. The runs
// come back in BuiltinAngles order and are written into artifact, one file
// per angle, before Run returns.
//
// One angle failing stops none of the others: the runs are complete whether
// or not err is nil, a failed angle is among them with Error set and no
// answer, and err says so once every angle has ended. An error before any
// session ran (no brief, a directory that could not be made) returns no
// runs. diff is the pull request's diff, and "" when it was not gathered.
func (a *Angles) Run(ctx context.Context, artifact string, project *Project, brief *Brief, diff string) ([]AngleRun, error) {
	if brief == nil {
		return nil, errors.New("angles: no brief to review from")
	}
	if project == nil {
		project = &Project{}
	}
	angles := project.EnabledAngles()
	if len(angles) == 0 {
		return nil, nil
	}
	dir := a.Dir
	if dir == "" {
		dir = filepath.Join(artifact, ScratchDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	prompts := make([]string, len(angles))
	for i, angle := range angles {
		prompt, err := anglePrompt(angle, brief, diff)
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
			run := AngleRun{Angle: angle, Provider: a.Provider, Model: a.Model, Dir: dir}
			res, err := a.Agent.Run(ctx, AgentRequest{Name: angle, Prompt: prompts[i], Dir: dir})
			if err != nil {
				run.Error = err.Error()
			} else {
				run.SessionID, run.Answer, run.Turns, run.CostUSD = res.ID, res.Text, res.Turns, res.CostUSD
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

// Resume reopens an angle's session with a follow-up question, for the
// triage action that asks one, and returns what the session answered. The
// session runs where it ran before, as the agent that ran it, under the same
// read-only restriction. An angle that failed has no session to reopen, and
// codex has no resume: both are errors, as is an agent other than the one
// the run records, which would start a session that had read nothing.
func (a *Angles) Resume(ctx context.Context, run AngleRun, question string) (*AgentResult, error) {
	switch {
	case run.Failed() || run.SessionID == "":
		return nil, fmt.Errorf("the %s angle's session did not finish, so there is nothing to resume: run the review again", run.Angle)
	case run.Provider == config.AgentCodex:
		return nil, fmt.Errorf("the %s angle ran as codex, which cannot resume a session", run.Angle)
	case run.Provider != a.Provider:
		return nil, fmt.Errorf("the %s angle ran as %s and the configured provider is %s, which cannot resume its session", run.Angle, run.Provider, a.Provider)
	}
	return a.Agent.Run(ctx, AgentRequest{Name: run.Angle, Prompt: resumePrompt(run, question), Dir: run.Dir, ResumeID: run.SessionID})
}

// anglePrompt is the whole of what an angle session is told: how a review
// session behaves, what this angle looks for, the brief, the diff when
// there is one, and what to answer with, in that order. The brief and the
// diff can be long, so the instruction they follow is repeated in one line
// at the end.
func anglePrompt(angle string, brief *Brief, diff string) (string, error) {
	instructions, err := angleInstructions(angle)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(angleFrame)
	out.WriteString("\n---\n\n")
	out.WriteString(instructions)
	out.WriteString("\n---\n\n")
	out.WriteString(brief.Text())
	out.WriteString("\n---\n\n")
	if strings.TrimSpace(diff) == "" {
		out.WriteString("The diff was not gathered: read the files the brief's touched areas name.\n")
	} else {
		fmt.Fprintf(&out, "## Diff\n\n%s\n", fenced(diff))
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

// resumePrompt is what a reopened angle session is asked. It has the brief,
// the diff and its own findings already: only the question is new.
func resumePrompt(run AngleRun, question string) string {
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
