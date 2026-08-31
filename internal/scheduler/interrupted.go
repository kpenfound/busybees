package scheduler

import (
	"log/slog"

	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// Crash recovery. A session that was running when the scheduler died leaves
// a directory with a transcript no result file ever closed, and a branch
// that may carry work nobody reported. Nothing used to say so: the next
// session for the issue started as if the branch were untouched.
//
// The scheduler records the session it is about to run in the issue's
// bookkeeping (state.IssueState.Session) and clears it when the session
// ends, so a record that outlives its session is the signal. The worker
// that takes the issue over reads it, tells the first session of the role
// that was interrupted (prompts.Data.Interrupted) and marks itself resumed
// for `bees status`.

// takeInterrupted reports what the session recorded for an issue is doing
// now, and consumes the record. It answers nil unless a session started and
// never finished: a record whose session is still running is left alone —
// its scheduler owns it, and a session that has not written its result yet
// is simply a session in progress — and one whose session finished after
// the record was written is cleared without a word.
func (s *Scheduler) takeInterrupted(log *slog.Logger, bk *state.IssueState) *session.Interrupted {
	rec := bk.Session
	if rec == nil {
		return nil
	}
	in, running := session.CheckInterrupted(rec.Role, rec.Dir, s.alive)
	if running {
		log.Warn("a session recorded for this issue is still running; leaving it alone",
			"session", rec.Name, "role", rec.Role, "dir", rec.Dir)
		return nil
	}
	bk.Session = nil
	if err := s.store.SaveIssue(*bk); err != nil {
		log.Warn("could not clear the recorded session", "session", rec.Name, "err", err)
	}
	if in == nil {
		return nil
	}
	log.Info("the previous session for this issue was interrupted", "session", in.Name,
		"role", in.Role, "turns", in.Turns, "killed", in.Killed, "transcript", in.Transcript)
	return in
}

// holdInterrupted keeps an interruption until a session of the role it
// happened to runs for the issue, and forgetInterrupted drops it when the
// worker ends. The report is worth having for the session that takes the
// work over and stale for anything later, so it never outlives one worker.
func (s *Scheduler) holdInterrupted(issue int, in *session.Interrupted) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interrupted[issue] = in
}

func (s *Scheduler) forgetInterrupted(issue int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.interrupted, issue)
}

// interruptedFor hands the held interruption to a session of the role it
// happened to, once. Another role's session gets nothing: a developer told
// that a reviewer session was interrupted learns nothing it can act on, and
// the branch advice would be about a session that never wrote to it.
func (s *Scheduler) interruptedFor(issue int, role string) *session.Interrupted {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.interrupted[issue]
	if !ok || in == nil || in.Role != role {
		return nil
	}
	delete(s.interrupted, issue)
	return in
}

// markResumed records that a worker took over from an interrupted session,
// so `bees status` tells it apart from one starting fresh.
func (s *Scheduler) markResumed(w *state.Worker) {
	if w == nil {
		return
	}
	s.mu.Lock()
	w.Resumed = true
	s.mu.Unlock()
	s.writeStatus()
}

// recordRunningSession writes the session about to run into the issue's
// bookkeeping. The state is re-read here rather than taken from the copy the
// worker holds: the worker's copy is a stage older, and this field is the
// one thing about an issue that must survive the process that wrote it.
// Failing to record it costs a crash report, never the session.
func (s *Scheduler) recordRunningSession(spec sessionSpec, issue int, dir string) {
	bk, err := s.store.Issue(issue)
	if err != nil {
		s.log.Warn("could not read issue bookkeeping", "issue", issue, "err", err)
		return
	}
	bk.Session = &state.SessionRun{Role: spec.role, Name: spec.name, Dir: dir, StartedAt: s.now()}
	if err := s.store.SaveIssue(bk); err != nil {
		s.log.Warn("could not record the running session", "issue", issue, "session", spec.name, "err", err)
	}
}

// clearRunningSession removes that record: the session ended, whatever it
// ended with, so nothing was interrupted.
func (s *Scheduler) clearRunningSession(issue int) {
	bk, err := s.store.Issue(issue)
	if err != nil {
		s.log.Warn("could not read issue bookkeeping", "issue", issue, "err", err)
		return
	}
	if bk.Session == nil {
		return
	}
	bk.Session = nil
	if err := s.store.SaveIssue(bk); err != nil {
		s.log.Warn("could not clear the recorded session", "issue", issue, "err", err)
	}
}
