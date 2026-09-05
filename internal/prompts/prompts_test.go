package prompts

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
)

// sampleMailTime is a fixed timestamp, never time.Now(): tests render the
// same prompt from two independent sample() calls and compare the two strings,
// and mail.Format renders a message's timestamp at second precision, so a
// wall-clock fixture differs whenever the two calls straddle a second
// boundary. Nothing asserts on the rendered value; TestSampleFixtureIsDeterministic
// guards the rule.
var sampleMailTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func sample() Data {
	return Data{
		Project: config.Project{Repo: "acme/widgets", DefaultBranch: "main", Remote: "origin"},
		Filter:  config.Filter{Label: "bees", Assignee: "kyle"},
		Labels:  config.LabelsFor("bees"),
		WorkDir: "/tmp/ws", Branch: "bees/issue-4", StateDir: "/s", SessionDir: "/s/sessions/1", NotesFile: "/s/notes/x.md",
		Notes:             "remember this",
		Inbox:             []mail.Message{{ID: "m1", From: "reviewer", To: "developer", Subject: "Review round 1", Body: "please fix", PR: 9, CreatedAt: sampleMailTime}},
		Issue:             &github.Issue{Number: 4, Title: "Add thing", Body: "details", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:feature"}}, Author: github.Author{Login: "kyle"}},
		PR:                &github.PR{Number: 9, Title: "Add thing", HeadRefName: "bees/issue-4", BaseRefName: "main", Author: github.Author{Login: "bot"}},
		Issues:            []github.Issue{{Number: 6, Title: "Waiting", Labels: []github.Label{{Name: "bees:triage"}, {Name: "bees:bug"}}}, {Number: 7, Title: "Building", Labels: []github.Label{{Name: "bees:in-progress"}}}},
		TriageIssues:      []github.Issue{{Number: 5, Title: "Other", Body: "b"}},
		MergedPRs:         []github.PR{{Number: 8, Title: "Merged", Body: "x"}},
		Milestones:        []github.Milestone{{Number: 1, Title: "v1", Description: "first\nrelease"}},
		Features:          []github.Issue{{Number: 12, Title: "Exports", Labels: []github.Label{{Name: "bees:feature"}, {Name: "bees:question"}}}},
		Progress:          map[int]github.SubIssueSummary{12: {Total: 4, Completed: 2}},
		Parent:            &github.Parent{Number: 12, Title: "Exports"},
		Stages:            config.DefaultReviewStages,
		Parents:           map[int]github.Parent{5: {Number: 12, Title: "Exports"}, 6: {Number: 12, Title: "Exports"}},
		Blockers:          map[int][]int{5: {37}, 6: {37}},
		FreshFeatures:     []github.Issue{{Number: 13, Title: "Search", Body: "find things", Author: github.Author{Login: "kyle"}}},
		CompletedFeatures: []github.Issue{{Number: 14, Title: "Import", Body: "load things", Author: github.Author{Login: "kyle"}}},
		Feedback:          []github.Issue{{Number: 9, Title: "Dark mode please", Body: "would be nice", Author: github.Author{Login: "kyle"}, Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "also on mobile"}}}},
		MaxSize:           "l",
		Round:             1, MaxRounds: 3,
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

// No field of sample() may depend on the wall clock. Several tests render the
// same prompt from two independent sample() calls and compare the two strings
// byte for byte (TestConsolidateNotesParagraph does), and mail.Format renders
// a message's timestamp at second precision — so a time.Now() fixture makes
// those tests fail whenever the two calls land either side of a second
// boundary. Rendering deliberately across a boundary turns that one-in-a-few-
// thousand flake into a failure every run.
func TestSampleFixtureIsDeterministic(t *testing.T) {
	first, err := Task(config.RoleDeveloper, sample())
	if err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); time.Now().Truncate(time.Second).Equal(start.Truncate(time.Second)); {
		time.Sleep(time.Millisecond)
	}
	second, err := Task(config.RoleDeveloper, sample())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("two sample() renders differ, so a fixture field depends on the wall clock:\n%s", firstDiffLine(first, second))
	}
}

// firstDiffLine describes where two renders that should be identical part ways.
func firstDiffLine(a, b string) string {
	as, bs := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			return fmt.Sprintf("line %d:\n  %q\n  %q", i+1, as[i], bs[i])
		}
	}
	return fmt.Sprintf("%d lines vs %d lines", len(as), len(bs))
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

// The triage list a project manager session is handed is capped at
// scheduler.triage_batch_size (runProjectManager, internal/scheduler/
// singletons.go): everything past the cap arrives in Issues, where it used to
// be indistinguishable from issues in other states. The overflow gets a
// section of its own, and does not appear twice.
func TestProjectManagerSeesTheRestOfTheTriageQueue(t *testing.T) {
	pjm, err := Task(config.RoleProjectManager, sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Issues to triage (1 shown, more below)",
		"## Also in `bees:triage`, bodies not shown",
		"| 6 | bug | #37 | - | Waiting |",
	} {
		if !strings.Contains(pjm, want) {
			t.Errorf("project manager task missing %q:\n%s", want, pjm)
		}
	}
	if strings.Contains(pjm, "| 6 | triage |") {
		t.Errorf("an overflow triage issue is also in the other-issues table:\n%s", pjm)
	}
	// Nothing beyond the batch: no section, and no promise of one.
	d := sample()
	d.Issues = []github.Issue{{Number: 7, Title: "Building", Labels: []github.Label{{Name: "bees:in-progress"}}}}
	pjm, err = Task(config.RoleProjectManager, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pjm, "more below") || strings.Contains(pjm, "Also in") {
		t.Errorf("triage queue fits in one batch; there should be no overflow section:\n%s", pjm)
	}
}

// runProjectManager (internal/scheduler/singletons.go) passes the open pull
// requests as Data.PRs; the template must render them, or the field is dead
// data the project manager never sees (#388).
func TestProjectManagerSeesOpenPullRequests(t *testing.T) {
	d := sample()
	d.PRs = []github.PR{{Number: 9, Title: "Add thing", HeadRefName: "bees/issue-4"}}
	pjm, err := Task(config.RoleProjectManager, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Open pull requests (1)",
		"| 9 | Add thing | bees/issue-4 |",
	} {
		if !strings.Contains(pjm, want) {
			t.Errorf("project manager task missing %q:\n%s", want, pjm)
		}
	}

	d.PRs = nil
	pjm, err = Task(config.RoleProjectManager, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pjm, "## Open pull requests (0)") || !strings.Contains(pjm, "_None._") {
		t.Errorf("project manager task missing the empty pull-request case:\n%s", pjm)
	}
}

// The shared preamble must not state a rule more absolutely than the factory
// applies it, because the role prompt rendered right after it contradicts the
// absolute (#180). Two sentences: bees:priority ("only a person adds or
// removes it", while the project manager may add it to a work item that
// unblocks the factory and the product manager carries one from a feedback
// issue onto the work item it creates, #214), and what a person says is
// authoritative (issues and PRs only, while a person can also write to a role
// through the mailbox).
func TestPreambleDoesNotOverstateWhatTheFactoryApplies(t *testing.T) {
	for _, role := range config.Roles {
		sys, err := System(role, sample(), "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sys, "Only a person adds or removes it") {
			t.Errorf("%s preamble states bees:priority absolutely; the project manager may add it:\n%s", role, sys)
		}
		for _, want := range []string{
			"Only a person decides what carries it, and only a person removes it",
			"in mail from `human` as\nauthoritative",
		} {
			if !strings.Contains(sys, want) {
				t.Errorf("%s preamble missing %q:\n%s", role, want, sys)
			}
		}
	}
}

// bees:priority is a person's lever, with one exception the role prompt has to
// name explicitly or it simply contradicts the shared preamble. The archive
// shows the alternative the role reaches for when it wants something built
// next and has no lever — parking ready issues back in triage — so the prompt
// rules that out in the same breath.
func TestProjectManagerMayOnlyReorderTheQueueWithPriority(t *testing.T) {
	sys, err := System(config.RoleProjectManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"You are the one exception to",
		"Never move `bees:ready` issues back",
		"in its own order (`scheduler.dispatch_order`)",
	} {
		if !strings.Contains(flowed(sys), want) {
			t.Errorf("project manager system prompt missing %q:\n%s", want, sys)
		}
	}
	if strings.Contains(flowed(sys), "takes the oldest") {
		t.Errorf("project manager system prompt still states dispatch order as oldest-first:\n%s", sys)
	}
}

// A person reaches the product manager through feedback issues *and* through
// the mailbox, and the prompt used to name only the first — contradicting the
// shared preamble, which says mail from `human` outranks these instructions.
// Both halves are pinned: the direction sentence the other roles already carry,
// and the absence of the absolute "humans talk to you through issues" claim
// that made the mailbox look like no channel at all.
func TestProductManagerTakesDirectionFromHumanMail(t *testing.T) {
	sys, err := System(config.RoleProductManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Mail from `human` is not a question but a direction",
		"not the only channel",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("product manager system prompt missing %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "Humans talk to you through issues labelled") {
		t.Errorf("product manager system prompt still states feedback issues as the only channel:\n%s", sys)
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
		!strings.Contains(pjm, "| 7 | in-progress | - | - | - | Building |") {
		t.Fatalf("project manager task missing blocked-by column: %s", pjm)
	}
	noDeps := sample()
	noDeps.Blockers = nil
	pjm, _ = Task(config.RoleProjectManager, noDeps)
	if strings.Contains(pjm, "blocked by:") || !strings.Contains(pjm, "| 7 | in-progress | - | - | - | Building |") {
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
	if !strings.Contains(pm, "| 1 | v1 | 0 | 0 | first release |") || !strings.Contains(pm, "| 6 | triage | bug |") || !strings.Contains(pm, "#9: Dark mode please") || !strings.Contains(pm, "also on mobile") || !strings.Contains(pm, "#13: Search") || !strings.Contains(pm, "| 12 | - | 2/4 done | - | yes | Exports |") {
		t.Fatalf("pm task: %s", pm)
	}
	// The parent feature of every open work item, so a loose one is visible
	// without rebuilding the tree from GitHub: #6 is attached, #7 is not.
	if !strings.Contains(pm, "| # | State | Kind | Parent | Milestone | Title |") {
		t.Fatalf("product manager task missing the parent column: %s", pm)
	}
	if !strings.Contains(pm, "| 6 | triage | bug | #12 Exports | - | Waiting |") {
		t.Fatalf("product manager task missing an attached work item's parent: %s", pm)
	}
	if !strings.Contains(pm, "| 7 | in-progress | - | - | - | Building |") {
		t.Fatalf("product manager task should show - for a loose work item: %s", pm)
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
	// Pre-sizing goes through issue_create's `labels` list, not a CLI flag:
	// the tool has no --label (internal/mcpserver/tools.go, issueCreateInput).
	if !strings.Contains(pm, `labels: ["bees:size/s"]`) {
		t.Errorf("product manager prompt should show pre-sizing through the tool:\n%s", pm)
	}
	if strings.Contains(pm, `--label "bees:size/`) {
		t.Errorf("product manager prompt still passes a size as a CLI flag:\n%s", pm)
	}
}

// The product manager may report every status the done tool offers it, and no
// other. The prompt is where a session learns them, so the two must not drift:
// internal/session owns the enum.
// Every role's prompt ends with the statuses that role may report, and that
// sentence is where a session learns them: internal/session owns the enum, so
// the two must not drift (#355 is the same drift in `bees done --help`).
func TestEveryRolePromptListsEveryOutcome(t *testing.T) {
	var every []string
	for _, role := range config.Roles {
		every = append(every, session.ValidOutcomes(role)...)
	}
	for _, role := range config.Roles {
		sys, err := System(role, sample(), "")
		if err != nil {
			t.Fatal(err)
		}
		_, statuses, ok := strings.Cut(sys, "Outcome statuses:")
		if !ok {
			t.Errorf("%s prompt has no outcome statuses line:\n%s", role, sys)
			continue
		}
		want := session.ValidOutcomes(role)
		for _, w := range want {
			if !strings.Contains(statuses, "`"+w+"`") {
				t.Errorf("%s prompt does not offer the %q outcome:%s", role, w, statuses)
			}
		}
		for _, other := range every {
			if slices.Contains(want, other) {
				continue
			}
			if strings.Contains(statuses, "`"+other+"`") {
				t.Errorf("%s prompt offers %q, which is another role's outcome:%s", role, other, statuses)
			}
		}
	}
}

// bees:question is cleared by the orchestrator when a person answers
// (scheduler.freshIssues), so a feature can reach the product manager with the
// label already gone. The prompt must say so, or a session reads the missing
// label as "I never asked".
func TestProductManagerDoesNotClearItsOwnQuestionLabel(t *testing.T) {
	pm, err := System(config.RoleProductManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"never take `bees:question` off yourself",
		"the orchestrator removes it",
		"`waiting: false` only to withdraw",
	} {
		if !strings.Contains(pm, want) {
			t.Errorf("product manager prompt missing %q:\n%s", want, pm)
		}
	}
}

// Work items filed by other roles have no parent feature, so attaching them is
// recurring work the prompt must name — with the tool that does it.
func TestProductManagerAttachesLooseWorkItems(t *testing.T) {
	pm, err := System(config.RoleProductManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pm, "Keep the feature tree honest") || !strings.Contains(pm, "`issue_link`") {
		t.Errorf("product manager prompt does not tell it to attach loose work items:\n%s", pm)
	}
	task, err := Task(config.RoleProductManager, sample())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task, "Check the feature tree") {
		t.Errorf("product manager task does not run the feature-tree check:\n%s", task)
	}
}

// Two of productManagerHasWork's wake conditions leave the fresh-feature,
// feedback and mail sections of the task empty (internal/scheduler/singletons.go):
// a person's comment on a proposal, and — since #239 — a feature whose every
// sub-issue has closed. Both have a task section of their own, so an idle rule
// that names only the first three sections tells the session to answer a waiting
// person, or to leave a finished feature open, with `idle`.
func TestProductManagerIdleRuleCoversProposals(t *testing.T) {
	pm, err := System(config.RoleProductManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	idle := pm[strings.Index(pm, "Working a pass:"):]
	if i := strings.Index(idle, "\n\nPacing:"); i > 0 {
		idle = idle[:i]
	}
	for _, want := range []string{
		"read the proposals section before you conclude anything",
		"leaves that section on its own",
		`"Features whose work is done" closed its last work item`,
		"report `idle` and mean it",
	} {
		if !strings.Contains(idle, want) {
			t.Errorf("product manager idle rule missing %q:\n%s", want, idle)
		}
	}
	// The rule must not be a blanket "proposals section non-empty" veto:
	// github.Issue.AwaitingBee seeds the human side with CreatedAt, so a
	// proposal the product manager has never answered sits there forever.
	if !strings.Contains(idle, "that you have not answered") {
		t.Errorf("product manager idle rule vetoes on the whole proposals section:\n%s", idle)
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
	d.FreshFeatures = []github.Issue{approved}
	d.Proposals = []github.Issue{proposal}
	d.Progress = nil

	task, err := Task(config.RoleProductManager, d)
	if err != nil {
		t.Fatal(err)
	}

	// The header line of each section carries the state, since instruction 2
	// acts on those sections.
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

	// The proposal is presented, but never under the section instruction 2
	// tells the product manager to break down.
	breakdown, proposals, ok := cut3(task,
		"## Feature issues needing you", "## Proposals awaiting a person's approval", "## All open feature issues")
	if !ok {
		t.Fatalf("product manager task has no proposals section:\n%s", task)
	}
	if strings.Contains(breakdown, "#40") {
		t.Errorf("a proposal is listed as a feature needing breakdown:\n%s", breakdown)
	}
	if !strings.Contains(proposals, "#40: Bee-written idea") || !strings.Contains(proposals, "why") {
		t.Errorf("the proposal is not presented with its body:\n%s", proposals)
	}
	if strings.Contains(proposals, "#41") {
		t.Errorf("an approved feature is listed as a proposal:\n%s", proposals)
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
	// Every proposal is waiting on a person, whether it is fresh or not.
	if got := rowOf("40"); !strings.Contains(got, "| proposal |") {
		t.Errorf("proposal row does not say it waits on a person: %s", got)
	}
	if got := rowOf("41"); strings.Contains(got, "| proposal |") {
		t.Errorf("approved feature row waits on a person: %s", got)
	}

	// And the instruction to break features down carries the exception.
	if !strings.Contains(task, "The proposals listed above are the exception") {
		t.Errorf("break-it-down instruction has no proposal exception:\n%s", task)
	}
}

// cut3 splits text at three headings and returns what stands under the first
// two of them.
func cut3(text, a, b, c string) (string, string, bool) {
	_, rest, ok := strings.Cut(text, a)
	if !ok {
		return "", "", false
	}
	first, rest, ok := strings.Cut(rest, b)
	if !ok {
		return "", "", false
	}
	second, _, ok := strings.Cut(rest, c)
	return first, second, ok
}

// With scheduler.notify set, the product manager is told to start a question
// comment with the mentions — and with it unset the prompt is unchanged.
func TestProductManagerMentionsNotify(t *testing.T) {
	d := sample()
	off, err := System(config.RoleProductManager, d, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "@kpenfound") || strings.Contains(off, "Start the comment with") {
		t.Fatalf("notify is unset but the prompt mentions somebody:\n%s", off)
	}

	d.Notify = "@kpenfound @myorg/bees-team"
	on, err := System(config.RoleProductManager, d, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on, "Start the comment with `@kpenfound @myorg/bees-team`") {
		t.Fatalf("product manager system prompt does not carry the mentions:\n%s", on)
	}
	// The mention paragraph is the whole difference between the two renders.
	i := strings.Index(on, "Start the comment with")
	j := strings.Index(on, "Stop working on that feature")
	if i < 0 || j <= i {
		t.Fatalf("mention paragraph is not where it should be:\n%s", on)
	}
	if got := strings.Replace(on, on[i:j], "", 1); got != off {
		t.Errorf("notify changes the prompt beyond the mention paragraph:\n%s", got)
	}
}

// Verification is CI's job, not the reviewer's: a person said so on #5, and the
// archive shows every short round-1 approval spending most of its turns on
// `go build/vet/test`, `dagger check` and throwaway worktrees. Both halves are
// pinned separately, because a paraphrase of "run the tests" creeping back in
// would leave the positive sentence untouched.
func TestReviewerDoesNotRunTheTestSuite(t *testing.T) {
	sys, err := System(config.RoleReviewer, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Verifying that the change builds and passes is CI's job",
		"do not spend the session re-running the repository's\n   test-suite",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("reviewer system prompt missing %q:\n%s", want, sys)
		}
	}
	for _, gone := range []string{
		"Run the tests the way the repository documents",
		"exercise the change where practical",
	} {
		if strings.Contains(sys, gone) {
			t.Errorf("reviewer system prompt still tells it to run the tests (%q):\n%s", gone, sys)
		}
	}
	// A repository that reports no checks is the case where the old prompt
	// made the reviewer stand in for CI; now it says so in its note instead.
	d := sample()
	d.ChecksStatus = "passed"
	task, err := Task(config.RoleReviewer, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task, "nothing was verified for you") {
		t.Errorf("reviewer task does not say nothing was verified:\n%s", task)
	}
	if strings.Contains(task, "run the tests yourself") || strings.Contains(task, "test-suite yourself") {
		t.Errorf("reviewer task still tells it to run the tests:\n%s", task)
	}
}

// #150 and #168 were both filed on pull requests that had already been
// approved, and both are the same shape: a fix applied at one site while an
// identical sibling site kept the defect. That is the class of defect the
// review misses, so the "look for" list names it.
func TestReviewerLooksForTheSameShapeElsewhere(t *testing.T) {
	sys, err := System(config.RoleReviewer, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"**the same shape elsewhere**",
		"not for the line the PR happened to edit",
		"is one to drop,\n   not to hedge",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("reviewer system prompt missing %q:\n%s", want, sys)
		}
	}
}

// Checks mode runs before the first review as well as after an approval — the
// prereview stage in scheduler.workIssue calls fixFailedChecks independently of
// auto_merge — so the system prompt must not condition it on auto-merge and the
// checks task must not tell a pre-review session that it approved anything.
// The task also names the outcome for "they went green on their own", which the
// reviewer's three statuses do not suggest on their own.
func TestReviewerChecksModeAlsoHappensBeforeTheFirstReview(t *testing.T) {
	sys, err := System(config.RoleReviewer, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "before your\nfirst review when pre-review checks are on") {
		t.Errorf("reviewer system prompt does not say checks mode precedes the first review:\n%s", sys)
	}
	if !strings.Contains(sys, "again after your approval when\nauto-merge is on") {
		t.Errorf("reviewer system prompt does not say checks mode follows an approval under auto-merge:\n%s", sys)
	}
	if strings.Contains(sys, "when auto-merge is enabled and the required checks fail after your") {
		t.Errorf("reviewer system prompt still conditions checks mode on auto-merge alone:\n%s", sys)
	}
	checks, err := TaskNamed(config.RoleReviewer, "reviewer_checks", sample())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(checks, "You approved this pull request") {
		t.Errorf("checks task claims an approval that the pre-review session never made:\n%s", checks)
	}
	if !strings.Contains(checks, "already green when you look") || !strings.Contains(checks, "`status: approved`") {
		t.Errorf("checks task does not say what to report when the checks are already green:\n%s", checks)
	}
}

// The reviewer's pre-review checks section: one line per check and a sentence
// per status. It is absent when the pre-review read was skipped.
func TestReviewerChecksSection(t *testing.T) {
	d := sample()
	d.Checks = []github.Check{{Name: "go / test", Bucket: "pass", Link: "https://ci.example.com/1"}}
	d.ChecksStatus, d.ChecksTimeout = "passed", "10m"
	rev, _ := Task(config.RoleReviewer, d)
	if !strings.Contains(rev, "## Required checks") || !strings.Contains(rev, "go / test — pass — https://ci.example.com/1") {
		t.Fatalf("passing checks are not listed: %s", rev)
	}
	if !strings.Contains(rev, "CI is green") {
		t.Fatalf("reviewer is not told CI is green: %s", rev)
	}

	d.ChecksStatus, d.Checks[0].Bucket = "pending", "pending"
	rev, _ = Task(config.RoleReviewer, d)
	if !strings.Contains(rev, "still pending after `10m`") || strings.Contains(rev, "CI is green") {
		t.Fatalf("pending checks: %s", rev)
	}

	d.Checks, d.ChecksStatus = nil, "passed"
	rev, _ = Task(config.RoleReviewer, d)
	if !strings.Contains(rev, "reports no required checks") {
		t.Fatalf("a repository with no checks: %s", rev)
	}

	if rev, _ = Task(config.RoleReviewer, sample()); strings.Contains(rev, "Required checks") {
		t.Fatalf("the section must be absent when the read was skipped: %s", rev)
	}
}

// flowed collapses a rendered prompt's line wrapping, so an assertion about a
// sentence does not also pin where that sentence happens to break.
func flowed(s string) string { return strings.Join(strings.Fields(s), " ") }

// The developer runs the repository's own lint and tests before it pushes.
func TestDeveloperRunsTheRepositoryChecksBeforePushing(t *testing.T) {
	sys, err := System(config.RoleDeveloper, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(sys)
	if !strings.Contains(flow, "the repository's own lint and test commands") {
		t.Fatalf("developer system prompt has no self-check step:\n%s", sys)
	}
	if !strings.Contains(flow, "Record the exact commands in your notes file") {
		t.Fatalf("developer system prompt does not ask for the commands in the notes:\n%s", sys)
	}
	// Lint is the cheapest of the three self-checks but not the one review
	// rounds are actually spent on. Across the 22 reviewer messages in the
	// session archive not one cites a lint or format failure, while three ask
	// for a regression guard that does not guard (PRs 129, 65 r2, 189) and six
	// for a claim the change itself made false and left standing elsewhere
	// (PRs 77, 108, 163, 184, 187, 188). Both belong in the step.
	for _, want := range []string{
		"Undo your fix and confirm the test you added fails",
		"searching for the claim rather than for the sentence you edited",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("developer self-check step is missing %q:\n%s", want, sys)
		}
	}
}

// Falling behind the default branch is the most common reason for an extra
// review round: 6 of the 22 reviewer messages in the session archive carry a
// "merge main" blocker (PRs 65 r2, 65 r3, 94 r2, 99, 134, 149), and PR 65 r3
// exists for no other reason. The rule used to live only in the developer's
// notes file, where only 36 of 96 archived sessions acted on it.
func TestDeveloperMergesTheDefaultBranchBeforePushing(t *testing.T) {
	sys, err := System(config.RoleDeveloper, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(sys)
	if !strings.Contains(flow, "git fetch origin && git merge origin/main") {
		t.Fatalf("developer system prompt does not ask for the merge:\n%s", sys)
	}
	// The command must name the remote-tracking ref. Nothing in the factory
	// updates a worktree's local `main`: workspace.Manager.Fetch fetches in the
	// main clone (advancing refs/remotes/origin/* only) and Manager.Branch
	// creates the worktree from origin/<base>, so `git merge main` merges
	// whatever a person last left checked out and usually says "Already up to
	// date" while the branch is still behind.
	if !strings.Contains(flow, "not the local `main` branch") {
		t.Fatalf("developer system prompt does not warn off the local default branch:\n%s", sys)
	}
	// Two of those six rounds were spent on a merge git reported as clean:
	// it resolves by context, so "no conflict" does not mean "still builds".
	if !strings.Contains(flow, "A conflict-free merge is not a safe one") {
		t.Fatalf("developer system prompt does not warn that a clean merge can still break:\n%s", sys)
	}
}

// 0 questions and 0 developer messages in 96 archived sessions, while the one
// session that did hit a self-contradicting issue (issue 76, PR 146) chose,
// wrote both judgement calls into the PR body, and was approved with "both
// judgement calls are right, keep them". Choosing is the default; asking parks
// the issue and restarts the work in a session with none of this one's context.
func TestDeveloperChoosesBeforeItAsks(t *testing.T) {
	sys, err := System(config.RoleDeveloper, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(sys)
	for _, want := range []string{
		"Where the issue leaves you a choice, **make it**",
		"record the choice in the pull request under a heading",
		"Ask only when no reading is safe",
		"starts the work again with the answer",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("developer prompt does not prefer choosing to asking: missing %q:\n%s", want, sys)
		}
	}
	// workIssue verifies the message was sent during the session before it
	// honours `question`; without one the issue is escalated to a human
	// (internal/scheduler/developer.go, OutcomeQuestion).
	if !strings.Contains(flow, "**in this session**") {
		t.Errorf("developer prompt does not say the question must be sent in this session:\n%s", sys)
	}
	// `failed` reaches escalate() like any unknown status, so the note becomes
	// the comment a person reads on bees:needs-human. Saying only that the
	// status exists is what left it unused in 96 sessions.
	if !strings.Contains(flow, "`failed` stops the factory on this issue") ||
		!strings.Contains(flow, "labels it `bees:needs-human` and posts your note") {
		t.Errorf("developer prompt does not say what `failed` does:\n%s", sys)
	}
}

// QA tests the product, and a session that finds nothing is a success.
//
// Both rules come out of the session archive. QA's findings drifted into
// code reading — the 2026-08-30 sessions filed "the product_manager prompt
// falsely claims proposal-parent enforcement exists" and "the visibility
// backstop's own doc-comment scenarios are unreachable" — which is the
// reviewer's job on the pull request, not QA's on a merged tree. And "you
// need not file anything" had to be added to this repository's bees.toml as
// a custom instruction because the prompt did not carry it; it belongs in
// the prompt. Each clause is asserted separately so it fails on its own.
func TestQALooksForProductDefectsAndNeedNotFileAnything(t *testing.T) {
	sys, err := System(config.RoleQA, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"looking for **product defects**",
		"not for critique of how the",
		"**Filing an issue is not the goal; the report is.**",
		"is a good result — say so and file nothing",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("qa system prompt missing %q:\n%s", want, sys)
		}
	}
	qa, err := Task(config.RoleQA, sample())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(qa, "every defect you reproduced (none, if the batch is clean)") {
		t.Errorf("qa task still tells the session to file bugs unconditionally:\n%s", qa)
	}
}

// Before filing, QA searches closed issues too and reproduces the failure
// itself, and it never starts something that acts on the live project.
//
// The closed-issues half: the task lists open bugs only (runQA filters
// snap.issues by the bug label, internal/scheduler/singletons.go), so
// "search for an existing report" pointed at half the record — #84 and #103
// were both filed against reports that were already closed. A closed issue is
// context and not somewhere to file: ListOpenIssues asks gh for --state open
// (internal/github/github.go), so no role's task ever carries one and nothing
// reopens it, which is why a failure reproduced again becomes a new bug. The
// reproduce
// half: #103 was filed from a truncated reporter dump and closed as not
// reproducible. The never-start half: session 20260829-192238 ran
// `bees exec developer` with $BEES_CONFIG still pointing at the live
// factory; only the missing --issue flag stopped it launching a real
// session. Each half is a separate assertion, and the old wording each one
// replaces is asserted absent so a revert cannot pass silently.
func TestQAReproducesBeforeFilingAndStartsNothingLive(t *testing.T) {
	sys, err := System(config.RoleQA, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"search the existing issues, closed as well as open",
		"nothing in the factory reads a closed issue",
		"**reproduce it here**",
		"that acts on the real world for you",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("qa system prompt missing %q:\n%s", want, sys)
		}
	}
	for _, gone := range []string{
		"Search for an existing report first",
		"Comment on the report you find",
		"exercise it as a user would",
	} {
		if strings.Contains(sys, gone) {
			t.Errorf("qa system prompt still carries the old wording %q:\n%s", gone, sys)
		}
	}
}

// The reviewer can be steered by mail like every other role: `bees mail send
// --from human --to reviewer` is a documented channel, and until #197 the
// reviewer's sessions were built with no inbox at all, so a message sat unread
// forever. Three assertions: the mail section in both reviewer task templates
// (checks mode included — a check is diagnosed in a session of its own, and a
// person steering it writes to the same address), and the direction sentence
// the other roles already carry in the system prompt.
func TestReviewerReadsItsMail(t *testing.T) {
	for _, name := range []string{config.RoleReviewer, "reviewer_checks"} {
		task, err := TaskNamed(config.RoleReviewer, name, sample())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, want := range []string{"## Mail for you (1)", "Review round 1", "please fix"} {
			if !strings.Contains(task, want) {
				t.Errorf("%s task missing %q:\n%s", name, want, task)
			}
		}
		d := sample()
		d.Inbox = nil
		empty, err := TaskNamed(config.RoleReviewer, name, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(empty, "## Mail for you (0)") || !strings.Contains(empty, "_No new mail._") {
			t.Errorf("%s task has no empty mail section:\n%s", name, empty)
		}
	}
	sys, err := System(config.RoleReviewer, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Mail from `human` is not a question but a direction",
		"anything\naddressed to `reviewer`",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("reviewer system prompt missing %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "You may send mail to: `developer`.") {
		t.Errorf("reviewer system prompt still describes mail as send-only:\n%s", sys)
	}
}

// QA can be steered by mail like every other role: `bees mail send --from
// human --to qa` is a documented channel, and until #199 QA's sessions were
// built with no inbox at all, so a message sat unread forever. Two halves: the
// mail section in the task template (empty case included, so a filled-in
// section is not the only shape that renders) and the direction sentence the
// other roles already carry in the system prompt.
func TestQAReadsItsMail(t *testing.T) {
	task, err := Task(config.RoleQA, sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Mail for you (1)", "Review round 1", "please fix"} {
		if !strings.Contains(task, want) {
			t.Errorf("qa task missing %q:\n%s", want, task)
		}
	}
	d := sample()
	d.Inbox = nil
	empty, err := Task(config.RoleQA, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty, "## Mail for you (0)") || !strings.Contains(empty, "_No new mail._") {
		t.Errorf("qa task has no empty mail section:\n%s", empty)
	}
	sys, err := System(config.RoleQA, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"**Read your mail.**",
		"Mail from `human` is not a question but a direction",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("qa system prompt missing %q:\n%s", want, sys)
		}
	}
}

// Since #213 an issue a person files with only the `bees` label is routed to
// the product manager as feedback instead of into triage, so some of its
// inbox is now a small, ready-to-build ask rather than an idea. The prompt is
// the one place where that wording is load-bearing — the same rule is stated
// in prose in docs/workflow.md and docs/roles.md, where a test would fight
// every rewording — so the instruction is pinned here: turn the ask into a
// work item related to the feedback issue rather than a feature written
// around it, and carry the person's `bees:priority` onto the work item, which
// is the half nothing else in the factory does for it. The condition itself
// names the two labels that actually route (#226): `bees:bug` is a kind label
// but not a routing kind, so a person's bug report with no state label reaches
// the product manager the same way a bare `bees` issue does. The wants are
// matched against the flowed prompt because that condition wraps.
func TestProductManagerRoutesAReadyToBuildAsk(t *testing.T) {
	sys, err := System(config.RoleProductManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(sys)
	for _, want := range []string{
		"no state label and neither `bees:feature` nor `bees:feedback`",
		"`bees:bug` report",
		"**do not write a feature around it**",
		"`related: <the feedback issue>`",
		"If the person put `bees:priority` on the",
		"put it on the work item too",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("product manager system prompt missing %q:\n%s", want, sys)
		}
	}
}

// A feature whose every sub-issue has closed reaches the product manager in a
// section of its own (#239), not as a row in the feature table where the
// scheduler's one wake for it would be easy to miss. What that section asks
// for is a single yes/no — is the feature's original intent complete? — so it
// must not read as an invitation to keep a finished feature open by widening
// it. The wants are matched against the flowed task because the framing
// sentences wrap.
func TestProductManagerTaskPresentsCompletedFeatures(t *testing.T) {
	d := sample()
	d.CompletedFeatures = []github.Issue{{Number: 14, Title: "Import", Body: "load csv files",
		Author: github.Author{Login: "kyle"}}}
	task, err := Task(config.RoleProductManager, d)
	if err != nil {
		t.Fatal(err)
	}
	done := section(t, task, "## Features whose work is done")
	for _, want := range []string{
		"#14: Import",
		"load csv files",
		"is the feature's original intent complete?",
		"`gh issue close`",
		"not an invitation to widen it",
	} {
		if !strings.Contains(flowed(done), flowed(want)) {
			t.Errorf("completed-feature section missing %q:\n%s", want, done)
		}
	}
	// The count in the heading is what tells a session at a glance that the
	// section holds an event.
	if !strings.Contains(task, "## Features whose work is done (1)") {
		t.Errorf("completed-feature heading does not carry the count:\n%s", task)
	}

	// With nothing complete the section says so rather than disappearing, so
	// the absence is readable too.
	empty, err := Task(config.RoleProductManager, func() Data { d := sample(); d.CompletedFeatures = nil; return d }())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty, "## Features whose work is done (0)") ||
		!strings.Contains(empty, "_None: no feature had its last open work item closed since you last ran._") {
		t.Errorf("empty completed-feature section is missing:\n%s", empty)
	}
}

// section returns what stands under a heading of a rendered task prompt, up
// to the next heading.
func section(t *testing.T, prompt, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, heading)
	if !ok {
		t.Fatalf("prompt has no %q heading:\n%s", heading, prompt)
	}
	body, _, _ := strings.Cut(rest, "\n## ")
	return body
}

// stageHeadings extracts the stage names the reviewer's task template knows
// how to describe: the backticked token on every line that starts with the
// stable prefix "### `". Each stage's block in the template is headed by one,
// so the names are read off the prose the reviewer actually gets rather than
// off the {{if}} chain that selects it.
func stageHeadings(t *testing.T) []string {
	t.Helper()
	b, err := files.ReadFile("partials/stages.md")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "### `")
		if !ok {
			continue
		}
		name, _, ok := strings.Cut(rest, "`")
		if !ok {
			t.Fatalf("unterminated stage heading: %q", line)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatalf("no \"### `\" stage heading in partials/stages.md; the pin has lost its anchor")
	}
	return names
}

// The task template describes each stage in prose, and config validates
// roles.reviewer.stages against config.KnownReviewStages — two lists that have
// to name the same stages. A stage the config accepts but the template cannot
// describe renders as a heading-less gap the reviewer silently skips; a stage
// the template describes but the config rejects can never be configured. The
// comparison is a set both ways, because the template's order is the order the
// {{if}} chain happens to be written in, not a contract (the *rendered* order
// is the configured one — see TestReviewerStagesRenderInTheConfiguredOrder).
func TestReviewerTaskDescribesEveryKnownStage(t *testing.T) {
	got, want := stageHeadings(t), slices.Clone(config.KnownReviewStages)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("partials/stages.md describes %v; config.KnownReviewStages is %v", got, want)
	}
}

// Only the configured stages are rendered, in the configured order, and the
// section is absent altogether when no stage is set — which is what keeps the
// reviewer's checks-mode task, and every other role, unaffected by #240.
func TestReviewerStagesRenderInTheConfiguredOrder(t *testing.T) {
	d := sample()
	d.Stages = []string{"style", "implementation"}
	rev, err := Task(config.RoleReviewer, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rev, "## Review stages") {
		t.Fatalf("no stages section:\n%s", rev)
	}
	if i, j := strings.Index(rev, "### `style`"), strings.Index(rev, "### `implementation`"); i < 0 || j < 0 || i > j {
		t.Errorf("stages are not rendered in the configured order (style at %d, implementation at %d):\n%s", i, j, rev)
	}
	for _, gone := range []string{"### `cleanliness`", "### `completeness`", "### `product-fit`"} {
		if strings.Contains(rev, gone) {
			t.Errorf("unconfigured stage %q is rendered:\n%s", gone, rev)
		}
	}
	// Every stage ends in a verdict, and one failure blocks the approval.
	for _, want := range []string{"<stage>: pass —", "<stage>: fail —", "Approve only when every stage passed"} {
		if !strings.Contains(rev, want) {
			t.Errorf("stages section missing %q:\n%s", want, rev)
		}
	}
	if !strings.Contains(flowed(rev), "grouped by stage** in the stages' order") {
		t.Errorf("the instructions do not ask for feedback grouped by stage:\n%s", rev)
	}

	d.Stages = nil
	rev, err = Task(config.RoleReviewer, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rev, "## Review stages") || strings.Contains(rev, "### `style`") {
		t.Errorf("the stages section survives an empty stage list:\n%s", rev)
	}
	checks, err := TaskNamed(config.RoleReviewer, "reviewer_checks", sample())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(checks, "## Review stages") {
		t.Errorf("the checks-mode task reviews in stages:\n%s", checks)
	}
}

// product-fit is the one stage with a source of truth outside the diff and the
// issue, so it is the one stage that needs Data.Parent. It has to render both
// ways: the scheduler leaves Parent nil for a work item that belongs to no
// feature, and the stage must then say what it judged against instead rather
// than render a dangling "#: ".
func TestProductFitStageNamesTheParentFeature(t *testing.T) {
	d := sample()
	d.Stages = []string{config.StageProductFit}
	rev, err := Task(config.RoleReviewer, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flowed(rev), "**#12: Exports** — is the source of truth") {
		t.Errorf("product-fit does not name the parent feature:\n%s", rev)
	}

	d.Parent = nil
	rev, err = Task(config.RoleReviewer, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flowed(rev), "belongs to no feature, so the README and the docs are the only source of truth") {
		t.Errorf("product-fit without a parent feature:\n%s", rev)
	}
	if strings.Contains(rev, "#0") || strings.Contains(rev, "**#: ") {
		t.Errorf("product-fit renders an empty parent reference:\n%s", rev)
	}
}

// The reviewer's system prompt carries the rules that do not vary with the
// configured stage list: run every one, a verdict each, one grouped message,
// and an approval that needs them all. The task carries the list itself.
func TestReviewerSystemPromptCarriesTheStagedRules(t *testing.T) {
	sys, err := System(config.RoleReviewer, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(sys)
	for _, want := range []string{
		"Run **every** stage, and do not stop at the first one that finds something you would block on",
		"Give each stage a verdict line of its own",
		"**Approve** when every stage passed",
		"A single failed stage is `changes-requested`, whatever the others said",
		"**grouped by stage**, the stages in the task's order",
		"A checks-mode session runs no stages",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("reviewer system prompt missing %q:\n%s", want, sys)
		}
	}
}

// Planning mode has two sections in the product manager's task, and they say
// opposite things: while a person is still agreeing an issue nothing is
// created from it, and once they have agreed it the scope is settled. Both
// name the labels through config, never as literals, and the empty case says
// so rather than rendering an empty heading.
func TestProductManagerTaskRendersPlanningMode(t *testing.T) {
	labels := config.LabelsFor("bees")
	issue := func(n int, title string, names ...string) github.Issue {
		i := github.Issue{Number: n, Title: title, Body: "why " + title, Author: github.Author{Login: "kyle"}}
		for _, l := range names {
			i.Labels = append(i.Labels, github.Label{Name: l})
		}
		return i
	}
	d := sample()
	d.Planning = []github.Issue{issue(50, "Offline mode", labels.Feature, labels.Planning)}
	d.Planned = []github.Issue{issue(51, "Exports", labels.Feature, labels.Planned)}

	task, err := Task(config.RoleProductManager, d)
	if err != nil {
		t.Fatal(err)
	}
	planning := taskSection(t, task, "## Planning with a person")
	for _, want := range []string{
		"#50: Offline mode", "why Offline mode",
		"Break nothing down from these",
		"never add or remove either",
	} {
		if !strings.Contains(flowed(planning), flowed(want)) {
			t.Errorf("planning section is missing %q:\n%s", want, planning)
		}
	}
	if strings.Contains(planning, "#51") {
		t.Errorf("an agreed issue is presented as still in planning:\n%s", planning)
	}
	planned := taskSection(t, task, "## Agreed with a person")
	for _, want := range []string{
		"#51: Exports",
		"It is **settled**. Do not re-open the scope",
		"`## Decisions` section",
		"break none of them down twice",
	} {
		if !strings.Contains(flowed(planned), flowed(want)) {
			t.Errorf("agreed section is missing %q:\n%s", want, planned)
		}
	}
	if strings.Contains(planned, "#50") {
		t.Errorf("an issue still in planning is presented as agreed:\n%s", planned)
	}

	// Nothing in planning: both sections state that rather than disappearing.
	d.Planning, d.Planned = nil, nil
	task, err = Task(config.RoleProductManager, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(taskSection(t, task, "## Planning with a person"), "_Nothing is in planning._") {
		t.Errorf("empty planning section:\n%s", task)
	}
}

// taskSection returns what stands under a heading, up to the next one.
func taskSection(t *testing.T, task, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(task, heading)
	if !ok {
		t.Fatalf("task has no %q heading:\n%s", heading, task)
	}
	body, _, _ := strings.Cut(rest, "\n## ")
	return body
}

// TestAnInterruptedSessionIsReportedAtTheTopOfTheTask: the session that takes
// over from one a killed scheduler left unfinished is told so before it is
// told anything else — how far it got, where its transcript is, and what its
// own role has to do about it (#250). The assertion is deliberately stronger
// than "the section is there": with nothing interrupted the task prompt must
// be byte for byte the one this version has always rendered, so the section
// is checked as a *prefix* of an otherwise unchanged prompt.
//
// The advice is per role and only half of it is true of a reviewer: a
// reviewer session commits nothing and opens no pull request, and a round
// that reported no verdict has to be redone rather than carried on from. So
// each template is rendered with an interruption of the role it serves, and
// the wording the other role gets must be absent.
func TestAnInterruptedSessionIsReportedAtTheTopOfTheTask(t *testing.T) {
	const transcript = "/s/sessions/20260831-081500-developer-issue-4-r1-ab/transcript.jsonl"
	cases := []struct {
		name    string
		role    string
		want    []string
		unwant  []string
		summary string
	}{
		{
			name: config.RoleDeveloper, role: config.RoleDeveloper,
			summary: "developer session that ran for this issue before you was stopped after 3 turns",
			want:    []string{"The branch may carry work it never reported"},
			unwant:  []string{"this round starts over"},
		},
		{
			name: config.RoleReviewer, role: config.RoleReviewer,
			summary: "reviewer session that ran for this issue before you was stopped after 3 turns",
			want:    []string{"It reported no verdict, so this round starts over"},
			unwant:  []string{"The branch may carry work it never reported"},
		},
		{
			name: "reviewer_checks", role: config.RoleReviewer,
			summary: "reviewer session that ran for this issue before you was stopped after 3 turns",
			want:    []string{"It reported no verdict, so this round starts over"},
			unwant:  []string{"The branch may carry work it never reported"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quiet, err := TaskNamed(tc.role, tc.name, sample())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(quiet, "never reported an outcome") {
				t.Errorf("%s reports an interruption that never happened:\n%s", tc.name, quiet)
			}
			d := sample()
			d.Interrupted = &session.Interrupted{
				Role: tc.role, Name: "20260831-081500-" + tc.role + "-issue-4-r1-ab",
				Transcript: transcript, Turns: 3, Killed: true, Note: "stopped by bees kill",
			}
			task, err := TaskNamed(tc.role, tc.name, d)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(task, quiet) {
				t.Fatalf("%s: the interruption changed more than the section it adds:\n%s", tc.name, task)
			}
			section := strings.TrimSuffix(task, quiet)
			wants := append([]string{tc.summary, "never reported an outcome", transcript}, tc.want...)
			for _, want := range wants {
				if !strings.Contains(flowed(section), want) {
					t.Errorf("%s section missing %q:\n%s", tc.name, want, section)
				}
			}
			for _, unwant := range tc.unwant {
				if strings.Contains(flowed(section), unwant) {
					t.Errorf("%s section carries the other role's advice (%q):\n%s", tc.name, unwant, section)
				}
			}
		})
	}
}

// TestAnInterruptionWithNothingToShowStillReads: a session killed in its
// first seconds wrote no transcript and took no turn, and the section must
// not then say "after 0 turns" or point at a file that does not exist.
func TestAnInterruptionWithNothingToShowStillReads(t *testing.T) {
	d := sample()
	d.Interrupted = &session.Interrupted{Role: config.RoleDeveloper, Name: "x"}
	task, err := Task(config.RoleDeveloper, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(task, "0 turns") {
		t.Errorf("a session that took no turn is counted:\n%s", task)
	}
	if !strings.Contains(flowed(task), "It stopped before it wrote a transcript.") {
		t.Errorf("no transcript, and the prompt does not say so:\n%s", task)
	}
}

// A person's comment on the issue reaches the developer as mail from `human`
// (scheduler.deliverHumanIssueComments, #304). The prompt has to say three
// things a rendering of the comment history cannot: that such a comment is a
// direction rather than context, where it ranks against the issue and the
// reviewer, and that the reply goes on the issue rather than on the pull
// request — the paragraph above it sends every reply to the PR.
func TestDeveloperTakesDirectionsFromPeopleOnTheIssue(t *testing.T) {
	sys, err := System(config.RoleDeveloper, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(sys)
	for _, want := range []string{
		"Directions from people",
		"comment on the **issue** while you are working on it",
		"reaches you as mail from `human`",
		"it outranks the issue body and the reviewer",
		"Reply **on the issue**, not on the pull request, with the `comment` tool",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("developer prompt is missing %q:\n%s", want, sys)
		}
	}
	// The reply target is the issue's own number, not a placeholder: a
	// developer session renders one issue.
	if !strings.Contains(flow, fmt.Sprintf("`number: %d`", sample().Issue.Number)) {
		t.Errorf("developer prompt does not name the issue number to reply to:\n%s", sys)
	}
}

// A review a person asked for by labelling a pull request has no issue
// behind it: both reviewer prompts must render with Data.Issue nil, and the
// sentences that name the issue must keep their wording when it is set.
func TestRequestedReviewRendersWithoutAnIssue(t *testing.T) {
	d := sample()
	d.Issue = nil
	d.PR = &github.PR{Number: 42, Title: "Fix the widget", Body: "It was broken.", URL: "https://github.com/acme/widgets/pull/42",
		HeadRefName: "fix-widget", BaseRefName: "main", Author: github.Author{Login: "kyle"}}
	sys, err := System(config.RoleReviewer, d, "")
	if err != nil {
		t.Fatalf("system prompt with no issue: %v", err)
	}
	for _, want := range []string{"`mail_send` (`to: developer`, `pr: 42`, `subject:", "(`issue_create` with `bug: true`);"} {
		if !strings.Contains(flowed(sys), want) {
			t.Errorf("system prompt without an issue lacks %q", want)
		}
	}
	task, err := TaskNamed(config.RoleReviewer, "reviewer_requested", d)
	if err != nil {
		t.Fatalf("requested-review task with no issue: %v", err)
	}
	for _, want := range []string{"# Task: review pull request #42 (requested by a person)", "`bees:review-requested`",
		"## Pull request #42: Fix the widget", "https://github.com/acme/widgets/pull/42", "branch `fix-widget` → `main`", "author: kyle", "It was broken.",
		"## Mail for you (1)", "please fix", "## Your notes", "remember this", "gh pr diff 42 -R acme/widgets", "`pr_view`", "`status: approved`", "`status: changes-requested`"} {
		if !strings.Contains(task, want) {
			t.Errorf("requested-review task lacks %q:\n%s", want, task)
		}
	}
	if strings.Contains(task, "## Issue") {
		t.Errorf("requested-review task renders an issue section:\n%s", task)
	}

	// With an issue, the review-loop sentences read as they always have.
	sys, err = System(config.RoleReviewer, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`mail_send` (`to: developer`, `pr: 9`, `issue: 4`, `subject:", "(`issue_create` with `bug: true`, `related: 4`);"} {
		if !strings.Contains(flowed(sys), want) {
			t.Errorf("system prompt with an issue lacks %q", want)
		}
	}
}

// requestedReview is the Data a requested review renders from: no issue, a
// person's pull request, and the mode set.
func requestedReview() Data {
	d := sample()
	d.Issue = nil
	d.Mode = "requested"
	d.ActsAs = "busybees-bot"
	d.PR = &github.PR{Number: 42, Title: "Fix the widget", Body: "It was broken.", URL: "https://github.com/acme/widgets/pull/42",
		HeadRefName: "fix-widget", BaseRefName: "main", Author: github.Author{Login: "kyle"}}
	return d
}

// A requested review puts its verdict on the pull request as a GitHub
// review: the system prompt says so only in that mode, and the mode leaves
// a developer's review, which mails the developer, untouched.
func TestRequestedReviewSystemPromptSubmitsAReview(t *testing.T) {
	sys, err := System(config.RoleReviewer, requestedReview(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(sys)
	for _, want := range []string{
		"put your verdict on it as a GitHub review",
		"exactly one GitHub review, submitted with `submit_review` (`number: 42`)",
		"**Approve** (`event: approve`) when every stage passed",
		"**Request changes** (`event: request-changes`) when any stage failed",
		"**Comment** (`event: comment`) in place of `approve` when the pull request's author is the login the factory acts as",
		"The `<!-- bees:reviewer -->` marker is appended for you",
		"There is no developer on this pull request, so you send no mail",
		"Outcome statuses: `approved`, `changes-requested` (both after submitting the review), `failed`",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("requested-review system prompt lacks %q:\n%s", want, sys)
		}
	}
	for _, gone := range []string{"Do not submit a GitHub review", "`mail_send` (`to: developer`", "linked issue", "You may send mail to: `developer`", "the developer learns to skip"} {
		if strings.Contains(flow, gone) {
			t.Errorf("requested-review system prompt still says %q:\n%s", gone, sys)
		}
	}

	// A developer's pull request: the review-loop rules, and no review tool.
	normal, err := System(config.RoleReviewer, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	flow = flowed(normal)
	for _, want := range []string{"Do not submit a GitHub review and do not post your feedback as a comment", "`mail_send` (`to: developer`, `pr: 9`, `issue: 4`", "You may send mail to: `developer`, and to no one else"} {
		if !strings.Contains(flow, want) {
			t.Errorf("reviewer system prompt lacks %q:\n%s", want, normal)
		}
	}
	for _, gone := range []string{"submit_review", "event: approve", "GitHub refuses an approval"} {
		if strings.Contains(flow, gone) {
			t.Errorf("reviewer system prompt for a developer's pull request mentions %q:\n%s", gone, normal)
		}
	}
	// The mode is the only switch: the same data without it renders the
	// review-loop prompt, issue or no issue.
	d := requestedReview()
	d.Mode = ""
	off, err := System(config.RoleReviewer, d, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "submit_review") || !strings.Contains(off, "Do not submit a GitHub review") {
		t.Errorf("Mode empty with no issue rendered the requested-review prompt:\n%s", off)
	}
}

// The requested-review task is the whole brief: the pull request, the
// stages judged against its description rather than an issue, who the
// factory is on GitHub, and the review to submit.
func TestRequestedReviewTaskHasNoIssueAndSubmitsAReview(t *testing.T) {
	d := requestedReview()
	d.Stages = append(slices.Clone(config.DefaultReviewStages), "product-fit")
	task, err := TaskNamed(config.RoleReviewer, "reviewer_requested", d)
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(task)
	for _, want := range []string{
		"## No issue, no acceptance criteria",
		"Do not invent criteria the description does not state",
		"## Who you are on GitHub",
		"The factory acts as `busybees-bot` on GitHub. The pull request's author is `kyle`, so an approval is accepted.",
		"## Review stages",
		"### `implementation`", "### `completeness`", "### `cleanliness`", "### `style`", "### `product-fit`",
		"The pull request's description is the source of truth: there is no issue and no acceptance criteria",
		"This pull request belongs to no feature, so the README and the docs are the only source of truth",
		"Approve only when every stage passed",
		"submit your verdict as exactly one GitHub review with `submit_review` (`number: 42`)",
		"`event: approve` when every stage passed, `event: request-changes` when any failed, `event: comment` in place of `approve`",
		"Then report `done` with `status: approved` (after an approval, or a comment in its place) or `status: changes-requested`",
		"send no mail",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("requested-review task lacks %q:\n%s", want, task)
		}
	}
	for _, gone := range []string{"## Issue", "acceptance criteria one at a time", "the issue never mentioned", "changes the issue did not ask for", "work item", "mail_send"} {
		if strings.Contains(flow, gone) {
			t.Errorf("requested-review task still says %q:\n%s", gone, task)
		}
	}

	// The factory is the author: comment instead of approve.
	d.ActsAs = "kyle"
	same, err := TaskNamed(config.RoleReviewer, "reviewer_requested", d)
	if err != nil {
		t.Fatal(err)
	}
	if want := "The factory acts as `kyle` on GitHub. That is this pull request's author, and GitHub refuses an approval from a pull request's own author: where you would approve, submit a `comment` review instead"; !strings.Contains(flowed(same), want) {
		t.Errorf("same-account task lacks %q:\n%s", want, same)
	}
	// No [github]: the session finds out whose account it is.
	d.ActsAs = ""
	shared, err := TaskNamed(config.RoleReviewer, "reviewer_requested", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The factory has no GitHub account of its own", "Run `gh api user --jq .login` before you submit", "When that login is `kyle`, this pull request's author, GitHub refuses an approval"} {
		if !strings.Contains(flowed(shared), want) {
			t.Errorf("shared-account task lacks %q:\n%s", want, shared)
		}
	}
	if strings.Contains(shared, "The factory acts as") {
		t.Errorf("shared-account task names a login it does not have:\n%s", shared)
	}

	// The stages prose is shared with the review-loop task, which keeps
	// naming the issue.
	loop, err := Task(config.RoleReviewer, sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The issue above is the source of truth. Take its acceptance criteria one at a time", "the inputs and states the issue never mentioned", "changes the issue did not ask for"} {
		if !strings.Contains(flowed(loop), want) {
			t.Errorf("review-loop task lacks %q:\n%s", want, loop)
		}
	}
}

// Round 2 onward is a follow-up on the reviewer's own previous round, not a
// fresh review: it must account for every point already raised and say the
// scope is neither widened to the new commits alone nor narrowed to them.
// Round 1 must render exactly as it does today — the instruction is new text
// gated on .Round, not a rewording of anything round 1 already said (#397).
func TestReviewerFollowUpRoundAccountsForPreviousPoints(t *testing.T) {
	round1, err := Task(config.RoleReviewer, sample())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(round1, "follow-up review") {
		t.Errorf("round 1 task already reads as a follow-up review:\n%s", round1)
	}

	d := sample()
	d.Round = 2
	d.PreviousRounds = []mail.Message{{ID: "m1", From: "reviewer", To: "developer", Subject: "Review round 1", Body: "please fix", PR: 9, CreatedAt: sampleMailTime}}
	round2, err := Task(config.RoleReviewer, d)
	if err != nil {
		t.Fatal(err)
	}
	flow := flowed(round2)
	for _, want := range []string{
		"This is a follow-up review, not a fresh one",
		"go through `## Your feedback from previous rounds` point by point and say whether each was addressed",
		"judge the change as it now stands against every stage above, the same as a first review",
		"do not narrow it to the commits made since last round, and do not widen it into extra scrutiny of them either",
		"Read the whole diff, and raise anything you missed in round 1 too",
	} {
		if !strings.Contains(flow, want) {
			t.Errorf("round 2 task missing %q:\n%s", want, round2)
		}
	}
}
