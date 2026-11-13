package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

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
	if err := s.db.RecordDelivery(r.Context(), store.Delivery{ID: id, Event: event, Repo: repo, Issue: int64(payload.Issue.Number)}); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	select {
	case s.ingressWake <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) reconcileDeliveries(ctx context.Context) error {
	s.ingressMu.Lock()
	defer s.ingressMu.Unlock()
	deliveries, err := s.db.PendingDeliveries(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range deliveries {
		if err := s.runner.ReconcileIngress(ctx, d.Repo, d.Issue, d.Event == "issues"); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.db.CompleteDelivery(ctx, d.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
