package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

type toolEnvironment struct {
	repo *gitx.Repo
	env  *tools.Env
	dir  string
}

func (e *toolEnvironment) close() error {
	if e == nil || e.dir == "" {
		return nil
	}
	dir := e.dir
	e.dir = ""
	err := errors.Join(e.env.WS.Close(false), e.repo.Close())
	root, openErr := os.OpenRoot(filepath.Dir(dir))
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	defer func() { _ = root.Close() }()
	return errors.Join(err, root.RemoveAll(filepath.Base(dir)))
}

func (r *Runner) clone(ctx context.Context, run store.Run, meta github.Repo, sha string) (_ *toolEnvironment, err error) {
	base := filepath.Join(r.cfg.DataDir(), "workspaces")
	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, errors.New("create workspace directory failed")
	}
	dir, err := os.MkdirTemp(base, "run-"+strconv.FormatInt(run.ID, 10)+"-")
	if err != nil {
		return nil, errors.New("create run workspace failed")
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	token, err := r.opts.Tokens.Token(ctx)
	if err != nil {
		return nil, errors.New("GitHub token unavailable; set GITHUB_TOKEN")
	}
	var repo *gitx.Repo
	defer func() {
		if err != nil && repo != nil {
			_ = repo.Close()
		}
	}()
	err = r.operation(ctx, run, "git", "clone", func() error {
		var cloneErr error
		repo, cloneErr = r.opts.Clone(ctx, gitx.CloneOptions{Dir: filepath.Join(dir, "repo"), Host: "github.com", Owner: ref(run.Repo).Owner, Name: ref(run.Repo).Name, Branch: meta.DefaultBranch, DefaultBranch: meta.DefaultBranch, Token: token})
		return cloneErr
	})
	if err != nil {
		return nil, err
	}
	if sha != "" {
		if err := r.operation(ctx, run, "git", "checkout_sha", func() error { return repo.CheckoutSHA(ctx, sha) }); err != nil {
			return nil, err
		}
	}
	ws, err := workspace.Open(repo.Dir(), workspace.Options{Protected: r.cfg.Repos[run.Repo].Protected})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = ws.Close(false)
		}
	}()
	policy, err := execx.NewPolicy(ws.Dir(), r.cfg.Repos[run.Repo].Commands)
	if err != nil {
		return nil, err
	}
	executor, err := r.opts.Executor(ws.Dir())
	if err != nil {
		return nil, err
	}
	limits := r.cfg.Limits
	return &toolEnvironment{repo: repo, dir: dir, env: &tools.Env{WS: ws, Exec: executor, Policy: policy, RunDir: dir, Redactor: r.redactor, Limits: tools.Limits{CmdTimeout: limits.CmdTimeout, CmdOutputBytes: limits.CmdOutputBytes, CmdStoreBytes: limits.CmdStoreBytes}}}, nil
}

func (r *Runner) preflight(ctx context.Context, run store.Run) (github.Repo, github.Issue, error) {
	if r.cfg.Repos[run.Repo] == nil {
		return github.Repo{}, github.Issue{}, errors.New("repository is not in configuration allowlist")
	}
	meta, err := r.gh.GetRepo(ctx, ref(run.Repo))
	if err != nil {
		return meta, github.Issue{}, err
	}
	issue, err := r.gh.GetIssue(ctx, ref(run.Repo), int(run.IssueNumber))
	if err != nil {
		return meta, issue, err
	}
	if issue.State != "open" || issue.IsPullRequest {
		return meta, issue, errors.New("issue must be open and must not be a pull request")
	}
	if meta.DefaultBranch == "" {
		return meta, issue, errors.New("repository has no default branch")
	}
	if run.Mode == config.ModeFix && !meta.Permissions.Push {
		return meta, issue, errors.New("fix mode requires repository push permission")
	}
	return meta, issue, nil
}

func (r *Runner) refine(ctx context.Context, run *store.Run, prior *store.Spec, notes string, reuse *store.Spec) (err error) {
	session, limits, totals, err := r.session(ctx, *run)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, max(limits.Agent.Deadline.Sub(r.now()), 0))
	defer cancel()
	started := time.Now()
	defer func() { err = errors.Join(err, r.saveAccounting(ctx, *run, session, totals, started)) }()
	if totals.Active >= limits.Timeout || session.Tokens >= limits.Agent.MaxTotalTokens || session.Cost >= limits.Agent.MaxCostMicros {
		return budgetError(agent.EndBudget)
	}
	meta, issue, err := r.preflight(ctx, *run)
	if err != nil {
		return err
	}
	title := r.redactor.Redact(issue.Title)
	if err := r.transition(ctx, run, store.StatusRunning, store.PhasePreflight, store.RunPatch{IssueTitle: &title}); err != nil {
		return err
	}
	if reuse != nil {
		return r.reuseSpec(ctx, run, *reuse)
	}
	if err := r.transition(ctx, run, store.StatusRunning, store.PhaseClone, store.RunPatch{}); err != nil {
		return err
	}
	env, err := r.clone(ctx, *run, meta, "")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, env.close()) }()
	sha, err := env.repo.HeadSHA(ctx)
	if err != nil {
		return err
	}
	run.BaseSHA = sha
	if err := r.transition(ctx, run, store.StatusRunning, store.PhaseRefine, store.RunPatch{BaseSHA: &sha}); err != nil {
		return err
	}
	loop, err := r.loop(*run, env, run.RefineModel)
	if err != nil {
		return err
	}
	limits.Agent.MaxTurns = limits.RefineTurns
	if run.Mode == config.ModeInvestigate {
		report, err := loop.Investigate(ctx, agent.InvestigateRequest{Title: issue.Title, Body: issue.Body, Session: session, Limits: limits.Agent})
		if err != nil {
			return err
		}
		if report.Reason != agent.EndSubmitted || report.ReportMD == "" {
			return budgetError(report.Reason)
		}
		if err := r.artifact(ctx, run.ID, "report", report.ReportMD); err != nil {
			return err
		}
		if !run.DryRun {
			if _, err := r.comment(ctx, *run, report.ReportMD); err != nil {
				return err
			}
		}
		outcome := "investigated"
		if run.DryRun {
			outcome = "dry_run"
		}
		if err := env.close(); err != nil {
			return err
		}
		return r.transition(ctx, run, store.StatusSucceeded, store.PhaseRefine, store.RunPatch{Outcome: &outcome, Summary: &report.ReportMD})
	}
	result, err := loop.Refine(ctx, agent.RefineRequest{RunID: run.ID, Title: issue.Title, Body: issue.Body, Prior: prior, Notes: notes, Store: r.db, Session: session, Limits: limits.Agent})
	if err != nil {
		if result.Reason != "" && result.Reason != agent.EndSubmitted {
			return budgetError(result.Reason)
		}
		return err
	}
	if err := r.postSpec(ctx, *run, &result.Spec, result.Questions); err != nil {
		return err
	}
	if err := env.close(); err != nil {
		return err
	}
	return r.transition(ctx, run, result.Status, store.PhaseRefine, store.RunPatch{})
}

func (r *Runner) specComment(run store.Run, spec store.Spec) string {
	return github.NeutralizeMentions(fmt.Sprintf("%s\n\npatchbot run %d, specification v%d, SHA-256 `%s`.\nUse `/approve` to approve or `/revise <notes>` to request a new version. GitHub edits are not imported.\n<!-- patchbot:spec run=%d v=%d sha=%s -->", spec.BodyMD, run.ID, spec.Version, spec.Hash, run.ID, spec.Version, spec.Hash))
}

func (r *Runner) postSpec(ctx context.Context, run store.Run, spec *store.Spec, questions []string) error {
	if err := r.artifact(ctx, run.ID, "spec", spec.BodyMD); err != nil {
		return err
	}
	if !run.DryRun {
		comment, err := r.comment(ctx, run, r.specComment(run, *spec))
		if err != nil {
			return err
		}
		if err := r.db.SetSpecComment(ctx, run.ID, spec.Version, spec.Hash, comment.ID); err != nil {
			return err
		}
		spec.CommentID = &comment.ID
	}
	for _, body := range questions {
		if _, err := r.db.SaveQuestion(ctx, store.Question{RunID: run.ID, Body: body, CommentID: spec.CommentID}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) reuseSpec(ctx context.Context, run *store.Run, old store.Spec) error {
	oldRun, err := r.db.GetRun(ctx, old.RunID)
	if err != nil {
		return err
	}
	approval, err := r.db.ValidApproval(ctx, old.RunID, old.Version, old.Hash)
	if err != nil {
		return err
	}
	spec := old
	spec.RunID, spec.Version, spec.CommentID, spec.CreatedAt = run.ID, 1, nil, time.Time{}
	spec.Source, spec.Notes = "agent", fmt.Sprintf("Reused approved spec from run %d version %d", old.RunID, old.Version)
	spec, err = r.db.SaveSpec(ctx, spec)
	if err != nil {
		return err
	}
	if err := r.postSpec(ctx, *run, &spec, nil); err != nil {
		return err
	}
	run.BaseSHA = oldRun.BaseSHA
	if err := r.transition(ctx, run, store.StatusAwaitingApproval, store.PhaseRefine, store.RunPatch{BaseSHA: &run.BaseSHA}); err != nil {
		return err
	}
	approval.RunID, approval.SpecVersion, approval.SpecHash = run.ID, spec.Version, spec.Hash
	if err := r.db.DecideRun(ctx, run.Status, store.StatusApproved, approval, run.Generation); err != nil {
		return err
	}
	run.Status = store.StatusApproved
	run.Generation++
	return nil
}

// Execute runs the approved code phase synchronously. Refine mode completes
// approval without ever installing code tools or cloning for coding.
func (r *Runner) Execute(ctx context.Context, id int64) error {
	lock := r.control(id)
	lock.Lock()
	defer lock.Unlock()
	run, err := r.db.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if run.Status != store.StatusApproved {
		return store.ErrConflict
	}
	if run.Mode == config.ModeRefine {
		spec, err := r.checkedSpec(ctx, run)
		if err == nil {
			_, err = agent.Check(ctx, r.db, id, spec.IssueHash, r.gateOptions())
		}
		if err != nil {
			return errors.Join(err, r.transition(ctx, &run, store.StatusAwaitingApproval, run.Phase, store.RunPatch{}))
		}
		outcome := "spec_approved"
		return r.transition(ctx, &run, store.StatusSucceeded, run.Phase, store.RunPatch{Outcome: &outcome})
	}
	if run.Mode != config.ModeFix {
		return store.ErrConflict
	}
	err = r.withWork(ctx, id, func(work context.Context) error { return r.code(work, &run) })
	if errors.Is(err, store.ErrConflict) {
		return err
	}
	if errors.Is(err, agent.ErrApprovalDenied) {
		return errors.Join(err, r.transition(context.WithoutCancel(ctx), &run, store.StatusAwaitingApproval, run.Phase, store.RunPatch{}))
	}
	if err != nil {
		return r.fail(ctx, run, err)
	}
	return nil
}

func (r *Runner) code(ctx context.Context, run *store.Run) (err error) {
	session, limits, totals, err := r.session(ctx, *run)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, max(limits.Agent.Deadline.Sub(r.now()), 0))
	defer cancel()
	started := time.Now()
	defer func() { err = errors.Join(err, r.saveAccounting(ctx, *run, session, totals, started)) }()
	spec, err := r.checkedSpec(ctx, *run)
	if err != nil {
		return err
	}
	if _, err := agent.Check(ctx, r.db, run.ID, spec.IssueHash, r.gateOptions()); err != nil {
		return err
	}
	meta, issue, err := r.preflight(ctx, *run)
	if err != nil {
		return err
	}
	if err := r.publicationAvailable(ctx, *run); err != nil {
		return err
	}
	if err := r.transition(ctx, run, store.StatusApproved, store.PhaseClone, store.RunPatch{}); err != nil {
		return err
	}
	env, err := r.clone(ctx, *run, meta, run.BaseSHA)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, env.close()) }()
	branch := fmt.Sprintf("patchbot/issue-%d", run.IssueNumber)
	if err := r.operation(ctx, *run, "git", "create_branch", func() error { return env.repo.CreateBranch(ctx, branch) }); err != nil {
		return err
	}
	if err := r.transition(ctx, run, store.StatusApproved, store.PhaseBaseline, store.RunPatch{Branch: &branch}); err != nil {
		return err
	}
	baseline := "not_run"
	baselineInfo := baseline
	command, _ := r.cfg.Repos[run.Repo].TestCommandID()
	if command != "" && r.cfg.Limits.BaselineTests {
		test, err := r.baseline(ctx, *run, env, command, limits.Agent.TestTimeout)
		if err != nil {
			return err
		}
		baseline = "failed"
		baselineInfo = fmt.Sprintf("exit_code: %d; timed_out: %t\n%s", test.ExitCode, test.TimedOut, tools.CapOutput(test.Output, 8<<10))
		if test.ExitCode == 0 && !test.TimedOut {
			baseline = "passed"
		}
		changed, err := env.repo.HasChanges(ctx)
		if err != nil {
			return err
		}
		if changed {
			// Discard only this disposable baseline checkout. Re-clone the pinned
			// commit so generated baseline files cannot become agent changes.
			if err := env.close(); err != nil {
				return err
			}
			env, err = r.clone(ctx, *run, meta, run.BaseSHA)
			if err != nil {
				return err
			}
			if err := r.operation(ctx, *run, "git", "create_branch", func() error { return env.repo.CreateBranch(ctx, branch) }); err != nil {
				return err
			}
		}
	}
	// Fetch again after clone/baseline: waiting or repository execution must not
	// turn an earlier issue snapshot into permission to write.
	issue, err = r.gh.GetIssue(ctx, ref(run.Repo), int(run.IssueNumber))
	if err != nil {
		return err
	}
	token, err := agent.Check(ctx, r.db, run.ID, agent.IssueHash(issue.Title, issue.Body), r.gateOptions())
	if err != nil {
		return err
	}
	codeRun := *run
	codeRun.Phase = store.PhaseCode
	loop, err := r.loop(codeRun, env, run.CodeModel)
	if err != nil {
		return err
	}
	result, codeErr := loop.Code(ctx, token, agent.CodeRequest{RunID: run.ID, Title: issue.Title, Body: issue.Body, Changes: env.repo, Baseline: baselineInfo, TestCommand: command, Session: session, Limits: limits.Agent, OnPhase: func(c context.Context, phase store.Phase) error {
		if run.Status == store.StatusApproved {
			if err := r.db.StartCode(c, run.ID, spec.Version, spec.Hash, run.Generation); err != nil {
				return err
			}
			run.Status, run.Phase = store.StatusRunning, store.PhaseCode
			run.Generation++
			return nil
		}
		return r.transition(c, run, store.StatusRunning, phase, store.RunPatch{})
	}})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if codeErr != nil && !result.Salvage {
		return codeErr
	}
	if result.Reason == agent.EndRefusal {
		return budgetError(result.Reason)
	}
	if result.NoChanges {
		if result.Reason != agent.EndSubmitted {
			return budgetError(result.Reason)
		}
		explanation := "The agent finished without changes."
		if result.Submission != nil {
			explanation = result.Submission.Summary
		}
		if !run.DryRun {
			if _, err := r.comment(ctx, *run, explanation); err != nil {
				return err
			}
		}
		outcome := "no_changes"
		return r.transition(ctx, run, store.StatusSucceeded, run.Phase, store.RunPatch{Outcome: &outcome, Summary: &explanation})
	}
	if !result.CanPublish {
		return budgetError(result.Reason)
	}
	return r.publish(ctx, run, env, meta, spec, result, baseline, codeErr)
}

func (r *Runner) baseline(ctx context.Context, run store.Run, env *toolEnvironment, command string, timeout time.Duration) (execx.Result, error) {
	start := r.now()
	test, err := tools.ExecuteCommand(ctx, env.env, command, nil, timeout)
	finish := r.now()
	duration, exit := finish.Sub(start).Milliseconds(), int64(test.ExitCode)
	status := "ok"
	if err != nil || test.ExitCode != 0 || test.TimedOut {
		status = "error"
	}
	record := &agent.StoreRecorder{Store: r.db, RunID: run.ID, Phase: store.PhaseBaseline, Redactor: r.redactor}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	recordErr := record.Record(recordCtx, store.Step{Kind: "test", Tool: command, Status: status, OutputText: test.Output, ExitCode: &exit, DurationMS: &duration, StartedAt: start, FinishedAt: &finish})
	return test, errors.Join(err, recordErr, r.artifact(recordCtx, run.ID, "baseline", fmt.Sprintf("exit_code: %d; timed_out: %t\n%s", test.ExitCode, test.TimedOut, test.Output)))
}

func validateDraft(ctx context.Context, run store.Run, repo *config.Repo, draft tools.SpecDraft) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo == nil || run.ID <= 0 {
		return store.ErrConflict
	}
	if strings.TrimSpace(draft.Summary) == "" || len(draft.AcceptanceCriteria) > 12 || len(draft.AcceptanceCriteria)+len(draft.OpenQuestions) == 0 || len(draft.Canonical()) > 64<<10 {
		return errors.New("invalid specification content")
	}
	seen := map[string]bool{}
	for _, criterion := range draft.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" || seen[criterion] {
			return errors.New("criteria must be nonempty and distinct")
		}
		seen[criterion] = true
	}
	for _, list := range [][]string{draft.OpenQuestions, draft.TestPlan, draft.Risks} {
		for _, item := range list {
			if strings.TrimSpace(item) == "" {
				return errors.New("specification entries must not be empty")
			}
		}
	}
	policy, err := workspace.NewPolicy(repo.Protected)
	if err != nil {
		return err
	}
	for _, file := range draft.Files {
		if strings.TrimSpace(file.Change) == "" {
			return errors.New("file change description required")
		}
		if _, err := policy.Check(file.Path); err != nil {
			return err
		}
	}
	return nil
}
