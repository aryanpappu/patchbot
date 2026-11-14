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

	"github.com/aryanpappu/patchbot/internal/cfaccess"
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
Offline by default. --online lists provider models and checks GitHub identity:
/user (PAT mode), or the App, its installations and granted permissions for
every configured repository (app mode).
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
			if tool == "go" && cfg.Executor.Container() {
				add("PASS", "go version", "not required on the host; repository commands run in the container image")
				continue
			}
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
	var app *github.App
	switch {
	case cfg.GitHub.App():
		app = doctorAppKey(cfg, a.RunnerOptions, r, add)
		if strings.TrimSpace(token) != "" {
			add("WARN", "GitHub credential", "GITHUB_TOKEN is set but ignored in app mode (there is no PAT fallback); unset it")
		}
	case strings.TrimSpace(token) == "":
		add("FAIL", "GitHub credential", "set GITHUB_TOKEN")
	default:
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
		a.doctorAccess(ctx, cfg, *online, add)
		if rootErr == nil {
			excluded := []string{filepath.Join(cfg.DataDir(), "workspaces"), opts.Dir}
			doctorExecutor(ctx, cfg, root, r, lookup, add, excluded...)
			where := ""
			if cfg.Executor.Container() {
				where = " (resolved in the container image)"
			}
			for _, name := range sortedNames(cfg.Repos) {
				for _, id := range sortedNames(cfg.Repos[name].Commands) {
					p, err := execx.NewPolicy(root, map[string][]string{id: cfg.Repos[name].Commands[id]}, cfg.CommandPolicyOptions(excluded...))
					if err != nil {
						add("FAIL", name+" / "+id, err.Error())
					} else {
						argv, _ := p.Resolve(id, nil)
						add("PASS", name+" / "+id, argv[0]+where)
					}
				}
			}
		}
		for _, warning := range cfg.Warnings() {
			add("WARN", "config", warning)
		}
		if *online {
			a.doctorOnline(ctx, cfg, token, app, add)
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

func (a *App) doctorOnline(ctx context.Context, cfg *config.Config, token string, app *github.App, add func(string, string, string)) {
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
	if cfg.GitHub.App() {
		doctorAppOnline(ctx, cfg, app, add)
		return
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

// doctorExecutor checks the container runtime binary, rootless mode, the
// digest-pinned image and whether the image is present locally (runs use
// --pull never). A rootful runtime is a warning: it still works, but the
// runtime daemon is root-equivalent and must not be reachable by patchbot.
func doctorExecutor(ctx context.Context, cfg *config.Config, root string, r *secret.Redactor, lookup func(string) (string, bool), add func(string, string, string), excluded ...string) {
	if !cfg.Executor.Container() {
		add("PASS", "executor", "local host processes (not a sandbox; use kind = \"container\" for untrusted code)")
		return
	}
	cc, err := cfg.ContainerConfig(lookup, excluded...)
	if err != nil {
		add("FAIL", "container runtime", err.Error())
		return
	}
	add("PASS", "container runtime", cc.Runtime)
	add("PASS", "container image", "digest-pinned "+cc.Image)
	run := func(args ...string) (string, error) {
		executor, err := execx.NewLocalExecutor(root)
		if err != nil {
			return "", err
		}
		result, err := executor.Run(ctx, execx.Spec{Argv: append([]string{cc.Runtime}, args...), Dir: root, Env: cc.HostEnv, Timeout: 15 * time.Second, OutputCap: 4096, Redactor: r})
		if err != nil || result.ExitCode != 0 || result.TimedOut || result.Cancelled {
			return "", errors.New("command failed or timed out")
		}
		return strings.TrimSpace(result.Output), nil
	}
	format := "{{.SecurityOptions}}"
	if cc.Flavor == execx.RuntimePodman {
		format = "{{.Host.Security.Rootless}}"
	}
	output, err := run("info", "--format", format)
	switch {
	case err != nil:
		add("FAIL", "container rootless", cc.Flavor+" info failed; is the runtime installed and XDG_RUNTIME_DIR/DOCKER_HOST set?")
	case cc.Flavor == execx.RuntimePodman && output == "true", cc.Flavor == execx.RuntimeDocker && strings.Contains(output, "rootless"):
		add("PASS", "container rootless", cc.Flavor+" runs rootless")
	default:
		add("WARN", "container rootless", cc.Flavor+" is not rootless; its daemon is root-equivalent, so use rootless podman or rootless docker")
	}
	if _, err := run("image", "inspect", "--format", "{{.Id}}", cc.Image); err != nil {
		add("WARN", "container image present", "not found locally; runs use --pull never, so pull it first: "+cc.Flavor+" pull "+cc.Image)
	} else {
		add("PASS", "container image present", "available locally")
	}
}

func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// doctorAppKey checks the App key file placement and signs a JWT locally.
// Errors are fixed strings; key material is never printed.
func doctorAppKey(cfg *config.Config, opts runner.Options, r *secret.Redactor, add func(string, string, string)) *github.App {
	excluded := []string{cfg.DataDir(), filepath.Join(cfg.DataDir(), "workspaces")}
	key, err := github.LoadAppPrivateKey(cfg.GitHub.PrivateKeyFile, excluded...)
	if err != nil {
		add("FAIL", "GitHub App key", err.Error())
		return nil
	}
	add("PASS", "GitHub App key", "regular file, mode 0600 or stricter, owned by this user, outside the data directory")
	app, err := github.NewApp(github.AppOptions{AppID: cfg.GitHub.AppID, Key: key, InstallationID: cfg.GitHub.InstallationID,
		BaseURL: opts.GitHubAPI, HTTPClient: opts.GitHubHTTP, OnMint: func(token string) { r.Add(token, github.GitHeaderForm(token)) }})
	if err == nil {
		_, err = app.SignJWT()
	}
	if err != nil {
		add("FAIL", "GitHub App JWT", "cannot sign a JWT with the configured key")
		return nil
	}
	add("PASS", "GitHub App JWT", "signed locally (RS256)")
	return app
}

// doctorAppOnline checks the App identity and, for every allowlisted
// repository, that the App is installed with the permissions runs need and
// that a single-repository token can be minted.
func doctorAppOnline(ctx context.Context, cfg *config.Config, app *github.App, add func(string, string, string)) {
	if app == nil {
		add("FAIL", "GitHub App", "skipped: the key check failed")
		return
	}
	identity, err := app.Identity(ctx)
	if err != nil {
		add("FAIL", "GitHub App identity", err.Error())
		return
	}
	add("PASS", "GitHub App identity", fmt.Sprintf("%s (commit author id %d)", identity.Login, identity.UserID))
	if len(cfg.Repos) == 0 {
		add("WARN", "GitHub App installation", "no repositories configured")
	}
	for _, name := range sortedNames(cfg.Repos) {
		owner, repoName, _ := strings.Cut(name, "/")
		repo := github.RepoRef{Owner: owner, Name: repoName}
		check := "GitHub App " + name
		info, err := app.Installation(ctx, repo)
		if err != nil {
			add("FAIL", check, "App is not installed on this repository or cannot reach it: "+err.Error())
			continue
		}
		if cfg.GitHub.InstallationID > 0 && info.ID != cfg.GitHub.InstallationID {
			add("FAIL", check, fmt.Sprintf("repository belongs to installation %d, not the configured installation_id", info.ID))
			continue
		}
		var missing []string
		for _, perm := range sortedNames(github.RunPermissions) {
			want, got := github.RunPermissions[perm], info.Permissions[perm]
			if got != want && (want != "read" || got != "write") {
				missing = append(missing, perm+":"+want)
			}
		}
		if len(missing) > 0 {
			add("FAIL", check, "installation lacks permissions: "+strings.Join(missing, ", "))
			continue
		}
		if _, err := app.RepoToken(ctx, repo); err != nil {
			add("FAIL", check, "cannot mint a single-repository token: "+err.Error())
			continue
		}
		add("PASS", check, fmt.Sprintf("installation %d; contents/issues/pull_requests write, metadata read; token minted", info.ID))
	}
}

// doctorAccess checks the Cloudflare Access settings and, online, that the
// team JWKS is reachable and holds RSA keys. Config validity is already
// enforced by Validate; this reports the mode and the live key set.
func (a *App) doctorAccess(ctx context.Context, cfg *config.Config, online bool, add func(string, string, string)) {
	auth := cfg.Server.Auth
	if auth.Mode != config.AuthCloudflareAccess {
		return
	}
	add("PASS", "server auth", fmt.Sprintf("cloudflare_access: team %s, %d allowed email(s)", auth.TeamDomain, len(auth.AllowedEmails)))
	if os.Getenv("PATCHBOT_UI_TOKEN") != "" {
		add("WARN", "server auth", "PATCHBOT_UI_TOKEN is set but ignored in cloudflare_access mode")
	}
	if !online {
		return
	}
	verifier, err := cfaccess.New(cfaccess.Config{TeamDomain: auth.TeamDomain, Audience: auth.Audience, AllowedEmails: auth.AllowedEmails, CertsURL: a.AccessCertsURL})
	if err == nil {
		var n int
		if n, err = verifier.Check(ctx); err == nil {
			add("PASS", "access JWKS", fmt.Sprintf("%d RSA signing key(s) at https://%s/cdn-cgi/access/certs", n, auth.TeamDomain))
			return
		}
	}
	add("FAIL", "access JWKS", "could not fetch usable RSA keys from the team certs URL; check team_domain and network")
}
