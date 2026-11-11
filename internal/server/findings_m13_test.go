package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

// Finding 1: even same-origin GET/SSE requests must reject a rebound Host.
func TestReadRoutesValidateHost(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusRejected)
	for _, path := range []string{"/", "/api/runs", fmt.Sprintf("/api/runs/%d", run.ID), fmt.Sprintf("/api/runs/%d/artifacts/diff", run.ID), fmt.Sprintf("/api/runs/%d/events", run.ID)} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://attacker.invalid"+path, nil)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			w := httptest.NewRecorder()
			f.server.ServeHTTP(w, r)
			requireStatus(t, w, http.StatusForbidden)
		})
	}
}

func holdWorkers(t *testing.T, m *manager) func() {
	t.Helper()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	for _, id := range []int64{-1, -2} {
		if err := m.submit(t.Context(), id, func(context.Context) error { entered <- struct{}{}; <-release; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	return func() { close(release) }
}

// Finding 2: recovery in another process must preserve every admitted job.
func TestQueuedJobsKeepOwnershipBeforeWork(t *testing.T) {
	for _, action := range []string{"start", "revise", "approve"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t, nil, nil)
			release := holdWorkers(t, f.server.manager)
			defer release()
			var run store.Run
			if action == "start" {
				w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"fix"}`)
				requireStatus(t, w, 202)
				run = f.responseRun(w)
			} else {
				run = f.seed(store.StatusAwaitingApproval)
				body := `{}`
				if action == "revise" {
					body = `{"notes":"clarify"}`
				}
				requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/%s", run.ID, action), body), 202)
			}
			// Use a cutoff after admission and observe a later heartbeat, without
			// aging real time by the production one-minute stale threshold.
			cutoff := time.Now().UTC()
			awaitCondition(t, func() bool {
				current, err := f.db.GetRun(t.Context(), run.ID)
				return err == nil && current.HeartbeatAt != nil && current.HeartbeatAt.After(cutoff)
			}, "queued job never refreshed its heartbeat")
			other, err := store.Open(t.Context(), f.cfg.DatabasePath())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = other.Close() }()
			opts := f.server.opts.Runner
			opts.Clock = func() time.Time { return cutoff.Add(time.Minute) }
			if _, err := runner.New(t.Context(), f.cfg, other, opts); err != nil {
				t.Fatal(err)
			}
			current, err := other.GetRun(t.Context(), run.ID)
			if err != nil || !current.Status.IsActive() {
				t.Fatalf("another process reclaimed owned job: %+v, %v", current, err)
			}
			requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/cancel", run.ID), `{}`), 200)
		})
	}
}

func awaitCondition(t *testing.T, condition func() bool, failure string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-timer.C:
			t.Fatal(failure)
		case <-ticker.C:
		}
	}
}

// Finding 3: a clone-phase denial must permit a fresh approval and dispatch.
func TestReapprovalDispatchesAfterQueuedExpiry(t *testing.T) {
	var offset atomic.Int64
	f := newFixture(t, fake.New("anthropic", specStep()), func(opts *Options) {
		opts.Runner.Clock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	})
	f.withRepo()
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"refine","dry_run":true}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	f.waitFor(run.ID, store.StatusAwaitingApproval)
	release := holdWorkers(t, f.server.manager)
	path := fmt.Sprintf("/api/runs/%d", run.ID)
	requireStatus(t, f.request("POST", path+"/approve", `{}`), 202)
	offset.Store(int64(15 * 24 * time.Hour))
	release()
	parked := f.waitFor(run.ID, store.StatusAwaitingApproval)
	if parked.Phase != store.PhaseRefine {
		t.Fatalf("denial retained execution claim: %s", parked.Phase)
	}
	awaitCondition(t, func() bool { return !f.server.manager.busy(run.ID) }, "expired worker did not finish")
	// Keep the replacement worker queued so the test observes dispatch itself.
	release = holdWorkers(t, f.server.manager)
	defer release()
	offset.Store(0)
	requireStatus(t, f.request("POST", path+"/approve", `{}`), 202)
	if !f.server.manager.busy(run.ID) {
		t.Fatal("approval committed without dispatch")
	}
	requireStatus(t, f.request("POST", path+"/cancel", `{}`), 200)
}

// Finding 4: a fresh orphan at restart must be recovered later in that lifetime.
func TestMaintenanceRecoversRunsThatBecomeStaleAfterRestart(t *testing.T) {
	var offset atomic.Int64
	f := newFixture(t, nil, func(opts *Options) {
		opts.SyncInterval = 5 * time.Millisecond
		opts.Runner.Clock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	})
	if err := f.server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	var orphans []store.Run
	for i, status := range []store.Status{store.StatusQueued, store.StatusRunning, store.StatusApproved} {
		now := time.Now().UTC()
		run, err := f.db.CreateRun(t.Context(), store.Run{Repo: "owner/repo", IssueNumber: int64(i + 1), Status: status, Phase: store.PhaseClone, HeartbeatAt: &now})
		if err != nil {
			t.Fatal(err)
		}
		orphans = append(orphans, run)
	}
	var err error
	f.server, err = New(t.Context(), f.cfg, f.db, f.server.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, orphan := range orphans {
		run, readErr := f.db.GetRun(t.Context(), orphan.ID)
		if readErr != nil || run.Status != orphan.Status {
			t.Fatalf("startup reclaimed fresh orphan: %+v, %v", run, readErr)
		}
	}
	offset.Store(int64(2 * time.Minute))
	for _, orphan := range orphans {
		awaitCondition(t, func() bool {
			run, readErr := f.db.GetRun(t.Context(), orphan.ID)
			return readErr == nil && run.Status == store.StatusFailed && run.ErrorKind == "interrupted"
		}, "maintenance never recovered orphan")
		if _, err := f.db.CreateRun(t.Context(), store.Run{Repo: orphan.Repo, IssueNumber: orphan.IssueNumber}); err != nil {
			t.Fatalf("recovered issue cannot be retried: %v", err)
		}
	}
}

// Finding 5: once all pending capacity is used, admission must be mutation-free.
func TestPendingQueueRejectsExcessWork(t *testing.T) {
	f := newFixture(t, nil, nil)
	release := holdWorkers(t, f.server.manager)
	defer release()
	// The production default permits sixteen pending jobs.
	for i := range 16 {
		requireStatus(t, f.request("POST", "/api/runs", fmt.Sprintf(`{"repo":"owner/repo","issue":%d,"mode":"fix"}`, i+1)), 202)
	}
	waiting := f.seed(store.StatusAwaitingApproval)
	for _, action := range []string{"approve", "revise"} {
		body := `{}`
		if action == "revise" {
			body = `{"notes":"clarify"}`
		}
		w := f.request("POST", fmt.Sprintf("/api/runs/%d/%s", waiting.ID, action), body)
		requireStatus(t, w, 429)
		if !strings.Contains(w.Body.String(), "queue is full") {
			t.Fatalf("unclear backpressure error: %s", w.Body.String())
		}
		current, err := f.db.GetRun(t.Context(), waiting.ID)
		if err != nil || current.Status != waiting.Status || current.Generation != waiting.Generation {
			t.Fatalf("rejected admission changed run: %+v, %v", current, err)
		}
		approvals, err := f.db.ListApprovals(t.Context(), waiting.ID)
		if err != nil || len(approvals) != 0 {
			t.Fatalf("undispatchable decision committed: %+v, %v", approvals, err)
		}
	}
	rejected := f.seed(store.StatusRejected)
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/retry", rejected.ID), `{}`), 429)
	page, err := f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range page.Runs {
		if run.RetryOf != nil && *run.RetryOf == rejected.ID {
			t.Fatalf("full queue created a retry: %+v", run)
		}
	}
	requireStatus(t, f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":99,"mode":"fix"}`), 429)
	page, err = f.db.ListRuns(t.Context(), store.RunFilter{IssueNumber: 99})
	if err != nil || len(page.Runs) != 0 {
		t.Fatalf("full queue created a run: %+v, %v", page, err)
	}
	// Cancel pending work while the workers remain held.
	page, err = f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range page.Runs {
		if run.Status.IsActive() {
			requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/cancel", run.ID), `{}`), 200)
		}
	}
}

// Finding 6: replacing the configured bind name with a numeric address must
// retain the configured Host, including a dynamically assigned bound port.
func TestConfiguredLocalhostSurvivesNumericBind(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.cfg.Server.Addr = "localhost:0"
	f.server.mu.Lock()
	f.server.addr = f.cfg.Server.Addr
	f.server.mu.Unlock()
	listener := &idleListener{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.server.Serve(ctx, listener) }()
	awaitCondition(t, func() bool {
		f.server.mu.RLock()
		defer f.server.mu.RUnlock()
		return f.server.addr == listener.Addr().String()
	}, "Serve did not record bound address")
	for _, host := range []string{"localhost:80", "LOCALHOST:80", "127.0.0.1:80", "[::1]:80"} {
		r := httptest.NewRequest("POST", "http://"+host+"/api/runs", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, r)
		requireStatus(t, w, 400)
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
