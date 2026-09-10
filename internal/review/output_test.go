package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/github"
)

// sampleDiff is a pull request's diff as gh prints it: a file with two
// hunks, a file the change deleted, and a renamed file.
const sampleDiff = `diff --git a/gather.go b/gather.go
index 1111111..2222222 100644
--- a/gather.go
+++ b/gather.go
@@ -10,6 +10,7 @@ func Gather() {
 	a()
 	b()
-	c()
+	c(1)
+	d()
 	e()
 	f()
 	g()
@@ -40,3 +41,4 @@ func Other() {
 	x()
 	y()
 	z()
+	w()
diff --git a/old.go b/old.go
deleted file mode 100644
index 3333333..0000000
--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package old
-
diff --git a/before.go b/after.go
similarity index 90%
rename from before.go
rename to after.go
--- a/before.go
+++ b/after.go
@@ -1,2 +1,2 @@
 package widgets
-// before
+// after
\ No newline at end of file
diff --git a/schema.sql b/schema.sql
--- a/schema.sql
+++ b/schema.sql
@@ -1,3 +1,3 @@
 create table widgets (id int);
--- the old comment
+++ the new comment, which starts with two pluses
 create table gadgets (id int);
`

func TestAnchorsAreEveryLineOfEveryHunkOnBothSides(t *testing.T) {
	a := ParseAnchors(sampleDiff)
	has := func(file, side string, start, end int) bool {
		t.Helper()
		return a.Has(&Finding{File: file, Side: side, Lines: LineRange{start, end}})
	}
	for _, c := range []struct {
		file       string
		side       string
		start, end int
		want       bool
		why        string
	}{
		{"gather.go", SideNew, 13, 13, true, "an added line"},
		{"gather.go", SideNew, 10, 10, true, "the first context line of the hunk"},
		{"gather.go", SideNew, 16, 16, true, "the last context line of the hunk"},
		{"gather.go", SideNew, 10, 16, true, "the whole hunk"},
		{"gather.go", SideNew, 17, 17, false, "the line after the hunk"},
		{"gather.go", SideNew, 9, 9, false, "the line before the hunk"},
		{"gather.go", SideNew, 44, 44, true, "the added line of the second hunk"},
		{"gather.go", SideNew, 16, 41, false, "a range across two hunks"},
		{"gather.go", SideNew, 16, 17, false, "a range leaving the hunk"},
		{"gather.go", SideOld, 12, 12, true, "the removed line, on the old side"},
		{"gather.go", SideOld, 13, 13, true, "a context line, on the old side"},
		{"gather.go", SideOld, 16, 16, false, "the old side has one line fewer"},
		{"gather.go", SideNew, 12, 12, true, "the old side's removed line is the new side's changed one"},
		{"old.go", SideOld, 1, 2, true, "a deleted file's lines, on the old side"},
		{"old.go", SideNew, 1, 1, false, "a deleted file has no new side"},
		{"after.go", SideNew, 2, 2, true, "a renamed file under its new path"},
		{"before.go", SideNew, 2, 2, false, "not under its old path"},
		{"after.go", SideNew, 3, 3, false, "the no-newline marker is not a line"},
		{"nowhere.go", SideNew, 1, 1, false, "a file the diff does not touch"},
		{"schema.sql", SideOld, 2, 2, true, "a removed line starting with two dashes is a line, not a header"},
		{"schema.sql", SideNew, 2, 2, true, "an added line starting with two pluses is a line, not a header"},
		{"schema.sql", SideNew, 3, 3, true, "and the hunk goes on after them"},
	} {
		if got := has(c.file, c.side, c.start, c.end); got != c.want {
			t.Errorf("%s:%d-%d (%s): anchored %v, want %v: %s", c.file, c.start, c.end, c.side, got, c.want, c.why)
		}
	}
	// A finding about the change as a whole, one with a file and no lines,
	// and a nil set of anchors anchor nothing.
	for _, f := range []Finding{{}, {File: "gather.go", Side: SideNew}} {
		if a.Has(&f) {
			t.Errorf("%+v anchored", f)
		}
	}
	var none *Anchors
	if none.Has(&Finding{File: "gather.go", Side: SideNew, Lines: LineRange{13, 13}}) {
		t.Error("nil anchors anchored a finding")
	}
}

// selections are the findings of judged as triage selected them, the first
// with its text edited.
func selections(edited string) []Selection {
	a := &Artifact{Findings: &Findings{Items: Merge([]Finding{
		{Angle: AngleTests, Category: "missing test", Severity: SeverityHigh, File: "gather.go", Lines: LineRange{12, 13}, Side: SideNew,
			Title: "c has no test for its argument", Body: "nothing exercises c(1)", Suggestion: "\tc(1) // tested\n\td()"},
		{Angle: AngleStyle, Category: "naming", Severity: SeverityMedium, File: "old.go", Lines: LineRange{1, 1}, Side: SideOld,
			Title: "The package was the last of its name", Body: "nothing else was called old", Suggestion: "package older"},
		{Angle: AngleStyle, Category: "docs", Severity: SeverityLow, File: "README.md", Lines: LineRange{3, 3}, Side: SideNew,
			Title: "The README still names c()", Body: "the sentence the change made false is still there"},
		{Angle: AngleAcceptance, Category: "scope", Severity: SeverityLow,
			Title: "The change renames Gather, which the issue did not ask for", Body: "every caller moves"},
	}, nil)}, Triage: &Triage{}}
	q := &Queue{Artifact: a}
	for i, f := range a.Findings.Items {
		comment := ""
		if i == 0 {
			comment = edited
		}
		a.Triage.Decisions = append(a.Triage.Decisions, Decision{Finding: f.ID, Action: ActionSelect, Comment: comment})
	}
	return q.Selected()
}

func TestComposeAnchorsWhatTheDiffHasAndFoldsTheRest(t *testing.T) {
	selected := selections("Edited: c has no test.")
	req, err := Compose(OutputReject, selected, ParseAnchors(sampleDiff))
	if err != nil {
		t.Fatal(err)
	}
	if req.Event != "REQUEST_CHANGES" {
		t.Errorf("event %q", req.Event)
	}
	if len(req.Comments) != 2 {
		t.Fatalf("comments %+v, want the two the diff has the lines of", req.Comments)
	}
	// A range on the new side, with the edited text and the suggestion as a
	// block GitHub can apply.
	c := req.Comments[0]
	if c.Path != "gather.go" || c.Line != 13 || c.StartLine != 12 || c.Side != "RIGHT" || c.StartSide != "RIGHT" {
		t.Errorf("the range is anchored as %+v", c)
	}
	if want := "Edited: c has no test.\n\n```suggestion\n\tc(1) // tested\n\td()\n```"; c.Body != want {
		t.Errorf("comment body:\n%s\nwant:\n%s", c.Body, want)
	}
	// One line on the old side: no start, LEFT, and the suggestion as a
	// plain code block, since GitHub cannot apply one to a removed line.
	c = req.Comments[1]
	if c.Path != "old.go" || c.Line != 1 || c.StartLine != 0 || c.StartSide != "" || c.Side != "LEFT" {
		t.Errorf("the removed line is anchored as %+v", c)
	}
	if want := "The package was the last of its name\n\nnothing else was called old\n\n```\npackage older\n```"; c.Body != want {
		t.Errorf("comment body:\n%s\nwant:\n%s", c.Body, want)
	}
	// The finding on a line the diff does not have and the one about the
	// change as a whole are in the body, located, in order, and not among
	// the comments.
	for _, want := range []string{
		"`README.md:3` · low · docs\n\nThe README still names c()\n\nthe sentence the change made false is still there",
		"\n\n---\n\n",
		"low · scope\n\nThe change renames Gather, which the issue did not ask for\n\nevery caller moves",
	} {
		if !strings.Contains(req.Body, want) {
			t.Errorf("the body lacks %q:\n%s", want, req.Body)
		}
	}
	if strings.Contains(req.Body, "c has no test") || strings.Contains(req.Body, "gather.go") {
		t.Errorf("an anchored finding is in the body too:\n%s", req.Body)
	}
	if req.CommitID != "" {
		t.Errorf("Compose set the commit %q: Post reads it", req.CommitID)
	}
}

func TestComposeGivesABodyToAReviewGitHubNeedsOneFor(t *testing.T) {
	// Every selection anchored: comment and reject need a body all the
	// same, approve does not.
	selected := selections("")[:2]
	anchors := ParseAnchors(sampleDiff)
	for mode, want := range map[string]string{OutputComment: "2 comments inline.", OutputReject: "2 comments inline.", OutputApprove: ""} {
		req, err := Compose(mode, selected, anchors)
		if err != nil {
			t.Fatal(err)
		}
		if req.Body != want || len(req.Comments) != 2 {
			t.Errorf("%s: body %q with %d comments, want %q with 2", mode, req.Body, len(req.Comments), want)
		}
	}
	// Nothing selected: an approval with nothing to say is posted, the
	// other two are refused, and the person can answer.
	req, err := Compose(OutputApprove, nil, anchors)
	if err != nil || req.Event != "APPROVE" || req.Body != "" || len(req.Comments) != 0 {
		t.Errorf("an empty approval: %+v, %v", req, err)
	}
	for _, mode := range []string{OutputComment, OutputReject} {
		_, err := Compose(mode, nil, anchors)
		if !isRefusal(err) || !strings.Contains(err.Error(), "nothing was selected") {
			t.Errorf("%s with nothing selected: %v, want a refusal", mode, err)
		}
	}
	// A mode that posts nothing composes nothing.
	for _, mode := range []string{OutputReport, OutputDiscard, OutputAsk, ""} {
		if _, err := Compose(mode, selected, anchors); err == nil {
			t.Errorf("%q composed a review", mode)
		}
	}
}

func TestSuggestionsWithBackticksAreFencedLonger(t *testing.T) {
	s := Selection{Finding: Finding{File: "README.md", Lines: LineRange{3, 3}, Side: SideNew, Title: "T", Suggestion: "```go\nx\n```"}, Comment: "T"}
	got := renderSelection(s, true, false)
	if want := "T\n\n````suggestion\n```go\nx\n```\n````"; got != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

// reviewGH is a fake gh for Post: the pull request, its diff, and what was
// posted through the stdin hook.
type reviewGH struct {
	pr     github.PR
	diff   string
	errs   map[string]bool
	posted []string
	args   []string
}

func (f *reviewGH) client(t *testing.T) *github.Client {
	t.Helper()
	c := github.New(testRepo)
	c.Exec = func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "view":
			if f.errs["pr view"] {
				return nil, errors.New("gh pr view: no")
			}
			return json.Marshal(f.pr)
		case args[0] == "pr" && args[1] == "diff":
			if f.errs["pr diff"] {
				return nil, errors.New("gh pr diff: no")
			}
			return []byte(f.diff), nil
		}
		t.Errorf("unexpected gh %s", strings.Join(args, " "))
		return nil, errors.New("unexpected")
	}
	c.ExecStdin = func(_ context.Context, stdin string, args ...string) ([]byte, error) {
		if f.errs["post"] {
			return nil, errors.New("gh api: 422")
		}
		f.posted = append(f.posted, stdin)
		f.args = args
		return nil, nil
	}
	return c
}

func TestPostSubmitsOneReviewAgainstTheHeadItRead(t *testing.T) {
	gh := &reviewGH{pr: github.PR{Number: 7, HeadSHA: "abc123"}, diff: sampleDiff}
	ref := Ref{Repo: testRepo, Number: 7}
	posted, err := Post(context.Background(), gh.client(t), ref, OutputApprove, selections(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("%d calls posted, want one review", len(gh.posted))
	}
	if got := strings.Join(gh.args, " "); got != "api --method POST repos/acme/widgets/pulls/7/reviews --input -" {
		t.Errorf("posted through %q", got)
	}
	var sent github.ReviewRequest
	if err := json.Unmarshal([]byte(gh.posted[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.CommitID != "abc123" || sent.Event != "APPROVE" || len(sent.Comments) != 2 {
		t.Errorf("sent %+v", sent)
	}
	if posted.CommitID != "abc123" || len(posted.Comments) != 2 {
		t.Errorf("returned %+v, want what was sent", posted)
	}
	// Nothing is sent when the pull request or the diff cannot be read,
	// when the review cannot be composed, and for a mode that posts
	// nothing.
	for _, c := range []struct {
		name string
		gh   *reviewGH
		mode string
		sel  []Selection
		want string
	}{
		{"pr view", &reviewGH{errs: map[string]bool{"pr view": true}}, OutputComment, selections(""), "read acme/widgets#7"},
		{"pr diff", &reviewGH{errs: map[string]bool{"pr diff": true}}, OutputComment, selections(""), "read the diff of acme/widgets#7"},
		// Refused before the pull request is read: a gh that cannot read
		// it is not what the error names.
		{"empty", &reviewGH{errs: map[string]bool{"pr view": true}}, OutputComment, nil, "nothing was selected"},
		{"report", &reviewGH{diff: sampleDiff}, OutputReport, selections(""), "posts nothing"},
	} {
		_, err := Post(context.Background(), c.gh.client(t), ref, c.mode, c.sel)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
		if len(c.gh.posted) != 0 {
			t.Errorf("%s: a review was posted", c.name)
		}
	}
	// A post GitHub refused is the error, with nothing returned.
	gh = &reviewGH{diff: sampleDiff, errs: map[string]bool{"post": true}}
	if posted, err := Post(context.Background(), gh.client(t), ref, OutputComment, selections("")); err == nil || posted != nil {
		t.Errorf("a refused post returned %+v, %v", posted, err)
	}
}

func TestTheReportIsEverySelectionAsMarkdown(t *testing.T) {
	got := Report(testBrief(), selections("Edited: c has no test."))
	for _, want := range []string{
		"# Review of acme/widgets#7: widgets: gather the context\n\nhttps://github.com/acme/widgets/pull/7\n\n",
		"`gather.go:12-13` · high · missing test\n\nEdited: c has no test.\n\n```\n\tc(1) // tested\n\td()\n```\n\n---\n\n",
		"`old.go:1 (removed)` · medium · naming\n\nThe package was the last of its name\n\nnothing else was called old\n\n```\npackage older\n```",
		"`README.md:3` · low · docs\n\n",
		"low · scope\n\nThe change renames Gather, which the issue did not ask for\n\nevery caller moves\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "suggestion") {
		t.Errorf("the report holds a suggestion block, which nothing outside GitHub applies:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("the report ends %q", got[len(got)-4:])
	}
	brief := testBrief()
	brief.Title = ""
	if got := Report(brief, nil); got != "# Review of acme/widgets#7\n\nhttps://github.com/acme/widgets/pull/7\n\nNothing was selected.\n" {
		t.Errorf("an empty report:\n%s", got)
	}
}

func TestPostsNamesTheModesThatSubmitAReview(t *testing.T) {
	for _, mode := range OutputModes {
		want := mode == OutputApprove || mode == OutputComment || mode == OutputReject
		if Posts(mode) != want {
			t.Errorf("Posts(%q) = %v", mode, !want)
		}
		if (Verb(mode) != "") != want {
			t.Errorf("Verb(%q) = %q", mode, Verb(mode))
		}
	}
	if Verb(OutputReject) != "requested changes" || Verb(OutputApprove) != "approved" || Verb(OutputComment) != "commented" {
		t.Error("the verbs are not the past tense of the modes")
	}
}
