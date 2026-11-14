package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

// issueGitHub is a fake GitHub that can also open and list issues.
type issueGitHub struct {
	*fakeGitHub
	imu    sync.Mutex
	issues []github.Issue
	// createErr fails CreateIssue before anything is created.
	createErr error
	// lostReply creates the issue but reports a transport failure (the crash window).
	lostReply bool
	listErr   error
	lists     int
	attempts  int
}

func newIssueGitHub() *issueGitHub { return &issueGitHub{fakeGitHub: &fakeGitHub{}} }

func (g *issueGitHub) CreateIssue(_ context.Context, repo github.RepoRef, in github.NewIssue) (github.Issue, error) {
	g.imu.Lock()
	defer g.imu.Unlock()
	g.attempts++
	if g.createErr != nil {
		return github.Issue{}, g.createErr
	}
	n := len(g.issues) + 1
	issue := github.Issue{Number: n, NodeID: "I_node" + strconv.Itoa(n), Title: in.Title, Body: in.Body, State: "open",
		HTMLURL: "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(n)}
	g.issues = append(g.issues, issue)
	if g.lostReply {
		return github.Issue{}, errors.New("CreateIssue: API request failed")
	}
	return issue, nil
}

func (g *issueGitHub) ListIssuesSince(_ context.Context, _ github.RepoRef, _ time.Time, page int) ([]github.Issue, error) {
	g.imu.Lock()
	defer g.imu.Unlock()
	g.lists++
	if g.listErr != nil {
		return nil, g.listErr
	}
	if page > 1 {
		return nil, nil
	}
	out := make([]github.Issue, 0, len(g.issues))
	for i := len(g.issues) - 1; i >= 0; i-- {
		out = append(out, g.issues[i])
	}
	return out, nil
}

func (g *issueGitHub) GetIssue(_ context.Context, _ github.RepoRef, n int) (github.Issue, error) {
	g.imu.Lock()
	defer g.imu.Unlock()
	if n < 1 || n > len(g.issues) {
		return github.Issue{}, &github.ErrNotFound{APIError: github.APIError{StatusCode: 404, Message: "Not Found"}}
	}
	return g.issues[n-1], nil
}

func (g *issueGitHub) count() int {
	g.imu.Lock()
	defer g.imu.Unlock()
	return len(g.issues)
}

func newIssueFixture(t *testing.T, p *fake.Provider) (*fixture, *issueGitHub) {
	t.Helper()
	g := newIssueGitHub()
	f := newFixture(t, p, func(o *Options) { o.Runner.GitHub = g })
	f.gh = g.fakeGitHub
	return f, g
}

const goodKey = "0b0f4a52-7a38-4a54-9d1e-5d0c8a1f3c11"

func issueJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	body := map[string]any{"repo": "owner/repo", "title": "Add dark mode", "body": "Please add a dark theme.", "idempotency_key": goodKey}
	for k, v := range fields {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type issueResponse struct {
	Issue struct {
		Repo        string `json:"repo"`
		Number      int64  `json:"number"`
		NodeID      string `json:"node_id"`
		HTMLURL     string `json:"html_url"`
		RequestedBy string `json:"requested_by"`
		Created     bool   `json:"created"`
	} `json:"issue"`
	Run   *store.Run `json:"run"`
	Error struct{ Kind, Message string }
}

func decodeIssue(t *testing.T, w *httptest.ResponseRecorder) issueResponse {
	t.Helper()
	var out issueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON %q: %v", w.Body.String(), err)
	}
	return out
}

func TestCreateIssueWritesMarkerFooterAndRecordsActor(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	w := f.request("POST", "/api/issues", issueJSON(t, nil))
	requireStatus(t, w, http.StatusCreated)
	out := decodeIssue(t, w)
	if out.Issue.Number != 1 || out.Issue.NodeID != "I_node1" || !out.Issue.Created || out.Issue.RequestedBy != "operator" || out.Run != nil {
		t.Fatalf("response: %+v", out)
	}
	if out.Issue.HTMLURL != "https://github.com/owner/repo/issues/1" {
		t.Fatalf("url %q", out.Issue.HTMLURL)
	}
	if g.count() != 1 {
		t.Fatalf("issues created: %d", g.count())
	}
	issue := g.issues[0]
	marker := "<!-- patchbot:issue-created v1 key=" + goodKey + " -->"
	if issue.Title != "Add dark mode" || !strings.HasPrefix(issue.Body, "Please add a dark theme.") || !strings.HasSuffix(issue.Body, marker) ||
		!strings.Contains(issue.Body, "Created from the patchbot dashboard by `operator`") {
		t.Fatalf("issue content: %q", issue.Body)
	}
	row, err := f.db.DashboardIssue(t.Context(), goodKey)
	if err != nil || row.Pending || row.Number != 1 || row.NodeID != "I_node1" || row.Actor != "operator" || row.Repo != "owner/repo" || row.RunID != 0 {
		t.Fatalf("row: %+v %v", row, err)
	}
	if byNumber, ok, err := f.db.DashboardIssueByNumber(t.Context(), "owner/repo", 1); err != nil || !ok || byNumber.IdempotencyKey != goodKey {
		t.Fatalf("lookup by number: %+v %v %v", byNumber, ok, err)
	}
	if !github.HasPatchbotMarker(issue.Body) {
		t.Fatal("issue body must carry the patchbot marker")
	}
}

func TestCreateIssueIsIdempotent(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	first := decodeIssue(t, f.request("POST", "/api/issues", issueJSON(t, nil)))
	w := f.request("POST", "/api/issues", issueJSON(t, nil))
	requireStatus(t, w, http.StatusOK)
	second := decodeIssue(t, w)
	if second.Issue.Number != first.Issue.Number || second.Issue.Created || g.count() != 1 || g.attempts != 1 {
		t.Fatalf("retry created again: %+v attempts=%d", second, g.attempts)
	}
	// The same key with different content is refused, not silently reused.
	w = f.request("POST", "/api/issues", issueJSON(t, map[string]any{"title": "Something else"}))
	requireStatus(t, w, http.StatusUnprocessableEntity)
	if decodeIssue(t, w).Error.Kind != "idempotency_conflict" || g.count() != 1 {
		t.Fatal("conflicting reuse of a key")
	}
	// A different key opens a different issue.
	w = f.request("POST", "/api/issues", issueJSON(t, map[string]any{"idempotency_key": "another-key-1234"}))
	requireStatus(t, w, http.StatusCreated)
	if g.count() != 2 {
		t.Fatal("second key should create a second issue")
	}
}

func TestCreateIssueConcurrentSameKeyCreatesOnce(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- f.request("POST", "/api/issues", issueJSON(t, nil)).Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 && code != 201 && code != 409 {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if g.count() != 1 {
		t.Fatalf("created %d issues", g.count())
	}
}

func TestCreateIssueCrashWindowRecoversByMarker(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	g.lostReply = true
	w := f.request("POST", "/api/issues", issueJSON(t, nil))
	requireStatus(t, w, http.StatusBadGateway)
	if decodeIssue(t, w).Error.Kind != "issue_state_unknown" {
		t.Fatalf("body: %s", w.Body.String())
	}
	row, err := f.db.DashboardIssue(t.Context(), goodKey)
	if err != nil || !row.Pending {
		t.Fatalf("expected a pending row: %+v %v", row, err)
	}
	g.lostReply = false
	// While the first request may still be in flight a retry must wait.
	w = f.request("POST", "/api/issues", issueJSON(t, nil))
	requireStatus(t, w, http.StatusConflict)
	if decodeIssue(t, w).Error.Kind != "issue_creation_in_progress" || g.count() != 1 {
		t.Fatalf("early retry: %s", w.Body.String())
	}
	f.server.issueGrace = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	w = f.request("POST", "/api/issues", issueJSON(t, nil))
	requireStatus(t, w, http.StatusOK)
	out := decodeIssue(t, w)
	if out.Issue.Number != 1 || out.Issue.Created || g.count() != 1 {
		t.Fatalf("recovery created a duplicate: %+v count=%d", out, g.count())
	}
	if row, _ := f.db.DashboardIssue(t.Context(), goodKey); row.Pending || row.Number != 1 || row.NodeID != "I_node1" {
		t.Fatalf("row not completed: %+v", row)
	}
}

func TestCreateIssueCrashWindowCreatesWhenSearchFindsNothing(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	g.createErr = &github.APIError{StatusCode: 502, Message: "bad gateway"}
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, nil)), http.StatusBadGateway)
	g.createErr = nil
	f.server.issueGrace = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, nil)), http.StatusCreated)
	if g.count() != 1 || g.lists == 0 {
		t.Fatalf("count=%d lists=%d", g.count(), g.lists)
	}
}

func TestCreateIssueUnverifiableRecoveryNeverCreates(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	g.lostReply = true
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, nil)), http.StatusBadGateway)
	g.lostReply, g.listErr = false, errors.New("list failed")
	f.server.issueGrace = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	w := f.request("POST", "/api/issues", issueJSON(t, nil))
	requireStatus(t, w, http.StatusBadGateway)
	if decodeIssue(t, w).Error.Kind != "issue_state_unknown" || g.attempts != 1 {
		t.Fatalf("a failed search must not create: attempts=%d %s", g.attempts, w.Body.String())
	}
}

func TestCreateIssueStartRunUsesRunPathAndNeverApproves(t *testing.T) {
	f, g := newIssueFixture(t, fake.New("anthropic", specStep()))
	f.withRepo()
	w := f.request("POST", "/api/issues", issueJSON(t, map[string]any{"start_run": true, "mode": "refine", "dry_run": true}))
	requireStatus(t, w, http.StatusCreated)
	out := decodeIssue(t, w)
	if out.Run == nil || out.Run.IssueNumber != out.Issue.Number || out.Run.Repo != "owner/repo" {
		t.Fatalf("run: %s", w.Body.String())
	}
	if w.Header().Get("Location") != fmt.Sprintf("/api/runs/%d", out.Run.ID) {
		t.Fatalf("location %q", w.Header().Get("Location"))
	}
	waiting := f.waitFor(out.Run.ID, store.StatusAwaitingApproval)
	if waiting.Trigger != "ui" || waiting.Mode != "refine" || !waiting.DryRun {
		t.Fatalf("run not started like POST /api/runs: %+v", waiting)
	}
	if approvals, err := f.db.ListApprovals(t.Context(), out.Run.ID); err != nil || len(approvals) != 0 {
		t.Fatalf("creating an issue must not approve: %+v %v", approvals, err)
	}
	row, _ := f.db.DashboardIssue(t.Context(), goodKey)
	if row.RunID != out.Run.ID {
		t.Fatalf("run not linked: %+v", row)
	}
	// Retry: same issue, same run, no second run.
	w = f.request("POST", "/api/issues", issueJSON(t, map[string]any{"start_run": true, "mode": "refine", "dry_run": true}))
	requireStatus(t, w, http.StatusOK)
	again := decodeIssue(t, w)
	if again.Run == nil || again.Run.ID != out.Run.ID || g.count() != 1 {
		t.Fatalf("retry: %s", w.Body.String())
	}
	if page, _ := f.db.ListRuns(t.Context(), store.RunFilter{}); len(page.Runs) != 1 {
		t.Fatalf("runs: %d", len(page.Runs))
	}
	// The run detail names the dashboard requester.
	w = f.request("GET", fmt.Sprintf("/api/runs/%d", out.Run.ID), "")
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"requested_by":"operator"`) {
		t.Fatalf("detail lacks requester: %s", w.Body.String())
	}
}

func TestCreateIssueResumesRunAfterEarlierRunFailure(t *testing.T) {
	f, g := newIssueFixture(t, fake.New("anthropic", specStep()))
	f.withRepo()
	req := issueJSON(t, map[string]any{"start_run": true, "mode": "refine", "dry_run": true})
	// Pre-create the row as if the first request died after the issue existed.
	g.issues = append(g.issues, github.Issue{Number: 1, Title: "x", State: "open"})
	in := issueRequest{Repo: "owner/repo", Title: "Add dark mode", Body: "Please add a dark theme.", StartRun: true, IdempotencyKey: goodKey, Mode: "refine", DryRun: true}
	if _, _, err := f.db.BeginDashboardIssue(t.Context(), store.DashboardIssue{IdempotencyKey: goodKey, RequestHash: in.hash(), Repo: "owner/repo", Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CompleteDashboardIssue(t.Context(), goodKey, 1, "I_node1", "https://github.com/owner/repo/issues/1", time.Now()); err != nil {
		t.Fatal(err)
	}
	w := f.request("POST", "/api/issues", req)
	requireStatus(t, w, http.StatusOK)
	out := decodeIssue(t, w)
	if out.Run == nil || out.Issue.Created || g.attempts != 0 {
		t.Fatalf("resume: %s attempts=%d", w.Body.String(), g.attempts)
	}
	f.waitFor(out.Run.ID, store.StatusAwaitingApproval)
}

func TestCreateIssueRefusesNotAllowlistedRepo(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	for _, repo := range []string{"other/repo", "", "owner/repo/extra", "../etc"} {
		requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, map[string]any{"repo": repo})), http.StatusBadRequest)
	}
	if g.attempts != 0 {
		t.Fatal("GitHub must not be called for a repo outside the allowlist")
	}
	if _, err := f.db.DashboardIssue(t.Context(), goodKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a row was recorded: %v", err)
	}
	// Case differences resolve like POST /api/runs.
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, map[string]any{"repo": " Owner/Repo "})), http.StatusCreated)
}

func TestCreateIssueValidation(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	long := strings.Repeat("é", 257)
	for name, fields := range map[string]map[string]any{
		"empty title":      {"title": "   "},
		"missing title":    {"title": nil},
		"long title":       {"title": long},
		"title newline":    {"title": "a\nb"},
		"title tab":        {"title": "a\tb"},
		"control in title": {"title": "a\x07b"},
		"nul in body":      {"body": "a\x00b"},
		"escape in body":   {"body": "a\x1bb"},
		"big body":         {"body": strings.Repeat("a", 64<<10+1)},
		"marker in body":   {"body": "x <!--  PatchBot:issue-created -->"},
		"marker in title":  {"title": "<!-- patchbot:x -->"},
		"missing key":      {"idempotency_key": nil},
		"short key":        {"idempotency_key": "abc"},
		"odd key":          {"idempotency_key": "key with spaces!!"},
		"unknown provider": {"start_run": true, "provider": "nope"},
		"bad mode":         {"start_run": true, "mode": "yolo"},
		"negative budget":  {"start_run": true, "max_cost_micros": -1},
	} {
		w := f.request("POST", "/api/issues", issueJSON(t, fields))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d: %s", name, w.Code, w.Body.String())
		}
	}
	requireStatus(t, f.request("POST", "/api/issues", `{"repo":"owner/repo","title":"t","idempotency_key":"`+goodKey+`","unknown":1}`), http.StatusBadRequest)
	requireStatus(t, f.request("POST", "/api/issues", `null`), http.StatusBadRequest)
	if g.attempts != 0 {
		t.Fatalf("invalid requests reached GitHub: %d", g.attempts)
	}
	// Boundaries are accepted: 256-rune title, 64 KiB body, tabs and newlines.
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, map[string]any{"title": strings.Repeat("é", 256), "body": strings.Repeat("a", 64<<10), "idempotency_key": "boundary-key-1"})), http.StatusCreated)
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, map[string]any{"body": "line1\n\tline2\r\nline3", "idempotency_key": "boundary-key-2"})), http.StatusCreated)
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, map[string]any{"body": "", "idempotency_key": "boundary-key-3"})), http.StatusCreated)
	if got := g.issues[2].Body; !strings.HasPrefix(got, "---\n") {
		t.Fatalf("empty body: %q", got)
	}
	requireStatus(t, f.request("GET", "/api/issues", ""), http.StatusMethodNotAllowed)
}

func TestCreateIssueGitHubErrorsAreSafeAndRetryable(t *testing.T) {
	leak := "ghp_" + testCredential + " raw GitHub body"
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   string
	}{
		"forbidden":     {&github.ErrForbidden{APIError: github.APIError{StatusCode: 403, Message: leak}}, 403, "github_permission_denied"},
		"unauthorized":  {&github.ErrUnauthorized{APIError: github.APIError{StatusCode: 401, Message: leak}}, 403, "github_permission_denied"},
		"not found":     {&github.ErrNotFound{APIError: github.APIError{StatusCode: 404, Message: leak}}, 403, "github_permission_denied"},
		"disabled":      {&github.APIError{StatusCode: 410, Message: leak}, 422, "issues_disabled"},
		"validation":    {&github.ErrValidation{APIError: github.APIError{StatusCode: 422, Message: leak}}, 422, "github_rejected"},
		"rate limited":  {&github.ErrRateLimited{APIError: github.APIError{StatusCode: 429, Message: leak}}, 429, "github_rate_limited"},
		"no credential": {github.ErrCredentialUnavailable, 502, "github_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			f, g := newIssueFixture(t, nil)
			g.createErr = tc.err
			w := f.request("POST", "/api/issues", issueJSON(t, nil))
			requireStatus(t, w, tc.status)
			if decodeIssue(t, w).Error.Kind != tc.kind || strings.Contains(w.Body.String(), "raw GitHub body") || strings.Contains(w.Body.String(), "ghp_") {
				t.Fatalf("response: %s", w.Body.String())
			}
			// Nothing was created, so the same key is free to retry immediately.
			g.createErr = nil
			requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, nil)), http.StatusCreated)
			if g.count() != 1 {
				t.Fatal("retry after a definite failure must create once")
			}
		})
	}
}

func TestCreateIssueNotConnected(t *testing.T) {
	f := newSetupFixture(t, setupUIToken, nil)
	r := httptest.NewRequest("POST", "http://"+f.cfg.Server.Addr+"/api/issues", strings.NewReader(issueJSON(t, nil)))
	r.Header.Set("Authorization", "Bearer "+setupUIToken)
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "github_not_connected") || !strings.Contains(w.Body.String(), "/setup/github") {
		t.Fatalf("issue before connect: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.db.DashboardIssue(t.Context(), goodKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a row was recorded: %v", err)
	}
}

func TestCreateIssueUnsupportedClient(t *testing.T) {
	f := newFixture(t, nil, nil)
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, nil)), http.StatusNotImplemented)
}

func TestCreateIssueRequiresAuthAndSameOrigin(t *testing.T) {
	g := newIssueGitHub()
	f := newFixture(t, nil, func(o *Options) {
		o.Token = secret.New("ui-secret-fixture-token")
		o.Runner.GitHub = g
	})
	for _, auth := range []string{"", "Bearer wrong", "Basic " + f.token} {
		r := httptest.NewRequest("POST", "http://"+f.cfg.Server.Addr+"/api/issues", strings.NewReader(issueJSON(t, nil)))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, r)
		requireStatus(t, w, http.StatusUnauthorized)
	}
	r := httptest.NewRequest("POST", "http://"+f.cfg.Server.Addr+"/api/issues", strings.NewReader(issueJSON(t, nil)))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site request: %d", w.Code)
	}
	if g.attempts != 0 {
		t.Fatal("unauthenticated request reached GitHub")
	}
	f.gh = g.fakeGitHub
	requireStatus(t, f.request("POST", "/api/issues", issueJSON(t, nil)), http.StatusCreated)
}

func TestCreateIssueBodyLimit(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	huge := `{"repo":"owner/repo","title":"t","body":"` + strings.Repeat("a", 2<<20) + `","idempotency_key":"` + goodKey + `"}`
	requireStatus(t, f.request("POST", "/api/issues", huge), http.StatusRequestEntityTooLarge)
	if g.attempts != 0 {
		t.Fatal("oversized request reached GitHub")
	}
}

func TestCreateIssueAccessModeRecordsEmail(t *testing.T) {
	g := newIssueGitHub()
	a := newAccessFixture(t, fake.New("anthropic", specStep()), func(o *Options) { o.Runner.GitHub = g })
	a.gh = g.fakeGitHub
	a.withRepo()
	jwt := a.token(nil)
	requireStatus(t, a.do("POST", "/api/issues", issueJSON(t, nil), ""), http.StatusUnauthorized)
	w := a.do("POST", "/api/issues", issueJSON(t, map[string]any{"start_run": true, "mode": "refine", "dry_run": true}), jwt)
	requireStatus(t, w, http.StatusCreated)
	out := decodeIssue(t, w)
	if out.Issue.RequestedBy != accessOwner || !strings.Contains(g.issues[0].Body, "`"+accessOwner+"`") {
		t.Fatalf("actor not recorded: %s / %q", w.Body.String(), g.issues[0].Body)
	}
	row, _ := a.db.DashboardIssue(t.Context(), goodKey)
	if row.Actor != accessOwner {
		t.Fatalf("row actor %q", row.Actor)
	}
	a.waitFor(out.Run.ID, store.StatusAwaitingApproval)
	actions, err := a.db.ListActions(t.Context(), out.Run.ID)
	if err != nil || len(actions) == 0 {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Actor != accessOwner || action.Channel != "ui" {
			t.Fatalf("run action audit: %+v", action)
		}
	}
}

func TestCreateIssueHostileTextIsInertData(t *testing.T) {
	f, g := newIssueFixture(t, nil)
	payload := `<script>alert(1)</script><img src=x onerror=alert(1)>`
	w := f.request("POST", "/api/issues", issueJSON(t, map[string]any{"title": payload, "body": payload}))
	requireStatus(t, w, http.StatusCreated)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", w.Header())
	}
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("markup in response: %s", w.Body.String())
	}
	if g.issues[0].Title != payload || !strings.HasPrefix(g.issues[0].Body, payload+"\n\n---") {
		t.Fatalf("sent to GitHub: %q / %q", g.issues[0].Title, g.issues[0].Body)
	}
	// The page shell never embeds issue text.
	page := f.request("GET", "/runs/new", "")
	if strings.Contains(page.Body.String(), "alert(1)") {
		t.Fatal("issue text reached the page shell")
	}
}
