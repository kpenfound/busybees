package duplicates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/github"
)

func TestTokens(t *testing.T) {
	got := tokens("The `bees status` panics when status.json is EMPTY (#12).")
	want := []string{"12", "bees", "empty", "json", "panics", "status"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d tokens %v, want %v", len(got), got, want)
	}
	for _, w := range []string{"the", "when", "is"} {
		if got[w] {
			t.Errorf("stop word %q kept", w)
		}
	}
}

func TestDice(t *testing.T) {
	set := func(words ...string) map[string]bool {
		m := map[string]bool{}
		for _, w := range words {
			m[w] = true
		}
		return m
	}
	cases := []struct {
		a, b map[string]bool
		want float64
	}{
		{set(), set(), 0},
		{set("a"), set(), 0},
		{set("a", "b"), set("a", "b"), 1},
		{set("a", "b"), set("b", "c"), 0.5},
		{set("a", "b", "c"), set("d"), 0},
	}
	for _, c := range cases {
		if got := dice(c.a, c.b); got != c.want {
			t.Errorf("dice(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestScore(t *testing.T) {
	const (
		title = "bees status panics when status.json is empty"
		body  = "Run `bees status` with an empty status.json.\n\nExpected: a message. Actual: a panic."
	)
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

	// The same text is a perfect score; a rewording keeps most of it.
	if got := Score(title, body, title, body); got != 1 {
		t.Errorf("identical: %v", got)
	}
	if got := Score(title, body, "QA: `bees status` panics on an empty status.json", body); got < 0.8 {
		t.Errorf("reworded title: %v", got)
	}
	// The title weighs 0.7 of the score: the same title over an unrelated
	// body clears the threshold, the same body under an unrelated title does
	// not.
	if got := Score(title, body, title, "Nothing in common here at all whatsoever."); !near(got, titleWeight) {
		t.Errorf("same title, other body: %v, want %v", got, titleWeight)
	}
	if got := Score(title, body, "Add a reviewer model key to the config template", body); got >= Threshold {
		t.Errorf("same body, other title: %v reached the threshold", got)
	}
	// An issue without a body is compared on its title alone, from either
	// side.
	if got := Score(title, "", title, body); got != 1 {
		t.Errorf("no body on the new issue: %v", got)
	}
	if got := Score(title, body, title, ""); got != 1 {
		t.Errorf("no body on the existing issue: %v", got)
	}
	// Unrelated issues score low even though both mention bees.
	if got := Score(title, body, "Add a bees.toml key for the reviewer model", "Every new key needs a default and a test."); got >= 0.3 {
		t.Errorf("unrelated: %v", got)
	}
}

// fixture is a repository's worth of issues: the open one to match, a closed
// one to match, a human-filed one without the bees label to match, and
// unrelated ones that must not.
func fixture() []github.Issue {
	return []github.Issue{
		{Number: 1, Title: "Add a bees.toml key for the reviewer model", Body: "Every new key needs a default, validation and a test.", State: "CLOSED", Labels: []github.Label{{Name: "bees"}}},
		{Number: 2, Title: "bees status panics when status.json is empty", Body: "Run bees status right after bees init. Expected a message, got a panic.", State: "CLOSED", Labels: []github.Label{{Name: "bees"}, {Name: "bees:bug"}}},
		{Number: 3, Title: "Document the release workflow", Body: "docs/releasing.md should say how the tag is cut.", State: "OPEN", Labels: []github.Label{{Name: "bees"}}},
		{Number: 4, Title: "bees status panics on an empty status.json", Body: "", State: "OPEN"},
		{Number: 5, Title: "bees status panics on an empty status.json", Body: "Seen after bees init: bees status panics instead of printing a message.", State: "OPEN", Labels: []github.Label{{Name: "bees"}}},
		{Number: 6, Title: "The live view flickers on resize", Body: "Resize the terminal while bees run draws the live view.", State: "OPEN", Labels: []github.Label{{Name: "bees"}}},
	}
}

func numbers(ms []Match) string {
	var out []string
	for _, m := range ms {
		out = append(out, fmt.Sprint(m.Number))
	}
	return strings.Join(out, " ")
}

func TestRankFindsSimilarIssuesBestFirst(t *testing.T) {
	got := Rank(fixture(), "QA: bees status panics on empty status.json", "Ran bees status after bees init and it panics instead of printing a message.")
	// 4 and 5 share the exact title. 4 has no body, so it is compared on
	// the title alone; 5's body is close but not the same and weighs its
	// score down a little. 2 is the closed one with a reworded title.
	// Nothing else.
	if numbers(got) != "4 5 2" {
		t.Fatalf("ranked %s: %+v", numbers(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Errorf("not ranked by score: %+v", got)
		}
	}
	if got[0].Score < Threshold || got[2].Score < Threshold {
		t.Errorf("a match below the threshold: %+v", got)
	}
	// The human-filed issue without the bees label and the closed one are
	// both there, with their state and title carried over.
	if got[0].State != "OPEN" || got[0].Title != "bees status panics on an empty status.json" {
		t.Errorf("match 4: %+v", got[0])
	}
	if got[2].State != "CLOSED" || got[2].Number != 2 {
		t.Errorf("match 2: %+v", got[2])
	}
}

func TestRankReturnsNothingForAnUnrelatedIssue(t *testing.T) {
	got := Rank(fixture(), "bees kill leaves the MCP server running", "After bees kill, bees mcp serve is still listening on the port.")
	if len(got) != 0 {
		t.Fatalf("unrelated issue matched %s: %+v", numbers(got), got)
	}
	if got := Rank(nil, "anything", "at all"); len(got) != 0 {
		t.Fatalf("empty repository matched: %+v", got)
	}
}

func TestRankBreaksTiesTowardsTheNewerIssue(t *testing.T) {
	issues := []github.Issue{
		{Number: 10, Title: "bees status panics on an empty status.json", State: "OPEN"},
		{Number: 30, Title: "bees status panics on an empty status.json", State: "CLOSED"},
		{Number: 20, Title: "bees status panics on an empty status.json", State: "OPEN"},
	}
	got := Rank(issues, "bees status panics on an empty status.json", "")
	if numbers(got) != "30 20 10" {
		t.Fatalf("ties ranked %s", numbers(got))
	}
}

func TestFindListsEveryIssueAndRanks(t *testing.T) {
	c := github.New("acme/widgets")
	var calls [][]string
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return json.Marshal(fixture())
	}
	got, err := Find(context.Background(), c, "bees status panics on an empty status.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
	if args := strings.Join(calls[0], " "); !strings.HasPrefix(args, "issue list -R acme/widgets --state all ") || strings.Contains(args, "--label") {
		t.Fatalf("Find listed with %q; it must ask for open and closed issues without a filter", args)
	}
	// 4 and 5 tie on their identical title (the new issue has no body, so
	// bodies do not count) and the newer one comes first.
	if numbers(got) != "5 4 2" {
		t.Fatalf("ranked %s: %+v", numbers(got), got)
	}
	if got[0].Score != 1 || got[1].Score != 1 {
		t.Errorf("an identical title without a body on the new issue should score 1: %+v", got[:2])
	}
}

func TestFindReturnsTheListingError(t *testing.T) {
	c := github.New("acme/widgets")
	boom := errors.New("gh: api rate limit exceeded")
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) { return nil, boom }
	got, err := Find(context.Background(), c, "anything", "")
	if !errors.Is(err, boom) || got != nil {
		t.Fatalf("got %v, %v; want the listing error and no matches", got, err)
	}
}
