package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestBindAndBearerProtection(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		allowed bool
	}{
		{"127.0.0.1:8080", true}, {"127.8.9.1:0", true}, {"[::1]:8080", true}, {"localhost:8080", true},
		{"0.0.0.0:8080", false}, {"[::]:8080", false}, {":8080", false}, {"192.0.2.1:8080", false}, {"example.com:8080", false},
	} {
		if err := ValidateAddress(tc.addr, secret.Secret{}); (err == nil) != tc.allowed {
			t.Errorf("address %s: %v", tc.addr, err)
		}
		if err := ValidateAddress(tc.addr, secret.New(testCredential)); err != nil {
			t.Errorf("authenticated address %s: %v", tc.addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1", "127.0.0.1:bad", "127.0.0.1:65536", "127.0.0.1:-1"} {
		if ValidateAddress(addr, secret.New(testCredential)) == nil {
			t.Errorf("invalid address accepted: %s", addr)
		}
	}
	f := newFixture(t, nil, func(opts *Options) { opts.Token = secret.New("ui-secret-fixture-token") })
	run := f.seed(store.StatusRejected)
	for _, path := range []string{"/", "/api/providers", "/api/runs", fmt.Sprintf("/api/runs/%d/events", run.ID), fmt.Sprintf("/api/runs/%d/artifacts/diff", run.ID), "/missing"} {
		for _, auth := range []string{"", "Bearer wrong", "Basic " + f.token, "Bearer " + f.token + "x"} {
			req := httptest.NewRequest("GET", "http://"+f.cfg.Server.Addr+path, nil)
			req.Header.Set("Authorization", auth)
			w := httptest.NewRecorder()
			f.server.ServeHTTP(w, req)
			requireStatus(t, w, 401)
			if strings.Contains(w.Body.String(), f.token) {
				t.Fatal("credential echoed")
			}
		}
	}
	requireStatus(t, f.request("GET", "/api/providers", ""), 200)
}

func TestCrossOriginHostAndSecurityHeaders(t *testing.T) {
	f := newFixture(t, nil, nil)
	for _, tc := range []struct {
		method, host, origin, fetch string
		want                        int
	}{
		{"POST", f.cfg.Server.Addr, "https://attacker.invalid", "", 403},
		{"POST", f.cfg.Server.Addr, "", "cross-site", 403},
		{"PUT", f.cfg.Server.Addr, "https://attacker.invalid", "", 403},
		{"POST", "attacker.invalid", "", "", 403},
		{"OPTIONS", "attacker.invalid", "", "", 403},
		{"POST", f.cfg.Server.Addr, "http://" + f.cfg.Server.Addr, "same-origin", 400},
	} {
		req := httptest.NewRequest(tc.method, "http://"+f.cfg.Server.Addr+"/api/runs", strings.NewReader(`{}`))
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Sec-Fetch-Site", tc.fetch)
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, req)
		requireStatus(t, w, tc.want)
		for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
			if w.Header().Get(header) == "" {
				t.Errorf("missing %s", header)
			}
		}
	}
	f.server.mu.Lock()
	f.server.addr = "0.0.0.0:8080"
	f.server.mu.Unlock()
	req := httptest.NewRequest("POST", "http://192.0.2.2:8080/api/runs", nil)
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 8080}))
	if !f.server.validHost(req) {
		t.Fatal("wildcard listener rejected its local connection address")
	}
	req.Host = "attacker.invalid:8080"
	if f.server.validHost(req) {
		t.Fatal("wildcard listener accepted arbitrary Host")
	}
}

func TestSecretsNeverAppearInResponses(t *testing.T) {
	token := "private-ui-fixture-value" //nolint:gosec // G101: deliberately fake credential for response redaction checks.
	f := newFixture(t, nil, func(opts *Options) { opts.Token = secret.New(token) })
	run := f.seed(store.StatusRejected)
	text := token + " " + testCredential + " sk-ant-abcdefghijklmnopqrstuv"
	if err := f.db.SaveArtifact(t.Context(), store.Artifact{RunID: run.ID, Kind: "diff", Content: text}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"output": text})
	if _, err := f.db.AppendEvent(t.Context(), store.Event{RunID: run.ID, Type: "test", PayloadJSON: payload}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/providers", "/api/providers/anthropic/models", "/api/runs", fmt.Sprintf("/api/runs/%d", run.ID), fmt.Sprintf("/api/runs/%d/artifacts/diff", run.ID), fmt.Sprintf("/api/runs/%d/events", run.ID)} {
		w := f.request("GET", path, "")
		requireStatus(t, w, 200)
		for _, value := range []string{token, testCredential, "sk-ant-abcdefghijklmnopqrstuv"} {
			if strings.Contains(w.Body.String(), value) {
				t.Fatal("secret in response")
			}
		}
	}
}

func TestHostPolicyForConfiguredAndBoundAddresses(t *testing.T) {
	f := newFixture(t, nil, func(opts *Options) { opts.Token = secret.New(testCredential) })
	for _, tc := range []struct {
		configured, bound, request string
		allowed                    bool
	}{
		{"control.example:0", "192.0.2.2:8080", "control.example:8080", true},
		{"control.example:0", "192.0.2.2:8080", "CONTROL.EXAMPLE:8080", true},
		{"control.example:0", "192.0.2.2:8080", "192.0.2.2:8080", true},
		{"control.example:0", "192.0.2.2:8080", "localhost:8080", false},
		{"control.example:0", "192.0.2.2:8080", "127.0.0.1:8080", false},
		{"control.example:0", "192.0.2.2:8080", "[::1]:8080", false},
		{"control.example:0", "192.0.2.2:8080", "control.example:8081", false},
		{"control.example:0", "192.0.2.2:8080", "control.example.attacker.invalid:8080", false},
		{"localhost:0", "127.0.0.1:80", "localhost", true},
		{"localhost:0", "127.0.0.1:80", "127.0.0.1", true},
		{"localhost:0", "[::1]:80", "[::1]", true},
		{"localhost:0", "[::1]:8080", "127.0.0.1:8080", true},
		{"localhost:0", "[::1]:8080", "attacker.invalid:8080", false},
		{"0.0.0.0:0", "0.0.0.0:8080", "192.0.2.2:8080", true},
		{"0.0.0.0:0", "0.0.0.0:8080", "0.0.0.0:8080", false},
		{"0.0.0.0:0", "0.0.0.0:8080", "localhost:8080", false},
	} {
		t.Run(tc.bound+"/"+tc.request, func(t *testing.T) {
			f.cfg.Server.Addr = tc.configured
			f.server.mu.Lock()
			f.server.addr = tc.bound
			f.server.mu.Unlock()
			r := httptest.NewRequest("GET", "http://"+tc.request+"/", nil)
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 8080}))
			if got := f.server.validHost(r); got != tc.allowed {
				t.Fatalf("configured %s, bound %s: Host %s allowed=%t, want %t", tc.configured, tc.bound, tc.request, got, tc.allowed)
			}
		})
	}
}
