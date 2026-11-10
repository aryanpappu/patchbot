package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (c *HTTPClient) call(ctx context.Context, repo RepoRef, parts []string, method string, in, out any, statuses ...int) (responseMeta, error) {
	u, err := c.endpoint(repo, parts...)
	if err != nil {
		return responseMeta{}, err
	}
	return c.request(ctx, method, u, in, out, statuses...)
}

// GetRepo fetches preflight metadata, including push permission and visibility.
func (c *HTTPClient) GetRepo(ctx context.Context, ref RepoRef) (Repo, error) {
	var repo Repo
	_, err := c.call(ctx, ref, nil, http.MethodGet, nil, &repo, http.StatusOK)
	return repo, operationError("GetRepo", err)
}

// GetIssue fetches the issue's current text and metadata.
func (c *HTTPClient) GetIssue(ctx context.Context, repo RepoRef, number int) (Issue, error) {
	var issue Issue
	_, err := c.call(ctx, repo, []string{"issues", strconv.Itoa(number)}, http.MethodGet, nil, &issue, http.StatusOK)
	return issue, operationError("GetIssue", err)
}

// GetIssueComment fetches a spec rendering for the runner's tampering check.
func (c *HTTPClient) GetIssueComment(ctx context.Context, repo RepoRef, id int64) (Comment, error) {
	var comment Comment
	_, err := c.call(ctx, repo, []string{"issues", "comments", strconv.FormatInt(id, 10)}, http.MethodGet, nil, &comment, http.StatusOK)
	return comment, operationError("GetIssueComment", err)
}

// ListIssueComments follows Link pagination, returning at most 300 comments.
// A zero since includes all comments; otherwise GitHub filters by updated time.
func (c *HTTPClient) ListIssueComments(ctx context.Context, repo RepoRef, number int, since time.Time) ([]Comment, error) {
	comments, _, _, err := c.listComments(ctx, repo, number, since, "")
	return comments, operationError("ListIssueComments", err)
}

// ListIssueCommentsSince supports conditional polling. The ETag is returned only
// for a single-page response: caching one page cannot validate later pages.
func (c *HTTPClient) ListIssueCommentsSince(ctx context.Context, repo RepoRef, number int, since time.Time, etag string) ([]Comment, string, bool, error) {
	comments, newETag, unchanged, err := c.listComments(ctx, repo, number, since, etag)
	return comments, newETag, unchanged, operationError("ListIssueCommentsSince", err)
}

type etagKey struct{}

func (c *HTTPClient) listComments(ctx context.Context, repo RepoRef, number int, since time.Time, etag string) ([]Comment, string, bool, error) {
	u, err := c.endpoint(repo, "issues", strconv.Itoa(number), "comments")
	if err != nil {
		return nil, "", false, err
	}
	query := url.Values{"per_page": {"100"}}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339Nano))
	}
	u.RawQuery = query.Encode()
	comments := make([]Comment, 0)
	seen := make(map[string]bool)
	var newETag string
	for page := 0; u != nil; page++ {
		if page >= maxComments || seen[u.String()] {
			return nil, "", false, errors.New("pagination cycle or page limit exceeded")
		}
		seen[u.String()] = true
		requestCtx := ctx
		if page == 0 && etag != "" {
			requestCtx = context.WithValue(ctx, etagKey{}, etag)
		}
		var batch []Comment
		meta, err := c.request(requestCtx, http.MethodGet, u, nil, &batch, http.StatusOK, http.StatusNotModified)
		if err != nil {
			return nil, "", false, err
		}
		if meta.status == http.StatusNotModified {
			if page != 0 || etag == "" {
				return nil, "", false, errors.New("unexpected not-modified response")
			}
			if newETag = meta.header.Get("ETag"); newETag == "" {
				newETag = etag
			}
			return nil, newETag, true, nil
		}
		if page == 0 {
			newETag = meta.header.Get("ETag")
		} else {
			newETag = ""
		}
		remaining := maxComments - len(comments)
		comments = append(comments, batch[:min(len(batch), remaining)]...)
		if len(comments) == maxComments {
			return comments, "", false, nil
		}
		u, err = nextPage(u, meta.header)
		if err != nil {
			return nil, "", false, err
		}
	}
	return comments, newETag, false, nil
}

// CreateIssueComment posts text with mentions neutralized.
func (c *HTTPClient) CreateIssueComment(ctx context.Context, repo RepoRef, number int, body string) (Comment, error) {
	var comment Comment
	in := struct {
		Body string `json:"body"`
	}{NeutralizeMentions(body)}
	_, err := c.call(ctx, repo, []string{"issues", strconv.Itoa(number), "comments"}, http.MethodPost, in, &comment, http.StatusCreated)
	return comment, operationError("CreateIssueComment", err)
}

// UpdateIssueComment replaces a comment with mention-neutralized text.
func (c *HTTPClient) UpdateIssueComment(ctx context.Context, repo RepoRef, id int64, body string) error {
	in := struct {
		Body string `json:"body"`
	}{NeutralizeMentions(body)}
	_, err := c.call(ctx, repo, []string{"issues", "comments", strconv.FormatInt(id, 10)}, http.MethodPatch, in, nil, http.StatusOK)
	return operationError("UpdateIssueComment", err)
}

// IsCollaborator checks live repository membership rather than trusting a
// comment's author_association. Missing membership (404) is not an error.
func (c *HTTPClient) IsCollaborator(ctx context.Context, repo RepoRef, user string) (bool, error) {
	_, err := c.call(ctx, repo, []string{"collaborators", user}, http.MethodGet, nil, nil, http.StatusNoContent)
	var missing *ErrNotFound
	if errors.As(err, &missing) {
		return false, nil
	}
	return err == nil, operationError("IsCollaborator", err)
}

// FindOpenPR finds an open pull request whose head belongs to this repository.
// It returns nil when no matching pull request exists.
func (c *HTTPClient) FindOpenPR(ctx context.Context, repo RepoRef, headBranch string) (*PullRequest, error) {
	u, err := c.endpoint(repo, "pulls")
	if err != nil {
		return nil, operationError("FindOpenPR", err)
	}
	u.RawQuery = url.Values{"state": {"open"}, "head": {repo.Owner + ":" + headBranch}, "per_page": {"100"}}.Encode()
	var prs []PullRequest
	_, err = c.request(ctx, http.MethodGet, u, nil, &prs, http.StatusOK)
	if err != nil {
		return nil, operationError("FindOpenPR", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// CreatePR publishes a pull request, preserving the harness's draft decision.
// Both the title and body have mentions neutralized before sending.
func (c *HTTPClient) CreatePR(ctx context.Context, repo RepoRef, in NewPR) (PullRequest, error) {
	in.Title, in.Body = NeutralizeMentions(in.Title), NeutralizeMentions(in.Body)
	var pr PullRequest
	_, err := c.call(ctx, repo, []string{"pulls"}, http.MethodPost, in, &pr, http.StatusCreated)
	return pr, operationError("CreatePR", err)
}

// GetBranch fetches a branch and reports whether it exists.
func (c *HTTPClient) GetBranch(ctx context.Context, repo RepoRef, branch string) (Branch, bool, error) {
	var result Branch
	_, err := c.call(ctx, repo, []string{"branches", branch}, http.MethodGet, nil, &result, http.StatusOK)
	var missing *ErrNotFound
	if errors.As(err, &missing) {
		return Branch{}, false, nil
	}
	return result, err == nil, operationError("GetBranch", err)
}

// BranchExists is a convenience for callers that do not need the branch tip.
func (c *HTTPClient) BranchExists(ctx context.Context, repo RepoRef, branch string) (bool, error) {
	_, exists, err := c.GetBranch(ctx, repo, branch)
	return exists, operationError("BranchExists", err)
}

// AddLabels adds labels without removing existing labels.
func (c *HTTPClient) AddLabels(ctx context.Context, repo RepoRef, number int, labels []string) error {
	if labels == nil {
		labels = []string{}
	}
	in := struct {
		Labels []string `json:"labels"`
	}{labels}
	_, err := c.call(ctx, repo, []string{"issues", strconv.Itoa(number), "labels"}, http.MethodPost, in, nil, http.StatusOK)
	return operationError("AddLabels", err)
}

// RemoveLabel removes one label, encoding slashes and spaces as part of its name.
func (c *HTTPClient) RemoveLabel(ctx context.Context, repo RepoRef, number int, label string) error {
	_, err := c.call(ctx, repo, []string{"issues", strconv.Itoa(number), "labels", label}, http.MethodDelete, nil, nil, http.StatusOK)
	return operationError("RemoveLabel", err)
}
