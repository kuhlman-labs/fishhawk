package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// gateContainerDigest is a syntactically valid sha256 digest for fixtures.
const gateContainerDigest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// gateContainerStageImage / gateContainerWorkflowImage are the two declared
// images the precedence fixtures distinguish; gateContainerGoldenImage is
// the image the shared golden's `stage_image` member carries.
const (
	gateContainerStageImage    = "ghcr.io/org/stage-gate" + gateContainerDigest
	gateContainerWorkflowImage = "ghcr.io/org/workflow-gate" + gateContainerDigest
	gateContainerGoldenImage   = "ghcr.io/kuhlman-labs/fishhawk-gate" + gateContainerDigest
)

// gateContainerFixture seeds the prompt fake with a run on workflow wfID of
// specYAML and one stage row per type in stageTypes (Sequence = index), and
// returns the server, the signing key and the stage rows.
func gateContainerFixture(t *testing.T, specYAML, wfID string, stageTypes ...run.StageType) (*Server, *promptRunRepo, uuid.UUID, []*run.Stage, func(stageID uuid.UUID) *httptest.ResponseRecorder) {
	t.Helper()
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	priv, _ := sf.issue(t, runID)
	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    wfID,
		TriggerSource: "manual",
		WorkflowSpec:  []byte(specYAML),
	}
	rows := make([]*run.Stage, 0, len(stageTypes))
	for i, typ := range stageTypes {
		st := &run.Stage{ID: uuid.New(), RunID: runID, Type: typ, Sequence: i}
		rows = append(rows, st)
		rr.getStages[st.ID] = st
	}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: rows}
	fetch := func(stageID uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		w := promptRequest(t, s, runID, stageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("prompt status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		return w
	}
	return s, rr, runID, rows, fetch
}

// decodeGateContainer decodes the response body's gate_container field.
func decodeGateContainer(t *testing.T, body []byte) *gateContainerConfig {
	t.Helper()
	var resp promptResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	return resp.GateContainer
}

// gateContainerPrecedenceSpec declares a workflow-level image and, on the
// implement stage, a stage-level image (when stageBlock is true).
func gateContainerPrecedenceSpec(stageBlock bool) string {
	doc := `version: "2"
workflows:
  feature_change:
    gate_container:
      image: ` + gateContainerWorkflowImage + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	if stageBlock {
		doc += "        gate_container:\n          image: " + gateContainerStageImage + "\n"
	}
	return doc
}

// TestGetStagePrompt_GateContainerStageOverridesWorkflow pins precedence
// stage > workflow on the signed prompt fetch: the implement stage's own
// block wins with source "stage".
func TestGetStagePrompt_GateContainerStageOverridesWorkflow(t *testing.T) {
	_, _, _, rows, fetch := gateContainerFixture(t, gateContainerPrecedenceSpec(true), "feature_change",
		run.StageTypePlan, run.StageTypeImplement)

	got := decodeGateContainer(t, fetch(rows[1].ID).Body.Bytes())
	want := gateContainerConfig{Image: gateContainerStageImage, Source: spec.GateContainerSourceStage}
	if got == nil || *got != want {
		t.Fatalf("gate_container = %+v, want %+v", got, want)
	}
}

// TestGetStagePrompt_GateContainerWorkflowLevel pins that a stage declaring
// nothing inherits the workflow-level block with source "workflow" — on the
// overriding fixture's silent sibling (plan) and on a fixture where no stage
// overrides.
func TestGetStagePrompt_GateContainerWorkflowLevel(t *testing.T) {
	want := gateContainerConfig{Image: gateContainerWorkflowImage, Source: spec.GateContainerSourceWorkflow}

	_, _, _, rows, fetch := gateContainerFixture(t, gateContainerPrecedenceSpec(false), "feature_change",
		run.StageTypePlan, run.StageTypeImplement)
	if got := decodeGateContainer(t, fetch(rows[1].ID).Body.Bytes()); got == nil || *got != want {
		t.Fatalf("implement gate_container = %+v, want %+v", got, want)
	}

	_, _, _, rows, fetch = gateContainerFixture(t, gateContainerPrecedenceSpec(true), "feature_change",
		run.StageTypePlan, run.StageTypeImplement)
	if got := decodeGateContainer(t, fetch(rows[0].ID).Body.Bytes()); got == nil || *got != want {
		t.Fatalf("silent plan stage gate_container = %+v, want the workflow block %+v", got, want)
	}
}

// TestGetStagePrompt_GateContainerOmittedWhenAbsent pins the byte-identical
// back-compat path: a workflow declaring no gate_container serves NO key.
// That the resolver then never lists stages is asserted by the
// "workflow declares nothing" row of TestResolveGateContainerConfig_Degradations,
// which counts the calls.
func TestGetStagePrompt_GateContainerOmittedWhenAbsent(t *testing.T) {
	const plain = `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	_, _, _, rows, fetch := gateContainerFixture(t, plain, "feature_change", run.StageTypeImplement)
	w := fetch(rows[0].ID)
	if got := decodeGateContainer(t, w.Body.Bytes()); got != nil {
		t.Fatalf("gate_container = %+v, want nil", got)
	}
	if strings.Contains(w.Body.String(), "gate_container") {
		t.Fatalf("gate_container key must be omitted when undeclared:\n%s", w.Body.String())
	}
}

// gateContainerIdentitySpec declares TWO implement stages with DIFFERENT
// images and no workflow-level block, so a first-of-type lookup serves the
// first image to both.
const gateContainerIdentitySpec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement_a
        type: implement
        executor:
          agent: claude-code
        gate_container:
          image: ghcr.io/org/first` + gateContainerDigest + `
      - id: implement_b
        type: implement
        executor:
          agent: claude-code
        gate_container:
          image: ghcr.io/org/second` + gateContainerDigest + `
`

// TestGetStagePrompt_GateContainerResolvedByStageIdentity pins resolution by
// stage IDENTITY: with two implement stages declaring different images, the
// prompt for each stage row returns its OWN image. A first-of-type lookup
// serves the first image to the second stage.
func TestGetStagePrompt_GateContainerResolvedByStageIdentity(t *testing.T) {
	_, _, _, rows, fetch := gateContainerFixture(t, gateContainerIdentitySpec, "feature_change",
		run.StageTypeImplement, run.StageTypeImplement)

	for i, wantImage := range []string{"ghcr.io/org/first" + gateContainerDigest, "ghcr.io/org/second" + gateContainerDigest} {
		got := decodeGateContainer(t, fetch(rows[i].ID).Body.Bytes())
		want := gateContainerConfig{Image: wantImage, Source: spec.GateContainerSourceStage}
		if got == nil || *got != want {
			t.Fatalf("stage row %d gate_container = %+v, want its own block %+v", i, got, want)
		}
	}
}

// TestGetStagePrompt_GateContainerExtendsDerivedOverride runs on a DERIVING
// workflow (extends: base) whose stage overrides the base stage's {image}
// with {dockerfile, context}: the handler serves exactly the deriving source
// with source "stage" and no image — the reuse source rule reaching the
// wire — and the base's workflow-level block is not inherited.
func TestGetStagePrompt_GateContainerExtendsDerivedOverride(t *testing.T) {
	const doc = `version: "2"
workflows:
  base:
    gate_container:
      image: ghcr.io/org/base-workflow:1
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        gate_container:
          image: ghcr.io/org/base-stage` + gateContainerDigest + `
  derived:
    extends: base
    stages:
      - id: apply
        type: implement
        gate_container:
          dockerfile: build/gate/Dockerfile
          context: build/gate
`
	_, _, _, rows, fetch := gateContainerFixture(t, doc, "derived", run.StageTypeImplement)
	got := decodeGateContainer(t, fetch(rows[0].ID).Body.Bytes())
	want := gateContainerConfig{Dockerfile: "build/gate/Dockerfile", Context: "build/gate", Source: spec.GateContainerSourceStage}
	if got == nil || *got != want {
		t.Fatalf("derived gate_container = %+v, want exactly the deriving build source %+v", got, want)
	}
}

// TestGetStagePromptRender_CarriesGateContainer pins the SECOND construction
// site: the SPA-readable prompt-render path serves the same resolution as
// the signed fetch, by stage identity.
func TestGetStagePromptRender_CarriesGateContainer(t *testing.T) {
	s, _, _, rows, _ := gateContainerFixture(t, gateContainerIdentitySpec, "feature_change",
		run.StageTypeImplement, run.StageTypeImplement)

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+rows[1].ID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("render status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	got := decodeGateContainer(t, w.Body.Bytes())
	want := gateContainerConfig{Image: "ghcr.io/org/second" + gateContainerDigest, Source: spec.GateContainerSourceStage}
	if got == nil || *got != want {
		t.Fatalf("render gate_container = %+v, want %+v", got, want)
	}
}

// countingStagesRepo counts ListStagesForRun calls and can force an error.
type countingStagesRepo struct {
	*promptRunRepo
	listCalls int
	listErr   error
}

func (c *countingStagesRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	c.listCalls++
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.promptRunRepo.ListStagesForRun(ctx, runID)
}

// TestResolveGateContainerConfig_Degradations pins each degradation branch.
// nil: no run, no stage, no cached spec, an unparseable spec, a workflow
// absent from the spec, and a workflow declaring nothing (which must not
// list stages). Workflow-level fallback: a stage-list error and a stage that
// cannot be mapped onto its spec stage; and nil for both when the workflow
// itself declares no block.
func TestResolveGateContainerConfig_Degradations(t *testing.T) {
	declaring := gateContainerPrecedenceSpec(true)
	stageOnly := gateContainerIdentitySpec
	const undeclared = `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	workflowBlock := &gateContainerConfig{Image: gateContainerWorkflowImage, Source: spec.GateContainerSourceWorkflow}

	newRepo := func(runID uuid.UUID, rows []*run.Stage, listErr error) *countingStagesRepo {
		rr := newPromptRunRepo()
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: rows}
		return &countingStagesRepo{promptRunRepo: rr, listErr: listErr}
	}

	cases := []struct {
		name       string
		nilRun     bool
		nilStage   bool
		spec       string
		noSpec     bool
		workflowID string
		// rowsHaveStage controls whether the resolved stage appears in the
		// listed rows (false = unmappable).
		rowsHaveStage bool
		stageType     run.StageType
		listErr       error
		want          *gateContainerConfig
		wantListCalls int
	}{
		{name: "nil run", nilRun: true, spec: declaring, workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeImplement},
		{name: "nil stage", nilStage: true, spec: declaring, workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeImplement},
		{name: "no cached spec", noSpec: true, workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeImplement},
		{name: "unparseable spec", spec: "{{{not yaml", workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeImplement},
		{name: "workflow not in spec", spec: declaring, workflowID: "other_workflow", rowsHaveStage: true, stageType: run.StageTypeImplement},
		{name: "workflow declares nothing: no stage listing", spec: undeclared, workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeImplement},
		{name: "list error degrades to workflow block", spec: declaring, workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeImplement, listErr: errors.New("db down"), want: workflowBlock, wantListCalls: 1},
		{name: "unmappable stage degrades to workflow block", spec: declaring, workflowID: "feature_change", rowsHaveStage: false, stageType: run.StageTypeImplement, want: workflowBlock, wantListCalls: 1},
		{name: "stage type beyond the spec ordinal degrades to workflow block", spec: declaring, workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeReview, want: workflowBlock, wantListCalls: 1},
		{name: "list error with no workflow block is nil", spec: stageOnly, workflowID: "feature_change", rowsHaveStage: true, stageType: run.StageTypeImplement, listErr: errors.New("db down"), wantListCalls: 1},
		{name: "unmappable stage with no workflow block is nil", spec: stageOnly, workflowID: "feature_change", rowsHaveStage: false, stageType: run.StageTypeImplement, wantListCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			stage := &run.Stage{ID: uuid.New(), RunID: runID, Type: tc.stageType}
			var rows []*run.Stage
			if tc.rowsHaveStage {
				rows = []*run.Stage{stage}
			} else {
				rows = []*run.Stage{{ID: uuid.New(), RunID: runID, Type: tc.stageType}}
			}
			repo := newRepo(runID, rows, tc.listErr)
			s, _, _ := newPromptServerRepo(t, repo)

			var runRow *run.Run
			if !tc.nilRun {
				runRow = &run.Run{ID: runID, WorkflowID: tc.workflowID}
				if !tc.noSpec {
					runRow.WorkflowSpec = []byte(tc.spec)
				}
			}
			if tc.nilStage {
				stage = nil
			}
			got := s.resolveGateContainerConfig(context.Background(), runRow, stage)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("resolveGateContainerConfig = %+v, want %+v", got, tc.want)
			}
			if repo.listCalls != tc.wantListCalls {
				t.Errorf("ListStagesForRun calls = %d, want %d", repo.listCalls, tc.wantListCalls)
			}
		})
	}
}

// gateContainerGolden reads the CROSS-MODULE GOLDEN FIXTURE
// testdata/wire/gate_container_prompt.json (E51.3 / #2136) — the SAME file
// the runner's upload decode test reads — as compacted member bytes. Fails
// closed on a read or decode error.
func gateContainerGolden(t *testing.T) map[string][]byte {
	t.Helper()
	path := filepath.Join(repoRoot(t), "testdata", "wire", "gate_container_prompt.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared gate_container golden %s: %v", path, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		t.Fatalf("decode shared gate_container golden: %v", err)
	}
	out := make(map[string][]byte, len(members))
	for k, raw := range members {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			t.Fatalf("compact member %s: %v", k, err)
		}
		out[k] = buf.Bytes()
	}
	return out
}

// TestGetStagePrompt_GateContainerMatchesSharedGolden pins the EMIT side of
// the cross-module contract: the gate_container bytes the real /prompt
// handler serves equal the shared golden's members byte-for-byte. The
// runner's TestFetchPrompt_DecodesGateContainerSharedGolden decodes the
// same members, so a tag rename on either side reddens one of the two.
func TestGetStagePrompt_GateContainerMatchesSharedGolden(t *testing.T) {
	golden := gateContainerGolden(t)
	cases := map[string]string{
		"stage_image": `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        gate_container:
          image: ` + gateContainerGoldenImage + `
`,
		"workflow_build": `version: "2"
workflows:
  feature_change:
    gate_container:
      dockerfile: build/gate/Dockerfile
      context: build/gate
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`,
	}
	if len(golden) != len(cases) {
		t.Fatalf("golden members = %d, want %d (a member without an emit case is unpinned)", len(golden), len(cases))
	}
	for member, doc := range cases {
		t.Run(member, func(t *testing.T) {
			want, ok := golden[member]
			if !ok {
				t.Fatalf("golden has no member %q", member)
			}
			_, _, _, rows, fetch := gateContainerFixture(t, doc, "feature_change", run.StageTypeImplement)
			var body map[string]json.RawMessage
			if err := json.Unmarshal(fetch(rows[0].ID).Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			var got bytes.Buffer
			if err := json.Compact(&got, body["gate_container"]); err != nil {
				t.Fatalf("compact gate_container %q: %v", body["gate_container"], err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("gate_container bytes = %s\nwant shared golden %s", got.Bytes(), want)
			}
		})
	}
}
