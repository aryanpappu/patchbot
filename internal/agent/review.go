package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// Review incompletion reasons. Every one of them fails closed: only a valid
// submitted approve verdict can pass a review.
const (
	ReviewMalformed = "malformed_verdict"
	ReviewTruncated = "diff_truncated"
)

// newReviewRegistry reuses the refine read-only tools. The reviewer can never
// write files or run commands, so no approval capability is involved.
func newReviewRegistry(env *tools.Env, criteriaIDs []string) (*stageRegistry, error) {
	r, err := tools.NewReadOnlyRegistry(env, tools.SubmitReview{CriteriaIDs: criteriaIDs})
	if err != nil {
		return nil, err
	}
	return &stageRegistry{r}, nil
}

// TestEvidence is the harness's own record of the last verification. It is
// supplied to the reviewer instead of the coder's claims.
type TestEvidence struct {
	Status   string
	Attempts int
	ExitCode *int
	TimedOut bool
	Output   string
}

// ReviewRequest supplies the approved spec, the actual bounded workspace diff
// and harness evidence. The coder's summary is deliberately absent. Session
// carries run-wide accounting only; the review has its own conversation.
type ReviewRequest struct {
	Spec     store.Spec
	Diff     string
	DiffHash string
	Tests    TestEvidence
	Session  *Session
	Limits   Limits
}

// ReviewResult holds a validated verdict, or Incomplete with a reason.
type ReviewResult struct {
	Outcome
	Verdict    *tools.ReviewVerdict
	Incomplete string
	// Truncated reports that the reviewer did not receive the complete diff.
	Truncated bool
}

// Approved reports whether the review produced a valid approve verdict.
func (r ReviewResult) Approved() bool {
	return r.Incomplete == "" && !r.Truncated && r.Verdict != nil && r.Verdict.Verdict == tools.VerdictApprove
}

// CriteriaIDs returns the IDs (AC1...) of an approved spec's criteria.
func CriteriaIDs(spec store.Spec) ([]string, []string, error) {
	draft, err := tools.ParseSpec(spec.BodyMD)
	if err != nil || draft.Hash() != spec.Hash {
		return nil, nil, &Error{Kind: "internal", Message: "approved spec content is invalid"}
	}
	ids := make([]string, len(draft.AcceptanceCriteria))
	for i := range ids {
		ids[i] = fmt.Sprintf("AC%d", i+1)
	}
	return ids, draft.AcceptanceCriteria, nil
}

// Review runs a read-only review. Provider failures, refusals, limits and
// malformed verdicts return an incomplete result, never an approval. Only
// cancellation and harness (recording) failures return an error.
func (l *Loop) Review(ctx context.Context, req ReviewRequest) (ReviewResult, error) {
	ids, _, err := CriteriaIDs(req.Spec)
	if err != nil {
		return ReviewResult{}, err
	}
	stage := *l
	stage.tools, err = newReviewRegistry(stage.Env, ids)
	if err != nil {
		return ReviewResult{}, &Error{Kind: "internal", Message: "build review tools failed"}
	}
	r := stage.redactor()
	var task strings.Builder
	fmt.Fprintf(&task, "Approved specification version %d, SHA-256 %s.\n", req.Spec.Version, req.Spec.Hash)
	task.WriteString(untrusted("approved specification", r.Redact(req.Spec.BodyMD)))
	fmt.Fprintf(&task, "\nCriterion IDs to assess: %s.\n", strings.Join(ids, ", "))
	diff := r.Redact(req.Diff)
	encodedDiff := boundedUntrusted("diff", diff, 64<<10)
	truncated := strings.HasSuffix(req.Diff, "[diff truncated]\n") || encodedDiff != untrusted("diff", diff)
	fmt.Fprintf(&task, "\nWorkspace diff against the approved base (snapshot SHA-256 %s; truncated: %t):\n", req.DiffHash, truncated)
	task.WriteString(encodedDiff)
	exit := "none"
	if req.Tests.ExitCode != nil {
		exit = fmt.Sprint(*req.Tests.ExitCode)
	}
	fmt.Fprintf(&task, "\nHarness verification: status %s; attempts %d; exit code %s; timed out %t.\n", req.Tests.Status, req.Tests.Attempts, exit, req.Tests.TimedOut)
	if req.Tests.Output != "" {
		task.WriteString(boundedUntrusted("harness test output", r.Redact(req.Tests.Output), 8<<10))
	}
	session := forkAccounting(req.Session)
	defer req.Session.absorbAccounting(session)
	session = startStage(session, reviewPrompt, task.String())
	outcome, runErr := stage.Run(ctx, session, req.Limits)
	result := ReviewResult{Outcome: outcome, Truncated: truncated}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if runErr != nil {
		var classified *Error
		if !errors.As(runErr, &classified) || classified.Kind == "internal" {
			return result, runErr
		}
		result.Incomplete = classified.Kind
		return result, nil
	}
	switch {
	case outcome.Reason != EndSubmitted:
		result.Incomplete = string(outcome.Reason)
	case outcome.Synthesized || outcome.Submission == nil || outcome.Submission.Review == nil:
		result.Incomplete = ReviewMalformed
	case truncated && outcome.Submission.Review.Verdict == tools.VerdictApprove:
		// An approval of a partial diff would vouch for bytes nobody reviewed.
		result.Incomplete = ReviewTruncated
	default:
		result.Verdict = outcome.Submission.Review
	}
	return result, nil
}

// forkAccounting copies only run-wide accounting. The review conversation and
// stage heuristics stay separate from the coder's, which repair resumes.
func forkAccounting(s *Session) *Session {
	f := &Session{}
	if s == nil {
		return f
	}
	f.Cost, f.Tokens, f.Turns = s.Cost, s.Tokens, s.Turns
	f.AccountingUncertain, f.Unpriced, f.initialized = s.AccountingUncertain, s.Unpriced, s.initialized
	if s.Usage != nil {
		usage := *s.Usage
		f.Usage = &usage
	}
	return f
}

// absorbAccounting returns a forked stage's charges to the run session.
// Charges only grow, so every later stage sees the review's spending.
func (s *Session) absorbAccounting(f *Session) {
	if s == nil || f == nil {
		return
	}
	s.Usage, s.Cost, s.Tokens, s.Turns = f.Usage, f.Cost, f.Tokens, f.Turns
	s.AccountingUncertain, s.Unpriced = f.AccountingUncertain, f.Unpriced
	s.initialized = s.initialized || f.initialized
}

// resumeStage starts a repair cycle in an existing conversation: a fresh turn
// allowance and loop heuristics, with accounting and denials retained.
func resumeStage(s *Session) {
	s.endTurns, s.malformed, s.stuckCount = 0, 0, 0
	s.stuckHash = [32]byte{}
	s.turnBase = s.Turns
}
