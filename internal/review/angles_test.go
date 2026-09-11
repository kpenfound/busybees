package review

import (
	"bytes"
	"context"
	"errors"
	"maps"
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

func TestOneSessionPerEnabledAngleRunsAtOnce(t *testing.T) {
	project := projectWith(t, "[angles]\ngeneral = false\n")
	agent := newFakeAngleAgent(3)
	angles := &Angles{Agent: agent, Provider: config.AgentClaude, Model: "opus", Dir: t.TempDir()}
	runs, err := angles.Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	// One session per angle the size calls for that the project enables,
	// in BuiltinAngles order, and not one for the angle the project turned
	// off.
	want := []string{AngleDocs, AngleTests, AngleAcceptance}
	if got := ranAngles(runs); !reflect.DeepEqual(got, want) {
		t.Fatalf("angles run = %v, want %v", got, want)
	}
	if len(agent.reqs) != 3 {
		t.Fatalf("%d sessions ran (%v), want 3", len(agent.reqs), agent.order)
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

func TestAProjectWithNoConfigurationRunsEveryAngleOfTheSize(t *testing.T) {
	agent := newFakeAngleAgent(len(briefAngles))
	for _, project := range []*Project{nil, {}} {
		runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
		if err != nil {
			t.Fatal(err)
		}
		if got := ranAngles(runs); !reflect.DeepEqual(got, briefAngles) {
			t.Errorf("angles run for project %+v = %v, want every one of size m's %v", project, got, briefAngles)
		}
	}
}

// Each size is reviewed from its own angles and from no other: the agent is
// asked for exactly those sessions.
func TestEachSizeRunsItsOwnAngles(t *testing.T) {
	for _, tc := range []struct {
		size string
		want []string
	}{
		{"xs", []string{AngleQuickGeneral, AngleDocs}},
		{"s", []string{AngleQuickGeneral, AngleDocs}},
		{"m", []string{AngleGeneral, AngleDocs, AngleTests, AngleAcceptance}},
		{"l", []string{AngleGeneral, AngleDocs, AngleTests, AngleAcceptance}},
		{"xl", []string{AngleGeneral, AngleDocs, AngleTests, AngleAcceptance, AngleSideEffects}},
	} {
		t.Run(tc.size, func(t *testing.T) {
			agent := newFakeAngleAgent(len(tc.want))
			runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), &Project{}, sized(tc.size), testDiff)
			if err != nil {
				t.Fatal(err)
			}
			if got := ranAngles(runs); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("angles run = %v, want %v", got, tc.want)
			}
			if got := slices.Sorted(maps.Keys(agent.reqs)); !reflect.DeepEqual(got, slices.Sorted(slices.Values(tc.want))) {
				t.Errorf("sessions asked for = %v, want %v", got, tc.want)
			}
		})
	}
}

// The size narrows what the project enables and nothing else: an angle the
// project turned off stays off whatever the size calls for, and one it
// turned on explicitly runs only when the size calls for it too.
func TestASizeNeverRunsAnAngleTheProjectTurnedOff(t *testing.T) {
	project := projectWith(t, "[angles]\ndocs = false\nside_effects = false\nquick_general = true\n")
	agent := newFakeAngleAgent(3)
	runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, sized("xl"), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ranAngles(runs), []string{AngleGeneral, AngleTests, AngleAcceptance}; !reflect.DeepEqual(got, want) {
		t.Errorf("angles run = %v, want %v", got, want)
	}
	for _, angle := range []string{AngleDocs, AngleSideEffects, AngleQuickGeneral} {
		if _, ran := agent.reqs[angle]; ran {
			t.Errorf("the %s angle ran", angle)
		}
	}
}

// A small change from a project that turned the docs angle off gets the
// quick general pass alone, which is a review and not an error.
func TestASmallChangeWithDocsOffRunsTheQuickPassAlone(t *testing.T) {
	project := projectWith(t, "[angles]\ndocs = false\n")
	for _, size := range []string{"xs", "s"} {
		t.Run(size, func(t *testing.T) {
			agent := newFakeAngleAgent(1)
			runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, sized(size), testDiff)
			if err != nil {
				t.Fatalf("one angle left is an error: %v", err)
			}
			if got := ranAngles(runs); !reflect.DeepEqual(got, []string{AngleQuickGeneral}) || len(agent.reqs) != 1 {
				t.Errorf("angles run = %v, sessions %v, want the quick general angle alone", got, agent.order)
			}
		})
	}
}

// A brief that never went through Brief.Validate can have no size of Sizes:
// it gets the largest size's angles, and the log says so.
func TestABriefWithoutASizeGetsTheLargestSizesAngles(t *testing.T) {
	for _, size := range []string{"", "huge"} {
		t.Run(size, func(t *testing.T) {
			agent := newFakeAngleAgent(5)
			var log bytes.Buffer
			runs, err := (&Angles{Agent: agent, Dir: t.TempDir(), Log: &log}).Run(context.Background(), t.TempDir(), &Project{}, sized(size), testDiff)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := ranAngles(runs), []string{AngleGeneral, AngleDocs, AngleTests, AngleAcceptance, AngleSideEffects}; !reflect.DeepEqual(got, want) {
				t.Errorf("angles run = %v, want xl's %v", got, want)
			}
			if want := `the brief sizes the change "` + size + `", which is not one of xs, s, m, l, xl: the angles of xl run`; !strings.Contains(log.String(), want) {
				t.Errorf("log = %q, want %q", log.String(), want)
			}
		})
	}
	// A size that is one of Sizes is not reported.
	var log bytes.Buffer
	if _, err := (&Angles{Agent: newFakeAngleAgent(len(briefAngles)), Dir: t.TempDir(), Log: &log}).Run(context.Background(), t.TempDir(), &Project{}, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Errorf("log = %q, want nothing said about a size that is one", log.String())
	}
}

// Every size has its angles, every one of them an angle, and nothing else
// has any: a size added to Sizes without angles would get the largest's.
func TestEverySizeHasItsAngles(t *testing.T) {
	if got, want := slices.Sorted(maps.Keys(sizeAngles)), slices.Sorted(slices.Values(Sizes)); !reflect.DeepEqual(got, want) {
		t.Errorf("angles for sizes %v, want for each of %v", got, want)
	}
	for size, angles := range sizeAngles {
		for _, angle := range angles {
			if !slices.Contains(BuiltinAngles, angle) {
				t.Errorf("size %s names %q, which is not an angle", size, angle)
			}
		}
	}
}

func TestAnAngleSessionIsToldItsAngleTheBriefAndTheDiff(t *testing.T) {
	// No one size runs every angle: xs and xl between them do. A checkout
	// (fakeDocker) is what makes diff.patch's directory one the angles'
	// read-only tools can reach: without one the angles fall back to the
	// machine's own checkout, which diff.patch is not written into
	// (TestTheDiffIsNotWrittenIntoTheMachinesCheckout).
	reqs := map[string]AgentRequest{}
	diffPaths := map[string]string{}
	for _, size := range []string{"xs", "xl"} {
		docker := fakeDocker(t)
		agent := newFakeAngleAgent(len(sizeAngles[size]))
		artifact := t.TempDir()
		angles := &Angles{Agent: agent, Dir: t.TempDir(), Checkout: &Checkout{DockerBin: docker}}
		if _, err := angles.Run(context.Background(), artifact, &Project{}, sized(size), testDiff); err != nil {
			t.Fatal(err)
		}
		maps.Copy(reqs, agent.reqs)
		for _, angle := range sizeAngles[size] {
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
			"The diff is at " + diffPath + ": read it there.",
			"Review acme/widgets#7 from the " + angleTitles[angle] + " angle. Answer with the JSON object alone.\n",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("the %s session was not told %q:\n%s", angle, want, prompt)
			}
		}
		if strings.Contains(prompt, testDiff) {
			t.Errorf("the %s session's prompt still has the diff inline:\n%s", angle, prompt)
		}
		// The frame, the angle, the brief, the diff, the closing line: in
		// that order, so the instructions come before what can be long.
		last := -1
		for _, mark := range []string{"You are one session", heading, "# Review brief", "The diff is at", "Answer with the JSON object alone"} {
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

// Angles.Dir (the machine's own checkout of the repository under review,
// used when Checkout is nil or fails) is a working tree the review did not
// make, so a gathered diff is not written into it: an angle's read-only
// tools can reach nothing outside the directory it runs in, and the review
// must not leave a stray file behind in a tree it does not own either.
func TestTheDiffIsNotWrittenIntoTheMachinesCheckout(t *testing.T) {
	agent := newFakeAngleAgent(1)
	project := onlyAngle(t, AngleGeneral)
	host := t.TempDir()
	if _, err := (&Angles{Agent: agent, Dir: host}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	prompt := agent.reqs[AngleGeneral].Prompt
	if !strings.Contains(prompt, "The diff was not gathered") || strings.Contains(prompt, "The diff is at") {
		t.Errorf("a session run in the machine's checkout was not given the no-diff fallback:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(host, DiffFile)); !os.IsNotExist(err) {
		t.Errorf("diff.patch written into the machine's own checkout (stat err: %v)", err)
	}
}

func TestWithoutADiffTheSessionIsToldSo(t *testing.T) {
	agent := newFakeAngleAgent(1)
	project := onlyAngle(t, AngleGeneral)
	artifact := t.TempDir()
	if _, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), artifact, project, testBrief(), " \n"); err != nil {
		t.Fatal(err)
	}
	prompt := agent.reqs[AngleGeneral].Prompt
	if !strings.Contains(prompt, "The diff was not gathered") || strings.Contains(prompt, "The diff is at") {
		t.Errorf("a session with no diff was not told so:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(artifact, DiffFile)); !os.IsNotExist(err) {
		t.Errorf("diff.patch written for a review with no diff (stat err: %v)", err)
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
	agent := newFakeAngleAgent(len(briefAngles))
	agent.fail = map[string]error{AngleGeneral: errors.New("general session: no capacity")}
	artifact := t.TempDir()
	runs, err := (&Angles{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
	if err == nil || !strings.Contains(err.Error(), "general session: no capacity") {
		t.Fatalf("err = %v, want the failed angle's error", err)
	}
	if len(runs) != len(briefAngles) {
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
	agent := newFakeAngleAgent(len(briefAngles))
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
	agent := newFakeAngleAgent(len(briefAngles))
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
	agent := newFakeAngleAgent(len(briefAngles))
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

// Progress is told once as each angle's session starts and once as it
// ends, with how it ended, and about no other angle. The angles run at
// once, so nothing is asserted about the order across angles.
func TestProgressIsToldAsEachAngleStartsAndEnds(t *testing.T) {
	agent := newFakeAngleAgent(len(briefAngles))
	agent.fail = map[string]error{AngleDocs: errors.New("docs session: exit status 1")}
	progress := newProgressRecorder()
	angles := &Angles{Agent: agent, Dir: t.TempDir(), Progress: progress.record}
	runs, err := angles.Run(context.Background(), t.TempDir(), &Project{}, testBrief(), testDiff)
	if err == nil {
		t.Fatal("the failed angle was not reported")
	}
	if got := ranAngles(runs); !reflect.DeepEqual(got, briefAngles) {
		t.Fatalf("angles run = %v, want %v", got, briefAngles)
	}
	if len(progress.events) != len(briefAngles) {
		t.Errorf("progress was told about %v, want %v", slices.Sorted(maps.Keys(progress.events)), briefAngles)
	}
	for _, angle := range briefAngles {
		want := []AngleEvent{AngleStarted, AngleFinished}
		if angle == AngleDocs {
			want = []AngleEvent{AngleStarted, AngleFailed}
		}
		if got := progress.events[angle]; !reflect.DeepEqual(got, want) {
			t.Errorf("the %s angle's progress = %v, want %v", angle, got, want)
		}
	}
}

// A run that starts no session tells Progress nothing: one refused before
// any session ran, and one whose project turned every angle off.
func TestProgressIsToldNothingWhenNoSessionRuns(t *testing.T) {
	progress := newProgressRecorder()
	agent := newFakeAngleAgent(0)
	if _, err := (&Angles{Agent: agent, Dir: t.TempDir(), Progress: progress.record}).Run(context.Background(), t.TempDir(), &Project{}, nil, testDiff); err == nil {
		t.Fatal("a run without a brief is an error")
	}
	off := projectWith(t, "[angles]\nquick_general = false\ngeneral = false\ndocs = false\ntest_coverage = false\nacceptance_criteria = false\nside_effects = false\n")
	if _, err := (&Angles{Agent: agent, Dir: t.TempDir(), Progress: progress.record}).Run(context.Background(), t.TempDir(), off, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	if len(progress.events) != 0 {
		t.Errorf("progress was told %v, want nothing", progress.events)
	}
}
