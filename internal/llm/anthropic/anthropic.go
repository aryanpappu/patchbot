// Package anthropic adapts the official Anthropic SDK to patchbot's LLM types.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/aryanpappu/patchbot/internal/llm"
)

// Kind is the provider kind registered by this adapter.
const Kind = "anthropic"

// Options supplies the HTTP client. The base URL comes from ProviderConfig.
type Options struct{ HTTPClient *http.Client }

// Provider is an Anthropic provider instance. Retries belong to llm.WithRetry.
type Provider struct {
	cfg      llm.ProviderConfig
	messages sdk.MessageService
	models   sdk.ModelService
}

var _ llm.Provider = (*Provider)(nil)

// New constructs a provider without making a request or discovering credentials.
func New(cfg llm.ProviderConfig) (llm.Provider, error) { return NewWithOptions(cfg, Options{}) }

// NewWithOptions constructs a provider with an injectable HTTP client.
func NewWithOptions(cfg llm.ProviderConfig, opts Options) (*Provider, error) {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	copyClient := *client
	transport := copyClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copyClient.Transport = boundedTransport{base: transport}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &copyClient
	// Service constructors avoid NewClient's environment/profile credential chain.
	options := []option.RequestOption{option.WithBaseURL(base), option.WithAPIKey(cfg.APIKey.Reveal()), option.WithHTTPClient(client), option.WithMaxRetries(0)}
	return &Provider{cfg: cfg, messages: sdk.NewMessageService(options...), models: sdk.NewModelService(options...)}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.cfg.Name }

func (p *Provider) ready(ctx context.Context) error {
	if err := p.contextFailure(ctx); err != nil {
		return err
	}
	if !p.cfg.APIKey.IsSet() {
		return &llm.Error{Kind: llm.ErrAuth, Provider: p.Name(), Message: "API key is not configured; set the provider's api_key_env variable"}
	}
	return nil
}

// Chat sends a non-streaming Messages request.
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, 10*time.Minute, errRequestTimeout)
	defer cancel()
	if err := p.ready(ctx); err != nil {
		return nil, err
	}
	req.Messages = mergeToolMessages(req.Messages)
	if err := req.Validate(); err != nil {
		return nil, p.failure(llm.ErrBadRequest, "invalid chat request", err)
	}
	params, err := p.params(req)
	if err != nil {
		return nil, err
	}
	// The SDK can reject large non-streaming calls before sending HTTP. Treat
	// that local limit as a bad request rather than a retryable outage.
	if _, err := sdk.CalculateNonStreamingTimeout(req.MaxTokens, params.Model, p.messages.Options); err != nil {
		return nil, p.failure(llm.ErrBadRequest, "request exceeds SDK non-streaming limits or has invalid options", err)
	}
	var raw *http.Response
	var body []byte
	// Let the SDK finish reading the body before decoding it. A dropped
	// connection and incomplete JSON can both report io.ErrUnexpectedEOF;
	// only a decoding failure of a complete body is malformed.
	_, err = p.messages.New(ctx, params, option.WithResponseInto(&raw), option.WithResponseBodyInto(&body))
	if err != nil {
		return nil, p.mapError(ctx, err, raw)
	}
	var msg sdk.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		if ctxErr := p.contextFailure(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		failure := p.failure(llm.ErrMalformed, "invalid provider response", err)
		if raw != nil {
			failure.RequestID = raw.Header.Get("request-id")
			failure.RetryAfter = llm.ParseRetryAfter(raw.Header.Get("retry-after"), time.Now())
		}
		return nil, failure
	}
	return p.response(&msg, raw)
}

// Consecutive normalized tool messages can be supplied by callers that produce
// one result at a time. Validate the merged message so IDs and order are checked.
func mergeToolMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == llm.RoleTool && len(out) > 0 && out[len(out)-1].Role == llm.RoleTool {
			out[len(out)-1].Parts = append(out[len(out)-1].Parts, m.Parts...)
		} else {
			out = append(out, m.Clone())
		}
	}
	return out
}

func (p *Provider) params(req llm.ChatRequest) (sdk.MessageNewParams, error) {
	out := sdk.MessageNewParams{Model: sdk.Model(req.Model), MaxTokens: int64(req.MaxTokens)}
	cache := sdk.CacheControlEphemeralParam{Type: "ephemeral"}
	if req.System != "" {
		out.System = []sdk.TextBlockParam{{Text: req.System, CacheControl: cache}}
	}
	if req.Temperature != nil {
		out.Temperature = sdk.Float(*req.Temperature)
	}
	for i, tool := range req.Tools {
		t := sdk.ToolParam{Name: tool.Name, Description: sdk.String(tool.Description), InputSchema: param.Override[sdk.ToolInputSchemaParam](tool.Schema)}
		if i == len(req.Tools)-1 {
			t.CacheControl = cache
		}
		out.Tools = append(out.Tools, sdk.ToolUnionParam{OfTool: &t})
	}
	for _, m := range req.Messages {
		blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.Parts))
		for _, part := range m.Parts {
			switch part.Type {
			case llm.PartText:
				blocks = append(blocks, sdk.NewTextBlock(part.Text))
			case llm.PartToolCall:
				c := part.ToolCall
				blocks = append(blocks, sdk.NewToolUseBlock(c.ID, c.Args, c.Name))
			case llm.PartToolResult:
				r := part.ToolResult
				block := sdk.NewToolResultBlock(r.CallID, r.Content, r.IsError)
				if r.Content == "" {
					block.OfToolResult.Content = nil
				}
				blocks = append(blocks, block)
			case llm.PartOpaque:
				if m.Origin != p.Name() {
					continue
				}
				var header struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(part.ProviderMeta, &header); err != nil || header.Type == "" {
					return out, p.failure(llm.ErrBadRequest, "invalid replay block", err)
				}
				blocks = append(blocks, param.Override[sdk.ContentBlockParamUnion](part.ProviderMeta))
			}
		}
		if len(blocks) == 0 {
			return out, p.failure(llm.ErrBadRequest, "message has no replayable content", nil)
		}
		if m.Role == llm.RoleAssistant {
			out.Messages = append(out.Messages, sdk.NewAssistantMessage(blocks...))
			continue
		}
		// Tool results and subsequent user feedback share one user message.
		if len(out.Messages) > 0 && out.Messages[len(out.Messages)-1].Role == sdk.MessageParamRoleUser {
			out.Messages[len(out.Messages)-1].Content = append(out.Messages[len(out.Messages)-1].Content, blocks...)
		} else {
			out.Messages = append(out.Messages, sdk.NewUserMessage(blocks...))
		}
	}
	// Cache the conversation prefix as well as the system and tool definitions.
	// A valid request ends on the user side, so this never changes replay blocks.
	lastMessage := &out.Messages[len(out.Messages)-1]
	last := &lastMessage.Content[len(lastMessage.Content)-1]
	if last.OfText != nil {
		last.OfText.CacheControl = cache
	}
	if last.OfToolResult != nil {
		last.OfToolResult.CacheControl = cache
	}
	return out, nil
}

func (p *Provider) response(msg *sdk.Message, raw *http.Response) (*llm.ChatResponse, error) {
	if !msg.JSON.Content.Valid() || msg.Model == "" || string(msg.Role) != "assistant" {
		return nil, p.failure(llm.ErrMalformed, "invalid message envelope", nil)
	}
	out := &llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Origin: p.Name()}, Model: string(msg.Model), RawStop: string(msg.StopReason), StopReason: stopReason(string(msg.StopReason))}
	if raw != nil {
		out.RequestID = raw.Header.Get("request-id")
	}
	if msg.JSON.Usage.Valid() && msg.Usage.JSON.InputTokens.Valid() && msg.Usage.JSON.OutputTokens.Valid() {
		out.Usage = &llm.Usage{InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens, CacheReadTokens: msg.Usage.CacheReadInputTokens, CacheWriteTokens: msg.Usage.CacheCreationInputTokens}
	}
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out.Message.Parts = append(out.Message.Parts, llm.Text(b.Text))
			}
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			if !isObject(args) {
				// A truncated call is never exposed as an executable ToolCall.
				if out.StopReason == llm.StopMaxTokens {
					out.Message.Parts = append(out.Message.Parts, opaque(b.RawJSON()))
					continue
				}
				return nil, p.failure(llm.ErrMalformed, "tool arguments are not an object", nil)
			}
			if b.ID == "" || b.Name == "" {
				return nil, p.failure(llm.ErrMalformed, "tool call is missing its ID or name", nil)
			}
			out.Message.Parts = append(out.Message.Parts, llm.Call(b.ID, b.Name, bytes.Clone(args)))
		default:
			if b.Type == "" || !isObject(json.RawMessage(b.RawJSON())) {
				return nil, p.failure(llm.ErrMalformed, "invalid replay block", nil)
			}
			out.Message.Parts = append(out.Message.Parts, opaque(b.RawJSON()))
		}
	}
	if len(out.Message.Parts) > 0 {
		if err := llm.Validate([]llm.Message{llm.UserText("validation"), out.Message}); err != nil {
			return nil, p.failure(llm.ErrMalformed, "invalid assistant message", err)
		}
	}
	return out, nil
}

func opaque(raw string) llm.Part {
	return llm.Part{Type: llm.PartOpaque, ProviderMeta: json.RawMessage(raw)}
}
func isObject(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) > 0 && b[0] == '{' && json.Valid(b)
}

func stopReason(s string) llm.StopReason {
	switch s {
	case "end_turn", "stop_sequence":
		return llm.StopEndTurn
	case "tool_use":
		return llm.StopToolUse
	case "max_tokens", "model_context_window_exceeded":
		return llm.StopMaxTokens
	case "refusal":
		return llm.StopRefusal
	default:
		return llm.StopOther
	}
}

// ListModels lists every page of models offered by this provider.
func (p *Provider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, 30*time.Second, errRequestTimeout)
	defer cancel()
	if err := p.ready(ctx); err != nil {
		return nil, err
	}
	pager := p.models.ListAutoPaging(ctx, sdk.ModelListParams{})
	var models []llm.ModelInfo
	for pager.Next() {
		m := pager.Current()
		models = append(models, llm.ModelInfo{ID: m.ID, DisplayName: m.DisplayName, Source: llm.SourceLive})
	}
	if err := pager.Err(); err != nil {
		return nil, p.mapError(ctx, err, nil)
	}
	return models, nil
}

func (p *Provider) failure(kind llm.ErrorKind, message string, cause error) *llm.Error {
	return (&llm.Error{Kind: kind, Provider: p.Name(), Message: message}).WithCause(cause)
}

func (p *Provider) mapError(ctx context.Context, err error, raw *http.Response) error {
	if ctxErr := p.contextFailure(ctx); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, errResponseTooLarge) {
		return p.failure(llm.ErrMalformed, "provider response exceeds 10 MB", nil)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	// A client/SDK timeout while the caller's context is alive is retryable.
	// Keep its URL-bearing error text behind WithCause.
	out := p.failure(llm.ErrUnavailable, "", err)
	if apiErr, ok := errors.AsType[*sdk.Error](err); ok {
		out.Status = apiErr.StatusCode
		raw = apiErr.Response
		out.Kind = llm.KindFromStatus(out.Status)
		// Read the message only to classify it. Never print the API body, which can
		// echo prompts or credentials; the SDK error itself is retained via WithCause.
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if out.Status == http.StatusBadRequest && json.Unmarshal([]byte(apiErr.RawJSON()), &body) == nil {
			message := strings.ToLower(body.Error.Message)
			if strings.Contains(message, "prompt is too long") || strings.Contains(message, "prompt too long") || strings.Contains(message, "context length") {
				out.Kind = llm.ErrContextTooLong
			}
		}
		out.Message = "provider rejected the request"
	}
	if raw != nil {
		out.RequestID = raw.Header.Get("request-id")
		out.RetryAfter = llm.ParseRetryAfter(raw.Header.Get("retry-after"), time.Now())
	}
	return out
}

const maxResponseBody = 10 << 20

var errResponseTooLarge = errors.New("provider response exceeds 10 MB")

// The SDK reads bodies internally. Bound every response, including error and
// paginated model responses, before handing it to the SDK decoder.
type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedBody{ReadCloser: response.Body, remaining: maxResponseBody}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errResponseTooLarge
	}
	n, err := b.ReadCloser.Read(p[:min(len(p), b.remaining+1)])
	b.remaining -= n
	if b.remaining < 0 {
		return n, errResponseTooLarge
	}
	return n, err
}

var errRequestTimeout = errors.New("adapter request timeout")

func (p *Provider) contextFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(context.Cause(ctx), errRequestTimeout) {
			return p.failure(llm.ErrUnavailable, "provider request timed out", err)
		}
		return err
	}
	return nil
}
