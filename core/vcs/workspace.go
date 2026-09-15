// Package vcs defines workspaces without prescribing a version control system.
package vcs

import "context"

// Workspace is the directory a session runs in and its optional VCS resources.
// Providing resources does not grant access: the session profile must allow it.
// The caller owns the workspace lifetime, which may span several sessions.
type Workspace interface {
	Directory() string
	VCS() *Access
}

// Access describes additional resources needed to operate on version control.
// Mounts are writable directories mounted at their host paths in containers.
// A workspace with no repository metadata returns nil from VCS.
type Access struct {
	Mounts []string
}

// Directory is a caller-owned directory with no VCS resources.
type Directory string

func (d Directory) Directory() string { return string(d) }
func (d Directory) VCS() *Access      { return nil }

// Request selects a workspace. Ref and Branch are opaque to core: the provider
// interprets them. An empty Branch asks for a workspace without a writable branch.
type Request struct {
	Name   string
	Ref    string
	Branch string
}

// Provider owns workspace creation, release and recovery. Release must receive
// the workspace returned by Acquire. Callers release on errors and cancellation
// using a non-cancelled context; providers apply their retention policy there.
// Prune recovers stale provider metadata after an interrupted process.
type Provider interface {
	Acquire(context.Context, Request) (Workspace, error)
	Release(context.Context, Workspace) error
	Prune(context.Context) error
}
