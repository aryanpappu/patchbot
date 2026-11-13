package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
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

type listOnlyStore struct {
	store.Store
	runs []store.Run
	err  error
}

func (s listOnlyStore) ListRuns(_ context.Context, f store.RunFilter) (store.RunPage, error) {
	if !slices.Equal(f.Statuses, []store.Status{store.StatusQueued, store.StatusRunning}) {
		return store.RunPage{}, errors.New("unexpected filter")
	}
	return store.RunPage{Runs: s.runs}, s.err
}

func TestSweepContainersOnlyWhenIdle(t *testing.T) {
	for _, test := range []struct {
		name  string
		db    listOnlyStore
		swept bool
	}{
		{"idle", listOnlyStore{}, true},
		{"active run", listOnlyStore{runs: []store.Run{{ID: 1, Status: store.StatusRunning}}}, false},
		{"store error", listOnlyStore{err: errors.New("locked")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			r := &Runner{cfg: config.Default(), db: test.db, redactor: secret.NewRedactor(), opts: Options{ContainerSweep: func(context.Context) (int, error) {
				calls++
				return 2, nil
			}}}
			r.sweepContainers(t.Context())
			if (calls == 1) != test.swept {
				t.Fatalf("sweep calls = %d, want swept=%t", calls, test.swept)
			}
		})
	}
	// Local executor without an explicit hook never sweeps (and never touches the store).
	r := &Runner{cfg: config.Default(), db: listOnlyStore{err: errors.New("must not be called")}, redactor: secret.NewRedactor()}
	r.sweepContainers(t.Context())
}
