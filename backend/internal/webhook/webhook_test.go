package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// signed returns the GitHub-style header value for body+secret,
// shared by tests that exercise both the verifier and the
// HTTP-handler layer.
func signed(t *testing.T, secret, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_OK(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"hello":"world"}`)
	if err := VerifySignature(secret, body, signed(t, secret, body)); err != nil {
		t.Errorf("VerifySignature: %v", err)
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	body := []byte("payload")
	hdr := signed(t, []byte("right"), body)
	err := VerifySignature([]byte("wrong"), body, hdr)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestVerifySignature_MalformedHeader(t *testing.T) {
	cases := []string{"", "sha256", "sha1=abc", "sha256=zzz"}
	for _, h := range cases {
		t.Run(h, func(t *testing.T) {
			err := VerifySignature([]byte("s"), []byte("b"), h)
			if !errors.Is(err, ErrSignatureMissing) {
				t.Errorf("err = %v, want ErrSignatureMissing", err)
			}
		})
	}
}

func TestVerifySignature_NoSecret(t *testing.T) {
	err := VerifySignature(nil, []byte("b"), "sha256=00")
	if !errors.Is(err, ErrSecretNotConfigured) {
		t.Errorf("err = %v, want ErrSecretNotConfigured", err)
	}
}

func TestMemoryStore_FirstSeen(t *testing.T) {
	s := NewMemoryStore(0)
	if err := s.Mark("delivery-1"); err != nil {
		t.Errorf("first Mark: %v", err)
	}
}

func TestMemoryStore_DuplicateRejected(t *testing.T) {
	s := NewMemoryStore(0)
	_ = s.Mark("delivery-1")
	err := s.Mark("delivery-1")
	if !errors.Is(err, ErrDeliveryDuplicate) {
		t.Errorf("err = %v, want ErrDeliveryDuplicate", err)
	}
}

func TestMemoryStore_EmptyID(t *testing.T) {
	s := NewMemoryStore(0)
	if err := s.Mark(""); !errors.Is(err, ErrDeliveryMissing) {
		t.Errorf("err = %v, want ErrDeliveryMissing", err)
	}
}

func TestMemoryStore_TTLEvicts(t *testing.T) {
	s := NewMemoryStore(time.Hour)
	// Inject a fake clock so we can advance without sleeping.
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := t0
	s.now = func() time.Time { return cur }

	if err := s.Mark("d1"); err != nil {
		t.Fatal(err)
	}
	// Within TTL: still a duplicate.
	cur = t0.Add(30 * time.Minute)
	if err := s.Mark("d1"); !errors.Is(err, ErrDeliveryDuplicate) {
		t.Errorf("within TTL err = %v, want ErrDeliveryDuplicate", err)
	}
	// Past TTL: entry evicts on next Mark; second insert succeeds.
	cur = t0.Add(2 * time.Hour)
	if err := s.Mark("d1"); err != nil {
		t.Errorf("past TTL err = %v, want nil", err)
	}
}

func TestParseEvent_HappyPath(t *testing.T) {
	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "kuhlman-labs/fishhawk"},
		"sender": {"login": "kuhlman-labs"}
	}`)
	ev, err := ParseEvent("issues", "deliv-1", body)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.Type != "issues" {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.DeliveryID != "deliv-1" {
		t.Errorf("DeliveryID = %q", ev.DeliveryID)
	}
	if ev.Action != "labeled" {
		t.Errorf("Action = %q", ev.Action)
	}
	if ev.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("Repo = %q", ev.Repo)
	}
	if ev.Sender != "kuhlman-labs" {
		t.Errorf("Sender = %q", ev.Sender)
	}
	if string(ev.RawBody) != string(body) {
		t.Errorf("RawBody not preserved verbatim")
	}
}

func TestParseEvent_MissingHeaders(t *testing.T) {
	if _, err := ParseEvent("", "deliv-1", []byte(`{}`)); !errors.Is(err, ErrEventTypeMissing) {
		t.Errorf("missing event type: %v", err)
	}
	if _, err := ParseEvent("issues", "", []byte(`{}`)); !errors.Is(err, ErrDeliveryMissing) {
		t.Errorf("missing delivery: %v", err)
	}
}

func TestParseEvent_EmptyBody(t *testing.T) {
	// GitHub's "ping" event sometimes posts a small body, but the
	// header-only form should still parse cleanly.
	ev, err := ParseEvent("ping", "deliv-1", nil)
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if ev.Type != "ping" {
		t.Errorf("Type = %q", ev.Type)
	}
}

func TestParseEvent_MalformedBody(t *testing.T) {
	_, err := ParseEvent("issues", "deliv-1", []byte("{not json"))
	if err == nil {
		t.Error("expected parse error")
	}
}

func TestParseEvent_AbsentFieldsAreEmpty(t *testing.T) {
	// "push" events don't have a top-level action; the Event must
	// still parse and report Action="" rather than failing.
	ev, err := ParseEvent("push", "deliv-1", []byte(`{"repository":{"full_name":"x/y"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Action != "" {
		t.Errorf("Action = %q, want empty", ev.Action)
	}
	if ev.Repo != "x/y" {
		t.Errorf("Repo = %q", ev.Repo)
	}
	if ev.Sender != "" {
		t.Errorf("Sender = %q, want empty", ev.Sender)
	}
}

func TestErrors_Distinct(t *testing.T) {
	// Pin sentinel errors as non-equivalent so a future refactor
	// doesn't accidentally collapse them.
	pairs := [][2]error{
		{ErrSignatureInvalid, ErrSignatureMissing},
		{ErrSignatureMissing, ErrSecretNotConfigured},
		{ErrDeliveryDuplicate, ErrDeliveryMissing},
		{ErrEventTypeMissing, ErrDeliveryMissing},
	}
	for _, p := range pairs {
		if errors.Is(p[0], p[1]) {
			t.Errorf("errors.Is(%v, %v) = true, want false", p[0], p[1])
		}
	}
}

// TestParseEvent_LeavesForgeAndCredentialRefZero pins the GitHub-path
// regression guard (E45.6 / #1860): the GitHub parser never sets the
// two forge-neutral identity fields, so the GitHub webhook path is
// byte-for-byte unchanged by the GitLab addition.
func TestParseEvent_LeavesForgeAndCredentialRefZero(t *testing.T) {
	ev, err := ParseEvent("issues", "deliv-1", []byte(`{"action":"labeled","installation":{"id":42}}`))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.Forge != "" {
		t.Errorf("Forge = %q, want empty (GitHub legacy)", ev.Forge)
	}
	if ev.CredentialRef != "" {
		t.Errorf("CredentialRef = %q, want empty (GitHub uses InstallationID)", ev.CredentialRef)
	}
}

// TestDeliveryStore_GitHubAndGitLabUUIDsDoNotCollide pins the dedup
// namespacing: the same raw UUID arriving as a GitHub delivery and as a
// GitLab delivery are distinct keys, because ParseGitLabEvent prefixes
// "gitlab:" onto the delivery id. Without the prefix one forge's event
// would silently dedup the other's.
func TestDeliveryStore_GitHubAndGitLabUUIDsDoNotCollide(t *testing.T) {
	const uuid = "11111111-2222-3333-4444-555555555555"
	store := NewMemoryStore(0)

	gh, err := ParseEvent("issues", uuid, []byte(`{"action":"labeled"}`))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	gl, err := ParseGitLabEvent("Issue Hook", uuid, []byte(`{"object_kind":"issue"}`))
	if err != nil {
		t.Fatalf("ParseGitLabEvent: %v", err)
	}
	if err := store.Mark(gh.DeliveryID); err != nil {
		t.Fatalf("mark github delivery: %v", err)
	}
	// The GitLab delivery shares the raw UUID but is namespaced, so it
	// must be recorded as a fresh (non-duplicate) key.
	if err := store.Mark(gl.DeliveryID); err != nil {
		t.Fatalf("gitlab delivery collided with github: %v", err)
	}
}
