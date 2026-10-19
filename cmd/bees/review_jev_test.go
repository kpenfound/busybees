package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/fakegh"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/versions"
)

// Both the brief and angle protocols ignore the other fields. Every fake
// angle reports three problems at distinct anchors; deterministic merging
// keeps one copy of each before the boost runs.
const jevPipelineAnswer = `{"summary":"Updates widgets","size":"s","findings":[
 {"category":"correctness","severity":"high","file":"one.go","lines":[1,1],"side":"new","title":"First problem","body":"First specific problem"},
 {"category":"security","severity":"medium","file":"two.go","lines":[2,2],"side":"new","title":"Eliminated problem","body":"Second specific problem"},
 {"category":"performance","severity":"low","file":"three.go","lines":[3,3],"side":"new","title":"Ranked first problem","body":"Third specific problem"}
]}`

func jevPipelineAgent(t *testing.T) {
	t.Helper()
	originalPath := os.Getenv("PATH")
	fakeClaude(t, jevPipelineAnswer)
	bin := os.Getenv("PATH")
	// Checkout acquisition is deliberately unavailable; never use host docker.
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEES_CLAUDE_BIN", filepath.Join(bin, "claude"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+originalPath)
}

func jevPR() *github.PR {
	return &github.PR{Number: 7, Title: "Update widgets", State: "OPEN", HeadRefName: "main", BaseRefName: "main", URL: "https://github.com/acme/widgets/pull/7", Author: github.Author{Login: "human"}, Labels: []github.Label{{Name: "bees"}, {Name: "bees:review-requested"}}, CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now()}
}

func readCLIJudgment(t *testing.T, dir string) review.JevJudgment {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, review.JevFile))
	if err != nil {
		t.Fatal(err)
	}
	var r review.JevJudgment
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReviewCommandJevRanksBeforeTriage(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
			home := reviewHome(t)
			t.Setenv("OPENROUTER_API_KEY", "")
			if enabled {
				t.Setenv("OPENROUTER_API_KEY", "cli-test-key")
			}
			jevPipelineAgent(t)
			p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "fake-jev", Answers: map[string]systemone.Answer{"finding-0": jevtest.Noul(.5), "finding-1": jevtest.Noul(.1), "finding-2": jevtest.Noul(.9)}}}}}
			gh := fakegh.New("acme/widgets")
			gh.PRs[7] = jevPR()
			original := newReviewRunner
			t.Cleanup(func() { newReviewRunner = original })
			newReviewRunner = func(ctx context.Context, ref review.Ref, cfg *review.Config, dir string) (*review.Runner, error) {
				r, err := original(ctx, ref, cfg, dir)
				if err != nil {
					return nil, err
				}
				if r.Jev == nil {
					t.Fatal("NewRunner did not construct the Judge")
				}
				r.Jev.Provider = p.Factory()
				r.Pipeline.Client.Exec = gh.Exec
				r.Pipeline.Client.ExecStdin = gh.ExecStdin
				return r, nil
			}
			out, _, err := runReviewWith(t, "s\ns\ns\n", "acme/widgets#7", "--no-tui", "--report")
			if err != nil {
				t.Fatal(err)
			}
			dir, err := review.LatestArtifactDir(filepath.Join(home, review.DefaultStoragePath), review.Ref{Repo: "acme/widgets", Number: 7})
			if err != nil {
				t.Fatal(err)
			}
			triage, err := review.ReadTriage(dir)
			if err != nil {
				t.Fatal(err)
			}
			if enabled {
				if len(p.Requests()) != 1 || len(triage.Decisions) != 2 {
					t.Fatalf("requests=%d, triage=%+v", len(p.Requests()), triage)
				}
				if strings.Contains(out, "Eliminated problem") || strings.Index(out, "Ranked first problem") < 0 || strings.Index(out, "First problem") <= strings.Index(out, "Ranked first problem") {
					t.Fatal(out)
				}
				if !strings.Contains(out, "Jev ranked 3 findings and eliminated 1") {
					t.Fatal(out)
				}
				r := readCLIJudgment(t, dir)
				if len(r.Eliminated) != 1 || r.Eliminated[0].Finding.Title != "Eliminated problem" {
					t.Fatalf("record: %+v", r)
				}
			} else {
				if len(p.Requests()) != 0 || len(triage.Decisions) != 3 || strings.Contains(out, "Jev ranked") || strings.Contains(out, "Jev fell back") {
					t.Fatal(out)
				}
				if _, err := os.Stat(filepath.Join(dir, review.JevFile)); !os.IsNotExist(err) {
					t.Fatalf("disabled record: %v", err)
				}
				if strings.Index(out, "First problem") < 0 || strings.Index(out, "Eliminated problem") <= strings.Index(out, "First problem") || strings.Index(out, "Ranked first problem") <= strings.Index(out, "Eliminated problem") {
					t.Fatal(out)
				}
			}
		})
	}
}

// Build actual machine loops, run requested reviews and then use the same
// runtime's reload builder for an added project and a restarted project.
// Every subsequent review must see the first project's rate-limit cooldown.
func TestMachineReviewsShareJevCooldownAcrossReload(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	t.Setenv("OPENROUTER_API_KEY", "machine-test-key")
	jevPipelineAgent(t)
	p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindRateLimited, Message: "fake rate limit"}}}}
	judge := review.NewJevJudge(jev.DefaultSettings())
	judge.Provider = p.Factory()
	// A fixed clock prevents a slow machine from expiring the cooldown.
	judge.Now = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }
	original := factoryReviewJudge
	t.Cleanup(func() { factoryReviewJudge = original })
	factoryReviewJudge = func() *jev.Judge { return judge }
	paths := []string{writeProject(t, "acme/a", ""), writeProject(t, "acme/b", "")}
	m := loadMachine(t, paths...)
	g := &globalFlags{logger: logging.New(logging.Options{Console: &bytes.Buffer{}})}
	t.Cleanup(func() { _ = g.logger.Close() })
	rt := newMachineRuntime(g, m)
	run := func(projectPath string, projects []daemon.Project, want jev.Reason) {
		t.Helper()
		for _, project := range projects {
			if project.Name == projectPath {
				l, err := project.Start(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				loop := l.(*projectLoop)
				defer func() { _ = loop.Close() }()
				gh := fakegh.New(loop.app.cfg.Project.Repo)
				gh.PRs[7] = jevPR()
				loop.app.gh.Exec = gh.Exec
				loop.app.gh.ExecStdin = gh.ExecStdin
				loop.Once = true
				loop.OnlyRoles = map[string]bool{config.RoleReviewer: true}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := loop.Run(ctx); err != nil {
					t.Fatal(err)
				}
				dir, err := review.LatestArtifactDir(loop.app.store.ReviewsDir(), review.Ref{Repo: loop.app.cfg.Project.Repo, Number: 7})
				if err != nil {
					t.Fatal(err)
				}
				record := readCLIJudgment(t, dir)
				if record.Reason != want {
					t.Fatalf("%s: %+v", projectPath, record)
				}
				a, err := review.ReadArtifact(dir)
				if err != nil {
					t.Fatal(err)
				}
				titles := []string{}
				for _, f := range a.Findings.Items {
					titles = append(titles, f.Title)
				}
				if !slices.Equal(titles, []string{"First problem", "Eliminated problem", "Ranked first problem"}) {
					t.Fatalf("fallback findings: %v", titles)
				}
				return
			}
		}
		t.Fatal("project missing")
	}
	initial := rt.projects(m)
	run(paths[0], initial, jev.ReasonRateLimited)
	run(paths[1], initial, jev.ReasonCoolingDown)
	paths = append(paths, writeProject(t, "acme/c", ""))
	next := loadMachine(t, paths...)
	// Reload through the same handler used by SIGHUP and the live view.
	body, err := os.ReadFile(next.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	changes := make(chan []daemon.Project, 1)
	reloader := &machineReloader{path: m.Path, build: rt.projects, apply: rt.reloadProjects, changes: changes, log: g.logger.Logger}
	if err := reloader.reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	reloaded := <-changes
	run(paths[2], reloaded, jev.ReasonCoolingDown)
	run(paths[0], reloaded, jev.ReasonCoolingDown)
	if len(p.Requests()) != 1 {
		t.Fatalf("shared cooldown sent %d requests", len(p.Requests()))
	}
}

func TestFactoryReviewJudgeUsesProcessDefaults(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	j := factoryReviewJudge()
	if j != factoryReviewJudge() {
		t.Fatal("Judge was constructed more than once")
	}
	if got, want := j.Config(), jev.DefaultSettings(); got != want {
		t.Fatalf("factory settings=%+v, want %+v", got, want)
	}
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	if !j.Config().Enabled {
		t.Fatal("process key did not enable Judge")
	}
}
