package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

const addBody = `{"name":"acme/widgets","settings":{"commands":{"test":["go","test","./..."]},"test_command":"test","protected":["secrets"]}}`

type repoReply struct {
	Repo struct {
		Name    string
		Source  string
		Version int64
	}
	Repos []struct {
		Name    string
		Source  string
		Version int64
	}
	Audit []store.RepoAudit
	Error struct{ Kind string }
}

func decodeRepo(t *testing.T, body []byte) repoReply {
	t.Helper()
	var r repoReply
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRepoAPILifecycle(t *testing.T) {
	f := newFixture(t, nil, nil)
	list := decodeRepo(t, f.request("GET", "/api/repos", "").Body.Bytes())
	if len(list.Repos) != 1 || list.Repos[0].Name != "owner/repo" || list.Repos[0].Source != "config" {
		t.Fatalf("list: %+v", list)
	}
	w := f.request("POST", "/api/repos", addBody)
	requireStatus(t, w, 201)
	if r := decodeRepo(t, w.Body.Bytes()); r.Repo.Version != 1 || r.Repo.Source != "dashboard" {
		t.Fatalf("add: %s", w.Body.String())
	}
	requireStatus(t, f.request("POST", "/api/repos", addBody), 409)
	// A stale version is a 409, a missing one is 428.
	edit := `{"version":%d,"settings":{"commands":{"test":["make","check"]},"test_command":"test"}}`
	requireStatus(t, f.request("PUT", "/api/repos/acme/widgets", fmt.Sprintf(edit, 5)), 409)
	requireStatus(t, f.request("PUT", "/api/repos/acme/widgets", `{"settings":{}}`), 428)
	w = f.request("PUT", "/api/repos/acme/widgets", fmt.Sprintf(edit, 1))
	requireStatus(t, w, 200)
	if r := decodeRepo(t, w.Body.Bytes()); r.Repo.Version != 2 {
		t.Fatalf("edit: %s", w.Body.String())
	}
	got := decodeRepo(t, f.request("GET", "/api/repos/acme/widgets", "").Body.Bytes())
	if got.Repo.Version != 2 {
		t.Fatalf("get: %+v", got)
	}
	requireStatus(t, f.request("DELETE", "/api/repos/acme/widgets", ""), 200)
	requireStatus(t, f.request("GET", "/api/repos/acme/widgets", ""), 404)
	w = f.request("GET", "/api/repos/acme/widgets/audit", "")
	requireStatus(t, w, 200)
	audit := decodeRepo(t, w.Body.Bytes()).Audit
	if len(audit) != 3 || audit[0].Action != "remove" || audit[2].Action != "add" || audit[0].Actor != "operator" || audit[0].Channel != "ui" {
		t.Fatalf("audit: %+v", audit)
	}
	requireStatus(t, f.request("GET", "/api/repos/nobody/nothing/audit", ""), 404)
}

func TestRepoAPIRefusals(t *testing.T) {
	f := newFixture(t, nil, nil)
	// Config-file repositories are read-only and cannot be shadowed.
	w := f.request("POST", "/api/repos", `{"name":"owner/repo","settings":{}}`)
	requireStatus(t, w, 409)
	if decodeRepo(t, w.Body.Bytes()).Error.Kind != "name_in_config" {
		t.Fatal(w.Body.String())
	}
	requireStatus(t, f.request("PUT", "/api/repos/owner/repo", `{"version":1,"settings":{}}`), 403)
	requireStatus(t, f.request("DELETE", "/api/repos/owner/repo", ""), 403)
	for name, body := range map[string]string{
		"shell string command": `{"name":"acme/a","settings":{"commands":{"test":"go test ./..."}}}`,
		"shell in program":     `{"name":"acme/a","settings":{"commands":{"test":["go test"]}}}`,
		"test cmd missing":     `{"name":"acme/a","settings":{"commands":{"build":["make"]},"test_command":"test"}}`,
		"bad name":             `{"name":"nope","settings":{}}`,
		"dotdot protected":     `{"name":"acme/a","settings":{"protected":["../x"]}}`,
		"unknown field":        `{"name":"acme/a","settings":{"shell":"sh"}}`,
		"credential argument":  `{"name":"acme/a","settings":{"commands":{"t":["x","ghp_` + strings.Repeat("a1B2", 9) + `"]}}}`,
	} {
		w := f.request("POST", "/api/repos", body)
		if w.Code != 400 {
			t.Errorf("%s: status %d: %s", name, w.Code, w.Body.String())
		}
	}
	if strings.Contains(f.request("GET", "/api/repos", "").Body.String(), "acme/a") {
		t.Fatal("a refused repository was stored")
	}
	huge := `{"name":"acme/a","settings":{"commands":{"t":["` + strings.Repeat("x", 2<<20) + `"]}}}`
	requireStatus(t, f.request("POST", "/api/repos", huge), 413)
	requireStatus(t, f.request("PATCH", "/api/repos", ""), 405)
}

func TestNewRunUsesAddedRepoWithoutRestart(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.withRepo()
	run := `{"repo":"acme/widgets","issue":3,"mode":"investigate","dry_run":true}`
	requireStatus(t, f.request("POST", "/api/runs", run), 400)
	requireStatus(t, f.request("POST", "/api/repos", addBody), 201)
	w := f.request("POST", "/api/runs", run)
	requireStatus(t, w, 202)
	var out struct{ Run store.Run }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Run.Repo != "acme/widgets" {
		t.Fatalf("run: %s %v", w.Body.String(), err)
	}
	// The run froze the settings it started with.
	art, err := f.db.GetArtifact(t.Context(), out.Run.ID, "repo_settings", 0)
	if err != nil || !strings.Contains(art.Content, "secrets") {
		t.Fatalf("frozen settings: %q %v", art.Content, err)
	}
}

func TestRepoDeleteRefusedWithActiveRun(t *testing.T) {
	f := newFixture(t, nil, nil)
	requireStatus(t, f.request("POST", "/api/repos", addBody), 201)
	run, err := f.db.CreateRun(t.Context(), store.Run{Repo: "acme/widgets", IssueNumber: 9, Status: store.StatusAwaitingApproval, Provider: "anthropic", RefineModel: "m", CodeModel: "m", Mode: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	w := f.request("DELETE", "/api/repos/acme/widgets", "")
	requireStatus(t, w, 409)
	if decodeRepo(t, w.Body.Bytes()).Error.Kind != "repo_busy" {
		t.Fatal(w.Body.String())
	}
	if err := f.db.SetRunStatus(t.Context(), run.ID, store.StatusAwaitingApproval, store.StatusCancelled, store.RunPatch{Phase: store.PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request("DELETE", "/api/repos/acme/widgets", ""), 200)
	if _, err := f.db.GetRun(t.Context(), run.ID); err != nil {
		t.Fatal("history must stay")
	}
}

func TestRepoAPIRequiresAuthentication(t *testing.T) {
	f := newFixture(t, nil, func(o *Options) { o.Token = secret.New("ui-token-for-repos") })
	for _, c := range [][2]string{
		{"GET", "/api/repos"}, {"POST", "/api/repos"}, {"GET", "/api/repos/owner/repo"},
		{"PUT", "/api/repos/acme/widgets"}, {"DELETE", "/api/repos/acme/widgets"}, {"GET", "/api/repos/owner/repo/audit"},
	} {
		w := f.requestWith(c[0], c[1], addBody, "")
		if w.Code != 401 {
			t.Errorf("%s %s without token: %d", c[0], c[1], w.Code)
		}
	}
	requireStatus(t, f.request("GET", "/api/repos", ""), 200)

	a := newAccessFixture(t, nil)
	for _, c := range [][2]string{{"GET", "/api/repos"}, {"POST", "/api/repos"}, {"PUT", "/api/repos/acme/widgets"}, {"DELETE", "/api/repos/acme/widgets"}, {"GET", "/api/repos/owner/repo/audit"}} {
		if w := a.do(c[0], c[1], addBody, ""); w.Code != 401 {
			t.Errorf("access %s %s without JWT: %d", c[0], c[1], w.Code)
		}
	}
}

func (f *fixture) requestWith(method, path, body, token string) *httptest.ResponseRecorder {
	f.t.Helper()
	saved := f.token
	f.token = token
	defer func() { f.token = saved }()
	return f.request(method, path, body)
}

func TestRepoAuditRecordsAccessEmail(t *testing.T) {
	a := newAccessFixture(t, nil)
	jwt := a.token(nil)
	requireStatus(t, a.do("POST", "/api/repos", addBody, jwt), 201)
	requireStatus(t, a.do("PUT", "/api/repos/acme/widgets", `{"version":1,"settings":{"commands":{"test":["make"]}}}`, jwt), 200)
	rows, err := a.db.ListRepoAudit(t.Context(), "acme/widgets", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("audit: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row.Actor != accessOwner || row.Channel != "ui" {
			t.Fatalf("audit row: %+v", row)
		}
	}
}

func TestRepoSettingsXSSStaysInert(t *testing.T) {
	f := newFixture(t, nil, nil)
	payload := `<script>alert(1)</script><img src=x onerror=alert(1)>`
	body, _ := json.Marshal(map[string]any{"name": "acme/xss", "settings": map[string]any{
		"commands": map[string]any{"test": []string{"echo", payload}}, "protected": []string{payload}}})
	requireStatus(t, f.request("POST", "/api/repos", string(body)), 201)
	w := f.request("GET", "/api/repos", "")
	if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<img") {
		t.Fatalf("markup in JSON response: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", w.Header())
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("CSP missing")
	}
	// The payload round-trips as data.
	var got struct {
		Repos []struct {
			Name     string
			Settings struct{ Protected []string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range got.Repos {
		found = found || (r.Name == "acme/xss" && len(r.Settings.Protected) == 1 && r.Settings.Protected[0] == payload)
	}
	if !found {
		t.Fatal("payload did not round-trip")
	}
}

func TestRepoAPIOversizedSettingsAndBlankPath(t *testing.T) {
	f := newFixture(t, nil, nil)
	cmds := []string{}
	for i := range 20 {
		args := []string{`"go"`}
		for range 60 {
			args = append(args, `"`+strings.Repeat("x", 100)+`"`)
		}
		cmds = append(cmds, fmt.Sprintf(`"c%d":[%s]`, i, strings.Join(args, ",")))
	}
	requireStatus(t, f.request("POST", "/api/repos", `{"name":"acme/big","settings":{"commands":{`+strings.Join(cmds, ",")+`}}}`), 413)
	requireStatus(t, f.request("POST", "/api/repos", `{"name":"acme/blank","settings":{"commands":{"test":["go","test"]},"path":[],"image":""}}`), 201)
}
