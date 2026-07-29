package server

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- Layer A: legacy duplicate-kind deploy-constraint characterization ------
//
// CHARACTERIZATION tests for #2218 (E52.6). They were written and run GREEN
// against unmodified HEAD BEFORE the workflow-v2 reshape touched any
// production file; every asserted value is a GOLDEN VALUE read off the code as
// it stood, not a value predicted from the new design.
//
// #2218 reshapes v2's `constraints` from a LIST into an OBJECT. An earlier
// design collapsed the v0/v1 LIST into one canonical object with a merge rule
// (slice kinds concatenating, scalars min/max-winning). That rule would have
// SILENTLY LOOSENED a deploy environment allow-list, because the two deploy
// consumers fold a duplicate-kind document DIFFERENTLY — and, on the very same
// document, disagree with EACH OTHER today:
//
//   - approvals.go preflightDeploy assigns `allowedEnvs = c.AllowedEnvironments`
//     inside the loop, so the LAST entry wins → only `prod` is permitted.
//   - trace.go deployEnvironmentForRun returns the FIRST non-empty entry's
//     first element → `staging`.
//
// Three mutually inconsistent folds cannot be preserved under one canonical
// resolution, which is exactly why the shipped mechanism is NOT a canonical
// resolution: the v0/v1 path keeps its list representation, a v2 object
// normalizes to a ONE-ELEMENT slice, and no consumer is edited. These tests
// lock that existing mutual inconsistency as BEHAVIOUR rather than resolving
// it, and are the preservation proof — the v2-equivalence tests below are
// deliberately NOT evidence for this claim, because both of their sides pass
// through the new normalization.

// deploySpecLegacyDuplicateEnvs is the operator's exact example: a v1 deploy
// stage declaring allowed_environments TWICE, staging first and prod last.
const deploySpecLegacyDuplicateEnvs = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - allowed_environments: [staging]
          - allowed_environments: [prod]
        produces:
          - artifact: deployment
`

// TestDeployGate_LegacyDuplicateAllowedEnvironments_LastWinsUnchanged drives
// the operator's exact example end to end through the REAL approval handler —
// spec parse → constraint fold → approval handler → audit, a path no per-layer
// unit test crosses. LAST-wins means `staging` is REFUSED and `prod` is
// permitted; a merge rule that concatenated the two entries would have
// silently permitted `staging` too, loosening a governance gate.
func TestDeployGate_LegacyDuplicateAllowedEnvironments_LastWinsUnchanged(t *testing.T) {
	t.Run("staging_refused", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		stage, _ := seedDeployRun(rr, "release", deploySpecLegacyDuplicateEnvs)
		w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		assertDeployRefused(t, w, rr, au, stage, "deploy_environment_not_allowed")
	})

	t.Run("prod_permitted", func(t *testing.T) {
		s, _, rr, _ := newApprovalServer(t)
		stage, _ := seedDeployRun(rr, "release", deploySpecLegacyDuplicateEnvs)
		w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=prod"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (prod is the LAST-wins allow-list):\n%s", w.Code, w.Body.String())
		}
		cur, _ := rr.GetStage(context.Background(), stage.ID)
		if cur.State == run.StageStateAwaitingDeployApproval {
			t.Errorf("deploy stage still parked at %q; the approval should have advanced it", cur.State)
		}
	})
}

// TestDeployEnvironmentForRun_LegacyDuplicate_FirstWinsUnchanged pins the
// FIFTH constraint consumer on the SAME document. It deliberately asserts the
// OPPOSITE answer from the gate above: deployEnvironmentForRun is first-wins,
// so it reports `staging` on a document whose gate permits only `prod`. That
// disagreement is the existing behaviour, and locking it is the point.
func TestDeployEnvironmentForRun_LegacyDuplicate_FirstWinsUnchanged(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	_, runRow := seedDeployRun(rr, "release", deploySpecLegacyDuplicateEnvs)
	if got := s.deployEnvironmentForRun(context.Background(), runRow.ID); got != "staging" {
		t.Errorf("deployEnvironmentForRun = %q, want %q (FIRST-wins, deliberately the opposite of the gate's last-wins %q)",
			got, "staging", "prod")
	}
}

// --- Layer B: v2 multi-kind ENFORCEMENT equivalence (binding condition 1b) --
//
// Kept strictly separate from Layer A and NEVER cited as evidence for it: both
// sides of every assertion below pass through the new v2 normalization, so
// they can only prove v2 ≡ v1, never that v0/v1 was preserved.
//
// PARSED-REPRESENTATION EQUALITY IS DELIBERATELY NOT ASSERTED HERE, and the
// difference is expected rather than a defect. A v1 document expressing SEVERAL
// constraint kinds must write them as a LIST of single-key maps (the v0/v1
// `constraint` $def is maxProperties:1), which parses to an N-ELEMENT slice.
// The v2 OBJECT denotes exactly one Constraint carrying all those kinds, so it
// normalizes to a ONE-ELEMENT slice. The two parsed *Spec values therefore
// differ by construction for any multi-kind fixture, and narrowing the fixture
// to a single kind to recover deep equality would not prove the acceptance
// criterion, which is explicitly about an object carrying max_files_changed
// AND forbidden_paths together. So equivalence is asserted at the ENFORCEMENT
// level: both parsed specs are driven through the actual consumer folds they
// reach and the RESOLVED OUTCOMES must be identical. (The single-kind
// parsed-Spec deep-equality assertion is valid and is kept — it lives in
// backend/internal/spec's TestParseV2_SingleKindEquivalentToV1.)

// multiKindV1ImplementSpec / multiKindV2ImplementSpec are a matched pair: the
// same three post-hoc constraint kinds, spelled as a v1 list of single-key
// maps and as a v2 object.
const multiKindV1ImplementSpec = `version: "1.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 45
          - forbidden_paths: ["infra/**", ".github/workflows/**"]
          - allowed_paths: ["backend/**"]
          - required_outcomes: [tests_added_or_updated]
          - diff_coverage:
              command: "make cov"
              report_path: "cov.info"
              min_new_line_coverage: 85
        produces:
          - artifact: pull_request
`

const multiKindV2ImplementSpec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          max_files_changed: 45
          forbidden_paths: ["infra/**", ".github/workflows/**"]
          allowed_paths: ["backend/**"]
          required_outcomes: [tests_added_or_updated]
          diff_coverage:
            command: "make cov"
            report_path: "cov.info"
            min_new_line_coverage: 85
        produces:
          - artifact: pull_request
`

// multiKindV1DeploySpec / multiKindV2DeploySpec are the deploy-side matched
// pair, exercising the two pre-flight consumers.
const multiKindV1DeploySpec = `version: "1.6"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - allowed_environments: [staging, prod]
          - change_freeze: false
          - required_upstream: [ci_green]
        produces:
          - artifact: deployment
`

const multiKindV2DeploySpec = `version: "2"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          allowed_environments: [staging, prod]
          change_freeze: false
          required_upstream: [ci_green]
        produces:
          - artifact: deployment
`

func implementConstraints(t *testing.T, specYAML string) []spec.Constraint {
	t.Helper()
	s, err := spec.ParseBytes([]byte(specYAML))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return s.Workflows["feature_change"].Stages[0].Constraints
}

// TestV2MultiKindConstraints_EnforcementEquivalentToV1 is binding condition
// 1(b): representations legitimately differ, so the assertion is on the
// RESOLVED enforcement outcome of every fold a constraint reaches.
func TestV2MultiKindConstraints_EnforcementEquivalentToV1(t *testing.T) {
	v1Implement := implementConstraints(t, multiKindV1ImplementSpec)
	v2Implement := implementConstraints(t, multiKindV2ImplementSpec)

	// The representations DO differ — that is the expected shape, and
	// asserting it here documents why deep equality is not the assertion.
	if len(v1Implement) == len(v2Implement) {
		t.Fatalf("fixture is not multi-kind: v1 has %d entries and v2 has %d; the point of this test is that a multi-kind v2 object normalizes to ONE element while v1 spells it as N",
			len(v1Implement), len(v2Implement))
	}
	if len(v2Implement) != 1 {
		t.Fatalf("v2 constraints = %d entries, want exactly 1 (an object denotes exactly one Constraint)", len(v2Implement))
	}

	// (1) mergeConstraints — the post-implement policy gate's fold.
	if got, want := mergeConstraints(v2Implement), mergeConstraints(v1Implement); !reflect.DeepEqual(got, want) {
		t.Errorf("mergeConstraints(v2) =\n%#v\nmergeConstraints(v1) =\n%#v", got, want)
	}
	// (2) flattenPathConstraints — the plan-time scope pre-check's fold.
	if got, want := flattenPathConstraints(v2Implement), flattenPathConstraints(v1Implement); !reflect.DeepEqual(got, want) {
		t.Errorf("flattenPathConstraints(v2) =\n%#v\nflattenPathConstraints(v1) =\n%#v", got, want)
	}
	// (3) resolveDiffCoverageConfig — the prompt handler's max-wins read,
	// driven through the real *Server method against a seeded run.
	s, _, rr, _ := newApprovalServer(t)
	_, v1Run := seedDeployRun(rr, "feature_change", multiKindV1ImplementSpec)
	_, v2Run := seedDeployRun(rr, "feature_change", multiKindV2ImplementSpec)
	ctx := context.Background()
	gotDC := s.resolveDiffCoverageConfig(ctx, mustGetRun(t, rr, v2Run), run.StageTypeImplement)
	wantDC := s.resolveDiffCoverageConfig(ctx, mustGetRun(t, rr, v1Run), run.StageTypeImplement)
	if wantDC == nil {
		t.Fatalf("resolveDiffCoverageConfig(v1) = nil; the fixture declares diff_coverage, so the fold is not being exercised")
	}
	if !reflect.DeepEqual(gotDC, wantDC) {
		t.Errorf("resolveDiffCoverageConfig(v2) = %#v, want %#v", gotDC, wantDC)
	}

	// (4) the deploy pre-flight read, driven end to end through the REAL
	// approval handler on the matched deploy pair: the same requested
	// environment must resolve to the same verdict on both spellings.
	for _, env := range []string{"prod", "qa"} {
		v1Code := deployApprovalStatus(t, multiKindV1DeploySpec, env)
		v2Code := deployApprovalStatus(t, multiKindV2DeploySpec, env)
		if v1Code != v2Code {
			t.Errorf("deploy pre-flight for --environment=%s: v1 status %d, v2 status %d", env, v1Code, v2Code)
		}
	}
	// (5) deployEnvironmentForRun — the fifth consumer, on the same pair.
	sd, _, rrd, _ := newApprovalServer(t)
	_, v1Deploy := seedDeployRun(rrd, "release", multiKindV1DeploySpec)
	_, v2Deploy := seedDeployRun(rrd, "release", multiKindV2DeploySpec)
	if got, want := sd.deployEnvironmentForRun(ctx, v2Deploy.ID), sd.deployEnvironmentForRun(ctx, v1Deploy.ID); got != want {
		t.Errorf("deployEnvironmentForRun(v2) = %q, want %q", got, want)
	}
}

// deployApprovalStatus submits a deploy approval for the given environment
// against a freshly seeded run carrying specYAML, returning the HTTP status.
func deployApprovalStatus(t *testing.T, specYAML, env string) int {
	t.Helper()
	s, _, rr, _ := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", specYAML)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=`+env+`"}`)
	return w.Code
}

func mustGetRun(t *testing.T, rr *approvalRunRepo, seeded *run.Run) *run.Run {
	t.Helper()
	got, err := rr.GetRun(context.Background(), seeded.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return got
}
