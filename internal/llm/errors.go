package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// ErrorKind classifies a provider failure.
type ErrorKind string

// The error kinds.
const (
	ErrAuth           ErrorKind = "auth"             // 401, 403: bad or missing key
	ErrRateLimit      ErrorKind = "rate_limit"       // 429
	ErrOverloaded     ErrorKind = "overloaded"       // the service is up but shedding load: 503, 529
	ErrUnavailable    ErrorKind = "unavailable"      // other 5xx, network failure, timeout, EOF
	ErrContextTooLong ErrorKind = "context_too_long" // the prompt exceeds the model's window
	ErrBadRequest     ErrorKind = "bad_request"      // other 4xx: our request is wrong
	ErrMalformed      ErrorKind = "malformed"        // a response we cannot use
	ErrRefusal        ErrorKind = "refusal"          // the provider refused the request outright
)

// Error is a failure of a provider call. Adapters return it for every
// failure except context cancellation.
//
// Everything an Error prints or serializes is safe to log and to show to the
// user. The underlying cause is not: a transport error can quote the request
// URL, and some APIs take the key in the query string. So the cause is kept
// for errors.Is and errors.As only, and never appears in Error(), in JSON or
// in any fmt output.
type Error struct {
	Kind ErrorKind
	// Provider is the instance name.
	Provider string
	// Status is the HTTP status, or 0 when there was none.
	Status int
	// RetryAfter is how long the provider asked us to wait; 0 when it did not.
	RetryAfter time.Duration
	// Message describes the failure. Adapters redact it, and it passes the
	// token-pattern redactor again whenever it is printed.
	Message   string
	RequestID string

	// cause is behind a pointer so that fmt, which prints unexported fields
	// by walking memory, finds an address and not the error's text.
	cause *cause
}

type cause struct{ err error }

// WithCause records the underlying error and returns e. The cause is
// reachable through Unwrap, errors.Is and errors.As, and is never printed.
func (e *Error) WithCause(err error) *Error {
	e.cause = nil
	if err != nil {
		e.cause = &cause{err: err}
	}
	return e
}

// Error implements error. It never includes the cause's text; when there is
// no message it names only the class of the cause, such as "timed out".
func (e *Error) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(": ")
	}
	b.WriteString(string(e.Kind))
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	if detail := e.detail(); detail != "" {
		b.WriteString(": ")
		b.WriteString(detail)
	}
	return b.String()
}

// detail returns the sanitized description: the redacted message, or the
// class of the cause when there is no message.
func (e *Error) detail() string {
	if e.Message != "" {
		return (*secret.Redactor)(nil).Redact(e.Message)
	}
	return causeClass(e.Unwrap())
}

// causeClass describes an underlying error without quoting it.
func causeClass(err error) string {
	if err == nil {
		return ""
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return "timed out"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed early"
	default:
		return "network or transport failure"
	}
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error {
	if e.cause == nil {
		return nil
	}
	return e.cause.err
}

// MarshalJSON implements json.Marshaler with the sanitized fields only.
func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind         ErrorKind `json:"kind"`
		Provider     string    `json:"provider,omitempty"`
		Status       int       `json:"status,omitempty"`
		RetryAfterMS int64     `json:"retry_after_ms,omitempty"`
		Message      string    `json:"message,omitempty"`
		RequestID    string    `json:"request_id,omitempty"`
	}{e.Kind, e.Provider, e.Status, e.RetryAfter.Milliseconds(), e.detail(), e.RequestID})
}

// Retryable reports whether the same request may succeed if sent again.
// Only rate limits, overload and unavailability qualify.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case ErrRateLimit, ErrOverloaded, ErrUnavailable:
		return true
	default:
		return false
	}
}

// Is lets errors.Is match on Kind: errors.Is(err, &Error{Kind: ErrAuth}).
// Fields left zero in the target are not compared.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return (t.Kind == "" || t.Kind == e.Kind) &&
		(t.Provider == "" || t.Provider == e.Provider) &&
		(t.Status == 0 || t.Status == e.Status)
}

// KindOf returns the kind of the first *Error in err's chain.
func KindOf(err error) (ErrorKind, bool) {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind, true
	}
	return "", false
}

// IsRetryable reports whether err is an *Error that may be retried.
func IsRetryable(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Retryable()
}

// KindFromStatus maps an HTTP error status to a kind. Adapters refine the
// result when the body says more, for example context_too_long on a 400.
func KindFromStatus(status int) ErrorKind {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusTooManyRequests:
		return ErrRateLimit
	case status == http.StatusServiceUnavailable, status == 529:
		return ErrOverloaded
	case status == http.StatusRequestTimeout, status >= 500:
		return ErrUnavailable
	case status == http.StatusRequestEntityTooLarge:
		return ErrContextTooLong
	default:
		return ErrBadRequest
	}
}

// ParseRetryAfter reads a Retry-After header: a number of seconds or an HTTP
// date. It returns 0 when the header is missing, malformed or in the past.
func ParseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(header, 64); err == nil {
		if seconds <= 0 || seconds > 1e9 {
			return 0
		}
		return time.Duration(seconds * float64(time.Second))
	}
	if at, err := http.ParseTime(header); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// SafeText describes err for a log line, a warning or the UI. Text is taken
// only from errors this package produced, whose text is sanitized; any other
// error in the chain is named by its class, never quoted. The result also
// passes the token-pattern redactor.
func SafeText(err error) string {
	if err == nil {
		return ""
	}
	var text string
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		text = causeClass(err)
	case errors.Is(err, ErrNoAdapter):
		text = ErrNoAdapter.Error()
	case errors.Is(err, ErrUnknownProvider):
		text = ErrUnknownProvider.Error()
	case errors.Is(err, errEmptyModelList):
		text = errEmptyModelList.Error()
	default:
		if e, ok := errors.AsType[*Error](err); ok {
			text = e.Error()
		} else {
			text = "the provider could not be reached or set up"
		}
	}
	return (*secret.Redactor)(nil).Redact(text)
}
