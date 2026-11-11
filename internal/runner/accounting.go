package runner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/store"
)

type accounting struct {
	Cost, Tokens        int64
	Turns               int
	Uncertain, Unpriced bool
	Active              time.Duration
}

func (r *Runner) session(ctx context.Context, run store.Run) (*agent.Session, runLimits, accounting, error) {
	var limits runLimits
	if err := json.Unmarshal(run.LimitsJSON, &limits); err != nil {
		return nil, limits, accounting{}, errors.New("invalid stored run limits")
	}
	_, hard, err := r.cfg.MaxCostMicro()
	if err != nil {
		return nil, limits, accounting{}, err
	}
	if limits.Agent.MaxCostMicros <= 0 || hard <= 0 || limits.Agent.MaxTotalTokens <= 0 || limits.Timeout <= 0 {
		return nil, limits, accounting{}, errors.New("invalid stored run budget")
	}
	limits.Agent.MaxCostMicros = min(limits.Agent.MaxCostMicros, hard)
	limits.Agent.MaxTestAttempts = min(max(limits.Agent.MaxTestAttempts, 1), 3)
	var totals accounting
	saved, err := r.db.GetArtifact(ctx, run.ID, "accounting", 0)
	if err == nil {
		err = json.Unmarshal([]byte(saved.Content), &totals)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, limits, totals, err
	}
	// Usage events are written after every call, including retries and unknown
	// usage reservations. They survive a new CLI process and never reset budget.
	var after int64
	for {
		events, err := r.db.EventsAfter(ctx, run.ID, after, 500)
		if err != nil {
			return nil, limits, totals, err
		}
		for _, event := range events {
			after = event.Seq
			if event.Type != "usage" {
				continue
			}
			var usage struct {
				Cost      int64 `json:"total_cost_micros"`
				Tokens    int64 `json:"total_tokens"`
				Turns     int   `json:"turn"`
				Uncertain bool  `json:"accounting_uncertain"`
				Unpriced  bool  `json:"unpriced"`
			}
			if err := json.Unmarshal(event.PayloadJSON, &usage); err != nil {
				return nil, limits, totals, errors.New("invalid persisted accounting event")
			}
			totals.Cost, totals.Tokens, totals.Turns = max(totals.Cost, usage.Cost), max(totals.Tokens, usage.Tokens), max(totals.Turns, usage.Turns)
			totals.Uncertain, totals.Unpriced = totals.Uncertain || usage.Uncertain, totals.Unpriced || usage.Unpriced
		}
		if len(events) < 500 {
			break
		}
	}
	limits.Agent.Deadline = r.now().Add(max(limits.Timeout-totals.Active, 0))
	return &agent.Session{Cost: totals.Cost, Tokens: totals.Tokens, Turns: totals.Turns, AccountingUncertain: totals.Uncertain, Unpriced: totals.Unpriced}, limits, totals, nil
}

func (r *Runner) saveAccounting(ctx context.Context, run store.Run, session *agent.Session, prior accounting, start time.Time) error {
	totals := accounting{Cost: session.Cost, Tokens: session.Tokens, Turns: session.Turns, Uncertain: session.AccountingUncertain, Unpriced: session.Unpriced, Active: prior.Active + time.Since(start)}
	raw, err := json.Marshal(totals)
	if err != nil {
		return err
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return r.db.SaveArtifact(recordCtx, store.Artifact{RunID: run.ID, Kind: "accounting", Content: string(raw)})
}

func (r *Runner) loop(run store.Run, env *toolEnvironment, model string) (*agent.Loop, error) {
	p, pricing, err := r.provider(run.Provider)
	if err != nil {
		return nil, err
	}
	return &agent.Loop{Provider: p, Model: model, Env: env.env, Pricing: pricing, Clock: r.now, Recorder: &agent.StoreRecorder{Store: r.db, RunID: run.ID, Phase: run.Phase, Redactor: r.redactor}}, nil
}

func budgetError(reason agent.EndReason) error {
	kind := string(reason)
	if reason == agent.EndBudget {
		kind = "budget_exhausted"
	}
	return &agent.Error{Kind: kind, Message: "agent stopped: " + string(reason)}
}
