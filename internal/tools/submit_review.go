package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aryanpappu/patchbot/internal/llm"
)

// Review verdicts a reviewer can submit. The harness records a third state,
// incomplete, for every review that ends without a valid submission.
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request_changes"
)

// Finding severities. Critical and major findings block an approval.
var reviewSeverities = map[string]bool{"critical": true, "major": true, "minor": true, "info": true}

// ReviewFinding is one reviewer observation about the actual diff. It is model
// output: the harness treats it as untrusted evidence and feedback only.
type ReviewFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// ReviewCriterion assesses one approved acceptance criterion by its ID (AC1...).
type ReviewCriterion struct {
	ID  string `json:"id"`
	Met bool   `json:"met"`
}

// ReviewVerdict is the structured, validated reviewer submission.
type ReviewVerdict struct {
	Verdict  string            `json:"verdict"`
	Findings []ReviewFinding   `json:"findings"`
	Criteria []ReviewCriterion `json:"criteria"`
}

// Blocking reports whether a finding prevents approval.
func (f ReviewFinding) Blocking() bool { return f.Severity == "critical" || f.Severity == "major" }

// SubmitReview is the terminal reviewer tool. CriteriaIDs are the trusted
// approved criterion IDs; every one must be assessed exactly once.
type SubmitReview struct{ CriteriaIDs []string }

// Terminal marks submit_review as a stage-ending tool.
func (SubmitReview) Terminal() bool { return true }

// Spec describes submit_review.
func (SubmitReview) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "submit_review", Description: "Finish the review with a structured verdict on the diff against the approved specification. approve requires every criterion met and no critical or major finding; otherwise use request_changes with at least one finding.", Schema: json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string","enum":["approve","request_changes"],"description":"approve or request_changes."},"findings":{"type":"array","description":"Concrete problems in the diff, at most 30.","items":{"type":"object","properties":{"file":{"type":"string","description":"Repository-relative path, or empty for the whole patch."},"line":{"type":"integer","minimum":0,"description":"Line in the new file, or 0 when not applicable."},"severity":{"type":"string","enum":["critical","major","minor","info"]},"summary":{"type":"string","description":"What is wrong and why."}},"required":["file","line","severity","summary"],"additionalProperties":false}},"criteria":{"type":"array","description":"Every approved criterion ID exactly once.","items":{"type":"object","properties":{"id":{"type":"string","description":"Criterion ID such as AC1."},"met":{"type":"boolean"}},"required":["id","met"],"additionalProperties":false}}},"required":["verdict","findings","criteria"],"additionalProperties":false}`)}
}

// Execute validates the verdict. A mistake is returned to the model so it can
// correct it; a review that never submits a valid verdict is incomplete.
func (t SubmitReview) Execute(ctx context.Context, _ *Env, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var v ReviewVerdict
	if err := decodeArgs(raw, &v); err != nil {
		return failed("%v", err)
	}
	if v.Verdict != VerdictApprove && v.Verdict != VerdictRequestChanges {
		return failed("verdict must be approve or request_changes")
	}
	if len(v.Findings) > 30 {
		return failed("report at most 30 findings")
	}
	blocking := false
	for _, f := range v.Findings {
		if !reviewSeverities[f.Severity] || f.Line < 0 || strings.TrimSpace(f.Summary) == "" || len(f.Summary) > 2000 || len(f.File) > 512 || strings.ContainsAny(f.File, "\x00\r\n") {
			return failed("each finding needs a valid severity, a line of 0 or more, a nonempty summary up to 2000 bytes and a file path up to 512 bytes")
		}
		blocking = blocking || f.Blocking()
	}
	if len(v.Criteria) != len(t.CriteriaIDs) {
		return failed("assess every approved criterion exactly once: %s", strings.Join(t.CriteriaIDs, ", "))
	}
	want := map[string]bool{}
	for _, id := range t.CriteriaIDs {
		want[id] = true
	}
	allMet := true
	for _, c := range v.Criteria {
		if !want[c.ID] {
			return failed("criterion IDs must match the approved IDs exactly once: %s", strings.Join(t.CriteriaIDs, ", "))
		}
		delete(want, c.ID)
		allMet = allMet && c.Met
	}
	if v.Verdict == VerdictApprove && (!allMet || blocking) {
		return failed("approve requires every criterion met and no critical or major finding; use request_changes")
	}
	if v.Verdict == VerdictRequestChanges && len(v.Findings) == 0 {
		return failed("request_changes requires at least one finding")
	}
	if v.Findings == nil {
		v.Findings = []ReviewFinding{}
	}
	// The registry redacts every submission before it leaves the tool layer.
	return Result{Content: fmt.Sprintf("Review verdict %s accepted with %d findings.", v.Verdict, len(v.Findings)), Submission: &Submission{Review: &v}}, nil
}
