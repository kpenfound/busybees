package review

import (
	"reflect"
	"strings"
	"testing"
)

// noisy is a finding as the judge left it: anchored, with an id, a category
// and a title for a rule to match.
func noisy(angle, category, severity, file, title string) Finding {
	f := Finding{
		Angle:    angle,
		Category: category,
		Severity: severity,
		File:     file,
		Lines:    LineRange{12, 14},
		Side:     SideNew,
		Title:    title,
		Body:     "what is wrong with " + file + " and why it matters",
	}
	f.ID = findingID(&f)
	return f
}

// shortNames is the pattern a reviewer of testRepo has said no to before.
func shortNames(action string) Rule {
	return Rule{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: action, Text: "receiver names are short here", Count: 3}
}

func TestARuleDropsTheFindingsItIsAbout(t *testing.T) {
	findings := []Finding{
		noisy(AngleStyle, "naming", SeverityMedium, "widget.go", "Receiver names here are short"),
		noisy(AngleStyle, "naming", SeverityMedium, "gadget.go", "The package name says nothing"),
		noisy(AngleTests, "naming", SeverityMedium, "widget.go", "Receiver names here are short"),
	}
	kept, silenced := Filter(findings, []Rule{shortNames(RuleDrop)}, testRepo)
	if want := []string{"The package name says nothing", "Receiver names here are short"}; !reflect.DeepEqual(titles(kept), want) {
		t.Errorf("kept %q, want %q: only the finding the rule is about is dropped", titles(kept), want)
	}
	if kept[1].Angle != AngleTests {
		t.Errorf("the kept %q is from %s, want the angle the rule does not name", kept[1].Title, kept[1].Angle)
	}
	if len(silenced) != 1 {
		t.Fatalf("silenced %+v, want the one finding the rule dropped", silenced)
	}
	want := Silenced{
		ID:       findings[0].ID,
		Title:    "Receiver names here are short",
		Angle:    AngleStyle,
		Category: "naming",
		Action:   RuleDrop,
		Severity: SeverityMedium,
		Rule:     shortNames(RuleDrop).Line(),
	}
	if !reflect.DeepEqual(silenced[0], want) {
		t.Errorf("silenced = %+v, want %+v: what was hidden and the rule that hid it", silenced[0], want)
	}
}

func TestADownrankRuleLetsTheFindingThroughOneSeverityLower(t *testing.T) {
	findings := []Finding{
		noisy(AngleStyle, "naming", SeverityHigh, "widget.go", "Receiver names here are short"),
		noisy(AngleStyle, "wording", SeverityMedium, "gadget.go", "The comment repeats the code"),
	}
	kept, silenced := Filter(findings, []Rule{shortNames(RuleDownrank)}, testRepo)
	if len(kept) != 2 {
		t.Fatalf("kept %q, want both: downrank hides nothing", titles(kept))
	}
	// Ranked down from high to medium, and now behind the medium finding it
	// was ahead of.
	if kept[0].Title != "The comment repeats the code" || kept[1].Severity != SeverityMedium {
		t.Errorf("kept = %+v, want the ranked-down finding at medium and after the one it no longer outranks", kept)
	}
	if len(silenced) != 1 || silenced[0].Action != RuleDownrank || silenced[0].Severity != SeverityHigh {
		t.Errorf("silenced = %+v, want the finding recorded at the severity it had", silenced)
	}
}

func TestAFindingAlreadyAtInfoIsNotRankedBelowIt(t *testing.T) {
	findings := []Finding{noisy(AngleStyle, "naming", SeverityInfo, "widget.go", "Receiver names here are short")}
	kept, silenced := Filter(findings, []Rule{shortNames(RuleDownrank)}, testRepo)
	if len(kept) != 1 || kept[0].Severity != SeverityInfo {
		t.Errorf("kept = %+v, want the finding still there, at info", kept)
	}
	if len(silenced) != 1 {
		t.Errorf("silenced = %+v, want the rule recorded even where it changed nothing", silenced)
	}
}

func TestARuleWithNoTextSilencesItsWholeCategory(t *testing.T) {
	findings := []Finding{
		noisy(AngleStyle, "naming", SeverityHigh, "widget.go", "Receiver names here are short"),
		noisy(AngleStyle, "naming", SeverityHigh, "gadget.go", "The package name says nothing"),
		noisy(AngleStyle, "wording", SeverityHigh, "gadget.go", "The comment repeats the code"),
	}
	rule := Rule{Repo: anyValue, Angle: AngleStyle, Category: "naming", Action: RuleDrop}
	kept, silenced := Filter(findings, []Rule{rule}, testRepo)
	if want := []string{"The comment repeats the code"}; !reflect.DeepEqual(titles(kept), want) {
		t.Errorf("kept %q, want %q: a rule with no text is about every finding in its category", titles(kept), want)
	}
	if len(silenced) != 2 {
		t.Errorf("silenced %+v, want both naming findings", silenced)
	}
}

func TestARuleOfAnotherRepositoryOrAngleIsNotAboutThisFinding(t *testing.T) {
	findings := []Finding{noisy(AngleStyle, "naming", SeverityHigh, "widget.go", "Receiver names here are short")}
	for _, rule := range []Rule{
		{Repo: "acme/gadgets", Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "receiver names are short here"},
		{Repo: testRepo, Angle: AngleTests, Category: "naming", Action: RuleDrop, Text: "receiver names are short here"},
		{Repo: testRepo, Angle: AngleStyle, Category: "wording", Action: RuleDrop, Text: "receiver names are short here"},
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: "the package name says nothing"},
	} {
		kept, silenced := Filter(findings, []Rule{rule}, testRepo)
		if len(kept) != 1 || len(silenced) != 0 {
			t.Errorf("rule %q silenced a finding it is not about: kept %+v, silenced %+v", rule.Line(), kept, silenced)
		}
	}
	// The one that is about it, whatever case its fields are written in.
	rule := Rule{Repo: "ACME/Widgets", Angle: AngleStyle, Category: "Naming", Action: RuleDrop, Text: "receiver names are short here"}
	if kept, silenced := Filter(findings, []Rule{rule}, testRepo); len(kept) != 0 || len(silenced) != 1 {
		t.Errorf("rule %q is about the finding: kept %+v, silenced %+v", rule.Line(), kept, silenced)
	}
}

func TestADropRuleActsWhereverItIsWritten(t *testing.T) {
	findings := []Finding{noisy(AngleStyle, "naming", SeverityHigh, "widget.go", "Receiver names here are short")}
	drop := shortNames(RuleDrop)
	downrank := Rule{Repo: anyValue, Angle: anyValue, Category: anyValue, Action: RuleDownrank}
	for _, rules := range [][]Rule{{drop, downrank}, {downrank, drop}} {
		kept, silenced := Filter(findings, rules, testRepo)
		if len(kept) != 0 || len(silenced) != 1 || silenced[0].Action != RuleDrop {
			t.Errorf("rules %q then %q: kept %+v, silenced %+v, want the drop to act either way",
				rules[0].Line(), rules[1].Line(), kept, silenced)
		}
	}
}

func TestFindingsAreWhatTheyWereWithoutARuleAboutThem(t *testing.T) {
	findings := []Finding{
		noisy(AngleStyle, "naming", SeverityHigh, "widget.go", "Receiver names here are short"),
		noisy(AngleTests, "missing test", SeverityLow, "gadget.go", "Gather has no test"),
	}
	for _, rules := range [][]Rule{nil, {{Repo: "acme/gadgets", Angle: anyValue, Category: anyValue, Action: RuleDrop}}} {
		kept, silenced := Filter(findings, rules, testRepo)
		if !reflect.DeepEqual(kept, findings) || silenced != nil {
			t.Errorf("with rules %+v: kept %+v and silenced %+v, want the findings as they were", rules, kept, silenced)
		}
	}
}

func TestAnAngleIsToldTheRulesThatNameIt(t *testing.T) {
	rules := []Rule{
		shortNames(RuleDrop),
		{Repo: anyValue, Angle: anyValue, Category: anyValue, Action: RuleDownrank, Text: "every angle hears this one"},
		{Repo: "acme/gadgets", Angle: AngleStyle, Action: RuleDrop, Text: "another repository"},
		{Repo: testRepo, Angle: AngleTests, Action: RuleDrop, Text: "another angle"},
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop},
	}
	got := noiseSection(rules, testRepo, AngleStyle)
	for _, want := range []string{
		"## Dismissed before\n",
		"- receiver names are short here\n",
		"- every angle hears this one\n",
		"Do not report one of them again unless this change makes it newly wrong",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the style angle is not told %q:\n%s", want, got)
		}
	}
	// The rules reach the session as what was dismissed, not as the lines
	// the notes file writes them as.
	for _, notWant := range []string{"another repository", "another angle", "[" + testRepo + "]", "drop:"} {
		if strings.Contains(got, notWant) {
			t.Errorf("the style angle is told %q, which is not its business:\n%s", notWant, got)
		}
	}
	// Nothing to say is nothing said, not an empty heading.
	if got := noiseSection(rules, testRepo, AngleSideEffects); !strings.Contains(got, "every angle hears this one") {
		t.Errorf("side effects: %q", got)
	}
	if got := noiseSection(rules[3:], testRepo, AngleStyle); got != "" {
		t.Errorf("with no rule for the angle the section is %q, want nothing", got)
	}
	if got := noiseSection(nil, testRepo, AngleStyle); got != "" {
		t.Errorf("with no rules at all the section is %q, want nothing", got)
	}
}
