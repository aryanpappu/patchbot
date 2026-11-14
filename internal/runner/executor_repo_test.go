//go:build unix

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/testpath"
)

func repoImage(c string) string { return "example.test/img:1@sha256:" + strings.Repeat(c, 64) }

func loadRepoConfig(t *testing.T, bin, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "patchbot.toml")
	content := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n%s", bin, filepath.Join(dir, "data"), body)
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

const repoTables = `
[repos."owner/go"]
commands={test=["go","test","./..."]}
[repos."owner/php"]
image="` + "example.test/php:8@sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `"
path=["/opt/php/bin","/usr/bin"]
commands={test=["phpunit"]}
`

func TestPerRepoImageChangesContainerArgv(t *testing.T) {
	bin := testpath.Dir(t)
	log := filepath.Join(t.TempDir(), "argv")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\n", log)
	// #nosec G306 -- executable fixture needs its owner execute bit.
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	goImage := repoImage("a")
	cfg := loadRepoConfig(t, bin, "[executor]\nkind=\"container\"\nimage=\""+goImage+"\"\n"+repoTables)
	exec := defaultExecutor(cfg)
	for _, repo := range []string{"owner/go", "owner/php"} {
		ws := t.TempDir()
		e, err := exec(ws, cfg.RepoExecutor(repo))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Run(t.Context(), execx.Spec{Argv: []string{"tool"}, Dir: ws}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(log) // #nosec G304 -- test-owned file.
	if err != nil {
		t.Fatal(err)
	}
	var runs []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "create ") {
			runs = append(runs, line)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 container creates, got %d:\n%s", len(runs), data)
	}
	if !strings.Contains(runs[0], " "+goImage+" tool") || strings.Contains(runs[0], "php") {
		t.Errorf("go repo argv: %s", runs[0])
	}
	if !strings.Contains(runs[1], " example.test/php:8@sha256:b") || strings.Contains(runs[1], goImage) || !strings.Contains(runs[1], "PATH=/opt/php/bin:/usr/bin") {
		t.Errorf("php repo argv: %s", runs[1])
	}
}

func TestRepoImageOverridesGlobalAndFallsBack(t *testing.T) {
	cfg := loadRepoConfig(t, "/usr/bin", "[executor]\nkind=\"container\"\nimage=\""+repoImage("a")+"\"\n"+repoTables)
	if got := cfg.RepoExecutor("owner/go"); got.Image != repoImage("a") || got.ImageSource != "executor.image" || got.Path[0] != "/usr/local/go/bin" {
		t.Errorf("fallback = %+v", got)
	}
	if got := cfg.RepoExecutor("owner/php"); !strings.HasPrefix(got.Image, "example.test/php:8@sha256:b") || got.ImageSource != `repos."owner/php".image` || got.Path[0] != "/opt/php/bin" {
		t.Errorf("override = %+v", got)
	}
	if opts := cfg.CommandPolicyOptions("owner/php"); !opts.InContainer || opts.TrustedPath[0] != "/opt/php/bin" {
		t.Errorf("policy = %+v", opts)
	}
}

func TestRunExecutorFreezesAndRecordsImage(t *testing.T) {
	cfg := loadRepoConfig(t, "/usr/bin", "[executor]\nkind=\"container\"\nimage=\""+repoImage("a")+"\"\n"+repoTables)
	if err := os.MkdirAll(cfg.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	run, err := db.CreateRun(t.Context(), store.Run{Repo: "owner/php", IssueNumber: 1, Mode: "fix", Provider: "anthropic", Status: store.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: cfg, db: db, redactor: secret.NewRedactor()}
	first, err := r.runExecutor(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	art, err := db.GetArtifact(t.Context(), run.ID, "executor", 0)
	if err != nil || !strings.Contains(art.Content, first.Image) || !strings.Contains(art.Content, "owner/php") {
		t.Fatalf("recorded evidence = %q, %v", art.Content, err)
	}
	// A later config with another image does not change the run.
	r.cfg = loadRepoConfig(t, "/usr/bin", "[executor]\nkind=\"container\"\nimage=\""+repoImage("c")+"\"\n[repos.\"owner/php\"]\nimage=\""+repoImage("d")+"\"\ncommands={test=[\"x\"]}\n")
	again, err := r.runExecutor(t.Context(), run)
	if err != nil || again.Image != first.Image {
		t.Fatalf("image not frozen: %+v %v", again, err)
	}
}

func TestLocalExecutorRecordsNothing(t *testing.T) {
	cfg := loadRepoConfig(t, "/usr/bin", repoTables)
	r := &Runner{cfg: cfg, redactor: secret.NewRedactor()}
	got, err := r.runExecutor(t.Context(), store.Run{ID: 1, Repo: "owner/php"})
	if err != nil || got.Image != "" {
		t.Fatalf("local = %+v %v", got, err)
	}
	e, err := defaultExecutor(cfg)(t.TempDir(), got)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*execx.LocalExecutor); !ok {
		t.Fatalf("executor is %T", e)
	}
}

func TestRunExecutorRefusesToFreezeRedactableSettings(t *testing.T) {
	token := "sk-abcdefghijklmnopqrstuvwx"
	cfg := loadRepoConfig(t, "/usr/bin", "[executor]\nkind=\"container\"\nimage=\"example.test/"+token+"@sha256:"+strings.Repeat("a", 64)+"\"\n"+
		"[repos.\"owner/php\"]\ncommands={test=[\"x\"]}\npath=[\"/opt/"+token+"/bin\"]\n")
	if err := os.MkdirAll(cfg.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	run, err := db.CreateRun(t.Context(), store.Run{Repo: "owner/php", IssueNumber: 1, Mode: "fix", Provider: "anthropic", Status: store.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	red := secret.NewRedactor()
	red.AddSecrets(secret.New(token))
	r := &Runner{cfg: cfg, db: db, redactor: red}
	if _, err := r.runExecutor(t.Context(), run); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("want a fail-closed error without the value, got %v", err)
	}
	if _, err := db.GetArtifact(t.Context(), run.ID, "executor", 0); err == nil {
		t.Fatal("a corrupted executor artifact was stored")
	}
}
