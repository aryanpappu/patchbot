package execx

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func assertCut(t *testing.T, output string, total int64, capBytes int) {
	t.Helper()
	start := strings.Index(output, "\n[... ")
	if start < 0 {
		t.Fatal("missing omission marker")
	}
	end := strings.Index(output[start+1:], "\n") + start + 2
	if end <= start+1 {
		t.Fatal("unterminated omission marker")
	}
	marker := output[start:end]
	var omitted int64
	if _, err := fmt.Sscanf(marker, "\n[... %d bytes omitted ...]\n", &omitted); err != nil {
		t.Fatal(err)
	}
	if want := total - int64(len(output)-len(marker)); omitted != want {
		t.Fatalf("omitted %d, want %d", omitted, want)
	}
	if len(output) > capBytes {
		t.Fatalf("output length %d > cap %d", len(output), capBytes)
	}
}

func TestCapWriterHeadTail(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{MinOutputCap, 127, 1024, DefaultOutputCap} {
		for _, size := range []int{0, 1, limit / 4, limit - 1, limit, limit + 1, limit * 8} {
			for _, chunk := range []int{1, 31, limit * 9} {
				w := newCapWriter(limit)
				data := make([]byte, size)
				for i := range data {
					data[i] = byte('A' + i%26)
				}
				for offset := 0; offset < size; offset += chunk {
					part := data[offset:min(size, offset+chunk)]
					if n, err := w.Write(part); err != nil || n != len(part) {
						t.Fatalf("Write = %d, %v", n, err)
					}
				}
				output, total, truncated := w.snapshot()
				if total != int64(size) || truncated != (size > limit) {
					t.Fatalf("size %d, cap %d: total %d, truncated %v", size, limit, total, truncated)
				}
				if !truncated {
					if output != string(data) {
						t.Fatal("uncut output differs")
					}
					continue
				}
				assertCut(t, output, total, limit)
				if !strings.HasPrefix(output, string(data[:limit/4])) ||
					!strings.HasSuffix(output, string(data[size-8:])) {
					t.Fatal("lost head or tail")
				}
			}
		}
	}
}

func TestCapWriterMemoryAndConcurrentWrites(t *testing.T) {
	w := newCapWriter(DefaultOutputCap)
	chunk := bytes.Repeat([]byte("x"), 8192)
	// Allocation count and backing capacity are deterministic even under -race;
	// a whole-process heap delta would also count the test runner and GC activity.
	allocs := testing.AllocsPerRun(20, func() { _, _ = w.Write(chunk) })
	if allocs != 0 || cap(w.head)+cap(w.tail) != DefaultOutputCap {
		t.Fatalf("Write allocations %v; backing capacity %d", allocs, cap(w.head)+cap(w.tail))
	}
	w = newCapWriter(DefaultOutputCap)
	var writers sync.WaitGroup
	for range 8 {
		writers.Go(func() {
			for range 160 {
				_, _ = w.Write(chunk)
				_, _, _ = w.snapshot()
			}
		})
	}
	writers.Wait()
	output, total, truncated := w.snapshot()
	if total != 10<<20 || !truncated {
		t.Fatalf("total %d, truncated %v", total, truncated)
	}
	assertCut(t, output, total, DefaultOutputCap)
}
