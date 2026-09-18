// Package management implements the plugin's Management API.
//
// Every data operation the panel performs goes through here. The panel's own
// assets are served from a Resource route, which bypasses CPA's management
// auth, so no secret may ever be returned from there (NF-05, design doc 4.7).
package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/accounts"
	"github.com/yangshoulai/codex-turn-state-manager/internal/intercept"
	"github.com/yangshoulai/codex-turn-state-manager/internal/models"
	"github.com/yangshoulai/codex-turn-state-manager/internal/probe"
	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
)

// Service is the app surface the API needs. Implemented by app.App so this
// package does not depend on the composition root.
type Service interface {
	Settings() *settings.Manager
	Accounts() *accounts.Registry
	Models() *models.Registry
	States() *states.Registry
	Proxies() *proxies.Pool
	Windows() *probe.WindowManager
	ProbeHistory() probe.HistoryStore
	ProbeScheduler() *probe.Scheduler

	Catalog() *models.Catalog
	PipelineStats() intercept.StatsSnapshot

	SyncAccounts(ctx context.Context) (int, error)
	TriggerProbe(ctx context.Context, authIndex, model string) error
}

// API serves the Management API.
type API struct {
	svc Service
	now func() time.Time
}

// New builds the API.
func New(svc Service) *API { return &API{svc: svc, now: time.Now} }

// Register mounts every route on mux under the plugin's management prefix.
func (a *API) Register(mux *http.ServeMux) {
	base := version.ManagementBasePath

	for _, route := range Routes() {
		mux.HandleFunc(route.Method+" "+base+route.Path, route.bind(a))
	}
}

// Route is one Management API route.
//
// CPA dispatches management calls by exact path -- routes are a map keyed by
// "METHOD /path" and paths containing ":" or "*" are rejected at registration.
// There is no wildcard option, so anything variable travels as a query
// parameter and every path here must be a literal.
//
// This table is also what gets declared to the host, so it is the single source
// of truth for what exists.
type Route struct {
	Method  string
	Path    string
	Handler func(*API) http.HandlerFunc
}

func (r Route) bind(a *API) http.HandlerFunc { return r.Handler(a) }

// Routes returns every Management API route, in registration order.
func Routes() []Route {
	return []Route{
		{http.MethodGet, "/status", func(a *API) http.HandlerFunc { return a.status }},

		{http.MethodGet, "/settings", func(a *API) http.HandlerFunc { return a.getSettings }},
		{http.MethodPut, "/settings", func(a *API) http.HandlerFunc { return a.putSettings }},

		{http.MethodGet, "/time-windows", func(a *API) http.HandlerFunc { return a.listWindows }},
		{http.MethodPost, "/time-windows", func(a *API) http.HandlerFunc { return a.createWindow }},
		{http.MethodPut, "/time-windows", func(a *API) http.HandlerFunc { return a.updateWindow }},
		{http.MethodDelete, "/time-windows", func(a *API) http.HandlerFunc { return a.deleteWindow }},

		{http.MethodGet, "/models", func(a *API) http.HandlerFunc { return a.listModels }},
		{http.MethodGet, "/accounts", func(a *API) http.HandlerFunc { return a.listAccounts }},
		{http.MethodPost, "/accounts/sync", func(a *API) http.HandlerFunc { return a.syncAccounts }},
		{http.MethodGet, "/accounts/models", func(a *API) http.HandlerFunc { return a.listAccountModels }},
		{http.MethodPut, "/accounts/models/probe", func(a *API) http.HandlerFunc { return a.setProbeEnabled }},
		{http.MethodDelete, "/accounts/models", func(a *API) http.HandlerFunc { return a.forgetModel }},

		{http.MethodGet, "/bindings", func(a *API) http.HandlerFunc { return a.listBindings }},
		{http.MethodDelete, "/bindings", func(a *API) http.HandlerFunc { return a.deleteBinding }},
		{http.MethodGet, "/bindings/history", func(a *API) http.HandlerFunc { return a.bindingHistory }},
		{http.MethodDelete, "/bindings/history", func(a *API) http.HandlerFunc { return a.clearBindingHistory }},

		{http.MethodGet, "/proxy-nodes", func(a *API) http.HandlerFunc { return a.listProxies }},
		{http.MethodPut, "/proxy-nodes", func(a *API) http.HandlerFunc { return a.replaceProxies }},

		{http.MethodGet, "/probe-history", func(a *API) http.HandlerFunc { return a.probeHistory }},
	}
}

// pairQuery reads the (authIndex, model) pair a binding endpoint operates on.
func pairQuery(w http.ResponseWriter, r *http.Request) (states.Pair, bool) {
	pair := states.Pair{
		AuthIndex: strings.TrimSpace(r.URL.Query().Get("authIndex")),
		Model:     strings.TrimSpace(r.URL.Query().Get("model")),
	}
	if pair.AuthIndex == "" || pair.Model == "" {
		writeError(w, http.StatusBadRequest,
			errors.New("authIndex and model query parameters are required"))
		return states.Pair{}, false
	}
	return pair, true
}

// ---------------------------------------------------------------------------
// status

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	values := a.svc.Settings().Current()
	caps := values.Capabilities()
	proxies := a.svc.Proxies().All()

	healthy := 0
	now := a.now()
	for _, p := range proxies {
		if p.Healthy(now) {
			healthy++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":  version.Version,
		"now":      now.UTC(),
		"settings": toSettingsDTO(values),
		"capabilities": map[string]bool{
			"enabled": caps.Enabled,
			"probe":   caps.Probe,
			"inject":  caps.Inject,
			"capture": caps.Capture,
			"route":   caps.Route,
		},
		"proxies": map[string]int{"total": len(proxies), "healthy": healthy},
		// Counters rather than log lines: this is how the panel can say whether
		// the plugin has ever actually injected, which a header rewrite
		// otherwise leaves no trace of.
		"pipeline": a.svc.PipelineStats(),
	})
}

// ---------------------------------------------------------------------------
// settings

type settingsDTO struct {
	GlobalEnabled            bool   `json:"globalEnabled"`
	GlobalProbeEnabled       bool   `json:"globalProbeEnabled"`
	GlobalReverseBindEnabled bool   `json:"globalReverseBindEnabled"`
	StatePriorityEnabled     bool   `json:"statePriorityEnabled"`
	ScanIntervalSec          int    `json:"scanIntervalSec"`
	ProbeConcurrency         int    `json:"probeConcurrency"`
	StateTTLMin              int    `json:"stateTtlMin"`
	RefreshThresholdPct      int    `json:"refreshThresholdPct"`
	TargetStateLength        int    `json:"targetStateLength"`
	MaxProbeDurationSec      int    `json:"maxProbeDurationSec"`
	RoutingStrategy          string `json:"routingStrategy"`
	AccountSyncIntervalSec   int    `json:"accountSyncIntervalSec"`
	ProbeRetentionHours      int    `json:"probeHistoryRetentionHours"`
}

func toSettingsDTO(v *settings.Values) settingsDTO {
	return settingsDTO{
		GlobalEnabled:            v.GlobalEnabled,
		GlobalProbeEnabled:       v.GlobalProbeEnabled,
		GlobalReverseBindEnabled: v.GlobalReverseBindEnabled,
		StatePriorityEnabled:     v.StatePriorityEnabled,
		ScanIntervalSec:          int(v.ScanInterval / time.Second),
		ProbeConcurrency:         v.ProbeConcurrency,
		StateTTLMin:              int(v.StateTTL / time.Minute),
		RefreshThresholdPct:      v.RefreshThresholdPct,
		TargetStateLength:        v.TargetStateLength,
		MaxProbeDurationSec:      int(v.MaxProbeDuration / time.Second),
		RoutingStrategy:          string(v.RoutingStrategy),
		AccountSyncIntervalSec:   int(v.AccountSyncInterval / time.Second),
		ProbeRetentionHours:      int(v.ProbeRetention / time.Hour),
	}
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, toSettingsDTO(a.svc.Settings().Current()))
}

// settingsPatchDTO uses pointers so an omitted field means "leave unchanged".
type settingsPatchDTO struct {
	GlobalEnabled            *bool   `json:"globalEnabled"`
	GlobalProbeEnabled       *bool   `json:"globalProbeEnabled"`
	GlobalReverseBindEnabled *bool   `json:"globalReverseBindEnabled"`
	StatePriorityEnabled     *bool   `json:"statePriorityEnabled"`
	ScanIntervalSec          *int    `json:"scanIntervalSec"`
	ProbeConcurrency         *int    `json:"probeConcurrency"`
	StateTTLMin              *int    `json:"stateTtlMin"`
	RefreshThresholdPct      *int    `json:"refreshThresholdPct"`
	TargetStateLength        *int    `json:"targetStateLength"`
	MaxProbeDurationSec      *int    `json:"maxProbeDurationSec"`
	RoutingStrategy          *string `json:"routingStrategy"`
	AccountSyncIntervalSec   *int    `json:"accountSyncIntervalSec"`
	ProbeRetentionHours      *int    `json:"probeHistoryRetentionHours"`
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	var dto settingsPatchDTO
	if !decodeJSON(w, r, &dto) {
		return
	}

	patch := settings.Patch{
		GlobalEnabled:            dto.GlobalEnabled,
		GlobalProbeEnabled:       dto.GlobalProbeEnabled,
		GlobalReverseBindEnabled: dto.GlobalReverseBindEnabled,
		StatePriorityEnabled:     dto.StatePriorityEnabled,
		ProbeConcurrency:         dto.ProbeConcurrency,
		RefreshThresholdPct:      dto.RefreshThresholdPct,
		TargetStateLength:        dto.TargetStateLength,
	}
	if dto.ScanIntervalSec != nil {
		d := time.Duration(*dto.ScanIntervalSec) * time.Second
		patch.ScanInterval = &d
	}
	if dto.StateTTLMin != nil {
		d := time.Duration(*dto.StateTTLMin) * time.Minute
		patch.StateTTL = &d
	}
	if dto.MaxProbeDurationSec != nil {
		d := time.Duration(*dto.MaxProbeDurationSec) * time.Second
		patch.MaxProbeDuration = &d
	}
	if dto.AccountSyncIntervalSec != nil {
		d := time.Duration(*dto.AccountSyncIntervalSec) * time.Second
		patch.AccountSyncInterval = &d
	}
	if dto.ProbeRetentionHours != nil {
		d := time.Duration(*dto.ProbeRetentionHours) * time.Hour
		patch.ProbeRetention = &d
	}
	if dto.RoutingStrategy != nil {
		strategy, err := settings.ParseRoutingStrategy(*dto.RoutingStrategy)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		patch.RoutingStrategy = &strategy
	}

	updated, err := a.svc.Settings().Update(r.Context(), patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettingsDTO(updated))
}

// ---------------------------------------------------------------------------
// time windows

func (a *API) listWindows(w http.ResponseWriter, r *http.Request) {
	windows := probe.SortWindows(a.svc.Windows().All())
	if windows == nil {
		windows = []probe.TimeWindow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"windows": windows})
}

func (a *API) createWindow(w http.ResponseWriter, r *http.Request) {
	var win probe.TimeWindow
	if !decodeJSON(w, r, &win) {
		return
	}
	if win.ID == "" {
		win.ID = fmt.Sprintf("w%d", a.now().UnixNano())
	}
	if err := a.svc.Windows().Upsert(r.Context(), win); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, win)
}

func (a *API) updateWindow(w http.ResponseWriter, r *http.Request) {
	var win probe.TimeWindow
	if !decodeJSON(w, r, &win) {
		return
	}
	win.ID = strings.TrimSpace(r.URL.Query().Get("id"))
	if win.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id query parameter is required"))
		return
	}
	if err := a.svc.Windows().Upsert(r.Context(), win); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, win)
}

func (a *API) deleteWindow(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id query parameter is required"))
		return
	}
	if err := a.svc.Windows().Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// ---------------------------------------------------------------------------
// accounts

// listModels returns the account model list and where it came from.
//
// The list is read from the same manifest CLIProxyAPI syncs. The plugin has no
// callback for CPA's own registry, so this is the nearest authoritative source;
// whether an account can actually serve a model is answered by probing.
func (a *API) listModels(w http.ResponseWriter, r *http.Request) {
	registry := a.svc.Models()
	names := a.svc.Catalog().Models()
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{
			"model":        name,
			"minReasoning": string(registry.MinReasoning(name)),
		})
	}
	payload := map[string]any{"models": out}
	if at := a.svc.Catalog().FetchedAt(); !at.IsZero() {
		payload["fetchedAt"] = at.UTC()
	} else {
		payload["fetchedAt"] = nil
		payload["note"] = "尚未成功拉取模型清单，当前为内置列表"
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	list := a.svc.Accounts().AllWithBlockReason(a.now())
	if list == nil {
		list = []accounts.AccountView{}
	}

	type accountView struct {
		accounts.AccountView
		Bindings int `json:"bindings"`
	}
	// Count from the binding table rather than from the model list: bindings
	// outlive a model disappearing from the catalog, and the count is about
	// what is actually held, not about what is offered.
	bound := map[string]int{}
	for pair, binding := range a.svc.States().All() {
		if a.svc.States().StatusOf(binding, a.now()).Usable() {
			bound[pair.AuthIndex]++
		}
	}

	out := make([]accountView, 0, len(list))
	for _, acc := range list {
		out = append(out, accountView{AccountView: acc, Bindings: bound[acc.AuthIndex]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (a *API) syncAccounts(w http.ResponseWriter, r *http.Request) {
	n, err := a.svc.SyncAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accounts": n})
}

func (a *API) listAccountModels(w http.ResponseWriter, r *http.Request) {
	authIndex := strings.TrimSpace(r.URL.Query().Get("authIndex"))
	if authIndex == "" {
		writeError(w, http.StatusBadRequest, errors.New("authIndex query parameter is required"))
		return
	}
	modelStates, ok := a.svc.Accounts().Models(authIndex)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown account %q", authIndex))
		return
	}

	type modelView struct {
		accounts.ModelState
		Status       string     `json:"status"`
		StateLength  int        `json:"stateLength,omitempty"`
		Source       string     `json:"source,omitempty"`
		ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
		BoundAt      *time.Time `json:"boundAt,omitempty"`
		NextProbeAt  *time.Time `json:"nextProbeAt,omitempty"`
		InFlight     bool       `json:"inFlight"`
		MinReasoning string     `json:"minReasoning"`
		// NonTargetStreak counts consecutive probes that returned a state of the
		// wrong length. Such a pair is re-probed every few minutes by design, so
		// a long run is the only signal that a model will never yield a usable
		// token -- visible here rather than polled at quota forever.
		NonTargetStreak int `json:"nonTargetStreak,omitempty"`
	}
	out := make([]modelView, 0, len(modelStates))
	for _, ms := range modelStates {
		view := modelView{
			ModelState:   ms,
			Status:       states.StatusMissing.String(),
			MinReasoning: string(a.svc.Models().MinReasoning(ms.Model)),
		}
		pair := states.Pair{AuthIndex: authIndex, Model: ms.Model}
		if b, status := a.svc.States().Lookup(authIndex, ms.Model); status != states.StatusMissing {
			view.Status = status.String()
			view.StateLength = b.StateLength
			view.Source = string(b.Source)
			expires, bound := b.ExpiresAt, b.BoundAt
			view.ExpiresAt, view.BoundAt = &expires, &bound
		}
		if at, ok := a.svc.ProbeScheduler().NextProbeAt(pair); ok && !at.IsZero() {
			view.NextProbeAt = &at
		}
		view.InFlight = a.svc.ProbeScheduler().InFlight(pair)
		view.NonTargetStreak = a.svc.ProbeScheduler().NonTargetStreak(pair)
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (a *API) setProbeEnabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	authIndex := strings.TrimSpace(r.URL.Query().Get("authIndex"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if authIndex == "" || model == "" {
		writeError(w, http.StatusBadRequest,
			errors.New("authIndex and model query parameters are required"))
		return
	}
	if err := a.svc.Accounts().SetProbeEnabled(r.Context(), authIndex, model, body.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authIndex": authIndex, "model": model, "probeEnabled": body.Enabled,
	})
}

// forgetModel drops an operator-added model from an account.
//
// The seed model list is only a starting point and the plugin cannot read
// CPA's model registry, so an operator has to be able to remove entries that
// this account does not serve.
func (a *API) forgetModel(w http.ResponseWriter, r *http.Request) {
	authIndex := strings.TrimSpace(r.URL.Query().Get("authIndex"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if authIndex == "" || model == "" {
		writeError(w, http.StatusBadRequest,
			errors.New("authIndex and model query parameters are required"))
		return
	}

	// Drop the binding first: leaving a state value attached to a pair that no
	// longer appears anywhere would be invisible and unremovable from the panel.
	pair := states.Pair{AuthIndex: authIndex, Model: model}
	if err := a.svc.States().Delete(r.Context(), pair, states.SourceManual); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.svc.Accounts().ForgetModel(r.Context(), authIndex, model); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authIndex": authIndex, "model": model, "removed": true})
}

// ---------------------------------------------------------------------------
// bindings

// bindingView deliberately truncates the state value. The full value is
// available only through the history endpoint's explicit expand parameter, so
// it is not fanned out to every render of the account list.
type bindingView struct {
	AuthIndex    string    `json:"authIndex"`
	Model        string    `json:"model"`
	StatePrefix  string    `json:"statePrefix"`
	StateLength  int       `json:"stateLength"`
	Source       string    `json:"source"`
	ProxyID      string    `json:"proxyId,omitempty"`
	BoundAt      time.Time `json:"boundAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Status       string    `json:"status"`
	RemainingSec int       `json:"remainingSec"`
}

func (a *API) listBindings(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	all := a.svc.States().All()
	out := make([]bindingView, 0, len(all))
	for _, b := range all {
		out = append(out, a.toBindingView(b, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": out})
}

func (a *API) toBindingView(b states.Binding, now time.Time) bindingView {
	status := a.svc.States().StatusOf(b, now)
	remaining := int(b.ExpiresAt.Sub(now).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return bindingView{
		AuthIndex:    b.AuthIndex,
		Model:        b.Model,
		StatePrefix:  prefix(b.StateValue, 8),
		StateLength:  b.StateLength,
		Source:       string(b.Source),
		ProxyID:      b.ProxyID,
		BoundAt:      b.BoundAt,
		ExpiresAt:    b.ExpiresAt,
		Status:       status.String(),
		RemainingSec: remaining,
	}
}

func (a *API) deleteBinding(w http.ResponseWriter, r *http.Request) {
	pair, ok := pairQuery(w, r)
	if !ok {
		return
	}
	if err := a.svc.States().Delete(r.Context(), pair, states.SourceManual); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Deleting a binding is an operator saying "this value is wrong"; re-probe
	// it as soon as the next scan runs rather than waiting out the old backoff.
	a.svc.ProbeScheduler().ProbeNow(pair)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (a *API) bindingHistory(w http.ResponseWriter, r *http.Request) {
	store, ok := a.svc.States().HistoryStore().(interface {
		ListHistory(ctx context.Context, p states.Pair, limit, offset int) ([]states.HistoryEntry, error)
	})
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("history store unavailable"))
		return
	}
	pair, ok := pairQuery(w, r)
	if !ok {
		return
	}
	limit := intQuery(r, "limit", 50)
	offset := intQuery(r, "offset", 0)

	entries, err := store.ListHistory(r.Context(), pair, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	expand := r.URL.Query().Get("expand") == "1"
	type historyView struct {
		ID          int64     `json:"id"`
		StatePrefix string    `json:"statePrefix"`
		StateValue  string    `json:"stateValue,omitempty"`
		StateLength int       `json:"stateLength"`
		Source      string    `json:"source"`
		Action      string    `json:"action"`
		ProxyID     string    `json:"proxyId,omitempty"`
		BoundAt     time.Time `json:"boundAt"`
		CreatedAt   time.Time `json:"createdAt"`
	}
	out := make([]historyView, 0, len(entries))
	for _, e := range entries {
		view := historyView{
			ID:          e.ID,
			StatePrefix: prefix(e.StateValue, 8),
			StateLength: e.StateLength,
			Source:      string(e.Source),
			Action:      string(e.Action),
			ProxyID:     e.ProxyID,
			BoundAt:     e.BoundAt,
			CreatedAt:   e.CreatedAt,
		}
		if expand {
			view.StateValue = e.StateValue
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": out, "limit": limit, "offset": offset})
}

func (a *API) clearBindingHistory(w http.ResponseWriter, r *http.Request) {
	store, ok := a.svc.States().HistoryStore().(interface {
		ClearHistory(ctx context.Context, p states.Pair) (int64, error)
	})
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("history store unavailable"))
		return
	}
	pair, ok := pairQuery(w, r)
	if !ok {
		return
	}
	n, err := store.ClearHistory(r.Context(), pair)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

// ---------------------------------------------------------------------------
// proxies

type proxyView struct {
	proxies.Node
	Status string `json:"status"`
}

func (a *API) listProxies(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	nodes := a.svc.Proxies().All()
	out := make([]proxyView, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, proxyView{Node: n, Status: n.Status(now)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": out})
}

func (a *API) replaceProxies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Proxies []proxies.Node `json:"proxies"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := a.svc.Proxies().ReplaceAll(r.Context(), body.Proxies); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a.listProxies(w, r)
}

// ---------------------------------------------------------------------------
// probe history

func (a *API) probeHistory(w http.ResponseWriter, r *http.Request) {
	q := probe.ProbeQuery{
		AuthIndex: strings.TrimSpace(r.URL.Query().Get("authIndex")),
		Model:     strings.TrimSpace(r.URL.Query().Get("model")),
		Limit:     intQuery(r, "limit", 50),
		Offset:    intQuery(r, "offset", 0),
	}.Normalise()

	entries, err := a.svc.ProbeHistory().ListProbes(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []probe.HistoryEntry{}
	}

	// The total is what makes paging usable; without it the panel can only
	// offer "next" and hope.
	total, err := a.svc.ProbeHistory().CountProbes(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"probes": entries,
		"limit":  q.Limit,
		"offset": q.Offset,
		"total":  total,
	})
}

// ---------------------------------------------------------------------------
// helpers

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return false
	}
	return true
}

func intQuery(r *http.Request, key string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// prefix returns the first n characters of s, used so the panel shows a
// recognisable stub instead of the whole token.
func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
