package server

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	errShuttingDown = errors.New("server is shutting down")
	errQueueFull    = errors.New("pending run queue is full; retry when capacity is available")
)

type job struct {
	id     int64
	ctx    context.Context
	cancel context.CancelFunc
	work   func(context.Context) error
}

type reservationKey struct{}

type reservation struct {
	manager *manager
	used    bool // protected by manager.mu
}

// manager owns a bounded queue without allocating goroutines for waiting work.
// Reservations secure admission before the runner commits lifecycle decisions.
type manager struct {
	mu            sync.Mutex
	running       map[int64]*job
	pending       []*job
	limit         int
	queueLimit    int
	reserved      int
	closing       bool
	done          chan struct{}
	drained       bool
	heartbeatDone chan struct{}
}

func newManager(limit, queueLimit int) *manager {
	if limit <= 0 {
		limit = 2
	}
	if queueLimit <= 0 {
		queueLimit = 16
	}
	return &manager{running: make(map[int64]*job), limit: limit, queueLimit: queueLimit, done: make(chan struct{})}
}

func (m *manager) reserve(ctx context.Context) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return ctx, nil, errShuttingDown
	}
	if len(m.pending)+m.reserved >= m.queueLimit {
		return ctx, nil, errQueueFull
	}
	m.reserved++
	r := &reservation{manager: m}
	release := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !r.used {
			r.used = true
			m.reserved--
			m.checkDrained()
		}
	}
	return context.WithValue(ctx, reservationKey{}, r), release, nil
}

func (m *manager) submit(ctx context.Context, id int64, work func(context.Context) error) error {
	r, ok := ctx.Value(reservationKey{}).(*reservation)
	if !ok || r.manager != m {
		admitted, release, err := m.reserve(ctx)
		if err != nil {
			return err
		}
		defer release()
		return m.submit(admitted, id, work)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.used {
		return errors.New("work admission already consumed")
	}
	r.used = true
	m.reserved--
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.pending = append(m.pending, &job{id: id, ctx: workCtx, cancel: cancel, work: work})
	m.schedule()
	return nil
}

// schedule runs under mu and starts only work that can execute immediately.
// Skipping a blocked run lets unrelated jobs use otherwise idle workers.
func (m *manager) schedule() {
	for i := 0; i < len(m.pending) && len(m.running) < m.limit; {
		current := m.pending[i]
		if m.running[current.id] != nil {
			i++
			continue
		}
		copy(m.pending[i:], m.pending[i+1:])
		m.pending[len(m.pending)-1] = nil
		m.pending = m.pending[:len(m.pending)-1]
		m.running[current.id] = current
		go m.run(current)
	}
	m.checkDrained()
}

func (m *manager) run(current *job) {
	defer current.cancel()
	if current.ctx.Err() == nil {
		_ = current.work(current.ctx) // The runner persists failures; raw errors never reach logs.
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, current.id)
	m.schedule()
}

func (m *manager) cancel(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.running[id]; current != nil {
		current.cancel()
	}
	kept := m.pending[:0]
	for _, current := range m.pending {
		if current.id == id {
			current.cancel()
		} else {
			kept = append(kept, current)
		}
	}
	clear(m.pending[len(kept):])
	m.pending = kept
	m.checkDrained()
}

func (m *manager) checkDrained() {
	if m.closing && !m.drained && len(m.running) == 0 && len(m.pending) == 0 && m.reserved == 0 {
		m.drained = true
		close(m.done)
	}
}

func (m *manager) stopAdmission() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closing = true
	m.checkDrained()
}

func (m *manager) wait(ctx context.Context) error {
	select {
	case <-m.done:
		if m.heartbeatDone != nil {
			select {
			case <-m.heartbeatDone:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		for _, current := range m.running {
			current.cancel()
		}
		for _, current := range m.pending {
			current.cancel()
		}
		clear(m.pending)
		m.pending = nil
		m.checkDrained()
		m.mu.Unlock()
		return ctx.Err()
	}
}

func (m *manager) busy(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[id] != nil {
		return true
	}
	for _, current := range m.pending {
		if current.id == id {
			return true
		}
	}
	return false
}

// startHeartbeats owns queued and running jobs until shutdown has drained them.
// One ticker covers the entire bounded queue, including same-run follow-ups.
func (m *manager) startHeartbeats(interval time.Duration, beat func(context.Context, int64) error) {
	m.heartbeatDone = make(chan struct{})
	go func() {
		defer close(m.heartbeatDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.done:
				return
			case <-ticker.C:
				m.mu.Lock()
				ids := make(map[int64]bool, len(m.running)+len(m.pending))
				for id := range m.running {
					ids[id] = true
				}
				for _, current := range m.pending {
					ids[current.id] = true
				}
				m.mu.Unlock()
				for id := range ids {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = beat(ctx, id) // Waiting/terminal transitions can win the heartbeat CAS.
					cancel()
				}
			}
		}
	}()
}
