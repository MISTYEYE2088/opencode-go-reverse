package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Panel integration: one status document, one status page, one manual refresh
// route. The per-auth quota buttons come from the host already:
// GET/POST /v0/management/plugins/go-adapter/quota?auth_index=… lands in
// handleQuotaFetch, which is the manual recovery path.
//
// Trust boundary: /v0/management/* is authenticated by the host, /v0/resource/*
// is NOT (pluginapi: "Resource requests are not management-authenticated"). The
// resource route therefore serves a page with no data in it; the page asks for
// the management key and fetches the authenticated route itself.

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRegistration struct {
	Routes    []managementRoute `json:"Routes"`
	Resources []resourceRoute   `json:"Resources"`
}

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode,omitempty"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body"`
}

func handleManagementRegister() ([]byte, error) {
	return json.Marshal(managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: "/plugins/" + providerKey + "/status",
				Description: "Pool state: quota windows, scores, quarantine."},
			{Method: http.MethodPost, Path: "/plugins/" + providerKey + "/refresh",
				Description: "Manual re-probe of every key; recovered keys rejoin the pool."},
		},
		Resources: []resourceRoute{
			{Path: "/status", Menu: "Go Adapter",
				Description: "Pool state per key; the page asks for the management key."},
		},
	})
}

// isManagementPath tells the authenticated route from the public resource route.
//
// #FUNC     Report whether this request reached us on the management route.
// #EDGE-IN  any path that merely contains "/management/" → NOT management; the
//
//	public route must never serve data because of a substring
//
// #WHY      the first version matched the substring, so a future resource path
//
//	containing "/management/" would have re-opened the anonymous leak.
func isManagementPath(path string) bool {
	lower := strings.ToLower(path)
	// Only OUR registered management routes count. A substring test on
	// "/management/" (or even "/v0/management/") would let a future public
	// resource path that embeds such a segment take the authenticated branch.
	for _, suffix := range []string{"/plugins/" + providerKey + "/status", "/plugins/" + providerKey + "/refresh"} {
		if strings.HasSuffix(lower, suffix) && strings.Contains(lower, "/management") {
			return true
		}
	}
	return false
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	path := strings.ToLower(strings.TrimSpace(req.Path))
	management := isManagementPath(path)
	switch {
	case strings.HasSuffix(path, "/status") && strings.EqualFold(req.Method, http.MethodGet):
		if !management {
			// Public route: the page carries no data, only the code that asks
			// for a key and renders the authenticated answer.
			return json.Marshal(managementResponse{StatusCode: http.StatusOK,
				Headers: map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
				Body:    renderStatusShell()})
		}
		body, err := statusJSON()
		if err != nil {
			return jsonError(http.StatusInternalServerError, err)
		}
		return json.Marshal(managementResponse{StatusCode: http.StatusOK,
			Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body})
	case strings.HasSuffix(path, "/refresh") && strings.EqualFold(req.Method, http.MethodPost):
		if !management {
			return jsonError(http.StatusNotFound, errors.New("unknown route"))
		}
		body, err := refreshAllJSON()
		if err != nil {
			return jsonError(http.StatusInternalServerError, err)
		}
		return json.Marshal(managementResponse{StatusCode: http.StatusOK,
			Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body})
	default:
		return jsonError(http.StatusNotFound, errors.New("unknown route"))
	}
}

// jsonError renders an error body as JSON. %q produces Go escaping, which is not
// JSON for control bytes.
func jsonError(status int, err error) ([]byte, error) {
	body, errMarshal := json.Marshal(map[string]string{"error": err.Error()})
	if errMarshal != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	return json.Marshal(managementResponse{StatusCode: status,
		Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body})
}

type accountWindows struct {
	Rolling float64 `json:"5h_remaining_pct"`
	Weekly  float64 `json:"weekly_remaining_pct"`
	Monthly float64 `json:"monthly_remaining_pct"`
}

type accountStatus struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Disabled   bool   `json:"disabled"`
	Badge      string `json:"badge"`
	KeyStatus  string `json:"key_status"`
	Quarantine string `json:"quarantine,omitempty"`
	// Veto names the paid-quota veto currently applied (e.g.
	// monthly-over-threshold) so the score explains itself on the page.
	Veto string `json:"veto,omitempty"`
	// Score is what the scheduler actually gives this key: -20000 while
	// quarantined, so the page and the pick log never disagree.
	Score   int             `json:"score"`
	Windows *accountWindows `json:"windows,omitempty"`
}

// catalogueStatus answers "is the model list live or baked in?" without reading
// the source: source=upstream means fetched from the gateway, bootstrap means the
// cold-start fallback was used.
type catalogueStatus struct {
	Source    string `json:"source"`
	Models    int    `json:"models"`
	FetchedAt string `json:"fetched_at,omitempty"`
}

type poolStatus struct {
	Version       string          `json:"version"`
	GeneratedAt   string          `json:"generated_at"`
	StickySession int             `json:"sticky_sessions"`
	Catalogue     catalogueStatus `json:"catalogue"`
	Accounts      []accountStatus `json:"accounts"`
}

// statusJSON renders the pool: per key the host state, the quarantine flag, and —
// for keys that are not quarantined — the live windows plus the scheduler score.
func statusJSON() ([]byte, error) {
	var list authFileList
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &list); err != nil {
		return nil, fmt.Errorf("auth list unavailable: %w", err)
	}
	stickyMu.Lock()
	stickyCount := len(sticky)
	stickyMu.Unlock()

	accounts := make([]accountStatus, 0, len(list.Files))
	for _, f := range list.Files {
		if f.Provider != providerKey {
			continue
		}
		status := accountStatus{ID: f.ID, Label: f.Label, Disabled: f.Disabled}
		key, ok := keyForAuthID(f.ID)
		if f.Disabled {
			// Disabled is the host-level state (set at add time for keys that
			// never worked): recovery here is the panel's enable toggle.
			status.KeyStatus = "disabled"
			status.Badge = "DISABLED"
			accounts = append(accounts, status)
			continue
		}
		if !ok || key == "" {
			status.KeyStatus = "key-unresolvable"
			status.Badge = "KEY UNRESOLVABLE"
			accounts = append(accounts, status)
			continue
		}
		if reason, bad := quarantined(quotaCacheKey(key, currentConfig().BaseURL)); bad {
			status.KeyStatus = "quarantined"
			status.Badge = "QUARANTINED"
			status.Quarantine = reason
			status.Score = suspectPenalty
			accounts = append(accounts, status)
			continue
		}
		quota, err := quotaFor(f.ID, key)
		if err != nil {
			status.KeyStatus = "unknown"
			status.Badge = "UNKNOWN"
			accounts = append(accounts, status)
			continue
		}
		status.KeyStatus = "valid"
		status.Badge = "VALID"
		score, why := Score(quota, false, defaultScoreOptions(time.Now()))
		status.Score = score
		status.Veto = why
		status.Windows = &accountWindows{
			Rolling: quota.Rolling.Remaining(),
			Weekly:  quota.Weekly.Remaining(),
			Monthly: quota.Monthly.Remaining(),
		}
		accounts = append(accounts, status)
	}
	cat := currentCatalogue()
	catalogueInfo := catalogueStatus{Source: cat.Source, Models: len(cat.IDs)}
	if !cat.Fetched.IsZero() {
		catalogueInfo.FetchedAt = cat.Fetched.UTC().Format(time.RFC3339)
	}
	return json.MarshalIndent(poolStatus{
		Version: pluginVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		StickySession: stickyCount, Catalogue: catalogueInfo, Accounts: accounts,
	}, "", "  ")
}

type refreshOutcome struct {
	ID         string `json:"id"`
	Verdict    string `json:"verdict"`
	Quarantine string `json:"quarantine,omitempty"`
}

type refreshSummary struct {
	Checked          int              `json:"checked"`
	Recovered        []refreshOutcome `json:"recovered"`
	StillQuarantined []refreshOutcome `json:"still_quarantined"`
	Failures         []refreshOutcome `json:"failures"`
	GeneratedAt      string           `json:"generated_at"`
}

// refreshAllJSON is the manual button for the whole pool: every key gets the same
// forced probe the per-auth quota button does, and recovered keys rejoin.
func refreshAllJSON() ([]byte, error) {
	var list authFileList
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &list); err != nil {
		return nil, fmt.Errorf("auth list unavailable: %w", err)
	}
	summary := refreshSummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, f := range list.Files {
		if f.Provider != providerKey || f.Disabled {
			continue
		}
		key, ok := keyForAuthID(f.ID)
		if !ok || key == "" {
			summary.Failures = append(summary.Failures, refreshOutcome{ID: f.ID, Verdict: "key-unresolvable"})
			continue
		}
		summary.Checked++
		quota, err := fetchUsageWith(manualProbeClient(), key, currentConfig().BaseURL)
		cacheKey := quotaCacheKey(key, currentConfig().BaseURL)
		if err == nil {
			quarantineClear(cacheKey)
			quotaMu.Lock()
			quotaCache[cacheKey] = quotaEntry{quota: quota, err: nil, at: time.Now()}
			quotaMu.Unlock()
			summary.Recovered = append(summary.Recovered, refreshOutcome{ID: f.ID, Verdict: "valid"})
			continue
		}
		var bad invalidKeyError
		if errors.As(err, &bad) {
			quarantineSet(cacheKey, "invalid: "+bad.reason)
			summary.StillQuarantined = append(summary.StillQuarantined,
				refreshOutcome{ID: f.ID, Verdict: "invalid", Quarantine: bad.reason})
			continue
		}
		// Network or upstream trouble: the key keeps its standing.
		summary.Failures = append(summary.Failures, refreshOutcome{ID: f.ID, Verdict: "network"})
	}
	return json.MarshalIndent(summary, "", "  ")
}

// manualProbeClient is the forced-probe client shared by the manual paths.
func manualProbeClient() *http.Client { return &http.Client{Timeout: usageTimeout} }

// renderStatusShell is the page served on the PUBLIC resource route. It contains
// no pool data: it asks for the management key, calls the authenticated route,
// and builds the DOM with textContent (never innerHTML).
//
// #FUNC     Render a data-free page that fetches the authenticated status.
// #NOT      No external assets: the panel may run on an isolated network.
// #WHY      resource routes are not authenticated by the host, so anything
//
//	pre-rendered here is readable by anyone who can reach the port.
func renderStatusShell() []byte {
	return []byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Go Adapter — pool status</title><style>
body{font-family:system-ui,sans-serif;background:#121212;color:#eee;margin:2rem}
h1{font-size:1.4rem}.sub{color:#9a9a9a;font-size:.85rem}
.card{background:#1e1e1e;border-radius:10px;padding:1rem 1.25rem;margin:.75rem 0;max-width:44rem}
.row{display:flex;align-items:center;gap:.75rem;flex-wrap:wrap}
.badge{font-size:.72rem;font-weight:700;letter-spacing:.08em;padding:.2rem .6rem;border-radius:999px}
.b-valid{background:#103c26;color:#57d99a}.b-quarantined{background:#4a1420;color:#ff7b93}
.b-disabled,.b-key{background:#2a2a2a;color:#9a9a9a}.b-unknown{background:#4a3a10;color:#e8c35a}
.mono{font-family:ui-monospace,monospace;font-size:.8rem;color:#bbb}
.score{margin-left:auto;font-weight:700}.bad{color:#ff7b93}.good{color:#57d99a}
button{background:#2c5a46;border:0;border-radius:8px;padding:.5rem 1rem;color:#d9f5e8;cursor:pointer}
input{background:#1a1a1a;border:1px solid #333;border-radius:8px;padding:.5rem;color:#eee;min-width:20rem}
pre{background:#1a1a1a;padding:.75rem;border-radius:8px;font-size:.8rem;overflow:auto}
</style></head><body>
<h1>Go Adapter — pool status</h1>
<div class="sub">This page holds no data of its own. It asks for the proxy's management key
and reads <span class="mono">/v0/management/plugins/go-adapter/status</span>, which the host authenticates.</div>
<div class="row" style="margin:1rem 0">
  <input id="key" type="password" placeholder="management key (config: management.secret-key)">
  <button onclick="load()">Load</button>
  <button onclick="refreshAll()">Refresh all keys</button>
</div>
<div id="pool"></div><pre id="out" style="display:none"></pre>
<script>
const STATUS = "/v0/management/plugins/go-adapter/status";
const REFRESH = "/v0/management/plugins/go-adapter/refresh";
function key(){ const el = document.getElementById("key"); return el.value.trim(); }

function el(tag, cls, text){
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}
function out(msg){ const o = document.getElementById("out"); o.style.display = "block"; o.textContent = msg; }

async function load(){
  const k = key(); if (!k) { out("management key required"); return; }
  const pool = document.getElementById("pool");
  out("loading…");
  try {
    const r = await fetch(STATUS, {headers:{Authorization:"Bearer "+k}});
    if (!r.ok) { out(r.status + " " + (await r.text())); return; }
    const s = await r.json();
    pool.replaceChildren();
    const head = el("div","sub","version " + s.version + " · generated " + s.generated_at +
      " · sticky sessions: " + s.sticky_sessions + " · catalogue: " + s.catalogue.source +
      " (" + s.catalogue.models + " models)");
    pool.appendChild(head);
    if (!s.accounts || s.accounts.length === 0) {
      pool.appendChild(el("p", null, 'No go-adapter auth files. Add one via the panel, or drop {"type":"go-adapter","api_key":"…"} into the auth directory.'));
    }
    for (const a of (s.accounts || [])) {
      const card = el("div","card");
      const row = el("div","row");
      const css = a.badge === "VALID" ? "b-valid" : a.badge === "QUARANTINED" ? "b-quarantined"
                : a.badge === "UNKNOWN" ? "b-unknown" : "b-disabled";
      row.appendChild(el("span", "badge " + css, a.badge));
      row.appendChild(el("strong", null, a.id));
      if (a.label && a.label !== a.id) row.appendChild(el("span", "mono", a.label));
      const score = el("span", "score " + (a.score <= -10000 ? "bad" : "good"), String(a.score));
      row.appendChild(score);
      card.appendChild(row);
      if (a.quarantine) card.appendChild(el("div","mono","quarantined: " + a.quarantine));
      if (a.veto) card.appendChild(el("div","mono","paid quota veto: " + a.veto + " (free models still route)"));
      if (a.windows) card.appendChild(el("div","mono",
        "5h " + a.windows["5h_remaining_pct"] + "% · weekly " + a.windows.weekly_remaining_pct +
        "% · monthly " + a.windows.monthly_remaining_pct + "% left"));
      pool.appendChild(card);
    }
    out("loaded");
  } catch (e) { out("request failed: " + e); }
}

async function refreshAll(){
  const k = key(); if (!k) { out("management key required"); return; }
  out("re-probing…");
  try {
    const r = await fetch(REFRESH, {method:"POST", headers:{Authorization:"Bearer "+k}});
    out(r.status + " " + (await r.text()));
    if (r.ok) setTimeout(load, 800);
  } catch (e) { out("request failed: " + e); }
}
document.getElementById("key").addEventListener("keydown", e => { if (e.key === "Enter") load(); });
</script></body></html>`)
}
