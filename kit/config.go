package kit

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// Config is the plugin's section under plugins.configs.<pluginID>.
type Config struct {
	// BaseURL is the effective API origin (base-url setting or Spec.BaseURL).
	BaseURL string
	// ExtraModels are additional upstream model IDs from the extra-models setting.
	ExtraModels []string
	raw         []byte
}

// URL joins the effective base URL with an endpoint path.
func (c Config) URL(path string) string { return c.BaseURL + path }

// Decode unmarshals the plugin's YAML section into a provider-defined struct
// (use `yaml:"field-name"` tags matching Spec.ConfigFields).
func (c Config) Decode(target any) error {
	if len(strings.TrimSpace(string(c.raw))) == 0 {
		return nil
	}
	return yaml.Unmarshal(c.raw, target)
}

type builtinConfig struct {
	BaseURL     string   `yaml:"base-url"`
	ExtraModels []string `yaml:"extra-models"`
}

func parseConfig(raw []byte, spec *Spec) (Config, error) {
	cfg := Config{raw: raw}
	var parsed builtinConfig
	if errDecode := cfg.Decode(&parsed); errDecode != nil {
		return Config{}, fmt.Errorf("parse plugin config: %w", errDecode)
	}
	base := spec.BaseURL
	if strings.TrimSpace(parsed.BaseURL) != "" {
		base = parsed.BaseURL
	}
	normalized, errBase := parseBaseURL(base)
	if errBase != nil {
		return Config{}, fmt.Errorf("base-url: %w", errBase)
	}
	cfg.BaseURL = normalized
	builtin := make(map[string]bool, len(spec.Models))
	for _, model := range spec.Models {
		builtin[model.ID] = true
	}
	for _, id := range parsed.ExtraModels {
		id = strings.TrimSpace(id)
		if id != "" && !builtin[id] {
			builtin[id] = true
			cfg.ExtraModels = append(cfg.ExtraModels, id)
		}
	}
	return cfg, nil
}

func parseBaseURL(raw string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, errParse := url.Parse(base)
	if errParse != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("must be an absolute http(s) URL, got %q", raw)
	}
	return base, nil
}

func builtinConfigFields(spec *Spec) []pluginapi.ConfigField {
	fields := []pluginapi.ConfigField{
		{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Default: spec.BaseURL, Description: "API origin; endpoint paths are appended to it."},
		{Name: "extra-models", Type: pluginapi.ConfigFieldTypeArray, Default: []string{}, Description: "Additional upstream model IDs to expose."},
	}
	return append(fields, spec.ConfigFields...)
}

// Ctx carries per-call plugin state into provider code.
type Ctx struct {
	Spec   *Spec
	Config Config
	// HostConfig is CPA's host summary (auth dir, proxy) when the call provides one.
	HostConfig pluginapi.HostConfigSummary
	Now        func() time.Time

	host         Host
	callbackID   string
	resourceBase string
}

// Do performs a buffered upstream request through CPA (proxy and request logs apply).
func (c *Ctx) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return c.host.HTTPDo(c.callbackID, req)
}

// DoStream opens a streaming upstream request through CPA.
func (c *Ctx) DoStream(req pluginapi.HTTPRequest) (HTTPStream, error) {
	return c.host.HTTPDoStream(c.callbackID, req)
}

// Log writes through CPA's logger. Never pass secrets in fields.
func (c *Ctx) Log(level, message string, fields map[string]any) {
	c.host.Log(c.callbackID, level, message, fields)
}

// ResourceURL builds an absolute URL for one of the plugin's resource paths,
// using origin (scheme://host[:port]) taken from CPA's login callback URL.
func (c *Ctx) ResourceURL(origin, path string) string {
	return strings.TrimRight(origin, "/") + c.resourceBase + path
}

// ResourcePath returns the server-relative path of a resource route.
func (c *Ctx) ResourcePath(path string) string { return c.resourceBase + path }

// Origin extracts scheme://host from an absolute URL such as CPA's callback URL.
func Origin(raw string) (string, error) {
	u, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("not an absolute http(s) URL: %q", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}
