package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

func (f *fixture) capabilities(id int64) runner.Capabilities {
	f.t.Helper()
	w := f.request("GET", fmt.Sprintf("/api/runs/%d", id), "")
	requireStatus(f.t, w, 200)
	var detail struct {
		Capabilities *runner.Capabilities `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.Capabilities == nil {
		f.t.Fatalf("missing capabilities: %s, %v", w.Body.String(), err)
	}
	return *detail.Capabilities
}

func TestCapabilitiesMatchAcceptedAPIActions(t *testing.T) {
	decision := runner.Capabilities{CanApprove: true, CanEdit: true, CanRevise: true, CanReject: true, CanCancel: true}
	editable := runner.Capabilities{CanEdit: true, CanRevise: true, CanReject: true, CanCancel: true}
	cancel := runner.Capabilities{CanCancel: true}
	retry := runner.Capabilities{CanRetry: true}
	for _, tc := range []struct {
		name      string
		status    store.Status
		versions  int
		limit     int
		expired   bool
		noSpec    bool
		questions bool
		badHash   bool
		want      runner.Capabilities
	}{
		{name: "queued", status: store.StatusQueued, want: cancel},
		{name: "running", status: store.StatusRunning, want: cancel},
		{name: "awaiting", status: store.StatusAwaitingApproval, want: decision},
		{name: "needs_info", status: store.StatusNeedsInfo, want: editable},
		{name: "approved", status: store.StatusApproved, want: editable},
		{name: "succeeded", status: store.StatusSucceeded},
		{name: "failed", status: store.StatusFailed, want: retry},
		{name: "rejected", status: store.StatusRejected, want: retry},
		{name: "cancelled_idempotent", status: store.StatusCancelled, want: runner.Capabilities{CanCancel: true, CanRetry: true}},
		{name: "last_revision_available", status: store.StatusAwaitingApproval, versions: 5, want: decision},
		{name: "default_revision_cap", status: store.StatusAwaitingApproval, versions: 6, want: runner.Capabilities{CanApprove: true, CanReject: true, CanCancel: true}},
		{name: "past_revision_cap", status: store.StatusAwaitingApproval, versions: 7, want: runner.Capabilities{CanReject: true, CanCancel: true}},
		{name: "configured_revision_cap", status: store.StatusAwaitingApproval, versions: 3, limit: 2, want: runner.Capabilities{CanApprove: true, CanReject: true, CanCancel: true}},
		{name: "expired", status: store.StatusAwaitingApproval, expired: true, want: cancel},
		{name: "no_spec", status: store.StatusAwaitingApproval, noSpec: true, want: cancel},
		{name: "open_questions", status: store.StatusAwaitingApproval, questions: true, want: editable},
		{name: "invalid_hash", status: store.StatusAwaitingApproval, badHash: true, want: editable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, func(opts *Options) {
				if tc.expired {
					opts.Runner.Clock = func() time.Time { return time.Now().Add(15 * 24 * time.Hour) }
				}
			})
			if tc.limit != 0 {
				f.cfg.Approval.MaxRevisions = tc.limit
			}
			// Keep dispatched work queued so acceptance can be compared without
			// depending on a provider, clone, or subsequent worker transitions.
			release := holdWorkers(t, f.server.manager)
			defer release()
			for _, action := range []string{"approve", "edit", "revise", "reject", "cancel", "retry"} {
				t.Run(action, func(t *testing.T) {
					run := f.seed(store.StatusAwaitingApproval)
					if tc.noSpec {
						var err error
						run, err = f.db.CreateRun(t.Context(), store.Run{Repo: "owner/repo", IssueNumber: time.Now().UnixNano(), Status: tc.status})
						if err != nil {
							t.Fatal(err)
						}
					} else {
						versions := max(1, tc.versions)
						if tc.questions || tc.badHash {
							versions++
						}
						for version := 2; version <= versions; version++ {
							d := draft()
							if tc.questions {
								d.OpenQuestions = []string{"Which behavior?"}
							}
							hash := d.Hash()
							if tc.badHash {
								hash = strings.Repeat("0", 64)
							}
							prior, err := f.db.LatestSpec(t.Context(), run.ID)
							if err != nil {
								t.Fatal(err)
							}
							if _, err := f.db.SaveSpec(t.Context(), store.Spec{RunID: run.ID, BodyMD: d.Markdown(), Hash: hash, IssueHash: prior.IssueHash, Source: "agent"}); err != nil {
								t.Fatal(err)
							}
						}
						if tc.status != run.Status {
							if err := f.db.SetRunStatus(t.Context(), run.ID, run.Status, tc.status, store.RunPatch{Phase: store.PhaseRefine}); err != nil {
								t.Fatal(err)
							}
						}
					}
					capabilities := f.capabilities(run.ID)
					if capabilities != tc.want {
						t.Fatalf("capabilities = %+v, want %+v", capabilities, tc.want)
					}
					allowed := map[string]bool{"approve": capabilities.CanApprove, "edit": capabilities.CanEdit, "revise": capabilities.CanRevise, "reject": capabilities.CanReject, "cancel": capabilities.CanCancel, "retry": capabilities.CanRetry}[action]
					path := fmt.Sprintf("/api/runs/%d", run.ID)
					method, body, endpoint := "POST", `{}`, path+"/"+action
					switch action {
					case "edit":
						raw, err := json.Marshal(draft())
						if err != nil {
							t.Fatal(err)
						}
						method, body, endpoint = "PUT", string(raw), path+"/spec"
					case "revise":
						body = `{"notes":"clarify the result"}`
					}
					w := f.request(method, endpoint, body)
					accepted := w.Code >= 200 && w.Code < 300
					if accepted != allowed {
						t.Fatalf("can_%s = %v, API status %d: %s", action, allowed, w.Code, w.Body.String())
					}
					if accepted && action == "retry" {
						newRun := f.responseRun(w)
						requireStatus(t, f.request("POST", fmt.Sprintf("/api/runs/%d/cancel", newRun.ID), `{}`), 200)
					}
					current, err := f.db.GetRun(t.Context(), run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if current.Status.IsActive() {
						requireStatus(t, f.request("POST", path+"/cancel", `{}`), 200)
					}
				})
			}
		})
	}
}

func TestCapabilitiesRefreshAfterCancelAndLastEdit(t *testing.T) {
	f := newFixture(t, nil, nil)
	run := f.seed(store.StatusAwaitingApproval)
	path := fmt.Sprintf("/api/runs/%d", run.ID)
	if !f.capabilities(run.ID).CanEdit {
		t.Fatal("initial spec should be editable")
	}
	for range 5 {
		raw, err := json.Marshal(draft())
		if err != nil {
			t.Fatal(err)
		}
		requireStatus(t, f.request("PUT", path+"/spec", string(raw)), 200)
	}
	if c := f.capabilities(run.ID); c.CanEdit || c.CanRevise || !c.CanApprove {
		t.Fatalf("last edit did not exhaust revisions: %+v", c)
	}
	requireStatus(t, f.request("POST", path+"/cancel", `{}`), 200)
	if c := f.capabilities(run.ID); c.CanEdit || c.CanRevise || c.CanApprove || c.CanReject || !c.CanRetry {
		t.Fatalf("cancelled capabilities: %+v", c)
	}
}
