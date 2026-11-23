package runner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/store"
)

// Action kinds recorded for escalation. The old run gets "escalated" (its
// payload names the new run); the new run gets "escalation".
const (
	actionEscalated  = "escalated"
	actionEscalation = "escalation"
	escalationActor  = "patchbot"
	escalationChan   = "escalation"
)

// EscalationInfo describes where a run sits on the support-level ladder.
type EscalationInfo struct {
	Enabled bool `json:"enabled"`
	// Level is 1-based; 0 means the run's provider and code model are not on the ladder.
	Level  int                     `json:"level"`
	Levels int                     `json:"levels"`
	Next   *config.EscalationLevel `json:"next,omitempty"`
	// From is the run this one escalated from; To is the run it escalated to.
	From    *int64 `json:"from,omitempty"`
	To      *int64 `json:"to,omitempty"`
	Trigger string `json:"trigger,omitempty"`
}

type escalationPayload struct {
	FromRun int64  `json:"from_run,omitempty"`
	ToRun   int64  `json:"to_run,omitempty"`
	Level   int    `json:"level"`
	Trigger string `json:"trigger"`
}

// level returns the run's 0-based ladder index, or -1 when it is not on it.
func (r *Runner) level(run store.Run) int {
	return slices.Index(r.cfg.Escalation.Levels, config.EscalationLevel{Provider: run.Provider, Model: run.CodeModel})
}

func (r *Runner) nextLevel(run store.Run) (config.EscalationLevel, int, bool) {
	i := r.level(run)
	if i < 0 || i+1 >= len(r.cfg.Escalation.Levels) {
		return config.EscalationLevel{}, 0, false
	}
	return r.cfg.Escalation.Levels[i+1], i + 1, true
}

// escalationTrigger reports which configured trigger, if any, run ended with.
func (r *Runner) escalationTrigger(run store.Run) (string, bool) {
	on := r.cfg.Escalation.On
	switch {
	case run.Status == store.StatusFailed && slices.Contains(on, run.ErrorKind):
		return run.ErrorKind, true
	case run.Status == store.StatusSucceeded && run.DryRun && run.PRNumber == nil && run.TestsStatus == "failed" && slices.Contains(on, "tests_failed"):
		return "tests_failed", true
	}
	return "", false
}

// afterRun starts the next escalation level when a finished run qualifies. It
// is best effort: any problem is recorded as an artifact on the old run and the
// run is left for a person, exactly as without escalation.
func (r *Runner) afterRun(ctx context.Context, id int64) {
	if !r.cfg.Escalation.Enabled {
		return
	}
	ctx = context.WithoutCancel(ctx)
	run, err := r.db.GetRun(ctx, id)
	if err != nil {
		return
	}
	trigger, ok := r.escalationTrigger(run)
	if !ok {
		return
	}
	next, index, ok := r.nextLevel(run)
	if !ok {
		_ = r.artifact(ctx, id, "escalation_note", "Escalation stopped: this run is on the top level or not on the configured ladder. A person needs to look at it.")
		return
	}
	if to, _ := r.escalatedTo(ctx, id); to != nil {
		return // already escalated
	}
	// Record the decision first so a crash or a second caller cannot start two retries.
	if err := r.action(ctx, id, actionEscalated, escalationActor, escalationChan, escalationPayload{Level: index + 1, Trigger: trigger}); err != nil {
		return
	}
	newRun, err := r.retry(WithActor(ctx, escalationActor), run, next.Provider, next.Model)
	if newRun.ID == 0 {
		_ = r.artifact(ctx, id, "escalation_note", "Escalation to the next level could not start: "+r.redactor.Redact(errString(err)))
		return
	}
	_ = r.action(ctx, newRun.ID, actionEscalation, escalationActor, escalationChan, escalationPayload{FromRun: id, Level: index + 1, Trigger: trigger})
	_ = r.action(ctx, id, actionEscalated, escalationActor, escalationChan, escalationPayload{ToRun: newRun.ID, Level: index + 1, Trigger: trigger})
}

func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// escalatedTo returns the run this one escalated to, if any. A recorded
// decision without a target still counts as escalated.
func (r *Runner) escalatedTo(ctx context.Context, id int64) (*int64, error) {
	actions, err := r.db.ListActions(ctx, id)
	if err != nil {
		return nil, err
	}
	var found *int64
	for _, a := range actions {
		if a.Kind != actionEscalated {
			continue
		}
		var p escalationPayload
		if json.Unmarshal(a.PayloadJSON, &p) == nil && p.ToRun != 0 {
			to := p.ToRun
			found = &to
		} else if found == nil {
			zero := int64(0)
			found = &zero
		}
	}
	return found, nil
}

// Escalation reports the run's ladder position and links for the dashboard.
func (r *Runner) Escalation(ctx context.Context, run store.Run) (EscalationInfo, error) {
	e := r.cfg.Escalation
	info := EscalationInfo{Enabled: e.Enabled, Levels: len(e.Levels), Level: r.level(run) + 1}
	if next, _, ok := r.nextLevel(run); ok {
		info.Next = &next
	}
	actions, err := r.db.ListActions(ctx, run.ID)
	if err != nil {
		return info, err
	}
	for _, a := range actions {
		var p escalationPayload
		if json.Unmarshal(a.PayloadJSON, &p) != nil {
			continue
		}
		switch {
		case a.Kind == actionEscalation && p.FromRun != 0:
			from := p.FromRun
			info.From, info.Trigger = &from, p.Trigger
		case a.Kind == actionEscalated && p.ToRun != 0:
			to := p.ToRun
			info.To = &to
		}
	}
	return info, nil
}

// ErrNotOnLadder is returned by Escalate when no higher level exists for the run.
var ErrNotOnLadder = errors.New("run is not below the top of the escalation ladder")

// escalatable reports whether a finished run may be escalated by hand: any run
// Retry accepts, or a dry run that ended with its tests still failing.
func (r *Runner) escalatable(run store.Run) bool {
	if r.checkActionState(run, "retry") == nil {
		return true
	}
	return run.Status == store.StatusSucceeded && run.DryRun && run.PRNumber == nil && run.TestsStatus == "failed"
}

// Escalate is the manual "next level" retry: a Retry with the next ladder
// level. A run that already escalated cannot escalate again.
func (r *Runner) Escalate(ctx context.Context, id int64) (store.Run, error) {
	old, err := r.db.GetRun(ctx, id)
	if err != nil {
		return store.Run{}, err
	}
	if !r.escalatable(old) {
		return store.Run{}, store.ErrConflict
	}
	if to, err := r.escalatedTo(ctx, id); err != nil || to != nil {
		return store.Run{}, errors.Join(store.ErrConflict, err)
	}
	next, index, ok := r.nextLevel(old)
	if !ok {
		return store.Run{}, ErrNotOnLadder
	}
	run, err := r.retry(ctx, old, next.Provider, next.Model)
	if run.ID != 0 {
		err = errors.Join(err,
			r.action(context.WithoutCancel(ctx), run.ID, actionEscalation, r.actorFor(ctx), r.actionChannel(), escalationPayload{FromRun: id, Level: index + 1, Trigger: "manual"}),
			r.action(context.WithoutCancel(ctx), id, actionEscalated, r.actorFor(ctx), r.actionChannel(), escalationPayload{ToRun: run.ID, Level: index + 1, Trigger: "manual"}))
	}
	return run, err
}
