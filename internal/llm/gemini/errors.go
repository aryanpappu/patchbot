package gemini

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
)

var contextLimit = regexp.MustCompile(`(?i)context (length|window)|maximum context|too many tokens|token (count|limit)|exceeds?.{0,40}(input|token)|input.{0,40}(exceed|limit)`)

func (p *Provider) statusError(status int, header http.Header, data []byte) error {
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return p.failure(llm.ErrMalformed, "invalid Gemini error JSON", err)
	}
	e := p.failure(llm.KindFromStatus(status), "Gemini API request failed", nil)
	e.Status = status
	rpc := envelope.Error
	// HTTP status wins. RPC code supplies classification for proxies that use an
	// uninformative HTTP code; known status names are never echoed verbatim.
	if rpc.Code >= 400 && (status < 400 || status >= 600) {
		e.Kind = llm.KindFromStatus(rpc.Code)
	}
	if status == http.StatusBadRequest && (contextLimit.MatchString(rpc.Message) || strings.Contains(strings.ToLower(rpc.Status), "context_length")) {
		e.Kind = llm.ErrContextTooLong
		e.Message = "Gemini input exceeds the context token limit"
	}
	if e.Kind == llm.ErrRateLimit {
		e.RetryAfter = llm.ParseRetryAfter(header.Get("Retry-After"), time.Now())
		for _, detail := range rpc.Details {
			if detail.Type != "type.googleapis.com/google.rpc.RetryInfo" {
				continue
			}
			if delay, err := time.ParseDuration(detail.RetryDelay); err == nil && delay > e.RetryAfter {
				e.RetryAfter = delay
			}
		}
	}
	return e
}
