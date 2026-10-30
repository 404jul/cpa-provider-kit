// Package abi exports a kit plugin over CLIProxyAPI's native plugin C ABI.
//
// A provider's main package registers its plugin in init and builds with
// go build -buildmode=c-shared:
//
//	func init() { abi.Register(func(host kit.Host) abi.Handler { return kit.New(provider.Spec(version), host) }) }
//	func main() {}
package abi

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

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

extern int cpakitPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cpakitPluginFree(void*, size_t);
extern void cpakitPluginShutdown(void);

static int cpakit_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void cpakit_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/404jul/cpa-provider-kit/kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Handler serves RPC methods; *kit.Plugin implements it.
type Handler interface {
	Handle(method string, request []byte) []byte
}

var (
	hostAPI  atomic.Pointer[C.cliproxy_host_api]
	mu       sync.Mutex
	factory  func(kit.Host) Handler
	instance Handler
)

// Register installs the plugin factory. Call it from an init function.
func Register(build func(host kit.Host) Handler) {
	mu.Lock()
	defer mu.Unlock()
	factory = build
	instance = nil
}

func current() Handler {
	mu.Lock()
	defer mu.Unlock()
	if instance == nil && factory != nil {
		instance = factory(cHost{})
	}
	return instance
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	hostAPI.Store(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cpakitPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cpakitPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cpakitPluginShutdown)
	return 0
}

//export cpakitPluginCall
func cpakitPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	handler := current()
	if method == nil || handler == nil {
		raw, _ := pluginabi.NewErrorEnvelope("plugin_unavailable", "plugin is not registered (missing abi.Register?)")
		writeResponse(response, raw)
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, handler.Handle(C.GoString(method), requestBytes))
	return 0
}

//export cpakitPluginFree
func cpakitPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cpakitPluginShutdown
func cpakitPluginShutdown() {
	hostAPI.Store(nil)
}

func writeResponse(response *C.cliproxy_buffer, data []byte) {
	if response == nil || len(data) == 0 {
		return
	}
	ptr := C.malloc(C.size_t(len(data)))
	if ptr == nil {
		return
	}
	C.memcpy(ptr, unsafe.Pointer(&data[0]), C.size_t(len(data)))
	response.ptr = ptr
	response.len = C.size_t(len(data))
}

// callHost sends one JSON request through host.call and decodes the envelope result.
func callHost(method string, request any, result any) error {
	host := hostAPI.Load()
	if host == nil || host.call == nil {
		return errors.New("CLIProxyAPI host callbacks are unavailable")
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return errMarshal
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(raw) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(raw))
		defer C.free(unsafe.Pointer(requestPtr))
	}
	var resp C.cliproxy_buffer
	code := C.cpakit_call_host(host, cMethod, requestPtr, C.size_t(len(raw)), &resp)
	var rawResp []byte
	if resp.ptr != nil {
		rawResp = C.GoBytes(resp.ptr, C.int(resp.len))
		C.cpakit_free_host_buffer(host, resp.ptr, resp.len)
	}
	var envelope pluginabi.Envelope
	if len(rawResp) > 0 {
		if errDecode := json.Unmarshal(rawResp, &envelope); errDecode != nil {
			return fmt.Errorf("%s: decode host response: %w", method, errDecode)
		}
	}
	if !envelope.OK {
		if envelope.Error != nil && envelope.Error.Message != "" {
			return errors.New(envelope.Error.Message)
		}
		return fmt.Errorf("%s failed (code %d)", method, int(code))
	}
	if result == nil || len(envelope.Result) == 0 {
		return nil
	}
	return json.Unmarshal(envelope.Result, result)
}

// cHost implements kit.Host over the CPA host callback table.
type cHost struct{}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

func newHostHTTPRequest(callbackID string, req pluginapi.HTTPRequest) hostHTTPRequest {
	return hostHTTPRequest{HostCallbackID: callbackID, Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body}
}

func (cHost) HTTPDo(callbackID string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var resp pluginapi.HTTPResponse
	err := callHost(pluginabi.MethodHostHTTPDo, newHostHTTPRequest(callbackID, req), &resp)
	return resp, err
}

func (cHost) HTTPDoStream(callbackID string, req pluginapi.HTTPRequest) (kit.HTTPStream, error) {
	var resp struct {
		StatusCode int         `json:"status_code"`
		Headers    http.Header `json:"headers,omitempty"`
		StreamID   string      `json:"stream_id,omitempty"`
	}
	if errCall := callHost(pluginabi.MethodHostHTTPDoStream, newHostHTTPRequest(callbackID, req), &resp); errCall != nil {
		return nil, errCall
	}
	if resp.StreamID == "" {
		return nil, errors.New("host.http.do_stream returned no stream_id")
	}
	return &hostStream{id: resp.StreamID, status: resp.StatusCode, header: resp.Headers}, nil
}

func (cHost) StreamEmit(streamID string, payload []byte) error {
	return callHost(pluginabi.MethodHostStreamEmit, struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}{streamID, payload}, nil)
}

func (cHost) StreamClose(streamID string, failure *pluginabi.Error) {
	request := struct {
		StreamID    string           `json:"stream_id"`
		Error       string           `json:"error,omitempty"`
		ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
	}{StreamID: streamID, ErrorDetail: failure}
	if failure != nil {
		request.Error = failure.Message
	}
	_ = callHost(pluginabi.MethodHostStreamClose, request, nil)
}

func (cHost) Log(callbackID, level, message string, fields map[string]any) {
	_ = callHost(pluginabi.MethodHostLog, struct {
		HostCallbackID string         `json:"host_callback_id,omitempty"`
		Level          string         `json:"level"`
		Message        string         `json:"message"`
		Fields         map[string]any `json:"fields,omitempty"`
	}{callbackID, level, message, fields}, nil)
}

// hostStream reads an upstream body opened with host.http.do_stream. Next is
// called from a single goroutine; Close may race with it and is idempotent.
type hostStream struct {
	id     string
	status int
	header http.Header
	done   bool
	closed atomic.Bool
}

func (s *hostStream) StatusCode() int     { return s.status }
func (s *hostStream) Header() http.Header { return s.header }

func (s *hostStream) Next() ([]byte, error) {
	for !s.done {
		var resp struct {
			Payload []byte `json:"payload,omitempty"`
			Error   string `json:"error,omitempty"`
			Done    bool   `json:"done,omitempty"`
		}
		if errRead := callHost(pluginabi.MethodHostHTTPStreamRead, struct {
			StreamID string `json:"stream_id"`
		}{s.id}, &resp); errRead != nil {
			return nil, errRead
		}
		if resp.Error != "" {
			return resp.Payload, errors.New(resp.Error)
		}
		s.done = resp.Done
		if len(resp.Payload) > 0 {
			return resp.Payload, nil
		}
	}
	return nil, io.EOF
}

func (s *hostStream) Close() {
	if s.closed.Swap(true) {
		return
	}
	_ = callHost(pluginabi.MethodHostHTTPStreamClose, struct {
		StreamID string `json:"stream_id"`
	}{s.id}, nil)
}
