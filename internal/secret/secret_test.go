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
	s := secret.Secret(testValue)
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
	if got := secret.Secret(testValue).Reveal(); got != testValue {
		t.Errorf("Reveal() = %q, want the value", got)
	}
	if !secret.Secret("x").IsSet() || secret.Secret("").IsSet() {
		t.Error("IsSet() is wrong for a set or an empty secret")
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
