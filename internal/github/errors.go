package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxErrorBody = 1024

// APIError is a bounded, credential-redacted GitHub error response.
// Use errors.As to inspect this or one of the specific status error types.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub HTTP %d: %s", e.StatusCode, e.Message)
}

// ErrNotFound indicates HTTP 404.
type ErrNotFound struct{ APIError }

// Unwrap exposes the common API error metadata.
func (e *ErrNotFound) Unwrap() error { return &e.APIError }

// ErrUnauthorized indicates HTTP 401.
type ErrUnauthorized struct{ APIError }

// Unwrap exposes the common API error metadata.
func (e *ErrUnauthorized) Unwrap() error { return &e.APIError }

// ErrForbidden indicates HTTP 403 without evidence of a rate limit.
type ErrForbidden struct{ APIError }

// Unwrap exposes the common API error metadata.
func (e *ErrForbidden) Unwrap() error { return &e.APIError }

// ErrRateLimited indicates a primary or secondary rate limit. Reset is zero
// when GitHub provides no valid reset or retry header; the caller chooses backoff.
type ErrRateLimited struct {
	APIError
	Reset time.Time
}

// Unwrap exposes the common API error metadata.
func (e *ErrRateLimited) Unwrap() error { return &e.APIError }

// ErrValidation indicates HTTP 422, including GitHub's validation message.
type ErrValidation struct{ APIError }

// Unwrap exposes the common API error metadata.
func (e *ErrValidation) Unwrap() error { return &e.APIError }

func responseError(status int, header http.Header, body []byte, token string) error {
	message := string(body)
	var wire struct{ Message string }
	if json.Unmarshal(body, &wire) == nil && wire.Message != "" {
		message = wire.Message
	}
	limited := strings.ToLower(message)
	api := APIError{StatusCode: status, Message: redactMessage(message, token)}
	switch {
	case status == http.StatusTooManyRequests || (status == http.StatusForbidden &&
		(header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != "" ||
			strings.Contains(limited, "rate limit") || strings.Contains(limited, "abuse detection"))):
		return &ErrRateLimited{APIError: api, Reset: rateReset(header, time.Now())}
	case status == http.StatusNotFound:
		return &ErrNotFound{api}
	case status == http.StatusUnauthorized:
		return &ErrUnauthorized{api}
	case status == http.StatusForbidden:
		return &ErrForbidden{api}
	case status == http.StatusUnprocessableEntity:
		return &ErrValidation{api}
	default:
		return &api
	}
}

func rateReset(header http.Header, now time.Time) time.Time {
	var reset time.Time
	if seconds, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil && seconds >= 0 {
		reset = time.Unix(seconds, 0).UTC()
	}
	retry := header.Get("Retry-After")
	// Bound the conversion to time.Duration so a hostile header cannot overflow.
	if seconds, err := strconv.ParseInt(retry, 10, 64); err == nil && seconds >= 0 && seconds <= 1<<32 {
		if after := now.Add(time.Duration(seconds) * time.Second); after.After(reset) {
			reset = after
		}
	} else if after, err := http.ParseTime(retry); err == nil && after.After(reset) {
		reset = after
	}
	return reset
}

func tokenForms(token string) []string {
	if token == "" {
		return nil
	}
	return []string{token, url.QueryEscape(token), url.PathEscape(token),
		base64.StdEncoding.EncodeToString([]byte(token)), base64.RawStdEncoding.EncodeToString([]byte(token))}
}

func redactMessage(message, token string) string {
	for _, secret := range tokenForms(token) {
		message = strings.ReplaceAll(message, secret, "[redacted]")
		// A truncated response may end partway through a credential.
		for n := min(len(secret)-1, len(message)); n > 0; n-- {
			if strings.HasSuffix(message, secret[:n]) {
				message = message[:len(message)-n] + "[redacted]"
				break
			}
		}
	}
	if len(message) > maxErrorBody {
		message = message[:maxErrorBody]
	}
	return message
}
