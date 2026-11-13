package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/memory"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestMemoryCommands(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	file := fmt.Sprintf(`[defaults]
approvers = ["owner"]
[providers.anthropic]
api_key_env = ""
default_model = "m"
price_free = true
[approval]
local_actor = "cli-owner"
[data]
dir = %q
[repos."owner/repo"]
commands = { test = ["go", "test", "./..."] }
`, data)
	if err := os.WriteFile(filepath.Join(dir, "patchbot.toml"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		app := &cli.App{Stdout: &out, Stderr: &errOut, Config: config.Options{Dir: dir, LookupEnv: func(string) (string, bool) { return "", false },
			UserConfigDir: func() (string, error) { return filepath.Join(dir, "user-config"), nil },
			UserHomeDir:   func() (string, error) { return filepath.Join(dir, "home"), nil }}}
		code := app.Run(args)
		return code, out.String(), errOut.String()
	}
	// Seed a model proposal the way the runner would.
	if code, _, stderr := run("memory", "list"); code != 0 {
		t.Fatalf("list on empty store: %s", stderr)
	}
	db, err := store.Open(t.Context(), filepath.Join(data, "patchbot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	long := "Model proposal\nwith two lines. " + strings.Repeat("Hidden tail beyond the list summary. ", 5) + "FINAL CLAUSE."
	seeded, err := memory.AddLesson(t.Context(), db, nil, memory.LessonInput{Repo: "owner/repo", Text: long, Source: store.MemorySourceModel, Actor: "model:m", Channel: "harness"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.AddLesson(t.Context(), db, nil, memory.LessonInput{Repo: "other/repo", Text: "Not allowlisted.", Source: store.MemorySourceModel, Actor: "model:m", Channel: "harness"}); err != nil {
		t.Fatal(err)
	}
	// A legacy row with hidden characters is escaped visibly by show.
	hidden := "Visible." + string(rune(0x202E)) + "neddih"
	if _, err := db.AddMemory(t.Context(), store.MemoryEntry{Repo: "owner/repo", Kind: store.MemoryLesson, Source: store.MemorySourceModel, Text: hidden, CreatedBy: "legacy"}, "harness"); err != nil {
		t.Fatal(err)
	}
	hash := seeded.ContentHash
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args       []string
		code       int
		out, notIn string
	}{
		{[]string{"memory", "list", "--status", "pending"}, 0, "Model proposal with two lines.", "FINAL CLAUSE"},
		{[]string{"memory", "list"}, 0, "owner/repo", "other/repo"},
		{[]string{"memory", "list", "--repo", "other/repo"}, 1, "", ""},
		{[]string{"memory", "show", "1"}, 0, "FINAL CLAUSE.", ""},
		{[]string{"memory", "show", "1"}, 0, "Content hash: " + hash, ""},
		{[]string{"memory", "show", "2"}, 1, "", ""},
		{[]string{"memory", "show", "3"}, 0, `Visible.\u{202E}neddih`, ""},
		{[]string{"memory", "approve", "1"}, 2, "", ""},
		{[]string{"memory", "approve", "1", "--hash", hash[:7]}, 2, "", ""},
		{[]string{"memory", "approve", "1", "--hash", "deadbeefdeadbeef"}, 1, "", ""},
		{[]string{"memory", "approve", "2", "--hash", "deadbeef"}, 1, "", ""},
		{[]string{"memory", "approve", "1", "--hash", hash[:8]}, 0, "Memory 1: approved", ""},
		{[]string{"memory", "approve", "1", "--hash", hash[:8]}, 1, "", ""},
		{[]string{"memory", "add", "--repo", "Owner/Repo", "--text", "Operator rule."}, 0, "Memory 4: approved lesson for owner/repo", ""},
		{[]string{"memory", "add", "--repo", "owner/repo", "--text", "Operator rule."}, 1, "", ""},
		{[]string{"memory", "add", "--repo", "other/repo", "--text", "x"}, 1, "", ""},
		{[]string{"memory", "forget", "1"}, 0, "Memory 1: revoked", ""},
		{[]string{"memory", "list", "--repo", "owner/repo"}, 0, "forgotten; content hash", "Model proposal"},
		{[]string{"memory", "reject", "3", "--hash", "deadbeef"}, 1, "", ""},
		{[]string{"memory", "reject", "99"}, 1, "", ""},
		{[]string{"memory", "list", "--status", "everything"}, 2, "", ""},
		{[]string{"memory", "approve"}, 2, "", ""},
		{[]string{"memory", "approve", "0"}, 2, "", ""},
		{[]string{"memory", "add", "--repo", "owner/repo"}, 2, "", ""},
		{[]string{"memory", "purge", "1"}, 2, "", ""},
		{[]string{"memory"}, 2, "", ""},
		{[]string{"memory", "--help"}, 0, "Usage:", ""},
	}
	for _, tc := range tests {
		code, stdout, stderr := run(tc.args...)
		if code != tc.code || !strings.Contains(stdout, tc.out) || (tc.notIn != "" && strings.Contains(stdout, tc.notIn)) {
			t.Fatalf("%v: code %d\nstdout %s\nstderr %s", tc.args, code, stdout, stderr)
		}
	}
	file = strings.Replace(file, "[data]", "[memory]\nenabled = false\n[data]", 1)
	if err := os.WriteFile(filepath.Join(dir, "patchbot.toml"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ := run("memory", "list"); code != 0 || !strings.Contains(stdout, "Memory is disabled") {
		t.Fatalf("disabled notice: %d %s", code, stdout)
	}
}
