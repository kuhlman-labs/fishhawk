package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Deployed-target identity (E35.2 / #1599) — the release arm of acceptance head
// resolution. Every fixture here seeds a DECOY reported head on the run's chain
// by construction, so an arm leg that wrongly falls through to the reported-head
// ledger answers the decoy instead of "" (counterfactuals C2 / condition 2).

const (
	deployedSHAX     = "1111111111111111111111111111111111111111"
	deployedSHAY     = "2222222222222222222222222222222222222222"
	deployedSHA64    = "3333333333333333333333333333333333333333333333333333333333333333"
	decoyReportedSHA = "dddddddddddddddddddddddddddddddddddddddd"
)

// releaseExampleSpec reads the committed release-acceptance example — the same
// bytes release_acceptance_integration_test.go drives — so a drifted example
// reddens these tests too.
func releaseExampleSpec(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "spec", "examples", "workflow-v2-release-acceptance.yaml")) //nolint:gosec // fixed in-repo test fixture path
	if err != nil {
		t.Fatalf("read committed release-acceptance example: %v", err)
	}
	return b
}

// deployStanza is a valid delegating, staging-only deploy stage producing a
// deployment artifact (the committed example's shape).
func deployStanza(id string) string {
	return `      - id: ` + id + `
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        constraints:
          allowed_environments: [staging]
        produces:
          - artifact: deployment
`
}

const acceptanceStanzaHead = `      - id: verify
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts: [staging.example.com]
`

// featureChangeAcceptanceSpec declares an acceptance stage with NO deployment
// input — the feature_change shape the reported-head arm owns.
const featureChangeAcceptanceSpec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor: {agent: claude-code}
      - id: acceptance
        type: acceptance
        executor: {agent: claude-code}
`

// twoDeploySpec declares two deploy stages; the acceptance stage consumes
// `inputs` (rendered verbatim), so a fixture can bind it to the SECOND deploy
// (the inverse type-ordinal mapping) or to both (ambiguous).
func twoDeploySpec(inputs string) string {
	return `version: "2"
workflows:
  release:
    stages:
` + deployStanza("deploy_a") + deployStanza("deploy_b") + acceptanceStanzaHead + `        inputs:
` + inputs
}

// deployedIdentityFixture is a planless release run on the package fakes: a
// deploy row (Sequence 0), an acceptance row (Sequence 1), the committed
// example as its spec, and a decoy reported head on the run's chain.
type deployedIdentityFixture struct {
	s          *Server
	rr         *promptRunRepo
	ar         *fakeArtifactRepo
	au         *auditFake
	runRow     *run.Run
	deploy     *run.Stage
	acceptance *run.Stage
	created    time.Time
}

func newDeployedIdentityFixture(t *testing.T) *deployedIdentityFixture {
	t.Helper()
	runID := uuid.New()
	rr := newPromptRunRepo()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	runRow := &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "release", WorkflowSpec: releaseExampleSpec(t)}
	deploy := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeDeploy, Sequence: 0, State: run.StageStateSucceeded}
	acceptance := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeAcceptance, Sequence: 1, State: run.StageStateRunning}
	rr.getRuns[runID] = runRow
	rr.getStages[deploy.ID] = deploy
	rr.getStages[acceptance.ID] = acceptance
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {acceptance, deploy}}
	au.seeded = append(au.seeded, makeReportedHeadEntry(runID, acceptance.ID, "pull_request_opened", decoyReportedSHA, time.Now().Add(-time.Hour)))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})
	return &deployedIdentityFixture{s: s, rr: rr, ar: ar, au: au, runRow: runRow, deploy: deploy, acceptance: acceptance, created: time.Now().UTC()}
}

// addDeployment appends a deployment artifact to stageID with a strictly
// increasing created_at, mirroring ListForStage's created_at-ascending order.
func (f *deployedIdentityFixture) addDeployment(t *testing.T, stageID uuid.UUID, body map[string]any) {
	t.Helper()
	content, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	f.addRaw(stageID, artifact.KindDeployment, content)
}

func (f *deployedIdentityFixture) addRaw(stageID uuid.UUID, kind artifact.Kind, content []byte) {
	f.created = f.created.Add(time.Second)
	f.ar.all = append(f.ar.all, &artifact.Artifact{ID: uuid.New(), StageID: stageID, Kind: kind, Content: content, CreatedAt: f.created})
}

func deployment(outcome, ref, sha string) map[string]any {
	m := map[string]any{
		"environment":      "staging",
		"ref":              ref,
		"external_run_url": "https://github.com/kuhlman-labs/example/actions/runs/1",
		"outcome":          outcome,
	}
	if sha != "" {
		m["sha"] = sha
	}
	return m
}

// TestResolvePostDeployExpectedSHA_ArmOutcomes is the per-failure-mode table
// for the release arm: one row per applicability leg and per derivation leg.
func TestResolvePostDeployExpectedSHA_ArmOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, f *deployedIdentityFixture)
		wantSHA     string
		wantApplies bool
	}{
		{
			name: "feature_change spec (no deployment input) -> not applicable",
			setup: func(_ *testing.T, f *deployedIdentityFixture) {
				f.runRow.WorkflowID = "feature_change"
				f.runRow.WorkflowSpec = []byte(featureChangeAcceptanceSpec)
			},
		},
		{
			name:  "run not found -> not applicable (no row declares nothing)",
			setup: func(_ *testing.T, f *deployedIdentityFixture) { delete(f.rr.getRuns, f.runRow.ID) },
		},
		{
			name:  "no workflow-spec snapshot -> not applicable",
			setup: func(_ *testing.T, f *deployedIdentityFixture) { f.runRow.WorkflowSpec = nil },
		},
		{
			name:        "GetRun error -> undeterminable, fail closed",
			setup:       func(_ *testing.T, f *deployedIdentityFixture) { f.rr.runErr = errors.New("db down") },
			wantApplies: true,
		},
		{
			name:        "unparseable spec -> undeterminable, fail closed",
			setup:       func(_ *testing.T, f *deployedIdentityFixture) { f.runRow.WorkflowSpec = []byte("version: [not yaml") },
			wantApplies: true,
		},
		{
			name:        "workflow absent from spec -> undeterminable, fail closed",
			setup:       func(_ *testing.T, f *deployedIdentityFixture) { f.runRow.WorkflowID = "nope" },
			wantApplies: true,
		},
		{
			name:        "ListStagesForRun error -> undeterminable, fail closed",
			setup:       func(_ *testing.T, f *deployedIdentityFixture) { f.rr.stagesByRunID = nil },
			wantApplies: true,
		},
		{
			name: "stage absent from the run's rows -> undeterminable, fail closed",
			setup: func(_ *testing.T, f *deployedIdentityFixture) {
				f.rr.stagesByRunID[f.runRow.ID] = []*run.Stage{f.deploy}
			},
			wantApplies: true,
		},
		{
			name: "stage unmappable at its type ordinal -> undeterminable, fail closed",
			setup: func(_ *testing.T, f *deployedIdentityFixture) {
				// A second acceptance row ahead of ours makes ours ordinal 1;
				// the spec declares one acceptance stage.
				extra := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Type: run.StageTypeAcceptance, Sequence: 1}
				f.acceptance.Sequence = 2
				f.rr.stagesByRunID[f.runRow.ID] = []*run.Stage{f.deploy, extra, f.acceptance}
			},
			wantApplies: true,
		},
		{
			name: "deploy row missing -> unresolved",
			setup: func(_ *testing.T, f *deployedIdentityFixture) {
				f.rr.stagesByRunID[f.runRow.ID] = []*run.Stage{f.acceptance}
			},
			wantApplies: true,
		},
		{
			name: "more than one deployment input -> unresolved (ambiguous target)",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.runRow.WorkflowSpec = []byte(twoDeploySpec("          - {artifact: deployment, from_stage: deploy_a}\n          - {artifact: deployment, from_stage: deploy_b}\n"))
				second := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Type: run.StageTypeDeploy, Sequence: 1}
				f.acceptance.Sequence = 2
				f.rr.stagesByRunID[f.runRow.ID] = []*run.Stage{f.deploy, second, f.acceptance}
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
				f.addDeployment(t, second.ID, deployment("succeeded", "main", deployedSHAX))
			},
			wantApplies: true,
		},
		{
			name:        "artifact repo unconfigured -> unresolved",
			setup:       func(_ *testing.T, f *deployedIdentityFixture) { f.s.cfg.ArtifactRepo = nil },
			wantApplies: true,
		},
		{
			name: "artifact list error -> unresolved",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
				f.ar.listErr = errors.New("artifact store down")
			},
			wantApplies: true,
		},
		{
			name: "no deployment artifact -> unresolved",
			setup: func(_ *testing.T, f *deployedIdentityFixture) {
				f.addRaw(f.deploy.ID, artifact.KindPlan, []byte(`{"sha":"`+deployedSHAX+`","outcome":"succeeded"}`))
			},
			wantApplies: true,
		},
		{
			name: "newest rolled_back after a succeeded sha -> unresolved",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
				f.addDeployment(t, f.deploy.ID, deployment("rolled_back", "main", ""))
			},
			wantApplies: true,
		},
		{
			name: "newest undecodable after a succeeded sha -> unresolved",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
				f.addRaw(f.deploy.ID, artifact.KindDeployment, []byte(`not json`))
			},
			wantApplies: true,
		},
		{
			name: "symbolic ref only -> unresolved",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", ""))
			},
			wantApplies: true,
		},
		{
			name: "abbreviated hex ref -> unresolved",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "abc1234", ""))
			},
			wantApplies: true,
		},
		{
			name: "full-hex ref, no sha -> ref lowercased",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", strings.ToUpper("abcdefabcdefabcdefabcdefabcdefabcdefabcd"), ""))
			},
			wantSHA:     "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
			wantApplies: true,
		},
		{
			name: "sha preferred over a full-hex ref",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", deployedSHAY, deployedSHAX))
			},
			wantSHA:     deployedSHAX,
			wantApplies: true,
		},
		{
			name: "64-hex sha -> returned",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHA64))
			},
			wantSHA:     deployedSHA64,
			wantApplies: true,
		},
		{
			name: "two succeeded records with distinct SHAs -> unresolved (disagreement)",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAY))
			},
			wantApplies: true,
		},
		{
			name: "agreeing succeeded SHAs plus a sha-less record -> the SHA",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", ""))
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", strings.ToUpper(deployedSHAX)))
			},
			wantSHA:     deployedSHAX,
			wantApplies: true,
		},
		{
			name: "consumes the SECOND deploy -> maps by type ordinal to the second row",
			setup: func(t *testing.T, f *deployedIdentityFixture) {
				f.runRow.WorkflowSpec = []byte(twoDeploySpec("          - {artifact: deployment, from_stage: deploy_b}\n"))
				second := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Type: run.StageTypeDeploy, Sequence: 1}
				f.acceptance.Sequence = 2
				f.rr.stagesByRunID[f.runRow.ID] = []*run.Stage{f.acceptance, second, f.deploy}
				f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
				f.addDeployment(t, second.ID, deployment("succeeded", "main", deployedSHAY))
			},
			wantSHA:     deployedSHAY,
			wantApplies: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployedIdentityFixture(t)
			tc.setup(t, f)
			sha, applies := f.s.resolvePostDeployExpectedSHA(context.Background(), f.runRow.ID, f.acceptance.ID)
			if sha != tc.wantSHA || applies != tc.wantApplies {
				t.Errorf("resolvePostDeployExpectedSHA = (%q, %v), want (%q, %v)", sha, applies, tc.wantSHA, tc.wantApplies)
			}
		})
	}
}

// TestResolveAcceptanceExpectedHeadSHA_ReleaseArmNeverFallsBackToReportedHead
// drives the CALLER (approval conditions 2 and 3): every row carries a decoy
// reported head on the run's chain, so a caller that falls through to the
// reported-head ledger on an undeterminable or unresolved arm answers the decoy
// instead of the empty expectation the runner refuses pre-spawn.
func TestResolveAcceptanceExpectedHeadSHA_ReleaseArmNeverFallsBackToReportedHead(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *deployedIdentityFixture)
		want  string
	}{
		{"GetRun error", func(_ *testing.T, f *deployedIdentityFixture) { f.rr.runErr = errors.New("db down") }, ""},
		{"ListStagesForRun error", func(_ *testing.T, f *deployedIdentityFixture) { f.rr.stagesByRunID = nil }, ""},
		{"unparseable spec", func(_ *testing.T, f *deployedIdentityFixture) { f.runRow.WorkflowSpec = []byte("version: [") }, ""},
		{"stage absent from the run's rows", func(_ *testing.T, f *deployedIdentityFixture) {
			f.rr.stagesByRunID[f.runRow.ID] = []*run.Stage{f.deploy}
		}, ""},
		{"newest rolled_back", func(t *testing.T, f *deployedIdentityFixture) {
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
			f.addDeployment(t, f.deploy.ID, deployment("rolled_back", "main", ""))
		}, ""},
		{"symbolic ref only", func(t *testing.T, f *deployedIdentityFixture) {
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", ""))
		}, ""},
		{"no deployment artifact", func(*testing.T, *deployedIdentityFixture) {}, ""},
		{"deployed sha resolved", func(t *testing.T, f *deployedIdentityFixture) {
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
		}, deployedSHAX},
		{"feature_change spec keeps the reported head", func(_ *testing.T, f *deployedIdentityFixture) {
			f.runRow.WorkflowID = "feature_change"
			f.runRow.WorkflowSpec = []byte(featureChangeAcceptanceSpec)
		}, decoyReportedSHA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployedIdentityFixture(t)
			tc.setup(t, f)
			if got := f.s.resolveAcceptanceExpectedHeadSHA(context.Background(), f.runRow.ID, f.acceptance.ID); got != tc.want {
				t.Errorf("resolveAcceptanceExpectedHeadSHA = %q, want %q (decoy reported head is %q)", got, tc.want, decoyReportedSHA)
			}
		})
	}
}

// TestResolveAcceptanceExpectedHeadSHA_NonConsumingStageInConsumingWorkflow
// isolates the STAGE-level applicability leg: the workflow declares a
// deployment input (on `verify`), so the workflow-level check passes, but the
// stage under test is a SECOND acceptance stage (`smoke`) declaring none — it is
// positively known not to consume a deployment and keeps the reported head.
func TestResolveAcceptanceExpectedHeadSHA_NonConsumingStageInConsumingWorkflow(t *testing.T) {
	f := newDeployedIdentityFixture(t)
	f.runRow.WorkflowSpec = []byte(`version: "2"
workflows:
  release:
    stages:
` + deployStanza("deploy") + acceptanceStanzaHead + `        inputs:
          - {artifact: deployment, from_stage: deploy}
      - id: smoke
        type: acceptance
        executor: {agent: claude-code}
`)
	verify := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Type: run.StageTypeAcceptance, Sequence: 1}
	f.acceptance.Sequence = 2 // the stage under test is `smoke`, acceptance ordinal 1
	f.rr.stagesByRunID[f.runRow.ID] = []*run.Stage{f.deploy, verify, f.acceptance}
	f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))

	if sha, applies := f.s.resolvePostDeployExpectedSHA(context.Background(), f.runRow.ID, f.acceptance.ID); sha != "" || applies {
		t.Errorf("resolvePostDeployExpectedSHA = (%q, %v), want (\"\", false)", sha, applies)
	}
	if got := f.s.resolveAcceptanceExpectedHeadSHA(context.Background(), f.runRow.ID, f.acceptance.ID); got != decoyReportedSHA {
		t.Errorf("resolveAcceptanceExpectedHeadSHA = %q, want the reported head %q", got, decoyReportedSHA)
	}
	if got := f.s.resolveAcceptanceExpectedHeadSHA(context.Background(), f.runRow.ID, verify.ID); got != deployedSHAX {
		t.Errorf("consuming sibling `verify` = %q, want deployed %q", got, deployedSHAX)
	}
}

// TestResolveAcceptanceExpectedHeadSHAWalkingParents_ReleaseArmShortCircuits
// pins counterfactual C8: a release child whose deployment is unresolvable
// answers "" even though its ParentRunID chain carries a reported head, and a
// resolvable child answers its deployed SHA without walking.
func TestResolveAcceptanceExpectedHeadSHAWalkingParents_ReleaseArmShortCircuits(t *testing.T) {
	for _, tc := range []struct {
		name string
		sha  string
		want string
	}{
		{"unresolved deployment -> empty, no walk", "", ""},
		{"resolved deployment -> deployed sha", deployedSHAX, deployedSHAX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployedIdentityFixture(t)
			// Drop the own-run decoy: the decoy lives on the PARENT, so only
			// the walk can reach it.
			f.au.seeded = nil
			parentID := uuid.New()
			f.rr.getRuns[parentID] = &run.Run{ID: parentID}
			f.au.seeded = append(f.au.seeded, makeReportedHeadEntry(parentID, uuid.New(), "pull_request_opened", decoyReportedSHA, time.Now()))
			f.runRow.ParentRunID = &parentID
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", tc.sha))
			if got := f.s.resolveAcceptanceExpectedHeadSHAWalkingParents(context.Background(), f.runRow, f.acceptance.ID); got != tc.want {
				t.Errorf("WalkingParents = %q, want %q (parent's reported head is %q)", got, tc.want, decoyReportedSHA)
			}
		})
	}
}

// TestAcceptanceValidatedHeadSHA_ReleaseArm pins the verdict-binding hook and
// its placement AFTER both anchor guards (counterfactuals C6, C7).
func TestAcceptanceValidatedHeadSHA_ReleaseArm(t *testing.T) {
	anchor := func(f *deployedIdentityFixture, seq int64) {
		sid := f.acceptance.ID
		seedHeadEntry(f.au, f.runRow.ID, &sid, CategoryAcceptanceDispatched, seq, map[string]any{"stage_id": sid.String()})
	}
	cases := []struct {
		name      string
		setup     func(t *testing.T, f *deployedIdentityFixture)
		wantSHA   string
		wantBound bool
	}{
		{"anchored + succeeded deployment, no reported heads -> deployed sha", func(t *testing.T, f *deployedIdentityFixture) {
			f.au.seeded = nil
			anchor(f, 5)
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
		}, deployedSHAX, true},
		{"no dispatch anchor -> unresolved", func(t *testing.T, f *deployedIdentityFixture) {
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
		}, "", false},
		{"anchor at or below an acceptance_reopened marker -> stale, unresolved", func(t *testing.T, f *deployedIdentityFixture) {
			anchor(f, 5)
			sid := f.acceptance.ID
			seedHeadEntry(f.au, f.runRow.ID, &sid, CategoryAcceptanceReopened, 6, map[string]any{"stage_id": sid.String()})
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", deployedSHAX))
		}, "", false},
		{"anchored + unresolved deployment + pre-anchor reported head -> unresolved", func(t *testing.T, f *deployedIdentityFixture) {
			f.au.seeded = nil
			seedHeadEntry(f.au, f.runRow.ID, nil, "pull_request_opened", 1, map[string]any{"head_sha": decoyReportedSHA})
			anchor(f, 5)
			f.addDeployment(t, f.deploy.ID, deployment("succeeded", "main", ""))
		}, "", false},
		{"feature_change spec keeps the reported-head walk", func(_ *testing.T, f *deployedIdentityFixture) {
			f.au.seeded = nil
			f.runRow.WorkflowID = "feature_change"
			f.runRow.WorkflowSpec = []byte(featureChangeAcceptanceSpec)
			seedHeadEntry(f.au, f.runRow.ID, nil, "pull_request_opened", 1, map[string]any{"head_sha": decoyReportedSHA})
			anchor(f, 5)
		}, decoyReportedSHA, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployedIdentityFixture(t)
			tc.setup(t, f)
			sha, ok := f.s.acceptanceValidatedHeadSHA(context.Background(), f.runRow.ID, f.acceptance.ID)
			if sha != tc.wantSHA || ok != tc.wantBound {
				t.Errorf("acceptanceValidatedHeadSHA = (%q, %v), want (%q, %v)", sha, ok, tc.wantSHA, tc.wantBound)
			}
		})
	}
}

// TestShipAcceptance_PostDeploy_PassBindsDeployedSHA is approval condition 4:
// a passed verdict shipped through the REAL acceptance handler for a
// post-deploy stage records acceptance_outcome_recorded with head_sha = the
// deployed SHA and verdict passed — not clamped to undecidable(head_unresolved),
// which is what a release run (no reported-head ledger) recorded before #1599.
// The control row ships the same verdict against a symbolic-only deployment and
// asserts the clamp, so the passing row cannot be green for an unrelated reason.
func TestShipAcceptance_PostDeploy_PassBindsDeployedSHA(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sha         string
		wantHead    string
		wantVerdict string
	}{
		{"deployed sha recorded -> passed, bound to it", deployedSHAX, deployedSHAX, "passed"},
		{"symbolic-only deployment -> clamped undecidable", "", "", "undecidable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, au, rr := newAcceptanceServer(t, runID, stageID)
			acceptance := rr.getStages[stageID]
			acceptance.Sequence = 1
			deploy := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeDeploy, Sequence: 0, State: run.StageStateSucceeded}
			rr.getStages[deploy.ID] = deploy
			rr.getRuns[runID] = &run.Run{ID: runID, WorkflowID: "release", WorkflowSpec: releaseExampleSpec(t)}
			rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {deploy, acceptance}}
			body, _ := json.Marshal(deployment("succeeded", "main", tc.sha))
			ar.all = append(ar.all, &artifact.Artifact{ID: uuid.New(), StageID: deploy.ID, Kind: artifact.KindDeployment, Content: body, CreatedAt: time.Now().UTC()})
			seedHeadEntry(au, runID, &stageID, CategoryAcceptanceDispatched, 2, map[string]any{"stage_id": stageID.String()})
			priv, _ := sf.issue(t, runID)

			w := shipAcceptanceRequest(t, s, runID, stageID, priv, validAcceptanceBytes(t), "")
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			var recorded *audit.ChainAppendParams
			for i := range au.appended {
				if au.appended[i].Category == "acceptance_outcome_recorded" {
					recorded = &au.appended[i]
				}
			}
			if recorded == nil {
				t.Fatal("no acceptance_outcome_recorded entry appended")
			}
			var payload struct {
				HeadSHA string `json:"head_sha"`
				Verdict string `json:"verdict"`
			}
			if err := json.Unmarshal(recorded.Payload, &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if payload.HeadSHA != tc.wantHead || payload.Verdict != tc.wantVerdict {
				t.Errorf("acceptance_outcome_recorded head_sha=%q verdict=%q, want head_sha=%q verdict=%q:\n%s",
					payload.HeadSHA, payload.Verdict, tc.wantHead, tc.wantVerdict, recorded.Payload)
			}
		})
	}
}

// TestRunStageForSpecStage pins the inverse type-ordinal mapping directly,
// including a repeated deploy type and a Sequence tie broken by stage ID.
func TestRunStageForSpecStage(t *testing.T) {
	wf := spec.Workflow{Stages: []spec.Stage{
		{ID: "plan", Type: spec.StageTypePlan},
		{ID: "deploy_a", Type: spec.StageTypeDeploy},
		{ID: "deploy_b", Type: spec.StageTypeDeploy},
		{ID: "verify", Type: spec.StageTypeAcceptance},
	}}
	runID := uuid.New()
	a := &run.Stage{ID: uuid.MustParse("00000000-0000-0000-0000-00000000000a"), RunID: runID, Type: run.StageTypeDeploy, Sequence: 0}
	b := &run.Stage{ID: uuid.MustParse("00000000-0000-0000-0000-00000000000b"), RunID: runID, Type: run.StageTypeDeploy, Sequence: 0}
	v := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeAcceptance, Sequence: 1}
	rows := []*run.Stage{v, b, a} // repo order is not trusted

	for _, tc := range []struct {
		specID string
		want   *run.Stage
	}{
		{"deploy_a", a},
		{"deploy_b", b},
		{"verify", v},
		{"plan", nil},    // a plan-filtered child carries no plan row
		{"missing", nil}, // no such spec stage
	} {
		got, ok := runStageForSpecStage(wf, rows, tc.specID)
		if (tc.want == nil) != !ok || (tc.want != nil && got != tc.want) {
			t.Errorf("runStageForSpecStage(%q) = (%v, %v), want %v", tc.specID, got, ok, tc.want)
		}
	}
	if _, ok := runStageForSpecStage(wf, []*run.Stage{a, v}, "deploy_b"); ok {
		t.Error("runStageForSpecStage(deploy_b) with one deploy row = ok, want a row/spec disagreement")
	}
}
