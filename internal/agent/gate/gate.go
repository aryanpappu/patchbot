// Package gate issues approval capabilities bound to a current persisted spec.
package gate

import (
 "context"
 "errors"
 "fmt"
 "time"

 "github.com/aryanpappu/patchbot/internal/store"
)

// ErrDenied means code must not start or register write tools.
var ErrDenied = errors.New("approval gate denied")

// Store is the read boundary needed to validate an approval. store.Store
// implements it. The gate never changes run status or records a decision.
type Store interface {
 GetRun(context.Context, int64) (store.Run, error)
 LatestSpec(context.Context, int64) (store.Spec, error)
 ValidApproval(context.Context, int64, int64, string) (store.Approval, error)
}

// Options is trusted policy. Zero values select the product defaults.
type Options struct {
 TTL time.Duration
 MaxRevisions int
 Clock func() time.Time
}

// ApprovalToken is sealed; the only accepted implementation is the private
// value returned by Check. Nil and user implementations are refused by Validate.
type ApprovalToken interface { approvalToken() }

type token struct {
 store Store
 runID int64
 issueHash string
 spec store.Spec
 approvalID int64
 options Options
}

func (token) approvalToken() {}

// Check returns a capability only for the latest approved version and issue.
// The concrete return type and all its fields are private. A caller can pass
// the inferred value to agent.Code, but cannot construct one itself.
func Check(ctx context.Context, s Store, runID int64, issueHash string, options ...Options) (token, error) {
 if s == nil || runID <= 0 || issueHash == "" || len(options) > 1 { return token{}, deny("missing gate inputs") }
 opts := Options{}
 if len(options) == 1 { opts = options[0] }
 if opts.TTL < 0 || opts.MaxRevisions < 0 { return token{}, deny("invalid approval policy") }
 if opts.TTL == 0 { opts.TTL = 14*24*time.Hour }
 if opts.MaxRevisions == 0 { opts.MaxRevisions = 5 }
 if opts.Clock == nil { opts.Clock = time.Now }
 run, err := s.GetRun(ctx, runID)
 if err != nil { return token{}, fmt.Errorf("%w: read run: %w", ErrDenied, err) }
 if run.ID != runID || run.Status != store.StatusApproved { return token{}, deny("run is not approved") }
 spec, err := s.LatestSpec(ctx, runID)
 if err != nil { return token{}, fmt.Errorf("%w: latest spec: %w", ErrDenied, err) }
 if spec.RunID != runID || spec.Version < 1 || spec.Hash == "" { return token{}, deny("invalid latest spec") }
 if spec.IssueHash != issueHash { return token{}, deny("issue changed after refinement") }
 if spec.Version-1 > int64(opts.MaxRevisions) { return token{}, deny("too many spec revisions") }
 approval, err := s.ValidApproval(ctx, runID, spec.Version, spec.Hash)
 if err != nil { return token{}, fmt.Errorf("%w: matching approval: %w", ErrDenied, err) }
 if approval.RunID != runID || approval.SpecVersion != spec.Version || approval.SpecHash != spec.Hash || approval.Decision != "approve" { return token{}, deny("approval does not match latest spec") }
 now := opts.Clock()
 if approval.CreatedAt.IsZero() || approval.CreatedAt.After(now) || !now.Before(approval.CreatedAt.Add(opts.TTL)) { return token{}, deny("approval expired or has an invalid date") }
 if run.WaitingSince != nil && !now.Before(run.WaitingSince.Add(opts.TTL)) { return token{}, deny("approval wait expired") }
 return token{store:s, runID:runID, issueHash:issueHash, spec:spec, approvalID:approval.ID, options:opts}, nil
}

// Validate rechecks the capability immediately before Code builds its registry.
// It also binds it to this request, so a token cannot authorize another run,
// issue, or later version. Store errors fail closed.
func Validate(ctx context.Context, capability ApprovalToken, runID int64, issueHash string) (store.Spec, error) {
 t, ok := capability.(token)
 if !ok || t.store == nil || t.runID != runID || t.issueHash != issueHash { return store.Spec{}, deny("missing or mismatched capability") }
 current, err := Check(ctx, t.store, runID, issueHash, t.options)
 if err != nil { return store.Spec{}, err }
 if current.spec.Version != t.spec.Version || current.spec.Hash != t.spec.Hash || current.approvalID != t.approvalID { return store.Spec{}, deny("approval changed since gate check") }
 return current.spec, nil
}

func deny(message string) error { return fmt.Errorf("%w: %s", ErrDenied, message) }
