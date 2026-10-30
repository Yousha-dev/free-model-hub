package main

// This file declares the C ABI for the CPA plugin host and the CGO implementation of host callbacks.
//
// The host calls cliproxy_plugin_init after dlopen to pass two function tables;
// Subsequent RPCs go through cliproxyPluginCall, and host callbacks initiated by the plugin go through
// nativeHostCallScoped below. The struct layout must be byte-for-byte identical with the host sdk/pluginabi.

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

// Access wrappers for the host function table. Must be static: cgo copies the preamble into multiple
// generated files, and non-static definitions would cause duplicate symbol linkage failures. Host pointers are saved
// by the Go side and passed on each call, so C-level global variables are no longer needed here.
static int cpa_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (api == NULL || api->call == NULL) {
		return 1;
	}
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void cpa_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	if (api != NULL && api->free_buffer != NULL && ptr != NULL) {
		api->free_buffer(ptr, len);
	}
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unsafe"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// hostAPI stores the function table pointer passed by the host in cliproxy_plugin_init.
// Lifecycle is guaranteed by the host: dlclose occurs only after Shutdown drains.
var hostAPI *C.cliproxy_host_api

// cliproxy_plugin_init is called once by the host after loading the dynamic library.
// Returns 0 on success; non-zero causes the host to determine that plugin loading failed and roll back.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// cliproxyPluginCall is the sole entry point for all RPCs initiated by the host.
//
// Note on error handling convention: business errors are both written to the response envelope (allowing the caller to read code/message)
// and returned as a non-zero code. The host checks both—the envelope for the upper layer, and the code for the C layer.
//
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, core.ErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	methodName := C.GoString(method)
	requestBytes, errRequest := hostRequestBytes(methodName, unsafe.Pointer(request), uintptr(requestLen))
	if errRequest != nil {
		writeResponse(response, core.ErrorEnvelopeFromError(errRequest))
		return 1
	}
	raw, errHandle := handleMethod(methodName, requestBytes)
	if errHandle != nil {
		writeResponse(response, core.ErrorEnvelopeFromError(errHandle))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

// isReadOnlyRequestMethod determines whether the method can read-only borrow the host buffer during synchronous calls.
//
// Methods in the whitelist must satisfy: do not modify input parameters, do not store any slice fragments, and do not continue accessing
// after the call returns (including goroutine). Use a switch instead of a package-level map to avoid introducing mutable package-level state;
// add a case here for new methods rather than growing another if-statement in the ABI entry point.
func isReadOnlyRequestMethod(method string) bool {
	switch method {
	case pluginabi.MethodResponseNormalizeBefore:
		return true
	}
	return false
}

// hostRequestBytes converts the host request buffer into a Go slice.
//
// By default, it makes a copy (C.GoBytes); only methods identified by isReadOnlyRequestMethod directly borrow
// the host buffer—the response normalization hook is called for every SSE line, carrying two complete
// request bodies in the parameters (up to several MBs under long contexts), and copying would become a fixed overhead proportional to the request size.
//
// The borrowed slice is valid only within the synchronous call stack of handleMethod: callers must not modify it,
// nor store it beyond the return of the call. A nil pointer with a zero length is treated as an empty request;
// a nil pointer with a non-zero length is a host contract violation, reporting an error explicitly rather than silently treating it as an empty request.
func hostRequestBytes(method string, request unsafe.Pointer, requestLen uintptr) ([]byte, error) {
	if request == nil {
		if requestLen > 0 {
			return nil, core.NewPluginError("invalid_request",
				"request pointer is null but request length is non-zero", http.StatusBadRequest)
		}
		return nil, nil
	}
	if requestLen == 0 {
		return nil, nil
	}
	if isReadOnlyRequestMethod(method) {
		return unsafe.Slice((*byte)(request), requestLen), nil
	}
	return C.GoBytes(request, C.int(requestLen)), nil
}

// cliproxyPluginFree frees the response buffer allocated by the plugin using C.CBytes, called by the host.
//
//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

// cliproxyPluginShutdown is called by the host before unloading the dynamic library.
//
// The order cannot be swapped:
//  1. Stop background loops (check-in scheduler, task queue) so they stop initiating new host calls;
//  2. Cancel and wait for in-flight streaming forwarding to complete host.stream.close before shutdown;
//  3. Close host call admission and wait for active CGO calls to return;
//  4. Clear the host api.
//
// The host will dlclose this dynamic library after Shutdown: any Go/CGO stack still executing will crash,
// so step 3 must wait for draining (see explanation in hostgate.go).
const (
	// shutdownDiagnosticInterval is the interval for printing diagnostic logs during the shutdown draining period.
	shutdownDiagnosticInterval = 5 * time.Second
	// shutdownWaitTimeout is only used to wait for this plugin's own streaming goroutines.
	shutdownWaitTimeout = 10 * time.Second
)

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	pluginApp.StopBackgroundWork()
	pluginApp.WaitPluginStreams(shutdownWaitTimeout)
	hostbridge.WaitForShutdown(shutdownDiagnosticInterval)
	hostAPI = nil
}

// writeResponse copies the plugin response to the C heap buffer, transferring ownership to the host (released via free_buffer).
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

// main is a hard requirement of the c-shared build mode: even when producing a dynamic library,
// Go's main package must still contain a main function. It is never called.
func main() {}

// nativeHostCallScoped is the actual implementation of host callbacks (injected by hostbridge.Configure).
func nativeHostCallScoped(callbackID string, method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := hostbridge.MarshalPayload(callbackID, payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("%s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("%s: allocate host callback payload", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	if hostAPI == nil {
		return nil, fmt.Errorf("%s: host api not initialized", method)
	}
	callCode := C.cpa_call_host(hostAPI, cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.cpa_free_host_buffer(hostAPI, response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("%s: host callback returned no response, code=%d", method, int(callCode))
	}
	result, errDecode := hostbridge.DecodeEnvelope(rawResponse)
	if errDecode != nil {
		return nil, fmt.Errorf("%s: %w", method, errDecode)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("%s: host callback returned code=%d", method, int(callCode))
	}
	return result, nil
}

// init injects the CGO implementation: subsequently, root and internal packages uniformly use hostbridge.Call.
func init() {
	hostbridge.Configure(nativeHostCallScoped)
}
