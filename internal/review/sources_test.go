package review

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/github"
)

func TestPRBodySourceGathersTheConversationInOrder(t *testing.T) {
	f := sampleGH()
	f.comments = append(f.comments, activity(3, "kim", "   ", 9))
	bundle := gatherTest(t, f, nil, "")
	items := bundle.Of(SourcePRBody)
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
	}
	// The pull request first, then its conversation oldest first, whoever
	// wrote it; an empty body is nothing to read.
	if want := []string{"acme/widgets#7", "review by lee", "comment by sam"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("items %v, want %v", names, want)
	}
	if body := items[0].Content; !strings.Contains(body, "Add the widget") || !strings.Contains(body, "A summary.") ||
		!strings.Contains(body, "open by kim, widget into main") {
		t.Fatalf("the pull request item is %q", body)
	}
	if got := items[1].Content; got != "CHANGES_REQUESTED\n\nOne thing." {
		t.Fatalf("review item %q, want its verdict and its body", got)
	}
}

func TestDiffSourceReportsAnEmptyDiff(t *testing.T) {
	f := sampleGH()
	f.diff = "\n"
	bundle := gatherTest(t, f, nil, "")
	if got := bundle.Of(SourceDiff); len(got) != 0 {
		t.Fatalf("an empty diff gathered %+v", got)
	}
	if !strings.Contains(strings.Join(bundle.Skipped, "\n"), "the diff of acme/widgets#7 is empty") {
		t.Fatalf("skipped %v, want the empty diff", bundle.Skipped)
	}
}

func TestLinkedIssuesTakesTheClosedOnesFirst(t *testing.T) {
	pr := github.PR{Number: 7, Body: "Refs #4. Closes #12, mentions #4 again and #7 itself."}
	if got := linkedIssues(pr); !reflect.DeepEqual(got, []int{12, 4}) {
		t.Fatalf("linked issues %v, want the closed one first and the pull request itself left out", got)
	}
}

func TestLinkedIssuesSkipsWhatIsNotAnIssue(t *testing.T) {
	f := sampleGH()
	f.pr.Body = "Closes #12. See #99."
	bundle := gatherTest(t, f, nil, "")
	if got := bundle.Of(SourceLinkedIssues); len(got) != 1 || got[0].Name != "#12" {
		t.Fatalf("items %+v, want the one issue that could be read", got)
	}
	if !strings.Contains(strings.Join(bundle.Skipped, "\n"), "#99 is mentioned by acme/widgets#7") {
		t.Fatalf("skipped %v, want the reference that is not an issue", bundle.Skipped)
	}
}

func TestLinkedIssuesStopsAtTheCap(t *testing.T) {
	f := sampleGH()
	var refs []string
	f.issues = map[int]github.Issue{}
	for i := 1; i <= maxLinkedIssues+3; i++ {
		refs = append(refs, fmt.Sprintf("#%d", i+100))
		f.issues[i+100] = github.Issue{Number: i + 100, Title: "one", State: "OPEN"}
	}
	f.pr.Body = strings.Join(refs, " ")
	bundle := gatherTest(t, f, nil, "")
	if got := bundle.Of(SourceLinkedIssues); len(got) != maxLinkedIssues {
		t.Fatalf("%d items, want the cap of %d", len(got), maxLinkedIssues)
	}
	if !strings.Contains(strings.Join(bundle.Skipped, "\n"), "the last 3 were not read") {
		t.Fatalf("skipped %v, want the three the cap left out", bundle.Skipped)
	}
}

func TestStyleFilesReadsTheBuiltInsAndTheProjectsOwn(t *testing.T) {
	dir := gitRepo(t, "https://github.com/"+testRepo)
	writeFile(t, dir, "CLAUDE.md", "Widgets spin.")
	writeFile(t, dir, "CONTRIBUTING.md", "Run the tests.")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, filepath.Join("docs", "style.md"), "Short sentences.")
	p, err := ParseProject("style_sources = [\"docs/*.md\", \"CLAUDE.md\", \"rules/*.md\"]\n", filepath.Join(dir, ProjectFile))
	if err != nil {
		t.Fatal(err)
	}
	bundle := gatherTest(t, sampleGH(), p, dir)
	var names []string
	for _, it := range bundle.Of(SourceStyleFiles) {
		names = append(names, it.Name)
	}
	// The built-ins in their own order, then the project's, and a file
	// named twice read once.
	if want := []string{"CLAUDE.md", "CONTRIBUTING.md", filepath.Join("docs", "style.md")}; !reflect.DeepEqual(names, want) {
		t.Fatalf("style files %v, want %v", names, want)
	}
	// A built-in the repository does not have is not worth reporting; a
	// style source the project asked for and that matches nothing is.
	skipped := strings.Join(bundle.Skipped, "\n")
	if !strings.Contains(skipped, `"rules/*.md" matched no file`) {
		t.Fatalf("skipped %v, want the pattern that matched nothing", bundle.Skipped)
	}
	if strings.Contains(skipped, "AGENTS.md") {
		t.Fatalf("skipped %v: a built-in the repository does not have is not missing context", bundle.Skipped)
	}
	if strings.Contains(skipped, "CLAUDE.md") {
		t.Fatalf("skipped %v: a style source naming a file already read matched it all the same", bundle.Skipped)
	}
}

func TestAStyleSourceCannotLeaveTheCheckout(t *testing.T) {
	dir := gitRepo(t, "https://github.com/"+testRepo)
	outside := filepath.Join(filepath.Dir(dir), "secrets.md")
	if err := os.WriteFile(outside, []byte("the passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := ParseProject("style_sources = [\"../secrets.md\"]\n", filepath.Join(dir, ProjectFile))
	if err != nil {
		t.Fatal(err)
	}
	bundle := gatherTest(t, sampleGH(), p, dir)
	for _, it := range bundle.Of(SourceStyleFiles) {
		if strings.Contains(it.Content, "the passphrase") {
			t.Fatalf("a style source read %q, outside the checkout", it.Name)
		}
	}
	if !strings.Contains(strings.Join(bundle.Skipped, "\n"), "leaves the checkout") {
		t.Fatalf("skipped %v, want the pattern that leaves the checkout", bundle.Skipped)
	}
}

func TestCallersFindsTheChangedSymbolElsewhere(t *testing.T) {
	dir := gitRepo(t, "https://github.com/"+testRepo)
	writeFile(t, dir, "widget.go", "package a\n\nfunc Widget() {}\n")
	writeFile(t, dir, "spin.go", "package a\n\nfunc spin() { Widget() }\n")
	writeFile(t, dir, "other.go", "package a\n\nfunc other() { Widgets() }\n")
	runGit(t, dir, "add", ".")
	bundle := gatherTest(t, sampleGH(), nil, dir)

	items := bundle.Of(SourceCallers)
	if len(items) != 1 || items[0].Name != "Widget" {
		t.Fatalf("callers %+v, want the one changed symbol", items)
	}
	if !strings.Contains(items[0].Content, "spin.go:3:") {
		t.Fatalf("callers content %q, want the line that calls it", items[0].Content)
	}
	// widget.go is a file the diff changed: the bundle has it whole
	// already. Widgets is another symbol, not this one.
	if strings.Contains(items[0].Content, "widget.go") || strings.Contains(items[0].Content, "other.go") {
		t.Fatalf("callers content %q, want the callers outside the diff only", items[0].Content)
	}
}

// git grep searches the directory it runs in and below, and names what it
// found relative to it. A review run from a subdirectory, or a context.toml
// that lives in one, must still find the callers in the rest of the
// repository, and still line them up with the diff's paths.
func TestCallersSearchTheWholeCheckoutNotJustWhereTheReviewRan(t *testing.T) {
	repo := gitRepo(t, "https://github.com/"+testRepo)
	for _, pkg := range []string{"pkgA", "pkgB"} {
		if err := os.MkdirAll(filepath.Join(repo, pkg), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, repo, filepath.Join("pkgA", "widget.go"), "package a\n\nfunc Widget() {}\n")
	writeFile(t, repo, filepath.Join("pkgB", "user.go"), "package b\n\nfunc use() { a.Widget() }\n")
	writeFile(t, repo, filepath.Join("pkgA", ProjectFile), "")
	runGit(t, repo, "add", ".")

	sub := filepath.Join(repo, "pkgA")
	project, err := LoadProject(filepath.Join(sub, ProjectFile))
	if err != nil {
		t.Fatal(err)
	}
	f := sampleGH()
	f.diff = "diff --git a/pkgA/widget.go b/pkgA/widget.go\n--- a/pkgA/widget.go\n+++ b/pkgA/widget.go\n@@ -1 +1,2 @@\n+func Widget() {}\n"
	bundle := gatherTest(t, f, project, sub)

	items := bundle.Of(SourceCallers)
	if len(items) != 1 || items[0].Name != "Widget" {
		t.Fatalf("callers %+v, want the changed symbol", items)
	}
	if !strings.Contains(items[0].Content, "pkgB/user.go:3:") {
		t.Fatalf("callers content %q, want the caller outside the directory the review ran in", items[0].Content)
	}
	// The paths git grep reports have to be the diff's, or the file the
	// diff changed is not recognised as one and comes back as its own
	// caller.
	if strings.Contains(items[0].Content, "widget.go") {
		t.Fatalf("callers content %q, want the changed file left out", items[0].Content)
	}
}

func TestCallersOutsideAGitCheckoutGathersNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "spin.go", "package a\n\nfunc spin() { Widget() }\n")
	bundle := gatherTest(t, sampleGH(), nil, dir)
	if got := bundle.Of(SourceCallers); len(got) != 0 {
		t.Fatalf("callers %+v, want none outside a checkout", got)
	}
	if !strings.Contains(strings.Join(bundle.Skipped, "\n"), "is not a git checkout") {
		t.Fatalf("skipped %v, want the directory that is not a checkout", bundle.Skipped)
	}
}

func TestCallersStopAtTheCaps(t *testing.T) {
	dir := gitRepo(t, "https://github.com/"+testRepo)
	var diff, callers strings.Builder
	diff.WriteString("--- a/widget.go\n+++ b/widget.go\n")
	for i := range maxCallerSymbols + 2 {
		fmt.Fprintf(&diff, "+func Sym%d() {}\n", i)
	}
	for i := range maxCallerLines + 5 {
		fmt.Fprintf(&callers, "// Sym0 line %d\n", i)
	}
	// A symbol past the cap: its callers are there to be found and the cap
	// is what keeps them out of the bundle.
	fmt.Fprintf(&callers, "// Sym%d line\n", maxCallerSymbols+1)
	writeFile(t, dir, "callers.go", callers.String())
	runGit(t, dir, "add", ".")

	f := sampleGH()
	f.diff = diff.String()
	bundle := gatherTest(t, f, nil, dir)
	if got := bundle.Of(SourceCallers); len(got) != 1 {
		t.Fatalf("callers %d items, want the one symbol that has any", len(got))
	} else if lines := strings.Count(got[0].Content, "\n"); lines != maxCallerLines+1 {
		t.Fatalf("%d lines, want %d and the line saying how many were left out", lines, maxCallerLines+1)
	} else if !strings.Contains(got[0].Content, "and 5 more lines naming Sym0") {
		t.Fatalf("content ends %q", got[0].Content[len(got[0].Content)-80:])
	}
	if !strings.Contains(strings.Join(bundle.Skipped, "\n"), "callers of the last 2 were not looked for") {
		t.Fatalf("skipped %v, want the symbols the cap left out", bundle.Skipped)
	}
}

func TestChangedSymbols(t *testing.T) {
	diff := strings.Join([]string{
		"--- a/widget.go", "+++ b/widget.go",
		"+func Widget() {}",
		"-func (w *Widget) Spin() {}",
		"+type Gear struct{}",
		"+	const MaxGear = 3",
		"+def spin(self):",
		"+export function mount() {}",
		"+pub struct Cog;",
		"+export const Ratio = 2",
		"+	x := Widget()",
		" func Untouched() {}",
		"+func Widget() {}",
	}, "\n")
	want := []string{"Widget", "Spin", "Gear", "MaxGear", "spin", "mount", "Cog", "Ratio"}
	if got := changedSymbols(diff); !reflect.DeepEqual(got, want) {
		t.Fatalf("changedSymbols = %v, want %v", got, want)
	}
	if got := changedFiles(diff); !reflect.DeepEqual(got, map[string]bool{"widget.go": true}) {
		t.Fatalf("changedFiles = %v", got)
	}
}

func TestTheDiffIsReadOnce(t *testing.T) {
	f := sampleGH()
	dir := gitRepo(t, "https://github.com/"+testRepo)
	runGit(t, dir, "add", ".")
	gatherTest(t, f, nil, dir)
	diffs := 0
	for _, call := range f.calls {
		if call == "pr diff" {
			diffs++
		}
	}
	if diffs != 1 {
		t.Fatalf("gh pr diff ran %d times, want once for the diff source and the callers source together", diffs)
	}
}

func TestInputRootIsTheProjectDirectoryWhenThereIsAContextFile(t *testing.T) {
	in := &Input{Dir: "/checkout", Project: &Project{}}
	if got := in.Root(); got != "/checkout" {
		t.Fatalf("root %q, want the checkout when there is no context.toml", got)
	}
	in.Project = &Project{Path: filepath.Join("/checkout", "sub", ProjectFile), Loaded: true}
	if got, want := in.Root(), filepath.Join("/checkout", "sub"); got != want {
		t.Fatalf("root %q, want %q: paths in context.toml are relative to it", got, want)
	}
}
