package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Precedent at the gate (E75.4 / #3732, ADR-082 #3728 decision (c), rules 2
// and 6). When a HUMAN gate is open on a run, the captain-facing read surfaces
// (the gate view and the single-run read) carry a BOUNDED precedent block for
// that gate, computed through the SAME query GET /v0/precedent runs
// (runPrecedentQuery), and the chain records exactly what was surfaced as one
// `precedent_surfaced` entry.
//
// Precedent is DISPLAY-ONLY. It is never a gate input (no gate state, allowed
// action or transition reads it) and never an agent input (no prompt renders
// it). It reports how this kind of gate was decided before; the decision is
// still the captain's.
//
// GATE-OPEN IS READ-TIME. There is no single seam where a human gate opens —
// awaiting_approval / awaiting_scope_decision are entered from a dozen sites —
// so the block is computed when a captain-facing surface READS a run whose gate
// is open, and the fingerprint de-duplication is what makes "record once" hold
// across repeated reads.
//
// Long-form contract: backend/internal/server/README.md § "Precedent at the
// gate".

// CategoryPrecedentSurfaced is the audit category recording one surfaced gate
// precedent block. INTERNAL: it is chain-only and never its own issue-thread
// activity line.
const CategoryPrecedentSurfaced = "precedent_surfaced"

// gatePrecedentMaxItems bounds the cited items in one gate block. The full
// ranked set stays one call away through the block's full_query pointer.
const gatePrecedentMaxItems = 3

// gatePrecedentTool is the MCP tool the full_query pointer names.
const gatePrecedentTool = "fishhawk_precedent"

// gatePrecedentGate is the resolved open gate: which decision class is
// pending, where it will be recorded, and the ranking signals it carries.
type gatePrecedentGate struct {
	Class decisionindex.DecisionClass
	// GateStageID is the stage whose gate is open — the stage the
	// precedent_surfaced entry is recorded on and de-duplicated by.
	GateStageID uuid.UUID
	// ContextStageID is the stage the ranking context (touched paths, fired
	// escalation keys) is derived from: the stage the pending decision itself
	// will be recorded on. It differs from GateStageID only for concern_waive,
	// whose waiver lands on the CONCERN's own stage.
	ContextStageID uuid.UUID
	// StageKind is the hard stage-kind filter for the candidate window, named
	// per arm rather than taken from the gate stage's type: a
	// merge_verdict_recorded entry carries NO stage (so its index rows have an
	// empty stage kind and a "review" filter would match none of them), and a
	// concern waiver's row carries the concern's stage kind, not the review
	// stage's.
	StageKind       string
	ConcernCategory string
	Severity        string
	// AlternateClass names a sibling decision class the captain can also take
	// at this gate, reachable only through full_query (concern_defer beside
	// concern_waive). One gate yields ONE class: Summarize's modal-outcome share
	// is defined over one class's outcome vocabulary, so merging classes would
	// make the agreement ratio uninterpretable.
	AlternateClass decisionindex.DecisionClass
}

// gatePrecedentClass is the PURE, deterministic gate-class ladder. First match
// wins:
//
//  1. any stage in awaiting_scope_decision            -> scope_amendment
//  2. the plan stage in awaiting_approval             -> plan_approval
//  3. a review stage in awaiting_approval, >=1 OPEN
//     concern (concernsKnown)                         -> concern_waive, with the
//     newest open concern's normalized category + severity as ranking signals
//  4. a review stage in awaiting_approval, no open
//     concern (concernsKnown)                         -> merge_verdict
//  5. anything else                                   -> ok=false (no human gate)
//
// When concernsKnown is false (the concern read failed) arms 3 and 4 cannot be
// told apart, so a review gate yields ok=false rather than a guessed class.
func gatePrecedentClass(stages []*run.Stage, concerns []*concern.Concern, concernsKnown bool) (gatePrecedentGate, bool) {
	for _, st := range stages {
		if st != nil && st.State == run.StageStateAwaitingScopeDecision {
			return gatePrecedentGate{
				Class:          decisionindex.ClassScopeAmendment,
				GateStageID:    st.ID,
				ContextStageID: st.ID,
				StageKind:      string(st.Type),
			}, true
		}
	}
	for _, st := range stages {
		if st != nil && st.Type == run.StageTypePlan && st.State == run.StageStateAwaitingApproval {
			return gatePrecedentGate{
				Class:          decisionindex.ClassPlanApproval,
				GateStageID:    st.ID,
				ContextStageID: st.ID,
				StageKind:      string(run.StageTypePlan),
			}, true
		}
	}
	for _, st := range stages {
		if st == nil || st.Type != run.StageTypeReview || st.State != run.StageStateAwaitingApproval {
			continue
		}
		if !concernsKnown {
			return gatePrecedentGate{}, false
		}
		if newest := newestOpenConcern(concerns); newest != nil {
			category, _ := decisionindex.NormalizeConcernCategory(newest.Category)
			return gatePrecedentGate{
				Class:           decisionindex.ClassConcernWaive,
				GateStageID:     st.ID,
				ContextStageID:  newest.StageID,
				StageKind:       newest.StageKind,
				ConcernCategory: category,
				Severity:        newest.Severity,
				AlternateClass:  decisionindex.ClassConcernDefer,
			}, true
		}
		return gatePrecedentGate{
			Class:          decisionindex.ClassMergeVerdict,
			GateStageID:    st.ID,
			ContextStageID: st.ID,
			// merge_verdict_recorded carries no stage: see StageKind's doc.
			StageKind: "",
		}, true
	}
	return gatePrecedentGate{}, false
}

// newestOpenConcern returns the open concern with the highest origin review
// sequence (created_at, then id, as tie-breaks so the choice is total), or nil.
func newestOpenConcern(concerns []*concern.Concern) *concern.Concern {
	var best *concern.Concern
	for _, c := range concerns {
		if c == nil || !c.State.IsOpen() {
			continue
		}
		if best == nil || newerConcern(c, best) {
			best = c
		}
	}
	return best
}

func newerConcern(a, b *concern.Concern) bool {
	if a.OriginReviewSequence != b.OriginReviewSequence {
		return a.OriginReviewSequence > b.OriginReviewSequence
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID.String() > b.ID.String()
}

// gatePrecedentBlock is the bounded precedent block on the gate view and the
// single-run read. DISPLAY-ONLY: never authority, never a gate input, never an
// agent input.
type gatePrecedentBlock struct {
	DecisionClass string `json:"decision_class"`
	// StageID is the stage whose gate is open.
	StageID string `json:"stage_id"`
	// StageKind is the hard stage-kind filter the candidate window used
	// (empty = any, e.g. merge_verdict).
	StageKind    string `json:"stage_kind,omitempty"`
	IndexVersion string `json:"index_version"`
	// Fingerprint is precedent.Fingerprint over this block — the key the
	// precedent_surfaced entry is de-duplicated by.
	Fingerprint string            `json:"fingerprint"`
	Items       []precedent.Item  `json:"items"`
	Summary     precedent.Summary `json:"summary"`
	// Truncated is true when the candidate window was full.
	Truncated bool                `json:"truncated"`
	Degraded  []precedentDegraded `json:"degraded"`
	FullQuery gatePrecedentQuery  `json:"full_query"`
}

// gatePrecedentQuery points at the full, unbounded precedent query for this
// gate.
type gatePrecedentQuery struct {
	// Endpoint is the ready-to-issue REST path.
	Endpoint      string `json:"endpoint"`
	Tool          string `json:"tool"`
	DecisionClass string `json:"decision_class"`
	Repo          string `json:"repo"`
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id"`
	// AlternateDecisionClass names a sibling class the captain can also
	// decide at this gate (concern_defer beside concern_waive); re-run the
	// query with it to see that class's precedent.
	AlternateDecisionClass string `json:"alternate_decision_class,omitempty"`
}

// precedentSurfacedPayload is the precedent_surfaced audit payload: exactly
// what was surfaced, by CITATION. It carries NO reason prose — ADR-082 rule 1
// keeps decision prose on its own chain entry, and copying a query-time
// excerpt into a new entry would duplicate it.
type precedentSurfacedPayload struct {
	DecisionClass string                      `json:"decision_class"`
	StageID       string                      `json:"stage_id"`
	StageKind     string                      `json:"stage_kind"`
	IndexVersion  string                      `json:"index_version"`
	Fingerprint   string                      `json:"fingerprint"`
	Cited         []precedentSurfacedCitation `json:"cited"`
	Summary       precedent.Summary           `json:"summary"`
	Degraded      []precedentSurfacedDegraded `json:"degraded"`
}

// precedentSurfacedCitation is one cited decision in the audit payload.
type precedentSurfacedCitation struct {
	SourceSequence  int64                     `json:"source_sequence"`
	SourceEntryHash string                    `json:"source_entry_hash"`
	Outcome         string                    `json:"outcome,omitempty"`
	Score           precedent.ScoreComponents `json:"score"`
	MatchedKeys     precedent.MatchedKeys     `json:"matched_keys"`
}

// precedentSurfacedDegraded is a degradation as recorded on the chain: the
// reason and the cited coordinates only. The live surface's free-text detail
// is omitted because it can carry an error string, not decision evidence.
type precedentSurfacedDegraded struct {
	Reason         string `json:"reason"`
	SourceSequence int64  `json:"source_sequence,omitempty"`
	RunID          string `json:"run_id,omitempty"`
}

// gatePrecedentFor computes the gate precedent block for ru and records it.
//
// BEST-EFFORT AND TOTAL. Each of these returns nil with NO audit entry, leaving
// the response byte-identical to a pre-E75.4 one: PrecedentIndex unwired, the
// stage read fails, no human gate is open, the run's account id is not a UUID,
// the GateContext resolve fails, the index List fails, or the repository has
// zero indexed decisions of the gate's class. A warn log names each failure.
//
// The run's OWN account scopes the read (the captain block's partition rule):
// precedent for a run's gate comes from that run's tenant, and an untenanted
// run reads only untenanted rows (ListFilter.AccountScoped).
func (s *Server) gatePrecedentFor(ctx context.Context, ru *run.Run, concerns []*concern.Concern, concernsKnown bool) *gatePrecedentBlock {
	if ru == nil || s.cfg.PrecedentIndex == nil || s.cfg.RunRepo == nil {
		return nil
	}
	warn := func(msg string, err error) {
		attrs := []slog.Attr{slog.String("run_id", ru.ID.String())}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gate precedent: "+msg+"; omitting the precedent block", attrs...)
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, ru.ID)
	if err != nil {
		warn("list stages failed", err)
		return nil
	}
	gate, ok := gatePrecedentClass(stages, concerns, concernsKnown)
	if !ok {
		return nil
	}
	var acct *uuid.UUID
	if ru.AccountID != "" {
		u, perr := uuid.Parse(ru.AccountID)
		if perr != nil {
			warn("run account id is not a UUID", perr)
			return nil
		}
		acct = &u
	}
	ctxStage := gate.ContextStageID
	gc, err := s.cfg.PrecedentIndex.GateContext(ctx, decisionindex.GateRef{
		RunID: ru.ID, StageID: &ctxStage, AccountID: acct,
	})
	if err != nil {
		warn("resolve gate context failed", err)
		return nil
	}
	pctx := precedent.Context{
		Repo:            gc.Repo,
		DecisionClass:   gate.Class,
		StageKind:       gate.StageKind,
		TouchedPaths:    gc.TouchedPaths,
		ConcernCategory: gate.ConcernCategory,
		Severity:        gate.Severity,
		EscalationKeys:  gc.EscalationKeys,
	}
	if pctx.Repo == "" {
		pctx.Repo = ru.Repo
	}
	res, err := s.runPrecedentQuery(ctx, pctx, acct, gatePrecedentMaxItems)
	if err != nil {
		warn("list decision index failed", err)
		return nil
	}
	if res.Candidates == 0 || len(res.Items) == 0 {
		return nil
	}

	class := string(gate.Class)
	stageID := gate.GateStageID.String()
	block := &gatePrecedentBlock{
		DecisionClass: class,
		StageID:       stageID,
		StageKind:     gate.StageKind,
		IndexVersion:  precedent.IndexVersion,
		Fingerprint:   precedent.Fingerprint(class, stageID, res.Items, res.Summary),
		Items:         res.Items,
		Summary:       res.Summary,
		Truncated:     res.Truncated,
		Degraded:      res.Degraded,
		FullQuery: gatePrecedentQuery{
			Endpoint: "GET /v0/precedent?decision_class=" + class + "&run_id=" + ru.ID.String() +
				"&stage_id=" + gate.ContextStageID.String(),
			Tool:                   gatePrecedentTool,
			DecisionClass:          class,
			Repo:                   pctx.Repo,
			RunID:                  ru.ID.String(),
			StageID:                gate.ContextStageID.String(),
			AlternateDecisionClass: string(gate.AlternateClass),
		},
	}
	s.recordPrecedentSurfaced(ctx, ru.ID, gate.GateStageID, block)
	return block
}

// recordPrecedentSurfaced appends ONE precedent_surfaced entry for block unless
// an entry with the same fingerprint AND stage already exists on the run.
//
// BEST-EFFORT: the surface never depends on the write. A nil audit repository
// records nothing; an APPEND failure is warn-logged and swallowed.
//
// DE-DUPLICATION IS READ-THEN-APPEND AND IS NOT ATOMIC. Two CONCURRENT first
// reads of the same gate can both see "absent" and both append, producing a
// rare duplicate precedent_surfaced entry. That is the same accepted posture as
// escalationAlreadyAudited: a duplicate over-reports a surfacing that genuinely
// happened, and a uniqueness constraint to prevent it would be a schema change
// out of proportion to the defect. "Exactly one entry per unchanged gate" is a
// SEQUENTIAL-read guarantee, not a uniqueness guarantee.
//
// A de-duplication READ failure EMITS anyway (fail toward visibility): a
// duplicate entry is benign, a missing one loses the record of what the captain
// was shown.
func (s *Server) recordPrecedentSurfaced(ctx context.Context, runID, stageID uuid.UUID, block *gatePrecedentBlock) {
	if s.cfg.AuditRepo == nil || block == nil {
		return
	}
	if s.precedentAlreadySurfaced(ctx, runID, block.StageID, block.Fingerprint) {
		return
	}
	p := precedentSurfacedPayload{
		DecisionClass: block.DecisionClass,
		StageID:       block.StageID,
		StageKind:     block.StageKind,
		IndexVersion:  block.IndexVersion,
		Fingerprint:   block.Fingerprint,
		Cited:         make([]precedentSurfacedCitation, 0, len(block.Items)),
		Summary:       block.Summary,
		Degraded:      make([]precedentSurfacedDegraded, 0, len(block.Degraded)),
	}
	for _, it := range block.Items {
		// ReasonExcerpt is deliberately NOT copied (ADR-082 rule 1).
		p.Cited = append(p.Cited, precedentSurfacedCitation{
			SourceSequence:  it.SourceSequence,
			SourceEntryHash: it.SourceEntryHash,
			Outcome:         it.Outcome,
			Score:           it.Score,
			MatchedKeys:     it.MatchedKeys,
		})
	}
	for _, d := range block.Degraded {
		p.Degraded = append(p.Degraded, precedentSurfacedDegraded{
			Reason: d.Reason, SourceSequence: d.SourceSequence, RunID: d.RunID,
		})
	}
	payload, err := json.Marshal(p)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gate precedent: marshal precedent_surfaced payload failed; the block is still surfaced",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return
	}
	actorKind := audit.ActorSystem
	sid := stageID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &sid,
		Timestamp: time.Now().UTC(),
		Category:  CategoryPrecedentSurfaced,
		ActorKind: &actorKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gate precedent: precedent_surfaced audit append failed; the block is still surfaced",
			slog.String("run_id", runID.String()),
			slog.String("fingerprint", block.Fingerprint),
			slog.String("error", err.Error()))
	}
}

// precedentAlreadySurfaced reports whether a precedent_surfaced entry with this
// stage AND fingerprint already exists on the run. A read failure returns false
// — emit anyway, per recordPrecedentSurfaced's fail-toward-visibility note
// (mirrors escalationAlreadyAudited).
func (s *Server) precedentAlreadySurfaced(ctx context.Context, runID uuid.UUID, stageID, fingerprint string) bool {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryPrecedentSurfaced)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gate precedent: precedent_surfaced de-duplication read failed; emitting anyway",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return false
	}
	for _, e := range entries {
		var p precedentSurfacedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Fingerprint == fingerprint && p.StageID == stageID {
			return true
		}
	}
	return false
}
