package kit

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Host is the subset of CPA host callbacks a provider plugin uses. The cgo
// bridge in package abi implements it over host.call; tests use kittest.Host.
type Host interface {
	// HTTPDo performs a buffered upstream request through CPA's transport (host.http.do).
	HTTPDo(callbackID string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	// HTTPDoStream opens a streaming upstream request (host.http.do_stream).
	HTTPDoStream(callbackID string, req pluginapi.HTTPRequest) (HTTPStream, error)
	// StreamEmit forwards one executor stream chunk to CPA (host.stream.emit).
	StreamEmit(streamID string, payload []byte) error
	// StreamClose finishes an executor stream, optionally with a failure (host.stream.close).
	StreamClose(streamID string, failure *pluginabi.Error)
	// Log writes through CPA's logger (host.log).
	Log(callbackID, level, message string, fields map[string]any)
}

// HTTPStream is an open upstream response body delivered in raw chunks.
type HTTPStream interface {
	StatusCode() int
	Header() http.Header
	// Next returns the next raw body chunk; io.EOF marks a clean end of body.
	Next() ([]byte, error)
	// Close releases the upstream stream; safe to call more than once.
	Close()
}
