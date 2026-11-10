package llm

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
type Error struct {
	Kind ErrorKind
	// Provider is the instance name.
	Provider string
	// Status is the HTTP status, or 0 when there was none.
	Status int
	// RetryAfter is how long the provider asked us to wait; 0 when it did not.
	RetryAfter time.Duration
	// Message is safe to log and show: the adapter redacts it.
	Message   string
	RequestID string
	// Err is the underlying error, if any.
	Err error
}

// Error implements error.
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
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil && e.Message == "" {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

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
