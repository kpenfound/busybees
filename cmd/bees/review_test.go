package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/review"
)

// runReview runs `bees review ...` from an empty directory with no bees.toml
// and no git repository: the review tool reads neither. It returns what the
// command printed to stdout, what cobra wrote (help and usage), and the
// error.
func runReview(t *testing.T, args ...string) (stdout, cobraOut string, err error) {
	t.Helper()
	return runReviewWith(t, "", args...)
}

// runReviewWith is runReview with input typed at the command.
func runReviewWith(t *testing.T, input string, args ...string) (stdout, cobraOut string, err error) {
	t.Helper()
	return runReviewIn(t, t.TempDir(), input, args...)
}

// runReviewIn is runReviewWith run from dir.
func runReviewIn(t *testing.T, dir, input string, args ...string) (stdout, cobraOut string, err error) {
	t.Helper()
	t.Chdir(dir)
	t.Setenv("BEES_CONFIG", filepath.Join(dir, "missing.toml"))
	var out bytes.Buffer
	stdout = captureStdout(t, func() {
		root := newRoot()
		root.SetArgs(append([]string{"review"}, args...))
		root.SetIn(strings.NewReader(input))
		root.SetOut(&out)
		root.SetErr(&out)
		err = root.Execute()
	})
	return stdout, out.String(), err
}

// reviewHome points the global configuration at a directory of its own and
// returns it: with no config.toml in it, the reviewer notes and the review
// artifacts land there.
func reviewHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	return filepath.Join(home, "bees")
}

// storedReview writes a judged review of acme/widgets#7 under home, with two
// findings and a style session to reopen, and returns its directory.
func storedReview(t *testing.T, home string) string {
	t.Helper()
	ref := review.Ref{Repo: "acme/widgets", Number: 7}
	dir := review.ArtifactDir(filepath.Join(home, review.DefaultStoragePath), ref, time.Date(2026, 9, 10, 15, 4, 5, 0, time.UTC))
	a := &review.Artifact{
		Dir:   dir,
		Brief: &review.Brief{Ref: ref, Summary: "gathers the context"},
		Runs:  []review.AngleRun{{Angle: review.AngleStyle, Provider: "claude", Dir: t.TempDir(), SessionID: "sess-style"}},
		Findings: &review.Findings{Items: []review.Finding{
			{ID: "aaaa0001", Angle: review.AngleStyle, Category: "naming", Severity: review.SeverityMedium, File: "widget.go", Lines: review.LineRange{Start: 3, End: 3}, Side: review.SideNew, Title: "The receiver is named after its type", Body: "w, not widget"},
			{ID: "aaaa0002", Angle: review.AngleStyle, Category: "docs", Severity: review.SeverityLow, Title: "The package has no doc comment", Body: "every package here has one"},
		}},
	}
	if err := a.Write(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReviewTriageOpensTheLatestReviewAndPicksUpWhereItStopped(t *testing.T) {
	home := reviewHome(t)
	dir := storedReview(t, home)
	stdout, _, err := runReviewWith(t, "s\nq\n", "triage", "acme/widgets#7")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"acme/widgets#7: the review started 20260910-150405\n",
		"[1 of 2 findings undecided] aaaa0001 · medium · style · naming\n",
		"1 selected, 0 dismissed, 0 deferred, 1 undecided of 2 findings\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("triage did not print %q:\n%s", want, stdout)
		}
	}
	// Run again: only what is undecided is offered.
	stdout, _, err = runReviewWith(t, "f\n", "triage", "https://github.com/acme/widgets/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "aaaa0001 ·") || !strings.Contains(stdout, "[1 of 1 finding undecided] aaaa0002 ·") {
		t.Errorf("the second run did not pick up where the first stopped:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 selected, 0 dismissed, 1 deferred, 0 undecided of 2 findings\n") {
		t.Errorf("the second run's summary is wrong:\n%s", stdout)
	}
	tr, err := review.ReadTriage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Decisions) != 2 || tr.Decisions[0].Action != review.ActionSelect || tr.Decisions[1].Action != review.ActionDefer {
		t.Errorf("triage.json holds %+v", tr.Decisions)
	}
}

func TestReviewTriageRecordsADismissalInTheConfiguredNotes(t *testing.T) {
	home := reviewHome(t)
	storedReview(t, home)
	if _, _, err := runReviewWith(t, "d\nthe type is one letter here\nq\n", "triage", "acme/widgets#7"); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(home, review.DefaultNotesPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notes), "- [acme/widgets] [style] [naming] the type is one letter here\n") {
		t.Errorf("the notes do not hold the dismissal:\n%s", notes)
	}
}

func TestReviewTriageEditsTheCommentInTheEditor(t *testing.T) {
	home := reviewHome(t)
	dir := storedReview(t, home)
	editor := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf '\\nEdited by the script.\\n' >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// $VISUAL before $EDITOR, as everywhere.
	t.Setenv("VISUAL", editor)
	t.Setenv("EDITOR", "false")
	if _, _, err := runReviewWith(t, "e\nq\n", "triage", "acme/widgets#7"); err != nil {
		t.Fatal(err)
	}
	tr, err := review.ReadTriage(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "The receiver is named after its type\n\nw, not widget\n\nEdited by the script."
	if len(tr.Decisions) != 1 || tr.Decisions[0].Action != review.ActionSelect || tr.Decisions[0].Comment != want {
		t.Errorf("triage.json holds %+v, want the selection with the edited text %q", tr.Decisions, want)
	}
	// An editor that fails selects nothing.
	t.Setenv("VISUAL", "")
	stdout, _, err := runReviewWith(t, "e\nq\n", "triage", "acme/widgets#7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "the editor failed: false: exit status 1\n") {
		t.Errorf("triage did not say the editor failed:\n%s", stdout)
	}
}

func TestReviewTriageAsksThroughTheConfiguredAgent(t *testing.T) {
	home := reviewHome(t)
	storedReview(t, home)
	// No claude on the PATH: the ask fails on the agent the configuration
	// names, which is the one the queue was wired with.
	t.Setenv("PATH", t.TempDir())
	stdout, _, err := runReviewWith(t, "a\nIs that the convention here?\nq\n", "triage", "acme/widgets#7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "ask failed: ") || !strings.Contains(stdout, "claude") {
		t.Errorf("triage did not try the configured agent:\n%s", stdout)
	}
}

func TestReviewTriageWithoutAReviewSaysSo(t *testing.T) {
	reviewHome(t)
	_, _, err := runReview(t, "triage", "acme/widgets#7")
	if err == nil || !strings.Contains(err.Error(), "no review of acme/widgets#7") {
		t.Errorf("err = %v, want the missing review named", err)
	}
	// A bare number outside a checkout has no repository to look under.
	_, _, err = runReview(t, "triage", "7")
	if err == nil || !strings.Contains(err.Error(), "owner/name#7") {
		t.Errorf("err = %v, want the form to give", err)
	}
	if _, _, err := runReview(t, "triage"); err == nil {
		t.Error("triage ran without a pull request")
	}
}

func TestReviewTriageReadsTheConfigurationGiven(t *testing.T) {
	reviewHome(t)
	// A configuration elsewhere, whose storage path is where the review is.
	elsewhere := t.TempDir()
	config := filepath.Join(elsewhere, review.ConfigFile)
	if err := os.WriteFile(config, []byte("storage_path = \"kept\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := review.Ref{Repo: "acme/widgets", Number: 7}
	a := &review.Artifact{
		Dir:      review.ArtifactDir(filepath.Join(elsewhere, "kept"), ref, time.Date(2026, 9, 10, 15, 4, 5, 0, time.UTC)),
		Brief:    &review.Brief{Ref: ref, Summary: "gathers the context"},
		Findings: &review.Findings{Items: []review.Finding{{ID: "aaaa0003", Angle: review.AngleStyle, Category: "docs", Severity: review.SeverityLow, Title: "The package has no doc comment", Body: "every package here has one"}}},
	}
	if err := a.Write(); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runReviewWith(t, "q\n", "triage", "acme/widgets#7", "--config", config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "aaaa0003 ·") {
		t.Errorf("the review under the given configuration's storage path was not opened:\n%s", stdout)
	}
}

func TestReviewTriageReadsNoContextTomlOutsideACheckoutOfTheRepository(t *testing.T) {
	home := reviewHome(t)
	storedReview(t, home)
	// A context.toml that would not load, in a directory that is not a
	// checkout of acme/widgets: not this review's, and not read.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, review.ProjectFile), []byte("bogus = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runReviewIn(t, dir, "q\n", "triage", "acme/widgets#7")
	if err != nil {
		t.Fatalf("the context.toml of an unrelated directory was read: %v", err)
	}
	if !strings.Contains(stdout, "2 undecided of 2 findings") {
		t.Errorf("triage did not run:\n%s", stdout)
	}
}

// dismissals is a reviewer notes file holding one dismissal per reason, all
// of them from the style angle of one repository.
func dismissals(t *testing.T, reasons ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reviewer-notes.md")
	for _, reason := range reasons {
		d := review.Dismissal{Repo: "acme/widgets", Angle: "style", Category: "naming", Reason: reason}
		if err := review.AppendDismissal(path, d); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestReviewConsolidateWritesTheRulesIntoTheNotes(t *testing.T) {
	notes := dismissals(t,
		"receiver names are short here",
		"receiver names here are short",
		"short receiver names are fine here",
		"nobody calls that function",
	)
	stdout, _, err := runReview(t, "consolidate", "--notes", notes)
	if err != nil {
		t.Fatal(err)
	}
	rule := "- [acme/widgets] [style] [naming] drop: receiver names are short here (3 dismissals)"
	for _, want := range []string{notes, "4 dismissals", "added:", rule} {
		if !strings.Contains(stdout, want) {
			t.Errorf("consolidate did not report %q:\n%s", want, stdout)
		}
	}
	body, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), rule+"\n") {
		t.Errorf("the rule was not written into the notes:\n%s", body)
	}
	// The dismissal dismissed once is no pattern, and no rule was made of
	// it.
	if !strings.Contains(string(body), "nobody calls that function\n") {
		t.Errorf("the dismissals were not left alone:\n%s", body)
	}
	if strings.Contains(string(body), "downrank") {
		t.Errorf("a single dismissal became a rule:\n%s", body)
	}

	// Again: the same dismissals are the same rule, and the file does not
	// grow.
	before := string(body)
	stdout, _, err = runReview(t, "consolidate", "--notes", notes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "nothing to consolidate") {
		t.Errorf("a second consolidation reported:\n%s", stdout)
	}
	if after, err := os.ReadFile(notes); err != nil || string(after) != before {
		t.Errorf("a second consolidation rewrote the notes:\n%s", after)
	}

	// One more dismissal of the pattern already ruled on: the rule is the
	// same rule, and its count moves.
	if err := review.AppendDismissal(notes, review.Dismissal{
		Repo: "acme/widgets", Angle: "style", Category: "naming", Reason: "here receiver names are short",
	}); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runReview(t, "consolidate", "--notes", notes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "added:") {
		t.Errorf("a dismissal of a pattern already ruled on added a rule:\n%s", stdout)
	}
	refreshed := "- [acme/widgets] [style] [naming] drop: receiver names are short here (4 dismissals)"
	if !strings.Contains(stdout, "refreshed:") || !strings.Contains(stdout, refreshed) {
		t.Errorf("consolidate did not report %q as refreshed:\n%s", refreshed, stdout)
	}
	if body, err := os.ReadFile(notes); err != nil || !strings.Contains(string(body), refreshed+"\n") || strings.Contains(string(body), rule+"\n") {
		t.Errorf("the refreshed rule was not written into the notes:\n%s", body)
	}
}

func TestReviewConsolidateDryRunWritesNothing(t *testing.T) {
	notes := dismissals(t, "receiver names are short here", "receiver names here are short")
	before, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runReview(t, "consolidate", "--notes", notes, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "downrank: receiver names are short here (2 dismissals)") {
		t.Errorf("--dry-run printed no rule:\n%s", stdout)
	}
	after, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("--dry-run wrote to the notes:\n%s", after)
	}
}

func TestReviewConsolidateReadsTheNotesPathFromTheGlobalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	notes := dismissals(t, "receiver names are short here", "receiver names here are short")
	if err := os.MkdirAll(filepath.Join(home, "bees"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "notes_path = " + strconv.Quote(notes) + "\n"
	if err := os.WriteFile(filepath.Join(home, "bees", review.ConfigFile), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runReview(t, "consolidate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, notes) || !strings.Contains(stdout, "added:") {
		t.Errorf("consolidate did not work on the configured notes (%s):\n%s", notes, stdout)
	}
}

func TestReviewConsolidateWithoutNotesSaysSo(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "reviewer-notes.md")
	stdout, _, err := runReview(t, "consolidate", "--notes", missing)
	if err != nil {
		t.Fatalf("a person who has dismissed nothing is not an error: %v", err)
	}
	if !strings.Contains(stdout, "no reviewer notes yet") || !strings.Contains(stdout, missing) {
		t.Errorf("consolidate with no notes printed:\n%s", stdout)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("consolidate created the notes it had nothing to write into")
	}
}

func TestReviewIsAGroupOfSubcommands(t *testing.T) {
	_, cobraOut, err := runReview(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cobraOut, "consolidate") {
		t.Errorf("`bees review` does not list its subcommands:\n%s", cobraOut)
	}
	if _, _, err := runReview(t, "nonesuch"); err == nil {
		t.Error("an unknown review subcommand is not an error")
	}
}

func TestReviewConsolidateReportsWhatItDidNotReadAsARule(t *testing.T) {
	notes := dismissals(t, "receiver names are short here", "receiver names here are short")
	// The markers are the notes file's format (internal/review): what is
	// between them is rules, and a line there that is not one is a person's
	// to fix, so consolidation says it saw it and leaves it alone.
	const typo = "- [acme/widgets] [style] [naming] dropp: a misspelt action"
	body, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	const end = "<!-- /bees:review:rules -->"
	if !strings.Contains(string(body), end) {
		t.Fatalf("the notes hold no rules block:\n%s", body)
	}
	patched := strings.Replace(string(body), end, typo+"\n"+end, 1)
	if err := os.WriteFile(notes, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runReview(t, "consolidate", "--notes", notes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 line in the rules block did not read as a rule", typo} {
		if !strings.Contains(stdout, want) {
			t.Errorf("consolidate did not report %q:\n%s", want, stdout)
		}
	}
	after, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), typo+"\n") {
		t.Errorf("the line consolidation could not read is gone:\n%s", after)
	}
}

func TestReviewRunsThePipelineFromTheContextGather(t *testing.T) {
	home := reviewHome(t)
	// No gh on the PATH: the review fails at the first thing the pipeline
	// does, reading the pull request, and nothing is written.
	t.Setenv("PATH", t.TempDir())
	stdout, _, err := runReview(t, "acme/widgets#7")
	if err == nil || !strings.Contains(err.Error(), "read acme/widgets#7") || !strings.Contains(err.Error(), "gh") {
		t.Errorf("err = %v, want the gather that could not read the pull request through gh", err)
	}
	if !strings.Contains(stdout, "gathering the context of acme/widgets#7\n") {
		t.Errorf("the review did not say what it was doing:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(home, review.DefaultStoragePath)); !os.IsNotExist(err) {
		t.Errorf("something was written under the storage path: %v", err)
	}
	// A bare number outside a checkout has no repository, as for triage.
	if _, _, err := runReview(t, "7"); err == nil || !strings.Contains(err.Error(), "owner/name#7") {
		t.Errorf("err = %v, want the form to give", err)
	}
}

func TestReviewEndFlagsAreCheckedBeforeAnythingRuns(t *testing.T) {
	reviewHome(t)
	t.Setenv("PATH", t.TempDir())
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"acme/widgets#7", "--post", "approved"}, `--post "approved": want one of approve, comment, reject`},
		{[]string{"acme/widgets#7", "--post", "approve", "--report"}, "--post and --report choose different ends"},
		{[]string{"triage", "acme/widgets#7", "--post", "report"}, `--post "report": want one of approve, comment, reject`},
		{[]string{"triage", "acme/widgets#7", "--report", "--post", "comment"}, "--post and --report choose different ends"},
	} {
		stdout, _, err := runReview(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
		if stdout != "" {
			t.Errorf("%v ran something first:\n%s", c.args, stdout)
		}
	}
}

func TestReviewTriageEndsWithTheReport(t *testing.T) {
	home := reviewHome(t)
	dir := storedReview(t, home)
	// --report: the selected finding is printed as markdown, and no gh is
	// needed for it.
	t.Setenv("PATH", t.TempDir())
	stdout, _, err := runReviewWith(t, "s\nf\n", "triage", "acme/widgets#7", "--report")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1 selected, 0 dismissed, 1 deferred, 0 undecided of 2 findings\n",
		"# Review of acme/widgets#7\n\nhttps://github.com/acme/widgets/pull/7\n\n",
		"`widget.go:3` · medium · naming\n\nThe receiver is named after its type\n\nw, not widget\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report lacks %q:\n%s", want, stdout)
		}
	}
	_, report, _ := strings.Cut(stdout, "# Review of")
	if strings.Contains(report, "The package has no doc comment") || strings.Contains(stdout, "selected. a approve") {
		t.Errorf("the report holds the deferred finding, or the end was asked for:\n%s", stdout)
	}
	// The output key of the configuration chooses when no flag does.
	if err := os.WriteFile(filepath.Join(home, review.ConfigFile), []byte("output = \"report\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runReviewWith(t, "", "triage", "acme/widgets#7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "# Review of acme/widgets#7\n") || strings.Contains(stdout, "selected. a approve") {
		t.Errorf("output = \"report\" did not print the report unasked:\n%s", stdout)
	}
	// The decisions are what the artifact holds, whatever the end.
	tr, err := review.ReadTriage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Decisions) != 2 {
		t.Errorf("triage.json holds %+v", tr.Decisions)
	}
}

func TestReviewTriageAsksHowToEndAndPostsThroughGh(t *testing.T) {
	home := reviewHome(t)
	dir := storedReview(t, home)
	t.Setenv("PATH", t.TempDir())
	// A comment-only review with nothing selected, asked for by a flag, is
	// the command's error: nothing to ask again at, and no gh call.
	stdout, _, err := runReviewWith(t, "q\n", "triage", "acme/widgets#7", "--post", "comment")
	if err == nil || !strings.Contains(err.Error(), "nothing was selected") {
		t.Errorf("--post comment with nothing selected: err = %v", err)
	}
	if strings.Contains(stdout, "selected. a approve") {
		t.Errorf("a flag's refusal was asked about:\n%s", stdout)
	}
	// With nothing chosen, the console asks at the end. A comment-only
	// review with nothing selected is refused, with no gh call, and asked
	// again; d discards, and says so.
	stdout, _, err = runReviewWith(t, "q\nc\nd\n", "triage", "acme/widgets#7")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n0 findings selected. a approve and comment · c comment only · r reject and comment · o output the report · d discard\n> ",
		"nothing was selected, and a comment-only review with nothing in it cannot be posted\n",
		"acme/widgets#7: nothing posted\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Count(stdout, "selected. a approve") != 2 {
		t.Errorf("the end was asked %d times, want twice:\n%s", strings.Count(stdout, "selected. a approve"), stdout)
	}
	// o prints the report.
	stdout, _, err = runReviewWith(t, "s\nq\no\n", "triage", "acme/widgets#7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "\n1 finding selected. a approve") || !strings.Contains(stdout, "# Review of acme/widgets#7\n") {
		t.Errorf("the end was not asked, or the report not printed:\n%s", stdout)
	}
	// An end that posts goes through gh, which is not here: the command
	// fails naming it, and the selection made before is still in the
	// artifact.
	_, _, err = runReviewWith(t, "q\na\n", "triage", "acme/widgets#7")
	if err == nil || !strings.Contains(err.Error(), "gh") {
		t.Errorf("err = %v, want the gh call that could not run", err)
	}
	_, _, err = runReviewWith(t, "", "triage", "acme/widgets#7", "--post", "reject")
	if err == nil || !strings.Contains(err.Error(), "gh") {
		t.Errorf("--post reject: err = %v, want the gh call that could not run", err)
	}
	tr, err := review.ReadTriage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Decisions) != 1 || tr.Decisions[0].Action != review.ActionSelect {
		t.Errorf("triage.json holds %+v, want the one selection", tr.Decisions)
	}
}
