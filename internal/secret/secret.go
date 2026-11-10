// Package secret holds secret values and removes them from text before it is
// logged, stored or sent anywhere.
package secret

import (
	"fmt"
	"log/slog"
)

// Mask is the text that replaces a secret.
const Mask = "[redacted]"

// Secret is a sensitive string such as an API key. Every formatting, logging
// and JSON path prints Mask. The value is available only through Reveal, so
// each use is easy to find with grep.
//
// Two paths cannot be covered by a string type: a conversion with string(s),
// and use as a JSON map key, where encoding/json reads string keys directly.
// Do neither.
type Secret string

// Reveal returns the secret value.
func (s Secret) Reveal() string { return string(s) }

// IsSet reports whether the secret has a value.
func (s Secret) IsSet() bool { return s != "" }

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

// MarshalText implements encoding.TextMarshaler, which covers TOML and XML.
func (Secret) MarshalText() ([]byte, error) { return []byte(Mask), nil }
