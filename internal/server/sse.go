package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aryanpappu/patchbot/internal/store"
)

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.method(w, r, http.MethodGet) {
		return
	}
	id, ok := s.runID(w, r)
	if !ok {
		return
	}
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("after")
	}
	after := int64(0)
	if value != "" {
		var err error
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after < 0 {
			s.writeError(w, 400, "invalid_request", "invalid event sequence")
			return
		}
	}
	controller := http.NewResponseController(w)
	// Drop the request's read deadline once its headers/body have been read.
	// The connection remains open for a stream longer than ReadTimeout.
	_ = controller.SetReadDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	sub, unsubscribe := s.bus.subscribe(id)
	defer unsubscribe()
	heartbeat := time.NewTicker(s.opts.HeartbeatInterval)
	defer heartbeat.Stop()
	poll := time.NewTicker(s.opts.PollInterval)
	defer poll.Stop()
	write := func(frame string) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(s.opts.StreamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := fmt.Fprint(w, frame); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !write(": connected\n\n") {
		return
	}
	terminal := false
	for {
		select {
		case <-sub.lagged:
			return
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		default:
		}
		events, err := s.db.EventsAfter(r.Context(), id, after, 500)
		if err != nil {
			return
		}
		for _, event := range events {
			select {
			case <-heartbeat.C:
				if !write(": heartbeat\n\n") {
					return
				}
			default:
			}
			select {
			case <-sub.lagged:
				return
			case <-s.done:
				return
			case <-r.Context().Done():
				return
			default:
			}
			payload, err := s.safeJSON(event.PayloadJSON)
			if err != nil {
				return
			}
			if !write(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", event.Seq, eventName(s.safeText(event.Type)), payload)) {
				return
			}
			after = event.Seq
		}
		if len(events) > 0 {
			continue
		}
		// Read again after observing a terminal state: its transaction could have
		// committed between EventsAfter and GetRun, including the final event.
		if terminal {
			return
		}
		run, err := s.db.GetRun(r.Context(), id)
		if err != nil {
			return
		}
		if !run.Status.IsActive() && !s.manager.busy(id) {
			terminal = true
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-sub.lagged:
			return
		case <-sub.wake:
		case <-poll.C:
		case <-heartbeat.C:
			if !write(": heartbeat\n\n") {
				return
			}
		}
	}
}

func eventName(name string) string {
	if name == "" || strings.IndexFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '_' && r != '-'
	}) >= 0 {
		return "message"
	}
	return name
}

// Test output already passes the runner's persistence redactor. Present the
// recorded harness attempts as downloadable artifacts without copying them.
func testArtifacts(steps []store.Step) []store.Artifact {
	artifacts := []store.Artifact{}
	for _, step := range steps {
		if step.Kind != "test" || step.Phase != store.PhaseVerify || step.Status == "running" {
			continue
		}
		artifacts = append(artifacts, store.Artifact{RunID: step.RunID, Kind: "test_output", Attempt: int64(len(artifacts) + 1), Content: step.OutputText, CreatedAt: step.StartedAt})
	}
	return artifacts
}
