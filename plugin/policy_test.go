package main

import (
	"encoding/json"
	"testing"
	"time"
)

// Rule 1: a model outside the OpenCode Go catalogue is not ours, so the plugin
// does not score it even when one of our own credentials shows up in the pool.
func TestModelOutsideGoRangeIsNotScored(t *testing.T) {
	body, errMarshal := json.Marshal(schedPickRequest{
		Provider:  "deepseek",
		Providers: []string{"deepseek", "gemini"},
		Model:     "some-other-model",
		Candidates: []schedCandidate{
			{ID: "go-adapter-1", Provider: providerKey},
			{ID: "deepseek-1", Provider: "deepseek"},
		},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	out, err := handleSchedulerPick(body)
	if err != nil {
		t.Fatalf("must not error: %v", err)
	}
	var resp schedPickResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if resp.Handled || resp.Reject || len(resp.ExcludeCandidates) != 0 {
		t.Fatalf("a model we do not serve must be left to the host, got %+v", resp)
	}
}

// The gate accepts a mixed route that includes us, which is the only case where
// another provider's credentials may appear next to ours.
func TestCoversGoModelsAcceptsMixedRoute(t *testing.T) {
	if !coversGoModels(schedPickRequest{Provider: "mixed", Providers: []string{"deepseek", providerKey}}) {
		t.Fatal("a mixed route that lists us is ours to schedule")
	}
	if coversGoModels(schedPickRequest{Provider: "deepseek", Providers: []string{"deepseek"}}) {
		t.Fatal("a route without us must not be scheduled here")
	}
	if !coversGoModels(schedPickRequest{Provider: providerKey}) {
		t.Fatal("our own provider is ours")
	}
}

// Rule 2: for a fresh session our model always goes to the subscription while any
// of our keys is usable, no matter how good the other provider looks.
func TestFreshSessionPrefersSubscription(t *testing.T) {
	cands := []candidate{
		{AuthID: "go-adapter-1", Quota: acct(20, 40, 20*24*time.Hour)},
		{AuthID: "deepseek-1", Foreign: true},
	}
	d := Pick(cands, defaultScoreOptions(now))
	if !d.Handled || d.AuthID != "go-adapter-1" {
		t.Fatalf("a usable subscription key must win over a foreign one, got %+v", d)
	}
	for _, s := range d.Ranked {
		if s.AuthID == "deepseek-1" && s.Score != 0 {
			t.Fatalf("the foreign credential must score 0, got %d", s.Score)
		}
	}
}

// A nearly spent key still beats the foreign fallback: "use it or lose it".
func TestNearlySpentKeyStillBeatsForeign(t *testing.T) {
	cands := []candidate{
		{AuthID: "go-adapter-1", Quota: acct(95, 95, 8*time.Hour)},
		{AuthID: "deepseek-1", Foreign: true},
	}
	if d := Pick(cands, defaultScoreOptions(now)); !d.Handled || d.AuthID != "go-adapter-1" {
		t.Fatalf("a live subscription key must be used up first, got %+v", d)
	}
}

// Rule 3: a session already bound to the other provider keeps it — its 1000 beats
// our best possible quota score.
func TestBoundForeignSessionKeepsItsAccount(t *testing.T) {
	cands := []candidate{
		{AuthID: "go-adapter-1", Quota: acct(0, 0, 30*24*time.Hour)},
		{AuthID: "deepseek-1", Foreign: true, SameSession: true},
	}
	d := Pick(cands, defaultScoreOptions(now))
	if !d.Handled || d.AuthID != "deepseek-1" {
		t.Fatalf("a bound account must keep the session, got %+v", d)
	}
}

// Rule 3 across our own pool: the bound account wins even when a sibling has more
// room, which is what keeps prompt cache warm.
func TestBoundSubscriptionAccountBeatsBetterSibling(t *testing.T) {
	cands := []candidate{
		{AuthID: "go-adapter-1", Quota: acct(60, 60, 20*24*time.Hour), SameSession: true},
		{AuthID: "go-adapter-2", Quota: acct(1, 1, 25*24*time.Hour)},
	}
	d := Pick(cands, defaultScoreOptions(now))
	if !d.Handled || d.AuthID != "go-adapter-1" {
		t.Fatalf("the bound account must keep the session, got %+v", d)
	}
}

// A bound account whose key died is not protected by stickiness: the bonus cannot
// lift it above zero, so the session moves.
func TestBoundQuarantinedAccountLosesToFallback(t *testing.T) {
	cands := []candidate{
		{AuthID: "go-adapter-1", Suspect: "invalid: unauthorized", SameSession: true},
		{AuthID: "deepseek-1", Foreign: true},
	}
	d := Pick(cands, defaultScoreOptions(now))
	if !d.Handled || d.AuthID != "deepseek-1" {
		t.Fatalf("a dead bound key must lose the session, got %+v", d)
	}
}
