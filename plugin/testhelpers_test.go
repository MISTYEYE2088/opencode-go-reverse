package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// Usage fixture with three states, one per policy class the caller cares about:
// mode 0 = 502 (transient, must NOT quarantine), mode 1 = 200 (healthy),
// mode 2 = 401 (definitive rejection, must quarantine at -20000).
const (
	usageTransient = 0
	usageHealthy   = 1
	usageRejected  = 2
)

func flippableUsageServer(t *testing.T, mode *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case usageTransient:
			w.WriteHeader(502)
			_, _ = w.Write([]byte("bad gateway"))
			return
		case usageRejected:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-11-18T12:02:19.000Z"},"weekly":{"status":"ok","percent":10,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":10,"resetsAt":"2026-11-21T01:47:19.000Z"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func isTransientErr(err error) bool {
	var transient transientUsageError
	return errors.As(err, &transient)
}

func isQuarantinedErr(err error) bool {
	var qua quarantinedError
	return errors.As(err, &qua)
}

// Round-2 red-team fixes, one pinning test per finding.

func utf8Valid(s string) bool { return utf8.ValidString(s) }

// Round-3 red-team fixes, one pinning test per finding.

// One test per red-team fix. Each test names the finding it pins so a regression
// maps straight back to the report.

func setBase(t *testing.T, url string) { setTestBaseURL(t, url) }

func chatServer(t *testing.T, status int, body, retryAfter string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func execReq(storage string, payload string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"AuthID":      "go-adapter-1",
		"Model":       "hy4-flash-free",
		"Payload":     []byte(payload),
		"StorageJSON": []byte(storage),
	})
	return raw
}

const goodPayload = `{"model":"hy4-flash-free","messages":[{"role":"user","content":"hi"}]}`

var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func opts() scoreOptions { return defaultScoreOptions(now) }

// win builds a window from percent used, resetting in the given time.
func win(pct float64, in time.Duration) Window {
	return Window{Percent: pct, Status: "ok", Known: true, ResetsAt: now.Add(in)}
}

// acct builds a snapshot: 5h used, monthly used, monthly reset in.
func acct(rolling, monthly float64, monthlyIn time.Duration) Quota {
	return Quota{
		Rolling: win(rolling, 2*time.Hour),
		Weekly:  win(50, 3*24*time.Hour),
		Monthly: win(monthly, monthlyIn),
	}
}

func score(t *testing.T, q Quota, sticky bool) int {
	t.Helper()
	s, why := Score(q, sticky, opts())
	if why != "" {
		t.Fatalf("unexpected veto: %s", why)
	}
	return s
}

// What/how: each test stands up a fake usage endpoint and checks that probeKey
// classifies the answer the way the auth quarantine depends on — definitive
// rejections disable, everything else fails open.

func usageServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("probe sent no Authorization header")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func isInvalidKeyErr(err error) bool {
	var bad invalidKeyError
	return errors.As(err, &bad)
}

// setTestBaseURL points the live config at a test server for one test.
func setTestBaseURL(t *testing.T, baseURL string) {
	t.Helper()
	old := currentConfig()
	configPtr.Store(&pluginConfig{BaseURL: baseURL, UserAgent: old.UserAgent, Models: old.Models})
	t.Cleanup(func() { configPtr.Store(&old) })
}
