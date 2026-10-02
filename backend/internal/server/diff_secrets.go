package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diffsecrets"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// diffSecretsDetectedCategory is the origin audit entry of a diff secrets
// check raise (E80.3 / #3760): one entry per review round that raised at least
// one NEW server_check concern, system actor, stage-anchored. Its sequence is
// the concerns' OriginReviewSequence. INTERNAL: no issue-comment activity line,
// so it is not an issue-comment surface. The payload names locations and
// pattern classes only — never matched bytes.
const diffSecretsDetectedCategory = "diff_secrets_detected"

// diffSecretsRaiseMu serializes the read-check-insert of raiseDiffSecretConcerns
// so two concurrent rounds for one stage (the trace-time hook and the fix-up
// re-review backstop) cannot both observe a check key absent and mint a
// duplicate. It holds within ONE process only — a multi-replica deployment can
// still race (documented as a residual in server/README.md).
var diffSecretsRaiseMu sync.Mutex

// diffSecretsDetectedPayload is the diff_secrets_detected audit payload.
type diffSecretsDetectedPayload struct {
	Check          string                    `json:"check"`
	HeadSHA        string                    `json:"head_sha,omitempty"`
	PatchTruncated bool                      `json:"patch_truncated"`
	HitCount       int                       `json:"hit_count"`
	Groups         []diffSecretsGroupPayload `json:"groups"`
}

type diffSecretsGroupPayload struct {
	Path     string `json:"path"`
	Lines    []int  `json:"lines"`
	Pattern  string `json:"pattern"`
	CheckKey string `json:"check_key"`
}

// raiseDiffSecretConcerns runs the deterministic diff secrets check (ADR-084
// D5 / rule 5, E80.3 / #3760) over the ADDED lines of diff.Patch with
// redaction.DefaultPatterns — no model call — and raises one server_check
// implement concern (severity high, category security) per (file, pattern)
// group not already on record for this stage.
//
// De-duplication: a group is skipped when a server_check row with the same
// check_key exists on THIS stage in an open, waived or deferred state. A
// superseded or addressed row does not suppress a re-raise. A ListByRun error
// fails OPEN to raising: a duplicate concern is noise, a missed secret is
// silent.
//
// Ordering: the diff_secrets_detected audit entry is appended FIRST and its
// sequence stamps the concerns. An append failure WARN-logs and raises nothing
// (no concern without its origin record); an InsertRaised failure WARN-logs and
// leaves the audit entry as the authoritative record.
//
// Returns every group the round's diff carries (deduplicated or not) — the
// round's hits, the seam a later persona attachment (E80.5 / #3762) consumes.
// nil when nothing was scanned or nothing matched. Logs carry run/stage ids,
// counts, paths, line numbers and pattern names only.
func (s *Server) raiseDiffSecretConcerns(ctx context.Context, runID, stageID uuid.UUID, diff policy.Diff, headSHA string) []diffsecrets.Group {
	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}
	if diff.Patch == "" {
		if len(diff.ChangedFiles) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: diff carries no patch text — added lines not scanned",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int("changed_files", len(diff.ChangedFiles)),
			)
		}
		return nil
	}
	res := diffsecrets.Scan(diff.Patch, redaction.DefaultPatterns)
	if len(res.Hits) == 0 {
		return nil
	}
	groups := diffsecrets.GroupHits(res.Hits)

	diffSecretsRaiseMu.Lock()
	defer diffSecretsRaiseMu.Unlock()

	fresh := groups
	if rows, err := s.cfg.ConcernRepo.ListByRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: list concerns failed — raising without de-duplication",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
	} else {
		recorded := map[string]bool{}
		for _, row := range rows {
			if row == nil || row.StageID != stageID || !row.IsServerCheck() || row.CheckKey == "" {
				continue
			}
			if row.State.IsOpen() || row.State == concern.StateWaived || row.State == concern.StateDeferred {
				recorded[row.CheckKey] = true
			}
		}
		fresh = nil
		for _, g := range groups {
			if !recorded[g.Key()] {
				fresh = append(fresh, g)
			}
		}
	}
	if len(fresh) == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "diff secrets check: every hit is already on record for this stage",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("hit_count", len(res.Hits)),
			slog.Int("group_count", len(groups)),
		)
		return groups
	}

	hitCount := 0
	payloadGroups := make([]diffSecretsGroupPayload, 0, len(fresh))
	for _, g := range fresh {
		hitCount += len(g.Lines)
		payloadGroups = append(payloadGroups, diffSecretsGroupPayload{
			Path: g.Path, Lines: g.Lines, Pattern: g.Pattern, CheckKey: g.Key(),
		})
	}
	payload, _ := json.Marshal(diffSecretsDetectedPayload{
		Check:          diffsecrets.CheckName,
		HeadSHA:        headSHA,
		PatchTruncated: diff.PatchTruncated,
		HitCount:       hitCount,
		Groups:         payloadGroups,
	})
	systemKind := audit.ActorKind("system")
	entry, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  diffSecretsDetectedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: append diff_secrets_detected failed — no concern raised this round",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("group_count", len(fresh)),
			slog.String("error", err.Error()),
		)
		return groups
	}

	raised := make([]concern.RaisedConcern, 0, len(fresh))
	for _, g := range fresh {
		raised = append(raised, concern.RaisedConcern{
			Severity: "high",
			Category: "security",
			Note:     diffsecrets.Note(g),
			CheckKey: g.Key(),
		})
	}
	if _, ierr := s.cfg.ConcernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            concern.StageKindImplement,
		Provenance:           concern.ProvenanceServerCheck,
		OriginReviewSequence: entry.Sequence,
		Concerns:             raised,
	}); ierr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: insert concerns failed — the diff_secrets_detected entry is the record",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int64("origin_sequence", entry.Sequence),
			slog.String("error", ierr.Error()),
		)
		return groups
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: credential-shaped additions raised as server_check concerns",
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.Int("hit_count", hitCount),
		slog.Int("raised", len(fresh)),
		slog.String("locations", diffSecretsLocations(fresh)),
		slog.Bool("patch_truncated", diff.PatchTruncated),
	)
	return groups
}

// diffSecretsLocations renders "path:l1,l2 (pattern)" per group for the log.
func diffSecretsLocations(groups []diffsecrets.Group) string {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		lines := make([]string, 0, len(g.Lines))
		for _, l := range g.Lines {
			lines = append(lines, strconv.Itoa(l))
		}
		parts = append(parts, g.Path+":"+strings.Join(lines, ",")+" ("+g.Pattern+")")
	}
	return strings.Join(parts, "; ")
}

// redactReviewPatch passes the review prompt's diff through
// redaction.RedactDefault so a credential the diff secrets check found never
// reaches a model reviewer verbatim (E80.3 / #3760). A diff with no
// credential-shaped string comes back byte-identical. Logs per-pattern hit
// counts only.
func (s *Server) redactReviewPatch(ctx context.Context, runID, stageID uuid.UUID, patch string) string {
	if patch == "" {
		return patch
	}
	out, hits := redaction.RedactDefault([]byte(patch))
	if len(hits) == 0 {
		return patch
	}
	counts := make([]string, 0, len(hits))
	for _, h := range hits {
		counts = append(counts, h.Pattern+"="+strconv.Itoa(h.Count))
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "implement review: redacted credential-shaped strings from the review diff",
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.String("hits", strings.Join(counts, ",")),
	)
	return string(out)
}
