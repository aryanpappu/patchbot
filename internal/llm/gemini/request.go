package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aryanpappu/patchbot/internal/llm"
)

type content struct {
	Role  string            `json:"role,omitempty"`
	Parts []json.RawMessage `json:"parts"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func (p *Provider) encodeRequest(req llm.ChatRequest) ([]byte, error) {
	type declaration struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Parameters  *llm.Schema `json:"parameters"`
	}
	type tool struct {
		Declarations []declaration `json:"functionDeclarations"`
	}
	header := struct {
		System *content `json:"systemInstruction,omitempty"`
		Tools  []tool   `json:"tools,omitempty"`
		Config struct {
			MaxTokens   int      `json:"maxOutputTokens"`
			Temperature *float64 `json:"temperature,omitempty"`
		} `json:"generationConfig"`
	}{}
	if req.System != "" {
		raw, err := json.Marshal(map[string]string{"text": req.System})
		if err != nil {
			return nil, p.failure(llm.ErrBadRequest, "cannot encode system instruction", err)
		}
		header.System = &content{Parts: []json.RawMessage{raw}}
	}
	header.Config.MaxTokens, header.Config.Temperature = req.MaxTokens, req.Temperature
	if len(req.Tools) > 0 {
		declarations := make([]declaration, 0, len(req.Tools))
		for _, spec := range req.Tools {
			schema, err := llm.ParseSchema(spec.Schema)
			if err != nil {
				return nil, p.failure(llm.ErrBadRequest, "invalid or unsupported tool schema", err)
			}
			schema = schema.For(llm.DialectGemini)
			upperTypes(schema)
			declarations = append(declarations, declaration{spec.Name, spec.Description, schema})
		}
		header.Tools = []tool{{Declarations: declarations}}
	}
	data, err := json.Marshal(header)
	if err != nil {
		return nil, p.failure(llm.ErrBadRequest, "cannot encode Gemini generation settings", err)
	}
	// encoding/json compacts RawMessage values. Write content parts directly to
	// preserve signed and unknown fields, including whitespace, byte for byte.
	data = append(data[:len(data)-1], `,"contents":[`...)
	var pending []string
	for i, msg := range req.Messages {
		if i > 0 {
			data = append(data, ',')
		}
		role := "user"
		if msg.Role == llm.RoleAssistant {
			role = "model"
		}
		data = append(data, `{"role":"`+role+`","parts":[`...)
		var parts []json.RawMessage
		resultIndex := 0
		if msg.Role == llm.RoleAssistant {
			pending = nil
		}
		for _, part := range msg.Parts {
			raw, wireID, err := p.encodePart(msg, part, pending, resultIndex)
			if err != nil {
				return nil, err
			}
			if part.Type == llm.PartToolCall {
				pending = append(pending, wireID)
			}
			if part.Type == llm.PartToolResult {
				resultIndex++
			}
			if raw != nil {
				parts = append(parts, raw)
			}
		}
		if len(parts) == 0 {
			return nil, p.failure(llm.ErrBadRequest, "message has no Gemini-compatible parts", nil)
		}
		for j, raw := range parts {
			if j > 0 {
				data = append(data, ',')
			}
			data = append(data, raw...)
		}
		data = append(data, `]}`...)
	}
	return append(data, `]}`...), nil
}

func upperTypes(s *llm.Schema) {
	s.Type = strings.ToUpper(s.Type)
	for _, child := range s.Properties {
		upperTypes(child)
	}
	if s.Items != nil {
		upperTypes(s.Items)
	}
}

func (p *Provider) encodePart(msg llm.Message, part llm.Part, pending []string, index int) (json.RawMessage, string, error) {
	if msg.Origin == p.name && len(part.ProviderMeta) > 0 {
		id, err := checkReplay(part)
		if err != nil {
			return nil, "", p.failure(llm.ErrBadRequest, "provider metadata disagrees with its normalized part", err)
		}
		return part.ProviderMeta, id, nil
	}
	var value any
	wireID := ""
	switch part.Type {
	case llm.PartOpaque:
		return nil, "", nil // Foreign provider data cannot be replayed.
	case llm.PartText:
		value = map[string]string{"text": part.Text}
	case llm.PartToolCall:
		c := part.ToolCall
		wireID = c.ID
		value = map[string]any{"functionCall": functionCall{ID: c.ID, Name: c.Name, Args: c.Args}}
	case llm.PartToolResult:
		if index >= len(pending) {
			return nil, "", p.failure(llm.ErrBadRequest, "ambiguous Gemini tool result correlation", nil)
		}
		r := part.ToolResult
		key := "output"
		if r.IsError {
			key = "error"
		}
		value = map[string]any{"functionResponse": struct {
			ID       string            `json:"id,omitempty"`
			Name     string            `json:"name"`
			Response map[string]string `json:"response"`
		}{pending[index], r.Name, map[string]string{key: r.Content}}}
	default:
		return nil, "", p.failure(llm.ErrBadRequest, "unsupported message part", nil)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, "", p.failure(llm.ErrBadRequest, "cannot encode Gemini part", err)
	}
	return raw, wireID, nil
}

func checkReplay(part llm.Part) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(part.ProviderMeta, &fields); err != nil {
		return "", err
	}
	if fields == nil {
		return "", errors.New("part metadata must be an object")
	}
	switch part.Type {
	case llm.PartText:
		var text string
		if err := json.Unmarshal(fields["text"], &text); err != nil {
			return "", err
		}
		if text != part.Text {
			return "", errors.New("text changed")
		}
	case llm.PartToolCall:
		var call functionCall
		if err := json.Unmarshal(fields["functionCall"], &call); err != nil {
			return "", err
		}
		args := call.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if call.Name != part.ToolCall.Name || (call.ID != "" && call.ID != part.ToolCall.ID) || !sameJSON(args, part.ToolCall.Args) {
			return "", errors.New("tool call changed")
		}
		return call.ID, nil
	case llm.PartOpaque:
	default:
		return "", errors.New("unsupported replay part")
	}
	return "", nil
}

func sameJSON(a, b []byte) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}
