package gitlabclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// countingProjectServer is an httptest server answering GET
// /api/v4/projects/:path with a fixed project body, counting every request it
// receives and recording the Authorization/PRIVATE-TOKEN header. It is the
// outbound-host observation point: the cross-boundary tests assert a resolved
// base routes a real method here, and the fail-closed tests assert it receives
// ZERO requests (no token ever ships).
func countingProjectServer(t *testing.T) (*httptest.Server, *int64, *string) {
	t.Helper()
	var count int64
	var gotToken string
	// TLS so the resolved-base URL passes account.ValidateResolvedBaseURL's
	// https requirement; the paired srv.Client() trusts the ephemeral cert.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":7,"web_url":"https://x/grp/proj"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &count, &gotToken
}

// TestFactory_Client_RoutesToResolvedHost is the cross-boundary happy path:
// resolver -> Factory.Client -> a real GetProject call reaches the RESOLVED
// host (an httptest server distinct from the deployment default), the resolver
// is keyed on the installation ref, and the PRIVATE-TOKEN ships to the
// resolved host.
func TestFactory_Client_RoutesToResolvedHost(t *testing.T) {
	resolvedSrv, resolvedCount, gotToken := countingProjectServer(t)
	defaultSrv, defaultCount, _ := countingProjectServer(t)

	var gotRef string
	fac := NewFactory(defaultSrv.URL,
		WithFactoryHTTPClient(resolvedSrv.Client()),
		WithFactoryResolveBaseURL(func(_ context.Context, ref string) (string, error) {
			gotRef = ref
			return resolvedSrv.URL, nil
		}),
	)

	c, err := fac.Client(context.Background(), "gitlab:123", "glpat-secret")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if _, err := c.GetProject(context.Background(), "grp/proj"); err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	if gotRef != "gitlab:123" {
		t.Errorf("resolver got ref = %q, want %q", gotRef, "gitlab:123")
	}
	if atomic.LoadInt64(resolvedCount) != 1 {
		t.Errorf("resolved host received %d requests, want 1", atomic.LoadInt64(resolvedCount))
	}
	if atomic.LoadInt64(defaultCount) != 0 {
		t.Errorf("default host received %d requests, want 0 (routed to resolved host)", atomic.LoadInt64(defaultCount))
	}
	if *gotToken != "glpat-secret" {
		t.Errorf("resolved host saw PRIVATE-TOKEN = %q, want glpat-secret", *gotToken)
	}
}

// TestFactory_Client_FailClosed pins the per-mode fail-closed contract (#2094
// binding condition 1): a resolver DB fault, a disallowed host (allowlist
// miss), and a bad scheme (http / relative) each make Client return an error,
// construct NO client, and issue NO request — the token never ships.
func TestFactory_Client_FailClosed(t *testing.T) {
	sentinel := errors.New("boom: db fault")
	for _, tc := range []struct {
		name      string
		allowlist []string
		resolve   func(context.Context, string) (string, error)
		wantWrap  error // non-nil → assert Client's error wraps it
	}{
		{
			name:     "resolver db fault",
			resolve:  func(context.Context, string) (string, error) { return "", sentinel },
			wantWrap: sentinel,
		},
		{
			name:      "disallowed host (allowlist miss)",
			allowlist: []string{"allowed.example.com"},
			resolve:   func(context.Context, string) (string, error) { return "https://evil.example.com", nil },
		},
		{
			name:    "bad scheme (http)",
			resolve: func(context.Context, string) (string, error) { return "http://insecure.example.com", nil },
		},
		{
			name:    "bad scheme (relative)",
			resolve: func(context.Context, string) (string, error) { return "/api/v4", nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A live server whose Doer is threaded in: if the fail-closed
			// branch leaked a constructed client, a request could reach here.
			srv, count, gotToken := countingProjectServer(t)

			fac := NewFactory(srv.URL,
				WithFactoryHTTPClient(srv.Client()),
				WithFactoryResolveBaseURL(tc.resolve),
				WithFactoryAllowedInstallationHosts(tc.allowlist),
			)

			c, err := fac.Client(context.Background(), "gitlab:123", "glpat-secret")
			if err == nil {
				t.Fatal("Client succeeded, want a fail-closed error")
			}
			if c != nil {
				t.Errorf("Client returned a non-nil client on the fail-closed path, want nil")
			}
			if tc.wantWrap != nil && !errors.Is(err, tc.wantWrap) {
				t.Errorf("err = %v, want it to wrap the resolver error", err)
			}
			if atomic.LoadInt64(count) != 0 {
				t.Errorf("server received %d requests, want 0 (fail-closed, no request issued)", atomic.LoadInt64(count))
			}
			if *gotToken != "" {
				t.Errorf("server saw PRIVATE-TOKEN = %q, want empty (token must never ship on fail-closed)", *gotToken)
			}
		})
	}
}

// TestFactory_Client_BackwardCompat pins the deployment-default posture: a nil
// resolver, an empty installation ref, and an empty resolved base (NULL column
// / unknown installation) each leave the constructed client on the deployment
// default host, byte-identical to Mode 1.
func TestFactory_Client_BackwardCompat(t *testing.T) {
	t.Run("nil resolver", func(t *testing.T) {
		defaultSrv, defaultCount, _ := countingProjectServer(t)
		fac := NewFactory(defaultSrv.URL, WithFactoryHTTPClient(defaultSrv.Client()))

		c, err := fac.Client(context.Background(), "gitlab:123", "glpat-secret")
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		if _, err := c.GetProject(context.Background(), "grp/proj"); err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if atomic.LoadInt64(defaultCount) != 1 {
			t.Errorf("default host received %d requests, want 1 (nil resolver -> default)", atomic.LoadInt64(defaultCount))
		}
	})

	t.Run("empty installation ref skips resolver", func(t *testing.T) {
		defaultSrv, defaultCount, _ := countingProjectServer(t)
		consulted := false
		fac := NewFactory(defaultSrv.URL,
			WithFactoryHTTPClient(defaultSrv.Client()),
			WithFactoryResolveBaseURL(func(context.Context, string) (string, error) {
				consulted = true
				return "https://elsewhere.example.com", nil
			}),
		)

		c, err := fac.Client(context.Background(), "", "glpat-secret")
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		if _, err := c.GetProject(context.Background(), "grp/proj"); err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if consulted {
			t.Error("resolver was consulted for an empty installation ref, want it skipped (deployment default)")
		}
		if atomic.LoadInt64(defaultCount) != 1 {
			t.Errorf("default host received %d requests, want 1 (empty ref -> default)", atomic.LoadInt64(defaultCount))
		}
	})

	t.Run("empty resolved base keeps default", func(t *testing.T) {
		defaultSrv, defaultCount, _ := countingProjectServer(t)
		consulted := false
		fac := NewFactory(defaultSrv.URL,
			WithFactoryHTTPClient(defaultSrv.Client()),
			WithFactoryResolveBaseURL(func(context.Context, string) (string, error) {
				consulted = true
				return "", nil // NULL column / unknown installation
			}),
		)

		c, err := fac.Client(context.Background(), "gitlab:123", "glpat-secret")
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		if _, err := c.GetProject(context.Background(), "grp/proj"); err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if !consulted {
			t.Error("resolver was not consulted, want it consulted for the empty-override branch")
		}
		if atomic.LoadInt64(defaultCount) != 1 {
			t.Errorf("default host received %d requests, want 1 (empty resolved base -> default)", atomic.LoadInt64(defaultCount))
		}
	})
}
