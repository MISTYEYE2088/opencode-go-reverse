package main

import (
	"sync"
	"time"
)

// Quarantine: a key the gateway has definitively rejected (401, expired, or an
// account without the Go plan) is parked at -20000, its routine checks stop, and
// a manual quota check — the per-key button or the pool-wide refresh — is what
// brings it back. A key that a manual check proves healthy rejoins immediately.
//
// Everything that is not a definitive rejection — 5xx, 429, a timeout, a WAF page,
// a body that is not a usage document — is treated as the network, not the key:
// it fails open at a neutral score and leaves the standing of the key alone.
//
// The state lives HERE, in plugin-side scheduler state, never in auth.Disabled:
// the host's merge semantics force Disabled back on once set, so a quarantine
// written there could not be cleared by the manual check. Keeping it
// scheduler-side is what makes "manual check → valid → back in the pool" work
// with no fight against the host.

// suspectPenalty is the score for a quarantined candidate. It is below vetoPenalty
// so a dead key can never win, sticky session or not.
const suspectPenalty = -20000

// quarantinedError is returned by the routine path for keys already flagged:
// no fetch happens, so no upstream call is spent on a dead key.
type quarantinedError struct{ reason string }

func (e quarantinedError) Error() string { return "quarantined key: " + e.reason }

type quarantineEntry struct {
	reason string
	at     time.Time
}

var (
	quarantineMu sync.Mutex
	quarantine   = map[string]quarantineEntry{}
)

// quarantined reports whether a cache key is currently flagged.
func quarantined(cacheKey string) (string, bool) {
	quarantineMu.Lock()
	defer quarantineMu.Unlock()
	entry, ok := quarantine[cacheKey]
	if !ok {
		return "", false
	}
	return entry.reason, true
}

// quarantineSet flags a key. Routine checks skip it until a manual check clears it.
func quarantineSet(cacheKey, reason string) {
	quarantineMu.Lock()
	quarantine[cacheKey] = quarantineEntry{reason: reason, at: time.Now()}
	quarantineMu.Unlock()
}

// quarantineClear re-admits a key after a successful manual probe.
func quarantineClear(cacheKey string) {
	quarantineMu.Lock()
	delete(quarantine, cacheKey)
	quarantineMu.Unlock()
}

// countQuarantined is used by the status page and tests.
func countQuarantined() int {
	quarantineMu.Lock()
	defer quarantineMu.Unlock()
	return len(quarantine)
}
