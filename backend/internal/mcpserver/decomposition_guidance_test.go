package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// #5215 rules 4, 6, 9 and 11 (R2): the decomposition verb text, the
// consolidate refusal hint, the embedded runbook, and the getRunStatus call
// site of the two next_actions folds. Every assertion reads text an operator
// actually receives: SERVED tool descriptions over an in-memory MCP ListTools,
// the handler's error, the embedded runbookMarkdown, and the served
// next_actions. Description anchors are lowercased and whitespace-flattened
// (fixupGuidanceFlatten), so a copy-edit does not redden a test while removing
// the rule does.

// decompositionGuidanceSection returns text[from : the first `to` after it],
// failing when either anchor is missing.
func decompositionGuidanceSection(t *testing.T, label, text, from, to string) string {
	t.Helper()
	start := strings.Index(text, from)
	if start < 0 {
		t.Fatalf("%s lacks %q", label, from)
	}
	end := strings.Index(text[start:], to)
	if end < 0 {
		t.Fatalf("%s: %q is not followed by %q", label, from, to)
	}
	return text[start : start+end]
}

func TestConsolidateSlices_DescriptionRoutesThroughRunStatus(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_consolidate_slices")
	for _, want := range []string{
		// The lead: the event-driven fan-in normally settles the parent.
		"still awaiting_children",
		"event-driven fan-in normally integrates the slices and settles the parent",
		"read fishhawk_get_run_status on the parent",
		// not_awaiting_children is not proof of success.
		"does not by itself mean the fan-in succeeded",
		"slice-conflict failure",
		"failed child",
		"pending or awaiting_host_dispatch",
		"fishhawk_dispatch_stage",
		"fishhawk_drive_run",
		"fan-in verify-only pass",
		// slice_integration_error is safe to re-invoke.
		"stays awaiting_children",
		"idempotent",
		"re-invoke once the cause clears",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_consolidate_slices description lacks %q", want)
		}
	}
	for _, stale := range []string{
		"locally a settled parent stays parked until you call this verb",
		"already resolved or not a decomposition",
		"off by default in the local dev stack",
	} {
		if strings.Contains(desc, stale) {
			t.Errorf("fishhawk_consolidate_slices description still carries the stale claim %q", stale)
		}
	}
	mergeGuidanceADR110(t, "fishhawk_consolidate_slices", desc)
}

func TestAwaitChildren_DescriptionConditionalSettledStep(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_await_children")
	section := decompositionGuidanceSection(t, "fishhawk_await_children description", desc,
		`- "children_settled" — every child succeeded and the newest`, `- "timeout"`)
	for _, want := range []string{
		"next_step is fishhawk_consolidate_slices only while the parent's implement stage is still awaiting_children",
		"event-driven fan-in normally settles the parent first",
		"409 not_awaiting_children",
		"fishhawk_get_run_status on the parent instead",
		"pending or awaiting_host_dispatch",
		"fishhawk_drive_run",
		"fan-in verify-only pass",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("children_settled bullet lacks %q:\n%s", want, section)
		}
	}
	mergeGuidanceADR110(t, "fishhawk_await_children children_settled", section)
}

func TestConsolidateRefusalHint(t *testing.T) {
	t.Run("not_awaiting_children", func(t *testing.T) {
		hint := consolidateRefusalHint(&apiError{StatusCode: http.StatusConflict, Code: "not_awaiting_children"})
		for _, want := range []string{"fishhawk_get_run_status", "does not by itself mean the fan-in succeeded", "slice conflict", "pending or awaiting_host_dispatch", "fishhawk_dispatch_stage", "fishhawk_drive_run"} {
			if !strings.Contains(hint, want) {
				t.Errorf("not_awaiting_children hint %q lacks %q", hint, want)
			}
		}
		mergeGuidanceADR110(t, "consolidate not_awaiting_children hint", hint)
	})
	t.Run("slice_integration_error", func(t *testing.T) {
		hint := consolidateRefusalHint(&apiError{StatusCode: http.StatusBadGateway, Code: "slice_integration_error"})
		for _, want := range []string{"re-invoke", "idempotent", "awaiting_children"} {
			if !strings.Contains(hint, want) {
				t.Errorf("slice_integration_error hint %q lacks %q", hint, want)
			}
		}
		mergeGuidanceADR110(t, "consolidate slice_integration_error hint", hint)
	})
	t.Run("wrapped apiError still matches", func(t *testing.T) {
		wrapped := fmt.Errorf("transport layer: %w", &apiError{StatusCode: http.StatusConflict, Code: "not_awaiting_children"})
		if consolidateRefusalHint(wrapped) == "" {
			t.Error("a wrapped not_awaiting_children apiError yields no hint; errors.As must unwrap it")
		}
	})
	t.Run("other code", func(t *testing.T) {
		for _, code := range []string{"children_in_flight", "children_failed", "not_a_decomposed_parent", ""} {
			if got := consolidateRefusalHint(&apiError{StatusCode: http.StatusConflict, Code: code}); got != "" {
				t.Errorf("code %q hint = %q, want \"\"", code, got)
			}
		}
	})
	t.Run("transport error", func(t *testing.T) {
		if got := consolidateRefusalHint(errors.New("dial tcp: connection refused")); got != "" {
			t.Errorf("transport error hint = %q, want \"\"", got)
		}
		if got := consolidateRefusalHint(nil); got != "" {
			t.Errorf("nil error hint = %q, want \"\"", got)
		}
	})
}

// TestConsolidateSlices_RefusalErrorCarriesHint drives the REAL handler against
// the package fake backend's consolidate seam. A hinted refusal keeps the %w
// wrap (errors.As on *apiError still works, approval condition Q5) and appends
// the remedy; every other code's error is byte-identical to the pre-#5215 shape.
func TestConsolidateSlices_RefusalErrorCarriesHint(t *testing.T) {
	for _, tc := range []struct {
		status   int
		code     string
		wantHint bool
	}{
		{http.StatusConflict, "not_awaiting_children", true},
		{http.StatusBadGateway, "slice_integration_error", true},
		{http.StatusConflict, "children_in_flight", false},
		{http.StatusConflict, "children_failed", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)
			parent := uuid.New()
			fb.mu.Lock()
			fb.consolidateStatus, fb.consolidateCode = tc.status, tc.code
			fb.mu.Unlock()

			_, _, err := r.consolidateSlices(context.Background(), nil, ConsolidateSlicesInput{RunID: parent.String()})
			if err == nil {
				t.Fatal("consolidateSlices err = nil, want the refusal")
			}
			fb.mu.Lock()
			calls := fb.consolidateCalledBy[parent]
			fb.mu.Unlock()
			if calls != 1 {
				t.Fatalf("consolidate called %d times, want 1 (the refusal must come from the backend seam)", calls)
			}
			var ae *apiError
			if !errors.As(err, &ae) {
				t.Fatalf("errors.As(*apiError) failed on %v; the %%w wrap must survive", err)
			}
			if ae.Code != tc.code || ae.StatusCode != tc.status {
				t.Errorf("apiError = %d %q, want %d %q", ae.StatusCode, ae.Code, tc.status, tc.code)
			}
			hint := consolidateRefusalHint(ae)
			if tc.wantHint {
				if want := "consolidate slices: " + ae.Error() + " — " + hint; err.Error() != want {
					t.Errorf("error = %q, want %q", err.Error(), want)
				}
				if hint == "" {
					t.Error("hinted code produced an empty hint")
				}
				return
			}
			if want := "consolidate slices: " + ae.Error(); err.Error() != want {
				t.Errorf("error = %q, want byte-identical %q", err.Error(), want)
			}
		})
	}
	t.Run("success unchanged", func(t *testing.T) {
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		_, out, err := r.consolidateSlices(context.Background(), nil, ConsolidateSlicesInput{RunID: uuid.NewString()})
		if err != nil || out.Result.Outcome != "integrated" {
			t.Errorf("consolidateSlices = %+v, %v; want integrated, nil", out, err)
		}
	})
}

func TestRunbook_DecomposedPathConditionalDispatchAndUnionMerge(t *testing.T) {
	rb := fixupGuidanceFlatten(runbookMarkdown)
	section := decompositionGuidanceSection(t, "runbook", rb,
		"3. **let the fan-in settle", "### driving with `fishhawk_drive_run`")
	for _, want := range []string{
		"event-driven fan-in normally",
		"only while that stage is **still `awaiting_children`**",
		"`409 not_awaiting_children`",
		"does not by itself mean success",
		"slice conflict",
		"`502 slice_integration_error`",
		"idempotent",
		"fan-in verify-only pass",
		"`pending` or `awaiting_host_dispatch`",
		"`fishhawk_dispatch_stage`",
		"`fishhawk_drive_run`",
		"before the consolidated review",
		"never hand-push a union merge",
		"vouch exemption",
		"unverified, unreviewed and unaccepted",
		"`fishhawk_fixup_stage`",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("runbook decomposed-parent path lacks %q", want)
		}
	}
	if strings.Contains(rb, "this operator verb **is** the fan-in") {
		t.Error("runbook still claims fishhawk_consolidate_slices IS the local fan-in")
	}
	mergeGuidanceADR110(t, "runbook decomposed-parent path", section)

	retry := decompositionGuidanceSection(t, "runbook", rb,
		"before retrying a failed implement stage, read any checkpoint ref", "### decomposed-parent native path")
	for _, want := range []string{"`inspect_checkpoint_ref`", "`params.ref`", "refs/fishhawk/checkpoints/", "`git show`", "a retry re-runs from the branch"} {
		if !strings.Contains(retry, want) {
			t.Errorf("runbook retry checkpoint line lacks %q", want)
		}
	}
	mergeGuidanceADR110(t, "runbook retry checkpoint line", retry)
}

// TestGetRunStatus_FanInVerifyPassNamed is the getRunStatus call-site pin for
// foldFanInVerifyPassAdvisory: the audit row is served by the fake backend and
// the assertion reads the served next_actions, so deleting the tools.go fold
// call reddens it (the run_stage.go call site has its own pin).
func TestGetRunStatus_FanInVerifyPassNamed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	implStage := uuid.NewString()
	fb.mu.Lock()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running", WorkflowID: "feature_change", RunnerKind: "local"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: implStage, RunID: runID.String(), Sequence: 2, Type: "implement", State: "awaiting_host_dispatch"},
	}
	fb.auditByRun[runID] = []AuditEntry{
		{ID: uuid.NewString(), Sequence: 7, RunID: runID.String(), StageID: &implStage, Category: categoryStageMergeCandidateVerifyTriggered, Payload: map[string]any{"cause": "fan_in"}},
	}
	fb.mu.Unlock()

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil {
		t.Fatal("next_actions absent")
	}
	var found bool
	for _, a := range out.NextActions.Actions {
		if a.Action == "fishhawk_await_stage" && a.Params["stage_id"] == implStage && strings.Contains(a.Reason, "fan-in verify-only pass") {
			found = true
		}
	}
	if !found {
		t.Errorf("served next_actions %v (state %s) carry no fan-in verify-pass await", actionNames(out.NextActions), out.NextActions.State)
	}
}

// TestGetRunStatus_BindingAssertionRecoveryNamed is the getRunStatus call-site
// pin for foldBindingAssertionRecoveryAdvisory (the category-B failed-row arm).
func TestGetRunStatus_BindingAssertionRecoveryNamed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	implStage := uuid.NewString()
	category, reason := "B", "scope completeness failed by the operator"
	fb.mu.Lock()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running", WorkflowID: "feature_change", RunnerKind: "local"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: implStage, RunID: runID.String(), Sequence: 2, Type: "implement", State: "failed", FailureCategory: &category, FailureReason: &reason},
	}
	fb.auditByRun[runID] = []AuditEntry{
		{ID: uuid.NewString(), Sequence: 6, RunID: runID.String(), StageID: &implStage, Category: auditCategoryScopeCompletenessFailed, Payload: baAssertions},
		{ID: uuid.NewString(), Sequence: 5, RunID: runID.String(), StageID: &implStage, Category: auditCategoryScopeCompletenessParked, Payload: baAssertions},
	}
	fb.mu.Unlock()

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil || out.NextActions.State != "implement_failed_category_b" {
		t.Fatalf("next_actions = %+v, want implement_failed_category_b", out.NextActions)
	}
	a := findAction(t, out.NextActions, "fishhawk_resume_run")
	if a.Params["reason"] != bindingAssertionResumeReasonPlaceholder || !strings.Contains(a.Reason, "parks again or fails") {
		t.Errorf("served resume action = %+v, want the binding-assertion recovery fold applied", a)
	}
}
