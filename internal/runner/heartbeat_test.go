package runner_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

// parkingStore forces a heartbeat's old running snapshot to lose to the
// waiting transition. The transition can finish only after the heartbeat
// rereads the waiting row, or after an erroneous cancellation wakes it.
type parkingStore struct {
	store.Store
	parked, rechecked chan struct{}
	once              sync.Once
}

func (s *parkingStore) Heartbeat(ctx context.Context, id int64) error {
	select {
	case <-s.parked:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Store.Heartbeat(ctx, id)
}

func (s *parkingStore) SetRunStatus(ctx context.Context, id int64, from, to store.Status, patch store.RunPatch) error {
	if err := s.Store.SetRunStatus(ctx, id, from, to, patch); err != nil {
		return err
	}
	if to != store.StatusAwaitingApproval {
		return nil
	}
	close(s.parked)
	select {
	case <-s.rechecked:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *parkingStore) GetRun(ctx context.Context, id int64) (store.Run, error) {
	run, err := s.Store.GetRun(ctx, id)
	if err == nil && run.Status == store.StatusAwaitingApproval {
		select {
		case <-s.parked:
			s.once.Do(func() { close(s.rechecked) })
		default:
		}
	}
	return run, err
}

func TestHeartbeatLosingToWaitingDoesNotCancel(t *testing.T) {
	f := newFixture(t, "true", specStep())
	db := &parkingStore{Store: f.db, parked: make(chan struct{}), rechecked: make(chan struct{})}
	service, err := runner.New(t.Context(), f.cfg, db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	run, err := service.Start(ctx, runner.Request{Repo: "owner/repo", Issue: 12, Mode: "fix", DryRun: true})
	if err != nil || run.Status != store.StatusAwaitingApproval {
		t.Fatalf("heartbeat cancelled a parking run: %s, %v", run.Status, err)
	}
}
