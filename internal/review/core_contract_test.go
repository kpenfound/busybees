package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAcquiredBundleReachesCoreWithoutLosingContext(t *testing.T) {
	b := testBundle()
	b.Items = append(b.Items, Item{Source: "custom source", Name: "specification", Content: "```\ncomplete text\n```"})
	agent := &fakeAgent{answer: answeredBrief, id: "distiller", cost: 1.25}
	brief, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	wantContext := "# Context for acme/widgets#7\n\nhttps://github.com/acme/widgets/pull/7\n\n## diff: acme/widgets#7\n\n```\ndiff --git a/gather.go b/gather.go\n```\n\n## style_files: CLAUDE.md\n\n```\nEvery new key needs a test.\n```\n\n## custom source: specification\n\n````\n```\ncomplete text\n```\n````\n\n## Not gathered\n\n- #99 is mentioned by acme/widgets#7 and is not an issue that could be read\n"
	if !strings.Contains(agent.req.Prompt, wantContext) {
		t.Fatalf("distiller context changed:\n%s", agent.req.Prompt)
	}
	if brief.Ref != b.Ref || brief.Title != b.PR.Title || brief.Author != b.PR.Author.Login || !reflect.DeepEqual(brief.NotGathered, b.Skipped) || !reflect.DeepEqual(brief.Sources, []string{SourceDiff, SourceStyleFiles, "custom source"}) {
		t.Fatalf("lost acquisition metadata: %+v", brief)
	}
	if len(brief.AcceptanceCriteria) != 1 || len(brief.StyleRules) != 1 || len(brief.TouchedAreas) != 1 || brief.CostUSD != 1.25 {
		t.Fatalf("lost brief content: %+v", brief)
	}
}

func TestLegacyArtifactStillReadsWritesAndTriages(t *testing.T) {
	// These are the pre-extraction wire shapes, including the concrete GitHub ref.
	files := map[string]string{
		BriefFile: `{"ref":{"repo":"acme/widgets","number":7},"title":"title","author":"author","summary":"summary","size":"m","acceptance_criteria":[{"text":"criterion","source":"#12"}],"style_rules":[{"text":"rule"}],"touched_areas":[{"name":"area","paths":["a.go"]}],"sources":["diff"],"not_gathered":["missing context"],"session_id":"distiller","cost_usd":0.5}`,
		filepath.Join(AnglesDir, AngleGeneral+".json"): `{"angle":"general","provider":"claude","model":"opus","dir":"/checkout","session_id":"angle","answer":"answer","turns":3,"cost_usd":0.75}`,
		filepath.Join(AnglesDir, AngleDocs+".json"):    `{"angle":"docs","provider":"claude","dir":"/checkout","error":"unavailable"}`,
		FindingsFile: `{"findings":[{"id":"12345678","angle":"general","session_id":"angle","category":"bug","severity":"high","file":"a.go","lines":[1,2],"side":"new","title":"title","body":"body","suggestion":"fix","evidence":"proof","sources":["spec"],"also_from":["test_coverage"]}],"skipped":["docs failed"],"silenced":[{"id":"87654321","title":"quiet","angle":"general","category":"style","action":"drop","severity":"low","rule":"rule"}]}`,
		TriageFile:   `{"decisions":[{"finding":"12345678","action":"select","comment":"edited"}]}`,
	}
	dir := t.TempDir()
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, name, data)
	}
	artifact, err := ReadArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Brief.Ref != (Ref{Repo: testRepo, Number: 7}) || artifact.Runs[0].Turns != 3 || artifact.Runs[0].CostUSD != 0.75 || !artifact.Runs[1].Failed() {
		t.Fatalf("legacy identity/accounting/failed angle: %+v", artifact)
	}
	queue, err := NewQueue(artifact, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Pending()) != 0 || len(queue.Selected()) != 1 || queue.Selected()[0].Comment != "edited" {
		t.Fatalf("triage decisions changed: %+v", queue.Selected())
	}
	if err := artifact.Write(); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var before, after any
		if err := json.Unmarshal([]byte(want), &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &after); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s wire shape changed:\n%s", name, data)
		}
	}
}
