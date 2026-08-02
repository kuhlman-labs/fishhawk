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

func (r *postgresRepo) RedeemAuthorizationCode(ctx context.Context, plaintext string, verify func(*AuthorizationCode) error, mint MintRequest) (*IssuedGrant, error) {
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

		issued, err := mintPair(ctx, q, consumed.ID, mint)
		if err != nil {
			return err
		}
		issued.Code = rowToCode(consumed)
		grant = issued
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return grant, nil
}

func (r *postgresRepo) RevokeGrantsForCode(ctx context.Context, codeID uuid.UUID) (int64, error) {
	q := oauthstoredb.New(r.pool)
	return r.revokeLineage(ctx, q, codeID)
}

// revokeLineage revokes every still-active access and refresh token descended
// from codeID and returns the total row count. Shared by RevokeGrantsForCode
// and RotateRefreshToken's reuse path so the two can never drift.
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

func (r *postgresRepo) RotateRefreshToken(ctx context.Context, plaintext string, mint MintRequest) (*IssuedGrant, error) {
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
		row, err := q.LockRefreshTokenByHash(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("oauthstore: lock refresh token: %w", err)
		}
		presented := rowToRefreshToken(row)

		// Revoked and expired are returned as errors: there is nothing to
		// commit on those paths. Revoked is classified FIRST so a token swept
		// by a prior reuse detection reports ErrRevoked rather than replaying
		// the sweep.
		if presented.IsRevoked() {
			return ErrRevoked
		}
		if presented.IsExpired(r.now()) {
			return ErrExpired
		}

		if presented.IsConsumed() {
			// RFC 6749 §4.1.2 reuse: revoke the WHOLE lineage, keyed on the
			// PRESENTED token's authorization_code_id (NOT NULL by schema).
			if _, err := r.revokeLineage(ctx, q, presented.AuthorizationCodeID); err != nil {
				return err
			}
			reused = true
			return nil // COMMIT the revocation; signal outside.
		}

		// Lineage is DERIVED, never caller-supplied (#2433 approval condition
		// 1): the successor inherits the presented token's
		// authorization_code_id, so it can never escape RevokeGrantsForCode.
		issued, err := mintPair(ctx, q, presented.AuthorizationCodeID, mint)
		if err != nil {
			return err
		}
		if _, err := q.ConsumeRefreshToken(ctx, oauthstoredb.ConsumeRefreshTokenParams{
			ID:           presented.ID,
			ConsumedAt:   tstz(r.now()),
			ReplacedByID: &issued.Refresh.ID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The conditional predicate rejected the row: a concurrent
				// rotation consumed it first. Roll back this mint.
				return ErrRefreshReused
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

// mintPair inserts the access/refresh token pair for one grant, stamping
// codeID as the lineage on BOTH rows. codeID is always derived by the caller
// from a row this store owns — never from a MintRequest.
func mintPair(ctx context.Context, q *oauthstoredb.Queries, codeID uuid.UUID, mint MintRequest) (*IssuedGrant, error) {
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
