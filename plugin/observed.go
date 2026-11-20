package main

import (
	"strings"
	"sync"
	"time"
)

// What the panel exists to answer: "why did it pick that account?" and "what
// actually went wrong?". Both answers are observations, so they are recorded
// where the decisions and failures happen and replayed here.
//
// Bounded rings, never maps that grow: a page that grows with traffic is a leak.
const (
	// recentPickCap is the number of remembered pick decisions. Enough to
	// explain "the last few minutes", small enough that nobody needs to page.
	recentPickCap = 20
	// recentErrCap bounds remembered upstream/probe failures.
	recentErrCap = 20
)

// pickRecord is one scheduling decision as it actually happened, so the panel
// can replay "session → account, and why" without grepping the host log.
type pickRecord struct {
	Time     string `json:"time"`
	Session  string `json:"session"`
	Model    string `json:"model,omitempty"`
	Foreign  bool   `json:"foreign_pool"`
	Winner   string `json:"winner"`
	Reason   string `json:"reason"`
	Decision string `json:"decision"`
	// Scores carries every candidate as id=score so the ranking is auditable.
	Scores []scoreEntry `json:"scores,omitempty"`
}

// errRecord is one upstream or probe failure, scrubbed, with the account it
// belonged to. key_status "disabled" with no reason is the bug this exists for.
type errRecord struct {
	Time   string `json:"time"`
	AuthID string `json:"auth_id,omitempty"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

var (
	obsMu      sync.Mutex
	obsPicks   []pickRecord
	obsErrs    []errRecord
	pausedAuth = map[string]time.Time{} // auth id → until
)

// recordPick appends one decision to the ring.
//
// #FUNC     Remember the last scheduling decision for the panel.
// #WHY      the panel answers "why this account" from state, not from the host
//
//	log — log lines are a forensics channel, not a queryable one.
func recordPick(rec pickRecord) {
	obsMu.Lock()
	defer obsMu.Unlock()
	obsPicks = append(obsPicks, rec)
	if len(obsPicks) > recentPickCap {
		obsPicks = obsPicks[len(obsPicks)-recentPickCap:]
	}
}

// recordErr appends one scrubbed failure to the ring.
//
// #FUNC     Remember the last upstream failure for the panel.
// #EDGE-IN  detail arrives pre-scrubbed; keys are masked before this call.
// #WHY      a key shown "disabled" with no visible reason is undebuggable, and
//
//	the raw error text lives only in the log nobody opens.
func recordErr(authID, kind, detail string) {
	obsMu.Lock()
	defer obsMu.Unlock()
	obsErrs = append(obsErrs, errRecord{
		Time: time.Now().UTC().Format(time.RFC3339), AuthID: strings.TrimSpace(authID),
		Kind: strings.TrimSpace(kind), Detail: strings.TrimSpace(detail),
	})
	if len(obsErrs) > recentErrCap {
		obsErrs = obsErrs[len(obsErrs)-recentErrCap:]
	}
}

// recentPicks returns a copy of the ring, newest last.
func recentPicks() []pickRecord {
	obsMu.Lock()
	defer obsMu.Unlock()
	out := make([]pickRecord, len(obsPicks))
	copy(out, obsPicks)
	return out
}

// recentErrs returns a copy of the ring, newest last.
func recentErrs() []errRecord {
	obsMu.Lock()
	defer obsMu.Unlock()
	out := make([]errRecord, len(obsErrs))
	copy(out, obsErrs)
	return out
}

// pauseAuth parks an account out of scheduling for a bounded time, in plugin
// state only. It is the soft lever between "do nothing" and "disable the auth
// record": no host state is touched, so it cannot fight the host's merge and it
// expires on its own.
//
// #FUNC     Park one account out of scheduling for a while.
// #IN       the auth id and a duration (0 unpauses immediately).
// #EDGE-IN  an unknown id → still accepted; a pause on a vanished record is a
//
//	no-op that expires
//
// #OUT      the effective until-time (zero when unpaused).
// #WHY      `pausedAuth` lives scheduler-side because the host's `Disabled`
//
//	write is sticky for plugin auths — the file watcher re-parses and
//	re-applies the plugin's own verdict, so a "temporary" disable via
//	the host becomes permanent in practice.
func pauseAuth(authID string, d time.Duration) time.Time {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return time.Time{}
	}
	obsMu.Lock()
	defer obsMu.Unlock()
	if d <= 0 {
		delete(pausedAuth, authID)
		return time.Time{}
	}
	until := time.Now().Add(d)
	pausedAuth[authID] = until
	return until
}

// pauseRemaining reports how long an account stays parked (0 when active).
func pauseRemaining(authID string) time.Duration {
	obsMu.Lock()
	defer obsMu.Unlock()
	until, ok := pausedAuth[authID]
	if !ok {
		return 0
	}
	left := time.Until(until)
	if left <= 0 {
		delete(pausedAuth, authID)
		return 0
	}
	return left
}

// pausedSnapshot lists parked accounts with their remaining time.
func pausedSnapshot() map[string]time.Duration {
	obsMu.Lock()
	defer obsMu.Unlock()
	out := make(map[string]time.Duration, len(pausedAuth))
	now := time.Now()
	for id, until := range pausedAuth {
		if left := until.Sub(now); left > 0 {
			out[id] = left
		}
	}
	return out
}

// sweepPausesLocked drops expired pauses; called under obsMu by pick paths.
func sweepPausesLocked(now time.Time) {
	for id, until := range pausedAuth {
		if now.After(until) {
			delete(pausedAuth, id)
		}
	}
}

// isPaused reports whether an account is parked right now.
func isPaused(authID string) bool {
	obsMu.Lock()
	defer obsMu.Unlock()
	until, ok := pausedAuth[authID]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(pausedAuth, authID)
		return false
	}
	return true
}
