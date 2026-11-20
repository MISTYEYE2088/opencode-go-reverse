package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Subscription quota lives on the Go gateway, not on the inference path:
//
//	GET {base}/usage  ->  {"usage":{"rolling":…,"weekly":…,"monthly":…}}
//
// The base is the same endpoint family as chat, minus the OpenAI suffix.
const (
	usagePath    = "/usage"
	quotaTTL     = 60 * time.Second
	usageTimeout = 5 * time.Second
)

// usageWindow is one window as the gateway reports it.
type usageWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type usageResponse struct {
	Usage struct {
		Rolling usageWindow `json:"rolling"`
		Weekly  usageWindow `json:"weekly"`
		Monthly usageWindow `json:"monthly"`
	} `json:"usage"`
}

// quotaEntry caches one account's windows; failures are cached too, so a broken
// endpoint is not hammered once per request.
type quotaEntry struct {
	quota Quota
	err   error
	at    time.Time
}

var (
	quotaMu    sync.Mutex
	quotaCache = map[string]quotaEntry{}
)

// fetchUsageFn is a seam so tests can fault the leader path.
var fetchUsageFn = fetchUsage

// errQuotaUnknown means "no snapshot yet": the scheduler treats it as neutral
// rather than as a reason to suspect the key.
var errQuotaUnknown = errors.New("quota unknown")

// seedQuota records windows that were read for another purpose, so the first
// scheduler pick already has a real score instead of the neutral one.
//
// #FUNC     Store a quota snapshot that some earlier call already fetched.
// #EDGE-IN  a probe that failed carried no windows (Quota{}.Known() is false) →
//
//	nothing is stored, so a network blip never looks like a real reading.
//
// #WHY      the key validation at add/refresh time hits the same usage route; the
//
//	numbers were being thrown away and the first pick scored 450.
func seedQuota(apiKey, base string, quota Quota) {
	if strings.TrimSpace(apiKey) == "" || !quota.Known() {
		return
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	cacheKey := quotaCacheKey(apiKey, base)
	if reason, bad := quarantined(cacheKey); bad {
		// A quarantined key stays quarantined: a snapshot must not paper over a
		// definitive rejection.
		_ = reason
		return
	}
	quotaCache[cacheKey] = quotaEntry{quota: quota, at: time.Now()}
}

// fetchUsage reads the windows for one key and normalises them into Quota.
func fetchUsage(apiKey, base string) (Quota, error) {
	return fetchUsageWith(&http.Client{Timeout: usageTimeout}, apiKey, base)
}

// toWindow validates one window. A malformed window becomes Unknown rather than
// zero: a silent zero would make a full account look empty.
func toWindow(w usageWindow) Window {
	if w.Status != "ok" && w.Status != "rate-limited" {
		return Window{}
	}
	if w.Percent < 0 || w.Percent > 100 {
		return Window{}
	}
	out := Window{Percent: w.Percent, Status: w.Status, Known: true}
	if t, errParse := time.Parse(time.RFC3339, w.ResetsAt); errParse == nil {
		out.ResetsAt = t
	}
	return out
}

// inflightEntry is one in-flight usage fetch per cache key: joiners block on
// done and reuse the leader's result instead of issuing their own call.
type inflightEntry struct {
	done  chan struct{}
	quota Quota
	err   error
}

var (
	inflightMu sync.Mutex
	inflight   = map[string]*inflightEntry{}
)

// quotaFor returns the cached snapshot for a credential, refreshing past the TTL.
//
// #FUNC     Cached quota snapshot per credential, single-flighted per key.
// #EDGE_IN  Two picks miss the cache at once → the joiner waits for the leader's
//
//	fetch, one upstream call → TestConcurrentQuotaForSingleFlight
//
// #EDGE_IN  Distinct keys miss concurrently → fetches run in parallel, nothing
//
//	serializes → TestConcurrentQuotaForDistinctKeysRunInParallel
//
// #WHY      `quotaMu` is never held across network I/O: a global lock around the
//
//	5s fetch turned one slow /usage answer into a head-of-line stall
//	for every pick, and the unlocked sweep raced the map write.
func quotaFor(authID, apiKey string) (Quota, error) {
	cfgLive := currentConfig()
	cacheKey := quotaCacheKey(apiKey, cfgLive.BaseURL)

	// Routine checks are disabled for quarantined keys: no fetch, no upstream
	// call. Only the manual refresh re-admits the key.
	if reason, bad := quarantined(cacheKey); bad {
		return Quota{}, quarantinedError{reason: reason}
	}

	quotaMu.Lock()
	if entry, ok := quotaCache[cacheKey]; ok && time.Since(entry.at) < quotaTTL {
		quotaMu.Unlock()
		return entry.quota, entry.err
	}
	quotaMu.Unlock()

	inflightMu.Lock()
	flight, joined := inflight[cacheKey]
	if !joined {
		flight = &inflightEntry{done: make(chan struct{})}
		inflight[cacheKey] = flight
	}
	inflightMu.Unlock()

	if joined {
		<-flight.done
		return flight.quota, flight.err
	}

	// Leader installs cleanup before any work: whatever happens next, joiners
	// must never wedge on an inflight entry nobody will close.
	flightCleanupDone := false
	defer func() {
		if flightCleanupDone {
			return
		}
		inflightMu.Lock()
		delete(inflight, cacheKey)
		inflightMu.Unlock()
		close(flight.done)
	}()

	// Leader fetches with no lock held, then publishes.
	flight.quota, flight.err = fetchUsageFn(apiKey, cfgLive.BaseURL)
	quarantineAfterProbe(cacheKey, flight.err)

	quotaMu.Lock()
	quotaCache[cacheKey] = quotaEntry{quota: flight.quota, err: flight.err, at: time.Now()}
	sweepQuotaCacheLocked()
	quotaMu.Unlock()

	inflightMu.Lock()
	delete(inflight, cacheKey)
	inflightMu.Unlock()
	close(flight.done)
	flightCleanupDone = true
	return flight.quota, flight.err
}

// quarantineAfterProbe flags a key the gateway definitively rejected, and only
// that: an expired/invalid key, or an account without the Go plan. 5xx, 429, a
// timeout or a body that is not a usage document is the network talking, so the
// key keeps its standing.
//
// A successful probe never clears the flag here: the routine path is not the
// recovery channel — a manual quota check is.
func quarantineAfterProbe(cacheKey string, err error) {
	if err == nil {
		return
	}
	var bad invalidKeyError
	if errors.As(err, &bad) {
		quarantineSet(cacheKey, "invalid: "+bad.reason)
	}
}

// quotaCached answers from the cache only and never blocks on the network.
//
// #FUNC     Cache-only quota read for the routing path.
// #EDGE-IN  cold cache → neutral "unknown" plus a background warm-up, never a
//
//	5s stall inside scheduler.pick
//
// #OUT      snapshot and its (cached) error, or errQuotaUnknown.
// #WHY      the pick runs on the client's request path; a serial /usage sweep
//
//	made one key's slow answer delay every request that routed through it.
func quotaCached(apiKey string) (Quota, error) {
	cfg := currentConfig()
	cacheKey := quotaCacheKey(apiKey, cfg.BaseURL)
	if reason, bad := quarantined(cacheKey); bad {
		return Quota{}, quarantinedError{reason: reason}
	}
	quotaMu.Lock()
	entry, ok := quotaCache[cacheKey]
	quotaMu.Unlock()
	if !ok {
		kickQuotaRefresh(apiKey, cfg.BaseURL)
		return Quota{}, errQuotaUnknown
	}
	if time.Since(entry.at) >= quotaTTL {
		kickQuotaRefresh(apiKey, cfg.BaseURL)
	}
	return entry.quota, entry.err
}

// kickQuotaRefresh warms the cache off the request path, at most one goroutine
// per credential at a time.
//
// #FUNC     Start a background quota refresh unless one is already running.
// #EDGE-IN  hundreds of picks in the same second → one goroutine per key, not one
//
//	per pick → TestQuotaWarmupSpawnsOneGoroutine
//
// #WHY      an unguarded goroutine per pick parked up to N_picks × N_candidates
//
//	blocked goroutines during a /usage outage (measured: 300 picks
//	× 3 candidates → ~900 goroutines).
func kickQuotaRefresh(apiKey, base string) {
	cacheKey := quotaCacheKey(apiKey, base)
	warmMu.Lock()
	if _, running := warming[cacheKey]; running {
		warmMu.Unlock()
		return
	}
	warming[cacheKey] = struct{}{}
	warmMu.Unlock()

	go func() {
		defer func() { _ = recover() }()
		defer func() {
			warmMu.Lock()
			delete(warming, cacheKey)
			warmMu.Unlock()
		}()
		_, _ = quotaFor("", apiKey)
	}()
}

var (
	warmMu  sync.Mutex
	warming = map[string]struct{}{}
)

// sweepQuotaCacheLocked drops entries nobody has touched for two TTLs. It must
// run under quotaMu: an unlocked sweep races the map write and, past 64
// entries, escalates to an unrecoverable runtime throw inside the host process.
func sweepQuotaCacheLocked() {
	if len(quotaCache) <= 64 {
		return
	}
	now := time.Now()
	for k, e := range quotaCache {
		if now.Sub(e.at) > 2*quotaTTL {
			delete(quotaCache, k)
		}
	}
}

// quotaCacheKey derives the cache id from the credential, never the auth id.
// #FUNC      The cache key for one credential's quota: sha256(api_key + base).
// #WHY       keyed by the key string so two records sharing one `api_key` share
//
//	one snapshot — the account is the same upstream entity.
func quotaCacheKey(apiKey, baseURL string) string {
	sum := sha256.Sum256([]byte(apiKey + "\x00" + baseURL))
	return hex.EncodeToString(sum[:8])
}

// quotaFetchRequest is the host's quota.fetch payload (snake_case).
type quotaFetchRequest struct {
	AuthIndex   string `json:"auth_index"`
	AuthID      string `json:"auth_id"`
	Provider    string `json:"provider"`
	StorageJSON []byte `json:"storage_json"`
}

// bucket converts one window into the host's rendering shape.
func bucket(name string, w Window, now time.Time) map[string]any {
	frac := 0.0
	reset := ""
	if w.Known {
		frac = w.Remaining() / 100
		if !w.ResetsAt.IsZero() {
			reset = w.ResetsAt.UTC().Format(time.RFC3339)
		}
	}
	return map[string]any{
		"window":            name,
		"remainingFraction": frac,
		"resetTime":         reset,
	}
}

// handleQuotaFetch answers quota.fetch with the three windows as buckets.
func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req quotaFetchRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	var cred credential
	_ = json.Unmarshal(req.StorageJSON, &cred)
	apiKey := strings.TrimSpace(cred.APIKey)
	if apiKey == "" {
		if k, ok := keyForAuthID(req.AuthID); ok {
			apiKey = k
		}
	}
	if apiKey == "" {
		return nil, fmt.Errorf("no credential for auth %q", req.AuthID)
	}

	// The manual path (the panel's quota button): always a live probe, never
	// the cache and never the quarantine skip. A valid answer re-admits the
	// key to the working pool; a non-transport failure keeps it flagged.
	q, err := fetchUsageWith(&http.Client{Timeout: usageTimeout}, apiKey, currentConfig().BaseURL)
	cacheKey := quotaCacheKey(apiKey, currentConfig().BaseURL)
	if err != nil {
		quarantineAfterProbe(cacheKey, err)
		return nil, err
	}
	quarantineClear(cacheKey)
	quotaMu.Lock()
	quotaCache[cacheKey] = quotaEntry{quota: q, err: nil, at: time.Now()}
	quotaMu.Unlock()
	now := time.Now()
	resp := map[string]any{
		"groups": []any{map[string]any{
			"displayName": "OpenCode Go",
			"buckets": []any{
				bucket("5h", q.Rolling, now),
				bucket("weekly", q.Weekly, now),
				bucket("monthly", q.Monthly, now),
			},
		}},
		"summary": []any{
			map[string]any{"key": "score", "label": "Scheduler score", "value": mustScore(q), "unit": "pts"},
		},
	}
	return json.Marshal(resp)
}

// mustScore exposes the scheduler score in the panel so the numbers are visible.
func mustScore(q Quota) int {
	s, _ := Score(q, false, defaultScoreOptions(time.Now()))
	return s
}

// handleQuotaDescribe announces the provider and its reset capability.
func handleQuotaDescribe() ([]byte, error) {
	return json.Marshal(map[string]any{
		"supported_providers": []string{providerKey},
		"display_name":        "OpenCode Go",
		"supports_reset":      false,
	})
}

// quotaMethodList is the set of quota methods this plugin implements.
var quotaMethodList = []string{
	pluginabi.MethodQuotaIdentifier,
	pluginabi.MethodQuotaDescribe,
	pluginabi.MethodQuotaFetch,
	pluginabi.MethodQuotaReset,
}
