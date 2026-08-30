package prompts

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
)

func sample() Data {
	return Data{
		Project: config.Project{Repo: "acme/widgets", DefaultBranch: "main", Remote: "origin"},
		Filter:  config.Filter{Label: "bees", Assignee: "kyle"},
		Labels:  config.LabelsFor("bees"),
		WorkDir: "/tmp/ws", Branch: "bees/issue-4", StateDir: "/s", SessionDir: "/s/sessions/1", NotesFile: "/s/notes/x.md",
		Notes:         "remember this",
		Inbox:         []mail.Message{{ID: "m1", From: "reviewer", To: "developer", Subject: "Review round 1", Body: "please fix", PR: 9, CreatedAt: time.Now()}},
		Issue:         &github.Issue{Number: 4, Title: "Add thing", Body: "details", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:feature"}}, Author: github.Author{Login: "kyle"}},
		PR:            &github.PR{Number: 9, Title: "Add thing", HeadRefName: "bees/issue-4", BaseRefName: "main", Author: github.Author{Login: "bot"}},
		Issues:        []github.Issue{{Number: 5, Title: "Other", Labels: []github.Label{{Name: "bees:triage"}, {Name: "bees:bug"}}}},
		TriageIssues:  []github.Issue{{Number: 5, Title: "Other", Body: "b"}},
		MergedPRs:     []github.PR{{Number: 8, Title: "Merged", Body: "x"}},
		Milestones:    []github.Milestone{{Number: 1, Title: "v1", Description: "first\nrelease"}},
		Features:      []github.Issue{{Number: 12, Title: "Exports", Labels: []github.Label{{Name: "bees:feature"}, {Name: "bees:question"}}}},
		Progress:      map[int]github.SubIssueSummary{12: {Total: 4, Completed: 2}},
		Parent:        &github.Parent{Number: 12, Title: "Exports"},
		Parents:       map[int]github.Parent{5: {Number: 12, Title: "Exports"}},
		Blockers:      map[int][]int{5: {37}},
		FreshFeatures: []github.Issue{{Number: 13, Title: "Search", Body: "find things", Author: github.Author{Login: "kyle"}}},
		Feedback:      []github.Issue{{Number: 9, Title: "Dark mode please", Body: "would be nice", Author: github.Author{Login: "kyle"}, Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "also on mobile"}}}},
		MaxSize:       "l",
		Round:         1, MaxRounds: 3,
	}
}

func TestRenderAllRoles(t *testing.T) {
	for _, role := range config.Roles {
		sys, err := System(role, sample(), "custom instructions here")
		if err != nil {
			t.Fatalf("%s system: %v", role, err)
		}
		for _, want := range []string{"busybees", Title(role), "`mail_send`", "`done`", "`issue_create`", "custom instructions here", "--label \"bees\" --assignee \"kyle\"", "/s/notes/x.md"} {
			if !strings.Contains(sys, want) {
				t.Errorf("%s system prompt missing %q", role, want)
			}
		}
		task, err := Task(role, sample())
		if err != nil {
			t.Fatalf("%s task: %v", role, err)
		}
		if !strings.Contains(task, "remember this") {
			t.Errorf("%s task prompt missing notes", role)
		}
		if strings.Contains(sys+task, "<no value>") {
			t.Errorf("%s prompt contains <no value>", role)
		}
	}
}

// The consolidation paragraph is asked for by the scheduler and must reach
// every task prompt in one wording. Nothing else about the prompt changes.
func TestConsolidateNotesParagraph(t *testing.T) {
	// The exact text the partial renders, so that removing it from the
	// prompt has to give back the prompt as it is rendered today.
	const para = "\nAlso consolidate your notes this session (every 10 sessions): rewrite `/s/notes/x.md`\n" +
		"into the sections above — merge duplicates, drop what is stale or contradicted, keep\n" +
		"decisions, commands and gotchas. Do it before you report your outcome, in addition to\n" +
		"your normal work.\n"

	for _, name := range append(append([]string{}, config.Roles...), "reviewer_checks") {
		off, err := TaskNamed(config.RoleDeveloper, name, sample())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(off, "Also consolidate") {
			t.Errorf("%s asks for consolidation without being told to:\n%s", name, off)
		}

		d := sample()
		d.ConsolidateNotes, d.ConsolidateReason = true, "every 10 sessions"
		on, err := TaskNamed(config.RoleDeveloper, name, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(on, para) {
			t.Fatalf("%s does not ask for consolidation:\n%s", name, on)
		}
		if got := strings.Replace(on, para, "", 1); got != off {
			t.Errorf("%s changed beyond the paragraph:\n%s", name, got)
		}
		if i := strings.Index(on, para); i < strings.Index(on, "remember this") {
			t.Errorf("%s asks before showing the notes:\n%s", name, on)
		}
	}
}

// The system prompt tells every role what shape its notes should have, so
// there is something to consolidate into.
func TestSystemPromptNamesTheNotesSections(t *testing.T) {
	for _, role := range config.Roles {
		sys, err := System(role, sample(), "")
		if err != nil {
			t.Fatal(err)
		}
		for _, section := range []string{"Project facts", "Conventions", "Decisions", "Open questions"} {
			if !strings.Contains(sys, section) {
				t.Errorf("%s system prompt does not name the %q section", role, section)
			}
		}
	}
}

func TestProjectManagerIsToldTheMaxSize(t *testing.T) {
	d := sample()
	d.MaxSize = "m"
	sys, err := System(config.RoleProjectManager, d, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "anything larger than `m` is not dispatched") {
		t.Fatalf("project manager system prompt does not carry max_size:\n%s", sys)
	}
}

func TestRoleSpecifics(t *testing.T) {
	dev, _ := Task(config.RoleDeveloper, sample())
	if !strings.Contains(dev, "part of feature #12: Exports") {
		t.Fatalf("developer task missing parent: %s", dev)
	}
	pjm, _ := Task(config.RoleProjectManager, sample())
	if !strings.Contains(pjm, "parent feature: #12 Exports") {
		t.Fatalf("project manager task missing parent: %s", pjm)
	}
	// Declared, still-open dependencies: on the triage header line and as a
	// column of the other-issues table.
	if !strings.Contains(pjm, "blocked by: #37 (open)") {
		t.Fatalf("project manager task missing blockers: %s", pjm)
	}
	if !strings.Contains(pjm, "| # | State | Kind | Blocked by | Milestone | Title |") ||
		!strings.Contains(pjm, "| 5 | triage | bug | #37 | - | Other |") {
		t.Fatalf("project manager task missing blocked-by column: %s", pjm)
	}
	noDeps := sample()
	noDeps.Blockers = nil
	pjm, _ = Task(config.RoleProjectManager, noDeps)
	if strings.Contains(pjm, "blocked by:") || !strings.Contains(pjm, "| 5 | triage | bug | - | - | Other |") {
		t.Fatalf("project manager task without blockers: %s", pjm)
	}
	if !strings.Contains(dev, "please fix") || !strings.Contains(dev, "`status: pr-updated`, `pr: 9`") {
		t.Fatalf("developer task: %s", dev)
	}
	d := sample()
	d.PR = nil
	dev, _ = Task(config.RoleDeveloper, d)
	if !strings.Contains(dev, "Closes #4") || strings.Contains(dev, "pr-updated") {
		t.Fatalf("developer first-round task: %s", dev)
	}
	rev, _ := Task(config.RoleReviewer, sample())
	if !strings.Contains(rev, "review pull request #9 (round 1 of 3)") {
		t.Fatalf("reviewer task: %s", rev)
	}
	pm, _ := Task(config.RoleProductManager, sample())
	if !strings.Contains(pm, "| 1 | v1 | 0 | 0 | first release |") || !strings.Contains(pm, "| 5 | triage | bug |") || !strings.Contains(pm, "#9: Dark mode please") || !strings.Contains(pm, "also on mobile") || !strings.Contains(pm, "#13: Search") || !strings.Contains(pm, "| 12 | - | 2/4 done | - | yes | Exports |") {
		t.Fatalf("pm task: %s", pm)
	}
	qa, _ := Task(config.RoleQA, sample())
	if !strings.Contains(qa, "PR #8: Merged") || !strings.Contains(qa, "This is your first run") {
		t.Fatalf("qa task: %s", qa)
	}
	d = sample()
	d.FailedChecks = []github.Check{{Name: "go / test", Bucket: "fail", Link: "https://github.com/acme/widgets/actions/runs/42/job/7", Workflow: "CI"}}
	checks, err := TaskNamed(config.RoleReviewer, "reviewer_checks", d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(checks, "**go / test** (CI) — fail") || !strings.Contains(checks, "actions/runs/42") {
		t.Fatalf("reviewer checks task: %s", checks)
	}
	d = sample()
	d.CommitFlags = "--gpg-sign --signoff"
	sys, _ := System(config.RoleDeveloper, d, "")
	if !strings.Contains(sys, "When creating git commits, always use the following extra flags: `--gpg-sign --signoff`.") {
		t.Fatalf("commit flags missing:\n%s", sys)
	}
	sys, _ = System(config.RoleDeveloper, sample(), "")
	if strings.Contains(sys, "extra flags") {
		t.Fatal("no commit flags configured; sentence should be absent")
	}
	if strings.Contains(sys, "Additional instructions") {
		t.Fatal("empty custom prompt should not add a section")
	}
}

func TestReviewerPromptStatesTheSize(t *testing.T) {
	d := sample()
	d.Size = "xs"
	sys, err := System(config.RoleReviewer, d, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "this is an `xs` change") || !strings.Contains(sys, "do not ask for restructuring") {
		t.Fatalf("reviewer prompt missing the size:\n%s", sys)
	}
	if strings.Contains(sys, "crosses subsystems") {
		t.Fatalf("reviewer prompt mixes in another size:\n%s", sys)
	}
	d.Size = "l"
	sys, _ = System(config.RoleReviewer, d, "")
	if !strings.Contains(sys, "this is an `l` change") || !strings.Contains(sys, "crosses subsystems") {
		t.Fatalf("reviewer prompt for l:\n%s", sys)
	}
	// An unsized issue says nothing about size.
	sys, _ = System(config.RoleReviewer, sample(), "")
	if strings.Contains(sys, "Size: this is") {
		t.Fatalf("unsized reviewer prompt should not mention a size:\n%s", sys)
	}
}

func TestManagerPromptsDescribeSizes(t *testing.T) {
	pjm, err := System(config.RoleProjectManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bees:size/xs", "bees:size/s", "bees:size/m", "bees:size/l", "bees:size/xl", "split it instead of labelling it"} {
		if !strings.Contains(pjm, want) {
			t.Errorf("project manager prompt missing %q", want)
		}
	}
	pm, _ := System(config.RoleProductManager, sample(), "")
	if !strings.Contains(pm, `--label "bees:size/s"`) {
		t.Errorf("product manager prompt should show pre-sizing:\n%s", pm)
	}
}

// A proposal and an approved feature must be distinguishable in the product
// manager's task prompt: the label is the only discriminator (bees and people
// share one GitHub account, so the author says nothing), and the prompt tells
// the product manager to break approved features down.
func TestProductManagerTaskMarksProposals(t *testing.T) {
	feature := func(n int, title string, labels ...string) github.Issue {
		i := github.Issue{Number: n, Title: title, Body: "why", Author: github.Author{Login: "kyle"}}
		for _, l := range labels {
			i.Labels = append(i.Labels, github.Label{Name: l})
		}
		return i
	}
	labels := config.LabelsFor("bees")
	proposal := feature(40, "Bee-written idea", labels.Feature, labels.Proposal)
	approved := feature(41, "Human-written feature", labels.Feature)

	d := sample()
	d.Features = []github.Issue{proposal, approved}
	d.FreshFeatures = []github.Issue{proposal, approved}
	d.Progress = nil

	task, err := Task(config.RoleProductManager, d)
	if err != nil {
		t.Fatal(err)
	}

	// The fresh-feature header line carries the state, since instruction 2 acts
	// on that section.
	for _, want := range []string{
		"#40: Bee-written idea",
		"proposal: yes",
		"#41: Human-written feature",
		"proposal: no",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("product manager task is missing %q:\n%s", want, task)
		}
	}

	// The feature table has a Proposal column, and the two rows differ in it.
	rowOf := func(n string) string {
		for _, line := range strings.Split(task, "\n") {
			if strings.HasPrefix(line, "| "+n+" |") {
				return line
			}
		}
		t.Fatalf("no table row for feature #%s:\n%s", n, task)
		return ""
	}
	if !strings.Contains(task, "| # | Milestone | Progress | Proposal | Waiting on person | Title |") {
		t.Errorf("feature table has no Proposal column:\n%s", task)
	}
	if got := rowOf("40"); !strings.Contains(got, "| yes |") {
		t.Errorf("proposal row is not marked as a proposal: %s", got)
	}
	if got := rowOf("41"); strings.Contains(got, "| yes |") {
		t.Errorf("approved feature row claims to be a proposal: %s", got)
	}

	// And the instruction to break features down carries the exception.
	if !strings.Contains(task, "unless it is a proposal") {
		t.Errorf("break-it-down instruction has no proposal exception:\n%s", task)
	}
}
