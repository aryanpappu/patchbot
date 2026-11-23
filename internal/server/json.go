package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

const maxBodyBytes = 1 << 20

func (s *Server) decode(w http.ResponseWriter, r *http.Request, value any, optional bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	// Reject JSON null, which encoding/json otherwise accepts for a struct.
	var raw json.RawMessage
	err := decoder.Decode(&raw)
	if optional && errors.Is(err, io.EOF) {
		return true
	}
	if err == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		err = errors.New("null body")
	}
	if err == nil {
		strict := json.NewDecoder(bytes.NewReader(raw))
		strict.DisallowUnknownFields()
		err = strict.Decode(value)
	}
	if err == nil {
		var extra any
		if tailErr := decoder.Decode(&extra); tailErr != io.EOF {
			if tailErr != nil {
				err = tailErr
			} else {
				err = errors.New("multiple JSON values")
			}
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		s.writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 1 MiB")
	} else {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request")
	}
	return false
}

func (s *Server) writeError(w http.ResponseWriter, status int, kind, message string) {
	s.writeJSON(w, status, map[string]any{"error": map[string]string{"kind": kind, "message": message}})
}

func (s *Server) respondError(w http.ResponseWriter, err error) {
	status, kind, message := errorEnvelope(err)
	s.writeError(w, status, kind, message)
}

// errorEnvelope maps an internal error to a client-safe status, kind and message.
func errorEnvelope(err error) (int, string, string) {
	switch {
	case errors.Is(err, errQueueFull):
		return http.StatusTooManyRequests, "queue_full", errQueueFull.Error()
	case errors.Is(err, runner.ErrInvalidRequest):
		return http.StatusBadRequest, "invalid_request", "invalid specification content or paths"
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not_found", "run or resource not found"
	case errors.Is(err, store.ErrActiveRunExists):
		return http.StatusConflict, "active_run_exists", "an active run already exists for this issue"
	case errors.Is(err, runner.ErrInvalidAnswers):
		return http.StatusBadRequest, "invalid_request", err.Error()
	case errors.Is(err, runner.ErrNotOnLadder):
		return http.StatusConflict, "conflict", runner.ErrNotOnLadder.Error()
	case errors.Is(err, runner.ErrSpecChanged):
		return http.StatusConflict, "spec_changed", runner.ErrSpecChanged.Error()
	case errors.Is(err, store.ErrConflict), errors.Is(err, agent.ErrApprovalDenied):
		return http.StatusConflict, "conflict", "action is not valid for the current run or specification"
	case errors.Is(err, runner.ErrGitHubNotConnected):
		return http.StatusConflict, "github_not_connected", err.Error()
	case errors.Is(err, errShuttingDown):
		return http.StatusServiceUnavailable, "unavailable", "server is shutting down"
	case errors.Is(err, llm.ErrUnknownProvider):
		return http.StatusBadRequest, "invalid_request", "unknown provider"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout, "timeout", "request timed out"
	default:
		return http.StatusInternalServerError, "internal", "could not complete request"
	}
}

func (s *Server) safeText(value string) string { return s.redactor.Redact(value) }

func (s *Server) safeJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return nil, err
	}
	return json.Marshal(s.redactValue(tree))
}

func (s *Server) redactValue(value any) any {
	switch value := value.(type) {
	case string:
		return s.safeText(value)
	case []any:
		for i, item := range value {
			value[i] = s.redactValue(item)
		}
		return value
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			text, isString := item.(string)
			if isString && store.IsSchemaIdentifier(key, text) {
				out[key] = item
			} else {
				out[key] = s.redactValue(item)
			}
		}
		return out
	default:
		return value
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	raw, err := s.safeJSON(value)
	if err != nil {
		raw = []byte(`{"error":{"kind":"internal","message":"could not encode response"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	//nolint:gosec // G705: json.Marshal escapes HTML; the content type is JSON with nosniff.
	_, _ = w.Write(append(raw, '\n'))
}
