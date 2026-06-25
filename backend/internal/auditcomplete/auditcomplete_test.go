package auditcomplete_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// happyPath builds a fully-complete run: plan + implement + review
// stages, all terminal, with the required artifacts and chained
// audit entries. Each test that expects a pass starts here and
// mutates one piece to assert the failure mode.
func happyPath(t *testing.T) (uuid.UUID, *fakeRuns, *fakeArtifacts, *fakeAudit) {
	t.Helper()
	runID := uuid.New()
	plan := mkStage(runID, 1, run.StageTypePlan, run.StageStateSucceeded)
	impl := mkStage(runID, 2, run.StageTypeImplement, run.StageStateSucceeded)
	rev := mkStage(runID, 3, run.StageTypeReview, run.StageStateAwaitingApproval)

	runs := &fakeRuns{stages: []*run.Stage{plan, impl, rev}}
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			plan.ID: {planArtifact(plan.ID, "standard_v1")},
			impl.ID: {pullRequestArtifact(impl.ID)},
		},
	}
	auditRepo := &fakeAudit{}
	auditRepo.appendChained(t, runID, &plan.ID, "stage_dispatched", nil)
	auditRepo.appendChained(t, runID, &plan.ID, "trace_uploaded", traceVariantPayload("raw"))
	auditRepo.appendChained(t, runID, &plan.ID, "trace_uploaded", traceVariantPayload("redacted"))
	auditRepo.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("raw"))
	auditRepo.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("redacted"))

	return runID, runs, arts, auditRepo
}

func TestCompute_AllRulesPass(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass; missing=%v", state, missing)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing items; got %+v", missing)
	}
}

func TestCompute_PendingWhenStageMidFlight(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	// Implement stage hasn't terminated yet.
	runs.stages[1].State = run.StageStateRunning
	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending", state)
	}
	if len(missing) != 0 {
		t.Fatalf("missing should be empty during pending; got %+v", missing)
	}
}

func TestCompute_FailWhenPlanMissing(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	planID := runs.stages[0].ID
	delete(arts.byStage, planID)
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingPlan) {
		t.Fatalf("missing did not include plan_missing: %+v", missing)
	}
}

func TestCompute_FailWhenPlanWrongSchemaVersion(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	planID := runs.stages[0].ID
	arts.byStage[planID] = []*artifact.Artifact{planArtifact(planID, "draft_v0")}
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingPlan) {
		t.Fatalf("missing did not include plan_missing: %+v", missing)
	}
}

func TestCompute_FailWhenRedactedTraceMissing(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	implID := runs.stages[1].ID
	// Drop the implement stage's redacted trace entry.
	ar.dropEntry(func(e *audit.Entry) bool {
		if e.StageID == nil || *e.StageID != implID {
			return false
		}
		if e.Category != "trace_uploaded" {
			return false
		}
		return string(e.Payload) == string(traceVariantPayload("redacted"))
	})
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingTrace) {
		t.Fatalf("missing did not include trace_missing: %+v", missing)
	}
}

func TestCompute_FailWhenStageHasNoTraceEntry(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	implID := runs.stages[1].ID
	ar.dropEntry(func(e *audit.Entry) bool {
		return e.StageID != nil && *e.StageID == implID && e.Category == "trace_uploaded"
	})
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingTrace) {
		t.Fatalf("missing did not include trace_missing: %+v", missing)
	}
}

func TestCompute_FailWhenPullRequestMissing(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	implID := runs.stages[1].ID
	delete(arts.byStage, implID)
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingPullRequest) {
		t.Fatalf("missing did not include pr_missing: %+v", missing)
	}
}

func TestCompute_FailWhenChainTampered(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	// Mutate the second entry's payload after-the-fact: this
	// breaks the recomputed hash without changing the stored
	// EntryHash.
	ar.entries[1].Payload = json.RawMessage(`{"variant":"raw","tampered":true}`)
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingChain) {
		t.Fatalf("missing did not include chain_invalid: %+v", missing)
	}
}

func TestCompute_PassWithoutPlanStage(t *testing.T) {
	// routine_change-shaped run: implement only, no plan, no review.
	runID := uuid.New()
	impl := mkStage(runID, 1, run.StageTypeImplement, run.StageStateSucceeded)
	runs := &fakeRuns{stages: []*run.Stage{impl}}
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		impl.ID: {pullRequestArtifact(impl.ID)},
	}}
	ar := &fakeAudit{}
	ar.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("raw"))
	ar.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("redacted"))

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass; missing=%+v", state, missing)
	}
}

func TestCompute_ListStagesError(t *testing.T) {
	runID := uuid.New()
	runs := &fakeRuns{listErr: errors.New("db down")}
	state, _, err := auditcomplete.Compute(context.Background(), runID, deps(runs, &fakeArtifacts{}, &fakeAudit{}))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending on error path", state)
	}
}

func TestCompute_NilDeps(t *testing.T) {
	_, _, err := auditcomplete.Compute(context.Background(), uuid.New(), auditcomplete.Deps{})
	if err == nil {
		t.Fatalf("expected error from nil deps")
	}
}

// --- Rule 5 (foreign commit, #282) ---

// foreignCommitSetup builds a run with a populated PR artifact +
// known head_sha + installation. The four tests below mutate one
// piece (the live PRHead callback, the parent chain, etc.) to drive
// the behavior they care about. Returns the runID + the fake repos
// + the canonical recorded head_sha.
func foreignCommitSetup(t *testing.T) (uuid.UUID, *fakeRuns, *fakeArtifacts, *fakeAudit, string) {
	t.Helper()
	runID := uuid.New()
	plan := mkStage(runID, 1, run.StageTypePlan, run.StageStateSucceeded)
	impl := mkStage(runID, 2, run.StageTypeImplement, run.StageStateSucceeded)
	rev := mkStage(runID, 3, run.StageTypeReview, run.StageStateAwaitingApproval)

	const recordedSHA = "abc123def4567890abc123def4567890abc12345"
	const prNumber = 275

	installID := int64(99)
	runs := &fakeRuns{
		stages: []*run.Stage{plan, impl, rev},
		runs: map[uuid.UUID]*run.Run{
			runID: {
				ID:             runID,
				Repo:           "kuhlman-labs/fishhawk",
				WorkflowID:     "feature_change",
				InstallationID: &installID,
			},
		},
	}
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			plan.ID: {planArtifact(plan.ID, "standard_v1")},
			impl.ID: {pullRequestArtifactWithBody(impl.ID, recordedSHA, prNumber)},
		},
	}
	auditRepo := &fakeAudit{}
	auditRepo.appendChained(t, runID, &plan.ID, "trace_uploaded", traceVariantPayload("raw"))
	auditRepo.appendChained(t, runID, &plan.ID, "trace_uploaded", traceVariantPayload("redacted"))
	auditRepo.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("raw"))
	auditRepo.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("redacted"))

	return runID, runs, arts, auditRepo, recordedSHA
}

func TestCompute_Rule5_PRHeadMatches_Passes(t *testing.T) {
	// Live HEAD matches the recorded artifact head_sha → pass.
	// This is the canonical happy path; everything other rules say
	// pass should keep passing once rule 5 lands.
	runID, runs, arts, ar, recordedSHA := foreignCommitSetup(t)
	d := auditcomplete.Deps{
		Runs: runs, Artifacts: arts, Audit: ar,
		PRHead: stubPRHead(t, recordedSHA, nil),
	}
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Errorf("state = %s want pass; missing=%+v", state, missing)
	}
}

func TestCompute_Rule5_ForeignCommit_Fails(t *testing.T) {
	// Live HEAD differs from the recorded SHA — exactly the case
	// the user reproduced by pushing a prettier fix directly to PR
	// #275 (which then had `fishhawk_audit_complete` stay green
	// because the rule didn't exist yet). Pre-#282 we'd render
	// pass; post-#282 the drift fails the rule with the foreign-
	// commit kind and both shas in the detail.
	runID, runs, arts, ar, _ := foreignCommitSetup(t)
	const foreignSHA = "deadbeef1111deadbeef1111deadbeef11111111"
	d := auditcomplete.Deps{
		Runs: runs, Artifacts: arts, Audit: ar,
		PRHead: stubPRHead(t, foreignSHA, nil),
	}
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s, want fail; missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingForeignCommit) {
		t.Errorf("expected foreign_commit missing item; got %+v", missing)
	}
	// Detail names both the observed and known shas so a reviewer
	// can identify the drift without cross-referencing.
	var detail string
	for _, m := range missing {
		if m.Kind == auditcomplete.MissingForeignCommit {
			detail = m.Detail
		}
	}
	if !strings.Contains(detail, foreignSHA[:7]) {
		t.Errorf("detail should name observed sha %q: %s", foreignSHA[:7], detail)
	}
}

func TestCompute_Rule5_LiveFetchFailure_Pending(t *testing.T) {
	// GitHub fetch errors must NOT flip the audit to fail — that
	// produces a flapping signal on every transient outage. The
	// rule emits a `head_fetch_failed` missing item and the overall
	// state demotes to pending so branch protection re-evaluates
	// on the next successful publish.
	runID, runs, arts, ar, _ := foreignCommitSetup(t)
	d := auditcomplete.Deps{
		Runs: runs, Artifacts: arts, Audit: ar,
		PRHead: stubPRHead(t, "", errors.New("rate limited")),
	}
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePending {
		t.Errorf("state = %s want pending (head_fetch_failed shouldn't fail audit); missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingHeadFetchFail) {
		t.Errorf("expected head_fetch_failed missing item; got %+v", missing)
	}
}

func TestCompute_Rule5_NilPRHead_SkipsCleanly(t *testing.T) {
	// Dev / CLI posture: no GitHub client wired. Compute MUST skip
	// rule 5 cleanly — other rules still evaluate, the overall
	// state is the rest-of-the-audit's verdict (pass here).
	runID, runs, arts, ar, _ := foreignCommitSetup(t)
	d := auditcomplete.Deps{Runs: runs, Artifacts: arts, Audit: ar} // PRHead nil
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Errorf("state = %s want pass (nil PRHead skips rule 5); missing=%+v", state, missing)
	}
}

// --- Rule 6 (review-pending presence gate, #947) ---

func TestReviewPresent(t *testing.T) {
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	started := func(ts ...time.Time) []*audit.Entry {
		out := make([]*audit.Entry, 0, len(ts))
		for _, t := range ts {
			out = append(out, &audit.Entry{Timestamp: t})
		}
		return out
	}
	cases := []struct {
		name         string
		in           auditcomplete.ReviewPresenceInputs
		wantPresent  bool
		wantBackstop bool
	}{
		{
			name:        "no agent reviewer configured",
			in:          auditcomplete.ReviewPresenceInputs{ReviewersAgent: 0, Started: started(base), TerminalCount: 0, Backstop: time.Hour, Now: base},
			wantPresent: true,
		},
		{
			name:        "configured but never dispatched",
			in:          auditcomplete.ReviewPresenceInputs{ReviewersAgent: 1, Started: nil, TerminalCount: 0, Backstop: time.Hour, Now: base},
			wantPresent: true,
		},
		{
			name:        "terminal count meets configured agents",
			in:          auditcomplete.ReviewPresenceInputs{ReviewersAgent: 2, Started: started(base), TerminalCount: 2, Backstop: time.Hour, Now: base.Add(time.Minute)},
			wantPresent: true,
		},
		{
			name:        "dispatched, no terminal, within backstop -> in-flight",
			in:          auditcomplete.ReviewPresenceInputs{ReviewersAgent: 1, Started: started(base), TerminalCount: 0, Backstop: time.Hour, Now: base.Add(30 * time.Minute)},
			wantPresent: false,
		},
		{
			name:         "dispatched, no terminal, past backstop -> present + degrade",
			in:           auditcomplete.ReviewPresenceInputs{ReviewersAgent: 1, Started: started(base), TerminalCount: 0, Backstop: time.Hour, Now: base.Add(2 * time.Hour)},
			wantPresent:  true,
			wantBackstop: true,
		},
		{
			name:         "backstop anchors on EARLIEST dispatch",
			in:           auditcomplete.ReviewPresenceInputs{ReviewersAgent: 2, Started: started(base.Add(90*time.Minute), base), TerminalCount: 1, Backstop: time.Hour, Now: base.Add(80 * time.Minute)},
			wantPresent:  true,
			wantBackstop: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, backstop := auditcomplete.ReviewPresent(tc.in)
			if present != tc.wantPresent {
				t.Errorf("present = %v want %v", present, tc.wantPresent)
			}
			if backstop != tc.wantBackstop {
				t.Errorf("backstopElapsed = %v want %v", backstop, tc.wantBackstop)
			}
		})
	}
}

// reviewPendingSetup extends happyPath with a run row carrying a configured
// implement-stage reviewers.agent count, and returns the implement stage's
// dispatch timestamp anchor used to drive the backstop branch deterministically.
func reviewPendingSetup(t *testing.T, agent int) (uuid.UUID, *fakeRuns, *fakeArtifacts, *fakeAudit) {
	t.Helper()
	runID, runs, arts, ar := happyPath(t)
	// happyPath leaves runs.runs nil; seed a row so GetRun returns a real
	// run for the ImplementReviewers closure (its content is irrelevant —
	// the test closure ignores it and returns a fixed config).
	runs.runs = map[uuid.UUID]*run.Run{runID: {ID: runID}}
	return runID, runs, arts, ar
}

// reviewDeps wires the rule-6 closures onto a base Deps. now==zero leaves
// Deps.Now nil (defaults to time.Now); a non-zero now injects a fixed clock.
func reviewDeps(r *fakeRuns, a *fakeArtifacts, au *fakeAudit, agent int, backstop time.Duration, now time.Time) auditcomplete.Deps {
	d := deps(r, a, au)
	d.ImplementReviewers = func(*run.Run) *spec.ReviewersConfig { return &spec.ReviewersConfig{Agent: agent} }
	d.ReviewBackstop = func(int) time.Duration { return backstop }
	if !now.IsZero() {
		fixed := now
		d.Now = func() time.Time { return fixed }
	}
	return d
}

// dispatchTime mirrors the fakeAudit timestamp formula so tests can anchor an
// injected clock relative to the implement_review_started entry.
func dispatchTime(seq int) time.Time {
	return time.Date(2026, 5, 7, 12, 0, seq, 0, time.UTC)
}

func TestCompute_Rule6_PendingWhenReviewDispatchedNotLanded(t *testing.T) {
	runID, runs, arts, ar := reviewPendingSetup(t, 1)
	implID := runs.stages[1].ID
	ar.appendChained(t, runID, &implID, "implement_review_started", nil) // seq 5
	// Within backstop: clock just after dispatch.
	d := reviewDeps(runs, arts, ar, 1, time.Hour, dispatchTime(5).Add(time.Minute))
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending (review_pending is the only miss); missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingReviewPending) {
		t.Fatalf("missing did not include review_pending: %+v", missing)
	}
}

func TestCompute_Rule6_ClearsWhenTerminalReached(t *testing.T) {
	runID, runs, arts, ar := reviewPendingSetup(t, 1)
	implID := runs.stages[1].ID
	ar.appendChained(t, runID, &implID, "implement_review_started", nil)
	ar.appendChained(t, runID, &implID, "implement_reviewed", nil)
	d := reviewDeps(runs, arts, ar, 1, time.Hour, dispatchTime(5).Add(time.Minute))
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass once review lands; missing=%+v", state, missing)
	}
}

func TestCompute_Rule6_ApproveWithConcernsClears(t *testing.T) {
	// ADR-027 posture: a non-blocking terminal verdict (approve_with_concerns)
	// still counts as PRESENT — the presence gate is not the advisory gate.
	runID, runs, arts, ar := reviewPendingSetup(t, 1)
	implID := runs.stages[1].ID
	ar.appendChained(t, runID, &implID, "implement_review_started", nil)
	payload, _ := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{map[string]any{"category": "scope"}}})
	ar.appendChained(t, runID, &implID, "implement_reviewed", payload)
	d := reviewDeps(runs, arts, ar, 1, time.Hour, dispatchTime(5).Add(time.Minute))
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (approve_with_concerns is terminal -> present); missing=%+v", state, missing)
	}
}

func TestCompute_Rule6_BackstopDemotesStuckReview(t *testing.T) {
	// A reviewer that died emitting no terminal entry must not wedge the gate
	// forever: past the backstop the review is treated as present.
	runID, runs, arts, ar := reviewPendingSetup(t, 1)
	implID := runs.stages[1].ID
	ar.appendChained(t, runID, &implID, "implement_review_started", nil)
	d := reviewDeps(runs, arts, ar, 1, time.Hour, dispatchTime(5).Add(2*time.Hour))
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (backstop elapsed -> present); missing=%+v", state, missing)
	}
}

func TestCompute_Rule6_NeverDispatchedSkips(t *testing.T) {
	// reviewers.agent>0 configured but no implement_review_started -> nothing
	// to wait on; the rule is inert and the rest of the audit passes.
	runID, runs, arts, ar := reviewPendingSetup(t, 1)
	d := reviewDeps(runs, arts, ar, 1, time.Hour, time.Time{})
	state, missing, err := auditcomplete.Compute(context.Background(), runID, d)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (never dispatched); missing=%+v", state, missing)
	}
}

func TestCompute_Rule6_NilClosuresSkip(t *testing.T) {
	// Dev / test posture: ImplementReviewers / ReviewBackstop unwired -> the
	// rule is skipped even with a dispatched-but-unlanded review present.
	runID, runs, arts, ar := reviewPendingSetup(t, 1)
	implID := runs.stages[1].ID
	ar.appendChained(t, runID, &implID, "implement_review_started", nil)
	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (nil closures skip rule 6); missing=%+v", state, missing)
	}
}

// --- Rule 7 (security-findings gate, #1096) ---
//
// One behavioral test per enumerated failure mode:
//   (a) high finding above the floor              -> StateFail + MissingSecurityFindings
//   (b) finding floored under an OLDER fixup       -> gate clears (stale)
//   (b2) clean re-scan above the fixup             -> gate clears (re-scan-clears)
//   (b3) DIRTY re-scan above the fixup             -> StateFail (floor doesn't suppress forever)
//   (c1) securityscan read error                   -> pending-flavored, fail OPEN
//   (c2) fixup-marker read error                   -> pending-flavored, fail OPEN
//   (c3) payload decode error                      -> pending-flavored, fail OPEN
//   (d) no securityscan entry at all               -> gate does not block

const secScanCat = securityscan.AuditCategorySecurityFindings

// highFinding is a representative high-severity finding on the implement diff.
func highFinding() securityscan.Finding {
	return securityscan.Finding{
		Number:    7,
		RuleID:    "go/sql-injection",
		Severity:  securityscan.SeverityHigh,
		State:     "open",
		Path:      "backend/internal/server/handler.go",
		StartLine: 42,
	}
}

// secFindingsPayload marshals the cross-slice securityscan audit payload —
// `{"findings": [...]}`. Zero findings is a clean scan.
func secFindingsPayload(t *testing.T, findings ...securityscan.Finding) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"findings": findings})
	if err != nil {
		t.Fatalf("marshal securityscan payload: %v", err)
	}
	return b
}

func TestRule7_GateKindBoundToContract(t *testing.T) {
	// Invariant (1): the gate MissingKind is the securityscan contract token,
	// imported unchanged — not a redeclared literal that could drift.
	if got, want := string(auditcomplete.MissingSecurityFindings), securityscan.GateMissingKind; got != want {
		t.Errorf("MissingSecurityFindings = %q, want securityscan.GateMissingKind %q", got, want)
	}
	if got, want := string(auditcomplete.MissingSecurityFindings), "security_findings_unresolved"; got != want {
		t.Errorf("MissingSecurityFindings = %q, want %q", got, want)
	}
}

func TestCompute_Rule7_HighFindingFailsGate(t *testing.T) {
	// (a) A high-severity finding on the diff with no intervening fixup holds
	// the merge: StateFail with the distinct security_findings_unresolved kind.
	runID, runs, arts, ar := happyPath(t)
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t, highFinding()))

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail; missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingSecurityFindings) {
		t.Fatalf("missing did not include security_findings_unresolved: %+v", missing)
	}
	// The detail names the count + first finding so a reviewer can route it.
	var detail string
	for _, m := range missing {
		if m.Kind == auditcomplete.MissingSecurityFindings {
			detail = m.Detail
		}
	}
	if !strings.Contains(detail, "go/sql-injection") {
		t.Errorf("detail should name the firing rule: %s", detail)
	}
}

func TestCompute_Rule7_StaleFindingBelowFixupClears(t *testing.T) {
	// (b) A finding recorded BEFORE the latest stage_fixup_triggered is stale —
	// the fixup may have resolved it, and we wait for the re-scan rather than
	// gating on the old result. Gate clears.
	runID, runs, arts, ar := happyPath(t)
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t, highFinding())) // dirty, older
	ar.appendChained(t, runID, nil, "stage_fixup_triggered", nil)                     // fixup floors it

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (finding below fixup floor is stale); missing=%+v", state, missing)
	}
	if containsKind(missing, auditcomplete.MissingSecurityFindings) {
		t.Errorf("stale finding must not gate: %+v", missing)
	}
}

func TestCompute_Rule7_CleanRescanAfterFixupClears(t *testing.T) {
	// (b2) The condition-(4) path: a dirty scan, then a fixup, then a CLEAN
	// re-scan above the floor. The clean re-scan is current and clears the gate.
	runID, runs, arts, ar := happyPath(t)
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t, highFinding())) // dirty
	ar.appendChained(t, runID, nil, "stage_fixup_triggered", nil)                     // fixup
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t))                // clean re-scan

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (clean re-scan clears); missing=%+v", state, missing)
	}
	if containsKind(missing, auditcomplete.MissingSecurityFindings) {
		t.Errorf("clean re-scan must clear the gate: %+v", missing)
	}
}

func TestCompute_Rule7_DirtyRescanAfterFixupReblocks(t *testing.T) {
	// (b3) Flooring must not permanently suppress: a fresh DIRTY scan above the
	// fixup floor re-blocks. Guards the floor logic in the other direction.
	runID, runs, arts, ar := happyPath(t)
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t, highFinding())) // dirty
	ar.appendChained(t, runID, nil, "stage_fixup_triggered", nil)                     // fixup
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t, highFinding())) // still dirty

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail (dirty re-scan re-blocks); missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingSecurityFindings) {
		t.Fatalf("dirty re-scan above floor must gate: %+v", missing)
	}
}

func TestCompute_Rule7_ScanReadError_FailsOpenPending(t *testing.T) {
	// (c1) A read failure on the securityscan category must fail OPEN: a
	// pending-flavored item, NOT StateFail, NOT a hard Compute error, NOT a
	// silently-open pass. Mirrors Rule 5 (head_fetch_failed).
	runID, runs, arts, ar := happyPath(t)
	ar.catErr = map[string]error{secScanCat: errors.New("db down")}

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute must not hard-error on a securityscan read failure: %v", err)
	}
	if state == stagecheck.StateFail {
		t.Fatalf("read failure must not flip to fail (fail-open); missing=%+v", missing)
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending (fail-open, not silently-open); missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingSecurityScanUnverified) {
		t.Fatalf("missing did not include security_findings_unverified: %+v", missing)
	}
}

func TestCompute_Rule7_FixupReadError_FailsOpenPending(t *testing.T) {
	// (c2) A read failure on the fixup markers (reached only once a scan entry
	// exists) must also fail OPEN as a pending-flavored item.
	runID, runs, arts, ar := happyPath(t)
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t, highFinding()))
	ar.catErr = map[string]error{"stage_fixup_triggered": errors.New("db down")}

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute must not hard-error on a fixup-marker read failure: %v", err)
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending (fail-open); missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingSecurityScanUnverified) {
		t.Fatalf("missing did not include security_findings_unverified: %+v", missing)
	}
}

func TestCompute_Rule7_DecodeError_FailsOpenPending(t *testing.T) {
	// (c3) A securityscan payload that decodes wrong (valid JSON the audit
	// hash canonicalizes fine, but a type mismatch for the findings contract)
	// must fail OPEN — a decode error is pending-flavored, never StateFail and
	// never a silently-open pass.
	runID, runs, arts, ar := happyPath(t)
	ar.appendChained(t, runID, nil, secScanCat, json.RawMessage(`{"findings":"not-an-array"}`))

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute must not hard-error on a decode failure: %v", err)
	}
	if state == stagecheck.StateFail {
		t.Fatalf("decode failure must not flip to fail (fail-open); missing=%+v", missing)
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending (fail-open, not silently-open); missing=%+v", state, missing)
	}
	if !containsKind(missing, auditcomplete.MissingSecurityScanUnverified) {
		t.Fatalf("missing did not include security_findings_unverified: %+v", missing)
	}
}

func TestCompute_Rule7_NoScanEntry_DoesNotBlock(t *testing.T) {
	// (d) The async-window posture: CodeQL runs after the PR opens, so a run
	// with no securityscan entry yet must NOT block (absence != open gate).
	runID, runs, arts, ar := happyPath(t)

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (no scan entry is not blocking); missing=%+v", state, missing)
	}
	if containsKind(missing, auditcomplete.MissingSecurityFindings) ||
		containsKind(missing, auditcomplete.MissingSecurityScanUnverified) {
		t.Errorf("absence of a scan must add no missing item: %+v", missing)
	}
}

func TestCompute_Rule7_MediumFindingDoesNotGate(t *testing.T) {
	// The webhook filters to high severity before recording, but assert the
	// gate is keyed on a non-empty recorded list: an entry the writer emitted
	// empty (e.g. only medium findings, all filtered out) does not block.
	runID, runs, arts, ar := happyPath(t)
	ar.appendChained(t, runID, nil, secScanCat, secFindingsPayload(t)) // no gating findings

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass (empty findings list clears); missing=%+v", state, missing)
	}
}

// stubPRHead returns a PRHeadFetcher that always returns `(headSHA,
// err)`. Captures the input args in t.Log so a failing test can show
// what was asked.
func stubPRHead(t *testing.T, headSHA string, err error) auditcomplete.PRHeadFetcher {
	t.Helper()
	return func(_ context.Context, installationID int64, repo githubclient.RepoRef, prNumber int) (string, error) {
		t.Logf("PRHead called: installationID=%d repo=%s pr=%d", installationID, repo.String(), prNumber)
		return headSHA, err
	}
}

// --- Retry chain (#281 / E16.5) ---

// retryChainSetup seeds a parent run + a retry run that follows it,
// each with its own plan + implement + review stages, distinct
// head_shas on their pull_request artifacts, and complete
// trace_uploaded chains. Mirrors the on-the-wire shape produced by
// the dispatcher's auto-retry path (#279): retry.ParentRunID =
// parent.ID, retry.RetryAttempt = parent.RetryAttempt + 1,
// distinct head_shas because the runner commits fresh on each
// attempt.
//
// Returns both run IDs + the fakes so individual tests can mutate a
// single run's audit story to assert the parent / child boundary
// holds.
func retryChainSetup(t *testing.T) (parentID, retryID uuid.UUID, runs *fakeRuns, arts *fakeArtifacts, ar *fakeAudit) {
	t.Helper()
	parentID = uuid.New()
	retryID = uuid.New()

	parentImpl := mkStage(parentID, 2, run.StageTypeImplement, run.StageStateSucceeded)
	retryImpl := mkStage(retryID, 1, run.StageTypeImplement, run.StageStateSucceeded)
	// Variant A from the issue body: retry runs skip the plan stage —
	// only the parent has one. Adding a retry-side plan would
	// over-state the rig: real retries never get one.
	parentPlan := mkStage(parentID, 1, run.StageTypePlan, run.StageStateSucceeded)
	parentReview := mkStage(parentID, 3, run.StageTypeReview, run.StageStateAwaitingApproval)
	retryReview := mkStage(retryID, 2, run.StageTypeReview, run.StageStateAwaitingApproval)

	installID := int64(99)
	parentRow := &run.Run{
		ID: parentID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		InstallationID: &installID, RetryAttempt: 0, MaxRetriesSnapshot: 1,
	}
	retryRow := &run.Run{
		ID: retryID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		InstallationID: &installID, ParentRunID: &parentID,
		RetryAttempt: 1, MaxRetriesSnapshot: 1,
	}

	runs = &fakeRuns{
		stages: []*run.Stage{parentPlan, parentImpl, parentReview, retryImpl, retryReview},
		runs: map[uuid.UUID]*run.Run{
			parentID: parentRow,
			retryID:  retryRow,
		},
	}
	arts = &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			parentPlan.ID: {planArtifact(parentPlan.ID, "standard_v1")},
			// Distinct head_shas — the runner commits fresh on each
			// attempt and the foreign-commit rule reads these to
			// build the known-set.
			parentImpl.ID: {pullRequestArtifactWithBody(parentImpl.ID, "p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1p1", 275)},
			retryImpl.ID:  {pullRequestArtifactWithBody(retryImpl.ID, "r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2r2", 275)},
		},
	}

	ar = &fakeAudit{}
	// Parent's chain: dispatch + traces on plan & implement.
	ar.appendChained(t, parentID, &parentPlan.ID, "stage_dispatched", nil)
	ar.appendChained(t, parentID, &parentPlan.ID, "trace_uploaded", traceVariantPayload("raw"))
	ar.appendChained(t, parentID, &parentPlan.ID, "trace_uploaded", traceVariantPayload("redacted"))
	ar.appendChained(t, parentID, &parentImpl.ID, "trace_uploaded", traceVariantPayload("raw"))
	ar.appendChained(t, parentID, &parentImpl.ID, "trace_uploaded", traceVariantPayload("redacted"))
	// Retry's chain: traces on its own implement stage. Independent
	// chain — prev_hash threading is per-run, so the retry's first
	// entry has prev_hash=nil just like the parent's first did.
	ar.appendChainedReset(t, retryID, &retryImpl.ID, "trace_uploaded", traceVariantPayload("raw"))
	ar.appendChained(t, retryID, &retryImpl.ID, "trace_uploaded", traceVariantPayload("redacted"))
	return parentID, retryID, runs, arts, ar
}

func TestRetryChain_AuditComplete_PassesPerRun(t *testing.T) {
	// Parent and retry each carry their own complete audit story.
	// Compute is run-scoped: both IDs return pass independently,
	// proving the implementation doesn't smear state across the
	// retry boundary.
	parentID, retryID, runs, arts, ar := retryChainSetup(t)
	d := deps(runs, arts, ar)
	parentState, parentMissing, err := auditcomplete.Compute(context.Background(), parentID, d)
	if err != nil {
		t.Fatalf("Compute(parent): %v", err)
	}
	if parentState != stagecheck.StatePass {
		t.Errorf("parent state = %s, want pass; missing=%+v", parentState, parentMissing)
	}
	retryState, retryMissing, err := auditcomplete.Compute(context.Background(), retryID, d)
	if err != nil {
		t.Fatalf("Compute(retry): %v", err)
	}
	if retryState != stagecheck.StatePass {
		t.Errorf("retry state = %s, want pass; missing=%+v", retryState, retryMissing)
	}
}

func TestRetryChain_AuditComplete_RetryGapsDontInfectParent(t *testing.T) {
	// Drop the retry's redacted trace entry — the retry's audit story
	// is now broken, but the parent's is untouched. Compute(parent)
	// must still pass; Compute(retry) must fail with trace_missing.
	// This is what proves the per-run scoping actually works: a
	// regression that read all entries regardless of run_id would
	// false-positive a pass on the retry (since the parent's traces
	// are present) AND a false-fail on the parent if the gap-detection
	// ever leaked across run boundaries.
	parentID, retryID, runs, arts, ar := retryChainSetup(t)
	retryImplID := runs.stages[3].ID // see retryChainSetup ordering
	ar.dropEntry(func(e *audit.Entry) bool {
		if e.StageID == nil || *e.StageID != retryImplID {
			return false
		}
		if e.Category != "trace_uploaded" {
			return false
		}
		return string(e.Payload) == string(traceVariantPayload("redacted"))
	})

	d := deps(runs, arts, ar)
	parentState, parentMissing, err := auditcomplete.Compute(context.Background(), parentID, d)
	if err != nil {
		t.Fatalf("Compute(parent): %v", err)
	}
	if parentState != stagecheck.StatePass {
		t.Errorf("parent state = %s, want pass (gap on retry must not infect parent); missing=%+v",
			parentState, parentMissing)
	}
	retryState, retryMissing, err := auditcomplete.Compute(context.Background(), retryID, d)
	if err != nil {
		t.Fatalf("Compute(retry): %v", err)
	}
	if retryState != stagecheck.StateFail {
		t.Fatalf("retry state = %s, want fail; missing=%+v", retryState, retryMissing)
	}
	if !containsKind(retryMissing, auditcomplete.MissingTrace) {
		t.Errorf("retry missing should include trace_missing: %+v", retryMissing)
	}
}

func TestRetryChain_AuditLogChain_VerifiesAcrossRuns(t *testing.T) {
	// The per-run chain integrity rule (rule 4 of Compute) MUST hold
	// for each run in a retry chain. Both parent and retry have
	// their own chains; both must verify. A regression that broke
	// linkage between successive entries on either side would surface
	// here as a chain_invalid missing item.
	parentID, retryID, runs, arts, ar := retryChainSetup(t)
	d := deps(runs, arts, ar)

	for _, tc := range []struct {
		name  string
		runID uuid.UUID
	}{
		{"parent", parentID},
		{"retry", retryID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, missing, err := auditcomplete.Compute(context.Background(), tc.runID, d)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if state != stagecheck.StatePass {
				t.Errorf("state = %s, want pass; missing=%+v", state, missing)
			}
			if containsKind(missing, auditcomplete.MissingChain) ||
				containsKind(missing, auditcomplete.MissingChainBroken) {
				t.Errorf("chain integrity check produced an issue: %+v", missing)
			}
		})
	}
}

// --- helpers ---

func deps(r *fakeRuns, a *fakeArtifacts, au *fakeAudit) auditcomplete.Deps {
	return auditcomplete.Deps{Runs: r, Artifacts: a, Audit: au}
}

func mkStage(runID uuid.UUID, seq int, typ run.StageType, state run.StageState) *run.Stage {
	return &run.Stage{
		ID:       uuid.New(),
		RunID:    runID,
		Sequence: seq,
		Type:     typ,
		State:    state,
	}
}

func planArtifact(stageID uuid.UUID, schemaVersion string) *artifact.Artifact {
	v := schemaVersion
	return &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       json.RawMessage(`{}`),
	}
}

func pullRequestArtifact(stageID uuid.UUID) *artifact.Artifact {
	return &artifact.Artifact{
		ID:      uuid.New(),
		StageID: stageID,
		Kind:    artifact.KindPullRequest,
		Content: json.RawMessage(`{}`),
	}
}

// pullRequestArtifactWithBody is the rule-5 (#282) variant —
// `pullRequestArtifact` returns `{}` which doesn't carry the
// head_sha / pr_number that the foreign-commit rule needs to gather
// the Fishhawk-recorded SHA set.
func pullRequestArtifactWithBody(stageID uuid.UUID, headSHA string, prNumber int) *artifact.Artifact {
	body, _ := json.Marshal(map[string]any{
		"head_sha":  headSHA,
		"pr_number": prNumber,
	})
	return &artifact.Artifact{
		ID:      uuid.New(),
		StageID: stageID,
		Kind:    artifact.KindPullRequest,
		Content: body,
	}
}

func traceVariantPayload(variant string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"variant": variant})
	return b
}

func containsKind(items []auditcomplete.MissingItem, kind auditcomplete.MissingKind) bool {
	for _, it := range items {
		if it.Kind == kind {
			return true
		}
	}
	return false
}

// --- fakes ---

type fakeRuns struct {
	run.Repository // embed for the methods we don't care about (panic on call is fine)
	stages         []*run.Stage
	listErr        error
	// runs is the chain GetRun walks. Rule 5 (foreign-commit
	// detection) walks parent_run_id upward; tests that exercise
	// that path seed multiple entries here. Tests that don't care
	// leave it nil — GetRun falls back to a default issue-triggered
	// run synthesized from stages[0].RunID.
	runs map[uuid.UUID]*run.Run
}

func (f *fakeRuns) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// When `runs` is seeded, scope the stages to the requested run
	// (the chain walk hits multiple run ids). When it isn't, keep
	// the original "all stages everywhere" behavior so existing
	// tests don't have to change.
	if f.runs != nil {
		var out []*run.Stage
		for _, s := range f.stages {
			if s.RunID == runID {
				out = append(out, s)
			}
		}
		return out, nil
	}
	return f.stages, nil
}

func (f *fakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	// Default: synthesize an issue-triggered run rooted at this id
	// so existing tests (which don't seed `runs`) keep working. The
	// foreign-commit rule reads InstallationID + Repo from the
	// returned row; tests that don't care leave them nil.
	return &run.Run{ID: id}, nil
}

type fakeArtifacts struct {
	artifact.Repository
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (f *fakeArtifacts) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	return f.byStage[stageID], nil
}

type fakeAudit struct {
	audit.Repository
	entries []*audit.Entry
	// catErr injects a per-category error into ListForRunByCategory so the
	// rule-7 fail-open tests can drive a read failure on exactly the
	// securityscan / fixup category without disturbing the other reads.
	catErr map[string]error
}

// appendChained mirrors what the real audit.Repository.AppendChained
// does at the integrity layer: compute the canonical hash, link
// prev → entry within the run's chain. Per-run scoping mirrors
// production — each run has its own chain, prev_hash threads only
// within that run. Tests use this so the synthetic chain is
// identical in shape to the production one and verifyChain agrees.
func (f *fakeAudit) appendChained(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload json.RawMessage) {
	t.Helper()
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	// Find the last entry that belongs to THIS run — that's the
	// prev_hash anchor. Entries on other runs (siblings in a retry
	// chain, etc.) don't link in.
	var prev *string
	for i := len(f.entries) - 1; i >= 0; i-- {
		e := f.entries[i]
		if e.RunID != nil && *e.RunID == runID {
			ph := e.EntryHash
			prev = &ph
			break
		}
	}
	r := runID
	ts := time.Date(2026, 5, 7, 12, 0, int(len(f.entries)), 0, time.UTC)
	hash, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:        &r,
		StageID:      stageID,
		Timestamp:    ts,
		Category:     category,
		ActorKind:    nil,
		ActorSubject: nil,
		Payload:      payload,
		PrevHash:     prev,
	})
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	f.entries = append(f.entries, &audit.Entry{
		ID:        uuid.New(),
		Sequence:  int64(len(f.entries) + 1),
		RunID:     &r,
		StageID:   stageID,
		Timestamp: ts,
		Category:  category,
		Payload:   payload,
		PrevHash:  prev,
		EntryHash: hash,
	})
}

// appendChainedReset is appendChained's alias; the per-run scoping
// in appendChained already gives a fresh chain for a new run_id.
// Kept as a named entry point in tests so the intent ("start a new
// run's chain") is explicit at the call site.
func (f *fakeAudit) appendChainedReset(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload json.RawMessage) {
	t.Helper()
	f.appendChained(t, runID, stageID, category, payload)
}

func (f *fakeAudit) dropEntry(pred func(*audit.Entry) bool) {
	out := f.entries[:0]
	for _, e := range f.entries {
		if !pred(e) {
			out = append(out, e)
		}
	}
	f.entries = out
}

func (f *fakeAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	out := []*audit.Entry{}
	for _, e := range f.entries {
		// Scope by runID so retry-chain tests (#281) can seed parent
		// + child chains side by side and have each verifyChain call
		// see only its own entries. Single-run tests are unaffected:
		// their entries all carry the one runID, so the filter is a
		// no-op for them.
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if err := f.catErr[category]; err != nil {
		return nil, err
	}
	out := []*audit.Entry{}
	for _, e := range f.entries {
		if e.Category != category {
			continue
		}
		if e.RunID != nil && *e.RunID != runID {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
