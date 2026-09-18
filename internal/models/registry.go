// Package models holds the per-model capability table.
//
// Probe requests must use the cheapest reasoning level a model accepts, and
// that floor differs per model (none / minimal / low), so it is data rather
// than a hardcoded constant (design doc 3.6).
package models

import (
	"sort"
	"strings"
	"sync"
)

// ReasoningEffort is the lowest reasoning effort a model accepts.
type ReasoningEffort string

const (
	EffortNone    ReasoningEffort = "none"
	EffortMinimal ReasoningEffort = "minimal"
	EffortLow     ReasoningEffort = "low"
	EffortMedium  ReasoningEffort = "medium"
	EffortHigh    ReasoningEffort = "high"
)

// Capability describes one model known to the plugin.
type Capability struct {
	Model string `json:"model"`
	// MinReasoning is the cheapest effort the model accepts. Probes use it.
	MinReasoning ReasoningEffort `json:"minReasoning"`
	// SupportsTools records whether the model accepts tool definitions. Probes
	// always send an empty tool list, but the panel surfaces this.
	SupportsTools bool `json:"supportsTools"`
	// Probeable is false for models known not to return a turn state; the
	// scheduler skips them rather than burning quota.
	Probeable bool `json:"probeable"`
}

// fallback is used for models absent from the table. "low" is the floor every
// model in the current manifest accepts, so an operator-supplied model name
// still produces a valid probe request rather than being rejected outright.
var fallback = Capability{
	MinReasoning:  EffortLow,
	SupportsTools: false,
	Probeable:     true,
}

// known is the seed model list.
//
// It answers exactly one question: what reasoning level may a probe use, since
// a probe must pick the cheapest level the model accepts. It is NOT a claim
// about which models an account can serve -- the plugin cannot read CPA's model
// registry (the host exposes no callback for it), so the per-account list is
// configured by the operator and this list only seeds it.
//
// The slugs below were taken from the manifest CPA itself syncs,
// https://github.com/router-for-me/models (codex_client_models.json). Every
// entry there lists "low" as its lowest supported reasoning level, so the floor
// is uniform for now. Refresh this list when that manifest changes; the
// fallback below covers any model it has not heard of.
var known = []Capability{
	{Model: "gpt-6-astra", MinReasoning: EffortLow, Probeable: true},
	{Model: "gpt-5.6-sol", MinReasoning: EffortLow, Probeable: true},
	{Model: "gpt-5.6-terra", MinReasoning: EffortLow, Probeable: true},
	{Model: "gpt-5.6-luna", MinReasoning: EffortLow, Probeable: true},
	{Model: "gpt-5.5", MinReasoning: EffortLow, Probeable: true},
	{Model: "gpt-5.3-codex-spark", MinReasoning: EffortLow, Probeable: true},
}

// Registry is a concurrency-safe capability table.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Capability
}

// NewRegistry builds a registry preloaded with the built-in table.
func NewRegistry() *Registry {
	r := &Registry{m: make(map[string]Capability, len(known))}
	for _, c := range known {
		r.m[c.Model] = c
	}
	return r
}

// Lookup returns the capability for a model, falling back for unknown models.
func (r *Registry) Lookup(model string) Capability {
	key := Normalize(model)
	r.mu.RLock()
	c, ok := r.m[key]
	r.mu.RUnlock()
	if !ok {
		c = fallback
		c.Model = model
		return c
	}
	c.Model = model
	return c
}

// MinReasoning returns the reasoning floor for a model.
func (r *Registry) MinReasoning(model string) ReasoningEffort {
	return r.Lookup(model).MinReasoning
}

// Upsert adds or replaces a capability entry.
func (r *Registry) Upsert(c Capability) {
	if c.Model == "" {
		return
	}
	if c.MinReasoning == "" {
		c.MinReasoning = fallback.MinReasoning
	}
	r.mu.Lock()
	r.m[c.Model] = c
	r.mu.Unlock()
}

// All returns the table sorted by model name, for the management API.
func (r *Registry) All() []Capability {
	r.mu.RLock()
	out := make([]Capability, 0, len(r.m))
	for _, c := range r.m {
		out = append(out, c)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Normalize canonicalises a model identifier for table lookups.
func Normalize(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
