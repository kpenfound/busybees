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
