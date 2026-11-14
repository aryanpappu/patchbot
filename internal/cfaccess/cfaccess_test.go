package cfaccess

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	team = "crimson-scene-dba4.cloudflareaccess.com"
	aud  = "0123456789abcdef0123456789abcdef"
)

var epoch = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type jwksServer struct {
	*httptest.Server
	mu    sync.Mutex
	keys  map[string]*rsa.PrivateKey
	fetch atomic.Int32
}

func newJWKS(t *testing.T, keys map[string]*rsa.PrivateKey) *jwksServer {
	s := &jwksServer{keys: keys}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.fetch.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		type jwk struct{ Kty, Kid, Use, Alg, N, E string }
		var out struct {
			Keys []jwk `json:"keys"`
		}
		for kid, k := range s.keys {
			out.Keys = append(out.Keys, jwk{"RSA", kid, "sig", "RS256", b64(k.N.Bytes()), b64(big.NewInt(int64(k.E)).Bytes())})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": lowerKeys(out.Keys)})
	}))
	t.Cleanup(s.Close)
	return s
}

// lowerKeys renders JWK field names in lower case as JWKS requires.
func lowerKeys[T any](in []T) []map[string]string {
	var out []map[string]string
	for _, k := range in {
		raw, _ := json.Marshal(k)
		var m map[string]string
		_ = json.Unmarshal(raw, &m)
		l := map[string]string{}
		for key, v := range m {
			l[strings.ToLower(key)] = v
		}
		out = append(out, l)
	}
	return out
}

func (s *jwksServer) set(keys map[string]*rsa.PrivateKey) {
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func claimsAt(now time.Time) map[string]any {
	return map[string]any{"iss": "https://" + team, "aud": []string{aud}, "email": "Owner@Example.com",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix()}
}

func sign(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(sig)
}

func rs(kid string) map[string]any { return map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"} }

func setup(t *testing.T) (*Verifier, *jwksServer, *clock, *rsa.PrivateKey) {
	t.Helper()
	key := newKey(t)
	srv := newJWKS(t, map[string]*rsa.PrivateKey{"k1": key})
	clk := &clock{t: epoch}
	v, err := New(Config{TeamDomain: team, Audience: aud, AllowedEmails: []string{"owner@example.com"}, CertsURL: srv.URL, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return v, srv, clk, key
}

func TestVerifyValid(t *testing.T) {
	v, _, clk, key := setup(t)
	email, err := v.Verify(t.Context(), sign(t, key, rs("k1"), claimsAt(clk.now())))
	if err != nil || email != "owner@example.com" {
		t.Fatalf("email %q err %v", email, err)
	}
	c := claimsAt(clk.now())
	c["aud"] = aud // string form
	if _, err := v.Verify(t.Context(), sign(t, key, rs("k1"), c)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejects(t *testing.T) {
	v, _, clk, key := setup(t)
	other := newKey(t)
	now := clk.now()
	mod := func(f func(map[string]any)) string {
		c := claimsAt(now)
		f(c)
		return sign(t, key, rs("k1"), c)
	}
	hmacTok := func() string {
		h, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "k1"})
		c, _ := json.Marshal(claimsAt(now))
		in := b64(h) + "." + b64(c)
		m := hmac.New(sha256.New, []byte("secret"))
		m.Write([]byte(in))
		return in + "." + b64(m.Sum(nil))
	}()
	noneH, _ := json.Marshal(map[string]any{"alg": "none", "kid": "k1"})
	noneC, _ := json.Marshal(claimsAt(now))
	cases := map[string]string{
		"empty":         "",
		"garbage":       "a.b",
		"bad signature": sign(t, other, rs("k1"), claimsAt(now)),
		"wrong aud":     mod(func(c map[string]any) { c["aud"] = []string{"other"} }),
		"wrong iss":     mod(func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" }),
		"expired":       mod(func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() }),
		"not yet valid": mod(func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() }),
		"future iat":    mod(func(c map[string]any) { c["iat"] = now.Add(5 * time.Minute).Unix() }),
		"no exp":        mod(func(c map[string]any) { delete(c, "exp") }),
		"no email":      mod(func(c map[string]any) { delete(c, "email") }),
		"alg none":      b64(noneH) + "." + b64(noneC) + ".",
		"alg HS256":     hmacTok,
		"unknown kid":   sign(t, key, rs("zzz"), claimsAt(now)),
		"missing kid":   sign(t, key, map[string]any{"alg": "RS256"}, claimsAt(now)),
	}
	for name, token := range cases {
		if _, err := v.Verify(t.Context(), token); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
	if _, err := v.Verify(t.Context(), mod(func(c map[string]any) { c["email"] = "intruder@example.com" })); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("email not allowed: %v", err)
	}
}

func TestSkewAllowsSmallDrift(t *testing.T) {
	v, _, clk, key := setup(t)
	c := claimsAt(clk.now())
	c["nbf"] = clk.now().Add(30 * time.Second).Unix()
	c["exp"] = clk.now().Add(-30 * time.Second).Unix()
	if _, err := v.Verify(t.Context(), sign(t, key, rs("k1"), c)); err != nil {
		t.Fatalf("30s drift must be tolerated: %v", err)
	}
}

func TestUnknownKidRefetchesAtMostOncePerMinute(t *testing.T) {
	v, srv, clk, key := setup(t)
	if _, err := v.Verify(t.Context(), sign(t, key, rs("k1"), claimsAt(clk.now()))); err != nil {
		t.Fatal(err)
	}
	if srv.fetch.Load() != 1 {
		t.Fatalf("fetches %d", srv.fetch.Load())
	}
	bad := sign(t, key, rs("nope"), claimsAt(clk.now()))
	clk.add(2 * time.Minute)
	for range 5 {
		_, _ = v.Verify(t.Context(), bad)
	}
	if srv.fetch.Load() != 2 {
		t.Fatalf("unknown kid must refetch exactly once within a minute, fetches %d", srv.fetch.Load())
	}
	clk.add(61 * time.Second)
	_, _ = v.Verify(t.Context(), bad)
	if srv.fetch.Load() != 3 {
		t.Fatalf("expected a refetch after the minute, fetches %d", srv.fetch.Load())
	}
}

func TestKeyRotation(t *testing.T) {
	v, srv, clk, key := setup(t)
	if _, err := v.Verify(t.Context(), sign(t, key, rs("k1"), claimsAt(clk.now()))); err != nil {
		t.Fatal(err)
	}
	fresh := newKey(t)
	srv.set(map[string]*rsa.PrivateKey{"k2": fresh})
	clk.add(2 * time.Minute)
	if _, err := v.Verify(t.Context(), sign(t, fresh, rs("k2"), claimsAt(clk.now()))); err != nil {
		t.Fatalf("rotated key rejected: %v", err)
	}
	// The retired key no longer verifies once the cache has been replaced.
	if _, err := v.Verify(t.Context(), sign(t, key, rs("k1"), claimsAt(clk.now()))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retired key accepted: %v", err)
	}
}

func TestJWKSFailures(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status":   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) },
		"not json": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"no keys": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"keys":[{"kty":"EC","kid":"x"}]}`))
		},
		"too big": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat(" ", maxJWKSBytes+10)))
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
		},
	} {
		srv := httptest.NewServer(handler)
		v, err := New(Config{TeamDomain: team, Audience: aud, AllowedEmails: []string{"a@b.co"}, CertsURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Check(context.Background()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		srv.Close()
	}
}

func TestCheckCountsKeys(t *testing.T) {
	v, _, _, _ := setup(t)
	if n, err := v.Check(t.Context()); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestRetiredKeyRejectedAfterMaxAge(t *testing.T) {
	v, srv, clk, key := setup(t)
	token := func() string { return sign(t, key, rs("k1"), claimsAt(clk.now())) }
	if _, err := v.Verify(t.Context(), token()); err != nil {
		t.Fatal(err)
	}
	// k1 is retired upstream; within the max age the cache still serves it.
	fresh := newKey(t)
	srv.set(map[string]*rsa.PrivateKey{"k2": fresh})
	clk.add(30 * time.Minute)
	if _, err := v.Verify(t.Context(), token()); err != nil {
		t.Fatalf("within max age: %v", err)
	}
	clk.add(31 * time.Minute)
	if _, err := v.Verify(t.Context(), token()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retired key accepted after max age: %v", err)
	}
}

func TestStaleKeysFailClosedWhenRefreshFails(t *testing.T) {
	v, srv, clk, key := setup(t)
	token := func() string { return sign(t, key, rs("k1"), claimsAt(clk.now())) }
	if _, err := v.Verify(t.Context(), token()); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	clk.add(2 * time.Hour)
	if _, err := v.Verify(t.Context(), token()); err != nil {
		t.Fatalf("stale but under the hard limit should still verify: %v", err)
	}
	clk.add(23 * time.Hour)
	if _, err := v.Verify(t.Context(), token()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("keys past the hard stale limit accepted: %v", err)
	}
}

func TestCancelledRequestDoesNotBlockRefresh(t *testing.T) {
	v, srv, clk, key := setup(t)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = v.Verify(cancelled, sign(t, key, rs("zzz"), claimsAt(clk.now())))
	// The next legitimate request must not wait out any cooldown.
	if _, err := v.Verify(t.Context(), sign(t, key, rs("k1"), claimsAt(clk.now()))); err != nil {
		t.Fatalf("legitimate request blocked after a cancelled one: %v", err)
	}
	if srv.fetch.Load() > 2 {
		t.Fatalf("fetches %d", srv.fetch.Load())
	}
}
