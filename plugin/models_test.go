package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetCatalogue clears the cache and restores the seams after a test.
func resetCatalogue(t *testing.T) {
	t.Helper()
	catalogueMu.Lock()
	sharedCatalogue.cat = catalogue{}
	sharedCatalogue.nextTry = time.Time{}
	sharedCatalogue.base = ""
	sharedCatalogue.inflight = nil
	perKeyCatalogue = map[string]catalogue{}
	catalogueMu.Unlock()
	keyPool, fetch := modelKeyPoolFn, fetchModelsFn
	t.Cleanup(func() {
		modelKeyPoolFn, fetchModelsFn = keyPool, fetch
		catalogueMu.Lock()
		sharedCatalogue.cat = catalogue{}
		sharedCatalogue.nextTry = time.Time{}
		sharedCatalogue.base = ""
		sharedCatalogue.inflight = nil
		perKeyCatalogue = map[string]catalogue{}
		catalogueMu.Unlock()
	})
}

// stubCatalogue points the two seams at fixed values and counts fetches.
func stubCatalogue(keys []string, fetch func(string, string, string) ([]string, error)) *atomic.Int32 {
	var calls atomic.Int32
	modelKeyPoolFn = func() ([]string, error) { return append([]string(nil), keys...), nil }
	fetchModelsFn = func(baseURL, apiKey, userAgent string) ([]string, error) {
		calls.Add(1)
		return fetch(baseURL, apiKey, userAgent)
	}
	return &calls
}

// withModels pins the configured model list for one test.
func withModels(t *testing.T, models ...string) {
	t.Helper()
	old := currentConfig()
	cfg := old
	cfg.Models = stringList(models)
	configPtr.Store(&cfg)
	t.Cleanup(func() { configPtr.Store(&old) })
}

// TestCatalogueComesFromUpstream pins the point of models.go: with no configured
// list, the advertised models are the ones the gateway returned, not a baked table.
func TestCatalogueComesFromUpstream(t *testing.T) {
	resetCatalogue(t)
	stubCatalogue([]string{"oc_sk_test"}, func(string, string, string) ([]string, error) {
		return []string{"fresh-model-a", "fresh-model-b"}, nil
	})

	cat := currentCatalogue()
	if cat.Source != catalogueSourceUpstream {
		t.Fatalf("source = %q, want %q", cat.Source, catalogueSourceUpstream)
	}
	if strings.Join(cat.IDs, ",") != "fresh-model-a,fresh-model-b" {
		t.Fatalf("ids = %v", cat.IDs)
	}
	if cat.Fetched.IsZero() {
		t.Fatal("fetched time not recorded")
	}
}

// TestCatalogueServedFromCacheWithinTTL: a second call inside the TTL must not
// spend another upstream request.
func TestCatalogueServedFromCacheWithinTTL(t *testing.T) {
	resetCatalogue(t)
	calls := stubCatalogue([]string{"oc_sk_test"}, func(string, string, string) ([]string, error) {
		return []string{"m1"}, nil
	})

	currentCatalogue()
	currentCatalogue()
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

// TestCatalogueConfigPinsList: an explicit models: list wins and no call is made.
func TestCatalogueConfigPinsList(t *testing.T) {
	resetCatalogue(t)
	calls := stubCatalogue([]string{"oc_sk_test"}, func(string, string, string) ([]string, error) {
		return []string{"upstream-model"}, nil
	})
	withModels(t, "pinned-a", "pinned-b")

	cat := currentCatalogue()
	if cat.Source != catalogueSourceConfig {
		t.Fatalf("source = %q, want config", cat.Source)
	}
	if strings.Join(cat.IDs, ",") != "pinned-a,pinned-b" {
		t.Fatalf("ids = %v", cat.IDs)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestCatalogueKeepsLastGoodOnFailure: a failed refresh serves the previous
// upstream list instead of dropping models out of /v1/models.
func TestCatalogueKeepsLastGoodOnFailure(t *testing.T) {
	resetCatalogue(t)
	fail := atomic.Bool{}
	stubCatalogue([]string{"oc_sk_test"}, func(string, string, string) ([]string, error) {
		if fail.Load() {
			return nil, fmt.Errorf("boom")
		}
		return []string{"known-model"}, nil
	})

	if got := currentCatalogue(); got.Source != catalogueSourceUpstream {
		t.Fatalf("first source = %q", got.Source)
	}
	// Expire the cache without waiting.
	catalogueMu.Lock()
	sharedCatalogue.nextTry = time.Time{}
	catalogueMu.Unlock()
	fail.Store(true)

	cat := currentCatalogue()
	if cat.Source != catalogueSourceLastGood {
		t.Fatalf("source = %q, want last-good", cat.Source)
	}
	if strings.Join(cat.IDs, ",") != "known-model" {
		t.Fatalf("ids = %v", cat.IDs)
	}
}

// TestCatalogueBootstrapWhenNothingUsable: no key and no cache still answers,
// but says so in the source field rather than pretending the list is live.
func TestCatalogueBootstrapWhenNothingUsable(t *testing.T) {
	resetCatalogue(t)
	stubCatalogue(nil, func(string, string, string) ([]string, error) {
		t.Fatal("must not fetch without a key")
		return nil, nil
	})

	cat := currentCatalogue()
	if cat.Source != catalogueSourceBootstrap {
		t.Fatalf("source = %q, want bootstrap", cat.Source)
	}
	if len(cat.IDs) != len(bootstrapModels) {
		t.Fatalf("ids = %v", cat.IDs)
	}
	if !cat.Fetched.IsZero() {
		t.Fatal("bootstrap must not claim a fetch time")
	}
}

// TestCatalogueTriesNextKeyWhenOneFails: one dead key must not hide the catalogue.
func TestCatalogueTriesNextKeyWhenOneFails(t *testing.T) {
	resetCatalogue(t)
	var tried []string
	var mu sync.Mutex
	stubCatalogue([]string{"oc_sk_dead", "oc_sk_good"}, func(_ string, apiKey, _ string) ([]string, error) {
		mu.Lock()
		tried = append(tried, apiKey)
		mu.Unlock()
		if apiKey == "oc_sk_dead" {
			return nil, fmt.Errorf("401")
		}
		return []string{"m1"}, nil
	})

	cat := currentCatalogue()
	if cat.Source != catalogueSourceUpstream || len(cat.IDs) != 1 {
		t.Fatalf("source=%q ids=%v", cat.Source, cat.IDs)
	}
	if len(tried) != 2 || tried[1] != "oc_sk_good" {
		t.Fatalf("tried = %v", tried)
	}
}

// TestFetchUpstreamModelsRejectsBadResponses: a non-200, a body that is not
// OpenAI-shaped, and an empty data array are errors, never an empty catalogue.
func TestFetchUpstreamModelsRejectsBadResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"no"}}`},
		{"server error", http.StatusInternalServerError, `oops`},
		{"not json", http.StatusOK, `<html>not a catalogue</html>`},
		{"no data", http.StatusOK, `{"data":[]}`},
		{"wrong shape", http.StatusOK, `{"models":["a"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := fetchUpstreamModels(srv.URL+"/v1", "oc_sk_test", "ua"); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

// TestFetchUpstreamModelsParsesAndDedupes: real shape, duplicate ids, blank ids.
func TestFetchUpstreamModelsParsesAndDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oc_sk_test" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "ua" {
			t.Errorf("user-agent = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"a"},{"id":"  "},{"id":"b"}]}`))
	}))
	defer srv.Close()

	models, err := fetchUpstreamModels(srv.URL+"/v1", "oc_sk_test", "ua")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(models, ",") != "a,b" {
		t.Fatalf("models = %v", models)
	}
}

// TestKeysFromAuthFilesSkipsUnusable: the filtering rules that keep a dead or
// quarantined key out of the catalogue call.
func TestKeysFromAuthFilesSkipsUnusable(t *testing.T) {
	resetCatalogue(t)
	base := currentConfig().BaseURL
	quarantineClear(quotaCacheKey("oc_sk_quarantined", base))
	quarantineSet(quotaCacheKey("oc_sk_quarantined", base), "unauthorized")
	t.Cleanup(func() { quarantineClear(quotaCacheKey("oc_sk_quarantined", base)) })

	rows := []authFileEntry{
		{ID: "good", Provider: providerKey, AuthIndex: "1"},
		{ID: "disabled", Provider: providerKey, AuthIndex: "2", Disabled: true},
		{ID: "other-provider", Provider: "openai", AuthIndex: "3"},
		{ID: "no-index", Provider: providerKey},
		{ID: "unresolvable", Provider: providerKey, AuthIndex: "4"},
		{ID: "quarantined", Provider: providerKey, AuthIndex: "5"},
		{ID: "duplicate", Provider: providerKey, AuthIndex: "6"},
	}
	keys := map[string]string{
		"good":           "oc_sk_good",
		"disabled":       "oc_sk_disabled",
		"other-provider": "oc_sk_other",
		"unresolvable":   "",
		"quarantined":    "oc_sk_quarantined",
		"duplicate":      "oc_sk_good",
	}
	got := keysFromAuthFiles(rows, base, func(id string) (string, bool) {
		key := keys[id]
		return key, key != ""
	})
	if strings.Join(got, ",") != "oc_sk_good" {
		t.Fatalf("keys = %v", got)
	}
}

// TestFetchUpstreamModelsHandlesRealSizeCatalogue is the regression for a real
// failure: the first live fetch read the body through the 16 KiB error-text cap,
// truncated the JSON and fell back to the bootstrap list. A real catalogue is
// ~19 KB, so the reader must not use the error cap.
func TestFetchUpstreamModelsHandlesRealSizeCatalogue(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"model-%03d","object":"model","description":%q}`,
			i, strings.Repeat("long description ", 20))
	}
	b.WriteString(`]}`)
	if b.Len() <= errorLimit {
		t.Fatalf("test payload is only %d bytes, must exceed errorLimit %d to be a regression", b.Len(), errorLimit)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	models, err := fetchUpstreamModels(srv.URL+"/v1", "oc_sk_test", "ua")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 200 {
		t.Fatalf("models = %d, want 200", len(models))
	}
}

// withBase points the plugin at another gateway for one test, exactly what a
// reconfigure does.
func withBase(t *testing.T, baseURL string) {
	t.Helper()
	old := currentConfig()
	cfg := old
	cfg.BaseURL = baseURL
	configPtr.Store(&cfg)
	t.Cleanup(func() { configPtr.Store(&old) })
}

// The model set belongs to the subscription, so model.for_auth must answer with
// the catalogue of the key it was given, not the provider-wide one.
func TestForAuthServesAccountCatalogue(t *testing.T) {
	resetCatalogue(t)
	modelKeyPoolFn = func() ([]string, error) { return []string{"oc_sk_shared"}, nil }
	fetchModelsFn = func(_ string, apiKey, _ string) ([]string, error) {
		if apiKey == "oc_sk_account_b" {
			return []string{"b-only-model"}, nil
		}
		return []string{"a-only-model"}, nil
	}
	raw, _ := json.Marshal(map[string]any{
		"AuthID":       "go-2",
		"AuthProvider": providerKey,
		"StorageJSON":  []byte(`{"api_key":"oc_sk_account_b"}`),
	})
	out, err := handleModelForAuth(raw)
	if err != nil {
		t.Fatalf("for_auth failed: %v", err)
	}
	if !strings.Contains(string(out), "b-only-model") || strings.Contains(string(out), "a-only-model") {
		t.Fatalf("for_auth must use that account's catalogue, got %s", out)
	}
}

// An auth record we cannot read must not empty that auth's model set.
func TestForAuthFallsBackWhenStorageUnreadable(t *testing.T) {
	resetCatalogue(t)
	modelKeyPoolFn = func() ([]string, error) { return []string{"oc_sk_shared"}, nil }
	fetchModelsFn = func(_ string, apiKey, _ string) ([]string, error) {
		if apiKey == "oc_sk_shared" {
			return []string{"shared-model"}, nil
		}
		return nil, fmt.Errorf("unexpected key %q", apiKey)
	}
	out, err := handleModelForAuth([]byte(`{"AuthID":"go-3","StorageJSON":"not json"}`))
	if err != nil {
		t.Fatalf("for_auth failed: %v", err)
	}
	if !strings.Contains(string(out), "shared-model") {
		t.Fatalf("unreadable storage must fall back to the shared catalogue, got %s", out)
	}
}

// Ids from the gateway or an operator are untrusted: trim, drop empties and
// control characters, cap the length, dedupe.
func TestCatalogueIDsSanitised(t *testing.T) {
	got := sanitizeModelIDs([]string{
		" good ", "", "   ", "bad\x00id", "ansi\x1b[31mred", "dup", "dup",
		strings.Repeat("x", modelIDLimit+1),
	})
	if strings.Join(got, ",") != "good,dup" {
		t.Fatalf("sanitised = %v", got)
	}
}

// A reconfigured base-url must not keep serving the previous gateway's list.
func TestCatalogueRefetchesAfterBaseURLChange(t *testing.T) {
	resetCatalogue(t)
	modelKeyPoolFn = func() ([]string, error) { return []string{"oc_sk_k"}, nil }
	fetchModelsFn = func(baseURL, _ string, _ string) ([]string, error) {
		if strings.Contains(baseURL, "gw-b") {
			return []string{"b-model"}, nil
		}
		return []string{"a-model"}, nil
	}
	withBase(t, "https://gw-a.example/v1")
	if cat := currentCatalogue(); strings.Join(cat.IDs, ",") != "a-model" {
		t.Fatalf("first catalogue = %v", cat.IDs)
	}
	withBase(t, "https://gw-b.example/v1")
	cat := currentCatalogue()
	if strings.Join(cat.IDs, ",") != "b-model" {
		t.Fatalf("after base-url change the catalogue must be refetched, got %v", cat.IDs)
	}
}

// A failed fetch is remembered for the TTL: a dead gateway must not be re-tried
// on every model.static call or panel GET.
func TestCatalogueFailureIsCached(t *testing.T) {
	resetCatalogue(t)
	var calls atomic.Int32
	modelKeyPoolFn = func() ([]string, error) { return []string{"oc_sk_dead"}, nil }
	fetchModelsFn = func(string, string, string) ([]string, error) {
		calls.Add(1)
		return nil, fmt.Errorf("connection refused")
	}
	first := currentCatalogue()
	second := currentCatalogue()
	if calls.Load() != 1 {
		t.Fatalf("failed attempt must be cached, upstream calls = %d", calls.Load())
	}
	if first.Source != catalogueSourceBootstrap || second.Source != catalogueSourceBootstrap {
		t.Fatalf("sources = %q / %q", first.Source, second.Source)
	}
}

// A 200 carrying JSON that is not a usage document is not proof a key works:
// treating it as valid would clear a real quarantine.
func TestFakeUsageDocumentIsNotValid(t *testing.T) {
	for _, body := range []string{
		`{"error":"invalid api key"}`,
		`{"usage":{"rolling":{"status":"ok","percent":5,"resetsAt":"2026-11-18T12:00:00Z"}}}`,
		`{"usage":{"rolling":{"status":"weird","percent":5},"weekly":{"status":"ok","percent":5},"monthly":{"status":"ok","percent":5}}}`,
	} {
		srv := usageServer(t, 200, body)
		if status, _ := probeKey(http.DefaultClient, "k", srv.URL); status != statusUnknown {
			t.Fatalf("body %s classified %q, want unknown", body, status)
		}
	}
}

// The per-account catalogue caches its failures too: the host asks again on every
// auth change and refresh.
func TestPerKeyCatalogueFailureIsCached(t *testing.T) {
	resetCatalogue(t)
	var calls atomic.Int32
	modelKeyPoolFn = func() ([]string, error) { return []string{"oc_sk_shared"}, nil }
	fetchModelsFn = func(_ string, apiKey, _ string) ([]string, error) {
		if apiKey == "oc_sk_account_bad" {
			calls.Add(1)
			return nil, fmt.Errorf("down")
		}
		return []string{"shared"}, nil
	}
	catalogueForKey("https://gw.example/v1", "oc_sk_account_bad", "ua")
	catalogueForKey("https://gw.example/v1", "oc_sk_account_bad", "ua")
	if calls.Load() != 1 {
		t.Fatalf("per-key failure must be cached, calls = %d", calls.Load())
	}
}

// After a base-url change the previous gateway's list must not be passed off as
// the new one's.
func TestFallbackDoesNotCrossGateways(t *testing.T) {
	resetCatalogue(t)
	modelKeyPoolFn = func() ([]string, error) { return []string{"oc_sk_k"}, nil }
	fetchModelsFn = func(baseURL, _ string, _ string) ([]string, error) {
		if strings.Contains(baseURL, "gw-b") {
			return nil, fmt.Errorf("down")
		}
		return []string{"from-a"}, nil
	}
	withBase(t, "https://gw-a.example/v1")
	if cat := currentCatalogue(); strings.Join(cat.IDs, ",") != "from-a" {
		t.Fatalf("first = %v", cat.IDs)
	}
	withBase(t, "https://gw-b.example/v1")
	cat := currentCatalogue()
	for _, id := range cat.IDs {
		if id == "from-a" {
			t.Fatalf("old gateway's model served for the new one: %v", cat.IDs)
		}
	}
}

// A bootstrap list stays labelled bootstrap: the status must not claim it came
// from a gateway.
func TestBootstrapStaysLabelledBootstrap(t *testing.T) {
	resetCatalogue(t)
	modelKeyPoolFn = func() ([]string, error) { return nil, fmt.Errorf("host auth list unavailable") }
	fetchModelsFn = func(string, string, string) ([]string, error) { return nil, fmt.Errorf("no key") }
	first := currentCatalogue()
	if first.Source != catalogueSourceBootstrap {
		t.Fatalf("first source = %q", first.Source)
	}
	catalogueMu.Lock()
	sharedCatalogue.nextTry = time.Time{}
	catalogueMu.Unlock()
	second := currentCatalogue()
	if second.Source != catalogueSourceBootstrap {
		t.Fatalf("second source = %q, want bootstrap", second.Source)
	}
}
