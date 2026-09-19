package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/yangshoulai/codex-turn-state-manager/internal/accounts"
)

// ModelListStore implements accounts.ModelListStore against SQLite.
type ModelListStore struct{ db *DB }

// AccountModels returns the per-account model list store.
func (db *DB) ModelLists() *ModelListStore { return &ModelListStore{db: db} }

// ListModelLists implements accounts.ModelListStore.
func (s *ModelListStore) ListModelLists(ctx context.Context) ([]accounts.ModelList, error) {
	rows, err := s.db.sql.QueryContext(ctx,
		`SELECT auth_index, models, source, synced_at, error FROM account_models`)
	if err != nil {
		return nil, fmt.Errorf("storage: list account model lists: %w", err)
	}
	defer rows.Close()

	var out []accounts.ModelList
	for rows.Next() {
		var (
			l        accounts.ModelList
			raw      string
			syncedAt string
		)
		if err := rows.Scan(&l.AuthIndex, &raw, &l.Source, &syncedAt, &l.Error); err != nil {
			return nil, fmt.Errorf("storage: scan account model list: %w", err)
		}
		if raw != "" {
			// A row written by a future build with a shape this one cannot read
			// degrades to "no list", which the registry already handles by
			// falling back to the shared catalog.
			_ = json.Unmarshal([]byte(raw), &l.Models)
		}
		l.SyncedAt = ParseTime(syncedAt)
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveModelList implements accounts.ModelListStore.
func (s *ModelListStore) SaveModelList(ctx context.Context, l accounts.ModelList) error {
	models := l.Models
	if models == nil {
		models = []string{}
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("storage: encode account model list: %w", err)
	}
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO account_models (auth_index, models, source, synced_at, error)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(auth_index) DO UPDATE SET
				models    = excluded.models,
				source    = excluded.source,
				synced_at = excluded.synced_at,
				error     = excluded.error`,
			l.AuthIndex, string(raw), l.Source, FormatTime(l.SyncedAt), l.Error)
		if err != nil {
			return fmt.Errorf("storage: save account model list: %w", err)
		}
		return nil
	})
}

// DeleteModelList drops an account's list so the next sync refetches it. Used
// when the account disappears from CPA.
func (s *ModelListStore) DeleteModelList(ctx context.Context, authIndex string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM account_models WHERE auth_index = ?`, authIndex); err != nil {
			return fmt.Errorf("storage: delete account model list: %w", err)
		}
		// The probe toggles go too: they are keyed by the same pair, and leaving
		// them behind would silently re-arm probing if an account with the same
		// auth index ever came back.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM account_model_config WHERE auth_index = ?`, authIndex); err != nil {
			return fmt.Errorf("storage: delete account model config: %w", err)
		}
		return nil
	})
}

var _ accounts.ModelListStore = (*ModelListStore)(nil)
