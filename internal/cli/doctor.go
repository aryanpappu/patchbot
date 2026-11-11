package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/version"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

const doctorUsage = `Usage: patchbot doctor [--config FILE] [--online]

Check tools, config, credentials, private storage, migrations and repo commands.
Offline by default. --online only lists provider models and calls GitHub /user.
PASS and WARN exit 0; any FAIL exits 1. No secrets are printed.
`

var doctorEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

type doctorRow struct{ status, check, detail string }

func (a *App) runDoctor(args []string) int {
	opts := a.Config
	flags := flag.NewFlagSet("patchbot doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.Path, "config", opts.Path, "configuration file")
	online := flags.Bool("online", false, "check provider models and GitHub identity")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(a.Stdout, doctorUsage)
			return 0
		}
		_, _ = fmt.Fprint(a.Stderr, doctorUsage)
		return 2
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	token, _ := lookup("GITHUB_TOKEN")
	r := secret.NewRedactor()
	r.Extend(a.outputRedactor)
	r.AddExact(token)
	rows := []doctorRow{}
	add := func(status, check, detail string) { rows = append(rows, doctorRow{status, check, detail}) }
	cfg, err := a.loadConfig(opts)
	if err != nil {
		add("FAIL", "config", err.Error())
		return a.printDoctor(rows, r)
	}
	r.Extend(cfg.Redactor())
	if err := cfg.Validate(); err != nil {
		add("FAIL", "config", err.Error())
		return a.printDoctor(rows, r)
	}
	if cfg.Path() == "" {
		add("WARN", "config", "no file found; using defaults and overrides")
	} else {
		add("PASS", "config", cfg.Path())
	}

	parent := a.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	dirs := config.Default().Limits.TrustedPath
	if cfg != nil {
		dirs = cfg.Limits.TrustedPath
	}
	root, rootErr := os.Getwd()
	if rootErr != nil {
		add("FAIL", "tools", "cannot determine working directory")
	} else {
		for _, tool := range []string{"go", "git"} {
			output, err := doctorVersion(ctx, root, dirs, tool, r, cfg.DataDir(), filepath.Join(cfg.DataDir(), "workspaces"), opts.Dir)
			if err == nil {
				if tool == "git" {
					err = gitx.CheckVersion(output)
				} else {
					fields := strings.Fields(output)
					if len(fields) < 3 || !version.IsValid(fields[2]) || version.Compare(fields[2], "go1.27") < 0 {
						err = errors.New("go >= 1.27 is required")
					}
				}
			}
			if err != nil {
				add("FAIL", tool+" version", err.Error())
			} else {
				add("PASS", tool+" version", output)
			}
		}
	}
	if strings.TrimSpace(token) == "" {
		add("FAIL", "GitHub credential", "set GITHUB_TOKEN")
	} else {
		add("PASS", "GitHub credential", "GITHUB_TOKEN is set")
	}
	if cfg != nil {
		for _, name := range sortedNames(cfg.Providers) {
			p := cfg.Providers[name]
			switch {
			case p.APIKeyEnv == "" && p.Kind == config.KindOpenAI:
				add("WARN", "provider "+name, "no api_key_env; unauthenticated local endpoints are allowed")
			case !doctorEnvName.MatchString(p.APIKeyEnv):
				add("FAIL", "provider "+name, "api_key_env must name an uppercase environment variable")
			case !p.APIKey.IsSet():
				add("FAIL", "provider "+name, "set "+p.APIKeyEnv)
			default:
				add("PASS", "provider "+name, p.APIKeyEnv+" is set")
			}
		}
		doctorStorage(ctx, cfg, add)
		if rootErr == nil {
			for _, name := range sortedNames(cfg.Repos) {
				for _, id := range sortedNames(cfg.Repos[name].Commands) {
					p, err := execx.NewPolicy(root, map[string][]string{id: cfg.Repos[name].Commands[id]}, execx.PolicyOptions{TrustedPath: dirs, ExcludedRoots: []string{cfg.DataDir(), filepath.Join(cfg.DataDir(), "workspaces"), opts.Dir}})
					if err != nil {
						add("FAIL", name+" / "+id, err.Error())
					} else {
						argv, _ := p.Resolve(id, nil)
						add("PASS", name+" / "+id, argv[0])
					}
				}
			}
		}
		for _, warning := range cfg.Warnings() {
			add("WARN", "config", warning)
		}
		if *online {
			a.doctorOnline(ctx, cfg, token, add)
		}
	}
	for _, warning := range a.credentialWarnings() {
		add("WARN", "credential masking", warning)
	}
	if !*online {
		add("WARN", "online checks", "skipped; use --online for ListModels and GitHub /user")
	}
	return a.printDoctor(rows, r)
}

func (a *App) printDoctor(rows []doctorRow, r *secret.Redactor) int {
	table := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "RESULT\tCHECK\tDETAIL")
	code := 0
	for _, row := range rows {
		if row.status == "FAIL" {
			code = 1
		}
		detail := strings.Join(strings.Fields(r.Redact(row.detail)), " ")
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\n", row.status, r.Redact(row.check), detail)
	}
	if err := table.Flush(); err != nil {
		return 1
	}
	return code
}

func doctorStorage(ctx context.Context, cfg *config.Config, add func(string, string, string)) {
	info, err := os.Lstat(cfg.DataDir())
	switch {
	case errors.Is(err, os.ErrNotExist):
		add("WARN", "data directory", "absent; first run creates it with mode 0700")
	case err != nil || !info.IsDir() || info.Mode().Perm() != 0o700:
		add("FAIL", "data directory", "must be a real directory with mode 0700")
	default:
		add("PASS", "data directory", "mode 0700")
	}
	info, err = os.Lstat(cfg.DatabasePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		add("WARN", "database", "absent; first run creates it and applies migrations")
	case err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600:
		add("FAIL", "database", "must be a regular file with mode 0600")
	default:
		count, err := store.CheckMigrations(ctx, cfg.DatabasePath())
		if err != nil {
			add("FAIL", "database migrations", err.Error())
		} else {
			add("PASS", "database migrations", fmt.Sprintf("%d migrations; checksums match", count))
		}
	}
}

func (a *App) doctorOnline(ctx context.Context, cfg *config.Config, token string, add func(string, string, string)) {
	configs, err := cfg.LLMProviders()
	if err != nil {
		add("FAIL", "provider models", err.Error())
		return
	}
	constructors := a.RunnerOptions.Constructors
	if constructors == nil {
		constructors = providers.Constructors(a.Providers)
	}
	for _, p := range configs {
		construct := constructors[p.Kind]
		if construct == nil {
			add("FAIL", "models "+p.Name, "provider adapter unavailable")
			continue
		}
		provider, err := construct(p)
		if err == nil {
			_, err = provider.ListModels(ctx)
		}
		if err != nil {
			add("FAIL", "models "+p.Name, "ListModels failed; check credentials and endpoint")
		} else {
			add("PASS", "models "+p.Name, "ListModels succeeded")
		}
	}
	client := a.RunnerOptions.GitHub
	if client == nil {
		client, err = github.New(nil, "", runner.Token{Value: secret.New(token)})
	}
	identity, ok := client.(interface {
		CurrentUser(context.Context) (string, error)
	})
	if err == nil && ok {
		_, err = identity.CurrentUser(ctx)
	}
	if err != nil || !ok {
		add("FAIL", "GitHub /user", "identity check failed; check GITHUB_TOKEN")
	} else {
		add("PASS", "GitHub /user", "authenticated")
	}
}

func doctorVersion(ctx context.Context, root string, dirs []string, tool string, r *secret.Redactor, excluded ...string) (string, error) {
	roots := append([]string{root}, excluded...)
	var binary string
	var err error
	args := []string{"version"}
	if tool == "git" {
		binary, err = gitx.ResolveGit(dirs, roots...)
		args[0] = "--version"
	} else {
		binary, err = execx.ResolveExecutable(tool, dirs, roots...)
	}
	if err != nil {
		return "", err
	}
	executor, err := execx.NewLocalExecutor(root)
	if err != nil {
		return "", err
	}
	result, err := executor.Run(ctx, execx.Spec{
		Argv: append([]string{binary}, args...), Dir: root,
		Env:     []string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator)), "GOTOOLCHAIN=local", "GOENV=off"},
		Timeout: 5 * time.Second, OutputCap: 4096, Redactor: r,
	})
	if err != nil || result.ExitCode != 0 || result.TimedOut || result.Cancelled || result.Truncated {
		return "", errors.New("version command failed, timed out or exceeded its output limit")
	}
	return strings.TrimSpace(result.Output), nil
}

func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
