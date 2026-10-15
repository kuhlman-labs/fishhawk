package specialistjob

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// Audit categories (registered in backend/internal/audit/categories.go).
const (
	// CategoryRequested: a seat filed a job. Payload {job_id, kind,
	// requester, subject, inputs}.
	CategoryRequested = "specialist_job_requested"
	// CategoryCompleted: a job's typed result. Payload {job_id, kind,
	// requester (from the job row), subject, verdict, reason, evidence,
	// charged_to, duration_ms, requested_sequence, provenance
	// (bearer|runner_attested), blocking_eligible, attested_by (omitted
	// unless runner_attested)}.
	CategoryCompleted = "specialist_job_completed"
	// CategoryTakenOver: a running job whose heartbeat went stale past
	// concurrency.JobHeartbeatTTL was re-admitted under a new execution nonce
	// (approval condition C1).
	CategoryTakenOver = "specialist_job_taken_over"
)

// State is a specialist_jobs.state value.
type State string

// Job states.
const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateCompleted State = "completed"
)

// Store refusals. Each is returned wrapped; assert with errors.Is.
var (
	// ErrInvalidParams: a call the store refuses before any SQL runs.
	ErrInvalidParams = errors.New("specialistjob: invalid store parameters")
	// ErrNotFound: no job with that id in the caller's account. A
	// foreign-account job reads as not found.
	ErrNotFound = errors.New("specialistjob: job not found")
	// ErrSubjectRunNotFound: the subject run does not exist in the job's
	// account, so its chain must not carry the job's entries.
	ErrSubjectRunNotFound = errors.New("specialistjob: subject run not found in the job's account")
	// ErrNotAdmissible: a draft job or a completed job asked for admission.
	ErrNotAdmissible = errors.New("specialistjob: job is not admissible (draft or completed)")
	// ErrAlreadyRunning: a live running job asked for admission under a
	// different nonce.
	ErrAlreadyRunning = errors.New("specialistjob: job is already running under another nonce")
	// ErrNotRunning: a completion for a job that is queued or completed.
	ErrNotRunning = errors.New("specialistjob: job is not running")
	// ErrNonceMismatch: a completion whose execution nonce is not the
	// running job's.
	ErrNonceMismatch = errors.New("specialistjob: execution nonce does not match the running job")
)

// Actor is the authenticated identity performing a store call.
type Actor struct {
	Kind    audit.ActorKind
	Subject string
}

func (a Actor) ptrs() (*audit.ActorKind, *string) {
	var k *audit.ActorKind
	var s *string
	if a.Kind != "" {
		kind := a.Kind
		k = &kind
	}
	if a.Subject != "" {
		sub := a.Subject
		s = &sub
	}
	return k, s
}

// Job is one specialist_jobs row. ExecutionNonce is a capability: a read
// surface must never render it (approval condition C1). The attestation
// private key is never stored, so no Job field can carry it.
type Job struct {
	ID                uuid.UUID
	AccountID         *uuid.UUID
	Kind              Kind
	State             State
	Requester         Requester
	Subject           Subject
	Inputs            json.RawMessage
	AdmissionGroup    string
	Host              string
	ExecutionNonce    string
	Verdict           Verdict
	Result            json.RawMessage
	RequestedSequence int64
	CompletedSequence *int64
	RequestedAt       time.Time
	LastSeenAt        time.Time
	StartedAt         *time.Time
	CompletedAt       *time.Time
	// Provenance is set only on a completed job: runner_attested or bearer
	// (a pre-0104 completion, provenance NULL, reads as bearer).
	Provenance string
	// AttestationPublicKey is the job's live-or-expired attestation public
	// key (nil when none was minted, or once a completion consumed it).
	AttestationPublicKey ed25519.PublicKey
	// AttestationSubject is the minting attester's token subject.
	AttestationSubject   string
	AttestationExpiresAt *time.Time
	// AttestationLive is attestation_expires_at > clock_timestamp(), decided
	// on the DATABASE clock by the reading statement.
	AttestationLive bool
}

// Store is the pgx-backed specialist-job store over specialist_jobs (0103).
// Account scoping is explicit (account_id IS NOT DISTINCT FROM the caller's
// account), so it holds under the superuser runtime role that bypasses RLS.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewStore returns the specialist-job store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

const jobColumns = `id, account_id, kind, state, requester_seat, requester_actor_kind, requester_subject,
  subject_run_id, subject_campaign_id, COALESCE(subject_head_sha, ''), COALESCE(subject_repo, ''),
  inputs, COALESCE(admission_group, ''), host, COALESCE(execution_nonce, ''), COALESCE(verdict, ''), result,
  requested_sequence, completed_sequence, requested_at, last_seen_at, started_at, completed_at,
  CASE WHEN state = 'completed' THEN COALESCE(provenance, 'bearer') ELSE '' END,
  attestation_public_key, COALESCE(attestation_subject, ''), attestation_expires_at,
  COALESCE(attestation_expires_at > clock_timestamp(), false)`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	var acct, runID, campaignID pgtype.UUID
	var kind, state, seat, verdict string
	var inputs, result, pubKey []byte
	if err := row.Scan(&j.ID, &acct, &kind, &state, &seat, &j.Requester.ActorKind, &j.Requester.Subject,
		&runID, &campaignID, &j.Subject.HeadSHA, &j.Subject.Repo,
		&inputs, &j.AdmissionGroup, &j.Host, &j.ExecutionNonce, &verdict, &result,
		&j.RequestedSequence, &j.CompletedSequence, &j.RequestedAt, &j.LastSeenAt, &j.StartedAt, &j.CompletedAt,
		&j.Provenance, &pubKey, &j.AttestationSubject, &j.AttestationExpiresAt, &j.AttestationLive); err != nil {
		return nil, err
	}
	if acct.Valid {
		id := uuid.UUID(acct.Bytes)
		j.AccountID = &id
	}
	if runID.Valid {
		j.Subject.RunID = uuid.UUID(runID.Bytes).String()
	}
	if campaignID.Valid {
		j.Subject.CampaignID = uuid.UUID(campaignID.Bytes).String()
	}
	j.Kind, j.State, j.Requester.Seat, j.Verdict = Kind(kind), State(state), Seat(seat), Verdict(verdict)
	j.Inputs, j.Result = inputs, result
	if len(pubKey) > 0 {
		j.AttestationPublicKey = ed25519.PublicKey(pubKey)
	}
	return &j, nil
}

func accountString(acct *uuid.UUID) string {
	if acct == nil {
		return ""
	}
	return acct.String()
}

func pgUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: %q is not a uuid", ErrInvalidParams, s)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CreateParams files one runner-executed job.
type CreateParams struct {
	// AccountID is the caller identity's account (nil = untenanted).
	AccountID *uuid.UUID
	// Request is the ParseRequest result. Request.Inputs is recorded
	// verbatim (as semantic JSON) on the audit chain.
	Request *Request
	// Requester is SERVER-DERIVED from the authenticated identity, never
	// read from the request body.
	Requester Requester
}

// CreateCompletedParams files and completes one draft job.
type CreateCompletedParams struct {
	AccountID *uuid.UUID
	Request   *Request
	Requester Requester
	// Text is the RenderDraft output.
	Text string
}

func validateCreate(req *Request, requester Requester) error {
	switch {
	case req == nil:
		return fmt.Errorf("%w: request is required", ErrInvalidParams)
	case len(req.Inputs) == 0:
		return fmt.Errorf("%w: request inputs are required", ErrInvalidParams)
	case requester.Seat != SeatOperator && requester.Seat != SeatFirstOfficer:
		return fmt.Errorf("%w: requester seat %q", ErrInvalidParams, requester.Seat)
	case requester.ActorKind == "" || requester.Subject == "":
		return fmt.Errorf("%w: requester actor kind and subject are required", ErrInvalidParams)
	}
	return nil
}

type requestedPayload struct {
	JobID     string          `json:"job_id"`
	Kind      Kind            `json:"kind"`
	Requester Requester       `json:"requester"`
	Subject   Subject         `json:"subject"`
	Inputs    json.RawMessage `json:"inputs"`
}

type completedPayload struct {
	JobID             string          `json:"job_id"`
	Kind              Kind            `json:"kind"`
	Requester         Requester       `json:"requester"`
	Subject           Subject         `json:"subject"`
	Verdict           Verdict         `json:"verdict"`
	Reason            string          `json:"reason,omitempty"`
	Evidence          json.RawMessage `json:"evidence,omitempty"`
	ChargedTo         ChargedTo       `json:"charged_to"`
	DurationMS        int64           `json:"duration_ms"`
	RequestedSequence int64           `json:"requested_sequence"`
	Provenance        string          `json:"provenance"`
	BlockingEligible  bool            `json:"blocking_eligible"`
	AttestedBy        string          `json:"attested_by,omitempty"`
}

type takenOverPayload struct {
	JobID              string    `json:"job_id"`
	Kind               Kind      `json:"kind"`
	AdmissionGroup     string    `json:"admission_group"`
	Host               string    `json:"host"`
	PreviousHost       string    `json:"previous_host"`
	PreviousLastSeenAt time.Time `json:"previous_last_seen_at"`
}

// Create inserts a queued runner-executed job and appends
// specialist_job_requested in ONE transaction: on the subject run's chain when
// subject.run_id is set (the run must be in the job's account), else on the
// account's global chain. A draft is refused (use CreateCompleted).
func (s *Store) Create(ctx context.Context, p CreateParams) (*Job, error) {
	if err := validateCreate(p.Request, p.Requester); err != nil {
		return nil, err
	}
	if !p.Request.Kind.RunnerExecuted() {
		return nil, fmt.Errorf("%w: kind %q is rendered in-process (CreateCompleted)", ErrInvalidParams, p.Request.Kind)
	}
	var job *Job
	err := postgres.WithTenant(ctx, s.pool, accountString(p.AccountID), func(tx pgx.Tx) error {
		var err error
		job, _, err = s.createTx(ctx, tx, p.AccountID, p.Request, p.Requester, StateQueued)
		return err
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// CreateCompleted inserts a completed draft job and appends
// specialist_job_requested then specialist_job_completed in ONE transaction.
// The verdict is pass and the evidence is {text}. Only a draft is accepted.
func (s *Store) CreateCompleted(ctx context.Context, p CreateCompletedParams) (*Job, error) {
	if err := validateCreate(p.Request, p.Requester); err != nil {
		return nil, err
	}
	if p.Request.Kind != KindDraft {
		return nil, fmt.Errorf("%w: kind %q is runner-executed (Create)", ErrInvalidParams, p.Request.Kind)
	}
	evidence, err := json.Marshal(DraftEvidence{Text: p.Text})
	if err != nil {
		return nil, fmt.Errorf("specialistjob: marshal draft evidence: %w", err)
	}
	completion := Completion{
		SchemaVersion: SchemaVersion, Kind: KindDraft, Verdict: VerdictPass, Evidence: evidence,
	}
	var job *Job
	err = postgres.WithTenant(ctx, s.pool, accountString(p.AccountID), func(tx pgx.Tx) error {
		created, _, err := s.createTx(ctx, tx, p.AccountID, p.Request, p.Requester, StateQueued)
		if err != nil {
			return err
		}
		requester := Actor{Kind: audit.ActorKind(p.Requester.ActorKind), Subject: p.Requester.Subject}
		job, err = s.completeTx(ctx, tx, created, &completion, requester, ProvenanceBearer, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Store) createTx(ctx context.Context, tx pgx.Tx, acct *uuid.UUID, req *Request, requester Requester, state State) (*Job, *audit.Entry, error) {
	runID, err := pgUUID(req.Subject.RunID)
	if err != nil {
		return nil, nil, err
	}
	campaignID, err := pgUUID(req.Subject.CampaignID)
	if err != nil {
		return nil, nil, err
	}
	id := uuid.New()
	payload, err := json.Marshal(requestedPayload{
		JobID: id.String(), Kind: req.Kind, Requester: requester, Subject: req.Subject, Inputs: req.Inputs,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("specialistjob: marshal requested payload: %w", err)
	}
	actor := Actor{Kind: audit.ActorKind(requester.ActorKind), Subject: requester.Subject}
	entry, err := s.appendEntry(ctx, tx, acct, req.Subject, CategoryRequested, actor, payload)
	if err != nil {
		return nil, nil, err
	}
	job, err := scanJob(tx.QueryRow(ctx, `INSERT INTO specialist_jobs
  (id, account_id, kind, state, requester_seat, requester_actor_kind, requester_subject,
   subject_run_id, subject_campaign_id, subject_head_sha, subject_repo, inputs, requested_sequence)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING `+jobColumns,
		id, acct, string(req.Kind), string(state), string(requester.Seat), requester.ActorKind, requester.Subject,
		runID, campaignID, nullText(req.Subject.HeadSHA), nullText(req.Subject.Repo), []byte(req.Inputs), entry.Sequence))
	if err != nil {
		return nil, nil, fmt.Errorf("specialistjob: insert job: %w", err)
	}
	return job, entry, nil
}

// appendEntry routes one chain append: the subject run's chain when
// subject.run_id is set (the run must exist in the job's account), else the
// account's global chain.
func (s *Store) appendEntry(ctx context.Context, tx pgx.Tx, acct *uuid.UUID, subject Subject, category string, actor Actor, payload json.RawMessage) (*audit.Entry, error) {
	kind, sub := actor.ptrs()
	if subject.RunID == "" {
		e, err := audit.AppendGlobalChainedTx(ctx, tx, audit.GlobalChainAppendParams{
			Timestamp: s.now(), Category: category, ActorKind: kind, ActorSubject: sub, Payload: payload, AccountID: acct,
		})
		if err != nil {
			return nil, fmt.Errorf("specialistjob: append %s: %w", category, err)
		}
		return e, nil
	}
	runID, err := uuid.Parse(subject.RunID)
	if err != nil {
		return nil, fmt.Errorf("%w: subject run_id %q is not a uuid", ErrInvalidParams, subject.RunID)
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1 AND account_id IS NOT DISTINCT FROM $2)`,
		runID, acct).Scan(&found); err != nil {
		return nil, fmt.Errorf("specialistjob: read subject run: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("specialistjob: run %s: %w", runID, ErrSubjectRunNotFound)
	}
	e, err := audit.AppendChainedTx(ctx, tx, audit.ChainAppendParams{
		RunID: runID, Timestamp: s.now(), Category: category, ActorKind: kind, ActorSubject: sub, Payload: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("specialistjob: append %s: %w", category, err)
	}
	return e, nil
}

// Get reads one job in the caller's account. A foreign-account or unknown
// job is ErrNotFound.
func (s *Store) Get(ctx context.Context, accountID *uuid.UUID, jobID uuid.UUID) (*Job, error) {
	job, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+`
FROM specialist_jobs WHERE id = $1 AND account_id IS NOT DISTINCT FROM $2`, jobID, accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("specialistjob: job %s: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("specialistjob: get job: %w", err)
	}
	return job, nil
}

// lockJob reads one job FOR UPDATE in the caller's account, with whether a
// running job's heartbeat is stale past concurrency.JobHeartbeatTTL (judged
// on the database clock).
func lockJob(ctx context.Context, tx pgx.Tx, acct *uuid.UUID, jobID uuid.UUID) (*Job, bool, error) {
	var stale bool
	var job *Job
	row := tx.QueryRow(ctx, `SELECT `+jobColumns+`,
  last_seen_at <= clock_timestamp() - make_interval(secs => $3)
FROM specialist_jobs WHERE id = $1 AND account_id IS NOT DISTINCT FROM $2 FOR UPDATE`,
		jobID, acct, concurrency.JobHeartbeatTTL.Seconds())
	job, err := scanJob(scanTail{row: row, extra: &stale})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("specialistjob: job %s: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return nil, false, fmt.Errorf("specialistjob: lock job: %w", err)
	}
	return job, stale, nil
}

// scanTail appends extra scan targets after scanJob's columns.
type scanTail struct {
	row   pgx.Row
	extra *bool
}

func (s scanTail) Scan(dest ...any) error { return s.row.Scan(append(dest, s.extra)...) }

// AdmitParams asks to admit (or heartbeat) one runner-executed job.
type AdmitParams struct {
	AccountID *uuid.UUID
	JobID     uuid.UUID
	// Host is the runner's host label; the job is admitted through
	// concurrency.VerifyGroupKey(Host).
	Host string
	// Nonce is the runner's execution nonce: an admission records it, a
	// re-POST with the same nonce is a heartbeat.
	Nonce string
	// Actor is the admitting identity (the takeover entry's actor).
	Actor Actor
	// Attester is the caller's token subject when it is an attester (an
	// explicit API token holding AttestScope); empty means not an attester.
	// An admission that moves the job to running mints a key for a non-empty
	// Attester and CLEARS any previous key otherwise; a same-nonce heartbeat
	// refreshes the key's expiry only when Attester is the minting subject.
	Attester string
}

// Admission is AdmitJob's verdict.
type Admission struct {
	// Admitted: the job is running under ExecutionNonce (a fresh admission,
	// a same-nonce heartbeat, or a stale takeover).
	Admitted       bool
	ExecutionNonce string
	// Position is the 1-based standing when queued (0 when admitted): the
	// fresh queued jobs ahead plus the fresh verify-stage waiters, plus one.
	Position int
	// Contended: the group lock was held by another admission, so this call
	// queued without deciding.
	Contended bool
	// TakenOver: this admission replaced a stale running job's nonce.
	TakenOver bool
	Job       *Job
	// AttestationKey is the job-scoped private key, delivered ONCE: set only
	// by the admission that minted it (never on a heartbeat or a queue
	// answer). The store keeps only the public half.
	AttestationKey       ed25519.PrivateKey
	AttestationExpiresAt *time.Time
}

// AdmitJob admits one runner-executed job through the local-verify:<host>
// concurrency group, never an implement slot. See README.md § "Admission".
//
//   - A draft or completed job is ErrNotAdmissible.
//   - A running job with the SAME nonce is an idempotent heartbeat
//     (last_seen_at refreshed, admitted).
//   - A running job with a DIFFERENT nonce is ErrAlreadyRunning while its
//     heartbeat is fresh; once stale past concurrency.JobHeartbeatTTL it is
//     re-decided like a queued job and, when admitted, TAKEN OVER under the
//     new nonce with a specialist_job_taken_over entry (approval condition C1).
//   - A queued job is admitted only under the group lock, when the group's
//     live stage holders plus live job holders are below
//     concurrency.DefaultVerifyLimit, no fresh verify-stage waiter exists
//     (a job yields to merge-candidate passes), and no fresh queued job of
//     the same group and account is older. Otherwise it stays queued with a
//     refreshed last_seen_at.
//   - An admission that moves the job to running (fresh or takeover) mints
//     an attestation key for a non-empty p.Attester, returned once in
//     Admission.AttestationKey, and clears any previous key otherwise. A
//     same-nonce heartbeat slides the key's expiry by AttestationTTL only
//     when p.Attester is the minting subject.
func (s *Store) AdmitJob(ctx context.Context, p AdmitParams) (*Admission, error) {
	switch {
	case p.JobID == uuid.Nil:
		return nil, fmt.Errorf("%w: job id is required", ErrInvalidParams)
	case p.Nonce == "" || len(p.Nonce) > 128:
		return nil, fmt.Errorf("%w: nonce must be 1..128 bytes", ErrInvalidParams)
	}
	var out *Admission
	err := postgres.WithTenant(ctx, s.pool, accountString(p.AccountID), func(tx pgx.Tx) error {
		var err error
		out, err = s.admitTx(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) admitTx(ctx context.Context, tx pgx.Tx, p AdmitParams) (*Admission, error) {
	job, stale, err := lockJob(ctx, tx, p.AccountID, p.JobID)
	if err != nil {
		return nil, err
	}
	if job.Kind == KindDraft || job.State == StateCompleted {
		return nil, fmt.Errorf("specialistjob: job %s (%s, %s): %w", job.ID, job.Kind, job.State, ErrNotAdmissible)
	}
	takeover := false
	if job.State == StateRunning {
		if job.ExecutionNonce == p.Nonce {
			// A heartbeat slides the key's expiry only for the minting
			// attester, and never re-delivers the key.
			job, err = scanJob(tx.QueryRow(ctx, `UPDATE specialist_jobs SET last_seen_at = clock_timestamp(),
  attestation_expires_at = CASE
    WHEN $2 <> '' AND attestation_subject = $2 AND attestation_public_key IS NOT NULL
      THEN clock_timestamp() + make_interval(secs => $3)
    ELSE attestation_expires_at END
WHERE id = $1 RETURNING `+jobColumns, job.ID, p.Attester, AttestationTTL.Seconds()))
			if err != nil {
				return nil, fmt.Errorf("specialistjob: heartbeat: %w", err)
			}
			return &Admission{Admitted: true, ExecutionNonce: p.Nonce, Job: job}, nil
		}
		if !stale {
			return nil, fmt.Errorf("specialistjob: job %s: %w", job.ID, ErrAlreadyRunning)
		}
		takeover = true
	}

	group := concurrency.VerifyGroupKey(p.Host)
	hit, err := concurrency.TryGroupLockTx(ctx, tx, p.AccountID, group)
	if err != nil {
		return nil, err
	}
	occ, err := concurrency.GroupOccupancyTx(ctx, tx, p.AccountID, group)
	if err != nil {
		return nil, err
	}
	var jobsAhead int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM specialist_jobs
WHERE admission_group = $1 AND state = 'queued' AND id <> $2
  AND account_id IS NOT DISTINCT FROM $3
  AND last_seen_at > clock_timestamp() - make_interval(secs => $4)
  AND (requested_at, id) < ($5, $2)`,
		group, job.ID, p.AccountID, concurrency.QueueTTL.Seconds(), job.RequestedAt).Scan(&jobsAhead); err != nil {
		return nil, fmt.Errorf("specialistjob: count jobs ahead: %w", err)
	}
	admit := hit &&
		occ.StageHolders+occ.JobHolders < concurrency.DefaultVerifyLimit &&
		occ.FreshStageWaiters == 0 &&
		jobsAhead == 0
	if !admit {
		out := &Admission{Position: jobsAhead + occ.FreshStageWaiters + 1, Contended: !hit, Job: job}
		if takeover {
			// A stale running row is left untouched: refreshing its
			// last_seen_at would make it count as a live holder again.
			return out, nil
		}
		out.Job, err = scanJob(tx.QueryRow(ctx, `UPDATE specialist_jobs
SET admission_group = $2, host = $3, last_seen_at = clock_timestamp()
WHERE id = $1 RETURNING `+jobColumns, job.ID, group, p.Host))
		if err != nil {
			return nil, fmt.Errorf("specialistjob: refresh queued job: %w", err)
		}
		return out, nil
	}

	// An attester gets a fresh key; any other admitter clears the columns, so
	// a takeover always invalidates the previous runner's key.
	var pub []byte
	var priv ed25519.PrivateKey
	if p.Attester != "" {
		var pk ed25519.PublicKey
		if pk, priv, err = MintAttestationKey(); err != nil {
			return nil, err
		}
		pub = pk
	}
	previous := job
	job, err = scanJob(tx.QueryRow(ctx, `UPDATE specialist_jobs
SET state = 'running', admission_group = $2, host = $3, execution_nonce = $4,
  started_at = clock_timestamp(), last_seen_at = clock_timestamp(),
  attestation_public_key = $5, attestation_subject = $6,
  attestation_expires_at = CASE WHEN $5::bytea IS NULL THEN NULL
    ELSE clock_timestamp() + make_interval(secs => $7) END
WHERE id = $1 RETURNING `+jobColumns, job.ID, group, p.Host, p.Nonce, pub, nullText(p.Attester), AttestationTTL.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("specialistjob: admit job: %w", err)
	}
	if takeover {
		payload, err := json.Marshal(takenOverPayload{
			JobID: job.ID.String(), Kind: job.Kind, AdmissionGroup: group, Host: p.Host,
			PreviousHost: previous.Host, PreviousLastSeenAt: previous.LastSeenAt.UTC(),
		})
		if err != nil {
			return nil, fmt.Errorf("specialistjob: marshal takeover payload: %w", err)
		}
		if _, err := s.appendEntry(ctx, tx, p.AccountID, job.Subject, CategoryTakenOver, p.Actor, payload); err != nil {
			return nil, err
		}
	}
	// Without an attester both are nil: priv was never minted and the UPDATE
	// set attestation_expires_at NULL.
	return &Admission{Admitted: true, ExecutionNonce: p.Nonce, TakenOver: takeover, Job: job,
		AttestationKey: priv, AttestationExpiresAt: job.AttestationExpiresAt}, nil
}

// CompleteParams ships one running job's completion.
type CompleteParams struct {
	AccountID *uuid.UUID
	JobID     uuid.UUID
	// Completion is the ParseCompletion result.
	Completion *Completion
	// Actor is the completing identity (the completed entry's actor). The
	// payload's requester always comes from the job row, never from Actor.
	Actor Actor
	// Attestation is the result's signature, or nil for an unsigned (bearer)
	// result.
	Attestation *ResultAttestation
}

// ResultAttestation is a signed result as posted.
type ResultAttestation struct {
	// Signature is the decoded Ed25519 signature over ResultMessage.
	Signature []byte
	// Body is the EXACT request body Completion was parsed from.
	Body []byte
	// CallerSubject is the posting identity's token subject; it must be the
	// key's minting subject.
	CallerSubject string
}

// Complete records a running job's typed result: it appends
// specialist_job_completed and sets the row completed in ONE transaction. A
// queued or completed job is ErrNotRunning, a different execution nonce is
// ErrNonceMismatch, a signed result the job's stored attestation does not
// verify is ErrAttestationInvalid (an *AttestationError, nothing written),
// and a completion kind that is not the job's is ErrKindMismatch. A verified
// result records provenance runner_attested and consumes the key; an
// unsigned one records bearer. It touches only specialist_jobs and the audit
// chain.
func (s *Store) Complete(ctx context.Context, p CompleteParams) (*Job, error) {
	switch {
	case p.JobID == uuid.Nil:
		return nil, fmt.Errorf("%w: job id is required", ErrInvalidParams)
	case p.Completion == nil:
		return nil, fmt.Errorf("%w: completion is required", ErrInvalidParams)
	}
	var job *Job
	err := postgres.WithTenant(ctx, s.pool, accountString(p.AccountID), func(tx pgx.Tx) error {
		locked, _, err := lockJob(ctx, tx, p.AccountID, p.JobID)
		if err != nil {
			return err
		}
		if locked.State != StateRunning {
			return fmt.Errorf("specialistjob: job %s is %s: %w", locked.ID, locked.State, ErrNotRunning)
		}
		if p.Completion.ExecutionNonce != locked.ExecutionNonce {
			return fmt.Errorf("specialistjob: job %s: %w", locked.ID, ErrNonceMismatch)
		}
		provenance := ProvenanceBearer
		attestedBy := ""
		if a := p.Attestation; a != nil {
			stored := StoredAttestation{PublicKey: locked.AttestationPublicKey, Subject: locked.AttestationSubject, Live: locked.AttestationLive}
			if err := VerifyResultAttestation(stored, a.CallerSubject, locked.ID, a.Body, a.Signature); err != nil {
				return fmt.Errorf("specialistjob: job %s: %w", locked.ID, err)
			}
			provenance, attestedBy = ProvenanceRunnerAttested, a.CallerSubject
		}
		job, err = s.completeTx(ctx, tx, locked, p.Completion, p.Actor, provenance, attestedBy)
		return err
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// completeTx composes the result from the job ROW (requester, subject,
// charge) and the completion (verdict, reason, evidence, duration), validates
// it against $defs.result, appends specialist_job_completed and sets the row
// completed with provenance, consuming any attestation key.
func (s *Store) completeTx(ctx context.Context, tx pgx.Tx, job *Job, c *Completion, actor Actor, provenance, attestedBy string) (*Job, error) {
	if c.Kind != job.Kind {
		return nil, fmt.Errorf("specialistjob: job %s is %s, completion is %s: %w", job.ID, job.Kind, c.Kind, ErrKindMismatch)
	}
	charged := ChargedTo{Seat: job.Requester.Seat, Subject: job.Requester.Subject}
	result, err := json.Marshal(Result{
		JobID: job.ID.String(), Kind: job.Kind, Requester: job.Requester, Subject: job.Subject,
		Verdict: c.Verdict, Reason: c.Reason, Evidence: c.Evidence, ChargedTo: charged, DurationMS: c.DurationMS,
	})
	if err != nil {
		return nil, fmt.Errorf("specialistjob: marshal result: %w", err)
	}
	if err := ValidateResult(result); err != nil {
		return nil, fmt.Errorf("specialistjob: composed result is off-contract: %w", err)
	}
	payload, err := json.Marshal(completedPayload{
		JobID: job.ID.String(), Kind: job.Kind, Requester: job.Requester, Subject: job.Subject,
		Verdict: c.Verdict, Reason: c.Reason, Evidence: c.Evidence, ChargedTo: charged,
		DurationMS: c.DurationMS, RequestedSequence: job.RequestedSequence,
		Provenance: provenance, BlockingEligible: BlockingEligible(provenance), AttestedBy: attestedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("specialistjob: marshal completed payload: %w", err)
	}
	entry, err := s.appendEntry(ctx, tx, job.AccountID, job.Subject, CategoryCompleted, actor, payload)
	if err != nil {
		return nil, err
	}
	out, err := scanJob(tx.QueryRow(ctx, `UPDATE specialist_jobs
SET state = 'completed', verdict = $2, result = $3, completed_sequence = $4, completed_at = clock_timestamp(),
  provenance = $5, attestation_public_key = NULL, attestation_expires_at = NULL
WHERE id = $1 RETURNING `+jobColumns, job.ID, string(c.Verdict), result, entry.Sequence, provenance))
	if err != nil {
		return nil, fmt.Errorf("specialistjob: complete job: %w", err)
	}
	return out, nil
}

// listCompletedLimit bounds ListCompletedForRun.
const listCompletedLimit = 100

// ListCompletedForRun returns the completed jobs whose subject is runID in
// the caller's account, ordered by completed_sequence (the run chain's
// order), at most 100.
func (s *Store) ListCompletedForRun(ctx context.Context, acct *uuid.UUID, runID uuid.UUID) ([]*Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobColumns+`
FROM specialist_jobs
WHERE subject_run_id = $1 AND account_id IS NOT DISTINCT FROM $2 AND state = 'completed'
ORDER BY completed_sequence, id LIMIT $3`, runID, acct, listCompletedLimit)
	if err != nil {
		return nil, fmt.Errorf("specialistjob: list completed jobs: %w", err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("specialistjob: scan completed job: %w", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("specialistjob: list completed jobs: %w", err)
	}
	return out, nil
}
