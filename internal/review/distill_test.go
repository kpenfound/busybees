package review

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kpenfound/busybees/internal/github"
)

// fakeAgent stands in for the coding agent: it records the session it was
// asked to run and answers with what the test gave it.
type fakeAgent struct {
	answer string
	id     string
	cost   float64
	err    error
	req    AgentRequest
	runs   int
}

func (f *fakeAgent) Run(_ context.Context, req AgentRequest) (*AgentResult, error) {
	f.runs++
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	return &AgentResult{ID: f.id, Text: f.answer, Turns: 2, CostUSD: f.cost}, nil
}

func testBundle() *Bundle {
	return &Bundle{
		Ref: Ref{Repo: testRepo, Number: 7},
		PR:  github.PR{Number: 7, Title: "widgets: gather the context", Author: github.Author{Login: "octocat"}},
		Items: []Item{
			{Source: SourceDiff, Name: "acme/widgets#7", Content: "diff --git a/gather.go b/gather.go"},
			{Source: SourceStyleFiles, Name: "CLAUDE.md", Content: "Every new key needs a test."},
		},
		Skipped: []string{"#99 is mentioned by acme/widgets#7 and is not an issue that could be read"},
	}
}

const answeredBrief = `{
  "summary": "gathers the context sources a project declares",
  "size": "m",
  "acceptance_criteria": [{"text": "a source that cannot read something does not fail the review", "source": "#566"}],
  "style_rules": [{"text": "every new key needs a test", "source": "CLAUDE.md"}],
  "touched_areas": [{"name": "internal/review", "paths": ["gather.go"], "summary": "the pipeline and its sources"}]
}`

func TestTheDistillerRunsTheConfiguredAgent(t *testing.T) {
	cfg, err := ParseConfig("provider = \"codex\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDistiller(cfg, "/checkout")
	agent, ok := d.Agent.(*CLIAgent)
	if !ok {
		t.Fatalf("agent = %T, want the CLI agent", d.Agent)
	}
	if agent.Provider != "codex" || d.Dir != "/checkout" {
		t.Errorf("distiller = %+v, %+v, want the configured provider and the checkout", agent, d.Dir)
	}
}

func TestTheDistillerRunsAsTheBriefModel(t *testing.T) {
	for _, tc := range []struct{ toml, want string }{
		{"model = \"opus\"\nbrief_model = \"sonnet\"\n", "sonnet"},
		{"model = \"opus\"\nangle_models.docs = \"haiku\"\n", "opus"},
	} {
		cfg, err := ParseConfig(tc.toml, filepath.Join(t.TempDir(), ConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		agent := NewDistiller(cfg, "").Agent.(*CLIAgent)
		if agent.Model != tc.want || agent.Provider != DefaultProvider {
			t.Errorf("%s: distiller agent = %+v, want model %q", tc.toml, agent, tc.want)
		}
	}
}
