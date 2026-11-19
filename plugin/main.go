// Package main is the C-ABI entry point for the opencode-go CLIProxyAPI plugin.
//
// CLIProxyAPI loads plugins as shared libraries and talks to them over a small
// JSON RPC: the host calls cliproxy_plugin_init once with a host API table, then
// dispatches method calls through cliproxyPluginCall. The glue below is the
// minimum that satisfies that contract and mirrors the reference plugin
// (hrz6976/cpa-plugin-opencode-go-pool, MIT).
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"math"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// pluginVersion is stamped at build time via -ldflags "-X main.pluginVersion=...".
var pluginVersion = "0.0.0-dev"

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	methodName := C.GoString(method)
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		// C.int truncation turns a >=2GiB length negative and panics C.GoBytes,
		// which no host recover() can catch across the C boundary.
		if requestLen > C.size_t(math.MaxInt32) {
			writeResponse(response, errorEnvelope("request_too_large", "request exceeds 2GiB"))
			return 1
		}
		// Refuse an oversized executor envelope BEFORE copying it. The ABI hands
		// us a C buffer, so the copy is unavoidable — but only for requests we
		// are willing to serve. (The host's own max-request-body-bytes defaults
		// to unlimited, so nothing upstream of this line bounds it.)
		if isExecutorMethod(methodName) && uint64(requestLen) > uint64(envelopeLimit) {
			writeResponse(response, errorEnvelope("request_too_large",
				fmt.Sprintf("request envelope exceeds the %d byte cap", envelopeLimit)))
			return 1
		}
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(methodName, requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// envelope mirrors the host's RPC envelope: {"ok":true,"result":…} or an error.
type rpcEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcEnvelopeErr `json:"error,omitempty"`
}

type rpcEnvelopeErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// callHost performs one host callback and decodes its result into out.
func callHost(method string, payload any, out any) error {
	var req []byte
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return errMarshal
		}
		req = raw
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(req))
		defer C.free(unsafe.Pointer(reqPtr))
	}
	rc := C.call_host_api(cMethod, reqPtr, C.size_t(len(req)), &response)
	var outBytes []byte
	if response.ptr != nil {
		if response.len > C.size_t(math.MaxInt32) {
			// A host reply beyond C.int range must be refused, not truncated
			// into a negative GoBytes length that panics past every recover().
			C.free_host_buffer(response.ptr, response.len)
			return fmt.Errorf("host call %s: reply exceeds C.int range", method)
		}
		outBytes = C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return fmt.Errorf("host call %s failed", method)
	}
	var env rpcEnvelope
	if errUnmarshal := json.Unmarshal(outBytes, &env); errUnmarshal != nil {
		return errUnmarshal
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("%s: host error", method)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// authFileEntry is one row of host.auth.list.
type authFileEntry struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Label     string `json:"label"`
	Disabled  bool   `json:"disabled"`
}

type authFileList struct {
	Files []authFileEntry `json:"files"`
}

type authGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

// keyForAuthID resolves an auth record's API key through the host, so the
// scheduler can score accounts it never received a secret for.
func keyForAuthID(authID string) (string, bool) {
	var list authFileList
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &list); err != nil {
		return "", false
	}
	for _, f := range list.Files {
		if f.ID != authID || f.Provider != providerKey || f.Disabled || f.AuthIndex == "" {
			continue
		}
		var got authGetResponse
		if err := callHost(pluginabi.MethodHostAuthGet, map[string]string{"auth_index": f.AuthIndex}, &got); err != nil {
			return "", false
		}
		var cred credential
		if json.Unmarshal(got.JSON, &cred) != nil {
			return "", false
		}
		if cred.APIKey == "" {
			return "", false
		}
		return cred.APIKey, true
	}
	return "", false
}

// isExecutorMethod reports whether a method carries a client request body, i.e.
// the only methods worth a size gate before the C copy.
func isExecutorMethod(method string) bool {
	switch method {
	case pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream:
		return true
	}
	return false
}
