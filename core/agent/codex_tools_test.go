package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// codexInheritedFeatures is what the fake's `codex features list` reports
// from the user's and the project's configuration, before the runner's
// overrides: every tool on, a removed feature codex ignores, and a stage
// of two words.
const codexInheritedFeatures = `apply_patch_streaming_events  under development  false
apps                          stable             true
code_mode_host                stable             true
image_generation              stable             true
item_ids                      removed            true
memories                      stable             false
multi_agent                   stable             true
shell_tool                    stable             true
sleep_tool                    stable             true
unified_exec                  stable             true
view_image                    stable             true
web_search_request            deprecated         false`

// codexFeatures is the fake's `codex features list`: the inherited
// features with every `-c features.<name>=false` among the probe's
// arguments ($probe) applied, then managed, a sed script standing for a
// managed configuration that has the last word.
func codexFeatures(managed string) string {
	return `list='` + codexInheritedFeatures + `'
for f in $(sed -n 's/^features\.\([a-z_]*\)=false$/\1/p' "$probe"); do
  list="$(printf '%s\n' "$list" | sed "s/^\($f .*\)true\$/\1false/")"
done
printf '%s\n' "$list" | sed '` + managed + `'`
}

// codexOwnTransport is how the fake reports the session's own server,
// grantedServer, when the probe's arguments carry it.
const codexOwnTransport = `{"type":"stdio","command":"/bin/sh","args":["-c","touch mcp-called"],"env":null,"env_vars":[],"cwd":null}`

// codexServers is the fake's `codex mcp list --json`: an inherited server,
// legacy, enabled unless the probe's arguments disable it, and the
// session's own server when they carry it, reported as own (a transport).
func codexServers(own string) string {
	return `legacy=true
if grep -qF '"legacy"={enabled=false}' "$probe"; then legacy=false; fi
own=''
if grep -q '^mcp_servers\.tools\.command=' "$probe"; then own=',{"name":"tools","enabled":true,"transport":` + own + `}'; fi
printf '[{"name":"legacy","enabled":%s,"transport":{"type":"stdio","command":"inherited","args":[],"env":null,"env_vars":[],"cwd":null}}%s]\n' "$legacy" "$own"`
}

// codexGrantedFake stands in for codex. `features list` and `mcp list`
// append their arguments to record.config-args and print features and
// servers, shell snippets that read the probe's arguments from $probe;
// the turn records its arguments and environment, marks that it launched,
// and uses its tools the way codex would under the configuration it was
// handed: apply_patch writes edited.txt in the working directory unless
// the read-only sandbox rejects it, the shell touches a marker unless
// shell_tool is off, the session's "tools" server runs the script its
// overrides give it, the inherited legacy server and web search touch
// markers unless they are switched off. A tool the configuration does not
// give ends as a failed item instead.
func codexGrantedFake(t *testing.T, features, servers string) (string, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record")
	script := `if [ "$1 $2" = "features list" ] || [ "$1 $2" = "mcp list" ]; then
  probe="` + record + `.probe"
  printf '%s\n' "$@" > "$probe"
  cat "$probe" >> "` + record + `.config-args"
  if [ -n "$RUN_DIR" ] && [ -f "$RUN_DIR/docker-args.txt" ]; then cp "$RUN_DIR/docker-args.txt" "` + record + `.probe-engine"; fi
  if [ "$1" = features ]; then
` + features + `
  else
` + servers + `
  fi
  exit 0
fi
printf '%s\n' "$@" > "` + record + `.args"
env > "` + record + `.env"
touch "` + record + `.launched"
has() { grep -qxF -- "$1" "` + record + `.args"; }
item() { printf '{"type":"item.completed","item":{"type":"%s","status":"%s"}}\n' "$1" "$2"; }
echo '{"type":"thread.started","thread_id":"thread-g"}'
if has --dangerously-bypass-approvals-and-sandbox; then echo written > edited.txt; item file_change completed; else item file_change failed; fi
if has features.shell_tool=false; then item command_execution failed; else touch "` + record + `.shell"; item command_execution completed; fi
server="$(sed -n 's/^mcp_servers\.tools\.args=\["-c","\(.*\)"\]$/\1/p' "` + record + `.args")"
if [ -n "$server" ]; then /bin/sh -c "$server"; item mcp_tool_call completed; else item mcp_tool_call failed; fi
if grep -qF '"legacy"={enabled=false}' "` + record + `.args"; then item mcp_tool_call failed; else touch "` + record + `.legacy"; item mcp_tool_call completed; fi
if has 'web_search="disabled"'; then item web_search failed; else touch "` + record + `.web"; item web_search completed; fi
echo '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
echo '{"type":"turn.completed"}'
`
	return agenttest.Script(t, "codex", script), record
}

func codexGrantedDefault(t *testing.T) (string, string) {
	t.Helper()
	return codexGrantedFake(t, codexFeatures(""), codexServers(codexOwnTransport))
}

var codexHeld = heldAgent{
	profile: func() Profile {
		return Profile{Name: "builder", Agent: AgentCodex, Model: "m", Effort: "high", Timeout: time.Minute,
			MCP: map[string]MCPEntry{"tools": grantedServer}}
	},
	runner: func(bin string) *Runner { return &Runner{CodexBin: bin} },
	tools:  []string{"apply_patch", "mcp__tools"},
	marker: "list",
	// Features and servers, then both again with the whole configuration.
	probes: 4,
}

// TestCodexWritableTurnIsHeldToItsGrantedTools: in every placement the
// contract declares supported, a writable codex turn granted apply_patch
// and its own MCP server writes through apply_patch and calls the server,
// and nothing else the inherited configuration turned on is there: the
// shell, the other tool features, update_plan, web search and the
// inherited MCP server are switched off. The features that give no tool,
// and the removed one, are left alone. The configuration is inspected
// where the turn runs.
func TestCodexWritableTurnIsHeldToItsGrantedTools(t *testing.T) {
	for _, pl := range heldPlacements(codexHeld) {
		t.Run(pl.where.String(), func(t *testing.T) {
			if !codexWritableTools()[pl.where].Supported {
				t.Fatalf("%s is not declared supported", pl.where)
			}
			bin, record := codexGrantedDefault(t)
			r, req, probed := pl.setup(t, bin)
			if PlacementOf(req.Profile) != pl.where {
				t.Fatalf("the request runs in %s", PlacementOf(req.Profile))
			}
			res, err := r.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError || res.ClaudeID != "thread-g" {
				t.Fatalf("result: %+v", res)
			}
			probed(t, record)
			args := lines(t, record+".args")
			for _, want := range []string{
				"--dangerously-bypass-approvals-and-sandbox", `approval_policy="never"`, "agents.enabled=false", "orchestrator.mcp.enabled=false",
				"tools.update_plan.enabled=false", "tools.experimental_request_user_input.enabled=false", `web_search="disabled"`,
				"features.apps=false", "features.image_generation=false", "features.multi_agent=false", "features.shell_tool=false",
				"features.sleep_tool=false", "features.view_image=false", `mcp_servers={"legacy"={enabled=false}}`,
				`mcp_servers.tools.command="/bin/sh"`,
			} {
				if !slices.Contains(args, want) {
					t.Errorf("args lack %s: %v", want, args)
				}
			}
			for _, kept := range []string{"features.code_mode_host=false", "features.unified_exec=false", "features.item_ids=false", "features.memories=false", "--sandbox"} {
				if slices.Contains(args, kept) {
					t.Errorf("args carry %s: %v", kept, args)
				}
			}
			// The granted tool wrote into the working directory and the
			// session's server ran; nothing else did.
			if b, err := os.ReadFile(filepath.Join(req.workDir(), "edited.txt")); err != nil || string(b) != "written\n" {
				t.Errorf("apply_patch did not write: %q, %v", b, err)
			}
			if _, err := os.Stat(filepath.Join(req.workDir(), "mcp-called")); err != nil {
				t.Errorf("the session's MCP tool did not run: %v", err)
			}
			for _, marker := range []string{".shell", ".legacy", ".web"} {
				if _, err := os.Stat(record + marker); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s ran without being granted: %v", marker, err)
				}
			}
			transcript, _ := os.ReadFile(res.Transcript)
			for _, want := range []string{`"type":"file_change","status":"completed"`, `"type":"command_execution","status":"failed"`, `"type":"web_search","status":"failed"`} {
				if !strings.Contains(string(transcript), want) {
					t.Errorf("transcript lacks %s:\n%s", want, transcript)
				}
			}
		})
	}
}

// A granted tool keeps what the inherited configuration gives it: the
// shell and view_image stay on, and so does web search.
func TestCodexWritableTurnKeepsItsGrantedTools(t *testing.T) {
	bin, record := codexGrantedDefault(t)
	r, req, _ := heldPlacements(codexHeld)[0].setup(t, bin)
	req.Grants.Tools = []string{"apply_patch", "shell", "update_plan", "view_image", "web_search", "mcp__tools"}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	args := lines(t, record+".args")
	for _, dropped := range []string{"features.shell_tool=false", "features.view_image=false", "tools.update_plan.enabled=false", `web_search="disabled"`} {
		if slices.Contains(args, dropped) {
			t.Errorf("args carry %s for a granted tool: %v", dropped, args)
		}
	}
	if !slices.Contains(args, "features.image_generation=false") {
		t.Errorf("an ungranted tool was left on: %v", args)
	}
	for _, marker := range []string{".shell", ".web"} {
		if _, err := os.Stat(record + marker); err != nil {
			t.Errorf("granted %s did not run: %v", marker, err)
		}
	}
}

// A codex turn not granted apply_patch runs in codex's read-only sandbox,
// where the patch is rejected, and its MCP server still runs.
func TestCodexWritableTurnWithoutApplyPatchIsReadOnly(t *testing.T) {
	bin, record := codexGrantedDefault(t)
	r, req, _ := heldPlacements(codexHeld)[0].setup(t, bin)
	req.Grants.Tools = []string{"mcp__tools"}
	res, err := r.Run(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	args := lines(t, record+".args")
	if flagValue(args, "--sandbox") != "read-only" || slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") || !slices.Contains(args, `approval_policy="never"`) {
		t.Errorf("args: %v", args)
	}
	if _, err := os.Stat(filepath.Join(req.workDir(), "edited.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("apply_patch wrote without being granted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(req.workDir(), "mcp-called")); err != nil {
		t.Errorf("the session's MCP tool did not run: %v", err)
	}
}

// TestCodexWritableTurnRefusesAWiderEffectiveConfiguration: in every
// supported placement, a managed configuration that keeps the shell on
// over the runner's override is found by the inventory where the turn
// runs, and the turn is refused before codex is launched.
func TestCodexWritableTurnRefusesAWiderEffectiveConfiguration(t *testing.T) {
	for _, pl := range heldPlacements(codexHeld) {
		t.Run(pl.where.String(), func(t *testing.T) {
			bin, record := codexGrantedFake(t, codexFeatures(`s/^\(shell_tool .*\)false$/\1true/`), codexServers(codexOwnTransport))
			r, req, _ := pl.setup(t, bin)
			_, err := r.Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), `effective feature "shell_tool" is still enabled`) {
				t.Fatalf("error = %v", err)
			}
			if _, err := os.Stat(record + ".launched"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("codex launched on a widened configuration: %v", err)
			}
		})
	}
}

// TestCodexWritableTurnFailsClosedBeforeLaunch: every way the effective
// configuration can defeat the hold, or fail to say, refuses the turn
// before codex is launched.
func TestCodexWritableTurnFailsClosedBeforeLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, features, servers, want string
	}{
		{"failed feature inventory", `echo broken >&2; exit 3`, codexServers(codexOwnTransport), "list features"},
		{"unreadable feature inventory", `echo 'shell_tool stable maybe'`, codexServers(codexOwnTransport), "unreadable line"},
		{"empty feature inventory", `true`, codexServers(codexOwnTransport), "no feature listed"},
		{"a new feature kept on", codexFeatures(`$a\
new_tool                      stable             true`), codexServers(codexOwnTransport), `effective feature "new_tool" is still enabled`},
		{"failed server inventory", codexFeatures(""), `echo broken >&2; exit 3`, "list MCP servers"},
		{"malformed server inventory", codexFeatures(""), `echo invalid`, "decode MCP servers"},
		{"null server inventory", codexFeatures(""), `echo null`, "must be an array"},
		{"inherited server re-enabled", codexFeatures(""), `echo '[{"name":"legacy","enabled":true},{"name":"tools","enabled":true,"transport":` + codexOwnTransport + `}]'`, `MCP server "legacy" is not disabled`},
		{"inherited server enabled by default", codexFeatures(""), `echo '[{"name":"legacy"},{"name":"tools","enabled":true,"transport":` + codexOwnTransport + `}]'`, `MCP server "legacy" is not disabled`},
		{"own server replaced", codexFeatures(""), codexServers(`{"type":"stdio","command":"evil","args":["-c","touch mcp-called"]}`), `MCP server "tools" is not the session's own`},
		{"own server given an environment", codexFeatures(""), codexServers(`{"type":"stdio","command":"/bin/sh","args":["-c","touch mcp-called"],"env":{"NODE_OPTIONS":"--require /tmp/evil.js"}}`), `MCP server "tools" is not the session's own`},
		{"own server given a directory", codexFeatures(""), codexServers(`{"type":"stdio","command":"/bin/sh","args":["-c","touch mcp-called"],"cwd":"/tmp"}`), `MCP server "tools" is not the session's own`},
		{"own server given a key", codexFeatures(""), codexServers(`{"type":"stdio","command":"/bin/sh","args":["-c","touch mcp-called"],"wrapper":"sudo"}`), `MCP server "tools" is not the session's own`},
		{"own server reached over http", codexFeatures(""), codexServers(`{"type":"streamable_http","url":"http://evil.example/mcp"}`), `MCP server "tools" is not the session's own`},
		{"own server without a transport", codexFeatures(""), `echo '[{"name":"legacy","enabled":false},{"name":"tools","enabled":true}]'`, `MCP server "tools" is not the session's own`},
		{"own server disabled", codexFeatures(""), `echo '[{"name":"legacy","enabled":false},{"name":"tools","enabled":false,"transport":` + codexOwnTransport + `}]'`, `MCP server "tools", the session's own, is not enabled`},
		{"own server removed", codexFeatures(""), `echo '[{"name":"legacy","enabled":false}]'`, `removed the session's MCP server "tools"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := codexGrantedFake(t, tc.features, tc.servers)
			r, req, _ := heldPlacements(codexHeld)[0].setup(t, bin)
			_, err := r.Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "granted codex setup") {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(record + ".launched"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("codex launched after the granted setup failed: %v", err)
			}
		})
	}
}

// Every placement the contract declares codex unsupported in is refused
// before anything starts, the inventories included, with ErrUnsupported
// naming the agent, the sandbox and the remedy.
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
			bin, record := codexGrantedDefault(t)
			prof := codexHeld.profile()
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
			for _, marker := range []string{".launched", ".config-args"} {
				if _, err := os.Stat(record + marker); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s written for a refused turn: %v", marker, err)
				}
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
		for _, pl := range heldPlacements(codexHeld) {
			t.Run(tc.name+"/"+pl.where.String(), func(t *testing.T) {
				bin, record := codexGrantedDefault(t)
				r, req, _ := pl.setup(t, bin)
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
				for _, marker := range []string{".launched", ".config-args"} {
					if _, err := os.Stat(record + marker); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("%s written for a refused turn: %v", marker, err)
					}
				}
			})
		}
	}
}

// A session's remote server is compared whole: its url, bearer variable
// and headers, and nothing added.
func TestCodexHoldComparesTheSessionServersWhole(t *testing.T) {
	own := map[string]MCPEntry{"remote": {URL: "https://x.example/mcp", BearerTokenEnv: "TOKEN", Headers: map[string]string{"X-Role": "builder"}}}
	for _, tc := range []struct {
		name, transport, want string
	}{
		{"as written", `{"type":"streamable_http","url":"https://x.example/mcp","bearer_token_env_var":"TOKEN","http_headers":{"X-Role":"builder"},"env_http_headers":null,"http_headers_helper":null}`, ""},
		{"another bearer variable", `{"type":"streamable_http","url":"https://x.example/mcp","bearer_token_env_var":"OTHER","http_headers":{"X-Role":"builder"}}`, "is not the session's own"},
		{"a header added", `{"type":"streamable_http","url":"https://x.example/mcp","bearer_token_env_var":"TOKEN","http_headers":{"X-Role":"builder","X-Forward":"evil"}}`, "is not the session's own"},
		{"a header from the environment", `{"type":"streamable_http","url":"https://x.example/mcp","bearer_token_env_var":"TOKEN","http_headers":{"X-Role":"builder"},"env_http_headers":{"X-Key":"SECRET"}}`, "is not the session's own"},
		{"a headers helper", `{"type":"streamable_http","url":"https://x.example/mcp","bearer_token_env_var":"TOKEN","http_headers":{"X-Role":"builder"},"http_headers_helper":"/tmp/evil"}`, "is not the session's own"},
		{"headers dropped", `{"type":"streamable_http","url":"https://x.example/mcp","bearer_token_env_var":"TOKEN"}`, "is not the session's own"},
		{"another url", `{"type":"streamable_http","url":"https://evil.example/mcp","bearer_token_env_var":"TOKEN","http_headers":{"X-Role":"builder"}}`, "is not the session's own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var servers []codexServer
			if err := json.Unmarshal([]byte(`[{"name":"remote","enabled":true,"transport":`+tc.transport+`}]`), &servers); err != nil {
				t.Fatal(err)
			}
			err := validateCodexServers(servers, own)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
