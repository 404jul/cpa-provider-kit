// Package kittest drives a kit plugin through CPA's RPC methods without CPA,
// using a scripted fake host.
package kittest

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/404jul/cpa-provider-kit/kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Host is a fake kit.Host. Set Do and/or Stream to script upstream responses.
type Host struct {
	// Do answers buffered requests.
	Do func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse
	// Stream answers streaming requests.
	Stream func(req pluginapi.HTTPRequest) *Stream

	mu       sync.Mutex
	requests []pluginapi.HTTPRequest
	logs     []string
}

// Requests returns every upstream request made so far.
func (h *Host) Requests() []pluginapi.HTTPRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginapi.HTTPRequest(nil), h.requests...)
}

// Logs returns logged messages as "level: message".
func (h *Host) Logs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

func (h *Host) record(req pluginapi.HTTPRequest) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	h.mu.Unlock()
}

// HTTPDo implements kit.Host.
func (h *Host) HTTPDo(_ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.record(req)
	if h.Do == nil {
		return pluginapi.HTTPResponse{StatusCode: http.StatusNotImplemented, Body: []byte(`{"error":{"message":"kittest: no Do handler"}}`)}, nil
	}
	return h.Do(req), nil
}

// HTTPDoStream implements kit.Host.
func (h *Host) HTTPDoStream(_ string, req pluginapi.HTTPRequest) (kit.HTTPStream, error) {
	h.record(req)
	if h.Stream == nil {
		return &Stream{Status: http.StatusNotImplemented}, nil
	}
	return h.Stream(req), nil
}

// StreamEmit implements kit.Host. kittest drives streams synchronously (no
// stream_id), so the host-side streaming bridge is never used.
func (h *Host) StreamEmit(string, []byte) error { return nil }

// StreamClose implements kit.Host.
func (h *Host) StreamClose(string, *pluginabi.Error) {}

// Log implements kit.Host.
func (h *Host) Log(_, level, message string, _ map[string]any) {
	h.mu.Lock()
	h.logs = append(h.logs, level+": "+message)
	h.mu.Unlock()
}

// Stream is a scripted upstream body.
type Stream struct {
	Status  int
	Headers http.Header
	Chunks  []string

	mu     sync.Mutex
	closed bool
}

// SSE builds an event-stream response split into chunkSize-byte reads, which
// exercises event reassembly across reads.
func SSE(body string, chunkSize int) *Stream {
	s := &Stream{Status: http.StatusOK, Headers: http.Header{"Content-Type": {"text/event-stream"}}}
	for len(body) > chunkSize && chunkSize > 0 {
		s.Chunks = append(s.Chunks, body[:chunkSize])
		body = body[chunkSize:]
	}
	s.Chunks = append(s.Chunks, body)
	return s
}

// JSON builds a non-stream JSON response to a stream request.
func JSON(status int, body string) *Stream {
	return &Stream{Status: status, Headers: http.Header{"Content-Type": {"application/json"}}, Chunks: []string{body}}
}

// StatusCode implements kit.HTTPStream.
func (s *Stream) StatusCode() int { return s.Status }

// Header implements kit.HTTPStream.
func (s *Stream) Header() http.Header { return s.Headers }

// Next implements kit.HTTPStream.
func (s *Stream) Next() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.Chunks[0]
	s.Chunks = s.Chunks[1:]
	return []byte(chunk), nil
}

// Close implements kit.HTTPStream.
func (s *Stream) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Closed reports whether the plugin released the stream.
func (s *Stream) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Call sends one RPC to the plugin. It fails the test on transport-level
// problems and returns the plugin's error envelope, if any; on success the
// result is decoded into out (when non-nil).
func Call(t testing.TB, plugin *kit.Plugin, method string, request any, out any) *pluginabi.Error {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("%s: encode request: %v", method, errMarshal)
	}
	var env pluginabi.Envelope
	if errDecode := json.Unmarshal(plugin.Handle(method, raw), &env); errDecode != nil {
		t.Fatalf("%s: decode envelope: %v", method, errDecode)
	}
	if !env.OK {
		if env.Error == nil {
			t.Fatalf("%s: failed envelope without error", method)
		}
		return env.Error
	}
	if out != nil {
		if errDecode := json.Unmarshal(env.Result, out); errDecode != nil {
			t.Fatalf("%s: decode result: %v", method, errDecode)
		}
	}
	return nil
}

// MustCall is Call that fails the test on a plugin error.
func MustCall(t testing.TB, plugin *kit.Plugin, method string, request any, out any) {
	t.Helper()
	if failure := Call(t, plugin, method, request, out); failure != nil {
		t.Fatalf("%s: %s (status %d)", method, failure.Message, failure.HTTPStatus)
	}
}

// Register registers the plugin with optional YAML config.
func Register(t testing.TB, plugin *kit.Plugin, configYAML string) {
	t.Helper()
	MustCall(t, plugin, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte(configYAML)}, nil)
}

// ExecRequest is the executor RPC payload: CPA's ExecutorRequest plus routing fields.
type ExecRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// Exec builds an executor request for format with an auth file as storage.
func Exec(format kit.Format, model, payload, authFile string) ExecRequest {
	return ExecRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Format: string(format), Model: model, Payload: []byte(payload), StorageJSON: []byte(authFile), Headers: http.Header{},
	}}
}

// StreamResult is the decoded executor.execute_stream result.
type StreamResult struct {
	Headers http.Header                     `json:"headers"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks"`
}

// Events returns the chunk payloads as strings.
func (r StreamResult) Events() []string {
	out := make([]string, 0, len(r.Chunks))
	for _, chunk := range r.Chunks {
		out = append(out, string(chunk.Payload))
	}
	return out
}
