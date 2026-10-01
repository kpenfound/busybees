package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/busybees/core/vcs"
)

// Signer creates signed commits with `git commit-tree -S`, in any git
// store: a working tree, a bare repository, or the git store a Jujutsu
// repository shares, colocated or under .jj/repo/store/git. The author and
// the committer are the repository's configured identity (user.name and
// user.email, or author.* and committer.*), and the signature is made with
// its signing key and format (user.signingKey, gpg.format and the
// gpg.*program the format names). An identity that is not configured is
// refused rather than guessed, and GIT_AUTHOR_* and GIT_COMMITTER_* in the
// process's environment are ignored. A commit whose object carries no
// signature is never recorded, so a misconfigured signer fails Sign.
//
// Commit i of a signing is kept under the ref RefPrefix<id>/<i>, which no
// branch, tag or Jujutsu bookmark is, and the signing's record is
// StateDir/<id>.json in the repository's common git directory, written
// after each commit. A commit created and kept under its ref before the
// record said so is taken up by the next Sign or SignStatus once its tree,
// parents, message and signature are checked, so nothing is signed twice.
//
// The zero Signer runs "git" from PATH. It fetches and pushes nothing, and
// runs no hook.
type Signer struct {
	// Bin is the git executable; empty is "git".
	Bin string
}

var _ vcs.Signer = Signer{}

const (
	// RefPrefix is where a signing's commits are kept.
	RefPrefix = "refs/core-sign/"
	// StateDir is the directory of the common git directory that holds the
	// signings' records.
	StateDir = "core-sign"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// signState is a signing, as its record holds it.
type signState struct {
	Request vcs.SignRequest `json:"request"`
	Commits []string        `json:"commits"`
}

func (s *signState) signing() vcs.Signing {
	sg := vcs.Signing{Request: s.Request, Commits: slices.Clone(s.Commits)}
	if len(s.Commits) == len(s.Request.Commits) {
		sg.Finished, sg.Head = true, s.Commits[len(s.Commits)-1]
	}
	return sg
}

// Sign creates the commits of req, signed, taking up what a signing under
// req.ID created before.
func (g Signer) Sign(ctx context.Context, ws vcs.Workspace, req vcs.SignRequest) (vcs.Signing, error) {
	dir := ws.Directory()
	if err := checkRequest(req); err != nil {
		return vcs.Signing{}, err
	}
	resolved, err := g.resolve(ctx, dir, req)
	if err != nil {
		return vcs.Signing{}, err
	}
	st, err := g.load(ctx, dir, req.ID)
	if err != nil {
		return vcs.Signing{}, err
	}
	if st == nil {
		st = &signState{Request: resolved, Commits: []string{}}
		if err := g.save(ctx, dir, st); err != nil {
			return vcs.Signing{}, err
		}
	} else if !sameRequest(st.Request, resolved) {
		return st.signing(), fmt.Errorf("%w: %s", vcs.ErrSigningMismatch, req.ID)
	}
	if err := g.takeUp(ctx, dir, st, true); err != nil {
		return st.signing(), err
	}
	if len(st.Commits) < len(st.Request.Commits) {
		if err := g.identity(ctx, dir); err != nil {
			return st.signing(), err
		}
	}
	for len(st.Commits) < len(st.Request.Commits) {
		i := len(st.Commits)
		spec := st.Request.Commits[i]
		parents := st.parents(i)
		id, err := g.commitTree(ctx, dir, spec.Tree, parents, spec.Message)
		if err != nil {
			return st.signing(), err
		}
		if err := g.check(ctx, dir, id, spec.Tree, parents, spec.Message); err != nil {
			return st.signing(), err
		}
		ref := commitRef(req.ID, i)
		if _, err := g.run(ctx, dir, "update-ref", "--no-deref", ref, id, ""); err != nil {
			// Another process created it first: its commit is taken up
			// when it is the one this request asks for.
			if terr := g.takeUp(ctx, dir, st, true); terr != nil || len(st.Commits) <= i {
				return st.signing(), err
			}
			continue
		}
		st.Commits = append(st.Commits, id)
		if err := g.save(ctx, dir, st); err != nil {
			return st.signing(), err
		}
	}
	return st.signing(), nil
}

// SignStatus reports the signing recorded under id, with any commit kept
// under its ref that the record does not say yet.
func (g Signer) SignStatus(ctx context.Context, ws vcs.Workspace, id string) (vcs.Signing, error) {
	dir := ws.Directory()
	if !validID.MatchString(id) {
		return vcs.Signing{}, fmt.Errorf("the signing id %q is not letters, digits, '-' and '_', starting with a letter or digit, 128 at most", id)
	}
	st, err := g.load(ctx, dir, id)
	if err != nil {
		return vcs.Signing{}, err
	}
	if st == nil {
		return vcs.Signing{}, fmt.Errorf("%w: %s", vcs.ErrNoSigning, id)
	}
	if err := g.takeUp(ctx, dir, st, false); err != nil {
		return st.signing(), err
	}
	return st.signing(), nil
}

// Forget deletes the refs of the signing recorded under id, then its
// record.
func (g Signer) Forget(ctx context.Context, ws vcs.Workspace, id string) error {
	dir := ws.Directory()
	if !validID.MatchString(id) {
		return fmt.Errorf("the signing id %q is not letters, digits, '-' and '_', starting with a letter or digit, 128 at most", id)
	}
	st, err := g.load(ctx, dir, id)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("%w: %s", vcs.ErrNoSigning, id)
	}
	refs, err := g.run(ctx, dir, "for-each-ref", "--format=%(refname)", RefPrefix+id+"/")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(refs) {
		if _, err := g.run(ctx, dir, "update-ref", "--no-deref", "-d", ref); err != nil {
			return err
		}
	}
	p, err := g.statePath(ctx, dir, id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func checkRequest(req vcs.SignRequest) error {
	if !validID.MatchString(req.ID) {
		return fmt.Errorf("the signing id %q is not letters, digits, '-' and '_', starting with a letter or digit, 128 at most", req.ID)
	}
	if len(req.Commits) == 0 {
		return errors.New("the signing has no commits")
	}
	if req.Commits[0].FollowsPrevious {
		return errors.New("the signing's first commit follows no previous commit")
	}
	return nil
}

// resolve resolves the request's trees and parents to ids.
func (g Signer) resolve(ctx context.Context, dir string, req vcs.SignRequest) (vcs.SignRequest, error) {
	out := vcs.SignRequest{ID: req.ID}
	for i, c := range req.Commits {
		r := vcs.CommitSpec{FollowsPrevious: c.FollowsPrevious, Message: c.Message}
		var err error
		if r.Tree, err = g.object(ctx, dir, c.Tree, "tree"); err != nil {
			return vcs.SignRequest{}, fmt.Errorf("the tree of commit %d: %w", i, err)
		}
		for _, p := range c.Parents {
			id, err := g.object(ctx, dir, p, "commit")
			if err != nil {
				return vcs.SignRequest{}, fmt.Errorf("a parent of commit %d: %w", i, err)
			}
			r.Parents = append(r.Parents, id)
		}
		out.Commits = append(out.Commits, r)
	}
	return out, nil
}

func (g Signer) object(ctx context.Context, dir, rev, kind string) (string, error) {
	if rev == "" {
		return "", errors.New("no revision")
	}
	id, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{"+kind+"}")
	if err != nil {
		return "", fmt.Errorf("%s is not a %s", rev, kind)
	}
	return id, nil
}

func sameRequest(a, b vcs.SignRequest) bool {
	return a.ID == b.ID && slices.EqualFunc(a.Commits, b.Commits, func(x, y vcs.CommitSpec) bool {
		return x.Tree == y.Tree && x.FollowsPrevious == y.FollowsPrevious && x.Message == y.Message && slices.Equal(x.Parents, y.Parents)
	})
}

// parents are the parents of commit i, the previous commit first when it
// follows it.
func (s *signState) parents(i int) []string {
	c := s.Request.Commits[i]
	if c.FollowsPrevious {
		return append([]string{s.Commits[i-1]}, c.Parents...)
	}
	return slices.Clone(c.Parents)
}

func commitRef(id string, i int) string {
	return RefPrefix + id + "/" + strconv.Itoa(i)
}

// takeUp adds to st the commits kept under the signing's refs past the ones
// it records, each checked against the request, and with save records
// them. A ref holding a commit the request does not ask for is an error.
func (g Signer) takeUp(ctx context.Context, dir string, st *signState, save bool) error {
	took := false
	for len(st.Commits) < len(st.Request.Commits) {
		i := len(st.Commits)
		ref := commitRef(st.Request.ID, i)
		id, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
		if err != nil || id == "" {
			break
		}
		spec := st.Request.Commits[i]
		if err := g.check(ctx, dir, id, spec.Tree, st.parents(i), spec.Message); err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		st.Commits = append(st.Commits, id)
		took = true
	}
	if took && save {
		return g.save(ctx, dir, st)
	}
	return nil
}

// identity refuses a repository with no configured author or committer.
func (g Signer) identity(ctx context.Context, dir string) error {
	for _, who := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		if _, err := g.run(ctx, dir, "-c", "user.useConfigOnly=true", "var", who); err != nil {
			return fmt.Errorf("the repository %s has no configured identity to commit as: %w", dir, err)
		}
	}
	return nil
}

// commitTree creates the signed commit, the repository's identity as its
// author and committer.
func (g Signer) commitTree(ctx context.Context, dir, tree string, parents []string, message string) (string, error) {
	args := []string{"-c", "user.useConfigOnly=true", "commit-tree", "-S"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-", tree)
	id, err := g.output(ctx, dir, strings.NewReader(message), args...)
	if err != nil {
		return "", fmt.Errorf("%w: %w", vcs.ErrUnsigned, err)
	}
	return strings.TrimSpace(id), nil
}

// check reads the commit and refuses it unless it records the tree, the
// parents and the message, and carries a signature.
func (g Signer) check(ctx context.Context, dir, id, tree string, parents []string, message string) error {
	raw, err := g.output(ctx, dir, nil, "cat-file", "commit", id)
	if err != nil {
		return err
	}
	c := parseCommit(raw)
	switch {
	case !c.signed:
		return fmt.Errorf("%w: %s carries no signature", vcs.ErrUnsigned, id)
	case c.tree != tree:
		return fmt.Errorf("the commit %s records the tree %s, not %s", id, c.tree, tree)
	case !slices.Equal(c.parents, parents):
		return fmt.Errorf("the commit %s has the parents %v, not %v", id, c.parents, parents)
	case c.message != message:
		return fmt.Errorf("the commit %s has the message %q, not %q", id, c.message, message)
	}
	return nil
}

type commitObject struct {
	tree    string
	parents []string
	signed  bool
	message string
}

// parseCommit reads a raw commit object: its headers, a continuation line
// starting with a space, then a blank line and the message.
func parseCommit(raw string) commitObject {
	var c commitObject
	headers, message, _ := strings.Cut(raw, "\n\n")
	c.message = message
	for line := range strings.SplitSeq(headers, "\n") {
		if strings.HasPrefix(line, " ") {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.tree = value
		case "parent":
			c.parents = append(c.parents, value)
		case "gpgsig", "gpgsig-sha256":
			c.signed = true
		}
	}
	return c
}

// statePath is the record of the signing id, in the common git directory
// so that every worktree of the repository sees it.
func (g Signer) statePath(ctx context.Context, dir, id string) (string, error) {
	common, err := g.run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(common, StateDir, id+".json"), nil
}

func (g Signer) load(ctx context.Context, dir, id string) (*signState, error) {
	p, err := g.statePath(ctx, dir, id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := &signState{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if st.Request.ID != id || len(st.Request.Commits) == 0 || len(st.Commits) > len(st.Request.Commits) {
		return nil, fmt.Errorf("%s holds no signing %s", p, id)
	}
	return st, nil
}

// save writes the record through a temporary file, so a process that stops
// part way leaves the previous record or the new one.
func (g Signer) save(ctx context.Context, dir string, st *signState) error {
	p, err := g.statePath(ctx, dir, st.Request.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ignoredEnv are the variables that would make git act on another
// repository or commit as someone other than the repository's identity.
var ignoredEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_NAMESPACE",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
}

// run runs git as output does and returns its output trimmed of
// surrounding space.
func (g Signer) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.output(ctx, dir, nil, args...)
	return strings.TrimSpace(out), err
}

// output runs git in dir with stdin and its fixed configuration, in the
// process's environment less ignoredEnv, and returns its output.
func (g Signer) output(ctx context.Context, dir string, stdin *strings.Reader, args ...string) (string, error) {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	full := append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "gc.auto=0"}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(ignoredEnv, name) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_ADVICE=0", "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}
