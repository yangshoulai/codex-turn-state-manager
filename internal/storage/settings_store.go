package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// SettingsStore is the key/value settings table.
type SettingsStore struct{ db *DB }

// Settings returns the settings store.
func (db *DB) Settings() *SettingsStore { return &SettingsStore{db: db} }

// All returns every settings row.
func (s *SettingsStore) All(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.sql.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("storage: list settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("storage: scan settings: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Put writes a single setting.
func (s *SettingsStore) Put(ctx context.Context, key, value string) error {
	return s.db.setSettings(ctx, map[string]string{key: value})
}

// PutMany writes several settings in one transaction.
func (s *SettingsStore) PutMany(ctx context.Context, values map[string]string) error {
	return s.db.setSettings(ctx, values)
}

func (db *DB) setSettings(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		for k, v := range values {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return fmt.Errorf("storage: write setting %q: %w", k, err)
			}
		}
		return nil
	})
}
