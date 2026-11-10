package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

const compactedOutput = "[output removed to save context; call the tool again if needed]"

type limitError struct{ reason EndReason }

func (e *limitError) Error() string { return string(e.reason) }

type attemptProvider struct {
	llm.Provider
	chat func(context.Context, llm.ChatRequest) (*llm.ChatResponse, error)
}

func (p attemptProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return p.chat(ctx, req)
}

// Run drives a session until submission or a bounded stop. Re-entry after test
// failure retains accounting, turn counts, overflow recovery and stuck state.
// The caller supplies a raw provider; this is the sole retry layer.
func (l *Loop) Run(ctx context.Context, s *Session, lim Limits) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if s == nil || l.Provider == nil || l.Tools == nil || l.Model == "" {
		return Outcome{}, &Error{Kind: "internal", Message: "loop requires a session, provider, model and tools"}
	}
	lim = lim.defaults()
	if lim.Deadline.IsZero() {
		if s.deadline.IsZero() {
			s.deadline = l.now().Add(30 * time.Minute)
		}
		lim.Deadline = s.deadline
	}
	l.initialize(s)
	priceKey := l.Provider.Name() + "\x00" + l.Model
	if s.priceWarningKey != priceKey {
		s.priceWarningKey = priceKey
		if _, known := l.Pricing.Price(l.Model); !known {
			s.Unpriced = true
			if err := l.event(ctx, "log", map[string]any{"level": "warn", "message": "Model price is unknown; only the token budget applies."}); err != nil {
				return Outcome{}, err
			}
		}
	}
	if reason := l.bounds(ctx, s, lim); reason != "" {
		return Outcome{Reason: reason}, nil
	}
	callCtx, cancel := stageContext(ctx, lim.Deadline, l.now())
	defer cancel()
	provider := llm.WithRetry(attemptProvider{Provider: l.Provider, chat: func(c context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
		return l.attempt(c, s, lim)
	}}, l.RetryPolicy, l.RetryClock)
	malformedErrors := 0
	for {
		resp, err := provider.Chat(callCtx, llm.ChatRequest{})
		if ctx.Err() != nil {
			return Outcome{}, ctx.Err()
		}
		if err != nil {
			var limit *limitError
			if errors.As(err, &limit) {
				return Outcome{Reason: limit.reason}, nil
			}
			if callCtx.Err() != nil {
				return Outcome{Reason: EndDeadline}, nil
			}
			kind, _ := llm.KindOf(err)
			switch {
			case kind == llm.ErrContextTooLong && !s.contextCompacted:
				s.contextCompacted = true
				compact(s.Messages, 3)
				if e := l.event(ctx, "log", map[string]string{"level": "warn", "message": "Context overflow: compacting history and retrying once."}); e != nil {
					return Outcome{}, e
				}
				continue
			case kind == llm.ErrMalformed && malformedErrors == 0:
				malformedErrors++
				continue
			default:
				return Outcome{}, l.classify(err)
			}
		}
		malformedErrors = 0
		outcome, done, err := l.consume(callCtx, s, resp)
		if ctx.Err() != nil {
			return Outcome{}, ctx.Err()
		}
		if callCtx.Err() != nil {
			return Outcome{Reason: EndDeadline}, nil
		}
		if err != nil || done {
			return outcome, err
		}
	}
}

func (l *Loop) initialize(s *Session) {
	if !s.initialized {
		if s.Usage == nil && !s.AccountingUncertain {
			s.Usage = &llm.Usage{}
		}
		s.Tokens = max(s.Tokens, s.Usage.Total())
		s.Cost = max(s.Cost, 0)
		s.initialized = true
	}
	s.System = l.redactor().Redact(s.System)
	redactConversation(s.Messages, l.redactor())
}

func (l *Loop) bounds(ctx context.Context, s *Session, lim Limits) EndReason {
	if ctx.Err() != nil {
		return EndDeadline
	}
	if !lim.Deadline.IsZero() && !l.now().Before(lim.Deadline) {
		return EndDeadline
	}
	if s.Turns-s.turnBase >= lim.MaxTurns {
		return EndMaxTurns
	}
	return ""
}

func (l *Loop) attempt(ctx context.Context, s *Session, lim Limits) (*llm.ChatResponse, error) {
	if reason := l.bounds(ctx, s, lim); reason != "" {
		return nil, &limitError{reason}
	}
	redactConversation(s.Messages, l.redactor())
	req := llm.ChatRequest{Model: l.Model, System: s.System, Messages: s.Messages, Tools: l.Tools.Specs(), MaxTokens: lim.MaxOutputTokens}
	// One token per serialized byte, plus envelope overhead, is a conservative
	// first-call estimate without depending on any provider's tokenizer. Never
	// reserve less than the last reported input, including cached input.
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, &Error{Kind: "internal", Message: "request encoding failed"}
	}
	input := max(s.LastInputTokens, int64(len(raw))+256)
	tokens := add(input, int64(lim.MaxOutputTokens))
	reserve, priced := l.Pricing.Reserve(l.Model, input, int64(lim.MaxOutputTokens))
	if exceeds(s.Tokens, tokens, lim.MaxTotalTokens) || (priced && lim.MaxCostMicros > 0 && exceeds(s.Cost, reserve, lim.MaxCostMicros)) {
		return nil, &limitError{EndBudget}
	}
	if lim.ContextWindow > 0 && s.LastInputTokens > int64(lim.ContextWindow)*7/10 {
		compact(s.Messages, 6)
	}
	req.Messages = s.Messages
	if err := req.Validate(); err != nil {
		return nil, &Error{Kind: "internal", Message: "invalid conversation or tool definitions"}
	}
	start := l.now()
	// Clone so neither a provider nor later compaction can mutate past requests.
	resp, callErr := l.Provider.Chat(ctx, req.Clone())
	s.Turns++
	var usage *llm.Usage
	if resp != nil {
		usage = resp.Usage
	}
	cost, known := l.Pricing.Cost(l.Model, usage)
	if usage == nil {
		s.AccountingUncertain = true
		s.Usage = nil
		s.Tokens = add(s.Tokens, tokens)
		if priced {
			s.Cost = add(s.Cost, reserve)
		}
	} else {
		s.Usage = s.Usage.Add(usage)
		s.Tokens = add(s.Tokens, usage.Total())
		s.LastInputTokens = add(add(usage.InputTokens, usage.CacheReadTokens), usage.CacheWriteTokens)
		if known {
			s.Cost = add(s.Cost, cost)
		}
	}
	if callErr == nil {
		callErr = l.validateResponse(s, resp)
	}
	finish := l.now()
	duration, turn := finish.Sub(start).Milliseconds(), int64(s.Turns)
	inputJSON, _ := json.Marshal(map[string]any{"model": l.Model, "messages": recordedMessages(req.Messages), "reserved_tokens": tokens, "reserved_cost_micros": reserve, "usage_missing": usage == nil})
	step := store.Step{Kind: "llm", Tool: "chat", Status: "ok", Turn: &turn, InputJSON: string(inputJSON), StartedAt: start, FinishedAt: &finish, DurationMS: &duration}
	if known {
		step.CostMicros = &cost
	}
	if usage != nil {
		step.InputTokens, step.OutputTokens = &usage.InputTokens, &usage.OutputTokens
		step.CacheReadTokens, step.CacheWriteTokens = &usage.CacheReadTokens, &usage.CacheWriteTokens
	}
	if resp != nil {
		step.StopReason = string(resp.StopReason)
		out, _ := json.Marshal(recordedMessages([]llm.Message{resp.Message}))
		step.OutputText = redactJSON(out, l.redactor())
	}
	if callErr != nil {
		step.Status = "error"
		step.OutputText = llm.SafeText(callErr)
	}
	if err := l.record(ctx, step); err != nil {
		return nil, err
	}
	if err := l.event(ctx, "llm.response", map[string]any{"turn": s.Turns, "stop_reason": step.StopReason, "response": step.OutputText, "error": callErr != nil}); err != nil {
		return nil, err
	}
	if err := l.event(ctx, "usage", map[string]any{"turn": s.Turns, "usage": usage, "cost_micros": step.CostMicros, "total_cost_micros": s.Cost, "total_tokens": s.Tokens, "accounting_uncertain": s.AccountingUncertain, "unpriced": s.Unpriced}); err != nil {
		return nil, err
	}
	if callErr != nil {
		return nil, callErr
	}
	return resp, nil
}

func (l *Loop) validateResponse(s *Session, resp *llm.ChatResponse) error {
	if resp == nil {
		return &llm.Error{Kind: llm.ErrMalformed, Message: "empty provider response"}
	}
	msg := replyMessage(resp)
	candidate := append(llm.CloneMessages(s.Messages), msg)
	redactConversation(candidate, l.redactor())
	if msg.Role != llm.RoleAssistant || llm.Validate(candidate) != nil {
		return &llm.Error{Kind: llm.ErrMalformed, Message: "invalid assistant response"}
	}
	return nil
}

func replyMessage(resp *llm.ChatResponse) llm.Message {
	msg := resp.Message.Clone()
	// A truncated or refused action is never executable, even when its JSON
	// happens to parse. Keep text and provider metadata unchanged.
	if resp.StopReason == llm.StopMaxTokens || resp.StopReason == llm.StopRefusal {
		msg = withoutCalls(msg)
	}
	if len(msg.Parts) == 0 {
		msg.Parts = []llm.Part{llm.Text("[empty model reply]")}
	}
	return msg
}

func (l *Loop) consume(ctx context.Context, s *Session, resp *llm.ChatResponse) (Outcome, bool, error) {
	defer func() { redactConversation(s.Messages, l.redactor()) }()
	msg := replyMessage(resp)
	s.Messages = append(s.Messages, msg)
	if resp.StopReason == llm.StopRefusal {
		return Outcome{Reason: EndRefusal}, true, nil
	}
	calls := msg.ToolCalls()
	if len(calls) > 0 {
		s.endTurns, s.malformed = 0, 0
		return l.dispatch(ctx, s, calls)
	}
	switch resp.StopReason {
	case llm.StopMaxTokens:
		s.endTurns, s.malformed = 0, 0
		feedback(s, "Your reply reached the output limit. No tool calls from it were executed. Use smaller edits and try again.")
	case llm.StopEndTurn:
		s.malformed = 0
		s.endTurns++
		if s.endTurns >= 2 {
			return Outcome{Reason: EndSubmitted, Synthesized: true, Submission: &tools.Submission{Summary: msg.Text(), PRTitle: "Proposed change", Report: msg.Text()}}, true, nil
		}
		feedback(s, "Call the stage's terminal tool to finish, or continue working.")
	default:
		s.endTurns = 0
		s.malformed++
		if s.malformed >= 2 {
			return Outcome{}, false, &Error{Kind: "provider_malformed", Message: "two consecutive unusable model replies"}
		}
		feedback(s, "Your reply could not be processed. Try again using the available tools.")
	}
	return Outcome{}, false, nil
}

func (l *Loop) dispatch(ctx context.Context, s *Session, calls []llm.ToolCall) (Outcome, bool, error) {
	results := llm.Message{Role: llm.RoleTool}
	var submission *tools.Submission
	var stop bool
	var dispatchErr error
	warn := false
	for _, call := range calls {
		start := l.now()
		eventArgs, argsErr := jsonValue(call.Args)
		if argsErr != nil {
			eventArgs = string(call.Args)
		}
		if err := l.event(ctx, "tool.call", map[string]any{"turn": s.Turns, "call_id": call.ID, "name": call.Name, "args": eventArgs}); err != nil {
			return Outcome{}, false, err
		}
		var detail tools.Result
		var result llm.ToolResult
		var err error
		// Finish the call/result group even when a terminal call appears before its
		// siblings. Calls after completion or a stop receive an explicit skipped result.
		if submission != nil || stop || dispatchErr != nil {
			result = llm.ToolResult{CallID: call.ID, Name: call.Name, Content: "Skipped: the stage has already ended.", IsError: true}
			detail = tools.Result{Content: result.Content, RecordOutput: result.Content, IsError: true}
		} else {
			result, detail, err = l.Tools.DispatchDetailed(ctx, call)
			if err != nil {
				result = llm.ToolResult{CallID: call.ID, Name: call.Name, Content: "Tool execution failed in the harness.", IsError: true}
				detail = tools.Result{IsError: true, RecordOutput: result.Content}
				dispatchErr = &Error{Kind: "internal", Message: "tool execution failed"}
			}
			if detail.Submission != nil {
				submission = detail.Submission
			}
			if detail.SecurityDenied {
				s.denials++
				if eventErr := l.event(ctx, "security.denied", map[string]any{"tool": call.Name, "reason": detail.Content}); eventErr != nil {
					return Outcome{}, false, eventErr
				}
				if s.denials >= 5 {
					dispatchErr = &Error{Kind: "policy_violations", Message: "five tool policy denials"}
				}
			}
			hash := callHash(call)
			if hash == s.stuckHash {
				s.stuckCount++
			} else {
				s.stuckHash, s.stuckCount = hash, 1
			}
			if s.stuckCount == 3 {
				warn = true
			}
			if s.stuckCount >= 4 {
				stop = true
			}
		}
		finish, turn := l.now(), int64(s.Turns)
		duration := finish.Sub(start).Milliseconds()
		status := "ok"
		if result.IsError || err != nil {
			status = "error"
		}
		var exit *int64
		if detail.ExitCode != nil {
			value := int64(*detail.ExitCode)
			exit = &value
		}
		step := store.Step{Kind: "tool", Tool: call.Name, Status: status, Turn: &turn, InputJSON: string(call.Args), OutputText: detail.RecordOutput, ExitCode: exit, StartedAt: start, FinishedAt: &finish, DurationMS: &duration}
		if e := l.record(ctx, step); e != nil {
			return Outcome{}, false, e
		}
		if e := l.event(ctx, "tool.result", map[string]any{"turn": s.Turns, "result": result, "timed_out": detail.TimedOut, "exit_code": exit}); e != nil {
			return Outcome{}, false, e
		}
		// Tool payloads may contain malicious instructions. JSON string encoding
		// keeps fake closing delimiters inside the untrusted payload.
		if call.Name == "run_cmd" {
			result.Content = boundedUntrusted("command output", result.Content, 8<<10)
		} else {
			result.Content = untrusted("tool output", result.Content)
		}
		results.Parts = append(results.Parts, llm.Result(result.CallID, result.Name, result.Content, result.IsError))
	}
	s.Messages = append(s.Messages, results)
	if dispatchErr != nil {
		return Outcome{}, false, dispatchErr
	}
	if submission != nil {
		return Outcome{Reason: EndSubmitted, Submission: submission}, true, nil
	}
	if stop {
		return Outcome{Reason: EndStuck}, true, nil
	}
	if warn {
		feedback(s, "The same tool call has repeated three times. Change your approach; another identical call will stop this stage.")
	}
	return Outcome{}, false, nil
}

func compact(messages []llm.Message, keepTurns int) {
	cutoff := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleAssistant {
			keepTurns--
			if keepTurns < 0 {
				cutoff = i + 1
				break
			}
			cutoff = i
		}
	}
	if keepTurns >= 0 {
		return
	}
	for i := 0; i <= cutoff && i < len(messages); i++ {
		for j := range messages[i].Parts {
			part := &messages[i].Parts[j]
			if part.Type == llm.PartToolResult && part.ToolResult != nil {
				part.ToolResult.Content = compactedOutput
			}
		}
	}
}

func withoutCalls(msg llm.Message) llm.Message {
	parts := msg.Parts[:0]
	for _, part := range msg.Parts {
		if part.Type != llm.PartToolCall {
			parts = append(parts, part)
		}
	}
	msg.Parts = parts
	return msg
}

func feedback(s *Session, text string) {
	if len(s.Messages) > 0 && s.Messages[len(s.Messages)-1].Role == llm.RoleUser {
		last := &s.Messages[len(s.Messages)-1]
		last.Parts = append(last.Parts, llm.Text(text))
	} else {
		s.Messages = append(s.Messages, llm.UserText(text))
	}
}

func callHash(call llm.ToolCall) [32]byte {
	raw := call.Args
	if args, err := jsonValue(raw); err == nil {
		raw, _ = json.Marshal(args)
	}
	return sha256.Sum256(append([]byte(call.Name+"\x00"), raw...))
}

func add(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
func exceeds(spent, reserve, capValue int64) bool {
	return spent > capValue || reserve > capValue-spent
}

func (l *Loop) classify(err error) error {
	var internal *Error
	if errors.As(err, &internal) {
		return internal
	}
	kind, _ := llm.KindOf(err)
	name := "provider_unavailable"
	switch kind {
	case llm.ErrAuth:
		name = "provider_auth"
	case llm.ErrContextTooLong:
		name = "context_overflow"
	case llm.ErrBadRequest:
		name = "provider_bad_request"
	case llm.ErrMalformed:
		name = "provider_malformed"
	}
	return &Error{Kind: name, Message: l.redactor().Redact(llm.SafeText(err))}
}

// untrusted uses a JSON string so delimiter-like source text remains escaped.
func untrusted(kind, data string) string {
	raw, _ := json.Marshal(data)
	return fmt.Sprintf("<untrusted_data kind=%q encoding=\"json-string\">\n%s\n</untrusted_data>", kind, raw)
}

// Keep the encoded command payload, including its framing, within the model
// output cap. Escaped control characters can be larger than their source bytes.
func boundedUntrusted(kind, data string, limit int) string {
	encoded := untrusted(kind, data)
	for len(encoded) > limit && len(data) > 0 {
		next := max(0, len(data)-max(1, (len(encoded)-limit)*len(data)/len(encoded)))
		data = tools.CapOutput(data, next)
		encoded = untrusted(kind, data)
	}
	return encoded
}

func redactConversation(messages []llm.Message, redactor interface{ Redact(string) string }) {
	for i := range messages {
		for j := range messages[i].Parts {
			part := &messages[i].Parts[j]
			switch part.Type {
			case llm.PartText:
				part.Text = redactor.Redact(part.Text)
			case llm.PartToolCall:
				if part.ToolCall != nil {
					value, err := jsonValue(part.ToolCall.Args)
					if _, object := value.(map[string]any); err == nil && object {
						// Redact string leaves without changing JSON numbers.
						part.ToolCall.Args, _ = json.Marshal(redactWith(value, redactor))
					} else {
						// Dispatch sees the original bad arguments and returns
						// an error. Later requests still require object Args.
						part.ToolCall.Args = json.RawMessage(`{}`)
					}
				}
			case llm.PartToolResult:
				if part.ToolResult != nil {
					part.ToolResult.Content = redactor.Redact(part.ToolResult.Content)
				}
			}
		}
	}
}
func redactWith(value any, r interface{ Redact(string) string }) any {
	switch v := value.(type) {
	case string:
		return r.Redact(v)
	case []any:
		for i := range v {
			v[i] = redactWith(v[i], r)
		}
	case map[string]any:
		for k := range v {
			v[k] = redactWith(v[k], r)
		}
	}
	return value
}
