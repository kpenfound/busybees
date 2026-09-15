package scheduler

import "sync"

// SharedPool bounds what several schedulers in one process run at once: the
// machine config's max_developers, the total of developer slots in use
// across every project a daemon manages. Each scheduler keeps its own pool
// of scheduler.max_developers slots and, joined to a shared pool, takes one
// of these on top of each of its own, for the same things: a developer
// worker, every attempt of a fan-out, a requested review. A single-project
// run has none.
//
// Fairness. A scheduler that is refused joins a queue, and from then on the
// pool serves the queue in order: while it is not empty only the scheduler
// at its head may take slots, whatever is free, and a scheduler the head
// refused too, or one that never waited, queues up behind. A scheduler
// leaves the queue when its claim is granted, or when a dispatch pass of
// its ends without a refusal, or when it stops polling (nothing will claim
// its pending turn). A pass can have nothing left to dispatch: its ready
// issue closed or its budget ran out, so a turn nobody uses is not kept.
// Whenever the head could be served the pool wakes it, and the local pass
// that follows makes the claim. A scheduler that got its turn and wants
// more queues up again at the back, which is what hands a freed slot round
// the projects waiting for one rather than back to the one that just gave
// it up: a busy project cannot starve the others.
//
// A claim is all or none, as it is against a scheduler's own pool, so a
// fan-out is clamped to the shared pool's size as it is to max_developers
// (attemptsFor); a claim the pool could never fill would hold the head of
// the queue for good.
type SharedPool struct {
	size int

	mu    sync.Mutex
	inUse int
	// queue is the schedulers waiting for slots, first to be served first.
	queue []*sharedMember
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

// sharedMember is one scheduler's membership of a SharedPool.
type sharedMember struct {
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

// join makes a member for the scheduler called name, woken with wake.
func (p *SharedPool) join(name string, wake func()) *sharedMember {
	return &sharedMember{pool: p, name: name, wake: wake}
}

// acquire takes n slots without waiting, all of them or none. Refused, the
// member is queued, if it is not already, and woken when its turn comes.
func (m *sharedMember) acquire(n int) bool {
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

// release gives n slots back and wakes the queue's head when that serves it.
func (m *sharedMember) release(n int) {
	p := m.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inUse -= n
	p.wakeHead()
}

// pass tells the pool the scheduler's dispatch pass ended. One in which
// nothing was refused leaves the queue: the scheduler has nothing waiting
// for a slot, and the turn passes on.
func (m *sharedMember) pass() {
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

// leave drops a stopped scheduler's pending claim without releasing the
// slots its in-flight workers still own. Their normal releases handle those.
func (m *sharedMember) leave() {
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
func (m *sharedMember) position() int {
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
