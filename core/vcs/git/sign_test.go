package git

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/core/vcs/internal/vcstest"
)

// fakeGPG signs anything: it records its arguments in calls beside it and
// answers the way gpg does, with no key involved.
const fakeGPG = `#!/bin/sh
echo "$@" >> "$(dirname "$0")/calls"
cat > /dev/null
printf '\n[GNUPG:] SIG_CREATED D 22 8 00 1700000000 FAKE\n' >&2
printf -- '-----BEGIN PGP SIGNATURE-----\n\nZmFrZQ==\n-----END PGP SIGNATURE-----\n'
`

// failingGPG refuses to sign.
const failingGPG = `#!/bin/sh
echo "$@" >> "$(dirname "$0")/calls"
cat > /dev/null
echo "gpg: signing failed: No secret key" >&2
exit 2
`

// fakeSSH signs the way ssh-keygen -Y sign does: the signature beside the
// file named last.
const fakeSSH = `#!/bin/sh
echo "$@" >> "$(dirname "$0")/calls"
for last; do :; done
printf -- '-----BEGIN SSH SIGNATURE-----\nZmFrZQ==\n-----END SSH SIGNATURE-----\n' > "$last.sig"
`

// isolate keeps the machine's git configuration, and with it any personal
// identity or key, out of the test, and sets an identity in the
// environment that the signer must ignore.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, who := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+who+"_NAME", "Session")
		t.Setenv("GIT_"+who+"_EMAIL", "session@example.com")
	}
}

// program writes a signing program and returns its path; its calls are
// recorded in the file calls beside it.
func program(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sign")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func calls(t *testing.T, prog string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(prog), "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// signingStack is vcstest's repository with the owner's identity and the
// fake gpg as its signing program, and returns the program.
func signingStack(t *testing.T) (*vcstest.Stack, string) {
	t.Helper()
	isolate(t)
	s := vcstest.NewStack(t, false)
	prog := program(t, fakeGPG)
	s.Git(t, "config", "user.name", "Owner")
	s.Git(t, "config", "user.email", "owner@example.com")
	s.Git(t, "config", "user.signingKey", "OWNERKEY")
	s.Git(t, "config", "gpg.program", prog)
	return s, prog
}

// rewrite is the child's two commits as a linear history on main, with
// the owner's messages.
func rewrite(s *vcstest.Stack, id string) vcs.SignRequest {
	return vcs.SignRequest{ID: id, Commits: []vcs.CommitSpec{
		{Tree: s.Child[0] + "^{tree}", Parents: []string{"main"}, Message: "child: use the feature\n\nSigned-off-by: Owner <owner@example.com>\n"},
		{Tree: s.Child[1], FollowsPrevious: true, Message: "child: finish\n\nSigned-off-by: Owner <owner@example.com>"},
	}}
}

func refs(t *testing.T, s *vcstest.Stack) map[string]string {
	t.Helper()
	out := map[string]string{}
	for line := range strings.SplitSeq(s.Git(t, "for-each-ref", "--format=%(refname) %(objectname)"), "\n") {
		name, id, _ := strings.Cut(line, " ")
		out[name] = id
	}
	return out
}

// The child's commits are rewritten onto main as a signed linear history:
// the trees are the ones reviewed, each commit's parent is the one before,
// the messages are as given, the owner is author and committer whatever
// the environment says, and the repository's key signs. No branch, HEAD or
// file of the working tree moves, and the same request again returns the
// same commits without signing anything.
func TestSignALinearRewrittenHistory(t *testing.T) {
	s, prog := signingStack(t)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	before := refs(t, s)
	req := rewrite(s, "op-1")

	sg, err := Signer{}.Sign(ctx, ws, req)
	if err != nil {
		t.Fatal(err)
	}
	if !sg.Finished || len(sg.Commits) != 2 || sg.Head != sg.Commits[1] {
		t.Fatalf("signing: %+v", sg)
	}
	wantParents := [][]string{{s.Onto}, {sg.Commits[0]}}
	for i, c := range sg.Commits {
		raw := s.Git(t, "cat-file", "commit", c)
		obj := parseCommit(raw)
		if want := s.Git(t, "rev-parse", s.Child[i]+"^{tree}"); obj.tree != want {
			t.Errorf("commit %d tree %s, want the child's %s", i, obj.tree, want)
		}
		if !slices.Equal(obj.parents, wantParents[i]) {
			t.Errorf("commit %d parents %v, want %v", i, obj.parents, wantParents[i])
		}
		if !strings.Contains(raw, "\ngpgsig -----BEGIN PGP SIGNATURE-----") {
			t.Errorf("commit %d carries no signature:\n%s", i, raw)
		}
		if got := show(t, s, c, "%an <%ae>|%cn <%ce>"); got != "Owner <owner@example.com>|Owner <owner@example.com>" {
			t.Errorf("commit %d author and committer %q, want the owner", i, got)
		}
		// The message is written as given, a missing final newline
		// included.
		untrimmed, err := Signer{}.output(ctx, s.Dir, nil, "cat-file", "commit", c)
		if err != nil || parseCommit(untrimmed).message != req.Commits[i].Message {
			t.Errorf("commit %d message %q, want exactly %q (%v)", i, parseCommit(untrimmed).message, req.Commits[i].Message, err)
		}
	}
	if c := calls(t, prog); len(c) != 2 || !strings.Contains(c[0], "-bsau OWNERKEY") {
		t.Errorf("signing program calls %q, want two with the repository's key", c)
	}

	after := refs(t, s)
	for name, id := range before {
		if after[name] != id {
			t.Errorf("%s moved from %s to %s", name, id, after[name])
		}
	}
	for i, c := range sg.Commits {
		if got := after[commitRef("op-1", i)]; got != c {
			t.Errorf("ref of commit %d: %s, want %s", i, got, c)
		}
	}
	if got := s.Git(t, "rev-parse", "--abbrev-ref", "HEAD"); got != "child" {
		t.Errorf("HEAD %s, want child", got)
	}
	if got := s.Git(t, "status", "--porcelain"); got != "" {
		t.Errorf("working tree changed:\n%s", got)
	}

	again, err := Signer{}.Sign(ctx, ws, req)
	if err != nil || !slices.Equal(again.Commits, sg.Commits) || !again.Finished {
		t.Errorf("the same signing again: %+v, %v, want %v", again, err, sg.Commits)
	}
	if st, err := (Signer{}).SignStatus(ctx, ws, "op-1"); err != nil || !slices.Equal(st.Commits, sg.Commits) || !st.Finished {
		t.Errorf("status: %+v, %v", st, err)
	}
	if c := calls(t, prog); len(c) != 2 {
		t.Errorf("signing program called %d times, want the first signing's 2", len(c))
	}

	if err := (Signer{}).Forget(ctx, ws, "op-1"); err != nil {
		t.Fatal(err)
	}
	for name := range refs(t, s) {
		if strings.HasPrefix(name, RefPrefix) {
			t.Errorf("%s left after Forget", name)
		}
	}
	if _, err := (Signer{}).SignStatus(ctx, ws, "op-1"); !errors.Is(err, vcs.ErrNoSigning) {
		t.Errorf("status after Forget: %v, want ErrNoSigning", err)
	}
	if err := (Signer{}).Forget(ctx, ws, "op-1"); !errors.Is(err, vcs.ErrNoSigning) {
		t.Errorf("Forget again: %v, want ErrNoSigning", err)
	}
}

// Parents are taken as given: a root commit has none, and a commit that
// follows the previous one has it first, then its own.
func TestSignExplicitParents(t *testing.T) {
	s, _ := signingStack(t)
	ctx := context.Background()
	sg, err := Signer{}.Sign(ctx, vcs.Directory(s.Dir), vcs.SignRequest{ID: "parents", Commits: []vcs.CommitSpec{
		{Tree: s.OldBase + "^{tree}", Message: "root\n"},
		{Tree: s.Head + "^{tree}", FollowsPrevious: true, Parents: []string{s.Onto}, Message: "merge\n"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := parseCommit(s.Git(t, "cat-file", "commit", sg.Commits[0])).parents; len(got) != 0 {
		t.Errorf("root commit parents %v", got)
	}
	if got := parseCommit(s.Git(t, "cat-file", "commit", sg.Commits[1])).parents; !slices.Equal(got, []string{sg.Commits[0], s.Onto}) {
		t.Errorf("second commit parents %v, want the first then main", got)
	}
	if !slices.Equal(sg.Request.Commits[1].Parents, []string{s.Onto}) || sg.Request.Commits[0].Tree != s.Git(t, "rev-parse", s.OldBase+"^{tree}") {
		t.Errorf("recorded request %+v, want it resolved to ids", sg.Request)
	}
}

// A signing program that fails fails the signing: nothing is created
// unsigned, and once the program works the same request signs it all.
func TestSignFailsWithoutAnUnsignedFallback(t *testing.T) {
	s, _ := signingStack(t)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	failing := program(t, failingGPG)
	s.Git(t, "config", "gpg.program", failing)
	objects := s.Git(t, "count-objects", "-v")

	sg, err := Signer{}.Sign(ctx, ws, rewrite(s, "op"))
	if !errors.Is(err, vcs.ErrUnsigned) {
		t.Fatalf("Sign with a failing signer: %v, want ErrUnsigned", err)
	}
	if len(sg.Commits) != 0 || sg.Finished {
		t.Errorf("signing: %+v, want nothing created", sg)
	}
	if len(calls(t, failing)) != 1 {
		t.Errorf("failing signer called %d times", len(calls(t, failing)))
	}
	if got := s.Git(t, "count-objects", "-v"); got != objects {
		t.Errorf("objects written by a failed signing:\n%s\nwas\n%s", got, objects)
	}
	for name := range refs(t, s) {
		if strings.HasPrefix(name, RefPrefix) {
			t.Errorf("%s kept for a failed signing", name)
		}
	}
	if st, err := (Signer{}).SignStatus(ctx, ws, "op"); err != nil || len(st.Commits) != 0 || st.Finished {
		t.Errorf("status: %+v, %v", st, err)
	}

	works := program(t, fakeGPG)
	s.Git(t, "config", "gpg.program", works)
	if sg, err := (Signer{}).Sign(ctx, ws, rewrite(s, "op")); err != nil || !sg.Finished {
		t.Errorf("retry: %+v, %v", sg, err)
	}
}

// A signing program that exits 0 and writes no signature is refused too.
func TestSignRefusesAnEmptySignature(t *testing.T) {
	s, _ := signingStack(t)
	s.Git(t, "config", "gpg.program", program(t, "#!/bin/sh\ncat > /dev/null\n"))
	if _, err := (Signer{}).Sign(context.Background(), vcs.Directory(s.Dir), rewrite(s, "op")); !errors.Is(err, vcs.ErrUnsigned) {
		t.Errorf("Sign: %v, want ErrUnsigned", err)
	}
}

// A process that stopped after keeping a commit under its ref and before
// recording it: SignStatus reports the commit, and Sign takes it up and
// signs only the rest. A ref holding some other commit is refused.
func TestAnInterruptedSigningIsTakenUp(t *testing.T) {
	s, prog := signingStack(t)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	req := rewrite(s, "op")
	sg, err := Signer{}.Sign(ctx, ws, req)
	if err != nil {
		t.Fatal(err)
	}
	// The record as it was after the first commit and before the second
	// was recorded, with the second's ref already there.
	p := filepath.Join(s.Dir, ".git", StateDir, "op.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var st signState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	st.Commits = st.Commits[:1]
	data, _ = json.Marshal(st)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := Signer{}.SignStatus(ctx, ws, "op")
	if err != nil || !status.Finished || !slices.Equal(status.Commits, sg.Commits) {
		t.Errorf("status: %+v, %v, want both commits", status, err)
	}
	again, err := Signer{}.Sign(ctx, ws, req)
	if err != nil || !slices.Equal(again.Commits, sg.Commits) {
		t.Errorf("Sign: %+v, %v, want %v", again, err, sg.Commits)
	}
	if c := calls(t, prog); len(c) != 2 {
		t.Errorf("signing program called %d times, want 2", len(c))
	}

	// Only the first commit recorded, and the second's ref moved to a
	// commit the request does not ask for.
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s.Git(t, "update-ref", commitRef("op", 1), s.Head)
	if _, err := (Signer{}).Sign(ctx, ws, req); err == nil || !strings.Contains(err.Error(), commitRef("op", 1)) {
		t.Errorf("Sign over a foreign ref: %v, want it refused", err)
	}
}

// An id is recorded with its request: a different request under it is
// refused with what was recorded, whether trees, parents or messages
// differ.
func TestSignRefusesAReusedID(t *testing.T) {
	s, _ := signingStack(t)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	sg, err := Signer{}.Sign(ctx, ws, rewrite(s, "op"))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*vcs.SignRequest){
		"message": func(r *vcs.SignRequest) { r.Commits[1].Message = "other" },
		"tree":    func(r *vcs.SignRequest) { r.Commits[1].Tree = s.Onto },
		"parent":  func(r *vcs.SignRequest) { r.Commits[0].Parents = []string{s.OldBase} },
		"linear":  func(r *vcs.SignRequest) { r.Commits[1].FollowsPrevious = false },
		"commits": func(r *vcs.SignRequest) { r.Commits = r.Commits[:1] },
	} {
		req := rewrite(s, "op")
		change(&req)
		got, err := Signer{}.Sign(ctx, ws, req)
		if !errors.Is(err, vcs.ErrSigningMismatch) || !slices.Equal(got.Commits, sg.Commits) {
			t.Errorf("%s changed: %+v, %v, want ErrSigningMismatch with the recorded signing", name, got, err)
		}
	}
	// The same request through other revisions of the same objects is the
	// same signing.
	req := rewrite(s, "op")
	req.Commits[0].Parents = []string{s.Onto}
	req.Commits[1].Tree = s.Git(t, "rev-parse", s.Head+"^{tree}")
	if got, err := (Signer{}).Sign(ctx, ws, req); err != nil || !slices.Equal(got.Commits, sg.Commits) {
		t.Errorf("same objects: %+v, %v", got, err)
	}
}

// Requests that cannot be signed are refused before anything is recorded.
func TestSignRefusesABadRequest(t *testing.T) {
	s, prog := signingStack(t)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	for name, req := range map[string]vcs.SignRequest{
		"no id":           {Commits: rewrite(s, "x").Commits},
		"id with a slash": {ID: "a/b", Commits: rewrite(s, "x").Commits},
		"id with dots":    {ID: "a..b", Commits: rewrite(s, "x").Commits},
		"no commits":      {ID: "op"},
		"first follows":   {ID: "op", Commits: []vcs.CommitSpec{{Tree: s.Head, FollowsPrevious: true}}},
		"no tree":         {ID: "op", Commits: []vcs.CommitSpec{{Message: "m"}}},
		"unknown tree":    {ID: "op", Commits: []vcs.CommitSpec{{Tree: "nosuch"}}},
		"blob as tree":    {ID: "op", Commits: []vcs.CommitSpec{{Tree: s.Head + ":a.txt"}}},
		"tree as parent":  {ID: "op", Commits: []vcs.CommitSpec{{Tree: s.Head, Parents: []string{s.Head + "^{tree}"}}}},
	} {
		if _, err := (Signer{}).Sign(ctx, ws, req); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".git", StateDir, "op.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused request was recorded: %v", err)
	}
	if c := calls(t, prog); len(c) != 0 {
		t.Errorf("signing program called for refused requests: %q", c)
	}
}

// A repository with no configured identity is refused, though the
// environment names one, and nothing is signed.
func TestSignRefusesAnUnconfiguredIdentity(t *testing.T) {
	s, prog := signingStack(t)
	s.Git(t, "config", "--unset", "user.email")
	_, err := Signer{}.Sign(context.Background(), vcs.Directory(s.Dir), rewrite(s, "op"))
	if err == nil || !strings.Contains(err.Error(), "no configured identity") {
		t.Errorf("Sign: %v, want the identity refused", err)
	}
	if c := calls(t, prog); len(c) != 0 {
		t.Errorf("signing program called: %q", c)
	}
}

// author.* and committer.* in the repository's configuration name who
// authors and who commits.
func TestSignUsesTheConfiguredAuthorAndCommitter(t *testing.T) {
	s, _ := signingStack(t)
	s.Git(t, "config", "author.name", "Owner Author")
	s.Git(t, "config", "committer.email", "commits@example.com")
	sg, err := Signer{}.Sign(context.Background(), vcs.Directory(s.Dir), rewrite(s, "op"))
	if err != nil {
		t.Fatal(err)
	}
	if got := show(t, s, sg.Head, "%an <%ae>|%cn <%ce>"); got != "Owner Author <owner@example.com>|Owner <commits@example.com>" {
		t.Errorf("author and committer %q", got)
	}
}

// The repository's signing format is used: with gpg.format ssh, its ssh
// program signs with the configured key file.
func TestSignUsesTheSigningFormat(t *testing.T) {
	s, gpg := signingStack(t)
	ssh := program(t, fakeSSH)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Git(t, "config", "gpg.format", "ssh")
	s.Git(t, "config", "gpg.ssh.program", ssh)
	s.Git(t, "config", "user.signingKey", key)
	sg, err := Signer{}.Sign(context.Background(), vcs.Directory(s.Dir), rewrite(s, "op"))
	if err != nil {
		t.Fatal(err)
	}
	if raw := s.Git(t, "cat-file", "commit", sg.Head); !strings.Contains(raw, "\ngpgsig -----BEGIN SSH SIGNATURE-----") {
		t.Errorf("commit:\n%s\nwant an ssh signature", raw)
	}
	if c := calls(t, ssh); len(c) != 2 || !strings.Contains(c[0], "-f "+key) {
		t.Errorf("ssh program calls %q, want two with the key file", c)
	}
	if c := calls(t, gpg); len(c) != 0 {
		t.Errorf("gpg called with gpg.format ssh: %q", c)
	}
}

// With a disposable ssh key, the signature is one git verifies against
// the key.
func TestSignWithADisposableSSHKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	s, _ := signingStack(t)
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "disposable", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, []byte("owner@example.com "+string(pub)), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Git(t, "config", "gpg.format", "ssh")
	s.Git(t, "config", "user.signingKey", key)
	s.Git(t, "config", "gpg.ssh.allowedSignersFile", allowed)
	sg, err := Signer{}.Sign(context.Background(), vcs.Directory(s.Dir), rewrite(s, "op"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range sg.Commits {
		s.Git(t, "verify-commit", c)
	}
}

// requireJJ skips without jj, except in the test runtime, and gives jj a
// configuration of the test's own.
func requireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("BUSYBEES_REQUIRE_JJ") != "" {
			t.Fatalf("BUSYBEES_REQUIRE_JJ is set and jj is not installed: %v", err)
		}
		t.Skip("jj is not installed")
	}
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("user.name = \"Test\"\nuser.email = \"test@example.com\"\nui.paginate = \"never\"\nui.color = \"never\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", cfg)
}

func jj(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// In a git repository Jujutsu is colocated with, the signing leaves the
// bookmarks and the working-copy change as they were.
func TestSignInAColocatedJujutsuRepository(t *testing.T) {
	s, _ := signingStack(t)
	requireJJ(t)
	jj(t, s.Dir, "git", "init", "--colocate")
	bookmarks := jj(t, s.Dir, "bookmark", "list", "--all")
	working := jj(t, s.Dir, "log", "--no-graph", "-r", "@", "-T", "commit_id")

	sg, err := Signer{}.Sign(context.Background(), vcs.Directory(s.Dir), rewrite(s, "op"))
	if err != nil || !sg.Finished {
		t.Fatalf("Sign: %+v, %v", sg, err)
	}
	if got := jj(t, s.Dir, "bookmark", "list", "--all"); got != bookmarks {
		t.Errorf("bookmarks:\n%s\nwere\n%s", got, bookmarks)
	}
	if got := jj(t, s.Dir, "log", "--no-graph", "-r", "@", "-T", "commit_id"); got != working {
		t.Errorf("working-copy commit %s, was %s", got, working)
	}
}

// The git store of a Jujutsu repository that is not colocated is signed
// in directly, and jj goes on reading it.
func TestSignInAJujutsuGitStore(t *testing.T) {
	isolate(t)
	requireJJ(t)
	dir := t.TempDir()
	jj(t, dir, "git", "init", "--no-colocate")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jj(t, dir, "commit", "-m", "reviewed")
	reviewed := jj(t, dir, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	store := filepath.Join(dir, ".jj", "repo", "store", "git")
	git := func(args ...string) string {
		t.Helper()
		out, err := Signer{}.run(context.Background(), store, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git("config", "user.name", "Owner")
	git("config", "user.email", "owner@example.com")
	git("config", "gpg.program", program(t, fakeGPG))

	sg, err := Signer{}.Sign(context.Background(), vcs.Directory(store), vcs.SignRequest{ID: "op", Commits: []vcs.CommitSpec{
		{Tree: reviewed + "^{tree}", Message: "signed\n"},
	}})
	if err != nil || !sg.Finished {
		t.Fatalf("Sign: %+v, %v", sg, err)
	}
	if got := git("rev-parse", sg.Head+"^{tree}"); got != git("rev-parse", reviewed+"^{tree}") {
		t.Errorf("tree %s, want the reviewed one's", got)
	}
	if _, err := os.Stat(filepath.Join(store, StateDir, "op.json")); err != nil {
		t.Errorf("record not in the store: %v", err)
	}
	jj(t, dir, "git", "import")
	jj(t, dir, "log", "--no-graph", "-r", "all()")
}
