package server

import (
	"bytes"
	"net/http"
	"strconv"

	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/web"
)

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	state := web.State{Page: "runs", AuthRequired: s.authenticated}
	if r.URL.Path == "/runs/new" {
		state.Page = "new"
	} else if value := r.PathValue("id"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			s.writeError(w, http.StatusNotFound, "not_found", "page not found")
			return
		}
		state.Page, state.RunID = "detail", id
		if !s.authenticated {
			data, err := s.detailData(r.Context(), id)
			if err != nil {
				s.respondError(w, err)
				return
			}
			initial, err := s.safeJSON(data)
			if err != nil {
				s.writeError(w, http.StatusInternalServerError, "internal", "could not encode page state")
				return
			}
			state.Initial = initial
			state.Preview = s.safeText(data["run"].(store.Run).IssueTitle)
			if spec, ok := data["latest_spec"].(*store.Spec); ok && spec != nil {
				state.Preview += "\n\n" + s.safeText(spec.BodyMD)
			}
		}
	}
	var body bytes.Buffer
	if err := web.Render(&body, s.cfg.Server.VueSrc == "local", state); err != nil {
		s.writeError(w, http.StatusInternalServerError, "internal", "could not render page")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	if !web.PublicPath(r.URL.Path) {
		s.writeError(w, http.StatusNotFound, "not_found", "asset not found")
		return
	}
	web.Asset(w, r, s.cfg.Server.VueSrc == "local")
}
