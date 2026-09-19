/*
 * Codex Turn State Manager -- admin panel.
 *
 * The management key is held in a module-scoped variable for the lifetime of
 * the page only. It is deliberately never written to localStorage,
 * sessionStorage, a cookie, or the URL, per the plugin's security rules.
 */
"use strict";

const BASE = "/v0/management/codex-turn-state-manager";

/*
 * The management key can be inherited rather than typed.
 *
 * CPA's management center keeps its session in localStorage under
 * "cli-proxy-auth", obfuscated with a prefix plus an XOR key derived from the
 * origin and user agent. Because this panel is served from the same origin, it
 * can read that session and skip asking for a key the operator has already
 * entered.
 *
 * Read-only on purpose: this panel never writes the key anywhere. Persisting it
 * is CPA's decision, made in the management center, not ours.
 *
 * The format is not a published contract -- it was read out of the shipped
 * management bundle and cross-checked against a plugin that does the same. If
 * it ever changes, decoding fails and the panel falls back to asking.
 */
const CPA_SESSION_KEY = "cli-proxy-auth";
const CPA_OBFUSCATION_SALT = "cli-proxy-api-webui::secure-storage";

/*
 * Two obfuscation versions are in the wild under the same storage key.
 *
 * v1 is what CPA's own management center writes, and it derives the XOR key
 * from the origin *and the user agent*. v2 is what CPA-Manager-Plus writes
 * after its migration, and it drops the user agent -- precisely because tying a
 * stored session to the UA breaks it whenever the browser updates itself.
 *
 * Reading only v1 is why a session written by the other panel came back
 * "format not recognised" and the operator was asked for a key they had already
 * entered. Both are accepted; neither is written, here or anywhere else.
 */
const OBFUSCATION = [
  { prefix: "enc::v2::", key: () => `${CPA_OBFUSCATION_SALT}|v2|${location.host}` },
  { prefix: "enc::v1::", key: () => `${CPA_OBFUSCATION_SALT}|${location.host}|${navigator.userAgent}` },
];

function xorBytes(bytes, keyBytes) {
  const out = new Uint8Array(bytes.length);
  for (let i = 0; i < bytes.length; i++) out[i] = bytes[i] ^ keyBytes[i % keyBytes.length];
  return out;
}

// decodePanelStorage accepts either obfuscation version, or plain JSON.
//
// The version is taken from the prefix rather than tried in turn: an XOR with
// the wrong key produces bytes that decode to garbage instead of throwing, so
// guessing would risk parsing nonsense rather than failing cleanly.
function decodePanelStorage(raw) {
  if (!raw) return null;
  let text = raw;
  for (const { prefix, key } of OBFUSCATION) {
    if (!text.startsWith(prefix)) continue;
    const binary = atob(text.slice(prefix.length));
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    text = new TextDecoder().decode(xorBytes(bytes, new TextEncoder().encode(key())));
    break;
  }
  return JSON.parse(text);
}

/*
 * readInheritedKey reports what could be inherited, and why not when it could
 * not. The distinction matters to the operator: the management center only
 * persists the key when "remember password" was ticked at login, so a session
 * that exists but carries no key is a different problem from no session at all,
 * and the panel should say which one it hit rather than silently showing a
 * login form.
 */
function readInheritedKey() {
  let raw = null;
  try {
    raw = localStorage.getItem(CPA_SESSION_KEY);
  } catch {
    return { key: "", reason: "storage-unavailable" };
  }
  if (!raw) return { key: "", reason: "no-session" };

  let state;
  try {
    const parsed = decodePanelStorage(raw);
    state = parsed && parsed.state ? parsed.state : parsed;
  } catch (err) {
    // Worth surfacing: a decode failure means the stored blob was written with
    // a different origin or user agent, which is the one cause the operator
    // cannot see from the outside.
    console.warn("[turn-state] could not decode " + CPA_SESSION_KEY +
      " for this origin/user-agent:", err && err.message);
    return { key: "", reason: "undecodable" };
  }
  if (!state) return { key: "", reason: "undecodable" };

  const value = state.managementKey;
  if (typeof value !== "string" || !value.trim()) {
    console.info("[turn-state] session found but it carries no managementKey; " +
      "fields present: " + Object.keys(state).join(", "));
    return { key: "", reason: "session-without-key" };
  }
  return { key: value.trim(), reason: "ok" };
}

// inheritHint turns a failure reason into something the operator can act on.
function inheritHint(reason) {
  switch (reason) {
    case "session-without-key":
      return "检测到管理面板会话，但其中没有保存密钥 —— 登录管理面板时需勾选「记住密码」，否则密钥只存在于页面内存中，插件无法读取。你也可以直接在此输入。";
    case "undecodable":
      return "检测到管理面板会话，但格式无法识别（CPA 可能更改了存储格式）。请在此手动输入密钥。";
    case "storage-unavailable":
      return "浏览器禁止访问本地存储，无法沿用管理面板会话。请手动输入密钥。";
    default:
      return "";
  }
}

// Defaults mirror settings.Defaults() on the Go side. They exist so the panel
// can offer a restore action and label each field; the server remains the
// authority, and whatever it returns overwrites these on load.
const DEFAULTS = {
  globalEnabled: true,
  globalProbeEnabled: true,
  globalReverseBindEnabled: true,
  statePriorityEnabled: true,
  scanIntervalSec: 60,
  probeConcurrency: 2,
  stateTtlMin: 60,
  refreshThresholdPct: 15,
  targetStateLength: 292,
  maxProbeDurationSec: 90,
  probeHistoryRetentionHours: 24,
  maxProxiesPerProbe: 10,
  routingStrategy: "respect_cpa_priority",
};

let managementKey = "";
let settingsState = null;
let proxiesState = [];
let windowsState = [];
let accountsState = [];
let expanded = new Set();
// modelCache holds every account's models, filled by one bulk request per
// refresh. modelsLoaded distinguishes "not fetched yet" from "this account has
// no models", which read the same on screen otherwise.
const modelCache = new Map();
let modelsLoaded = false;
let modelsError = "";
// windowState is the scheduler's live view of the time window, from /status.
let windowState = null;
// Seed model names for the add control. Suggestions only: the plugin cannot
// read an account's real model list from CPA, so nothing here is probed until
// an operator adds it.
let probeTotal = 0;
let probeOffset = 0;
let probeAccountFilter = "";
let accountFilter = "";
let modelCatalog = [];
let modelCatalogNote = "";

/* ------------------------------------------------------------------ utils */

const $ = (id) => document.getElementById(id);

/*
 * on binds a listener without letting a missing element take the whole script
 * down.
 *
 * The panel is a single file of top-level bindings, so one null lookup used to
 * abort everything after it -- including the bootstrap that decides whether to
 * inherit a key or ask for one. The visible symptom was a login form, which
 * pointed at authentication rather than at the missing element that actually
 * caused it.
 */
function on(id, event, handler) {
  const node = $(id);
  if (!node) {
    console.error(`[turn-state] element #${id} is missing; binding ${event} skipped`);
    return;
  }
  node.addEventListener(event, handler);
}

// fail shows an unrecoverable panel error rather than leaving a blank page or a
// misleading login form.
function fail(message) {
  const node = $("fatal");
  if (node) {
    node.textContent = message;
    node.hidden = false;
  }
}

window.addEventListener("error", (event) => {
  console.error("[turn-state] uncaught error:", event.message, event.filename, event.lineno);
});

function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k === "html") node.innerHTML = v;
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else if (k === "dataset") Object.assign(node.dataset, v);
      else node.setAttribute(k, v === true ? "" : String(v));
    }
  }
  for (const child of [].concat(children || [])) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

/*
 * Redraw only when the data actually changed.
 *
 * The panel polls every 15s. Rebuilding a section regardless of whether
 * anything moved made the page flicker and drop text selection twice a minute,
 * which is why these sections keep a fingerprint of what they last rendered.
 */
const rendered = new Map();

function changed(key, payload) {
  const next = JSON.stringify(payload);
  if (rendered.get(key) === next) return false;
  rendered.set(key, next);
  return true;
}

function invalidate(key) { rendered.delete(key); }

let toastTimer = null;
function toast(message, bad) {
  const node = $("toast");
  node.textContent = message;
  node.classList.toggle("bad", !!bad);
  node.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { node.hidden = true; }, bad ? 5200 : 2600);
}

function setConn(text, cls) {
  const node = $("conn");
  node.textContent = text;
  node.className = "pill " + cls;
}

async function api(method, path, body) {
  const options = {
    method,
    headers: { "X-Management-Key": managementKey },
    cache: "no-store",
  };
  if (body !== undefined) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  const resp = await fetch(BASE + path, options);
  const text = await resp.text();
  let parsed = false;
  let payload = null;
  if (text) {
    try { payload = JSON.parse(text); parsed = true; } catch { payload = { error: text }; }
  }
  if (!resp.ok) {
    // A 404 with no JSON body is the router answering, not this plugin: the
    // management route the page asked for does not exist. In practice that
    // means the plugin was updated underneath an open tab -- the route set is
    // rebuilt on reload, so old paths stop answering immediately -- while this
    // page is still running the previous version's code. Saying "404 page not
    // found" leaves the operator with nothing to do; naming the fix does not.
    if (resp.status === 404 && !parsed) {
      const stale = new Error(
        "管理接口返回 404：页面可能来自旧版本的插件，而插件已经更新。请刷新页面（Ctrl/Cmd+Shift+R 可绕过缓存）。");
      stale.status = 404;
      stale.stalePage = true;
      throw stale;
    }
    const err = new Error((payload && payload.error) || `HTTP ${resp.status}`);
    err.status = resp.status;
    throw err;
  }
  return payload;
}

// qs builds a query string. CPA dispatches plugin management routes by exact
// path, so anything variable travels as a query parameter rather than a path
// segment.
function qs(params) {
  const search = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null) search.set(k, String(v));
  }
  return search.toString();
}

function fmtDuration(seconds) {
  if (seconds === null || seconds === undefined) return "—";
  if (seconds <= 0) return "已过期";
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  if (m >= 60) return `${Math.floor(m / 60)}h ${m % 60}m`;
  if (m > 0) return `${m}m ${String(s).padStart(2, "0")}s`;
  return `${s}s`;
}

// fmtClock drops the date. The probe table keeps 24 hours of history, so the
// date is almost always today and the column is the widest thing in the row;
// the full timestamp stays in the cell's title.
function fmtClock(value) {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  const pad = (n) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function fmtTime(value) {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  const pad = (n) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function fmtAgo(value) {
  if (!value) return "从未";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  const secs = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
  if (secs < 60) return `${secs}s 前`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m 前`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h 前`;
  return `${Math.floor(secs / 86400)}d 前`;
}

// planBadge renders the subscription tier. It comes from the upstream's own
// response headers when traffic has been seen, and falls back to the
// credential's id_token claim -- which is frequently absent, and was the only
// source before.
function planBadge(plan) {
  if (!plan || !plan.type) return null;
  const label = plan.label || plan.type;
  const title = plan.activeUntil
    ? `套餐 ${label}，有效期至 ${fmtTime(plan.activeUntil)}`
    : `套餐 ${label}`;
  // Tint by tier. The claim value is whatever upstream sent, so an unrecognised
  // tier falls through to the neutral badge rather than being forced into a
  // colour that would imply a position in a ranking we have not verified.
  const tier = String(plan.type).toLowerCase().replace(/[^a-z0-9]/g, "");
  const known = ["free", "plus", "pro", "team", "enterprise"];
  const modifier = known.includes(tier) ? " pill-plan-" + tier : "";
  return el("span", { class: "pill pill-plan" + modifier, text: label, title });
}

// quotaBadge renders the rate-limit windows the upstream reported, which is the
// account state CPA does not expose to plugins.
function quotaBadge(quota) {
  if (!quota) return null;
  const parts = [];
  if (quota.primaryUsedPercent != null) {
    const window = quota.primaryWindowMinutes ? `/${quota.primaryWindowMinutes}m` : "";
    parts.push(`5h ${quota.primaryUsedPercent}%${window}`);
  }
  if (quota.secondaryUsedPercent != null) {
    const window = quota.secondaryWindowMinutes ? `/${quota.secondaryWindowMinutes}m` : "";
    parts.push(`周 ${quota.secondaryUsedPercent}%${window}`);
  }
  if (!parts.length) return null;

  const worst = Math.max(quota.primaryUsedPercent ?? 0, quota.secondaryUsedPercent ?? 0);
  const cls = worst >= 100 ? "pill-bad" : worst >= 80 ? "pill-warn" : "pill-idle";
  const resets = [quota.primaryResetAt, quota.secondaryResetAt].filter(Boolean);
  const title = resets.length ? `上游报告的额度使用率；最近重置 ${fmtTime(resets[0])}` : "上游报告的额度使用率";
  return el("span", { class: "pill " + cls, text: parts.join(" · "), title });
}

const STATUS_PILL = {
  fresh: ["pill-ok", "有效"],
  refresh_due: ["pill-warn", "待刷新"],
  expired: ["pill-bad", "已过期"],
  missing: ["pill-idle", "无绑定"],
};

function statusPill(status) {
  const [cls, label] = STATUS_PILL[status] || STATUS_PILL.missing;
  return el("span", { class: "pill " + cls, text: label });
}

const OUTCOME_PILL = {
  SUCCESS_TARGET: "pill-ok",
  SUCCESS_NON_TARGET: "pill-warn",
  RATE_LIMIT: "pill-warn",
  UPSTREAM_ERROR: "pill-warn",
  NETWORK_ERROR: "pill-bad",
  AUTH_ERROR: "pill-bad",
  MODEL_UNSUPPORTED: "pill-bad",
  PROBE_NO_PROXY_AVAILABLE: "pill-idle",
  PROBE_TIMEOUT_ALL_PROXIES: "pill-warn",
  ABORTED: "pill-idle",
};

function table(headers, rows, emptyText) {
  if (!rows.length) return el("div", { class: "empty", text: emptyText });
  const thead = el("thead", null, el("tr", null, headers.map((h) =>
    el("th", { text: h.label, class: h.class || null }))));
  const tbody = el("tbody", null, rows);
  return el("table", { class: "table" }, [thead, tbody]);
}

/* ---------------------------------------------------------------- config */

const CONFIG_OPEN_KEY = "codex-turn-state-manager:config-open";

function setConfigOpen(open) {
  const body = $("config-body");
  const toggle = $("config-toggle");
  if (!body || !toggle) return;
  body.hidden = !open;
  toggle.setAttribute("aria-expanded", open ? "true" : "false");
  try { localStorage.setItem(CONFIG_OPEN_KEY, open ? "1" : "0"); } catch { /* preference only */ }
}

on("config-toggle", "click", () => {
  setConfigOpen($("config-body").hidden);
});

function restoreConfigOpen() {
  let open = false;
  try { open = localStorage.getItem(CONFIG_OPEN_KEY) === "1"; } catch { /* default closed */ }
  setConfigOpen(open);
}

// refreshConfigSummary gives the collapsed header something useful to say, so
// the state is legible without expanding it.
function refreshConfigSummary() {
  const node = $("config-summary");
  if (!node || !settingsState) return;
  const parts = [];
  parts.push(settingsState.globalEnabled ? "总开关 开" : "总开关 关");
  if (settingsState.globalEnabled) {
    parts.push(settingsState.globalProbeEnabled ? "探测 开" : "探测 关");
    parts.push(settingsState.globalReverseBindEnabled ? "反向绑定 开" : "反向绑定 关");
    parts.push(settingsState.statePriorityEnabled ? "调度干预 开" : "调度干预 关");
  }
  // The window state, as the scheduler sees it right now rather than as the
  // table was last rendered: a window that is disabled, mis-entered, or missing
  // all mean unrestricted probing, and they look identical otherwise.
  const enabledWindows = windowsState.filter((w) => w.enabled);
  if (!enabledWindows.length) {
    parts.push("全天可探测");
  } else {
    const ranges = enabledWindows
      .map((w) => `${w.startTime}–${w.endTime}`)
      .join("、");
    const allowed = windowState ? windowState.allowsProbe : null;
    // The server clock is part of the statement, not decoration: the window is
    // evaluated in the host's timezone while these timestamps render in the
    // browser's, and an eight-hour gap makes "why did it probe at 02:44" a
    // question about clocks rather than about configuration.
    // The server's own wall clock, already formatted there. Reformatting it
    // through new Date() would render it in the browser's zone while the label
    // still said the server's, which reads as a timezone claim that is false.
    let clock = "";
    if (windowState && windowState.serverClock) {
      const zone = windowState.zone ? ` ${windowState.zone}` : "";
      clock = ` · 服务器 ${windowState.serverClock}${zone}`;
      // The window is evaluated on that clock, so a difference from the one the
      // operator reads the records in is the whole explanation for "why did it
      // probe outside my hours". Say the size of the gap rather than leaving it
      // to be worked out.
      const serverOffset = windowState.offsetMinutes;
      const browserOffset = -new Date().getTimezoneOffset();
      if (typeof serverOffset === "number" && serverOffset !== browserOffset) {
        const hours = Math.abs(serverOffset - browserOffset) / 60;
        clock += `（与浏览器相差 ${hours} 小时，窗口按服务器时间判定）`;
      }
    }
    parts.push(allowed === false
      ? `窗口 ${ranges}（当前禁止探测${clock}）`
      : `窗口 ${ranges}${clock}`);
  }
  const proxies = proxiesState.length;
  parts.push(proxies ? `${proxies} 个代理节点` : "无代理节点");
  node.textContent = parts.join(" · ");
}

function refreshAccountsSummary() {
  const node = $("accounts-summary");
  if (!node) return;
  if (!accountsState.length) {
    // This runs after a successful load, so "never synced" would be wrong: the
    // sync happened and CPA reported no Codex accounts.
    node.textContent = "CPA 中暂无 Codex 账号";
    return;
  }
  let bound = 0;
  for (const account of accountsState) bound += account.bindings || 0;
  node.textContent = `${accountsState.length} 个账号 · ${bound} 个 State 绑定`;
}

/* ------------------------------------------------------------------ gate */

function enterPanel(inheritedKey) {
  $("gate").hidden = true;
  $("panel").hidden = false;
  fillKeyField(inheritedKey || managementKey, inheritedKey ? "inherited" : "manual");
  setConn("已连接", "pill-ok");
}

function showGate(message) {
  const gate = $("gate");
  const panel = $("panel");
  const session = $("session");
  if (gate) gate.hidden = false;
  if (panel) panel.hidden = true;
  if (session) session.hidden = true;
  setConn("未连接", "pill-idle");
  const error = $("gate-error");
  if (error) {
    error.textContent = message || "";
    error.hidden = !message;
  }
}

async function connectWith(key, inheritedKey) {
  managementKey = key;
  await loadAll();
  enterPanel(inheritedKey);
}

// describeError renders an exception usefully in a UI string.
/*
 * The management key is shown, not hidden.
 *
 * An operator needs to answer "is a key configured, and is it the right one"
 * at a glance. A banner that conceals the value behind a toggle answers
 * neither, so the key sits in the config area as a password field: masked by
 * the browser, readable when someone chooses to look, and editable in place.
 * The plugin still never writes it anywhere -- readInheritedKey only reads.
 */
function fillKeyField(value, source) {
  const field = $("mgmt-key");
  if (field) field.value = value || "";
  const note = $("mgmt-key-source");
  if (!note) return;
  const length = (value || "").length;
  if (!length) {
    note.textContent = "未设置。从 CPA 管理面板打开本页可自动沿用，也可直接在此填入。";
    return;
  }
  note.textContent = source === "inherited"
    ? `已沿用 CPA 管理面板的会话密钥（长度 ${length}）。可直接修改后点「应用」。`
    : `当前使用手动输入的密钥（长度 ${length}）。`;
}

function describeError(err) {
  if (!err) return "未知错误";
  if (err.message) return err.message;
  return String(err);
}

on("session-switch", "click", () => {
  managementKey = "";
  $("key").value = "";
  showGate();
  $("key").focus();
});

on("gate-form", "submit", async (event) => {
  event.preventDefault();
  const key = $("key").value.trim();
  if (!key) return;
  try {
    await connectWith(key);
  } catch (err) {
    managementKey = "";
    showGate(err.message);
  }
});

// Bootstrap: try to inherit the session the management center already holds,
// and only ask when that is unavailable or no longer valid.
(async function bootstrap() {
  console.info("[turn-state] panel script loaded");
  restoreConfigOpen();
  const inherited = readInheritedKey();
  console.info("[turn-state] key inheritance:", inherited.reason,
    "| host:", location.host,
    "| has storage entry:", (() => {
      try { return !!localStorage.getItem(CPA_SESSION_KEY); } catch { return "unavailable"; }
    })());
  if (inherited.key) {
    try {
      await connectWith(inherited.key, inherited.key);
      return;
    } catch (err) {
      // The plugin was updated while this page was open, so the routes this
      // copy of the panel knows about no longer exist. Reload once to pick up
      // the panel that ships with the running plugin -- this is the case that
      // actually happened in production, where the symptom was a login form
      // and a 404 that named nothing the operator could act on.
      if (err && err.stalePage && !staleReloadAttempted()) {
        markStaleReloadAttempted();
        window.location.reload();
        return;
      }
      // Report *why*, not just that it failed. "Rejected key" and "the panel
      // threw while rendering" look identical otherwise, and they need
      // completely different responses.
      console.error("[turn-state] using the inherited key failed:", err);
      managementKey = "";
      showGate("沿用管理面板会话失败：" + describeError(err) + "（可在此手动输入密钥）");
      return;
    }
  }
  if (inherited.reason !== "no-session") {
    const reason = $("gate-reason");
    if (reason) reason.textContent = inheritHint(inherited.reason);
  }
  showGate(inheritHint(inherited.reason));
})().catch((err) => {
  // Bootstrap itself failed. Say so instead of leaving the login form up, which
  // would send the operator looking for a key problem that does not exist.
  console.error("[turn-state] bootstrap failed:", err);
  fail("面板初始化失败：" + (err && err.message ? err.message : String(err)) +
       "\n请刷新页面；若持续出现，请把控制台的 [turn-state] 日志一并反馈。");
});

/* ------------------------------------------------------------- settings */

function fillSettingsForm(values) {
  $("s-global").checked = values.globalEnabled;
  $("s-probe").checked = values.globalProbeEnabled;
  $("s-reverse").checked = values.globalReverseBindEnabled;
  $("s-routing").checked = values.statePriorityEnabled;
  $("s-retention").value = values.probeHistoryRetentionHours;
  $("s-maxproxies").value = values.maxProxiesPerProbe;
  $("s-scan").value = values.scanIntervalSec;
  $("s-concurrency").value = values.probeConcurrency;
  $("s-ttl").value = values.stateTtlMin;
  $("s-threshold").value = values.refreshThresholdPct;
  $("s-length").value = values.targetStateLength;
  $("s-maxprobe").value = values.maxProbeDurationSec;
  for (const radio of document.querySelectorAll('input[name="strategy"]')) {
    radio.checked = radio.value === values.routingStrategy;
  }
  // The sub-switches are inert while the master switch is off; disable them so
  // the UI matches what the runtime actually does.
  $("s-probe").disabled = !values.globalEnabled;
  $("s-reverse").disabled = !values.globalEnabled;
  $("s-routing").disabled = !values.globalEnabled;
  refreshConfigSummary();
}

on("s-routing", "change", () => {});

on("mgmt-key-apply", "click", async () => {
  const candidate = ($("mgmt-key").value || "").trim();
  if (!candidate) {
    toast("请先填入管理密钥", true);
    return;
  }
  const previous = managementKey;
  try {
    await connectWith(candidate);
    fillKeyField(candidate, "manual");
    toast("密钥已应用");
  } catch (err) {
    // Restore the working key so a typo does not lock the operator out of a
    // panel that was fine a moment ago.
    managementKey = previous;
    fillKeyField(previous, "manual");
    toast("密钥无效：" + describeError(err), true);
  }
});

on("restore-defaults", "click", () => {
  if (!settingsState) return;
  fillSettingsForm(DEFAULTS);
  toast("已填入默认值，点「保存配置」生效");
});

on("s-global", "change", () => {
  const on = $("s-global").checked;
  $("s-probe").disabled = !on;
  $("s-reverse").disabled = !on;
});

on("save-settings", "click", async () => {
  const strategy = document.querySelector('input[name="strategy"]:checked');
  const patch = {
    globalEnabled: $("s-global").checked,
    globalProbeEnabled: $("s-probe").checked,
    globalReverseBindEnabled: $("s-reverse").checked,
    statePriorityEnabled: $("s-routing").checked,
    scanIntervalSec: Number($("s-scan").value),
    probeConcurrency: Number($("s-concurrency").value),
    stateTtlMin: Number($("s-ttl").value),
    refreshThresholdPct: Number($("s-threshold").value),
    targetStateLength: Number($("s-length").value),
    maxProbeDurationSec: Number($("s-maxprobe").value),
    probeHistoryRetentionHours: Number($("s-retention").value),
    maxProxiesPerProbe: Number($("s-maxproxies").value),
    routingStrategy: strategy ? strategy.value : undefined,
  };
  try {
    const values = await api("PUT", "/settings", patch);
    settingsState = values;
    fillSettingsForm(values);
    toast("设置已保存");
  } catch (err) {
    toast(err.message, true);
  }
});

/* -------------------------------------------------------- time windows */

const DAY_NAMES = ["日", "一", "二", "三", "四", "五", "六"];

function renderWindows() {
  const tbody = $("windows-table").querySelector("tbody");
  clear(tbody);
  if (!windowsState.length) {
    tbody.append(el("tr", null, el("td", { colspan: "6" },
      el("span", { class: "muted small", text: "未配置窗口 — 当前全天可探测" }))));
    return;
  }
  for (const win of windowsState) {
    // Checkboxes rather than a text field. The text field displayed the days as
    // CJK characters but parsed only digits, so saving a window without editing
    // it failed with "无效的星期值" on the panel's own output. A control that
    // cannot express a value it just rendered is the bug; seven boxes cannot
    // get out of step with what they show.
    tbody.append(el("tr", null, [
      el("td", null, el("input", { type: "text", value: win.label || "", "data-k": "label" })),
      el("td", null, el("div", {
        class: "days", "data-k": "days",
        title: "勾选生效的星期；全选表示每天",
      },
        daysToChecked(win.daysOfWeek).map((on, index) => el("label", {
          class: "day",
          title: index === 0 ? "周日" : "周" + DAY_NAMES[index],
        }, [
          el("input", { type: "checkbox", checked: on }),
          el("span", { text: DAY_NAMES[index] }),
        ])))),
      el("td", null, el("input", { type: "text", value: win.startTime, "data-k": "start" })),
      el("td", null, el("input", { type: "text", value: win.endTime, "data-k": "end" })),
      el("td", null, el("input", { type: "checkbox", checked: win.enabled, "data-k": "enabled" })),
      el("td", null, el("div", { class: "row-actions" }, [
        el("button", { class: "btn btn-sm", type: "button", text: "保存",
          onclick: (ev) => saveWindow(win.id, ev.target.closest("tr")) }),
        el("button", { class: "btn btn-sm btn-danger", type: "button", text: "删除",
          onclick: () => deleteWindow(win.id) }),
      ])),
    ]));
  }
}

// daysToChecked maps a stored day list onto the seven checkboxes.
//
// An empty list means "every day" -- that is how the server reads it, and it is
// what an operator gets before choosing anything -- so it has to render as all
// seven checked. Rendering it as none checked made the panel unable to show the
// state it had just saved: selecting all seven, saving, and watching the
// selection vanish was the visible result.
function daysToChecked(daysOfWeek) {
  const days = daysOfWeek || [];
  if (!days.length) return DAY_NAMES.map(() => true);
  return DAY_NAMES.map((_, index) => days.includes(index));
}

// checkedToDays is the inverse, and canonicalises back to the empty list when
// every day is selected so the stored form does not depend on how the operator
// arrived at it.
function checkedToDays(cell) {
  const boxes = [...cell.querySelectorAll("input[type=\"checkbox\"]")];
  const days = boxes.flatMap((box, index) => (box.checked ? [index] : []));
  return days.length === boxes.length ? [] : days;
}

async function saveWindow(id, row) {
  const read = (k) => row.querySelector(`[data-k="${k}"]`);
  const days = checkedToDays(read("days"));
  try {
    await api("PUT", `/time-windows?${qs({ id })}`, {
      label: read("label").value || id,
      daysOfWeek: days,
      startTime: read("start").value.trim(),
      endTime: read("end").value.trim(),
      enabled: read("enabled").checked,
      sortOrder: 0,
    });
    await loadWindows();
    toast("窗口已保存");
  } catch (err) {
    toast(err.message, true);
  }
}

async function deleteWindow(id) {
  try {
    await api("DELETE", `/time-windows?${qs({ id })}`);
    await loadWindows();
    toast("窗口已删除");
  } catch (err) {
    toast(err.message, true);
  }
}

on("add-window", "click", async () => {
  try {
    await api("POST", "/time-windows", {
      label: "工作时段",
      daysOfWeek: [1, 2, 3, 4, 5],
      startTime: "08:00",
      endTime: "20:00",
      enabled: true,
      sortOrder: 0,
    });
    await loadWindows();
  } catch (err) {
    toast(err.message, true);
  }
});

/* ------------------------------------------------------------- accounts */

function renderAccounts() {
  const host = $("accounts");
  if (!changed("accounts", [accountsState, [...expanded], accountFilter])) return;
  clear(host);

  if (!accountsState.length) {
    host.append(el("div", { class: "empty", text: "暂无账号 — 点击「同步账号」从 CPA 拉取" }));
    return;
  }

  const needle = accountFilter.trim().toLowerCase();
  const visible = needle
    ? accountsState.filter((a) =>
        (a.label || "").toLowerCase().includes(needle) ||
        (a.authIndex || "").toLowerCase().includes(needle) ||
        (a.plan && a.plan.type || "").toLowerCase().includes(needle))
    : accountsState;

  if (!visible.length) {
    host.append(el("div", { class: "empty", text: `没有匹配「${accountFilter}」的账号` }));
    return;
  }

  for (const account of visible) {
    const open = expanded.has(account.authIndex);
    // CPA's ready state is "active"; anything else is worth a second look.
    const statusClass = account.status === "active" && !account.disabled ? "pill-ok"
      : account.disabled ? "pill-idle" : "pill-warn";

    const body = el("div", { class: "account-body", dataset: { accountBody: account.authIndex } });
    body.hidden = !open;

    const head = el("div", {
      class: "account-head",
      onclick: () => toggleAccount(account.authIndex),
    }, [
      el("div", { class: "who" }, [
        el("strong", { text: account.label || account.authIndex }),
        planBadge(account.plan),
        // The shape the plugin will accept for this account. It varies by tier
        // -- a Team account's values are a Plus account's wrong length -- so
        // showing it is what lets an operator reading a probe log tell "this
        // value is the wrong shape" from "something else is wrong".
        account.expectedStateLength
          ? el("span", {
              class: "pill pill-idle",
              title: `该账号的 State 需为 ${account.expectedStateBlocks} 个密文块（${account.expectedStateLength} 字符）；由套餐推导，套餐未知时用配置的目标长度`,
              text: `${account.expectedStateLength} 字符`,
            })
          : null,
        quotaBadge(account.quota),
        el("span", { class: "pill " + statusClass, text: account.status || "unknown" }),
        account.blockedReason
          ? el("span", { class: "pill pill-warn", title: account.blockedReason, text: "已暂停探测" })
          : null,
      ]),
      el("div", { class: "meta" }, [
        el("span", { text: `${account.bindings || 0} 个绑定` }),
        el("span", { class: "mono", text: account.authIndex }),
        el("span", { text: open ? "▾" : "▸" }),
      ]),
    ]);

    const block = el("div", { class: "account" }, [head, body]);
    if (open) renderModels(account.authIndex, body, account.blockedReason);
    host.append(block);
  }
}

function toggleAccount(authIndex) {
  if (expanded.has(authIndex)) expanded.delete(authIndex);
  else expanded.add(authIndex);
  renderAccounts();
}

// loadModelCatalog reads the account model list the plugin maintains from the
// same manifest CPA syncs.
// probeNow runs one on-demand probe and reports what happened.
//
// The outcome is surfaced verbatim, including a non-target length: "it answered
// with 312" is a result, not a failure, and hiding it behind a generic error
// would make the button useless for the question people press it to answer.
async function probeNow(authIndex, model, button) {
  if (button) {
    button.disabled = true;
    button.textContent = "探测中…";
  }
  try {
    const r = await api("POST", `/accounts/models/probe-now?${qs({ authIndex, model })}`);
    const bits = [r.outcome];
    if (r.stateLength) bits.push(`长度 ${r.stateLength}`);
    if (r.proxyId) bits.push(proxyLabel(r.proxyId));
    if (r.latencyMs) bits.push(`${r.latencyMs}ms`);
    if (r.error) bits.push(r.error);
    toast(`${model}：${bits.join(" · ")}`, !r.succeeded && r.outcome !== "SUCCESS_NON_TARGET");
  } catch (err) {
    toast(err.message, true);
  } finally {
    if (button) {
      button.disabled = false;
      button.textContent = "立即探测";
    }
    invalidate("probes");
    refresh();
  }
}

async function loadModelCatalog() {
  try {
    const payload = await api("GET", "/models");
    modelCatalog = payload.models || [];
    modelCatalogNote = payload.fetchedAt
      ? `模型清单拉取于 ${fmtTime(payload.fetchedAt)}`
      : (payload.note || "");
  } catch {
    modelCatalog = [];
    modelCatalogNote = "";
  }
}

async function addModel(authIndex, container, model) {
  const name = (model || "").trim();
  if (!name) return;
  try {
    await api("PUT", `/accounts/models/probe?${qs({ authIndex, model: name })}`, { enabled: true });
    toast(`已添加 ${name} 并开启探测`);
    await loadAllModels();
    invalidate(`models:${authIndex}`);
    const account = accountsState.find((a) => a.authIndex === authIndex);
    renderModels(authIndex, container, account && account.blockedReason);
    await loadAccounts();
  } catch (err) {
    toast(err.message, true);
  }
}

// loadAllModels fetches every account's models in one request.
//
// Per-account fetching meant the refresh cost grew with the number of accounts
// the operator had open, and expanding one waited on a round trip for data the
// account list already implied. One request on the same 15s cycle replaces
// both, and the tables then render from the cache.
async function loadAllModels() {
  try {
    const payload = await api("GET", "/accounts/models");
    const byAccount = payload.accounts || {};
    modelCache.clear();
    for (const [authIndex, models] of Object.entries(byAccount)) {
      modelCache.set(authIndex, models || []);
    }
    modelsLoaded = true;
  } catch (err) {
    modelsError = err.message;
  }
}

// renderModels draws one account's table from the cache. It never fetches, so
// expanding an account is instant.
function renderModels(authIndex, container, blockedReason) {
  if (!container.hasChildNodes()) {
    container.append(el("div", { class: "empty", text: "加载中…" }));
  }
  if (!modelCache.has(authIndex)) {
    // Bulk load has not landed yet, or this account is unknown to the registry.
    if (modelsError && !container.querySelector(".empty")) {
      clear(container);
      container.append(el("div", { class: "empty", text: modelsError }));
    }
    return;
  }

  const models = modelCache.get(authIndex) || [];
  // Redraw when something moved, or when there is nothing here to keep.
  //
  // The second half is not redundant: renderAccounts rebuilds every account
  // block from scratch on each poll that changes the list, which hands this
  // function a brand-new empty body while the memo below still holds the
  // previous signature. Trusting the memo alone left the fresh body showing
  // "加载中…" forever, because the content had not changed and nothing ever
  // drew into it.
  //
  // changed() is called unconditionally so the memo tracks reality either way;
  // rebuilding identical rows on every poll is what makes the table flicker.
  const drawn = container.querySelector("table") !== null;
  const moved = changed(`models:${authIndex}`, [models, blockedReason]);
  if (drawn && !moved) return;
  clear(container);
  const rows = models.map((m) => {
    const probeToggle = el("input", { type: "checkbox", checked: m.probeEnabled });
    probeToggle.addEventListener("change", async () => {
      try {
        await api("PUT",
          `/accounts/models/probe?${qs({ authIndex, model: m.model })}`,
          { enabled: probeToggle.checked });
        toast(`${m.model} 探测已${probeToggle.checked ? "开启" : "关闭"}`);
      } catch (err) {
        probeToggle.checked = !probeToggle.checked;
        toast(err.message, true);
      }
    });

    const actions = el("div", { class: "row-actions" }, [
      el("button", { class: "btn btn-sm", type: "button", text: "立即探测",
        title: "用一个代理探测一次，无论结果如何都结束。手动探测不受时间窗口限制",
        onclick: (ev) => probeNow(authIndex, m.model, ev.target) }),
      el("button", { class: "btn btn-sm", type: "button", text: "删除绑定",
        onclick: () => deleteBinding(authIndex, m.model) }),
      el("button", { class: "btn btn-sm", type: "button", text: "历史",
        onclick: () => showHistory(authIndex, m.model) }),
    ]);

    const ttl = m.status === "missing" || m.status === "expired"
      ? "—"
      : fmtDuration(Math.floor((new Date(m.expiresAt) - Date.now()) / 1000));

    // A model that keeps answering with the wrong state length never becomes
    // usable, but is retried every few minutes by design. Say so rather than
    // letting the operator wonder why nothing ever binds.
    const futile = m.nonTargetStreak >= 3
      ? el("span", {
          class: "pill pill-warn",
          title: `连续 ${m.nonTargetStreak} 次返回非目标长度，该模型可能永远不会产出可用的 State`,
          text: `连续 ${m.nonTargetStreak} 次非目标长度`,
        })
      : null;

    return el("tr", null, [
      el("td", { class: "mono", text: m.model }),
      el("td", null, probeToggle),
      el("td", null, [
        statusPill(m.status),
        m.source ? el("span", { class: "muted small", text: " " + m.source }) : null,
      ]),
      el("td", { class: "num", text: ttl }),
      el("td", { class: "num mono", text: m.stateLength || "—" }),
      el("td", { class: "muted small", text: m.nextProbeAt ? fmtTime(m.nextProbeAt) : "—" }),
      el("td", null, futile),
      el("td", null, actions),
    ]);
  });

  container.append(table(
    [{ label: "模型" }, { label: "探测" }, { label: "State" }, { label: "剩余" },
     { label: "长度" }, { label: "下次探测" }, { label: "提示" }, { label: "" }],
    rows, "该账号的模型清单为空"));

  // The list is maintained by the plugin from the same manifest CPA syncs, so
  // there is nothing to add by hand -- only per-model probe toggles.
  if (blockedReason) {
    container.append(el("p", { class: "notice" }, [
      el("strong", { text: "该账号暂停探测。" }),
      el("span", { text: blockedReason }),
    ]));
  }

  container.append(el("p", { class: "muted small",
    text: "模型清单自动同步自 CPA 的模型表，不需要手动维护；" +
          "账号不支持某个模型时，探测会返回 MODEL_UNSUPPORTED。" }));
}

async function deleteBinding(authIndex, model) {
  if (!confirm(`删除 ${authIndex} / ${model} 的绑定 State？\n\n删除后该组合会在下次扫描时重新探测。`)) return;
  try {
    await api("DELETE", `/bindings?${qs({ authIndex, model })}`);
    toast("绑定已删除");
    await refresh();
  } catch (err) {
    toast(err.message, true);
  }
}

/* -------------------------------------------------------------- history */

let modalContext = null;

async function showHistory(authIndex, model) {
  modalContext = { kind: "history", authIndex, model };
  $("modal-title").textContent = `${authIndex} / ${model} — 绑定历史`;
  $("modal-clear").hidden = false;
  $("modal-reset-all").hidden = true;
  $("modal").hidden = false;

  const body = $("modal-body");
  clear(body);
  body.append(el("div", { class: "empty", text: "加载中…" }));

  try {
    const payload = await api("GET",
      `/bindings/history?${qs({ authIndex, model, limit: 100 })}`);
    const rows = (payload.history || []).map((h) => el("tr", null, [
      el("td", { class: "mono", text: fmtTime(h.createdAt) }),
      el("td", null, el("span", {
        class: "pill " + (h.action === "deleted" ? "pill-bad"
          : h.action === "replaced" ? "pill-warn" : "pill-ok"),
        text: h.action,
      })),
      el("td", null, el("span", { class: "pill pill-idle", text: h.source })),
      el("td", { class: "mono" }, [
        el("span", { text: h.statePrefix ? h.statePrefix + "…" : "—" }),
        h.stateLength ? copyButton(authIndex, model, h.id) : null,
      ]),
      el("td", { class: "num", text: h.stateLength || "—" }),
      el("td", { class: "mono", text: proxyLabel(h.proxyId) }),
    ]));
    clear(body);
    body.append(table(
      [{ label: "时间" }, { label: "操作" }, { label: "来源" },
       { label: "State 前缀" }, { label: "长度" }, { label: "代理" }],
      rows, "暂无历史记录"));
    if (rows.length) {
      body.append(el("p", { class: "muted small",
        text: "列表只显示 State 值前 8 位；点复制图标取回完整值到剪贴板，不会写进页面。" }));
    }
  } catch (err) {
    clear(body);
    body.append(el("div", { class: "empty", text: err.message }));
  }
}

on("modal-close", "click", () => { $("modal").hidden = true; });

// Escape closes the dialog, which is what anyone reaches for.
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !$("modal").hidden) {
    $("modal").hidden = true;
  }
});
on("modal", "click", (event) => {
  if (event.target === $("modal")) $("modal").hidden = true;
});
// showCooldowns is the detail modal for one proxy: every account that has a
// record against it, with a reset per row.
//
// A node no longer has one health value -- the same proxy works for one account
// and not for another -- so the per-account rows are the whole picture, and the
// reset has to be available per row rather than only globally.
async function showCooldowns(proxyId) {
  modalContext = { kind: "cooldowns", proxyId };
  $("modal-title").textContent = `${proxyLabel(proxyId)} — 各账号冷却状态`;
  $("modal-clear").hidden = true;
  $("modal-reset-all").hidden = false;
  $("modal").hidden = false;

  const body = $("modal-body");
  clear(body);
  body.append(el("div", { class: "empty", text: "加载中…" }));

  try {
    const payload = await api("GET", `/proxy-nodes/cooldowns?${qs({ nodeId: proxyId })}`);
    const rows = (payload.cooldowns || []).map((c) => {
      const until = c.cooldownUntil ? new Date(c.cooldownUntil) : null;
      const cooling = until && until > new Date();
      const button = el("button", {
        class: "btn btn-sm", type: "button", text: "重置",
        onclick: () => resetCooldown(c.authIndex, proxyId),
      });
      return el("tr", null, [
        el("td", { text: accountLabel(c.authIndex), title: c.authIndex }),
        el("td", null, el("span", {
          class: "pill " + (cooling ? "pill-warn" : "pill-ok"),
          text: cooling ? "冷却中" : "可用",
        })),
        el("td", { class: "muted small", text: cooling ? fmtDuration(Math.round((until - Date.now()) / 1000)) : "—" }),
        el("td", { class: "num", text: c.consecutiveFailures || 0 }),
        el("td", { class: "num", text: c.failureCount || 0 }),
        el("td", { class: "muted small", text: fmtAgo(c.lastFailure) }),
        el("td", null, el("div", { class: "row-actions" }, button)),
      ]);
    });
    clear(body);
    body.append(table(
      [{ label: "账号" }, { label: "状态" }, { label: "剩余" },
       { label: "连败" }, { label: "累计失败" }, { label: "最近失败" }, { label: "" }],
      rows, "该代理还没有任何失败记录"));
  } catch (err) {
    clear(body);
    body.append(el("div", { class: "empty", text: err.message }));
  }
}

async function resetCooldown(authIndex, proxyId) {
  try {
    await api("POST", `/proxy-nodes/reset?${qs({ nodeId: proxyId, authIndex })}`);
    toast("已重置该账号在此代理上的冷却");
    await showCooldowns(proxyId);
    await loadProxies();
  } catch (err) {
    toast(err.message, true);
  }
}

on("reset-cooldowns", "click", async () => {
  if (!confirm("清除所有账号在所有代理上的冷却与失败计数？")) return;
  try {
    await api("POST", "/proxy-nodes/reset-all");
    toast("已重置全部冷却");
    await loadProxies();
  } catch (err) {
    toast(err.message, true);
  }
});

on("modal-reset-all", "click", async () => {
  if (!confirm("清除所有账号在所有代理上的冷却与失败计数？")) return;
  try {
    await api("POST", "/proxy-nodes/reset-all");
    toast("已重置全部冷却");
    $("modal").hidden = true;
    await loadProxies();
  } catch (err) {
    toast(err.message, true);
  }
});

on("modal-clear", "click", async () => {
  if (!modalContext) return;
  if (!confirm("清除该组合的全部绑定历史？此操作不可撤销。")) return;
  const { authIndex, model } = modalContext;
  try {
    await api("DELETE", `/bindings/history?${qs({ authIndex, model })}`);
    toast("历史已清除");
    showHistory(authIndex, model);
  } catch (err) {
    toast(err.message, true);
  }
});

/* --------------------------------------------------------------- proxies */

function renderProxies() {
  const host = $("proxies");
  if (!changed("proxies", proxiesState)) return;
  clear(host);

  if (!proxiesState.length) {
    host.append(el("div", { class: "empty", text: "代理池为空 — 未配置代理时探测将无法进行" }));
    return;
  }

  const rows = proxiesState.map((node, index) => {
    const statusCls = node.status === "healthy" ? "pill-ok"
      : node.status === "cooldown" ? "pill-warn" : "pill-idle";
    const set = (k, v) => {
      proxiesState[index][k] = v;
      invalidate("proxies");
    };

    return el("tr", null, [
      el("td", null, el("input", {
        type: "text", value: node.url, placeholder: "http://host:port",
        oninput: (e) => set("url", e.target.value.trim()),
        // Persist on blur rather than per keystroke: a save on every character
        // would rewrite the pool mid-word.
        onblur: () => saveProxies(),
      })),
      el("td", null, el("input", {
        type: "checkbox", checked: node.enabled,
        onchange: (e) => { set("enabled", e.target.checked); saveProxies(); },
      })),
      el("td", null, el("span", { class: "pill " + statusCls, text: node.status })),
      el("td", { class: "num", text: node.lastLatencyMs ? node.lastLatencyMs + "ms" : "—" }),
      el("td", { class: "num", text: `${node.successCount || 0} / ${node.failureCount || 0}` }),
      // Availability is per account, so this column reports how many accounts
      // currently have the node benched rather than a single verdict.
      el("td", null, node.coolingForAccounts
        ? el("span", { class: "pill pill-warn", text: `${node.coolingForAccounts} 个账号冷却中` })
        : el("span", { class: "muted small", text: "全部可用" })),
      el("td", { class: "muted small", text: fmtAgo(node.lastUsedAt) }),
      el("td", null, el("div", { class: "row-actions" }, [
        el("button", { class: "btn btn-sm", type: "button", text: "详情",
          title: "查看该代理在每个账号上的冷却状态",
          onclick: () => showCooldowns(node.id) }),
        el("button", { class: "btn btn-sm btn-danger", type: "button", text: "移除",
          onclick: () => {
            proxiesState.splice(index, 1);
            invalidate("proxies");
            renderProxies();
            saveProxies();
          } }),
      ])),
    ]);
  });

  host.append(table(
    [{ label: "地址" }, { label: "启用" }, { label: "状态" }, { label: "延迟" },
     { label: "成功/失败" }, { label: "冷却" }, { label: "最近使用" }, { label: "" }],
    rows, "代理池为空"));
}

// cooldownText explains a node's availability, and why it is unavailable.
//
// "cooling" on its own does not say for how long, and the reason is what tells
// an operator whether to wait or to fix something. Both are one line here
// because a cooldown that has to be investigated is a cooldown that gets
// worked around by deleting the node.
function cooldownText(node) {
  if (!node.cooldownUntil) return "—";
  const until = new Date(node.cooldownUntil);
  if (Number.isNaN(until.getTime()) || until <= new Date()) return "—";
  const secs = Math.round((until - Date.now()) / 1000);
  return `冷却 ${fmtDuration(secs)}（至 ${fmtTime(node.cooldownUntil)}）`;
}

on("add-proxy", "click", () => {
  const id = "proxy-" + Math.random().toString(36).slice(2, 8);
  proxiesState.push({
    id, url: "", enabled: true, successCount: 0, failureCount: 0,
    coolingForAccounts: 0, status: "healthy",
  });
  renderProxies();
});

// proxyID derives a stable, readable identifier from the address so the probe
// history names a node the operator recognises.
function proxyID(url, fallback) {
  try {
    const parsed = new URL(url);
    if (parsed.host) return parsed.host;
  } catch { /* not a usable URL yet */ }
  return fallback;
}

async function saveProxies() {
  const payload = proxiesState
    .filter((n) => n.url)
    .map((n) => ({
      id: proxyID(n.url, n.id),
      url: n.url,
      enabled: n.enabled,
      successCount: n.successCount || 0,
      failureCount: n.failureCount || 0,
      lastLatencyMs: n.lastLatencyMs || null,
      lastUsedAt: n.lastUsedAt || null,
      lastSuccess: n.lastSuccess || null,
      lastFailure: n.lastFailure || null,
    }));
  if (!payload.length) return;
  try {
    const resp = await api("PUT", "/proxy-nodes", { proxies: payload });
    proxiesState = resp.proxies || [];
    invalidate("proxies");
    renderProxies();
  } catch (err) {
    toast(err.message, true);
  }
}

on("save-proxies", "click", async () => {
  const payload = proxiesState
    .filter((n) => n.url)
    .map((n) => ({
      id: n.id, url: n.url, enabled: n.enabled,
      successCount: n.successCount || 0,
      failureCount: n.failureCount || 0,
      lastLatencyMs: n.lastLatencyMs || null,
      lastUsedAt: n.lastUsedAt || null,
      lastSuccess: n.lastSuccess || null,
      lastFailure: n.lastFailure || null,
    }));
  if (payload.length !== proxiesState.length) {
    toast("已忽略地址为空的节点", true);
  }
  try {
    const resp = await api("PUT", "/proxy-nodes", { proxies: payload });
    proxiesState = resp.proxies || [];
    renderProxies();
    toast("代理池已保存");
  } catch (err) {
    toast(err.message, true);
  }
});

/* ---------------------------------------------------------- probe history */

/*
 * copyButton copies a history entry's full state value.
 *
 * The value is fetched on demand and handed straight to the clipboard; it never
 * enters the DOM, so it is not sitting in the page for anything else to read.
 * The list view shows only a prefix for the same reason.
 */
function copyButton(authIndex, model, id) {
  const button = el("button", {
    class: "btn btn-sm btn-icon", type: "button", title: "复制完整 State 值",
    "aria-label": "复制完整 State 值",
  });
  button.innerHTML = COPY_ICON;

  button.addEventListener("click", async (event) => {
    event.stopPropagation();
    try {
      const payload = await api("GET",
        `/bindings/history?${qs({ authIndex, model, limit: 200, expand: 1 })}`);
      const entry = (payload.history || []).find((h) => h.id === id);
      if (!entry || !entry.stateValue) {
        toast("没有取到该记录的完整值", true);
        return;
      }
      await navigator.clipboard.writeText(entry.stateValue);
      toast(`已复制完整 State 值（${entry.stateValue.length} 字符）`);
    } catch (err) {
      toast("复制失败：" + describeError(err), true);
    }
  });
  return button;
}

const COPY_ICON =
  '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="none" ' +
  'stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">' +
  '<rect x="5.5" y="5.5" width="8" height="9" rx="1.5"/>' +
  '<path d="M10.5 3.5v-.5a1.5 1.5 0 0 0-1.5-1.5H4a1.5 1.5 0 0 0-1.5 1.5V10"/>' +
  '</svg>';

// accountLabel resolves a persistence key to something a person recognises.
// The auth index is an opaque hash; the email is what the operator thinks in.
function accountLabel(authIndex) {
  const account = accountsState.find((a) => a.authIndex === authIndex);
  return (account && (account.label || account.email)) || authIndex;
}

// proxyLabel resolves a probe record's proxy id to the node's address, which is
// what the operator configured. The id itself is internal.
function proxyLabel(id) {
  if (!id) return "—";
  const node = proxiesState.find((n) => n.id === id);
  return node ? node.url : id;
}

function renderProbes(probes) {
  const host = $("probes");
  if (!changed("probes", [probes, probeTotal, probeOffset, probeAccountFilter])) return;
  clear(host);

  const rows = (probes || []).map((p) => el("tr", null, [
    el("td", { class: "mono", text: fmtClock(p.probedAt), title: fmtTime(p.probedAt) }),
    el("td", { text: accountLabel(p.authIndex), title: accountLabel(p.authIndex) }),
    el("td", { class: "mono", text: p.model, title: p.model }),
    el("td", { class: "mono", text: proxyLabel(p.proxyId), title: proxyLabel(p.proxyId) }),
    el("td", null, el("span", {
      class: "pill " + (OUTCOME_PILL[p.result] || "pill-idle"),
      text: p.result,
    })),
    el("td", { class: "num", text: p.stateLength || "—" }),
    el("td", { class: "num", text: p.latencyMs != null ? p.latencyMs + "ms" : "—" }),
  ]));

  host.append(table(
    [{ label: "时间" }, { label: "账号" }, { label: "模型" }, { label: "代理" },
     { label: "结果" }, { label: "长度" }, { label: "耗时" }],
    rows, "该筛选下暂无探测记录"));

  // Paging, so "最近探测记录" is not limited to whatever fits on one screen.
  const pageSize = Number($("probe-limit").value) || 50;
  const shown = (probes || []).length;
  const from = shown ? probeOffset + 1 : 0;
  const to = probeOffset + shown;
  host.append(el("div", { class: "pager" }, [
    el("span", { class: "muted small", text: `第 ${from}–${to} 条，共 ${probeTotal} 条` }),
    el("span", { class: "actions" }, [
      el("button", {
        class: "btn btn-sm", type: "button", text: "上一页",
        disabled: probeOffset === 0,
        onclick: () => { probeOffset = Math.max(0, probeOffset - pageSize); invalidate("probes"); loadProbes(); },
      }),
      el("button", {
        class: "btn btn-sm", type: "button", text: "下一页",
        disabled: probeOffset + shown >= probeTotal,
        onclick: () => { probeOffset += pageSize; invalidate("probes"); loadProbes(); },
      }),
    ]),
  ]));
}

on("refresh-probes", "click", () => { probeOffset = 0; invalidate("probes"); loadProbes(); });

on("probe-account-filter", "change", () => {
  probeAccountFilter = $("probe-account-filter").value;
  probeOffset = 0;
  invalidate("probes");
  loadProbes();
});

on("account-filter", "input", () => {
  accountFilter = $("account-filter").value;
  invalidate("accounts");
  renderAccounts();
});
on("probe-limit", "change", () => loadProbes());

/* ----------------------------------------------------------------- load */

async function loadSettings() {
  settingsState = await api("GET", "/settings");
  fillSettingsForm(settingsState);
}

async function loadWindows() {
  const payload = await api("GET", "/time-windows");
  windowsState = payload.windows || [];
  renderWindows();
  refreshConfigSummary();
}

async function loadAccounts() {
  const payload = await api("GET", "/accounts");
  accountsState = payload.accounts || [];
  renderAccounts();
  refreshAccountsSummary();
}

async function loadProxies() {
  const payload = await api("GET", "/proxy-nodes");
  proxiesState = payload.proxies || [];
  renderProxies();
  refreshConfigSummary();
}

async function loadProbes() {
  const limit = Number($("probe-limit").value) || 50;
  const query = qs({
    limit,
    offset: probeOffset,
    authIndex: probeAccountFilter || undefined,
  });
  const payload = await api("GET", `/probe-history?${query}`);
  probeTotal = payload.total != null ? payload.total : (payload.probes || []).length;
  renderProbes(payload.probes);
  renderProbeFilter();
}

// renderProbeFilter offers the accounts that actually appear in the history,
// rather than every account, so the list stays short on a large pool.
function renderProbeFilter() {
  const select = $("probe-account-filter");
  if (!select) return;
  const known = accountsState.map((a) => a.authIndex);
  const wanted = [...new Set([probeAccountFilter, ...known].filter(Boolean))].sort();

  if (!changed("probe-filter", wanted)) return;
  const current = probeAccountFilter;
  clear(select);
  select.append(el("option", { value: "", text: "全部账号" }));
  for (const authIndex of wanted) {
    select.append(el("option", { value: authIndex, text: accountLabel(authIndex) }));
  }
  select.value = current;
}

async function loadStatus() {
  const status = await api("GET", "/status");
  windowState = status.window || null;
  $("version").textContent = status.version ? `v${status.version}` : "";

  // The pipeline counters are the only evidence that injection and capture are
  // happening at all: both rewrite headers and leave nothing else behind.
  const p = status.pipeline || {};
  const node = $("pipeline");
  if (!node) return;
  const parts = [];
  if (p.requestsSeen) parts.push(`请求 ${p.requestsSeen}`);
  if (p.injected) parts.push(`注入 ${p.injected}`);
  if (p.captured || p.capturedReused) {
    parts.push(`捕获 ${p.captured || 0}${p.capturedReused ? `+${p.capturedReused}续期` : ""}`);
  }
  if (p.invalidated) parts.push(`自愈 ${p.invalidated}`);

  // Whether the scan loop is alive. A stalled loop looks exactly like an idle
  // one from every other number on this page -- pairs overdue, no new rows --
  // so the age of the last scan is the one figure that separates them.
  const scan = status.scheduler || {};
  if (scan.lastScanAt) {
    const age = Math.max(0, Math.round((Date.now() - new Date(scan.lastScanAt).getTime()) / 1000));
    parts.push(`扫描 ${age}s 前`);
    const interval = (status.settings && status.settings.scanIntervalSec) || 60;
    // Three intervals of silence is well past a slow tick and into "stuck".
    if (age > interval * 3) {
      node.classList.add("pipeline-stale");
      node.title = `扫描循环已停止 ${age} 秒（间隔 ${interval} 秒）。探测不会进行；重启 CPA 可恢复。`;
    }
  }
  if (p.unresolvedAuth) parts.push(`账号未知 ${p.unresolvedAuth}`);
  node.textContent = parts.length ? parts.join(" · ") : "";
  node.title = parts.length
    ? "本进程累计：请求到达注入阶段 / 实际注入 / 从流量捕获 / 自愈失效 / 账号无法识别"
    : "本进程尚未处理任何请求；请求经 CPA 转发后这里会出现计数";
}

async function loadAll() {
  await loadStatus();
  await loadModelCatalog();
  await loadAccounts();   // the probe history and its filter name accounts
  await loadSettings();
  // A failing subsystem must not block the panel; each section reports its own
  // error so the operator can still reach the settings that would fix it.
  await Promise.allSettled([loadWindows(), loadAccounts(), loadProxies(), loadProbes()]);
}

async function refresh() {
  // loadStatus belongs here: without it the pipeline counters freeze at
  // whatever they were when the panel connected, which is the one moment they
  // are guaranteed to read zero.
  const results = await Promise.allSettled([
    loadStatus(), loadAccounts(), loadProxies(), loadProbes(),
    // Every account's models arrive in this one request, so the cost of the
    // cycle no longer grows with how many accounts are expanded.
    loadAllModels(),
  ]);

  // The page outlived the plugin it was served by: the management routes were
  // rebuilt underneath it and the ones it knows about are gone. Everything it
  // can do is already broken, so reload once to pick up the plugin's own panel.
  //
  // Once, not every cycle: a reload that does not fix it must not become a
  // refresh loop. sessionStorage is per-tab, so a second tab still gets its own
  // attempt.
  if (results.some((r) => r.status === "rejected" && r.reason && r.reason.stalePage)) {
    clearInterval(refreshTimer);
    if (!staleReloadAttempted()) {
      markStaleReloadAttempted();
      window.location.reload();
      return;
    }
    toast("插件已更新，但当前页面仍是旧版本。请手动刷新（Ctrl/Cmd+Shift+R 绕过缓存）。", true);
    return;
  }

  // Redraw from the cache that just landed. Expanded model tables were once the
  // one thing left out, so a binding that turned fresh stayed stale on screen
  // until a manual reload.
  for (const authIndex of expanded) {
    const body = document.querySelector(`[data-account-body="${CSS.escape(authIndex)}"]`);
    if (!body) continue;
    const account = accountsState.find((a) => a.authIndex === authIndex);
    renderModels(authIndex, body, account && account.blockedReason);
  }
}

on("sync-accounts", "click", async () => {
  try {
    const resp = await api("POST", "/accounts/sync");
    await loadAccounts();
    toast(`已同步 ${resp.accounts} 个账号`);
  } catch (err) {
    toast(err.message, true);
  }
});

// Refresh the volatile sections periodically so probe activity is visible
// without a manual reload.
// staleReloadKey guards the single automatic reload described in refresh().
const staleReloadKey = "turn-state:stale-reload";

function staleReloadAttempted() {
  try {
    return sessionStorage.getItem(staleReloadKey) === "1";
  } catch {
    // Storage unavailable: skip the automatic reload rather than risk a loop
    // we cannot count.
    return true;
  }
}

function markStaleReloadAttempted() {
  try {
    sessionStorage.setItem(staleReloadKey, "1");
  } catch { /* the reload still happens; only the guard is lost */ }
}

const refreshTimer = setInterval(() => {
  if (managementKey && !$("panel").hidden) refresh();
}, 15000);
