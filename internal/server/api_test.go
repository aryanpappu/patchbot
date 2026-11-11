package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestAPIStartApprovePR(t *testing.T) {
	f := newFixture(t, fake.New("anthropic", specStep(), writeStep(), submitStep()), nil)
	f.withRepo()
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"fix","refine_model":"planner","code_model":"coder"}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	waiting := f.waitFor(run.ID, store.StatusAwaitingApproval)
	if waiting.RefineModel != "planner" || waiting.CodeModel != "coder" {
		t.Fatal("stage models were not frozen")
	}
	path := fmt.Sprintf("/api/runs/%d", run.ID)
	requireStatus(t, f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"fix"}`), 409)
	w = f.request("GET", path, "")
	requireStatus(t, w, 200)
	var detail struct {
		LatestSpec *store.Spec  `json:"latest_spec"`
		Specs      []store.Spec `json:"spec_versions"`
		Steps      []store.Step `json:"steps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.LatestSpec == nil || len(detail.Specs) != 1 || len(detail.Steps) == 0 {
		t.Fatalf("incomplete detail: %s", w.Body.String())
	}
	requireStatus(t, f.request("POST", path+"/approve", `{}`), 202)
	done := f.waitFor(run.ID, store.StatusSucceeded)
	if done.Outcome != "pr_opened" || done.PRDraft || done.PRURL == "" {
		t.Fatalf("bad publication: %+v", done)
	}
	f.gh.mu.Lock()
	count := len(f.gh.prs)
	f.gh.mu.Unlock()
	if count != 1 {
		t.Fatal("PR was not created")
	}
	if got := runGit(t, f.cfg.DataDir(), "--git-dir", f.remote, "branch", "--list", "patchbot/*"); !strings.Contains(got, "patchbot/issue-12") {
		t.Fatal("branch was not pushed")
	}
	for _, name := range []string{"diff", "baseline", "test_output", "pr_title", "pr_body"} {
		w := f.request("GET", path+"/artifacts/"+name, "")
		requireStatus(t, w, 200)
		if w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("unsafe artifact headers")
		}
	}
	actions, err := f.db.ListActions(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Actor != "operator" || a.Channel != "ui" {
			t.Fatalf("incorrect UI audit: %+v", a)
		}
	}
	approvals, err := f.db.ListApprovals(t.Context(), run.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Channel != "ui" {
		t.Fatalf("incorrect approval audit: %+v: %v", approvals, err)
	}
	f.provider.AssertConsumed(t)
}

func TestSpecEditVoidsApproval(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusApproved)
	d := draft()
	d.Summary = "A revised result"
	raw, _ := json.Marshal(d)
	path := fmt.Sprintf("/api/runs/%d/spec", run.ID)
	w := f.request("PUT", path, string(raw))
	requireStatus(t, w, 200)
	latest, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 2 || latest.Hash == draft().Hash() {
		t.Fatal("edit did not append a new spec")
	}
	if _, err := f.db.ValidApproval(t.Context(), run.ID, latest.Version, latest.Hash); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("edited spec retained approval")
	}
	current, err := f.db.GetRun(t.Context(), run.ID)
	if err != nil || current.Status != store.StatusAwaitingApproval {
		t.Fatalf("edit: %+v: %v", current, err)
	}
	requireStatus(t, f.request("PUT", path, `{"summary":"x","files":[{"path":"../escape","change":"bad"}]}`), 400)
}

func TestReviseRejectRetry(t *testing.T) {
	f := newFixture(t, fake.New("anthropic", specStep(), specStep(), specStep()), nil)
	f.withRepo()
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"refine","dry_run":true}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	f.waitFor(run.ID, store.StatusAwaitingApproval)
	path := fmt.Sprintf("/api/runs/%d", run.ID)
	requireStatus(t, f.request("POST", path+"/revise", `{"notes":"make it clearer"}`), 202)
	f.waitFor(run.ID, store.StatusAwaitingApproval)
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil || spec.Version != 2 {
		t.Fatalf("revision: %+v: %v", spec, err)
	}
	requireStatus(t, f.request("POST", path+"/reject", `{}`), 200)
	requireStatus(t, f.request("POST", path+"/approve", `{}`), 409)
	w = f.request("POST", path+"/retry", `{"provider":"anthropic","model":"other"}`)
	requireStatus(t, w, 202)
	retry := f.responseRun(w)
	if retry.RetryOf == nil || *retry.RetryOf != run.ID || retry.ID == run.ID || retry.CodeModel != "other" {
		t.Fatalf("retry: %+v", retry)
	}
	f.waitFor(retry.ID, store.StatusAwaitingApproval)
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/approve", retry.ID), ""), 202)
	f.waitFor(retry.ID, store.StatusSucceeded)
	for _, id := range []int64{run.ID, retry.ID} {
		actions, err := f.db.ListActions(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range actions {
			if a.Channel != "ui" || a.Actor != "operator" {
				t.Fatalf("audit: %+v", a)
			}
		}
	}
}

func TestProvidersModelsAndPagination(t *testing.T) {
	f := newFixture(t, nil, nil)
	requireStatus(t, f.request("GET", "/", ""), 200)
	requireStatus(t, f.request("GET", "/api/providers", ""), 200)
	f.provider.SetModels([]llm.ModelInfo{{ID: "live-model", DisplayName: "Live"}}, nil)
	w := f.request("GET", "/api/providers/anthropic/models", "")
	requireStatus(t, w, 200)
	for _, value := range []string{"live-model", "configured", `"live":true`} {
		if !strings.Contains(w.Body.String(), value) {
			t.Fatalf("missing merged models: %s", w.Body.String())
		}
	}
	f.server.models.Invalidate("")
	f.provider.SetModels(nil, &llm.Error{Kind: llm.ErrAuth, Message: testCredential})
	w = f.request("GET", "/api/providers/anthropic/models", "")
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"live":false`) || !strings.Contains(w.Body.String(), "warning") {
		t.Fatal("missing fallback warning")
	}
	f.seed(store.StatusFailed)
	f.seed(store.StatusAwaitingApproval)
	f.seed(store.StatusFailed)
	w = f.request("GET", "/api/runs?repo=owner%2Frepo&status=failed&limit=1", "")
	requireStatus(t, w, 200)
	var page store.RunPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 || page.NextCursor == "" {
		t.Fatalf("page: %+v", page)
	}
	first := page.Runs[0].ID
	w = f.request("GET", "/api/runs?repo=owner%2Frepo&status=failed&limit=1&cursor="+page.NextCursor, "")
	requireStatus(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 || page.Runs[0].ID == first || page.NextCursor != "" {
		t.Fatal("incorrect cursor continuation")
	}
}

func TestAPIErrors(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusRejected)
	path := fmt.Sprintf("/api/runs/%d", run.ID)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/missing", "", 404}, {"POST", "/", "{}", 405}, {"POST", "/api/providers", "{}", 405},
		{"GET", "/api/providers/missing/models", "", 404}, {"POST", "/api/providers/anthropic/models", "{}", 405},
		{"GET", "/api/runs/99999", "", 404}, {"GET", "/api/runs/not-an-id", "", 400}, {"POST", path, "{}", 405},
		{"GET", "/api/runs?cursor=broken", "", 400}, {"GET", "/api/runs?limit=999", "", 400}, {"GET", "/api/runs?issue=-1", "", 400}, {"GET", "/api/runs?status=bogus", "", 400},
		{"POST", "/api/runs", `{"repo":"other/repo","issue":1}`, 400}, {"POST", "/api/runs", `{"repo":"owner/repo","issue":0}`, 400},
		{"POST", "/api/runs", `{"repo":"owner/repo","issue":1,"provider":"missing"}`, 400},
		{"POST", "/api/runs", `{"repo":"owner/repo","issue":1,"mode":"skip-approval"}`, 400},
		{"POST", "/api/runs", `{"repo":"owner/repo","issue":1,"unknown":true}`, 400},
		{"POST", "/api/runs", `{}`, 400}, {"POST", "/api/runs", `null`, 400}, {"POST", "/api/runs", `{} {}`, 400},
		{"POST", "/api/runs", `{"repo":"` + strings.Repeat("x", maxBodyBytes) + `"}`, 413},
		{"GET", path + "/approve", "", 405}, {"POST", path + "/approve", "{}", 409}, {"POST", path + "/cancel", "{}", 409},
		{"POST", path + "/reject", "{}", 409}, {"POST", path + "/revise", `{"notes":""}`, 400},
		{"POST", path + "/revise", `{"notes":"change"}`, 409}, {"POST", path + "/retry", `{"provider":"unknown"}`, 400},
		{"POST", path + "/retry", `{"model":"\n"}`, 400}, {"POST", path + "/retry", `{"actor":"attacker"}`, 400},
		{"GET", path + "/spec", "", 405}, {"PUT", path + "/spec", `{"unknown":true}`, 400}, {"PUT", path + "/spec", `{}`, 409},
		{"GET", path + "/events?after=-1", "", 400}, {"POST", path + "/events", "{}", 405},
		{"GET", path + "/artifacts/diff", "", 404}, {"GET", path + "/artifacts/accounting", "", 404}, {"GET", path + "/artifacts/diff?attempt=-1", "", 400},
		{"POST", path + "/artifacts/diff", "{}", 405},
	} {
		t.Run(tc.method+tc.path+fmt.Sprint(tc.status), func(t *testing.T) { requireStatus(t, f.request(tc.method, tc.path, tc.body), tc.status) })
	}
	waiting := f.seed(store.StatusAwaitingApproval)
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/retry", waiting.ID), `{}`), 409)
}

func TestCancelStopsBackgroundWork(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	p := fake.NewFunc("anthropic", func(ctx context.Context, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	})
	f := newFixture(t, p, nil)
	f.withRepo()
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"fix"}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("provider not reached")
	}
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/cancel", run.ID), `{}`), 200)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("API cancel did not stop provider")
	}
	f.waitFor(run.ID, store.StatusCancelled)
}

func TestEveryUICancelIsAudited(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	path := fmt.Sprintf("/api/runs/%d/cancel", run.ID)
	requireStatus(t, f.request("POST", path, `{}`), 200)
	requireStatus(t, f.request("POST", path, `{}`), 200)
	actions, err := f.db.ListActions(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("cancel intents were not audited: %+v", actions)
	}
	for _, a := range actions {
		if a.Kind != "cancel" || a.Actor != "operator" || a.Channel != "ui" {
			t.Fatalf("cancel audit: %+v", a)
		}
	}
}
