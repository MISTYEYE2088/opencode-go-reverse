package main

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Auth records are what the host selects per request. For this provider one
// record is one API key: {"type":"go-adapter","api_key":"oc_sk_…","label":"…"}.
// The host writes such a file under <auth-dir> with mode 0600, and asks this
// plugin to parse it (auth.parse) because the "type" is ours.

type authParseRequest struct {
	Provider string `json:"Provider"`
	Path     string `json:"Path"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
}

type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	Disabled         bool              `json:"Disabled"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter"`
}

type authParseResponse struct {
	Handled bool      `json:"Handled"`
	Auth    *authData `json:"Auth,omitempty"`
}

// credential is the provider-owned part of an auth record.
type credential struct {
	APIKey string `json:"api_key"`
	Label  string `json:"label,omitempty"`
}

// handleAuthParse recognises our auth files, validates the key against the
// gateway, and quarantines definitively rejected keys.
//
// #FUNC     Turn one auth file into a validated auth record.
// #EDGE-IN  A key the gateway rejects (401 / no Go plan) arrives → the record is
//
//	returned with Disabled set and the reason in Metadata → TestUnauthorizedKeyIsInvalid
//
// #EDGE-OUT A key whose probe could not complete (5xx, timeout) → the record stays
//
//	enabled with key_status unknown → TestServerErrorIsTransient
//
// #WHY      only `statusInvalid` sets `Disabled`; the whole point of the add-time
//
//	check is that a broken key can never sit silently in the pool, but a
//	a network blip must never quarantine a good one → TestTransientProbeDoesNotQuarantine
func handleAuthParse(raw []byte) ([]byte, error) {
	var req authParseRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	// Not ours: let the host fall back to its built-in parsers.
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerKey) {
		return json.Marshal(authParseResponse{Handled: false})
	}

	var cred credential
	if err := json.Unmarshal(req.RawJSON, &cred); err != nil || strings.TrimSpace(cred.APIKey) == "" {
		// Ours by type, but unusable: handled with no auth so the host skips it
		// instead of guessing.
		return json.Marshal(authParseResponse{Handled: true})
	}

	id := strings.TrimSuffix(req.FileName, ".json")
	if id == "" {
		id = strings.TrimSuffix(strings.TrimSpace(cred.Label), ".json")
	}
	storage, _ := json.Marshal(credential{APIKey: strings.TrimSpace(cred.APIKey), Label: cred.Label})

	apiKey := strings.TrimSpace(cred.APIKey)
	base := currentConfig().BaseURL
	status, reason, quota := checkKeyWithQuota(apiKey, base)
	applyQuarantineFromProbe(apiKey, status, reason)
	// One validation call already returned the windows and the credential we
	// would otherwise re-ask the host for on every pick: keep both.
	seedQuota(apiKey, base, quota)
	rememberAuthKey(id, apiKey)
	return json.Marshal(authParseResponse{
		Handled: true,
		Auth: &authData{
			Provider:         providerKey,
			ID:               id,
			FileName:         req.FileName,
			Label:            labelFor(cred),
			StorageJSON:      storage,
			Metadata:         keyMetadata(status, reason),
			Disabled:         status == statusInvalid,
			NextRefreshAfter: time.Now().Add(refreshInterval),
		},
	})
}

// authRefreshRequest is the host's auth.refresh payload (Go field names on the wire).
type authRefreshRequest struct {
	AuthID       string         `json:"AuthID"`
	AuthProvider string         `json:"AuthProvider"`
	StorageJSON  []byte         `json:"StorageJSON"`
	Metadata     map[string]any `json:"Metadata"`
}

// authRefreshResponse returns the refreshed record and when to check again.
type authRefreshResponse struct {
	Auth             authData  `json:"Auth"`
	NextRefreshAfter time.Time `json:"NextRefreshAfter"`
}

// applyQuarantineFromProbe keeps the quarantine set in step with a probe
// verdict. Valid clears it (a key that works again is welcome back), a
// definitive rejection sets it, and an unavailable gateway leaves it untouched:
// the network is not evidence about the key.
func applyQuarantineFromProbe(apiKey, status, reason string) {
	cacheKey := quotaCacheKey(apiKey, currentConfig().BaseURL)
	switch status {
	case statusValid:
		quarantineClear(cacheKey)
	case statusInvalid:
		quarantineSet(cacheKey, "invalid: "+reason)
	}
}

// handleAuthRefresh re-validates one key on the host's refresh cycle.
//
// #FUNC     Re-probe one credential and return the updated auth record.
// #EDGE_IN  A key that was valid at add time but is now rejected → the quarantine
//
//	set flips on and the scheduler stops routing to it → TestUnauthorizedKeyIsInvalid
//
// #EDGE_IN  StorageJSON is corrupt or keyless → the record returns Disabled with
//
//	reason storage-corrupt → TestRefreshQuarantinesCorruptStorage
//
// #WHY      the host Disabled bit is preserved via `authStateFn`, never
//
//	recomputed: the host merge forces Disabled back on once set
//	(sched-2-3), so flipping it here would be a one-way door the
//	manual refresh cannot reopen.
func handleAuthRefresh(raw []byte) ([]byte, error) {
	var req authRefreshRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	var cred credential
	storageErr := json.Unmarshal(req.StorageJSON, &cred)
	apiKey := strings.TrimSpace(cred.APIKey)

	if storageErr != nil || apiKey == "" {
		return json.Marshal(authRefreshResponse{
			Auth: authData{
				Provider:         providerKey,
				ID:               req.AuthID,
				FileName:         refreshFileName(req.AuthID),
				Label:            labelFor(cred),
				Metadata:         keyMetadata(statusInvalid, "storage-corrupt"),
				Disabled:         true,
				NextRefreshAfter: time.Now().Add(refreshInterval),
			},
			NextRefreshAfter: time.Now().Add(refreshInterval),
		})
	}

	storage, _ := json.Marshal(credential{APIKey: apiKey, Label: cred.Label})
	cacheKey := quotaCacheKey(apiKey, currentConfig().BaseURL)
	if reason, bad := quarantined(cacheKey); bad {
		// Routine checks are disabled for this key by design: the manual
		// refresh is its only re-check, so no upstream call is spent here.
		state := authStateFn(req.AuthID)
		return json.Marshal(authRefreshResponse{
			Auth: authData{
				Provider:         providerKey,
				ID:               req.AuthID,
				FileName:         refreshFileName(req.AuthID),
				Label:            labelFor(cred),
				StorageJSON:      storage,
				Metadata:         keyMetadata("quarantined", reason),
				Disabled:         state.Disabled,
				NextRefreshAfter: time.Now().Add(refreshInterval),
			},
			NextRefreshAfter: time.Now().Add(refreshInterval),
		})
	}

	base := currentConfig().BaseURL
	status, reason, quota := checkKeyWithQuota(apiKey, base)
	applyQuarantineFromProbe(apiKey, status, reason)
	seedQuota(apiKey, base, quota)
	rememberAuthKey(req.AuthID, apiKey)
	state := authStateFn(req.AuthID)
	return json.Marshal(authRefreshResponse{
		Auth: authData{
			Provider:         providerKey,
			ID:               req.AuthID,
			FileName:         refreshFileName(req.AuthID),
			Label:            labelFor(cred),
			StorageJSON:      storage,
			Metadata:         keyMetadata(status, reason),
			Disabled:         state.Disabled,
			NextRefreshAfter: time.Now().Add(refreshInterval),
		},
		NextRefreshAfter: time.Now().Add(refreshInterval),
	})
}

// refreshFileName recovers the auth file name from the host so refresh keeps
// the record attached to its file.
func refreshFileName(authID string) string {
	if name, ok := fileNameForAuthID(authID); ok {
		return name
	}
	return authID + ".json"
}

// fileNameForAuthID recovers the auth file name from the host so refresh keeps
// the record attached to its file.
func fileNameForAuthID(authID string) (string, bool) {
	var list authFileList
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &list); err != nil {
		return "", false
	}
	for _, f := range list.Files {
		if f.ID == authID && f.Provider == providerKey {
			if f.Name != "" {
				return f.Name, true
			}
		}
	}
	return "", false
}

// labelFor prefers the user's label, else a masked key so logs and the panel
// can identify the account without exposing it.
//
// #FUNC     Build a display label that cannot expose useful key material.
// #EDGE-IN  A short key (oc_sk_ + <20 secret chars) arrives → no tail is shown at all → TestLabelForNeverLeaksShortKeys
// #WHY      the length floor counts the secret part after the public `oc_sk_`
//
//	prefix; against a 7-char secret, six tail characters would be most
//	of the key, so short secrets get no tail and long ones get four.
func labelFor(cred credential) string {
	if l := strings.TrimSpace(cred.Label); l != "" {
		return l
	}
	key := strings.TrimSpace(cred.APIKey)
	secret := strings.TrimPrefix(key, "oc_sk_")
	if len(secret) > 20 {
		// Take the last four bytes, advancing until the slice is valid UTF-8
		// (at most four steps). Invalid trailing bytes can only shrink the
		// tail, never widen it toward the whole secret.
		start := len(secret) - 4
		if start < 0 {
			start = 0
		}
		for start < len(secret) && !utf8.ValidString(secret[start:]) {
			start++
		}
		if start >= len(secret) {
			return providerKey
		}
		return providerKey + "-" + secret[start:]
	}
	return providerKey
}

// #FUNC      Answer which provider id this plugin claims.
// #WHY       the host routes auth-file management by provider id, so claiming
//
//	`go-adapter` here is what makes the panel's auth dialogs work.
func handleAuthIdentifier() []byte {
	raw, _ := json.Marshal(identifierResponse{Identifier: providerKey})
	return raw
}

// authMethodList is the set of auth methods this plugin implements.
var authMethodList = []string{pluginabi.MethodAuthParse}
