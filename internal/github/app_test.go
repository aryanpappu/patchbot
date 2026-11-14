package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

func appTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		var err error
		testKey, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
	})
	return testKey
}

// fakeApp is a fake GitHub App API. Every request's JWT is verified with the
// public key; minted tokens are recorded with the repository they cover.
type fakeApp struct {
	t   *testing.T
	key *rsa.PublicKey
	mu  sync.Mutex
	// repoIDs maps "owner/name" to an immutable ID.
	repoIDs map[string]int64
	// lifetime of minted tokens; zero is one hour.
	lifetime time.Duration
	// mintStatus, when set, is returned for run-token mints (revocation).
	mintStatus int
	// rateLimited counts down 429 responses before success.
	rateLimited int
	retryAfter  string
	mints       []mintRecord
	minted      map[string]int64 // token -> repo ID
	rejected    map[string]bool  // tokens the API now refuses (401)
	counter     int
	delay       time.Duration
	userAuth    string
	rejectAll   bool
	// mutateRun / mutateLookup rewrite a successful run-token / ID-lookup
	// mint response (scope verification tests).
	mutateRun, mutateLookup func(map[string]any)
}

type mintRecord struct {
	RepositoryIDs []int64           `json:"repository_ids"`
	Repositories  []string          `json:"repositories"`
	Permissions   map[string]string `json:"permissions"`
	Token         string            `json:"-"`
}

func newFakeApp(t *testing.T) (*fakeApp, *httptest.Server) {
	t.Helper()
	f := &fakeApp{t: t, key: &appTestKey(t).PublicKey, repoIDs: map[string]int64{"owner/repo": 101, "owner/other": 202},
		minted: map[string]int64{}, rejected: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeApp) verifyJWT(req *http.Request) bool {
	token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return verifyTestJWT(f.key, token) == nil
}

func verifyTestJWT(key *rsa.PublicKey, token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig)
}

func (f *fakeApp) serve(w http.ResponseWriter, req *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	path := req.URL.Path
	switch {
	case path == "/app":
		if !f.verifyJWT(req) {
			reply(401, map[string]string{"message": "bad JWT"})
			return
		}
		reply(200, map[string]any{"slug": "patchbot", "id": 9})
	case path == "/users/patchbot[bot]":
		f.mu.Lock()
		f.userAuth = req.Header.Get("Authorization")
		f.mu.Unlock()
		reply(200, map[string]any{"id": 4242, "login": "patchbot[bot]"})
	case strings.HasSuffix(path, "/installation") && strings.HasPrefix(path, "/repos/"):
		if !f.verifyJWT(req) {
			reply(401, map[string]string{"message": "bad JWT"})
			return
		}
		full := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/installation")
		if _, ok := f.repoIDs[full]; !ok {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		reply(200, map[string]any{"id": 77, "permissions": RunPermissions})
	case path == "/app/installations/77/access_tokens" && req.Method == http.MethodPost:
		if !f.verifyJWT(req) {
			reply(401, map[string]string{"message": "bad JWT"})
			return
		}
		var in mintRecord
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			reply(400, nil)
			return
		}
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.rateLimited > 0 {
			f.rateLimited--
			w.Header().Set("Retry-After", f.retryAfter)
			reply(429, map[string]string{"message": "API rate limit exceeded"})
			return
		}
		if f.mintStatus != 0 && len(in.RepositoryIDs) > 0 {
			reply(f.mintStatus, map[string]string{"message": "installation suspended"})
			return
		}
		f.counter++
		token := fmt.Sprintf("ghs_fakeInstallationToken%020d", f.counter)
		in.Token = token
		f.mints = append(f.mints, in)
		lifetime := f.lifetime
		if lifetime == 0 {
			lifetime = time.Hour
		}
		var repos []map[string]any
		for _, name := range in.Repositories {
			id := f.repoIDs["owner/"+name]
			repos = append(repos, map[string]any{"id": id, "full_name": "owner/" + name})
			f.minted[token] = id
		}
		for _, id := range in.RepositoryIDs {
			for full, known := range f.repoIDs {
				if known == id {
					repos = append(repos, map[string]any{"id": id, "full_name": full})
				}
			}
			f.minted[token] = id
		}
		resp := map[string]any{"token": token, "expires_at": time.Now().Add(lifetime).UTC().Format(time.RFC3339),
			"permissions": in.Permissions, "repositories": repos}
		if len(in.RepositoryIDs) > 0 && f.mutateRun != nil {
			f.mutateRun(resp)
		}
		if len(in.Repositories) > 0 && f.mutateLookup != nil {
			f.mutateLookup(resp)
		}
		reply(201, resp)
	case strings.HasPrefix(path, "/repos/"):
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		full := strings.Join(strings.SplitN(strings.TrimPrefix(path, "/repos/"), "/", 3)[:2], "/")
		f.mu.Lock()
		id, ok := f.minted[token]
		rejected := f.rejected[token] || f.rejectAll
		f.mu.Unlock()
		if !ok || rejected || id != f.repoIDs[full] {
			reply(401, map[string]string{"message": "Bad credentials " + token})
			return
		}
		reply(200, map[string]any{"number": 1, "title": "ok", "state": "open"})
	default:
		f.t.Errorf("unexpected %s %s", req.Method, path)
		reply(404, nil)
	}
}

func (f *fakeApp) runMints() []mintRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mintRecord
	for _, m := range f.mints {
		if len(m.RepositoryIDs) > 0 {
			out = append(out, m)
		}
	}
	return out
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestApp(t *testing.T, server *httptest.Server, opts AppOptions) *App {
	t.Helper()
	opts.AppID, opts.Key, opts.BaseURL, opts.HTTPClient = 123, appTestKey(t), server.URL, server.Client()
	app, err := NewApp(opts)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

var repoA, repoB = RepoRef{Owner: "owner", Name: "repo"}, RepoRef{Owner: "owner", Name: "other"}

func TestSignAppJWTClaimsAndSignature(t *testing.T) {
	t.Parallel()
	key := appTestKey(t)
	now := time.Unix(1_800_000_000, 0)
	token, err := SignAppJWT(key, 123, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyTestJWT(&key.PublicKey, token); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	parts := strings.Split(token, ".")
	var header map[string]string
	var claims struct {
		IAT, EXP int64
		ISS      string
	}
	for i, out := range []any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(raw, out) != nil {
			t.Fatalf("decode part %d: %v", i, err)
		}
	}
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %v", header)
	}
	if claims.IAT != now.Unix()-60 || claims.EXP != now.Add(9*time.Minute).Unix() || claims.ISS != "123" {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.EXP-now.Unix() > 9*60 {
		t.Fatal("exp beyond now+9m")
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if verifyTestJWT(&other.PublicKey, token) == nil {
		t.Fatal("JWT verified with an unrelated key")
	}
}

func writeKey(t *testing.T, dir, name string, block *pem.Block, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppPrivateKeyFormatsAndFileSafety(t *testing.T) {
	t.Parallel()
	key := appTestKey(t)
	dir := t.TempDir()
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := writeKey(t, dir, "pkcs1.pem", &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}, 0o600)
	p8 := writeKey(t, dir, "pkcs8.pem", &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}, 0o400)
	for _, path := range []string{pkcs1, p8} {
		loaded, err := LoadAppPrivateKey(path)
		if err != nil || !loaded.Equal(key) {
			t.Fatalf("load %s: %v", filepath.Base(path), err)
		}
	}
	open := writeKey(t, dir, "open.pem", &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}, 0o640)
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(pkcs1, link); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := writeKey(t, data, "key.pem", &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}, 0o600)
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"mode 0640": open, "symlink": link, "inside data dir": inside, "relative": "key.pem", "not PEM": garbage, "dir": dir}
	for name, path := range cases {
		_, err := LoadAppPrivateKey(path, data)
		if err == nil {
			t.Fatalf("%s: loaded", name)
		}
		if strings.Contains(err.Error(), "MII") || strings.Contains(err.Error(), "BEGIN") {
			t.Fatalf("%s: error quotes key material: %v", name, err)
		}
	}
}

func TestRunTokenHasExactlyOneRepoIDAndFourPermissions(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	var registered []string
	var mu sync.Mutex
	app := newTestApp(t, server, AppOptions{OnMint: func(token string) {
		mu.Lock()
		defer mu.Unlock()
		registered = append(registered, token)
	}})
	token, err := app.RepoToken(t.Context(), repoA)
	if err != nil {
		t.Fatal(err)
	}
	mints := fake.runMints()
	if len(mints) != 1 {
		t.Fatalf("run mints = %d", len(mints))
	}
	m := mints[0]
	if len(m.RepositoryIDs) != 1 || m.RepositoryIDs[0] != 101 || len(m.Repositories) != 0 {
		t.Fatalf("mint scope = %+v", m)
	}
	want := map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read"}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions = %v", m.Permissions)
	}
	for k, v := range want {
		if m.Permissions[k] != v {
			t.Fatalf("permissions = %v", m.Permissions)
		}
	}
	// The ID lookup mint is metadata-only and narrowed to the repository name.
	fake.mu.Lock()
	first := fake.mints[0]
	fake.mu.Unlock()
	if len(first.Repositories) != 1 || first.Repositories[0] != "repo" || len(first.Permissions) != 1 || first.Permissions["metadata"] != "read" {
		t.Fatalf("ID lookup mint = %+v", first)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(registered) != 2 || registered[1] != token {
		t.Fatalf("OnMint saw %d tokens; every minted token must be registered", len(registered))
	}
}

func TestTokenCacheReuseAndRefreshNearExpiry(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	clock := &fakeClock{now: time.Now()}
	app := newTestApp(t, server, AppOptions{Clock: clock.Now})
	first, err := app.RepoToken(t.Context(), repoA)
	if err != nil {
		t.Fatal(err)
	}
	again, err := app.RepoToken(t.Context(), repoA)
	if err != nil || again != first || len(fake.runMints()) != 1 {
		t.Fatalf("cache not reused: same=%v mints=%d err=%v", again == first, len(fake.runMints()), err)
	}
	clock.Add(54 * time.Minute) // 6 minutes left: still cached
	if tok, _ := app.RepoToken(t.Context(), repoA); tok != first {
		t.Fatal("refreshed too early")
	}
	clock.Add(2 * time.Minute) // 4 minutes left: refresh
	fresh, err := app.RepoToken(t.Context(), repoA)
	if err != nil || fresh == first || len(fake.runMints()) != 2 {
		t.Fatalf("no refresh near expiry: mints=%d err=%v", len(fake.runMints()), err)
	}
}

func TestConcurrentMintsAreCoalesced(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	fake.delay = 50 * time.Millisecond
	app := newTestApp(t, server, AppOptions{})
	var wg sync.WaitGroup
	tokens := make([]string, 32)
	errs := make([]error, 32)
	for i := range tokens {
		wg.Go(func() { tokens[i], errs[i] = app.RepoToken(context.Background(), repoA) })
	}
	wg.Wait()
	for i := range tokens {
		if errs[i] != nil || tokens[i] != tokens[0] {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.mints) != 2 { // one ID lookup + one run token
		t.Fatalf("mints = %d, want 2", len(fake.mints))
	}
}

func TestConcurrentReposNeverShareATokenAndIsolationHolds(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	app := newTestApp(t, server, AppOptions{})
	client, err := NewWithRepoTokens(server.Client(), server.URL, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := map[RepoRef]string{}
	var mu sync.Mutex
	for range 8 {
		for _, repo := range []RepoRef{repoA, repoB} {
			wg.Go(func() {
				token, err := app.RepoToken(context.Background(), repo)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := client.GetIssue(context.Background(), repo, 1); err != nil {
					t.Errorf("GetIssue %s: %v", repo.Name, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if prev, ok := got[repo]; ok && prev != token {
					t.Errorf("repo %s got two tokens", repo.Name)
				}
				got[repo] = token
			})
		}
	}
	wg.Wait()
	if got[repoA] == "" || got[repoA] == got[repoB] {
		t.Fatal("two repositories shared a token")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.minted[got[repoA]] != 101 || fake.minted[got[repoB]] != 202 {
		t.Fatal("token minted for the wrong repository")
	}
	// The fake refuses a token used for another repository (401), so a
	// cross-repository token can never succeed.
	pinned := got[repoA]
	cross, err := New(server.Client(), server.URL, tokenFunc(func(context.Context) (string, error) { return pinned, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cross.GetIssue(context.Background(), repoB, 1); err == nil {
		t.Fatal("repo A's token was accepted for repo B")
	}
}

func TestUnauthorizedInvalidatesAndRemintsOnce(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	app := newTestApp(t, server, AppOptions{})
	client, err := NewWithRepoTokens(server.Client(), server.URL, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := app.RepoToken(t.Context(), repoA)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.rejected[first] = true // revoked early on GitHub's side
	fake.mu.Unlock()
	if _, err := client.GetIssue(t.Context(), repoA, 1); err != nil {
		t.Fatalf("GetIssue after 401: %v", err)
	}
	if n := len(fake.runMints()); n != 2 {
		t.Fatalf("run mints = %d, want 2 (one re-mint)", n)
	}
	// A token GitHub keeps refusing is re-minted once, not endlessly.
	fake.mu.Lock()
	fake.rejectAll = true
	fake.mu.Unlock()
	_, err = client.GetIssue(t.Context(), repoA, 1)
	var unauthorized *ErrUnauthorized
	if !errors.As(err, &unauthorized) {
		t.Fatalf("persistent 401 = %v", err)
	}
	if n := len(fake.runMints()); n != 3 {
		t.Fatalf("run mints = %d, want exactly one more re-mint", n)
	}
	if strings.Contains(err.Error(), "ghs_") {
		t.Fatalf("error quotes a token: %v", err)
	}
}

func TestRevokedInstallationFailsClosed(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	fake.mintStatus = http.StatusNotFound
	app := newTestApp(t, server, AppOptions{})
	_, err := app.RepoToken(t.Context(), repoA)
	var mint *MintError
	if !errors.As(err, &mint) || mint.Kind != "denied" {
		t.Fatalf("revoked installation: %v", err)
	}
	client, _ := NewWithRepoTokens(server.Client(), server.URL, app, tokenFunc(func(context.Context) (string, error) {
		return "ghp_shouldNeverBeUsedAsFallback000000", nil
	}))
	if _, err := client.GetIssue(t.Context(), repoA, 1); err == nil || !errors.As(err, &mint) {
		t.Fatalf("GetIssue with a revoked installation = %v; want a closed failure", err)
	}
}

func TestRateLimitBackoffHonoursRetryAfterAndCancellation(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	var waits []time.Duration
	var mu sync.Mutex
	app := newTestApp(t, server, AppOptions{Sleep: func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}})
	fake.mu.Lock()
	fake.rateLimited, fake.retryAfter = 1, "3"
	fake.mu.Unlock()
	if _, err := app.RepoToken(t.Context(), repoA); err != nil {
		t.Fatalf("after one rate limit: %v", err)
	}
	mu.Lock()
	if len(waits) != 1 || waits[0] < 2*time.Second || waits[0] > 5*time.Second {
		t.Fatalf("waits = %v; want ~Retry-After", waits)
	}
	mu.Unlock()

	// A long reset is not waited for inline.
	fake.mu.Lock()
	fake.rateLimited, fake.retryAfter = 1, "3600"
	fake.mu.Unlock()
	_, err := app.RepoToken(t.Context(), repoB)
	var mint *MintError
	if !errors.As(err, &mint) || mint.Kind != "rate_limited" {
		t.Fatalf("long reset: %v", err)
	}

	// Cancellation interrupts the backoff.
	blocking := newTestApp(t, server, AppOptions{Sleep: sleepContext})
	fake.mu.Lock()
	fake.rateLimited, fake.retryAfter = 10, "60"
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err = blocking.RepoToken(ctx, RepoRef{Owner: "owner", Name: "repo"})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("cancelled backoff = %v after %v", err, time.Since(start))
	}
}

func TestAppIdentityAndCommitEmail(t *testing.T) {
	t.Parallel()
	fake, server := newFakeApp(t)
	app := newTestApp(t, server, AppOptions{})
	identity, err := app.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if identity.Login != "patchbot[bot]" || identity.UserID != 4242 ||
		identity.CommitEmail() != "4242+patchbot[bot]@users.noreply.github.com" {
		t.Fatalf("identity = %+v", identity)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.userAuth != "" {
		t.Fatal("public user lookup sent a credential")
	}
}

func TestAppBotCommentsNeverEligible(t *testing.T) {
	t.Parallel()
	now := time.Now()
	collab := func(string) bool { return true }
	never := func(int64) bool { return false }
	bot := Comment{ID: 5, Author: "patchbot[bot]", Body: "/approve", CreatedAt: now, UpdatedAt: now}
	if EligibleCommand(bot, []string{"owner", "patchbot[bot]"}, collab, "patchbot[bot]", never) {
		t.Fatal("App bot comment counted as a command")
	}
	owner := Comment{ID: 6, Author: "owner", Body: "/approve", CreatedAt: now, UpdatedAt: now}
	if !EligibleCommand(owner, []string{"owner"}, collab, "patchbot[bot]", never) {
		t.Fatal("owner approval rejected with a distinct bot identity")
	}
}

func TestMintedTokenNotInErrors(t *testing.T) {
	t.Parallel()
	var echoed atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		echoed.Store(token)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom ` + token + `"}`)) // #nosec G705 -- test server echoes a credential on purpose.
	}))
	t.Cleanup(server.Close)
	app := newTestApp(t, server, AppOptions{Sleep: func(context.Context, time.Duration) error { return nil }})
	_, err := app.RepoToken(t.Context(), repoA)
	if err == nil {
		t.Fatal("expected failure")
	}
	jwt, _ := echoed.Load().(string)
	if jwt == "" || strings.Contains(err.Error(), jwt) || strings.Contains(err.Error(), strings.Split(jwt, ".")[2]) {
		t.Fatalf("error quotes the JWT: %v", err)
	}
}

// TestMintedScopeMustBeVerified: a mint response whose permissions or
// repositories are missing, null, empty or different from the request fails
// closed and is never cached, for both the run token and the ID lookup token.
func TestMintedScopeMustBeVerified(t *testing.T) {
	t.Parallel()
	run := map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range run {
			out[k] = v
		}
		for k, v := range extra {
			if v == "" {
				delete(out, k)
			} else {
				out[k] = v
			}
		}
		return out
	}
	runCases := map[string]func(map[string]any){
		"missing permissions":  func(r map[string]any) { delete(r, "permissions") },
		"null permissions":     func(r map[string]any) { r["permissions"] = nil },
		"empty permissions":    func(r map[string]any) { r["permissions"] = map[string]string{} },
		"extra permission":     func(r map[string]any) { r["permissions"] = with(map[string]string{"administration": "write"}) },
		"wrong level":          func(r map[string]any) { r["permissions"] = with(map[string]string{"metadata": "write"}) },
		"missing permission":   func(r map[string]any) { r["permissions"] = with(map[string]string{"issues": ""}) },
		"missing repositories": func(r map[string]any) { delete(r, "repositories") },
		"null repositories":    func(r map[string]any) { r["repositories"] = nil },
		"empty repositories":   func(r map[string]any) { r["repositories"] = []any{} },
		"extra repository": func(r map[string]any) {
			r["repositories"] = append(r["repositories"].([]map[string]any), map[string]any{"id": 202, "full_name": "owner/other"})
		},
		"wrong repository": func(r map[string]any) { r["repositories"] = []map[string]any{{"id": 202, "full_name": "owner/other"}} },
	}
	lookupCases := map[string]func(map[string]any){
		"missing permissions":  func(r map[string]any) { delete(r, "permissions") },
		"null permissions":     func(r map[string]any) { r["permissions"] = nil },
		"extra permission":     func(r map[string]any) { r["permissions"] = map[string]string{"metadata": "read", "contents": "read"} },
		"wrong level":          func(r map[string]any) { r["permissions"] = map[string]string{"metadata": "write"} },
		"missing repositories": func(r map[string]any) { delete(r, "repositories") },
		"null repositories":    func(r map[string]any) { r["repositories"] = nil },
		"empty repositories":   func(r map[string]any) { r["repositories"] = []any{} },
		"extra repository": func(r map[string]any) {
			r["repositories"] = append(r["repositories"].([]map[string]any), map[string]any{"id": 202, "full_name": "owner/other"})
		},
		"wrong repository":   func(r map[string]any) { r["repositories"] = []map[string]any{{"id": 202, "full_name": "owner/other"}} },
		"zero repository id": func(r map[string]any) { r["repositories"] = []map[string]any{{"id": 0, "full_name": "owner/repo"}} },
	}
	check := func(t *testing.T, fake *fakeApp, app *App) {
		t.Helper()
		token, err := app.RepoToken(t.Context(), repoA)
		var mint *MintError
		if token != "" || !errors.As(err, &mint) || mint.Kind != "denied" || strings.Contains(err.Error(), "ghs_") {
			t.Fatalf("unverified scope accepted: token=%q err=%v", token, err)
		}
		// Not cached: the next call mints again and still fails closed.
		fake.mu.Lock()
		before := len(fake.mints)
		fake.mu.Unlock()
		if token, err := app.RepoToken(t.Context(), repoA); token != "" || err == nil {
			t.Fatal("rejected token was cached")
		}
		fake.mu.Lock()
		after := len(fake.mints)
		fake.mu.Unlock()
		if after <= before {
			t.Fatal("second call did not re-mint; a rejected result was cached")
		}
	}
	for name, mutate := range runCases {
		t.Run("run/"+name, func(t *testing.T) {
			t.Parallel()
			fake, server := newFakeApp(t)
			fake.mutateRun = mutate
			check(t, fake, newTestApp(t, server, AppOptions{}))
		})
	}
	for name, mutate := range lookupCases {
		t.Run("lookup/"+name, func(t *testing.T) {
			t.Parallel()
			fake, server := newFakeApp(t)
			fake.mutateLookup = mutate
			check(t, fake, newTestApp(t, server, AppOptions{}))
		})
	}
}
