package main

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// pluginConfig is what a user fills in under plugins.configs.go-adapter.
type pluginConfig struct {
	BaseURL   string     `yaml:"base-url"`
	UserAgent string     `yaml:"user-agent"`
	Models    stringList `yaml:"models"`
	// The host injects these two into every config subtree before passing it
	// (pluginhost normalizedConfigNode). They are accepted so strict decoding
	// does not reject the whole block, and ignored by the plugin.
	Enabled  *bool `yaml:"enabled,omitempty"`
	Priority *int  `yaml:"priority,omitempty"`
}

// stringList accepts a YAML list or a comma-separated scalar, because the
// plugin's own advertised config field describes the scalar form.
type stringList []string

func (s *stringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		// A scalar must actually be a string: `models: 123` or `models: true` is
		// a config error, not a model id.
		if node.Tag != "!!str" {
			return fmt.Errorf("models: expected a string or a list of strings, got %s", node.Tag)
		}
		*s = splitComma(node.Value)
		return nil
	}
	// A sequence must be a sequence of strings: [1, 2] is a config error, not a
	// catalogue of numeric ids.
	var nodes []yaml.Node
	if err := node.Decode(&nodes); err != nil {
		return err
	}
	list := make([]string, 0, len(nodes))
	for _, item := range nodes {
		if item.Tag != "!!str" {
			return fmt.Errorf("models: expected a list of strings, found %s", item.Tag)
		}
		list = append(list, item.Value)
	}
	*s = list
	return nil
}

func splitComma(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

const (
	defaultBaseURL   = "https://opencode.ai/inference/go/openai/v1"
	defaultUserAgent = "opencode-go-reverse/0.1"
)

// parseConfig decodes the plugin's YAML config block, applying defaults, and
// reports the parse error instead of swallowing it. Decoding is strict: the
// host-injected keys (enabled, priority) are declared so they pass, while any
// unknown key — a typo like baseUrl: — is an error, never a silent default.
//
// #FUNC     Decode plugin config with defaults applied.
// #EDGE-IN  Duplicate keys or wrong types arrive → the error is returned, the
//
//	caller keeps the last known-good config → TestParseConfigSurfacesYamlError
//
// #WHY      a swallowed YAML error silently fails open to `defaultBaseURL`,
//
//	re-pointing credentialed traffic at an upstream the operator never chose.
func parseConfig(raw []byte) (pluginConfig, error) {
	cfg := pluginConfig{}
	var err error
	if len(raw) > 0 {
		// Strict decode: a typo'd key name (baseUrl:) must be a config error,
		// not a silent slide back to the default upstream.
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		err = decoder.Decode(&cfg)
	}
	cfg.BaseURL = strings.TrimSuffix(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	} else if errScheme := validateBaseURL(cfg.BaseURL); errScheme != nil {
		// A scheme-less base-url ("opencode.ai/…") would fail every request with
		// "unsupported protocol scheme" and silently fall back to the bootstrap
		// catalogue, so it is a config error naming the field.
		return cfg, fmt.Errorf("base-url: %w", errScheme)
	}
	cfg.UserAgent = strings.TrimSpace(cfg.UserAgent)
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	return cfg, err
}

// configPtr holds the live config. An atomic pointer because the host calls
// plugin.reconfigure and executor/scheduler methods on parallel goroutines.
var configPtr atomic.Pointer[pluginConfig]

func init() {
	defaults, _ := parseConfig(nil)
	configPtr.Store(&defaults)
}

// currentConfig returns the live configuration for serving paths.
func currentConfig() pluginConfig {
	if p := configPtr.Load(); p != nil {
		return *p
	}
	defaults, _ := parseConfig(nil)
	return defaults
}

// validateBaseURL requires an absolute http(s) URL: the plugin talks to one
// gateway and a typo must surface as a config error, not as a silent fallback.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("needs an http:// or https:// prefix, got %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("needs a host, got %q", raw)
	}
	return nil
}
