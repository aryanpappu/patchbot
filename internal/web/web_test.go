package web

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"html"
	"io/fs"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func TestStateEscapingAndPlainMarkdown(t *testing.T) {
	payload := `</script><script>alert("xss")</script><img src=x onerror=alert(1)>` + "\u2028\u2029&"
	markdown := "# Spec\n<script>alert(1)</script>\n[unsafe](javascript:alert(1))\n**text**"
	raw, err := json.Marshal(map[string]string{"issue_title": payload})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Render(&out, false, State{Page: "detail", RunID: 1, Initial: raw, Preview: markdown}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, payload) || strings.Contains(html, "<img") || strings.Contains(html, "<script>alert") {
		t.Fatalf("active markup escaped its text context: %s", html)
	}
	start := strings.Index(html, `id="state">`) + len(`id="state">`)
	end := strings.Index(html[start:], "</script>") + start
	var state struct {
		Initial map[string]string `json:"initial"`
	}
	if err := json.Unmarshal([]byte(html[start:end]), &state); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if state.Initial["issue_title"] != payload {
		t.Fatalf("state did not round-trip: %q", state.Initial["issue_title"])
	}
	// The chosen plain-text alternative has no markdown-generated HTML or links.
	if !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "[unsafe](javascript:alert(1))") || strings.Contains(html, `href="javascript:`) || strings.Contains(html, "<strong>") {
		t.Fatal("plain markdown was not preserved safely")
	}
}

func TestPinnedSRI(t *testing.T) {
	if VueVersion != "3.5.13" || VueURL != "https://cdn.jsdelivr.net/npm/vue@3.5.13/dist/vue.global.prod.js" {
		t.Fatal("Vue must have an exact reviewed version")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(VueIntegrity, "sha384-"))
	if err != nil || len(raw) != 48 {
		t.Fatal("integrity must be a SHA-384 SRI value")
	}
	for _, local := range []bool{false, true} {
		var out bytes.Buffer
		if err := Render(&out, local, State{Page: "runs"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(html.UnescapeString(out.String()), `integrity="`+VueIntegrity+`" crossorigin="anonymous"`) {
			t.Fatal("script is missing SRI or crossorigin")
		}
	}
}

var forbiddenSinks = []string{"v-html", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write(", "eval(", "new Function(",
	"srcdoc", "template:", "compile(", "createContextualFragment", "DOMParser", "setAttribute(\"on", "setAttribute('on", "javascript:"}

func TestNoUnsafeHTMLRendering(t *testing.T) {
	err := fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (!strings.HasSuffix(path, ".js") && !strings.HasSuffix(path, ".html")) {
			return err
		}
		data, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		for _, forbidden := range forbiddenSinks {
			// A downloaded upstream Vue runtime is outside our app's source policy.
			if path != "assets/vue.global.prod.js" && bytes.Contains(data, []byte(forbidden)) {
				t.Errorf("%s contains forbidden HTML/code sink %q", path, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVendoredVueMatchesIntegrity(t *testing.T) {
	data, err := files.ReadFile("assets/vue.global.prod.js")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha512.Sum384(data)
	actual := "sha384-" + base64.StdEncoding.EncodeToString(hash[:])
	if actual != VueIntegrity {
		t.Fatalf("embedded Vue SRI: got %s, want %s", actual, VueIntegrity)
	}
}

// The request form renders user text only through value bindings and text
// nodes, and links only through the validated link() helper.
func TestWriteRequestFormUsesSafeSinks(t *testing.T) {
	data, err := files.ReadFile("assets/components/NewRequest.js")
	if err != nil {
		t.Fatal(err)
	}
	form := string(data)
	for _, want := range []string{`input(draft, "title"`, `area(draft, "body"`, "link(made.html_url", `"Create issue"`, "/api/issues", "idempotency_key"} {
		if !strings.Contains(form, want) {
			t.Errorf("request form lacks %q", want)
		}
	}
	for _, bad := range []string{"innerHTML", "v-html", "href:", "template:"} {
		if strings.Contains(form, bad) {
			t.Errorf("request form uses %q", bad)
		}
	}
	ui, err := files.ReadFile("assets/format.js")
	if err != nil || !strings.Contains(string(ui), `["http:", "https:"].includes(parsed.protocol)`) {
		t.Fatal("link helper protocol check missing")
	}
}

func moduleSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range assetNames {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		data, err := files.ReadFile("assets/" + name)
		if err != nil {
			t.Fatalf("listed asset %s is not embedded: %v", name, err)
		}
		out[name] = string(data)
	}
	return out
}

// The served list equals the embedded tree exactly: nothing unlisted is
// reachable and nothing listed is missing.
func TestAssetListMatchesEmbeddedTree(t *testing.T) {
	embedded := map[string]bool{}
	err := fs.WalkDir(files, "assets", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			embedded[strings.TrimPrefix(path, "assets/")] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{"vue.global.prod.js": true, "vue.global.prod.js.placeholder": true}
	for _, name := range assetNames {
		if listed[name] {
			t.Fatalf("duplicate asset %s", name)
		}
		listed[name] = true
		if strings.Contains(name, "..") || strings.Contains(name, "*") || strings.HasPrefix(name, "/") {
			t.Fatalf("asset name %q is not a plain relative path", name)
		}
	}
	for name := range embedded {
		if !listed[name] {
			t.Errorf("embedded file %s is not on the served list (or the placeholder rules changed)", name)
		}
	}
	for name := range listed {
		if !embedded[name] {
			t.Errorf("listed asset %s is not embedded", name)
		}
	}
	if AssetPath("/assets/placeholder") || AssetPath("/assets/vue.global.prod.js.placeholder") || AssetPath("/assets/components/") ||
		AssetPath("/assets/components/../app.css") || AssetPath("/assets/components/Evil.js") || AssetPath("/assets/shell.html") || AssetPath("/api/runs") {
		t.Fatal("AssetPath accepted an unlisted path")
	}
}

// Every static import resolves to a listed module, so the page never requests
// a path the server would refuse.
func TestModuleImportsResolve(t *testing.T) {
	importRe := regexp.MustCompile(`from "(\.{1,2}/[^"]+)"`)
	for name, src := range moduleSources(t) {
		for _, m := range importRe.FindAllStringSubmatch(src, -1) {
			target := path.Join(path.Dir(name), m[1])
			if !assetSet[target] {
				t.Errorf("%s imports %s, which is not on the asset list", name, target)
			}
		}
	}
	for _, want := range []string{"main.js", "ui.js", "format.js", "components/RunDetail.js", "composables/useRunStream.js"} {
		if !assetSet[want] {
			t.Errorf("missing %s", want)
		}
	}
}

// Modules never build markup from strings, assign hrefs from data without the
// validated helper, or hold secrets in storage.
func TestModulesRenderOnlyText(t *testing.T) {
	htmlInTemplate := regexp.MustCompile("`[^`\\n]*<[a-zA-Z/!][^`\\n]*`")
	hrefLiteral := regexp.MustCompile(`href:\s*("/[^"]*"|runHref\([^)]*\)|item\.href|href\b)`)
	for name, src := range moduleSources(t) {
		if name == "setup.js" {
			continue
		}
		for _, bad := range forbiddenSinks {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains forbidden sink %q", name, bad)
			}
		}
		if htmlInTemplate.MatchString(src) {
			t.Errorf("%s builds markup in a template string", name)
		}
		for _, line := range strings.Split(src, "\n") {
			if i := strings.Index(line, "href:"); i >= 0 && !strings.HasPrefix(strings.TrimSpace(line), "//") && !hrefLiteral.MatchString(line[i:]) {
				t.Errorf("%s sets href from an unvalidated value: %s", name, strings.TrimSpace(line))
			}
		}
		if strings.Contains(src, "localStorage") && name != "composables/useTheme.js" {
			t.Errorf("%s uses localStorage; only the theme choice may", name)
		}
		if strings.Contains(src, "sessionStorage") && name != "composables/useApi.js" {
			t.Errorf("%s uses sessionStorage; only the unlock helper may", name)
		}
		if strings.Contains(src, "document.cookie") {
			t.Errorf("%s touches cookies", name)
		}
	}
}

// XSS payloads in specs, diffs, logs and repository names reach the DOM only
// through text helpers: block(), Vue text children and the diff line text.
func TestUntrustedSurfacesUseTextHelpers(t *testing.T) {
	src := moduleSources(t)
	need := map[string][]string{
		"components/DiffView.js":    {"line.text"},
		"components/LogStream.js":   {"block(event.output"},
		"components/ChangesView.js": {"block(a.content"},
		"components/SpecView.js":    {"block(specText(current)", "h(\"p\", { class: \"pre\" }, spec.summary)"},
		"components/RepoCard.js":    {"h(\"h3\", r.name)"},
		"components/MemoryView.js":  {"block(visible("},
		"ui.js":                     {`h("pre", { class: "output", tabindex: 0, "aria-label": label }, text(value))`},
	}
	for name, wants := range need {
		for _, want := range wants {
			if !strings.Contains(src[name], want) {
				t.Errorf("%s no longer renders %q as text", name, want)
			}
		}
	}
	if strings.Contains(src["components/DiffView.js"], "href") {
		t.Fatal("diff view must not create links")
	}
}

func TestShellLoadsModuleFromSelf(t *testing.T) {
	var out bytes.Buffer
	if err := Render(&out, false, State{Page: "inbox"}); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	if !strings.Contains(page, `<script type="module" src="/assets/main.js"></script>`) {
		t.Fatal("entry module missing")
	}
	if strings.Count(page, "<script") != 3 || strings.Contains(page, "onclick") || strings.Contains(page, "style=") {
		t.Fatal("shell must hold only the JSON state, Vue and the module")
	}
	if strings.Contains(CSP(false), "unsafe-") || strings.Contains(CSP(true), "unsafe-") {
		t.Fatal("CSP must not allow unsafe-inline or unsafe-eval")
	}
}

func TestAssetServingFixedList(t *testing.T) {
	for _, name := range assetNames {
		w := httptest.NewRecorder()
		Asset(w, httptest.NewRequest("GET", "/assets/"+name, nil), false)
		want := "text/javascript; charset=utf-8"
		if strings.HasSuffix(name, ".css") {
			want = "text/css; charset=utf-8"
		}
		if w.Code != 200 || w.Header().Get("Content-Type") != want || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, bad := range []string{"/assets/", "/assets/components/Nope.js", "/assets/app.js", "/assets/components/../ui.js", "/assets/vue.global.prod.js", "/assets/vue.global.prod.js.placeholder"} {
		w := httptest.NewRecorder()
		Asset(w, httptest.NewRequest("GET", bad, nil), false)
		if w.Code != 404 {
			t.Errorf("%s served with %d", bad, w.Code)
		}
	}
}

// The approve call sends exactly the version and hash captured when the confirm
// dialog opened, and never retries on spec_changed.
func TestApproveSendsShownSpecVersionAndHash(t *testing.T) {
	data, err := files.ReadFile("assets/components/RunDetail.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	ask := strings.Index(src, "confirm.ask({ title: `Approve specification")
	send := strings.Index(src, `act("approve", { spec_version: spec.version, spec_hash: spec.hash })`)
	capture := strings.Index(src, "const spec = s.detail?.latest_spec;")
	if capture < 0 || ask < capture || send < ask {
		t.Fatal("approve must capture the spec before the dialog and send its version and hash after confirmation")
	}
	if strings.Count(src, `act("approve"`) != 1 {
		t.Fatal("approve must have a single call site")
	}
}
