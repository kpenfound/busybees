package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	core "github.com/kpenfound/busybees/core/review"
	"github.com/kpenfound/busybees/internal/config"
)

// The busybees angle adapter supplies configured read-only CLI agents and
// prepares GitHub checkouts for core/review's concurrent angle execution.
// Provider-specific session resumption stays here for triage.

// Angles runs the angle sessions of a review.
type Angles struct {
	// Agent runs the sessions; Provider and Model say which agent it is,
	// which is what every run records so it can be reopened by the same
	// one.
	Agent    Agent
	Provider string
	Model    string
	// Sized replaces the built-in angles of each size it names (core/review size selection),
	// config.toml's angles; a size it leaves out keeps the built-in list.
	Sized map[string][]string
	// Models is the model of each angle it names, config.toml's
	// angle_models: that angle's session runs as a copy of Agent with the
	// model replaced, when Agent is the CLI agent. Factory Agents overrides
	// take precedence and can select a different provider.
	Models map[string]string
	// Agents is the agent of each angle it names, which may run another
	// provider: the factory's roles.reviewer.angle_profiles, or config.toml's
	// angle_profiles. It takes precedence over Models.
	Agents map[string]*CLIAgent
	// Checkout clones the pull request's head into the artifact directory
	// for the sessions to run in (checkout.go), which is where they run
	// whenever it succeeds. It is nil to attempt none; a clone already
	// under the artifact directory, made as the context was gathered, is
	// run in either way.
	Checkout *Checkout
	// Dir is the checkout of the repository under review the machine has,
	// which the sessions run in when Checkout is nil or could not make
	// one: whatever branch it has checked out, which need not be the pull
	// request's. It is "" on a machine that has none (see CheckoutOf), and
	// the sessions then run in an empty directory inside the artifact
	// directory: a review must not read whichever repository the command
	// happened to be run in, and the directory has to outlive the run for
	// the sessions to be reopened.
	Dir string
	// Log is where a checkout that could not be made is reported, with
	// where the sessions run instead, and nowhere when nil.
	Log io.Writer
	// Rules are the reviewer notes' rules (notes.go), read by whoever runs
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
type AngleEvent = core.AngleEvent

const (
	AngleStarted  = core.AngleStarted
	AngleFinished = core.AngleFinished
	AngleFailed   = core.AngleFailed
)

// NewAngles is the angle runner of a review, as the global configuration
// says: cfg's provider and model, the angles and per-angle models cfg sets,
// the profile of each angle cfg's angle_profiles names, a checkout of the pull request's head
// authenticated by cfg's github.token, and dir the checkout Open was given
// for when that one cannot be made.
func NewAngles(cfg *Config, dir string) *Angles {
	agent := NewAgent(cfg)
	checkout := &Checkout{}
	if cfg != nil {
		checkout.Token = cfg.GitHub.ResolvedToken()
	}
	a := &Angles{Agent: agent, Provider: agent.Provider, Model: agent.Model, Checkout: checkout, Dir: dir}
	if cfg != nil {
		a.Sized, a.Models = cfg.Angles, cfg.AngleModels
		for angle, name := range cfg.AngleProfiles {
			if a.Agents == nil {
				a.Agents = map[string]*CLIAgent{}
			}
			a.Agents[angle] = cfg.profileAgent(name)
		}
	}
	return a
}

// agentFor selects an Agents override first, then a Models
// override on a copy of the CLI agent, then the default Agent and Model.
func (a *Angles) agentFor(angle string) (Agent, string) {
	if agent := a.Agents[angle]; agent != nil {
		return agent, agent.Model
	}
	model := a.Models[angle]
	cli, ok := a.Agent.(*CLIAgent)
	if model == "" || !ok {
		return a.Agent, a.Model
	}
	copied := *cli
	copied.Model = model
	return &copied, model
}

// dir is the directory the sessions run in: the checkout of ref's head
// under artifact when there is one already or Checkout makes one, else
// Dir, else the scratch directory under artifact, made if it is not
// there. A checkout the angles make is of the head alone: the base is the
// diff source's need, and the diff is gathered by the time they run.
func (a *Angles) dir(ctx context.Context, artifact string, ref Ref) (string, error) {
	if dir := filepath.Join(artifact, CheckoutDir); isDir(dir) {
		return dir, nil
	}
	if a.Checkout != nil {
		dir := filepath.Join(artifact, CheckoutDir)
		err := a.Checkout.Run(ctx, ref, "", dir)
		if err == nil {
			a.logf("checked out %s under %s", ref, dir)
			return dir, nil
		}
		where := "an empty directory"
		if a.Dir != "" {
			where = a.Dir
		}
		a.logf("could not check out %s in a container: %v; the angles run in %s", ref, err, where)
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

// isDir reports whether path is a directory that is there.
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// logf writes one line to Log.
func (a *Angles) logf(format string, args ...any) {
	if a.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(a.Log, format+"\n", args...)
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
	case run.Provider != a.providerFor(run.Angle):
		return nil, fmt.Errorf("the %s angle ran as %s and the configured provider is %s, which cannot resume its session", run.Angle, run.Provider, a.providerFor(run.Angle))
	}
	agent, _ := a.agentFor(run.Angle)
	return agent.Run(ctx, AgentRequest{Name: run.Angle, Prompt: core.ResumePrompt(run, question), Dir: run.Dir, ResumeID: run.SessionID})
}

// core supplies execution settings and a directory prepared by the GitHub adapter.
func (a *Angles) core(ref Ref) *core.Angles[Ref] {
	return &core.Angles[Ref]{Agent: a.Agent, Provider: a.Provider, Model: a.Model, Sized: a.Sized,
		Dir: a.Dir, Log: a.Log, Rules: coreRules(a.Rules), Progress: a.Progress, AgentFor: a.agentFor,
		ProviderFor: a.providerFor,
		Prepare:     func(ctx context.Context, artifact string) (string, error) { return a.dir(ctx, artifact, ref) },
	}
}

// providerFor is the provider the angle's session runs as.
func (a *Angles) providerFor(angle string) string {
	if agent := a.Agents[angle]; agent != nil {
		return agent.provider()
	}
	return a.Provider
}

func (a *Angles) Run(ctx context.Context, artifact string, project *Project, brief *Brief, diff string) ([]AngleRun, error) {
	if brief == nil {
		return nil, errors.New("angles: no brief to review from")
	}
	return a.core(brief.Ref).Run(ctx, artifact, project.core(), brief, diff)
}

type AngleRun = core.AngleRun

const AnglesDir = core.AnglesDir
const CheckoutDir = core.CheckoutDir
const ScratchDir = core.ScratchDir
const DiffFile = core.DiffFile

var WriteAngleRun = core.WriteAngleRun
var ReadAngleRuns = core.ReadAngleRuns
