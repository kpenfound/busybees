package review

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Ref
	}{
		{"https://github.com/acme/widgets/pull/12", Ref{"acme/widgets", 12}},
		{"http://github.com/acme/widgets/pull/12", Ref{"acme/widgets", 12}},
		{"github.com/acme/widgets/pull/12", Ref{"acme/widgets", 12}},
		{"https://www.github.com/acme/widgets/pull/12/files", Ref{"acme/widgets", 12}},
		{"https://github.com/acme/widgets/pull/12#issuecomment-9", Ref{"acme/widgets", 12}},
		{"acme/widgets#12", Ref{"acme/widgets", 12}},
		{"  acme/widgets#12  ", Ref{"acme/widgets", 12}},
		{"12", Ref{"", 12}},
		{"#12", Ref{"", 12}},
	} {
		got, err := ParseRef(tc.in)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseRefRejects(t *testing.T) {
	for _, in := range []string{"", "   ", "acme/widgets", "https://github.com/acme/widgets/issues/12", "https://gitlab.com/acme/widgets/pull/12", "acme/widgets#0", "0", "-3", "twelve", "acme/widgets#12x"} {
		if got, err := ParseRef(in); err == nil {
			t.Fatalf("ParseRef(%q) = %+v, want an error", in, got)
		}
	}
}

func TestRefStringAndURL(t *testing.T) {
	ref := Ref{"acme/widgets", 12}
	if got := ref.String(); got != "acme/widgets#12" {
		t.Fatalf("String() = %q", got)
	}
	if got := ref.URL(); got != "https://github.com/acme/widgets/pull/12" {
		t.Fatalf("URL() = %q", got)
	}
}

func TestResolveRefTakesTheRepositoryFromTheRemote(t *testing.T) {
	dir := gitRepo(t, "git@github.com:acme/widgets.git")
	ref, err := ResolveRef(context.Background(), "12", dir)
	if err != nil {
		t.Fatal(err)
	}
	if ref != (Ref{"acme/widgets", 12}) {
		t.Fatalf("ref %+v, want acme/widgets#12", ref)
	}
	// A reference that names a repository keeps it, whatever the checkout is.
	ref, err = ResolveRef(context.Background(), "other/thing#3", dir)
	if err != nil || ref != (Ref{"other/thing", 3}) {
		t.Fatalf("ref %+v: %v", ref, err)
	}
}

func TestResolveRefWithoutARemoteSaysWhatToGiveInstead(t *testing.T) {
	err := resolveErr(t, gitRepo(t, ""))
	for _, want := range []string{`"12"`, "origin", "owner/name#12"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	// A directory that is not a checkout at all is the same answer.
	if err := resolveErr(t, t.TempDir()); err == nil {
		t.Fatal("a bare number outside a checkout resolved")
	}
}

func resolveErr(t *testing.T, dir string) error {
	t.Helper()
	ref, err := ResolveRef(context.Background(), "12", dir)
	if err == nil {
		t.Fatalf("ResolveRef in %s = %+v, want an error", dir, ref)
	}
	return err
}

func TestCheckoutOfRefusesAnotherRepository(t *testing.T) {
	dir := gitRepo(t, "https://github.com/acme/widgets")
	if got := CheckoutOf(context.Background(), "acme/widgets", dir); got != dir {
		t.Fatalf("CheckoutOf its own repository = %q, want %q", got, dir)
	}
	if got := CheckoutOf(context.Background(), "ACME/Widgets", dir); got != dir {
		t.Fatalf("CheckoutOf is case-insensitive: %q, want %q", got, dir)
	}
	if got := CheckoutOf(context.Background(), "other/thing", dir); got != "" {
		t.Fatalf("CheckoutOf another repository = %q, want no checkout", got)
	}
	if got := CheckoutOf(context.Background(), "acme/widgets", t.TempDir()); got != "" {
		t.Fatalf("CheckoutOf a directory that is not a checkout = %q", got)
	}
}

// gitRepo is a git checkout with remote as its origin, or with none at all
// when remote is empty.
func gitRepo(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if remote != "" {
		runGit(t, dir, "remote", "add", Remote, remote)
	}
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
