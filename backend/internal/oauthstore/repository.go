// Package oauthstore persists the OAuth 2.1 authorization server's
// credentials: CIMD-derived client registrations, authorization codes, access
// tokens and refresh tokens (E66.18 / #2433, ADR-076; migration 0063).
//
// # Why persistence lives here and not in internal/oauthas
//
// oauthas is the pure domain core — PKCE, redirect matching, typed RFC 6749
// errors, CIMD fetch. Its TestPackage_ImportsNeitherServerNorDatabase
// machine-enforces that it imports no internal/server, no internal/postgres, no
// internal/db and no jackc/pgx, in both Imports and TestImports. A storage file
// added there would fail that test immediately. So this is its sibling
// persistence package. The dependency direction is ONE-WAY: oauthstore imports
// oauthas for value types, never the reverse — which keeps that boundary test
// passing unchanged.
//
// # Credentials are hashed at rest
//
// Every credential follows the api_tokens pattern: a prefixed random plaintext
// over 32 crypto/rand bytes, of which only the hex sha256 is stored. The
// PlainText field on Code / AccessToken / RefreshToken is populated EXACTLY
// ONCE, by the mint path that generated it, and is never re-derivable — every
// later read returns it empty. TestCredentials_PlaintextReturnedOnlyAtMint
// pins that across every read path this interface exposes.
//
// # What this package is NOT
//
// No HTTP handler, no /.well-known metadata document, no bearer middleware,
// no CIMD fetching. #2391 owns the endpoints and composes this store with
// oauthas.
package oauthstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
)

// Credential prefixes. Distinct per credential class so a leaked string is
// greppable AND self-identifying: presenting an access token where a refresh
// token is expected fails at HashPlaintext without a database round-trip.
const (
	// CodePrefix marks an authorization code.
	CodePrefix = "fhc_"
	// AccessTokenPrefix marks an access token.
	AccessTokenPrefix = "fho_"
	// RefreshTokenPrefix marks a refresh token.
	RefreshTokenPrefix = "fhr_"
)

// credentialSecretBytes is the entropy every credential carries: 256 bits,
// which is also the unguessability the 0063 header's isolation argument rests
// on (every credential read is keyed on the hash of one of these).
const credentialSecretBytes = 32

// Errors callers switch on.
var (
	// ErrNotFound means NO row carries that credential hash — never issued, or
	// deleted. It is deliberately DISTINCT from ErrRevoked at this API even
	// though #2391 will collapse both into a single opaque HTTP response: the
	// audit trail and the lineage-revocation tests read the distinction, and a
	// revoked credential is an observable STATE rather than an absence.
	ErrNotFound = errors.New("oauthstore: not found")

	// ErrCodeConsumed means the authorization code has already been redeemed.
	// Its second presentation is the RFC 6749 §4.1.2 replay signal.
	ErrCodeConsumed = errors.New("oauthstore: authorization code already consumed")

	// ErrExpired means the row exists but its expires_at has lapsed.
	ErrExpired = errors.New("oauthstore: credential expired")

	// ErrRevoked means the row exists and carries a revoked_at. Distinct from
	// ErrNotFound — see ErrNotFound.
	ErrRevoked = errors.New("oauthstore: credential revoked")

	// ErrMalformedCredential means the presented string did not carry the
	// expected prefix or was implausibly short. Returned WITHOUT any database
	// round-trip.
	ErrMalformedCredential = errors.New("oauthstore: malformed credential")

	// ErrRefreshReused means an already-rotated refresh token was presented
	// again. When RotateRefreshToken returns this, the whole-lineage revocation
	// is already COMMITTED — see RotateRefreshToken.
	ErrRefreshReused = errors.New("oauthstore: refresh token reused")
)

// GeneratePlaintext produces a fresh credential string with the given prefix
// over credentialSecretBytes of crypto/rand entropy.
func GeneratePlaintext(prefix string) (string, error) {
	buf := make([]byte, credentialSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauthstore: random bytes: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashPlaintext returns the canonical hex sha256 of a credential string, after
// checking it carries prefix. ErrMalformedCredential on a wrong prefix or an
// implausibly short string — so a junk bearer never reaches the database and
// never pollutes the hash space.
func HashPlaintext(prefix, plaintext string) (string, error) {
	if !strings.HasPrefix(plaintext, prefix) {
		return "", ErrMalformedCredential
	}
	if len(plaintext) <= len(prefix)+8 {
		return "", ErrMalformedCredential
	}
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:]), nil
}

// Client is a persisted CIMD-derived client registration.
//
// NOT FORGE-SCOPED, and deliberately so (0064 / #2437): a registration names
// WHICH SOFTWARE is asking — ClientID is a CIMD document URL — not who
// authenticated, so it carries no identity provider and one URL is one
// registration globally. The three credential types below (AuthorizationCode,
// AccessToken, RefreshToken) DO record a subject and keep their Provider field.
//
// REFRESH-ON-FETCH: UpsertClient overwrites the metadata columns when the same
// ClientID is seen again, because the CIMD document is authoritative and
// client-controlled. FirstSeenAt is preserved across refreshes; UpdatedAt moves.
type Client struct {
	ID                      uuid.UUID
	ClientID                string
	RedirectURIs            []string
	GrantTypes              []string
	ResponseTypes           []string
	TokenEndpointAuthMethod string
	ClientName              string
	ClientURI               string
	LogoURI                 string
	Scope                   string
	AccountID               string
	FirstSeenAt             time.Time
	UpdatedAt               time.Time
}

// AuthorizationCode is a persisted authorization code.
//
// PlainText is set exactly once, by CreateAuthorizationCode. Every other path
// that returns an AuthorizationCode leaves it empty.
type AuthorizationCode struct {
	ID                  uuid.UUID
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod oauthas.CodeChallengeMethod
	Scopes              []string
	Resource            string
	Subject             string
	Provider            string
	AccountID           string
	IssuedAt            time.Time
	ExpiresAt           time.Time
	ConsumedAt          *time.Time

	// PlainText is the code the client receives. Empty on every read.
	PlainText string
}

// IsConsumed reports whether the code has already been redeemed.
func (c AuthorizationCode) IsConsumed() bool { return c.ConsumedAt != nil }

// IsExpired reports whether the code has lapsed at now. The boundary is
// INCLUSIVE: a code is expired at exactly ExpiresAt, matching the repository's
// `!now.Before(expires)` classification.
func (c AuthorizationCode) IsExpired(now time.Time) bool { return !now.Before(c.ExpiresAt) }

// AccessToken is a persisted bearer token.
//
// PlainText is set exactly once, by the mint path. Every read leaves it empty.
type AccessToken struct {
	ID                  uuid.UUID
	Subject             string
	ClientID            string
	Audience            string
	Scopes              []string
	Provider            string
	AccountID           string
	AuthorizationCodeID uuid.UUID
	IssuedAt            time.Time
	ExpiresAt           time.Time
	LastUsedAt          *time.Time
	RevokedAt           *time.Time

	PlainText string
}

// IsRevoked reports whether the token carries a revoked_at.
func (t AccessToken) IsRevoked() bool { return t.RevokedAt != nil }

// IsExpired reports whether the token has lapsed at now (inclusive boundary).
func (t AccessToken) IsExpired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// RefreshToken is a persisted rotating refresh token.
//
// PlainText is set exactly once, by the mint path. Every read leaves it empty.
type RefreshToken struct {
	ID                  uuid.UUID
	Subject             string
	ClientID            string
	Audience            string
	Scopes              []string
	Provider            string
	AccountID           string
	AuthorizationCodeID uuid.UUID
	AccessTokenID       uuid.UUID
	ReplacedByID        *uuid.UUID
	IssuedAt            time.Time
	ExpiresAt           time.Time
	LastUsedAt          *time.Time
	ConsumedAt          *time.Time
	RevokedAt           *time.Time

	PlainText string
}

// IsConsumed reports whether the token has already been rotated.
func (t RefreshToken) IsConsumed() bool { return t.ConsumedAt != nil }

// IsRevoked reports whether the token carries a revoked_at.
func (t RefreshToken) IsRevoked() bool { return t.RevokedAt != nil }

// IsExpired reports whether the token has lapsed at now (inclusive boundary).
func (t RefreshToken) IsExpired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// NewClient is the input to UpsertClient. It is the persisted projection of an
// oauthas.ClientMetadata plus the tenant discriminator — and NO forge one: see
// Client for why a registration has no identity provider.
type NewClient struct {
	Metadata  oauthas.ClientMetadata
	AccountID string
}

// NewAuthorizationCode is the input to CreateAuthorizationCode.
type NewAuthorizationCode struct {
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod oauthas.CodeChallengeMethod
	Scopes              []string
	Resource            string
	Subject             string
	Provider            string
	AccountID           string
	ExpiresAt           time.Time
}

// RedemptionRequest is the WHOLE of what a redemption caller supplies: the
// minted pair's expiries.
//
// Every AUTHORITY-bearing field — subject, client_id, audience, scopes,
// provider, account_id — and the authorization_code_id lineage are DERIVED by
// RedeemAuthorizationCode from the authorization-code row it consumed under the
// lock. This type cannot express any of them, so holding an authorization code
// buys exactly one thing: a token pair carrying the authority that code was
// ISSUED with.
//
// This is the redemption half of the same principle RotationRequest carries. A
// caller-supplied authority here was the wider hole: the code row already
// records subject, scopes, client_id, resource, provider and account_id — a
// prior authority record exactly analogous to a presented refresh token's — and
// accepting those values from the caller instead meant possession of ONE
// authorization code could mint credentials carrying authority the code never
// granted (a different subject or tenant, a widened scope set, another forge).
// No chaining test can see that, because chaining tests pass the expected
// values; so the wrong value is made INEXPRESSIBLE rather than validated, and
// #2391 does not have to perfectly reconstruct every field for the boundary to
// hold.
//
// The audience mapping is stated because it is the one non-identity projection:
// the pair's `audience` is the code's RFC 8707 `resource` indicator, recorded at
// /authorize. A code carrying no resource mints an empty audience rather than
// one the token caller chose.
//
// RFC 6749 §3.3 scope NARROWING at the token endpoint is deliberately not
// expressible today: the pair inherits the code's scope set verbatim. Adding a
// narrowing field later is additive and MUST be validated as a strict subset of
// the code's scopes — widening must stay inexpressible.
type RedemptionRequest struct {
	AccessTokenExpiry  time.Time
	RefreshTokenExpiry time.Time
}

// RotationRequest is the WHOLE of what a rotation caller supplies: the successor
// pair's expiries.
//
// Every AUTHORITY-bearing field — subject, client_id, audience, scopes,
// provider, account_id — and the authorization_code_id lineage are DERIVED by
// RotateRefreshToken from the presented refresh token's own row. This type
// cannot express any of them, so holding a refresh token buys exactly one thing:
// a successor carrying the SAME authority. Cross-client, cross-subject,
// cross-audience, cross-scope, cross-provider and cross-tenant issuance are
// inexpressible rather than merely discouraged (#2433 implement-review
// follow-up to approval condition 1, which closed the lineage half of the same
// hole).
//
// RFC 6749 §6 scope NARROWING is deliberately not expressible today: the
// successor inherits the presented token's scope set verbatim. Adding a
// narrowing field later is additive and MUST be validated as a strict subset of
// the presented token's scopes — widening must stay inexpressible.
type RotationRequest struct {
	AccessTokenExpiry  time.Time
	RefreshTokenExpiry time.Time
}

// IssuedGrant is the result of a redemption or a rotation: a freshly minted
// token pair, each carrying its PlainText exactly once.
//
// Code is the authorization code the grant descends from, WITHOUT its plaintext
// (RedeemAuthorizationCode returns the row it consumed; RotateRefreshToken
// leaves it nil — rotation does not re-read the originating code).
type IssuedGrant struct {
	Code    *AuthorizationCode
	Access  *AccessToken
	Refresh *RefreshToken
}

// Repository persists the authorization server's credentials.
//
// The shape is chosen so VERIFY-BEFORE-CONSUME is orderable by the caller: a
// consume-then-return signature (which would burn a legitimate client's code on
// a failed check) is explicitly foreclosed.
type Repository interface {
	// UpsertClient records a CIMD-derived registration, OVERWRITING the
	// metadata of the existing row for that client_id (the conflict target is
	// client_id ALONE — 0064). See Client: the document is authoritative, so a
	// client that rotates its redirect URIs is reflected on the next fetch
	// rather than pinned to its first snapshot.
	UpsertClient(ctx context.Context, in NewClient) (*Client, error)

	// GetClientByID returns the registration for clientID, or ErrNotFound.
	// client_id is globally unique, so this resolves to AT MOST ONE row —
	// there is no forge scope to disambiguate and no ambiguous pair a caller
	// could have to arbitrate.
	GetClientByID(ctx context.Context, clientID string) (*Client, error)

	// CreateAuthorizationCode mints a code and returns the row with PlainText
	// set — the only time that value exists outside the client.
	CreateAuthorizationCode(ctx context.Context, in NewAuthorizationCode) (*AuthorizationCode, error)

	// LookupAuthorizationCode is a READ-ONLY load. It never touches
	// consumed_at, so #2391's token endpoint can validate client_id,
	// redirect_uri, PKCE and resource and render a typed oauthas error WITHOUT
	// burning the grant. Returns the row whatever its consumed/expired state;
	// the caller inspects IsConsumed/IsExpired. ErrNotFound if no row carries
	// the hash, ErrMalformedCredential on a bad string.
	LookupAuthorizationCode(ctx context.Context, plaintext string) (*AuthorizationCode, error)

	// RedeemAuthorizationCode is the transactional single-use seam. Ordering is
	// a CONTRACT, not an implementation detail:
	//
	//  1. open ONE transaction;
	//  2. SELECT the code FOR UPDATE (serializing concurrent redemptions);
	//  3. refuse a consumed code with ErrCodeConsumed — verify is NOT called;
	//  4. refuse a lapsed code with ErrExpired — verify is NOT called;
	//  5. call verify with the loaded row;
	//  6. ONLY when verify returns nil, run the conditional consume and mint
	//     the token pair.
	//
	// A verify error is returned UNCHANGED and rolls the transaction back, so
	// an intercepted code presented with a wrong verifier does not burn the
	// legitimate client's code — a later redemption with a passing verify still
	// succeeds. verify may be nil, meaning "no additional checks".
	//
	// AUTHORITY AND LINEAGE ARE BOTH DERIVED, never supplied. The minted pair
	// inherits subject, client_id, scopes, provider and account_id from the code
	// row consumed in step 6, its audience from that row's RFC 8707 resource
	// indicator, and the code's own id as its lineage — so the pair carries the
	// authority the code was ISSUED with and RevokeGrantsForCode stays reachable.
	// RedemptionRequest carries only the pair's expiries; see its doc for why the
	// wrong value is made inexpressible rather than validated.
	RedeemAuthorizationCode(ctx context.Context, plaintext string, verify func(*AuthorizationCode) error, redeem RedemptionRequest) (*IssuedGrant, error)

	// RevokeGrantsForCode revokes every still-active access and refresh token
	// descended from codeID and returns the total number of rows revoked. This
	// is the RFC 6749 §4.1.2 replay response, implementable because the lineage
	// columns are real and NOT NULL. It takes the same authorization-code row
	// lock RotateRefreshToken takes, so a concurrent rotation cannot slip a
	// successor past the sweep. An unknown codeID revokes zero rows without
	// erroring.
	RevokeGrantsForCode(ctx context.Context, codeID uuid.UUID) (int64, error)

	// AuthenticateAccessToken resolves a bearer plaintext to its token row.
	//
	// The three failure modes stay DISTINCT: ErrRevoked when the row exists and
	// carries a revoked_at, ErrExpired when it has lapsed, and ErrNotFound ONLY
	// when no row carries that hash at all. ErrMalformedCredential on a bad
	// string, with no database round-trip. On success last_used_at is stamped
	// best-effort.
	AuthenticateAccessToken(ctx context.Context, plaintext string) (*AccessToken, error)

	// RotateRefreshToken performs the OAuth 2.1 rotation: consume the presented
	// token, mint a successor pair, and chain them via replaced_by_id.
	//
	// AUTHORITY AND LINEAGE ARE BOTH DERIVED, never supplied. The successor
	// inherits subject, client_id, audience, scopes, provider, account_id AND
	// authorization_code_id from the presented token's row; RotationRequest
	// carries only the successor expiries. So a caller can neither mint a
	// successor outside RevokeGrantsForCode's reach (#2433 approval condition 1)
	// nor mint one under a different client, subject, audience, scope set,
	// provider or tenant (its implement-review follow-up).
	//
	// verify is the READ/VERIFY SEAM this otherwise-opaque plaintext API would
	// lack. It receives the presented token row — PlainText empty, like every
	// other read — AFTER classification and BEFORE anything is minted or
	// consumed, so #2391 can enforce RFC 6749 §6's "was this refresh token
	// issued to the authenticated client?" binding. Derivation alone does not
	// cover that: it stops a thief minting under someone else's authority, but a
	// thief presenting a stolen token would still receive a valid successor
	// under the VICTIM's authority. That binding is the caller's to enforce, as
	// with RedeemAuthorizationCode's verify. A verify error is returned
	// UNCHANGED and rolls the transaction back, so a rejected rotation does not
	// consume the legitimate client's token — its later, passing rotation still
	// succeeds. verify may be nil, meaning "no additional checks".
	//
	// verify is NOT called for a revoked, reused or expired token: those are
	// classified first, and the reuse sweep in particular is a security response
	// that must not be gated on the caller's callback.
	//
	// REUSE: presenting an already-consumed token revokes the WHOLE lineage
	// (every access and refresh token sharing its authorization_code_id) and
	// returns ErrRefreshReused. That revocation is GUARANTEED COMMITTED before
	// this method returns — a caller that reads the store afterwards observes a
	// revoked lineage, never a rolled-back one.
	//
	// The guarantee holds UNDER CONCURRENCY, not merely in isolation: this
	// method and RevokeGrantsForCode both serialize on the authorization-code
	// row (FOR UPDATE) before touching any descendant, so a rotation racing a
	// sweep of the same lineage cannot leave a live successor behind the reuse
	// verdict. Either the rotation commits first and the sweep sees its
	// successor, or the sweep commits first and the rotation observes ErrRevoked
	// and mints nothing.
	//
	// CLASSIFICATION ORDER is part of the contract: revoked → REUSED → expired.
	// Reuse is classified BEFORE expiry deliberately — a replayed rotated token
	// is the compromise signal the sweep exists for, and the presented token's
	// own expiry says nothing about its successors, which carry later expiries.
	// So an already-rotated token that has ALSO lapsed still sweeps the lineage
	// and returns ErrRefreshReused, never ErrExpired. ErrExpired is reserved for
	// a token that lapsed WITHOUT having been rotated; a token with no matching
	// row yields ErrNotFound.
	RotateRefreshToken(ctx context.Context, plaintext string, verify func(*RefreshToken) error, rotate RotationRequest) (*IssuedGrant, error)

	// RevokeAccessToken marks an access token revoked. Idempotent: a second
	// call returns the existing row with its original revoked_at. Carries NO
	// database-level tenant check — the authorization decision belongs to the
	// already-authenticated caller (0063 header, part (c)).
	RevokeAccessToken(ctx context.Context, id uuid.UUID) (*AccessToken, error)

	// RevokeRefreshToken marks a refresh token revoked. Idempotent, and with
	// the same no-database-tenant-check caveat as RevokeAccessToken.
	RevokeRefreshToken(ctx context.Context, id uuid.UUID) (*RefreshToken, error)
}
