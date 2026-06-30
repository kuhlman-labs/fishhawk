package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// runImplementReviewLoop is the count-form test entry retained for callers
// written against the pre-#955 signature (an agent count instead of resolved
// invocations — today TestImplementReviewLoop_RepublishesAuditCompleteWhenReviewLands
// in trace_test.go). It expands the bare count into default-reviewer
// invocations exactly as resolveReviewerInvocations does for the count form,
// then delegates to runImplementReviewInvocations.
func (s *Server) runImplementReviewLoop(ctx context.Context, runID, stageID uuid.UUID, agents int, authority planreview.AuthorityMode, promptText, authorModel string) bool {
	invocations := make([]reviewerInvocation, agents)
	for i := range invocations {
		invocations[i] = reviewerInvocation{reviewer: s.defaultPlanReviewer()}
	}
	return s.runImplementReviewInvocations(ctx, runID, stageID, invocations, authority, promptText, authorModel, "", "", s.cfg.ReviewBudget)
}

// Implement-stage workflow specs with reviewers config. The implement
// stage carries no constraints so the trace handler's policy re-eval
// passes and reaches the implement-review hook (ADR-027 impl 2/2).
var (
	specImplementGatingReviewers = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 0
`)

	specImplementAdvisoryReviewers = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 1
`)

	// specImplementGatingReviewersV1ReviewTimeout is a workflow-v1 spec whose
	// implement-stage gating reviewers block declares a per-stage review_timeout
	// (47s) — observably distinct from the deployment-default floor the #1494
	// e2e test pins.
	specImplementGatingReviewersV1ReviewTimeout = []byte(`version: "1.0"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 0
          review_timeout: 47s
`)
)

// newImplementReviewServer wires an orchestratorRepo seeded with a
// succeeded plan stage (carrying a plan artifact) and a dispatched
// implement stage requiring approval, plus the given reviewer and spec.
// Returns the server, signing fake, audit fake, run repo, and the
// implement stage so callers can assert its post-trace state.
func newImplementReviewServer(t *testing.T, reviewer PlanReviewer, workflowSpec []byte) (
	*Server, *signingFake, *auditFake, *orchestratorRepo, *run.Run, *run.Stage,
) {
	t.Helper()
	return newImplementReviewServerWithSet(t, singleReviewerSet{reviewer}, workflowSpec)
}

// newImplementReviewServerWithSet is the ReviewerSet-injecting variant of
// newImplementReviewServer for heterogeneous reviewers.agents tests (#955).
func newImplementReviewServerWithSet(t *testing.T, set ReviewerSet, workflowSpec []byte) (
	*Server, *signingFake, *auditFake, *orchestratorRepo, *run.Run, *run.Stage,
) {
	t.Helper()
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	ts := newTraceStoreFake()
	au := newAuditFake()

	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = workflowSpec
	runRow.Repo = "kuhlman-labs/example"

	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedBudgetPlanArtifact(t, art, planStage.ID, &plan.Plan{
		PlanVersion:                "standard_v1",
		Summary:                    "Add foo helper",
		PredictedRuntimeMinutes:    10,
		PredictedRuntimeConfidence: plan.RuntimeConfidenceMedium,
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
			},
		},
	})

	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	s := New(Config{
		Addr:          "127.0.0.1:0",
		SigningRepo:   sf,
		TraceStore:    ts,
		AuditRepo:     au,
		RunRepo:       rr,
		ArtifactRepo:  art,
		PlanReviewers: set,
	})
	return s, sf, au, rr, runRow, implStage
}

// implementDiffBundle builds a trace bundle carrying a git_diff event so
// bundle.ExtractDiff yields the given changed files.
func implementDiffBundle(t *testing.T, files []map[string]string) []byte {
	t.Helper()
	return makeTestBundle(t, files)
}

// implementDiffBundleWithPatch builds a trace bundle whose git_diff
// event carries both the name-status file list AND the unified-diff
// patch text, so the trace handler threads diff.Patch into the
// implement-review prompt (#585).
func implementDiffBundleWithPatch(t *testing.T, files []map[string]string, patch string) []byte {
	t.Helper()
	var raw bytes.Buffer
	manifest, _ := json.Marshal(map[string]any{"bundle_schema": "v1"})
	manifestLine, _ := json.Marshal(map[string]any{"seq": 1, "kind": "manifest", "data": json.RawMessage(manifest)})
	raw.Write(manifestLine)
	raw.WriteByte('\n')

	payload, _ := json.Marshal(map[string]any{
		"kind":      "name_status",
		"base_ref":  "origin/main",
		"files":     files,
		"num_files": len(files),
		"patch":     patch,
	})
	diffLine, _ := json.Marshal(map[string]any{
		"seq": 2, "kind": "git_diff", "data": json.RawMessage(payload),
	})
	raw.Write(diffLine)
	raw.WriteByte('\n')

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// implementDiffBundleWithScopeDrift builds a trace bundle carrying BOTH a
// git_diff event and a scope_drift policy_event, so bundle.ExtractScopeDrift
// yields the given undeclared paths alongside the scoped diff. Used by the
// cross-boundary test that proves the drift list reaches the reviewer prompt
// end-to-end (#695).
func implementDiffBundleWithScopeDrift(t *testing.T, files []map[string]string, drift []string) []byte {
	t.Helper()
	var raw bytes.Buffer
	manifest, _ := json.Marshal(map[string]any{"bundle_schema": "v1"})
	manifestLine, _ := json.Marshal(map[string]any{"seq": 1, "kind": "manifest", "data": json.RawMessage(manifest)})
	raw.Write(manifestLine)
	raw.WriteByte('\n')

	diffPayload, _ := json.Marshal(map[string]any{
		"kind":      "name_status",
		"base_ref":  "origin/main",
		"files":     files,
		"num_files": len(files),
	})
	diffLine, _ := json.Marshal(map[string]any{
		"seq": 2, "kind": "git_diff", "data": json.RawMessage(diffPayload),
	})
	raw.Write(diffLine)
	raw.WriteByte('\n')

	driftPayload, _ := json.Marshal(map[string]any{
		"check":      "scope_drift",
		"outcome":    "excluded",
		"undeclared": drift,
	})
	driftLine, _ := json.Marshal(map[string]any{
		"seq": 3, "kind": "policy_event", "data": json.RawMessage(driftPayload),
	})
	raw.Write(driftLine)
	raw.WriteByte('\n')

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// implementFixupBundleWithHeadSHA builds a push_fixup implement bundle (so the
// forward-gate keeps the stage in `running` across uploads, like a fix-up
// re-dispatch / a retried raw upload) carrying a git_diff AND a verify_run
// event with the given committed-tree head_sha. The head_sha is the #797
// implement-review dedup key: raw and redacted variants of one pack carry the
// same value, while a re-pack carries a new one.
func implementFixupBundleWithHeadSHA(t *testing.T, fileCount int, headSHA string) []byte {
	t.Helper()
	var raw bytes.Buffer
	manifest, _ := json.Marshal(bundle.Manifest{BundleSchema: "v1", PushFixup: true})
	manifestLine, _ := json.Marshal(map[string]any{"seq": 1, "kind": "manifest", "data": json.RawMessage(manifest)})
	raw.Write(manifestLine)
	raw.WriteByte('\n')

	files := make([]map[string]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		files = append(files, map[string]string{"path": fmt.Sprintf("file%d.go", i), "status": "modified"})
	}
	diffPayload, _ := json.Marshal(map[string]any{
		"kind": "name_status", "base_ref": "origin/main", "files": files, "num_files": fileCount,
	})
	diffLine, _ := json.Marshal(map[string]any{"seq": 2, "kind": "git_diff", "data": json.RawMessage(diffPayload)})
	raw.Write(diffLine)
	raw.WriteByte('\n')

	verifyPayload, _ := json.Marshal(map[string]any{
		"command": "go build ./...", "head_sha": headSHA, "exit_code": 0, "output": "", "outcome": "passed",
	})
	verifyLine, _ := json.Marshal(map[string]any{"seq": 3, "kind": "verify_run", "data": json.RawMessage(verifyPayload)})
	raw.Write(verifyLine)
	raw.WriteByte('\n')

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// implementDiffBundleWithGateEvidence builds a trace bundle carrying BOTH a
// git_diff event and a runner-shaped gate_evidence event (#963) whose
// verify_run digest FAILED with a [build failed] tail. Used by the
// cross-boundary test proving the gate evidence reaches the reviewer prompt
// end-to-end. The payload JSON here is the lockstep runner↔backend wire
// contract — field names must match the runner's composeGateEvidence.
func implementDiffBundleWithGateEvidence(t *testing.T, files []map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	manifest, _ := json.Marshal(map[string]any{"bundle_schema": "v1"})
	manifestLine, _ := json.Marshal(map[string]any{"seq": 1, "kind": "manifest", "data": json.RawMessage(manifest)})
	raw.Write(manifestLine)
	raw.WriteByte('\n')

	diffPayload, _ := json.Marshal(map[string]any{
		"kind":      "name_status",
		"base_ref":  "origin/main",
		"files":     files,
		"num_files": len(files),
	})
	diffLine, _ := json.Marshal(map[string]any{
		"seq": 2, "kind": "git_diff", "data": json.RawMessage(diffPayload),
	})
	raw.Write(diffLine)
	raw.WriteByte('\n')

	evidencePayload, _ := json.Marshal(map[string]any{
		"verify_runs": []map[string]any{
			{
				"command":     "scripts/test",
				"head_sha":    "abc123",
				"tree_sha":    "def456",
				"exit_code":   2,
				"outcome":     "failed",
				"output_tail": "FAIL\tgithub.com/kuhlman-labs/fishhawk/backend/internal/foo [build failed]",
			},
		},
		"verify_summary": map[string]any{
			"outcome": "failed", "iterations": 1, "max_iterations": 3,
		},
		"scope_facts": map[string]any{
			"declared_files": 2, "staged_files": 1,
			"undeclared_paths": []string{"backend/internal/foo/helper.go"},
			"undeclared_categorized": []map[string]any{
				{"path": "backend/internal/foo/helper.go", "category": "A", "disposition": "excluded_from_commit"},
			},
		},
	})
	evidenceLine, _ := json.Marshal(map[string]any{
		"seq": 3, "kind": "gate_evidence", "data": json.RawMessage(evidencePayload),
	})
	raw.Write(evidenceLine)
	raw.WriteByte('\n')

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// TestShipTrace_ImplementReview_GateEvidenceThreadedIntoPrompt is the
// cross-boundary integration test for #963: a real trace bundle carrying a
// gate_evidence event with a FAILING verify_run digest ships through
// handleShipTrace (the raw-variant implement-review hook), and the captured
// reviewer prompt renders the '### Gate evidence' section naming the
// [build failed] tail with the binding outrank guidance — proving the
// bundle-reader → trace-handler → prompt-render seam end-to-end (the run
// 07bce059 gap: a reviewer producing a text-level verdict about a head the
// gates already knew did not compile). Per-layer units miss this seam.
func TestShipTrace_ImplementReview_GateEvidenceThreadedIntoPrompt(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundleWithGateEvidence(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	for _, want := range []string{
		"### Gate evidence (machine-verified — outranks text-level findings)",
		"outcome: failed (exit code 2)",
		"[build failed]",
		"name it FIRST in `concerns`",
		"Verify summary: outcome=failed (iterations 1/3)",
		"- declared scope.files: 2",
		"- files staged into the commit: 1",
		// The per-path drift category (#991) crosses the wire-decode →
		// server-map → render seam intact.
		"- backend/internal/foo/helper.go (category A: agent edit to a tracked file EXCLUDED from the commit — " +
			"the pushed head may be missing a required change)",
		// The softened non-goals preamble defers to the evidence section.
		"Mechanical correctness is reported by the deterministic gates in the 'Gate evidence' section above",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q from threaded gate evidence:\n%s", want, got)
		}
	}
}

// TestShipTrace_ImplementReview_NoGateEvidence_PromptUnchanged asserts the
// fail-open contract (#963): a bundle WITHOUT a gate_evidence event (every
// pre-#963 bundle, every no-gate stage) dispatches the review with no Gate
// evidence section and the original non-goals preamble — absent evidence
// never blocks or alters the dispatch.
func TestShipTrace_ImplementReview_NoGateEvidence_PromptUnchanged(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	if strings.Contains(got, "### Gate evidence") {
		t.Errorf("no-evidence bundle must not render a Gate evidence section:\n%s", got)
	}
	if !strings.Contains(got, "Mechanical correctness is already gated upstream") {
		t.Errorf("no-evidence prompt must keep the original non-goals preamble:\n%s", got)
	}
}

// TestShipTrace_ImplementReview_ScopeDriftThreadedIntoPrompt is the
// cross-boundary integration test for #695: a real trace bundle carrying
// BOTH a git_diff event and a scope_drift policy_event ships through
// handleShipTrace, and the captured reviewer prompt names the drifted path
// with the operator-may-stage framing — proving the bundle-reader →
// trace-handler → prompt-render seam end-to-end, not just per-layer.
func TestShipTrace_ImplementReview_ScopeDriftThreadedIntoPrompt(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundleWithScopeDrift(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}},
		[]string{"backend/internal/foo/foo_test.go"})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	for _, want := range []string{
		"Scope drift (excluded from the diff above — operator may stage)",
		"backend/internal/foo/foo_test.go",
		"operator may stage",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q from threaded scope drift:\n%s", want, got)
		}
	}
}

// TestShipTrace_ImplementReview_AmendedScopeThreadedIntoPrompt is the
// cross-boundary integration test for #829: an operator-authorized scope
// amendment recorded at approval time — via the #824 structured
// add_scope_files fold AND the #730 approval-condition prose fold — must reach
// the implement-review prompt's "Scope amended at approval" section so the
// reviewer treats those paths as in-scope instead of flagging them as drift.
// runImplementReviews builds the review prompt directly from the raw plan
// scope, so this proves the approval-store -> trace-handler -> prompt-builder
// seam end-to-end: the resolvers are re-applied review-side, not just on the
// implement-stage prompt path (handleGetStagePrompt). Per-layer units miss
// this seam.
func TestShipTrace_ImplementReview_AmendedScopeThreadedIntoPrompt(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)

	// Seed the approval entries the resolvers read back: one structured
	// add_scope_files fold (#824) and one approval-condition comment naming a
	// path (#730). The raw plan scope is backend/internal/foo/foo.go (seeded by
	// newImplementReviewServer); neither amended path is in it.
	au.seeded = append(au.seeded,
		makeApproveWithScopeFilesEntry(runRow.ID, []string{"backend/cmd/fishhawk-mcp/README.md"}),
		makeApproveWithCommentEntry(runRow.ID, "Approved — also update docs/extra.md to reflect the change."),
	)

	priv, _ := sf.issue(t, runRow.ID)
	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	for _, want := range []string{
		"Scope amended at approval (operator-authorized — in-scope, NOT drift)",
		"backend/cmd/fishhawk-mcp/README.md", // #824 structured fold
		"docs/extra.md",                      // #730 prose fold
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q from threaded amended scope:\n%s", want, got)
		}
	}
	// The raw plan scope file must NOT appear in the amended-scope section — it
	// is already rendered by writePlanForReview. Assert it is not listed as a
	// bullet under the amended-scope header.
	amendedIdx := strings.Index(got, "### Scope amended at approval")
	if amendedIdx < 0 {
		t.Fatalf("amended-scope section header absent:\n%s", got)
	}
	nextSection := strings.Index(got[amendedIdx+1:], "\n### ")
	end := len(got)
	if nextSection >= 0 {
		end = amendedIdx + 1 + nextSection
	}
	if strings.Contains(got[amendedIdx:end], "- backend/internal/foo/foo.go") {
		t.Errorf("raw plan scope file must not be listed in amended-scope section:\n%s", got[amendedIdx:end])
	}
}

// TestShipTrace_ImplementReview_ApprovalConditionsThreadedIntoPrompt is the
// cross-boundary integration test for #1021: the operator's binding
// approve-with-conditions text (#558) recorded in the audit log must reach
// the implement-review prompt's "Approval conditions" section so the
// reviewer judges the diff against the amended plan instead of flagging a
// correctly implemented amendment as a plan deviation (runs 338d6b0f,
// 256032f6). It exercises the audit-store -> resolveApprovalConditions ->
// prompt.Trigger -> buildImplementReview seam end-to-end through a real raw
// trace bundle, the same harness the #829 amended-scope threading test uses.
func TestShipTrace_ImplementReview_ApprovalConditionsThreadedIntoPrompt(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)

	// Seed the approval_submitted entry resolveApprovalConditions reads back.
	const condition = "Approved with one amendment: validate the nonce server-side, not in the CLI."
	au.seeded = append(au.seeded, makeApproveWithCommentEntry(runRow.ID, condition))

	priv, _ := sf.issue(t, runRow.ID)
	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	for _, want := range []string{
		"### Approval conditions (binding — AMEND the plan, win on conflict)",
		"that is NOT a plan deviation",
		condition,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q from threaded approval conditions:\n%s", want, got)
		}
	}
}

// TestShipTrace_ImplementReview_NoApprovalConditions_SectionAbsent is the
// #1021 companion boundary: with no approval comment seeded, the review
// prompt must not render the Approval-conditions section — the
// no-conditions prompt stays byte-identical to today.
func TestShipTrace_ImplementReview_NoApprovalConditions_SectionAbsent(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	if got := reviewer.calls[0]; strings.Contains(got, "### Approval conditions") {
		t.Errorf("approval-conditions section should be absent when no approval comment is seeded:\n%s", got)
	}
}

// TestShipTrace_ImplementReview_PatchThreadedIntoPrompt asserts the
// git_diff event's patch text reaches the reviewer prompt end-to-end:
// the trace handler sets trig.DiffPatch from diff.Patch, and
// buildImplementReview renders the real hunks (#585).
func TestShipTrace_ImplementReview_PatchThreadedIntoPrompt(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	patch := "diff --git a/backend/internal/foo/foo.go b/backend/internal/foo/foo.go\n" +
		"@@ -1,2 +1,2 @@\n-old impl\n+new impl\n"
	bundleBytes := implementDiffBundleWithPatch(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	}, patch)
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	for _, want := range []string{"-old impl", "+new impl", "@@ -1,2 +1,2 @@", "both added and removed lines are visible above"} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q from threaded patch:\n%s", want, got)
		}
	}
}

// TestShipTrace_ImplementReview_GatingReject_StageFailedB asserts that
// under gating authority (agent>=1, human==0) a reject verdict fails the
// implement stage as category-B rather than advancing it to
// awaiting_approval.
func TestShipTrace_ImplementReview_GatingReject_StageFailedB(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	if implStage.State != run.StageStateFailed {
		t.Fatalf("implement stage state = %q, want failed", implStage.State)
	}
	if implStage.FailureCategory == nil || *implStage.FailureCategory != run.FailureB {
		t.Errorf("failure category = %v, want B", implStage.FailureCategory)
	}

	// One implement_reviewed audit entry with gating authority + reject.
	var found bool
	au.mu.Lock()
	for _, e := range au.appended {
		if e.Category == "implement_reviewed" {
			found = true
		}
	}
	au.mu.Unlock()
	if !found {
		t.Error("no implement_reviewed audit entry emitted")
	}

	// The reviewer received a non-empty diff in its prompt.
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	if !strings.Contains(reviewer.calls[0], "backend/internal/foo/foo.go") {
		t.Errorf("reviewer prompt missing the diff's changed file:\n%s", reviewer.calls[0])
	}
}

// TestShipTrace_ImplementReview_ReviewerError_EmitsImplementReviewFailed is
// the #664 symmetric producer-contract test for the implement stage: a
// reviewer that errors (modelling a timeout) writes exactly one terminal
// implement_review_failed audit entry carrying the error string, and zero
// implement_reviewed entries. It also pins that gating advance semantics are
// untouched (#574): an erroring gating reviewer does not fail the stage.
func TestShipTrace_ImplementReview_ReviewerError_EmitsImplementReviewFailed(t *testing.T) {
	reviewer := &fakePlanReviewer{
		err: fmt.Errorf("review timed out: context deadline exceeded"),
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// Exactly one implement_review_failed entry; zero implement_reviewed.
	var failedEntries []planreview.ReviewFailedPayload
	au.mu.Lock()
	for _, e := range au.appended {
		switch e.Category {
		case "implement_review_failed":
			var p planreview.ReviewFailedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				au.mu.Unlock()
				t.Fatalf("decode implement_review_failed payload: %v", err)
			}
			failedEntries = append(failedEntries, p)
		case "implement_reviewed":
			t.Errorf("unexpected implement_reviewed entry on the reviewer-error path")
		}
	}
	au.mu.Unlock()
	if len(failedEntries) != 1 {
		t.Fatalf("implement_review_failed entries = %d, want 1", len(failedEntries))
	}
	if failedEntries[0].Reason != "review timed out: context deadline exceeded" {
		t.Errorf("reason = %q, want the reviewer error string", failedEntries[0].Reason)
	}
	if failedEntries[0].Authority != planreview.AuthorityGating {
		t.Errorf("authority = %q, want gating", failedEntries[0].Authority)
	}
	// #747: a fast, non-deadline error is not a timeout-kill.
	if failedEntries[0].Timeout {
		t.Errorf("timeout = true, want false for a non-deadline reviewer error")
	}

	// #574: an erroring gating reviewer must NOT fail the stage.
	if implStage.State == run.StageStateFailed {
		t.Errorf("reviewer error must not fail the gating implement stage; state = %q", implStage.State)
	}
}

// TestShipTrace_ImplementReview_BudgetTimeout_EmitsTimeoutTrue is the #747
// server-level seam test for the implement stage: a reviewer that blocks until
// its invocation deadline fires, run under a tiny review budget, must produce
// exactly one implement_review_failed entry carrying Timeout=true. Exercises
// budget computation + deadline application + audit emit together (cf. #618).
func TestShipTrace_ImplementReview_BudgetTimeout_EmitsTimeoutTrue(t *testing.T) {
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, deadlineWaitingReviewer{}, specImplementGatingReviewers)
	s.cfg.ReviewBudget = planreview.ReviewBudget{Floor: 20 * time.Millisecond}
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	var failedEntries []planreview.ReviewFailedPayload
	au.mu.Lock()
	for _, e := range au.appended {
		switch e.Category {
		case "implement_review_failed":
			var p planreview.ReviewFailedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				au.mu.Unlock()
				t.Fatalf("decode implement_review_failed payload: %v", err)
			}
			failedEntries = append(failedEntries, p)
		case "implement_reviewed":
			t.Errorf("unexpected implement_reviewed entry on the budget-timeout path")
		}
	}
	au.mu.Unlock()
	if len(failedEntries) != 1 {
		t.Fatalf("implement_review_failed entries = %d, want 1", len(failedEntries))
	}
	if !failedEntries[0].Timeout {
		t.Errorf("timeout = false, want true for a budget-deadline kill")
	}
}

// TestShipTrace_ImplementReview_StageReviewTimeoutOverridesDefault is the
// #1494 cross-boundary seam test for the implement stage, mirroring the
// plan-stage arm: a spec carrying reviewers.review_timeout drives the
// review-wait budget FLOOR off that spec value (47s) rather than the
// FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment default (11s). It crosses
// schema-decode -> ResolveReviewTimeout -> planreview.Budget at the real
// gating implement-review dispatch site; PerKB/Cap are zeroed so the applied
// budget equals the resolved Floor, read off the reviewer's invocation deadline.
func TestShipTrace_ImplementReview_StageReviewTimeoutOverridesDefault(t *testing.T) {
	rev := &budgetCapturingReviewer{}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, rev, specImplementGatingReviewersV1ReviewTimeout)
	s.cfg.ReviewBudget = planreview.ReviewBudget{Floor: 11 * time.Second}
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	rev.mu.Lock()
	budget, hadDeadline := rev.budget, rev.hadDeadline
	rev.mu.Unlock()
	if !hadDeadline {
		t.Fatal("reviewer invocation carried no deadline; budget was not applied")
	}
	if budget <= 45*time.Second || budget > 47*time.Second {
		t.Errorf("review budget = %v, want ~47s (implement stage review_timeout wins over the 11s deployment default)", budget)
	}
}

// TestShipTrace_ImplementReview_NoReviewTimeoutUsesDefault is the converse
// #1494 implement-stage seam test: absent reviewers.review_timeout, the budget
// FLOOR falls back to the FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment default. It
// reuses the v0.3 gating spec (no review_timeout) so the fallback is exercised.
func TestShipTrace_ImplementReview_NoReviewTimeoutUsesDefault(t *testing.T) {
	rev := &budgetCapturingReviewer{}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, rev, specImplementGatingReviewers)
	s.cfg.ReviewBudget = planreview.ReviewBudget{Floor: 11 * time.Second}
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	rev.mu.Lock()
	budget, hadDeadline := rev.budget, rev.hadDeadline
	rev.mu.Unlock()
	if !hadDeadline {
		t.Fatal("reviewer invocation carried no deadline; budget was not applied")
	}
	if budget <= 9*time.Second || budget > 11*time.Second {
		t.Errorf("review budget = %v, want ~11s (deployment default floor when review_timeout is absent)", budget)
	}
}

// TestShipTrace_ImplementReview_GatingApprove_Advances asserts that under
// gating authority an approve verdict lets the stage advance to its
// terminal state (awaiting_approval, since the stage requires approval).
func TestShipTrace_ImplementReview_GatingApprove_Advances(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	if implStage.State != run.StageStateAwaitingApproval {
		t.Errorf("implement stage state = %q, want awaiting_approval", implStage.State)
	}
}

// TestShipTrace_ImplementReview_FixupForwardGated_StillReviews is CONDITION 3
// of #794: the advisory implement RE-REVIEW must still fire at trace time for a
// forward-gated fix-up stage. The fix-up bundle stamps push_fixup AND carries a
// non-empty diff, so the trace handler defers the TERMINAL transition (the stage
// stays `running` until the /pull-request fixup_pushed report) — but the
// re-review runs on the bundle diff while the stage stays running. A regression
// that silently stopped the fix-up re-review from firing must fail here.
func TestShipTrace_ImplementReview_FixupForwardGated_StillReviews(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, rr, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	bundleBytes := makeFixupPushBundle(t, true, 2, t0, t1)
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// Forward-gated: the terminal transition is deferred to the /pull-request
	// report, so the stage stays running (NOT awaiting_approval).
	got, err := rr.GetStage(t.Context(), implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateRunning {
		t.Errorf("fix-up stage state = %q, want %q (terminal transition deferred)",
			got.State, run.StageStateRunning)
	}

	// Re-review still fired at trace time despite the gate. Advisory reviews
	// run detached (#584); drain before asserting on the audit entry.
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed entries = %d, want 1 (fix-up re-review must fire even when the terminal transition is forward-gated)", n)
	}
}

// TestShipTrace_ImplementReview_DoubleVariantUpload_DispatchesOnce is the
// #793 regression: the runner POSTs BOTH the raw and the redacted variant of
// the same bundle, and a forward-gated implement stage (push_fixup here) stays
// in `running` across both uploads, so advanceStageAfterTrace re-enters the
// implement-review block on the redacted upload too. Before the variant gate
// this dispatched a SECOND review (2x cost, divergent verdicts). With the
// raw-variant gate (mirroring the recordCost #678 gate) the redacted upload is
// a no-op: exactly one implement_review_started and one implement_reviewed for
// the bundle. Fails on main with two of each.
func TestShipTrace_ImplementReview_DoubleVariantUpload_DispatchesOnce(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	// One bundle, two variants. The runner uploads raw first, then redacted,
	// as two separate signed requests over the same stage_id.
	bundleBytes := makeFixupPushBundle(t, true, 2, t0, t1)
	for _, variant := range []string{"raw", "redacted"} {
		w := shipRequest(t, s, runRow.ID, implStage.ID, variant, priv, bundleBytes, "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s upload status = %d, want 202:\n%s", variant, w.Code, w.Body.String())
		}
	}

	// Advisory reviews run detached (#584); drain before asserting.
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_review_started"); n != 1 {
		t.Errorf("implement_review_started entries = %d, want 1 (redacted variant must not re-dispatch)", n)
	}
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed entries = %d, want 1 (redacted variant must not re-dispatch)", n)
	}
}

// TestShipTrace_ImplementReview_FixupRedispatch_StillReviews is CONDITION 3 of
// the #793 plan and the guard against a stage_id-only dedup regression. A
// fix-up re-dispatch (#788/#794) re-opens the SAME stage_id with a NEW diff and
// re-uploads its own raw variant — it is a separate upload cycle, not the
// redacted half of the first. The raw-variant gate must NOT suppress it: each
// cycle's raw upload fires its own single review on its own diff. A guard keyed
// on stage_id alone would find the first cycle's implement_review_started and
// skip the fix-up's re-review entirely — this test fails in that case.
func TestShipTrace_ImplementReview_FixupRedispatch_StillReviews(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	// Two separate upload cycles for the SAME stage_id. The forward-gate keeps
	// the stage in `running` across both, mirroring a fix-up re-dispatch onto
	// the existing PR branch. Each cycle carries a DISTINCT verify_run head_sha
	// (a re-pack runs a new committed-tree verify → new commit SHA), proving the
	// (stage_id, head_sha) key discriminates by SHA — not merely by the
	// empty-head_sha bypass.
	for i, headSHA := range []string{"fixupsha-cycle-1", "fixupsha-cycle-2"} {
		bundleBytes := implementFixupBundleWithHeadSHA(t, 2+i, headSHA)
		w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("fix-up cycle (head_sha %s) status = %d, want 202:\n%s", headSHA, w.Code, w.Body.String())
		}
	}

	// Advisory reviews run detached (#584); drain before asserting.
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_review_started"); n != 2 {
		t.Errorf("implement_review_started entries = %d, want 2 (fix-up re-dispatch must get its own review)", n)
	}
	if n := countAuditCategory(au, "implement_reviewed"); n != 2 {
		t.Errorf("implement_reviewed entries = %d, want 2 (fix-up re-dispatch must get its own review)", n)
	}
}

// TestShipTrace_ImplementReview_RetriedRawUpload_DedupsBySHA is the #797
// regression: the raw-variant gate (#793) dedups the raw+redacted pair of one
// pack, but NOT a RETRIED raw upload — a transient 5xx after the review already
// dispatched makes the runner re-POST the same raw bundle, which under the
// variant-only gate dispatched a SECOND review (divergent verdicts, #777 hint
// over-fire). The (stage_id, head_sha) idempotency guard suppresses the retry:
// the same head_sha already has an implement_review_started entry, so the
// second raw upload is a no-op. This drives the real extract → emit → guard
// seam end-to-end (advanceStageAfterTrace → bundle.ExtractHeadSHA →
// runImplementReviews → emitReviewStarted write → implementReviewAlreadyStarted
// read) — a per-layer unit would pass while this producer/consumer seam breaks
// (cf. #618). Fails before #797 with two of each.
func TestShipTrace_ImplementReview_RetriedRawUpload_DedupsBySHA(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	// One pack with a fixed verify_run head_sha, POSTed raw TWICE — a
	// transient-5xx retry re-uploads the identical raw bundle.
	bundleBytes := implementFixupBundleWithHeadSHA(t, 2, "retried-raw-sha")
	for i := 0; i < 2; i++ {
		w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("raw upload %d status = %d, want 202:\n%s", i, w.Code, w.Body.String())
		}
	}

	// Advisory reviews run detached (#584); drain before asserting.
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_review_started"); n != 1 {
		t.Errorf("implement_review_started entries = %d, want 1 (retried raw upload must dedup on head_sha)", n)
	}
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed entries = %d, want 1 (retried raw upload must dedup on head_sha)", n)
	}
}

// TestShipTrace_ImplementReview_ScopeDriftOnly_Advances asserts the
// flag-only contract (ADR-027 Decision Q6): a reviewer returning
// approve_with_concerns with a single {category:"scope"} concern under
// gating authority does NOT fail the stage — drift alone never blocks.
func TestShipTrace_ImplementReview_ScopeDriftOnly_Advances(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApproveWithConcerns,
			Concerns: []planreview.Concern{
				{Severity: planreview.SeverityLow, Category: "scope", Note: "touched a file outside scope.files"},
			},
		},
		model: "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
		{"path": "backend/internal/other/other.go", "status": "A"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	if implStage.State == run.StageStateFailed {
		t.Fatalf("scope drift alone must not fail the stage; state = %q", implStage.State)
	}
	if implStage.State != run.StageStateAwaitingApproval {
		t.Errorf("implement stage state = %q, want awaiting_approval", implStage.State)
	}
}

// TestShipTrace_ImplementReview_Advisory_RecordsVerdictRequiresApproval
// asserts that under advisory authority (agent>=1, human>=1) even a reject
// verdict is recorded as implement_reviewed but does NOT block — the human
// gate stays authoritative and the stage advances to awaiting_approval.
func TestShipTrace_ImplementReview_Advisory_RecordsVerdictRequiresApproval(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	if implStage.State != run.StageStateAwaitingApproval {
		t.Errorf("advisory: implement stage state = %q, want awaiting_approval", implStage.State)
	}
	// Advisory review runs detached (#584); drain it before asserting on
	// the audit entry it writes.
	s.waitBackgroundReviews()

	// Verdict still recorded with advisory authority.
	var rec *planreview.ImplementReviewedPayload
	au.mu.Lock()
	for i := range au.appended {
		if au.appended[i].Category == "implement_reviewed" {
			var p planreview.ImplementReviewedPayload
			if err := json.Unmarshal(au.appended[i].Payload, &p); err == nil {
				rec = &p
			}
		}
	}
	au.mu.Unlock()
	if rec == nil {
		t.Fatal("no implement_reviewed audit entry emitted under advisory authority")
	}
	if rec.Authority != planreview.AuthorityAdvisory {
		t.Errorf("authority = %q, want advisory", rec.Authority)
	}
	if rec.Verdict != planreview.VerdictReject {
		t.Errorf("verdict = %q, want reject (recorded but non-blocking)", rec.Verdict)
	}
}

// TestShipTrace_ImplementReview_Advisory_RunsAsync asserts the #584
// decoupling for the implement path: under advisory authority the trace
// upload returns 202 AND the stage advances to its terminal
// awaiting_approval state BEFORE the (blocked) reviewer finishes. Once
// released and drained, the implement_reviewed audit entry lands. The
// human gate stays authoritative, so the advisory verdict arriving after
// advancement is correct.
func TestShipTrace_ImplementReview_Advisory_RunsAsync(t *testing.T) {
	reviewer := newBlockingPlanReviewer(
		&planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		"claude-sonnet-4-6",
	)
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// Terminal transition happened synchronously despite the blocked
	// reviewer — the review is detached.
	if implStage.State != run.StageStateAwaitingApproval {
		t.Errorf("implement stage state = %q, want awaiting_approval (advanced before review)", implStage.State)
	}

	// The async review cannot have recorded yet (release not closed).
	if n := countAuditCategory(au, "implement_reviewed"); n != 0 {
		t.Fatalf("implement_reviewed entries = %d before release, want 0 (review was not async)", n)
	}

	close(reviewer.release)
	s.waitBackgroundReviews()

	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed entries = %d after release, want 1", n)
	}
}

// TestShipTrace_ImplementReview_Started_PrecedesReviewed asserts the #600
// ordering invariant for the implement path: the implement_review_started
// audit entry is appended BEFORE the terminal implement_reviewed entry under
// both gating (synchronous) and advisory (detached goroutine) authority. The
// MCP review_status proxy reads started as 'pending' and reviewed as the
// terminal state; a started landing after reviewed would misreport an
// already-complete review as pending.
func TestShipTrace_ImplementReview_Started_PrecedesReviewed(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec []byte
	}{
		{"gating", specImplementGatingReviewers},
		{"advisory", specImplementAdvisoryReviewers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &fakePlanReviewer{
				verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
				model:   "claude-opus-4-7",
			}
			s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, tc.spec)
			priv, _ := sf.issue(t, runRow.ID)

			bundleBytes := implementDiffBundle(t, []map[string]string{
				{"path": "backend/internal/foo/foo.go", "status": "M"},
			})
			w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
			}
			// Advisory dispatches detached (#584); drain before asserting.
			s.waitBackgroundReviews()

			au.mu.Lock()
			defer au.mu.Unlock()
			startedIdx, reviewedIdx := -1, -1
			for i, e := range au.appended {
				switch e.Category {
				case "implement_review_started":
					if startedIdx == -1 {
						startedIdx = i
					}
				case "implement_reviewed":
					if reviewedIdx == -1 {
						reviewedIdx = i
					}
				}
			}
			if startedIdx == -1 {
				t.Fatal("no implement_review_started audit entry emitted")
			}
			if reviewedIdx == -1 {
				t.Fatal("no implement_reviewed audit entry emitted")
			}
			if startedIdx >= reviewedIdx {
				t.Errorf("implement_review_started index %d must precede implement_reviewed index %d", startedIdx, reviewedIdx)
			}
			var p planreview.ReviewStartedPayload
			if err := json.Unmarshal(au.appended[startedIdx].Payload, &p); err != nil {
				t.Fatalf("decode implement_review_started payload: %v", err)
			}
			if p.ConfiguredAgents != 1 {
				t.Errorf("configured_agents = %d, want 1", p.ConfiguredAgents)
			}
		})
	}
}

// TestShipTrace_ImplementReview_Advisory_ContextDetached asserts the
// detached implement review survives cancellation of the context passed
// into runImplementReviews (simulating the upload client disconnect
// cancelling r.Context() mid-review). The verdict must still record, which
// it only does if the goroutine runs on a context.WithoutCancel'd context.
func TestShipTrace_ImplementReview_Advisory_ContextDetached(t *testing.T) {
	reviewer := newBlockingPlanReviewer(
		&planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		"claude-sonnet-4-6",
	)
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)

	diff := policy.Diff{
		ChangedFiles: []policy.ChangedFile{
			{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Advisory → dispatches a detached goroutine and returns false.
	if s.runImplementReviews(ctx, runRow.ID, implStage.ID, diff, nil, "", nil) {
		t.Fatal("advisory runImplementReviews returned true (advisory must never gate)")
	}

	<-reviewer.started
	cancel()
	close(reviewer.release)

	s.waitBackgroundReviews()

	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed entries = %d, want 1 — detached review must survive parent-context cancel", n)
	}
}

// specImplementHeterogeneousGating declares the #955 heterogeneous agents
// list on the implement stage (human 0 → gating, so the review loop runs
// synchronously inside the trace upload).
var specImplementHeterogeneousGating = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-4-8
            - provider: codex
              model: gpt-5.5
          human: 0
`)

// TestShipTrace_ImplementReview_Heterogeneous_CrossBoundary is the #955
// cross-boundary integration test for the implement loop: a real workflow
// spec declaring two heterogeneous agent reviewers drives the trace-upload
// path end-to-end and produces exactly two implement_reviewed entries with
// the two distinct ReviewerModel values, two reviewer-cost recordings, and
// a started proxy reporting the effective count len(agents)==2.
func TestShipTrace_ImplementReview_Heterogeneous_CrossBoundary(t *testing.T) {
	anthropicFake := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-8",
	}
	codexFake := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "gpt-5.5",
	}
	set := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": anthropicFake,
		"codex":     codexFake,
	}, def: anthropicFake}
	s, sf, au, _, runRow, implStage := newImplementReviewServerWithSet(t, set, specImplementHeterogeneousGating)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	anthropicFake.mu.Lock()
	codexFake.mu.Lock()
	if len(anthropicFake.calls) != 1 || len(codexFake.calls) != 1 {
		t.Errorf("adapter calls = anthropic:%d codex:%d, want 1 each",
			len(anthropicFake.calls), len(codexFake.calls))
	}
	codexFake.mu.Unlock()
	anthropicFake.mu.Unlock()

	var reviewed []planreview.ImplementReviewedPayload
	costs := 0
	for _, e := range au.appended {
		switch e.Category {
		case "implement_reviewed":
			var p planreview.ImplementReviewedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode implement_reviewed payload: %v", err)
			}
			reviewed = append(reviewed, p)
		case "cost_recorded":
			if strings.Contains(string(e.Payload), `"source":"implement_review"`) {
				costs++
			}
		case "implement_review_started":
			var p planreview.ReviewStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode implement_review_started payload: %v", err)
			}
			if p.ConfiguredAgents != 2 {
				t.Errorf("started configured_agents = %d, want 2 (len(agents))", p.ConfiguredAgents)
			}
		}
	}
	if len(reviewed) != 2 {
		t.Fatalf("implement_reviewed entries = %d, want 2", len(reviewed))
	}
	models := map[string]bool{}
	for _, p := range reviewed {
		models[p.ReviewerModel] = true
		if p.Authority != planreview.AuthorityGating {
			t.Errorf("authority = %q, want gating (agents list, human 0)", p.Authority)
		}
	}
	if !models["claude-opus-4-8"] || !models["gpt-5.5"] {
		t.Errorf("reviewer models = %v, want both claude-opus-4-8 and gpt-5.5", models)
	}
	if costs != 2 {
		t.Errorf("implement_review cost_recorded entries = %d, want 2", costs)
	}
}

// reviewModelResolvedEntry builds a review-stage model_resolved audit entry
// (#1416) carrying the operator's gate-resolved review model, seeded into the
// auditFake so gateResolvedReviewModel reads it back at implement-review time.
func reviewModelResolvedEntry(runID uuid.UUID, model string) *audit.Entry {
	payload, _ := json.Marshal(modelResolvedPayload{
		ResolvedModel: ResolvedModel{Value: model, Source: ModelSourceOperator},
		StageType:     string(run.StageTypeReview),
	})
	return &audit.Entry{RunID: &runID, Category: CategoryModelResolved, Payload: payload}
}

// TestRunImplementReviews_ReviewModelOverride_Threaded is the #1426
// cross-boundary behavioral test: it drives the REAL production
// runImplementReviews entrypoint and asserts that the gate-resolved review_model
// override actually reaches the reviewer-adapter lookup. The gap #1426 closes is
// precisely the seam between the gate-resolved audit value (gateResolvedReviewModel)
// and the reviewer invocation (resolveReviewerInvocationsWithReviewModel) — a per-
// layer unit passes on each side while the seam stays unwired (cf. #618). Two
// named branches: (1) override present — a seeded review model_resolved entry
// makes the capturingReviewerSet resolve EVERY heterogeneous reviewer under the
// operator override model; (2) override absent (fail-open) — no review
// model_resolved entry leaves each reviewer on its spec-declared model, byte-
// identical to today.
func TestRunImplementReviews_ReviewModelOverride_Threaded(t *testing.T) {
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{
		{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified},
	}}

	t.Run("override present — every reviewer resolved under the operator model", func(t *testing.T) {
		set := &capturingReviewerSet{reviewer: &fakePlanReviewer{
			verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
			model:   "operator-review-model",
		}}
		s, _, au, _, runRow, implStage := newImplementReviewServerWithSet(t, set, specImplementHeterogeneousGating)
		// The plan gate recorded the operator's review_model for the review stage.
		au.seeded = append(au.seeded, reviewModelResolvedEntry(runRow.ID, "operator-review-model"))

		s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, "", nil)

		if len(set.calls) != 2 {
			t.Fatalf("For called %d times, want 2 (one per heterogeneous reviewer)", len(set.calls))
		}
		for i, c := range set.calls {
			if c.model != "operator-review-model" {
				t.Errorf("For call %d resolved model %q, want operator-review-model (override threaded through the audit-read → resolve → lookup seam)", i, c.model)
			}
		}
	})

	t.Run("override absent (fail-open) — every reviewer resolved under its spec model", func(t *testing.T) {
		set := &capturingReviewerSet{reviewer: &fakePlanReviewer{
			verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		}}
		s, _, _, _, runRow, implStage := newImplementReviewServerWithSet(t, set, specImplementHeterogeneousGating)
		// No review model_resolved entry seeded → gateResolvedReviewModel returns
		// "" → the spawn stays byte-identical to today.

		s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, "", nil)

		if len(set.calls) != 2 {
			t.Fatalf("For called %d times, want 2 (one per heterogeneous reviewer)", len(set.calls))
		}
		gotModels := map[string]bool{set.calls[0].model: true, set.calls[1].model: true}
		if !gotModels["claude-opus-4-8"] || !gotModels["gpt-5.5"] {
			t.Errorf("For models = %v, want the spec models {claude-opus-4-8, gpt-5.5} (fail-open: no override leaves the spec model byte-identical)", gotModels)
		}
	})
}

// TestShipTrace_ImplementReview_Heterogeneous_UnresolvableProvider_Gating
// pins the implement-loop analog of the plan-side gating degradation test:
// one of two declared gating reviewers is unconfigured (config drift on an
// in-flight run — fresh gating runs are blocked at dispatch by the runs.go
// pre-check). The resolve failure emits exactly one implement_review_failed
// entry, leaves hasRejection untouched, and the resolvable reviewer's
// approve verdict still governs: the stage must not fail.
func TestShipTrace_ImplementReview_Heterogeneous_UnresolvableProvider_Gating(t *testing.T) {
	anthropicFake := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-8",
	}
	// codex deliberately absent from the set → For("codex") errors.
	set := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": anthropicFake,
	}, def: anthropicFake}
	s, sf, au, _, runRow, implStage := newImplementReviewServerWithSet(t, set, specImplementHeterogeneousGating)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	var reviewed []planreview.ImplementReviewedPayload
	var failedEntries []planreview.ReviewFailedPayload
	au.mu.Lock()
	for _, e := range au.appended {
		switch e.Category {
		case "implement_reviewed":
			var p planreview.ImplementReviewedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				au.mu.Unlock()
				t.Fatalf("decode implement_reviewed payload: %v", err)
			}
			reviewed = append(reviewed, p)
		case "implement_review_failed":
			var p planreview.ReviewFailedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				au.mu.Unlock()
				t.Fatalf("decode implement_review_failed payload: %v", err)
			}
			failedEntries = append(failedEntries, p)
		}
	}
	au.mu.Unlock()

	if len(reviewed) != 1 {
		t.Fatalf("implement_reviewed entries = %d, want 1 (the resolvable anthropic reviewer)", len(reviewed))
	}
	if reviewed[0].ReviewerModel != "claude-opus-4-8" || reviewed[0].Authority != planreview.AuthorityGating {
		t.Errorf("reviewed entry = model %q authority %q, want claude-opus-4-8 / gating",
			reviewed[0].ReviewerModel, reviewed[0].Authority)
	}
	if len(failedEntries) != 1 {
		t.Fatalf("implement_review_failed entries = %d, want 1 (the unresolvable codex reviewer)", len(failedEntries))
	}
	if !strings.Contains(failedEntries[0].Reason, "not configured") {
		t.Errorf("failed reason = %q, want the resolve error", failedEntries[0].Reason)
	}
	if failedEntries[0].Authority != planreview.AuthorityGating {
		t.Errorf("failed authority = %q, want gating", failedEntries[0].Authority)
	}

	// A resolve failure must not count as a rejection: the resolvable
	// reviewer approved, so the gating implement stage must not fail.
	if implStage.State == run.StageStateFailed {
		t.Errorf("gating resolve failure must not fail the stage; state = %q", implStage.State)
	}
}

// TestImplementReviewLoop_PersistsConcernsWithOriginSequence is the #964
// insert-AFTER-append contract test: two implement_reviewed entries from
// different reviewer models append, and each verdict's concerns persist
// in the durable store with origin_review_sequence equal to THAT entry's
// returned audit sequence, state raised, stage_kind implement.
func TestImplementReviewLoop_PersistsConcernsWithOriginSequence(t *testing.T) {
	au := newSeqAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()

	rev1 := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApproveWithConcerns,
			Concerns: []planreview.Concern{
				{Severity: planreview.SeverityHigh, Category: "correctness", Note: "first-a"},
				{Severity: planreview.SeverityLow, Category: "style", Note: "first-b"},
			},
		},
		model: "claude-opus-4-8",
	}
	rev2 := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:  planreview.VerdictApproveWithConcerns,
			Concerns: []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "scope", Note: "second-a"}},
		},
		model: "gpt-5.5",
	}

	s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: rev1}, {reviewer: rev2}},
		planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget)

	reviewed := au.entriesByCategory("implement_reviewed")
	if len(reviewed) != 2 {
		t.Fatalf("implement_reviewed entries = %d, want 2", len(reviewed))
	}

	rows, err := cr.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("persisted concerns = %d, want 3", len(rows))
	}
	for i, row := range rows {
		if row.StageID != stageID {
			t.Errorf("row %d StageID = %s, want %s", i, row.StageID, stageID)
		}
		if row.StageKind != concern.StageKindImplement {
			t.Errorf("row %d StageKind = %q, want implement", i, row.StageKind)
		}
		if row.State != concern.StateRaised {
			t.Errorf("row %d State = %q, want raised", i, row.State)
		}
	}
	// The first verdict's two concerns carry the FIRST entry's returned
	// sequence + model; the second verdict's concern carries the SECOND's.
	for i := 0; i < 2; i++ {
		if rows[i].OriginReviewSequence != reviewed[0].Sequence {
			t.Errorf("row %d sequence = %d, want first entry's %d", i, rows[i].OriginReviewSequence, reviewed[0].Sequence)
		}
		if rows[i].ReviewerModel == nil || *rows[i].ReviewerModel != "claude-opus-4-8" {
			t.Errorf("row %d ReviewerModel = %v, want claude-opus-4-8", i, rows[i].ReviewerModel)
		}
	}
	if rows[2].OriginReviewSequence != reviewed[1].Sequence {
		t.Errorf("row 2 sequence = %d, want second entry's %d", rows[2].OriginReviewSequence, reviewed[1].Sequence)
	}
	if rows[2].ReviewerModel == nil || *rows[2].ReviewerModel != "gpt-5.5" {
		t.Errorf("row 2 ReviewerModel = %v, want gpt-5.5", rows[2].ReviewerModel)
	}
	if reviewed[0].Sequence == reviewed[1].Sequence {
		t.Error("the two entries must carry distinct sequences")
	}
}

// TestImplementReviewLoop_FailedAppendSkipsConcernPersistence: when the
// audit append fails there is no sequence to stamp, so concern
// persistence for that verdict is skipped (warn-only) — the audit chain
// stays the sole sequence authority.
func TestImplementReviewLoop_FailedAppendSkipsConcernPersistence(t *testing.T) {
	au := newAuditFake()
	au.appendErr = errors.New("db down")
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()

	rev := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:  planreview.VerdictApproveWithConcerns,
			Concerns: []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "scope", Note: "n"}},
		},
		model: "claude-opus-4-8",
	}
	s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: rev}}, planreview.AuthorityAdvisory, "prompt", "author", "", "", planreview.DefaultReviewBudget)

	rows, _ := cr.ListByRun(context.Background(), runID)
	if len(rows) != 0 {
		t.Errorf("persisted concerns = %d, want 0 when the append failed", len(rows))
	}
}

// TestImplementReviewLoop_ConcernInsertFailureDoesNotFailLoop: a concern
// store failure warn-logs and never affects the loop's verdict handling —
// the audit payload remains authoritative.
func TestImplementReviewLoop_ConcernInsertFailureDoesNotFailLoop(t *testing.T) {
	au := newSeqAuditFake()
	cr := newFakeConcernRepo()
	cr.insertErr = errors.New("store down")
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()

	rev := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:  planreview.VerdictReject,
			Concerns: []planreview.Concern{{Severity: planreview.SeverityHigh, Category: "correctness", Note: "n"}},
		},
		model: "claude-opus-4-8",
	}
	hasRejection := s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: rev}}, planreview.AuthorityAdvisory, "prompt", "author", "", "", planreview.DefaultReviewBudget)

	if !hasRejection {
		t.Error("hasRejection = false, want true (insert failure must not mask the verdict)")
	}
	if len(au.entriesByCategory("implement_reviewed")) != 1 {
		t.Error("the implement_reviewed entry must still have appended")
	}
}

// ---------------------------------------------------------------------------
// Delta-verifying re-reviews + resolution processing (#984)
// ---------------------------------------------------------------------------

// TestImplementReviewLoop_ConfirmedResolutionTransitionsToAddressed is the
// #984 wire → decode → process → store seam: a reviewer verdict carrying a
// confirmed concern_resolutions entry transitions the seeded
// addressed_pending row to addressed, and the resolutions are recorded on
// the authoritative implement_reviewed audit payload.
func TestImplementReviewLoop_ConfirmedResolutionTransitionsToAddressed(t *testing.T) {
	au := newSeqAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()

	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "unhandled error path")
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed"); err != nil {
		t.Fatalf("seed addressed_pending: %v", err)
	}

	rev := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApprove,
			ConcernResolutions: []planreview.ConcernResolution{
				{ID: row.ID.String(), Resolution: "confirmed", Note: "the fixup handles the error"},
			},
		},
		model: "claude-opus-4-8",
	}
	s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: rev}}, planreview.AuthorityAdvisory, "prompt", "author", "", "", planreview.DefaultReviewBudget)

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateAddressed {
		t.Errorf("state = %q, want addressed", rows[0].State)
	}
	if rows[0].StateReason != "the fixup handles the error" {
		t.Errorf("state_reason = %q, want the resolution note", rows[0].StateReason)
	}

	reviewed := au.entriesByCategory("implement_reviewed")
	if len(reviewed) != 1 {
		t.Fatalf("implement_reviewed entries = %d, want 1", len(reviewed))
	}
	var p planreview.ImplementReviewedPayload
	if err := json.Unmarshal(reviewed[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(p.ConcernResolutions) != 1 || p.ConcernResolutions[0].ID != row.ID.String() ||
		p.ConcernResolutions[0].Resolution != "confirmed" {
		t.Errorf("payload resolutions = %+v, want the confirmed entry recorded authoritatively", p.ConcernResolutions)
	}
}

// TestImplementReviewLoop_ReopenWinsBothOrders pins #984's REOPEN WINS
// contract across heterogeneous reviewers in BOTH arrival orders: a
// confirm landing before the reopen is overridden (addressed → reopened
// is a valid edge), and a confirm landing after the reopen is rejected
// by the state machine (reopened → addressed is absent, warn-skipped).
// Both interleavings end reopened — order-independent, no reconciliation
// pass.
func TestImplementReviewLoop_ReopenWinsBothOrders(t *testing.T) {
	for _, tc := range []struct {
		name        string
		resolutions [2]string // reviewer A's then reviewer B's resolution
	}{
		{"confirm_then_reopen", [2]string{"confirmed", "reopened"}},
		{"reopen_then_confirm", [2]string{"reopened", "confirmed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			au := newSeqAuditFake()
			cr := newFakeConcernRepo()
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
			runID, stageID := uuid.New(), uuid.New()

			row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "contested concern")
			if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed"); err != nil {
				t.Fatalf("seed addressed_pending: %v", err)
			}

			revA := &fakePlanReviewer{
				verdict: &planreview.ReviewVerdict{
					Verdict:            planreview.VerdictApprove,
					ConcernResolutions: []planreview.ConcernResolution{{ID: row.ID.String(), Resolution: tc.resolutions[0]}},
				},
				model: "claude-opus-4-8",
			}
			revB := &fakePlanReviewer{
				verdict: &planreview.ReviewVerdict{
					Verdict:            planreview.VerdictApprove,
					ConcernResolutions: []planreview.ConcernResolution{{ID: row.ID.String(), Resolution: tc.resolutions[1]}},
				},
				model: "gpt-5.5",
			}
			s.runImplementReviewInvocations(context.Background(), runID, stageID,
				[]reviewerInvocation{{reviewer: revA}, {reviewer: revB}},
				planreview.AuthorityAdvisory, "prompt", "author", "", "", planreview.DefaultReviewBudget)

			rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
			if rows[0].State != concern.StateReopened {
				t.Errorf("state = %q, want reopened (reopen wins over confirm in order %v)", rows[0].State, tc.resolutions)
			}
		})
	}
}

// TestImplementReviewLoop_SloppyResolutionsWarnSkip: every malformed or
// inapplicable resolution entry — garbage UUID, unknown ID, unknown
// resolution string, a plan-stage concern's ID — is warn-skipped while
// the valid sibling entry still applies, and the loop returns normally.
// A sloppy reviewer can never wedge the gate (#984 / #982 posture).
func TestImplementReviewLoop_SloppyResolutionsWarnSkip(t *testing.T) {
	au := newSeqAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()

	valid := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "valid concern")
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{valid.ID}, "routed"); err != nil {
		t.Fatalf("seed addressed_pending: %v", err)
	}
	planConcern := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindPlan, 90, "plan-stage concern")

	rev := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictApprove,
			ConcernResolutions: []planreview.ConcernResolution{
				{ID: "not-a-uuid", Resolution: "confirmed"},
				{ID: uuid.NewString(), Resolution: "confirmed"},        // unknown ID
				{ID: planConcern.ID.String(), Resolution: "confirmed"}, // plan-stage concern
				{ID: valid.ID.String(), Resolution: "fixed-i-promise"}, // unknown resolution string
				{ID: valid.ID.String(), Resolution: "confirmed"},       // the valid sibling
			},
		},
		model: "claude-opus-4-8",
	}
	hasRejection := s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: rev}}, planreview.AuthorityAdvisory, "prompt", "author", "", "", planreview.DefaultReviewBudget)
	if hasRejection {
		t.Error("hasRejection = true, want false (sloppy resolutions must not affect the verdict)")
	}

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{valid.ID, planConcern.ID})
	if rows[0].State != concern.StateAddressed {
		t.Errorf("valid concern state = %q, want addressed (valid sibling still applied)", rows[0].State)
	}
	if rows[1].State != concern.StateRaised {
		t.Errorf("plan concern state = %q, want raised (a reviewer can never touch a plan-stage concern)", rows[1].State)
	}
}

// TestShipTrace_ImplementReview_PriorConcernsThreadedIntoPrompt is the
// #984 cross-boundary prompt test: seeded addressed_pending AND waived
// concern rows for the stage reach the built reviewer prompt's delta-
// verification section end-to-end through the trace-upload path —
// the waived one with the operator's audited reason and the not-
// re-litigable framing, the addressed_pending one under the mandatory-
// resolution rule.
func TestShipTrace_ImplementReview_PriorConcernsThreadedIntoPrompt(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr

	pending := seedConcernRow(t, cr, runRow.ID, implStage.ID, concern.StageKindImplement, 100, "unhandled error path")
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{pending.ID}, "routed"); err != nil {
		t.Fatalf("seed addressed_pending: %v", err)
	}
	waived := seedConcernRow(t, cr, runRow.ID, implStage.ID, concern.StageKindImplement, 101, "doc companion drift")
	if _, err := cr.ApplyResolution(context.Background(), waived.ID, concern.StateWaived, "accepted trade-off"); err != nil {
		t.Fatalf("seed waived: %v", err)
	}
	// A foreign-stage concern must NOT thread into this stage's prompt.
	foreign := seedConcernRow(t, cr, runRow.ID, uuid.New(), concern.StageKindImplement, 102, "other-stage concern")

	priv, _ := sf.issue(t, runRow.ID)
	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	for _, want := range []string{
		"### Prior concerns (delta verification)",
		pending.ID.String(),
		"state: addressed_pending",
		"you MUST emit exactly one entry in the verdict's `concern_resolutions` array",
		waived.ID.String(),
		"state: waived",
		"operator waive reason: accepted trade-off",
		"MUST NOT re-raise or re-litigate a waived concern",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q from threaded prior concerns:\n%s", want, got)
		}
	}
	if strings.Contains(got, foreign.ID.String()) {
		t.Errorf("reviewer prompt must not carry another stage's concern %s:\n%s", foreign.ID, got)
	}
}

// TestShipTrace_ImplementReview_NoConcerns_PromptUnchanged: a stage with
// no recorded concerns dispatches a review prompt with neither the
// prior-concerns section nor the schema's concern_resolutions member —
// the first review's prompt is unchanged from the pre-#984 output (the
// byte-level identity is pinned in prompt_test.go; this guards the
// trace-handler seam).
func TestShipTrace_ImplementReview_NoConcerns_PromptUnchanged(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	s.cfg.ConcernRepo = newFakeConcernRepo()

	priv, _ := sf.issue(t, runRow.ID)
	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	if strings.Contains(got, "### Prior concerns") {
		t.Errorf("concern-free stage must not render the prior-concerns section:\n%s", got)
	}
	if strings.Contains(got, "concern_resolutions") {
		t.Errorf("concern-free stage must not extend the verdict schema:\n%s", got)
	}
}

// TestWaiveConcern_SuppressedFromOpenConcernsButThreadedIntoPrompt is the
// #984 done-means end-to-end: after the operator waives a concern through
// the real handler, it disappears from GET /v0/runs/{id}'s open-states-
// only concerns block AND appears only in the next re-review prompt's
// not-re-litigable section.
func TestWaiveConcern_SuppressedFromOpenConcernsButThreadedIntoPrompt(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, _, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr

	stillOpen := seedConcernRow(t, cr, runRow.ID, implStage.ID, concern.StageKindImplement, 100, "still open")
	toWaive := seedConcernRow(t, cr, runRow.ID, implStage.ID, concern.StageKindImplement, 101, "waive me")

	// Waive through the real handler so the audit-first contract runs.
	if w := postWaive(t, s, toWaive.ID.String(), waiveConcernRequest{Reason: "false positive"}); w.Code != http.StatusOK {
		t.Fatalf("waive status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// The run's concerns block lists ONLY the open concern.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runRow.ID.String(), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get run status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal run: %v", err)
	}
	if resp.Concerns == nil {
		t.Fatalf("concerns block missing:\n%s", w.Body.String())
	}
	if resp.Concerns.Open != 1 {
		t.Errorf("concerns.open = %d, want 1 (the waived concern is suppressed)", resp.Concerns.Open)
	}
	for _, item := range resp.Concerns.Items {
		if item.ID == toWaive.ID {
			t.Errorf("waived concern %s listed in the open-concerns block", toWaive.ID)
		}
	}

	// The re-review prompt carries the waived concern ONLY in the
	// not-re-litigable prior-concerns section, with the audited reason.
	priv, _ := sf.issue(t, runRow.ID)
	bundleBytes := implementDiffBundle(t,
		[]map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}})
	wr := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if wr.Code != http.StatusAccepted {
		t.Fatalf("ship status = %d, want 202:\n%s", wr.Code, wr.Body.String())
	}
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	for _, want := range []string{
		"### Prior concerns (delta verification)",
		toWaive.ID.String(),
		"state: waived",
		"operator waive reason: false positive",
		stillOpen.ID.String(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q:\n%s", want, got)
		}
	}
}
