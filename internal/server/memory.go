package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/aryanpappu/patchbot/internal/memory"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// Memory routes share the server's bearer auth, host checks and cross-origin
// mutation protection. Decisions use the verified Access email (else approval.local_actor) on the ui channel.
func (s *Server) memoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/memory", s.memoryList)
	for _, action := range []string{store.MemoryApprove, store.MemoryReject, store.MemoryForget} {
		mux.HandleFunc("/api/memory/{id}/"+action, s.memoryDecide)
	}
}

var memoryStatuses = []string{store.MemoryActive, store.MemoryPending, store.MemoryApproved, store.MemoryRejected, store.MemoryRevoked}

func (s *Server) memoryList(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodPost {
		s.memoryAdd(w, r)
		return
	}
	query := r.URL.Query()
	// Only allowlisted repositories are listed; others cannot be decided either.
	filter := store.MemoryFilter{Repo: strings.ToLower(strings.TrimSpace(query.Get("repo"))), Repos: s.allowlistedRepos(), Kind: query.Get("kind"), Limit: 500}
	if filter.Repo != "" && s.cfg.Repos[filter.Repo] == nil {
		s.writeError(w, 400, "invalid_request", "require an allowlisted repo")
		return
	}
	if filter.Kind != "" && filter.Kind != store.MemoryObservation && filter.Kind != store.MemoryLesson {
		s.writeError(w, 400, "invalid_request", "invalid memory kind")
		return
	}
	for _, value := range query["status"] {
		for _, status := range strings.Split(value, ",") {
			if !slices.Contains(memoryStatuses, status) {
				s.writeError(w, 400, "invalid_request", "invalid memory status")
				return
			}
			filter.Statuses = append(filter.Statuses, status)
		}
	}
	entries, err := s.db.ListMemory(r.Context(), filter)
	if err != nil {
		s.respondError(w, err)
		return
	}
	values := make([]view, 0, len(entries))
	for _, e := range entries {
		values = append(values, memoryView(e))
	}
	s.writeJSON(w, 200, map[string]any{"enabled": s.cfg.Memory.Enabled, "entries": values})
}

type view struct {
	store.MemoryEntry
	Summary string `json:"summary"`
}

// memoryView escapes any non-printable rune so stored text cannot hide
// content from the reviewing operator, including legacy or imported rows.
func memoryView(e store.MemoryEntry) view {
	e.Text = tools.Visible(e.Text)
	summary := e.Text
	if e.Kind == store.MemoryObservation {
		summary = tools.Visible(memory.RenderObservation(e))
	}
	return view{e, summary}
}

func (s *Server) allowlistedRepos() []string {
	repos := make([]string, 0, len(s.cfg.Repos))
	for name := range s.cfg.Repos {
		repos = append(repos, strings.ToLower(name))
	}
	return repos
}

func (s *Server) memoryAdd(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo string `json:"repo"`
		Text string `json:"text"`
	}
	if !s.decode(w, r, &input, false) {
		return
	}
	repo := strings.ToLower(strings.TrimSpace(input.Repo))
	if s.cfg.Repos[repo] == nil {
		s.writeError(w, 400, "invalid_request", "require an allowlisted repo")
		return
	}
	entry, err := memory.AddLesson(r.Context(), s.db, s.redactor, memory.LessonInput{Repo: repo, Text: input.Text, Source: store.MemorySourceOperator, Actor: s.actor(r), Channel: "ui"})
	if errors.Is(err, tools.ErrInvalidLesson) {
		s.writeError(w, 400, "invalid_request", tools.ErrInvalidLesson.Error())
		return
	}
	if errors.Is(err, store.ErrDuplicateMemory) {
		s.writeError(w, 409, "conflict", "this lesson already exists for the repository")
		return
	}
	if err != nil {
		s.respondError(w, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/memory?repo=%s", repo))
	s.writeJSON(w, http.StatusCreated, map[string]any{"entry": memoryView(entry)})
}

func (s *Server) memoryDecide(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodPost) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, 400, "invalid_request", "memory ID must be a positive integer")
		return
	}
	// content_hash is the hash the operator was shown: required to approve,
	// checked when given for reject/forget.
	var input struct {
		ContentHash string `json:"content_hash"`
	}
	if !s.decode(w, r, &input, true) {
		return
	}
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	entry, err := memory.Decide(r.Context(), s.db, memory.DecideInput{ID: id, Action: action, Actor: s.actor(r), Channel: "ui", HashPrefix: input.ContentHash,
		Allowed: func(repo string) bool { return s.cfg.Repos[repo] != nil }})
	switch {
	case errors.Is(err, memory.ErrHashRequired):
		s.writeError(w, 400, "invalid_request", memory.ErrHashRequired.Error())
		return
	case errors.Is(err, memory.ErrNotAllowlisted):
		s.writeError(w, 403, "forbidden", memory.ErrNotAllowlisted.Error())
		return
	case errors.Is(err, memory.ErrHashMismatch):
		s.writeError(w, 409, "conflict", "content hash does not match; reload and review the entry")
		return
	case err != nil:
		s.respondError(w, err)
		return
	}
	s.writeJSON(w, 200, map[string]any{"entry": memoryView(entry)})
}
