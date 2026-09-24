package mcpserver

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

func releaseFixture() *fakeGitHub {
	f := newFakeGitHub()
	f.milestones = []github.Milestone{{Number: 7, Title: "v0.5.0", ClosedIssues: 2}, {Number: 8, Title: "v0.6.0", ClosedIssues: 1}}
	return f
}

func TestReleaseShipGuards(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seed      func(*fakeGitHub)
		milestone int
		want      string
	}{
		{"missing milestone", func(*fakeGitHub) {}, 99, "not open"},
		{"empty milestone", func(f *fakeGitHub) { f.milestones[0].ClosedIssues = 0 }, 7, "at least one closed"},
		{"open issue", func(f *fakeGitHub) { f.milestones[0].OpenIssues = 1 }, 7, "no open issues"},
		{"invalid tag", func(f *fakeGitHub) { f.milestones[0].Title = "bad tag" }, 7, "not a valid Git tag"},
		{"existing tag", func(f *fakeGitHub) { f.tags["v0.5.0"] = "prior" }, 7, "already exists"},
		{"milestoned PR", func(f *fakeGitHub) {
			f.prs[11] = github.PR{Number: 11, State: "OPEN", Milestone: &github.MilestoneRef{Title: "v0.5.0"}}
		}, 7, "pull request #11 is open"},
		{"PR closing milestone issue", func(f *fakeGitHub) {
			f.issue(10)
			i := f.issues[10]
			i.Milestone = &github.MilestoneRef{Title: "v0.5.0"}
			f.issues[10] = i
			f.prs[11] = github.PR{Number: 11, State: "OPEN", Body: "Closes #10"}
		}, 7, "pull request #11 is still open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := releaseFixture()
			tc.seed(f)
			h := newHarness(t, config.RoleReleaseManager, Deps{GitHub: f})
			r := h.callRaw("release_ship", map[string]any{"milestone": tc.milestone})
			if !r.IsError || !strings.Contains(resultText(r), tc.want) || len(f.releaseCalls) != 0 {
				t.Fatalf("result=%q error=%v calls=%v", resultText(r), r.IsError, f.releaseCalls)
			}
		})
	}
}

func TestReleaseShipSuccessAndRetry(t *testing.T) {
	f := releaseFixture()
	h := newHarness(t, config.RoleReleaseManager, Deps{GitHub: f})
	got := h.call("release_ship", map[string]any{"milestone": 7})
	for _, phrase := range []string{"Tag v0.5.0 pushed at main commit main-head", "GitHub release v0.5.0 created with generated notes", "Milestone #7 closed"} {
		if !strings.Contains(got, phrase) {
			t.Errorf("result %q lacks %q", got, phrase)
		}
	}
	if f.tags["v0.5.0"] != "main-head" || !f.releases["v0.5.0"] || f.milestones[0].State != "closed" || f.milestones[1].State == "closed" || !slices.Equal(f.releaseCalls, []string{"tag", "release", "close"}) {
		t.Fatalf("wrong effects: tags=%v releases=%v milestones=%v calls=%v", f.tags, f.releases, f.milestones, f.releaseCalls)
	}
	// The selected milestone is no longer open. A retry cannot retag it.
	r := h.callRaw("release_ship", map[string]any{"milestone": 7})
	if !r.IsError || len(f.releaseCalls) != 3 {
		t.Fatalf("retry = %q, calls=%v", resultText(r), f.releaseCalls)
	}
}

func TestReleaseShipPartialFailures(t *testing.T) {
	for _, tc := range []struct {
		step        string
		calls       []string
		mustHave    []string
		mustNotHave []string
	}{
		{"milestones", nil, []string{"could not read open milestones"}, nil},
		{"prs", nil, []string{"could not inspect open pull requests"}, nil},
		{"exists", nil, []string{"could not check whether tag"}, nil},
		{"head", nil, []string{"could not read main head"}, nil},
		{"tag", []string{"tag"}, []string{"could not create and push tag"}, []string{"Tag v0.5.0 pushed"}},
		{"release", []string{"tag", "release"}, []string{"Tag v0.5.0 pushed", "could not create GitHub release", "remains open"}, []string{"Milestone #7 closed"}},
		{"close", []string{"tag", "release", "close"}, []string{"Tag v0.5.0 pushed", "GitHub release v0.5.0 created", "could not close milestone #7"}, nil},
	} {
		t.Run(tc.step, func(t *testing.T) {
			f := releaseFixture()
			f.releaseErr[tc.step] = errors.New("injected failure")
			h := newHarness(t, config.RoleReleaseManager, Deps{GitHub: f})
			r := h.callRaw("release_ship", map[string]any{"milestone": 7})
			got := resultText(r)
			if !r.IsError || !slices.Equal(f.releaseCalls, tc.calls) {
				t.Fatalf("result=%q calls=%v", got, f.releaseCalls)
			}
			for _, phrase := range tc.mustHave {
				if !strings.Contains(got, phrase) {
					t.Errorf("%q lacks %q", got, phrase)
				}
			}
			for _, phrase := range tc.mustNotHave {
				if strings.Contains(got, phrase) {
					t.Errorf("%q contains %q", got, phrase)
				}
			}
			if f.milestones[0].State == "closed" || f.milestones[1].State == "closed" {
				t.Fatal("closed a milestone after failure")
			}
			if tc.step == "release" || tc.step == "close" {
				// A partial write never retries the tag or proceeds to another step.
				before := len(f.releaseCalls)
				r = h.callRaw("release_ship", map[string]any{"milestone": 7})
				if !r.IsError || !strings.Contains(resultText(r), "already exists") || len(f.releaseCalls) != before {
					t.Fatalf("retry=%q calls=%v", resultText(r), f.releaseCalls)
				}
			}
		})
	}
}

func TestReleaseShipNeverClosesAnotherMilestone(t *testing.T) {
	f := releaseFixture()
	h := newHarness(t, config.RoleReleaseManager, Deps{GitHub: f})
	h.call("release_ship", map[string]any{"milestone": 8})
	if f.milestones[0].State == "closed" || f.milestones[1].State != "closed" || f.tags["v0.6.0"] != "main-head" {
		t.Fatalf("shipped wrong milestone: %+v tags=%v", f.milestones, f.tags)
	}
}

func TestValidReleaseTag(t *testing.T) {
	for _, tag := range []string{"v0.5.0", "release-1", "v1.2.3+meta"} {
		if !validReleaseTag(tag) {
			t.Errorf("rejected %q", tag)
		}
	}
	for _, tag := range []string{"", "@", ".hidden", "v1.", "v1..2", "v1.lock", "v1/2", "v1 2", "v1?", "v1@{2}", "v1\x7f"} {
		if validReleaseTag(tag) {
			t.Errorf("accepted %q", tag)
		}
	}
}
