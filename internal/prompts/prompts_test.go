package prompts

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
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
		Issues:        []github.Issue{{Number: 6, Title: "Waiting", Labels: []github.Label{{Name: "bees:triage"}, {Name: "bees:bug"}}}, {Number: 7, Title: "Building", Labels: []github.Label{{Name: "bees:in-progress"}}}},
		TriageIssues:  []github.Issue{{Number: 5, Title: "Other", Body: "b"}},
		MergedPRs:     []github.PR{{Number: 8, Title: "Merged", Body: "x"}},
		Milestones:    []github.Milestone{{Number: 1, Title: "v1", Description: "first\nrelease"}},
		Features:      []github.Issue{{Number: 12, Title: "Exports", Labels: []github.Label{{Name: "bees:feature"}, {Name: "bees:question"}}}},
		Progress:      map[int]github.SubIssueSummary{12: {Total: 4, Completed: 2}},
		Parent:        &github.Parent{Number: 12, Title: "Exports"},
		Parents:       map[int]github.Parent{5: {Number: 12, Title: "Exports"}},
		Blockers:      map[int][]int{5: {37}, 6: {37}},
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

// The shared preamble must not state a rule more absolutely than the factory
// applies it, because the role prompt rendered right after it contradicts the
// absolute (#180). Two sentences: bees:priority ("only a person adds or
// removes it", while the project manager may add it to a work item that
// unblocks the factory), and what a person says is authoritative (issues and
// PRs only, while a person can also write to a role through the mailbox).
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
			"Only a person adds it — with one exception, named in the project manager's",
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
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("project manager system prompt missing %q:\n%s", want, sys)
		}
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
func TestProductManagerPromptListsEveryOutcome(t *testing.T) {
	pm, err := System(config.RoleProductManager, sample(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, statuses, ok := strings.Cut(pm, "Outcome statuses:")
	if !ok {
		t.Fatalf("product manager prompt has no outcome statuses line:\n%s", pm)
	}
	for _, want := range session.ValidOutcomes(config.RoleProductManager) {
		if !strings.Contains(statuses, "`"+want+"`") {
			t.Errorf("product manager prompt does not offer the %q outcome:\n%s", want, statuses)
		}
	}
	for _, other := range []string{"pr-opened", "pr-updated", "question", "approved", "changes-requested"} {
		if strings.Contains(statuses, "`"+other+"`") {
			t.Errorf("product manager prompt offers %q, which is another role's outcome:\n%s", other, statuses)
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

// productManagerHasWork has four wake conditions, and the fourth is a person's
// comment on a proposal (internal/scheduler/singletons.go:69-98). Proposals are
// partitioned into their own task section, so that wake leaves the fresh-feature,
// feedback and mail sections empty: an idle rule that names only those three
// tells the session to answer a waiting person with `idle`.
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
