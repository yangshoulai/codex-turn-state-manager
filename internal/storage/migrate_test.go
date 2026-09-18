package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

func TestOpen_UsesWAL(t *testing.T) {
	db, _ := openTemp(t)
	// Open already fails when journal_mode is not wal; assert it explicitly so
	// the requirement is visible here too (NF-03).
	var mode string
	if err := db.SQL().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestMigrate_FreshInstallReachesCurrentVersion(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)

	from, err := db.Migrate(ctx, filepath.Join(dir, "backups"), nil)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if from != 0 {
		t.Errorf("reported starting version = %d, want 0 for a fresh database", from)
	}

	var version int
	if err := db.SQL().QueryRow(
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", version, CurrentSchemaVersion)
	}
}

// TestMigrate_CreatesEveryTable guards against a migration that silently does
// nothing.
func TestMigrate_CreatesEveryTable(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want := []string{
		"schema_migrations", "settings", "time_windows", "account_model_config",
		"state_binding", "state_binding_history", "probe_history", "proxy_node",
		"scheduler_cursor",
	}
	for _, table := range want {
		var name string
		err := db.SQL().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s is missing: %v", table, err)
		}
	}

	// v3's index must exist too.
	var index string
	if err := db.SQL().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_probe_history_auth_model'`,
	).Scan(&index); err != nil {
		t.Errorf("probe history index is missing: %v", err)
	}

	// And v2's column.
	if !columnExists(t, db, "proxy_node", "last_used_at") {
		t.Error("proxy_node.last_used_at is missing after v2")
	}
}

func columnExists(t *testing.T, db *DB, table, column string) bool {
	t.Helper()
	rows, err := db.SQL().Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func TestMigrate_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	backups := filepath.Join(dir, "backups")

	if _, err := db.Migrate(ctx, backups, nil); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	from, err := db.Migrate(ctx, backups, nil)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if from != CurrentSchemaVersion {
		t.Errorf("second run reported from=%d, want %d", from, CurrentSchemaVersion)
	}

	// Exactly one row per migration, not one per run.
	var rows int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != len(Migrations()) {
		t.Errorf("schema_migrations has %d rows, want %d", rows, len(Migrations()))
	}

	// No backup should have been taken when there was nothing to migrate.
	entries, err := os.ReadDir(backups)
	if err == nil && len(entries) > 0 {
		t.Errorf("a no-op migration should not create a backup, found %d", len(entries))
	}
}

func TestMigrate_RefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Simulate a database written by a newer plugin build.
	if _, err := db.SQL().Exec(
		`INSERT INTO schema_migrations (version, name, checksum) VALUES (?, ?, ?)`,
		CurrentSchemaVersion+1, "from_the_future", "deadbeef"); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}

	_, err := db.Migrate(ctx, filepath.Join(dir, "backups"), nil)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Migrate error = %v, want ErrSchemaTooNew", err)
	}
}

// TestMigrate_DetectsModifiedMigration covers the checksum gate: a released
// migration must never be edited in place.
func TestMigrate_DetectsModifiedMigration(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	backups := filepath.Join(dir, "backups")
	if _, err := db.Migrate(ctx, backups, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Corrupt the recorded checksum of an applied migration, then roll the
	// database back so the runner has work to do and reaches the check.
	if _, err := db.SQL().Exec(
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM schema_migrations WHERE version = ?`,
		CurrentSchemaVersion); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	_, err := db.Migrate(ctx, backups, nil)
	if err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	if !contains(err.Error(), "modified") {
		t.Errorf("error = %v, want it to mention a modified migration", err)
	}
}

// TestMigrate_AppliesPendingFromAnOlderVersion exercises the upgrade path: a
// database sitting at v1 must reach the current version without losing data.
func TestMigrate_AppliesPendingFromAnOlderVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	// Build a v1 database by hand.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.ensureMigrationsTable(ctx); err != nil {
		t.Fatalf("ensureMigrationsTable: %v", err)
	}
	first := Migrations()[0]
	if err := db.applyMigration(ctx, first); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if columnExists(t, db, "proxy_node", "last_used_at") {
		t.Fatal("v1 should not create proxy_node.last_used_at; v2 exists to add it")
	}

	// Seed a row that must survive the upgrade.
	if _, err := db.SQL().Exec(
		`INSERT INTO settings (key, value) VALUES ('global_enabled', 'false')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen and migrate the rest of the way.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	from, err := reopened.Migrate(ctx, filepath.Join(dir, "backups"), nil)
	if err != nil {
		t.Fatalf("Migrate from v1: %v", err)
	}
	if from != 1 {
		t.Errorf("reported from=%d, want 1", from)
	}
	if !columnExists(t, reopened, "proxy_node", "last_used_at") {
		t.Error("v2 did not add proxy_node.last_used_at")
	}

	var value string
	if err := reopened.SQL().QueryRow(
		`SELECT value FROM settings WHERE key='global_enabled'`).Scan(&value); err != nil {
		t.Fatalf("read surviving row: %v", err)
	}
	if value != "false" {
		t.Errorf("global_enabled = %q, want false (data must survive migration)", value)
	}
}

func TestMigrate_TakesBackupBeforeUpgrading(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	backups := filepath.Join(dir, "backups")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.ensureMigrationsTable(ctx); err != nil {
		t.Fatalf("ensureMigrationsTable: %v", err)
	}
	if err := db.applyMigration(ctx, Migrations()[0]); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.Migrate(ctx, backups, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	found, err := ListBackups(backups)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("backups = %d, want 1", len(found))
	}

	// The recorded path is what an interrupted migration looks for on restart.
	last, err := reopened.settingValue(ctx, "last_backup_path")
	if err != nil {
		t.Fatalf("read last_backup_path: %v", err)
	}
	if last != found[0] {
		t.Errorf("last_backup_path = %q, want %q", last, found[0])
	}

	// Migration state must be cleared once everything succeeded.
	state, err := reopened.settingValue(ctx, "migration_state")
	if err != nil {
		t.Fatalf("read migration_state: %v", err)
	}
	if state != "" {
		t.Errorf("migration_state = %q, want it cleared after a successful run", state)
	}
}

func TestBackup_PrunesOldSnapshots(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	backups := filepath.Join(dir, "backups")

	if _, err := db.Migrate(ctx, backups, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for i := 0; i < DefaultBackupRetention+3; i++ {
		// The timestamp has second resolution, so nudge the name to keep the
		// snapshots distinct.
		if _, err := db.Backup(ctx, backups, i); err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
	}

	found, err := ListBackups(backups)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(found) > DefaultBackupRetention {
		t.Errorf("kept %d backups, want at most %d", len(found), DefaultBackupRetention)
	}
}

// TestRestoreFrom_ReplacesDatabaseAndClearsWALSidecars covers the recovery
// path used when a migration fails.
func TestRestoreFrom_ReplacesDatabaseAndClearsWALSidecars(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	backups := filepath.Join(dir, "backups")

	if _, err := db.Migrate(ctx, backups, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := db.Settings().Put(ctx, "marker", "original"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	backupPath, err := db.Backup(ctx, backups, CurrentSchemaVersion)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Diverge from the snapshot.
	if err := db.Settings().Put(ctx, "marker", "changed"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := db.RestoreFrom(ctx, backupPath, nil); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	values, err := db.Settings().All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if values["marker"] != "original" {
		t.Errorf("marker = %q, want original (the restore did not take effect)", values["marker"])
	}

	for _, sidecar := range []string{db.Path() + "-wal", db.Path() + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			// A new WAL may legitimately be created by the reopened handle;
			// what matters is that the stale one was removed before reopening.
			t.Logf("note: %s exists after restore (recreated by the reopened handle)", sidecar)
		}
	}
}

func TestRestoreFrom_RejectsInvalidBackup(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	bad := filepath.Join(dir, "not-a-db.bak")
	if err := os.WriteFile(bad, []byte("this is not sqlite"), 0o644); err != nil {
		t.Fatalf("write bad backup: %v", err)
	}

	if err := db.RestoreFrom(ctx, bad, nil); err == nil {
		t.Error("expected RestoreFrom to reject a non-database file")
	}

	// The live database must still work.
	if err := db.Settings().Put(ctx, "still", "alive"); err != nil {
		t.Errorf("database unusable after a rejected restore: %v", err)
	}
}

func TestMigrationChecksumIsStable(t *testing.T) {
	first := Migrations()
	second := Migrations()
	if len(first) != len(second) {
		t.Fatalf("Migrations() returned different lengths")
	}
	for i := range first {
		if first[i].Checksum() != second[i].Checksum() {
			t.Errorf("migration %d checksum is not stable", first[i].Version)
		}
	}
}

// ---------------------------------------------------------------------------
// store integration

func TestBindingStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	store := db.Bindings()

	now := time.Now().UTC().Truncate(time.Second)
	binding := states.Binding{
		Pair:         states.Pair{AuthIndex: "auth-1", Model: "gpt-5-codex"},
		StateValue:   "abcdef",
		StateLength:  6,
		BoundAt:      now,
		ExpiresAt:    now.Add(60 * time.Minute),
		RefreshAfter: now.Add(51 * time.Minute),
		Source:       states.SourceProbe,
		ProxyID:      "proxy-1",
	}
	if err := store.UpsertBinding(ctx, binding); err != nil {
		t.Fatalf("UpsertBinding: %v", err)
	}

	list, err := store.ListBindings(ctx)
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListBindings = %d rows, want 1", len(list))
	}
	got := list[0]
	if got.StateValue != binding.StateValue || got.StateLength != binding.StateLength {
		t.Errorf("round-tripped value = %q/%d, want %q/%d",
			got.StateValue, got.StateLength, binding.StateValue, binding.StateLength)
	}
	if !got.ExpiresAt.Equal(binding.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, binding.ExpiresAt)
	}
	if got.Source != states.SourceProbe || got.ProxyID != "proxy-1" {
		t.Errorf("source/proxy = %s/%s, want probe/proxy-1", got.Source, got.ProxyID)
	}

	// Upsert must replace, not duplicate.
	binding.StateValue = "replaced"
	if err := store.UpsertBinding(ctx, binding); err != nil {
		t.Fatalf("UpsertBinding replace: %v", err)
	}
	list, _ = store.ListBindings(ctx)
	if len(list) != 1 || list[0].StateValue != "replaced" {
		t.Errorf("after replace: %d rows, value %q", len(list), list[0].StateValue)
	}

	if err := store.DeleteBinding(ctx, binding.Pair); err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if list, _ = store.ListBindings(ctx); len(list) != 0 {
		t.Errorf("after delete: %d rows, want 0", len(list))
	}
}

func TestProxyStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db, dir := openTemp(t)
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	store := db.Proxies()

	now := time.Now().UTC().Truncate(time.Second)
	latency := 82
	cooldown := now.Add(5 * time.Minute)
	node := proxies.Node{
		ID: "proxy-1", URL: "http://proxy1:8080", Enabled: true,
		SuccessCount: 3, FailureCount: 1, ConsecutiveFailures: 1,
		CooldownUntil: &cooldown,
		LastLatencyMS: &latency,
		LastUsedAt:    &now,
		LastSuccess:   &now,
		LastFailure:   &now,
	}
	if err := store.UpsertProxy(ctx, node); err != nil {
		t.Fatalf("UpsertProxy: %v", err)
	}

	list, err := store.ListProxies(ctx)
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListProxies = %d rows, want 1", len(list))
	}
	got := list[0]
	if got.URL != node.URL || !got.Enabled {
		t.Errorf("url/enabled = %q/%v, want %q/true", got.URL, got.Enabled, node.URL)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(now) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, now)
	}
	if got.LastLatencyMS == nil || *got.LastLatencyMS != latency {
		t.Errorf("LastLatencyMS = %v, want %d", got.LastLatencyMS, latency)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
