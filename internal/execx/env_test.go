package execx

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	values := make(map[string]string)
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	return values
}

func TestMinimalEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "parent-anthropic-secret")
	t.Setenv("GITHUB_TOKEN", "parent-github-secret")
	t.Setenv("UNRELATED_PARENT", "also-not-inherited")
	denied := []string{"TOKEN", "MY_KEY", "client_secret", "PASSWORD_FILE", "AWS_PROFILE", "GH_HOST",
		"GITHUB_ACTOR", "ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "GEMINI_MODEL", "GOOGLE_CLOUD_PROJECT"}
	extras := map[string]string{"CUSTOM": "literal value", "PATH": ".", "GOFLAGS": "-mod=mod",
		"GOTOOLCHAIN": "auto", "GOENV": "/outside", "GOWORK": "/outside/go.work", "HOME": "/outside",
		"bad\x00name": "value", "BAD_VALUE": "has\x00nul", "BAD=NAME": "value", "": "value"}
	for _, name := range denied {
		extras[name] = "extra-secret"
	}
	home, tmp := t.TempDir(), t.TempDir()
	got := MinimalEnv(home, tmp, extras)
	want := map[string]string{"PATH": TrustedPath, "HOME": home, "TMPDIR": tmp, "LANG": "C.UTF-8",
		"GOTOOLCHAIN": "local", "GOENV": "off", "GOFLAGS": "", "GOWORK": "off",
		"GOCACHE": filepath.Join(home, "go-cache"), "GOPATH": filepath.Join(home, "go"), "CI": "1", "CUSTOM": "literal value"}
	if !reflect.DeepEqual(envMap(got), want) {
		t.Fatalf("environment = %#v, want %#v", envMap(got), want)
	}
	if !reflect.DeepEqual(got, MinimalEnv(home, tmp, extras)) {
		t.Fatal("environment is not deterministic")
	}
}
