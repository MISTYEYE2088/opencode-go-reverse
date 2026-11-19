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

// sched-1-2: a decline still excludes the credentials the plugin proved dead.
func TestDeclineExcludesVetoedCandidates(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "a", Quota: acct(40, 40, 20*24*time.Hour)},
		{AuthID: "b", Quota: acct(40, 40, 20*24*time.Hour)},
		{AuthID: "dead", VetoReason: "invalid-key: unauthorized", Quota: acct(40, 40, 20*24*time.Hour)},
	}, defaultScoreOptions(now))
	if d.Handled {
		t.Fatal("two identical healthy candidates must tie into a decline")
	}
	if len(d.Vetoed) != 1 || d.Vetoed[0] != "dead" {
		t.Fatalf("decline must exclude the vetoed id, got %v", d.Vetoed)
	}
}

// sched-1-10: the audit line carries the scores the decision actually used.
func TestPickRankedShowsVetoOverride(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "healthy", Quota: acct(10, 10, 20*24*time.Hour)},
		{AuthID: "dead", VetoReason: "invalid-key: unauthorized", SameSession: true, Quota: acct(10, 10, 20*24*time.Hour)},
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

func TestPickDeclinesOnTie(t *testing.T) {
	q := acct(40, 40, 20*24*time.Hour)
	d := Pick([]candidate{{AuthID: "a", Quota: q}, {AuthID: "b", Quota: q}}, opts())
	if d.Handled {
		t.Fatalf("tie must defer, got %+v", d)
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
		{AuthID: "dead", SameSession: true, VetoReason: "invalid-key: unauthorized",
			Quota: Quota{Rolling: win(10, 2*time.Hour), Weekly: win(1, 24*time.Hour), Monthly: win(30, 20*24*time.Hour)}},
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
