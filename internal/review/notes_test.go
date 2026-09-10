package review

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// notesAt is an empty reviewer notes path in a directory of its own, which
// is what a person who has dismissed nothing has.
func notesAt(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "notes", DefaultNotesPath)
}

// readFile is the notes file as it stands, for the assertions about what a
// write left in it.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// dismiss appends one dismissal of testRepo, from the style angle, and fails
// the test when it cannot.
func dismiss(t *testing.T, path, angle, category, reason string) {
	t.Helper()
	if err := AppendDismissal(path, Dismissal{Repo: testRepo, Angle: angle, Category: category, Reason: reason}); err != nil {
		t.Fatal(err)
	}
}

func TestNotesThatAreNotThereAreNotAnError(t *testing.T) {
	path := notesAt(t)
	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if n.Loaded || n.Path != path || len(n.Rules) != 0 || len(n.Dismissals) != 0 {
		t.Errorf("notes of a file that is not there = %+v, want empty and not loaded", n)
	}
}

func TestADismissalIsAppendedToTheNotes(t *testing.T) {
	path := notesAt(t)
	f := testFinding()
	if err := AppendDismissal(path, DismissalOf(testRepo, &f, "the fixture covers it")); err != nil {
		t.Fatal(err)
	}
	dismiss(t, path, AngleStyle, "naming", "receiver names are short here")

	// The first dismissal creates the file, with the sections and the block
	// a consolidation writes into.
	body := readFile(t, path)
	for _, want := range []string{"# Reviewer notes\n", "## Rules\n", rulesBegin + "\n" + rulesEnd, "## Dismissals\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("the notes a first dismissal created hold no %q:\n%s", want, body)
		}
	}
	// The dismissal is the finding's repository, angle and category, and the
	// reason triage gave.
	if want := "- [acme/widgets] [test_coverage] [missing test] the fixture covers it\n"; !strings.Contains(body, want) {
		t.Errorf("the notes hold no %q:\n%s", want, body)
	}

	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if !n.Loaded {
		t.Error("notes that were written read as not loaded")
	}
	want := []Dismissal{
		{Repo: testRepo, Angle: AngleTests, Category: "missing test", Reason: "the fixture covers it"},
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Reason: "receiver names are short here"},
	}
	if !reflect.DeepEqual(n.Dismissals, want) {
		t.Errorf("dismissals = %+v, want %+v, oldest first", n.Dismissals, want)
	}
}

func TestADismissalGoesAtTheEndOfNotesThatAlreadyExist(t *testing.T) {
	path := notesAt(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file of a person's own: no template, no sections, and no closing
	// newline for the appended line to follow.
	if err := os.WriteFile(path, []byte("my own notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dismiss(t, path, AngleStyle, "naming", "receiver names are short here")

	want := "my own notes\n- [acme/widgets] [style] [naming] receiver names are short here\n"
	if got := readFile(t, path); got != want {
		t.Errorf("notes =\n%q\nwant\n%q", got, want)
	}
	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Dismissals) != 1 || len(n.Rules) != 0 {
		t.Errorf("notes = %+v, want the one dismissal and no rule", n)
	}
}

func TestRulesAreReadBetweenTheMarkersAndDismissalsAnywhere(t *testing.T) {
	path := notesAt(t)
	body := `# Reviewer notes

Prose about [something] a person wrote.

## Rules

` + rulesBegin + `
- [acme/widgets] [style] [naming] drop: receiver names are short here (4 dismissals)
- [*] [*] [*] downrank: whatever it is
[acme/widgets] [style] [naming] dropp: a misspelt action
- [acme/widgets] [style] [naming] a rule with no action at all
` + rulesEnd + `

## Dismissals

- [acme/widgets] [test_coverage] [missing test] the fixture covers it
[acme/widgets] [side_effects] [] no bullet, no category
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	wantRules := []Rule{
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "receiver names are short here", Count: 4},
		{Repo: anyValue, Angle: anyValue, Category: anyValue, Action: RuleDownrank, Text: "whatever it is"},
	}
	if !reflect.DeepEqual(n.Rules, wantRules) {
		t.Errorf("rules = %+v, want %+v", n.Rules, wantRules)
	}
	// A line in the block that is not a rule is kept as it is, not read as
	// one and not thrown away.
	wantSkipped := []string{
		"[acme/widgets] [style] [naming] dropp: a misspelt action",
		"- [acme/widgets] [style] [naming] a rule with no action at all",
	}
	if !reflect.DeepEqual(n.Skipped, wantSkipped) {
		t.Errorf("skipped = %q, want %q", n.Skipped, wantSkipped)
	}
	// Dismissals are read outside the block only, with or without a bullet;
	// the prose is not one.
	wantDismissals := []Dismissal{
		{Repo: testRepo, Angle: AngleTests, Category: "missing test", Reason: "the fixture covers it"},
		{Repo: testRepo, Angle: AngleSideEffects, Category: "", Reason: "no bullet, no category"},
	}
	if !reflect.DeepEqual(n.Dismissals, wantDismissals) {
		t.Errorf("dismissals = %+v, want %+v", n.Dismissals, wantDismissals)
	}
}

func TestARuleIsWrittenAsItIsRead(t *testing.T) {
	for _, r := range []Rule{
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "receiver names are short here", Count: 4},
		{Repo: anyValue, Angle: anyValue, Category: anyValue, Action: RuleDownrank, Text: "one dismissal only", Count: 1},
		{Repo: anyValue, Angle: anyValue, Category: anyValue, Action: RuleDownrank, Text: "no count at all"},
	} {
		got, ok := parseRule(r.Line())
		if !ok || !reflect.DeepEqual(got, r) {
			t.Errorf("parseRule(%q) = %+v, %v, want %+v", r.Line(), got, ok, r)
		}
	}
	// The count is written in the words a person reads, and singular for
	// one.
	if got := (Rule{Angle: AngleStyle, Action: RuleDrop, Text: "x", Count: 1}).Line(); got != "- [*] [style] [*] drop: x (1 dismissal)" {
		t.Errorf("rule line = %q", got)
	}
}

func TestConsolidateMakesARuleOfAPatternThatRepeats(t *testing.T) {
	path := notesAt(t)
	// Three dismissals of one pattern, worded differently; two of another;
	// one of a third.
	dismiss(t, path, AngleStyle, "naming", "receiver names are short here")
	dismiss(t, path, AngleStyle, "naming", "short receiver names are fine here")
	dismiss(t, path, AngleStyle, "naming", "receiver names here are short")
	dismiss(t, path, AngleTests, "missing test", "generated files carry no tests")
	dismiss(t, path, AngleTests, "missing test", "generated files carry no tests")
	dismiss(t, path, AngleSideEffects, "compatibility", "nobody else calls it")

	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	added, refreshed := n.Consolidate()
	want := []Rule{
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "receiver names are short here", Count: 3},
		{Repo: testRepo, Angle: AngleTests, Category: "missing test", Action: RuleDownrank, Text: "generated files carry no tests", Count: 2},
	}
	if !reflect.DeepEqual(added, want) {
		t.Errorf("added %+v, want %+v: dismissed %d times drops, %d ranks down, and once is no pattern", added, want, DropDismissals, MinDismissals)
	}
	if len(refreshed) != 0 {
		t.Errorf("refreshed %+v, want none: no rule was there to refresh", refreshed)
	}

	// Consolidating again writes nothing: the same dismissals are the same
	// rules with the same counts.
	if err := n.Write(); err != nil {
		t.Fatal(err)
	}
	again, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Rules, want) {
		t.Fatalf("the written rules read back as %+v, want %+v", again.Rules, want)
	}
	if added, refreshed := again.Consolidate(); len(added) != 0 || len(refreshed) != 0 {
		t.Errorf("a second consolidation added %+v and refreshed %+v, want neither", added, refreshed)
	}
}

func TestAPatternOfAnotherRepoAngleOrCategoryIsAnotherPattern(t *testing.T) {
	path := notesAt(t)
	// The same reason, four times, under four different headings: no
	// pattern repeats.
	dismiss(t, path, AngleStyle, "naming", "not how we write it")
	dismiss(t, path, AngleStyle, "wording", "not how we write it")
	dismiss(t, path, AngleTests, "naming", "not how we write it")
	if err := AppendDismissal(path, Dismissal{Repo: "acme/gadgets", Angle: AngleStyle, Category: "naming", Reason: "not how we write it"}); err != nil {
		t.Fatal(err)
	}
	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if added, _ := n.Consolidate(); len(added) != 0 {
		t.Errorf("added %+v, want none: four headings are four patterns of one dismissal", added)
	}
	// One more of the first, and that one alone is a rule.
	dismiss(t, path, AngleStyle, "naming", "not how we write it")
	if n, err = ReadNotes(path); err != nil {
		t.Fatal(err)
	}
	added, _ := n.Consolidate()
	if len(added) != 1 || added[0].Angle != AngleStyle || added[0].Category != "naming" || added[0].Repo != testRepo {
		t.Errorf("added %+v, want the one rule for %s/%s/naming", added, testRepo, AngleStyle)
	}
}

func TestConsolidateKeepsTheRuleAPersonWroteAndRefreshesItsCount(t *testing.T) {
	path := notesAt(t)
	for range 3 {
		dismiss(t, path, AngleStyle, "naming", "receiver names are short here")
	}
	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	// A person got there first: the same pattern, ranked down rather than
	// dropped, in their own words and with a count two dismissals old.
	n.Rules = []Rule{{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDownrank, Text: "short receiver names are the convention here", Count: 2}}

	added, refreshed := n.Consolidate()
	if len(added) != 0 {
		t.Errorf("added %+v, want none: the pattern is already ruled on", added)
	}
	want := []Rule{{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDownrank, Text: "short receiver names are the convention here", Count: 3}}
	if !reflect.DeepEqual(n.Rules, want) {
		t.Errorf("rules = %+v, want %+v: the action and the words stay, the count moves", n.Rules, want)
	}
	if !reflect.DeepEqual(refreshed, want) {
		t.Errorf("refreshed = %+v, want %+v", refreshed, want)
	}
}

func TestARuleWithNoTextRulesOnItsWholeCategory(t *testing.T) {
	path := notesAt(t)
	for range 2 {
		dismiss(t, path, AngleStyle, "naming", "receiver names are short here")
	}
	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	n.Rules = []Rule{{Repo: anyValue, Angle: AngleStyle, Category: "naming", Action: RuleDrop}}
	added, refreshed := n.Consolidate()
	if len(added) != 0 || len(refreshed) != 1 || refreshed[0].Count != 2 {
		t.Errorf("added %+v, refreshed %+v: a rule with no text is the rule for every pattern in its category", added, refreshed)
	}
}

func TestWritingTheRulesLeavesEveryOtherLineAlone(t *testing.T) {
	path := notesAt(t)
	dismiss(t, path, AngleStyle, "naming", "receiver names are short here")
	before := readFile(t, path)

	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	n.Rules = []Rule{{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "receiver names are short here", Count: 3}}
	n.Skipped = []string{"- a line of my own"}
	if err := n.Write(); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, path)
	block := rulesBegin + "\n" +
		"- [acme/widgets] [style] [naming] drop: receiver names are short here (3 dismissals)\n" +
		"- a line of my own\n" + rulesEnd
	if want := strings.Replace(before, rulesBegin+"\n"+rulesEnd, block, 1); got != want {
		t.Errorf("the written notes are\n%s\nwant\n%s", got, want)
	}
	// A line the write could not read as a rule is still not one when it is
	// read back.
	again, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Rules) != 1 || !reflect.DeepEqual(again.Skipped, n.Skipped) {
		t.Errorf("read back: rules %+v, skipped %q", again.Rules, again.Skipped)
	}
}

func TestNotesWithNoBlockGetOne(t *testing.T) {
	path := notesAt(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("my own notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	n.Rules = []Rule{{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "receiver names are short here"}}
	if err := n.Write(); err != nil {
		t.Fatal(err)
	}
	want := "my own notes\n\n" + rulesHeading + "\n\n" + rulesBegin + "\n" +
		"- [acme/widgets] [style] [naming] drop: receiver names are short here\n" + rulesEnd + "\n"
	if got := readFile(t, path); got != want {
		t.Errorf("notes =\n%q\nwant\n%q", got, want)
	}
}

func TestRulesForAreTheOnesThatNameTheReviewAndTheAngle(t *testing.T) {
	n := &Notes{Rules: []Rule{
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "here"},
		{Repo: "acme/gadgets", Angle: AngleStyle, Action: RuleDrop, Text: "another repository"},
		{Repo: anyValue, Angle: AngleTests, Action: RuleDrop, Text: "another angle"},
		{Angle: anyValue, Action: RuleDownrank, Text: "every repository, every angle"},
	}}
	got := n.RulesFor(testRepo, AngleStyle)
	if want := []string{"here", "every repository, every angle"}; len(got) != 2 || got[0].Text != want[0] || got[1].Text != want[1] {
		t.Errorf("RulesFor(%s, %s) = %+v, want the rules saying %q", testRepo, AngleStyle, got, want)
	}
}
