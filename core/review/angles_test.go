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

const testDiff = "diff --git a/gather.go b/gather.go\n+func Gather() {}\n"

// briefAngles are the angles testBrief's size, m, is reviewed from by a
// project that turns none of them off.
var briefAngles = []string{AngleGeneral, AngleDocs, AngleTests, AngleAcceptance}

// sized is testBrief with another size.
func sized(size string) *Brief[testRef] {
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
func TestOneSessionPerEnabledAngleRunsAtOnce(t *testing.T) {
	project := &Settings{Angles: map[string]bool{"general": false}}
	agent := newFakeAngleAgent(3)
	angles := &Angles[testRef]{Agent: agent, Provider: "fake", Model: "opus", Dir: t.TempDir()}
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
		if r.Provider != "fake" || r.Model != "opus" || r.Dir != angles.Dir {
			t.Errorf("%s run = %+v, want the agent and the directory it ran as recorded", r.Angle, r)
		}
		if r.Failed() {
			t.Errorf("%s run failed: %s", r.Angle, r.Error)
		}
	}
}

func TestEveryAngleOffRunsNothing(t *testing.T) {
	project := &Settings{Angles: map[string]bool{"quick_general": false, "general": false, "docs": false, "test_coverage": false, "acceptance_criteria": false, "side_effects": false}}
	agent := newFakeAngleAgent(0)
	runs, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
	if err != nil || runs != nil {
		t.Fatalf("runs, err = %v, %v, want none and no error", runs, err)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("%d sessions ran with every angle off", len(agent.reqs))
	}
}

func TestASettingsWithNoConfigurationRunsEveryAngleOfTheSize(t *testing.T) {
	agent := newFakeAngleAgent(len(briefAngles))
	for _, project := range []*Settings{nil, {}} {
		runs, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff)
		if err != nil {
			t.Fatal(err)
		}
		if got := ranAngles(runs); !reflect.DeepEqual(got, briefAngles) {
			t.Errorf("angles run for project %+v = %v, want every one of size m's %v", project, got, briefAngles)
		}
	}
}

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
			runs, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), &Settings{}, sized(tc.size), testDiff)
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

func TestASizeNeverRunsAnAngleTheSettingsTurnedOff(t *testing.T) {
	project := &Settings{Angles: map[string]bool{"docs": false, "side_effects": false, "quick_general": true}}
	agent := newFakeAngleAgent(3)
	runs, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, sized("xl"), testDiff)
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

func TestASmallChangeWithDocsOffRunsTheQuickPassAlone(t *testing.T) {
	project := &Settings{Angles: map[string]bool{"docs": false}}
	for _, size := range []string{"xs", "s"} {
		t.Run(size, func(t *testing.T) {
			agent := newFakeAngleAgent(1)
			runs, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), project, sized(size), testDiff)
			if err != nil {
				t.Fatalf("one angle left is an error: %v", err)
			}
			if got := ranAngles(runs); !reflect.DeepEqual(got, []string{AngleQuickGeneral}) || len(agent.reqs) != 1 {
				t.Errorf("angles run = %v, sessions %v, want the quick general angle alone", got, agent.order)
			}
		})
	}
}

func TestABriefWithoutASizeGetsTheLargestSizesAngles(t *testing.T) {
	for _, size := range []string{"", "huge"} {
		t.Run(size, func(t *testing.T) {
			agent := newFakeAngleAgent(5)
			var log bytes.Buffer
			runs, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir(), Log: &log}).Run(context.Background(), t.TempDir(), &Settings{}, sized(size), testDiff)
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
	if _, err := (&Angles[testRef]{Agent: newFakeAngleAgent(len(briefAngles)), Dir: t.TempDir(), Log: &log}).Run(context.Background(), t.TempDir(), &Settings{}, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Errorf("log = %q, want nothing said about a size that is one", log.String())
	}
}

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

func TestTheDiffIsNotWrittenIntoTheMachinesCheckout(t *testing.T) {
	agent := newFakeAngleAgent(1)
	project := onlyAngle(t, AngleGeneral)
	host := t.TempDir()
	if _, err := (&Angles[testRef]{Agent: agent, Dir: host}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff); err != nil {
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
	if _, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), artifact, project, testBrief(), " \n"); err != nil {
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
	runs, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), artifact, &Settings{}, testBrief(), testDiff)
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
	want, err := (&Angles[testRef]{Agent: agent, Provider: "fake", Dir: t.TempDir()}).Run(context.Background(), artifact, &Settings{}, testBrief(), testDiff)
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
	if err := WriteAngleRun(artifact, &AngleRun{Angle: AngleTests, Provider: "fake", SessionID: "s"}); err != nil {
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
	runs, err := (&Angles[testRef]{Agent: agent}).Run(context.Background(), artifact, &Settings{}, testBrief(), testDiff)
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
	runs, err := (&Angles[testRef]{Agent: agent, Dir: dir}).Run(context.Background(), t.TempDir(), &Settings{}, testBrief(), testDiff)
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
	if _, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), t.TempDir(), &Settings{}, nil, testDiff); err == nil {
		t.Fatal("angles ran with no brief to read")
	}
	if len(agent.reqs) != 0 {
		t.Errorf("%d sessions ran with nothing to read", len(agent.reqs))
	}
}

func TestProgressIsToldAsEachAngleStartsAndEnds(t *testing.T) {
	agent := newFakeAngleAgent(len(briefAngles))
	agent.fail = map[string]error{AngleDocs: errors.New("docs session: exit status 1")}
	progress := newProgressRecorder()
	angles := &Angles[testRef]{Agent: agent, Dir: t.TempDir(), Progress: progress.record}
	runs, err := angles.Run(context.Background(), t.TempDir(), &Settings{}, testBrief(), testDiff)
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

func TestProgressIsToldNothingWhenNoSessionRuns(t *testing.T) {
	progress := newProgressRecorder()
	agent := newFakeAngleAgent(0)
	if _, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir(), Progress: progress.record}).Run(context.Background(), t.TempDir(), &Settings{}, nil, testDiff); err == nil {
		t.Fatal("a run without a brief is an error")
	}
	off := &Settings{Angles: map[string]bool{"quick_general": false, "general": false, "docs": false, "test_coverage": false, "acceptance_criteria": false, "side_effects": false}}
	if _, err := (&Angles[testRef]{Agent: agent, Dir: t.TempDir(), Progress: progress.record}).Run(context.Background(), t.TempDir(), off, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	if len(progress.events) != 0 {
		t.Errorf("progress was told %v, want nothing", progress.events)
	}
}
