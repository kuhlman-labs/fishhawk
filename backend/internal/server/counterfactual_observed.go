package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// CategoryCounterfactualObserved is the stage-scoped audit row recording ONE
// runner-executed, plan-declared counterfactual (E85.3 / #4143). It is appended
// at RAW trace upload from the bundle's gate_evidence.counterfactual_observations
// member — the runner's own execution record inside the signed bundle, not an
// agent-claimed sidecar — one row per observation. INTERNAL: no issue-comment
// surface reads it.
const CategoryCounterfactualObserved = "counterfactual_observed"

// controlUnpinnedConcernCategory is the review-concern category a GREEN
// counterfactual raises: mutating the declared control left the declared tests
// passing, so nothing pins the control. It is a concern classifier on the
// tolerant review_concerns.category column, NOT an audit category — the
// ConcernCategory suffix keeps the audit-category registry sweep off it.
const controlUnpinnedConcernCategory = "control_unpinned"

// counterfactualObservedGreen is the only observed value that raises a concern.
// red passes silently; timeout and error are recorded and raise nothing.
const counterfactualObservedGreen = "green"

// counterfactualObservedSource stamps every row as derived from the runner's
// signed bundle, distinguishing it from the agent's #2929 self-report.
const counterfactualObservedSource = "runner"

// counterfactualRaiseMu serializes the read-check-insert of
// raiseControlUnpinnedConcern so two concurrent raw uploads for one stage
// cannot both observe a check key absent and mint a duplicate. It holds within
// ONE process only (the diffSecretsRaiseMu residual).
var counterfactualRaiseMu sync.Mutex

// counterfactualObservedPayload is the counterfactual_observed audit payload:
// the stage + bundle coordinates, the source, and the flat evidence member,
// embedded so its fields sit at the payload's top level.
type counterfactualObservedPayload struct {
	StageID     string `json:"stage_id"`
	ContentHash string `json:"content_hash"`
	Source      string `json:"source"`
	bundle.CounterfactualObservationEvidence
}

// controlUnpinnedCheckKey is the server_check de-duplication key of a GREEN
// observation: one concern per (stage, index, control_path), however many
// bundles re-observe it.
func controlUnpinnedCheckKey(obs bundle.CounterfactualObservationEvidence) string {
	return controlUnpinnedConcernCategory + "|" + strconv.Itoa(obs.Index) + "|" + obs.ControlPath
}

// recordCounterfactualObservations appends one counterfactual_observed row per
// observation a raw trace bundle carries, and raises a control_unpinned
// server_check concern for each GREEN one. The trace handler calls it inside
// its raw-variant guard right after recordGateIsolation, BEFORE the budget
// short-circuits and the agent-failed branch, so a recorded observation is on
// the chain before any stage failure.
//
// Best-effort throughout — the bundle is already stored and audited:
//   - no AuditRepo, no gate_evidence, an unparsable gate_evidence (WARN), or a
//     gate_evidence without the member → no row;
//   - an observation whose (stage_id, content_hash, index) row already exists
//     → skipped, so a re-POST of the same raw bundle does not duplicate it (and
//     does not re-raise);
//   - a failed dedup read WARN-logs and appends every observation (a duplicate
//     row is harmless; a lost record is not);
//   - a failed append WARN-logs, skips that observation's concern (no concern
//     without its origin row) and continues with the next.
func (s *Server) recordCounterfactualObservations(ctx context.Context, runID, stageID uuid.UUID, contentHash string, bundleBytes []byte) {
	if s.cfg.AuditRepo == nil {
		return
	}
	ev, err := bundle.ExtractGateEvidence(bundleBytes)
	if err != nil {
		if !errors.Is(err, bundle.ErrNoGateEvidence) {
			s.cfg.Logger.Warn("counterfactuals: extract gate evidence failed; not recording counterfactual_observed",
				"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
		}
		return
	}
	if len(ev.CounterfactualObservations) == 0 {
		return
	}
	recorded := s.counterfactualObservationsRecorded(ctx, runID, stageID, contentHash)
	systemKind := audit.ActorKind("system")
	for _, obs := range ev.CounterfactualObservations {
		if recorded[obs.Index] {
			continue
		}
		payload, err := json.Marshal(counterfactualObservedPayload{
			StageID:                           stageID.String(),
			ContentHash:                       contentHash,
			Source:                            counterfactualObservedSource,
			CounterfactualObservationEvidence: obs,
		})
		if err != nil {
			s.cfg.Logger.Warn("counterfactuals: marshal payload failed",
				"run_id", runID.String(), "stage_id", stageID.String(), "index", obs.Index, "error", err.Error())
			continue
		}
		entry, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &stageID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryCounterfactualObserved,
			ActorKind: &systemKind,
			Payload:   payload,
		})
		if err != nil {
			s.cfg.Logger.Warn("counterfactuals: append counterfactual_observed failed — no concern raised for it",
				"run_id", runID.String(), "stage_id", stageID.String(), "index", obs.Index, "error", err.Error())
			continue
		}
		if obs.Observed == counterfactualObservedGreen {
			s.raiseControlUnpinnedConcern(ctx, runID, stageID, entry.Sequence, obs)
		}
	}
}

// counterfactualObservationsRecorded returns the observation indexes already
// recorded for (stage_id, content_hash). A read failure returns nil (record
// everything — see recordCounterfactualObservations).
func (s *Server) counterfactualObservationsRecorded(ctx context.Context, runID, stageID uuid.UUID, contentHash string) map[int]bool {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCounterfactualObserved)
	if err != nil {
		s.cfg.Logger.Warn("counterfactuals: dedup read failed; appending without dedup",
			"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
		return nil
	}
	recorded := map[int]bool{}
	for _, e := range entries {
		var p struct {
			StageID     string `json:"stage_id"`
			ContentHash string `json:"content_hash"`
			Index       int    `json:"index"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.StageID == stageID.String() && p.ContentHash == contentHash {
			recorded[p.Index] = true
		}
	}
	return recorded
}

// raiseControlUnpinnedConcern raises ONE server_check implement concern
// (severity medium, category control_unpinned) for a GREEN observation, its
// OriginReviewSequence the observation's counterfactual_observed row. Being
// server_check, only a human can waive or defer it, and while it is open the
// delegated approve/merge settlement cannot fire.
//
// De-duplication: skipped when a server_check row with the same check key is
// on THIS stage in an open, waived or deferred state. A ListByRun error fails
// OPEN to raising: a duplicate concern is noise, a missed unpinned control is
// silent. An InsertRaised failure WARN-logs; the audit row stays the record.
func (s *Server) raiseControlUnpinnedConcern(ctx context.Context, runID, stageID uuid.UUID, originSeq int64, obs bundle.CounterfactualObservationEvidence) {
	if s.cfg.ConcernRepo == nil {
		return
	}
	key := controlUnpinnedCheckKey(obs)

	counterfactualRaiseMu.Lock()
	defer counterfactualRaiseMu.Unlock()

	if rows, err := s.cfg.ConcernRepo.ListByRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "counterfactuals: list concerns failed — raising without de-duplication",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
	} else {
		for _, row := range rows {
			if row == nil || row.StageID != stageID || !row.IsServerCheck() || row.CheckKey != key {
				continue
			}
			if row.State.IsOpen() || row.State == concern.StateWaived || row.State == concern.StateDeferred {
				return
			}
		}
	}

	if _, err := s.cfg.ConcernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            concern.StageKindImplement,
		Provenance:           concern.ProvenanceServerCheck,
		OriginReviewSequence: originSeq,
		Concerns: []concern.RaisedConcern{{
			Severity: "medium",
			Category: controlUnpinnedConcernCategory,
			Note:     controlUnpinnedNote(obs),
			CheckKey: key,
		}},
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "counterfactuals: insert control_unpinned concern failed — the counterfactual_observed row is the record",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int64("origin_sequence", originSeq),
			slog.String("error", err.Error()),
		)
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "counterfactuals: a plan-declared counterfactual stayed green — control_unpinned concern raised",
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.Int("index", obs.Index),
		slog.String("control_path", obs.ControlPath),
	)
}

// controlUnpinnedNote renders the concern note: which counterfactual, the
// control it mutated, the selector and package that stayed green, and the
// short head SHA it ran at.
func controlUnpinnedNote(obs bundle.CounterfactualObservationEvidence) string {
	head := obs.HeadSHA
	if len(head) > 12 {
		head = head[:12]
	}
	if head == "" {
		head = "unknown"
	}
	return "plan-declared counterfactual #" + strconv.Itoa(obs.Index) + " stayed GREEN: the runner mutated " +
		obs.ControlPath + " and `go test -run " + obs.TestRun + " " + obs.TestPackage +
		"` still passed at head " + head + ", so no named test pins this control. " +
		"Add a test that fails when the control is removed, or waive with a reason (human only)."
}
