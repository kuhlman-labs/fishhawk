package server

import (
	"context"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// decomposedParentNoParentLevelVerifyReason is the DISTINCT machine literal a
// decomposed parent's implement review carries in
// GateEvidence.VerifyEvidenceUnavailableReason (#3132). It deliberately does NOT
// reuse one of resolveStageGateEvidence's transport-gap literals: on a fan-out
// parent the parent-level verify is absent BY CONSTRUCTION (the stage spawns no
// agent and produces no commit), not because an artifact failed to arrive, and a
// literal that says "transport gap" would name the wrong cause.
const decomposedParentNoParentLevelVerifyReason = "decomposed_parent_no_parent_level_verify"

// Per-slice named-degrade literals owned by THIS resolver, as distinct from the
// ones resolveStageGateEvidence returns (which are passed through verbatim).
const (
	childStageListFailedReason    = "child_stage_list_failed"
	childHasNoImplementStage      = "child_has_no_implement_stage"
	childEvidenceUnresolvedReason = "child_gate_evidence_unresolved"
)

// childSliceVerifyEvidence returns a decomposed PARENT run's per-slice
// committed-tree verify evidence (#3132), one record per fan-out CHILD, plus how
// many PASSING records the MaxSliceVerifyEntries bound dropped.
//
// A decomposed parent's implement stage is a FAN-OUT: it spawns no agent, writes
// no commit, and uploads no trace bundle, so bundle.ExtractGateEvidence has
// nothing to read on the parent's own chain and DispatchConsolidatedReview passes
// a nil GateEvidence. The consequence is the defect: the reviewer judges the
// consolidated diff — the LARGEST diffs the loop produces — with the tree's
// compile and test state entirely unknown. The per-slice verify DID run; each
// child uploads its own redacted trace carrying a gate_evidence event, and the
// existing resolveStageGateEvidence(runID, stageID) seam already reads exactly
// that, per run and per stage. This function propagates it.
//
// It mirrors childApprovedAmendmentScopePaths' best-effort, fail-open posture
// (#2820) — an evidence rollup must NEVER block a review. A nil RunRepo /
// AuditRepo / TraceStore or a listAllDecomposedChildren error contributes nothing
// (nil, 0); an ordinary non-decomposed run has no children and returns nil with
// no allocation, keeping the prompt byte-identical.
//
// NAMED-ABSENCE INVARIANT: every per-child degrade produces a ROW carrying a
// machine reason, NEVER a silently missing slice. A dropped slice reads to the
// reviewer as "this slice does not exist", which is the silent-absence class this
// change exists to close. A per-child failure therefore also never short-circuits
// its siblings.
//
// ORDERING + BOUND (operator binding condition 1). Records are ordered
// NON-PASSING FIRST — any slice whose terminal verify failed, whose summary
// outcome is `failed`, or that carries an UnavailableReason — then passing
// slices, each group stable in slice-index order. EVERY non-passing record is
// RETAINED, however many there are: MaxSliceVerifyEntries bounds the PASSING
// rows only, so the bound can never drop a row whose absence endangers a merge.
// A non-passing-first ordering with an unconditional trailing truncation would
// still eat non-passing rows once they outnumber the bound — and the rendered
// block asserts that every failed or unresolved slice is present, so that
// truncation would make the prompt lie. A fan-out with more non-passing slices
// than the bound therefore renders MORE than MaxSliceVerifyEntries rows and
// omits zero; the prompt-size bound is honoured for the case it exists for (a
// wide GREEN fan-out) and deliberately yielded when the alternative is a hidden
// failure.
func (s *Server) childSliceVerifyEvidence(ctx context.Context, parentRunID uuid.UUID) ([]prompt.GateSliceVerify, int) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.TraceStore == nil {
		return nil, 0
	}
	// listAllDecomposedChildren pages past the 100-row ListRuns cap
	// (consolidate.go), so a wide fan-out is fully covered rather than silently
	// truncated at the query layer.
	children, err := s.listAllDecomposedChildren(ctx, parentRunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"implement review: list decomposition children failed — per-slice verify evidence contributes nothing for this run",
			slog.String("run_id", parentRunID.String()),
			slog.String("error", err.Error()),
		)
		return nil, 0
	}
	if len(children) == 0 {
		// Not a decomposed parent — an ordinary run pays nothing and renders
		// nothing (byte-identical prompt).
		return nil, 0
	}

	// The SAME deterministic comparator childApprovedAmendmentScopePaths uses, so
	// repeated builds of the same parent render identically.
	sort.SliceStable(children, func(i, j int) bool {
		a, b := children[i], children[j]
		ai, bi := a.SliceIndex, b.SliceIndex
		switch {
		case ai != nil && bi != nil:
			if *ai != *bi {
				return *ai < *bi
			}
		case ai != nil && bi == nil:
			return true // known slice index sorts before an unknown one
		case ai == nil && bi != nil:
			return false
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID.String() < b.ID.String()
	})

	// Partition rather than sort-then-truncate: the bound is spent on the PASSING
	// rows only (binding condition 1), so no non-passing row can ever be dropped —
	// not even when the non-passing rows alone outnumber the bound. Appending in
	// child order inside each group keeps the slice-index ordering the comparator
	// above established.
	nonPassing := make([]prompt.GateSliceVerify, 0, len(children))
	passing := make([]prompt.GateSliceVerify, 0, len(children))
	for _, child := range children {
		rec := s.sliceVerifyForChild(ctx, parentRunID, child)
		if sliceVerifyNonPassing(rec) {
			nonPassing = append(nonPassing, rec)
			continue
		}
		passing = append(passing, rec)
	}
	budget := prompt.MaxSliceVerifyEntries - len(nonPassing)
	if budget < 0 {
		budget = 0
	}
	omitted := 0
	if len(passing) > budget {
		omitted = len(passing) - budget
		passing = passing[:budget]
	}
	return append(nonPassing, passing...), omitted
}

// sliceVerifyForChild resolves ONE child's record. It always returns a record —
// never an "omit this slice" signal — per the named-absence invariant.
func (s *Server) sliceVerifyForChild(ctx context.Context, parentRunID uuid.UUID, child *run.Run) prompt.GateSliceVerify {
	rec := prompt.GateSliceVerify{ChildRunID: child.ID.String()}
	if child.SliceIndex != nil {
		v := *child.SliceIndex
		rec.SliceIndex = &v
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, child.ID)
	if err != nil {
		// Best-effort per child: this slice renders as a named absence and its
		// SIBLINGS still resolve — never an all-or-nothing early return.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"implement review: list child stages failed — this slice renders as a named absence",
			slog.String("run_id", parentRunID.String()),
			slog.String("child_run_id", child.ID.String()),
			slog.String("error", err.Error()),
		)
		rec.UnavailableReason = childStageListFailedReason
		return rec
	}
	var impl *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			impl = st
			break
		}
	}
	if impl == nil {
		rec.UnavailableReason = childHasNoImplementStage
		return rec
	}
	rec.ChildStageState = string(impl.State)

	// The EXISTING seam, already parameterized by run id and stage id: this is a
	// new CALLER, not a behavior change.
	ev, reason := s.resolveStageGateEvidence(ctx, child.ID, impl.ID)
	if ev == nil {
		if reason == "" {
			// Defensive: resolveStageGateEvidence names every degrade, but an
			// unnamed nil must still produce a NAMED row rather than an
			// unexplained blank one.
			reason = childEvidenceUnresolvedReason
		}
		rec.UnavailableReason = reason
		return rec
	}
	rec.UnavailableReason = reason // "" on a full success; a partial literal otherwise

	// The head SHA rides the verify runs, and it must name the commit the
	// AUTHORITATIVE evidence ran against — the one the row's outcome is about. So
	// it is resolved from the SAME non-superseded runs the row renders, taking the
	// LAST (terminal-most) non-empty one: after a verify-fix iteration the earlier
	// run ran against an OLDER commit, and reading the raw list would label a
	// terminal PASSED outcome with the superseded FAILING iteration's SHA — the
	// exact misattribution `verified head` exists to prevent, and one a future
	// cross-check against the integration_commit_recorded ledger would compare
	// against the wrong commit. When no non-superseded run recorded a head_sha
	// (the gate-skipped / infra-failure paths emit an empty one) the field stays
	// EMPTY and the row renders "(not recorded)" rather than borrowing a
	// superseded iteration's SHA.
	for _, vr := range ev.VerifyRuns {
		if vr.Superseded {
			// An earlier iteration the verify-fix loop absorbed and re-ran is NOT
			// the committed-tree result for this slice; carrying it here would
			// invite the reviewer to read an absorbed failure as a slice failure.
			continue
		}
		if vr.HeadSHA != "" {
			rec.VerifiedHeadSHA = vr.HeadSHA
		}
		if vr.Outcome == "passed" {
			// A green command's tail carries nothing a reviewer needs, and N
			// slices x ~4KB is the bound this block has to respect.
			vr.OutputTail = ""
			vr.TailTruncated = false
		}
		rec.VerifyRuns = append(rec.VerifyRuns, vr)
	}
	if len(rec.VerifyRuns) > prompt.MaxSliceVerifyRunsPerSlice {
		// Keep the LAST N: the terminal run is the authoritative one.
		rec.VerifyRuns = rec.VerifyRuns[len(rec.VerifyRuns)-prompt.MaxSliceVerifyRunsPerSlice:]
	}
	rec.VerifySummary = ev.VerifySummary
	return rec
}

// sliceVerifyNonPassing reports whether a record must be ordered ahead of the
// passing ones so the entry bound can never drop it (binding condition 1): an
// unresolved slice, a `failed` summary, or a terminal (last, non-superseded)
// verify run whose outcome is not `passed`.
func sliceVerifyNonPassing(rec prompt.GateSliceVerify) bool {
	if rec.UnavailableReason != "" {
		return true
	}
	if rec.VerifySummary != nil && rec.VerifySummary.Outcome == "failed" {
		return true
	}
	if n := len(rec.VerifyRuns); n > 0 && rec.VerifyRuns[n-1].Outcome != "passed" {
		return true
	}
	return false
}
