// Package git replays revisions in a git working tree (vcs.Replayer).
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
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/vcs"
)

// Replayer replays revisions in a git working tree, one commit at a time
// with cherry-pick, with the working tree detached at the replay's tip:
// no branch is moved. Each replayed commit keeps its original's message,
// author and committer, so the same replay of the same commits onto the
// same revision gives the same commit ids, and a commit that is or
// becomes empty is kept, so the caller's commit boundaries stay. A replay
// that stops on a conflict leaves it in the working tree for the caller to
// resolve, and its state in the worktree's git directory (StateFile).
// Replay refuses a working tree with changes to tracked files.
//
// The zero Replayer runs "git" from PATH. It fetches and pushes nothing.
type Replayer struct {
	// Bin is the git executable; empty is "git".
	Bin string
}

var _ vcs.Replayer = Replayer{}

// StateFile is the file in the worktree's git directory that holds a
// replay in progress.
const StateFile = "core-replay.json"

// state is a replay in progress, as StateFile holds it.
type state struct {
	Request vcs.ReplayRequest `json:"request"`
	// Commits are the originals in the order they are replayed.
	Commits []string `json:"commits"`
	// Replayed are the commits the first len(Replayed) originals became.
	Replayed []string `json:"replayed"`
	// Tip is the commit the next original is replayed onto.
	Tip string `json:"tip"`
	// Restore is what HEAD was before the replay: a ref, or a commit id
	// when it was detached.
	Restore string `json:"restore"`
	// Conflicts are the paths the next original conflicted in, sorted.
	Conflicts []string `json:"conflicts,omitempty"`
}

func (s *state) replay() vcs.Replay {
	r := vcs.Replay{Request: s.Request, Conflicts: slices.Clone(s.Conflicts)}
	for i, c := range s.Commits {
		rc := vcs.ReplayedCommit{Original: c}
		if i < len(s.Replayed) {
			rc.Replayed = s.Replayed[i]
		}
		r.Commits = append(r.Commits, rc)
	}
	if len(s.Replayed) == len(s.Commits) {
		r.Candidate = s.Tip
	} else {
		r.InProgress = true
	}
	return r
}

// Replay detaches the working tree at req.Onto and replays the commits of
// req.OldBase..req.Head onto it.
func (g Replayer) Replay(ctx context.Context, ws vcs.Workspace, req vcs.ReplayRequest) (vcs.Replay, error) {
	dir := ws.Directory()
	if st, err := g.load(ctx, dir); err != nil {
		return vcs.Replay{}, err
	} else if st != nil {
		return st.replay(), vcs.ErrReplayInProgress
	}
	var err error
	resolved := vcs.ReplayRequest{}
	for _, r := range []struct {
		name string
		rev  string
		to   *string
	}{{"old base", req.OldBase, &resolved.OldBase}, {"head", req.Head, &resolved.Head}, {"onto", req.Onto, &resolved.Onto}} {
		if *r.to, err = g.commit(ctx, dir, r.rev); err != nil {
			return vcs.Replay{}, fmt.Errorf("the replay's %s: %w", r.name, err)
		}
	}
	if _, err := g.run(ctx, dir, nil, "merge-base", "--is-ancestor", resolved.OldBase, resolved.Head); err != nil {
		return vcs.Replay{}, fmt.Errorf("the old base %s is not an ancestor of the head %s: %w", req.OldBase, req.Head, err)
	}
	if merges, err := g.run(ctx, dir, nil, "rev-list", "--merges", resolved.OldBase+".."+resolved.Head); err != nil {
		return vcs.Replay{}, err
	} else if m := strings.Fields(merges); len(m) > 0 {
		return vcs.Replay{}, fmt.Errorf("the range %s..%s holds the merge %s, which a replay does not take", req.OldBase, req.Head, m[0])
	}
	list, err := g.run(ctx, dir, nil, "rev-list", "--reverse", "--topo-order", resolved.OldBase+".."+resolved.Head)
	if err != nil {
		return vcs.Replay{}, err
	}
	if changed, err := g.run(ctx, dir, nil, "status", "--porcelain", "--untracked-files=no"); err != nil {
		return vcs.Replay{}, err
	} else if changed != "" {
		return vcs.Replay{}, fmt.Errorf("the working tree %s has changes to tracked files:\n%s", dir, changed)
	}
	restore, err := g.run(ctx, dir, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil || restore == "" {
		if restore, err = g.run(ctx, dir, nil, "rev-parse", "--verify", "HEAD"); err != nil {
			return vcs.Replay{}, err
		}
	}
	st := &state{Request: resolved, Commits: strings.Fields(list), Replayed: []string{}, Tip: resolved.Onto, Restore: restore}
	if _, err := g.run(ctx, dir, nil, "checkout", "--quiet", "--detach", resolved.Onto); err != nil {
		return vcs.Replay{}, err
	}
	if err := g.save(ctx, dir, st); err != nil {
		return vcs.Replay{}, err
	}
	return g.proceed(ctx, dir, st)
}

// ReplayStatus reads the replay in progress.
func (g Replayer) ReplayStatus(ctx context.Context, ws vcs.Workspace) (vcs.Replay, error) {
	st, err := g.load(ctx, ws.Directory())
	if err != nil || st == nil {
		return vcs.Replay{}, err
	}
	return st.replay(), nil
}

// Continue stages the working tree, refuses a conflicted path that still
// holds a conflict marker, commits the stopped commit with its
// original's message, author and committer, and replays the rest.
func (g Replayer) Continue(ctx context.Context, ws vcs.Workspace) (vcs.Replay, error) {
	dir := ws.Directory()
	st, err := g.load(ctx, dir)
	if err != nil {
		return vcs.Replay{}, err
	}
	if st == nil {
		return vcs.Replay{}, vcs.ErrNoReplay
	}
	next := st.Commits[len(st.Replayed)]
	head, err := g.run(ctx, dir, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return vcs.Replay{}, err
	}
	picking := g.exists(ctx, dir, "CHERRY_PICK_HEAD")
	switch {
	case picking:
		if _, err := g.run(ctx, dir, nil, "add", "--all"); err != nil {
			return vcs.Replay{}, err
		}
		if marked, err := g.markers(ctx, dir, st.Conflicts); err != nil {
			return vcs.Replay{}, err
		} else if len(marked) > 0 {
			r := st.replay()
			r.Conflicts = marked
			return r, fmt.Errorf("%w: %s still hold conflict markers", vcs.ErrUnresolved, strings.Join(marked, ", "))
		}
		env, err := g.committer(ctx, dir, next)
		if err != nil {
			return vcs.Replay{}, err
		}
		if _, err := g.run(ctx, dir, env, "commit", "--quiet", "--no-verify", "--allow-empty", "--allow-empty-message", "--cleanup=verbatim", "--reuse-message="+next); err != nil {
			return vcs.Replay{}, err
		}
	case head == st.Tip:
		// Stopped before the next commit was replayed: it is replayed now.
		return g.proceed(ctx, dir, st)
	default:
		// Stopped after it was replayed and before the state said so.
		if parent, err := g.run(ctx, dir, nil, "rev-parse", "--verify", "HEAD^"); err != nil || parent != st.Tip {
			return vcs.Replay{}, fmt.Errorf("HEAD of %s is %s, which is neither the replay's tip %s nor a commit on it", dir, head, st.Tip)
		}
	}
	if st.Tip, err = g.run(ctx, dir, nil, "rev-parse", "--verify", "HEAD"); err != nil {
		return vcs.Replay{}, err
	}
	st.Replayed, st.Conflicts = append(st.Replayed, st.Tip), nil
	if err := g.save(ctx, dir, st); err != nil {
		return vcs.Replay{}, err
	}
	return g.proceed(ctx, dir, st)
}

// Abort drops a stopped cherry-pick, returns HEAD to what it was before the
// replay and forgets the replay. The commits it made are left to git's
// garbage collection.
func (g Replayer) Abort(ctx context.Context, ws vcs.Workspace) error {
	dir := ws.Directory()
	st, err := g.load(ctx, dir)
	if err != nil {
		return err
	}
	if st == nil {
		return vcs.ErrNoReplay
	}
	if _, err := g.run(ctx, dir, nil, "reset", "--quiet", "--hard"); err != nil {
		return err
	}
	if g.exists(ctx, dir, "CHERRY_PICK_HEAD") {
		if _, err := g.run(ctx, dir, nil, "cherry-pick", "--quit"); err != nil {
			return err
		}
		if err := g.remove(ctx, dir, "CHERRY_PICK_HEAD"); err != nil {
			return err
		}
	}
	target := st.Restore
	if branch, ok := strings.CutPrefix(target, "refs/heads/"); ok {
		_, err = g.run(ctx, dir, nil, "checkout", "--quiet", branch)
	} else {
		_, err = g.run(ctx, dir, nil, "checkout", "--quiet", "--detach", target)
	}
	if err != nil {
		return err
	}
	return g.remove(ctx, dir, StateFile)
}

// proceed replays the originals not replayed yet, one at a time, saving
// the state after each. It stops on the first conflict, and forgets the
// replay once every original is replayed.
func (g Replayer) proceed(ctx context.Context, dir string, st *state) (vcs.Replay, error) {
	for len(st.Replayed) < len(st.Commits) {
		next := st.Commits[len(st.Replayed)]
		env, err := g.committer(ctx, dir, next)
		if err != nil {
			return vcs.Replay{}, err
		}
		if _, err := g.run(ctx, dir, env, "cherry-pick", "--allow-empty", "--allow-empty-message", "--keep-redundant-commits", "--cleanup=verbatim", next); err != nil {
			conflicts, uerr := g.unmerged(ctx, dir)
			if uerr != nil || !g.exists(ctx, dir, "CHERRY_PICK_HEAD") {
				return vcs.Replay{}, fmt.Errorf("replay %s: %w", next, err)
			}
			st.Conflicts = conflicts
			return st.replay(), g.save(ctx, dir, st)
		}
		if st.Tip, err = g.run(ctx, dir, nil, "rev-parse", "--verify", "HEAD"); err != nil {
			return vcs.Replay{}, err
		}
		st.Replayed = append(st.Replayed, st.Tip)
		if err := g.save(ctx, dir, st); err != nil {
			return vcs.Replay{}, err
		}
	}
	r := st.replay()
	return r, g.remove(ctx, dir, StateFile)
}

// committer is the environment that gives a replayed commit the
// committer of its original.
func (g Replayer) committer(ctx context.Context, dir, commit string) ([]string, error) {
	out, err := g.run(ctx, dir, nil, "show", "--no-patch", "--format=%cn%x00%ce%x00%cd", "--date=raw", commit)
	if err != nil {
		return nil, err
	}
	f := strings.Split(out, "\x00")
	if len(f) != 3 {
		return nil, fmt.Errorf("the committer of %s: %q", commit, out)
	}
	return []string{"GIT_COMMITTER_NAME=" + f[0], "GIT_COMMITTER_EMAIL=" + f[1], "GIT_COMMITTER_DATE=" + f[2]}, nil
}

// unmerged are the paths the index has as unmerged, sorted.
func (g Replayer) unmerged(ctx context.Context, dir string) ([]string, error) {
	out, err := g.run(ctx, dir, nil, "diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	paths := strings.FieldsFunc(out, func(r rune) bool { return r == 0 })
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// markers are the paths among conflicted that still hold a line git starts
// or ends a conflict with.
func (g Replayer) markers(ctx context.Context, dir string, conflicted []string) ([]string, error) {
	top, err := g.run(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	var marked []string
	for _, p := range conflicted {
		data, err := os.ReadFile(filepath.Join(top, p))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("<<<<<<< ")) || bytes.HasPrefix(line, []byte(">>>>>>> ")) {
				marked = append(marked, p)
				break
			}
		}
	}
	return marked, nil
}

// commit resolves a revision to the id of a commit.
func (g Replayer) commit(ctx context.Context, dir, rev string) (string, error) {
	if rev == "" {
		return "", errors.New("no revision")
	}
	return g.run(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
}

// path is the path of a file in the worktree's git directory.
func (g Replayer) path(ctx context.Context, dir, name string) (string, error) {
	p, err := g.run(ctx, dir, nil, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

func (g Replayer) exists(ctx context.Context, dir, name string) bool {
	p, err := g.path(ctx, dir, name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func (g Replayer) remove(ctx context.Context, dir, name string) error {
	p, err := g.path(ctx, dir, name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (g Replayer) load(ctx context.Context, dir string) (*state, error) {
	p, err := g.path(ctx, dir, StateFile)
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
	st := &state{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if len(st.Commits) == 0 || len(st.Replayed) >= len(st.Commits) {
		return nil, fmt.Errorf("%s holds no replay in progress", p)
	}
	return st, nil
}

// save writes the state through a temporary file, so a process that stops
// part way leaves the previous state or the new one.
func (g Replayer) save(ctx context.Context, dir string, st *state) error {
	p, err := g.path(ctx, dir, StateFile)
	if err != nil {
		return err
	}
	if len(st.Replayed) == len(st.Commits) {
		return nil // finished: proceed removes it
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

// run runs git in dir with the replay's fixed configuration and env laid
// over the process's environment, and returns its output trimmed of
// surrounding space.
func (g Replayer) run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	full := append([]string{
		"-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "rerere.enabled=false",
		"-c", "gc.auto=0",
	}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_ADVICE=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
