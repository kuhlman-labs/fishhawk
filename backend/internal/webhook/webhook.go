// Package webhook implements GitHub App webhook receipt: signature
// verification, delivery-ID dedup, and event parsing. The HTTP
// handler that wires these primitives lives in
// backend/internal/server/webhook.go.
//
// Per docs/api/v0.openapi.yaml, the receiver is at
// `POST /webhooks/github`. Auth is the GitHub HMAC over the body
// (X-Hub-Signature-256); no Bearer or cookie required.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Errors callers may want to switch on.
var (
	ErrSignatureInvalid    = errors.New("webhook: signature invalid")
	ErrSignatureMissing    = errors.New("webhook: signature header missing or malformed")
	ErrDeliveryDuplicate   = errors.New("webhook: delivery already processed")
	ErrDeliveryMissing     = errors.New("webhook: X-GitHub-Delivery missing")
	ErrEventTypeMissing    = errors.New("webhook: X-GitHub-Event missing")
	ErrSecretNotConfigured = errors.New("webhook: secret not configured")
)

// signaturePrefix is the algorithm tag GitHub uses on the
// X-Hub-Signature-256 header. Hard-coded because GitHub doesn't
// support algorithm negotiation; if they ship a v2 we'll add a
// branch.
const signaturePrefix = "sha256="

// VerifySignature checks that signatureHeader (expected:
// "sha256=<hex>") is a valid HMAC-SHA256 of body using secret. It
// uses crypto/hmac.Equal for constant-time comparison so an
// attacker cannot leak the signature byte-by-byte via timing.
//
// Returns ErrSignatureMissing for empty / malformed headers and
// ErrSignatureInvalid for valid-shape-but-wrong values; callers
// usually translate both to 401.
func VerifySignature(secret []byte, body []byte, signatureHeader string) error {
	if len(secret) == 0 {
		return ErrSecretNotConfigured
	}
	if !strings.HasPrefix(signatureHeader, signaturePrefix) {
		return ErrSignatureMissing
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signatureHeader, signaturePrefix))
	if err != nil {
		return ErrSignatureMissing
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		return ErrSignatureInvalid
	}
	return nil
}

// DeliveryStore deduplicates webhook deliveries by X-GitHub-Delivery
// (a UUID GitHub assigns per delivery, retained across the retry
// window). Implementations decide on persistence and TTL.
//
// Production should back this with Postgres so dedup survives
// restarts and works across multiple instances. v0 self-execution
// runs single-instance; in-memory with a 24h TTL covers the retry
// window for that deployment.
type DeliveryStore interface {
	// Mark records id as processed. Returns ErrDeliveryDuplicate if
	// id was already recorded; nil on first write.
	Mark(id string) error
}

// MemoryStore is a process-local DeliveryStore with TTL eviction.
// Suitable for single-instance v0 deployments. NOT suitable for
// multi-instance because nodes don't share state — two instances
// could both process the same delivery.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
	now     func() time.Time
}

// NewMemoryStore returns a MemoryStore with the given TTL. ttl=0
// means "no eviction"; tests use that to keep behavior deterministic.
// Production should use 24h to comfortably exceed GitHub's webhook
// retry window (~3h) without growing unboundedly.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	return &MemoryStore{
		entries: make(map[string]time.Time),
		ttl:     ttl,
		now:     func() time.Time { return time.Now() },
	}
}

// Mark records id as seen. Repeated calls with the same id (within
// the TTL window) return ErrDeliveryDuplicate.
func (s *MemoryStore) Mark(id string) error {
	if id == "" {
		return ErrDeliveryMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.ttl > 0 {
		s.evictExpired(now)
	}
	if _, ok := s.entries[id]; ok {
		return ErrDeliveryDuplicate
	}
	s.entries[id] = now
	return nil
}

// evictExpired drops entries older than the TTL. Caller holds mu.
func (s *MemoryStore) evictExpired(now time.Time) {
	for k, t := range s.entries {
		if now.Sub(t) > s.ttl {
			delete(s.entries, k)
		}
	}
}

// Event is the parsed envelope of a webhook payload. Fields are
// what we care about across event types; type-specific logic
// re-decodes from RawBody.
type Event struct {
	// Type is the X-GitHub-Event header value (e.g. "issues",
	// "pull_request", "push", "ping").
	Type string

	// DeliveryID is the X-GitHub-Delivery header value.
	DeliveryID string

	// Action is the top-level "action" field common to most webhook
	// payloads (e.g. "labeled", "opened", "closed"). Empty for
	// events that don't have actions (e.g. "push", "ping").
	Action string

	// Repo is the repository's full name ("owner/repo") when the
	// payload includes one. Empty for events without a repo
	// (e.g. installation-level events).
	Repo string

	// Sender is the GitHub login of the user / app that triggered
	// the event. Empty when the payload doesn't include one.
	Sender string

	// RawBody is the original payload, kept for downstream handlers
	// that need event-specific fields not surfaced here.
	RawBody []byte
}

// minimal is the subset of fields we extract from every webhook
// payload. JSON unmarshal is permissive about absent fields, so the
// same struct works for issues, pull_request, push, ping, etc.
type minimal struct {
	Action     string `json:"action,omitempty"`
	Repository struct {
		FullName string `json:"full_name,omitempty"`
	} `json:"repository,omitempty"`
	Sender struct {
		Login string `json:"login,omitempty"`
	} `json:"sender,omitempty"`
}

// ParseEvent extracts the headers and decodes the common fields
// from body. Returns errors for missing required headers; never
// returns an error for missing JSON fields (those are simply
// reported as zero values on the Event).
func ParseEvent(eventType, deliveryID string, body []byte) (Event, error) {
	if eventType == "" {
		return Event{}, ErrEventTypeMissing
	}
	if deliveryID == "" {
		return Event{}, ErrDeliveryMissing
	}
	ev := Event{
		Type:       eventType,
		DeliveryID: deliveryID,
		RawBody:    body,
	}
	// Body may be empty for some test deliveries; attempt to parse
	// only when present.
	if len(body) > 0 {
		var m minimal
		if err := json.Unmarshal(body, &m); err != nil {
			return ev, fmt.Errorf("webhook: parse body: %w", err)
		}
		ev.Action = m.Action
		ev.Repo = m.Repository.FullName
		ev.Sender = m.Sender.Login
	}
	return ev, nil
}
