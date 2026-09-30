package server

// The "since you last looked" digest surface (E75.6 / #3734, ADR-082 #3728
// rule 7). Two routes over backend/internal/digest:
//
//   - GET /v0/digest computes the caller's digest for one repository against
//     their read watermark. It NEVER writes — retrieval does not advance the
//     watermark — and it applies digest.Bound at digest.DefaultByteBudget,
//     the ONE serialized-size bound every digest surface shares.
//   - POST /v0/digest/mark-read advances the caller's watermark behind the
//     EXISTING write:approvals scope (no new scope, the E34.2 / #1595
//     precedent, so no token loses access). It is never silent: the
//     digest_marked_read GLOBAL-chain entry is appended FIRST and the
//     watermark moves only after that append succeeds (digest.Store.MarkRead
//     owns the ordering; this file supplies the appender).
//
// The watermark is keyed by a subject + repository + account, so two
// subjects' digests are independent. WHICH subject is ADR-083 rule 6 (E76.3 /
// #3766), resolved through currentCaptain and named on every response as
// captain_subject_basis so a fallback is never silent:
//
//   - GET reads the repository's SEATED captain's watermark by default
//     ("captain"); a vacant seat or an unreadable captain record falls back
//     to the caller's own subject ("caller_vacant" / "caller_unavailable",
//     the pre-E76.3 behaviour). An optional read-only captain_subject query
//     parameter overrides the key: "captain" when it names the seated
//     captain, "caller" when it names the caller, "explicit" otherwise.
//   - mark-read ALWAYS advances the CALLER's own watermark — only the seated
//     captain advances the captain's, because for them the two keys are the
//     same row. A non-captain's mark moves their own key ("caller"), never
//     the seat's. It accepts no captain_subject: a write must never let a
//     caller aim an advance at an arbitrary key. The digest_marked_read entry
//     records marked_by and the basis either way.
//
// A nil DigestStore / DigestIndex (or, for mark-read, a nil AuditRepo)
// degrades both routes to 501 digest_unconfigured naming what is missing,
// rather than panicking.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
)

const (
	// scopeDigestRead gates GET /v0/digest: the digest is a projection of the
	// audit chain, so it takes the chain's read scope (the gate-view
	// precedent, scopeGateViewRead).
	scopeDigestRead = scopeGateViewRead
	// scopeDigestMarkRead gates POST /v0/digest/mark-read: the EXISTING
	// write:approvals scope — a captain acknowledging what they reviewed.
	scopeDigestMarkRead = "write:approvals"

	// digestMarkedReadCategory is the chain entry mark-read appends before
	// the watermark moves (registered in audit/categories.go).
	digestMarkedReadCategory = "digest_marked_read"

	// digestMarkReadMaxBodyBytes bounds the mark-read request body.
	digestMarkReadMaxBodyBytes = 4 << 10

	// digestCaptainSubjectMaxBytes caps GET's captain_subject override.
	digestCaptainSubjectMaxBytes = 256
)

// The captain_subject_basis values (E76.3 / #3766): which subject a digest
// response's watermark key is.
const (
	// digestBasisCaptain: the key is the repository's seated captain.
	digestBasisCaptain = "captain"
	// digestBasisCaller: a captain is seated but the key is the caller's own
	// subject (a non-captain's mark-read, or a GET override naming the caller).
	digestBasisCaller = "caller"
	// digestBasisCallerVacant: the seat is vacant, so the key is the caller's.
	digestBasisCallerVacant = "caller_vacant"
	// digestBasisCallerUnavailable: the captain record is not wired or could
	// not be read, so the key is the caller's (never reported as a vacancy).
	digestBasisCallerUnavailable = "caller_unavailable"
	// digestBasisExplicit: a GET override naming neither the caller nor the
	// seated captain.
	digestBasisExplicit = "explicit"
)

// digestCallerFallbackBasis maps a non-captain resolution onto the caller-
// keyed basis that names it.
func digestCallerFallbackBasis(captainBasis string) string {
	switch captainBasis {
	case captainBasisCaptain:
		return digestBasisCaller
	case captainBasisVacant:
		return digestBasisCallerVacant
	default:
		return digestBasisCallerUnavailable
	}
}

// resolveDigestReadKey picks GET's watermark key. With no override it is the
// seated captain, falling back to the caller; an override is taken verbatim
// and only its basis is resolved.
func (s *Server) resolveDigestReadKey(ctx context.Context, repo, caller, override string, hasOverride bool) (key, basis string) {
	captainSubject, _, captainBasis := s.currentCaptain(ctx, identityAccountID(ctx), repo)
	seated := captainBasis == captainBasisCaptain
	if hasOverride {
		switch {
		case seated && override == captainSubject:
			return override, digestBasisCaptain
		case override == caller:
			return override, digestBasisCaller
		default:
			return override, digestBasisExplicit
		}
	}
	if seated {
		return captainSubject, digestBasisCaptain
	}
	return caller, digestCallerFallbackBasis(captainBasis)
}

// resolveDigestMarkKey picks mark-read's watermark key: ALWAYS the caller's
// own subject (maintainer decision on #3766 — only the seated captain
// advances the captain's watermark, because for them the keys coincide).
// Only the basis depends on the seat.
func (s *Server) resolveDigestMarkKey(ctx context.Context, repo, caller string) (key, basis string) {
	captainSubject, _, captainBasis := s.currentCaptain(ctx, identityAccountID(ctx), repo)
	if captainBasis == captainBasisCaptain && captainSubject == caller {
		return caller, digestBasisCaptain
	}
	return caller, digestCallerFallbackBasis(captainBasis)
}

// digestResponse is the GET /v0/digest answer: the shared digest wire model
// plus the basis its watermark key was resolved on.
type digestResponse struct {
	digest.Digest
	CaptainSubjectBasis string `json:"captain_subject_basis"`
}

// digestBasisOverhead is the serialized size captain_subject_basis adds to a
// bounded digest, so the WHOLE response stays inside digest.DefaultByteBudget.
func digestBasisOverhead(basis string) int {
	b, _ := json.Marshal(basis)
	return len(`,"captain_subject_basis":`) + len(b)
}

// digestConfigured writes the 501 digest_unconfigured envelope and returns
// false when a collaborator the route needs is not wired.
func (s *Server) digestConfigured(w http.ResponseWriter, r *http.Request, needAudit bool) bool {
	var missing []string
	if s.cfg.DigestStore == nil {
		missing = append(missing, "digest_store")
	}
	if s.cfg.DigestIndex == nil {
		missing = append(missing, "decision_index")
	}
	if needAudit && s.cfg.AuditRepo == nil {
		missing = append(missing, "audit_repository")
	}
	if len(missing) == 0 {
		return true
	}
	// The missing collaborators ride the MESSAGE: writeError redacts every
	// non-allow-listed 5xx detail key, and the named reason is the point.
	s.writeError(w, r, http.StatusNotImplemented, "digest_unconfigured",
		"the digest surface is not wired on this deployment; missing: "+strings.Join(missing, ", "), nil)
	return false
}

// parseDigestSequence reads an optional non-negative integer query parameter
// (0 when absent). It writes the 400 and returns ok=false on a bad value.
func (s *Server) parseDigestSequence(w http.ResponseWriter, r *http.Request, field string) (int64, bool) {
	raw := r.URL.Query().Get(field)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			field+" must be a non-negative integer",
			map[string]any{"field": field, "got": raw})
		return 0, false
	}
	return n, true
}

// writeDigestError maps a digest package error onto the wire: a malformed
// request is 400 validation_failed, a to_sequence above the chain head is 400
// to_sequence_beyond_chain_head naming the head, anything else is 500.
func (s *Server) writeDigestError(w http.ResponseWriter, r *http.Request, err error) {
	var beyond *digest.BeyondChainHeadError
	switch {
	case errors.As(err, &beyond):
		s.writeError(w, r, http.StatusBadRequest, "to_sequence_beyond_chain_head",
			err.Error(),
			map[string]any{"field": "to_sequence", "requested": beyond.Requested, "chain_head": beyond.Head})
	case errors.Is(err, digest.ErrInvalidRequest):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(), nil)
	default:
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"digest failed", map[string]any{"error": err.Error()})
	}
}

// handleGetDigest serves GET /v0/digest?repo=&section=&from_sequence=&to_sequence=.
func (s *Server) handleGetDigest(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeDigestRead) {
		return
	}
	if !s.digestConfigured(w, r, false) {
		return
	}
	q := r.URL.Query()
	repo := q.Get("repo")
	if repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required (owner/name)", map[string]any{"field": "repo"})
		return
	}
	section, err := digest.ParseSection(q.Get("section"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(),
			map[string]any{"field": "section"})
		return
	}
	from, ok := s.parseDigestSequence(w, r, "from_sequence")
	if !ok {
		return
	}
	to, ok := s.parseDigestSequence(w, r, "to_sequence")
	if !ok {
		return
	}
	override, hasOverride := "", q.Has("captain_subject")
	if hasOverride {
		override = q.Get("captain_subject")
		if override == "" || len(override) > digestCaptainSubjectMaxBytes {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"captain_subject, when set, must be a non-empty subject of at most "+strconv.Itoa(digestCaptainSubjectMaxBytes)+" bytes",
				map[string]any{"field": "captain_subject", "length": len(override)})
			return
		}
	}
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}
	ctx := r.Context()
	key, basis := s.resolveDigestReadKey(ctx, repo, IdentityFrom(ctx).Subject, override, hasOverride)
	d, err := digest.Build(ctx, digest.Deps{Store: s.cfg.DigestStore, Index: s.cfg.DigestIndex}, digest.Request{
		Repo:           repo,
		CaptainSubject: key,
		AccountID:      identityAccountID(ctx),
		Section:        section,
		FromSequence:   from,
		ToSequence:     to,
	})
	if err != nil {
		s.writeDigestError(w, r, err)
		return
	}
	bounded, err := digest.Bound(d, digest.DefaultByteBudget-digestBasisOverhead(basis))
	if err != nil {
		s.writeDigestError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, digestResponse{Digest: bounded, CaptainSubjectBasis: basis})
}

// digestMarkReadRequest is the POST /v0/digest/mark-read body. It
// deliberately carries NO captain_subject: DisallowUnknownFields refuses one
// with 400, so a write can never be aimed at another subject's key.
type digestMarkReadRequest struct {
	Repo       string `json:"repo"`
	ToSequence int64  `json:"to_sequence"`
}

// digestMarkReadResponse is the POST /v0/digest/mark-read answer.
type digestMarkReadResponse struct {
	Repo           string `json:"repo"`
	CaptainSubject string `json:"captain_subject"`
	// CaptainSubjectBasis is "captain" when the caller is the seated captain,
	// otherwise the caller-keyed basis naming why the seat's key did not move.
	CaptainSubjectBasis string `json:"captain_subject_basis"`
	digest.MarkReadResult
}

// handleDigestMarkRead serves POST /v0/digest/mark-read.
func (s *Server) handleDigestMarkRead(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeDigestMarkRead) {
		return
	}
	if !s.digestConfigured(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, digestMarkReadMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req digestMarkReadRequest
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}
	if req.Repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required (owner/name)", map[string]any{"field": "repo"})
		return
	}
	if req.ToSequence <= 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"to_sequence must be a positive chain sequence", map[string]any{"field": "to_sequence"})
		return
	}
	ctx := r.Context()
	caller := IdentityFrom(ctx).Subject
	key, basis := s.resolveDigestMarkKey(ctx, req.Repo, caller)
	res, err := s.cfg.DigestStore.MarkRead(ctx, digest.AppenderFunc(s.appendDigestMarkedRead), digest.MarkReadParams{
		AccountID:           identityAccountID(ctx),
		CaptainSubject:      key,
		MarkedBy:            caller,
		CaptainSubjectBasis: basis,
		Repo:                req.Repo,
		ToSequence:          req.ToSequence,
	})
	if err != nil {
		s.writeDigestError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, digestMarkReadResponse{
		Repo: req.Repo, CaptainSubject: key, CaptainSubjectBasis: basis, MarkReadResult: res})
}

// appendDigestMarkedRead is MarkRead's appender: one digest_marked_read entry
// on the GLOBAL chain partitioned by the caller's account (the mark is repo-
// and subject-scoped and belongs to no run), attributed to the subject that
// performed the mark (marked_by).
func (s *Server) appendDigestMarkedRead(ctx context.Context, e digest.MarkedReadEvent) error {
	payload, err := e.Payload()
	if err != nil {
		return err
	}
	kind := audit.ActorUser
	subject := e.MarkedBy
	if subject == "" {
		subject = e.CaptainSubject
	}
	_, err = s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    time.Now().UTC(),
		Category:     digestMarkedReadCategory,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
		AccountID:    e.AccountID,
	})
	return err
}
