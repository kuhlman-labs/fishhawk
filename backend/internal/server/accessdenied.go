package server

import (
	"html/template"
	"net/http"
)

// The membership-gate denial page (E44.31 / #2467).
//
// handleForgeCallback denies a sign-in on two named branches and 302s the
// browser at a RELATIVE target. That target resolves against the CALLBACK'S
// OWN ORIGIN — fishhawkd — so in the standard split-origin layout (Vite on
// :5173 proxying only /v0 to fishhawkd on :8080) it never reaches the SPA
// page added by #1827. fishhawkd registered no such route, so the operator
// landed on a bare ServeMux 404 with no explanation of why sign-in failed.
//
// This file makes that target resolve AND explain itself: the callback
// classifies its denial into a stable reason code and carries it on the
// redirect, and handleAccessDenied renders a self-contained html/template
// page naming the branch and its remedy — the same shape manifest.go and
// oauthauthorize.go already use for their server-rendered pages.
//
// NO IDENTITY IS RENDERED HERE, and none is carried on the redirect.
// /access-denied is a SEPARATE, unauthenticated request from the callback:
// the callback authenticates and then 302s, so this handler has no session,
// no cookie and no authenticated state to read — a provider/login on the
// query would have exactly the authority the SPA page has, which is none.
// Anyone could craft /access-denied?provider=github&login=<valid-name> and
// obtain a Fishhawk-branded page asserting that login signed in. A charset
// bound stops markup but establishes nothing about authenticity, so the
// claim is removed rather than hardened: the page states only what the
// DEPLOYMENT did (it refused the sign-in) and what the operator can do
// about it, never what the user did. The reason code — a closed enum, not
// free text — is the whole payload, and it is what makes the page useful.
//
// STATUS 200, NOT 403, deliberately. The request to /access-denied SUCCEEDS
// and returns the explanation; a 403 would claim the operator is forbidden
// from READING the explanation, and would additionally invite an
// intermediary proxy to replace the body with its own error page — which is
// precisely the class of failure this issue exists to fix. The denial itself
// is still recorded honestly: the callback creates no session and no cookie,
// and logs the provider and login server-side.

// accessDeniedReason is the closed set of denial branch codes the callback
// carries on its redirect. The zero value is the unknown fallback: an
// unrecognized code renders the generic page, never the raw input.
type accessDeniedReason string

const (
	// accessDeniedUnknown is the fallback for an absent or unrecognized
	// reason code. Renders the generic body.
	accessDeniedUnknown accessDeniedReason = ""
	// accessDeniedNoResolver is the Config.AuthMembership == nil branch:
	// no membership resolver is wired at all, so EVERY sign-in is denied.
	// A deployment-configuration fault, not a per-user one.
	accessDeniedNoResolver accessDeniedReason = "no_membership_resolver"
	// accessDeniedNoAccount is the len(accountIDs) == 0 branch: the
	// resolver ran and no workspace account on this deployment admits
	// the authenticated login.
	accessDeniedNoAccount accessDeniedReason = "no_admitting_account"
)

// parseAccessDeniedReason is a closed allow-list, NOT a passthrough. The
// reason value arrives on the query string of a PUBLIC, UNAUTHENTICATED
// route and selects the rendered body, so it must never be attacker-chosen
// free text. Any value that is not one of the two branch codes — including
// the empty string — yields accessDeniedUnknown and the generic body.
//
// The second return distinguishes the two ways that happens: false means a
// NON-EMPTY value was supplied and rejected, which handleAccessDenied
// records in the log. That log line is what makes this control's effect
// OBSERVABLE (and makes the generic page's "check the fishhawkd log"
// advice true); without it, an unrecognized reason would be
// indistinguishable from an absent one and the allow-list would have no
// independently assertable behavior.
func parseAccessDeniedReason(raw string) (accessDeniedReason, bool) {
	switch accessDeniedReason(raw) {
	case accessDeniedNoResolver:
		return accessDeniedNoResolver, true
	case accessDeniedNoAccount:
		return accessDeniedNoAccount, true
	case accessDeniedUnknown:
		// Absent: the generic body is correct and nothing was rejected.
		return accessDeniedUnknown, true
	default:
		return accessDeniedUnknown, false
	}
}

// accessDeniedView is the denial page's template model. The ONLY field is
// the allow-listed branch code: no query value reaches the template, and
// nothing on the page names who attempted the sign-in.
type accessDeniedView struct {
	// Reason is one of the two branch codes, or empty for the generic body.
	Reason accessDeniedReason
}

// NoResolver / NoAccount keep the branch predicates in Go rather than
// spreading string comparisons through the template.
func (v accessDeniedView) NoResolver() bool { return v.Reason == accessDeniedNoResolver }
func (v accessDeniedView) NoAccount() bool  { return v.Reason == accessDeniedNoAccount }

// accessDeniedTmpl follows the house pattern established by
// manifestStartTmpl, consentTmpl and forgeChoiceTmpl: a self-contained
// document, no external CSS and no external assets, with every dynamic
// value crossing the boundary through html/template rather than string
// concatenation. Here the only dynamic input is the branch selection —
// every rendered word is a literal in this template.
var accessDeniedTmpl = template.Must(template.New("access-denied").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Access denied</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;line-height:1.5}code{background:#f2f2f2;padding:0.1rem 0.3rem}</style>
</head>
<body>
<h1>Access denied</h1>
<p>This Fishhawk deployment did not admit the sign-in.</p>
{{if .NoResolver}}<h2>Why</h2>
<p>The login gate has no membership resolver wired on this deployment, so every sign-in is denied. This is a deployment-configuration fault, not a problem with the account you used.</p>
<h2>Remedy</h2>
<p>An operator must configure the database and the workspace profile. For a single-tenant self-host, set <code>FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY</code> to the forge login or organization that owns the deployment. See the self-hosted deployment guide.</p>
{{else if .NoAccount}}<h2>Why</h2>
<p>No workspace account on this deployment admits the login the sign-in was made with: there is no invited membership for it, and no auto-join policy matched.</p>
<h2>Remedy</h2>
<p>Either set <code>FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY</code> to that login (single-tenant self-host), or ask an existing workspace admin to invite it.</p>
{{else}}<h2>Why</h2>
<p>This deployment denied the sign-in. The specific reason was not carried to this page.</p>
<h2>Remedy</h2>
<p>Either set <code>FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY</code> to the login the sign-in was made with (single-tenant self-host), or ask an existing workspace admin to invite it. If every sign-in on this deployment is denied, the login gate has no membership resolver configured and an operator must wire the database.</p>
{{end}}
<p>This page names no account: it is served without a session, so it cannot verify who attempted the sign-in. The fishhawkd log records the attempt, naming the forge and the login it was made with.</p>
</body>
</html>`))

// handleAccessDenied implements GET /access-denied — the backend-origin
// landing page for the callback's membership-gate denial. It reads exactly
// one query parameter, and that parameter only SELECTS one of three fixed
// bodies; see the file comment for why no identity is read or rendered.
func (s *Server) handleAccessDenied(w http.ResponseWriter, r *http.Request) {
	rawReason := r.URL.Query().Get("reason")
	reason, recognized := parseAccessDeniedReason(rawReason)
	if !recognized {
		// The allow-list refused a non-empty value. Log it: this is the
		// observable effect of the control, and the generic page tells the
		// operator to look here.
		s.cfg.Logger.Warn("access-denied page: unrecognized reason code; rendering the generic body",
			"reason", rawReason)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Ordinary hygiene for a public error page reached mid-OAuth: the
	// Referer would otherwise carry this URL (and, on the hop that got
	// here, the forge's) to anything the operator navigates to next.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if err := accessDeniedTmpl.Execute(w, accessDeniedView{Reason: reason}); err != nil {
		s.cfg.Logger.Error("render access-denied page", "error", err.Error())
	}
}
