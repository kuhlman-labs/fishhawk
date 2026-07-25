package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// live-validation walk audit categories (#2045, E48.35). Two distinct
// categories implement the intent-marker-before-file idempotency the operator's
// replan directives require:
//
//   - liveValidationWalkIntentKind records the "walk attempt started" marker
//     BEFORE the forge call. Once ANY intent marker exists a prior approval
//     already attempted (or completed) the filing, so a re-approval is a no-op —
//     it never files a second walk. This is the durable idempotency anchor.
//   - liveValidationWalkLinkedKind records the "walk filed" (or "filing
//     failed") outcome AFTER the forge call. The surface reads the newest linked
//     marker to render the pending count + walk ref (or the file-manually
//     variant).
//
// The two-category split (rather than one category with a phase discriminator)
// is deliberate: it lets the linked-marker write fail in isolation from the
// intent-marker write, so the partial-failure re-approval path — issue filed,
// linked-marker write lost — is exercisable and its idempotency provable.
//
// These are INTERNAL markers read exclusively via AuditRepo.ListForRunByCategory
// (a direct repo query) and distilled into the run-status live_validation
// surface — they are never operator-awaited through the registry-gated
// GET /audit / fishhawk_await_audit path. They are therefore deliberately NOT
// entered in audit.KnownCategories, and named `...Kind` rather than `...Category`
// to mark that distinction.
const (
	liveValidationWalkIntentKind = "live_validation_walk_intent"
	liveValidationWalkLinkedKind = "live_validation_walk_linked"
)

// liveValidationWalkMarker is the audit payload written for BOTH the intent and
// linked markers (the phase field names which). The pending count + criterion
// ids are carried on both so the surface can render from whichever marker is
// newest — a linked marker, or a stranded intent marker with no linked marker
// following it.
type liveValidationWalkMarker struct {
	// Phase is "intent" (written before the forge call) or "linked" (written
	// after). Redundant with the category but self-describing on the payload.
	Phase string `json:"phase"`
	// PendingCriteriaCount is the number of requires_live_validation criteria on
	// the approved plan awaiting an operator live check.
	PendingCriteriaCount int `json:"pending_criteria_count"`
	// CriterionIDs are the slug join keys of those criteria.
	CriterionIDs []string `json:"criterion_ids,omitempty"`
	// WalkRef is the filed walk work-item ref ("#N"); empty on an intent marker
	// and on a filing-failure linked marker.
	WalkRef string `json:"walk_ref,omitempty"`
	// FilingFailed is true on a linked marker written when the forge filing
	// failed (walk_ref empty). Always false on an intent marker.
	FilingFailed bool `json:"filing_failed"`
}

// runLiveValidationPayload is the run-status / gate-view wire surface for a
// run's pending operator live-validation walk (#2045). It is a distinct type
// from the audit marker so a payload-shape change in the audit trail cannot
// silently leak through the API surface. Populated ONLY by the single-run reads
// (handleGetRun, buildGateView), same best-effort single-read posture as the
// other distilled surfaces (Concerns / SecurityFindings); omitted (nil) when
// the run carries no live_validation marker.
//
// FilingFailed is the "file the walk by hand" decision bit: it is true for BOTH
// a linked-marker filing failure AND a stranded intent-only marker (the
// crash-window case), so a consumer that reads only FilingFailed never renders
// the healthy "walk: #X" variant for a run whose walk is not durably filed
// (binding condition A(1) — treat filing_failed and intent-only identically for
// rendering). FilingIncomplete additionally flags the stranded-intent sub-case
// so a consumer can word it "walk filing incomplete" vs "walk filing failed".
type runLiveValidationPayload struct {
	PendingCriteriaCount int    `json:"pending_criteria_count"`
	WalkRef              string `json:"walk_ref,omitempty"`
	FilingFailed         bool   `json:"filing_failed"`
	FilingIncomplete     bool   `json:"filing_incomplete,omitempty"`
}

// liveValidationWalkArea is the area:* label supplied on the filed chore walk so
// the chore type's required 'area' label namespace is satisfied (autonomy is
// covered by the type's label_default). A missing namespace is fail-open (a WARN
// in applyAndFileWorkItem, still files), so the value is cosmetic — it names the
// subsystem the walk-filing machinery lives in.
const liveValidationWalkArea = "area:backend"

// fileOrLinkLiveValidationWalk is the best-effort on-approval hook (#2045,
// E48.35): when an operator approves a plan carrying any requires_live_validation
// acceptance criterion, it auto-files (or, on a re-approval, no-ops on) a
// `chore`-type operator-validation walk work item and records a durable audit
// marker so the pending live check is tracked rather than shipped silently
// unvalidated. It is invoked from finishApprovalAdvance with the same
// best-effort posture as fileSplitProposalChildren — every forge / work-item /
// audit error logs and returns, NEVER unwinding the approval the gate already
// recorded. A plan with no marked criterion no-ops with zero side effects.
//
// Idempotency (binding condition A / replan directive 2) — INTENT-MARKER-BEFORE-
// FILE: a durable intent marker is appended BEFORE the forge call. On entry, if
// ANY intent marker is already present a prior approval already attempted the
// filing, so this is a no-op — it NEVER files a second walk. This deliberately
// does NOT re-file on a bare intent marker: a stranded intent marker (the
// process died between the intent append and the forge call, condition A) is
// indistinguishable from "issue filed, linked-marker write failed" without a
// forge query this hook deliberately avoids. Re-filing either would reopen the
// double-file window the intent marker closes.
//
// MANUAL RECOVERY (binding condition A(2)): a stranded intent-only marker — an
// intent marker with no linked marker following it — degrades to the pre-#2045
// status quo: the surface renders the file-manually guidance (see
// liveValidationForRun) and the OPERATOR files the walk by hand. This hook will
// not re-file it. This is the accepted residual of the forge-query-free
// idempotency design; it is rare (requires a crash inside the narrow
// intent-append → forge-call window).
func (s *Server) fileOrLinkLiveValidationWalk(ctx context.Context, stage *run.Stage) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	runID := stage.RunID

	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.logLiveValidationWarn(ctx, runID, "load approved plan failed", err.Error())
		return
	}
	if approvedPlan == nil {
		return
	}
	crits := plan.LiveValidationCriteria(approvedPlan.Verification)
	if len(crits) == 0 {
		return // no marked criterion → no forge call, no marker
	}

	// Idempotency anchor: the intent marker is the durable "walk attempt started"
	// record. Its presence — from ANY prior approval — means this hook already
	// ran, so no-op rather than file a second walk.
	priorIntent, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, liveValidationWalkIntentKind)
	if err != nil {
		s.logLiveValidationWarn(ctx, runID, "list intent markers failed", err.Error())
		return
	}
	if len(priorIntent) > 0 {
		return // already attempted → idempotent no-op
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.logLiveValidationWarn(ctx, runID, "get run failed", err.Error())
		return
	}
	owner, name, ok := splitRepoFullName(runRow.Repo)
	if !ok {
		s.logLiveValidationWarn(ctx, runID, "malformed run repo", runRow.Repo)
		return
	}
	parentIssue := 0
	if runRow.TriggerRef != nil {
		if n, ok := parseIssueRef(*runRow.TriggerRef); ok {
			parentIssue = n
		}
	}
	if parentIssue == 0 {
		// No originating issue to companion-link / parent against; the whole
		// hook is issue-scoped, so skip rather than file an orphan walk.
		return
	}

	ids := make([]string, 0, len(crits))
	for _, c := range crits {
		ids = append(ids, c.ID)
	}

	// (b) Durable INTENT marker BEFORE the forge call. On an append failure we
	// have filed nothing — a re-approval finds no intent marker and retries
	// cleanly (no orphan walk, no double file).
	if err := s.appendLiveValidationMarker(ctx, runRow, liveValidationWalkIntentKind, liveValidationWalkMarker{
		Phase:                "intent",
		PendingCriteriaCount: len(crits),
		CriterionIDs:         ids,
	}); err != nil {
		s.logLiveValidationWarn(ctx, runID, "append intent marker failed; filed nothing", err.Error())
		return
	}

	// (c) File the chore walk (best-effort epic-parenting with a companion-link
	// fallback). (d) Append the LINKED marker in BOTH outcomes — on success with
	// the walk ref, on ANY filing failure with filing_failed=true and an empty
	// ref — so approval never advances leaving pending live-validation criteria
	// with zero surfaced indication (replan directive 1).
	walkRef, filed := s.fileLiveValidationChore(ctx, runRow, owner, name, parentIssue, crits)
	linked := liveValidationWalkMarker{
		Phase:                "linked",
		PendingCriteriaCount: len(crits),
		CriterionIDs:         ids,
	}
	if filed {
		linked.WalkRef = walkRef
	} else {
		linked.FilingFailed = true
	}
	if err := s.appendLiveValidationMarker(ctx, runRow, liveValidationWalkLinkedKind, linked); err != nil {
		// The linked-marker write failed. The walk MAY already be filed. A
		// re-approval reads the intent marker and no-ops (never a second walk);
		// the surface renders the file-manually variant from the stranded intent
		// marker (liveValidationForRun). Best-effort: warn, never unwind.
		s.logLiveValidationWarn(ctx, runID, "append linked marker failed", err.Error())
	}
}

// fileLiveValidationChore files the `chore`-type operator-validation walk work
// item (replan directives 3 & 4). It returns ("#N", true) on a successful
// filing and ("", false) on any failure (the caller then writes a filing-failure
// linked marker).
//
// Best-effort epic-parenting: it first attempts a parent-epic-derived filing —
// Relations.ParentEpic set to the triggering issue, so applyAndFileWorkItem's
// deriveEpicTitleVar resolves the {epic} title placeholder from the issue's
// leading [E<n>] token (and the explicit n avoids the child-number forge query).
// When that derivation is unavailable (no forge client, an unreachable issue, or
// an issue whose title carries no [E<n>] token) the chore title_format's {epic}
// placeholder cannot resolve and Apply returns a missing_placeholders 422; the
// walk then falls back to an explicit {epic}/{n} title_vars filing that always
// renders and companion-links to the triggering issue instead. The parent-epic
// attempt fails at Apply BEFORE any provider.File call, so the fallback files
// exactly one walk.
func (s *Server) fileLiveValidationChore(ctx context.Context, runRow *run.Run, owner, name string, parentIssue int, crits []plan.AcceptanceCriterion) (string, bool) {
	conv, err := conventionsLoader(ctx, runRow.Repo)
	if err != nil {
		s.logLiveValidationWarn(ctx, runRow.ID, "load work-management conventions failed", err.Error())
		return "", false
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
	}
	if runRow.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil {
		if scope, rerr := s.resolveRepoScope(ctx, owner, name); rerr == nil {
			target.Scope = scope
		}
	}

	parentRef := "#" + strconv.Itoa(parentIssue)
	summary := "Operator live-validation walk for " + parentRef

	// Attempt 1: parent-epic-derived filing.
	epicReq := workmgmt.FilingRequest{
		Type:      "chore",
		Summary:   summary,
		Body:      liveValidationWalkBody(parentRef, crits, false),
		Labels:    []string{liveValidationWalkArea},
		TitleVars: map[string]string{"n": "1"},
		Relations: workmgmt.Relations{
			ParentEpic:   parentRef,
			EvidenceRuns: []string{runRow.ID.String()},
		},
	}
	if _, created, werr := s.applyAndFileWorkItem(ctx, epicReq, conv, target, owner, name); werr == nil {
		return fmt.Sprintf("#%d", created.Number), true
	} else {
		s.logLiveValidationWarn(ctx, runRow.ID, "epic-parented walk filing failed; trying companion-link fallback", werr.msg)
	}

	// Attempt 2: companion-link fallback with explicit title vars (always
	// renders). Companion-links to the triggering issue in the relation + body.
	fallbackReq := workmgmt.FilingRequest{
		Type:      "chore",
		Summary:   summary,
		Body:      liveValidationWalkBody(parentRef, crits, true),
		Labels:    []string{liveValidationWalkArea},
		TitleVars: map[string]string{"epic": strconv.Itoa(parentIssue), "n": "1"},
		Relations: workmgmt.Relations{
			CompanionTo:  []string{parentRef},
			EvidenceRuns: []string{runRow.ID.String()},
		},
	}
	if _, created, werr := s.applyAndFileWorkItem(ctx, fallbackReq, conv, target, owner, name); werr == nil {
		return fmt.Sprintf("#%d", created.Number), true
	} else {
		s.logLiveValidationWarn(ctx, runRow.ID, "companion-link fallback walk filing failed", werr.msg)
		return "", false
	}
}

// liveValidationWalkBody assembles the walk body: what the walk is, the
// criteria awaiting an operator live check, and either a parent-epic reference
// or (companion=true) a companion-link to the triggering issue.
func liveValidationWalkBody(parentRef string, crits []plan.AcceptanceCriterion, companion bool) string {
	body := "## Summary\n\nThis run's approved plan carries acceptance criteria whose true verification " +
		"needs a live forge/deploy/external target the default-deny acceptance sandbox cannot reach " +
		"(`requires_live_validation`). The acceptance stage short-circuits them; this walk tracks the " +
		"operator live check so nothing ships silently unvalidated (#2045).\n\n"
	if companion {
		body += "Companion to " + parentRef + ".\n\n"
	}
	body += "## Done-means\n\nEach criterion below has been live-validated by the operator against the real target:\n\n"
	for _, c := range crits {
		stmt := c.Statement
		if stmt == "" {
			stmt = c.ID
		}
		body += fmt.Sprintf("- [ ] `%s` — %s\n", c.ID, stmt)
	}
	return body
}

// appendLiveValidationMarker appends one intent-or-linked live-validation walk
// audit marker under the given category. It returns the append error (also
// WARN-logged by the caller) so the intent-marker append can be treated as the
// hard idempotency prerequisite.
func (s *Server) appendLiveValidationMarker(ctx context.Context, runRow *run.Run, category string, marker liveValidationWalkMarker) error {
	payload, _ := json.Marshal(marker)
	systemKind := audit.ActorSystem
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   payload,
	})
	return err
}

// liveValidationForRun distills the run's pending operator live-validation walk
// (#2045) from the newest live_validation walk marker. The single-run reads
// (handleGetRun, buildGateView) call it with the same best-effort posture as the
// other distilled surfaces: a nil AuditRepo or a read failure degrades to an
// omitted field (WARN, never a failed read), and a run with no marker returns
// nil.
//
// Marker precedence (binding condition A(1)):
//   - A linked marker (the forge outcome) wins over any earlier intent marker.
//     A healthy linked marker (walk_ref set) renders "walk: #N"; a filing-failure
//     linked marker (filing_failed) renders the file-manually variant.
//   - A stranded intent marker (an intent marker with NO linked marker following
//     it — the crash-window case) renders the file-manually variant too
//     (filing_failed=true), additionally flagged filing_incomplete so a consumer
//     can word it "walk filing incomplete" vs "walk filing failed". It is NEVER
//     rendered as the healthy "walk: #N" variant and never as a malformed
//     empty-ref string.
func (s *Server) liveValidationForRun(ctx context.Context, runID uuid.UUID) *runLiveValidationPayload {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	linked, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, liveValidationWalkLinkedKind)
	if err != nil {
		s.cfg.Logger.Warn("list live-validation linked markers failed; omitting live_validation block",
			"run_id", runID.String(), "error", err.Error())
		return nil
	}
	if len(linked) > 0 {
		// Newest wins: ListForRunByCategory is sequence-ascending.
		newest := linked[len(linked)-1]
		var m liveValidationWalkMarker
		if uerr := json.Unmarshal(newest.Payload, &m); uerr != nil {
			s.cfg.Logger.Warn("decode live-validation linked marker failed; omitting live_validation block",
				"run_id", runID.String(), "error", uerr.Error())
			return nil
		}
		return &runLiveValidationPayload{
			PendingCriteriaCount: m.PendingCriteriaCount,
			WalkRef:              m.WalkRef,
			FilingFailed:         m.FilingFailed,
		}
	}

	// No linked marker: a stranded intent marker (the crash-window case) still
	// surfaces as file-manually so the pending criteria are never silently
	// accepted.
	intent, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, liveValidationWalkIntentKind)
	if err != nil {
		s.cfg.Logger.Warn("list live-validation intent markers failed; omitting live_validation block",
			"run_id", runID.String(), "error", err.Error())
		return nil
	}
	if len(intent) == 0 {
		return nil
	}
	newest := intent[len(intent)-1]
	var m liveValidationWalkMarker
	if uerr := json.Unmarshal(newest.Payload, &m); uerr != nil {
		s.cfg.Logger.Warn("decode live-validation intent marker failed; omitting live_validation block",
			"run_id", runID.String(), "error", uerr.Error())
		return nil
	}
	return &runLiveValidationPayload{
		PendingCriteriaCount: m.PendingCriteriaCount,
		FilingFailed:         true,
		FilingIncomplete:     true,
	}
}

// logLiveValidationWarn is the shared WARN logger for the best-effort hook, so
// no branch fails the approval silently.
func (s *Server) logLiveValidationWarn(ctx context.Context, runID uuid.UUID, msg, detail string) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "live-validation filing: "+msg,
		slog.String("run_id", runID.String()),
		slog.String("detail", detail),
	)
}
