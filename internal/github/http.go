package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/version"
)

const (
	defaultBaseURL  = "https://api.github.com"
	requestTimeout  = 30 * time.Second
	maxResponseBody = 10 << 20
	maxComments     = 300
)

// HTTPClient implements Client using the versioned GitHub REST API. Its settings
// are immutable after construction, making it safe for concurrent use.
type HTTPClient struct {
	httpClient *http.Client
	baseURL    *url.URL
	tokens     TokenSource
	userAgent  string
}

var _ Client = (*HTTPClient)(nil)

// New constructs a REST client. Empty baseURL selects api.github.com; an
// Enterprise base URL can include /api/v3. A nil httpClient uses http.DefaultClient.
// Each request has a 30 second deadline, including token acquisition and reads.
// The supplied http.Client is copied; redirects are refused to protect credentials.
func New(httpClient *http.Client, baseURL string, tokens TokenSource) (*HTTPClient, error) {
	if tokens == nil {
		return nil, errors.New("New: token source is required")
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" {
		return nil, errors.New("New: invalid GitHub API base URL")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	transport := *httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPClient{
		httpClient: &transport, baseURL: base, tokens: tokens, userAgent: "patchbot/" + version.Version,
	}, nil
}

func (c *HTTPClient) endpoint(repo RepoRef, parts ...string) (*url.URL, error) {
	if !validRepoPart(repo.Owner) || !validRepoPart(repo.Name) {
		return nil, errors.New("invalid repository reference")
	}
	path := strings.TrimRight(c.baseURL.EscapedPath(), "/") + "/repos/" +
		url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return nil, errors.New("invalid API path component")
		}
		path += "/" + url.PathEscape(part)
	}
	u := *c.baseURL
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return nil, errors.New("invalid API path")
	}
	u.Path, u.RawPath = decoded, path
	return &u, nil
}

func validRepoPart(part string) bool {
	return part != "" && part != "." && part != ".." && !strings.ContainsAny(part, "/\\\x00")
}

type responseMeta struct {
	header http.Header
	status int
}

func (c *HTTPClient) request(ctx context.Context, method string, target *url.URL, body, out any, statuses ...int) (responseMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	token, err := c.tokens.Token(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return responseMeta{}, ctx.Err()
		}
		// A token source's error can itself contain a credential.
		return responseMeta{}, errors.New("authentication token unavailable")
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return responseMeta{}, errors.New("invalid authentication token")
	}
	for _, secret := range tokenForms(token) {
		if strings.Contains(target.String(), secret) {
			return responseMeta{}, errors.New("credential found in request URL")
		}
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return responseMeta{}, errors.New("cannot encode request body")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(data))
	if err != nil {
		return responseMeta{}, errors.New("cannot construct API request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag, ok := ctx.Value(etagKey{}).(string); ok && etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return responseMeta{}, safeIOError(ctx, err, "API request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	meta := responseMeta{header: resp.Header, status: resp.StatusCode}
	if !slices.Contains(statuses, resp.StatusCode) {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if readErr != nil {
			return meta, safeIOError(ctx, readErr, "cannot read API error response")
		}
		return meta, responseError(resp.StatusCode, resp.Header, data, token)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return meta, safeIOError(ctx, err, "cannot read API response")
	}
	if len(data) > maxResponseBody {
		return meta, errors.New("API response exceeds 10 MB")
	}
	if out != nil && resp.StatusCode != http.StatusNotModified {
		if err := json.Unmarshal(data, out); err != nil {
			// Decoder errors can quote response data, including echoed credentials.
			return meta, errors.New("invalid API JSON response")
		}
	}
	return meta, nil
}

func safeIOError(ctx context.Context, err error, message string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New(message)
}

// nextPage accepts only links to the same API endpoint, preventing pagination
// from sending authorization to another host or an unrelated API path.
func nextPage(current *url.URL, header http.Header) (*url.URL, error) {
	for _, line := range header.Values("Link") {
		for _, link := range strings.Split(line, ",") {
			parts := strings.Split(link, ";")
			next := false
			for _, parameter := range parts[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if ok && strings.EqualFold(key, "rel") && slices.Contains(strings.Fields(strings.Trim(value, "\"")), "next") {
					next = true
				}
			}
			if !next {
				continue
			}
			ref, err := url.Parse(strings.Trim(strings.TrimSpace(parts[0]), "<>"))
			if err != nil {
				return nil, errors.New("invalid pagination URL")
			}
			u := current.ResolveReference(ref)
			if u.Scheme != current.Scheme || u.Host != current.Host || u.EscapedPath() != current.EscapedPath() ||
				u.User != nil || u.Fragment != "" || u.Opaque != "" {
				return nil, errors.New("unsafe pagination URL")
			}
			return u, nil
		}
	}
	return nil, nil
}

func operationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
