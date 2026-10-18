package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/state"
)

const jevReviewerTOML = devOnlyTOML + `
[roles.reviewer]
angles.m = ["general", "docs", "test_coverage"]
`

func factoryJevRecord(t *testing.T, h *harness, pr int) review.JevJudgment {
	t.Helper()
	dir, err := review.LatestArtifactDir(h.store.ReviewsDir(), review.Ref{Repo: h.cfg.Project.Repo, Number: pr})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, review.JevFile))
	if err != nil {
		t.Fatal(err)
	}
	var record review.JevJudgment
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// Exercise dispatch, fake brief/angle processes, the adapter, the judge
// session's rendered prompt and its posted review for both factory entry paths.
func TestFactoryJevReachesJudgeSession(t *testing.T) {
	for _, requested := range []bool{false, true} {
		t.Run(map[bool]string{false: "developer", true: "requested"}[requested], func(t *testing.T) {
			var baseline string
			for _, mode := range []string{"baseline", "off", "on", "timeout", "request_cancelled"} {
				t.Run(mode, func(t *testing.T) {
					t.Setenv("OPENROUTER_API_KEY", "")
					t.Setenv("FAKE_REVIEW_SIZE", "m")
					// The pipeline's configured order is general, docs, tests. Jev
					// removes docs and puts tests ahead of general.
					p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "fake-jev", Answers: map[string]systemone.Answer{
						"finding-0": jevtest.Noul(.5), "finding-1": jevtest.Noul(.1), "finding-2": jevtest.Noul(.9),
					}}}}}
					if mode == "timeout" {
						p.Results = []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindTimeout, Message: "fake timeout"}}}
					}
					if mode == "request_cancelled" {
						p.Results = []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindCancelled, Message: "fake request cancellation"}}}
					}
					j := review.NewJevJudge(jev.DefaultSettings())
					j.Provider = p.Factory()
					if mode != "baseline" && mode != "off" {
						t.Setenv("OPENROUTER_API_KEY", "factory-test-key")
					}
					toml := jevReviewerTOML
					if requested {
						toml = strings.Replace(jevReviewerTOML, devOnlyTOML, reviewOnlyTOML, 1)
					}
					h := newHarnessAt(t, toml, time.Now(), func(d *Deps) {
						if mode != "baseline" {
							d.Jev = j
						}
					})
					pr, promptIndex := 201, 1
					if requested {
						pr, promptIndex = 42, 0
						pushBranch(t, h.clone, "fix-widget")
						h.gh.PRs[42] = personsPR("bees", "bees:review-requested")
					} else {
						seedReady(h, 1, "s", time.Now().Add(-time.Hour))
						seedCounter(t, h, "review", 1)
					}
					runPass(t, h)
					completion := "pull request approved"
					if requested {
						completion = "requested review finished"
					}
					if !strings.Contains(h.logs.String(), completion) {
						t.Fatalf("review did not complete: %s", h.logs.String())
					}
					prompt := findingsOf(promptOf(t, h, promptIndex))
					dir, err := review.LatestArtifactDir(h.store.ReviewsDir(), review.Ref{Repo: h.cfg.Project.Repo, Number: pr})
					if err != nil {
						t.Fatal(err)
					}
					a, err := review.ReadArtifact(dir)
					if err != nil {
						t.Fatal(err)
					}
					prompt = strings.ReplaceAll(prompt, dir, "ARTIFACT")
					if mode == "baseline" {
						baseline = prompt
					}
					switch mode {
					case "on":
						titles := []string{}
						for _, f := range a.Findings.Items {
							titles = append(titles, f.Title)
						}
						if !slices.Equal(titles, []string{"test coverage and documentation: Widget does nothing", "general: Widget does nothing"}) {
							t.Fatalf("ranked findings: %v", titles)
						}
						if strings.Contains(prompt, "documentation accuracy: Widget does nothing") || strings.Index(prompt, titles[0]) < 0 || strings.Index(prompt, titles[1]) <= strings.Index(prompt, titles[0]) {
							t.Fatalf("judge prompt: %s", prompt)
						}
						if !strings.Contains(prompt, "Jev probability, highest first") || !strings.Contains(prompt, "jev.json") {
							t.Fatalf("ranked prompt describes wrong order: %s", prompt)
						}
						record := factoryJevRecord(t, h, pr)
						if len(record.Eliminated) != 1 || record.Eliminated[0].Finding.Angle != "docs" {
							t.Fatalf("record: %+v", record)
						}
						verified, ok := h.sched.verifyReview(h.sched.log, state.WorkState{ReviewArtifact: dir}, pr)
						if !ok || !verified.JevRanked || verified.Findings != review.RenderFindings(a.Findings.Items) {
							t.Fatalf("verification lost ranking: %+v", verified)
						}
						if len(p.Requests()) != 1 || !strings.Contains(h.logs.String(), "Jev ranked 3 findings and eliminated 1") {
							t.Fatalf("requests=%d; logs=%s", len(p.Requests()), h.logs.String())
						}
					default:
						if prompt != baseline {
							t.Fatalf("%s changed unboosted prompt:\n%s\nwant:\n%s", mode, prompt, baseline)
						}
						if mode == "off" || mode == "baseline" {
							if len(p.Requests()) != 0 || strings.Contains(h.logs.String(), "review: Jev") {
								t.Fatal("disabled Jev did work")
							}
							if _, err := os.Stat(filepath.Join(dir, review.JevFile)); !os.IsNotExist(err) {
								t.Fatalf("disabled judgment: %v", err)
							}
						} else {
							want := jev.ReasonTimeout
							if mode == "request_cancelled" {
								want = jev.ReasonCancelled
							}
							if r := factoryJevRecord(t, h, pr); r.Reason != want {
								t.Fatalf("fallback: %+v", r)
							}
							if !strings.Contains(h.logs.String(), "Jev fell back: "+string(want)) {
								t.Fatal(h.logs.String())
							}
						}
					}
				})
			}
		})
	}
}

func TestFactoryReviewCancellationDuringJevIsNotCompleted(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "factory-test-key")
	t.Setenv("FAKE_REVIEW_SIZE", "m")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &jevtest.Provider{Wait: func(call context.Context) error { cancel(); <-call.Done(); return call.Err() }}
	j := review.NewJevJudge(jev.DefaultSettings())
	j.Provider = p.Factory()
	h := newHarnessAt(t, jevReviewerTOML, time.Now(), func(d *Deps) { d.Jev = j })
	pr := personsPR()
	h.gh.PRs[pr.Number] = pr
	events := h.sched.Subscribe()
	found, a, err := h.sched.runReview(ctx, h.sched.log, *pr, h.clone, "cancelled-review", 0, 1, "m")
	if !errors.Is(err, context.Canceled) || found != nil || a != nil {
		t.Fatalf("cancelled result = %v, %v, %v", found, a, err)
	}
	if len(p.Requests()) != 1 {
		t.Fatal("Jev not reached")
	}
	dir, err := review.LatestArtifactDir(h.store.ReviewsDir(), review.Ref{Repo: h.cfg.Project.Repo, Number: pr.Number})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := review.ReadArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Findings.Items) != 3 {
		t.Fatalf("cancellation boosted findings: %+v", saved.Findings.Items)
	}
	if _, err := os.Stat(filepath.Join(dir, review.JevFile)); !os.IsNotExist(err) {
		t.Fatalf("cancelled judgment: %v", err)
	}
	ledger, err := h.store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 1 || ledger[0].Outcome != OutcomeFailed {
		t.Fatalf("cancelled ledger: %+v", ledger)
	}
	for {
		select {
		case e := <-events:
			if e.Kind == EventReviewEnded {
				if e.Success {
					t.Fatal("cancelled review completed")
				}
				return
			}
		default:
			t.Fatal("missing review end event")
		}
	}
}
