package ops

import (
	"sort"
	"sync"
	"time"
)

// OpFailure is a stable snapshot of one operation's current failure streak.
type OpFailure struct {
	Op    string `json:"op"`
	Count int    `json:"count"`
	// First and Last are the ends of the streak: the failure that started
	// it and the most recent one.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	// LastError is the most recent caller-supplied error text.
	LastError string `json:"last_error,omitempty"`
	// Escalated records that this streak has already crossed its threshold.
	// The caller decides whether that signal warrants escalation.
	Escalated bool `json:"escalated,omitempty"`
}

// Degraded tracks operation failures. A zero value is ready for use.
type Degraded struct {
	mu       sync.Mutex
	failures map[string]OpFailure
}

// Record clears a streak on success; otherwise it returns the new snapshot
// and a threshold-crossing signal once per streak. The caller supplies any
// error-text normalization and decides how to surface the crossing.
func (d *Degraded) Record(name string, failed bool, message string, now time.Time, threshold int) (OpFailure, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !failed {
		delete(d.failures, name)
		return OpFailure{}, false
	}
	if d.failures == nil {
		d.failures = make(map[string]OpFailure)
	}
	e, exists := d.failures[name]
	if !exists {
		e = OpFailure{Op: name, First: now}
	}
	e.Count++
	e.Last = now
	e.LastError = message
	crossed := e.Count >= threshold && !e.Escalated
	e.Escalated = e.Escalated || crossed
	d.failures[name] = e
	return e, crossed
}

// Snapshot returns independent records sorted by name.
func (d *Degraded) Snapshot() []OpFailure {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.failures) == 0 {
		return nil
	}
	out := make([]OpFailure, 0, len(d.failures))
	for _, e := range d.failures {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Op < out[j].Op })
	return out
}
