// Package alerttrigger is the alert-ingress half of ADR-053 option A
// (E35.4 / #1601): an HMAC-authenticated POST /v0/triggers/alert turns a
// verified alert into a conventions-complete incident issue, deduplicated by
// alert fingerprint, with an optional per-source auto-start of a hotfix run
// that ships OFF.
//
// This file is the dedup ledger over the alert_incidents table (migration
// 0099). The first alert for a (source, repo, fingerprint) CLAIMS the row and
// files the issue; Complete records the filed issue; a repeat alert finds the
// filed row and comments on it instead of filing again. Dedup lives in
// Postgres, not in forge search, because a forge search index is eventually
// consistent: a fast re-fire could otherwise double-file. The primary key
// makes the row the single writer across fishhawkd instances.
//
// The store is raw pgx (no sqlc), the backend/internal/concurrency
// precedent. Every time comparison uses the DATABASE clock (now()), never a
// Go time.Now seed, so the stale-reclaim cutoff carries no cross-clock
// dependency (#3048). The package contract, including the wire format and
// the response-code table, is README.md.
package alerttrigger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Key identifies one incident: an alert source, the repository its incident
// issues are filed in, and the sender's fingerprint for the alert.
type Key struct {
	SourceID    string
	Repo        string
	Fingerprint string
}

func (k Key) validate() error {
	if k.SourceID == "" || k.Repo == "" || k.Fingerprint == "" {
		return fmt.Errorf("alerttrigger: incomplete key (source_id=%q repo=%q fingerprint=%q): all three are required", k.SourceID, k.Repo, k.Fingerprint)
	}
	return nil
}

// ClaimKind is what a Claim decided for one alert.
type ClaimKind string

const (
	// ClaimNew means the caller holds the claim (Claim.Token) and MUST file the
	// incident issue, then Complete — or Release on a filing failure so the
	// next alert can file. Answered for the first alert of a fingerprint and
	// for the stale reclaim of a claim whose filer never completed.
	ClaimNew ClaimKind = "new"
	// ClaimExisting means an issue is already filed (Claim.IssueNumber /
	// Claim.IssueURL); the caller comments on it instead of filing.
	ClaimExisting ClaimKind = "existing"
	// ClaimInFlight means another caller holds a live (not yet stale) claim and is
	// filing now. Nothing is filed and nothing is commented; the occurrence
	// is still counted.
	ClaimInFlight ClaimKind = "in_flight"
)

// Claim is the outcome of Store.Claim.
type Claim struct {
	Kind ClaimKind
	// Token is the claim identity a ClaimNew caller passes to Complete or
	// Release. uuid.Nil for ClaimExisting and ClaimInFlight.
	Token uuid.UUID
	// IssueNumber and IssueURL name the filed issue; set only for
	// ClaimExisting.
	IssueNumber int
	IssueURL    string
	// Occurrences is the row's occurrence count AFTER this alert was counted
	// (1 for the first alert of a fingerprint).
	Occurrences int
}

// ErrClaimLost is returned by Complete and Release when the row no longer
// carries the caller's claim: another caller stale-reclaimed it (a new
// token), it was already filed, or it was released. A Complete that answers
// it recorded NOTHING, so a late original filer can never overwrite the
// newer claimant's row.
var ErrClaimLost = errors.New("alerttrigger: incident claim lost (reclaimed, filed or released by another caller)")

// ErrIncidentNotFiled is returned by RecordRun when no FILED row exists for
// the key — a run can only be recorded against an incident issue.
var ErrIncidentNotFiled = errors.New("alerttrigger: no filed incident for key")

// Store is the alert_incidents dedup ledger.
type Store interface {
	// Claim counts one alert for key and decides whether the caller files
	// (ClaimNew), comments (ClaimExisting) or does nothing (ClaimInFlight).
	// A claim whose filer has not completed within staleAfter (measured on
	// the database clock) is reclaimed with a fresh token. staleAfter must
	// be positive.
	Claim(ctx context.Context, key Key, staleAfter time.Duration) (Claim, error)
	// Complete records the filed issue on the row ONLY while it is still an
	// unfiled claim holding token; otherwise ErrClaimLost and no write.
	Complete(ctx context.Context, key Key, token uuid.UUID, issueNumber int, issueURL string) error
	// Release deletes the row ONLY while it is still an unfiled claim
	// holding token (a filing failed), so the next alert files afresh;
	// otherwise ErrClaimLost and no write.
	Release(ctx context.Context, key Key, token uuid.UUID) error
	// RecordRun stamps the auto-started run on a FILED row; ErrIncidentNotFiled
	// when there is none.
	RecordRun(ctx context.Context, key Key, runID uuid.UUID) error
}

// PostgresStore is the pgx-backed Store over alert_incidents (0099). One
// Claim is ONE transaction on ONE pooled connection: the insert-or-lock
// sequence below decides under a row lock, so concurrent alerts for one key
// yield exactly one ClaimNew.
type PostgresStore struct {
	pool *pgxpool.Pool
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore returns the Postgres incident store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// claimAttempts bounds the insert-or-lock loop inside one Claim transaction.
// A second pass is needed only when a concurrent Release deletes the row
// between the conflicting INSERT and the locking SELECT; a third is slack.
const claimAttempts = 3

const (
	claimInsertSQL = `INSERT INTO alert_incidents (source_id, repo, fingerprint, claim_token)
VALUES ($1, $2, $3, $4)
ON CONFLICT (source_id, repo, fingerprint) DO NOTHING
RETURNING occurrences`

	// claimLockSQL locks the existing row and evaluates staleness on the
	// DATABASE clock in the same statement (#3048).
	claimLockSQL = `SELECT issue_number, issue_url, (claimed_at < now() - make_interval(secs => $4)) AS stale
FROM alert_incidents
WHERE source_id = $1 AND repo = $2 AND fingerprint = $3
FOR UPDATE`

	claimCountSQL = `UPDATE alert_incidents
SET occurrences = occurrences + 1, last_seen_at = now()
WHERE source_id = $1 AND repo = $2 AND fingerprint = $3
RETURNING occurrences`

	claimReclaimSQL = `UPDATE alert_incidents
SET claim_token = $4, claimed_at = now(), occurrences = occurrences + 1, last_seen_at = now()
WHERE source_id = $1 AND repo = $2 AND fingerprint = $3
RETURNING occurrences`

	completeSQL = `UPDATE alert_incidents
SET issue_number = $5, issue_url = $6, last_seen_at = now()
WHERE source_id = $1 AND repo = $2 AND fingerprint = $3
  AND claim_token = $4 AND issue_number IS NULL`

	releaseSQL = `DELETE FROM alert_incidents
WHERE source_id = $1 AND repo = $2 AND fingerprint = $3
  AND claim_token = $4 AND issue_number IS NULL`

	recordRunSQL = `UPDATE alert_incidents
SET run_id = $4
WHERE source_id = $1 AND repo = $2 AND fingerprint = $3
  AND issue_number IS NOT NULL`
)

// Claim implements Store.
func (s *PostgresStore) Claim(ctx context.Context, key Key, staleAfter time.Duration) (Claim, error) {
	if err := key.validate(); err != nil {
		return Claim{}, err
	}
	if staleAfter <= 0 {
		return Claim{}, fmt.Errorf("alerttrigger: Claim staleAfter must be positive, got %s", staleAfter)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Claim{}, fmt.Errorf("alerttrigger: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out, err := claimInTx(ctx, tx, key, staleAfter)
	if err != nil {
		return Claim{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, fmt.Errorf("alerttrigger: commit claim: %w", err)
	}
	return out, nil
}

func claimInTx(ctx context.Context, tx pgx.Tx, key Key, staleAfter time.Duration) (Claim, error) {
	for attempt := 0; attempt < claimAttempts; attempt++ {
		// INSERT first: under READ COMMITTED a conflicting insert from an
		// uncommitted transaction makes this statement WAIT for it, so the
		// loser sees the winner's committed row below rather than racing it.
		token := uuid.New()
		var occurrences int
		err := tx.QueryRow(ctx, claimInsertSQL, key.SourceID, key.Repo, key.Fingerprint, token).Scan(&occurrences)
		if err == nil {
			return Claim{Kind: ClaimNew, Token: token, Occurrences: occurrences}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Claim{}, fmt.Errorf("alerttrigger: claim insert: %w", err)
		}

		var (
			issueNumber *int
			issueURL    *string
			stale       bool
		)
		err = tx.QueryRow(ctx, claimLockSQL, key.SourceID, key.Repo, key.Fingerprint, staleAfter.Seconds()).
			Scan(&issueNumber, &issueURL, &stale)
		if errors.Is(err, pgx.ErrNoRows) {
			// Released between the INSERT and the lock: insert again.
			continue
		}
		if err != nil {
			return Claim{}, fmt.Errorf("alerttrigger: claim lock: %w", err)
		}

		switch {
		case issueNumber != nil:
			if err := tx.QueryRow(ctx, claimCountSQL, key.SourceID, key.Repo, key.Fingerprint).Scan(&occurrences); err != nil {
				return Claim{}, fmt.Errorf("alerttrigger: count occurrence: %w", err)
			}
			url := ""
			if issueURL != nil {
				url = *issueURL
			}
			return Claim{Kind: ClaimExisting, IssueNumber: *issueNumber, IssueURL: url, Occurrences: occurrences}, nil
		case stale:
			// The previous filer never completed within staleAfter: take the
			// claim over with a fresh token, so its late Complete is refused.
			if err := tx.QueryRow(ctx, claimReclaimSQL, key.SourceID, key.Repo, key.Fingerprint, token).Scan(&occurrences); err != nil {
				return Claim{}, fmt.Errorf("alerttrigger: reclaim stale claim: %w", err)
			}
			return Claim{Kind: ClaimNew, Token: token, Occurrences: occurrences}, nil
		default:
			if err := tx.QueryRow(ctx, claimCountSQL, key.SourceID, key.Repo, key.Fingerprint).Scan(&occurrences); err != nil {
				return Claim{}, fmt.Errorf("alerttrigger: count occurrence: %w", err)
			}
			return Claim{Kind: ClaimInFlight, Occurrences: occurrences}, nil
		}
	}
	return Claim{}, fmt.Errorf("alerttrigger: claim for %s/%s/%s did not settle after %d attempts (row repeatedly released mid-claim)", key.SourceID, key.Repo, key.Fingerprint, claimAttempts)
}

// Complete implements Store.
func (s *PostgresStore) Complete(ctx context.Context, key Key, token uuid.UUID, issueNumber int, issueURL string) error {
	if err := key.validate(); err != nil {
		return err
	}
	if issueNumber <= 0 || issueURL == "" {
		return fmt.Errorf("alerttrigger: Complete needs a positive issue number and a non-empty URL, got %d %q", issueNumber, issueURL)
	}
	tag, err := s.pool.Exec(ctx, completeSQL, key.SourceID, key.Repo, key.Fingerprint, token, issueNumber, issueURL)
	if err != nil {
		return fmt.Errorf("alerttrigger: complete claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// Release implements Store.
func (s *PostgresStore) Release(ctx context.Context, key Key, token uuid.UUID) error {
	if err := key.validate(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, releaseSQL, key.SourceID, key.Repo, key.Fingerprint, token)
	if err != nil {
		return fmt.Errorf("alerttrigger: release claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// RecordRun implements Store.
func (s *PostgresStore) RecordRun(ctx context.Context, key Key, runID uuid.UUID) error {
	if err := key.validate(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, recordRunSQL, key.SourceID, key.Repo, key.Fingerprint, runID)
	if err != nil {
		return fmt.Errorf("alerttrigger: record run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFiled
	}
	return nil
}
