package accounts

import (
	"context"
	"sync"
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
	return NewRegistry(RegistryConfig{Host: host, Store: stubStore{}}), host
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

// ---------------------------------------------------------------------------
// per-account model lists

// memModelStore is a ModelListStore with no disk behind it.
type memModelStore struct {
	mu    sync.Mutex
	lists map[string]ModelList
	saved int
}

func newMemModelStore() *memModelStore {
	return &memModelStore{lists: map[string]ModelList{}}
}

func (s *memModelStore) ListModelLists(context.Context) ([]ModelList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ModelList, 0, len(s.lists))
	for _, l := range s.lists {
		out = append(out, l)
	}
	return out, nil
}

func (s *memModelStore) SaveModelList(_ context.Context, l ModelList) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists[l.AuthIndex] = l
	s.saved++
	return nil
}

func (s *memModelStore) DeleteModelList(_ context.Context, authIndex string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lists, authIndex)
	return nil
}

func (s *memModelStore) get(authIndex string) (ModelList, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lists[authIndex]
	return l, ok
}

// stubFetcher returns a fixed list, or an error.
type stubFetcher struct {
	models []string
	err    error
	calls  int
}

func (f *stubFetcher) FetchModels(context.Context, string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]string{}, f.models...), nil
}

// newModelRegistry builds a registry with a catalog fallback and one account.
func newModelRegistry(t *testing.T, catalog []string) (*Registry, *memModelStore) {
	t.Helper()
	host := hostapi.NewMockHost()
	host.AddAccount(hostapi.Account{
		AuthIndex: "auth-a", AuthID: "id-a",
		Provider: hostapi.ProviderCodex, Label: "a",
		Status: hostapi.AccountStatusActive,
	})
	store := newMemModelStore()
	reg := NewRegistry(RegistryConfig{
		Host:          host,
		Store:         stubStore{},
		ModelStore:    store,
		CatalogModels: func() []string { return catalog },
	})
	ctx := context.Background()
	if err := reg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := reg.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return reg, store
}

// TestModels_FallsBackToTheCatalogBeforeTheFirstFetch pins the degraded state:
// the shared manifest is the union of every tier, so it over-approximates any
// one account, but an over-approximation beats an empty list.
func TestModels_FallsBackToTheCatalogBeforeTheFirstFetch(t *testing.T) {
	reg, _ := newModelRegistry(t, []string{"gpt-5.5", "gpt-5.6-sol"})

	got, ok := reg.Models("auth-a")
	if !ok {
		t.Fatal("account is not known")
	}
	if len(got) != 2 {
		t.Fatalf("models = %v, want the two catalog entries", got)
	}

	list, own := reg.ModelList("auth-a")
	if own {
		t.Error("ModelList reported an account list before one was fetched")
	}
	if list.Source != ModelSourceCatalog {
		t.Errorf("Source = %q, want %q", list.Source, ModelSourceCatalog)
	}
}

// TestLoadModels_PrefersTheAccountsOwnList is the point of the whole feature: a
// Team account and a Plus account do not offer the same models, and the union
// of every tier is wrong for both.
func TestLoadModels_PrefersTheAccountsOwnList(t *testing.T) {
	reg, store := newModelRegistry(t, []string{"catalog-only"})
	fetcher := &stubFetcher{models: []string{"gpt-5.6-sol"}}
	reg.SetModelFetcher(fetcher)

	list, err := reg.LoadModels(context.Background(), "auth-a", false)
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if list.Source != ModelSourceUpstream {
		t.Errorf("Source = %q, want %q", list.Source, ModelSourceUpstream)
	}
	if list.SyncedAt.IsZero() {
		t.Error("SyncedAt was not stamped")
	}

	got, _ := reg.Models("auth-a")
	if len(got) != 1 || got[0].Model != "gpt-5.6-sol" {
		t.Errorf("models = %v, want the account's own list", got)
	}
	if _, ok := store.get("auth-a"); !ok {
		t.Error("the list was not persisted")
	}
}

// TestLoadModels_SkipsAnAccountThatAlreadyHasOne pins the cost rule: the list
// is read from upstream against the account's own quota, so only a first load
// or a deliberate refresh pays for it.
func TestLoadModels_SkipsAnAccountThatAlreadyHasOne(t *testing.T) {
	reg, _ := newModelRegistry(t, nil)
	fetcher := &stubFetcher{models: []string{"gpt-5.6-sol"}}
	reg.SetModelFetcher(fetcher)
	ctx := context.Background()

	if _, err := reg.LoadModels(ctx, "auth-a", false); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if _, err := reg.LoadModels(ctx, "auth-a", false); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if fetcher.calls != 1 {
		t.Errorf("fetches = %d, want 1: an existing list must not be refetched", fetcher.calls)
	}

	if _, err := reg.LoadModels(ctx, "auth-a", true); err != nil {
		t.Fatalf("forced load: %v", err)
	}
	if fetcher.calls != 2 {
		t.Errorf("fetches = %d, want 2 after a forced refresh", fetcher.calls)
	}
}

// TestLoadModels_KeepsTheOldListWhenTheFetchFails: a stale list is worth more
// than an empty one, and the failure is recorded so the panel can say why the
// list is old rather than presenting it as current.
func TestLoadModels_KeepsTheOldListWhenTheFetchFails(t *testing.T) {
	reg, _ := newModelRegistry(t, nil)
	fetcher := &stubFetcher{models: []string{"gpt-5.6-sol"}}
	reg.SetModelFetcher(fetcher)
	ctx := context.Background()

	if _, err := reg.LoadModels(ctx, "auth-a", false); err != nil {
		t.Fatalf("first load: %v", err)
	}

	fetcher.err = context.DeadlineExceeded
	list, err := reg.LoadModels(ctx, "auth-a", true)
	if err == nil {
		t.Fatal("want the fetch error back")
	}
	if len(list.Models) != 1 || list.Models[0] != "gpt-5.6-sol" {
		t.Errorf("models = %v, want the previous list", list.Models)
	}
	if list.Error == "" {
		t.Error("the failure was not recorded on the list")
	}

	got, _ := reg.Models("auth-a")
	if len(got) != 1 {
		t.Errorf("the failed refresh replaced a working list with %v", got)
	}
}

func TestLoadModels_RefusesAnUnknownAccount(t *testing.T) {
	reg, _ := newModelRegistry(t, nil)
	reg.SetModelFetcher(&stubFetcher{models: []string{"m"}})
	if _, err := reg.LoadModels(context.Background(), "nobody", false); err == nil {
		t.Fatal("want an error for an account the registry does not track")
	}
}

func TestMissingModelLists(t *testing.T) {
	reg, _ := newModelRegistry(t, nil)
	ctx := context.Background()

	if got := reg.MissingModelLists(); len(got) != 1 || got[0] != "auth-a" {
		t.Fatalf("MissingModelLists = %v, want [auth-a]", got)
	}
	reg.SetModelFetcher(&stubFetcher{models: []string{"m"}})
	if _, err := reg.LoadModels(ctx, "auth-a", false); err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if got := reg.MissingModelLists(); len(got) != 0 {
		t.Errorf("MissingModelLists = %v, want none", got)
	}
}

// TestForget_DropsEverythingKeyedByTheAccount: a removed account's bindings and
// probe toggles would otherwise be resurrected if the same auth index came back
// with different credentials.
func TestForget_DropsEverythingKeyedByTheAccount(t *testing.T) {
	reg, store := newModelRegistry(t, nil)
	ctx := context.Background()
	reg.SetModelFetcher(&stubFetcher{models: []string{"m"}})
	if _, err := reg.LoadModels(ctx, "auth-a", false); err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if err := reg.SetProbeEnabled(ctx, "auth-a", "m", true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}

	reg.Forget(ctx, "auth-a")

	if _, ok := reg.Get("auth-a"); ok {
		t.Error("the account is still tracked")
	}
	if reg.ProbeEnabled("auth-a", "m") {
		t.Error("the probe toggle survived removal")
	}
	if _, ok := store.get("auth-a"); ok {
		t.Error("the stored model list survived removal")
	}
	if _, ok := reg.Models("auth-a"); ok {
		t.Error("Models still answers for a removed account")
	}
}

// TestSync_DropsAccountsCPANoLongerLists: the sync is the other removal path,
// and it is the one that runs unattended.
func TestSync_DropsAccountsCPANoLongerLists(t *testing.T) {
	reg, _ := newModelRegistry(t, nil)
	ctx := context.Background()
	if err := reg.SetProbeEnabled(ctx, "auth-a", "m", true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}
	if len(reg.EnabledPairs()) != 1 {
		t.Fatal("the pair is not enabled to begin with")
	}

	// The mock host now reports no accounts at all.
	host := hostapi.NewMockHost()
	gone := NewRegistry(RegistryConfig{Host: host, Store: stubStore{}})
	if err := gone.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The original registry still sees the account, so the drop has to come
	// from its own sync observing an empty pool. Point it at the empty host.
	reg.host = host
	if _, err := reg.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, ok := reg.Get("auth-a"); ok {
		t.Error("the account survived a sync that no longer listed it")
	}
	if got := reg.EnabledPairs(); len(got) != 0 {
		t.Errorf("EnabledPairs = %v, want none", got)
	}
}
