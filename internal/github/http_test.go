package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/version"
)

const testToken = "ghp_m11_dummy_token_never_expose_123456789" //nolint:gosec // Deliberately fake credential used to verify redaction.

var testRepo = RepoRef{Owner: "owner", Name: "repo"}

type tokenFunc func(context.Context) (string, error)

func (f tokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

func newTestClient(t *testing.T, handler http.HandlerFunc) *HTTPClient {
	t.Helper()
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, want := range map[string]string{
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
			"User-Agent":           "patchbot/" + version.Version,
			"Authorization":        "Bearer " + testToken,
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("request header %s is incorrect", name)
			}
		}
		if strings.Contains(r.URL.String(), testToken) {
			t.Error("credential leaked into URL")
		}
		handler(w, r)
	}))
	client, err := New(server.Client(), server.URL+"/api/v3/", tokenFunc(func(context.Context) (string, error) {
		return testToken, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestOperations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tt := range []struct {
		name, method, path, query, response string
		status                              int
		body, want                          any
		run                                 func(*HTTPClient) (any, error)
	}{
		{
			name: "repository", method: "GET", path: "/repos/owner/repo", status: 200,
			response: `{"name":"repo","default_branch":"main","clone_url":"https://example.org/repo.git","permissions":{"push":true},"private":true}`,
			want:     Repo{Name: "repo", DefaultBranch: "main", CloneURL: "https://example.org/repo.git", Permissions: Permissions{Push: true}, Private: true},
			run:      func(c *HTTPClient) (any, error) { return c.GetRepo(ctx, testRepo) },
		},
		{
			name: "issue", method: "GET", path: "/repos/owner/repo/issues/7", status: 200,
			response: `{"number":7,"title":"bug","body":"details","state":"open","labels":[{"name":"ai-fix"}],"user":{"login":"alice"},"author_association":"COLLABORATOR","updated_at":"2026-11-10T00:00:00Z"}`,
			want:     Issue{Number: 7, Title: "bug", Body: "details", State: "open", Labels: []Label{{Name: "ai-fix"}}, Author: "alice", AuthorAssociation: "COLLABORATOR", UpdatedAt: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)},
			run:      func(c *HTTPClient) (any, error) { return c.GetIssue(ctx, testRepo, 7) },
		},
		{
			name: "issue is PR", method: "GET", path: "/repos/owner/repo/issues/7", status: 200,
			response: `{"number":7,"pull_request":{"url":"https://example.org/pr/7"}}`, want: Issue{Number: 7, IsPullRequest: true},
			run: func(c *HTTPClient) (any, error) { return c.GetIssue(ctx, testRepo, 7) },
		},
		{
			name: "get comment", method: "GET", path: "/repos/owner/repo/issues/comments/99", status: 200,
			response: `{"id":99,"body":"spec","user":{"login":"patchbot"},"html_url":"https://example.org/comment/99"}`,
			want:     Comment{ID: 99, Body: "spec", Author: "patchbot", HTMLURL: "https://example.org/comment/99"},
			run:      func(c *HTTPClient) (any, error) { return c.GetIssueComment(ctx, testRepo, 99) },
		},
		{
			name: "create comment", method: "POST", path: "/repos/owner/repo/issues/7/comments", status: 201,
			body: map[string]any{"body": "Hi @\u200dalice `@bob`"}, response: `{"id":9,"user":{"login":"patchbot"}}`, want: Comment{ID: 9, Author: "patchbot"},
			run: func(c *HTTPClient) (any, error) { return c.CreateIssueComment(ctx, testRepo, 7, "Hi @alice `@bob`") },
		},
		{
			name: "update comment", method: "PATCH", path: "/repos/owner/repo/issues/comments/99", status: 200,
			body: map[string]any{"body": "See @\u200dorg/team"}, response: `{}`,
			run: func(c *HTTPClient) (any, error) { return nil, c.UpdateIssueComment(ctx, testRepo, 99, "See @org/team") },
		},
		{
			name: "collaborator", method: "GET", path: "/repos/owner/repo/collaborators/alice", status: 204, want: true,
			run: func(c *HTTPClient) (any, error) { return c.IsCollaborator(ctx, testRepo, "alice") },
		},
		{
			name: "not collaborator", method: "GET", path: "/repos/owner/repo/collaborators/alice", status: 404, response: `{"message":"Not Found"}`, want: false,
			run: func(c *HTTPClient) (any, error) { return c.IsCollaborator(ctx, testRepo, "alice") },
		},
		{
			name: "find PR", method: "GET", path: "/repos/owner/repo/pulls", status: 200, query: "head=owner%3Apatchbot%2Fissue-7&per_page=100&state=open",
			response: `[{"number":8,"draft":true,"head":{"ref":"patchbot/issue-7","sha":"abc"}}]`,
			want:     &PullRequest{Number: 8, Draft: true, Head: PRBranch{Ref: "patchbot/issue-7", SHA: "abc"}},
			run:      func(c *HTTPClient) (any, error) { return c.FindOpenPR(ctx, testRepo, "patchbot/issue-7") },
		},
		{
			name: "no open PR", method: "GET", path: "/repos/owner/repo/pulls", status: 200, query: "head=owner%3Apatchbot%2Fissue-7&per_page=100&state=open", response: `[]`, want: (*PullRequest)(nil),
			run: func(c *HTTPClient) (any, error) { return c.FindOpenPR(ctx, testRepo, "patchbot/issue-7") },
		},
		{
			name: "create draft PR", method: "POST", path: "/repos/owner/repo/pulls", status: 201,
			body:     map[string]any{"title": "Fix for @\u200dalice", "body": "Tests failed @\u200dorg/team", "head": "patchbot/issue-7", "base": "main", "draft": true},
			response: `{"number":8,"draft":true,"html_url":"https://example.org/pr/8"}`, want: PullRequest{Number: 8, Draft: true, HTMLURL: "https://example.org/pr/8"},
			run: func(c *HTTPClient) (any, error) {
				return c.CreatePR(ctx, testRepo, NewPR{Title: "Fix for @alice", Body: "Tests failed @org/team", Head: "patchbot/issue-7", Base: "main", Draft: true})
			},
		},
		{
			name: "branch exists", method: "GET", path: "/repos/owner/repo/branches/patchbot%2Fissue-7", status: 200, response: `{"name":"patchbot/issue-7","commit":{"sha":"abc"}}`, want: true,
			run: func(c *HTTPClient) (any, error) {
				branch, exists, err := c.GetBranch(ctx, testRepo, "patchbot/issue-7")
				if err == nil && (branch.Name != "patchbot/issue-7" || branch.Commit.SHA != "abc") {
					return nil, errors.New("branch metadata missing")
				}
				return exists, err
			},
		},
		{
			name: "branch missing", method: "GET", path: "/repos/owner/repo/branches/missing", status: 404, response: `{}`, want: false,
			run: func(c *HTTPClient) (any, error) {
				_, exists, err := c.GetBranch(ctx, testRepo, "missing")
				return exists, err
			},
		},
		{
			name: "branch convenience", method: "GET", path: "/repos/owner/repo/branches/main", status: 200, response: `{"name":"main"}`, want: true,
			run: func(c *HTTPClient) (any, error) { return c.BranchExists(ctx, testRepo, "main") },
		},
		{
			name: "add labels", method: "POST", path: "/repos/owner/repo/issues/7/labels", status: 200, response: `[]`, body: map[string]any{"labels": []any{"ai-fix", "ready"}},
			run: func(c *HTTPClient) (any, error) {
				return nil, c.AddLabels(ctx, testRepo, 7, []string{"ai-fix", "ready"})
			},
		},
		{
			name: "remove label", method: "DELETE", path: "/repos/owner/repo/issues/7/labels/ai%2Ffix%20now", status: 200, response: `[]`,
			run: func(c *HTTPClient) (any, error) { return nil, c.RemoveLabel(ctx, testRepo, 7, "ai/fix now") },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.EscapedPath() != "/api/v3"+tt.path || r.URL.RawQuery != tt.query {
					t.Errorf("request = %s %s; want %s /api/v3%s?%s", r.Method, r.URL, tt.method, tt.path, tt.query)
				}
				if tt.body != nil {
					if r.Header.Get("Content-Type") != "application/json" {
						t.Error("missing JSON content type")
					}
					var body any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode request: %v", err)
					}
					if diff := cmp.Diff(tt.body, body); diff != "" {
						t.Errorf("request body (-want +got):\n%s", diff)
					}
				}
				w.WriteHeader(tt.status)
				if _, err := io.WriteString(w, tt.response); err != nil {
					t.Errorf("write response: %v", err)
				}
			})
			got, err := tt.run(client)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTypedErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		status  int
		message string
		headers map[string]string
		target  any
	}{
		{"not found", 404, "Not Found", nil, new(*ErrNotFound)},
		{"unauthorized", 401, "Bad credentials", nil, new(*ErrUnauthorized)},
		{"forbidden", 403, "Resource not accessible", nil, new(*ErrForbidden)},
		{"primary rate limit", 403, "API rate limit exceeded", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "2000000000"}, new(*ErrRateLimited)},
		{"secondary rate limit", 403, "You have exceeded a secondary rate limit", nil, new(*ErrRateLimited)},
		{"secondary retry", 403, "Please wait", map[string]string{"Retry-After": "60"}, new(*ErrRateLimited)},
		{"abuse limit", 403, "You have triggered an abuse detection mechanism", nil, new(*ErrRateLimited)},
		{"429", 429, "Too many requests", nil, new(*ErrRateLimited)},
		{"retry date", 429, "Too many requests", map[string]string{"Retry-After": "Wed, 18 May 2033 03:33:20 GMT"}, new(*ErrRateLimited)},
		{"validation", 422, "Validation Failed: head already exists", nil, new(*ErrValidation)},
		{"other", 500, "Internal server error", nil, new(*APIError)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tt.status)
				writeJSON(t, w, map[string]string{"message": tt.message + " " + testToken})
			})
			before := time.Now()
			_, err := client.GetRepo(context.Background(), testRepo)
			if err == nil || !errors.As(err, tt.target) {
				t.Fatalf("error = %v; want %T", err, tt.target)
			}
			if !strings.HasPrefix(err.Error(), "GetRepo: ") || !strings.Contains(err.Error(), tt.message) {
				t.Errorf("missing operation or GitHub message: %v", err)
			}
			assertNoToken(t, err)
			var api *APIError
			if !errors.As(err, &api) || api.StatusCode != tt.status {
				t.Errorf("missing common API error metadata: %v", err)
			}
			var rate *ErrRateLimited
			if errors.As(err, &rate) {
				switch {
				case tt.headers["X-RateLimit-Reset"] != "":
					if want := time.Unix(2000000000, 0); !rate.Reset.Equal(want) {
						t.Errorf("reset = %v; want %v", rate.Reset, want)
					}
				case tt.headers["Retry-After"] == "60":
					if rate.Reset.Before(before.Add(time.Minute)) || rate.Reset.After(time.Now().Add(time.Minute)) {
						t.Errorf("retry reset = %v", rate.Reset)
					}
				case tt.headers["Retry-After"] != "":
					want, parseErr := http.ParseTime(tt.headers["Retry-After"])
					if parseErr != nil || !rate.Reset.Equal(want) {
						t.Errorf("retry date = %v; want %v", rate.Reset, want)
					}
				default:
					if !rate.Reset.IsZero() {
						t.Errorf("reset without headers = %v", rate.Reset)
					}
				}
			}
		})
	}
}

func TestCreatePRValidation(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(t, w, map[string]string{"message": "Validation Failed: no commits between head and base " + testToken})
	})
	_, err := client.CreatePR(context.Background(), testRepo, NewPR{Title: "Fix", Head: "patchbot/issue-7", Base: "main", Draft: true})
	var validation *ErrValidation
	if !errors.As(err, &validation) || !strings.HasPrefix(err.Error(), "CreatePR: ") ||
		!strings.Contains(validation.Message, "no commits between head and base") {
		t.Fatalf("CreatePR did not surface validation failure: %v", err)
	}
	assertNoToken(t, err)
}

func assertNoToken(t *testing.T, err error) {
	t.Helper()
	for current := err; current != nil; current = errors.Unwrap(current) {
		for _, secret := range tokenForms(testToken) {
			if strings.Contains(current.Error(), secret) {
				t.Error("credential leaked in error chain")
			}
		}
	}
}

func TestCommentPagination(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC)
	for _, tt := range []struct {
		name                    string
		perPage, want, requests int
	}{
		{"three pages", 2, 6, 3},
		{"300 cap", 100, 300, 3},
		{"oversized server page", 120, 300, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				query := r.URL.Query()
				if query.Get("since") != since.Format(time.RFC3339) || query.Get("per_page") != "100" {
					t.Errorf("pagination query: %v", query)
				}
				page := 1
				if text := query.Get("page"); text != "" {
					var err error
					page, err = strconv.Atoi(text)
					if err != nil {
						t.Error(err)
					}
				}
				if page < 3 || tt.perPage >= 100 {
					query.Set("page", strconv.Itoa(page+1))
					w.Header().Set("Link", fmt.Sprintf(`<%s?%s>; rel="last", <%s?%s>; rel="next"`, r.URL.Path, query.Encode(), r.URL.Path, query.Encode()))
				}
				batch := make([]map[string]any, tt.perPage)
				for index := range batch {
					batch[index] = map[string]any{"id": (page-1)*tt.perPage + index + 1, "user": map[string]string{"login": "alice"}, "created_at": "2026-11-10T00:00:00Z", "updated_at": "2026-11-10T00:00:00Z"}
				}
				writeJSON(t, w, batch)
			})
			comments, err := client.ListIssueComments(context.Background(), testRepo, 7, since)
			if err != nil {
				t.Fatal(err)
			}
			if len(comments) != tt.want || int(requests.Load()) != tt.requests {
				t.Fatalf("got %d comments, %d requests; want %d, %d", len(comments), requests.Load(), tt.want, tt.requests)
			}
			for index, comment := range comments {
				if comment.ID != int64(index+1) || comment.Author != "alice" || comment.CreatedAt.IsZero() || !comment.UpdatedAt.Equal(comment.CreatedAt) {
					t.Errorf("comment %d lost metadata: %+v", index, comment)
				}
			}
		})
	}
}

func TestConditionalCommentPolling(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		etag      string
		status    int
		pages     int
		wantETag  string
		unchanged bool
		wantError bool
	}{
		{name: "new response", status: 200, pages: 1, wantETag: `"new"`},
		{name: "unchanged", etag: `"old"`, status: 304, pages: 1, wantETag: `"new"`, unchanged: true},
		{name: "multiple pages disable etag", etag: `"old"`, status: 200, pages: 2},
		{name: "unsolicited 304", status: 304, pages: 1, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				call := int(calls.Add(1))
				want := ""
				if call == 1 {
					want = tt.etag
				}
				if got := r.Header.Get("If-None-Match"); got != want {
					t.Errorf("If-None-Match = %q; want %q", got, want)
				}
				if r.URL.Query().Has("since") {
					t.Error("zero since should be omitted")
				}
				if call < tt.pages {
					w.Header().Set("Link", `<`+r.URL.Path+`?page=2>; rel="next"`)
				}
				w.Header().Set("ETag", `"new"`)
				w.WriteHeader(tt.status)
				if tt.status == 200 {
					writeJSON(t, w, []map[string]any{{"id": call}})
				}
			})
			comments, etag, unchanged, err := client.ListIssueCommentsSince(context.Background(), testRepo, 7, time.Time{}, tt.etag)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v; want error %v", err, tt.wantError)
			}
			if tt.wantError {
				return
			}
			if etag != tt.wantETag || unchanged != tt.unchanged || (!unchanged && len(comments) != tt.pages) {
				t.Fatalf("got %d comments, ETag %q, unchanged %v", len(comments), etag, unchanged)
			}
		})
	}
}

func TestErrorBodyRedactionAndLimit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body string }{
		{"json", `{"message":"echo ` + testToken + `"}`},
		{"plain", "echo " + testToken},
		{"base64", base64.StdEncoding.EncodeToString([]byte(testToken))},
		{"malformed json", `{"message":"` + testToken},
		{"truncated credential", strings.Repeat("x", 1000) + testToken},
		{"long body", strings.Repeat("x", 10000)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(500)
				_, _ = io.WriteString(w, tt.body)
			})
			_, err := client.GetRepo(context.Background(), testRepo)
			var api *APIError
			if !errors.As(err, &api) {
				t.Fatalf("error = %v; want APIError", err)
			}
			assertNoToken(t, err)
			if len(api.Message) > maxErrorBody {
				t.Errorf("error message has %d bytes", len(api.Message))
			}
			if strings.Contains(api.Message, "ghp_m11_dummy") {
				t.Error("partial credential was not redacted")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingBody struct{ closed atomic.Bool }

func (*failingBody) Read([]byte) (int, error) { return 0, errors.New(testToken) }
func (b *failingBody) Close() error           { b.closed.Store(true); return nil }

func TestLocalErrorsNeverExposeToken(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(*HTTPClient)
	}{
		{"token source", func(c *HTTPClient) {
			c.tokens = tokenFunc(func(context.Context) (string, error) { return "", errors.New(testToken) })
		}},
		{"invalid token", func(c *HTTPClient) {
			c.tokens = tokenFunc(func(context.Context) (string, error) { return testToken + "\n", nil })
		}},
		{"empty token", func(c *HTTPClient) {
			c.tokens = tokenFunc(func(context.Context) (string, error) { return "", nil })
		}},
		{"transport", func(c *HTTPClient) {
			c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New(testToken)
			})
		}},
		{"read response", func(c *HTTPClient) {
			c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: &failingBody{}, Header: make(http.Header)}, nil
			})
		}},
		{"read error body", func(c *HTTPClient) {
			c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 500, Body: &failingBody{}, Header: make(http.Header)}, nil
			})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) { t.Error("unexpected API request") })
			tt.edit(client)
			_, err := client.GetRepo(context.Background(), testRepo)
			if err == nil {
				t.Fatal("expected error")
			}
			assertNoToken(t, err)
		})
	}
}

func TestResponseLimitsAndMalformedJSON(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body, want string }{
		{"oversized", `{}` + strings.Repeat(" ", maxResponseBody), "exceeds 10 MB"},
		{"malformed", testToken, "invalid API JSON"},
		{"bad timestamp", `{"updated_at":"` + testToken + `"}`, "invalid API JSON"},
		{"trailing json", `{} {}`, "invalid API JSON"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tt.body) })
			_, err := client.GetIssue(context.Background(), testRepo, 7)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want %q", err, tt.want)
			}
			assertNoToken(t, err)
		})
	}
}

func TestPaginationSecurity(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, link string }{
		{"foreign origin", `<https://example.invalid/comments?page=2>; rel="next"`},
		{"foreign endpoint", `</api/v3/user>; rel="next"`},
		{"malformed", `<http://%>; rel="next"`},
		{"cycle", `</api/v3/repos/owner/repo/issues/7/comments?per_page=100>; rel="next"`},
		{"credential query", `</api/v3/repos/owner/repo/issues/7/comments?token=` + testToken + `>; rel="next"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Link", tt.link)
				writeJSON(t, w, []Comment{})
			})
			_, err := client.ListIssueComments(context.Background(), testRepo, 7, time.Time{})
			if err == nil || calls.Load() != 1 {
				t.Fatalf("error = %v, requests = %d; want error and one request", err, calls.Load())
			}
			assertNoToken(t, err)
		})
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var originCalls atomic.Int32
	other := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	_ = other.Client() // Initialize the in-memory server URL.
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		originCalls.Add(1)
		w.Header().Set("Location", other.URL+"/"+testToken)
		w.WriteHeader(http.StatusFound)
	})
	_, err := client.GetRepo(context.Background(), testRepo)
	if err == nil || calls.Load() != 0 || originCalls.Load() != 1 {
		t.Fatalf("redirect error = %v, external requests = %d, origin requests = %d", err, calls.Load(), originCalls.Load())
	}
	assertNoToken(t, err)
}

func TestCancellationAndDeadlines(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			ctx, cancel := tt.ctx()
			defer cancel()
			_, err := client.GetRepo(ctx, testRepo)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v; want %v", err, tt.want)
			}
		})
	}
	client := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.tokens = tokenFunc(func(ctx context.Context) (string, error) {
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining > 30*time.Second || remaining < 29*time.Second {
			t.Errorf("default timeout = %v, present %v", remaining, ok)
		}
		return testToken, nil
	})
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("request has no deadline")
		}
		return nil, context.Canceled
	})
	_, err := client.GetRepo(context.Background(), testRepo)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	ts := tokenFunc(func(context.Context) (string, error) { return testToken, nil })
	for _, base := range []string{"http://%", "ftp://example.org", "https://user:" + testToken + "@example.org", "https://example.org?token=" + testToken, "https://example.org#fragment", "/relative"} {
		_, err := New(nil, base, ts)
		if err == nil {
			t.Error("accepted invalid base URL")
		}
		assertNoToken(t, err)
	}
	if _, err := New(nil, "", nil); err == nil {
		t.Error("accepted nil token source")
	}
	client, err := New(nil, "", ts)
	if err != nil || client.baseURL.String() != defaultBaseURL {
		t.Fatalf("default constructor: %v", err)
	}
	// Construction does not mutate the caller's client or redirect policy.
	custom := &http.Client{Timeout: time.Second}
	if _, err := New(custom, "https://example.org/api/v3", ts); err != nil {
		t.Fatal(err)
	}
	if custom.CheckRedirect != nil || custom.Timeout != time.Second {
		t.Error("constructor mutated supplied client")
	}
}

func TestConcurrentRequests(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, Repo{DefaultBranch: "main"}) })
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := client.GetRepo(context.Background(), testRepo); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestRateResetHeaderPrecedence(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	for _, tt := range []struct {
		reset, retry string
		want         time.Time
	}{
		{"1500", "60", time.Unix(1500, 0)},
		{"1001", "60", time.Unix(1060, 0)},
		{"invalid", "invalid", time.Time{}},
		{"-1", "-1", time.Time{}},
		{"", "9223372036854775807", time.Time{}},
	} {
		header := make(http.Header)
		header.Set("X-RateLimit-Reset", tt.reset)
		header.Set("Retry-After", tt.retry)
		if got := rateReset(header, now); !got.Equal(tt.want) {
			t.Errorf("reset %q, retry %q = %v; want %v", tt.reset, tt.retry, got, tt.want)
		}
	}
}

func TestPaginationAbsoluteLinkAndRelations(t *testing.T) {
	t.Parallel()
	current, err := url.Parse("https://example.org/api/v3/comments?page=1")
	if err != nil {
		t.Fatal(err)
	}
	header := make(http.Header)
	header.Add("Link", `<https://example.org/api/v3/comments?page=9>; rel="last"`)
	header.Add("Link", `<https://example.org/api/v3/comments?page=2>; type="application/json"; rel="next alternate"`)
	next, err := nextPage(current, header)
	if err != nil || next.String() != "https://example.org/api/v3/comments?page=2" {
		t.Fatalf("nextPage() = %v, %v", next, err)
	}
}
