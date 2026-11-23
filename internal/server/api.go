package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// Register paths without method patterns so method and 404 errors share JSON.
	mux.HandleFunc("/{$}", s.page)
	mux.HandleFunc("/runs", s.page)
	mux.HandleFunc("/runs/new", s.page)
	mux.HandleFunc("/repos", s.page)
	mux.HandleFunc("/memory", s.page)
	mux.HandleFunc("/runs/{id}", s.page)
	mux.HandleFunc("/assets/{name}", s.asset)
	mux.HandleFunc("/assets/components/{name}", s.asset)
	mux.HandleFunc("/assets/composables/{name}", s.asset)
	mux.HandleFunc("/api/session", s.session)
	mux.HandleFunc("/api/providers", s.providers)
	mux.HandleFunc("/api/providers/{id}/models", s.providerModels)
	mux.HandleFunc("/api/runs", s.runs)
	mux.HandleFunc("/api/issues", s.createIssue)
	mux.HandleFunc("/api/runs/{id}", s.runDetail)
	mux.HandleFunc("/api/runs/{id}/spec", s.editSpec)
	mux.HandleFunc("/api/runs/{id}/events", s.events)
	mux.HandleFunc("/api/runs/{id}/artifacts/{name}", s.artifact)
	for _, action := range []string{"approve", "revise", "reject", "cancel", "retry", "answer", "escalate"} {
		mux.HandleFunc("/api/runs/{id}/"+action, s.action)
	}
	s.memoryRoutes(mux)
	s.repoRoutes(mux)
	mux.HandleFunc("/setup/github", s.githubSetupPage)
	mux.HandleFunc(github.CallbackPath, s.githubSetupCallback)
	mux.HandleFunc("/api/github/setup/start", s.githubSetupStart)
	mux.HandleFunc("/api/github/status", s.githubStatus)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { s.writeError(w, 404, "not_found", "resource not found") })
	return mux
}

func (s *Server) method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, method := range allowed {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	return false
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	type provider struct {
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		DefaultModel string `json:"default_model"`
		Available    bool   `json:"available"`
	}
	values := make([]provider, 0, len(s.registry.Names()))
	for _, name := range s.registry.Names() {
		cfg, _ := s.registry.Config(name)
		_, err := s.registry.Provider(name)
		values = append(values, provider{name, cfg.Kind, cfg.DefaultModel, err == nil})
	}
	repos := s.repos.Names()
	s.writeJSON(w, 200, map[string]any{"providers": values, "repos": repos, "defaults": map[string]any{"provider": s.cfg.Defaults.Provider, "mode": s.cfg.Defaults.Mode, "refine_model": s.cfg.Model("refine"), "code_model": s.cfg.Model("code"), "max_cost_usd": s.cfg.Defaults.MaxCostUSD}})
}

func (s *Server) providerModels(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	if _, ok := s.registry.Config(r.PathValue("id")); !ok {
		s.writeError(w, 404, "not_found", "provider not found")
		return
	}
	list, err := s.models.List(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "provider_unavailable", "model listing failed and no configured models are available")
		return
	}
	type model struct {
		ID          string          `json:"id"`
		DisplayName string          `json:"display_name"`
		Source      llm.ModelSource `json:"source"`
	}
	values := make([]model, 0, len(list.Models))
	for _, m := range list.Models {
		values = append(values, model{m.ID, m.DisplayName, m.Source})
	}
	s.writeJSON(w, 200, map[string]any{"models": values, "live": list.Live, "warning": list.Warning, "fetched_at": list.FetchedAt})
}

type startRequest struct {
	Repo          string `json:"repo"`
	Issue         int64  `json:"issue"`
	Mode          string `json:"mode"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	RefineModel   string `json:"refine_model"`
	CodeModel     string `json:"code_model"`
	DryRun        bool   `json:"dry_run"`
	MaxCostMicros int64  `json:"max_cost_micros"`
	// IngressKey is set by the server only (never decoded from a request).
	IngressKey string `json:"-"`
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		s.listRuns(w, r)
		return
	}
	var input startRequest
	if !s.decode(w, r, &input, false) {
		return
	}
	input.Repo = strings.ToLower(strings.TrimSpace(input.Repo))
	if !s.repoAllowed(input.Repo) || input.Issue <= 0 {
		s.writeError(w, 400, "invalid_request", startInvalid)
		return
	}
	if message := s.validStartOptions(input); message != "" {
		s.writeError(w, 400, "invalid_request", message)
		return
	}
	run, err := s.startRun(r.Context(), input)
	if err != nil {
		s.respondError(w, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/runs/%d", run.ID))
	s.writeJSON(w, http.StatusAccepted, map[string]any{"run": publicRun(run)})
}

const startInvalid = "require an allowlisted repo, positive issue, valid mode, models and budget"

// repoAllowed is the single allowlist check for requests naming a repository.
// repo must already be trimmed and lower-cased.
func (s *Server) repoAllowed(repo string) bool { return s.repos.Allowed(repo) }

// validStartOptions checks everything in a start request except repo and
// issue. It returns a client-safe message, or "" when the request is valid.
func (s *Server) validStartOptions(input startRequest) string {
	if (input.Mode != "" && input.Mode != config.ModeFix && input.Mode != config.ModeRefine && input.Mode != config.ModeInvestigate) || input.MaxCostMicros < 0 || !validModels(input.Model, input.RefineModel, input.CodeModel) {
		return startInvalid
	}
	if input.Provider != "" && s.cfg.Providers[input.Provider] == nil {
		return "unknown provider"
	}
	return ""
}

// startRun is the one place a dashboard request becomes a run. POST /api/runs
// and POST /api/issues both use it, so lifecycle rules live only in the runner.
func (s *Server) startRun(ctx context.Context, input startRequest) (store.Run, error) {
	return s.runner.Start(ctx, runner.Request{IngressKey: input.IngressKey, Repo: input.Repo, Issue: input.Issue, Mode: input.Mode, Provider: input.Provider, Model: input.Model, RefineModel: input.RefineModel, CodeModel: input.CodeModel, DryRun: input.DryRun, MaxCostMicros: input.MaxCostMicros})
}

func validModels(models ...string) bool {
	for _, model := range models {
		if len(model) > 512 || strings.ContainsAny(model, "\r\n\x00") || (model != "" && strings.TrimSpace(model) == "") {
			return false
		}
	}
	return true
}

func validStatus(status store.Status) bool {
	return status.IsActive() || status == store.StatusSucceeded || status == store.StatusFailed || status == store.StatusRejected || status == store.StatusCancelled
}

func parseFilter(query url.Values) (store.RunFilter, error) {
	filter := store.RunFilter{Repo: query.Get("repo"), Cursor: query.Get("cursor")}
	var err error
	if issue := query.Get("issue"); issue != "" {
		filter.IssueNumber, err = strconv.ParseInt(issue, 10, 64)
		if err != nil || filter.IssueNumber <= 0 {
			return filter, errors.New("invalid issue")
		}
	}
	if limit := query.Get("limit"); limit != "" {
		filter.Limit, err = strconv.Atoi(limit)
		if err != nil || filter.Limit <= 0 || filter.Limit > 500 {
			return filter, errors.New("invalid limit")
		}
	}
	for _, value := range query["status"] {
		for _, status := range strings.Split(value, ",") {
			if !validStatus(store.Status(status)) {
				return filter, errors.New("invalid status")
			}
			filter.Statuses = append(filter.Statuses, store.Status(status))
		}
	}
	if filter.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(filter.Cursor)
		if err != nil {
			return filter, err
		}
		var cursor struct{ CreatedAt, ID int64 }
		if err := json.Unmarshal(raw, &cursor); err != nil || cursor.ID <= 0 || cursor.CreatedAt < 0 {
			return filter, errors.New("invalid cursor")
		}
	}
	return filter, nil
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	filter, err := parseFilter(r.URL.Query())
	if err != nil {
		s.writeError(w, 400, "invalid_request", "invalid run filter or cursor")
		return
	}
	page, err := s.db.ListRuns(r.Context(), filter)
	if err != nil {
		s.respondError(w, err)
		return
	}
	for i := range page.Runs {
		page.Runs[i] = publicRun(page.Runs[i])
	}
	s.writeJSON(w, 200, page)
}

func (s *Server) runID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, 400, "invalid_request", "run ID must be a positive integer")
		return 0, false
	}
	if _, err := s.db.GetRun(r.Context(), id); err != nil {
		s.respondError(w, err)
		return 0, false
	}
	return id, true
}

func (s *Server) runDetail(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	id, ok := s.runID(w, r)
	if !ok {
		return
	}
	data, err := s.detailData(r.Context(), id)
	if err != nil {
		s.respondError(w, err)
		return
	}
	s.writeJSON(w, 200, data)
}

// Both the API and the unauthenticated loopback shell use this read model.
// Token-protected shells never load it; their browser must authenticate first.
func (s *Server) detailData(ctx context.Context, id int64) (map[string]any, error) {
	run, err := s.db.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	specs, err := s.db.ListSpecs(ctx, id)
	if err != nil {
		return nil, err
	}
	approvals, err := s.db.ListApprovals(ctx, id)
	if err != nil {
		return nil, err
	}
	steps, err := s.db.ListSteps(ctx, id)
	if err != nil {
		return nil, err
	}
	artifacts, err := s.db.ListArtifacts(ctx, id)
	if err != nil {
		return nil, err
	}
	reviews, err := s.db.ListReviews(ctx, id)
	if err != nil {
		return nil, err
	}
	publicArtifacts := []store.ArtifactMetadata{}
	for _, a := range artifacts {
		if publicArtifact(a.Kind) {
			publicArtifacts = append(publicArtifacts, a)
		}
	}
	for _, a := range testArtifacts(steps) {
		publicArtifacts = append(publicArtifacts, store.ArtifactMetadata{Kind: a.Kind, Attempt: a.Attempt, Bytes: int64(len(a.Content)), CreatedAt: a.CreatedAt})
	}
	// The detail endpoint returns step summaries, without transcripts.
	for i := range steps {
		steps[i].InputJSON = ""
		steps[i].OutputText = ""
	}
	var latest *store.Spec
	if len(specs) > 0 {
		latest = &specs[len(specs)-1]
	}
	questions, err := s.db.ListQuestions(ctx, id)
	if err != nil {
		return nil, err
	}
	escalation, err := s.runner.Escalation(ctx, run)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"run": publicRun(run), "capabilities": s.runner.ActionCapabilities(run, latest), "latest_spec": latest, "spec_versions": specs, "approvals": approvals, "steps": steps, "artifacts": publicArtifacts, "reviews": reviews,
		"questions": questions, "escalation": escalation}
	// An issue opened from the dashboard is authored by the bot or PAT user; the
	// requester is recorded here.
	if issue, ok, err := s.db.DashboardIssueByNumber(ctx, run.Repo, run.IssueNumber); err == nil && ok {
		data["dashboard_issue"] = map[string]any{"requested_by": issue.Actor, "created_at": issue.CreatedAt}
	}
	return data, nil
}

func (s *Server) editSpec(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodPut) {
		return
	}
	id, ok := s.runID(w, r)
	if !ok {
		return
	}
	var draft tools.SpecDraft
	if !s.decode(w, r, &draft, false) {
		return
	}
	spec, err := s.runner.EditSpec(r.Context(), id, draft)
	if err != nil {
		s.respondError(w, err)
		return
	}
	s.writeJSON(w, 200, map[string]any{"spec": spec})
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodPost) {
		return
	}
	id, ok := s.runID(w, r)
	if !ok {
		return
	}
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var err error
	status := http.StatusOK
	switch action {
	case "revise":
		var input struct {
			Notes string `json:"notes"`
		}
		if !s.decode(w, r, &input, false) {
			return
		}
		if strings.TrimSpace(input.Notes) == "" {
			s.writeError(w, 400, "invalid_request", "revision notes are required")
			return
		}
		err = s.runner.Revise(r.Context(), id, input.Notes)
		status = http.StatusAccepted
	case "retry":
		var input struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if !s.decode(w, r, &input, true) {
			return
		}
		if !validModels(input.Model) || (input.Provider != "" && s.cfg.Providers[input.Provider] == nil) {
			s.writeError(w, 400, "invalid_request", "invalid provider or model")
			return
		}
		var run store.Run
		run, err = s.runner.Retry(r.Context(), id, input.Provider, input.Model)
		if err != nil {
			s.respondError(w, err)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/api/runs/%d", run.ID))
		s.writeJSON(w, http.StatusAccepted, map[string]any{"run": publicRun(run)})
		return
	case "answer":
		var input struct {
			SpecVersion int64    `json:"spec_version"`
			SpecHash    string   `json:"spec_hash"`
			Answers     []string `json:"answers"`
		}
		if !s.decode(w, r, &input, false) {
			return
		}
		if input.SpecVersion <= 0 || input.SpecHash == "" || len(input.Answers) == 0 || len(input.Answers) > 50 {
			s.writeError(w, 400, "invalid_request", "spec_version, spec_hash and answers are required")
			return
		}
		err = s.runner.AnswerQuestions(r.Context(), id, s.actor(r), input.SpecVersion, input.SpecHash, input.Answers)
		status = http.StatusAccepted
	case "escalate":
		var input struct{}
		if !s.decode(w, r, &input, true) {
			return
		}
		var run store.Run
		run, err = s.runner.Escalate(r.Context(), id)
		if err != nil {
			s.respondError(w, err)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/api/runs/%d", run.ID))
		s.writeJSON(w, http.StatusAccepted, map[string]any{"run": publicRun(run)})
		return
	case "approve":
		var input struct {
			SpecVersion int64  `json:"spec_version"`
			SpecHash    string `json:"spec_hash"`
		}
		if !s.decode(w, r, &input, false) {
			return
		}
		if input.SpecVersion <= 0 || input.SpecHash == "" {
			s.writeError(w, 400, "invalid_request", "spec_version and spec_hash are required")
			return
		}
		err = s.runner.ApproveSpecAndExecute(r.Context(), id, s.actor(r), input.SpecVersion, input.SpecHash)
		status = http.StatusAccepted
	default:
		var input struct{}
		if !s.decode(w, r, &input, true) {
			return
		}
		switch action {
		case "reject":
			err = s.runner.Reject(r.Context(), id)
		case "cancel":
			err = s.runner.Cancel(r.Context(), id)
			if err == nil {
				s.manager.cancel(id)
			}
		}
	}
	if err != nil {
		s.respondError(w, err)
		return
	}
	run, err := s.db.GetRun(r.Context(), id)
	if err != nil {
		s.respondError(w, err)
		return
	}
	s.writeJSON(w, status, map[string]any{"run": publicRun(run)})
}

func publicArtifact(name string) bool {
	switch name {
	case "diff", "baseline", "test_output", "review", "pr_title", "pr_body", "escalation_note":
		return true
	default:
		return false
	}
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	id, ok := s.runID(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !publicArtifact(name) {
		s.writeError(w, 404, "not_found", "artifact not found")
		return
	}
	attempt := int64(0)
	if value := r.URL.Query().Get("attempt"); value != "" {
		var err error
		attempt, err = strconv.ParseInt(value, 10, 64)
		if err != nil || attempt < 0 {
			s.writeError(w, 400, "invalid_request", "invalid artifact attempt")
			return
		}
	}
	artifact, err := s.db.GetArtifact(r.Context(), id, name, attempt)
	if errors.Is(err, store.ErrNotFound) && name == "test_output" {
		steps, readErr := s.db.ListSteps(r.Context(), id)
		if readErr != nil {
			s.respondError(w, readErr)
			return
		}
		for _, a := range testArtifacts(steps) {
			if attempt == 0 || a.Attempt == attempt {
				artifact, err = a, nil
			}
		}
	}
	if err != nil {
		s.respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.safeText(artifact.Content)))
}

// Error details remain in local records. Public run responses expose a stable
// message rather than a transport or upstream diagnostic.
func publicRun(run store.Run) store.Run {
	if run.ErrorMessage != "" {
		run.ErrorMessage = "run stopped; inspect the local run records for details"
	}
	return run
}
