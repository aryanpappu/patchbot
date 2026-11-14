package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/aryanpappu/patchbot/internal/cfaccess"
	"github.com/aryanpappu/patchbot/internal/runner"
)

type emailKey struct{}

// accessPublic lists the only paths served without a token: fixed, data-free
// static assets. Pages are not public because the signed-in email is shown.
func accessPublic(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/assets/app.js", "/assets/app.css", "/assets/vue.global.prod.js":
		return true
	}
	return false
}

// accessAuth verifies the Cloudflare Access JWT. On success it returns a
// request whose context carries the email as the action actor. The token is
// never logged and failures reveal nothing about why they failed.
func (s *Server) accessAuth(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if accessPublic(r) {
		return r, true
	}
	email, err := s.access.Verify(r.Context(), r.Header.Get(cfaccess.Header))
	if err == nil {
		ctx := runner.WithActor(context.WithValue(r.Context(), emailKey{}, email), email)
		return r.WithContext(ctx), true
	}
	status, code, message := http.StatusUnauthorized, "unauthorized", "sign in through Cloudflare Access"
	if errors.Is(err, cfaccess.ErrNotAllowed) {
		status, code, message = http.StatusForbidden, "forbidden", "this account is not allowed"
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(http.StatusText(status) + "\n"))
		return r, false
	}
	s.writeError(w, status, code, message)
	return r, false
}

// sessionEmail is the verified login email, or empty outside Access mode.
func sessionEmail(r *http.Request) string {
	email, _ := r.Context().Value(emailKey{}).(string)
	return email
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"email": sessionEmail(r), "mode": s.cfg.Server.Auth.Mode})
}

// actor names who performed a UI/API action: the verified login email in
// Access mode, otherwise approval.local_actor.
func (s *Server) actor(r *http.Request) string {
	if email := sessionEmail(r); email != "" {
		return email
	}
	return s.cfg.Approval.LocalActor
}
