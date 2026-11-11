package store

import (
	"context"
	"time"
)

// ArtifactMetadata describes an artifact without loading its content.
type ArtifactMetadata struct {
	Kind      string    `json:"kind"`
	Attempt   int64     `json:"attempt"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// ListSpecs returns the complete specification history in version order.
func (s *SQLite) ListSpecs(ctx context.Context, id int64) ([]Spec, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+specColumns+" FROM specs WHERE run_id=? ORDER BY version", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Spec{}
	for rows.Next() {
		value, err := scanSpec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// ListApprovals returns every decision in audit order.
func (s *SQLite) ListApprovals(ctx context.Context, id int64) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE run_id=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Approval{}
	for rows.Next() {
		value, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// ListArtifacts returns metadata in kind/attempt order, without internal content.
func (s *SQLite) ListArtifacts(ctx context.Context, id int64) ([]ArtifactMetadata, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT kind,attempt,length(CAST(content AS BLOB)),created_at FROM run_artifacts WHERE run_id=? ORDER BY kind,attempt", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ArtifactMetadata{}
	for rows.Next() {
		var value ArtifactMetadata
		var created int64
		if err := rows.Scan(&value.Kind, &value.Attempt, &value.Bytes, &created); err != nil {
			return nil, err
		}
		value.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, value)
	}
	return out, rows.Err()
}
