// Package agent drives bounded, provider-neutral refine, code and investigate
// conversations. Publication and run lifecycle transitions belong to the runner.
package agent

import (
 "context"
 "time"

 "github.com/aryanpappu/patchbot/internal/llm"
 "github.com/aryanpappu/patchbot/internal/secret"
 "github.com/aryanpappu/patchbot/internal/tools"
)

// Limits bound a stage. Test repair reuses the same session and limits.
// Zero token/turn fields select defaults; zero MaxCostMicros disables the cost cap.
type Limits struct {
 MaxTurns int
 MaxOutputTokens int
 MaxTotalTokens int64
 MaxCostMicros int64
 ContextWindow int
 Deadline time.Time
 MaxTestAttempts int
 TestTimeout time.Duration
}

func (l Limits) defaults() Limits {
 if l.MaxTurns <= 0 { l.MaxTurns = 40 }
 if l.MaxOutputTokens <= 0 { l.MaxOutputTokens = 8192 }
 if l.MaxTotalTokens <= 0 { l.MaxTotalTokens = 2_000_000 }
 if l.MaxTestAttempts <= 0 { l.MaxTestAttempts = 3 }
 if l.TestTimeout <= 0 { l.TestTimeout = 10*time.Minute }
 return l
}

// Session belongs to one goroutine. Usage is nil after any unknown usage. Cost
// and Tokens include retained worst-case reservations for calls without usage;
// they are conservative budget charges, not claims about a provider's invoice.
// Seed these totals from the run when starting a new stage or revision.
type Session struct {
 System string
 Messages []llm.Message
 Usage *llm.Usage
 Cost int64
 Tokens int64
 Turns int
 LastInputTokens int64
 AccountingUncertain bool
 Unpriced bool

 initialized bool
 warnedPrice bool
 contextCompacted bool
 endTurns int
 malformed int
 stuckHash [32]byte
 stuckCount int
 denials int
}

// EndReason is a normal loop ending.
type EndReason string

// Loop endings.
const (
 EndSubmitted EndReason = "submitted"
 EndMaxTurns EndReason = "max_turns"
 EndBudget EndReason = "budget"
 EndDeadline EndReason = "deadline"
 EndStuck EndReason = "stuck"
 EndRefusal EndReason = "refusal"
)

// Outcome contains a validated terminal submission or a bounded stop reason.
type Outcome struct {
 Reason EndReason
 Submission *tools.Submission
 Synthesized bool
}

// Engine lets a runner replace Loop without depending on its implementation.
type Engine interface { Run(context.Context, *Session, Limits) (Outcome, error) }

// Loop is immutable during Run. A provider supplied here must have its own
// retries disabled: Loop applies llm.WithRetry and records every attempt.
type Loop struct {
 Provider llm.Provider
 Model string
 Tools *tools.Registry
 Env *tools.Env
 Pricing llm.Pricing
 Recorder Recorder
 Redactor *secret.Redactor
 Clock func() time.Time
 RetryPolicy llm.RetryPolicy
 RetryClock llm.Clock
}

func (l *Loop) now() time.Time { if l.Clock != nil { return l.Clock() }; return time.Now() }
func (l *Loop) redactor() *secret.Redactor {
 if l.Redactor != nil { return l.Redactor }
 if l.Env != nil { return l.Env.Redactor }
 return nil
}

// Error is a classified harness or provider failure. Message is redacted;
// raw causes are deliberately not retained where a caller might print them.
type Error struct { Kind string; Message string }
func (e *Error) Error() string { return e.Kind+": "+e.Message }
