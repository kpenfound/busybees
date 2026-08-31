package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// releaseWorkflow is the one workflow file this repository has. Tests read it
// from disk rather than embedding a copy: the file is what has to stay true.
// It is listed in dagger.toml's includeExtraFiles so the check container sees
// it, like CLAUDE.md and bees.example.toml.
const releaseWorkflow = "../../.github/workflows/release.yml"

// releasingDoc documents the assets the workflow publishes.
const releasingDoc = "../../docs/releasing.md"

// installScript downloads a release, and is the third place the asset naming
// scheme is written down. Like the other two it is read from disk and listed
// in dagger.toml's includeExtraFiles.
const installScript = "../../install.sh"

// ldflagsX matches the linker's `-X <import path>.<name>=<value>`.
var ldflagsX = regexp.MustCompile(`-X\s+([\w./-]+)\.(\w+)=([^"\s]+)`)

// tarball matches the tar invocation that names a release asset.
var tarball = regexp.MustCompile(`tar -czf "dist/([^"]+)"`)

// matrixEntry matches one platform of the build matrix.
var matrixEntry = regexp.MustCompile(`(?m)^\s*- goos:\s*(\w+)\s*\n\s*goarch:\s*(\w+)`)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestReleaseWorkflowStampsAVariableThatExists pins the only link between the
// release workflow and this package: the workflow stamps the version with
// `-ldflags -X main.version=<tag>`, which the compiler never checks. Renaming
// that variable — or dropping the tag from the value — leaves every build
// green and every released binary reporting "dev", which is only visible once
// a person has already pushed the tag.
func TestReleaseWorkflowStampsAVariableThatExists(t *testing.T) {
	m := ldflagsX.FindStringSubmatch(readFile(t, releaseWorkflow))
	if m == nil {
		t.Fatalf("%s: no `-X <pkg>.<name>=<value>` in it. That flag is what makes a released binary report its tag; restore it rather than leaving releases unstamped.", releaseWorkflow)
	}
	pkg, name, value := m[1], m[2], m[3]

	if pkg != "main" {
		t.Errorf("%s stamps %s.%s, but the version variable lives in package main of cmd/bees", releaseWorkflow, pkg, name)
	}
	// The tag is what `bees version` must report, and GITHUB_REF_NAME is the
	// tag GitHub hands a tag-triggered run.
	if !strings.Contains(value, "GITHUB_REF_NAME") {
		t.Errorf("%s stamps %s.%s=%s, which is not the tag that triggered the run", releaseWorkflow, pkg, name, value)
	}

	vars := packageMainStringVars(t)
	if !vars[name] {
		t.Errorf("%s stamps main.%s, which package main does not declare. Its string variables are %v; rename the flag and the variable together.", releaseWorkflow, name, sortedKeys(vars))
	}
}

// TestReleaseAssetNamesAreDocumented pins the asset naming scheme, which is a
// public interface rather than an implementation detail: install.sh at the
// repository root parses it to pick the right download. Both halves matter — a platform added
// to the matrix and left undocumented, and a documented platform the workflow
// does not build.
func TestReleaseAssetNamesAreDocumented(t *testing.T) {
	workflow := readFile(t, releaseWorkflow)
	doc := readFile(t, releasingDoc)

	m := tarball.FindStringSubmatch(workflow)
	if m == nil {
		t.Fatalf("%s: no `tar -czf \"dist/...\"` in it; that line is what names the assets", releaseWorkflow)
	}
	pattern := m[1]

	platforms := matrixEntry.FindAllStringSubmatch(workflow, -1)
	if len(platforms) == 0 {
		t.Fatalf("%s: no `- goos: ... / goarch: ...` entries; the build matrix is what decides which assets exist", releaseWorkflow)
	}

	built := map[string]bool{}
	for _, p := range platforms {
		// The docs write the tag as <version>; the workflow writes it as the
		// environment variable the runner sets.
		name := strings.NewReplacer(
			"${GITHUB_REF_NAME}", "<version>",
			"${GOOS}", p[1],
			"${GOARCH}", p[2],
		).Replace(pattern)
		built[name] = true
		if !strings.Contains(doc, name) {
			t.Errorf("%s builds %s but %s does not name it. The scheme is what the install script parses; document the asset.", releaseWorkflow, name, releasingDoc)
		}
	}

	documented := regexp.MustCompile(`bees_<version>_\w+_\w+\.tar\.gz`)
	for _, name := range documented.FindAllString(doc, -1) {
		if !built[name] {
			t.Errorf("%s promises %s but the workflow's matrix does not build it", releasingDoc, name)
		}
	}

	for _, f := range []struct{ path, text string }{{releaseWorkflow, workflow}, {releasingDoc, doc}} {
		if !strings.Contains(f.text, "checksums.txt") {
			t.Errorf("%s does not mention checksums.txt, which every release carries", f.path)
		}
	}
}

// packageMainStringVars collects the package-level string variables of
// cmd/bees, which is where an `-X` stamp can land.
func packageMainStringVars(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/bees: %v", err)
	}
	fset := token.NewFileSet()
	vars := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if i < len(vs.Values) && isStringLiteral(vs.Values[i]) {
						vars[n.Name] = true
					}
				}
			}
		}
	}
	return vars
}

func isStringLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assetFormat matches the printf format install.sh builds an asset name with,
// and assetCall the one place that fills it in. Both are needed: the format
// says what the name looks like, the call says which part is which.
var assetFormat = regexp.MustCompile(`printf '([^']*\.tar\.gz)`)

var assetCall = regexp.MustCompile(`asset_name "\$(\w+)" "\$(\w+)" "\$(\w+)"`)

// assetPart names the three variables of an asset name on each side, so the
// two can be compared with their order intact.
var assetPart = map[string]string{
	// the workflow's environment variables
	"${GITHUB_REF_NAME}": "<version>",
	"${GOOS}":            "<os>",
	"${GOARCH}":          "<arch>",
	// install.sh's shell variables
	"version": "<version>",
	"os":      "<os>",
	"arch":    "<arch>",
}

// TestInstallScriptBuildsTheAssetNamesTheWorkflowPublishes pins the third copy
// of the asset naming scheme. The workflow writes the names, docs/releasing.md
// documents them (the test above), and install.sh parses them — and it is the
// copy that cannot be fixed after the fact, because the script people run is
// whatever main served them last. Nothing else compares the two: the script is
// shell, it is deliberately untested end to end (it would have to reach
// GitHub), and a rename would leave every check green and every install
// broken.
func TestInstallScriptBuildsTheAssetNamesTheWorkflowPublishes(t *testing.T) {
	workflow := readFile(t, releaseWorkflow)
	script := readFile(t, installScript)

	m := tarball.FindStringSubmatch(workflow)
	if m == nil {
		t.Fatalf("%s: no `tar -czf \"dist/...\"` in it; that line is what names the assets", releaseWorkflow)
	}
	// Both sides are rewritten to the same vocabulary before they are
	// compared, so the check covers the order of the three parts and not only
	// their shape: bees_<os>_<arch>_<version> and bees_<version>_<os>_<arch>
	// are both "bees_%s_%s_%s" and only one of them installs anything.
	want := regexp.MustCompile(`\$\{\w+\}`).ReplaceAllStringFunc(m[1], func(v string) string {
		part, ok := assetPart[v]
		if !ok {
			t.Errorf("%s names an asset with %s, which this test cannot map to a part of the name; teach assetPart about it", releaseWorkflow, v)
			return v
		}
		return part
	})

	f := assetFormat.FindStringSubmatch(script)
	if f == nil {
		t.Fatalf("%s: no `printf '...tar.gz'` in it; that format is how it names the asset to download", installScript)
	}
	c := assetCall.FindStringSubmatch(script)
	if c == nil {
		t.Fatalf("%s: no `asset_name \"$version\" \"$os\" \"$arch\"` call in it; that call is what fills the format in", installScript)
	}
	got := f[1]
	for _, v := range c[1:] {
		part, ok := assetPart[v]
		if !ok {
			t.Fatalf("%s passes $%s to asset_name, which this test cannot map to a part of the name; teach assetPart about it", installScript, v)
		}
		got = strings.Replace(got, "%s", part, 1)
	}
	if got != want {
		t.Errorf("%s publishes %q, %s downloads %q. They have to agree, and the workflow is the source of truth.", releaseWorkflow, want, installScript, got)
	}

	if !strings.Contains(script, "checksums.txt") {
		t.Errorf("%s does not fetch checksums.txt, so it cannot verify what it downloads", installScript)
	}
	for _, p := range matrixEntry.FindAllStringSubmatch(workflow, -1) {
		for _, want := range []string{p[1], p[2]} {
			if !strings.Contains(script, want) {
				t.Errorf("%s builds %s/%s but %s never names %q, so that platform cannot be installed", releaseWorkflow, p[1], p[2], installScript, want)
			}
		}
	}
}
