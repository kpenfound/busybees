package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// codexHeldProfile is the profile a held codex turn's tests run: granted a
// model, an effort and its own MCP server (grantedServer, defined in
// opencode_tools_test.go).
func codexHeldProfile() Profile {
	return Profile{Name: "builder", Agent: AgentCodex, Model: "m", Effort: "high", Timeout: time.Minute,
		MCP: map[string]MCPEntry{"tools": grantedServer}}
}

// codexHeldRequest is a host-placed request for a held codex turn granted
// tools and its own MCP server, the way a caller holding it to built-in
// tools would. It also grants the environment variable agenttest.CodexAppServer
// uses to make the test binary itself the fake app-server process, so the
// host boundary's env allowlist does not strip it from the process the turn
// actually runs.
func codexHeldRequest(t *testing.T, tools ...string) Request {
	t.Helper()
	req := grantAll(Request{Name: "g", Profile: codexHeldProfile(), Workspace: fakeWorkspace{dir: realTempDir(t)}, Prompt: "TASK"}, "FAKE_CODEX_APP_SERVER*")
	req.Grants.Tools = append(slices.Clone(tools), "mcp__tools")
	return req
}

// codexHeldPlacement is one placement codexWritableTools declares
// supported, with how to build a Runner and a Request granted apply_patch
// for it.
type codexHeldPlacement struct {
	where Placement
	setup func(t *testing.T, bin string) (*Runner, Request)
}

// codexHeldPlacements are the placements a held codex turn is tested in:
// host, confined host, container and sbx.
func codexHeldPlacements() []codexHeldPlacement {
	return []codexHeldPlacement{
		{Placement{Sandbox: SandboxNone}, func(t *testing.T, bin string) (*Runner, Request) {
			return &Runner{CodexBin: bin, SessionsDir: t.TempDir()}, codexHeldRequest(t, "apply_patch")
		}},
		{Placement{Sandbox: SandboxNone, Confine: true}, func(t *testing.T, bin string) (*Runner, Request) {
			work := realTempDir(t)
			p := codexHeldProfile()
			p.Confine = true
			req := Request{Name: "g", Profile: p, Workspace: fakeWorkspace{dir: work}, Prompt: "TASK",
				Grants: &Grants{Env: []string{"PATH"}, Tools: []string{"apply_patch", "mcp__tools"},
					Mounts: []Mount{{Path: work, Access: ReadWrite}}}}
			return &Runner{CodexBin: bin, SessionsDir: t.TempDir(), Confiner: &fakeConfiner{}, SystemPaths: []Mount{}}, req
		}},
		{Placement{Sandbox: SandboxContainer}, func(t *testing.T, bin string) (*Runner, Request) {
			work, session := realTempDir(t), realTempDir(t)
			p := codexHeldProfile()
			p.Sandbox, p.SandboxImage = SandboxContainer, "image"
			req := grantAll(Request{Name: "g", Profile: p, Workspace: fakeWorkspace{dir: work}, SessionDir: session, Prompt: "TASK", Env: map[string]string{"RUN_DIR": session}})
			req.Grants.Tools = []string{"apply_patch", "mcp__tools"}
			return &Runner{CodexBin: bin, SessionsDir: t.TempDir(), DockerBin: agenttest.Docker(t, "image", "RUN_DIR")}, req
		}},
		{Placement{Sandbox: SandboxSbx}, func(t *testing.T, bin string) (*Runner, Request) {
			work, session := realTempDir(t), realTempDir(t)
			p := codexHeldProfile()
			p.Sandbox = SandboxSbx
			req := grantAll(Request{Name: "g", Profile: p, Workspace: fakeWorkspace{dir: work}, SessionDir: session, Prompt: "TASK", Env: map[string]string{"RUN_DIR": session}})
			req.Grants.Tools = []string{"apply_patch", "mcp__tools"}
			return &Runner{CodexBin: bin, SessionsDir: t.TempDir(), SbxBin: agenttest.Sbx(t, "RUN_DIR")}, req
		}},
	}
}

// codexRecordStarts is how many times the fake started, or 0 when it never
// ran at all (a refused turn's record file is never created).
func codexRecordStarts(t *testing.T, record string) int {
	t.Helper()
	if _, err := os.Stat(record); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	return agenttest.ReadCodexAppServerRecord(t, record).Starts
}

// codexRecordedThreadStart finds the thread/start request the fake
// recorded and decodes its parameters.
func codexRecordedThreadStart(t *testing.T, rec agenttest.CodexAppServerRecord) codexRPCParams {
	t.Helper()
	for _, line := range rec.Lines {
		var l struct {
			Method string         `json:"method"`
			Params codexRPCParams `json:"params"`
		}
		if json.Unmarshal([]byte(line), &l) == nil && l.Method == "thread/start" {
			return l.Params
		}
	}
	t.Fatalf("thread/start was not recorded: %v", rec.Lines)
	return codexRPCParams{}
}

// setDotted sets a dotted key in a decoded JSON object, making the nested
// objects it needs along the way.
func setDotted(m map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = value
}

// codexConfigAnswer is config/read's config and origins for a held turn
// granted tools, with every codexHeldSettings key set to the value the
// command line gave it and codexSessionFlags as its origin: by default,
// what a turn whose command line won every held setting looks like.
func codexConfigAnswer(tools []string) (config, origins map[string]any) {
	config, origins = map[string]any{}, map[string]any{}
	for _, s := range codexHeldSettings(tools) {
		var v any
		_ = json.Unmarshal([]byte(s.value), &v)
		setDotted(config, s.key, v)
		origins[s.key] = map[string]any{"name": map[string]any{"type": codexSessionFlags}}
	}
	return config, origins
}

// codexCompletedActions scripts a fake `codex app-server`'s one action once
// turn/start is answered: a turn/completed notification reporting the turn
// as completed, so the driver's awaitTurnCompleted does not wait forever for
// a notification the script never schedules.
func codexCompletedActions() []agenttest.CodexAppServerAction {
	return []agenttest.CodexAppServerAction{
		{Notify: &agenttest.CodexAppServerMessage{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"status": "completed"}}}},
	}
}

// TestCodexHeldTurnRunsOnOneProcess: a held codex turn's command line
// carries codexHeldArgs, and its config/read check and its thread/start
// both reach the one `codex app-server` process the turn itself runs on
// (spec#4): the fake started once, and config/read precedes thread/start
// in what it received.
func TestCodexHeldTurnRunsOnOneProcess(t *testing.T) {
	config, origins := codexConfigAnswer([]string{"apply_patch"})
	bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{
		ConfigResult: map[string]any{"config": config, "origins": origins},
		ThreadID:     "thread-g",
		Actions:      codexCompletedActions(),
	})
	r, req := codexHeldPlacements()[0].setup(t, bin)
	res, err := r.Run(context.Background(), req)
	if err != nil || res.IsError || res.ClaudeID != "thread-g" {
		t.Fatalf("run: %+v, %v", res, err)
	}
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	if rec.Starts != 1 {
		t.Fatalf("the fake started %d times, want 1", rec.Starts)
	}
	args := rec.Argv[0]
	for _, want := range []string{
		"app-server", `approval_policy="never"`, "agents.enabled=false", "orchestrator.mcp.enabled=false",
		"tools.experimental_request_user_input.enabled=false", "tools.update_plan.enabled=false", `web_search="disabled"`,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %v", want, args)
		}
	}
	for _, gone := range []string{"exec", "--json", "--sandbox", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "-"} {
		if slices.Contains(args, gone) {
			t.Errorf("args carry %s, which an app-server turn does not take: %v", gone, args)
		}
	}
	configIdx, startIdx := -1, -1
	for i, line := range rec.Lines {
		switch {
		case strings.Contains(line, `"method":"config/read"`):
			configIdx = i
		case strings.Contains(line, `"method":"thread/start"`):
			startIdx = i
		}
	}
	if configIdx < 0 || startIdx < 0 || configIdx > startIdx {
		t.Fatalf("config/read did not precede thread/start on the one process: %v", rec.Lines)
	}
}

// TestCodexHeldTurnSandbox: thread/start's sandbox is read-only for a held
// turn not granted apply_patch and danger-full-access for one that is,
// with approvalPolicy always "never" (spec#5's table, for a held turn).
func TestCodexHeldTurnSandbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tools   []string
		sandbox string
	}{
		{"without apply_patch", nil, "read-only"},
		{"with apply_patch", []string{"apply_patch"}, "danger-full-access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, origins := codexConfigAnswer(tc.tools)
			bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{
				ConfigResult: map[string]any{"config": config, "origins": origins},
				ThreadID:     "thread-g",
				Actions:      codexCompletedActions(),
			})
			r := &Runner{CodexBin: bin, SessionsDir: t.TempDir()}
			req := codexHeldRequest(t, tc.tools...)
			res, err := r.Run(context.Background(), req)
			if err != nil || res.IsError {
				t.Fatalf("run: %+v, %v", res, err)
			}
			rec := agenttest.ReadCodexAppServerRecord(t, record)
			start := codexRecordedThreadStart(t, rec)
			if start.Sandbox != tc.sandbox {
				t.Errorf("sandbox = %q, want %q", start.Sandbox, tc.sandbox)
			}
			if start.ApprovalPolicy != "never" {
				t.Errorf("approvalPolicy = %q, want never", start.ApprovalPolicy)
			}
		})
	}
}

// TestCodexWritableTurnFailsClosedBeforeLaunch: every way config/read's
// answer can defeat the hold, or fail to say, ends the session as an
// error with no thread/start sent and no second process started
// (spec#4). "app server without an answer" covers the app-server process
// exiting before it answers config/read: codexAppServerEnded gives the
// driver's one deterministic message whether that showed up as a failed
// write or a read already at its end, so the assertion is on that message
// rather than on a pipe error.
func TestCodexWritableTurnFailsClosedBeforeLaunch(t *testing.T) {
	tools := []string{"apply_patch"}
	for _, tc := range []struct {
		name   string
		script agenttest.CodexAppServerScript
		want   string
	}{
		{"app server without an answer", agenttest.CodexAppServerScript{ExitBefore: "config/read"}, "the app server ended without answering config/read"},
		{"config/read refused", agenttest.CodexAppServerScript{ConfigError: &agenttest.CodexRPCError{Code: -32000, Message: "not allowed"}}, "config/read: not allowed"},
		{"config/read malformed", agenttest.CodexAppServerScript{ConfigResult: "x"}, "decode effective configuration"},
		{"config/read without origins", agenttest.CodexAppServerScript{ConfigResult: map[string]any{"config": map[string]any{}}}, "no config or origins"},
		{"a setting without an origin", func() agenttest.CodexAppServerScript {
			config, origins := codexConfigAnswer(tools)
			delete(origins, "tools.update_plan.enabled")
			return agenttest.CodexAppServerScript{ConfigResult: map[string]any{"config": config, "origins": origins}}
		}(), `effective setting "tools.update_plan.enabled" has no origin`},
		{"a setting with another value", func() agenttest.CodexAppServerScript {
			config, origins := codexConfigAnswer(tools)
			setDotted(config, "web_search", "live")
			return agenttest.CodexAppServerScript{ConfigResult: map[string]any{"config": config, "origins": origins}}
		}(), `effective setting "web_search" is live, want "disabled"`},
		{"approvals asked for", func() agenttest.CodexAppServerScript {
			config, origins := codexConfigAnswer(tools)
			setDotted(config, "approval_policy", "on-request")
			return agenttest.CodexAppServerScript{ConfigResult: map[string]any{"config": config, "origins": origins}}
		}(), `effective setting "approval_policy" is on-request`},
		{"a managed layer wins", func() agenttest.CodexAppServerScript {
			config, origins := codexConfigAnswer(tools)
			origins["agents.enabled"] = map[string]any{"name": map[string]any{"type": "mdm"}}
			return agenttest.CodexAppServerScript{ConfigResult: map[string]any{"config": config, "origins": origins}}
		}(), `effective setting "agents.enabled" comes from the "mdm" layer, not the turn's command line`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.script.ThreadID = "thread-g"
			bin, record := agenttest.CodexAppServer(t, tc.script)
			r := &Runner{CodexBin: bin, SessionsDir: t.TempDir()}
			req := codexHeldRequest(t, tools...)
			res, err := r.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || res.ErrorSubtype != "error" || !strings.Contains(res.ResultText, tc.want) {
				t.Fatalf("result = %+v, want an error containing %q", res, tc.want)
			}
			rec := agenttest.ReadCodexAppServerRecord(t, record)
			if rec.Starts != 1 {
				t.Fatalf("the fake started %d times, want 1", rec.Starts)
			}
			for _, line := range rec.Lines {
				if strings.Contains(line, `"method":"thread/start"`) {
					t.Fatalf("thread/start was sent after a failed held-settings check: %v", rec.Lines)
				}
			}
		})
	}
}

// Every placement the contract declares codex unsupported in is refused
// before anything starts, with ErrUnsupported naming the agent, the
// sandbox and the remedy.
func TestCodexWritableTurnRefusedWhereUnsupported(t *testing.T) {
	var unsupported []Placement
	for _, p := range Placements {
		if !codexWritableTools()[p].Supported {
			unsupported = append(unsupported, p)
		}
	}
	if len(unsupported) == 0 {
		t.Fatal("no unsupported placement declared")
	}
	for _, p := range unsupported {
		t.Run(p.String(), func(t *testing.T) {
			bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{ThreadID: "thread-g"})
			prof := codexHeldProfile()
			prof.Sandbox, prof.Confine = p.Sandbox, p.Confine
			req := grantAll(Request{Name: "g", Profile: prof, Workspace: fakeWorkspace{dir: realTempDir(t)}, Prompt: "TASK"})
			req.Grants.Tools = []string{"apply_patch", "mcp__tools"}
			r := &Runner{CodexBin: bin, SessionsDir: t.TempDir(), Confiner: &fakeConfiner{}}
			_, err := r.Run(context.Background(), req)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("error = %v, want ErrUnsupported", err)
			}
			for _, want := range []string{`"codex"`, `"` + p.Sandbox + `"`, "run codex in sandbox"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			if n := codexRecordStarts(t, record); n != 0 {
				t.Errorf("codex launched for a refused turn: %d starts", n)
			}
			if entries, _ := os.ReadDir(r.SessionsDir); len(entries) != 0 {
				t.Errorf("a session directory was made for a refused turn: %v", entries)
			}
		})
	}
}

// A grant codex has no tool for, and a grant its controls cannot express
// exactly, are refused before anything starts, in every placement, with
// ErrUnsupported naming the agent, the sandbox, the tools and the remedy.
func TestCodexWritableTurnRefusesWhatItCannotHold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []string
		want  []string
	}{
		{"another agent's tool name", []string{"Read"}, []string{`"codex"`, `no built-in tool "Read"`, "grant one of apply_patch, image_generation, shell"}},
		{"shell without apply_patch", []string{"shell"}, []string{`"codex"`, "in sandbox %s", `withholds "apply_patch"`, `would hold "shell" too`, `grant "apply_patch" as well, or leave out "shell"`}},
	} {
		for _, pl := range codexHeldPlacements() {
			t.Run(tc.name+"/"+pl.where.String(), func(t *testing.T) {
				bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{ThreadID: "thread-g"})
				r, req := pl.setup(t, bin)
				req.Grants.Tools = append(slices.Clone(tc.tools), "mcp__tools")
				_, err := r.Run(context.Background(), req)
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("error = %v, want ErrUnsupported", err)
				}
				for _, want := range tc.want {
					if strings.Contains(want, "%s") {
						want = strings.Replace(want, "%s", pl.where.String(), 1)
					}
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not say %s", err, want)
					}
				}
				if n := codexRecordStarts(t, record); n != 0 {
					t.Errorf("codex launched for a refused grant: %d starts", n)
				}
			})
		}
	}
}
