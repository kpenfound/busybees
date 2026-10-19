package review

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
)

const jevTestKey = "secret-jev-credential-never-record"

func fakeJev(p *jevtest.Provider) *jev.Judge {
	return &jev.Judge{
		Config: func() jev.Settings { s := jev.DefaultSettings(); s.Enabled = true; return s },
		Getenv: func(string) string { return jevTestKey }, Provider: p.Factory(),
	}
}
func jevAnswer(ps ...float64) jevtest.Result {
	answers := make(map[string]systemone.Answer, len(ps))
	for i, p := range ps {
		answers[findingKey(i)] = jevtest.Noul(p)
	}
	return jevtest.Result{Response: systemone.Response{Model: "answered-model", Answers: answers, Usage: systemone.Usage{InputTokens: 120, OutputTokens: 5, CostKnown: true, CostUSD: 0.001}}}
}
func runJevReview(t *testing.T, j *jev.Judge, agent *reviewAgent) (*Artifact, string) {
	t.Helper()
	r, log := testRunner(t, agent, &Notes{Rules: []Rule{shortNames(RuleDownrank)}})
	r.Jev = j
	a, err := r.Run(context.Background(), Ref{Repo: testRepo, Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	return a, log.String()
}
func readJev(t *testing.T, a *Artifact) JevJudgment {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(a.Dir, JevFile))
	if err != nil {
		t.Fatal(err)
	}
	var record JevJudgment
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}
func artifactFiles(t *testing.T, a *Artifact) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(a.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(a.Dir, path)
		if err != nil {
			return err
		}
		// Session directories differ between otherwise identical fake runs.
		if strings.HasPrefix(filepath.ToSlash(name), "angles/") {
			var run map[string]any
			if err := json.Unmarshal(data, &run); err != nil {
				return err
			}
			run["dir"] = "ANGLE_WORKING_DIRECTORY"
			data, err = json.Marshal(run)
			if err != nil {
				return err
			}
		}
		files[name] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestJevOffPreservesArtifactFilesAndOrder(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	for _, unset := range []bool{true, false} {
		t.Run(fmt.Sprint("unset-", unset), func(t *testing.T) {
			t.Setenv(jev.DefaultAPIKeyEnv, "")
			if unset {
				if err := os.Unsetenv(jev.DefaultAPIKeyEnv); err != nil {
					t.Fatal(err)
				}
			}
			p := &jevtest.Provider{Results: []jevtest.Result{jevAnswer(0.1, 0.9, 0.8)}}
			j := NewJevJudge(jev.DefaultSettings())
			j.Provider = p.Factory()
			a, log := runJevReview(t, j, &reviewAgent{})
			if len(p.Requests()) != 0 || strings.Contains(log, "\nJev ") {
				t.Fatalf("off boost ran: %s", log)
			}
			if _, err := os.Stat(filepath.Join(a.Dir, JevFile)); !os.IsNotExist(err) {
				t.Fatalf("off judgment: %v", err)
			}
			if !reflect.DeepEqual(a.Findings, baseline.Findings) {
				t.Fatalf("off findings changed: %+v", a.Findings)
			}
			got, want := artifactFiles(t, a), artifactFiles(t, baseline)
			// The only normalized field is the fake angle's working directory;
			// every other artifact field and the full file inventory must match.
			if len(got) != len(want) {
				t.Fatalf("artifact files %v, want %v", got, want)
			}
			for name, w := range want {
				g, ok := got[name]
				if !ok {
					t.Errorf("missing %s", name)
				}
				if g != w {
					t.Errorf("%s differs", name)
				}
			}
		})
	}
}

func TestJevRanksEliminatesAndPreservesFullFindings(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	for _, tc := range []struct {
		name       string
		ps         []float64
		order      []int
		eliminated []int
	}{
		{"rank", []float64{0.7, 0.1, 0.9}, []int{2, 0}, []int{1}},
		{"ties and threshold", []float64{JevThreshold, 0.8, 0.8}, []int{1, 2, 0}, nil},
		{"eliminate all", []float64{0.1, 0.1, 0.1}, []int{}, []int{0, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &jevtest.Provider{Results: []jevtest.Result{jevAnswer(tc.ps...)}}
			a, log := runJevReview(t, fakeJev(p), &reviewAgent{})
			expected := make([]Finding, 0, len(tc.order))
			for _, i := range tc.order {
				expected = append(expected, baseline.Findings.Items[i])
			}
			if !reflect.DeepEqual(a.Findings.Items, expected) {
				t.Fatalf("findings changed beyond selection and ordering:\ngot %+v\nwant %+v", a.Findings.Items, expected)
			}
			persisted, err := ReadFindings(a.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted, a.Findings) {
				t.Fatal("findings.json differs from returned findings")
			}
			q, err := NewQueue(a, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(q.Pending(), expected) && len(expected) > 0 {
				t.Fatal("queue differs")
			}
			rec := readJev(t, a)
			if rec.Outcome != jev.Accepted || rec.Version != JevVersion || rec.Threshold != JevThreshold || rec.AnsweringModel != "answered-model" || rec.ConfiguredModel != jev.DefaultModel || rec.Usage == nil || rec.Usage.InputTokens != 120 {
				t.Fatalf("record %+v", rec)
			}
			for i, p := range tc.ps {
				if rec.Probabilities[findingKey(i)] != p {
					t.Error("probability missing")
				}
			}
			if len(rec.Eliminated) != len(tc.eliminated) {
				t.Fatal("wrong eliminated count")
			}
			for n, i := range tc.eliminated {
				if !reflect.DeepEqual(rec.Eliminated[n].Finding, baseline.Findings.Items[i]) || rec.Eliminated[n].Probability != tc.ps[i] {
					t.Fatal("eliminated finding lost")
				}
			}
			if strings.Count(log, "\nJev ") != 1 || !strings.Contains(log, fmt.Sprintf("Jev ranked 3 findings and eliminated %d", len(tc.eliminated))) {
				t.Fatalf("log %s", log)
			}
			assertNoJevKey(t, a, log)
		})
	}
}

func TestJevRequestContainsOnlyAllowedContext(t *testing.T) {
	p := &jevtest.Provider{Results: []jevtest.Result{jevAnswer(0.7, 0.7, 0.7)}}
	r, _ := testRunner(t, &reviewAgent{}, &Notes{Rules: []Rule{shortNames(RuleDownrank)}})
	gh := sampleGH()
	gh.diff = "diff --git a/widget.go b/widget.go\n--- a/widget.go\n+++ b/widget.go\n@@ -1,2 +1,3 @@\n-DIFF_ONLY_OLD\n+DIFF_ONLY_NEW\n+DIFF_ONLY_SECOND\n context\n"
	r.Pipeline.Client = gh.client(t)
	writeFile(t, r.Pipeline.Dir, "widget.go", "CHECKOUT_CONTENT_ONLY\n")
	r.Jev = fakeJev(p)
	a, err := r.Run(context.Background(), Ref{Repo: testRepo, Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	reqs := p.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests %d", len(reqs))
	}
	req := reqs[0]
	state := req.State.(string)
	for _, s := range []string{a.Brief.Summary, a.Brief.AcceptanceCriteria[0].Text, a.Brief.StyleRules[0].Text, a.Brief.TouchedAreas[0].Summary, "receiver names are short here", `"path":"widget.go","added":2,"removed":1`} {
		if !strings.Contains(state, s) {
			t.Errorf("state lacks %q: %s", s, state)
		}
	}
	for i, f := range a.Findings.Items {
		if !strings.Contains(state, f.Title) || !strings.Contains(state, f.Body) {
			t.Errorf("missing finding %d", i)
		}
		q, ok := req.Questions[findingKey(i)]
		if !ok || q.Kind != systemone.KindNoul {
			t.Errorf("question %+v", q)
		}
	}
	for _, s := range []string{gh.diff, "DIFF_ONLY_OLD", "DIFF_ONLY_NEW", "CHECKOUT_CONTENT_ONLY", "diff.patch", r.Pipeline.Dir, "not_gathered", "sess-distiller", jevTestKey} {
		if strings.Contains(state, s) {
			t.Errorf("state includes forbidden context %q", s)
		}
	}
	if len(req.Questions) != len(a.Findings.Items) || req.Validate() != nil {
		t.Fatal("invalid question set")
	}
}

func TestJevFallbacksPreserveFindings(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	for _, tc := range []struct {
		name     string
		kind     systemone.ErrorKind
		reason   jev.Reason
		requests int
	}{
		{"timeout", systemone.KindTimeout, jev.ReasonTimeout, 2},
		{"rate limit", systemone.KindRateLimited, jev.ReasonRateLimited, 1},
		{"unavailable", systemone.KindUnavailable, jev.ReasonUnavailable, 2},
		{"rejected", systemone.KindRejected, jev.ReasonRejected, 1},
		{"unusable", systemone.KindMalformed, jev.ReasonUnusable, 1},
		{"request cancelled", systemone.KindCancelled, jev.ReasonCancelled, 1},
		{"invalid settings", "", jev.ReasonUnconfigured, 0},
		{"cooling down", systemone.KindRateLimited, jev.ReasonCoolingDown, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: tc.kind, Message: "provider detail " + jevTestKey}}}}
			j := fakeJev(p)
			if tc.name == "invalid settings" {
				j.Config = func() jev.Settings { s := jev.DefaultSettings(); s.Enabled = true; s.Timeout = 0; return s }
			}
			if tc.name == "cooling down" {
				j.Evaluate(context.Background(), systemone.Request{State: "warmup", Questions: map[string]systemone.Question{"q": systemone.Noul("question")}})
			}
			a, log := runJevReview(t, j, &reviewAgent{})
			if !reflect.DeepEqual(a.Findings, baseline.Findings) {
				t.Fatalf("fallback changed findings: %+v", a.Findings)
			}
			before, err := os.ReadFile(filepath.Join(baseline.Dir, FindingsFile))
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(a.Dir, FindingsFile))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("fallback rewrote findings.json")
			}

			record := readJev(t, a)
			if record.Outcome != jev.Fallback || record.Reason != tc.reason || len(p.Requests()) != tc.requests {
				t.Fatalf("record %+v, requests %d", record, len(p.Requests()))
			}
			if strings.Count(log, "\nJev ") != 1 || !strings.Contains(log, "Jev fell back: "+string(tc.reason)) {
				t.Fatalf("log %s", log)
			}
			assertNoJevKey(t, a, log)
		})
	}
}

func TestJevUnusableAcceptedResponseFallsBack(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	for _, name := range []string{"missing", "wrong kind", "nan", "infinite", "negative", "above one", "extra", "no model", "invalid usage"} {
		t.Run(name, func(t *testing.T) {
			result := jevAnswer(0.9, 0.1, 0.8)
			switch name {
			case "missing":
				delete(result.Response.Answers, findingKey(0))
			case "wrong kind":
				result.Response.Answers[findingKey(0)] = systemone.Answer{Kind: systemone.KindChoice}
			case "nan":
				result.Response.Answers[findingKey(0)] = jevtest.Noul(math.NaN())
			case "infinite":
				result.Response.Answers[findingKey(0)] = jevtest.Noul(math.Inf(1))
			case "negative":
				result.Response.Answers[findingKey(0)] = jevtest.Noul(-0.1)
			case "above one":
				result.Response.Answers[findingKey(0)] = jevtest.Noul(1.1)
			case "extra":
				result.Response.Answers["invented"] = jevtest.Noul(0.9)
			case "no model":
				result.Response.Model = ""
			case "invalid usage":
				result.Response.Usage.CostUSD = math.NaN()
			}
			a, _ := runJevReview(t, fakeJev(&jevtest.Provider{Results: []jevtest.Result{result}}), &reviewAgent{})
			if !reflect.DeepEqual(a.Findings, baseline.Findings) || readJev(t, a).Reason != jev.ReasonUnusable {
				t.Fatal("unusable response applied")
			}
		})
	}
}

func TestJevOversizedFindingsFallBackWithoutRequest(t *testing.T) {
	agent := func() *reviewAgent {
		return &reviewAgent{answers: map[string]string{AngleGeneral: sessionAnswer(`{"title":"huge finding","body":"` + strings.Repeat("x", jevStateBytes*2) + `"}`)}}
	}
	baseline, _ := runJevReview(t, nil, agent())
	p := &jevtest.Provider{}
	a, _ := runJevReview(t, fakeJev(p), agent())
	rec := readJev(t, a)
	if !reflect.DeepEqual(a.Findings, baseline.Findings) || len(p.Requests()) != 0 || rec.Reason != jev.ReasonRejected || !strings.Contains(rec.Detail, "complete findings") {
		t.Fatalf("oversized findings: %+v", rec)
	}
}

func TestJevShortensContextInOrderAndNeverFindings(t *testing.T) {
	b := testBrief()
	b.TouchedAreas[0].Summary = strings.Repeat("touch-", jevStateBytes)
	b.Summary = strings.Repeat("summary-", 100)
	findings := []Finding{{Title: "complete title", Body: "complete body", Suggestion: "replacement", Evidence: "evidence"}}
	req, err := jevRequest(b, findings, "", nil, jev.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	state := req.State.(string)
	if !strings.Contains(state, "[shortened]") || !strings.Contains(state, b.Summary) || !strings.Contains(state, `"body":"complete body"`) {
		t.Fatal("shortening lost higher priority context or finding")
	}
	body, err := req.Body(jev.DefaultModel)
	if err != nil || len(body) > jevRequestBytes || len(state) > jevStateBytes {
		t.Fatal("budget not respected")
	}
	// Exercise all shortening stages, including byte-heavy JSON escaping.
	b.Summary = strings.Repeat("\x00", jevStateBytes)
	b.Title = strings.Repeat("世界", jevStateBytes)
	b.AcceptanceCriteria = []Point{{Text: strings.Repeat("a", jevStateBytes)}}
	b.StyleRules = []Point{{Text: strings.Repeat("s", jevStateBytes)}}
	rules := []Rule{{Text: strings.Repeat("n", jevStateBytes)}}
	b.Excluded = []ExcludedFile{{Path: strings.Repeat("e", jevStateBytes)}}
	req, err = jevRequest(b, findings, "", rules, jev.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if req.Validate() != nil || !strings.Contains(req.State.(string), `"evidence":"evidence"`) {
		t.Fatal("lost finding")
	}
}

func TestJevReviewCancellationPreservesPostPipelineContract(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &jevtest.Provider{Wait: func(ctx context.Context) error { cancel(); return jevtest.BlockUntilDone(ctx) }}
	r, _ := testRunner(t, &reviewAgent{}, &Notes{Rules: []Rule{shortNames(RuleDownrank)}})
	r.Jev = fakeJev(p)
	a, err := r.Run(ctx, Ref{Repo: testRepo, Number: 7})
	if err != nil || ctx.Err() != context.Canceled || a == nil {
		t.Fatalf("post-pipeline result %v, %v", a, err)
	}
	if !reflect.DeepEqual(a.Findings, baseline.Findings) || hasAcceptedJev(t, a) {
		t.Fatal("cancelled review boosted")
	}
	// Compare with cancellation immediately after the pipeline: callers get
	// artifact, nil and a cancelled context, as scheduler.runReview expects.
	r, _ = testRunner(t, &reviewAgent{}, &Notes{Rules: []Rule{shortNames(RuleDownrank)}})
	after, cancelAfter := context.WithCancel(context.Background())
	defer cancelAfter()
	prior, priorErr := r.Run(after, Ref{Repo: testRepo, Number: 7})
	cancelAfter()
	if priorErr != err || after.Err() != ctx.Err() || !reflect.DeepEqual(prior.Findings, a.Findings) {
		t.Fatal("cancellation contract changed")
	}
}

func TestJevNoFindingsMakesNoRequest(t *testing.T) {
	agent := &reviewAgent{answers: map[string]string{AngleGeneral: sessionAnswer(), AngleTests: sessionAnswer()}}
	p := &jevtest.Provider{}
	a, log := runJevReview(t, fakeJev(p), agent)
	if len(p.Requests()) != 0 || len(a.Findings.Items) != 0 || strings.Contains(log, "\nJev ") {
		t.Fatal("empty review asked Jev")
	}
	if _, err := os.Stat(filepath.Join(a.Dir, JevFile)); !os.IsNotExist(err) {
		t.Fatal("empty review recorded judgment")
	}
}

func assertNoJevKey(t *testing.T, a *Artifact, log string) {
	t.Helper()
	if strings.Contains(log, jevTestKey) {
		t.Fatal("API key in log")
	}
	for path, body := range artifactFiles(t, a) {
		if strings.Contains(body, jevTestKey) {
			t.Errorf("API key in %s", path)
		}
	}
}

type countingTransport struct{ calls int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, fmt.Errorf("network must not be reached")
}

func TestJevDefaultProviderRefusesNetworkInTestBinary(t *testing.T) {
	t.Setenv(jev.DefaultAPIKeyEnv, jevTestKey)
	transport := &countingTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old })
	j := NewJevJudge(jev.DefaultSettings())
	a, _ := runJevReview(t, j, &reviewAgent{})
	if rec := readJev(t, a); rec.Reason != jev.ReasonRejected || !strings.Contains(rec.Detail, "test binary") {
		t.Fatalf("guard: %+v", rec)
	}
	// A directly constructed Judge with an implicit default provider is also
	// refused by Runner, so forgetting a fake cannot send a real request.
	j.Provider = nil
	a, _ = runJevReview(t, j, &reviewAgent{})
	if readJev(t, a).Reason != jev.ReasonRejected || transport.calls != 0 {
		t.Fatal("default provider reached the network")
	}
}

func TestJevConfigStrictKeysValuesAndDefaults(t *testing.T) {
	cfg, err := ParseConfig("", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jev.Settings() != jev.DefaultSettings() {
		t.Fatalf("defaults %+v", cfg.Jev.Settings())
	}
	for _, tc := range []struct{ key, value string }{
		{"unknown", `"x"`}, {"enabled", "true"}, {"url", `"file:///tmp/jev"`}, {"url", `"https://example.com/?key=secret"`},
		{"url", `""`}, {"model", `"jev latest"`}, {"model", `""`}, {"api_key_env", `"1BAD"`}, {"api_key_env", `""`},
		{"timeout", `"0s"`}, {"timeout", `"2m"`}, {"timeout", `"not a duration"`}, {"timeout", "12"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			_, err := ParseConfig("[jev]\n"+tc.key+" = "+tc.value+"\n", cfg.Path)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.key) {
				t.Fatalf("key %s: %v", tc.key, err)
			}
		})
	}
	configHome(t)
	path := writeFile(t, t.TempDir(), ConfigFile, "[jev]\nurl = \"https://example.com/api\"\nmodel = \"jev-test\"\napi_key_env = \"TEST_JEV_KEY\"\ntimeout = \"3s\"\n")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Jev.Settings()
	if s.URL != "https://example.com/api" || s.Model != "jev-test" || s.APIKeyEnv != "TEST_JEV_KEY" || s.Timeout != 3*time.Second {
		t.Fatalf("settings %+v", s)
	}
	t.Setenv("TEST_JEV_KEY", jevTestKey)
	dump, _ := json.Marshal(cfg)
	if strings.Contains(string(dump), jevTestKey) {
		t.Fatal("config dump leaked API key")
	}
	j := NewJevJudge(s)
	if !j.Config().Enabled {
		t.Fatal("custom key did not enable boost")
	}
	t.Setenv("TEST_JEV_KEY", "")
	if j.Config().Enabled {
		t.Fatal("empty custom key enabled boost")
	}
}

func TestJevKeepsEveryFindingFieldAndAlsoFrom(t *testing.T) {
	newAgent := func() *reviewAgent {
		return &reviewAgent{answers: map[string]string{
			AngleGeneral: sessionAnswer(`{"category":"bug","severity":"high","file":"widget.go","lines":[1,2],"side":"new","title":"Widget loses the second result","body":"The second item is lost when collecting results.","suggestion":"return results","evidence":"The reviewer observed a missing second result.","sources":["#12","CLAUDE.md"]}`),
			AngleTests:   sessionAnswer(`{"category":"bug","severity":"high","file":"widget.go","lines":[1,2],"side":"new","title":"Widget loses the second result","body":"The second item is lost when collecting results."}`),
		}}
	}
	baseline, _ := runJevReview(t, nil, newAgent())
	if len(baseline.Findings.Items) != 1 || len(baseline.Findings.Items[0].AlsoFrom) != 1 {
		t.Fatalf("fixture not merged: %+v", baseline.Findings.Items)
	}
	p := &jevtest.Provider{Results: []jevtest.Result{jevAnswer(0.8)}}
	a, _ := runJevReview(t, fakeJev(p), newAgent())
	if !reflect.DeepEqual(a.Findings, baseline.Findings) {
		t.Fatal("kept finding fields changed")
	}
	data, _ := json.Marshal(baseline.Findings.Items[0])
	if !strings.Contains(p.Requests()[0].State.(string), string(data)) {
		t.Fatal("state omits finding fields")
	}
}

func TestJevBlockedProviderTimesOutWithLiveReviewContext(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	p := &jevtest.Provider{Wait: func(ctx context.Context) error {
		<-ctx.Done()
		return &systemone.Error{Kind: systemone.KindTimeout, Message: "request deadline passed"}
	}}
	j := fakeJev(p)
	j.Config = func() jev.Settings {
		s := jev.DefaultSettings()
		s.Enabled = true
		s.Timeout = 5 * time.Millisecond
		return s
	}
	a, _ := runJevReview(t, j, &reviewAgent{})
	if !reflect.DeepEqual(a.Findings, baseline.Findings) || readJev(t, a).Reason != jev.ReasonTimeout || len(p.Requests()) != 2 {
		t.Fatal("timeout changed review")
	}
}

func TestJevSuccessAfterReviewCancellationCannotBoost(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &jevtest.Provider{Wait: func(context.Context) error { cancel(); return nil }, Results: []jevtest.Result{jevAnswer(0.9, 0.1, 0.8)}}
	r, _ := testRunner(t, &reviewAgent{}, &Notes{Rules: []Rule{shortNames(RuleDownrank)}})
	r.Jev = fakeJev(p)
	a, err := r.Run(ctx, Ref{Repo: testRepo, Number: 7})
	if err != nil || ctx.Err() == nil || !reflect.DeepEqual(a.Findings, baseline.Findings) || hasAcceptedJev(t, a) {
		t.Fatal("late success hid review cancellation")
	}
}

func TestJevStateIncludesExcludedFilesButNotOtherBriefMetadata(t *testing.T) {
	b := testBrief()
	b.Excluded = []ExcludedFile{{Path: "generated.go", Added: 10, Removed: 8, Reason: "generated header"}}
	req, err := jevRequest(b, []Finding{{Title: "A finding", Body: "Its body"}}, "", nil, jev.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	state := req.State.(string)
	for _, want := range []string{"generated.go", `"added":10`, `"removed":8`, "generated header"} {
		if !strings.Contains(state, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, forbidden := range []string{b.Author, b.SessionID, b.NotGathered[0], "Gathered from"} {
		if strings.Contains(state, forbidden) {
			t.Errorf("unexpected %s", forbidden)
		}
	}
}

func hasAcceptedJev(t *testing.T, a *Artifact) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Join(a.Dir, JevFile)); os.IsNotExist(err) {
		return false
	}
	return readJev(t, a).Outcome == jev.Accepted
}

func TestJevRequestCancellationAloneLeavesReviewLive(t *testing.T) {
	baseline, _ := runJevReview(t, nil, &reviewAgent{})
	p := &jevtest.Provider{Wait: func(ctx context.Context) error {
		request, cancel := context.WithCancel(ctx)
		cancel()
		return jevtest.BlockUntilDone(request)
	}}
	ctx := context.Background()
	r, log := testRunner(t, &reviewAgent{}, &Notes{Rules: []Rule{shortNames(RuleDownrank)}})
	r.Jev = fakeJev(p)
	a, err := r.Run(ctx, Ref{Repo: testRepo, Number: 7})
	if err != nil || ctx.Err() != nil || !reflect.DeepEqual(a.Findings, baseline.Findings) {
		t.Fatal("request cancellation changed review")
	}
	if readJev(t, a).Reason != jev.ReasonCancelled || !strings.Contains(log.String(), "Jev fell back: cancelled") {
		t.Fatal("request cancellation not recorded")
	}
}
