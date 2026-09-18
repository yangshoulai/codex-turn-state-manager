package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/yangshoulai/codex-turn-state-manager/internal/probe"
)

// ProbeStore implements probe.HistoryStore against SQLite.
type ProbeStore struct{ db *DB }

// Probes returns the probe history store.
func (db *DB) Probes() *ProbeStore { return &ProbeStore{db: db} }

// AppendProbe implements probe.HistoryStore.
func (s *ProbeStore) AppendProbe(ctx context.Context, e probe.HistoryEntry) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO probe_history
				(auth_index, model, proxy_id, result, state_length, latency_ms, probed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.AuthIndex, e.Model, nullable(e.ProxyID), string(e.Result),
			nullInt(e.StateLength), e.LatencyMS, FormatTime(e.ProbedAt))
		if err != nil {
			return fmt.Errorf("storage: append probe history: %w", err)
		}
		return nil
	})
}

// ListProbes implements probe.HistoryStore, newest first.
func (s *ProbeStore) ListProbes(ctx context.Context, limit, offset int) ([]probe.HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT id, auth_index, model, COALESCE(proxy_id, ''), result,
		       COALESCE(state_length, 0), COALESCE(latency_ms, 0), probed_at
		FROM probe_history
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("storage: list probe history: %w", err)
	}
	return scanProbes(rows)
}

// ListProbesFor returns probe history for one pair, newest first.
func (s *ProbeStore) ListProbesFor(ctx context.Context, authIndex, model string, limit int) ([]probe.HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT id, auth_index, model, COALESCE(proxy_id, ''), result,
		       COALESCE(state_length, 0), COALESCE(latency_ms, 0), probed_at
		FROM probe_history
		WHERE auth_index = ? AND model = ?
		ORDER BY id DESC
		LIMIT ?`, authIndex, model, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: list probe history for %s/%s: %w", authIndex, model, err)
	}
	return scanProbes(rows)
}

// PruneProbes keeps the newest `keep` rows and deletes the rest, bounding
// table growth on a long-running instance.
func (s *ProbeStore) PruneProbes(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	var affected int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM probe_history
			WHERE id NOT IN (SELECT id FROM probe_history ORDER BY id DESC LIMIT ?)`, keep)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}

func scanProbes(rows *sql.Rows) ([]probe.HistoryEntry, error) {
	defer rows.Close()
	var out []probe.HistoryEntry
	for rows.Next() {
		var (
			e        probe.HistoryEntry
			result   string
			probedAt string
		)
		if err := rows.Scan(&e.ID, &e.AuthIndex, &e.Model, &e.ProxyID, &result,
			&e.StateLength, &e.LatencyMS, &probedAt); err != nil {
			return nil, fmt.Errorf("storage: scan probe history: %w", err)
		}
		e.Result = probe.Outcome(result)
		e.ProbedAt = ParseTime(probedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

var _ probe.HistoryStore = (*ProbeStore)(nil)
