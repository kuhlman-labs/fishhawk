package server

// Delegation shadow stamp (ADR-085 rule 4 / E82.1 / #3778).
//
// When a HUMAN acts on one of the five delegable run classes — approve or
// reject at a gate, route a fix-up, waive a concern, retry a stage, record a
// merge verdict — the server evaluates that class's single legal condition
// against the PRE-decision run state through the same internal/delegation
// condition code checkDelegation uses, and appends ONE
// `delegation_shadow_evaluated` audit entry AFTER the human's decision row.
//
// The stamp is RECORD-ONLY and BLIND: it grants nothing, changes no decision,
// is never folded into a delegation.Result, and is never rendered to the
// deciding human (no notifyStatusUpdate / notifyOperatorVisible, not an
// issue-comment surface). It is the E82 record's raw evidence of how often a
// human's decision agrees with what delegation would have done.
//
// Two phases, split around the decision call so the evaluation sees the
// state the human decided ON:
//
//   - captureDelegationShadow runs IMMEDIATELY BEFORE the decision call,
//     after every refusal gate, under a short bounded timeout. It never
//     refuses the human's action: every failure becomes an `unevaluable`
//     stamp naming the failure.
//   - recordDelegationShadow runs ONLY after the decision call succeeded and
//     wrote a FRESH decision row (never on a duplicate, a refusal, or an
//     error). It appends under a context detached from request cancellation,
//     and a failed append is warn-logged, never propagated — the human's
//     decision has already landed.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/escalation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CategoryDelegationShadowEvaluated is the audit category of the record-only
// delegation shadow stamp. Registered in audit.KnownCategories; deliberately
// NOT an issue-comment surface (docs/issue-comment-surfaces.md).
const CategoryDelegationShadowEvaluated = "delegation_shadow_evaluated"

// delegationShadowVersion versions the stamp payload so the E82 projection
// can key on its shape.
const delegationShadowVersion = 1

// defaultDelegationShadowCaptureTimeout bounds how long the capture may delay
// the human's action (approval condition 5). A capture that has not answered
// by then yields an `unevaluable` stamp and the action proceeds.
const defaultDelegationShadowCaptureTimeout = 3 * time.Second

// delegationShadowPayload is the `delegation_shadow_evaluated` payload.
type delegationShadowPayload struct {
	ShadowVersion int    `json:"shadow_version"`
	Action        string `json:"action"`
	Class         string `json:"class,omitempty"`
	Condition     string `json:"condition,omitempty"`
	// Verdict is one of delegation.ShadowVerdicts(); Met mirrors
	// Verdict == met.
	Verdict string `json:"verdict"`
	Met     bool   `json:"met"`
	Reason  string `json:"reason,omitempty"`
	// PageEvent names the active page_human_on event that turned a met/unmet
	// verdict into not_delegable.
	PageEvent  string `json:"page_event,omitempty"`
	Mode       string `json:"mode,omitempty"`
	ModeSource string `json:"mode_source,omitempty"`
	// Anchored is true iff the class's resolved mode is report: report-mode
	// proposals are SHOWN to the operator, so agreement there is anchored
	// (ADR-085 rule 4) and the projection must separate it from blind
	// gated-mode agreement.
	Anchored           bool   `json:"anchored"`
	Tier               string `json:"tier,omitempty"`
	MatrixResolved     bool   `json:"matrix_resolved"`
	ResolvedMatrixHash string `json:"resolved_matrix_hash,omitempty"`
	WorkflowSHA        string `json:"workflow_sha,omitempty"`
	// EscalationFingerprint / EscalationKeys / MaxAutonomy are the E75.1
	// escalation stratum: the fired set's result fingerprint, its sorted
	// stable RuleKeys, and the composed ceiling. EscalationKeys is always
	// present (empty when nothing fired).
	EscalationFingerprint string   `json:"escalation_fingerprint,omitempty"`
	EscalationKeys        []string `json:"escalation_keys"`
	MaxAutonomy           string   `json:"max_autonomy,omitempty"`
	HumanDecision         string   `json:"human_decision"`
	DecisionCategory      string   `json:"decision_category"`
	ActorKind             string   `json:"actor_kind"`
	ActorSubject          string   `json:"actor_subject"`
	StageID               string   `json:"stage_id,omitempty"`
	ConcernIDs            []string `json:"concern_ids,omitempty"`
}

// pendingDelegationShadow is a captured (pre-decision) evaluation awaiting
// the decision's outcome. A nil *pendingDelegationShadow means "never stamp"
// (a delegated or agent action, or no audit store) and every method on it is
// nil-safe.
type pendingDelegationShadow struct {
	runID   uuid.UUID
	payload delegationShadowPayload
}

// shadowEscalationResolver is a NON-emitting delegation.EscalationResolver:
// it evaluates the run's escalations exactly as resolveEscalations does but
// never writes escalation_fired, so the shadow's only write is the stamp. It
// keeps the Result so the stamp can record the fired stratum. Pointer
// receiver: Shadow calls it once and the capture reads res afterwards.
type shadowEscalationResolver struct {
	s   *Server
	res escalation.Result
}

// ResolveEscalations implements delegation.EscalationResolver. The
// no-escalations short-circuit is the first statement, as in
// resolveEscalations, so a workflow declaring none costs no reads.
func (r *shadowEscalationResolver) ResolveEscalations(ctx context.Context, runRow *run.Run, wf *spec.Workflow, _ uuid.UUID) (spec.ComposedRequirements, error) {
	if runRow == nil || wf == nil || len(wf.Escalations) == 0 {
		return spec.ComposedRequirements{}, nil
	}
	change, err := r.s.escalationChange(ctx, runRow, wf.Escalations)
	if err != nil {
		return spec.ComposedRequirements{}, err
	}
	res, err := escalation.Evaluate(wf.Escalations, change)
	if err != nil {
		return spec.ComposedRequirements{}, fmt.Errorf("evaluate escalations: %w", err)
	}
	r.res = res
	return res.Requirements, nil
}

// delegationShadowCaptureTimeoutOverride is a test seam (nanoseconds; 0 =
// the default). Atomic so an abandoned capture goroutine from one test can
// never race a later test's override.
var delegationShadowCaptureTimeoutOverride atomic.Int64

// shadowCaptureTimeout returns the capture bound.
func shadowCaptureTimeout() time.Duration {
	if d := time.Duration(delegationShadowCaptureTimeoutOverride.Load()); d > 0 {
		return d
	}
	return defaultDelegationShadowCaptureTimeout
}

// captureDelegationShadow evaluates what delegation would have done for
// action on the run's CURRENT (pre-decision) state. It returns nil — no stamp
// will ever be written — when the action is delegated, the subject is an
// agent identity (isAgentSubject), or no audit store is wired. Otherwise it
// ALWAYS returns a pending stamp: every failure, including the capture
// timeout, degrades to verdict `unevaluable` with a reason naming the failure.
//
// The evaluation runs on a context detached from request cancellation and
// bounded by shadowCaptureTimeout; the select below returns at that bound
// even if a repository ignores its context, so the capture never delays the
// human's action beyond it.
func (s *Server) captureDelegationShadow(ctx context.Context, runID uuid.UUID, action, subject string, delegated bool) *pendingDelegationShadow {
	if delegated || isAgentSubject(subject) || s.cfg.AuditRepo == nil {
		return nil
	}
	p := &pendingDelegationShadow{
		runID: runID,
		payload: delegationShadowPayload{
			ShadowVersion:  delegationShadowVersion,
			Action:         action,
			ActorKind:      string(actorKindForSubject(subject)),
			ActorSubject:   subject,
			EscalationKeys: []string{},
		},
	}

	timeout := shadowCaptureTimeout()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// Buffered so an abandoned evaluation (timeout) can still complete its
	// send and exit; the payload it carries is a copy nobody else touches.
	done := make(chan delegationShadowPayload, 1)
	base := p.payload
	go func() {
		done <- s.evaluateDelegationShadow(cctx, runID, base)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case out := <-done:
		p.payload = out
	case <-timer.C:
		p.payload = unevaluableShadow(base, "capture_timeout",
			fmt.Sprintf("the shadow evaluation did not finish within %s", timeout))
	}
	return p
}

// unevaluableShadow stamps base as unevaluable, naming the failure mode.
func unevaluableShadow(base delegationShadowPayload, mode, detail string) delegationShadowPayload {
	base.Verdict = string(delegation.ShadowUnevaluable)
	base.Met = false
	base.Reason = mode + ": " + detail
	return base
}

// evaluateDelegationShadow is the capture body. It reads the run and its
// cached spec, runs delegation.Shadow with the non-emitting resolver, and
// applies the page_human_on override Shadow itself does not apply.
func (s *Server) evaluateDelegationShadow(ctx context.Context, runID uuid.UUID, base delegationShadowPayload) delegationShadowPayload {
	if s.cfg.RunRepo == nil || s.cfg.ConcernRepo == nil {
		return unevaluableShadow(base, "repositories_unconfigured",
			"the shadow evaluation requires run and concern repositories")
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return unevaluableShadow(base, "run_read_failed", err.Error())
	}
	base.WorkflowSHA = runRow.WorkflowSHA
	if len(runRow.WorkflowSpec) == 0 {
		return unevaluableShadow(base, "no_cached_spec",
			"the run carries no cached workflow spec, so no delegation contract can be resolved")
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return unevaluableShadow(base, "spec_unparseable", err.Error())
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return unevaluableShadow(base, "workflow_missing",
			fmt.Sprintf("workflow %q is not in the run's cached spec", runRow.WorkflowID))
	}
	resolver := &shadowEscalationResolver{s: s}
	ev, err := delegation.NewEvaluator(s.cfg.RunRepo, s.cfg.ConcernRepo, s.cfg.AuditRepo, resolver)
	if err != nil {
		return unevaluableShadow(base, "evaluator_unavailable", err.Error())
	}
	sh, err := ev.Shadow(ctx, runRow, &wf, base.Action)
	if err != nil {
		return unevaluableShadow(base, "shadow_evaluation_failed", err.Error())
	}

	out := base
	out.Class = sh.Class
	out.Condition = string(sh.Condition)
	out.Verdict = string(sh.Verdict)
	out.Met = sh.Met
	out.Reason = sh.Reason
	out.Mode = string(sh.Mode)
	out.ModeSource = string(sh.Source)
	out.Anchored = sh.Mode == spec.ModeReport
	out.Tier = string(sh.Tier)
	out.MatrixResolved = sh.MatrixResolved
	out.ResolvedMatrixHash = delegationview.HashMatrix(sh.Matrix)
	if resolver.res.Any() {
		out.EscalationFingerprint = escalation.Fingerprint(resolver.res)
		keys := make([]string, 0, len(resolver.res.Fired))
		for _, f := range resolver.res.Fired {
			keys = append(keys, escalation.RuleKey(f.Escalation))
		}
		sort.Strings(keys)
		out.EscalationKeys = keys
	}
	out.MaxAutonomy = string(resolver.res.Requirements.MaxAutonomy)

	// page_human_on override (ADR-085: a page event is never a would-be
	// auto). Only a met/unmet verdict is overridden — not_delegable already
	// says delegation would not have acted.
	if sh.Verdict == delegation.ShadowMet || sh.Verdict == delegation.ShadowUnmet {
		open, oerr := s.cfg.ConcernRepo.ListOpenByRun(ctx, runID)
		if oerr != nil {
			return unevaluableShadow(out, "open_concern_read_failed", oerr.Error())
		}
		if pe := s.activePageEvent(ctx, runRow, &wf, &delegation.Result{MustPageHuman: sh.MustPageHuman}, open); pe != "" {
			out.Verdict = string(delegation.ShadowNotDelegable)
			out.Met = false
			out.PageEvent = pe
			out.Reason = "page_human_on: " + pe + " is active at the gate, so delegation would have paged the human rather than acted"
		}
	}
	return out
}

// recordDelegationShadow appends the captured stamp after the human's
// decision landed. Nil-safe. The append runs on a context detached from
// request cancellation so a client disconnect after the decision does not
// drop the stamp; a marshal/append failure is warn-logged and never
// propagated. It never notifies — the stamp is blind.
func (s *Server) recordDelegationShadow(ctx context.Context, p *pendingDelegationShadow, stageID *uuid.UUID, decision, decisionCategory string, concernIDs []uuid.UUID) {
	if p == nil || s.cfg.AuditRepo == nil {
		return
	}
	payload := p.payload
	payload.HumanDecision = decision
	payload.DecisionCategory = decisionCategory
	if stageID != nil && *stageID != uuid.Nil {
		payload.StageID = stageID.String()
	}
	for _, id := range concernIDs {
		payload.ConcernIDs = append(payload.ConcernIDs, id.String())
	}
	b, err := json.Marshal(payload)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "delegation shadow: marshal stamp failed; the human's decision stands",
			slog.String("run_id", p.runID.String()), slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorKind("system")
	var sid *uuid.UUID
	if stageID != nil && *stageID != uuid.Nil {
		v := *stageID
		sid = &v
	}
	if _, err := s.cfg.AuditRepo.AppendChained(context.WithoutCancel(ctx), audit.ChainAppendParams{
		RunID:     p.runID,
		StageID:   sid,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDelegationShadowEvaluated,
		ActorKind: &systemKind,
		Payload:   b,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "delegation shadow: append stamp failed; the human's decision stands",
			slog.String("run_id", p.runID.String()),
			slog.String("action", payload.Action),
			slog.String("error", err.Error()))
	}
}

// retryShadowDecisionCategory names the audit category a successful human
// retry wrote, derived from the PRE-decision stage the way run.RetryStage
// decides it: only an override of a category-B failure is recorded as
// stage_override_retried.
func retryShadowDecisionCategory(stage *run.Stage, override bool) string {
	if override && stage != nil && stage.FailureCategory != nil && *stage.FailureCategory == run.FailureB {
		return CategoryStageOverrideRetried
	}
	return CategoryStageRetried
}
