package main

import (
	"testing"
	"time"
)

// The mapping from an auth record to its key costs two host calls, so it is
// resolved once and reused: a pool of N accounts must not make every request
// pay N lookups.
func TestKeyForAuthIDResolvesOncePerEntry(t *testing.T) {
	oldLoader := authKeyLoader
	defer func() { authKeyLoader = oldLoader }()
	calls := 0
	authKeyLoader = func(id string) (string, bool) {
		calls++
		return "oc_sk_cached", id == "seat-1"
	}
	forgetAuthKey("seat-1")

	for i := 0; i < 5; i++ {
		key, ok := keyForAuthID("seat-1")
		if !ok || key != "oc_sk_cached" {
			t.Fatalf("lookup %d: got %q %v", i, key, ok)
		}
	}
	if calls != 1 {
		t.Fatalf("the host must be asked once, not %d times", calls)
	}
}

// A key the host hands us (parse, refresh, execution) overwrites the cached one
// immediately: the host calls auth.parse for exactly the event that changed it.
func TestRememberAuthKeyOverwritesWithoutHostLookup(t *testing.T) {
	oldLoader := authKeyLoader
	defer func() { authKeyLoader = oldLoader }()
	authKeyLoader = func(string) (string, bool) {
		t.Fatal("a known mapping must not consult the host")
		return "", false
	}
	forgetAuthKey("seat-1")
	rememberAuthKey("seat-1", "oc_sk_first")
	rememberAuthKey("seat-1", "oc_sk_second")

	key, ok := keyForAuthID("seat-1")
	if !ok || key != "oc_sk_second" {
		t.Fatalf("the newest credential must win, got %q", key)
	}
}

// An entry that aged out is re-read, so a key edited on disk cannot be used
// forever just because it was cached once.
func TestAgedAuthKeyIsReread(t *testing.T) {
	oldLoader := authKeyLoader
	defer func() { authKeyLoader = oldLoader }()
	calls := 0
	authKeyLoader = func(string) (string, bool) {
		calls++
		return "oc_sk_fresh", true
	}
	forgetAuthKey("seat-1")
	rememberAuthKey("seat-1", "oc_sk_stale")

	authKeyMu.Lock()
	entry := authKeys["seat-1"]
	entry.stamp = time.Now().Add(-authKeyTTL - time.Second)
	authKeys["seat-1"] = entry
	authKeyMu.Unlock()

	key, ok := keyForAuthID("seat-1")
	if !ok || key != "oc_sk_fresh" {
		t.Fatalf("an aged entry must be re-read, got %q", key)
	}
	if calls != 1 {
		t.Fatalf("want one re-read, got %d", calls)
	}
}

// A deleted record must not be scored from memory.
func TestForgetAuthKeyDropsTheEntry(t *testing.T) {
	oldLoader := authKeyLoader
	defer func() { authKeyLoader = oldLoader }()
	authKeyLoader = func(string) (string, bool) { return "", false }
	rememberAuthKey("seat-gone", "oc_sk_removed")
	forgetAuthKey("seat-gone")
	if _, ok := keyForAuthID("seat-gone"); ok {
		t.Fatal("a forgotten account must not resolve")
	}
}

// The validation call already returns the windows; keeping them is what makes the
// first scheduler decision score the account instead of guessing 450.
func TestSeedQuotaScoresFirstPick(t *testing.T) {
	withBase(t, "https://gw.example/v1")
	key := "oc_sk_seed_test"
	snapshot := acct(10, 10, 20*24*time.Hour)
	seedQuota(key, "https://gw.example/v1", snapshot)

	got, err := quotaCached(key)
	if err != nil {
		t.Fatalf("a seeded snapshot must be readable: %v", err)
	}
	if !got.Known() {
		t.Fatal("the seeded snapshot must be complete")
	}
	if s := score(t, got, false); s <= 450 {
		t.Fatalf("a healthy seeded account must score above neutral, got %d", s)
	}
}

// A failed probe carries no windows; storing that as a reading would turn a
// network blip into a fake quota.
func TestSeedQuotaIgnoresEmptySnapshot(t *testing.T) {
	withBase(t, "https://gw.example/v1")
	key := "oc_sk_seed_empty"
	seedQuota(key, "https://gw.example/v1", Quota{})
	if _, err := quotaCached(key); err == nil {
		t.Fatal("an empty snapshot must not be cached as data")
	}
}
