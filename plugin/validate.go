package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Key validity, checked against the gateway itself. The usage route is the
// probe: one request proves both the credential and the subscription, and it
// costs no tokens — which is exactly what an add-time check needs.

const (
	statusValid   = "valid"
	statusInvalid = "invalid"
	statusUnknown = "unknown"

	keyCheckTimeout = 10 * time.Second
	refreshInterval = 15 * time.Minute
)

// invalidKeyError marks a key the gateway definitively rejected: 401, or a
// subscription without the Go plan. This is the only failure that says something
// about the key itself, so it is the only one that may sideline a key.
type invalidKeyError struct{ reason string }

func (e invalidKeyError) Error() string { return "invalid key: " + e.reason }

// transientUsageError marks a usage read that failed for a reason which says
// nothing about the key: 5xx, 429, another odd status, a body that is not a usage
// document, or no HTTP answer at all.
//
// #WHY a bad minute at the gateway, a WAF page, a proxy timeout or a schema
//
//	change must never cost a working key its place in the pool.
type transientUsageError struct{ reason string }

func (e transientUsageError) Error() string { return "usage unavailable: " + e.reason }

// checkKey probes the gateway live. There is deliberately no result cache: the
// only callers are auth parse and auth refresh, both rare, and a cached verdict
// is exactly how a just-revoked key slips past an add-time check.
func checkKey(apiKey, baseURL string) (string, string) {
	return probeKey(&http.Client{Timeout: keyCheckTimeout}, apiKey, baseURL)
}

// probeKey runs one live probe and classifies it.
//
// #FUNC     Classify one key against the gateway's usage route.
// #EDGE-IN  401, or 403 EntitlementError → statusInvalid with the reason
// #EDGE-IN  5xx / 429 / malformed / no answer → statusUnknown, never a disable
// #EDGE-OUT only statusInvalid may quarantine or disable a key.
// #WHY      quarantine is for a key the gateway has rejected, not for a network
//
//	that is having a bad minute; a 502 at 03:00 must not empty the pool.
func probeKey(client *http.Client, apiKey, baseURL string) (string, string) {
	if _, err := fetchUsageWith(client, apiKey, baseURL); err != nil {
		var bad invalidKeyError
		if errors.As(err, &bad) {
			return statusInvalid, bad.reason
		}
		var transient transientUsageError
		if errors.As(err, &transient) {
			return statusUnknown, transient.reason
		}
		return statusUnknown, "usage-unreachable"
	}
	return statusValid, ""
}

// keyMetadata is what the auth record carries so the panel and the logs can say
// why a key is quarantined instead of showing a silent skip. It also carries the
// refresh interval the host reads (conductor_refresh.go durationFromMetadata) to
// schedule re-validation, because NextRefreshAfter alone does not schedule
// anything for plugin providers.
//
// #FUNC     Build auth metadata: verdict, check time, refresh interval.
// #WHY      `refresh_interval_seconds` is what makes the host actually call
//
//	auth.refresh for a plugin provider; without it a revoked key stays
//	enabled until a file changes.
func keyMetadata(status, reason string) map[string]any {
	meta := map[string]any{
		"key_status":               status,
		"key_checked_at":           time.Now().UTC().Format(time.RFC3339),
		"refresh_interval_seconds": int(refreshInterval.Seconds()),
	}
	if reason != "" {
		meta["key_reason"] = reason
	}
	if note := visibleNote(status, reason); note != "" {
		// The host's auth-files listing renders "note" verbatim next to each
		// key (auth_files.go), so the quarantine is visible where users
		// already manage keys, not only on the plugin page.
		meta["note"] = note
	}
	return meta
}

// visibleNote is the one-line, human-readable verdict shown in the host panel.
// Valid keys stay silent; everything a debugger needs is on one line.
func visibleNote(status, reason string) string {
	switch status {
	case statusInvalid, "quarantined":
		return "go: QUARANTINED (" + reason + ") — manual quota refresh rechecks"
	case "storage-corrupt":
		return "go: storage corrupt — record disabled"
	case statusUnknown:
		return "go: usage probe unavailable (" + reason + ") — key left as it was"
	}
	return ""
}

// usageURL derives the current usage route from the configured chat base:
// https://…/inference/go/openai/v1 -> https://…/inference/go/v1/usage
func usageURL(base string) string {
	b := strings.TrimSuffix(base, "/")
	b = strings.TrimSuffix(b, "/openai/v1")
	b = strings.TrimSuffix(b, "/v1")
	return b + "/v1/usage"
}

// legacyUsageURL is the older route that serves the same document:
// https://…/zen/go/v1/usage.
//
// #WHY it is the single fallback when the current route is missing or broken, so
//
//	a route rename costs a fallback instead of quarantining every key.
func legacyUsageURL(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/zen/go/v1/usage"
}

// fetchUsageWith is the single HTTP path for usage reads; the quota cache and the
// key probe share it so classification cannot drift between them.
//
// #FUNC     Read the three windows, trying the current route then the legacy one.
// #EDGE-IN  401 / 403 EntitlementError → invalidKeyError, fallback NOT tried: the
//
//	key is the problem, not the route
//
// #EDGE-IN  other non-200, malformed body, or no answer → next route; both failing
//
//	yields transientUsageError
//
// #OUT      normalised Quota, or invalidKeyError / transientUsageError.
func fetchUsageWith(client *http.Client, apiKey, base string) (Quota, error) {
	routes := []string{usageURL(base)}
	if legacy := legacyUsageURL(base); legacy != "" && legacy != routes[0] {
		routes = append(routes, legacy)
	}
	var lastErr error
	for _, route := range routes {
		quota, errFetch := fetchUsageRoute(client, route, apiKey)
		if errFetch == nil {
			return quota, nil
		}
		var bad invalidKeyError
		if errors.As(errFetch, &bad) {
			return Quota{}, bad
		}
		lastErr = errFetch
	}
	if lastErr == nil {
		lastErr = transientUsageError{reason: "no-route"}
	}
	return Quota{}, lastErr
}

// fetchUsageRoute performs one usage request and classifies the answer.
func fetchUsageRoute(client *http.Client, route, apiKey string) (Quota, error) {
	req, errReq := http.NewRequest(http.MethodGet, route, nil)
	if errReq != nil {
		return Quota{}, transientUsageError{reason: "bad-request"}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, errDo := client.Do(req)
	if errDo != nil {
		return Quota{}, transientUsageError{reason: "unreachable"}
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return Quota{}, transientUsageError{reason: "unreadable-body"}
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized:
		return Quota{}, invalidKeyError{reason: "unauthorized"}
	case resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "EntitlementError"):
		// 403 EntitlementError: the key authenticates but has no Go plan.
		return Quota{}, invalidKeyError{reason: "no-go-plan"}
	default:
		return Quota{}, transientUsageError{reason: "http-" + strconv.Itoa(resp.StatusCode)}
	}
	var ur usageResponse
	if errUnmarshal := json.Unmarshal(body, &ur); errUnmarshal != nil {
		return Quota{}, transientUsageError{reason: "not-a-usage-document"}
	}
	// A 200 carrying some other JSON (a WAF page with {"error":…}, a renamed
	// schema) is not proof that the key works: without all three windows this is
	// an unusable answer, and treating it as valid would clear a real quarantine.
	quota := Quota{
		Rolling: toWindow(ur.Usage.Rolling),
		Weekly:  toWindow(ur.Usage.Weekly),
		Monthly: toWindow(ur.Usage.Monthly),
	}
	if !quota.Rolling.Known || !quota.Weekly.Known || !quota.Monthly.Known {
		return Quota{}, transientUsageError{reason: "not-a-usage-document"}
	}
	return Quota{
		Rolling: toWindow(ur.Usage.Rolling),
		Weekly:  toWindow(ur.Usage.Weekly),
		Monthly: toWindow(ur.Usage.Monthly),
	}, nil
}
