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
