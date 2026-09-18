// Package storage owns SQLite persistence: connection setup, schema
// migration, backup/restore and the per-table stores.
//
// SQLite is never touched on the request hot path. Hot-path reads are served
// from the immutable in-memory snapshot held by internal/states (NF-01), which
// is why writes can be serialised behind a single mutex here without affecting
// request latency.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	// modernc.org/sqlite is a pure-Go driver. We deliberately avoid the CGO
	// (mattn/go-sqlite3) driver: the plugin itself is a C-ABI shared library
	// loaded into the CPA process, and a second copy of the sqlite3 C symbols
	// inside that library risks interposition with whatever the host links.
	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB with the plugin's write serialisation and lifecycle.
type DB struct {
	path string
	sql  *sql.DB

	// writeMu serialises write transactions. SQLite allows one writer at a
	// time; taking the lock in-process turns would-be SQLITE_BUSY retries into
	// a plain queue, which matters because probe workers and the management
	// API write concurrently.
	writeMu sync.Mutex
}

// Open opens (creating if necessary) the database at path in WAL mode.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("storage: create data dir: %w", err)
		}
	}

	dsn := "file:" + path + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}

	// WAL still admits one writer at a time. A modest pool lets management
	// reads proceed while a probe history write is in flight.
	sqldb.SetMaxOpenConns(8)
	sqldb.SetMaxIdleConns(8)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("storage: ping %s: %w", path, err)
	}

	db := &DB{path: path, sql: sqldb}
	if err := db.verifyWAL(context.Background()); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) verifyWAL(ctx context.Context) error {
	var mode string
	if err := db.sql.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("storage: read journal_mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("storage: expected journal_mode=wal, got %q", mode)
	}
	return nil
}

// Path returns the on-disk database path.
func (db *DB) Path() string { return db.path }

// SQL exposes the underlying handle for stores in this package. Callers
// outside storage should use a typed store instead.
func (db *DB) SQL() *sql.DB { return db.sql }

// Read runs fn against the database without taking the write lock.
func (db *DB) Read(ctx context.Context, fn func(*sql.DB) error) error {
	return fn(db.sql)
}

// Write runs fn inside a serialised write transaction. The transaction is
// rolled back if fn returns an error, and committed otherwise.
func (db *DB) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("storage: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// Close closes the database handle.
func (db *DB) Close() error {
	if db.sql == nil {
		return nil
	}
	return db.sql.Close()
}
