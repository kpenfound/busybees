package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/github"
)

// fakeAgent stands in for the coding agent: it records the session it was
// asked to run and answers with what the test gave it.
type fakeAgent struct {
	answer string
	id     string
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
	return &AgentResult{ID: f.id, Text: f.answer, Turns: 2}, nil
}

func testBundle() *Bundle {
	return &Bundle{
		Ref: Ref{Repo: testRepo, Number: 7},
		PR:  github.PR{Number: 7, Title: "widgets: gather the context"},
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

func TestTheDistillerReadsTheBundleAndWritesTheBrief(t *testing.T) {
	agent := &fakeAgent{answer: answeredBrief, id: "sess-1"}
	brief, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), testBundle())
	if err != nil {
		t.Fatal(err)
	}
	want := &Brief{
		Ref:                Ref{Repo: testRepo, Number: 7},
		Title:              "widgets: gather the context",
		Summary:            "gathers the context sources a project declares",
		Size:               "m",
		AcceptanceCriteria: []Point{{Text: "a source that cannot read something does not fail the review", Source: "#566"}},
		StyleRules:         []Point{{Text: "every new key needs a test", Source: "CLAUDE.md"}},
		TouchedAreas:       []TouchedArea{{Name: "internal/review", Paths: []string{"gather.go"}, Summary: "the pipeline and its sources"}},
		Sources:            []string{SourceDiff, SourceStyleFiles},
		NotGathered:        []string{"#99 is mentioned by acme/widgets#7 and is not an issue that could be read"},
		SessionID:          "sess-1",
	}
	if !reflect.DeepEqual(brief, want) {
		t.Errorf("brief =\n%+v\nwant\n%+v", brief, want)
	}
}

func TestTheFactsInTheBriefComeFromTheBundle(t *testing.T) {
	// A session that answered with a pull request, a title or a session id
	// of its own is answering something it was not asked: bees knows those,
	// and takes them from the gather.
	agent := &fakeAgent{answer: `{"summary": "s", "size": "xs", "ref": {"repo": "evil/repo", "number": 1},
		"title": "a title of its own", "session_id": "made up",
		"sources": ["invented"], "not_gathered": ["nothing at all"]}`, id: "sess-1"}
	brief, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), testBundle())
	if err != nil {
		t.Fatal(err)
	}
	if brief.Ref != (Ref{Repo: testRepo, Number: 7}) {
		t.Errorf("ref = %s, want the one the bundle was gathered for", brief.Ref)
	}
	if brief.Title != "widgets: gather the context" {
		t.Errorf("title = %q, want the pull request's", brief.Title)
	}
	if brief.SessionID != "sess-1" {
		t.Errorf("session id = %q, want the session's own", brief.SessionID)
	}
	if got := strings.Join(brief.Sources, ","); got != SourceDiff+","+SourceStyleFiles {
		t.Errorf("sources = %q, want the bundle's", got)
	}
	if len(brief.NotGathered) != 1 || !strings.HasPrefix(brief.NotGathered[0], "#99") {
		t.Errorf("not gathered = %q, want what the gather skipped", brief.NotGathered)
	}
}

func TestTheDistillerSessionIsToldWhatToDoAndGivenTheBundle(t *testing.T) {
	agent := &fakeAgent{answer: answeredBrief}
	if _, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), testBundle()); err != nil {
		t.Fatal(err)
	}
	prompt := agent.req.Prompt
	for _, want := range []string{
		"You are the distiller of a pull request review.",
		"acceptance_criteria",
		"5. **Size**",
		"`xs`, `s`, `m`, `l` or `xl`",
		"from the change's scope and its risk",
		`"size": "m",`,
		"# Context for acme/widgets#7",
		"diff --git a/gather.go b/gather.go",
		"Every new key needs a test.",
		"Write the brief for acme/widgets#7.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the distiller was not told %q:\n%s", want, prompt)
		}
	}
	if agent.req.Name != DistillerName {
		t.Errorf("session name = %q, want %q", agent.req.Name, DistillerName)
	}
	if i, j := strings.Index(prompt, "You are the distiller"), strings.Index(prompt, "# Context for"); i > j {
		t.Errorf("the bundle comes before the instructions:\n%s", prompt)
	}
}

func TestWithoutACheckoutTheSessionRunsInAnEmptyDirectory(t *testing.T) {
	agent := &fakeAgent{answer: answeredBrief}
	if _, err := (&Distiller{Agent: agent}).Distill(context.Background(), testBundle()); err != nil {
		t.Fatal(err)
	}
	// The session must not read whichever repository the command was run
	// in, so it is given a directory of its own, and it is not left behind.
	if agent.req.Dir == "" {
		t.Fatal("the session ran in the directory the command was run in")
	}
	if _, err := os.Stat(agent.req.Dir); !os.IsNotExist(err) {
		t.Errorf("%s was left behind: %v", agent.req.Dir, err)
	}
}

func TestTheDistillerRunsInTheCheckout(t *testing.T) {
	dir := t.TempDir()
	agent := &fakeAgent{answer: answeredBrief}
	if _, err := (&Distiller{Agent: agent, Dir: dir}).Distill(context.Background(), testBundle()); err != nil {
		t.Fatal(err)
	}
	if agent.req.Dir != dir {
		t.Errorf("session ran in %s, want the checkout %s", agent.req.Dir, dir)
	}
}

func TestASessionThatFailedProducesNoBrief(t *testing.T) {
	agent := &fakeAgent{err: errors.New("distiller session: no capacity")}
	_, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), testBundle())
	if err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Fatalf("err = %v, want what the session failed with", err)
	}
}

func TestAnAnswerThatIsNotABriefIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, answer, want string }{
		{"prose", "I could not read the diff, sorry.", "no JSON object"},
		{"a summary that is not text", `{"summary": ["s"]}`, "is not a brief"},
		{"no summary", `{"acceptance_criteria": [{"text": "a"}]}`, "no summary"},
		{"an empty summary", `{"summary": "   ", "size": "m"}`, "no summary"},
		{"no size", `{"summary": "s"}`, "no size"},
		{"an empty size", `{"summary": "s", "size": "  "}`, "no size"},
		{"a size that is not one", `{"summary": "s", "size": "huge"}`, `"huge", which is not one of xs, s, m, l, xl`},
		{"a size that is not text", `{"summary": "s", "size": 3}`, "is not a brief"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{answer: tc.answer}
			_, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), testBundle())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "acme/widgets#7") {
				t.Errorf("err = %v, want the pull request named", err)
			}
		})
	}
}

func TestTheBriefIsReadOutOfWhateverTheSessionWrappedItIn(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{
		{"the object alone", `{"summary": "the one", "size": "xs"}`},
		{"prose around it", "Here is the brief:\n\n" + `{"summary": "the one", "size": "xs"}` + "\n\nHope it helps."},
		{"a fenced block", "Here it is:\n\n```json\n" + `{"summary": "the one", "size": "xs"}` + "\n```\n"},
		{"the last of two blocks", "The shape:\n```json\n{\"summary\": \"an example\", \"size\": \"m\"}\n```\nThe brief:\n```\n{\"summary\": \"the one\", \"size\": \"xs\"}\n```\n"},
		{"a block that is not the brief after it", "```json\n{\"summary\": \"the one\", \"size\": \"xs\"}\n```\nThe line it came from:\n```\ndiff --git a/x b/x\n```\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{answer: tc.answer}
			brief, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), testBundle())
			if err != nil {
				t.Fatal(err)
			}
			if brief.Summary != "the one" {
				t.Errorf("summary = %q, want the brief the session ended with", brief.Summary)
			}
		})
	}
}

func TestAStatementWithNothingInItIsNotInTheBrief(t *testing.T) {
	agent := &fakeAgent{answer: `{"summary": " s ", "size": " XL ",
		"acceptance_criteria": [{"text": " keeps the criterion ", "source": " #12 "}, {"text": "  "}],
		"style_rules": [{"text": ""}],
		"touched_areas": [{"name": ""}, {"name": " internal/review ", "paths": ["a.go", " "]}]}`}
	brief, err := (&Distiller{Agent: agent, Dir: t.TempDir()}).Distill(context.Background(), testBundle())
	if err != nil {
		t.Fatal(err)
	}
	if brief.Summary != "s" {
		t.Errorf("summary = %q, want it trimmed", brief.Summary)
	}
	if brief.Size != "xl" {
		t.Errorf("size = %q, want it trimmed and lowercased", brief.Size)
	}
	want := []Point{{Text: "keeps the criterion", Source: "#12"}}
	if !reflect.DeepEqual(brief.AcceptanceCriteria, want) {
		t.Errorf("acceptance criteria = %+v, want %+v", brief.AcceptanceCriteria, want)
	}
	if brief.StyleRules != nil {
		t.Errorf("style rules = %+v, want none", brief.StyleRules)
	}
	wantAreas := []TouchedArea{{Name: "internal/review", Paths: []string{"a.go"}}}
	if !reflect.DeepEqual(brief.TouchedAreas, wantAreas) {
		t.Errorf("touched areas = %+v, want %+v", brief.TouchedAreas, wantAreas)
	}
}

func TestDistillingWithoutABundle(t *testing.T) {
	agent := &fakeAgent{answer: answeredBrief}
	if _, err := (&Distiller{Agent: agent}).Distill(context.Background(), nil); err == nil {
		t.Fatal("a review with no context gathered produced a brief")
	}
	if agent.runs != 0 {
		t.Errorf("%d sessions ran with nothing to read", agent.runs)
	}
}

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

func TestASessionCannotAnswerWithWhatItWasNotAsked(t *testing.T) {
	// parseBrief takes the five things the session was asked for and
	// nothing else, so a brief carries no fact a session made up even
	// before Distill fills the facts in from the bundle.
	brief, err := parseBrief(`{"summary": "s", "size": "xs", "ref": {"repo": "evil/repo", "number": 1},
		"title": "a title of its own", "session_id": "made up",
		"sources": ["invented"], "not_gathered": ["nothing at all"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if brief.Ref != (Ref{}) || brief.Title != "" || brief.SessionID != "" || brief.Sources != nil || brief.NotGathered != nil {
		t.Errorf("brief = %+v, want only what the session was asked for", brief)
	}
}
