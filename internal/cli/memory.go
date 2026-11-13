package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/memory"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

const memoryUsage = `Usage:
  patchbot memory list [--repo owner/name] [--status S] [--kind K] [--config FILE]
  patchbot memory approve <id> [--config FILE]
  patchbot memory reject <id> [--config FILE]
  patchbot memory forget <id> [--config FILE]
  patchbot memory add --repo owner/name --text "..." [--config FILE]

Statuses: active, pending, approved, rejected, revoked. Kinds: observation, lesson.
Only approved lessons and fresh active observations are given to the model, as
unverified context in the user turn. approve/reject act on pending lessons.
forget revokes any entry and erases lesson text; audit actions, content hashes,
run transcripts that already used the entry and backups are not rewritten.
add stores an operator lesson that is approved immediately.
`

type memoryCommand struct {
	action, repo, status, kind, text string
	id                               int64
}

func parseMemoryCommand(args []string) (memoryCommand, config.Options, error) {
	var cmd memoryCommand
	var opts config.Options
	if len(args) == 0 {
		return cmd, opts, errors.New("memory subcommand is required")
	}
	cmd.action, args = args[0], args[1:]
	if cmd.action == "-h" || cmd.action == "--help" || cmd.action == "help" {
		return cmd, opts, flag.ErrHelp
	}
	flags := flag.NewFlagSet("patchbot memory "+cmd.action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.Path, "config", "", "configuration file")
	switch cmd.action {
	case "list":
		flags.StringVar(&cmd.repo, "repo", "", "repository filter")
		flags.StringVar(&cmd.status, "status", "", "status filter")
		flags.StringVar(&cmd.kind, "kind", "", "kind filter")
	case "add":
		flags.StringVar(&cmd.repo, "repo", "", "allowed repository")
		flags.StringVar(&cmd.text, "text", "", "lesson text")
	case store.MemoryApprove, store.MemoryReject, store.MemoryForget:
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			if slices.Contains(args, "-h") || slices.Contains(args, "--help") {
				return cmd, opts, flag.ErrHelp
			}
			return cmd, opts, errors.New("memory ID is required")
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return cmd, opts, errors.New("memory ID must be a positive integer")
		}
		cmd.id, args = id, args[1:]
	default:
		return cmd, opts, errors.New("unknown memory subcommand")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cmd, opts, err
		}
		return cmd, opts, errors.New("invalid memory options")
	}
	if flags.NArg() > 0 {
		return cmd, opts, errors.New("unexpected positional argument")
	}
	if cmd.status != "" && !slices.Contains([]string{store.MemoryActive, store.MemoryPending, store.MemoryApproved, store.MemoryRejected, store.MemoryRevoked}, cmd.status) {
		return cmd, opts, errors.New("unknown memory status")
	}
	if cmd.kind != "" && cmd.kind != store.MemoryObservation && cmd.kind != store.MemoryLesson {
		return cmd, opts, errors.New("unknown memory kind")
	}
	if cmd.action == "add" && (strings.TrimSpace(cmd.repo) == "" || strings.TrimSpace(cmd.text) == "") {
		return cmd, opts, errors.New("--repo and --text are required")
	}
	return cmd, opts, nil
}

func (a *App) runMemory(args []string) int {
	cmd, parsed, err := parseMemoryCommand(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(a.Stdout, memoryUsage)
			return 0
		}
		_, _ = fmt.Fprintf(a.Stderr, "patchbot memory: %s\n\n%s", err, memoryUsage)
		return 2
	}
	opts := a.Config
	if parsed.Path != "" {
		opts.Path = parsed.Path
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
	redactor := secret.NewRedactor()
	redactor.Extend(cfg.Redactor())
	redactor.Extend(a.outputRedactor)
	if err := a.executeMemory(ctx, cfg, db, redactor, cmd); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			err = errors.New("memory entry not found")
		case errors.Is(err, store.ErrConflict):
			err = errors.New("memory entry is not in a state that allows this action")
		}
		a.printProblems(redactor, err)
		return 1
	}
	return 0
}

func (a *App) executeMemory(ctx context.Context, cfg *config.Config, db store.Store, redactor *secret.Redactor, cmd memoryCommand) error {
	switch cmd.action {
	case "list":
		filter := store.MemoryFilter{Repo: cmd.repo, Kind: cmd.kind, Limit: 1000}
		if cmd.status != "" {
			filter.Statuses = []string{cmd.status}
		}
		entries, err := db.ListMemory(ctx, filter)
		if err != nil {
			return err
		}
		if !cfg.Memory.Enabled {
			_, _ = fmt.Fprintln(a.Stdout, "Memory is disabled: nothing is retrieved or written by runs.")
		}
		table := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(table, "ID\tREPO\tKIND\tSTATUS\tSOURCE\tRUN\tCONTENT")
		for _, e := range entries {
			run := "-"
			if e.RunID != nil {
				run = strconv.FormatInt(*e.RunID, 10)
			}
			_, _ = fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, redactor.Redact(e.Repo), e.Kind, e.Status, e.Source, run, redactor.Redact(summary(e)))
		}
		return table.Flush()
	case "add":
		repo := strings.ToLower(strings.TrimSpace(cmd.repo))
		if cfg.Repos[repo] == nil {
			return errors.New("repository is not in configuration allowlist")
		}
		entry, err := memory.AddLesson(ctx, db, cfg.Redactor(), memory.LessonInput{Repo: repo, Text: cmd.text, Source: store.MemorySourceOperator, Actor: cfg.Approval.LocalActor, Channel: "cli"})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Stdout, "Memory %d: %s lesson for %s\n", entry.ID, entry.Status, redactor.Redact(entry.Repo))
		return nil
	default:
		entry, err := db.DecideMemory(ctx, cmd.id, cmd.action, cfg.Approval.LocalActor, "cli")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Stdout, "Memory %d: %s\n", entry.ID, entry.Status)
		return nil
	}
}

// summary is a single-line view; full lesson text is shown up to 120 bytes.
func summary(e store.MemoryEntry) string {
	text := e.Text
	if e.Kind == store.MemoryObservation {
		text = memory.RenderObservation(e)
	} else if text == "" {
		text = "(forgotten; content hash " + e.ContentHash + ")"
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= 120 {
		return text
	}
	cut := 117
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}
