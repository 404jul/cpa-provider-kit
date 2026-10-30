package kit

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/tidwall/gjson"
)

// ErrorPolicy tunes how upstream failures become client-visible errors. CPA
// uses the HTTP status for cooldowns: 401 bad credential, 403 forbidden,
// 429 quota/rate limit, 5xx transient.
type ErrorPolicy struct {
	// CodeStatus maps a provider business error code (as found in the body) to
	// an HTTP status. Return 0 to fall back to the upstream HTTP status.
	CodeStatus func(code string) int
}

// Fail builds an error carrying an HTTP status for CPA. Executors, auth
// handlers, and quota functions may return it (or wrap it).
func Fail(status int, code, format string, args ...any) error {
	return pluginabi.NewError(code, fmt.Sprintf(format, args...), status)
}

// asFailure converts any error into a pluginabi.Error; plain errors map to
// fallbackStatus.
func asFailure(err error, fallbackStatus int) *pluginabi.Error {
	if err == nil {
		return nil
	}
	var failure *pluginabi.Error
	if errors.As(err, &failure) && failure != nil {
		return failure
	}
	return pluginabi.NewError(errorCodeFor(fallbackStatus), err.Error(), fallbackStatus)
}

// IsFailureBody reports whether a 2xx JSON body is actually an error envelope.
// Recognized: {"error":{…}}, {"success":false,"code":…}, and
// {"base_resp":{"status_code":N≠0}}.
func IsFailureBody(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	root := gjson.ParseBytes(trimmed)
	if root.Get("error").IsObject() {
		return true
	}
	if success := root.Get("success"); success.Exists() && success.Type == gjson.False {
		return true
	}
	if status := root.Get("base_resp.status_code"); status.Exists() && status.Int() != 0 {
		return true
	}
	return false
}

// UpstreamFailure turns an upstream response into an error for CPA, keeping
// the provider's message and business code.
func (p ErrorPolicy) UpstreamFailure(providerName string, status int, header http.Header, body []byte) *pluginabi.Error {
	code, message := parseErrorBody(body)
	httpStatus := 0
	if code != "" && p.CodeStatus != nil {
		httpStatus = p.CodeStatus(code)
	}
	if httpStatus == 0 && status >= 400 {
		httpStatus = status
	}
	if httpStatus == 0 {
		if n, errAtoi := strconv.Atoi(code); errAtoi == nil && n >= 400 && n <= 599 {
			httpStatus = n
		} else {
			httpStatus = http.StatusBadGateway
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(truncate(body, 300)))
	}
	if message == "" {
		message = http.StatusText(httpStatus)
	}
	text := fmt.Sprintf("%s: %s", providerName, message)
	if code != "" {
		text = fmt.Sprintf("%s: %s (code %s)", providerName, message, code)
	}
	failure := pluginabi.NewError(errorCodeFor(httpStatus), text, httpStatus)
	if header != nil {
		if retry, errAtoi := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After"))); errAtoi == nil && retry > 0 {
			failure.RetryAfterSeconds = retry
		}
	}
	return failure
}

func parseErrorBody(body []byte) (code, message string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", ""
	}
	root := gjson.ParseBytes(trimmed)
	if errObj := root.Get("error"); errObj.IsObject() {
		code = errObj.Get("code").String()
		if code == "" {
			// Anthropic-style bodies put a type name here; some gateways put a numeric code.
			code = errObj.Get("type").String()
		}
		return code, errObj.Get("message").String()
	}
	if base := root.Get("base_resp"); base.IsObject() {
		return base.Get("status_code").String(), base.Get("status_msg").String()
	}
	if root.Get("code").Exists() {
		return root.Get("code").String(), firstNonEmpty(root.Get("msg").String(), root.Get("message").String())
	}
	return "", firstNonEmpty(root.Get("message").String(), root.Get("msg").String(), root.Get("error").String())
}

// TransportFailure reports an unreachable upstream as a retryable 502.
func TransportFailure(providerName string, err error) *pluginabi.Error {
	failure := pluginabi.NewError("upstream_unreachable", fmt.Sprintf("%s request failed: %v", providerName, err), http.StatusBadGateway)
	failure.Retryable = true
	return failure
}

func errorCodeFor(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusNotImplemented:
		return "unsupported_operation"
	default:
		return "upstream_error"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func truncate(b []byte, limit int) []byte {
	if len(b) <= limit {
		return b
	}
	return b[:limit]
}
