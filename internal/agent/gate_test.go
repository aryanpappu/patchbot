package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/store"
)

type memory struct {
	run                  store.Run
	spec                 store.Spec
	approval             store.Approval
	specErr, approvalErr error
}

func (m *memory) GetRun(context.Context, int64) (store.Run, error)      { return m.run, nil }
func (m *memory) LatestSpec(context.Context, int64) (store.Spec, error) { return m.spec, m.specErr }
func (m *memory) ValidApproval(context.Context, int64, int64, string) (store.Approval, error) {
	return m.approval, m.approvalErr
}

func fixture(now time.Time) *memory {
	return &memory{
		run:      store.Run{ID: 1, Status: store.StatusApproved},
		spec:     store.Spec{RunID: 1, Version: 1, Hash: "hash", IssueHash: "issue"},
		approval: store.Approval{ID: 1, RunID: 1, SpecVersion: 1, SpecHash: "hash", Decision: "approve", CreatedAt: now.Add(-time.Hour)},
	}
}

func TestCheckDenials(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	cases := map[string]func(*memory){
		"wrong status":       func(m *memory) { m.run.Status = store.StatusRunning },
		"no spec":            func(m *memory) { m.specErr = store.ErrNotFound },
		"no approval":        func(m *memory) { m.approvalErr = store.ErrNotFound },
		"older version":      func(m *memory) { m.spec.Version = 2 },
		"hash mismatch":      func(m *memory) { m.approval.SpecHash = "other" },
		"issue changed":      func(m *memory) { m.spec.IssueHash = "other" },
		"expired":            func(m *memory) { m.approval.CreatedAt = now.Add(-14 * 24 * time.Hour) },
		"future":             func(m *memory) { m.approval.CreatedAt = now.Add(time.Minute) },
		"too many revisions": func(m *memory) { m.spec.Version = 7; m.approval.SpecVersion = 7 },
		"rejected":           func(m *memory) { m.approval.Decision = "reject" },
		"old waiting run":    func(m *memory) { old := now.Add(-15 * 24 * time.Hour); m.run.WaitingSince = &old },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := fixture(now)
			change(m)
			_, err := Check(t.Context(), m, 1, "issue", ApprovalOptions{Clock: func() time.Time { return now }})
			if !errors.Is(err, ErrApprovalDenied) {
				t.Fatalf("check=%v", err)
			}
		})
	}
}

func TestTokenRevalidatedAndBound(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := fixture(now)
	token, err := Check(t.Context(), m, 1, "issue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateApproval(t.Context(), token, 1, "issue"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		run   int64
		issue string
	}{{2, "issue"}, {1, "changed"}} {
		if _, err := validateApproval(t.Context(), token, tc.run, tc.issue); !errors.Is(err, ErrApprovalDenied) {
			t.Fatal("token not bound")
		}
	}
	m.spec.Version = 2
	m.approval.SpecVersion = 2
	if _, err := validateApproval(t.Context(), token, 1, "issue"); !errors.Is(err, ErrApprovalDenied) {
		t.Fatal("stale token accepted")
	}
	if _, err := validateApproval(t.Context(), approvalToken{}, 1, "issue"); !errors.Is(err, ErrApprovalDenied) {
		t.Fatal("zero capability accepted")
	}

}

func TestConfiguredTTLAndRevisionBoundary(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := fixture(now)
	m.spec.Version = 6
	m.approval.SpecVersion = 6
	opts := ApprovalOptions{Clock: func() time.Time { return now }, TTL: 2 * time.Hour, MaxRevisions: 5}
	if _, err := Check(t.Context(), m, 1, "issue", opts); err != nil {
		t.Fatal(err)
	}
	opts.TTL = 30 * time.Minute
	if _, err := Check(t.Context(), m, 1, "issue", opts); !errors.Is(err, ErrApprovalDenied) {
		t.Fatal("configured TTL ignored")
	}
	opts.TTL = 2 * time.Hour
	opts.MaxRevisions = 4
	if _, err := Check(t.Context(), m, 1, "issue", opts); !errors.Is(err, ErrApprovalDenied) {
		t.Fatal("configured revisions ignored")
	}
}
