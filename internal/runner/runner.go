// Package runner orchestrates durable issue runs and their approval boundary.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// Options supplies trusted infrastructure. Clone, Executor and Constructors are
// test seams; production uses gitx, execx and the raw provider constructors.
type Options struct {
	GitHub            github.Client
	Tokens            github.TokenSource
	Constructors      llm.Constructors
	Clone             func(context.Context, gitx.CloneOptions) (*gitx.Repo, error)
	Executor          func(string) (execx.Executor, error)
	Clock             func() time.Time
	BotLogin          string
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
	// DisableFailureComments suppresses the otherwise enabled failure comment.
	DisableFailureComments bool
}

// Request fixes provider/model and limits for the lifetime of a new run.
type Request struct {
	Repo                  string
	Issue                 int64
	Mode, Provider, Model string
	DryRun                bool
	MaxCostMicros         int64
}

// Runner is safe for concurrent actions. Long work owns a per-run context;
// Cancel also works from another process through the persisted status.
type Runner struct {
	cfg      *config.Config
	db       store.Store
	gh       github.Client
	opts     Options
	redactor *secret.Redactor
	mu       sync.Mutex
	active   map[int64]context.CancelFunc
	controls map[int64]*sync.Mutex
}

type runLimits struct {
	Agent       agent.Limits
	RefineTurns int
	Timeout     time.Duration
}

// Token is a secret-backed PAT source, shared by GitHub and git clone/push.
type Token struct{ Value secret.Secret }

// Token supplies a credential without embedding it in diagnostics.
func (t Token) Token(context.Context) (string, error) {
	if !t.Value.IsSet() {
		return "", errors.New("set GITHUB_TOKEN")
	}
	return t.Value.Reveal(), nil
}

// New prepares the runner and recovers expired heartbeats on startup.
// The caller owns the store and closes it after runner work has finished.
func New(ctx context.Context, cfg *config.Config, db store.Store, opts Options) (*Runner, error) {
	if cfg == nil || db == nil {
		return nil, errors.New("runner requires configuration and storage")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Clone == nil {
		opts.Clone = gitx.Clone
	}
	if opts.Executor == nil {
		opts.Executor = func(dir string) (execx.Executor, error) { return execx.NewLocalExecutor(dir) }
	}
	if opts.Constructors == nil {
		opts.Constructors = providers.Constructors(providers.Options{})
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 10 * time.Second
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = time.Minute
	}
	if opts.Tokens == nil {
		opts.Tokens = Token{Value: secret.New(os.Getenv("GITHUB_TOKEN"))}
	}
	gh := opts.GitHub
	if gh == nil {
		var err error
		gh, err = github.New(nil, "", opts.Tokens)
		if err != nil {
			return nil, err
		}
	}
	r := &Runner{cfg: cfg, db: db, gh: gh, opts: opts, redactor: cfg.Redactor(), active: make(map[int64]context.CancelFunc), controls: make(map[int64]*sync.Mutex)}
	if token, err := opts.Tokens.Token(ctx); err == nil {
		r.redactor.AddSecrets(secret.New(token))
	}
	if _, err := db.FailStaleRuns(ctx, r.now().Add(-opts.StaleAfter)); err != nil {
		return nil, err
	}
	manager, err := workspace.NewManager(cfg.DataDir())
	if err != nil {
		return nil, err
	}
	_, sweepErr := manager.Sweep(ctx, max(24*time.Hour, cfg.Limits.RunTimeout+cfg.Limits.TestTimeout+opts.StaleAfter))
	if err := errors.Join(sweepErr, manager.Close()); err != nil {
		return nil, err
	}
	if err := r.expireWaiting(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Runner) now() time.Time { return r.opts.Clock().UTC() }
func (r *Runner) control(id int64) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.controls[id] == nil {
		r.controls[id] = &sync.Mutex{}
	}
	return r.controls[id]
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func ref(repo string) github.RepoRef {
	owner, name, _ := strings.Cut(repo, "/")
	return github.RepoRef{Owner: owner, Name: name}
}

func (r *Runner) limits(req Request) (runLimits, error) {
	budget, hard, err := r.cfg.MaxCostMicro()
	if err != nil {
		return runLimits{}, err
	}
	if req.MaxCostMicros < 0 {
		return runLimits{}, errors.New("budget must be positive")
	}
	if req.MaxCostMicros > 0 {
		budget = req.MaxCostMicros
	}
	if budget <= 0 || hard <= 0 {
		return runLimits{}, errors.New("budget and hard maximum must be positive")
	}
	budget = min(budget, hard)
	c := r.cfg.Limits
	return runLimits{Agent: agent.Limits{MaxTurns: c.MaxTurns, MaxOutputTokens: c.MaxOutputTokens, MaxTotalTokens: c.MaxTotalTokens, MaxCostMicros: budget, MaxTestAttempts: min(c.MaxTestAttempts, 3), TestTimeout: c.TestTimeout}, RefineTurns: c.RefineMaxTurns, Timeout: c.RunTimeout}, nil
}

func (r *Runner) provider(name string) (llm.Provider, llm.Pricing, error) {
	configs, err := r.cfg.LLMProviders()
	if err != nil {
		return nil, llm.Pricing{}, err
	}
	for _, cfg := range configs {
		if cfg.Name != name {
			continue
		}
		ctor := r.opts.Constructors[cfg.Kind]
		if ctor == nil {
			return nil, llm.Pricing{}, llm.ErrNoAdapter
		}
		if cfg.APIKeyEnv != "" && !cfg.APIKey.IsSet() {
			return nil, llm.Pricing{}, fmt.Errorf("provider %s: set %s", name, cfg.APIKeyEnv)
		}
		p, err := ctor(cfg)
		if err != nil {
			return nil, llm.Pricing{}, errors.New("could not construct provider")
		}
		if p == nil || p.Name() != name {
			return nil, llm.Pricing{}, errors.New("provider identity mismatch")
		}
		return p, cfg.Pricing, nil
	}
	return nil, llm.Pricing{}, llm.ErrUnknownProvider
}

func (r *Runner) prepare(req Request) (store.Run, error) {
	req.Repo = strings.ToLower(strings.TrimSpace(req.Repo))
	if !repoPattern.MatchString(req.Repo) || req.Issue <= 0 {
		return store.Run{}, errors.New("require --repo owner/name and a positive --issue")
	}
	if r.cfg.Repos[req.Repo] == nil {
		return store.Run{}, errors.New("repository is not in configuration allowlist")
	}
	if req.Mode == "" {
		req.Mode = r.cfg.Defaults.Mode
	}
	if req.Mode != config.ModeFix && req.Mode != config.ModeRefine && req.Mode != config.ModeInvestigate {
		return store.Run{}, errors.New("mode must be fix, refine or investigate")
	}
	if req.Provider == "" {
		req.Provider = r.cfg.Defaults.Provider
	}
	p := r.cfg.Providers[req.Provider]
	if p == nil {
		return store.Run{}, llm.ErrUnknownProvider
	}
	refine, code := r.cfg.Defaults.RefineModel, r.cfg.Defaults.CodeModel
	if req.Provider != r.cfg.Defaults.Provider {
		refine, code = "", ""
	}
	if req.Model != "" {
		refine, code = req.Model, req.Model
	}
	if refine == "" {
		refine = p.DefaultModel
	}
	if code == "" {
		code = p.DefaultModel
	}
	if refine == "" || code == "" || strings.ContainsAny(refine+code, "\r\n\x00") {
		return store.Run{}, errors.New("set a valid model for both stages")
	}
	if _, _, err := r.provider(req.Provider); err != nil {
		return store.Run{}, err
	}
	limits, err := r.limits(req)
	if err != nil {
		return store.Run{}, err
	}
	raw, err := json.Marshal(limits)
	if err != nil {
		return store.Run{}, err
	}
	return store.Run{Repo: req.Repo, IssueNumber: req.Issue, Mode: req.Mode, Provider: req.Provider, RefineModel: refine, CodeModel: code, DryRun: req.DryRun, Status: store.StatusQueued, Phase: store.PhasePreflight, LimitsJSON: raw}, nil
}

// Start creates and executes the read-only phase, returning a durable waiting
// run (or a completed investigation). It never bypasses approval, even dry-run.
func (r *Runner) Start(ctx context.Context, req Request) (store.Run, error) {
	run, err := r.prepare(req)
	if err != nil {
		return run, err
	}
	if req.Mode == "" {
		issue, err := r.gh.GetIssue(ctx, ref(run.Repo), int(run.IssueNumber))
		if err != nil {
			return store.Run{}, err
		}
		for _, mode := range []string{config.ModeInvestigate, config.ModeRefine, config.ModeFix} {
			for _, label := range issue.Labels {
				if label.Name == "ai-"+mode {
					run.Mode = mode
					return r.start(ctx, run, nil)
				}
			}
		}
	}
	return r.start(ctx, run, nil)
}

func (r *Runner) start(ctx context.Context, run store.Run, reuse *store.Spec) (store.Run, error) {
	run, err := r.db.CreateRun(ctx, run)
	if err != nil {
		return run, err
	}
	if err = r.action(ctx, run.ID, "start", r.cfg.Approval.LocalActor, "cli", nil); err == nil {
		err = r.withWork(ctx, run.ID, func(work context.Context) error {
			if err := r.transition(work, &run, store.StatusRunning, store.PhasePreflight, store.RunPatch{}); err != nil {
				return err
			}
			return r.refine(work, &run, nil, "", reuse)
		})
	}
	if err != nil {
		err = r.fail(ctx, run, err)
	}
	latest, readErr := r.db.GetRun(context.WithoutCancel(ctx), run.ID)
	return latest, errors.Join(err, readErr)
}

func (r *Runner) transition(ctx context.Context, run *store.Run, status store.Status, phase store.Phase, patch store.RunPatch) error {
	patch.Phase = phase
	patch.ExpectedGeneration = &run.Generation
	if err := r.db.SetRunStatus(ctx, run.ID, run.Status, status, patch); err != nil {
		return err
	}
	updated, err := r.db.GetRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if updated.Status != status || updated.Generation != run.Generation+1 {
		return store.ErrConflict
	}
	*run = updated
	return nil
}

func (r *Runner) action(ctx context.Context, id int64, kind, actor, channel string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	// Data is harness-owned; all user text is redacted before encoding.
	_, err = r.db.RecordAction(ctx, store.Action{RunID: id, Kind: kind, Actor: r.redactor.Redact(actor), Channel: channel, PayloadJSON: raw})
	return err
}

func (r *Runner) artifact(ctx context.Context, id int64, kind, content string) error {
	return r.db.SaveArtifact(ctx, store.Artifact{RunID: id, Kind: kind, Content: tools.CapOutput(r.redactor.Redact(content), 256<<10)})
}
