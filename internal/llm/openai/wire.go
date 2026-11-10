package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/aryanpappu/patchbot/internal/llm"
)

type wireFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role         string        `json:"role"`
	Content      *string       `json:"content"`
	ToolCalls    []wireCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	FunctionCall *wireFunction `json:"function_call,omitempty"`
	Refusal      *string       `json:"refusal,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []wireMessage `json:"messages"`
	Tools               []wireTool    `json:"tools,omitempty"`
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	ParallelToolCalls   *bool         `json:"parallel_tool_calls,omitempty"`
}

func (p *Provider) encodeRequest(req llm.ChatRequest) ([]byte, error) {
	wire := chatRequest{Model: req.Model, Temperature: req.Temperature}
	if p.quirks.MaxTokensField == "max_completion_tokens" {
		wire.MaxCompletionTokens = req.MaxTokens
	} else {
		wire.MaxTokens = req.MaxTokens
	}
	if req.System != "" {
		wire.Messages = append(wire.Messages, wireMessage{Role: "system", Content: &req.System})
	}
	for _, message := range req.Messages {
		if message.Role == llm.RoleTool {
			for _, part := range message.Parts {
				result := part.ToolResult
				text := result.Content
				if result.IsError {
					text = "ERROR: " + text
				}
				wire.Messages = append(wire.Messages, wireMessage{Role: "tool", Content: &text, ToolCallID: result.CallID})
			}
			continue
		}
		wm := wireMessage{Role: string(message.Role)}
		text := message.Text()
		if text != "" || message.Role == llm.RoleUser {
			wm.Content = &text
		}
		for _, part := range message.Parts {
			switch {
			case len(part.ProviderMeta) > 0 && message.Origin == p.name:
				return nil, p.failure(llm.ErrBadRequest, "unsupported OpenAI continuation metadata", nil)
			case part.Type == llm.PartToolCall:
				args, err := json.Marshal(string(part.ToolCall.Args))
				if err != nil {
					return nil, p.failure(llm.ErrBadRequest, "cannot encode tool arguments", err)
				}
				wm.ToolCalls = append(wm.ToolCalls, wireCall{ID: part.ToolCall.ID, Type: "function", Function: wireFunction{Name: part.ToolCall.Name, Arguments: args}})
			}
		}
		wire.Messages = append(wire.Messages, wm)
	}
	for _, tool := range req.Tools {
		wt := wireTool{Type: "function"}
		wt.Function.Name, wt.Function.Description = tool.Name, tool.Description
		wt.Function.Parameters = tool.Schema
		wire.Tools = append(wire.Tools, wt)
	}
	if len(wire.Tools) > 0 && !p.quirks.OmitParallelToolCalls {
		enabled := true
		wire.ParallelToolCalls = &enabled
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, p.failure(llm.ErrBadRequest, "cannot encode chat request", err)
	}
	return body, nil
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func (p *Provider) normalize(req llm.ChatRequest, wire chatResponse) (*llm.ChatResponse, error) {
	if len(wire.Choices) != 1 || wire.Choices[0].Message.Role != "assistant" {
		return nil, p.failure(llm.ErrMalformed, "expected one assistant choice", nil)
	}
	choice := wire.Choices[0]
	out := &llm.ChatResponse{Model: wire.Model, Message: llm.Message{Role: llm.RoleAssistant, Origin: p.name}}
	switch choice.FinishReason {
	case "stop":
		out.StopReason = llm.StopEndTurn
	case "tool_calls", "function_call":
		out.StopReason = llm.StopToolUse
	case "length":
		out.StopReason = llm.StopMaxTokens
	case "content_filter":
		out.StopReason = llm.StopRefusal
	default:
		out.StopReason = llm.StopOther
	}
	// RawStop is logged by callers: don't copy arbitrary provider text there.
	if out.StopReason != llm.StopOther {
		out.RawStop = choice.FinishReason
	}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		out.Message.Parts = append(out.Message.Parts, llm.Text(*choice.Message.Content))
	}
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		out.StopReason = llm.StopRefusal
		out.Message.Parts = append(out.Message.Parts, llm.Text(*choice.Message.Refusal))
	}
	// Truncated or blocked calls must never become executable, including
	// syntactically valid arguments that may be only part of the intended call.
	if out.StopReason != llm.StopMaxTokens && out.StopReason != llm.StopRefusal {
		if err := p.normalizeCalls(req, choice.Message, out); err != nil {
			return nil, err
		}
	}
	if len(out.Message.Parts) == 0 && out.StopReason != llm.StopMaxTokens && out.StopReason != llm.StopRefusal {
		return nil, p.failure(llm.ErrMalformed, "assistant response has no content", nil)
	}
	if wire.Usage != nil && wire.Usage.PromptTokens != nil && wire.Usage.CompletionTokens != nil {
		input, output, cached := *wire.Usage.PromptTokens, *wire.Usage.CompletionTokens, wire.Usage.PromptDetails.CachedTokens
		if input < 0 || output < 0 || cached < 0 || cached > input {
			return nil, p.failure(llm.ErrMalformed, "invalid API usage counts", nil)
		}
		out.Usage = &llm.Usage{InputTokens: input - cached, OutputTokens: output, CacheReadTokens: cached}
	}
	return out, nil
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (p *Provider) normalizeCalls(req llm.ChatRequest, message wireMessage, out *llm.ChatResponse) error {
	calls := message.ToolCalls
	if message.FunctionCall != nil {
		if len(calls) > 0 {
			return p.failure(llm.ErrMalformed, "ambiguous legacy and modern tool calls", nil)
		}
		calls = []wireCall{{Type: "function", Function: *message.FunctionCall}}
	}
	if len(calls) == 0 && out.StopReason == llm.StopToolUse {
		return p.failure(llm.ErrMalformed, "tool finish reason without a tool call", nil)
	}
	used := make(map[string]bool)
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls() {
			used[c.ID] = true
		}
	}
	// Reserve all supplied IDs before making any up, to avoid collisions
	// between a synthetic ID and a later provider call.
	reserved := make(map[string]bool)
	for _, c := range calls {
		reserved[c.ID] = true
	}
	for i, call := range calls {
		if call.Type != "function" || !toolNamePattern.MatchString(call.Function.Name) {
			return p.failure(llm.ErrMalformed, "unsupported tool call type or name", nil)
		}
		args, err := normalizeArguments(call.Function.Arguments)
		if err != nil {
			return p.failure(llm.ErrMalformed, "tool arguments must be a valid JSON object", err)
		}
		id := call.ID
		if id == "" || used[id] {
			for suffix := 0; ; suffix++ {
				id = fmt.Sprintf("call_%d_%d_%d", len(req.Messages), i, suffix)
				if !used[id] && !reserved[id] {
					break
				}
			}
		}
		used[id] = true
		out.Message.Parts = append(out.Message.Parts, llm.Call(id, call.Function.Name, args))
	}
	if len(calls) > 0 {
		out.StopReason = llm.StopToolUse
	}
	return nil
}

func normalizeArguments(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		raw = bytes.TrimSpace([]byte(text))
		if len(raw) == 0 {
			return json.RawMessage(`{}`), nil
		}
	}
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return nil, errors.New("arguments are not an object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
