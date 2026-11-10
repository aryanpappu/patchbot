package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aryanpappu/patchbot/internal/store"
)

// ErrApprovalDenied means code must not start or register write tools.
var ErrApprovalDenied = errors.New("approval gate denied")

// ApprovalStore is the read boundary needed to validate an approval. store.Store
// implements it. The gate never changes run status or records a decision.
type ApprovalStore interface {
	GetRun(context.Context, int64) (store.Run, error)
	LatestSpec(context.Context, int64) (store.Spec, error)
	ValidApproval(context.Context, int64, int64, string) (store.Approval, error)
}

// ApprovalOptions is trusted policy. Zero values select the product defaults.
type ApprovalOptions struct {
	TTL          time.Duration
	MaxRevisions int
	Clock        func() time.Time
}

type approvalToken struct {
	store      ApprovalStore
	runID      int64
	issueHash  string
	spec       store.Spec
	approvalID int64
	options    ApprovalOptions
}

// Check returns a capability only for the latest approved version and issue.
// The concrete return type and all its fields are private. A caller can pass
// the inferred value to Loop.Code, but cannot construct one itself.
//
//nolint:revive // Returning a private capability prevents callers from constructing a token.
func Check(ctx context.Context, s ApprovalStore, runID int64, issueHash string, options ...ApprovalOptions) (approvalToken, error) {
	if err := ctx.Err(); err != nil {
		return approvalToken{}, err
	}
	if s == nil || runID <= 0 || issueHash == "" || len(options) > 1 {
		return approvalToken{}, deny("missing gate inputs")
	}
	opts := ApprovalOptions{}
	if len(options) == 1 {
		opts = options[0]
	}
	if opts.TTL < 0 || opts.MaxRevisions < 0 {
		return approvalToken{}, deny("invalid approval policy")
	}
	if opts.TTL == 0 {
		opts.TTL = 14 * 24 * time.Hour
	}
	if opts.MaxRevisions == 0 {
		opts.MaxRevisions = 5
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return approvalToken{}, fmt.Errorf("%w: read run: %w", ErrApprovalDenied, err)
	}
	if run.ID != runID || run.Status != store.StatusApproved {
		return approvalToken{}, deny("run is not approved")
	}
	spec, err := s.LatestSpec(ctx, runID)
	if err != nil {
		return approvalToken{}, fmt.Errorf("%w: latest spec: %w", ErrApprovalDenied, err)
	}
	if spec.RunID != runID || spec.Version < 1 || spec.Hash == "" {
		return approvalToken{}, deny("invalid latest spec")
	}
	if spec.IssueHash != issueHash {
		return approvalToken{}, deny("issue changed after refinement")
	}
	if spec.Version-1 > int64(opts.MaxRevisions) {
		return approvalToken{}, deny("too many spec revisions")
	}
	approval, err := s.ValidApproval(ctx, runID, spec.Version, spec.Hash)
	if err != nil {
		return approvalToken{}, fmt.Errorf("%w: matching approval: %w", ErrApprovalDenied, err)
	}
	if approval.RunID != runID || approval.SpecVersion != spec.Version || approval.SpecHash != spec.Hash || approval.Decision != "approve" {
		return approvalToken{}, deny("approval does not match latest spec")
	}
	now := opts.Clock()
	if approval.CreatedAt.IsZero() || approval.CreatedAt.After(now) || !now.Before(approval.CreatedAt.Add(opts.TTL)) {
		return approvalToken{}, deny("approval expired or has an invalid date")
	}
	if run.WaitingSince != nil && !now.Before(run.WaitingSince.Add(opts.TTL)) {
		return approvalToken{}, deny("approval wait expired")
	}
	return approvalToken{store: s, runID: runID, issueHash: issueHash, spec: spec, approvalID: approval.ID, options: opts}, nil
}

// validateApproval rechecks the capability before Code builds its registry.
// It also binds it to this request, so a token cannot authorize another run,
// issue, or later version. Store errors fail closed.
func validateApproval(ctx context.Context, t approvalToken, runID int64, issueHash string) (store.Spec, error) {
	if t.store == nil || t.runID != runID || t.issueHash != issueHash {
		return store.Spec{}, deny("missing or mismatched capability")
	}
	current, err := Check(ctx, t.store, runID, issueHash, t.options)
	if err != nil {
		return store.Spec{}, err
	}
	if current.spec.Version != t.spec.Version || current.spec.Hash != t.spec.Hash || current.approvalID != t.approvalID {
		return store.Spec{}, deny("approval changed since gate check")
	}
	return current.spec, nil
}

func deny(message string) error { return fmt.Errorf("%w: %s", ErrApprovalDenied, message) }
