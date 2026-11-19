package main

import (
	"encoding/json"
	"sync"
	"testing"
)

// inputs-2-4: a typo'd config key is an error, not a silent default.
func TestParseConfigRejectsUnknownFields(t *testing.T) {
	_, err := parseConfig([]byte("baseUrl: https://evil.example\n"))
	if err == nil {
		t.Fatal("unknown fields must be rejected under strict decoding")
	}
}

// inputs-2-4, host-node half: the host injects enabled/priority into every
// config subtree — strict decoding must accept those and still reject typos.
func TestParseConfigAcceptsHostKeys(t *testing.T) {
	cfgParsed, err := parseConfig([]byte("enabled: true\npriority: 2\nbase-url: https://a.example\n"))
	if err != nil {
		t.Fatalf("host-injected keys must pass strict decoding: %v", err)
	}
	if cfgParsed.BaseURL != "https://a.example" {
		t.Fatalf("base-url lost: %+v", cfgParsed)
	}
	if _, err := parseConfig([]byte("enabled: true\nbaseUrl: https://evil.example\n")); err == nil {
		t.Fatal("a typo'd key alongside host keys must still be rejected")
	}
}

// inputs-1-5: a YAML error is reported, never swallowed.
func TestParseConfigSurfacesYamlError(t *testing.T) {
	_, err := parseConfig([]byte("base-url: https://a.example\nbase-url: https://b.example\n"))
	if err == nil {
		t.Fatal("duplicate keys must be reported")
	}
}

// inputs-1-6: the advertised comma-separated form works.
func TestParseConfigAcceptsScalarModels(t *testing.T) {
	cfgParsed, err := parseConfig([]byte("models: gemini-x, claude-y\n"))
	if err != nil {
		t.Fatalf("scalar models must parse: %v", err)
	}
	if len(cfgParsed.Models) != 2 || cfgParsed.Models[0] != "gemini-x" {
		t.Fatalf("scalar models must split, got %v", cfgParsed.Models)
	}
}

// sched-1-1: config swaps under concurrent load are race-free.
func TestConfigSwapUnderRace(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("base-url: https://x.example\n")})
				_, _ = handleMethod("plugin.reconfigure", raw)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			c := currentConfig()
			_ = c.BaseURL + c.UserAgent
		}
	}()
	for i := 0; i < 2000; i++ {
		c := currentConfig()
		_ = c.BaseURL + c.UserAgent
	}
	close(stop)
	wg.Wait()
}

// Strict decoding also means strict types: `models: 123` is a config error, not a
// model called "123".
func TestParseConfigRejectsNonStringModels(t *testing.T) {
	for _, raw := range []string{"models: 123\n", "models: true\n", "models: [1, 2]\n"} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatalf("%q must be rejected", raw)
		}
	}
}

// A base-url without a scheme would fail every request with "unsupported
// protocol scheme"; it is a config error instead.
func TestParseConfigRejectsSchemalessBaseURL(t *testing.T) {
	if _, err := parseConfig([]byte("base-url: opencode.ai/inference/go/openai/v1\n")); err == nil {
		t.Fatal("a scheme-less base-url must be rejected")
	}
	if _, err := parseConfig([]byte("base-url: https://opencode.ai/x\n")); err != nil {
		t.Fatalf("a normal https base-url must be accepted: %v", err)
	}
}
