package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/store"
)

func (r *Runner) operation(ctx context.Context, run store.Run, kind, name string, work func() error) error {
	start := r.now()
	err := work()
	finish := r.now()
	duration := max(finish.Sub(start).Milliseconds(), 0)
	status, output := "ok", ""
	if err != nil {
		status, output = "error", r.redactor.Redact(err.Error())
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	recorder := agent.StoreRecorder{Store: r.db, RunID: run.ID, Phase: run.Phase, Redactor: r.redactor}
	recordErr := recorder.Record(recordCtx, store.Step{Kind: kind, Tool: name, Status: status, OutputText: output, StartedAt: start, FinishedAt: &finish, DurationMS: &duration})
	return errors.Join(err, recordErr)
}

func (r *Runner) comment(ctx context.Context, run store.Run, kind, body string) (github.Comment, error) {
	if run.DryRun {
		return github.Comment{}, errors.New("dry run cannot publish comments")
	}
	var comment github.Comment
	err := r.operation(ctx, run, "publish", "issue_comment", func() error {
		var err error
		comment, err = r.postComment(ctx, run, kind, body)
		return err
	})
	return comment, err
}

// postComment is the only path that publishes a GitHub comment. Every body
// carries a hidden patchbot marker, and the returned comment ID is recorded
// right after a successful post. With a shared identity (token owner is also
// an approver) comment sync refuses both, so patchbot's own text, including a
// model-written spec that begins with /approve, can never approve a run.
func (r *Runner) postComment(ctx context.Context, run store.Run, kind, body string) (github.Comment, error) {
	body = r.redactor.Redact(body)
	if !github.HasPatchbotMarker(body) {
		body = fmt.Sprintf("%s\n%s", body, github.Marker(fmt.Sprintf("%s run=%d", kind, run.ID)))
	}
	comment, err := r.gh.CreateIssueComment(ctx, ref(run.Repo), int(run.IssueNumber), body)
	if err != nil {
		return comment, err
	}
	// Record even if the caller's context ends right after the post.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.db.RecordPostedComment(recordCtx, store.PostedComment{CommentID: comment.ID, RunID: run.ID,
		Repo: run.Repo, IssueNumber: run.IssueNumber, Kind: kind, CreatedAt: r.now()}); err != nil {
		return comment, fmt.Errorf("record posted comment: %w", err)
	}
	return comment, nil
}
