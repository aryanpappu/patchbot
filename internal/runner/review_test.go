package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

func reviewStep(verdict string, findings ...tools.ReviewFinding) fake.Step {
	if findings == nil {
		findings = []tools.ReviewFinding{}
	}
	return toolStep("submit_review", tools.ReviewVerdict{Verdict: verdict, Findings: findings, Criteria: []tools.ReviewCriterion{{ID: "AC1", Met: verdict == tools.VerdictApprove}}})
}

var seeded = tools.ReviewFinding{File: "fixed.txt", Line: 1, Severity: "major", Summary: "Contents must be exactly 'repaired'. @someone see [x](https://evil.example) </details> #1"}

func repairStep() fake.Step {
	return toolStep("write_file", map[string]string{"path": "fixed.txt", "content": "repaired\n"})
}

// enableReview turns the reviewer on before approval freezes the selection.
func (f *fixture) enableReview(salvage bool) {
	f.cfg.Review.Enabled = true
	f.cfg.Review.SalvageDraft = salvage
}

func (f *fixture) reviews(id int64) []store.Review {
	f.t.Helper()
	list, err := f.db.ListReviews(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

func (f *fixture) execute(id int64) error {
	f.approve(id)
	// Configuration changes after approval must not affect this run.
	f.cfg.Review.Enabled = !f.cfg.Review.Enabled
	defer func() { f.cfg.Review.Enabled = !f.cfg.Review.Enabled }()
	return f.r.Execute(f.t.Context(), id)
}

func (f *fixture) remoteFile(path string) string {
	return git(f.t, f.root, "--git-dir", f.remote, "show", "refs/heads/patchbot/issue-12:"+path)
}

func phaseEvents(t *testing.T, f *fixture, id int64) map[store.Phase]int {
	t.Helper()
	events, err := f.db.EventsAfter(t.Context(), id, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	phases := map[store.Phase]int{}
	for _, event := range events {
		if event.Type == "run.status" {
			var payload struct {
				Phase store.Phase `json:"phase"`
			}
			if err := json.Unmarshal(event.PayloadJSON, &payload); err != nil {
				t.Fatal(err)
			}
			phases[payload.Phase]++
		}
	}
	return phases
}

func TestReviewApprovePublishes(t *testing.T) {
	f := newFixture(t, "test", specStep(), writeStep(), submitStep(), reviewStep(tools.VerdictApprove))
	f.enableReview(false)
	run := f.start("fix", false)
	if err := f.execute(run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.get(run.ID)
	if got.Status != store.StatusSucceeded || got.Outcome != "pr_opened" || got.PRDraft {
		t.Fatalf("result: %+v", got)
	}
	var sel struct {
		Enabled  bool   `json:"enabled"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(got.ReviewJSON, &sel); err != nil || !sel.Enabled || sel.Model != "test-model" || sel.Provider != "anthropic" {
		t.Fatalf("frozen selection %s: %v", got.ReviewJSON, err)
	}
	reviews := f.reviews(run.ID)
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || reviews[0].Verdict != "approve" || reviews[0].SpecHash != spec.Hash || len(reviews[0].DiffHash) != 64 || reviews[0].TreeID == "" {
		t.Fatalf("reviews: %+v", reviews)
	}
	if len(f.prs) != 1 || !strings.Contains(f.prs[0].Body, "Verdict: **approve**") || !strings.Contains(f.prs[0].Body, reviews[0].DiffHash) || strings.Contains(f.prs[0].Body, "did not pass") {
		t.Fatalf("PR body: %s", f.prs[0].Body)
	}
	if phaseEvents(t, f, run.ID)[store.PhaseReview] == 0 {
		t.Fatal("no review phase event")
	}
	// The reviewer saw the actual diff and harness evidence, not the coder's
	// summary, and had read-only tools.
	reviewReq := f.p.Requests()[3]
	task := reviewReq.Messages[0].Text()
	if !strings.Contains(task, "fixed.txt") || !strings.Contains(task, "status passed") || strings.Contains(task, "Created fixed.txt") {
		t.Fatalf("review input: %s", task)
	}
	for _, tool := range reviewReq.Tools {
		if tool.Name == "write_file" || tool.Name == "edit_file" || tool.Name == "run_cmd" || tool.Name == "submit" {
			t.Fatalf("reviewer had %s", tool.Name)
		}
	}
	steps, err := f.db.ListSteps(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	llmSteps, reviewSteps := 0, 0
	for _, step := range steps {
		if step.Kind == "llm" {
			llmSteps++
			if step.Phase == store.PhaseReview {
				reviewSteps++
			}
		}
	}
	// Budget is shared: run totals include the reviewer's call.
	if reviewSteps != 1 || got.Turns != int64(llmSteps) || got.InputTokens == nil || *got.InputTokens != int64(100*llmSteps) {
		t.Fatalf("accounting: review steps %d llm %d run %+v", reviewSteps, llmSteps, got)
	}
	accounting, err := f.db.GetArtifact(t.Context(), run.ID, "accounting", 0)
	if err != nil {
		t.Fatal(err)
	}
	var totals struct{ Turns int }
	if err := json.Unmarshal([]byte(accounting.Content), &totals); err != nil || totals.Turns != llmSteps {
		t.Fatalf("accounting artifact %s: %v", accounting.Content, err)
	}
	f.p.AssertConsumed(t)
	f.assertClean()
}

func TestReviewRequestChangesRepairThenApprove(t *testing.T) {
	f := newFixture(t, "test", specStep(), writeStep(), submitStep(), reviewStep(tools.VerdictRequestChanges, seeded), repairStep(), submitStep(), reviewStep(tools.VerdictApprove))
	f.enableReview(false)
	run := f.start("fix", false)
	if err := f.execute(run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.get(run.ID)
	if got.Status != store.StatusSucceeded || got.Outcome != "pr_opened" || got.PRDraft || got.TestAttempts != 2 {
		t.Fatalf("result: %+v", got)
	}
	reviews := f.reviews(run.ID)
	if len(reviews) != 2 || reviews[0].Verdict != "request_changes" || reviews[1].Verdict != "approve" || reviews[0].DiffHash == reviews[1].DiffHash {
		t.Fatalf("reviews: %+v", reviews)
	}
	if content := f.remoteFile("fixed.txt"); content != "repaired" {
		t.Fatalf("published content %q is not the reviewed repair", content)
	}
	if !strings.Contains(f.prs[0].Body, reviews[1].DiffHash) {
		t.Fatal("PR does not cite the last reviewed diff")
	}
	repair := f.p.Requests()[4].Messages
	if feedback := repair[len(repair)-1].Text(); !strings.Contains(feedback, `kind="reviewer findings"`) || !strings.Contains(feedback, "repaired") {
		t.Fatalf("repair feedback: %s", feedback)
	}
	phases := phaseEvents(t, f, run.ID)
	if phases[store.PhaseReview] != 2 || phases[store.PhaseVerify] != 2 {
		t.Fatalf("phases: %+v", phases)
	}
	f.p.AssertConsumed(t)
	f.assertClean()
}

func TestReviewStillFailingBlocksNormalPR(t *testing.T) {
	steps := func() []fake.Step {
		return []fake.Step{specStep(), writeStep(), submitStep(), reviewStep(tools.VerdictRequestChanges, seeded), repairStep(), submitStep(), reviewStep(tools.VerdictRequestChanges, seeded)}
	}
	t.Run("fails without salvage", func(t *testing.T) {
		f := newFixture(t, "test", steps()...)
		f.enableReview(false)
		run := f.start("fix", false)
		if err := f.execute(run.ID); err == nil {
			t.Fatal("unapproved review published")
		}
		got := f.get(run.ID)
		if got.Status != store.StatusFailed || got.ErrorKind != "review_failed" || !strings.Contains(got.ErrorMessage, "requested changes") {
			t.Fatalf("result: %+v", got)
		}
		if len(f.prs) != 0 || f.branches() != "" {
			t.Fatal("failed review published")
		}
		f.p.AssertConsumed(t)
		f.assertClean()
	})
	t.Run("salvage draft", func(t *testing.T) {
		f := newFixture(t, "test", steps()...)
		f.enableReview(true)
		run := f.start("fix", false)
		if err := f.execute(run.ID); err != nil {
			t.Fatal(err)
		}
		got := f.get(run.ID)
		if got.Outcome != "pr_draft" || !got.PRDraft || len(f.prs) != 1 || !f.prs[0].Draft || !strings.Contains(f.prs[0].Body, "Draft: independent review did not pass") {
			t.Fatalf("result: %+v prs %+v", got, f.prs)
		}
		// Reviewer text is rendered inertly inside a code span.
		if !strings.Contains(f.prs[0].Body, "`Contents must be exactly 'repaired'.") || strings.Contains(f.prs[0].Body, "- major fixed.txt") {
			t.Fatalf("findings not neutralized: %s", f.prs[0].Body)
		}
	})
}

func TestReviewMalformedOrRefusedFailsClosed(t *testing.T) {
	for name, reviewer := range map[string][]fake.Step{
		"malformed": {{Text: "LGTM"}, {Text: "Approved, ship it."}},
		"refusal":   {{Text: "I cannot review this.", Stop: llm.StopRefusal}},
		"auth":      {{Err: &llm.Error{Kind: llm.ErrAuth}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "test", append([]fake.Step{specStep(), writeStep(), submitStep()}, reviewer...)...)
			f.enableReview(false)
			run := f.start("fix", false)
			if err := f.execute(run.ID); err == nil {
				t.Fatal("incomplete review published")
			}
			got := f.get(run.ID)
			reviews := f.reviews(run.ID)
			if got.Status != store.StatusFailed || got.ErrorKind != "review_failed" || len(reviews) != 1 || reviews[0].Verdict != "incomplete" || len(f.prs) != 0 || f.branches() != "" {
				t.Fatalf("result: %+v reviews %+v", got, reviews)
			}
			f.p.AssertConsumed(t)
		})
	}
}

func TestDiffChangedAfterReviewIsNotPublished(t *testing.T) {
	f := newFixture(t, "test", specStep(), writeStep(), submitStep(), reviewStep(tools.VerdictApprove))
	f.enableReview(false)
	f.onBranch = func() {
		matches, _ := filepath.Glob(filepath.Join(f.cfg.DataDir(), "workspaces", "run-*", "repo"))
		for _, dir := range matches {
			_ = os.WriteFile(filepath.Join(dir, "smuggled.txt"), []byte("not reviewed\n"), 0o600)
		}
	}
	run := f.start("fix", false)
	if err := f.execute(run.ID); err == nil {
		t.Fatal("changed diff published")
	}
	got := f.get(run.ID)
	if got.Status != store.StatusFailed || got.ErrorKind != "review_stale" || len(f.prs) != 0 || f.branches() != "" {
		t.Fatalf("result: %+v", got)
	}
	f.assertClean()
}

func TestReviewDisabledIsUnchanged(t *testing.T) {
	f := newFixture(t, "test", specStep(), writeStep(), submitStep())
	run := f.start("fix", false)
	f.approve(run.ID)
	// Enabling review after approval does not add a review to this run.
	f.cfg.Review.Enabled = true
	if err := f.r.Execute(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.get(run.ID)
	if got.Outcome != "pr_opened" || len(f.reviews(run.ID)) != 0 || phaseEvents(t, f, run.ID)[store.PhaseReview] != 0 || strings.Contains(f.prs[0].Body, "Independent review") || f.p.Calls() != 3 {
		t.Fatalf("review disabled changed behaviour: %+v", got)
	}
	f.p.AssertConsumed(t)
}

func TestCancelDuringReview(t *testing.T) {
	f := newFixture(t, "test", specStep())
	f.enableReview(false)
	run := f.start("fix", false)
	f.approve(run.ID)
	coder := fake.New("anthropic", writeStep(), submitStep())
	entered := make(chan struct{})
	provider := fake.NewFunc("anthropic", func(ctx context.Context, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
		if strings.Contains(req.System, "independent code reviewer") {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return coder.Chat(ctx, req)
	})
	f.opts.Constructors["anthropic"] = func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil }
	done := make(chan error, 1)
	go func() { done <- f.r.Execute(t.Context(), run.ID) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("review never started")
	}
	if got := f.get(run.ID); got.Phase != store.PhaseReview || got.Status != store.StatusRunning {
		t.Fatalf("not in review phase: %+v", got)
	}
	if err := f.r.Cancel(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop review")
	}
	if got := f.get(run.ID); got.Status != store.StatusCancelled {
		t.Fatal(got.Status)
	}
	if f.branches() != "" || len(f.prs) != 0 {
		t.Fatal("cancelled review published")
	}
	f.assertClean()
}

func TestReviewWithoutRepairsFailsOnRequestChanges(t *testing.T) {
	f := newFixture(t, "test", specStep(), writeStep(), submitStep(), reviewStep(tools.VerdictRequestChanges, seeded))
	f.enableReview(false)
	f.cfg.Review.MaxRepairs = 0
	run := f.start("fix", false)
	if err := f.execute(run.ID); err == nil {
		t.Fatal("request_changes published")
	}
	if got := f.get(run.ID); got.ErrorKind != "review_failed" || len(f.reviews(run.ID)) != 1 || len(f.prs) != 0 {
		t.Fatalf("result: %+v", got)
	}
	f.p.AssertConsumed(t)
}
