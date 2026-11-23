package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// maxAnswerRunes bounds one answer; the combined notes still go through the
// ordinary revision path and its redaction.
const maxAnswerRunes = 4000

// ErrInvalidAnswers is returned when answers do not match the spec's questions.
var ErrInvalidAnswers = errors.New("answers must match the specification's open questions")

// OpenQuestions returns the latest spec's unresolved questions.
func OpenQuestions(spec store.Spec) []string {
	draft, err := tools.ParseSpec(spec.BodyMD)
	if err != nil {
		return nil
	}
	return draft.OpenQuestions
}

// AnswerQuestions records one answer per open question of exactly the spec
// version the person saw, then reruns refinement with the questions and
// answers as revision notes. It is a revise, so it is bound by the same state,
// expiry and revision-limit rules.
func (r *Runner) AnswerQuestions(ctx context.Context, id int64, actor string, version int64, hash string, answers []string) error {
	run, err := r.db.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if run.Status != store.StatusNeedsInfo {
		return store.ErrConflict
	}
	// Check expiry and the revision limit before recording anything.
	if err := r.requireActionState(ctx, run, "revise"); err != nil {
		return err
	}
	spec, err := r.db.LatestSpec(ctx, id)
	if err != nil {
		return err
	}
	if spec.Version != version || spec.Hash != hash {
		return store.ErrConflict
	}
	if err := r.checkActionSpec(spec, "revise"); err != nil {
		return err
	}
	questions := OpenQuestions(spec)
	if len(questions) == 0 || len(answers) != len(questions) {
		return ErrInvalidAnswers
	}
	for i, answer := range answers {
		answers[i] = strings.TrimSpace(answer)
		if answers[i] == "" || utf8.RuneCountInString(answers[i]) > maxAnswerRunes {
			return fmt.Errorf("%w: answer %d must be 1 to %d characters", ErrInvalidAnswers, i+1, maxAnswerRunes)
		}
	}
	if strings.TrimSpace(actor) == "" {
		actor = r.actorFor(ctx)
	}
	// Rows saved with this spec, read before the revise saves the next spec's.
	saved, err := r.db.ListQuestions(ctx, id)
	if err != nil {
		return err
	}
	var notes strings.Builder
	notes.WriteString("The requester answered the open questions. Use these answers and do not ask them again.\n")
	for i, q := range questions {
		fmt.Fprintf(&notes, "\nQuestion %d: %s\nAnswer %d: %s\n", i+1, q, i+1, answers[i])
	}
	// Answers are recorded only once the revise was accepted, so a refused or
	// racing submit leaves no answered rows behind.
	if err := r.revise(ctx, id, notes.String(), actor, r.actionChannel(), &spec); err != nil {
		return err
	}
	return r.recordAnswers(context.WithoutCancel(ctx), id, actor, saved, questions, answers)
}

// recordAnswers stores answers on the questions saved with the spec, newest
// unanswered match first, and inserts a row for any question not found.
func (r *Runner) recordAnswers(ctx context.Context, id int64, actor string, saved []store.Question, questions, answers []string) error {
	used := map[int64]bool{}
	now := r.now()
	channel := r.actionChannel()
	actor = r.redactor.Redact(actor)
	for i, body := range questions {
		answer := r.redactor.Redact(answers[i])
		q := store.Question{RunID: id, Body: body, CreatedAt: now}
		for j := len(saved) - 1; j >= 0; j-- {
			if s := saved[j]; s.Body == body && s.AnsweredAt == nil && !used[s.ID] {
				q = s
				break
			}
		}
		used[q.ID] = true
		q.Answer, q.AnsweredBy, q.AnswerChannel, q.AnsweredAt = &answer, &actor, &channel, &now
		if _, err := r.db.SaveQuestion(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
