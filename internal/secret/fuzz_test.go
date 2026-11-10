package secret_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// FuzzRedactor checks that a registered secret never survives Redact or a
// Writer, wherever it sits in the text and wherever the stream is split.
func FuzzRedactor(f *testing.F) {
	f.Add("hunter2-hunter2", "before ", " after", uint16(3), uint16(11))
	f.Add("abababab", "ab", "abab", uint16(1), uint16(2))
	f.Add("s3cr3t/with+chars=", "Bearer ", "\n-----BEGIN PRIVATE KEY-----\n", uint16(9), uint16(40))
	f.Add("ghp_0123456789abcdefghij", "sk-", "AIza", uint16(0), uint16(200))
	f.Add("line1\nline2", strings.Repeat("x", 300), strings.Repeat("y", 300), uint16(301), uint16(305))
	f.Add("mykeyBearer", "", " "+strings.Repeat("A", 200), uint16(0), uint16(11))
	f.Add("tokenXbasic", "x", " "+strings.Repeat("Q", 200), uint16(4), uint16(150))

	f.Fuzz(func(t *testing.T, value, before, after string, cut1, cut2 uint16) {
		// The mask is public text. A secret that is part of the mask, or that
		// holds a bracket and so can be rebuilt from a mask and its
		// neighbours, cannot be hidden by any mask and is not a real secret.
		if len(value) < secret.MinValueLen || strings.ContainsAny(value, "[]") || value == "redacted" {
			t.Skip()
		}
		r := secret.NewRedactor(value)
		in := before + value + after + value

		got := r.Redact(in)
		if strings.Contains(got, value) {
			t.Fatalf("Redact(%q) = %q, which still contains %q", in, got, value)
		}
		if again := r.Redact(got); strings.Contains(again, value) {
			t.Fatalf("second Redact brought the secret back: %q", again)
		}

		a, b := int(cut1)%(len(in)+1), int(cut2)%(len(in)+1)
		if a > b {
			a, b = b, a
		}
		var out bytes.Buffer
		w := secret.NewWriter(&out, r)
		for _, part := range []string{in[:a], in[a:b], in[b:]} {
			if n, err := w.Write([]byte(part)); err != nil || n != len(part) {
				t.Fatalf("Write = %d, %v", n, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if strings.Contains(out.String(), value) {
			t.Fatalf("Writer split at %d and %d wrote %q, which still contains %q", a, b, out.String(), value)
		}
	})
}

// FuzzWriterMatchesRedact checks that streaming does not change the result:
// however the input is split, a Writer produces exactly what Redact produces
// for the whole input.
func FuzzWriterMatchesRedact(f *testing.F) {
	pad := strings.Repeat("ordinary log text ", 20)
	f.Add(pad+"Authorization: Bearer abcdefghijklmnopqrstuvwxyz"+pad, uint16(370), uint16(380))
	f.Add(pad+fakeGitHub+" sk-"+pad+fakeOpenAI, uint16(362), uint16(400))
	f.Add(pad+fakePEM+pad+"-----BEGIN PRIVATE KEY-----\nabc", uint16(380), uint16(420))
	f.Add("hunter2-hunter2-hunter2"+pad+"hunter2-hunter2-hunter", uint16(5), uint16(700))
	f.Add("bearer bearer  sk-ant-sk-ghp_github_pat_AIza"+pad, uint16(7), uint16(21))
	f.Add("hunter2-hunter2-hunter2Bearer "+pad+strings.Repeat("A", 200), uint16(3), uint16(30))
	f.Add(pad+"Authorization: Basic dXNlcjpodW50ZXIyLWh1bnRlcjI= basic "+pad, uint16(365), uint16(390))

	f.Fuzz(func(t *testing.T, in string, cut1, cut2 uint16) {
		if len(in) > 32<<10 {
			t.Skip() // above this the Writer may mask early to bound memory
		}
		// The second value ends in a pattern prefix, so a pattern match can
		// begin inside a registered value.
		r := secret.NewRedactor(knownValue, "mykeyBearer")
		want := r.Redact(in)
		a, b := int(cut1)%(len(in)+1), int(cut2)%(len(in)+1)
		if a > b {
			a, b = b, a
		}
		var out bytes.Buffer
		w := secret.NewWriter(&out, r)
		for _, part := range []string{in[:a], in[a:b], in[b:]} {
			if _, err := w.Write([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != want {
			t.Fatalf("split at %d and %d:\n  in %q\n got %q\nwant %q", a, b, in, got, want)
		}
	})
}
