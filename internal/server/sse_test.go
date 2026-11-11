package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/testhttp"
)

type sseFrame struct {
	id                   int64
	event, data, comment string
}

func readFrame(reader *bufio.Reader) (sseFrame, error) {
	var frame sseFrame
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return frame, err
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return frame, nil
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			frame.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, ": "):
			frame.comment = strings.TrimPrefix(line, ": ")
		}
	}
}

func openStream(t *testing.T, httpServerURL string, client *http.Client, id int64, query, lastID string) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/runs/%d/events%s", httpServerURL, id, query), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", lastID)
	response, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("stream status %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	frame, err := readFrame(reader)
	if err != nil || frame.comment != "connected" {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("stream connect: %+v: %v", frame, err)
	}
	return response, reader, cancel
}

func nextEvent(t *testing.T, reader *bufio.Reader) sseFrame {
	t.Helper()
	for {
		frame, err := readFrame(reader)
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		if frame.id != 0 {
			if !json.Valid([]byte(frame.data)) {
				t.Fatalf("invalid event JSON: %s", frame.data)
			}
			return frame
		}
	}
}

func requireStreamEnd(t *testing.T, reader *bufio.Reader, lastSeq int64) {
	t.Helper()
	frame, err := readFrame(reader)
	want := fmt.Sprintf(`{"last_seq":%d}`, lastSeq)
	if err != nil || frame.event != "end" || frame.data != want || frame.id != 0 {
		t.Fatalf("end frame = %+v, %v; want last_seq %d", frame, err, lastSeq)
	}
	if _, err := readFrame(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal stream did not close after end: %v", err)
	}
}

func (f *fixture) appendEvents(id int64, count int) {
	f.t.Helper()
	for range count {
		if _, err := f.db.AppendEvent(f.t.Context(), store.Event{RunID: id, Type: "output", PayloadJSON: json.RawMessage(`{"text":"event"}`)}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestSSEReplayReconnectNoGapsOrDuplicates(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	f.appendEvents(run.ID, 4)
	httpServer := testhttp.NewServer(f.server)
	defer httpServer.Close()
	response, reader, cancel := openStream(t, httpServer.URL, httpServer.Client(), run.ID, "", "")
	for seq := int64(1); seq <= 4; seq++ {
		if frame := nextEvent(t, reader); frame.id != seq {
			t.Fatalf("replay seq %d, want %d", frame.id, seq)
		}
	}
	f.appendEvents(run.ID, 2)
	if frame := nextEvent(t, reader); frame.id != 5 {
		t.Fatalf("live seq %d", frame.id)
	}
	cancel()
	_ = response.Body.Close()
	// The sixth event may already have been written to the dead connection.
	// Reconnect from the last event actually consumed by this client.
	f.appendEvents(run.ID, 3)
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/cancel", run.ID), `{}`), 200)
	response, reader, cancel = openStream(t, httpServer.URL, httpServer.Client(), run.ID, "?after=1", "5")
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	expected, err := f.db.EventsAfter(t.Context(), run.ID, 5, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range expected {
		if frame := nextEvent(t, reader); frame.id != event.Seq {
			t.Fatalf("reconnected seq %d, want %d", frame.id, event.Seq)
		}
	}
	requireStreamEnd(t, reader, expected[len(expected)-1].Seq)
	if len(expected) != 5 {
		t.Fatalf("expected pending + new + terminal events: %d", len(expected))
	}
}

func TestSSEQueryReplayBatchesAndTerminal(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	f.appendEvents(run.ID, 1100)
	if err := f.db.SetRunStatus(t.Context(), run.ID, run.Status, store.StatusSucceeded, store.RunPatch{Phase: store.PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	httpServer := testhttp.NewServer(f.server)
	defer httpServer.Close()
	response, reader, cancel := openStream(t, httpServer.URL, httpServer.Client(), run.ID, "?after=7", "")
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	for seq := int64(8); seq <= 1101; seq++ {
		if frame := nextEvent(t, reader); frame.id != seq {
			t.Fatalf("seq %d, want %d", frame.id, seq)
		}
	}
	requireStreamEnd(t, reader, 1101)
}

func TestSSEHeartbeatAndPollingFallback(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	f.server.unsubscribe() // Simulate an unavailable in-process broadcaster.
	httpServer := testhttp.NewServer(f.server)
	defer httpServer.Close()
	response, reader, cancel := openStream(t, httpServer.URL, httpServer.Client(), run.ID, "", "")
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	frame, err := readFrame(reader)
	if err != nil || frame.comment != "heartbeat" {
		t.Fatalf("heartbeat: %+v: %v", frame, err)
	}
	f.appendEvents(run.ID, 1)
	if frame := nextEvent(t, reader); frame.id != 1 {
		t.Fatal("polling did not recover missed notification")
	}
	// Closing the browser leaves the durable run waiting.
	cancel()
	_ = response.Body.Close()
	current, err := f.db.GetRun(t.Context(), run.ID)
	if err != nil || current.Status != store.StatusAwaitingApproval {
		t.Fatal("browser disconnect cancelled run")
	}
}

func TestSlowSSEClientNeverBlocksEventWrites(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	httpServer := testhttp.NewServer(f.server)
	defer httpServer.Close()
	response, _, cancel := openStream(t, httpServer.URL, httpServer.Client(), run.ID, "", "")
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	payload, _ := json.Marshal(strings.Repeat("x", 128<<10))
	start := time.Now()
	for range 100 {
		if _, err := f.db.AppendEvent(t.Context(), store.Event{RunID: run.ID, Type: "output", PayloadJSON: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("slow client blocked event writes")
	}
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		f.server.bus.mu.Lock()
		count := len(f.server.bus.clients[run.ID])
		f.server.bus.mu.Unlock()
		if count == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("lagging stream was not disconnected")
		case <-tick.C:
		}
	}
	events, err := f.db.EventsAfter(t.Context(), run.ID, 0, 500)
	if err != nil || len(events) != 100 {
		t.Fatalf("event writes lost: %d: %v", len(events), err)
	}
}

func TestBroadcasterDisconnectsLaggingSubscriber(t *testing.T) {
	var bus broadcaster
	sub, stop := bus.subscribe(1)
	defer stop()
	for range 100 {
		bus.publish(1)
	}
	select {
	case <-sub.lagged:
	default:
		t.Fatal("bounded subscriber was not dropped")
	}
}

func TestTerminalSSEDrainsEventsFromFinishingWorker(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	release := make(chan struct{})
	started := make(chan struct{})
	if err := f.server.manager.submit(t.Context(), run.ID, func(ctx context.Context) error {
		close(started)
		<-release
		_, err := f.db.AppendEvent(ctx, store.Event{RunID: run.ID, Type: "cleanup", PayloadJSON: json.RawMessage(`{}`)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/reject", run.ID), `{}`), 200)
	httpServer := testhttp.NewServer(f.server)
	defer httpServer.Close()
	response, reader, cancel := openStream(t, httpServer.URL, httpServer.Client(), run.ID, "", "")
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	if frame := nextEvent(t, reader); frame.id != 1 {
		t.Fatal("missing terminal event")
	}
	if frame, err := readFrame(reader); err != nil || frame.comment != "heartbeat" {
		t.Fatalf("ended before the finishing worker drained: %+v, %v", frame, err)
	}
	close(release)
	if frame := nextEvent(t, reader); frame.id != 2 || frame.event != "cleanup" {
		t.Fatalf("lost finishing event: %+v", frame)
	}
	requireStreamEnd(t, reader, 2)
}

type replayStore struct {
	store.Store
	read func(context.Context, int64, int64, int) ([]store.Event, error)
}

func (s replayStore) EventsAfter(ctx context.Context, id, after int64, limit int) ([]store.Event, error) {
	return s.read(ctx, id, after, limit)
}

// Exercise failures even after terminal status has been observed: an empty or
// failed replay must never turn a disconnect into false completion evidence.
func TestSSEEndNotSentOnReplayFailureOrLag(t *testing.T) {
	for _, failure := range []string{"replay_first", "replay_mid_batch", "replay_after_terminal", "lag_mid_batch", "lag_after_terminal"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t, nil, nil)
			run := f.seed(store.StatusFailed)
			if strings.HasSuffix(failure, "mid_batch") {
				f.appendEvents(run.ID, 600)
			}
			// A separate handler shares the real store but owns its own stream
			// broadcaster; no background goroutine sees the injected store.
			s := &Server{opts: f.server.opts, redactor: f.server.redactor, manager: f.server.manager, done: f.server.done}
			calls := 0
			s.db = replayStore{Store: f.db, read: func(ctx context.Context, id, after int64, limit int) ([]store.Event, error) {
				calls++
				if (failure == "replay_first" && calls == 1) || (failure == "replay_mid_batch" && calls == 2) || (failure == "replay_after_terminal" && calls == 3) {
					return nil, errors.New("injected replay failure")
				}
				if (failure == "lag_mid_batch" && calls == 2) || (failure == "lag_after_terminal" && calls == 3) {
					for range 40 {
						s.bus.publish(id)
					}
				}
				return f.db.EventsAfter(ctx, id, after, limit)
			}}
			r := httptest.NewRequest("GET", "/events", nil)
			r.SetPathValue("id", strconv.FormatInt(run.ID, 10))
			w := httptest.NewRecorder()
			s.events(w, r)
			if strings.Contains(w.Body.String(), "event: end\n") {
				t.Fatalf("failure emitted end: %s", w.Body.String())
			}
			if strings.Contains(failure, "mid_batch") && !strings.Contains(w.Body.String(), "id: 500\n") {
				t.Fatal("test did not deliver the first batch before failure")
			}
		})
	}
}

func TestSSEEndOnAlreadyDrainedTerminalReplay(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusFailed)
	response := f.request("GET", fmt.Sprintf("/api/runs/%d/events?after=1", run.ID), "")
	requireStatus(t, response, 200)
	reader := bufio.NewReader(response.Body)
	if _, err := readFrame(reader); err != nil {
		t.Fatal(err)
	}
	requireStreamEnd(t, reader, 1)
}

func TestSSEEndDrainsEventCommittedWithTerminalStatus(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	s := &Server{opts: f.server.opts, redactor: f.server.redactor, manager: f.server.manager, done: f.server.done}
	first := true
	s.db = replayStore{Store: f.db, read: func(ctx context.Context, id, after int64, limit int) ([]store.Event, error) {
		events, err := f.db.EventsAfter(ctx, id, after, limit)
		if first {
			first = false
			// Commit the terminal status and its event after the empty replay
			// read, before the handler observes the new status through GetRun.
			if transitionErr := f.db.SetRunStatus(ctx, id, run.Status, store.StatusFailed, store.RunPatch{Phase: store.PhaseRefine}); transitionErr != nil {
				t.Fatal(transitionErr)
			}
		}
		return events, err
	}}
	r := httptest.NewRequest("GET", "/events", nil)
	r.SetPathValue("id", strconv.FormatInt(run.ID, 10))
	w := httptest.NewRecorder()
	s.events(w, r)
	reader := bufio.NewReader(w.Body)
	if _, err := readFrame(reader); err != nil {
		t.Fatal(err)
	}
	if frame := nextEvent(t, reader); frame.id != 1 || frame.event != "run.status" {
		t.Fatalf("lost terminal transaction event: %+v", frame)
	}
	requireStreamEnd(t, reader, 1)
}
