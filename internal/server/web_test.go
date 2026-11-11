package server

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/web"
)

func stateFromPage(t *testing.T, html string) web.State {
	t.Helper()
	const marker = `<script type="application/json" id="state">`
	_, tail, ok := strings.Cut(html, marker)
	if !ok {
		t.Fatal("missing state block")
	}
	raw, _, ok := strings.Cut(tail, "</script>")
	if !ok {
		t.Fatal("missing state closing tag")
	}
	var state web.State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("invalid bootstrap JSON: %v", err)
	}
	return state
}

func TestPagesAndEscapedIssueState(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	title := `</script><img src=x onerror="alert(1)"><script>alert(2)</script>`
	if err := f.db.SetRunStatus(t.Context(), run.ID, run.Status, run.Status, store.RunPatch{IssueTitle: &title, Phase: store.PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, page string }{
		{"/", "runs"}, {"/runs", "runs"}, {"/runs/new", "new"}, {fmt.Sprintf("/runs/%d", run.ID), "detail"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := f.request("GET", tc.path, "")
			requireStatus(t, w, 200)
			if w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatal("incorrect page media type")
			}
			state := stateFromPage(t, w.Body.String())
			if state.Page != tc.page || state.AuthRequired {
				t.Fatalf("wrong state: %+v", state)
			}
			if strings.Contains(w.Body.String(), title) || strings.Contains(w.Body.String(), "<img src=x") {
				t.Fatal("issue title became markup")
			}
			if tc.page == "detail" {
				var detail struct{ Run store.Run }
				if err := json.Unmarshal(state.Initial, &detail); err != nil || detail.Run.IssueTitle != title || state.RunID != run.ID {
					t.Fatalf("issue state failed to round-trip: %v", err)
				}
			}
		})
	}
	for _, path := range []string{"/runs/0", "/runs/nope", "/runs/1/missing"} {
		requireStatus(t, f.request("GET", path, ""), 404)
	}
	requireStatus(t, f.request("GET", "/runs/999999", ""), 404)
	requireStatus(t, f.request("POST", "/runs/new", "{}"), 405)
}

func TestModeCSPAndAssets(t *testing.T) {
	f := newFixture(t, nil, nil)
	for _, mode := range []string{"cdn", "local"} {
		t.Run(mode, func(t *testing.T) {
			f.cfg.Server.VueSrc = mode
			page := f.request("GET", "/runs/new", "")
			requireStatus(t, page, 200)
			scripts := "'self'"
			source := web.VueURL
			if mode == "cdn" {
				scripts += " https://cdn.jsdelivr.net"
			} else {
				source = "/assets/vue.global.prod.js"
			}
			want := "default-src 'none'; script-src " + scripts + "; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
			if got := page.Header().Get("Content-Security-Policy"); got != want {
				t.Fatalf("CSP = %q, want %q", got, want)
			}
			if !strings.Contains(html.UnescapeString(page.Body.String()), `src="`+source+`" integrity="`+web.VueIntegrity+`" crossorigin="anonymous"`) {
				t.Fatal("wrong Vue source or missing integrity pin")
			}
			for path, media := range map[string]string{"/assets/app.js": "text/javascript; charset=utf-8", "/assets/app.css": "text/css; charset=utf-8"} {
				w := f.request("GET", path, "")
				requireStatus(t, w, 200)
				if w.Header().Get("Content-Type") != media || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Body.Len() == 0 {
					t.Fatalf("incorrect static response for %s", path)
				}
			}
			vue := f.request("GET", "/assets/vue.global.prod.js", "")
			switch {
			case mode == "cdn":
				if vue.Code != 404 {
					t.Fatal("CDN mode exposed local Vue")
				}
			case vue.Code == 200:
				if vue.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
					t.Fatal("incorrect vendor media type")
				}
			case vue.Code != 503 || !strings.Contains(vue.Body.String(), "make vendor-vue"):
				t.Fatal("missing vendor must explain how to install it")
			}
			if vue.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("vendor response missing nosniff")
			}
		})
	}
	requireStatus(t, f.request("GET", "/assets/shell.html", ""), 404)
	requireStatus(t, f.request("POST", "/assets/app.js", "{}"), 405)
}

func TestProtectedShellNeverRendersTokenOrRunData(t *testing.T) {
	const token = "test-ui-private-credential"
	f := newFixture(t, nil, func(opts *Options) { opts.Token = secret.New(token) })
	run := f.seed(store.StatusAwaitingApproval)
	title := "private issue " + token
	if err := f.db.SetRunStatus(t.Context(), run.ID, run.Status, run.Status, store.RunPatch{IssueTitle: &title, Phase: store.PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/runs", "/runs/new", fmt.Sprintf("/runs/%d", run.ID)} {
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + token} {
			r := httptest.NewRequest(http.MethodGet, "http://"+f.cfg.Server.Addr+path, nil)
			r.Header.Set("Authorization", auth)
			w := httptest.NewRecorder()
			f.server.ServeHTTP(w, r)
			requireStatus(t, w, 200)
			state := stateFromPage(t, w.Body.String())
			if !state.AuthRequired || state.Initial != nil || state.Preview != "" || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "private issue") {
				t.Fatal("protected shell disclosed data or credentials")
			}
		}
	}
	for _, path := range []string{"/assets/app.js", "/assets/app.css"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+f.cfg.Server.Addr+path, nil)
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, r)
		requireStatus(t, w, 200)
	}
	for _, path := range []string{fmt.Sprintf("/api/runs/%d", run.ID), fmt.Sprintf("/api/runs/%d/events", run.ID), "/api/providers"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+f.cfg.Server.Addr+path, nil)
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, r)
		requireStatus(t, w, 401)
	}
	req := httptest.NewRequest(http.MethodGet, "http://attacker.invalid/runs/new", nil)
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	requireStatus(t, w, 403)
}
