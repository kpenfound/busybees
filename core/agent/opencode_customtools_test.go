package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// openCodeScanProbe is the part of a fake opencode that answers the search
// for custom tools, the probe run with BUN_BE_BUN=1: it records its
// arguments in record.scan-args, its environment in record.scan-env and,
// in a container, the engine's arguments in record.scan-engine, then runs
// scan, a shell snippet standing in for openCodeToolScan.
func openCodeScanProbe(record, scan string) string {
	return `if [ "$BUN_BE_BUN" = 1 ]; then
  printf '%s\n' "$@" > "` + record + `.scan-args"
  env > "` + record + `.scan-env"
  if [ -n "$RUN_DIR" ] && [ -f "$RUN_DIR/docker-args.txt" ]; then cp "$RUN_DIR/docker-args.txt" "` + record + `.scan-engine"; fi
  ` + scan + `
  exit 0
fi
`
}

// openCodeScanFake does what openCodeToolScan does, in sh: the same roots
// from the same variables and working directory, and every entry that is
// not a directory in their tool/ and tools/ directories, dot files and
// dangling links included.
const openCodeScanFake = `home="${HOME:-/}"
roots="${XDG_CONFIG_HOME:-$home/.config}/opencode
$home/.config/opencode
$home/.opencode"
if [ -n "$OPENCODE_CONFIG_DIR" ]; then roots="$roots
$OPENCODE_CONFIG_DIR"; fi
d="$(pwd -P)"
while :; do roots="$roots
${d%/}/.opencode"; [ "$d" = / ] && break; d="$(dirname "$d")"; done
list() { out=""; while IFS= read -r l; do [ -n "$l" ] && out="$out${out:+,}\"$l\""; done; printf '%s' "$out"; }
found="$(printf '%s\n' "$roots" | while IFS= read -r r; do for s in tool tools; do for f in "$r/$s"/* "$r/$s"/.[!.]*; do
  [ -d "$f" ] && continue
  if [ -e "$f" ] || [ -L "$f" ]; then printf '%s\n' "$f"; fi
done; done; done)"
printf '{"probe":"` + openCodeToolScanMarker + `","roots":[%s],"tools":[%s],"errors":[]}\n' "$(printf '%s\n' "$roots" | list)" "$(printf '%s\n' "$found" | list)"`

// heldOpenCodeTurn is one kind of held opencode turn: how it is run on the
// host with a fake whose scan is given, with the variables env and in the
// working directory work, and what its setup errors say they come from.
type heldOpenCodeTurn struct {
	name   string
	prefix string
	run    func(t *testing.T, scan, work string, env map[string]string) (record string, err error)
}

var heldOpenCodeTurns = []heldOpenCodeTurn{
	{"restricted", "restricted opencode setup", func(t *testing.T, scan, work string, env map[string]string) (string, error) {
		bin, record := restrictedOpenCodeFakeScanning(t, "", restrictedOpenCodeAnswer, scan)
		r := restrictedRunner(t, "", "")
		r.OpenCodeBin = bin
		req := restrictedRequestFor(AgentOpenCode, work)
		req.Env = env
		_, err := r.RunRestricted(context.Background(), req)
		return record, err
	}},
	{"granted", "granted opencode setup", func(t *testing.T, scan, work string, env map[string]string) (string, error) {
		bin, record := grantedOpenCodeFakeScanning(t, grantedEffective(""), scan)
		r := &Runner{OpenCodeBin: bin, SessionsDir: t.TempDir()}
		req := grantAll(Request{Name: "g", Profile: grantedProfile(), Workspace: fakeWorkspace{dir: work}, SessionDir: realTempDir(t), Prompt: "TASK", Env: env})
		req.Grants.Tools = []string{"read", "edit", "mcp__tools"}
		_, err := r.Run(context.Background(), req)
		return record, err
	}},
}

// launched reports whether the fake opencode started the model: the
// restricted fake records its arguments, the granted one a marker.
func launched(record string) bool {
	for _, marker := range []string{".args", ".launched"} {
		if _, err := os.Stat(record + marker); err == nil {
			return true
		}
	}
	return false
}

// writeTool puts a custom tool opencode would import at dir/name.
func writeTool(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A held turn, restricted or granted, with no custom tool anywhere starts,
// after one search run as the opencode executable's runtime with the
// turn's own variables; the turn itself is not started as that runtime.
func TestHeldOpenCodeTurnSearchesForCustomToolsFirst(t *testing.T) {
	for _, kind := range heldOpenCodeTurns {
		t.Run(kind.name, func(t *testing.T) {
			work := realTempDir(t)
			// Directories that are not tools and files outside tool/ and
			// tools/ are nothing opencode loads.
			if err := os.MkdirAll(filepath.Join(work, ".opencode", "tools", "lib"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTool(t, filepath.Join(work, ".opencode", "plugin"), "p.ts")
			record, err := kind.run(t, openCodeScanFake, work, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !launched(record) {
				t.Fatal("the turn did not start")
			}
			// Bun reads neither its configuration file nor .env files from the
			// working directory for the scan: a repository could preload code
			// into it with either.
			got := lines(t, record+".scan-args")
			if len(got) < 4 || !slices.Equal(got[:3], []string{"--config=/dev/null", "--no-env-file", "-e"}) || !strings.HasPrefix(got[3], "const fs = require(") {
				t.Errorf("scan arguments = %q", got)
			}
			scanEnv, err := os.ReadFile(record + ".scan-env")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"\nBUN_BE_BUN=1\n", "\nBUN_OPTIONS=\n", "\n" + EnvOpenCodePermission + "=", "\n" + EnvOpenCodeConfig + "="} {
				if !strings.Contains("\n"+string(scanEnv), want) {
					t.Errorf("the scan's environment lacks %q", want)
				}
			}
			env, _ := os.ReadFile(record + ".env")
			if strings.Contains("\n"+string(env), "\nBUN_BE_BUN=") {
				t.Error("the turn was started with BUN_BE_BUN")
			}
		})
	}
}

// A custom tool in any root opencode searches refuses a held turn before
// any inventory and before the model, naming the file: the tool is named
// like the built-in read tool both turns hold.
func TestHeldOpenCodeTurnRefusesACustomToolInEveryRoot(t *testing.T) {
	for _, kind := range heldOpenCodeTurns {
		for _, root := range []struct {
			name string
			dir  func(home, xdg, configDir, work string) string
		}{
			{"XDG_CONFIG_HOME", func(_, xdg, _, _ string) string { return filepath.Join(xdg, "opencode", "tools") }},
			{"~/.config/opencode", func(home, _, _, _ string) string { return filepath.Join(home, ".config", "opencode", "tool") }},
			{"~/.opencode", func(home, _, _, _ string) string { return filepath.Join(home, ".opencode", "tools") }},
			{"OPENCODE_CONFIG_DIR", func(_, _, configDir, _ string) string { return filepath.Join(configDir, "tool") }},
			{"project", func(_, _, _, work string) string { return filepath.Join(work, ".opencode", "tools") }},
			{"parent of the working directory", func(_, _, _, work string) string { return filepath.Join(filepath.Dir(work), ".opencode", "tool") }},
		} {
			t.Run(kind.name+"/"+root.name, func(t *testing.T) {
				home, xdg, configDir := realTempDir(t), realTempDir(t), realTempDir(t)
				work := filepath.Join(realTempDir(t), "work")
				if err := os.Mkdir(work, 0o755); err != nil {
					t.Fatal(err)
				}
				tool := writeTool(t, root.dir(home, xdg, configDir, work), "read.ts")
				env := map[string]string{"HOME": home, "XDG_CONFIG_HOME": xdg, "OPENCODE_CONFIG_DIR": configDir}
				record, err := kind.run(t, openCodeScanFake, work, env)
				if err == nil || !strings.Contains(err.Error(), kind.prefix+": ") || !strings.Contains(err.Error(), "would load custom tools") || !strings.Contains(err.Error(), tool) {
					t.Fatalf("error = %v, want the custom tool %s refused", err, tool)
				}
				if launched(record) {
					t.Fatal("the model started with a custom tool present")
				}
				if _, err := os.Stat(record + ".config-args"); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the configuration was inspected before the custom tool was refused: %v", err)
				}
			})
		}
	}
}

// Dot files and links, dangling ones included, are custom tools too.
func TestHeldOpenCodeTurnRefusesHiddenAndLinkedCustomTools(t *testing.T) {
	for _, name := range []string{".hidden.ts", "dangling.js"} {
		t.Run(name, func(t *testing.T) {
			work := realTempDir(t)
			dir := filepath.Join(work, ".opencode", "tools")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tool := filepath.Join(dir, name)
			if name == "dangling.js" {
				if err := os.Symlink("nowhere.js", tool); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTool(t, dir, name)
			}
			record, err := heldOpenCodeTurns[0].run(t, openCodeScanFake, work, nil)
			if err == nil || !strings.Contains(err.Error(), tool) {
				t.Fatalf("error = %v, want %s refused", err, tool)
			}
			if launched(record) {
				t.Fatal("the model started")
			}
		})
	}
}

// A search that fails, says something else, or could not read a root
// refuses the turn before the model starts.
func TestHeldOpenCodeTurnFailsClosedWhenTheSearchDoesNot(t *testing.T) {
	for _, kind := range heldOpenCodeTurns {
		for _, tc := range []struct {
			name, scan, want string
		}{
			{"failed", `echo 'error: unknown option -e' >&2; exit 1`, "unknown option -e"},
			{"malformed", `echo 'opencode 1.18.31'`, "did not report the search"},
			{"another report", `echo '{"roots":["/.opencode"],"tools":[],"errors":[]}'`, "did not report the search"},
			{"no roots", `echo '{"probe":"` + openCodeToolScanMarker + `","roots":[],"tools":[],"errors":[]}'`, "did not report the search"},
			{"unreadable root", `echo '{"probe":"` + openCodeToolScanMarker + `","roots":["/r"],"tools":[],"errors":["/r/tools: EACCES"]}'`, "could not read /r/tools: EACCES"},
		} {
			t.Run(kind.name+"/"+tc.name, func(t *testing.T) {
				record, err := kind.run(t, tc.scan, realTempDir(t), nil)
				if err == nil || !strings.Contains(err.Error(), kind.prefix+": ") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
				if launched(record) {
					t.Fatal("the model started after the search failed")
				}
			})
		}
	}
}

// In every placement a granted turn is supported in, the search runs where
// the turn would, and a custom tool in the working directory there
// refuses the turn before the model starts.
func TestOpenCodeWritableTurnRefusesCustomToolsInEveryPlacement(t *testing.T) {
	for _, pl := range grantedPlacements() {
		t.Run(pl.where.String(), func(t *testing.T) {
			bin, record := grantedOpenCodeFake(t, grantedEffective(""))
			r, req, _ := pl.setup(t, bin)
			tool := writeTool(t, filepath.Join(req.workDir(), ".opencode", "tools"), "read.ts")
			_, err := r.Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "would load custom tools") || !strings.Contains(err.Error(), tool) {
				t.Fatalf("error = %v, want %s refused", err, tool)
			}
			if launched(record) {
				t.Fatal("the model started with a custom tool present")
			}
			if _, err := os.Stat(record + ".scan-args"); err != nil {
				t.Fatalf("no search ran: %v", err)
			}
			switch {
			case r.Confiner != nil:
				if n := len(r.Confiner.(*fakeConfiner).started); n != 1 {
					t.Errorf("the confiner started %d processes, want the search alone", n)
				}
			case r.DockerBin != "":
				engine := lines(t, record+".scan-engine")
				if engine[0] != "run" || !strings.HasSuffix(flagValue(engine, "--name"), "-probe") || !slices.Contains(engine, "image") || slices.Contains(engine, "--interactive") {
					t.Errorf("search engine command: %v", engine)
				}
			case r.SbxBin != "":
				probes := lines(t, filepath.Join(filepath.Dir(r.SbxBin), "sbx-probe.txt"))
				if countLines(probes, "exec") != 1 || !slices.Contains(probes, "BUN_BE_BUN") {
					t.Errorf("search in the sandbox: %v", probes)
				}
			}
		})
	}
}

// The clean report the fakes of other packages print is one the search
// accepts.
func TestTheSharedCleanReportIsAccepted(t *testing.T) {
	probe := func(context.Context, string, []string, []envVar, talker) ([]byte, error) {
		return []byte(agenttest.OpenCodeNoCustomTools + "\n"), nil
	}
	if err := openCodeCustomTools(context.Background(), probe, "opencode", nil); err != nil {
		t.Fatal(err)
	}
}
