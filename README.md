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

## 开发计划

对应设计文档 [§6.1 阶段划分](./插件开发文档.md)。**本表是开发进度的唯一记录，完成一项就更新一次状态。**

| 图例 | 含义 |
|:---:|---|
| ✅ | 已完成 |
| 🔄 | 进行中 |
| ⬜ | 未开始 |

| # | 阶段 | 内容 | 产出（验收标准） | 状态 |
|:--:|---|---|---|:--:|
| 0 | **P0 — CPA ABI 适配层** | `internal/pluginabi`：把 CPA 宿主 ABI 适配到 `hostapi.Host`，导出注册入口，打通 Scheduler → AfterAuth → 响应头 → request.complete 的回调链路 | 插件可被真实 CPA 实例加载，端到端闭环跑通 | 🔄 |
| 1 | **P0 — 基础框架** | 插件项目骨架、CGO 构建、SQLite 初始化、PersistenceManager + Schema 迁移框架、Management API 基础路由 | 可编译加载的插件，启动后可恢复配置 | 🔄 |
| 1b | **P0 — 与 CPA v7.3.7 对齐** | 读 CPA 源码核对接口，修正 correlation、候选身份、优先级分档、路由注册、资源路由等设计偏差 | 设计文档与真实 ABI 一致 | ✅ |
| 2 | **P0 — 账号同步** | AccountRegistry：定时同步 CPA Codex 账号 | 面板可展示账号列表 | ✅ |
| 3 | **P0 — 代理池** | ProxyPool：节点增删改、健康状态、冷却排序、`last_used_at` 持久化 | 代理池可管理，选择顺序按冷却时间 | ✅ |
| 4 | **P0 — 探测引擎** | ProbeScheduler + TimeWindowManager + ProbeExecutor（含遍历所有代理直到命中目标），探测并发数默认 2 且可配置 | 可对指定 `(账号, 模型)` 发起定时探测 | ✅ |
| 5 | **P0 — 请求拦截** | CorrelationManager + RequestStateInjector + ResponseStateCollector | 请求头替换与响应头反向绑定全链路打通 | 🔄 |
| 6 | **P0 — 全局开关** | 定时探测开关 + 反向绑定开关，独立控制，状态持久化 | 两个开关可独立启停 | ✅ |
| 7 | **P0 — 绑定管理** | 绑定删除 + 历史记录 + Management API | 面板可删除绑定并查看历史 | ✅ |
| 8 | **P1 — 调度干预** | CredentialScheduler + `state_priority_enabled` 开关 + `SchedulerAcrossPriorities` | CPA 调度结果可被插件干预 | 🔄 |
| 9 | **P1 — 管理面板** | ResourceUI：简洁前端 + 时间窗口配置 + 历史弹窗 + 代理池展示 + 探测并发配置 | 可视化操作全部功能 | 🔄 |
| 10 | **P1 — 自愈与退避** | State 失败自动失效、探测退避策略、被动续期 | 系统具备自愈能力 | ✅ |
| 11 | **P2 — 可观测性** | 探测历史查询、代理健康统计、State 状态可视化 | 运维面板完善 | 🔄 |

### 状态判定口径

✅ 必须同时满足三条，缺一不可：

1. 组件自身代码完成；
2. **承载该产出验收标准的关键函数有单元测试，覆盖率不为 0**（`go test -coverpkg=./...` 实测，不看感觉）；
3. 在开发 harness 中实跑验证过。

另外：依赖 CPA 真实回调行为（#5、#8）或依赖浏览器渲染（#9、#11）的部分，不计入 ✅。

### 各 ✅ 项的证据

| # | 证据 |
|:--:|---|
| 2 | `accounts.Sync` 81.5%、`Load` 88.9%、`ResolveAuthID` 100%、`ProbeEnabled` 100%、`SetProbeEnabled` 83.3%；`POST /accounts/sync`、`GET /accounts/{authIndex}/models`、`PUT /accounts/{authIndex}/models/{model}/probe` 三个端点均有测试；harness 实跑同步出 3 个账号。 |
| 3 | `proxies` 包 86.5%；LRU 排序、冷却排除、同时间按 id 稳定排序、`ReplaceAll`、`Load` 恢复均有测试；`PUT /proxy-nodes` 端点有测试（含删除缺失节点与拒绝空 URL）；harness 中实测节点失败后进入 cooldown 且 `lastUsedAt` 持久化。 |
| 4 | `probe` 包 77.9%。`buildProbeBody` 100%、`newProxyClient` 100%、`attempt` 90.6%、`isModelUnsupported` 90.0%、`Probe` 88.4%、`newProbeHTTPRequest` 81.8%。测试用一个 `httptest` 假上游当作代理节点，因此**探测请求体形状（§3.6）、认证头、遍历顺序、五种结果分类、冷却规则、每代理一条历史、凭据每轮实时读取（NF-06）、以及不等待 SSE 流**都被真实断言，而非目测。 |
| 6 | `settings` 包 85.4%；五种开关组合的矩阵测试 + `app` 层「总开关关闭时四项能力全部旁路」的端到端测试。 |
| 7 | `states.Delete` 80%、`Bind` 90%，`intercept` 侧失效路径 85.4%；三个 HTTP 端点 `deleteBinding` 66.7%、`bindingHistory` 80.0%、`clearBindingHistory` 60.0% 均有测试，覆盖删除留痕、历史前缀截断与 `expand=1` 展开、清历史不影响绑定、重复删除幂等。 |
| 10 | `backoff.NextDelay` 82.4%、`ProxyCooldown` 100%、`Terminal`/`ProxyFault` 100%、`states.Invalidate` 100%；`intercept` 包 85.4%，含失效触发条件与被动续期。 |

### 进行中项的具体缺口

| # | 缺口 |
|:--:|---|
| 0 | **已在 CPA v7.3.7 容器中验证**：共享库加载成功、注册被接受、能力声明生效、Management API 可达（`make cpa-docker-up`）。**仍差真实流量驱动**——环境里没有 Codex 账号，拦截器/调度器/响应捕获从未被真实请求触发过。 |
| 1 | 仅差「可被 CPA 加载」这一条，见 #0。 |
| 5 | **请求注入已在真实流量上验证**：经 CPA 的请求确实带上了注入的头，`unresolvedAuth` 与 `noBinding` 均为 0。**反向绑定在这条路径上不生效**——实测响应到达插件时有 28 个响应头，其中没有 `X-Codex-Turn-State`；响应头 map 非空、插件读取无误，是上游不在这条链路返回它。已按运维决定接受为已知限制（它是降低探测频率的优化，非核心能力）。详见设计文档 §10.12。 |
| 8 | 已确认 `AuthID` + `Handled: true` 可用，候选身份经 `auth.ID` → `auth_index` 映射，优先级分档由插件自己算。剩余缺口是真实环境验证，随 #0 一并进行。 |
| 9 | 路由已改为查询参数形式并全部注册，`internal/management` 内**已无 0% 覆盖率的端点**；Management API 与面板静态资源均已在真实 CPA v7.3.7 中验证可达。仍缺的是**在浏览器里实际点击验证交互**。 |
| 11 | API 侧（探测历史查询、代理健康统计、请求管道计数、上游套餐与额度）已完成并在真实实例中验证；可视化部分随 #9 一并验证。 |

### 关于第 0 项

#0 不在设计文档 §6.1 的计划表内，是框架落地后暴露出来的前置项，也是当前唯一的阻塞点：它直接卡住 #1、#5、#8 的最终验收。设计文档 §8「关键技术验证清单」的 12 项验证应当在这一步内完成。

### 已验证的核心闭环

```
探测   SUCCESS_TARGET  len=292  经代理直连上游        ✅ 真实实例
绑定   写入 SQLite → 内存快照 → 排定 TTL×85% 退避     ✅
注入   经 CPA 的请求带上注入头，账号识别无误           ✅ 真实流量
观测   请求管道计数（注入/捕获/账号未知/自愈）          ✅
账号   套餐与额度取自上游响应头                        ✅ 真实流量
```

### 建议的推进顺序

1. **#9 面板浏览器实测**——功能与数据都已就位，剩下是交互层面的收尾。
2. **#11 可观测性收尾**——随 #9 一并完成。
3. **#8 调度干预的真实验证**——需要多账号环境才能体现「优先选择持有 State 的账号」。

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

`GET /status` also reports `pipeline`: counters for requests reaching the
injection stage, injections performed, accounts that could not be resolved,
bindings missing, state captured from traffic, and self-healing invalidations.
A header rewrite leaves no other trace, so these are how "is the plugin actually
doing anything?" gets answered without logging every request. The same payload
carries `lastHeaderInit`, a snapshot of what the most recent response actually
carried — which is how the reverse-bind limitation above was established.

The panel's own assets are served from `/v0/resource/plugins/codex-turn-state-manager/`,
which bypasses management auth. Note the entry point is `/index.html`, not the bare
base path: CPA rejects a resource route whose path trims to empty, so a bare `/`
cannot be registered. Nothing secret is ever served from there: state values
are returned as prefixes through the Management API, and the management key is held in
page memory only.

## Data

`state.db` (SQLite, WAL) lives in the plugin data directory, with pre-migration backups
under `backups/`. Schema changes are forward-only migrations with SHA-256 checksums; a
database written by a newer plugin build refuses to start rather than risk corruption.

## Licence

Not yet specified.
