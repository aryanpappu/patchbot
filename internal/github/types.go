// Package github provides the GitHub REST operations needed by the runner.
package github

import (
	"context"
	"encoding/json"
	"time"
)

// TokenSource supplies a PAT today and can supply GitHub App tokens later.
// Implementations must be safe for concurrent calls.
type TokenSource interface {
	Token(context.Context) (string, error)
}

// Client is the runner's GitHub boundary. Missing branches and open PRs return
// false and nil respectively; other missing resources return *ErrNotFound.
type Client interface {
	GetRepo(context.Context, RepoRef) (Repo, error)
	GetIssue(context.Context, RepoRef, int) (Issue, error)
	ListIssueComments(context.Context, RepoRef, int, time.Time) ([]Comment, error)
	ListIssueCommentsSince(context.Context, RepoRef, int, time.Time, string) ([]Comment, string, bool, error)
	GetIssueComment(context.Context, RepoRef, int64) (Comment, error)
	CreateIssueComment(context.Context, RepoRef, int, string) (Comment, error)
	UpdateIssueComment(context.Context, RepoRef, int64, string) error
	IsCollaborator(context.Context, RepoRef, string) (bool, error)
	FindOpenPR(context.Context, RepoRef, string) (*PullRequest, error)
	CreatePR(context.Context, RepoRef, NewPR) (PullRequest, error)
	GetBranch(context.Context, RepoRef, string) (Branch, bool, error)
	BranchExists(context.Context, RepoRef, string) (bool, error)
	AddLabels(context.Context, RepoRef, int, []string) error
	RemoveLabel(context.Context, RepoRef, int, string) error
}

// RepoRef identifies a repository without a URL or credentials.
type RepoRef struct {
	Owner string
	Name  string
}

// Permissions contains the repository capabilities needed for preflight.
type Permissions struct {
	Push bool `json:"push"`
}

// Repo contains repository metadata used for preflight and cloning.
type Repo struct {
	Name          string      `json:"name"`
	FullName      string      `json:"full_name"`
	DefaultBranch string      `json:"default_branch"`
	CloneURL      string      `json:"clone_url"`
	Private       bool        `json:"private"`
	Permissions   Permissions `json:"permissions"`
}

// Label is a GitHub issue label.
type Label struct {
	Name string `json:"name"`
}

// Issue includes the current issue text used to invalidate stale approvals.
type Issue struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	Labels            []Label   `json:"labels"`
	IsPullRequest     bool      `json:"-"`
	Author            string    `json:"-"`
	AuthorAssociation string    `json:"author_association"`
	HTMLURL           string    `json:"html_url"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// UnmarshalJSON normalizes GitHub's nested user and pull_request fields.
func (i *Issue) UnmarshalJSON(data []byte) error {
	type plain Issue
	var wire struct {
		plain
		User        struct{ Login string } `json:"user"`
		PullRequest *json.RawMessage       `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*i = Issue(wire.plain)
	i.Author = wire.User.Login
	i.IsPullRequest = wire.PullRequest != nil
	return nil
}

// Comment retains timestamps so edited commands can be rejected.
type Comment struct {
	ID                int64     `json:"id"`
	Author            string    `json:"-"`
	AuthorAssociation string    `json:"author_association"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	HTMLURL           string    `json:"html_url"`
}

// UnmarshalJSON normalizes GitHub's nested user field.
func (c *Comment) UnmarshalJSON(data []byte) error {
	type plain Comment
	var wire struct {
		plain
		User struct{ Login string } `json:"user"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = Comment(wire.plain)
	c.Author = wire.User.Login
	return nil
}

// PRBranch identifies the head or base of a pull request.
type PRBranch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// PullRequest is the result of finding or creating a pull request.
type PullRequest struct {
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	State   string   `json:"state"`
	HTMLURL string   `json:"html_url"`
	Draft   bool     `json:"draft"`
	Head    PRBranch `json:"head"`
	Base    PRBranch `json:"base"`
}

// NewPR supplies the fields required to publish a pull request.
type NewPR struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Draft bool   `json:"draft"`
}

// Branch contains the branch name and tip commit.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}
