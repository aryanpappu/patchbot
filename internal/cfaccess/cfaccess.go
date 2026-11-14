// Package cfaccess verifies Cloudflare Access application tokens
// (Cf-Access-Jwt-Assertion) with the standard library only. It accepts RS256
// signatures from the team JWKS and nothing else.
package cfaccess

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Header is the request header Cloudflare Access adds after login.
const Header = "Cf-Access-Jwt-Assertion"

const (
	skew          = 60 * time.Second
	refreshEvery  = time.Minute
	failBackoff   = 15 * time.Second
	keyMaxAge     = time.Hour
	keyHardStale  = 24 * time.Hour
	maxTokenBytes = 16 << 10
	maxJWKSBytes  = 1 << 20
	minRSABits    = 2048
)

// Errors are deliberately coarse; callers must not echo token content.
var (
	// ErrInvalid covers every malformed, unsigned, expired or mis-scoped token.
	ErrInvalid = errors.New("invalid access token")
	// ErrNotAllowed means a valid token names an email that is not allowed.
	ErrNotAllowed = errors.New("email not allowed")
)

// Config describes one Access application.
type Config struct {
	TeamDomain    string
	Audience      string
	AllowedEmails []string
	// CertsURL overrides https://<team>/cdn-cgi/access/certs (tests).
	CertsURL string
	Client   *http.Client
	Now      func() time.Time
}

// Verifier checks tokens and caches the team signing keys by kid.
type Verifier struct {
	issuer   string
	audience string
	allowed  map[string]bool
	certs    string
	client   *http.Client
	now      func() time.Time

	fetchMu   sync.Mutex
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time // last successful fetch
	failedAt  time.Time // last failed fetch
}

// New builds a verifier. It performs no network I/O.
func New(c Config) (*Verifier, error) {
	if c.TeamDomain == "" || c.Audience == "" || len(c.AllowedEmails) == 0 {
		return nil, errors.New("team domain, audience and allowed emails are required")
	}
	v := &Verifier{issuer: "https://" + c.TeamDomain, audience: c.Audience, allowed: map[string]bool{}, certs: c.CertsURL, client: c.Client, now: c.Now}
	if v.certs == "" {
		v.certs = "https://" + c.TeamDomain + "/cdn-cgi/access/certs"
	}
	if v.now == nil {
		v.now = time.Now
	}
	if v.client == nil {
		v.client = &http.Client{Timeout: 5 * time.Second}
	}
	// Redirects could move the key fetch to another origin.
	client := *v.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	v.client = &client
	for _, e := range c.AllowedEmails {
		v.allowed[strings.ToLower(strings.TrimSpace(e))] = true
	}
	return v, nil
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type claims struct {
	Iss   string          `json:"iss"`
	Aud   json.RawMessage `json:"aud"`
	Exp   *float64        `json:"exp"`
	Nbf   *float64        `json:"nbf"`
	Iat   *float64        `json:"iat"`
	Email string          `json:"email"`
}

// Verify returns the lower-cased email of a valid, allowed token.
func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	if token == "" || len(token) > maxTokenBytes {
		return "", ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrInvalid
	}
	var h header
	if decodeSegment(parts[0], &h) != nil || h.Alg != "RS256" || h.Kid == "" {
		return "", ErrInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) == 0 {
		return "", ErrInvalid
	}
	key, err := v.key(ctx, h.Kid)
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return "", ErrInvalid
	}
	var c claims
	if decodeSegment(parts[1], &c) != nil || c.Iss != v.issuer || !audienceMatches(c.Aud, v.audience) || c.Exp == nil || c.Iat == nil {
		return "", ErrInvalid
	}
	now := v.now()
	if now.After(unix(*c.Exp).Add(skew)) || now.Add(skew).Before(unix(*c.Iat)) || (c.Nbf != nil && now.Add(skew).Before(unix(*c.Nbf))) {
		return "", ErrInvalid
	}
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if email == "" || len(email) > 320 || strings.ContainsFunc(email, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		return "", ErrInvalid
	}
	if !v.allowed[email] {
		return "", ErrNotAllowed
	}
	return email, nil
}

func unix(f float64) time.Time { return time.Unix(int64(f), 0) }

func decodeSegment(s string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func audienceMatches(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// key returns the signing key for kid.
//
//   - A key set younger than keyMaxAge serves known kids directly.
//   - An older set is refreshed on demand; if that fails the old keys keep
//     working until keyHardStale, after which verification fails closed.
//   - An unknown kid refreshes at most once per refreshEvery after the last
//     successful fetch, and failed fetches back off for failBackoff, so forged
//     kids cannot hammer the JWKS URL.
//
// The fetch is shared and runs under its own bounded context, so a client that
// cancels its request can neither abort it nor consume the cooldown.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if k, fresh := v.lookup(kid); k != nil && fresh {
		return k, nil
	}
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	// Another request may have refreshed while this one waited.
	k, fresh := v.lookup(kid)
	if k != nil && fresh {
		return k, nil
	}
	v.mu.Lock()
	now := v.now()
	age := now.Sub(v.fetchedAt)
	havekeys := !v.fetchedAt.IsZero()
	// A known kid in a set past keyMaxAge is due; an unknown kid is due once
	// the success cooldown has passed. Both wait out a recent failure.
	due := !havekeys || (k != nil && age >= keyMaxAge) || (k == nil && age >= refreshEvery)
	backoff := !v.failedAt.IsZero() && now.Sub(v.failedAt) < failBackoff
	v.mu.Unlock()
	if !due || backoff {
		if k != nil {
			return k, nil // stale but within the hard limit (lookup enforces it)
		}
		return nil, errors.New("unknown key")
	}
	keys, err := v.fetch(context.WithoutCancel(ctx))
	v.mu.Lock()
	if err != nil {
		v.failedAt = v.now()
		v.mu.Unlock()
		if k != nil {
			return k, nil
		}
		return nil, err
	}
	v.keys, v.fetchedAt, v.failedAt = keys, v.now(), time.Time{}
	k = keys[kid]
	v.mu.Unlock()
	if k == nil {
		return nil, errors.New("unknown key")
	}
	return k, nil
}

// lookup returns the cached key and whether the set is within keyMaxAge. A
// set older than keyHardStale yields no key at all.
func (v *Verifier) lookup(kid string) (*rsa.PublicKey, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := v.now().Sub(v.fetchedAt)
	if v.fetchedAt.IsZero() || age >= keyHardStale {
		return nil, false
	}
	return v.keys[kid], age < keyMaxAge
}

// Check fetches the JWKS once and reports how many RSA keys it holds.
func (v *Verifier) Check(ctx context.Context) (int, error) {
	keys, err := v.fetch(ctx)
	return len(keys), err
}

type jwks struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certs, nil)
	if err != nil {
		return nil, errors.New("invalid JWKS URL")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, errors.New("JWKS request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS request returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil || len(body) > maxJWKSBytes {
		return nil, errors.New("JWKS body unreadable or too large")
	}
	var set jwks
	if json.Unmarshal(body, &set) != nil {
		return nil, errors.New("JWKS is not valid JSON")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := new(big.Int).SetBytes(e)
		mod := new(big.Int).SetBytes(n)
		if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64()%2 == 0 || mod.BitLen() < minRSABits {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: mod, E: int(exp.Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS has no usable RSA keys")
	}
	return keys, nil
}
