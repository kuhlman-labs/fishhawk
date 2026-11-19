package server

// Waive proposals for server-check concerns (E80.28 / #5229, ADR-143
// Amendment A1): the propose half of propose-and-confirm, the chain fold every
// proposal reader and guard shares, and the recorded-head derivation the
// proposal binds to.
//
// A proposal is ADVISORY. An operator agent records its judgement, reason and
// evidence for a human to confirm or reject from the Web UI (the confirm and
// reject handlers live in waive_proposal_decide.go). The proposal clears
// nothing: the concern row is never touched here, so the merge gate and
// delegated settlement read it exactly as before.
//
// STATE lives on the run's audit chain, folded by loadWaiveProposalState from
// five categories (no migration, no table). Every proposal-state write goes
// through audit.GuardedChainAppender, whose guard re-runs the SAME fold and
// the SAME head derivation under the run-row lock, in the append's own
// transaction (V1). The out-of-lock pre-checks here are fast paths only; the
// in-lock guard is the control.
//
// THE FOLD (two passes, ascending sequence):
//
//  1. correctedIntent: every concern_waive_failed row names the
//     concern_waived intent it corrects, through its intent_sequence key, or,
//     for a legacy row written before that key existed, the newest
//     concern_waived for the same concern that precedes it.
//  2. Per concern_id, replayed in sequence order: a concern_waive_proposed row
//     becomes LIVE only when none is live, otherwise it is INERT and counted
//     (the guarded propose already serializes; this is defense in depth). A
//     concern_waive_proposal_rejected or _dropped row naming the live sequence
//     closes it. A concern_waived row whose proposal_sequence names the live
//     sequence closes it, and a LATER concern_waive_failed naming that intent
//     reopens it (V2): a failed waive disposition leaves the proposal live, so
//     the human can retry the confirm. The reopen happens only when the
//     concern has no live proposal at the corrective row: a proposal recorded
//     between the confirm's intent and its corrective row was acknowledged
//     (201) and keeps its place, so the failed confirm's proposal stays closed
//     (superseded) and a retry of it answers 409 waive_proposal_not_live
//     naming the newer sequence. No acknowledged proposal becomes inert.
//
// RESIDUAL (Y2): the concern_waived intent commits, and the decision-index
// decorator indexes it as a concern_waive decision row, BEFORE the
// disposition runs. So whenever a confirm's disposition fails, a concern_waive
// decision row exists for a concern that stayed open (a successful retry adds
// a second row for the same concern); the index does not read
// concern_waive_failed. The corrective concern_waive_failed write is best
// effort: when it lands the fold keeps the proposal live as above; when it
// fails too, the chain holds a linked intent with no recorded failure and the
// fold also closes the proposal. The confirm handler returns an error either
// way, never success. The remedy is a direct human waive or a new proposal;
// nothing is waived by mistake.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// The three waive-proposal run-chain categories (registered INTERNAL in
// audit.KnownCategories).
const (
	// CategoryConcernWaiveProposed records a pending proposal (actor = the
	// proposing operator agent).
	CategoryConcernWaiveProposed = "concern_waive_proposed"
	// CategoryConcernWaiveProposalRejected records a human's rejection (actor =
	// the human on a Web UI session). The concern stays open.
	CategoryConcernWaiveProposalRejected = "concern_waive_proposal_rejected"
	// CategoryConcernWaiveProposalDropped records a stale proposal's drop
	// (actor system, cause head_moved).
	CategoryConcernWaiveProposalDropped = "concern_waive_proposal_dropped"
)

// Payload keys shared by the fold (reader) and waive.go (writer), so the V2
// write-read contract has one definition.
const (
	waiveProposalKeySequence       = "proposal_sequence"
	waiveProposalKeyProposedBy     = "proposed_by"
	waiveProposalKeyProposedByKind = "proposed_by_actor_kind"
	waiveProposalKeyHeadSHA        = "proposal_head_sha"
	waiveFailedKeyIntentSequence   = "intent_sequence"
)

// waiveProposalDropCauseHeadMoved is the only drop cause today.
const waiveProposalDropCauseHeadMoved = "head_moved"

// waiveProposalNextStep is the 201 next_step: no agent path decides.
const waiveProposalNextStep = "The proposal is pending and clears nothing. A human signed in to the Fishhawk Web UI " +
	"confirms or rejects it from the attention queue's decision panel. There is no CLI or MCP confirm."

// waiveProposalLink binds a confirm's concern_waived intent to the proposal it
// confirms. applyConcernWaiveLinked stamps it on the payload only when non-nil,
// so a direct or bulk waive's payload is byte-identical to before.
type waiveProposalLink struct {
	ProposalSequence    int64
	ProposedBy          string
	ProposedByActorKind string
	ProposalHeadSHA     string
}

// waiveProposal is one folded proposal.
type waiveProposal struct {
	Sequence            int64
	ConcernID           uuid.UUID
	StageID             *uuid.UUID
	ProposedBy          string
	ProposedByActorKind string
	ProposedAt          time.Time
	Judgement           string
	TradeoffCost        string
	Reason              string
	Evidence            string
	HeadSHA             string
	CheckKey            string
	Category            string
	Severity            string
}

// waiveProposalState is the folded per-concern proposal state of one run.
type waiveProposalState struct {
	// live is the one live proposal per concern.
	live map[uuid.UUID]*waiveProposal
	// inert counts proposed rows that arrived while another was live.
	inert map[uuid.UUID]int
}

// Live returns the concern's live proposal, or nil.
func (st *waiveProposalState) Live(concernID uuid.UUID) *waiveProposal {
	if st == nil {
		return nil
	}
	return st.live[concernID]
}

// liveProposalMatches is the ONE sequence-match predicate the out-of-lock
// pre-checks and the in-lock guards share. It returns the concern's live
// proposal (nil when none) and whether it is the proposal named by seq.
func liveProposalMatches(st *waiveProposalState, concernID uuid.UUID, seq int64) (*waiveProposal, bool) {
	live := st.Live(concernID)
	return live, live != nil && seq > 0 && live.Sequence == seq
}

// waiveProposalChainPayload decodes the keys the fold reads from all five
// categories. Pointer integers keep absent distinct from zero.
type waiveProposalChainPayload struct {
	ConcernID        string `json:"concern_id"`
	ProposalSequence *int64 `json:"proposal_sequence"`
	IntentSequence   *int64 `json:"intent_sequence"`
	ProposedBy       string `json:"proposed_by"`
	Judgement        string `json:"judgement"`
	TradeoffCost     string `json:"tradeoff_cost"`
	Reason           string `json:"reason"`
	Evidence         string `json:"evidence"`
	HeadSHA          string `json:"head_sha"`
	CheckKey         string `json:"check_key"`
	Category         string `json:"category"`
	Severity         string `json:"severity"`
}

type waiveProposalFoldRow struct {
	entry     *audit.Entry
	payload   waiveProposalChainPayload
	concernID uuid.UUID
}

// waiveProposalFoldCategories are read in this order; the fold re-sorts by
// sequence, so the order is immaterial.
var waiveProposalFoldCategories = []string{
	CategoryConcernWaiveProposed,
	CategoryConcernWaiveProposalRejected,
	CategoryConcernWaiveProposalDropped,
	CategoryConcernWaived,
	CategoryConcernWaiveFailed,
}

// loadWaiveProposalState folds the run's waive-proposal state through read,
// which is the audit repository for an out-of-lock pre-check and the
// guarded append's tx-bound reader for the in-lock guard. A read error is
// returned (callers fail closed).
func loadWaiveProposalState(ctx context.Context, read audit.ChainReader, runID uuid.UUID) (*waiveProposalState, error) {
	var rows []waiveProposalFoldRow
	for _, cat := range waiveProposalFoldCategories {
		entries, err := read.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", cat, err)
		}
		for _, e := range entries {
			if e == nil {
				continue
			}
			var p waiveProposalChainPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			cid, perr := uuid.Parse(p.ConcernID)
			if perr != nil {
				continue
			}
			rows = append(rows, waiveProposalFoldRow{entry: e, payload: p, concernID: cid})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].entry.Sequence < rows[j].entry.Sequence })

	// Pass 1: the intent each concern_waive_failed row corrects, keyed by the
	// corrective row's own sequence.
	correctedIntent := map[int64]int64{}
	for i, r := range rows {
		if r.entry.Category != CategoryConcernWaiveFailed {
			continue
		}
		if r.payload.IntentSequence != nil {
			correctedIntent[r.entry.Sequence] = *r.payload.IntentSequence
			continue
		}
		// Legacy corrective row: the nearest preceding intent for the concern.
		for j := i - 1; j >= 0; j-- {
			prev := rows[j]
			if prev.entry.Category == CategoryConcernWaived && prev.concernID == r.concernID {
				correctedIntent[r.entry.Sequence] = prev.entry.Sequence
				break
			}
		}
	}

	// Pass 2: per-concern live proposal, replayed in sequence order. A linked
	// concern_waived intent closes the live proposal and parks it under the
	// intent's sequence; a later concern_waive_failed naming that intent
	// restores it ONLY when the concern has no live proposal at that point.
	// A proposal recorded while the intent awaited its outcome was
	// acknowledged (201) against a closed state, so it keeps its place and
	// the failed confirm's proposal stays closed (superseded).
	st := &waiveProposalState{live: map[uuid.UUID]*waiveProposal{}, inert: map[uuid.UUID]int{}}
	closedByIntent := map[int64]*waiveProposal{}
	for _, r := range rows {
		live := st.live[r.concernID]
		switch r.entry.Category {
		case CategoryConcernWaiveProposed:
			if live != nil {
				st.inert[r.concernID]++
				continue
			}
			st.live[r.concernID] = proposalFromRow(r)
		case CategoryConcernWaiveProposalRejected, CategoryConcernWaiveProposalDropped:
			if live != nil && r.payload.ProposalSequence != nil && *r.payload.ProposalSequence == live.Sequence {
				delete(st.live, r.concernID)
			}
		case CategoryConcernWaived:
			if live != nil && r.payload.ProposalSequence != nil && *r.payload.ProposalSequence == live.Sequence {
				delete(st.live, r.concernID)
				closedByIntent[r.entry.Sequence] = live
			}
		case CategoryConcernWaiveFailed:
			intent, ok := correctedIntent[r.entry.Sequence]
			if !ok {
				continue
			}
			p := closedByIntent[intent]
			if p == nil {
				continue
			}
			delete(closedByIntent, intent)
			if st.live[p.ConcernID] == nil {
				st.live[p.ConcernID] = p
			}
		}
	}
	return st, nil
}

func proposalFromRow(r waiveProposalFoldRow) *waiveProposal {
	p := &waiveProposal{
		Sequence:     r.entry.Sequence,
		ConcernID:    r.concernID,
		StageID:      r.entry.StageID,
		ProposedAt:   r.entry.Timestamp,
		Judgement:    r.payload.Judgement,
		TradeoffCost: r.payload.TradeoffCost,
		Reason:       r.payload.Reason,
		Evidence:     r.payload.Evidence,
		HeadSHA:      r.payload.HeadSHA,
		CheckKey:     r.payload.CheckKey,
		Category:     r.payload.Category,
		Severity:     r.payload.Severity,
	}
	if r.entry.ActorSubject != nil {
		p.ProposedBy = *r.entry.ActorSubject
	}
	if r.entry.ActorKind != nil {
		p.ProposedByActorKind = string(*r.entry.ActorKind)
	}
	return p
}

// recordedRunHead returns the run's newest CHAIN-RECORDED head through read:
// the head named by the highest-sequence row across
// gateViewCurrentHeadSources, or "" when the chain records none. It applies
// the same per-source rules as the gate view (an already_up_to_date
// branch_rebased moves no head; merge_commit_sha is the rebase fallback). A
// push Fishhawk never recorded is not seen; the ADR-090 merge gate reads the
// forge head.
func recordedRunHead(ctx context.Context, read audit.ChainReader, runID uuid.UUID) (string, error) {
	var head string
	var best int64 = -1
	for _, src := range gateViewCurrentHeadSources {
		entries, err := read.ListForRunByCategory(ctx, runID, src.category)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			var p map[string]any
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			if src.category == CategoryBranchRebased {
				if up, _ := p["already_up_to_date"].(bool); up {
					continue
				}
			}
			sha, _ := p[src.field].(string)
			if sha == "" && src.category == CategoryBranchRebased {
				sha, _ = p["merge_commit_sha"].(string)
			}
			if sha == "" || e.Sequence < best {
				continue
			}
			head, best = sha, e.Sequence
		}
	}
	return head, nil
}

// pendingWaiveProposals returns, per OPEN server_check row of rows, the live
// proposal ONLY when the current recorded head is non-empty and equals the
// proposal's head. A stale proposal is pending nowhere. A read error is
// returned (the read surfaces omit the block and record a gap).
func (s *Server) pendingWaiveProposals(ctx context.Context, runID uuid.UUID, rows []*concern.Concern) (map[uuid.UUID]*waiveProposal, error) {
	out := map[uuid.UUID]*waiveProposal{}
	hasOpen := false
	for _, r := range rows {
		if r != nil && r.IsServerCheck() && r.State.IsOpen() {
			hasOpen = true
			break
		}
	}
	if !hasOpen || s.cfg.AuditRepo == nil {
		return out, nil
	}
	st, err := loadWaiveProposalState(ctx, s.cfg.AuditRepo, runID)
	if err != nil {
		return nil, err
	}
	head, err := recordedRunHead(ctx, s.cfg.AuditRepo, runID)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return out, nil
	}
	for _, r := range rows {
		if r == nil || !r.IsServerCheck() || !r.State.IsOpen() {
			continue
		}
		if live := st.Live(r.ID); live != nil && live.HeadSHA == head {
			out[r.ID] = live
		}
	}
	return out, nil
}

// waiveProposalExistsError is the in-lock (and pre-check) refusal of a second
// live proposal for a concern.
type waiveProposalExistsError struct{ Sequence int64 }

func (e waiveProposalExistsError) Error() string {
	return fmt.Sprintf("a live waive proposal already exists for this concern (audit sequence %d)", e.Sequence)
}

// waiveProposalHeadMovedError is the in-lock refusal when the recorded run
// head is no longer the head the proposal was bound to.
type waiveProposalHeadMovedError struct{ Bound, Current string }

func (e waiveProposalHeadMovedError) Error() string {
	return fmt.Sprintf("the recorded run head moved from %s to %s", e.Bound, e.Current)
}

// errWaiveProposalDropSkipped is the drop guard's refusal when another actor
// already closed the proposal, or the head no longer matches what the payload
// would record. Callers skip the drop; it is not an error to the client.
var errWaiveProposalDropSkipped = errors.New("waive proposal drop skipped: proposal no longer live or head changed")

// errGuardedAppendUnavailable is returned when a guarded waive is requested
// and the audit repository lacks audit.GuardedChainAppender. Callers map it to
// 503 waive_proposal_atomic_append_unavailable: there is no non-atomic
// fallback (V1).
var errGuardedAppendUnavailable = errors.New("audit repository lacks the atomic guarded append (audit.GuardedChainAppender)")

// guardedAppender returns the repository's guarded-append capability.
func (s *Server) guardedAppender() (audit.GuardedChainAppender, bool) {
	ga, ok := s.cfg.AuditRepo.(audit.GuardedChainAppender)
	return ga, ok
}

// writeGuardedAppendUnavailable writes the fail-closed 503.
func (s *Server) writeGuardedAppendUnavailable(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, http.StatusServiceUnavailable, "waive_proposal_atomic_append_unavailable",
		"the audit store lacks the atomic guarded append this verb requires; nothing was recorded", nil)
}

// dropStaleWaiveProposal records concern_waive_proposal_dropped for live
// through the guarded path. The in-lock guard requires live to still be the
// concern's live proposal AND the recorded head to still equal currentHead
// (which differs from the proposal's head), so the payload is truthful.
// dropped=false with a nil error means another actor already closed it or the
// head changed again: the drop is skipped.
func (*Server) dropStaleWaiveProposal(ctx context.Context, ga audit.GuardedChainAppender, row *concern.Concern, live *waiveProposal, currentHead string) (dropped bool, err error) {
	payload, _ := json.Marshal(map[string]any{
		"concern_id":             row.ID.String(),
		waiveProposalKeySequence: live.Sequence,
		waiveProposalKeyHeadSHA:  live.HeadSHA,
		"current_head_sha":       currentHead,
		"cause":                  waiveProposalDropCauseHeadMoved,
	})
	systemKind := audit.ActorSystem
	guard := func(ctx context.Context, read audit.ChainReader) error {
		st, ferr := loadWaiveProposalState(ctx, read, row.RunID)
		if ferr != nil {
			return ferr
		}
		if _, ok := liveProposalMatches(st, row.ID, live.Sequence); !ok {
			return errWaiveProposalDropSkipped
		}
		head, herr := recordedRunHead(ctx, read, row.RunID)
		if herr != nil {
			return herr
		}
		if head == live.HeadSHA || head != currentHead {
			return errWaiveProposalDropSkipped
		}
		return nil
	}
	_, err = ga.AppendChainedGuarded(ctx, audit.ChainAppendParams{
		RunID:     row.RunID,
		StageID:   &row.StageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryConcernWaiveProposalDropped,
		ActorKind: &systemKind,
		Payload:   payload,
	}, guard)
	if errors.Is(err, errWaiveProposalDropSkipped) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// proposeWaiveRequest is the body of POST /v0/concerns/{concern_id}/waive-proposal.
type proposeWaiveRequest struct {
	Reason          string `json:"reason"`
	Evidence        string `json:"evidence"`
	Judgement       string `json:"judgement"`
	TradeoffCost    string `json:"tradeoff_cost"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
}

// waiveProposalView is the wire shape of one proposal (the 201 body's
// proposal, and the read surfaces' waive_proposal block). Reason, evidence and
// tradeoff cost are AGENT-AUTHORED and untrusted: agent_authored is always
// true, and a client renders them as text. The location and pattern class
// reach a human from the server-written check_key.
type waiveProposalView struct {
	ProposalSequence    int64     `json:"proposal_sequence"`
	ConcernID           string    `json:"concern_id"`
	StageID             string    `json:"stage_id,omitempty"`
	ProposedBy          string    `json:"proposed_by"`
	ProposedByActorKind string    `json:"proposed_by_actor_kind"`
	ProposedAt          time.Time `json:"proposed_at"`
	Judgement           string    `json:"judgement"`
	TradeoffCost        string    `json:"tradeoff_cost,omitempty"`
	Reason              string    `json:"reason"`
	Evidence            string    `json:"evidence"`
	HeadSHA             string    `json:"head_sha"`
	CheckKey            string    `json:"check_key"`
	Category            string    `json:"category,omitempty"`
	Severity            string    `json:"severity,omitempty"`
	AgentAuthored       bool      `json:"agent_authored"`
}

// view renders p on the wire.
func (p *waiveProposal) view() waiveProposalView {
	v := waiveProposalView{
		ProposalSequence:    p.Sequence,
		ConcernID:           p.ConcernID.String(),
		ProposedBy:          p.ProposedBy,
		ProposedByActorKind: p.ProposedByActorKind,
		ProposedAt:          p.ProposedAt,
		Judgement:           p.Judgement,
		TradeoffCost:        p.TradeoffCost,
		Reason:              p.Reason,
		Evidence:            p.Evidence,
		HeadSHA:             p.HeadSHA,
		CheckKey:            p.CheckKey,
		Category:            p.Category,
		Severity:            p.Severity,
		AgentAuthored:       true,
	}
	if p.StageID != nil {
		v.StageID = p.StageID.String()
	}
	return v
}

// proposeWaiveResponse is the 201 body. proposed_by and credential_kind are
// the acting-identity echo (#5227).
type proposeWaiveResponse struct {
	Proposal       waiveProposalView `json:"proposal"`
	ProposedBy     string            `json:"proposed_by"`
	CredentialKind string            `json:"credential_kind"`
	NextStep       string            `json:"next_step"`
}

// handleProposeWaive implements POST /v0/concerns/{concern_id}/waive-proposal
// (E80.28 / #5229, ADR-143 A1): an operator agent records a pending waive
// proposal on a server_check concern for a human to confirm or reject. It
// clears nothing and changes no gate.
//
// Every refusal comes before any append, in this order: 401; 403
// run_token_forbidden for a run-bound (mcp:run:) token; 403
// insufficient_scope; 503 concern_store_unconfigured; 400 validation_failed;
// the judgement rules; 400 waive_proposal_contains_credential; 404; 422
// concern_not_server_check; 409 concern_not_open; 503
// waive_proposal_atomic_append_unavailable; the head checks (500 on a read
// error, 409 waive_proposal_head_unresolved, 409 waive_proposal_head_moved);
// 409 waive_proposal_exists.
//
// X5(a): the run-bound refusal comes before the HANDLER'S OWN scope check and
// concern read. The route's requireConcernAccount middleware runs first and
// resolves the concern's run for the account check, so a caller from another
// account gets 403 account_forbidden there. An unknown concern id falls
// through to this handler, which refuses the run-bound token before reading
// anything, so the handler itself never lets the token probe concern ids (the
// acceptance_arbitration.go convention). The run's own agent is the party the
// check guards, so it may not propose its own waive.
func (s *Server) handleProposeWaive(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound token cannot propose a waive: the run's own agent is the party the server check guards (ADR-143 A1). "+
				"An operator agent or a human proposes it", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:fixups") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:fixups",
			map[string]any{"required_scope": "write:stages or write:fixups"})
		return
	}
	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"waive-proposal endpoint requires concern + audit repositories", nil)
		return
	}
	concernID, err := uuid.Parse(r.PathValue("concern_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_id must be a valid UUID",
			map[string]any{"field": "concern_id", "got": r.PathValue("concern_id")})
		return
	}
	var req proposeWaiveRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {reason, evidence, judgement, tradeoff_cost?, expected_head_sha?}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	for _, f := range []struct{ name, val string }{{"reason", req.Reason}, {"evidence", req.Evidence}} {
		if strings.TrimSpace(f.val) == "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				f.name+" is required: the human confirming the proposal reads it", map[string]any{"field": f.name})
			return
		}
		if len(f.val) > maxOperatorConcernBytes {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				fmt.Sprintf("%s is %d bytes; the maximum is %d (it is not truncated)", f.name, len(f.val), maxOperatorConcernBytes),
				map[string]any{"field": f.name, "bytes": len(f.val), "max": maxOperatorConcernBytes})
			return
		}
	}
	judgement, tradeoffCost, ok := s.concernJudgementFromRequest(w, r, req.Judgement, req.TradeoffCost, req.Reason)
	if !ok {
		return
	}
	// Credential scan (best effort: RedactDefault refuses only its
	// DefaultPatterns). The details name the field and pattern class only,
	// never the matched bytes.
	for _, f := range []struct{ name, val string }{{"reason", req.Reason}, {"evidence", req.Evidence}, {"tradeoff_cost", tradeoffCost}} {
		if _, hits := redaction.RedactDefault([]byte(f.val)); len(hits) > 0 {
			patterns := make([]string, 0, len(hits))
			for _, h := range hits {
				patterns = append(patterns, h.Pattern)
			}
			s.writeError(w, r, http.StatusBadRequest, "waive_proposal_contains_credential",
				fmt.Sprintf("%s contains credential-shaped text; describe the finding without pasting the matched bytes", f.name),
				map[string]any{"field": f.name, "patterns": patterns})
			return
		}
	}

	rows, err := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{concernID})
	if err != nil {
		if errors.Is(err, concern.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "concern_not_found", "no concern with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get concern failed", map[string]any{"error": err.Error()})
		return
	}
	row := rows[0]
	if !row.IsServerCheck() {
		s.writeError(w, r, http.StatusUnprocessableEntity, "concern_not_server_check",
			"only a server_check concern takes a waive proposal; a reviewer-raised concern is waived directly",
			map[string]any{"concern_id": row.ID.String(), "provenance": row.Provenance})
		return
	}
	if !row.State.IsOpen() {
		s.writeError(w, r, http.StatusConflict, "concern_not_open",
			fmt.Sprintf("concern %s is %s, not open; there is nothing to propose", row.ID, row.State),
			map[string]any{"concern_id": row.ID.String(), "state": string(row.State)})
		return
	}
	ga, ok := s.guardedAppender()
	if !ok {
		s.writeGuardedAppendUnavailable(w, r)
		return
	}

	ctx := r.Context()
	head, err := recordedRunHead(ctx, s.cfg.AuditRepo, row.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"reading the recorded run head failed; nothing was recorded", map[string]any{"error": err.Error()})
		return
	}
	if head == "" {
		s.writeError(w, r, http.StatusConflict, "waive_proposal_head_unresolved",
			"the run has no recorded head to bind a proposal to; nothing was recorded", nil)
		return
	}
	if req.ExpectedHeadSHA != "" && req.ExpectedHeadSHA != head {
		s.writeError(w, r, http.StatusConflict, "waive_proposal_head_moved",
			"the recorded run head is not the expected head; nothing was recorded",
			map[string]any{"expected_head_sha": req.ExpectedHeadSHA, "current_head_sha": head})
		return
	}

	// Pre-fold (fast path; the in-lock guard is the control).
	st, err := loadWaiveProposalState(ctx, s.cfg.AuditRepo, row.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"reading waive-proposal state failed; nothing was recorded", map[string]any{"error": err.Error()})
		return
	}
	if live := st.Live(row.ID); live != nil {
		if live.HeadSHA == head {
			s.writeWaiveProposalExists(w, r, row, live.Sequence)
			return
		}
		if _, derr := s.dropStaleWaiveProposal(ctx, ga, row, live, head); derr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
				"recording the stale proposal's drop failed; nothing was proposed", map[string]any{"error": derr.Error()})
			return
		}
	}

	subject := id.Subject
	actorKind := actorKindForSubject(subject)
	fields := map[string]any{
		"concern_id": row.ID.String(),
		"stage_id":   row.StageID.String(),
		"stage_kind": row.StageKind,
		"provenance": row.Provenance,
		"check_key":  row.CheckKey,
		"category":   row.Category,
		"severity":   row.Severity,
		"reason":     req.Reason,
		"judgement":  string(judgement),
		"evidence":   req.Evidence,
		"head_sha":   head,
	}
	if tradeoffCost != "" {
		fields["tradeoff_cost"] = tradeoffCost
	}
	payload, _ := json.Marshal(fields)
	guard := func(ctx context.Context, read audit.ChainReader) error {
		gst, ferr := loadWaiveProposalState(ctx, read, row.RunID)
		if ferr != nil {
			return ferr
		}
		if live := gst.Live(row.ID); live != nil {
			return waiveProposalExistsError{Sequence: live.Sequence}
		}
		cur, herr := recordedRunHead(ctx, read, row.RunID)
		if herr != nil {
			return herr
		}
		if cur != head {
			return waiveProposalHeadMovedError{Bound: head, Current: cur}
		}
		return nil
	}
	now := time.Now().UTC()
	entry, err := ga.AppendChainedGuarded(ctx, audit.ChainAppendParams{
		RunID:        row.RunID,
		StageID:      &row.StageID,
		Timestamp:    now,
		Category:     CategoryConcernWaiveProposed,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}, guard)
	if err != nil {
		var exists waiveProposalExistsError
		if errors.As(err, &exists) {
			s.writeWaiveProposalExists(w, r, row, exists.Sequence)
			return
		}
		var moved waiveProposalHeadMovedError
		if errors.As(err, &moved) {
			s.writeError(w, r, http.StatusConflict, "waive_proposal_head_moved",
				"the recorded run head moved while the proposal was being recorded; nothing was recorded",
				map[string]any{"expected_head_sha": moved.Bound, "current_head_sha": moved.Current})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
			"appending the concern_waive_proposed entry failed; nothing was proposed", map[string]any{"error": err.Error()})
		return
	}

	stageID := row.StageID
	prop := &waiveProposal{
		Sequence:            entrySequence(entry),
		ConcernID:           row.ID,
		StageID:             &stageID,
		ProposedBy:          subject,
		ProposedByActorKind: string(actorKind),
		ProposedAt:          now,
		Judgement:           string(judgement),
		TradeoffCost:        tradeoffCost,
		Reason:              req.Reason,
		Evidence:            req.Evidence,
		HeadSHA:             head,
		CheckKey:            row.CheckKey,
		Category:            row.Category,
		Severity:            row.Severity,
	}
	s.writeJSON(w, r, http.StatusCreated, proposeWaiveResponse{
		Proposal:       prop.view(),
		ProposedBy:     subject,
		CredentialKind: approvalCredentialKind(id),
		NextStep:       waiveProposalNextStep,
	})
}

// writeWaiveProposalExists writes 409 waive_proposal_exists naming the live
// proposal's sequence.
func (s *Server) writeWaiveProposalExists(w http.ResponseWriter, r *http.Request, row *concern.Concern, seq int64) {
	s.writeError(w, r, http.StatusConflict, "waive_proposal_exists",
		fmt.Sprintf("concern %s already has a live waive proposal (audit sequence %d); a human confirms or rejects it first", row.ID, seq),
		map[string]any{"concern_id": row.ID.String(), "proposal_sequence": seq})
}
