package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

// filter.creator never hides the factory's own work. The issues and pull
// requests the factory opens are authored by the account it acts as, which
// cannot be made to match the criterion the way an assignee or a milestone
// can, so the poll lists that account's items alongside the creator's: a
// factory acting as a bot under creator = "kyle" sees what kyle opened and
// what it opened itself, and nothing a third account opened.
func TestFilterCreatorKeepsTheFactoryOwnItemsVisible(t *testing.T) {
	h := newHarness(t, devOnlyTOML+"\n[filter]\ncreator = \"kyle\"\n")
	h.sched.query.Self = "bot" // what cmd/bees resolves from github.login
	seed := func(n int, author string) {
		h.gh.issues[n] = &github.Issue{Number: n, Title: fmt.Sprintf("Issue %d", n), State: "OPEN",
			Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}},
			Author: github.Author{Login: author}, CreatedAt: time.Now().Add(-time.Hour)}
	}
	seed(1, "kyle")
	seed(2, "bot")
	seed(3, "someone-else")
	h.gh.prs[201] = &github.PR{Number: 201, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}, Author: github.Author{Login: "bot"}}
	h.gh.prs[202] = &github.PR{Number: 202, State: "OPEN", HeadRefName: "other", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}, Author: github.Author{Login: "someone-else"}}

	snap, err := h.sched.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var issues, prs []int
	for _, i := range snap.issues {
		issues = append(issues, i.Number)
	}
	for _, p := range snap.prs {
		prs = append(prs, p.Number)
	}
	if got := fmt.Sprint(issues); got != "[1 2]" {
		t.Errorf("polled issues %s, want kyle's and the factory's own", got)
	}
	if got := fmt.Sprint(prs); got != "[201]" {
		t.Errorf("polled pull requests %s, want the factory's own only", got)
	}
}
