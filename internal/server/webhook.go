package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/store"
)

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	stopping := s.stopping
	s.mu.RUnlock()
	if stopping {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	signature, ok := strings.CutPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	received, err := hex.DecodeString(signature)
	mac := hmac.New(sha256.New, []byte(s.cfg.Webhook.Secret.Reveal()))
	_, _ = mac.Write(body)
	if !ok || err != nil || !s.cfg.Webhook.Secret.IsSet() || !hmac.Equal(received, mac.Sum(nil)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	id := r.Header.Get("X-GitHub-Delivery")
	if id == "" || len(id) > 200 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Issue github.Issue `json:"issue"`
		Label github.Label `json:"label"`
	}
	if json.Unmarshal(body, &payload) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	repo := strings.ToLower(payload.Repository.FullName)
	eligible := event == "issue_comment" || (event == "issues" && (payload.Action == "unlabeled" || (payload.Action == "labeled" && s.cfg.Webhook.TriggerLabels[payload.Label.Name] != "")))
	if payload.Issue.HTMLURL != "" && !strings.EqualFold(payload.Issue.HTMLURL, "https://github.com/"+repo+"/issues/"+strconv.Itoa(payload.Issue.Number)) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !eligible || payload.Issue.IsPullRequest || payload.Issue.Number <= 0 || s.cfg.Repos[repo] == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.db.RecordDelivery(r.Context(), store.Delivery{ID: id, Event: event, Repo: repo, Issue: int64(payload.Issue.Number)}, s.pendingLimit()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	select {
	case s.ingressWake <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusAccepted)
}

const (
	// maxPendingDeliveries bounds unprocessed receipts; beyond it the receiver
	// answers 503 without recording, and GitHub may redeliver later.
	maxPendingDeliveries = 1000
	// maxDeliveryAttempts abandons a hint that keeps failing; label and comment
	// polling still recover the underlying state.
	maxDeliveryAttempts = 10
	// deliveryRetention is how long processed receipts stay deduplicated.
	deliveryRetention = 14 * 24 * time.Hour
	// ingressReconcileTimeout bounds one coalesced issue reconcile so a slow
	// GitHub response cannot stall the maintenance loop.
	ingressReconcileTimeout = 2 * time.Minute
)

func (s *Server) pendingLimit() int {
	if s.maxPending > 0 {
		return s.maxPending
	}
	return maxPendingDeliveries
}

type ingressGroup struct {
	repo             string
	issue            int64
	ids              []string
	comments, labels bool
}

// reconcileDeliveries coalesces pending hints per issue: any number of replayed
// or bursty deliveries for one issue cost one current-state reconcile.
func (s *Server) reconcileDeliveries(ctx context.Context) error {
	s.ingressMu.Lock()
	defer s.ingressMu.Unlock()
	deliveries, err := s.db.PendingDeliveries(ctx)
	if err != nil {
		return err
	}
	type key struct {
		repo  string
		issue int64
	}
	index := make(map[key]*ingressGroup)
	var groups []*ingressGroup
	for _, d := range deliveries {
		k := key{d.Repo, d.Issue}
		g := index[k]
		if g == nil {
			g = &ingressGroup{repo: d.Repo, issue: d.Issue}
			index[k] = g
			groups = append(groups, g)
		}
		g.ids = append(g.ids, d.ID)
		if d.Event == "issues" {
			g.labels = true
		} else {
			g.comments = true
		}
	}
	var errs []error
	for _, g := range groups {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		work, cancel := context.WithTimeout(ctx, ingressReconcileTimeout)
		err := s.runner.ReconcileIssue(work, g.repo, g.issue, g.comments, g.labels)
		cancel()
		if err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				continue // shutdown is not a failed attempt
			}
			abandoned, failErr := s.db.FailDeliveries(ctx, g.ids, maxDeliveryAttempts)
			if failErr != nil {
				errs = append(errs, failErr)
			}
			if abandoned > 0 {
				slog.Warn("abandoned webhook deliveries after repeated failures", "repo", g.repo, "issue", g.issue, "count", abandoned)
			}
			continue
		}
		if err := s.db.CompleteDeliveries(ctx, g.ids); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pruneDeliveries forgets processed receipts after the retention window.
func (s *Server) pruneDeliveries(ctx context.Context) error {
	_, err := s.db.PruneDeliveries(ctx, time.Now().Add(-deliveryRetention))
	return err
}

// logMaintenance reports a background failure without raw secrets.
func (s *Server) logMaintenance(task string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	message := err.Error()
	if s.redactor != nil {
		message = s.redactor.Redact(message)
	}
	slog.Warn("maintenance task failed", "task", task, "error", message)
}
