package runner

import (
	"context"
	"errors"
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

func (r *Runner) comment(ctx context.Context, run store.Run, body string) (github.Comment, error) {
	if run.DryRun {
		return github.Comment{}, errors.New("dry run cannot publish comments")
	}
	var comment github.Comment
	err := r.operation(ctx, run, "publish", "issue_comment", func() error {
		var err error
		comment, err = r.gh.CreateIssueComment(ctx, ref(run.Repo), int(run.IssueNumber), r.redactor.Redact(body))
		return err
	})
	return comment, err
}
