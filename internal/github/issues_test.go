package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCreateIssuePostsNeutralizedContent(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/repos/owner/repo/issues" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if strings.Contains(in["title"], "@octo") && !strings.Contains(in["title"], "@\u200d") || !strings.Contains(in["body"], "@\u200dteam") || !strings.Contains(in["body"], "me@example.com") {
			t.Errorf("mentions not neutralized: %v", in)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, map[string]any{"number": 9, "node_id": "I_kw", "html_url": "https://github.com/owner/repo/issues/9", "title": in["title"], "body": in["body"], "state": "open", "user": map[string]string{"login": "bot[bot]"}})
	})
	issue, err := client.CreateIssue(t.Context(), RepoRef{Owner: "owner", Name: "repo"}, NewIssue{Title: "ping @octocat", Body: "hi @team and me@example.com"})
	if err != nil || issue.Number != 9 || issue.NodeID != "I_kw" || issue.Author != "bot[bot]" {
		t.Fatalf("issue %+v: %v", issue, err)
	}
}

func TestCreateIssueErrorsAreTyped(t *testing.T) {
	t.Parallel()
	for status, check := range map[int]func(error) bool{
		403: func(err error) bool { var e *ErrForbidden; return errors.As(err, &e) },
		404: func(err error) bool { var e *ErrNotFound; return errors.As(err, &e) },
		410: func(err error) bool { var e *APIError; return errors.As(err, &e) && e.StatusCode == 410 },
		422: func(err error) bool { var e *ErrValidation; return errors.As(err, &e) },
	} {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"nope ` + testToken + `"}`))
		})
		_, err := client.CreateIssue(t.Context(), RepoRef{Owner: "owner", Name: "repo"}, NewIssue{Title: "t"})
		if err == nil || !check(err) || strings.Contains(err.Error(), testToken) {
			t.Errorf("status %d: %v", status, err)
		}
	}
}

func TestListIssuesSinceQuery(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "all" || q.Get("sort") != "created" || q.Get("direction") != "desc" || q.Get("page") != "2" || q.Get("since") != "2026-11-14T10:00:00Z" {
			t.Errorf("query %v", q)
		}
		writeJSON(t, w, []map[string]any{{"number": 3, "body": "b"}, {"number": 2, "pull_request": map[string]string{}}})
	})
	issues, err := client.ListIssuesSince(t.Context(), RepoRef{Owner: "owner", Name: "repo"}, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC), 2)
	if err != nil || len(issues) != 2 || issues[0].Body != "b" || !issues[1].IsPullRequest {
		t.Fatalf("%+v %v", issues, err)
	}
}
