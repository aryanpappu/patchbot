package secret_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
)

const knownValue = "hunter2-hunter2-hunter2"

// writeSplit sends parts through a Writer and returns everything it wrote.
func writeSplit(t *testing.T, r *secret.Redactor, parts ...string) string {
	t.Helper()
	var out bytes.Buffer
	w := secret.NewWriter(&out, r)
	for _, part := range parts {
		n, err := w.Write([]byte(part))
		if err != nil || n != len(part) {
			t.Fatalf("Write(%d bytes) = %d, %v", len(part), n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.String()
}

func TestWriterSplitAtEveryByte(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("log line with ordinary words\n", 12)
	tests := []struct {
		name string
		in   string
	}{
		{"known value", pad + "pw=" + knownValue + ";" + pad},
		{"known value at the start", knownValue + pad},
		{"known value at the end", pad + knownValue},
		{"github token", pad + "push " + fakeGitHub + " done\n" + pad},
		{"anthropic key", pad + fakeAnthropic + "\n" + pad},
		{"bearer header", pad + "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345\n" + pad},
		{"pem block", pad + fakePEM + "\n" + pad},
		{"two secrets close together", pad + knownValue + " " + fakeGoogle + " " + knownValue + pad},
		{"no secret", pad + pad},
		{"short input", "ok"},
		{"empty input", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := secret.NewRedactor(knownValue)
			want := r.Redact(tt.in)
			for i := 0; i <= len(tt.in); i++ {
				if got := writeSplit(t, r, tt.in[:i], tt.in[i:]); got != want {
					t.Fatalf("split at %d:\n got %q\nwant %q", i, got, want)
				}
			}
		})
	}
}

func TestWriterByteAtATime(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("x", 300) + knownValue + " " + fakeGitHub + "\n" + fakePEM + strings.Repeat("y", 300)
	r := secret.NewRedactor(knownValue)
	parts := make([]string, 0, len(in))
	for i := range len(in) {
		parts = append(parts, in[i:i+1])
	}
	if got, want := writeSplit(t, r, parts...), r.Redact(in); got != want {
		t.Errorf("byte at a time:\n got %q\nwant %q", got, want)
	}
}

func TestWriterHoldsBackOnlyTheTail(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	w := secret.NewWriter(&out, secret.NewRedactor(knownValue))
	in := strings.Repeat("a", 4096)
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 || out.Len() >= len(in) {
		t.Errorf("after one write, %d of %d bytes were passed on; want most but not all", out.Len(), len(in))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Error("Flush did not release the tail unchanged")
	}
}

func TestWriterBoundsMemoryOnEndlessMatch(t *testing.T) {
	t.Parallel()
	chunk := strings.Repeat("A", 8<<10)
	tests := []struct {
		name   string
		prefix string
		suffix string
		want   string
	}{
		{"endless token", "start sk-", " tail\n", "start [redacted] tail\n"},
		{"endless token at the end", "start sk-", "", "start [redacted]"},
		{"private key with no end", "start -----BEGIN PRIVATE KEY-----\n", "", "start [redacted]"},
		{
			"private key ending after the bound",
			"start -----BEGIN PRIVATE KEY-----\n",
			"\n-----END PRIVATE KEY-----\ntail\n",
			"start [redacted]\ntail\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out countingWriter
			w := secret.NewWriter(&out, nil)
			write := func(s string) {
				t.Helper()
				if _, err := w.Write([]byte(s)); err != nil {
					t.Fatal(err)
				}
			}
			write(tt.prefix)
			for range 64 { // 512 KiB, far above the 64 KiB bound
				write(chunk)
			}
			// Split the suffix so an END line also arrives in two writes.
			write(tt.suffix[:len(tt.suffix)/2])
			write(tt.suffix[len(tt.suffix)/2:])
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := out.buf.String(); got != tt.want {
				t.Errorf("output = %q, want %q", truncate(got), tt.want)
			}
		})
	}
}

func TestWriterReportsDestinationError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("disk full")
	w := secret.NewWriter(failingWriter{err: wantErr}, nil)
	_, err := w.Write([]byte(strings.Repeat("a", 1024)))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Write error = %v, want %v", err, wantErr)
	}
	if _, err := w.Write([]byte("more")); !errors.Is(err, wantErr) {
		t.Errorf("second Write error = %v, want the first error again", err)
	}
	if err := w.Flush(); !errors.Is(err, wantErr) {
		t.Errorf("Flush error = %v, want the first error again", err)
	}
}

func TestWriterCloseClosesDestination(t *testing.T) {
	t.Parallel()
	dst := &closeRecorder{}
	w := secret.NewWriter(dst, nil)
	if _, err := w.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !dst.closed || dst.String() != "tail" {
		t.Errorf("closed = %v, output = %q; want closed with the tail written", dst.closed, dst.String())
	}
}

type countingWriter struct{ buf bytes.Buffer }

func (c *countingWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

type closeRecorder struct {
	bytes.Buffer
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:100] + "..." + s[len(s)-100:]
	}
	return s
}
