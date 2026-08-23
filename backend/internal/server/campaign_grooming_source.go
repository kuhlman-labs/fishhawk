package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// groomingSourceRequest is the POST /v0/campaigns `grooming_source` object —
// the THIRD campaign source (E54.6 / #2238), alongside epic_ref and an explicit
// items list. Its PRESENCE selects the source; it is mutually exclusive with
// both of the others.
type groomingSourceRequest struct {
	// RunID is the grooming run whose approved grooming_report artifact carries
	// the ratified priority order. Required.
	RunID string `json:"run_id"`
	// Limit optionally caps the batch to the top N CONVERTIBLE entries by rank.
	// 0 (or omitted) means no cap. Negative is a validation failure.
	Limit int `json:"limit,omitempty"`
	// AllowSuperseded explicitly acknowledges building from an order that a
	// NAMED newer approved grooming run has superseded. Default false refuses —
	// an operator can act on a refusal; they cannot act on a campaign silently
	// built from an order they did not ratify. It does NOT reach an undetermined
	// scan: that refusal is unconditional (K2), because acknowledging a check
	// that never completed acknowledges nothing.
	AllowSuperseded bool `json:"allow_superseded,omitempty"`
}

// campaignGroomingSourcePayload is BOTH the durable provenance block persisted
// on campaigns.grooming_source AND the `grooming_source` block the campaign
// response carries. One type, so the record an operator reads back can never
// drift from the record that was stored.
//
// It is written by the campaign row's OWN single-row INSERT (campaign.Persist →
// CreateCampaign), so a grooming-sourced campaign can never exist
// unprovenanced. The campaign_grooming_source_resolved audit entry is a SECOND,
// best-effort copy — useful for the chain, but not the system of record.
type campaignGroomingSourcePayload struct {
	// SourceRunID / SourceStageID / ReportArtifactID identify the ratified
	// order: the grooming run, its plan stage, and the report artifact row.
	SourceRunID      uuid.UUID `json:"source_run_id"`
	SourceStageID    uuid.UUID `json:"source_stage_id"`
	ReportArtifactID uuid.UUID `json:"report_artifact_id"`
	// ReportContentHash is the report artifact's content hash — what makes the
	// provenance reproducible (WHICH report text, not merely which run).
	ReportContentHash string `json:"report_content_hash"`
	// OrderedRefs are the campaign's issue refs in ratified rank order, and
	// OrderedCount is their number. This IS the queue order that was created.
	OrderedRefs  []string `json:"ordered_refs"`
	OrderedCount int      `json:"ordered_count"`
	// Excluded names every ordering entry that did not become a campaign item,
	// with its reason. Never omitted silently: a truncated batch an operator
	// cannot see is the failure this block exists to prevent.
	Excluded []campaign.GroomingOrderExclusion `json:"excluded,omitempty"`
	// Limit is the applied cap (omitted when uncapped) and OmittedByLimit is the
	// count of CONVERTIBLE entries that cap dropped. The two are distinct from
	// Excluded on purpose: a capped entry WAS convertible, so conflating the two
	// would make this count underivable.
	Limit          int `json:"limit,omitempty"`
	OmittedByLimit int `json:"omitted_by_limit"`
	// SupersededBy names the newer approved grooming run whose order this
	// campaign deliberately did NOT use, set only when the caller passed
	// allow_superseded. Omitted otherwise.
	//
	// There is deliberately NO companion field for an undetermined scan: an
	// incomplete scan is an unconditional refusal (K2), so no created campaign
	// can carry that state and a field recording it could only ever be false.
	SupersededBy *uuid.UUID `json:"superseded_by,omitempty"`
}

// groomingSourceError is a typed refusal from the grooming-source resolution
// ladder. Carrying the HTTP status + API code + details as data (rather than
// mapping error strings at the call site) keeps every refusal branch's
// observable output pinned to one place.
type groomingSourceError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *groomingSourceError) Error() string { return e.Message }

// Grooming-source refusal codes. Every one of these is reachable without a live
// forge, because the whole ladder runs before the provider seam.
const (
	codeGroomingRunNotFound  = "grooming_run_not_found"
	codeGroomingRepoMismatch = "grooming_order_repo_mismatch"
	codeGroomingOrderAbsent  = "grooming_order_absent"
	codeGroomingNotApproved  = "grooming_order_not_approved"
	codeGroomingOrderInvalid = "grooming_order_invalid"
	codeGroomingOrderEmpty   = "grooming_order_empty"
	codeGroomingSuperseded   = "grooming_order_superseded"
	// codeGroomingSupersessionUndetermined is the K2 fail-closed refusal: the
	// bounded scan could not establish that no newer approved grooming run
	// exists. It is an UNCONDITIONAL refusal — allow_superseded does not reach
	// it — because "the scan was capped" is not evidence of absence, and a
	// caller-set flag cannot stand in for a check that never completed.
	codeGroomingSupersessionUndetermined = "grooming_order_supersession_undetermined"
	// codeGroomingSupersessionUnreadable is the infrastructure sibling: a read
	// the currency decision depends on FAILED. It covers BOTH such reads — the
	// SOURCE run's own stages/artifacts/approvals and the supersession scan's
	// CANDIDATE runs — because the operator remedy is identical (a backend read
	// failure to retry, not a stale order), and the two messages already name
	// which read failed. Distinct from undetermined because allow_superseded
	// does NOT bypass it — an operator can acknowledge a known-stale order, but
	// nobody can acknowledge a read failure.
	codeGroomingSupersessionUnreadable = "grooming_order_supersession_unreadable"
)

// auditCampaignGroomingSourceResolved is the audit category written once per
// campaign created from an approved grooming order. Registered in
// audit.KnownCategories so an operator can await it without allow_unknown; it is
// audit-only and renders NO issue comment (docs/issue-comment-surfaces.md).
const auditCampaignGroomingSourceResolved = "campaign_grooming_source_resolved"

// groomingSupersessionPageSize and groomingSupersessionMaxPages bound the
// supersession scan. The scan pages until it sees a SHORT page (proof it
// reached the end of the workflow-scoped run list) or until it has read
// MaxPages full pages, at which point it cannot prove absence and REFUSES.
//
// This is deliberately NOT the churn guard's posture. groomingChurnBaseline is
// a SUPPRESSOR: when it cannot read its window it proposes everything, which is
// the safe direction for a suppressor. This check is authorization-shaped — it
// decides whether an operator's ratified order is still the current one — so it
// fails toward REFUSING.
const (
	groomingSupersessionPageSize = 100
	groomingSupersessionMaxPages = 20
)

// resolveGroomingOrder walks the grooming-source ladder and returns the
// rank-ordered issue set to build the campaign from, or a typed refusal.
//
// The ladder is ordered so a cheap refusal never costs a forge round-trip —
// every step below runs against the local run/artifact/approval stores, and the
// whole function executes BEFORE the work-management provider is consulted:
//
//  1. run_id parses as a UUID                       -> 400 validation_failed
//  2. the run exists AND belongs to the caller's
//     tenant account                                -> 404 grooming_run_not_found
//     (a cross-tenant run is INDISTINGUISHABLE from a missing one)
//  3. the run's repo == the campaign's repo         -> 422 grooming_order_repo_mismatch
//  4. a plan stage carries a grooming_report        -> 422 grooming_order_absent
//     (an UNREADABLE source run is 502 grooming_order_supersession_unreadable:
//     one code covers both currency-deciding reads — see its declaration)
//  5. that stage's approval gate was GRANTED        -> 422 grooming_order_not_approved
//  6. the report parses                             -> 422 grooming_order_invalid
//  7. the supersession scan COMPLETED                -> 422 grooming_order_supersession_undetermined
//     / 502 grooming_order_supersession_unreadable
//     and named no newer approved grooming run       -> 422 grooming_order_superseded
//  8. the order names at least one target-repo issue -> 422 grooming_order_empty
//
// STEP 5 IS THE RATIFICATION CHECK. The ratification mechanism is the
// backlog_grooming workflow's `groom` stage approval gate
// (.fishhawk/workflows.yaml: type approval, count 1, not [author, agent]), so an
// approved order is defined here as: the grooming_report artifact of a plan
// stage carrying at least one approval.DecisionApprove and NO DecisionReject.
//
// THAT GATE APPROVAL IS NOW BOTH THE RATIFICATION SIGNAL AND THE APPLY TRIGGER
// (E54.19 / #2822): grooming_apply.go's applyApprovedGrooming fires on the same
// approval and re-reads the SAME predicate (>= 1 grant, 0 rejections) before it
// dispatches a single hygiene mutation. Keeping the two surfaces on one
// predicate is deliberate — a campaign built from a report and an apply of that
// report must agree on what "ratified" means, and two independently-drifting
// definitions would let one act on an order the other refuses. Tightening both
// to a real gate-satisfaction count (they coincide only under `count: 1`) is a
// follow-up that must move them together.
func (s *Server) resolveGroomingOrder(ctx context.Context, owner, name, repoFullName string, gs *groomingSourceRequest) (*campaign.GroomingOrder, *campaignGroomingSourcePayload, error) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.ApprovalRepo == nil {
		return nil, nil, &groomingSourceError{
			Status: http.StatusServiceUnavailable, Code: "grooming_source_unconfigured",
			Message: "campaign creation from a grooming order requires configured run, artifact and approval repositories",
		}
	}

	runID, err := uuid.Parse(gs.RunID)
	if err != nil {
		return nil, nil, &groomingSourceError{
			Status: http.StatusBadRequest, Code: "validation_failed",
			Message: "grooming_source.run_id must be a UUID",
			Details: map[string]any{"field": "grooming_source.run_id", "got": gs.RunID},
		}
	}

	notFound := &groomingSourceError{
		Status: http.StatusNotFound, Code: codeGroomingRunNotFound,
		Message: "no grooming run with that id is visible to this account",
		Details: map[string]any{"run_id": runID.String()},
	}
	sourceRun, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if errors.Is(err, run.ErrNotFound) {
		return nil, nil, notFound
	}
	if err != nil {
		// THE CAUSE RIDES internalCauseKey, NEVER A PLAIN DETAIL KEY (E67.15 /
		// #2587). A repository error renders storage, query and infrastructure
		// text that an agent-facing authenticated caller has no business
		// reading; writeError strips this channel from the body at ANY status
		// and folds it into the ONE operator log record keyed by error_ref. The
		// client keeps the static message and the product-owned run_id.
		return nil, nil, &groomingSourceError{
			Status: http.StatusInternalServerError, Code: "internal_error",
			Message: "could not read the grooming run",
			Details: map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()},
		}
	}
	// TENANT SCOPING RETURNS THE SAME 404 AS A MISSING RUN. A distinct code (or
	// a 403) would confirm the run's existence to a caller in another account,
	// turning the id space into an oracle — which the campaign-ownership gate
	// (enforceCampaignAccount) can afford to do on an id the caller already
	// holds, but this endpoint cannot: here the run id is CALLER-SUPPLIED.
	//
	// THE MATCH IS EXACT, INCLUDING AN EMPTY SOURCE ACCOUNT. The #1829
	// NULL-allow window other gates honour is for an id the caller ALREADY
	// holds; here the run id is caller-supplied, so allowing an untenanted run
	// would let any authenticated tenant name an arbitrary untenanted run and
	// consume its approved order. An untenanted run is reachable only by an
	// untenanted caller (both sides empty).
	if sourceRun.AccountID != IdentityFrom(ctx).AccountID {
		return nil, nil, notFound
	}
	if sourceRun.Repo != repoFullName {
		return nil, nil, &groomingSourceError{
			Status: http.StatusUnprocessableEntity, Code: codeGroomingRepoMismatch,
			Message: fmt.Sprintf("the grooming run groomed %s, not %s", sourceRun.Repo, repoFullName),
			Details: map[string]any{"run_id": runID.String(), "grooming_repo": sourceRun.Repo, "campaign_repo": repoFullName},
		}
	}

	found, err := s.approvedGroomingReport(ctx, runID)
	if err != nil {
		return nil, nil, &groomingSourceError{
			Status: http.StatusBadGateway, Code: codeGroomingSupersessionUnreadable,
			Message: "could not read the grooming run's stages, artifacts or approvals",
			Details: map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()},
		}
	}
	if found == nil {
		return nil, nil, &groomingSourceError{
			Status: http.StatusUnprocessableEntity, Code: codeGroomingOrderAbsent,
			Message: "the grooming run shipped no grooming_report artifact",
			Details: map[string]any{"run_id": runID.String()},
		}
	}
	if !found.approved {
		return nil, nil, &groomingSourceError{
			Status: http.StatusUnprocessableEntity, Code: codeGroomingNotApproved,
			Message: "the grooming run's report was not ratified: its plan stage carries no granted approval, or carries a rejection",
			Details: map[string]any{
				"run_id": runID.String(), "stage_id": found.stageID.String(),
				"approvals": found.approveCount, "rejections": found.rejectCount,
			},
		}
	}
	report, perr := plan.ParseGroomingReport(found.content)
	if perr != nil {
		return nil, nil, &groomingSourceError{
			Status: http.StatusUnprocessableEntity, Code: codeGroomingOrderInvalid,
			Message: "the grooming run's report artifact does not parse as a grooming_report",
			Details: map[string]any{"run_id": runID.String(), "error": perr.Error()},
		}
	}

	superseded, undetermined, serr := s.newerApprovedGroomingRun(ctx, sourceRun)
	if serr != nil {
		return nil, nil, serr
	}
	// AN UNDETERMINED SCAN IS AN UNCONDITIONAL REFUSAL — allow_superseded does
	// NOT reach it. The flag acknowledges a POSITIVELY IDENTIFIED superseding
	// run, which an operator can look at and decide about; an incomplete scan
	// gives them nothing to decide about, so treating the flag as an
	// acknowledgement of it would turn a caller-controlled request field into a
	// bypass of an authorization-shaped check (K2).
	if undetermined {
		return nil, nil, &groomingSourceError{
			Status: http.StatusUnprocessableEntity, Code: codeGroomingSupersessionUndetermined,
			Message: "the supersession scan could not establish that no newer approved grooming run exists; narrow the workflow's run history so the scan can reach the end of it",
			Details: map[string]any{"source_run_id": runID.String(), "scanned_pages": groomingSupersessionMaxPages},
		}
	}
	if superseded != nil && !gs.AllowSuperseded {
		return nil, nil, &groomingSourceError{
			Status: http.StatusUnprocessableEntity, Code: codeGroomingSuperseded,
			Message: "a newer approved grooming run has superseded this order; groom from the newer run, or pass allow_superseded to build from this one deliberately",
			Details: map[string]any{"source_run_id": runID.String(), "superseded_by": superseded.String()},
		}
	}

	order, oerr := campaign.OrderFromReport(report, owner, name, gs.Limit)
	if oerr != nil {
		var goe *campaign.GroomingOrderError
		code := codeGroomingOrderInvalid
		if errors.As(oerr, &goe) && goe.Code == campaign.GroomingOrderErrEmpty {
			code = codeGroomingOrderEmpty
		}
		return nil, nil, &groomingSourceError{
			Status: http.StatusUnprocessableEntity, Code: code,
			Message: oerr.Error(), Details: map[string]any{"run_id": runID.String()},
		}
	}
	order.RunID = runID
	order.StageID = found.stageID
	order.ArtifactID = found.artifactID
	order.ContentHash = found.contentHash
	if gs.AllowSuperseded {
		order.SupersededBy = superseded
	}
	return order, groomingSourcePayload(order), nil
}

// groomingSourcePayload projects a resolved order into the durable provenance
// block. OrderedRefs is normalized to a non-nil slice so the persisted JSON and
// the response both carry an array rather than null.
func groomingSourcePayload(o *campaign.GroomingOrder) *campaignGroomingSourcePayload {
	refs := o.Refs
	if refs == nil {
		refs = []string{}
	}
	return &campaignGroomingSourcePayload{
		SourceRunID:       o.RunID,
		SourceStageID:     o.StageID,
		ReportArtifactID:  o.ArtifactID,
		ReportContentHash: o.ContentHash,
		OrderedRefs:       refs,
		OrderedCount:      len(refs),
		Excluded:          o.Excluded,
		Limit:             o.Limit,
		OmittedByLimit:    o.OmittedByLimit,
		SupersededBy:      o.SupersededBy,
	}
}

// groomingReportHit is one run's grooming_report artifact plus the approval
// verdict on its stage.
type groomingReportHit struct {
	stageID      uuid.UUID
	artifactID   uuid.UUID
	contentHash  string
	content      []byte
	approved     bool
	approveCount int
	rejectCount  int
}

// approvedGroomingReport resolves a run's grooming_report artifact and the
// approval verdict on the plan stage carrying it. Returns (nil, nil) when the
// run shipped no report — an ABSENCE, distinct from an error, which is always
// returned as an error so a read failure is never mistaken for "no report".
//
// When a run carries more than one plan stage with a report, the LATEST by
// artifact CreatedAt wins, so a re-run of the groom stage supersedes its own
// earlier attempt within one run.
func (s *Server) approvedGroomingReport(ctx context.Context, runID uuid.UUID) (*groomingReportHit, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list stages for run %s: %w", runID, err)
	}
	var best *groomingReportHit
	// FIRST-HIT SENTINEL, NOT A NUMERIC FLOOR. A prior `bestAt int64 = -1` +
	// `a.CreatedAt.UnixNano() <= bestAt` skipped an artifact whose CreatedAt is
	// the ZERO time, because time.Time{}.UnixNano() is a large NEGATIVE number:
	// a run that DID ship a report surfaced as 422 grooming_order_absent. The
	// `best != nil &&` prefix makes the FIRST candidate win unconditionally, so
	// no wall-clock value can be mistaken for an absence.
	var bestAt time.Time
	for _, st := range stages {
		if st == nil || st.Type != run.StageTypePlan {
			continue
		}
		arts, aerr := s.cfg.ArtifactRepo.ListForStage(ctx, st.ID)
		if aerr != nil {
			return nil, fmt.Errorf("list artifacts for stage %s: %w", st.ID, aerr)
		}
		for _, a := range arts {
			if a == nil || a.Kind != artifact.KindGroomingReport {
				continue
			}
			if best != nil && !a.CreatedAt.After(bestAt) {
				continue
			}
			approvals, perr := s.cfg.ApprovalRepo.ListForStage(ctx, st.ID)
			if perr != nil {
				return nil, fmt.Errorf("list approvals for stage %s: %w", st.ID, perr)
			}
			hit := &groomingReportHit{
				stageID: st.ID, artifactID: a.ID,
				contentHash: a.ContentHash, content: a.Content,
			}
			for _, ap := range approvals {
				if ap == nil {
					continue
				}
				switch ap.Decision {
				case approval.DecisionApprove:
					hit.approveCount++
				case approval.DecisionReject:
					hit.rejectCount++
				}
			}
			// RATIFIED means at least one grant AND no rejection. A rejection
			// alongside a grant is NOT ratified: the gate was contested, and a
			// contested order is exactly the one an operator must re-decide.
			hit.approved = hit.approveCount > 0 && hit.rejectCount == 0
			best, bestAt = hit, a.CreatedAt
		}
	}
	return best, nil
}

// newerApprovedGroomingRun answers "has a newer approved grooming run of this
// workflow superseded the source order?" It returns the newest such run, a flag
// saying the scan could not PROVE absence, or a typed refusal.
//
// K2: A BOUNDED SCAN THAT MIGHT MISS IS NOT AN ANSWER. Building a campaign from
// a superseded order is precisely what this control exists to prevent, and
// annotating the response with "the scan was capped" relabels the silence
// rather than removing it. So the scan PAGES: it reads full pages until it sees
// a SHORT page — positive proof it reached the end of the workflow-scoped run
// list — and only then reports absence. If it burns through
// groomingSupersessionMaxPages full pages without reaching the end, it reports
// UNDETERMINED, which the caller turns into an unconditional refusal — the
// operator acknowledgement covers a NAMED superseding run, never a scan that
// did not finish.
//
// Any read failure (ListRuns, or a candidate's stages/artifacts/approvals) is a
// hard refusal that allow_superseded does NOT bypass: an operator can knowingly
// accept a stale order, but nobody can acknowledge a read that did not happen.
func (s *Server) newerApprovedGroomingRun(ctx context.Context, source *run.Run) (*uuid.UUID, bool, error) {
	unreadable := func(err error) *groomingSourceError {
		return &groomingSourceError{
			Status: http.StatusBadGateway, Code: codeGroomingSupersessionUnreadable,
			Message: "could not determine whether a newer approved grooming run supersedes this order",
			Details: map[string]any{"source_run_id": source.ID.String(), internalCauseKey: err.Error()},
		}
	}

	var candidates []*run.Run
	exhausted := false
	for page := 0; page < groomingSupersessionMaxPages; page++ {
		runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			Repo:      source.Repo,
			AccountID: source.AccountID,
			// Workflow-scoped for the same density reason groomingChurnBaseline
			// is: the repo string alone is diluted by every implement/review run
			// that shares it. Scoping also makes the "newer grooming order"
			// question well posed — only a run of the SAME grooming workflow can
			// supersede this one's order.
			WorkflowID: source.WorkflowID,
			Limit:      groomingSupersessionPageSize,
			Offset:     page * groomingSupersessionPageSize,
		})
		if err != nil {
			return nil, false, unreadable(err)
		}
		for _, rn := range runs {
			// STRICTLY NEWER in the same (created_at, id) total order the churn
			// baseline declares, so the predicate is total and deterministic
			// rather than dependent on clock resolution — and so the source run
			// never supersedes itself.
			if rn == nil || rn.ID == source.ID || !groomingRunPrecedes(source, rn) {
				continue
			}
			if !sameGroomingTenant(rn, source) {
				continue
			}
			candidates = append(candidates, rn)
		}
		if len(runs) < groomingSupersessionPageSize {
			exhausted = true
			break
		}
	}
	if !exhausted {
		// The list did not end within the page budget, so a newer approved run
		// may exist beyond it. Report UNDETERMINED rather than "not superseded".
		return nil, true, nil
	}

	// Newest first, id-tiebroken: the first candidate carrying an APPROVED
	// report is the one that superseded the source order.
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
		}
		return candidates[i].ID.String() > candidates[j].ID.String()
	})
	for _, rn := range candidates {
		hit, err := s.approvedGroomingReport(ctx, rn.ID)
		if err != nil {
			return nil, false, unreadable(err)
		}
		if hit != nil && hit.approved {
			id := rn.ID
			return &id, false, nil
		}
	}
	return nil, false, nil
}

// groomingDanglingDetails enriches the campaign_dangling_dependency 422 with
// the batch's PROVENANCE when the items came from a grooming order (AC7), so
// the operator message names the offending edge AND where the batch came from
// — and therefore which of the two remedies applies: widen the batch (raise or
// drop `limit` so the dependency's issue is included) or drop the edge.
func groomingDanglingDetails(details map[string]any, order *campaign.GroomingOrder) map[string]any {
	if order == nil {
		return details
	}
	if details == nil {
		details = map[string]any{}
	}
	details[danglingSourceKey] = danglingSourceGroomingOrder
	details[danglingGroomingRunKey] = order.RunID.String()
	if order.Limit > 0 {
		details[danglingGroomingLimitKey] = order.Limit
	}
	return details
}

// Detail keys the grooming-flavored campaign_dangling_dependency error adds on
// top of the #2120 edge lists. Like those, they are the CONTRACT between this
// producer and the fishhawk-mcp consumer, which branches its operator remedy on
// their presence rather than string-parsing the message.
const (
	danglingSourceKey           = "dangling_source"
	danglingGroomingRunKey      = "grooming_run_id"
	danglingGroomingLimitKey    = "grooming_order_limit"
	danglingSourceGroomingOrder = "grooming_order"
)

// compile-time guard: the grooming order derivation must stay PURE campaign
// package work. If this ever fails to compile because ReorderByPriority grew a
// context or a provider argument, the read-once boundary (AC3) has been broken.
var _ func(*workmgmt.EpicChildrenResult, []int) (*workmgmt.EpicChildrenResult, error) = campaign.ReorderByPriority
