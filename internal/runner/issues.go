package runner

import (
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
