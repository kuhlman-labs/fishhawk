package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// autodrive_e2e_test.go is the E88.4 / #4528 cross-boundary proof of local
// auto-drive (slice 2: harness + plan leg). One campaign item is driven by the
// REAL pieces end to end, against pgtest Postgres:
//
//   - campaigndriver.Ticker over the real campaign, run and audit repositories,
//     whose GateActor is server.DriveCampaignRunGate under the campaign
//     operator identity (with a recording merger) and whose RunStarter mints the
//     run through server.CreateRunForTrigger from an inline spec (standing in
//     for StartRunForCampaignIssue's GitHub spec fetch);
//   - the host-dispatcher tick loop (newHostDispatchLoop) over a
//     host-dispatcher/<host> token against the real HTTP routes, with only the
//     spawn seam faked;
//   - server.New behind httptest, with the webhook secret set so the review leg
//     (autodrive_e2e_review_test.go) can deliver signed GitHub webhooks.
//
// The fake spawner does NOT emulate the runner protocol: it emulates a
// runner's END STATE through the run, artifact, audit and orchestrator
// repositories (plan: the all-skip-with-basis plan artifact, a clean dual plan
// review and awaiting_approval; implement: a PR url and succeeded, then
// Advance). Like every pgtest test these skip without Docker.

const (
	adE2ERepo          = "kuhlman-labs/fishhawk-e2e"
	adE2EPRURL         = "https://github.com/kuhlman-labs/fishhawk-e2e/pull/7"
	adE2EWebhookSecret = "autodrive-e2e-webhook-secret"
	adE2EHumanSubject  = "github:kuhlman-labs"
	adE2EDispatcherSub = "host-dispatcher/e2e-host"
	adE2ETicks         = 3
)

// adE2EPlanSpecYAML is the plan-leg workflow: a plan gate carrying a human
// approvals block (count: 1) with approve delegated at mode: auto, so the
// delegated approve declines with decision_required:human_quorum_required, and
// an implement stage with no review stage.
const adE2EPlanSpecYAML = `version: "2"
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
        produces:
          - artifact: pull_request
`

// adE2EOperatorAgentOverride is the campaign's operator_agent override:
// may_approve delegated at clean_dual_approval.
const adE2EOperatorAgentOverride = `{"may_approve":"clean_dual_approval"}`

// adE2EPlanJSON is the fixture plan: every acceptance criterion is
// skip_expected with an expectation_basis, so the orchestrator's
// all-skip-with-basis short-circuit settles acceptance server-side.
func adE2EPlanJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "prove local auto-drive end to end",
		"verification": map[string]any{
			"test_strategy": "integration",
			"rollback_plan": "revert the PR",
			"acceptance_criteria": []map[string]any{
				{"id": "marker-recorded", "statement": "the parked decision is recorded", "source": "inferred", "rationale": "covered hermetically", "skip_expected": true, "expectation_basis": "validated in autodrive_e2e_test.go"},
				{"id": "status-awaiting", "statement": "campaign status names what the item awaits", "source": "inferred", "rationale": "covered hermetically", "skip_expected": true, "expectation_basis": "validated in campaign_item_awaiting_test.go"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture plan: %v", err)
	}
	return b
}

// adE2EMerger is the recording GitHubMerger the campaign gate actor carries.
type adE2EMerger struct {
	mu    sync.Mutex
	calls int
}

func (m *adE2EMerger) MergePullRequest(context.Context, *runpkg.Run, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return nil
}

func (m *adE2EMerger) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// adE2ESpawn is one recorded spawn.
type adE2ESpawn struct {
	RunID, StageID, StageType string
}

// adE2ESpawner is the dispatcher's spawn seam. It records every spawn and, per
// stage type, applies the runner end state through the repositories. A stage
// type with no emulation is recorded and left dispatched.
type adE2ESpawner struct {
	h      *adE2EHarness
	mu     sync.Mutex
	spawns []adE2ESpawn
	// emulate maps a stage type to the end state applied on its spawn.
	emulate map[string]func(runID, stageID uuid.UUID)
}

func (s *adE2ESpawner) spawn(_ string, _, _ []string, runID, stageID string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
	st, err := s.h.runs.GetStage(s.h.ctx, uuid.MustParse(stageID))
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.spawns = append(s.spawns, adE2ESpawn{RunID: runID, StageID: stageID, StageType: string(st.Type)})
	fn := s.emulate[string(st.Type)]
	s.mu.Unlock()
	if fn != nil {
		fn(uuid.MustParse(runID), st.ID)
	}
	return filepath.Join(s.h.root, "runner-"+stageID+".log"), nil
}

func (s *adE2ESpawner) recorded() []adE2ESpawn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]adE2ESpawn(nil), s.spawns...)
}

// adE2EHarness wires one Postgres-backed server, campaign driver and host
// dispatcher. The review leg reuses it with its own spec.
type adE2EHarness struct {
	t   *testing.T
	ctx context.Context

	runs       runpkg.Repository
	audit      audit.Repository
	artifacts  artifact.Repository
	campaigns  campaign.Repository
	approvals  approval.Repository
	amendments scopeamendment.Repository
	tokens     apitoken.Repository
	orch       *orchestrator.Orchestrator
	srv        *server.Server
	ts         *httptest.Server

	root     string
	workDir  string
	specYAML string

	campaign *campaign.Campaign
	item     *campaign.Item

	driver  *campaigndriver.Ticker
	merger  *adE2EMerger
	loop    *hostDispatchLoop
	spawner *adE2ESpawner

	dispatcherBearer string
	operatorBearer   string
	agentBearer      string
	noAuditBearer    string
}

// newAdE2EHarness stands the stack up and creates a running campaign with one
// item under specYAML. Nothing is ticked yet.
func newAdE2EHarness(t *testing.T, specYAML string) *adE2EHarness {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	h := &adE2EHarness{
		t: t, ctx: ctx, specYAML: specYAML,
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
		OperatorAgent: []byte(adE2EOperatorAgentOverride),
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

func (h *adE2EHarness) mint(subject string, scopes []string) string {
	h.t.Helper()
	tok, err := h.tokens.Issue(h.ctx, subject, scopes)
	if err != nil {
		h.t.Fatalf("issue token for %s: %v", subject, err)
	}
	return tok.PlainText
}

func (h *adE2EHarness) resolver(bearer string) *runResolver {
	return &runResolver{
		api:          newAPIClient(config{backendURL: h.ts.URL, apiToken: bearer}),
		getenv:       func(string) string { return "" },
		allowedRoots: []string{h.root},
	}
}

func (h *adE2EHarness) newDispatchLoop() *hostDispatchLoop {
	h.t.Helper()
	cfg, err := HostDispatcherConfig{
		BackendURL: h.ts.URL, APIToken: h.dispatcherBearer,
		AllowedRoots: []string{h.root}, StateDir: filepath.Join(h.t.TempDir(), "state"),
		MaxInFlight: 10, PerTickCap: 10, DailyCap: 100, Log: io.Discard,
	}.withDefaults()
	if err != nil {
		h.t.Fatalf("dispatcher config: %v", err)
	}
	if err := prepareHostDispatchStateDir(cfg.StateDir, fileOwnerUID); err != nil {
		h.t.Fatalf("prepare state dir: %v", err)
	}
	l := newHostDispatchLoop(cfg, h.resolver(h.dispatcherBearer), "/bin/fishhawk-runner")
	l.core.spawn = h.spawner.spawn
	l.core.probe = func(context.Context, string) runnerLivenessVerdict { return runnerDead }
	l.core.environ = func() []string { return []string{"PATH=/usr/bin"} }
	l.lockHeld = func() bool { return true }
	l.terminate = func(string) int { return 0 }
	return l
}

// adE2EStarter mints the item's run from the inline spec, marked for the host
// dispatcher and bound to the local checkout.
type adE2EStarter struct{ h *adE2EHarness }

func (s adE2EStarter) StartCampaignRun(ctx context.Context, it *campaign.Item, c *campaign.Campaign) (*runpkg.Run, error) {
	parsed, err := spec.ParseBytes([]byte(s.h.specYAML))
	if err != nil {
		return nil, err
	}
	wf, ok := parsed.Workflows["feature_change"]
	if !ok {
		return nil, errors.New("inline spec has no feature_change workflow")
	}
	ref := it.IssueRef
	return s.h.srv.CreateRunForTrigger(ctx, server.CreateRunForTriggerParams{
		Repo: c.Repo, WorkflowID: "feature_change", WorkflowSHA: "e2e",
		TriggerSource: runpkg.TriggerGitHubIssue, TriggerRef: &ref,
		RunnerKind: runpkg.RunnerKindLocal, WorkingDir: s.h.workDir, AutoDispatch: true,
		HaveStageDefs: true, WorkflowDef: wf, WorkflowSpec: []byte(s.h.specYAML),
		MaxRetriesSnapshot: webhook.WorkflowMaxRetries(wf),
	})
}

// adE2EGateActor is the campaign gate actor: server.DriveCampaignRunGate under
// the campaign operator identity with the recording merger.
type adE2EGateActor struct{ h *adE2EHarness }

func (a adE2EGateActor) DriveRunGate(ctx context.Context, runRow *runpkg.Run) (campaigndriver.GateActionOutcome, error) {
	return a.DriveRunGateWithCampaign(ctx, runRow, nil)
}

func (a adE2EGateActor) DriveRunGateWithCampaign(ctx context.Context, runRow *runpkg.Run, override []byte) (campaigndriver.GateActionOutcome, error) {
	id := server.Identity{
		Subject: operatorrole.CampaignActorSubject,
		TokenID: "operator-agent-campaign",
		Scopes:  operatorrole.CampaignActorScopes(),
	}
	return a.h.srv.DriveCampaignRunGate(ctx, runRow, id, a.h.merger, override)
}

// --- runner end states ------------------------------------------------------

func (h *adE2EHarness) ensureRunRunning(runID uuid.UUID) {
	h.t.Helper()
	r, err := h.runs.GetRun(h.ctx, runID)
	if err != nil {
		h.t.Errorf("get run: %v", err)
		return
	}
	if r.State == runpkg.StatePending {
		if _, err := h.runs.TransitionRun(h.ctx, runID, runpkg.StateRunning); err != nil {
			h.t.Errorf("run -> running: %v", err)
		}
	}
	// A runner's first trace report locks runner_kind (server/trace.go), which
	// is what makes the orchestrator park later stages at
	// awaiting_host_dispatch rather than firing the github_actions trigger.
	if !r.RunnerKindResolved {
		locker, ok := h.runs.(interface {
			ResolveRunnerKind(ctx context.Context, runID uuid.UUID, observed string) (runpkg.RunnerKindResolution, error)
		})
		if !ok {
			h.t.Fatalf("run repository cannot lock runner_kind")
		}
		if _, err := locker.ResolveRunnerKind(h.ctx, runID, runpkg.RunnerKindLocal); err != nil {
			h.t.Errorf("lock runner_kind: %v", err)
		}
	}
}

func (h *adE2EHarness) transitionStage(stageID uuid.UUID, states ...runpkg.StageState) {
	h.t.Helper()
	for _, to := range states {
		if _, err := h.runs.TransitionStage(h.ctx, stageID, to, nil); err != nil {
			h.t.Errorf("stage %s -> %s: %v", stageID, to, err)
			return
		}
	}
}

func (h *adE2EHarness) appendRunAudit(runID uuid.UUID, stageID *uuid.UUID, category string, payload any) {
	h.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("marshal %s: %v", category, err)
	}
	if _, err := h.audit.AppendChained(h.ctx, audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	}); err != nil {
		h.t.Errorf("append %s: %v", category, err)
	}
}

// emulatePlanParked is a plan runner's end state: the fixture plan artifact,
// a settled clean two-agent plan review, and the stage at awaiting_approval.
func (h *adE2EHarness) emulatePlanParked(runID, stageID uuid.UUID) {
	h.ensureRunRunning(runID)
	content := adE2EPlanJSON(h.t)
	sum := sha256.Sum256(content)
	schema := "standard_v1"
	if _, err := h.artifacts.Create(h.ctx, artifact.CreateParams{
		StageID: stageID, Kind: artifact.KindPlan, SchemaVersion: &schema,
		Content: content, ContentHash: hex.EncodeToString(sum[:]),
	}); err != nil {
		h.t.Errorf("store plan artifact: %v", err)
	}
	h.transitionStage(stageID, runpkg.StageStateRunning, runpkg.StageStateAwaitingApproval)
	sid := stageID
	h.appendRunAudit(runID, &sid, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	for range 2 {
		h.appendRunAudit(runID, &sid, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	}
}

// emulateImplementSucceeded is an implement runner's end state: the PR url on
// the run, the stage succeeded, and the orchestrator advanced.
func (h *adE2EHarness) emulateImplementSucceeded(runID, stageID uuid.UUID) {
	h.ensureRunRunning(runID)
	h.transitionStage(stageID, runpkg.StageStateRunning)
	if _, err := h.runs.SetRunPullRequestURL(h.ctx, runID, adE2EPRURL); err != nil {
		h.t.Errorf("set PR url: %v", err)
	}
	h.transitionStage(stageID, runpkg.StageStateSucceeded)
	if _, err := h.orch.Advance(h.ctx, runID); err != nil {
		h.t.Errorf("advance after implement: %v", err)
	}
}

// emulateImplementRunning leaves the implement runner mid-stage.
func (h *adE2EHarness) emulateImplementRunning(runID, stageID uuid.UUID) {
	h.ensureRunRunning(runID)
	h.transitionStage(stageID, runpkg.StageStateRunning)
}

// --- ticks and reads ---------------------------------------------------------

func (h *adE2EHarness) driverTick() {
	h.t.Helper()
	h.driver.Tick(h.ctx)
}

func (h *adE2EHarness) dispatcherTick() {
	h.t.Helper()
	if err := h.loop.tick(h.ctx); err != nil {
		h.t.Fatalf("dispatcher tick: %v", err)
	}
}

// tickBoth runs n rounds of one driver tick then one dispatcher tick.
func (h *adE2EHarness) tickBoth(n int) {
	h.t.Helper()
	for range n {
		h.driverTick()
		h.dispatcherTick()
	}
}

// runID returns the item's linked run (fatal when the driver has not started it).
func (h *adE2EHarness) runID() uuid.UUID {
	h.t.Helper()
	it, err := h.campaigns.GetCampaignItem(h.ctx, h.item.ID)
	if err != nil {
		h.t.Fatalf("get campaign item: %v", err)
	}
	if it.RunID == nil {
		h.t.Fatalf("campaign item %s has no linked run (state %s)", it.ID, it.State)
	}
	return *it.RunID
}

func (h *adE2EHarness) stageOf(runID uuid.UUID, typ runpkg.StageType) *runpkg.Stage {
	h.t.Helper()
	stages, err := h.runs.ListStagesForRun(h.ctx, runID)
	if err != nil {
		h.t.Fatalf("list stages: %v", err)
	}
	for _, st := range stages {
		if st.Type == typ {
			return st
		}
	}
	h.t.Fatalf("run %s has no %s stage", runID, typ)
	return nil
}

func (h *adE2EHarness) auditRows(runID uuid.UUID, category string) []*audit.Entry {
	h.t.Helper()
	rows, err := h.audit.ListForRunByCategory(h.ctx, runID, category)
	if err != nil {
		h.t.Fatalf("list %s rows: %v", category, err)
	}
	return rows
}

// status reads the campaign status through the real MCP client and returns
// the output and the item's awaiting block (nil when absent).
func (h *adE2EHarness) status(bearer string) (GetCampaignStatusOutput, *campaignItemAwaiting) {
	h.t.Helper()
	_, out, err := h.resolver(bearer).getCampaignStatus(h.ctx, nil, GetCampaignStatusInput{CampaignID: h.campaign.ID.String()})
	if err != nil {
		h.t.Fatalf("get campaign status: %v", err)
	}
	for i := range out.Items {
		if out.Items[i].IssueRef == h.item.IssueRef {
			return out, out.Items[i].Awaiting
		}
	}
	h.t.Fatalf("campaign status has no item %s: %+v", h.item.IssueRef, out.Items)
	return out, nil
}

// postApproval submits an approve on stageID under bearer and returns the
// HTTP status and the error code ("" on success).
func (h *adE2EHarness) postApproval(bearer string, stageID uuid.UUID) (int, string) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.ts.URL+"/v0/stages/"+stageID.String()+"/approvals",
		bytes.NewReader([]byte(`{"decision":"approve"}`)))
	if err != nil {
		h.t.Fatalf("build approval request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post approval: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	code := e.Error.Code
	if code == "" {
		code = e.Code
	}
	if resp.StatusCode >= 300 && code == "" {
		code = strings.TrimSpace(string(body))
	}
	return resp.StatusCode, code
}

func (h *adE2EHarness) approvalCount(stageID uuid.UUID) int {
	h.t.Helper()
	rows, err := h.approvals.ListForStage(h.ctx, stageID)
	if err != nil {
		h.t.Fatalf("list approvals: %v", err)
	}
	return len(rows)
}

// driveToPlanGate ticks until the plan stage parks at awaiting_approval and
// returns the run and plan stage ids.
func (h *adE2EHarness) driveToPlanGate() (uuid.UUID, uuid.UUID) {
	h.t.Helper()
	h.tickBoth(1)
	runID := h.runID()
	plan := h.stageOf(runID, runpkg.StageTypePlan)
	if plan.State != runpkg.StageStateAwaitingApproval {
		h.t.Fatalf("plan stage = %s after one driver+dispatcher round, want awaiting_approval (spawns %+v, dispatcher status %+v)",
			plan.State, h.spawner.recorded(), h.loop.status)
	}
	return runID, plan.ID
}

func spawnTypes(spawns []adE2ESpawn) []string {
	out := make([]string, 0, len(spawns))
	for _, s := range spawns {
		out = append(out, s.StageType)
	}
	return out
}

// --- (a) no hand dispatch ----------------------------------------------------

// TestAutoDriveE2E_NoHandDispatch is criterion (a): the plan and implement
// dispatches come only from the host dispatcher (its run_auto_driven acts and
// host-dispatch markers), and no dispatch row carries an operator subject. The
// dispatcher_disabled subtest is the issue's counterfactual: nothing else in
// the harness posts the marker, so without dispatcher ticks the plan stage
// stays undispatched and the status names the host dispatcher.
func TestAutoDriveE2E_NoHandDispatch(t *testing.T) {
	t.Run("dispatcher_enabled", func(t *testing.T) {
		h := newAdE2EHarness(t, adE2EPlanSpecYAML)
		runID, planID := h.driveToPlanGate()
		if code, ec := h.postApproval(h.operatorBearer, planID); code >= 300 {
			t.Fatalf("human plan approval = %d %s, want 2xx", code, ec)
		}
		h.tickBoth(adE2ETicks)

		if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan,implement" {
			t.Fatalf("spawns = %v, want exactly [plan implement] from the dispatcher", got)
		}
		acts := h.auditRows(runID, server.CategoryRunAutoDriven)
		var stages []string
		for _, e := range acts {
			if e.ActorSubject == nil || *e.ActorSubject != adE2EDispatcherSub {
				t.Errorf("run_auto_driven actor = %v, want %s (no operator subject)", e.ActorSubject, adE2EDispatcherSub)
			}
			var p struct {
				Action string `json:"action"`
				Stage  string `json:"stage"`
				Source string `json:"source"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode run_auto_driven: %v", err)
			}
			if p.Source != hostdispatcher.SourceTag {
				t.Errorf("run_auto_driven source = %q, want %q", p.Source, hostdispatcher.SourceTag)
			}
			stages = append(stages, p.Stage)
		}
		sort.Strings(stages)
		if strings.Join(stages, ",") != "implement,plan" {
			t.Errorf("run_auto_driven stages = %v, want one per dispatched stage (plan, implement)", stages)
		}
		for _, typ := range []runpkg.StageType{runpkg.StageTypePlan, runpkg.StageTypeImplement} {
			if st := h.stageOf(runID, typ); st.State != runpkg.StageStateSucceeded {
				t.Errorf("%s stage = %s, want succeeded", typ, st.State)
			}
		}
	})

	t.Run("dispatcher_disabled", func(t *testing.T) {
		h := newAdE2EHarness(t, adE2EPlanSpecYAML)
		for range adE2ETicks {
			h.driverTick()
		}
		runID := h.runID()
		plan := h.stageOf(runID, runpkg.StageTypePlan)
		if plan.State != runpkg.StageStatePending && plan.State != runpkg.StageStateAwaitingHostDispatch {
			t.Fatalf("plan stage = %s after %d driver ticks with no dispatcher, want pending/awaiting_host_dispatch", plan.State, adE2ETicks)
		}
		if n := len(h.spawner.recorded()); n != 0 {
			t.Fatalf("spawns = %d, want 0 with the dispatcher disabled", n)
		}
		if n := len(h.auditRows(runID, server.CategoryRunAutoDriven)); n != 0 {
			t.Fatalf("run_auto_driven rows = %d, want 0 with the dispatcher disabled", n)
		}
		_, blk := h.status(h.operatorBearer)
		if blk == nil || blk.Kind != "host_dispatcher" || blk.Reason != "host_dispatch" || blk.StageID != plan.ID.String() {
			t.Fatalf("awaiting = %+v, want host_dispatcher/host_dispatch on the plan stage", blk)
		}
	})
}

// --- (b) the plan gate parks across three identities ------------------------

// TestAutoDriveE2E_PlanGateParks is criterion (b): N driver and dispatcher
// ticks leave the plan gate parked with no approval row; the dispatcher token
// is refused by the route allowlist, the campaign operator-agent identity does
// not advance the gate, and only the human approval does, after which the next
// dispatcher tick spawns implement.
func TestAutoDriveE2E_PlanGateParks(t *testing.T) {
	h := newAdE2EHarness(t, adE2EPlanSpecYAML)
	h.spawner.emulate["implement"] = h.emulateImplementRunning
	runID, planID := h.driveToPlanGate()

	h.tickBoth(adE2ETicks)
	if st := h.stageOf(runID, runpkg.StageTypePlan); st.State != runpkg.StageStateAwaitingApproval {
		t.Fatalf("plan stage = %s after %d ticks, want awaiting_approval", st.State, adE2ETicks)
	}
	if n := h.approvalCount(planID); n != 0 {
		t.Fatalf("approval rows = %d after %d ticks, want 0 (nothing auto-approves a human-quorum gate)", n, adE2ETicks)
	}
	if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan" {
		t.Fatalf("spawns = %v, want only the plan", got)
	}

	code, ec := h.postApproval(h.dispatcherBearer, planID)
	if code != http.StatusForbidden || ec != "host_dispatcher_route_forbidden" {
		t.Fatalf("dispatcher-token approval = %d %q, want 403 host_dispatcher_route_forbidden", code, ec)
	}
	if n := h.approvalCount(planID); n != 0 {
		t.Fatalf("approval rows = %d after the dispatcher refusal, want 0", n)
	}

	agentCode, agentErr := h.postApproval(h.agentBearer, planID)
	t.Logf("operator-agent/campaign approval answered %d %q", agentCode, agentErr)
	h.tickBoth(1)
	if st := h.stageOf(runID, runpkg.StageTypePlan); st.State != runpkg.StageStateAwaitingApproval {
		t.Fatalf("plan stage = %s after an operator-agent approval, want awaiting_approval", st.State)
	}
	if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan" {
		t.Fatalf("spawns = %v after an operator-agent approval, want only the plan", got)
	}

	if code, ec := h.postApproval(h.operatorBearer, planID); code >= 300 {
		t.Fatalf("human approval = %d %s, want 2xx", code, ec)
	}
	if st := h.stageOf(runID, runpkg.StageTypePlan); st.State != runpkg.StageStateSucceeded {
		t.Fatalf("plan stage = %s after the human approval, want succeeded", st.State)
	}
	h.dispatcherTick()
	if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan,implement" {
		t.Fatalf("spawns = %v after the human approval and one dispatcher tick, want [plan implement]", got)
	}
}

// --- (d) a pending amendment: no respawn, no decision -----------------------

// TestAutoDriveE2E_PendingAmendmentNoRespawnNoDecision is criterion (d): a
// pending scope amendment on the running implement stage. Dispatcher ticks add
// no spawn and driver ticks add no decision marker; the status lists the
// amendment and reads human/scope_amendment_pending for an audit reader, and
// system/in_flight with decision_withheld for a caller without read:audit.
func TestAutoDriveE2E_PendingAmendmentNoRespawnNoDecision(t *testing.T) {
	h := newAdE2EHarness(t, adE2EPlanSpecYAML)
	h.spawner.emulate["implement"] = h.emulateImplementRunning
	runID, planID := h.driveToPlanGate()
	if code, ec := h.postApproval(h.operatorBearer, planID); code >= 300 {
		t.Fatalf("human approval = %d %s, want 2xx", code, ec)
	}
	h.tickBoth(1)
	impl := h.stageOf(runID, runpkg.StageTypeImplement)
	if impl.State != runpkg.StageStateRunning {
		t.Fatalf("implement stage = %s, want running", impl.State)
	}
	amend, err := h.amendments.Create(h.ctx, scopeamendment.CreateParams{
		RunID: runID, StageID: impl.ID, Reason: "a coupled test must change",
		Paths: []scopeamendment.PathEntry{{Path: "backend/internal/foo/foo_test.go", Operation: "modify"}},
	})
	if err != nil {
		t.Fatalf("create amendment: %v", err)
	}
	spawnsBefore := len(h.spawner.recorded())
	markersBefore := len(h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionRequired))

	for range adE2ETicks {
		h.dispatcherTick()
	}
	if n := len(h.spawner.recorded()); n != spawnsBefore {
		t.Fatalf("spawns = %d after %d dispatcher ticks, want %d (no respawn of a running stage)", n, adE2ETicks, spawnsBefore)
	}
	for range adE2ETicks {
		h.driverTick()
	}
	if n := len(h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionRequired)); n != markersBefore {
		t.Fatalf("decision_required rows = %d after %d driver ticks, want %d (an amendment is not a gate decision)", n, adE2ETicks, markersBefore)
	}

	out, blk := h.status(h.operatorBearer)
	if out.PendingAmendments == nil || len(out.PendingAmendments.Items) != 1 ||
		out.PendingAmendments.Items[0].Amendment.ID != amend.ID.String() || !out.PendingAmendments.Items[0].DecisionLive {
		t.Fatalf("pending_amendments = %+v, want the one live amendment %s", out.PendingAmendments, amend.ID)
	}
	if blk == nil || blk.Kind != "human" || blk.Reason != "scope_amendment_pending" || blk.StageID != impl.ID.String() || blk.DecisionWithheld {
		t.Fatalf("awaiting (operator) = %+v, want human/scope_amendment_pending on the implement stage", blk)
	}

	_, blk = h.status(h.noAuditBearer)
	if blk == nil || blk.Kind != "system" || blk.Reason != "in_flight" || !blk.DecisionWithheld || blk.Decision != nil {
		t.Fatalf("awaiting (no read:audit) = %+v, want system/in_flight with decision_withheld", blk)
	}
}

// --- (e) one marker per occurrence, one clear after the human approval -------

// TestAutoDriveE2E_DecisionMarkerOncePerOccurrence is criterion (e): N ticks
// at the plan gate record exactly one campaign_gate_decision_required row
// (exact payload key set, gate = the plan stage, human_quorum_required,
// approve), the status shows it on a human/gate_approval block, and after the
// human approval the next driver tick records exactly one
// campaign_gate_decision_cleared row and the status no longer shows it.
func TestAutoDriveE2E_DecisionMarkerOncePerOccurrence(t *testing.T) {
	h := newAdE2EHarness(t, adE2EPlanSpecYAML)
	h.spawner.emulate["implement"] = h.emulateImplementRunning
	runID, planID := h.driveToPlanGate()
	h.tickBoth(adE2ETicks)

	markers := h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionRequired)
	if len(markers) != 1 {
		t.Fatalf("decision_required rows = %d after %d ticks at the plan gate, want exactly 1", len(markers), adE2ETicks+1)
	}
	var payload map[string]any
	if err := json.Unmarshal(markers[0].Payload, &payload); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "action,campaign_id,decision_state,gate,issue_ref,run_id"; got != want {
		t.Fatalf("marker payload keys = %s, want exactly %s", got, want)
	}
	if payload["gate"] != planID.String() || payload["decision_state"] != operatorrole.DecisionStateHumanQuorumRequired ||
		payload["action"] != "approve" || payload["run_id"] != runID.String() ||
		payload["campaign_id"] != h.campaign.ID.String() || payload["issue_ref"] != h.item.IssueRef {
		t.Fatalf("marker payload = %v, want the plan gate human_quorum_required/approve", payload)
	}
	if markers[0].StageID == nil || *markers[0].StageID != planID {
		t.Fatalf("marker stage_id = %v, want the plan stage %s", markers[0].StageID, planID)
	}
	if n := len(h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionCleared)); n != 0 {
		t.Fatalf("decision_cleared rows = %d while parked, want 0", n)
	}

	_, blk := h.status(h.operatorBearer)
	if blk == nil || blk.Kind != "human" || blk.Reason != "gate_approval" || blk.StageID != planID.String() {
		t.Fatalf("awaiting = %+v, want human/gate_approval on the plan stage", blk)
	}
	if d := blk.Decision; d == nil || d.Gate != planID.String() || d.DecisionState != operatorrole.DecisionStateHumanQuorumRequired || d.Action != "approve" {
		t.Fatalf("awaiting decision = %+v, want the plan-gate marker", blk.Decision)
	}

	if code, ec := h.postApproval(h.operatorBearer, planID); code >= 300 {
		t.Fatalf("human approval = %d %s, want 2xx", code, ec)
	}
	h.driverTick()
	cleared := h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionCleared)
	if len(cleared) != 1 {
		t.Fatalf("decision_cleared rows = %d after the approval and one driver tick, want exactly 1", len(cleared))
	}
	var cp map[string]any
	if err := json.Unmarshal(cleared[0].Payload, &cp); err != nil {
		t.Fatalf("decode clear: %v", err)
	}
	if cp["gate"] != planID.String() {
		t.Fatalf("clear payload = %v, want gate %s", cp, planID)
	}
	h.tickBoth(adE2ETicks)
	if n := len(h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionCleared)); n != 1 {
		t.Fatalf("decision_cleared rows = %d after %d more ticks, want still 1", n, adE2ETicks)
	}
	if n := len(h.auditRows(runID, campaigndriver.CategoryCampaignGateDecisionRequired)); n != 1 {
		t.Fatalf("decision_required rows = %d after the approval, want still 1", n)
	}
	_, blk = h.status(h.operatorBearer)
	if blk != nil && blk.Decision != nil {
		t.Fatalf("awaiting = %+v after the approval, want no decision", blk)
	}
}
