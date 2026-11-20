package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidKeyClassified(t *testing.T) {
	srv := usageServer(t, 200, `{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-11-18T12:02:19.000Z"},
		"weekly":{"status":"ok","percent":20,"resetsAt":"2026-11-23T00:00:00.000Z"},
		"monthly":{"status":"ok","percent":30,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`)
	status, reason := probeKey(http.DefaultClient, "k", srv.URL)
	if status != statusValid || reason != "" {
		t.Fatalf("want valid, got %q/%q", status, reason)
	}
}

func TestUnauthorizedKeyIsInvalid(t *testing.T) {
	// The exact body the real gateway returns for a bad key (measured 2026-11-18).
	srv := usageServer(t, 401, `{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}`)
	status, reason := probeKey(http.DefaultClient, "k", srv.URL)
	if status != statusInvalid || reason != "unauthorized" {
		t.Fatalf("want invalid/unauthorized, got %q/%q", status, reason)
	}
	quota, err := fetchUsageWith(http.DefaultClient, "k", srv.URL)
	if !errors.As(err, new(*invalidKeyError)) && !isInvalidKeyErr(err) {
		t.Fatalf("fetchUsage must surface invalidKeyError for 401, got %v (quota=%v)", err, quota)
	}
}

func TestEntitlementErrorIsInvalid(t *testing.T) {
	srv := usageServer(t, 403, `{"type":"error","error":{"type":"EntitlementError"}}`)
	status, reason := probeKey(http.DefaultClient, "k", srv.URL)
	if status != statusInvalid || reason != "no-go-plan" {
		t.Fatalf("want invalid/no-go-plan, got %q/%q", status, reason)
	}
}

func TestServerErrorIsTransient(t *testing.T) {
	// A 5xx is the gateway having a bad minute, not the key: it must classify as
	// unknown and leave the key in the pool (user decision).
	srv := usageServer(t, 500, "boom")
	status, reason := probeKey(http.DefaultClient, "k", srv.URL)
	if status != statusUnknown || reason != "http-500" {
		t.Fatalf("want unknown/http-500, got %q/%q", status, reason)
	}
}

func TestRateLimitedIsTransient(t *testing.T) {
	srv := usageServer(t, 429, `{"error":"slow down"}`)
	status, reason := probeKey(http.DefaultClient, "k", srv.URL)
	if status != statusUnknown || reason != "http-429" {
		t.Fatalf("want unknown/http-429, got %q/%q", status, reason)
	}
}

func TestForbiddenWithoutEntitlementIsTransient(t *testing.T) {
	// A bare 403 (WAF, proxy) is not a verdict about the key.
	srv := usageServer(t, 403, `{"_tag":"Forbidden"}`)
	status, reason := probeKey(http.DefaultClient, "k", srv.URL)
	if status != statusUnknown || reason != "http-403" {
		t.Fatalf("want unknown/http-403, got %q/%q", status, reason)
	}
}

// TestLegacyUsageRouteFallback pins the documented route chain: the current route
// failing must be retried once on /zen/go/v1/usage instead of quarantining keys.
func TestLegacyUsageRouteFallback(t *testing.T) {
	var primaryHits, legacyHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inference/go/v1/usage":
			primaryHits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not here"))
		case "/zen/go/v1/usage":
			legacyHits++
			_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-11-18T12:02:19.000Z"},"weekly":{"status":"ok","percent":10,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":10,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	status, reason := probeKey(http.DefaultClient, "k", srv.URL+"/inference/go/openai/v1")
	if status != statusValid {
		t.Fatalf("legacy route must rescue the read, got %q/%q", status, reason)
	}
	if primaryHits != 1 || legacyHits != 1 {
		t.Fatalf("want 1 primary + 1 legacy call, got %d/%d", primaryHits, legacyHits)
	}
}

func TestUnreachableGatewayIsUnknown(t *testing.T) {
	srv := usageServer(t, 200, "{}")
	srv.Close() // nothing is listening any more
	status, _ := probeKey(http.DefaultClient, "k", srv.URL)
	if status != statusUnknown {
		t.Fatalf("want unknown when unreachable, got %q", status)
	}
}

// A 200 carrying something that is not a three-window usage document is "no
// answer", never a validation: a WAF page must not clear a quarantine, and it
// must not quarantine a good key either.
func TestMalformed200IsNotAUsageDocument(t *testing.T) {
	srv := usageServer(t, 200, `{"error":{"message":"challenge"}}`)
	t.Cleanup(srv.Close)
	status, reason, quota := probeKeyWithQuota(http.DefaultClient, "k", srv.URL)
	if status != statusUnknown {
		t.Fatalf("a non-usage 200 must be unknown, got %q (%q)", status, reason)
	}
	if quota.Known() {
		t.Fatal("a non-usage 200 must not yield quota data")
	}
}
