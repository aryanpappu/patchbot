package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/server"
	"github.com/aryanpappu/patchbot/internal/store"
)

const serveUsage = `Usage: patchbot serve [--addr 127.0.0.1:8080] [--config FILE]

Serve the web page, JSON API and event streams. Non-loopback addresses require
PATCHBOT_UI_TOKEN. When set, enter it on the page; every API request requires
that bearer token. Page shells and static assets contain no private data.
`

func (a *App) runServe(args []string) int {
	flags := flag.NewFlagSet("patchbot serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	opts := a.Config
	var addr string
	flags.StringVar(&opts.Path, "config", opts.Path, "configuration file")
	flags.StringVar(&addr, "addr", "", "listening address")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(a.Stdout, serveUsage)
			return 0
		}
		_, _ = fmt.Fprint(a.Stderr, serveUsage)
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprint(a.Stderr, serveUsage)
		return 2
	}
	if addr != "" {
		opts.Flags = append(append([]config.Override(nil), opts.Flags...), config.Override{Key: "server.addr", Value: addr, Flag: "--addr"})
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	uiToken, _ := lookup("PATCHBOT_UI_TOKEN")
	githubToken, _ := lookup("GITHUB_TOKEN")
	redactor := secret.NewRedactor(githubToken)
	redactor.AddExact(uiToken)
	cfg, err := config.Load(opts)
	if err != nil {
		a.printProblems(redactor, err)
		return 1
	}
	redactor.Extend(cfg.Redactor())
	if err := server.ValidateAddress(cfg.Server.Addr, secret.New(uiToken)); err != nil {
		a.printProblems(redactor, err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		a.printProblems(redactor, err)
		return 1
	}
	if err := os.MkdirAll(cfg.DataDir(), 0700); err != nil {
		a.printProblems(redactor, errors.New("could not create data directory"))
		return 1
	}
	parent := a.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, cfg.DatabasePath())
	if err != nil {
		a.printProblems(redactor, errors.New("could not open server storage"))
		return 1
	}
	defer func() { _ = db.Close() }()
	runnerOpts := a.RunnerOptions
	if runnerOpts.Constructors == nil {
		runnerOpts.Constructors = providers.Constructors(a.Providers)
	}
	if runnerOpts.Tokens == nil {
		runnerOpts.Tokens = runner.Token{Value: secret.New(githubToken)}
	}
	service, err := server.New(ctx, cfg, db, server.Options{Token: secret.New(uiToken), Runner: runnerOpts})
	if err != nil {
		a.printProblems(redactor, err)
		return 1
	}
	err = service.ListenAndServe(ctx)
	if err != nil {
		a.printProblems(redactor, errors.New("server stopped with an error"))
		return 1
	}
	return 0
}
