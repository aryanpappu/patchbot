// Package gemini adapts the public Generative Language REST API v1beta to llm.
// It performs no retries; llm.WithRetry owns retry policy.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

const (
	// Kind is the registry kind of this adapter.
	Kind            = "gemini"
	defaultBaseURL  = "https://generativelanguage.googleapis.com/v1beta"
	maxResponseBody = 10 << 20
)

// Provider is an immutable, concurrency-safe Gemini client.
type Provider struct {
	name   string
	key    secret.Secret
	base   *url.URL
	client *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New constructs a provider without making a network request.
func New(cfg llm.ProviderConfig) (llm.Provider, error) { return NewWithClient(cfg, nil) }

// NewWithClient constructs a provider using an injectable HTTP client. BaseURL
// is the API root including its version (normally /v1beta). The client is copied
// and redirects are refused so credentials cannot follow a redirect.
func NewWithClient(cfg llm.ProviderConfig, client *http.Client) (*Provider, error) {
	p := &Provider{name: cfg.Name, key: cfg.APIKey}
	if !cfg.APIKey.IsSet() || strings.ContainsAny(cfg.APIKey.Reveal(), "\r\n") {
		return nil, p.failure(llm.ErrAuth, "set a valid Gemini API key", nil)
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, p.failure(llm.ErrBadRequest, "invalid Gemini API base URL", err)
	}
	if containsKey(u.String(), cfg.APIKey.Reveal()) {
		return nil, p.failure(llm.ErrBadRequest, "credential found in API base URL", nil)
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p.base, p.client = u, &copyClient
	return p, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.name }

func (p *Provider) failure(kind llm.ErrorKind, message string, cause error) *llm.Error {
	return (&llm.Error{Provider: p.name, Kind: kind, Message: message}).WithCause(cause)
}

func containsKey(target, key string) bool {
	return strings.Contains(target, key) || strings.Contains(target, url.QueryEscape(key)) || strings.Contains(target, url.PathEscape(key))
}

// Chat implements llm.Provider.
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, p.failure(llm.ErrBadRequest, "invalid normalized chat request or unsupported tool schema", err)
	}
	model := strings.TrimPrefix(req.Model, "models/")
	if !modelID.MatchString(model) {
		return nil, p.failure(llm.ErrBadRequest, "invalid Gemini model ID", nil)
	}
	body, err := p.encodeRequest(req)
	if err != nil {
		return nil, err
	}
	var response generateResponse
	if err := p.request(ctx, http.MethodPost, "/models/"+model+":generateContent", "", body, &response); err != nil {
		return nil, err
	}
	return p.decodeResponse(req, response)
}

var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (p *Provider) request(ctx context.Context, method, path, page string, body []byte, out any) error {
	target := *p.base
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawPath = ""
	if page != "" {
		target.RawQuery = url.Values{"pageToken": {page}}.Encode()
	}
	if containsKey(target.String(), p.key.Reveal()) {
		return p.failure(llm.ErrBadRequest, "credential found in request URL", nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return p.failure(llm.ErrBadRequest, "cannot construct Gemini request", err)
	}
	req.Header.Set("x-goog-api-key", p.key.Reveal())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return p.ioFailure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return p.ioFailure(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) > maxResponseBody {
		return p.failure(llm.ErrMalformed, "Gemini response exceeds 10 MB", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p.statusError(resp.StatusCode, resp.Header, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return p.failure(llm.ErrMalformed, "invalid Gemini response JSON", err)
	}
	return nil
}

func (p *Provider) ioFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return p.failure(llm.ErrUnavailable, "Gemini transport or body read failed", err)
}

// ListModels implements llm.Provider, preserving API order across pages.
func (p *Provider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	var models []llm.ModelInfo
	seenPages := map[string]bool{}
	page := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var response struct {
			Models []struct {
				Name            string   `json:"name"`
				DisplayName     string   `json:"displayName"`
				InputTokenLimit int      `json:"inputTokenLimit"`
				Methods         []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			Next string `json:"nextPageToken"`
		}
		if err := p.request(ctx, http.MethodGet, "/models", page, nil, &response); err != nil {
			return nil, err
		}
		for _, m := range response.Models {
			if !slices.Contains(m.Methods, "generateContent") {
				continue
			}
			id := strings.TrimPrefix(m.Name, "models/")
			if !modelID.MatchString(id) || m.InputTokenLimit < 0 {
				return nil, p.failure(llm.ErrMalformed, "invalid Gemini model listing", nil)
			}
			models = append(models, llm.ModelInfo{ID: id, DisplayName: m.DisplayName, ContextWindow: m.InputTokenLimit, Source: llm.SourceLive})
		}
		if response.Next == "" {
			return models, nil
		}
		if seenPages[response.Next] {
			return nil, p.failure(llm.ErrMalformed, "repeated Gemini pagination token", nil)
		}
		seenPages[response.Next], page = true, response.Next
	}
}
