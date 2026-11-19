package main

import (
	"testing"
)

// inputs-2-4: a broken reconfigure envelope keeps the last known-good config.
func TestReconfigureBrokenEnvelopeKeepsLastGood(t *testing.T) {
	before := currentConfig()
	if _, err := handleMethod("plugin.reconfigure", []byte(`{"config_yaml": not-json`)); err != nil {
		t.Fatalf("reconfigure must not hard-fail: %v", err)
	}
	after := currentConfig()
	if after.BaseURL != before.BaseURL {
		t.Fatalf("broken envelope reset live config: %q -> %q", before.BaseURL, after.BaseURL)
	}
}
