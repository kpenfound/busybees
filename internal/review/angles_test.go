package review

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// fakeAngleAgent stands in for the coding agent under a fan-out: it records
// every session it was asked to run, by name, and can hold each one until
// expect of them have started, which is how a test tells sessions run at
// once from sessions run one after another.
type fakeAngleAgent struct {
	expect int
	fail   map[string]error

	mu      sync.Mutex
	reqs    map[string]AgentRequest
	order   []string
	started int
	all     chan struct{}
}

func newFakeAngleAgent(expect int) *fakeAngleAgent {
	return &fakeAngleAgent{expect: expect, reqs: map[string]AgentRequest{}, all: make(chan struct{})}
}

func (f *fakeAngleAgent) Run(_ context.Context, req AgentRequest) (*AgentResult, error) {
	f.mu.Lock()
	f.reqs[req.Name] = req
	f.order = append(f.order, req.Name)
	f.started++
	if f.started == f.expect {
		close(f.all)
	}
	f.mu.Unlock()
	if f.expect > 0 {
		select {
		case <-f.all:
		case <-time.After(5 * time.Second):
			return nil, errors.New(req.Name + " session: the other angles never started, so the angles were not run at once")
		}
	}
	if err := f.fail[req.Name]; err != nil {
		return nil, err
	}
	return &AgentResult{ID: "sess-" + req.Name, Text: `{"findings": []}`, Turns: 2, CostUSD: 0.25}, nil
}

func projectWith(t *testing.T, text string) *Project {
	t.Helper()
	p, err := ParseProject(text, filepath.Join(t.TempDir(), ProjectFile))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// onlyAngle is a project that turns off every angle but one.
func onlyAngle(t *testing.T, angle string) *Project {
	t.Helper()
	toml := "[angles]\n"
	for _, a := range BuiltinAngles {
		if a != angle {
			toml += a + " = false\n"
		}
	}
	return projectWith(t, toml)
}

const testDiff = "diff --git a/gather.go b/gather.go\n+func Gather() {}\n"

// briefAngles are the angles testBrief's size, m, is reviewed from by a
// project that turns none of them off.
var briefAngles = []string{AngleGeneral, AngleDocs, AngleTests, AngleAcceptance}

// sized is testBrief with another size.
func sized(size string) *Brief {
	b := testBrief()
	b.Size = size
	return b
}

// ranAngles are the angles runs are of, in the order they came back.
func ranAngles(runs []AngleRun) []string {
	var got []string
	for _, r := range runs {
		got = append(got, r.Angle)
	}
	return got
}

func TestAnAngleSessionIsToldItsAngleTheBriefAndTheDiff(t *testing.T) {
	// No one size runs every angle: xs and xl between them do. The diff is
	// in every prompt. A checkout (fakeDocker) is what makes diff.patch's
	// directory one the angles' read-only tools can reach: without one the
	// angles fall back to the machine's own checkout, which diff.patch is
	// not written into (TestTheDiffIsNotWrittenIntoTheMachinesCheckout).
	reqs := map[string]AgentRequest{}
	diffPaths := map[string]string{}
	for _, size := range []string{"xs", "xl"} {
		docker := fakeDocker(t)
		agent := newFakeAngleAgent(len((&Angles{}).core(Ref{}).SelectedAngles(nil, size)))
		artifact := t.TempDir()
		angles := &Angles{Agent: agent, Dir: t.TempDir(), Checkout: &Checkout{DockerBin: docker}}
		if _, err := angles.Run(context.Background(), artifact, &Project{}, sized(size), testDiff); err != nil {
			t.Fatal(err)
		}
		maps.Copy(reqs, agent.reqs)
		for _, angle := range (&Angles{}).core(Ref{}).SelectedAngles(nil, size) {
			diffPaths[angle] = filepath.Join(artifact, CheckoutDir, DiffFile)
		}
	}
	if len(reqs) != len(BuiltinAngles) {
		t.Fatalf("%d angles ran, want all %d", len(reqs), len(BuiltinAngles))
	}
	for angle, heading := range map[string]string{
		AngleQuickGeneral: "## Your angle: quick general",
		AngleGeneral:      "## Your angle: general",
		AngleDocs:         "## Your angle: documentation accuracy",
		AngleAcceptance:   "## Your angle: acceptance criteria",
		AngleTests:        "## Your angle: test coverage and documentation",
		AngleSideEffects:  "## Your angle: side effects",
	} {
		prompt := reqs[angle].Prompt
		diffPath := diffPaths[angle]
		if data, err := os.ReadFile(diffPath); err != nil {
			t.Errorf("the %s angle's diff was not written to %s: %v", angle, diffPath, err)
		} else if string(data) != testDiff {
			t.Errorf("%s: %s = %q, want %q", angle, diffPath, data, testDiff)
		}
		for _, want := range []string{
			"You are one session of a pull request review",
			"Your session is read-only.",
			heading,
			"# Review brief: acme/widgets#7",
			"- every new key needs a test (CLAUDE.md)",
			"The diff of the change:\n\n```diff\n" + testDiff,
			"The same diff is at " + diffPath + ", for searching.",
			"Review acme/widgets#7 from the " + map[string]string{AngleQuickGeneral: "quick general", AngleSideEffects: "side effects", AngleGeneral: "general", AngleDocs: "documentation accuracy", AngleTests: "test coverage and documentation", AngleAcceptance: "acceptance criteria"}[angle] + " angle. Answer with the JSON object alone.\n",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("the %s session was not told %q:\n%s", angle, want, prompt)
			}
		}
		// The frame, the angle, the brief, the diff, the closing line: in
		// that order, so the instructions come before what can be long.
		last := -1
		for _, mark := range []string{"You are one session", heading, "# Review brief", "The diff of the change", "Answer with the JSON object alone"} {
			at := strings.Index(prompt, mark)
			if at <= last {
				t.Errorf("the %s session's prompt has %q out of order:\n%s", angle, mark, prompt)
			}
			last = at
		}

	}
}

func TestAnAngleIsResumedWhereItRanWithTheQuestion(t *testing.T) {
	agent := newFakeAngleAgent(0)
	angles := &Angles{Agent: agent, Provider: config.AgentClaude, Dir: t.TempDir()}
	run := AngleRun{Angle: AngleSideEffects, Provider: config.AgentClaude, Dir: "/where/it/ran", SessionID: "sess-9", Answer: `{"findings": []}`}
	res, err := angles.Resume(context.Background(), run, "Does anything else read this file?")
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "sess-"+AngleSideEffects {
		t.Errorf("result = %+v, want what the reopened session answered", res)
	}
	req := agent.reqs[AngleSideEffects]
	if req.ResumeID != "sess-9" {
		t.Errorf("resume id = %q, want the run's session id", req.ResumeID)
	}
	// Where the run says, not where this review's checkout is: the agent
	// finds the session under the directory it ran in.
	if req.Dir != "/where/it/ran" {
		t.Errorf("resumed in %q, want where the run says it ran", req.Dir)
	}
	for _, want := range []string{"Does anything else read this file?", "from the side effects angle", "still read-only"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("the reopened session was not told %q:\n%s", want, req.Prompt)
		}
	}
	if strings.Contains(req.Prompt, "You are one session of a pull request review") {
		t.Errorf("the reopened session was given the whole frame again:\n%s", req.Prompt)
	}
}

func TestAnAngleThatCannotBeResumedSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		run      AngleRun
		want     string
	}{
		{"a failed angle", config.AgentClaude, AngleRun{Angle: AngleGeneral, Provider: config.AgentClaude, Dir: "/d", Error: "general session: no capacity"}, "did not finish"},
		{"no session id", config.AgentClaude, AngleRun{Angle: AngleGeneral, Provider: config.AgentClaude, Dir: "/d"}, "did not finish"},
		{"a codex session", config.AgentCodex, AngleRun{Angle: AngleGeneral, Provider: config.AgentCodex, Dir: "/d", SessionID: "thread-1"}, "codex, which cannot resume"},
		{"another agent's session", config.AgentCodex, AngleRun{Angle: AngleGeneral, Provider: config.AgentClaude, Dir: "/d", SessionID: "sess-1"}, "ran as claude and the configured provider is codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newFakeAngleAgent(0)
			_, err := (&Angles{Agent: agent, Provider: tc.provider}).Resume(context.Background(), tc.run, "why?")
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), AngleGeneral) {
				t.Fatalf("err = %v, want %q and the angle named", err, tc.want)
			}
			if len(agent.reqs) != 0 {
				t.Errorf("a session ran: %v", agent.order)
			}
		})
	}
}

func TestAnAngleSessionIsReadOnly(t *testing.T) {
	// The restriction is the agent's, and an angle session gets it by
	// running through the agent: this is the fan-out driving the real CLI
	// command line, not a fake agent.
	bin, record := fakeCLI(t, claudeAnswer)
	project := onlyAngle(t, AngleGeneral)
	angles := &Angles{Agent: &CLIAgent{ClaudeBin: bin}, Provider: config.AgentClaude, Dir: t.TempDir()}
	runs, err := angles.Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].SessionID != "sess-1" || runs[0].Answer != "the brief" {
		t.Fatalf("runs = %+v, want the one angle's session read", runs)
	}
	got := args(t, record)
	for _, want := range []string{
		"\n--allowedTools\nRead,Grep,Glob,LS,NotebookRead\n",
		"\n--disallowedTools\nBash,BashOutput,Edit,KillShell,MultiEdit,NotebookEdit,Task,WebFetch,WebSearch,Write\n",
		"\n--permission-prompts\nnone\n",
		"\n--strict-mcp-config\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the angle session was not given %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("an angle session skipped permissions:\n%s", got)
	}
	if prompt := recorded(t, record, "stdin"); !strings.Contains(prompt, "## Your angle: general") {
		t.Errorf("the CLI was not given the angle's prompt:\n%s", prompt)
	}
}

func TestTheAnglesRunTheConfiguredAgent(t *testing.T) {
	cfg, err := ParseConfig("provider = \"codex\"\nmodel = \"gpt-5\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	a := NewAngles(cfg, "/checkout")
	agent, ok := a.Agent.(*CLIAgent)
	if !ok {
		t.Fatalf("agent = %T, want the CLI agent", a.Agent)
	}
	if agent.Provider != "codex" || a.Provider != "codex" || a.Model != "gpt-5" || a.Dir != "/checkout" {
		t.Errorf("angles = %+v, %+v, want the configured provider and model and the checkout", agent, a)
	}
}

func TestConfiguredAnglesReplaceASizesBuiltinList(t *testing.T) {
	cfg, err := ParseConfig("[angles]\nxs = [\"quick_general\", \"side_effects\"]\nxl = [\"docs\"]\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		size    string
		project *Project
		want    []string
	}{
		{"the size's own list", "xs", &Project{}, []string{AngleQuickGeneral, AngleSideEffects}},
		{"still filtered by context.toml", "xs", projectWith(t, "[angles]\nside_effects = false\n"), []string{AngleQuickGeneral}},
		{"a size the file leaves out keeps the built-in list", "s", &Project{}, []string{AngleQuickGeneral, AngleDocs}},
		{"a size that is not one gets the largest's", "huge", &Project{}, []string{AngleDocs}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			angles := NewAngles(cfg, t.TempDir())
			angles.Checkout = nil
			angles.Agent = newFakeAngleAgent(len(tc.want))
			runs, err := angles.Run(context.Background(), t.TempDir(), tc.project, sized(tc.size), testDiff)
			if err != nil {
				t.Fatal(err)
			}
			if got := ranAngles(runs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("angles run = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOnlyTheQuickPassRunsForAnXSChangeConfiguredSo(t *testing.T) {
	cfg, err := ParseConfig("[angles]\nxs = [\"quick_general\"]\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	a := NewAngles(cfg, "")
	if got := a.core(Ref{}).SelectedAngles(nil, "xs"); !reflect.DeepEqual(got, []string{AngleQuickGeneral}) {
		t.Errorf("xs angles = %v, want the quick pass alone", got)
	}
	if got := NewAngles(nil, "").core(Ref{}).SelectedAngles(nil, "xs"); !reflect.DeepEqual(got, []string{AngleQuickGeneral, AngleDocs}) {
		t.Errorf("xs angles with no configuration = %v, want the built-in list", got)
	}
}

func TestAnAngleModelRunsThatAngleAsItsModel(t *testing.T) {
	cfg, err := ParseConfig("model = \"opus\"\n[angle_models]\ndocs = \"haiku\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ angle, want string }{
		{AngleDocs, "haiku"},
		{AngleGeneral, "opus"},
	} {
		t.Run(tc.angle, func(t *testing.T) {
			bin, record := fakeCLI(t, claudeAnswer)
			angles := NewAngles(cfg, t.TempDir())
			angles.Checkout = nil
			angles.Agent.(*CLIAgent).ClaudeBin = bin
			runs, err := angles.Run(context.Background(), t.TempDir(), onlyAngle(t, tc.angle), testBrief(), testDiff)
			if err != nil {
				t.Fatal(err)
			}
			if got := args(t, record); !strings.Contains(got, "\n--model\n"+tc.want+"\n") {
				t.Errorf("the %s session was not run as %s:\n%s", tc.angle, tc.want, got)
			}
			if len(runs) != 1 || runs[0].Model != tc.want || runs[0].Provider != config.AgentClaude {
				t.Errorf("runs = %+v, want the %s run recorded as %s", runs, tc.angle, tc.want)
			}
			// A resumed session runs as the model it ran as.
			if _, err := angles.Resume(context.Background(), runs[0], "why?"); err != nil {
				t.Fatal(err)
			}
			if got := args(t, record); !strings.Contains(got, "\n--model\n"+tc.want+"\n") || !strings.Contains(got, "\n--resume\n") {
				t.Errorf("the resumed %s session was not run as %s:\n%s", tc.angle, tc.want, got)
			}
			if angles.Model != "opus" || angles.Agent.(*CLIAgent).Model != "opus" {
				t.Errorf("the override changed the shared agent: %+v", angles.Agent)
			}
		})
	}
}

func TestAnAngleSessionIsToldWhatItsReviewerDismissedBefore(t *testing.T) {
	agent := newFakeAngleAgent(len(briefAngles))
	angles := &Angles{Agent: agent, Dir: t.TempDir(), Rules: []Rule{
		{Repo: testRepo, Angle: AngleGeneral, Category: "naming", Action: RuleDrop, Text: "receiver names are short here"},
		{Repo: "acme/gadgets", Angle: AngleGeneral, Action: RuleDrop, Text: "another repository's notes"},
	}}
	if _, err := angles.Run(context.Background(), t.TempDir(), &Project{}, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	prompt := agent.reqs[AngleGeneral].Prompt
	for _, want := range []string{"## Dismissed before", "- receiver names are short here"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the general session was not told %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "another repository's notes") {
		t.Errorf("the general session was told a rule about another repository:\n%s", prompt)
	}
	// After what the angle looks for, before the brief: the session is told
	// what was dismissed from its angle, not from the change.
	if at := strings.Index(prompt, "## Dismissed before"); at < strings.Index(prompt, "## Your angle: general") || at > strings.Index(prompt, "# Review brief") {
		t.Errorf("the dismissals are out of order in the general session's prompt:\n%s", prompt)
	}
	for _, angle := range BuiltinAngles {
		if angle != AngleGeneral && strings.Contains(agent.reqs[angle].Prompt, "## Dismissed before") {
			t.Errorf("the %s session was told what the general angle's reviewer dismissed", angle)
		}
	}
}

// progressRecorder records what Angles.Progress was told, from every
// angle's goroutine at once.
type progressRecorder struct {
	mu     sync.Mutex
	events map[string][]AngleEvent
}

func newProgressRecorder() *progressRecorder {
	return &progressRecorder{events: map[string][]AngleEvent{}}
}

func (p *progressRecorder) record(angle string, event AngleEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events[angle] = append(p.events[angle], event)
}

// An angle whose session fell back to another agent is recorded under the
// agent and model that answered, so the artifact names the session that
// exists, and a resume of it is refused the way a codex session's is rather
// than run as claude with a codex thread id.
func TestAnAngleThatFellBackIsRecordedUnderTheAgentThatAnswered(t *testing.T) {
	limited, _ := fakeCLI(t, `echo '{"type":"result","subtype":"error","is_error":true,"result":"Rate limit reached for opus","session_id":"sess-0","num_turns":0}'`)
	answering, _ := fakeCLI(t, codexAnswer)
	agent := &CLIAgent{ClaudeBin: limited, Model: "opus", Fallback: &CLIAgent{Provider: config.AgentCodex, CodexBin: answering, Model: "gpt-cheap"}}
	angles := &Angles{Agent: agent, Provider: config.AgentClaude, Model: "opus", Dir: t.TempDir()}
	runs, err := angles.Run(context.Background(), t.TempDir(), onlyAngle(t, AngleDocs), testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Provider != config.AgentCodex || runs[0].Model != "gpt-cheap" || runs[0].SessionID != "thread-9" {
		t.Fatalf("runs = %+v, want the docs run recorded as codex on gpt-cheap with its thread id", runs)
	}
	if _, err := angles.Resume(context.Background(), runs[0], "why?"); err == nil || !strings.Contains(err.Error(), "codex, which cannot resume") {
		t.Fatalf("resume of a fallen-back angle: %v, want it refused as a codex session", err)
	}
}
