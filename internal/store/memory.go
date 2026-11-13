package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Memory entry kinds, statuses, sources and operator decisions.
const (
	MemoryObservation = "observation"
	MemoryLesson      = "lesson"

	MemoryActive   = "active"
	MemoryPending  = "pending"
	MemoryApproved = "approved"
	MemoryRejected = "rejected"
	MemoryRevoked  = "revoked"

	MemorySourceHarness  = "harness"
	MemorySourceModel    = "model"
	MemorySourceOperator = "operator"

	MemoryApprove = "approve"
	MemoryReject  = "reject"
	MemoryForget  = "forget"
)

// MemoryEntry is one repository-scoped memory record. Observations are typed
// harness facts and never contain prose. Lessons are prose: model proposals
// start pending, operator lessons start approved. Forgetting revokes an entry
// and erases lesson text; the content hash and audit actions remain.
type MemoryEntry struct {
	ID           int64      `json:"id"`
	Repo         string     `json:"repo"`
	Kind         string     `json:"kind"`
	Status       string     `json:"status"`
	Source       string     `json:"source"`
	RunID        *int64     `json:"run_id"`
	CommandID    string     `json:"command_id"`
	ExitCode     *int64     `json:"exit_code"`
	TimedOut     *bool      `json:"timed_out"`
	BaseSHA      string     `json:"base_sha"`
	HeadSHA      string     `json:"head_sha"`
	PolicyDigest string     `json:"policy_digest"`
	EvidenceRef  string     `json:"evidence_ref"`
	ObservedAt   *time.Time `json:"observed_at"`
	Text         string     `json:"text"`
	ContentHash  string     `json:"content_hash"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	DecidedBy    *string    `json:"decided_by"`
	DecidedAt    *time.Time `json:"decided_at"`
}

// MemoryAction audits a memory creation or decision.
type MemoryAction struct {
	ID        int64     `json:"id"`
	EntryID   int64     `json:"entry_id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Channel   string    `json:"channel"`
	CreatedAt time.Time `json:"created_at"`
}

// MemoryFilter selects entries in descending ID order. Limit defaults to 200
// and is capped at 1000.
type MemoryFilter struct {
	Repo     string
	Kind     string
	Statuses []string
	Limit    int
}

// RetrievalQuery selects only entries eligible for a model prompt: active,
// fresh observations under the current command-policy digest and approved
// lessons. Everything else is filtered by SQL, not by the caller.
type RetrievalQuery struct {
	Repo          string
	PolicyDigest  string
	ObservedAfter time.Time
	MaxCandidates int
}

// MemoryStore persists repository memory.
type MemoryStore interface {
	AddMemory(ctx context.Context, entry MemoryEntry, channel string) (MemoryEntry, error)
	GetMemory(ctx context.Context, id int64) (MemoryEntry, error)
	ListMemory(ctx context.Context, filter MemoryFilter) ([]MemoryEntry, error)
	ListMemoryActions(ctx context.Context, id int64) ([]MemoryAction, error)
	// DecideMemory applies approve/reject (pending lessons only) or forget
	// (any entry not already revoked) with the actor and time recorded.
	DecideMemory(ctx context.Context, id int64, action, actor, channel string) (MemoryEntry, error)
	// RetrievalCandidates returns eligible observations (newest first) and
	// approved lessons (most recently decided first) for one repository.
	RetrievalCandidates(ctx context.Context, q RetrievalQuery) (observations, lessons []MemoryEntry, err error)
}

// MemoryContentHash binds an entry's retrievable content. Observations hash
// their typed fields; lessons hash their text.
func MemoryContentHash(e MemoryEntry) string {
	var raw []byte
	if e.Kind == MemoryObservation {
		var exit int64
		var timedOut bool
		var observed int64
		if e.ExitCode != nil {
			exit = *e.ExitCode
		}
		if e.TimedOut != nil {
			timedOut = *e.TimedOut
		}
		if e.ObservedAt != nil {
			observed = e.ObservedAt.UnixMilli()
		}
		raw, _ = json.Marshal([]any{e.Repo, e.CommandID, exit, timedOut, e.BaseSHA, e.HeadSHA, e.PolicyDigest, e.EvidenceRef, observed})
	} else {
		raw, _ = json.Marshal([]any{e.Repo, e.Text})
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

const memoryColumns = `id,repo,kind,status,source,run_id,command_id,exit_code,timed_out,base_sha,head_sha,
policy_digest,evidence_ref,observed_at,text,content_hash,created_by,created_at,decided_by,decided_at`

func scanMemory(row scanner) (MemoryEntry, error) {
	var e MemoryEntry
	var timedOut, observed, decided sql.NullInt64
	var created int64
	err := row.Scan(&e.ID, &e.Repo, &e.Kind, &e.Status, &e.Source, &e.RunID, &e.CommandID, &e.ExitCode, &timedOut,
		&e.BaseSHA, &e.HeadSHA, &e.PolicyDigest, &e.EvidenceRef, &observed, &e.Text, &e.ContentHash, &e.CreatedBy,
		&created, &e.DecidedBy, &decided)
	if timedOut.Valid {
		value := timedOut.Int64 == 1
		e.TimedOut = &value
	}
	e.ObservedAt, e.DecidedAt = optionalTime(observed), optionalTime(decided)
	e.CreatedAt = time.UnixMilli(created).UTC()
	return e, mapError(err)
}

func validMemoryChannel(channel string) bool {
	return channel == "ui" || channel == "cli" || channel == "harness"
}

// AddMemory inserts an entry and its creation audit atomically. The store
// computes the content hash. Operator lessons are approved on creation; model
// lessons are pending; observations are active.
func (s *SQLite) AddMemory(ctx context.Context, e MemoryEntry, channel string) (MemoryEntry, error) {
	if !validMemoryChannel(channel) {
		return MemoryEntry{}, errors.New("invalid memory channel")
	}
	e.Repo = strings.ToLower(strings.TrimSpace(e.Repo))
	switch {
	case e.Kind == MemoryObservation && e.Source == MemorySourceHarness:
		e.Status = MemoryActive
		at := timestamp(derefTime(e.ObservedAt))
		e.ObservedAt = &at
	case e.Kind == MemoryLesson && e.Source == MemorySourceModel:
		e.Status = MemoryPending
	case e.Kind == MemoryLesson && e.Source == MemorySourceOperator:
		e.Status = MemoryApproved
	default:
		return MemoryEntry{}, errors.New("invalid memory kind or source")
	}
	e.ContentHash = MemoryContentHash(e)
	e.CreatedAt = timestamp(e.CreatedAt)
	e.DecidedBy, e.DecidedAt = nil, nil
	if e.Status == MemoryApproved {
		actor, at := e.CreatedBy, e.CreatedAt
		e.DecidedBy, e.DecidedAt = &actor, &at
	}
	var timedOut any
	if e.TimedOut != nil {
		timedOut = boolInt(*e.TimedOut)
	}
	var saved MemoryEntry
	err := s.withTx(ctx, func(tx *eventTx) error {
		var err error
		saved, err = scanMemory(tx.QueryRowContext(ctx, `INSERT INTO memory_entries(repo,kind,status,source,run_id,command_id,exit_code,timed_out,
			base_sha,head_sha,policy_digest,evidence_ref,observed_at,text,content_hash,created_by,created_at,decided_by,decided_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING `+memoryColumns,
			e.Repo, e.Kind, e.Status, e.Source, e.RunID, e.CommandID, e.ExitCode, timedOut, e.BaseSHA, e.HeadSHA,
			e.PolicyDigest, e.EvidenceRef, optionalMillis(e.ObservedAt), e.Text, e.ContentHash, e.CreatedBy,
			e.CreatedAt.UnixMilli(), e.DecidedBy, optionalMillis(e.DecidedAt)))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_actions(entry_id,action,actor,channel,created_at) VALUES(?,?,?,?,?)`,
			saved.ID, "create", e.CreatedBy, channel, e.CreatedAt.UnixMilli())
		return err
	})
	if err != nil {
		return MemoryEntry{}, err
	}
	return saved, nil
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// GetMemory reads one entry.
func (s *SQLite) GetMemory(ctx context.Context, id int64) (MemoryEntry, error) {
	return scanMemory(s.db.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memory_entries WHERE id=?`, id))
}

// ListMemory returns entries for operator review, newest first.
func (s *SQLite) ListMemory(ctx context.Context, f MemoryFilter) ([]MemoryEntry, error) {
	query := `SELECT ` + memoryColumns + ` FROM memory_entries WHERE 1=1`
	var args []any
	if f.Repo != "" {
		query += ` AND repo=?`
		args = append(args, strings.ToLower(strings.TrimSpace(f.Repo)))
	}
	if f.Kind != "" {
		query += ` AND kind=?`
		args = append(args, f.Kind)
	}
	if len(f.Statuses) > 0 {
		query += ` AND status IN (` + placeholders(len(f.Statuses)) + `)`
		for _, status := range f.Statuses {
			args = append(args, status)
		}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, min(limit, 1000))
	return s.queryMemory(ctx, query, args...)
}

func (s *SQLite) queryMemory(ctx context.Context, query string, args ...any) ([]MemoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []MemoryEntry{}
	for rows.Next() {
		e, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListMemoryActions returns an entry's audit history in order.
func (s *SQLite) ListMemoryActions(ctx context.Context, id int64) ([]MemoryAction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,entry_id,action,actor,channel,created_at FROM memory_actions WHERE entry_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []MemoryAction{}
	for rows.Next() {
		var a MemoryAction
		var created int64
		if err := rows.Scan(&a.ID, &a.EntryID, &a.Action, &a.Actor, &a.Channel, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideMemory compares and sets the entry status in one transaction with its
// audit action. Forget erases lesson text so it can never be retrieved again.
func (s *SQLite) DecideMemory(ctx context.Context, id int64, action, actor, channel string) (MemoryEntry, error) {
	if strings.TrimSpace(actor) == "" || !validMemoryChannel(channel) || channel == "harness" {
		return MemoryEntry{}, errors.New("memory decisions require an operator actor and channel")
	}
	var update string
	switch action {
	case MemoryApprove:
		update = `UPDATE memory_entries SET status='approved',decided_by=?,decided_at=? WHERE id=? AND kind='lesson' AND status='pending'`
	case MemoryReject:
		update = `UPDATE memory_entries SET status='rejected',decided_by=?,decided_at=? WHERE id=? AND kind='lesson' AND status='pending'`
	case MemoryForget:
		update = `UPDATE memory_entries SET status='revoked',text='',decided_by=?,decided_at=? WHERE id=? AND status!='revoked'`
	default:
		return MemoryEntry{}, errors.New("unknown memory action")
	}
	now := time.Now().UTC().UnixMilli()
	err := s.withTx(ctx, func(tx *eventTx) error {
		result, err := tx.ExecContext(ctx, update, actor, now, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM memory_entries WHERE id=?`, id).Scan(&exists); err != nil {
				return err
			}
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_actions(entry_id,action,actor,channel,created_at) VALUES(?,?,?,?,?)`, id, action, actor, channel, now)
		return err
	})
	if err != nil {
		return MemoryEntry{}, err
	}
	return s.GetMemory(ctx, id)
}

// RetrievalCandidates applies every eligibility rule in SQL: repository,
// kind, status, command-policy digest and observation freshness.
func (s *SQLite) RetrievalCandidates(ctx context.Context, q RetrievalQuery) ([]MemoryEntry, []MemoryEntry, error) {
	limit := q.MaxCandidates
	if limit <= 0 {
		limit = 200
	}
	repo := strings.ToLower(strings.TrimSpace(q.Repo))
	if repo == "" || q.PolicyDigest == "" {
		return []MemoryEntry{}, []MemoryEntry{}, nil
	}
	observations, err := s.queryMemory(ctx, `SELECT `+memoryColumns+` FROM memory_entries
		WHERE repo=? AND kind='observation' AND status='active' AND source='harness' AND policy_digest=? AND observed_at>=?
		ORDER BY observed_at DESC, id DESC LIMIT ?`, repo, q.PolicyDigest, q.ObservedAfter.UnixMilli(), limit)
	if err != nil {
		return nil, nil, err
	}
	lessons, err := s.queryMemory(ctx, `SELECT `+memoryColumns+` FROM memory_entries
		WHERE repo=? AND kind='lesson' AND status='approved' AND decided_at IS NOT NULL AND length(text)>0
		ORDER BY decided_at DESC, id DESC LIMIT ?`, repo, limit)
	if err != nil {
		return nil, nil, err
	}
	return observations, lessons, nil
}
