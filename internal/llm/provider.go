package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Provider is one configured model service. Adapters for each API implement
// it. Calls are not streamed.
type Provider interface {
	// Name is the instance name from the config, such as "anthropic" or
	// "ollama", not the kind of API.
	Name() string
	// Chat sends one request. A failure of the service is returned as an
	// *Error. A context error is returned as it is.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ListModels asks the service which models it offers.
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ChatRequest is one call to a model.
type ChatRequest struct {
	Model    string
	System   string
	Messages []Message
	Tools    []ToolSpec
	// MaxTokens caps the output of this call.
	MaxTokens int
	// Temperature is sent only when it is not nil. Some models reject it.
	Temperature *float64
}

// Validate checks a request before it is sent: the model and output cap are
// set, the tools are well formed, and the messages pass Validate and end on
// the user side, with every tool call answered.
func (r ChatRequest) Validate() error {
	var errs []error
	if r.Model == "" {
		errs = append(errs, errors.New("no model"))
	}
	if r.MaxTokens <= 0 {
		errs = append(errs, fmt.Errorf("max tokens is %d, want more than 0", r.MaxTokens))
	}
	seen := map[string]bool{}
	for i, tool := range r.Tools {
		if err := tool.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("tool %d: %w", i, err))
		}
		if seen[tool.Name] {
			errs = append(errs, fmt.Errorf("tool %d: name %q is used twice", i, tool.Name))
		}
		seen[tool.Name] = true
	}
	switch {
	case len(r.Messages) == 0:
		errs = append(errs, errors.New("no messages"))
	case r.Messages[len(r.Messages)-1].Role == RoleAssistant:
		errs = append(errs, errors.New("the last message is from the assistant; a request must end with a user or tool message"))
	}
	if err := Validate(r.Messages); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Clone returns a deep copy of the request.
func (r ChatRequest) Clone() ChatRequest {
	out := r
	out.Messages = CloneMessages(r.Messages)
	if r.Tools != nil {
		out.Tools = make([]ToolSpec, len(r.Tools))
		for i, tool := range r.Tools {
			out.Tools[i] = tool
			out.Tools[i].Schema = slices.Clone(tool.Schema)
		}
	}
	if r.Temperature != nil {
		t := *r.Temperature
		out.Temperature = &t
	}
	return out
}

// ChatResponse is the model's answer to one ChatRequest.
type ChatResponse struct {
	// Message has the assistant role.
	Message    Message
	StopReason StopReason
	// RawStop is the provider's own stop value, kept for logs.
	RawStop string
	// Usage is nil when the provider reported none.
	Usage *Usage
	// Model is the model the provider says it used.
	Model     string
	RequestID string
}

// Clone returns a deep copy of the response.
func (r *ChatResponse) Clone() *ChatResponse {
	if r == nil {
		return nil
	}
	out := *r
	out.Message = r.Message.Clone()
	if r.Usage != nil {
		u := *r.Usage
		out.Usage = &u
	}
	return &out
}

// ModelSource says where a model list entry came from.
type ModelSource string

// The model sources.
const (
	SourceLive   ModelSource = "live"   // the provider's model list API
	SourceConfig ModelSource = "config" // the config file or built-in defaults
)

// ModelInfo describes one model a provider offers.
type ModelInfo struct {
	ID          string
	DisplayName string
	// ContextWindow is in tokens; 0 when unknown.
	ContextWindow int
	Source        ModelSource
}
