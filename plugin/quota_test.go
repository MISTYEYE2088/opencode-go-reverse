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

// The manual path (the panel's quota button) re-probes and re-admits.
func TestManualQuotaFetchRecoversQuarantinedKey(t *testing.T) {
	var mode atomic.Int32 // 0 = 502, 1 = healthy
	srv := flippableUsageServer(t, &mode)
	setBase(t, srv.URL)

	mode.Store(usageRejected)
	if _, err := quotaFor("auth", "oc_sk_recover"); !isInvalidKeyErr(err) {
		t.Fatalf("setup: 401 must be invalid, got %v", err)
	}

	mode.Store(usageHealthy)
	raw, _ := json.Marshal(quotaFetchRequest{AuthID: "auth", StorageJSON: []byte(`{"api_key":"oc_sk_recover"}`)})
	out, err := handleQuotaFetch(raw)
	if err != nil {
		t.Fatalf("manual refresh must succeed once the gateway answers: %v", err)
	}
	if !strings.Contains(string(out), "5h") {
		t.Fatalf("manual refresh must return buckets, got %s", out)
	}

	// The key is back in the working pool: the routine path now fetches.
	if _, err := quotaFor("auth", "oc_sk_recover"); err != nil {
		t.Fatalf("recovered key must be routable again, got %v", err)
	}
}

// sched-2-1: distinct keys fetching concurrently must not race the cache map.
func TestConcurrentQuotaForMultiKeyRaceFree(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":5,"resetsAt":"2026-11-18T12:02:19.000Z"},"weekly":{"status":"ok","percent":5,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":5,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`))
	}))
	t.Cleanup(srv.Close)
	setBase(t, srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := quotaFor(fmt.Sprintf("auth-%d", i), fmt.Sprintf("k%d", i)); err != nil {
				t.Errorf("quotaFor(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if got := hits.Load(); got != 8 {
		t.Fatalf("expected one fetch per distinct key, got %d", got)
	}
}

// sched-2-2: distinct keys fetch in parallel, nothing serializes.
func TestConcurrentQuotaForDistinctKeysRunInParallel(t *testing.T) {
	var inflightNow, inflightMax atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflightNow.Add(1)
		for {
			max := inflightMax.Load()
			if cur <= max || inflightMax.CompareAndSwap(max, cur) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		inflightNow.Add(-1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":5,"resetsAt":"2026-11-18T12:02:19.000Z"},"weekly":{"status":"ok","percent":5,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":5,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`))
		select {
		case <-release:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	setBase(t, srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = quotaFor(fmt.Sprintf("a%d", i), fmt.Sprintf("k%d", i))
		}(i)
	}
	wg.Wait()
	if inflightMax.Load() < 2 {
		t.Fatalf("distinct keys serialized: max inflight = %d", inflightMax.Load())
	}
}

// round3-5: a dead leader must not wedge the key for the process lifetime.
func TestSingleflightLeaderPanicUnwedges(t *testing.T) {
	old := fetchUsageFn
	fetchUsageFn = func(string, string) (Quota, error) {
		panic("simulated leader failure")
	}
	t.Cleanup(func() { fetchUsageFn = old })

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = recover() }() // the leader's panic is this goroutine's
		_, _ = quotaFor("auth", "panic-key")
	}()
	wg.Wait()

	// The inflight entry must be gone: a new call leads a fresh flight instead
	// of blocking forever on a channel nobody will close.
	done := make(chan struct{})
	go func() {
		defer close(done)
		fetchUsageFn = func(string, string) (Quota, error) { return Quota{}, fmt.Errorf("ok-now") }
		defer func() { fetchUsageFn = old }()
		_, _ = quotaFor("auth", "panic-key")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a dead leader wedged the key: inflight entry was never cleaned")
	}
}

// sched-1-9: a fixed key is not stuck with the old key's cached veto.
func TestQuotaCacheKeysFollowTheKey(t *testing.T) {
	bad := usageServer(t, 401, `{"error":{"type":"AuthError"}}`)
	setBase(t, bad.URL)
	if _, err := quotaFor("go-adapter-1", "oc_sk_old"); !isInvalidKeyErr(err) {
		t.Fatalf("old key must be rejected, got %v", err)
	}
	good := usageServer(t, 200, `{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-11-18T12:02:19.000Z"},"weekly":{"status":"ok","percent":10,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":10,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`)
	setBase(t, good.URL)
	// Same auth id, new key: the cache must not replay the old veto.
	if _, err := quotaFor("go-adapter-1", "oc_sk_new"); err != nil {
		t.Fatalf("new key must not inherit the old key's cached failure: %v", err)
	}
}

// sched-1-8: concurrent misses produce one upstream fetch.
func TestConcurrentQuotaForSingleFlight(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"2026-11-18T12:02:19.000Z"},"weekly":{"status":"ok","percent":1,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":1,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`))
	}))
	t.Cleanup(srv.Close)
	setBase(t, srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = quotaFor("a", "k") }()
	}
	wg.Wait()
	// A real per-key singleflight guarantees exactly one fetch; the old
	// global-mutex version could not promise this (round-2 finding sched-2-2).
	if got := hits.Load(); got != 1 {
		t.Fatalf("single-flight violated: %d upstream fetches", got)
	}
}

// A cold cache must not spawn one goroutine per pick: a burst of picks on one key
// keeps exactly one warm-up in flight.
func TestQuotaWarmupSpawnsOneGoroutine(t *testing.T) {
	block := make(chan struct{})
	var fetches atomic.Int32
	fetchUsageFn = func(string, string) (Quota, error) {
		fetches.Add(1)
		<-block
		return Quota{}, errQuotaUnknown
	}
	t.Cleanup(func() { fetchUsageFn = fetchUsage })

	base := currentConfig().BaseURL
	for i := 0; i < 200; i++ {
		kickQuotaRefresh("oc_sk_burst", base)
	}
	// Give the scheduler a moment to start whatever it is going to start.
	time.Sleep(50 * time.Millisecond)
	if got := fetches.Load(); got != 1 {
		t.Fatalf("warm-up fetches = %d, want exactly 1 for one credential", got)
	}
	warmMu.Lock()
	inflightWarmups := len(warming)
	warmMu.Unlock()
	if inflightWarmups != 1 {
		t.Fatalf("warming entries = %d, want 1", inflightWarmups)
	}
	close(block)
}
