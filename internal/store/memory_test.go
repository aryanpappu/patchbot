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

// decideAs decides with the entry's current repository and content hash.
func decideAs(t *testing.T, s *SQLite, id int64, action, actor, channel string) (MemoryEntry, error) {
	t.Helper()
	e, err := s.GetMemory(t.Context(), id)
	if err != nil {
		return MemoryEntry{}, err
	}
	return s.DecideMemory(t.Context(), MemoryDecision{ID: id, Action: action, Actor: actor, Channel: channel, Repo: e.Repo, ContentHash: e.ContentHash})
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
	}
	for _, tc := range tests {
		got, err := decideAs(t, s, tc.id, tc.action, "local", "ui")
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
	if _, err := s.DecideMemory(ctx, MemoryDecision{ID: 9999, Action: MemoryApprove, Actor: "local", Channel: "ui", Repo: "owner/repo", ContentHash: model.ContentHash}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	if _, err := decideAs(t, s, operator.ID, MemoryApprove, "", "ui"); err == nil {
		t.Fatal("decision without actor accepted")
	}
	if _, err := decideAs(t, s, operator.ID, MemoryForget, "local", "harness"); err == nil {
		t.Fatal("harness channel may not decide memory")
	}
}

func TestDecideMemoryRequiresShownRepoAndHash(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e, err := s.AddMemory(t.Context(), MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "Shown text.", CreatedBy: "m"}, "harness")
	if err != nil {
		t.Fatal(err)
	}
	other := hashOf("different text")
	for _, tc := range []struct {
		name, repo, hash string
		want             error
	}{
		{"wrong hash", "owner/repo", other, ErrConflict},
		{"wrong repo", "other/repo", e.ContentHash, ErrConflict},
		{"no hash", "owner/repo", "", nil},
		{"no repo", "", e.ContentHash, nil},
	} {
		_, err := s.DecideMemory(t.Context(), MemoryDecision{ID: e.ID, Action: MemoryApprove, Actor: "local", Channel: "cli", Repo: tc.repo, ContentHash: tc.hash})
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if got, _ := s.GetMemory(t.Context(), e.ID); got.Status != MemoryPending {
		t.Fatal("mismatched decision applied")
	}
}

func TestMemoryDuplicateContent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	lesson := MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "Same text.", CreatedBy: "m"}
	first, err := s.AddMemory(t.Context(), lesson, "harness")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMemory(t.Context(), lesson, "harness"); !errors.Is(err, ErrDuplicateMemory) || !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	lesson.Repo = "other/repo"
	if _, err := s.AddMemory(t.Context(), lesson, "harness"); err != nil {
		t.Fatalf("same text in another repo: %v", err)
	}
	if _, err := decideAs(t, s, first.ID, MemoryForget, "local", "cli"); err != nil {
		t.Fatal(err)
	}
	lesson.Repo = "owner/repo"
	if _, err := s.AddMemory(t.Context(), lesson, "harness"); err != nil {
		t.Fatalf("re-adding forgotten content: %v", err)
	}
	if n, err := s.CountMemory(t.Context(), "owner/repo", MemoryLesson, MemoryPending); err != nil || n != 1 {
		t.Fatalf("count %d %v", n, err)
	}
	if got, err := s.ListMemory(t.Context(), MemoryFilter{Repos: []string{"other/repo"}}); err != nil || len(got) != 1 || got[0].Repo != "other/repo" {
		t.Fatalf("repos filter: %+v %v", got, err)
	}
	if got, err := s.ListMemory(t.Context(), MemoryFilter{Repos: []string{}}); err != nil || len(got) != 0 {
		t.Fatalf("empty repos filter: %+v %v", got, err)
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
	revoked := add(observation("owner/repo", digest, now.Add(-time.Second)))
	if _, err := decideAs(t, s, revoked.ID, MemoryForget, "local", "cli"); err != nil {
		t.Fatal(err)
	}
	pending := add(MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "pending", CreatedBy: "m"})
	rejected := add(MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "rejected", CreatedBy: "m"})
	if _, err := decideAs(t, s, rejected.ID, MemoryReject, "local", "cli"); err != nil {
		t.Fatal(err)
	}
	approved := add(MemoryEntry{Repo: "owner/repo", Kind: MemoryLesson, Source: MemorySourceModel, Text: "approved", CreatedBy: "m"})
	if _, err := decideAs(t, s, approved.ID, MemoryApprove, "local", "cli"); err != nil {
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
