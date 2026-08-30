package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/state"
)

// A factory operation can fail on every pass for days without anybody
// noticing: the scheduler warns and carries on, and a warning in a log tail
// nobody watches is the same as silence. Every operation that can fail that
// way reports its outcome here, so a broken one shows up in the run's summary
// stream, in status.json and in `bees status` instead of only in the log.
//
// This is bookkeeping, not policy: recording a failure never changes what the
// scheduler does next.

// degradedEscalateAfter is how many consecutive failures of one operation
// make it a person's problem. One failure is usually a transient GitHub
// error; three in a row is a breakage that will not fix itself.
const degradedEscalateAfter = 3

// opFailure is the current failure streak of one named operation. It exists
// only while the operation is failing: a success deletes it.
type opFailure struct {
	count     int
	first     time.Time
	last      time.Time
	err       string
	escalated bool
}

// op records the outcome of a named factory operation. A nil err clears the
// operation's failure streak; a non-nil err logs the warning that used to be
// logged here, extends the streak and escalates once when it gets long.
//
// It returns err != nil, so a call site can keep its own control flow:
//
//	if s.op("list-created", err, "visibility backstop: list created items", "err", err) {
//		return
//	}
func (s *Scheduler) op(name string, err error, msg string, attrs ...any) bool {
	return s.opAs(s.log, slog.LevelWarn, name, err, msg, attrs...)
}

// opAs is op for the sites that cannot use the scheduler's logger at warn
// level: a developer worker logs through a logger carrying worker/issue/branch,
// and the poll failure has always reported at error level. The escalation
// record is emitted by the scheduler's own logger either way.
func (s *Scheduler) opAs(log *slog.Logger, level slog.Level, name string, err error, msg string, attrs ...any) bool {
	if err != nil {
		log.Log(context.Background(), level, msg, append([]any{"op", name}, attrs...)...)
	}
	return s.track(name, err)
}

// track is op without the logging, for an operation whose caller reports the
// failure itself. ensureVisible is why it exists: it makes up to three
// independent GitHub mutations and joins them into one error its callers log
// as a single line naming the item, so each mutation records its own streak
// here without adding a line of its own.
func (s *Scheduler) track(name string, err error) bool {
	if err == nil {
		s.mu.Lock()
		delete(s.degraded, name)
		s.mu.Unlock()
		return false
	}
	now := s.now()
	s.mu.Lock()
	e := s.degraded[name]
	if e == nil {
		e = &opFailure{first: now}
		s.degraded[name] = e
	}
	e.count++
	e.last = now
	e.err = oneLine(err.Error(), escalationNoteLimit)
	shout := e.count >= degradedEscalateAfter && !e.escalated
	e.escalated = e.escalated || shout
	count, last := e.count, e.err
	s.mu.Unlock()

	if shout {
		// Deliberately no GitHub comment and no mail: there is no issue to
		// comment on for a factory-wide operation, and no role can fix a
		// broken credential or a missing label. The summary line and
		// `bees status` are the surfacing.
		s.log.Error(fmt.Sprintf("⚠ %s has failed %d times in a row: %s", name, count, last),
			logging.SummaryKey, true, "op", name, "failures", count, "err", last)
	}
	return true
}

// degradedLocked snapshots the failing operations for status.json, sorted by
// operation name so the file is stable between writes. The caller holds s.mu.
func (s *Scheduler) degradedLocked() []state.OpFailure {
	if len(s.degraded) == 0 {
		return nil
	}
	out := make([]state.OpFailure, 0, len(s.degraded))
	for name, e := range s.degraded {
		out = append(out, state.OpFailure{
			Op:        name,
			Count:     e.count,
			First:     e.first,
			Last:      e.last,
			LastError: e.err,
			Escalated: e.escalated,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Op < out[j].Op })
	return out
}
