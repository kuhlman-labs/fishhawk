package server

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
)

// GET/POST /v0/oauth/authorize (ADR-076 slice 3, #2436). The GET renders the
// consent page after validating the request; the POST is the consent decision.
// Both run the SAME validation ladder (the POST re-runs it against the untrusted
// hidden fields), split into an in-place phase (RFC 6749 §4.1.2.1 forbids
// redirecting only when the redirect URI is missing/invalid/mismatched) and a
// redirect phase (every error funnelled through redirectOAuthError so state and
// the RFC 9207 iss are never dropped).

// authorizeRequest is the raw, unvalidated authorization parameters read from
// either the GET query (GET) or the POST body (consent POST). PostForm-only /
// query-only reading is the caller's job.
type authorizeRequest struct {
	clientID            string
	redirectURI         string
	responseType        string
	codeChallenge       string
	codeChallengeMethod string
	resource            string
	scope               string
	state               string
}

func authorizeRequestFromValues(v url.Values) authorizeRequest {
	return authorizeRequest{
		clientID:            v.Get("client_id"),
		redirectURI:         v.Get("redirect_uri"),
		responseType:        v.Get("response_type"),
		codeChallenge:       v.Get("code_challenge"),
		codeChallengeMethod: v.Get("code_challenge_method"),
		resource:            v.Get("resource"),
		scope:               v.Get("scope"),
		state:               v.Get("state"),
	}
}

// authorizeResolved is the validated output of the ladder: everything a consent
// render or a code mint needs, all derived from the request under validation.
type authorizeResolved struct {
	client *resolvedOAuthClient
	// responseRedirect is the DELIVERY URI (#2470), not the registered one: the
	// matched registration with the requested port substituted on the loopback
	// branch. The name is load-bearing — it was called matchedRedirect, and that
	// name made "deliver the response to the registered URI" look correct at
	// every call site while a loopback client on an ephemeral port never received
	// its code. ONE value feeds the success 302, every error/deny 302, and the
	// code row's redirect_uri, so the token endpoint's byte-exact comparison sees
	// the URI the client actually used.
	responseRedirect string
	method           oauthas.CodeChallengeMethod
	scopes           []string
}

// toOAuthError extracts the concrete *oauthas.Error a domain helper returned, or
// wraps an unexpected error as server_error so redirectOAuthError always has a
// code.
func toOAuthError(err error) *oauthas.Error {
	var oe *oauthas.Error
	if errors.As(err, &oe) {
		return oe
	}
	return &oauthas.Error{Code: oauthas.ErrCodeServerError, Description: "internal error"}
}

// runAuthorizeLadder validates req through the in-place then redirect phases. On
// ANY failure it writes the response (an in-place RFC 6749 §5.2 error or a
// redirect) and returns ok=false; on success it returns the resolved client,
// the response (delivery) redirect URI, and the parsed PKCE method + scopes.
func (s *Server) runAuthorizeLadder(w http.ResponseWriter, r *http.Request, req authorizeRequest) (authorizeResolved, bool) {
	// ---- IN-PLACE PHASE: no redirect is permitted yet. ----
	// The CIMD limiter fires INSIDE resolveOAuthClient, before
	// ResolveRedirectURI has validated any redirect_uri, so a rate-limit 429 is
	// rendered in place and NEVER redirected — required by RFC 6749 §4.1.2.1,
	// which forbids delivering an error to an unverified redirect target. The
	// shared renderer maps the limiter refusal to 429 + Retry-After and a store
	// outage to 503; a plain invalid_client stays 400 here.
	client, cerr := s.resolveOAuthClient(r, req.clientID)
	if cerr != nil {
		s.writeOAuthClientError(w, r, http.StatusBadRequest, cerr)
		return authorizeResolved{}, false
	}
	responseRedirect, merr := oauthas.ResolveRedirectURI(client.RedirectURIs, req.redirectURI)
	if merr != nil {
		oe := toOAuthError(merr)
		s.writeOAuthError(w, r, http.StatusBadRequest, oe.Code, oe.Description)
		return authorizeResolved{}, false
	}

	// ---- REDIRECT PHASE: every error delivered to the response redirect URI. ----
	// (3) response_type: server-wide "code" first, then the client's registered
	// capability (CONDITION 2).
	if req.responseType != "code" {
		s.redirectOAuthError(w, r, responseRedirect, req.state, &oauthas.Error{
			Code:        oauthas.ErrCodeUnsupportedResponseType,
			Description: "only response_type=code is supported",
		})
		return authorizeResolved{}, false
	}
	if !containsOAuth(registeredResponseTypes(client), "code") {
		s.redirectOAuthError(w, r, responseRedirect, req.state, &oauthas.Error{
			Code:        oauthas.ErrCodeUnauthorizedClient,
			Description: "the client is not registered for response_type=code",
		})
		return authorizeResolved{}, false
	}
	// (4) PKCE method + challenge (S256 only; absent method refused, never defaulted).
	method, err := oauthas.ParseCodeChallengeMethod(req.codeChallengeMethod)
	if err != nil {
		s.redirectOAuthError(w, r, responseRedirect, req.state, toOAuthError(err))
		return authorizeResolved{}, false
	}
	if err := oauthas.ValidateCodeChallenge(req.codeChallenge); err != nil {
		s.redirectOAuthError(w, r, responseRedirect, req.state, toOAuthError(err))
		return authorizeResolved{}, false
	}
	// (5) RFC 8707 resource: required and must match the configured resource.
	if _, err := oauthas.ParseResourceIdentifier(req.resource); err != nil {
		s.redirectOAuthError(w, r, responseRedirect, req.state, toOAuthError(err))
		return authorizeResolved{}, false
	}
	if !oauthas.ResourceMatches(s.oauthAS.resource.String(), req.resource) {
		s.redirectOAuthError(w, r, responseRedirect, req.state, &oauthas.Error{
			Code:        oauthas.ErrCodeInvalidTarget,
			Description: "the requested resource does not match this authorization server's resource identifier",
		})
		return authorizeResolved{}, false
	}
	// (6) scopes: valid against the server-wide vocabulary first. A request that
	// CARRIES NO SCOPE TOKEN (the key absent, or a present-but-empty `scope=` —
	// url.Values.Get cannot tell them apart and they are deliberately equivalent)
	// is DEFAULTED per RFC 6749 §3.3 rather than refused (#2466): to the client's
	// registered scope intersected with this server's vocabulary when the
	// registration pins one, else to the whole vocabulary. Every PRESENT value
	// still validates exactly as before — an unknown scope, a whitespace-only
	// `scope=%20%20`, and a scope exceeding the registration all still fail
	// invalid_scope.
	registered := registeredScopeSet(client)
	scopes, err := oauthas.ResolveRequestedScope(req.scope, registered)
	if err != nil {
		s.redirectOAuthError(w, r, responseRedirect, req.state, toOAuthError(err))
		return authorizeResolved{}, false
	}
	// (7) registered scope restriction (CONDITION 2's registration-driven
	// boundary, sibling to response_types/grant_types). When the client PINNED
	// a scope set, the request must stay within it — else a client the operator
	// deliberately registered narrow could obtain any operator scope, widening
	// the authority baked into the code and every derived token. An ABSENT
	// registered scope is NO restriction (the documented distinction), so the
	// check runs only when the set is non-empty. It is deliberately NOT skipped
	// for a DEFAULTED set: that set passes by construction (every member came
	// from the registration, or the registration was absent and imposes no
	// restriction), so one unconditional restriction ladder beats a conditional
	// one a later edit could get wrong.
	if len(registered) > 0 {
		for _, want := range scopes {
			if !containsOAuth(registered, want) {
				s.redirectOAuthError(w, r, responseRedirect, req.state, &oauthas.Error{
					Code:        oauthas.ErrCodeInvalidScope,
					Description: "the requested scope exceeds the client's registered scope",
				})
				return authorizeResolved{}, false
			}
		}
	}

	return authorizeResolved{client: client, responseRedirect: responseRedirect, method: method, scopes: scopes}, true
}

// handleOAuthAuthorize serves GET /v0/oauth/authorize: validate, then (identity
// LAST) either redirect a signed-out user to the forge sign-in or render the
// consent page.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.oauthASEnabled(w, r) {
		return
	}
	req := authorizeRequestFromValues(r.URL.Query())
	resolved, ok := s.runAuthorizeLadder(w, r, req)
	if !ok {
		return
	}

	// IDENTITY LAST: a signed-out user with an ALREADY-VALID request is sent to
	// sign in; a malformed request got its RFC error above, at their own client.
	id := IdentityFrom(r.Context())
	if id.SessionID == "" {
		s.startFederatedSignIn(w, r)
		return
	}

	s.renderConsent(w, r, req, resolved)
}

// handleOAuthConsent serves POST /v0/oauth/authorize: re-run the ladder against
// the untrusted hidden fields, then mint (approve) or redirect access_denied
// (deny). CSRF is enforced upstream by the middleware's form-field fallback.
func (s *Server) handleOAuthConsent(w http.ResponseWriter, r *http.Request) {
	if !s.oauthASEnabled(w, r) {
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() || id.SessionID == "" {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"sign in to submit an authorization decision", nil)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not parse the consent form", nil)
		return
	}
	// PostForm only — a query-string parameter must not influence the decision.
	req := authorizeRequestFromValues(r.PostForm)
	resolved, ok := s.runAuthorizeLadder(w, r, req)
	if !ok {
		return
	}

	// DENY is a redirect (client + redirect URI have validated by here).
	if r.PostForm.Get("action") != "approve" {
		s.redirectOAuthError(w, r, resolved.responseRedirect, req.state, &oauthas.Error{
			Code:        oauthas.ErrCodeAccessDenied,
			Description: "the user denied the authorization request",
		})
		return
	}

	// APPROVE: derive provider from auth.User.Provider (NEVER parsed from the
	// "github:"-prefixed Identity.Subject, which middleware.go hardcodes
	// regardless of the signing-in forge). account_id comes from the session.
	uid, perr := uuid.Parse(id.UserID)
	if perr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"identity carried an invalid user id", nil)
		return
	}
	user, uerr := s.cfg.AuthRepo.GetUser(r.Context(), uid)
	if uerr != nil {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required", "user not found", nil)
		return
	}

	code, err := s.cfg.OAuthStore.CreateAuthorizationCode(r.Context(), oauthstore.NewAuthorizationCode{
		ClientID: req.clientID,
		// The DELIVERY URI, not the registration: the token endpoint compares this
		// byte-exactly against the redirect_uri the client presents, and the client
		// presents the one it requested (#2470).
		RedirectURI:         resolved.responseRedirect,
		CodeChallenge:       req.codeChallenge,
		CodeChallengeMethod: resolved.method,
		Scopes:              resolved.scopes,
		Resource:            req.resource,
		Subject:             id.Subject,
		Provider:            user.Provider,
		AccountID:           id.AccountID,
		ExpiresAt:           s.nowFunc().Add(s.oauthAS.codeTTL),
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not mint the authorization code", nil)
		return
	}

	// 302 to the RESPONSE redirect URI with the code, echoed state and iss — the
	// registered URI carrying the requested loopback port, so a native client
	// listening on an ephemeral port actually receives the code (RFC 8252 §7.3).
	// The code plaintext exists only in this redirect.
	u, err := url.Parse(resolved.responseRedirect)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not build the authorization response redirect", nil)
		return
	}
	q := u.Query()
	q.Set("code", code.PlainText)
	if req.state != "" {
		q.Set("state", req.state)
	}
	q.Set("iss", s.oauthAS.issuer.String())
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// renderConsent renders the consent page. CONDITION 1: when the request carries
// no __Host-csrf cookie (the Strict cookie is NOT sent on the cross-site
// top-level authorize navigation that brings the user here), mint a fresh one
// with the existing generator/setter and embed THAT value, relying on the
// same-site form POST to carry the newly set cookie. A handler that read only
// the request cookie would embed nothing and 403 every legitimate user — a
// failure no httptest-cookie test can see. See backend/internal/server/README.md.
func (s *Server) renderConsent(w http.ResponseWriter, r *http.Request, req authorizeRequest, resolved authorizeResolved) {
	csrfToken := ""
	if c, err := r.Cookie(CSRFCookieName); err == nil {
		csrfToken = c.Value
	}
	if csrfToken == "" {
		tok, err := generateCSRFToken()
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not generate a CSRF token", nil)
			return
		}
		setCSRFCookie(w, tok)
		csrfToken = tok
	}

	display := resolved.client.ClientName
	if display == "" {
		display = req.clientID
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := consentTmpl.Execute(w, consentView{
		ClientDisplay:       display,
		ClientURI:           resolved.client.ClientURI,
		Resource:            req.resource,
		Scopes:              resolved.scopes,
		CSRFToken:           csrfToken,
		ClientID:            req.clientID,
		RedirectURI:         req.redirectURI,
		ResponseType:        req.responseType,
		CodeChallenge:       req.codeChallenge,
		CodeChallengeMethod: req.codeChallengeMethod,
		ResourceParam:       req.resource,
		// The RESOLVED scope set, NOT the raw request value (#2466 CONDITION B):
		// what the page DISPLAYS is what the POST submits, so what the user saw
		// is what gets granted. Echoing the raw value would make a scope-less GET
		// produce a scope-less POST that re-derives the default against whatever
		// the registration says at POST time — a consent/grant divergence. With
		// the resolved value the POST validates through the ordinary PRESENT-scope
		// path: if the registration NARROWED in between, the POST carries scopes
		// it no longer permits and is refused invalid_scope at step 7 (a refusal,
		// not a silent grant); if it BROADENED, the narrower displayed set is
		// granted. Both match what was displayed. This adds no tampering surface —
		// a user editing their own hidden field could equally have crafted that
		// authorize request directly, and step 7 still bounds it whenever a
		// registration pins a scope.
		Scope: oauthas.ScopeString(resolved.scopes),
		State: req.state,
	}); err != nil {
		s.cfg.Logger.Error("render oauth consent page", "error", err.Error())
	}
}

// consentView is the template model. Every client-supplied string reaches the
// page only through html/template auto-escaping; ClientURI renders as TEXT,
// never as an href, so a CIMD document cannot inject a scheme.
type consentView struct {
	ClientDisplay string
	ClientURI     string
	Resource      string
	Scopes        []string
	CSRFToken     string

	// Hidden authorization parameters carried through the form POST.
	ClientID            string
	RedirectURI         string
	ResponseType        string
	CodeChallenge       string
	CodeChallengeMethod string
	ResourceParam       string
	Scope               string
	State               string
}

// consentTmpl is a deliberately minimal, unstyled consent page (per the
// operator's budget cut-line). It names the client, the resource and each
// requested scope, and posts the authorization parameters back as hidden fields
// with an approve/deny action and the CSRF token.
var consentTmpl = template.Must(template.New("oauth-consent").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Authorize</title></head>
<body>
<h1>Authorize application</h1>
<p><strong>{{.ClientDisplay}}</strong> is requesting access.</p>
{{if .ClientURI}}<p>Client URL: {{.ClientURI}}</p>{{end}}
<p>Resource: {{.Resource}}</p>
<p>Requested scopes:</p>
<ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul>
<form method="POST" action="/v0/oauth/authorize">
<input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="response_type" value="{{.ResponseType}}">
<input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
<input type="hidden" name="resource" value="{{.ResourceParam}}">
<input type="hidden" name="scope" value="{{.Scope}}">
<input type="hidden" name="state" value="{{.State}}">
<button type="submit" name="action" value="approve">Approve</button>
<button type="submit" name="action" value="deny">Deny</button>
</form>
</body>
</html>`))

// signInForges lists the BROWSER sign-in legs this deployment has
// configured, in a deterministic github-then-gitlab order (E66.4 / #2392).
//
// This is deliberately INDEPENDENT of the device-flow enumeration in
// configuredIdentityProviders: the browser leg is gated on the Confidential
// application's OAuth trio (client_id + secret + callback), the device leg
// on a NON-Confidential application's client id alone, and a deployment may
// configure either, both, or neither. Reading one from the other would make
// a half-configured deployment render a sign-in link to a 503 route.
func (s *Server) signInForges() []string {
	var forges []string
	if s.cfg.GitHubOAuth != nil {
		forges = append(forges, identity.ProviderGitHub)
	}
	if s.cfg.GitLabOAuth != nil {
		forges = append(forges, identity.ProviderGitLab)
	}
	return forges
}

// startFederatedSignIn is the authorization endpoint's federated-login
// step for a signed-out user with an already-valid request.
//
// Zero configured forges keeps the pre-#2392 503 oauth_unconfigured. EXACTLY
// ONE redirects straight to it — byte-identical to the previous unconditional
// /v0/auth/github/login redirect on a GitHub-only deployment. TWO renders a
// minimal provider-choice page: a LINK per forge, each carrying the same
// next= value in its query string (there is no form and no hidden field —
// the page is a pure navigation choice).
func (s *Server) startFederatedSignIn(w http.ResponseWriter, r *http.Request) {
	forges := s.signInForges()
	if len(forges) == 0 {
		s.writeError(w, r, http.StatusServiceUnavailable, "oauth_unconfigured",
			"forge sign-in is not configured on this deployment", nil)
		return
	}
	next := r.URL.Path + "?" + r.URL.RawQuery
	if len(forges) == 1 {
		http.Redirect(w, r, forgeLoginURL(forges[0], next), http.StatusFound)
		return
	}
	choices := make([]forgeChoice, 0, len(forges))
	for _, forge := range forges {
		choices = append(choices, forgeChoice{Provider: forge, LoginURL: forgeLoginURL(forge, next)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := forgeChoiceTmpl.Execute(w, forgeChoiceView{Forges: choices}); err != nil {
		s.cfg.Logger.Error("render forge choice page", "error", err.Error())
	}
}

// forgeLoginURL builds the per-forge sign-in URL with the return target
// query-escaped, the same shape the single-forge redirect has always used.
func forgeLoginURL(forge, next string) string {
	return "/v0/auth/" + forge + "/login?next=" + url.QueryEscape(next)
}

// forgeChoice / forgeChoiceView are the choice page's template model. The
// LoginURL is already query-escaped; html/template additionally applies
// URL-context escaping inside the href and HTML escaping to the visible
// text, so a `next` carrying a quote or an angle bracket cannot break out
// of the attribute (pinned by TestOAuthAuthorize_ChoicePage_EscapesNext).
type forgeChoice struct {
	Provider string
	LoginURL string
}

type forgeChoiceView struct {
	Forges []forgeChoice
}

// forgeChoiceTmpl is a deliberately minimal, unstyled sign-in chooser,
// matching consentTmpl's template hygiene: every dynamic value crosses the
// boundary through html/template, never through string concatenation.
var forgeChoiceTmpl = template.Must(template.New("oauth-forge-choice").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Sign in</title></head>
<body>
<h1>Sign in to continue</h1>
<p>Choose the forge to sign in with.</p>
<ul>{{range .Forges}}<li><a href="{{.LoginURL}}">{{.Provider}}</a></li>{{end}}</ul>
</body>
</html>`))
