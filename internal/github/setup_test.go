//go:build unix

package github

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/testhttp"
)

func setupPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestWriteNewKeyFile(t *testing.T) {
	keyPEM := setupPEM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "k.pem")
	if err := WriteNewKeyFile(path, keyPEM); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if _, err := LoadAppPrivateKey(path); err != nil {
		t.Fatalf("written key does not load: %v", err)
	}
	if err := WriteNewKeyFile(path, keyPEM); !errors.Is(err, ErrKeyFileExists) {
		t.Fatalf("overwrite: %v", err)
	}
	// A symlink (even dangling) at the path is refused, not followed.
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if err := WriteNewKeyFile(link, keyPEM); !errors.Is(err, ErrKeyFileExists) {
		t.Fatalf("symlink: %v", err)
	}
	for _, bad := range []string{"relative.pem", filepath.Join(dir, "missing", "k.pem"), dir + "/a/../b.pem"} {
		if err := WriteNewKeyFile(bad, keyPEM); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := WriteNewKeyFile(filepath.Join(dir, "junk.pem"), []byte("not a key")); err == nil {
		t.Fatal("accepted a non-key")
	}
	// An excluded root (the data directory) is refused and nothing is left.
	inside := filepath.Join(dir, "inside.pem")
	if err := WriteNewKeyFile(inside, keyPEM, dir); err == nil {
		t.Fatal("key inside an excluded root accepted")
	}
	if _, err := os.Lstat(inside); err == nil {
		t.Fatal("refused key left on disk")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".patchbot-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestConvertManifest(t *testing.T) {
	keyPEM := setupPEM(t)
	var gotPath, gotMethod string
	respond := func(status int, body []byte) *http.Client {
		server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotMethod = r.URL.Path, r.Method
			if r.Header.Get("Authorization") != "" {
				t.Error("conversion must be unauthenticated")
			}
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}))
		t.Cleanup(server.Close)
		client := server.Client()
		client.Transport = rewrite{base: client.Transport, url: server.URL}
		return client
	}
	good, _ := json.Marshal(map[string]any{"id": 5, "slug": "patchbot-ab12cd", "pem": string(keyPEM), "client_secret": "S", "webhook_secret": "W"})
	conv, err := ConvertManifest(t.Context(), respond(201, good), "", "abc123")
	if err != nil || conv.ID != 5 || conv.Slug != "patchbot-ab12cd" || !bytes.Equal(conv.PEM, keyPEM) {
		t.Fatalf("%+v %v", conv, err)
	}
	if gotMethod != "POST" || gotPath != "/app-manifests/abc123/conversions" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	for name, tc := range map[string]struct {
		status int
		body   []byte
		code   string
	}{
		"not found":     {404, []byte(`{"message":"SECRET-ECHO"}`), "abc123"},
		"oversized":     {201, append([]byte(`{"pem":"`), bytes.Repeat([]byte("A"), maxConversionBody)...), "abc123"},
		"bad json":      {201, []byte(`{SECRET-ECHO`), "abc123"},
		"no key":        {201, []byte(`{"id":5,"slug":"x","pem":"SECRET-ECHO"}`), "abc123"},
		"bad slug":      {201, []byte(`{"id":5,"slug":"../x","pem":"SECRET-ECHO"}`), "abc123"},
		"path in code":  {201, good, "../x"},
		"empty code":    {201, good, ""},
		"zero id":       {201, []byte(`{"id":0,"slug":"x"}`), "abc123"},
		"redirect-like": {302, []byte(`SECRET-ECHO`), "abc123"},
	} {
		_, err := ConvertManifest(t.Context(), respond(tc.status, tc.body), "", tc.code)
		if err == nil || strings.Contains(err.Error(), "SECRET-ECHO") || strings.Contains(err.Error(), "abc123") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// rewrite sends requests for api.github.com to the test server.
type rewrite struct {
	base http.RoundTripper
	url  string
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = "http", strings.TrimPrefix(r.url, "http://")
	if strings.HasPrefix(r.url, "https://") {
		clone.URL.Scheme, clone.URL.Host = "https", strings.TrimPrefix(r.url, "https://")
	}
	return r.base.RoundTrip(clone)
}

func TestNewManifestPermissionsMatchRunPermissions(t *testing.T) {
	m := NewManifest("https://patchbot.example.org/", "n")
	if m.RedirectURL != "https://patchbot.example.org/setup/github/callback" || m.URL != "https://patchbot.example.org" || m.Public || m.HookAttributes.Active {
		t.Fatalf("%+v", m)
	}
	if len(m.DefaultPermissions) != len(RunPermissions) {
		t.Fatal("permission sets differ")
	}
	for k, v := range RunPermissions {
		if m.DefaultPermissions[k] != v {
			t.Fatalf("%s", k)
		}
	}
	m.DefaultPermissions["contents"] = "read"
	if RunPermissions["contents"] != "write" {
		t.Fatal("manifest aliases RunPermissions")
	}
}
