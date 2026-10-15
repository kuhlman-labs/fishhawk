package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// E86.57 / #4397 (and the #4440 variant): fishhawk_audit_complete is published
// AT the head a merge-candidate verify pass verified, and the pre-merge
// republish lands AT the head the merge gate admitted. Every test here asserts
// the Check Run parameters the forge fake RECEIVED (vouchCheckCreator.calls),
// never that a helper was called.
//
// Fixture adaptations live in THIS file (approval condition C2): the shared
// fixtures in conflictresolution_test.go, merge_run_test.go,
// merge_candidate_verify_test.go, autodrive_test.go and vouch_test.go are
// reused unchanged and only wrapped here.

// checkRunCalls snapshots the check runs the forge fake received.
func checkRunCalls(c *vouchCheckCreator) []forge.CreateCheckRunParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]forge.CreateCheckRunParams(nil), c.calls...)
}

// checkRunHeads returns the head sha of every received check run, in order.
func checkRunHeads(c *vouchCheckCreator) []string {
	var out []string
	for _, p := range checkRunCalls(c) {
		out = append(out, p.HeadSHA)
	}
	return out
}

// shipConflictResolutionPushed ships the runner's signed conflict-resolution
// report at crMergeSHA — the writer half of the #3673 seam.
func shipConflictResolutionPushed(t *testing.T, sd *crSeed) {
	t.Helper()
	priv, _ := sd.sf.issue(t, sd.runID)
	report := []byte(`{"outcome":"conflict_resolution_pushed","branch":"` + rebaseBranchName +
		`","head_sha":"` + crMergeSHA + `","base_sha":"` + crAgentHeadSHA + `","files_changed_count":2}`)
	if w := shipPRRequest(t, sd.s, sd.runID, sd.impl.ID, priv, report, ""); w.Code != http.StatusOK {
		t.Fatalf("conflict-resolution report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// startConflictResolutionVerifyPass is the state the merge-candidate verify
// pass for the conflict-resolution head leaves: the implement stage re-opened
// to `running` and a stage-scoped trigger expecting crMergeSHA, recorded as
// having interrupted a SUCCEEDED stage. Appended directly (by construction)
// rather than through startMergeCandidateVerifyPass, which needs a workflow
// spec this fixture does not carry.
func startConflictResolutionVerifyPass(sd *crSeed) {
	sd.impl.State = run.StageStateRunning
	raw, _ := json.Marshal(mergeCandidateVerifyTrigger{
		Branch: mcvBranch, BaseRef: rebaseBaseRef, ExpectedHeadSHA: crMergeSHA,
		Cause: mergeCandidateCauseConflictResolution, VerifyCommand: mcvCommand,
		PriorState: string(run.StageStateSucceeded),
	})
	stageID := sd.impl.ID
	sd.au.mu.Lock()
	defer sd.au.mu.Unlock()
	sd.au.appended = append(sd.au.appended, audit.ChainAppendParams{
		RunID: sd.runID, StageID: &stageID,
		Category: CategoryStageMergeCandidateVerifyTriggered, Payload: raw,
	})
}

// shipMergeCandidateReport ships a signed merge_candidate_verified report.
func shipMergeCandidateReport(t *testing.T, sd *crSeed, body []byte) (*http.Response, pullRequestMergeCandidateResponse) {
	t.Helper()
	priv, _ := sd.sf.issue(t, sd.runID)
	w := shipPRRequest(t, sd.s, sd.runID, sd.impl.ID, priv, body, "")
	var resp pullRequestMergeCandidateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK {
		t.Fatalf("merge-candidate report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	return w.Result(), resp
}

// seedConflictResolutionVerifyPass wraps seedConflictResolutionE2E with the
// sequence stamping liveMergeCandidateTrigger's trigger-consumption compare
// needs, ships the conflict-resolution report, and starts the verify pass.
func seedConflictResolutionVerifyPass(t *testing.T) *crSeed {
	t.Helper()
	sd := seedConflictResolutionE2E(t)
	sd.au.stampSequence = true
	shipConflictResolutionPushed(t, sd)
	startConflictResolutionVerifyPass(sd)
	return sd
}

// assertCheckOutputOmits fails when either output field of p names kind.
func assertCheckOutputOmits(t *testing.T, p forge.CreateCheckRunParams, kind auditcomplete.MissingKind, why string) {
	t.Helper()
	for _, field := range []struct{ name, text string }{
		{"OutputSummary", p.OutputSummary},
		{"OutputText", p.OutputText},
	} {
		if strings.Contains(field.text, string(kind)) {
			t.Errorf("check run %s names %s (%s):\n%s", field.name, kind, why, field.text)
		}
	}
}

// TestConflictResolutionPushed_VerifyPassedPostsAuditCheckAtMergeHead is the
// #4397 done-means, end to end: the runner's signed conflict_resolution_pushed
// report at crMergeSHA, then its signed merge_candidate_verified `passed`
// report for that head, through the audit chain → settleMergeCandidateVerifyStage
// → auditcomplete.ComputeResult → auditcheckpublisher → the forge fake. A
// fishhawk_audit_complete check run must then EXIST at crMergeSHA with no
// fishhawk_rebase_run_branch invocation anywhere in the test.
//
// Approval condition C1: this fixture's audit fake carries no EntryHash and no
// trace rows, so the recomputed state is a FAILURE on the chain/trace rules
// (observed: status completed, conclusion failure) and the conclusion is not
// asserted. What is decidable is pinned: the check run is ABSENT before the
// verified ship, EXISTS at crMergeSHA after it, is not in_progress and carries
// no stage_not_terminal item (the ordering control: the recompute observed the
// implement stage already settled), and carries no foreign_commit item (rule 5
// knows crMergeSHA through the conflict_resolution_pushed union).
//
// Counterfactuals (run by hand): deleting the
// republishAuditCheckOnMergeCandidateVerdict call in
// succeedMergeCandidateVerifiedStage leaves zero check runs → RED on the
// existence assertion; hoisting it above the settle posts status in_progress
// with a stage_not_terminal item (implement stage `running`) → RED on both
// the status and the stage_not_terminal assertion.
func TestConflictResolutionPushed_VerifyPassedPostsAuditCheckAtMergeHead(t *testing.T) {
	sd := seedConflictResolutionVerifyPass(t)

	// ABSENT before: the conflict report publishes nothing, and findHeadSHA
	// could only ever resolve the stale artifact head crAgentHeadSHA.
	if heads := checkRunHeads(sd.creator); len(heads) != 0 {
		t.Fatalf("check runs before the verified ship = %v, want none", heads)
	}

	_, resp := shipMergeCandidateReport(t, sd, mcvReportBody(crMergeSHA, mergeCandidateResultPassed, "", "ok"))
	if resp.Result != mergeCandidateResultPassed || resp.HeadSHA != crMergeSHA || resp.Idempotent {
		t.Fatalf("response = %+v, want a fresh passed verdict for %s", resp, crMergeSHA)
	}
	if got := sd.impl.State; got != run.StageStateSucceeded {
		t.Fatalf("implement stage = %s, want succeeded (the trigger's prior state)", got)
	}

	calls := checkRunCalls(sd.creator)
	if len(calls) != 1 {
		t.Fatalf("check runs after the verified ship = %d (%v), want exactly 1", len(calls), checkRunHeads(sd.creator))
	}
	if calls[0].HeadSHA != crMergeSHA {
		t.Errorf("check run head_sha = %q, want the conflict-resolution head %q (not the stale artifact head %q)",
			calls[0].HeadSHA, crMergeSHA, crAgentHeadSHA)
	}
	if calls[0].Name != "fishhawk_audit_complete" {
		t.Errorf("check run name = %q, want fishhawk_audit_complete", calls[0].Name)
	}
	if calls[0].Status == forge.CheckRunStatusInProgress {
		t.Errorf("check run status = in_progress, want a settled state: the recompute saw the implement stage before the settle")
	}
	assertCheckOutputOmits(t, calls[0], auditcomplete.MissingStageNotTerminal,
		"the recompute ran before the implement stage was settled to its prior gate")
	assertCheckOutputOmits(t, calls[0], auditcomplete.MissingForeignCommit,
		"the conflict-resolution head is the runner's own commit (#3673)")
	if n := len(auditEntries(sd.au, CategoryBranchRebased)); n != 0 {
		t.Errorf("branch_rebased rows = %d, want 0 (no rebase verb was invoked)", n)
	}
}

// TestMergeCandidateVerified_NonPassedResultsPostNoCheck pins the passed-only
// rule: a failed result, a not_executed result, and a `passed` report for a
// head the trigger did not expect (recorded as not_executed head_mismatch)
// each post NO check run at any head, while the report still returns 200 and
// the verdict row is recorded. Counterfactual: `if result !=
// mergeCandidateResultPassed {` → `if false {` lands a check run → RED.
func TestMergeCandidateVerified_NonPassedResultsPostNoCheck(t *testing.T) {
	const otherHead = "eeee555555555555555555555555555555555555"
	cases := map[string]struct {
		head, result, reason string
		wantResult           string
	}{
		"failed":        {crMergeSHA, mergeCandidateResultFailed, "verify red", mergeCandidateResultFailed},
		"not_executed":  {crMergeSHA, mergeCandidateResultNotExecuted, "runner could not check out the head", mergeCandidateResultNotExecuted},
		"head_mismatch": {otherHead, mergeCandidateResultPassed, "", mergeCandidateResultNotExecuted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sd := seedConflictResolutionVerifyPass(t)

			_, resp := shipMergeCandidateReport(t, sd, mcvReportBody(tc.head, tc.result, tc.reason, "tail"))
			if resp.Result != tc.wantResult {
				t.Fatalf("recorded result = %q, want %q", resp.Result, tc.wantResult)
			}
			if rows := auditEntries(sd.au, CategoryMergeCandidateVerified); len(rows) != 1 {
				t.Fatalf("merge_candidate_verified rows = %d, want 1", len(rows))
			}
			if heads := checkRunHeads(sd.creator); len(heads) != 0 {
				t.Errorf("a %s report posted check runs at %v, want none (passed-only)", name, heads)
			}
		})
	}
}

// TestMergeCandidateVerified_DuplicateReplayPostsNothingNew pins the
// idempotent-replay placement: the republish sits after the rec.Duplicate
// return, so a runner retry of the identical passed report answers
// idempotent:true and posts no second check run.
func TestMergeCandidateVerified_DuplicateReplayPostsNothingNew(t *testing.T) {
	sd := seedConflictResolutionVerifyPass(t)
	body := mcvReportBody(crMergeSHA, mergeCandidateResultPassed, "", "ok")

	shipMergeCandidateReport(t, sd, body)
	if n := len(checkRunCalls(sd.creator)); n != 1 {
		t.Fatalf("check runs after the first ship = %d, want 1", n)
	}

	_, resp := shipMergeCandidateReport(t, sd, body)
	if !resp.Idempotent {
		t.Fatalf("replay response = %+v, want idempotent:true", resp)
	}
	if heads := checkRunHeads(sd.creator); len(heads) != 1 {
		t.Errorf("check runs after the replay = %v, want still exactly 1", heads)
	}
}

// TestMergeCandidateVerified_PublishFailureDoesNotUnwind pins best-effort: a
// forge error on the at-head publish leaves the report 200, the verdict row
// recorded and the implement stage settled to its prior gate.
func TestMergeCandidateVerified_PublishFailureDoesNotUnwind(t *testing.T) {
	sd := seedConflictResolutionVerifyPass(t)
	sd.creator.err = errors.New("forge unavailable")

	_, resp := shipMergeCandidateReport(t, sd, mcvReportBody(crMergeSHA, mergeCandidateResultPassed, "", "ok"))
	if resp.Result != mergeCandidateResultPassed {
		t.Fatalf("recorded result = %q, want passed", resp.Result)
	}
	// Anti-vacuity: the publish was attempted, so the failure branch ran.
	if heads := checkRunHeads(sd.creator); len(heads) != 1 || heads[0] != crMergeSHA {
		t.Fatalf("attempted check runs = %v, want one at %s", heads, crMergeSHA)
	}
	if rows := auditEntries(sd.au, CategoryMergeCandidateVerified); len(rows) != 1 {
		t.Errorf("merge_candidate_verified rows = %d, want 1 (the verdict is durable)", len(rows))
	}
	if got := sd.impl.State; got != run.StageStateSucceeded {
		t.Errorf("implement stage = %s, want succeeded (the settle is not unwound)", got)
	}
}

// wireAuditCheckCreator attaches an artifact repository holding a
// pull_request artifact at recordedHead on implStageID and an audit-check
// publisher over a recording creator — what findHeadSHA would resolve, and
// what the forge fake receives.
func wireAuditCheckCreator(t *testing.T, s *Server, implStageID uuid.UUID, recordedHead string) *vouchCheckCreator {
	t.Helper()
	arts := newFakeArtifactRepo()
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: implStageID,
		Kind: artifact.KindPullRequest, Content: pullRequestArtifactBody(recordedHead),
	})
	s.cfg.ArtifactRepo = arts
	creator := &vouchCheckCreator{}
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      creator,
		Runs:        s.cfg.RunRepo,
		Artifacts:   arts,
		Audit:       s.cfg.AuditRepo,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	return creator
}

// assertAllChecksAt fails unless at least one check run was received and
// every one is at want.
func assertAllChecksAt(t *testing.T, c *vouchCheckCreator, want, stale string) {
	t.Helper()
	heads := checkRunHeads(c)
	if len(heads) == 0 {
		t.Fatalf("no check run was posted; want one at the gate-admitted live head %s", want)
	}
	for _, h := range heads {
		if h != want {
			t.Errorf("check run posted at %s, want the gate-admitted live head %s (the recorded head is %s)", h, want, stale)
		}
	}
}

// TestMergeRun_BeforeMergeRepublishTargetsGateAdmittedLiveHead pins the
// operator merge endpoint: the merge-candidate gate admits the live PR head
// mcvHead (passed verdict for exactly it, behind 0), while the recorded
// pull_request artifact names mcvOtherHead. The forced pre-dispatch republish
// must land at mcvHead. Counterfactuals: the gate's final admit returning
// mergeCandidateGateResult{} or handleMergeRun passing "" lands it at
// mcvOtherHead → RED.
func TestMergeRun_BeforeMergeRepublishTargetsGateAdmittedLiveHead(t *testing.T) {
	g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
	g.seedRebased(mcvHead, mcvHead, false)
	g.seedTrigger(mcvHead)
	g.seedVerdict(mcvHead, mergeCandidateResultPassed)
	creator := wireAuditCheckCreator(t, g.s, g.impl.ID, mcvOtherHead)

	g.assertAdmitted(t, g.post(t))
	assertAllChecksAt(t, creator, mcvHead, mcvOtherHead)
}

// newGatedMergeAtHeadServer wraps newGatedMergeRepublishServer (whose recorded
// artifact head is abc12345) with its publisher swapped onto a recording
// creator, so the head each publish lands at is observable.
func newGatedMergeAtHeadServer(t *testing.T) (*Server, *run.Run, []*run.Stage, *vouchCheckCreator) {
	t.Helper()
	s, r, stages := newGatedMergeRepublishServer(t, &mergeOrderLog{}, nil)
	creator := &vouchCheckCreator{}
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      creator,
		Runs:        s.cfg.RunRepo,
		Artifacts:   s.cfg.ArtifactRepo,
		Audit:       s.cfg.AuditRepo,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	return s, r, stages, creator
}

// TestDispatchAcceptanceGatedMerge_RepublishesAtLiveHead pins the shared tail:
// a non-empty liveHead is where the publish lands.
func TestDispatchAcceptanceGatedMerge_RepublishesAtLiveHead(t *testing.T) {
	const liveHead = "feedfacefeedfacefeedfacefeedfacefeedface"
	s, r, stages, creator := newGatedMergeAtHeadServer(t)
	merger := &fakeMerger{}

	outcome, gateState, err := s.dispatchAcceptanceGatedMerge(context.Background(), r, stages, merger, liveHead)
	if err != nil || outcome != mergeDispatchMerged {
		t.Fatalf("outcome = %v (gate %q, err %v), want mergeDispatchMerged", outcome, gateState, err)
	}
	assertAllChecksAt(t, creator, liveHead, "abc12345")
}

// TestDispatchAcceptanceGatedMerge_EmptyLiveHeadKeepsRecordedHead pins the
// fallback arm: an empty liveHead (ungated D7 admit, merged PR) keeps the
// findHeadSHA resolution — the recorded artifact head.
func TestDispatchAcceptanceGatedMerge_EmptyLiveHeadKeepsRecordedHead(t *testing.T) {
	s, r, stages, creator := newGatedMergeAtHeadServer(t)
	merger := &fakeMerger{}

	outcome, gateState, err := s.dispatchAcceptanceGatedMerge(context.Background(), r, stages, merger, "")
	if err != nil || outcome != mergeDispatchMerged {
		t.Fatalf("outcome = %v (gate %q, err %v), want mergeDispatchMerged", outcome, gateState, err)
	}
	if heads := checkRunHeads(creator); len(heads) != 1 || heads[0] != "abc12345" {
		t.Errorf("check runs = %v, want exactly one at the recorded head abc12345", heads)
	}
}

// TestAutoDrive_MayMerge_RepublishesAtGateAdmittedLiveHead is approval
// condition C4: the PRODUCTION may_merge arm driven end to end through the
// auto-drive HTTP handler — delegation evaluation, mergeGateReady, the review
// coverage hold, the merge-candidate gate against the GitHub stub (live head
// mcvHead, passed verdict for exactly it), the acceptance gate, the forced
// pre-merge republish and the merge dispatch. The recorded pull_request
// artifact names mcvOtherHead, so the publish lands at the gate-admitted head
// only if the arm threads gate.HeadSHA. Counterfactual: passing "" from the
// arm lands it at mcvOtherHead → RED.
func TestAutoDrive_MayMerge_RepublishesAtGateAdmittedLiveHead(t *testing.T) {
	s, au, runRow, _ := seedMayMergeCandidateRun(t, autoDriveVerifySpecYAML)
	seedAutoDriveChainRow(au, runRow.ID, CategoryBranchRebased, map[string]any{"new_head_sha": mcvHead})
	seedAutoDriveChainRow(au, runRow.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: mcvHead, Cause: mergeCandidateCauseBaseAdvance, Result: mergeCandidateResultPassed})
	merger := &fakeMerger{}
	s.cfg.GateMerger = merger

	stages, err := s.cfg.RunRepo.ListStagesForRun(context.Background(), runRow.ID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	var implID uuid.UUID
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			implID = st.ID
		}
	}
	if implID == uuid.Nil {
		t.Fatal("fixture has no implement stage")
	}
	creator := wireAuditCheckCreator(t, s, implID, mcvOtherHead)

	w := autoDrivePost(t, s, s.handleAutoDrive, runRow.ID, "", "{}", autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var out autoDriveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge", out)
	}
	if merger.called != 1 {
		t.Fatalf("merge dispatched %d times, want 1", merger.called)
	}
	assertAllChecksAt(t, creator, mcvHead, mcvOtherHead)
}
