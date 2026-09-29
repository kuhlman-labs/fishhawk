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
// The watermark is keyed by the caller's auth subject + repository + account,
// so two captains' digests are independent. A nil DigestStore / DigestIndex
// (or, for mark-read, a nil AuditRepo) degrades both routes to 501
// digest_unconfigured naming what is missing, rather than panicking.

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
)

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
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}
	ctx := r.Context()
	d, err := digest.Build(ctx, digest.Deps{Store: s.cfg.DigestStore, Index: s.cfg.DigestIndex}, digest.Request{
		Repo:           repo,
		CaptainSubject: IdentityFrom(ctx).Subject,
		AccountID:      identityAccountID(ctx),
		Section:        section,
		FromSequence:   from,
		ToSequence:     to,
	})
	if err != nil {
		s.writeDigestError(w, r, err)
		return
	}
	bounded, err := digest.Bound(d, digest.DefaultByteBudget)
	if err != nil {
		s.writeDigestError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, bounded)
}

// digestMarkReadRequest is the POST /v0/digest/mark-read body.
type digestMarkReadRequest struct {
	Repo       string `json:"repo"`
	ToSequence int64  `json:"to_sequence"`
}

// digestMarkReadResponse is the POST /v0/digest/mark-read answer.
type digestMarkReadResponse struct {
	Repo           string `json:"repo"`
	CaptainSubject string `json:"captain_subject"`
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
	subject := IdentityFrom(ctx).Subject
	res, err := s.cfg.DigestStore.MarkRead(ctx, digest.AppenderFunc(s.appendDigestMarkedRead), digest.MarkReadParams{
		AccountID:      identityAccountID(ctx),
		CaptainSubject: subject,
		Repo:           req.Repo,
		ToSequence:     req.ToSequence,
	})
	if err != nil {
		s.writeDigestError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, digestMarkReadResponse{Repo: req.Repo, CaptainSubject: subject, MarkReadResult: res})
}

// appendDigestMarkedRead is MarkRead's appender: one digest_marked_read entry
// on the GLOBAL chain partitioned by the caller's account (the mark is repo-
// and captain-scoped and belongs to no run), attributed to the captain.
func (s *Server) appendDigestMarkedRead(ctx context.Context, e digest.MarkedReadEvent) error {
	payload, err := e.Payload()
	if err != nil {
		return err
	}
	kind := audit.ActorUser
	subject := e.CaptainSubject
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
