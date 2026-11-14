package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/repos"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestDoctorListsDashboardReposWithSource(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	file := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[data]\ndir=%q\n[repos.\"owner/repo\"]\ncommands={test=[\"go\",\"test\"]}\n", data)
	db, err := store.Open(t.Context(), filepath.Join(data, "patchbot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Repos = map[string]*config.Repo{"owner/repo": {}}
	reg, err := repos.New(t.Context(), cfg, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Add(t.Context(), "acme/widgets", repos.Settings{Commands: map[string][]string{"test": {"go", "test"}}}, "me", "ui"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	// #nosec G101 -- dummy credential.
	_, out, errs := runApp(t, file, map[string]string{"GITHUB_TOKEN": "dummy-github-token"}, "doctor")
	text := out + errs
	for _, want := range []string{"repo acme/widgets", "source dashboard", "repo owner/repo", "source config"} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, text)
		}
	}
}
