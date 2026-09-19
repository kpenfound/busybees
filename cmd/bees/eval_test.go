package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

func TestEvalCaseMustExist(t *testing.T) {
	evalHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	for rel, content := range map[string]string{
		"evals/one/case.toml":      "test = \"false\"\n[[issues]]\nnumber = 1\ntitle = \"x\"\n",
		"evals/one/repo/README.md": "x\n",
	} {
		if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rel, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
