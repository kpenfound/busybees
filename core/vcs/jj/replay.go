// Package jj replays revisions in a Jujutsu workspace (vcs.Replayer).
package jj

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
	"strings"

	"github.com/kpenfound/busybees/core/vcs"
)

// Replayer replays revisions in a Jujutsu workspace with `jj duplicate`:
// the originals and every bookmark stay where they are, and the replayed
// revisions are new changes, one per original, with the original's
// description and author. A duplicate has a change id of its own, so a
// replay reports each original beside its duplicate (vcs.Replay.Commits),
// and change ids, which are random, make no two replays alike.
//
// jj records a conflict in the revision instead of stopping, so a replay
// always creates every revision. When one conflicts, the working copy is
// put on a new change on top of the first that does, where jj writes the
// conflict markers into the files; Continue squashes the caller's
// resolution into it, jj rebases the revisions above it, and the replay
// moves on to the next one that conflicts. A finished replay leaves the
// working copy on a new change on top of the candidate. The replay's state
// is kept in the workspace's .jj directory (StateFile).
//
// The zero Replayer runs "jj" from PATH, with no user configuration of the
// caller's: jj is given a configuration of its own (JJ_CONFIG). It fetches
// and pushes nothing.
type Replayer struct {
	// Bin is the jj executable; empty is "jj".
	Bin string
	// User and Email are who a replayed revision is committed by; empty
	// is "vcs replay" and "replay@localhost". The author is the original's.
	User, Email string
}

var _ vcs.Replayer = Replayer{}

// StateFile is the file in the workspace's .jj directory that holds a
// replay in progress.
const StateFile = "core-replay.json"

// state is a replay in progress, as StateFile holds it.
type state struct {
	Request vcs.ReplayRequest `json:"request"`
	// Commits are the originals in the order they are replayed, and
	// Changes the change ids of their duplicates, in the same order.
	Commits []string `json:"commits"`
	Changes []string `json:"changes"`
	// Resolving is the change the working copy was put on top of.
	Resolving string `json:"resolving"`
	// Restore is the change the working copy was, and RestoreParents its
	// parents' commits, before the replay.
	Restore        string   `json:"restore"`
	RestoreParents []string `json:"restore_parents"`
}

// Replay duplicates the revisions of req.OldBase..req.Head onto req.Onto.
func (j Replayer) Replay(ctx context.Context, ws vcs.Workspace, req vcs.ReplayRequest) (vcs.Replay, error) {
	dir := ws.Directory()
	if st, err := j.load(ctx, dir); err != nil {
		return vcs.Replay{}, err
	} else if st != nil {
		r, err := j.report(ctx, dir, st)
		if err != nil {
			return vcs.Replay{}, err
		}
		return r, vcs.ErrReplayInProgress
	}
	var err error
	resolved := vcs.ReplayRequest{}
	for _, r := range []struct {
		name string
		rev  string
		to   *string
	}{{"old base", req.OldBase, &resolved.OldBase}, {"head", req.Head, &resolved.Head}, {"onto", req.Onto, &resolved.Onto}} {
		if *r.to, err = j.commit(ctx, dir, r.rev); err != nil {
			return vcs.Replay{}, fmt.Errorf("the replay's %s: %w", r.name, err)
		}
	}
	if ids, err := j.ids(ctx, dir, resolved.OldBase+" & ::"+resolved.Head, "commit_id"); err != nil {
		return vcs.Replay{}, err
	} else if len(ids) == 0 {
		return vcs.Replay{}, fmt.Errorf("the old base %s is not an ancestor of the head %s", req.OldBase, req.Head)
	}
	rng := resolved.OldBase + ".." + resolved.Head
	if merges, err := j.ids(ctx, dir, "merges() & ("+rng+")", "commit_id"); err != nil {
		return vcs.Replay{}, err
	} else if len(merges) > 0 {
		return vcs.Replay{}, fmt.Errorf("the range %s..%s holds the merge %s, which a replay does not take", req.OldBase, req.Head, merges[0])
	}
	commits, err := j.ids(ctx, dir, rng, "commit_id")
	if err != nil {
		return vcs.Replay{}, err
	}
	slices.Reverse(commits) // jj lists children first
	st := &state{Request: resolved, Commits: commits}
	if len(commits) == 0 {
		return vcs.Replay{Request: resolved, Candidate: resolved.Onto}, nil
	}
	if st.Restore, st.RestoreParents, err = j.workingCopy(ctx, dir); err != nil {
		return vcs.Replay{}, err
	}
	out, err := j.output(ctx, dir, true, append(append([]string{"duplicate"}, commits...), "-d", resolved.Onto)...)
	if err != nil {
		return vcs.Replay{}, err
	}
	if st.Changes, err = j.duplicates(ctx, dir, commits, out); err != nil {
		return vcs.Replay{}, err
	}
	return j.advance(ctx, dir, st)
}

// ReplayStatus reads the replay in progress.
func (j Replayer) ReplayStatus(ctx context.Context, ws vcs.Workspace) (vcs.Replay, error) {
	st, err := j.load(ctx, ws.Directory())
	if err != nil || st == nil {
		return vcs.Replay{}, err
	}
	return j.report(ctx, ws.Directory(), st)
}

// Continue squashes the working copy, the caller's resolution, into the
// replayed revision under it, and moves the working copy to the next one
// that conflicts, or finishes.
func (j Replayer) Continue(ctx context.Context, ws vcs.Workspace) (vcs.Replay, error) {
	dir := ws.Directory()
	st, err := j.load(ctx, dir)
	if err != nil {
		return vcs.Replay{}, err
	}
	if st == nil {
		return vcs.Replay{}, vcs.ErrNoReplay
	}
	parents, err := j.ids(ctx, dir, "@-", "change_id")
	if err != nil {
		return vcs.Replay{}, err
	}
	if len(parents) != 1 || !slices.Contains(st.Changes, parents[0]) {
		return vcs.Replay{}, fmt.Errorf("the working copy of %s is not on a revision of the replay (its parents are %v)", dir, parents)
	}
	if _, err := j.run(ctx, dir, "squash"); err != nil {
		return vcs.Replay{}, err
	}
	previous := st.Resolving
	r, err := j.advance(ctx, dir, st)
	if err == nil && r.InProgress && st.Resolving == previous {
		err = fmt.Errorf("%w: %s still conflict", vcs.ErrUnresolved, strings.Join(r.Conflicts, ", "))
	}
	return r, err
}

// Abort abandons the replayed revisions, and the working copy on them,
// and returns the working copy to the change it was before the replay.
func (j Replayer) Abort(ctx context.Context, ws vcs.Workspace) error {
	dir := ws.Directory()
	st, err := j.load(ctx, dir)
	if err != nil {
		return err
	}
	if st == nil {
		return vcs.ErrNoReplay
	}
	abandon := slices.Clone(st.Changes)
	if parents, err := j.ids(ctx, dir, "@-", "change_id"); err == nil && len(parents) == 1 && slices.Contains(st.Changes, parents[0]) {
		abandon = append(abandon, "@")
	}
	if _, err := j.run(ctx, dir, append([]string{"abandon"}, abandon...)...); err != nil {
		return err
	}
	if ids, err := j.ids(ctx, dir, st.Restore, "change_id"); err == nil && len(ids) == 1 {
		_, err = j.run(ctx, dir, "edit", st.Restore)
		if err != nil {
			return err
		}
	} else if _, err := j.run(ctx, dir, append([]string{"new"}, st.RestoreParents...)...); err != nil {
		return err
	}
	return j.remove(ctx, dir)
}

// advance finds the first replayed revision that conflicts, in the order
// of the originals, and puts the working copy on top of it, or, with none,
// on top of the candidate, and forgets the replay.
func (j Replayer) advance(ctx context.Context, dir string, st *state) (vcs.Replay, error) {
	conflicted, err := j.ids(ctx, dir, "conflicts() & ("+strings.Join(st.Changes, " | ")+")", "change_id")
	if err != nil {
		return vcs.Replay{}, err
	}
	i := slices.IndexFunc(st.Changes, func(c string) bool { return slices.Contains(conflicted, c) })
	if i < 0 {
		r, err := j.report(ctx, dir, st)
		if err != nil {
			return vcs.Replay{}, err
		}
		r.InProgress, r.Conflicts = false, nil
		r.Candidate = r.Commits[len(r.Commits)-1].Replayed
		if _, err := j.run(ctx, dir, "new", st.Changes[len(st.Changes)-1]); err != nil {
			return vcs.Replay{}, err
		}
		return r, j.remove(ctx, dir)
	}
	if st.Resolving != st.Changes[i] {
		if parents, err := j.ids(ctx, dir, "@-", "change_id"); err != nil {
			return vcs.Replay{}, err
		} else if len(parents) != 1 || parents[0] != st.Changes[i] {
			if _, err := j.run(ctx, dir, "new", st.Changes[i]); err != nil {
				return vcs.Replay{}, err
			}
		}
		st.Resolving = st.Changes[i]
		if err := j.save(ctx, dir, st); err != nil {
			return vcs.Replay{}, err
		}
	}
	return j.report(ctx, dir, st)
}

// report is the replay in progress: each original beside its duplicate's
// commit, and the paths that conflict in the revision being resolved.
func (j Replayer) report(ctx context.Context, dir string, st *state) (vcs.Replay, error) {
	r := vcs.Replay{Request: st.Request, InProgress: true}
	for i, c := range st.Commits {
		id, err := j.commit(ctx, dir, st.Changes[i])
		if err != nil {
			return vcs.Replay{}, err
		}
		r.Commits = append(r.Commits, vcs.ReplayedCommit{Original: c, Replayed: id})
	}
	if st.Resolving != "" {
		conflicts, err := j.conflicts(ctx, dir, st.Resolving)
		if err != nil {
			return vcs.Replay{}, err
		}
		r.Conflicts = conflicts
	}
	return r, nil
}

// conflictDescription is what `jj resolve --list` writes after a path.
var conflictDescription = regexp.MustCompile(`\s+\d+-sided conflict.*$`)

// conflicts are the paths that conflict in a revision, sorted; none when
// it has no conflict.
func (j Replayer) conflicts(ctx context.Context, dir, rev string) ([]string, error) {
	if ids, err := j.ids(ctx, dir, "conflicts() & "+rev, "change_id"); err != nil || len(ids) == 0 {
		return nil, err
	}
	out, err := j.run(ctx, dir, "resolve", "--list", "-r", rev)
	if err != nil {
		return nil, err
	}
	var paths []string
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		if p := conflictDescription.ReplaceAllString(line, ""); p != "" {
			paths = append(paths, filepath.ToSlash(p))
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// duplicatedLine is a line `jj duplicate` writes for each revision it
// duplicates: the original's commit id, then the duplicate's change id and
// commit id, shortened.
var duplicatedLine = regexp.MustCompile(`(?m)^Duplicated ([0-9a-f]+) as ([k-z]+) ([0-9a-f]+)\b`)

// duplicates are the change ids of the duplicates of commits, in their
// order, read from what `jj duplicate` wrote.
func (j Replayer) duplicates(ctx context.Context, dir string, commits []string, out string) ([]string, error) {
	changes := make([]string, len(commits))
	for _, m := range duplicatedLine.FindAllStringSubmatch(out, -1) {
		i := slices.IndexFunc(commits, func(c string) bool { return strings.HasPrefix(c, m[1]) })
		if i < 0 {
			continue
		}
		ids, err := j.ids(ctx, dir, m[3], "change_id")
		if err != nil {
			return nil, err
		}
		if len(ids) != 1 || !strings.HasPrefix(ids[0], m[2]) {
			return nil, fmt.Errorf("the duplicate %s of %s: change %v", m[3], commits[i], ids)
		}
		changes[i] = ids[0]
	}
	for i, c := range changes {
		if c == "" {
			return nil, fmt.Errorf("jj duplicate did not say what %s became:\n%s", commits[i], out)
		}
	}
	return changes, nil
}

// workingCopy is the working copy's change and its parents' commits.
func (j Replayer) workingCopy(ctx context.Context, dir string) (string, []string, error) {
	change, err := j.ids(ctx, dir, "@", "change_id")
	if err != nil {
		return "", nil, err
	}
	parents, err := j.ids(ctx, dir, "@-", "commit_id")
	if err != nil {
		return "", nil, err
	}
	return change[0], parents, nil
}

// commit resolves a revision to the id of one commit.
func (j Replayer) commit(ctx context.Context, dir, rev string) (string, error) {
	if rev == "" {
		return "", errors.New("no revision")
	}
	ids, err := j.ids(ctx, dir, rev, "commit_id")
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("revision %s is %d commits, not one", rev, len(ids))
	}
	return ids[0], nil
}

// ids are a keyword of each revision of revset, in jj's order.
func (j Replayer) ids(ctx context.Context, dir, revset, keyword string) ([]string, error) {
	out, err := j.run(ctx, dir, "log", "--no-graph", "-r", revset, "-T", keyword+` ++ "\n"`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (j Replayer) statePath(ctx context.Context, dir string) (string, error) {
	root, err := j.run(ctx, dir, "root")
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(root), ".jj", StateFile), nil
}

func (j Replayer) load(ctx context.Context, dir string) (*state, error) {
	p, err := j.statePath(ctx, dir)
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
	if len(st.Commits) == 0 || len(st.Changes) != len(st.Commits) {
		return nil, fmt.Errorf("%s holds no replay in progress", p)
	}
	return st, nil
}

// save writes the state through a temporary file, so a process that stops
// part way leaves the previous state or the new one.
func (j Replayer) save(ctx context.Context, dir string, st *state) error {
	p, err := j.statePath(ctx, dir)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p+".tmp", data, 0o644); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (j Replayer) remove(ctx context.Context, dir string) error {
	p, err := j.statePath(ctx, dir)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// run runs jj in dir with a configuration of the replay's own and returns
// what it wrote to stdout.
func (j Replayer) run(ctx context.Context, dir string, args ...string) (string, error) {
	return j.output(ctx, dir, false, args...)
}

// output runs jj as run does, and returns what it wrote to stdout followed,
// with stderr, by what it wrote there.
func (j Replayer) output(ctx context.Context, dir string, stderr bool, args ...string) (string, error) {
	bin := j.Bin
	if bin == "" {
		bin = "jj"
	}
	user, email := j.User, j.Email
	if user == "" {
		user = "vcs replay"
	}
	if email == "" {
		email = "replay@localhost"
	}
	cfg, err := os.CreateTemp("", "jj-replay-*.toml")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(cfg.Name()) }()
	_, err = fmt.Fprintf(cfg, "user.name = %q\nuser.email = %q\nui.paginate = \"never\"\nui.color = \"never\"\nui.editor = \"false\"\n", user, email)
	if cerr := cfg.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "JJ_CONFIG="+cfg.Name(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("jj %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	if stderr {
		return out.String() + errOut.String(), nil
	}
	return out.String(), nil
}
