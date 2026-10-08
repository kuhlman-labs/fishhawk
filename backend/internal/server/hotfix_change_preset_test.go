package server

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// hotfixChangePresetPath is the SHIPPED incident hotfix preset (E35.5 /
// #1602). These seam tests drive it through the alert ingress (E35.4 / #1601)
// rather than an inline copy, so a drift in the committed example fails here.
const hotfixChangePresetPath = "../../../docs/spec/examples/workflow-v2-hotfix-change.yaml"

func readHotfixChangePreset(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(hotfixChangePresetPath)
	if err != nil {
		t.Fatalf("read %s: %v", hotfixChangePresetPath, err)
	}
	return string(raw)
}

// TestStartAlertRun_ShippedHotfixChangePresetAdmitted is the cross-boundary
// seam for the preset's routing: the shipped bytes go through StartAlertRun ->
// handleCreateRun (applies_to admission, where an alert run maps to
// spec.TriggerDiff) -> a persisted run row.
//
// The [scheduled] variant is the in-test counterfactual isolating the
// applies_to trigger list as the admitting control: the same document with
// only that list rewritten is refused 422 workflow_not_applicable and mints
// nothing. Observed RED with the example's `trigger: [diff]` rewritten to
// `[scheduled]`: the first assertion reports outcome refused/422
// workflow_not_applicable instead of started.
func TestStartAlertRun_ShippedHotfixChangePresetAdmitted(t *testing.T) {
	shipped := readHotfixChangePreset(t)

	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: newAuditFake()})
	out, err := s.StartAlertRun(context.Background(), alertParams(shipped))
	if err != nil {
		t.Fatalf("StartAlertRun: %v", err)
	}
	if out.Kind != AlertStartStarted || out.Status != http.StatusCreated {
		t.Fatalf("outcome = %+v, want started/201 on the shipped hotfix_change preset", out)
	}
	r := onlyRun(t, repo)
	if r.TriggerSource != run.TriggerAlert || r.WorkflowID != "hotfix_change" {
		t.Errorf("run trigger_source/workflow_id = %q/%q, want alert/hotfix_change", r.TriggerSource, r.WorkflowID)
	}

	const admitting = "      trigger: [diff]\n"
	if n := strings.Count(shipped, admitting); n != 1 {
		t.Fatalf("anchor %q occurs %d times in the shipped preset, want exactly 1", admitting, n)
	}
	scheduled := strings.Replace(shipped, admitting, "      trigger: [scheduled]\n", 1)
	refusedRepo := newFakeRepo()
	rs := New(Config{Addr: "127.0.0.1:0", RunRepo: refusedRepo, AuditRepo: newAuditFake()})
	refused, err := rs.StartAlertRun(context.Background(), alertParams(scheduled))
	if err != nil {
		t.Fatalf("StartAlertRun([scheduled]): %v", err)
	}
	if refused.Kind != AlertStartRefused || refused.Status != http.StatusUnprocessableEntity || refused.Code != "workflow_not_applicable" {
		t.Fatalf("[scheduled] outcome = %+v, want refused/422 workflow_not_applicable", refused)
	}
	if n := runRowCount(refusedRepo); n != 0 {
		t.Errorf("refused start minted %d runs, want 0", n)
	}
}

// TestGetRun_HotfixChangePreset_DelegationKeepsMergeHuman crosses spec parse
// -> alert admission -> persisted run -> buildDelegationPayload: the run
// status of an alert-started hotfix run advertises approve, waive and merge
// gated by an EXPLICIT entry, fixup and retry delegated by the medium tier,
// the nine-event page list, and no delegated approve, waive or merge in the
// evaluated actions.
//
// Counterfactual: delete `approve: {mode: gated}` from the example -> RED:
// matrix[approve] resolves auto / tier / clean_dual_approval and approve
// appears in the evaluated actions.
func TestGetRun_HotfixChangePreset_DelegationKeepsMergeHuman(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	out, err := s.StartAlertRun(context.Background(), alertParams(readHotfixChangePreset(t)))
	if err != nil || out.Kind != AlertStartStarted {
		t.Fatalf("StartAlertRun: outcome = %+v, err = %v; want started", out, err)
	}

	resp, raw := getRunResponse(t, s, out.RunID)
	if resp.Delegation == nil {
		t.Fatal("delegation block missing")
	}
	if resp.Delegation.Autonomy != "medium" {
		t.Errorf("autonomy = %q, want medium", resp.Delegation.Autonomy)
	}
	if deleg := rawDelegation(t, raw); deleg["autonomy"] != "medium" {
		t.Errorf("raw autonomy = %v, want medium", deleg["autonomy"])
	}

	wantMatrix := map[string]struct{ mode, source, condition string }{
		"approve": {"gated", "explicit", ""},
		"waive":   {"gated", "explicit", ""},
		"merge":   {"gated", "explicit", ""},
		"fixup":   {"auto", "tier", "convergent_concerns"},
		"retry":   {"auto", "tier", "infra_flake"},
	}
	got := map[string]runDelegationMatrixPayload{}
	for _, m := range resp.Delegation.Matrix {
		got[m.Action] = m
	}
	if len(got) != len(wantMatrix) {
		t.Fatalf("matrix = %+v, want %d classes", resp.Delegation.Matrix, len(wantMatrix))
	}
	for class, want := range wantMatrix {
		m := got[class]
		if m.Mode != want.mode || m.Source != want.source || m.Condition != want.condition {
			t.Errorf("matrix[%s] = mode %q source %q condition %q, want %q/%q/%q",
				class, m.Mode, m.Source, m.Condition, want.mode, want.source, want.condition)
		}
	}

	wantPage := []string{
		"advisory_reviewer_reject", "gating_reviewer_reject", "plan_rejection",
		"scope_amendment", "budget_override", "policy_override",
		"exception_request", "requirement_arbitration", "clarification_request",
	}
	if !slices.Equal(resp.Delegation.MustPageHuman, wantPage) {
		t.Errorf("must_page_human = %v, want %v", resp.Delegation.MustPageHuman, wantPage)
	}

	for _, a := range resp.Delegation.Actions {
		switch a.Action {
		case "approve", "waive", "merge":
			t.Errorf("%s present in evaluated actions (%+v); the hotfix preset delegates none of approve, waive or merge", a.Action, a)
		}
	}
	if fixup := delegationAction(t, resp, "route_fixup"); fixup.Mode != "auto" || fixup.Source != "tier" {
		t.Errorf("route_fixup mode/source = %q/%q, want auto/tier", fixup.Mode, fixup.Source)
	}
	if retry := delegationAction(t, resp, "retry"); retry.Mode != "auto" || retry.Source != "tier" {
		t.Errorf("retry mode/source = %q/%q, want auto/tier", retry.Mode, retry.Source)
	}
}
