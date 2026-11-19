package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// authState is what the scheduler-side quarantine needs from the host about
// one auth record.
type authState struct {
	Key      string
	Disabled bool
	Exists   bool
}

// authStateFn is a seam so tests can stub the host lookup.
var authStateFn = authStateForAuthID

// authStateForAuthID resolves one auth's credential and host-disabled flag.
//
// #FUNC     Resolve an auth's key and Disabled flag through the host.
// #WHY      refresh must PRESERVE the host Disabled bit instead of recomputing
//
//	it: the host merge forces Disabled back on once set (sched-2-3),
//	so a refresh that flipped it would trap a recovered key out of
//	the pool with no automatic way back.
func authStateForAuthID(authID string) authState {
	var list authFileList
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &list); err != nil {
		return authState{}
	}
	for _, f := range list.Files {
		if f.ID != authID || f.Provider != providerKey {
			continue
		}
		state := authState{Disabled: f.Disabled, Exists: true}
		if f.AuthIndex == "" {
			return state
		}
		var got authGetResponse
		if err := callHost(pluginabi.MethodHostAuthGet, map[string]string{"auth_index": f.AuthIndex}, &got); err != nil {
			return state
		}
		var cred credential
		if json.Unmarshal(got.JSON, &cred) == nil {
			state.Key = strings.TrimSpace(cred.APIKey)
		}
		return state
	}
	return authState{}
}
