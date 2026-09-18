package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/yangshoulai/codex-turn-state-manager/internal/accounts"
	"github.com/yangshoulai/codex-turn-state-manager/internal/probe"
)

// WindowStore implements probe.WindowStore against SQLite.
type WindowStore struct{ db *DB }

// Windows returns the time-window store.
func (db *DB) Windows() *WindowStore { return &WindowStore{db: db} }

// ListWindows implements probe.WindowStore.
func (s *WindowStore) ListWindows(ctx context.Context) ([]probe.TimeWindow, error) {
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT id, label, days_of_week, start_time, end_time, enabled, sort_order
		FROM time_windows
		ORDER BY sort_order, start_time`)
	if err != nil {
		return nil, fmt.Errorf("storage: list time windows: %w", err)
	}
	defer rows.Close()

	var out []probe.TimeWindow
	for rows.Next() {
		var (
			w          probe.TimeWindow
			daysRaw    string
			enabledInt int
		)
		if err := rows.Scan(&w.ID, &w.Label, &daysRaw, &w.StartTime, &w.EndTime,
			&enabledInt, &w.SortOrder); err != nil {
			return nil, fmt.Errorf("storage: scan time window: %w", err)
		}
		w.Enabled = enabledInt != 0
		if daysRaw != "" {
			// A malformed day list degrades to "every day" rather than
			// refusing to start; the operator sees the window and can fix it.
			if err := json.Unmarshal([]byte(daysRaw), &w.DaysOfWeek); err != nil {
				w.DaysOfWeek = nil
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpsertWindow implements probe.WindowStore.
func (s *WindowStore) UpsertWindow(ctx context.Context, w probe.TimeWindow) error {
	days, err := json.Marshal(w.DaysOfWeek)
	if err != nil {
		return fmt.Errorf("storage: encode days_of_week: %w", err)
	}
	if w.DaysOfWeek == nil {
		days = []byte("[]")
	}
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO time_windows
				(id, label, days_of_week, start_time, end_time, enabled, sort_order)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				label        = excluded.label,
				days_of_week = excluded.days_of_week,
				start_time   = excluded.start_time,
				end_time     = excluded.end_time,
				enabled      = excluded.enabled,
				sort_order   = excluded.sort_order`,
			w.ID, w.Label, string(days), w.StartTime, w.EndTime, boolInt(w.Enabled), w.SortOrder)
		if err != nil {
			return fmt.Errorf("storage: upsert time window %s: %w", w.ID, err)
		}
		return nil
	})
}

// DeleteWindow implements probe.WindowStore.
func (s *WindowStore) DeleteWindow(ctx context.Context, id string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM time_windows WHERE id = ?`, id)
		return err
	})
}

var _ probe.WindowStore = (*WindowStore)(nil)

// AccountModelStore implements accounts.ConfigStore against SQLite.
type AccountModelStore struct{ db *DB }

// AccountModels returns the per-pair probe configuration store.
func (db *DB) AccountModels() *AccountModelStore { return &AccountModelStore{db: db} }

// ListAccountModels implements accounts.ConfigStore.
func (s *AccountModelStore) ListAccountModels(ctx context.Context) ([]accounts.Config, error) {
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT auth_index, model, probe_enabled FROM account_model_config`)
	if err != nil {
		return nil, fmt.Errorf("storage: list account models: %w", err)
	}
	defer rows.Close()

	var out []accounts.Config
	for rows.Next() {
		var (
			c       accounts.Config
			enabled int
		)
		if err := rows.Scan(&c.AuthIndex, &c.Model, &enabled); err != nil {
			return nil, fmt.Errorf("storage: scan account model: %w", err)
		}
		c.ProbeEnabled = enabled != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpsertAccountModel implements accounts.ConfigStore.
func (s *AccountModelStore) UpsertAccountModel(ctx context.Context, c accounts.Config) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO account_model_config (auth_index, model, probe_enabled)
			VALUES (?, ?, ?)
			ON CONFLICT(auth_index, model) DO UPDATE SET probe_enabled = excluded.probe_enabled`,
			c.AuthIndex, c.Model, boolInt(c.ProbeEnabled))
		if err != nil {
			return fmt.Errorf("storage: upsert account model %s/%s: %w", c.AuthIndex, c.Model, err)
		}
		return nil
	})
}

var _ accounts.ConfigStore = (*AccountModelStore)(nil)

// CursorStore persists the round-robin position per account so a CPA restart
// does not always resume on the same account.
type CursorStore struct{ db *DB }

// Cursors returns the scheduler cursor store.
func (db *DB) Cursors() *CursorStore { return &CursorStore{db: db} }

// LastUsed returns the stored cursor for an account.
func (s *CursorStore) LastUsed(ctx context.Context, authIndex string) (string, bool, error) {
	var last string
	err := s.db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(last_used, '') FROM scheduler_cursor WHERE auth_index = ?`,
		authIndex).Scan(&last)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("storage: read cursor %s: %w", authIndex, err)
	}
	return last, true, nil
}

// SetLastUsed upserts the cursor for an account.
func (s *CursorStore) SetLastUsed(ctx context.Context, authIndex, lastUsed string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO scheduler_cursor (auth_index, last_used) VALUES (?, ?)
			ON CONFLICT(auth_index) DO UPDATE SET last_used = excluded.last_used`,
			authIndex, lastUsed)
		return err
	})
}

// AllCursors returns every cursor, keyed by authIndex.
func (s *CursorStore) AllCursors(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.sql.QueryContext(ctx,
		`SELECT auth_index, COALESCE(last_used, '') FROM scheduler_cursor`)
	if err != nil {
		return nil, fmt.Errorf("storage: list cursors: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("storage: scan cursor: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
