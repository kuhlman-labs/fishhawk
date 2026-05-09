package server

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

// manifestStateCookieName isolates the manifest-flow state cookie
// from the OAuth sign-in state cookie so a sign-in flow in another
// tab can't be confused for a manifest-flow callback.
const manifestStateCookieName = "fishhawk_manifest_state"

// handleGitHubManifestFlowStart implements
// GET /v0/auth/github/manifest-flow-start.
//
// Mints a state value, stores it in a short-lived browser cookie,
// and renders an HTML page with a self-submitting form pointing at
// GitHub's manifest-flow endpoint. The form carries the rendered
// App manifest as a hidden input and round-trips the state via the
// query string GitHub will preserve in its callback redirect.
//
// Required query parameters:
//
//	backend_url   absolute base URL of fishhawkd (e.g. http://localhost:8080)
//	webhook_url   destination GitHub will deliver webhooks to (e.g. a smee.io URL for local dev)
//
// Optional query parameters:
//
//	owner         GitHub user or org login. When set, the form posts to
//	              /organizations/<owner>/settings/apps/new instead of
//	              /settings/apps/new (which targets the operator's
//	              personal account).
//	name          App name override. Defaults to "Fishhawk".
func (s *Server) handleGitHubManifestFlowStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GitHubManifest == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "manifest_unconfigured",
			"manifest flow not configured", nil)
		return
	}

	backendURL := strings.TrimRight(r.URL.Query().Get("backend_url"), "/")
	webhookURL := strings.TrimRight(r.URL.Query().Get("webhook_url"), "/")
	if backendURL == "" || !strings.HasPrefix(backendURL, "http") {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"backend_url query parameter is required and must start with http(s)://", nil)
		return
	}
	if webhookURL == "" || !strings.HasPrefix(webhookURL, "http") {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"webhook_url query parameter is required (use a smee.io URL for local dev) and must start with http(s)://", nil)
		return
	}

	state, err := auth.GenerateState()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not generate manifest state", map[string]any{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     manifestStateCookieName,
		Value:    state,
		Path:     "/v0/auth/github/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(auth.StateCookieTTL),
		MaxAge:   int(auth.StateCookieTTL.Seconds()),
	})

	manifest := buildManifest(backendURL, webhookURL, r.URL.Query().Get("name"))
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not marshal manifest", map[string]any{"error": err.Error()})
		return
	}

	formAction := "https://github.com/settings/apps/new"
	if owner := r.URL.Query().Get("owner"); owner != "" {
		// Targets an org account. Empty = personal account.
		formAction = "https://github.com/organizations/" + url.PathEscape(owner) + "/settings/apps/new"
	}
	formAction += "?state=" + url.QueryEscape(state)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := manifestStartTmpl.Execute(w, manifestStartView{
		FormAction:   formAction,
		ManifestJSON: string(manifestJSON),
		WebhookURL:   webhookURL,
	}); err != nil {
		s.cfg.Logger.Error("render manifest-start page", "error", err.Error())
	}
}

// handleGitHubManifestCallback implements
// GET /v0/auth/github/manifest-callback.
//
// Verifies state against the cookie issued by manifest-flow-start,
// POSTs the code to api.github.com to fetch App credentials, and
// renders an HTML success page with the App ID, OAuth client ID,
// webhook secret, OAuth client secret, and PEM private key for
// the operator to copy out into their .env / secrets backend.
//
// Single-use: the state cookie is cleared on entry whether or not
// validation succeeds. The credentials are rendered once and then
// gone — there's no stash on the server side.
func (s *Server) handleGitHubManifestCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GitHubManifest == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "manifest_unconfigured",
			"manifest flow not configured", nil)
		return
	}

	stateParam := r.URL.Query().Get("state")
	stateCookie, cookieErr := r.Cookie(manifestStateCookieName)

	// Always clear the state cookie on entry — single-use, regardless
	// of whether validation passes.
	http.SetCookie(w, &http.Cookie{
		Name:     manifestStateCookieName,
		Value:    "",
		Path:     "/v0/auth/github/",
		HttpOnly: true,
		Secure:   true,
		MaxAge:   -1,
	})

	if cookieErr != nil || stateParam == "" || stateCookie.Value != stateParam {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"manifest state did not validate", nil)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"missing code parameter", nil)
		return
	}

	creds, err := s.cfg.GitHubManifest.Convert(r.Context(), code)
	if err != nil {
		// The code is one-shot and good for ten minutes; if we
		// failed to convert it, the operator must restart the flow.
		s.writeError(w, r, http.StatusBadGateway, "manifest_conversion_failed",
			"GitHub rejected the manifest conversion code; restart the flow at /v0/auth/github/manifest-flow-start",
			map[string]any{"error": err.Error()})
		return
	}

	s.cfg.Logger.Info("github app created via manifest flow",
		"app_id", creds.ID,
		"slug", creds.Slug,
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := manifestSuccessTmpl.Execute(w, manifestSuccessView{
		Creds:                  creds,
		OAuthCallbackURLForEnv: deriveOAuthCallbackURL(r),
	}); err != nil {
		s.cfg.Logger.Error("render manifest-success page", "error", err.Error())
	}
}

// deriveOAuthCallbackURL builds the OAuth callback URL the
// operator should put in .env. We can't introspect the App's
// callback list from here, but the manifest we just registered
// declares it, so it matches whatever was passed to the start
// endpoint as backend_url. We reconstruct it from r.Host as a
// best effort.
func deriveOAuthCallbackURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host + "/v0/auth/github/callback"
}

// buildManifest renders the GitHub App manifest payload. Mirrors
// docs/github-app/manifest.template.json but is built in Go so the
// backend can mint the state cookie and embed the dynamic URLs in
// one round-trip rather than asking the operator to render the
// template separately.
func buildManifest(backendURL, webhookURL, name string) map[string]any {
	if name == "" {
		name = "Fishhawk"
	}
	return map[string]any{
		"name":         name,
		"url":          backendURL,
		"description":  "Workflow + governance layer for AI-driven software changes. See https://github.com/kuhlman-labs/fishhawk.",
		"public":       false,
		"redirect_url": backendURL + "/v0/auth/github/manifest-callback",
		"callback_urls": []string{
			backendURL + "/v0/auth/github/callback",
		},
		"request_oauth_on_install": false,
		"setup_on_update":          false,
		"hook_attributes": map[string]any{
			"url":    webhookURL,
			"active": true,
		},
		"default_permissions": map[string]string{
			// actions:write is required to fire workflow_dispatch
			// (POST /repos/.../actions/workflows/{file}/dispatches).
			// workflows:write below is a separate scope — it lets
			// the App edit .github/workflows/*.yml files but does
			// NOT include the dispatch endpoint. Both are needed.
			"actions":       "write",
			"contents":      "write",
			"issues":        "write",
			"pull_requests": "write",
			"checks":        "write",
			"workflows":     "write",
			"members":       "read",
			"metadata":      "read",
			// administration:read lets the backend read branch
			// protection + rulesets to derive the required-checks
			// list at run-create time (ADR-017 / #249, consumed by
			// #251). Read-only — we don't manage protection from
			// Fishhawk.
			"administration": "read",
		},
		"default_events": []string{
			"issues",
			"issue_comment",
			"pull_request",
			"push",
			"workflow_run",
			"check_run",
			"check_suite",
			// #251 / ADR-017: receive notice when an upstream
			// protection edit invalidates a cached snapshot.
			// Acknowledged by MatchEvent, no run-create.
			"branch_protection_rule",
			"repository_ruleset",
		},
	}
}

type manifestStartView struct {
	FormAction   string
	ManifestJSON string
	WebhookURL   string
}

type manifestSuccessView struct {
	Creds                  *auth.ManifestCredentials
	OAuthCallbackURLForEnv string
}

// manifestStartTmpl renders an auto-submitting form. JS submits
// immediately on load; the visible button is a fallback for
// JS-disabled or paste-into-browser cases. The manifest JSON
// goes through html/template auto-escaping (it's emitted inside
// a `value=""` attribute), preventing injection from a malicious
// backend_url / webhook_url.
var manifestStartTmpl = template.Must(template.New("manifest-start").Parse(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Submit Fishhawk App manifest to GitHub</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; max-width: 720px; margin: 4rem auto; padding: 0 1rem; color: #111; }
    code { background: #f4f4f4; padding: 0.15rem 0.35rem; border-radius: 3px; }
    button { font-size: 1rem; padding: 0.6rem 1.1rem; cursor: pointer; }
    p { line-height: 1.5; }
  </style>
</head>
<body>
  <h1>Submitting App manifest to GitHub&hellip;</h1>
  <p>Webhook deliveries will go to <code>{{.WebhookURL}}</code>.</p>
  <p>If this page doesn't redirect automatically, click the button.</p>
  <form id="manifest-form" action="{{.FormAction}}" method="post">
    <input type="hidden" name="manifest" value="{{.ManifestJSON}}">
    <button type="submit">Continue to GitHub &rarr;</button>
  </form>
  <script>
    document.getElementById('manifest-form').submit();
  </script>
</body>
</html>
`))

// manifestSuccessTmpl renders the App credentials. Secrets are
// shown plaintext in <pre> blocks; the page sets Cache-Control:
// no-store so it doesn't sit in browser history. The operator
// copy-pastes the .env block, downloads/saves the PEM, and closes
// the tab.
var manifestSuccessTmpl = template.Must(template.New("manifest-success").Parse(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Fishhawk App created</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; max-width: 760px; margin: 3rem auto; padding: 0 1rem; color: #111; line-height: 1.5; }
    code { background: #f4f4f4; padding: 0.15rem 0.35rem; border-radius: 3px; }
    pre { background: #f4f4f4; padding: 0.85rem 1rem; border-radius: 4px; overflow-x: auto; font-size: 0.9rem; }
    .warn { background: #fff5b1; border-left: 4px solid #d4a017; padding: 0.6rem 0.9rem; margin: 1rem 0; }
    a { color: #0366d6; }
    h2 { margin-top: 2rem; }
  </style>
</head>
<body>
  <h1>GitHub App created</h1>
  <p><strong>{{.Creds.Name}}</strong> &middot; ID <code>{{.Creds.ID}}</code> &middot; <a href="{{.Creds.HTMLURL}}" target="_blank" rel="noopener">manage on GitHub &rarr;</a></p>

  <div class="warn">
    <strong>Secrets are shown once.</strong> Copy them now &mdash; reloading this page will not recover them. The backend does not persist them; you have to.
  </div>

  <h2>1. Save the private key</h2>
  <p>Write the PEM block below to a file readable only by your user (e.g. <code>~/.fishhawk/private-key.pem</code>) and <code>chmod 600</code>.</p>
  <pre>{{.Creds.PEM}}</pre>

  <h2>2. Populate .env</h2>
  <p>Append these lines to <code>.env</code> at the repo root (or wire them into your secrets backend for hosted deploys).</p>
  <pre># GitHub App "{{.Creds.Name}}" — created via manifest flow
FISHHAWKD_GITHUB_APP_ID={{.Creds.ID}}
FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE=&lt;path you wrote the PEM to&gt;
FISHHAWKD_GITHUB_WEBHOOK_SECRET={{.Creds.WebhookSecret}}
FISHHAWKD_OAUTH_CLIENT_ID={{.Creds.ClientID}}
FISHHAWKD_OAUTH_CLIENT_SECRET={{.Creds.ClientSecret}}
FISHHAWKD_OAUTH_CALLBACK_URL={{.OAuthCallbackURLForEnv}}
</pre>

  <h2>3. Install the App on a repo</h2>
  <p>Visit <a href="{{.Creds.HTMLURL}}/installations/new" target="_blank" rel="noopener">{{.Creds.HTMLURL}}/installations/new</a> and grant access to the repo Fishhawk should govern.</p>

  <h2>4. Restart fishhawkd</h2>
  <p>The 503 warnings on startup (OAuth, webhook receiver) will go away once the env vars above are picked up.</p>

  <p style="margin-top: 2rem; color: #666; font-size: 0.85rem;">Close this tab when done. The backend logs the App ID and slug for audit purposes; secrets are not logged.</p>
</body>
</html>
`))
