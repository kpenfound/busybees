package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// opencode imports every file it finds in the tool/ and tools/ directories
// of its configuration roots as a custom tool, --pure or not, and before any
// permission applies: importing runs the file's top-level code, and a custom
// tool named like a built-in one ("read.ts") takes the built-in's
// permission. `debug config` reports none of them, and opencode has no
// switch that stops the scan. The roots are its global configuration
// directory ($XDG_CONFIG_HOME/opencode, else ~/.config/opencode), every
// .opencode directory from the working directory up to the worktree,
// ~/.opencode and $OPENCODE_CONFIG_DIR.
//
// A held turn therefore looks for such files first, where the turn will
// run (prober) and with its environment, and refuses to start when there
// is one or when the look fails. The look is openCodeToolScan run by the
// opencode executable itself as the JavaScript runtime it is built on
// (BUN_BE_BUN), since a probe runs the agent's executable and nothing else.
// The scan searches more than opencode does: every .opencode directory up
// to the root of the filesystem, both global directories, and every file in
// tool/ and tools/ rather than *.js and *.ts only.

// openCodeToolScanMarker names the report openCodeToolScan prints, so that
// output from anything else (an opencode that is no longer a Bun
// executable and ignores BUN_BE_BUN) is not taken for one.
const openCodeToolScanMarker = "bees-opencode-custom-tools/1"

// openCodeToolScan lists every entry that is not a directory in the tool/
// and tools/ directories of each root, following links the way opencode's
// scan does; a dangling link is listed too. A directory missing, or a root
// that is a file, is nothing to load; any other error reading one is
// reported, and refuses the turn.
const openCodeToolScan = `const fs = require("fs"), path = require("path"), os = require("os");
const env = process.env, home = os.homedir(), cwd = process.cwd();
const roots = [];
const add = (d) => { if (d && !roots.includes(d)) roots.push(d); };
add(path.join(env.XDG_CONFIG_HOME || path.join(home, ".config"), "opencode"));
add(path.join(home, ".config", "opencode"));
add(path.join(home, ".opencode"));
if (env.OPENCODE_TEST_HOME) add(path.join(env.OPENCODE_TEST_HOME, ".opencode"));
add(env.OPENCODE_CONFIG_DIR);
for (let d = cwd; ; d = path.dirname(d)) { add(path.join(d, ".opencode")); if (path.dirname(d) === d) break; }
const tools = [], errors = [];
for (const root of roots) for (const sub of ["tool", "tools"]) {
  const dir = path.join(root, sub);
  let names;
  try { names = fs.readdirSync(dir); } catch (e) {
    if (e.code !== "ENOENT" && e.code !== "ENOTDIR") errors.push(dir + ": " + (e.code || e.message));
    continue;
  }
  for (const name of names) {
    const file = path.join(dir, name);
    try { if (fs.statSync(file).isDirectory()) continue; } catch (e) {}
    tools.push(file);
  }
}
console.log(JSON.stringify({ probe: "` + openCodeToolScanMarker + `", roots, tools, errors }));`

// openCodeToolScanArgs run openCodeToolScan with Bun's own configuration
// and .env files, which opencode does not read and a repository could use
// to preload code into the scan, left out.
var openCodeToolScanArgs = []string{"--config=/dev/null", "--no-env-file", "-e", openCodeToolScan}

// openCodeToolReport is what openCodeToolScan prints.
type openCodeToolReport struct {
	Probe  string   `json:"probe"`
	Roots  []string `json:"roots"`
	Tools  []string `json:"tools"`
	Errors []string `json:"errors"`
}

// openCodeCustomTools refuses a held turn opencode would give a custom
// tool, or whose roots could not be searched. extra are the turn's
// variables; the scan runs with them, as the executable's runtime, and
// with any options for that runtime cleared.
func openCodeCustomTools(ctx context.Context, probe prober, bin string, extra []envVar) error {
	vars := append(extra[:len(extra):len(extra)], envVar{"BUN_BE_BUN", "1"}, envVar{"BUN_OPTIONS", ""})
	out, err := probe(ctx, bin, openCodeToolScanArgs, vars, nil)
	if err != nil {
		return fmt.Errorf("search for custom tools: %w; a held opencode turn needs an opencode built on Bun that runs a script with BUN_BE_BUN=1", err)
	}
	var report openCodeToolReport
	if err := json.Unmarshal(out, &report); err != nil || report.Probe != openCodeToolScanMarker || len(report.Roots) == 0 {
		return fmt.Errorf("search for custom tools: opencode did not report the search (%q); a held opencode turn needs an opencode built on Bun that runs a script with BUN_BE_BUN=1", strings.TrimSpace(string(out)))
	}
	if len(report.Errors) > 0 {
		return fmt.Errorf("search for custom tools: could not read %s; make them readable where the turn runs, or remove them", strings.Join(report.Errors, ", "))
	}
	if len(report.Tools) > 0 {
		return fmt.Errorf("opencode would load custom tools before its permissions apply: %s; remove them from where the turn runs, and run the turn again", strings.Join(report.Tools, ", "))
	}
	return nil
}
