package server

// The captain record surface (E76.2 / #3765, ADR-083 #3751 rules 2-5). Six
// routes over backend/internal/captain:
//
//   - GET  /v0/captain?repo=          the derived record: current captain,
//     pending handover offer, last captain and the chain history. Never writes.
//   - POST /v0/captain/offer          the sitting captain offers the seat.
//   - POST /v0/captain/withdraw       the offering captain withdraws the offer.
//   - POST /v0/captain/accept         the named successor accepts.
//   - POST /v0/captain/relinquish     the sitting captain vacates the seat.
//   - POST /v0/captain/claim          a fallback claim of a VACANT seat, gated
//     on the repository's approval predicate (classifyRepoPredicate).
//
// Every POST runs through captain.Store.Apply, which serializes read ->
// derive -> validate -> append inside ONE advisory-locked transaction. The
// agent/delegated refusal and every other refusal live ONLY in the captain
// package's transitions: this file computes the actor booleans and the
// predicate outcome and passes them in, and maps each typed refusal to its
// OWN status/code (captainErrorStatus) so no refusal mode collapses into
// another. The five verbs take the EXISTING write:approvals scope (no new
// scope, so no token loses access). A nil CaptainStore degrades all six to
// 501 captain_unconfigured. Nothing here is read by approval quorum or
// eligibility (ADR-083 rule 1).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

const (
	// scopeCaptainRead gates GET /v0/captain: the record is a projection of
	// the audit chain, so it takes the chain's read scope (the digest /
	// gate-view precedent).
	scopeCaptainRead = scopeGateViewRead
	// scopeCaptainWrite gates the five verbs: the EXISTING write:approvals
	// scope, so the Auth checklist's impact inventory is empty.
	scopeCaptainWrite = "write:approvals"

	// captainMaxBodyBytes bounds a verb request body.
	captainMaxBodyBytes = 4 << 10
	// captainHistoryLimit caps the history GET /v0/captain renders: the
	// NEWEST entries, in ascending order, with history_total naming how many
	// the chain holds.
	captainHistoryLimit = 50

	// captainTrivialBasis is the predicate basis recorded on a claim whose
	// repository positively declares no min_permission and no member_of.
	captainTrivialBasis = "trivial:any-non-agent-token-holder"
)

// captainConfigured writes the 501 captain_unconfigured envelope and returns
// false when the store is not wired.
func (s *Server) captainConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.CaptainStore != nil {
		return true
	}
	// The missing collaborator rides the MESSAGE: writeError redacts every
	// non-allow-listed 5xx detail key.
	s.writeError(w, r, http.StatusNotImplemented, "captain_unconfigured",
		"the captain record surface is not wired on this deployment; missing: captain_store", nil)
	return false
}

// captainRefusal is one typed captain error's wire mapping.
type captainRefusal struct {
	status int
	code   string
}

// captainRefusals maps each captain sentinel to its OWN status and code. It
// is a table (not a switch) so TestCaptainErrorStatus_EachRefusalDistinct can
// assert no two refusal modes share a code.
var captainRefusals = []struct {
	err error
	captainRefusal
}{
	{captain.ErrActorRequired, captainRefusal{http.StatusUnauthorized, "authentication_required"}},
	{captain.ErrAgentIdentity, captainRefusal{http.StatusForbidden, "captain_agent_identity_refused"}},
	{captain.ErrNotCaptain, captainRefusal{http.StatusForbidden, "captain_not_captain"}},
	{captain.ErrNotOfferer, captainRefusal{http.StatusForbidden, "captain_not_offerer"}},
	{captain.ErrOfferSuccessorMismatch, captainRefusal{http.StatusForbidden, "captain_offer_successor_mismatch"}},
	{captain.ErrPredicateRejected, captainRefusal{http.StatusForbidden, "captain_predicate_rejected"}},
	{captain.ErrNoCaptain, captainRefusal{http.StatusConflict, "captain_no_captain"}},
	{captain.ErrNoOffer, captainRefusal{http.StatusConflict, "captain_no_offer"}},
	{captain.ErrCaptainExists, captainRefusal{http.StatusConflict, "captain_exists"}},
	{captain.ErrSelfHandover, captainRefusal{http.StatusConflict, "captain_self_handover"}},
	{captain.ErrSuccessorRequired, captainRefusal{http.StatusBadRequest, "captain_successor_required"}},
	{captain.ErrRepoRequired, captainRefusal{http.StatusBadRequest, "validation_failed"}},
	{captain.ErrPredicateUndeterminable, captainRefusal{http.StatusUnprocessableEntity, "captain_predicate_undeterminable"}},
}

// captainErrorStatus maps a captain error onto the wire; ok is false for an
// unrecognized (internal) error.
func captainErrorStatus(err error) (captainRefusal, bool) {
	for _, m := range captainRefusals {
		if errors.Is(err, m.err) {
			return m.captainRefusal, true
		}
	}
	return captainRefusal{http.StatusInternalServerError, "internal_error"}, false
}

// writeCaptainError writes a captain error's envelope.
func (s *Server) writeCaptainError(w http.ResponseWriter, r *http.Request, err error, details map[string]any) {
	ref, ok := captainErrorStatus(err)
	if !ok {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"captain record operation failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeError(w, r, ref.status, ref.code, err.Error(), details)
}

// captainRecordResponse is the current captain on the wire. ClaimVerified is
// rendered SEPARATELY from IdentityVerified: null for an assigned (handed-
// over) seat, true/false only for a claimed one.
type captainRecordResponse struct {
	Subject           string    `json:"subject"`
	IdentityVerified  bool      `json:"identity_verified"`
	ClaimVerified     *bool     `json:"claim_verified"`
	Basis             string    `json:"basis"`
	AssignedSequence  int64     `json:"assigned_sequence"`
	AssignedEntryHash string    `json:"assigned_entry_hash"`
	AssignedAt        time.Time `json:"assigned_at"`
}

// captainOfferResponse is the pending handover offer on the wire.
// IdentityVerified is the SUCCESSOR's provider qualification.
type captainOfferResponse struct {
	Successor        string    `json:"successor"`
	IdentityVerified bool      `json:"identity_verified"`
	OfferedBy        string    `json:"offered_by"`
	OfferEntryHash   string    `json:"offer_entry_hash"`
	OfferedSequence  int64     `json:"offered_sequence"`
	OfferedAt        time.Time `json:"offered_at"`
}

// captainHistoryItem is one captain chain entry on the wire.
type captainHistoryItem struct {
	Sequence  int64           `json:"sequence"`
	EntryHash string          `json:"entry_hash"`
	Category  string          `json:"category"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload"`
}

// captainResponse is the GET /v0/captain body.
type captainResponse struct {
	Repo           string                 `json:"repo"`
	Captain        *captainRecordResponse `json:"captain"`
	PendingOffer   *captainOfferResponse  `json:"pending_offer"`
	LastCaptain    *string                `json:"last_captain"`
	History        []captainHistoryItem   `json:"history"`
	HistoryTotal   int                    `json:"history_total"`
	SkippedEntries int                    `json:"skipped_entries"`
}

// captainVerbResponse is every POST verb's 200 body: the recorded event and
// the record re-derived with it folded in.
type captainVerbResponse struct {
	Repo         string                 `json:"repo"`
	Event        captainHistoryItem     `json:"event"`
	Captain      *captainRecordResponse `json:"captain"`
	PendingOffer *captainOfferResponse  `json:"pending_offer"`
}

// renderCaptainRecord projects a derived Record. claim_verified is the
// record's OWN field, never the identity-verification value.
func renderCaptainRecord(rec *captain.Record) *captainRecordResponse {
	if rec == nil {
		return nil
	}
	return &captainRecordResponse{
		Subject:           rec.Subject,
		IdentityVerified:  rec.IdentityVerified,
		ClaimVerified:     rec.ClaimVerified,
		Basis:             string(rec.Basis),
		AssignedSequence:  rec.AssignedSequence,
		AssignedEntryHash: rec.AssignedEntryHash,
		AssignedAt:        rec.AssignedAt,
	}
}

func renderCaptainOffer(o *captain.HandoverOffer) *captainOfferResponse {
	if o == nil {
		return nil
	}
	return &captainOfferResponse{
		Successor:        o.Successor,
		IdentityVerified: o.SuccessorIdentityVerified,
		OfferedBy:        o.OfferedBy,
		OfferEntryHash:   o.EntryHash,
		OfferedSequence:  o.Sequence,
		OfferedAt:        o.OfferedAt,
	}
}

// renderCaptainSnapshot builds the GET body from a lock-free read.
func renderCaptainSnapshot(repo string, snap *captain.Snapshot) captainResponse {
	out := captainResponse{
		Repo:           repo,
		Captain:        renderCaptainRecord(snap.State.Current),
		PendingOffer:   renderCaptainOffer(snap.State.PendingOffer),
		LastCaptain:    snap.State.LastCaptain,
		History:        []captainHistoryItem{},
		HistoryTotal:   len(snap.Entries),
		SkippedEntries: snap.State.SkippedEntries,
	}
	entries := snap.Entries
	if len(entries) > captainHistoryLimit {
		entries = entries[len(entries)-captainHistoryLimit:]
	}
	for _, e := range entries {
		out.History = append(out.History, captainHistoryItem{
			Sequence: e.Sequence, EntryHash: e.EntryHash, Category: e.Category, At: e.Timestamp, Payload: e.Payload,
		})
	}
	return out
}

// handleGetCaptain serves GET /v0/captain?repo=.
func (s *Server) handleGetCaptain(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeCaptainRead) {
		return
	}
	if !s.captainConfigured(w, r) {
		return
	}
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required (owner/name)", map[string]any{"field": "repo"})
		return
	}
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}
	snap, err := s.cfg.CaptainStore.Read(r.Context(), identityAccountID(r.Context()), repo)
	if err != nil {
		s.writeCaptainError(w, r, err, nil)
		return
	}
	s.writeJSON(w, r, http.StatusOK, renderCaptainSnapshot(repo, snap))
}

// captainVerbRequest is every POST verb's body. Successor is accepted only
// by offer. Delegated opts the request into the ADR-040 delegated-action
// path — which the captain record REFUSES (via the domain guard), because a
// delegated identity can never hold, hand over or claim the seat.
type captainVerbRequest struct {
	Repo      string `json:"repo"`
	Successor string `json:"successor,omitempty"`
	Delegated bool   `json:"delegated,omitempty"`
}

// captainVerb names one POST verb and its transition.
type captainVerb struct {
	name           string
	takesSuccessor bool
	transition     func(captain.State, captain.Params) (captain.Event, error)
}

var (
	captainVerbOffer      = captainVerb{"offer", true, captain.Offer}
	captainVerbWithdraw   = captainVerb{"withdraw", false, captain.Withdraw}
	captainVerbAccept     = captainVerb{"accept", false, captain.Accept}
	captainVerbRelinquish = captainVerb{"relinquish", false, captain.Relinquish}
	captainVerbClaim      = captainVerb{"claim", false, captain.Claim}
)

func (s *Server) handleCaptainOffer(w http.ResponseWriter, r *http.Request) {
	s.handleCaptainVerb(w, r, captainVerbOffer)
}

func (s *Server) handleCaptainWithdraw(w http.ResponseWriter, r *http.Request) {
	s.handleCaptainVerb(w, r, captainVerbWithdraw)
}

func (s *Server) handleCaptainAccept(w http.ResponseWriter, r *http.Request) {
	s.handleCaptainVerb(w, r, captainVerbAccept)
}

func (s *Server) handleCaptainRelinquish(w http.ResponseWriter, r *http.Request) {
	s.handleCaptainVerb(w, r, captainVerbRelinquish)
}

func (s *Server) handleCaptainClaim(w http.ResponseWriter, r *http.Request) {
	s.handleCaptainVerb(w, r, captainVerbClaim)
}

// handleCaptainVerb is the shared body of the five POST verbs.
func (s *Server) handleCaptainVerb(w http.ResponseWriter, r *http.Request, verb captainVerb) {
	if !s.requireWriteScope(w, r, scopeCaptainWrite) {
		return
	}
	if !s.captainConfigured(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, captainMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req captainVerbRequest
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.Successor = strings.TrimSpace(req.Successor)
	if req.Repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required (owner/name)", map[string]any{"field": "repo"})
		return
	}
	if req.Successor != "" && !verb.takesSuccessor {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"successor applies only to offer; "+verb.name+" takes repo only",
			map[string]any{"field": "successor", "verb": verb.name})
		return
	}
	// Writes are NOT visibility-filtered (#2071): the repo-ACL mirror is a
	// read filter, and the digest mark-read precedent leaves writes to scope.
	ctx := r.Context()
	subject := IdentityFrom(ctx).Subject
	// The two actor booleans are COMPUTED here and REFUSED only in the
	// captain package's guardActor — the single authoritative check.
	params := captain.Params{
		Repo:             req.Repo,
		Actor:            subject,
		ActorIsAgent:     captainActorIsAgent(subject),
		ActorIsDelegated: req.Delegated,
		Successor:        req.Successor,
	}
	if verb.name == captainVerbClaim.name {
		params.Predicate, params.PredicateBasis = s.classifyRepoPredicate(ctx, req.Repo, subject)
	}
	applied, err := s.cfg.CaptainStore.Apply(ctx, captain.ApplyParams{
		AccountID: identityAccountID(ctx),
		Repo:      req.Repo,
		Actor:     subject,
		ActorKind: actorKindForSubject(subject),
		Timestamp: time.Now().UTC(),
	}, func(st captain.State) (captain.Event, error) {
		return verb.transition(st, params)
	})
	if err != nil {
		details := map[string]any{"verb": verb.name, "repo": req.Repo}
		if verb.name == captainVerbClaim.name {
			details["predicate_basis"] = params.PredicateBasis
		}
		s.writeCaptainError(w, r, err, details)
		return
	}
	s.writeJSON(w, r, http.StatusOK, captainVerbResponse{
		Repo: req.Repo,
		Event: captainHistoryItem{
			Sequence: applied.Entry.Sequence, EntryHash: applied.Entry.EntryHash,
			Category: applied.Entry.Category, At: applied.Entry.Timestamp, Payload: applied.Entry.Payload,
		},
		Captain:      renderCaptainRecord(applied.State.Current),
		PendingOffer: renderCaptainOffer(applied.State.PendingOffer),
	})
}

// captainActorIsAgent reports whether subject is an agent identity: the
// operator-agent token family (actorKindForSubject) or a run-bound MCP token
// subject, which belongs to an agent run. It only CLASSIFIES; the refusal is
// the captain package's guardActor.
func captainActorIsAgent(subject string) bool {
	return actorKindForSubject(subject) == audit.ActorAgent || strings.HasPrefix(subject, "mcp:run:")
}

// classifyRepoPredicate resolves a claim's predicate as an explicit
// trichotomy that FAILS CLOSED:
//
//   - Undeterminable: no run repository wired, the run list failed, no run,
//     no cached WorkflowSpec bytes, a spec that does not parse, a legacy
//     GitHub-handle `approvers` gate (a predicate this path cannot evaluate),
//     or an identity provider that is unavailable/unconfigured. An unreadable
//     predicate is NEVER trivial.
//   - Trivial: the spec cached on the repository's NEWEST run parses and,
//     folded strictest-per-dimension across every approval gate of every
//     workflow, declares no min_permission and no member_of.
//   - NonTrivialSatisfied / NonTrivialRejected: the fold declares one or
//     both, and resolvePredicates (the approval quorum's own forge seam)
//     resolved the claimant against it.
//
// The newest run's cached spec is the handleGetRepoPosture precedent: the
// claim path never fetches the spec live from the forge.
func (s *Server) classifyRepoPredicate(ctx context.Context, repo, subject string) (captain.PredicateOutcome, string) {
	if s.cfg.RunRepo == nil {
		return captain.PredicateUndeterminable, "undeterminable:no_run_repository"
	}
	rows, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo: repo, AccountID: IdentityFrom(ctx).AccountID, Limit: 1,
	})
	if err != nil {
		return captain.PredicateUndeterminable, "undeterminable:list_runs_failed"
	}
	if len(rows) == 0 {
		return captain.PredicateUndeterminable, "undeterminable:no_run"
	}
	newest := rows[0]
	if len(bytes.TrimSpace(newest.WorkflowSpec)) == 0 {
		return captain.PredicateUndeterminable, "undeterminable:no_cached_spec"
	}
	parsed, err := spec.ParseBytes(newest.WorkflowSpec)
	if err != nil {
		return captain.PredicateUndeterminable, "undeterminable:spec_unparseable"
	}
	folded, legacy := foldRepoApprovals(parsed)
	if legacy {
		return captain.PredicateUndeterminable, "undeterminable:legacy_approvers_gate"
	}
	if folded.minPermission == "" && len(folded.memberOf) == 0 {
		return captain.PredicateTrivial, captainTrivialBasis
	}
	basis := captainPredicateBasis(folded)
	outcome, _, _ := s.resolvePredicates(ctx, observationForgeID(newest.InstallationRef), repo, subject, folded)
	switch outcome {
	case predicateSatisfied:
		return captain.PredicateNonTrivialSatisfied, basis
	case predicateRejected:
		return captain.PredicateNonTrivialRejected, basis
	case predicateUnconfigured:
		return captain.PredicateUndeterminable, "undeterminable:identity_unconfigured"
	default: // predicateUnavailable
		return captain.PredicateUndeterminable, "undeterminable:identity_unavailable"
	}
}

// foldRepoApprovals folds every approval gate's *spec.Approvals block of
// every workflow STRICTEST-PER-DIMENSION through effectiveApprovals (max
// count, strictest min_permission, union of member_of), never mutating the
// parsed spec. legacy is true when any approval gate carries only the v0/v1
// GitHub-handle `approvers` allow-list, which the claim cannot evaluate.
func foldRepoApprovals(parsed *spec.Spec) (escalatedApprovals, bool) {
	acc := escalatedApprovals{}
	found := false
	legacy := false
	ids := make([]string, 0, len(parsed.Workflows))
	for id := range parsed.Workflows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, stg := range parsed.Workflows[id].Stages {
			for _, gate := range stg.Gates {
				if gate.Type != spec.GateTypeApproval {
					continue
				}
				if gate.Approvals == nil {
					if gate.Approvers != nil {
						legacy = true
					}
					continue
				}
				req := spec.ComposedRequirements{MinPermission: acc.minPermission, MemberOf: acc.memberOf}
				if found {
					c := acc.count
					req.Count = &c
				}
				acc = effectiveApprovals(gate.Approvals, req)
				found = true
			}
		}
	}
	// The conjunction is order-free; sort it so the recorded basis is
	// deterministic regardless of gate order.
	sort.Strings(acc.memberOf)
	return acc, legacy
}

// captainPredicateBasis renders a non-trivial fold as the recorded basis,
// e.g. "min_permission:admin" or "min_permission:write;member_of:org/a,org/b".
func captainPredicateBasis(a escalatedApprovals) string {
	var parts []string
	if a.minPermission != "" {
		parts = append(parts, "min_permission:"+a.minPermission)
	}
	if len(a.memberOf) > 0 {
		parts = append(parts, "member_of:"+strings.Join(a.memberOf, ","))
	}
	return strings.Join(parts, ";")
}
