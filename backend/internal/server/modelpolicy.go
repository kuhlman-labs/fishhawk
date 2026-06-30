package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ModelSource names which rung of the implement-model resolution ladder
// supplied the resolved model. It is carried alongside the resolved value
// (ResolvedModel) so calibration history can attribute a runtime observation
// to the rung that chose the model — operator overrides vs planner
// recommendations vs the deployment default.
type ModelSource string

// The implement-model ladder rungs, lowest precedence to highest. ModelSourceNone
// is the sentinel for an all-empty ladder: no rung supplied a model, so the
// runner spawns the implement agent on the adapter's built-in default exactly as
// today (no --model argument).
const (
	ModelSourceNone     ModelSource = ""
	ModelSourceDefault  ModelSource = "default"
	ModelSourceSpec     ModelSource = "spec"
	ModelSourcePlan     ModelSource = "plan"
	ModelSourceOperator ModelSource = "operator"
)

// ResolvedModel is the source-tagged outcome of the implement-model ladder.
// Value is the resolved model id (empty == "use the adapter default spawn",
// today's behavior); Source names the winning rung.
type ResolvedModel struct {
	Value  string      `json:"model"`
	Source ModelSource `json:"model_source"`
}

// CategoryModelResolved is the audit-kind the approval gate emits once, on a
// valid plan-stage approve, recording the source-tagged implement model the
// gate resolved (the {model, model_source} payload — ResolvedModel's json
// tags). It is INTRODUCED by the operator-gate slice (#1013): the gate is the
// sole writer, and gateResolvedModel is the sole reader (the read-only bridge
// that routes the resolution to the runner spawn). The trace/calibration path
// deliberately never emits it (trace_test.go's surface-sweep guard), so the
// gate's emission is the single source of the run's authoritative resolution.
const CategoryModelResolved = "model_resolved"

// resolveImplementModel applies the 4-rung implement-model ladder, lowest to
// highest precedence:
//
//	deployment default  <  spec.executor.model  <  plan.model_recommendation.implement_model  <  operator gate decision
//
// The highest non-empty rung wins and its name is returned as the source. An
// all-empty ladder returns {Value: "", Source: ModelSourceNone}: the caller
// then carries no model, and the runner spawns the implement agent identically
// to today (byte-for-byte, no --model argument). The function is pure — it does
// no IO and is the single chokepoint both the prompt/calibration paths (this
// slice) and the approval gate (the operator-gate slice) resolve through, so a
// child run resolves its sub_plan recommendation the same way a top-level run
// resolves its plan recommendation.
func resolveImplementModel(deflt, spec, plan, operator string) ResolvedModel {
	switch {
	case operator != "":
		return ResolvedModel{Value: operator, Source: ModelSourceOperator}
	case plan != "":
		return ResolvedModel{Value: plan, Source: ModelSourcePlan}
	case spec != "":
		return ResolvedModel{Value: spec, Source: ModelSourceSpec}
	case deflt != "":
		return ResolvedModel{Value: deflt, Source: ModelSourceDefault}
	default:
		return ResolvedModel{Value: "", Source: ModelSourceNone}
	}
}

// resolvePlanModel applies the plan-model ladder, lowest to highest precedence:
//
//	deployment default  <  spec.executor.model (plan stage)  <  operator gate decision
//
// It mirrors resolveImplementModel but omits the implement ladder's plan
// model_recommendation rung: the plan agent is spawned BEFORE any plan artifact
// exists, so there is no plan recommendation to read for the plan stage's own
// model (the model_recommendation a plan emits is for the IMPLEMENT stage). The
// highest non-empty rung wins and its name is the source; an all-empty ladder
// returns {Value: "", Source: ModelSourceNone}, so the caller carries no model
// and the runner spawns the plan agent byte-for-byte as today (no --model
// argument). Pure — no IO — and the single chokepoint both the prompt path
// (this slice) and the approval gate (the operator-override slice) resolve
// through, so a re-dispatched plan resolves its operator override the same way.
func resolvePlanModel(deflt, spec, operator string) ResolvedModel {
	switch {
	case operator != "":
		return ResolvedModel{Value: operator, Source: ModelSourceOperator}
	case spec != "":
		return ResolvedModel{Value: spec, Source: ModelSourceSpec}
	case deflt != "":
		return ResolvedModel{Value: deflt, Source: ModelSourceDefault}
	default:
		return ResolvedModel{Value: "", Source: ModelSourceNone}
	}
}

// resolveReviewModel applies the review-model ladder, lowest to highest
// precedence:
//
//	deployment default  <  spec.executor.model (review stage)  <  operator gate decision
//
// It mirrors resolvePlanModel exactly: the review agent — like the plan agent —
// carries no plan model_recommendation rung (model_recommendation feeds the
// implement stage), so the ladder is the same 3-rung shape. The operator rung is
// the review_model the operator supplies at the plan-approval gate (#1416); it
// governs the post-plan-gate implement review (and any post-gate re-review), NOT
// the already-completed plan review. The highest non-empty rung wins; an
// all-empty ladder returns {Value: "", Source: ModelSourceNone}, so the reviewer
// invocation falls back to the spec model byte-for-byte as today. Pure — no IO.
func resolveReviewModel(deflt, spec, operator string) ResolvedModel {
	switch {
	case operator != "":
		return ResolvedModel{Value: operator, Source: ModelSourceOperator}
	case spec != "":
		return ResolvedModel{Value: spec, Source: ModelSourceSpec}
	case deflt != "":
		return ResolvedModel{Value: deflt, Source: ModelSourceDefault}
	default:
		return ResolvedModel{Value: "", Source: ModelSourceNone}
	}
}

// ResolvedEffort is the source-tagged outcome of the per-reviewer
// reasoning-effort ladder (#1493). Value is the resolved reasoning-effort
// string (empty == "carry no effort override", today's behavior where the
// codex adapter inherits the host ~/.codex config); Source names the winning
// rung, reusing the ModelSource enum (ModelSourceDefault / ModelSourceSpec /
// ModelSourceNone) so the resolver mirrors resolveReviewModel exactly.
type ResolvedEffort struct {
	Value  string      `json:"reasoning_effort"`
	Source ModelSource `json:"reasoning_effort_source"`
}

// ResolveReviewerReasoningEffort applies the per-reviewer reasoning-effort
// ladder, lowest to highest precedence:
//
//	deployment default (FISHHAWKD_CODEX_REASONING_EFFORT)  <  spec reviewers.agents[i].reasoning_effort
//
// It mirrors resolveReviewModel's source-tagged shape but carries only the two
// rungs the reasoning-effort knob has: the codex deployment default and the
// per-reviewer spec value (there is no operator gate override for effort). The
// highest non-empty rung wins and its name is the source; an all-empty ladder
// returns {Value: "", Source: ModelSourceNone}, so the reviewer carries no
// effort override and the codex adapter inherits its host config byte-for-byte
// as today. Pure — no IO. Codex-only at the seam: the anthropic/claudecode
// adapters ignore the resolved value. Exported so the deployment's codex
// reviewer construction (serve.go) resolves the env-default rung through the
// same chokepoint.
func ResolveReviewerReasoningEffort(deflt, spec string) ResolvedEffort {
	switch {
	case spec != "":
		return ResolvedEffort{Value: spec, Source: ModelSourceSpec}
	case deflt != "":
		return ResolvedEffort{Value: deflt, Source: ModelSourceDefault}
	default:
		return ResolvedEffort{Value: "", Source: ModelSourceNone}
	}
}

// modelResolvedPayload is the model_resolved audit payload (#1416): the
// source-tagged ResolvedModel plus a StageType discriminator. Once the plan
// gate stamps a model_resolved entry for MORE THAN ONE stage (implement, plan,
// review), the per-stage readers need to tell the entries apart. Each entry is
// keyed by its TARGET stage's StageID (so the observability slice reads a
// stage's model by StageID), and the StageType is carried in the payload so the
// runner-spawn reader — which holds only the run id, not a stage id — can filter
// the run's entries to the stage it routes WITHOUT an extra stage lookup.
//
// StageType is ADDITIVE to the {model, model_source} wire contract the slice-1
// reader relies on (#1013): a legacy implement-only entry written before this
// slice decodes to StageType=="" and is treated as the implement resolution (the
// only stage that stamped the category before #1416). gateResolvedModelForStage
// owns that compatibility via modelResolvedStageMatches.
type modelResolvedPayload struct {
	ResolvedModel
	StageType string `json:"stage_type,omitempty"`
}

// AllowedModels is the per-adapter allowed-model policy sourced from deployment
// config (ParseAllowedModels). It maps an adapter name (claudecode | codex |
// anthropic) to the set of model ids the operator permits for that adapter.
//
// An adapter with no configured set — or an empty whole policy — FAILS OPEN:
// IsAllowed returns true for any model, byte-identical to today's behavior
// where no allow-list exists. The allow-list only ever tightens, never widens,
// once configured.
type AllowedModels map[string]map[string]bool

// IsAllowed reports whether model is permitted for adapter under this policy.
// It fails OPEN — returns true — when:
//   - model is empty (the resolved value is empty == today's default spawn),
//   - the adapter has no configured allow-set, or
//   - the adapter's allow-set is empty.
//
// Only a non-empty allow-set that omits the model rejects it. This is the
// gate-time validation primitive the approval slice calls against the RESOLVED
// model regardless of which rung supplied it.
func (a AllowedModels) IsAllowed(adapter, model string) bool {
	if model == "" {
		return true
	}
	set, ok := a[adapter]
	if !ok || len(set) == 0 {
		return true
	}
	return set[model]
}

// ParseAllowedModels parses the per-adapter allowed-model deployment config.
// The format is a semicolon-separated list of `adapter=model1,model2` groups:
//
//	claudecode=claude-opus-4-8,claude-sonnet-4-6;codex=gpt-5.5
//
// Whitespace around adapters and models is trimmed; empty groups and empty
// model entries are skipped. An empty/blank input yields an empty policy, which
// fails open for every adapter (today's behavior). Parsing never errors —
// malformed groups (no '=' or an empty adapter) are skipped so a typo degrades
// to fail-open rather than failing the boot.
func ParseAllowedModels(raw string) AllowedModels {
	out := AllowedModels{}
	for _, group := range strings.Split(raw, ";") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		eq := strings.IndexByte(group, '=')
		if eq <= 0 {
			continue
		}
		adapter := strings.TrimSpace(group[:eq])
		if adapter == "" {
			continue
		}
		set := out[adapter]
		if set == nil {
			set = map[string]bool{}
		}
		for _, m := range strings.Split(group[eq+1:], ",") {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			set[m] = true
		}
		if len(set) > 0 {
			out[adapter] = set
		}
	}
	if len(out) == 0 {
		return AllowedModels{}
	}
	return out
}

// resolveImplementModelForRun resolves the implement-model ladder for a run
// through the pure resolveImplementModel chokepoint, reading each rung without
// a static dependency on the schema/struct slice that owns the plan
// model_recommendation and spec executor.model fields (those live in a sibling
// decomposition slice). Rungs are read defensively from the data the run
// already carries:
//
//   - default:  the deployment-configured default implement model (cfg).
//   - spec:     executor.model on the run's workflow spec implement stage,
//     read from the raw spec bytes (specImplementExecutorModel).
//   - plan:     model_recommendation.implement_model on the run's approved
//     standard_v1 plan artifact, read from the raw artifact bytes
//     (planImplementModelRecommendation).
//   - operator: the approval gate's persisted resolution. When the gate has
//     recorded a model_resolved audit entry (the operator-gate slice), its
//     source-tagged value is authoritative and returned verbatim — the gate
//     already evaluated the full ladder including any operator override.
//
// When no gate resolution exists yet (pre-approval, or a deployment without the
// gate writer), the function falls back to resolving {default, spec, plan} with
// an empty operator rung. An empty result (ModelSourceNone) means today's spawn.
func (s *Server) resolveImplementModelForRun(ctx context.Context, runRow *run.Run) ResolvedModel {
	if rm, ok := s.gateResolvedModel(ctx, runRow.ID); ok {
		return rm
	}
	deflt := s.cfg.ImplementModelDefault
	specModel := specImplementExecutorModel(runRow.WorkflowSpec, runRow.WorkflowID)
	planModel := s.planImplementModelRecommendation(ctx, runRow.ID)
	return resolveImplementModel(deflt, specModel, planModel, "")
}

// resolvePlanModelForRun resolves the plan-model ladder for a run through the
// pure resolvePlanModel chokepoint. It reads executor.model on the run's
// workflow plan stage (specPlanExecutorModel) from the raw spec bytes, keeping
// this slice free of a static dependency on the spec.Executor.Model field. The
// deployment-default rung is left empty here: a PlanModelDefault config field is
// owned by the allow-list/config slice, not this one, so the plan default is ""
// and the ladder is effectively spec-only until that slice folds in a default
// and the operator-override slice folds in the gate rung. The operator rung is
// empty on this path — a plan prompt fetch precedes the plan-approval gate, so
// it carries only the spec pin. An all-empty ladder (ModelSourceNone) leaves
// PlanModel empty so the runner spawns the plan agent identically to today.
//
// The operator-override slice (#1416) fills in the reserved ctx parameter: it
// reads the plan gate's per-stage model_resolved resolution FIRST, so a
// re-dispatched plan stage spawns under the operator's plan_model override when
// one was recorded at the plan-approval gate. When no gate plan resolution
// exists (pre-approval, or a deployment without the gate writer) the function
// falls back to the spec-only ladder, leaving an empty/spec-only resolution
// byte-identical to today.
func (s *Server) resolvePlanModelForRun(ctx context.Context, runRow *run.Run) ResolvedModel {
	if rm, ok := s.gateResolvedModelForStage(ctx, runRow.ID, string(run.StageTypePlan)); ok {
		return rm
	}
	specModel := specPlanExecutorModel(runRow.WorkflowSpec, runRow.WorkflowID)
	return resolvePlanModel("", specModel, "")
}

// gateResolvePlanModel resolves the plan-model ladder AT THE APPROVAL GATE, with
// the operator's plan_model as the highest rung (#1416). Like
// gateResolveImplementModel it does NOT consult a prior gate resolution — the
// gate is the WRITER, so re-reading would let a prior approve shadow a
// re-approval's fresh override. It reads the spec rung (specPlanExecutorModel)
// and folds in the operator string; the deployment-default rung is empty (a
// PlanModelDefault config field is the allow-list slice's concern). An all-empty
// ladder yields ModelSourceNone (today's empty spawn). Pure resolution.
func (*Server) gateResolvePlanModel(runRow *run.Run, operator string) ResolvedModel {
	specModel := specPlanExecutorModel(runRow.WorkflowSpec, runRow.WorkflowID)
	return resolvePlanModel("", specModel, strings.TrimSpace(operator))
}

// gateResolveReviewModel resolves the review-model ladder AT THE APPROVAL GATE,
// with the operator's review_model as the highest rung (#1416). It mirrors
// gateResolvePlanModel: spec rung (specReviewExecutorModel) plus the operator
// override, no deployment default, no re-read of a prior gate resolution. The
// resolved value is recorded as the review stage's model_resolved audit and read
// back at implement-review time (gateResolvedReviewModel) to override the
// reviewer invocation's model. An all-empty ladder yields ModelSourceNone, so
// the reviewer falls back to its spec model byte-for-byte as today.
func (*Server) gateResolveReviewModel(runRow *run.Run, operator string) ResolvedModel {
	specModel := specReviewExecutorModel(runRow.WorkflowSpec, runRow.WorkflowID)
	return resolveReviewModel("", specModel, strings.TrimSpace(operator))
}

// gateResolvedReviewModel returns the review model the plan gate resolved for
// the run (#1416), or "" when no review model_resolved entry exists (or it
// recorded an empty resolution). It is the read-only bridge the implement-review
// invocation path (resolveReviewerInvocations) consults to override each
// reviewer's model. Returning "" — the fail-open default — leaves the reviewer
// on its spec model byte-for-byte as today.
func (s *Server) gateResolvedReviewModel(ctx context.Context, runID uuid.UUID) string {
	rm, ok := s.gateResolvedModelForStage(ctx, runID, string(run.StageTypeReview))
	if !ok {
		return ""
	}
	return rm.Value
}

// resolveFixupImplementModel resolves the model a fix-up pass will run under
// (#1164). When the operator supplied a (trimmed-non-empty) override on the
// fixup dispatch it wins as the operator rung — {override, ModelSourceOperator}.
// Otherwise the fix-up inherits the run's already-resolved implement model
// (resolveImplementModelForRun, which itself reads the gate's authoritative
// model_resolved entry when present), preserving the BYTE-IDENTICAL default:
// a fix-up with no override spawns under exactly the model the original
// implement pass did. The returned ResolvedModel is pinned on the
// stage_fixup_triggered audit entry at trigger time so the prompt-fetch
// read-back (fixupResolvedModelFromAudit) is deterministic regardless of any
// later config change.
func (s *Server) resolveFixupImplementModel(ctx context.Context, runRow *run.Run, operatorOverride string) ResolvedModel {
	if ov := strings.TrimSpace(operatorOverride); ov != "" {
		return ResolvedModel{Value: ov, Source: ModelSourceOperator}
	}
	return s.resolveImplementModelForRun(ctx, runRow)
}

// checkFixupModelAllowed is the fix-up model gate (#1164). It validates an
// ALREADY-RESOLVED fix-up model (resolveFixupImplementModel's output) against
// the run adapter's allow-list, mirroring approvals.go:checkPlanModelAllowed —
// one model policy across the plan gate and the fix-up. Returns true to proceed.
// Returns false after writing a 422 fixup_invalid_model (naming the resolved
// SOURCE, mirroring plan_invalid_model) when the resolved value is non-empty and
// the adapter's configured allow-set omits it.
//
// Fail-OPEN, matching the plan gate: an empty resolved model (ModelSourceNone,
// today's default spawn) skips the check; an empty/unconfigured allow-list — or
// an adapter with no set — accepts any model via IsAllowed (byte-identical to
// today).
func (s *Server) checkFixupModelAllowed(w http.ResponseWriter, r *http.Request, stage *run.Stage, runRow *run.Run, resolved ResolvedModel) bool {
	if resolved.Value == "" {
		// Empty resolution: today's default spawn. Nothing to validate.
		return true
	}
	adapter := adapterForImplementAgent(specImplementExecutorAgent(runRow.WorkflowSpec, runRow.WorkflowID))
	if s.cfg.ImplementAllowedModels.IsAllowed(adapter, resolved.Value) {
		return true
	}
	s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_invalid_model",
		fmt.Sprintf("resolved fix-up implement model %q (source %s) is not in the configured allow-list for adapter %q; choose an allowed model via the implement_model fix-up override, or widen the deployment allow-list",
			resolved.Value, resolved.Source, adapter),
		map[string]any{
			"stage_id":     stage.ID.String(),
			"model":        resolved.Value,
			"model_source": string(resolved.Source),
			"adapter":      adapter,
		})
	return false
}

// fixupResolvedModelFromAudit reads the model a fix-up pass was PINNED to at
// trigger time (#1164) from the newest stage_fixup_triggered audit entry for the
// stage. The trigger handler always writes the fixup_model / fixup_model_source
// keys (resolveFixupImplementModel's resolution), so the prompt-fetch path reads
// the pin back deterministically — the model survives any later config change.
//
// It distinguishes "pinned" (ok=true) from "no pin" (ok=false) by KEY PRESENCE,
// not non-emptiness: a present-but-empty fixup_model (the empty-ladder default
// spawn the trigger deliberately pinned) returns {Value:"", ...}, true so the
// caller honors the empty pin rather than re-deriving a non-empty rung. ok=false
// — fall through to live resolution — only when the AuditRepo is unconfigured,
// the lookup fails, no triggered entry exists for the stage, the payload is
// undecodable, or the entry predates #1164 (no fixup_model key written).
func (s *Server) fixupResolvedModelFromAudit(ctx context.Context, runID, stageID uuid.UUID) (ResolvedModel, bool) {
	if s.cfg.AuditRepo == nil {
		return ResolvedModel{}, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		return ResolvedModel{}, false
	}
	// Newest wins: ListForRunByCategory returns append (sequence-ascending)
	// order, so the LAST entry for the stage is the most-recent fix-up pass —
	// same selection as maybeRecoverFixupFailure's reader of this category.
	var newest []byte
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			newest = e.Payload
		}
	}
	if newest == nil {
		return ResolvedModel{}, false
	}
	var probe struct {
		// Pointer so a PRESENT-but-empty pin (ok=true, Value="") is
		// distinguishable from an ABSENT key (pre-#1164 entry → ok=false).
		FixupModel       *string `json:"fixup_model"`
		FixupModelSource string  `json:"fixup_model_source"`
	}
	if err := json.Unmarshal(newest, &probe); err != nil {
		return ResolvedModel{}, false
	}
	if probe.FixupModel == nil {
		// Pre-#1164 fix-up entry carried no pin — fall through to live
		// resolution so old runs stay byte-identical.
		return ResolvedModel{}, false
	}
	return ResolvedModel{Value: *probe.FixupModel, Source: ModelSource(probe.FixupModelSource)}, true
}

// gateResolveImplementModel resolves the implement-model ladder AT THE
// APPROVAL GATE, with the operator override as the highest rung. Unlike
// resolveImplementModelForRun, it does NOT consult gateResolvedModel: the gate
// is the WRITER of the model_resolved audit, not a re-reader of its own past
// resolution, so re-reading it would let a prior approve's resolution shadow a
// re-approval's fresh operator override. It reads the same visible rungs
// (deployment default, spec executor.model, plan model_recommendation) and
// folds in the operator string supplied at approve time. Pure resolution
// through resolveImplementModel; an all-empty ladder yields ModelSourceNone
// (today's empty spawn). The caller validates the RESOLVED value against the
// allow-list and emits the model_resolved audit from the returned value.
func (s *Server) gateResolveImplementModel(ctx context.Context, runRow *run.Run, operator string) ResolvedModel {
	deflt := s.cfg.ImplementModelDefault
	specModel := specImplementExecutorModel(runRow.WorkflowSpec, runRow.WorkflowID)
	planModel := s.planImplementModelRecommendation(ctx, runRow.ID)
	return resolveImplementModel(deflt, specModel, planModel, strings.TrimSpace(operator))
}

// adapterForImplementAgent maps an implement stage's executor.agent id to the
// AllowedModels adapter key the gate validates against. The workflow spec's
// implement executor names the agent id ("claude-code" | "codex"; runner
// --agent default is "claude-code"), while the allow-list — and the reviewer
// `provider` vocabulary — key on the adapter name ("claudecode" | "codex" |
// "anthropic"). An empty/absent agent therefore maps to "claudecode" (the
// default spawn's adapter); an unrecognized id passes through verbatim so a
// future agent keys its own configured set rather than silently failing open
// under a mismatched key. This is the deterministic bridge that keeps the gate
// validating against the operator's configured set for the run's real adapter.
func adapterForImplementAgent(agent string) string {
	switch strings.TrimSpace(agent) {
	case "", "claude-code", "claudecode":
		return "claudecode"
	case "codex":
		return "codex"
	default:
		return strings.TrimSpace(agent)
	}
}

// specImplementExecutorAgent reads executor.agent on the implement stage of the
// given workflow from raw workflow-spec bytes via a local YAML probe, returning
// "" when the spec is empty, malformed, or declares no executor.agent. Mirrors
// specImplementExecutorModel's stage-lookup (prefer id=="implement", else the
// first stage whose type=="implement") and stays free of a static dependency on
// the spec.Executor.Agent field. The gate maps the returned id to the
// allow-list adapter key via adapterForImplementAgent.
func specImplementExecutorAgent(specBytes []byte, workflowID string) string {
	if len(specBytes) == 0 {
		return ""
	}
	var probe struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Type     string `yaml:"type"`
				Executor struct {
					Agent string `yaml:"agent"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(specBytes, &probe); err != nil {
		return ""
	}
	wf, ok := probe.Workflows[workflowID]
	if !ok {
		return ""
	}
	for _, st := range wf.Stages {
		if st.ID == "implement" {
			return strings.TrimSpace(st.Executor.Agent)
		}
	}
	for _, st := range wf.Stages {
		if st.Type == "implement" {
			return strings.TrimSpace(st.Executor.Agent)
		}
	}
	return ""
}

// resolvedImplementModelForRunID is the by-id convenience over
// resolveImplementModelForRun used by the calibration-stamp path (trace.go),
// which holds a run id rather than the loaded run. Loads the run and resolves;
// returns the empty ResolvedModel (ModelSourceNone) when the RunRepo is
// unconfigured or the load fails, so a stamp degrades to "default spawn" rather
// than unwinding the best-effort trace handler.
func (s *Server) resolvedImplementModelForRunID(ctx context.Context, runID uuid.UUID) ResolvedModel {
	if s.cfg.RunRepo == nil {
		return ResolvedModel{Source: ModelSourceNone}
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return ResolvedModel{Source: ModelSourceNone}
	}
	return s.resolveImplementModelForRun(ctx, runRow)
}

// gateResolvedModel returns the most-recent model_resolved audit entry's
// source-tagged resolution for the run, if any. The model_resolved kind is
// emitted ONLY by the approval gate (the operator-gate slice) — this slice
// never emits it, keeping its surface sweep clean — so reading it here is the
// read-only bridge that carries the gate's authoritative resolution (including
// an operator override) into the prompt and calibration paths. Returns
// ok=false when the AuditRepo is unconfigured, the lookup fails, or no
// model_resolved entry exists for the run, so the caller falls back to the
// visible-rung computation.
//
// WIRE CONTRACT: the model_resolved payload MUST carry the {model, model_source}
// keys (ResolvedModel's json tags). The gate writer (sibling slice) emits that
// shape; a drift degrades this read to ok=false (fall back), never a panic.
//
// It resolves the IMPLEMENT stage's entry specifically (#1416): the plan gate now
// stamps model_resolved for the plan and review stages too, so the bare
// "most-recent for the run" read would return whichever stage's entry happened
// to be written last. gateResolvedModelForStage filters by the payload's
// StageType discriminator (treating a legacy entry with no StageType as the
// implement resolution) so the runner-spawn route is unaffected by the sibling
// entries.
func (s *Server) gateResolvedModel(ctx context.Context, runID uuid.UUID) (ResolvedModel, bool) {
	return s.gateResolvedModelForStage(ctx, runID, string(run.StageTypeImplement))
}

// gateResolvedModelForStage returns the most-recent model_resolved entry whose
// payload StageType identifies the given stage (#1416), or ok=false when the
// AuditRepo is unconfigured, the lookup fails, or no entry matches the stage. It
// is the per-stage generalization of gateResolvedModel: the run can now carry one
// model_resolved entry per stamped stage (implement, plan, review), and each
// reader filters to the stage it routes. A recorded empty resolution
// (ModelSourceNone) for a matching stage is surfaced as ok=true so the caller
// honors the gate's deliberate "use the default spawn" decision rather than
// re-deriving a lower rung.
func (s *Server) gateResolvedModelForStage(ctx context.Context, runID uuid.UUID, stageType string) (ResolvedModel, bool) {
	if s.cfg.AuditRepo == nil {
		return ResolvedModel{}, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryModelResolved)
	if err != nil || len(entries) == 0 {
		return ResolvedModel{}, false
	}
	// Newest wins: ListForRunByCategory returns ascending by sequence, so sort
	// by the monotonic Sequence descending defensively rather than assume an
	// order.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Sequence > entries[j].Sequence
	})
	for _, e := range entries {
		var p modelResolvedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if !modelResolvedStageMatches(p.StageType, stageType) {
			continue
		}
		if p.Value == "" {
			// A recorded empty resolution is a valid "use the default spawn"
			// decision; surface it so the caller does not re-derive a non-empty
			// rung the gate deliberately left empty.
			return ResolvedModel{Value: "", Source: ModelSourceNone}, true
		}
		return p.ResolvedModel, true
	}
	return ResolvedModel{}, false
}

// modelResolvedStageMatches reports whether a model_resolved entry's payload
// StageType (have) identifies the stage a reader wants (want). A legacy
// implement-only entry written before #1416 carries no StageType (have==""); it
// is treated as the implement resolution so the runner-spawn route stays
// byte-identical for runs approved before this slice.
func modelResolvedStageMatches(have, want string) bool {
	if have == want {
		return true
	}
	return want == string(run.StageTypeImplement) && have == ""
}

// planImplementModelRecommendation reads model_recommendation.implement_model
// from the run's most-recent approved standard_v1 plan artifact. It decodes the
// raw artifact bytes into a local probe struct so this slice carries no static
// dependency on the plan struct field (owned by a sibling schema slice). Returns
// "" when the AuditRepo/ArtifactRepo are unconfigured, no plan artifact exists,
// the decode fails, or the field is absent — every path degrades to "no plan
// recommendation".
func (s *Server) planImplementModelRecommendation(ctx context.Context, runID uuid.UUID) string {
	if s.cfg.ArtifactRepo == nil || s.cfg.RunRepo == nil {
		return ""
	}
	content := s.latestPlanArtifactContent(ctx, runID)
	if len(content) == 0 {
		return ""
	}
	return planModelRecommendationFromBytes(content)
}

// latestPlanArtifactContent returns the raw bytes of the run's most-recent
// kind=plan, schema_version=standard_v1 artifact, or nil. It mirrors
// tryLoadPlanForRun's selection but returns bytes (not a decoded *plan.Plan) so
// the model_recommendation can be read without the sibling-owned struct field.
func (s *Server) latestPlanArtifactContent(ctx context.Context, runID uuid.UUID) []byte {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil
	}
	var planStageID uuid.UUID
	for _, st := range stages {
		if st.Type == run.StageTypePlan {
			planStageID = st.ID
			break
		}
	}
	if planStageID == uuid.Nil {
		return nil
	}
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, planStageID)
	if err != nil {
		return nil
	}
	var picked *artifact.Artifact
	for _, a := range arts {
		if a.Kind != artifact.KindPlan {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || a.CreatedAt.After(picked.CreatedAt) {
			picked = a
		}
	}
	if picked == nil {
		return nil
	}
	return picked.Content
}

// planModelRecommendationFromBytes decodes model_recommendation.implement_model
// from standard_v1 plan artifact JSON via a local probe, returning "" when the
// JSON is malformed or the field is absent. Kept pure (bytes in, string out) so
// it is unit-testable and free of any sibling-slice struct dependency.
func planModelRecommendationFromBytes(content []byte) string {
	var probe struct {
		ModelRecommendation *struct {
			ImplementModel string `json:"implement_model"`
		} `json:"model_recommendation"`
	}
	if err := json.Unmarshal(content, &probe); err != nil {
		return ""
	}
	if probe.ModelRecommendation == nil {
		return ""
	}
	return strings.TrimSpace(probe.ModelRecommendation.ImplementModel)
}

// specImplementExecutorModel reads executor.model on the implement stage of the
// given workflow from raw workflow-spec bytes via a local YAML probe, returning
// "" when the spec is empty, malformed, or declares no executor.model. Decoding
// from bytes (rather than the parsed spec.Stage) keeps this slice free of a
// static dependency on the spec.Executor.Model field (owned by a sibling schema
// slice). The probe mirrors resolveVerifyConfig's stage lookup: prefer a stage
// whose id == "implement", else the first stage whose type == "implement".
func specImplementExecutorModel(specBytes []byte, workflowID string) string {
	if len(specBytes) == 0 {
		return ""
	}
	var probe struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Type     string `yaml:"type"`
				Executor struct {
					Model string `yaml:"model"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(specBytes, &probe); err != nil {
		return ""
	}
	wf, ok := probe.Workflows[workflowID]
	if !ok {
		return ""
	}
	for _, st := range wf.Stages {
		if st.ID == "implement" {
			return strings.TrimSpace(st.Executor.Model)
		}
	}
	for _, st := range wf.Stages {
		if st.Type == "implement" {
			return strings.TrimSpace(st.Executor.Model)
		}
	}
	return ""
}

// specPlanExecutorModel reads executor.model on the plan stage of the given
// workflow from raw workflow-spec bytes via a local YAML probe, returning ""
// when the spec is empty, malformed, or declares no executor.model. It mirrors
// specImplementExecutorModel exactly but targets the PLAN stage (prefer a stage
// whose id == "plan", else the first stage whose type == "plan"), staying free
// of a static dependency on the spec.Executor.Model field (owned by a sibling
// schema slice). This is the spec rung of the plan-model ladder — the pinned
// plan executor.model Scenario B honors.
func specPlanExecutorModel(specBytes []byte, workflowID string) string {
	if len(specBytes) == 0 {
		return ""
	}
	var probe struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Type     string `yaml:"type"`
				Executor struct {
					Model string `yaml:"model"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(specBytes, &probe); err != nil {
		return ""
	}
	wf, ok := probe.Workflows[workflowID]
	if !ok {
		return ""
	}
	for _, st := range wf.Stages {
		if st.ID == "plan" {
			return strings.TrimSpace(st.Executor.Model)
		}
	}
	for _, st := range wf.Stages {
		if st.Type == "plan" {
			return strings.TrimSpace(st.Executor.Model)
		}
	}
	return ""
}

// specReviewExecutorModel reads executor.model on the review stage of the given
// workflow from raw workflow-spec bytes via a local YAML probe, returning ""
// when the spec is empty, malformed, or declares no executor.model. It mirrors
// specPlanExecutorModel exactly but targets the REVIEW stage (prefer a stage
// whose id == "review", else the first stage whose type == "review"), staying
// free of a static dependency on the spec.Executor.Model field. This is the spec
// rung of the review-model ladder (#1416).
func specReviewExecutorModel(specBytes []byte, workflowID string) string {
	if len(specBytes) == 0 {
		return ""
	}
	var probe struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Type     string `yaml:"type"`
				Executor struct {
					Model string `yaml:"model"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(specBytes, &probe); err != nil {
		return ""
	}
	wf, ok := probe.Workflows[workflowID]
	if !ok {
		return ""
	}
	for _, st := range wf.Stages {
		if st.ID == "review" {
			return strings.TrimSpace(st.Executor.Model)
		}
	}
	for _, st := range wf.Stages {
		if st.Type == "review" {
			return strings.TrimSpace(st.Executor.Model)
		}
	}
	return ""
}

// logModelResolution emits a debug line when a non-default model is resolved,
// aiding operator visibility without a new audit surface. Best-effort, no-op on
// a nil logger.
func (s *Server) logModelResolution(ctx context.Context, runID uuid.UUID, rm ResolvedModel) {
	if s.cfg.Logger == nil || rm.Value == "" {
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "server: resolved implement model",
		slog.String("run_id", runID.String()),
		slog.String("model", rm.Value),
		slog.String("model_source", string(rm.Source)),
	)
}
