package accounts

import (
	"context"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
)

// stubStore is a ConfigStore with no rows: these tests are about the account
// cache, not about model configuration.
type stubStore struct{}

func (stubStore) ListAccountModels(context.Context) ([]Config, error) { return nil, nil }
func (stubStore) UpsertAccountModel(context.Context, Config) error    { return nil }
func (stubStore) DeleteAccountModel(context.Context, string, string) error {
	return nil
}

func newRegistry(t *testing.T, accounts int) (*Registry, *hostapi.MockHost) {
	t.Helper()
	host := hostapi.NewMockHost()
	for i := 0; i < accounts; i++ {
		host.AddAccount(hostapi.Account{
			AuthIndex: "codex-auth-" + string(rune('1'+i)),
			AuthID:    "auth-id-" + string(rune('1'+i)),
			Provider:  hostapi.ProviderCodex,
			Label:     "user" + string(rune('1'+i)),
			Status:    hostapi.AccountStatusActive,
		})
	}
	// A nil PlanReader makes resolvePlans a no-op, which keeps the credential
	// path out of a test that is about the in-memory cache.
	return NewRegistry(host, stubStore{}, nil, nil, nil), host
}

func TestRegistry_RecordSignals(t *testing.T) {
	reg, _ := newRegistry(t, 1)
	ctx := context.Background()
	if _, err := reg.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	used := 42
	resetAt := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	reg.RecordSignals("codex-auth-1", headers.Signals{
		PlanType:             "free",
		PrimaryUsedPercent:   &used,
		PrimaryWindowMinutes: intPtr(300),
		PrimaryResetAt:       &resetAt,
	})

	acc, ok := reg.Get("codex-auth-1")
	if !ok {
		t.Fatal("account disappeared")
	}
	if acc.Quota == nil {
		t.Fatal("quota was not stored")
	}
	if acc.Quota.PrimaryUsedPercent == nil || *acc.Quota.PrimaryUsedPercent != 42 {
		t.Errorf("primary used percent = %v, want 42", acc.Quota.PrimaryUsedPercent)
	}
	if acc.Plan.Type != "free" {
		t.Errorf("plan type = %q, want free", acc.Plan.Type)
	}

	// An empty signal set must not erase what is already known: the probe path
	// and the traffic path both call this, and one of them may observe nothing.
	reg.RecordSignals("codex-auth-1", headers.Signals{})
	if acc, _ := reg.Get("codex-auth-1"); acc.Quota == nil {
		t.Error("an empty signal set erased the stored quota")
	}

	// An unknown account is ignored rather than creating a row.
	reg.RecordSignals("nope", headers.Signals{PlanType: "pro"})
	if _, ok := reg.Get("nope"); ok {
		t.Error("RecordSignals created an account")
	}
}

// TestRegistry_SyncKeepsSignalsHarvestedFromTraffic is the regression guard for
// a defect that was invisible until a live request had been made.
//
// Sync rebuilds the account map from the host's list, which carries no plan and
// no quota. It carried the Plan over from the previous entry and forgot the
// Quota, so every five-minute sync silently blanked the rate-limit badges --
// the plan survived, which made it look intentional rather than broken.
func TestRegistry_SyncKeepsSignalsHarvestedFromTraffic(t *testing.T) {
	reg, _ := newRegistry(t, 1)
	ctx := context.Background()
	if _, err := reg.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	used := 7
	secondary := 3
	reg.RecordSignals("codex-auth-1", headers.Signals{
		PlanType:               "plus",
		PrimaryUsedPercent:     &used,
		SecondaryUsedPercent:   &secondary,
		PrimaryWindowMinutes:   intPtr(300),
		SecondaryWindowMinutes: intPtr(10080),
	})

	if _, err := reg.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	acc, ok := reg.Get("codex-auth-1")
	if !ok {
		t.Fatal("account disappeared across sync")
	}
	if acc.Plan.Type != "plus" {
		t.Errorf("plan type = %q, want plus", acc.Plan.Type)
	}
	if acc.Quota == nil {
		t.Fatal("sync dropped the quota the upstream had reported")
	}
	if acc.Quota.PrimaryUsedPercent == nil || *acc.Quota.PrimaryUsedPercent != 7 {
		t.Errorf("primary used percent = %v, want 7", acc.Quota.PrimaryUsedPercent)
	}
	if acc.Quota.SecondaryUsedPercent == nil || *acc.Quota.SecondaryUsedPercent != 3 {
		t.Errorf("secondary used percent = %v, want 3", acc.Quota.SecondaryUsedPercent)
	}
}

func intPtr(n int) *int { return &n }
