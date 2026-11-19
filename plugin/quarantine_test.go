package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A definitively rejected key is flagged; the next routine call spends no
// upstream hit on it.
func TestInvalidQuarantineSkipsRoutineChecks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(401)
	}))
	t.Cleanup(srv.Close)
	setBase(t, srv.URL)

	if _, err := quotaFor("auth", "oc_sk_doomed"); !isInvalidKeyErr(err) {
		t.Fatalf("401 must be invalid, got %v", err)
	}
	before := hits.Load()
	if _, err := quotaFor("auth", "oc_sk_doomed"); !isQuarantinedErr(err) {
		t.Fatalf("routine call must hit the quarantine skip, got %v", err)
	}
	if hits.Load() != before {
		t.Fatalf("routine check spent an upstream call on a quarantined key")
	}
}

// The user's number: a quarantined candidate scores -20000 and can never win,
// not even with the session bonus.
func TestSuspectScoresMinusTwentyThousand(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "healthy", Quota: acct(80, 80, 20*24*time.Hour)}, // poor but alive
		{AuthID: "suspect", SameSession: true, Suspect: "usage-502"},
	}, defaultScoreOptions(now))
	if !d.Handled || d.AuthID != "healthy" {
		t.Fatalf("suspect must lose even with stickiness, got %+v", d)
	}
	for _, s := range d.Ranked {
		if s.AuthID == "suspect" && s.Score != suspectPenalty {
			t.Fatalf("suspect must score %d, got %d", suspectPenalty, s.Score)
		}
	}
	// Exclusions ride along on DECLINES (the host still has to pick someone);
	// on a handled decision the winner is already ours, so Vetoed is empty by design.
	tie := Pick([]candidate{
		{AuthID: "a", Quota: acct(40, 40, 20*24*time.Hour)},
		{AuthID: "b", Quota: acct(40, 40, 20*24*time.Hour)},
		{AuthID: "suspect", Suspect: "usage-502"},
	}, defaultScoreOptions(now))
	if tie.Handled {
		t.Fatal("two identical healthy candidates must decline")
	}
	if len(tie.Vetoed) != 1 || tie.Vetoed[0] != "suspect" {
		t.Fatalf("decline must exclude the suspect id, got %v", tie.Vetoed)
	}
}

// The status document is the machine surface and it must carry the -20000 and the
// reason, so the page and the pick log never disagree.
func TestStatusJSONReportsQuarantine(t *testing.T) {
	pool := poolStatus{
		Version: "test", GeneratedAt: "now",
		Accounts: []accountStatus{
			{ID: "good", Label: "seat-1", Badge: "VALID", KeyStatus: "valid", Score: 701,
				Windows: &accountWindows{Rolling: 97, Weekly: 29, Monthly: 7}},
			{ID: "bad", Label: "seat-2", Badge: "QUARANTINED",
				KeyStatus: "quarantined", Quarantine: "invalid: unauthorized", Score: suspectPenalty},
		},
	}
	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{"VALID", "QUARANTINED", "-20000", "invalid: unauthorized", "701"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("status json must show %q", want)
		}
	}
}

// The PUBLIC resource page must carry no pool data at all: the host does not
// authenticate resource routes, so anything pre-rendered is world-readable.
func TestStatusPageServesNoData(t *testing.T) {
	page := string(renderStatusShell())
	// Markers that only a rendered pool can contain: a score, a key, a label.
	for _, secretish := range []string{"-20000", "oc_sk_", "quarantined: invalid"} {
		if strings.Contains(page, secretish) {
			t.Fatalf("public page must not contain %q", secretish)
		}
	}
	// It must still be usable: it asks for the key and calls the authenticated route.
	for _, want := range []string{"management key", "/v0/management/plugins/go-adapter/status", "/refresh"} {
		if !strings.Contains(page, want) {
			t.Fatalf("public page must mention %q", want)
		}
	}
	// Rendering is textContent-based, never innerHTML.
	if strings.Contains(page, "innerHTML") {
		t.Fatal("page must not build DOM from HTML strings")
	}
}

// Free models are exempt from window vetoes: a month at 97% used must not
// block a model that draws no paid quota. The dead-key quarantine is not
// exempt (a bad key serves nothing).
func TestFreeModelSkipsWindowVetoes(t *testing.T) {
	dry := acct(10, 97, 20*24*time.Hour) // monthly 97% used, over the 95% line
	paid := defaultScoreOptions(now)
	paid.VetoMonthlyPercent = 95
	if _, why := Score(dry, false, paid); why == "" {
		t.Fatal("paid model must be vetoed when the month is over the line")
	}
	free := paid
	free.FreeModel = true
	if _, why := Score(dry, false, free); why != "" {
		t.Fatalf("free model must not be window-vetoed, got %q", why)
	}
}

// A 502/429/timeout is the network, not the key: it must NOT quarantine, and the
// next routine call must still be allowed to probe.
func TestTransientProbeDoesNotQuarantine(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(502)
	}))
	t.Cleanup(srv.Close)
	setBase(t, srv.URL)

	if _, err := quotaFor("auth", "oc_sk_blip"); !isTransientErr(err) {
		t.Fatalf("502 must be transient, got %v", err)
	}
	if _, bad := quarantined(quotaCacheKey("oc_sk_blip", currentConfig().BaseURL)); bad {
		t.Fatal("a 502 must not quarantine the key")
	}
	quotaMu.Lock()
	delete(quotaCache, quotaCacheKey("oc_sk_blip", currentConfig().BaseURL))
	quotaMu.Unlock()
	if _, err := quotaFor("auth", "oc_sk_blip"); !isTransientErr(err) {
		t.Fatalf("routine path must keep probing, got %v", err)
	}
	if hits.Load() < 2 {
		t.Fatalf("routine checks must continue after a blip, hits=%d", hits.Load())
	}
}

// When every candidate is out, the plugin declines without excluding the whole
// pool: excluding all of them would make the host answer "no auth available".
func TestExclusionsDropWhenEveryCandidateIsOut(t *testing.T) {
	d := Pick([]candidate{
		{AuthID: "a", Suspect: "invalid: unauthorized"},
		{AuthID: "b", Suspect: "invalid: unauthorized"},
	}, defaultScoreOptions(now))
	if d.Handled {
		t.Fatal("all-dead pool must decline")
	}
	if got := pickExclusions(d, 2); got != nil {
		t.Fatalf("exclusions must be dropped when all are out, got %v", got)
	}
	// With a healthy candidate left, the bad ones are still named.
	d2 := Pick([]candidate{
		{AuthID: "a", Quota: acct(10, 10, 20*24*time.Hour)},
		{AuthID: "b", Suspect: "invalid: unauthorized"},
	}, defaultScoreOptions(now))
	if !d2.Handled {
		t.Fatalf("one healthy candidate must be handled: %+v", d2)
	}
}
