package server

import (
	"context"
	"encoding/json"
	"log/slog"
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
func (s *Server) gateResolvedModel(ctx context.Context, runID uuid.UUID) (ResolvedModel, bool) {
	if s.cfg.AuditRepo == nil {
		return ResolvedModel{}, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "model_resolved")
	if err != nil || len(entries) == 0 {
		return ResolvedModel{}, false
	}
	// Newest wins: ListForRunByCategory returns ascending by sequence, so sort
	// by the monotonic Sequence descending defensively rather than assume an
	// order.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Sequence > entries[j].Sequence
	})
	var rm ResolvedModel
	if err := json.Unmarshal(entries[0].Payload, &rm); err != nil {
		return ResolvedModel{}, false
	}
	if rm.Value == "" {
		// A recorded empty resolution is a valid "use the default spawn"
		// decision; surface it so the caller does not re-derive a non-empty
		// rung the gate deliberately left empty.
		return ResolvedModel{Value: "", Source: ModelSourceNone}, true
	}
	return rm, true
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
