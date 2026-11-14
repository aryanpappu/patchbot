package runner

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/secret"
)

// setupGitHub selects the per-repository credential source and the REST
// client. PAT mode keeps the old behaviour (one token for every repository).
// App mode loads the private key once, resolves the bot identity and commit
// author, and fails startup on any error: there is no fallback to a PAT.
func (r *Runner) setupGitHub(ctx context.Context) error {
	opts := &r.opts
	if opts.RepoTokens == nil && r.cfg.GitHub.App() && r.cfg.GitHub.AppID == 0 {
		// Setup mode: the App ID comes from the "Connect GitHub" flow. Without
		// one the server still starts, but every credential request fails closed
		// (never a PAT) and new runs are refused with ErrGitHubNotConnected.
		app, found, err := r.db.GitHubApp(ctx)
		if err != nil {
			return errors.New("could not read the connected GitHub App")
		}
		if found {
			r.cfg.GitHub.AppID = app.AppID
		} else {
			r.notConnected = true
			opts.RepoTokens = notConnectedTokens{}
		}
	}
	if opts.RepoTokens == nil && r.cfg.GitHub.App() {
		app, identity, err := NewGitHubApp(ctx, r.cfg, opts.GitHubAPI, opts.GitHubHTTP, r.registerToken)
		if err != nil {
			return err
		}
		opts.RepoTokens = app
		opts.BotLogin = identity.Login
		opts.CommitAuthor = gitx.Author{Name: identity.Login, Email: identity.CommitEmail()}
	}
	if opts.RepoTokens == nil {
		if opts.Tokens == nil {
			opts.Tokens = Token{Value: secret.New(os.Getenv("GITHUB_TOKEN"))}
		}
		if token, err := opts.Tokens.Token(ctx); err == nil {
			r.redactor.AddSecrets(secret.New(token))
			r.redactor.Add(github.GitHeaderForm(token))
		}
		opts.RepoTokens = github.StaticRepoTokens{Source: opts.Tokens}
	}
	r.tokens = opts.RepoTokens
	r.gh = opts.GitHub
	if r.gh == nil {
		var err error
		if r.cfg.GitHub.App() {
			r.gh, err = github.NewWithRepoTokens(opts.GitHubHTTP, opts.GitHubAPI, opts.RepoTokens, nil)
		} else {
			r.gh, err = github.New(opts.GitHubHTTP, opts.GitHubAPI, opts.Tokens)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ErrGitHubNotConnected refuses new runs until the Connect GitHub flow ran.
var ErrGitHubNotConnected = errors.New("GitHub is not connected yet: open /setup/github in the dashboard and connect GitHub first")

// notConnectedTokens fails every credential request in setup mode.
type notConnectedTokens struct{}

func (notConnectedTokens) RepoToken(context.Context, github.RepoRef) (string, error) {
	return "", ErrGitHubNotConnected
}

// GitHubConnected reports whether the runner has GitHub credentials. It is
// false only in app mode before the Connect GitHub flow created the App.
func (r *Runner) GitHubConnected() bool { return !r.notConnected }

// registerToken masks a minted token, and its git header form, in every
// redactor that output can pass through: the runner's own and the caller's
// (CLI or server). It runs before the token is used anywhere.
func (r *Runner) registerToken(token string) {
	for _, redactor := range []*secret.Redactor{r.redactor, r.opts.Redactor} {
		if redactor != nil {
			redactor.Add(token, github.GitHeaderForm(token))
		}
	}
}

// NewGitHubApp builds the App token source from [github] and resolves the bot
// identity. onMint receives every minted token before it is used.
func NewGitHubApp(ctx context.Context, cfg *config.Config, baseURL string, client *http.Client, onMint func(string)) (*github.App, github.AppIdentity, error) {
	g := cfg.GitHub
	key, err := github.LoadAppPrivateKey(g.PrivateKeyFile, cfg.DataDir(), filepath.Join(cfg.DataDir(), "workspaces"))
	if err != nil {
		return nil, github.AppIdentity{}, err
	}
	app, err := github.NewApp(github.AppOptions{AppID: g.AppID, Key: key, InstallationID: g.InstallationID,
		BaseURL: baseURL, HTTPClient: client, OnMint: onMint})
	if err != nil {
		return nil, github.AppIdentity{}, err
	}
	identity, err := app.Identity(ctx)
	if err != nil {
		return nil, github.AppIdentity{}, errors.Join(errors.New("GitHub App identity check failed; refusing to start (no PAT fallback)"), err)
	}
	return app, identity, nil
}

// githubCredentialError never quotes a token source's error text.
func githubCredentialError(err error) error {
	var mint *github.MintError
	if errors.As(err, &mint) {
		return mint
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("GitHub credential unavailable; check GITHUB_TOKEN or the [github] App settings")
}
