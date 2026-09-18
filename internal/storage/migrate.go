package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// CurrentSchemaVersion is the schema version this build expects. Bump it in
// the same commit that appends a migration -- never edit a released migration.
const CurrentSchemaVersion = 4

// Migration is one forward-only schema step.
//
// Checksum is derived from Stmts rather than being stored by hand so that
// editing a released migration is detected at startup instead of silently
// diverging between installations (design doc 7.4.2).
type Migration struct {
	Version int
	Name    string
	Stmts   []string
}

// Checksum returns the SHA-256 of the migration's statements.
func (m Migration) Checksum() string {
	h := sha256.Sum256([]byte(strings.Join(m.Stmts, "\n--\n")))
	return hex.EncodeToString(h[:])
}

// migrations is the ordered migration chain. Append only.
//
// Note on v1/v2: v1 intentionally creates proxy_node *without* last_used_at so
// that v2 has something to add. A fresh install therefore runs 1 -> 2 -> 3 and
// lands on the schema in design doc section 4.3. Do not "tidy" this by moving
// the column into v1 -- v2 would then fail with "duplicate column name" on
// every existing installation.
var migrations = []Migration{
	{
		Version: 1,
		Name:    "init",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS settings (
				key   TEXT PRIMARY KEY,
				value TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS time_windows (
				id           TEXT PRIMARY KEY,
				label        TEXT NOT NULL,
				days_of_week TEXT NOT NULL DEFAULT '[]',
				start_time   TEXT NOT NULL,
				end_time     TEXT NOT NULL,
				enabled      INTEGER NOT NULL DEFAULT 1,
				sort_order   INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE IF NOT EXISTS account_model_config (
				auth_index    TEXT NOT NULL,
				model         TEXT NOT NULL,
				probe_enabled INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (auth_index, model)
			)`,
			`CREATE TABLE IF NOT EXISTS state_binding (
				auth_index    TEXT NOT NULL,
				model         TEXT NOT NULL,
				state_value   TEXT NOT NULL,
				state_length  INTEGER NOT NULL,
				bound_at      TEXT NOT NULL,
				expires_at    TEXT NOT NULL,
				refresh_after TEXT NOT NULL,
				source        TEXT NOT NULL,
				proxy_id      TEXT,
				PRIMARY KEY (auth_index, model)
			)`,
			`CREATE TABLE IF NOT EXISTS state_binding_history (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				auth_index   TEXT NOT NULL,
				model        TEXT NOT NULL,
				state_value  TEXT NOT NULL,
				state_length INTEGER NOT NULL,
				source       TEXT NOT NULL,
				action       TEXT NOT NULL,
				bound_at     TEXT NOT NULL,
				expires_at   TEXT,
				proxy_id     TEXT,
				created_at   TEXT NOT NULL DEFAULT (datetime('now'))
			)`,
			`CREATE TABLE IF NOT EXISTS probe_history (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				auth_index   TEXT NOT NULL,
				model        TEXT NOT NULL,
				proxy_id     TEXT,
				result       TEXT NOT NULL,
				state_length INTEGER,
				latency_ms   INTEGER,
				probed_at    TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS proxy_node (
				id                   TEXT PRIMARY KEY,
				url                  TEXT NOT NULL,
				enabled              INTEGER NOT NULL DEFAULT 1,
				success_count        INTEGER NOT NULL DEFAULT 0,
				failure_count        INTEGER NOT NULL DEFAULT 0,
				consecutive_failures INTEGER NOT NULL DEFAULT 0,
				cooldown_until       TEXT,
				last_latency_ms      INTEGER,
				last_success         TEXT,
				last_failure         TEXT
			)`,
			`CREATE TABLE IF NOT EXISTS scheduler_cursor (
				auth_index TEXT PRIMARY KEY,
				last_used  TEXT
			)`,
		},
	},
	{
		Version: 2,
		Name:    "add_proxy_last_used_at",
		Stmts: []string{
			`ALTER TABLE proxy_node ADD COLUMN last_used_at TEXT`,
		},
	},
	{
		Version: 3,
		Name:    "add_probe_history_index",
		Stmts: []string{
			`CREATE INDEX IF NOT EXISTS idx_probe_history_auth_model
				ON probe_history(auth_index, model, probed_at DESC)`,
		},
	},
	{
		Version: 4,
		Name:    "proxy_cooldown_per_account",
		Stmts: []string{
			// A proxy's health is a property of the (account, proxy) pair, not
			// of the proxy. The same node returns the target state length for
			// one account and a non-target length for another, so cooling the
			// node globally lets one account's failures take it away from every
			// other account.
			//
			// proxy_node.consecutive_failures and proxy_node.cooldown_until are
			// left in place but no longer read: shipped migrations are not
			// edited, and dropping columns means the create-new/copy/drop/rename
			// dance for data that is now meaningless.
			`CREATE TABLE IF NOT EXISTS proxy_cooldown (
				auth_index           TEXT NOT NULL,
				proxy_id             TEXT NOT NULL,
				consecutive_failures INTEGER NOT NULL DEFAULT 0,
				failure_count        INTEGER NOT NULL DEFAULT 0,
				cooldown_until       TEXT,
				last_failure         TEXT,
				PRIMARY KEY (auth_index, proxy_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_proxy_cooldown_proxy
				ON proxy_cooldown(proxy_id)`,
		},
	},
}

// Migrations returns the migration chain. Intended for tests and diagnostics.
func Migrations() []Migration {
	out := make([]Migration, len(migrations))
	copy(out, migrations)
	return out
}

// ErrSchemaTooNew means the database was written by a newer plugin build.
// We refuse to start rather than risk corrupting data we do not understand
// (design doc 7.6.2).
var ErrSchemaTooNew = errors.New("storage: database schema is newer than this plugin build")

// Migrate brings the database up to CurrentSchemaVersion.
//
// It returns the version the database was at on entry. backupDir receives a
// pre-migration snapshot when any migration is actually applied; pass an empty
// string to skip backups (tests only).
func (db *DB) Migrate(ctx context.Context, backupDir string, logf func(string, ...any)) (int, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if err := db.ensureMigrationsTable(ctx); err != nil {
		return 0, err
	}

	from, applied, err := db.readVersion(ctx)
	if err != nil {
		return 0, err
	}
	if from > CurrentSchemaVersion {
		return from, fmt.Errorf("%w: database version %d, plugin supports %d. "+
			"Upgrade the plugin, or restore an older database from the backups directory",
			ErrSchemaTooNew, from, CurrentSchemaVersion)
	}
	if from == CurrentSchemaVersion {
		return from, nil
	}

	// A previous run died mid-migration. The transaction layer guarantees no
	// partial DDL landed, so the database is still at `from`; we still restore
	// the pre-migration snapshot when one exists, because the interrupted run
	// may have been about to write something we cannot see.
	//
	// This only applies to a database that already had schema: on a fresh
	// install there is no settings table to read a marker from, and nothing to
	// recover -- a crash leaves the database at version 0 and the next start
	// simply replays every migration.
	if from > 0 {
		if state, err := db.settingValue(ctx, "migration_state"); err == nil && state == "migrating" {
			last := mustSetting(ctx, db, "last_backup_path")
			logf("previous migration was interrupted (database still at v%d), last backup: %s", from, last)
			if last != "" {
				if err := db.RestoreFrom(ctx, last, logf); err != nil {
					return from, fmt.Errorf("storage: interrupted migration and backup restore failed: %w", err)
				}
				// RestoreFrom reopens the handle; re-read the version.
				if from, _, err = db.readVersion(ctx); err != nil {
					return from, err
				}
			}
		}
	}

	if err := db.verifyChecksums(ctx, applied); err != nil {
		return from, err
	}

	pending := pendingMigrations(migrations, from)
	if len(pending) == 0 {
		return from, nil
	}

	// Only back up when there is existing schema to protect. A fresh database
	// has nothing to lose, and its settings table -- where the backup path is
	// recorded -- does not exist until migration 1 has run.
	if backupDir != "" && from > 0 {
		path, err := db.Backup(ctx, backupDir, from)
		if err != nil {
			return from, fmt.Errorf("storage: pre-migration backup: %w", err)
		}
		logf("pre-migration backup written to %s", path)
	}

	if err := db.markMigrationState(ctx, from, "migrating"); err != nil {
		return from, err
	}
	for _, m := range pending {
		if err := db.applyMigration(ctx, m); err != nil {
			// The failed migration rolled back, so the schema is untouched.
			// Recover the pre-migration snapshot so the operator gets a
			// database they can inspect, then refuse to start.
			if backupDir != "" && from > 0 {
				last := mustSetting(ctx, db, "last_backup_path")
				if last != "" {
					if rErr := db.RestoreFrom(ctx, last, logf); rErr != nil {
						return from, errors.Join(err, fmt.Errorf("storage: restore after failed migration: %w", rErr))
					}
				}
			}
			return from, fmt.Errorf("storage: migration %d (%s) failed: %w", m.Version, m.Name, err)
		}
		logf("applied migration %d (%s)", m.Version, m.Name)
	}

	if err := db.clearMigrationState(ctx); err != nil {
		return from, err
	}
	return from, nil
}

func (db *DB) applyMigration(ctx context.Context, m Migration) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range m.Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("statement failed: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES (?, ?, ?)`,
			m.Version, m.Name, m.Checksum())
		return err
	})
}

func (db *DB) ensureMigrationsTable(ctx context.Context) error {
	// Deliberately not a migration itself: the table must exist before we can
	// read a version number at all.
	_, err := db.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		checksum   TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}
	return nil
}

// readVersion returns the highest applied version plus the applied rows, so
// the caller can verify checksums without a second query.
func (db *DB) readVersion(ctx context.Context) (int, []appliedMigration, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return 0, nil, fmt.Errorf("storage: read schema_migrations: %w", err)
	}
	defer rows.Close()

	var (
		maxVersion int
		applied    []appliedMigration
	)
	for rows.Next() {
		var a appliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum); err != nil {
			return 0, nil, fmt.Errorf("storage: scan schema_migrations: %w", err)
		}
		applied = append(applied, a)
		if a.Version > maxVersion {
			maxVersion = a.Version
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("storage: iterate schema_migrations: %w", err)
	}
	return maxVersion, applied, nil
}

type appliedMigration struct {
	Version  int
	Name     string
	Checksum string
}

// verifyChecksums rejects a database whose applied migrations no longer match
// this build's migration text.
func (db *DB) verifyChecksums(ctx context.Context, applied []appliedMigration) error {
	known := make(map[int]Migration, len(migrations))
	for _, m := range migrations {
		known[m.Version] = m
	}
	for _, a := range applied {
		m, ok := known[a.Version]
		if !ok {
			return fmt.Errorf("storage: migration %d (%s) is recorded in the database but missing from this build", a.Version, a.Name)
		}
		if got := m.Checksum(); got != a.Checksum {
			return fmt.Errorf("storage: migration %d (%s) has been modified: database checksum %s, build checksum %s",
				a.Version, a.Name, a.Checksum, got)
		}
	}
	return nil
}

func pendingMigrations(all []Migration, from int) []Migration {
	var out []Migration
	for _, m := range all {
		if m.Version > from {
			out = append(out, m)
		}
	}
	return out
}

func (db *DB) settingValue(ctx context.Context, key string) (string, error) {
	var v string
	err := db.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

func mustSetting(ctx context.Context, db *DB, key string) string {
	v, err := db.settingValue(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

// markMigrationState records that a migration is in flight, so the next start
// can tell an interrupted upgrade from a clean one.
//
// The marker lives in `settings`, which migration 1 creates. A database at
// version 0 has no such table and does not need a marker: if it crashes
// mid-upgrade it is left at version 0 and the next start replays everything,
// which is exactly what an uninterrupted fresh install does.
func (db *DB) markMigrationState(ctx context.Context, from int, state string) error {
	if from == 0 {
		return nil
	}
	now := nowRFC3339()
	pairs := map[string]string{
		"migration_state":        state,
		"migration_from_version": fmt.Sprint(from),
		"migration_to_version":   fmt.Sprint(CurrentSchemaVersion),
		"migration_started_at":   now,
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		for k, v := range pairs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) clearMigrationState(ctx context.Context) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM settings WHERE key IN
			 ('migration_state','migration_from_version','migration_to_version','migration_started_at')`)
		return err
	})
}
