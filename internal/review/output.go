package review

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/text"
)

// The end of a review. Once triage has selected what goes into the output
// (Queue.Selected), one of five things happens to it, and Config.Output or
// the command line says which (the output modes in config.go):
//
//	approve   the selected findings are posted as review comments and the
//	          pull request is approved
//	comment   the same comments, as a comment-only review
//	reject    the same comments, as a request-changes review
//	report    the selected findings are printed as a markdown report, and
//	          nothing is posted
//	discard   nothing is posted and nothing is printed
//
// The three that post go through Post: one review, with every comment in
// it, submitted in one call (github.Client.PostReview), never one comment
// at a time. A finding is posted as a comment on its lines when the pull
// request's diff has them (Anchors); one the diff does not have, and one
// about the change as a whole, is folded into the review's summary
// instead, so a line the angle got wrong cannot make GitHub refuse the
// whole review. A suggestion goes with its comment as a suggestion block
// when the comment is on the new side of the diff, which is the only place
// GitHub can apply one.
//
// Report renders the same selections as markdown for a person to paste
// wherever they like.

// Events are the review events GitHub takes for the three modes that post.
var events = map[string]string{
	OutputApprove: "APPROVE",
	OutputComment: "COMMENT",
	OutputReject:  "REQUEST_CHANGES",
}

// Posts reports whether an output mode submits a review.
func Posts(mode string) bool {
	_, ok := events[mode]
	return ok
}

// diffFileStart matches the line that starts one file in a unified diff.
var diffFileStart = regexp.MustCompile(`^diff --git a/(.*) b/(.*)$`)

// hunkStart matches a hunk header and captures the first line of each side.
var hunkStart = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Anchors is the set of lines a review comment can be anchored to: every
// line of every hunk in a pull request's diff, on both sides, each with the
// hunk it is in. GitHub takes a comment on any of them and refuses one on
// any other line, and takes a range only inside one hunk.
type Anchors struct {
	hunks map[anchor]int
}

// anchor is one line of one side of one file in the diff.
type anchor struct {
	path string
	side string
	line int
}

// ParseAnchors reads the anchors out of a unified diff, as `gh pr diff`
// prints it. A file's lines are under its new path, or its old one when
// the change deleted it, which is where GitHub shows them.
func ParseAnchors(diff string) *Anchors {
	a := &Anchors{hunks: map[anchor]int{}}
	var path string
	var oldLine, newLine, hunk int
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		if m := diffFileStart.FindStringSubmatch(line); m != nil {
			path, inHunk = m[2], false
			continue
		}
		if m := hunkStart.FindStringSubmatch(line); m != nil {
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			hunk++
			inHunk = true
			continue
		}
		if !inHunk || path == "" {
			continue
		}
		// A file's header lines (--- and +++) come before its first
		// hunk, after the diff --git line that ends the hunk before, so
		// inside a hunk a line starting with --- or +++ is a removed or
		// added line whose text starts with two dashes or two pluses.
		switch {
		case strings.HasPrefix(line, "+"):
			a.hunks[anchor{path, SideNew, newLine}] = hunk
			newLine++
		case strings.HasPrefix(line, "-"):
			a.hunks[anchor{path, SideOld, oldLine}] = hunk
			oldLine++
		case strings.HasPrefix(line, " "):
			a.hunks[anchor{path, SideNew, newLine}] = hunk
			a.hunks[anchor{path, SideOld, oldLine}] = hunk
			newLine++
			oldLine++
		}
	}
	return a
}

// Has reports whether a comment can be anchored where the finding points:
// a file, lines, and every one of the lines in one hunk of the diff on the
// finding's side. A finding with no lines has line 0, which no hunk has.
func (a *Anchors) Has(f *Finding) bool {
	if a == nil || !f.Anchored() {
		return false
	}
	want, ok := a.hunks[anchor{f.File, f.Side, f.Lines.Start}]
	if !ok {
		return false
	}
	for line := f.Lines.Start + 1; line <= f.Lines.End; line++ {
		if hunk, ok := a.hunks[anchor{f.File, f.Side, line}]; !ok || hunk != want {
			return false
		}
	}
	return true
}

// Compose is the review to post for an output mode that posts: each
// selected finding the diff has the lines of as a comment on them, the
// rest folded into the body. A comment-only or request-changes review
// with nothing in it is refused: GitHub takes neither without a body, and
// an approval with nothing selected is an approval with nothing to say.
func Compose(mode string, selected []Selection, anchors *Anchors) (*github.ReviewRequest, error) {
	event, ok := events[mode]
	if !ok {
		return nil, fmt.Errorf("output mode %q posts nothing", mode)
	}
	req := &github.ReviewRequest{Event: event}
	var folded []Selection
	for _, s := range selected {
		f := s.Finding
		if !anchors.Has(&f) {
			folded = append(folded, s)
			continue
		}
		side := "RIGHT"
		if f.Side == SideOld {
			side = "LEFT"
		}
		c := github.ReviewComment{Path: f.File, Line: f.Lines.End, Side: side, Body: renderSelection(s, f.Side == SideNew, false)}
		if f.Lines.Start != f.Lines.End {
			c.StartLine, c.StartSide = f.Lines.Start, side
		}
		req.Comments = append(req.Comments, c)
	}
	req.Body = renderSelections(folded, true)
	if mode != OutputApprove && req.Body == "" {
		if err := emptyReview(mode, selected); err != nil {
			return nil, err
		}
		req.Body = text.Count(len(req.Comments), "comment") + " inline."
	}
	return req, nil
}

// emptyReview is the refusal for a review GitHub would not take: a
// comment-only or request-changes review with nothing selected. An
// approval with nothing selected is not one.
func emptyReview(mode string, selected []Selection) error {
	if mode == OutputApprove || len(selected) > 0 {
		return nil
	}
	return refuse("nothing was selected, and a %s review with nothing in it cannot be posted", modeNames[mode])
}

// Verb says what a mode that posts did, once it has: "approved",
// "commented" or "requested changes".
func Verb(mode string) string { return verbs[mode] }

// verbs are the three posting modes in the past tense.
var verbs = map[string]string{
	OutputApprove: "approved",
	OutputComment: "commented",
	OutputReject:  "requested changes",
}

// modeNames are the output modes as a sentence names them.
var modeNames = map[string]string{
	OutputApprove: "approving",
	OutputComment: "comment-only",
	OutputReject:  "request-changes",
	OutputReport:  "report",
	OutputDiscard: "discard",
}

// Post submits the review one of the three posting modes asks for, in one
// call: the pull request is read for the commit the comments are anchored
// against and the diff they are anchored in, both as they are now. It
// returns what was posted. A mode that posts nothing, and a review
// GitHub would not take (Compose), are errors and nothing is sent.
func Post(ctx context.Context, client *github.Client, ref Ref, mode string, selected []Selection) (*github.ReviewRequest, error) {
	if !Posts(mode) {
		return nil, fmt.Errorf("output mode %q posts nothing", mode)
	}
	if err := emptyReview(mode, selected); err != nil {
		// Refused before anything is read: the person can answer it
		// without a round trip.
		return nil, err
	}
	pr, err := client.GetPR(ctx, ref.Number)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ref, err)
	}
	diff, err := client.PRDiff(ctx, ref.Number)
	if err != nil {
		return nil, fmt.Errorf("read the diff of %s: %w", ref, err)
	}
	req, err := Compose(mode, selected, ParseAnchors(diff))
	if err != nil {
		return nil, err
	}
	req.CommitID = pr.HeadSHA
	if err := client.PostReview(ctx, ref.Number, *req); err != nil {
		return nil, err
	}
	return req, nil
}

// Report renders the selected findings as a markdown report: the pull
// request, then every selection with where it points, its text and its
// suggestion, most severe first as triage ordered them. It is what the
// report mode prints, and it ends with a newline.
func Report(brief *Brief, selected []Selection) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Review of %s", brief.Ref)
	if brief.Title != "" {
		fmt.Fprintf(&out, ": %s", brief.Title)
	}
	fmt.Fprintf(&out, "\n\n%s\n", brief.Ref.URL())
	if len(selected) == 0 {
		out.WriteString("\nNothing was selected.\n")
		return out.String()
	}
	out.WriteString("\n" + renderSelections(selected, true) + "\n")
	return out.String()
}

// renderSelections renders selections one after another, separated by a
// rule, and "" for none.
func renderSelections(selected []Selection, located bool) string {
	parts := make([]string, 0, len(selected))
	for _, s := range selected {
		parts = append(parts, renderSelection(s, false, located))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// renderSelection is one selection as markdown: where it points and how
// severe it is when located is set (a comment on the lines themselves needs
// neither), the comment text, and the suggestion after it as a suggestion
// block when GitHub can apply it there and as a code block otherwise.
func renderSelection(s Selection, applicable, located bool) string {
	f := s.Finding
	var out strings.Builder
	if located {
		if f.Anchored() {
			where := f.File
			if !f.Lines.IsZero() {
				where += ":" + f.Lines.String()
			}
			if f.Side == SideOld {
				where += " (removed)"
			}
			fmt.Fprintf(&out, "`%s` · ", where)
		}
		fmt.Fprintf(&out, "%s · %s\n\n", f.Severity, f.Category)
	}
	out.WriteString(s.Comment)
	if f.Suggestion != "" {
		lang := ""
		if applicable {
			lang = "suggestion"
		}
		out.WriteString("\n\n" + fencedAs(f.Suggestion, lang))
	}
	return out.String()
}

// fencedAs is fenced with a language after the opening fence.
func fencedAs(content, lang string) string {
	block := fenced(content)
	fence, rest, _ := strings.Cut(block, "\n")
	return fence + lang + "\n" + rest
}
