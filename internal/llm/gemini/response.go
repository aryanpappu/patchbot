package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/aryanpappu/patchbot/internal/llm"
)

type generateResponse struct {
	Candidates []struct {
		Content content `json:"content"`
		Finish  string  `json:"finishReason"`
	} `json:"candidates"`
	Feedback *struct {
		Block string `json:"blockReason"`
	} `json:"promptFeedback"`
	Usage *struct {
		Prompt     *int64 `json:"promptTokenCount"`
		Candidates *int64 `json:"candidatesTokenCount"`
		Cached     *int64 `json:"cachedContentTokenCount"`
		Thoughts   *int64 `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	Model string `json:"modelVersion"`
	ID    string `json:"responseId"`
}

func (p *Provider) decodeResponse(req llm.ChatRequest, raw generateResponse) (*llm.ChatResponse, error) {
	response := &llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Origin: p.name}, Model: raw.Model, RequestID: raw.ID}
	if response.Model == "" {
		response.Model = req.Model
	}
	if raw.Usage != nil && raw.Usage.Prompt != nil && raw.Usage.Candidates != nil {
		u := raw.Usage
		cached, thoughts := int64(0), int64(0)
		if u.Cached != nil {
			cached = *u.Cached
		}
		if u.Thoughts != nil {
			thoughts = *u.Thoughts
		}
		if *u.Prompt < 0 || *u.Candidates < 0 || cached < 0 || thoughts < 0 || cached > *u.Prompt || thoughts > math.MaxInt64-*u.Candidates {
			return nil, p.failure(llm.ErrMalformed, "invalid Gemini usage counts", nil)
		}
		response.Usage = &llm.Usage{InputTokens: *u.Prompt - cached, CacheReadTokens: cached, OutputTokens: *u.Candidates + thoughts}
	}
	if raw.Feedback != nil && raw.Feedback.Block != "" {
		response.RawStop, response.StopReason = raw.Feedback.Block, llm.StopRefusal
		return response, nil
	}
	if len(raw.Candidates) != 1 {
		return nil, p.failure(llm.ErrMalformed, "expected exactly one Gemini candidate", nil)
	}
	candidate := raw.Candidates[0]
	response.RawStop = candidate.Finish
	switch candidate.Finish {
	case "STOP":
		response.StopReason = llm.StopEndTurn
	case "MAX_TOKENS":
		response.StopReason = llm.StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		response.StopReason = llm.StopRefusal
	case "":
		return nil, p.failure(llm.ErrMalformed, "Gemini candidate has no finish reason", nil)
	default:
		response.StopReason = llm.StopOther
	}
	if candidate.Content.Role != "" && candidate.Content.Role != "model" {
		return nil, p.failure(llm.ErrMalformed, "Gemini candidate has invalid role", nil)
	}
	turn := 0
	seen := map[string]bool{}
	for _, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			turn++
		}
		for _, c := range m.ToolCalls() {
			seen[c.ID] = true
		}
	}
	// Reserve all explicit IDs before assigning any synthetic ones.
	for _, part := range candidate.Content.Parts {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(part, &fields); err != nil {
			return nil, p.failure(llm.ErrMalformed, "invalid Gemini part", err)
		}
		if f, ok := fields["functionCall"]; ok && candidate.Finish == "STOP" {
			var id struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(f, &id); err != nil {
				return nil, p.failure(llm.ErrMalformed, "invalid Gemini function call", err)
			}
			if id.ID != "" {
				if seen[id.ID] {
					return nil, p.failure(llm.ErrMalformed, "ambiguous duplicate Gemini call ID", nil)
				}
				seen[id.ID] = true
			}
		}
	}
	callIndex := 0
	for _, rawPart := range candidate.Content.Parts {
		part, err := p.decodePart(rawPart, candidate.Finish != "STOP")
		if err != nil {
			return nil, err
		}
		if part.Type == llm.PartToolCall {
			if part.ToolCall.ID == "" {
				id := fmt.Sprintf("call_%d_%d", turn, callIndex)
				for seen[id] {
					id += "_"
				}
				part.ToolCall.ID, seen[id] = id, true
			}
			callIndex++
		}
		response.Message.Parts = append(response.Message.Parts, part)
	}
	if callIndex > 0 {
		response.StopReason = llm.StopToolUse
	}
	if len(response.Message.Parts) == 0 && response.StopReason == llm.StopEndTurn {
		return nil, p.failure(llm.ErrMalformed, "Gemini returned an empty completed candidate", nil)
	}
	return response, nil
}

func (p *Provider) decodePart(raw json.RawMessage, suppressCalls bool) (llm.Part, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return llm.Part{}, p.failure(llm.ErrMalformed, "invalid Gemini part", err)
	}
	if len(fields) == 0 {
		return llm.Part{}, p.failure(llm.ErrMalformed, "empty Gemini part", nil)
	}
	part := llm.Part{Type: llm.PartOpaque, ProviderMeta: slices.Clone(raw)}
	text, hasText := fields["text"]
	call, hasCall := fields["functionCall"]
	if hasText && hasCall {
		return llm.Part{}, p.failure(llm.ErrMalformed, "ambiguous Gemini part with text and function call", nil)
	}
	switch {
	case hasText:
		var value string
		if err := json.Unmarshal(text, &value); err != nil || bytes.Equal(bytes.TrimSpace(text), []byte("null")) {
			return llm.Part{}, p.failure(llm.ErrMalformed, "invalid Gemini text part", err)
		}
		if value != "" {
			part.Type, part.Text = llm.PartText, value
		}
		if len(fields) == 1 && value != "" {
			part.ProviderMeta = nil
		}
	case hasCall && !suppressCalls:
		var c functionCall
		if err := json.Unmarshal(call, &c); err != nil {
			return llm.Part{}, p.failure(llm.ErrMalformed, "invalid Gemini function call", err)
		}
		if len(c.Args) == 0 {
			c.Args = json.RawMessage(`{}`)
		}
		args := bytes.TrimSpace(c.Args)
		if len(args) == 0 || args[0] != '{' || !json.Valid(args) {
			return llm.Part{}, p.failure(llm.ErrMalformed, "Gemini function arguments must be a JSON object", nil)
		}
		part.Type, part.ToolCall = llm.PartToolCall, &llm.ToolCall{ID: "check", Name: c.Name, Args: slices.Clone(c.Args)}
		// Reuse the normalized type's name/argument checks without exposing data.
		if err := llm.Validate([]llm.Message{llm.UserText("check"), {Role: llm.RoleAssistant, Parts: []llm.Part{part}}}); err != nil {
			return llm.Part{}, p.failure(llm.ErrMalformed, "invalid normalized Gemini function call", err)
		}
		part.ToolCall.ID = c.ID
	}
	return part, nil
}
