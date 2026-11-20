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
	RejectCode        string   `json:"RejectCode,omitempty"`
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
// #FUNC      Record one session→account binding.
// #EDGE-IN   map at `stickyCap` (10 000) → expired entries swept first, then an
//
//	arbitrary tenth evicted, so a map-flood client cannot wedge the
//	host → TestStickyMapCappedAndSwept
//
// #WHY       `sticky` keys are hashes, never raw session ids: a client-controlled
//
//	header stored verbatim is a memory-growth and log-forgery channel.
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
//	along even when the plugin declines → TestExclusionsDropWhenEveryCandidateIsOut
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

	// Rule 1: a model outside the OpenCode Go catalogue is not ours at all. The
	// host lists a provider for a model only once that provider registered it,
	// so this gate is the model-name check — and it runs before any scoring, so
	// another provider's traffic is never graded with quota data we do not have.
	if !coversGoModels(req) {
		logPick(hash, decision{Reason: "model outside the OpenCode Go catalogue, deferring to host"})
		return json.Marshal(schedPickResponse{Handled: false})
	}

	cands := buildCandidates(req.Candidates, bound, keyForAuthID)

	opts := defaultScoreOptions(time.Now())
	// The upstream free tier ("-free" suffix) draws no paid quota: window
	// vetoes are account-quota protection and must not block it.
	opts.FreeModel = strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.Model)), "-free")
	// Equal scores are decided from the session hash, so one conversation keeps
	// one account instead of rotating between equal-scoring keys.
	opts.TieSeed = hash

	decision := Pick(cands, opts)
	logPick(hash, decision)
	if decision.Handled {
		bindSession(hash, decision.AuthID)
	}
	return json.Marshal(pickResponseFor(decision, cands))
}

// coversGoModels reports whether the host resolved this request to our provider.
//
// #FUNC     Decide whether a pick request is ours to make at all.
// #IN       the pick request's primary provider and its accepted provider list.
// #OUT      true when "go-adapter" is among them.
// #WHY      the host keeps one scheduler plugin for the whole proxy, so without
//
//	this gate the plugin would grade requests for keys it cannot read.
func coversGoModels(req schedPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) {
		return true
	}
	for _, provider := range req.Providers {
		if strings.EqualFold(strings.TrimSpace(provider), providerKey) {
			return true
		}
	}
	return false
}

// pickResponseFor maps a decision onto the reply the host understands.
//
// #FUNC     Turn a pick decision into a scheduler response.
// #IN       the decision and the scored candidates behind it.
// #EDGE-IN  every candidate is a quarantined key of ours → reject with our own
//
//	code and guidance, because a decline would send the request straight
//	back onto a proven-dead key.
//
// #OUT      a handled pick, a rejection, or a decline with exclusions.
// #EDGE-OUT another provider's credential is in the pool → it wins at zero once
//
//	our pool is spent, so the request is served rather than failed.
//
// #WHY      a silent decline is only safe when a healthy credential remains
//
//	somewhere in the host's pool.
func pickResponseFor(d decision, cands []candidate) schedPickResponse {
	if d.Handled {
		return schedPickResponse{AuthID: d.AuthID, Handled: true}
	}
	if allSuspect(cands) {
		return schedPickResponse{
			Handled:    true,
			Reject:     true,
			RejectCode: "auth_unavailable",
			RejectReason: "every go-adapter key is quarantined after a definitive auth rejection; " +
				"revive one with its quota button in the host's auth list, or Refresh all keys on the Go Adapter page",
		}
	}
	// Exclusions are a hint, not a veto over the host's whole pool: excluding
	// every candidate would turn a bad minute into "no auth available" for all
	// requests, so decline cleanly and let the host try.
	return schedPickResponse{Handled: false, ExcludeCandidates: pickExclusions(d, len(cands))}
}

// allSuspect reports whether the pool consists only of quarantined keys. A
// foreign credential is never suspect, so a mixed pool never reaches the reject
// path: it falls through to the other provider instead.
func allSuspect(cands []candidate) bool {
	if len(cands) == 0 {
		return false
	}
	for _, c := range cands {
		if c.Suspect == "" {
			return false
		}
	}
	return true
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
//
// #FUNC     Score the host's candidate list from this plugin's point of view.
// #IN       the host's candidates, the session's bound auth id, and a resolver
//
//	from auth id to our key.
//
// #EDGE-IN  a candidate names a provider that is not ours → it is marked foreign
//
//	and keeps no quota signal at all, so it can only win at zero when
//	our own pool is spent.
//
// #OUT      one candidate per host candidate, ours scored, the rest marked.
// #EDGE-OUT a foreign candidate the session is bound to still earns the
//
//	stickiness bonus (its 1000 is the reason a conversation stays on it).
//
// #WHY      the host offers one scheduler every provider's credentials, and
//
//	scoring a key whose quota cannot be read would invent a comparison.
func buildCandidates(scheds []schedCandidate, bound string, resolve func(string) (string, bool)) []candidate {
	cands := make([]candidate, 0, len(scheds))
	for _, c := range scheds {
		// A named provider settles ownership without asking the host; only a
		// blank provider needs the resolver.
		named := strings.TrimSpace(c.Provider)
		if named != "" && !strings.EqualFold(named, providerKey) {
			cands = append(cands, candidate{
				AuthID:      c.ID,
				Foreign:     true,
				SameSession: bound != "" && bound == c.ID,
			})
			continue
		}
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
		if !keyOK {
			// Ours by name, but the host could not hand us the key: it stays a
			// candidate at the neutral score, without the stickiness bonus,
			// because an unresolvable bound account is exactly the dead one we
			// must not reward.
			cands = append(cands, candidate{AuthID: c.ID})
			continue
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
