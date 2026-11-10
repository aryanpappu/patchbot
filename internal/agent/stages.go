package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// stageRegistry cannot be supplied by a caller or replaced by a raw tools
// registry. Its constructors fix the stage's capabilities.
type stageRegistry struct{ *tools.Registry }

func newRefineRegistry(env *tools.Env) (*stageRegistry, error) {
	r, err := tools.NewReadOnlyRegistry(env, tools.SubmitSpec{})
	if err != nil {
		return nil, err
	}
	return &stageRegistry{r}, nil
}

func newInvestigateRegistry(env *tools.Env) (*stageRegistry, error) {
	r, err := tools.NewReadOnlyRegistry(env, tools.SubmitReport{})
	if err != nil {
		return nil, err
	}
	return &stageRegistry{r}, nil
}

// No write-capable stage registry can be constructed without Check's private
// token. Revalidate it immediately before registering writes and commands.
func newCodeRegistry(ctx context.Context, approval approvalToken, env *tools.Env, runID int64, issueHash string, criteria []string) (*stageRegistry, error) {
	if _, err := validateApproval(ctx, approval, runID, issueHash); err != nil {
		return nil, err
	}
	r, err := tools.NewCodeRegistry(env, tools.RunCmd{}, tools.Submit{Criteria: criteria})
	if err != nil {
		return nil, &Error{Kind: "internal", Message: "build code tools failed"}
	}
	return &stageRegistry{r}, nil
}

// SpecStore appends an immutable spec version. store.Store implements it.
type SpecStore interface {
	SaveSpec(context.Context, store.Spec) (store.Spec, error)
}

// RefineRequest supplies the issue snapshot and optional previous spec/notes.
// Session carries run-wide accounting across revisions and stages. Run lifecycle
// transitions and posting comments remain the runner's responsibility.
type RefineRequest struct {
	RunID       int64
	Title, Body string
	Prior       *store.Spec
	Notes       string
	Store       SpecStore
	Session     *Session
	Limits      Limits
}

// SpecResult is either an approval-ready spec or questions requiring an answer.
// The draft spec is saved in both cases, preserving its version and audit trail.
type SpecResult struct {
	Outcome
	Spec      store.Spec
	Questions []string
	Status    store.Status
}

// Refine explores read-only and saves a new version, including unresolved questions.
func (l *Loop) Refine(ctx context.Context, req RefineRequest) (SpecResult, error) {
	if req.Store == nil || req.RunID <= 0 {
		return SpecResult{}, &Error{Kind: "internal", Message: "refine requires spec storage and a run ID"}
	}
	stage := *l
	var err error
	stage.tools, err = newRefineRegistry(stage.Env)
	if err != nil {
		return SpecResult{}, &Error{Kind: "internal", Message: "build refine tools failed"}
	}
	task := issueData(stage.redactor().Redact(req.Title), stage.redactor().Redact(req.Body))
	version := int64(1)
	source := "agent"
	if req.Prior != nil {
		if req.Prior.RunID != req.RunID {
			return SpecResult{}, &Error{Kind: "internal", Message: "previous spec belongs to another run"}
		}
		version, source = req.Prior.Version+1, "revise"
		task += fmt.Sprintf("\nPrevious specification version: %d\n", req.Prior.Version) + untrusted("previous specification", stage.redactor().Redact(req.Prior.BodyMD))
	}
	if req.Notes != "" {
		task += "\n" + untrusted("revision notes or answers", stage.redactor().Redact(req.Notes))
	}
	session := startStage(req.Session, refinePrompt, task)
	lim := req.Limits
	if lim.MaxTurns <= 0 {
		lim.MaxTurns = 20
	}
	outcome, err := stage.Run(ctx, session, lim)
	result := SpecResult{Outcome: outcome}
	if err != nil {
		return result, err
	}
	if outcome.Reason != EndSubmitted || outcome.Submission == nil || outcome.Submission.Spec == nil {
		return result, &Error{Kind: "refine_incomplete", Message: "refinement ended without a validated specification"}
	}
	draft := outcome.Submission.Spec
	spec := store.Spec{RunID: req.RunID, Version: version, BodyMD: draft.Markdown(), Hash: draft.Hash(), IssueHash: IssueHash(req.Title, req.Body), Source: source, Notes: stage.redactor().Redact(req.Notes)}
	saved, err := req.Store.SaveSpec(ctx, spec)
	if err != nil {
		return result, &Error{Kind: "internal", Message: "saving specification failed"}
	}
	result.Spec, result.Status = saved, store.StatusAwaitingApproval
	if len(draft.OpenQuestions) > 0 {
		result.Questions = append([]string(nil), draft.OpenQuestions...)
		result.Status = store.StatusNeedsInfo
	}
	return result, nil
}

// InvestigateRequest supplies an issue and run-wide accounting, if any.
type InvestigateRequest struct {
	Title, Body string
	Session     *Session
	Limits      Limits
}

// ReportResult contains the investigation outcome and a redacted report.
type ReportResult struct {
	Outcome
	ReportMD string
}

// Investigate explores read-only and returns a report for the runner to publish.
func (l *Loop) Investigate(ctx context.Context, req InvestigateRequest) (ReportResult, error) {
	stage := *l
	var err error
	stage.tools, err = newInvestigateRegistry(stage.Env)
	if err != nil {
		return ReportResult{}, &Error{Kind: "internal", Message: "build investigation tools failed"}
	}
	session := startStage(req.Session, investigatePrompt, issueData(stage.redactor().Redact(req.Title), stage.redactor().Redact(req.Body)))
	outcome, err := stage.Run(ctx, session, req.Limits)
	result := ReportResult{Outcome: outcome}
	if outcome.Submission != nil {
		result.ReportMD = outcome.Submission.Report
	}
	return result, err
}

// ChangeSource is implemented by gitx.Repo. Diff detection is a trusted harness
// operation; write-tool calls and the model's claims cannot establish changes.
type ChangeSource interface {
	HasChanges(context.Context) (bool, error)
}

// CodeRequest supplies trusted harness configuration. TestCommand is a policy
// command ID from repo configuration, never a command chosen by the model.
type CodeRequest struct {
	RunID       int64
	Title, Body string
	Changes     ChangeSource
	TestCommand string
	Session     *Session
	Limits      Limits
}

// CodeResult tells the runner whether there is anything to publish and whether
// it must be a draft. Only CanPublish permits publication. A non-nil error is
// retained even when salvage is allowed;
// the runner can publish only when Salvage explicitly permits it.
type CodeResult struct {
	Outcome
	NoChanges    bool
	CanPublish   bool
	Draft        bool
	Salvage      bool
	TestsStatus  string
	TestAttempts int
	LastTest     *execx.Result
}

// Code requires a private concrete capability produced by Check. It revalidates the
// capability before constructing any write tools. There is deliberately no
// overload accepting only a CodeRequest, and no approval boolean in that request.
func (l *Loop) Code(ctx context.Context, approval approvalToken, req CodeRequest) (CodeResult, error) {
	spec, err := validateApproval(ctx, approval, req.RunID, IssueHash(req.Title, req.Body))
	if err != nil {
		return CodeResult{}, err
	}
	draft, err := tools.ParseSpec(spec.BodyMD)
	if err != nil || draft.Hash() != spec.Hash || len(draft.OpenQuestions) != 0 {
		return CodeResult{}, fmt.Errorf("%w: approved spec content is invalid or has open questions", ErrApprovalDenied)
	}
	if req.Changes == nil {
		return CodeResult{}, &Error{Kind: "internal", Message: "code requires harness change detection"}
	}
	stage := *l
	stage.tools, err = newCodeRegistry(ctx, approval, stage.Env, req.RunID, IssueHash(req.Title, req.Body), draft.AcceptanceCriteria)
	if err != nil {
		return CodeResult{}, err
	}
	session := startStage(req.Session, codePrompt, issueData(stage.redactor().Redact(req.Title), stage.redactor().Redact(req.Body))+"\nApproved specification:\n"+untrusted("approved specification", stage.redactor().Redact(spec.BodyMD)))
	lim := req.Limits.defaults()
	result := CodeResult{TestsStatus: "not_run"}
	for {
		result.CanPublish, result.Salvage, result.Draft = false, false, false
		outcome, loopErr := stage.Run(ctx, session, lim)
		if outcome.Synthesized && outcome.Submission != nil {
			for _, text := range draft.AcceptanceCriteria {
				outcome.Submission.Criteria = append(outcome.Submission.Criteria, tools.CriterionResult{Text: text, Note: "No structured assessment was submitted."})
			}
			outcome.Submission = outcome.Submission.Redacted(stage.redactor())
		}
		result.Outcome = outcome
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !salvageable(outcome, loopErr) {
			return result, loopErr
		}
		changed, err := req.Changes.HasChanges(ctx)
		if err != nil {
			return result, &Error{Kind: "internal", Message: "inspect working tree failed"}
		}
		if !changed {
			result.NoChanges = true
			return result, loopErr
		}
		hardStop := loopErr != nil || outcome.Reason != EndSubmitted
		result.Draft = hardStop || outcome.Synthesized
		if outcome.Submission != nil {
			for _, criterion := range outcome.Submission.Criteria {
				if !criterion.Met {
					result.Draft = true
				}
			}
		}
		if req.TestCommand == "" {
			result.CanPublish, result.Salvage = true, hardStop
			result.Draft = true
			return result, loopErr
		}
		test, err := stage.verify(ctx, req.TestCommand, lim, result.TestAttempts+1)
		if err != nil {
			return result, err
		}
		result.TestAttempts++
		result.LastTest = &test
		// Test commands can modify or revert files, so re-inspect even on success.
		changed, err = req.Changes.HasChanges(ctx)
		if err != nil {
			return result, &Error{Kind: "internal", Message: "inspect tested working tree failed"}
		}
		if !changed {
			result.NoChanges = true
			result.Draft, result.Salvage = false, false
			return result, loopErr
		}
		if test.ExitCode == 0 && !test.TimedOut {
			result.CanPublish, result.Salvage = true, hardStop
			result.TestsStatus = "passed"
			return result, loopErr
		}
		result.TestsStatus, result.Draft = "failed", true
		if hardStop || result.TestAttempts >= lim.MaxTestAttempts {
			result.CanPublish, result.Salvage = true, hardStop
			return result, loopErr
		}
		feedback(session, fmt.Sprintf("Harness tests failed (attempt %d of %d; exit code %d; timed out: %t). Repair the patch, then submit again.\n%s", result.TestAttempts, lim.MaxTestAttempts, test.ExitCode, test.TimedOut, boundedUntrusted("test output", tools.CapOutput(test.Output, 8<<10), 8<<10)))
	}
}

func salvageable(outcome Outcome, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || outcome.Reason == EndRefusal {
		return false
	}
	if err == nil {
		return true
	}
	var classified *Error
	if !errors.As(err, &classified) {
		return false
	}
	// Safety/integrity and harness failures require local diagnostics, not a patch.
	return classified.Kind == "provider_unavailable" || classified.Kind == "context_overflow" || classified.Kind == "provider_malformed" || classified.Kind == "provider_bad_request"
}

func (l *Loop) verify(ctx context.Context, command string, lim Limits, attempt int) (execx.Result, error) {
	start := l.now()
	result, err := tools.ExecuteCommand(ctx, l.Env, command, nil, lim.TestTimeout)
	finish := l.now()
	duration, exit := finish.Sub(start).Milliseconds(), int64(result.ExitCode)
	input, _ := json.Marshal(map[string]any{"command": command, "attempt": attempt})
	status := "ok"
	if err != nil || result.ExitCode != 0 || result.TimedOut {
		status = "error"
	}
	step := store.Step{Kind: "test", Phase: store.PhaseVerify, Tool: command, Status: status, InputJSON: string(input), OutputText: result.Output, ExitCode: &exit, StartedAt: start, FinishedAt: &finish, DurationMS: &duration}
	if e := l.record(ctx, step); e != nil {
		return result, e
	}
	if e := l.event(ctx, "test.result", map[string]any{"attempt": attempt, "passed": status == "ok", "exit_code": exit, "timed_out": result.TimedOut, "output": tools.CapOutput(result.Output, 8<<10)}); e != nil {
		return result, e
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, &Error{Kind: "internal", Message: "harness test execution failed"}
	}
	return result, nil
}
