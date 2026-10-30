package kit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AuthStyle selects how the credential is sent upstream.
type AuthStyle int

const (
	// AuthDefault sends "Authorization: Bearer <token>", plus "X-Api-Key" for FormatClaude.
	AuthDefault AuthStyle = iota
	// AuthBearer sends only "Authorization: Bearer <token>".
	AuthBearer
	// AuthXAPIKey sends only "X-Api-Key: <token>".
	AuthXAPIKey
	// AuthRaw sends "Authorization: <token>" without a scheme.
	AuthRaw
)

// Endpoint is one upstream route for a protocol.
type Endpoint struct {
	// Path is appended to the base URL, e.g. "/v1/chat/completions".
	Path string
	// CountTokensPath optionally serves Anthropic count_tokens (FormatClaude only).
	CountTokensPath string
	Auth            AuthStyle
}

// Passthrough forwards requests unchanged (except the model name and stream
// flag) to providers that natively speak OpenAI Chat Completions, Anthropic
// Messages, and/or OpenAI Responses. CPA translates other client protocols.
type Passthrough struct {
	// Endpoints maps each served protocol to its upstream route. Only
	// FormatChat, FormatClaude, and FormatResponses are supported.
	Endpoints map[Format]Endpoint
	// Token returns the secret sent upstream; nil uses Credential.APIKey.
	Token func(cred Credential) (string, error)
	// Headers optionally adds provider-specific headers to every upstream request.
	Headers func(call Call, h http.Header)
	// Body optionally rewrites the payload after the model and stream flag are set.
	Body func(call Call, body []byte) ([]byte, error)
}

func (p *Passthrough) check() error {
	if len(p.Endpoints) == 0 {
		return errors.New("Passthrough needs at least one endpoint")
	}
	for format, endpoint := range p.Endpoints {
		switch format {
		case FormatChat, FormatClaude, FormatResponses:
		default:
			return fmt.Errorf("Passthrough does not support format %q; implement Executor instead", format)
		}
		if !strings.HasPrefix(endpoint.Path, "/") {
			return fmt.Errorf("endpoint path for %s must start with /", format)
		}
	}
	return nil
}

// Formats implements Executor in a stable order.
func (p *Passthrough) Formats() []Format {
	formats := make([]Format, 0, len(p.Endpoints))
	for format := range p.Endpoints {
		formats = append(formats, format)
	}
	sort.Slice(formats, func(i, j int) bool { return formats[i] < formats[j] })
	return formats
}

func (p *Passthrough) request(ctx *Ctx, call Call, stream bool, path string) (pluginRequest, error) {
	endpoint, ok := p.Endpoints[call.Format]
	if !ok {
		return pluginRequest{}, Fail(http.StatusBadRequest, "unsupported_format", "%s cannot serve format %s", ctx.Spec.Name, call.Format)
	}
	token := call.Credential.APIKey
	if p.Token != nil {
		var errToken error
		if token, errToken = p.Token(call.Credential); errToken != nil {
			return pluginRequest{}, errToken
		}
	}
	if token == "" {
		return pluginRequest{}, Fail(http.StatusUnauthorized, "invalid_api_key", "%s credential has no token", ctx.Spec.Name)
	}
	body, errBody := PrepareBody(call.Payload, call.Model, stream)
	if errBody != nil {
		return pluginRequest{}, Fail(http.StatusBadRequest, "invalid_request", "%v", errBody)
	}
	if p.Body != nil {
		if body, errBody = p.Body(call, body); errBody != nil {
			return pluginRequest{}, errBody
		}
	}
	if path == "" {
		path = endpoint.Path
	}
	headers := upstreamHeaders(call, endpoint.Auth, token, stream)
	if p.Headers != nil {
		p.Headers(call, headers)
	}
	return pluginRequest{url: ctx.Config.URL(path), headers: headers, body: body}, nil
}

type pluginRequest struct {
	url     string
	headers http.Header
	body    []byte
}

func httpPost(req pluginRequest) pluginapi.HTTPRequest {
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: req.url, Headers: req.headers, Body: req.body}
}

// Execute implements Executor.
func (p *Passthrough) Execute(ctx *Ctx, call Call) (Response, error) {
	req, errReq := p.request(ctx, call, false, "")
	if errReq != nil {
		return Response{}, errReq
	}
	return doBuffered(ctx, req)
}

// CountTokens implements Executor for Anthropic endpoints with CountTokensPath.
func (p *Passthrough) CountTokens(ctx *Ctx, call Call) (Response, error) {
	endpoint := p.Endpoints[call.Format]
	if call.Format != FormatClaude || endpoint.CountTokensPath == "" {
		return Response{}, Fail(http.StatusNotImplemented, "unsupported_operation", "%s does not support token counting for %s", ctx.Spec.Name, call.Format)
	}
	req, errReq := p.request(ctx, call, false, endpoint.CountTokensPath)
	if errReq != nil {
		return Response{}, errReq
	}
	if body, errDelete := sjson.DeleteBytes(req.body, "stream"); errDelete == nil {
		req.body = body
	}
	return doBuffered(ctx, req)
}

func doBuffered(ctx *Ctx, req pluginRequest) (Response, error) {
	resp, errDo := ctx.Do(httpPost(req))
	if errDo != nil {
		return Response{}, TransportFailure(ctx.Spec.Name, errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || IsFailureBody(resp.Body) {
		return Response{}, ctx.Spec.Errors.UpstreamFailure(ctx.Spec.Name, resp.StatusCode, resp.Headers, resp.Body)
	}
	return Response{Payload: resp.Body, Headers: ResponseHeaders(resp.Headers)}, nil
}

// ExecuteStream implements Executor.
func (p *Passthrough) ExecuteStream(ctx *Ctx, call Call) (Stream, error) {
	req, errReq := p.request(ctx, call, true, "")
	if errReq != nil {
		return Stream{}, errReq
	}
	return OpenSSE(ctx, call.Format, httpPost(req))
}

// OpenSSE opens an upstream SSE request and shapes its events for format.
// Custom executors whose upstream already speaks the client protocol can reuse it.
func OpenSSE(ctx *Ctx, format Format, req pluginapi.HTTPRequest) (Stream, error) {
	upstream, errDo := ctx.DoStream(req)
	if errDo != nil {
		return Stream{}, TransportFailure(ctx.Spec.Name, errDo)
	}
	first, errFirst := upstream.Next()
	if errFirst != nil && !errors.Is(errFirst, io.EOF) {
		upstream.Close()
		return Stream{}, TransportFailure(ctx.Spec.Name, errFirst)
	}
	status := upstream.StatusCode()
	// Some gateways answer stream requests with a JSON error envelope and HTTP 200.
	if status < 200 || status >= 300 || isJSONBody(upstream.Header(), first) {
		body := readRest(upstream, first, errFirst)
		upstream.Close()
		if status >= 200 && status < 300 && !IsFailureBody(body) {
			return Stream{}, Fail(http.StatusBadGateway, "upstream_error", "%s returned a non-streaming response to a stream request", ctx.Spec.Name)
		}
		return Stream{}, ctx.Spec.Errors.UpstreamFailure(ctx.Spec.Name, status, upstream.Header(), body)
	}
	shaper := &eventShaper{format: format, providerName: ctx.Spec.Name, policy: ctx.Spec.Errors}
	events := newSSEStream(upstream, shaper, first, errFirst)
	return Stream{Headers: ResponseHeaders(upstream.Header()), Next: events.next, Close: upstream.Close}, nil
}

const maxErrorBodyBytes = 1 << 20

func isJSONBody(header http.Header, first []byte) bool {
	contentType := strings.ToLower(header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return false
	}
	if strings.Contains(contentType, "json") {
		return true
	}
	trimmed := bytes.TrimSpace(first)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func readRest(upstream HTTPStream, first []byte, errFirst error) []byte {
	body := append([]byte(nil), first...)
	if errFirst != nil {
		return body
	}
	for len(body) < maxErrorBodyBytes {
		chunk, errNext := upstream.Next()
		body = append(body, chunk...)
		if errNext != nil {
			break
		}
	}
	return body
}

// PrepareBody pins the upstream model to CPA's resolved model and makes the
// stream flag match the call type, preserving key order.
func PrepareBody(payload []byte, model string, stream bool) ([]byte, error) {
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() {
		return nil, errors.New("request payload must be a JSON object")
	}
	out := payload
	var errSet error
	if model = strings.TrimSpace(model); model != "" && gjson.GetBytes(out, "model").String() != model {
		if out, errSet = sjson.SetBytes(out, "model", model); errSet != nil {
			return nil, errSet
		}
	}
	if current := gjson.GetBytes(out, "stream"); (current.Type == gjson.True) != stream && (stream || current.Exists()) {
		if out, errSet = sjson.SetBytes(out, "stream", stream); errSet != nil {
			return nil, errSet
		}
	}
	return out, nil
}

func upstreamHeaders(call Call, style AuthStyle, token string, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	switch style {
	case AuthBearer:
		h.Set("Authorization", "Bearer "+token)
	case AuthXAPIKey:
		h.Set("X-Api-Key", token)
	case AuthRaw:
		h.Set("Authorization", token)
	default:
		h.Set("Authorization", "Bearer "+token)
		if call.Format == FormatClaude {
			h.Set("X-Api-Key", token)
		}
	}
	if call.Format == FormatClaude {
		version := strings.TrimSpace(call.Headers.Get("Anthropic-Version"))
		if version == "" {
			version = "2023-06-01"
		}
		h.Set("Anthropic-Version", version)
		if beta := call.Headers.Values("Anthropic-Beta"); len(beta) > 0 {
			h["Anthropic-Beta"] = append([]string(nil), beta...)
		}
	}
	if ua := strings.TrimSpace(call.Headers.Get("User-Agent")); ua != "" {
		h.Set("User-Agent", ua)
	}
	return h
}

// ResponseHeaders drops transport-level headers that must not be replayed downstream.
func ResponseHeaders(upstream http.Header) http.Header {
	if len(upstream) == 0 {
		return nil
	}
	out := http.Header{}
	for name, values := range upstream {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection", "Keep-Alive",
			"Set-Cookie", "Alt-Svc", "Strict-Transport-Security", "Vary", "Date":
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}
