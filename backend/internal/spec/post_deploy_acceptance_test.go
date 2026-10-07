package spec_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// releaseAcceptanceExamplePath is the committed post-deploy acceptance example
// (E35.1 / #1598). It is also the driver of the release seam test in
// backend/internal/server and the stanza the operator hand-applies to the live
// release workflow, so this test welds it to the parser.
const releaseAcceptanceExamplePath = "../../../docs/spec/examples/workflow-v2-release-acceptance.yaml"

// TestReleaseAcceptanceExample_Parses reads the committed example FROM DISK and
// asserts the deploy -> acceptance shape the rest of E35 builds on.
func TestReleaseAcceptanceExample_Parses(t *testing.T) {
	data, err := os.ReadFile(releaseAcceptanceExamplePath)
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	parsed, err := spec.ParseBytes(data)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", releaseAcceptanceExamplePath, err)
	}
	if parsed.Version != "2" {
		t.Errorf("Version = %q, want \"2\"", parsed.Version)
	}
	wf, ok := parsed.Workflows["release"]
	if !ok {
		t.Fatalf("example declares no `release` workflow; got %d workflows", len(parsed.Workflows))
	}
	if len(wf.Stages) != 2 {
		t.Fatalf("release stages = %d, want 2 (deploy then acceptance)", len(wf.Stages))
	}
	deploy, accept := wf.Stages[0], wf.Stages[1]
	if deploy.Type != spec.StageTypeDeploy || accept.Type != spec.StageTypeAcceptance {
		t.Fatalf("stage order = [%s %s], want [deploy acceptance]", deploy.Type, accept.Type)
	}
	if len(accept.Inputs) != 1 || accept.Inputs[0].Artifact != string(spec.ArtifactDeployment) || accept.Inputs[0].FromStage != deploy.ID {
		t.Errorf("acceptance inputs = %+v, want exactly [{artifact: deployment, from_stage: %s}]", accept.Inputs, deploy.ID)
	}
	if accept.Egress == nil || len(accept.Egress.TargetHosts) == 0 {
		t.Errorf("acceptance egress = %+v, want a non-empty target_hosts", accept.Egress)
	}
	var envs []string
	for _, c := range deploy.Constraints {
		envs = append(envs, c.AllowedEnvironments...)
	}
	if len(envs) == 0 {
		t.Error("deploy stage declares no allowed_environments; the example must pin a staging target")
	}
	for _, env := range envs {
		if strings.EqualFold(env, "production") {
			t.Errorf("deploy allowed_environments = %v includes production; the example is staging-only (ADR-053 Decision 3)", envs)
		}
	}
}

// postDeployDoc renders a minimal v2 release workflow: a staging deploy stage
// followed by whatever consumer stage(s) the case supplies (already indented as
// list items under `stages:`). deployEnvs is the deploy stage's
// allowed_environments flow list body.
func postDeployDoc(deployEnvs, consumers string) string {
	return `version: "2"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        constraints:
          allowed_environments: [` + deployEnvs + `]
        produces:
          - artifact: deployment
` + consumers
}

// acceptanceConsumer is a deployment-consuming acceptance stage with the given
// extra stage-level YAML (egress / permissions), indented under the item.
func acceptanceConsumer(from, extra string) string {
	return `      - id: verify
        type: acceptance
        executor:
          agent: claude-code
        inputs:
          - artifact: deployment
            from_stage: ` + from + `
` + extra
}

const stagingEgress = `        egress:
          target_hosts: [staging.example.com]
`

// TestParse_PostDeployAcceptance_Rejections is the per-failure-mode table: one
// schema-valid fixture per binding rule, each isolating that rule (every other
// rule is satisfied), asserting the exact shipped message AND the reported
// path. The counterfactual for each row is recorded in the PR: disable the
// rule's condition and ParseBytes returns nil, so the row goes RED.
func TestParse_PostDeployAcceptance_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantPath string
		wantMsg  string
	}{
		{
			// Rule 1 (C1): an AGENT-executed review stage WITH egress consuming
			// deployment from an earlier staging deploy — admitted by the v2
			// schema, by from_stage resolution and by the v2 egress binding
			// (egress is valid on any agent stage), so only the consumer-type
			// rule rejects it. A human executor or a missing egress would let
			// a later rule mask the deletion.
			name: "consumer_not_acceptance",
			doc: postDeployDoc("staging", `      - id: signoff
        type: review
        executor:
          agent: claude-code
        inputs:
          - artifact: deployment
            from_stage: deploy
`+stagingEgress),
			wantPath: "/workflows/release/stages/1/inputs/0/artifact",
			wantMsg:  fmt.Sprintf(spec.MsgFmtDeploymentInputNotAcceptance, "review"),
		},
		{
			// Rule 2 (C2): an acceptance stage WITH egress consuming
			// deployment from an implement stage — egress is declared and the
			// referent has no allowed_environments, so only the referent-type
			// rule rejects it.
			name: "referent_not_deploy",
			doc: `version: "2"
workflows:
  release:
    stages:
      - id: build
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
` + acceptanceConsumer("build", stagingEgress),
			wantPath: "/workflows/release/stages/1/inputs/0/from_stage",
			wantMsg:  fmt.Sprintf(spec.MsgFmtDeploymentInputNotFromDeploy, "build", "implement"),
		},
		{
			// Rule 3 (condition 2, ADR-053 Decision 3): the deploy stage may
			// target production. Mixed case pins the case-insensitive match;
			// egress is declared so the egress rule cannot mask it.
			name:     "deploy_allows_production",
			doc:      postDeployDoc("staging, Production", acceptanceConsumer("deploy", stagingEgress)),
			wantPath: "/workflows/release/stages/1/inputs/0/from_stage",
			wantMsg:  fmt.Sprintf(spec.MsgFmtDeploymentInputProductionTarget, "verify", "deploy", "Production"),
		},
		{
			// Rule 4 (C3): an acceptance stage consuming deployment from a
			// staging deploy with neither egress nor permissions.network —
			// otherwise valid, since egress is optional on acceptance.
			name:     "missing_egress",
			doc:      postDeployDoc("staging", acceptanceConsumer("deploy", "")),
			wantPath: "/workflows/release/stages/1/egress",
			wantMsg:  spec.MsgDeploymentInputRequiresEgress,
		},
		{
			// Ordering pin: deployment from a LATER deploy stage reports the
			// existing must-be-earlier rule, which runs before the binding.
			name: "deploy_referent_later",
			doc: `version: "2"
workflows:
  release:
    stages:
` + acceptanceConsumer("deploy", stagingEgress) + `      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        produces:
          - artifact: deployment
`,
			wantPath: "/workflows/release/stages/0/inputs/0/from_stage",
			wantMsg:  fmt.Sprintf(spec.MsgFmtFromStageNotEarlier, "deploy", 1, 0),
		},
		{
			// needs pin: `needs: [deploy]` stays rejected — the deployment
			// input is consumer-bound, so a deploy referent has no default.
			name: "needs_deploy_still_rejected",
			doc: postDeployDoc("staging", `      - id: verify
        type: acceptance
        executor:
          agent: claude-code
        needs: [deploy]
`+stagingEgress),
			wantPath: fmt.Sprintf(spec.PathFmtNeeds, "release", 1, 0),
			wantMsg:  fmt.Sprintf(spec.MsgFmtNeedsNoDefaultArtifact, "deploy", "deploy", "deploy"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(tc.doc))
			if err == nil {
				t.Fatal("ParseBytes accepted the document; want a ValidationError")
			}
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %T %v, want *spec.ValidationError", err, err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", ve.Path, tc.wantPath)
			}
			if ve.Message != tc.wantMsg {
				t.Errorf("Message = %q\nwant      %q", ve.Message, tc.wantMsg)
			}
		})
	}
}

// TestParse_PostDeployAcceptance_Accepted pins the positive twins: each
// rejection above has a nearby document that parses. The permissions.network
// twin guards against an egress rule that reads only the raw `egress` key.
func TestParse_PostDeployAcceptance_Accepted(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{
			name: "egress_target_hosts",
			doc:  postDeployDoc("staging", acceptanceConsumer("deploy", stagingEgress)),
		},
		{
			name: "permissions_network_spelling",
			doc: postDeployDoc("staging", acceptanceConsumer("deploy", `        permissions:
          network:
            target_hosts: [staging.example.com]
`)),
		},
		{
			// An environment merely CONTAINING the word is not the production
			// declaration; only the literal name (any case) is refused.
			name: "non_production_environment_names",
			doc:  postDeployDoc("staging, preproduction, qa", acceptanceConsumer("deploy", stagingEgress)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := spec.ParseBytes([]byte(tc.doc))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			st := parsed.Workflows["release"].Stages[1]
			if st.Egress == nil || len(st.Egress.TargetHosts) != 1 {
				t.Errorf("verify Egress = %+v, want the declared staging host", st.Egress)
			}
		})
	}
}

// TestParse_PostDeployAcceptance_FrozenMajorRejects pins that the frozen
// workflow-v1 schema did NOT gain the enum member: a v1.3 document declaring a
// deployment input fails at the schema layer, before Validate.
func TestParse_PostDeployAcceptance_FrozenMajorRejects(t *testing.T) {
	doc := `version: "1.3"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        constraints:
          - allowed_environments: [staging]
        produces:
          - artifact: deployment
      - id: verify
        type: acceptance
        executor:
          agent: claude-code
        inputs:
          - artifact: deployment
            from_stage: deploy
        egress:
          target_hosts: [staging.example.com]
`
	_, err := spec.ParseBytes([]byte(doc))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T %v, want *spec.SchemaError (v1 is frozen; its input enum has no deployment member)", err, err)
	}
	if !strings.Contains(se.Path, "/inputs/0") {
		t.Errorf("SchemaError.Path = %q, want it under the verify stage's inputs/0", se.Path)
	}
}
