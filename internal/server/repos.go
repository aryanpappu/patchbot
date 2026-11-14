package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/aryanpappu/patchbot/internal/repos"
	"github.com/aryanpappu/patchbot/internal/store"
)

// Repository management routes share the server's authentication (Access JWT
// or bearer token), host check, cross-origin protection and 1 MiB body limit.
// The actor is the verified Access email (else approval.local_actor), channel ui.
// Config-file repositories are listed but read-only.
func (s *Server) repoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/repos", s.reposCollection)
	mux.HandleFunc("/api/repos/{owner}/{name}", s.repoItem)
	mux.HandleFunc("/api/repos/{owner}/{name}/audit", s.repoAudit)
}

type repoBody struct {
	Name     string         `json:"name"`
	Version  int64          `json:"version"`
	Settings repos.Settings `json:"settings"`
}

func (s *Server) reposCollection(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		s.writeJSON(w, http.StatusOK, map[string]any{"repos": s.repos.List(), "container": s.cfg.Executor.Container()})
		return
	}
	var input repoBody
	if !s.decode(w, r, &input, false) {
		return
	}
	entry, err := s.repos.Add(r.Context(), input.Name, input.Settings, s.actor(r), "ui")
	if err != nil {
		s.repoError(w, err)
		return
	}
	w.Header().Set("Location", "/api/repos/"+entry.Name)
	s.writeJSON(w, http.StatusCreated, map[string]any{"repo": entry, "github": s.runner.CheckRepo(r.Context(), entry.Name)})
}

func (s *Server) repoItem(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet, http.MethodPut, http.MethodDelete) {
		return
	}
	name := r.PathValue("owner") + "/" + r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		entry, ok := s.repos.Get(name)
		if !ok {
			s.writeError(w, http.StatusNotFound, "not_found", "repository not found")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"repo": entry})
	case http.MethodPut:
		var input repoBody
		if !s.decode(w, r, &input, false) {
			return
		}
		version, ok := ifMatch(r, input.Version)
		if !ok {
			s.writeError(w, http.StatusPreconditionRequired, "version_required", "send the version you edited (If-Match header or version field)")
			return
		}
		entry, err := s.repos.Update(r.Context(), name, input.Settings, version, s.actor(r), "ui")
		if err != nil {
			s.repoError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"repo": entry})
	default:
		version, _ := ifMatch(r, 0)
		if err := s.repos.Remove(r.Context(), name, version, s.actor(r), "ui"); err != nil {
			s.repoError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"removed": repos.Normalize(name)})
	}
}

// ifMatch reads the expected version from the If-Match header (an integer,
// optionally quoted) or, failing that, from fallback. ok is false when neither
// is a positive version.
func ifMatch(r *http.Request, fallback int64) (int64, bool) {
	if h := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); h != "" {
		v, err := strconv.ParseInt(h, 10, 64)
		return v, err == nil && v > 0
	}
	return fallback, fallback > 0
}

func (s *Server) repoAudit(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	name := r.PathValue("owner") + "/" + r.PathValue("name")
	rows, err := s.repos.Audit(r.Context(), name)
	if err != nil {
		s.respondError(w, err)
		return
	}
	if len(rows) == 0 && !s.repos.Has(name) {
		s.writeError(w, http.StatusNotFound, "not_found", "repository not found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"audit": rows})
}

func (s *Server) repoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repos.ErrInvalid):
		var ve *repos.ValidationError
		if errors.As(err, &ve) && ve.TooLarge() {
			s.writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
			return
		}
		s.writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, repos.ErrCollision):
		s.writeError(w, http.StatusConflict, "name_in_config", err.Error())
	case errors.Is(err, repos.ErrReadOnly):
		s.writeError(w, http.StatusForbidden, "read_only", err.Error())
	case errors.Is(err, store.ErrRepoExists):
		s.writeError(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, store.ErrVersionConflict):
		s.writeError(w, http.StatusConflict, "version_conflict", "the repository changed since you loaded it; reload and retry")
	case errors.Is(err, store.ErrRepoBusy):
		s.writeError(w, http.StatusConflict, "repo_busy", "the repository has active runs; finish or cancel them first")
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "not_found", "repository not found")
	default:
		s.respondError(w, err)
	}
}
