package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

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
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   ts,
		AuditRepo:    au,
		RunRepo:      rr,
		ArtifactRepo: art,
		PlanReviewer: reviewer,
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

	// #574: an erroring gating reviewer must NOT fail the stage.
	if implStage.State == run.StageStateFailed {
		t.Errorf("reviewer error must not fail the gating implement stage; state = %q", implStage.State)
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
	if s.runImplementReviews(ctx, runRow.ID, implStage.ID, diff, nil) {
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
