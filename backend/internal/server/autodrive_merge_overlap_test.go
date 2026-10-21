package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// autodrive_merge_overlap_test.go pins the merge overlap guard on the
// DELEGATED may_merge arm (#4582): a 409 refusal parks decision_required with
// the code as the state, a 502 read failure is observe-only, the merger is
// never called, there is no delegated override, every refusal is recorded as a
// delegated_merge_overlap_refused row carrying counts only (approval
// conditions C4/C5), and the note names other runs only to a read:audit
// caller. Every test reads committed state (merger calls, audit rows), not
// only the outcome.

// newDelegatedOverlapFixture is a mergeOverlapFixture with run A made
// may_merge-ready: the autoDriveSpecYAML contract (no verify declared, no
// acceptance stage), workflow feature_change, plan + implement succeeded and a
// checks_green_awaiting_merge drive advance. The overlapGitHub stub serves the
// guard's main...<head> compare and the merge-candidate gate's zero-commit
// behind probe, so with the guard absent every later delegated gate admits
// and the merger is called once (TestAutoDriveRunGate_MergeOverlap_NoOverlapMerges).
func newDelegatedOverlapFixture(t *testing.T) *mergeOverlapFixture {
	t.Helper()
	f := newMergeOverlapFixture(t)
	f.rr.mu.Lock()
	f.a.WorkflowSpec = []byte(autoDriveSpecYAML)
	f.a.WorkflowID = "feature_change"
	f.a.State = run.StateRunning
	f.rr.mu.Unlock()
	f.rr.seedStage(f.a.ID, 0, run.StageStateSucceeded)
	f.seedImplementOn(f.a.ID, run.StageStateSucceeded)
	seedReviewEntry(t, f.au, f.a.ID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})
	return f
}

// overlapAuditReaderIdentity is an operator-agent identity holding read:audit,
// so the delegated note carries the guard's full detail.
func overlapAuditReaderIdentity() Identity {
	return Identity{
		Subject: operatorrole.CampaignActorSubject,
		TokenID: "tok-op-agent",
		Scopes:  []string{"write:approvals", scopeGateViewRead},
	}
}

func (f *mergeOverlapFixture) driveGate(t *testing.T, id Identity) AutoDriveOutcome {
	t.Helper()
	runRow, err := f.rr.GetRun(context.Background(), f.a.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	out, err := f.s.AutoDriveRunGate(context.Background(), runRow, id, f.merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	return out
}

// assertNoMergeRows asserts the delegated refusal committed nothing on the
// merge path: no verdict, no override, no run_auto_driven row, no merge.
func (f *mergeOverlapFixture) assertNoMergeRows(t *testing.T) {
	t.Helper()
	if f.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", f.merger.called)
	}
	for _, cat := range []string{CategoryMergeVerdictRecorded, CategoryMergeOverlapOverridden, CategoryRunAutoDriven} {
		if n := len(overlapRows(f.au, cat)); n != 0 {
			t.Errorf("%s rows = %d, want 0", cat, n)
		}
	}
}

// delegatedRefusalRow returns the single delegated_merge_overlap_refused row's
// raw payload and decoded fields.
func (f *mergeOverlapFixture) delegatedRefusalRow(t *testing.T) (string, map[string]any) {
	t.Helper()
	rows := overlapRows(f.au, CategoryDelegatedMergeOverlapRefused)
	if len(rows) != 1 {
		t.Fatalf("delegated_merge_overlap_refused rows = %d, want 1", len(rows))
	}
	var fields map[string]any
	if err := json.Unmarshal(rows[0].Payload, &fields); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return string(rows[0].Payload), fields
}

// assertNamesNoOtherRun fails when text names B's run id, stage id or issue ref.
func assertNamesNoOtherRun(t *testing.T, what, text string, b *run.Run, st *run.Stage) {
	t.Helper()
	for _, leak := range []string{b.ID.String(), st.ID.String(), *b.TriggerRef} {
		if strings.Contains(text, leak) {
			t.Errorf("%s names another run's detail %q:\n%s", what, leak, text)
		}
	}
}

// TestAutoDriveRunGate_MergeOverlap_RefusesAndParks: B's implement stage
// dispatched or running on shared.go, which A's PR changes, parks the
// delegated merge decision_required overlapping_implement_in_flight with the
// merger never called. Counterfactual (a): replacing the may_merge call line
// with a constant not-held init lets the merge-candidate gate admit and the
// merger run → RED.
func TestAutoDriveRunGate_MergeOverlap_RefusesAndParks(t *testing.T) {
	for _, state := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		t.Run(string(state), func(t *testing.T) {
			f := newDelegatedOverlapFixture(t)
			b, st := f.seedOther(t, state, "shared.go")
			out := f.driveGate(t, overlapAuditReaderIdentity())
			if !out.DecisionRequired || out.DecisionState != mergeCodeOverlappingImplement || out.Action != delegation.ActionMerge || out.Acted || out.Paged {
				t.Fatalf("outcome = %+v, want decision_required %s on merge", out, mergeCodeOverlappingImplement)
			}
			f.assertNoMergeRows(t)
			if !strings.Contains(out.Note, b.ID.String()) || !strings.Contains(out.Note, "allow_overlap is operator-only") {
				t.Errorf("note = %q, want B's run id (read:audit caller) and the no-override line", out.Note)
			}
			if !strings.Contains(f.log.String(), "code="+mergeCodeOverlappingImplement) {
				t.Errorf("log lacks the refusal code:\n%s", f.log.String())
			}
			assertNamesNoOtherRun(t, "server log", f.log.String(), b, st)
			raw, fields := f.delegatedRefusalRow(t)
			if fields["refused_code"] != mergeCodeOverlappingImplement || fields["blocking_count"] != float64(1) ||
				fields["undetermined_count"] != float64(0) || fields["head_sha"] != overlapHead || fields["run_id"] != f.a.ID.String() {
				t.Fatalf("delegated refusal row = %v", fields)
			}
			assertNamesNoOtherRun(t, "delegated_merge_overlap_refused payload", raw, b, st)
		})
	}
}

// TestAutoDriveRunGate_MergeOverlap_UndeterminedParks (approval condition
// C4): A's changed paths are unreadable (no installation) while B's implement
// runs, so the delegated merge parks decision_required overlap_undetermined
// and records the decision with counts only.
func TestAutoDriveRunGate_MergeOverlap_UndeterminedParks(t *testing.T) {
	f := newDelegatedOverlapFixture(t)
	b, st := f.seedOther(t, run.StageStateRunning, "shared.go")
	f.a.InstallationID = nil
	out := f.driveGate(t, campaignOperatorIdentity())
	if !out.DecisionRequired || out.DecisionState != mergeCodeOverlapUndetermined || out.Action != delegation.ActionMerge || out.Acted {
		t.Fatalf("outcome = %+v, want decision_required %s", out, mergeCodeOverlapUndetermined)
	}
	f.assertNoMergeRows(t)
	assertNamesNoOtherRun(t, "note", out.Note, b, st)
	raw, fields := f.delegatedRefusalRow(t)
	if fields["refused_code"] != mergeCodeOverlapUndetermined || fields["refused_reason"] != overlapReasonChangedPathsUnavailable ||
		fields["undetermined_count"] != float64(1) || fields["blocking_count"] != float64(0) {
		t.Fatalf("delegated refusal row = %v", fields)
	}
	assertNamesNoOtherRun(t, "delegated_merge_overlap_refused payload", raw, b, st)
}

// TestAutoDriveRunGate_MergeOverlap_CheckFailedObservesOnly: a compare read
// failure (502 overlap_check_failed) is observe-only fail-closed, not a park.
// Counterfactual (e): `if false {` on the 502 classification parks it
// decision_required → RED.
func TestAutoDriveRunGate_MergeOverlap_CheckFailedObservesOnly(t *testing.T) {
	f := newDelegatedOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.gh.compareStatus = http.StatusInternalServerError
	out := f.driveGate(t, overlapAuditReaderIdentity())
	if out.Acted || out.Paged || out.DecisionRequired {
		t.Fatalf("outcome = %+v, want observe-only", out)
	}
	if !strings.Contains(out.Note, mergeCodeOverlapCheckFailed) || !strings.Contains(out.Note, "fail-closed") {
		t.Fatalf("note = %q", out.Note)
	}
	f.assertNoMergeRows(t)
	_, fields := f.delegatedRefusalRow(t)
	if fields["refused_code"] != mergeCodeOverlapCheckFailed || fields["refused_reason"] != overlapReasonForgeRead {
		t.Fatalf("delegated refusal row = %v", fields)
	}
}

// TestAutoDriveRunGate_MergeOverlap_NoOverlapMerges: on the SAME fixture, a
// disjoint or settled implement elsewhere admits and the delegated merge runs
// as before — which is what makes the fixture isolate the guard.
func TestAutoDriveRunGate_MergeOverlap_NoOverlapMerges(t *testing.T) {
	for name, seed := range map[string]func(t *testing.T, f *mergeOverlapFixture){
		"disjoint":            func(t *testing.T, f *mergeOverlapFixture) { f.seedOther(t, run.StageStateRunning, "disjoint.go") },
		"implement_succeeded": func(t *testing.T, f *mergeOverlapFixture) { f.seedOther(t, run.StageStateSucceeded, "shared.go") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDelegatedOverlapFixture(t)
			seed(t, f)
			out := f.driveGate(t, overlapAuditReaderIdentity())
			if !out.Acted || out.Action != delegation.ActionMerge || out.DecisionRequired {
				t.Fatalf("outcome = %+v, want acted merge", out)
			}
			if f.merger.called != 1 {
				t.Fatalf("merger called %d times, want 1", f.merger.called)
			}
			if n := len(overlapRows(f.au, CategoryDelegatedMergeOverlapRefused)); n != 0 {
				t.Fatalf("delegated_merge_overlap_refused rows = %d, want 0 (the guard admitted)", n)
			}
		})
	}
}

// TestAutoDriveRunGate_MergeOverlap_NoteWithheldWithoutReadAudit (approval
// condition C4): the campaign operator identity holds no read:audit, so the
// note carries counts, never B's run id, stage id or issue ref.
// Counterfactual (d): `shown = o` in autodrive.go puts the guard's message
// naming B's run and stage in the note → RED.
func TestAutoDriveRunGate_MergeOverlap_NoteWithheldWithoutReadAudit(t *testing.T) {
	f := newDelegatedOverlapFixture(t)
	b, st := f.seedOther(t, run.StageStateRunning, "shared.go")
	out := f.driveGate(t, campaignOperatorIdentity())
	if !out.DecisionRequired || out.DecisionState != mergeCodeOverlappingImplement {
		t.Fatalf("outcome = %+v, want decision_required %s", out, mergeCodeOverlappingImplement)
	}
	assertNamesNoOtherRun(t, "note", out.Note, b, st)
	if !strings.Contains(out.Note, "1 live run(s) elsewhere") || !strings.Contains(out.Note, "read:audit") {
		t.Fatalf("note = %q, want the blocking count and the read:audit pointer", out.Note)
	}
	f.assertNoMergeRows(t)
}

// TestAutoDriveRunGate_MergeOverlap_RefusalRecordedOncePerDecision (approval
// condition C5): re-evaluating an unchanged refusal appends no second row; a
// changed decision (a second blocker) appends one. Counterfactual: deleting
// the recordDelegatedMergeOverlapRefusal call leaves zero rows → RED;
// defeating the latest-row comparison appends a row per poll → RED.
func TestAutoDriveRunGate_MergeOverlap_RefusalRecordedOncePerDecision(t *testing.T) {
	f := newDelegatedOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	for i := 0; i < 2; i++ {
		if out := f.driveGate(t, campaignOperatorIdentity()); !out.DecisionRequired {
			t.Fatalf("pass %d outcome = %+v", i, out)
		}
	}
	if n := len(overlapRows(f.au, CategoryDelegatedMergeOverlapRefused)); n != 1 {
		t.Fatalf("rows after two identical refusals = %d, want 1", n)
	}
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.driveGate(t, campaignOperatorIdentity())
	rows := overlapRows(f.au, CategoryDelegatedMergeOverlapRefused)
	if len(rows) != 2 {
		t.Fatalf("rows after a changed decision = %d, want 2", len(rows))
	}
	var last map[string]any
	_ = json.Unmarshal(rows[1].Payload, &last)
	if last["blocking_count"] != float64(2) {
		t.Fatalf("second row = %v, want blocking_count 2", last)
	}
	if rows[1].ActorSubject == nil || *rows[1].ActorSubject != operatorrole.CampaignActorSubject {
		t.Errorf("actor subject = %v", rows[1].ActorSubject)
	}
}

// TestAutoDriveRunGate_MergeOverlap_RecordFailureStillRefuses: a failed
// record (the dedupe read, or the append) is logged and the refusal stands —
// the record never turns a refusal into a merge.
func TestAutoDriveRunGate_MergeOverlap_RecordFailureStillRefuses(t *testing.T) {
	for name, set := range map[string]func(f *mergeOverlapFixture){
		"append": func(f *mergeOverlapFixture) { f.au.appendErrCategory = CategoryDelegatedMergeOverlapRefused },
		"read":   func(f *mergeOverlapFixture) { f.au.listByCategoryErrCategory = CategoryDelegatedMergeOverlapRefused },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDelegatedOverlapFixture(t)
			f.seedOther(t, run.StageStateRunning, "shared.go")
			set(f)
			out := f.driveGate(t, campaignOperatorIdentity())
			if !out.DecisionRequired || out.DecisionState != mergeCodeOverlappingImplement {
				t.Fatalf("outcome = %+v, want the refusal to stand", out)
			}
			f.assertNoMergeRows(t)
			if !strings.Contains(f.log.String(), "delegated_merge_overlap_refused could not be recorded") {
				t.Fatalf("log lacks the record failure:\n%s", f.log.String())
			}
		})
	}
}

// TestAutoDrive_MergeOverlap_EndpointReportsDecisionRequired: the cross-layer
// path — POST /v0/runs/{run_id}/auto-drive → AutoDriveRunGate → delegation
// evaluator → mergeOverlapGuard over the run repo, the artifact-backed plan
// scope and the httptest GitHub compare → the JSON response, with no
// run_auto_driven row and no merge.
func TestAutoDrive_MergeOverlap_EndpointReportsDecisionRequired(t *testing.T) {
	f := newDelegatedOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	w := autoDrivePost(t, f.s, f.s.handleAutoDrive, f.a.ID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["decision_required"] != true || raw["decision_state"] != mergeCodeOverlappingImplement || raw["acted"] != false || raw["action"] != delegation.ActionMerge {
		t.Fatalf("response = %v", raw)
	}
	f.assertNoMergeRows(t)
	if n := len(overlapRows(f.au, CategoryDelegatedMergeOverlapRefused)); n != 1 {
		t.Fatalf("delegated_merge_overlap_refused rows = %d, want 1", n)
	}
}
