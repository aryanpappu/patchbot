package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDashboardIssueLifecycle(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	hash := strings.Repeat("a", 64)
	d := DashboardIssue{IdempotencyKey: "key-12345678", RequestHash: hash, Repo: "owner/repo", Actor: "me@example.com", CreatedAt: time.Now().Add(-time.Hour)}
	row, inserted, err := s.BeginDashboardIssue(ctx, d)
	if err != nil || !inserted || !row.Pending || row.Number != 0 {
		t.Fatalf("begin: %+v %v %v", row, inserted, err)
	}
	again, inserted, err := s.BeginDashboardIssue(ctx, d)
	if err != nil || inserted || again.Actor != "me@example.com" {
		t.Fatalf("second begin: %+v %v %v", again, inserted, err)
	}
	// A fresh pending row cannot be claimed; a stale one can, once at a time.
	if ok, err := s.ClaimPendingDashboardIssue(ctx, d.IdempotencyKey, time.Now().Add(-2*time.Hour), time.Now()); err != nil || ok {
		t.Fatalf("claimed a fresh row: %v %v", ok, err)
	}
	if ok, err := s.ClaimPendingDashboardIssue(ctx, d.IdempotencyKey, time.Now(), time.Now()); err != nil || !ok {
		t.Fatalf("stale claim: %v %v", ok, err)
	}
	if ok, _ := s.ClaimPendingDashboardIssue(ctx, d.IdempotencyKey, time.Now().Add(-time.Minute), time.Now()); ok {
		t.Fatal("a claimed row was claimed twice")
	}
	if err := s.SetDashboardIssueRun(ctx, d.IdempotencyKey, 5); !errors.Is(err, ErrConflict) {
		t.Fatalf("run linked to a pending row: %v", err)
	}
	if err := s.CompleteDashboardIssue(ctx, d.IdempotencyKey, 7, "I_x", "https://github.com/owner/repo/issues/7", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDashboardIssue(ctx, d.IdempotencyKey, 7, "I_x", "u", time.Now()); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
	if err := s.CompleteDashboardIssue(ctx, d.IdempotencyKey, 8, "I_y", "u", time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("completion changed the issue: %v", err)
	}
	if err := s.SetDashboardIssueRun(ctx, d.IdempotencyKey, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDashboardIssueRun(ctx, d.IdempotencyKey, 6); !errors.Is(err, ErrConflict) {
		t.Fatalf("run relinked: %v", err)
	}
	got, ok, err := s.DashboardIssueByNumber(ctx, "owner/repo", 7)
	if err != nil || !ok || got.RunID != 5 || got.NodeID != "I_x" || got.Pending {
		t.Fatalf("by number: %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.DashboardIssueByNumber(ctx, "owner/repo", 99); err != nil || ok {
		t.Fatalf("unknown issue: %v %v", ok, err)
	}
	// Created rows are never deleted; pending rows are.
	if err := s.DeletePendingDashboardIssue(ctx, d.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DashboardIssue(ctx, d.IdempotencyKey); err != nil {
		t.Fatalf("created row removed: %v", err)
	}
	p := DashboardIssue{IdempotencyKey: "key-87654321", RequestHash: hash, Repo: "owner/repo", Actor: "a"}
	if _, _, err := s.BeginDashboardIssue(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePendingDashboardIssue(ctx, p.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DashboardIssue(ctx, p.IdempotencyKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending row survived: %v", err)
	}
	for _, bad := range []DashboardIssue{{}, {IdempotencyKey: "short", RequestHash: hash, Repo: "r", Actor: "a"}, {IdempotencyKey: "key-00000000", RequestHash: "x", Repo: "r", Actor: "a"}} {
		if _, _, err := s.BeginDashboardIssue(ctx, bad); err == nil {
			t.Fatalf("invalid row accepted: %+v", bad)
		}
	}
}
