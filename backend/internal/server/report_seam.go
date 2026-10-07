package server

// The shared proposal-report seam (#4012, E3775 phase 2).
//
// A proposal-report role (upkeep today, comms next) ingests a report artifact
// on a stage that DECLARES it, records it on the run's chain, captures the
// captain's per-entry dispositions inside a window, and on approval settles
// that window and files the approved entries. The helpers below are that
// protocol with the report KIND, the recorded-row CATEGORY and the window
// FAMILY as parameters, so a new role reuses them instead of a third copy. The
// upkeep files keep every existing function as a thin wrapper over these, and
// their tests are the behavior-preservation proof. Contract:
// backend/internal/server/README.md § "Shared proposal-report seam".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// ---------------------------------------------------------------------------
// Stage binding
// ---------------------------------------------------------------------------

// Undecidable reasons carried by stageArtifactBinding. Internal: each role's
// ingest collapses both into its own caller-visible refusal.
const (
	stageBindingWorkflowUnresolved = "workflow_unresolved"
	stageBindingStageUnmappable    = "stage_unmappable"
)

// stageArtifactBinding is what the run's cached workflow spec says about one
// stage and one report artifact kind.
//
//   - Resolved: the run row, its cached spec and the run's workflow were all
//     found and parsed. False means there is no declaration to read
//     (Undecidable is then workflow_unresolved).
//   - WorkflowDeclares: some stage of the run's workflow declares
//     `produces: <kind>`.
//   - StageDeclares: THIS stage does.
//   - Undecidable: non-empty when the declaration could not be resolved for
//     this stage (workflow_unresolved, stage_unmappable).
type stageArtifactBinding struct {
	Resolved         bool
	WorkflowDeclares bool
	StageDeclares    bool
	Undecidable      string
}

// resolveStageArtifactBinding reads the stage's `produces: <kind>` declaration
// from the run's cached spec via resolveRunWorkflowDef and
// specStageForRunStage.
//
// The returned error is reserved for a store that did not ANSWER (a GetRun or
// ListStagesForRun transport failure). Every "no declaration exists" leg (nil
// RunRepo, no run row, no cached spec, unparseable spec, workflow absent) is
// Resolved=false with Undecidable=workflow_unresolved, and a stage the spec
// cannot be mapped onto is Undecidable=stage_unmappable — each caller decides
// its own posture from that.
//
// ListStagesForRun is called ONLY when the workflow declares kind on some
// stage, so ordinary workflows pay one GetRun and one spec parse and never
// list stages.
func (s *Server) resolveStageArtifactBinding(ctx context.Context, runID uuid.UUID, stage *run.Stage, kind spec.ArtifactKind) (stageArtifactBinding, error) {
	_, wf, _, _, ok, err := s.resolveRunWorkflowDef(ctx, runID)
	if err != nil {
		return stageArtifactBinding{}, err
	}
	if !ok {
		return stageArtifactBinding{Undecidable: stageBindingWorkflowUnresolved}, nil
	}
	b := stageArtifactBinding{Resolved: true}
	for _, st := range wf.Stages {
		if stageProducesKind(st, kind) {
			b.WorkflowDeclares = true
			break
		}
	}
	if !b.WorkflowDeclares {
		return b, nil
	}
	rows, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return stageArtifactBinding{}, fmt.Errorf("list stages for run %s: %w", runID, err)
	}
	sp, mapped := specStageForRunStage(wf, rows, stage)
	if !mapped {
		b.Undecidable = stageBindingStageUnmappable
		return b, nil
	}
	b.StageDeclares = stageProducesKind(sp, kind)
	return b, nil
}

// stageProducesKind reports whether st declares `produces: <kind>`. Local so
// the seam needs no per-kind predicate in package spec.
func stageProducesKind(st spec.Stage, kind spec.ArtifactKind) bool {
	for _, p := range st.Produces {
		if p.Artifact == kind {
			return true
		}
	}
	return false
}

// stageRefusesOtherProposal is the plan-path guard's decision: true when the
// stage must NOT have any artifact outside the role's allowlist ingested.
// Fails OPEN when nothing is resolvable (Resolved=false), so runs that cannot
// resolve a workflow keep today's path; fails CLOSED inside a workflow that
// declares the kind, where a stage that declares it, or one that cannot be
// mapped, refuses.
func stageRefusesOtherProposal(b stageArtifactBinding) bool {
	return b.Resolved && b.WorkflowDeclares && (b.StageDeclares || b.Undecidable != "")
}

// ---------------------------------------------------------------------------
// Ingest helpers
// ---------------------------------------------------------------------------

// proposalGuardMaxKindBytes bounds the agent-supplied top-level kind that
// proposalGuardBodyDetail echoes back (#3922 approval condition 6): the text
// reaches the 400, the stage's failure reason and the schema-retry feedback.
const proposalGuardMaxKindBytes = 64

// proposalGuardBodyDetail says why a body refused on a stage declaring
// `produces: <reportKind>` was read as a plan: derr non-nil means it did not
// parse (the parse error is kept verbatim); otherwise it carries no top-level
// "kind", carries a kind plan.AllArtifactKinds recognizes that allowed does
// not admit, or carries one nothing recognizes, which is echoed truncated to
// proposalGuardMaxKindBytes. Pure; derr is plan.DetectArtifactKind's error.
func proposalGuardBodyDetail(body []byte, derr error, reportKind string, allowed map[plan.ArtifactKind]bool) string {
	if derr != nil {
		return "the body is not a parseable " + reportKind + " (" + derr.Error() + ")"
	}
	var disc struct {
		Kind string `json:"kind"`
	}
	// DetectArtifactKind already decoded this body into the same shape, so a
	// decode error here is unreachable; it reads as kind-less either way.
	_ = json.Unmarshal(body, &disc)
	if disc.Kind == "" {
		return `the body carries no top-level "kind", so it was read as a plan`
	}
	if recognizedArtifactKind(disc.Kind) && !allowed[plan.ArtifactKind(disc.Kind)] {
		// A recognized kind is a constant, never agent-chosen text, so it is
		// echoed whole.
		return fmt.Sprintf("its top-level kind %q is a recognized artifact kind but is not allowed on a stage declaring produces: %s", disc.Kind, reportKind)
	}
	k := disc.Kind
	if len(k) > proposalGuardMaxKindBytes {
		k = strings.ToValidUTF8(k[:proposalGuardMaxKindBytes], "") + "...[truncated]"
	}
	return fmt.Sprintf("its top-level kind %q is not a recognized artifact kind", k)
}

// recognizedArtifactKind reports whether kind is one of plan.AllArtifactKinds
// — the enumeration a new plan-stage sibling must join — so the guard detail
// tracks the recognized set without a second list.
func recognizedArtifactKind(kind string) bool {
	for _, k := range plan.AllArtifactKinds() {
		if string(k) == kind {
			return true
		}
	}
	return false
}

// reportRowsListError is recordedReportRow's store-read failure. Its text is
// "list <category> rows: <err>". A caller that words the failure with the run
// id (latestUpkeepReport) rewraps Err in its own text instead of wrapping this
// error, so no message is double-wrapped.
type reportRowsListError struct {
	Category string
	Err      error
}

func (e *reportRowsListError) Error() string {
	return fmt.Sprintf("list %s rows: %v", e.Category, e.Err)
}

func (e *reportRowsListError) Unwrap() error { return e.Err }

// recordedReportRow returns the HIGHEST-sequence row of category on the run's
// chain (a tie, impossible on one chain, takes the later-listed row). With
// artifactID non-empty only rows whose payload names artifactID compete (an
// undecodable row is skipped) and no such row is an error, the fail-closed
// direction; with artifactID empty every row competes and no row at all is
// (nil, nil), the caller's absence signal. A list failure is a
// *reportRowsListError.
func (s *Server) recordedReportRow(ctx context.Context, runID uuid.UUID, category, artifactID string) (*audit.Entry, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return nil, &reportRowsListError{Category: category, Err: err}
	}
	var best *audit.Entry
	for _, e := range rows {
		if e == nil {
			continue
		}
		if artifactID != "" {
			var probe struct {
				ArtifactID string `json:"artifact_id"`
			}
			if json.Unmarshal(e.Payload, &probe) != nil || probe.ArtifactID != artifactID {
				continue
			}
		}
		if best == nil || e.Sequence >= best.Sequence {
			best = e
		}
	}
	if best == nil && artifactID != "" {
		return nil, fmt.Errorf("no %s row names artifact %s", category, artifactID)
	}
	return best, nil
}

// recordedRowArtifactID decodes the artifact id a recorded report row names.
// An undecodable payload or an unparseable id is an error naming the row's
// sequence, never a silent fallback to an older row.
func recordedRowArtifactID(e *audit.Entry, category string) (uuid.UUID, error) {
	var p struct {
		ArtifactID string `json:"artifact_id"`
	}
	if jerr := json.Unmarshal(e.Payload, &p); jerr != nil {
		return uuid.Nil, fmt.Errorf("decode %s row %d: %w", category, e.Sequence, jerr)
	}
	id, perr := uuid.Parse(p.ArtifactID)
	if perr != nil {
		return uuid.Nil, fmt.Errorf("%s row %d names artifact %q: %w", category, e.Sequence, p.ArtifactID, perr)
	}
	return id, nil
}

// recordedReportArtifact reads the artifact id named by the HIGHEST-sequence
// row of category, without reading the report body. found=false with a nil
// error means no row.
func (s *Server) recordedReportArtifact(ctx context.Context, runID uuid.UUID, category string) (uuid.UUID, bool, error) {
	row, err := s.recordedReportRow(ctx, runID, category, "")
	if err != nil {
		return uuid.Nil, false, err
	}
	if row == nil {
		return uuid.Nil, false, nil
	}
	id, err := recordedRowArtifactID(row, category)
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// decodeStrictSingleJSONBody is a capture's STRICT single-document decode: an
// empty body decodes to the zero value (the caller's empty-batch rung then
// decides), DisallowUnknownFields applies at EVERY depth, and trailing content
// after the first document is refused. shapeMsg is the 400 message for an
// undecodable body or an unknown key, trailingMsg the one for trailing
// content; both come from the caller so each role names its own request
// shape.
func (s *Server) decodeStrictSingleJSONBody(w http.ResponseWriter, r *http.Request, dst any, shapeMsg, trailingMsg string) bool {
	if r.Body == nil {
		return true
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	decErr := dec.Decode(dst)
	switch {
	case decErr != nil && !errors.Is(decErr, io.EOF):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", shapeMsg,
			map[string]any{"error": decErr.Error()})
		return false
	case decErr == nil:
		var trailing json.RawMessage
		if tErr := dec.Decode(&trailing); !errors.Is(tErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed", trailingMsg,
				map[string]any{"field": "body"})
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Apply helpers
// ---------------------------------------------------------------------------

// reportGateOutcome is reportGateRatified's verdict. The zero value is NOT
// ratified, so an unset outcome fails closed.
type reportGateOutcome int

const (
	// reportGateContestedOrUngranted: the gate carries no grant, or a
	// rejection contests it.
	reportGateContestedOrUngranted reportGateOutcome = iota
	// reportGateNoRepository: no approval repository is configured.
	reportGateNoRepository
	// reportGateUnreadable: the stage's approval rows could not be read; the
	// error is returned alongside.
	reportGateUnreadable
	// reportGateRatifiedOK: at least one grant and no rejection.
	reportGateRatifiedOK
)

// reportGateRatified re-ratifies a report gate from the stage's approval rows.
// The submission is not the gate: a gate contested by a rejection is not
// ratified even when the latest submission is a grant. The error is non-nil
// only for reportGateUnreadable. Each role words its own degrade from the
// outcome.
func (s *Server) reportGateRatified(ctx context.Context, stageID uuid.UUID) (reportGateOutcome, error) {
	if s.cfg.ApprovalRepo == nil {
		return reportGateNoRepository, nil
	}
	approvals, aerr := s.cfg.ApprovalRepo.ListForStage(ctx, stageID)
	if aerr != nil {
		return reportGateUnreadable, aerr
	}
	grants, rejections := 0, 0
	for _, ap := range approvals {
		if ap == nil {
			continue
		}
		switch ap.Decision {
		case approval.DecisionApprove:
			grants++
		case approval.DecisionReject:
			rejections++
		}
	}
	if grants == 0 || rejections > 0 {
		return reportGateContestedOrUngranted, nil
	}
	return reportGateRatifiedOK, nil
}

// reportWindowCloser is an atomic window close: the watermark entry and the
// consumed dispositions ({this artifact, below the watermark}), with an
// existing watermark returned unchanged.
type reportWindowCloser func(ctx context.Context, p audit.ChainAppendParams, artifactID string) (*audit.Entry, []*audit.Entry, error)

// reportWindowFamily describes one role's disposition window to
// settleReportWindow: the audit family name (whose categories
// audit.LookupWindowFamily supplies) and how to find the atomic close on the
// configured repository. atomicClose returning ok=false selects the
// non-atomic fallback.
type reportWindowFamily struct {
	name        string
	atomicClose func(audit.Repository) (reportWindowCloser, bool)
}

// upkeepWindowFamily asserts the TYPED audit.UpkeepWindowAppender, not
// audit.FamilyWindowAppender: the server's upkeep test fakes implement only the
// typed capability, and switching the assertion would silently move them onto
// the fallback.
var upkeepWindowFamily = reportWindowFamily{
	name: audit.WindowFamilyUpkeep,
	atomicClose: func(repo audit.Repository) (reportWindowCloser, bool) {
		a, ok := repo.(audit.UpkeepWindowAppender)
		if !ok {
			return nil, false
		}
		return a.AppendChainedUpkeepWindowClose, true
	},
}

// commsWindowFamily is the comms role's window (#4012; consumed by the comms
// apply, phase 7). It asserts the GENERIC audit.FamilyWindowAppender with the
// comms family name.
var commsWindowFamily = reportWindowFamily{
	name:        audit.WindowFamilyComms,
	atomicClose: genericFamilyCloser(audit.WindowFamilyComms),
}

// genericFamilyCloser returns an atomicClose that drives
// audit.FamilyWindowAppender for family.
func genericFamilyCloser(family string) func(audit.Repository) (reportWindowCloser, bool) {
	return func(repo audit.Repository) (reportWindowCloser, bool) {
		a, ok := repo.(audit.FamilyWindowAppender)
		if !ok {
			return nil, false
		}
		return func(ctx context.Context, p audit.ChainAppendParams, artifactID string) (*audit.Entry, []*audit.Entry, error) {
			return a.AppendChainedFamilyWindowClose(ctx, family, p, artifactID)
		}, true
	}
}

// settleReportWindow closes artifactID's capture window for fam with the
// given settlement and returns the consumed disposition rows; the caller
// collapses them (collapseConsumed), which also drops rows of other
// artifacts.
//
//   - A family audit.LookupWindowFamily does not know is refused with
//     *audit.UnknownWindowFamilyError BEFORE any write.
//   - With the family's atomic capability present (production) it drives that
//     close: one transaction under the run-row lock.
//   - Otherwise (in-memory repos) it runs a non-atomic, permanence-aware
//     read-then-append over the family's watermark and disposition
//     categories: an existing watermark is reused, never extending the bound
//     past the first one.
func (s *Server) settleReportWindow(ctx context.Context, fam reportWindowFamily, stage *run.Stage, artifactID, settlement string) ([]*audit.Entry, error) {
	info, ok := audit.LookupWindowFamily(fam.name)
	if !ok {
		return nil, &audit.UnknownWindowFamilyError{Family: fam.name}
	}
	now := time.Now().UTC()
	payload, err := json.Marshal(groomingWindowPayload{
		RunID: stage.RunID.String(), StageID: stage.ID.String(),
		ArtifactID: artifactID, Settlement: settlement,
		ClosedAt: now.Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	systemKind := audit.ActorSystem
	stageID := stage.ID
	params := audit.ChainAppendParams{
		RunID: stage.RunID, StageID: &stageID, Timestamp: now,
		Category: info.WatermarkCategory, ActorKind: &systemKind, Payload: payload,
	}

	if fam.atomicClose != nil {
		if closeFn, ok := fam.atomicClose(s.cfg.AuditRepo); ok {
			_, consumed, aerr := closeFn(ctx, params, artifactID)
			if aerr != nil {
				return nil, aerr
			}
			return consumed, nil
		}
	}

	// FALLBACK (in-memory repos): permanence-aware read-then-append.
	existing, err := s.windowSettlementFor(ctx, stage.RunID, info.WatermarkCategory, artifactID)
	if err != nil {
		return nil, err
	}
	var belowSeq int64
	if existing != nil {
		belowSeq = existing.AuditSequence // PERMANENCE: never extend the bound.
	} else {
		wm, aerr := s.cfg.AuditRepo.AppendChained(ctx, params)
		if aerr != nil {
			return nil, aerr
		}
		belowSeq = wm.Sequence
	}
	disp, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, stage.RunID, info.DispositionCategory)
	if err != nil {
		return nil, err
	}
	scoped := make([]*audit.Entry, 0, len(disp))
	for _, e := range disp {
		if e != nil && e.Sequence < belowSeq {
			scoped = append(scoped, e)
		}
	}
	return scoped, nil
}

// collapseConsumed collapses consumed disposition rows of artifactID
// LAST-WINS by chain sequence per entry id. decode reads one row's payload;
// a row it cannot decode (ok=false), one without an id, and one naming
// another artifact are skipped, so a junk row never manufactures a verdict.
func collapseConsumed[T any](entries []*audit.Entry, artifactID string, decode func(payload []byte) (id, artifactID string, v T, ok bool)) map[string]T {
	type ranked struct {
		v T
		s int64
	}
	latest := map[string]ranked{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		id, art, v, ok := decode(e.Payload)
		if !ok || id == "" || art != artifactID {
			continue
		}
		if cur, seen := latest[id]; seen && cur.s > e.Sequence {
			continue
		}
		latest[id] = ranked{v, e.Sequence}
	}
	out := make(map[string]T, len(latest))
	for id, r := range latest {
		out[id] = r.v
	}
	return out
}

// runScopedWorkTarget builds the run-scoped work-management target the way
// handleFileWorkItem's run-scoped path does: coordinates from owner/name (the
// caller's split of the run's repo — this helper never re-splits it),
// provider connections from conv, the credential scope from the run's
// installation, else resolved for the GitHub provider. A failed installation
// lookup returns the error with a ZERO scope and the rest of the target
// filled; each caller keeps its own posture (the dedupe degrades, the filing
// proceeds and the provider fails closed per finding).
func (s *Server) runScopedWorkTarget(ctx context.Context, runRow *run.Run, owner, name string, conv workmgmt.Conventions) (workmgmt.Target, error) {
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		GitLab:  conv.GitLab,
	}
	if runRow != nil && runRow.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil && conv.Provider == workmgmtgithub.ProviderName {
		scope, err := s.resolveRepoScope(ctx, owner, name)
		if err != nil {
			return target, err
		}
		target.Scope = scope
	}
	return target, nil
}

// startDetachedReportApply runs fn on a goroutine tracked by bgReportApply,
// which Shutdown drains (bounded by the shutdown context). fn's context is
// DETACHED from base's cancellation and bounded by budget, so neither a client
// disconnect nor a caller's prelaunch deadline strands the apply.
func (s *Server) startDetachedReportApply(base context.Context, budget time.Duration, fn func(ctx context.Context)) {
	s.bgReportApply.Add(1)
	go func() {
		defer s.bgReportApply.Done()
		applyCtx, cancel := context.WithTimeout(context.WithoutCancel(base), budget)
		defer cancel()
		fn(applyCtx)
	}()
}
