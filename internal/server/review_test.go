package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

func TestAPIReviewPhaseAndArtifacts(t *testing.T) {
	approve := toolStep("submit_review", tools.ReviewVerdict{Verdict: tools.VerdictApprove, Findings: []tools.ReviewFinding{}, Criteria: []tools.ReviewCriterion{{ID: "AC1", Met: true}}})
	f := newFixture(t, fake.New("anthropic", specStep(), writeStep(), submitStep(), approve), nil)
	f.withRepo()
	f.cfg.Review.Enabled = true
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"fix"}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	f.waitFor(run.ID, store.StatusAwaitingApproval)
	path := fmt.Sprintf("/api/runs/%d", run.ID)
	requireStatus(t, f.request("POST", path+"/approve", f.approveBody(t, run.ID)), 202)
	done := f.waitFor(run.ID, store.StatusSucceeded)
	if done.Outcome != "pr_opened" {
		t.Fatalf("run: %+v", done)
	}
	w = f.request("GET", path, "")
	requireStatus(t, w, 200)
	var detail struct {
		Run       store.Run                `json:"run"`
		Reviews   []store.Review           `json:"reviews"`
		Steps     []store.Step             `json:"steps"`
		Artifacts []store.ArtifactMetadata `json:"artifacts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Reviews) != 1 || detail.Reviews[0].Verdict != "approve" || !strings.Contains(string(detail.Run.ReviewJSON), `"enabled":true`) {
		t.Fatalf("detail reviews: %s", w.Body.String())
	}
	reviewSteps, reviewArtifact := 0, false
	for _, step := range detail.Steps {
		if step.Phase == store.PhaseReview {
			reviewSteps++
		}
	}
	for _, a := range detail.Artifacts {
		reviewArtifact = reviewArtifact || (a.Kind == "review" && a.Attempt == 1)
	}
	if reviewSteps == 0 || !reviewArtifact {
		t.Fatalf("review steps %d artifact %t", reviewSteps, reviewArtifact)
	}
	w = f.request("GET", path+"/artifacts/review?attempt=1", "")
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), "Review round 1: approve") || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("review artifact: %s", w.Body.String())
	}
}
