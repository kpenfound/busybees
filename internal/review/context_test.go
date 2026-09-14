package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

const testRepo = "acme/widgets"

// fakeGH answers the gh calls a context gather makes. Every field is what
// one call returns; a name in errs makes that call fail instead.
type fakeGH struct {
	pr   github.PR
	diff string
	// diffErr is what `gh pr diff` fails with when errs names it, in place
	// of a plain refusal.
	diffErr  string
	issues   map[int]github.Issue
	comments []map[string]any
	reviews  []map[string]any
	errs     map[string]bool
	calls    []string
}

func (f *fakeGH) client(t *testing.T) *github.Client {
	t.Helper()
	c := github.New(testRepo)
	c.Exec = func(_ context.Context, args ...string) ([]byte, error) {
		f.calls = append(f.calls, strings.Join(args[:2], " "))
		switch {
		case args[0] == "pr" && args[1] == "view":
			return f.answer(t, "pr view", f.pr)
		case args[0] == "pr" && args[1] == "diff":
			if f.errs["pr diff"] {
				if f.diffErr != "" {
					return nil, fmt.Errorf("gh pr diff %d -R %s: exit status 1: %s", f.pr.Number, testRepo, f.diffErr)
				}
				return nil, fmt.Errorf("gh pr diff: no")
			}
			return []byte(f.diff), nil
		case args[0] == "issue" && args[1] == "view":
			n, err := strconv.Atoi(args[2])
			if err != nil {
				return nil, err
			}
			issue, ok := f.issues[n]
			if !ok {
				return nil, fmt.Errorf("gh issue view %d: no issue", n)
			}
			return f.answer(t, "issue view", issue)
		case strings.HasSuffix(args[len(args)-1], "/reviews"):
			return f.answer(t, "reviews", []any{f.reviews})
		case strings.HasSuffix(args[len(args)-1], "/comments"):
			return f.answer(t, "comments", []any{f.comments})
		}
		return nil, fmt.Errorf("unexpected gh %s", strings.Join(args, " "))
	}
	return c
}

func (f *fakeGH) answer(t *testing.T, call string, v any) ([]byte, error) {
	t.Helper()
	if f.errs[call] {
		return nil, fmt.Errorf("gh %s: no", call)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func activity(id int, author, body string, minute int) map[string]any {
	return map[string]any{
		"id": id, "user": map[string]any{"login": author}, "body": body,
		"created_at":   time.Date(2026, 9, 10, 12, minute, 0, 0, time.UTC).Format(time.RFC3339),
		"submitted_at": time.Date(2026, 9, 10, 12, minute, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

// samplePR is a pull request with a body, a conversation and a diff.
func sampleGH() *fakeGH {
	return &fakeGH{
		pr: github.PR{Number: 7, Title: "Add the widget", State: "OPEN", Body: "Closes #12\n\nA summary.",
			HeadRefName: "widget", BaseRefName: "main", Author: github.Author{Login: "kim"}},
		diff: "diff --git a/widget.go b/widget.go\n--- a/widget.go\n+++ b/widget.go\n@@ -1 +1,2 @@\n+func Widget() {}\n",
		issues: map[int]github.Issue{12: {Number: 12, Title: "We want a widget", State: "OPEN", Body: "It should spin.",
			Comments: []github.Comment{{Author: github.Author{Login: "sam"}, Body: "Anticlockwise."}}}},
		comments: []map[string]any{activity(1, "sam", "Looks good.", 5)},
		reviews:  []map[string]any{{"id": 2, "user": map[string]any{"login": "lee"}, "body": "One thing.", "state": "CHANGES_REQUESTED", "submitted_at": "2026-09-10T12:03:00Z"}},
	}
}

func gatherTest(t *testing.T, f *fakeGH, p *Project, dir string) *Bundle {
	t.Helper()
	pipeline := &Pipeline{Client: f.client(t), Project: p, Dir: dir}
	bundle, err := pipeline.Gather(context.Background(), 7, "")
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return bundle
}

func TestGatherRunsEveryEnabledSourceInOrder(t *testing.T) {
	f := sampleGH()
	dir := gitRepo(t, "https://github.com/"+testRepo)
	writeFile(t, dir, "CLAUDE.md", "Widgets spin.")
	bundle := gatherTest(t, f, nil, dir)

	if bundle.Ref != (Ref{testRepo, 7}) || bundle.PR.Title != "Add the widget" {
		t.Fatalf("bundle is about %+v / %q", bundle.Ref, bundle.PR.Title)
	}
	want := []string{SourceDiff, SourcePRBody, SourceLinkedIssues, SourceStyleFiles}
	if got := bundle.Sources(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sources %v, want %v", got, want)
	}
	if got := bundle.Of(SourceDiff); len(got) != 1 || !strings.Contains(got[0].Content, "func Widget() {}") {
		t.Fatalf("diff items %+v", got)
	}
	if got := bundle.Of(SourceLinkedIssues); len(got) != 1 || got[0].Name != "#12" ||
		!strings.Contains(got[0].Content, "It should spin.") || !strings.Contains(got[0].Content, "Anticlockwise.") {
		t.Fatalf("linked issue items %+v", got)
	}
	if got := bundle.Of(SourceStyleFiles); len(got) != 1 || got[0].Name != "CLAUDE.md" || got[0].Content != "Widgets spin." {
		t.Fatalf("style items %+v", got)
	}
}

func TestGatherWithoutACheckoutSaysWhatItCouldNotRead(t *testing.T) {
	bundle := gatherTest(t, sampleGH(), nil, "")
	if got := bundle.Sources(); !reflect.DeepEqual(got, []string{SourceDiff, SourcePRBody, SourceLinkedIssues}) {
		t.Fatalf("sources %v, want the ones that read GitHub alone", got)
	}
	if len(bundle.Skipped) != 2 {
		t.Fatalf("skipped %v, want the two sources that read files", bundle.Skipped)
	}
	for _, want := range []string{"style files", "callers"} {
		if !strings.Contains(strings.Join(bundle.Skipped, "\n"), want) {
			t.Fatalf("skipped %v does not name %q", bundle.Skipped, want)
		}
	}
}

func TestGatherHonoursADisabledSourceAndTheProjectsOwn(t *testing.T) {
	dir := gitRepo(t, "https://github.com/"+testRepo)
	writeFile(t, dir, "CLAUDE.md", "Widgets spin.")
	writeFile(t, dir, "schema.sql", "create table widget();")
	p, err := ParseProject(`
[[context_sources]]
name = "diff"
enabled = false

[[context_sources]]
name = "schema"
files = ["*.sql"]
`, filepath.Join(dir, ProjectFile))
	if err != nil {
		t.Fatal(err)
	}
	bundle := gatherTest(t, sampleGH(), p, dir)
	if got := bundle.Of(SourceDiff); len(got) != 0 {
		t.Fatalf("a disabled source gathered %+v", got)
	}
	got := bundle.Of("schema")
	if len(got) != 1 || got[0].Name != "schema.sql" || got[0].Content != "create table widget();" {
		t.Fatalf("the project's own source gathered %+v", got)
	}
	// The project's own sources come last, after every built-in.
	if last := bundle.Sources(); last[len(last)-1] != "schema" {
		t.Fatalf("sources %v, want the project's own last", last)
	}
}

func TestGatherFailsWhenThePullRequestCannotBeRead(t *testing.T) {
	f := sampleGH()
	f.errs = map[string]bool{"pr view": true}
	pipeline := &Pipeline{Client: f.client(t)}
	if _, err := pipeline.Gather(context.Background(), 7, ""); err == nil || !strings.Contains(err.Error(), "acme/widgets#7") {
		t.Fatalf("error %v, want one naming the pull request", err)
	}
}

func TestGatherFailsWhenASourceCannotRun(t *testing.T) {
	f := sampleGH()
	f.errs = map[string]bool{"comments": true}
	pipeline := &Pipeline{Client: f.client(t)}
	_, err := pipeline.Gather(context.Background(), 7, "")
	if err == nil || !strings.Contains(err.Error(), `context source "pr_body"`) {
		t.Fatalf("error %v, want one naming the source", err)
	}
}

// tooLarge is what gh says of a pull request past GitHub's limit on the
// files a diff may touch.
const tooLarge = "could not find pull request diff: HTTP 406: Sorry, the diff exceeded the maximum number of files (300)"

// The diff source reads the diff from the checkout the pipeline clones
// under the artifact directory, never through gh: a pull request GitHub
// refuses the diff of is gathered all the same.
func TestTheDiffIsReadFromTheCheckoutNotThroughGh(t *testing.T) {
	f := sampleGH()
	f.errs = map[string]bool{"pr diff": true}
	f.diffErr = tooLarge
	docker := fakeDocker(t)
	artifact := filepath.Join(t.TempDir(), "review-7")
	var log bytes.Buffer
	pipeline := &Pipeline{Client: f.client(t), Checkout: &Checkout{DockerBin: docker}, Log: &log}
	bundle, err := pipeline.Gather(context.Background(), 7, artifact)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := bundle.Of(SourceDiff)
	if len(got) != 1 || !strings.Contains(got[0].Content, "+++ b/spinner.go") || !strings.Contains(got[0].Content, "+func Spin() {}") {
		t.Fatalf("diff items %+v, want the checkout's diff", got)
	}
	if slices.Contains(f.calls, "pr diff") {
		t.Errorf("gh was asked for the diff: %v", f.calls)
	}
	if len(bundle.Skipped) != 2 || !strings.Contains(bundle.Skipped[0], "style files") || !strings.Contains(bundle.Skipped[1], "callers") {
		t.Errorf("skipped %v, want the two sources that read the machine's checkout alone", bundle.Skipped)
	}
	// The clone was made under the artifact, with the base branch named,
	// and is reported for what it is; it stays for the angles to run in.
	want := filepath.Join(artifact, CheckoutDir)
	if _, err := os.Stat(filepath.Join(want, "widget.go")); err != nil {
		t.Errorf("the clone's files are not in the checkout: %v", err)
	}
	if run := beside(t, docker, "run-args.txt"); !strings.Contains(run, "\nrefs/pull/7/head\nmain\n") {
		t.Errorf("run args do not name the base branch:\n%s", run)
	}
	if !strings.Contains(log.String(), "checked out acme/widgets#7 under "+want+"\n") {
		t.Errorf("log = %q, want the checkout reported", log.String())
	}

	// The same pipeline gathering again for the same review uses the clone
	// that is there.
	if _, err := pipeline.Gather(context.Background(), 7, artifact); err != nil {
		t.Fatal(err)
	}
	if got := beside(t, docker, "calls.txt"); got != "image\nbuild\nrun\n" {
		t.Errorf("docker was called %q, want one clone for the two gathers", got)
	}
}

// Without a checkout the diff is read through gh, as it is without a
// Checkout at all or with no artifact directory to clone under.
func TestWithoutACheckoutTheDiffIsReadThroughGh(t *testing.T) {
	f := sampleGH()
	missing := filepath.Join(t.TempDir(), "docker")
	var log bytes.Buffer
	pipeline := &Pipeline{Client: f.client(t), Checkout: &Checkout{DockerBin: missing}, Log: &log}
	artifact := filepath.Join(t.TempDir(), "review-7")
	bundle, err := pipeline.Gather(context.Background(), 7, artifact)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if got := bundle.Of(SourceDiff); len(got) != 1 || !strings.Contains(got[0].Content, "func Widget() {}") {
		t.Fatalf("diff items %+v, want gh's diff", got)
	}
	if n := slices.Index(f.calls, "pr diff"); n < 0 {
		t.Errorf("gh was not asked for the diff: %v", f.calls)
	}
	if _, err := os.Stat(filepath.Join(artifact, CheckoutDir)); !os.IsNotExist(err) {
		t.Errorf("a checkout directory was left behind: %v", err)
	}
	if want := "could not check out acme/widgets#7 in a container: " + missing + " is not installed; the diff is read through gh\n"; !strings.Contains(log.String(), want) {
		t.Errorf("log = %q, want %q", log.String(), want)
	}

	// No artifact directory: nothing is attempted, and docker is not even
	// looked for.
	docker := fakeDocker(t)
	f = sampleGH()
	pipeline = &Pipeline{Client: f.client(t), Checkout: &Checkout{DockerBin: docker}}
	if _, err := pipeline.Gather(context.Background(), 7, ""); err != nil {
		t.Fatal(err)
	}
	if got := beside(t, docker, "calls.txt"); got != "" {
		t.Errorf("docker was called %q with no artifact directory to clone under", got)
	}
	if !slices.Contains(f.calls, "pr diff") {
		t.Errorf("gh was not asked for the diff: %v", f.calls)
	}
}

// A diff that can be read neither from a checkout nor through gh is
// context the review goes on without: the diff source, and the callers
// source that reads the changed symbols out of it, are skipped and say why,
// and Gather returns the bundle.
func TestADiffThatCannotBeReadIsSkippedNotAnError(t *testing.T) {
	f := sampleGH()
	f.errs = map[string]bool{"pr diff": true}
	f.diffErr = tooLarge
	dir := gitRepo(t, "https://github.com/"+testRepo)
	pipeline := &Pipeline{Client: f.client(t), Dir: dir, Checkout: &Checkout{DockerBin: filepath.Join(t.TempDir(), "docker")}}
	bundle, err := pipeline.Gather(context.Background(), 7, filepath.Join(t.TempDir(), "review-7"))
	if err != nil {
		t.Fatalf("Gather returned %v, want the bundle without the diff", err)
	}
	if got := bundle.Sources(); !reflect.DeepEqual(got, []string{SourcePRBody, SourceLinkedIssues}) {
		t.Errorf("sources %v, want the ones that need no diff", got)
	}
	if len(bundle.Skipped) != 2 {
		t.Fatalf("skipped %v, want the diff and the callers", bundle.Skipped)
	}
	for i, want := range []string{"diff: the diff of acme/widgets#7 could not be read through gh: ", "callers: callers of the changed symbols were not looked for: "} {
		if !strings.HasPrefix(bundle.Skipped[i], want) || !strings.Contains(bundle.Skipped[i], "maximum number of files (300)") {
			t.Errorf("skipped[%d] = %q, want %q and what gh said", i, bundle.Skipped[i], want)
		}
	}
	// The diff was asked of gh once for both sources, not once each.
	if n := strings.Count(strings.Join(f.calls, "\n"), "pr diff"); n != 1 {
		t.Errorf("gh pr diff ran %d times, want once", n)
	}
}

func TestGatherNeedsACollectorForEveryBuiltinItRuns(t *testing.T) {
	f := sampleGH()
	pipeline := &Pipeline{Client: f.client(t), Collectors: []Collector{diffSource{}}}
	_, err := pipeline.Gather(context.Background(), 7, "")
	if err == nil || !strings.Contains(err.Error(), `context source "pr_body": no collector`) {
		t.Fatalf("error %v, want one naming the source with no collector", err)
	}
}

// A collector replacing a built-in is what makes the source pluggable: the
// name in context.toml binds to whatever is registered for it.
func TestACollectorReplacesTheBuiltinOfTheSameName(t *testing.T) {
	f := sampleGH()
	pipeline := &Pipeline{Client: f.client(t), Collectors: append(Builtins(), stubSource{})}
	bundle, err := pipeline.Gather(context.Background(), 7, "")
	if err != nil {
		t.Fatal(err)
	}
	got := bundle.Of(SourceDiff)
	if len(got) != 1 || got[0].Content != "a diff of my own" {
		t.Fatalf("diff items %+v, want the replacement's", got)
	}
	for _, call := range f.calls {
		if call == "pr diff" {
			t.Fatal("the built-in diff source ran as well")
		}
	}
}

type stubSource struct{}

func (stubSource) Name() string { return SourceDiff }
func (stubSource) Collect(context.Context, *Input) ([]Item, error) {
	return []Item{{Source: SourceDiff, Name: "stub", Content: "a diff of my own"}}, nil
}

func TestBundleText(t *testing.T) {
	b := &Bundle{Ref: Ref{testRepo, 7}, Items: []Item{
		{Source: SourceDiff, Name: "acme/widgets#7", Content: "+one\n"},
		{Source: SourceStyleFiles, Name: "CLAUDE.md", Content: "Fenced:\n```go\nx := 1\n```\n"},
	}, Skipped: []string{"#99 is mentioned and is not an issue"}}
	text := b.Text()
	for _, want := range []string{
		"# Context for acme/widgets#7",
		"https://github.com/acme/widgets/pull/7",
		"## diff: acme/widgets#7",
		"```\n+one\n```",
		"## style_files: CLAUDE.md",
		"````\nFenced:\n```go\nx := 1\n```\n````",
		"## Not gathered\n\n- #99 is mentioned and is not an issue\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bundle text does not contain %q:\n%s", want, text)
		}
	}
}

func TestFenceIsLongerThanAnythingInside(t *testing.T) {
	for _, tc := range []struct{ content, fence string }{
		{"plain", "```"},
		{"a ``` fence", "````"},
		{"a ````` fence", "``````"},
		{"``inline``", "```"},
	} {
		if got := fenced(tc.content); !strings.HasPrefix(got, tc.fence+"\n") || !strings.HasSuffix(got, "\n"+tc.fence) {
			t.Fatalf("fenced(%q) = %q, want the fence %q", tc.content, got, tc.fence)
		}
	}
}

func TestOpenTakesTheTokenAndRefusesAnotherRepositorysCheckout(t *testing.T) {
	t.Setenv("REVIEW_TOKEN", "ghp_secret")
	cfg, err := ParseConfig("[github]\ntoken = \"$REVIEW_TOKEN\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{testRepo, 7}
	dir := gitRepo(t, "https://github.com/"+testRepo)
	writeFile(t, dir, ProjectFile, "[angles]\ngeneral = false\n")

	p, err := Open(context.Background(), ref, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Client.Repo != testRepo || p.Client.Token != "ghp_secret" {
		t.Fatalf("client %+v, want the repository under review and the configured token", p.Client)
	}
	if p.Client.ActsAs != "" {
		t.Fatalf("client acts as %q: a review is the person running it, never a factory login", p.Client.ActsAs)
	}
	if p.Dir != dir || !p.Project.Loaded {
		t.Fatalf("pipeline reads %q, project loaded %v", p.Dir, p.Project.Loaded)
	}

	// Run from a checkout of something else, the review reads neither its
	// files nor its context.toml.
	other := gitRepo(t, "https://github.com/other/thing")
	writeFile(t, other, ProjectFile, "[angles]\ngeneral = false\n")
	t.Chdir(other)
	p, err = Open(context.Background(), ref, cfg, other)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != "" || p.Project.Loaded {
		t.Fatalf("pipeline reads %q, project loaded %v: want no checkout at all", p.Dir, p.Project.Loaded)
	}
	if got := p.Project.EnabledAngles(); len(got) != len(BuiltinAngles) {
		t.Fatalf("angles %v, want the defaults rather than the other repository's", got)
	}
}

func TestOpenWithoutAConfigurationUsesTheMachinesOwnAuthentication(t *testing.T) {
	p, err := Open(context.Background(), Ref{testRepo, 7}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p.Client.Token != "" {
		t.Fatalf("token %q, want gh's own authentication", p.Client.Token)
	}
}
