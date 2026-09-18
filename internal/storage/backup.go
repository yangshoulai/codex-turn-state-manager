package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TimestampLayout is the wire format for every time column. Stored as UTC
// RFC3339 so that lexicographic ordering in SQL matches chronological order,
// which lets the proxy pool sort by last_used_at in a single UPDATE/ORDER BY.
const TimestampLayout = time.RFC3339Nano

func nowRFC3339() string { return time.Now().UTC().Format(TimestampLayout) }

// FormatTime renders t for storage.
func FormatTime(t time.Time) string { return t.UTC().Format(TimestampLayout) }

// FormatTimePtr renders a nullable time column.
func FormatTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return FormatTime(*t)
}

// ParseTime reads a time column. Empty/NULL yields the zero time.
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(TimestampLayout, s)
	if err != nil {
		// Tolerate rows written by SQLite's own datetime('now') default.
		if t2, err2 := time.Parse("2006-01-02 15:04:05", s); err2 == nil {
			return t2.UTC()
		}
		return time.Time{}
	}
	return t.UTC()
}

// ParseNullTime reads a nullable time column.
func ParseNullTime(ns sql.NullString) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return ParseTime(ns.String)
}

// DefaultBackupRetention is how many pre-migration snapshots to keep.
const DefaultBackupRetention = 5

// Backup writes a consistent snapshot of the database into dir using
// VACUUM INTO, which is WAL-safe and produces a compacted copy.
//
// It also records last_backup_path / last_backup_at in settings so an
// interrupted migration can find the snapshot on the next start.
func (db *DB) Backup(ctx context.Context, dir string, fromVersion int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("storage: create backup dir: %w", err)
	}
	name := fmt.Sprintf("state.db.v%d.%s.bak", fromVersion, time.Now().UTC().Format("20060102T150405Z"))
	dest := filepath.Join(dir, name)

	// VACUUM INTO refuses to overwrite, which is the behaviour we want.
	if _, err := db.sql.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("storage: vacuum into %s: %w", dest, err)
	}

	if err := db.setSettings(ctx, map[string]string{
		"last_backup_path": dest,
		"last_backup_at":   nowRFC3339(),
	}); err != nil {
		return "", err
	}

	if err := pruneBackups(dir, DefaultBackupRetention); err != nil {
		// Retention is housekeeping; a failure here must not sink the backup.
		return dest, nil
	}
	return dest, nil
}

// ListBackups returns backup files in dir, newest first.
func ListBackups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	// Names embed a sortable UTC timestamp, so descending name order is
	// descending age.
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

func pruneBackups(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	backups, err := ListBackups(dir)
	if err != nil {
		return err
	}
	for _, path := range backups[min(keep, len(backups)):] {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Order matters: the handle is closed first, then the -wal/-shm sidecars are
// removed so SQLite cannot replay a WAL belonging to the discarded database,
// then the backup is copied into place and the handle reopened.
// RestoreFrom replaces the live database with a backup.
func (db *DB) RestoreFrom(ctx context.Context, backupPath string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("storage: backup %s is not readable: %w", backupPath, err)
	}

	// VACUUM INTO output is a plain database file; let sqlite validate it
	// before we destroy the current one.
	check, err := sql.Open("sqlite", "file:"+backupPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("storage: open backup for validation: %w", err)
	}
	if err := check.Ping(); err != nil {
		_ = check.Close()
		return fmt.Errorf("storage: backup %s is not a valid database: %w", backupPath, err)
	}
	_ = check.Close()

	if err := db.sql.Close(); err != nil {
		return fmt.Errorf("storage: close before restore: %w", err)
	}

	for _, sidecar := range []string{db.path + "-wal", db.path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			logf("could not remove %s: %v", sidecar, err)
		}
	}

	if err := copyFile(backupPath, db.path); err != nil {
		return fmt.Errorf("storage: copy backup into place: %w", err)
	}

	reopened, err := Open(db.path)
	if err != nil {
		return fmt.Errorf("storage: reopen after restore: %w", err)
	}
	db.sql = reopened.sql
	logf("database restored from %s", backupPath)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".restore.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
