package web

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"html"
	"io/fs"
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

func TestNoUnsafeHTMLRendering(t *testing.T) {
	err := fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (!strings.HasSuffix(path, ".js") && !strings.HasSuffix(path, ".html")) {
			return err
		}
		data, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		for _, forbidden := range []string{"v-html", "innerHTML", "outerHTML", "document.write(", "eval(", "new Function("} {
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
