package review

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/text"
)

// Triage at a terminal. Console walks the queue one finding at a time,
// prints each one whole, and reads what to do with it from a line of input:
// one of the four actions (triage.go), the select action with the comment
// text edited first, the finding left for later, or the triage stopped.
// Every decision is in the artifact before the next finding is shown, so
// stopping loses nothing and running the console again picks up the
// findings still undecided.
//
// It reads lines and writes lines, and nothing else: no screen, no colour,
// no key that is not followed by return. That is what makes it drivable from
// a pipe, in a test, and by anything that is not a person.

// The keys the console takes, each followed by return. A whole word is taken
// too.
const (
	keySelect  = "s"
	keyEdit    = "e"
	keyDismiss = "d"
	keyDefer   = "f"
	keyAsk     = "a"
	keyNext    = "n"
	keyQuit    = "q"
	keyHelp    = "?"
)

// Console drives a Queue from an input and an output.
type Console struct {
	// In is where the keys and the lines they ask for are read from, and
	// Out where the findings and the prompts are written.
	In  io.Reader
	Out io.Writer
	// Editor opens a comment text for editing and returns what it was
	// edited to, for the key that edits a finding's text before selecting
	// it. With no editor that key is not offered.
	Editor func(text string) (string, error)

	// out is Out for one Run, remembering the first write that failed.
	out *output
}

// output is the console's writer for one run: it remembers the first error
// writing to it and drops what comes after, so that a terminal that went
// away stops triage instead of being asked for more keys.
type output struct {
	w   io.Writer
	err error
}

func (o *output) Write(p []byte) (int, error) {
	if o.err != nil {
		return len(p), nil
	}
	n, err := o.w.Write(p)
	if err != nil {
		o.err = err
	}
	return n, err
}

// printf writes to the console's output. What failed is out.err.
func (c *Console) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.out, format, args...)
}

// Run triages q until nothing is left undecided, the input ends, or the
// quit key is pressed, and prints where triage stands. An error is one that
// stopped triage: an action that could not be written, an input that could
// not be read, an output that could not be written. What a single action
// failed with (a session that could not be reopened, a dismissal with no
// reason) is printed, and the finding is shown again.
func (c *Console) Run(ctx context.Context, q *Queue) error {
	c.out = &output{w: c.Out}
	in := bufio.NewScanner(c.In)
	in.Buffer(make([]byte, 0, 64*1024), 1<<20)
	skipped := map[string]bool{}
	for c.out.err == nil {
		f, ok := c.next(q, skipped)
		if !ok {
			break
		}
		c.show(q, f)
		key, ok := c.read(in, c.keys())
		if !ok || c.out.err != nil {
			break
		}
		stop, err := c.act(ctx, q, in, f, key, skipped)
		if err != nil {
			return err
		}
		if stop {
			break
		}
	}
	c.summary(q)
	return errors.Join(in.Err(), c.out.err)
}

// next is the first pending finding not passed over this run, and false
// when there is none.
func (c *Console) next(q *Queue, skipped map[string]bool) (Finding, bool) {
	for _, f := range q.Pending() {
		if !skipped[f.ID] {
			return f, true
		}
	}
	return Finding{}, false
}

// act takes one key on a finding. It reports whether triage stops.
func (c *Console) act(ctx context.Context, q *Queue, in *bufio.Scanner, f Finding, key string, skipped map[string]bool) (bool, error) {
	switch key {
	case keySelect, ActionSelect:
		return false, c.report(q.Select(f.ID, ""))
	case keyEdit, "edit":
		if c.Editor == nil {
			c.printf("%s\n", "no editor: set $VISUAL or $EDITOR to edit the comment text")
			return false, nil
		}
		edited, err := c.Editor(f.Comment())
		if err != nil {
			c.printf("the editor failed: %v\n", err)
			return false, nil
		}
		if strings.TrimSpace(edited) == "" {
			c.printf("%s\n", "the comment text is empty, so the finding stays undecided")
			return false, nil
		}
		return false, c.report(q.Select(f.ID, edited))
	case keyDismiss, ActionDismiss:
		reason, ok := c.read(in, "reason (recorded in your reviewer notes): ")
		if !ok {
			return true, nil
		}
		return false, c.report(q.Dismiss(f.ID, reason))
	case keyDefer, ActionDefer:
		return false, c.report(q.Defer(f.ID))
	case keyAsk, ActionAsk:
		question, ok := c.read(in, fmt.Sprintf("question for the %s angle: ", f.Angle))
		if !ok {
			return true, nil
		}
		answer, err := q.Ask(ctx, f.ID, question)
		if err != nil {
			c.printf("ask failed: %v\n", err)
			return false, nil
		}
		c.printf("\n%s\n", answer.Text)
		if len(answer.Added) > 0 {
			c.printf("\nthe answer added %s to the queue:\n", text.Count(len(answer.Added), "finding"))
			for _, a := range answer.Added {
				c.printf("  %s  %s  %s\n", a.ID, a.Severity, a.Title)
			}
		}
		c.printf("\n")
	case keyNext, "next":
		skipped[f.ID] = true
	case keyQuit, "quit":
		return true, nil
	case keyHelp, "help":
		c.printf("%s\n", c.help())
	default:
		c.printf("%q is not a key\n%s\n", key, c.help())
	}
	return false, nil
}

// report prints what an action failed with, and returns the errors that
// stop triage: everything but a refusal the person can answer.
func (c *Console) report(err error) error {
	if err == nil {
		return nil
	}
	c.printf("%v\n", err)
	return nil
}

// read prints a prompt and reads one line, trimmed. It reports false when
// the input has ended.
func (c *Console) read(in *bufio.Scanner, prompt string) (string, bool) {
	c.printf("%s", prompt)
	if !in.Scan() {
		c.printf("\n")
		return "", false
	}
	return strings.TrimSpace(in.Text()), true
}

// keys is the prompt under a finding.
func (c *Console) keys() string {
	edit := ""
	if c.Editor != nil {
		edit = "e edit and select · "
	}
	return fmt.Sprintf("s select · %sd dismiss · f defer · a ask · n next · q quit · ? help\n> ", edit)
}

// help says what each key does.
func (c *Console) help() string {
	lines := []string{
		"  s  select: the finding goes into the review's output as written",
	}
	if c.Editor != nil {
		lines = append(lines, "  e  edit the comment text in your editor, then select")
	}
	return strings.Join(append(lines,
		"  d  dismiss: leave it out, and record why in your reviewer notes",
		"  f  defer: leave it out of this review, and record nothing",
		"  a  ask the angle that found it a question; a finding the answer turns up joins the queue",
		"  n  next: leave it undecided for now",
		"  q  quit: what has been decided is kept, and the rest is still here next time",
	), "\n")
}

// show prints one finding whole, and what has been asked about it before.
func (c *Console) show(q *Queue, f Finding) {
	pending := q.Pending()
	at := slices.IndexFunc(pending, func(p Finding) bool { return p.ID == f.ID })
	c.printf("\n[%d of %s undecided] %s · %s · %s · %s\n", at+1, text.Count(len(pending), "finding"), f.ID, f.Severity, f.Angle, f.Category)
	if f.Anchored() {
		where := f.File
		if !f.Lines.IsZero() {
			where += ":" + f.Lines.String()
		}
		if f.Side == SideOld {
			where += " (removed)"
		}
		c.printf("%s\n", where)
	}
	c.printf("\n%s\n", f.Comment())
	if f.Suggestion != "" {
		c.printf("\nSuggestion:\n%s\n", indent(f.Suggestion))
	}
	if f.Evidence != "" {
		c.printf("\nEvidence: %s\n", f.Evidence)
	}
	if len(f.Sources) > 0 {
		c.printf("Sources: %s\n", strings.Join(f.Sources, ", "))
	}
	if len(f.AlsoFrom) > 0 {
		c.printf("Also from: %s\n", strings.Join(f.AlsoFrom, ", "))
	}
	for _, d := range q.Asked(f.ID) {
		c.printf("\nQ: %s\nA: %s\n", d.Question, d.Answer)
	}
	c.printf("\n")
}

// summary prints where triage stands: how many findings each decision in
// force covers, and how many are still undecided.
func (c *Console) summary(q *Queue) {
	counts := map[string]int{}
	for _, f := range q.Findings() {
		if d, ok := q.DecisionOn(f.ID); ok {
			counts[d.Action]++
		} else {
			counts["undecided"]++
		}
	}
	var parts []string
	for _, k := range []struct{ action, past string }{{ActionSelect, "selected"}, {ActionDismiss, "dismissed"}, {ActionDefer, "deferred"}, {"undecided", "undecided"}} {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k.action], k.past))
	}
	c.printf("%s of %s\n", strings.Join(parts, ", "), text.Count(len(q.Findings()), "finding"))
}

// indent puts two spaces before every line of s.
func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}
