package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/store"
)

const (
	setupStateTTL  = 10 * time.Minute
	setupStateMax  = 16
	setupCallback  = github.CallbackPath
	statusTimeout  = 15 * time.Second
	setupCSP       = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'"
	installBaseURL = "https://github.com/apps/"
)

// setupStates holds the one-time state values of in-flight Connect GitHub
// attempts. Only SHA-256 digests are kept; take compares every digest in
// constant time and removes a match, so a state works once.
type setupStates struct {
	mu      sync.Mutex
	entries []setupState
}

type setupState struct {
	digest  [32]byte
	expires time.Time
}

var errSetupBusy = errors.New("too many setup attempts in progress; wait ten minutes")

func (s *setupStates) issue(now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("cannot read random bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.entries) >= setupStateMax {
		return "", errSetupBusy
	}
	s.entries = append(s.entries, setupState{sha256.Sum256(raw), now.Add(setupStateTTL)})
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *setupStates) pruneLocked(now time.Time) {
	kept := s.entries[:0]
	for _, e := range s.entries {
		if now.Before(e.expires) {
			kept = append(kept, e)
		}
	}
	s.entries = kept
}

// take consumes state. It returns false for an unknown, reused, expired or
// malformed value.
func (s *setupStates) take(state string, now time.Time) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(state)
	if err != nil || len(raw) != 32 {
		return false
	}
	digest := sha256.Sum256(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	match := -1
	for i, e := range s.entries {
		if subtle.ConstantTimeCompare(e.digest[:], digest[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return false
	}
	s.entries = append(s.entries[:match], s.entries[match+1:]...)
	return true
}

func (s *Server) setupNow() time.Time {
	if s.opts.Runner.Clock != nil {
		return s.opts.Runner.Clock()
	}
	return time.Now()
}

// setupAvailability reports why the flow cannot run, or "" when it can.
func (s *Server) setupAvailability(ctx context.Context) (reason string, connected bool) {
	g := s.cfg.GitHub
	if g.AppID > 0 {
		return "GitHub is already connected (app_id is set in the configuration).", true
	}
	if _, found, err := s.db.GitHubApp(ctx); err != nil {
		return "Could not read the stored GitHub App.", false
	} else if found {
		return "GitHub is already connected.", true
	}
	if g.PrivateKeyFile != "" {
		if _, err := os.Lstat(g.PrivateKeyFile); err == nil {
			return "A private key file already exists, so GitHub is treated as connected. Remove it deliberately to start over.", true
		}
	}
	switch {
	case !g.App():
		return `Set [github] auth = "app" to use Connect GitHub.`, false
	case g.PrivateKeyFile == "":
		return "Set [github] private_key_file to the path where the key will be stored.", false
	case g.SetupPublicURL == "":
		return "Set [github] setup_public_url to this server's public origin.", false
	}
	return "", false
}

type setupPageData struct {
	Title, Message   string
	Connect          bool
	Slug, InstallURL string
	RestartHint      bool
	AppID            int64
}

var setupTemplate = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>patchbot · connect GitHub</title>
<link rel="stylesheet" href="/assets/app.css">
<script defer src="/assets/setup.js"></script></head>
<body><header><a href="/runs">patchbot</a><nav aria-label="Main"><a href="/">Inbox</a><a href="/runs">Runs</a><a href="/runs/new">New request</a><a href="/repos">Repositories</a><a href="/memory">Memory</a></nav></header>
<main id="main"><h1>{{.Title}}</h1>
{{with .Message}}<p role="status">{{.}}</p>{{end}}
{{if .Connect}}<div class="panel"><p>patchbot asks GitHub to create a private App named in the manifest, with only contents, issues and pull requests (write) and metadata (read), and no webhook. GitHub shows you the request first; you confirm it there. The private key goes straight to this server and is never shown to you.</p>
<button id="connect" type="button">Connect GitHub</button><p id="setup-status" role="status"></p><noscript><p>JavaScript is required for this page.</p></noscript></div>{{end}}
{{if .Slug}}<div class="panel"><h2>Next: install the App</h2>
<p>The App <strong>{{.Slug}}</strong> (ID {{.AppID}}) was created. Install it only on the repositories patchbot may work on:</p>
<p><a href="{{.InstallURL}}" rel="noopener noreferrer">Install {{.Slug}} on chosen repositories</a></p>
{{if .RestartHint}}<p class="warning">Restart patchbot (for example <code>systemctl restart patchbot</code>) so it starts using the new App. Until then new runs are refused.</p>{{end}}
<button id="recheck" type="button">Re-check status</button><p id="recheck-status" role="status"></p></div>{{end}}
</main></body></html>`))

func (s *Server) setupPage(w http.ResponseWriter, status int, data setupPageData) {
	w.Header().Set("Content-Security-Policy", setupCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = setupTemplate.Execute(w, data)
}

// githubSetupPage serves GET /setup/github. It is a data-free shell, public
// like the other shells: the manifest and state are only issued by the
// authenticated POST /api/github/setup/start that its script calls.
func (s *Server) githubSetupPage(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	if reason, _ := s.setupAvailability(r.Context()); reason != "" {
		s.setupPage(w, http.StatusConflict, setupPageData{Title: "Connect GitHub is not available", Message: reason})
		return
	}
	s.setupPage(w, http.StatusOK, setupPageData{Title: "Connect GitHub", Connect: true})
}

// githubSetupStart issues a state and the manifest. Bearer authentication and
// the cross-origin check in ServeHTTP apply.
func (s *Server) githubSetupStart(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodPost) {
		return
	}
	if reason, _ := s.setupAvailability(r.Context()); reason != "" {
		s.writeError(w, http.StatusConflict, "setup_unavailable", reason)
		return
	}
	if err := github.PreflightKeyFile(s.cfg.GitHub.PrivateKeyFile); err != nil {
		s.writeError(w, http.StatusConflict, "key_file", err.Error())
		return
	}
	name := s.cfg.GitHub.SetupAppName
	if name == "" {
		var err error
		if name, err = github.RandomAppName(); err != nil {
			s.writeError(w, http.StatusInternalServerError, "internal", "could not create an App name")
			return
		}
	}
	state, err := s.setup.issue(s.setupNow())
	if err != nil {
		if errors.Is(err, errSetupBusy) {
			s.writeError(w, http.StatusTooManyRequests, "busy", err.Error())
		} else {
			s.writeError(w, http.StatusInternalServerError, "internal", "could not create a setup state")
		}
		return
	}
	manifest, err := json.Marshal(github.NewManifest(s.cfg.GitHub.SetupPublicURL, name))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "internal", "could not encode the manifest")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"action":   github.SetupNewAppURL + "?state=" + url.QueryEscape(state),
		"manifest": string(manifest),
	})
}

// githubSetupCallback serves GET /setup/github/callback. Browser navigation
// from GitHub cannot carry the bearer token, so the single-use random state
// issued by the authenticated start call is the credential here.
func (s *Server) githubSetupCallback(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	ctx := r.Context()
	if reason, _ := s.setupAvailability(ctx); reason != "" {
		s.setupPage(w, http.StatusConflict, setupPageData{Title: "Connect GitHub is not available", Message: reason})
		return
	}
	query := r.URL.Query()
	code, state := query.Get("code"), query.Get("state")
	if !s.setup.take(state, s.setupNow()) {
		s.setupPage(w, http.StatusBadRequest, setupPageData{Title: "Connect GitHub failed", Message: "The setup link is invalid, expired or already used. Go back to /setup/github and start again."})
		return
	}
	keyPath := s.cfg.GitHub.PrivateKeyFile
	if err := github.PreflightKeyFile(keyPath); err != nil {
		s.setupPage(w, http.StatusConflict, setupPageData{Title: "Connect GitHub failed", Message: err.Error()})
		return
	}
	conv, err := github.ConvertManifest(ctx, s.opts.Runner.GitHubHTTP, s.opts.Runner.GitHubAPI, code)
	if err != nil {
		s.setupPage(w, http.StatusBadGateway, setupPageData{Title: "Connect GitHub failed", Message: err.Error() + ". Start again from /setup/github."})
		return
	}
	defer clear(conv.PEM)
	excluded := []string{s.cfg.DataDir(), filepath.Join(s.cfg.DataDir(), "workspaces")}
	if err := github.WriteNewKeyFile(keyPath, conv.PEM, excluded...); err != nil {
		s.setupPage(w, http.StatusInternalServerError, setupPageData{Title: "The App was created but its key was not saved",
			Message: err.Error() + ". Delete the App named " + conv.Slug + " in your GitHub developer settings, fix the problem and start again."})
		return
	}
	record := store.GitHubApp{AppID: conv.ID, Slug: conv.Slug, CreatedAt: s.setupNow()}
	if err := s.db.SaveGitHubApp(ctx, record); err != nil {
		s.setupPage(w, http.StatusInternalServerError, setupPageData{Title: "The App and key were saved, but recording it failed",
			Message: "Add app_id = " + strconv.FormatInt(conv.ID, 10) + " to [github] in the configuration and restart patchbot."})
		return
	}
	s.setupPage(w, http.StatusOK, setupPageData{Title: "GitHub connected", Slug: conv.Slug, AppID: conv.ID,
		InstallURL: installBaseURL + url.PathEscape(conv.Slug) + "/installations/new", RestartHint: !s.runner.GitHubConnected()})
}

// githubStatus serves GET /api/github/status.
func (s *Server) githubStatus(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	ctx := r.Context()
	out := map[string]any{"mode": s.cfg.GitHub.Auth, "connected": false, "slug": "", "app_id": int64(0)}
	if !s.cfg.GitHub.App() {
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	appID := s.cfg.GitHub.AppID
	slug := ""
	record, found, err := s.db.GitHubApp(ctx)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "internal", "could not read the stored GitHub App")
		return
	}
	if found {
		slug = record.Slug
		if appID == 0 {
			appID = record.AppID
		}
	}
	out["setup_available"], out["restart_required"] = false, false
	if reason, _ := s.setupAvailability(ctx); reason == "" {
		out["setup_available"] = true
	}
	if appID == 0 {
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	out["app_id"], out["slug"] = appID, slug
	excluded := []string{s.cfg.DataDir(), filepath.Join(s.cfg.DataDir(), "workspaces")}
	key, err := github.LoadAppPrivateKey(s.cfg.GitHub.PrivateKeyFile, excluded...)
	if err != nil {
		out["key_error"] = err.Error()
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	out["connected"] = true
	out["restart_required"] = !s.runner.GitHubConnected()
	app, err := github.NewApp(github.AppOptions{AppID: appID, Key: key, BaseURL: s.opts.Runner.GitHubAPI, HTTPClient: s.opts.Runner.GitHubHTTP})
	if err != nil {
		out["installations_error"] = "Could not check installations."
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	installs, err := app.Installations(checkCtx)
	if err != nil {
		out["installations_error"] = "Could not list installations from GitHub: " + err.Error()
	} else {
		out["installations"] = installs
	}
	s.writeJSON(w, http.StatusOK, out)
}
