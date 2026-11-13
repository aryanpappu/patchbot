package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// reviewSelection is frozen on the run by the approve decision. Execution
// reads only this snapshot, never the mutable configuration.
type reviewSelection struct {
	Enabled         bool    `json:"enabled"`
	Provider        string  `json:"provider,omitempty"`
	Model           string  `json:"model,omitempty"`
	MaxRepairs      int     `json:"max_repairs"`
	MaxTurns        int     `json:"max_turns"`
	ReserveFraction float64 `json:"reserve_fraction"`
	SalvageDraft    bool    `json:"salvage_draft"`
}

// freezeReview resolves the reviewer selection for an approve decision.
func (r *Runner) freezeReview(run store.Run) json.RawMessage {
	c := r.cfg.Review
	sel := reviewSelection{Enabled: c.Enabled}
	if c.Enabled {
		sel.Provider, sel.Model = run.Provider, c.Model
		if sel.Model == "" {
			sel.Model = run.RefineModel
		}
		sel.MaxRepairs, sel.MaxTurns = min(max(c.MaxRepairs, 0), 1), c.MaxTurns
		sel.ReserveFraction, sel.SalvageDraft = c.ReserveFraction, c.SalvageDraft
	}
	raw, _ := json.Marshal(sel)
	return raw
}

// frozenReview parses the run's frozen selection. A run approved without one
// (for example before this feature existed) has review disabled.
func frozenReview(run store.Run) (reviewSelection, error) {
	var sel reviewSelection
	if len(run.ReviewJSON) == 0 {
		return sel, nil
	}
	if err := json.Unmarshal(run.ReviewJSON, &sel); err != nil {
		return sel, errors.New("invalid frozen review selection")
	}
	if !sel.Enabled {
		return reviewSelection{}, nil
	}
	if sel.Provider == "" || sel.Model == "" || strings.ContainsAny(sel.Model, "\r\n\x00") || sel.MaxRepairs < 0 || sel.MaxRepairs > 1 || sel.MaxTurns <= 0 || !(sel.ReserveFraction >= 0 && sel.ReserveFraction <= 0.5) {
		return sel, errors.New("invalid frozen review selection")
	}
	return sel, nil
}

// reserveForReview keeps a fraction of the run's token and cost limits (which
// already include refine spending) and of the remaining active time away from
// coding and repair so a review can still run.
func reserveForReview(lim agent.Limits, fraction float64, now time.Time) agent.Limits {
	if fraction <= 0 {
		return lim
	}
	keep := func(v int64) int64 { return v - int64(float64(v)*fraction) }
	lim.MaxTotalTokens = keep(lim.MaxTotalTokens)
	if lim.MaxCostMicros > 0 {
		lim.MaxCostMicros = keep(lim.MaxCostMicros)
	}
	if !lim.Deadline.IsZero() {
		remaining := max(lim.Deadline.Sub(now), 0)
		lim.Deadline = now.Add(remaining - time.Duration(float64(remaining)*fraction))
	}
	return lim
}

// reviewState tracks review rounds of one code execution.
type reviewState struct {
	sel  reviewSelection
	spec store.Spec
	last *store.Review
	// verdict is the last round's parsed verdict, if it produced one.
	verdict *tools.ReviewVerdict
	// err is a harness failure inside the hook. It always blocks publication.
	err error
}

func (s *reviewState) passed() bool { return s != nil && s.last != nil && s.last.Verdict == "approve" }

// failure explains why review did not pass, for the run error or draft PR.
func (s *reviewState) failure() string {
	switch {
	case s.last == nil:
		return "independent review did not run"
	case s.last.Verdict == "request_changes":
		return fmt.Sprintf("independent review still requested changes after round %d", s.last.Round)
	default:
		return fmt.Sprintf("independent review was incomplete (%s)", s.last.Reason)
	}
}

type snapshot struct{ diff, tree, hash string }

// snapshotDiff hashes the base, the complete worktree tree and the bounded diff
// text. Publication must reproduce exactly this hash.
func (r *Runner) snapshotDiff(ctx context.Context, run store.Run, env *toolEnvironment) (snapshot, error) {
	diff, err := env.repo.Diff(ctx)
	if err != nil {
		return snapshot{}, err
	}
	tree, err := env.repo.TreeID(ctx)
	if err != nil {
		return snapshot{}, err
	}
	raw, _ := json.Marshal([3]string{run.BaseSHA, tree, diff})
	sum := sha256.Sum256(raw)
	return snapshot{diff: diff, tree: tree, hash: hex.EncodeToString(sum[:])}, nil
}

// reviewHook runs one review round per publishable code result. Reviewer
// failures are recorded as incomplete rounds; only harness failures return.
func (r *Runner) reviewHook(run *store.Run, env *toolEnvironment, session *agent.Session, limits agent.Limits, st *reviewState) (func(context.Context, agent.CodeResult) (string, error), error) {
	reviewRun := *run
	reviewRun.Phase = store.PhaseReview
	reviewRun.Provider = st.sel.Provider
	loop, err := r.loop(reviewRun, env, st.sel.Model)
	if err != nil {
		return nil, err
	}
	limits.MaxTurns = st.sel.MaxTurns
	return func(ctx context.Context, result agent.CodeResult) (string, error) {
		feedback, err := r.reviewRound(ctx, run, env, loop, session, limits, st, result)
		if err != nil && st.err == nil {
			st.err = err
		}
		return feedback, err
	}, nil
}

func (r *Runner) reviewRound(ctx context.Context, run *store.Run, env *toolEnvironment, loop *agent.Loop, session *agent.Session, limits agent.Limits, st *reviewState, code agent.CodeResult) (string, error) {
	if err := r.transition(ctx, run, store.StatusRunning, store.PhaseReview, store.RunPatch{}); err != nil {
		return "", err
	}
	snap, err := r.snapshotDiff(ctx, *run, env)
	if err != nil {
		return "", err
	}
	evidence := agent.TestEvidence{Status: code.TestsStatus, Attempts: code.TestAttempts}
	if code.LastTest != nil {
		exit := code.LastTest.ExitCode
		evidence.ExitCode, evidence.TimedOut, evidence.Output = &exit, code.LastTest.TimedOut, code.LastTest.Output
	}
	result, err := loop.Review(ctx, agent.ReviewRequest{Spec: st.spec, Diff: snap.diff, DiffHash: snap.hash, Tests: evidence, Session: session, Limits: limits})
	if err != nil {
		return "", err
	}
	record := store.Review{RunID: run.ID, SpecVersion: st.spec.Version, SpecHash: st.spec.Hash, DiffHash: snap.hash, BaseSHA: run.BaseSHA, TreeID: snap.tree, Provider: st.sel.Provider, Model: st.sel.Model}
	st.verdict = result.Verdict
	if result.Verdict != nil && result.Incomplete == "" {
		record.Verdict = result.Verdict.Verdict
		findings, _ := json.Marshal(result.Verdict.Findings)
		criteria, _ := json.Marshal(result.Verdict.Criteria)
		record.FindingsJSON, record.CriteriaJSON = json.RawMessage(r.redactor.Redact(string(findings))), json.RawMessage(r.redactor.Redact(string(criteria)))
		if !json.Valid(record.FindingsJSON) || !json.Valid(record.CriteriaJSON) {
			record.FindingsJSON, record.CriteriaJSON = nil, nil
		}
	} else {
		record.Verdict, record.Reason = "incomplete", result.Incomplete
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	saved, err := r.db.SaveReview(recordCtx, record)
	if err != nil {
		return "", err
	}
	st.last = &saved
	if err := r.db.SaveArtifact(recordCtx, store.Artifact{RunID: run.ID, Kind: "review", Attempt: saved.Round, Content: tools.CapOutput(r.redactor.Redact(renderReview(saved, st.verdict)), 64<<10)}); err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{"round": saved.Round, "verdict": saved.Verdict, "reason": saved.Reason, "spec_hash": saved.SpecHash, "diff_hash": saved.DiffHash, "model": saved.Model, "repairs": code.ReviewRepairs})
	recorder := agent.StoreRecorder{Store: r.db, RunID: run.ID, Phase: store.PhaseReview, Redactor: r.redactor}
	if err := recorder.Event(recordCtx, "review.verdict", payload); err != nil {
		return "", err
	}
	// Skip a repair the coder could no longer pay for: the run then fails
	// closed now instead of after spending the review reserve on a repair.
	coderAllowance := reserveForReview(limits, st.sel.ReserveFraction, r.now())
	affordable := session.Tokens < coderAllowance.MaxTotalTokens && (coderAllowance.MaxCostMicros <= 0 || session.Cost < coderAllowance.MaxCostMicros)
	if saved.Verdict == "request_changes" && code.ReviewRepairs < st.sel.MaxRepairs && !code.Salvage && affordable {
		return renderFindings(st.verdict), nil
	}
	return "", nil
}

// renderFindings is the coder's repair feedback. The caller wraps it as
// untrusted data; it is model output, not an instruction from the operator.
func renderFindings(v *tools.ReviewVerdict) string {
	var b strings.Builder
	for _, c := range v.Criteria {
		if !c.Met {
			fmt.Fprintf(&b, "Criterion %s is not met.\n", c.ID)
		}
	}
	for _, f := range v.Findings {
		fmt.Fprintf(&b, "[%s] %s:%d %s\n", f.Severity, f.File, f.Line, oneLine(f.Summary, 2000))
	}
	return b.String()
}

func renderReview(review store.Review, v *tools.ReviewVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review round %d: %s\n", review.Round, review.Verdict)
	if review.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s\n", review.Reason)
	}
	fmt.Fprintf(&b, "Reviewer: %s / %s\nSpec v%d SHA-256 %s\nDiff SHA-256 %s\nTree %s\n", review.Provider, review.Model, review.SpecVersion, review.SpecHash, review.DiffHash, review.TreeID)
	if v != nil && review.Verdict != "incomplete" {
		b.WriteString("\nCriteria:\n")
		for _, c := range v.Criteria {
			fmt.Fprintf(&b, "- %s: %s\n", c.ID, map[bool]string{true: "met", false: "not met"}[c.Met])
		}
		b.WriteString("\nFindings:\n")
		if len(v.Findings) == 0 {
			b.WriteString("- none\n")
		}
		for _, f := range v.Findings {
			fmt.Fprintf(&b, "- [%s] %s:%d %s\n", f.Severity, f.File, f.Line, oneLine(f.Summary, 2000))
		}
	}
	return b.String()
}

// prReview renders review evidence for the PR body. Findings are reviewer
// output: one line each, bounded, and never presented as a quality guarantee.
func prReview(st *reviewState) string {
	if st == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n### Independent review\n\n")
	if st.last == nil {
		b.WriteString("Review did not run.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Verdict: **%s** (round %d; %s / %s). Bound to spec SHA-256 `%s` and diff SHA-256 `%s`.\n", st.last.Verdict, st.last.Round, st.last.Provider, st.last.Model, st.last.SpecHash, st.last.DiffHash)
	if st.last.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s.\n", st.last.Reason)
	}
	if st.verdict != nil && st.last.Verdict != "incomplete" {
		b.WriteString("\nCriteria (reviewer assessment):")
		for _, c := range st.verdict.Criteria {
			fmt.Fprintf(&b, " %s %s;", c.ID, map[bool]string{true: "met", false: "not met"}[c.Met])
		}
		b.WriteString("\n\nFindings:\n\n")
		if len(st.verdict.Findings) == 0 {
			b.WriteString("- none\n")
		}
		for _, f := range st.verdict.Findings {
			fmt.Fprintf(&b, "- %s %s: %s\n", f.Severity, codeSpan(fmt.Sprintf("%s:%d", oneLine(f.File, 200), f.Line)), codeSpan(oneLine(f.Summary, 500)))
		}
	}
	b.WriteString("\nThe review is advisory evidence. It does not replace the approval gate, the harness tests or human review.\n")
	return b.String()
}

// codeSpan renders reviewer text inertly in Markdown: no links, images, HTML,
// mentions or issue references are interpreted inside a code span.
func codeSpan(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }

func oneLine(s string, limit int) string {
	return tools.CapOutput(strings.Join(strings.Fields(s), " "), limit)
}
