// Package llm defines the provider-neutral types every model adapter maps to
// and from: messages, tool calls, usage, errors, retries, pricing and the
// model registry. It knows nothing about any one provider's wire format.
package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Role says who wrote a message. There is no system role: the system prompt
// is a field of ChatRequest, because most provider APIs take it outside the
// message list.
type Role string

// The roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // carries only tool_result parts
)

// PartType says which field of a Part is set.
type PartType string

// The part types.
const (
	PartText       PartType = "text"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	// PartOpaque is a provider block that must be sent back unchanged, such
	// as an Anthropic thinking block. Its content is in ProviderMeta.
	PartOpaque PartType = "provider_meta"
)

// Part is one ordered piece of a message.
type Part struct {
	Type       PartType    `json:"type"`
	Text       string      `json:"text,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	// ProviderMeta is written by an adapter and read only by the same
	// adapter. On an opaque part it is the whole block. On a text or
	// tool_call part it is extra data tied to that part, such as a Gemini
	// thought signature. Nothing else may read, change or drop it.
	ProviderMeta json.RawMessage `json:"provider_meta,omitempty"`
}

// Message is one turn of a conversation.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
	// Origin is the provider instance that produced an assistant message.
	// An adapter drops opaque parts whose Origin is not its own name.
	Origin string `json:"origin,omitempty"`
}

// ToolCall is the model asking for a tool to run.
type ToolCall struct {
	// ID is the provider's ID, or one the adapter made up when the provider
	// gave none. It is unique within a conversation.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Args is always a JSON object; "{}" when the provider sent nothing.
	Args json.RawMessage `json:"args"`
}

// ToolResult is the answer to one ToolCall.
type ToolResult struct {
	CallID string `json:"call_id"`
	// Name repeats the tool name, because some providers match by name.
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Schema is the JSON Schema of the arguments, in the subset that
	// ParseSchema accepts. Its root type is object.
	Schema json.RawMessage `json:"schema"`
}

// StopReason says why the model stopped.
type StopReason string

// The stop reasons.
const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal" // safety block, content filter or refusal
	StopOther     StopReason = "other"
)

// Usage is the token count of one call or the sum of several. A provider
// that reports no usage yields a nil *Usage, which means unknown, not zero.
type Usage struct {
	// InputTokens excludes cached tokens.
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens includes reasoning tokens.
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Add returns the sum of u and o. A nil value is unknown, and a sum with an
// unknown part is unknown, so Add returns nil if either side is nil.
func (u *Usage) Add(o *Usage) *Usage {
	if u == nil || o == nil {
		return nil
	}
	return &Usage{
		InputTokens:      satAdd(u.InputTokens, o.InputTokens),
		OutputTokens:     satAdd(u.OutputTokens, o.OutputTokens),
		CacheReadTokens:  satAdd(u.CacheReadTokens, o.CacheReadTokens),
		CacheWriteTokens: satAdd(u.CacheWriteTokens, o.CacheWriteTokens),
	}
}

// Total returns every token counted once. It is 0 for a nil Usage.
func (u *Usage) Total() int64 {
	if u == nil {
		return 0
	}
	return satAdd(satAdd(u.InputTokens, u.OutputTokens), satAdd(u.CacheReadTokens, u.CacheWriteTokens))
}

// Text returns a text part.
func Text(s string) Part { return Part{Type: PartText, Text: s} }

// Call returns a tool_call part.
func Call(id, name string, args json.RawMessage) Part {
	return Part{Type: PartToolCall, ToolCall: &ToolCall{ID: id, Name: name, Args: args}}
}

// Result returns a tool_result part.
func Result(callID, name, content string, isError bool) Part {
	return Part{Type: PartToolResult, ToolResult: &ToolResult{CallID: callID, Name: name, Content: content, IsError: isError}}
}

// UserText returns a user message with one text part.
func UserText(s string) Message {
	return Message{Role: RoleUser, Parts: []Part{Text(s)}}
}

// Text returns the message's text parts joined together.
func (m Message) Text() string {
	var b bytes.Buffer
	for _, p := range m.Parts {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls returns the message's tool calls in order.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, p := range m.Parts {
		if p.Type == PartToolCall && p.ToolCall != nil {
			calls = append(calls, *p.ToolCall)
		}
	}
	return calls
}

// Clone returns a deep copy of the message.
func (m Message) Clone() Message {
	out := m
	if m.Parts != nil {
		out.Parts = make([]Part, len(m.Parts))
		for i, p := range m.Parts {
			out.Parts[i] = p.Clone()
		}
	}
	return out
}

// Clone returns a deep copy of the part.
func (p Part) Clone() Part {
	out := p
	if p.ToolCall != nil {
		call := *p.ToolCall
		call.Args = slices.Clone(p.ToolCall.Args)
		out.ToolCall = &call
	}
	if p.ToolResult != nil {
		result := *p.ToolResult
		out.ToolResult = &result
	}
	out.ProviderMeta = slices.Clone(p.ProviderMeta)
	return out
}

// CloneMessages returns a deep copy of a message list.
func CloneMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	out := make([]Message, len(messages))
	for i, m := range messages {
		out[i] = m.Clone()
	}
	return out
}

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Validate checks the invariants every adapter relies on:
//
//  1. The list starts with a user message, and turns alternate between the
//     user side (user or tool) and the assistant. The one exception is a user
//     message directly after a tool message, which is how extra instructions
//     follow tool results.
//  2. A tool message comes only directly after an assistant message that
//     made tool calls, and it answers every call exactly once, in the same
//     order, with the same ID and name. No call is left without a result
//     before the next assistant turn. Calls in a final assistant message are
//     allowed, because their results have not been produced yet.
//  3. Tool call IDs are unique in the list, and arguments are JSON objects.
//
// It returns every violation, joined with errors.Join.
func Validate(messages []Message) error {
	var errs []error
	addf := func(i int, format string, args ...any) {
		errs = append(errs, fmt.Errorf("message %d: %s", i, fmt.Sprintf(format, args...)))
	}
	seenIDs := map[string]bool{}
	var pending []ToolCall // calls of the previous assistant message

	for i, m := range messages {
		for _, err := range validateParts(m, seenIDs) {
			addf(i, "%v", err)
		}
		var prev Role
		if i > 0 {
			prev = messages[i-1].Role
		}
		switch m.Role {
		case RoleUser:
			switch {
			case prev == RoleUser:
				addf(i, "user message follows a user message")
			case prev == RoleAssistant && len(pending) > 0:
				addf(i, "user message follows an assistant message whose tool calls have no results (%s)", callIDs(pending))
			}
			pending = nil
		case RoleTool:
			switch {
			case prev != RoleAssistant:
				addf(i, "tool message does not follow an assistant message")
			case len(pending) == 0:
				addf(i, "tool message follows an assistant message that made no tool calls")
			default:
				for _, err := range matchResults(pending, m) {
					addf(i, "%v", err)
				}
			}
			pending = nil
		case RoleAssistant:
			switch prev {
			case "":
				addf(i, "the first message must be a user message")
			case RoleAssistant:
				addf(i, "assistant message follows an assistant message")
			}
			pending = m.ToolCalls()
		default:
			addf(i, "unknown role %q", m.Role)
			pending = nil
		}
	}
	return errors.Join(errs...)
}

// validateParts checks one message on its own.
func validateParts(m Message, seenIDs map[string]bool) []error {
	var errs []error
	addf := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(m.Parts) == 0 {
		addf("has no parts")
	}
	for j, p := range m.Parts {
		set := 0
		for _, isSet := range []bool{p.Text != "", p.ToolCall != nil, p.ToolResult != nil} {
			if isSet {
				set++
			}
		}
		allowed := false
		switch p.Type {
		case PartText:
			allowed = m.Role == RoleUser || m.Role == RoleAssistant
			if p.Text == "" || set != 1 {
				addf("part %d: a text part needs non-empty text and nothing else", j)
			}
		case PartToolCall:
			allowed = m.Role == RoleAssistant
			if p.ToolCall == nil || set != 1 {
				addf("part %d: a tool_call part needs a tool call and nothing else", j)
				break
			}
			for _, err := range validateCall(*p.ToolCall, seenIDs) {
				addf("part %d: %v", j, err)
			}
		case PartToolResult:
			allowed = m.Role == RoleTool
			switch {
			case p.ToolResult == nil || set != 1:
				addf("part %d: a tool_result part needs a tool result and nothing else", j)
			case p.ToolResult.CallID == "":
				addf("part %d: tool result has no call ID", j)
			}
		case PartOpaque:
			allowed = m.Role == RoleAssistant
			if set != 0 || len(p.ProviderMeta) == 0 {
				addf("part %d: a provider_meta part needs provider data and nothing else", j)
			}
		default:
			addf("part %d: unknown part type %q", j, p.Type)
			continue
		}
		if !allowed && isKnownRole(m.Role) {
			addf("part %d: a %s part is not allowed in %s message", j, p.Type, article(m.Role))
		}
		if len(p.ProviderMeta) > 0 && m.Role != RoleAssistant {
			addf("part %d: provider data is allowed only in an assistant message", j)
		}
	}
	return errs
}

func validateCall(c ToolCall, seenIDs map[string]bool) []error {
	var errs []error
	switch {
	case c.ID == "":
		errs = append(errs, errors.New("tool call has no ID"))
	case seenIDs[c.ID]:
		errs = append(errs, fmt.Errorf("tool call ID %q is used twice", c.ID))
	default:
		seenIDs[c.ID] = true
	}
	if !toolNameRE.MatchString(c.Name) {
		errs = append(errs, fmt.Errorf("tool call %q: %q is not a tool name", c.ID, c.Name))
	}
	if !isJSONObject(c.Args) {
		errs = append(errs, fmt.Errorf("tool call %q: arguments are not a JSON object", c.ID))
	}
	return errs
}

// matchResults checks that a tool message answers exactly the pending calls.
func matchResults(pending []ToolCall, m Message) []error {
	var results []ToolResult
	for _, p := range m.Parts {
		if p.Type == PartToolResult && p.ToolResult != nil {
			results = append(results, *p.ToolResult)
		}
	}
	var errs []error
	for k := range max(len(pending), len(results)) {
		switch {
		case k >= len(results):
			errs = append(errs, fmt.Errorf("tool call %q (%s) has no result", pending[k].ID, pending[k].Name))
		case k >= len(pending):
			errs = append(errs, fmt.Errorf("tool result %d answers %q, which is not a call of the previous assistant message",
				k, results[k].CallID))
		case results[k].CallID != pending[k].ID:
			errs = append(errs, fmt.Errorf("tool result %d answers %q, want %q: results must be in call order",
				k, results[k].CallID, pending[k].ID))
		case results[k].Name != pending[k].Name:
			errs = append(errs, fmt.Errorf("tool result %d for call %q names tool %q, want %q",
				k, results[k].CallID, results[k].Name, pending[k].Name))
		}
	}
	return errs
}

func article(r Role) string {
	if r == RoleAssistant {
		return "an " + string(r)
	}
	return "a " + string(r)
}

func isKnownRole(r Role) bool {
	return r == RoleUser || r == RoleAssistant || r == RoleTool
}

func callIDs(calls []ToolCall) string {
	var b bytes.Buffer
	for i, c := range calls {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.ID)
	}
	return b.String()
}

// isJSONObject reports whether raw is exactly one valid JSON object.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

func satAdd(a, b int64) int64 {
	sum := a + b
	if a > 0 && b > 0 && sum < 0 {
		return 1<<63 - 1
	}
	return sum
}
