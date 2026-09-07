package credstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// refreshAS is an httptest token endpoint speaking the RFC 6749 §5.1 /
// §5.2 shapes, with reuse detection: the first presentation of the
// seeded refresh token rotates it; any later presentation of a
// consumed token is invalid_grant, exactly as the real AS behaves.
type refreshAS struct {
	srv       *httptest.Server
	mu        sync.Mutex
	live      string // the currently valid refresh token
	rotations atomic.Int64
	dials     atomic.Int64
	delay     time.Duration
	lastReq   *http.Request
	lastForm  map[string][]string
}

func newRefreshAS(t *testing.T, seedRefresh string) *refreshAS {
	t.Helper()
	as := &refreshAS{live: seedRefresh}
	as.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.dials.Add(1)
		_ = r.ParseForm()
		as.mu.Lock()
		as.lastReq = r.Clone(context.Background())
		as.lastForm = r.PostForm
		presented := r.PostForm.Get("refresh_token")
		if presented == "" || presented != as.live {
			as.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token consumed or unknown"}`))
			return
		}
		n := as.rotations.Add(1)
		as.live = "fhr_rotated_" + itoa(n)
		next := as.live
		as.mu.Unlock()
		time.Sleep(as.delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fho_new_` + itoa(n) + `","token_type":"Bearer","expires_in":900,"refresh_token":"` + next + `","scope":"read:runs write:runs"}`))
	}))
	t.Cleanup(as.srv.Close)
	return as
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

func seedCred(as *refreshAS, issued time.Time, lifetime time.Duration) Credential {
	exp := issued.Add(lifetime)
	return Credential{
		Token:         "fho_old",
		Subject:       "github:octocat",
		Provider:      "github",
		Scopes:        []string{"read:runs"},
		RefreshToken:  "fhr_seed",
		ClientID:      "fishhawk-cli",
		TokenEndpoint: as.srv.URL + "/v0/oauth/token",
		IssuedAt:      &issued,
		ExpiresAt:     &exp,
	}
}

// TestOAuthRefreshRoundTripRefreshesExpiringCredential is the cross-
// boundary walk: a real §5.1 body from the AS, through Refresh, into
// the store, and read back carrying the ROTATED refresh token, the new
// access token, and a recomputed expiry.
func TestOAuthRefreshRoundTripRefreshesExpiringCredential(t *testing.T) {
	withXDG(t)
	as := newRefreshAS(t, "fhr_seed")
	now := time.Now().Truncate(time.Second)
	const backend = "http://localhost:8080"
	if err := Store(backend, seedCred(as, now.Add(-14*time.Minute), 15*time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := refreshStoredAt(context.Background(), nil, backend, now)
	if err != nil {
		t.Fatalf("refreshStoredAt: %v", err)
	}
	if got.Token != "fho_new_1" {
		t.Errorf("Token = %q, want fho_new_1", got.Token)
	}
	stored, err := Load(backend)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "fhr_rotated_1" {
		t.Errorf("stored RefreshToken = %q, want the ROTATED fhr_rotated_1 — an unpersisted rotation is a burned token", stored.RefreshToken)
	}
	if stored.Token != "fho_new_1" {
		t.Errorf("stored Token = %q, want fho_new_1", stored.Token)
	}
	if stored.IssuedAt == nil || !stored.IssuedAt.Equal(now) {
		t.Errorf("stored IssuedAt = %v, want %v", stored.IssuedAt, now)
	}
	if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(now.Add(900*time.Second)) {
		t.Errorf("stored ExpiresAt = %v, want now+900s", stored.ExpiresAt)
	}
	if stored.Subject != "github:octocat" || stored.Provider != "github" {
		t.Errorf("subject/provider not preserved: %+v", stored)
	}
	if len(stored.Scopes) != 2 || stored.Scopes[1] != "write:runs" {
		t.Errorf("scopes not taken from the response: %+v", stored.Scopes)
	}
	if stored.ClientID != "fishhawk-cli" || stored.TokenEndpoint != as.srv.URL+"/v0/oauth/token" {
		t.Errorf("client_id/token_endpoint not preserved: %+v", stored)
	}
	if as.rotations.Load() != 1 {
		t.Errorf("rotations = %d, want 1", as.rotations.Load())
	}
}

// The public-client contract, asserted on the request the AS RECEIVED:
// no client_secret in the form and no Authorization header at all,
// with grant_type/refresh_token/client_id present.
func TestRefresh_SendsNoClientSecretAndNoAuthorizationHeader(t *testing.T) {
	as := newRefreshAS(t, "fhr_seed")
	c := seedCred(as, time.Now(), 15*time.Minute)
	if _, err := Refresh(context.Background(), nil, c); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	if v := as.lastReq.Header.Values("Authorization"); len(v) != 0 {
		t.Errorf("Authorization header sent: %q — the public-client endpoint refuses it", v)
	}
	if _, present := as.lastForm["client_secret"]; present {
		t.Errorf("client_secret sent: %q — the public-client endpoint refuses it", as.lastForm["client_secret"])
	}
	if got := as.lastForm["grant_type"]; len(got) != 1 || got[0] != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", got)
	}
	if got := as.lastForm["refresh_token"]; len(got) != 1 || got[0] != "fhr_seed" {
		t.Errorf("refresh_token = %q, want fhr_seed", got)
	}
	if got := as.lastForm["client_id"]; len(got) != 1 || got[0] != "fishhawk-cli" {
		t.Errorf("client_id = %q, want fishhawk-cli", got)
	}
	if ct := as.lastReq.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// A §5.2 error envelope surfaces as a typed *RefreshError whose Code
// is the AS's code — error IDENTITY, not just non-nil.
func TestRefresh_ErrorEnvelopeCarriesCode(t *testing.T) {
	as := newRefreshAS(t, "fhr_seed")
	c := seedCred(as, time.Now(), 15*time.Minute)
	c.RefreshToken = "fhr_consumed" // never the live one → invalid_grant
	_, err := Refresh(context.Background(), nil, c)
	var rerr *RefreshError
	if !errors.As(err, &rerr) {
		t.Fatalf("want *RefreshError, got %T: %v", err, err)
	}
	if rerr.Code != "invalid_grant" {
		t.Errorf("Code = %q, want invalid_grant", rerr.Code)
	}
	if rerr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", rerr.StatusCode)
	}
	if !strings.Contains(rerr.Error(), "invalid_grant") || !strings.Contains(rerr.Error(), "consumed") {
		t.Errorf("Error() = %q, want the code and description", rerr.Error())
	}
}

func TestRefresh_NonJSONBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	t.Cleanup(srv.Close)
	c := Credential{Token: "fho_a", RefreshToken: "fhr_a", ClientID: "c", TokenEndpoint: srv.URL}
	got, err := Refresh(context.Background(), nil, c)
	if err == nil {
		t.Fatalf("want an error on a non-JSON 200, got credential %+v", got)
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("error = %q, want it to name the non-JSON body", err)
	}
	// A non-2xx non-JSON body is a RefreshError with no code and a snippet.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	t.Cleanup(srv2.Close)
	c.TokenEndpoint = srv2.URL
	_, err = Refresh(context.Background(), nil, c)
	var rerr *RefreshError
	if !errors.As(err, &rerr) || rerr.StatusCode != http.StatusBadGateway || rerr.Code != "" {
		t.Fatalf("want a codeless *RefreshError with 502, got %T: %v", err, err)
	}
	if !strings.Contains(rerr.Description, "upstream down") {
		t.Errorf("Description = %q, want the body snippet", rerr.Description)
	}
}

// A 2xx with no access_token must never become a silently empty bearer.
func TestRefresh_MissingAccessTokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":900,"refresh_token":"fhr_x"}`))
	}))
	t.Cleanup(srv.Close)
	c := Credential{Token: "fho_a", RefreshToken: "fhr_a", ClientID: "c", TokenEndpoint: srv.URL}
	got, err := Refresh(context.Background(), nil, c)
	if err == nil {
		t.Fatalf("want an error when access_token is absent, got %+v", got)
	}
	if got.Token != "" {
		t.Errorf("must not return a credential on error; got token %q", got.Token)
	}
	if !strings.Contains(err.Error(), "no access_token") {
		t.Errorf("error = %q, want it to name the missing access_token", err)
	}
}

// A non-refreshable credential is refused BEFORE any dial.
func TestRefresh_NotRefreshableNeverDials(t *testing.T) {
	as := newRefreshAS(t, "fhr_seed")
	c := seedCred(as, time.Now(), 15*time.Minute)
	c.RefreshToken = ""
	_, err := Refresh(context.Background(), nil, c)
	if !errors.Is(err, ErrNotRefreshable) {
		t.Fatalf("want ErrNotRefreshable, got %v", err)
	}
	if as.dials.Load() != 0 {
		t.Errorf("dials = %d, want 0", as.dials.Load())
	}
}

// A non-rotating AS (no refresh_token in the response) leaves the
// presented refresh token valid per §6; an absent expires_in reads as
// the shipped default lifetime, never as non-expiring.
func TestRefresh_NonRotatingASKeepsRefreshTokenAndDefaultsLifetime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fho_n","token_type":"Bearer"}`))
	}))
	t.Cleanup(srv.Close)
	now := time.Now()
	c := Credential{Token: "fho_a", RefreshToken: "fhr_keep", ClientID: "c", TokenEndpoint: srv.URL}
	got, err := refreshAt(context.Background(), nil, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "fhr_keep" {
		t.Errorf("RefreshToken = %q, want the presented one kept", got.RefreshToken)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(now.Add(DefaultLifetime)) {
		t.Errorf("ExpiresAt = %v, want now+DefaultLifetime", got.ExpiresAt)
	}
}

// RefreshStored: a credential outside its skew is returned unchanged
// without a dial.
func TestRefreshStored_NotNeededNeverDials(t *testing.T) {
	withXDG(t)
	as := newRefreshAS(t, "fhr_seed")
	now := time.Now()
	const backend = "http://localhost:8080"
	if err := Store(backend, seedCred(as, now, 15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := refreshStoredAt(context.Background(), nil, backend, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "fho_old" || as.dials.Load() != 0 {
		t.Fatalf("fresh credential must be returned as-is with no dial; got %+v dials=%d", got, as.dials.Load())
	}
}

// RefreshStored: a refused refresh surfaces the typed error and leaves
// the store byte-untouched (read back), so a transient refusal never
// erases a credential.
func TestRefreshStored_FailureLeavesStoreUntouched(t *testing.T) {
	withXDG(t)
	as := newRefreshAS(t, "fhr_other") // the stored fhr_seed is never live → invalid_grant
	now := time.Now()
	const backend = "http://localhost:8080"
	seed := seedCred(as, now.Add(-time.Hour), 15*time.Minute)
	if err := Store(backend, seed); err != nil {
		t.Fatal(err)
	}
	_, err := refreshStoredAt(context.Background(), nil, backend, now)
	var rerr *RefreshError
	if !errors.As(err, &rerr) || rerr.Code != "invalid_grant" {
		t.Fatalf("want *RefreshError invalid_grant, got %v", err)
	}
	stored, err := Load(backend)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token != "fho_old" || stored.RefreshToken != "fhr_seed" {
		t.Fatalf("store mutated on a failed refresh: %+v", stored)
	}
}

func TestRefreshStored_NotFoundPropagates(t *testing.T) {
	withXDG(t)
	_, err := RefreshStored(context.Background(), nil, "http://localhost:8080")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestRefreshStored_TwoConsumersRotateOnce is the multi-process
// condition: two consumers sharing one store race the refresh. The AS
// delays its response so both are inside the sequence at once; the
// exclusive lock serializes them, and the loser RE-READS the winner's
// stored credential instead of presenting the consumed token. Exactly
// ONE rotation happens and both end with the same valid credential.
// Deleting the lock, or moving the Load ahead of it, makes the loser
// present fhr_seed after it was consumed → invalid_grant → RED.
func TestRefreshStored_TwoConsumersRotateOnce(t *testing.T) {
	withXDG(t)
	as := newRefreshAS(t, "fhr_seed")
	as.delay = 300 * time.Millisecond
	now := time.Now()
	const backend = "http://localhost:8080"
	if err := Store(backend, seedCred(as, now.Add(-14*time.Minute), 15*time.Minute)); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		cred Credential
		err  error
	}
	results := make([]outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			c, err := refreshStoredAt(context.Background(), nil, backend, now)
			results[i] = outcome{c, err}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("consumer %d: %v (the loser re-presented a consumed refresh token)", i, r.err)
		}
	}
	if results[0].cred.Token != results[1].cred.Token || results[0].cred.RefreshToken != results[1].cred.RefreshToken {
		t.Fatalf("consumers ended with different credentials: %+v vs %+v", results[0].cred, results[1].cred)
	}
	if results[0].cred.Token != "fho_new_1" || results[0].cred.RefreshToken != "fhr_rotated_1" {
		t.Fatalf("consumers must hold the rotated credential; got %+v", results[0].cred)
	}
	if n := as.rotations.Load(); n != 1 {
		t.Fatalf("rotations = %d, want exactly 1", n)
	}
	stored, err := Load(backend)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "fhr_rotated_1" {
		t.Fatalf("stored RefreshToken = %q, want fhr_rotated_1", stored.RefreshToken)
	}
}

// --- redirect refusal (credential-exfiltration egress) ---------------

// TestRefresh_RefusesRedirectAndLeaksNothingToTheHOP is the behavioral
// assertion the security concern asks for: the "unauthorized
// destination receives no secrets". The hop is a REACHABLE in-test
// server (an unreachable address would fail with a dial error that maps
// to the same refusal shape whether or not the control exists), it
// records every request it sees, and the assertion is that it saw ZERO
// — no refresh_token, no client_id, not even a bare request line.
func TestRefresh_RefusesRedirectAndLeaksNothingToTheHOP(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		t.Run(itoa(int64(status)), func(t *testing.T) {
			var hopHits atomic.Int64
			var hopBodies []string
			var hopMu sync.Mutex
			hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hopHits.Add(1)
				b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
				hopMu.Lock()
				hopBodies = append(hopBodies, string(b))
				hopMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"stolen","refresh_token":"stolen","expires_in":900}`))
			}))
			defer hop.Close()

			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, hop.URL+"/v0/oauth/token", status)
			}))
			defer redirector.Close()

			issued := time.Now().Add(-14 * time.Minute)
			exp := issued.Add(15 * time.Minute)
			cred := Credential{
				Token:         "fho_old",
				RefreshToken:  "fhr_secret_seed",
				ClientID:      "fishhawk-cli",
				TokenEndpoint: redirector.URL + "/v0/oauth/token",
				IssuedAt:      &issued,
				ExpiresAt:     &exp,
			}
			got, err := Refresh(context.Background(), nil, cred)
			if err == nil {
				t.Fatalf("want a refusal, got credential %+v", got)
			}
			var rr *RedirectRefusedError
			if !errors.As(err, &rr) {
				t.Fatalf("want *RedirectRefusedError, got %T: %v", err, err)
			}
			if n := hopHits.Load(); n != 0 {
				hopMu.Lock()
				bodies := hopBodies
				hopMu.Unlock()
				t.Fatalf("the redirect destination received %d request(s); bodies=%q — the refresh token was exfiltrated", n, bodies)
			}
			if got.Token != "" || got.RefreshToken != "" {
				t.Fatalf("a refused refresh must yield no credential; got %+v", got)
			}
		})
	}
}

// TestRefresh_CallerSuppliedRedirectPolicyCannotOptOut pins that the
// control is not a default a caller can override: a client handed in
// with a permissive CheckRedirect is still refused, and the caller's
// own client is left unmutated.
func TestRefresh_CallerSuppliedRedirectPolicyCannotOptOut(t *testing.T) {
	var hopHits atomic.Int64
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hopHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stolen","expires_in":900}`))
	}))
	defer hop.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, hop.URL+"/v0/oauth/token", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	permissive := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	issued := time.Now().Add(-14 * time.Minute)
	exp := issued.Add(15 * time.Minute)
	_, err := Refresh(context.Background(), permissive, Credential{
		Token:         "fho_old",
		RefreshToken:  "fhr_secret_seed",
		ClientID:      "fishhawk-cli",
		TokenEndpoint: redirector.URL + "/v0/oauth/token",
		IssuedAt:      &issued,
		ExpiresAt:     &exp,
	})
	var rr *RedirectRefusedError
	if !errors.As(err, &rr) {
		t.Fatalf("want *RedirectRefusedError even with a permissive caller client, got %v", err)
	}
	if n := hopHits.Load(); n != 0 {
		t.Fatalf("the redirect destination received %d request(s)", n)
	}
	if permissive.CheckRedirect == nil {
		t.Fatal("credstore must not mutate the caller's http.Client")
	}
	if permissive.CheckRedirect(nil, nil) != nil {
		t.Fatal("the caller's own CheckRedirect was replaced in place")
	}
}

// --- bounded, cancellable lock wait ----------------------------------

// TestRefreshStored_LockWaitIsBoundedAndCancellable holds
// credentials.lock on an INDEPENDENT file descriptor for longer than
// the caller's deadline — the suspended-peer case — and asserts
// RefreshStored honors ctx instead of blocking in flock. With a
// blocking LOCK_EX the call cannot return until the holder releases, so
// the elapsed assertion goes RED (and the whole test times out).
func TestRefreshStored_LockWaitIsBoundedAndCancellable(t *testing.T) {
	credPath := withXDG(t)
	as := newRefreshAS(t, "fhr_seed")
	now := time.Now()
	const backend = "http://localhost:8080"
	if err := Store(backend, seedCred(as, now.Add(-14*time.Minute), 15*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// An independent holder, taken directly rather than through
	// RefreshStored, so the RED lands on the behavioral assertion and not
	// on a fixture-setup guard.
	lockFile := credPath + ".lock"
	f, err := os.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	go func() {
		<-held
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}()
	defer close(held)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = refreshStoredAt(ctx, nil, backend, now)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want a lock-wait failure while the lock is independently held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("lock wait took %s; it neither honored the deadline nor stayed bounded", elapsed)
	}
	if n := as.dials.Load(); n != 0 {
		t.Fatalf("no token endpoint may be dialed when the lock was never acquired; dials=%d", n)
	}
}

// --- login write vs refresh rotation ---------------------------------

// TestStore_SerializesWithRefreshRotation is the login/store-versus-
// refresh race in COMMITTED STATE. A refresh rotates the credential for
// one backend while a login writes a credential for ANOTHER backend
// into the same shared JSON file. storeMergeBarrier pins the writer
// inside the read-merge-write window the lock exists to close, so the
// interleaving is deterministic rather than clock-raced.
//
// With Store taking the lock, the login write cannot enter that window
// until the rotation is committed, so BOTH survive. Without it the
// login reads the pre-rotation map, waits, and renames its stale merge
// over the rotation — restoring a refresh token the AS has consumed.
func TestStore_SerializesWithRefreshRotation(t *testing.T) {
	withXDG(t)
	as := newRefreshAS(t, "fhr_seed")
	as.delay = 300 * time.Millisecond
	now := time.Now()
	const refreshBackend = "http://localhost:8080"
	const loginBackend = "http://localhost:9090"
	if err := Store(refreshBackend, seedCred(as, now.Add(-14*time.Minute), 15*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rotated := make(chan struct{})
	storeMergeBarrier = func(backendURL string) {
		if backendURL != loginBackend {
			return
		}
		select {
		case <-rotated:
		case <-time.After(10 * time.Second):
		}
	}
	t.Cleanup(func() { storeMergeBarrier = nil })

	var wg sync.WaitGroup
	var refreshErr, loginErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, refreshErr = refreshStoredAt(context.Background(), nil, refreshBackend, now)
		close(rotated)
	}()
	// Start the login write only once the refresh is demonstrably INSIDE
	// its locked sequence (the token endpoint has been dialed). Racing the
	// two starts would let the login take the lock first, and the barrier
	// would then wait on a rotation that cannot proceed. The window under
	// test is the login's read-merge-write, not who calls first.
	for as.dials.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		loginErr = Store(loginBackend, Credential{Token: "fho_login", RefreshToken: "fhr_login", ClientID: "fishhawk-cli", TokenEndpoint: as.srv.URL + "/v0/oauth/token"})
	}()
	wg.Wait()

	if refreshErr != nil {
		t.Fatalf("refresh: %v", refreshErr)
	}
	if loginErr != nil {
		t.Fatalf("login store: %v", loginErr)
	}

	got, err := Load(refreshBackend)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "fhr_rotated_1" {
		t.Fatalf("stored RefreshToken for %s = %q, want fhr_rotated_1: the login write clobbered the rotation and restored a consumed refresh token", refreshBackend, got.RefreshToken)
	}
	login, err := Load(loginBackend)
	if err != nil {
		t.Fatal(err)
	}
	if login.Token != "fho_login" {
		t.Fatalf("stored login credential = %+v, want Token fho_login", login)
	}
}
