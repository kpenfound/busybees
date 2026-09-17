package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type agentFunc func(context.Context, AgentRequest) (*AgentResult, error)

func (f agentFunc) Run(ctx context.Context, req AgentRequest) (*AgentResult, error) {
	return f(ctx, req)
}

// The reference deliberately has neither a repository nor a tracker number.
// All stages run with in-memory agents and a caller-owned comparison policy.
func TestSyntheticPipelineUsesInjectedPolicyAndPreservesArtifacts(t *testing.T) {
	bundle := testBundle()
	bundle.Items = append(bundle.Items, Item{Source: "custom", Name: "requirements", Content: "keep ``` fenced content intact"})
	dir := filepath.Join(t.TempDir(), "artifact")
	var mu sync.Mutex
	requests := map[string]AgentRequest{}
	events := map[string][]AngleEvent{}
	var selected []string
	agent := agentFunc(func(_ context.Context, req AgentRequest) (*AgentResult, error) {
		mu.Lock()
		requests[req.Name] = req
		mu.Unlock()
		switch req.Name {
		case DistillerName:
			return &AgentResult{ID: "brief-session", Text: answeredBrief, CostUSD: 0.5, CostKnown: true}, nil
		case AngleTests:
			return nil, errors.New("test angle unavailable")
		case AngleGeneral:
			return &AgentResult{ID: "general-session", Turns: 3, CostUSD: 0.25, CostKnown: true, Text: sessionAnswer(`{"title":"duplicate one","body":"body one","severity":"high","category":"first","sources":["requirement"]}`)}, nil
		case AngleDocs:
			return &AgentResult{ID: "docs-session", Turns: 4, CostUSD: 0.75, CostKnown: true, Text: sessionAnswer(`{"title":"duplicate two","body":"body two","severity":"low","category":"second","suggestion":"fix it","sources":["style"]}`)}, nil
		default:
			return &AgentResult{ID: "criteria-session", Turns: 1, Text: sessionAnswer(`{"title":"unwanted","severity":"medium","category":"noise"}`)}, nil
		}
	})
	var comparisons [][4]string
	compare := func(a, b, c, d string) bool {
		comparisons = append(comparisons, [4]string{a, b, c, d})
		return (a == "duplicate one" && b == "body one" && c == "duplicate two" && d == "body two") || (a == "quiet rule" && c == "unwanted")
	}
	runner := &Runner[testRef]{Distiller: &Distiller[testRef]{Agent: agent}, Angles: &Angles[testRef]{Agent: agent, Provider: "fake", Model: "base"}, Compare: compare,
		Rules:       []Rule{{Scope: testRepo, Angle: "*", Category: "noise", Action: RuleDrop, Text: "quiet rule", Count: 3}},
		AnglesReady: func(angles []string) { selected = angles },
		Progress: func(angle string, ev AngleEvent) {
			mu.Lock()
			defer mu.Unlock()
			events[angle] = append(events[angle], ev)
		},
	}
	runner.Angles.AgentFor = func(angle string) (Agent, string) { return agent, "profile-" + angle }
	artifact, err := runner.Run(context.Background(), dir, bundle, testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Findings.Items) != 1 {
		t.Fatalf("injected comparator/filter left %d findings, want one: %+v", len(artifact.Findings.Items), artifact.Findings)
	}
	f := artifact.Findings.Items[0]
	if f.Title != "duplicate one" || f.Severity != SeverityHigh || f.Suggestion != "fix it" || !reflect.DeepEqual(f.AlsoFrom, []string{AngleDocs}) || !reflect.DeepEqual(f.Sources, []string{"requirement", "style"}) {
		t.Fatalf("merged finding lost content: %+v", f)
	}
	if len(comparisons) == 0 || len(artifact.Findings.Silenced) != 1 || len(artifact.Findings.Skipped) != 1 {
		t.Fatalf("comparison, filter or skipped reporting missing: %v %+v", comparisons, artifact.Findings)
	}
	if artifact.Findings.Silenced[0].Rule != "- [component] [*] [noise] drop: quiet rule (3 dismissals)" {
		t.Fatal(artifact.Findings.Silenced)
	}
	if !reflect.DeepEqual(selected, briefAngles) {
		t.Fatalf("selected %v", selected)
	}
	if artifact.Brief.CostUSD != 0.5 || artifact.Brief.SessionID != "brief-session" || !reflect.DeepEqual(artifact.Brief.NotGathered, bundle.Skipped) || !reflect.DeepEqual(artifact.Brief.Sources, bundle.Sources()) {
		t.Fatalf("brief metadata: %+v", artifact.Brief)
	}
	for _, run := range artifact.Runs {
		if run.Model != "profile-"+run.Angle {
			t.Errorf("model: %+v", run)
		}
		want := AngleFinished
		if run.Angle == AngleTests {
			want = AngleFailed
		}
		if !reflect.DeepEqual(events[run.Angle], []AngleEvent{AngleStarted, want}) {
			t.Errorf("events %v", events)
		}
		if run.Angle == AngleDocs && (run.Turns != 4 || run.CostUSD != 0.75) {
			t.Errorf("accounting: %+v", run)
		}
	}
	if !strings.Contains(requests[DistillerName].Prompt, bundle.Text()) {
		t.Fatal("distiller lost raw context")
	}
	if !strings.Contains(requests[AngleDocs].Prompt, "a source that cannot read something") || !strings.Contains(requests[AngleDocs].Prompt, "gather.go") {
		t.Fatal("angle lost criteria or touched areas")
	}
	if data, err := os.ReadFile(filepath.Join(dir, ScratchDir, DiffFile)); err != nil || string(data) != testDiff {
		t.Fatalf("diff %q: %v", data, err)
	}
	if _, err := os.Stat(requests[DistillerName].Dir); !os.IsNotExist(err) {
		t.Fatalf("distiller scratch left behind: %v", err)
	}
	read, err := ReadArtifact[testRef](dir)
	if err != nil || !reflect.DeepEqual(read, artifact) {
		t.Fatalf("artifact roundtrip: %v\n%+v\n%+v", err, read, artifact)
	}
}

func TestPipelineCancellationLeavesOnlyCompletedStages(t *testing.T) {
	for _, stage := range []string{DistillerName, "angles"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := filepath.Join(t.TempDir(), "artifact")
			if err := os.MkdirAll(filepath.Join(dir, "acquired"), 0o755); err != nil {
				t.Fatal(err)
			}
			agent := agentFunc(func(ctx context.Context, req AgentRequest) (*AgentResult, error) {
				if req.Name == DistillerName && stage != "distiller" {
					return &AgentResult{Text: answeredBrief}, nil
				}
				cancel()
				<-ctx.Done()
				return nil, ctx.Err()
			})
			r := &Runner[testRef]{Distiller: &Distiller[testRef]{Agent: agent}, Angles: &Angles[testRef]{Agent: agent}}
			a, err := r.Run(ctx, dir, testBundle(), testDiff)
			if a != nil || err == nil || !strings.Contains(err.Error(), "context canceled") {
				t.Fatalf("artifact %v, error %v", a, err)
			}
			if stage == DistillerName {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("pre-brief artifact survived: %v", err)
				}
			} else {
				partial, err := ReadArtifact[testRef](dir)
				if err != nil || partial.Findings != nil || len(partial.Runs) != 4 {
					t.Fatalf("partial artifact: %+v, %v", partial, err)
				}
				for _, run := range partial.Runs {
					if !run.Failed() {
						t.Fatal("canceled angle succeeded")
					}
				}
			}
		})
	}
}
