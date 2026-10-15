package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
)

// Specialist jobs (ADR-097 D2/D6, E85.9 / #4300): the four /v0/specialist-jobs
// routes. They are NOT run-scoped routes — a job's subject may name a run, a
// campaign or only a head SHA — so each handler does its own account scoping
// through the store (account_id IS NOT DISTINCT FROM the caller's account).
// The result handler writes ONLY specialist_jobs and the audit chain, never a
// run or stage row. Result attestation (E85.15 / #4363): an admission by an
// attester (attesterFor) mints a job-scoped key, and a result signed with it
// records provenance runner_attested; every other result records bearer.
// Long-form contract: backend/internal/specialistjob/README.md.

// SpecialistJobStore is the store surface the handlers use;
// *specialistjob.Store satisfies it. An interface so a handler test can inject
// a failing store for the 500 arms.
type SpecialistJobStore interface {
	Create(ctx context.Context, p specialistjob.CreateParams) (*specialistjob.Job, error)
	CreateCompleted(ctx context.Context, p specialistjob.CreateCompletedParams) (*specialistjob.Job, error)
	Get(ctx context.Context, accountID *uuid.UUID, jobID uuid.UUID) (*specialistjob.Job, error)
	AdmitJob(ctx context.Context, p specialistjob.AdmitParams) (*specialistjob.Admission, error)
	Complete(ctx context.Context, p specialistjob.CompleteParams) (*specialistjob.Job, error)
	// ListCompletedForRun feeds the gate view's display-only specialist_jobs
	// block (E85.15 / #4363).
	ListCompletedForRun(ctx context.Context, accountID *uuid.UUID, runID uuid.UUID) ([]*specialistjob.Job, error)
}

const (
	// specialistJobRequestMaxBytes caps POST /v0/specialist-jobs. The schema
	// bounds every member, so a larger body is padding or abuse.
	specialistJobRequestMaxBytes = 64 << 10
	// specialistJobResultMaxBytes caps POST .../result (inventory evidence
	// alone may carry 2000 lines of up to 1000 chars before truncation).
	specialistJobResultMaxBytes = 256 << 10
	// specialistJobAdmissionMaxBytes caps POST .../admission ({host, nonce}).
	specialistJobAdmissionMaxBytes = 4 << 10
	// specialistJobHostMaxLen bounds the admission host label.
	specialistJobHostMaxLen = 253
)

// specialistJobResponse is the job read shape (GET, and the POST/admission/
// result bodies). It NEVER carries execution_nonce (approval condition C1):
// the nonce is the running runner's capability to ship the result.
//
// CROSS-MODULE WIRE CONTRACT: the runner decodes this body as
// upload.SpecialistJob (runner/internal/upload/upload.go); the pair is pinned
// by backend/internal/wirecontract's manifest and the shared golden
// testdata/wire/specialist_job.json.
type specialistJobResponse struct {
	JobID             string                  `json:"job_id"`
	Kind              string                  `json:"kind"`
	State             string                  `json:"state"`
	Requester         specialistjob.Requester `json:"requester"`
	Subject           specialistjob.Subject   `json:"subject"`
	Inputs            json.RawMessage         `json:"inputs"`
	AdmissionGroup    string                  `json:"admission_group,omitempty"`
	Host              string                  `json:"host,omitempty"`
	Verdict           string                  `json:"verdict,omitempty"`
	Result            json.RawMessage         `json:"result,omitempty"`
	RequestedSequence int64                   `json:"requested_sequence"`
	CompletedSequence *int64                  `json:"completed_sequence,omitempty"`
	RequestedAt       time.Time               `json:"requested_at"`
	LastSeenAt        time.Time               `json:"last_seen_at"`
	StartedAt         *time.Time              `json:"started_at,omitempty"`
	CompletedAt       *time.Time              `json:"completed_at,omitempty"`
	// Provenance is set only on a completed job: runner_attested (the result
	// verified against the job's attestation key) or bearer (everything else,
	// including a pre-0104 completion).
	Provenance string `json:"provenance,omitempty"`
	// BlockingEligible is specialistjob.BlockingEligible(Provenance): whether
	// the result may count as ADR-089 D4 blocking-grade evidence. False for
	// every job that is not a runner-attested completion.
	BlockingEligible bool `json:"blocking_eligible"`
	// Stale is derived: a queued job unseen past concurrency.QueueTTL (its
	// runner stopped polling admission) or a running job unseen past
	// concurrency.JobHeartbeatTTL (its runner stopped heartbeating). Judged
	// on this server's clock against the DB-stamped last_seen_at, so it is
	// an advisory read hint, never an admission input.
	Stale bool `json:"stale"`
}

// specialistJobAdmissionRequest is the POST .../admission body.
//
// CROSS-MODULE WIRE CONTRACT: the runner encodes it as
// upload.SpecialistJobAdmissionRequest (runner/internal/upload/upload.go).
type specialistJobAdmissionRequest struct {
	Host  string `json:"host"`
	Nonce string `json:"nonce"`
}

// specialistJobAdmissionResponse is the POST .../admission answer.
// execution_nonce is the caller's own nonce, echoed only when admitted.
// attestation_key (the base64 std encoding of the job-scoped Ed25519 private
// key) and attestation_expires_at are set ONLY by the admission that minted
// the key — an attester's admission that moved the job to running — and never
// on a heartbeat, a queue answer or a non-attester's admission. The server
// keeps only the public half.
//
// CROSS-MODULE WIRE CONTRACT: the runner decodes it as
// upload.SpecialistJobAdmission (runner/internal/upload/upload.go).
type specialistJobAdmissionResponse struct {
	Admitted       bool                  `json:"admitted"`
	ExecutionNonce string                `json:"execution_nonce,omitempty"`
	Position       int                   `json:"position"`
	Contended      bool                  `json:"contended"`
	TakenOver      bool                  `json:"taken_over"`
	Job            specialistJobResponse `json:"job"`
	// AttestationKey is delivered once; see the type comment.
	AttestationKey       string     `json:"attestation_key,omitempty"`
	AttestationExpiresAt *time.Time `json:"attestation_expires_at,omitempty"`
}

func toSpecialistJobResponse(j *specialistjob.Job, now time.Time) specialistJobResponse {
	out := specialistJobResponse{
		JobID: j.ID.String(), Kind: string(j.Kind), State: string(j.State),
		Requester: j.Requester, Subject: j.Subject, Inputs: j.Inputs,
		AdmissionGroup: j.AdmissionGroup, Host: j.Host, Verdict: string(j.Verdict),
		RequestedSequence: j.RequestedSequence, CompletedSequence: j.CompletedSequence,
		RequestedAt: j.RequestedAt, LastSeenAt: j.LastSeenAt, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt,
		Provenance: j.Provenance, BlockingEligible: specialistjob.BlockingEligible(j.Provenance),
	}
	if len(j.Result) > 0 {
		out.Result = j.Result
	}
	switch j.State {
	case specialistjob.StateQueued:
		out.Stale = now.Sub(j.LastSeenAt) > concurrency.QueueTTL
	case specialistjob.StateRunning:
		out.Stale = now.Sub(j.LastSeenAt) > concurrency.JobHeartbeatTTL
	}
	return out
}

// specialistJobsConfigured answers 503 specialist_jobs_unconfigured when no
// store is wired (the documented partial rollback).
func (s *Server) specialistJobsConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.SpecialistJobs == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "specialist_jobs_unconfigured",
			"specialist-job endpoints require a configured specialist-job store", nil)
		return false
	}
	return true
}

// readSpecialistJobBody reads a body bounded at limit, answering 413
// request_too_large above it.
func (s *Server) readSpecialistJobBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, "request_too_large",
				"request body exceeds size cap", map[string]any{"limit_bytes": limit})
			return nil, false
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return nil, false
	}
	return body, true
}

func (s *Server) specialistJobID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("job_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"job_id must be a valid UUID", map[string]any{"field": "job_id", "got": r.PathValue("job_id")})
		return uuid.Nil, false
	}
	return id, true
}

// specialistJobParseDetails renders a ParseRequest/ParseCompletion refusal.
func specialistJobParseDetails(err error) map[string]any {
	var se *specialistjob.SchemaError
	if errors.As(err, &se) {
		return map[string]any{"path": se.Path, "error": se.Message}
	}
	d := map[string]any{"error": err.Error()}
	if errors.Is(err, specialistjob.ErrKindMismatch) {
		d["reason"] = "kind_mismatch"
	}
	return d
}

// specialistJobActor is the authenticated identity as an audit actor: the
// kind comes from the token subject (an operator-agent token is agent,
// approval condition C4), never from the request. An attester (a runner
// credential, attesterFor) acts as system under its token subject.
func specialistJobActor(id Identity) specialistjob.Actor {
	if attesterFor(id) != "" {
		return specialistjob.Actor{Kind: audit.ActorSystem, Subject: id.Subject}
	}
	return specialistjob.Actor{Kind: actorKindForSubject(id.Subject), Subject: id.Subject}
}

// attesterFor returns the caller's token subject when it is a specialist-job
// ATTESTER, "" otherwise. An attester is an explicit API token (TokenID set:
// a cookie session carries no scope list and bypasses scope checks, so it can
// never be one) holding specialistjob.AttestScope whose subject is not a
// run-bound mcp:run: token. Only an attester's admission mints an
// attestation key, and only an attester may post a signed result.
func attesterFor(id Identity) string {
	if id.TokenID == "" || !hasScope(id, specialistjob.AttestScope) || strings.HasPrefix(id.Subject, "mcp:run:") {
		return ""
	}
	return id.Subject
}

// requireSpecialistJobScope is requireWriteScope for the three routes a
// runner credential uses (GET, admission, result): it accepts routeScope OR
// specialistjob.AttestScope. It preserves requireWriteScope's posture exactly
// otherwise — anonymous is 401, a cookie session (TokenID "") bypasses the
// scope check, and a token holding neither scope is 403 insufficient_scope.
func (s *Server) requireSpecialistJobScope(w http.ResponseWriter, r *http.Request, routeScope string) bool {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return false
	}
	if id.TokenID != "" && !hasScope(id, routeScope) && !hasScope(id, specialistjob.AttestScope) {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: "+routeScope+" (or "+specialistjob.AttestScope+")",
			map[string]any{"required_scope": routeScope, "accepted_scopes": []string{routeScope, specialistjob.AttestScope}})
		return false
	}
	return true
}

// writeSpecialistJobAttestationRefusal answers 401
// specialist_job_attestation_invalid with details.reason.
func (s *Server) writeSpecialistJobAttestationRefusal(w http.ResponseWriter, r *http.Request, reason string) {
	s.writeError(w, r, http.StatusUnauthorized, "specialist_job_attestation_invalid",
		"the result's attestation was refused: "+reason, map[string]any{"reason": reason})
}

// specialistJobResultAttestation reads the result's signature header. Absent
// is (nil, true): an unsigned result, recorded as bearer. A header that is
// repeated or not hex is refused signature_malformed, and a signature from a
// caller that is not an attester is refused caller_not_attester — both 401
// before any store call. A present-but-empty (or wrong-length) signature
// reaches the store, which refuses it signature_malformed.
func (s *Server) specialistJobResultAttestation(w http.ResponseWriter, r *http.Request, body []byte) (*specialistjob.ResultAttestation, bool) {
	vals := r.Header.Values(specialistjob.ResultSignatureHeader)
	if len(vals) == 0 {
		return nil, true
	}
	sig, err := hex.DecodeString(vals[0])
	if len(vals) != 1 || err != nil {
		s.writeSpecialistJobAttestationRefusal(w, r, specialistjob.ReasonSignatureMalformed)
		return nil, false
	}
	attester := attesterFor(IdentityFrom(r.Context()))
	if attester == "" {
		s.writeSpecialistJobAttestationRefusal(w, r, specialistjob.ReasonCallerNotAttester)
		return nil, false
	}
	return &specialistjob.ResultAttestation{Signature: sig, Body: body, CallerSubject: attester}, true
}

// writeSpecialistJobStoreError maps a store refusal onto the HTTP surface.
func (s *Server) writeSpecialistJobStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, specialistjob.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "specialist_job_not_found", "no specialist job with that id", nil)
	case errors.Is(err, specialistjob.ErrSubjectRunNotFound):
		s.writeError(w, r, http.StatusNotFound, "run_not_found", "the subject run does not exist in your account", nil)
	case errors.Is(err, specialistjob.ErrNotAdmissible):
		s.writeError(w, r, http.StatusConflict, "specialist_job_not_admissible",
			"a draft or completed job cannot be admitted", nil)
	case errors.Is(err, specialistjob.ErrAlreadyRunning):
		s.writeError(w, r, http.StatusConflict, "specialist_job_already_running",
			"the job is running under another execution nonce with a live heartbeat", nil)
	case errors.Is(err, specialistjob.ErrNotRunning):
		s.writeError(w, r, http.StatusConflict, "specialist_job_not_running",
			"the job is not running (queued, or already completed)", nil)
	case errors.Is(err, specialistjob.ErrNonceMismatch):
		s.writeError(w, r, http.StatusConflict, "specialist_job_nonce_mismatch",
			"the execution nonce is not the running job's", nil)
	case errors.Is(err, specialistjob.ErrAttestationInvalid):
		reason := ""
		var ae *specialistjob.AttestationError
		if errors.As(err, &ae) {
			reason = ae.Reason
		}
		s.writeSpecialistJobAttestationRefusal(w, r, reason)
	case errors.Is(err, specialistjob.ErrKindMismatch), errors.Is(err, specialistjob.ErrInvalidParams):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(), specialistJobParseDetails(err))
	default:
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"specialist-job store call failed", map[string]any{"error": err.Error()})
	}
}

// handleRequestSpecialistJob implements POST /v0/specialist-jobs.
//
// Auth: write:stages, from a SEAT identity. A run-bound (mcp:run:<uuid>)
// token is refused specialist_job_requester_not_a_seat even when it carries
// write:stages: a job is requested by the operator (or, after E85.6, the
// first officer), never by an agent inside a run. The requester is derived
// from the authenticated identity and is NEVER read from the body (the
// closed request schema refuses a requester member).
func (s *Server) handleRequestSpecialistJob(w http.ResponseWriter, r *http.Request) {
	if !s.specialistJobsConfigured(w, r) {
		return
	}
	if !s.requireWriteScope(w, r, "write:stages") {
		return
	}
	id := IdentityFrom(r.Context())
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		s.writeError(w, r, http.StatusForbidden, "specialist_job_requester_not_a_seat",
			"a run-bound token cannot request a specialist job; request it from an operator identity", nil)
		return
	}
	acct, ok := s.callerAccountUUID(w, r)
	if !ok {
		return
	}
	body, ok := s.readSpecialistJobBody(w, r, specialistJobRequestMaxBytes)
	if !ok {
		return
	}
	req, err := specialistjob.ParseRequest(body)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"specialist-job request is invalid", specialistJobParseDetails(err))
		return
	}
	if req.Subject.CampaignID != "" && !s.specialistJobCampaignVisible(w, r, req.Subject.CampaignID, id.AccountID) {
		return
	}
	requester := specialistjob.Requester{
		Seat: specialistjob.SeatOperator, ActorKind: string(actorKindForSubject(id.Subject)), Subject: id.Subject,
	}
	var job *specialistjob.Job
	if req.Kind == specialistjob.KindDraft {
		text, rerr := specialistjob.RenderDraft(*req.Draft)
		if rerr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"draft could not be rendered", map[string]any{"error": rerr.Error()})
			return
		}
		job, err = s.cfg.SpecialistJobs.CreateCompleted(r.Context(), specialistjob.CreateCompletedParams{
			AccountID: acct, Request: req, Requester: requester, Text: text,
		})
	} else {
		job, err = s.cfg.SpecialistJobs.Create(r.Context(), specialistjob.CreateParams{
			AccountID: acct, Request: req, Requester: requester,
		})
	}
	if err != nil {
		s.writeSpecialistJobStoreError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, toSpecialistJobResponse(job, time.Now()))
}

// specialistJobCampaignVisible requires the subject campaign to exist in the
// caller's account (an untenanted campaign matches only an untenanted caller);
// anything else is 404 campaign_not_found, so a foreign campaign's existence
// is not disclosed.
func (s *Server) specialistJobCampaignVisible(w http.ResponseWriter, r *http.Request, raw, callerAcct string) bool {
	notFound := func() bool {
		s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
			"no campaign with that id in your account", map[string]any{"campaign_id": raw})
		return false
	}
	cid, err := uuid.Parse(raw)
	if err != nil || s.cfg.CampaignRepo == nil {
		return notFound()
	}
	acct, err := s.cfg.CampaignRepo.GetCampaignAccountID(r.Context(), cid)
	if errors.Is(err, campaign.ErrNotFound) {
		return notFound()
	}
	if err != nil {
		s.writeDBUnavailable(w, r)
		return false
	}
	if acct != callerAcct {
		return notFound()
	}
	return true
}

// handleGetSpecialistJob implements GET /v0/specialist-jobs/{job_id}: read:runs
// (or attest:specialist-jobs), the job's own account only (a foreign job is
// specialist_job_not_found).
func (s *Server) handleGetSpecialistJob(w http.ResponseWriter, r *http.Request) {
	if !s.specialistJobsConfigured(w, r) {
		return
	}
	if !s.requireSpecialistJobScope(w, r, "read:runs") {
		return
	}
	jobID, ok := s.specialistJobID(w, r)
	if !ok {
		return
	}
	acct, ok := s.callerAccountUUID(w, r)
	if !ok {
		return
	}
	job, err := s.cfg.SpecialistJobs.Get(r.Context(), acct, jobID)
	if err != nil {
		s.writeSpecialistJobStoreError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toSpecialistJobResponse(job, time.Now()))
}

// handleAdmitSpecialistJob implements POST /v0/specialist-jobs/{job_id}/admission:
// admit (or heartbeat, or take over a stale running job) through the
// local-verify:<host> concurrency group. write:stages (or
// attest:specialist-jobs). An attester's admission that moves the job to
// running carries the job-scoped attestation key, once.
func (s *Server) handleAdmitSpecialistJob(w http.ResponseWriter, r *http.Request) {
	if !s.specialistJobsConfigured(w, r) {
		return
	}
	if !s.requireSpecialistJobScope(w, r, "write:stages") {
		return
	}
	jobID, ok := s.specialistJobID(w, r)
	if !ok {
		return
	}
	acct, ok := s.callerAccountUUID(w, r)
	if !ok {
		return
	}
	body, ok := s.readSpecialistJobBody(w, r, specialistJobAdmissionMaxBytes)
	if !ok {
		return
	}
	var in specialistJobAdmissionRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"admission body must be {host, nonce}", map[string]any{"error": err.Error()})
		return
	}
	if in.Host == "" || len(in.Host) > specialistJobHostMaxLen || in.Nonce == "" || len(in.Nonce) > 128 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"host must be 1..253 bytes and nonce 1..128 bytes", nil)
		return
	}
	id := IdentityFrom(r.Context())
	adm, err := s.cfg.SpecialistJobs.AdmitJob(r.Context(), specialistjob.AdmitParams{
		AccountID: acct, JobID: jobID, Host: in.Host, Nonce: in.Nonce,
		Actor: specialistJobActor(id), Attester: attesterFor(id),
	})
	if err != nil {
		s.writeSpecialistJobStoreError(w, r, err)
		return
	}
	out := specialistJobAdmissionResponse{
		Admitted: adm.Admitted, Position: adm.Position, Contended: adm.Contended, TakenOver: adm.TakenOver,
		Job: toSpecialistJobResponse(adm.Job, time.Now()),
	}
	if adm.Admitted {
		out.ExecutionNonce = adm.ExecutionNonce
		if len(adm.AttestationKey) > 0 {
			out.AttestationKey = base64.StdEncoding.EncodeToString(adm.AttestationKey)
			out.AttestationExpiresAt = adm.AttestationExpiresAt
		}
	}
	s.writeJSON(w, r, http.StatusOK, out)
}

// handleSpecialistJobResult implements POST /v0/specialist-jobs/{job_id}/result:
// the runner's completion, validated against the STORED job kind, recorded as
// specialist_job_completed with the requester taken from the job row. A
// result carrying X-Fishhawk-Signature from the minting attester that
// verifies over the exact body records provenance runner_attested; an
// unsigned one records bearer; a refused signature is 401
// specialist_job_attestation_invalid with nothing written. Touches only
// specialist_jobs and the audit chain. write:stages (or attest:specialist-jobs).
func (s *Server) handleSpecialistJobResult(w http.ResponseWriter, r *http.Request) {
	if !s.specialistJobsConfigured(w, r) {
		return
	}
	if !s.requireSpecialistJobScope(w, r, "write:stages") {
		return
	}
	jobID, ok := s.specialistJobID(w, r)
	if !ok {
		return
	}
	acct, ok := s.callerAccountUUID(w, r)
	if !ok {
		return
	}
	body, ok := s.readSpecialistJobBody(w, r, specialistJobResultMaxBytes)
	if !ok {
		return
	}
	attestation, ok := s.specialistJobResultAttestation(w, r, body)
	if !ok {
		return
	}
	job, err := s.cfg.SpecialistJobs.Get(r.Context(), acct, jobID)
	if err != nil {
		s.writeSpecialistJobStoreError(w, r, err)
		return
	}
	completion, err := specialistjob.ParseCompletion(body, job.Kind)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"specialist-job completion is invalid", specialistJobParseDetails(err))
		return
	}
	done, err := s.cfg.SpecialistJobs.Complete(r.Context(), specialistjob.CompleteParams{
		AccountID: acct, JobID: jobID, Completion: completion,
		Actor: specialistJobActor(IdentityFrom(r.Context())), Attestation: attestation,
	})
	if err != nil {
		s.writeSpecialistJobStoreError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toSpecialistJobResponse(done, time.Now()))
}
