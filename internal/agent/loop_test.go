package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

type recorder struct {
	steps  []store.Step
	events []string
	fail   bool
}

func (r *recorder) Record(_ context.Context, s store.Step) error {
	if r.fail {
		return errors.New("disk failed")
	}
	r.steps = append(r.steps, s)
	return nil
}
func (r *recorder) Event(_ context.Context, kind string, data json.RawMessage) error {
	if !json.Valid(data) {
		return errors.New("invalid event JSON")
	}
	r.events = append(r.events, kind+" "+string(data))
	return nil
}

type executor struct {
	specs []execx.Spec
	run   func(context.Context, execx.Spec) (execx.Result, error)
}

func (e *executor) Run(ctx context.Context, spec execx.Spec) (execx.Result, error) {
	e.specs = append(e.specs, spec)
	if e.run != nil {
		return e.run(ctx, spec)
	}
	return execx.Result{Output: "harness verified", ExitCode: 0}, nil
}

type testClock struct {
	now    time.Time
	sleeps int
}

func (c *testClock) Now() time.Time { return c.now }
func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	c.sleeps++
	return ctx.Err()
}

func call(name string, value any) llm.ToolCall {
	raw, _ := json.Marshal(value)
	return llm.ToolCall{Name: name, Args: raw}
}
func turn(calls ...llm.ToolCall) fake.Step {
	return fake.Step{ToolCalls: calls, Usage: &llm.Usage{InputTokens: 20, OutputTokens: 10}}
}
func reportStep() fake.Step {
	return turn(call("submit_report", map[string]any{"report_md": "Evidence in README.md."}))
}

func setup(t *testing.T, steps ...fake.Step) (*Loop, *fake.Provider, *recorder, *executor) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "repo")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Open(dir, workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(true) })
	if err := ws.WriteFile("README.md", []byte("before")); err != nil {
		t.Fatal(err)
	}
	policy, err := execx.NewPolicy(dir, map[string][]string{"test": {"/usr/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{WS: ws, RunDir: base, Policy: policy, Redactor: secret.NewRedactor()}
	ex := &executor{}
	env.Exec = ex
	registry, err := newInvestigateRegistry(env)
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New("fake", steps...)
	r := &recorder{}
	l := &Loop{Provider: p, Model: "model", tools: registry, Env: env, Pricing: llm.Pricing{Free: true}, Recorder: r, RetryClock: &testClock{now: time.Now()}}
	return l, p, r, ex
}
func session() *Session {
	return &Session{System: "test", Messages: []llm.Message{llm.UserText("investigate")}}
}
func validRequests(t *testing.T, p *fake.Provider) {
	t.Helper()
	for i, req := range p.Requests() {
		if err := req.Validate(); err != nil {
			t.Fatalf("request %d invalid: %v", i, err)
		}
	}
}

func TestLoopErrorResultsAndTerminalGroup(t *testing.T) {
	t.Parallel()
	l, p, r, _ := setup(t,
		turn(call("unknown", map[string]any{}), call("read_file", map[string]any{"path": 3}), llm.ToolCall{Name: "read_file", Args: json.RawMessage(`{"path":`)}, llm.ToolCall{Name: "read_file", Args: json.RawMessage(`[]`)}, llm.ToolCall{Name: "read_file", Args: json.RawMessage(`null`)}),
		turn(call("submit_report", map[string]any{"report_md": "done"}), call("write_file", map[string]any{"path": "README.md", "content": "unsafe"})),
	)
	s := session()
	out, err := l.Run(t.Context(), s, Limits{})
	if err != nil || out.Reason != EndSubmitted || out.Submission.Report != "done" {
		t.Fatalf("run = %+v, %v", out, err)
	}
	validRequests(t, p)
	results := p.Requests()[1].Messages[2].Parts
	if len(results) != 5 {
		t.Fatalf("results = %d", len(results))
	}
	for _, part := range results {
		if !part.ToolResult.IsError {
			t.Fatalf("expected error result: %+v", part)
		}
	}
	if llm.Validate(s.Messages) != nil {
		t.Fatal("terminal batch lost call/result pairs")
	}
	data, err := l.Env.WS.ReadFile("README.md")
	if err != nil || string(data) != "before" {
		t.Fatal("call after terminal executed")
	}
	if len(r.steps) != 9 {
		t.Fatalf("steps = %d", len(r.steps))
	}
}

func TestLoopStops(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		lim    Limits
		reason EndReason
		seed   int
	}{
		{name: "turns", lim: Limits{MaxTurns: 1}, reason: EndMaxTurns, seed: 1},
		{name: "tokens", lim: Limits{MaxTotalTokens: 1}, reason: EndBudget},
		{name: "cost", lim: Limits{MaxCostMicros: 1}, reason: EndBudget},
		{name: "deadline", lim: Limits{Deadline: time.Now().Add(-time.Minute)}, reason: EndDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, p, _, _ := setup(t)
			l.Pricing = llm.Pricing{Models: map[string]llm.Price{"model": {Input: 1_000_000, Output: 1_000_000}}}
			s := session()
			s.Turns = tc.seed
			out, err := l.Run(t.Context(), s, tc.lim)
			if err != nil || out.Reason != tc.reason || p.Calls() != 0 {
				t.Fatalf("out=%+v err=%v calls=%d", out, err, p.Calls())
			}
		})
	}
	l, p, _, _ := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Run(ctx, session(), Limits{}); !errors.Is(err, context.Canceled) || p.Calls() != 0 {
		t.Fatalf("cancel=%v", err)
	}
}

func TestLoopRefusalAndTruncation(t *testing.T) {
	t.Parallel()
	for _, reason := range []llm.StopReason{llm.StopRefusal, llm.StopMaxTokens} {
		t.Run(string(reason), func(t *testing.T) {
			cut := fake.Step{Stop: reason, ToolCalls: []llm.ToolCall{{Name: "write_file", Args: json.RawMessage(`{"path":"README.md","content":`)}}}
			l, p, _, _ := setup(t, cut, submitStep())
			token, _, run := approved(t)
			reg, err := newCodeRegistry(t.Context(), token, l.Env, run.ID, IssueHash("title", "body"), draftSpec().AcceptanceCriteria)
			if err != nil {
				t.Fatal(err)
			}
			l.tools = reg
			out, err := l.Run(t.Context(), session(), Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if reason == llm.StopRefusal && (out.Reason != EndRefusal || p.Calls() != 1) {
				t.Fatalf("refusal=%+v", out)
			}
			if reason == llm.StopMaxTokens && (out.Reason != EndSubmitted || len(p.Requests()[1].Messages[1].ToolCalls()) != 0) {
				t.Fatal("truncated tool call survived")
			}
			data, _ := l.Env.WS.ReadFile("README.md")
			if string(data) != "before" {
				t.Fatal("truncated/refused call executed")
			}
			validRequests(t, p)
		})
	}
}

func TestLoopNudgeSynthesisAndStuck(t *testing.T) {
	t.Parallel()
	t.Run("synthesis", func(t *testing.T) {
		l, p, _, _ := setup(t, fake.Step{Text: "finished"}, fake.Step{Text: "final answer"})
		out, err := l.Run(t.Context(), session(), Limits{})
		if err != nil || !out.Synthesized || out.Submission.Summary != "final answer" {
			t.Fatalf("%+v %v", out, err)
		}
		validRequests(t, p)
	})
	t.Run("stuck", func(t *testing.T) {
		steps := make([]fake.Step, 4)
		for i := range steps {
			steps[i] = turn(call("read_file", map[string]any{"path": "README.md"}))
		}
		l, p, _, _ := setup(t, steps...)
		out, err := l.Run(t.Context(), session(), Limits{})
		if err != nil || out.Reason != EndStuck || !strings.Contains(p.Requests()[3].Messages[len(p.Requests()[3].Messages)-1].Text(), "repeated three") {
			t.Fatalf("%+v %v", out, err)
		}
		validRequests(t, p)
	})
}

func TestLoopProviderErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind  llm.ErrorKind
		calls int
		want  string
	}{
		{llm.ErrAuth, 1, "provider_auth"}, {llm.ErrBadRequest, 1, "provider_bad_request"},
		{llm.ErrContextTooLong, 2, "context_overflow"}, {llm.ErrMalformed, 2, "provider_malformed"},
		{llm.ErrRateLimit, 4, "provider_unavailable"}, {llm.ErrOverloaded, 4, "provider_unavailable"}, {llm.ErrUnavailable, 4, "provider_unavailable"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			l, p, r, _ := setup(t)
			p.Script(fake.Step{Err: &llm.Error{Kind: tc.kind}}, fake.Step{Err: &llm.Error{Kind: tc.kind}}, fake.Step{Err: &llm.Error{Kind: tc.kind}}, fake.Step{Err: &llm.Error{Kind: tc.kind}})
			_, err := l.Run(t.Context(), session(), Limits{})
			var classified *Error
			if !errors.As(err, &classified) || classified.Kind != tc.want || p.Calls() != tc.calls || len(r.steps) != tc.calls {
				t.Fatalf("err=%v calls=%d records=%d", err, p.Calls(), len(r.steps))
			}
		})
	}
}

func oldHistory() []llm.Message {
	messages := []llm.Message{llm.UserText("task")}
	for i := range 9 {
		id := fmt.Sprintf("old-%d", i)
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Call(id, "read_file", json.RawMessage(`{"path":"README.md"}`)), {Type: llm.PartOpaque, ProviderMeta: json.RawMessage(`{"signature":"unchanged"}`)}}}, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result(id, "read_file", strings.Repeat("old output", 100), false)}})
	}
	return messages
}
func TestLoopCompaction(t *testing.T) {
	t.Parallel()
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprint(force), func(t *testing.T) {
			l, p, _, _ := setup(t)
			if force {
				p.Script(fake.Step{Err: &llm.Error{Kind: llm.ErrContextTooLong}})
			}
			p.Script(reportStep())
			s := session()
			s.Messages = oldHistory()
			s.LastInputTokens = 800
			lim := Limits{ContextWindow: 1000}
			if force {
				lim.ContextWindow = 0
			}
			out, err := l.Run(t.Context(), s, lim)
			if err != nil || out.Reason != EndSubmitted {
				t.Fatalf("%+v %v", out, err)
			}
			reqs := p.Requests()
			last := reqs[len(reqs)-1]
			removed := 0
			for _, msg := range last.Messages {
				for _, part := range msg.Parts {
					if part.ToolResult != nil && strings.Contains(part.ToolResult.Content, "output removed") {
						removed++
					}
					if part.Type == llm.PartOpaque && string(part.ProviderMeta) != `{"signature":"unchanged"}` {
						t.Fatal("opaque data changed")
					}
				}
			}
			want := 3
			if force {
				want = 6
			}
			if removed != want {
				t.Fatalf("compacted %d results, want %d", removed, want)
			}
			validRequests(t, p)
		})
	}
}

func TestLoopUnknownUsageAndPrices(t *testing.T) {
	t.Parallel()
	l, p, r, _ := setup(t, fake.Step{ToolCalls: reportStep().ToolCalls})
	l.Pricing = llm.Pricing{Models: map[string]llm.Price{"model": {Input: 1_000_000, Output: 2_000_000}}}
	s := session()
	out, err := l.Run(t.Context(), s, Limits{MaxCostMicros: 100_000})
	if err != nil || out.Reason != EndSubmitted || !s.AccountingUncertain || s.Usage != nil || s.Cost <= 0 || s.Tokens <= 0 || r.steps[0].CostMicros != nil {
		t.Fatalf("%+v session=%+v err=%v", out, s, err)
	}
	if p.Calls() != 1 {
		t.Fatal("calls")
	}
	// A second call is denied using the retained charge, even though no usage
	// was reported. Use the first reservation as the entire remaining cap.
	l.Provider = fake.New("fake", reportStep())
	s.Messages = []llm.Message{llm.UserText("continue")}
	out, err = l.Run(t.Context(), s, Limits{MaxCostMicros: s.Cost})
	if err != nil || out.Reason != EndBudget || l.Provider.(*fake.Provider).Calls() != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	l, p, r, _ = setup(t, reportStep())
	l.Pricing = llm.Pricing{}
	s = session()
	out, err = l.Run(t.Context(), s, Limits{MaxCostMicros: 1})
	if err != nil || out.Reason != EndSubmitted || !s.Unpriced || r.steps[0].CostMicros != nil || p.Calls() != 1 {
		t.Fatalf("unpriced %+v %v", out, err)
	}
}

func TestLoopSecretsNeverReachRecorderOrNextRequest(t *testing.T) {
	t.Parallel()
	const key = "private-password-with-newline\nsecond-line"
	l, p, r, ex := setup(t, turn(call("run_cmd", map[string]any{"name": "test"})), submitStep())
	l.Env.Redactor = secret.NewRedactor(key)
	ex.run = func(context.Context, execx.Spec) (execx.Result, error) {
		return execx.Result{Output: strings.Repeat("a", 1900) + key + strings.Repeat("a", 6200) + key + strings.Repeat("z", 280000), ExitCode: 1}, nil
	}
	token, _, run := approved(t)
	registry, err := newCodeRegistry(t.Context(), token, l.Env, run.ID, IssueHash("title", "body"), draftSpec().AcceptanceCriteria)
	if err != nil {
		t.Fatal(err)
	}
	l.tools = registry
	s := session()
	_, err = l.Run(t.Context(), s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	all, _ := json.Marshal(r.steps)
	events := strings.Join(r.events, "\n")
	next, _ := json.Marshal(p.Requests()[1])
	for _, data := range []string{string(all), events, string(next)} {
		if strings.Contains(data, "private-password") || strings.Contains(data, "second-line") {
			t.Fatal("secret fragment leaked")
		}
		if !strings.Contains(data, "redacted") {
			t.Fatal("redaction missing")
		}
	}
	var command *store.Step
	for i := range r.steps {
		if r.steps[i].Tool == "run_cmd" {
			command = &r.steps[i]
		}
	}
	if command == nil || len(command.OutputText) > 256<<10 || len(command.OutputText) < 8<<10 {
		t.Fatal("full capped record was not retained")
	}
	validRequests(t, p)
	for _, msg := range p.Requests()[1].Messages {
		for _, part := range msg.Parts {
			if part.ToolResult != nil && part.ToolResult.Name == "run_cmd" && len(part.ToolResult.Content) > 8<<10 {
				t.Fatal("encoded command output exceeded 8 KB")
			}
		}
	}
}

func TestLoopPolicyAndRecorderFailure(t *testing.T) {
	t.Parallel()
	steps := []fake.Step{}
	for i := range 5 {
		steps = append(steps, turn(call("read_file", map[string]any{"path": fmt.Sprintf("../secret-%d", i)})))
	}
	l, _, r, _ := setup(t, steps...)
	_, err := l.Run(t.Context(), session(), Limits{})
	var classified *Error
	if !errors.As(err, &classified) || classified.Kind != "policy_violations" {
		t.Fatalf("%v", err)
	}
	count := 0
	for _, ev := range r.events {
		if strings.HasPrefix(ev, "security.denied ") {
			count++
		}
	}
	if count != 5 {
		t.Fatalf("denials=%d", count)
	}
	l, p, r, _ := setup(t, turn(call("read_file", map[string]any{"path": "README.md"})))
	r.fail = true
	_, err = l.Run(t.Context(), session(), Limits{})
	if !errors.As(err, &classified) || classified.Kind != "internal" || p.Calls() != 1 || len(r.steps) != 0 {
		t.Fatalf("%v", err)
	}
}

func TestLoopMalformedReplyRetriedOnce(t *testing.T) {
	t.Parallel()
	bad := fake.Step{Response: &llm.ChatResponse{Message: llm.UserText("wrong role"), StopReason: llm.StopEndTurn}}
	l, p, r, _ := setup(t, bad, reportStep())
	out, err := l.Run(t.Context(), session(), Limits{})
	if err != nil || out.Reason != EndSubmitted || p.Calls() != 2 || r.steps[0].Status != "error" {
		t.Fatalf("%+v %v", out, err)
	}
	validRequests(t, p)
}

func TestLoopRetryChecksBudgetAndTurnLimit(t *testing.T) {
	t.Parallel()
	for _, lim := range []Limits{{MaxTurns: 1}, {MaxTotalTokens: 20_000}} {
		l, p, r, _ := setup(t, fake.Step{Err: &llm.Error{Kind: llm.ErrUnavailable}})
		out, err := l.Run(t.Context(), session(), lim)
		want := EndMaxTurns
		if lim.MaxTurns == 0 {
			want = EndBudget
		}
		if err != nil || out.Reason != want || p.Calls() != 1 || len(r.steps) != 1 {
			t.Fatalf("%+v %v calls=%d", out, err, p.Calls())
		}
	}
}
