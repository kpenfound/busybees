package scheduler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/state"
)

// A factory operation can fail on every pass for days without anybody
// noticing: the scheduler warns and carries on, and a warning in a log tail
// nobody watches is the same as silence. Every operation that can fail that
// way reports its outcome here, so a broken one shows up in the run's summary
// stream, in status.json and in `bees status` instead of only in the log.
//
// Core tracks the streak; this adapter decides how a crossing is surfaced.
// Recording a failure never changes what the scheduler dispatches next.

// degradedEscalateAfter is how many consecutive failures of one operation
// make it a person's problem. One failure is usually a transient GitHub
// error; three in a row is a breakage that will not fix itself.
const degradedEscalateAfter = 3

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
	message := ""
	if err != nil {
		message = oneLine(err.Error(), escalationNoteLimit)
	}
	e, shout := s.degraded.Record(name, err != nil, message, s.now(), degradedEscalateAfter)
	count, last := e.Count, e.LastError

	if shout {
		// Deliberately no GitHub comment and no mail: there is no issue to
		// comment on for a factory-wide operation, and no role can fix a
		// broken credential or a missing label. The summary line and
		// `bees status` are the surfacing.
		s.log.Error(fmt.Sprintf("⚠ %s has failed %d times in a row: %s", name, count, last),
			logging.SummaryKey, true, "op", name, "failures", count, "err", last)
	}
	return err != nil
}

// degradedLocked snapshots the failing operations for status.json, sorted by
// operation name so the file is stable between writes. The caller holds s.mu.
func (s *Scheduler) degradedLocked() []state.OpFailure { return s.degraded.Snapshot() }
