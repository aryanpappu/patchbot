package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/execx"
)

func TestDefaultExecutorFollowsConfig(t *testing.T) {
	dir := t.TempDir()
	load := func(extra string) *config.Config {
		t.Helper()
		path := filepath.Join(dir, "patchbot.toml")
		content := "[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[\"/usr/bin\",\"/bin\"]\n[data]\ndir=" + `"` + filepath.Join(dir, "data") + `"` + "\n" + extra
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(config.Options{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	workspace := t.TempDir()
	executor, err := defaultExecutor(load(""))(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := executor.(*execx.LocalExecutor); !ok {
		t.Fatalf("default executor is %T, want local", executor)
	}
	image := "golang@sha256:" + strings.Repeat("0", 64)
	_, err = defaultExecutor(load("[executor]\nkind=\"container\"\nruntime=\"podman\"\nimage=\"" + image + "\"\n"))(workspace)
	if _, statErr := os.Stat("/usr/bin/podman"); statErr == nil {
		if err != nil {
			t.Fatalf("container executor with installed podman: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "executor.runtime podman") {
		t.Fatalf("missing runtime error = %v", err)
	}
}
