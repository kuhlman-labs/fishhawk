// Package diagnostics builds a product-facts-only diagnostic bundle for a
// run: the minimal, redaction-safe summary the operator can attach to an
// upstream Fishhawk product report (#1006). The bundle carries STRUCTURED
// product facts only — run id, stage states, the failing stage's category
// and surface, the audit sequence range, build versions + git SHAs, the
// workflow spec hash, and the runner kind. It deliberately carries NO
// diffs, paths, prompts, free text, or audit payload bodies; the failing
// stage's FailureReason (free text) is excluded by construction. The
// failing stage DOES carry a FailureDetailClass — a closed enum DERIVED
// from that free text by ClassifyFailureDetail — but the reason text
// itself never crosses the boundary: only the table-owned enum literal
// does.
//
// This package is the read foundation of the product-feedback feature
// (slice 1). The deduped egress path (fingerprint, FeedbackProvider) and
// the operator surfaces (MCP tool, CLI report verb) ride on top of it in
// later slices and are intentionally not in this package.
package diagnostics

import (
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// DiagnosticBundle is the wire shape returned by
// GET /v0/runs/{run_id}/diagnostics. Every field is a product fact safe
// to leave the operator's boundary without redaction. Free text never
// appears here — see the package doc.
type DiagnosticBundle struct {
	RunID              string         `json:"run_id"`
	WorkflowID         string         `json:"workflow_id"`
	WorkflowSpecHash   string         `json:"workflow_spec_hash"`
	RunnerKind         string         `json:"runner_kind"`
	RunState           string         `json:"run_state"`
	Stages             []StageFact    `json:"stages"`
	FailingStage       *FailingStage  `json:"failing_stage,omitempty"`
	AuditSequenceRange *SequenceRange `json:"audit_sequence_range,omitempty"`
	Versions           VersionFacts   `json:"versions"`
}

// StageFact is one stage's position and state — no timing detail, no
// failure reason. The ordered slice mirrors the run's stage sequence.
type StageFact struct {
	Sequence int    `json:"sequence"`
	Type     string `json:"type"`
	State    string `json:"state"`
}

// FailingStage names which stage failed and how, as product facts.
// FailureCategory is the single-letter MVP_SPEC §6 class (A/B/C/D).
// FailureSurface is the audit category of the most-recent audit entry
// scoped to the failing stage (e.g. "policy_evaluated") — a structured
// enum value, never a payload body or the free-text FailureReason. It
// is the "error code / failing surface" the downstream fingerprint
// (slice 2) keys on.
// FailureDetailClass is a closed-enum normalization of the stage's
// free-text FailureReason ("auth-401" | "bad-object-ref" |
// "target-unreachable" | "" when unclassified), produced by
// ClassifyFailureDetail. It lets the fingerprint distinguish distinct
// root causes that share a surface (#1962). The raw reason text is
// NEVER copied in — only the table-owned enum literal is, so this field
// is a redaction-safe product fact by construction.
type FailingStage struct {
	Sequence           int    `json:"sequence"`
	Type               string `json:"type"`
	FailureCategory    string `json:"failure_category"`
	FailureSurface     string `json:"failure_surface,omitempty"`
	FailureDetailClass string `json:"failure_detail_class,omitempty"`
}

// SequenceRange is the [min,max] of the run's audit-entry sequence
// numbers — enough to anchor a chain export without carrying any
// entry payloads.
type SequenceRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// VersionFacts carries the build identity the backend authoritatively
// knows. Fishhawkd is this binary's version + git SHA (stamped by
// scripts/dev / release ldflags; "dev"/"unknown" when unstamped).
// MinRunnerVersion is the minimum runner the backend requires — the
// runner's own reported version is not persisted on the run row in v0,
// so it is not synthesized here.
type VersionFacts struct {
	Fishhawkd        Component `json:"fishhawkd"`
	MinRunnerVersion string    `json:"min_runner_version"`
}

// Component is a single build's version + git SHA.
type Component struct {
	Version string `json:"version"`
	GitSHA  string `json:"git_sha"`
}

// Collect assembles the product-facts bundle from a loaded run, its
// stages, and its audit entries. It is a pure function: no I/O, no
// package-global reads — the caller injects build versions so the
// result is deterministic and testable. auditEntries are assumed
// sequence-ascending (the repo's ListForRun contract); Collect does
// not rely on it for correctness beyond picking the failing stage's
// most-recent surface.
//
// By construction the returned bundle contains only structured facts.
// The failing stage's free-text FailureReason is never copied in.
func Collect(r *run.Run, stages []*run.Stage, auditEntries []*audit.Entry, versions VersionFacts) DiagnosticBundle {
	b := DiagnosticBundle{
		Versions: versions,
		Stages:   []StageFact{},
	}
	if r != nil {
		b.RunID = r.ID.String()
		b.WorkflowID = r.WorkflowID
		b.WorkflowSpecHash = r.WorkflowSHA
		b.RunnerKind = r.RunnerKind
		b.RunState = string(r.State)
	}

	// Order defensively by sequence so the bundle is deterministic
	// regardless of the repo's return order.
	ordered := make([]*run.Stage, len(stages))
	copy(ordered, stages)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Sequence < ordered[j].Sequence
	})

	var failing *run.Stage
	for _, st := range ordered {
		if st == nil {
			continue
		}
		b.Stages = append(b.Stages, StageFact{
			Sequence: st.Sequence,
			Type:     string(st.Type),
			State:    string(st.State),
		})
		// The failing stage is the last one (highest sequence) in a
		// failed terminal state with a recorded category.
		if st.State == run.StageStateFailed && st.FailureCategory != nil {
			failing = st
		}
	}

	if failing != nil {
		fs := &FailingStage{
			Sequence:        failing.Sequence,
			Type:            string(failing.Type),
			FailureCategory: string(*failing.FailureCategory),
			FailureSurface:  failingSurface(failing.ID, auditEntries),
		}
		// Derive the closed-enum detail class FROM the free-text reason;
		// the reason text itself still never enters the bundle.
		if failing.FailureReason != nil {
			fs.FailureDetailClass = ClassifyFailureDetail(*failing.FailureReason)
		}
		b.FailingStage = fs
	}

	if rng := sequenceRange(auditEntries); rng != nil {
		b.AuditSequenceRange = rng
	}

	return b
}

// failingSurface returns the audit category of the most-recent entry
// scoped to the given stage. The category is a structured enum string
// (the cause-specific audit kind the failure call site emitted, e.g.
// "policy_evaluated"); the entry payload is never read. Empty when no
// entry is tagged to the stage.
func failingSurface(stageID uuid.UUID, entries []*audit.Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e == nil || e.StageID == nil {
			continue
		}
		if *e.StageID == stageID {
			return e.Category
		}
	}
	return ""
}

// sequenceRange computes the [min,max] of the entries' sequence
// numbers. Returns nil when there are no entries.
func sequenceRange(entries []*audit.Entry) *SequenceRange {
	var (
		seen     bool
		min, max int64
	)
	for _, e := range entries {
		if e == nil {
			continue
		}
		if !seen {
			min, max = e.Sequence, e.Sequence
			seen = true
			continue
		}
		if e.Sequence < min {
			min = e.Sequence
		}
		if e.Sequence > max {
			max = e.Sequence
		}
	}
	if !seen {
		return nil
	}
	return &SequenceRange{Min: min, Max: max}
}
