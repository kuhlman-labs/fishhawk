package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
)

// Divergence detection at the gate (E75.5 / #3733, ADR-082 #3728 decision (d)
// and rule 5). After a captain's decision at an allow-listed gate is DURABLY
// recorded — a concern waive, a concern defer, or a plan reject — the server
// re-runs the precedent query for that decision (the SAME runPrecedentQuery
// the gate block and GET /v0/precedent use) and evaluates precedent.Decide.
// When, and only when, the verdict is diverged, it appends ONE
// precedent_divergence entry: the record a later surface turns into the
// optional "one-off, or a change of doctrine?" question.
//
// It is never authority and never a gate input: it runs after the decision is
// recorded and can neither fail nor alter it. Long-form contract:
// backend/internal/server/README.md § "Divergence at the gate".

// CategoryPrecedentDivergence records one detected divergence from clear
// precedent. INTERNAL: chain-only, never its own issue-thread activity line.
const CategoryPrecedentDivergence = "precedent_divergence"

// divergenceMaxCited bounds the citations one precedent_divergence entry
// carries; CitedTotal reports the untruncated count.
const divergenceMaxCited = 10

// gateDivergenceDecision is one durably-recorded captain decision the hook
// evaluates.
type gateDivergenceDecision struct {
	RunID uuid.UUID
	// StageID is the stage the decision was recorded on — the concern's own
	// stage for waive/defer, the plan stage for a reject. It is both the
	// ranking-context stage and the per-gate de-duplication key.
	StageID uuid.UUID
	Class   decisionindex.DecisionClass
	// Outcome is the decision's index outcome ("waived", "deferred",
	// "reject").
	Outcome         string
	StageKind       string
	ConcernCategory string
	Severity        string
	// RejectClass is the validated reject_class on a plan reject. precedent.Context
	// carries no reject-class signal, so it is RECORDED on the entry rather
	// than used to rank.
	RejectClass string
	// DecisionSequence is the chain sequence of the decision's own entry; the
	// index row it projected is excluded from its own precedent set.
	DecisionSequence int64
}

// precedentDivergencePayload is the precedent_divergence audit payload. Cited
// precedent is carried by CITATION ONLY — no reason prose (ADR-082 rule 1),
// exactly as precedentSurfacedPayload.
type precedentDivergencePayload struct {
	DecisionClass    string                        `json:"decision_class"`
	StageID          string                        `json:"stage_id"`
	StageKind        string                        `json:"stage_kind"`
	DecisionSequence int64                         `json:"decision_sequence"`
	Outcome          string                        `json:"outcome"`
	RejectClass      string                        `json:"reject_class,omitempty"`
	ModalOutcome     string                        `json:"modal_outcome"`
	AgreementRatio   float64                       `json:"agreement_ratio"`
	HumanCount       int                           `json:"human_count"`
	Threshold        precedentDivergenceThreshold  `json:"threshold"`
	IndexVersion     string                        `json:"index_version"`
	Cited            []precedentDivergenceCitation `json:"cited"`
	CitedTotal       int                           `json:"cited_total"`
}

// precedentDivergenceThreshold is the effective rule the verdict ran under.
type precedentDivergenceThreshold struct {
	MinDecisions    int     `json:"min_decisions"`
	MinAgreement    float64 `json:"min_agreement"`
	WindowSeconds   int64   `json:"window_seconds"`
	DoctrineVersion string  `json:"doctrine_version"`
}

// precedentDivergenceCitation is one cited prior decision.
type precedentDivergenceCitation struct {
	SourceSequence  int64                     `json:"source_sequence"`
	SourceEntryHash string                    `json:"source_entry_hash"`
	DecisionClass   string                    `json:"decision_class"`
	Outcome         string                    `json:"outcome,omitempty"`
	Score           precedent.ScoreComponents `json:"score"`
	MatchedKeys     precedent.MatchedKeys     `json:"matched_keys"`
}

// noteGateDivergence evaluates one recorded decision and appends a
// precedent_divergence entry when it went against clear precedent.
//
// BEST-EFFORT AND TOTAL. Every one of these returns with NO entry and touches
// nothing the caller returns: a nil or disabled DivergenceConfig (the shipped
// default — silently), an allow-list miss, a nil PrecedentIndex / AuditRepo /
// RunRepo, a run read failure, a non-UUID run account id, a GateContext
// failure, an index List failure, zero candidates, a below-threshold or agreed
// verdict, a de-duplication hit or de-duplication READ failure, and an append
// failure. Each non-default miss warn-logs.
func (s *Server) noteGateDivergence(ctx context.Context, d gateDivergenceDecision) {
	cfg := s.cfg.DivergenceConfig
	if cfg == nil || !cfg.Enabled {
		return
	}
	if !cfg.ClassAllowed(d.Class, d.Outcome) {
		return
	}
	warn := func(msg string, err error) {
		attrs := []slog.Attr{
			slog.String("run_id", d.RunID.String()),
			slog.String("decision_class", string(d.Class)),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gate divergence: "+msg+"; no divergence recorded", attrs...)
	}
	if s.cfg.PrecedentIndex == nil || s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		warn("precedent index, audit repository or run repository unwired", nil)
		return
	}
	ru, err := s.cfg.RunRepo.GetRun(ctx, d.RunID)
	if err != nil || ru == nil {
		warn("run read failed", err)
		return
	}
	var acct *uuid.UUID
	if ru.AccountID != "" {
		u, perr := uuid.Parse(ru.AccountID)
		if perr != nil {
			warn("run account id is not a UUID", perr)
			return
		}
		acct = &u
	}
	stageID := d.StageID
	gc, err := s.cfg.PrecedentIndex.GateContext(ctx, decisionindex.GateRef{
		RunID: d.RunID, StageID: &stageID, AccountID: acct,
	})
	if err != nil {
		warn("resolve gate context failed", err)
		return
	}
	repo := gc.Repo
	if repo == "" {
		repo = ru.Repo
	}
	category, _ := decisionindex.NormalizeConcernCategory(d.ConcernCategory)

	var (
		items      []precedent.Item
		candidates int
	)
	for _, class := range precedent.ComparisonClasses(d.Class) {
		res, qerr := s.runPrecedentQuery(ctx, precedent.Context{
			Repo:            repo,
			DecisionClass:   class,
			StageKind:       d.StageKind,
			TouchedPaths:    gc.TouchedPaths,
			ConcernCategory: category,
			Severity:        d.Severity,
			EscalationKeys:  gc.EscalationKeys,
		}, acct, precedentDefaultLimit)
		if qerr != nil {
			warn("list decision index failed", qerr)
			return
		}
		candidates += res.Candidates
		for _, it := range res.Items {
			if it.SourceSequence == d.DecisionSequence && it.RunID == d.RunID.String() {
				continue // the decision itself is not its own precedent
			}
			items = append(items, it)
		}
	}
	if candidates == 0 || len(items) == 0 {
		return
	}

	v := precedent.Decide(*cfg, d.Class, d.Outcome, gc.DoctrineVersion, time.Now().UTC(), items)
	if v.Kind != precedent.VerdictDiverged {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "gate divergence: no divergence",
			slog.String("run_id", d.RunID.String()),
			slog.String("verdict", string(v.Kind)),
			slog.String("reason", string(v.Reason)))
		return
	}

	// ASK ONCE PER GATE. Read-then-append, NOT atomic — the same accepted
	// posture as recordPrecedentSurfaced: two concurrent first decisions can
	// both see "absent" and both append. Unlike recordPrecedentSurfaced, a
	// de-duplication READ failure records NOTHING (fail toward quiet): a
	// missed question is benign, while re-asking is the nagging the rule's
	// brakes exist to prevent.
	existing, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, d.RunID, CategoryPrecedentDivergence)
	if err != nil {
		warn("de-duplication read failed", err)
		return
	}
	for _, e := range existing {
		var p precedentDivergencePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.StageID == d.StageID.String() && p.DecisionClass == string(d.Class) {
			return
		}
	}

	p := precedentDivergencePayload{
		DecisionClass:    string(d.Class),
		StageID:          d.StageID.String(),
		StageKind:        d.StageKind,
		DecisionSequence: d.DecisionSequence,
		Outcome:          d.Outcome,
		RejectClass:      d.RejectClass,
		ModalOutcome:     v.ModalOutcome,
		AgreementRatio:   v.AgreementRatio,
		HumanCount:       v.Human,
		Threshold: precedentDivergenceThreshold{
			MinDecisions:    cfg.MinDecisions,
			MinAgreement:    cfg.MinAgreement,
			WindowSeconds:   int64(cfg.Window / time.Second),
			DoctrineVersion: gc.DoctrineVersion,
		},
		IndexVersion: precedent.IndexVersion,
		Cited:        make([]precedentDivergenceCitation, 0, min(len(v.Cited), divergenceMaxCited)),
		CitedTotal:   len(v.Cited),
	}
	for i, it := range v.Cited {
		if i >= divergenceMaxCited {
			break
		}
		// ReasonExcerpt is deliberately NOT copied (ADR-082 rule 1).
		p.Cited = append(p.Cited, precedentDivergenceCitation{
			SourceSequence:  it.SourceSequence,
			SourceEntryHash: it.SourceEntryHash,
			DecisionClass:   it.DecisionClass,
			Outcome:         it.Outcome,
			Score:           it.Score,
			MatchedKeys:     it.MatchedKeys,
		})
	}
	payload, err := json.Marshal(p)
	if err != nil {
		warn("marshal precedent_divergence payload failed", err)
		return
	}
	actorKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     d.RunID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryPrecedentDivergence,
		ActorKind: &actorKind,
		Payload:   payload,
	}); err != nil {
		warn("precedent_divergence audit append failed", err)
	}
}

// entrySequence is e's chain sequence, or 0 for a nil entry (a fake or a
// repository that returns none), which excludes nothing.
func entrySequence(e *audit.Entry) int64 {
	if e == nil {
		return 0
	}
	return e.Sequence
}
