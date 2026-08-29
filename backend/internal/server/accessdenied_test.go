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
	noAccountText  = "No workspace account on this deployment admits this login"
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
// explanation, the sanitized login, the single-tenant key, and the
// admin-invite remedy.
func TestAccessDenied_NoAdmittingAccount(t *testing.T) {
	w, _ := accessDeniedPage(t, "reason=no_admitting_account&provider=github&login=octocat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{noAccountText, "octocat", "GitHub", singleTenantK, inviteRemedy} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, noResolverText) {
		t.Errorf("body carries the OTHER branch's text:\n%s", body)
	}
}

// The denial page sets the three headers the plan names. Cache-Control keeps
// the login-bearing page out of shared caches; Referrer-Policy keeps the
// login off any outbound navigation from the page.
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

// M4: a login failing the charset/length check is OMITTED, not truncated.
// Every hostile value is seeded BY CONSTRUCTION as a literal here — never
// produced by calling sanitizeForgeLogin inside the test's own setup — so
// the RED under a deleted control lands on the body assertion below rather
// than on fixture setup.
func TestAccessDenied_HostileLogin_Omitted(t *testing.T) {
	for name, bad := range map[string]string{
		"markup":       `<script>alert(1)</script>`,
		"phish-text":   "Call support at 555-0100 to restore access",
		"control-char": "octo\x00cat",
		"newline":      "octocat\nSend your token to evil@example.com",
		"over-length":  strings.Repeat("a", 65),
		"empty":        "",
		"whitespace":   "   ",
		"slash":        "octocat/../admin",
	} {
		t.Run(name, func(t *testing.T) {
			q := url.Values{}
			q.Set("reason", "no_admitting_account")
			q.Set("provider", "github")
			q.Set("login", bad)
			w, _ := accessDeniedPage(t, q.Encode())
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			// Omission, not truncation: no fragment of the hostile value
			// survives, and the page falls back to the identity-free
			// sentence.
			for _, frag := range strings.Fields(bad) {
				if len(frag) >= 4 && strings.Contains(body, frag) {
					t.Errorf("body carried a fragment %q of the rejected login:\n%s", frag, body)
				}
			}
			if !strings.Contains(body, "This Fishhawk deployment did not admit the sign-in.") {
				t.Errorf("body missing the identity-free sentence:\n%s", body)
			}
			if strings.Contains(body, "You signed in to") {
				t.Errorf("body claimed an identity it could not verify:\n%s", body)
			}
		})
	}
}

// M5: an absent login renders the identity-free sentence with no
// empty-name artifact.
func TestAccessDenied_LoginAbsent(t *testing.T) {
	w, _ := accessDeniedPage(t, "reason=no_admitting_account")
	body := w.Body.String()
	if !strings.Contains(body, "This Fishhawk deployment did not admit the sign-in.") {
		t.Errorf("body missing the identity-free sentence:\n%s", body)
	}
	if strings.Contains(body, "You signed in to") {
		t.Errorf("body rendered an identity line with no login:\n%s", body)
	}
	if !strings.Contains(body, noAccountText) {
		t.Errorf("branch explanation lost when the login is absent:\n%s", body)
	}
}

// A recognized login with an ABSENT or UNRECOGNIZED provider still omits the
// identity line: naming a login with no forge reads as a bare string.
func TestAccessDenied_ProviderGuard(t *testing.T) {
	for name, q := range map[string]string{
		"provider absent":       "reason=no_admitting_account&login=octocat",
		"provider unrecognized": "reason=no_admitting_account&provider=bitbucket&login=octocat",
	} {
		t.Run(name, func(t *testing.T) {
			w, _ := accessDeniedPage(t, q)
			body := w.Body.String()
			if strings.Contains(body, "You signed in to") {
				t.Errorf("identity line rendered with no recognized provider:\n%s", body)
			}
			if strings.Contains(body, "bitbucket") {
				t.Errorf("body echoed the unrecognized provider:\n%s", body)
			}
		})
	}
}

// M6: accessDeniedRedirect's target resolution and parameter merge.
//
// The already-carries-a-reason row is the CONDITION 4 pin: url.Values.Set
// REPLACES the operator's value, so the branch code wins. Under Add the
// operator's value would come first and Query().Get would return it — the
// page would name the wrong denial, which is exactly the failure this issue
// exists to fix.
func TestAccessDeniedRedirect_Table(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		reason     accessDeniedReason
		provider   string
		login      string
		wantPath   string
		wantQuery  map[string]string
		absentKeys []string
	}{
		{
			name:   "empty config falls back to the default target",
			reason: accessDeniedNoAccount, provider: "github", login: "octocat",
			wantPath:  "/access-denied",
			wantQuery: map[string]string{"reason": "no_admitting_account", "provider": "github", "login": "octocat"},
		},
		{
			name:       "scheme-relative config is refused by isSafeRelativeRedirect",
			configured: "//evil.example.com/", reason: accessDeniedNoResolver, provider: "gitlab", login: "gluser",
			wantPath:  "/access-denied",
			wantQuery: map[string]string{"reason": "no_membership_resolver", "provider": "gitlab", "login": "gluser"},
		},
		{
			name:       "absolute config is refused by isSafeRelativeRedirect",
			configured: "https://evil.example.com/access-denied", reason: accessDeniedNoAccount, provider: "github", login: "octocat",
			wantPath:  "/access-denied",
			wantQuery: map[string]string{"reason": "no_admitting_account"},
		},
		{
			name:       "plain configured path is honored",
			configured: "/no-entry", reason: accessDeniedNoAccount, provider: "github", login: "octocat",
			wantPath:  "/no-entry",
			wantQuery: map[string]string{"reason": "no_admitting_account", "provider": "github", "login": "octocat"},
		},
		{
			name:       "a configured path's pre-existing query survives the merge",
			configured: "/no-entry?theme=dark", reason: accessDeniedNoAccount, provider: "github", login: "octocat",
			wantPath:  "/no-entry",
			wantQuery: map[string]string{"theme": "dark", "reason": "no_admitting_account", "provider": "github", "login": "octocat"},
		},
		{
			name:       "an operator-set reason key is REPLACED, not duplicated (Set, not Add)",
			configured: "/no-entry?reason=operator-chose-this", reason: accessDeniedNoAccount, provider: "github", login: "octocat",
			wantPath:  "/no-entry",
			wantQuery: map[string]string{"reason": "no_admitting_account"},
		},
		{
			name:   "a login failing the charset check is omitted from the redirect",
			reason: accessDeniedNoAccount, provider: "github", login: "<script>alert(1)</script>",
			wantPath:   "/access-denied",
			wantQuery:  map[string]string{"reason": "no_admitting_account", "provider": "github"},
			absentKeys: []string{"login"},
		},
		{
			name:   "an unrecognized provider is omitted from the redirect",
			reason: accessDeniedNoAccount, provider: "bitbucket", login: "octocat",
			wantPath:   "/access-denied",
			wantQuery:  map[string]string{"reason": "no_admitting_account", "login": "octocat"},
			absentKeys: []string{"provider"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Addr: "127.0.0.1:0", AuthAccessDeniedRedirect: tc.configured})
			got := s.accessDeniedRedirect(tc.reason, tc.provider, tc.login)
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

// sanitizeForgeLogin's accept side: the charset the two forges actually
// produce must pass, or the control would omit every legitimate login and
// the page would silently lose its identity line for everyone.
func TestSanitizeForgeLogin_Accepts(t *testing.T) {
	for _, login := range []string{"octocat", "alice-acme", "alice_acme", "a.b_c-d", "x", strings.Repeat("a", 64)} {
		got, ok := sanitizeForgeLogin(login)
		if !ok || got != login {
			t.Errorf("sanitizeForgeLogin(%q) = %q,%v; want %q,true", login, got, ok, login)
		}
	}
}
