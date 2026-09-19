package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/callhistory"
)

// CallStore implements callhistory.Store against SQLite.
type CallStore struct{ db *DB }

// Calls returns the intercepted-request history store.
func (db *DB) Calls() *CallStore { return &CallStore{db: db} }

// AppendCalls writes a batch in one transaction.
//
// Batched on purpose: the recorder hands over everything it accumulated on a
// one-second tick, and a transaction per request would make a busy proxy
// rewrite the WAL for every turn.
func (s *CallStore) AppendCalls(ctx context.Context, rows []callhistory.Record) error {
	if len(rows) == 0 {
		return nil
	}
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO call_history
				(auth_index, model, request_id, carried_state, injected_state,
				 response_state, status_code, outcome, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("storage: prepare call history insert: %w", err)
		}
		defer stmt.Close()

		for _, r := range rows {
			if _, err := stmt.ExecContext(ctx,
				r.AuthIndex, r.Model, r.RequestID, r.CarriedState, r.InjectedState,
				r.ResponseState, r.StatusCode, r.Outcome, FormatTime(r.CreatedAt),
			); err != nil {
				return fmt.Errorf("storage: append call history: %w", err)
			}
		}
		return nil
	})
}

// callWhere builds the shared filter clause.
func callWhere(q callhistory.Query) (string, []any) {
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

// ListCalls implements callhistory.Store, newest first.
func (s *CallStore) ListCalls(ctx context.Context, q callhistory.Query) ([]callhistory.Record, error) {
	q = q.Normalise()
	where, args := callWhere(q)
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT id, auth_index, model, request_id, carried_state, injected_state,
		       response_state, status_code, outcome, created_at
		FROM call_history`+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list call history: %w", err)
	}
	defer rows.Close()

	var out []callhistory.Record
	for rows.Next() {
		var (
			r         callhistory.Record
			createdAt string
		)
		if err := rows.Scan(&r.ID, &r.AuthIndex, &r.Model, &r.RequestID,
			&r.CarriedState, &r.InjectedState, &r.ResponseState,
			&r.StatusCode, &r.Outcome, &createdAt); err != nil {
			return nil, fmt.Errorf("storage: scan call history: %w", err)
		}
		r.CreatedAt = ParseTime(createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountCalls implements callhistory.Store.
func (s *CallStore) CountCalls(ctx context.Context, q callhistory.Query) (int, error) {
	where, args := callWhere(q)
	var n int
	if err := s.db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM call_history`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count call history: %w", err)
	}
	return n, nil
}

// PruneCallsBefore deletes rows older than the cutoff.
func (s *CallStore) PruneCallsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var affected int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM call_history WHERE created_at < ?`, FormatTime(cutoff))
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}

// DeleteCallsForAccount removes one account's rows.
//
// Scoped to an account rather than truncating the table: the panel offers this
// from a per-account view, and a control that empties every account's history
// is one mis-click away from destroying evidence someone was reading.
func (s *CallStore) DeleteCallsForAccount(ctx context.Context, authIndex string) (int64, error) {
	var affected int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM call_history WHERE auth_index = ?`, authIndex)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}

var _ callhistory.Store = (*CallStore)(nil)
