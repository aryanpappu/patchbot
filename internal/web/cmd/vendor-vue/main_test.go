package main

import (
	"crypto/sha512"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/web"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstallVerifiesBeforeReplacingVendor(t *testing.T) {
	const content = "/* fixture Vue */"
	hash := sha512.Sum384([]byte(content))
	valid := "sha384-" + base64.StdEncoding.EncodeToString(hash[:])
	for _, tc := range []struct {
		name, integrity, body string
		status                int
		wantError             bool
	}{
		{"verified", valid, content, 200, false},
		{"mismatch", valid, "tampered", 200, true},
		{"http error", valid, content, 502, true},
		{"oversized", valid, strings.Repeat("x", (2<<20)+1), 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "vue.global.prod.js")
			if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != web.VueURL {
					t.Fatalf("unexpected download URL: %s", r.URL)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			err := install(client, tc.integrity, dir)
			if (err != nil) != tc.wantError {
				t.Fatalf("install error: %v", err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			data, err := root.ReadFile("vue.global.prod.js")
			if err != nil {
				t.Fatal(err)
			}
			want := content
			if tc.wantError {
				want = "previous"
			}
			if string(data) != want {
				t.Fatalf("installed %q, want %q", data, want)
			}
		})
	}
}
