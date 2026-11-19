package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// errControl is an error message containing bytes that Go's %q would escape in a
// way JSON cannot parse.
var errControl = errors.New("bad\x01byte")

// The resource route is not authenticated by the host, so it must serve a page
// with no data — even when the caller asks for JSON.
func TestResourceRouteServesShellOnly(t *testing.T) {
	raw, _ := json.Marshal(managementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/go-adapter/status",
		Query:  map[string][]string{"format": {"json"}},
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatalf("resource route errored: %v", err)
	}
	var resp managementResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ct := resp.Headers["Content-Type"][0]; !strings.Contains(ct, "text/html") {
		t.Fatalf("public route must serve HTML, got %q", ct)
	}
	body := string(resp.Body)
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("public route must not serve JSON: %s", body)
	}
	if !strings.Contains(body, "management key") {
		t.Fatalf("page must ask for the management key")
	}
}

// The resource route refuses the write path entirely.
func TestResourceRouteRejectsRefresh(t *testing.T) {
	raw, _ := json.Marshal(managementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/go-adapter/refresh",
	})
	out, _ := handleManagement(raw)
	var resp managementResponse
	_ = json.Unmarshal(out, &resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("refresh must not exist on the public route, got %d", resp.StatusCode)
	}
}

// Error bodies must be JSON even when the message carries control bytes.
func TestJSONErrorIsValidJSON(t *testing.T) {
	out, err := jsonError(http.StatusInternalServerError, errControl)
	if err != nil {
		t.Fatalf("jsonError failed: %v", err)
	}
	var resp managementResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if !json.Valid(resp.Body) {
		t.Fatalf("body is not valid JSON: %q", resp.Body)
	}
}

// The management/public split is decided by the registered route, never by a
// substring: a public path that embeds "/management/" must stay public.
func TestManagementPathClassification(t *testing.T) {
	cases := map[string]bool{
		"/v0/management/plugins/go-adapter/status":             true,
		"/v0/management/plugins/go-adapter/refresh":            true,
		"/management/plugins/go-adapter/status":                true,
		"/v0/resource/plugins/go-adapter/status":               false,
		"/v0/resource/plugins/go-adapter/x/management/status":  false,
		"/v0/resource/plugins/go-adapter/v0/management/status": false,
		"/v0/management/plugins/other-plugin/status":           false,
	}
	for path, want := range cases {
		if got := isManagementPath(path); got != want {
			t.Fatalf("isManagementPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// Only methods that carry a client body are size-gated before the C copy.
func TestIsExecutorMethodGate(t *testing.T) {
	if !isExecutorMethod("executor.execute") || !isExecutorMethod("executor.execute_stream") {
		t.Fatal("executor methods must be gated")
	}
	for _, method := range []string{"auth.parse", "quota.fetch", "management.handle", "model.static"} {
		if isExecutorMethod(method) {
			t.Fatalf("%s must not be gated", method)
		}
	}
}
