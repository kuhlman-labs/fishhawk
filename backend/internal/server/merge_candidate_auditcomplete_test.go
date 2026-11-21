package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-090 Amendment A2 (#5409): fishhawk_audit_complete reads the merge gate's
// merge-candidate verify predicate for the run's live PR head. These tests run
// the cross-boundary seam end to end: the runner's signed
// conflict_resolution_pushed and merge_candidate_verified reports → the audit
// chain → server.mergeCandidateVerifyState → the PRODUCTION auditCompleteDeps()
// wiring → auditcomplete.ComputeResult (rule 8) → auditcheckpublisher → the
// forge fake. The fixtures in conflictresolution_test.go and
// merge_candidate_auditcheck_test.go are reused unchanged and only wrapped here.
//
// The forge fake serves crMergeSHA as the live PR head. The fixture's audit fake
// carries no EntryHash and no trace rows, so the overall conclusion is a
// failure on the chain/trace rules (#4397 approval condition C1); these tests
// assert the presence or absence of the merge_candidate_* kinds, and a non-
// success conclusion where the rule alone must hold it.

// mcAuditCompleteSeed is seedConflictResolutionE2E with the run row carrying
// specYAML as its workflow spec and the chain stamping sequences (which
// liveMergeCandidateTrigger's trigger-consumption compare needs).
func mcAuditCompleteSeed(t *testing.T, specYAML string) *crSeed {
	t.Helper()
	sd := seedConflictResolutionE2E(t)
	sd.au.stampSequence = true
	row := sd.rr.getRuns[sd.runID]
	row.WorkflowID = "feature_change"
	row.WorkflowSpec = []byte(specYAML)
	return sd
}

// mcKinds returns every merge_candidate_* kind in items.
func mcKinds(items []auditcomplete.MissingItem) []auditcomplete.MissingKind {
	var out []auditcomplete.MissingKind
	for _, m := range items {
		if strings.HasPrefix(string(m.Kind), "merge_candidate_") {
			out = append(out, m.Kind)
		}
	}
	return out
}

// mcItem returns the first item of kind, or nil.
func mcItem(items []auditcomplete.MissingItem, kind auditcomplete.MissingKind) *auditcomplete.MissingItem {
	for i := range items {
		if items[i].Kind == kind {
			return &items[i]
		}
	}
	return nil
}

// computeAuditComplete recomputes through the production wiring.
func computeAuditComplete(t *testing.T, sd *crSeed) auditcomplete.Result {
	t.Helper()
	res, err := auditcomplete.ComputeResult(context.Background(), sd.runID, sd.s.auditCompleteDeps())
	if err != nil {
		t.Fatalf("ComputeResult: %v", err)
	}
	return res
}

// assertNoMergeCandidateKind fails when either output field of p names any
// merge_candidate_* kind.
func assertNoMergeCandidateKind(t *testing.T, p forge.CreateCheckRunParams, why string) {
	t.Helper()
	for _, kind := range []auditcomplete.MissingKind{
		auditcomplete.MissingMergeCandidateUnverified,
		auditcomplete.MissingMergeCandidateVerifyFailed,
		auditcomplete.MissingMergeCandidateCheckFailed,
	} {
		assertCheckOutputOmits(t, p, kind, why)
	}
}

// TestAuditComplete_MergeCandidateUnverifiedBeforePass: after the signed
// conflict_resolution_pushed report and before any pass, the recompute carries
// merge_candidate_unverified naming crMergeSHA, a forced publish at crMergeSHA
// posts a check run that names it and is not a success, and the real GET
// /v0/stages/{id}/checks handler serializes the kind (approval condition M3).
//
// Counterfactual 1: `s.mergeCandidateStateFetcher(),` → `nil,` in
// auditCompleteDeps skips rule 8, so no merge_candidate_unverified item exists
// and this test goes RED on the presence assertion.
func TestAuditComplete_MergeCandidateUnverifiedBeforePass(t *testing.T) {
	sd := mcAuditCompleteSeed(t, mcvUndelegatedSpecYAML)
	shipConflictResolutionPushed(t, sd)
	// The report re-parks the review gate; the operator's approval terminalizes
	// the implement stage so the recompute reaches rule 8 rather than the
	// mid-flight stage_not_terminal return.
	sd.impl.State = run.StageStateSucceeded

	res := computeAuditComplete(t, sd)
	item := mcItem(res.Missing, auditcomplete.MissingMergeCandidateUnverified)
	if item == nil {
		t.Fatalf("missing = %+v, want a merge_candidate_unverified item for the unverified conflict-resolution head", res.Missing)
	}
	if !strings.Contains(item.Detail, crMergeSHA) || !strings.Contains(item.Detail, mergeCandidateCauseConflictResolution) {
		t.Errorf("detail = %q, want it to name the live head %s and cause %s", item.Detail, crMergeSHA, mergeCandidateCauseConflictResolution)
	}
	if strings.Contains(item.Detail, crAgentHeadSHA) {
		t.Errorf("detail names the stale artifact head %s; the rule binds the LIVE head: %s", crAgentHeadSHA, item.Detail)
	}
	if got := mcKinds(res.Missing); len(got) != 1 {
		t.Errorf("merge_candidate kinds = %v, want exactly one", got)
	}

	if _, ok := sd.s.recomputeAndForcePublishAuditCompleteAtHead(context.Background(), sd.runID, crMergeSHA); !ok {
		t.Fatal("forced publish computed no result")
	}
	calls := checkRunCalls(sd.creator)
	if len(calls) == 0 {
		t.Fatal("forced publish at crMergeSHA posted no check run")
	}
	last := calls[len(calls)-1]
	if last.HeadSHA != crMergeSHA {
		t.Errorf("check run head = %s, want %s", last.HeadSHA, crMergeSHA)
	}
	if !strings.Contains(last.OutputText+last.OutputSummary, string(auditcomplete.MissingMergeCandidateUnverified)) {
		t.Errorf("check run output does not name merge_candidate_unverified:\nsummary=%s\ntext=%s", last.OutputSummary, last.OutputText)
	}
	if last.Conclusion == forge.CheckRunConclusionSuccess {
		t.Errorf("check run conclusion = success on an unverified merge candidate")
	}

	// M3: the real GET /v0/stages/{id}/checks handler serializes the kind.
	sd.s.cfg.StageCheckRepo = newStageCheckRepoFake()
	var reviewID string
	for _, st := range sd.rr.stagesByRunID[sd.runID] {
		if st.Type == run.StageTypeReview {
			reviewID = st.ID.String()
		}
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/checks", reviewID), nil)
	w := httptest.NewRecorder()
	sd.s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET checks status = %d:\n%s", w.Code, w.Body.String())
	}
	var body struct {
		Items []struct {
			Name    string `json:"name"`
			Missing []struct {
				Kind   string `json:"kind"`
				Detail string `json:"detail"`
			} `json:"missing"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var kinds []string
	for _, it := range body.Items {
		if it.Name != AuditCompleteCheckName {
			continue
		}
		for _, m := range it.Missing {
			if strings.HasPrefix(m.Kind, "merge_candidate_") {
				kinds = append(kinds, m.Kind)
			}
		}
	}
	if len(kinds) != 1 || kinds[0] != "merge_candidate_unverified" {
		t.Errorf("serialized merge_candidate missing[].kind = %v, want [merge_candidate_unverified]:\n%s", kinds, w.Body.String())
	}
}

// TestAuditComplete_MergeCandidatePassedClears: after the pass and a signed
// passed merge_candidate_verified report for crMergeSHA, the verdict
// republish's check run at crMergeSHA names no merge_candidate_* kind.
func TestAuditComplete_MergeCandidatePassedClears(t *testing.T) {
	sd := mcAuditCompleteSeed(t, mcvUndelegatedSpecYAML)
	shipConflictResolutionPushed(t, sd)
	startConflictResolutionVerifyPass(sd)

	_, resp := shipMergeCandidateReport(t, sd, mcvReportBody(crMergeSHA, mergeCandidateResultPassed, "", "ok"))
	if resp.Result != mergeCandidateResultPassed || resp.HeadSHA != crMergeSHA {
		t.Fatalf("response = %+v, want a passed verdict for %s", resp, crMergeSHA)
	}
	calls := checkRunCalls(sd.creator)
	if len(calls) != 1 || calls[0].HeadSHA != crMergeSHA {
		t.Fatalf("check runs = %v, want exactly one at %s", checkRunHeads(sd.creator), crMergeSHA)
	}
	assertNoMergeCandidateKind(t, calls[0], "a passed verify for exactly the live head clears rule 8")
	if got := mcKinds(computeAuditComplete(t, sd).Missing); len(got) != 0 {
		t.Errorf("recompute merge_candidate kinds = %v, want none after a pass for the live head", got)
	}
}

// TestAuditComplete_MergeCandidateFailedVerify: a failed report for crMergeSHA
// makes the recompute carry the hard merge_candidate_verify_failed kind.
func TestAuditComplete_MergeCandidateFailedVerify(t *testing.T) {
	sd := mcAuditCompleteSeed(t, mcvUndelegatedSpecYAML)
	shipConflictResolutionPushed(t, sd)
	startConflictResolutionVerifyPass(sd)

	_, resp := shipMergeCandidateReport(t, sd, mcvReportBody(crMergeSHA, mergeCandidateResultFailed, "verify red", "FAIL"))
	if resp.Result != mergeCandidateResultFailed {
		t.Fatalf("recorded result = %q, want failed", resp.Result)
	}
	if !sd.impl.State.IsTerminal() {
		t.Fatalf("implement stage = %s after the failed report; the recompute would stop at stage_not_terminal", sd.impl.State)
	}
	res := computeAuditComplete(t, sd)
	item := mcItem(res.Missing, auditcomplete.MissingMergeCandidateVerifyFailed)
	if item == nil {
		t.Fatalf("missing = %+v, want merge_candidate_verify_failed for the failed head", res.Missing)
	}
	if !strings.Contains(item.Detail, crMergeSHA) {
		t.Errorf("detail = %q, want it to name %s", item.Detail, crMergeSHA)
	}
}

// TestAuditComplete_MergeCandidateNoVerifyCommand_NoItem is the ADR-090 D4
// regression guard: the same fixture with a spec declaring no verify command
// carries no merge_candidate_* item.
func TestAuditComplete_MergeCandidateNoVerifyCommand_NoItem(t *testing.T) {
	sd := mcAuditCompleteSeed(t, mcvNoVerifySpecYAML)
	shipConflictResolutionPushed(t, sd)
	sd.impl.State = run.StageStateSucceeded

	if got := mcKinds(computeAuditComplete(t, sd).Missing); len(got) != 0 {
		t.Errorf("merge_candidate kinds = %v, want none when no verify command is declared (D4)", got)
	}
}

// TestAuditComplete_MergeCandidatePassForOtherHead_StillUnverified pins the
// exact-head binding: a passed verdict recorded for the agent head
// crAgentHeadSHA does not clear the live head crMergeSHA.
func TestAuditComplete_MergeCandidatePassForOtherHead_StillUnverified(t *testing.T) {
	sd := mcAuditCompleteSeed(t, mcvUndelegatedSpecYAML)
	shipConflictResolutionPushed(t, sd)
	sd.impl.State = run.StageStateSucceeded

	raw, _ := json.Marshal(mergeCandidateVerifiedPayload{
		HeadSHA: crAgentHeadSHA, Cause: mergeCandidateCauseConflictResolution,
		VerifyCommand: mcvCommand, Result: mergeCandidateResultPassed,
	})
	implID := sd.impl.ID
	sd.au.mu.Lock()
	sd.au.appended = append(sd.au.appended, audit.ChainAppendParams{
		RunID: sd.runID, StageID: &implID, Category: CategoryMergeCandidateVerified, Payload: raw,
	})
	sd.au.mu.Unlock()

	res := computeAuditComplete(t, sd)
	item := mcItem(res.Missing, auditcomplete.MissingMergeCandidateUnverified)
	if item == nil {
		t.Fatalf("missing = %+v, want merge_candidate_unverified: a pass for %s must not clear %s", res.Missing, crAgentHeadSHA, crMergeSHA)
	}
	if !strings.Contains(item.Detail, crMergeSHA) {
		t.Errorf("detail = %q, want it to name the live head %s", item.Detail, crMergeSHA)
	}
}

// TestAuditComplete_MergeCandidateStateConstantsMatchSeam pins the server's
// state spellings to the auditcomplete seam they alias.
func TestAuditComplete_MergeCandidateStateConstantsMatchSeam(t *testing.T) {
	for server, seam := range map[string]auditcomplete.MergeCandidateStatus{
		mergeCandidateStateNotRequired: auditcomplete.MergeCandidateNotRequired,
		mergeCandidateStateUnverified:  auditcomplete.MergeCandidateUnverified,
		mergeCandidateStateInFlight:    auditcomplete.MergeCandidateInFlight,
		mergeCandidateStatePassed:      auditcomplete.MergeCandidatePassed,
		mergeCandidateStateFailed:      auditcomplete.MergeCandidateFailed,
	} {
		if server != string(seam) {
			t.Errorf("server state %q != auditcomplete seam %q", server, seam)
		}
	}
	for want, got := range map[string]string{
		"not_required": mergeCandidateStateNotRequired,
		"unverified":   mergeCandidateStateUnverified,
		"in_flight":    mergeCandidateStateInFlight,
		"passed":       mergeCandidateStatePassed,
		"failed":       mergeCandidateStateFailed,
	} {
		if got != want {
			t.Errorf("state spelling = %q, want %q", got, want)
		}
	}
}
