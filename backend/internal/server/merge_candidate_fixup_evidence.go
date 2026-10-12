package server

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// resolveMergeCandidateFixupEvidence returns the recorded evidence of the
// FAILED merge-candidate verify pass the stage's live fix-up answers (E86.8 /
// #4250), for the slim fix-up prompt, or nil.
//
// The binding is a WINDOW on the audit chain. The stage's newest
// stage_fixup_triggered entry is the live fix-up (sequence Tn); the one before
// it is Tp (0 when there is none). The verdict this fix-up answers is the
// NEWEST same-stage merge_candidate_verified row with Tp < sequence < Tn — the
// verdict the live fix-up is the FIRST answer to, whether ADR-090 D6 routed it
// or an operator routed it with fishhawk_fixup_stage. Evidence is returned only
// when that verdict is `failed` and recorded failures, a tail or a summary.
//
// Nil for: no fix-up trigger; no verdict in the window; a newest-in-window
// verdict that is not failed; a failed verdict older than the previous fix-up
// (stale — an earlier fix-up already answered it); a verdict with no evidence;
// or any audit read or decode error, WARN-logged. Fail-open like every other
// prompt resolver: the evidence is advisory context, never a gate.
//
// The routed concern stays trusted-only (ADR-090 D6); this evidence reaches the
// agent through its own enveloped prompt section instead (the D6 amendment the
// Captain ruled on 2026-10-11).
func (s *Server) resolveMergeCandidateFixupEvidence(ctx context.Context, runID, stageID uuid.UUID) *prompt.MergeCandidateFailure {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	warn := func(msg string, err error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: merge-candidate fix-up evidence "+msg+" — rendering none",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
	fixups, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryStageFixupTriggered)
	if err != nil {
		warn("fix-up trigger read failed", err)
		return nil
	}
	if len(fixups) == 0 {
		return nil
	}
	live := fixups[len(fixups)-1].Sequence
	var previous int64
	if len(fixups) > 1 {
		previous = fixups[len(fixups)-2].Sequence
	}
	verdicts, err := s.stageEntriesStrict(ctx, runID, stageID, CategoryMergeCandidateVerified)
	if err != nil {
		warn("verdict read failed", err)
		return nil
	}
	for i := len(verdicts) - 1; i >= 0; i-- {
		v := verdicts[i]
		if v.Sequence >= live {
			continue
		}
		if v.Sequence <= previous {
			return nil
		}
		var p mergeCandidateVerifiedPayload
		if err := json.Unmarshal(v.Payload, &p); err != nil {
			warn("verdict decode failed", err)
			return nil
		}
		if p.Result != mergeCandidateResultFailed {
			return nil
		}
		mf := &prompt.MergeCandidateFailure{
			HeadSHA:       p.HeadSHA,
			Cause:         p.Cause,
			VerifyCommand: p.VerifyCommand,
			Failures:      p.Failures,
			OutputTail:    p.OutputTail,
		}
		if sum := p.FailureSummary; sum != nil {
			mf.FailingPackages = sum.Packages
			mf.FailingTests = sum.Tests
			mf.BuildFailedPackages = sum.BuildFailed
			mf.SummaryLossy = sum.Lossy
		}
		if !mf.HasEvidence() {
			return nil
		}
		return mf
	}
	return nil
}
