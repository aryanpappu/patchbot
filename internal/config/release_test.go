package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"

	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestReleaseExampleLoads(t *testing.T) {
	c, err := config.Load(config.Options{Path: filepath.Join("..", "..", "patchbot.example.toml"), LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Limits.TrustedPath, config.Default().Limits.TrustedPath) {
		t.Fatal("example trusted_path differs from defaults")
	}
	if c.Server.MaxQueuedRuns != 16 || !c.Defaults.CommentOnFailure {
		t.Fatal("example omits current defaults")
	}
}

func TestTrustedPathSourceAndValidation(t *testing.T) {
	e := newEnv(t)
	bin := testpath.Dir(t)
	e.local(t, "[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=["+strconvQuote(bin)+"]\n")
	c := mustLoad(t, e.options())
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := c.WriteEffective(&out); err != nil {
		t.Fatal(err)
	}
	if c.Source("limits.trusted_path").Kind != config.SourceFile || !strings.Contains(out.String(), "trusted_path =") {
		t.Fatal(out.String())
	}
	c.Limits.TrustedPath = []string{filepath.Join(c.DataDir(), "workspaces", "bin")}
	if err := c.Validate(); err == nil {
		t.Fatal("accepted data directory tool path")
	}
}

func strconvQuote(s string) string { return `"` + filepath.ToSlash(s) + `"` }

func TestPrivateDataDirectory(t *testing.T) {
	c := config.Default()
	c.Data.Dir = filepath.Join(t.TempDir(), "data")
	if err := c.EnsureDataDir(); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- deliberately unsafe permissions exercise rejection.
	if err := os.Chmod(c.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureDataDir(); err == nil {
		t.Fatal("accepted public data directory")
	}
}
