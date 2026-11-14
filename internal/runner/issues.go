package runner

import (
	"context"
	"errors"

	"github.com/aryanpappu/patchbot/internal/github"
)

// ErrIssueCreationUnsupported means the configured GitHub client cannot open issues.
var ErrIssueCreationUnsupported = errors.New("the GitHub client cannot create issues")

// IssueWriter returns the runner's GitHub issue-creation seam. It uses the
// same client, and so the same credential path (App installation token for the
// repository, or the PAT), as every other runner GitHub call. It fails with
// ErrGitHubNotConnected in app-mode setup.
func (r *Runner) IssueWriter() (github.IssueWriter, error) {
	if r.notConnected {
		return nil, ErrGitHubNotConnected
	}
	writer, ok := r.gh.(github.IssueWriter)
	if !ok {
		return nil, ErrIssueCreationUnsupported
	}
	return writer, nil
}

// BotLogin is the GitHub login patchbot's own writes carry: "<slug>[bot]" in
// App mode, the token owner's login in PAT mode. It fails closed when unknown.
func (r *Runner) BotLogin(ctx context.Context) (string, error) {
	if r.opts.BotLogin != "" {
		return r.opts.BotLogin, nil
	}
	identity, ok := r.gh.(interface {
		CurrentUser(context.Context) (string, error)
	})
	if !ok {
		return "", errors.New("GitHub bot identity unavailable; comment sync disabled")
	}
	return identity.CurrentUser(ctx)
}
