package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultCatalogURL is the manifest CLIProxyAPI itself syncs.
//
// The plugin reads the same file rather than keeping its own list, because the
// list is not the plugin's data to own: an earlier version shipped a hardcoded
// table that went stale and had the panel offering gpt-5-codex long after it
// had been retired.
const DefaultCatalogURL = "https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json"

// planTiers are the Codex plan buckets in the manifest. The plugin cannot tell
// which plan an account is on -- the host exposes no model or plan callback --
// so it offers the union. That differs from CPA by at most two models, and a
// probe answers definitively with MODEL_UNSUPPORTED.
var planTiers = []string{"codex-free", "codex-team", "codex-plus", "codex-pro"}

// builtinModels are the Codex built-ins CPA injects regardless of the manifest
// (registry.WithCodexBuiltins). Kept here so the list matches what CPA reports
// for the same account.
var builtinModels = []string{
	"gpt-image-1.5",
	"gpt-image-2",
	"gpt-image-2.5-flare",
	"gpt-image-2.5-sunburst",
	"gpt-image-2.5",
}

// fallbackModels is used until the first successful fetch, and forever on a
// host with no egress. It mirrors the manifest at the time of writing; being
// approximate is acceptable because it is only a starting list, and probing is
// what decides whether a model actually works.
var fallbackModels = []string{
	"gpt-6-astra",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"codex-auto-review",
}

// CatalogInterval is how often the manifest is re-read.
const CatalogInterval = 6 * time.Hour

// Catalog holds the account model list, refreshed from the shared manifest.
type Catalog struct {
	url     string
	client  *http.Client
	refresh time.Duration
	logf    func(string, ...any)

	// list is read on panel requests; writers swap a fresh slice.
	list      atomic.Pointer[[]string]
	fetchedAt atomic.Int64

	mu sync.Mutex // serialises fetches
}

// NewCatalog builds a catalog seeded with the fallback list.
func NewCatalog(url string, logf func(string, ...any)) *Catalog {
	if strings.TrimSpace(url) == "" {
		url = DefaultCatalogURL
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &Catalog{
		url:     url,
		refresh: CatalogInterval,
		logf:    logf,
		client:  &http.Client{Timeout: 20 * time.Second},
	}
	c.store(append(append([]string{}, fallbackModels...), builtinModels...))
	return c
}

// Models returns the current catalog. It is a snapshot read, safe on any path.
func (c *Catalog) Models() []string {
	ptr := c.list.Load()
	if ptr == nil {
		return nil
	}
	out := make([]string, len(*ptr))
	copy(out, *ptr)
	return out
}

// FetchedAt reports when the catalog was last refreshed from the manifest.
func (c *Catalog) FetchedAt() time.Time {
	ns := c.fetchedAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Start refreshes once and then keeps the catalog current until ctx ends.
//
// A failed refresh is logged and ignored: a stale catalog degrades to a stale
// suggestion list, which is not worth disturbing the host over.
func (c *Catalog) Start(ctx context.Context) {
	go func() {
		if err := c.Refresh(ctx); err != nil {
			c.logf("model catalog fetch failed, using the built-in list: %v", err)
		}

		ticker := time.NewTicker(c.refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Refresh(ctx); err != nil {
					c.logf("model catalog refresh failed: %v", err)
				}
			}
		}
	}()
}

// Refresh re-reads the manifest and swaps the catalog in.
func (c *Catalog) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("models: build catalog request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("models: fetch catalog: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("models: catalog returned %d", resp.StatusCode)
	}
	// Bounded: the manifest is a few hundred kilobytes, and an unexpectedly
	// large body must not become a memory problem inside the host process.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("models: read catalog: %w", err)
	}

	list, err := parseCatalog(raw)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return fmt.Errorf("models: catalog contained no codex models")
	}
	c.store(list)
	c.fetchedAt.Store(time.Now().UnixNano())
	c.logf("model catalog refreshed with %d models", len(list))
	return nil
}

func (c *Catalog) store(list []string) {
	sorted := append([]string{}, list...)
	sort.Strings(sorted)
	c.list.Store(&sorted)
}

// parseCatalog extracts the Codex model ids plus the built-ins.
func parseCatalog(raw []byte) ([]string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("models: decode catalog: %w", err)
	}

	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}

	for _, tier := range planTiers {
		body, ok := doc[tier]
		if !ok {
			continue
		}
		var entries []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &entries); err != nil {
			// One malformed tier must not discard the others.
			continue
		}
		for _, e := range entries {
			add(e.ID)
		}
	}
	for _, id := range builtinModels {
		add(id)
	}
	return out, nil
}
