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
				Description: "Pool state: quota windows, score breakdowns, recent decisions."},
			{Method: http.MethodPost, Path: "/plugins/" + providerKey + "/refresh",
				Description: "Manual re-probe (all keys, or one by auth_id); recovered keys rejoin."},
			{Method: http.MethodPost, Path: "/plugins/" + providerKey + "/pause",
				Description: "Park one key out of scheduling for N minutes (0 unpauses)."},
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
	for _, suffix := range []string{
		"/plugins/" + providerKey + "/status",
		"/plugins/" + providerKey + "/refresh",
		"/plugins/" + providerKey + "/pause",
	} {
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
				Headers: map[string][]string{
					"Content-Type":            {"text/html; charset=utf-8"},
					"Content-Security-Policy": {panelCSP()},
					"X-Content-Type-Options":  {"nosniff"},
					"Referrer-Policy":         {"no-referrer"},
					"Cache-Control":           {"no-store"},
					"X-Frame-Options":         {"DENY"},
				},
				Body: renderStatusShell()})
		}
		body, err := statusJSON()
		if err != nil {
			return jsonError(http.StatusInternalServerError, err)
		}
		return json.Marshal(managementResponse{StatusCode: http.StatusOK,
			Headers: map[string][]string{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
	case strings.HasSuffix(path, "/refresh") && strings.EqualFold(req.Method, http.MethodPost):
		if !management {
			return jsonError(http.StatusNotFound, errors.New("unknown route"))
		}
		authID := strings.TrimSpace(firstQueryValue(req.Query, "auth_id"))
		body, err := refreshJSON(authID)
		if err != nil {
			return jsonError(http.StatusInternalServerError, err)
		}
		return json.Marshal(managementResponse{StatusCode: http.StatusOK,
			Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body})
	case strings.HasSuffix(path, "/pause") && strings.EqualFold(req.Method, http.MethodPost):
		if !management {
			return jsonError(http.StatusNotFound, errors.New("unknown route"))
		}
		body, err := pauseJSON(req.Body)
		if err != nil {
			return jsonError(http.StatusBadRequest, err)
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
	Score int `json:"score"`
	// Breakdown explains the score: rolling share, density share, session
	// bonus, penalty. The page renders it next to the number.
	Breakdown *scoreBreakdown `json:"breakdown,omitempty"`
	Windows   *accountWindows `json:"windows,omitempty"`
	// ResetsIn is the human countdown to the next window reset ("3h12m").
	ResetsIn string `json:"resets_in,omitempty"`
	// BurnPerHour is the observed spend rate, for "how long until empty".
	BurnPerHour *float64 `json:"burn_per_hour,omitempty"`
	// EmptyIn estimates when the rolling window runs out at the current pace.
	EmptyIn string `json:"empty_in,omitempty"`
	// PausedFor is the remaining pause ("14m") when operator-parked.
	PausedFor string `json:"paused_for,omitempty"`
	// FileName is the host auth file backing this record, so the page's
	// delete/disable buttons address the host's own routes by name.
	FileName string `json:"file_name,omitempty"`
}

// scoreBreakdown mirrors the terms of Score so a number is never a black box.
type scoreBreakdown struct {
	Rolling    int    `json:"rolling"`
	Density    int    `json:"density"`
	Neutral    bool   `json:"neutral"`
	Veto       string `json:"veto,omitempty"`
	Quarantine string `json:"quarantine,omitempty"`
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
	// RecentPicks replays the last scheduling decisions.
	RecentPicks []pickRecord `json:"recent_picks,omitempty"`
	// RecentErrors carries the last upstream/probe failures, scrubbed.
	RecentErrors []errRecord `json:"recent_errors,omitempty"`
	// BuiltFor is the host version this plugin was compiled against; the page
	// shows it next to the running host's own version line when available.
	BuiltFor string `json:"built_for,omitempty"`
	// WriteOps is probed by the page itself (same-origin, with the management
	// key) — kept in the type so the JSON contract is documented in one place.
	WriteOps *writeOpsStatus `json:"write_ops,omitempty"`
}

// writeOpsStatus is the capability probe for the panel's write buttons.
type writeOpsStatus struct {
	AuthFilesRoute string `json:"auth_files_route"`
	Available      bool   `json:"available"`
}

// statusJSON renders the pool: per key the host state, the quarantine flag, and —
// for keys that are not quarantined — the live windows plus the scheduler score.
// #FUNC      Render the pool: per key the host state, quarantine, live windows
//
//	and the scheduler score.
//
// #WHY       every number here must match the pick log exactly — a status page
//
//	that disagrees with the scheduler is worse than none.
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
		status := accountStatus{ID: f.ID, Label: f.Label, Disabled: f.Disabled, FileName: f.Name}
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
			status.Breakdown = &scoreBreakdown{Quarantine: reason}
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
		opts := defaultScoreOptions(time.Now())
		score, why := Score(quota, false, opts)
		status.Score = score
		status.Veto = why
		status.Breakdown = breakdownFor(quota, opts, why)
		status.Windows = &accountWindows{
			Rolling: quota.Rolling.Remaining(),
			Weekly:  quota.Weekly.Remaining(),
			Monthly: quota.Monthly.Remaining(),
		}
		status.ResetsIn = countdown(quota)
		status.BurnPerHour, status.EmptyIn = burnEstimate(f.ID, quota)
		accounts = append(accounts, status)
	}
	cat := currentCatalogue()
	catalogueInfo := catalogueStatus{Source: cat.Source, Models: len(cat.IDs)}
	if !cat.Fetched.IsZero() {
		catalogueInfo.FetchedAt = cat.Fetched.UTC().Format(time.RFC3339)
	}
	paused := pausedSnapshot()
	for i := range accounts {
		if left, ok := paused[accounts[i].ID]; ok {
			accounts[i].PausedFor = humanDuration(left)
		}
	}
	return json.MarshalIndent(poolStatus{
		Version: pluginVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		StickySession: stickyCount, Catalogue: catalogueInfo, Accounts: accounts,
		RecentPicks: recentPicks(), RecentErrors: recentErrs(),
		BuiltFor: hostVersionBuiltFor(),
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

// firstQueryValue reads one value from a management query map.
func firstQueryValue(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// pauseRequest is the pause route body.
type pauseRequest struct {
	AuthID  string `json:"auth_id"`
	Minutes int    `json:"minutes"`
}

// pauseJSON parks one account, or reports the current pause state when minutes
// is absent.
//
// #FUNC      Answer the pause route: park, unpause, or query.
// #IN        {"auth_id":"go-adapter-1","minutes":15} — 0 unparks immediately.
// #OUT       the auth id, whether it is parked now, and the remaining minutes.
// #WHY       parking is scheduler-side state only: a host-side disable is sticky
//
//	for plugin auths (the watcher re-applies our verdict), so "temporary"
//	must not be written where the host keeps it forever.
func pauseJSON(body []byte) ([]byte, error) {
	var req pauseRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("invalid pause request: %w", err)
		}
	}
	if strings.TrimSpace(req.AuthID) == "" {
		return nil, errors.New("auth_id is required")
	}
	until := time.Time{}
	if req.Minutes != 0 || bodyContainsMinutes(body) {
		until = pauseAuth(req.AuthID, time.Duration(req.Minutes)*time.Minute)
	}
	left := pauseRemaining(req.AuthID)
	out := map[string]any{
		"auth_id":   req.AuthID,
		"paused":    left > 0,
		"remaining": 0,
	}
	if left > 0 {
		out["remaining"] = int(left.Minutes() + 1)
		out["until"] = until.UTC().Format(time.RFC3339)
	}
	return json.Marshal(out)
}

// bodyContainsMinutes reports whether the request set minutes at all, so an
// absent field means "query" rather than "unpause".
func bodyContainsMinutes(body []byte) bool {
	var probe map[string]any
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	_, ok := probe["minutes"]
	return ok
}

// refreshJSON is the manual probe for one key (auth_id) or the whole pool
// (empty auth_id). A single-key refresh is what "just added a key, is it good?"
// needs — waiting for the 15-minute host cycle is not an answer.
//
// #FUNC      Manual re-probe: one key or the whole pool.
// #OUT       the same verdict summary refreshAllJSON produces, scoped.
// #WHY       a key that was added a second ago has no scheduled refresh yet;
//
//	the panel must offer an immediate check.
func refreshJSON(authID string) ([]byte, error) {
	if authID == "" {
		return refreshAllJSON()
	}
	var list authFileList
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &list); err != nil {
		return nil, fmt.Errorf("auth list unavailable: %w", err)
	}
	for _, f := range list.Files {
		if f.Provider != providerKey || f.ID != authID {
			continue
		}
		if f.Disabled {
			return json.Marshal(refreshSummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339),
				Failures: []refreshOutcome{{ID: f.ID, Verdict: "disabled"}}})
		}
		key, ok := keyForAuthID(f.ID)
		if !ok || key == "" {
			return json.Marshal(refreshSummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339),
				Failures: []refreshOutcome{{ID: f.ID, Verdict: "key-unresolvable"}}})
		}
		summary := refreshSummary{Checked: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
		quota, err := fetchUsageWith(manualProbeClient(), key, currentConfig().BaseURL)
		cacheKey := quotaCacheKey(key, currentConfig().BaseURL)
		switch {
		case err == nil:
			quarantineClear(cacheKey)
			quotaMu.Lock()
			quotaCache[cacheKey] = quotaEntry{quota: quota, err: nil, at: time.Now()}
			quotaMu.Unlock()
			summary.Recovered = append(summary.Recovered, refreshOutcome{ID: f.ID, Verdict: "valid"})
		default:
			var bad invalidKeyError
			if errors.As(err, &bad) {
				quarantineSet(cacheKey, "invalid: "+bad.reason)
				summary.StillQuarantined = append(summary.StillQuarantined,
					refreshOutcome{ID: f.ID, Verdict: "invalid", Quarantine: bad.reason})
			} else {
				summary.Failures = append(summary.Failures, refreshOutcome{ID: f.ID, Verdict: "network"})
			}
		}
		return json.MarshalIndent(summary, "", "  ")
	}
	return nil, fmt.Errorf("no go-adapter auth named %q", authID)
}

// refreshAllJSON is the manual button for the whole pool: every key gets the same
// forced probe the per-auth quota button does, and recovered keys rejoin.
// #FUNC      The manual pool-wide re-probe: every key is checked, quarantined
//
//	keys revive on a valid answer, network failures cost nothing.
//
// #WHY       this is the documented recovery path for a definitive rejection;
//
//	routine checks stay off for quarantined keys by policy.
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
