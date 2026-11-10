package llm_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
)

var emptyArgs = json.RawMessage(`{}`)

func assistant(parts ...llm.Part) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Parts: parts, Origin: "test"}
}

func toolMsg(parts ...llm.Part) llm.Message {
	return llm.Message{Role: llm.RoleTool, Parts: parts}
}

func opaque(meta string) llm.Part {
	return llm.Part{Type: llm.PartOpaque, ProviderMeta: json.RawMessage(meta)}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	user := llm.UserText("fix the bug")
	callA := llm.Call("a", "read_file", json.RawMessage(`{"path":"main.go"}`))
	callB := llm.Call("b", "list_dir", emptyArgs)
	resA := llm.Result("a", "read_file", "package main", false)
	resB := llm.Result("b", "list_dir", "no such directory", true)

	tests := []struct {
		name     string
		messages []llm.Message
		// wantErr lists substrings the error must contain; empty means valid.
		wantErr []string
	}{
		{name: "empty list", messages: nil},
		{name: "one user message", messages: []llm.Message{user}},
		{name: "user then assistant", messages: []llm.Message{user, assistant(llm.Text("done"))}},
		{
			name:     "full tool exchange",
			messages: []llm.Message{user, assistant(llm.Text("looking"), callA, callB), toolMsg(resA, resB), assistant(llm.Text("done"))},
		},
		{
			name:     "pending calls in the last assistant message",
			messages: []llm.Message{user, assistant(callA)},
		},
		{
			name:     "user feedback after tool results",
			messages: []llm.Message{user, assistant(callA), toolMsg(resA), llm.UserText("the tests failed"), assistant(llm.Text("ok"))},
		},
		{
			name: "opaque part and part metadata in an assistant message",
			messages: []llm.Message{user, assistant(
				opaque(`{"type":"thinking","signature":"abc"}`),
				llm.Part{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "a", Name: "read_file", Args: emptyArgs}, ProviderMeta: json.RawMessage(`"sig"`)},
			), toolMsg(resA)},
		},
		{
			name:     "arguments with surrounding space",
			messages: []llm.Message{user, assistant(llm.Call("a", "read_file", json.RawMessage(" {\"path\": \"x\"}\n")))},
		},

		// Rule 1: alternation.
		{name: "starts with assistant", messages: []llm.Message{assistant(llm.Text("hi"))}, wantErr: []string{"message 0: the first message must be a user message"}},
		{name: "starts with tool", messages: []llm.Message{toolMsg(resA)}, wantErr: []string{"message 0: tool message does not follow an assistant message"}},
		{name: "two user messages", messages: []llm.Message{user, llm.UserText("again")}, wantErr: []string{"message 1: user message follows a user message"}},
		{name: "two assistant messages", messages: []llm.Message{user, assistant(llm.Text("a")), assistant(llm.Text("b"))}, wantErr: []string{"message 2: assistant message follows an assistant message"}},
		{name: "two tool messages", messages: []llm.Message{user, assistant(callA, callB), toolMsg(resA), toolMsg(resB)}, wantErr: []string{`message 2: tool call "b" (list_dir) has no result`, "message 3: tool message does not follow an assistant message"}},
		{name: "two user messages after tool", messages: []llm.Message{user, assistant(callA), toolMsg(resA), llm.UserText("x"), llm.UserText("y")}, wantErr: []string{"message 4: user message follows a user message"}},
		{name: "unknown role", messages: []llm.Message{{Role: "system", Parts: []llm.Part{llm.Text("x")}}}, wantErr: []string{`message 0: unknown role "system"`}},
		{name: "empty role", messages: []llm.Message{{Parts: []llm.Part{llm.Text("x")}}}, wantErr: []string{`unknown role ""`}},

		// Rule 2: calls and results.
		{name: "orphan call before the next assistant turn", messages: []llm.Message{user, assistant(callA), llm.UserText("never mind"), assistant(llm.Text("ok"))}, wantErr: []string{"message 2: user message follows an assistant message whose tool calls have no results (a)"}},
		{name: "result without a call", messages: []llm.Message{user, assistant(llm.Text("no calls")), toolMsg(resA)}, wantErr: []string{"message 2: tool message follows an assistant message that made no tool calls"}},
		{name: "result for an unknown call", messages: []llm.Message{user, assistant(callA), toolMsg(llm.Result("zzz", "read_file", "x", false))}, wantErr: []string{`message 2: tool result 0 answers "zzz", want "a"`}},
		{name: "result for an older call", messages: []llm.Message{user, assistant(callA), toolMsg(resA), assistant(callB), toolMsg(resA)}, wantErr: []string{`message 4: tool result 0 answers "a", want "b"`}},
		{name: "missing result", messages: []llm.Message{user, assistant(callA, callB), toolMsg(resA)}, wantErr: []string{`message 2: tool call "b" (list_dir) has no result`}},
		{name: "extra result", messages: []llm.Message{user, assistant(callA), toolMsg(resA, resB)}, wantErr: []string{`message 2: tool result 1 answers "b", which is not a call of the previous assistant message`}},
		{name: "duplicate result", messages: []llm.Message{user, assistant(callA), toolMsg(resA, resA)}, wantErr: []string{`tool result 1 answers "a", which is not a call`}},
		{name: "results out of order", messages: []llm.Message{user, assistant(callA, callB), toolMsg(resB, resA)}, wantErr: []string{`tool result 0 answers "b", want "a": results must be in call order`}},
		{name: "result names another tool", messages: []llm.Message{user, assistant(callA), toolMsg(llm.Result("a", "write_file", "x", false))}, wantErr: []string{`tool result 0 for call "a" names tool "write_file", want "read_file"`}},
		{name: "result with no call id", messages: []llm.Message{user, assistant(callA), toolMsg(llm.Result("", "read_file", "x", false))}, wantErr: []string{"part 0: tool result has no call ID"}},

		// Rule 3: IDs and arguments.
		{name: "call with no id", messages: []llm.Message{user, assistant(llm.Call("", "read_file", emptyArgs))}, wantErr: []string{"tool call has no ID"}},
		{name: "duplicate id in one message", messages: []llm.Message{user, assistant(callA, callA)}, wantErr: []string{`tool call ID "a" is used twice`}},
		{name: "id reused in a later turn", messages: []llm.Message{user, assistant(callA), toolMsg(resA), assistant(callA)}, wantErr: []string{`message 3: part 0: tool call ID "a" is used twice`}},
		{name: "bad tool name", messages: []llm.Message{user, assistant(llm.Call("a", "read file", emptyArgs))}, wantErr: []string{`"read file" is not a tool name`}},
		{name: "empty tool name", messages: []llm.Message{user, assistant(llm.Call("a", "", emptyArgs))}, wantErr: []string{`"" is not a tool name`}},
		{name: "nil args", messages: []llm.Message{user, assistant(llm.Call("a", "read_file", nil))}, wantErr: []string{`tool call "a": arguments are not a JSON object`}},
		{name: "args are an array", messages: []llm.Message{user, assistant(llm.Call("a", "read_file", json.RawMessage(`[1]`)))}, wantErr: []string{"arguments are not a JSON object"}},
		{name: "args are a string", messages: []llm.Message{user, assistant(llm.Call("a", "read_file", json.RawMessage(`"{}"`)))}, wantErr: []string{"arguments are not a JSON object"}},
		{name: "args are null", messages: []llm.Message{user, assistant(llm.Call("a", "read_file", json.RawMessage(`null`)))}, wantErr: []string{"arguments are not a JSON object"}},
		{name: "args are truncated", messages: []llm.Message{user, assistant(llm.Call("a", "read_file", json.RawMessage(`{"path":`)))}, wantErr: []string{"arguments are not a JSON object"}},
		{name: "args are two objects", messages: []llm.Message{user, assistant(llm.Call("a", "read_file", json.RawMessage(`{}{}`)))}, wantErr: []string{"arguments are not a JSON object"}},

		// Parts.
		{name: "message with no parts", messages: []llm.Message{{Role: llm.RoleUser}}, wantErr: []string{"message 0: has no parts"}},
		{name: "empty text part", messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText}}}}, wantErr: []string{"part 0: a text part needs non-empty text"}},
		{name: "text part with a tool call", messages: []llm.Message{user, assistant(llm.Part{Type: llm.PartText, Text: "x", ToolCall: &llm.ToolCall{ID: "a", Name: "t", Args: emptyArgs}})}, wantErr: []string{"a text part needs non-empty text and nothing else"}},
		{name: "tool_call part with no call", messages: []llm.Message{user, assistant(llm.Part{Type: llm.PartToolCall})}, wantErr: []string{"a tool_call part needs a tool call"}},
		{name: "tool_result part with no result", messages: []llm.Message{user, assistant(callA), toolMsg(llm.Part{Type: llm.PartToolResult})}, wantErr: []string{"a tool_result part needs a tool result"}},
		{name: "unknown part type", messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: "image"}}}}, wantErr: []string{`unknown part type "image"`}},
		{name: "tool call in a user message", messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{callA}}}, wantErr: []string{"a tool_call part is not allowed in a user message"}},
		{name: "tool result in a user message", messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{resA}}}, wantErr: []string{"a tool_result part is not allowed in a user message"}},
		{name: "text in a tool message", messages: []llm.Message{user, assistant(callA), toolMsg(resA, llm.Text("note"))}, wantErr: []string{"a text part is not allowed in a tool message"}},
		{name: "tool result in an assistant message", messages: []llm.Message{user, assistant(resA)}, wantErr: []string{"a tool_result part is not allowed in an assistant message"}},
		{name: "opaque part in a user message", messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{opaque(`{}`)}}}, wantErr: []string{"a provider_meta part is not allowed in a user message"}},
		{name: "opaque part with no data", messages: []llm.Message{user, assistant(llm.Part{Type: llm.PartOpaque})}, wantErr: []string{"a provider_meta part needs provider data"}},
		{name: "opaque part with text", messages: []llm.Message{user, assistant(llm.Part{Type: llm.PartOpaque, Text: "x", ProviderMeta: json.RawMessage(`{}`)})}, wantErr: []string{"a provider_meta part needs provider data and nothing else"}},
		{name: "provider data on a user part", messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "x", ProviderMeta: json.RawMessage(`1`)}}}}, wantErr: []string{"provider data is allowed only in an assistant message"}},

		{
			name:     "every violation is reported",
			messages: []llm.Message{assistant(llm.Call("", "bad name", nil)), llm.UserText("x")},
			wantErr:  []string{"the first message must be a user message", "tool call has no ID", "is not a tool name", "arguments are not a JSON object", "message 1: user message follows an assistant message whose tool calls"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := llm.Validate(tt.messages)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate succeeded, want an error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not contain %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestMessageHelpers(t *testing.T) {
	t.Parallel()
	m := assistant(llm.Text("a"), llm.Call("1", "read_file", emptyArgs), opaque(`{}`), llm.Text("b"), llm.Call("2", "list_dir", emptyArgs))
	if got := m.Text(); got != "ab" {
		t.Errorf("Text() = %q, want ab", got)
	}
	want := []llm.ToolCall{{ID: "1", Name: "read_file", Args: emptyArgs}, {ID: "2", Name: "list_dir", Args: emptyArgs}}
	if diff := cmp.Diff(want, m.ToolCalls()); diff != "" {
		t.Errorf("ToolCalls() (-want +got):\n%s", diff)
	}
	if llm.UserText("x").ToolCalls() != nil {
		t.Error("ToolCalls() of a text message is not nil")
	}
}

func TestCloneIsDeep(t *testing.T) {
	t.Parallel()
	temp := 0.5
	orig := llm.ChatRequest{
		Model:  "m",
		System: "s",
		Messages: []llm.Message{
			llm.UserText("hi"),
			assistant(llm.Call("a", "read_file", json.RawMessage(`{"path":"a"}`)), opaque(`{"sig":"1"}`)),
			toolMsg(llm.Result("a", "read_file", "content", false)),
		},
		Tools:       []llm.ToolSpec{{Name: "read_file", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:   10,
		Temperature: &temp,
	}
	clone := orig.Clone()
	if diff := cmp.Diff(orig, clone); diff != "" {
		t.Fatalf("Clone differs (-orig +clone):\n%s", diff)
	}

	// Change every reachable piece of the clone; the original must not move.
	clone.Messages[0].Parts[0].Text = "changed"
	clone.Messages[1].Parts[0].ToolCall.Name = "changed"
	clone.Messages[1].Parts[0].ToolCall.Args[2] = 'X'
	clone.Messages[1].Parts[1].ProviderMeta[2] = 'X'
	clone.Messages[2].Parts[0].ToolResult.Content = "changed"
	clone.Messages = append(clone.Messages, llm.UserText("more"))
	clone.Tools[0].Schema[2] = 'X'
	clone.Tools[0].Name = "changed"
	*clone.Temperature = 9

	if orig.Messages[0].Parts[0].Text != "hi" ||
		orig.Messages[1].Parts[0].ToolCall.Name != "read_file" ||
		string(orig.Messages[1].Parts[0].ToolCall.Args) != `{"path":"a"}` ||
		string(orig.Messages[1].Parts[1].ProviderMeta) != `{"sig":"1"}` ||
		orig.Messages[2].Parts[0].ToolResult.Content != "content" ||
		len(orig.Messages) != 3 ||
		string(orig.Tools[0].Schema) != `{"type":"object"}` || orig.Tools[0].Name != "read_file" ||
		*orig.Temperature != 0.5 {
		t.Errorf("changing the clone changed the original: %+v", orig)
	}

	if llm.CloneMessages(nil) != nil {
		t.Error("CloneMessages(nil) is not nil")
	}
	var nilResp *llm.ChatResponse
	if nilResp.Clone() != nil {
		t.Error("Clone of a nil response is not nil")
	}
	resp := &llm.ChatResponse{Message: assistant(llm.Text("x")), Usage: &llm.Usage{InputTokens: 1}}
	respClone := resp.Clone()
	respClone.Usage.InputTokens = 99
	respClone.Message.Parts[0].Text = "changed"
	if resp.Usage.InputTokens != 1 || resp.Message.Parts[0].Text != "x" {
		t.Error("changing a cloned response changed the original")
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	a := &llm.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4}
	b := &llm.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40}
	var unknown *llm.Usage

	tests := []struct {
		name      string
		a, b      *llm.Usage
		want      *llm.Usage
		wantTotal int64
	}{
		{"sum", a, b, &llm.Usage{InputTokens: 11, OutputTokens: 22, CacheReadTokens: 33, CacheWriteTokens: 44}, 110},
		{"zero is known", a, &llm.Usage{}, a, 10},
		{"unknown left", unknown, b, nil, 0},
		{"unknown right", a, unknown, nil, 0},
		{"both unknown", unknown, unknown, nil, 0},
		{"saturates", &llm.Usage{InputTokens: math.MaxInt64}, &llm.Usage{InputTokens: 5, OutputTokens: 1}, &llm.Usage{InputTokens: math.MaxInt64, OutputTokens: 1}, math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.a.Add(tt.b)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Add (-want +got):\n%s", diff)
			}
			if total := got.Total(); total != tt.wantTotal {
				t.Errorf("Total() = %d, want %d", total, tt.wantTotal)
			}
		})
	}
	if a.InputTokens != 1 || b.InputTokens != 10 {
		t.Error("Add changed an operand")
	}
}

func TestChatRequestValidate(t *testing.T) {
	t.Parallel()
	tool := llm.ToolSpec{Name: "read_file", Description: "Read a file.", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}
	valid := llm.ChatRequest{Model: "m", MaxTokens: 100, Messages: []llm.Message{llm.UserText("hi")}, Tools: []llm.ToolSpec{tool}}

	tests := []struct {
		name    string
		change  func(r *llm.ChatRequest)
		wantErr string
	}{
		{"valid", func(*llm.ChatRequest) {}, ""},
		{"ends with tool results", func(r *llm.ChatRequest) {
			r.Messages = []llm.Message{llm.UserText("hi"), assistant(llm.Call("a", "read_file", emptyArgs)), toolMsg(llm.Result("a", "read_file", "x", false))}
		}, ""},
		{"no model", func(r *llm.ChatRequest) { r.Model = "" }, "no model"},
		{"no output cap", func(r *llm.ChatRequest) { r.MaxTokens = 0 }, "max tokens is 0"},
		{"no messages", func(r *llm.ChatRequest) { r.Messages = nil }, "no messages"},
		{"ends with assistant", func(r *llm.ChatRequest) { r.Messages = append(r.Messages, assistant(llm.Text("x"))) }, "the last message is from the assistant"},
		{"ends with unanswered calls", func(r *llm.ChatRequest) {
			r.Messages = append(r.Messages, assistant(llm.Call("a", "read_file", emptyArgs)))
		}, "the last message is from the assistant"},
		{"broken history", func(r *llm.ChatRequest) { r.Messages = append(r.Messages, llm.UserText("again")) }, "user message follows a user message"},
		{"bad tool", func(r *llm.ChatRequest) { r.Tools[0].Schema = json.RawMessage(`{"type":"string"}`) }, `tool 0: tool read_file: schema: the root type is "string", want object`},
		{"duplicate tool", func(r *llm.ChatRequest) { r.Tools = append(r.Tools, r.Tools[0]) }, `tool 1: name "read_file" is used twice`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := valid.Clone()
			tt.change(&req)
			err := req.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
