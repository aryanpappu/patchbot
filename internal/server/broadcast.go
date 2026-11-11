package server

import "sync"

// Notifications are hints only. Every stream reads ordered events from SQLite,
// so dropped or reordered notifications cannot introduce replay gaps.
type subscription struct {
	wake   chan struct{}
	lagged chan struct{}
}

type broadcaster struct {
	mu      sync.Mutex
	clients map[int64]map[*subscription]struct{}
}

func (b *broadcaster) subscribe(id int64) (*subscription, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.clients == nil {
		b.clients = make(map[int64]map[*subscription]struct{})
	}
	if b.clients[id] == nil {
		b.clients[id] = make(map[*subscription]struct{})
	}
	sub := &subscription{wake: make(chan struct{}, 32), lagged: make(chan struct{})}
	b.clients[id][sub] = struct{}{}
	return sub, func() {
		b.mu.Lock()
		delete(b.clients[id], sub)
		if len(b.clients[id]) == 0 {
			delete(b.clients, id)
		}
		b.mu.Unlock()
	}
}

func (b *broadcaster) publish(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.clients[id] {
		select {
		case sub.wake <- struct{}{}:
		default:
			close(sub.lagged)
			delete(b.clients[id], sub)
		}
	}
}
