package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/accounts"
	"github.com/yangshoulai/codex-turn-state-manager/internal/callhistory"
)

// migrated opens a fresh database at the current schema.
func migrated(t *testing.T) *DB {
	t.Helper()
	db, dir := openTemp(t)
	if _, err := db.Migrate(context.Background(), filepath.Join(dir, "backups"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestCallStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store := migrated(t).Calls()
	at := time.Date(2026, 9, 19, 10, 30, 0, 0, time.UTC)

	rows := []callhistory.Record{
		{AuthIndex: "auth-a", Model: "gpt-5.6-sol", RequestID: "r1",
			CarriedState: "CARRIED", InjectedState: "INJECTED", ResponseState: "RETURNED",
			StatusCode: 200, Outcome: "succeeded", CreatedAt: at},
		{AuthIndex: "auth-a", Model: "gpt-5.5", RequestID: "r2",
			StatusCode: 429, Outcome: "failed", CreatedAt: at.Add(time.Second)},
		{AuthIndex: "auth-b", Model: "gpt-5.6-sol", RequestID: "r3",
			StatusCode: 500, Outcome: "failed", CreatedAt: at.Add(2 * time.Second)},
	}
	if err := store.AppendCalls(ctx, rows); err != nil {
		t.Fatalf("AppendCalls: %v", err)
	}

	all, err := store.ListCalls(ctx, callhistory.Query{})
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("rows = %d, want 3", len(all))
	}
	// Newest first.
	if all[0].RequestID != "r3" {
		t.Errorf("first row = %q, want the newest (r3)", all[0].RequestID)
	}

	// The three state values are the reason the table exists; a truncating
	// column type would silently ruin it.
	var found bool
	for _, r := range all {
		if r.RequestID != "r1" {
			continue
		}
		found = true
		if r.CarriedState != "CARRIED" || r.InjectedState != "INJECTED" || r.ResponseState != "RETURNED" {
			t.Errorf("states = %q / %q / %q", r.CarriedState, r.InjectedState, r.ResponseState)
		}
		if r.CarriedLength() != 7 || r.InjectedLength() != 8 || r.ResponseLength() != 8 {
			t.Errorf("lengths = %d / %d / %d",
				r.CarriedLength(), r.InjectedLength(), r.ResponseLength())
		}
		if !r.CreatedAt.Equal(at) {
			t.Errorf("CreatedAt = %v, want %v", r.CreatedAt, at)
		}
	}
	if !found {
		t.Fatal("the row carrying the state values was not read back")
	}

	filtered, err := store.ListCalls(ctx, callhistory.Query{AuthIndex: "auth-a"})
	if err != nil {
		t.Fatalf("ListCalls filtered: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("filtered rows = %d, want 2", len(filtered))
	}

	n, err := store.CountCalls(ctx, callhistory.Query{AuthIndex: "auth-a"})
	if err != nil {
		t.Fatalf("CountCalls: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestCallStore_Paging(t *testing.T) {
	ctx := context.Background()
	store := migrated(t).Calls()
	base := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)

	var rows []callhistory.Record
	for i := 0; i < 10; i++ {
		rows = append(rows, callhistory.Record{
			AuthIndex: "a", Model: "m", RequestID: string(rune('a' + i)),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	if err := store.AppendCalls(ctx, rows); err != nil {
		t.Fatalf("AppendCalls: %v", err)
	}

	page, err := store.ListCalls(ctx, callhistory.Query{Limit: 3, Offset: 2})
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("page size = %d, want 3", len(page))
	}
	// Offsets count back from the newest, so page 2 starting at offset 2 holds
	// the 3rd, 4th and 5th newest: h, g, f.
	if page[0].RequestID != "h" || page[2].RequestID != "f" {
		t.Errorf("page = %q..%q, want h..f", page[0].RequestID, page[2].RequestID)
	}

	// The count ignores paging, which is what makes a page count possible.
	total, err := store.CountCalls(ctx, callhistory.Query{Limit: 3, Offset: 2})
	if err != nil {
		t.Fatalf("CountCalls: %v", err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
}

func TestCallStore_PrunesByAge(t *testing.T) {
	ctx := context.Background()
	store := migrated(t).Calls()
	now := time.Now().UTC()

	if err := store.AppendCalls(ctx, []callhistory.Record{
		{AuthIndex: "a", Model: "m", RequestID: "old", CreatedAt: now.Add(-25 * time.Hour)},
		{AuthIndex: "a", Model: "m", RequestID: "new", CreatedAt: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("AppendCalls: %v", err)
	}

	n, err := store.PruneCallsBefore(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneCallsBefore: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}

	left, err := store.ListCalls(ctx, callhistory.Query{})
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(left) != 1 || left[0].RequestID != "new" {
		t.Errorf("left = %v, want only the recent row", left)
	}
}

func TestCallStore_DeletesOneAccount(t *testing.T) {
	ctx := context.Background()
	store := migrated(t).Calls()
	now := time.Now().UTC()

	if err := store.AppendCalls(ctx, []callhistory.Record{
		{AuthIndex: "a", Model: "m", CreatedAt: now},
		{AuthIndex: "b", Model: "m", CreatedAt: now},
	}); err != nil {
		t.Fatalf("AppendCalls: %v", err)
	}

	n, err := store.DeleteCallsForAccount(ctx, "a")
	if err != nil {
		t.Fatalf("DeleteCallsForAccount: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	if got, _ := store.CountCalls(ctx, callhistory.Query{AuthIndex: "b"}); got != 1 {
		t.Errorf("account b lost %d rows", 1-got)
	}
}

func TestModelListStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store := migrated(t).ModelLists()
	at := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)

	if err := store.SaveModelList(ctx, accounts.ModelList{
		AuthIndex: "auth-a",
		Models:    []string{"gpt-5.6-sol", "gpt-5.5"},
		Source:    accounts.ModelSourceUpstream,
		SyncedAt:  at,
	}); err != nil {
		t.Fatalf("SaveModelList: %v", err)
	}

	lists, err := store.ListModelLists(ctx)
	if err != nil {
		t.Fatalf("ListModelLists: %v", err)
	}
	if len(lists) != 1 {
		t.Fatalf("lists = %d, want 1", len(lists))
	}
	got := lists[0]
	if len(got.Models) != 2 || got.Models[0] != "gpt-5.6-sol" {
		t.Errorf("models = %v", got.Models)
	}
	if got.Source != accounts.ModelSourceUpstream {
		t.Errorf("source = %q", got.Source)
	}
	if !got.SyncedAt.Equal(at) {
		t.Errorf("syncedAt = %v, want %v", got.SyncedAt, at)
	}

	// A second save replaces rather than appending: one row per account is the
	// contract that makes "does this account have a list" a single lookup.
	if err := store.SaveModelList(ctx, accounts.ModelList{
		AuthIndex: "auth-a",
		Models:    []string{"gpt-5.6-luna"},
		Source:    accounts.ModelSourceUpstream,
		SyncedAt:  at.Add(time.Hour),
		Error:     "boom",
	}); err != nil {
		t.Fatalf("SaveModelList (replace): %v", err)
	}
	lists, _ = store.ListModelLists(ctx)
	if len(lists) != 1 {
		t.Fatalf("lists = %d after a replace, want 1", len(lists))
	}
	if len(lists[0].Models) != 1 || lists[0].Error != "boom" {
		t.Errorf("replace did not take: %+v", lists[0])
	}
}

// TestModelListStore_DeleteAlsoDropsProbeToggles pins the coupling: the two
// tables are keyed by the same pair, and leaving the toggles behind would
// silently re-arm probing if the account ever came back.
func TestModelListStore_DeleteAlsoDropsProbeToggles(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	ctxStore := db.AccountModels()

	if err := ctxStore.UpsertAccountModel(ctx, accounts.Config{
		AuthIndex: "auth-a", Model: "m", ProbeEnabled: true,
	}); err != nil {
		t.Fatalf("UpsertAccountModel: %v", err)
	}
	if err := db.ModelLists().SaveModelList(ctx, accounts.ModelList{
		AuthIndex: "auth-a", Models: []string{"m"}, SyncedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveModelList: %v", err)
	}

	if err := db.ModelLists().DeleteModelList(ctx, "auth-a"); err != nil {
		t.Fatalf("DeleteModelList: %v", err)
	}

	cfgs, err := ctxStore.ListAccountModels(ctx)
	if err != nil {
		t.Fatalf("ListAccountModels: %v", err)
	}
	if len(cfgs) != 0 {
		t.Errorf("probe toggles survived: %v", cfgs)
	}
	lists, _ := db.ModelLists().ListModelLists(ctx)
	if len(lists) != 0 {
		t.Errorf("model list survived: %v", lists)
	}
}

// TestMigrate_V5TablesExist guards the schema itself, so a future edit that
// drops a column or renames a table fails here rather than at runtime.
func TestMigrate_V5TablesExist(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)

	for _, table := range []string{"account_models", "call_history"} {
		var name string
		err := db.SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s is missing: %v", table, err)
		}
	}
}
