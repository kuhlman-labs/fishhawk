package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// autodrive_e2e_acceptance_test.go is the #5298 cross-boundary proof that the
// host dispatcher dispatches a feature_change acceptance stage, and holds it
// for the implement review ROUND first. It reuses the E88.4 harness
// (autodrive_e2e_test.go) against pgtest Postgres, so every piece below is
// real except the spawn seam:
//
//   - the server's GET /v0/runs/{run_id}/stages, whose implement row carries
//     review_agents_configured from the run's workflow spec;
//   - the host-dispatcher tick loop under a host-dispatcher/<host> token,
//     reading the round through GET /v0/runs/{run_id}/audit (the route the
//     hostdispatcher allowlist admits);
//   - the loop's own status.json (run_refusals), written by the real tick;
//   - the campaign-status overlay, reading that status.json through
//     FISHHAWK_HOST_DISPATCH_STATE_DIR and, before the dispatcher has refused
//     anything, the round itself (the pre-tick check).
//
// The implement emulation writes implement_review_started (configured_agents
// 2) on the implement stage BEFORE the stage succeeds and Advance runs, the
// production trace-time order, or writes no started row at all for the
// expected-but-not-recorded window. The decomposed-parent specifics of that
// window are pinned by the host_dispatcher and drive_run unit tests; this leg
// covers the same window through the same predicate.

// adE2EAcceptanceSpecYAML is the acceptance-leg workflow: the plan-leg plan
// gate, an implement stage declaring TWO agent reviewers, then acceptance
// ahead of a human review gate (the feature_change order).
const adE2EAcceptanceSpecYAML = `version: "2"
workflows:
  feature_change:
    autonomy: high
    actions:
      approve:
        mode: auto
        when: clean_dual_approval
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: plan
          - artifact: pull_request
            from_stage: implement
        produces:
          - artifact: acceptance
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvals:
              count: 1
`

// adE2EAcceptanceConfiguredAgents is the implement agent reviewer count the
// spec above declares, and the configured_agents the started row carries.
const adE2EAcceptanceConfiguredAgents = 2

// adE2EAcceptancePlanJSON is the acceptance-leg plan: ONE drivable criterion
// (no skip_expected), so no acceptance short-circuit settles the stage and the
// dispatcher must spawn it. No target host is declared, so the target check
// passes without a probe.
func adE2EAcceptancePlanJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "dispatch acceptance after the implement review round",
		"verification": map[string]any{
			"test_strategy": "integration",
			"rollback_plan": "revert the PR",
			"acceptance_criteria": []map[string]any{
				{"id": "acceptance-dispatched", "statement": "the host dispatcher spawns the acceptance stage", "source": "inferred", "rationale": "driven by the acceptance runner"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal acceptance plan: %v", err)
	}
	return b
}

// emulateImplementReviewRound is an implement runner's end state with the
// implement review round's started row: when started is set,
// implement_review_started (configured_agents 2) is appended on the implement
// stage before it succeeds and Advance runs.
func (h *adE2EHarness) emulateImplementReviewRound(started bool) func(runID, stageID uuid.UUID) {
	return func(runID, stageID uuid.UUID) {
		h.ensureRunRunning(runID)
		h.transitionStage(stageID, runpkg.StageStateRunning)
		if _, err := h.runs.SetRunPullRequestURL(h.ctx, runID, adE2EPRURL); err != nil {
			h.t.Errorf("set PR url: %v", err)
		}
		if started {
			h.appendImplementReviewStarted(runID, stageID)
		}
		h.transitionStage(stageID, runpkg.StageStateSucceeded)
		if _, err := h.orch.Advance(h.ctx, runID); err != nil {
			h.t.Errorf("advance after implement: %v", err)
		}
	}
}

func (h *adE2EHarness) appendImplementReviewStarted(runID, implID uuid.UUID) {
	h.t.Helper()
	sid := implID
	h.appendRunAudit(runID, &sid, "implement_review_started", planreview.ReviewStartedPayload{
		ConfiguredAgents: adE2EAcceptanceConfiguredAgents, Authority: planreview.AuthorityAdvisory,
	})
}

// appendImplementVerdict appends one implement_reviewed row on the implement
// stage and returns it (its sequence anchors a concern row).
func (h *adE2EHarness) appendImplementVerdict(runID, implID uuid.UUID, verdict planreview.Verdict, concerns []planreview.Concern) *audit.Entry {
	h.t.Helper()
	raw, err := json.Marshal(planreview.PlanReviewedPayload{
		ReviewerKind: "agent", Authority: planreview.AuthorityAdvisory, Verdict: verdict, Concerns: concerns,
	})
	if err != nil {
		h.t.Fatalf("marshal implement_reviewed: %v", err)
	}
	sid := implID
	e, err := h.audit.AppendChained(h.ctx, audit.ChainAppendParams{
		RunID: runID, StageID: &sid, Timestamp: time.Now().UTC(), Category: "implement_reviewed", Payload: raw,
	})
	if err != nil {
		h.t.Fatalf("append implement_reviewed: %v", err)
	}
	return e
}

// settleImplementRound lands both configured verdicts. With withConcern, the
// first is approve_with_concerns and its concern is persisted as an OPEN
// concern row, the way the server ingests a verdict.
func (h *adE2EHarness) settleImplementRound(runID, implID uuid.UUID, withConcern bool) {
	h.t.Helper()
	if !withConcern {
		h.appendImplementVerdict(runID, implID, planreview.VerdictApprove, nil)
		h.appendImplementVerdict(runID, implID, planreview.VerdictApprove, nil)
		return
	}
	c := planreview.Concern{Severity: planreview.SeverityMedium, Category: "correctness", Note: "an edge case is untested"}
	e := h.appendImplementVerdict(runID, implID, planreview.VerdictApproveWithConcerns, []planreview.Concern{c})
	if _, err := h.concerns.InsertRaised(h.ctx, concern.InsertRaisedParams{
		RunID: runID, StageID: implID, StageKind: concern.StageKindImplement,
		OriginReviewSequence: e.Sequence,
		Concerns:             []concern.RaisedConcern{{Severity: string(c.Severity), Category: c.Category, Note: c.Note}},
	}); err != nil {
		h.t.Fatalf("insert implement concern: %v", err)
	}
	h.appendImplementVerdict(runID, implID, planreview.VerdictApprove, nil)
	open, err := h.concerns.ListOpenByRun(h.ctx, runID)
	if err != nil {
		h.t.Fatalf("list open concerns: %v", err)
	}
	if len(open) != 1 {
		h.t.Fatalf("open concerns = %d, want 1 (the fixture's open implement concern)", len(open))
	}
}

// dispatcherStatus reads the loop's REAL status.json from its state dir.
func (h *adE2EHarness) dispatcherStatus() HostDispatcherStatus {
	h.t.Helper()
	st, err := ReadHostDispatcherStatus(h.loop.cfg.StateDir)
	if err != nil {
		h.t.Fatalf("read dispatcher status.json: %v", err)
	}
	return st
}

// requireRunRefusal asserts status.json holds run runID's refusal entry with
// code on stageID.
func (h *adE2EHarness) requireRunRefusal(runID, stageID uuid.UUID, code, when string) {
	h.t.Helper()
	ref, ok := h.dispatcherStatus().RunRefusals[runID.String()]
	if !ok || ref.Code != code || ref.StageID != stageID.String() {
		h.t.Fatalf("status.json run_refusals[%s] %s = %+v (present %v), want code %s on the acceptance stage %s",
			runID, when, ref, ok, code, stageID)
	}
}

// overlayStatus reads the campaign status through an overlay-enabled resolver
// (the operator's own stdio posture) pointed at the loop's state dir, and
// returns the item's awaiting block.
func (h *adE2EHarness) overlayStatus() *campaignItemAwaiting {
	h.t.Helper()
	r := h.resolver(h.operatorBearer)
	r.localHostRungs = true
	stateDir := h.loop.cfg.StateDir
	r.getenv = func(k string) string {
		if k == hostDispatchStateDirEnv {
			return stateDir
		}
		return ""
	}
	_, out, err := r.getCampaignStatus(h.ctx, nil, GetCampaignStatusInput{CampaignID: h.campaign.ID.String()})
	if err != nil {
		h.t.Fatalf("get campaign status (overlay): %v", err)
	}
	for i := range out.Items {
		if out.Items[i].IssueRef == h.item.IssueRef {
			return out.Items[i].Awaiting
		}
	}
	h.t.Fatalf("campaign status has no item %s", h.item.IssueRef)
	return nil
}

// requireOverlayRoundHold asserts the overlay reads the acceptance item as
// system/in_flight (the implement review round holds it).
func (h *adE2EHarness) requireOverlayRoundHold(accID uuid.UUID, when string) {
	h.t.Helper()
	blk := h.overlayStatus()
	if blk == nil || blk.Kind != hostDispatchAwaitKindSystem || blk.Reason != hostDispatchReasonInFlight ||
		blk.StageType != "acceptance" || blk.StageID != accID.String() {
		h.t.Fatalf("overlay awaiting %s = %+v, want system/in_flight on the acceptance stage %s", when, blk, accID)
	}
}

// derivedStatus reads GET /v0/runs/{run_id}'s derived_status from the real
// server.
func (h *adE2EHarness) derivedStatus(runID uuid.UUID) string {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.ts.URL+"/v0/runs/"+runID.String(), nil)
	if err != nil {
		h.t.Fatalf("build run read: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.operatorBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("get run: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET /v0/runs/%s = %d %s", runID, resp.StatusCode, body)
	}
	var v struct {
		DerivedStatus string `json:"derived_status"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		h.t.Fatalf("decode run: %v", err)
	}
	return v.DerivedStatus
}

// requireNoAcceptanceSpawn asserts the dispatcher has spawned only plan and
// implement.
func (h *adE2EHarness) requireNoAcceptanceSpawn(when string) {
	h.t.Helper()
	if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan,implement" {
		h.t.Fatalf("spawns %s = %v, want [plan implement] (acceptance held for the implement review round)", when, got)
	}
}

// requireAcceptanceSpawned asserts the acceptance spawn, its auto-preview env
// pair, the cleared refusal entry and the dispatcher's run_auto_driven act.
func (h *adE2EHarness) requireAcceptanceSpawned(runID, accID uuid.UUID) {
	h.t.Helper()
	spawns := h.spawner.recorded()
	if got := spawnTypes(spawns); strings.Join(got, ",") != "plan,implement,acceptance" {
		h.t.Fatalf("spawns after the round settled = %v, want [plan implement acceptance]", got)
	}
	acc := spawns[len(spawns)-1]
	if acc.StageID != accID.String() {
		h.t.Fatalf("acceptance spawn stage = %s, want %s", acc.StageID, accID)
	}
	env := strings.Join(acc.Env, "\n")
	for _, want := range []string{
		acceptancePreviewCmdEnv + "=" + acceptancePreviewDefaultCmd,
		acceptancePreviewTeardownCmdEnv + "=" + acceptancePreviewDefaultTeardownCmd,
	} {
		if !strings.Contains("\n"+env+"\n", "\n"+want+"\n") {
			h.t.Fatalf("acceptance spawn env = %v, want %q (the auto-preview pair)", acc.Env, want)
		}
	}
	if ref, ok := h.dispatcherStatus().RunRefusals[runID.String()]; ok {
		h.t.Fatalf("status.json run_refusals[%s] = %+v after the acceptance spawn, want the entry cleared", runID, ref)
	}
	found := false
	for _, e := range h.auditRows(runID, server.CategoryRunAutoDriven) {
		var p struct {
			Stage  string `json:"stage"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			h.t.Fatalf("decode run_auto_driven: %v", err)
		}
		if p.Stage == "acceptance" && p.Source == hostdispatcher.SourceTag {
			found = true
		}
	}
	if !found {
		h.t.Fatalf("no run_auto_driven row with stage acceptance and source %s", hostdispatcher.SourceTag)
	}
}

// startAcceptanceLeg drives the item to the acceptance stage: the plan gate,
// the human approval, and one round whose implement spawn advances the run.
// It returns the run, implement and acceptance stage ids.
func (h *adE2EHarness) startAcceptanceLeg(started bool) (uuid.UUID, uuid.UUID, uuid.UUID) {
	h.t.Helper()
	h.planJSON = adE2EAcceptancePlanJSON(h.t)
	h.spawner.emulate["implement"] = h.emulateImplementReviewRound(started)
	runID, planID := h.driveToPlanGate()
	if code, ec := h.postApproval(h.operatorBearer, planID); code >= 300 {
		h.t.Fatalf("human plan approval = %d %s, want 2xx", code, ec)
	}
	h.tickBoth(1)

	impl := h.stageOf(runID, runpkg.StageTypeImplement)
	acc := h.stageOf(runID, runpkg.StageTypeAcceptance)
	rev := h.stageOf(runID, runpkg.StageTypeReview)
	if impl.State != runpkg.StageStateSucceeded || acc.State != runpkg.StageStateAwaitingHostDispatch || rev.State != runpkg.StageStatePending {
		h.t.Fatalf("stages after implement = implement %s, acceptance %s, review %s; want succeeded, awaiting_host_dispatch, pending (spawns %v)",
			impl.State, acc.State, rev.State, spawnTypes(h.spawner.recorded()))
	}
	// The pre-#5298 dispatcher could dispatch acceptance only from a
	// derived acceptance_pending, which needs a parked review; this run
	// never has one, so the dispatch below cannot come from that path.
	if ds := h.derivedStatus(runID); ds == derivedAcceptancePending {
		h.t.Fatalf("derived_status = %q, want anything but %s (feature_change parks no review before acceptance)", ds, derivedAcceptancePending)
	}
	h.requireNoAcceptanceSpawn("after implement")
	return runID, impl.ID, acc.ID
}

// TestAutoDriveE2E_AcceptanceDispatchedAfterImplementReview is #5298's
// cross-boundary leg: acceptance is held while the implement review round is
// in flight or expected but unrecorded, the hold reaches status.json and the
// overlay, and the next tick after the round settles spawns acceptance with
// the auto-preview pair, with and without an open implement concern.
func TestAutoDriveE2E_AcceptanceDispatchedAfterImplementReview(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withConcern bool
	}{
		{name: "no_open_concerns"},
		{name: "open_implement_concerns", withConcern: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdE2EHarness(t, adE2EAcceptanceSpecYAML)
			runID, implID, accID := h.startAcceptanceLeg(true)

			for range adE2ETicks {
				h.dispatcherTick()
			}
			h.requireNoAcceptanceSpawn("while the round is in flight")
			h.requireRunRefusal(runID, accID, hdRefuseImplementReviewInFlight, "while the round is in flight")
			h.requireOverlayRoundHold(accID, "while the round is in flight")

			h.settleImplementRound(runID, implID, tc.withConcern)
			h.dispatcherTick()
			h.requireAcceptanceSpawned(runID, accID)
		})
	}

	t.Run("round_not_started_then_settles", func(t *testing.T) {
		h := newAdE2EHarness(t, adE2EAcceptanceSpecYAML)
		runID, implID, accID := h.startAcceptanceLeg(false)

		// The real server's stage read carries the workflow-declared implement
		// reviewer count, read under the dispatcher's own token.
		stages, err := h.resolver(h.dispatcherBearer).api.ListRunStages(h.ctx, runID)
		if err != nil {
			t.Fatalf("list stages (dispatcher token): %v", err)
		}
		impl := stageByType(stages, "implement")
		if impl == nil {
			t.Fatal("the stage read has no implement row")
		}
		if impl.ReviewAgentsConfigured == nil || *impl.ReviewAgentsConfigured != adE2EAcceptanceConfiguredAgents {
			got := "absent"
			if impl.ReviewAgentsConfigured != nil {
				got = strconv.Itoa(*impl.ReviewAgentsConfigured)
			}
			t.Fatalf("implement stage review_agents_configured = %s, want %d (the workflow-declared implement agent reviewers)", got, adE2EAcceptanceConfiguredAgents)
		}
		if n := len(h.auditRows(runID, "implement_review_started")); n != 0 {
			t.Fatalf("implement_review_started rows = %d, want 0 (no round recorded yet)", n)
		}

		// Pre-tick: the dispatcher has not evaluated acceptance yet (no
		// run_refusals entry), and the overlay already reads the hold from
		// the round itself.
		if ref, ok := h.dispatcherStatus().RunRefusals[runID.String()]; ok {
			t.Fatalf("status.json run_refusals[%s] = %+v before acceptance was evaluated, want none", runID, ref)
		}
		h.requireOverlayRoundHold(accID, "before the dispatcher's first acceptance tick")

		h.dispatcherTick()
		h.requireNoAcceptanceSpawn("with no round recorded")
		h.requireRunRefusal(runID, accID, hdRefuseImplementReviewNotStarted, "with no round recorded")
		h.requireOverlayRoundHold(accID, "with no round recorded")

		h.appendImplementReviewStarted(runID, implID)
		h.dispatcherTick()
		h.requireNoAcceptanceSpawn("once the round started")
		h.requireRunRefusal(runID, accID, hdRefuseImplementReviewInFlight, "once the round started")

		h.settleImplementRound(runID, implID, false)
		h.dispatcherTick()
		h.requireAcceptanceSpawned(runID, accID)
	})
}
