package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// selected IDs, content hashes and the hash of the delivered text are
// persisted first, so later approvals, rejections or forgets cannot change
// what an in-flight stage received. Disabled memory reads and writes nothing.
func (r *Runner) selectMemory(ctx context.Context, run store.Run, stage string) (string, error) {
	if !r.cfg.Memory.Enabled {
		return "", nil
	}
	sel, err := memory.Select(ctx, r.db, r.cfg.Memory, run.Repo, stage, r.policyDigest(run), r.now())
	if err != nil {
		return "", err
	}
	text := r.redactor.Redact(sel.Text)
	sum := sha256.Sum256([]byte(text))
	sel.TextSHA256 = hex.EncodeToString(sum[:])
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
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.db.SaveArtifact(recordCtx, store.Artifact{RunID: run.ID, Kind: memoryArtifact, Attempt: attempt, Content: sel.Evidence()}); err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{"stage": stage, "attempt": attempt, "policy_digest": sel.PolicyDigest, "bytes": sel.Bytes, "text_sha256": sel.TextSHA256, "entries": sel.Entries})
	if err != nil {
		return "", err
	}
	if err := r.memoryEvent(recordCtx, run, "memory.selected", payload); err != nil {
		return "", err
	}
	return text, nil
}

func (r *Runner) memoryEvent(ctx context.Context, run store.Run, kind string, payload json.RawMessage) error {
	recorder := agent.StoreRecorder{Store: r.db, RunID: run.ID, Phase: run.Phase, Redactor: r.redactor}
	return recorder.Event(ctx, kind, payload)
}

// memoryWarn records a non-fatal memory problem. Memory writes are auxiliary:
// they never abort a run or replace its own result or error.
func (r *Runner) memoryWarn(ctx context.Context, run store.Run, message string) {
	raw, _ := json.Marshal(map[string]string{"level": "warn", "message": message})
	_ = r.memoryEvent(ctx, run, "log", raw)
}

// observe stores a typed harness observation. It takes the measured result,
// never its output, so command output cannot become memory. Failures are
// recorded as warnings only.
func (r *Runner) observe(ctx context.Context, run store.Run, command string, result execx.Result, head, evidence string) {
	if !r.cfg.Memory.Enabled || command == "" {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := memory.AddObservation(recordCtx, r.db, memory.ObservationInput{Repo: run.Repo, CommandID: command, ExitCode: result.ExitCode, TimedOut: result.TimedOut,
		BaseSHA: run.BaseSHA, HeadSHA: head, PolicyDigest: r.policyDigest(run), Evidence: evidence, RunID: run.ID, ObservedAt: r.now()})
	if err != nil {
		r.memoryWarn(recordCtx, run, "Memory observation could not be recorded; the run continues.")
	}
}

// saveLessons stores model proposals as pending. They are never retrieved
// until an operator approves them. Proposals over the per-repository pending
// cap, duplicates and invalid text are dropped with an event. Failures are
// warnings only and never mask the code stage's result.
func (r *Runner) saveLessons(ctx context.Context, run store.Run, lessons []string) {
	if !r.cfg.Memory.Enabled || len(lessons) == 0 {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id := run.ID
	dropped := map[string]int{}
	for i, text := range lessons {
		if i >= tools.MaxLessonsPerRun {
			dropped["run_limit"]++
			continue
		}
		pending, err := r.db.CountMemory(recordCtx, run.Repo, store.MemoryLesson, store.MemoryPending)
		if err != nil {
			r.memoryWarn(recordCtx, run, "Lesson proposals could not be recorded; the run continues.")
			return
		}
		if pending >= r.cfg.Memory.MaxPending {
			dropped["pending_cap"]++
			continue
		}
		_, err = memory.AddLesson(recordCtx, r.db, r.redactor, memory.LessonInput{Repo: run.Repo, Text: text, Source: store.MemorySourceModel, RunID: &id, Actor: fmt.Sprintf("model:%s", run.CodeModel), Channel: "harness"})
		switch {
		case err == nil:
		case errors.Is(err, tools.ErrInvalidLesson):
			dropped["invalid"]++
		case errors.Is(err, store.ErrDuplicateMemory):
			dropped["duplicate"]++
		default:
			r.memoryWarn(recordCtx, run, "A lesson proposal could not be recorded; the run continues.")
		}
	}
	if len(dropped) > 0 {
		raw, _ := json.Marshal(map[string]any{"dropped": dropped, "max_pending": r.cfg.Memory.MaxPending})
		_ = r.memoryEvent(recordCtx, run, "memory.lessons_dropped", raw)
	}
}
