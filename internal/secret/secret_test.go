package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
)

const testValue = "s3cr3t-value-0123456789"

func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()
	s := secret.New(testValue)
	type holder struct {
		Key secret.Secret
		Ptr *secret.Secret
	}
	h := holder{Key: s, Ptr: &s}

	tests := []struct {
		name string
		got  func() string
	}{
		{"String", s.String},
		{"GoString", s.GoString},
		{"%s", func() string { return fmt.Sprintf("%s", s) }},
		{"%v", func() string { return fmt.Sprintf("%v", s) }},
		{"%+v", func() string { return fmt.Sprintf("%+v", s) }},
		{"%#v", func() string { return fmt.Sprintf("%#v", s) }},
		{"%q", func() string { return fmt.Sprintf("%q", s) }},
		{"%x", func() string { return fmt.Sprintf("%x", s) }},
		{"%d", func() string { return fmt.Sprintf("%d", s) }},
		{"pointer %v", func() string { return fmt.Sprintf("%v", &s) }},
		{"struct %+v", func() string { return fmt.Sprintf("%+v", h) }},
		{"struct %#v", func() string { return fmt.Sprintf("%#v", h) }},
		{"Sprint", func() string { return fmt.Sprint(s) }},
		{"json", func() string { return mustJSON(t, s) }},
		{"json struct", func() string { return mustJSON(t, h) }},
		{"json map value", func() string { return mustJSON(t, map[string]secret.Secret{"k": s}) }},
		{"json map key", func() string { return mustJSON(t, map[secret.Secret]int{s: 1}) }},
		{"slice %v", func() string { return fmt.Sprintf("%v", []secret.Secret{s}) }},
		{"slog text", func() string { return logWith(slog.NewTextHandler, s) }},
		{"slog json", func() string { return logWith(slog.NewJSONHandler, s) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.got()
			if strings.Contains(got, testValue) || strings.Contains(got, "s3cr3t") {
				t.Fatalf("output leaks the secret: %q", got)
			}
			if !strings.Contains(got, secret.Mask) {
				t.Errorf("output = %q, want it to contain %q", got, secret.Mask)
			}
		})
	}
}

func TestSecretRevealAndIsSet(t *testing.T) {
	t.Parallel()
	if got := secret.New(testValue).Reveal(); got != testValue {
		t.Errorf("Reveal() = %q, want the value", got)
	}
	if !secret.New("x").IsSet() || secret.New("").IsSet() || (secret.Secret{}).IsSet() {
		t.Error("IsSet() is wrong for a set or an empty secret")
	}
	if (secret.Secret{}).Reveal() != "" {
		t.Error("the zero Secret reveals a value")
	}
	if !secret.New("a").Equal(secret.New("a")) || secret.New("a").Equal(secret.New("b")) ||
		!secret.New("").Equal(secret.Secret{}) || secret.New("a").Equal(secret.Secret{}) {
		t.Error("Equal is wrong")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func logWith[H slog.Handler](newHandler func(io.Writer, *slog.HandlerOptions) H, s secret.Secret) string {
	var buf bytes.Buffer
	slog.New(newHandler(&buf, nil)).Info("msg", "key", s, slog.Group("g", slog.Any("inner", s)))
	return buf.String()
}

// private mimics a struct in another package that keeps a key in an
// unexported field, such as an HTTP client. Package fmt cannot call methods
// on unexported fields and prints them by reflection instead.
type private struct {
	name string
	key  secret.Secret
	ptr  *secret.Secret
	list []secret.Secret
	m    map[string]secret.Secret
}

type outer struct {
	Public  string
	private private
	iface   any
}

func TestSecretInUnexportedFields(t *testing.T) {
	t.Parallel()
	s := secret.New(testValue)
	p := private{name: "client", key: s, ptr: &s, list: []secret.Secret{s}, m: map[string]secret.Secret{"k": s}}
	o := outer{Public: "x", private: p, iface: p}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		for name, value := range map[string]any{"struct": p, "pointer": &p, "nested": o, "nested pointer": &o, "slice": []private{p}, "map": map[string]outer{"k": o}} {
			if got := fmt.Sprintf(verb, value); leaks(got) {
				t.Errorf("%s of a %s leaks the secret: %s", verb, name, got)
			}
		}
	}
	for name, value := range map[string]any{"struct": p, "pointer": &p, "nested": o, "nested pointer": &o} {
		if got := mustJSON(t, value); leaks(got) {
			t.Errorf("JSON of a %s leaks the secret: %s", name, got)
		}
		if got := fmt.Sprint(value) + fmt.Sprintln(value); leaks(got) {
			t.Errorf("Sprint of a %s leaks the secret: %s", name, got)
		}
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "v", value)
		slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "v", value)
		if leaks(buf.String()) {
			t.Errorf("slog of a %s leaks the secret: %s", name, buf.String())
		}
	}
}

// leaks reports whether text holds the test value as text or as hex, which
// is how %x prints a string.
func leaks(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(text, testValue) || strings.Contains(text, "s3cr3t") ||
		strings.Contains(lower, fmt.Sprintf("%x", testValue)) || strings.Contains(lower, fmt.Sprintf("%x", "s3cr3t"))
}
