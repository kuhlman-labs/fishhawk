package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// escape.go is the run-scoped escape-record verb, POST
// /v0/runs/{run_id}/escapes (E94.3 / #4694; ADR-117 D3 + Amendment A1,
// RATIFIED, with A1.2 as amended). An escape is a defect that got past the
// live reviews. It is stored as append-only escape_recorded rows on the run's
// audit chain, one row per state change (A1.1): a BIRTH (born counted or
// pending_judgement, A1.2 / A1.4) and at most ONE closure successor naming it
// in supersedes_sequence (A1.2 / A1.3). The long-form contract — the refusal
// ladder, the birth rule, the per-source keys, the closure state machine, the
// reader contract and the residuals — is server/README.md § "Escape records".

// CategoryEscapeRecorded is the audit category of every escape row. It is
// INTERNAL (no issue-comment activity line) and withheld from run-bound audit
// reads (ADR-117 A1.6).
const CategoryEscapeRecorded = "escape_recorded"

// escapeVersion is the payload version every escape row carries.
const escapeVersion = 1

// Escape sources (A1.2's per-source birth keys).
const (
	escapeSourceLaterReviewer   = "later_reviewer"
	escapeSourceFollowUpBug     = "follow_up_bug"
	escapeSourceOperatorBattery = "operator_battery"
	escapeSourceHumanFinding    = "human_finding"
	escapeSourceCIRedOnMain     = "ci_red_on_main"
)

// Escape states and judgement bases.
const (
	escapeStatePendingJudgement = "pending_judgement"
	escapeStateCounted          = "counted"
	escapeStateDismissed        = "dismissed"

	escapeBasisNotRequired         = "not_required"
	escapeBasisAwaitingAttestation = "awaiting_attestation"
	escapeBasisOperatorAttestation = "operator_attestation"

	escapeRecordKindBirth   = "birth"
	escapeRecordKindClosure = "closure"

	escapeSecurityLabel = "security"
)

// Payload keys the two dedupe specs scan.
const (
	// escapeSourceEventKey is the birth dedupe key (A1.2).
	escapeSourceEventKey = "source_event_key"
	// escapeSupersedesKey is the closure dedupe key (A1.2).
	escapeSupersedesKey = "supersedes_sequence"
	// escapeSourceKeyConcernPrefix prefixes a later_reviewer birth key:
	// concern:<concern_uuid>, per #4690 ADR-117 A1.2 as amended by erratum E1
	// (re-transcription PR #4784). It is NEVER the concern's
	// OriginReviewSequence, which every concern of one verdict shares.
	escapeSourceKeyConcernPrefix = "concern:"
	// escapeSourceKeyIdempotencyPrefix prefixes every other source's key.
	escapeSourceKeyIdempotencyPrefix = "idempotency_key:"
)

// Request bounds.
const (
	escapeMaxBodyBytes          = 256 << 10
	escapeMaxDescriptionRunes   = 4000
	escapeMaxNoteRunes          = 2000
	escapeMaxFiles              = 200
	escapeMaxPathBytes          = 1024
	escapeMaxIntroducingCommits = 20
)

var (
	escapeSources = map[string]bool{
		escapeSourceLaterReviewer: true, escapeSourceFollowUpBug: true, escapeSourceOperatorBattery: true,
		escapeSourceHumanFinding: true, escapeSourceCIRedOnMain: true,
	}
	escapeSeverities = map[string]bool{"high": true, "medium": true, "low": true}
	// escapeCategories is CLOSED: a caller cannot invent a category to dodge
	// the security allow-list.
	escapeCategories = map[string]bool{
		"correctness": true, escapeSecurityLabel: true, "reliability": true, "concurrency": true,
		"performance": true, "data_integrity": true, "test_coverage": true, "documentation": true, "other": true,
	}
	escapeJudgements = map[string]string{"defect": escapeStateCounted, "no_defect": escapeStateDismissed}

	escapeShaRE            = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	escapeIdempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	// escapeSecurityClassRE is an interim closed-charset slug with no enum:
	// the vocabulary closes when #4774 defines the class list (#4783).
	escapeSecurityClassRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
	escapeGHSARE          = regexp.MustCompile(`^GHSA(-[0-9a-z]{4}){3}$`)
	escapeOpaqueAdvisory  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:#/-]{0,127}$`)

	// escapeSecurityReviewedHeadKeys is the ONLY reviewed_head projection a
	// security escape carries (approval condition C3): the scalar identity
	// fields. The rendered-diff path sets are dropped.
	escapeSecurityReviewedHeadKeys = []string{"status", "kind", "sha", "tree_sha", "change_id", "origin"}
)

// escapeRecordRequest is a RECORD body. security_finding_id is deliberately
// NOT a field (ADR-126 D7 deferred, R2): strict decoding refuses it.
// Description is a pointer so a security record's PRESENT-but-empty
// description is still refused as free text.
type escapeRecordRequest struct {
	Source             string   `json:"source"`
	Severity           string   `json:"severity"`
	Category           string   `json:"category"`
	Description        *string  `json:"description"`
	Files              []string `json:"files"`
	HeadSHA            string   `json:"head_sha"`
	IntroducingCommits []string `json:"introducing_commits"`
	ConcernID          string   `json:"concern_id"`
	IdempotencyKey     string   `json:"idempotency_key"`
	SecurityClass      string   `json:"security_class"`
	AdvisoryID         string   `json:"advisory_id"`
	Delegated          bool     `json:"delegated"`
}

// escapeCloseRequest is a CLOSE body: any body carrying supersedes_sequence.
type escapeCloseRequest struct {
	SupersedesSequence int64                     `json:"supersedes_sequence"`
	Attestation        *escapeAttestationRequest `json:"attestation"`
	Delegated          bool                      `json:"delegated"`
}

type escapeAttestationRequest struct {
	Judgement string `json:"judgement"`
	Note      string `json:"note"`
}

// escapeSecurity is a security escape's ONLY descriptive content (A1.5).
type escapeSecurity struct {
	Class      string `json:"class"`
	AdvisoryID string `json:"advisory_id"`
}

type escapeSubject struct {
	PullRequestURL    string `json:"pull_request_url,omitempty"`
	PullRequestMerged bool   `json:"pull_request_merged"`
	HeadSHA           string `json:"head_sha,omitempty"`
}

// escapeConcernRef binds a later_reviewer escape to its source concern.
type escapeConcernRef struct {
	ID                   string `json:"id"`
	RunID                string `json:"run_id"`
	StageKind            string `json:"stage_kind"`
	OriginReviewSequence int64  `json:"origin_review_sequence"`
	ReviewRoundSequence  int64  `json:"review_round_sequence,omitempty"`
	ReviewerRole         string `json:"reviewer_role,omitempty"`
	ReviewerModel        string `json:"reviewer_model,omitempty"`
	Provenance           string `json:"provenance,omitempty"`
}

// escapeObject is the immutable escape a birth records and a closure copies.
type escapeObject struct {
	Source                   string            `json:"source"`
	Severity                 string            `json:"severity"`
	Category                 string            `json:"category"`
	Description              string            `json:"description,omitempty"`
	Files                    []string          `json:"files,omitempty"`
	Security                 *escapeSecurity   `json:"security,omitempty"`
	Subject                  escapeSubject     `json:"subject"`
	ReviewedHead             json.RawMessage   `json:"reviewed_head,omitempty"`
	IntroducingCommits       []string          `json:"introducing_commits,omitempty"`
	IntroducingCommitsSource string            `json:"introducing_commits_source,omitempty"`
	Concern                  *escapeConcernRef `json:"concern,omitempty"`
	IdempotencyKey           string            `json:"idempotency_key,omitempty"`
}

type escapeRecorder struct {
	Subject        string `json:"subject"`
	ActorKind      string `json:"actor_kind"`
	CredentialKind string `json:"credential_kind"`
	Delegated      bool   `json:"delegated"`
}

type escapeBirthPayload struct {
	EscapeVersion  int            `json:"escape_version"`
	RecordKind     string         `json:"record_kind"`
	RunID          string         `json:"run_id"`
	State          string         `json:"state"`
	JudgementBasis string         `json:"judgement_basis"`
	PendingReason  string         `json:"pending_reason,omitempty"`
	AgentRecorded  bool           `json:"agent_recorded"`
	Recorder       escapeRecorder `json:"recorder"`
	SourceEventKey string         `json:"source_event_key"`
	Escape         escapeObject   `json:"escape"`
}

type escapeAttestation struct {
	Judgement      string `json:"judgement"`
	Note           string `json:"note,omitempty"`
	JudgementID    string `json:"judgement_id"`
	ActorSubject   string `json:"actor_subject"`
	CredentialKind string `json:"credential_kind"`
}

type escapeClosurePayload struct {
	EscapeVersion      int               `json:"escape_version"`
	RecordKind         string            `json:"record_kind"`
	RunID              string            `json:"run_id"`
	State              string            `json:"state"`
	JudgementBasis     string            `json:"judgement_basis"`
	SupersedesSequence string            `json:"supersedes_sequence"`
	Escape             json.RawMessage   `json:"escape"`
	Attestation        escapeAttestation `json:"attestation"`
}

// escapeStoredRow is the part of a committed escape row the closure reads.
type escapeStoredRow struct {
	RecordKind string          `json:"record_kind"`
	State      string          `json:"state"`
	Escape     json.RawMessage `json:"escape"`
}

type escapeResponse struct {
	RunID              string `json:"run_id"`
	AuditSequence      int64  `json:"audit_sequence"`
	RecordKind         string `json:"record_kind"`
	State              string `json:"state"`
	JudgementBasis     string `json:"judgement_basis"`
	PendingReason      string `json:"pending_reason,omitempty"`
	SourceEventKey     string `json:"source_event_key,omitempty"`
	SupersedesSequence string `json:"supersedes_sequence,omitempty"`
}

// escapeRefusal is a typed refusal raised inside the closure transaction
// (the BeforeAppend hook), so the handler can render it after the rollback.
type escapeRefusal struct {
	status  int
	code    string
	msg     string
	details map[string]any
}

func (e *escapeRefusal) Error() string { return e.code + ": " + e.msg }

// escapeRequireAuthenticated refuses an anonymous identity 401 BEFORE
// requireRunAccount, whose ownership check would otherwise answer an
// anonymous caller 403 account_forbidden on an account-owned run.
func (s *Server) escapeRequireAuthenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if IdentityFrom(r.Context()).IsAnonymous() {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"an authenticated token is required", nil)
			return
		}
		next(w, r)
	}
}

// handleRecordEscape is POST /v0/runs/{run_id}/escapes. A body carrying
// supersedes_sequence is a CLOSE; any other body is a RECORD. Every refusal
// appends nothing.
func (s *Server) handleRecordEscape(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// The agent INSIDE a run never records or closes an escape (#4694 AC5).
	// A PREFIX check, so a malformed mcp:run:<non-uuid> subject is refused.
	if isRunBoundSubject(id.Subject) {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound token cannot record or close an escape", nil)
		return
	}
	if !s.requireWriteScope(w, r, "write:runs") {
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "escape_unconfigured",
			"the escape verb needs a run and an audit repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		escapeValidation(s, w, r, "run_id", "run_id must be a UUID")
		return
	}
	raw, ok := s.escapeReadBody(w, r)
	if !ok {
		return
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys) // shape errors surface from the strict decode below
	if _, isClose := keys[escapeSupersedesKey]; isClose {
		var req escapeCloseRequest
		if !escapeDecodeStrict(s, w, r, raw, &req) {
			return
		}
		s.escapeClose(w, r, id, runID, req)
		return
	}
	var req escapeRecordRequest
	if !escapeDecodeStrict(s, w, r, raw, &req) {
		return
	}
	s.escapeRecord(w, r, id, runID, req)
}

// escapeReadBody reads the bounded request body.
func (s *Server) escapeReadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		escapeValidation(s, w, r, "body", "a JSON request body is required")
		return nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, escapeMaxBodyBytes))
	if err != nil {
		escapeValidation(s, w, r, "body", fmt.Sprintf("request body unreadable or over %d bytes", escapeMaxBodyBytes))
		return nil, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		escapeValidation(s, w, r, "body", "a JSON request body is required")
		return nil, false
	}
	return raw, true
}

// escapeDecodeStrict decodes ONE JSON document into dst, refusing an unknown
// field (naming it) and any trailing content.
func escapeDecodeStrict(s *Server, w http.ResponseWriter, r *http.Request, raw []byte, dst any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		field := "body"
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			field = strings.Trim(name, `"`)
		}
		escapeValidation(s, w, r, field, "request body does not match the escape shape: "+err.Error())
		return false
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		escapeValidation(s, w, r, "body", "request body carries trailing content after the JSON document")
		return false
	}
	return true
}

func escapeValidation(s *Server, w http.ResponseWriter, r *http.Request, field, msg string) {
	s.writeError(w, r, http.StatusBadRequest, "validation_failed", msg, map[string]any{"field": field})
}

// escapeLoadRun reads the target run: 404 run_not_found, 500 on any other
// error. Run state never gates a record or a close (A1.2).
func (s *Server) escapeLoadRun(w http.ResponseWriter, r *http.Request, runID uuid.UUID) (*run.Run, bool) {
	rn, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if errors.Is(err, run.ErrNotFound) || (err == nil && rn == nil) {
		s.writeError(w, r, http.StatusNotFound, "run_not_found", "run "+runID.String()+" not found", nil)
		return nil, false
	}
	if err != nil {
		s.escapeInternal(w, r, runID, "load run", err)
		return nil, false
	}
	return rn, true
}

func (s *Server) escapeInternal(w http.ResponseWriter, r *http.Request, runID uuid.UUID, step string, err error) {
	s.writeError(w, r, http.StatusInternalServerError, "internal_error", "could not "+step,
		map[string]any{internalCauseKey: "escape " + runID.String() + ": " + step + ": " + err.Error()})
}

// escapeSecurityFreeTextField names the first free-text field a RECORD body
// carries (A1.5 refuses each on a security escape), or "".
func escapeSecurityFreeTextField(req escapeRecordRequest) string {
	switch {
	case req.Description != nil:
		return "description"
	case req.Files != nil:
		return "files"
	case req.IntroducingCommits != nil:
		return "introducing_commits"
	}
	return ""
}

// escapeCanonicalUUID reports whether s is a UUID in canonical lowercase
// hyphenated form (R9b: a security idempotency key carries no caller prose).
func escapeCanonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// escapeValidRepoPath reports whether p is a clean repo-relative path.
func escapeValidRepoPath(p string) bool {
	if p == "" || len(p) > escapeMaxPathBytes || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	if path.Clean(p) != p || p == "." {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// escapeValidAdvisoryID validates a security advisory id by the run's forge:
// a GitHub run carries a GHSA id, any other forge an opaque bounded token.
func escapeValidAdvisoryID(forge, id string) bool {
	if forge == "github" {
		return escapeGHSARE.MatchString(id)
	}
	return escapeOpaqueAdvisory.MatchString(id)
}

// escapeValidateRecord applies the RECORD field rules that need no store
// read. It returns the offending field and message, or "" when valid.
func escapeValidateRecord(req escapeRecordRequest) (string, string) {
	if !escapeSources[req.Source] {
		return "source", "source must be one of later_reviewer, follow_up_bug, operator_battery, human_finding, ci_red_on_main"
	}
	if !escapeSeverities[req.Severity] {
		return "severity", "severity must be high, medium or low"
	}
	if !escapeCategories[req.Category] {
		return "category", "category must be one of correctness, security, reliability, concurrency, performance, data_integrity, test_coverage, documentation, other"
	}
	security := req.Category == escapeSecurityLabel
	if security {
		if req.SecurityClass == "" || !escapeSecurityClassRE.MatchString(req.SecurityClass) {
			return "security_class", "a security escape requires security_class matching ^[a-z0-9][a-z0-9_]{0,63}$"
		}
		if req.AdvisoryID == "" {
			return "advisory_id", "a security escape requires advisory_id (the forge's draft-advisory id)"
		}
	} else {
		if req.SecurityClass != "" {
			return "security_class", "security_class is accepted only on category=security"
		}
		if req.AdvisoryID != "" {
			return "advisory_id", "advisory_id is accepted only on category=security"
		}
		if req.Description == nil || strings.TrimSpace(*req.Description) == "" {
			return "description", "description is required unless category=security"
		}
		if len(req.Files) == 0 {
			return "files", "files is required unless category=security"
		}
	}
	if req.Description != nil && utf8.RuneCountInString(*req.Description) > escapeMaxDescriptionRunes {
		return "description", fmt.Sprintf("description exceeds %d characters", escapeMaxDescriptionRunes)
	}
	if len(req.Files) > escapeMaxFiles {
		return "files", fmt.Sprintf("files names more than %d paths", escapeMaxFiles)
	}
	for _, f := range req.Files {
		if !escapeValidRepoPath(f) {
			return "files", "files entry " + strconv.Quote(f) + " is not a clean repo-relative path"
		}
	}
	if req.HeadSHA != "" && !escapeShaRE.MatchString(req.HeadSHA) {
		return "head_sha", "head_sha must be a 40- or 64-character lowercase hex commit id"
	}
	if len(req.IntroducingCommits) > escapeMaxIntroducingCommits {
		return "introducing_commits", fmt.Sprintf("introducing_commits names more than %d commits", escapeMaxIntroducingCommits)
	}
	for _, c := range req.IntroducingCommits {
		if !escapeShaRE.MatchString(c) {
			return "introducing_commits", "introducing_commits entries must be 40- or 64-character lowercase hex commit ids"
		}
	}
	if req.Source == escapeSourceLaterReviewer {
		// A later_reviewer escape dedupes on concern:<uuid>; it carries no
		// idempotency key (approval condition C1).
		if req.IdempotencyKey != "" {
			return "idempotency_key", "a later_reviewer escape is keyed by its concern; idempotency_key is refused"
		}
		if _, err := uuid.Parse(req.ConcernID); err != nil {
			return "concern_id", "a later_reviewer escape requires concern_id (a UUID)"
		}
	} else {
		if req.ConcernID != "" {
			return "concern_id", "concern_id is accepted only on source=later_reviewer"
		}
		if !escapeIdempotencyKeyRE.MatchString(req.IdempotencyKey) {
			return "idempotency_key", "idempotency_key is required and must match ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$"
		}
		// R9b as narrowed by C1: a security escape whose dedupe key IS the
		// idempotency key carries a canonical UUID, never caller prose.
		if security && !escapeCanonicalUUID(req.IdempotencyKey) {
			return "idempotency_key", "a security escape's idempotency_key must be a canonical lowercase UUID"
		}
	}
	return "", ""
}

// escapeBirthState is the A1.2 / A1.4 birth rule. The arms are ordered: a
// review-finding source is pending whoever records it; an agent-recorded
// escape (agent subject or delegated request) is pending; a recorder without a
// forge-verified credential is pending; only then is a birth counted.
func escapeBirthState(source string, agentRecorded, forgeVerified bool) (state, basis, pendingReason string) {
	if source == escapeSourceLaterReviewer {
		return escapeStatePendingJudgement, escapeBasisAwaitingAttestation, "review_finding_source"
	}
	if agentRecorded {
		return escapeStatePendingJudgement, escapeBasisAwaitingAttestation, "agent_recorded"
	}
	if !forgeVerified {
		return escapeStatePendingJudgement, escapeBasisAwaitingAttestation, "credential_not_forge_verified"
	}
	return escapeStateCounted, escapeBasisNotRequired, ""
}

// escapeRecord is the RECORD branch.
func (s *Server) escapeRecord(w http.ResponseWriter, r *http.Request, id Identity, runID uuid.UUID, req escapeRecordRequest) {
	ctx := r.Context()
	if req.Source == escapeSourceLaterReviewer && s.cfg.ConcernRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "escape_unconfigured",
			"a later_reviewer escape needs the concern repository", nil)
		return
	}
	security := req.Category == escapeSecurityLabel
	if field := escapeSecurityFreeTextField(req); security && field != "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "escape_security_free_text_refused",
			"a security escape carries only its class, advisory id, severity, subject and source; "+field+" is refused (ADR-117 A1.5)",
			map[string]any{"field": field})
		return
	}
	if field, msg := escapeValidateRecord(req); field != "" {
		escapeValidation(s, w, r, field, msg)
		return
	}
	rn, ok := s.escapeLoadRun(w, r, runID)
	if !ok {
		return
	}
	if security && !escapeValidAdvisoryID(runForge(rn), req.AdvisoryID) {
		escapeValidation(s, w, r, "advisory_id", "advisory_id is not a valid advisory id for this run's forge")
		return
	}

	merged, err := s.runPRObservablyMerged(ctx, runID)
	if err != nil {
		s.escapeInternal(w, r, runID, "read the run's merge state", err)
		return
	}
	subject := escapeSubject{PullRequestMerged: merged, HeadSHA: req.HeadSHA}
	if rn.PullRequestURL != nil {
		subject.PullRequestURL = *rn.PullRequestURL
	}
	if subject.HeadSHA == "" && !subject.PullRequestMerged {
		s.writeError(w, r, http.StatusUnprocessableEntity, "escape_subject_unresolved",
			"the escape names no head_sha and the run's pull request is not observably merged", nil)
		return
	}

	esc := escapeObject{Source: req.Source, Severity: req.Severity, Category: req.Category, Subject: subject}
	if security {
		esc.Security = &escapeSecurity{Class: req.SecurityClass, AdvisoryID: req.AdvisoryID}
	} else {
		esc.Description = *req.Description
		esc.Files = req.Files
		if len(req.IntroducingCommits) > 0 {
			esc.IntroducingCommits = req.IntroducingCommits
			esc.IntroducingCommitsSource = "operator_supplied"
		}
	}

	if req.Source == escapeSourceLaterReviewer {
		c, ok := s.escapeResolveConcern(w, r, rn, req.ConcernID)
		if !ok {
			return
		}
		roundSeq, roundHead, err := s.escapeConcernRoundHead(ctx, c)
		if err != nil {
			s.escapeInternal(w, r, runID, "read the concern's review round", err)
			return
		}
		if security {
			roundHead = escapeSecurityReviewedHead(roundHead)
		}
		esc.ReviewedHead = roundHead
		ref := &escapeConcernRef{
			ID: c.ID.String(), RunID: c.RunID.String(), StageKind: c.StageKind,
			OriginReviewSequence: c.OriginReviewSequence, ReviewRoundSequence: roundSeq,
			ReviewerRole: c.ReviewerRole, Provenance: c.Provenance,
		}
		if c.ReviewerModel != nil {
			ref.ReviewerModel = *c.ReviewerModel
		}
		esc.Concern = ref
		key := escapeSourceKeyConcernPrefix + c.ID.String()
		s.escapeAppendBirth(w, r, id, rn, req, esc, key)
		return
	}
	esc.IdempotencyKey = req.IdempotencyKey
	key := escapeSourceKeyIdempotencyPrefix + req.IdempotencyKey
	s.escapeAppendBirth(w, r, id, rn, req, esc, key)
}

// escapeResolveConcern loads a later_reviewer escape's concern, failing
// closed: a missing concern (the repository's wrapped concern.ErrNotFound), a
// missing concern run, or a concern in another account is 422; any other read
// error is 500. The three 422 cases answer identically, so the verb is not an
// existence oracle across accounts.
func (s *Server) escapeResolveConcern(w http.ResponseWriter, r *http.Request, rn *run.Run, rawID string) (*concern.Concern, bool) {
	ctx := r.Context()
	cid, _ := uuid.Parse(rawID) // validated by escapeValidateRecord
	notFound := func() {
		s.writeError(w, r, http.StatusUnprocessableEntity, "escape_concern_not_found",
			"concern "+cid.String()+" not found", map[string]any{"field": "concern_id"})
	}
	cs, err := s.cfg.ConcernRepo.GetByIDs(ctx, []uuid.UUID{cid})
	if err != nil {
		if errors.Is(err, concern.ErrNotFound) {
			notFound()
			return nil, false
		}
		s.escapeInternal(w, r, rn.ID, "read the concern", err)
		return nil, false
	}
	if len(cs) == 0 || cs[0] == nil {
		notFound()
		return nil, false
	}
	c := cs[0]
	crun, err := s.cfg.RunRepo.GetRun(ctx, c.RunID)
	if errors.Is(err, run.ErrNotFound) || (err == nil && crun == nil) {
		notFound()
		return nil, false
	}
	if err != nil {
		s.escapeInternal(w, r, rn.ID, "read the concern's run", err)
		return nil, false
	}
	if crun.AccountID != rn.AccountID {
		notFound()
		return nil, false
	}
	return c, true
}

// escapeConcernRoundHead resolves the reviewed_head of the review round that
// raised c, over the CONCERN's run chain (R3): the implement_reviewed verdict
// at c.OriginReviewSequence → its review_round_sequence → the
// implement_review_started row at that sequence → its reviewed_head, copied
// JSON-equal. A plan-stage concern, a missing verdict, a zero round sequence,
// a missing started row or a pre-A1.7 row (no reviewed_head key) yields no
// head. A read error is returned: the record is immutable, so it fails closed.
func (s *Server) escapeConcernRoundHead(ctx context.Context, c *concern.Concern) (int64, json.RawMessage, error) {
	if c.StageKind == concern.StageKindPlan {
		return 0, nil, nil
	}
	verdict, err := s.auditEntryAt(ctx, c.RunID, "implement_reviewed", c.OriginReviewSequence)
	if err != nil {
		return 0, nil, fmt.Errorf("list implement_reviewed: %w", err)
	}
	if verdict == nil {
		return 0, nil, nil
	}
	var vp struct {
		ReviewRoundSequence int64 `json:"review_round_sequence"`
	}
	if json.Unmarshal(verdict.Payload, &vp) != nil || vp.ReviewRoundSequence <= 0 {
		return 0, nil, nil
	}
	started, err := s.auditEntryAt(ctx, c.RunID, "implement_review_started", vp.ReviewRoundSequence)
	if err != nil {
		return vp.ReviewRoundSequence, nil, fmt.Errorf("list implement_review_started: %w", err)
	}
	if started == nil {
		return vp.ReviewRoundSequence, nil, nil
	}
	var sp map[string]json.RawMessage
	if json.Unmarshal(started.Payload, &sp) != nil {
		return vp.ReviewRoundSequence, nil, nil
	}
	head, ok := sp["reviewed_head"]
	if !ok || bytes.Equal(bytes.TrimSpace(head), []byte("null")) {
		return vp.ReviewRoundSequence, nil, nil
	}
	return vp.ReviewRoundSequence, head, nil
}

// escapeSecurityReviewedHead projects a reviewed_head onto its scalar identity
// fields for a security escape (approval condition C3); the files,
// partial_files and omitted_files path sets are dropped. An undecodable head
// is dropped whole.
func escapeSecurityReviewedHead(head json.RawMessage) json.RawMessage {
	if head == nil {
		return nil
	}
	var all map[string]json.RawMessage
	if json.Unmarshal(head, &all) != nil {
		return nil
	}
	kept := make(map[string]json.RawMessage, len(escapeSecurityReviewedHeadKeys))
	for _, k := range escapeSecurityReviewedHeadKeys {
		if v, ok := all[k]; ok {
			kept[k] = v
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return out
}

// escapeAppendBirth builds the birth row and appends it deduplicated on
// source_event_key; a duplicate is 409 escape_already_recorded.
func (s *Server) escapeAppendBirth(w http.ResponseWriter, r *http.Request, id Identity, rn *run.Run, req escapeRecordRequest, esc escapeObject, key string) {
	credKind := approvalCredentialKind(id)
	agentRecorded := isAgentSubject(id.Subject) || req.Delegated
	state, basis, pendingReason := escapeBirthState(req.Source, agentRecorded, identity.ForgeVerifiedCredential(credKind))
	actorKind := actorKindForSubject(id.Subject)
	payload, err := json.Marshal(escapeBirthPayload{
		EscapeVersion: escapeVersion, RecordKind: escapeRecordKindBirth, RunID: rn.ID.String(),
		State: state, JudgementBasis: basis, PendingReason: pendingReason, AgentRecorded: agentRecorded,
		Recorder:       escapeRecorder{Subject: id.Subject, ActorKind: string(actorKind), CredentialKind: credKind, Delegated: req.Delegated},
		SourceEventKey: key, Escape: esc,
	})
	if err != nil {
		s.escapeInternal(w, r, rn.ID, "encode the escape", err)
		return
	}
	entry, err := s.escapeAppendDeduped(r.Context(), escapeChainParams(rn.ID, id.Subject, actorKind, payload),
		audit.DedupeSpec{PayloadKey: escapeSourceEventKey, PayloadValue: key})
	var dup *audit.DedupedDuplicateError
	if errors.As(err, &dup) {
		details := map[string]any{"source_event_key": key}
		if dup.Existing != nil {
			details["existing_sequence"] = dup.Existing.Sequence
		}
		s.writeError(w, r, http.StatusConflict, "escape_already_recorded",
			"an escape with this source event key is already recorded on the run", details)
		return
	}
	if err != nil {
		s.escapeInternal(w, r, rn.ID, "append the escape", err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, escapeResponse{
		RunID: rn.ID.String(), AuditSequence: entry.Sequence, RecordKind: escapeRecordKindBirth,
		State: state, JudgementBasis: basis, PendingReason: pendingReason, SourceEventKey: key,
	})
}

// escapeClose is the CLOSE branch: the A1.3 attestation refusals, then the
// single-successor closure, atomic under the run-row lock.
func (s *Server) escapeClose(w http.ResponseWriter, r *http.Request, id Identity, runID uuid.UUID, req escapeCloseRequest) {
	if req.SupersedesSequence <= 0 {
		escapeValidation(s, w, r, "supersedes_sequence", "supersedes_sequence must be a positive audit sequence")
		return
	}
	if req.Attestation == nil {
		escapeValidation(s, w, r, "attestation", "a close requires attestation {judgement, note?}")
		return
	}
	closedState, ok := escapeJudgements[req.Attestation.Judgement]
	if !ok {
		escapeValidation(s, w, r, "attestation.judgement", "attestation.judgement must be defect or no_defect")
		return
	}
	if utf8.RuneCountInString(req.Attestation.Note) > escapeMaxNoteRunes {
		escapeValidation(s, w, r, "attestation.note", fmt.Sprintf("attestation.note exceeds %d characters", escapeMaxNoteRunes))
		return
	}

	// A1.3: three INDEPENDENT refusals on the attestation write path.
	attest := req.Attestation != nil
	credKind := approvalCredentialKind(id)
	if attest && isAgentSubject(id.Subject) {
		s.writeError(w, r, http.StatusForbidden, "escape_attestation_requires_human",
			"an agent identity cannot attest an escape closure (ADR-117 A1.3)",
			map[string]any{"refused_actor": "agent_token"})
		return
	}
	if attest && req.Delegated {
		s.writeError(w, r, http.StatusForbidden, "escape_attestation_requires_human",
			"a delegated request cannot attest an escape closure (ADR-117 A1.3)",
			map[string]any{"refused_actor": "delegated"})
		return
	}
	if attest && !identity.ForgeVerifiedCredential(credKind) {
		s.writeError(w, r, http.StatusForbidden, "escape_attestation_requires_forge_credential",
			"an escape closure needs a forge-verified credential (ADR-117 A1.3)",
			map[string]any{"credential_kind": credKind, "forge_verified_kinds": identity.ForgeVerifiedKinds})
		return
	}

	rn, ok := s.escapeLoadRun(w, r, runID)
	if !ok {
		return
	}
	supersedesKey := strconv.FormatInt(req.SupersedesSequence, 10)
	note := req.Attestation.Note
	attestation := escapeAttestation{
		Judgement: req.Attestation.Judgement, Note: note, JudgementID: uuid.NewString(),
		ActorSubject: id.Subject, CredentialKind: credKind,
	}
	hook := func(same []*audit.Entry, p *audit.ChainAppendParams) error {
		var targetEntry *audit.Entry
		for _, e := range same {
			if e != nil && e.Sequence == req.SupersedesSequence {
				targetEntry = e
				break
			}
		}
		if targetEntry == nil {
			return &escapeRefusal{status: http.StatusUnprocessableEntity, code: "escape_target_not_found",
				msg: "no escape_recorded row at sequence " + supersedesKey + " on this run's chain"}
		}
		var target escapeStoredRow
		if err := json.Unmarshal(targetEntry.Payload, &target); err != nil {
			target = escapeStoredRow{}
		}
		if target.State != escapeStatePendingJudgement {
			return &escapeRefusal{status: http.StatusUnprocessableEntity, code: "escape_target_not_pending",
				msg:     "the escape at sequence " + supersedesKey + " is not pending_judgement",
				details: map[string]any{"state": target.State}}
		}
		var probe struct {
			Category string `json:"category"`
		}
		_ = json.Unmarshal(target.Escape, &probe)
		if probe.Category == escapeSecurityLabel && note != "" {
			return &escapeRefusal{status: http.StatusUnprocessableEntity, code: "escape_security_free_text_refused",
				msg:     "a security escape's closure carries no free-text note (ADR-117 A1.5)",
				details: map[string]any{"field": "attestation.note"}}
		}
		payload, err := json.Marshal(escapeClosurePayload{
			EscapeVersion: escapeVersion, RecordKind: escapeRecordKindClosure, RunID: rn.ID.String(),
			State: closedState, JudgementBasis: escapeBasisOperatorAttestation,
			SupersedesSequence: supersedesKey, Escape: target.Escape, Attestation: attestation,
		})
		if err != nil {
			return fmt.Errorf("encode closure: %w", err)
		}
		p.Payload = payload
		return nil
	}
	actorKind := actorKindForSubject(id.Subject)
	spec := audit.DedupeSpec{PayloadKey: escapeSupersedesKey, PayloadValue: supersedesKey, BeforeAppend: hook}
	entry, err := s.escapeAppendDeduped(r.Context(), escapeChainParams(rn.ID, id.Subject, actorKind, nil), spec)
	var refusal *escapeRefusal
	var dup *audit.DedupedDuplicateError
	switch {
	case errors.As(err, &refusal):
		s.writeError(w, r, refusal.status, refusal.code, refusal.msg, refusal.details)
		return
	case errors.As(err, &dup):
		details := map[string]any{"supersedes_sequence": supersedesKey}
		if dup.Existing != nil {
			details["successor_sequence"] = dup.Existing.Sequence
		}
		s.writeError(w, r, http.StatusConflict, "escape_already_closed",
			"the escape at sequence "+supersedesKey+" is already closed", details)
		return
	case err != nil:
		s.escapeInternal(w, r, rn.ID, "append the escape closure", err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, escapeResponse{
		RunID: rn.ID.String(), AuditSequence: entry.Sequence, RecordKind: escapeRecordKindClosure,
		State: closedState, JudgementBasis: escapeBasisOperatorAttestation, SupersedesSequence: supersedesKey,
	})
}

// escapeChainParams is an escape row's chained-append shape: run-scoped and
// stage-less.
func escapeChainParams(runID uuid.UUID, subject string, actorKind audit.ActorKind, payload json.RawMessage) audit.ChainAppendParams {
	sub := subject
	ak := actorKind
	return audit.ChainAppendParams{
		RunID: runID, StageID: nil, Timestamp: time.Now().UTC(), Category: CategoryEscapeRecorded,
		ActorKind: &ak, ActorSubject: &sub, Payload: payload,
	}
}

// escapeAppendDeduped appends through the atomic DedupedChainAppender when the
// audit repository has it (production), else the non-atomic list-then-append
// fallback ADR-117 A1.2 allows for an in-memory fake, through the same
// audit.CheckDedupeSpec scan + hook.
func (s *Server) escapeAppendDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	if d, ok := s.cfg.AuditRepo.(audit.DedupedChainAppender); ok {
		return d.AppendChainedDeduped(ctx, p, spec)
	}
	same, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, p.RunID, p.Category)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", p.Category, err)
	}
	if err := audit.CheckDedupeSpec(same, &p, spec); err != nil {
		return nil, err
	}
	return s.cfg.AuditRepo.AppendChained(ctx, p)
}
