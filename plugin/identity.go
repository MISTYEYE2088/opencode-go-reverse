package main

import (
	"strings"
	"sync"
	"time"
)

// Every scheduler decision needs the API key behind an auth record: quotas are
// read with the key, not with the record name. The host has no permanent
// name→key table — it lists records, and serves one record's credential on
// request — so resolving that mapping on every pick cost two host calls per
// candidate per request, and every extra account made every request slower.
//
// The mapping is therefore kept here: written whenever the host hands us a
// credential (auth.parse, auth.refresh, executor calls), and re-read from the
// host only when an entry has aged out. A key edited on disk reaches us through
// auth.parse, which overwrites the entry, so a stale key never survives the
// event that changed it.
const authKeyTTL = 10 * time.Minute

// authKeyCap bounds the map against a host that keeps inventing records.
const authKeyCap = 4096

type authKeyEntry struct {
	key   string
	stamp time.Time
}

var (
	authKeyMu sync.Mutex
	authKeys  = map[string]authKeyEntry{}
	// authKeyLoader is the host lookup, replaceable so tests can count calls.
	authKeyLoader = hostAuthKey
)

// keyForAuthID returns the credential behind one auth record.
//
// #FUNC     Resolve an auth id to its API key, cached.
// #IN       the auth record id as the host names it.
// #EDGE-IN  an unknown or expired id → one host lookup; if that fails the answer
//
//	is "no key", which the scheduler treats as neutral, never as a veto.
//
// #OUT      the key and whether it is known.
// #WHY      two host calls per candidate per pick made selection cost grow with
//
//	the pool size, for a mapping that only changes when the host tells us.
func keyForAuthID(authID string) (string, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", false
	}
	authKeyMu.Lock()
	entry, ok := authKeys[authID]
	authKeyMu.Unlock()
	if ok && time.Since(entry.stamp) < authKeyTTL {
		return entry.key, true
	}
	key, found := authKeyLoader(authID)
	if !found {
		return "", false
	}
	rememberAuthKey(authID, key)
	return key, true
}

// rememberAuthKey stores a credential the host has just handed us.
//
// #FUNC     Record (or replace) one account's key.
// #WHY      a replaced key must overwrite the old one immediately: the host calls
//
//	auth.parse for exactly this event, so the cache follows the file.
func rememberAuthKey(authID, key string) {
	authID = strings.TrimSpace(authID)
	key = strings.TrimSpace(key)
	if authID == "" || key == "" {
		return
	}
	authKeyMu.Lock()
	defer authKeyMu.Unlock()
	if len(authKeys) >= authKeyCap {
		now := time.Now()
		for id, entry := range authKeys {
			if now.Sub(entry.stamp) >= authKeyTTL {
				delete(authKeys, id)
			}
		}
		if len(authKeys) >= authKeyCap {
			for id := range authKeys {
				delete(authKeys, id)
			}
		}
	}
	authKeys[authID] = authKeyEntry{key: key, stamp: time.Now()}
}

// forgetAuthKey drops one account, so a removed record is never scored again.
func forgetAuthKey(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	authKeyMu.Lock()
	delete(authKeys, authID)
	authKeyMu.Unlock()
}
