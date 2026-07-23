package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
)

// fakeRepoVisibility is the server.RepoVisibility stub. `visible` answers per
// repo (absent = not visible); `err` makes every lookup a STORE fault; and
// every call is recorded so a test can assert the cross-forge deny made ZERO
// forge calls and that per-request memoization holds.
type fakeRepoVisibility struct {
	mu        sync.Mutex
	visible   map[string]bool
	err       error
	calls     []string
	purged    []string
	purgeErr  error
	purgeCall int
}

func newFakeRepoVisibility(visible map[string]bool) *fakeRepoVisibility {
	return &fakeRepoVisibility{visible: visible}
}

func (f *fakeRepoVisibility) Visible(_ context.Context, provider, subject, repo string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, provider+"|"+subject+"|"+repo)
	if f.err != nil {
		return false, f.err
	}
	return f.visible[repo], nil
}

func (f *fakeRepoVisibility) InvalidateSubject(_ context.Context, provider, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purgeCall++
	f.purged = append(f.purged, provider+"|"+subject)
	return f.purgeErr
}

func (f *fakeRepoVisibility) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// memberIdentity is a cookie-session (non-bearer) caller — the only identity
// kind repo filtering applies to.
func memberIdentity() Identity {
	return Identity{
		Subject:   "github:alice",
		UserID:    "00000000-0000-0000-0000-0000000000a1",
		SessionID: "00000000-0000-0000-0000-0000000000a2",
		AccountID: testOperatorAccountID,
	}
}

// withIdentity attaches id to req's context, bypassing bearerAuth for
// handler-direct tests.
func withIdentity(req *http.Request, id Identity) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
}

// visibilityServer builds a Server with a wired mirror and a member (non-admin)
// role, the posture in which filtering actually applies.
func visibilityServer(vis RepoVisibility, providers ProviderResolver) *Server {
	return New(Config{
		Addr:           "127.0.0.1:0",
		AccountRoles:   fakeAccountRoles{role: account.RoleMember},
		RepoVisibility: vis,
		RepoProviders:  providers,
	})
}

func ctxWith(id Identity) context.Context {
	return context.WithValue(context.Background(), ctxKeyIdentity, id)
}

// TestRepoFilterFor_NotApplicable covers every posture in which filtering is
// resolved OFF, including failure modes (f) admin bypass, (g) no mirror wired,
// and (i) bearer/MCP identity.
func TestRepoFilterFor_NotApplicable(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{})
	cases := []struct {
		name string
		srv  *Server
		id   Identity
	}{
		{
			name: "mode g: no mirror wired (untenanted-allow)",
			srv:  New(Config{Addr: "127.0.0.1:0", AccountRoles: fakeAccountRoles{role: account.RoleMember}}),
			id:   memberIdentity(),
		},
		{
			name: "anonymous caller",
			srv:  visibilityServer(vis, nil),
			id:   Identity{Subject: "anonymous"},
		},
		{
			name: "mode i: bearer token identity is unfiltered",
			srv:  visibilityServer(vis, nil),
			id:   Identity{Subject: "svc:ci", TokenID: "tok-1", AccountID: testOperatorAccountID},
		},
		{
			name: "mode i: mcp run token identity is unfiltered",
			srv:  visibilityServer(vis, nil),
			id:   Identity{Subject: "mcp:run:abc", TokenID: "tok-2", AccountID: testOperatorAccountID},
		},
		{
			name: "mode f: workspace admin bypasses filtering",
			srv: New(Config{Addr: "127.0.0.1:0", RepoVisibility: vis,
				AccountRoles: fakeAccountRoles{role: account.RoleAdmin}}),
			id: memberIdentity(),
		},
		{
			name: "subject without a provider prefix",
			srv:  visibilityServer(vis, nil),
			id:   Identity{Subject: "alice", SessionID: "s", AccountID: testOperatorAccountID},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := tc.srv.repoFilterFor(ctxWith(tc.id))
			if err != nil {
				t.Fatalf("repoFilterFor: %v", err)
			}
			if f != nil {
				t.Fatalf("filter = %+v, want nil (filtering not applicable)", f)
			}
			// A nil filter allows unconditionally and asks the mirror nothing.
			ok, err := f.allows(context.Background(), "acme/app")
			if err != nil || !ok {
				t.Fatalf("nil filter allows = (%v, %v), want (true, nil)", ok, err)
			}
			if vis.callCount() != 0 {
				t.Fatalf("mirror consulted %d times, want 0", vis.callCount())
			}
		})
	}
}

// TestRepoFilterFor_RoleResolutionError pins that a role-resolution fault is
// neither an admin bypass nor a deny — it surfaces so the caller 503s.
func TestRepoFilterFor_RoleResolutionError(t *testing.T) {
	srv := New(Config{Addr: "127.0.0.1:0",
		RepoVisibility: newFakeRepoVisibility(map[string]bool{}),
		AccountRoles:   fakeAccountRoles{err: errors.New("db down")}})
	f, err := srv.repoFilterFor(ctxWith(memberIdentity()))
	if err == nil {
		t.Fatalf("err = nil, want a role-resolution error; filter = %+v", f)
	}
}

// TestRepoFilter_Allows covers the per-repo decision, including failure modes
// (a)/(b) — a forge fault surfaces from the mirror as not-visible, so the repo
// is dropped and the request proceeds — and (h), a store fault.
func TestRepoFilter_Allows(t *testing.T) {
	t.Run("visible repo allowed, non-visible denied", func(t *testing.T) {
		vis := newFakeRepoVisibility(map[string]bool{"acme/app": true})
		srv := visibilityServer(vis, nil)
		f, err := srv.repoFilterFor(ctxWith(memberIdentity()))
		if err != nil || f == nil {
			t.Fatalf("repoFilterFor = (%v, %v), want a filter", f, err)
		}
		if ok, _ := f.allows(context.Background(), "acme/app"); !ok {
			t.Error("acme/app: allows = false, want true")
		}
		// Modes (a)/(b): the mirror absorbs a forge fault into false+nil, so
		// the repo is simply not visible and the request proceeds.
		if ok, err := f.allows(context.Background(), "other/repo"); ok || err != nil {
			t.Errorf("other/repo: allows = (%v, %v), want (false, nil)", ok, err)
		}
		// The subject handed to the mirror is the forge-neutral member ref.
		if got := vis.calls[0]; got != "github|alice|acme/app" {
			t.Errorf("mirror call = %q, want github|alice|acme/app", got)
		}
	})

	t.Run("mode h: store fault surfaces for the 503", func(t *testing.T) {
		vis := newFakeRepoVisibility(map[string]bool{})
		vis.err = errors.New("mirror store unavailable")
		srv := visibilityServer(vis, nil)
		f, _ := srv.repoFilterFor(ctxWith(memberIdentity()))
		ok, err := f.allows(context.Background(), "acme/app")
		if ok || err == nil {
			t.Fatalf("allows = (%v, %v), want (false, non-nil error)", ok, err)
		}
		// A store fault must NOT be memoized — the next request may succeed.
		vis.err = nil
		vis.visible["acme/app"] = true
		if ok, err := f.allows(context.Background(), "acme/app"); !ok || err != nil {
			t.Fatalf("after recovery allows = (%v, %v), want (true, nil)", ok, err)
		}
	})

	t.Run("mode e: cross-forge deny makes ZERO forge calls", func(t *testing.T) {
		vis := newFakeRepoVisibility(map[string]bool{"gl-group/app": true})
		srv := visibilityServer(vis, mapProviderResolver{"gl-group/app": "gitlab"})
		f, _ := srv.repoFilterFor(ctxWith(memberIdentity())) // github:alice
		ok, err := f.allows(context.Background(), "gl-group/app")
		if ok || err != nil {
			t.Fatalf("allows = (%v, %v), want (false, nil)", ok, err)
		}
		if vis.callCount() != 0 {
			t.Fatalf("mirror consulted %d times on a cross-forge row, want 0", vis.callCount())
		}
	})

	t.Run("same-forge row consults the mirror", func(t *testing.T) {
		vis := newFakeRepoVisibility(map[string]bool{"acme/app": true})
		srv := visibilityServer(vis, mapProviderResolver{"acme/app": "github"})
		f, _ := srv.repoFilterFor(ctxWith(memberIdentity()))
		if ok, _ := f.allows(context.Background(), "acme/app"); !ok {
			t.Error("allows = false, want true")
		}
		if vis.callCount() != 1 {
			t.Errorf("mirror calls = %d, want 1", vis.callCount())
		}
	})

	t.Run("unresolvable row forge draws no cross-forge conclusion", func(t *testing.T) {
		vis := newFakeRepoVisibility(map[string]bool{"acme/app": true})
		// found=false: unregistered owner, or one registered under BOTH forges.
		srv := visibilityServer(vis, &fakeProviderResolver{})
		f, _ := srv.repoFilterFor(ctxWith(memberIdentity()))
		if ok, err := f.allows(context.Background(), "acme/app"); !ok || err != nil {
			t.Fatalf("allows = (%v, %v), want (true, nil) — the mirror decides", ok, err)
		}
	})

	t.Run("provider-resolution fault surfaces for the 503", func(t *testing.T) {
		vis := newFakeRepoVisibility(map[string]bool{"acme/app": true})
		srv := visibilityServer(vis, &fakeProviderResolver{err: errors.New("db down")})
		f, _ := srv.repoFilterFor(ctxWith(memberIdentity()))
		if ok, err := f.allows(context.Background(), "acme/app"); ok || err == nil {
			t.Fatalf("allows = (%v, %v), want (false, non-nil error)", ok, err)
		}
		if vis.callCount() != 0 {
			t.Errorf("mirror consulted %d times after a resolver fault, want 0", vis.callCount())
		}
	})

	t.Run("per-request memoization asks the mirror once per repo", func(t *testing.T) {
		vis := newFakeRepoVisibility(map[string]bool{"acme/app": true})
		srv := visibilityServer(vis, nil)
		f, _ := srv.repoFilterFor(ctxWith(memberIdentity()))
		for i := 0; i < 5; i++ {
			if ok, _ := f.allows(context.Background(), "acme/app"); !ok {
				t.Fatal("allows = false, want true")
			}
			if ok, _ := f.allows(context.Background(), "other/repo"); ok {
				t.Fatal("allows = true, want false")
			}
		}
		if vis.callCount() != 2 {
			t.Errorf("mirror calls = %d, want 2 (one per distinct repo)", vis.callCount())
		}
	})
}

// TestEnforceRepoVisibility covers the POINT-READ envelope: a non-visible repo
// is 403 repo_forbidden (never a silent 200) and a filter fault is 503.
func TestEnforceRepoVisibility(t *testing.T) {
	cases := []struct {
		name     string
		vis      *fakeRepoVisibility
		repo     string
		wantOK   bool
		wantCode int
		wantErr  string
	}{
		{name: "visible", vis: newFakeRepoVisibility(map[string]bool{"acme/app": true}),
			repo: "acme/app", wantOK: true, wantCode: http.StatusOK},
		{name: "not visible → 403 repo_forbidden",
			vis: newFakeRepoVisibility(map[string]bool{}), repo: "acme/app",
			wantCode: http.StatusForbidden, wantErr: "repo_forbidden"},
		{name: "mode h: store fault → 503 service_unavailable",
			vis:      &fakeRepoVisibility{visible: map[string]bool{}, err: errors.New("db down")},
			repo:     "acme/app",
			wantCode: http.StatusServiceUnavailable, wantErr: "service_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := visibilityServer(tc.vis, nil)
			rec := httptest.NewRecorder()
			req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/x", nil), memberIdentity())
			got := srv.enforceRepoVisibility(rec, req, tc.repo)
			if got != tc.wantOK {
				t.Fatalf("enforceRepoVisibility = %v, want %v", got, tc.wantOK)
			}
			if tc.wantOK {
				return
			}
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			assertErrorCode(t, rec, tc.wantErr)
		})
	}
}
