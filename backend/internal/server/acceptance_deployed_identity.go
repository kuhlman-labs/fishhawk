package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// resolvePostDeployExpectedSHA is the RELEASE ARM of acceptance head
// resolution (E35.2 / #1599, ADR-053): a post-deploy acceptance stage — one
// whose spec stage declares a `deployment` input — validates the DEPLOYED
// build, so its expected head is the deployed commit SHA recorded on the
// consumed deploy stage's deployment artifact, never the reported-head ledger
// (a release run has none, and a reported head names a PR candidate, not what
// is serving at the deployed host).
//
// applies=false means the stage is POSITIVELY KNOWN not to consume a
// deployment, and ONLY that: the caller then keeps the reported-head path
// (the feature_change arm), byte-identical to before #1599. It is returned on
// exactly five legs, each a FACT that no deployment input is declared, never
// an absence of knowledge (the fact-versus-outage split resolveRunWorkflowDef
// draws):
//
//   - RunRepo is nil (unwired test servers only; every production server
//     wires it, and there is no run row to read a declaration from);
//   - GetRun answers run.ErrNotFound: a run with no row declares nothing. It
//     is unreachable for a live acceptance stage (stage rows reference their
//     run, and runs are never deleted);
//   - the run carries no workflow-spec snapshot: there is no stored
//     declaration, and every production run-creation path stores one, so a
//     deployment-consuming stage (a workflow-v2 declaration) always has one;
//   - the run's workflow declares no stage with a deployment input at all
//     (decided from the spec alone, so a stage-read blip cannot move a
//     feature_change run off its reported head);
//   - the stage maps to a spec stage that declares no deployment input.
//
// applies=true is returned on EVERY other leg, and with sha=="" on every leg
// that cannot produce a deployed SHA. That includes the legs where it cannot
// be DETERMINED whether the stage consumes a deployment — any other GetRun
// error, a ListStagesForRun error, an unparseable spec, a workflow absent from
// it, a stage absent from the run's rows, a stage with no spec counterpart at
// its type ordinal (operator approval condition 2: an undeterminable identity
// must not fall back to the reported head). The
// caller returns "" verbatim, and an empty expectation is refused pre-spawn by
// both the dispatch verb and the runner (acceptance_expected_head_unresolved),
// and clamps a shipped pass to undecidable(head_unresolved).
//
// Once the stage is known to consume a deployment, the expectation is derived
// from the consumed deploy stage's deployment artifacts (deployedSHAFromArtifacts):
// the NEWEST must be a decodable `succeeded` record, and every succeeded
// record's deployed SHA (`sha`, else a full-hex `ref`) must agree on exactly
// one value. Anything else — no deploy row, an artifact read error, no
// deployment artifact, a newest rolled_back/failed/partial record, a symbolic
// ref only, an abbreviated SHA, two distinct SHAs — resolves "".
func (s *Server) resolvePostDeployExpectedSHA(ctx context.Context, runID, stageID uuid.UUID) (string, bool) {
	if s.cfg.RunRepo == nil {
		return "", false
	}
	unresolved := func(reason string, attrs ...slog.Attr) (string, bool) {
		attrs = append([]slog.Attr{
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("reason", reason),
		}, attrs...)
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance: post-deploy expected head unresolved; omitting it (never falling back to a reported head)", attrs...)
		return "", true
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if errors.Is(err, run.ErrNotFound) {
		return "", false
	}
	if err != nil {
		return unresolved("get_run_failed", slog.String("error", err.Error()))
	}
	if runRow.WorkflowSpec == nil {
		return "", false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return unresolved("workflow_spec_unparseable", slog.String("error", err.Error()))
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return unresolved("workflow_absent_from_spec", slog.String("workflow_id", runRow.WorkflowID))
	}
	if !workflowConsumesDeployment(wf) {
		return "", false
	}
	rows, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return unresolved("list_stages_failed", slog.String("error", err.Error()))
	}
	var stage *run.Stage
	for _, st := range rows {
		if st.ID == stageID {
			stage = st
			break
		}
	}
	if stage == nil {
		return unresolved("stage_absent_from_run")
	}
	specStage, mapped := specStageForRunStage(wf, rows, stage)
	if !mapped {
		return unresolved("stage_unmappable_to_spec")
	}
	var inputs []spec.Input
	for _, in := range specStage.Inputs {
		if in.Artifact == string(spec.ArtifactDeployment) {
			inputs = append(inputs, in)
		}
	}
	if len(inputs) == 0 {
		return "", false
	}
	if len(inputs) > 1 {
		return unresolved("ambiguous_deployment_inputs", slog.Int("inputs", len(inputs)))
	}
	deployRow, ok := runStageForSpecStage(wf, rows, inputs[0].FromStage)
	if !ok {
		return unresolved("deploy_stage_row_absent", slog.String("from_stage", inputs[0].FromStage))
	}
	if s.cfg.ArtifactRepo == nil {
		return unresolved("artifact_repo_unconfigured")
	}
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, deployRow.ID)
	if err != nil {
		return unresolved("list_deploy_artifacts_failed", slog.String("error", err.Error()))
	}
	sha, reason := deployedSHAFromArtifacts(arts)
	if reason != "" {
		return unresolved(reason, slog.String("deploy_stage_id", deployRow.ID.String()))
	}
	return sha, true
}

// workflowConsumesDeployment reports whether any stage of wf declares a
// `deployment` input — the spec-only half of resolvePostDeployExpectedSHA's
// applicability decision.
func workflowConsumesDeployment(wf spec.Workflow) bool {
	for _, st := range wf.Stages {
		for _, in := range st.Inputs {
			if in.Artifact == string(spec.ArtifactDeployment) {
				return true
			}
		}
	}
	return false
}

// deployedSHAFromArtifacts derives the deployed commit SHA from one deploy
// stage's artifacts (created_at-ascending, ArtifactRepo.ListForStage order).
// It returns the lowercased SHA and "" on success, or "" and a stable reason
// on every fail-closed leg:
//
//   - no_deployment_artifact: the stage carries no deployment record;
//   - newest_deployment_not_succeeded: the NEWEST record is rolled_back,
//     failed, partial, or undecodable — the environment no longer serves (or
//     never served) the build an older succeeded record names;
//   - deployed_sha_symbolic_only: no succeeded record carries a full commit
//     SHA (`sha`, or a full-hex `ref`) — a symbolic ref like `main` or an
//     abbreviated SHA cannot be prefix-matched by the runner's identity probe;
//   - deployed_sha_disagreement: succeeded records name more than one distinct
//     SHA (e.g. a different succeeded record posted after dispatch) — the
//     deployed build is ambiguous, and the rule is what stops a later record
//     re-binding a verdict without any timestamp comparison (#3048).
func deployedSHAFromArtifacts(arts []*artifact.Artifact) (string, string) {
	var deployments []*artifact.Artifact
	for _, a := range arts {
		if a.Kind == artifact.KindDeployment {
			deployments = append(deployments, a)
		}
	}
	if len(deployments) == 0 {
		return "", "no_deployment_artifact"
	}
	var newest deploymentBody
	if err := json.Unmarshal(deployments[len(deployments)-1].Content, &newest); err != nil ||
		newest.Outcome != string(run.DeployOutcomeSucceeded) {
		return "", "newest_deployment_not_succeeded"
	}
	distinct := map[string]struct{}{}
	for _, a := range deployments {
		var body deploymentBody
		if err := json.Unmarshal(a.Content, &body); err != nil || body.Outcome != string(run.DeployOutcomeSucceeded) {
			continue
		}
		switch {
		case isDeployCommitSHA(body.SHA):
			distinct[strings.ToLower(body.SHA)] = struct{}{}
		case isDeployCommitSHA(body.Ref):
			distinct[strings.ToLower(body.Ref)] = struct{}{}
		}
	}
	switch len(distinct) {
	case 0:
		return "", "deployed_sha_symbolic_only"
	case 1:
		for sha := range distinct {
			return sha, ""
		}
	}
	return "", "deployed_sha_disagreement"
}

// runStageForSpecStage is the INVERSE of specStageForRunStage: it resolves the
// spec stage with id specID to its runtime row by TYPE ORDINAL — the k-th spec
// stage of type T maps to the k-th row of type T, rows sorted by Sequence then
// stage ID. The ordinal (not Sequence-as-spec-index) is what survives a
// plan-filtered retry/recovery child's dense renumbering; see
// specStageForRunStage for the full rationale.
//
// Returns ok=false when specID names no spec stage, or the run has fewer than
// k+1 rows of that type (a row/spec disagreement the caller fails closed on).
func runStageForSpecStage(wf spec.Workflow, rows []*run.Stage, specID string) (*run.Stage, bool) {
	k := -1
	var typ spec.StageType
	seen := 0
	for _, sp := range wf.Stages {
		if sp.ID == specID {
			typ = sp.Type
			k = 0
			for _, prior := range wf.Stages {
				if prior.ID == specID {
					break
				}
				if prior.Type == sp.Type {
					k++
				}
			}
			break
		}
	}
	if k < 0 {
		return nil, false
	}
	sorted := make([]*run.Stage, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Sequence != sorted[j].Sequence {
			return sorted[i].Sequence < sorted[j].Sequence
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	for _, st := range sorted {
		if string(st.Type) != string(typ) {
			continue
		}
		if seen == k {
			return st, true
		}
		seen++
	}
	return nil, false
}
