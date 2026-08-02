package oauthas

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// validDocJSON returns a Claude Code-shaped CIMD document that also carries fields
// this package does not model (proving lenient decoding).
func validDocJSON(clientID string) string {
	return `{
		"client_id": "` + clientID + `",
		"client_name": "Claude Code",
		"redirect_uris": ["http://127.0.0.1/callback", "http://localhost/cb"],
		"grant_types": ["authorization_code", "refresh_token"],
		"response_types": ["code"],
		"token_endpoint_auth_method": "none",
		"scope": "read:runs",
		"software_id": "com.anthropic.claude-code",
		"unmodelled": {"nested": true}
	}`
}

// newDocServer starts a TLS server serving handler and returns it plus its URL
// (which is the self-referential client_id).
func newDocServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestFetch_Succeeds(t *testing.T) {
	t.Parallel()
	var clientID string
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, validDocJSON(clientID))
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	doc, err := f.Fetch(context.Background(), clientID)
	if err != nil {
		t.Fatalf("Fetch = %v, want success", err)
	}
	if doc.ClientID != clientID {
		t.Fatalf("ClientID = %q, want %q", doc.ClientID, clientID)
	}
	if len(doc.RedirectURIs) != 2 {
		t.Fatalf("RedirectURIs = %v", doc.RedirectURIs)
	}
	if doc.TokenEndpointAuthMethod != "none" {
		t.Fatalf("TokenEndpointAuthMethod = %q", doc.TokenEndpointAuthMethod)
	}
	if doc.ClientName != "Claude Code" {
		t.Fatalf("ClientName = %q", doc.ClientName)
	}
}

func TestFetch_RefusesNonHTTPS(t *testing.T) {
	t.Parallel()
	f := &Fetcher{}
	_, err := f.Fetch(context.Background(), "http://client.example/id")
	if err == nil {
		t.Fatalf("Fetch(http) = nil, want refusal")
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetch_RefusesNonSelfReferentialDocument(t *testing.T) {
	t.Parallel()
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, validDocJSON("https://evil.example/other"))
	})
	f := &Fetcher{HTTPClient: srv.Client()}
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil || !errors.Is(err, ErrNotSelfReferential) {
		t.Fatalf("Fetch = %v, want ErrNotSelfReferential", err)
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetch_RefusesOversizeBody(t *testing.T) {
	t.Parallel()
	var clientID string
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, validDocJSON(clientID))
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client(), MaxBytes: 8}
	_, err := f.Fetch(context.Background(), clientID)
	if err == nil || !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("Fetch = %v, want ErrDocumentTooLarge", err)
	}
}

func TestFetch_RefusesOnTimeout(t *testing.T) {
	t.Parallel()
	var clientID string
	handlerWedge := timescale.D(400 * time.Millisecond)
	srv := newDocServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(handlerWedge):
			writeJSON(w, http.StatusOK, validDocJSON(clientID))
		case <-r.Context().Done():
		}
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client(), Timeout: timescale.D(80 * time.Millisecond)}
	start := time.Now()
	_, err := f.Fetch(context.Background(), clientID)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Fetch = nil, want timeout error")
	}
	if elapsed >= timescale.D(2*time.Second) {
		t.Fatalf("Fetch took %v, expected it to return near the timeout", elapsed)
	}
}

func TestFetch_RefusesMissingRedirectURIs(t *testing.T) {
	t.Parallel()
	var clientID string
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"client_id":"`+clientID+`","token_endpoint_auth_method":"none"}`)
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	_, err := f.Fetch(context.Background(), clientID)
	if err == nil {
		t.Fatalf("Fetch = nil, want refusal for missing redirect_uris")
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetch_RefusesUnsupportedTokenEndpointAuthMethod(t *testing.T) {
	t.Parallel()
	var clientID string
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"client_id":"`+clientID+`","redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"client_secret_basic"}`)
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	_, err := f.Fetch(context.Background(), clientID)
	if err == nil {
		t.Fatalf("Fetch = nil, want refusal for client_secret_basic")
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetch_RefusesAbsentTokenEndpointAuthMethod(t *testing.T) {
	t.Parallel()
	var clientID string
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"client_id":"`+clientID+`","redirect_uris":["http://127.0.0.1/cb"]}`)
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	_, err := f.Fetch(context.Background(), clientID)
	if err == nil {
		t.Fatalf("Fetch = nil, want refusal for absent token_endpoint_auth_method (defaults to client_secret_basic)")
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetch_RefusesPrivateConnectAddress(t *testing.T) {
	t.Parallel()
	// No injected HTTPClient: the Fetcher builds a guarded dialer; the Lookup seam
	// resolves to a private address, which the default guard refuses.
	f := &Fetcher{Lookup: staticLookup(netip.MustParseAddr("10.0.0.5"))}
	_, err := f.Fetch(context.Background(), "https://client.example/id")
	if err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("Fetch = %v, want ErrBlockedAddress", err)
	}
}

// TestFetch_RefusesDNSRebindingPublicThenPrivate is the falsifying test for the
// pre-flight-resolve defect. It drives the Fetcher's guarded dialer directly so it
// can observe the single resolution and the pinned literal.
func TestFetch_RefusesDNSRebindingPublicThenPrivate(t *testing.T) {
	t.Parallel()
	public := netip.MustParseAddr("93.184.216.34")
	private := netip.MustParseAddr("127.0.0.1")

	// Part 1: consumed (first) answer public → the dial pins the public literal and
	// the host is resolved exactly ONCE. A pre-flight-resolve regression would
	// resolve twice and let the transport connect to the private second answer.
	var calls int
	lookup := func(context.Context, string) ([]netip.Addr, error) {
		calls++
		if calls == 1 {
			return []netip.Addr{public}, nil
		}
		return []netip.Addr{private}, nil
	}
	gd := newGuardedDialer(PublicOnlyAddrGuard, lookup)
	var dialed string
	gd.dialRaw = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errStopDial
	}
	if _, err := gd.DialContext(context.Background(), "tcp", "rebind.example:443"); !errors.Is(err, errStopDial) {
		t.Fatalf("expected recorder sentinel, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("host resolved %d times, want exactly 1 (re-resolution is the rebinding hole)", calls)
	}
	if want := net.JoinHostPort(public.String(), "443"); dialed != want {
		t.Fatalf("dialed %q, want pinned public literal %q", dialed, want)
	}

	// Part 2: when the consumed answer is private, the dial is refused and never
	// attempted.
	gd2 := newGuardedDialer(PublicOnlyAddrGuard, staticLookup(private))
	gd2.dialRaw = failDial(t)
	if _, err := gd2.DialContext(context.Background(), "tcp", "rebind.example:443"); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("private consumed answer = %v, want ErrBlockedAddress", err)
	}
}

func TestFetch_RefusesRedirectSchemeDowngrade(t *testing.T) {
	t.Parallel()
	srv := newDocServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/downgraded", http.StatusFound)
	})
	f := &Fetcher{HTTPClient: srv.Client()}
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatalf("Fetch = nil, want refusal for https->http redirect")
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetch_RefusesRedirectToUserinfoOrFragment(t *testing.T) {
	t.Parallel()
	for _, loc := range []string{"https://user@ok.example/cb", "https://ok.example/cb#frag"} {
		location := loc
		srv := newDocServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, location, http.StatusFound)
		})
		f := &Fetcher{HTTPClient: srv.Client()}
		if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
			t.Fatalf("Fetch redirect to %q = nil, want refusal", location)
		}
	}
}

func TestFetch_RefusesTooManyRedirects(t *testing.T) {
	t.Parallel()
	srv := newDocServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Always redirect (relative, so it stays on the same https loopback origin
		// and keeps redirecting); the hop cap must trip before the loop runs away.
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	f := &Fetcher{HTTPClient: srv.Client(), MaxRedirects: 1}
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatalf("Fetch = nil, want redirect-limit refusal")
	}
}

func TestFetch_RefusesNon200(t *testing.T) {
	t.Parallel()
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	f := &Fetcher{HTTPClient: srv.Client()}
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatalf("Fetch = nil, want refusal for non-200")
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetch_RefusesWrongContentType(t *testing.T) {
	t.Parallel()
	var clientID string
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, validDocJSON(clientID))
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	_, err := f.Fetch(context.Background(), clientID)
	if err == nil {
		t.Fatalf("Fetch = nil, want refusal for non-JSON content type")
	}
	assertCode(t, err, ErrCodeInvalidClient)
}

func TestFetcher_DefaultGuardIsPublicOnly(t *testing.T) {
	t.Parallel()
	f := &Fetcher{}
	got := reflect.ValueOf(f.guard()).Pointer()
	want := reflect.ValueOf(PublicOnlyAddrGuard).Pointer()
	if got != want {
		t.Fatalf("zero-value Fetcher's default guard is not PublicOnlyAddrGuard")
	}
}

func TestFetch_CachesValidatedDocument(t *testing.T) {
	t.Parallel()
	var clientID string
	var hits int32
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(w, http.StatusOK, validDocJSON(clientID))
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	if _, err := f.Fetch(context.Background(), clientID); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if _, err := f.Fetch(context.Background(), clientID); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server hit %d times, want 1 (second call should be cached)", got)
	}
}

// TestFetch_CacheReturnsDeepCopy is the #2427 condition-2 pin: mutating a returned
// document must not poison a subsequent cache hit.
func TestFetch_CacheReturnsDeepCopy(t *testing.T) {
	t.Parallel()
	var clientID string
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, validDocJSON(clientID))
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	first, err := f.Fetch(context.Background(), clientID)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	// Poison the returned document's slices.
	first.RedirectURIs[0] = "https://attacker.example/steal"
	first.RedirectURIs = append(first.RedirectURIs, "https://attacker.example/more")
	first.GrantTypes[0] = "implicit"

	second, err := f.Fetch(context.Background(), clientID)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if second.RedirectURIs[0] != "http://127.0.0.1/callback" || len(second.RedirectURIs) != 2 {
		t.Fatalf("cache hit was poisoned: RedirectURIs = %v", second.RedirectURIs)
	}
	if second.GrantTypes[0] != "authorization_code" {
		t.Fatalf("cache hit was poisoned: GrantTypes = %v", second.GrantTypes)
	}
}

func TestFetch_CacheExpiresAtTTL(t *testing.T) {
	t.Parallel()
	var clientID string
	var hits int32
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(w, http.StatusOK, validDocJSON(clientID))
	})
	clientID = srv.URL
	now := time.Unix(1_000_000, 0)
	f := &Fetcher{HTTPClient: srv.Client(), CacheTTL: time.Minute, Now: func() time.Time { return now }}
	if _, err := f.Fetch(context.Background(), clientID); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	now = now.Add(time.Minute) // exactly at TTL → expired
	if _, err := f.Fetch(context.Background(), clientID); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hit %d times, want 2 (entry should expire at TTL)", got)
	}
}

func TestFetch_DoesNotCacheFailures(t *testing.T) {
	t.Parallel()
	var clientID string
	var n int32
	srv := newDocServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, validDocJSON(clientID))
	})
	clientID = srv.URL
	f := &Fetcher{HTTPClient: srv.Client()}
	if _, err := f.Fetch(context.Background(), clientID); err == nil {
		t.Fatalf("first Fetch = nil, want failure")
	}
	doc, err := f.Fetch(context.Background(), clientID)
	if err != nil {
		t.Fatalf("second Fetch = %v, want success (failure must not be cached)", err)
	}
	if doc.ClientID != clientID {
		t.Fatalf("second Fetch ClientID = %q", doc.ClientID)
	}
}

func TestValidateClientIDURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "https ok", in: "https://client.example/id"},
		{name: "https query ok", in: "https://client.example/id?v=1"},
		{name: "http refused", in: "http://client.example/id", wantErr: true},
		{name: "userinfo refused", in: "https://u@client.example/id", wantErr: true},
		{name: "fragment refused", in: "https://client.example/id#f", wantErr: true},
		{name: "empty-fragment refused", in: "https://client.example/id#", wantErr: true},
		{name: "hostless refused", in: "https:///id", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ValidateClientIDURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateClientIDURL(%q) = nil, want error", tc.in)
				}
				assertCode(t, err, ErrCodeInvalidClient)
				return
			}
			if err != nil {
				t.Fatalf("ValidateClientIDURL(%q) = %v", tc.in, err)
			}
		})
	}
}
