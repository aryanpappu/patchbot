package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/store"
)

// SyncWaiting performs one comment/expiry pass, without a background poller.
// A command is eligible only for the current waiting window, so old comments
// cannot approve a later revision or a retry. Dry runs never consume commands.
func (r *Runner) SyncWaiting(ctx context.Context) error {
	runs, err := r.waitingRuns(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if r.expired(run) {
			if err := r.expire(ctx, run); err != nil && !errors.Is(err, store.ErrConflict) {
				errs = append(errs, err)
			}
			continue
		}
		if run.DryRun {
			continue
		}
		if err := r.syncRun(ctx, run); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) syncRun(ctx context.Context, run store.Run) error {
	bot := r.opts.BotLogin
	if bot == "" {
		identity, ok := r.gh.(interface {
			CurrentUser(context.Context) (string, error)
		})
		if !ok {
			return errors.New("GitHub bot identity unavailable; comment sync disabled")
		}
		var err error
		bot, err = identity.CurrentUser(ctx)
		if err != nil {
			return err
		}
	}
	spec, err := r.db.LatestSpec(ctx, run.ID)
	if err != nil {
		return err
	}
	since := spec.CreatedAt
	if run.WaitingSince != nil && run.WaitingSince.After(since) {
		since = *run.WaitingSince
	}
	comments, err := r.gh.ListIssueComments(ctx, ref(run.Repo), int(run.IssueNumber), since)
	if err != nil {
		return err
	}
	sort.Slice(comments, func(i, j int) bool {
		if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	for _, comment := range comments {
		if !comment.CreatedAt.After(since) || (spec.CommentID != nil && comment.ID <= *spec.CommentID) {
			continue
		}
		command, ok := github.ParseCommand(comment)
		if !ok || (command.Kind != github.CommandApprove && command.Kind != github.CommandRevise) {
			continue
		}
		var collaboratorErr error
		if !github.EligibleCommand(comment, r.cfg.Defaults.Approvers, func(login string) bool {
			member, err := r.gh.IsCollaborator(ctx, ref(run.Repo), login)
			collaboratorErr = err
			return err == nil && member
		}, bot) {
			if collaboratorErr != nil {
				return collaboratorErr
			}
			continue
		}
		current, err := r.db.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		if !waiting(current.Status) || current.WaitingSince == nil || !sameTime(current.WaitingSince, run.WaitingSince) {
			return nil
		}
		switch command.Kind {
		case github.CommandApprove:
			if current.Status != store.StatusAwaitingApproval {
				continue
			}
			if err := r.approve(ctx, run.ID, comment.Author, "github", &spec); err != nil {
				if errors.Is(err, errCommentEdited) {
					return errors.Join(err, r.commentEditAdvice(ctx, run, spec))
				}
				return err
			}
			return r.Execute(ctx, run.ID)
		case github.CommandRevise:
			return r.revise(ctx, run.ID, command.Notes, comment.Author, "github", &spec)
		default:
			continue
		}
	}
	return nil
}

func (r *Runner) commentEditAdvice(ctx context.Context, run store.Run, spec store.Spec) error {
	kind := fmt.Sprintf("spec_comment_warning_v%d", spec.Version)
	if _, err := r.db.GetArtifact(ctx, run.ID, kind, 0); !errors.Is(err, store.ErrNotFound) {
		return err
	}
	message := "The specification comment was edited on GitHub. Those edits are not imported. Use `/revise <notes>` or edit the specification in patchbot before approving a new version."
	if _, err := r.comment(ctx, run, message); err != nil {
		return err
	}
	return r.artifact(ctx, run.ID, kind, message)
}

func sameTime(a, b *time.Time) bool { return a != nil && b != nil && a.Equal(*b) }
