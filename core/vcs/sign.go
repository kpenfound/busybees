package vcs

import (
	"context"
	"errors"
)

// Signer constructs signed commits from trees the caller has already
// reviewed, with parents and messages the caller gives explicitly, using
// the repository's own identity, signing key and signing format. It runs
// in the caller's process with the caller's VCS access and credentials:
// it is not a session, and it grants a session nothing, so a session that
// produced the trees never holds the signing key. core/vcs/git implements
// it for any git store, the one a Jujutsu repository shares included.
//
// A signing moves no branch and writes no file of a working tree. Each
// commit it creates is kept reachable under a ref of its own, outside the
// branches, until Forget, and what it has created is recorded in the
// repository as it goes, under the request's ID. A process interrupted
// part way reads the record with SignStatus, or calls Sign again with the
// same request, which picks it up and signs only what is missing; a
// finished signing returned again is the same commits, signed once.
// Pushing the result and moving branches to it are the caller's.
//
// A commit is never created unsigned: when signing fails, Sign fails.
type Signer interface {
	// Sign creates the commits of req, signed, or returns the ones a
	// signing under req.ID created before. A request whose ID was used for
	// a different request is refused with ErrSigningMismatch.
	Sign(ctx context.Context, ws Workspace, req SignRequest) (Signing, error)
	// SignStatus reports what the signing with the ID has created. With no
	// signing recorded under it, it returns ErrNoSigning.
	SignStatus(ctx context.Context, ws Workspace, id string) (Signing, error)
	// Forget drops the record of the signing with the ID and the refs that
	// keep its commits, once the caller is done with them. The commits are
	// left to garbage collection unless something else refers to them.
	// With no signing recorded under it, it returns ErrNoSigning.
	Forget(ctx context.Context, ws Workspace, id string) error
}

// SignRequest is one signing: commits created in order, each from a tree
// and with parents and a message the caller gives.
type SignRequest struct {
	// ID identifies the operation, so that a process that restarts can
	// find it again. It is letters, digits, '-' and '_', 128 at most,
	// starting with a letter or digit.
	ID string
	// Commits are created in order.
	Commits []CommitSpec
}

// CommitSpec is one commit of a signing.
type CommitSpec struct {
	// Tree is the tree the commit records: a revision the provider
	// resolves to a tree, recorded as its id.
	Tree string
	// FollowsPrevious makes the commit created before it in the same
	// request its first parent, ahead of Parents. A linear history is a
	// first commit with its base in Parents and every later one following
	// the previous.
	FollowsPrevious bool
	// Parents are revisions the provider resolves to commits, recorded as
	// their ids. A first commit with none, and FollowsPrevious unset, is a
	// root commit.
	Parents []string
	// Message is the commit message, written as given: no cleanup, no
	// trailer added.
	Message string
}

// Signing is what a signing has created.
type Signing struct {
	// Request is the signing as recorded, its trees and parents resolved
	// to ids.
	Request SignRequest
	// Commits are the signed commits created so far, in the order of
	// Request.Commits.
	Commits []string
	// Finished is whether every commit of the request has been created;
	// Head is then the last one.
	Finished bool
	Head     string
}

var (
	// ErrNoSigning is returned for an ID with no signing recorded.
	ErrNoSigning = errors.New("no signing is recorded under the id")
	// ErrSigningMismatch refuses a request whose ID was recorded with a
	// different request.
	ErrSigningMismatch = errors.New("the id was used for a different signing")
	// ErrUnsigned wraps a commit that could not be signed.
	ErrUnsigned = errors.New("the commit could not be signed")
)
