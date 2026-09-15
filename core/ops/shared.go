package ops

import "sync"

// SharedPool bounds concurrent work across caller-owned loops. Refused members
// queue FIFO, with all-or-none claims. A served member rejoins at the tail if
// it needs more, so repeated passes share slots round-robin. End each dispatch
// pass with Pass and call Leave when a loop stops; in-flight claims are still
// released normally. Claims must be between one and Size slots.
type SharedPool struct {
	size int

	mu    sync.Mutex
	inUse int
	// queue is the schedulers waiting for slots, first to be served first.
	queue []*Member
}

// NewSharedPool makes a pool of size slots. A size below 1 is one slot.
func NewSharedPool(size int) *SharedPool {
	if size < 1 {
		size = 1
	}
	return &SharedPool{size: size}
}

// Size is how many slots the pool has.
func (p *SharedPool) Size() int { return p.size }

// InUse is how many of them are taken right now.
func (p *SharedPool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inUse
}

// Waiting names the schedulers queued for a slot, in the order they are
// served.
func (p *SharedPool) Waiting() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.queue))
	for _, m := range p.queue {
		names = append(names, m.name)
	}
	return names
}

// Member is one scheduler's membership of a SharedPool.
type Member struct {
	pool *SharedPool
	name string
	// wake asks the scheduler for a local pass: it is the head of the queue
	// and the pool can serve it.
	wake func()
	// want is how many slots the last refused claim asked for.
	want int
	// refused says a claim was refused since the last pass ended.
	refused bool
}

// Join makes a member called name. wake must be non-blocking and must not
// call back into the pool; it runs under the pool lock.
func (p *SharedPool) Join(name string, wake func()) *Member {
	return &Member{pool: p, name: name, wake: wake}
}

// Acquire takes n slots without waiting, all of them or none. Refused, the
// member is queued, if it is not already, and woken when its turn comes.
func (m *Member) Acquire(n int) bool {
	p := m.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if head := len(p.queue) > 0 && p.queue[0] == m; head || len(p.queue) == 0 {
		if p.inUse+n <= p.size {
			p.inUse += n
			if head {
				p.queue = p.queue[1:]
				p.wakeHead()
			}
			return true
		}
	}
	m.refused, m.want = true, n
	if m.position() < 0 {
		p.queue = append(p.queue, m)
	}
	return false
}

// Release gives n slots back and wakes the queue's head when that serves it.
func (m *Member) Release(n int) {
	p := m.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inUse -= n
	p.wakeHead()
}

// Pass tells the pool the scheduler's dispatch pass ended. One in which
// nothing was refused leaves the queue: the scheduler has nothing waiting
// for a slot, and the turn passes on.
func (m *Member) Pass() {
	p := m.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	refused := m.refused
	m.refused = false
	if refused {
		return
	}
	if i := m.position(); i >= 0 {
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		if i == 0 {
			p.wakeHead()
		}
	}
}

// Leave drops a stopped scheduler's pending claim without releasing the
// slots its in-flight workers still own. Their normal releases handle those.
func (m *Member) Leave() {
	if m == nil {
		return
	}
	p := m.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	m.refused = false
	if i := m.position(); i >= 0 {
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		if i == 0 {
			p.wakeHead()
		}
	}
}

// position is the member's place in the queue, -1 when it is not queued.
// Called with the pool's lock held.
func (m *Member) position() int {
	for i, q := range m.pool.queue {
		if q == m {
			return i
		}
	}
	return -1
}

// wakeHead wakes the queue's head when the pool can serve what it asked for.
// Called with the pool's lock held; the wake itself never blocks (signal).
func (p *SharedPool) wakeHead() {
	if len(p.queue) == 0 {
		return
	}
	if head := p.queue[0]; p.inUse+head.want <= p.size {
		head.wake()
	}
}

// Pool returns the pool this member belongs to.
func (m *Member) Pool() *SharedPool { return m.pool }
