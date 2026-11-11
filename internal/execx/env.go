package execx

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

// TrustedPath is independent of the caller's PATH and excludes the checkout.
// These directories must be controlled by the operator, as must their binaries.
const TrustedPath = "/usr/bin:/bin"

var errInvalidEnv = errors.New("invalid explicit environment entry")

// MinimalEnv constructs a child environment without reading os.Environ. home and
// tmp should be private absolute per-run directories, created by the caller.
// Extras can override ordinary variables, but cannot replace the fixed base
// settings or introduce secret-like names. Dependency downloads remain allowed;
// automatic toolchain downloads and ambient Go configuration are disabled.
func MinimalEnv(home, tmp string, extra map[string]string) []string {
	return minimalEnv(home, tmp, extra, TrustedPath)
}

func minimalEnv(home, tmp string, extra map[string]string, path string) []string {
	values := map[string]string{
		"PATH": path, "HOME": home, "TMPDIR": tmp, "LANG": "C.UTF-8",
		"GOTOOLCHAIN": "local", "GOENV": "off", "GOFLAGS": "", "GOWORK": "off",
		"GOCACHE": filepath.Join(home, "go-cache"), "GOPATH": filepath.Join(home, "go"), "CI": "1",
	}
	for name, value := range extra {
		if _, fixed := values[name]; !fixed && validEnv(name, value) {
			values[name] = value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		if validEnv(name, values[name]) {
			env = append(env, name+"="+values[name])
		}
	}
	return env
}

func validEnv(name, value string) bool {
	if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
		return false
	}
	upper := strings.ToUpper(name)
	for _, word := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD"} {
		if strings.Contains(upper, word) {
			return false
		}
	}
	for _, prefix := range []string{"AWS_", "GH_", "GITHUB_", "ANTHROPIC_", "OPENAI_", "GEMINI_", "GOOGLE_"} {
		if strings.HasPrefix(upper, prefix) {
			return false
		}
	}
	return true
}

func scrubEnv(env []string) ([]string, error) {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) {
			return nil, errInvalidEnv
		}
		if validEnv(name, value) {
			clean = append(clean, entry)
		}
	}
	return clean, nil
}
