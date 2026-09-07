package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// withXDG points the store at a throwaway dir for the duration of a
// test via XDG_CONFIG_HOME, returning the expected credentials path.
func withXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, dirName, fileName)
}

func TestStoreLoadRoundTrip(t *testing.T) {
	withXDG(t)

	exp := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	want := Credential{
		Token:     "fhk_abc123",
		Subject:   "github:octocat",
		Scopes:    []string{"read:runs", "write:approvals"},
		Provider:  "github",
		ExpiresAt: &exp,
	}
	const backend = "http://localhost:8080"

	if err := Store(backend, want); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load(backend)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Token != want.Token || got.Subject != want.Subject || got.Provider != want.Provider {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "read:runs" {
		t.Fatalf("scopes not preserved: %+v", got.Scopes)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expiry not preserved: %v", got.ExpiresAt)
	}
}

// A trailing slash on the backend URL must address the same key.
func TestLoadNormalizesTrailingSlash(t *testing.T) {
	withXDG(t)
	if err := Store("http://localhost:8080/", Credential{Token: "fhk_x"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load("http://localhost:8080")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Token != "fhk_x" {
		t.Fatalf("normalized lookup failed: %+v", got)
	}
}

func TestLoadNotFound(t *testing.T) {
	withXDG(t)
	// No store file at all.
	if _, err := Load("http://localhost:8080"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound on empty store, got %v", err)
	}
	// Store a different backend, then miss on ours.
	if err := Store("http://other:9090", Credential{Token: "fhk_o"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := Load("http://localhost:8080"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for absent backend, got %v", err)
	}
}

// Storing two backends keeps both; a re-store of one overwrites only
// that key.
func TestStoreMergesAndOverwrites(t *testing.T) {
	withXDG(t)
	if err := Store("http://a:8080", Credential{Token: "fhk_a1"}); err != nil {
		t.Fatalf("Store a: %v", err)
	}
	if err := Store("http://b:8080", Credential{Token: "fhk_b1"}); err != nil {
		t.Fatalf("Store b: %v", err)
	}
	if err := Store("http://a:8080", Credential{Token: "fhk_a2"}); err != nil {
		t.Fatalf("re-store a: %v", err)
	}

	all, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 backends, got %d: %+v", len(all), all)
	}
	if all["http://a:8080"].Token != "fhk_a2" {
		t.Fatalf("overwrite failed: %+v", all["http://a:8080"])
	}
	if all["http://b:8080"].Token != "fhk_b1" {
		t.Fatalf("sibling clobbered: %+v", all["http://b:8080"])
	}
}

// The credentials file must be mode 0600 — it holds live secrets.
func TestStoreFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file mode semantics do not apply on windows")
	}
	path := withXDG(t)
	if err := Store("http://localhost:8080", Credential{Token: "fhk_secret"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Fatalf("credentials file mode = %o, want %o", perm, filePerm)
	}
}

func TestListEmptyWhenNoFile(t *testing.T) {
	withXDG(t)
	all, err := List()
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("want empty map, got %+v", all)
	}
}

// A corrupt store file surfaces an error rather than silently
// degrading to "no credentials".
func TestListCorruptFileErrors(t *testing.T) {
	path := withXDG(t)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), filePerm); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if _, err := List(); err == nil {
		t.Fatal("want error on corrupt store, got nil")
	}
	if _, err := Load("http://localhost:8080"); err == nil {
		t.Fatal("Load must propagate the parse error, got nil")
	}
}

func TestPathHonorsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(dir, dirName, fileName)
	if p != want {
		t.Fatalf("Path = %q, want %q", p, want)
	}
}

// --- refresh fields + derived skew (#2393 / ADR-076) ------------------------

// The refresh fields round-trip through the store, and a store written
// before they existed (an fhk_ device-flow record with none of them)
// still decodes unchanged.
func TestStoreLoadRoundTripRefreshFields(t *testing.T) {
	withXDG(t)
	issued := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	exp := issued.Add(15 * time.Minute)
	want := Credential{
		Token:         "fho_access",
		RefreshToken:  "fhr_refresh",
		ClientID:      "fishhawk-cli",
		TokenEndpoint: "http://localhost:8080/v0/oauth/token",
		IssuedAt:      &issued,
		ExpiresAt:     &exp,
	}
	if err := Store("http://localhost:8080", want); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load("http://localhost:8080")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != want.RefreshToken || got.ClientID != want.ClientID || got.TokenEndpoint != want.TokenEndpoint {
		t.Fatalf("refresh fields not preserved: %+v", got)
	}
	if got.IssuedAt == nil || !got.IssuedAt.Equal(issued) {
		t.Fatalf("issued_at not preserved: %v", got.IssuedAt)
	}
}

func TestLegacyStoreWithoutRefreshFieldsDecodes(t *testing.T) {
	path := withXDG(t)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatal(err)
	}
	legacy := `{"http://localhost:8080":{"token":"fhk_legacy","subject":"github:octocat"}}`
	if err := os.WriteFile(path, []byte(legacy), filePerm); err != nil {
		t.Fatal(err)
	}
	got, err := Load("http://localhost:8080")
	if err != nil {
		t.Fatalf("Load legacy store: %v", err)
	}
	if got.Token != "fhk_legacy" || got.Refreshable() || got.ExpiresAt != nil || got.IssuedAt != nil {
		t.Fatalf("legacy record decoded wrong: %+v", got)
	}
	if NeedsRefresh(got, time.Now()) {
		t.Fatal("a legacy non-expiring credential must never need refresh")
	}
}

func TestRefreshSkew_DerivedFromLifetime(t *testing.T) {
	cases := []struct {
		lifetime, want time.Duration
	}{
		{15 * time.Minute, 2 * time.Minute}, // the shipped default: lifetime*2/15
		{time.Hour, 8 * time.Minute},        // scales with the lifetime
		{0, 0},                              // no lifetime, no margin
		{-time.Minute, 0},
	}
	for _, c := range cases {
		if got := RefreshSkew(c.lifetime); got != c.want {
			t.Errorf("RefreshSkew(%v) = %v, want %v", c.lifetime, got, c.want)
		}
	}
}

// A very short lifetime is floored at 30s — lifetime*2/15 of 3 minutes
// is 24s, thinner than a refresh round-trip can be relied on to be.
func TestRefreshSkew_FloorAppliesAtVeryShortLifetime(t *testing.T) {
	if got := RefreshSkew(3 * time.Minute); got != 30*time.Second {
		t.Fatalf("RefreshSkew(3m) = %v, want the 30s floor", got)
	}
}

// The floor is itself capped at lifetime/2 so a 40s token gets a 20s
// margin, not a 30s one that exceeds its own remaining life.
func TestRefreshSkew_CapAppliesAtHalfLifetime(t *testing.T) {
	if got := RefreshSkew(40 * time.Second); got != 20*time.Second {
		t.Fatalf("RefreshSkew(40s) = %v, want the lifetime/2 cap of 20s", got)
	}
}

func refreshableAt(issued time.Time, lifetime time.Duration) Credential {
	exp := issued.Add(lifetime)
	return Credential{
		Token:         "fho_a",
		RefreshToken:  "fhr_a",
		ClientID:      "fishhawk-cli",
		TokenEndpoint: "http://localhost:8080/v0/oauth/token",
		IssuedAt:      &issued,
		ExpiresAt:     &exp,
	}
}

func TestNeedsRefresh_NilExpiresAtIsNonExpiring(t *testing.T) {
	c := Credential{Token: "fho_a", RefreshToken: "fhr_a", ClientID: "c", TokenEndpoint: "http://as/token"}
	if NeedsRefresh(c, time.Now()) {
		t.Fatal("nil ExpiresAt means non-expiring; must not need refresh")
	}
}

// An fhk_ device-flow credential with an expiry but no refresh token /
// client id / token endpoint has nothing to refresh WITH — deleting this
// guard would send it to an empty token endpoint.
func TestNeedsRefresh_NoRefreshTokenIsNotRefreshable(t *testing.T) {
	now := time.Now()
	exp := now.Add(30 * time.Second)
	c := Credential{Token: "fhk_a", ExpiresAt: &exp}
	if NeedsRefresh(c, now) {
		t.Fatal("a credential with no refresh token must not need refresh")
	}
	// Each of the three fields is individually load-bearing.
	full := refreshableAt(now.Add(-14*time.Minute), 15*time.Minute)
	for name, mut := range map[string]func(*Credential){
		"refresh_token":  func(c *Credential) { c.RefreshToken = "" },
		"client_id":      func(c *Credential) { c.ClientID = "" },
		"token_endpoint": func(c *Credential) { c.TokenEndpoint = "" },
	} {
		c := full
		mut(&c)
		if NeedsRefresh(c, now) {
			t.Errorf("missing %s must make the credential non-refreshable", name)
		}
	}
	if !NeedsRefresh(full, now) {
		t.Fatal("control: the full credential inside its skew must need refresh")
	}
}

func TestNeedsRefresh_InsideSkewIsTrue(t *testing.T) {
	now := time.Now()
	// 15m lifetime → 2m skew; 90s left is inside it.
	c := refreshableAt(now.Add(-13*time.Minute-30*time.Second), 15*time.Minute)
	if !NeedsRefresh(c, now) {
		t.Fatal("90s left on a 15m token is inside the 2m skew; must need refresh")
	}
	// Already expired and still refreshable → still true: refresh, don't fail.
	expired := refreshableAt(now.Add(-time.Hour), 15*time.Minute)
	if !NeedsRefresh(expired, now) {
		t.Fatal("an expired refreshable credential must need refresh")
	}
}

func TestNeedsRefresh_OutsideSkewIsFalse(t *testing.T) {
	now := time.Now()
	// 15m lifetime → 2m skew; 5m left is outside it.
	c := refreshableAt(now.Add(-10*time.Minute), 15*time.Minute)
	if NeedsRefresh(c, now) {
		t.Fatal("5m left on a 15m token is outside the 2m skew; must not need refresh")
	}
}

// With no IssuedAt the lifetime is unknown, so the skew for the shipped
// default lifetime applies rather than a zero margin.
func TestNeedsRefresh_NilIssuedAtUsesDefaultLifetimeSkew(t *testing.T) {
	now := time.Now()
	c := refreshableAt(now, 15*time.Minute)
	c.IssuedAt = nil
	if c.Lifetime() != DefaultLifetime {
		t.Fatalf("Lifetime() with nil IssuedAt = %v, want DefaultLifetime", c.Lifetime())
	}
	exp := now.Add(90 * time.Second) // inside RefreshSkew(DefaultLifetime)=2m
	c.ExpiresAt = &exp
	if !NeedsRefresh(c, now) {
		t.Fatal("90s left with unknown lifetime must fall inside the default 2m skew")
	}
	exp2 := now.Add(5 * time.Minute)
	c.ExpiresAt = &exp2
	if NeedsRefresh(c, now) {
		t.Fatal("5m left with unknown lifetime must be outside the default 2m skew")
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	future := now.Add(time.Second)
	if (Credential{}).Expired(now) {
		t.Error("nil ExpiresAt must never be expired")
	}
	if !(Credential{ExpiresAt: &past}).Expired(now) {
		t.Error("past expiry must be expired")
	}
	if (Credential{ExpiresAt: &future}).Expired(now) {
		t.Error("future expiry must not be expired")
	}
}
