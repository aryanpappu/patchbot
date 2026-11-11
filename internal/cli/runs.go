package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

const runUsage = `Usage:
  patchbot run --repo owner/name --issue N [--mode fix|refine|investigate]
               [--provider P --model M] [--dry-run] [--config FILE]
  patchbot approve <id> [--config FILE]
  patchbot revise <id> --notes "..." [--config FILE]
  patchbot reject <id> [--config FILE]
  patchbot cancel <id> [--config FILE]
  patchbot runs [--status S] [--sync] [--config FILE]
  patchbot retry <id> [--provider P --model M] [--config FILE]

Approval always names the latest spec. CLI approvals use approval.local_actor.
Dry runs wait for approval too, then print spec, diff and PR text.
Exit codes: 0 success or waiting, 1 failed or draft PR, 2 usage, 3 active-run
conflict, 130 cancelled execution. There is no pre-approve option.
`

type runCommand struct {
	name          string
	id            int64
	request       runner.Request
	notes, status string
	sync          bool
}

func (a *App) parseRunCommand(name string, args []string) (runCommand, config.Options, error) {
	cmd := runCommand{name: name}
	opts := a.Config
	flags := flag.NewFlagSet("patchbot "+name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.Path, "config", opts.Path, "configuration file")
	switch name {
	case "run":
		flags.StringVar(&cmd.request.Repo, "repo", "", "allowed repository")
		flags.Int64Var(&cmd.request.Issue, "issue", 0, "issue number")
		flags.StringVar(&cmd.request.Mode, "mode", "", "fix, refine or investigate")
		flags.BoolVar(&cmd.request.DryRun, "dry-run", false, "print publication without GitHub writes")
	case "runs":
		flags.StringVar(&cmd.status, "status", "", "status filter")
		flags.BoolVar(&cmd.sync, "sync", false, "sync waiting GitHub commands once")
	case "revise":
		flags.StringVar(&cmd.notes, "notes", "", "revision notes")
	}
	if name == "run" || name == "retry" {
		flags.StringVar(&cmd.request.Provider, "provider", "", "provider instance")
		flags.StringVar(&cmd.request.Model, "model", "", "model for both stages")
	}
	if name != "run" && name != "runs" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		var err error
		cmd.id, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil || cmd.id <= 0 {
			return cmd, opts, errors.New("run ID must be a positive integer")
		}
		args = args[1:]
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cmd, opts, err
		}
		return cmd, opts, errors.New("invalid command options")
	}
	if flags.NArg() > 0 {
		return cmd, opts, errors.New("unexpected positional argument")
	}
	if name != "run" && name != "runs" && cmd.id == 0 {
		return cmd, opts, errors.New("run ID is required")
	}
	if name == "run" {
		if cmd.request.Repo == "" || cmd.request.Issue <= 0 {
			return cmd, opts, errors.New("--repo and a positive --issue are required")
		}
		if cmd.request.Mode != "" && cmd.request.Mode != config.ModeFix && cmd.request.Mode != config.ModeRefine && cmd.request.Mode != config.ModeInvestigate {
			return cmd, opts, errors.New("--mode must be fix, refine or investigate")
		}
	}
	if name == "revise" && strings.TrimSpace(cmd.notes) == "" {
		return cmd, opts, errors.New("--notes must not be empty")
	}
	if cmd.status != "" && !validStatus(store.Status(cmd.status)) {
		return cmd, opts, errors.New("unknown run status")
	}
	return cmd, opts, nil
}

func validStatus(status store.Status) bool {
	return status.IsActive() || status == store.StatusSucceeded || status == store.StatusFailed || status == store.StatusCancelled || status == store.StatusRejected
}

func (a *App) runControl(name string, args []string) int {
	cmd, opts, err := a.parseRunCommand(name, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(a.Stdout, runUsage)
			return 0
		}
		_, _ = fmt.Fprintf(a.Stderr, "patchbot %s: %s\n\n%s", name, secret.NewRedactor().Redact(err.Error()), runUsage)
		return 2
	}
	opts.Flags = append([]config.Override(nil), opts.Flags...)
	for _, override := range []config.Override{
		{Key: "defaults.provider", Value: cmd.request.Provider, Flag: "--provider"},
		{Key: "defaults.refine_model", Value: cmd.request.Model, Flag: "--model"},
		{Key: "defaults.code_model", Value: cmd.request.Model, Flag: "--model"},
		{Key: "defaults.mode", Value: cmd.request.Mode, Flag: "--mode"},
	} {
		if override.Value != "" {
			opts.Flags = append(opts.Flags, override)
		}
	}
	cfg, err := a.loadConfig(opts)
	if err != nil {
		a.printProblems(nil, err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		a.printProblems(cfg.Redactor(), err)
		return 1
	}
	if err := cfg.EnsureDataDir(); err != nil {
		a.printProblems(cfg.Redactor(), errors.New("could not create data directory"))
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabasePath())
	if err != nil {
		a.printProblems(cfg.Redactor(), err)
		return 1
	}
	defer func() { _ = db.Close() }()
	runnerOpts := a.RunnerOptions
	fullRedactor := secret.NewRedactor()
	fullRedactor.Extend(runnerOpts.Redactor)
	fullRedactor.Extend(a.outputRedactor)
	runnerOpts.Redactor = fullRedactor
	if runnerOpts.Constructors == nil {
		runnerOpts.Constructors = providers.Constructors(a.Providers)
	}
	if runnerOpts.Tokens == nil {
		lookup := opts.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, _ := lookup("GITHUB_TOKEN")
		runnerOpts.Tokens = runner.Token{Value: secret.New(value)}
	}
	service, err := runner.New(ctx, cfg, db, runnerOpts)
	if err != nil {
		a.printProblems(cfg.Redactor(), err)
		return 1
	}
	redactor := fullRedactor
	if token, err := runnerOpts.Tokens.Token(ctx); err == nil {
		redactor.AddSecrets(secret.New(token))
	}
	return a.executeRunCommand(ctx, cmd, cfg, db, service, redactor)
}

func (a *App) executeRunCommand(ctx context.Context, cmd runCommand, cfg *config.Config, db store.Store, service *runner.Runner, redactor *secret.Redactor) int {
	var run store.Run
	var err error
	switch cmd.name {
	case "run":
		run, err = service.Start(ctx, cmd.request)
	case "retry":
		run, err = service.Retry(ctx, cmd.id, cmd.request.Provider, cmd.request.Model)
		if err == nil && run.Status == store.StatusApproved {
			err = service.Execute(ctx, run.ID)
			run, _ = db.GetRun(context.WithoutCancel(ctx), run.ID)
		}
	case "approve":
		err = service.Approve(ctx, cmd.id, cfg.Approval.LocalActor)
		if err == nil {
			err = service.Execute(ctx, cmd.id)
		}
	case "revise":
		err = service.Revise(ctx, cmd.id, cmd.notes)
	case "reject":
		err = service.Reject(ctx, cmd.id)
	case "cancel":
		err = service.Cancel(ctx, cmd.id)
	case "runs":
		if cmd.sync {
			err = service.SyncWaiting(ctx)
		}
		if err == nil {
			err = a.listRuns(ctx, db, cmd.status, redactor)
		}
	}
	if cmd.id != 0 && cmd.name != "retry" {
		run, _ = db.GetRun(context.WithoutCancel(ctx), cmd.id)
	}
	if run.ID != 0 {
		_, _ = fmt.Fprintf(a.Stdout, "Run %d: %s (%s, %s)\n", run.ID, run.Status, run.Mode, run.Phase)
		if run.PRURL != "" {
			_, _ = fmt.Fprintln(a.Stdout, redactor.Redact(run.PRURL))
		}
		if run.DryRun || waitingForSpec(run.Status) {
			if printErr := a.printArtifacts(ctx, db, run, redactor); printErr != nil {
				err = errors.Join(err, printErr)
			}
		}
		if run.Status == store.StatusAwaitingApproval {
			_, _ = fmt.Fprintf(a.Stdout, "Approve with: patchbot approve %d\n", run.ID)
		}
		if run.Status == store.StatusNeedsInfo {
			_, _ = fmt.Fprintf(a.Stdout, "Provide answers with: patchbot revise %d --notes \"...\"\n", run.ID)
		}
	}
	if err != nil {
		a.printProblems(redactor, err)
		if errors.Is(err, store.ErrActiveRunExists) {
			return 3
		}
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	if run.Status == store.StatusCancelled && cmd.name != "cancel" {
		return 130
	}
	if run.PRDraft && run.Status == store.StatusSucceeded {
		return 1
	}
	return 0
}

func waitingForSpec(status store.Status) bool {
	return status == store.StatusAwaitingApproval || status == store.StatusNeedsInfo
}

func (a *App) printArtifacts(ctx context.Context, db store.Store, run store.Run, redactor *secret.Redactor) error {
	for _, kind := range []string{"spec", "report", "diff", "pr_title", "pr_body"} {
		artifact, err := db.GetArtifact(context.WithoutCancel(ctx), run.ID, kind, 0)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(a.Stdout, "\n%s:\n%s\n", kind, redactor.Redact(artifact.Content)); err != nil {
			return errors.New("could not write run artifact")
		}
	}
	return nil
}

func (a *App) listRuns(ctx context.Context, db store.Store, status string, redactor *secret.Redactor) error {
	table := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "ID\tREPO\tISSUE\tMODE\tSTATUS\tPHASE\tPR")
	filter := store.RunFilter{Limit: 500}
	if status != "" {
		filter.Statuses = []store.Status{store.Status(status)}
	}
	for {
		page, err := db.ListRuns(ctx, filter)
		if err != nil {
			return err
		}
		for _, run := range page.Runs {
			_, _ = fmt.Fprintf(table, "%d\t%s\t%d\t%s\t%s\t%s\t%s\n", run.ID, redactor.Redact(run.Repo), run.IssueNumber, run.Mode, run.Status, run.Phase, redactor.Redact(run.PRURL))
		}
		if page.NextCursor == "" {
			break
		}
		filter.Cursor = page.NextCursor
	}
	return table.Flush()
}
