package runner

import (
	"context"
	"time"

	"github.com/aryanpappu/patchbot/internal/repos"
)

// Repos returns the effective repository allowlist the runner consults.
func (r *Runner) Repos() *repos.Registry { return r.repos }

// RepoCheck is the result of the optional reachability check for a repository.
// It informs the operator and never blocks adding the repository.
type RepoCheck struct {
	// Checked is false when no check could be attempted (GitHub not connected).
	Checked   bool   `json:"checked"`
	Reachable bool   `json:"reachable"`
	Push      bool   `json:"push"`
	Message   string `json:"message"`
}

// CheckRepo asks GitHub, with the credential a run would use (the App
// installation for that repository, or the PAT), whether repo is reachable.
func (r *Runner) CheckRepo(ctx context.Context, repo string) RepoCheck {
	if r.notConnected {
		return RepoCheck{Message: "GitHub is not connected yet, so reachability was not checked"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	meta, err := r.gh.GetRepo(ctx, ref(repo))
	if err != nil {
		return RepoCheck{Checked: true, Message: r.redactor.Redact("GitHub could not read this repository with the configured credential: " + err.Error())}
	}
	return RepoCheck{Checked: true, Reachable: true, Push: meta.Permissions.Push, Message: "reachable"}
}
