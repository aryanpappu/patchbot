package github

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/version"
)

const (
	// maxConversionBody bounds the manifest conversion response (a PEM key and a
	// few short fields are well under 16 KiB).
	maxConversionBody = 64 << 10
	// conversionTimeout bounds the one-time code exchange.
	conversionTimeout = 20 * time.Second
	// SetupNewAppURL is where the browser posts the manifest.
	SetupNewAppURL = "https://github.com/settings/apps/new"
)

var manifestCodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Manifest is the GitHub App manifest patchbot registers. Only the fields
// patchbot needs are present: no webhook (inactive), no events, private, and
// exactly the run permissions.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     ManifestHook      `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
}

// ManifestHook is the inactive webhook declaration GitHub requires to carry a URL.
type ManifestHook struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// CallbackPath is the redirect target GitHub returns the one-time code to.
const CallbackPath = "/setup/github/callback"

// NewManifest builds the manifest for publicURL (an origin without a path).
func NewManifest(publicURL, name string) Manifest {
	origin := strings.TrimRight(publicURL, "/")
	perms := make(map[string]string, len(RunPermissions))
	for k, v := range RunPermissions {
		perms[k] = v
	}
	return Manifest{
		Name:               name,
		URL:                origin,
		HookAttributes:     ManifestHook{URL: origin + "/hooks/github", Active: false},
		RedirectURL:        origin + CallbackPath,
		Public:             false,
		DefaultPermissions: perms,
	}
}

// RandomAppName returns "patchbot-" followed by six random lowercase letters
// and digits. App names are unique across GitHub, so a fixed name would collide.
func RandomAppName() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("cannot read random bytes")
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "patchbot-" + string(b), nil
}

// Conversion is the part of the manifest conversion response patchbot keeps.
// The response also carries client_id, client_secret and webhook_secret; they
// are never decoded, so they never leave the response buffer.
type Conversion struct {
	ID   int64
	Slug string
	PEM  []byte
}

// ConvertManifest exchanges the one-time code for the new App's credentials:
// POST {baseURL}/app-manifests/{code}/conversions. The body is bounded, the
// call has a deadline, redirects are refused, and neither the response nor the
// code is ever included in an error.
func ConvertManifest(ctx context.Context, httpClient *http.Client, baseURL, code string) (Conversion, error) {
	if !manifestCodeRE.MatchString(code) {
		return Conversion{}, errors.New("GitHub setup: invalid code")
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil {
		return Conversion{}, errors.New("GitHub setup: invalid API base URL")
	}
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + "/app-manifests/" + code + "/conversions"
	target.RawQuery = ""
	ctx, cancel := context.WithTimeout(ctx, conversionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return Conversion{}, errors.New("GitHub setup: cannot build the request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "patchbot/"+version.Version)
	client := http.Client{}
	if httpClient != nil {
		client = *httpClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Conversion{}, errors.New("GitHub setup: the code exchange timed out")
		}
		return Conversion{}, errors.New("GitHub setup: the code exchange request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxConversionBody+1))
	defer clear(data)
	if err != nil || len(data) > maxConversionBody {
		return Conversion{}, errors.New("GitHub setup: unreadable or oversized response")
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Conversion{}, fmt.Errorf("GitHub setup: GitHub refused the code exchange (HTTP %d); the code is single-use and expires after one hour", resp.StatusCode)
	}
	var out struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		PEM  string `json:"pem"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Conversion{}, errors.New("GitHub setup: invalid response")
	}
	if out.ID <= 0 || !validSlug(out.Slug) || len(out.Slug) > 100 {
		return Conversion{}, errors.New("GitHub setup: response lacks a valid App ID or slug")
	}
	pemBytes := []byte(out.PEM)
	if key, err := ParseAppPrivateKey(pemBytes); err != nil || key == nil {
		clear(pemBytes)
		return Conversion{}, errors.New("GitHub setup: response lacks a usable private key")
	}
	return Conversion{ID: out.ID, Slug: out.Slug, PEM: pemBytes}, nil
}

// WriteNewKeyFile atomically creates path with mode 0600 holding pem, owned by
// the current (service) user. It refuses to touch an existing file, symlink or
// dangling link, and it never creates the parent directory: the operator
// provisions it. The key is written to an exclusive temp file in the same
// directory, synced, then hard-linked into place (link fails if path exists),
// so a reader never sees a partial key and an existing key is never replaced.
func WriteNewKeyFile(path string, pem []byte, excludedRoots ...string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return errors.New("private_key_file: must be a clean absolute path")
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrKeyFileExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("private_key_file: cannot inspect the path")
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("private_key_file: its directory must already exist and be owned by the service user")
	}
	if _, err := ParseAppPrivateKey(pem); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".patchbot-key-*")
	if err != nil {
		return errors.New("private_key_file: cannot create a file in its directory")
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	fail := func(msg string) error { _ = tmp.Close(); return errors.New("private_key_file: " + msg) }
	if err := tmp.Chmod(0o600); err != nil {
		return fail("cannot set mode 0600")
	}
	if _, err := tmp.Write(pem); err != nil {
		return fail("cannot write")
	}
	if err := tmp.Sync(); err != nil {
		return fail("cannot sync")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("private_key_file: cannot close")
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrKeyFileExists
		}
		return errors.New("private_key_file: cannot move the key into place")
	}
	if err := CheckAppKeyFile(path, excludedRoots...); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// ErrKeyFileExists means the key path is already occupied; setup never overwrites it.
var ErrKeyFileExists = errors.New("private_key_file: already exists; refusing to overwrite")

// AppInstallation is one account the App is installed on.
type AppInstallation struct {
	ID                  int64  `json:"id"`
	Account             string `json:"account"`
	RepositorySelection string `json:"repository_selection"`
}

// Installations lists the App's installations with GET /app/installations,
// authenticated by a JWT. Repositories are not listed: that needs an
// installation token, which patchbot mints only per run and repository.
func (a *App) Installations(ctx context.Context) ([]AppInstallation, error) {
	target := *a.jwtClient.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/app/installations"
	target.RawPath = ""
	target.RawQuery = "per_page=100"
	var raw []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"`
	}
	err := a.retry(ctx, func() error {
		_, err := a.jwtClient.request(ctx, nil, http.MethodGet, &target, nil, &raw, http.StatusOK)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]AppInstallation, 0, len(raw))
	for _, item := range raw {
		out = append(out, AppInstallation{ID: item.ID, Account: item.Account.Login, RepositorySelection: item.RepositorySelection})
	}
	return out, nil
}

// PreflightKeyFile checks, before the single-use code is spent, that a key can
// be written to path: the path is free and its directory exists and accepts a
// new file. Losing the key after the App was created would strand that App.
func PreflightKeyFile(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return errors.New("private_key_file: must be a clean absolute path")
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrKeyFileExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("private_key_file: cannot inspect the path")
	}
	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return errors.New("private_key_file: its directory must already exist and be owned by the service user")
	}
	probe, err := os.CreateTemp(dir, ".patchbot-probe-*")
	if err != nil {
		return errors.New("private_key_file: its directory is not writable by the service user")
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}
