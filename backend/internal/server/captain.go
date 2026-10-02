package server

// The captain record surface (E76.2 / #3765, ADR-083 #3751 rules 2-5). Six
// routes over backend/internal/captain:
//
//   - GET  /v0/captain?repo=          the derived record: current captain,
//     pending handover offer, last captain and the chain history. Never writes.
//   - POST /v0/captain/offer          the sitting captain offers the seat;
//     the handover brief is composed first and its brief_hash + window (or a
//     brief_unavailable marker) recorded on the offer (handover_brief.go).
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
//
// The GET body and every verb's 200 body carry a delegation_unconfirmed
// block (E76.5 / #3768, ADR-083 rule 7) when the delegation-confirmation
// store is wired: the workflows with no counted confirmation since the latest
// seat change. It is CHAIN-ONLY — it never reports hash staleness, which only
// the delegation-confirmation reads do (captainDelegationUnconfirmed).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
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

// The captain-resolution bases every consumer of currentCaptain branches on
// (E76.3 / #3766, ADR-083 rule 6). They are the issuecomment package's
// values, so the notifier seam and the server helper cannot drift apart.
const (
	captainBasisCaptain     = issuecomment.CaptainBasisCaptain
	captainBasisVacant      = issuecomment.CaptainBasisVacant
	captainBasisUnavailable = issuecomment.CaptainBasisUnavailable
)

// currentCaptain is the ONE shared read of a repository's current captain
// for every consumer that enriches a surface with it (the issue-comment page
// and anchor here; the digest watermark key, the delegated-approval default
// principal and the gate-view note in the sibling E76.3 slices; E60.3 #2292
// and E77.6 #3740 after them). It folds the outcome into an explicit
// trichotomy:
//
//   - captainBasisCaptain — a seat is held; subject and identityVerified are
//     the derived record's.
//   - captainBasisVacant — the record read fine and State.Current is nil.
//   - captainBasisUnavailable — CaptainStore is not wired, or the read
//     failed (logged at WARN, never returned). An unavailable read is never
//     reported as a vacancy.
//
// accountID is EXPLICIT (nil = the untenanted partition): a caller serving
// a request passes identityAccountID(ctx), while the notifier passes the
// RUN's account, because it fires from transition hooks whose ctx carries no
// request identity. It never errors — every consumer is a best-effort
// enrichment that must degrade to its pre-E76.3 behaviour, never fail. The
// result is NEVER an input to approval quorum or eligibility (ADR-083 rule 1).
func (s *Server) currentCaptain(ctx context.Context, accountID *uuid.UUID, repo string) (subject string, identityVerified bool, basis string) {
	if s.cfg.CaptainStore == nil {
		return "", false, captainBasisUnavailable
	}
	snap, err := s.cfg.CaptainStore.Read(ctx, accountID, repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "captain: current-captain read failed; consumer degrades to unaddressed",
			slog.String("repo", repo), slog.String("error", err.Error()))
		return "", false, captainBasisUnavailable
	}
	if snap == nil || snap.State.Current == nil {
		return "", false, captainBasisVacant
	}
	return snap.State.Current.Subject, snap.State.Current.IdentityVerified, captainBasisCaptain
}

// issueCommentCaptainResolver adapts currentCaptain to the issuecomment
// notifier's CaptainResolver seam, or returns nil when CaptainStore is not
// wired so the notifier is constructed exactly as before E76.3. The run's
// account id arrives as a string: "" is the untenanted partition, and a
// non-empty value that is not a UUID resolves UNAVAILABLE rather than
// silently reading the untenanted partition.
func (s *Server) issueCommentCaptainResolver() issuecomment.CaptainResolver {
	if s.cfg.CaptainStore == nil {
		return nil
	}
	return func(ctx context.Context, accountID, repo string) issuecomment.CaptainResolution {
		var acct *uuid.UUID
		if accountID != "" {
			u, err := uuid.Parse(accountID)
			if err != nil {
				return issuecomment.CaptainResolution{Basis: captainBasisUnavailable}
			}
			acct = &u
		}
		subject, verified, basis := s.currentCaptain(ctx, acct, repo)
		return issuecomment.CaptainResolution{Subject: subject, IdentityVerified: verified, Basis: basis}
	}
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
// IdentityVerified is the SUCCESSOR's provider qualification. The brief_*
// fields are the handover brief the offer recorded (E76.4 / #3767): a hash
// plus window, or brief_unavailable with a reason; all empty for an offer
// written before E76.4.
type captainOfferResponse struct {
	Successor              string    `json:"successor"`
	IdentityVerified       bool      `json:"identity_verified"`
	OfferedBy              string    `json:"offered_by"`
	OfferEntryHash         string    `json:"offer_entry_hash"`
	OfferedSequence        int64     `json:"offered_sequence"`
	OfferedAt              time.Time `json:"offered_at"`
	BriefHash              string    `json:"brief_hash,omitempty"`
	BriefFromSequence      int64     `json:"brief_from_sequence,omitempty"`
	BriefToSequence        int64     `json:"brief_to_sequence,omitempty"`
	BriefUnavailable       bool      `json:"brief_unavailable,omitempty"`
	BriefUnavailableReason string    `json:"brief_unavailable_reason,omitempty"`
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
	// DelegationUnconfirmed is absent when the delegation-confirmation store
	// is not wired (never an empty list that would read as "all confirmed").
	DelegationUnconfirmed *captainDelegationUnconfirmed `json:"delegation_unconfirmed,omitempty"`
}

// captainVerbResponse is every POST verb's 200 body: the recorded event and
// the record re-derived with it folded in.
type captainVerbResponse struct {
	Repo         string                 `json:"repo"`
	Event        captainHistoryItem     `json:"event"`
	Captain      *captainRecordResponse `json:"captain"`
	PendingOffer *captainOfferResponse  `json:"pending_offer"`
	// DelegationUnconfirmed is re-derived AFTER the verb's entry commits, so
	// an accept's response already lists every workflow the new captain has
	// yet to confirm.
	DelegationUnconfirmed *captainDelegationUnconfirmed `json:"delegation_unconfirmed,omitempty"`
}

// The inventory_unavailable reasons of captainDelegationUnconfirmed. Each
// names why the workflow SET could not be read, so an empty workflows list is
// never mistaken for "every workflow is confirmed".
const (
	captainDelegationNoRunRepository = "no_run_repository"
	captainDelegationListRunsFailed  = "list_runs_failed"
	captainDelegationNoCachedSpec    = "no_cached_spec"
	captainDelegationSpecUnparseable = "spec_unparseable"
	captainDelegationChainReadFailed = "chain_read_failed"
)

// captainDelegationUnconfirmed is the hand-off surface's delegation block
// (E76.5 / #3768, ADR-083 rule 7).
//
// Workflows lists every workflow with NO counted delegation_confirmed since
// the latest seat change (captain_assigned or captain_claimed), never-
// confirmed ones included. The verdict is CHAIN-ONLY (delegationconfirm.
// Derive + Statuses): HashStalenessReported is always false, and a workflow's
// ABSENCE here is not proof its confirmation is still hash-current — only
// GET .../delegation/confirmation and GET .../delegation report hash_stale.
//
// The workflow SET is the delegation view's, projected from the spec cached
// on the repository's NEWEST run (Source is always run_cache): the hand-off
// surface never fetches the spec live from the forge, the
// classifyRepoPredicate precedent. InventoryUnavailable names why the set
// could not be read (the list is then empty and must not be read as
// confirmed); Unavailable names a chain read failure.
type captainDelegationUnconfirmed struct {
	SeatSequence int64 `json:"seat_sequence"`
	// UnconfirmedSince is the timestamp of the seat-change entry at
	// SeatSequence — when the handover the incoming captain answers occurred.
	// Absent (null) when no captain has ever sat.
	UnconfirmedSince      *time.Time `json:"unconfirmed_since,omitempty"`
	Source                string     `json:"source"`
	WorkflowSHA           string     `json:"workflow_sha,omitempty"`
	Workflows             []string   `json:"workflows"`
	HashStalenessReported bool       `json:"hash_staleness_reported"`
	InventoryUnavailable  string     `json:"inventory_unavailable,omitempty"`
	Unavailable           string     `json:"unavailable,omitempty"`
}

// captainDelegationUnconfirmed builds the block for repo, or nil when the
// delegation-confirmation store is not wired. It never fails the caller: a
// chain or inventory read failure is reported IN the block.
func (s *Server) captainDelegationUnconfirmed(ctx context.Context, repo string) *captainDelegationUnconfirmed {
	if s.cfg.DelegationConfirmStore == nil {
		return nil
	}
	out := &captainDelegationUnconfirmed{Source: delegationSourceRunCache, Workflows: []string{}}
	snap, err := s.cfg.DelegationConfirmStore.Read(ctx, identityAccountID(ctx), repo)
	if err != nil {
		// A fixed reason, not err.Error(): the block rides a 200 body.
		out.Unavailable = captainDelegationChainReadFailed
		return out
	}
	out.SeatSequence = snap.State.SeatSequence
	if !snap.State.SeatAt.IsZero() {
		at := snap.State.SeatAt
		out.UnconfirmedSince = &at
	}
	ids, sha, reason := s.captainDelegationInventory(ctx, repo)
	if reason != "" {
		out.InventoryUnavailable = reason
		return out
	}
	out.WorkflowSHA = sha
	out.Workflows = delegationconfirm.Unconfirmed(delegationconfirm.Statuses(snap.State, ids))
	return out
}

// captainDelegationInventory returns the delegation view's workflow ids from
// the spec cached on repo's newest run, or a non-empty reason when that set
// cannot be read. It reads no forge.
func (s *Server) captainDelegationInventory(ctx context.Context, repo string) ([]string, string, string) {
	if s.cfg.RunRepo == nil {
		return nil, "", captainDelegationNoRunRepository
	}
	rows, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo: repo, AccountID: IdentityFrom(ctx).AccountID, Limit: 1,
	})
	if err != nil {
		return nil, "", captainDelegationListRunsFailed
	}
	if len(rows) == 0 || len(bytes.TrimSpace(rows[0].WorkflowSpec)) == 0 {
		return nil, "", captainDelegationNoCachedSpec
	}
	parsed, err := spec.ParseBytes(rows[0].WorkflowSpec)
	if err != nil {
		return nil, "", captainDelegationSpecUnparseable
	}
	projected := delegationview.Project(parsed)
	ids := make([]string, 0, len(projected))
	for _, wf := range projected {
		ids = append(ids, wf.ID)
	}
	return ids, rows[0].WorkflowSHA, ""
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
		Successor:              o.Successor,
		IdentityVerified:       o.SuccessorIdentityVerified,
		OfferedBy:              o.OfferedBy,
		OfferEntryHash:         o.EntryHash,
		OfferedSequence:        o.Sequence,
		OfferedAt:              o.OfferedAt,
		BriefHash:              o.Brief.Hash,
		BriefFromSequence:      o.Brief.FromSequence,
		BriefToSequence:        o.Brief.ToSequence,
		BriefUnavailable:       o.Brief.Unavailable,
		BriefUnavailableReason: o.Brief.UnavailableReason,
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
	out := renderCaptainSnapshot(repo, snap)
	out.DelegationUnconfirmed = s.captainDelegationUnconfirmed(r.Context(), repo)
	s.writeJSON(w, r, http.StatusOK, out)
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
	if verb.name == captainVerbOffer.name {
		// The handover brief (E76.4 / #3767) is composed HERE, before Apply:
		// composition does I/O and Apply's decide callback runs inside the
		// advisory-locked transaction. An unestablishable brief is recorded
		// as brief_unavailable with a reason, never a refusal.
		params.Brief = s.composeOfferBrief(ctx, req.Repo, subject, req.Successor)
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
		Captain:               renderCaptainRecord(applied.State.Current),
		PendingOffer:          renderCaptainOffer(applied.State.PendingOffer),
		DelegationUnconfirmed: s.captainDelegationUnconfirmed(ctx, req.Repo),
	})
}

// captainActorIsAgent reports whether subject is an agent identity: the
// operator-agent token family or a run-bound MCP token subject, which belongs
// to an agent run. It delegates to isAgentSubject, the classification the
// human-only server-check clearing guard shares, so the two cannot drift. It
// only CLASSIFIES; the refusal is the captain package's guardActor.
func captainActorIsAgent(subject string) bool {
	return isAgentSubject(subject)
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
