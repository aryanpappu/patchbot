// Package web embeds the browser interface. Run data comes from the JSON API.
package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
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
	// Email is the verified Cloudflare Access login, shown in the header.
	Email string `json:"email,omitempty"`
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

// assetNames is the complete, fixed list of files served under /assets/. Nothing
// is matched by prefix or wildcard: a file is public only if it is named here,
// and a test asserts the list equals the embedded asset tree (minus the
// vendored Vue build, which is added conditionally by Asset).
var assetNames = []string{
	"main.js", "ui.js", "format.js", "setup.js", "app.css",
	"components/ActionBar.js", "components/App.js", "components/BudgetMeters.js", "components/ChangesView.js",
	"components/ConfirmDialog.js", "components/DiffView.js", "components/HelpDialog.js", "components/Inbox.js",
	"components/LogStream.js", "components/MemoryView.js", "components/Modal.js", "components/ModelFields.js",
	"components/NewRequest.js", "components/PhaseStepper.js", "components/RepoCard.js", "components/RepoEditor.js",
	"components/QuestionsPanel.js", "components/RepoPage.js", "components/RunCard.js", "components/RunDetail.js", "components/RunList.js",
	"components/Sidebar.js", "components/SpecEditor.js", "components/SpecView.js", "components/Tabs.js",
	"components/Toast.js", "components/Unlock.js",
	"composables/useApi.js", "composables/useBadges.js", "composables/useConfirm.js", "composables/useKeyboard.js",
	"composables/useProviders.js", "composables/useRunStream.js", "composables/useSession.js", "composables/useTask.js",
	"composables/useTheme.js", "composables/useToast.js",
}

const vueAsset = "vue.global.prod.js"

var assetSet = func() map[string]bool {
	set := map[string]bool{vueAsset: true}
	for _, name := range assetNames {
		set[name] = true
	}
	return set
}()

// AssetNames returns the fixed served asset list, excluding the vendored Vue.
func AssetNames() []string { return append([]string(nil), assetNames...) }

// AssetPath reports whether path is exactly one of the fixed, data-free asset
// URLs. Access mode and token mode both use it for the unauthenticated GET list.
func AssetPath(path string) bool {
	name, ok := strings.CutPrefix(path, "/assets/")
	return ok && assetSet[name]
}

// PublicPath identifies exact shell/asset GET paths that contain no private data.
func PublicPath(path string) bool {
	switch path {
	case "/", "/runs", "/runs/new", "/repos", "/memory", "/setup/github":
		return true
	}
	if AssetPath(path) {
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
	name, ok := strings.CutPrefix(r.URL.Path, "/assets/")
	if !ok || !assetSet[name] || (name == vueAsset && !local) {
		http.NotFound(w, r)
		return
	}
	media := "text/javascript; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		media = "text/css; charset=utf-8"
	}
	data, err := files.ReadFile("assets/" + name)
	if err != nil {
		http.Error(w, "Vue is not vendored. Run make vendor-vue, then rebuild patchbot.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", media)
	// data is an embedded file chosen only from the fixed assetNames list, with a
	// fixed script/style media type and nosniff.
	_, _ = w.Write(data) //nolint:gosec // G705: not user-controlled content
}
