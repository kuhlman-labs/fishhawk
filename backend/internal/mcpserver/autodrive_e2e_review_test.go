package mcpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// autodrive_e2e_review_test.go is the E88.4 / #4528 review leg (slice 3),
// criterion (c): after implement and acceptance the review gate parks, and the
// delegated merge seam records zero calls until the change merges on the
// forge. It reuses the slice-2 harness (autodrive_e2e_test.go): the same
// adE2EHarness struct, fake spawner end states, ticks and reads.
//
// One harness difference: the GitHub webhook receiver needs a delivery store
// as well as the secret (server.handleWebhook answers 503
// webhook_store_unconfigured without one), and the slice-2 constructor sets
// only the secret. newAdE2EReviewHarness therefore stands the same stack up
// with WebhookDeliveries wired and a per-subtest campaign override. Every
// other wire is identical to newAdE2EHarness.
//
// What the leg proves, step by step:
//
//  1. The implement end state advances the run into acceptance, and because
//     every fixture plan criterion is skip_expected with a basis, the
//     orchestrator's all-skip-with-basis short-circuit settles acceptance
//     server-side: acceptance_outcome_recorded verdict not_validated, basis
//     all-skip-with-basis, and NO acceptance spawn. There is no
//     acceptance-free spec fallback.
//  2. The review stage parks at awaiting_approval. N driver and dispatcher
//     ticks record zero merger calls and no decision marker on the review
//     stage, and the status awaiting block reads human/gate_approval with
//     stage_type review and no decision.
//  3. An in-Fishhawk approval of the review stage is refused 409
//     review_stage_managed_by_github (ADR-018).
//  4. The human PR approval arrives the way the server admits it: a signed
//     pull_request_review (submitted, approved) webhook. It records
//     pr_approved_on_github and leaves the stage parked; the merger still
//     records zero calls, and fishhawk_get_run_status next_actions name
//     fishhawk_merge_run.
//  5. A signed pull_request (closed, merged) webhook settles the review stage
//     and the run; the next driver tick settles the item and the awaiting
//     block is gone. The merger still records zero calls: the merge happened
//     on the forge, not through the delegated seam.
//
// The other half of criterion (c), "fishhawk_merge_run passes the ADR-112
// forge-approval fold and merges", needs a live GitHub review snapshot read
// through githubclient; this package has no hermetic seam for it. It is the
// requires_live_validation criterion review-approval-merge-run-live on
// rolling walk #5207.

// adE2EReviewSpecYAML is the review-leg workflow: the plan-leg plan gate,
// then implement, acceptance and a human review gate. Fixture preconditions
// (approval condition C6, pinned by adE2EReviewFixturePreconditions):
//
//   - the review gate declares NO approvals.members, so the merge-settle
//     members check (mergeSettledMembersAdmit) admits without a forge read;
//   - the implement stage declares NO agent reviewers, so no implement review
//     is ever started and the ADR-036 settle hold
//     (checkImplementReviewSettled) cannot leave a merge unsettled;
//   - the review stage takes the implement pull_request as an input, which
//     is what makes it GitHub-managed (ADR-018) rather than an in-Fishhawk
//     confirm gate;
//   - the workflow contract delegates merge (mode auto,
//     gates_resolved_ci_green), so the default subtest runs with may_merge
//     DELEGATED and the zero-merger-call result is not an artifact of an
//     undelegated merge.
const adE2EReviewSpecYAML = `version: "2"
workflows:
  feature_change:
    autonomy: high
    actions:
      approve:
        mode: auto
        when: clean_dual_approval
      merge:
        mode: auto
        when: gates_resolved_ci_green
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

// adE2EMayMergeGatedOverride is the E25.12 campaign operator_agent override
// that leaves may_merge GATED: it delegates only may_approve, and a campaign
// override wins wholesale over the workflow contract, so the workflow's merge
// delegation does not apply under it.
const adE2EMayMergeGatedOverride = `{"may_approve":"clean_dual_approval"}`

// adE2EReviewHeadSHA / adE2EReviewBaseSHA are the head and base the merge
// webhook reports.
const (
	adE2EReviewHeadSHA = "1111111111111111111111111111111111111111"
	adE2EReviewBaseSHA = "2222222222222222222222222222222222222222"
)

// adE2EReviewFixturePreconditions asserts the C6 fixture preconditions on the
// parsed review-leg spec, so a later edit that adds review-gate members or
// implement agent reviewers fails here, naming the precondition, rather than
// as an unexplained merge-settle hold further down.
func adE2EReviewFixturePreconditions(t *testing.T) {
	t.Helper()
	parsed, err := spec.ParseBytes([]byte(adE2EReviewSpecYAML))
	if err != nil {
		t.Fatalf("parse review-leg spec: %v", err)
	}
	wf, ok := parsed.Workflows["feature_change"]
	if !ok {
		t.Fatal("review-leg spec has no feature_change workflow")
	}
	if wf.OperatorAgent == nil || wf.OperatorAgent.MayMerge != spec.ConditionGatesResolvedCIGreen {
		t.Fatalf("precondition: the workflow contract must delegate merge at gates_resolved_ci_green, got %+v", wf.OperatorAgent)
	}
	var sawImplement, sawAcceptance, sawReview bool
	for _, st := range wf.Stages {
		switch st.Type {
		case spec.StageTypeImplement:
			sawImplement = true
			if st.Reviewers != nil && (st.Reviewers.Agent > 0 || len(st.Reviewers.Agents) > 0) {
				t.Fatalf("precondition: the implement stage must declare no agent reviewers (no unsettled implement review), got %+v", st.Reviewers)
			}
		case spec.StageTypeAcceptance:
			sawAcceptance = true
		case spec.StageTypeReview:
			sawReview = true
			if !st.Executor.Human {
				t.Fatal("precondition: the review stage must be human-executed")
			}
			for _, g := range st.Gates {
				if g.Approvals != nil && len(g.Approvals.Members) > 0 {
					t.Fatalf("precondition: the review gate must declare no approvals.members, got %v", g.Approvals.Members)
				}
				if g.OperatorAgent != nil {
					t.Fatalf("precondition: the review gate must not override the operator_agent block, got %+v", g.OperatorAgent)
				}
			}
		}
	}
	if !sawImplement || !sawAcceptance || !sawReview {
		t.Fatalf("precondition: the review-leg spec must declare implement, acceptance and review (implement %v, acceptance %v, review %v)",
			sawImplement, sawAcceptance, sawReview)
	}
}

// newAdE2EReviewHarness is newAdE2EHarness for the review leg: the same
// Postgres-backed stack, plus a webhook delivery store so signed GitHub
// webhooks are admitted, and the campaign's operator_agent override set to
// override (nil = no override, so the workflow contract governs).
func newAdE2EReviewHarness(t *testing.T, override []byte) *adE2EHarness {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	h := &adE2EHarness{
		t: t, ctx: ctx, specYAML: adE2EReviewSpecYAML,
		runs:       runpkg.NewPostgresRepository(pool),
		audit:      audit.NewPostgresRepository(pool),
		artifacts:  artifact.NewPostgresRepository(pool),
		campaigns:  campaign.NewPostgresRepository(pool),
		approvals:  approval.NewPostgresRepository(pool),
		amendments: scopeamendment.NewPostgresRepository(pool),
		tokens:     apitoken.NewPostgresRepository(pool),
		merger:     &adE2EMerger{},
	}
	h.root = t.TempDir()
	h.workDir = filepath.Join(h.root, "checkout")
	if err := os.MkdirAll(h.workDir, 0o750); err != nil {
		t.Fatalf("mkdir working dir: %v", err)
	}
	h.orch = &orchestrator.Orchestrator{Runs: h.runs, Audit: h.audit, Artifacts: h.artifacts}
	h.srv = server.New(server.Config{
		RunRepo:             h.runs,
		AuditRepo:           h.audit,
		ArtifactRepo:        h.artifacts,
		CampaignRepo:        h.campaigns,
		ApprovalRepo:        h.approvals,
		ConcernRepo:         concern.NewPostgresRepository(pool),
		ScopeAmendmentRepo:  h.amendments,
		APITokenRepo:        h.tokens,
		Orchestrator:        h.orch,
		DefaultBranch:       defaultbranch.Static("main"),
		GitHubWebhookSecret: []byte(adE2EWebhookSecret),
		WebhookDeliveries:   webhook.NewMemoryStore(time.Hour),
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.ts = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.ts.Close)

	operatorScopes := append([]string{"read:campaigns"}, oauthas.SupportedScopes...)
	h.dispatcherBearer = h.mint(adE2EDispatcherSub, hostdispatcher.Scopes())
	h.operatorBearer = h.mint(adE2EHumanSubject, operatorScopes)
	h.agentBearer = h.mint(operatorrole.CampaignActorSubject, operatorScopes)
	h.noAuditBearer = h.mint("github:e2e-reader", []string{"read:campaigns", "read:runs"})

	c, err := h.campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo: adE2ERepo, EpicRef: "issue:4528",
		OperatorAgent: override,
		WorkingDir:    h.workDir, RunnerKind: runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	it, err := h.campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:4529"})
	if err != nil {
		t.Fatalf("create campaign item: %v", err)
	}
	if c, err = h.campaigns.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("start campaign: %v", err)
	}
	h.campaign, h.item = c, it

	h.driver = &campaigndriver.Ticker{
		Campaigns: h.campaigns,
		Runs:      h.runs,
		Starter:   adE2EStarter{h: h},
		Audit:     h.audit,
		GateActor: adE2EGateActor{h: h},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h.spawner = &adE2ESpawner{h: h, emulate: map[string]func(runID, stageID uuid.UUID){
		"plan":      h.emulatePlanParked,
		"implement": h.emulateImplementSucceeded,
	}}
	h.loop = h.newDispatchLoop()
	return h
}

// postGitHubWebhook delivers a signed GitHub webhook (HMAC-SHA256 over the
// configured secret, X-Hub-Signature-256) to the real /webhooks/github route
// and returns the HTTP status.
func (h *adE2EHarness) postGitHubWebhook(event string, payload any) int {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("marshal %s webhook: %v", event, err)
	}
	mac := hmac.New(sha256.New, []byte(adE2EWebhookSecret))
	mac.Write(body)
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.ts.URL+"/webhooks/github", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("build %s webhook: %v", event, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", uuid.NewString())
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post %s webhook: %v", event, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func adE2EWebhookRepo() map[string]any {
	return map[string]any{"full_name": adE2ERepo}
}

// adE2EPRReviewApproved is a pull_request_review.submitted payload: the human
// approves the run's PR at its head.
func adE2EPRReviewApproved() map[string]any {
	return map[string]any{
		"action": "submitted",
		"review": map[string]any{
			"user":      map[string]any{"login": "kuhlman-labs"},
			"state":     "approved",
			"body":      "LGTM",
			"commit_id": adE2EReviewHeadSHA,
		},
		"pull_request": map[string]any{"html_url": adE2EPRURL, "number": 7},
		"repository":   adE2EWebhookRepo(),
		"sender":       map[string]any{"login": "kuhlman-labs", "type": "User"},
	}
}

// adE2EPRMerged is a pull_request.closed payload with merged=true.
func adE2EPRMerged() map[string]any {
	return map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"html_url":  adE2EPRURL,
			"number":    7,
			"merged":    true,
			"merged_by": map[string]any{"login": "kuhlman-labs"},
			"head":      map[string]any{"sha": adE2EReviewHeadSHA},
			"base":      map[string]any{"sha": adE2EReviewBaseSHA},
		},
		"repository": adE2EWebhookRepo(),
		"sender":     map[string]any{"login": "kuhlman-labs", "type": "User"},
	}
}

// reviewStageMarkers returns the decision_required rows that name stageID,
// by payload gate or by row stage_id.
func (h *adE2EHarness) reviewStageMarkers(runID, stageID uuid.UUID) []*audit.Entry {
	h.t.Helper()
	var out []*audit.Entry
	for _, e := range h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionRequired) {
		var p struct {
			Gate string `json:"gate"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			h.t.Fatalf("decode decision_required row: %v", err)
		}
		if p.Gate == stageID.String() || (e.StageID != nil && *e.StageID == stageID) {
			out = append(out, e)
		}
	}
	return out
}

// runStatusNamesMergeRun reads fishhawk_get_run_status through this package's
// run-status path and reports whether next_actions name fishhawk_merge_run,
// with the classified state and action list for failure messages.
func (h *adE2EHarness) runStatusNamesMergeRun(runID uuid.UUID) (bool, string, []string) {
	h.t.Helper()
	_, out, err := h.resolver(h.operatorBearer).getRunStatus(h.ctx, nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		h.t.Fatalf("get run status: %v", err)
	}
	if out.NextActions == nil {
		return false, "", nil
	}
	names := make([]string, 0, len(out.NextActions.Actions))
	found := false
	for _, a := range out.NextActions.Actions {
		names = append(names, a.Action)
		if a.Action == "fishhawk_merge_run" {
			found = true
		}
	}
	return found, out.NextActions.State, names
}

// requireReviewParked asserts the review stage is still awaiting_approval and
// the delegated merge seam has recorded zero calls.
func (h *adE2EHarness) requireReviewParked(runID uuid.UUID, when string) {
	h.t.Helper()
	if st := h.stageOf(runID, runpkg.StageTypeReview); st.State != runpkg.StageStateAwaitingApproval {
		h.t.Fatalf("review stage = %s %s, want awaiting_approval (ADR-018: only the merge event unparks it)", st.State, when)
	}
	if n := h.merger.count(); n != 0 {
		h.t.Fatalf("merger calls = %d %s, want 0 (the review gate is parked)", n, when)
	}
}

// --- (c) the review gate parks ----------------------------------------------

// TestAutoDriveE2E_ReviewGateParks is criterion (c), drivable half. One subtest
// per campaign override:
//
//   - default: no campaign override, so the workflow contract governs and
//     may_merge is DELEGATED (gates_resolved_ci_green). The delegated merge arm
//     still records zero calls while the review stage holds awaiting_approval
//     until the forge merge (ADR-018). Two checks hold it, both keyed on that
//     parked stage: the condition itself (delegation evalGatesResolvedCIGreen
//     is not met while a stage awaits approval, and this harness records no
//     checks_green_awaiting_merge drive advance), then the delegated arm's
//     structural mergeGateReady. Removing mergeGateReady's awaiting_approval
//     check alone leaves this subtest green: the condition masks it.
//   - may_merge_gated_override_nondiscriminating_hermetically: the E25.12
//     campaign override that leaves may_merge gated. It also records zero
//     calls, but hermetically that result does NOT discriminate the override:
//     the parked review gate is structural (mergeGateReady and the condition
//     above), so it yields zero calls with or without the delegation. Whether
//     the gated override really holds a mergeable run is decided only by the
//     live criterion on rolling walk #5207.
func TestAutoDriveE2E_ReviewGateParks(t *testing.T) {
	adE2EReviewFixturePreconditions(t)
	for _, tc := range []struct {
		name     string
		override []byte
	}{
		{name: "default", override: nil},
		{name: "may_merge_gated_override_nondiscriminating_hermetically", override: []byte(adE2EMayMergeGatedOverride)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdE2EReviewHarness(t, tc.override)
			runID, planID := h.driveToPlanGate()
			if code, ec := h.postApproval(h.operatorBearer, planID); code >= 300 {
				t.Fatalf("human plan approval = %d %s, want 2xx", code, ec)
			}
			// One round: the dispatcher spawns implement, whose end state
			// advances the run through acceptance to the review gate.
			h.tickBoth(1)

			// (1) Acceptance settled server-side by the all-skip-with-basis
			// short-circuit, with no acceptance spawn.
			acc := h.stageOf(runID, runpkg.StageTypeAcceptance)
			if acc.State != runpkg.StageStateSucceeded {
				t.Fatalf("acceptance stage = %s, want succeeded (all-skip-with-basis short-circuit)", acc.State)
			}
			outcomes := h.auditRows(runID, server.CategoryAcceptanceOutcomeRecorded)
			if len(outcomes) != 1 {
				t.Fatalf("acceptance_outcome_recorded rows = %d, want exactly 1", len(outcomes))
			}
			var ao map[string]any
			if err := json.Unmarshal(outcomes[0].Payload, &ao); err != nil {
				t.Fatalf("decode acceptance_outcome_recorded: %v", err)
			}
			if ao["verdict"] != plan.AcceptanceVerdictNotValidated || ao[plan.AcceptanceBasisKey] != plan.AcceptanceBasisAllSkipWithBasis ||
				ao["stage_id"] != acc.ID.String() {
				t.Fatalf("acceptance_outcome_recorded = %v, want verdict %s, basis %s on stage %s",
					ao, plan.AcceptanceVerdictNotValidated, plan.AcceptanceBasisAllSkipWithBasis, acc.ID)
			}
			if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan,implement" {
				t.Fatalf("spawns = %v, want exactly [plan implement] (no acceptance spawn)", got)
			}

			// (2) The review gate parks: N ticks, zero merger calls, no
			// decision marker on the review stage, and the status names a
			// human gate approval on the review stage with no decision.
			review := h.stageOf(runID, runpkg.StageTypeReview)
			h.requireReviewParked(runID, "on arrival")
			h.tickBoth(adE2ETicks)
			h.requireReviewParked(runID, "after the parking ticks")
			if got := h.reviewStageMarkers(runID, review.ID); len(got) != 0 {
				t.Fatalf("decision_required rows naming the review stage = %d, want 0 (a review gate never writes a marker)", len(got))
			}
			if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan,implement" {
				t.Fatalf("spawns = %v after the parking ticks, want still [plan implement]", got)
			}
			_, blk := h.status(h.operatorBearer)
			if blk == nil || blk.Kind != "human" || blk.Reason != "gate_approval" || blk.StageID != review.ID.String() ||
				blk.StageType != "review" || blk.Decision != nil {
				t.Fatalf("awaiting = %+v, want human/gate_approval on the review stage (stage_type review) with no decision", blk)
			}

			// (3) The review stage admits no in-Fishhawk approval (ADR-018).
			code, ec := h.postApproval(h.operatorBearer, review.ID)
			if code != http.StatusConflict || ec != "review_stage_managed_by_github" {
				t.Fatalf("review-stage approval = %d %q, want 409 review_stage_managed_by_github", code, ec)
			}
			if n := h.approvalCount(review.ID); n != 0 {
				t.Fatalf("review-stage approval rows = %d after the refusal, want 0", n)
			}
			h.requireReviewParked(runID, "after the refused approval")

			// (4) The human PR approval through the signed webhook: recorded,
			// but the stage stays parked and the merger stays at zero.
			if code := h.postGitHubWebhook("pull_request_review", adE2EPRReviewApproved()); code != http.StatusAccepted {
				t.Fatalf("pull_request_review webhook = %d, want 202", code)
			}
			approved := h.auditRows(runID, server.CategoryPRApprovedOnGitHub)
			if len(approved) != 1 {
				t.Fatalf("pr_approved_on_github rows = %d, want exactly 1", len(approved))
			}
			if approved[0].StageID == nil || *approved[0].StageID != review.ID {
				t.Fatalf("pr_approved_on_github stage_id = %v, want the review stage %s", approved[0].StageID, review.ID)
			}
			h.requireReviewParked(runID, "after the PR approval webhook")
			h.tickBoth(adE2ETicks)
			h.requireReviewParked(runID, "after the post-approval ticks")
			if got := h.reviewStageMarkers(runID, review.ID); len(got) != 0 {
				t.Fatalf("decision_required rows naming the review stage = %d after the PR approval, want 0", len(got))
			}
			if found, state, names := h.runStatusNamesMergeRun(runID); !found {
				t.Fatalf("run status next_actions (state %q) = %v, want fishhawk_merge_run named after the PR approval", state, names)
			}

			// (5) The forge merge settles the review stage and the run; the
			// next driver tick settles the item. The merge happened on the
			// forge, so the delegated seam still records zero calls.
			if code := h.postGitHubWebhook("pull_request", adE2EPRMerged()); code != http.StatusAccepted {
				t.Fatalf("pull_request closed webhook = %d, want 202", code)
			}
			if st := h.stageOf(runID, runpkg.StageTypeReview); st.State != runpkg.StageStateSucceeded {
				t.Fatalf("review stage = %s after the merge webhook, want succeeded", st.State)
			}
			r, err := h.runs.GetRun(h.ctx, runID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if r.State != runpkg.StateSucceeded {
				t.Fatalf("run = %s after the merge webhook, want succeeded", r.State)
			}
			if n := len(h.auditRows(runID, server.CategoryPRMerged)); n != 1 {
				t.Fatalf("pr_merged rows = %d, want exactly 1", n)
			}
			h.driverTick()
			it, err := h.campaigns.GetCampaignItem(h.ctx, h.item.ID)
			if err != nil {
				t.Fatalf("get campaign item: %v", err)
			}
			if it.State != campaign.ItemStateSucceeded {
				t.Fatalf("campaign item = %s after the merge and one driver tick, want succeeded", it.State)
			}
			if _, blk := h.status(h.operatorBearer); blk != nil {
				t.Fatalf("awaiting = %+v after the item settled, want absent", blk)
			}
			if n := h.merger.count(); n != 0 {
				t.Fatalf("merger calls = %d after the forge merge, want 0 (the merge did not go through the delegated seam)", n)
			}
		})
	}
}
