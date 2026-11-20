package main

import (
	"crypto/sha256"
	"encoding/base64"
)

// renderStatusShell is the page served on the PUBLIC resource route. It contains
// no pool data: it asks for the management key and fetches the authenticated
// route itself, building the DOM with textContent (never innerHTML).
//
// #FUNC     Render a data-free page that manages the whole pool.
// #NOT      No external assets, no innerHTML, no inline event attributes.
// #EDGE-OUT the write buttons hide themselves when the host's auth-files route
//
//	is missing (older host): a button that 404s is a bug report, not a
//	feature.
//
// #WHY      resource routes are not authenticated by the host, so anything
//
//	pre-rendered here is readable by anyone who can reach the port.
func renderStatusShell() []byte {
	return []byte(panelHTML)
}

// panelCSP locks the page to itself: no origins, no plugins, the one inline
// script pinned by its exact sha256, and framing refused.
//
// #FUNC     The Content-Security-Policy header value for the public page.
// #WHY      the script hash means the page's JS only runs as shipped: injected
//
//	markup fails closed instead of executing.
func panelCSP() string {
	sum := sha256.Sum256([]byte(panelScript))
	return "default-src 'none'; script-src 'sha256-" +
		base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'"
}

const panelScript = `
const STATUS = "/v0/management/plugins/go-adapter/status";
const REFRESH = "/v0/management/plugins/go-adapter/refresh";
const PAUSE = "/v0/management/plugins/go-adapter/pause";
const AUTHFILES = "/v0/management/auth-files";
let writeOK = null;
let pool = null;

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};
const out = (msg) => {
  const o = document.getElementById("out");
  o.style.display = "block"; o.textContent = msg;
};
const banner = (msg, ok) => {
  const b = document.getElementById("banner");
  if (!msg) { b.className = "bar"; b.textContent = ""; return; }
  b.className = "bar" + (ok ? " ok" : "");
  b.textContent = msg;
};
const key = () => document.getElementById("key").value.trim();
const auth = () => ({ headers: { Authorization: "Bearer " + key() } });

async function jfetch(url, opts) {
  const r = await fetch(url, opts);
  const text = await r.text();
  if (!r.ok) throw new Error(r.status + " " + text.slice(0, 300));
  try { return JSON.parse(text); } catch { return text; }
}

// -- session handling --------------------------------------------------------
// The key lives in sessionStorage when the user opts in ("remember for this
// tab"). Same origin as the whole management UI, so this is exactly as
// sensitive as the panel the host itself ships.
function rememberKey(k) {
  try {
    if (document.getElementById("remember").checked && k) sessionStorage.setItem("ga-mgmt", k);
    else sessionStorage.removeItem("ga-mgmt");
  } catch (e) { /* private mode: degrade to typing it each time */ }
}
function recallKey() {
  try {
    const v = sessionStorage.getItem("ga-mgmt");
    if (v) { document.getElementById("key").value = v; document.getElementById("remember").checked = true; }
  } catch (e) {}
}

// -- capability probe --------------------------------------------------------
// One GET against the host's auth-files route with the real key. 200/401/403
// all mean "the route exists". 404 means the buttons must hide.
async function probeWrite() {
  if (writeOK !== null) return writeOK;
  try {
    const r = await fetch(AUTHFILES, auth());
    writeOK = r.status !== 404;
  } catch (e) { writeOK = false; }
  return writeOK;
}

// -- self-check --------------------------------------------------------------
function selfCheck(s) {
  const lines = [];
  const loaded = s && s.version;
  lines.push(["plugin loaded", loaded]);
  lines.push(["accounts: " + (s.accounts ? s.accounts.length : 0), true]);
  lines.push(["catalogue: " + (s.catalogue ? s.catalogue.source : "?"), true]);
  if (s.built_for) lines.push(["built for host " + s.built_for, true]);
  const box = el("div", "selfcheck");
  for (const [text, good] of lines) {
    const d = el("div", good ? null : "miss", (good ? "✓ " : "✗ ") + text);
    box.appendChild(d);
  }
  return box;
}

// -- rendering ---------------------------------------------------------------
function badgeFor(a) {
  switch (a.badge) {
    case "VALID": return ["b-valid", "VALID"];
    case "QUARANTINED": return ["b-quar", "QUARANTINED"];
    case "UNKNOWN": return ["b-unk", "UNKNOWN"];
    case "KEY UNRESOLVABLE": return ["b-off", "NO KEY"];
    default: return ["b-off", a.badge || "OFF"];
  }
}

function fmtBreakdown(a) {
  if (a.paused_for) return "paused: " + a.paused_for + " left";
  if (a.breakdown && a.breakdown.quarantine) return "quarantined: " + a.breakdown.quarantine;
  if (a.veto) return "veto: " + a.veto + " (free models still route)";
  if (!a.breakdown) return "";
  const b = a.breakdown;
  const parts = [];
  if (b.neutral) parts.push("no data (neutral 450)");
  else parts.push(b.rolling + " rolling + " + b.density + " density");
  if (a.windows) {
    const bits = [];
    if (a.resets_in) bits.push("resets in " + a.resets_in);
    if (a.burn_per_hour != null && a.burn_per_hour > 0) bits.push((a.burn_per_hour * 100).toFixed(1) + "%/h burn");
    if (a.empty_in) bits.push("empty in ~" + a.empty_in);
    if (bits.length) parts.push(bits.join(" · "));
  }
  return parts.join(" · ");
}

function accountCard(a, canWrite) {
  const card = el("div", "card");
  const row = el("div", "row");
  const [bc, bt] = badgeFor(a);
  if (a.paused_for) { row.appendChild(el("span", "badge b-pause", "PAUSED")); }
  row.appendChild(el("span", "badge " + bc, bt));
  row.appendChild(el("strong", null, a.id));
  if (a.label && a.label !== a.id) row.appendChild(el("span", "mono", a.label));
  const score = el("span", "score " + (a.score <= -10000 ? "neg" : "pos"), String(a.score));
  row.appendChild(score);
  if (a.windows) row.appendChild(el("span", "mono",
    "5h " + a.windows["5h_remaining_pct"] + "% · wk " + a.windows.weekly_remaining_pct +
    "% · mo " + a.windows.monthly_remaining_pct + "%"));
  card.appendChild(row);

  const bd = fmtBreakdown(a);
  if (bd) card.appendChild(el("div", "breakdown", bd));

  const act = el("div", "row");
  act.style.marginTop = ".45rem";
  const bCheck = el("button", null, "Check quota");
  bCheck.onclick = () => oneRefresh(a.id);
  act.appendChild(bCheck);
  if (a.paused_for) {
    const bUn = el("button", "ghost", "Unpause");
    bUn.onclick = () => doPause(a.id, 0);
    act.appendChild(bUn);
  } else {
    const bP15 = el("button", "ghost", "Pause 15m");
    bP15.onclick = () => doPause(a.id, 15);
    const bP60 = el("button", "ghost", "Pause 1h");
    bP60.onclick = () => doPause(a.id, 60);
    act.appendChild(bP15); act.appendChild(bP60);
  }
  if (canWrite && a.file_name) {
    const bDis = el("button", "warnb", a.disabled ? "Enable (host)" : "Disable (host)");
    bDis.onclick = () => hostDisable(a);
    const bDel = el("button", "warnb", "Delete");
    bDel.onclick = () => hostDelete(a);
    act.appendChild(bDis); act.appendChild(bDel);
  }
  card.appendChild(act);
  return card;
}

function renderPool(s, canWrite) {
  const box = document.getElementById("pool");
  box.replaceChildren();
  box.appendChild(selfCheck(s));
  if (!s.accounts || s.accounts.length === 0) {
    banner("0 keys in the pool. /v1/models is empty for that reason — it is not a wrong model name. Add a key below.", false);
    return;
  }
  banner("", false);
  const q = (document.getElementById("filter").value || "").toLowerCase();
  let list = s.accounts.filter(a =>
    !q || a.id.toLowerCase().includes(q) || (a.label || "").toLowerCase().includes(q));
  const sort = document.getElementById("sort").value;
  if (sort === "score") list.sort((x, y) => y.score - x.score);
  if (sort === "rolling") list.sort((x, y) =>
    ((y.windows || {})["5h_remaining_pct"] || 0) - ((x.windows || {})["5h_remaining_pct"] || 0));
  if (sort === "id") list.sort((x, y) => x.id < y.id ? -1 : 1);
  const head = el("div", "sub",
    "version " + s.version + " · sticky sessions: " + s.sticky_sessions +
    " · catalogue: " + s.catalogue.source + " (" + s.catalogue.models + " models)" +
    (s.built_for ? " · built for host " + s.built_for : ""));
  box.appendChild(head);
  for (const a of list) box.appendChild(accountCard(a, canWrite));
}

function renderPicks(s) {
  const box = document.getElementById("picks");
  box.replaceChildren();
  const card = el("div", "card");
  card.appendChild(el("strong", null, "Recent scheduling decisions"));
  if (!s.recent_picks || !s.recent_picks.length) {
    card.appendChild(el("div", "small", "No decisions yet this session."));
    box.appendChild(card); return;
  }
  const t = el("table");
  const hr = el("tr");
  for (const h of ["time", "session", "model", "outcome", "winner", "why", "scores"]) hr.appendChild(el("th", null, h));
  t.appendChild(hr);
  for (const p of s.recent_picks.slice().reverse()) {
    const r = el("tr");
    const scores = (p.scores || []).map(x => x.AuthID + "=" + x.Score).join("  ");
    for (const v of [p.time, p.session, p.model || "", p.decision + (p.foreign_pool ? " (mixed pool)" : ""),
      p.winner || "—", p.reason, scores]) r.appendChild(el("td", null, v));
    t.appendChild(r);
  }
  card.appendChild(t);
  box.appendChild(card);
}

function renderErrs(s) {
  const box = document.getElementById("errs");
  box.replaceChildren();
  const card = el("div", "card");
  card.appendChild(el("strong", null, "Recent failures"));
  if (!s.recent_errors || !s.recent_errors.length) {
    card.appendChild(el("div", "small", "No recorded failures this session."));
    box.appendChild(card); return;
  }
  const t = el("table");
  const hr = el("tr");
  for (const h of ["time", "auth", "kind", "detail"]) hr.appendChild(el("th", null, h));
  t.appendChild(hr);
  for (const e of s.recent_errors.slice().reverse()) {
    const r = el("tr");
    for (const v of [e.time, e.auth_id || "—", e.kind, e.detail]) r.appendChild(el("td", null, v));
    t.appendChild(r);
  }
  card.appendChild(t);
  box.appendChild(card);
}

// -- actions -----------------------------------------------------------------
async function load() {
  const k = key();
  if (!k) { out("management key required"); return; }
  rememberKey(k);
  out("loading…");
  try {
    const s = await jfetch(STATUS, auth());
    pool = s;
    const canWrite = await probeWrite();
    document.getElementById("addBtn").disabled = !canWrite;
    if (!canWrite) document.getElementById("addNote").textContent =
      "This host predates the auth-files write route; add keys by dropping a JSON file into the auth directory.";
    renderPool(s, canWrite);
    renderPicks(s);
    renderErrs(s);
    out("");
  } catch (e) { out("request failed: " + e.message); }
}

async function refreshAll() {
  const k = key();
  if (!k) { out("management key required"); return; }
  out("re-probing…");
  try {
    const r = await jfetch(REFRESH, { method: "POST", ...auth() });
    out(typeof r === "object" ? JSON.stringify(r) : String(r));
    setTimeout(load, 600);
  } catch (e) { out("refresh failed: " + e.message); }
}

async function oneRefresh(id) {
  out("checking " + id + "…");
  try {
    const r = await jfetch(REFRESH + "?auth_id=" + encodeURIComponent(id), { method: "POST", ...auth() });
    out(typeof r === "object" ? JSON.stringify(r) : String(r));
    setTimeout(load, 400);
  } catch (e) { out("check failed: " + e.message); }
}

async function doPause(id, minutes) {
  out((minutes > 0 ? "pausing " : "unpausing ") + id + "…");
  try {
    const r = await jfetch(PAUSE, { method: "POST", headers: auth().headers,
      body: JSON.stringify({ auth_id: id, minutes: minutes }) });
    out(typeof r === "object" ? JSON.stringify(r) : String(r));
    setTimeout(load, 300);
  } catch (e) { out("pause failed: " + e.message); }
}

// The add path goes through the HOST's auth-files route: the host writes the
// file, runs our parser (which validates the key live), and registers the
// record. The plugin never writes files itself.
async function addKey() {
  const k = key();
  const apiKey = document.getElementById("addKey").value.trim();
  if (!k) { out("management key required"); return; }
  if (!apiKey) { out("paste the OpenCode Go key first"); return; }
  const label = document.getElementById("addLabel").value.trim();
  const rand = Math.random().toString(36).slice(2, 10);
  const name = "go-adapter-" + rand + ".json";
  out("adding and validating…");
  try {
    await jfetch(AUTHFILES + "?name=" + encodeURIComponent(name), {
      method: "POST", headers: { ...auth().headers, "Content-Type": "application/json" },
      body: JSON.stringify({ type: "go-adapter", api_key: apiKey, label: label || undefined }),
    });
    document.getElementById("addKey").value = "";
    out("added as " + name + "; checking…");
    setTimeout(load, 800);
  } catch (e) { out("add failed: " + e.message); }
}

// Enabling an auth the plugin quarantined is futile (the watcher re-applies our
// verdict), so the button routes quarantine cases to a quota check instead.
async function hostDisable(a) {
  if (a.key_status === "quarantined") {
    out(a.id + " is quarantined: enable flows through a quota check, not the disabled bit. Checking…");
    return oneRefresh(a.id);
  }
  const to = !a.disabled;
  if (to && !confirm("Disable " + a.id + " in the host?")) return;
  try {
    await jfetch("/v0/management/auth-files/status", {
      method: "PATCH", headers: { ...auth().headers, "Content-Type": "application/json" },
      body: JSON.stringify({ name: a.file_name, disabled: to }),
    });
    out(a.id + (to ? " disabled" : " enabled"));
    setTimeout(load, 800);
  } catch (e) { out("failed: " + e.message); }
}

async function hostDelete(a) {
  if (!confirm("Delete " + a.id + " (" + a.file_name + ")? This removes the auth file.")) return;
  try {
    await jfetch(AUTHFILES + "?name=" + encodeURIComponent(a.file_name), {
      method: "DELETE", ...auth() });
    out(a.id + " deleted");
    setTimeout(load, 800);
  } catch (e) { out("delete failed: " + e.message); }
}

// -- wiring ------------------------------------------------------------------
document.getElementById("loadBtn").onclick = load;
document.getElementById("refreshAllBtn").onclick = refreshAll;
document.getElementById("addBtn").onclick = addKey;
document.getElementById("filter").oninput = () => { if (pool) renderPool(pool, writeOK); };
document.getElementById("sort").onchange = () => { if (pool) renderPool(pool, writeOK); };
document.getElementById("key").addEventListener("keydown", e => { if (e.key === "Enter") load(); });
document.getElementById("remember").onchange = () => rememberKey(key());
document.querySelectorAll(".tabs button").forEach(b => b.onclick = () => {
  document.querySelectorAll(".tabs button").forEach(x => x.classList.remove("on"));
  b.classList.add("on");
  for (const id of ["tab-pool", "tab-decisions", "tab-errors"]) {
    document.getElementById(id).classList.toggle("hidden", id !== "tab-" + b.dataset.tab);
  }
});
recallKey();
if (key()) load();
`

const panelHTML = `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">
<title>Go Adapter — pool</title><style>
:root{--bg:#121212;--card:#1e1e1e;--field:#1a1a1a;--line:#333;--txt:#eee;--dim:#9a9a9a;--ok:#57d99a;--bad:#ff7b93;--warn:#e8c35a}
*{box-sizing:border-box}
body{font-family:system-ui,sans-serif;background:var(--bg);color:var(--txt);margin:1.5rem}
h1{font-size:1.3rem;margin:0 0 .2rem}.sub{color:var(--dim);font-size:.82rem}
.bar{background:#4a1420;color:#fff;border-radius:10px;padding:.9rem 1.1rem;margin:1rem 0;font-weight:500;display:none}
.bar.ok{background:#103c26;display:block}
.card{background:var(--card);border-radius:10px;padding:1rem 1.25rem;margin:.75rem 0;max-width:52rem}
.row{display:flex;align-items:center;gap:.75rem;flex-wrap:wrap}
.badge{font-size:.7rem;font-weight:700;letter-spacing:.08em;padding:.2rem .6rem;border-radius:999px}
.b-valid{background:#103c26;color:var(--ok)}.b-quar{background:#4a1420;color:var(--bad)}
.b-off{background:#2a2a2a;color:var(--dim)}.b-unk{background:#4a3a10;color:var(--warn)}
.b-pause{background:#1d2f4a;color:#7fb4ff}
.mono{font-family:ui-monospace,monospace;font-size:.78rem;color:var(--dim)}
.score{margin-left:auto;font-weight:700}.neg{color:var(--bad)}.pos{color:var(--ok)}
.breakdown{margin:.35rem 0 0;font-size:.78rem;color:var(--dim)}
button{background:#2c5a46;border:0;border-radius:8px;padding:.42rem .85rem;color:#d9f5e8;cursor:pointer;font-size:.85rem}
button.ghost{background:#2a2a2a;color:var(--dim)}
button.warnb{background:#4a1420;color:#ffb3c2}
button:disabled{opacity:.4;cursor:default}
input,select{background:var(--field);border:1px solid var(--line);border-radius:8px;padding:.5rem;color:var(--txt);font-size:.9rem}
input.key{min-width:18rem}
pre{background:var(--field);padding:.75rem;border-radius:8px;font-size:.78rem;overflow:auto;max-height:16rem}
table{border-collapse:collapse;width:100%;font-size:.8rem;margin-top:.4rem}
td,th{padding:.3rem .5rem;border-bottom:1px solid #2a2a2a;text-align:left;color:var(--dim)}
th{color:var(--txt);font-weight:600}
.tabs{display:flex;gap:.5rem;margin:1.2rem 0 .4rem;flex-wrap:wrap}
.tabs button{background:#2a2a2a;color:var(--dim)}
.tabs button.on{background:#2c5a46;color:#d9f5e8}
.hidden{display:none}
.small{font-size:.8rem;color:var(--dim)}
.selfcheck{font-size:.82rem;margin:.6rem 0 0;color:var(--dim)}
.selfcheck div{margin:.15rem 0}.selfcheck .miss{color:var(--bad)}
.addform{display:flex;gap:.6rem;flex-wrap:wrap;margin-top:.6rem}
</style></head><body>
<h1>Go Adapter — pool</h1>
<div class="sub">This page holds no data; it asks for the proxy's management key and reads the
authenticated route. Keys are never echoed back.</div>

<div class="row" style="margin:1rem 0">
  <input id="key" class="key" type="password" placeholder="management key (config: management.secret-key)" autocomplete="off">
  <label class="small"><input type="checkbox" id="remember"> remember for this tab</label>
  <button id="loadBtn">Load</button>
  <button id="refreshAllBtn" class="ghost">Refresh all keys</button>
</div>
<div id="banner" class="bar"></div>

<div class="tabs">
  <button data-tab="pool" class="on">Pool</button>
  <button data-tab="decisions">Decisions</button>
  <button data-tab="errors">Errors</button>
</div>

<div id="tab-pool">
  <div class="row">
    <input id="filter" placeholder="filter by id or label" style="min-width:14rem">
    <select id="sort"><option value="score">sort: score</option><option value="rolling">sort: 5h left</option><option value="id">sort: id</option></select>
  </div>
  <div id="pool"></div>
  <div class="card">
    <strong>Add a key</strong>
    <div class="addform">
      <input id="addLabel" placeholder="label (optional)">
      <input id="addKey" type="password" placeholder="OpenCode Go API key" autocomplete="off" style="min-width:16rem">
      <button id="addBtn">Add</button>
    </div>
    <div class="small" style="margin-top:.35rem" id="addNote">The key is validated on the spot; it never appears on this page again.</div>
  </div>
</div>
<div id="tab-decisions" class="hidden"><div id="picks"></div></div>
<div id="tab-errors" class="hidden"><div id="errs"></div></div>
<pre id="out" style="display:none"></pre>
<script>` + "\n" + `` + panelScript + `` + "\n" + `</script></body></html>`
