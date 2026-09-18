# Codex Turn State Manager

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) plugin that manages
`X-Codex-Turn-State` values for Codex accounts.

Upstream returns `X-Codex-Turn-State` as a *per-turn* sticky-routing token. Observed
behaviour suggests a value of a particular length (292 by default) can be reused across
turns for a given `(account, model)` pair, giving more stable upstream routing. This
plugin probes accounts on a schedule, binds harvested values per pair, injects them into
outbound requests, and harvests fresh values from ordinary traffic responses.

> **This exploits undocumented behaviour.** Codex documents the token as per-turn only.
> The plugin is therefore built to be switchable, invalidatable and self-healing: it can
> be turned off entirely, a bad binding is dropped automatically after a failed request,
> and a failure to inject never fails the request.

The full specification lives in [`插件开发文档.md`](./插件开发文档.md). Agent-facing
working rules live in [`AGENTS.md`](./AGENTS.md).

---

## Status

The plugin's internals are complete and covered by tests. The **CPA ABI adapter is not
written**, because the plugin SDK surface has not been confirmed against a real CPA
instance or its source — see section 9.2 of the design document and section 7 of
`AGENTS.md`. `cmd/plugin/cshared.go` exports only an ABI version probe so the toolchain
can be validated.

Everything else runs today against the bundled development harness.

---

## Quick start

Requires Go 1.24+ and, for the shared library, a C compiler with `CGO_ENABLED=1`.

```bash
make run
```

Then open <http://127.0.0.1:8787/v0/resource/plugins/codex-turn-state-manager/> and sign
in with the harness key `devkey`.

The harness boots the real application against an in-memory mock of the CPA host and
serves the real Management API and admin panel, so the plugin can be developed without a
CPA instance and without touching real accounts.

```bash
make run ARGS="-accounts 8 -listen 127.0.0.1:9000"
```

## Build

```bash
make build          # development harness + C-ABI shared library
make build-dev      # harness only
make build-shared   # C-ABI .so / .dylib / .dll for the host OS
```

`c-shared` output **cannot** be cross-compiled by setting `GOOS` — build on the target OS
and architecture. The shared library's base name must match the key CPA uses in
`plugins.configs` (`codex-turn-state-manager`).

## Test

```bash
make test        # unit tests
make test-race   # the gate: probe workers and the API write concurrently
make vet
make fmt
```

---

## How it works

```
             ┌──────────────── CPA process ────────────────┐
             │                                             │
  request ──▶│ BeforeAuth ──▶ Scheduler ──▶ AfterAuth ──▶   │──▶ upstream
             │     │              │            │           │
             │     │ correlation  │ account    │ state     │
             │     └──────────────┴────────────┘ injection │
             │                                             │
             │  response headers ──▶ capture ──▶ binding    │
             └─────────────────────────────────────────────┘
                          ▲
             background ──┴── probe scheduler ──▶ proxy pool ──▶ upstream
```

Four runtime capabilities, all gated by one master switch:

| Capability | What it does |
|---|---|
| Probe | Actively harvests state through the proxy pool on a schedule |
| Inject | Rewrites `X-Codex-Turn-State` on outbound requests |
| Capture | Harvests state from ordinary traffic responses |
| Route | Prefers accounts that hold a usable binding |

With the master switch off, all four are bypassed and the plugin is a no-op on the
request path.

### Components

| Path | Responsibility |
|---|---|
| `internal/hostapi` | The only place CPA is described. A Go interface port plus an in-memory mock. |
| `internal/storage` | SQLite (WAL): schema migrations with checksums and backups, and the table stores. |
| `internal/settings` | Configuration, bounds, and the atomic runtime snapshot the hot path reads. |
| `internal/states` | Bindings, their lifecycle, and the lock-free hot-path snapshot. |
| `internal/proxies` | Proxy pool health, cooldown, and least-recently-used selection. |
| `internal/probe` | Scan scheduling, time windows, backoff, proxy traversal, request shape. |
| `internal/intercept` | Correlation across interceptor stages, injection, capture, self-healing. |
| `internal/routing` | Interference in CPA's account selection. |
| `internal/app` | Composition root; owns lifecycle and the request-path entry points. |
| `web` | The embedded admin panel (static assets only). |

### Key behaviours

**State lifecycle.** A binding is `FRESH` until 85% of its TTL has elapsed, then
`REFRESH_DUE` (still injected, but scheduled for renewal), then `EXPIRED`. Default TTL is
60 minutes.

**Only target-length values bind.** A harvested value whose length is not
`target_state_length` is recorded and discarded — never bound.

**Least-recently-used proxy rotation.** Each probe walks the whole pool in LRU order,
stamping `last_used_at` *before* each request so concurrent probes never pick the same
node. The walk stops early on a target hit, or on an account- or model-level error that
another proxy could not fix.

**Failure classification.** Timeouts, connect errors, TLS errors and 407 are proxy
faults and cool the node down (1m → 2m → 5m → 10m). Upstream 400/401/403/429 are not, and
never evict a healthy node.

**Scanning is not probing.** A one-minute scan only enqueues probes that are actually
due; a non-target-length result backs off for minutes, not seconds.

**Self-healing.** If a request that carried plugin-injected state fails with
`previous_response_not_found`, a routing error, or a run of 5xx, the binding is dropped
and re-probed — so a bad value cannot poison traffic for the rest of its TTL.

---

## Configuration

Set from the panel or the Management API. Defaults:

| Setting | Default | Notes |
|---|---|---|
| `global_enabled` | `true` | Master switch for all four capabilities |
| `global_probe_enabled` | `true` | Active probing (needs the master switch) |
| `global_reverse_bind_enabled` | `true` | Traffic capture (needs the master switch) |
| `scan_interval_sec` | `60` | How often the scheduler checks for due pairs |
| `probe_concurrency` | `2` | Simultaneous pair probes; each walks the pool serially |
| `state_ttl_min` | `60` | Binding lifetime |
| `refresh_threshold_pct` | `15` | Re-probe once this much of the TTL remains |
| `target_state_length` | `292` | The only length that binds |
| `max_probe_duration_sec` | `90` | Cap on one pair's traversal of the whole pool |
| `account_routing_strategy` | `respect_cpa_priority` | or `state_first` |

Time windows restrict when probing runs. Several windows may be configured, each with an
optional day-of-week set; a window may cross midnight (`22:00–06:00`). With no enabled
window, probing is unrestricted.

Proxy nodes are ordinary HTTP/HTTPS/SOCKS5 URLs.

## Management API

All routes are under `/v0/management/plugins/codex-turn-state-manager/` and require CPA's
management auth.

```
GET    /status
GET    /settings                          PUT /settings
GET    /time-windows                      POST /time-windows
PUT    /time-windows/{id}                 DELETE /time-windows/{id}
GET    /accounts                          POST /accounts/sync
GET    /accounts/{authIndex}/models
PUT    /accounts/{authIndex}/models/{model}/probe
GET    /bindings                          DELETE /bindings/{authIndex}/{model}
GET    /bindings/{authIndex}/{model}/history
DELETE /bindings/{authIndex}/{model}/history
GET    /proxy-nodes                       PUT /proxy-nodes
GET    /probe-history?limit=50
```

The panel's own assets are served from `/v0/resource/plugins/codex-turn-state-manager/`,
which bypasses management auth. Nothing secret is ever served from there: state values
are returned as prefixes through the Management API, and the management key is held in
page memory only.

## Data

`state.db` (SQLite, WAL) lives in the plugin data directory, with pre-migration backups
under `backups/`. Schema changes are forward-only migrations with SHA-256 checksums; a
database written by a newer plugin build refuses to start rather than risk corruption.

## Licence

Not yet specified.
