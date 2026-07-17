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

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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

	// Unmark removes id's processed-record so a later Mark(id) is a
	// first write again. Receivers call it to undo the pre-dispatch
	// record when a delivery's processing fails and they return a 5xx:
	// without it the forge's retry hits the still-recorded delivery,
	// is deduped to a 2xx, and permanently drops an event whose
	// processing actually failed. Idempotent — unmarking an id that
	// was never recorded is a nil no-op.
	Unmark(id string) error
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

// Unmark removes id so a later Mark(id) is treated as a first write
// again. The receiver calls it to undo a delivery record when the
// delivery's processing failed and it returned a 5xx — otherwise the
// retry would be deduped to a 2xx, permanently dropping the event. A
// no-op returning nil when id was never recorded (or is empty).
func (s *MemoryStore) Unmark(id string) error {
	if id == "" {
		return ErrDeliveryMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, id)
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

	// SenderType is the kind of actor — typically "User" or "Bot".
	// Used to gate dispatch on bot-authored events without parsing
	// login suffixes.
	SenderType string

	// InstallationID identifies the GitHub App installation the
	// event belongs to. Required for any backend → GitHub action
	// (fetching the workflow spec, firing workflow_dispatch).
	// Zero when the event isn't installation-scoped.
	InstallationID int64

	// Forge names the source forge for a parsed event. Empty is the
	// GitHub legacy default (ParseEvent never sets it, so the GitHub
	// path is byte-for-byte unchanged); ParseGitLabEvent sets
	// "gitlab" (ForgeGitLab). The dispatcher routes on this field so a
	// GitLab-sourced event never runs through the GitHub matcher.
	Forge string

	// CredentialRef is the forge-neutral installation_ref for the
	// event (ADR-058, #2009). Empty for GitHub, where InstallationID
	// carries the identity; for GitLab it is the "gitlab:<project_id>"
	// scope ref forge/gitlab.projectIDFromScope parses back. When set,
	// credentialScope() prefers forge.FromRef(CredentialRef) over the
	// GitHub installation-id wrapper.
	CredentialRef string

	// RawBody is the original payload, kept for downstream handlers
	// that need event-specific fields not surfaced here.
	RawBody []byte
}

// ForgeGitLab is the Event.Forge value ParseGitLabEvent stamps. The
// empty string is the GitHub legacy default (ParseEvent leaves it
// zero), so callers switching on the source forge compare against
// this constant rather than a bare literal.
const ForgeGitLab = "gitlab"

// credentialScope resolves the event's forge-neutral CredentialScope.
// A non-empty CredentialRef (the GitLab path) wraps verbatim via
// forge.FromRef; otherwise the GitHub installation-id convenience
// wrapper is used (zero InstallationID → the zero scope). This keeps
// the GitHub path byte-for-byte unchanged while letting a GitLab
// "gitlab:<project_id>" ref flow through the shared dispatcher.
func (ev Event) credentialScope() forge.CredentialScope {
	if ev.CredentialRef != "" {
		return forge.FromRef(ev.CredentialRef)
	}
	return forge.FromGitHubInstallationID(ev.InstallationID)
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
		Type  string `json:"type,omitempty"`
	} `json:"sender,omitempty"`
	Installation struct {
		ID int64 `json:"id,omitempty"`
	} `json:"installation,omitempty"`
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
		ev.SenderType = m.Sender.Type
		ev.InstallationID = m.Installation.ID
	}
	return ev, nil
}
