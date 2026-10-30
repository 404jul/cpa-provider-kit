package kit_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/404jul/cpa-provider-kit/kit"
	"github.com/404jul/cpa-provider-kit/kit/kittest"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const authFile = `{"type":"acme","api_key":"sk-test-1234"}`

func testSpec() *kit.Spec {
	return &kit.Spec{
		ID: "acme", Name: "Acme", Version: "0.1.0", Author: "tester", Repository: "https://github.com/example/acme",
		BaseURL: "https://api.acme.test",
		Models:  []kit.Model{{ID: "acme-1", ContextLength: 1000}},
		Auth:    &kit.APIKeyAuth{KeyURL: "https://acme.test/keys", Validate: kit.ValidateWithGET("/v1/models")},
		Executor: &kit.Passthrough{Endpoints: map[kit.Format]kit.Endpoint{
			kit.FormatChat:   {Path: "/v1/chat/completions"},
			kit.FormatClaude: {Path: "/anthropic/v1/messages", CountTokensPath: "/anthropic/v1/messages/count_tokens"},
		}},
		Errors: kit.ErrorPolicy{CodeStatus: func(code string) int {
			if code == "1308" {
				return http.StatusTooManyRequests
			}
			return 0
		}},
	}
}

func TestSpecCheckReportsMistakes(t *testing.T) {
	spec := testSpec()
	if err := spec.Check(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	spec.Version = "v1.0"
	spec.Repository = ""
	spec.Executor = &kit.Passthrough{Endpoints: map[kit.Format]kit.Endpoint{kit.FormatGemini: {Path: "/x"}}}
	err := spec.Check()
	for _, want := range []string{"Version", "Repository", "does not support format"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Check() = %v, want mention of %q", err, want)
		}
	}
	plugin := kit.New(spec, &kittest.Host{})
	if failure := kittest.Call(t, plugin, pluginabi.MethodPluginRegister, map[string]any{}, nil); failure == nil {
		t.Fatal("invalid spec must fail registration")
	}
}

func TestRegisterDeclaresCapabilitiesAndConfig(t *testing.T) {
	plugin := kit.New(testSpec(), &kittest.Host{})
	var reg struct {
		Metadata     pluginapi.Metadata `json:"metadata"`
		Capabilities map[string]any     `json:"capabilities"`
	}
	kittest.MustCall(t, plugin, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("base-url: http://127.0.0.1:9/\nextra-models: [acme-1, acme-2]\n")}, &reg)
	if reg.Metadata.Name != "Acme" || reg.Capabilities["management_api"] != true || reg.Capabilities["quota_provider"] != false {
		t.Fatalf("registration %+v", reg)
	}
	formats, _ := json.Marshal(reg.Capabilities["executor_input_formats"])
	if string(formats) != `["anthropic","chat-completions"]` {
		t.Fatalf("formats %s", formats)
	}
	var models pluginapi.ModelResponse
	kittest.MustCall(t, plugin, pluginabi.MethodModelForAuth, map[string]any{"StorageJSON": []byte(authFile)}, &models)
	if len(models.Models) != 2 || models.Models[0].ID != "acme-1" || models.Models[1].ID != "acme-2" || !models.Models[0].UserDefined {
		t.Fatalf("models %+v", models.Models)
	}
	if failure := kittest.Call(t, plugin, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("base-url: ftp://x")}, nil); failure == nil {
		t.Fatal("bad base-url must be rejected")
	}
}

func TestPassthroughRewritesModelAndMapsErrors(t *testing.T) {
	host := &kittest.Host{Do: func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if strings.Contains(string(req.Body), `"limit"`) {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"code":1308,"msg":"Usage limit reached","success":false}`)}
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"x","choices":[]}`)}
	}}
	plugin := kit.New(testSpec(), host)
	kittest.Register(t, plugin, "")
	req := kittest.Exec(kit.FormatClaude, "acme-1", `{"model":"alias","stream":true,"messages":[]}`, authFile)
	req.Headers.Set("Anthropic-Beta", "b1")
	kittest.MustCall(t, plugin, pluginabi.MethodExecutorExecute, req, nil)
	sent := host.Requests()[0]
	if sent.URL != "https://api.acme.test/anthropic/v1/messages" || string(sent.Body) != `{"model":"acme-1","stream":false,"messages":[]}` {
		t.Fatalf("sent %s %s", sent.URL, sent.Body)
	}
	if sent.Headers.Get("Authorization") != "Bearer sk-test-1234" || sent.Headers.Get("X-Api-Key") != "sk-test-1234" || sent.Headers.Get("Anthropic-Beta") != "b1" {
		t.Fatalf("headers %v", sent.Headers)
	}

	failure := kittest.Call(t, plugin, pluginabi.MethodExecutorExecute, kittest.Exec(kit.FormatChat, "acme-1", `{"model":"acme-1","limit":true}`, authFile), nil)
	if failure == nil || failure.HTTPStatus != http.StatusTooManyRequests || !strings.Contains(failure.Message, "code 1308") {
		t.Fatalf("200 envelope failure = %+v", failure)
	}
	if failure := kittest.Call(t, plugin, pluginabi.MethodExecutorExecute, kittest.Exec(kit.FormatResponses, "acme-1", `{}`, authFile), nil); failure == nil || failure.HTTPStatus != 400 {
		t.Fatalf("undeclared format must be rejected, got %+v", failure)
	}
	if failure := kittest.Call(t, plugin, pluginabi.MethodExecutorCountTokens, kittest.Exec(kit.FormatChat, "acme-1", `{}`, authFile), nil); failure == nil || failure.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("count tokens without path must be 501, got %+v", failure)
	}
}

func TestStreamsAreShapedPerFormat(t *testing.T) {
	chat := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\n: ping\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DONE]\r\n\r\n"
	claude := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}"
	var stream *kittest.Stream
	host := &kittest.Host{Stream: func(req pluginapi.HTTPRequest) *kittest.Stream {
		if strings.Contains(req.URL, "anthropic") {
			stream = kittest.SSE(claude, 5)
		} else {
			stream = kittest.SSE(chat, 3)
		}
		return stream
	}}
	plugin := kit.New(testSpec(), host)
	kittest.Register(t, plugin, "")

	var res kittest.StreamResult
	kittest.MustCall(t, plugin, pluginabi.MethodExecutorExecuteStream, kittest.Exec(kit.FormatChat, "acme-1", `{"messages":[]}`, authFile), &res)
	want := []string{`{"choices":[{"delta":{"content":"a"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`}
	if strings.Join(res.Events(), "|") != strings.Join(want, "|") || !stream.Closed() {
		t.Fatalf("chat events %q closed=%v", res.Events(), stream.Closed())
	}
	if !strings.Contains(string(host.Requests()[0].Body), `"stream":true`) {
		t.Fatalf("stream flag not forced: %s", host.Requests()[0].Body)
	}

	kittest.MustCall(t, plugin, pluginabi.MethodExecutorExecuteStream, kittest.Exec(kit.FormatClaude, "acme-1", `{"messages":[]}`, authFile), &res)
	want = []string{"event: message_start\ndata: {\"type\":\"message_start\"}\n\n", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}
	if strings.Join(res.Events(), "|") != strings.Join(want, "|") {
		t.Fatalf("claude events %q", res.Events())
	}
}

func TestStreamJSONEnvelopeOn200IsAnError(t *testing.T) {
	host := &kittest.Host{Stream: func(pluginapi.HTTPRequest) *kittest.Stream {
		return kittest.JSON(200, `{"code":1000,"msg":"Authentication Failed","success":false}`)
	}}
	spec := testSpec()
	spec.Errors.CodeStatus = func(code string) int {
		if code == "1000" {
			return http.StatusUnauthorized
		}
		return 0
	}
	plugin := kit.New(spec, host)
	kittest.Register(t, plugin, "")
	failure := kittest.Call(t, plugin, pluginabi.MethodExecutorExecuteStream, kittest.Exec(kit.FormatChat, "acme-1", `{}`, authFile), nil)
	if failure == nil || failure.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("got %+v", failure)
	}
}

func TestAPIKeyLoginFlow(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	host := &kittest.Host{Do: func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.Headers.Get("Authorization") == "Bearer bad.key" {
			return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":{"message":"invalid"}}`)}
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}
	}}
	plugin := kit.New(testSpec(), host)
	plugin.SetClock(func() time.Time { return now })
	kittest.Register(t, plugin, "")
	kittest.MustCall(t, plugin, pluginabi.MethodManagementRegister, map[string]any{"ResourceBasePath": "/v0/resource/plugins/acme-renamed"}, nil)

	var start pluginapi.AuthLoginStartResponse
	kittest.MustCall(t, plugin, pluginabi.MethodAuthLoginStart, map[string]any{"BaseURL": "http://127.0.0.1:8317/v0/management/oauth-callback"}, &start)
	u, _ := url.Parse(start.URL)
	if u.Host != "127.0.0.1:8317" || u.Path != "/v0/resource/plugins/acme-renamed/login" || u.Query().Get("state") != start.State {
		t.Fatalf("login URL %s", start.URL)
	}
	resource := func(path string, query url.Values) pluginapi.ManagementResponse {
		var resp pluginapi.ManagementResponse
		kittest.MustCall(t, plugin, pluginabi.MethodManagementHandle, map[string]any{"Method": "GET", "Path": "/v0/resource/plugins/acme-renamed" + path, "Query": query}, &resp)
		return resp
	}
	if page := resource("/login", url.Values{"state": {start.State}}); page.StatusCode != 200 || !strings.Contains(string(page.Body), "Connect Acme") {
		t.Fatalf("page %d", page.StatusCode)
	}
	if bad := resource("/login/submit", url.Values{"state": {start.State}, "api_key": {"bad.key"}}); bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rejected key status %d", bad.StatusCode)
	}
	var poll pluginapi.AuthLoginPollResponse
	kittest.MustCall(t, plugin, pluginabi.MethodAuthLoginPoll, map[string]any{"State": start.State}, &poll)
	if poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("expected pending after rejected key, got %+v", poll)
	}
	if ok := resource("/login/submit", url.Values{"state": {start.State}, "api_key": {"good.key.5678"}, "label": {"Work"}}); ok.StatusCode != 200 {
		t.Fatalf("submit %d %s", ok.StatusCode, ok.Body)
	}
	kittest.MustCall(t, plugin, pluginabi.MethodAuthLoginPoll, map[string]any{"State": start.State}, &poll)
	var stored map[string]any
	_ = json.Unmarshal(poll.Auth.StorageJSON, &stored)
	if poll.Status != pluginapi.AuthLoginStatusSuccess || poll.Auth.Label != "Work" || stored["type"] != "acme" || stored["api_key"] != "good.key.5678" ||
		!strings.HasPrefix(poll.Auth.FileName, "acme-") {
		t.Fatalf("poll %+v stored %v", poll, stored)
	}
	kittest.MustCall(t, plugin, pluginabi.MethodAuthLoginPoll, map[string]any{"State": start.State}, &poll)
	if poll.Status != pluginapi.AuthLoginStatusError {
		t.Fatal("completed sign-in must not be reusable")
	}
	now = now.Add(time.Hour)
	if page := resource("/login", url.Values{"state": {start.State}}); page.StatusCode != http.StatusGone {
		t.Fatalf("used state page %d", page.StatusCode)
	}
}

func TestParseAuthClaimsOnlyOwnFiles(t *testing.T) {
	plugin := kit.New(testSpec(), &kittest.Host{})
	var resp pluginapi.AuthParseResponse
	kittest.MustCall(t, plugin, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{FileName: "c.json", RawJSON: []byte(`{"type":"codex","api_key":"x"}`)}, &resp)
	if resp.Handled {
		t.Fatal("foreign file must be left to other parsers")
	}
	if failure := kittest.Call(t, plugin, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{FileName: "a.json", RawJSON: []byte(`{"type":"acme"}`)}, nil); failure == nil {
		t.Fatal("own file without api_key must be rejected")
	}
	kittest.MustCall(t, plugin, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{FileName: "a.json", RawJSON: []byte(`{"type":"ACME","api_key":"sk-abcdefgh","plan":"pro","prefix":"a"}`)}, &resp)
	if !resp.Handled || resp.Auth.Label != "Acme (pro) …efgh" || resp.Auth.Metadata["prefix"] != "a" || resp.Auth.Provider != "acme" {
		t.Fatalf("parse %+v", resp)
	}
}

func TestQuotaAndModelDiscovery(t *testing.T) {
	spec := testSpec()
	spec.Quota = func(ctx *kit.Ctx, cred kit.Credential) (pluginapi.QuotaFetchResponse, error) {
		if cred.APIKey != "sk-test-1234" {
			return pluginapi.QuotaFetchResponse{}, kit.Fail(401, "invalid_api_key", "bad key")
		}
		return pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "pro"}}, nil
	}
	spec.ListModels = func(*kit.Ctx, kit.Credential) ([]kit.Model, error) { return nil, errors.New("offline") }
	host := &kittest.Host{}
	plugin := kit.New(spec, host)
	kittest.Register(t, plugin, "")
	var quota pluginapi.QuotaFetchResponse
	kittest.MustCall(t, plugin, pluginabi.MethodQuotaFetch, map[string]any{"storage_json": []byte(authFile)}, &quota)
	if quota.Subscription == nil || quota.Subscription.Plan != "pro" {
		t.Fatalf("quota %+v", quota)
	}
	var models pluginapi.ModelResponse
	kittest.MustCall(t, plugin, pluginabi.MethodModelForAuth, map[string]any{"StorageJSON": []byte(authFile)}, &models)
	if len(models.Models) != 1 || len(host.Logs()) == 0 {
		t.Fatalf("discovery failure must fall back and log: %+v %v", models.Models, host.Logs())
	}
}
