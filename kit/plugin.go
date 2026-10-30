package kit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogCreated keeps model listings stable across restarts.
const catalogCreated = 1793318400 // 2026-10-29T00:00:00Z

// Plugin serves CPA's RPC methods for one Spec. CPA may call methods concurrently.
type Plugin struct {
	spec    *Spec
	specErr error
	host    Host
	now     func() time.Time

	mu           sync.RWMutex
	cfg          Config
	resourceBase string
}

// New builds a plugin. An invalid spec is reported to CPA at registration.
func New(spec *Spec, host Host) *Plugin {
	p := &Plugin{spec: spec, host: host, now: time.Now, resourceBase: "/v0/resource/plugins/" + spec.ID}
	if p.specErr = spec.Check(); p.specErr == nil {
		p.cfg, p.specErr = parseConfig(nil, spec)
	}
	return p
}

// SetClock overrides the time source (tests).
func (p *Plugin) SetClock(now func() time.Time) { p.now = now }

func (p *Plugin) ctx(callbackID string, hostCfg pluginapi.HostConfigSummary) *Ctx {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return &Ctx{Spec: p.spec, Config: p.cfg, HostConfig: hostCfg, Now: p.now, host: p.host, callbackID: callbackID, resourceBase: p.resourceBase}
}

// Handle executes one RPC method and always returns a JSON envelope.
func (p *Plugin) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = errorEnvelope(pluginabi.NewError("plugin_panic", fmt.Sprintf("%s: %v", method, recovered), http.StatusInternalServerError))
		}
	}()
	if p.specErr != nil {
		return errorEnvelope(pluginabi.NewError("invalid_spec", p.specErr.Error()))
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return p.register(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: p.spec.ID})
	case pluginabi.MethodAuthParse:
		return call(request, p.parseAuth)
	case pluginabi.MethodAuthLoginStart:
		return call(request, p.startLogin)
	case pluginabi.MethodAuthLoginPoll:
		return call(request, p.pollLogin)
	case pluginabi.MethodAuthRefresh:
		return call(request, p.refreshAuth)
	case pluginabi.MethodModelStatic:
		return okEnvelope(pluginapi.ModelResponse{Provider: p.spec.ID, Models: p.catalog(p.ctx("", pluginapi.HostConfigSummary{}), nil)})
	case pluginabi.MethodModelForAuth:
		return call(request, p.modelsForAuth)
	case pluginabi.MethodExecutorExecute:
		return call(request, p.execute)
	case pluginabi.MethodExecutorExecuteStream:
		return call(request, p.executeStream)
	case pluginabi.MethodExecutorCountTokens:
		return call(request, p.countTokens)
	case pluginabi.MethodExecutorHTTPRequest:
		return errorEnvelope(pluginabi.NewError("unsupported_operation", p.spec.Name+" does not proxy raw HTTP requests", http.StatusNotImplemented))
	case pluginabi.MethodManagementRegister:
		return call(request, p.registerManagement)
	case pluginabi.MethodManagementHandle:
		return call(request, p.handleResource)
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{p.spec.ID}, DisplayName: p.spec.Name})
	case pluginabi.MethodQuotaFetch:
		return call(request, p.fetchQuota)
	case pluginabi.MethodQuotaReset:
		return errorEnvelope(pluginabi.NewError("unsupported_operation", p.spec.Name+" quota resets on its own schedule", http.StatusNotImplemented))
	default:
		return errorEnvelope(pluginabi.NewError("unknown_method", "unknown method: "+method))
	}
}

// call decodes a request, runs fn, and wraps its result or error.
func call[Req, Resp any](request []byte, fn func(Req) (Resp, error)) []byte {
	var req Req
	if len(request) > 0 {
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return errorEnvelope(pluginabi.NewError("invalid_request", "decode request: "+errDecode.Error(), http.StatusBadRequest))
		}
	}
	resp, errCall := fn(req)
	if errCall != nil {
		return errorEnvelope(asFailure(errCall, http.StatusBadGateway))
	}
	return okEnvelope(resp)
}

func (p *Plugin) register(request []byte) []byte {
	var req lifecycleRequest
	if len(request) > 0 {
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return errorEnvelope(pluginabi.NewError("invalid_request", errDecode.Error()))
		}
	}
	cfg, errConfig := parseConfig(req.ConfigYAML, p.spec)
	if errConfig != nil {
		return errorEnvelope(pluginabi.NewError("invalid_config", errConfig.Error()))
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	formats := make([]string, 0, len(p.spec.Executor.Formats()))
	for _, format := range p.spec.Executor.Formats() {
		formats = append(formats, format.declared())
	}
	return okEnvelope(registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name: p.spec.Name, Version: p.spec.Version, Author: p.spec.Author,
			GitHubRepository: p.spec.Repository, Logo: p.spec.Logo, ConfigFields: builtinConfigFields(p.spec),
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         len(p.resourceHandlers()) > 0,
			QuotaProvider:         p.spec.Quota != nil,
		},
	})
}

// credentialFrom decodes an auth file; CPA passes the merged file as storage
// and the same keys as metadata.
func credentialFrom(storage []byte, metadata map[string]any) (Credential, error) {
	cred := Credential{Storage: storage}
	if len(storage) > 0 {
		if errDecode := json.Unmarshal(storage, &cred.Fields); errDecode != nil {
			return Credential{}, fmt.Errorf("decode auth file: %w", errDecode)
		}
	}
	if len(cred.Fields) == 0 && len(metadata) > 0 {
		cred.Fields = metadata
		cred.Storage, _ = json.Marshal(metadata)
	}
	cred.APIKey = cred.String("api_key")
	return cred, nil
}

// authData renders fields as an auth record. Metadata mirrors the whole file so
// host-managed keys (disabled, prefix, priority, excluded_models, …) survive re-saves.
func (p *Plugin) authData(ctx *Ctx, fileName string, fields map[string]any) (pluginapi.AuthData, AuthInfo, error) {
	metadata := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		metadata[key] = value
	}
	metadata["type"] = p.spec.ID
	raw, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return pluginapi.AuthData{}, AuthInfo{}, errMarshal
	}
	cred, _ := credentialFrom(raw, nil)
	info, errDescribe := p.spec.Auth.Describe(ctx, cred)
	if errDescribe != nil {
		return pluginapi.AuthData{}, AuthInfo{}, errDescribe
	}
	return pluginapi.AuthData{Provider: p.spec.ID, FileName: fileName, Label: info.Label, StorageJSON: raw,
		Metadata: metadata, NextRefreshAfter: info.NextRefresh}, info, nil
}

func (p *Plugin) parseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var fields map[string]any
	if json.Unmarshal(req.RawJSON, &fields) != nil {
		return pluginapi.AuthParseResponse{}, nil
	}
	if fileType, _ := fields["type"].(string); !strings.EqualFold(strings.TrimSpace(fileType), p.spec.ID) {
		return pluginapi.AuthParseResponse{}, nil
	}
	data, _, errData := p.authData(p.ctx("", req.Host), req.FileName, fields)
	if errData != nil {
		return pluginapi.AuthParseResponse{}, pluginabi.NewError("invalid_auth", fmt.Sprintf("%s: %v", req.FileName, errData))
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: data}, nil
}

func (p *Plugin) startLogin(req authLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	start, errStart := p.spec.Auth.StartLogin(p.ctx(req.HostCallbackID, req.Host), req.BaseURL)
	if errStart != nil {
		return pluginapi.AuthLoginStartResponse{}, errStart
	}
	return pluginapi.AuthLoginStartResponse{Provider: p.spec.ID, URL: start.URL, State: start.State, ExpiresAt: start.ExpiresAt, Metadata: start.Metadata}, nil
}

func (p *Plugin) pollLogin(req authLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	ctx := p.ctx(req.HostCallbackID, req.Host)
	result, errPoll := p.spec.Auth.PollLogin(ctx, req.State, req.Metadata)
	if errPoll != nil {
		return pluginapi.AuthLoginPollResponse{}, errPoll
	}
	resp := pluginapi.AuthLoginPollResponse{Status: result.Status, Message: result.Message}
	if result.Status == pluginapi.AuthLoginStatusSuccess {
		if result.Credential == nil {
			return pluginapi.AuthLoginPollResponse{}, errors.New("sign-in succeeded without a credential")
		}
		data, _, errData := p.authData(ctx, result.Credential.FileName, result.Credential.Fields)
		if errData != nil {
			return pluginapi.AuthLoginPollResponse{}, errData
		}
		resp.Auth = data
	}
	return resp, nil
}

func (p *Plugin) refreshAuth(req authRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	ctx := p.ctx(req.HostCallbackID, req.Host)
	cred, errCred := credentialFrom(req.StorageJSON, req.Metadata)
	if errCred != nil {
		return pluginapi.AuthRefreshResponse{}, errCred
	}
	fields, errRefresh := p.spec.Auth.Refresh(ctx, cred)
	if errRefresh != nil {
		return pluginapi.AuthRefreshResponse{}, asFailure(errRefresh, http.StatusUnauthorized)
	}
	data, info, errData := p.authData(ctx, "", fields)
	if errData != nil {
		return pluginapi.AuthRefreshResponse{}, asFailure(errData, http.StatusUnauthorized)
	}
	data.ID = req.AuthID
	return pluginapi.AuthRefreshResponse{Auth: data, NextRefreshAfter: info.NextRefresh}, nil
}

func (p *Plugin) modelsForAuth(req authModelRequest) (pluginapi.ModelResponse, error) {
	ctx := p.ctx(req.HostCallbackID, req.Host)
	var discovered []Model
	if p.spec.ListModels != nil {
		cred, errCred := credentialFrom(req.StorageJSON, req.Metadata)
		if errCred == nil {
			discovered, errCred = p.spec.ListModels(ctx, cred)
		}
		if errCred != nil {
			ctx.Log("warn", "model discovery failed; using static catalog", map[string]any{"auth_id": req.AuthID, "error": errCred.Error()})
			discovered = nil
		}
	}
	return pluginapi.ModelResponse{Provider: p.spec.ID, Models: p.catalog(ctx, discovered)}, nil
}

// catalog merges static models, discovered models, and extra-models.
func (p *Plugin) catalog(ctx *Ctx, discovered []Model) []pluginapi.ModelInfo {
	seen := map[string]bool{}
	var out []pluginapi.ModelInfo
	add := func(model Model) {
		if model.ID == "" || seen[model.ID] {
			return
		}
		seen[model.ID] = true
		out = append(out, p.modelInfo(model))
	}
	for _, model := range p.spec.Models {
		add(model)
	}
	for _, model := range discovered {
		add(model)
	}
	for _, id := range ctx.Config.ExtraModels {
		add(Model{ID: id, Description: "Additional model from plugin config."})
	}
	return out
}

func (p *Plugin) modelInfo(model Model) pluginapi.ModelInfo {
	inputs := model.Inputs
	if len(inputs) == 0 {
		inputs = []string{"text"}
	}
	displayName := model.DisplayName
	if displayName == "" {
		displayName = model.ID
	}
	return pluginapi.ModelInfo{
		ID: model.ID, Object: "model", Created: catalogCreated, OwnedBy: p.spec.ID, Type: "chat",
		DisplayName: displayName, Name: model.ID, Description: model.Description,
		InputTokenLimit: model.ContextLength, OutputTokenLimit: model.MaxOutput,
		ContextLength: model.ContextLength, MaxCompletionTokens: model.MaxOutput,
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   append([]string(nil), inputs...),
		SupportedOutputModalities:  []string{"text"},
		Thinking:                   model.Thinking,
		// Without declared thinking support, CPA forwards client settings unvalidated.
		UserDefined: model.Thinking == nil,
	}
}

func (p *Plugin) prepareCall(req executorRequest) (*Ctx, Call, error) {
	ctx := p.ctx(req.HostCallbackID, pluginapi.HostConfigSummary{})
	format := Format(req.Format)
	if !slices.Contains(p.spec.Executor.Formats(), format) {
		return nil, Call{}, Fail(http.StatusBadRequest, "unsupported_format", "%s cannot serve format %s", p.spec.Name, req.Format)
	}
	cred, errCred := credentialFrom(req.StorageJSON, req.AuthMetadata)
	if errCred != nil {
		return nil, Call{}, Fail(http.StatusUnauthorized, "invalid_api_key", "%v", errCred)
	}
	headers := req.Headers
	if headers == nil {
		headers = http.Header{}
	}
	return ctx, Call{Format: format, Model: req.Model, Payload: req.Payload, Headers: headers, Credential: cred}, nil
}

func (p *Plugin) execute(req executorRequest) (pluginapi.ExecutorResponse, error) {
	ctx, c, errPrepare := p.prepareCall(req)
	if errPrepare != nil {
		return pluginapi.ExecutorResponse{}, errPrepare
	}
	resp, errExec := p.spec.Executor.Execute(ctx, c)
	if errExec != nil {
		return pluginapi.ExecutorResponse{}, errExec
	}
	return pluginapi.ExecutorResponse{Payload: resp.Payload, Headers: resp.Headers}, nil
}

func (p *Plugin) countTokens(req executorRequest) (pluginapi.ExecutorResponse, error) {
	ctx, c, errPrepare := p.prepareCall(req)
	if errPrepare != nil {
		return pluginapi.ExecutorResponse{}, errPrepare
	}
	resp, errExec := p.spec.Executor.CountTokens(ctx, c)
	if errExec != nil {
		return pluginapi.ExecutorResponse{}, errExec
	}
	return pluginapi.ExecutorResponse{Payload: resp.Payload, Headers: resp.Headers}, nil
}

func (p *Plugin) executeStream(req executorRequest) (executorStreamResponse, error) {
	ctx, c, errPrepare := p.prepareCall(req)
	if errPrepare != nil {
		return executorStreamResponse{}, errPrepare
	}
	stream, errExec := p.spec.Executor.ExecuteStream(ctx, c)
	if errExec != nil {
		return executorStreamResponse{}, errExec
	}
	if stream.Next == nil {
		return executorStreamResponse{}, errors.New("executor returned a stream without Next")
	}
	if stream.Close == nil {
		stream.Close = func() {}
	}
	if req.StreamID == "" {
		chunks, failure := collect(stream)
		if failure != nil && len(chunks) == 0 {
			return executorStreamResponse{}, failure
		}
		return executorStreamResponse{Headers: stream.Headers, Chunks: chunks}, nil
	}
	go p.pump(req.StreamID, stream)
	return executorStreamResponse{Headers: stream.Headers}, nil
}

// pump forwards events through host.stream.emit until the stream ends, fails,
// or CPA stops accepting chunks (client disconnect).
func (p *Plugin) pump(streamID string, stream Stream) {
	var failure *pluginabi.Error
	defer func() {
		if recovered := recover(); recovered != nil {
			failure = pluginabi.NewError("plugin_panic", fmt.Sprintf("stream: %v", recovered), http.StatusInternalServerError)
		}
		stream.Close()
		p.host.StreamClose(streamID, failure)
	}()
	for {
		event, errNext := stream.Next()
		if errNext != nil {
			if !errors.Is(errNext, io.EOF) {
				failure = asFailure(errNext, http.StatusBadGateway)
			}
			return
		}
		if len(event) == 0 {
			continue
		}
		if p.host.StreamEmit(streamID, event) != nil {
			return
		}
	}
}

func collect(stream Stream) ([]pluginapi.ExecutorStreamChunk, *pluginabi.Error) {
	defer stream.Close()
	var chunks []pluginapi.ExecutorStreamChunk
	for {
		event, errNext := stream.Next()
		if errNext != nil {
			if errors.Is(errNext, io.EOF) {
				return chunks, nil
			}
			return chunks, asFailure(errNext, http.StatusBadGateway)
		}
		if len(event) > 0 {
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: event})
		}
	}
}

func (p *Plugin) resourceHandlers() []ResourceHandler {
	handlers := slices.Clone(p.spec.Resources)
	if handler, ok := p.spec.Auth.(ResourceHandler); ok {
		handlers = append(handlers, handler)
	}
	return handlers
}

func (p *Plugin) registerManagement(req pluginapi.ManagementRegistrationRequest) (managementRegistrationResponse, error) {
	if base := strings.TrimRight(strings.TrimSpace(req.ResourceBasePath), "/"); base != "" {
		p.mu.Lock()
		p.resourceBase = base
		p.mu.Unlock()
	}
	var routes []pluginapi.ResourceRoute
	for _, handler := range p.resourceHandlers() {
		routes = append(routes, handler.Resources()...)
	}
	return managementRegistrationResponse{Resources: routes}, nil
}

// handleResource dispatches to the handler owning the longest matching route.
func (p *Plugin) handleResource(req managementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method != "" && req.Method != http.MethodGet {
		return jsonResponse(http.StatusMethodNotAllowed, submitResult{Message: "method not allowed"}), nil
	}
	var owner ResourceHandler
	best := -1
	for _, handler := range p.resourceHandlers() {
		for _, route := range handler.Resources() {
			if strings.HasSuffix(req.Path, route.Path) && len(route.Path) > best {
				owner, best = handler, len(route.Path)
			}
		}
	}
	if owner == nil {
		return jsonResponse(http.StatusNotFound, submitResult{Message: "not found"}), nil
	}
	return owner.ServeResource(p.ctx(req.HostCallbackID, pluginapi.HostConfigSummary{}), req.ManagementRequest), nil
}

func (p *Plugin) fetchQuota(req quotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if p.spec.Quota == nil {
		return pluginapi.QuotaFetchResponse{}, Fail(http.StatusNotImplemented, "unsupported_operation", "%s does not report quota", p.spec.Name)
	}
	ctx := p.ctx(req.HostCallbackID, req.Host)
	cred, errCred := credentialFrom(req.StorageJSON, req.Metadata)
	if errCred != nil {
		return pluginapi.QuotaFetchResponse{}, Fail(http.StatusBadRequest, "invalid_auth", "%v", errCred)
	}
	resp, errQuota := p.spec.Quota(ctx, cred)
	if errQuota != nil {
		ctx.Log("warn", "quota fetch failed", map[string]any{"auth_id": req.AuthID, "error": errQuota.Error()})
	}
	return resp, errQuota
}
