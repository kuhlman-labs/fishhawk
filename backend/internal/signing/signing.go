// Package signing manages per-run Ed25519 signing keys (E2.3 / #24).
//
// Per ADR-008 (#72) the backend mints a fresh keypair when a run
// starts, stores the public half in the signing_keys table, and
// returns the private half to the caller (typically the runner via
// a transport not built here). The runner signs sha256(raw_bundle_
// bytes) and ships (bundle, signature); the backend verifies against
// the stored public key before accepting the trace.
//
// Rows in signing_keys are immutable once written, so the
// (run_id, public_key) pair is a stable, externally-verifiable
// record. The external verification tool (E2.6 / #27) reads that
// pair from an audit-log export and does Ed25519 verification with
// no backend trust required — using ComputeMessage and VerifyWith
// from this package, or its own equivalent.
package signing

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
)

// DefaultTTL is the lifetime of an issued key per ADR-008.
const DefaultTTL = 30 * time.Minute

// IssuedKey is what Issue returns. The PrivateKey is delivered to
// the caller but never persisted server-side; PublicKey is stored in
// signing_keys.
type IssuedKey struct {
	RunID      uuid.UUID
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// Key is the persisted record (public part only).
type Key struct {
	RunID     uuid.UUID
	PublicKey ed25519.PublicKey
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Errors returned by the signing repository. Tests and HTTP
// adapters can errors.Is against these.
var (
	// ErrNotFound means no signing key exists for the given run.
	ErrNotFound = errors.New("signing key not found")

	// ErrAlreadyIssued means a key was already issued for this run.
	// Re-issuance is not supported in v0; a run gets exactly one key.
	ErrAlreadyIssued = errors.New("signing key already issued for this run")

	// ErrExpired means the stored key's expires_at is in the past.
	// External verifiers may still accept (the public key is
	// stable); the backend rejects post-TTL signatures because the
	// runner shouldn't have been signing that late.
	ErrExpired = errors.New("signing key expired")

	// ErrSignatureInvalid means Ed25519 verification failed for the
	// provided message + signature against the stored public key.
	ErrSignatureInvalid = errors.New("signature invalid")
)

// ComputeMessage returns the canonical message that the runner
// signs and the backend verifies: sha256(raw_bundle_bytes). Per
// ADR-008, Ed25519 hashes internally; pre-hashing here adds a layer
// of canonicalization-free commitment to the bundle bytes.
//
// Exposed so the runner, the backend's verify path, and the
// external verifier all derive the message via the exact same
// function — no risk of one side normalizing differently.
func ComputeMessage(rawBundle []byte) []byte {
	sum := sha256.Sum256(rawBundle)
	return sum[:]
}

// Sign returns the Ed25519 signature of message using the given
// private key. The runner uses this to avoid importing crypto/
// ed25519 directly; the cipher choice stays encapsulated in this
// package.
func Sign(key ed25519.PrivateKey, message []byte) []byte {
	return ed25519.Sign(key, message)
}

// VerifyWith checks signature against message using the given
// public key. Returns ErrSignatureInvalid on mismatch, nil on
// success. Used by the external verifier (E2.6) where it has the
// public key directly from an audit-log export.
func VerifyWith(publicKey ed25519.PublicKey, message, signature []byte) error {
	if !ed25519.Verify(publicKey, message, signature) {
		return ErrSignatureInvalid
	}
	return nil
}
