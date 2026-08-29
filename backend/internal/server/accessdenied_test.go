package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// accessDeniedPage drives GET /access-denied through the FULL server
// handler with the supplied raw query, returning the recorder and the log
// buffer. Driving the real handler (not the template directly) is what
// makes these assertions read SHIPPED RENDERED OUTPUT.
func accessDeniedPage(t *testing.T, rawQuery string) (*httptest.ResponseRecorder, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	s := New(Config{
		Addr:   "127.0.0.1:0",
		Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	target := "/access-denied"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w, &logs
}

// Phrases the branch bodies own. Asserting on rendered prose is deliberate:
// a comment-only or no-op touch of accessdenied.go fails these, which a
// presence-only scope gate would not catch.
const (
	noResolverText = "no membership resolver wired on this deployment"
	noAccountText  = "No workspace account on this deployment admits the login"
	genericText    = "The specific reason was not carried to this page"
	singleTenantK  = "FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY"
	inviteRemedy   = "ask an existing workspace admin to invite it"
)

// M1: reason=no_membership_resolver renders the resolver-not-configured
// explanation and its deployment-configuration remedy, and NOT the
// no-admitting-account text.
func TestAccessDenied_NoMembershipResolver(t *testing.T) {
	w, _ := accessDeniedPage(t, "reason=no_membership_resolver")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, noResolverText) {
		t.Errorf("body missing the resolver-not-configured explanation:\n%s", body)
	}
	if !strings.Contains(body, "deployment-configuration fault") {
		t.Errorf("body missing the deployment-configuration framing:\n%s", body)
	}
	if !strings.Contains(body, singleTenantK) {
		t.Errorf("body missing %s remedy:\n%s", singleTenantK, body)
	}
	if strings.Contains(body, noAccountText) {
		t.Errorf("body carries the OTHER branch's text:\n%s", body)
	}
}

// M2: reason=no_admitting_account renders the no-workspace-admits
// explanation, the single-tenant key, and the admin-invite remedy. The
// provider and login on the query are ignored — see M4.
func TestAccessDenied_NoAdmittingAccount(t *testing.T) {
	w, _ := accessDeniedPage(t, "reason=no_admitting_account&provider=github&login=octocat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{noAccountText, singleTenantK, inviteRemedy} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, noResolverText) {
		t.Errorf("body carries the OTHER branch's text:\n%s", body)
	}
}

// The denial page sets the three headers the plan names. Cache-Control keeps
// this reason-bearing page out of shared caches; Referrer-Policy keeps its
// URL off any outbound navigation from the page.
func TestAccessDenied_Headers(t *testing.T) {
	w, _ := accessDeniedPage(t, "reason=no_admitting_account&provider=github&login=octocat")
	for header, want := range map[string]string{
		"Content-Type":    "text/html; charset=utf-8",
		"Cache-Control":   "no-store",
		"Referrer-Policy": "no-referrer",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// M3: an ABSENT reason renders the generic body at 200 with NEITHER
// branch's specific text, and logs NOTHING — an absent reason is not a
// rejected one, so it must be distinguishable from M3b.
func TestAccessDenied_ReasonAbsent_Generic(t *testing.T) {
	w, logs := accessDeniedPage(t, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, genericText) {
		t.Errorf("body missing the generic explanation:\n%s", body)
	}
	if strings.Contains(body, noResolverText) || strings.Contains(body, noAccountText) {
		t.Errorf("generic body carries a branch-specific explanation:\n%s", body)
	}
	if strings.Contains(logs.String(), "unrecognized reason code") {
		t.Errorf("absent reason logged as unrecognized:\n%s", logs.String())
	}
}

// M3b: an UNRECOGNIZED reason renders the generic body, the raw value
// appears NOWHERE in the response, and the allow-list's refusal is RECORDED.
//
// That log assertion is what makes parseAccessDeniedReason's counterfactual
// attainable (CONDITION 2, option (a)). Without it the control would have no
// independently observable effect — the template renders the generic branch
// for an unrecognized value whether or not the allow-list exists — and a
// mandated RED that cannot happen is worse than no counterfactual. Deleting
// the allow-list (passing the raw value through) drops the log line AND
// renders the raw value; both assertions below go RED.
func TestAccessDenied_ReasonUnrecognized_GenericAndRecorded(t *testing.T) {
	const raw = "totally-not-a-branch-code"
	w, logs := accessDeniedPage(t, "reason="+url.QueryEscape(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, genericText) {
		t.Errorf("body missing the generic explanation:\n%s", body)
	}
	if strings.Contains(body, raw) {
		t.Errorf("body echoed the unrecognized reason %q:\n%s", raw, body)
	}
	if strings.Contains(body, noResolverText) || strings.Contains(body, noAccountText) {
		t.Errorf("generic body carries a branch-specific explanation:\n%s", body)
	}
	if !strings.Contains(logs.String(), "unrecognized reason code") {
		t.Errorf("allow-list refusal was not recorded:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), raw) {
		t.Errorf("log did not name the rejected value:\n%s", logs.String())
	}
}

// M4: the page NEVER renders an identity, whatever the query carries.
//
// This is the control the fix-up pass installed in place of the earlier
// sanitize-then-render one (implement-review high/security): /access-denied
// is a separate, unauthenticated request, so a provider+login on its query
// establishes nothing — a crafted URL could otherwise produce a
// Fishhawk-branded page asserting that a named login signed in. The table
// therefore pairs hostile values with a PERFECTLY VALID one ("octocat",
// "provider-only"): a control that merely sanitized would keep the valid row
// GREEN while still rendering the claim, so the valid row is what makes the
// deletion counterfactual real. Re-adding any {{.Login}}/{{.Provider}}
// interpolation reddens the octocat row first.
//
// Every value is seeded BY CONSTRUCTION as a literal here — never produced by
// calling a sanitizer inside the test's own setup — so the RED lands on the
// body assertions below rather than on fixture setup. Each row additionally
// asserts that "octocat" specifically does not survive, which the earlier
// whitespace-fragment loop could not catch on the single-token rows
// (implement-review low/untested-path).
func TestAccessDenied_NeverRendersAnIdentity(t *testing.T) {
	for name, login := range map[string]string{
		"valid login":   "octocat",
		"markup":        `<script>alert(1)</script>`,
		"phish-text":    "Call support at 555-0100 to restore access",
		"control-char":  "octo\x00cat",
		"newline":       "octocat\nSend your token to evil@example.com",
		"over-length":   strings.Repeat("a", 65),
		"empty":         "",
		"whitespace":    "   ",
		"slash":         "octocat/../admin",
		"provider-only": "",
	} {
		t.Run(name, func(t *testing.T) {
			q := url.Values{}
			q.Set("reason", "no_admitting_account")
			q.Set("provider", "github")
			q.Set("login", login)
			w, _ := accessDeniedPage(t, q.Encode())
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			// Nothing of the supplied login reaches the page — not the whole
			// value, not a whitespace-delimited fragment, and not the
			// "octocat" prefix a truncating implementation would leave on the
			// single-token control-char and slash rows.
			if login != "" && strings.Contains(body, login) {
				t.Errorf("body carried the supplied login %q:\n%s", login, body)
			}
			for _, frag := range strings.Fields(login) {
				if len(frag) >= 4 && strings.Contains(body, frag) {
					t.Errorf("body carried a fragment %q of the supplied login:\n%s", frag, body)
				}
			}
			if strings.Contains(body, "octocat") {
				t.Errorf("body carried a truncated login prefix \"octocat\":\n%s", body)
			}
			if strings.Contains(body, "You signed in") {
				t.Errorf("body claimed an identity it cannot verify:\n%s", body)
			}
			// The provider is not rendered either: "you signed in to GitHub"
			// is the same unverified claim with the login left out.
			if strings.Contains(body, "GitHub") || strings.Contains(body, "github") {
				t.Errorf("body named the forge the request claimed:\n%s", body)
			}
			// The branch explanation is unaffected by any of this.
			if !strings.Contains(body, noAccountText) {
				t.Errorf("branch explanation lost:\n%s", body)
			}
		})
	}
}

// M5: with no identity parameters at all, the page states plainly that it
// names no account and points at the log that does.
func TestAccessDenied_NoIdentityParameters(t *testing.T) {
	w, _ := accessDeniedPage(t, "reason=no_admitting_account")
	body := w.Body.String()
	if !strings.Contains(body, "This Fishhawk deployment did not admit the sign-in.") {
		t.Errorf("body missing the deployment-did sentence:\n%s", body)
	}
	if !strings.Contains(body, "This page names no account") {
		t.Errorf("body missing the no-identity disclosure:\n%s", body)
	}
	if strings.Contains(body, "You signed in") {
		t.Errorf("body claimed an identity:\n%s", body)
	}
	if !strings.Contains(body, noAccountText) {
		t.Errorf("branch explanation lost:\n%s", body)
	}
}

// M6: accessDeniedRedirect's target resolution and parameter merge.
//
// The already-carries-a-reason row is the CONDITION 4 pin: url.Values.Set
// REPLACES the operator's value, so the branch code wins. Under Add the
// operator's value would come first and Query().Get would return it — the
// page would name the wrong denial, which is exactly the failure this issue
// exists to fix.
//
// The absentKeys rows pin the fix-up pass's other half: the redirect carries
// the reason and NOTHING else. Neither landing page can verify an identity
// read off this URL, so putting the provider and login on it would only leak
// them into browser history, proxy logs and Referer headers.
func TestAccessDeniedRedirect_Table(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		reason     accessDeniedReason
		wantPath   string
		wantQuery  map[string]string
		absentKeys []string
	}{
		{
			name:       "empty config falls back to the default target",
			reason:     accessDeniedNoAccount,
			wantPath:   "/access-denied",
			wantQuery:  map[string]string{"reason": "no_admitting_account"},
			absentKeys: []string{"provider", "login"},
		},
		{
			name:       "scheme-relative config is refused by isSafeRelativeRedirect",
			configured: "//evil.example.com/", reason: accessDeniedNoResolver,
			wantPath:  "/access-denied",
			wantQuery: map[string]string{"reason": "no_membership_resolver"},
		},
		{
			name:       "absolute config is refused by isSafeRelativeRedirect",
			configured: "https://evil.example.com/access-denied", reason: accessDeniedNoAccount,
			wantPath:  "/access-denied",
			wantQuery: map[string]string{"reason": "no_admitting_account"},
		},
		{
			name:       "plain configured path is honored",
			configured: "/no-entry", reason: accessDeniedNoAccount,
			wantPath:   "/no-entry",
			wantQuery:  map[string]string{"reason": "no_admitting_account"},
			absentKeys: []string{"provider", "login"},
		},
		{
			name:       "a configured path's pre-existing query survives the merge",
			configured: "/no-entry?theme=dark", reason: accessDeniedNoAccount,
			wantPath:  "/no-entry",
			wantQuery: map[string]string{"theme": "dark", "reason": "no_admitting_account"},
		},
		{
			name:       "an operator-set reason key is REPLACED, not duplicated (Set, not Add)",
			configured: "/no-entry?reason=operator-chose-this", reason: accessDeniedNoAccount,
			wantPath:  "/no-entry",
			wantQuery: map[string]string{"reason": "no_admitting_account"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Addr: "127.0.0.1:0", AuthAccessDeniedRedirect: tc.configured})
			got := s.accessDeniedRedirect(tc.reason)
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse %q: %v", got, err)
			}
			if u.Path != tc.wantPath {
				t.Errorf("path = %q, want %q (from %q)", u.Path, tc.wantPath, got)
			}
			q := u.Query()
			for k, want := range tc.wantQuery {
				if q.Get(k) != want {
					t.Errorf("query %s = %q, want %q (from %q)", k, q.Get(k), want, got)
				}
				if len(q[k]) != 1 {
					t.Errorf("query %s has %d values, want exactly 1 (Set, not Add): %q", k, len(q[k]), got)
				}
			}
			for _, k := range tc.absentKeys {
				if _, ok := q[k]; ok {
					t.Errorf("query %s present, want absent (from %q)", k, got)
				}
			}
		})
	}
}
