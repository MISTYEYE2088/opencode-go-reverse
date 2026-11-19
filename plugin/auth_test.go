package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// Refresh must not spend an upstream call on a quarantined key.
func TestRefreshSkipsQuarantinedProbes(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(401)
	}))
	t.Cleanup(srv.Close)
	setBase(t, srv.URL)

	if _, err := quotaFor("auth", "oc_sk_flagged"); !isInvalidKeyErr(err) {
		t.Fatalf("setup failed: %v", err)
	}
	old := authStateFn
	authStateFn = func(string) authState { return authState{Exists: true, Key: "oc_sk_flagged"} }
	t.Cleanup(func() { authStateFn = old })

	raw, _ := json.Marshal(authRefreshRequest{AuthID: "auth", StorageJSON: []byte(`{"api_key":"oc_sk_flagged"}`)})
	out, err := handleAuthRefresh(raw)
	if err != nil {
		t.Fatalf("refresh errored: %v", err)
	}
	var resp authRefreshResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if resp.Auth.Metadata["key_status"] != "quarantined" {
		t.Fatalf("expected quarantined status, got %v", resp.Auth.Metadata)
	}
	if hits.Load() != 1 {
		t.Fatalf("refresh must not re-probe a quarantined key, hits=%d", hits.Load())
	}
}

// The constraint-2 compatibility pin: refresh preserves the host Disabled bit
// even when the probe turns valid — the recovery is scheduler-side only.
func TestRefreshPreservesDisabledState(t *testing.T) {
	srv := usageServer(t, 200, `{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-11-18T12:02:19.000Z"},"weekly":{"status":"ok","percent":10,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":10,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`)
	setBase(t, srv.URL)

	old := authStateFn
	authStateFn = func(string) authState { return authState{Exists: true, Disabled: true, Key: "k"} }
	t.Cleanup(func() { authStateFn = old })

	raw, _ := json.Marshal(authRefreshRequest{AuthID: "auth", StorageJSON: []byte(`{"api_key":"k"}`)})
	out, err := handleAuthRefresh(raw)
	if err != nil {
		t.Fatalf("refresh errored: %v", err)
	}
	var resp authRefreshResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if !resp.Auth.Disabled {
		t.Fatal("refresh must preserve the host Disabled bit (sched-2-3)")
	}
	if resp.Auth.Metadata["key_status"] != statusValid {
		t.Fatalf("probe verdict must still be reported, got %v", resp.Auth.Metadata)
	}
}

// Visual identification: a quarantined key must be unmistakable on every
// surface — the host panel note, the badge, and the -20000 itself.
func TestVisibleNoteCarriesQuarantine(t *testing.T) {
	note := visibleNote(statusInvalid, "unauthorized")
	if !strings.Contains(note, "QUARANTINED") || !strings.Contains(note, "unauthorized") {
		t.Fatalf("note must name the state and reason, got %q", note)
	}
	if !strings.Contains(note, "refresh") {
		t.Fatalf("note must tell the user how to recover, got %q", note)
	}
	if visibleNote(statusValid, "") != "" {
		t.Fatal("valid keys must stay silent in the host panel")
	}
}

// secrets-2-5: the label tail never contains a cut rune.
func TestLabelTailRuneSafe(t *testing.T) {
	key := "oc_sk_" + strings.Repeat("x", 30) + "€€"
	label := labelFor(credential{APIKey: key})
	if !utf8.ValidString(label) {
		t.Fatalf("label contains invalid UTF-8: %q", label)
	}
}

// round3-6: invalid-UTF-8 secrets drop the tail instead of widening it.
func TestLabelWalkClampsOnInvalidUTF8(t *testing.T) {
	secret := strings.Repeat("x", 30) + "\x80\x80\x80\x80"
	label := labelFor(credential{APIKey: "oc_sk_" + secret})
	if strings.Contains(label, strings.Repeat("x", 10)) {
		t.Fatalf("walk widened the tail into the secret: %q", label)
	}
	if label != providerKey {
		t.Fatalf("invalid-UTF-8 tail must be dropped entirely, got %q", label)
	}
}

// secrets-1-1 / inputs-1-6 / scheduler-1-5: label and config fixes.
func TestLabelForNeverLeaksShortKeys(t *testing.T) {
	if got := labelFor(credential{APIKey: "oc_sk_" + "abcdefgh"}); got != providerKey {
		t.Fatalf("7-char secret must show no tail, got %q", got)
	}
	long := labelFor(credential{APIKey: "oc_sk_" + strings.Repeat("x", 30) + "Wxyz"})
	if !strings.HasSuffix(long, "Wxyz") || strings.Contains(long, strings.Repeat("x", 5)) {
		t.Fatalf("long key should show only the last 4 chars, got %q", long)
	}
	if got := labelFor(credential{Label: "my seat"}); got != "my seat" {
		t.Fatalf("user label must win, got %q", got)
	}
}

// sched-1-3: the metadata tells the host to schedule re-validation.
func TestKeyMetadataSchedulesRefresh(t *testing.T) {
	meta := keyMetadata(statusValid, "")
	if meta["refresh_interval_seconds"] != 900 {
		t.Fatalf("refresh interval missing: %v", meta)
	}
}

// inputs-1-10: corrupt storage is quarantined, never echoed.
func TestRefreshQuarantinesCorruptStorage(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"AuthID":      "go-adapter-1",
		"StorageJSON": []byte(`{"api_key":`),
	})
	out, err := handleAuthRefresh(raw)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	var resp authRefreshResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if !resp.Auth.Disabled {
		t.Fatal("corrupt storage must be quarantined")
	}
	if resp.Auth.Metadata["key_reason"] != "storage-corrupt" {
		t.Fatalf("expected storage-corrupt, got %v", resp.Auth.Metadata)
	}
	if len(resp.Auth.StorageJSON) != 0 {
		t.Fatalf("corrupt storage must not be echoed back, got %q", resp.Auth.StorageJSON)
	}
}

func TestParseResponseMarksInvalidKeyDisabled(t *testing.T) {
	// The wire-level consequence the host sees: a rejected key arrives as an
	// auth record with Disabled set, so it never becomes a routing candidate.
	srv := usageServer(t, 401, `{"type":"error","error":{"type":"AuthError"}}`)
	setTestBaseURL(t, srv.URL)

	raw, errMarshal := json.Marshal(map[string]any{
		"Provider": "go-adapter",
		"FileName": "go-adapter-x.json",
		"RawJSON":  []byte(`{"type":"go-adapter","api_key":"oc_sk_bogus","label":"x"}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	out, err := handleAuthParse(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	var resp authParseResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if !resp.Handled || resp.Auth == nil {
		t.Fatalf("expected a handled auth, got %s", out)
	}
	if !resp.Auth.Disabled {
		t.Fatalf("invalid key must arrive disabled, got %+v", resp.Auth)
	}
	if resp.Auth.Metadata["key_status"] != statusInvalid {
		t.Fatalf("expected key_status invalid, got %v", resp.Auth.Metadata)
	}
}

func TestParseResponseKeepsUnknownKeyEnabled(t *testing.T) {
	// The fail-open side of the same rule: an unreachable gateway must not
	// quarantine a key that may be perfectly good.
	srv := usageServer(t, 500, "boom")
	setTestBaseURL(t, srv.URL)

	raw, _ := json.Marshal(map[string]any{
		"Provider": "go-adapter",
		"FileName": "go-adapter-y.json",
		"RawJSON":  []byte(`{"type":"go-adapter","api_key":"oc_sk_maybe_good","label":"y"}`),
	})
	out, err := handleAuthParse(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	var resp authParseResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.Auth == nil || resp.Auth.Disabled {
		t.Fatalf("unknown key must stay enabled, got %s", out)
	}
}
