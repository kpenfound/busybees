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
// public interface rather than an implementation detail: the install script
// parses it to pick the right download. Both halves matter — a platform added
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
