package execx

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultMaxExtraArgs  = 64
	defaultMaxArgLength  = 8 << 10
	defaultMaxExtraBytes = 8 << 10
)

// PolicyOptions are operator-owned exceptions and limits, never model input.
// AllowArgs and AllowLeadingDash are indexed by command ID. AllowShells is
// indexed by executable basename, e.g. "bash", and must name an explicit opt-in.
// Zero limits select defaults. Options are copied by NewPolicy.
type PolicyOptions struct {
	AllowArgs        map[string]bool
	AllowLeadingDash map[string]bool
	AllowShells      map[string]bool
	MaxExtraArgs     int
	MaxArgLength     int
	MaxExtraBytes    int
}

type command struct {
	argv      []string
	allowArgs bool
	allowDash bool
}

// Policy holds immutable, resolved command IDs. Construct it from trusted
// per-repo commands = { name = [argv...] } configuration outside the checkout.
type Policy struct {
	commands map[string]command
	maxArgs  int
	maxLen   int
	maxBytes int
}

// NewPolicy resolves executables once using only TrustedPath. An absolute
// operator-supplied executable is also accepted, but must be outside root.
// At most one options value may be supplied.
func NewPolicy(root string, commands map[string][]string, options ...PolicyOptions) (*Policy, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	if len(options) > 1 {
		return nil, errors.New("at most one policy options value is allowed")
	}
	var opts PolicyOptions
	if len(options) == 1 {
		opts = options[0]
	}
	if opts.MaxExtraArgs < 0 || opts.MaxArgLength < 0 || opts.MaxExtraBytes < 0 {
		return nil, errors.New("argument limits must not be negative")
	}
	p := &Policy{commands: make(map[string]command), maxArgs: opts.MaxExtraArgs,
		maxLen: opts.MaxArgLength, maxBytes: opts.MaxExtraBytes}
	if p.maxArgs == 0 {
		p.maxArgs = defaultMaxExtraArgs
	}
	if p.maxLen == 0 {
		p.maxLen = defaultMaxArgLength
	}
	if p.maxBytes == 0 {
		p.maxBytes = defaultMaxExtraBytes
	}
	for name, argv := range commands {
		if name == "" || strings.ContainsRune(name, 0) || len(argv) == 0 || argv[0] == "" {
			return nil, errors.New("command ID and executable must not be empty or contain NUL")
		}
		for _, arg := range argv {
			if strings.ContainsRune(arg, 0) {
				return nil, fmt.Errorf("command %q contains NUL", name)
			}
		}
		binary, resolveErr := resolveExecutable(root, argv[0], opts.AllowShells)
		if resolveErr != nil {
			return nil, fmt.Errorf("command %q: %w", name, resolveErr)
		}
		copyArgv := append([]string(nil), argv...)
		copyArgv[0] = binary
		p.commands[name] = command{copyArgv, opts.AllowArgs[name], opts.AllowLeadingDash[name]}
	}
	return p, nil
}

// Resolve returns a fresh argv slice. Additional arguments require explicit
// per-command opt-in; flags require another opt-in. Literal metacharacters have
// no shell meaning. The approved program may still interpret their contents.
func (p *Policy) Resolve(name string, extraArgs []string) ([]string, error) {
	cmd, ok := p.commands[name]
	if !ok {
		return nil, fmt.Errorf("unknown command %q", name)
	}
	if len(extraArgs) > 0 && !cmd.allowArgs {
		return nil, errors.New("extra arguments are not allowed")
	}
	if len(extraArgs) > p.maxArgs {
		return nil, errors.New("too many extra arguments")
	}
	total := 0
	for _, arg := range extraArgs {
		if strings.ContainsRune(arg, 0) || (!cmd.allowDash && strings.HasPrefix(arg, "-")) {
			return nil, errors.New("invalid extra argument")
		}
		if len(arg) > p.maxLen || len(arg) > p.maxBytes-total {
			return nil, errors.New("extra arguments exceed length limit")
		}
		total += len(arg)
	}
	argv := make([]string, 0, len(cmd.argv)+len(extraArgs))
	argv = append(argv, cmd.argv...)
	return append(argv, extraArgs...), nil
}

func shellLike(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "fish", "dash", "env", "xargs", "sudo":
		return true
	default:
		return false
	}
}

func resolveExecutable(root, name string, allowShells map[string]bool) (string, error) {
	base := filepath.Base(name)
	explicitShell := shellLike(base) && allowShells[base]
	if shellLike(base) && !explicitShell {
		return "", errors.New("shell or command launcher is not allowed")
	}
	if !filepath.IsAbs(name) && base != name {
		return "", errors.New("relative executable paths are not allowed")
	}
	var path string
	var err error
	if filepath.IsAbs(name) {
		path, err = exec.LookPath(name)
	} else {
		// LookPath receives absolute candidates so it never reads ambient PATH.
		for _, dir := range filepath.SplitList(TrustedPath) {
			candidate := filepath.Join(dir, name)
			path, err = exec.LookPath(candidate)
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	if inside(root, path) {
		return "", errors.New("executable is inside the workspace")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable symlink: %w", err)
	}
	if !filepath.IsAbs(resolved) || inside(root, resolved) {
		return "", errors.New("executable must be absolute and outside the workspace")
	}
	if target := filepath.Base(resolved); shellLike(target) && !explicitShell && !allowShells[target] {
		return "", errors.New("executable resolves to a disallowed shell or launcher")
	}
	return resolved, nil
}
