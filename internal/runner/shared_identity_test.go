package runner_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

// lossyStore simulates lost provenance writes: the spec's comment ID and/or
// patchbot's posted-comment record silently never reach the database.
type lossyStore struct {
	store.Store
	dropSpecComment, dropPostedRecord bool
}

func (s *lossyStore) SetSpecComment(ctx context.Context, runID, version int64, hash string, id int64) error {
	if s.dropSpecComment {
		return nil
	}
	return s.Store.SetSpecComment(ctx, runID, version, hash, id)
}

func (s *lossyStore) RecordPostedComment(ctx context.Context, c store.PostedComment) error {
	if s.dropPostedRecord {
		return nil
	}
	return s.Store.RecordPostedComment(ctx, c)
}

// sharedFixture makes the token owner "owner" (an approver and collaborator)
// the identity that posts patchbot's comments, as with a single-user PAT.
func sharedFixture(t *testing.T, bot string, db store.Store, steps ...fake.Step) *fixture {
	t.Helper()
	f := newFixture(t, "true", steps...)
	f.botAuthor = bot
	f.opts.BotLogin = bot
	if db == nil {
		db = f.db
	}
	var err error
	f.r, err = runner.New(t.Context(), f.cfg, db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSharedIdentityOwnerApprovalAccepted(t *testing.T) {
	f := sharedFixture(t, "owner", nil, specStep(), writeStep(), submitStep())
	run := f.start("fix", false)
	f.addCommand("owner", "/approve", false)
	if err := f.r.SyncWaiting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.get(run.ID).Status; got != store.StatusSucceeded {
		t.Fatalf("owner-as-token-owner /approve: status %s", got)
	}
	approvals, err := f.db.ListApprovals(t.Context(), run.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Actor != "owner" || approvals[0].Channel != "github" {
		t.Fatalf("approvals = %+v, %v", approvals, err)
	}
	// Every comment patchbot posted (spec, PR note) is marked and recorded.
	f.mu.Lock()
	comments := append([]github.Comment(nil), f.comments...)
	f.mu.Unlock()
	var patchbotPosts int
	for _, c := range comments {
		if c.Body == "/approve" {
			continue
		}
		patchbotPosts++
		if !github.HasPatchbotMarker(c.Body) {
			t.Errorf("posted comment %d lacks a patchbot marker: %q", c.ID, c.Body)
		}
		if posted, err := f.db.IsPostedComment(t.Context(), c.ID); err != nil || !posted {
			t.Errorf("posted comment %d not recorded: %v, %v", c.ID, posted, err)
		}
	}
	if patchbotPosts < 2 {
		t.Fatalf("expected spec and PR-note comments, got %d", patchbotPosts)
	}
}

func TestSharedIdentityRejections(t *testing.T) {
	for _, tt := range []struct {
		name   string
		bot    string
		lossy  *lossyStore
		prefix string
		add    func(f *fixture, run store.Run)
	}{
		{
			// The spec's own comment ID was lost, so ordering no longer hides
			// it, and the (model-written) spec body begins with /approve.
			name: "spec comment beginning with /approve", bot: "owner",
			lossy: &lossyStore{dropSpecComment: true}, prefix: "/approve\n\n",
		},
		{
			// Both provenance writes were lost: only the marker remains.
			name: "marker without ID record", bot: "owner",
			lossy: &lossyStore{dropSpecComment: true, dropPostedRecord: true}, prefix: "/approve\n\n",
		},
		{
			name: "recorded ID without marker", bot: "owner",
			add: func(f *fixture, run store.Run) {
				f.addCommand("owner", "/approve", false)
				f.mu.Lock()
				id := f.comments[len(f.comments)-1].ID
				f.mu.Unlock()
				if err := f.db.RecordPostedComment(f.t.Context(), store.PostedComment{CommentID: id, RunID: run.ID,
					Repo: run.Repo, IssueNumber: run.IssueNumber, Kind: "test"}); err != nil {
					f.t.Fatal(err)
				}
			},
		},
		{
			name: "edited owner comment", bot: "owner",
			add: func(f *fixture, _ store.Run) { f.addCommand("owner", "/approve", true) },
		},
		{
			name: "owner comment quoting a marker", bot: "owner",
			add: func(f *fixture, _ store.Run) { f.addCommand("owner", "/approve\n<!-- patchbot:spec -->", false) },
		},
		{
			// "outsider" is a collaborator but not a configured approver.
			name: "non-approver bot login", bot: "outsider",
			add: func(f *fixture, _ store.Run) { f.addCommand("outsider", "/approve", false) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var f *fixture
			if tt.lossy != nil {
				// Build the base fixture first so the lossy wrapper can wrap its DB.
				base := newFixture(t, "true", specStep())
				tt.lossy.Store = base.db
				base.botAuthor, base.postPrefix, base.opts.BotLogin = tt.bot, tt.prefix, tt.bot
				// GitHub's clock runs ahead of the host, so the spec comment
				// falls inside the waiting window instead of before it.
				base.postSkew = time.Minute
				var err error
				base.r, err = runner.New(t.Context(), base.cfg, tt.lossy, base.opts)
				if err != nil {
					t.Fatal(err)
				}
				f = base
			} else {
				f = sharedFixture(t, tt.bot, nil, specStep())
			}
			run := f.start("fix", false)
			if got := f.get(run.ID).Status; got != store.StatusAwaitingApproval {
				t.Fatalf("status after spec: %s", got)
			}
			if tt.add != nil {
				tt.add(f, run)
			}
			if err := f.r.SyncWaiting(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := f.get(run.ID).Status; got != store.StatusAwaitingApproval || f.p.Calls() != 1 {
				t.Fatalf("shared-identity comment counted: status %s, calls %d", got, f.p.Calls())
			}
			if approvals, err := f.db.ListApprovals(t.Context(), run.ID); err != nil || len(approvals) != 0 {
				t.Fatalf("approvals recorded: %+v, %v", approvals, err)
			}
			if tt.prefix != "" {
				f.mu.Lock()
				spec := f.comments[0]
				f.mu.Unlock()
				if !strings.HasPrefix(spec.Body, "/approve") || spec.Author != tt.bot {
					t.Fatalf("fixture did not simulate a bot-authored /approve spec: %+v", spec)
				}
			}
		})
	}
}
