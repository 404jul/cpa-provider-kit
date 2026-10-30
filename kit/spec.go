// Package kit turns a declarative provider Spec into a complete CLIProxyAPI
// native plugin: auth files and sign-in, model catalog, executor, streaming,
// quota, and the RPC dispatch CPA expects. Package abi exports it over the C ABI.
package kit

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Format is a CPA protocol identifier (sdk/translator/formats.go).
type Format string

const (
	// FormatChat is OpenAI Chat Completions.
	FormatChat Format = "openai"
	// FormatClaude is Anthropic Messages.
	FormatClaude Format = "claude"
	// FormatResponses is OpenAI Responses.
	FormatResponses Format = "openai-response"
	// FormatGemini is Gemini generateContent.
	FormatGemini Format = "gemini"
)

// declared returns the capability name CPA accepts in executor_input_formats.
func (f Format) declared() string {
	switch f {
	case FormatChat:
		return "chat-completions"
	case FormatClaude:
		return "anthropic"
	case FormatResponses:
		return "responses"
	default:
		return string(f)
	}
}

// Spec declares one provider. Everything except ID, Name, Version, Author,
// Repository, BaseURL, Auth, and Executor is optional.
type Spec struct {
	// ID is the provider key: auth file "type", executor and quota identifier.
	// Keep it equal to the library file name (the plugin ID CPA derives).
	ID string
	// Name, Version, Author, Repository, and Logo are shown in CPA's plugin list.
	// CPA rejects plugins without Name, Version, Author, and Repository.
	Name       string
	Version    string
	Author     string
	Repository string
	Logo       string

	// BaseURL is the default API origin; users can override it with the
	// base-url plugin setting. Endpoint paths are appended to it.
	BaseURL string
	// ConfigFields are provider-specific settings, appended after the built-in
	// base-url and extra-models fields. Read them with Config.Decode.
	ConfigFields []pluginapi.ConfigField

	// Models is the static catalog served for every credential.
	Models []Model
	// ListModels optionally discovers models per credential. On error the
	// static catalog is used and the failure is logged.
	ListModels func(ctx *Ctx, cred Credential) ([]Model, error)

	// Auth adds, parses, and refreshes credentials. Use *APIKeyAuth for
	// API-key providers or implement AuthHandler for OAuth/device flows.
	Auth AuthHandler
	// Executor sends requests upstream. Use *Passthrough for providers that
	// natively speak OpenAI/Anthropic protocols or implement Executor.
	Executor Executor
	// Quota optionally reports plan usage in CPA's quota view.
	Quota func(ctx *Ctx, cred Credential) (pluginapi.QuotaFetchResponse, error)
	// Errors tunes how upstream failures map to HTTP statuses.
	Errors ErrorPolicy
	// Resources are extra browser pages served under /v0/resource/plugins/<id>/.
	Resources []ResourceHandler
}

// Model describes one catalog entry.
type Model struct {
	ID          string
	DisplayName string
	Description string
	// ContextLength and MaxOutput are token limits; zero means unknown.
	ContextLength int64
	MaxOutput     int64
	// Inputs lists input modalities; empty means text only.
	Inputs []string
	// Thinking declares reasoning controls CPA validates and converts. Nil
	// forwards client thinking/effort settings to the upstream unchanged.
	Thinking *pluginapi.ThinkingSupport
}

// Credential is a parsed auth file.
type Credential struct {
	// Storage is the raw auth file JSON.
	Storage []byte
	// Fields is the decoded auth file.
	Fields map[string]any
	// APIKey is Fields["api_key"], trimmed.
	APIKey string
}

// String returns a string field from the auth file.
func (c Credential) String(key string) string {
	value, _ := c.Fields[key].(string)
	return strings.TrimSpace(value)
}

// AuthInfo is what CPA shows for a credential.
type AuthInfo struct {
	Label string
	// NextRefresh schedules Refresh; zero means never (e.g. static API keys).
	NextRefresh time.Time
}

// LoginStart is returned to CPA when a sign-in begins.
type LoginStart struct {
	URL       string
	State     string
	ExpiresAt time.Time
	// Metadata is stored with the session and passed back to PollLogin.
	// Set "user_code" (and "flow": "device") to show a device code.
	Metadata map[string]any
}

// LoginResult is one poll outcome. Credential is set on success.
type LoginResult struct {
	Status     pluginapi.AuthLoginStatus
	Message    string
	Credential *NewCredential
}

// NewCredential is a credential produced by a sign-in. The kit adds "type".
type NewCredential struct {
	FileName string
	Fields   map[string]any
}

// AuthHandler implements a provider's credential lifecycle.
type AuthHandler interface {
	// Describe validates a parsed auth file and labels it. Returning an error
	// marks the file as invalid.
	Describe(ctx *Ctx, cred Credential) (AuthInfo, error)
	// StartLogin begins an interactive sign-in. callbackURL is CPA's OAuth
	// callback (http://127.0.0.1:<port>/…/oauth-callback); its origin also
	// serves the plugin's resources (ctx.ResourceURL).
	StartLogin(ctx *Ctx, callbackURL string) (LoginStart, error)
	// PollLogin reports progress for a sign-in started by StartLogin.
	PollLogin(ctx *Ctx, state string, metadata map[string]any) (LoginResult, error)
	// Refresh renews a credential (e.g. OAuth tokens) and returns its new fields.
	Refresh(ctx *Ctx, cred Credential) (map[string]any, error)
}

// Executor sends requests upstream.
type Executor interface {
	// Formats lists the protocols served natively; CPA translates the rest.
	Formats() []Format
	Execute(ctx *Ctx, call Call) (Response, error)
	// ExecuteStream returns a stream whose events are already shaped for
	// format: bare JSON per chunk for FormatChat, complete SSE frames
	// ("event: …\ndata: …\n\n") for FormatClaude and FormatResponses.
	ExecuteStream(ctx *Ctx, call Call) (Stream, error)
	CountTokens(ctx *Ctx, call Call) (Response, error)
}

// Call is one model request routed to the executor.
type Call struct {
	Format Format
	// Model is the upstream model ID after CPA resolved aliases and prefixes.
	Model string
	// Payload is the request body in Format.
	Payload []byte
	// Headers are the client's request headers.
	Headers    http.Header
	Credential Credential
}

// Response is a buffered executor result.
type Response struct {
	Payload []byte
	Headers http.Header
}

// Stream is a streaming executor result.
type Stream struct {
	Headers http.Header
	// Next returns the next event; io.EOF ends the stream cleanly.
	Next func() ([]byte, error)
	// Close releases upstream resources; called exactly once.
	Close func()
}

// ResourceHandler serves unauthenticated GET pages under /v0/resource/plugins/<id>/.
type ResourceHandler interface {
	// Resources lists the routes, with paths relative to the plugin prefix.
	Resources() []pluginapi.ResourceRoute
	// ServeResource handles a request for one of those routes.
	ServeResource(ctx *Ctx, req pluginapi.ManagementRequest) pluginapi.ManagementResponse
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*([-+][0-9A-Za-z.-]+)?$`)

// Check reports spec mistakes CPA would otherwise reject at load time.
func (s *Spec) Check() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if !idPattern.MatchString(s.ID) {
		add("ID %q must match %s", s.ID, idPattern)
	}
	for _, field := range []struct{ name, value string }{{"Name", s.Name}, {"Author", s.Author}, {"Repository", s.Repository}} {
		if strings.TrimSpace(field.value) == "" {
			add("%s is required", field.name)
		}
	}
	if !versionPattern.MatchString(s.Version) {
		add("Version %q must be a dotted number without a leading v", s.Version)
	}
	if _, errBase := parseBaseURL(s.BaseURL); errBase != nil {
		add("BaseURL: %v", errBase)
	}
	if s.Auth == nil {
		add("Auth is required")
	}
	if s.Executor == nil {
		add("Executor is required")
	} else if len(s.Executor.Formats()) == 0 {
		add("Executor must serve at least one format")
	}
	if p, ok := s.Executor.(*Passthrough); ok {
		if errPass := p.check(); errPass != nil {
			add("%v", errPass)
		}
	}
	seen := map[string]bool{}
	for _, model := range s.Models {
		if strings.TrimSpace(model.ID) == "" || seen[model.ID] {
			add("model IDs must be unique and non-empty (%q)", model.ID)
		}
		seen[model.ID] = true
	}
	if len(problems) > 0 {
		return errors.New("invalid provider spec: " + strings.Join(problems, "; "))
	}
	return nil
}
