package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/eval"
)

// evalHome keeps the person's own ~/.config/bees/config.toml out of a test.
func evalHome(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestEvalProfileMustExist(t *testing.T) {
	evalHome(t)
	path := writeProject(t, "acme/widgets", "[profiles.fast]\nmodel = \"sonnet\"\n")
	err := runRoot(t, "eval", "--config", path, "--profile", "slow")
	if err == nil || !strings.Contains(err.Error(), `--profile "slow": no such profile in `+path) {
		t.Fatalf("got %v", err)
	}
}

// evalCase writes evals/<name>/ in the current directory, graded by test.
func evalCase(t *testing.T, name, test string) {
	t.Helper()
	for rel, content := range map[string]string{
		"evals/" + name + "/case.toml":      "test = \"" + test + "\"\n[[issues]]\nnumber = 1\ntitle = \"x\"\n",
		"evals/" + name + "/repo/README.md": "x\n",
	} {
		if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rel, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvalCaseMustExist(t *testing.T) {
	evalHome(t)
	t.Chdir(t.TempDir())
	evalCase(t, "one", "false")
	err := runRoot(t, "eval", "--case", "two")
	if err == nil || !strings.Contains(err.Error(), `no case "two" under evals (cases: one)`) {
		t.Fatalf("got %v", err)
	}
}

// bees eval gh hands every argument to the eval's GitHub, flags the root
// command also has included, and prints what it answers.
func TestEvalGHForwardsEveryArgument(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Args []string `json:"args"`
		}
		_ = json.Unmarshal(b, &req)
		got = req.Args
		_, _ = w.Write([]byte(`{"stdout":"answered\n"}`))
	}))
	defer srv.Close()
	root := newRoot()
	var out bytes.Buffer
	root.SetArgs([]string{"eval", "gh", srv.URL, "pr", "list", "-R", "acme/widgets", "-c", "x", "--help"})
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"pr", "list", "-R", "acme/widgets", "-c", "x", "--help"}; !slices.Equal(got, want) {
		t.Fatalf("forwarded %q, want %q", got, want)
	}
	if out.String() != "answered\n" {
		t.Fatalf("printed %q", out.String())
	}
}

// A case whose test already passes on its fixture is invalid and fails: no
// session runs, the table is printed and bees eval exits non-zero.
func TestEvalExitsNonZeroOnAFailedCase(t *testing.T) {
	evalHome(t)
	t.Chdir(t.TempDir())
	evalCase(t, "passes-already", "true")
	root := newRoot()
	var out bytes.Buffer
	root.SetArgs([]string{"eval"})
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil || err.Error() != "1 case of 1 failed" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out.String(), "passes-already") || !strings.Contains(out.String(), "invalid") || !strings.Contains(out.String(), "report: ") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// An eval stopped before its cases ran exits non-zero.
func TestEvalExitsNonZeroWhenStopped(t *testing.T) {
	evalHome(t)
	t.Chdir(t.TempDir())
	evalCase(t, "one", "false")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := newRoot()
	root.SetArgs([]string{"eval"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "the eval was stopped after 0 cases of 1") {
		t.Fatalf("got %v", err)
	}
}

// Stopped after cases that passed is still a failure.
func TestEvalExit(t *testing.T) {
	passed := eval.CaseResult{Case: "a", Pass: true}
	for _, tc := range []struct {
		cases []eval.CaseResult
		total int
		want  string
	}{
		{[]eval.CaseResult{passed}, 1, ""},
		{[]eval.CaseResult{passed}, 2, "the eval was stopped after 1 case of 2"},
		{[]eval.CaseResult{passed, {Case: "b"}}, 2, "1 case of 2 failed"},
	} {
		err := evalExit(&eval.Report{Cases: tc.cases}, tc.total)
		if (err == nil && tc.want != "") || (err != nil && err.Error() != tc.want) {
			t.Errorf("%d of %d: got %v, want %q", len(tc.cases), tc.total, err, tc.want)
		}
	}
}
