package oauthstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	oauthstoredb "github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore/db"
)

// postgresRepo is the production Repository implementation.
type postgresRepo struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository. Caller
// retains ownership of pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{
		pool: pool,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

func (r *postgresRepo) UpsertClient(ctx context.Context, in NewClient) (*Client, error) {
	if in.Provider == "" {
		return nil, errors.New("oauthstore: provider required")
	}
	if in.Metadata.ClientID == "" {
		return nil, errors.New("oauthstore: client_id required")
	}
	accountID, err := parseAccountID(in.AccountID)
	if err != nil {
		return nil, err
	}
	q := oauthstoredb.New(r.pool)
	row, err := q.UpsertClient(ctx, oauthstoredb.UpsertClientParams{
		ID:                      uuid.New(),
		Provider:                in.Provider,
		ClientID:                in.Metadata.ClientID,
		RedirectUris:            emptyIfNil(in.Metadata.RedirectURIs),
		GrantTypes:              emptyIfNil(in.Metadata.GrantTypes),
		ResponseTypes:           emptyIfNil(in.Metadata.ResponseTypes),
		TokenEndpointAuthMethod: in.Metadata.TokenEndpointAuthMethod,
		ClientName:              nilIfEmpty(in.Metadata.ClientName),
		ClientUri:               nilIfEmpty(in.Metadata.ClientURI),
		LogoUri:                 nilIfEmpty(in.Metadata.LogoURI),
		Scope:                   nilIfEmpty(in.Metadata.Scope),
		AccountID:               accountID,
	})
	if err != nil {
		return nil, fmt.Errorf("oauthstore: upsert client: %w", err)
	}
	return rowToClient(row), nil
}

func (r *postgresRepo) GetClient(ctx context.Context, provider, clientID string) (*Client, error) {
	q := oauthstoredb.New(r.pool)
	row, err := q.GetClientByClientID(ctx, oauthstoredb.GetClientByClientIDParams{
		Provider: provider,
		ClientID: clientID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oauthstore: get client: %w", err)
	}
	return rowToClient(row), nil
}

func (r *postgresRepo) CreateAuthorizationCode(ctx context.Context, in NewAuthorizationCode) (*AuthorizationCode, error) {
	if in.Subject == "" {
		return nil, errors.New("oauthstore: subject required")
	}
	if in.ClientID == "" {
		return nil, errors.New("oauthstore: client_id required")
	}
	plaintext, err := GeneratePlaintext(CodePrefix)
	if err != nil {
		return nil, err
	}
	hash, err := HashPlaintext(CodePrefix, plaintext)
	if err != nil {
		return nil, err
	}
	accountID, err := parseAccountID(in.AccountID)
	if err != nil {
		return nil, err
	}
	q := oauthstoredb.New(r.pool)
	row, err := q.CreateAuthorizationCode(ctx, oauthstoredb.CreateAuthorizationCodeParams{
		ID:                  uuid.New(),
		CodeHash:            hash,
		ClientID:            in.ClientID,
		RedirectUri:         in.RedirectURI,
		CodeChallenge:       in.CodeChallenge,
		CodeChallengeMethod: string(in.CodeChallengeMethod),
		Scopes:              emptyIfNil(in.Scopes),
		Resource:            nilIfEmpty(in.Resource),
		Subject:             in.Subject,
		Provider:            defaultProvider(in.Provider),
		AccountID:           accountID,
		ExpiresAt:           tstz(in.ExpiresAt),
	})
	if err != nil {
		return nil, fmt.Errorf("oauthstore: create authorization code: %w", err)
	}
	code := rowToCode(row)
	code.PlainText = plaintext
	return code, nil
}

func (r *postgresRepo) LookupAuthorizationCode(ctx context.Context, plaintext string) (*AuthorizationCode, error) {
	hash, err := HashPlaintext(CodePrefix, plaintext)
	if err != nil {
		return nil, err
	}
	q := oauthstoredb.New(r.pool)
	row, err := q.GetAuthorizationCodeByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oauthstore: lookup authorization code: %w", err)
	}
	return rowToCode(row), nil
}

func (r *postgresRepo) RedeemAuthorizationCode(ctx context.Context, plaintext string, verify func(*AuthorizationCode) error, redeem RedemptionRequest) (*IssuedGrant, error) {
	hash, err := HashPlaintext(CodePrefix, plaintext)
	if err != nil {
		return nil, err
	}
	var grant *IssuedGrant
	txErr := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := oauthstoredb.New(tx)
		row, err := q.LockAuthorizationCodeByHash(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("oauthstore: lock authorization code: %w", err)
		}
		code := rowToCode(row)

		// Ordering is the contract (see Repository.RedeemAuthorizationCode):
		// a consumed or lapsed code is refused WITHOUT invoking verify, so a
		// caller's verifier never runs against a dead grant. This post-lock
		// consumed check is the REDUNDANT short-circuit — the load-bearing
		// single-use control is ConsumeAuthorizationCode's
		// `AND consumed_at IS NULL` predicate below. Kept because it yields a
		// typed refusal without paying for the caller's verify callback;
		// pinned by TestRedeemAuthorizationCode_ConsumedCodeSkipsVerify.
		if code.IsConsumed() {
			return ErrCodeConsumed
		}
		if code.IsExpired(r.now()) {
			return ErrExpired
		}
		if verify != nil {
			if err := verify(code); err != nil {
				// Returned UNCHANGED so BeginFunc rolls back: a wrong verifier
				// must not burn the legitimate client's code.
				return err
			}
		}

		consumed, err := q.ConsumeAuthorizationCode(ctx, oauthstoredb.ConsumeAuthorizationCodeParams{
			ID:         code.ID,
			ConsumedAt: tstz(r.now()),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The conditional predicate rejected the row: another transaction
			// consumed it between our snapshot and the UPDATE.
			return ErrCodeConsumed
		}
		if err != nil {
			return fmt.Errorf("oauthstore: consume authorization code: %w", err)
		}

		// Authority AND lineage are both DERIVED from the row this transaction
		// just consumed (#2433 implement-review follow-up): the pair inherits the
		// code's own subject/client/scopes/provider/account and its resource
		// indicator as the audience, and is stamped with the code's id. Taking
		// those from the caller instead would let possession of ONE code mint
		// credentials carrying authority the code never granted — the same
		// escalation the rotation path closed, on the other mint path.
		consumedCode := rowToCode(consumed)
		issued, err := mintPair(ctx, q, consumedCode.ID, codeAuthority(consumedCode, redeem))
		if err != nil {
			return err
		}
		issued.Code = consumedCode
		grant = issued
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return grant, nil
}

func (r *postgresRepo) RevokeGrantsForCode(ctx context.Context, codeID uuid.UUID) (int64, error) {
	var n int64
	// Runs under the SAME lineage lock RotateRefreshToken takes, acquired in the
	// same order (code row, then descendants), so a rotation concurrent with
	// this sweep is serialized rather than able to commit a successor pair the
	// sweep's statement snapshot cannot see. See lockLineage.
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := oauthstoredb.New(tx)
		if err := lockLineage(ctx, q, codeID); err != nil {
			return err
		}
		var err error
		n, err = r.revokeLineage(ctx, q, codeID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// lockLineage takes the authorization-code row's FOR UPDATE lock, which is the
// mutex for that code's WHOLE lineage. Every path that mints into a lineage or
// sweeps one takes it FIRST, so the ordering is uniform and deadlock-free
// (RedeemAuthorizationCode's LockAuthorizationCodeByHash is the same lock, taken
// at the same point).
//
// This is what makes the whole-lineage revocation guarantee hold under
// CONCURRENCY rather than only in isolation — see the query's own comment in
// queries.sql for why per-token row locks are not sufficient. A missing code row
// locks nothing and is not an error.
func lockLineage(ctx context.Context, q *oauthstoredb.Queries, codeID uuid.UUID) error {
	if err := q.LockAuthorizationCodeByID(ctx, codeID); err != nil {
		return fmt.Errorf("oauthstore: lock authorization code lineage: %w", err)
	}
	return nil
}

// revokeLineage revokes every still-active access and refresh token descended
// from codeID and returns the total row count. Shared by RevokeGrantsForCode
// and RotateRefreshToken's reuse path so the two can never drift. Callers MUST
// hold the lineage lock (lockLineage) first.
func (r *postgresRepo) revokeLineage(ctx context.Context, q *oauthstoredb.Queries, codeID uuid.UUID) (int64, error) {
	at := tstz(r.now())
	access, err := q.RevokeAccessTokensForCode(ctx, oauthstoredb.RevokeAccessTokensForCodeParams{
		AuthorizationCodeID: codeID,
		RevokedAt:           at,
	})
	if err != nil {
		return 0, fmt.Errorf("oauthstore: revoke access tokens for code: %w", err)
	}
	refresh, err := q.RevokeRefreshTokensForCode(ctx, oauthstoredb.RevokeRefreshTokensForCodeParams{
		AuthorizationCodeID: codeID,
		RevokedAt:           at,
	})
	if err != nil {
		return 0, fmt.Errorf("oauthstore: revoke refresh tokens for code: %w", err)
	}
	return access + refresh, nil
}

func (r *postgresRepo) AuthenticateAccessToken(ctx context.Context, plaintext string) (*AccessToken, error) {
	hash, err := HashPlaintext(AccessTokenPrefix, plaintext)
	if err != nil {
		return nil, err
	}
	q := oauthstoredb.New(r.pool)
	// UNFILTERED lookup (GetAccessTokenByHash carries no revoked_at predicate),
	// so classification happens HERE and revocation stays an observable state
	// rather than an absence. Collapsing any pair of these three outcomes
	// reddens TestAuthenticateAccessToken_RevokedExpiredUnknownAreDistinct.
	row, err := q.GetAccessTokenByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oauthstore: lookup access token: %w", err)
	}
	tok := rowToAccessToken(row)
	if tok.IsRevoked() {
		return nil, ErrRevoked
	}
	if tok.IsExpired(r.now()) {
		return nil, ErrExpired
	}
	// Best-effort touch; a failure here does not invalidate the auth decision.
	_ = q.TouchAccessTokenLastUsed(ctx, oauthstoredb.TouchAccessTokenLastUsedParams{
		ID:         tok.ID,
		LastUsedAt: tstz(r.now()),
	})
	return tok, nil
}

func (r *postgresRepo) RotateRefreshToken(ctx context.Context, plaintext string, verify func(*RefreshToken) error, rotate RotationRequest) (*IssuedGrant, error) {
	hash, err := HashPlaintext(RefreshTokenPrefix, plaintext)
	if err != nil {
		return nil, err
	}

	// reused is carried OUT of the transaction on purpose. pgx.BeginFunc
	// ROLLS BACK whenever the callback returns a non-nil error — so returning
	// ErrRefreshReused from inside would undo the very lineage revocation the
	// callback just performed, and the caller would observe an un-revoked
	// lineage behind a reuse error. Instead the reuse path returns NIL so the
	// revocation COMMITS, and the verdict is mapped to ErrRefreshReused after
	// the transaction closes. One transaction (rather than a second standalone
	// one) keeps the FOR UPDATE lock and the sweep atomic.
	//
	// Mandated counterfactual (c) reintroduces the rollback shape;
	// TestRotateRefreshToken_ReuseRevokesLineageAfterReturn goes red on its
	// post-call raw-SQL revoked_at assertions while the RETURNED error is
	// unchanged — which is why an error-identity-only test would not catch it.
	var reused bool
	var grant *IssuedGrant

	txErr := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := oauthstoredb.New(tx)

		// STEP 1 — resolve the LINEAGE, and nothing else. This read is
		// deliberately unlocked and no decision is taken from it: its only
		// output is authorization_code_id, an immutable column. Classifying off
		// this snapshot would be reading state we do not yet hold the lock for.
		lineageRow, err := q.GetRefreshTokenByHash(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("oauthstore: resolve refresh token lineage: %w", err)
		}

		// STEP 2 — take the LINEAGE LOCK before any decision-bearing read. This
		// is what makes the whole-lineage revocation guarantee hold under
		// concurrency: a rotation and a reuse sweep of the same lineage can no
		// longer interleave such that the sweep commits while a successor pair
		// it never saw is still being minted. Pinned by
		// TestRotateRefreshToken_ReuseSweepSerializesOnLineage.
		if err := lockLineage(ctx, q, lineageRow.AuthorizationCodeID); err != nil {
			return err
		}

		// STEP 3 — the AUTHORITATIVE read, taken under the lineage lock. Under
		// READ COMMITTED this statement's snapshot postdates the commit of any
		// rotation or sweep we queued behind, so it observes their consumed_at /
		// revoked_at rather than the stale values step 1 may have held.
		row, err := q.LockRefreshTokenByHash(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("oauthstore: lock refresh token: %w", err)
		}
		presented := rowToRefreshToken(row)

		// CLASSIFICATION ORDER IS A STATED DESIGN DECISION: revoked → reused →
		// expired (see Repository.RotateRefreshToken and the README).
		//
		// Revoked is first so a token already swept by a prior reuse detection
		// reports ErrRevoked rather than replaying the sweep.
		if presented.IsRevoked() {
			return ErrRevoked
		}

		if presented.IsConsumed() {
			// RFC 6749 §4.1.2 reuse: revoke the WHOLE lineage, keyed on the
			// PRESENTED token's authorization_code_id (NOT NULL by schema).
			//
			// Classified BEFORE expiry ON PURPOSE. A replayed rotated token is
			// precisely the compromise signal the sweep exists for, and the
			// PRESENTED token's own expiry says nothing about its SUCCESSORS —
			// they carry later expiries and would stay live. Ordering expiry
			// first would drop the signal exactly when a stolen token is
			// replayed late. Pinned by
			// TestRotateRefreshToken_ExpiredReuseStillSweepsLineage.
			if _, err := r.revokeLineage(ctx, q, presented.AuthorizationCodeID); err != nil {
				return err
			}
			reused = true
			return nil // COMMIT the revocation; signal outside.
		}

		// Expired is returned as an error: there is nothing to commit.
		if presented.IsExpired(r.now()) {
			return ErrExpired
		}

		// The verify seam runs ONLY on a live token, and only after every
		// classification above — the reuse sweep is a security response and
		// must not be gated on the caller's callback. It is the seam #2391 needs
		// to enforce the RFC 6749 §6 client binding, which derivation cannot
		// give it (see Repository.RotateRefreshToken).
		if verify != nil {
			if err := verify(presented); err != nil {
				// Returned UNCHANGED so BeginFunc rolls back: a rejected
				// rotation must not consume the legitimate client's token.
				return err
			}
		}

		// Lineage AND authority are both DERIVED, never caller-supplied (#2433
		// approval condition 1 and its implement-review follow-up): the
		// successor inherits the presented token's authorization_code_id, so it
		// can never escape RevokeGrantsForCode, and its subject/client/audience/
		// scopes/provider/account, so it can never carry authority the presented
		// token did not already hold.
		issued, err := mintPair(ctx, q, presented.AuthorizationCodeID, tokenAuthority(presented, rotate))
		if err != nil {
			return err
		}
		if _, err := q.ConsumeRefreshToken(ctx, oauthstoredb.ConsumeRefreshTokenParams{
			ID:           presented.ID,
			ConsumedAt:   tstz(r.now()),
			ReplacedByID: &issued.Refresh.ID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// UNREACHABLE by construction: the lineage lock plus this row's
				// own FOR UPDATE serialize every rotation of this lineage, and
				// step 3 read consumed_at as NULL under both, so no other
				// transaction can have consumed the row in between.
				//
				// It is deliberately NOT ErrRefreshReused. This return rolls the
				// transaction back, so the sentinel would reach the caller with
				// NO committed lineage revocation — contradicting the
				// commit-then-signal guarantee documented on
				// Repository.RotateRefreshToken. An invariant violation is an
				// internal error, not a reuse verdict.
				return fmt.Errorf("oauthstore: consume refresh token: conditional update matched no row while holding the lineage lock: %w", err)
			}
			return fmt.Errorf("oauthstore: consume refresh token: %w", err)
		}
		grant = issued
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	// Checked FIRST: the revocation is already committed at this point.
	if reused {
		return nil, ErrRefreshReused
	}
	return grant, nil
}

// grantAuthority is the six authority columns a minted pair carries, plus its
// expiries. It is UNEXPORTED on purpose: it is the store's INTERNAL carrier
// between a derivation helper and mintPair, never a caller input. Both public
// mint paths build it from a row this store already owns — the consumed
// authorization code (codeAuthority) or the presented refresh token
// (tokenAuthority) — so neither RedemptionRequest nor RotationRequest can
// express an authority field, and there is no wrong value to validate.
type grantAuthority struct {
	Subject            string
	ClientID           string
	Audience           string
	Scopes             []string
	Provider           string
	AccountID          string
	AccessTokenExpiry  time.Time
	RefreshTokenExpiry time.Time
}

// codeAuthority builds a redemption's authority ENTIRELY from the
// authorization-code row just consumed under the lock, taking only the expiries
// from the caller.
//
// Audience comes from the code's RFC 8707 `resource` indicator — the audience
// the code was issued FOR, recorded at /authorize. That is the one non-identity
// projection; every other field is copied straight across. A code with no
// resource yields an empty audience (the column is NOT NULL but unconstrained on
// emptiness), which is the honest answer: the store will not invent an audience
// the authorization request never named.
func codeAuthority(code *AuthorizationCode, redeem RedemptionRequest) grantAuthority {
	return grantAuthority{
		Subject:            code.Subject,
		ClientID:           code.ClientID,
		Audience:           code.Resource,
		Scopes:             code.Scopes,
		Provider:           code.Provider,
		AccountID:          code.AccountID,
		AccessTokenExpiry:  redeem.AccessTokenExpiry,
		RefreshTokenExpiry: redeem.RefreshTokenExpiry,
	}
}

// tokenAuthority builds a rotation's authority ENTIRELY from the presented
// refresh token's own row, taking only the expiries from the caller. Every
// authority field is therefore inherited rather than asserted, which is what
// makes cross-client / cross-scope / cross-tenant issuance inexpressible at the
// rotation API. The empty AccountID of an untenanted token round-trips back to a
// NULL account_id through parseAccountID.
func tokenAuthority(presented *RefreshToken, rotate RotationRequest) grantAuthority {
	return grantAuthority{
		Subject:            presented.Subject,
		ClientID:           presented.ClientID,
		Audience:           presented.Audience,
		Scopes:             presented.Scopes,
		Provider:           presented.Provider,
		AccountID:          presented.AccountID,
		AccessTokenExpiry:  rotate.AccessTokenExpiry,
		RefreshTokenExpiry: rotate.RefreshTokenExpiry,
	}
}

// mintPair inserts the access/refresh token pair for one grant, stamping
// codeID as the lineage on BOTH rows. Both codeID and mint are always derived
// by the caller from a row this store owns — never from a caller's request.
func mintPair(ctx context.Context, q *oauthstoredb.Queries, codeID uuid.UUID, mint grantAuthority) (*IssuedGrant, error) {
	// Unreachable in practice now that both authority sources are database rows
	// (a valid uuid or NULL round-tripping through accountIDString), but kept
	// rather than dropped: a future mint path must fail loud here rather than
	// silently drop a tenant.
	accountID, err := parseAccountID(mint.AccountID)
	if err != nil {
		return nil, err
	}

	accessPlain, err := GeneratePlaintext(AccessTokenPrefix)
	if err != nil {
		return nil, err
	}
	accessHash, err := HashPlaintext(AccessTokenPrefix, accessPlain)
	if err != nil {
		return nil, err
	}
	accessRow, err := q.CreateAccessToken(ctx, oauthstoredb.CreateAccessTokenParams{
		ID:                  uuid.New(),
		TokenHash:           accessHash,
		Subject:             mint.Subject,
		ClientID:            mint.ClientID,
		Audience:            mint.Audience,
		Scopes:              emptyIfNil(mint.Scopes),
		Provider:            defaultProvider(mint.Provider),
		AccountID:           accountID,
		AuthorizationCodeID: codeID,
		ExpiresAt:           tstz(mint.AccessTokenExpiry),
	})
	if err != nil {
		return nil, fmt.Errorf("oauthstore: create access token: %w", err)
	}

	refreshPlain, err := GeneratePlaintext(RefreshTokenPrefix)
	if err != nil {
		return nil, err
	}
	refreshHash, err := HashPlaintext(RefreshTokenPrefix, refreshPlain)
	if err != nil {
		return nil, err
	}
	refreshRow, err := q.CreateRefreshToken(ctx, oauthstoredb.CreateRefreshTokenParams{
		ID:                  uuid.New(),
		TokenHash:           refreshHash,
		Subject:             mint.Subject,
		ClientID:            mint.ClientID,
		Audience:            mint.Audience,
		Scopes:              emptyIfNil(mint.Scopes),
		Provider:            defaultProvider(mint.Provider),
		AccountID:           accountID,
		AuthorizationCodeID: codeID,
		AccessTokenID:       accessRow.ID,
		ExpiresAt:           tstz(mint.RefreshTokenExpiry),
	})
	if err != nil {
		return nil, fmt.Errorf("oauthstore: create refresh token: %w", err)
	}

	access := rowToAccessToken(accessRow)
	access.PlainText = accessPlain
	refresh := rowToRefreshToken(refreshRow)
	refresh.PlainText = refreshPlain
	return &IssuedGrant{Access: access, Refresh: refresh}, nil
}

func (r *postgresRepo) RevokeAccessToken(ctx context.Context, id uuid.UUID) (*AccessToken, error) {
	q := oauthstoredb.New(r.pool)
	row, err := q.RevokeAccessToken(ctx, oauthstoredb.RevokeAccessTokenParams{
		ID:        id,
		RevokedAt: tstz(r.now()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oauthstore: revoke access token: %w", err)
	}
	return rowToAccessToken(row), nil
}

func (r *postgresRepo) RevokeRefreshToken(ctx context.Context, id uuid.UUID) (*RefreshToken, error) {
	q := oauthstoredb.New(r.pool)
	row, err := q.RevokeRefreshToken(ctx, oauthstoredb.RevokeRefreshTokenParams{
		ID:        id,
		RevokedAt: tstz(r.now()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oauthstore: revoke refresh token: %w", err)
	}
	return rowToRefreshToken(row), nil
}

// --- row mappers -----------------------------------------------------------
//
// Every mapper leaves PlainText EMPTY. Only the mint paths above set it, on
// the value they just generated — which is what makes the plaintext
// non-re-derivable by construction rather than by convention.

func rowToClient(r oauthstoredb.OauthClient) *Client {
	return &Client{
		ID:                      r.ID,
		Provider:                r.Provider,
		ClientID:                r.ClientID,
		RedirectURIs:            r.RedirectUris,
		GrantTypes:              r.GrantTypes,
		ResponseTypes:           r.ResponseTypes,
		TokenEndpointAuthMethod: r.TokenEndpointAuthMethod,
		ClientName:              derefString(r.ClientName),
		ClientURI:               derefString(r.ClientUri),
		LogoURI:                 derefString(r.LogoUri),
		Scope:                   derefString(r.Scope),
		AccountID:               accountIDString(r.AccountID),
		FirstSeenAt:             r.FirstSeenAt.Time,
		UpdatedAt:               r.UpdatedAt.Time,
	}
}

func rowToCode(r oauthstoredb.OauthAuthorizationCode) *AuthorizationCode {
	return &AuthorizationCode{
		ID:                  r.ID,
		ClientID:            r.ClientID,
		RedirectURI:         r.RedirectUri,
		CodeChallenge:       r.CodeChallenge,
		CodeChallengeMethod: oauthas.CodeChallengeMethod(r.CodeChallengeMethod),
		Scopes:              r.Scopes,
		Resource:            derefString(r.Resource),
		Subject:             r.Subject,
		Provider:            r.Provider,
		AccountID:           accountIDString(r.AccountID),
		IssuedAt:            r.IssuedAt.Time,
		ExpiresAt:           r.ExpiresAt.Time,
		ConsumedAt:          timePtr(r.ConsumedAt),
	}
}

func rowToAccessToken(r oauthstoredb.OauthAccessToken) *AccessToken {
	return &AccessToken{
		ID:                  r.ID,
		Subject:             r.Subject,
		ClientID:            r.ClientID,
		Audience:            r.Audience,
		Scopes:              r.Scopes,
		Provider:            r.Provider,
		AccountID:           accountIDString(r.AccountID),
		AuthorizationCodeID: r.AuthorizationCodeID,
		IssuedAt:            r.IssuedAt.Time,
		ExpiresAt:           r.ExpiresAt.Time,
		LastUsedAt:          timePtr(r.LastUsedAt),
		RevokedAt:           timePtr(r.RevokedAt),
	}
}

func rowToRefreshToken(r oauthstoredb.OauthRefreshToken) *RefreshToken {
	return &RefreshToken{
		ID:                  r.ID,
		Subject:             r.Subject,
		ClientID:            r.ClientID,
		Audience:            r.Audience,
		Scopes:              r.Scopes,
		Provider:            r.Provider,
		AccountID:           accountIDString(r.AccountID),
		AuthorizationCodeID: r.AuthorizationCodeID,
		AccessTokenID:       r.AccessTokenID,
		ReplacedByID:        r.ReplacedByID,
		IssuedAt:            r.IssuedAt.Time,
		ExpiresAt:           r.ExpiresAt.Time,
		LastUsedAt:          timePtr(r.LastUsedAt),
		ConsumedAt:          timePtr(r.ConsumedAt),
		RevokedAt:           timePtr(r.RevokedAt),
	}
}

// --- small helpers ---------------------------------------------------------

func tstz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func emptyIfNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// defaultProvider mirrors the column DEFAULT so an unset Provider persists as
// 'github' rather than failing the provider CHECK constraint on an empty
// string.
func defaultProvider(p string) string {
	if p == "" {
		return "github"
	}
	return p
}

// parseAccountID maps the untenanted empty string to a NULL account_id and
// rejects a malformed uuid rather than silently dropping the tenant.
func parseAccountID(s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("oauthstore: invalid account_id %q: %w", s, err)
	}
	return &id, nil
}

func accountIDString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// Compile-time check: the production repo satisfies Repository.
var _ Repository = (*postgresRepo)(nil)
