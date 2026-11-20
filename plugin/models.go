package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// The catalogue is fetched from the gateway, never trusted from a table compiled
// into the binary.
//
// #WHY a baked list is the exact failure this project exists to point out: a
//
//	client that ships a snapshot cannot see a model added later, which is
//	why DSH/pi-ai never showed gemini-4-argon. The list below is only a
//	cold-start fallback for "nothing fetched yet and upstream down".
const (
	// modelCatalogueTTL bounds both how long a fetched list is served and how
	// long a failed attempt is remembered before another try.
	modelCatalogueTTL = 2 * time.Minute
	// modelProbeTimeout bounds one catalogue request. It is shorter than the
	// chat timeout because this call blocks the host's model registration.
	modelProbeTimeout = 15 * time.Second
	// modelCatalogueLimit caps how much of the response is read. It is NOT
	// errorLimit: a real catalogue is tens of KB, and the 16 KiB error-text cap
	// truncated the JSON in the first live run.
	modelCatalogueLimit = 4 << 20
	// modelIDLimit drops absurd ids before they reach the host registry.
	modelIDLimit = 200
)

const (
	catalogueSourceConfig    = "config"
	catalogueSourceUpstream  = "upstream"
	catalogueSourceLastGood  = "last-good"
	catalogueSourceBootstrap = "bootstrap"
)

// bootstrapModels is the cold-start fallback when the catalogue cannot be
// fetched and nothing was ever cached: the one model this plugin exists to
// serve, plus the two free-tier ids that cost nothing to route.
//
// #FUNC   Name the models served before any fetch has succeeded.
// #NOT    Not a catalogue: it is a floor, not a record. A snapshot copied here
//
//	rots — six of fourteen names were already gone when this was trimmed.
//
// #WHY    the fallback must stay minimal: a stale id here answers /v1/models
//
//	with a model that fails at request time, which is exactly the
//	baked-list failure this plugin exists to avoid.
var bootstrapModels = []string{
	"gemini-4-argon",
	"beam-free",
	"hy4-flash-free",
}

// modelKeyPoolFn returns the keys the catalogue may be fetched with. It is a seam
// so tests never need a host.
var modelKeyPoolFn = modelKeyPool

// fetchModelsFn performs the upstream /models call.
var fetchModelsFn = fetchUpstreamModels

// catalogue is one resolved model list plus where it came from.
type catalogue struct {
	IDs     []string
	Source  string
	Fetched time.Time // zero when nothing was fetched (config or bootstrap)
	base    string    // gateway this list describes
	nextTry time.Time // when the next fetch attempt is allowed
}

type catalogueFetch struct {
	done chan struct{}
	cat  catalogue
}

var (
	catalogueMu sync.Mutex
	// sharedCatalogue is the provider-wide list served on model.static.
	sharedCatalogue struct {
		cat      catalogue
		nextTry  time.Time
		base     string
		inflight *catalogueFetch
	}
	// perKeyCatalogue caches one account's list for model.for_auth: the model set
	// is per subscription, so two keys can legitimately disagree.
	perKeyCatalogue = map[string]catalogue{}
)

// currentCatalogue returns the provider-wide model list and its source.
//
// #FUNC     Resolve the list served on model.static: config, then a shared
// cached fetch, then the last good list, then the bootstrap list.
// #IN       nothing; reads the live config and the catalogue cache.
// #EDGE-IN  config `models:` set → never fetched. Two callers at once → one
//
//	fetch, both get its result. A failed attempt is cached for the TTL, so a
//	dead gateway is not re-tried on every call.
//
// #EDGE-IN  base-url changed by a reconfigure → cache miss, the old list is never
//
//	passed off as the new gateway's.
//
// #OUT      ids in upstream order and one of the catalogueSource values.
func currentCatalogue() catalogue {
	cfg := currentConfig()
	if len(cfg.Models) > 0 {
		return catalogue{IDs: sanitizeModelIDs(cfg.Models), Source: catalogueSourceConfig}
	}

	catalogueMu.Lock()
	if sharedCatalogue.base == cfg.BaseURL && sharedCatalogue.cat.Source != "" &&
		time.Now().Before(sharedCatalogue.nextTry) {
		out := copyCatalogue(sharedCatalogue.cat)
		catalogueMu.Unlock()
		return out
	}
	if flight := sharedCatalogue.inflight; flight != nil {
		catalogueMu.Unlock()
		<-flight.done
		return copyCatalogue(flight.cat)
	}
	// A fallback may only reuse a list from the same gateway: after a base-url
	// change the previous list describes a different upstream.
	prev := copyCatalogue(sharedCatalogue.cat)
	if prev.base != cfg.BaseURL {
		prev = catalogue{}
	}
	flight := &catalogueFetch{done: make(chan struct{})}
	sharedCatalogue.inflight = flight
	catalogueMu.Unlock()

	cat := catalogue{Source: catalogueSourceBootstrap, IDs: sanitizeModelIDs(bootstrapModels), base: cfg.BaseURL}
	func() {
		// Whatever happens inside the fetch, joiners must not wedge on a
		// channel nobody will close.
		defer func() {
			if recovered := recover(); recovered != nil {
				logWarn("go-adapter catalogue fetch panicked", map[string]any{"panic": fmt.Sprint(recovered)})
			}
		}()
		cat = fetchSharedCatalogue(cfg, prev)
	}()

	catalogueMu.Lock()
	sharedCatalogue.cat = cat
	sharedCatalogue.base = cfg.BaseURL
	sharedCatalogue.nextTry = time.Now().Add(modelCatalogueTTL)
	sharedCatalogue.inflight = nil
	flight.cat = cat
	close(flight.done)
	catalogueMu.Unlock()
	return copyCatalogue(cat)
}

// fetchSharedCatalogue performs the provider-wide fetch outside every lock.
// #FUNC      Fetch the provider-wide catalogue off every pick path.
// #IN        the live config and the last good catalogue.
// #EDGE-IN   every pooled key unusable → the previous list still serves, with a
//
//	warning → TestCatalogueKeepsLastGoodOnFailure
//
// #OUT       the fetched catalogue, or the previous/bootstrap one.
// #WHY       `modelCatalogueTTL` also caches failures, so a dead gateway is not
//
//	retried on every call.
func fetchSharedCatalogue(cfg pluginConfig, prev catalogue) catalogue {
	keys, errPool := modelKeyPoolFn()
	var lastErr error = errPool
	for _, key := range keys {
		models, errFetch := fetchModelsFn(cfg.BaseURL, key, cfg.UserAgent)
		if errFetch != nil {
			lastErr = errFetch
			continue
		}
		ids := sanitizeModelIDs(models)
		if len(ids) == 0 {
			lastErr = fmt.Errorf("catalogue had no usable ids")
			continue
		}
		return catalogue{IDs: ids, Source: catalogueSourceUpstream, Fetched: time.Now(),
			base: cfg.BaseURL}
	}

	if len(prev.IDs) > 0 {
		// Keep the real origin: a bootstrap list stays a bootstrap list.
		source := prev.Source
		if source != catalogueSourceBootstrap {
			source = catalogueSourceLastGood
		}
		if lastErr != nil {
			logWarn("go-adapter catalogue refresh failed; serving the previous catalogue",
				map[string]any{"error": lastErr.Error(), "models": len(prev.IDs), "source": source})
		}
		return catalogue{IDs: prev.IDs, Source: source, Fetched: prev.Fetched, base: prev.base}
	}

	fields := map[string]any{"models": len(bootstrapModels)}
	if errPool != nil {
		fields["reason"] = "auth list unavailable: " + errPool.Error()
	} else if len(keys) == 0 {
		fields["reason"] = "no usable key in the pool yet"
	}
	if lastErr != nil {
		fields["error"] = lastErr.Error()
	}
	logWarn("go-adapter catalogue unavailable; serving the bootstrap list", fields)
	return catalogue{IDs: sanitizeModelIDs(bootstrapModels), Source: catalogueSourceBootstrap,
		base: cfg.BaseURL}
}

// catalogueForKey is the per-account lookup used by model.for_auth.
//
// #FUNC     Return the model list belonging to one credential, cached briefly.
// #EDGE-IN  empty or unusable key → falls back to the shared catalogue.
// #EDGE-IN  a failed fetch → the shared catalogue, so a bad minute does not empty
//
//	the auth's model set.
//
// #OUT      ids and the source they came from.
func catalogueForKey(baseURL, apiKey, userAgent string) ([]string, string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		cat := currentCatalogue()
		return cat.IDs, cat.Source
	}
	cacheKey := quotaCacheKey(apiKey, baseURL)

	catalogueMu.Lock()
	if entry, ok := perKeyCatalogue[cacheKey]; ok && time.Now().Before(entry.nextTry) {
		ids := append([]string(nil), entry.IDs...)
		catalogueMu.Unlock()
		return ids, entry.Source
	}
	catalogueMu.Unlock()

	models, errFetch := fetchModelsFn(baseURL, apiKey, userAgent)
	if errFetch != nil {
		// Cache the failure: the host calls this on every auth change and every
		// refresh, and a dead gateway must not cost 15 s of registration time
		// per call. The shared catalogue is served meanwhile.
		cat := currentCatalogue()
		catalogueMu.Lock()
		perKeyCatalogue[cacheKey] = catalogue{IDs: cat.IDs, Source: cat.Source,
			Fetched: time.Now(), nextTry: time.Now().Add(modelCatalogueTTL)}
		catalogueMu.Unlock()
		return cat.IDs, cat.Source
	}
	ids := sanitizeModelIDs(models)

	catalogueMu.Lock()
	if len(perKeyCatalogue) > 64 {
		now := time.Now()
		for k, v := range perKeyCatalogue {
			if now.Sub(v.Fetched) > 2*modelCatalogueTTL {
				delete(perKeyCatalogue, k)
			}
		}
	}
	perKeyCatalogue[cacheKey] = catalogue{IDs: ids, Source: catalogueSourceUpstream,
		Fetched: time.Now(), nextTry: time.Now().Add(modelCatalogueTTL)}
	catalogueMu.Unlock()
	return append([]string(nil), ids...), catalogueSourceUpstream
}

// copyCatalogue returns a deep copy so callers cannot mutate the cache. The
// bookkeeping fields travel with it: dropping `base` made every fallback look
// like it came from another gateway.
func copyCatalogue(c catalogue) catalogue {
	return catalogue{
		IDs:     append([]string(nil), c.IDs...),
		Source:  c.Source,
		Fetched: c.Fetched,
		base:    c.base,
		nextTry: c.nextTry,
	}
}

// sanitizeModelIDs normalises whatever a gateway or an operator supplied.
//
// #FUNC     Trim, drop empties, drop control characters, cap length, dedupe.
// #EDGE-IN  "", "   ", "a\x00b", a 1 MiB id, duplicates → dropped or trimmed
//
//	before the id can reach the host registry and every /v1/models answer
//
// #OUT      ids in input order, duplicates removed.
func sanitizeModelIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || len(id) > modelIDLimit {
			continue
		}
		if strings.IndexFunc(id, isControlRune) >= 0 {
			continue
		}
		if !utf8.ValidString(id) {
			// JSON would rewrite the bytes to U+FFFD, so the id the host
			// registers would not be the id the gateway serves.
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// isControlRune reports C0/C1 control characters, which no model id may contain.
func isControlRune(r rune) bool { return unicode.IsControl(r) }

// modelKeyPool returns keys to try for the catalogue call: enabled, non
// quarantined auths of this provider, plus the host error when the list is
// unavailable so the log can tell an empty pool from a broken callback.
// #FUNC      Collect the keys a catalogue fetch may use.
// #EDGE-IN   a quarantined key → excluded: a dead key must not stand between
//
//	the pool and its model list → TestKeysFromAuthFilesSkipsUnusable
//
// #WHY       the pool is host state, re-read here because keys come and go.
func modelKeyPool() ([]string, error) {
	var list authFileList
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &list); err != nil {
		return nil, err
	}
	return keysFromAuthFiles(list.Files, currentConfig().BaseURL, keyForAuthID), nil
}

// keysFromAuthFiles filters host auth rows down to keys a catalogue call may use,
// skipping anything that cannot pay: wrong provider, disabled, a key the host
// cannot resolve, a key already proven dead, or a duplicate.
// #FUNC      Filter host auth rows down to usable catalogue keys.
// #EDGE-IN   wrong provider, disabled, unresolvable or duplicate key → skipped.
// #WHY       a duplicate `api_key` shares one quota cache entry, so fetching with
//
//	it twice would double upstream calls for the same account.
func keysFromAuthFiles(files []authFileEntry, baseURL string, keyFor func(string) (string, bool)) []string {
	keys := make([]string, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f.Provider != providerKey || f.Disabled || f.AuthIndex == "" {
			continue
		}
		key, ok := keyFor(f.ID)
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		if _, bad := quarantined(quotaCacheKey(key, baseURL)); bad {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// fetchUpstreamModels asks the gateway for its current model list.
//
// #FUNC     GET {base}/models with a key, and return the model ids.
// #EDGE-IN  non-200, a body that is not OpenAI-shaped, or an empty data array →
//
//	an error (never an empty list), so the caller keeps a usable catalogue
//
// #OUT      ids in upstream order, unsanitised.
func fetchUpstreamModels(baseURL, apiKey, userAgent string) ([]string, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/models"
	req, errReq := http.NewRequest(http.MethodGet, url, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	client := upstreamHTTPClient(modelProbeTimeout)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, modelCatalogueLimit))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: http %d: %s", resp.StatusCode, scrub(body, errorContext))
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("models: response is not OpenAI-shaped: %w", errUnmarshal)
	}
	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		models = append(models, m.ID)
	}
	// Sanitise at the network boundary: the caller (and the host registry)
	// only ever sees trimmed, deduped, control-free ids.
	clean := sanitizeModelIDs(models)
	if len(clean) == 0 {
		return nil, fmt.Errorf("models: empty catalogue in response")
	}
	return clean, nil
}

// modelInfos maps ids to the wire shape the host expects.
func modelInfos(ids []string) []modelInfo {
	models := make([]modelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, modelInfo{
			ID: id, Object: "model", OwnedBy: providerKey, Type: "chat",
			DisplayName: displayName(id), Name: id,
		})
	}
	return models
}

// handleModelForAuth serves model.for_auth: the catalogue this account can use.
//
// #FUNC     Answer per-auth model discovery with a live list.
// #IN       the host's AuthModelRequest JSON (StorageJSON carries our auth file).
// #EDGE-IN  storage that does not parse or has no api_key → the shared catalogue,
//
//	never an empty model set for that auth
//
// #OUT      modelResponse JSON with Provider and Models.
// #WHY      model.static is only sampled at startup and on config apply, so
//
//	without this path a model added upstream stays invisible until a
//	restart; this method is re-run when an auth changes and on its
//	refresh interval.
func handleModelForAuth(request []byte) ([]byte, error) {
	// StorageJSON is base64 on the wire ([]byte), exactly like every other
	// plugin RPC field: decoding it as a raw JSON token would hand us the
	// base64 *string* and silently lose the credential.
	var req struct {
		AuthID       string `json:"AuthID"`
		AuthProvider string `json:"AuthProvider"`
		StorageJSON  []byte `json:"StorageJSON"`
	}
	_ = json.Unmarshal(request, &req)
	cfg := currentConfig()
	var cred credential
	if len(req.StorageJSON) > 0 {
		_ = json.Unmarshal(req.StorageJSON, &cred)
	}
	ids, source := catalogueForKey(cfg.BaseURL, cred.APIKey, cfg.UserAgent)
	logInfo("go-adapter model discovery for auth", map[string]any{
		"auth_id": req.AuthID, "models": len(ids), "source": source,
	})
	return json.Marshal(modelResponse{Provider: providerKey, Models: modelInfos(ids)})
}

// logWarn writes one warning line to the host log, tagged with the plugin id.
func logWarn(message string, fields map[string]any) { logAt("warn", message, fields) }

// logInfo writes one info line to the host log.
func logInfo(message string, fields map[string]any) { logAt("info", message, fields) }

// logAt is the single host-log writer for plugin-level messages.
func logAt(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = providerKey
	_ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	}, nil)
}
