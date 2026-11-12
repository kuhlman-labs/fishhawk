package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// planRevisionKey is the wire key the runner's upload.FetchedPrompt decodes
// (#4867). Asserted as a RAW key so an absent field is distinguished from a
// false one: the omitempty contract is that a first-pass plan and every
// non-plan stage carry no key at all.
const planRevisionKey = `"plan_revision"`

// planRevisionFixture is a run with a plan stage parked at its approval gate
// and an implement stage, served through both prompt endpoints.
type planRevisionFixture struct {
	s           *Server
	rr          *promptRunRepo
	au          *auditFake
	runID       uuid.UUID
	planStageID uuid.UUID
	implStageID uuid.UUID
	priv        ed25519.PrivateKey
}

func newPlanRevisionFixture(t *testing.T, planState run.StageState) *planRevisionFixture {
	t.Helper()
	runID, planStageID, implStageID := uuid.New(), uuid.New(), uuid.New()
	sf := newSigningFake()
	priv, _ := sf.issue(t, runID)
	rr := newPromptRunRepo()
	au := newAuditFake()
	art := newFakeArtifactRepo()
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
		}},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	rr.getStages[planStageID] = &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: planState}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {rr.getStages[planStageID], rr.getStages[implStageID]}}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/example", WorkflowID: "feature_change",
		TriggerSource: run.TriggerCLI, RequiresCharter: chFalse()}

	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, SigningRepo: sf, ArtifactRepo: art})
	s.promptIssueGetterOverride = &stubIssueGetter{}
	return &planRevisionFixture{s: s, rr: rr, au: au, runID: runID,
		planStageID: planStageID, implStageID: implStageID, priv: priv}
}

// serve fetches stageID's prompt through BOTH endpoints: the signed
// runner-facing prompt and the unsigned prompt-render preview.
func (f *planRevisionFixture) serve(t *testing.T, stageID uuid.UUID) map[string]*httptest.ResponseRecorder {
	t.Helper()
	out := map[string]*httptest.ResponseRecorder{
		"signed":        promptRequest(t, f.s, f.runID, stageID, f.priv, ""),
		"prompt-render": promptRenderRequest(t, f.s, stageID),
	}
	for surface, w := range out {
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200:\n%s", surface, w.Code, w.Body.String())
		}
	}
	return out
}

// goldenPlanRevisionPromptJSON reads the CROSS-MODULE GOLDEN FIXTURE for the
// #4867 plan_revision prompt-response field from the SINGLE shared file
// testdata/wire/plan_revision_prompt.json. The runner's cross-boundary test
// (runner/cmd/fishhawk-runner/planrevisionadvance_test.go) decodes the SAME
// bytes into upload.FetchedPrompt and drives them through the advance to the
// shipped plan seed, so a tag drift on either side reddens one of the two.
func goldenPlanRevisionPromptJSON(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "testdata", "wire", "plan_revision_prompt.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared plan-revision prompt fixture %s: %v", path, err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// TestPromptPlanRevision_FirstPassOmitsKey: a plan stage of a run with NO
// plan_revised row serves no plan_revision key on either endpoint, so a
// first-pass plan dispatch is byte-unchanged (counterfactual (f): a
// planRevisionDispatch body of `return true` reddens this).
func TestPromptPlanRevision_FirstPassOmitsKey(t *testing.T) {
	f := newPlanRevisionFixture(t, run.StageStatePending)
	for surface, w := range f.serve(t, f.planStageID) {
		if strings.Contains(w.Body.String(), planRevisionKey) {
			t.Errorf("%s: a first-pass plan serve carries %s:\n%s", surface, planRevisionKey, w.Body.String())
		}
	}
}

// TestPromptPlanRevision_ServedAfterRevise crosses request → persistence →
// prompt: a REAL handleRevisePlan records the plan_revised row and re-opens the
// stage, and BOTH endpoints then serve plan_revision:true. Each endpoint is its
// own subtest, so deleting the assignment from ONE handler's response literal
// reddens only that endpoint's subtest (counterfactual (j)). The served
// {plan_revision, stage_type} pair equals the shared golden the runner decodes.
func TestPromptPlanRevision_ServedAfterRevise(t *testing.T) {
	f := newPlanRevisionFixture(t, run.StageStateAwaitingApproval)
	w := revisePlan(t, f.s, f.planStageID,
		`{"constraint":"advance the reused lineage worktree before re-planning"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("revise status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := f.rr.getStages[f.planStageID].State; got != run.StageStatePending {
		t.Fatalf("stage state after revise = %q, want pending", got)
	}
	golden := goldenPlanRevisionPromptJSON(t)

	for _, surface := range []string{"signed", "prompt-render"} {
		t.Run(surface, func(t *testing.T) {
			var pw *httptest.ResponseRecorder
			if surface == "signed" {
				pw = promptRequest(t, f.s, f.runID, f.planStageID, f.priv, "")
			} else {
				pw = promptRenderRequest(t, f.s, f.planStageID)
			}
			if pw.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", pw.Code, pw.Body.String())
			}
			body := pw.Body.String()
			if !strings.Contains(body, planRevisionKey+":true") {
				t.Fatalf("revised plan serve lacks %s:true:\n%s", planRevisionKey, body)
			}
			// The binding constraint section and the signal share one source.
			var resp promptResponse
			if err := json.Unmarshal(pw.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(resp.Prompt, "Revision constraint (binding") {
				t.Errorf("revised plan prompt lacks the Revision constraint section:\n%s", resp.Prompt)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(pw.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode raw: %v", err)
			}
			pair, err := json.Marshal(map[string]json.RawMessage{
				"plan_revision": raw["plan_revision"],
				"stage_type":    raw["stage_type"],
			})
			if err != nil {
				t.Fatal(err)
			}
			if string(pair) != golden {
				t.Errorf("served pair = %s, want the shared golden %s", pair, golden)
			}
		})
	}
}

// TestPromptPlanRevision_NonPlanStageOmitsKey: a NON-plan stage of a run that
// holds a plan_revised row serves no plan_revision key — trigger.RevisionConstraint
// is loaded only inside the plan branch, so the implement dispatch (whose own
// advance is the #3454 standalone block) never sees the plan signal.
func TestPromptPlanRevision_NonPlanStageOmitsKey(t *testing.T) {
	f := newPlanRevisionFixture(t, run.StageStateSucceeded)
	seedPlanRevised(f.au, f.runID, f.planStageID)
	if got := f.s.loadRevisionConstraint(context.Background(), f.runID); got == nil {
		t.Fatal("fixture precondition: the run holds no loadable plan_revised constraint")
	}
	for surface, w := range f.serve(t, f.implStageID) {
		if strings.Contains(w.Body.String(), planRevisionKey) {
			t.Errorf("%s: an implement serve carries %s:\n%s", surface, planRevisionKey, w.Body.String())
		}
	}
}
