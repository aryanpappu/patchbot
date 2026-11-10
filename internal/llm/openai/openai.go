// Package openai adapts OpenAI-compatible Chat Completions APIs to llm.
// It uses non-streaming HTTP requests and leaves retries to llm.WithRetry.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

// Kind is the registry key for this adapter.
const Kind = "openai"

const (
	maxResponseBody = 10 << 20
	requestTimeout  = 2 * time.Minute
)

var errRequestTimeout = errors.New("OpenAI adapter request timeout")

// Quirks selects optional request fields for one instance. Response quirks
// (null content, object arguments and missing usage) are always accepted.
type Quirks struct {
	// OmitParallelToolCalls supports servers that reject or ignore this field.
	// Otherwise parallel_tool_calls=true allows batches, executed in order by
	// the caller. Omitting it never strips tools from a request.
	OmitParallelToolCalls bool
	// MaxTokensField is "max_tokens" or "max_completion_tokens".
	// Empty means "max_tokens".
	MaxTokensField string
}

// Options supplies transport and compatibility overrides without changing
// the shared provider config. Quirks, when set, replaces URL detection.
type Options struct {
	HTTPClient *http.Client
	Quirks     *Quirks
}

// Provider is an immutable, concurrency-safe configured API instance.
type Provider struct {
	name   string
	base   *url.URL
	key    secret.Secret
	client *http.Client
	quirks Quirks
}

var _ llm.Provider = (*Provider)(nil)

// New implements llm.Constructor. BaseURL is required; APIKey is optional.
func New(cfg llm.ProviderConfig) (llm.Provider, error) {
	return NewWithOptions(cfg, Options{})
}

// NewWithOptions constructs an instance without network access. The client
// is copied and redirects are refused, so credentials cannot be forwarded.
// BaseURL includes the API prefix, e.g. /v1 or /openai/v1; query strings,
// userinfo and fragments are rejected without echoing their contents.
func NewWithOptions(cfg llm.ProviderConfig, opts Options) (*Provider, error) {
	p := &Provider{name: cfg.Name, key: cfg.APIKey}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, p.failure(llm.ErrBadRequest, "invalid API base URL", err)
	}
	if base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" {
		return nil, p.failure(llm.ErrBadRequest, "API base URL must be an absolute HTTP URL without credentials, query or fragment", nil)
	}
	if strings.ContainsAny(cfg.APIKey.Reveal(), "\r\n") {
		return nil, p.failure(llm.ErrAuth, "invalid API key header", nil)
	}
	p.base = base
	p.quirks = detectedQuirks(base)
	if opts.Quirks != nil {
		p.quirks = *opts.Quirks
	}
	if p.quirks.MaxTokensField == "" {
		p.quirks.MaxTokensField = "max_tokens"
	}
	if p.quirks.MaxTokensField != "max_tokens" && p.quirks.MaxTokensField != "max_completion_tokens" {
		return nil, p.failure(llm.ErrBadRequest, "unsupported maximum tokens field", nil)
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p.client = &copyClient
	return p, nil
}

func detectedQuirks(base *url.URL) Quirks {
	switch strings.ToLower(base.Hostname()) {
	case "api.openai.com":
		return Quirks{MaxTokensField: "max_completion_tokens"}
	case "api.groq.com", "localhost", "127.0.0.1", "::1", "ollama", "lmstudio":
		return Quirks{MaxTokensField: "max_tokens", OmitParallelToolCalls: true}
	default: // Includes OpenRouter and unrecognized compatible servers.
		return Quirks{MaxTokensField: "max_tokens"}
	}
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.name }

// Chat sends one Chat Completions request. Invalid requests never reach HTTP.
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, p.failure(llm.ErrBadRequest, "invalid chat request", err)
	}
	body, err := p.encodeRequest(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, requestTimeout, errRequestTimeout)
	defer cancel()
	var response chatResponse
	meta, err := p.request(ctx, http.MethodPost, p.endpoint("chat/completions"), body, &response)
	if err != nil {
		return nil, err
	}
	out, err := p.normalize(req, response)
	if err != nil {
		return nil, err
	}
	out.RequestID = p.requestID(meta.header)
	return out, nil
}

func (p *Provider) endpoint(path string) *url.URL {
	u := *p.base
	escaped := strings.TrimRight(u.EscapedPath(), "/") + "/" + path
	u.Path = strings.TrimRight(u.Path, "/") + "/" + path
	u.RawPath = escaped
	return &u
}

type responseMeta struct {
	header http.Header
	status int
}

func (p *Provider) request(ctx context.Context, method string, target *url.URL, body []byte, out any) (responseMeta, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return responseMeta{}, p.failure(llm.ErrBadRequest, "cannot construct API request", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.key.IsSet() {
		req.Header.Set("Authorization", "Bearer "+p.key.Reveal())
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return responseMeta{}, p.ioFailure(ctx, "API transport failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	meta := responseMeta{header: resp.Header, status: resp.StatusCode}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return meta, p.ioFailure(ctx, "cannot read API response", err)
	}
	if err := p.contextFailure(ctx); err != nil {
		return meta, err
	}
	if len(data) > maxResponseBody {
		return meta, p.failure(llm.ErrMalformed, "API response exceeds 10 MB", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return meta, p.statusError(meta, data)
	}
	// Providers can fail after sending HTTP 200, including after partial
	// generation. Classify errors before decoding or accepting any content.
	var envelope errorEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return meta, p.failure(llm.ErrMalformed, "invalid API JSON response", err)
	}
	if envelope.Error != nil {
		return meta, p.embeddedError(meta, envelope.Error)
	}
	for _, choice := range envelope.Choices {
		if choice.Error != nil {
			return meta, p.embeddedError(meta, choice.Error)
		}
		if choice.FinishReason == "error" {
			return meta, p.failure(llm.ErrMalformed, "API generation failed without an error code", nil)
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return meta, p.failure(llm.ErrMalformed, "invalid API JSON response", err)
	}
	return meta, nil
}

// Only fixed diagnostic text reaches observable errors. The underlying
// cause may contain URLs, keys or payloads, so it is retained only by WithCause.
func (p *Provider) failure(kind llm.ErrorKind, message string, cause error) *llm.Error {
	return (&llm.Error{Kind: kind, Provider: p.name, Message: message}).WithCause(cause)
}

func (p *Provider) ioFailure(ctx context.Context, message string, cause error) error {
	if err := p.contextFailure(ctx); err != nil {
		return err
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	return p.failure(llm.ErrUnavailable, message, cause)
}

func (p *Provider) contextFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(context.Cause(ctx), errRequestTimeout) {
			return p.failure(llm.ErrUnavailable, "API request timed out", err)
		}
		return err
	}
	return nil
}

var contextOverflow = regexp.MustCompile(`(?i)context (length|window)|maximum context|too many tokens`)

type apiError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

type errorEnvelope struct {
	Error   *apiError `json:"error"`
	Choices []struct {
		Error        *apiError `json:"error"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
}

func (p *Provider) embeddedError(meta responseMeta, detail *apiError) *llm.Error {
	var status int
	if json.Unmarshal(detail.Code, &status) != nil || status < 400 || status > 599 {
		return p.failure(llm.ErrMalformed, "API generation failed without a valid error code", nil)
	}
	meta.status = status
	return p.rejection(meta, detail)
}

func (p *Provider) statusError(meta responseMeta, data []byte) *llm.Error {
	var envelope errorEnvelope
	_ = json.Unmarshal(data, &envelope)
	return p.rejection(meta, envelope.Error)
}

func (p *Provider) rejection(meta responseMeta, detail *apiError) *llm.Error {
	kind := llm.KindFromStatus(meta.status)
	if meta.status == http.StatusBadRequest && detail != nil &&
		(string(detail.Code) == `"context_length_exceeded"` || contextOverflow.MatchString(detail.Message)) {
		kind = llm.ErrContextTooLong
	}
	return &llm.Error{
		Kind: kind, Provider: p.name, Status: meta.status,
		RetryAfter: llm.ParseRetryAfter(meta.header.Get("Retry-After"), time.Now()),
		Message:    "API rejected the request", RequestID: p.requestID(meta.header),
	}
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (p *Provider) requestID(header http.Header) string {
	id := header.Get("X-Request-ID")
	// Never expose an echoed credential, even one too short for the redactor.
	if !requestIDPattern.MatchString(id) || (p.key.IsSet() && strings.Contains(id, p.key.Reveal())) {
		return ""
	}
	return (*secret.Redactor)(nil).Redact(id)
}
