// Package repos is the effective repository allowlist: the read-only
// [repos."owner/name"] tables of the config file plus the repositories managed
// from the dashboard (stored in SQLite with an audit trail). The runner, the
// server, the webhook ingress, doctor and config show consult a Registry
// instead of reading config.Config.Repos where the allowlist matters.
//
// Dashboard repositories are validated by the same code as config repositories
// (config.Config.ValidateRepo) plus size limits and the workspace protected-path
// rules. A run freezes the settings it started with (see internal/runner), so
// an edit or removal here only affects runs that start afterwards.
package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// Sources of an effective repository.
const (
	SourceConfig    = "config"
	SourceDashboard = "dashboard"
)

// Errors returned by mutations. Validation failures wrap ErrInvalid and carry
// a message safe to show the operator (it never quotes a secret).
var (
	ErrInvalid   = errors.New("invalid repository")
	ErrReadOnly  = errors.New("repository comes from the config file and is read-only here")
	ErrCollision = errors.New("a repository with this name is defined in the config file")
	ErrNotFound  = store.ErrNotFound
)

// Limits keep one stored repository small enough to freeze as a run artifact.
const (
	maxCommands = 32
	maxArgv     = 64
	maxArgBytes = 4096
	maxProtect  = 64
	maxPathLen  = 512
)

// Settings is the editable part of a repository, in the shape of a
// [repos."owner/name"] table.
type Settings struct {
	// Commands maps a command ID to its argv (never a shell string).
	Commands    map[string][]string `json:"commands"`
	TestCommand string              `json:"test_command,omitempty"`
	Protected   []string            `json:"protected,omitempty"`
	// Image is digest-pinned (container mode only); Path is the container PATH.
	Image string   `json:"image,omitempty"`
	Path  []string `json:"path,omitempty"`
}

// FromRepo copies a config repository into Settings.
func FromRepo(r *config.Repo) Settings {
	if r == nil {
		return Settings{}
	}
	s := Settings{TestCommand: r.TestCommand, Image: r.Image, Protected: slices.Clone(r.Protected), Path: slices.Clone(r.Path)}
	s.Commands = make(map[string][]string, len(r.Commands))
	for id, argv := range r.Commands {
		s.Commands[id] = slices.Clone(argv)
	}
	return s
}

// Repo converts Settings back to a config.Repo (deep copy).
func (s Settings) Repo() *config.Repo {
	r := &config.Repo{TestCommand: s.TestCommand, Image: s.Image, Protected: slices.Clone(s.Protected), Path: slices.Clone(s.Path)}
	r.Commands = make(map[string][]string, len(s.Commands))
	for id, argv := range s.Commands {
		r.Commands[id] = slices.Clone(argv)
	}
	return r
}

// Entry is one effective repository.
type Entry struct {
	Name     string   `json:"name"`
	Source   string   `json:"source"`
	Settings Settings `json:"settings"`
	// Version is 0 for config repositories (read-only) and starts at 1 for
	// dashboard repositories; every edit adds one.
	Version   int64      `json:"version"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	CreatedBy string     `json:"created_by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	// BuiltinProtected are protected whatever the settings say.
	BuiltinProtected []string `json:"builtin_protected"`
}

// Registry is safe for concurrent use.
type Registry struct {
	cfg    *config.Config
	db     store.RepoStore
	redact func(string) string
	now    func() time.Time

	wmu  sync.Mutex // serializes mutations
	mu   sync.RWMutex
	dash map[string]Entry
}

// New loads the dashboard repositories. db may be nil (config repositories
// only; mutations then fail). redact must mask credential-shaped text; nil
// uses the config's redactor.
func New(ctx context.Context, cfg *config.Config, db store.RepoStore, redact func(string) string) (*Registry, error) {
	if redact == nil {
		redact = cfg.Redactor().Redact
	}
	r := &Registry{cfg: cfg, db: db, redact: redact, now: time.Now, dash: map[string]Entry{}}
	if err := r.Reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the dashboard repositories from the database. A stored row
// that no longer validates (for example after the executor mode changed) is
// skipped, so it can never widen the allowlist; List still reports nothing for
// it and the operator can remove or fix it by editing config.
func (r *Registry) Reload(ctx context.Context) error {
	if r.db == nil {
		return nil
	}
	recs, err := r.db.ListRepos(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]Entry, len(recs))
	for _, rec := range recs {
		e, err := r.entry(rec)
		if err != nil {
			continue
		}
		if _, clash := r.cfg.Repos[e.Name]; clash {
			continue // the config file wins; the row stays inert
		}
		next[e.Name] = e
	}
	r.mu.Lock()
	r.dash = next
	r.mu.Unlock()
	return nil
}

func (r *Registry) entry(rec store.RepoRecord) (Entry, error) {
	var s Settings
	if err := json.Unmarshal([]byte(rec.SettingsJSON), &s); err != nil {
		return Entry{}, err
	}
	if err := r.cfg.ValidateRepo(rec.Name, s.Repo()); err != nil {
		return Entry{}, err
	}
	created, updated := rec.CreatedAt, rec.UpdatedAt
	return Entry{Name: rec.Name, Source: SourceDashboard, Settings: s, Version: rec.Version,
		CreatedAt: &created, CreatedBy: rec.CreatedBy, UpdatedAt: &updated, UpdatedBy: rec.UpdatedBy,
		BuiltinProtected: workspace.BuiltinProtected()}, nil
}

// Normalize returns the canonical (lower-case, trimmed) repository name.
func Normalize(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// Lookup returns a private copy of the repository's settings.
func (r *Registry) Lookup(name string) (*config.Repo, bool) {
	e, ok := r.Get(name)
	if !ok {
		return nil, false
	}
	return e.Settings.Repo(), true
}

// Has reports whether name is on the allowlist.
func (r *Registry) Has(name string) bool {
	name = Normalize(name)
	if _, ok := r.cfg.Repos[name]; ok {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.dash[name]
	return ok
}

// Allowed is the single allowlist question: may runs, ingress and memory touch
// this repository? It is Has under the name callers outside this package use.
func (r *Registry) Allowed(name string) bool { return r.Has(name) }

// Get returns one effective repository.
func (r *Registry) Get(name string) (Entry, bool) {
	name = Normalize(name)
	if c, ok := r.cfg.Repos[name]; ok {
		return configEntry(name, c), true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.dash[name]
	return cloneEntry(e), ok
}

func configEntry(name string, c *config.Repo) Entry {
	return Entry{Name: name, Source: SourceConfig, Settings: FromRepo(c), BuiltinProtected: workspace.BuiltinProtected()}
}

func cloneEntry(e Entry) Entry {
	e.Settings = FromRepo(e.Settings.Repo())
	return e
}

// Names returns every allowlisted repository, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.cfg.Repos)+len(r.dash))
	for name := range r.dash {
		names = append(names, name)
	}
	r.mu.RUnlock()
	for name := range r.cfg.Repos {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// List returns the effective repositories sorted by name.
func (r *Registry) List() []Entry {
	names := r.Names()
	out := make([]Entry, 0, len(names))
	for _, name := range names {
		if e, ok := r.Get(name); ok {
			out = append(out, e)
		}
	}
	return out
}

// Validate applies every rule a dashboard repository must satisfy. name must
// already be normalized.
func (r *Registry) Validate(name string, s Settings) error {
	if _, clash := r.cfg.Repos[name]; clash {
		return ErrCollision
	}
	if len(s.Commands) > maxCommands || len(s.Protected) > maxProtect || len(s.Path) > maxProtect {
		return invalid("too many commands, protected paths or PATH entries")
	}
	for id, argv := range s.Commands {
		if len(argv) > maxArgv {
			return invalid("command %q has too many arguments", id)
		}
		for _, arg := range argv {
			if len(arg) > maxArgBytes {
				return invalid("command %q has an argument over %d bytes", id, maxArgBytes)
			}
		}
	}
	for _, p := range s.Protected {
		if len(p) > maxPathLen {
			return invalid("a protected path is over %d bytes", maxPathLen)
		}
	}
	if len(s.Image) > maxPathLen {
		return invalid("image is too long")
	}
	repo := s.Repo()
	if err := r.cfg.ValidateRepo(name, repo); err != nil {
		return invalid("%s", err)
	}
	if _, err := workspace.NewPolicy(repo.Protected); err != nil {
		return invalid("%s", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return invalid("settings cannot be encoded")
	}
	// The frozen run artifact and the audit rows are redacted on save. A value
	// that redaction would change cannot be stored losslessly: fail closed.
	if r.redact(string(raw)) != string(raw) {
		return invalid("a command, path or image looks like a credential and cannot be stored")
	}
	return nil
}

// ValidationError carries an operator-facing message.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

// Is makes errors.Is(err, ErrInvalid) true.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }

func invalid(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

func (r *Registry) auditJSON(s *Settings) string {
	if s == nil {
		return ""
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return r.redact(string(raw))
}

func (r *Registry) check() error {
	if r.db == nil {
		return errors.New("repository storage is not available")
	}
	return nil
}

// Add stores a new dashboard repository.
func (r *Registry) Add(ctx context.Context, name string, s Settings, actor, channel string) (Entry, error) {
	if err := r.check(); err != nil {
		return Entry{}, err
	}
	name = Normalize(name)
	if err := r.Validate(name, s); err != nil {
		return Entry{}, err
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	now := r.now()
	rec, err := r.db.AddRepo(ctx, store.RepoRecord{Name: name, SettingsJSON: r.auditJSON(&s), CreatedAt: now, CreatedBy: r.redact(actor)},
		store.RepoAudit{Repo: name, Action: store.RepoActionAdd, Actor: r.redact(actor), Channel: channel, AfterJSON: r.auditJSON(&s), At: now})
	if err != nil {
		return Entry{}, err
	}
	return r.publish(rec)
}

// Update replaces a dashboard repository's settings if its version is still
// expectedVersion.
func (r *Registry) Update(ctx context.Context, name string, s Settings, expectedVersion int64, actor, channel string) (Entry, error) {
	if err := r.check(); err != nil {
		return Entry{}, err
	}
	name = Normalize(name)
	if _, ok := r.cfg.Repos[name]; ok {
		return Entry{}, ErrReadOnly
	}
	if err := r.Validate(name, s); err != nil {
		return Entry{}, err
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	before, ok := r.Get(name)
	if !ok {
		return Entry{}, ErrNotFound
	}
	now := r.now()
	rec, err := r.db.UpdateRepo(ctx, store.RepoRecord{Name: name, SettingsJSON: r.auditJSON(&s), UpdatedAt: now, UpdatedBy: r.redact(actor)}, expectedVersion,
		store.RepoAudit{Repo: name, Action: store.RepoActionEdit, Actor: r.redact(actor), Channel: channel, BeforeJSON: r.auditJSON(&before.Settings), AfterJSON: r.auditJSON(&s), At: now})
	if err != nil {
		return Entry{}, err
	}
	return r.publish(rec)
}

// Remove deletes a dashboard repository unless it has active runs
// (store.ErrRepoBusy). Run history and the audit trail stay.
func (r *Registry) Remove(ctx context.Context, name string, expectedVersion int64, actor, channel string) error {
	if err := r.check(); err != nil {
		return err
	}
	name = Normalize(name)
	if _, ok := r.cfg.Repos[name]; ok {
		return ErrReadOnly
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	before, ok := r.Get(name)
	if !ok {
		return ErrNotFound
	}
	err := r.db.RemoveRepo(ctx, name, expectedVersion,
		store.RepoAudit{Repo: name, Action: store.RepoActionRemove, Actor: r.redact(actor), Channel: channel, BeforeJSON: r.auditJSON(&before.Settings), At: r.now()})
	if err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.dash, name)
	r.mu.Unlock()
	return nil
}

func (r *Registry) publish(rec store.RepoRecord) (Entry, error) {
	e, err := r.entry(rec)
	if err != nil {
		return Entry{}, err
	}
	r.mu.Lock()
	r.dash[e.Name] = e
	r.mu.Unlock()
	return cloneEntry(e), nil
}

// Audit returns the newest audit rows for a repository, also after removal.
func (r *Registry) Audit(ctx context.Context, name string) ([]store.RepoAudit, error) {
	if r.db == nil {
		return nil, nil
	}
	return r.db.ListRepoAudit(ctx, Normalize(name), 200)
}

// Merge adds dashboard records to a copy of the config repositories, for
// read-only consumers that hold no Registry (doctor, config show). Records
// that fail validation or collide with a config repository are returned in
// problems keyed by name.
func Merge(cfg *config.Config, recs []store.RepoRecord) (merged map[string]*config.Repo, dashboard []string, problems map[string]error) {
	merged = make(map[string]*config.Repo, len(cfg.Repos)+len(recs))
	for name, r := range cfg.Repos {
		merged[name] = r
	}
	problems = map[string]error{}
	for _, rec := range recs {
		var s Settings
		if err := json.Unmarshal([]byte(rec.SettingsJSON), &s); err != nil {
			problems[rec.Name] = errors.New("settings are unreadable")
			continue
		}
		if _, clash := cfg.Repos[rec.Name]; clash {
			problems[rec.Name] = ErrCollision
			continue
		}
		if err := cfg.ValidateRepo(rec.Name, s.Repo()); err != nil {
			problems[rec.Name] = err
			continue
		}
		merged[rec.Name] = s.Repo()
		dashboard = append(dashboard, rec.Name)
	}
	return merged, dashboard, problems
}
