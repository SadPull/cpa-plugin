//go:build cshared

// Command cpa-model-visibility is the CLIProxyAPI plugin entry point.
// It bridges the C ABI (cliproxy_plugin_init + JSON envelope calls) to the
// Go App in internal/plugin.
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

// Wrappers so Go can invoke the host function-pointer table via cgo.
static int cmv_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void cmv_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	plugin "cpa-model-visibility/internal/plugin"
)

var (
	app = plugin.Default()

	hostAPIMu sync.RWMutex
	hostAPI   *C.cliproxy_host_api // captured at init, used for host.log callbacks
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, pluginAPI *C.cliproxy_plugin_api) C.int {
	if host == nil || pluginAPI == nil {
		return 1
	}
	hostAPIMu.Lock()
	hostAPI = host
	hostAPIMu.Unlock()

	app.SetHostCaller(callHost)

	pluginAPI.abi_version = C.uint32_t(plugin.ABIVersion)
	pluginAPI.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	pluginAPI.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	pluginAPI.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil || response == nil {
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	result := app.HandleMethod(C.GoString(method), requestBytes)
	if len(result) == 0 {
		return 1
	}
	response.ptr = C.CBytes(result)
	response.len = C.size_t(len(result))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// Intentionally a no-op. The host calls this on its own exit path and
	// dlclose()es the library right afterwards; touching Go runtime state
	// here risks a SIGSEGV (see workbuddy/main.go). The plugin keeps no
	// resources that outlive the process.
}

// callHost marshals a plugin→host callback (host.log) and reads the
// envelope back.
func callHost(method string, payload any) (json.RawMessage, error) {
	hostAPIMu.RLock()
	api := hostAPI
	hostAPIMu.RUnlock()
	if api == nil {
		return nil, fmt.Errorf("host API unavailable")
	}

	request, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host request %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}

	var resp C.cliproxy_buffer
	rc := C.cmv_call_host(api, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(unsafe.Pointer(resp.ptr), C.int(resp.len))
	}
	if resp.ptr != nil {
		C.cmv_free_host_buffer(api, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}

	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(out) > 0 {
		if err := json.Unmarshal(out, &envelope); err != nil {
			return nil, fmt.Errorf("decode host envelope %s: %w", method, err)
		}
		if !envelope.OK {
			if envelope.Error != nil {
				return nil, fmt.Errorf("host call %s: %s: %s", method, envelope.Error.Code, envelope.Error.Message)
			}
			return nil, fmt.Errorf("host call %s failed", method)
		}
	}
	return envelope.Result, nil
}

func main() {}
