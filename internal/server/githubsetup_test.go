//go:build unix

package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/testhttp"
)

const (
	setupClientSecret  = "client-secret-must-not-persist"
	setupWebhookSecret = "webhook-secret-must-not-persist"
	setupUIToken       = "ui-token-for-setup-tests"
)

var (
	setupKeyOnce sync.Once
	setupKeyPEM  []byte
)

func testAppPEM(t *testing.T) []byte {
	t.Helper()
	setupKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		setupKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	})
	return setupKeyPEM
}

// fakeGitHubAPI answers the manifest conversion and the installation listing.
type fakeGitHubAPI struct {
	t           *testing.T
	mu          sync.Mutex
	conversions []string
	installs    int
	jwt         string
}

func (g *fakeGitHubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app-manifests/") && strings.HasSuffix(r.URL.Path, "/conversions"):
		code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app-manifests/"), "/conversions")
		g.conversions = append(g.conversions, code)
		if code != "good-code" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found echo ` + setupClientSecret + `"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4321, "slug": "patchbot-abc123", "node_id": "x",
			"client_id": "Iv1.abc", "client_secret": setupClientSecret, "webhook_secret": setupWebhookSecret, "pem": string(testAppPEM(g.t))})
	case r.URL.Path == "/app/installations":
		g.installs++
		g.jwt = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 99, "account": map[string]string{"login": "pappumishra"}, "repository_selection": "selected"}})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

type setupFixture struct {
	t       *testing.T
	cfg     *config.Config
	db      *store.SQLite
	server  *Server
	api     *fakeGitHubAPI
	keyPath string
	keyDir  string
	now     func() time.Time
	advance func(time.Duration)
	logs    *bytes.Buffer
}

func newSetupFixture(t *testing.T, uiToken string, prepare func(keyPath string)) *setupFixture {
	t.Helper()
	f := &setupFixture{t: t, api: &fakeGitHubAPI{t: t}, logs: &bytes.Buffer{}}
	var mu sync.Mutex
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	f.advance = func(d time.Duration) { mu.Lock(); defer mu.Unlock(); clock = clock.Add(d) }
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	dataDir, keyDir := t.TempDir(), t.TempDir()
	f.keyDir = keyDir
	f.keyPath = filepath.Join(keyDir, "github-app.pem")
	if prepare != nil {
		prepare(f.keyPath)
	}
	path := filepath.Join(t.TempDir(), "patchbot.toml")
	body := fmt.Sprintf(`[defaults]
approvers = ["owner"]
[providers.anthropic]
api_key_env = ""
default_model = "test-model"
models = ["test-model"]
price_free = true
[data]
dir = %q
[server]
addr = "127.0.0.1:80"
[github]
auth = "app"
private_key_file = %q
setup_public_url = "https://patchbot.example.org"
setup_app_name = "patchbot-test"
[repos."owner/repo"]
commands = {test = ["test", "-f", "result.txt"]}
`, dataDir, f.keyPath)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.cfg, err = config.Load(config.Options{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	f.db, err = store.Open(t.Context(), f.cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	hub := testhttp.NewServer(f.api)
	t.Cleanup(hub.Close)
	provider := fake.New("anthropic")
	opts := Options{Token: secret.New(uiToken), HeartbeatInterval: 20 * time.Millisecond, PollInterval: 10 * time.Millisecond, SyncInterval: time.Hour,
		Runner: runner.Options{GitHubAPI: hub.URL, GitHubHTTP: hub.Client(), Clock: f.now, HeartbeatInterval: 10 * time.Millisecond,
			Constructors: llm.Constructors{"anthropic": func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil }}}}
	f.server, err = New(t.Context(), f.cfg, f.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *setupFixture) do(method, target string, bearer bool) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://"+f.cfg.Server.Addr+target, nil)
	if bearer && f.server.authenticated {
		r.Header.Set("Authorization", "Bearer "+setupUIToken)
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}

// start asks for a manifest and returns the state and decoded response.
func (f *setupFixture) start() (state string, manifest string) {
	f.t.Helper()
	w := f.do("POST", "/api/github/setup/start", true)
	if w.Code != 200 {
		f.t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var out struct{ Action, Manifest string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	u, err := url.Parse(out.Action)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.Path != "/settings/apps/new" {
		f.t.Fatalf("action %q", out.Action)
	}
	state = u.Query().Get("state")
	if len(state) != 43 { // 32 random bytes, base64url without padding
		f.t.Fatalf("state %q is not 32 random bytes", state)
	}
	return state, out.Manifest
}

func (f *setupFixture) callback(code, state string) *httptest.ResponseRecorder {
	return f.do("GET", "/setup/github/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), false)
}

func TestSetupManifestIsExact(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	_, manifest := f.start()
	want := `{"name":"patchbot-test","url":"https://patchbot.example.org","hook_attributes":{"url":"https://patchbot.example.org/hooks/github","active":false},` +
		`"redirect_url":"https://patchbot.example.org/setup/github/callback","public":false,` +
		`"default_permissions":{"contents":"write","issues":"write","metadata":"read","pull_requests":"write"}}`
	if manifest != want {
		t.Fatalf("manifest\n got %s\nwant %s", manifest, want)
	}
	if strings.Contains(manifest, "default_events") {
		t.Fatal("manifest must subscribe to no events")
	}
}

func TestSetupDefaultAppNameIsRandom(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	f.cfg.GitHub.SetupAppName = ""
	_, first := f.start()
	_, second := f.start()
	re := regexp.MustCompile(`"name":"(patchbot-[a-z0-9]{6})"`)
	a, b := re.FindStringSubmatch(first), re.FindStringSubmatch(second)
	if a == nil || b == nil || a[1] == b[1] {
		t.Fatalf("names %v %v", a, b)
	}
}

func TestSetupStateIsSingleUseExpiringAndExact(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	// Mismatch, empty and malformed values never reach GitHub.
	good, _ := f.start()
	for _, bad := range []string{"", "x", strings.Repeat("A", 43), good + "A", good[:len(good)-1] + "_"} {
		if bad == good {
			continue
		}
		if w := f.callback("good-code", bad); w.Code != 400 {
			t.Fatalf("state %q: %d", bad, w.Code)
		}
	}
	// Expired.
	f.advance(10*time.Minute + time.Second)
	if w := f.callback("good-code", good); w.Code != 400 {
		t.Fatalf("expired: %d", w.Code)
	}
	if len(f.api.conversions) != 0 {
		t.Fatal("GitHub was called for an invalid state")
	}
	// Valid once, never twice.
	fresh, _ := f.start()
	if w := f.callback("good-code", fresh); w.Code != 200 {
		t.Fatalf("valid state: %d %s", w.Code, w.Body.String())
	}
	if w := f.callback("good-code", fresh); w.Code != 409 && w.Code != 400 {
		t.Fatalf("replay: %d", w.Code)
	}
	if len(f.api.conversions) != 1 {
		t.Fatalf("conversions %v", f.api.conversions)
	}
}

func TestSetupStateIsConsumedEvenWhenConversionFails(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	state, _ := f.start()
	w := f.callback("bad-code", state)
	if w.Code != 502 {
		t.Fatalf("conversion failure: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), setupClientSecret) || strings.Contains(f.logs.String(), setupClientSecret) {
		t.Fatal("GitHub error body leaked")
	}
	if _, err := os.Lstat(f.keyPath); err == nil {
		t.Fatal("key written after a failed exchange")
	}
	if w := f.callback("good-code", state); w.Code != 400 {
		t.Fatalf("reused state: %d", w.Code)
	}
}

func TestSetupSuccessWritesKeyAndRowAndLeaksNothing(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	if f.server.runner.GitHubConnected() {
		t.Fatal("runner must start in setup mode")
	}
	state, _ := f.start()
	w := f.callback("good-code", state)
	if w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "https://github.com/apps/patchbot-abc123/installations/new") || !strings.Contains(body, "Re-check status") {
		t.Fatalf("next step missing: %s", body)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self' https://github.com") {
		t.Fatal("setup page CSP lacks the GitHub form target")
	}
	info, err := os.Lstat(f.keyPath)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("key file: %v %v", info, err)
	}
	if got, _ := os.ReadFile(f.keyPath); !bytes.Equal(got, testAppPEM(t)) {
		t.Fatal("key content differs")
	}
	entries, _ := os.ReadDir(f.keyDir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	app, found, err := f.db.GitHubApp(t.Context())
	if err != nil || !found || app.AppID != 4321 || app.Slug != "patchbot-abc123" || app.InstallationID != 0 {
		t.Fatalf("row: %+v %v %v", app, found, err)
	}
	// Nothing secret in the response, logs or database file.
	dbBytes, _ := os.ReadFile(f.cfg.DatabasePath())
	for name, haystack := range map[string]string{"response": body, "logs": f.logs.String(), "db": string(dbBytes)} {
		for _, secretValue := range []string{setupClientSecret, setupWebhookSecret, "PRIVATE KEY", "good-code"} {
			if strings.Contains(haystack, secretValue) {
				t.Fatalf("%s contains %q", name, secretValue)
			}
		}
	}
	// A second attempt is refused: connected.
	if w := f.do("GET", "/setup/github", false); w.Code != 409 || !strings.Contains(w.Body.String(), "already connected") {
		t.Fatalf("setup page after connect: %d", w.Code)
	}
	if w := f.do("POST", "/api/github/setup/start", true); w.Code != 409 {
		t.Fatalf("start after connect: %d", w.Code)
	}
}

func TestSetupRefusesExistingKeyFile(t *testing.T) {
	existing := []byte("existing operator key, must stay untouched")
	f := newSetupFixture(t, setupUIToken, func(keyPath string) {
		if err := os.WriteFile(keyPath, existing, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if w := f.do("GET", "/setup/github", false); w.Code != 409 {
		t.Fatalf("page: %d", w.Code)
	}
	if w := f.do("POST", "/api/github/setup/start", true); w.Code != 409 {
		t.Fatalf("start: %d", w.Code)
	}
	if w := f.callback("good-code", "whatever"); w.Code != 409 {
		t.Fatalf("callback: %d", w.Code)
	}
	if got, _ := os.ReadFile(f.keyPath); !bytes.Equal(got, existing) {
		t.Fatal("existing key was modified")
	}
	if len(f.api.conversions) != 0 {
		t.Fatal("code was spent although the key path is taken")
	}
}

func TestSetupKeyAppearingBetweenStartAndCallbackIsNotOverwritten(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	state, _ := f.start()
	existing := []byte("appeared later")
	if err := os.WriteFile(f.keyPath, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := f.callback("good-code", state); w.Code != 409 {
		t.Fatalf("callback: %d", w.Code)
	}
	if got, _ := os.ReadFile(f.keyPath); !bytes.Equal(got, existing) || len(f.api.conversions) != 0 {
		t.Fatal("existing key replaced or code spent")
	}
}

func TestSetupDisabledWhenAlreadyConnectedByDatabaseOrConfig(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	if err := f.db.SaveGitHubApp(t.Context(), store.GitHubApp{AppID: 7, Slug: "other"}); err != nil {
		t.Fatal(err)
	}
	if w := f.do("GET", "/setup/github", false); w.Code != 409 {
		t.Fatalf("db connected: %d", w.Code)
	}
	if err := f.db.SaveGitHubApp(t.Context(), store.GitHubApp{AppID: 8, Slug: "second"}); err == nil {
		t.Fatal("second App row accepted")
	}
	g := newSetupFixture(t, setupUIToken, nil)
	g.cfg.GitHub.AppID = 55
	if w := g.do("GET", "/setup/github", false); w.Code != 409 {
		t.Fatalf("config connected: %d", w.Code)
	}
}

func TestSetupUnavailableWithoutConfiguration(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	f.cfg.GitHub.SetupPublicURL = ""
	if w := f.do("GET", "/setup/github", false); w.Code != 409 || !strings.Contains(w.Body.String(), "setup_public_url") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f.cfg.GitHub.SetupPublicURL = "https://patchbot.example.org"
	f.cfg.GitHub.Auth = config.GitHubAuthPAT
	if w := f.do("GET", "/setup/github", false); w.Code != 409 {
		t.Fatalf("pat mode: %d", w.Code)
	}
}

func TestSetupAuthentication(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	if w := f.do("POST", "/api/github/setup/start", false); w.Code != 401 {
		t.Fatalf("start without bearer: %d", w.Code)
	}
	if w := f.do("GET", "/api/github/status", false); w.Code != 401 {
		t.Fatalf("status without bearer: %d", w.Code)
	}
	// The data-free shell is reachable; the callback relies on its state.
	if w := f.do("GET", "/setup/github", false); w.Code != 200 {
		t.Fatalf("shell: %d", w.Code)
	}
	if w := f.callback("good-code", "unknown"); w.Code != 400 {
		t.Fatalf("callback with unknown state: %d", w.Code)
	}
	if w := f.do("GET", "/assets/setup.js", false); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("setup.js: %d", w.Code)
	}
}

func TestSetupStartRefusesWhenKeyDirectoryMissing(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	f.cfg.GitHub.PrivateKeyFile = filepath.Join(f.keyDir, "missing", "k.pem")
	if w := f.do("POST", "/api/github/setup/start", true); w.Code != 409 {
		t.Fatalf("start: %d", w.Code)
	}
}

func TestRunsRefusedBeforeConnection(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	r := httptest.NewRequest("POST", "http://"+f.cfg.Server.Addr+"/api/runs", strings.NewReader(`{"repo":"owner/repo","issue":12,"mode":"fix"}`))
	r.Header.Set("Authorization", "Bearer "+setupUIToken)
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "github_not_connected") || !strings.Contains(w.Body.String(), "/setup/github") {
		t.Fatalf("run before connect: %d %s", w.Code, w.Body.String())
	}
	if page, err := f.db.ListRuns(t.Context(), store.RunFilter{}); err != nil || len(page.Runs) != 0 {
		t.Fatalf("a run was created: %v %v", page, err)
	}
}

func TestGitHubStatus(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	var before map[string]any
	w := f.do("GET", "/api/github/status", true)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &before) != nil || before["connected"] != false || before["setup_available"] != true {
		t.Fatalf("before: %d %s", w.Code, w.Body.String())
	}
	state, _ := f.start()
	if w := f.callback("good-code", state); w.Code != 200 {
		t.Fatalf("callback %d", w.Code)
	}
	w = f.do("GET", "/api/github/status", true)
	var after struct {
		Connected       bool   `json:"connected"`
		Slug            string `json:"slug"`
		AppID           int64  `json:"app_id"`
		RestartRequired bool   `json:"restart_required"`
		Installations   []struct {
			ID                  int64  `json:"id"`
			Account             string `json:"account"`
			RepositorySelection string `json:"repository_selection"`
		} `json:"installations"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &after) != nil {
		t.Fatalf("after: %d %s", w.Code, w.Body.String())
	}
	if !after.Connected || after.Slug != "patchbot-abc123" || after.AppID != 4321 || !after.RestartRequired ||
		len(after.Installations) != 1 || after.Installations[0].Account != "pappumishra" || after.Installations[0].RepositorySelection != "selected" {
		t.Fatalf("status %+v", after)
	}
	if !strings.HasPrefix(f.api.jwt, "Bearer eyJ") || strings.Count(f.api.jwt, ".") != 2 {
		t.Fatalf("installations were not requested with a JWT: %q", f.api.jwt)
	}
	for _, leak := range []string{"PRIVATE KEY", setupClientSecret, setupWebhookSecret} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("status leaked %q", leak)
		}
	}
}

func TestSetupWithoutUIToken(t *testing.T) {
	f := newSetupFixture(t, "", nil)
	state, _ := f.start()
	if w := f.callback("good-code", state); w.Code != 200 {
		t.Fatalf("callback: %d", w.Code)
	}
}
