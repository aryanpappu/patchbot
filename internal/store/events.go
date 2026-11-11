package store

import "database/sql"

// eventTx accumulates notifications until the enclosing transaction commits.
type eventTx struct {
	*sql.Tx
	events []Event
}

type eventSink struct{ notify func(Event) }

// SubscribeEvents registers an in-process notification after each committed
// event write, including lifecycle transactions. Notifications may arrive out
// of order; consumers replay by sequence from SQLite. The callback must not
// block. The returned function unregisters it and is safe to call repeatedly.
func (s *SQLite) SubscribeEvents(notify func(Event)) func() {
	sink := &eventSink{notify: notify}
	s.eventMu.Lock()
	if s.eventSinks == nil {
		s.eventSinks = make(map[*eventSink]struct{})
	}
	s.eventSinks[sink] = struct{}{}
	s.eventMu.Unlock()
	return func() { s.eventMu.Lock(); delete(s.eventSinks, sink); s.eventMu.Unlock() }
}

func (s *SQLite) publishEvent(event Event) {
	s.eventMu.RLock()
	sinks := make([]*eventSink, 0, len(s.eventSinks))
	for sink := range s.eventSinks {
		sinks = append(sinks, sink)
	}
	s.eventMu.RUnlock()
	for _, sink := range sinks {
		sink.notify(event)
	}
}
