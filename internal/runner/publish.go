package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

func (r *Runner) publicationAvailable(ctx context.Context, run store.Run) error {
	branch := fmt.Sprintf("patchbot/issue-%d", run.IssueNumber)
	pr, err := r.gh.FindOpenPR(ctx, ref(run.Repo), branch)
	if err != nil {
		return err
	}
	if pr != nil {
		return errors.New("an open PR already exists for the patchbot branch")
	}
	exists, err := r.gh.BranchExists(ctx, ref(run.Repo), branch)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("patchbot branch already exists; refusing to overwrite it")
	}
	return nil
}

func (r *Runner) validateChanges(ctx context.Context, run store.Run, env *toolEnvironment) error {
	changes, err := env.repo.Status(ctx)
	if err != nil {
		return err
	}
	policy, err := workspace.NewPolicy(r.cfg.Repos[run.Repo].Protected)
	if err != nil {
		return err
	}
	for _, change := range changes {
		for _, path := range []string{change.Path, change.OriginalPath} {
			if path == "" {
				continue
			}
			if _, err := policy.Check(path); err != nil {
				return &agent.Error{Kind: "policy_violations", Message: "patch changes a protected or invalid path"}
			}
			if _, err := env.env.WS.Stat(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return &agent.Error{Kind: "policy_violations", Message: "patch contains an unsafe path"}
			}
		}
	}
	return nil
}

// publicationApproval checks remote spec freshness first, then the persisted
// decision and worker generation. Call again at each external write boundary:
// neither an earlier snapshot nor a heartbeat is publication authorization.
func (r *Runner) publicationApproval(ctx context.Context, run store.Run, spec store.Spec) (store.Approval, error) {
	current, err := r.checkedSpec(ctx, run)
	if err != nil {
		return store.Approval{}, err
	}
	if current.Version != spec.Version || current.Hash != spec.Hash {
		return store.Approval{}, agent.ErrApprovalDenied
	}
	approval, err := r.db.ValidApproval(ctx, run.ID, spec.Version, spec.Hash)
	if err != nil {
		return store.Approval{}, fmt.Errorf("%w: matching publication approval: %w", agent.ErrApprovalDenied, err)
	}
	persisted, err := r.db.GetRun(ctx, run.ID)
	if err != nil {
		return store.Approval{}, err
	}
	if persisted.Status != store.StatusRunning || persisted.Phase != store.PhasePublish || persisted.Generation != run.Generation {
		return store.Approval{}, store.ErrConflict
	}
	now := r.now()
	if approval.CreatedAt.IsZero() || approval.CreatedAt.After(now) || !now.Before(approval.CreatedAt.Add(r.gateOptions().TTL)) {
		return store.Approval{}, fmt.Errorf("%w: publication approval expired or has an invalid date", agent.ErrApprovalDenied)
	}
	return approval, ctx.Err()
}

func (r *Runner) publish(ctx context.Context, run *store.Run, env *toolEnvironment, meta github.Repo, spec store.Spec, result agent.CodeResult, baseline string, codeErr error) error {
	var summary *string
	if result.Submission != nil {
		summary = &result.Submission.Summary
	}
	if err := r.transition(ctx, run, store.StatusRunning, store.PhasePublish, store.RunPatch{TestsStatus: &result.TestsStatus, PRDraft: &result.Draft, Summary: summary}); err != nil {
		return err
	}
	approval, err := r.publicationApproval(ctx, *run, spec)
	if err != nil {
		return err
	}
	if err := r.validateChanges(ctx, *run, env); err != nil {
		return err
	}
	diff, err := env.repo.Diff(ctx)
	if err != nil {
		return err
	}
	if err := r.artifact(ctx, run.ID, "diff", diff); err != nil {
		return err
	}
	live, err := r.gh.GetRepo(ctx, ref(run.Repo))
	if err != nil {
		return err
	}
	branch, found, err := r.gh.GetBranch(ctx, ref(run.Repo), live.DefaultBranch)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("default branch no longer exists")
	}
	moved := live.DefaultBranch != meta.DefaultBranch || branch.Commit.SHA != run.BaseSHA
	title, body := r.prText(*run, spec, approval, result, baseline, moved, codeErr)
	if err := r.artifact(ctx, run.ID, "pr_title", title); err != nil {
		return err
	}
	if err := r.artifact(ctx, run.ID, "pr_body", body); err != nil {
		return err
	}
	if !run.DryRun {
		if err := r.publicationAvailable(ctx, *run); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var head string
	if err := r.operation(ctx, *run, "git", "commit", func() error {
		var err error
		head, err = env.repo.CommitAll(ctx, fmt.Sprintf("fix: %s (#%d)", title, run.IssueNumber), gitx.Author{})
		return err
	}); err != nil {
		return err
	}
	if err := r.transition(ctx, run, store.StatusRunning, store.PhasePublish, store.RunPatch{HeadSHA: &head}); err != nil {
		return err
	}
	if run.DryRun {
		outcome := "dry_run"
		return r.transition(ctx, run, store.StatusSucceeded, store.PhasePublish, store.RunPatch{Outcome: &outcome})
	}
	var guardErr error
	if err := r.operation(ctx, *run, "git", "push", func() error {
		if _, guardErr = r.publicationApproval(ctx, *run, spec); guardErr != nil {
			return guardErr
		}
		return env.repo.Push(ctx, run.Branch)
	}); err != nil {
		if guardErr != nil {
			return err
		}
		return &agent.Error{Kind: "push_failed", Message: "could not push guarded patchbot branch"}
	}
	var pr github.PullRequest
	err = r.operation(ctx, *run, "publish", "create_pr", func() error {
		if _, guardErr = r.publicationApproval(ctx, *run, spec); guardErr != nil {
			return guardErr
		}
		var err error
		pr, err = r.gh.CreatePR(ctx, ref(run.Repo), github.NewPR{Title: title, Body: body, Head: run.Branch, Base: live.DefaultBranch, Draft: result.Draft})
		return err
	})
	if err != nil {
		if guardErr != nil {
			return err
		}
		return &agent.Error{Kind: "pr_failed", Message: "branch pushed but pull request creation failed"}
	}
	number, outcome := int64(pr.Number), "pr_opened"
	if result.Draft {
		outcome = "pr_draft"
	}
	if err := r.transition(ctx, run, store.StatusRunning, store.PhasePublish, store.RunPatch{PRNumber: &number, PRURL: &pr.HTMLURL, PRDraft: &result.Draft}); err != nil {
		return err
	}
	if _, err := r.comment(ctx, *run, fmt.Sprintf("patchbot run %d: %s\nHarness tests: %s (%d attempts).", run.ID, pr.HTMLURL, result.TestsStatus, result.TestAttempts)); err != nil {
		if err := r.artifact(ctx, run.ID, "publish_comment_warning", "PR opened, but the issue comment could not be posted."); err != nil {
			return err
		}
	}
	return r.transition(ctx, run, store.StatusSucceeded, store.PhasePublish, store.RunPatch{Outcome: &outcome})
}

func (r *Runner) prText(run store.Run, spec store.Spec, approval store.Approval, result agent.CodeResult, baseline string, moved bool, codeErr error) (string, string) {
	title := fmt.Sprintf("Fix issue #%d", run.IssueNumber)
	var b strings.Builder
	if result.Salvage {
		fmt.Fprintf(&b, "Draft salvage: coding stopped (%s). Review the incomplete work.\n\n", result.Reason)
	}
	if codeErr != nil {
		b.WriteString("The agent stopped with a provider error; harness verification follows below.\n\n")
	}
	fmt.Fprintf(&b, "Fixes #%d\n\n", run.IssueNumber)
	link := fmt.Sprintf("stored run %d specification", run.ID)
	if spec.CommentID != nil {
		link = fmt.Sprintf("[approved specification](https://github.com/%s/issues/%d#issuecomment-%d)", run.Repo, run.IssueNumber, *spec.CommentID)
	}
	fmt.Fprintf(&b, "Approved spec: %s, version %d, SHA-256 `%s`, approved by %s.\n\n", link, spec.Version, spec.Hash, approval.Actor)
	fmt.Fprintf(&b, "Harness tests: **%s** (%d attempts). Baseline: **%s**.\n\nAcceptance criteria (agent claims; harness result above):\n\n", result.TestsStatus, result.TestAttempts, baseline)
	if result.TestsStatus == "not_run" {
		b.WriteString("Unverified: no test command is configured. This PR requires manual verification.\n\n")
	}
	if result.Submission != nil {
		title = result.Submission.PRTitle
		for index, criterion := range result.Submission.Criteria {
			tick := " "
			if criterion.Met {
				tick = "x"
			}
			fmt.Fprintf(&b, "- [%s] AC%d: %s — harness: %s. %s\n", tick, index+1, criterion.Text, result.TestsStatus, criterion.Note)
		}
		fmt.Fprintf(&b, "\n%s\n\n%s\n", result.Submission.Summary, result.Submission.PRBody)
	}
	if result.LastTest != nil {
		fmt.Fprintf(&b, "\nHarness exit code: %d; timed out: %t.\n\n```text\n%s\n```\n", result.LastTest.ExitCode, result.LastTest.TimedOut, tools.CapOutput(result.LastTest.Output, 8<<10))
	}
	if moved {
		fmt.Fprintf(&b, "\nThe default branch has moved since refinement. This patch is based on `%s`; review for integration changes.\n", run.BaseSHA)
	}
	cost := "unknown"
	if run.CostMicros != nil {
		cost = fmt.Sprintf("%d micro-USD", *run.CostMicros)
	}
	tokens := "unknown"
	if run.InputTokens != nil && run.OutputTokens != nil {
		tokens = fmt.Sprintf("%d input / %d output", *run.InputTokens, *run.OutputTokens)
	}
	fmt.Fprintf(&b, "\nUsage: %s tokens; cost: %s.\n", tokens, cost)
	fmt.Fprintf(&b, "\nProvider: %s; model: %s.\n\nGenerated by patchbot. Human review is required.\n", run.Provider, run.CodeModel)
	title = strings.ReplaceAll(strings.ReplaceAll(title, "\n", " "), "\r", " ")
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("Fix issue #%d", run.IssueNumber)
	}
	return tools.CapOutput(github.NeutralizeMentions(r.redactor.Redact(title)), 200), tools.CapOutput(github.NeutralizeMentions(r.redactor.Redact(b.String())), 60_000)
}
