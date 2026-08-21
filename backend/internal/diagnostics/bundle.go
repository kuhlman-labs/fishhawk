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
	// WedgeContext names WHY the run is stuck, when it is stuck in a
	// shape the backend can describe structurally (#1737). Absent
	// (omitted from the JSON entirely) on a run with no wedge shape and
	// on every bundle produced by the no-wedge Collect wrapper.
	WedgeContext *WedgeContext `json:"wedge_context,omitempty"`
}

// WedgeFacts is the caller-INJECTED wedge input, the same idiom as
// VersionFacts: the diagnostics package is pure, so the facts that need
// a repository read (required-check states, campaign item linkage) are
// assembled by the caller and handed in. The collector still owns the
// redaction contract — every injected field is either a closed-enum
// normalization (CampaignItemState) or a structured identifier set
// (BlockingChecks), never free text.
type WedgeFacts struct {
	// BlockingChecks are the run's required-check CONTEXT NAMES whose
	// latest recorded state is red. Context names come from the run's
	// branch-protection snapshot — configuration identifiers, not
	// output. Empty for every run with no resolved snapshot (#2497),
	// so the block degrades rather than fabricating names.
	BlockingChecks []string
	// CampaignItemState is the campaign item state for the item this run
	// executes, if any. Normalized through the closed WEDGE-INDICATING
	// item-state set on the way into WedgeContext — a healthy state
	// (running/succeeded/pending) and any unrecognized value are both
	// DROPPED, never echoed. The caller may inject the item's real state
	// unconditionally; the filtering is the collector's job.
	CampaignItemState string
	// BlockedDependents is how many sibling campaign items are blocked
	// waiting on this run's item. A count, never a name list. Carried
	// into WedgeContext only alongside a wedge-indicating
	// CampaignItemState — see buildWedgeContext.
	BlockedDependents int
}

// WedgeContext is the wire block: structured wedge facts only. Like the
// rest of the bundle it is redaction-safe BY CONSTRUCTION — every field
// is a count, a configuration identifier, or a table-owned enum literal.
// The drive Advance.Event string and a stage's free-text FailureReason
// are never copied in.
type WedgeContext struct {
	// BlockingChecks are red required-check context names. Omitted when
	// none are red, or when the run has no resolved checks snapshot.
	BlockingChecks []string `json:"blocking_checks,omitempty"`
	// CampaignItemState is a WEDGE-INDICATING campaign item-state enum
	// literal ("blocked" | "paused" | "failed"), or empty. A healthy
	// item state (running/succeeded/pending) is dropped, not echoed.
	CampaignItemState string `json:"campaign_item_state,omitempty"`
	// BlockedDependents counts sibling items waiting on this one. Present
	// only alongside a wedge-indicating CampaignItemState.
	BlockedDependents int `json:"blocked_dependents,omitempty"`
	// IntegrateWaveError is a closed marker for a fan-in failure,
	// derived from the run's audit CATEGORY set (never a payload body).
	// Currently the single literal "slice_integration_conflict", and
	// only while the conflict is the run's CURRENT fan-in state — a
	// later children_settled clears it.
	IntegrateWaveError string `json:"integrate_wave_error,omitempty"`
}

// sliceIntegrationConflictMarker is the audit category the fan-in
// conflict path emits AND the enum literal the wedge block carries. The
// two are deliberately the same string: the marker is the category
// itself, so nothing is derived from an audit payload.
const sliceIntegrationConflictMarker = "slice_integration_conflict"

// childrenSettledCategory is the audit category the fan-in path emits
// when integration SUCCEEDS (both the consolidate handler and the
// childcompletion sweeper write it). It is read only to decide whether
// a conflict earlier in the chain is still the run's CURRENT fan-in
// state — see integrateWaveError.
const childrenSettledCategory = "children_settled"

// wedgingCampaignItemStates is the closed set of campaign item-state
// literals the wedge block may carry. It is deliberately a STRICT SUBSET
// of the campaign item-state enum: only states that EXPLAIN a stuck run
// belong here.
//
// `running`, `succeeded`, `pending` and `cancelled` are excluded because
// they describe a run that is progressing, done, or deliberately stopped
// — emitting them would make the block fire on every campaign-linked
// run, turning "why is this stuck?" into "this run has a campaign
// item", which is historical association rather than a wedge (#1737
// implement review). The healthy-run omission is the whole anti-noise
// contract of this feature, so a healthy campaign item contributes
// nothing.
//
// Kept as a package-local table (rather than importing
// internal/campaign) so this package stays dependency-light and the
// emitted value is TABLE-OWNED: normalizeCampaignItemState returns the
// literal from this map, never any part of its input.
var wedgingCampaignItemStates = map[string]string{
	"blocked": "blocked",
	"paused":  "paused",
	"failed":  "failed",
}

// normalizeCampaignItemState maps an injected item state onto the closed
// table above. A state that is not wedge-indicating — and any
// unrecognized value — yields "": the wedge block drops it rather than
// describing a healthy item or echoing an unvetted string across the
// redaction boundary.
func normalizeCampaignItemState(state string) string {
	return wedgingCampaignItemStates[state]
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
//
// Collect is the no-wedge wrapper over CollectWithWedge: it passes a nil
// WedgeFacts, and a nil WedgeFacts suppresses the wedge block entirely,
// so every pre-#1737 caller keeps producing the bundle it produced
// before — no new key appears, not even on a run whose audit chain
// carries a fan-in conflict.
func Collect(r *run.Run, stages []*run.Stage, auditEntries []*audit.Entry, versions VersionFacts) DiagnosticBundle {
	return CollectWithWedge(r, stages, auditEntries, versions, nil)
}

// CollectWithWedge is Collect plus the wedge block (#1737). Pass a
// non-nil (possibly zero-valued) wedge to opt the bundle in: the block
// is then assembled from the injected facts AND the structured fan-in
// signal read off the audit CATEGORIES, and omitted when that assembly
// finds nothing — a healthy run opted in still carries no
// wedge_context. Pass nil to get Collect's exact pre-wedge output.
func CollectWithWedge(r *run.Run, stages []*run.Stage, auditEntries []*audit.Entry, versions VersionFacts, wedge *WedgeFacts) DiagnosticBundle {
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

	// The nil gate is what keeps Collect byte-identical to its
	// pre-wedge self for every un-migrated caller.
	if wedge != nil {
		b.WedgeContext = buildWedgeContext(wedge, auditEntries)
	}

	return b
}

// buildWedgeContext assembles the wire block from the injected facts
// plus the structured fan-in signal. Returns nil when nothing was
// found, so the bundle omits the key rather than carrying an empty
// object. Every value is copied through a closed table, a count, or a
// configuration identifier — see the WedgeContext doc.
func buildWedgeContext(wedge *WedgeFacts, entries []*audit.Entry) *WedgeContext {
	wc := WedgeContext{
		CampaignItemState:  normalizeCampaignItemState(wedge.CampaignItemState),
		IntegrateWaveError: integrateWaveError(entries),
	}
	// The dependent count is the blast radius OF A STUCK ITEM, so it
	// rides with a wedge-indicating item state and is dropped without
	// one. Siblings queued behind a healthily-`running` item are ordinary
	// campaign sequencing, not a wedge — reporting the count there would
	// re-open the healthy-run noise this filter closes.
	if wc.CampaignItemState != "" {
		wc.BlockedDependents = wedge.BlockedDependents
	}
	for _, name := range wedge.BlockingChecks {
		if name == "" {
			continue
		}
		wc.BlockingChecks = append(wc.BlockingChecks, name)
	}
	if len(wc.BlockingChecks) == 0 && wc.CampaignItemState == "" &&
		wc.BlockedDependents == 0 && wc.IntegrateWaveError == "" {
		return nil
	}
	return &wc
}

// integrateWaveError returns the fan-in conflict marker when the run is
// CURRENTLY stuck on a fan-in conflict. Only the entry CATEGORY is read
// — the payload (which names branches and carries conflict detail) is
// never touched — and the returned value is the package-owned literal,
// not the entry's own string.
//
// "Currently" is load-bearing (#1737 implement review): the audit chain
// is append-only history, so a run that hit a conflict, had it resolved,
// and integrated cleanly still carries the conflict entry forever. A
// scan that reported ANY conflict in the chain would describe that run
// as wedged for the rest of its life. So the marker is reported only
// when the conflict is the LATEST fan-in lifecycle signal: a later
// children_settled entry (integration succeeded) supersedes it. The
// decision is made on the highest SEQUENCE rather than slice position,
// so it does not depend on the caller handing entries in chain order.
func integrateWaveError(entries []*audit.Entry) string {
	var (
		latest    int64
		conflict  bool
		anySignal bool
	)
	for _, e := range entries {
		if e == nil {
			continue
		}
		isConflict := e.Category == sliceIntegrationConflictMarker
		if !isConflict && e.Category != childrenSettledCategory {
			continue
		}
		if anySignal && e.Sequence <= latest {
			continue
		}
		anySignal, latest, conflict = true, e.Sequence, isConflict
	}
	if conflict {
		return sliceIntegrationConflictMarker
	}
	return ""
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
