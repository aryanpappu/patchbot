package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/testhttp"
)

func TestManagerConcurrencyCancellationAndDetachedRequests(t *testing.T) {
	m := newManager(2, 16)
	entered := make(chan int, 3)
	release := make(chan struct{})
	var active, maxActive atomic.Int64
	work := func(n int) func(context.Context) error {
		return func(ctx context.Context) error {
			count := active.Add(1)
			defer active.Add(-1)
			for old := maxActive.Load(); count > old; old = maxActive.Load() {
				if maxActive.CompareAndSwap(old, count) {
					break
				}
			}
			entered <- n
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}
	}
	parent, cancel := context.WithCancel(t.Context())
	if err := m.submit(parent, 1, work(1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := m.submit(t.Context(), 2, work(2)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("detached jobs did not start")
		}
	}
	if err := m.submit(t.Context(), 3, work(3)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
		t.Fatal("concurrency limit exceeded")
	case <-time.After(20 * time.Millisecond):
	}
	m.cancel(3)
	close(release)
	m.stopAdmission()
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := m.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if maxActive.Load() != 2 {
		t.Fatal("default concurrency limit not observed")
	}
	if err := m.submit(t.Context(), 4, work(4)); !errors.Is(err, errShuttingDown) {
		t.Fatalf("shutdown accepted work: %v", err)
	}
}

func TestManagerSerializesFollowUpForSameRun(t *testing.T) {
	m := newManager(2, 16)
	release := make(chan struct{})
	entered := make(chan struct{})
	if err := m.submit(t.Context(), 1, func(context.Context) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := m.submit(t.Context(), 1, func(context.Context) error { close(entered); return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
		t.Fatal("follow-up ran concurrently")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	m.stopAdmission()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := m.wait(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("follow-up did not run")
	}
}

func TestGracefulShutdownDrainsWorkAndClosesStreams(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	httpServer := testhttp.NewServer(f.server)
	defer httpServer.Close()
	response, reader, cancel := openStream(t, httpServer.URL, httpServer.Client(), run.ID, "", "")
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	started, release := make(chan struct{}), make(chan struct{})
	if err := f.server.manager.submit(t.Context(), run.ID, func(ctx context.Context) error {
		close(started)
		select {
		case <-ctx.Done():
			t.Error("graceful shutdown cancelled in-flight work")
		case <-release:
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan error, 1)
	go func() {
		ctx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		stopped <- f.server.Shutdown(ctx)
	}()
	select {
	case err := <-stopped:
		t.Fatalf("shutdown did not wait: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := readFrame(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("SSE not closed on shutdown: %v", err)
	}
	requireStatus(t, f.request("GET", "/api/runs", ""), 503)
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.server.maintenanceDone:
	default:
		t.Fatal("maintenance ticker did not stop")
	}
	if f.server.http.ReadHeaderTimeout <= 0 || f.server.http.ReadTimeout <= 0 || f.server.http.IdleTimeout <= 0 || f.server.http.WriteTimeout != 0 {
		t.Fatal("incorrect HTTP timeouts")
	}
}

func TestShutdownTimeoutCancelsWork(t *testing.T) {
	f := newFixture(t, nil, nil)
	started, exited := make(chan struct{}), make(chan struct{})
	if err := f.server.manager.submit(t.Context(), 1, func(ctx context.Context) error { close(started); <-ctx.Done(); close(exited); return nil }); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("shutdown timeout did not cancel work")
	}
}

func TestMaintenanceSyncsGitHubApprovalsAndExpiresWaiting(t *testing.T) {
	var offset atomic.Int64
	f := newFixture(t, fake.New("anthropic", specStep()), func(opts *Options) {
		opts.SyncInterval = 10 * time.Millisecond
		opts.Runner.Clock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	})
	f.withRepo()
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"refine"}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	f.waitFor(run.ID, store.StatusAwaitingApproval)
	f.gh.mu.Lock()
	now := time.Now().Add(time.Second)
	f.gh.comments = append(f.gh.comments, github.Comment{ID: int64(len(f.gh.comments) + 1), Author: "owner", Body: "/approve", CreatedAt: now, UpdatedAt: now})
	f.gh.mu.Unlock()
	f.waitFor(run.ID, store.StatusSucceeded)
	approvals, err := f.db.ListApprovals(t.Context(), run.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Channel != "github" {
		t.Fatalf("sync approval: %+v: %v", approvals, err)
	}
	waiting := f.seed(store.StatusAwaitingApproval)
	offset.Store(int64(15 * 24 * time.Hour))
	expired := f.waitFor(waiting.ID, store.StatusFailed)
	if expired.ErrorKind != "expired" {
		t.Fatalf("waiting expiry: %+v", expired)
	}
}

func TestQueuedApprovedExecutionHasRecoverableHeartbeat(t *testing.T) {
	f := newFixture(t, nil, nil)
	// Hold both worker slots while approval queues execution.
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	for _, id := range []int64{100, 101} {
		if err := f.server.manager.submit(t.Context(), id, func(context.Context) error { entered <- struct{}{}; <-release; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	<-entered
	<-entered
	run := f.seed(store.StatusAwaitingApproval)
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/approve", run.ID), `{}`), 202)
	current, err := f.db.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != store.StatusApproved || current.HeartbeatAt == nil || current.Phase != store.PhaseClone {
		t.Fatalf("queued execution is not recoverable: %+v", current)
	}
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/cancel", run.ID), `{}`), 200)
	close(release)
}

func TestDisconnectDoesNotCancelRunningRun(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := fake.NewFunc("anthropic", func(ctx context.Context, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return fake.New("anthropic", specStep()).Chat(ctx, req)
		}
	})
	f := newFixture(t, p, nil)
	f.withRepo()
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"fix"}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("work did not start")
	}
	httpServer := testhttp.NewServer(f.server)
	defer httpServer.Close()
	response, _, cancel := openStream(t, httpServer.URL, httpServer.Client(), run.ID, "", "")
	cancel()
	_ = response.Body.Close()
	close(release)
	f.waitFor(run.ID, store.StatusAwaitingApproval)
}

type idleListener struct {
	done   chan struct{}
	once   sync.Once
	public bool
}

func (l *idleListener) Accept() (net.Conn, error) { <-l.done; return nil, net.ErrClosed }
func (l *idleListener) Close() error              { l.once.Do(func() { close(l.done) }); return nil }
func (l *idleListener) Addr() net.Addr {
	ip := net.IPv4(127, 0, 0, 1)
	if l.public {
		ip = net.IPv4zero
	}
	return &net.TCPAddr{IP: ip, Port: 80}
}

func TestServeContextWaitsForGracefulShutdown(t *testing.T) {
	f := newFixture(t, nil, nil)
	listener := &idleListener{done: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	if err := f.server.manager.submit(t.Context(), 1, func(context.Context) error { close(entered); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.server.Serve(ctx, listener) }()
	cancel()
	select {
	case <-listener.done:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not close listener")
	}
	select {
	case err := <-result:
		t.Fatalf("Serve returned before run drained: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestServeRechecksListenerAddress(t *testing.T) {
	f := newFixture(t, nil, nil)
	listener := &idleListener{done: make(chan struct{}), public: true}
	if err := f.server.Serve(t.Context(), listener); err == nil {
		t.Fatal("public listener accepted without token")
	}
	select {
	case <-listener.done:
	default:
		t.Fatal("rejected listener was not closed")
	}
}

func TestManagerBoundsReservationsAndReusesCancelledCapacity(t *testing.T) {
	m := newManager(2, 1)
	releaseWorkers := holdWorkers(t, m)
	defer func() { releaseWorkers() }()
	_, release, err := m.reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.reserve(t.Context()); !errors.Is(err, errQueueFull) {
		t.Fatalf("reservation exceeded configured queue capacity: %v", err)
	}
	release() // Validation failed before a durable action.
	ctx, release, err := m.reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	parent, cancel := context.WithCancel(ctx)
	cancel() // Disconnects do not revoke already-secured admission.
	if err := m.submit(parent, 3, func(context.Context) error { t.Error("cancelled pending work ran"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := m.submit(t.Context(), 4, func(context.Context) error { return nil }); !errors.Is(err, errQueueFull) {
		t.Fatalf("full queue accepted another job: %v", err)
	}
	m.cancel(3)
	if err := m.submit(t.Context(), 4, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("cancelled job retained queue capacity: %v", err)
	}
	m.cancel(4)
	ctx, releaseAfterShutdown, err := m.reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAfterShutdown()
	m.stopAdmission()
	if err := m.submit(ctx, 5, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("shutdown revoked a reservation after its decision could commit: %v", err)
	}
	m.cancel(5)
	// Drain after releasing the workers, without leaving test goroutines behind.
	releaseWorkers()
	releaseWorkers = func() {}
	waitCtx, cancelWait := context.WithTimeout(t.Context(), time.Second)
	defer cancelWait()
	if err := m.wait(waitCtx); err != nil {
		t.Fatal(err)
	}
}
