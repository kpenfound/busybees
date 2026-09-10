package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/review"
)

// runReview runs `bees review ...` from an empty directory with no bees.toml
// and no git repository: the review tool reads neither. It returns what the
// command printed to stdout, what cobra wrote (help and usage), and the
// error.
func runReview(t *testing.T, args ...string) (stdout, cobraOut string, err error) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("BEES_CONFIG", filepath.Join(dir, "missing.toml"))
	var out bytes.Buffer
	stdout = captureStdout(t, func() {
		root := newRoot()
		root.SetArgs(append([]string{"review"}, args...))
		root.SetOut(&out)
		root.SetErr(&out)
		err = root.Execute()
	})
	return stdout, out.String(), err
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
