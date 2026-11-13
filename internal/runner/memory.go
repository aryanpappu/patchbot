package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/memory"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// memoryArtifact records each stage's frozen selection. It is internal: the
// public artifact allowlist does not include it, but the matching
// memory.selected event exposes the IDs and content hashes.
const memoryArtifact = "memory_selection"

func (r *Runner) policyDigest(run store.Run) string {
	return memory.PolicyDigest(r.cfg.Repos[run.Repo], r.cfg.Limits.TrustedPath)
}

// selectMemory freezes the memory for one stage before the model sees it. The
// selected IDs and content hashes are persisted first, so later approvals,
// rejections or forgets cannot change what an in-flight stage received.
// Disabled memory neither reads nor writes anything.
func (r *Runner) selectMemory(ctx context.Context, run store.Run, stage string) (string, error) {
	if !r.cfg.Memory.Enabled {
		return "", nil
	}
	sel, err := memory.Select(ctx, r.db, r.cfg.Memory, run.Repo, stage, r.policyDigest(run), r.now())
	if err != nil {
		return "", err
	}
	artifacts, err := r.db.ListArtifacts(ctx, run.ID)
	if err != nil {
		return "", err
	}
	attempt := int64(0)
	for _, a := range artifacts {
		if a.Kind == memoryArtifact && a.Attempt >= attempt {
			attempt = a.Attempt + 1
		}
	}
	evidence := sel.Evidence()
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.db.SaveArtifact(recordCtx, store.Artifact{RunID: run.ID, Kind: memoryArtifact, Attempt: attempt, Content: evidence}); err != nil {
		return "", err
	}
	recorder := agent.StoreRecorder{Store: r.db, RunID: run.ID, Phase: run.Phase, Redactor: r.redactor}
	payload, err := json.Marshal(map[string]any{"stage": stage, "attempt": attempt, "policy_digest": sel.PolicyDigest, "bytes": sel.Bytes, "entries": sel.Entries})
	if err != nil {
		return "", err
	}
	if err := recorder.Event(recordCtx, "memory.selected", payload); err != nil {
		return "", err
	}
	return sel.Text, nil
}

// observe stores a typed harness observation. It takes the measured result,
// never its output, so command output cannot become memory.
func (r *Runner) observe(ctx context.Context, run store.Run, command string, result execx.Result, head, evidence string) error {
	if !r.cfg.Memory.Enabled || command == "" {
		return nil
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := memory.AddObservation(recordCtx, r.db, memory.ObservationInput{Repo: run.Repo, CommandID: command, ExitCode: result.ExitCode, TimedOut: result.TimedOut,
		BaseSHA: run.BaseSHA, HeadSHA: head, PolicyDigest: r.policyDigest(run), Evidence: evidence, RunID: run.ID, ObservedAt: r.now()})
	return err
}

// saveLessons stores model proposals as pending. They are never retrieved
// until an operator approves them.
func (r *Runner) saveLessons(ctx context.Context, run store.Run, lessons []string) error {
	if !r.cfg.Memory.Enabled || len(lessons) == 0 {
		return nil
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id := run.ID
	for i, text := range lessons {
		if i >= tools.MaxLessonsPerRun {
			break
		}
		_, err := memory.AddLesson(recordCtx, r.db, r.redactor, memory.LessonInput{Repo: run.Repo, Text: text, Source: store.MemorySourceModel, RunID: &id, Actor: fmt.Sprintf("model:%s", run.CodeModel), Channel: "harness"})
		if err != nil && !errors.Is(err, tools.ErrInvalidLesson) {
			return err
		}
	}
	return nil
}
