package ops

import (
	"context"
	"time"
)

// Wake is a level-triggered notification for one caller-owned reconcile loop.
// Create one per loop with NewWake. Signals coalesce until consumed; never close it.
type Wake chan struct{}

func NewWake() Wake { return make(Wake, 1) }

// Signal requests a local reconciliation without blocking the producer.
func (w Wake) Signal() {
	select {
	case w <- struct{}{}:
	default:
	}
}

// Drain consumes a pending wake before a full reconciliation, which supersedes it.
func (w Wake) Drain() {
	select {
	case <-w:
	default:
	}
}

// Wait runs local reconciliations until the scheduled tick or cancellation.
// The caller owns and stops the tick source. A wake never resets its cadence.
// A ready tick supersedes a pending wake. Signals during local reconciliation
// remain pending, since they may describe work arriving after its snapshot.
func (w Wake) Wait(ctx context.Context, tick <-chan time.Time, local func()) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		select {
		case <-tick:
			w.Drain()
			return ctx.Err() == nil
		default:
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick:
			w.Drain()
			return ctx.Err() == nil
		case <-w:
			if ctx.Err() != nil {
				return false
			}
			select {
			case <-tick:
				w.Drain()
				return true
			default:
			}
			local()
		}
	}
}

// Sleep waits for a retry delay, returning promptly on cancellation.
func Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
