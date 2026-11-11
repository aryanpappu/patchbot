package store

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func approvedSpec(t *testing.T, s *SQLite) (Run, Spec) {
	t.Helper()
	run := mustRun(t, s, Run{Status: StatusAwaitingApproval, Phase: PhaseRefine})
	spec, err := s.SaveSpec(t.Context(), Spec{RunID: run.ID, BodyMD: "v1", Hash: hashOf("v1"), Source: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DecideRun(t.Context(), run.Status, StatusApproved, Approval{RunID: run.ID, SpecVersion: spec.Version,
		SpecHash: spec.Hash, Decision: "approve", Actor: "owner", Channel: "cli"}, run.Generation); err != nil {
		t.Fatal(err)
	}
	return mustGetRun(t, s, run.ID), spec
}

func specEdit(run Run, spec Spec) SpecEdit {
	return SpecEdit{Spec: Spec{RunID: run.ID, BodyMD: "v2", Hash: hashOf("v2"), Source: "ui_edit"},
		From: run.Status, To: StatusAwaitingApproval, Generation: run.Generation, PriorVersion: spec.Version, PriorHash: spec.Hash, Actor: "owner"}
}

func TestSpecEditGuardsSourceAndPriorVersion(t *testing.T) {
	for _, winner := range []string{"cancel", "code claim", "revision", "other edit", "status ABA"} {
		t.Run(winner, func(t *testing.T) {
			s := openTestStore(t)
			run, spec := approvedSpec(t, s)
			edit := specEdit(run, spec)
			var err error
			switch winner {
			case "cancel":
				err = s.SetRunStatus(t.Context(), run.ID, run.Status, StatusCancelled, RunPatch{Phase: run.Phase})
			case "code claim":
				err = s.StartCode(t.Context(), run.ID, spec.Version, spec.Hash, run.Generation)
			case "revision":
				err = s.DecideRun(t.Context(), run.Status, StatusRunning, Approval{RunID: run.ID, SpecVersion: spec.Version,
					SpecHash: spec.Hash, Decision: "revise", Actor: "owner", Channel: "cli"}, run.Generation)
			case "other edit":
				_, err = s.EditSpec(t.Context(), edit)
			case "status ABA":
				err = s.SetRunStatus(t.Context(), run.ID, run.Status, StatusAwaitingApproval, RunPatch{Phase: run.Phase})
				if err == nil {
					err = s.SetRunStatus(t.Context(), run.ID, StatusAwaitingApproval, run.Status, RunPatch{Phase: run.Phase})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := mustGetRun(t, s, run.ID)
			events := mustEvents(t, s, run.ID, 0)
			if _, err := s.EditSpec(t.Context(), edit); !errors.Is(err, ErrConflict) {
				t.Fatalf("edit overwrote %s: %v", winner, err)
			}
			if diff := cmp.Diff(before, mustGetRun(t, s, run.ID)); diff != "" {
				t.Fatal(diff)
			}
			if diff := cmp.Diff(events, mustEvents(t, s, run.ID, 0)); diff != "" {
				t.Fatal(diff)
			}
			latest, err := s.LatestSpec(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if winner == "other edit" {
				want = 2
			}
			if latest.Version != want {
				t.Fatalf("losing edit appended spec v%d", latest.Version)
			}
		})
	}
}

func TestSpecEditRollsBackStatusContentEventsAndAudit(t *testing.T) {
	s := openTestStore(t)
	run, spec := approvedSpec(t, s)
	events := mustEvents(t, s, run.ID, 0)
	actions, err := s.ListActions(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER reject_edit_action BEFORE INSERT ON actions
 WHEN NEW.kind='edit_spec' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditSpec(t.Context(), specEdit(run, spec)); err == nil {
		t.Fatal("audit failure accepted")
	}
	if diff := cmp.Diff(run, mustGetRun(t, s, run.ID)); diff != "" {
		t.Fatal(diff)
	}
	latest, err := s.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(spec, latest); diff != "" {
		t.Fatal(diff)
	}
	if diff := cmp.Diff(events, mustEvents(t, s, run.ID, 0)); diff != "" {
		t.Fatal(diff)
	}
	gotActions, err := s.ListActions(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(actions, gotActions); diff != "" {
		t.Fatal(diff)
	}
}

func TestFailureGenerationRejectsSameStatusFromNewerWork(t *testing.T) {
	s := openTestStore(t)
	run := mustRun(t, s, Run{Status: StatusRunning, Phase: PhaseRefine})
	if err := s.SetRunStatus(t.Context(), run.ID, run.Status, run.Status, RunPatch{Phase: PhaseClone}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStatus(t.Context(), run.ID, run.Status, StatusFailed, RunPatch{Phase: run.Phase, ExpectedGeneration: &run.Generation}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale failure accepted: %v", err)
	}
	if got := mustGetRun(t, s, run.ID); got.Status != StatusRunning || got.Phase != PhaseClone {
		t.Fatalf("stale worker failed new generation: %+v", got)
	}
}

func TestDecisionGuardsVersionAndHashAtomically(t *testing.T) {
	for _, decision := range []string{"approve", "revise", "reject"} {
		for _, stale := range []string{"version", "hash"} {
			t.Run(decision+"/"+stale, func(t *testing.T) {
				s := openTestStore(t)
				run, spec := approvedSpec(t, s)
				latest, err := s.EditSpec(t.Context(), specEdit(run, spec))
				if err != nil {
					t.Fatal(err)
				}
				run = mustGetRun(t, s, run.ID)
				events := mustEvents(t, s, run.ID, 0)
				version, hash := spec.Version, spec.Hash
				if stale == "hash" {
					version, hash = latest.Version, hashOf("wrong")
				}
				to := map[string]Status{"approve": StatusApproved, "revise": StatusRunning, "reject": StatusRejected}[decision]
				err = s.DecideRun(t.Context(), run.Status, to, Approval{RunID: run.ID, SpecVersion: version,
					SpecHash: hash, Decision: decision, Actor: "owner", Channel: "github"}, run.Generation)
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("stale %s accepted: %v", stale, err)
				}
				if diff := cmp.Diff(run, mustGetRun(t, s, run.ID)); diff != "" {
					t.Fatal(diff)
				}
				if diff := cmp.Diff(events, mustEvents(t, s, run.ID, 0)); diff != "" {
					t.Fatal(diff)
				}
				var count int
				if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM approvals WHERE run_id=?`, run.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 1 {
					t.Fatalf("stale decision persisted %d approvals", count)
				}
			})
		}
	}
}
