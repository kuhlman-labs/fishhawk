package oauthstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
	oauthstoredb "github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

const (
	testClientID    = "https://client.example.com/oauth/client"
	testAltClientID = "https://other.example.com/oauth/client"
	testSubject     = "github:octocat"
	testAudience    = "https://app.fishhawk.dev/mcp"
)

// newRepo returns the Repository AND the raw pool. Several assertions must read
// COMMITTED state back over raw SQL on a connection OUTSIDE the repository's
// own transaction — that is the whole point of the reuse-revocation proof.
func newRepo(t *testing.T) (oauthstore.Repository, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.NewPool(t)
	return oauthstore.NewPostgresRepository(pool), pool
}

func newCode(t *testing.T, repo oauthstore.Repository, expiresAt time.Time) *oauthstore.AuthorizationCode {
	t.Helper()
	return newCodeFor(t, repo, testClientID, expiresAt)
}

func newCodeFor(t *testing.T, repo oauthstore.Repository, clientID string, expiresAt time.Time) *oauthstore.AuthorizationCode {
	t.Helper()
	code, err := repo.CreateAuthorizationCode(context.Background(), oauthstore.NewAuthorizationCode{
		ClientID:            clientID,
		RedirectURI:         "http://127.0.0.1:8765/callback",
		CodeChallenge:       oauthas.DeriveS256Challenge("verifier-verifier-verifier-verifier"),
		CodeChallengeMethod: oauthas.CodeChallengeMethodS256,
		Scopes:              []string{"runs:read"},
		Resource:            testAudience,
		Subject:             testSubject,
		Provider:            "github",
		ExpiresAt:           expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateAuthorizationCode: %v", err)
	}
	return code
}

func mintReq() oauthstore.MintRequest {
	now := time.Now().UTC()
	return oauthstore.MintRequest{
		Subject:            testSubject,
		ClientID:           testClientID,
		Audience:           testAudience,
		Scopes:             []string{"runs:read"},
		Provider:           "github",
		AccessTokenExpiry:  now.Add(time.Hour),
		RefreshTokenExpiry: now.Add(24 * time.Hour),
	}
}

// rotateReq is the WHOLE of a rotation caller's input. It carries no authority
// field because RotationRequest cannot express one — see
// TestRotateRefreshToken_AuthorityIsDerivedNotCallerSupplied.
func rotateReq() oauthstore.RotationRequest {
	now := time.Now().UTC()
	return oauthstore.RotationRequest{
		AccessTokenExpiry:  now.Add(time.Hour),
		RefreshTokenExpiry: now.Add(24 * time.Hour),
	}
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func scanString(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return out
}

func scanTimestamp(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) pgtype.Timestamptz {
	t.Helper()
	var out pgtype.Timestamptz
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return out
}

func scanInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var out int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Done-means: credentials hashed at rest, plaintext only at mint
// ---------------------------------------------------------------------------

// TestCredentials_HashedAtRest reads the stored digests back over RAW SQL
// against the migrated schema: the persisted value must neither equal nor
// CONTAIN the plaintext, and must equal a hex sha256 computed independently in
// this test from crypto/sha256 rather than restated from the implementation.
func TestCredentials_HashedAtRest(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	storedCode := scanString(t, pool, `SELECT code_hash FROM oauth_authorization_codes WHERE id = $1`, code.ID)
	if storedCode == code.PlainText || strings.Contains(storedCode, code.PlainText) {
		t.Fatalf("code_hash %q leaks the plaintext", storedCode)
	}
	if want := hexSHA256(code.PlainText); storedCode != want {
		t.Errorf("code_hash = %q, want %q", storedCode, want)
	}

	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}

	storedAccess := scanString(t, pool, `SELECT token_hash FROM oauth_access_tokens WHERE id = $1`, grant.Access.ID)
	if storedAccess == grant.Access.PlainText || strings.Contains(storedAccess, grant.Access.PlainText) {
		t.Fatalf("access token_hash %q leaks the plaintext", storedAccess)
	}
	if want := hexSHA256(grant.Access.PlainText); storedAccess != want {
		t.Errorf("access token_hash = %q, want %q", storedAccess, want)
	}

	storedRefresh := scanString(t, pool, `SELECT token_hash FROM oauth_refresh_tokens WHERE id = $1`, grant.Refresh.ID)
	if storedRefresh == grant.Refresh.PlainText || strings.Contains(storedRefresh, grant.Refresh.PlainText) {
		t.Fatalf("refresh token_hash %q leaks the plaintext", storedRefresh)
	}
	if want := hexSHA256(grant.Refresh.PlainText); storedRefresh != want {
		t.Errorf("refresh token_hash = %q, want %q", storedRefresh, want)
	}
}

// TestCredentials_PlaintextReturnedOnlyAtMint establishes the acceptance claim
// the digest assertions alone do NOT: the mint paths return a non-empty
// PlainText, and EVERY read path this Repository exposes returns it EMPTY. An
// implementation that memoised a plaintext or re-emitted it on a read fails.
//
// Coverage is EXHAUSTIVE over the interface, not illustrative: every exported
// method that can return a credential struct is asserted here. The mint paths
// (CreateAuthorizationCode, RedeemAuthorizationCode, RotateRefreshToken) return
// a non-empty PlainText; the five non-mint paths that return one —
// LookupAuthorizationCode, the code carried on RedeemAuthorizationCode's grant,
// AuthenticateAccessToken, RevokeAccessToken and RevokeRefreshToken — return it
// EMPTY. RevokeRefreshToken is the only public path that hands back a PERSISTED
// refresh-token row, so omitting it would leave the refresh class asserted on
// mint output alone. (GetClient/UpsertClient return a *Client, which carries no
// PlainText field at all; GetRefreshTokenByHash lives only in the generated
// layer, as an implementation detail of RotateRefreshToken.)
func TestCredentials_PlaintextReturnedOnlyAtMint(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	// Mint path 1: CreateAuthorizationCode.
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	if code.PlainText == "" {
		t.Fatal("CreateAuthorizationCode returned an empty PlainText, want the minted code")
	}

	// Read path 1: LookupAuthorizationCode.
	looked, err := repo.LookupAuthorizationCode(ctx, code.PlainText)
	if err != nil {
		t.Fatalf("LookupAuthorizationCode: %v", err)
	}
	if looked.PlainText != "" {
		t.Errorf("LookupAuthorizationCode PlainText = %q, want empty", looked.PlainText)
	}

	// Mint path 2: RedeemAuthorizationCode mints the pair...
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	if grant.Access.PlainText == "" || grant.Refresh.PlainText == "" {
		t.Fatal("RedeemAuthorizationCode returned an empty token PlainText, want both minted")
	}
	// ...but the code it CONSUMED is a read, not a mint.
	if grant.Code == nil || grant.Code.PlainText != "" {
		t.Errorf("redeemed grant.Code PlainText = %q, want empty", grant.Code.PlainText)
	}

	// Read path 2: AuthenticateAccessToken.
	authed, err := repo.AuthenticateAccessToken(ctx, grant.Access.PlainText)
	if err != nil {
		t.Fatalf("AuthenticateAccessToken: %v", err)
	}
	if authed.PlainText != "" {
		t.Errorf("AuthenticateAccessToken PlainText = %q, want empty", authed.PlainText)
	}

	// Mint path 3: rotation mints a fresh pair and carries no code plaintext.
	rotated, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	if rotated.Access.PlainText == "" || rotated.Refresh.PlainText == "" {
		t.Fatal("RotateRefreshToken returned an empty token PlainText, want both minted")
	}
	if rotated.Refresh.PlainText == grant.Refresh.PlainText {
		t.Error("rotation re-emitted the PRESENTED refresh token's plaintext, want a fresh successor")
	}
	if rotated.Code != nil {
		t.Errorf("rotation grant.Code = %+v, want nil (rotation does not re-read the code)", rotated.Code)
	}

	// Read path 3: the successor access token authenticates with no plaintext.
	authed2, err := repo.AuthenticateAccessToken(ctx, rotated.Access.PlainText)
	if err != nil {
		t.Fatalf("AuthenticateAccessToken (successor): %v", err)
	}
	if authed2.PlainText != "" {
		t.Errorf("successor AuthenticateAccessToken PlainText = %q, want empty", authed2.PlainText)
	}

	// Read path 4: RevokeRefreshToken — the ONLY public path that returns a
	// PERSISTED refresh-token row. Without this the refresh class would be
	// asserted on mint output alone, which is exactly what the invariant is
	// about: the row that came BACK OUT of the database must not carry a
	// plaintext.
	revokedRefresh, err := repo.RevokeRefreshToken(ctx, rotated.Refresh.ID)
	if err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	if revokedRefresh.PlainText != "" {
		t.Errorf("RevokeRefreshToken PlainText = %q, want empty", revokedRefresh.PlainText)
	}
	// And it really is the row we minted, not some empty struct that would make
	// the assertion above pass vacuously.
	if revokedRefresh.ID != rotated.Refresh.ID || revokedRefresh.RevokedAt == nil {
		t.Errorf("RevokeRefreshToken returned %+v, want the persisted successor row with revoked_at set", revokedRefresh)
	}

	// Read path 5: RevokeAccessToken, the same for the access class.
	revokedAccess, err := repo.RevokeAccessToken(ctx, rotated.Access.ID)
	if err != nil {
		t.Fatalf("RevokeAccessToken: %v", err)
	}
	if revokedAccess.PlainText != "" {
		t.Errorf("RevokeAccessToken PlainText = %q, want empty", revokedAccess.PlainText)
	}
	if revokedAccess.ID != rotated.Access.ID || revokedAccess.RevokedAt == nil {
		t.Errorf("RevokeAccessToken returned %+v, want the persisted successor row with revoked_at set", revokedAccess)
	}
}

// ---------------------------------------------------------------------------
// Verify-before-consume + single use
// ---------------------------------------------------------------------------

// TestRedeemAuthorizationCode_VerifyRejectionRollsBack is the CROSS-BOUNDARY
// seam proof: it spans the migration DDL, the sqlc-typed layer and the
// repository. A failing verify must return that error UNCHANGED, leave
// consumed_at NULL and mint zero tokens — and the legitimate client's later
// redemption must still succeed. A per-layer unit on either side would pass
// while the transaction boundary leaked.
func TestRedeemAuthorizationCode_VerifyRejectionRollsBack(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))

	sentinel := errors.New("pkce mismatch")
	_, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, func(*oauthstore.AuthorizationCode) error {
		return sentinel
	}, mintReq())
	if !errors.Is(err, sentinel) {
		t.Fatalf("RedeemAuthorizationCode error = %v, want the verify error unchanged", err)
	}

	consumed := scanTimestamp(t, pool, `SELECT consumed_at FROM oauth_authorization_codes WHERE id = $1`, code.ID)
	if consumed.Valid {
		t.Errorf("consumed_at = %v after a rejected verify, want NULL (the grant must not be burned)", consumed.Time)
	}
	if n := scanInt(t, pool, `SELECT count(*) FROM oauth_access_tokens WHERE authorization_code_id = $1`, code.ID); n != 0 {
		t.Errorf("minted %d access tokens on a rejected verify, want 0", n)
	}

	// The concrete proof: an intercepted code plus a wrong verifier does not
	// burn the legitimate client's code.
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, func(*oauthstore.AuthorizationCode) error {
		return nil
	}, mintReq())
	if err != nil {
		t.Fatalf("second RedeemAuthorizationCode after a rejected verify: %v", err)
	}
	if grant.Access == nil || grant.Refresh == nil {
		t.Fatal("second redemption returned an incomplete grant")
	}
}

// TestRedeemAuthorizationCode_VerifySeesTheLoadedRow proves verify is handed
// the persisted row (not a stub), which is what makes the caller-side
// client-binding / PKCE / redirect checks in #2391 possible at all.
//
// #2433 approval condition 6: this exercises the CALLER-SUPPLIED verify seam,
// NOT a repository-level control. RedeemAuthorizationCode takes no client_id
// and enforces no client binding itself — the binding below is the test's own
// verify callback standing in for #2391's token endpoint. Do not read this as a
// database-enforced binding.
func TestRedeemAuthorizationCode_VerifySeesTheLoadedRow(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))

	var seen *oauthstore.AuthorizationCode
	bindTo := func(clientID string) func(*oauthstore.AuthorizationCode) error {
		return func(c *oauthstore.AuthorizationCode) error {
			seen = c
			if c.ClientID != clientID {
				return &oauthas.Error{Code: oauthas.ErrCodeInvalidGrant, Description: "client_id mismatch"}
			}
			return nil
		}
	}

	// A code issued to testClientID, presented under a DIFFERENT client, is
	// refused — by the caller's verifier.
	if _, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, bindTo(testAltClientID), mintReq()); !errors.Is(err, &oauthas.Error{Code: oauthas.ErrCodeInvalidGrant}) {
		t.Fatalf("redemption under a mismatched client_id = %v, want an invalid_grant *oauthas.Error", err)
	}
	if seen == nil {
		t.Fatal("verify was never invoked on a live code")
	}
	if seen.ClientID != testClientID {
		t.Errorf("verify saw client_id %q, want the persisted %q", seen.ClientID, testClientID)
	}
	if seen.CodeChallengeMethod != oauthas.CodeChallengeMethodS256 {
		t.Errorf("verify saw code_challenge_method %q, want S256", seen.CodeChallengeMethod)
	}
	if seen.Subject != testSubject || seen.Resource != testAudience {
		t.Errorf("verify saw subject/resource %q/%q, want %q/%q", seen.Subject, seen.Resource, testSubject, testAudience)
	}
	if seen.PlainText != "" {
		t.Errorf("verify saw PlainText %q, want empty", seen.PlainText)
	}

	// The matching client still redeems.
	if _, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, bindTo(testClientID), mintReq()); err != nil {
		t.Fatalf("redemption under the matching client_id: %v", err)
	}
}

// TestRedeemAuthorizationCode_ExpiredSkipsVerify pins the TTL branch AND its
// ordering: an expired code is refused with ErrExpired and the verify callback
// is NEVER invoked (an implementation that verified before checking expiry
// fails the counter assertion).
func TestRedeemAuthorizationCode_ExpiredSkipsVerify(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(-time.Minute))

	calls := 0
	_, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, func(*oauthstore.AuthorizationCode) error {
		calls++
		return nil
	}, mintReq())
	if !errors.Is(err, oauthstore.ErrExpired) {
		t.Fatalf("RedeemAuthorizationCode on an expired code = %v, want ErrExpired", err)
	}
	if calls != 0 {
		t.Errorf("verify invoked %d times on an expired code, want 0", calls)
	}
	consumed := scanTimestamp(t, pool, `SELECT consumed_at FROM oauth_authorization_codes WHERE id = $1`, code.ID)
	if consumed.Valid {
		t.Error("an expired redemption stamped consumed_at, want NULL")
	}
}

// TestRedeemAuthorizationCode_ConsumedCodeSkipsVerify pins the POST-LOCK
// consumed check where IT is individually observable — via the verify-call
// counter. MANDATED COUNTERFACTUAL (b): deleting that check in
// RedeemAuthorizationCode makes verify run against a dead code and this
// assertion fails. The redundant conditional UPDATE predicate would still
// refuse the redemption, so error identity alone would NOT catch it; the
// counter is the seam that can.
func TestRedeemAuthorizationCode_ConsumedCodeSkipsVerify(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))

	if _, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq()); err != nil {
		t.Fatalf("first RedeemAuthorizationCode: %v", err)
	}

	calls := 0
	_, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, func(*oauthstore.AuthorizationCode) error {
		calls++
		return nil
	}, mintReq())
	if !errors.Is(err, oauthstore.ErrCodeConsumed) {
		t.Fatalf("replayed RedeemAuthorizationCode = %v, want ErrCodeConsumed", err)
	}
	if calls != 0 {
		t.Errorf("verify invoked %d times on an already-consumed code, want 0", calls)
	}
}

// TestConsumeAuthorizationCode_PredicateRejectsConsumedRow pins the
// LOAD-BEARING single-use control DIRECTLY at the query level. It issues the
// ConsumeAuthorizationCode UPDATE against an already-consumed row on a PLAIN
// pool connection — no row lock, no repository-level check in front of it — so
// the `AND consumed_at IS NULL` predicate is the only thing that can refuse it.
//
// MANDATED COUNTERFACTUAL (a): delete that predicate from queries.sql and this
// test fails, because the UPDATE then succeeds and returns a row.
func TestConsumeAuthorizationCode_PredicateRejectsConsumedRow(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))

	q := oauthstoredb.New(pool)
	first, err := q.ConsumeAuthorizationCode(ctx, oauthstoredb.ConsumeAuthorizationCodeParams{
		ID:         code.ID,
		ConsumedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		t.Fatalf("first ConsumeAuthorizationCode: %v", err)
	}
	if !first.ConsumedAt.Valid {
		t.Fatal("first consume did not stamp consumed_at")
	}

	_, err = q.ConsumeAuthorizationCode(ctx, oauthstoredb.ConsumeAuthorizationCodeParams{
		ID:         code.ID,
		ConsumedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second ConsumeAuthorizationCode = %v, want pgx.ErrNoRows (zero rows affected)", err)
	}

	// And the original stamp is untouched — the rejected UPDATE wrote nothing.
	after := scanTimestamp(t, pool, `SELECT consumed_at FROM oauth_authorization_codes WHERE id = $1`, code.ID)
	if !after.Valid || !after.Time.Equal(first.ConsumedAt.Time) {
		t.Errorf("consumed_at = %v after the rejected UPDATE, want the original %v", after, first.ConsumedAt.Time)
	}
}

// TestRedeemAuthorizationCode_ConcurrentSingleUse proves the CONJUNCTION of the
// two single-use controls under real contention: N goroutines redeem the SAME
// code, exactly one wins, every loser gets ErrCodeConsumed specifically, and
// exactly one access-token row exists for that code.
//
// THIS TEST IS NOT A COUNTERFACTUAL VEHICLE for either control, and it cannot
// be made into one. The post-lock consumed check and the conditional UPDATE
// predicate are DELIBERATELY REDUNDANT: deleting either alone leaves the other
// rejecting every loser, so this test stays green. Mandating a red here would
// mandate an unattainable result. The individual controls are pinned where each
// IS individually observable — the predicate by
// TestConsumeAuthorizationCode_PredicateRejectsConsumedRow (counterfactual a),
// the post-lock check by TestRedeemAuthorizationCode_ConsumedCodeSkipsVerify
// (counterfactual b).
func TestRedeemAuthorizationCode_ConcurrentSingleUse(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))

	const racers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	grants := make([]*oauthstore.IssuedGrant, racers)
	errs := make([]error, racers)

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			grants[i], errs[i] = repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
		}()
	}
	close(start)
	wg.Wait()

	winners, losers := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			if grants[i] == nil || grants[i].Access == nil {
				t.Errorf("racer %d won with an incomplete grant", i)
			}
		case errors.Is(err, oauthstore.ErrCodeConsumed):
			losers++
		default:
			t.Errorf("racer %d error = %v, want nil or ErrCodeConsumed (no other class)", i, err)
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if losers != racers-1 {
		t.Errorf("ErrCodeConsumed losers = %d, want %d", losers, racers-1)
	}
	if n := scanInt(t, pool, `SELECT count(*) FROM oauth_access_tokens WHERE authorization_code_id = $1`, code.ID); n != 1 {
		t.Errorf("access-token rows for the code = %d, want exactly 1", n)
	}
	if n := scanInt(t, pool, `SELECT count(*) FROM oauth_refresh_tokens WHERE authorization_code_id = $1`, code.ID); n != 1 {
		t.Errorf("refresh-token rows for the code = %d, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// Access-token authentication: revoked / expired / unknown stay DISTINCT
// ---------------------------------------------------------------------------

// TestAuthenticateAccessToken_RevokedExpiredUnknownAreDistinct asserts all
// three outcomes in ONE test with errors.Is on each sentinel, so an
// implementation that collapses any pair fails. This is reachable only because
// GetAccessTokenByHash is UNFILTERED on revoked_at and the repository
// classifies: re-adding `AND revoked_at IS NULL` would turn the revoked case
// into ErrNotFound and redden this test.
func TestAuthenticateAccessToken_RevokedExpiredUnknownAreDistinct(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	// Revoked.
	revokedCode := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	revokedGrant, err := repo.RedeemAuthorizationCode(ctx, revokedCode.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("redeem (revoked case): %v", err)
	}
	if _, err := repo.RevokeAccessToken(ctx, revokedGrant.Access.ID); err != nil {
		t.Fatalf("RevokeAccessToken: %v", err)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, revokedGrant.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("revoked token = %v, want ErrRevoked (NOT ErrNotFound — revocation is an observable state)", err)
	}

	// Expired.
	expiredCode := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	expiredMint := mintReq()
	expiredMint.AccessTokenExpiry = time.Now().UTC().Add(-time.Minute)
	expiredGrant, err := repo.RedeemAuthorizationCode(ctx, expiredCode.PlainText, nil, expiredMint)
	if err != nil {
		t.Fatalf("redeem (expired case): %v", err)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, expiredGrant.Access.PlainText); !errors.Is(err, oauthstore.ErrExpired) {
		t.Errorf("expired token = %v, want ErrExpired", err)
	}

	// Never issued.
	never, err := oauthstore.GeneratePlaintext(oauthstore.AccessTokenPrefix)
	if err != nil {
		t.Fatalf("GeneratePlaintext: %v", err)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, never); !errors.Is(err, oauthstore.ErrNotFound) {
		t.Errorf("never-issued token = %v, want ErrNotFound", err)
	}
}

// TestAuthenticateAccessToken_MalformedNoRoundTrip pins the malformed branch:
// a plaintext without the access-token prefix is refused with
// ErrMalformedCredential. The pool is CLOSED first, so any database round-trip
// would surface as a connection error instead — which is the observable proof
// that the guard short-circuits before touching the database.
func TestAuthenticateAccessToken_MalformedNoRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	pool.Close()

	for _, plaintext := range []string{
		"not-a-fishhawk-token",
		oauthstore.RefreshTokenPrefix + "wrong-class-but-well-formed",
		oauthstore.AccessTokenPrefix,
	} {
		if _, err := repo.AuthenticateAccessToken(ctx, plaintext); !errors.Is(err, oauthstore.ErrMalformedCredential) {
			t.Errorf("AuthenticateAccessToken(%q) = %v, want ErrMalformedCredential with no round-trip", plaintext, err)
		}
	}
	// Same guard on the other credential-consuming entry points.
	if _, err := repo.LookupAuthorizationCode(ctx, "nope"); !errors.Is(err, oauthstore.ErrMalformedCredential) {
		t.Errorf("LookupAuthorizationCode = %v, want ErrMalformedCredential", err)
	}
	if _, err := repo.RedeemAuthorizationCode(ctx, "nope", nil, mintReq()); !errors.Is(err, oauthstore.ErrMalformedCredential) {
		t.Errorf("RedeemAuthorizationCode = %v, want ErrMalformedCredential", err)
	}
	if _, err := repo.RotateRefreshToken(ctx, "nope", nil, rotateReq()); !errors.Is(err, oauthstore.ErrMalformedCredential) {
		t.Errorf("RotateRefreshToken = %v, want ErrMalformedCredential", err)
	}
}

// TestLookupAuthorizationCode_UnknownIsNotFound covers the read-only load's
// miss branch, and that the load never mutates the row it read.
func TestLookupAuthorizationCode_UnknownIsNotFound(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	never, err := oauthstore.GeneratePlaintext(oauthstore.CodePrefix)
	if err != nil {
		t.Fatalf("GeneratePlaintext: %v", err)
	}
	if _, err := repo.LookupAuthorizationCode(ctx, never); !errors.Is(err, oauthstore.ErrNotFound) {
		t.Errorf("LookupAuthorizationCode(unknown) = %v, want ErrNotFound", err)
	}

	// A live code loads WITHOUT being burned — the whole point of the split.
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	if _, err := repo.LookupAuthorizationCode(ctx, code.PlainText); err != nil {
		t.Fatalf("LookupAuthorizationCode: %v", err)
	}
	if consumed := scanTimestamp(t, pool, `SELECT consumed_at FROM oauth_authorization_codes WHERE id = $1`, code.ID); consumed.Valid {
		t.Error("LookupAuthorizationCode stamped consumed_at, want a read-only load")
	}
	if _, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq()); err != nil {
		t.Fatalf("redemption after a lookup: %v", err)
	}
}

// TestRedeemAuthorizationCode_UnknownIsNotFound covers the transactional
// path's miss branch, which is distinct from LookupAuthorizationCode's.
func TestRedeemAuthorizationCode_UnknownIsNotFound(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	never, err := oauthstore.GeneratePlaintext(oauthstore.CodePrefix)
	if err != nil {
		t.Fatalf("GeneratePlaintext: %v", err)
	}
	if _, err := repo.RedeemAuthorizationCode(ctx, never, nil, mintReq()); !errors.Is(err, oauthstore.ErrNotFound) {
		t.Errorf("RedeemAuthorizationCode(unknown) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Rotation, lineage and reuse
// ---------------------------------------------------------------------------

// TestRotateRefreshToken_RotatesAndChains pins the happy rotation: a successor
// pair is minted, the presented token has consumed_at AND replaced_by_id
// stamped, the successor access token authenticates, and the successor's
// authorization_code_id (read by raw SQL) equals the ORIGINATING code's id.
// That last assertion plus the NOT NULL column is what makes the lineage sweep
// in the reuse test reachable.
func TestRotateRefreshToken_RotatesAndChains(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}

	rotated, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	if rotated.Access == nil || rotated.Refresh == nil {
		t.Fatal("rotation returned an incomplete grant")
	}
	if rotated.Refresh.ID == grant.Refresh.ID {
		t.Fatal("rotation returned the presented token, want a fresh successor")
	}

	consumed := scanTimestamp(t, pool, `SELECT consumed_at FROM oauth_refresh_tokens WHERE id = $1`, grant.Refresh.ID)
	if !consumed.Valid {
		t.Error("presented refresh token consumed_at is NULL after rotation, want stamped")
	}
	replacedBy := scanString(t, pool, `SELECT replaced_by_id::text FROM oauth_refresh_tokens WHERE id = $1`, grant.Refresh.ID)
	if replacedBy != rotated.Refresh.ID.String() {
		t.Errorf("replaced_by_id = %s, want the successor %s", replacedBy, rotated.Refresh.ID)
	}

	// Lineage, read from the DATABASE rather than the returned struct.
	accessLineage := scanString(t, pool, `SELECT authorization_code_id::text FROM oauth_access_tokens WHERE id = $1`, rotated.Access.ID)
	if accessLineage != code.ID.String() {
		t.Errorf("successor access authorization_code_id = %s, want the originating code %s", accessLineage, code.ID)
	}
	refreshLineage := scanString(t, pool, `SELECT authorization_code_id::text FROM oauth_refresh_tokens WHERE id = $1`, rotated.Refresh.ID)
	if refreshLineage != code.ID.String() {
		t.Errorf("successor refresh authorization_code_id = %s, want the originating code %s", refreshLineage, code.ID)
	}

	if _, err := repo.AuthenticateAccessToken(ctx, rotated.Access.PlainText); err != nil {
		t.Errorf("successor access token does not authenticate: %v", err)
	}
}

// TestRotateRefreshToken_LineageIsDerivedNotCallerSupplied is the falsifying
// test for #2433 approval condition 1: the lineage guarantee must be
// STRUCTURAL, not conventional.
//
// The condition's stated remedy was "derive it and ignore any caller-supplied
// value"; this implementation goes one step further and makes the wrong value
// INEXPRESSIBLE — MintRequest carries no authorization-code id at all, so there
// is nothing for a caller to get wrong and no mismatch error to define. The
// reflection guard below is what keeps that structural: a future refactor that
// re-adds the field (the exact hole the condition names) reddens this test.
//
// The behavioural half then proves derivation end to end: rotate a token
// belonging to code A while an UNRELATED grant B (different client_id, audience
// and scopes) exists alongside it, and assert the successor is swept by
// RevokeGrantsForCode(A) and untouched by RevokeGrantsForCode(B).
//
// The AUTHORITY fields the successor inherits are the sibling guarantee, pinned
// separately by TestRotateRefreshToken_AuthorityIsDerivedNotCallerSupplied.
func TestRotateRefreshToken_LineageIsDerivedNotCallerSupplied(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	if f, ok := reflect.TypeOf(oauthstore.MintRequest{}).FieldByName("AuthorizationCodeID"); ok {
		t.Fatalf("MintRequest carries a caller-supplied lineage field %s — condition 1 forbids it; "+
			"RotateRefreshToken must DERIVE authorization_code_id from the presented token's row", f.Name)
	}

	codeA := newCodeFor(t, repo, testClientID, time.Now().UTC().Add(time.Minute))
	grantA, err := repo.RedeemAuthorizationCode(ctx, codeA.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("redeem A: %v", err)
	}
	codeB := newCodeFor(t, repo, testAltClientID, time.Now().UTC().Add(time.Minute))
	mintB := mintReq()
	mintB.ClientID = testAltClientID
	mintB.Audience = "https://elsewhere.example.com"
	mintB.Scopes = []string{"runs:write"}
	grantB, err := repo.RedeemAuthorizationCode(ctx, codeB.PlainText, nil, mintB)
	if err != nil {
		t.Fatalf("redeem B: %v", err)
	}

	// Rotate A's refresh token. B's grant is live alongside it, so a successor
	// stamped with anything other than A's code would be a real escape rather
	// than an unobservable one.
	rotated, err := repo.RotateRefreshToken(ctx, grantA.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	lineage := scanString(t, pool, `SELECT authorization_code_id::text FROM oauth_access_tokens WHERE id = $1`, rotated.Access.ID)
	if lineage != codeA.ID.String() {
		t.Fatalf("successor lineage = %s, want the PRESENTED token's code %s (never the unrelated grant B's %s)",
			lineage, codeA.ID, codeB.ID)
	}

	// Sweeping B must NOT reach the successor...
	if _, err := repo.RevokeGrantsForCode(ctx, codeB.ID); err != nil {
		t.Fatalf("RevokeGrantsForCode(B): %v", err)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, rotated.Access.PlainText); err != nil {
		t.Fatalf("successor was swept by an UNRELATED code's revocation: %v", err)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, grantB.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("B's own access token after RevokeGrantsForCode(B) = %v, want ErrRevoked", err)
	}

	// ...and sweeping A MUST.
	n, err := repo.RevokeGrantsForCode(ctx, codeA.ID)
	if err != nil {
		t.Fatalf("RevokeGrantsForCode(A): %v", err)
	}
	if n == 0 {
		t.Error("RevokeGrantsForCode(A) revoked 0 rows, want the successor lineage")
	}
	if _, err := repo.AuthenticateAccessToken(ctx, rotated.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("successor after RevokeGrantsForCode(A) = %v, want ErrRevoked — it escaped the sweep", err)
	}
}

// TestRotateRefreshToken_AuthorityIsDerivedNotCallerSupplied is the falsifying
// test for the implement-review follow-up to #2433 approval condition 1.
//
// Condition 1 made the LINEAGE store-owned, which stops a successor escaping the
// revocation sweep. It does not close the other half: while a rotation caller
// could still supply subject / client_id / audience / scopes / provider /
// account_id, anyone holding ONE refresh token could mint a successor under a
// DIFFERENT client, subject, audience, scope set, forge or tenant — an
// authorization-escalation surface no chaining test can see, because those tests
// pass the expected values.
//
// The remedy is the same shape as condition 1's: make the wrong value
// INEXPRESSIBLE rather than validated. RotationRequest carries the successor
// expiries and nothing else.
//
// Structural half: a reflection guard over RotationRequest's fields, so a future
// refactor re-adding an authority field reddens this test rather than silently
// reopening the hole.
//
// Behavioural half: mint a grant whose authority is distinctive on EVERY axis
// (tenanted account, non-default provider, two scopes, the alternate client and
// a distinct audience), rotate it, and read all six columns of BOTH successor
// rows back over RAW SQL. An implementation that took authority from the
// caller's request would write the request's zero values — and 'provider' in
// particular would come back as the defaulted 'github' rather than the presented
// 'gitlab', so the assertion cannot be satisfied vacuously.
func TestRotateRefreshToken_AuthorityIsDerivedNotCallerSupplied(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	for _, forbidden := range []string{
		"Subject", "ClientID", "Audience", "Scopes", "Provider", "AccountID", "AuthorizationCodeID",
	} {
		if f, ok := reflect.TypeOf(oauthstore.RotationRequest{}).FieldByName(forbidden); ok {
			t.Fatalf("RotationRequest carries the caller-supplied authority field %s — rotation must DERIVE "+
				"every authority field from the presented token's row so cross-client, cross-scope and "+
				"cross-tenant issuance stay inexpressible", f.Name)
		}
	}

	accountID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, provider, account_key, granularity) VALUES ($1, 'github', $2, 'enterprise')`,
		accountID, "acct-"+accountID.String()); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	code := newCodeFor(t, repo, testAltClientID, time.Now().UTC().Add(time.Minute))
	mint := mintReq()
	mint.Subject = "gitlab:victim"
	mint.ClientID = testAltClientID
	mint.Audience = "https://tenant.example.com/mcp"
	mint.Scopes = []string{"runs:read", "runs:write"}
	mint.Provider = "gitlab"
	mint.AccountID = accountID.String()
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mint)
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}

	rotated, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}

	cases := []struct {
		name  string
		query string
		id    uuid.UUID
	}{
		{
			"access",
			`SELECT subject, client_id, audience, scopes, provider, account_id::text FROM oauth_access_tokens WHERE id = $1`,
			rotated.Access.ID,
		},
		{
			"refresh",
			`SELECT subject, client_id, audience, scopes, provider, account_id::text FROM oauth_refresh_tokens WHERE id = $1`,
			rotated.Refresh.ID,
		},
	}
	for _, tc := range cases {
		var subject, clientID, audience, provider string
		var scopes []string
		// account_id is nullable, and a NULL is exactly what dropping the
		// derivation would write — so it is scanned as a pointer and reported as
		// a failed assertion rather than a scan error.
		var acctPtr *string
		if err := pool.QueryRow(ctx, tc.query, tc.id).Scan(
			&subject, &clientID, &audience, &scopes, &provider, &acctPtr); err != nil {
			t.Fatalf("read %s successor: %v", tc.name, err)
		}
		acct := "NULL"
		if acctPtr != nil {
			acct = *acctPtr
		}
		if subject != mint.Subject {
			t.Errorf("%s successor subject = %q, want the presented token's %q", tc.name, subject, mint.Subject)
		}
		if clientID != mint.ClientID {
			t.Errorf("%s successor client_id = %q, want the presented token's %q", tc.name, clientID, mint.ClientID)
		}
		if audience != mint.Audience {
			t.Errorf("%s successor audience = %q, want the presented token's %q", tc.name, audience, mint.Audience)
		}
		if !reflect.DeepEqual(scopes, mint.Scopes) {
			t.Errorf("%s successor scopes = %v, want the presented token's %v — a rotation must never widen or "+
				"replace the granted scope set", tc.name, scopes, mint.Scopes)
		}
		if provider != mint.Provider {
			t.Errorf("%s successor provider = %q, want the presented token's %q (never the column default)",
				tc.name, provider, mint.Provider)
		}
		if acct != accountID.String() {
			t.Errorf("%s successor account_id = %q, want the presented token's tenant %s", tc.name, acct, accountID)
		}
	}
}

// TestRotateRefreshToken_VerifyRejectionRollsBack pins the read/verify seam the
// same implement-review concern named. Deriving authority stops a token holder
// minting under SOMEONE ELSE's authority; it does not stop a THIEF presenting a
// stolen token and receiving a valid successor under the victim's authority.
// Only an RFC 6749 §6 "was this token issued to the authenticated client?" check
// does that, and the opaque plaintext API exposed no seam through which #2391
// could make it. Rotation now takes one, mirroring RedeemAuthorizationCode's.
//
// Three claims, each load-bearing:
//   - verify SEES the presented row (client_id, subject, lineage) and sees it
//     with an EMPTY PlainText, so the seam cannot become a plaintext leak;
//   - a verify error is returned UNCHANGED and rolls the transaction back —
//     consumed_at and replaced_by_id stay NULL and NO successor rows are minted,
//     so a wrong presenter cannot burn the legitimate client's refresh token;
//   - the legitimate rotation afterwards still succeeds.
//
// Counterfactual, stated at the strength it actually holds: dropping the verify
// call — or ignoring its error — reddens this test. The ROLLBACK half is NOT
// independently observable, and is deliberately not claimed to be: pgx.BeginFunc
// rolls back on ANY non-nil callback return, so moving the verify call BELOW
// mintPair/ConsumeRefreshToken leaves this test green. That was checked
// empirically rather than argued. The state assertions are therefore an
// INVARIANT check that the seam sits inside that single transaction, not a
// control with a counterfactual of its own; the ordering claim that IS
// individually observable is pinned next door by
// TestRotateRefreshToken_ClassificationSkipsVerify's call counter.
func TestRotateRefreshToken_VerifyRejectionRollsBack(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}

	countTokens := func() int {
		t.Helper()
		return scanInt(t, pool,
			`SELECT count(*) FROM oauth_access_tokens WHERE authorization_code_id = $1`, code.ID)
	}
	before := countTokens()

	wrongClient := errors.New("presenting client is not the token's client")
	var seen *oauthstore.RefreshToken
	if _, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, func(rt *oauthstore.RefreshToken) error {
		seen = rt
		return wrongClient
	}, rotateReq()); !errors.Is(err, wrongClient) {
		t.Fatalf("RotateRefreshToken with a rejecting verify = %v, want the caller's error unchanged", err)
	}

	if seen == nil {
		t.Fatal("verify was never called on a live refresh token")
	}
	if seen.ID != grant.Refresh.ID || seen.ClientID != testClientID || seen.Subject != testSubject {
		t.Errorf("verify saw %+v, want the PRESENTED row (id %s, client %q, subject %q)",
			seen, grant.Refresh.ID, testClientID, testSubject)
	}
	if seen.AuthorizationCodeID != code.ID {
		t.Errorf("verify saw authorization_code_id %s, want the originating code %s — #2391 needs the lineage "+
			"to make its binding decision", seen.AuthorizationCodeID, code.ID)
	}
	if seen.PlainText != "" {
		t.Errorf("verify saw PlainText %q, want empty — the seam must not re-emit a credential", seen.PlainText)
	}

	// Rolled back: nothing consumed, nothing minted.
	if consumed := scanTimestamp(t, pool,
		`SELECT consumed_at FROM oauth_refresh_tokens WHERE id = $1`, grant.Refresh.ID); consumed.Valid {
		t.Error("a rejected rotation stamped consumed_at, burning the legitimate client's refresh token")
	}
	if replaced := scanInt(t, pool,
		`SELECT count(*) FROM oauth_refresh_tokens WHERE id = $1 AND replaced_by_id IS NULL`,
		grant.Refresh.ID); replaced != 1 {
		t.Error("a rejected rotation stamped replaced_by_id")
	}
	if after := countTokens(); after != before {
		t.Errorf("a rejected rotation minted %d access token(s), want 0", after-before)
	}

	// The legitimate rotation still succeeds.
	rotated, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, func(*oauthstore.RefreshToken) error {
		return nil
	}, rotateReq())
	if err != nil {
		t.Fatalf("rotation after a rejected one: %v", err)
	}
	if rotated.Refresh.ID == grant.Refresh.ID {
		t.Fatal("rotation returned the presented token, want a fresh successor")
	}
}

// TestRotateRefreshToken_ClassificationSkipsVerify pins the ordering half of the
// seam's contract: verify runs ONLY on a live token. A revoked, reused or expired
// token is refused without paying for the caller's callback, and the reuse sweep
// in particular — a security response — must not be gated on it. The counter is
// the vehicle: a verify call moved above the classification block reddens it.
func TestRotateRefreshToken_ClassificationSkipsVerify(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	calls := 0
	counting := func(*oauthstore.RefreshToken) error {
		calls++
		return nil
	}

	// Reused: rotate once, then replay the consumed token.
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	successor, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	if _, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, counting, rotateReq()); !errors.Is(err, oauthstore.ErrRefreshReused) {
		t.Fatalf("replay = %v, want ErrRefreshReused", err)
	}
	if calls != 0 {
		t.Errorf("verify was called %d time(s) on a REUSED token, want 0 — the sweep must not be gated on the "+
			"caller's callback", calls)
	}
	// And the sweep still happened, callback or not.
	if revoked := scanTimestamp(t, pool,
		`SELECT revoked_at FROM oauth_refresh_tokens WHERE id = $1`, successor.Refresh.ID); !revoked.Valid {
		t.Error("the reuse sweep did not revoke the successor when a verify callback was supplied")
	}

	// Revoked: the successor was just swept.
	if _, err := repo.RotateRefreshToken(ctx, successor.Refresh.PlainText, counting, rotateReq()); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Fatalf("rotating a revoked token = %v, want ErrRevoked", err)
	}

	// Expired but never rotated.
	expiredCode := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	expiredMint := mintReq()
	expiredMint.RefreshTokenExpiry = time.Now().UTC().Add(-time.Minute)
	expiredGrant, err := repo.RedeemAuthorizationCode(ctx, expiredCode.PlainText, nil, expiredMint)
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode (expired): %v", err)
	}
	if _, err := repo.RotateRefreshToken(ctx, expiredGrant.Refresh.PlainText, counting, rotateReq()); !errors.Is(err, oauthstore.ErrExpired) {
		t.Fatalf("rotating an expired token = %v, want ErrExpired", err)
	}

	if calls != 0 {
		t.Errorf("verify was called %d time(s) across the revoked/reused/expired paths, want 0", calls)
	}
}

// TestRotateRefreshToken_ReuseRevokesLineageAfterReturn is where rejection
// defect 1 is pinned. Presenting an already-rotated refresh token must return
// ErrRefreshReused AND leave the whole lineage revoked in COMMITTED state.
//
// The revoked_at assertions read from a pool connection acquired AFTER the call
// returned — never from inside the repository's transaction — which is exactly
// what a rollback-on-error implementation cannot satisfy.
//
// MANDATED COUNTERFACTUAL (c): return ErrRefreshReused from inside
// RotateRefreshToken's pgx.BeginFunc callback instead of setting the captured
// flag and returning nil. The RETURNED error is unchanged, so an
// error-identity-only test stays green; these post-call raw-SQL assertions go
// RED because BeginFunc rolls the revocation back.
func TestRotateRefreshToken_ReuseRevokesLineageAfterReturn(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	successor, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}

	// Present the ALREADY-ROTATED token a second time.
	if _, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq()); !errors.Is(err, oauthstore.ErrRefreshReused) {
		t.Fatalf("reused refresh token = %v, want ErrRefreshReused", err)
	}

	// COMMITTED state, read after the call returned.
	for _, tc := range []struct {
		what  string
		table string
		id    uuid.UUID
	}{
		{"descendant access token", "oauth_access_tokens", grant.Access.ID},
		{"successor access token", "oauth_access_tokens", successor.Access.ID},
		{"presented refresh token", "oauth_refresh_tokens", grant.Refresh.ID},
		{"successor refresh token", "oauth_refresh_tokens", successor.Refresh.ID},
	} {
		got := scanTimestamp(t, pool, `SELECT revoked_at FROM `+tc.table+` WHERE id = $1`, tc.id)
		if !got.Valid {
			t.Errorf("%s revoked_at is NULL after the reuse call returned, want NON-NULL (the sweep must be COMMITTED, not rolled back)", tc.what)
		}
	}

	// The same fact through the public API — reachable only because the
	// access-token lookup is unfiltered on revoked_at.
	if _, err := repo.AuthenticateAccessToken(ctx, successor.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("successor access token after reuse = %v, want ErrRevoked", err)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, grant.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("descendant access token after reuse = %v, want ErrRevoked", err)
	}

	// A THIRD presentation pins the post-sweep classification ORDER: the token
	// is now revoked, so ErrRevoked wins over the reuse verdict.
	if _, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq()); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("third presentation = %v, want ErrRevoked (revoked is classified before consumed)", err)
	}
}

// TestRotateRefreshToken_ReuseSweepSerializesOnLineage is the COUNTERFACTUAL
// VEHICLE for the lineage lock, and therefore for the whole-lineage revocation
// guarantee holding under CONCURRENCY rather than only in isolation.
//
// The hole it closes: per-token row locks do not serialize a rotation against a
// sweep of the same lineage. The sweep's `UPDATE … WHERE authorization_code_id
// = $1` takes its statement snapshot when the statement starts, so a rotation
// that INSERTs a successor pair and commits after that instant produces rows the
// sweep can never see — a usable descendant credential surviving reuse
// detection, which is exactly what the API's guarantee forbids.
//
// A racing test cannot pin that reliably (the losing interleaving is timing
// dependent), so this asserts the MECHANISM directly and deterministically: an
// outside transaction holds the authorization-code row's FOR UPDATE lock, and
// the reuse call must BLOCK on it. Delete lockLineage from RotateRefreshToken
// and the call sails straight through — the sweep's UPDATEs never touch the code
// row — so the first assertion goes red.
func TestRotateRefreshToken_ReuseSweepSerializesOnLineage(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	successor, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}

	// Hold the lineage row lock from OUTSIDE the repository — this stands in for
	// a concurrent rotation that is mid-flight and has not yet committed.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocking transaction: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx,
		`SELECT id FROM oauth_authorization_codes WHERE id = $1 FOR UPDATE`, code.ID); err != nil {
		t.Fatalf("lock the lineage row: %v", err)
	}

	// Replay the already-rotated token. It must queue behind the lineage lock.
	done := make(chan error, 1)
	go func() {
		_, e := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
		done <- e
	}()

	select {
	case e := <-done:
		t.Fatalf("the reuse sweep completed (err = %v) while an outside transaction held the lineage row lock — "+
			"RotateRefreshToken does not serialize on the lineage, so a concurrent rotation can commit a successor "+
			"pair the sweep's snapshot never sees", e)
	case <-time.After(timescale.D(500 * time.Millisecond)):
		// Still blocked: the lineage lock is real.
	}

	// Release it; the sweep now proceeds and commits.
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release the lineage lock: %v", err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, oauthstore.ErrRefreshReused) {
			t.Fatalf("reuse after the lineage lock was released = %v, want ErrRefreshReused", e)
		}
	case <-time.After(timescale.D(15 * time.Second)):
		t.Fatal("the reuse sweep never completed after the lineage lock was released — it is deadlocked, not serialized")
	}

	if n := scanInt(t, pool,
		`SELECT count(*) FROM oauth_access_tokens WHERE authorization_code_id = $1 AND revoked_at IS NULL`,
		code.ID); n != 0 {
		t.Errorf("%d access tokens still live after the sweep, want 0", n)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, successor.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("successor access token after the serialized sweep = %v, want ErrRevoked", err)
	}
}

// TestRotateRefreshToken_ReuseLeavesNoLiveDescendantUnderConcurrency asserts the
// END-TO-END invariant the lineage lock exists to deliver, under a real race: a
// legitimate rotation of the LIVE successor runs concurrently with a replay of
// the CONSUMED predecessor, and once the reuse verdict is returned NO credential
// in that lineage may still be usable.
//
// Both admissible interleavings are safe and both are accepted here: either the
// rotation commits first (and the sweep, queued behind the lineage lock, sees its
// successor), or the sweep commits first (and the rotation's post-lock re-read
// observes revoked_at and returns ErrRevoked, minting nothing). What is NOT
// admissible — a successful rotation whose tokens survive the sweep — is what the
// live-row count and the AuthenticateAccessToken assertion below reject.
//
// This is the invariant, NOT the counterfactual vehicle: the losing interleaving
// is timing dependent, so removing the lock would make this test FLAKY rather
// than reliably red. TestRotateRefreshToken_ReuseSweepSerializesOnLineage is
// where the mechanism is pinned deterministically.
func TestRotateRefreshToken_ReuseLeavesNoLiveDescendantUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	const rounds = 8
	for round := range rounds {
		code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
		grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
		if err != nil {
			t.Fatalf("round %d redeem: %v", round, err)
		}
		// One clean rotation, so the predecessor is consumed (the reuse trigger)
		// and the successor is live (the credential that must not survive).
		successor, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
		if err != nil {
			t.Fatalf("round %d first rotation: %v", round, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		var rotateGrant *oauthstore.IssuedGrant
		var rotateErr, reuseErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			rotateGrant, rotateErr = repo.RotateRefreshToken(ctx, successor.Refresh.PlainText, nil, rotateReq())
		}()
		go func() {
			defer wg.Done()
			<-start
			_, reuseErr = repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
		}()
		close(start)
		wg.Wait()

		if !errors.Is(reuseErr, oauthstore.ErrRefreshReused) {
			t.Fatalf("round %d replay of the consumed predecessor = %v, want ErrRefreshReused", round, reuseErr)
		}
		if rotateErr != nil && !errors.Is(rotateErr, oauthstore.ErrRevoked) {
			t.Fatalf("round %d concurrent rotation = %v, want nil or ErrRevoked (no other class)", round, rotateErr)
		}

		if n := scanInt(t, pool,
			`SELECT count(*) FROM oauth_access_tokens WHERE authorization_code_id = $1 AND revoked_at IS NULL`,
			code.ID); n != 0 {
			t.Errorf("round %d: %d access tokens in the lineage are still live after reuse detection, want 0", round, n)
		}
		if n := scanInt(t, pool,
			`SELECT count(*) FROM oauth_refresh_tokens WHERE authorization_code_id = $1 AND revoked_at IS NULL`,
			code.ID); n != 0 {
			t.Errorf("round %d: %d refresh tokens in the lineage are still live after reuse detection, want 0", round, n)
		}
		// The concrete harm, through the public API: if the racing rotation won,
		// its access token must NOT authenticate.
		if rotateErr == nil {
			if _, err := repo.AuthenticateAccessToken(ctx, rotateGrant.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
				t.Errorf("round %d: the successor minted by the racing rotation = %v, want ErrRevoked — it escaped the reuse sweep",
					round, err)
			}
		}
	}
}

// TestRotateRefreshToken_ExpiredReuseStillSweepsLineage pins the reused-BEFORE-
// expired classification order. An already-rotated token that has ALSO lapsed is
// still a replay, and a replay is the compromise signal the sweep exists for —
// the presented token's expiry says nothing about its SUCCESSORS, which carry
// later expiries and are the credentials actually at risk.
//
// COUNTERFACTUAL: move the IsExpired check ahead of IsConsumed in
// RotateRefreshToken and this test goes red twice over — the returned error
// becomes ErrExpired and the live successor is never swept.
func TestRotateRefreshToken_ExpiredReuseStillSweepsLineage(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	successor, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}

	// The stolen predecessor lapses before it is replayed. Only its OWN
	// expires_at moves; the successor pair keeps the live expiries it was minted
	// with, which is the whole point.
	if _, err := pool.Exec(ctx,
		`UPDATE oauth_refresh_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		grant.Refresh.ID); err != nil {
		t.Fatalf("lapse the presented token: %v", err)
	}

	if _, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq()); !errors.Is(err, oauthstore.ErrRefreshReused) {
		t.Fatalf("replay of an expired-AND-consumed token = %v, want ErrRefreshReused (a late replay is still a replay)", err)
	}

	// The signal was acted on, not merely reported: the successor pair — whose
	// expiries have NOT lapsed — is revoked.
	if _, err := repo.AuthenticateAccessToken(ctx, successor.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("successor access token after an expired replay = %v, want ErrRevoked — the sweep was skipped", err)
	}
	if n := scanInt(t, pool,
		`SELECT count(*) FROM oauth_refresh_tokens WHERE authorization_code_id = $1 AND revoked_at IS NULL`,
		code.ID); n != 0 {
		t.Errorf("%d refresh tokens still live after an expired replay, want 0", n)
	}
}

// TestRotateRefreshToken_ExpiredUnknownAreDistinct covers the remaining
// rotation failure branches: a lapsed token that was NEVER rotated yields
// ErrExpired (the expired-AND-consumed combination is a reuse signal instead —
// see TestRotateRefreshToken_ExpiredReuseStillSweepsLineage) and a never-issued
// one ErrNotFound. (Revoked is covered by the third presentation in the reuse
// test above.)
func TestRotateRefreshToken_ExpiredUnknownAreDistinct(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	expiredMint := mintReq()
	expiredMint.RefreshTokenExpiry = time.Now().UTC().Add(-time.Minute)
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, expiredMint)
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	if _, err := repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq()); !errors.Is(err, oauthstore.ErrExpired) {
		t.Errorf("expired refresh token = %v, want ErrExpired", err)
	}
	// An expired rotation must not consume or mint.
	if consumed := scanTimestamp(t, pool, `SELECT consumed_at FROM oauth_refresh_tokens WHERE id = $1`, grant.Refresh.ID); consumed.Valid {
		t.Error("an expired rotation stamped consumed_at, want NULL")
	}
	if n := scanInt(t, pool, `SELECT count(*) FROM oauth_refresh_tokens WHERE authorization_code_id = $1`, code.ID); n != 1 {
		t.Errorf("refresh rows after an expired rotation = %d, want 1 (no successor minted)", n)
	}

	never, err := oauthstore.GeneratePlaintext(oauthstore.RefreshTokenPrefix)
	if err != nil {
		t.Fatalf("GeneratePlaintext: %v", err)
	}
	if _, err := repo.RotateRefreshToken(ctx, never, nil, rotateReq()); !errors.Is(err, oauthstore.ErrNotFound) {
		t.Errorf("never-issued refresh token = %v, want ErrNotFound", err)
	}
}

// TestRevokeGrantsForCode_ScopedToLineage is the RFC 6749 §4.1.2 replay
// response: revoking one code's grants must revoke exactly that code's
// descendants and leave an unrelated code's tokens untouched.
func TestRevokeGrantsForCode_ScopedToLineage(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	codeA := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grantA, err := repo.RedeemAuthorizationCode(ctx, codeA.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("redeem A: %v", err)
	}
	codeB := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grantB, err := repo.RedeemAuthorizationCode(ctx, codeB.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("redeem B: %v", err)
	}

	n, err := repo.RevokeGrantsForCode(ctx, codeA.ID)
	if err != nil {
		t.Fatalf("RevokeGrantsForCode: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d rows, want 2 (one access + one refresh)", n)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, grantA.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("A's access token = %v, want ErrRevoked", err)
	}
	if _, err := repo.AuthenticateAccessToken(ctx, grantB.Access.PlainText); err != nil {
		t.Errorf("B's access token was caught by A's sweep: %v", err)
	}
	if _, err := repo.RotateRefreshToken(ctx, grantA.Refresh.PlainText, nil, rotateReq()); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("A's refresh token = %v, want ErrRevoked", err)
	}

	// Idempotent: a second sweep finds nothing still active.
	if n, err := repo.RevokeGrantsForCode(ctx, codeA.ID); err != nil || n != 0 {
		t.Errorf("second RevokeGrantsForCode = (%d, %v), want (0, nil)", n, err)
	}
	// An unknown code id revokes nothing rather than erroring.
	if n, err := repo.RevokeGrantsForCode(ctx, uuid.New()); err != nil || n != 0 {
		t.Errorf("RevokeGrantsForCode(unknown) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestRevokeToken_IdempotentAndNotFound covers both by-id revoke paths: the
// COALESCE idempotence (a second revoke keeps the ORIGINAL revoked_at) and the
// unknown-id miss branch.
func TestRevokeToken_IdempotentAndNotFound(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}

	first, err := repo.RevokeAccessToken(ctx, grant.Access.ID)
	if err != nil {
		t.Fatalf("RevokeAccessToken: %v", err)
	}
	second, err := repo.RevokeAccessToken(ctx, grant.Access.ID)
	if err != nil {
		t.Fatalf("second RevokeAccessToken: %v", err)
	}
	if first.RevokedAt == nil || second.RevokedAt == nil || !first.RevokedAt.Equal(*second.RevokedAt) {
		t.Errorf("access revoked_at moved on the second revoke: %v → %v", first.RevokedAt, second.RevokedAt)
	}

	firstR, err := repo.RevokeRefreshToken(ctx, grant.Refresh.ID)
	if err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	secondR, err := repo.RevokeRefreshToken(ctx, grant.Refresh.ID)
	if err != nil {
		t.Fatalf("second RevokeRefreshToken: %v", err)
	}
	if firstR.RevokedAt == nil || secondR.RevokedAt == nil || !firstR.RevokedAt.Equal(*secondR.RevokedAt) {
		t.Errorf("refresh revoked_at moved on the second revoke: %v → %v", firstR.RevokedAt, secondR.RevokedAt)
	}

	if _, err := repo.RevokeAccessToken(ctx, uuid.New()); !errors.Is(err, oauthstore.ErrNotFound) {
		t.Errorf("RevokeAccessToken(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := repo.RevokeRefreshToken(ctx, uuid.New()); !errors.Is(err, oauthstore.ErrNotFound) {
		t.Errorf("RevokeRefreshToken(unknown) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Clients
// ---------------------------------------------------------------------------

// TestUpsertClient_RefreshesOnConflictAndIsProviderScoped is the acceptance
// criterion for #2433 approval condition 3: a CHANGED CIMD document UPDATES the
// persisted row (refresh-on-fetch), rather than being pinned to its first-use
// snapshot. first_seen_at is preserved across the refresh; updated_at moves.
// The same client_id under a different provider is a DISTINCT row.
func TestUpsertClient_RefreshesOnConflictAndIsProviderScoped(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	first, err := repo.UpsertClient(ctx, oauthstore.NewClient{
		Provider: "github",
		Metadata: oauthas.ClientMetadata{
			ClientID:                testClientID,
			RedirectURIs:            []string{"http://127.0.0.1:8765/callback"},
			GrantTypes:              []string{"authorization_code"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
			ClientName:              "Fishhawk CLI",
		},
	})
	if err != nil {
		t.Fatalf("UpsertClient: %v", err)
	}

	// The client rotates its redirect URIs and renames itself. Refresh-on-fetch
	// means the persisted row FOLLOWS the document.
	second, err := repo.UpsertClient(ctx, oauthstore.NewClient{
		Provider: "github",
		Metadata: oauthas.ClientMetadata{
			ClientID:                testClientID,
			RedirectURIs:            []string{"http://127.0.0.1:9999/cb", "http://[::1]:9999/cb"},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
			ClientName:              "Fishhawk CLI v2",
			ClientURI:               "https://client.example.com",
			Scope:                   "runs:read runs:write",
		},
	})
	if err != nil {
		t.Fatalf("UpsertClient (refresh): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("refresh created a NEW row (%s → %s), want the same registration updated", first.ID, second.ID)
	}
	if len(second.RedirectURIs) != 2 || second.RedirectURIs[0] != "http://127.0.0.1:9999/cb" {
		t.Errorf("redirect_uris = %v, want the refreshed document's", second.RedirectURIs)
	}
	if second.ClientName != "Fishhawk CLI v2" || second.Scope != "runs:read runs:write" {
		t.Errorf("client_name/scope = %q/%q, want the refreshed document's", second.ClientName, second.Scope)
	}
	if !second.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Errorf("first_seen_at moved on refresh: %v → %v", first.FirstSeenAt, second.FirstSeenAt)
	}
	// STRICTLY after. The previous `after || equal` form was not load-bearing:
	// it permitted an unchanged updated_at, so a DO UPDATE that dropped
	// `updated_at = now()` would have passed it — the very half of "first_seen_at
	// is preserved, updated_at MOVES" it was there to assert. The two upserts run
	// in separate transactions, so their now() values are distinct by
	// construction.
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at = %v, want strictly after the first insert's %v — refresh-on-fetch must move it", second.UpdatedAt, first.UpdatedAt)
	}

	// A read returns the refreshed document, not the pinned original.
	got, err := repo.GetClient(ctx, "github", testClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if len(got.RedirectURIs) != 2 {
		t.Errorf("GetClient redirect_uris = %v, want the refreshed pair", got.RedirectURIs)
	}
	if got.ClientURI != "https://client.example.com" {
		t.Errorf("GetClient client_uri = %q, want the refreshed value", got.ClientURI)
	}

	// Provider scoping: the same CIMD URL under gitlab is a DISTINCT row.
	gitlab, err := repo.UpsertClient(ctx, oauthstore.NewClient{
		Provider: "gitlab",
		Metadata: oauthas.ClientMetadata{
			ClientID:                testClientID,
			RedirectURIs:            []string{"http://127.0.0.1:1234/cb"},
			TokenEndpointAuthMethod: "none",
		},
	})
	if err != nil {
		t.Fatalf("UpsertClient (gitlab): %v", err)
	}
	if gitlab.ID == first.ID {
		t.Error("the same client_id under a different provider reused the github row, want a distinct registration")
	}
	if _, err := repo.GetClient(ctx, "gitlab", testAltClientID); !errors.Is(err, oauthstore.ErrNotFound) {
		t.Errorf("GetClient(unknown) = %v, want ErrNotFound", err)
	}
}

// TestUpsertClient_RejectsMissingIdentifiers covers the two required-field
// guards, which refuse before any database round-trip.
func TestUpsertClient_RejectsMissingIdentifiers(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	pool.Close()

	if _, err := repo.UpsertClient(ctx, oauthstore.NewClient{Metadata: oauthas.ClientMetadata{ClientID: testClientID}}); err == nil ||
		!strings.Contains(err.Error(), "provider required") {
		t.Errorf("UpsertClient with no provider = %v, want a provider-required error", err)
	}
	if _, err := repo.UpsertClient(ctx, oauthstore.NewClient{Provider: "github"}); err == nil ||
		!strings.Contains(err.Error(), "client_id required") {
		t.Errorf("UpsertClient with no client_id = %v, want a client_id-required error", err)
	}
}

// TestCreateAuthorizationCode_RejectsMissingIdentifiers covers the mirror
// guards on the code-mint path.
func TestCreateAuthorizationCode_RejectsMissingIdentifiers(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)
	pool.Close()

	if _, err := repo.CreateAuthorizationCode(ctx, oauthstore.NewAuthorizationCode{ClientID: testClientID}); err == nil ||
		!strings.Contains(err.Error(), "subject required") {
		t.Errorf("CreateAuthorizationCode with no subject = %v, want a subject-required error", err)
	}
	if _, err := repo.CreateAuthorizationCode(ctx, oauthstore.NewAuthorizationCode{Subject: testSubject}); err == nil ||
		!strings.Contains(err.Error(), "client_id required") {
		t.Errorf("CreateAuthorizationCode with no client_id = %v, want a client_id-required error", err)
	}
}

// TestAccountID_RoundTripsAndRejectsMalformed covers the tenant column both
// ways: a real account uuid round-trips onto every row of the grant, an unset
// account persists as NULL (the untenanted window), and a malformed string is
// REFUSED rather than silently dropping the tenant.
func TestAccountID_RoundTripsAndRejectsMalformed(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	accountID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, provider, account_key, granularity) VALUES ($1, 'github', $2, 'enterprise')`,
		accountID, "acct-"+accountID.String()); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	code, err := repo.CreateAuthorizationCode(ctx, oauthstore.NewAuthorizationCode{
		ClientID:            testClientID,
		RedirectURI:         "http://127.0.0.1:8765/callback",
		CodeChallenge:       oauthas.DeriveS256Challenge("verifier-verifier-verifier-verifier"),
		CodeChallengeMethod: oauthas.CodeChallengeMethodS256,
		Subject:             testSubject,
		AccountID:           accountID.String(),
		ExpiresAt:           time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateAuthorizationCode: %v", err)
	}
	if code.AccountID != accountID.String() {
		t.Errorf("code AccountID = %q, want %q", code.AccountID, accountID)
	}
	// Provider defaulted to 'github' from the empty input, matching the column
	// DEFAULT rather than failing the provider CHECK on an empty string.
	if code.Provider != "github" {
		t.Errorf("code Provider = %q, want the defaulted \"github\"", code.Provider)
	}

	mint := mintReq()
	mint.AccountID = accountID.String()
	mint.Provider = ""
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mint)
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	if grant.Access.AccountID != accountID.String() || grant.Refresh.AccountID != accountID.String() {
		t.Errorf("token AccountID = %q/%q, want %q", grant.Access.AccountID, grant.Refresh.AccountID, accountID)
	}
	if grant.Access.Provider != "github" {
		t.Errorf("token Provider = %q, want the defaulted \"github\"", grant.Access.Provider)
	}

	// Untenanted rows persist as NULL.
	untenanted := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	if untenanted.AccountID != "" {
		t.Errorf("untenanted code AccountID = %q, want empty", untenanted.AccountID)
	}
	if n := scanInt(t, pool,
		`SELECT count(*) FROM oauth_authorization_codes WHERE id = $1 AND account_id IS NULL`, untenanted.ID); n != 1 {
		t.Error("untenanted code did not persist a NULL account_id")
	}

	// Malformed is refused, not dropped.
	if _, err := repo.CreateAuthorizationCode(ctx, oauthstore.NewAuthorizationCode{
		ClientID:  testClientID,
		Subject:   testSubject,
		AccountID: "not-a-uuid",
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}); err == nil || !strings.Contains(err.Error(), "invalid account_id") {
		t.Errorf("CreateAuthorizationCode with a malformed account_id = %v, want an invalid-account_id error", err)
	}
	if _, err := repo.UpsertClient(ctx, oauthstore.NewClient{
		Provider:  "github",
		Metadata:  oauthas.ClientMetadata{ClientID: testClientID},
		AccountID: "not-a-uuid",
	}); err == nil || !strings.Contains(err.Error(), "invalid account_id") {
		t.Errorf("UpsertClient with a malformed account_id = %v, want an invalid-account_id error", err)
	}
	if _, err := repo.RedeemAuthorizationCode(ctx, untenanted.PlainText, nil, oauthstore.MintRequest{
		Subject:   testSubject,
		AccountID: "not-a-uuid",
	}); err == nil || !strings.Contains(err.Error(), "invalid account_id") {
		t.Errorf("RedeemAuthorizationCode with a malformed mint account_id = %v, want an invalid-account_id error", err)
	}
}

// TestRepository_DatabaseErrorsAreNotMisclassified is the store-unavailable
// failure mode. Every entry point must surface a real error when the database
// is unreachable — NEVER a sentinel, because reporting ErrNotFound for an
// outage would tell #2391's token endpoint "no such credential" when the truth
// is "we could not look".
func TestRepository_DatabaseErrorsAreNotMisclassified(t *testing.T) {
	ctx := context.Background()
	repo, pool := newRepo(t)

	// Mint real credentials, THEN take the database away.
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	grant, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq())
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	live := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	pool.Close()

	sentinels := []error{
		oauthstore.ErrNotFound, oauthstore.ErrRevoked, oauthstore.ErrExpired,
		oauthstore.ErrCodeConsumed, oauthstore.ErrRefreshReused, oauthstore.ErrMalformedCredential,
	}
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s succeeded against a closed pool, want an error", name)
			return
		}
		for _, s := range sentinels {
			if errors.Is(err, s) {
				t.Errorf("%s = %v, want a raw database error rather than the %v sentinel", name, err, s)
			}
		}
	}

	_, err = repo.UpsertClient(ctx, oauthstore.NewClient{
		Provider: "github",
		Metadata: oauthas.ClientMetadata{ClientID: testClientID},
	})
	check("UpsertClient", err)
	_, err = repo.GetClient(ctx, "github", testClientID)
	check("GetClient", err)
	_, err = repo.CreateAuthorizationCode(ctx, oauthstore.NewAuthorizationCode{
		ClientID: testClientID, Subject: testSubject, ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	check("CreateAuthorizationCode", err)
	_, err = repo.LookupAuthorizationCode(ctx, live.PlainText)
	check("LookupAuthorizationCode", err)
	_, err = repo.RedeemAuthorizationCode(ctx, live.PlainText, nil, mintReq())
	check("RedeemAuthorizationCode", err)
	_, err = repo.AuthenticateAccessToken(ctx, grant.Access.PlainText)
	check("AuthenticateAccessToken", err)
	_, err = repo.RotateRefreshToken(ctx, grant.Refresh.PlainText, nil, rotateReq())
	check("RotateRefreshToken", err)
	_, err = repo.RevokeGrantsForCode(ctx, code.ID)
	check("RevokeGrantsForCode", err)
	_, err = repo.RevokeAccessToken(ctx, grant.Access.ID)
	check("RevokeAccessToken", err)
	_, err = repo.RevokeRefreshToken(ctx, grant.Refresh.ID)
	check("RevokeRefreshToken", err)
}

// TestSchema_RejectsUnsupportedEnumValues pins the two database-level CHECK
// constraints the domain contract is encoded in: a non-S256 PKCE method and an
// unknown forge are refused by POSTGRES, not only by oauthas. The repository
// surfaces those as database errors (its own guards are about identifiers, not
// enums), which is the branch asserted here.
func TestSchema_RejectsUnsupportedEnumValues(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if _, err := repo.CreateAuthorizationCode(ctx, oauthstore.NewAuthorizationCode{
		ClientID:            testClientID,
		Subject:             testSubject,
		CodeChallengeMethod: "plain",
		ExpiresAt:           time.Now().UTC().Add(time.Minute),
	}); err == nil || !strings.Contains(err.Error(), "challenge_method_check") {
		t.Errorf("CreateAuthorizationCode with method=plain = %v, want the S256 CHECK violation", err)
	}
	if _, err := repo.CreateAuthorizationCode(ctx, oauthstore.NewAuthorizationCode{
		ClientID:            testClientID,
		Subject:             testSubject,
		CodeChallengeMethod: oauthas.CodeChallengeMethodS256,
		Provider:            "bitbucket",
		ExpiresAt:           time.Now().UTC().Add(time.Minute),
	}); err == nil || !strings.Contains(err.Error(), "provider_check") {
		t.Errorf("CreateAuthorizationCode with provider=bitbucket = %v, want the provider CHECK violation", err)
	}

	// The mint path propagates the same refusal rather than silently minting.
	code := newCode(t, repo, time.Now().UTC().Add(time.Minute))
	mint := mintReq()
	mint.Provider = "bitbucket"
	if _, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mint); err == nil ||
		!strings.Contains(err.Error(), "provider_check") {
		t.Errorf("RedeemAuthorizationCode with an invalid mint provider = %v, want the provider CHECK violation", err)
	}
	// ...and the rollback left the code redeemable.
	if _, err := repo.RedeemAuthorizationCode(ctx, code.PlainText, nil, mintReq()); err != nil {
		t.Errorf("redemption after a failed mint: %v — the failed mint burned the code", err)
	}
}
