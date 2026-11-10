// Package fake is a scripted llm.Provider. It makes agent tests
// deterministic and free: a test lists the responses the "model" gives, runs
// the code under test, then checks the requests that code sent.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/aryanpappu/patchbot/internal/llm"
)

// Kind is the provider kind the fake registers under.
const Kind = "fake"

// ErrExhausted is returned by Chat when the script has no step left.
var ErrExhausted = errors.New("fake provider: script exhausted")

// Step is one scripted answer to a Chat call.
type Step struct {
	// Text and ToolCalls build the assistant message, text first. A tool
	// call with no ID gets "call_<step>_<index>", and nil Args become "{}".
	Text      string
	ToolCalls []llm.ToolCall
	// Stop defaults to tool_use when there are tool calls, else end_turn.
	Stop llm.StopReason
	// Usage is returned as it is; nil means the provider reported none.
	Usage *llm.Usage
	// Response, if set, is returned in place of the fields above, for a test
	// that needs full control, such as opaque parts or a malformed message.
	Response *llm.ChatResponse
	// Err is returned instead of a response.
	Err error
	// Expect, if set, checks the incoming request. Its error fails the call.
	Expect func(req llm.ChatRequest) error
}

// Func answers a Chat call. call counts from 0.
type Func func(ctx context.Context, call int, req llm.ChatRequest) (*llm.ChatResponse, error)

// TB is the part of testing.TB that AssertConsumed needs.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// Provider is a scripted llm.Provider. It is safe for concurrent use.
type Provider struct {
	name string
	fn   Func

	mu        sync.Mutex
	steps     []Step
	next      int
	requests  []llm.ChatRequest
	models    []llm.ModelInfo
	modelsErr error
	listCalls int
}

var _ llm.Provider = (*Provider)(nil)

// New returns a provider that answers Chat calls with the steps in order.
func New(name string, steps ...Step) *Provider {
	return &Provider{name: name, steps: slices.Clone(steps)}
}

// NewFunc returns a provider that answers every Chat call with fn. It has no
// script, so AssertConsumed always passes.
func NewFunc(name string, fn Func) *Provider {
	return &Provider{name: name, fn: fn}
}

// Constructor builds an unscripted fake from a provider config, so a fake
// can be registered like a real adapter. It lists the config's models. Add
// steps with Script, or it fails every Chat call with ErrExhausted.
func Constructor(cfg llm.ProviderConfig) (llm.Provider, error) {
	p := New(cfg.Name)
	for _, id := range cfg.Models {
		p.models = append(p.models, llm.ModelInfo{ID: id})
	}
	return p, nil
}

// Script appends steps to the script.
func (p *Provider) Script(steps ...Step) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, steps...)
	return p
}

// SetModels sets what ListModels returns.
func (p *Provider) SetModels(models []llm.ModelInfo, err error) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.models, p.modelsErr = slices.Clone(models), err
	return p
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.name }

// Chat implements llm.Provider. It records a deep copy of the request, so
// later changes by the caller do not alter what a test sees.
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	call := len(p.requests)
	p.requests = append(p.requests, req.Clone())
	if p.fn != nil {
		p.mu.Unlock()
		return p.fn(ctx, call, req.Clone())
	}
	if p.next >= len(p.steps) {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: unexpected call %d with %d messages (model %q)",
			ErrExhausted, call+1, len(req.Messages), req.Model)
	}
	index := p.next
	step := p.steps[index]
	p.next++
	p.mu.Unlock()

	if step.Expect != nil {
		if err := step.Expect(req.Clone()); err != nil {
			return nil, fmt.Errorf("fake provider: step %d: unexpected request: %w", index, err)
		}
	}
	if step.Err != nil {
		return nil, step.Err
	}
	return p.response(index, step, req.Model), nil
}

func (p *Provider) response(index int, step Step, model string) *llm.ChatResponse {
	if step.Response != nil {
		return step.Response.Clone()
	}
	msg := llm.Message{Role: llm.RoleAssistant, Origin: p.name}
	if step.Text != "" {
		msg.Parts = append(msg.Parts, llm.Text(step.Text))
	}
	for i, call := range step.ToolCalls {
		if call.ID == "" {
			call.ID = fmt.Sprintf("call_%d_%d", index, i)
		}
		if len(call.Args) == 0 {
			call.Args = json.RawMessage(`{}`)
		}
		msg.Parts = append(msg.Parts, llm.Call(call.ID, call.Name, slices.Clone(call.Args)))
	}
	stop := step.Stop
	if stop == "" {
		stop = llm.StopEndTurn
		if len(step.ToolCalls) > 0 {
			stop = llm.StopToolUse
		}
	}
	resp := &llm.ChatResponse{
		Message:    msg,
		StopReason: stop,
		RawStop:    string(stop),
		Model:      model,
		RequestID:  fmt.Sprintf("fake_%d", index),
	}
	if step.Usage != nil {
		usage := *step.Usage
		resp.Usage = &usage
	}
	return resp
}

// ListModels implements llm.Provider.
func (p *Provider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listCalls++
	if p.modelsErr != nil {
		return nil, p.modelsErr
	}
	return slices.Clone(p.models), nil
}

// Requests returns a deep copy of every Chat request received, in order.
func (p *Provider) Requests() []llm.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]llm.ChatRequest, len(p.requests))
	for i, req := range p.requests {
		out[i] = req.Clone()
	}
	return out
}

// Calls returns how many Chat calls were received.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// ListCalls returns how many ListModels calls were received.
func (p *Provider) ListCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listCalls
}

// Remaining returns how many scripted steps were not used.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.steps) - p.next
}

// AssertConsumed fails the test if scripted steps are left over, which means
// the code under test stopped earlier than the test expected.
func (p *Provider) AssertConsumed(t TB) {
	t.Helper()
	p.mu.Lock()
	used, total := p.next, len(p.steps)
	p.mu.Unlock()
	if used < total {
		t.Errorf("fake provider %q: %d of %d scripted steps were not used (next unused: step %d)",
			p.name, total-used, total, used)
	}
}

// fileStep is the JSON form of a Step in a script file.
type fileStep struct {
	Text      string         `json:"text"`
	ToolCalls []llm.ToolCall `json:"tool_calls"`
	Stop      llm.StopReason `json:"stop"`
	Usage     *llm.Usage     `json:"usage"`
	// Error makes the step fail with an *llm.Error of this kind.
	Error   llm.ErrorKind `json:"error"`
	Message string        `json:"message"`
}

// FromFile reads a JSON script: an array of steps with the keys text,
// tool_calls, stop, usage, error and message. It is for demos that run the
// whole program without a real model.
func FromFile(name, path string) (*Provider, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user names the script file
	if err != nil {
		return nil, fmt.Errorf("fake script: %w", err)
	}
	steps, err := ParseScript(name, data)
	if err != nil {
		return nil, fmt.Errorf("fake script %s: %w", path, err)
	}
	return New(name, steps...), nil
}

// ParseScript parses the JSON script format of FromFile.
func ParseScript(name string, data []byte) ([]Step, error) {
	var raw []fileStep
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse: unexpected data after the script")
	}
	steps := make([]Step, len(raw))
	for i, fs := range raw {
		steps[i] = Step{Text: fs.Text, ToolCalls: fs.ToolCalls, Stop: fs.Stop, Usage: fs.Usage}
		if fs.Error != "" {
			steps[i].Err = &llm.Error{Kind: fs.Error, Provider: name, Message: fs.Message}
		} else if fs.Text == "" && len(fs.ToolCalls) == 0 {
			return nil, fmt.Errorf("step %d has no text, tool_calls or error", i)
		}
	}
	return steps, nil
}
