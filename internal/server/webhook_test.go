package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

const hookSecret = "webhook-fixture-secret"

func hookRequest(s *Server, body, id, event, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://127.0.0.1/hooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signature)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func hookSignature(body string) string {
	mac := hmac.New(sha256.New, []byte(hookSecret))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookAuthenticationBoundsAndDedupe(t *testing.T) {
	f := newFixture(t, nil, nil)
	cfg := config.Default()
	cfg.Webhook.Enabled = true
	cfg.Webhook.Secret = secret.New(hookSecret)
	s := &Server{cfg: cfg, db: f.db, authenticated: true, ingressWake: make(chan struct{}, 1), repos: f.server.repos}
	cfg.Repos = f.cfg.Repos
	body := ` {"action":"labeled","repository":{"full_name":"owner/repo"},"issue":{"number":12},"label":{"name":"ai-fix"}} `
	for _, signature := range []string{"", "sha256=bad", hookSignature(body + " ")} {
		requireHookStatus(t, hookRequest(s, body, "one", "issues", signature), 401)
	}
	// Even invalid JSON must fail signature verification before parsing.
	requireHookStatus(t, hookRequest(s, "{", "bad-json", "issues", ""), 401)
	requireHookStatus(t, hookRequest(s, "{", "bad-json", "issues", hookSignature("{")), 400)
	huge := strings.Repeat("x", (1<<20)+1)
	requireHookStatus(t, hookRequest(s, huge, "big", "issues", hookSignature(huge)), 413)
	requireHookStatus(t, hookRequest(s, body, "", "issues", hookSignature(body)), 400)
	for range 2 {
		requireHookStatus(t, hookRequest(s, body, "one", "issues", hookSignature(body)), 202)
	}
	pending, err := f.db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 1 || pending[0].ID != "one" {
		t.Fatalf("receipts: %+v: %v", pending, err)
	}
	if err := f.db.CompleteDelivery(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	requireHookStatus(t, hookRequest(s, body, "one", "issues", hookSignature(body)), 202)
	pending, err = f.db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("duplicate rearmed: %+v %v", pending, err)
	}
	for _, ignored := range []string{
		strings.ReplaceAll(body, "owner/repo", "other/repo"),
		strings.ReplaceAll(body, `"number":12`, `"number":12,"pull_request":{}`),
		strings.ReplaceAll(body, "ai-fix", "unrelated"),
		strings.ReplaceAll(body, `"number":12`, `"number":12,"html_url":"https://github.com/other/repo/issues/12"`),
	} {
		requireHookStatus(t, hookRequest(s, ignored, "ignored", "issues", hookSignature(ignored)), 202)
	}
	pending, err = f.db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("ignored receipt: %+v %v", pending, err)
	}
}

func TestWebhookStartupReprocessesReceipt(t *testing.T) {
	f := newFixture(t, nil, nil)
	opts := f.server.opts
	if err := f.server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordDelivery(t.Context(), store.Delivery{ID: "crash-before-dispatch", Event: "issues", Repo: "owner/repo", Issue: 12}, 10); err != nil {
		t.Fatal(err)
	}
	f.cfg.Webhook.Enabled = true
	f.cfg.Webhook.Secret = secret.New(hookSecret)
	var err error
	f.server, err = New(t.Context(), f.cfg, f.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pending, err := f.db.PendingDeliveries(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup did not replay receipt")
		}
		time.Sleep(time.Millisecond)
	}
}

func requireHookStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("webhook status %d, want %d", w.Code, want)
	}
}

func TestWebhookPendingBacklogIsBounded(t *testing.T) {
	f := newFixture(t, nil, nil)
	cfg := config.Default()
	cfg.Webhook.Enabled = true
	cfg.Webhook.Secret = secret.New(hookSecret)
	cfg.Repos = f.cfg.Repos
	s := &Server{cfg: cfg, db: f.db, authenticated: true, ingressWake: make(chan struct{}, 1), maxPending: 2, repos: f.server.repos}
	body := `{"action":"created","repository":{"full_name":"owner/repo"},"issue":{"number":12}}`
	for _, id := range []string{"a", "b"} {
		requireHookStatus(t, hookRequest(s, body, id, "issue_comment", hookSignature(body)), 202)
	}
	// Fresh delivery IDs on a replayed body cannot grow the backlog.
	requireHookStatus(t, hookRequest(s, body, "c", "issue_comment", hookSignature(body)), 503)
	requireHookStatus(t, hookRequest(s, body, "a", "issue_comment", hookSignature(body)), 202)
	pending, err := f.db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 2 {
		t.Fatalf("backlog recorded overflow: %+v %v", pending, err)
	}
}

func TestWebhookDeliveriesCoalescePerIssue(t *testing.T) {
	f := newFixture(t, nil, nil)
	for i := range 20 {
		event := "issue_comment"
		if i%2 == 0 {
			event = "issues"
		}
		if err := f.db.RecordDelivery(t.Context(), store.Delivery{ID: fmt.Sprint("burst-", i), Event: event, Repo: "owner/repo", Issue: 12}, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.RecordDelivery(t.Context(), store.Delivery{ID: "other", Event: "issues", Repo: "owner/repo", Issue: 13}, 100); err != nil {
		t.Fatal(err)
	}
	f.gh.mu.Lock()
	before := f.gh.issueReads
	f.gh.mu.Unlock()
	if err := f.server.reconcileDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.gh.mu.Lock()
	reads := f.gh.issueReads - before
	f.gh.mu.Unlock()
	if reads != 2 {
		t.Fatalf("21 deliveries for 2 issues cost %d issue reads", reads)
	}
	pending, err := f.db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("coalesced deliveries left pending: %+v %v", pending, err)
	}
}

func TestWebhookDeliveryGivesUpAndGoneIsDone(t *testing.T) {
	f := newFixture(t, nil, nil)
	record := func(id string) {
		t.Helper()
		if err := f.db.RecordDelivery(t.Context(), store.Delivery{ID: id, Event: "issues", Repo: "owner/repo", Issue: 12}, 100); err != nil {
			t.Fatal(err)
		}
	}
	f.gh.mu.Lock()
	f.gh.issueErr = &github.ErrNotFound{APIError: github.APIError{StatusCode: 404, Message: "Not Found"}}
	f.gh.mu.Unlock()
	record("deleted")
	if err := f.server.reconcileDeliveries(t.Context()); err != nil {
		t.Fatalf("deleted issue retried: %v", err)
	}
	f.gh.mu.Lock()
	f.gh.issueErr = errors.New("transient")
	f.gh.mu.Unlock()
	record("failing")
	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		if err := f.server.reconcileDeliveries(t.Context()); err == nil {
			t.Fatal("failure not reported")
		}
		pending, err := f.db.PendingDeliveries(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want := 1; attempt == maxDeliveryAttempts {
			if len(pending) != 0 {
				t.Fatalf("not abandoned after %d attempts", attempt)
			}
		} else if len(pending) != want {
			t.Fatalf("attempt %d pending %d", attempt, len(pending))
		}
	}
}
