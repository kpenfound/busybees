package ops

import (
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/work"
)

// Event describes caller-defined activity. Kind, role, stage, phase and outcome
// are opaque strings with no workflow interpretation. Publishers own their
// input; each subscriber receives its own copy of Work.Tags.
type Event struct {
	Work      work.Ref `json:"work"`
	Kind      string
	Activity  string
	Started   time.Time
	Phase     string
	Completed int
	Total     int
	Success   bool
	Time      time.Time
	Role      string
	Session   string
	Dir       string

	Stage     string
	Round     int
	Model     string
	Fallback  bool
	Sandbox   string
	Outcome   string
	Note      string
	Turns     int
	CostUSD   float64
	CostKnown bool
	Duration  time.Duration
	Err       string
}

// Bus publishes bounded, best-effort events. Slow subscribers lose new events
// without blocking producers. Channels live for the lifetime of the bus and
// are never closed. The supplied clock must be safe for concurrent calls.
type Bus struct {
	mu     sync.Mutex
	subs   []chan Event
	now    func() time.Time
	buffer int
}

func NewBus(buffer int, now func() time.Time) *Bus {
	if now == nil {
		now = time.Now
	}
	if buffer < 0 {
		buffer = 0
	}
	return &Bus{buffer: buffer, now: now}
}
func (b *Bus) Subscribe() <-chan Event {
	ch := make(chan Event, b.buffer)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch
}
func (b *Bus) Publish(ev Event) {
	ev.Time = b.now()
	b.mu.Lock()
	subs := b.subs
	b.mu.Unlock()
	for _, ch := range subs {
		copy := ev
		copy.Work = ev.Work.Clone()
		select {
		case ch <- copy:
		default:
		}
	}
}
