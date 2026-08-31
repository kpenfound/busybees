package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/busybees/internal/procs"
	"github.com/kpenfound/busybees/internal/session"
)

// errSessionKilled is what a session a person stopped reports back up the
// worker. It is not a failure to retry or to escalate: the issue has already
// been handed to the person who pressed the key.
var errSessionKilled = errors.New("session stopped by a person")

// liveSession is a session running right now, as KillSession needs it: the
// directory holding its pid file and the work item it is about. It is
// recorded in runSession beside the event that announces the session, and
// dropped when that session ends.
type liveSession struct {
	role  string
	dir   string
	issue int
	pr    int
}

// recordLiveSession remembers a session for as long as it runs, so a view
// watching the event stream can name one back to KillSession.
func (s *Scheduler) recordLiveSession(name string, ls liveSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[name] = ls
}

// dropLiveSession forgets a session that has ended. The kill mark is
// deliberately left: it is read by runSessionWithRetry, one frame above the
// session that has just returned, and clearing it here would eat it every
// time. Nothing else can leave one behind — KillSession only ever marks a
// session the map still holds, and drops the mark again on every path where
// it stops nothing — and tookKill consumes the one it sets.
func (s *Scheduler) dropLiveSession(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, name)
}

// tookKill reports whether a person stopped this session, and forgets that
// they did. runSessionWithRetry asks once per attempt: a stopped session is
// neither retried nor escalated a second time, because KillSession has
// already escalated the issue.
func (s *Scheduler) tookKill(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.killed[name] {
		return false
	}
	delete(s.killed, name)
	return true
}

// KillSession stops one running session and hands the work item it was
// working on to a person. It is what the live view's kill key calls, and it
// is the same two halves the factory already has: procs stops the process
// and its group exactly as `bees kill` does, and escalate labels the issue
// bees:needs-human and says why on GitHub, exactly as a worker that gives up
// does.
//
// The session's own worker is told nothing: it sees its session die, finds
// the kill mark this leaves behind and ends without retrying and without
// escalating the issue again. A singleton owns no work item, so stopping one
// stops a session and nothing more.
//
// The name is the one the event stream publishes (Event.Session).
func (s *Scheduler) KillSession(ctx context.Context, name string) error {
	s.mu.Lock()
	ls, ok := s.live[name]
	if ok {
		// Set before the process is stopped: the worker must find the mark
		// however quickly the session dies.
		s.killed[name] = true
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no session named %q is running", name)
	}

	p, found := procs.FromPIDFile(ls.dir, nil)
	if !found {
		s.mu.Lock()
		delete(s.killed, name)
		s.mu.Unlock()
		return fmt.Errorf("session %q is not running a process this scheduler can stop", name)
	}
	s.log.Warn(fmt.Sprintf("stopping session %s: a person asked for it", name),
		"role", ls.role, "session", name, "issue", ls.issue, "pid", p.PID)
	if err := procs.Kill(p, procs.DefaultGrace); err != nil {
		s.mu.Lock()
		delete(s.killed, name)
		s.mu.Unlock()
		return fmt.Errorf("stop session %q: %w", name, err)
	}
	// The session wrote no result, so the next session for this issue would
	// otherwise have to guess whether the machine crashed.
	if err := session.MarkInterrupted(ls.dir, "stopped from the live view"); err != nil {
		s.log.Warn("could not mark the stopped session", "session", name, "err", err)
	}
	if ls.issue <= 0 {
		return nil
	}
	return s.escalate(ctx, ls.issue, killedReason(ls))
}

// killedReason is what the escalation comment says. It names the role that
// was stopped and the pull request it was working on, and it says that the
// work is unreported: the branch is where the person now has to look.
func killedReason(ls liveSession) string {
	reason := fmt.Sprintf("A person stopped the running `%s` session from `bees run`'s live view.", ls.role)
	if ls.pr > 0 {
		reason += fmt.Sprintf(" It was working on #%d.", ls.pr)
	}
	return reason + " Whatever it had done is unreported and may still be on its branch."
}
