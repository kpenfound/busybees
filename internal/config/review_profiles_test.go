package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const reviewProfilesFixture = `version = 4
[profiles.base]
agent = "claude"
model = "base"
fallback_model = "base-fallback"
effort = "low"
sandbox = "claude"
[profiles.large]
agent = "codex"
model = "large"
fallback_model = "large-fallback"
effort = "max"
sandbox = "container"
[profiles.phase]
agent = "claude"
model = "phase"
fallback_model = "phase-fallback"
effort = "high"
sandbox = "none"
[roles.reviewer]
profile = "base"
profile_by_size = { l = "large" }
prompt = "role prompt"
max_turns = 17
timeout = "7m"
sandbox_image = "role-image"
env = { ROLE_ENV = "retained" }
skills = ["role-skill"]
mcp = { tool = { command = "role-command" } }
`

func TestReviewPhaseProfiles(t *testing.T) {
	for _, overrides := range []bool{false, true} {
		body := reviewProfilesFixture
		if overrides {
			body += "brief_profile = \"phase\"\njudge_profile = \"phase\"\nangle_profiles = { docs = \"phase\" }\n"
		}
		c, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatal(err)
		}
		r, _ := c.Role(RoleReviewer)
		for _, size := range []string{"", "s", "l", "unknown"} {
			sized := r.ForSize(size)
			want := sized.AgentProfile()
			if overrides {
				want = c.Profiles["phase"].resolved()
			}
			for _, phase := range []ResolvedRole{sized.ForBrief(), sized.ForJudge(), sized.ForAngle("docs")} {
				if got := phase.AgentProfile(); got != want {
					t.Errorf("size %q override %v: got %+v want %+v", size, overrides, got, want)
				}
				if phase.Prompt != "role prompt" || phase.MaxTurns != 17 || phase.Timeout != 7*time.Minute || phase.SandboxImage != "role-image" || phase.Env["ROLE_ENV"] != "retained" || !reflect.DeepEqual(phase.Skills, []string{"role-skill"}) || phase.MCP["tool"].Command != "role-command" || !reflect.DeepEqual(phase.AllowedTools, sized.AllowedTools) || !reflect.DeepEqual(phase.DisallowedTools, sized.DisallowedTools) {
					t.Errorf("lost role settings: %+v", phase)
				}
			}
			if got := sized.ForAngle("general").AgentProfile(); got != sized.AgentProfile() {
				t.Errorf("unspecified angle lost size fallback: %+v", got)
			}
		}
		v, err := c.View([]string{RoleReviewer})
		if err != nil {
			t.Fatal(err)
		}
		view := v.Roles[RoleReviewer]
		if view.BriefProfile != r.ForBrief().AgentProfile() || view.JudgeProfile != r.ForJudge().AgentProfile() || view.AngleProfiles["docs"] != r.ForAngle("docs").AgentProfile() || view.ReviewProfilesBySize["l"].AngleProfiles["general"] != r.ForSize("l").AgentProfile() {
			t.Fatalf("show did not resolve profiles: %+v", view)
		}
		data, _ := json.Marshal(view)
		for _, old := range []string{"brief_model", "judge_model", "angle_models"} {
			if strings.Contains(string(data), old) {
				t.Errorf("show contains %s", old)
			}
		}
		if !strings.Contains(view.HostReviewPolicy, "sandbox ignored") {
			t.Fatal("show omits host safety policy")
		}
	}
}

func TestReviewProfileValidation(t *testing.T) {
	for _, tc := range []struct{ body, path string }{
		{`[roles.reviewer]
brief_profile = "missing"`, "roles.reviewer.brief_profile"},
		{`[roles.reviewer]
judge_profile = "missing"`, "roles.reviewer.judge_profile"},
		{`[roles.reviewer]
angle_profiles.docs = "missing"`, "roles.reviewer.angle_profiles.docs"},
		{`[roles.reviewer]
angle_profiles.unknown = "host"`, "roles.reviewer.angle_profiles.unknown"},
		{`[roles.reviewer]
brief_profile = "remote"`, "roles.reviewer.brief_profile"},
		{`[roles.reviewer]
angle_profiles.docs = "remote"`, "roles.reviewer.angle_profiles.docs"},
		{`[roles.reviewer]
profile_by_size.l = "remote"`, "roles.reviewer.profile_by_size.l"},
		{`[global]
profile = "remote"`, "roles.reviewer.profile"},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			_, err := Load(writeConfig(t, "version = 4\n[profiles.host]\n[profiles.remote]\nagent = \"opencode\"\n"+tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("want %s, got %v", tc.path, err)
			}
		})
	}
	for _, scope := range []string{"global", "roles.developer", "roles.qa", "roles.product_manager", "roles.project_manager"} {
		for _, key := range []string{"brief_profile", "judge_profile", "angle_profiles"} {
			value := `"host"`
			if key == "angle_profiles" {
				value = `{ docs = "host" }`
			}
			_, err := Load(writeConfig(t, "version = 4\n[profiles.host]\n["+scope+"]\n"+key+" = "+value+"\n"))
			if err == nil || !strings.Contains(err.Error(), scope) || !strings.Contains(err.Error(), "only valid under roles.reviewer") {
				t.Fatalf("%s.%s: %v", scope, key, err)
			}
		}
	}
	// The judge can use the ordinary factory backends, including opencode.
	if _, err := Load(writeConfig(t, "version = 4\n[profiles.remote]\nagent = \"opencode\"\neffort = \"custom\"\n[roles.reviewer]\njudge_profile = \"remote\"\n")); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"prompt", "mcp", "skills", "env", "sandbox_image", "container_use_environment", "shell", "timeout", "max_turns", "vcs_access"} {
		_, err := Load(writeConfig(t, "version = 4\n[profiles.phase]\n"+field+" = \"invalid\"\n"))
		if err == nil || !strings.Contains(err.Error(), "profiles.phase."+field) {
			t.Errorf("profile accepted %s: %v", field, err)
		}
	}
}

func TestReviewProfileMigration(t *testing.T) {
	for _, body := range []string{
		"[roles.reviewer]\nbrief_model = \"brief\" # brief comment\njudge_model = \"judge\"\nangle_models = { docs = \"docs\", general = \"brief\" }\n",
		"roles.reviewer.brief_model = \"brief\" # brief comment\nroles.reviewer.judge_model = \"judge\"\nroles.reviewer.angle_models.docs = \"docs\"\nroles.reviewer.angle_models.general = \"brief\"\n",
		"[roles.reviewer]\nbrief_model = \"brief\" # brief comment\njudge_model = \"judge\"\n[roles.reviewer.angle_models]\ndocs = \"docs\"\ngeneral = \"brief\"\n",
		"roles = { reviewer = { brief_model = \"brief\", judge_model = \"judge\", angle_models = { docs = \"docs\", general = \"brief\" } } } # brief comment\n",
	} {
		t.Run(body, func(t *testing.T) {
			source := "version = 3\n# unrelated comment\n" + body + `[profiles.fallback]
agent = "codex"
model = "base"
fallback_model = "fallback"
effort = "max"
sandbox = "container"
[profiles.reuse]
agent = "codex"
model = "brief"
fallback_model = "fallback"
effort = "max"
sandbox = "container"
[global]
profile = "fallback"
prompt = """
brief_model = "embedded example"
"""
[project]
repo   = 'a/b' # exact formatting
`
			path := writeConfig(t, source)
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := c.Role(RoleReviewer)
			for phase, got := range map[string]AgentProfile{"brief": r.ForBrief().AgentProfile(), "judge": r.ForJudge().AgentProfile(), "docs": r.ForAngle("docs").AgentProfile(), "base": r.ForAngle("side_effects").AgentProfile()} {
				want := AgentProfile{Agent: "codex", Model: phase, FallbackModel: "fallback", Effort: "max", Sandbox: "container"}
				if got != want {
					t.Errorf("%s: got %+v want %+v", phase, got, want)
				}
			}
			if c.Roles[RoleReviewer].BriefProfile != "reuse" || c.Roles[RoleReviewer].AngleProfiles["general"] != "reuse" || len(c.Profiles) != 4 {
				t.Fatalf("did not reuse equal profiles: %+v", c.Profiles)
			}
			if _, err := c.Rewrite(); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			for _, kept := range []string{"# unrelated comment", "# brief comment", "repo   = 'a/b' # exact formatting", "brief_model = \"embedded example\""} {
				if !strings.Contains(string(b), kept) {
					t.Errorf("lost %q", kept)
				}
			}
			again, err := Load(path)
			if err != nil || again.NeedsRewrite() {
				t.Fatalf("reload: %v", err)
			}
			twice, err := migrateReviewProfiles(string(b))
			if err != nil || twice != string(b) {
				t.Fatalf("not idempotent: %v", err)
			}
		})
	}
}

func TestReviewMigrationClonesRoleFallbackAndKeepsComments(t *testing.T) {
	source := `version = 2
[global]
agent = "codex"
effort = "high"
sandbox = "container"
[roles.reviewer]
model = "role-model"
fallback_model = "role-fallback"
brief_model = "brief-choice" # preserve choice
judge_model = "judge-choice"
#angle_models = { docs = "example" } # preserve explanation
`
	c, err := Load(writeConfig(t, source))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := c.Role(RoleReviewer)
	for model, p := range map[string]AgentProfile{"brief-choice": r.ForBrief().AgentProfile(), "judge-choice": r.ForJudge().AgentProfile()} {
		want := AgentProfile{Agent: "codex", Model: model, FallbackModel: "role-fallback", Effort: "high", Sandbox: "container"}
		if p != want {
			t.Errorf("got %+v want %+v", p, want)
		}
	}
	if !strings.Contains(c.migrated, "# preserve choice") || !strings.Contains(c.migrated, "#angle_profiles") || !strings.Contains(c.migrated, "# preserve explanation") {
		t.Fatalf("comments missing: %s", c.migrated)
	}
	if twice, err := migrateReviewProfiles(c.migrated); err != nil || twice != c.migrated {
		t.Fatalf("comment migration not idempotent: %v", err)
	}
}

func TestReviewProfileMigrationCollisionAndConflicts(t *testing.T) {
	c, err := Load(writeConfig(t, `version = 3
[profiles.reviewer_brief_profile]
model = "occupied"
[roles.reviewer]
brief_model = "brief-choice"
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Roles[RoleReviewer].BriefProfile != "reviewer_brief_profile_2" || c.Profiles["reviewer_brief_profile"].Model != "occupied" {
		t.Fatalf("overwrote existing profile: %+v", c.Profiles)
	}
	for _, body := range []string{
		"[roles.reviewer]\nbrief_model = \"old\"\nbrief_profile = \"new\"\n",
		"roles.reviewer = { judge_model = \"old\", judge_profile = \"new\" }\n",
		"[roles.reviewer.angle_models]\ndocs = \"old\"\n[roles.reviewer.angle_profiles]\ngeneral = \"new\"\n",
	} {
		if _, err := migrateReviewProfiles(body); err == nil || !strings.Contains(err.Error(), "cannot mix legacy") {
			t.Fatalf("conflicting keys accepted: %v", err)
		}
	}
}

func TestReviewProfileEmptyKeysAreReviewerOnly(t *testing.T) {
	for _, scope := range []string{"global", "roles.developer"} {
		for _, key := range []string{"brief_profile", "judge_profile", "angle_profiles"} {
			value := `""`
			if key == "angle_profiles" {
				value = "{}"
			}
			_, err := Load(writeConfig(t, "version = 4\n["+scope+"]\n"+key+" = "+value+"\n"))
			if err == nil || !strings.Contains(err.Error(), scope) || !strings.Contains(err.Error(), "only valid under roles.reviewer") {
				t.Fatalf("%s.%s: %v", scope, key, err)
			}
		}
	}
}

func TestReviewPhaseSandboxValidation(t *testing.T) {
	for _, key := range []string{"brief_profile", "angle_profiles.docs", "judge_profile"} {
		c, err := Load(writeConfig(t, "version = 4\n[profiles.phase]\nagent = \"codex\"\nsandbox = \"claude\"\n[roles.reviewer]\n"+key+" = \"phase\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		err = c.CheckSandbox()
		if key == "judge_profile" {
			if err == nil || !strings.Contains(err.Error(), "codex") {
				t.Fatalf("judge escaped sandbox validation: %v", err)
			}
		} else if err != nil {
			t.Fatalf("%s applied ignored sandbox: %v", key, err)
		}
	}
}

func TestJudgeSandboxKeepsReviewerContainerSettings(t *testing.T) {
	for _, tc := range []struct {
		name, base, large, judge, environment, image, wantError string
	}{
		{name: "container reviewer and unboxed judge", base: "container", large: "container", judge: "none", environment: "envs/reviewer"},
		{name: "unboxed reviewer and container judge", base: "none", large: "none", judge: "container", image: "review-image"},
		{name: "ordinary reviewer still requires container", base: "none", large: "container", judge: "container", environment: "envs/reviewer", wantError: `container_use_environment is only valid when sandbox is "container"`},
		{name: "size profile still requires container", base: "container", large: "none", judge: "container", environment: "envs/reviewer", wantError: `container_use_environment is only valid when sandbox is "container"`},
		{name: "container settings remain exclusive", base: "container", large: "container", judge: "none", environment: "envs/reviewer", image: "review-image", wantError: "container_use_environment and sandbox_image are mutually exclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeConfig(t, fmt.Sprintf(`version = 4
[profiles.base]
sandbox = %q
[profiles.large]
sandbox = %q
[profiles.judge]
sandbox = %q
[roles.reviewer]
profile = "base"
profile_by_size = { l = "large" }
judge_profile = "judge"
container_use_environment = %q
sandbox_image = %q
[roles.reviewer.env]
GH_TOKEN = "test-token"
ANTHROPIC_API_KEY = "test-key"
`, tc.base, tc.large, tc.judge, tc.environment, tc.image)))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), "roles.reviewer: "+tc.wantError) {
					t.Fatalf("Load() = %v, want reviewer error %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Role(RoleReviewer)
			if err != nil {
				t.Fatal(err)
			}
			for _, size := range []string{"", "l"} {
				judge := r.ForSize(size).ForJudge()
				if judge.Sandbox != tc.judge || judge.ContainerUseEnvironment != tc.environment || judge.SandboxImage != tc.image {
					t.Errorf("size %q: judge sandbox/container settings = %q/%q/%q, want %q/%q/%q", size, judge.Sandbox, judge.ContainerUseEnvironment, judge.SandboxImage, tc.judge, tc.environment, tc.image)
				}
			}
			machine(t, &fakeMachine{onPath: []string{"docker"}})
			if err := c.CheckSandbox(); err != nil {
				t.Fatalf("valid reviewer/judge sandbox combination failed startup: %v", err)
			}
		})
	}
}
