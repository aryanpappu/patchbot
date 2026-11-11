// Package web embeds the browser interface. Run data comes from the JSON API.
package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"strconv"
)

// VueVersion pins the global production build used in both delivery modes.
const VueVersion = "3.5.13"

// VueURL is the only third-party script loaded by the page.
const VueURL = "https://cdn.jsdelivr.net/npm/vue@" + VueVersion + "/dist/vue.global.prod.js"

// VueIntegrity is the SRI hash of VueURL, checked against jsDelivr and unpkg on 2026-11-11.
// make vendor-vue verifies it before installing the downloaded build.
const VueIntegrity = "sha384-W/1Fp/LgAYO/oTn9Gs+PbeWuMuq1eQCnUMPCeg8POmMYchhzxctjEqtbiCIxDOON"

//go:embed assets shell.html
var files embed.FS

var shell = template.Must(template.ParseFS(files, "shell.html"))

// State contains bootstrap data. Initial and Preview stay empty when protected
// run data must be fetched after browser authentication.
type State struct {
	Page         string          `json:"page"`
	RunID        int64           `json:"run_id,omitempty"`
	AuthRequired bool            `json:"auth_required"`
	Initial      json.RawMessage `json:"initial,omitempty"`
	Preview      string          `json:"preview,omitempty"`
}

// Render writes a page shell. State remains an ordinary Go value so html/template
// performs JSON serialization and script-context escaping, including </script>.
func Render(w io.Writer, local bool, state State) error {
	src := VueURL
	if local {
		src = "/assets/vue.global.prod.js"
	}
	return shell.Execute(w, struct {
		Source    string
		Integrity string
		State     State
	}{src, VueIntegrity, state})
}

// CSP permits the app, styles and API on this origin and only the pinned CDN
// origin for Vue. Render functions avoid a runtime template compiler/eval grant.
func CSP(local bool) string {
	scripts := "'self'"
	if !local {
		scripts += " https://cdn.jsdelivr.net"
	}
	return "default-src 'none'; script-src " + scripts + "; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

// PublicPath identifies exact shell/asset GET paths that contain no private data.
func PublicPath(path string) bool {
	switch path {
	case "/", "/runs", "/runs/new", "/assets/app.js", "/assets/app.css", "/assets/vue.global.prod.js":
		return true
	}
	if len(path) > len("/runs/") && path[:len("/runs/")] == "/runs/" {
		id, err := strconv.ParseInt(path[len("/runs/"):], 10, 64)
		return err == nil && id > 0
	}
	return false
}

// Asset serves only explicitly named embedded assets with fixed media types.
func Asset(w http.ResponseWriter, r *http.Request, local bool) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	name, media := "", ""
	switch r.URL.Path {
	case "/assets/app.js":
		name, media = "app.js", "text/javascript; charset=utf-8"
	case "/assets/app.css":
		name, media = "app.css", "text/css; charset=utf-8"
	case "/assets/vue.global.prod.js":
		if local {
			name, media = "vue.global.prod.js", "text/javascript; charset=utf-8"
		}
	}
	if name == "" {
		http.NotFound(w, r)
		return
	}
	data, err := files.ReadFile("assets/" + name)
	if err != nil {
		http.Error(w, "Vue is not vendored. Run make vendor-vue, then rebuild patchbot.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", media)
	_, _ = w.Write(data)
}
