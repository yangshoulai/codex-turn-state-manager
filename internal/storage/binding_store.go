package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// BindingStore implements states.Store and states.HistoryStore against SQLite.
type BindingStore struct{ db *DB }

// Bindings returns the binding store.
func (db *DB) Bindings() *BindingStore { return &BindingStore{db: db} }

// UpsertBinding implements states.Store.
func (s *BindingStore) UpsertBinding(ctx context.Context, b states.Binding) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO state_binding
				(auth_index, model, state_value, state_length, bound_at, expires_at, refresh_after, source, proxy_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(auth_index, model) DO UPDATE SET
				state_value   = excluded.state_value,
				state_length  = excluded.state_length,
				bound_at      = excluded.bound_at,
				expires_at    = excluded.expires_at,
				refresh_after = excluded.refresh_after,
				source        = excluded.source,
				proxy_id      = excluded.proxy_id`,
			b.AuthIndex, b.Model, b.StateValue, b.StateLength,
			FormatTime(b.BoundAt), FormatTime(b.ExpiresAt), FormatTime(b.RefreshAfter),
			string(b.Source), nullable(b.ProxyID))
		if err != nil {
			return fmt.Errorf("storage: upsert binding %s/%s: %w", b.AuthIndex, b.Model, err)
		}
		return nil
	})
}

// DeleteBinding implements states.Store.
func (s *BindingStore) DeleteBinding(ctx context.Context, p states.Pair) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM state_binding WHERE auth_index = ? AND model = ?`, p.AuthIndex, p.Model)
		return err
	})
}

// ListBindings implements states.Store.
func (s *BindingStore) ListBindings(ctx context.Context) ([]states.Binding, error) {
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT auth_index, model, state_value, state_length,
		       bound_at, expires_at, refresh_after, source, COALESCE(proxy_id, '')
		FROM state_binding`)
	if err != nil {
		return nil, fmt.Errorf("storage: list bindings: %w", err)
	}
	defer rows.Close()

	var out []states.Binding
	for rows.Next() {
		var (
			b                         states.Binding
			boundAt, expires, refresh string
			source                    string
		)
		if err := rows.Scan(&b.AuthIndex, &b.Model, &b.StateValue, &b.StateLength,
			&boundAt, &expires, &refresh, &source, &b.ProxyID); err != nil {
			return nil, fmt.Errorf("storage: scan binding: %w", err)
		}
		b.BoundAt = ParseTime(boundAt)
		b.ExpiresAt = ParseTime(expires)
		b.RefreshAfter = ParseTime(refresh)
		b.Source = states.Source(source)
		out = append(out, b)
	}
	return out, rows.Err()
}

// AppendHistory implements states.HistoryStore.
func (s *BindingStore) AppendHistory(ctx context.Context, e states.HistoryEntry) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO state_binding_history
				(auth_index, model, state_value, state_length, source, action,
				 bound_at, expires_at, proxy_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.AuthIndex, e.Model, e.StateValue, e.StateLength,
			string(e.Source), string(e.Action), FormatTime(e.BoundAt),
			FormatTime(e.ExpiresAt), nullable(e.ProxyID), FormatTime(e.CreatedAt))
		if err != nil {
			return fmt.Errorf("storage: append binding history: %w", err)
		}
		return nil
	})
}

// ListHistory implements states.HistoryStore.
func (s *BindingStore) ListHistory(ctx context.Context, p states.Pair, limit, offset int) ([]states.HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT id, auth_index, model, state_value, state_length, source, action,
		       bound_at, COALESCE(expires_at, ''), COALESCE(proxy_id, ''), created_at
		FROM state_binding_history
		WHERE auth_index = ? AND model = ?
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, p.AuthIndex, p.Model, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("storage: list binding history: %w", err)
	}
	defer rows.Close()

	var out []states.HistoryEntry
	for rows.Next() {
		var (
			e                states.HistoryEntry
			boundAt, expires string
			source, action   string
			createdAt        string
		)
		if err := rows.Scan(&e.ID, &e.AuthIndex, &e.Model, &e.StateValue, &e.StateLength,
			&source, &action, &boundAt, &expires, &e.ProxyID, &createdAt); err != nil {
			return nil, fmt.Errorf("storage: scan binding history: %w", err)
		}
		e.Source = states.Source(source)
		e.Action = states.Action(action)
		e.BoundAt = ParseTime(boundAt)
		e.ExpiresAt = ParseTime(expires)
		e.CreatedAt = ParseTime(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearHistory implements states.HistoryStore.
func (s *BindingStore) ClearHistory(ctx context.Context, p states.Pair) (int64, error) {
	var affected int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM state_binding_history WHERE auth_index = ? AND model = ?`, p.AuthIndex, p.Model)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var _ states.Store = (*BindingStore)(nil)
var _ states.HistoryStore = (*BindingStore)(nil)

var errNotFound = errors.New("storage: not found")
