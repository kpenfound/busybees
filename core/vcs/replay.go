package vcs

import (
	"context"
	"errors"
)

// Replayer is an optional capability of a Provider: it replays a range of
// revisions onto another revision in a workspace the caller retains, and
// leaves every branch where it is. core/vcs/git and core/vcs/jj implement
// it over a workspace directory. A caller finds it by type assertion:
//
//	if r, ok := provider.(vcs.Replayer); ok { ... }
//
// A replay creates a candidate and nothing else. Moving a branch to the
// candidate, with the caller's compare-and-swap, and publishing it are the
// caller's. A replay runs in the caller's process with the caller's VCS
// access: it is not a session and grants a session nothing.
//
// A replay that stops on a conflict stays in progress in the workspace, its
// state recorded in the workspace's VCS metadata, until Continue finishes it
// or Abort drops it. A process that restarts picks it up with ReplayStatus
// and Continue.
type Replayer interface {
	// Replay replays req in the workspace. It returns a finished replay with
	// its candidate, or one in progress with the paths that conflict. A
	// workspace with a replay in progress is refused with
	// ErrReplayInProgress.
	Replay(ctx context.Context, ws Workspace, req ReplayRequest) (Replay, error)
	// ReplayStatus reports the replay in progress in the workspace; a
	// Replay whose InProgress is false when there is none.
	ReplayStatus(ctx context.Context, ws Workspace) (Replay, error)
	// Continue takes the caller's resolution of the files in the workspace
	// and goes on with the replay, until it finishes or stops on the next
	// conflict. A conflict the caller has not resolved is refused with an
	// error wrapping ErrUnresolved, returned with the replay as it stands.
	// With no replay in progress it returns ErrNoReplay.
	Continue(ctx context.Context, ws Workspace) (Replay, error)
	// Abort drops the replay in progress and what it created, and returns
	// the workspace to where it was before Replay. With no replay in
	// progress it returns ErrNoReplay.
	Abort(ctx context.Context, ws Workspace) error
}

// ReplayRequest names the revisions of a replay: the ones reachable from
// Head and not from OldBase, oldest first, are replayed onto Onto. OldBase
// is the boundary the caller chose, typically the revision a dependent
// branch was started from, and must be an ancestor of Head. A merge-base
// is not used: when the parent of a dependent branch was integrated as a
// squash, the merge-base with the new upstream lies below the parent's
// commits, and replaying from it would bring them back. A range holding a
// merge is refused. Each value is a revision the provider resolves.
type ReplayRequest struct {
	OldBase string
	Head    string
	Onto    string
}

// Replay is the state of one replay.
type Replay struct {
	// InProgress is whether the replay stopped on a conflict and waits for
	// Continue or Abort.
	InProgress bool
	// Candidate is the replayed Head, once the replay has finished; empty
	// while it is in progress.
	Candidate string
	// Conflicts are the paths, relative to the workspace and sorted, that
	// conflict in the revision the replay stopped on.
	Conflicts []string
	// Commits are the replayed revisions in order, each original beside the
	// revision it became; Replayed is empty for one not replayed yet.
	Commits []ReplayedCommit
	// Request is what was replayed, each revision resolved to its id.
	Request ReplayRequest
}

// ReplayedCommit is one revision of a replay.
type ReplayedCommit struct {
	Original string
	Replayed string
}

var (
	// ErrReplayInProgress refuses a replay in a workspace that has one.
	ErrReplayInProgress = errors.New("a replay is in progress in the workspace")
	// ErrNoReplay is returned by Continue and Abort with no replay in
	// progress.
	ErrNoReplay = errors.New("no replay is in progress in the workspace")
	// ErrUnresolved wraps a Continue whose conflicts are not all resolved.
	ErrUnresolved = errors.New("conflicts are not resolved")
)
