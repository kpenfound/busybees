// Package workspace manages the temporary git worktrees each claude session
// runs in. Worktrees are created from the "main" checkout (the clone that
// holds bees.toml) so they share objects and remote configuration.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Workspace is a temporary directory containing a git worktree.
type Workspace struct {
	// Root is the temp directory. RepoDir is the worktree inside it, named
	// after Root's unique basename: `git worktree add` derives the id under
	// .git/worktrees/ from the leaf name, so a shared name (once "repo")
	// makes concurrent adds race for the same id.
	Root    string
	RepoDir string
	// Branch is the checked-out branch, or "" for a detached checkout.
	Branch string
	// MainRepo is the clone the worktree belongs to.
	MainRepo string
}

// Manager creates and removes workspaces.
type Manager struct {
	// MainRepo is the path of the primary clone.
	MainRepo string
	// Root is the directory temp workspaces are created under.
	Root string
	// Remote is the git remote name. Default "origin".
	Remote string
	// Keep leaves workspaces on disk after Remove (debugging).
	Keep bool
}

// NewManager returns a manager. When root is empty a directory under the
// system temp dir is used.
func NewManager(mainRepo, root string) *Manager {
	if root == "" {
		root = filepath.Join(os.TempDir(), "bees")
	}
	return &Manager{MainRepo: mainRepo, Root: root, Remote: "origin"}
}

// Git runs git in dir.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Fetch updates the main clone from the remote.
func (m *Manager) Fetch(ctx context.Context) error {
	_, err := Git(ctx, m.MainRepo, "fetch", "--prune", m.Remote)
	return err
}

// Detached creates a workspace with a detached checkout of remote/<ref>
// (used by roles that only read or test the default branch).
func (m *Manager) Detached(ctx context.Context, name, ref string) (*Workspace, error) {
	ws, err := m.prepare(name)
	if err != nil {
		return nil, err
	}
	if _, err := Git(ctx, m.MainRepo, "worktree", "add", "--detach", ws.RepoDir, m.Remote+"/"+ref); err != nil {
		_ = os.RemoveAll(ws.Root)
		return nil, err
	}
	return ws, nil
}

// Branch creates a workspace on branch. If the branch exists on the remote it
// is checked out tracking the remote; if it exists only locally it is reused;
// otherwise it is created from remote/<base>.
func (m *Manager) Branch(ctx context.Context, name, branch, base string) (*Workspace, error) {
	ws, err := m.prepare(name)
	if err != nil {
		return nil, err
	}
	ws.Branch = branch
	remoteRef := m.Remote + "/" + branch

	var addErr error
	switch {
	case m.refExists(ctx, "refs/remotes/"+remoteRef):
		if m.refExists(ctx, "refs/heads/"+branch) {
			// Local branch exists (a previous worktree was removed but the
			// branch kept). Check it out and fast-forward to the remote.
			_, addErr = Git(ctx, m.MainRepo, "worktree", "add", ws.RepoDir, branch)
			if addErr == nil {
				_, addErr = Git(ctx, ws.RepoDir, "merge", "--ff-only", remoteRef)
			}
		} else {
			_, addErr = Git(ctx, m.MainRepo, "worktree", "add", "--track", "-b", branch, ws.RepoDir, remoteRef)
		}
	case m.refExists(ctx, "refs/heads/"+branch):
		_, addErr = Git(ctx, m.MainRepo, "worktree", "add", ws.RepoDir, branch)
	default:
		// --no-track: the new branch must not have origin/<base> as its
		// upstream, or a plain `git push` would refuse to push. Sessions get
		// push.autoSetupRemote through their environment (see package session).
		_, addErr = Git(ctx, m.MainRepo, "worktree", "add", "--no-track", "-b", branch, ws.RepoDir, m.Remote+"/"+base)
	}
	if addErr != nil {
		_ = os.RemoveAll(ws.Root)
		return nil, addErr
	}
	return ws, nil
}

// Remove deletes the worktree and temp directory (unless Keep is set).
func (m *Manager) Remove(ctx context.Context, ws *Workspace) error {
	if ws == nil {
		return nil
	}
	if m.Keep {
		return nil
	}
	_, err := Git(ctx, m.MainRepo, "worktree", "remove", "--force", ws.RepoDir)
	_ = os.RemoveAll(ws.Root)
	_, _ = Git(ctx, m.MainRepo, "worktree", "prune")
	return err
}

// Prune removes stale worktree metadata (e.g. after a crash).
func (m *Manager) Prune(ctx context.Context) error {
	_, err := Git(ctx, m.MainRepo, "worktree", "prune")
	return err
}

func (m *Manager) prepare(name string) (*Workspace, error) {
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(m.Root, sanitize(name)+"-")
	if err != nil {
		return nil, err
	}
	return &Workspace{Root: root, RepoDir: filepath.Join(root, filepath.Base(root)), MainRepo: m.MainRepo}, nil
}

func (m *Manager) refExists(ctx context.Context, ref string) bool {
	_, err := Git(ctx, m.MainRepo, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// DefaultBranch returns the default branch of remote, from the local
// refs/remotes/<remote>/HEAD symref (set by git clone) or, failing that, by
// asking the remote.
func DefaultBranch(ctx context.Context, dir, remote string) (string, error) {
	if out, err := Git(ctx, dir, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		return strings.TrimPrefix(out, remote+"/"), nil
	}
	out, err := Git(ctx, dir, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			return strings.TrimSpace(strings.TrimPrefix(strings.Fields(line)[1], "refs/heads/")), nil
		}
	}
	return "", fmt.Errorf("remote %s did not report a HEAD branch", remote)
}
