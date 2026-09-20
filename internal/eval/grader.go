package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/review"
)

// A check that needs judgement is decided by a session of its own, the way
// Inspect AI's model-graded scorers work: the case supplies a rubric, and
// the grader session is given that rubric, the transcript of the role's
// session and the state the run left behind, and answers with a score
// between 0 and 1 and the reasons for it.
//
// The grader is deliberately not the agent the eval runs the role on. Two
// runs of a case are compared by their scores, so what scores them has to
// stay put while --profile changes what is being scored: cmd/bees builds it
// from ~/.config/bees/config.toml, as the read-only session agent that
// `bees review` runs, and --profile does not reach it.

const (
	// DefaultPassScore is the score a graded check passes at when its
	// rubric names none.
	DefaultPassScore = 0.7
	// MaxTranscriptBytes bounds the transcript a grader session is given.
	// A longer one is cut in the middle, where a session says least about
	// what it decided.
	MaxTranscriptBytes = 128 << 10
	// MaxBodyBytes bounds one issue body, comment or message in the end
	// state a grader session is given.
	MaxBodyBytes = 8 << 10
)

// Rubric is one graded check: what the grader session is asked to judge,
// and the score the check passes at.
type Rubric struct {
	// Name is the requirement, as the report names it.
	Name string `toml:"name"`
	// Rubric is what the grader is told to judge the session by.
	Rubric string `toml:"rubric"`
	// Pass is the score the check passes at, DefaultPassScore when unset.
	Pass float64 `toml:"pass"`
}

// PassScore is the score the check passes at.
func (r Rubric) PassScore() float64 {
	if r.Pass <= 0 {
		return DefaultPassScore
	}
	return r.Pass
}

func (r Rubric) validate() []error {
	var errs []error
	if strings.TrimSpace(r.Name) == "" {
		errs = append(errs, errors.New("expect.graded: name: a graded check is named by the requirement it stands for"))
	}
	if strings.TrimSpace(r.Rubric) == "" {
		errs = append(errs, fmt.Errorf("expect.graded: rubric: %q has nothing for the grader to judge by", r.Name))
	}
	if r.Pass < 0 || r.Pass > 1 {
		errs = append(errs, fmt.Errorf("expect.graded: pass: %q asks for %v, which is not a score between 0 and 1", r.Name, r.Pass))
	}
	return errs
}

// Grade is what a grader session answered.
type Grade struct {
	// Score is between 0 and 1: 1 is the rubric fully met.
	Score float64 `json:"score"`
	// Reasons is what the grader saw, in a sentence or two.
	Reasons string `json:"reasons"`
}

// graded runs one grader session per rubric the case declares and turns
// each answer into a Check carrying the score. A grader that cannot answer
// fails its own check and leaves the rest of the grading alone: the
// mechanical checks stand either way.
func (f *caseFactory) graded(ctx context.Context, c Case, grader review.Agent, dir string) ([]Check, costs) {
	var spent costs
	if len(c.Expect.Graded) == 0 {
		return nil, spent
	}
	transcript := f.transcript()
	end := f.endState(c)
	checks := make([]Check, 0, len(c.Expect.Graded))
	for _, r := range c.Expect.Graded {
		if grader == nil {
			checks = append(checks, Check{Name: r.Name,
				Failure: fmt.Sprintf("%s: nothing graded it, because the run has no grader agent", r.Name)})
			continue
		}
		checks = append(checks, f.runGrader(ctx, c, r, grader, dir, transcript, end, &spent))
	}
	return checks, spent
}

// runGrader runs one rubric's session and reads its answer.
func (f *caseFactory) runGrader(ctx context.Context, c Case, r Rubric, grader review.Agent, dir, transcript, end string, spent *costs) Check {
	check := Check{Name: r.Name}
	res, err := grader.Run(ctx, review.AgentRequest{Name: "grader", Prompt: gradePrompt(c, r, transcript, end), Dir: dir})
	if err != nil {
		check.Failure = fmt.Sprintf("%s: the grader session failed: %s", r.Name, err)
		return check
	}
	spent.add(costs{usd: res.CostUSD, turns: res.Turns, sessions: 1, unknown: boolToInt(!res.CostKnown)})
	g, err := parseGrade(res.Text)
	if err != nil {
		check.Failure = fmt.Sprintf("%s: the grader answered no score: %s", r.Name, err)
		return check
	}
	score := g.Score
	check.Score, check.Detail = &score, g.Reasons
	check.Pass = score >= r.PassScore()
	check.Failure = fmt.Sprintf("%s: scored %.2f, under %.2f", r.Name, score, r.PassScore())
	return check
}

// parseGrade reads a grader session's answer: the JSON object it was asked
// for, in a code fence or not. Nothing else in the text is read.
func parseGrade(text string) (Grade, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return Grade{}, errors.New("the answer holds no JSON object")
	}
	var g Grade
	if err := json.Unmarshal([]byte(text[start:end+1]), &g); err != nil {
		return Grade{}, err
	}
	if g.Score < 0 || g.Score > 1 {
		return Grade{}, fmt.Errorf("scored %v, which is not between 0 and 1", g.Score)
	}
	g.Reasons = strings.TrimSpace(g.Reasons)
	return g, nil
}

// graderPrompt is the whole of what a grader session is told.
const graderPrompt = "Score one session of a busybees factory role against a rubric.\n" + `
## The case

%s

## The rubric

%s

## What the %s session did

Its own event stream, oldest first:

` + "```" + `jsonl
%s
` + "```" + `

## What the run left behind

%s

## Your answer

Reply with one JSON object and nothing else:

{"score": 0.0, "reasons": "one or two sentences"}

The score is between 0 and 1: 1 is the rubric fully met, 0 is not met at
all. Judge what the rubric asks about and nothing else, from what is above.
Say what you saw, not what you would rather have seen.
`

func gradePrompt(c Case, r Rubric, transcript, end string) string {
	description := c.Description
	if strings.TrimSpace(description) == "" {
		description = "The case is called " + c.Name + " and describes itself no further."
	}
	return fmt.Sprintf(graderPrompt, description, strings.TrimSpace(r.Rubric), c.Role, transcript, end)
}

// transcript is what this run's sessions printed, oldest first. Only the
// role under eval runs, so every session directory holds one of its
// sessions, and the directories are named after the time they started.
func (f *caseFactory) transcript() string {
	dir := f.store.SessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		f.log.Warn("could not read the sessions", "dir", dir, "err", err)
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), agent.TranscriptFile))
		if err != nil {
			continue
		}
		b.Write(data)
	}
	if b.Len() == 0 {
		return "(the session left no transcript)"
	}
	return cut(b.String(), MaxTranscriptBytes)
}

// endState is what the run left behind, for a grader to read: every issue
// there is now with the comments posted on it, the pull requests opened,
// and the mail the role sent.
func (f *caseFactory) endState(c Case) string {
	snap := f.gh.Snapshot()
	var b strings.Builder
	b.WriteString("### Issues\n")
	for _, i := range snap.Issues {
		fmt.Fprintf(&b, "\n#%d %s [%s] %s\n\n%s\n", i.Number, i.Title, i.State, strings.Join(labelNames(i), ", "), cut(i.Body, MaxBodyBytes))
		for _, comment := range snap.Comments[i.Number] {
			fmt.Fprintf(&b, "\ncomment on #%d:\n\n%s\n", i.Number, cut(comment, MaxBodyBytes))
		}
	}
	if len(snap.PRs) > 0 {
		b.WriteString("\n### Pull requests\n")
		for _, p := range snap.PRs {
			fmt.Fprintf(&b, "\n#%d %s [%s] %s -> %s\n\n%s\n", p.Number, p.Title, p.State, p.HeadRefName, p.BaseRefName, cut(p.Body, MaxBodyBytes))
		}
	}
	fmt.Fprintf(&b, "\n### Mail the %s sent\n", c.Role)
	msgs, err := f.box.List(mail.Filter{From: c.Role})
	if err != nil {
		fmt.Fprintf(&b, "\nThe mailbox could not be read: %s\n", err)
		return b.String()
	}
	if len(msgs) == 0 {
		b.WriteString("\nNone.\n")
	}
	for _, m := range msgs {
		about := ""
		if n := ghwork.Issue(m.Work); n != 0 {
			about = fmt.Sprintf(" about #%d", n)
		}
		fmt.Fprintf(&b, "\nto the %s%s: %s\n\n%s\n", m.To, about, m.Subject, cut(m.Body, MaxBodyBytes))
	}
	return b.String()
}

// cut shortens s to at most n bytes, keeping its beginning and its end and
// saying where the middle went.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "\n... (cut here) ...\n"
	half := (n - len(marker)) / 2
	if half <= 0 {
		return s[:n]
	}
	return s[:half] + marker + s[len(s)-half:]
}

// boolToInt counts a flag: 1 when it is set.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
