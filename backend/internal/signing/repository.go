package signing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Repository owns the signing_keys table. Append-only at the API
// surface — Issue inserts, Get reads, Verify reads + checks. There
// is no Update or Delete; database triggers backstop the same.
type Repository interface {
	// Issue mints a fresh Ed25519 keypair, stores the public half
	// keyed by runID with the given TTL window, and returns both
	// halves plus the timestamps. Multi-call: every Issue inserts a
	// new row (per migration 0012), so each stage's GitHub Actions
	// runner can issue its own private key independently. Verify
	// accepts a signature from any unexpired key for the run.
	//
	// The caller is responsible for delivering IssuedKey.PrivateKey
	// to the runner (typically over the response body of the
	// issuance HTTP endpoint, with the GitHub OIDC token verified
	// at the handler layer — that wiring is E3.7 / E4 territory).
	Issue(ctx context.Context, runID uuid.UUID, ttl time.Duration) (*IssuedKey, error)

	// Get returns the public-side Key for the run, or
	// ErrNotFound. Used by Verify and by the audit log export
	// path to embed (run_id, public_key) for external verifiers.
	Get(ctx context.Context, runID uuid.UUID) (*Key, error)

	// Verify checks that signature is valid for the given message
	// against ANY unexpired signing key issued for runID (newest
	// key tried first), so a sibling stage's key rotation does not
	// invalidate an in-flight runner's still-open upload. Returns:
	//   - ErrNotFound        no key exists for the run
	//   - ErrExpired         keys exist but all are expired
	//   - ErrSignatureInvalid unexpired keys exist but none verify
	//   - nil                success
	Verify(ctx context.Context, runID uuid.UUID, message, signature []byte) error
}
