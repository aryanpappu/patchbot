package github

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RunPermissions are the only permissions a run token is minted with.
var RunPermissions = map[string]string{
	"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read",
}

const (
	// refreshMargin re-mints a cached token with less than this much life left.
	refreshMargin = 5 * time.Minute
	// maxMintAttempts bounds retries of one App API call.
	maxMintAttempts = 4
	// maxRateWait is the longest rate-limit reset patchbot waits for inline.
	maxRateWait = 2 * time.Minute
	// flightTimeout bounds a shared mint that outlives its first caller.
	flightTimeout = 2 * time.Minute
)

// MintError is a credential-free failure to obtain an App credential. Kind is
// "denied" (revoked/uninstalled/missing grant: never retried, never falls back
// to another credential), "rate_limited" or "unavailable".
type MintError struct {
	Kind  string
	Reset time.Time
	msg   string
}

func (e *MintError) Error() string { return "GitHub App authentication " + e.Kind + ": " + e.msg }

type appClock func() time.Time

// AppOptions configure an App token source.
type AppOptions struct {
	AppID int64
	Key   *rsa.PrivateKey
	// InstallationID is optional; zero resolves each repository's installation.
	InstallationID int64
	BaseURL        string
	HTTPClient     *http.Client
	// OnMint is called with every minted token before it is used anywhere,
	// so it can be registered with redactors.
	OnMint func(token string)
	// Clock and Sleep are test seams.
	Clock func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// AppIdentity is the App's bot account.
type AppIdentity struct {
	Slug   string
	Login  string // <slug>[bot]
	UserID int64
}

// CommitEmail is GitHub's noreply address that attributes commits to the bot.
func (i AppIdentity) CommitEmail() string {
	return fmt.Sprintf("%d+%s@users.noreply.github.com", i.UserID, i.Login)
}

// Installation describes the App installation covering one repository.
type Installation struct {
	ID          int64             `json:"id"`
	Permissions map[string]string `json:"permissions"`
}

type tokenKey struct {
	installation, repo int64
	permissions        string
}

type cachedToken struct {
	token   string
	expires time.Time
}

type flight struct {
	done  chan struct{}
	value any
	err   error
}

// App mints repository-scoped installation tokens. It is safe for concurrent
// use. Tokens are cached per (installation, repository ID, permission set);
// concurrent requests for the same key share one mint.
type App struct {
	opts      AppOptions
	jwtClient *HTTPClient
	anon      *HTTPClient
	clock     appClock

	mu       sync.Mutex
	installs map[string]int64
	repoIDs  map[string]int64
	tokens   map[tokenKey]cachedToken
	flights  map[string]*flight
}

var _ RepoTokenSource = (*App)(nil)
var _ TokenInvalidator = (*App)(nil)

// NewApp validates options. It performs no network calls.
func NewApp(opts AppOptions) (*App, error) {
	if opts.AppID <= 0 || opts.Key == nil || opts.InstallationID < 0 {
		return nil, errors.New("GitHub App requires app_id and a private key")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepContext
	}
	a := &App{opts: opts, clock: opts.Clock, installs: map[string]int64{}, repoIDs: map[string]int64{},
		tokens: map[tokenKey]cachedToken{}, flights: map[string]*flight{}}
	var err error
	a.jwtClient, err = newClient(opts.HTTPClient, opts.BaseURL, jwtSource{a}, nil)
	if err != nil {
		return nil, err
	}
	a.anon, err = newClient(opts.HTTPClient, opts.BaseURL, nil, nil)
	if err != nil {
		return nil, err
	}
	a.anon.anonymous = true
	return a, nil
}

type jwtSource struct{ a *App }

func (s jwtSource) Token(context.Context) (string, error) {
	return SignAppJWT(s.a.opts.Key, s.a.opts.AppID, s.a.clock())
}

// SignJWT signs a JWT locally (doctor's offline check).
func (a *App) SignJWT() (string, error) { return jwtSource{a}.Token(context.Background()) }

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func repoKey(repo RepoRef) string { return strings.ToLower(repo.Owner + "/" + repo.Name) }

func permissionKey(perms map[string]string) string {
	keys := make([]string, 0, len(perms))
	for k, v := range perms {
		keys = append(keys, k+"="+v)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// RepoToken returns an installation token narrowed to exactly this repository's
// immutable ID with RunPermissions. It never returns any other credential.
func (a *App) RepoToken(ctx context.Context, repo RepoRef) (string, error) {
	if !validRepoPart(repo.Owner) || !validRepoPart(repo.Name) {
		return "", errors.New("invalid repository reference")
	}
	installation, err := a.installationID(ctx, repo)
	if err != nil {
		return "", err
	}
	repoID, err := a.repositoryID(ctx, installation, repo)
	if err != nil {
		return "", err
	}
	key := tokenKey{installation, repoID, permissionKey(RunPermissions)}
	if token, ok := a.cached(key); ok {
		return token, nil
	}
	value, err := a.share(ctx, fmt.Sprintf("token/%d/%d/%s", key.installation, key.repo, key.permissions), func(ctx context.Context) (any, error) {
		if token, ok := a.cached(key); ok {
			return token, nil
		}
		minted, err := a.mint(ctx, installation, map[string]any{"repository_ids": []int64{repoID}, "permissions": RunPermissions}, RunPermissions,
			func(r mintedRepo) bool { return r.ID == repoID })
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.tokens[key] = cachedToken{token: minted.Token, expires: minted.ExpiresAt}
		a.mu.Unlock()
		return minted.Token, nil
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

func (a *App) cached(key tokenKey) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.tokens[key]
	if ok && entry.expires.Sub(a.clock()) > refreshMargin {
		return entry.token, true
	}
	return "", false
}

// Invalidate drops a cached token that GitHub rejected (401), so the next
// RepoToken mints a new one. The installation lookup is dropped too, which
// lets a reinstalled App be found again.
func (a *App) Invalidate(repo RepoRef, token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, entry := range a.tokens {
		if entry.token == token {
			delete(a.tokens, key)
		}
	}
	if a.opts.InstallationID == 0 {
		delete(a.installs, repoKey(repo))
	}
}

// share runs fn once per key across concurrent callers. fn runs on a context
// detached from any one caller (bounded by flightTimeout); each caller stops
// waiting when its own context ends.
func (a *App) share(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	a.mu.Lock()
	f, ok := a.flights[key]
	if !ok {
		f = &flight{done: make(chan struct{})}
		a.flights[key] = f
		go func() {
			work, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightTimeout)
			defer cancel()
			f.value, f.err = fn(work)
			a.mu.Lock()
			delete(a.flights, key)
			a.mu.Unlock()
			close(f.done)
		}()
	}
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

func (a *App) installationID(ctx context.Context, repo RepoRef) (int64, error) {
	if a.opts.InstallationID > 0 {
		return a.opts.InstallationID, nil
	}
	name := repoKey(repo)
	a.mu.Lock()
	id, ok := a.installs[name]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	value, err := a.share(ctx, "install/"+name, func(ctx context.Context) (any, error) {
		info, err := a.Installation(ctx, repo)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.installs[name] = info.ID
		a.mu.Unlock()
		return info.ID, nil
	})
	if err != nil {
		return 0, err
	}
	return value.(int64), nil
}

// Installation returns the installation covering repo and the permissions the
// owner granted it (GET /repos/{owner}/{repo}/installation with the App JWT).
func (a *App) Installation(ctx context.Context, repo RepoRef) (Installation, error) {
	target, err := a.jwtClient.endpoint(repo, "installation")
	if err != nil {
		return Installation{}, err
	}
	var info Installation
	err = a.retry(ctx, func() error {
		_, err := a.jwtClient.request(ctx, nil, http.MethodGet, target, nil, &info, http.StatusOK)
		return err
	})
	if err != nil {
		return Installation{}, err
	}
	if info.ID <= 0 {
		return Installation{}, &MintError{Kind: "denied", msg: "installation response has no ID"}
	}
	return info, nil
}

// repositoryID resolves the immutable repository ID once per process with a
// metadata-only token narrowed to the repository's name. Later run tokens are
// narrowed by that ID, so a deleted and recreated owner/name fails closed.
func (a *App) repositoryID(ctx context.Context, installation int64, repo RepoRef) (int64, error) {
	name := repoKey(repo)
	a.mu.Lock()
	id, ok := a.repoIDs[name]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	value, err := a.share(ctx, fmt.Sprintf("repoid/%d/%s", installation, name), func(ctx context.Context) (any, error) {
		perms := map[string]string{"metadata": "read"}
		minted, err := a.mint(ctx, installation, map[string]any{"repositories": []string{repo.Name}, "permissions": perms}, perms,
			func(r mintedRepo) bool { return r.ID > 0 && strings.EqualFold(r.FullName, repo.Owner+"/"+repo.Name) })
		if err != nil {
			return nil, err
		}
		id := minted.Repositories[0].ID
		a.mu.Lock()
		a.repoIDs[name] = id
		a.mu.Unlock()
		return id, nil
	})
	if err != nil {
		return 0, err
	}
	return value.(int64), nil
}

type mintedRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

type mintedToken struct {
	Token        string            `json:"token"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Permissions  map[string]string `json:"permissions"`
	Repositories []mintedRepo      `json:"repositories"`
}

// verifyScope fails closed unless the response states exactly the requested
// permissions and exactly one repository that matches. A missing, null or empty
// field is unverified scope and is refused, never assumed.
func verifyScope(out mintedToken, want map[string]string, repoMatches func(mintedRepo) bool) error {
	if len(out.Permissions) == 0 {
		return &MintError{Kind: "denied", msg: "token response does not state its permissions"}
	}
	for name, level := range want {
		if out.Permissions[name] != level {
			return &MintError{Kind: "denied", msg: "token lacks permission " + name + ":" + level}
		}
	}
	for name := range out.Permissions {
		if _, ok := want[name]; !ok {
			return &MintError{Kind: "denied", msg: "token carries unrequested permission " + name}
		}
	}
	if len(out.Repositories) != 1 || repoMatches == nil || !repoMatches(out.Repositories[0]) {
		return &MintError{Kind: "denied", msg: "token is not limited to exactly the requested repository"}
	}
	return nil
}

// mint calls POST /app/installations/{id}/access_tokens and returns the token
// only after verifyScope passes; a token that fails it is never cached.
func (a *App) mint(ctx context.Context, installation int64, body map[string]any, want map[string]string, repoMatches func(mintedRepo) bool) (mintedToken, error) {
	target := *a.jwtClient.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	target.RawPath = ""
	var out mintedToken
	err := a.retry(ctx, func() error {
		out = mintedToken{}
		_, err := a.jwtClient.request(ctx, nil, http.MethodPost, &target, body, &out, http.StatusCreated)
		return err
	})
	if err != nil {
		return mintedToken{}, err
	}
	if out.Token == "" || strings.ContainsAny(out.Token, " \t\r\n\x00") || out.ExpiresAt.IsZero() {
		return mintedToken{}, &MintError{Kind: "unavailable", msg: "invalid installation token response"}
	}
	if a.opts.OnMint != nil {
		a.opts.OnMint(out.Token)
	}
	if !out.ExpiresAt.After(a.clock()) {
		return mintedToken{}, &MintError{Kind: "unavailable", msg: "installation token already expired"}
	}
	if err := verifyScope(out, want, repoMatches); err != nil {
		return mintedToken{}, err
	}
	return out, nil
}

// retry runs one App API call with bounded attempts. Rate limits wait for the
// reset (with jitter) when it is near; server errors back off exponentially.
// Revocation, missing installations and permission denials are not retried.
func (a *App) retry(ctx context.Context, call func() error) error {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := call()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var wait time.Duration
		var limited *ErrRateLimited
		var api *APIError
		switch {
		case errors.As(err, &limited):
			// Reset comes from GitHub's headers against the real clock.
			wait = time.Second
			if !limited.Reset.IsZero() {
				wait = max(time.Until(limited.Reset), time.Second)
			}
			if wait > maxRateWait || attempt >= maxMintAttempts {
				return &MintError{Kind: "rate_limited", Reset: limited.Reset, msg: "GitHub rate limit; retry after the reset"}
			}
			wait += time.Duration(rand.Int64N(int64(time.Second))) // #nosec G404 -- jitter only.
		case errors.As(err, &api) && (api.StatusCode == http.StatusUnauthorized || api.StatusCode == http.StatusForbidden ||
			api.StatusCode == http.StatusNotFound || api.StatusCode == http.StatusUnprocessableEntity):
			return &MintError{Kind: "denied", msg: fmt.Sprintf("GitHub refused the App request (HTTP %d); check the installation, its repositories and permissions", api.StatusCode)}
		case errors.As(err, &api) && api.StatusCode < 500:
			return &MintError{Kind: "unavailable", msg: fmt.Sprintf("unexpected GitHub response (HTTP %d)", api.StatusCode)}
		default:
			if attempt >= maxMintAttempts {
				return &MintError{Kind: "unavailable", msg: "GitHub App API unreachable"}
			}
			wait, backoff = backoff, backoff*2
		}
		if err := a.opts.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// Identity resolves the App's bot login from GET /app (JWT) and the bot user
// ID from GET /users/<slug>[bot]. Either failing is fatal for app mode.
func (a *App) Identity(ctx context.Context) (AppIdentity, error) {
	target := *a.jwtClient.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/app"
	target.RawPath = ""
	var app struct {
		Slug string `json:"slug"`
	}
	err := a.retry(ctx, func() error {
		_, err := a.jwtClient.request(ctx, nil, http.MethodGet, &target, nil, &app, http.StatusOK)
		return err
	})
	if err != nil {
		return AppIdentity{}, err
	}
	if app.Slug == "" || !validSlug(app.Slug) {
		return AppIdentity{}, errors.New("GitHub App identity: missing or invalid slug")
	}
	login := app.Slug + "[bot]"
	user := *a.anon.baseURL
	escaped := strings.TrimRight(user.EscapedPath(), "/") + "/users/" + url.PathEscape(login)
	user.Path, user.RawPath = strings.TrimRight(user.Path, "/")+"/users/"+login, escaped
	var bot struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	err = a.retry(ctx, func() error {
		_, err := a.anon.request(ctx, nil, http.MethodGet, &user, nil, &bot, http.StatusOK)
		return err
	})
	if err != nil {
		return AppIdentity{}, fmt.Errorf("GitHub App bot user %s unavailable: %w", login, err)
	}
	if bot.ID <= 0 || !strings.EqualFold(bot.Login, login) {
		return AppIdentity{}, fmt.Errorf("GitHub App bot user %s unavailable", login)
	}
	return AppIdentity{Slug: app.Slug, Login: login, UserID: bot.ID}, nil
}

func validSlug(slug string) bool {
	for _, ch := range slug {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' && ch != '_' {
			return false
		}
	}
	return len(slug) <= 100
}

// GitHeaderForm is the base64 value git sends in its extraheader for token.
// Registering it with redactors covers the encoded credential as well.
func GitHeaderForm(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}
