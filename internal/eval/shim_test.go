package eval

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/fakegh"
	"github.com/kpenfound/busybees/internal/github"
)

func TestShimArgs(t *testing.T) {
	got := shimArgs([]string{"pr", "create", "--body-file", "body.md", "--title", "t", "--input=req.json",
		"-F", "body=@notes/pr.md", "-F", "title=plain", "--field", "x=@/abs/file", "-f", "raw=@kept", "--body-file", "-"}, "/work")
	want := []string{"pr", "create", "--body-file", "/work/body.md", "--title", "t", "--input", "/work/req.json",
		"-F", "body=@/work/notes/pr.md", "-F", "title=plain", "--field", "x=@/abs/file", "-f", "raw=@kept", "--body-file", "-"}
	if !slices.Equal(got, want) {
		t.Fatalf("shimArgs:\n got %q\nwant %q", got, want)
	}
	for args, want := range map[string]bool{
		"issue edit 1 --body-file -":                true,
		"api repos/a/b/pulls/1/reviews --input -":   true,
		"api -X PATCH repos/a/b/pulls/1 -F body=@-": true,
		"pr create --body-file body.md":             false,
		"pr view 1":                                 false,
	} {
		if got := readsStdin(strings.Fields(args)); got != want {
			t.Errorf("readsStdin(%s) = %v", args, got)
		}
	}
}

// Shim against a served fake: output, a body read from a file relative to
// the caller, a body on standard input, and an error as gh reports one.
func TestShimServesTheFake(t *testing.T) {
	f := fakegh.New("acme/widgets")
	if err := f.Load(fakegh.Seed{Issues: []fakegh.SeedIssue{{Issue: github.Issue{Number: 1, Title: "One"}}}}); err != nil {
		t.Fatal(err)
	}
	srv, err := serve(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "body.md"), []byte("from a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := Shim(context.Background(), srv.URL, dir, args, strings.NewReader(stdin), &out, &errOut)
		return code, out.String(), errOut.String()
	}

	if code, out, _ := run("", "issue", "view", "1", "-R", "acme/widgets"); code != 0 || !strings.Contains(out, `"title":"One"`) {
		t.Fatalf("issue view: %d %q", code, out)
	}
	if code, _, stderr := run("", "issue", "comment", "1", "-R", "acme/widgets", "--body-file", "body.md"); code != 0 {
		t.Fatalf("issue comment: %d %s", code, stderr)
	}
	if code, _, stderr := run("from stdin", "issue", "edit", "1", "-R", "acme/widgets", "--body-file", "-"); code != 0 {
		t.Fatalf("issue edit: %d %s", code, stderr)
	}
	s := f.Snapshot()
	if i, _ := s.Issue(1); i.Body != "from stdin" || !slices.Equal(s.Comments[1], []string{"from a file"}) {
		t.Fatalf("issue 1: %+v, comments %v", i, s.Comments[1])
	}
	if code, _, stderr := run("", "issue", "view", "9"); code != 1 || !strings.Contains(stderr, "no issue 9") {
		t.Fatalf("a failing call: %d %q", code, stderr)
	}
	var stderr bytes.Buffer
	if code := Shim(context.Background(), srv.URL+"x", dir, []string{"pr", "list"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "did not answer") {
		t.Fatalf("a wrong address: %d %s", code, stderr.String())
	}
}

func TestWriteShim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := writeShim(dir, "/opt/it's/bees", "http://127.0.0.1:1/x"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "gh"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `exec '/opt/it'\''s/bees' 'eval' 'gh' 'http://127.0.0.1:1/x' "$@"`; !strings.Contains(string(b), want) {
		t.Fatalf("gh script:\n%s\nwant a line %s", b, want)
	}
	if info, err := os.Stat(filepath.Join(dir, "gh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("gh is not executable: %v %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "bees")); err != nil || target != "/opt/it's/bees" {
		t.Fatalf("bees link: %q %v", target, err)
	}
}
