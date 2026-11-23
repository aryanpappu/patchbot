package runner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestAnswerQuestionsRevisesWithAnswers(t *testing.T) {
	f := newFixture(t, "true", specStep("Which file name?", "What contents?"), specStep())
	run := f.start("fix", false)
	if run.Status != store.StatusNeedsInfo {
		t.Fatal(run.Status)
	}
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !f.r.ActionCapabilities(run, &spec).CanAnswer {
		t.Fatal("needs_info run cannot answer")
	}
	ctx := t.Context()
	for name, tc := range map[string]struct {
		version int64
		hash    string
		answers []string
		want    error
	}{
		"stale version": {spec.Version + 1, spec.Hash, []string{"a", "b"}, store.ErrConflict},
		"wrong hash":    {spec.Version, "x", []string{"a", "b"}, store.ErrConflict},
		"too few":       {spec.Version, spec.Hash, []string{"a"}, runner.ErrInvalidAnswers},
		"blank":         {spec.Version, spec.Hash, []string{"a", "  "}, runner.ErrInvalidAnswers},
		"too long":      {spec.Version, spec.Hash, []string{"a", strings.Repeat("x", 4001)}, runner.ErrInvalidAnswers},
	} {
		if err := f.r.AnswerQuestions(ctx, run.ID, "owner", tc.version, tc.hash, tc.answers); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := f.r.AnswerQuestions(ctx, run.ID, "owner", spec.Version, spec.Hash, []string{"fixed.txt", " fixed "}); err != nil {
		t.Fatal(err)
	}
	next, err := f.db.LatestSpec(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != 2 || !strings.Contains(next.Notes, "Question 2: What contents?\nAnswer 2: fixed\n") || f.get(run.ID).Status != store.StatusAwaitingApproval {
		t.Fatalf("revised spec: %+v", next)
	}
	questions, err := f.db.ListQuestions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 2 || questions[0].Answer == nil || *questions[0].Answer != "fixed.txt" || *questions[1].AnsweredBy != "owner" {
		t.Fatalf("questions: %+v", questions)
	}
	// Answering is only for needs_info.
	if err := f.r.AnswerQuestions(ctx, run.ID, "owner", next.Version, next.Hash, []string{"a"}); !errors.Is(err, store.ErrConflict) {
		t.Fatal(err)
	}
	f.assertClean()
}
