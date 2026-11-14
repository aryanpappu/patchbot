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
	"time"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/memory"
	"github.com/aryanpappu/patchbot/internal/repos"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

const memoryUsage = `Usage:
  patchbot memory list [--repo owner/name] [--status S] [--kind K] [--config FILE]
  patchbot memory show <id> [--config FILE]
  patchbot memory approve <id> --hash PREFIX [--config FILE]
  patchbot memory reject <id> [--hash PREFIX] [--config FILE]
  patchbot memory forget <id> [--hash PREFIX] [--config FILE]
  patchbot memory add --repo owner/name --text "..." [--config FILE]

Statuses: active, pending, approved, rejected, revoked. Kinds: observation, lesson.
Only approved lessons and fresh active observations are given to the model, as
unverified context in the user turn. list shows allowlisted repositories only
and truncates text; show prints the full text and content hash. approve
requires --hash with at least 8 hex characters of the content hash you
reviewed. approve/reject act on pending lessons.
forget revokes any entry and erases lesson text; audit actions, content hashes,
run transcripts that already used the entry and backups are not rewritten.
add stores an operator lesson that is approved immediately.
`

type memoryCommand struct {
	action, repo, status, kind, text, hash string
	id                                     int64
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
	case "show", store.MemoryApprove, store.MemoryReject, store.MemoryForget:
		if cmd.action != "show" {
			flags.StringVar(&cmd.hash, "hash", "", "content hash prefix you reviewed")
		}
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
	cmd.hash = strings.ToLower(strings.TrimSpace(cmd.hash))
	if (cmd.action == store.MemoryApprove || cmd.hash != "") && !memory.ValidHashPrefix(cmd.hash) {
		return cmd, opts, errors.New("--hash must be at least 8 hex characters of the content hash shown by memory show")
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
		case errors.Is(err, memory.ErrHashMismatch):
			err = errors.New("content hash does not match; run memory show again and review the entry")
		case errors.Is(err, store.ErrDuplicateMemory):
			err = errors.New("this lesson already exists for the repository")
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
	registry, err := repos.New(ctx, cfg, db, redactor.Redact)
	if err != nil {
		return err
	}
	allowed := registry.Has
	switch cmd.action {
	case "list":
		repos := registry.Names()
		repo := strings.ToLower(strings.TrimSpace(cmd.repo))
		if repo != "" && !allowed(repo) {
			return errors.New("repository is not in configuration allowlist")
		}
		filter := store.MemoryFilter{Repo: repo, Repos: repos, Kind: cmd.kind, Limit: 1000}
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
		if !registry.Has(repo) {
			return errors.New("repository is not in configuration allowlist")
		}
		entry, err := memory.AddLesson(ctx, db, cfg.Redactor(), memory.LessonInput{Repo: repo, Text: cmd.text, Source: store.MemorySourceOperator, Actor: cfg.Approval.LocalActor, Channel: "cli"})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Stdout, "Memory %d: %s lesson for %s\n", entry.ID, entry.Status, redactor.Redact(entry.Repo))
		return nil
	case "show":
		return a.showMemory(ctx, db, redactor, cmd.id, allowed)
	default:
		entry, err := memory.Decide(ctx, db, memory.DecideInput{ID: cmd.id, Action: cmd.action, Actor: cfg.Approval.LocalActor, Channel: "cli", HashPrefix: cmd.hash, Allowed: allowed})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Stdout, "Memory %d: %s\n", entry.ID, entry.Status)
		return nil
	}
}

func (a *App) showMemory(ctx context.Context, db store.Store, redactor *secret.Redactor, id int64, allowed func(string) bool) error {
	e, err := db.GetMemory(ctx, id)
	if err != nil {
		return err
	}
	if !allowed(e.Repo) {
		return memory.ErrNotAllowlisted
	}
	actions, err := db.ListMemoryActions(ctx, id)
	if err != nil {
		return err
	}
	run := "-"
	if e.RunID != nil {
		run = strconv.FormatInt(*e.RunID, 10)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Memory %d\nRepository:   %s\nKind:         %s\nStatus:       %s\nSource:       %s\nRun:          %s\nCreated:      %s by %s\nContent hash: %s\n",
		e.ID, e.Repo, e.Kind, e.Status, e.Source, run, e.CreatedAt.Format(time.RFC3339), e.CreatedBy, e.ContentHash)
	for _, action := range actions {
		fmt.Fprintf(&b, "Action:       %s by %s via %s at %s\n", action.Action, action.Actor, action.Channel, action.CreatedAt.Format(time.RFC3339))
	}
	switch {
	case e.Kind == store.MemoryObservation:
		fmt.Fprintf(&b, "\n%s\n", memory.RenderObservation(e))
	case e.Text == "":
		b.WriteString("\n(forgotten: text erased)\n")
	default:
		// Full text, with any non-printable rune escaped visibly.
		fmt.Fprintf(&b, "\n%s\n", tools.Visible(e.Text))
	}
	if e.Kind == store.MemoryLesson && e.Status == store.MemoryPending {
		fmt.Fprintf(&b, "\nApprove with: patchbot memory approve %d --hash %s\n", e.ID, e.ContentHash[:12])
	}
	_, err = fmt.Fprint(a.Stdout, redactor.Redact(b.String()))
	return err
}

// summary is a single-line view; full lesson text is shown up to 120 bytes.
func summary(e store.MemoryEntry) string {
	text := tools.Visible(e.Text)
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
