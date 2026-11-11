package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestBudgets(t *testing.T) {
	t.Run("hard maximum", func(t *testing.T) {
		f := newFixture(t, "true", specStep())
		run, err := f.r.Start(t.Context(), runner.Request{Repo: "owner/repo", Issue: 12, MaxCostMicros: 99_000_000})
		if err != nil {
			t.Fatal(err)
		}
		var limits struct{ Agent agent.Limits }
		if err := json.Unmarshal(run.LimitsJSON, &limits); err != nil {
			t.Fatal(err)
		}
		if limits.Agent.MaxCostMicros != 10_000_000 {
			t.Fatalf("cap: %d", limits.Agent.MaxCostMicros)
		}
	})
	t.Run("shared across revisions and process", func(t *testing.T) {
		step := specStep()
		step.Usage = &llm.Usage{InputTokens: 1_999_000}
		f := newFixture(t, "true", step, specStep())
		rate := 1.0
		f.cfg.Providers["anthropic"].PriceFree = false
		f.cfg.Providers["anthropic"].Prices = map[string]*config.Price{"test-model": {Input: &rate, Output: &rate}}
		run := f.start("fix", false)
		restored, err := runner.New(t.Context(), f.cfg, f.db, f.opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := restored.Revise(t.Context(), run.ID, "again"); err == nil {
			t.Fatal("revision reset the budget")
		}
		if got := f.get(run.ID); got.Status != store.StatusFailed || got.ErrorKind != "budget_exhausted" {
			t.Fatalf("result: %+v", got)
		}
		if f.p.Calls() != 1 || f.branches() != "" {
			t.Fatal("exhausted run called provider or pushed")
		}
	})
	t.Run("tiny budget", func(t *testing.T) {
		f := newFixture(t, "true", specStep())
		rate := 1.0
		f.cfg.Providers["anthropic"].PriceFree = false
		f.cfg.Providers["anthropic"].Prices = map[string]*config.Price{"test-model": {Input: &rate, Output: &rate}}
		run, err := f.r.Start(t.Context(), runner.Request{Repo: "owner/repo", Issue: 12, MaxCostMicros: 1})
		if err == nil || run.ErrorKind != "budget_exhausted" || f.p.Calls() != 0 {
			t.Fatalf("budget: %+v, %v", run, err)
		}
	})
	t.Run("unknown usage charged", func(t *testing.T) {
		step := specStep()
		step.Usage = nil
		f := newFixture(t, "true", step)
		run := f.start("fix", true)
		artifact, err := f.db.GetArtifact(t.Context(), run.ID, "accounting", 0)
		if err != nil {
			t.Fatal(err)
		}
		var totals struct {
			Tokens    int64
			Uncertain bool
		}
		if err := json.Unmarshal([]byte(artifact.Content), &totals); err != nil {
			t.Fatal(err)
		}
		if totals.Tokens <= 8192 || !totals.Uncertain || run.InputTokens != nil {
			t.Fatalf("lost unknown usage: %s", artifact.Content)
		}
	})
}

func TestExpiryAndStartupRecovery(t *testing.T) {
	f := newFixture(t, "true")
	old := time.Now().UTC().Add(-15 * 24 * time.Hour)
	waiting, err := f.db.CreateRun(t.Context(), store.Run{Repo: "owner/repo", IssueNumber: 1, Mode: "fix", Provider: "anthropic", Status: store.StatusAwaitingApproval, CreatedAt: old, WaitingSince: &old})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := f.db.CreateRun(t.Context(), store.Run{Repo: "owner/repo", IssueNumber: 2, Mode: "fix", Provider: "anthropic", Status: store.StatusRunning, CreatedAt: old, HeartbeatAt: &old})
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().UTC()
	healthy, err := f.db.CreateRun(t.Context(), store.Run{Repo: "owner/repo", IssueNumber: 3, Mode: "fix", Provider: "anthropic", Status: store.StatusRunning, HeartbeatAt: &recent})
	if err != nil {
		t.Fatal(err)
	}
	service, err := runner.New(t.Context(), f.cfg, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.get(waiting.ID); got.Status != store.StatusFailed || got.ErrorKind != "expired" {
		t.Fatalf("expiry: %+v", got)
	}
	if got := f.get(stale.ID); got.Status != store.StatusFailed || got.ErrorKind != "interrupted" {
		t.Fatalf("stale: %+v", got)
	}
	if got := f.get(healthy.ID); got.Status != store.StatusRunning {
		t.Fatal("fresh heartbeat failed")
	}
	if err := service.SyncWaiting(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{waiting.ID, stale.ID} {
		events, err := f.db.EventsAfter(t.Context(), id, 0, 50)
		if err != nil || len(events) != 1 || events[0].Type != "run.status" {
			t.Fatalf("missing recovery event: %v, %v", events, err)
		}
	}
}

func TestRevisionLimit(t *testing.T) {
	f := newFixture(t, "true", specStep(), specStep(), specStep(), specStep(), specStep(), specStep())
	run := f.start("fix", true)
	for range 5 {
		if err := f.r.Revise(t.Context(), run.ID, "refine again"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.Revise(t.Context(), run.ID, "sixth revision"); err == nil {
		t.Fatal("sixth revision accepted")
	}
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != 6 || f.p.Calls() != 6 {
		t.Fatal("revision limit was not enforced")
	}
	f.approve(run.ID)
}

func TestRetryRules(t *testing.T) {
	for _, change := range []string{"unchanged", "body changed", "spec edited", "approval expired", "rejected", "provider override"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t, "true", specStep(), specStep(), writeStep(), submitStep())
			run := f.start("fix", false)
			if change == "rejected" {
				if err := f.r.Reject(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.approve(run.ID)
			}
			if change == "spec edited" {
				d := draft()
				d.Summary = "edited"
				if _, err := f.r.EditSpec(t.Context(), run.ID, d); err != nil {
					t.Fatal(err)
				}
			}
			if change != "rejected" {
				if err := f.r.Cancel(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if change == "body changed" {
				f.mu.Lock()
				f.issue.Body += " new requirements"
				f.mu.Unlock()
			}
			if change == "approval expired" {
				f.mu.Lock()
				f.offset = 15 * 24 * time.Hour
				f.mu.Unlock()
			}
			provider, model := "", ""
			if change == "provider override" {
				f.cfg.Providers["other"] = &config.Provider{Kind: "anthropic", DefaultModel: "other-model", PriceFree: true}
				f.opts.Constructors["anthropic"] = func(c llm.ProviderConfig) (llm.Provider, error) {
					p := f.p
					if c.Name == "other" {
						return namedProvider{Provider: p, name: "other"}, nil
					}
					return p, nil
				}
				provider, model = "other", "override-model"
			}
			retried, err := f.r.Retry(t.Context(), run.ID, provider, model)
			if err != nil {
				t.Fatal(err)
			}
			if retried.ID == run.ID || retried.RetryOf == nil || *retried.RetryOf != run.ID {
				t.Fatalf("retry linkage: %+v", retried)
			}
			reuse := change == "unchanged" || change == "provider override"
			if reuse {
				if retried.Status != store.StatusApproved || retried.BaseSHA != run.BaseSHA || f.p.Calls() != 1 {
					t.Fatalf("did not reuse: %+v", retried)
				}
				if change == "provider override" && (retried.Provider != provider || retried.CodeModel != model || retried.RefineModel != model) {
					t.Fatal("override ignored")
				}
			} else if retried.Status != store.StatusAwaitingApproval || f.p.Calls() != 2 {
				t.Fatalf("invalid spec reused: %+v", retried)
			}
		})
	}
}

type namedProvider struct {
	llm.Provider
	name string
}

func (p namedProvider) Name() string { return p.name }

func TestDefaultBranchMovedAndPinnedBase(t *testing.T) {
	f := newFixture(t, "true", specStep(), writeStep(), submitStep())
	run := f.start("fix", false)
	if err := os.WriteFile(filepath.Join(f.seed, "new-default.txt"), []byte("new tip"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "advance default")
	git(t, f.seed, "push", f.remote, "main")
	moved := git(t, f.seed, "rev-parse", "HEAD")
	f.mu.Lock()
	f.base = moved
	f.mu.Unlock()
	f.approve(run.ID)
	if err := f.r.Execute(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.prs[0].Body, "default branch has moved") {
		t.Fatal("missing moved-default note")
	}
	parent := git(t, f.root, "--git-dir", f.remote, "rev-parse", "patchbot/issue-12^")
	if parent != run.BaseSHA {
		t.Fatal("code did not use approved base")
	}
}

func TestSpecStoreCannotEditAfterCodeClaim(t *testing.T) {
	f := newFixture(t, "true", specStep())
	run := f.start("fix", true)
	f.approve(run.ID)
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.StartCode(t.Context(), run.ID, spec.Version, spec.Hash, f.get(run.ID).Generation); err != nil {
		t.Fatal(err)
	}
	spec.Version++
	if _, err := f.db.SaveSpec(t.Context(), spec); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("code-time edit accepted: %v", err)
	}
	if err := f.db.StartCode(context.Background(), run.ID, spec.Version, spec.Hash, f.get(run.ID).Generation); !errors.Is(err, store.ErrConflict) {
		t.Fatal("duplicate start accepted")
	}
}

func TestSalvageAndPublicationSafety(t *testing.T) {
	t.Run("salvage at turn cap", func(t *testing.T) {
		f := newFixture(t, "true", specStep(), writeStep())
		f.cfg.Limits.MaxTurns = 1
		run := f.start("fix", false)
		f.approve(run.ID)
		if err := f.r.Execute(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		got := f.get(run.ID)
		if got.Outcome != "pr_draft" || got.TestAttempts != 1 || !strings.Contains(f.prs[0].Body, "max_turns") {
			t.Fatalf("salvage: %+v", got)
		}
	})
	t.Run("no test command", func(t *testing.T) {
		f := newFixture(t, "true", specStep(), writeStep(), submitStep())
		f.cfg.Repos["owner/repo"].Commands = nil
		run := f.start("fix", false)
		f.approve(run.ID)
		if err := f.r.Execute(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		if got := f.get(run.ID); got.Outcome != "pr_draft" || got.TestsStatus != "not_run" || !strings.Contains(f.prs[0].Body, "Unverified") {
			t.Fatalf("unverified: %+v", got)
		}
	})
	t.Run("no changes", func(t *testing.T) {
		f := newFixture(t, "true", specStep(), submitStep())
		run := f.start("fix", false)
		f.approve(run.ID)
		if err := f.r.Execute(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		if got := f.get(run.ID); got.Outcome != "no_changes" || got.PRURL != "" || f.branches() != "" {
			t.Fatalf("empty patch: %+v", got)
		}
	})
	t.Run("read-only label precedence", func(t *testing.T) {
		f := newFixture(t, "true", toolStep("submit_report", map[string]string{"report_md": "Analysis"}))
		f.issue.Labels = []github.Label{{Name: "ai-fix"}, {Name: "ai-refine"}, {Name: "ai-investigate"}}
		run := f.start("", false)
		if run.Mode != "investigate" || run.Outcome != "investigated" {
			t.Fatalf("labels: %+v", run)
		}
	})
}

func TestPreparedCodeCrashRecovery(t *testing.T) {
	f := newFixture(t, "true")
	old := time.Now().Add(-2 * time.Minute)
	run, err := f.db.CreateRun(t.Context(), store.Run{Repo: "owner/repo", IssueNumber: 12, Mode: "fix", Provider: "anthropic", Status: store.StatusApproved, Phase: store.PhaseBaseline, HeartbeatAt: &old})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.New(t.Context(), f.cfg, f.db, f.opts); err != nil {
		t.Fatal(err)
	}
	if got := f.get(run.ID); got.Status != store.StatusFailed || got.ErrorKind != "interrupted" {
		t.Fatalf("prepared code left active: %+v", got)
	}
}

func TestCommentTamperingAdviceAndBotIdentity(t *testing.T) {
	f := newFixture(t, "true", specStep())
	f.opts.BotLogin = ""
	service, err := runner.New(t.Context(), f.cfg, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	run := f.start("fix", false)
	f.mu.Lock()
	f.comments[0].Body += " changed on GitHub"
	f.mu.Unlock()
	f.addCommand("owner", "/approve", false)
	if err := service.SyncWaiting(t.Context()); !errors.Is(err, agent.ErrApprovalDenied) {
		t.Fatal(err)
	}
	if f.posts != 2 || !strings.Contains(f.comments[len(f.comments)-1].Body, "not imported") {
		t.Fatal("missing revise advice")
	}
	if err := service.SyncWaiting(t.Context()); !errors.Is(err, agent.ErrApprovalDenied) {
		t.Fatal(err)
	}
	if f.posts != 2 || f.get(run.ID).Status != store.StatusAwaitingApproval {
		t.Fatal("advice repeated or edited spec approved")
	}
}
