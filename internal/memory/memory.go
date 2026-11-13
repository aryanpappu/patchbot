// Package memory selects, renders and records repository memory.
//
// Trust model (docs/planning/step2/codex-round1.md section 1):
//   - Typed observations are written only by the harness from fields it
//     measured itself: command ID, exit status, timeout, base/head SHA,
//     command-policy digest, run ID and an evidence reference. Command output
//     is never stored in or rendered from an observation.
//   - Prose lessons come from the model (pending until an operator approves)
//     or from the operator (approved on creation). Only approved lessons are
//     retrievable.
//   - Selection is deterministic and bounded by entry count and bytes. The
//     result is unverified context for the user turn, never instructions.
//
// Memory cannot change command policy, protected paths, repository scope or
// the approval gate: nothing in this package is consulted by those checks.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// candidateLimit bounds rows read per kind before deterministic selection.
const candidateLimit = 200

// PolicyDigest identifies the operator command policy for a repository. A
// changed command, test command or trusted PATH makes earlier observations
// stale, so they are no longer retrieved.
func PolicyDigest(repo *config.Repo, trustedPath []string) string {
	var commands map[string][]string
	var test string
	if repo != nil {
		commands = repo.Commands
		test, _ = repo.TestCommandID()
	}
	// encoding/json sorts map keys, so the digest is deterministic.
	raw, _ := json.Marshal(map[string]any{"commands": commands, "test_command": test, "trusted_path": trustedPath})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Selected records one entry frozen into a stage.
type Selected struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	ContentHash string `json:"content_hash"`
	Bytes       int    `json:"bytes"`
}

// Selection is the frozen per-stage memory. Text is the rendered data that
// the agent wraps as untrusted data; Entries are recorded as evidence.
type Selection struct {
	Repo         string     `json:"repo"`
	Stage        string     `json:"stage"`
	PolicyDigest string     `json:"policy_digest"`
	SelectedAt   time.Time  `json:"selected_at"`
	MaxEntries   int        `json:"max_entries"`
	MaxBytes     int        `json:"max_bytes"`
	Bytes        int        `json:"bytes"`
	Entries      []Selected `json:"entries"`
	Text         string     `json:"-"`
}

// Select returns the bounded, deterministic memory for one stage. Disabled
// memory returns an empty selection without reading the store.
func Select(ctx context.Context, db store.MemoryStore, cfg config.Memory, repo, stage, policyDigest string, now time.Time) (Selection, error) {
	sel := Selection{Repo: strings.ToLower(strings.TrimSpace(repo)), Stage: stage, PolicyDigest: policyDigest, SelectedAt: now.UTC(), MaxEntries: cfg.MaxEntries, MaxBytes: cfg.MaxBytes, Entries: []Selected{}}
	if !cfg.Enabled || cfg.MaxEntries <= 0 || cfg.MaxBytes <= 0 || db == nil {
		return sel, nil
	}
	observations, lessons, err := db.RetrievalCandidates(ctx, store.RetrievalQuery{Repo: sel.Repo, PolicyDigest: policyDigest, ObservedAfter: now.Add(-cfg.ObservationTTL), MaxCandidates: candidateLimit})
	if err != nil {
		return sel, err
	}
	// Observations take at most half the entry budget, newest per command.
	obsSlots := (cfg.MaxEntries + 1) / 2
	var parts []string
	add := func(e store.MemoryEntry, text string) bool {
		if len(sel.Entries) >= cfg.MaxEntries || sel.Bytes+len(text) > cfg.MaxBytes {
			return false
		}
		sel.Entries = append(sel.Entries, Selected{ID: e.ID, Kind: e.Kind, ContentHash: e.ContentHash, Bytes: len(text)})
		sel.Bytes += len(text)
		parts = append(parts, text)
		return true
	}
	seen := map[string]bool{}
	taken := 0
	for _, e := range observations {
		// Defense in depth: the store filters these too.
		if e.Repo != sel.Repo || e.Kind != store.MemoryObservation || e.Status != store.MemoryActive || e.PolicyDigest != policyDigest || seen[e.CommandID] {
			continue
		}
		if taken >= obsSlots {
			break
		}
		seen[e.CommandID] = true
		if add(e, RenderObservation(e)) {
			taken++
		}
	}
	for _, e := range lessons {
		if e.Repo != sel.Repo || e.Kind != store.MemoryLesson || e.Status != store.MemoryApproved || e.Text == "" {
			continue
		}
		add(e, RenderLesson(e))
	}
	sel.Text = strings.Join(parts, "\n\n")
	return sel, nil
}

// RenderObservation formats only typed, harness-measured fields.
func RenderObservation(e store.MemoryEntry) string {
	exit, timedOut := int64(0), false
	if e.ExitCode != nil {
		exit = *e.ExitCode
	}
	if e.TimedOut != nil {
		timedOut = *e.TimedOut
	}
	observed := ""
	if e.ObservedAt != nil {
		observed = e.ObservedAt.UTC().Format(time.RFC3339)
	}
	head := e.HeadSHA
	if head == "" {
		head = "uncommitted"
	}
	run := "unknown"
	if e.RunID != nil {
		run = fmt.Sprint(*e.RunID)
	}
	return fmt.Sprintf("observation #%d: harness command %q exited %d (timed out: %t) at base %s, head %s; run %s; evidence %s; observed %s. This records an exit status only, not test completeness or correctness.",
		e.ID, e.CommandID, exit, timedOut, e.BaseSHA, head, run, e.EvidenceRef, observed)
}

// RenderLesson formats an approved lesson with its provenance.
func RenderLesson(e store.MemoryEntry) string {
	by := "operator"
	if e.DecidedBy != nil {
		by = *e.DecidedBy
	}
	return fmt.Sprintf("lesson #%d (%s, approved by %s): %s", e.ID, e.Source, by, e.Text)
}

// Evidence returns the JSON recorded for a frozen selection.
func (s Selection) Evidence() string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// CleanLesson trims, redacts and bounds lesson text (see tools.CleanLesson).
func CleanLesson(text string, redactor *secret.Redactor) (string, error) {
	return tools.CleanLesson(text, redactor)
}

// LessonInput describes a new prose lesson.
type LessonInput struct {
	Repo    string
	Text    string
	Source  string // model or operator
	RunID   *int64
	Actor   string
	Channel string
}

// AddLesson cleans and stores a lesson. Model lessons are pending; operator
// lessons are approved immediately with the operator as the deciding actor.
func AddLesson(ctx context.Context, db store.MemoryStore, redactor *secret.Redactor, in LessonInput) (store.MemoryEntry, error) {
	text, err := CleanLesson(in.Text, redactor)
	if err != nil {
		return store.MemoryEntry{}, err
	}
	if !slices.Contains([]string{store.MemorySourceModel, store.MemorySourceOperator}, in.Source) {
		return store.MemoryEntry{}, errors.New("invalid lesson source")
	}
	actor := strings.TrimSpace(redactor.Redact(in.Actor))
	if actor == "" {
		return store.MemoryEntry{}, errors.New("lesson actor required")
	}
	return db.AddMemory(ctx, store.MemoryEntry{Repo: in.Repo, Kind: store.MemoryLesson, Source: in.Source, RunID: in.RunID, Text: text, CreatedBy: actor}, in.Channel)
}

// ObservationInput describes one harness-run command result.
type ObservationInput struct {
	Repo, CommandID        string
	ExitCode               int
	TimedOut               bool
	BaseSHA, HeadSHA       string
	PolicyDigest, Evidence string
	RunID                  int64
	ObservedAt             time.Time
}

// AddObservation stores a typed observation. It accepts no free text, so
// command output can never become an observation.
func AddObservation(ctx context.Context, db store.MemoryStore, in ObservationInput) (store.MemoryEntry, error) {
	exit, timedOut, at := int64(in.ExitCode), in.TimedOut, in.ObservedAt
	var run *int64
	if in.RunID > 0 {
		run = &in.RunID
	}
	return db.AddMemory(ctx, store.MemoryEntry{Repo: in.Repo, Kind: store.MemoryObservation, Source: store.MemorySourceHarness, RunID: run,
		CommandID: in.CommandID, ExitCode: &exit, TimedOut: &timedOut, BaseSHA: in.BaseSHA, HeadSHA: in.HeadSHA,
		PolicyDigest: in.PolicyDigest, EvidenceRef: in.Evidence, ObservedAt: &at, CreatedBy: "harness"}, "harness")
}
