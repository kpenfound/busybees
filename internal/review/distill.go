package review

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// distillerInstructions is what the distiller session is told to do. The
// context bundle and the pull request it is about are appended to it.
//
//go:embed prompts/distiller.md
var distillerInstructions string

// DistillerName is the name of the distiller session, in an error and in the
// artifact directory.
const DistillerName = "distiller"

// Distiller runs the first session of a review: it reads the context bundle
// and writes the brief every session after it starts from.
//
// It is the session that turns a gather into a review. The bundle is raw and
// as large as the pull request made it; the brief is short, angle-agnostic
// and the same for every angle, so a change in what the angles are told
// starts here rather than in five prompts.
type Distiller struct {
	// Agent runs the session.
	Agent Agent
	// Dir is the checkout of the repository under review, which the
	// session's read-only tools can reach. It is "" on a machine that has
	// none (see CheckoutOf), and the session then runs in an empty
	// directory of its own: a review must not read whichever repository the
	// command happened to be run in.
	Dir string
}

// NewDistiller is the distiller a review runs, as the global configuration
// says: cfg's provider and model, and dir the checkout Open was given.
func NewDistiller(cfg *Config, dir string) *Distiller {
	return &Distiller{Agent: NewAgent(cfg), Dir: dir}
}

// Distill runs the distiller session over the bundle and returns the brief
// it produced. The session is asked for its reading of the context and for
// nothing bees already knows: the pull request, the sources gathered and
// what they could not read are copied out of the bundle afterwards, so no
// part of the brief is a fact a session could have got wrong.
func (d *Distiller) Distill(ctx context.Context, b *Bundle) (*Brief, error) {
	if b == nil {
		return nil, errors.New("distill: no context bundle")
	}
	dir := d.Dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "bees-review-")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}
	res, err := d.Agent.Run(ctx, AgentRequest{Name: DistillerName, Prompt: distillPrompt(b), Dir: dir})
	if err != nil {
		return nil, err
	}
	brief, err := parseBrief(res.Text)
	if err != nil {
		return nil, fmt.Errorf("the distiller session produced no brief for %s: %w", b.Ref, err)
	}
	brief.Ref = b.Ref
	brief.Title = b.PR.Title
	brief.Sources = b.Sources()
	brief.NotGathered = b.Skipped
	brief.SessionID = res.ID
	return brief, nil
}

// distillPrompt is the whole of what the distiller session is told: what to
// do, the context that was gathered, and what to answer with. The
// instructions come first and are repeated in one line at the end, after a
// bundle that can be very long.
func distillPrompt(b *Bundle) string {
	var out strings.Builder
	out.WriteString(distillerInstructions)
	out.WriteString("\n---\n\n")
	out.WriteString(b.Text())
	fmt.Fprintf(&out, "\n---\n\nWrite the brief for %s. Answer with the JSON object alone.\n", b.Ref)
	return out.String()
}

// briefDraft is the part of a brief the distiller writes. The rest of Brief
// is filled in from the bundle, and a session that answered with a ref or a
// session id of its own is answering something it was not asked.
type briefDraft struct {
	Summary            string        `json:"summary"`
	AcceptanceCriteria []Point       `json:"acceptance_criteria"`
	StyleRules         []Point       `json:"style_rules"`
	TouchedAreas       []TouchedArea `json:"touched_areas"`
}

// parseBrief reads the brief out of what the session answered.
func parseBrief(text string) (*Brief, error) {
	obj, ok := jsonObject(text)
	if !ok {
		return nil, errors.New("the session answered with no JSON object")
	}
	var draft briefDraft
	if err := json.Unmarshal([]byte(obj), &draft); err != nil {
		return nil, fmt.Errorf("the session's JSON object is not a brief: %w", err)
	}
	brief := &Brief{
		Summary:            draft.Summary,
		AcceptanceCriteria: draft.AcceptanceCriteria,
		StyleRules:         draft.StyleRules,
		TouchedAreas:       draft.TouchedAreas,
	}
	brief.normalise()
	if err := brief.Validate(); err != nil {
		return nil, err
	}
	return brief, nil
}

// jsonBlock matches a fenced JSON block, which is how a session answers when
// it cannot help formatting its answer.
var jsonBlock = regexp.MustCompile("(?s)```(?:json)?[ \t]*\r?\n(.*?)```")

// jsonObject is the JSON object in a session's answer: the last fenced block
// when it fenced one, and otherwise everything from the first brace to the
// last, which is the whole answer when the session answered as it was asked
// to.
func jsonObject(text string) (string, bool) {
	blocks := jsonBlock.FindAllStringSubmatch(text, -1)
	for i := len(blocks) - 1; i >= 0; i-- {
		if block := strings.TrimSpace(blocks[i][1]); strings.HasPrefix(block, "{") {
			return block, true
		}
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return "", false
	}
	return text[start : end+1], true
}
