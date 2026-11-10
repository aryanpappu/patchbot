package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
)

func request(text string) llm.ChatRequest {
	return llm.ChatRequest{Model: "fake-model", MaxTokens: 64, Messages: []llm.Message{llm.UserText(text)}}
}

// recorder is a fake.TB that keeps what AssertConsumed reports.
type recorder struct{ failures []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestScriptedResponses(t *testing.T) {
	t.Parallel()
	usage := &llm.Usage{InputTokens: 12, OutputTokens: 3}
	custom := &llm.ChatResponse{
		Message:    llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartOpaque, ProviderMeta: json.RawMessage(`{"t":1}`)}}},
		StopReason: llm.StopOther,
		RawStop:    "pause_turn",
	}
	authErr := &llm.Error{Kind: llm.ErrAuth, Provider: "fake"}

	tests := []struct {
		name    string
		step    fake.Step
		want    *llm.ChatResponse
		wantErr error
	}{
		{
			name: "text",
			step: fake.Step{Text: "all done", Usage: usage},
			want: &llm.ChatResponse{
				Message:    llm.Message{Role: llm.RoleAssistant, Origin: "fake", Parts: []llm.Part{llm.Text("all done")}},
				StopReason: llm.StopEndTurn, RawStop: "end_turn", Usage: usage, Model: "fake-model", RequestID: "fake_0",
			},
		},
		{
			name: "tool calls get IDs, empty args and the tool_use stop",
			step: fake.Step{Text: "reading", ToolCalls: []llm.ToolCall{
				{Name: "read_file", Args: json.RawMessage(`{"path":"a.go"}`)},
				{ID: "mine", Name: "list_dir"},
			}},
			want: &llm.ChatResponse{
				Message: llm.Message{Role: llm.RoleAssistant, Origin: "fake", Parts: []llm.Part{
					llm.Text("reading"),
					llm.Call("call_0_0", "read_file", json.RawMessage(`{"path":"a.go"}`)),
					llm.Call("mine", "list_dir", json.RawMessage(`{}`)),
				}},
				StopReason: llm.StopToolUse, RawStop: "tool_use", Model: "fake-model", RequestID: "fake_0",
			},
		},
		{
			name: "explicit stop reason and unknown usage",
			step: fake.Step{Text: "cut off", Stop: llm.StopMaxTokens},
			want: &llm.ChatResponse{
				Message:    llm.Message{Role: llm.RoleAssistant, Origin: "fake", Parts: []llm.Part{llm.Text("cut off")}},
				StopReason: llm.StopMaxTokens, RawStop: "max_tokens", Model: "fake-model", RequestID: "fake_0",
			},
		},
		{name: "refusal", step: fake.Step{Text: "I can't help with that.", Stop: llm.StopRefusal}, want: &llm.ChatResponse{
			Message:    llm.Message{Role: llm.RoleAssistant, Origin: "fake", Parts: []llm.Part{llm.Text("I can't help with that.")}},
			StopReason: llm.StopRefusal, RawStop: "refusal", Model: "fake-model", RequestID: "fake_0",
		}},
		{name: "full response", step: fake.Step{Response: custom}, want: custom},
		{name: "error", step: fake.Step{Err: authErr}, wantErr: authErr},
		{name: "error wins over text", step: fake.Step{Text: "ignored", Err: authErr}, wantErr: authErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := fake.New("fake", tt.step)
			got, err := p.Chat(context.Background(), request("go"))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Chat error = %v, want %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("response (-want +got):\n%s", diff)
			}
			if got != nil {
				if err := llm.Validate([]llm.Message{llm.UserText("go"), got.Message}); err != nil {
					t.Errorf("the scripted message breaks the message invariants: %v", err)
				}
			}
			p.AssertConsumed(t)
		})
	}
}

func TestResponsesAreCopies(t *testing.T) {
	t.Parallel()
	usage := &llm.Usage{InputTokens: 1}
	args := json.RawMessage(`{"a":1}`)
	custom := &llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("x")}}}
	p := fake.New("fake",
		fake.Step{ToolCalls: []llm.ToolCall{{Name: "t", Args: args}}, Usage: usage},
		fake.Step{Response: custom},
	)
	first, _ := p.Chat(context.Background(), request("1"))
	first.Usage.InputTokens = 99
	first.Message.Parts[0].ToolCall.Args[2] = 'X'
	second, _ := p.Chat(context.Background(), request("2"))
	second.Message.Parts[0].Text = "changed"
	if usage.InputTokens != 1 || string(args) != `{"a":1}` || custom.Message.Parts[0].Text != "x" {
		t.Error("changing a response changed the script")
	}
}

func TestRequestsAreRecordedAsDeepCopies(t *testing.T) {
	t.Parallel()
	p := fake.New("fake", fake.Step{Text: "1"}, fake.Step{Text: "2"})
	req := llm.ChatRequest{
		Model:     "m",
		System:    "sys",
		MaxTokens: 8,
		Messages: []llm.Message{
			llm.UserText("hi"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Call("a", "read_file", json.RawMessage(`{"path":"x"}`))}},
			{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("a", "read_file", "content", false)}},
		},
		Tools: []llm.ToolSpec{{Name: "read_file", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	want := req.Clone()

	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// The caller keeps using and changing its request, as an agent loop does.
	req.Messages[0].Parts[0].Text = "changed"
	req.Messages[1].Parts[0].ToolCall.Args[2] = 'X'
	req.Messages[2].Parts[0].ToolResult.Content = "changed"
	req.Tools[0].Schema[2] = 'X'
	req.Messages = append(req.Messages, llm.UserText("next"))
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	got := p.Requests()
	if len(got) != 2 || p.Calls() != 2 {
		t.Fatalf("%d requests recorded, Calls() = %d; want 2", len(got), p.Calls())
	}
	if diff := cmp.Diff(want, got[0]); diff != "" {
		t.Errorf("the first recorded request changed after the call (-want +got):\n%s", diff)
	}
	if len(got[1].Messages) != 4 {
		t.Errorf("the second request has %d messages, want 4", len(got[1].Messages))
	}

	// Requests() itself hands out copies.
	got[0].Messages[0].Parts[0].Text = "scribble"
	if p.Requests()[0].Messages[0].Parts[0].Text != "hi" {
		t.Error("changing the result of Requests() changed the record")
	}
}

func TestScriptExhausted(t *testing.T) {
	t.Parallel()
	p := fake.New("fake", fake.Step{Text: "only"})
	if _, err := p.Chat(context.Background(), request("1")); err != nil {
		t.Fatal(err)
	}
	_, err := p.Chat(context.Background(), request("2"))
	if !errors.Is(err, fake.ErrExhausted) || !strings.Contains(err.Error(), "unexpected call 2 with 1 messages") {
		t.Errorf("Chat past the script = %v", err)
	}
	// The unexpected request is still recorded, so a test can show it.
	if p.Calls() != 2 || p.Requests()[1].Messages[0].Text() != "2" {
		t.Errorf("the unexpected request was not recorded")
	}
	// An exhausted script is not retryable: a retry wrapper must not spin on it.
	if llm.IsRetryable(err) {
		t.Error("ErrExhausted is retryable")
	}
}

func TestAssertConsumed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		steps    int
		calls    int
		wantFail string
	}{
		{"all used", 2, 2, ""},
		{"empty script", 0, 0, ""},
		{"one left", 3, 2, `fake provider "fake": 1 of 3 scripted steps were not used (next unused: step 2)`},
		{"none used", 2, 0, `fake provider "fake": 2 of 2 scripted steps were not used (next unused: step 0)`},
		{"extra calls are not leftovers", 1, 3, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := fake.New("fake")
			for range tt.steps {
				p.Script(fake.Step{Text: "x"})
			}
			if p.Remaining() != tt.steps {
				t.Errorf("Remaining() = %d before any call, want %d", p.Remaining(), tt.steps)
			}
			for range tt.calls {
				_, _ = p.Chat(context.Background(), request("x"))
			}
			var rec recorder
			p.AssertConsumed(&rec)
			switch {
			case tt.wantFail == "" && len(rec.failures) != 0:
				t.Errorf("AssertConsumed failed: %v", rec.failures)
			case tt.wantFail != "" && (len(rec.failures) != 1 || rec.failures[0] != tt.wantFail):
				t.Errorf("AssertConsumed reported %q, want %q", rec.failures, tt.wantFail)
			}
			if want := max(tt.steps-tt.calls, 0); p.Remaining() != want {
				t.Errorf("Remaining() = %d, want %d", p.Remaining(), want)
			}
		})
	}
}

func TestExpect(t *testing.T) {
	t.Parallel()
	wantSystem := func(req llm.ChatRequest) error {
		if req.System != "be careful" {
			return fmt.Errorf("system prompt = %q", req.System)
		}
		// Changing the request inside Expect must not reach the record.
		req.Messages[0].Parts[0].Text = "scribble"
		return nil
	}
	p := fake.New("fake", fake.Step{Text: "ok", Expect: wantSystem}, fake.Step{Text: "ok", Expect: wantSystem})

	req := request("hi")
	req.System = "be careful"
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Errorf("Chat with the expected request: %v", err)
	}
	if p.Requests()[0].Messages[0].Text() != "hi" {
		t.Error("Expect changed the recorded request")
	}

	req.System = "be quick"
	_, err := p.Chat(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), `step 1: unexpected request: system prompt = "be quick"`) {
		t.Errorf("Chat with an unexpected request = %v", err)
	}
}

func TestNewFunc(t *testing.T) {
	t.Parallel()
	p := fake.NewFunc("fn", func(_ context.Context, call int, req llm.ChatRequest) (*llm.ChatResponse, error) {
		if call == 2 {
			return nil, &llm.Error{Kind: llm.ErrRateLimit, Provider: "fn"}
		}
		return &llm.ChatResponse{
			Message:    llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text(fmt.Sprintf("%d:%s", call, req.Messages[0].Text()))}},
			StopReason: llm.StopEndTurn,
		}, nil
	})
	for i, want := range []string{"0:a", "1:b"} {
		resp, err := p.Chat(context.Background(), request(string(rune('a'+i))))
		if err != nil || resp.Message.Text() != want {
			t.Errorf("call %d = %v, %v; want %q", i, resp, err, want)
		}
	}
	if _, err := p.Chat(context.Background(), request("c")); !errors.Is(err, &llm.Error{Kind: llm.ErrRateLimit}) {
		t.Errorf("third call = %v, want a rate limit error", err)
	}
	if p.Calls() != 3 || p.Remaining() != 0 {
		t.Errorf("Calls() = %d, Remaining() = %d", p.Calls(), p.Remaining())
	}
	var rec recorder
	p.AssertConsumed(&rec)
	if len(rec.failures) != 0 {
		t.Errorf("AssertConsumed on a func provider failed: %v", rec.failures)
	}
}

func TestContextIsHonoured(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := fake.New("fake", fake.Step{Text: "never"})
	if _, err := p.Chat(ctx, request("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Chat on a cancelled context = %v", err)
	}
	if _, err := p.ListModels(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ListModels on a cancelled context = %v", err)
	}
	if p.Calls() != 0 || p.Remaining() != 1 || p.ListCalls() != 0 {
		t.Error("a cancelled call used a step or was recorded")
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()
	p := fake.New("fake")
	if models, err := p.ListModels(context.Background()); err != nil || len(models) != 0 {
		t.Errorf("ListModels by default = %v, %v", models, err)
	}
	set := []llm.ModelInfo{{ID: "m1", ContextWindow: 1000}, {ID: "m2"}}
	p.SetModels(set, nil)
	set[0].ID = "changed"
	got, err := p.ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "m1" {
		t.Errorf("ListModels = %v, %v", got, err)
	}
	got[1].ID = "changed"
	if again, _ := p.ListModels(context.Background()); again[1].ID != "m2" {
		t.Error("changing a ListModels result changed the provider")
	}
	wantErr := &llm.Error{Kind: llm.ErrUnavailable, Provider: "fake"}
	p.SetModels(nil, wantErr)
	if _, err := p.ListModels(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("ListModels error = %v", err)
	}
	if p.ListCalls() != 4 || p.Name() != "fake" {
		t.Errorf("ListCalls() = %d, Name() = %q", p.ListCalls(), p.Name())
	}
}

func TestConstructor(t *testing.T) {
	t.Parallel()
	p, err := fake.Constructor(llm.ProviderConfig{Name: "demo", Kind: fake.Kind, Models: []string{"a", "b"}})
	if err != nil || p.Name() != "demo" {
		t.Fatalf("Constructor = %v, %v", p, err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0].ID != "a" || models[1].ID != "b" {
		t.Errorf("ListModels = %v, %v", models, err)
	}
	if _, err := p.Chat(context.Background(), request("x")); !errors.Is(err, fake.ErrExhausted) {
		t.Errorf("Chat on an unscripted fake = %v, want ErrExhausted", err)
	}
}

func TestConcurrentChat(t *testing.T) {
	t.Parallel()
	const n = 64
	p := fake.New("fake")
	for i := range n {
		p.Script(fake.Step{Text: fmt.Sprintf("answer %d", i)})
	}
	seen := make(chan string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			resp, err := p.Chat(context.Background(), request(fmt.Sprintf("q%d", i)))
			if err != nil {
				t.Errorf("Chat: %v", err)
				return
			}
			seen <- resp.Message.Text()
			_ = p.Requests()
		})
	}
	wg.Wait()
	close(seen)
	answers := map[string]bool{}
	for a := range seen {
		answers[a] = true
	}
	if len(answers) != n || p.Calls() != n {
		t.Errorf("%d distinct answers from %d calls, want %d of each", len(answers), p.Calls(), n)
	}
	p.AssertConsumed(t)
}

func TestFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	good := write("good.json", `[
		{"text": "I will read the file.", "tool_calls": [{"id": "c1", "name": "read_file", "args": {"path": "main.go"}}],
		 "usage": {"input_tokens": 100, "output_tokens": 20}},
		{"error": "overloaded", "message": "try again"},
		{"text": "Done.", "stop": "end_turn"}
	]`)
	p, err := fake.FromFile("demo", good)
	if err != nil {
		t.Fatal(err)
	}
	if p.Remaining() != 3 {
		t.Fatalf("Remaining() = %d, want 3", p.Remaining())
	}
	first, err := p.Chat(context.Background(), request("go"))
	if err != nil {
		t.Fatal(err)
	}
	calls := first.Message.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "c1" || string(calls[0].Args) != `{"path": "main.go"}` ||
		first.StopReason != llm.StopToolUse || first.Usage == nil || first.Usage.InputTokens != 100 {
		t.Errorf("first step = %+v", first)
	}
	_, err = p.Chat(context.Background(), request("go"))
	if !errors.Is(err, &llm.Error{Kind: llm.ErrOverloaded, Provider: "demo"}) || !strings.Contains(err.Error(), "try again") {
		t.Errorf("second step = %v, want an overloaded error", err)
	}
	third, err := p.Chat(context.Background(), request("go"))
	if err != nil || third.Message.Text() != "Done." || third.Usage != nil {
		t.Errorf("third step = %+v, %v", third, err)
	}
	p.AssertConsumed(t)

	bad := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"missing file", filepath.Join(dir, "absent.json"), "fake script:"},
		{"not json", write("bad.json", `[{"text":`), "parse:"},
		{"not an array", write("object.json", `{"text":"x"}`), "parse:"},
		{"unknown key", write("unknown.json", `[{"text":"x","delay":5}]`), "parse:"},
		{"empty step", write("empty.json", `[{"text":"x"},{}]`), "step 1 has no text, tool_calls or error"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := fake.FromFile("demo", tt.path); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("FromFile error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
