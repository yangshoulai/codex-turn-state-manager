/*
 * Codex Turn State Manager -- admin panel.
 *
 * The management key is held in a module-scoped variable for the lifetime of
 * the page only. It is deliberately never written to localStorage,
 * sessionStorage, a cookie, or the URL, per the plugin's security rules.
 */
"use strict";

const BASE = "/v0/management/plugins/codex-turn-state-manager";

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
const CPA_OBFUSCATION_PREFIX = "enc::v1::";
const CPA_OBFUSCATION_SALT = "cli-proxy-api-webui::secure-storage";

function xorBytes(bytes, keyBytes) {
  const out = new Uint8Array(bytes.length);
  for (let i = 0; i < bytes.length; i++) out[i] = bytes[i] ^ keyBytes[i % keyBytes.length];
  return out;
}

// decodePanelStorage accepts either an obfuscated blob or plain JSON.
function decodePanelStorage(raw) {
  if (!raw) return null;
  let text = raw;
  if (text.startsWith(CPA_OBFUSCATION_PREFIX)) {
    const binary = atob(text.slice(CPA_OBFUSCATION_PREFIX.length));
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    const keyBytes = new TextEncoder().encode(
      `${CPA_OBFUSCATION_SALT}|${location.host}|${navigator.userAgent}`
    );
    text = new TextDecoder().decode(xorBytes(bytes, keyBytes));
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
  } catch {
    return { key: "", reason: "undecodable" };
  }
  if (!state) return { key: "", reason: "undecodable" };

  const value = state.managementKey;
  if (typeof value !== "string" || !value.trim()) {
    // The session was found but the key was not persisted with it.
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

let managementKey = "";
let settingsState = null;
let proxiesState = [];
let windowsState = [];
let accountsState = [];
let expanded = new Set();
const modelCache = new Map();

/* ------------------------------------------------------------------ utils */

const $ = (id) => document.getElementById(id);

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
  let payload = null;
  if (text) {
    try { payload = JSON.parse(text); } catch { payload = { error: text }; }
  }
  if (!resp.ok) {
    const message = (payload && payload.error) || `HTTP ${resp.status}`;
    throw new Error(message);
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

/* ------------------------------------------------------------------ gate */

function enterPanel() {
  $("gate").hidden = true;
  $("panel").hidden = false;
  setConn("已连接", "pill-ok");
}

function showGate(message) {
  $("gate").hidden = false;
  $("panel").hidden = true;
  setConn("未连接", "pill-idle");
  if (message) {
    $("gate-error").textContent = message;
    $("gate-error").hidden = false;
  }
}

async function connectWith(key) {
  managementKey = key;
  await loadAll();
  enterPanel();
}

$("gate-form").addEventListener("submit", async (event) => {
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
  const inherited = readInheritedKey();
  if (inherited.key) {
    try {
      await connectWith(inherited.key);
      return;
    } catch {
      // The inherited key was rejected, or the API is unreachable. Fall through
      // to asking rather than leaving the operator with an error they cannot act on.
      managementKey = "";
      showGate("沿用的管理面板会话已失效，请重新输入密钥。");
      return;
    }
  }
  showGate(inheritHint(inherited.reason));
})();

/* ------------------------------------------------------------- settings */

function fillSettingsForm(values) {
  $("s-global").checked = values.globalEnabled;
  $("s-probe").checked = values.globalProbeEnabled;
  $("s-reverse").checked = values.globalReverseBindEnabled;
  $("s-scan").value = values.scanIntervalSec;
  $("s-concurrency").value = values.probeConcurrency;
  $("s-ttl").value = values.stateTtlMin;
  $("s-threshold").value = values.refreshThresholdPct;
  $("s-length").value = values.targetStateLength;
  $("s-maxprobe").value = values.maxProbeDurationSec;
  for (const radio of document.querySelectorAll('input[name="strategy"]')) {
    radio.checked = radio.value === values.routingStrategy;
  }
  // The two sub-switches are inert while the master switch is off; disable
  // them so the UI matches what the runtime actually does.
  $("s-probe").disabled = !values.globalEnabled;
  $("s-reverse").disabled = !values.globalEnabled;
}

$("s-global").addEventListener("change", () => {
  const on = $("s-global").checked;
  $("s-probe").disabled = !on;
  $("s-reverse").disabled = !on;
});

$("save-settings").addEventListener("click", async () => {
  const strategy = document.querySelector('input[name="strategy"]:checked');
  const patch = {
    globalEnabled: $("s-global").checked,
    globalProbeEnabled: $("s-probe").checked,
    globalReverseBindEnabled: $("s-reverse").checked,
    scanIntervalSec: Number($("s-scan").value),
    probeConcurrency: Number($("s-concurrency").value),
    stateTtlMin: Number($("s-ttl").value),
    refreshThresholdPct: Number($("s-threshold").value),
    targetStateLength: Number($("s-length").value),
    maxProbeDurationSec: Number($("s-maxprobe").value),
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
    const days = (!win.daysOfWeek || !win.daysOfWeek.length)
      ? "每天"
      : win.daysOfWeek.map((d) => DAY_NAMES[d]).join("");

    tbody.append(el("tr", null, [
      el("td", null, el("input", { type: "text", value: win.label || "", "data-k": "label" })),
      el("td", null, el("input", {
        type: "text", value: days, placeholder: "每天", "data-k": "days",
        title: "用 0-6 表示周日到周六，逗号分隔；留空表示每天",
      })),
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

function parseDays(text) {
  const trimmed = text.trim();
  if (!trimmed || trimmed === "每天") return [];
  return trimmed.split(/[,\s，]+/).filter(Boolean).map((part) => {
    const n = Number(part);
    if (!Number.isInteger(n) || n < 0 || n > 6) throw new Error(`无效星期值: ${part}`);
    return n;
  });
}

async function saveWindow(id, row) {
  const read = (k) => row.querySelector(`[data-k="${k}"]`);
  let days;
  try {
    days = parseDays(read("days").value);
  } catch (err) {
    toast(err.message, true);
    return;
  }
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

$("add-window").addEventListener("click", async () => {
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
  clear(host);

  if (!accountsState.length) {
    host.append(el("div", { class: "empty", text: "暂无账号 — 点击「同步账号」从 CPA 拉取" }));
    return;
  }

  for (const account of accountsState) {
    const open = expanded.has(account.authIndex);
    const statusClass = account.status === "available" ? "pill-ok"
      : account.disabled ? "pill-idle" : "pill-warn";

    const body = el("div", { class: "account-body" });
    body.hidden = !open;

    const head = el("div", {
      class: "account-head",
      onclick: () => toggleAccount(account.authIndex),
    }, [
      el("div", { class: "who" }, [
        el("strong", { text: account.label || account.authIndex }),
        el("span", { class: "pill " + statusClass, text: account.status || "unknown" }),
      ]),
      el("div", { class: "meta" }, [
        el("span", { text: `${account.bindings || 0} 个绑定` }),
        el("span", { class: "mono", text: account.authIndex }),
        el("span", { text: open ? "▾" : "▸" }),
      ]),
    ]);

    const block = el("div", { class: "account" }, [head, body]);
    if (open) loadModels(account.authIndex, body);
    host.append(block);
  }
}

function toggleAccount(authIndex) {
  if (expanded.has(authIndex)) expanded.delete(authIndex);
  else expanded.add(authIndex);
  renderAccounts();
}

async function loadModels(authIndex, container) {
  clear(container);
  container.append(el("div", { class: "empty", text: "加载中…" }));
  let payload;
  try {
    payload = await api("GET", `/accounts/models?${qs({ authIndex })}`);
  } catch (err) {
    clear(container);
    container.append(el("div", { class: "empty", text: err.message }));
    return;
  }
  modelCache.set(authIndex, payload.models || []);
  if (!expanded.has(authIndex)) return; // collapsed while loading

  clear(container);
  const rows = (payload.models || []).map((m) => {
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
      el("button", { class: "btn btn-sm", type: "button", text: "删除",
        onclick: () => deleteBinding(authIndex, m.model) }),
      el("button", { class: "btn btn-sm", type: "button", text: "历史",
        onclick: () => showHistory(authIndex, m.model) }),
    ]);

    const ttl = m.status === "missing" || m.status === "expired"
      ? "—"
      : fmtDuration(Math.floor((new Date(m.expiresAt) - Date.now()) / 1000));

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
      el("td", null, actions),
    ]);
  });

  container.append(table(
    [{ label: "模型" }, { label: "探测" }, { label: "State" }, { label: "剩余" },
     { label: "长度" }, { label: "下次探测" }, { label: "" }],
    rows, "该账号没有可用模型"));
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
  modalContext = { authIndex, model };
  $("modal-title").textContent = `${authIndex} / ${model} — 绑定历史`;
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
      el("td", { class: "mono", text: h.statePrefix ? h.statePrefix + "…" : "—" }),
      el("td", { class: "num", text: h.stateLength || "—" }),
      el("td", { class: "mono", text: h.proxyId || "—" }),
    ]));
    clear(body);
    body.append(table(
      [{ label: "时间" }, { label: "操作" }, { label: "来源" },
       { label: "State 前缀" }, { label: "长度" }, { label: "代理" }],
      rows, "暂无历史记录"));
    if (rows.length) {
      body.append(el("p", { class: "muted small",
        text: "仅显示 State 值前 8 位；完整值只保存在数据库中。" }));
    }
  } catch (err) {
    clear(body);
    body.append(el("div", { class: "empty", text: err.message }));
  }
}

$("modal-close").addEventListener("click", () => { $("modal").hidden = true; });
$("modal").addEventListener("click", (event) => {
  if (event.target === $("modal")) $("modal").hidden = true;
});
$("modal-clear").addEventListener("click", async () => {
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
  clear(host);

  if (!proxiesState.length) {
    host.append(el("div", { class: "empty", text: "代理池为空 — 未配置代理时探测将无法进行" }));
    return;
  }

  const rows = proxiesState.map((node, index) => {
    const statusCls = node.status === "healthy" ? "pill-ok"
      : node.status === "cooldown" ? "pill-warn" : "pill-idle";
    const set = (k, v) => { proxiesState[index][k] = v; };

    return el("tr", null, [
      el("td", null, el("input", {
        type: "text", value: node.url, placeholder: "http://host:port",
        oninput: (e) => set("url", e.target.value.trim()),
      })),
      el("td", null, el("input", {
        type: "checkbox", checked: node.enabled,
        onchange: (e) => set("enabled", e.target.checked),
      })),
      el("td", null, el("span", { class: "pill " + statusCls, text: node.status })),
      el("td", { class: "num", text: node.lastLatencyMs ? node.lastLatencyMs + "ms" : "—" }),
      el("td", { class: "num", text: `${node.successCount || 0} / ${node.failureCount || 0}` }),
      el("td", { class: "muted small", text: fmtAgo(node.lastUsedAt) }),
      el("td", null, el("div", { class: "row-actions" },
        el("button", { class: "btn btn-sm btn-danger", type: "button", text: "移除",
          onclick: () => { proxiesState.splice(index, 1); renderProxies(); } }))),
    ]);
  });

  host.append(table(
    [{ label: "地址" }, { label: "启用" }, { label: "状态" }, { label: "延迟" },
     { label: "成功/失败" }, { label: "最近使用" }, { label: "" }],
    rows, "代理池为空"));
}

$("add-proxy").addEventListener("click", () => {
  const id = "proxy-" + Math.random().toString(36).slice(2, 8);
  proxiesState.push({
    id, url: "", enabled: true, successCount: 0, failureCount: 0,
    consecutiveFailures: 0, status: "healthy",
  });
  renderProxies();
});

$("save-proxies").addEventListener("click", async () => {
  const payload = proxiesState
    .filter((n) => n.url)
    .map((n) => ({
      id: n.id, url: n.url, enabled: n.enabled,
      successCount: n.successCount || 0,
      failureCount: n.failureCount || 0,
      consecutiveFailures: n.consecutiveFailures || 0,
      cooldownUntil: n.cooldownUntil || null,
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

function renderProbes(probes) {
  const host = $("probes");
  clear(host);

  const rows = (probes || []).map((p) => el("tr", null, [
    el("td", { class: "mono", text: fmtTime(p.probedAt) }),
    el("td", { class: "mono", text: p.authIndex }),
    el("td", { class: "mono", text: p.model }),
    el("td", { class: "mono", text: p.proxyId || "—" }),
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
    rows, "暂无探测记录"));
}

$("refresh-probes").addEventListener("click", () => loadProbes());
$("probe-limit").addEventListener("change", () => loadProbes());

/* ----------------------------------------------------------------- load */

async function loadSettings() {
  settingsState = await api("GET", "/settings");
  fillSettingsForm(settingsState);
}

async function loadWindows() {
  const payload = await api("GET", "/time-windows");
  windowsState = payload.windows || [];
  renderWindows();
}

async function loadAccounts() {
  const payload = await api("GET", "/accounts");
  accountsState = payload.accounts || [];
  renderAccounts();
}

async function loadProxies() {
  const payload = await api("GET", "/proxy-nodes");
  proxiesState = payload.proxies || [];
  renderProxies();
}

async function loadProbes() {
  const limit = Number($("probe-limit").value) || 50;
  const payload = await api("GET", `/probe-history?limit=${limit}`);
  renderProbes(payload.probes);
}

async function loadStatus() {
  const status = await api("GET", "/status");
  $("version").textContent = status.version ? `v${status.version}` : "";
}

async function loadAll() {
  await loadStatus();
  await loadSettings();
  // A failing subsystem must not block the panel; each section reports its own
  // error so the operator can still reach the settings that would fix it.
  await Promise.allSettled([loadWindows(), loadAccounts(), loadProxies(), loadProbes()]);
}

async function refresh() {
  await Promise.allSettled([loadAccounts(), loadProxies(), loadProbes()]);
}

$("sync-accounts").addEventListener("click", async () => {
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
setInterval(() => {
  if (managementKey && !$("panel").hidden) refresh();
}, 15000);
