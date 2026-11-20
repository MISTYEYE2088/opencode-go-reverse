package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The page's write buttons key off FileName, so the status must always carry it.
func TestStatusCarriesFileName(t *testing.T) {
	raw, err := json.Marshal(accountStatus{ID: "go-adapter-1", FileName: "go-adapter-1.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"file_name":"go-adapter-1.json"`) {
		t.Fatalf("file_name must ship in the status, got %s", raw)
	}
}

// CSP must pin the script by hash and allow nothing else: no origins, no
// framing, no base. A page that answers "unsafe-inline" for scripts has no CSP.
func TestPanelCSPPinsScriptHash(t *testing.T) {
	csp := panelCSP()
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'sha256-",
		"connect-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
	if i := strings.Index(csp, "script-src"); !strings.HasPrefix(csp[i:], "script-src 'sha256-") {
		t.Fatalf("script-src must be hash-pinned only: %s", csp)
	}
	// The pinned hash must be the hash of the actual script text.
	sum := sha256Sum(panelScript)
	if !strings.Contains(csp, sum) {
		t.Fatalf("CSP hash does not match panelScript: %s", csp)
	}
}

// The public page must not regress into shipping pool data as it gained the
// write forms: the add form's input must be a password field and must not carry
// the key prefix in markup.
func TestPanelAddFormIsClean(t *testing.T) {
	page := string(renderStatusShell())
	if !strings.Contains(page, `type="password"`) {
		t.Fatal("the add-key input must be a password field")
	}
	if strings.Contains(page, "innerHTML") {
		t.Fatal("the page must build DOM with textContent, never innerHTML")
	}
	for _, banned := range []string{"oc_sk_", "localStorage"} {
		if strings.Contains(page, banned) {
			t.Fatalf("page must not contain %q", banned)
		}
	}
	if strings.Contains(page, "sessionStorage") && !strings.Contains(page, "ga-mgmt") {
		t.Fatal("session storage must use the plugin's own key name only")
	}
}

// pauseJSON: park, query, unpause, and reject a body with no auth_id.
func TestPauseLifecycle(t *testing.T) {
	resetObservations(t)

	body, err := pauseJSON([]byte(`{"auth_id":"go-adapter-1","minutes":15}`))
	if err != nil {
		t.Fatal(err)
	}
	var parked map[string]any
	if errDecode := json.Unmarshal(body, &parked); errDecode != nil {
		t.Fatal(errDecode)
	}
	if parked["paused"] != true {
		t.Fatalf("want paused=true, got %v", parked)
	}
	if !isPaused("go-adapter-1") {
		t.Fatal("the account must be parked")
	}

	// Query with no minutes field: state reported, nothing changes.
	body, err = pauseJSON([]byte(`{"auth_id":"go-adapter-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !isPaused("go-adapter-1") {
		t.Fatal("a query must not unpause")
	}

	body, err = pauseJSON([]byte(`{"auth_id":"go-adapter-1","minutes":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var unparked map[string]any
	_ = json.Unmarshal(body, &unparked)
	if unparked["paused"] != false || isPaused("go-adapter-1") {
		t.Fatalf("minutes=0 must unpark, got %v", unparked)
	}

	if _, err := pauseJSON([]byte(`{"minutes":5}`)); err == nil {
		t.Fatal("an auth_id-less pause must be rejected")
	}
}

// A paused account is excluded from ranking entirely, and only for the pause.
func TestPausedAccountExcludedFromPick(t *testing.T) {
	resetObservations(t)
	defer resetObservations(t)

	pauseAuth("go-adapter-1", 5*time.Minute)
	cands := buildCandidates([]schedCandidate{
		{ID: "go-adapter-1", Provider: providerKey},
		{ID: "go-adapter-2", Provider: providerKey},
	}, "", func(id string) (string, bool) { return "k-" + id, true })

	if len(cands) != 2 {
		t.Fatalf("both candidates must appear, got %d", len(cands))
	}
	found := map[string]string{}
	for _, c := range cands {
		found[c.AuthID] = c.Suspect
	}
	if !strings.Contains(found["go-adapter-1"], "paused") {
		t.Fatalf("the paused account must be marked, got %+v", found)
	}
	if found["go-adapter-2"] != "" {
		t.Fatalf("the live account must be unmarked, got %+v", found)
	}

	// Expired pause self-clears on read.
	pauseAuth("go-adapter-1", -time.Minute)
	if isPaused("go-adapter-1") {
		t.Fatal("an expired pause must clear itself")
	}
}

// The single-key refresh must scope the probe to the named account.
func TestSingleKeyRefreshUnknownIDFails(t *testing.T) {
	if _, err := refreshJSON("no-such-account"); err == nil {
		t.Fatal("an unknown auth id must be an error, not a silent all-refresh")
	}
}

// The pick record must replay what the log says, including the tie-break label.
func TestPickRecordCaptured(t *testing.T) {
	resetObservations(t)
	defer resetObservations(t)

	q := acct(40, 40, 20*24*time.Hour)
	body, err := json.Marshal(schedPickRequest{
		Provider:   providerKey,
		Model:      "gemini-4-argon",
		Candidates: []schedCandidate{{ID: "go-adapter-1", Provider: providerKey}, {ID: "go-adapter-2", Provider: providerKey}},
	})
	if err != nil {
		t.Fatal(err)
	}
	authKeyLoader = func(string) (string, bool) { return "k1", true }
	defer func() { authKeyLoader = hostAuthKey }()
	seedQuota("k1", currentConfig().BaseURL, q)

	if _, err := handleSchedulerPick(body); err != nil {
		t.Fatal(err)
	}
	picks := recentPicks()
	if len(picks) != 1 {
		t.Fatalf("one pick must be recorded, got %d", len(picks))
	}
	p := picks[0]
	if p.Model != "gemini-4-argon" || p.Winner == "" {
		t.Fatalf("record incomplete: %+v", p)
	}
	if p.Decision == "" {
		t.Fatalf("decision label missing: %+v", p)
	}
	if len(p.Scores) != 2 {
		t.Fatalf("both candidate scores must be kept, got %d", len(p.Scores))
	}
}

// Errors are bounded: a flood of failures cannot grow the ring without end.
func TestErrorRingIsBounded(t *testing.T) {
	resetObservations(t)
	defer resetObservations(t)

	for i := 0; i < 60; i++ {
		recordErr("go-adapter-1", "upstream", "http 500")
	}
	if got := len(recentErrs()); got != recentErrCap {
		t.Fatalf("ring must cap at %d, got %d", recentErrCap, got)
	}
}

// burnEstimate needs two samples; one sample must produce no number at all.
func TestBurnEstimateNeedsTwoSamples(t *testing.T) {
	resetBurn(t)
	rate, empty := burnEstimate("go-adapter-1", acct(10, 10, 20*24*time.Hour))
	if rate != nil || empty != "" {
		t.Fatalf("one sample must yield no estimate, got %v %q", rate, empty)
	}

	resetBurn(t)
	// Two samples an hour apart: usage 10% -> 50% means 40%/h burn.
	burnMu.Lock()
	burnSeen["go-adapter-2"] = []burnSample{
		{at: time.Now().Add(-time.Hour), usedFrac: 0.10},
	}
	burnMu.Unlock()
	q2 := acct(50, 10, 20*24*time.Hour)
	rate, empty = burnEstimate("go-adapter-2", q2)
	if rate == nil || *rate <= 0 {
		t.Fatalf("two rising samples must yield a positive rate, got %v", rate)
	}
	if empty == "" {
		t.Fatal("time-to-empty must be estimated once a slope exists")
	}
}

// resetObservations clears plugin-side observation state between tests.
func resetObservations(t *testing.T) {
	t.Helper()
	obsMu.Lock()
	obsPicks = nil
	obsErrs = nil
	pausedAuth = map[string]time.Time{}
	obsMu.Unlock()
	t.Cleanup(func() {
		obsMu.Lock()
		obsPicks = nil
		obsErrs = nil
		pausedAuth = map[string]time.Time{}
		obsMu.Unlock()
	})
}

// resetBurn clears the burn-rate samples for one test.
func resetBurn(t *testing.T) {
	t.Helper()
	burnMu.Lock()
	burnSeen = map[string][]burnSample{}
	burnMu.Unlock()
	t.Cleanup(func() {
		burnMu.Lock()
		burnSeen = map[string][]burnSample{}
		burnMu.Unlock()
	})
}

// sha256Sum mirrors panelCSP's hash so the test can verify the pin.
func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}
