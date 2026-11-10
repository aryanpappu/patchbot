package secret

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// maxPending bounds the tail held for a match that never ends, such as a
// private-key header followed by megabytes of output. Registered values may
// require a larger overlap to recognize secrets split across writes.
const maxPending = 64 << 10

// pemEndMax is longer than any END line pemEnd can match.
const pemEndMax = 64

type swallow int

const (
	swallowNone  swallow = iota
	swallowToken         // drop bytes until the current token ends
	swallowPEM           // drop bytes until the END line of a private key
)

// Writer redacts a stream. It holds back the tail of what it has seen, so a
// secret split across two Write calls is still masked. Call Flush or Close at
// the end of the stream to release the tail. A Writer is safe for concurrent
// use, which lets one Writer take both stdout and stderr of a command.
type Writer struct {
	mu   sync.Mutex
	dst  io.Writer
	r    *Redactor
	buf  []byte
	mode swallow
	err  error
}

// NewWriter returns a Writer that sends redacted output to dst.
func NewWriter(dst io.Writer, r *Redactor) *Writer {
	return &Writer{dst: dst, r: r}
}

// Write implements io.Writer. It reports len(p) even when part of p is held
// back, because the held bytes are written by a later Write or Flush.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.buf = append(w.buf, p...)
	if err := w.drain(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes everything held back. Use it only at the end of the stream or
// at a point where no secret can be half written, such as the end of a line.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	return w.drain(true)
}

// Close flushes the Writer and closes the destination if it is an io.Closer.
func (w *Writer) Close() error {
	err := w.Flush()
	if c, ok := w.dst.(io.Closer); ok {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

func (w *Writer) drain(final bool) error {
	var out bytes.Buffer
	w.process(&out, final)
	if out.Len() == 0 {
		return nil
	}
	if _, err := w.dst.Write(out.Bytes()); err != nil {
		w.err = err
		w.buf = nil
		return err
	}
	return nil
}

// process moves the safe prefix of w.buf, redacted, into out.
func (w *Writer) process(out *bytes.Buffer, final bool) {
	if !w.finishSwallow(final) {
		return
	}
	s := string(w.buf)
	spans := w.r.find(s)
	if final {
		writeMasked(out, s, spans, 0, len(s))
		w.buf = nil
		return
	}

	// Everything before cut is safe to write: no secret that starts there can
	// still be growing. A match that crosses cut moves cut back to its start.
	overlap := w.r.overlap()
	cut := safeCut(spans, len(s)-overlap)
	if cut < 0 {
		return
	}
	writeMasked(out, s, spans, 0, cut)
	if len(s)-cut <= max(maxPending, overlap) {
		w.buf = w.buf[cut:]
		return
	}
	// The held text has grown past the memory bound, which takes a match
	// that never ends. Every span but the last is complete, so write up to
	// the last one, mask it now and remember to drop the rest of it as it
	// arrives.
	last := spans[len(spans)-1]
	writeMasked(out, s, spans, cut, last.start)
	out.WriteString(Mask)
	rest := w.buf[last.end:]
	switch {
	case last.open:
		w.mode = swallowPEM
		rest = w.buf[max(0, len(w.buf)-pemEndMax):]
	case last.token && len(rest) == 0:
		w.mode = swallowToken
	}
	w.buf = append(w.buf[:0], rest...)
}

// finishSwallow drops the remainder of an oversized match. It reports whether
// normal processing can continue.
func (w *Writer) finishSwallow(final bool) bool {
	switch w.mode {
	case swallowToken:
		n := 0
		for n < len(w.buf) && isTokenChar(w.buf[n]) {
			n++
		}
		w.buf = w.buf[n:]
		if len(w.buf) == 0 && !final {
			return false
		}
	case swallowPEM:
		loc := pemEnd.FindIndex(w.buf)
		if loc == nil {
			if final {
				w.buf = nil
			} else {
				w.buf = append(w.buf[:0], w.buf[max(0, len(w.buf)-pemEndMax):]...)
				return false
			}
		} else {
			w.buf = w.buf[loc[1]:]
		}
	case swallowNone:
		return true
	}
	w.mode = swallowNone
	return true
}

// safeCut returns the largest cut at or below limit that no match crosses,
// so that everything before it is final. It returns -1 when limit is not
// positive.
//
// Moving the cut back to the start of one match can put it inside an earlier
// one: in "mykeyBearer <token>" the Bearer match begins inside the registered
// value "mykeyBearer". So the spans are rechecked until none crosses.
func safeCut(spans []span, limit int) int {
	if limit <= 0 {
		return -1
	}
	cut := limit
	for moved := true; moved; {
		moved = false
		for _, sp := range spans {
			if sp.from < cut && sp.end > cut {
				cut = sp.from
				moved = true
			}
		}
	}
	return cut
}

// writeMasked writes s[from:limit] with every span inside that range replaced
// by Mask. No span may cross from or limit.
func writeMasked(out *bytes.Buffer, s string, spans []span, from, limit int) {
	last := from
	for _, sp := range spans {
		if sp.start < from {
			continue
		}
		if sp.end > limit {
			break
		}
		out.WriteString(s[last:sp.start])
		out.WriteString(Mask)
		last = sp.end
	}
	out.WriteString(s[last:limit])
}

func isTokenChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte("._~+/=-", b) >= 0
}
