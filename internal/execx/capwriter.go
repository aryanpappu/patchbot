package execx

import (
	"fmt"
	"sync"
)

// capWriter retains a fixed head and a ring of trailing bytes. Write never
// allocates proportional to its argument and always accepts the entire write.
type capWriter struct {
	mu      sync.Mutex
	limit   int
	head    []byte
	tail    []byte
	tailLen int
	next    int
	total   int64
}

func newCapWriter(limit int) *capWriter {
	head := limit / 4
	return &capWriter{limit: limit, head: make([]byte, 0, head), tail: make([]byte, limit-head)}
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)
	if room := cap(w.head) - len(w.head); room > 0 {
		take := min(room, len(p))
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) >= len(w.tail) {
		copy(w.tail, p[len(p)-len(w.tail):])
		w.next, w.tailLen = 0, len(w.tail)
	} else if len(p) > 0 {
		first := copy(w.tail[w.next:], p)
		copy(w.tail, p[first:])
		w.next = (w.next + len(p)) % len(w.tail)
		w.tailLen = min(len(w.tail), w.tailLen+len(p))
	}
	return n, nil
}

func (w *capWriter) snapshot() (output string, total int64, truncated bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	truncated = w.total > int64(w.limit)
	keep := len(w.head) + w.tailLen
	marker := ""
	if truncated {
		// The marker consumes the same budget as retained bytes. Iterate until
		// its length agrees with the decimal omitted count (including digits).
		keep = w.limit
		for {
			marker = fmt.Sprintf("\n[... %d bytes omitted ...]\n", w.total-int64(keep))
			next := w.limit - len(marker)
			if next == keep {
				break
			}
			keep = next
		}
	}
	headLen := min(len(w.head), keep)
	tailLen := keep - headLen
	buf := make([]byte, 0, w.limit)
	buf = append(buf, w.head[:headLen]...)
	buf = append(buf, marker...)
	if tailLen > 0 {
		start := (w.next - tailLen + len(w.tail)) % len(w.tail)
		first := min(tailLen, len(w.tail)-start)
		buf = append(buf, w.tail[start:start+first]...)
		buf = append(buf, w.tail[:tailLen-first]...)
	}
	return string(buf), w.total, truncated
}
