package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Sticky session bindings: which account served which conversation. The host
// passes headers, not a session id, so we keep the map ourselves. Keys are
// stored as hashes: bounded size, no control characters, nothing raw in memory.
const (
	stickyTTL = 24 * time.Hour
	// stickyCap bounds the map against clients that send a fresh session header
	// on every request. Sweeping expired entries runs on insert.
	stickyCap = 10000
)

type binding struct {
	authID string
	at     time.Time
}

var (
	stickyMu sync.Mutex
	sticky   = map[string]binding{}
)

type schedCandidate struct {
	ID       string `json:"ID"`
	Provider string `json:"Provider"`
	Status   string `json:"Status"`
}

type schedOptions struct {
	Headers  map[string][]string `json:"Headers"`
	Metadata map[string]any      `json:"Metadata"`
}

type schedPickRequest struct {
	Provider   string           `json:"Provider"`
	Providers  []string         `json:"Providers"`
	Model      string           `json:"Model"`
	Stream     bool             `json:"Stream"`
	Options    schedOptions     `json:"Options"`
	Candidates []schedCandidate `json:"Candidates"`
}

// schedPickResponse mirrors pluginapi.SchedulerPickResponse (Go field names).
type schedPickResponse struct {
	AuthID            string   `json:"AuthID,omitempty"`
	Handled           bool     `json:"Handled"`
	Reject            bool     `json:"Reject,omitempty"`
	RejectReason      string   `json:"RejectReason,omitempty"`
	ExcludeCandidates []string `json:"ExcludeCandidates,omitempty"`
}

// sessionHash is the storage and log form of a session key: fixed size, hex only.
func sessionHash(sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:8])
}

func boundAuth(hash string) string {
	if hash == "" {
		return ""
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	b, ok := sticky[hash]
	if !ok || time.Since(b.at) > stickyTTL {
		if ok {
			delete(sticky, hash)
		}
		return ""
	}
	return b.authID
}

// bindSession records a binding and keeps the map bounded: expired entries are
// swept on every insert, and a full map still admits new sessions by evicting
// a slice of arbitrary old ones.
func bindSession(hash, authID string) {
	if hash == "" || authID == "" {
		return
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	if len(sticky) >= stickyCap {
		now := time.Now()
		for k, b := range sticky {
			if now.Sub(b.at) > stickyTTL {
				delete(sticky, k)
			}
		}
	}
	if len(sticky) >= stickyCap {
		evicted := 0
		for k := range sticky {
			delete(sticky, k)
			evicted++
			if evicted >= stickyCap/10 {
				break
			}
		}
	}
	sticky[hash] = binding{authID: authID, at: time.Now()}
}

// handleSchedulerPick scores every candidate and answers only when the winner is
// clear. A decline still names the credentials that must not serve the request,
// so the host's own selector never rotates onto a dead key.
//
// #FUNC     Answer one scheduler.pick over the candidate pool.
// #EDGE-IN  The account this session is bound to cannot be resolved through the
//
//	host right now → it loses the stickiness bonus, not the whole pick → TestStickyBonusNeedsResolvableKey
//
// #EDGE-OUT A key the gateway rejects vetoes its candidate, and the veto rides
//
//	along even when the plugin declines → TestDeclineExcludesVetoedCandidates
//
// #WHY      `Handled=false` without `ExcludeCandidates` hands a just-proven-dead
//
//	credential back to the host round-robin, which routes real traffic to it.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req schedPickRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			// A malformed pick request must degrade to "not handled", never
			// fail the client request the host was routing.
			declined, _ := json.Marshal(schedPickResponse{Handled: false})
			return declined, nil
		}
	}
	hash := sessionHash(sessionKeyFromHeaders(req.Options.Headers))
	bound := boundAuth(hash)

	cands := buildCandidates(req.Candidates, bound, keyForAuthID)

	opts := defaultScoreOptions(time.Now())
	// The upstream free tier ("-free" suffix) draws no paid quota: window
	// vetoes are account-quota protection and must not block it.
	opts.FreeModel = strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.Model)), "-free")

	decision := Pick(cands, opts)
	logPick(hash, decision)
	if !decision.Handled {
		// Exclusions are a hint, not a veto over the host's whole pool: when
		// every candidate is out, excluding all of them would turn a bad minute
		// into "no auth available" for every request, so decline cleanly and
		// let the host try.
		return json.Marshal(schedPickResponse{Handled: false,
			ExcludeCandidates: pickExclusions(decision, len(cands))})
	}
	bindSession(hash, decision.AuthID)
	return json.Marshal(schedPickResponse{AuthID: decision.AuthID, Handled: true})
}

// pickExclusions decides what to tell the host to avoid.
//
// #FUNC     Exclude the unusable candidates, unless that would exclude them all.
// #EDGE-IN  every candidate unusable → no exclusions at all
// #OUT      ids the host should skip, or nil.
// #WHY      excluding the whole pool turns "all keys look bad" into the host's
//
//	"no auth available" for every request, free models included; declining
//	without the hint lets the host try and report a real upstream error.
func pickExclusions(d decision, total int) []string {
	if len(d.Vetoed) == 0 || len(d.Vetoed) >= total {
		return nil
	}
	return d.Vetoed
}

// buildCandidates turns host candidates into scored candidates. resolve is
// injectable so the stickiness rule can be tested without host callbacks.
func buildCandidates(scheds []schedCandidate, bound string, resolve func(string) (string, bool)) []candidate {
	cands := make([]candidate, 0, len(scheds))
	for _, c := range scheds {
		key, keyOK := resolve(c.ID)
		q := Quota{}
		var cand candidate
		if keyOK && key != "" {
			// Cache-only: the pick must never block on a /usage round-trip.
			quota, qerr := quotaCached(key)
			q = quota
			// Only a definitive rejection marks the candidate dead at -20000.
			// Anything else (unknown, stale, network) leaves it at neutral.
			if qerr != nil {
				var bad invalidKeyError
				var qua quarantinedError
				switch {
				case errors.As(qerr, &qua):
					cand.Suspect = qua.reason
				case errors.As(qerr, &bad):
					cand.Suspect = "invalid: " + bad.reason
				}
			}
		}
		cand.AuthID = c.ID
		cand.Quota = q
		// The stickiness bonus requires a key we could actually resolve: an
		// unresolvable bound account is exactly the dead one we must not reward.
		cand.SameSession = bound != "" && bound == c.ID && keyOK
		cands = append(cands, cand)
	}
	return cands
}

// logPick records the decision in the host log with the exact scores used. The
// session appears only as a hash: the raw value is client-controlled, and a
// raw header in a log line is a forgery channel, not an audit trail.
//
// #FUNC     Write one auditable pick line to the host log.
// #WHY      `session=%s` with the raw header would let any client forge log
//
//	lines and flood the log; the hash keeps the line attributable
//	without repeating attacker bytes.
func logPick(sessionHashKey string, d decision) {
	if sessionHashKey == "" {
		sessionHashKey = "-"
	}
	parts := make([]string, 0, len(d.Ranked))
	for _, s := range d.Ranked {
		// %q on the id: an auth file may be named with a newline, and an
		// unquoted id would let its author forge log records.
		parts = append(parts, fmt.Sprintf("%q=%d", s.AuthID, s.Score))
	}
	quoted := make([]string, 0, len(d.Vetoed))
	for _, id := range d.Vetoed {
		quoted = append(quoted, fmt.Sprintf("%q", id))
	}
	msg := fmt.Sprintf("go-adapter pick: session=%s winner=%q reason=%q scores=[%s] excluded=[%s]",
		sessionHashKey, d.AuthID, d.Reason, strings.Join(parts, " "), strings.Join(quoted, " "))
	_ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   "info",
		"message": msg,
		"fields":  map[string]any{"plugin": providerKey},
	}, nil)
}
