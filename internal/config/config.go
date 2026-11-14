// Package config loads, layers and validates the patchbot configuration.
//
// Values come from four layers, later ones winning: the embedded defaults, a
// TOML file, PATCHBOT_* environment variables and command-line flags. The
// loader records which layer set each value. Secrets never come from a file:
// the file names an environment variable and the loader reads the secret from
// it into a secret.Secret.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// Provider kinds.
const (
	KindAnthropic = "anthropic"
	KindGemini    = "gemini"
	KindOpenAI    = "openai"
)

// Run modes.
const (
	ModeFix         = "fix"
	ModeRefine      = "refine"
	ModeInvestigate = "investigate"
)

// Config is the effective configuration.
type Config struct {
	Defaults  Defaults             `toml:"defaults"`
	Limits    Limits               `toml:"limits"`
	Approval  Approval             `toml:"approval"`
	Review    Review               `toml:"review"`
	Log       Log                  `toml:"log"`
	Webhook   Webhook              `toml:"webhook"`
	Server    Server               `toml:"server"`
	Data      Data                 `toml:"data"`
	Memory    Memory               `toml:"memory"`
	Executor  Executor             `toml:"executor"`
	GitHub    GitHub               `toml:"github"`
	Providers map[string]*Provider `toml:"providers"`
	Repos     map[string]*Repo     `toml:"repos"`

	// path is the config file that was read, or "" when none was found.
	path string
	// sources maps a dotted key such as `limits.max_turns` to its Source.
	sources map[string]Source
	// home is the user's home directory, used to expand "~/" in Data.Dir.
	home string
	// rejectedKeyEnvs are the api_key_env names of a file that failed to
	// load. They are used only to redact the load error.
	rejectedKeyEnvs []string
}

// Defaults are the choices a run uses unless it says otherwise.
type Defaults struct {
	// Provider is the name of a [providers.<name>] table.
	Provider string `toml:"provider"`
	// Mode is fix, refine or investigate.
	Mode string `toml:"mode"`
	// RefineModel and CodeModel fall back to the provider's default_model.
	RefineModel string `toml:"refine_model"`
	CodeModel   string `toml:"code_model"`
	// MaxCostUSD is the budget of one run across every stage.
	MaxCostUSD float64 `toml:"max_cost_usd"`
	// Approvers are the GitHub logins whose /approve comments count.
	Approvers []string `toml:"approvers"`
	// CommentOnFailure posts a short diagnostic except for cancellation/auth.
	CommentOnFailure bool `toml:"comment_on_failure"`
}

// Limits bound one run.
type Limits struct {
	MaxTurns        int   `toml:"max_turns"`
	RefineMaxTurns  int   `toml:"refine_max_turns"`
	MaxOutputTokens int   `toml:"max_output_tokens"`
	MaxTotalTokens  int64 `toml:"max_total_tokens"`
	// HardMaxCostUSD is the ceiling for any per-run budget.
	HardMaxCostUSD  float64       `toml:"hard_max_cost_usd"`
	MaxTestAttempts int           `toml:"max_test_attempts"`
	RunTimeout      time.Duration `toml:"run_timeout"`
	CmdTimeout      time.Duration `toml:"cmd_timeout"`
	TestTimeout     time.Duration `toml:"test_timeout"`
	// CmdOutputBytes is sent to the model per command; CmdStoreBytes is kept.
	CmdOutputBytes int  `toml:"cmd_output_bytes"`
	CmdStoreBytes  int  `toml:"cmd_store_bytes"`
	BaselineTests  bool `toml:"baseline_tests"`
	// TrustedPath contains operator-controlled absolute tool directories.
	TrustedPath []string `toml:"trusted_path"`
}

// Approval controls the spec approval gate.
type Approval struct {
	// LocalActor is the name recorded for CLI and UI actions.
	LocalActor   string        `toml:"local_actor"`
	PollInterval time.Duration `toml:"poll_interval"`
	// TTL is how long a run may wait for approval before it expires.
	TTL          time.Duration `toml:"ttl"`
	MaxRevisions int           `toml:"max_revisions"`
}

// Review configures the opt-in sequential reviewer. Approval freezes these
// values on the run; a later config change does not affect approved runs.
type Review struct {
	// Enabled turns on the read-only review stage after harness verification.
	Enabled bool `toml:"enabled"`
	// Model is the reviewer model; empty uses the run's refine model.
	Model string `toml:"model"`
	// MaxRepairs is the number of review-requested coder repair cycles (0 or 1).
	MaxRepairs int `toml:"max_repairs"`
	// MaxTurns bounds each review round.
	MaxTurns int `toml:"max_turns"`
	// ReserveFraction of the run's token, cost and time budget is kept back
	// from coding so the review can still run.
	ReserveFraction float64 `toml:"reserve_fraction"`
	// SalvageDraft publishes a clearly marked draft PR when review does not
	// pass. When false such a run fails without publishing.
	SalvageDraft bool `toml:"salvage_draft"`
}

// Log configures logging.
type Log struct {
	Level  string `toml:"level"`  // debug, info, warn or error
	Format string `toml:"format"` // text or json
}

// Server configures the web interface.
type Server struct {
	Addr              string     `toml:"addr"`
	MaxConcurrentRuns int        `toml:"max_concurrent_runs"`
	MaxQueuedRuns     int        `toml:"max_queued_runs"`
	VueSrc            string     `toml:"vue_src"` // cdn or local
	Auth              ServerAuth `toml:"auth"`
}

// Server authentication modes.
const (
	AuthToken            = "token"
	AuthCloudflareAccess = "cloudflare_access"
)

// ServerAuth selects how the web interface authenticates callers. In token
// mode PATCHBOT_UI_TOKEN guards non-loopback binds, as before. In
// cloudflare_access mode every request must carry a valid Cloudflare Access
// JWT for an allowed email, which becomes the recorded actor.
type ServerAuth struct {
	Mode          string   `toml:"mode"`
	TeamDomain    string   `toml:"team_domain"`
	Audience      string   `toml:"audience"`
	AllowedEmails []string `toml:"allowed_emails"`
}

// Memory configures repository memory. Retrieved memory is unverified context
// in the user turn; it never changes policy, scope or the approval gate.
type Memory struct {
	// Enabled turns retrieval, typed observations and lesson proposals on.
	Enabled bool `toml:"enabled"`
	// MaxEntries and MaxBytes bound what one stage receives.
	MaxEntries int `toml:"max_entries"`
	MaxBytes   int `toml:"max_bytes"`
	// MaxPending caps pending model lessons per repository; excess proposals
	// are dropped with an event.
	MaxPending int `toml:"max_pending"`
	// ObservationTTL is how long a typed observation stays retrievable.
	ObservationTTL time.Duration `toml:"observation_ttl"`
}

// Webhook configures authenticated GitHub wake-up hints.
type Webhook struct {
	Enabled       bool              `toml:"enabled"`
	SecretEnv     string            `toml:"secret_env"`
	Path          string            `toml:"path"`
	TriggerLabels map[string]string `toml:"trigger_labels"`
	// LabelPollInterval is how often open issues are swept for trigger labels
	// that a missed delivery did not report.
	LabelPollInterval time.Duration `toml:"label_poll_interval"`
	Secret            secret.Secret `toml:"-"`
}

// GitHub auth modes.
const (
	GitHubAuthPAT = "pat"
	GitHubAuthApp = "app"
)

// GitHub selects how patchbot authenticates to GitHub. PAT mode reads
// GITHUB_TOKEN from the environment. App mode signs a JWT with the App's
// private key file and mints per-run installation tokens narrowed to one
// repository; it never falls back to a PAT. No credential is ever in the file.
type GitHub struct {
	// Auth is "pat" (default) or "app".
	Auth string `toml:"auth"`
	// AppID is the numeric GitHub App ID (app mode).
	AppID int64 `toml:"app_id"`
	// PrivateKeyFile is the absolute path of the App's PEM private key. It
	// holds a path, not key material; the file is checked at startup.
	PrivateKeyFile string `toml:"private_key_file"`
	// InstallationID is optional; 0 resolves each repository's installation.
	InstallationID int64 `toml:"installation_id"`
}

// App reports whether GitHub App authentication is configured.
func (g GitHub) App() bool { return g.Auth == GitHubAuthApp }

// Data says where the database and workspaces live.
type Data struct {
	Dir string `toml:"dir"`
}

// Executor kinds.
const (
	ExecutorLocal     = "local"
	ExecutorContainer = "container"
)

// Executor says where repository commands (baseline tests, run_cmd and harness
// verification) run. Git and file tools always stay on the host.
type Executor struct {
	// Kind is local (host process, the default) or container.
	Kind string `toml:"kind"`
	// Runtime is podman or docker, resolved through limits.trusted_path.
	Runtime string `toml:"runtime"`
	// Image is digest-pinned: name[:tag]@sha256:<digest>.
	Image     string `toml:"image"`
	Memory    string `toml:"memory"`
	CPUs      string `toml:"cpus"`
	PidsLimit int    `toml:"pids_limit"`
	TmpfsSize string `toml:"tmpfs_size"`
	// Network must be "none".
	Network string `toml:"network"`
	// User is the non-root numeric uid:gid inside the container.
	User string `toml:"user"`
	// UserNS is "" or "keep-id" (podman: host user appears as User).
	UserNS string `toml:"userns"`
	// Path is PATH inside the container; it resolves bare command names.
	Path []string `toml:"path"`
	// WorkspaceMaxBytes fails a command that grows the workspace beyond it.
	WorkspaceMaxBytes int64 `toml:"workspace_max_bytes"`
}

// Container reports whether repository commands run in a container.
func (e Executor) Container() bool { return e.Kind == ExecutorContainer }

// Provider is one [providers.<name>] table.
type Provider struct {
	Kind    string `toml:"kind"`
	BaseURL string `toml:"base_url"`
	// APIKeyEnv names the environment variable that holds the API key.
	APIKeyEnv    string   `toml:"api_key_env"`
	Models       []string `toml:"models"`
	DefaultModel string   `toml:"default_model"`
	// PriceFree states that every model of this provider costs nothing, as
	// with a local server. It is different from having no prices.
	PriceFree bool              `toml:"price_free"`
	Prices    map[string]*Price `toml:"prices"`

	// APIKey is read from the environment variable named by APIKeyEnv.
	APIKey secret.Secret `toml:"-"`
}

// Price is one [providers.<name>.prices.<model>] table, in US dollars per
// million tokens.
//
// Every rate is a pointer so that a rate left out of the file can be told
// apart from a rate of 0. Input and Output are required: a price table with
// only an input rate must not turn into "output is free", because the budget
// would then stop counting output tokens. Write 0 for a rate that is free.
type Price struct {
	Input  *float64 `toml:"input"`
	Output *float64 `toml:"output"`
	// CacheRead and CacheWrite are optional and fall back to Input when nil.
	CacheRead  *float64 `toml:"cache_read"`
	CacheWrite *float64 `toml:"cache_write"`
	// Checked is the day the price was read from the provider's price page.
	Checked Date `toml:"checked"`
}

// Repo is one [repos."owner/name"] table. Only listed repositories can run.
type Repo struct {
	// Commands maps a command ID such as "test" to the argv it runs.
	Commands map[string][]string `toml:"commands"`
	// Protected lists extra paths the agent may neither read nor write.
	Protected []string `toml:"protected"`
	// TestCommand is the ID of the command that verifies a change.
	TestCommand string `toml:"test_command"`
}

// TestCommandID returns the command that verifies a change: TestCommand, or
// "test" when that is empty and such a command exists.
func (r *Repo) TestCommandID() (string, bool) {
	if r.TestCommand != "" {
		return r.TestCommand, true
	}
	if _, ok := r.Commands["test"]; ok {
		return "test", true
	}
	return "", false
}

// Date is a calendar day. In TOML it is a local date (2026-11-10) or the same
// text as a string.
type Date struct {
	time.Time
}

const dateLayout = "2006-01-02"

// UnmarshalTOML implements toml.Unmarshaler.
func (d *Date) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case time.Time:
		d.Time = time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)
		return nil
	case string:
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			// The value is not quoted back: nothing says it is not a secret
			// pasted into the wrong field.
			return errors.New("want a date like 2026-01-31")
		}
		d.Time = t
		return nil
	default:
		return errors.New("want a date like 2026-01-31")
	}
}

// String returns the date as YYYY-MM-DD, or "" for the zero date.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Format(dateLayout)
}

// Path returns the config file that was read, or "" when none was found.
func (c *Config) Path() string { return c.path }

// DataDir returns Data.Dir with a leading "~/" expanded.
func (c *Config) DataDir() string {
	dir := c.Data.Dir
	if dir == "~" {
		return c.home
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok && c.home != "" {
		return filepath.Join(c.home, rest)
	}
	return dir
}

// DatabasePath returns the SQLite database file inside the data directory.
func (c *Config) DatabasePath() string {
	return filepath.Join(c.DataDir(), "patchbot.sqlite")
}

// Model returns the model a stage uses with the default provider: the stage's
// own setting, else the provider's default_model. Stage is "refine" or "code".
func (c *Config) Model(stage string) string {
	model := c.Defaults.CodeModel
	if stage == "refine" {
		model = c.Defaults.RefineModel
	}
	if model != "" {
		return model
	}
	if p := c.Providers[c.Defaults.Provider]; p != nil {
		return p.DefaultModel
	}
	return ""
}

// Redactor returns a redactor that masks every secret the config holds, as
// well as token-shaped text. Diagnostics about the config pass through it,
// because a secret can turn up in any field: a key exported under the wrong
// variable name becomes a "log level".
func (c *Config) Redactor() *secret.Redactor {
	r := secret.NewRedactor()
	r.AddSecrets(c.Secrets()...)
	return r
}

// Secrets returns every secret the config holds, for registering with a
// secret.Redactor.
func (c *Config) Secrets() []secret.Secret {
	var out []secret.Secret
	if c.Webhook.Secret.IsSet() {
		out = append(out, c.Webhook.Secret)
	}
	for _, name := range sortedKeys(c.Providers) {
		if key := c.Providers[name].APIKey; key.IsSet() {
			out = append(out, key)
		}
	}
	return out
}

// EnsureDataDir creates private storage and refuses an existing public directory
// or symlink. Doctor reports the same policy without changing the filesystem.
func (c *Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir(), 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(c.DataDir())
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("data directory must be a real directory with mode 0700")
	}
	return nil
}
