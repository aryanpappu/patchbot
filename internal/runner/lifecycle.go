package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

var errCommentEdited = errors.New("GitHub spec comment was edited; use /revise or edit the spec in patchbot")

func (r *Runner) gateOptions() agent.ApprovalOptions {
	ttl := r.cfg.Approval.TTL
	if ttl <= 0 || ttl > 14*24*time.Hour {
		ttl = 14 * 24 * time.Hour
	}
	revisions := r.cfg.Approval.MaxRevisions
	if revisions <= 0 || revisions > 5 {
		revisions = 5
	}
	return agent.ApprovalOptions{TTL: ttl, MaxRevisions: revisions, Clock: r.now}
}

func waiting(s store.Status) bool {
	return s == store.StatusAwaitingApproval || s == store.StatusNeedsInfo
}

func (r *Runner) expired(run store.Run) bool {
	return waiting(run.Status) && run.WaitingSince != nil && !r.now().Before(run.WaitingSince.Add(r.gateOptions().TTL))
}

func (r *Runner) expire(ctx context.Context, run store.Run) error {
	kind, message, outcome := "expired", "approval wait expired", "expired"
	return r.transition(ctx, &run, store.StatusFailed, run.Phase, store.RunPatch{ErrorKind: &kind, ErrorMessage: &message, Outcome: &outcome})
}

func (r *Runner) waitingRuns(ctx context.Context) ([]store.Run, error) {
	var runs []store.Run
	filter := store.RunFilter{Statuses: []store.Status{store.StatusAwaitingApproval, store.StatusNeedsInfo}, Limit: 500}
	for {
		page, err := r.db.ListRuns(ctx, filter)
		if err != nil {
			return nil, err
		}
		runs = append(runs, page.Runs...)
		if page.NextCursor == "" {
			return runs, nil
		}
		filter.Cursor = page.NextCursor
	}
}

func (r *Runner) expireWaiting(ctx context.Context) error {
	runs, err := r.waitingRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if r.expired(run) {
			if err := r.expire(ctx, run); err != nil && !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) checkedSpec(ctx context.Context, run store.Run) (store.Spec, error) {
	spec, err := r.db.LatestSpec(ctx, run.ID)
	if err != nil {
		return spec, err
	}
	if spec.Version-1 > int64(r.gateOptions().MaxRevisions) {
		return spec, fmt.Errorf("%w: revision limit exceeded", agent.ErrApprovalDenied)
	}
	draft, err := tools.ParseSpec(spec.BodyMD)
	if err != nil || draft.Hash() != spec.Hash {
		return spec, fmt.Errorf("%w: specification content or hash changed", agent.ErrApprovalDenied)
	}
	issue, err := r.gh.GetIssue(ctx, ref(run.Repo), int(run.IssueNumber))
	if err != nil {
		return spec, err
	}
	if agent.IssueHash(issue.Title, issue.Body) != spec.IssueHash {
		return spec, fmt.Errorf("%w: issue changed; revise the specification", agent.ErrApprovalDenied)
	}
	if spec.CommentID != nil && !run.DryRun {
		comment, err := r.gh.GetIssueComment(ctx, ref(run.Repo), *spec.CommentID)
		if err != nil {
			return spec, err
		}
		if comment.Body != r.specComment(run, spec) {
			return spec, fmt.Errorf("%w: %w", agent.ErrApprovalDenied, errCommentEdited)
		}
	}
	return spec, nil
}

// Approve accepts only an approval-ready latest spec, under a named local actor.
// Execute is separate so a server can dispatch coding without blocking an action.
func (r *Runner) Approve(ctx context.Context, id int64, approver string) error {
	return r.approve(ctx, id, approver, "cli", nil)
}

func (r *Runner) approve(ctx context.Context, id int64, actor, channel string, expected *store.Spec) error {
	lock := r.control(id)
	lock.Lock()
	defer lock.Unlock()
	run, err := r.db.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if run.Status != store.StatusAwaitingApproval {
		return store.ErrConflict
	}
	if r.expired(run) {
		return errors.Join(errors.New("run expired"), r.expire(ctx, run))
	}
	spec, err := r.checkedSpec(ctx, run)
	if err != nil {
		return err
	}
	if expected != nil && (spec.Version != expected.Version || spec.Hash != expected.Hash) {
		return store.ErrConflict
	}
	draft, _ := tools.ParseSpec(spec.BodyMD)
	if len(draft.OpenQuestions) != 0 {
		return fmt.Errorf("%w: specification still has open questions", agent.ErrApprovalDenied)
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("approver name is required")
	}
	return r.db.DecideRun(ctx, run.Status, store.StatusApproved, store.Approval{RunID: id, SpecVersion: spec.Version, SpecHash: spec.Hash, Decision: "approve", Actor: r.redactor.Redact(actor), Channel: channel, CreatedAt: r.now()}, run.Generation)
}

// Revise records notes, invalidates the previous decision and reruns refinement.
func (r *Runner) Revise(ctx context.Context, id int64, notes string) error {
	return r.revise(ctx, id, notes, r.cfg.Approval.LocalActor, "cli", nil)
}

func (r *Runner) revise(ctx context.Context, id int64, notes, actor, channel string, expected *store.Spec) error {
	if strings.TrimSpace(notes) == "" {
		return errors.New("revision notes are required")
	}
	lock := r.control(id)
	lock.Lock()
	run, err := r.db.GetRun(ctx, id)
	if err != nil {
		lock.Unlock()
		return err
	}
	if !waiting(run.Status) && run.Status != store.StatusApproved {
		lock.Unlock()
		return store.ErrConflict
	}
	if r.expired(run) {
		lock.Unlock()
		return errors.Join(errors.New("run expired"), r.expire(ctx, run))
	}
	spec, err := r.db.LatestSpec(ctx, id)
	if err == nil && expected != nil && (spec.Version != expected.Version || spec.Hash != expected.Hash) {
		err = store.ErrConflict
	}
	if err == nil && spec.Version-1 >= int64(r.gateOptions().MaxRevisions) {
		err = errors.New("maximum 5 spec revisions reached")
	}
	if err == nil {
		err = r.db.DecideRun(ctx, run.Status, store.StatusRunning, store.Approval{RunID: id, SpecVersion: spec.Version, SpecHash: spec.Hash, Decision: "revise", Actor: r.redactor.Redact(actor), Channel: channel, CreatedAt: r.now()}, run.Generation)
	}
	lock.Unlock()
	if err != nil {
		return err
	}
	run.Status, run.Phase = store.StatusRunning, store.PhasePreflight
	run.Generation++
	notes = r.redactor.Redact(notes)
	if err := r.action(ctx, id, "revise", actor, channel, map[string]string{"notes": notes}); err != nil {
		return r.fail(ctx, run, err)
	}
	err = r.withWork(ctx, id, func(work context.Context) error { return r.refine(work, &run, &spec, notes, nil) })
	if err != nil {
		return r.fail(ctx, run, err)
	}
	return nil
}

// EditSpec creates a canonical UI version; an older approval cannot authorize it.
func (r *Runner) EditSpec(ctx context.Context, id int64, draft tools.SpecDraft) (store.Spec, error) {
	lock := r.control(id)
	lock.Lock()
	defer lock.Unlock()
	run, err := r.db.GetRun(ctx, id)
	if err != nil {
		return store.Spec{}, err
	}
	if !waiting(run.Status) && run.Status != store.StatusApproved {
		return store.Spec{}, store.ErrConflict
	}
	if r.expired(run) {
		return store.Spec{}, errors.Join(errors.New("run expired"), r.expire(ctx, run))
	}
	prior, err := r.db.LatestSpec(ctx, id)
	if err != nil {
		return store.Spec{}, err
	}
	if prior.Version-1 >= int64(r.gateOptions().MaxRevisions) {
		return store.Spec{}, errors.New("maximum 5 spec revisions reached")
	}
	draft = *(&tools.Submission{Spec: &draft}).Redacted(r.redactor).Spec
	// Apply the refinement content and path rules to operator edits.
	if err := validateDraft(ctx, run, r.cfg.Repos[run.Repo], draft); err != nil {
		return store.Spec{}, err
	}
	issue, err := r.gh.GetIssue(ctx, ref(run.Repo), int(run.IssueNumber))
	if err != nil {
		return store.Spec{}, err
	}
	status := store.StatusAwaitingApproval
	if len(draft.OpenQuestions) != 0 {
		status = store.StatusNeedsInfo
	}
	spec, err := r.db.EditSpec(ctx, store.SpecEdit{
		Spec: store.Spec{RunID: id, BodyMD: draft.Markdown(), Hash: draft.Hash(), IssueHash: agent.IssueHash(issue.Title, issue.Body), Source: "ui_edit", CreatedAt: r.now()},
		From: run.Status, To: status, Generation: run.Generation, PriorVersion: prior.Version, PriorHash: prior.Hash, Actor: r.redactor.Redact(r.cfg.Approval.LocalActor),
	})
	if err != nil {
		return spec, err
	}
	err = r.postSpec(ctx, run, &spec, nil)
	return spec, err
}

// Reject closes a waiting or approved run without starting publication.
func (r *Runner) Reject(ctx context.Context, id int64) error {
	lock := r.control(id)
	lock.Lock()
	defer lock.Unlock()
	run, err := r.db.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if !waiting(run.Status) && run.Status != store.StatusApproved {
		return store.ErrConflict
	}
	if r.expired(run) {
		return errors.Join(errors.New("run expired"), r.expire(ctx, run))
	}
	spec, err := r.db.LatestSpec(ctx, id)
	if err != nil {
		return err
	}
	return r.db.DecideRun(ctx, run.Status, store.StatusRejected, store.Approval{RunID: id, SpecVersion: spec.Version, SpecHash: spec.Hash, Decision: "reject", Actor: r.redactor.Redact(r.cfg.Approval.LocalActor), Channel: "cli", CreatedAt: r.now()}, run.Generation)
}

// Cancel stops local work immediately and CASes the persisted active status.
func (r *Runner) Cancel(ctx context.Context, id int64) error {
	r.mu.Lock()
	cancel := r.active[id]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for range 5 {
		run, err := r.db.GetRun(ctx, id)
		if err != nil {
			return err
		}
		if !run.Status.IsActive() {
			if run.Status == store.StatusCancelled {
				return nil
			}
			return store.ErrConflict
		}
		if err := r.action(ctx, id, "cancel", r.cfg.Approval.LocalActor, "cli", nil); err != nil {
			return err
		}
		outcome := "cancelled"
		err = r.transition(ctx, &run, store.StatusCancelled, run.Phase, store.RunPatch{Outcome: &outcome})
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return store.ErrConflict
}

func (r *Runner) withWork(ctx context.Context, id int64, work func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if r.active[id] != nil {
		r.mu.Unlock()
		cancel()
		return store.ErrConflict
	}
	r.active[id] = cancel
	r.mu.Unlock()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(r.opts.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				beatCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				run, err := r.db.GetRun(beatCtx, id)
				if err == nil && (run.Status == store.StatusRunning || (run.Status == store.StatusApproved && (run.Phase == store.PhaseClone || run.Phase == store.PhaseBaseline))) {
					err = r.db.Heartbeat(beatCtx, id)
					if errors.Is(err, store.ErrConflict) {
						// Parking or finishing can win after the read. A heartbeat
						// CAS conflict is not a cancellation of that successful work.
						run, err = r.db.GetRun(beatCtx, id)
					}
				}
				stop()
				if run.Status == store.StatusSucceeded {
					return
				}
				if err != nil || !run.Status.IsActive() {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		close(done)
		cancel()
		<-stopped
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
	}()
	return work(ctx)
}

func (r *Runner) fail(ctx context.Context, run store.Run, cause error) error {
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	current, err := r.db.GetRun(cleanup, run.ID)
	if err != nil {
		return errors.Join(cause, err)
	}
	if !current.Status.IsActive() || current.Generation != run.Generation {
		return cause
	}
	kind := "internal"
	var classified *agent.Error
	if errors.As(cause, &classified) {
		kind = classified.Kind
	}
	status, outcome := store.StatusFailed, "error"
	if errors.Is(cause, context.Canceled) {
		status, outcome, kind = store.StatusCancelled, "cancelled", "cancelled"
	}
	if errors.Is(cause, agent.ErrApprovalDenied) {
		kind = "approval_denied"
	}
	message := tools.CapOutput(r.redactor.Redact(cause.Error()), 2000)
	err = r.transition(cleanup, &run, status, run.Phase, store.RunPatch{Outcome: &outcome, ErrorKind: &kind, ErrorMessage: &message})
	if err == nil && status == store.StatusFailed && !run.DryRun && kind != "provider_auth" && r.cfg.Defaults.CommentOnFailure && !r.opts.DisableFailureComments {
		_, commentErr := r.gh.CreateIssueComment(cleanup, ref(run.Repo), int(run.IssueNumber), fmt.Sprintf("patchbot run %d stopped during %s (%s). Review the run locally before retrying.", run.ID, run.Phase, kind))
		if commentErr != nil {
			err = r.artifact(cleanup, run.ID, "failure_comment_warning", "Could not post failure comment.")
		}
	}
	return errors.Join(cause, err)
}

// Retry creates a new run. A still-valid approved spec is copied only for an
// unchanged issue; its provenance is retained and the ordinary gate is used.
func (r *Runner) Retry(ctx context.Context, id int64, provider, model string) (store.Run, error) {
	old, err := r.db.GetRun(ctx, id)
	if err != nil {
		return store.Run{}, err
	}
	if old.Status != store.StatusFailed && old.Status != store.StatusCancelled && old.Status != store.StatusRejected {
		return store.Run{}, store.ErrConflict
	}
	request := Request{Repo: old.Repo, Issue: old.IssueNumber, Mode: old.Mode, Provider: provider, Model: model, DryRun: old.DryRun}
	if request.Provider == "" {
		request.Provider = old.Provider
	}
	run, err := r.prepare(request)
	if err != nil {
		return run, err
	}
	if model == "" && request.Provider == old.Provider {
		run.RefineModel, run.CodeModel = old.RefineModel, old.CodeModel
	}
	run.RetryOf = &id
	var reuse *store.Spec
	if old.Mode == config.ModeFix {
		spec, specErr := r.checkedSpec(ctx, old)
		if specErr == nil {
			approval, aErr := r.db.ValidApproval(ctx, id, spec.Version, spec.Hash)
			if aErr == nil && r.now().Before(approval.CreatedAt.Add(r.gateOptions().TTL)) {
				reuse = &spec
			}
		}
	}
	run, err = r.start(ctx, run, reuse)
	if run.ID != 0 {
		err = errors.Join(err, r.action(context.WithoutCancel(ctx), run.ID, "retry", r.cfg.Approval.LocalActor, "cli", map[string]int64{"retry_of": id}))
	}
	return run, err
}
