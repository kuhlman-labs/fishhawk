package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptance_fixup_deferral.go — E72.58 / #4075. A class-1 acceptance failure
// that lands while the run's implement review round is still in flight (a
// decomposed parent's consolidated review lands minutes after acceptance)
// would spend the single normal fix-up pass before the review's concerns
// exist, so those concerns then need a second, forced full pass. Instead,
// triage records fixup_deferred_review_in_flight and routes nothing; when the
// round settles, releaseDeferredAcceptanceFixup routes ONE acceptance-mode
// pass carrying the acceptance obligation AND the round's routable review
// concerns. Long-form contract: README.md § "Acceptance triage".

// acceptanceDispositionFixupDeferred is the non-paging, non-routed triage
// disposition recording a class-1 fix-up held for an in-flight implement
// review round. Neither acceptanceDispositionPages nor
// countAcceptanceTriageRoutes counts it: it never pings and never consumes a
// re-run; the release writes the counted/paged disposition.
const acceptanceDispositionFixupDeferred = "fixup_deferred_review_in_flight"

// acceptanceFixupDeferral is the additive fixup_deferral object on a deferred
// acceptance_triage_decided payload. Concerns are the synthesized acceptance
// concerns (Provenance=acceptance survives the JSON hop, so the released pass
// still renders them quarantined).
type acceptanceFixupDeferral struct {
	ImplementStageID    string               `json:"implement_stage_id"`
	ReviewRoundSequence int64                `json:"review_round_sequence"`
	Concerns            []planreview.Concern `json:"concerns"`
}

// deferredTriagePayload decodes the fields the release copies from a deferred
// entry.
type deferredTriagePayload struct {
	ArtifactID    string                   `json:"artifact_id"`
	Class         string                   `json:"class"`
	Disposition   string                   `json:"disposition"`
	CriterionIDs  []string                 `json:"criterion_ids"`
	FailureMode   string                   `json:"failure_mode"`
	PriorRouted   int                      `json:"prior_routed_passes"`
	FixupDeferral *acceptanceFixupDeferral `json:"fixup_deferral"`
}

// acceptanceFixupReleaseMu serializes releaseDeferredAcceptanceFixup within
// this process: the end-of-round hook, triage's post-write call and the boot
// sweep must not double-route one deferral. Review rounds are already
// process-local (processStart, reviewRedispatchPending), so a multi-replica
// deployment would need a DB lock here.
var acceptanceFixupReleaseMu sync.Mutex

// implementReviewKind is the implement entry of orphanedReviewStages.
func implementReviewKind() orphanedReviewStageKind {
	for _, k := range orphanedReviewStages {
		if k.isImplement {
			return k
		}
	}
	return orphanedReviewStageKind{}
}

// implementReviewRoundInFlight reports whether the run's CURRENT implement
// review round (its latest implement_review_started entry) names
// implementStageID, is unsettled (landed < ConfiguredAgents), and is alive in
// THIS process: mid boot re-dispatch, or started at/after s.processStart. An
// unsettled prior-process round is NOT in flight — the boot sweep owns it —
// the same liveness reading GET /v0/restart-blockers uses. roundSeq is the
// started entry's sequence when one exists.
func (s *Server) implementReviewRoundInFlight(ctx context.Context, runID, implementStageID uuid.UUID) (bool, int64, error) {
	kind := implementReviewKind()
	latest, payload, ok, err := s.latestReviewStarted(ctx, runID, kind)
	if err != nil {
		return false, 0, err
	}
	if !ok || payload.ConfiguredAgents <= 0 || latest.StageID == nil || *latest.StageID != implementStageID {
		return false, 0, nil
	}
	landed, err := s.countLandedReviewTerminals(ctx, runID, kind, latest.Sequence)
	if err != nil {
		return false, latest.Sequence, err
	}
	if landed >= payload.ConfiguredAgents {
		return false, latest.Sequence, nil
	}
	if reviewRedispatchPending(runID, kind.label, latest.Sequence) || !latest.Timestamp.Before(s.processStart) {
		return true, latest.Sequence, nil
	}
	return false, latest.Sequence, nil
}

// acceptanceRouteFixupDelegation evaluates the run's delegation for
// route_fixup with the same predicate autoFixup's arm reads
// (res.Decision(delegation.ActionRouteFixup), nil campaign override as the
// merge-candidate route does): delegated reports the class is delegated at
// all, met that its convergent_concerns condition is satisfied now. An
// unevaluable delegation is neither (fail-closed).
func (s *Server) acceptanceRouteFixupDelegation(ctx context.Context, runRow *run.Run) (delegated, met bool, unmetReason string) {
	if runRow == nil {
		return false, false, "run not loaded"
	}
	res, _, ok := s.evaluateRunDelegation(ctx, runRow, nil)
	if !ok || res == nil {
		return false, false, "delegation not evaluable"
	}
	d, found := res.Decision(delegation.ActionRouteFixup)
	if !found {
		return false, false, "route_fixup not delegated"
	}
	return true, d.Met, d.UnmetReason
}

// decideAcceptanceFixupDeferral decides whether routeAcceptanceClass1 defers
// (the caller has already established the pass would be admitted). It defers
// only when the implement round is in flight AND the run delegates
// route_fixup (the Captain's binding condition on #4075: without delegation
// the system actor may not fold reviewer concerns, so deferring would only
// delay today's acceptance-only pass). An in-flight read error or an
// unloadable run falls through to the immediate route.
func (s *Server) decideAcceptanceFixupDeferral(ctx context.Context, runID, implementStageID uuid.UUID, selected []planreview.Concern) (*acceptanceFixupDeferral, string, bool) {
	inFlight, roundSeq, err := s.implementReviewRoundInFlight(ctx, runID, implementStageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: implement review in-flight read failed; routing immediately",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return nil, "", false
	}
	if !inFlight {
		return nil, "", false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: load run for delegation failed; routing immediately",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return nil, "", false
	}
	if delegated, _, why := s.acceptanceRouteFixupDelegation(ctx, runRow); !delegated {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"acceptance triage class-1: implement review in flight but route_fixup is not delegated; routing immediately",
			slog.String("run_id", runID.String()), slog.String("reason", why))
		return nil, "", false
	}
	deferral := &acceptanceFixupDeferral{
		ImplementStageID:    implementStageID.String(),
		ReviewRoundSequence: roundSeq,
		Concerns:            selected,
	}
	reason := fmt.Sprintf("implement review round (implement_review_started sequence %d) is in flight; the acceptance fix-up is deferred until it settles and then routes ONE pass carrying the acceptance obligation and the round's routable review concerns (a restart before the release is recovered by the boot review sweep)", roundSeq)
	return deferral, reason, true
}

// releaseDeferredAcceptanceFixup routes the run's pending deferred acceptance
// fix-up, if any, once its implement review round has settled. Called at the
// end of every implement review round (trace.go), after triage writes a
// deferral, and by the boot review sweep. Idempotent: it acts only when the
// NEWEST acceptance_triage_decided entry is still the deferral, and always
// writes a newer entry when it acts.
func (s *Server) releaseDeferredAcceptanceFixup(ctx context.Context, runID uuid.UUID) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	acceptanceFixupReleaseMu.Lock()
	defer acceptanceFixupReleaseMu.Unlock()

	warn := func(msg string, err error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "acceptance fix-up release: "+msg+"; deferral stays pending",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceTriageDecided)
	if err != nil {
		warn("list triage entries failed", err)
		return
	}
	var newest *audit.Entry
	for _, e := range entries {
		if newest == nil || e.Sequence > newest.Sequence {
			newest = e
		}
	}
	if newest == nil || acceptanceTriageDispositionOf(newest.Payload) != acceptanceDispositionFixupDeferred {
		return
	}
	var p deferredTriagePayload
	if err := json.Unmarshal(newest.Payload, &p); err != nil {
		warn("decode deferral failed", err)
		return
	}
	if p.FixupDeferral == nil || newest.StageID == nil {
		warn("deferral payload incomplete", fmt.Errorf("fixup_deferral or stage id absent on sequence %d", newest.Sequence))
		return
	}
	implementID, err := uuid.Parse(p.FixupDeferral.ImplementStageID)
	if err != nil {
		warn("decode deferral implement stage id failed", err)
		return
	}
	acceptanceStageID := *newest.StageID

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		warn("load run failed", err)
		return
	}
	if runRow.State.IsTerminal() {
		// The run ended while the fix-up waited: the deferral is moot.
		return
	}
	inFlight, _, err := s.implementReviewRoundInFlight(ctx, runID, implementID)
	if err != nil {
		warn("implement review in-flight read failed", err)
		return
	}
	if inFlight {
		// A round is still running (possibly a newer one); its settle releases.
		return
	}

	fields := acceptanceTriageFields(runID, acceptanceStageID, p.ArtifactID, p.Class,
		acceptanceDispositionFixupDispatched, p.CriterionIDs, p.FailureMode, p.PriorRouted, "", nil, nil)
	fields["released_deferral_sequence"] = newest.Sequence
	if n, cerr := s.countAcceptanceTriageRoutes(ctx, runID); cerr == nil {
		fields["prior_routed_passes"] = n
	}
	page := func(reason string) {
		fields["disposition"] = acceptanceDispositionFixupUnavailable
		fields["reason"] = reason
		s.appendAcceptanceTriageDecided(ctx, runID, acceptanceStageID, fields)
		s.notifyPageClass(ctx, runID, "acceptance_fixup_release")
	}

	// Supersede: another actor routed a pass on the implement stage after the
	// deferral was written. Never route twice — record and page.
	fixups, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		warn("list fix-up triggers failed", err)
		return
	}
	var supersededBy int64
	for _, e := range fixups {
		if e.StageID != nil && *e.StageID == implementID && e.Sequence > newest.Sequence && e.Sequence > supersededBy {
			supersededBy = e.Sequence
		}
	}
	if supersededBy > 0 {
		fields["superseded_by_fixup_sequence"] = supersededBy
		page(fmt.Sprintf("a fix-up pass (stage_fixup_triggered sequence %d) was routed on the implement stage while the acceptance fix-up was deferred; not routing a second pass — the acceptance obligation needs a human route", supersededBy))
		return
	}

	selected := append([]planreview.Concern(nil), p.FixupDeferral.Concerns...)
	var reviewIDs []uuid.UUID
	reviewIDStrings := []string{}
	_, met, unmet := s.acceptanceRouteFixupDelegation(ctx, runRow)
	switch {
	case !met:
		// The fold of reviewer concerns needs the run's route_fixup
		// delegation MET (binding condition 1 of #4075); the acceptance
		// obligation still routes alone.
		fields["review_concern_fold_unmet"] = unmet
	case s.cfg.ConcernRepo == nil:
		fields["review_concerns_unavailable"] = true
	default:
		rows, lerr := s.cfg.ConcernRepo.ListByRun(ctx, runID)
		if lerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"acceptance fix-up release: list review concerns failed; routing the acceptance obligation alone",
				slog.String("run_id", runID.String()), slog.String("error", lerr.Error()))
			fields["review_concerns_unavailable"] = true
			break
		}
		review, ids, held := foldableReviewConcerns(rows, implementID)
		selected = append(selected, review...)
		reviewIDs = ids
		for _, id := range ids {
			reviewIDStrings = append(reviewIDStrings, id.String())
		}
		if len(held) > 0 {
			fields["held_review_concern_ids"] = held
		}
	}
	fields["routed_review_concern_ids"] = reviewIDStrings

	priorPasses, err := s.countFixupPasses(ctx, runID, implementID)
	if err != nil {
		page("count fix-up passes failed (" + err.Error() + "); not routing the deferred acceptance fix-up")
		return
	}
	reason := fmt.Sprintf("implement review round settled; releasing the acceptance fix-up deferred at sequence %d with %d review concern(s) folded in", newest.Sequence, len(reviewIDs))
	if ferr := s.routeAcceptanceFixupPass(ctx, runID, implementID, acceptanceStageID, selected, reviewIDs, priorPasses, reason); ferr != nil {
		page("deferred acceptance fix-up refused on release: " + ferr.Error())
		return
	}
	fields["reason"] = reason
	s.appendAcceptanceTriageDecided(ctx, runID, acceptanceStageID, fields)
}

// foldableReviewConcerns selects the implement stage's open (raised /
// reopened) review concerns the release may route, mapped exactly as
// resolveConcernsByID does. conventions_override_attempt (never routable) and
// requirement (a human judgment, as autoFixup's requirementArbitrationOpen
// pages on) are held back and returned as held ids.
func foldableReviewConcerns(rows []*concern.Concern, implementStageID uuid.UUID) ([]planreview.Concern, []uuid.UUID, []string) {
	var out []planreview.Concern
	var ids []uuid.UUID
	var held []string
	for _, c := range rows {
		if c.StageKind != concern.StageKindImplement || c.StageID != implementStageID {
			continue
		}
		if c.State != concern.StateRaised && c.State != concern.StateReopened {
			continue
		}
		if planreview.IsConventionsOverrideAttempt(c.Category) || strings.EqualFold(c.Category, requirementConcernCategory) {
			held = append(held, c.ID.String())
			continue
		}
		out = append(out, planreview.Concern{
			Severity:        planreview.ConcernSeverity(c.Severity),
			Category:        c.Category,
			Note:            c.Note,
			SuggestedPatch:  c.SuggestedPatch,
			ReviewerRole:    c.ReviewerRole,
			QuoteUnverified: c.QuoteUnverified,
		})
		ids = append(ids, c.ID)
	}
	return out, ids, held
}

// acceptanceFixupDeferralPending reports whether the run's NEWEST
// acceptance_triage_decided disposition is fixup_deferred_review_in_flight —
// autoFixup's skip (binding condition 2 of #4075). A read error reports
// pending (WARN): the auto-driver retries on its next poll rather than risk
// superseding a deferral it could not see.
func (s *Server) acceptanceFixupDeferralPending(ctx context.Context, runID uuid.UUID) bool {
	if s.cfg.AuditRepo == nil {
		return false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceTriageDecided)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"auto-drive: read acceptance triage entries failed; not routing a fix-up this pass",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return true
	}
	var newest *audit.Entry
	for _, e := range entries {
		if newest == nil || e.Sequence > newest.Sequence {
			newest = e
		}
	}
	return newest != nil && acceptanceTriageDispositionOf(newest.Payload) == acceptanceDispositionFixupDeferred
}
