package cli_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
	"github.com/aryanpappu/patchbot/internal/testhttp"
)

func TestModelsFallbackAndUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		args     []string
		file     string
		code     int
		out, err string
	}{
		{name: "defaults without key", args: []string{"models"}, out: "claude-sonnet-5-5", err: "showing the models from the config"},
		{name: "unknown provider", args: []string{"models", "--provider", "missing"}, code: 1, err: "unknown provider"},
		{name: "unsupported adapter", args: []string{"models", "--provider", "local"}, file: "[providers.local]\nkind='openai'\nmodels=['custom-model']\n", out: "custom-model", err: "showing the models from the config"},
		{name: "unknown flag", args: []string{"models", "--bad"}, code: 2, err: "Usage: patchbot models"},
		{name: "missing flag value", args: []string{"models", "--provider"}, code: 2, err: "Usage: patchbot models"},
		{name: "positional argument", args: []string{"models", "unexpected"}, code: 2, err: "Usage: patchbot models"},
		{name: "help", args: []string{"models", "--help"}, out: "Usage: patchbot models"},
		{name: "config parse failure", args: []string{"models"}, file: "[defaults", code: 1, err: "error:"},
		{name: "config missing", args: []string{"models", "--config", "/no/such/patchbot-m7.toml"}, code: 1, err: "error:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, out, errOut := runApp(t, tc.file, nil, tc.args...)
			if code != tc.code || !strings.Contains(out, tc.out) || !strings.Contains(errOut, tc.err) {
				t.Errorf("exit=%d, stdout=%s, stderr=%s", code, out, errOut)
			}
			if tc.name == "defaults without key" {
				for _, want := range []string{"MODEL ID", "DISPLAY NAME", "SOURCE", "config"} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %q: %s", want, out)
					}
				}
			}
		})
	}
}

func TestModelsConfigRegistryAnthropic(t *testing.T) {
	t.Parallel()
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", failure), func(t *testing.T) {
			t.Parallel()
			var count atomic.Int32
			server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "custom-env-key" {
					t.Errorf("request=%s %s, key matched=%t", r.Method, r.URL.Path, r.Header.Get("x-api-key") == "custom-env-key")
				}
				w.Header().Set("Content-Type", "application/json")
				if failure {
					w.WriteHeader(401)
					_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"custom-env-key"}}`)
					return
				}
				if r.URL.Query().Get("after_id") == "" {
					_, _ = fmt.Fprint(w, `{"data":[{"id":"model-one","display_name":"Model One","type":"model","created_at":"2026-01-01T00:00:00Z"}],"has_more":true,"last_id":"model-one"}`)
					return
				}
				if r.URL.Query().Get("after_id") != "model-one" {
					t.Error("wrong page cursor")
				}
				_, _ = fmt.Fprint(w, `{"data":[{"id":"model-two","display_name":"Model Two","type":"model","created_at":"2026-01-01T00:00:00Z"}],"has_more":false,"last_id":"model-two"}`)
			}))
			defer server.Close()
			dir := t.TempDir()
			path := filepath.Join(dir, "explicit.toml")
			data := fmt.Sprintf("[providers.custom]\nkind='anthropic'\napi_key_env='CUSTOM_MODEL_KEY'\nbase_url=%q\nmodels=['configured-only']\n", server.URL)
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			app := cli.App{Stdout: &out, Stderr: &errOut, Config: config.Options{Dir: dir, LookupEnv: func(name string) (string, bool) {
				// --provider must override this environment selection.
				switch name {
				case "CUSTOM_MODEL_KEY":
					return "custom-env-key", true
				case "PATCHBOT_PROVIDER":
					return "anthropic", true
				}
				return "", false
			}, UserConfigDir: func() (string, error) { return dir, nil }}, Providers: providers.Options{HTTPClient: server.Client()}}
			code := app.Run([]string{"models", "--config", path, "--provider", "custom"})
			if code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, errOut.String())
			}
			if strings.Contains(out.String()+errOut.String(), "custom-env-key") {
				t.Error("key leaked")
			}
			if !strings.Contains(out.String(), "configured-only") || !strings.Contains(out.String(), "config") {
				t.Errorf("missing config model: %s", out.String())
			}
			if failure {
				if count.Load() != 1 || !strings.Contains(errOut.String(), "showing the models from the config") {
					t.Errorf("calls=%d, stderr=%s", count.Load(), errOut.String())
				}
			} else {
				for _, want := range []string{"model-one", "Model One", "model-two", "Model Two", "live"} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("missing %q: %s", want, out.String())
					}
				}
				if count.Load() != 2 || errOut.Len() != 0 {
					t.Errorf("calls=%d, stderr=%s", count.Load(), errOut.String())
				}
			}
		})
	}
}
