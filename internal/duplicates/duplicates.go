// Package duplicates finds the existing issues a new one would duplicate.
//
// It is a deterministic text check, not a judgement: Find lists every issue
// in the repository, open and closed, scores each against the title and body
// about to be filed, and returns the ones that clear a fixed threshold, best
// first. The caller decides what a match means — comment on it, refuse to
// file, or hand the candidates to the agent. Its callers are QA's file_bug
// tool (internal/mcpserver), which refuses a bug the repository already
// reports, and the scheduler's factory-error loop (drainFeedbackQueue),
// which files reports into the busybees repository. Issue creation itself
// (internal/issues.Create, the issue_create tool) does not go through it: the
// roles that split one issue into several similar ones — a triage split, a
// feature broken into work items — would trip it on purpose.
//
// The scoring is local so that it is the same in a test as in production and
// owes nothing to gh's search ranking. Titles and bodies are lowercased,
// split on anything that is not a letter or a digit, stripped of a few
// function words, and compared as sets with the Sørensen–Dice coefficient.
// The title weighs more than the body: it is what a person writes to name
// the defect, while bodies carry logs and reproduction steps that overlap by
// accident.
package duplicates

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/kpenfound/busybees/internal/github"
)

// Threshold is the score a candidate must reach to be returned by Find and
// Rank. Scores run from 0 (no word in common) to 1 (the same words).
const Threshold = 0.5

// The weights of the title and the body similarities in the score, when
// both issues have a body. They sum to 1, so an identical title with an
// unrelated body scores titleWeight and clears the threshold on its own,
// while an identical body under an unrelated title does not.
const (
	titleWeight = 0.7
	bodyWeight  = 0.3
)

// stopWords are dropped before comparing: they say nothing about what an
// issue is about, and two long titles would otherwise look alike for sharing
// "the", "a" and "when".
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "its": true, "not": true, "of": true, "on": true, "or": true,
	"that": true, "the": true, "this": true, "to": true, "when": true,
	"with": true,
}

// Match is an existing issue that looks like the one about to be filed.
type Match struct {
	Number int
	Title  string
	// State is the issue's state as gh reports it: OPEN or CLOSED. A closed
	// match is a duplicate too — of something already fixed, or already
	// declined — and the caller decides whether that changes anything.
	State string
	// Score is the similarity, Threshold or more. See Score.
	Score float64
}

// Find returns the issues in gh's repository that look like an issue with
// this title and body, best match first, or nothing when no issue reaches
// Threshold. It reads every issue, open and closed, whatever labels it
// carries: see github.Client.ListAllIssues for why the factory's filter does
// not apply here.
func Find(ctx context.Context, gh *github.Client, title, body string) ([]Match, error) {
	issues, err := gh.ListAllIssues(ctx)
	if err != nil {
		return nil, err
	}
	return Rank(issues, title, body), nil
}

// Rank is Find over a list already in hand: the issues scoring Threshold or
// more against title and body, best first, ties broken towards the newer
// issue.
func Rank(issues []github.Issue, title, body string) []Match {
	var out []Match
	for _, i := range issues {
		if s := Score(title, body, i.Title, i.Body); s >= Threshold {
			out = append(out, Match{Number: i.Number, Title: i.Title, State: i.State, Score: s})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Number > out[b].Number
	})
	return out
}

// Score is the similarity of two issues, between 0 and 1: the Dice
// coefficient of their title words, and when both have a body, that of their
// body words too, weighted titleWeight to bodyWeight. An issue without a body
// is compared on its title alone, so a one-line bug report can still match a
// detailed one — and, its body being unknown rather than different, it can
// rank above an issue whose body merely resembles the new one's.
func Score(title, body, otherTitle, otherBody string) float64 {
	t := dice(tokens(title), tokens(otherTitle))
	b1, b2 := tokens(body), tokens(otherBody)
	if len(b1) == 0 || len(b2) == 0 {
		return t
	}
	return titleWeight*t + bodyWeight*dice(b1, b2)
}

// tokens is the set of words in s: lowercased, split on anything that is not
// a letter or a digit, stop words dropped.
func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// dice is the Sørensen–Dice coefficient of two sets: twice the size of
// their intersection over the sum of their sizes. Two empty sets share
// nothing, not everything.
func dice(a, b map[string]bool) float64 {
	if len(a)+len(b) == 0 {
		return 0
	}
	common := 0
	for w := range a {
		if b[w] {
			common++
		}
	}
	return 2 * float64(common) / float64(len(a)+len(b))
}
