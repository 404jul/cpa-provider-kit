package kit

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ErrKeyRejected tells APIKeyAuth the upstream refused a submitted key.
var ErrKeyRejected = errors.New("API key rejected")

// KeyInfo is what a key validation learned about the account.
type KeyInfo struct {
	// Plan is shown in the credential label, e.g. "pro".
	Plan string
	// Fields are extra values stored in the auth file.
	Fields map[string]any
}

// APIKeyAuth is a ready-made AuthHandler for providers authenticated by a
// static API key. Sign-in opens a page served by CPA where the user pastes a
// key; auth files look like {"type":"<id>","api_key":"…","label":"…"}.
type APIKeyAuth struct {
	// KeyURL is where users create keys; linked from the sign-in page.
	KeyURL string
	// Hint is one sentence of guidance shown on the sign-in page.
	Hint string
	// Validate optionally checks a submitted key. Return an error wrapping
	// ErrKeyRejected to refuse it; other errors are logged and the key is kept.
	Validate func(ctx *Ctx, key string) (KeyInfo, error)

	once   sync.Once
	logins *loginStore
}

const (
	loginTTL        = 15 * time.Minute
	loginPagePath   = "/login"
	loginSubmitPath = "/login/submit"
)

func (a *APIKeyAuth) store() *loginStore {
	a.once.Do(func() { a.logins = &loginStore{sessions: make(map[string]*loginSession)} })
	return a.logins
}

// Describe implements AuthHandler.
func (a *APIKeyAuth) Describe(ctx *Ctx, cred Credential) (AuthInfo, error) {
	if cred.APIKey == "" {
		return AuthInfo{}, errors.New("auth file has no api_key")
	}
	if label := cred.String("label"); label != "" {
		return AuthInfo{Label: label}, nil
	}
	label := ctx.Spec.Name
	if plan := cred.String("plan"); plan != "" {
		label += " (" + plan + ")"
	}
	if len(cred.APIKey) >= 8 {
		label += " …" + cred.APIKey[len(cred.APIKey)-4:]
	}
	return AuthInfo{Label: label}, nil
}

// Refresh implements AuthHandler; static keys have nothing to renew.
func (a *APIKeyAuth) Refresh(_ *Ctx, cred Credential) (map[string]any, error) {
	return cred.Fields, nil
}

// StartLogin implements AuthHandler.
func (a *APIKeyAuth) StartLogin(ctx *Ctx, callbackURL string) (LoginStart, error) {
	origin, errOrigin := Origin(callbackURL)
	if errOrigin != nil {
		return LoginStart{}, errOrigin
	}
	state, expires, errCreate := a.store().create(ctx.Now())
	if errCreate != nil {
		return LoginStart{}, errCreate
	}
	return LoginStart{URL: ctx.ResourceURL(origin, loginPagePath) + "?state=" + state, State: state, ExpiresAt: expires}, nil
}

// PollLogin implements AuthHandler.
func (a *APIKeyAuth) PollLogin(ctx *Ctx, state string, _ map[string]any) (LoginResult, error) {
	cred, ok := a.store().poll(state, ctx.Now())
	switch {
	case !ok:
		return LoginResult{Status: pluginapi.AuthLoginStatusError, Message: "sign-in expired; start it again"}, nil
	case cred == nil:
		return LoginResult{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for the API key in the browser"}, nil
	default:
		return LoginResult{Status: pluginapi.AuthLoginStatusSuccess, Credential: cred}, nil
	}
}

// Resources implements ResourceHandler.
func (a *APIKeyAuth) Resources() []pluginapi.ResourceRoute {
	return []pluginapi.ResourceRoute{
		{Path: loginPagePath, Description: "API key sign-in page (opened by the login flow)."},
		{Path: loginSubmitPath, Description: "Receives the API key for a pending sign-in."},
	}
}

// ServeResource implements ResourceHandler. CPA serves plugin resources
// without management auth, so the random single-use state authorizes a submission.
func (a *APIKeyAuth) ServeResource(ctx *Ctx, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := req.Query.Get("state")
	if strings.HasSuffix(req.Path, loginSubmitPath) {
		return a.submit(ctx, req, state)
	}
	view := loginView{Name: ctx.Spec.Name, KeyURL: a.KeyURL, Hint: a.Hint, State: state,
		SubmitURL: ctx.ResourcePath(loginSubmitPath), Active: a.store().pending(state, ctx.Now())}
	var body strings.Builder
	if errRender := loginPage.Execute(&body, view); errRender != nil {
		return jsonResponse(http.StatusInternalServerError, submitResult{Message: errRender.Error()})
	}
	status := http.StatusOK
	if !view.Active {
		status = http.StatusGone
	}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: pageHeaders("text/html; charset=utf-8"), Body: []byte(body.String())}
}

const expiredMessage = "This sign-in link has expired or was already used. Start the sign-in again from CLIProxyAPI."

func (a *APIKeyAuth) submit(ctx *Ctx, req pluginapi.ManagementRequest, state string) pluginapi.ManagementResponse {
	key := strings.TrimSpace(req.Query.Get("api_key"))
	label := strings.TrimSpace(req.Query.Get("label"))
	if !a.store().pending(state, ctx.Now()) {
		return jsonResponse(http.StatusGone, submitResult{Message: expiredMessage})
	}
	if !validKeyShape(key) {
		return jsonResponse(http.StatusBadRequest, submitResult{Message: "Paste an API key (no spaces)."})
	}
	if runes := []rune(label); len(runes) > 120 {
		label = string(runes[:120])
	}
	var info KeyInfo
	if a.Validate != nil {
		var errValidate error
		info, errValidate = a.Validate(ctx, key)
		switch {
		case errors.Is(errValidate, ErrKeyRejected):
			return jsonResponse(http.StatusUnauthorized, submitResult{Message: ctx.Spec.Name + " rejected this API key."})
		case errValidate != nil:
			// Validation is a convenience; an outage there must not block sign-in.
			ctx.Log("warn", "API key validation inconclusive; saving key", map[string]any{"error": errValidate.Error()})
		}
	}
	fields := map[string]any{"api_key": key, "created_at": ctx.Now().UTC().Format(time.RFC3339)}
	for k, v := range info.Fields {
		fields[k] = v
	}
	if label != "" {
		fields["label"] = label
	}
	if info.Plan != "" {
		fields["plan"] = info.Plan
	}
	sum := sha256.Sum256([]byte(key))
	cred := &NewCredential{FileName: ctx.Spec.ID + "-" + hex.EncodeToString(sum[:6]) + ".json", Fields: fields}
	if !a.store().complete(state, cred, ctx.Now()) {
		return jsonResponse(http.StatusGone, submitResult{Message: expiredMessage})
	}
	ctx.Log("info", "API key accepted", map[string]any{"file": cred.FileName, "plan": info.Plan})
	return jsonResponse(http.StatusOK, submitResult{OK: true, Message: "Saved. CLIProxyAPI will finish adding the account in a few seconds; you can close this tab."})
}

// ValidateWithGET returns an APIKeyAuth.Validate that sends GET <base-url><path>
// with the key as a bearer token: 401/403 reject the key, 2xx accept it.
func ValidateWithGET(path string) func(ctx *Ctx, key string) (KeyInfo, error) {
	return func(ctx *Ctx, key string) (KeyInfo, error) {
		resp, errDo := ctx.Do(pluginapi.HTTPRequest{Method: http.MethodGet, URL: ctx.Config.URL(path),
			Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
		if errDo != nil {
			return KeyInfo{}, errDo
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return KeyInfo{}, ErrKeyRejected
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || IsFailureBody(resp.Body) {
			failure := ctx.Spec.Errors.UpstreamFailure(ctx.Spec.Name, resp.StatusCode, resp.Headers, resp.Body)
			if failure.HTTPStatus == http.StatusUnauthorized || failure.HTTPStatus == http.StatusForbidden {
				return KeyInfo{}, ErrKeyRejected
			}
			return KeyInfo{}, failure
		}
		return KeyInfo{}, nil
	}
}

func validKeyShape(key string) bool {
	if key == "" || len(key) > 1024 {
		return false
	}
	for _, r := range key {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

type loginSession struct {
	expiresAt time.Time
	cred      *NewCredential
}

type loginStore struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

func (s *loginStore) create(now time.Time) (string, time.Time, error) {
	buf := make([]byte, 24)
	if _, errRand := rand.Read(buf); errRand != nil {
		return "", time.Time{}, errRand
	}
	state := hex.EncodeToString(buf)
	expires := now.Add(loginTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	s.sessions[state] = &loginSession{expiresAt: expires}
	return state, expires, nil
}

func (s *loginStore) pending(state string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	session, ok := s.sessions[state]
	return ok && session.cred == nil
}

func (s *loginStore) complete(state string, cred *NewCredential, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.cred != nil {
		return false
	}
	session.cred = cred
	return true
}

// poll returns the completed credential once (removing the session), nil while
// pending, or ok=false when the session is unknown or expired.
func (s *loginStore) poll(state string, now time.Time) (*NewCredential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		return nil, false
	}
	if session.cred != nil {
		delete(s.sessions, state)
	}
	return session.cred, true
}

func (s *loginStore) pruneLocked(now time.Time) {
	for state, session := range s.sessions {
		if now.After(session.expiresAt) {
			delete(s.sessions, state)
		}
	}
}

type submitResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func jsonResponse(status int, result submitResult) pluginapi.ManagementResponse {
	body, _ := json.Marshal(result)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: pageHeaders("application/json"), Body: body}
}

func pageHeaders(contentType string) http.Header {
	return http.Header{
		"Content-Type":            {contentType},
		"Cache-Control":           {"no-store"},
		"Referrer-Policy":         {"no-referrer"},
		"X-Content-Type-Options":  {"nosniff"},
		"X-Frame-Options":         {"DENY"},
		"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'"},
	}
}

type loginView struct {
	Name      string
	KeyURL    string
	Hint      string
	State     string
	SubmitURL string
	Active    bool
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect {{.Name}}</title>
<style>
:root { color-scheme: light dark; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: Canvas; color: CanvasText; }
main { width: min(440px, calc(100vw - 32px)); padding: 28px; border: 1px solid color-mix(in srgb, CanvasText 18%, transparent); border-radius: 14px; }
h1 { font-size: 20px; margin: 0 0 6px; }
p { margin: 0 0 18px; line-height: 1.45; opacity: .8; font-size: 14px; }
label { display: block; font-size: 13px; font-weight: 600; margin: 14px 0 6px; }
input { box-sizing: border-box; width: 100%; padding: 10px 12px; font: inherit; font-size: 14px; border-radius: 8px; border: 1px solid color-mix(in srgb, CanvasText 30%, transparent); background: Canvas; color: CanvasText; }
button { margin-top: 20px; width: 100%; padding: 11px; font: inherit; font-weight: 600; border: 0; border-radius: 8px; background: #2563eb; color: #fff; cursor: pointer; }
button:disabled { opacity: .6; cursor: progress; }
#status { margin-top: 16px; font-size: 14px; min-height: 1.4em; }
.ok { color: #16a34a; } .err { color: #dc2626; }
a { color: inherit; }
</style>
</head>
<body>
<main>
<h1>Connect {{.Name}}</h1>
{{if .Active}}
<p>{{if .Hint}}{{.Hint}} {{end}}{{if .KeyURL}}Create a key at <a href="{{.KeyURL}}" target="_blank" rel="noopener noreferrer">{{.KeyURL}}</a>.{{end}}</p>
<form id="form" autocomplete="off">
<label for="api_key">API key</label>
<input id="api_key" name="api_key" type="password" required spellcheck="false" autofocus>
<label for="label">Label (optional)</label>
<input id="label" name="label" type="text" maxlength="120" placeholder="e.g. Work account">
<button id="submit" type="submit">Connect</button>
</form>
<div id="status" role="status"></div>
<script>
const form = document.getElementById("form");
const status = document.getElementById("status");
const button = document.getElementById("submit");
form.addEventListener("submit", async (event) => {
  event.preventDefault();
  button.disabled = true;
  status.className = "";
  status.textContent = "Checking key…";
  const params = new URLSearchParams({ state: {{.State}}, api_key: form.api_key.value.trim(), label: form.label.value.trim() });
  try {
    const response = await fetch({{.SubmitURL}} + "?" + params.toString(), { cache: "no-store", credentials: "omit" });
    const result = await response.json();
    status.className = result.ok ? "ok" : "err";
    status.textContent = result.message;
    if (result.ok) { form.remove(); return; }
  } catch (error) {
    status.className = "err";
    status.textContent = "Could not reach CLIProxyAPI: " + error;
  }
  button.disabled = false;
});
</script>
{{else}}
<p>This sign-in link has expired or was already used. Start the sign-in again from CLIProxyAPI.</p>
{{end}}
</main>
</body>
</html>
`))
