package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

const (
	maxIssueTitleRunes = 256
	maxIssueBodyBytes  = 64 << 10
	// issuePendingGrace is how long a pending row is presumed to belong to a
	// request still talking to GitHub; only an older row is reconciled by a retry.
	issuePendingGrace = 2 * time.Minute
	issueCallTimeout  = 90 * time.Second
	// issueSearchPages bounds the marker search (100 issues per page).
	issueSearchPages = 3
)

var issueKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// issueRequest is the POST /api/issues body. The run options mirror
// POST /api/runs and are used only when start_run is true.
type issueRequest struct {
	Repo           string `json:"repo"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	StartRun       bool   `json:"start_run"`
	IdempotencyKey string `json:"idempotency_key"`
	Mode           string `json:"mode"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	RefineModel    string `json:"refine_model"`
	CodeModel      string `json:"code_model"`
	DryRun         bool   `json:"dry_run"`
	MaxCostMicros  int64  `json:"max_cost_micros"`
}

func (in issueRequest) start(issue int64) startRequest {
	return startRequest{Repo: in.Repo, Issue: issue, Mode: in.Mode, Provider: in.Provider, Model: in.Model, RefineModel: in.RefineModel, CodeModel: in.CodeModel, DryRun: in.DryRun, MaxCostMicros: in.MaxCostMicros}
}

// hash identifies the request content, so one key cannot be reused for a
// different issue.
func (in issueRequest) hash() string {
	in.IdempotencyKey = ""
	raw, _ := json.Marshal(in)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type apiError struct {
	status        int
	kind, message string
}

// validIssueText rejects control characters other than newline and tab (and
// those too when multiline is false) and anything that is not valid UTF-8.
func validIssueText(value string, multiline bool) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == '\n' || r == '\t' {
			if !multiline {
				return false
			}
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// normalize trims and validates in place, returning a client-safe message.
func (s *Server) normalizeIssue(in *issueRequest) string {
	in.Repo = strings.ToLower(strings.TrimSpace(in.Repo))
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(strings.ReplaceAll(in.Body, "\r\n", "\n"))
	switch {
	case !s.repoAllowed(in.Repo):
		return "repository is not in the allowlist"
	case !issueKeyPattern.MatchString(in.IdempotencyKey):
		return "idempotency_key must be 8-128 letters, digits, '-' or '_'"
	case utf8.RuneCountInString(in.Title) < 1 || utf8.RuneCountInString(in.Title) > maxIssueTitleRunes:
		return "title must be 1-256 characters"
	case len(in.Body) > maxIssueBodyBytes:
		return "body must be at most 64 KiB"
	case !validIssueText(in.Title, false) || !validIssueText(in.Body, true):
		return "title and body may not contain control characters"
	case github.HasPatchbotMarker(in.Title) || github.HasPatchbotMarker(in.Body):
		return "title and body may not contain patchbot markers"
	}
	if in.StartRun {
		if message := s.validStartOptions(in.start(1)); message != "" {
			return message
		}
	}
	return ""
}

func issueMarker(key string) string { return "<!-- patchbot:issue-created v1 key=" + key + " -->" }

// issueBody is the user's text, a visible footer naming the dashboard actor
// (GitHub shows only the bot or PAT user) and the hidden marker that lets a
// retry find the issue after a crash.
func issueBody(body, actor, key string) string {
	clean := strings.Map(func(r rune) rune {
		if r == '`' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, actor)
	if len(clean) > 200 {
		clean = clean[:200]
	}
	footer := "---\n_Created from the patchbot dashboard by `" + clean + "`. Nothing runs until the specification is approved._\n" + issueMarker(key)
	if body == "" {
		return footer
	}
	return body + "\n\n" + footer
}

// notCreated reports whether a failed GitHub call certainly created nothing:
// no request was sent, or GitHub answered with a 4xx.
func notCreated(err error) bool {
	if errors.Is(err, github.ErrCredentialUnavailable) {
		return true
	}
	var mint *github.MintError
	if errors.As(err, &mint) {
		return true
	}
	var api *github.APIError
	return errors.As(err, &api) && api.StatusCode >= 400 && api.StatusCode < 500
}

// githubIssueError maps a failed create to a fixed message: neither a token nor
// GitHub's response text is ever relayed. A GitHub 401 is not returned as 401,
// which the dashboard would read as "sign in again".
func githubIssueError(err error) apiError {
	var limited *github.ErrRateLimited
	var api *github.APIError
	switch {
	case errors.As(err, &limited):
		return apiError{http.StatusTooManyRequests, "github_rate_limited", "GitHub rate limit reached; try again later"}
	case errors.As(err, &api) && api.StatusCode == http.StatusGone:
		return apiError{http.StatusUnprocessableEntity, "issues_disabled", "issues are disabled for this repository"}
	case errors.As(err, &api) && (api.StatusCode == http.StatusUnauthorized || api.StatusCode == http.StatusForbidden || api.StatusCode == http.StatusNotFound):
		return apiError{http.StatusForbidden, "github_permission_denied", "GitHub refused to create the issue: the token or App needs Issues write permission on this repository"}
	case errors.As(err, &api) && api.StatusCode == http.StatusUnprocessableEntity:
		return apiError{http.StatusUnprocessableEntity, "github_rejected", "GitHub rejected the issue content"}
	default:
		return apiError{http.StatusBadGateway, "github_unavailable", "could not reach GitHub to create the issue"}
	}
}

var errIssueUnknown = apiError{http.StatusBadGateway, "issue_state_unknown", "the issue may have been created; check the repository on GitHub, then retry with the same idempotency key"}

func issueURL(repo string, issue github.Issue) string {
	if u, err := url.Parse(issue.HTMLURL); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
		return u.String()
	}
	return "https://github.com/" + repo + "/issues/" + strconv.Itoa(issue.Number)
}

func (s *Server) pendingGrace() time.Duration {
	if s.issueGrace > 0 {
		return s.issueGrace
	}
	return issuePendingGrace
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodPost) {
		return
	}
	var in issueRequest
	if !s.decode(w, r, &in, false) {
		return
	}
	if message := s.normalizeIssue(&in); message != "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request", message)
		return
	}
	writer, err := s.runner.IssueWriter()
	if err != nil {
		if errors.Is(err, runner.ErrGitHubNotConnected) {
			s.respondError(w, err)
		} else {
			s.writeError(w, http.StatusNotImplemented, "not_supported", "this GitHub client cannot create issues")
		}
		return
	}
	actor := s.actor(r)
	// The GitHub call and its bookkeeping outlive a client that disconnects:
	// abandoning them midway is exactly the crash window the pending row covers.
	work, cancel := context.WithTimeout(context.WithoutCancel(runner.WithActor(r.Context(), actor)), issueCallTimeout)
	defer cancel()
	row, inserted, err := s.db.BeginDashboardIssue(work, store.DashboardIssue{IdempotencyKey: in.IdempotencyKey, RequestHash: in.hash(), Repo: in.Repo, Actor: actor, CreatedAt: time.Now()})
	if err != nil {
		s.respondError(w, err)
		return
	}
	if row.RequestHash != in.hash() || row.Repo != in.Repo {
		s.writeError(w, http.StatusUnprocessableEntity, "idempotency_conflict", "this idempotency key was already used for a different request")
		return
	}
	created := inserted
	if row.Pending {
		var failure *apiError
		row, created, failure = s.ensureIssue(work, writer, row, inserted, in)
		if failure != nil {
			s.writeError(w, failure.status, failure.kind, failure.message)
			return
		}
	}
	response := map[string]any{"issue": issueView(row, created)}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	if in.StartRun {
		run, err := s.issueRun(r.Context(), work, row, in)
		if run.ID > 0 {
			response["run"] = publicRun(run)
			w.Header().Set("Location", fmt.Sprintf("/api/runs/%d", run.ID))
		}
		if err != nil && run.ID == 0 {
			// The issue exists; a retry with the same key resumes at the run.
			code, kind, message := errorEnvelope(err)
			response["error"] = map[string]string{"kind": kind, "message": message}
			s.writeJSON(w, code, response)
			return
		}
	}
	s.writeJSON(w, status, response)
}

func issueView(row store.DashboardIssue, created bool) map[string]any {
	return map[string]any{"repo": row.Repo, "number": row.Number, "node_id": row.NodeID, "html_url": row.URL, "requested_by": row.Actor, "created": created}
}

// ensureIssue turns a pending row into a created issue. A fresh row creates the
// issue. A row left by an earlier request is first reconciled: the hidden marker
// is searched for, and only a complete search that finds nothing may create.
func (s *Server) ensureIssue(ctx context.Context, writer github.IssueWriter, row store.DashboardIssue, fresh bool, in issueRequest) (store.DashboardIssue, bool, *apiError) {
	repo := github.RepoRef{Owner: strings.SplitN(row.Repo, "/", 2)[0], Name: strings.SplitN(row.Repo, "/", 2)[1]}
	fail := func(e apiError) (store.DashboardIssue, bool, *apiError) { return row, false, &e }
	if !fresh {
		claimed, err := s.db.ClaimPendingDashboardIssue(ctx, row.IdempotencyKey, time.Now().Add(-s.pendingGrace()), time.Now())
		if err != nil {
			return fail(apiError{http.StatusInternalServerError, "internal", "could not complete request"})
		}
		if !claimed {
			return fail(apiError{http.StatusConflict, "issue_creation_in_progress", "a request with this idempotency key is still in progress; retry in a minute"})
		}
		found, err := s.findIssueByMarker(ctx, writer, repo, row)
		if err != nil {
			return fail(errIssueUnknown)
		}
		if found != nil {
			return s.completeIssue(ctx, row, *found, false)
		}
	}
	issue, err := writer.CreateIssue(ctx, repo, github.NewIssue{Title: in.Title, Body: issueBody(in.Body, row.Actor, row.IdempotencyKey)})
	if err != nil {
		if notCreated(err) {
			// Nothing was created, so the same key may be submitted again.
			_ = s.db.DeletePendingDashboardIssue(ctx, row.IdempotencyKey)
			return fail(githubIssueError(err))
		}
		return fail(errIssueUnknown)
	}
	return s.completeIssue(ctx, row, issue, true)
}

func (s *Server) completeIssue(ctx context.Context, row store.DashboardIssue, issue github.Issue, created bool) (store.DashboardIssue, bool, *apiError) {
	url := issueURL(row.Repo, issue)
	if err := s.db.CompleteDashboardIssue(ctx, row.IdempotencyKey, int64(issue.Number), issue.NodeID, url, time.Now()); err != nil {
		// The issue exists but could not be recorded: leave the row pending so a
		// retry finds it by marker instead of creating another.
		return row, false, &errIssueUnknown
	}
	row.Pending, row.Number, row.NodeID, row.URL = false, int64(issue.Number), issue.NodeID, url
	return row, created, nil
}

// findIssueByMarker lists recent issues of any state looking for this key's
// marker at the end of a body. A search that cannot cover every candidate is an
// error, never "not found".
func (s *Server) findIssueByMarker(ctx context.Context, writer github.IssueWriter, repo github.RepoRef, row store.DashboardIssue) (*github.Issue, error) {
	marker := issueMarker(row.IdempotencyKey)
	since := row.CreatedAt.Add(-time.Minute)
	for page := 1; page <= issueSearchPages; page++ {
		issues, err := writer.ListIssuesSince(ctx, repo, since, page)
		if err != nil {
			return nil, err
		}
		for i := range issues {
			if !issues[i].IsPullRequest && strings.HasSuffix(strings.TrimSpace(issues[i].Body), marker) {
				return &issues[i], nil
			}
		}
		if len(issues) < 100 {
			return nil, nil
		}
	}
	return nil, errors.New("issue search incomplete")
}

// issueRun starts (or finds) the run for a created issue through the same start
// path as POST /api/runs. Creating an issue never approves anything: the run
// stops at awaiting_approval like any other.
func (s *Server) issueRun(ctx, work context.Context, row store.DashboardIssue, in issueRequest) (store.Run, error) {
	if row.RunID > 0 {
		return s.db.GetRun(work, row.RunID)
	}
	// A crash after the run was created but before it was linked must not start
	// a second run: any run on this brand-new issue is ours.
	if page, err := s.db.ListRuns(work, store.RunFilter{Repo: row.Repo, IssueNumber: row.Number, Limit: 1}); err == nil && len(page.Runs) > 0 {
		_ = s.db.SetDashboardIssueRun(work, row.IdempotencyKey, page.Runs[0].ID)
		return page.Runs[0], nil
	}
	ctx = runner.WithActor(ctx, row.Actor)
	run, err := s.startRun(ctx, in.start(row.Number))
	if run.ID > 0 {
		_ = s.db.SetDashboardIssueRun(work, row.IdempotencyKey, run.ID)
	}
	return run, err
}
