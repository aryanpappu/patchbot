package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// Capabilities describes actions allowed by the persisted lifecycle and spec.
// Actions still validate their input, external issue/comment freshness, dispatch
// capacity and concurrent changes when executed.
type Capabilities struct {
	CanApprove bool `json:"can_approve"`
	CanEdit    bool `json:"can_edit"`
	CanRevise  bool `json:"can_revise"`
	CanReject  bool `json:"can_reject"`
	CanCancel  bool `json:"can_cancel"`
	CanRetry   bool `json:"can_retry"`
}

var errRunExpired = errors.New("run expired")

func (r *Runner) requireActionState(ctx context.Context, run store.Run, action string) error {
	err := r.checkActionState(run, action)
	if errors.Is(err, errRunExpired) {
		return errors.Join(err, r.expire(ctx, run))
	}
	return err
}

// ActionCapabilities uses the same guards as the mutating runner methods, on
// the run and latest spec snapshot returned in the detail response.
func (r *Runner) ActionCapabilities(run store.Run, spec *store.Spec) Capabilities {
	allowed := func(action string) bool {
		if r.checkActionState(run, action) != nil {
			return false
		}
		if action == "cancel" || action == "retry" {
			return true
		}
		if spec == nil || r.checkActionSpec(*spec, action) != nil {
			return false
		}
		return action != "approve" || strings.TrimSpace(r.cfg.Approval.LocalActor) != ""
	}
	return Capabilities{
		CanApprove: allowed("approve"), CanEdit: allowed("edit"),
		CanRevise: allowed("revise"), CanReject: allowed("reject"),
		CanCancel: allowed("cancel"), CanRetry: allowed("retry"),
	}
}

func (r *Runner) checkActionState(run store.Run, action string) error {
	var allowed bool
	switch action {
	case "approve":
		allowed = run.Status == store.StatusAwaitingApproval
	case "edit", "revise", "reject":
		allowed = waiting(run.Status) || run.Status == store.StatusApproved
	case "cancel":
		allowed = run.Status.IsActive() || run.Status == store.StatusCancelled
	case "retry":
		allowed = run.Status == store.StatusFailed || run.Status == store.StatusCancelled || run.Status == store.StatusRejected
	}
	if !allowed {
		return store.ErrConflict
	}
	if action != "cancel" && action != "retry" && r.expired(run) {
		return errRunExpired
	}
	return nil
}

func (r *Runner) checkActionSpec(spec store.Spec, action string) error {
	switch action {
	case "edit", "revise":
		if spec.Version-1 >= int64(r.gateOptions().MaxRevisions) {
			return fmt.Errorf("maximum %d spec revisions reached", r.gateOptions().MaxRevisions)
		}
	case "approve":
		if err := r.checkStoredSpec(spec); err != nil {
			return err
		}
		draft, _ := tools.ParseSpec(spec.BodyMD)
		if len(draft.OpenQuestions) != 0 {
			return fmt.Errorf("%w: specification still has open questions", agent.ErrApprovalDenied)
		}
	}
	return nil
}

func (r *Runner) checkStoredSpec(spec store.Spec) error {
	if spec.Version-1 > int64(r.gateOptions().MaxRevisions) {
		return fmt.Errorf("%w: revision limit exceeded", agent.ErrApprovalDenied)
	}
	draft, err := tools.ParseSpec(spec.BodyMD)
	if err != nil || draft.Hash() != spec.Hash {
		return fmt.Errorf("%w: specification content or hash changed", agent.ErrApprovalDenied)
	}
	return nil
}
