package config

import (
	"fmt"
	"slices"

	"github.com/aryanpappu/patchbot/internal/execx"
)

// CommandPolicyOptions returns the policy options for repository commands.
// Local execution resolves executables on the host trusted path; container
// execution keeps names for the image's PATH. Command IDs and the allowlist
// are the same either way.
func (c *Config) CommandPolicyOptions(excluded ...string) execx.PolicyOptions {
	if c.Executor.Container() {
		return execx.PolicyOptions{InContainer: true, TrustedPath: slices.Clone(c.Executor.Path)}
	}
	return execx.PolicyOptions{TrustedPath: c.Limits.TrustedPath, ExcludedRoots: append([]string{c.DataDir()}, excluded...)}
}

// ContainerConfig resolves the runtime binary through limits.trusted_path
// (never the ambient PATH) and returns the execx container configuration.
// lookup reads patchbot's own environment for the runtime CLI variables, such
// as XDG_RUNTIME_DIR; nothing from it is forwarded into the container.
func (c *Config) ContainerConfig(lookup func(string) (string, bool), excluded ...string) (execx.ContainerConfig, error) {
	e := c.Executor
	if !e.Container() {
		return execx.ContainerConfig{}, fmt.Errorf("executor.kind is %q, not container", e.Kind)
	}
	runtime, err := execx.ResolveExecutable(e.Runtime, c.Limits.TrustedPath, append([]string{c.DataDir()}, excluded...)...)
	if err != nil {
		return execx.ContainerConfig{}, fmt.Errorf("executor.runtime %s: %w", e.Runtime, err)
	}
	cfg := execx.ContainerConfig{
		Runtime: runtime, Flavor: e.Runtime, Image: e.Image, Memory: e.Memory, CPUs: e.CPUs,
		PidsLimit: e.PidsLimit, TmpfsSize: e.TmpfsSize, Network: e.Network, User: e.User,
		UserNS: e.UserNS, Path: slices.Clone(e.Path), HostEnv: execx.RuntimeHostEnv(c.Limits.TrustedPath, lookup),
	}
	return cfg, cfg.Validate()
}
