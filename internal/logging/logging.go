// Package logging sets up slog so that no secret reaches a log line.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// Options configure New.
type Options struct {
	// Level is debug, info, warn or error. Empty means info.
	Level string
	// Format is text or json. Empty means text.
	Format string
}

// New returns a logger that writes to w. Every message, attribute key and
// attribute value passes through r before it is written.
func New(w io.Writer, opts Options, r *secret.Redactor) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	handlerOpts := &slog.HandlerOptions{Level: level}
	var inner slog.Handler
	switch strings.ToLower(opts.Format) {
	case "", "text":
		inner = slog.NewTextHandler(w, handlerOpts)
	case "json":
		inner = slog.NewJSONHandler(w, handlerOpts)
	default:
		return nil, fmt.Errorf("log format %q is not one of text, json", opts.Format)
	}
	return slog.New(NewHandler(inner, r)), nil
}

// ParseLevel converts debug, info, warn or error to a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log level %q is not one of debug, info, warn, error", s)
	}
}

// WithRun returns a logger that tags every record with the run ID.
func WithRun(l *slog.Logger, runID string) *slog.Logger {
	return l.With(slog.String("run_id", runID))
}

// Handler is a slog.Handler that redacts records before passing them on.
//
// Attributes and groups bound with WithAttrs and WithGroup are kept as they
// were given and redacted each time a record is written, not once when they
// are bound. A logger is often built before every secret is known (the API
// keys are read after the logger exists), and a value bound early must still
// be masked once its secret is registered.
type Handler struct {
	inner slog.Handler
	r     *secret.Redactor
	// bound lists the WithAttrs and WithGroup calls, in order.
	bound []binding
}

// binding is one WithAttrs call (attrs set) or one WithGroup call (group set).
type binding struct {
	group string
	attrs []slog.Attr
}

// NewHandler wraps inner so that every record is redacted with r first.
func NewHandler(inner slog.Handler, r *secret.Redactor) *Handler {
	return &Handler{inner: inner, r: r}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	inner := h.inner
	for _, b := range h.bound {
		if b.attrs == nil {
			inner = inner.WithGroup(h.r.Redact(b.group))
			continue
		}
		redacted := make([]slog.Attr, len(b.attrs))
		for i, a := range b.attrs {
			redacted[i] = h.attr(a)
		}
		inner = inner.WithAttrs(redacted)
	}
	out := slog.NewRecord(rec.Time, rec.Level, h.r.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return inner.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.bind(binding{attrs: slices.Clone(attrs)})
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.bind(binding{group: name})
}

func (h *Handler) bind(b binding) *Handler {
	// Clip so that two handlers derived from h never share a backing array.
	return &Handler{inner: h.inner, r: h.r, bound: append(slices.Clip(h.bound), b)}
}

func (h *Handler) attr(a slog.Attr) slog.Attr {
	return slog.Attr{Key: h.r.Redact(a.Key), Value: h.value(a.Value)}
}

// value redacts one attribute value. Numbers, booleans, times and durations
// cannot hold a secret and pass through with their type. Everything else is
// turned into the text it would print as, then redacted, so an error, a
// Stringer, a byte slice or a struct cannot carry a secret past the handler.
func (h *Handler) value(v slog.Value) slog.Value {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(h.r.Redact(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		redacted := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			redacted[i] = h.attr(a)
		}
		return slog.GroupValue(redacted...)
	case slog.KindAny:
		return slog.StringValue(h.r.Redact(anyText(v.Any())))
	default:
		return v
	}
}

func anyText(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	case error:
		return x.Error()
	case fmt.Stringer:
		return safeString(x)
	default:
		return fmt.Sprintf("%+v", x)
	}
}

// safeString calls String and survives a nil receiver that panics, the same
// way package fmt does.
func safeString(s fmt.Stringer) (out string) {
	defer func() {
		if recover() != nil {
			out = "<nil>"
		}
	}()
	return s.String()
}
