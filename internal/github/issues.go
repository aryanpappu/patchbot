package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// NewIssue is the content of an issue to create. Title and body have mentions
// neutralized before sending, like every other text patchbot writes.
type NewIssue struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// IssueWriter is the optional issue-creation seam of the REST client (M26). It
// is separate from Client so clients that predate it stay usable.
type IssueWriter interface {
	CreateIssue(context.Context, RepoRef, NewIssue) (Issue, error)
	// ListIssuesSince returns one page (1-based) of issues of any state updated
	// at or after since, newest first. Pull requests are included; callers skip
	// them with Issue.IsPullRequest.
	ListIssuesSince(ctx context.Context, repo RepoRef, since time.Time, page int) ([]Issue, error)
}

var _ IssueWriter = (*HTTPClient)(nil)

// CreateIssue opens an issue as the credential's identity (the App bot, or the
// PAT's user). Mentions in the title and body are neutralized.
func (c *HTTPClient) CreateIssue(ctx context.Context, repo RepoRef, in NewIssue) (Issue, error) {
	in.Title, in.Body = NeutralizeMentions(in.Title), NeutralizeMentions(in.Body)
	var issue Issue
	_, err := c.call(ctx, repo, []string{"issues"}, http.MethodPost, in, &issue, http.StatusCreated)
	return issue, operationError("CreateIssue", err)
}

// ListIssuesSince lists issues of any state updated since the given time.
func (c *HTTPClient) ListIssuesSince(ctx context.Context, repo RepoRef, since time.Time, page int) ([]Issue, error) {
	u, err := c.endpoint(repo, "issues")
	if err != nil {
		return nil, operationError("ListIssuesSince", err)
	}
	q := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"100"}, "page": {strconv.Itoa(max(page, 1))}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	u.RawQuery = q.Encode()
	var issues []Issue
	_, err = c.request(ctx, &repo, http.MethodGet, u, nil, &issues, http.StatusOK)
	return issues, operationError("ListIssuesSince", err)
}
