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

The full specification lives in [`docs/插件开发文档.md`](./docs/插件开发文档.md). Agent-facing
working rules live in [`AGENTS.md`](./AGENTS.md).

---

## 开发计划

对应设计文档 [§6.1 阶段划分](./docs/插件开发文档.md)。**本表是开发进度的唯一记录，完成一项就更新一次状态。**

| 图例 | 含义 |
|:---:|---|
| ✅ | 已完成 |
| 🔄 | 进行中 |
| ⬜ | 未开始 |

| # | 阶段 | 内容 | 产出（验收标准） | 状态 |
|:--:|---|---|---|:--:|
| 0 | **P0 — CPA ABI 适配层** | `internal/pluginabi`：把 CPA 宿主 ABI 适配到 `hostapi.Host`，导出注册入口，打通 Scheduler → AfterAuth → 响应头 → request.complete 的回调链路 | 插件可被真实 CPA 实例加载，端到端闭环跑通 | 🔄 |
| 1 | **P0 — 基础框架** | 插件项目骨架、CGO 构建、SQLite 初始化、PersistenceManager + Schema 迁移框架、Management API 基础路由 | 可编译加载的插件，启动后可恢复配置 | ✅ |
| 1b | **P0 — 与 CPA v7.3.7 对齐** | 读 CPA 源码核对接口，修正 correlation、候选身份、优先级分档、路由注册、资源路由等设计偏差 | 设计文档与真实 ABI 一致 | ✅ |
| 2 | **P0 — 账号同步** | AccountRegistry：定时同步 CPA Codex 账号，逐账号手动同步，按账号拉取并持久化模型清单 | 面板可展示账号列表，可手动刷新单个账号的状态与模型 | ✅ |
| 3 | **P0 — 代理池** | ProxyPool：节点增删改、健康状态、冷却排序、`last_used_at` 持久化 | 代理池可管理，选择顺序按冷却时间 | ✅ |
| 4 | **P0 — 探测引擎** | ProbeScheduler + TimeWindowManager + ProbeExecutor（含遍历所有代理直到命中目标），探测并发数默认 2 且可配置 | 可对指定 `(账号, 模型)` 发起定时探测 | ✅ |
| 5 | **P0 — 请求拦截** | CorrelationManager + RequestStateInjector + ResponseStateCollector | 请求头替换与响应头反向绑定全链路打通 | 🔄 |
| 6 | **P0 — 全局开关** | 定时探测开关 + 反向绑定开关，独立控制，状态持久化 | 两个开关可独立启停 | ✅ |
| 7 | **P0 — 绑定管理** | 绑定删除 + 历史记录 + Management API | 面板可删除绑定并查看历史 | ✅ |
| 8 | **P1 — 调度干预** | CredentialScheduler + `state_priority_enabled` 开关 + `SchedulerAcrossPriorities` | CPA 调度结果可被插件干预 | 🔄 |
| 9 | **P1 — 管理面板** | ResourceUI：前端面板覆盖全部运行时行为——开关与参数、时间窗口、代理池、账号与模型（含套餐/额度徽章、探测判定与探测开关）、逐账号同步、绑定删除与历史弹窗、调用历史弹窗（分页）、探测记录（账号过滤 + 分页） | 可视化操作全部功能 | 🔄 |
| 10 | **P1 — 自愈与退避** | State 失败自动失效、探测退避策略、被动续期 | 系统具备自愈能力 | ✅ |
| 11 | **P2 — 可观测性** | 探测历史查询、代理健康统计、State 状态可视化、逐请求调用历史（请求携带值 / 注入值 / HTTP 状态 / 响应值，24 小时） | 运维面板完善 | 🔄 |

### 状态判定口径

✅ 必须同时满足三条，缺一不可：

1. 组件自身代码完成；
2. **承载该产出验收标准的关键函数有单元测试，覆盖率不为 0**（`go test -coverpkg=./...` 实测，不看感觉）；
3. 在开发 harness 中实跑验证过。

另外：依赖 CPA 真实回调行为（#5、#8）或依赖浏览器渲染（#9、#11）的部分，在**真的被触发过之前**不计入 ✅。这条针对的是「只能靠真实环境才能证伪」这件事，不是永久豁免——一旦该行为已经被实测过（例如注入已在真实流量上跑通、面板交互已在浏览器中点过），就按上面三条正常判定。

### 各 ✅ 项的证据

覆盖率口径：`go test -coverpkg=./... -coverprofile=...`（全仓 76.5%）。数字随代码变化，改完请重测，不要沿用旧值。

| # | 证据 |
|:--:|---|
| 1 | `storage/migrate.go` 72.3%、`settings/settings.go` 89.8%、`management/api.go` 73.3%；`TestApp_StartsAndRestoresState` 覆盖「重启后恢复」（NF-04）；迁移链有「全新安装到达 CurrentSchemaVersion / 重跑是 no-op / 拒绝更新版本 / 校验和不符 / 中断迁移回滚备份」五条测试；已在 CPA v7.3.7 容器中真实加载（`make cpa-docker-up`）。 |
| 2 | `accounts.Sync` 88.2%、`RecordSignals` 100%、`Load` 88.9%、`ResolveAuthID` 100%、`ProbeEnabled` 100%、`SetProbeEnabled` 83.3%；`POST /accounts/sync`、`GET /accounts/models`、`PUT /accounts/models/probe` 三个端点均有测试；harness 实跑同步出 3 个账号。逐账号同步（`POST /accounts/sync?authIndex=..&models=1`）在 harness 中实测：返回更新后的账号视图（含 verdict、模型来源与拉取错误），未知账号返回 404；`Judge` 的判定矩阵、`LoadModels` 的「已存在则跳过 / 强制刷新 / 失败保留旧清单」、`account_models` 的往返与「删除清单连带删除探测开关」均有测试。 |
| 3 | `proxies` 包 90.4%；LRU 排序、冷却排除、同时间按 id 稳定排序、`ReplaceAll`、`Load` 恢复均有测试；`PUT /proxy-nodes` 端点有测试（含删除缺失节点与拒绝空 URL）；harness 中实测节点失败后进入 cooldown 且 `lastUsedAt` 持久化。 |
| 4 | `probe` 包 83.3%。`buildProbeBody` 100%、`newProxyClient` 100%、`attempt` 90.6%、`isModelUnsupported` 90.0%、`Probe` 88.4%、`newProbeHTTPRequest` 81.8%。测试用一个 `httptest` 假上游当作代理节点，因此**探测请求体形状（§3.6）、认证头、遍历顺序、五种结果分类、冷却规则、每代理一条历史、凭据每轮实时读取（NF-06）、以及不等待 SSE 流**都被真实断言，而非目测。 |
| 6 | `settings` 包 89.8%；五种开关组合的矩阵测试 + `app` 层「总开关关闭时四项能力全部旁路」的端到端测试。 |
| 7 | `states.Delete` 80%、`Bind` 90%；三个 HTTP 端点 `deleteBinding` 75.0%、`bindingHistory` 77.3%、`clearBindingHistory` 58.3% 均有测试，覆盖删除留痕、历史前缀截断与 `expand=1` 展开、清历史不影响绑定、重复删除幂等。 |
| 10 | `backoff.NextDelay` 82.4%、`ProxyCooldown` 100%、`Terminal`/`ProxyFault` 100%、`states.Invalidate` 100%、`intercept` 包 88.3%（含 `ObserveCompletion` 88.9%，即 §3.12 的失效触发条件与被动续期）。 |

### 进行中项的具体缺口

| # | 缺口 |
|:--:|---|
| 0 | **已在 CPA v7.3.7 容器中验证**：共享库加载成功、注册被接受、能力声明生效、Management API 与面板资源可达（`make cpa-docker-up`），且**真实流量已经驱动过链路**——探测 → 绑定 → 注入跑通（见下）。**仍差 `request.complete` 这一环**：`pluginabi.handleCompletion` 与 `app.ObserveCompletion` 覆盖率为 0，既没有单测也在真实流量中未被观察到触发。自愈（§3.12）正是挂在这个回调上——它要等一次真实失败请求才会走到。非流式路径 `handleResponseIntercept` / `ObserveResponse` 同样是 0（至今所有真实流量都是 SSE 流式）。补齐这两个回调的单测即可转 ✅。 |
| 5 | **请求注入已在真实流量上验证**：经 CPA 的请求确实带上了注入的头，`unresolvedAuth` 与 `noBinding` 均为 0。**反向绑定在这条路径上不生效**——实测响应到达插件时有 28 个响应头，其中没有 `X-Codex-Turn-State`；响应头 map 非空、插件读取无误，是上游不在这条链路返回它。已按运维决定接受为已知限制（它是降低探测频率的优化，非核心能力）。详见设计文档 §10.12。**注意**：接受该限制意味着本项的验收标准「请求头替换与响应头反向绑定全链路打通」中后一半永远不会满足，因此维持 🔄——除非把验收标准改成「注入闭环 + 反向绑定为可选优化」。 |
| 8 | 已确认 `AuthID` + `Handled: true` 可用，候选身份经 `auth.ID` → `auth_index` 映射，优先级分档由插件自己算。剩余缺口是真实环境验证，随 #0 一并进行；`state_first` 的「跨优先级档挑选持有 State 的账号」需要**至少两个账号**才能体现，当前环境只有一个。 |
| 9 | 路由已改为查询参数形式并全部注册；Management API 与面板静态资源均已在真实 CPA v7.3.7 中验证可达；**面板交互已在浏览器中实测**（账号过滤、探测记录分页与账号过滤、绑定历史弹窗 ESC 关闭、模型列表自动加载、标题栏计数）。本轮又实测了逐账号「同步」按钮、调用历史弹窗（分页、HTTP 状态配色、State 三列各自的空值文案）、绑定历史弹窗（代理列脱敏、行高一致、无横向溢出）与面板占满宿主页面（1600px 视口下 `main` 宽 1585px）。仍列 🔄 有两个原因：一是本表口径把依赖浏览器渲染的项排除在 ✅ 之外，二是 `GET /models` 端点（`listModels`）是全仓唯一 0% 覆盖率的端点，`app.Catalog`、`models.parseCatalog` 也随之未测。 |
| 11 | API 侧（探测历史查询、代理健康统计、请求管道计数、上游套餐与额度）已完成并在真实实例中验证：实测一次真实请求后 `plan` 由 `X-Codex-Plan-Type` 填入 `free`，`lastHeaderInit` 里能看到 `X-Codex-Primary/Secondary-Used-Percent` 等额度头。注意这些值**只在进程处理过真实请求后才有**——重启后 `plan` 为空是正常现象，不是缺陷（§10.10）。调用历史链路有端到端测试（注入 → 响应 → 完成三步驱动出一条完整记录，含三个 State 值与状态码），总开关关闭时不写任何行；`call_history` 的往返、分页、按龄清理、按账号清除均有测试。**尚未在真实流量中观察过**，因此仍为 🔄。 |

### 关于第 0 项

#0 不在设计文档 §6.1 的计划表内，是框架落地后暴露出来的前置项：它直接卡住 #5、#8 的最终验收。设计文档 §8「关键技术验证清单」的 12 项验证应当在这一步内完成。

### 覆盖率已知缺口

`go tool cover -func` 实测的 0% 函数，按重要性排列：

| 函数 | 说明 |
|---|---|
| `pluginabi.handleCompletion` | `request.complete` 回调的 ABI 适配层。`app.ObserveCompletion` 与 `intercept.ObserveCompletion` 现已有覆盖（100% / 89.7%，含本轮新增的端到端调用历史测试），剩下的 0% 只是这一层把宿主报文翻译成 `hostapi.Completion` 的代码——它要真实宿主才会执行。 |
| `pluginabi.handleResponseIntercept` / `app.ObserveResponse` / `intercept.ObserveResponse` | 非流式响应路径。真实流量至今全为 SSE 流式，因此从未执行过。 |
| `management.listModels` | `GET /models`，全仓唯一 0% 的端点；`app.Catalog`、`models.parseCatalog` 随之未测。 |
| `accounts.AuthID` / `accounts.CatalogHas` / `accounts.ModelConfigured` | 无调用方的导出方法，属清理项。原先同列的 `accounts.ProbeBlocked` 是「按账号健康状态跳过探测」的第二条实现路径——判定规则改窄之后它就成了会与 `Judge` 悄悄分叉的陷阱，已删除。 |
| `probe.ReplaceAll`（时间窗口） | AGENTS.md §6 称时间窗口是全仓最易出错的一处，替换路径却没有测试。 |

另有未被任何地方调用的导出方法，属清理项而非缺口：`app.TriggerProbe`、`app.ManagementAPI`、`app.DB`、`app.Correlation`、`intercept.RequestInjector.Correlation`、`intercept.CorrelationManager.Len`。

### 已验证的核心闭环

```
探测   SUCCESS_TARGET  len=292  经代理直连上游        ✅ 真实实例
绑定   写入 SQLite → 内存快照 → 排定 TTL×85% 退避     ✅
注入   经 CPA 的请求带上注入头，账号识别无误           ✅ 真实流量
观测   请求管道计数（注入/捕获/账号未知/跳过/丢弃）     ✅ 真实流量
账号   套餐与额度取自上游响应头                        ✅ 真实流量（实测填入 plan=free 与 5h/周 用量）
自愈   失败请求 → 失效绑定 → 立即重探                  ⚠️ 有单测（ObserveCompletion 88.9%），未经真实失败触发
```

「自愈」一行是**未被证伪**而非已证明：真实失败请求没有发生过，`request.complete` 回调也还没在真实环境被观察到。

### 建议的推进顺序

1. **补 #0 的两个回调单测**（`handleCompletion` / `handleResponseIntercept`）——这是唯一还卡着 #0 的东西，补完即可转 ✅。
2. **#9 面板收尾**——交互已实测通过，剩下 `GET /models` 端点的测试与「浏览器项不计入 ✅」这条口径的取舍。
3. **#8 调度干预的真实验证**——需要多账号环境才能体现「优先选择持有 State 的账号」。

---

## Quick start

Requires Go 1.26+ (see `go.mod`) and, for the shared library, a C compiler with
`CGO_ENABLED=1`. The Linux cross-build in `make build-linux` uses `golang:1.26-bookworm`.

```bash
make run
```

Then open <http://127.0.0.1:8787/v0/resource/plugins/codex-turn-state-manager/> and sign
in with the harness key `devkey`. That bare base path is a harness convenience — it
redirects to `/index.html`. On a real CPA the entry point is `/index.html` directly,
because CPA rejects a resource route whose path trims to empty.

The harness boots the real application against an in-memory mock of the CPA host and
serves the real Management API and admin panel, so the plugin can be developed without a
CPA instance and without touching real accounts.

```bash
make run ARGS="-accounts 8 -listen 127.0.0.1:9000"
```

`ARGS` is appended after the defaults, so it can override any of them.

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

## Install from the plugin store

CPA's plugin store reads a `registry.json` and installs from the releases it
points at. Add this repository's registry as an extra source in CPA's config:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/yangshoulai/codex-turn-state-manager/main/registry.json"
```

The plugin then appears in the store and installs in one step, landing at
`plugins/<goos>/<goarch>/codex-turn-state-manager-v<version>.so`. The store
writes the manifest back into `plugins.configs.codex-turn-state-manager.store`
so it can offer updates later.

The official registry is always included; `store-sources` only adds to it.

## Release

Publishing a release is what makes the plugin installable — nothing is uploaded
by hand. Push a version tag and the workflow does the rest:

```bash
git tag v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` then builds one archive per platform, generates
`checksums.txt`, and creates the GitHub release. `workflow_dispatch` reruns a
release for an existing tag and replaces its assets.

**The artifact naming is a contract, not a convention.** CLIProxyAPI looks up one
exact asset name and rejects an archive that does not match, so all of this is
enforced by `make release-archive` before anything is published:

| Rule | Value |
|---|---|
| Archive name | `codex-turn-state-manager_<version>_<goos>_<goarch>.zip` |
| Version | the tag without its `v` — `v0.1.0` publishes `0.1.0` |
| Contents | exactly one dynamic library, at the archive root, named `codex-turn-state-manager<ext>` or `codex-turn-state-manager-v<version><ext>` |
| Checksums | `checksums.txt` in the same release, sha256 of the **archive** |
| Installed to | `plugins/<goos>/<goarch>/codex-turn-state-manager-v<version>.so` |

Platforms built: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64,
windows/amd64. `c-shared` cannot be cross-compiled by setting `GOOS` alone, so
each entry in the workflow matrix builds natively — both macOS architectures come
from one runner because Apple's toolchain is a cross-compiler. Adding a platform
is one entry in `matrix.include`; the archive name follows automatically.

`make release-archive` builds for the machine running it, so it needs a real
version: pass `VERSION=` when the working tree is not on a tag.

### The registry entry

[`registry.json`](./registry.json) describes the plugin. It omits `install`,
which means `github-release`: the store reads this repository's latest release
and derives the version from its tag. Keeping a `version` in the file too is a
display fallback — the release tag is authoritative.

A `direct` install type is also supported if you would rather host artifacts
somewhere other than GitHub Releases; it pins an explicit URL and sha256 per
platform instead.

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
| Log | Records one row per intercepted request: what it carried, what was injected, what upstream answered, how it ended |

With the master switch off, all five are bypassed and the plugin is a no-op on the
request path.

The log is the one capability with no sub-switch of its own. It answers "what did the
plugin do to my request", which is exactly the question an operator has when they have
just turned one of the other four off.

### Components

| Path | Responsibility |
|---|---|
| `internal/hostapi` | The only place CPA is described. A Go interface port plus an in-memory mock. |
| `internal/pluginabi` | Adapter from the real CPA C-ABI onto `hostapi.Host`. The only package allowed to reference CGO. |
| `internal/version` | Build identity and the management / resource route prefixes. |
| `internal/settings` | Configuration, bounds, and the atomic runtime snapshot the hot path reads. |
| `internal/storage` | SQLite (WAL): schema migrations with checksums and backups, and the table stores. |
| `internal/accounts` | CPA account sync, the `AuthIndex` ⇄ `AuthID` mapping, plan claims, per-account model lists, and the probe verdict. |
| `internal/models` | Each account's own model catalog, the shared manifest fallback, and each model's reasoning floor. |
| `internal/states` | Bindings, their lifecycle, and the lock-free hot-path snapshot. |
| `internal/proxies` | Proxy pool, the per-(account, proxy) cooldown ledger, and least-recently-used selection. |
| `internal/probe` | Scan scheduling, time windows, backoff, proxy traversal, request shape. |
| `internal/intercept` | Correlation across interceptor stages, injection, capture, self-healing. |
| `internal/callhistory` | Per-request record assembly and the background writer that keeps SQLite off the request path. |
| `internal/headers` | The header names this plugin reads and the upstream signal parsing. |
| `internal/routing` | Interference in CPA's account selection. |
| `internal/management` | Management API handlers. |
| `internal/app` | Composition root; owns lifecycle and the request-path entry points. |
| `web` | The embedded admin panel (static assets only). |

### Key behaviours

**State shape.** The token carries its own block count and issue time. Ten blocks
(292 characters) is the personal rule and twelve (332) the team rule; the plan
decides which applies to an account.

**TTL runs from when the upstream minted the value**, not from when the plugin
stored it. The envelope carries an issue timestamp, so a value that spent most of
its life before reaching us does not get a fresh hour — and one that arrives
already past its TTL is recorded and dropped rather than displacing a binding
that still works. Without this, a value harvested late would be injected until
the upstream stopped honouring it, which looks like "it worked, then it stopped"
with nothing in between.

**State lifecycle.** A binding is `FRESH` until 85% of its TTL has elapsed, then
`REFRESH_DUE` (still injected, but scheduled for renewal), then `EXPIRED`. Default TTL is
60 minutes.

**Only the right shape binds.** A harvested value that is not the shape its
account produces is recorded and discarded — never bound.

The shape is **per plan, not one global length**: the token is a version byte, an
issue timestamp, and a run of ciphertext blocks, and the tier decides how many
blocks. Ten blocks encode to 292 characters and are what personal accounts
produce; twelve encode to 332 and are what Team and Business accounts produce. A
Team value is a plus account's wrong length, so one `target_state_length` cannot
describe both. The plugin reads the block count from the token itself, resolves
the expected count from the account's plan (the plan on the response first, then
the stored one), and only falls back to comparing against `target_state_length`
when the plan is unknown — which is also the one case that setting governs.

The panel shows the resolved length on each account, so a probe log can be read
against the rule actually being applied.

**Least-recently-used proxy rotation.** Each probe walks the pool in LRU order, up to
`max_proxies_per_probe` nodes,
stamping `last_used_at` *before* each request so concurrent probes never pick the same
node. The walk stops early on a target hit, or on an account- or model-level error that
another proxy could not fix.

**Failure classification.** Timeouts, connect errors, TLS errors and 407 are proxy
faults. Upstream 400/401/403/429 are not, and never evict a healthy node — the walk stops
on them instead.

**Cooling is per account, not per proxy.** The same node returns the target state length
for one account and a non-target length for another, because the upstream decides per
account — so a node-level cooldown lets one account's failures take a working node away
from everyone else. Availability is therefore recorded per `(account, proxy)` pair: a
node benched for one account is still selectable by every other, and a round ends for one
account only when all of *its* nodes are benched. The node list shows how many accounts
currently have each node benched; the 详情 button opens the per-account rows with a reset
on each, and the section has a global reset.

**Cooldown ladder.** 1, 2, 4, 8, 16, 32 then 64 minutes, doubling and holding at the cap;
at the cap the pair's counter resets so the next failure starts at a minute again, which
is what keeps an outage longer than an hour from removing a pair permanently. Two things
earn a cooldown: a proxy fault, and a response whose state length is not the target — the
request succeeded, but this node is not yielding what that account needs.

**Scanning is not probing.** A one-minute scan only enqueues probes that are actually
due; a non-target-length result backs off for minutes, not seconds.

**A pair that keeps missing backs off further.** The first few wrong-shaped answers
retry at the base cadence — a transient deserves one — and the interval then doubles each
round up to `non_target_backoff_cap_min` (default 30). A model that never yields the
target shape would otherwise be re-probed every few minutes forever, and every round
walks the pool, so the cost lands on the proxies rather than on the one pair that is
never going to work. The run resets on any other outcome, so a model that starts working
is retried at the base cadence immediately.

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
| `state_priority_enabled` | `true` | Whether the plugin may interfere in CPA's account choice. Off still injects state it already holds |
| `scan_interval_sec` | `60` | How often the scheduler checks for due pairs |
| `probe_concurrency` | `2` | Simultaneous pair probes (1–32); each walks the pool serially |
| `state_ttl_min` | `60` | Binding lifetime, counted from the value's issue time |
| `refresh_threshold_pct` | `15` | Re-probe once this much of the TTL remains |
| `target_state_length` | `292` | Fallback length, used only when an account's plan is unknown; a known plan decides the shape |
| `max_probe_duration_sec` | `90` | Wall-clock cap on one pair's traversal |
| `max_proxies_per_probe` | `10` | Nodes one round may try before it stops, whatever the pool depth |
| `non_target_backoff_cap_min` | `30` | Ceiling for the growing retry interval of a pair that keeps missing |
| `account_sync_interval_sec` | `300` | How often the account list is re-read from CPA |
| `probe_history_retention_hours` | `24` | Probe rows older than this are pruned |
| `account_routing_strategy` | `respect_cpa_priority` | or `state_first` |

Time windows restrict when probing runs. Several windows may be configured, each with an
optional day-of-week set; a window may cross midnight (`22:00–06:00`). With no *enabled*
window, probing is unrestricted — a window that is disabled, entered wrong, or absent all
mean 24/7 probing, so the panel's config line reports the scheduler's live decision
(`窗口 08:00–02:00（当前禁止探测）`) rather than just counting rows.

The manual probe button on a model row **ignores the window deliberately**: it is an
explicit request for one probe now, and a pair whose scheduled probing is off is exactly
the one someone would want to test by hand. Its tooltip says so.

**Windows are evaluated in the CPA process's local timezone, not yours.** The panel
renders probe timestamps in your browser's timezone, so the two can be a long way apart —
a server on `TZ=Pacific/Honolulu` with a browser in UTC+8 differs by eighteen hours, and
an `08:00–02:00` window then admits probes at 07:00 by the browser's clock while the server
is looking at 13:00.

The config line states the clock the decision is made on, and names the gap when there is
one:

```
窗口 08:00–02:00 · 服务器 14:45:38 HST（与浏览器相差 18 小时，窗口按服务器时间判定）
```

**Set `TZ` on the CPA container** if you want the window to follow your own hours:

```yaml
environment:
  - TZ=Asia/Shanghai
```

Without it the window is interpreted wherever the process thinks it is, which on a VPS is
frequently UTC or the host's default.

Proxy nodes are ordinary HTTP/HTTPS/SOCKS5 URLs.

## Management API

All routes are under `/v0/management/codex-turn-state-manager/` and require CPA's
management auth.

**Note the path is flat — there is no `plugins/` segment.** The host offers plugins
`BasePath` `/v0/management` and resolves routes as `<BasePath> + <Path>`, so the plugin
segment is ours to choose. Nested under `plugins/` it would share a namespace with
CLIProxyAPI's own plugin administration API, and third-party management front ends that
treat `plugins` as a reserved segment would demand their own credentials before reaching
it. Resource routes are the opposite: the host inserts `/plugins/<id>` there itself.

```
GET    /status                            # includes the live time-window decision
GET    /settings
PUT    /settings
GET    /time-windows
POST   /time-windows
PUT    /time-windows?id=…
DELETE /time-windows?id=…
GET    /models
GET    /accounts
POST   /accounts/sync
GET    /accounts/models?authIndex=…     # omit authIndex for every account at once
POST   /accounts/models/probe-now?authIndex=…&model=…
DELETE /accounts/models?authIndex=…&model=…
PUT    /accounts/models/probe?authIndex=…&model=…
GET    /bindings
DELETE /bindings?authIndex=…&model=…
GET    /bindings/history?authIndex=…&model=…&expand=1&limit=&offset=
DELETE /bindings/history?authIndex=…&model=…
GET    /proxy-nodes
PUT    /proxy-nodes
POST   /proxy-nodes/reset?nodeId=…&authIndex=…
POST   /proxy-nodes/reset-all
GET    /proxy-nodes/cooldowns?nodeId=…
GET    /probe-history?authIndex=&model=&limit=50&offset=0
```

**Every dynamic segment is a query parameter, and that is a host constraint rather than a
style choice.** CPA dispatches management routes by exact path — `:`, `*` and `..` are
rejected at registration — so `/bindings/{authIndex}/{model}` is not registrable. The path
must also carry the `plugins/<pluginID>` segment itself. `GET /probe-history` returns
`total` alongside the page so the panel can paginate.

`GET /status` reports `version`, `now`, `settings`, the resolved `capabilities`,
proxy counts, and `pipeline`: counters for requests reaching the injection stage,
injections performed, accounts that could not be resolved, bindings missing, state
captured from traffic, and self-healing invalidations. A header rewrite leaves no other
trace, so these are how "is the plugin actually doing anything?" gets answered without
logging every request. `pipeline.lastHeaderInit` is a snapshot of what the most recent
response actually carried — which is how the reverse-bind limitation above was
established.

The panel's own assets are served from `/v0/resource/plugins/codex-turn-state-manager/`,
which bypasses management auth. The two cacheable assets carry a content hash in
their route (`/app.<hash>.js`, `/style.<hash>.css`) and are served `immutable`;
`index.html` keeps a stable address and is served `no-cache`, because it is what
records which asset build is current. That pairing is what makes an update take
effect immediately even behind a CDN that caches by file extension. Note the entry point is `/index.html`, not the bare
base path: CPA rejects a resource route whose path trims to empty, so a bare `/`
cannot be registered. Nothing secret is ever served from there: state values
are returned as prefixes through the Management API, and the management key is held in
page memory only.

## Data

`state.db` (SQLite, WAL) lives in the plugin data directory, with pre-migration backups
under `backups/`. Schema changes are forward-only migrations with SHA-256 checksums; a
database written by a newer plugin build refuses to start rather than risk corruption.

## Licence

MIT — see [LICENSE](./LICENSE).
