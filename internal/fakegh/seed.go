package fakegh

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

// Seed is a repository's state as data, for Load.
type Seed struct {
	Issues     []SeedIssue
	PRs        []SeedPR
	Milestones []github.Milestone
	// Labels are the label names that exist in the repository.
	Labels []string
}

// SeedIssue is one issue: its number, title, body, state, labels, milestone,
// author and comments as github.Issue holds them, and the feature it is a
// sub-issue of. An empty State is OPEN.
type SeedIssue struct {
	github.Issue
	Parent int
}

// SeedPR is one pull request, with its conversation comments and its
// reviews. An empty State is OPEN.
type SeedPR struct {
	github.PR
	Comments []github.Comment
	Reviews  []SeedReview
}

// SeedReview is one review a person or a bee left on a pull request.
type SeedReview struct {
	Author string
	// State is APPROVED, CHANGES_REQUESTED or COMMENTED.
	State string
	Body  string
	At    time.Time
}

// Load adds s to the state, replacing any issue or pull request with the
// same number. Comments are served as issue view's comments and from the
// REST comments endpoint, reviews from the REST reviews endpoint.
func (f *GitHub) Load(s Seed) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, si := range s.Issues {
		i := si.Issue
		if i.Number <= 0 {
			return fmt.Errorf("fakegh: seeded issue %q has no number", i.Title)
		}
		if i.State == "" {
			i.State = "OPEN"
		}
		i.Labels = slices.Clone(i.Labels)
		i.Comments = slices.Clone(i.Comments)
		f.Issues[i.Number] = &i
		if si.Parent != 0 {
			f.Parents[i.Number] = si.Parent
		}
		if err := f.seedComments(i.Number, i.Comments); err != nil {
			return err
		}
	}
	for _, sp := range s.PRs {
		p := sp.PR
		if p.Number <= 0 {
			return fmt.Errorf("fakegh: seeded pull request %q has no number", p.Title)
		}
		if p.State == "" {
			p.State = "OPEN"
		}
		p.Labels = slices.Clone(p.Labels)
		f.PRs[p.Number] = &p
		if err := f.seedComments(p.Number, sp.Comments); err != nil {
			return err
		}
		if len(sp.Reviews) > 0 {
			type review struct {
				ID          int64         `json:"id"`
				User        github.Author `json:"user"`
				Body        string        `json:"body"`
				State       string        `json:"state"`
				SubmittedAt time.Time     `json:"submitted_at"`
			}
			var out []review
			for _, r := range sp.Reviews {
				f.lastID++
				out = append(out, review{ID: f.lastID, User: github.Author{Login: r.Author}, Body: r.Body, State: r.State, SubmittedAt: r.At})
			}
			b, err := json.Marshal(out)
			if err != nil {
				return err
			}
			f.Activity[fmt.Sprintf("repos/%s/pulls/%d/reviews", f.Repo, p.Number)] = string(b)
		}
	}
	f.Milestones = append(f.Milestones, s.Milestones...)
	for _, l := range s.Labels {
		if !slices.Contains(f.Labels, l) {
			f.Labels = append(f.Labels, l)
		}
	}
	return nil
}

// seedComments serves comments from the REST comments endpoint of n.
// Called with f.mu held.
func (f *GitHub) seedComments(n int, comments []github.Comment) error {
	if len(comments) == 0 {
		return nil
	}
	type comment struct {
		ID        int64         `json:"id"`
		User      github.Author `json:"user"`
		Body      string        `json:"body"`
		HTMLURL   string        `json:"html_url"`
		CreatedAt time.Time     `json:"created_at"`
	}
	var out []comment
	for _, c := range comments {
		f.lastID++
		out = append(out, comment{ID: f.lastID, User: c.Author, Body: c.Body,
			HTMLURL:   fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", f.Repo, n, f.lastID),
			CreatedAt: c.CreatedAt})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	f.Activity[fmt.Sprintf("repos/%s/issues/%d/comments", f.Repo, n)] = string(b)
	return nil
}

// Snapshot is a copy of the state, for reading back what a run did.
type Snapshot struct {
	// Issues and PRs are ordered by number.
	Issues []github.Issue
	PRs    []github.PR
	// Comments are the bodies posted on each number through Exec.
	Comments map[int][]string
	// History lists the label additions per number, in order.
	History map[int][]string
	Merged  []int
	Labels  []string
	// Calls is how many gh invocations Exec answered.
	Calls int
}

// Snapshot copies the state as it is now.
func (f *GitHub) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyEdits()
	s := Snapshot{Comments: map[int][]string{}, History: map[int][]string{},
		Merged: slices.Clone(f.Merged), Labels: slices.Clone(f.Labels), Calls: len(f.Calls)}
	for _, i := range f.Issues {
		c := *i
		c.Labels = slices.Clone(i.Labels)
		c.Assignees = slices.Clone(i.Assignees)
		c.Comments = slices.Clone(i.Comments)
		s.Issues = append(s.Issues, c)
	}
	sort.Slice(s.Issues, func(a, b int) bool { return s.Issues[a].Number < s.Issues[b].Number })
	for _, p := range f.PRs {
		c := *p
		c.Labels = slices.Clone(p.Labels)
		c.Assignees = slices.Clone(p.Assignees)
		s.PRs = append(s.PRs, c)
	}
	sort.Slice(s.PRs, func(a, b int) bool { return s.PRs[a].Number < s.PRs[b].Number })
	for n, c := range f.Comments {
		s.Comments[n] = slices.Clone(c)
	}
	for n, h := range f.History {
		s.History[n] = slices.Clone(h)
	}
	return s
}

// Issue returns issue n from the snapshot.
func (s Snapshot) Issue(n int) (github.Issue, bool) {
	for _, i := range s.Issues {
		if i.Number == n {
			return i, true
		}
	}
	return github.Issue{}, false
}

// PR returns pull request n from the snapshot.
func (s Snapshot) PR(n int) (github.PR, bool) {
	for _, p := range s.PRs {
		if p.Number == n {
			return p, true
		}
	}
	return github.PR{}, false
}
