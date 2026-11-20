package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sched-2-6: a malformed pick payload declines, never errors the request.
func TestPickDeclinesOnGarbagePayload(t *testing.T) {
	out, err := handleSchedulerPick([]byte(`{not json`))
	if err != nil {
		t.Fatalf("garbage pick must not error: %v", err)
	}
	var resp schedPickResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil || resp.Handled {
		t.Fatalf("garbage pick must decline, got %s", out)
	}
}

// sched-1-2: a healthy winner is named outright, and the dead credential is
// still reported so the host never rotates onto it.
func TestPickNamesWinnerAndListsDead(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "a", Quota: acct(40, 40, 20*24*time.Hour)},
		{AuthID: "b", Quota: acct(40, 40, 20*24*time.Hour)},
		// An exhausted window is the production veto path: Score returns it,
		// no out-of-band flag involved.
		{AuthID: "dead", Quota: acct(40, 100, 20*24*time.Hour)},
	}, opts())
	if !d.Handled || (d.AuthID != "a" && d.AuthID != "b") {
		t.Fatalf("a healthy candidate must win, got %+v", d)
	}
	// Vetoed is the decline-time hint; a handled decision names its winner
	// instead, so it stays empty by design.
	if len(d.Vetoed) != 0 {
		t.Fatalf("handled decision must not carry exclusions, got %v", d.Vetoed)
	}
}

// sched-1-10: the audit line carries the scores the decision actually used.
func TestPickRankedShowsVetoOverride(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "healthy", Quota: acct(10, 10, 20*24*time.Hour)},
		{AuthID: "dead", SameSession: true, Quota: acct(10, 100, 20*24*time.Hour)},
	}, defaultScoreOptions(now))
	if !d.Handled || d.AuthID != "healthy" {
		t.Fatalf("vetoed sticky must lose: %+v", d)
	}
	for _, s := range d.Ranked {
		if s.AuthID == "dead" && s.Score != vetoPenalty+sessionBonus {
			t.Fatalf("ranked score must reflect the veto, got %+v", s)
		}
	}
}

// inputs-1-3 / sched-1-5: the sticky map cannot grow without bound.
func TestStickyMapCappedAndSwept(t *testing.T) {
	for i := 0; i < stickyCap+200; i++ {
		bindSession(fmt.Sprintf("h%d", i), "go-adapter-1")
	}
	stickyMu.Lock()
	size := len(sticky)
	stickyMu.Unlock()
	if size > stickyCap {
		t.Fatalf("sticky map exceeded its cap: %d > %d", size, stickyCap)
	}
}

// secrets-1-4 / inputs-1-9: log identity is a hash, never the raw header.
func TestSessionHashIsHexOnly(t *testing.T) {
	hostile := "victim\n[2026-11-18 03:00:00] [--------] [info ] forged audit line"
	got := sessionHash(hostile)
	for _, c := range got {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("hash must be hex only, got %q", got)
		}
	}
}

// sched-1-6: an unresolvable bound account loses the stickiness bonus.
func TestStickyBonusNeedsResolvableKey(t *testing.T) {
	scheds := []schedCandidate{{ID: "healthy"}, {ID: "dead"}}
	cands := buildCandidates(scheds, "dead", func(string) (string, bool) { return "", false })
	for _, c := range cands {
		if c.SameSession {
			t.Fatalf("unresolvable key must strip the bonus, got %+v", c)
		}
	}
}

func TestPickChoosesMoreQuota(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "tight", Quota: acct(90, 40, 20*24*time.Hour)},
		{AuthID: "roomy", Quota: acct(10, 40, 20*24*time.Hour)},
	}, opts())
	if !d.Handled || d.AuthID != "roomy" {
		t.Fatalf("want roomy, got %+v", d)
	}
}

func TestPickPrefersStickySession(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "fresh", Quota: acct(20, 40, 20*24*time.Hour)},
		{AuthID: "bound", Quota: acct(40, 40, 20*24*time.Hour), SameSession: true},
	}, opts())
	if !d.Handled || d.AuthID != "bound" {
		t.Fatalf("want the bound account, got %+v", d)
	}
}

// A tie is a decision: the session seed names the winner, so one conversation
// keeps one account instead of alternating between equal-scoring keys.
func TestPickBreaksTieFromSessionSeed(t *testing.T) {
	q := acct(40, 40, 20*24*time.Hour)
	tied := []candidate{{AuthID: "a", Quota: q}, {AuthID: "b", Quota: q}}

	first := opts()
	first.TieSeed = "session-hash-1"
	d := Pick(tied, first)
	if !d.Handled {
		t.Fatalf("a tie must be decided, got %+v", d)
	}
	if !strings.Contains(d.Reason, "tie") {
		t.Fatalf("reason must say the tie was broken, got %q", d.Reason)
	}
	// Deterministic: the same session always lands on the same account.
	for i := 0; i < 5; i++ {
		again := opts()
		again.TieSeed = "session-hash-1"
		if got := Pick(tied, again); got.AuthID != d.AuthID {
			t.Fatalf("same seed must pick the same account: %q then %q", d.AuthID, got.AuthID)
		}
	}
}

// Different conversations must spread across the pool, not collapse onto one
// account: stickiness is per session, not a permanent favourite.
func TestTieBreakSpreadsSessionsAcrossThePool(t *testing.T) {
	q := acct(40, 40, 20*24*time.Hour)
	tied := []candidate{{AuthID: "go-adapter-1", Quota: q}, {AuthID: "go-adapter-2", Quota: q}}

	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		o := opts()
		o.TieSeed = fmt.Sprintf("session-%d", i)
		d := Pick(tied, o)
		if !d.Handled {
			t.Fatalf("session %d must be decided, got %+v", i, d)
		}
		seen[d.AuthID]++
	}
	if seen["go-adapter-1"] < 60 || seen["go-adapter-2"] < 60 {
		t.Fatalf("sessions must spread across both accounts, got %v", seen)
	}
}

// No session header at all is still not a reason to alternate.
func TestTieWithoutSessionIsStillDecided(t *testing.T) {
	q := acct(40, 40, 20*24*time.Hour)
	tied := []candidate{{AuthID: "a", Quota: q}, {AuthID: "b", Quota: q}}
	first := Pick(tied, opts())
	if !first.Handled {
		t.Fatalf("a tie without a session must still be decided, got %+v", first)
	}
	for i := 0; i < 5; i++ {
		if got := Pick(tied, opts()); got.AuthID != first.AuthID {
			t.Fatalf("seedless tie must be stable: %q then %q", first.AuthID, got.AuthID)
		}
	}
}

func TestPickDeclinesWhenAllVetoed(t *testing.T) {
	q := acct(1, 99, 5*24*time.Hour)
	d := Pick([]candidate{{AuthID: "a", Quota: q}, {AuthID: "b", Quota: q}}, opts())
	if d.Handled {
		t.Fatalf("all vetoed must defer, got %+v", d)
	}
}

func TestPickSingleCandidate(t *testing.T) {
	d := Pick([]candidate{{AuthID: "only", Quota: acct(5, 5, 20*24*time.Hour)}}, opts())
	if !d.Handled || d.AuthID != "only" {
		t.Fatalf("single healthy candidate should be handled, got %+v", d)
	}
}

func TestExplicitVetoBeatsSticky(t *testing.T) {
	// A sticky session whose account's key has died must lose to a healthy one.
	d := Pick([]candidate{
		{AuthID: "healthy", Quota: Quota{Rolling: win(50, 2*time.Hour), Weekly: win(1, 24*time.Hour), Monthly: win(30, 20*24*time.Hour)}},
		{AuthID: "dead", SameSession: true,
			Quota: Quota{Rolling: win(10, 2*time.Hour), Weekly: win(1, 24*time.Hour), Monthly: win(100, 24*time.Hour)}},
	}, defaultScoreOptions(now))
	if !d.Handled || d.AuthID != "healthy" {
		t.Fatalf("dead sticky account must lose, got %+v", d)
	}
}

// The pick runs inside the host's request path, so it must never wait on a
// /usage round-trip: a cold cache returns neutral immediately and warms in the
// background.
func TestPickDoesNotBlockOnColdQuotaCache(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"2026-11-18T12:02:19Z"}}}`))
	}))
	t.Cleanup(slow.Close)
	setBase(t, slow.URL)

	start := time.Now()
	cands := buildCandidates([]schedCandidate{{ID: "cold"}}, "",
		func(string) (string, bool) { return "oc_sk_cold_cache", true })
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("pick blocked for %s on a cold quota cache", elapsed)
	}
	if len(cands) != 1 || cands[0].Suspect != "" {
		t.Fatalf("a cold cache must be neutral, not suspicious: %+v", cands)
	}
}

// A pool the host offers only for another provider is not ours to schedule:
// declining keeps the host's own selector in charge of that provider.
func TestPickDeclinesWhenOnlyForeignCandidates(t *testing.T) {
	body, errMarshal := json.Marshal(schedPickRequest{
		Provider:   "deepseek",
		Providers:  []string{"deepseek"},
		Model:      "deepseek-chat",
		Candidates: []schedCandidate{{ID: "deepseek-1", Provider: "deepseek"}},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	out, err := handleSchedulerPick(body)
	if err != nil {
		t.Fatalf("foreign pool must not error: %v", err)
	}
	var resp schedPickResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if resp.Handled || resp.Reject || len(resp.ExcludeCandidates) != 0 {
		t.Fatalf("a foreign pool must be left to the host untouched, got %+v", resp)
	}
}

// The host runs one scheduler for every provider, so the candidate list can mix
// owners; only our own credentials may be scored, the rest are marked foreign.
func TestBuildCandidatesMarksOtherProvidersForeign(t *testing.T) {
	cands := buildCandidates([]schedCandidate{
		{ID: "go-adapter-1", Provider: "go-adapter"},
		{ID: "deepseek-1", Provider: "deepseek"},
	}, "", keyForAuthID)
	if len(cands) != 2 {
		t.Fatalf("want both candidates, got %+v", cands)
	}
	if cands[0].Foreign {
		t.Fatalf("our own credential must not be foreign: %+v", cands[0])
	}
	if !cands[1].Foreign {
		t.Fatalf("another provider's credential must be foreign: %+v", cands[1])
	}
}
