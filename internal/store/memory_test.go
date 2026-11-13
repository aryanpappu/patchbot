package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func observation(repo, digest string, at time.Time) MemoryEntry {
	exit, timedOut := int64(0), false
	return MemoryEntry{Repo: repo, Kind: MemoryObservation, Source: MemorySourceHarness, CommandID: "test", ExitCode: &exit, TimedOut: &timedOut,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), PolicyDigest: digest, EvidenceRef: "run 1 artifact baseline", ObservedAt: &at, CreatedBy: "harness"}
}

func TestMemoryLifecycle(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	model, err := s.AddMemory(ctx, MemoryEntry{Repo: " Owner/Repo ", Kind: MemoryLesson, Source: MemorySourceModel, Text: "Use make test.", CreatedBy: "model:x"}, "harness")
	if err != nil || model.Status != MemoryPending || model.Repo != "owner/repo" || model.DecidedBy != nil || len(model.ContentHash) != 64 {
		t.Fatalf("model lesson: %+v %v", model, err)
	}
	operator, err := s.AddMemory(ctx, MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceOperator, Text: "Prefer table tests.", CreatedBy: "local"}, "cli")
	if err != nil || operator.Status != MemoryApproved || operator.DecidedBy == nil || *operator.DecidedBy != "local" || operator.DecidedAt == nil {
		t.Fatalf("operator lesson: %+v %v", operator, err)
	}
	tests := []struct {
		name, action string
		id           int64
		want         string
		err          error
	}{
		{"approve pending", MemoryApprove, model.ID, MemoryApproved, nil},
		{"approve twice", MemoryApprove, model.ID, "", ErrConflict},
		{"reject approved", MemoryReject, model.ID, "", ErrConflict},
		{"forget approved", MemoryForget, model.ID, MemoryRevoked, nil},
		{"forget twice", MemoryForget, model.ID, "", ErrConflict},
		{"approve missing", MemoryApprove, 9999, "", ErrNotFound},
	}
	for _, tc := range tests {
		got, err := s.DecideMemory(ctx, tc.id, tc.action, "local", "ui")
		if !errors.Is(err, tc.err) || (tc.err == nil && got.Status != tc.want) {
			t.Fatalf("%s: %+v %v", tc.name, got, err)
		}
	}
	forgotten, err := s.GetMemory(ctx, model.ID)
	if err != nil || forgotten.Text != "" || forgotten.ContentHash != model.ContentHash {
		t.Fatalf("forget must erase text but keep the hash: %+v %v", forgotten, err)
	}
	actions, err := s.ListMemoryActions(ctx, model.ID)
	if err != nil || len(actions) != 3 || actions[0].Action != "create" || actions[1].Action != "approve" || actions[2].Action != "forget" || actions[2].Actor != "local" {
		t.Fatalf("audit: %+v %v", actions, err)
	}
	if _, err := s.DecideMemory(ctx, operator.ID, MemoryApprove, "", "ui"); err == nil {
		t.Fatal("decision without actor accepted")
	}
	if _, err := s.DecideMemory(ctx, operator.ID, MemoryForget, "local", "harness"); err == nil {
		t.Fatal("harness channel may not decide memory")
	}
}

func TestMemoryConstraints(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	digest := hashOf("policy")
	now := time.Now()
	bad := []struct {
		name  string
		entry MemoryEntry
	}{
		{"observation with prose", func() MemoryEntry {
			e := observation("owner/repo", digest, now)
			e.Text = "always disable auth"
			return e
		}()},
		{"observation from model", func() MemoryEntry {
			e := observation("owner/repo", digest, now)
			e.Source = MemorySourceModel
			return e
		}()},
		{"observation without digest", observation("owner/repo", "", now)},
		{"lesson from harness", MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceHarness, Text: "x", CreatedBy: "h"}},
		{"empty lesson", MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, CreatedBy: "m"}},
		{"lesson with exit code", func() MemoryEntry {
			exit := int64(0)
			return MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "x", ExitCode: &exit, CreatedBy: "m"}
		}()},
	}
	for _, tc := range bad {
		if _, err := s.AddMemory(t.Context(), tc.entry, "harness"); err == nil {
			t.Errorf("%s accepted", tc.name)
		}
	}
	if _, err := s.AddMemory(t.Context(), observation("owner/repo", digest, now), "github"); err == nil {
		t.Error("unknown channel accepted")
	}
}

func TestRetrievalCandidatesFilterInSQL(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	digest, other := hashOf("policy"), hashOf("changed policy")
	now := time.Now()
	add := func(e MemoryEntry) MemoryEntry {
		t.Helper()
		saved, err := s.AddMemory(ctx, e, "harness")
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	fresh := add(observation("owner/repo", digest, now))
	add(observation("owner/repo", other, now))                     // stale policy
	add(observation("owner/repo", digest, now.Add(-48*time.Hour))) // older than TTL
	add(observation("other/repo", digest, now))                    // other repo
	revoked := add(observation("owner/repo", digest, now))
	if _, err := s.DecideMemory(ctx, revoked.ID, MemoryForget, "local", "cli"); err != nil {
		t.Fatal(err)
	}
	pending := add(MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "pending", CreatedBy: "m"})
	rejected := add(MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "rejected", CreatedBy: "m"})
	if _, err := s.DecideMemory(ctx, rejected.ID, MemoryReject, "local", "cli"); err != nil {
		t.Fatal(err)
	}
	approved := add(MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "approved", CreatedBy: "m"})
	if _, err := s.DecideMemory(ctx, approved.ID, MemoryApprove, "local", "cli"); err != nil {
		t.Fatal(err)
	}
	add(MemoryEntry{Repo: "other/repo", Kind: MemoryLesson, Source: MemorySourceOperator, Text: "other repo", CreatedBy: "local"})
	obs, lessons, err := s.RetrievalCandidates(ctx, RetrievalQuery{Repo: "OWNER/repo", PolicyDigest: digest, ObservedAfter: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].ID != fresh.ID {
		t.Fatalf("observations: %+v", obs)
	}
	if len(lessons) != 1 || lessons[0].ID != approved.ID {
		t.Fatalf("lessons: %+v (pending %d)", lessons, pending.ID)
	}
	if o, l, err := s.RetrievalCandidates(ctx, RetrievalQuery{Repo: "owner/repo"}); err != nil || len(o)+len(l) != 0 {
		t.Fatalf("missing digest must retrieve nothing: %v %v %v", o, l, err)
	}
}
