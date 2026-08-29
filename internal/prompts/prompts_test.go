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
		FreshFeatures: []github.Issue{{Number: 13, Title: "Search", Body: "find things", Author: github.Author{Login: "kyle"}}},
		Feedback:      []github.Issue{{Number: 9, Title: "Dark mode please", Body: "would be nice", Author: github.Author{Login: "kyle"}, Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "also on mobile"}}}},
		Round:         1, MaxRounds: 3,
	}
}

func TestRenderAllRoles(t *testing.T) {
	for _, role := range config.Roles {
		sys, err := System(role, sample(), "custom instructions here")
		if err != nil {
			t.Fatalf("%s system: %v", role, err)
		}
		for _, want := range []string{"busybees", Title(role), "bees mail send", "bees done", "custom instructions here", "--label \"bees\" --assignee \"kyle\"", "/s/notes/x.md"} {
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

func TestRoleSpecifics(t *testing.T) {
	dev, _ := Task(config.RoleDeveloper, sample())
	if !strings.Contains(dev, "part of feature #12: Exports") {
		t.Fatalf("developer task missing parent: %s", dev)
	}
	pjm, _ := Task(config.RoleProjectManager, sample())
	if !strings.Contains(pjm, "parent feature: #12 Exports") {
		t.Fatalf("project manager task missing parent: %s", pjm)
	}
	if !strings.Contains(dev, "please fix") || !strings.Contains(dev, "bees done pr-updated --pr 9") {
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
	if !strings.Contains(pm, "| 1 | v1 | 0 | 0 | first release |") || !strings.Contains(pm, "| 5 | triage | bug |") || !strings.Contains(pm, "#9: Dark mode please") || !strings.Contains(pm, "also on mobile") || !strings.Contains(pm, "#13: Search") || !strings.Contains(pm, "| 12 | - | 2/4 done | yes | Exports |") {
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
