package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

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

// probeWhere builds the shared filter clause.
func probeWhere(q probe.ProbeQuery) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if q.AuthIndex != "" {
		clauses = append(clauses, "auth_index = ?")
		args = append(args, q.AuthIndex)
	}
	if q.Model != "" {
		clauses = append(clauses, "model = ?")
		args = append(args, q.Model)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// ListProbes implements probe.HistoryStore, newest first by probe time.
//
// Ordered by probed_at rather than by insertion id: the two agree when probes
// are appended as they finish, and diverge the moment one is not. The table's
// index is on probed_at for the same reason.
func (s *ProbeStore) ListProbes(ctx context.Context, q probe.ProbeQuery) ([]probe.HistoryEntry, error) {
	q = q.Normalise()
	where, args := probeWhere(q)
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT id, auth_index, model, COALESCE(proxy_id, ''), result,
		       COALESCE(state_length, 0), COALESCE(latency_ms, 0), probed_at
		FROM probe_history`+where+`
		ORDER BY probed_at DESC, id DESC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list probe history: %w", err)
	}
	return scanProbes(rows)
}

// CountProbes implements probe.HistoryStore.
func (s *ProbeStore) CountProbes(ctx context.Context, q probe.ProbeQuery) (int, error) {
	where, args := probeWhere(q)
	var n int
	if err := s.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe_history`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count probe history: %w", err)
	}
	return n, nil
}

// PruneProbesBefore deletes rows older than the cutoff.
//
// Retention is by age rather than by row count: the panel reads the recent
// past, and a row cap would let a burst of probes evict the last hour while
// keeping days-old rows.
func (s *ProbeStore) PruneProbesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var affected int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM probe_history WHERE probed_at < ?`, FormatTime(cutoff))
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
