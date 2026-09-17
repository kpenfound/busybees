package review

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExcludeDropsTheFindingsAnchoredInAnExcludedFile(t *testing.T) {
	findings := []Finding{
		{File: "widget.go", Lines: LineRange{1, 1}, Side: SideNew, Title: "hand"},
		{File: "widget.pb.go", Lines: LineRange{3, 3}, Side: SideNew, Title: "generated"},
		{File: "./widget.pb.go", Lines: LineRange{5, 5}, Side: SideOld, Title: "generated, spelled with ./"},
		{Title: "the change as a whole"},
	}
	excluded := []ExcludedFile{{Path: "widget.pb.go", Added: 10}}
	kept, dropped := Exclude(findings, excluded)
	if got := titlesOf(kept); !reflect.DeepEqual(got, []string{"hand", "the change as a whole"}) {
		t.Errorf("kept %v", got)
	}
	if got := titlesOf(dropped); !reflect.DeepEqual(got, []string{"generated", "generated, spelled with ./"}) {
		t.Errorf("dropped %v", got)
	}
	// Nothing excluded is the list as it was.
	if kept, dropped := Exclude(findings, nil); len(kept) != 4 || dropped != nil {
		t.Errorf("with nothing excluded: kept %d, dropped %v", len(kept), dropped)
	}
}

func titlesOf(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Title)
	}
	return out
}

func TestARunDropsTheFindingsOnGeneratedFilesAndTellsEverySessionAboutThem(t *testing.T) {
	bundle := testBundle()
	bundle.Excluded = []ExcludedFile{{Path: "widget.pb.go", Added: 1200, Removed: 30, Reason: "generated header"}}
	dir := filepath.Join(t.TempDir(), "artifact")
	prompts := map[string]string{}
	agent := agentFunc(func(_ context.Context, req AgentRequest) (*AgentResult, error) {
		prompts[req.Name] = req.Prompt
		switch req.Name {
		case DistillerName:
			return &AgentResult{ID: "brief-session", Text: answeredBrief}, nil
		case AngleGeneral:
			return &AgentResult{ID: "general-session", Text: sessionAnswer(
				`{"title":"generated","body":"a name in the binding","severity":"high","category":"naming","file":"widget.pb.go","lines":[4,4],"side":"new"}`,
				`{"title":"hand","body":"a name in the hand file","severity":"low","category":"naming","file":"gather.go","lines":[4,4],"side":"new"}`,
			)}, nil
		default:
			return &AgentResult{ID: req.Name + "-session", Text: `{"findings": []}`}, nil
		}
	})
	var log bytes.Buffer
	runner := &Runner[testRef]{Distiller: &Distiller[testRef]{Agent: agent}, Angles: &Angles[testRef]{Agent: agent, Provider: "fake", Model: "base"}, Settings: onlyAngle(t, AngleGeneral), Log: &log}
	artifact, err := runner.Run(context.Background(), dir, bundle, testDiff)
	if err != nil {
		t.Fatal(err)
	}
	if got := titlesOf(artifact.Findings.Items); !reflect.DeepEqual(got, []string{"hand"}) {
		t.Errorf("findings %v, want the one on the hand file alone", got)
	}
	if !strings.Contains(log.String(), "  1 finding on generated files dropped\n") {
		t.Errorf("log:\n%s", log.String())
	}
	wantLine := "- widget.pb.go (+1200 -30; generated header)\n"
	for _, name := range []string{DistillerName, AngleGeneral} {
		if p := prompts[name]; !strings.Contains(p, "## Generated files not reviewed\n") || !strings.Contains(p, wantLine) {
			t.Errorf("the %s session was not told about the excluded file:\n%s", name, p)
		}
	}
	if !reflect.DeepEqual(artifact.Brief.Excluded, bundle.Excluded) {
		t.Errorf("brief.Excluded %+v, want the bundle's", artifact.Brief.Excluded)
	}
	read, err := ReadBrief[testRef](dir)
	if err != nil || !reflect.DeepEqual(read.Excluded, bundle.Excluded) {
		t.Errorf("brief read back: %v %+v", err, read)
	}
}

func TestABriefWrittenBeforeGeneratedFilesWereExcludedReadsBackWithNone(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"ref":{"Key":"component#7","Scope":"component","Link":"https://example.test/7"},"summary":"summary","size":"m","sources":["diff"],"not_gathered":["missing"]}`
	if err := os.WriteFile(filepath.Join(dir, BriefFile), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ReadBrief[testRef](dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.Excluded != nil || b.Summary != "summary" || !reflect.DeepEqual(b.NotGathered, []string{"missing"}) {
		t.Errorf("legacy brief read as %+v", b)
	}
	if strings.Contains(b.Text(), "Generated files") {
		t.Errorf("a brief with no excluded files has the heading:\n%s", b.Text())
	}
}
