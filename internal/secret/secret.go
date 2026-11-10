// Package secret holds secret values and removes them from text before it is
// logged, stored or sent anywhere.
package secret

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
)

// Mask is the text that replaces a secret.
const Mask = "[redacted]"

// Secret is a sensitive string such as an API key. Every formatting, logging
// and JSON path prints Mask. The value is available only through Reveal, so
// each use is easy to find with grep.
//
// The value sits behind a pointer on purpose. Package fmt cannot call methods
// on an unexported struct field, so it prints such a field by walking its
// memory; with a pointer it finds an address there, never the value. A Secret
// is therefore safe inside any struct, exported field or not.
//
// The zero value is an unset secret. Compare secrets with Equal, not ==.
type Secret struct {
	v *string
}

// New returns a Secret holding value. An empty value gives the unset Secret.
func New(value string) Secret {
	if value == "" {
		return Secret{}
	}
	return Secret{v: &value}
}

// Reveal returns the secret value.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

// IsSet reports whether the secret has a value.
func (s Secret) IsSet() bool { return s.v != nil && *s.v != "" }

// Equal reports whether two secrets hold the same value, in constant time for
// values of equal length.
func (s Secret) Equal(o Secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.Reveal()), []byte(o.Reveal())) == 1
}

// String implements fmt.Stringer and never returns the value.
func (Secret) String() string { return Mask }

// GoString implements fmt.GoStringer, which covers the %#v verb.
func (Secret) GoString() string { return Mask }

// Format implements fmt.Formatter, so no verb or flag can print the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, Mask) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(Mask) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Mask + `"`), nil }

// MarshalText implements encoding.TextMarshaler, which covers TOML, XML and
// JSON map keys.
func (Secret) MarshalText() ([]byte, error) { return []byte(Mask), nil }
