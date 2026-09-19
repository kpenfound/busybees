package fakegh

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

const repo = "acme/widgets"

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// seeded is a fake holding a feature, a work item under it with a person's
// comment, and an open pull request with a comment and a review, behind a
// client whose Exec is the fake's.
func seeded(t *testing.T) (*GitHub, *github.Client) {
	t.Helper()
	f := New(repo)
	f.Login = "bees-bot"
	f.Now = func() time.Time { return t0 }
	err := f.Load(Seed{
		Labels:     []string{"bees", "bees:ready", "bees:review"},
		Milestones: []github.Milestone{{Number: 3, Title: "v1"}},
		Issues: []SeedIssue{
			{Issue: github.Issue{Number: 5, Title: "Exports", Labels: []github.Label{{Name: "bees:feature"}}}},
			{Issue: github.Issue{Number: 7, Title: "CSV export", Body: "please",
				Labels:    []github.Label{{Name: "bees"}, {Name: "bees:ready"}},
				Milestone: &github.MilestoneRef{Title: "v1"},
				Author:    github.Author{Login: "kyle"},
				Comments:  []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "use commas", CreatedAt: t0}},
			}, Parent: 5},
		},
		PRs: []SeedPR{{
			PR:       github.PR{Number: 8, Title: "Add a thing", HeadRefName: "feature", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}},
			Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "looks close", CreatedAt: t0}},
			Reviews:  []SeedReview{{Author: "kyle", State: "CHANGES_REQUESTED", Body: "rename it", At: t0}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := github.New(repo)
	c.Exec = f.Exec
	return f, c
}

func TestSeedReadsBackThroughTheClient(t *testing.T) {
	_, c := seeded(t)
	ctx := context.Background()

	issues, err := c.ListOpenIssues(ctx, github.Query{Label: "bees"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Number != 7 || issues[0].State != "OPEN" || issues[0].MilestoneTitle() != "v1" {
		t.Fatalf("open bees issues: %+v", issues)
	}
	i, err := c.GetIssue(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(i.Comments) != 1 || i.Comments[0].Body != "use commas" {
		t.Fatalf("issue comments: %+v", i.Comments)
	}
	comments, err := c.CommentsSince(ctx, 7, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Author != "kyle" || comments[0].Body != "use commas" {
		t.Fatalf("REST comments: %+v", comments)
	}
	parent, err := c.ParentIssue(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if parent == nil || parent.Number != 5 || parent.Title != "Exports" {
		t.Fatalf("parent: %+v", parent)
	}
	children, err := c.ListSubIssues(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Number != 7 {
		t.Fatalf("sub-issues of 5: %+v", children)
	}

	p, err := c.GetPR(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if p.HeadRefName != "feature" || p.State != "OPEN" {
		t.Fatalf("pr: %+v", p)
	}
	prComments, err := c.CommentsSince(ctx, 8, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(prComments) != 1 || prComments[0].Body != "looks close" {
		t.Fatalf("pr comments: %+v", prComments)
	}
	reviews, err := c.ReviewsSince(ctx, 8, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || reviews[0].State != "CHANGES_REQUESTED" || reviews[0].Author != "kyle" {
		t.Fatalf("reviews: %+v", reviews)
	}
	if prComments[0].ID == comments[0].ID || reviews[0].ID == prComments[0].ID {
		t.Fatalf("seeded ids collide: %d %d %d", comments[0].ID, prComments[0].ID, reviews[0].ID)
	}
	ms, err := c.ListMilestones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Title != "v1" {
		t.Fatalf("milestones: %+v", ms)
	}
}

func TestExecWritesShowInTheSnapshot(t *testing.T) {
	f, c := seeded(t)
	ctx := context.Background()

	if err := c.EditLabels(ctx, 7, []string{"bees:review"}, []string{"bees:ready"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Comment(ctx, 7, "on it"); err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("Closes #7"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := f.Exec(ctx, "pr", "create", "-R", repo, "--base", "main", "--head", "bees/issue-7",
		"--label", "bees", "--title", "CSV export", "--body-file", body)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "https://github.com/acme/widgets/pull/9" {
		t.Fatalf("pr create answered %q", got)
	}
	if _, err := f.Exec(ctx, "pr", "create", "-R", repo, "--base", "main", "--head", "bees/issue-7", "--title", "again"); err == nil {
		t.Fatal("a second open pull request for the same head was created")
	}
	if _, err := f.Exec(ctx, "pr", "comment", "9", "-R", repo, "--body", "ready"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Exec(ctx, "issue", "close", "5", "-R", repo, "--comment", "done"); err != nil {
		t.Fatal(err)
	}

	s := f.Snapshot()
	i, ok := s.Issue(7)
	if !ok {
		t.Fatal("issue 7 gone")
	}
	if names := labelNames(i.Labels); !slices.Equal(names, []string{"bees", "bees:review"}) {
		t.Fatalf("issue 7 labels: %v", names)
	}
	if !slices.Equal(s.History[7], []string{"bees:review"}) {
		t.Fatalf("issue 7 label history: %v", s.History[7])
	}
	if !slices.Equal(s.Comments[7], []string{"on it"}) || !slices.Equal(s.Comments[9], []string{"ready"}) || !slices.Equal(s.Comments[5], []string{"done"}) {
		t.Fatalf("comments: %v", s.Comments)
	}
	feature, _ := s.Issue(5)
	if feature.State != "CLOSED" || feature.ClosedAt == nil || !feature.ClosedAt.Equal(t0) {
		t.Fatalf("issue 5 after close: %+v", feature)
	}
	p, ok := s.PR(9)
	if !ok {
		t.Fatalf("pull request 9 missing: %+v", s.PRs)
	}
	if p.State != "OPEN" || p.HeadRefName != "bees/issue-7" || p.BaseRefName != "main" || p.Body != "Closes #7" ||
		p.Author.Login != "bees-bot" || !slices.Equal(labelNames(p.Labels), []string{"bees"}) {
		t.Fatalf("pull request 9: %+v", p)
	}
	if found, err := c.FindPRForBranch(ctx, "bees/issue-7"); err != nil || found == nil || found.Number != 9 {
		t.Fatalf("FindPRForBranch: %+v, %v", found, err)
	}

	// The snapshot is a copy: changing it changes nothing in the fake.
	s.Issues[0].Labels[0].Name = "changed"
	if again, _ := f.Snapshot().Issue(s.Issues[0].Number); again.Labels[0].Name == "changed" {
		t.Fatal("the snapshot shares labels with the fake")
	}
	if f.CallCount("pr create") != 2 || f.Total() != f.Snapshot().Calls {
		t.Fatalf("calls: pr create %d, total %d", f.CallCount("pr create"), f.Total())
	}
}

func TestRequestEditAppliesOnTheNextCall(t *testing.T) {
	f, c := seeded(t)
	f.EditsDir = filepath.Join(t.TempDir(), "gh-edits")
	if err := RequestEdit(f.EditsDir, Edit{Number: 7, Add: []string{"bees:review"}, Remove: []string{"bees:ready"}}); err != nil {
		t.Fatal(err)
	}
	if err := RequestEdit(f.EditsDir, Edit{Number: 8, Review: "APPROVED"}); err != nil {
		t.Fatal(err)
	}
	i, err := c.GetIssue(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if names := labelNames(i.Labels); !slices.Equal(names, []string{"bees", "bees:review"}) {
		t.Fatalf("labels after the edit: %v", names)
	}
	reviews, err := c.ReviewsSince(context.Background(), 8, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 2 || reviews[1].State != "APPROVED" || reviews[1].Author != "bees-bot" {
		t.Fatalf("reviews after the edit: %+v", reviews)
	}
	if entries, _ := os.ReadDir(f.EditsDir); len(entries) != 0 {
		t.Fatalf("edits left behind: %v", entries)
	}
}

func TestErrForFailsTheCommand(t *testing.T) {
	f, c := seeded(t)
	f.ErrFor["issue edit"] = os.ErrPermission
	if err := c.EditLabels(context.Background(), 7, []string{"bees:review"}, nil); err == nil {
		t.Fatal("issue edit succeeded under ErrFor")
	}
	if _, err := f.Exec(context.Background(), "repo", "delete"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("an unknown command answered %v", err)
	}
}

func labelNames(labels []github.Label) []string {
	var out []string
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}
