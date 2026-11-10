package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
)

// maxResultBytes is the hard cap on any tool result, whatever the tool's
// own limits are.
const maxResultBytes = 128 << 10

// Registry is an ordered set of tools bound to one Env. It is safe for
// concurrent use as long as its tools are; the file tools are.
type Registry struct {
	env     *Env
	order   []string
	entries map[string]entry
}

type entry struct {
	tool   Tool
	spec   llm.ToolSpec
	schema *llm.Schema
}

// NewRegistry returns a registry of the given tools, in order. It fails on
// an invalid tool definition or a name used twice.
func NewRegistry(env *Env, tools ...Tool) (*Registry, error) {
	if env == nil || env.WS == nil {
		return nil, errors.New("tools: the registry needs an Env with a workspace")
	}
	bound := *env
	bound.Limits = env.Limits.withDefaults()
	r := &Registry{env: &bound, entries: make(map[string]entry, len(tools))}
	for _, tool := range tools {
		if tool == nil {
			return nil, errors.New("tools: a tool is nil")
		}
		spec := tool.Spec()
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
		if _, dup := r.entries[spec.Name]; dup {
			return nil, fmt.Errorf("tools: tool %s is registered twice", spec.Name)
		}
		schema, err := llm.ParseSchema(spec.Schema)
		if err != nil {
			return nil, fmt.Errorf("tools: tool %s: %w", spec.Name, err)
		}
		r.order = append(r.order, spec.Name)
		r.entries[spec.Name] = entry{tool: tool, spec: spec, schema: schema}
	}
	return r, nil
}

// ReadOnlyTools returns the tools that cannot change the checkout:
// read_file, list_dir and search.
func ReadOnlyTools() []Tool {
	return []Tool{ReadFile{}, ListDir{}, Search{}}
}

// CodeTools returns the read-only tools plus edit_file and write_file.
func CodeTools() []Tool {
	return append(ReadOnlyTools(), EditFile{}, WriteFile{})
}

// NewReadOnlyRegistry returns the registry for the refine and investigate
// stages. It holds no tool that can change the checkout, so those stages
// cannot, whatever the prompt says. Tools given in extra are added after
// the file tools.
func NewReadOnlyRegistry(env *Env, extra ...Tool) (*Registry, error) {
	return NewRegistry(env, append(ReadOnlyTools(), extra...)...)
}

// NewCodeRegistry returns the registry for the code stage: the read-only
// tools plus edit_file and write_file, then the tools given in extra.
func NewCodeRegistry(env *Env, extra ...Tool) (*Registry, error) {
	return NewRegistry(env, append(CodeTools(), extra...)...)
}

// Names returns the tool names in registration order.
func (r *Registry) Names() []string { return slices.Clone(r.order) }

// TerminalNames lists the tools that can finish a stage, in registration order.
func (r *Registry) TerminalNames() []string {
	var names []string
	for _, name := range r.order {
		if r.IsTerminal(name) {
			names = append(names, name)
		}
	}
	return names
}

// IsTerminal reports whether name is a registered terminal tool.
func (r *Registry) IsTerminal(name string) bool {
	e, ok := r.entries[name]
	if !ok {
		return false
	}
	t, ok := e.tool.(interface{ Terminal() bool })
	return ok && t.Terminal()
}

// Specs returns the tool definitions to send to the model, in registration
// order.
func (r *Registry) Specs() []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		spec := r.entries[name].spec
		spec.Schema = slices.Clone(spec.Schema)
		specs = append(specs, spec)
	}
	return specs
}

// Dispatch runs one tool call and returns the result to send back to the
// model. An unknown tool name and arguments that do not fit the schema are
// results with IsError set, so the model can correct the call. Every result
// is redacted and capped. A non-nil error means the run must stop.
func (r *Registry) Dispatch(ctx context.Context, call llm.ToolCall) (llm.ToolResult, error) {
	result, _, err := r.DispatchDetailed(ctx, call)
	return result, err
}

// DispatchDetailed also returns the redacted record and terminal submission.
// Keeping this separate preserves the small Dispatch API used by file tools.
func (r *Registry) DispatchDetailed(ctx context.Context, call llm.ToolCall) (llm.ToolResult, Result, error) {
	if err := ctx.Err(); err != nil {
		return llm.ToolResult{}, Result{}, err
	}
	start := time.Now()
	res, err := r.run(ctx, call)
	if err != nil {
		return llm.ToolResult{}, Result{}, fmt.Errorf("tool %s: %w", call.Name, err)
	}
	content := r.env.Redactor.Redact(strings.ToValidUTF8(res.Content, "�"))
	if len(content) > maxResultBytes {
		content = cutUTF8(content, maxResultBytes) + "\n[result cut at the size limit]"
	}
	if content == "" {
		content = "(no output)"
	}
	res.Content = content
	res.RecordOutput = r.env.Redactor.Redact(strings.ToValidUTF8(res.RecordOutput, "�"))
	if res.RecordOutput == "" {
		res.RecordOutput = content
	}
	res.RecordOutput = CapOutput(res.RecordOutput, r.env.Limits.CmdStoreBytes)
	if res.IsError || !r.IsTerminal(call.Name) {
		res.Submission = nil
	}
	if res.Submission != nil {
		res.Submission = res.Submission.Redacted(r.env.Redactor)
	}
	if r.env.Log != nil {
		if res.SecurityDenied {
			r.env.Log.WarnContext(ctx, "tool policy denied", "tool", r.env.Redactor.Redact(call.Name), "reason", content)
		}
		r.env.Log.DebugContext(ctx, "tool call",
			"tool", r.env.Redactor.Redact(call.Name), "is_error", res.IsError, "bytes", len(content), "duration", time.Since(start))
	}
	return llm.ToolResult{CallID: call.ID, Name: call.Name, Content: content, IsError: res.IsError}, res, nil
}

func (r *Registry) run(ctx context.Context, call llm.ToolCall) (Result, error) {
	e, ok := r.entries[call.Name]
	if !ok {
		// The name comes from the model. It is quoted and cut so that it
		// cannot inject lines into the result.
		return failed("unknown tool %q; the tools are: %s", cutUTF8(r.env.Redactor.Redact(call.Name), 80), strings.Join(r.order, ", "))
	}
	args := call.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := e.schema.ValidateArgs(args); err != nil {
		return failed("invalid arguments for %s: %v", call.Name, err)
	}
	return e.tool.Execute(ctx, r.env, args)
}
