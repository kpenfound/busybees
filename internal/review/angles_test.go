package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

func TestOneSessionPerEnabledAngleRunsAtOnce(t *testing.T) {
	project := projectWith(t, "[angles]\ngeneral = false\n")
	agent := newFakeAngleAgent(5)
	angles := &Angles{Agent: agent, Provider: config.AgentClaude, Model: "opus", Dir: t.TempDir()}
	runs, err := angles.Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	// One session per enabled angle, in BuiltinAngles order, and not one
	// for the angle the project turned off.
	var got []string
	for _, r := range runs {
		got = append(got, r.Angle)
	}
	want := []string{AngleQuickGeneral, AngleDocs, AngleTests, AngleAcceptance, AngleSideEffects}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("angles run = %v, want %v", got, want)
	}
	if len(agent.reqs) != 5 {
		t.Fatalf("%d sessions ran (%v), want 5", len(agent.reqs), agent.order)
	}
	if _, ran := agent.reqs[AngleGeneral]; ran {
		t.Errorf("the general angle ran with general = false")
	}
	for _, r := range runs {
		if r.SessionID != "sess-"+r.Angle || r.Answer != `{"findings": []}` || r.Turns != 2 || r.CostUSD != 0.25 {
			t.Errorf("%s run = %+v, want what its session came to", r.Angle, r)
		}
		if r.Provider != config.AgentClaude || r.Model != "opus" || r.Dir != angles.Dir {
			t.Errorf("%s run = %+v, want the agent and the directory it ran as recorded", r.Angle, r)
		}
		if r.Failed() {
			t.Errorf("%s run failed: %s", r.Angle, r.Error)
		}
	}
}

func TestEveryAngleOffRunsNothing(t *testing.T) {
	project := projectWith(t, "[angles]\nquick_general = false\ngeneral = false\ndocs = false\ntest_coverage = false\nacceptance_criteria = false\nside_effects = false\n")
	agent := newFakeAngleAgent(0)
	runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
	if err != nil || runs != nil {
		t.Fatalf("runs, err = %v, %v, want none and no error", runs, err)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("%d sessions ran with every angle off", len(agent.reqs))
	}
}

func TestAProjectWithNoConfigurationRunsEveryAngle(t *testing.T) {
	agent := newFakeAngleAgent(len(BuiltinAngles))
	for _, project := range []*Project{nil, {}} {
		runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != len(BuiltinAngles) {
			t.Errorf("%d angles ran for project %+v, want all %d", len(runs), project, len(BuiltinAngles))
		}
	}
}

func TestAnAngleSessionIsToldItsAngleTheBriefAndTheDiff(t *testing.T) {
	agent := newFakeAngleAgent(len(BuiltinAngles))
	if _, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), &Project{}, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	for angle, heading := range map[string]string{
		AngleQuickGeneral: "## Your angle: quick general",
		AngleGeneral:      "## Your angle: general",
		AngleDocs:         "## Your angle: documentation accuracy",
		AngleAcceptance:   "## Your angle: acceptance criteria",
		AngleTests:        "## Your angle: test coverage and documentation",
		AngleSideEffects:  "## Your angle: side effects",
	} {
		prompt := agent.reqs[angle].Prompt
		for _, want := range []string{
			"You are one session of a pull request review",
			"Your session is read-only.",
			heading,
			"# Review brief: acme/widgets#7",
			"- every new key needs a test (CLAUDE.md)",
			"## Diff\n\n```\n" + strings.TrimRight(testDiff, "\n") + "\n```",
			"Review acme/widgets#7 from the " + angleTitles[angle] + " angle. Answer with the JSON object alone.\n",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("the %s session was not told %q:\n%s", angle, want, prompt)
			}
		}
		// The frame, the angle, the brief, the diff, the closing line: in
		// that order, so the instructions come before what can be long.
		last := -1
		for _, mark := range []string{"You are one session", heading, "# Review brief", "## Diff", "Answer with the JSON object alone"} {
			at := strings.Index(prompt, mark)
			if at <= last {
				t.Errorf("the %s session's prompt has %q out of order:\n%s", angle, mark, prompt)
			}
			last = at
		}
		for _, other := range BuiltinAngles {
			if other != angle && strings.Contains(prompt, "## Your angle: "+angleTitles[other]) {
				t.Errorf("the %s session was also told the %s angle's instructions", angle, other)
			}
		}
	}
}

func TestWithoutADiffTheSessionIsToldSo(t *testing.T) {
	agent := newFakeAngleAgent(1)
	project := onlyAngle(t, AngleGeneral)
	if _, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, testBrief(), " \n"); err != nil {
		t.Fatal(err)
	}
	prompt := agent.reqs[AngleGeneral].Prompt
	if !strings.Contains(prompt, "The diff was not gathered") || strings.Contains(prompt, "## Diff") {
		t.Errorf("a session with no diff was not told so:\n%s", prompt)
	}
}

func TestEveryBuiltinAngleHasInstructions(t *testing.T) {
	for _, angle := range BuiltinAngles {
		got, err := angleInstructions(angle)
		if err != nil {
			t.Errorf("%s: %v", angle, err)
			continue
		}
		if !strings.HasPrefix(got, "## Your angle: "+angleTitles[angle]+"\n") {
			t.Errorf("%s's instructions do not open with its heading:\n%s", angle, got)
		}
	}
	if _, err := angleInstructions("vibes"); err == nil {
		t.Error("an angle with no instructions has some")
	}
}

// The instruction files and the titles are the catalog: one of each per
// angle in BuiltinAngles, and none for an angle that is not in it, such as
// the style angle the catalog no longer has.
func TestTheInstructionFilesAreTheCatalog(t *testing.T) {
	entries, err := angleInstructionFiles.ReadDir("prompts/angles")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, strings.TrimSuffix(e.Name(), ".md"))
	}
	if want := slices.Sorted(slices.Values(BuiltinAngles)); !reflect.DeepEqual(got, want) {
		t.Errorf("instruction files for %v, want one for each of %v", got, want)
	}
	for angle := range angleTitles {
		if !slices.Contains(BuiltinAngles, angle) {
			t.Errorf("a title for %q, which is not an angle", angle)
		}
	}
	if len(angleTitles) != len(BuiltinAngles) {
		t.Errorf("%d titles for %d angles", len(angleTitles), len(BuiltinAngles))
	}
}

func TestAnAngleThatFailedDoesNotStopTheOthers(t *testing.T) {
	agent := newFakeAngleAgent(len(BuiltinAngles))
	agent.fail = map[string]error{AngleGeneral: errors.New("general session: no capacity")}
	artifact := t.TempDir()
	runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
	if err == nil || !strings.Contains(err.Error(), "general session: no capacity") {
		t.Fatalf("err = %v, want the failed angle's error", err)
	}
	if len(runs) != len(BuiltinAngles) {
		t.Fatalf("%d runs came back, want every angle whether or not it failed", len(runs))
	}
	for _, r := range runs {
		switch {
		case r.Angle == AngleGeneral && (!r.Failed() || r.Error != "general session: no capacity" || r.SessionID != "" || r.Answer != ""):
			t.Errorf("general run = %+v, want the failure and nothing else", r)
		case r.Angle != AngleGeneral && (r.Failed() || r.Answer == ""):
			t.Errorf("%s run = %+v, want it to have finished", r.Angle, r)
		}
		if _, err := os.Stat(filepath.Join(artifact, AnglesDir, r.Angle+".json")); err != nil {
			t.Errorf("%s run not persisted: %v", r.Angle, err)
		}
	}
}

func TestAngleRunsArePersistedAndReadBack(t *testing.T) {
	agent := newFakeAngleAgent(len(BuiltinAngles))
	artifact := filepath.Join(t.TempDir(), "review-7")
	want, err := (&Angles{Agent: agent, Provider: config.AgentClaude, Dir: t.TempDir()}).Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadAngleRuns(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back\n%+v\nwant\n%+v", got, want)
	}
	// Enough to reopen each session: the agent, its id and where it ran.
	for _, r := range got {
		if r.Provider == "" || r.SessionID == "" || r.Dir == "" {
			t.Errorf("%s run = %+v, want the provider, the session id and the directory", r.Angle, r)
		}
	}
}

func TestReadingAngleRunsThatAreNotThere(t *testing.T) {
	runs, err := ReadAngleRuns(t.TempDir())
	if err != nil || runs != nil {
		t.Fatalf("runs, err = %v, %v, want none and no error for a review whose angles have not run", runs, err)
	}
	artifact := t.TempDir()
	if err := WriteAngleRun(artifact, &AngleRun{Angle: AngleTests, Provider: config.AgentClaude, SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	runs, err = ReadAngleRuns(artifact)
	if err != nil || len(runs) != 1 || runs[0].Angle != AngleTests {
		t.Fatalf("runs, err = %+v, %v, want the one angle that ran", runs, err)
	}
	bad := filepath.Join(artifact, AnglesDir, AngleGeneral+".json")
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAngleRuns(artifact); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("err = %v, want the file named", err)
	}
}

func TestWithoutACheckoutTheAnglesRunInAScratchDirectoryThatStays(t *testing.T) {
	agent := newFakeAngleAgent(len(BuiltinAngles))
	artifact := filepath.Join(t.TempDir(), "review-7")
	runs, err := (&Angles{Agent: agent}).Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(artifact, ScratchDir)
	for _, r := range runs {
		if agent.reqs[r.Angle].Dir != want || r.Dir != want {
			t.Errorf("%s ran in %q and recorded %q, want the scratch directory %s", r.Angle, agent.reqs[r.Angle].Dir, r.Dir, want)
		}
	}
	// The sessions are reopened from where they ran, so the directory is
	// not removed the way the distiller's is.
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Errorf("the scratch directory is gone: %v", err)
	}
}

func TestTheAnglesRunInTheCheckout(t *testing.T) {
	dir := t.TempDir()
	agent := newFakeAngleAgent(len(BuiltinAngles))
	runs, err := (&Angles{Agent: agent, Dir: dir}).Run(context.Background(), t.TempDir(), &Project{}, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if agent.reqs[r.Angle].Dir != dir {
			t.Errorf("%s ran in %s, want the checkout %s", r.Angle, agent.reqs[r.Angle].Dir, dir)
		}
	}
}

func TestRunningTheAnglesWithoutABrief(t *testing.T) {
	agent := newFakeAngleAgent(0)
	if _, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), &Project{}, nil, testDiff); err == nil {
		t.Fatal("angles ran with no brief to read")
	}
	if len(agent.reqs) != 0 {
		t.Errorf("%d sessions ran with nothing to read", len(agent.reqs))
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

func TestAnAngleSessionIsToldWhatItsReviewerDismissedBefore(t *testing.T) {
	agent := newFakeAngleAgent(len(BuiltinAngles))
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
