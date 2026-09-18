# AGENTS.md

Instructions for AI coding agents working in this repository. Read this before
touching any code. The authoritative specification is
[`插件开发文档.md`](./插件开发文档.md) — where this file and that document
disagree, the design document wins and this file is the bug.

---

## 1. What this project is

`codex-turn-state-manager` is a plugin for **CLIProxyAPI (CPA)**. CPA proxies
Codex accounts to the upstream Responses API. Upstream returns an
`X-Codex-Turn-State` header, officially documented as a *per-turn* sticky
routing token that should not be replayed across turns.

Observed behaviour suggests a turn-state value of a specific length (default
292) can be reused across turns per `(account, model)` pair, giving more stable
upstream routing. The plugin exploits that observation: it probes accounts on a
schedule, binds the harvested value to an `(authIndex, model)` pair, injects it
into subsequent outbound requests, and harvests fresh values from normal
traffic responses.

**This is an experimental strategy layered on undocumented behaviour.** That
fact drives most of the invariants below. The design must stay switchable,
invalidatable, and self-healing, and the plugin must never fail closed.

Delivered artefact: a **C-ABI shared library** (`.so` / `.dylib` / `.dll`) built
with CGO, loaded by CPA. The library base name must equal the key CPA uses in
`plugins.configs` (`codex-turn-state-manager`).

---

## 2. Hard invariants

These are not style preferences. Violating any of them is a defect.

### 2.1 The master switch gates all four runtime capabilities

`global_enabled` (default `true`) gates **probing, request injection, response
capture, and account-selection interference**. When it is off:

| Component | Required behaviour |
|---|---|
| `probe.Scheduler` | does not enqueue, does not probe |
| `intercept.RequestInjector` | does not modify headers, does not write correlation state |
| `intercept.ResponseCollector` | ignores response headers entirely — no capture, no TTL refresh, no rebind |
| `routing.Scheduler` | returns `DelegateBuiltin` |

Read the switch through **one atomic snapshot per request**
(`settings.Manager.Current()`), never through a cached field. A toggle must take
effect on the next request.

The three sub-switches (`global_probe_enabled`, `global_reverse_bind_enabled`,
`state_priority_enabled`) are only consulted when the master switch is on. Derive
this from `settings.Values.Capabilities()` rather than re-deriving the matrix at
each call site — there is exactly one implementation of this truth.

Note the asymmetry, which is intentional: with every sub-switch off but the
master on, the plugin still injects state it already holds. Only the master
switch stops injection. Routing is gated by the master switch **and**
`state_priority_enabled`, because overriding the host's load balancing is more
invasive than rewriting one request's headers.

Probes already in flight when the master switch flips must have their results
**discarded**, not written.

### 2.2 The request hot path never touches SQLite

`NF-01`: state lookup on the request path must be O(1) memory. The contract is:

```
SQLite  ──write──▶  atomic snapshot (internal/states)  ──read──▶  interceptors
```

`states.Registry.Lookup` is a lock-free `atomic.Pointer` load plus a map
lookup. Never add a DB query, a mutex, or a network call to that path. The
snapshot is **copy-on-write**: writers build a fresh map and swap the pointer,
so readers always see a consistent immutable value. Do not "optimise" this into
an in-place map mutation.

`proxies.Pool` is the deliberate exception — a handful of nodes, mutated under a
plain mutex. Do not grow it into something that needs a snapshot.

### 2.3 Schema changes go through a new migration, always

- Never edit a migration that has shipped. `Migration.Checksum()` is verified at
  startup and a mismatch **refuses to start**. If you need to change a released
  migration, you need a new migration.
- Bump `storage.CurrentSchemaVersion` in the **same commit** that appends the
  migration.
- Migrations run inside a transaction, after a `VACUUM INTO` backup.
- A migration that fails must leave the database untouched and restore the
  backup.
- New columns need a default value — old rows must not read back as `NULL`
  surprises.
- Index changes are migrations too.

**Do not "tidy" the v1/v2 split.** v1 intentionally creates `proxy_node`
*without* `last_used_at` so v2 has a column to add. A fresh install runs
1 → 2 → 3. Folding the column into v1 makes v2 fail with `duplicate column name`
on every existing installation.

SQLite cannot drop or retype a column: use the create-new / copy / drop-old /
rename dance, all inside one transaction (see the v4 example in the design doc,
section 7.4.3).

### 2.4 Only target-length values are ever bound

`F-15`: a harvested state whose length is not `target_state_length` (default
292) is written to `probe_history` and nothing else. It must not create,
replace, or refresh a binding. Enforce this at the call site before calling
`states.Registry.Bind`.

### 2.5 Credentials are fetched live and never persisted or logged

`NF-06` / `NF-07`: call `hostapi.Host.GetCredential` immediately before each
probe. Never cache an `access_token`, never write one to SQLite/history/logs,
and never refresh OAuth yourself — CPA owns the token lifecycle.

### 2.6 Resource routes never expose secrets

The embedded panel (`web/`, served under `ResourceBasePath`) bypasses CPA's
management auth. It may serve only static HTML/JS/CSS. Every data operation goes
through a Management API route under `ManagementBasePath`. Never render a token,
a proxy password, a full auth JSON blob, or a complete state value into a
resource response. The management key lives in browser session memory only — not
in `localStorage`, not in plugin state, not in logs.

### 2.7 A failure to classify is not a reason to evict a proxy

`NETWORK_ERROR` (`timeout`, connect error, TLS error, HTTP 407) is a genuine
proxy fault: back the node off and try the next one. Upstream `400` / `401` /
`403` / `429` are **not** proxy faults — never apply a cooldown for them.

`AUTH_ERROR` and `MODEL_UNSUPPORTED` are account- or model-level problems.
Trying another proxy cannot fix them, so abort the traversal immediately.

### 2.8 Probe request shape is fixed

Probes go **directly to the upstream Codex endpoint**, not through CPA's
`/v1/responses` (CPA's host HTTP API cannot carry a per-request proxy). Probes
use the cheapest reasoning level the model accepts, an empty tool list, and a
one-character input, to minimise token burn (`F-14`). The reasoning floor comes
from `models.Registry` — never hardcode it.

Read the response headers, and if the state matches the target length, bind and
**close the body immediately**. Do not wait for the SSE stream to finish.

### 2.9 Backoff is per-outcome, and scanning is not probing

`F-16`. "Scan every minute" and "probe every minute" are different things.
A non-target-length result must never cause a one-minute re-probe loop.

| Outcome | `next_probe_at` |
|---|---|
| `SUCCESS_TARGET` | `now + TTL × (1 − refresh_threshold)` |
| `SUCCESS_NON_TARGET` | `now + 2–5 min` (jittered) |
| `PROBE_NO_PROXY_AVAILABLE` | `now + 5 min` |
| `PROBE_TIMEOUT_ALL_PROXIES` | `now + 5 min` |
| `NETWORK_ERROR` | `now + 2 min`; node cooldown `1m → 2m → 5m → 10m` |
| `RATE_LIMIT` | honour `Retry-After` |
| `AUTH_ERROR` | `now + 30 min` |
| `MODEL_UNSUPPORTED` | `now + 30 min` or auto-disable |

### 2.10 Self-healing only applies to state the plugin injected

`3.12`: invalidate a binding and allow one retry-without-injected-state **only**
when the master switch was on *and* this specific request carried plugin-injected
state. `intercept.CorrelationManager` records whether injection happened — use
that flag, do not infer it.

The trigger is the host's `RequestCompletion` (outcome plus status), not a
parsed response body. A `rejected` or `canceled` request is never blamed on the
stored state, and a failure that cannot be attributed to the injected state does
not evict it.

---

## 3. Repository layout

```
cmd/plugin/
  main.go              dev harness: mock host + local management API + panel
  cshared.go           C-ABI entry points (build tag `cshared`)  [ABI boundary]
internal/
  hostapi/             the ONLY place CPA types are described
    host.go            Host interface, Account, Credential, scheduler DTOs
    mock.go            in-memory Host for tests and the dev harness
  pluginabi/           adapter from the real CPA C ABI onto hostapi.Host [ABI EDGE]
    abi.go             cgo preamble, exported symbols, host callback client
    plugin.go          JSON-RPC dispatch, registration, interceptors, scheduler
    hostclient.go      hostapi.Host implemented over host.auth.* callbacks
    management.go      Management API + resource route registration
  version/             build identity + route prefixes
  settings/            config keys, defaults, bounds, atomic runtime snapshot
  storage/             SQLite: connection, migrations, backup/restore, stores
  accounts/            AccountRegistry: CPA sync + AuthIndex⇄AuthID mapping
  models/              ModelRegistry: reasoning floor per model
  states/              StateRegistry: bindings, hot-path snapshot, history
  proxies/             ProxyPool: health, cooldown, LRU selection order
  probe/               scheduler, time windows, backoff, executor, request shape
  intercept/           correlation, request injection, response capture
  routing/             CredentialScheduler: account-choice interference
  management/          Management API handlers
  app/                 composition root; owns lifecycle and hot-path entry points
web/                   embedded admin panel (static assets only)
```

### The ABI boundary

`internal/hostapi` is a **port**. No package outside it may reference a CPA SDK
type, and no package outside `internal/pluginabi` may reference CGO. The CPA Go
types are not available to us at compile time; everything the plugin needs from
CPA is declared as a plain Go interface in `hostapi`, and the adapter implements
it.

This is what makes the whole domain testable against `hostapi.MockHost`. Keep it
that way: when you add a host capability, add it to the `Host` interface and to
`MockHost` in the same commit.

`internal/pluginabi` is the only package that may reference CGO or a CPA SDK
type. The plugin depends on `github.com/router-for-me/CLIProxyAPI/v7` directly
so the wire structs come from the SDK rather than being re-declared — a field
rename upstream is then a compile error instead of a silent mismatch.

Two shape rules that are easy to get wrong: the registration **response** uses
snake_case JSON keys while interceptor, scheduler and stream payloads use
PascalCase (the Go field names), and `host.auth.get` returns the raw on-disk
auth file rather than a parsed credential.

---

## 4. Naming conventions

`9.1` fixes this vocabulary. Do not invent synonyms.

| Term | Meaning |
|---|---|
| `AuthIndex` / `auth_index` | The plugin's **persistence key**. Stable. Used in every table and as the primary key of a binding. |
| `AuthID` / `authId` | CPA's **runtime** account handle, used by the scheduler. Not persisted as a key. |
| pair | An `(authIndex, model)` tuple — the unit of binding, probing, and config. |
| turn state | The `X-Codex-Turn-State` value. |
| binding | A turn-state value currently attached to a pair. |
| probe | An active outbound request whose only purpose is harvesting a turn state. |
| reverse bind | Harvesting a turn state passively from normal traffic. |
| target length | `target_state_length`, default 292. The only length that binds. |

`AccountRegistry` owns the `AuthIndex` ⇄ `AuthID` mapping. Never assume the two
are interchangeable.

Constants for the wire spellings of these headers live in `internal/intercept`.
Do not scatter string literals for header names.

---

## 5. Build, test, run

```bash
make build          # dev harness + C-ABI shared library
make build-dev      # standalone harness only (mock host, no CPA needed)
make build-shared   # C-ABI .so/.dylib/.dll for the host OS
make run            # run the harness on 127.0.0.1:8787 with a local SQLite file
make test           # unit tests
make test-race      # unit tests under the race detector
make vet            # go vet
make fmt            # gofmt -s
```

`CGO_ENABLED=1` is mandatory for the shared library. **`c-shared` output cannot
be cross-compiled by setting `GOOS`** — build on the target OS and
architecture (a CI matrix, not a cross-compile). The dev harness has no CGO
requirement.

The dev harness (`make run`) is the intended inner loop: it boots the real app
against `hostapi.MockHost`, serves the real Management API and panel, and needs
no CPA instance. Reach for it before reaching for a live CPA.

---

## 6. Testing expectations

Unit-test the deterministic logic; that is where the design's subtle rules live.
The race detector must stay clean — `make test-race` is the gate, not
`make test`.

Required coverage for anything you touch:

- **State lifecycle** — `FRESH` / `REFRESH_DUE` / `EXPIRED` / `MISSING`
  transitions at the exact threshold boundaries; bind → replace → refresh →
  delete; that a same-value bind refreshes TTL **without** writing history.
- **Time windows** — inside, outside, boundaries, multiple windows, disabled
  windows, empty day list (= every day), and **cross-midnight** windows such as
  `22:00–06:00` including the next-day-morning leg. This is the single most
  bug-prone function in the repo.
- **Proxy ordering** — never-used nodes first, then ascending `last_used_at`,
  ties by ID, cooldown and disabled nodes excluded.
- **Backoff** — one test per outcome, including that a non-target-length result
  does not schedule a one-minute retry.
- **Migration chain** — fresh install reaches `CurrentSchemaVersion`; re-running
  is a no-op; a database newer than the build is refused; checksum mismatch is
  refused; an interrupted migration restores its backup.
- **Switch matrix** — all five combinations in the design doc's table produce
  the documented `Capabilities`.
- **Routing strategies** — both `respect_cpa_priority` and `state_first`,
  including the fall-through to `DelegateBuiltin`.

Use `hostapi.MockHost` and an in-memory/temp-file SQLite. Inject clocks
(`states.Registry.SetClock`) instead of sleeping.

---

## 7. Verification status

Most of the original open questions were answered by reading the CPA v7.3.7
source rather than by running an instance; chapter 10 of the design document
records each one with its evidence. Only one item is still genuinely unverified.

**Answered from source:**

1. ~~Can a header injected in BeforeAuth be read in Scheduler and AfterAuth?~~ —
   the question is moot: the host publishes `selected_auth_id` /
   `selected_auth_index` in `Metadata`, so nothing is injected.
2. `SchedulerPickResponse` does accept a specific `AuthID`, and requires
   `Handled: true`. Candidates carry the host's `auth.ID`, not `auth_index`.
3. `host.auth.get` returns the raw on-disk auth file; `access_token` is a
   provider convention, not a contract.
4. `StreamChunkHeaderInitIndex == -1` is confirmed, and that call receives the
   unfiltered upstream response headers.
5. SQLite WAL concurrency, including concurrent `last_used_at` updates —
   covered by tests in `internal/storage/wal_test.go`.
6. Cross-midnight windows against DST — covered by
   `internal/probe/window_dst_test.go`.

**Still unverified — do not present as working:**

- **Does the `c-shared` build actually load into a real CPA instance?** The
  library exports the four symbols the loader expects and mirrors the official
  example's struct layout, but it has never been loaded. Until it has, the
  plugin is not production-ready, and behaviours that depend on the host calling
  back correctly (interception, scheduling, capture) are unproven end to end.

Do not remove this section until item 5 above has actually been exercised
against a running CPA.

---

## 8. Working agreement

- **The design document is the spec.** If code and document disagree, either fix
  the code or update the document — do not leave them divergent. Any behavioural
  change means editing `插件开发文档.md` in the same change.
- **One truth per rule.** The switch matrix lives in
  `settings.Values.Capabilities`; the naming convention lives here; the schema
  lives in `storage.migrations`. Do not duplicate a rule into a second place
  "for convenience".
- **Errors are values, not panics.** The plugin runs inside the CPA process: a
  panic takes the proxy down. Return errors, log them through `hostapi.Host.Log`,
  and degrade. Never `panic` on request-path input.
- **Comments explain constraints, not mechanics.** The doc comments in this repo
  cite design-document sections because that is the non-obvious part. Match that
  style; do not narrate what the next line does.
- **Every runtime behaviour is reachable from the panel or the settings table.**
  No hidden knobs.
- Prefer the standard library. The dependency set is deliberately thin (one
  SQLite driver); adding a dependency needs a reason that survives review.

---

## 9. Definition of done

A change is complete when:

1. `make fmt vet test-race` is clean.
2. `make build` produces both artefacts.
3. New behaviour has tests, including the boundary cases listed in section 6.
4. Schema changes bumped `CurrentSchemaVersion` with a new migration, and the
   checksum gate still passes against an existing database.
5. The design document was updated if behaviour changed.
6. Nothing in the change caches a credential, puts SQLite on the hot path,
   bypasses the master switch, or exposes a secret on a resource route.
7. **The status table in `README.md` was updated** if the change moved a
   development-plan item. That table is the single record of progress and is used
   to drive the work, so it must not drift from reality. Only mark an item ✅
   when it meets the bar stated under 状态判定口径 there — in particular, work
   that depends on real CPA callback behaviour or on browser rendering stays 🔄
   until it has actually been exercised in that environment.
