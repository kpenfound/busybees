package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func profileOf(r ResolvedRole) AgentProfile {
	return AgentProfile{Agent: r.Agent, Model: r.Model, FallbackModel: r.FallbackModel, Effort: r.Effort, Sandbox: r.Sandbox}
}

func TestProfileResolution(t *testing.T) {
	const profiles = `
[profiles.claude]
[profiles.remote]
agent = "opencode"
effort = "custom-variant"
[profiles.code]
agent = "codex"
model = "code-model"
sandbox = "container"
`
	for _, role := range Roles {
		t.Run(role, func(t *testing.T) {
			for _, tc := range []struct{ global, local, size, want string }{
				{"", "", "xs", "claude"},
				{`profile = "remote"`, "", "xs", "remote"},
				{"profile = \"claude\"\nprofile_by_size = { xs = \"remote\" }", "", "xs", "remote"},
				{"profile = \"claude\"\nprofile_by_size = { xs = \"remote\" }", `profile = "code"`, "xs", "code"},
				{`profile = "code"`, "profile = \"remote\"\nprofile_by_size = { xs = \"claude\" }", "xs", "claude"},
				{`profile_by_size = { xs = "code" }`, `profile_by_size = { s = "remote" }`, "xs", "code"},
				{`profile_by_size = { xs = "code" }`, `profile_by_size = { s = "remote" }`, "s", "remote"},
				{`profile_by_size = { xs = "code" }`, "", "", "claude"},
				{`profile_by_size = { xs = "code" }`, "", "unknown", "claude"},
			} {
				cfg, err := Load(writeConfig(t, "version = 3\n"+profiles+"\n[global]\n"+tc.global+"\n[roles."+role+"]\n"+tc.local+"\n"))
				if err != nil {
					t.Fatal(err)
				}
				r, err := cfg.Role(role)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]AgentProfile{
					"claude": {Agent: "claude", Model: "opus", FallbackModel: "sonnet", Sandbox: "none"},
					"remote": {Agent: "opencode", Effort: "custom-variant", Sandbox: "none"},
					"code":   {Agent: "codex", Model: "code-model", Sandbox: "container"},
				}[tc.want]
				if got := profileOf(r.ForSize(tc.size)); got != want {
					t.Errorf("%+v: got %+v want %+v", tc, got, want)
				}
			}
		})
	}
}

func TestProfileMigration(t *testing.T) {
	defaults := AgentProfile{Agent: "claude", Model: "opus", FallbackModel: "sonnet", Sandbox: "none"}
	for _, tc := range []struct {
		name, body, role, size string
		want                   AgentProfile
		count                  int
	}{
		{"implicit", "[global]\n#model = \"opus\"\n[roles.developer]\nprompt = \"keep\"\n", RoleDeveloper, "", defaults, 0},
		{"global", "[global]\nmodel = \"haiku\"\neffort = \"high\"\n", RoleQA, "", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Effort: "high", Sandbox: "none"}, 1},
		{"role", "[roles.developer]\nsandbox = \"claude\"\n", RoleDeveloper, "", AgentProfile{Agent: "claude", Model: "opus", FallbackModel: "sonnet", Sandbox: "claude"}, 1},
		{"inherit explicit global model", "[global]\nmodel = \"custom\"\neffort = \"high\"\n[roles.developer]\nagent = \"codex\"\n", RoleDeveloper, "", AgentProfile{Agent: "codex", Model: "custom", Effort: "high", Sandbox: "none"}, 2},
		{"codex empty defaults", "[roles.developer]\nagent = \"codex\"\n", RoleDeveloper, "", AgentProfile{Agent: "codex", Sandbox: "none"}, 1},
		{"opencode empty defaults", "[roles.developer]\nagent = \"opencode\"\n", RoleDeveloper, "", AgentProfile{Agent: "opencode", Sandbox: "none"}, 1},
		{"global agent role override", "[global]\nagent = \"opencode\"\n[roles.developer]\nagent = \"claude\"\n", RoleDeveloper, "", defaults, 2},
		{"size only", "[roles.developer]\nmodel_by_size = { xs = \"haiku\" }\n", RoleDeveloper, "xs", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Sandbox: "none"}, 2},
		{"size subtable", "[roles.developer.model_by_size]\nxs = \"haiku\"\n", RoleDeveloper, "xs", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Sandbox: "none"}, 2},
		{"quoted dotted keys", "roles.\"developer\".model = \"haiku\"\n", RoleDeveloper, "", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Sandbox: "none"}, 1},
		{"inline global", `global = { model = "haiku", prompt = "keep", env = { model = "env" } }
`, RoleQA, "", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Sandbox: "none"}, 1},
		{"inline role", `[roles]
developer = { model = "haiku", skills = ["a", "b"] }
`, RoleDeveloper, "", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Sandbox: "none"}, 1},
		{"inline roles", `roles = { developer = { model = "haiku" }, qa = { agent = "codex" } }
`, RoleDeveloper, "", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Sandbox: "none"}, 2},

		{"collision", "[profiles.developer]\nmodel = \"keep\"\n[roles.developer]\nmodel = \"haiku\"\n", RoleDeveloper, "", AgentProfile{Agent: "claude", Model: "haiku", FallbackModel: "sonnet", Sandbox: "none"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := "# user's comment\nversion = 2\n" + tc.body
			path := writeConfig(t, orig)
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := cfg.Role(tc.role)
			if err != nil {
				t.Fatal(err)
			}
			if got := profileOf(r.ForSize(tc.size)); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			if len(cfg.Profiles) != tc.count {
				t.Fatalf("profiles: %+v", cfg.Profiles)
			}
			backup, err := cfg.Rewrite()
			if err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(backup); string(b) != orig {
				t.Fatal("backup changed")
			}
			b, _ := os.ReadFile(path)
			if !strings.Contains(string(b), "# user's comment") {
				t.Fatal("lost comment")
			}
			if tc.count == 0 && strings.Contains(string(b), "\n[profiles.") {
				t.Fatal("generated a default profile")
			}
			again, err := Load(path)
			if err != nil || again.NeedsRewrite() {
				t.Fatalf("reload: %v", err)
			}
			rerole, _ := again.Role(tc.role)
			if !reflect.DeepEqual(r, rerole) {
				t.Fatal("rewrite changed resolution")
			}
			if twice, err := migrateAgentProfiles(string(b)); err != nil || twice != string(b) {
				t.Fatalf("migration not idempotent: %v", err)
			}
		})
	}
}

func TestProfileMigrationSequentialInlineRoles(t *testing.T) {
	for _, prefix := range []string{"", "[roles]\n"} {
		t.Run(prefix, func(t *testing.T) {
			key := "roles."
			if prefix != "" {
				key = ""
			}
			body := "version = 2\n" + prefix + key + `"developer" = { model = "haiku", skills = ["keep"] } # developer comment
` + key + `qa = { model = "sonnet", env = { model = "keep" } } # qa comment
`
			path := writeConfig(t, body)
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cfg.Rewrite(); err != nil {
				t.Fatal(err)
			}
			again, err := Load(path)
			if err != nil || again.NeedsRewrite() {
				t.Fatalf("reload: %v", err)
			}
			for role, model := range map[string]string{RoleDeveloper: "haiku", RoleQA: "sonnet"} {
				r, err := again.Role(role)
				if err != nil || r.Model != model {
					t.Fatalf("%s: model %q, error %v", role, r.Model, err)
				}
				if role == RoleDeveloper && !reflect.DeepEqual(r.Skills, []string{"keep"}) || role == RoleQA && r.Env["model"] != "keep" {
					t.Fatalf("lost unrelated settings: %+v", r)
				}
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, comment := range []string{"# developer comment", "# qa comment"} {
				if !strings.Contains(string(b), comment) {
					t.Fatalf("lost %s", comment)
				}
			}
			if twice, err := migrateAgentProfiles(string(b)); err != nil || twice != string(b) {
				t.Fatalf("migration not idempotent: %v", err)
			}
		})
	}
}

func TestProfileMigrationPreservesPromptAndComments(t *testing.T) {
	body := "version = 2\n[global]\nprompt = '''\n[roles.developer]\nmodel = \"example\"\n'''\nmodel = '''haiku''' # keep inline\n[roles.developer]\n#agent = \"claude\"\n[roles.developer.env]\nmodel = \"env model\"\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.Role(RoleDeveloper)
	if r.Model != "haiku" || r.Env["model"] != "env model" || r.Prompt != "[roles.developer]\nmodel = \"example\"" {
		t.Fatalf("corrupted settings: %+v", r)
	}
	if !strings.Contains(cfg.migrated, "# keep inline") {
		t.Fatal("lost inline comment")
	}
}

func TestProfileValidation(t *testing.T) {
	for _, scope := range append([]string{"global"}, func() []string {
		var s []string
		for _, r := range Roles {
			s = append(s, "roles."+r)
		}
		return s
	}()...) {
		for _, key := range legacyProfileKeys {
			t.Run(scope+"."+key, func(t *testing.T) {
				value := `"claude"`
				if key == "model_by_size" {
					value = `{ xs = "haiku" }`
				}
				_, err := Load(writeConfig(t, fmt.Sprintf("version = 3\n[%s]\n%s = %s\n", scope, key, value)))
				if err == nil || !strings.Contains(err.Error(), scope+"."+key) {
					t.Fatalf("error: %v", err)
				}
			})
		}
		for _, key := range []string{"profile", "profile_by_size"} {
			value := `"missing"`
			if key == "profile_by_size" {
				value = `{ xs = "missing" }`
			}
			_, err := Load(writeConfig(t, fmt.Sprintf("version = 3\n[%s]\n%s = %s\n", scope, key, value)))
			if err == nil || !strings.Contains(err.Error(), scope+"."+key) || !strings.Contains(err.Error(), "missing") {
				t.Fatalf("error: %v", err)
			}
		}
	}
	for body, want := range map[string]string{
		"[profiles.bad]\nagent = \"invalid\"":                       "profiles.bad.agent",
		"[profiles.bad]\nsandbox = \"invalid\"":                     "profiles.bad.sandbox",
		"[profiles.bad]\nprompt = \"invalid\"":                      "profiles.bad.prompt",
		"[profiles.bad]\nmodel_by_size = {}":                        "profiles.bad.model_by_size",
		"[global]\nprofile_by_size = { xxl = \"p\" }\n[profiles.p]": "unknown size",
		"[global]\nprofile_by_size = { xs = \"\" }":                 "profile_by_size.xs",
	} {
		if _, err := Load(writeConfig(t, "version = 3\n"+body+"\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
	}
	for _, tc := range []struct {
		name, agent, effort string
		valid               bool
	}{
		{name: "opencode variant", agent: AgentOpenCode, effort: "custom-variant", valid: true},
		{name: "explicit claude", agent: AgentClaude, effort: "invalid"},
		{name: "explicit codex", agent: AgentCodex, effort: "invalid"},
		{name: "default claude", effort: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "version = 3\n[profiles.test]\n"
			if tc.agent != "" {
				body += fmt.Sprintf("agent = %q\n", tc.agent)
			}
			body += fmt.Sprintf("effort = %q\n", tc.effort)
			_, err := Load(writeConfig(t, body))
			if tc.valid {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "profiles.test.effort") {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestSizeProfileSandboxValidation(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 3\n[profiles.bad]\nagent = \"codex\"\nsandbox = \"claude\"\n[roles.qa]\nprofile_by_size = { xs = \"bad\" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.CheckSandbox(); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("size profile escaped sandbox check: %v", err)
	}
}
