package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/cfaccess"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/store"
)

const (
	accessTeam  = "crimson-scene-dba4.cloudflareaccess.com"
	accessAud   = "0123456789abcdef0123456789abcdef"
	accessOwner = "owner@example.com"
)

type accessFixture struct {
	*fixture
	key *rsa.PrivateKey
}

func newAccessFixture(t *testing.T, p *fake.Provider) *accessFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256", "n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	t.Cleanup(jwks.Close)
	verifier, err := cfaccess.New(cfaccess.Config{TeamDomain: accessTeam, Audience: accessAud, AllowedEmails: []string{accessOwner}, CertsURL: jwks.URL})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureConfig(t, p, func(o *Options) { o.Access = verifier }, func(c *config.Config) {
		c.Server.Auth = config.ServerAuth{Mode: config.AuthCloudflareAccess, TeamDomain: accessTeam, Audience: accessAud, AllowedEmails: []string{accessOwner}}
	})
	return &accessFixture{f, key}
}

func (a *accessFixture) token(mutate func(map[string]any)) string {
	a.t.Helper()
	now := time.Now()
	claims := map[string]any{"iss": "https://" + accessTeam, "aud": []string{accessAud}, "email": accessOwner, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	if mutate != nil {
		mutate(claims)
	}
	enc := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
	input := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		a.t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (a *accessFixture) do(method, path, body, jwt string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(method, "http://"+a.cfg.Server.Addr+path, strings.NewReader(body))
	if jwt != "" {
		r.Header.Set(cfaccess.Header, jwt)
	}
	w := httptest.NewRecorder()
	a.server.ServeHTTP(w, r)
	if jwt != "" && strings.Contains(w.Body.String(), jwt) {
		a.t.Fatal("JWT echoed in response")
	}
	return w
}

func TestAccessApproveRecordsEmailAsActor(t *testing.T) {
	a := newAccessFixture(t, fake.New("anthropic", specStep(), writeStep(), submitStep()))
	a.withRepo()
	jwt := a.token(nil)
	w := a.do("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"fix"}`, jwt)
	requireStatus(t, w, 202)
	run := a.responseRun(w)
	a.waitFor(run.ID, store.StatusAwaitingApproval)
	requireStatus(t, a.do("POST", fmt.Sprintf("/api/runs/%d/approve", run.ID), `{}`, jwt), 202)
	a.waitFor(run.ID, store.StatusSucceeded)
	approvals, err := a.db.ListApprovals(t.Context(), run.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Actor != accessOwner || approvals[0].Channel != "ui" {
		t.Fatalf("approval audit: %+v: %v", approvals, err)
	}
	actions, err := a.db.ListActions(t.Context(), run.ID)
	if err != nil || len(actions) == 0 {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Actor != accessOwner || action.Channel != "ui" {
			t.Fatalf("action audit: %+v", action)
		}
	}
}

func TestAccessRejectCancelMemoryUseEmail(t *testing.T) {
	a := newAccessFixture(t, nil)
	jwt := a.token(nil)
	run := a.seed(store.StatusAwaitingApproval)
	requireStatus(t, a.do("POST", fmt.Sprintf("/api/runs/%d/reject", run.ID), `{}`, jwt), 200)
	approvals, err := a.db.ListApprovals(t.Context(), run.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Actor != accessOwner {
		t.Fatalf("reject audit: %+v: %v", approvals, err)
	}
	w := a.do("POST", "/api/memory", `{"repo":"owner/repo","text":"Run gofmt before committing."}`, jwt)
	requireStatus(t, w, 201)
	var created struct{ Entry store.MemoryEntry }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, a.do("POST", fmt.Sprintf("/api/memory/%d/forget", created.Entry.ID), `{}`, jwt), 200)
	actions, err := a.db.ListMemoryActions(t.Context(), created.Entry.ID)
	if err != nil || len(actions) == 0 {
		t.Fatalf("no memory actions: %v", err)
	}
	for _, m := range actions {
		if m.Actor != accessOwner {
			t.Fatalf("memory actor %q", m.Actor)
		}
	}
}

func TestAccessRejectsBadCredentials(t *testing.T) {
	a := newAccessFixture(t, nil)
	cases := map[string]struct {
		jwt    string
		status int
	}{
		"missing":     {"", 401},
		"garbage":     {"not.a.jwt", 401},
		"wrong aud":   {a.token(func(c map[string]any) { c["aud"] = []string{"x"} }), 401},
		"wrong iss":   {a.token(func(c map[string]any) { c["iss"] = "https://x.cloudflareaccess.com" }), 401},
		"expired":     {a.token(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }), 401},
		"not allowed": {a.token(func(c map[string]any) { c["email"] = "intruder@example.com" }), 403},
	}
	for name, tc := range cases {
		for _, path := range []string{"/api/runs", "/api/session", "/api/memory"} {
			requireStatus(t, a.do("GET", path, "", tc.jwt), tc.status)
		}
		w := a.do("POST", "/api/runs", `{}`, tc.jwt)
		requireStatus(t, w, tc.status)
		if strings.Contains(w.Body.String(), "intruder") || strings.Contains(strings.ToLower(w.Body.String()), "signature") {
			t.Errorf("%s: detail leaked: %s", name, w.Body.String())
		}
		page := a.do("GET", "/runs", "", tc.jwt)
		if page.Code != tc.status || strings.Contains(page.Body.String(), "<html") || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s: page %d %q", name, page.Code, page.Body.String())
		}
	}
	// Tamper with the signature of an otherwise valid token.
	valid := a.token(nil)
	tampered := valid[:len(valid)-4] + "AAAA"
	requireStatus(t, a.do("GET", "/api/runs", "", tampered), 401)
}

func TestAccessPublicAssetsAndSession(t *testing.T) {
	a := newAccessFixture(t, nil)
	if w := a.do("GET", "/assets/app.css", "", ""); w.Code != 200 {
		t.Fatalf("asset without JWT: %d", w.Code)
	}
	if w := a.do("GET", "/assets/other.js", "", ""); w.Code != 401 {
		t.Fatalf("unlisted asset path must need a JWT: %d", w.Code)
	}
	jwt := a.token(nil)
	w := a.do("GET", "/api/session", "", jwt)
	requireStatus(t, w, 200)
	var session struct{ Email, Mode string }
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil || session.Email != accessOwner || session.Mode != "cloudflare_access" {
		t.Fatalf("session %s", w.Body.String())
	}
	page := a.do("GET", "/runs", "", jwt)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Signed in as "+accessOwner) || strings.Contains(page.Body.String(), `"auth_required":true`) {
		t.Fatalf("page %d: %s", page.Code, page.Body.String())
	}
}

func TestAccessKeepsHostAndOriginProtection(t *testing.T) {
	a := newAccessFixture(t, nil)
	jwt := a.token(nil)
	r := httptest.NewRequest("GET", "http://evil.example/api/session", nil)
	r.Header.Set(cfaccess.Header, jwt)
	w := httptest.NewRecorder()
	a.server.ServeHTTP(w, r)
	requireStatus(t, w, 403)
	r = httptest.NewRequest("POST", "http://"+a.cfg.Server.Addr+"/api/runs", strings.NewReader(`{}`))
	r.Header.Set(cfaccess.Header, jwt)
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	a.server.ServeHTTP(w, r)
	requireStatus(t, w, 403)
}

func TestTokenModeSessionUnchanged(t *testing.T) {
	f := newFixture(t, nil, nil)
	w := f.request("GET", "/api/session", "")
	requireStatus(t, w, 200)
	var session struct{ Email, Mode string }
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil || session.Email != "" || session.Mode != "token" {
		t.Fatalf("session %s", w.Body.String())
	}
	if page := f.request("GET", "/runs", ""); page.Code != 200 || strings.Contains(page.Body.String(), "Signed in as") {
		t.Fatalf("token mode page changed: %d", page.Code)
	}
}
