package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/store"
)

// approveBody binds an approval to the latest stored spec, as the dashboard does.
func (f *fixture) approveBody(t *testing.T, id int64) string {
	t.Helper()
	spec, err := f.db.LatestSpec(t.Context(), id)
	if err != nil {
		return `{"spec_version":1,"spec_hash":"none"}`
	}
	raw, err := json.Marshal(map[string]any{"spec_version": spec.Version, "spec_hash": spec.Hash})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestApproveIsBoundToShownSpec(t *testing.T) {
	f := newFixture(t, fake.New("anthropic", specStep(), writeStep(), submitStep()), nil)
	f.withRepo()
	w := f.request("POST", "/api/runs", `{"repo":"owner/repo","issue":12,"mode":"refine","dry_run":true}`)
	requireStatus(t, w, 202)
	run := f.responseRun(w)
	f.waitFor(run.ID, store.StatusAwaitingApproval)
	path := fmt.Sprintf("/api/runs/%d/approve", run.ID)
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	approvals := func() int {
		rows, err := f.db.ListApprovals(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	for name, body := range map[string]string{
		"empty body":      `{}`,
		"missing hash":    fmt.Sprintf(`{"spec_version":%d}`, spec.Version),
		"missing version": fmt.Sprintf(`{"spec_hash":%q}`, spec.Hash),
		"zero version":    fmt.Sprintf(`{"spec_version":0,"spec_hash":%q}`, spec.Hash),
	} {
		requireStatus(t, f.request("POST", path, body), 400)
		if approvals() != 0 {
			t.Fatalf("%s recorded an approval", name)
		}
	}
	for name, body := range map[string]string{
		"stale version": fmt.Sprintf(`{"spec_version":%d,"spec_hash":%q}`, spec.Version+1, spec.Hash),
		"stale hash":    fmt.Sprintf(`{"spec_version":%d,"spec_hash":"deadbeef"}`, spec.Version),
	} {
		w := f.request("POST", path, body)
		requireStatus(t, w, 409)
		var failure struct {
			Error struct{ Kind, Message string }
		}
		_ = json.Unmarshal(w.Body.Bytes(), &failure)
		if failure.Error.Kind != "spec_changed" {
			t.Fatalf("%s: code %q", name, failure.Error.Kind)
		}
		if approvals() != 0 {
			t.Fatalf("%s recorded an approval", name)
		}
	}
	if got, err := f.db.GetRun(t.Context(), run.ID); err != nil || got.Status != store.StatusAwaitingApproval {
		t.Fatalf("status moved: %v %v", got.Status, err)
	}
	requireStatus(t, f.request("POST", path, f.approveBody(t, run.ID)), 202)
	if approvals() != 1 {
		t.Fatal("matching approval was not recorded")
	}
}
