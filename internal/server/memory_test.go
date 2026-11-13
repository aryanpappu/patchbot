package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/memory"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

type memoryList struct {
	Enabled bool `json:"enabled"`
	Entries []struct {
		store.MemoryEntry
		Summary string `json:"summary"`
	} `json:"entries"`
}

func TestMemoryAPI(t *testing.T) {
	f := newFixture(t, nil, func(opts *Options) { opts.Token = secret.New("memory-ui-fixture-token") })
	pending, err := memory.AddLesson(t.Context(), f.db, nil, memory.LessonInput{Repo: "owner/repo", Text: "Model proposal.", Source: store.MemorySourceModel, Actor: "model:x", Channel: "harness"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := memory.AddLesson(t.Context(), f.db, nil, memory.LessonInput{Repo: "other/repo", Text: "Other repo.", Source: store.MemorySourceModel, Actor: "model:x", Channel: "harness"})
	if err != nil {
		t.Fatal(err)
	}
	list := func(query string) memoryList {
		t.Helper()
		w := f.request("GET", "/api/memory"+query, "")
		requireStatus(t, w, 200)
		var out memoryList
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := list("?repo=owner/repo&status=pending"); !got.Enabled || len(got.Entries) != 1 || got.Entries[0].ID != pending.ID {
		t.Fatalf("filtered list: %+v", got)
	}
	// Entries for repositories outside the allowlist are neither listed nor decidable.
	if got := list(""); len(got.Entries) != 1 || got.Entries[0].Repo != "owner/repo" {
		t.Fatalf("default list must be allowlisted only: %+v", got)
	}
	hash := func(h string) string { return `{"content_hash":"` + h + `"}` }
	tests := []struct {
		name, method, path, body string
		status                   int
		wantStatus               string
	}{
		{"approve without hash", "POST", "/api/memory/1/approve", `{}`, 400, ""},
		{"approve short hash", "POST", "/api/memory/1/approve", hash(pending.ContentHash[:7]), 400, ""},
		{"approve wrong hash", "POST", "/api/memory/1/approve", hash(other.ContentHash), 409, ""},
		{"approve shown hash", "POST", "/api/memory/1/approve", hash(pending.ContentHash), 200, store.MemoryApproved},
		{"approve twice conflicts", "POST", "/api/memory/1/approve", hash(pending.ContentHash), 409, ""},
		{"reject non-allowlisted", "POST", "/api/memory/2/reject", ``, 403, ""},
		{"forget non-allowlisted", "POST", "/api/memory/2/forget", hash(other.ContentHash), 403, ""},
		{"forget with wrong hash", "POST", "/api/memory/1/forget", hash(other.ContentHash), 409, ""},
		{"forget approved", "POST", "/api/memory/1/forget", ``, 200, store.MemoryRevoked},
		{"missing entry", "POST", "/api/memory/99/forget", ``, 404, ""},
		{"bad id", "POST", "/api/memory/x/forget", ``, 400, ""},
		{"get not allowed", "GET", "/api/memory/1/forget", ``, 405, ""},
		{"unknown body field", "POST", "/api/memory/1/forget", `{"force":true}`, 400, ""},
		{"bad status filter", "GET", "/api/memory?status=everything", ``, 400, ""},
		{"non-allowlisted repo filter", "GET", "/api/memory?repo=other/repo", ``, 400, ""},
		{"operator lesson", "POST", "/api/memory", `{"repo":"Owner/Repo","text":"Operator rule."}`, 201, store.MemoryApproved},
		{"duplicate operator lesson", "POST", "/api/memory", `{"repo":"owner/repo","text":"Operator rule."}`, 409, ""},
		{"operator lesson not allowlisted", "POST", "/api/memory", `{"repo":"other/repo","text":"x"}`, 400, ""},
		{"operator lesson too long", "POST", "/api/memory", `{"repo":"owner/repo","text":"` + strings.Repeat("x", 601) + `"}`, 400, ""},
		{"operator lesson with bidi override", "POST", "/api/memory", `{"repo":"owner/repo","text":"a\u202eb"}`, 400, ""},
	}
	for _, tc := range tests {
		w := f.request(tc.method, tc.path, tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s: status %d, want %d: %s", tc.name, w.Code, tc.status, w.Body.String())
		}
		requireStatus(t, w, tc.status)
		if tc.wantStatus != "" {
			var out struct{ Entry store.MemoryEntry }
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Entry.Status != tc.wantStatus {
				t.Fatalf("%s: %s", tc.name, w.Body.String())
			}
			if tc.method == "POST" && (out.Entry.DecidedBy == nil || *out.Entry.DecidedBy != "operator") {
				t.Fatalf("%s: actor not recorded: %s", tc.name, w.Body.String())
			}
		}
	}
	forgotten, err := f.db.GetMemory(t.Context(), pending.ID)
	if err != nil || forgotten.Text != "" {
		t.Fatalf("forget kept text: %+v %v", forgotten, err)
	}
	if got, _ := f.db.GetMemory(t.Context(), other.ID); got.Status != store.MemoryPending {
		t.Fatal("a non-allowlisted entry was decided")
	}
	actions, err := f.db.ListMemoryActions(t.Context(), pending.ID)
	if err != nil || len(actions) != 3 || actions[2].Channel != "ui" || actions[2].Actor != "operator" {
		t.Fatalf("audit: %+v %v", actions, err)
	}
}

// Rows that predate validation (or were imported) are escaped visibly.
func TestMemoryAPIEscapesHiddenText(t *testing.T) {
	f := newFixture(t, nil, nil)
	hidden := "Looks fine." + string(rune(0x202E)) + string(rune(0xE0041))
	if _, err := f.db.AddMemory(t.Context(), store.MemoryEntry{Repo: "owner/repo", Kind: store.MemoryLesson, Source: store.MemorySourceModel, Text: hidden, CreatedBy: "legacy"}, "harness"); err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", "/api/memory", "")
	requireStatus(t, w, 200)
	var out memoryList
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Entries) != 1 {
		t.Fatalf("%s %v", w.Body.String(), err)
	}
	want := "Looks fine." + `\u{202E}\u{E0041}`
	if out.Entries[0].Text != want || out.Entries[0].Summary != want {
		t.Fatalf("text not escaped: %q", out.Entries[0].Text)
	}
}

func TestMemoryAPIRequiresAuthAndSameOrigin(t *testing.T) {
	f := newFixture(t, nil, func(opts *Options) { opts.Token = secret.New("memory-ui-fixture-token") })
	if _, err := memory.AddLesson(t.Context(), f.db, nil, memory.LessonInput{Repo: "owner/repo", Text: "Model proposal.", Source: store.MemorySourceModel, Actor: "model:x", Channel: "harness"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, token, origin string
		want                              int
	}{
		{"list without token", "GET", "/api/memory", "", "", 401},
		{"approve without token", "POST", "/api/memory/1/approve", "", "", 401},
		{"wrong token", "POST", "/api/memory/1/approve", "wrong", "", 401},
		{"cross-origin approve", "POST", "/api/memory/1/approve", "memory-ui-fixture-token", "https://attacker.invalid", 403},
		{"cross-origin add", "POST", "/api/memory", "memory-ui-fixture-token", "https://attacker.invalid", 403},
	} {
		req := httptest.NewRequest(tc.method, "http://"+f.cfg.Server.Addr+tc.path, strings.NewReader(`{}`))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	if got, _ := f.db.GetMemory(t.Context(), 1); got.Status != store.MemoryPending {
		t.Fatal("unauthorized request changed memory")
	}
}
