package config

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/aryanpappu/patchbot/internal/execx"
)

// CommandPolicyOptions returns the policy options for repository commands.
// Local execution resolves executables on the host trusted path; container
// execution keeps names for the image's PATH. Command IDs and the allowlist
// are the same either way.
func (c *Config) CommandPolicyOptions(repo string, excluded ...string) execx.PolicyOptions {
	if c.Executor.Container() {
		return execx.PolicyOptions{InContainer: true, TrustedPath: c.RepoExecutor(repo).Path}
	}
	return execx.PolicyOptions{TrustedPath: c.Limits.TrustedPath, ExcludedRoots: append([]string{c.DataDir()}, excluded...)}
}

// RepoExecutor is the container image and in-container PATH one repository
// runs with: its own [repos."owner/name"] values, else [executor]'s. It is
// recorded on the run so the image a run used is auditable.
type RepoExecutor struct {
	Image string   `json:"image"`
	Path  []string `json:"path"`
	// ImageSource is the config key the image came from.
	ImageSource string `json:"image_source"`
}

// RepoExecutor resolves the per-repository overrides. An unknown repo or ""
// yields the [executor] values.
func (c *Config) RepoExecutor(repo string) RepoExecutor {
	out := RepoExecutor{Image: c.Executor.Image, Path: slices.Clone(c.Executor.Path), ImageSource: "executor.image"}
	if r := c.Repos[repo]; r != nil {
		if r.Image != "" {
			out.Image, out.ImageSource = r.Image, fmt.Sprintf("repos.%q.image", repo)
		}
		if r.Path != nil {
			out.Path = slices.Clone(r.Path)
		}
	}
	return out
}

// imageFreeImage is a valid, never-pulled image for operations that do not use
// the image (container sweep, runtime checks) when none is configured globally.
const imageFreeImage = "patchbot-unused@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// ContainerConfig resolves the runtime binary through limits.trusted_path
// (never the ambient PATH) and returns the execx container configuration.
// lookup reads patchbot's own environment for the runtime CLI variables, such
// as XDG_RUNTIME_DIR; nothing from it is forwarded into the container.
// repo selects that repository's image and PATH; "" is for operations that do
// not run a repository command (sweep, runtime checks) and uses a placeholder
// image when [executor] has none.
func (c *Config) ContainerConfig(repo string, lookup func(string) (string, bool), excluded ...string) (execx.ContainerConfig, error) {
	return c.ContainerConfigFor(c.RepoExecutor(repo), lookup, excluded...)
}

// ContainerConfigFor builds the configuration for an already-resolved
// (for example run-frozen) image and PATH.
func (c *Config) ContainerConfigFor(re RepoExecutor, lookup func(string) (string, bool), excluded ...string) (execx.ContainerConfig, error) {
	e := c.Executor
	if !e.Container() {
		return execx.ContainerConfig{}, fmt.Errorf("executor.kind is %q, not container", e.Kind)
	}
	if re.Image == "" {
		re.Image = imageFreeImage
	}
	runtime, err := execx.ResolveExecutable(e.Runtime, c.Limits.TrustedPath, append([]string{c.DataDir()}, excluded...)...)
	if err != nil {
		return execx.ContainerConfig{}, fmt.Errorf("executor.runtime %s: %w", e.Runtime, err)
	}
	cfg := execx.ContainerConfig{
		Runtime: runtime, Flavor: e.Runtime, Image: re.Image, Memory: e.Memory, CPUs: e.CPUs,
		PidsLimit: e.PidsLimit, TmpfsSize: e.TmpfsSize, Network: e.Network, User: e.User,
		UserNS: e.UserNS, Path: slices.Clone(re.Path), WorkspaceMaxBytes: e.WorkspaceMaxBytes,
		HostEnv: execx.RuntimeHostEnv(c.Limits.TrustedPath, lookup),
	}
	if e.Runtime == execx.RuntimeDocker {
		// A private, empty config: the user's ~/.docker/config.json can inject
		// proxy variables into containers.
		cfg.HostEnv = append(cfg.HostEnv, "DOCKER_CONFIG="+c.DockerConfigDir())
	}
	return cfg, cfg.Validate()
}

// DockerConfigDir is docker's private DOCKER_CONFIG directory in the data dir.
func (c *Config) DockerConfigDir() string {
	return filepath.Join(c.DataDir(), "runtime", "docker-config")
}
