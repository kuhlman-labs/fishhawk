package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Tests for the per-item awaiting block on the campaign status read
// (E88.4 / #4528). Every fixture uses acme/* repos (ADR-110).

func awaitStage(seq int, typ run.StageType, state run.StageState) *run.Stage {
	return &run.Stage{ID: uuid.New(), Sequence: seq, Type: typ, State: state}
}

func awaitRun(autoDispatch bool, runnerKind string) *run.Run {
	return &run.Run{ID: uuid.New(), State: run.StateRunning, AutoDispatch: autoDispatch, RunnerKind: runnerKind}
}

func TestClassifyCampaignItemAwaiting(t *testing.T) {
	marked := awaitRun(true, run.RunnerKindLocal)
	cases := []struct {
		name       string
		r          *run.Run
		stages     []*run.Stage
		wantKind   string
		wantReason string
		wantStage  int // index into stages; -1 = no stage fields
	}{
		{"marked local pending", marked, []*run.Stage{awaitStage(1, run.StageTypePlan, run.StageStatePending)}, awaitingKindHostDispatcher, awaitingReasonHostDispatch, 0},
		{"marked local awaiting_host_dispatch", marked, []*run.Stage{awaitStage(1, run.StageTypePlan, run.StageStateAwaitingHostDispatch)}, awaitingKindHostDispatcher, awaitingReasonHostDispatch, 0},
		{"unmarked local pending", awaitRun(false, run.RunnerKindLocal), []*run.Stage{awaitStage(1, run.StageTypePlan, run.StageStatePending)}, awaitingKindHuman, awaitingReasonOperatorDispatch, 0},
		{"marked github_actions pending", awaitRun(true, run.RunnerKindGitHubActions), []*run.Stage{awaitStage(1, run.StageTypePlan, run.StageStatePending)}, awaitingKindHuman, awaitingReasonOperatorDispatch, 0},
		{"dispatched", marked, []*run.Stage{awaitStage(1, run.StageTypeImplement, run.StageStateDispatched)}, awaitingKindSystem, awaitingReasonInFlight, 0},
		{"running", marked, []*run.Stage{awaitStage(1, run.StageTypeImplement, run.StageStateRunning)}, awaitingKindSystem, awaitingReasonInFlight, 0},
		{"awaiting_deployment", marked, []*run.Stage{awaitStage(1, run.StageTypeDeploy, run.StageStateAwaitingDeployment)}, awaitingKindSystem, awaitingReasonInFlight, 0},
		{"plan awaiting_approval", marked, []*run.Stage{awaitStage(1, run.StageTypePlan, run.StageStateAwaitingApproval)}, awaitingKindHuman, awaitingReasonGateApproval, 0},
		{"review awaiting_approval", marked, []*run.Stage{awaitStage(1, run.StageTypeReview, run.StageStateAwaitingApproval)}, awaitingKindHuman, awaitingReasonGateApproval, 0},
		{"awaiting_deploy_approval", marked, []*run.Stage{awaitStage(1, run.StageTypeDeploy, run.StageStateAwaitingDeployApproval)}, awaitingKindHuman, awaitingReasonDeployApproval, 0},
		{"awaiting_input", marked, []*run.Stage{awaitStage(1, run.StageTypePlan, run.StageStateAwaitingInput)}, awaitingKindHuman, awaitingReasonInput, 0},
		{"awaiting_scope_decision", marked, []*run.Stage{awaitStage(1, run.StageTypeImplement, run.StageStateAwaitingScopeDecision)}, awaitingKindHuman, awaitingReasonScopeDecision, 0},
		{"awaiting_children", marked, []*run.Stage{awaitStage(1, run.StageTypeImplement, run.StageStateAwaitingChildren)}, awaitingKindSystem, awaitingReasonChildren, 0},
		{"all stages terminal", marked, []*run.Stage{
			awaitStage(1, run.StageTypePlan, run.StageStateSucceeded),
			awaitStage(2, run.StageTypeImplement, run.StageStateSucceeded),
			awaitStage(3, run.StageTypeReview, run.StageStateSuperseded),
		}, awaitingKindSystem, awaitingReasonNoActiveStage, -1},
		// A category-C infrastructure park (#4426 implement): the run stays
		// running with implement failed and acceptance pending. The
		// host-dispatch marker refuses acceptance (earlier_stage_failed), so
		// the block is human/stage_retry on the FAILED stage, never
		// host_dispatch on the pending one.
		{"failed implement ahead of pending acceptance, marked local", marked, []*run.Stage{
			awaitStage(1, run.StageTypePlan, run.StageStateSucceeded),
			awaitStage(2, run.StageTypeImplement, run.StageStateFailed),
			awaitStage(3, run.StageTypeAcceptance, run.StageStatePending),
		}, awaitingKindHuman, awaitingReasonStageRetry, 1},
		{"failed implement ahead of pending acceptance, unmarked", awaitRun(false, run.RunnerKindLocal), []*run.Stage{
			awaitStage(1, run.StageTypePlan, run.StageStateSucceeded),
			awaitStage(2, run.StageTypeImplement, run.StageStateFailed),
			awaitStage(3, run.StageTypeAcceptance, run.StageStatePending),
		}, awaitingKindHuman, awaitingReasonStageRetry, 1},
		// #4356 acceptance park with no later stage: not no_active_stage.
		{"failed last stage on a live run", marked, []*run.Stage{
			awaitStage(1, run.StageTypePlan, run.StageStateSucceeded),
			awaitStage(2, run.StageTypeImplement, run.StageStateSucceeded),
			awaitStage(3, run.StageTypeAcceptance, run.StageStateFailed),
		}, awaitingKindHuman, awaitingReasonStageRetry, 2},
		// Earliest non-terminal by SEQUENCE, not slice order: the slice lists
		// the later pending review first.
		{"earliest non-terminal selection", marked, []*run.Stage{
			awaitStage(3, run.StageTypeReview, run.StageStatePending),
			awaitStage(1, run.StageTypePlan, run.StageStateSucceeded),
			awaitStage(2, run.StageTypeImplement, run.StageStateAwaitingApproval),
		}, awaitingKindHuman, awaitingReasonGateApproval, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCampaignItemAwaiting(tc.r, tc.stages)
			if got.Kind != tc.wantKind || got.Reason != tc.wantReason {
				t.Fatalf("kind/reason = %s/%s, want %s/%s", got.Kind, got.Reason, tc.wantKind, tc.wantReason)
			}
			if got.RunID != tc.r.ID.String() {
				t.Fatalf("run_id = %q, want %s", got.RunID, tc.r.ID)
			}
			if got.NextStep == "" || got.NextStep != awaitingNextSteps[tc.wantReason] {
				t.Fatalf("next_step = %q, want the %s constant", got.NextStep, tc.wantReason)
			}
			if tc.wantStage < 0 {
				if got.StageID != "" || got.StageType != "" || got.StageState != "" {
					t.Fatalf("stage fields = %q/%q/%q, want none", got.StageID, got.StageType, got.StageState)
				}
				return
			}
			st := tc.stages[tc.wantStage]
			if got.StageID != st.ID.String() || got.StageType != string(st.Type) || got.StageState != string(st.State) {
				t.Fatalf("stage = %s/%s/%s, want %s/%s/%s", got.StageID, got.StageType, got.StageState, st.ID, st.Type, st.State)
			}
		})
	}
}

// awaitMarkerEntry builds a decision_required (or cleared) row for gate.
func awaitMarkerEntry(seq int64, category, gate string) *audit.Entry {
	body, _ := json.Marshal(map[string]any{
		"campaign_id": uuid.NewString(), "issue_ref": "issue:1", "run_id": uuid.NewString(),
		"gate": gate, "decision_state": "human_quorum_required", "action": "approve",
	})
	return &audit.Entry{Sequence: seq, Category: category, Timestamp: time.Unix(1_700_000_000+seq, 0).UTC(), Payload: body}
}

func TestOverlayCampaignItemAwaiting(t *testing.T) {
	r := awaitRun(true, run.RunnerKindLocal)
	runID := r.ID.String()
	gateStage := awaitStage(1, run.StageTypePlan, run.StageStateAwaitingApproval)
	inFlight := awaitStage(2, run.StageTypeImplement, run.StageStateRunning)
	children := awaitStage(2, run.StageTypeImplement, run.StageStateAwaitingChildren)
	reopened := awaitStage(1, run.StageTypePlan, run.StageStatePending)
	foreign := uuid.NewString()
	req := campaigndriver.CategoryCampaignGateDecisionRequired
	clr := campaigndriver.CategoryCampaignGateDecisionCleared
	runLevel := campaigndriver.GateRunLevel
	none := &campaignPendingAmendmentsPayload{Items: []campaignPendingAmendmentPayload{}}
	liveFor := func(id string, live bool) *campaignPendingAmendmentsPayload {
		return &campaignPendingAmendmentsPayload{Items: []campaignPendingAmendmentPayload{{ItemRunID: id, DecisionLive: live}}}
	}
	terminal := []*run.Stage{awaitStage(1, run.StageTypePlan, run.StageStateSucceeded)}

	cases := []struct {
		name            string
		stages          []*run.Stage
		readable        bool
		pending         *campaignPendingAmendmentsPayload
		markers, clears []*audit.Entry
		markerErr       error
		wantKind        string
		wantReason      string
		wantGate        string // "" = no decision
		wantWithheld    bool
		wantUnavailable bool
		wantLoad        bool
	}{
		{name: "matching stage marker shown", stages: []*run.Stage{gateStage}, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(4, req, gateStage.ID.String())},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantGate: gateStage.ID.String(), wantLoad: true},
		{name: "run-level marker on a human gate keeps the reason", stages: []*run.Stage{gateStage}, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(4, req, runLevel)},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantGate: runLevel, wantLoad: true},
		{name: "run-level marker on no_active_stage", stages: terminal, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(4, req, runLevel)},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonRunDecision, wantGate: runLevel, wantLoad: true},
		{name: "run-level marker on in_flight", stages: []*run.Stage{inFlight}, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(4, req, runLevel)},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonRunDecision, wantGate: runLevel, wantLoad: true},
		{name: "foreign-stage marker omitted", stages: []*run.Stage{gateStage}, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(4, req, foreign)},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantLoad: true},
		// C2: the SAME stage reopened awaiting_approval -> pending (a fix-up)
		// before the driver cleared the marker hides the decision.
		{name: "same stage reopened to pending hides the marker", stages: []*run.Stage{reopened}, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(4, req, reopened.ID.String())},
			wantKind: awaitingKindHostDispatcher, wantReason: awaitingReasonHostDispatch, wantLoad: true},
		// The stage is STILL awaiting_approval, so only the cleared check hides it.
		{name: "marker followed by cleared omitted", stages: []*run.Stage{gateStage}, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(4, req, gateStage.ID.String())},
			clears:   []*audit.Entry{awaitMarkerEntry(5, clr, gateStage.ID.String())},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantLoad: true},
		{name: "marker newer than cleared shown", stages: []*run.Stage{gateStage}, readable: true, pending: none,
			markers:  []*audit.Entry{awaitMarkerEntry(2, req, gateStage.ID.String()), awaitMarkerEntry(6, req, gateStage.ID.String())},
			clears:   []*audit.Entry{awaitMarkerEntry(5, clr, gateStage.ID.String())},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantGate: gateStage.ID.String(), wantLoad: true},
		{name: "live amendment on in_flight", stages: []*run.Stage{inFlight}, readable: true, pending: liveFor(runID, true),
			wantKind: awaitingKindHuman, wantReason: awaitingReasonScopeAmendmentPending, wantLoad: true},
		// C3: a decomposed parent at awaiting_children whose child filed a
		// live amendment.
		{name: "live amendment on children", stages: []*run.Stage{children}, readable: true, pending: liveFor(runID, true),
			wantKind: awaitingKindHuman, wantReason: awaitingReasonScopeAmendmentPending, wantLoad: true},
		{name: "live amendment of another item run", stages: []*run.Stage{inFlight}, readable: true, pending: liveFor(uuid.NewString(), true),
			wantKind: awaitingKindSystem, wantReason: awaitingReasonInFlight, wantLoad: true},
		{name: "settled amendment is not live", stages: []*run.Stage{inFlight}, readable: true, pending: liveFor(runID, false),
			wantKind: awaitingKindSystem, wantReason: awaitingReasonInFlight, wantLoad: true},
		{name: "unreadable withholds with a current marker and a live amendment", stages: []*run.Stage{inFlight}, readable: false,
			pending: liveFor(runID, true), markers: []*audit.Entry{awaitMarkerEntry(4, req, runLevel)},
			wantKind: awaitingKindSystem, wantReason: awaitingReasonInFlight, wantWithheld: true},
		{name: "unreadable gate block keeps its classification", stages: []*run.Stage{gateStage}, readable: false,
			pending: nil, markers: []*audit.Entry{awaitMarkerEntry(4, req, gateStage.ID.String())},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantWithheld: true},
		{name: "nil pending block is unavailable", stages: []*run.Stage{inFlight}, readable: true, pending: nil,
			wantKind: awaitingKindSystem, wantReason: awaitingReasonInFlight, wantUnavailable: true, wantLoad: true},
		{name: "truncated without this run is unavailable", stages: []*run.Stage{inFlight}, readable: true,
			pending:  &campaignPendingAmendmentsPayload{Items: []campaignPendingAmendmentPayload{{ItemRunID: uuid.NewString(), DecisionLive: true}}, Truncated: true},
			wantKind: awaitingKindSystem, wantReason: awaitingReasonInFlight, wantUnavailable: true, wantLoad: true},
		{name: "truncated listing this run's live amendment applies", stages: []*run.Stage{inFlight}, readable: true,
			pending:  &campaignPendingAmendmentsPayload{Items: []campaignPendingAmendmentPayload{{ItemRunID: runID, DecisionLive: true}}, Truncated: true},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonScopeAmendmentPending, wantLoad: true},
		{name: "marker read error is unavailable", stages: []*run.Stage{gateStage}, readable: true, pending: none,
			markerErr: errors.New("audit down"),
			wantKind:  awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantUnavailable: true, wantLoad: true},
		{name: "malformed marker payload not shown", stages: []*run.Stage{gateStage}, readable: true, pending: none,
			markers:  []*audit.Entry{{Sequence: 4, Category: req, Payload: json.RawMessage(`{"gate":`)}},
			wantKind: awaitingKindHuman, wantReason: awaitingReasonGateApproval, wantLoad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loaded := false
			got := overlayCampaignItemAwaiting(classifyCampaignItemAwaiting(r, tc.stages), tc.readable, tc.pending, func() (*awaitingMarker, error) {
				loaded = true
				if tc.markerErr != nil {
					return nil, tc.markerErr
				}
				return currentAwaitingMarker(tc.markers, tc.clears), nil
			})
			if got.Kind != tc.wantKind || got.Reason != tc.wantReason {
				t.Fatalf("kind/reason = %s/%s, want %s/%s", got.Kind, got.Reason, tc.wantKind, tc.wantReason)
			}
			if got.NextStep != awaitingNextSteps[tc.wantReason] {
				t.Fatalf("next_step = %q, want the %s constant", got.NextStep, tc.wantReason)
			}
			if got.DecisionWithheld != tc.wantWithheld || got.DecisionUnavailable != tc.wantUnavailable {
				t.Fatalf("withheld/unavailable = %v/%v, want %v/%v", got.DecisionWithheld, got.DecisionUnavailable, tc.wantWithheld, tc.wantUnavailable)
			}
			if loaded != tc.wantLoad {
				t.Fatalf("marker loaded = %v, want %v", loaded, tc.wantLoad)
			}
			if tc.wantGate == "" {
				if got.Decision != nil {
					t.Fatalf("decision = %+v, want none", got.Decision)
				}
				return
			}
			if got.Decision == nil || got.Decision.Gate != tc.wantGate ||
				got.Decision.DecisionState != "human_quorum_required" || got.Decision.Action != "approve" || got.Decision.RecordedAt.IsZero() {
				t.Fatalf("decision = %+v, want gate %s human_quorum_required/approve", got.Decision, tc.wantGate)
			}
		})
	}
}

// awaitingAuditFake serves seeded run-chain rows by (run, category) and
// records every ListForRunByCategory run id.
type awaitingAuditFake struct {
	audit.BaseFake
	mu      sync.Mutex
	rows    map[uuid.UUID][]*audit.Entry
	queried []uuid.UUID
	err     error
}

func newAwaitingAuditFake() *awaitingAuditFake {
	return &awaitingAuditFake{rows: map[uuid.UUID][]*audit.Entry{}}
}

func (a *awaitingAuditFake) seed(runID uuid.UUID, seq int64, category, gate string) {
	e := awaitMarkerEntry(seq, category, gate)
	a.mu.Lock()
	a.rows[runID] = append(a.rows[runID], e)
	a.mu.Unlock()
}

func (a *awaitingAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queried = append(a.queried, runID)
	if a.err != nil {
		return nil, a.err
	}
	var out []*audit.Entry
	for _, e := range a.rows[runID] {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *awaitingAuditFake) queriedRuns() []uuid.UUID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uuid.UUID(nil), a.queried...)
}

// awaitingRunRepo adds a ListStagesForRun error knob to the pending-amendment
// run fake (whose ListRuns honours the lineage filters).
type awaitingRunRepo struct {
	*pendingAmendRunRepo
	stagesErr error
}

func (r *awaitingRunRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.stagesErr != nil {
		return nil, r.stagesErr
	}
	return r.pendingAmendRunRepo.ListStagesForRun(ctx, runID)
}

type awaitingFixture struct {
	s      *Server
	rr     *awaitingRunRepo
	au     *awaitingAuditFake
	c      *campaign.Campaign
	other  *campaign.Campaign
	host   *run.Run // marked local, plan pending
	manual *run.Run // unmarked local, plan pending
	gate   *run.Run // plan awaiting_approval with a current matching marker
	gateSt *run.Stage
	alien  *run.Run // another campaign's run carrying a marker
}

// newAwaitingFixture builds campaign C (acme/widgets) with three running
// items — host (marked local, plan pending), manual (unmarked local, plan
// pending) and gate (plan awaiting_approval, current matching marker) — and
// campaign O (acme/gadgets) whose run alien carries a current run-level
// marker.
func newAwaitingFixture(t *testing.T) *awaitingFixture {
	t.Helper()
	rr := &awaitingRunRepo{pendingAmendRunRepo: &pendingAmendRunRepo{orchestratorRepo: newOrchestratorRepo()}}
	cr := &holdCampaignRepo{fakeCampaignRepo: newFakeCampaignRepo()}
	ar := &pendingAmendAmendRepo{fakeScopeAmendmentRepo: newFakeScopeAmendmentRepo()}
	au := newAwaitingAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, CampaignRepo: cr, ScopeAmendmentRepo: ar, AuditRepo: au})
	f := &awaitingFixture{s: s, rr: rr, au: au}
	mk := func(repo string, marked bool, state run.StageState) (*run.Run, *run.Stage) {
		r := seedPARun(rr.pendingAmendRunRepo, repo, nil, nil)
		r.AutoDispatch = marked
		r.RunnerKind = run.RunnerKindLocal
		st := rr.seedStage(r.ID, 1, state)
		return r, st
	}
	f.host, _ = mk("acme/widgets", true, run.StageStatePending)
	f.manual, _ = mk("acme/widgets", false, run.StageStatePending)
	f.gate, f.gateSt = mk("acme/widgets", true, run.StageStateAwaitingApproval)
	au.seed(f.gate.ID, 3, campaigndriver.CategoryCampaignGateDecisionRequired, f.gateSt.ID.String())
	f.c = cr.seedCampaignWithItems("acme/widgets", "issue:10", []*campaign.Item{
		{IssueRef: "issue:1", State: campaign.ItemStateRunning, RunID: idp(f.host.ID), Position: 0},
		{IssueRef: "issue:2", State: campaign.ItemStateRunning, RunID: idp(f.manual.ID), Position: 1},
		{IssueRef: "issue:3", State: campaign.ItemStateRunning, RunID: idp(f.gate.ID), Position: 2},
	})
	f.alien, _ = mk("acme/gadgets", true, run.StageStateRunning)
	au.seed(f.alien.ID, 2, campaigndriver.CategoryCampaignGateDecisionRequired, campaigndriver.GateRunLevel)
	f.other = cr.seedCampaignWithItems("acme/gadgets", "issue:70", []*campaign.Item{
		{IssueRef: "issue:7", State: campaign.ItemStateRunning, RunID: idp(f.alien.ID)},
	})
	return f
}

// statusItemsRaw decodes the status body's items as raw key maps, so absent
// keys are asserted on the wire rather than through zero values.
func statusItemsRaw(t *testing.T, w *httptest.ResponseRecorder) []map[string]json.RawMessage {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return body.Items
}

// statusAwaitingByRef returns each item's decoded awaiting block keyed by
// issue_ref (nil when the key is absent) and its raw key set.
func statusAwaitingByRef(t *testing.T, w *httptest.ResponseRecorder) (map[string]*campaignItemAwaitingPayload, map[string][]string) {
	t.Helper()
	out := map[string]*campaignItemAwaitingPayload{}
	keys := map[string][]string{}
	for _, it := range statusItemsRaw(t, w) {
		var ref string
		_ = json.Unmarshal(it["issue_ref"], &ref)
		raw, ok := it["awaiting"]
		if !ok {
			out[ref] = nil
			continue
		}
		var blk campaignItemAwaitingPayload
		if err := json.Unmarshal(raw, &blk); err != nil {
			t.Fatalf("decode awaiting: %v", err)
		}
		out[ref] = &blk
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		for k := range m {
			keys[ref] = append(keys[ref], k)
		}
		sort.Strings(keys[ref])
	}
	return out, keys
}

func TestCampaignStatus_Awaiting_ThroughHandler(t *testing.T) {
	f := newAwaitingFixture(t)
	got, keys := statusAwaitingByRef(t, callCampaignStatusAs(f.s, f.c.ID, gateViewReadIdentity()))
	host := got["issue:1"]
	if host == nil || host.Kind != awaitingKindHostDispatcher || host.Reason != awaitingReasonHostDispatch || host.RunID != f.host.ID.String() || host.Decision != nil {
		t.Fatalf("host item awaiting = %+v, want host_dispatcher/host_dispatch with no decision", host)
	}
	manual := got["issue:2"]
	if manual == nil || manual.Kind != awaitingKindHuman || manual.Reason != awaitingReasonOperatorDispatch {
		t.Fatalf("unmarked item awaiting = %+v, want human/operator_dispatch", manual)
	}
	gate := got["issue:3"]
	if gate == nil || gate.Kind != awaitingKindHuman || gate.Reason != awaitingReasonGateApproval ||
		gate.StageID != f.gateSt.ID.String() || gate.StageType != string(run.StageTypePlan) || gate.StageState != string(run.StageStateAwaitingApproval) {
		t.Fatalf("gate item awaiting = %+v, want human/gate_approval on the plan stage", gate)
	}
	if gate.Decision == nil || gate.Decision.Gate != f.gateSt.ID.String() || gate.Decision.DecisionState != "human_quorum_required" || gate.Decision.Action != "approve" {
		t.Fatalf("gate decision = %+v, want the matching marker", gate.Decision)
	}
	if gate.DecisionWithheld || gate.DecisionUnavailable {
		t.Fatalf("gate withheld/unavailable = %v/%v, want false/false", gate.DecisionWithheld, gate.DecisionUnavailable)
	}
	wantKeys := "decision,kind,next_step,reason,run_id,stage_id,stage_state,stage_type"
	if strings.Join(keys["issue:3"], ",") != wantKeys {
		t.Fatalf("gate awaiting keys = %v, want exactly %s", keys["issue:3"], wantKeys)
	}
	if strings.Join(keys["issue:1"], ",") != "kind,next_step,reason,run_id,stage_id,stage_state,stage_type" {
		t.Fatalf("host awaiting keys = %v", keys["issue:1"])
	}
}

// CF14 through the handler: the gate run's plan stage is STILL
// awaiting_approval and the marker names it, so the foreign-stage and C2
// checks both pass; only the later campaign_gate_decision_cleared row hides
// the decision (currentAwaitingMarker).
func TestCampaignStatus_Awaiting_ClearRowHidesDecisionOnParkedStage(t *testing.T) {
	f := newAwaitingFixture(t)
	f.au.seed(f.gate.ID, 4, campaigndriver.CategoryCampaignGateDecisionCleared, f.gateSt.ID.String())
	got, keys := statusAwaitingByRef(t, callCampaignStatusAs(f.s, f.c.ID, gateViewReadIdentity()))
	gate := got["issue:3"]
	if gate == nil || gate.Kind != awaitingKindHuman || gate.Reason != awaitingReasonGateApproval ||
		gate.StageID != f.gateSt.ID.String() || gate.StageState != string(run.StageStateAwaitingApproval) {
		t.Fatalf("gate item awaiting = %+v, want human/gate_approval on the still-parked plan stage", gate)
	}
	if gate.Decision != nil {
		t.Fatalf("gate decision = %+v, want none once a later cleared row follows the marker", gate.Decision)
	}
	if gate.DecisionWithheld || gate.DecisionUnavailable {
		t.Fatalf("gate withheld/unavailable = %v/%v, want false/false", gate.DecisionWithheld, gate.DecisionUnavailable)
	}
	if strings.Join(keys["issue:3"], ",") != "kind,next_step,reason,run_id,stage_id,stage_state,stage_type" {
		t.Fatalf("gate awaiting keys = %v, want no decision key", keys["issue:3"])
	}
}

// A run-bound mcp:run:<id> token WITH read:audit still never reads decision
// sources (mirrors TestCampaignStatus_PendingAmendmentsOmittedForRunBoundCaller).
func TestCampaignStatus_Awaiting_DecisionWithheld_RunBoundToken(t *testing.T) {
	f := newAwaitingFixture(t)
	id := Identity{Subject: "mcp:run:" + f.gate.ID.String(), TokenID: "tok-run", Scopes: []string{scopeGateViewRead}}
	assertAwaitingWithheld(t, f, id)
}

func TestCampaignStatus_Awaiting_DecisionWithheld_TokenWithoutReadAudit(t *testing.T) {
	f := newAwaitingFixture(t)
	assertAwaitingWithheld(t, f, Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"read:runs"}})
}

func assertAwaitingWithheld(t *testing.T, f *awaitingFixture, id Identity) {
	t.Helper()
	got, _ := statusAwaitingByRef(t, callCampaignStatusAs(f.s, f.c.ID, id))
	for _, ref := range []string{"issue:1", "issue:2", "issue:3"} {
		blk := got[ref]
		if blk == nil || !blk.DecisionWithheld || blk.Decision != nil || blk.DecisionUnavailable {
			t.Fatalf("%s awaiting = %+v, want decision_withheld with no decision", ref, blk)
		}
	}
	if gate := got["issue:3"]; gate.Kind != awaitingKindHuman || gate.Reason != awaitingReasonGateApproval {
		t.Fatalf("gate classification = %s/%s, want human/gate_approval unchanged", gate.Kind, gate.Reason)
	}
	if q := f.au.queriedRuns(); len(q) != 0 {
		t.Fatalf("decision marker reads = %v, want none for an unreadable caller", q)
	}
}

// Every marker read names an item's own linked run; the other campaign's run
// (which carries a current marker) is never queried or shown.
func TestCampaignStatus_Awaiting_AuditReadScopedToItemRuns(t *testing.T) {
	f := newAwaitingFixture(t)
	w := callCampaignStatusAs(f.s, f.c.ID, gateViewReadIdentity())
	_, _ = statusAwaitingByRef(t, w)
	itemRuns := map[uuid.UUID]bool{f.host.ID: true, f.manual.ID: true, f.gate.ID: true}
	q := f.au.queriedRuns()
	if len(q) == 0 {
		t.Fatal("no marker read issued; the scoping assertion would be vacuous")
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range q {
		if !itemRuns[id] {
			t.Fatalf("marker read for run %s, not one of the campaign's item runs", id)
		}
		seen[id] = true
	}
	if len(seen) != len(itemRuns) {
		t.Fatalf("marker reads covered %d item runs, want %d", len(seen), len(itemRuns))
	}
	if strings.Contains(w.Body.String(), f.alien.ID.String()) {
		t.Fatalf("status leaked the other campaign's run:\n%s", w.Body.String())
	}
}

// Unstarted, terminal and run-terminal items, and the items-list route, keep
// their exact wire shape: no awaiting key.
func TestCampaignStatus_Awaiting_AbsentForUnstartedAndTerminal(t *testing.T) {
	f := newAwaitingFixture(t)
	done := seedPARun(f.rr.pendingAmendRunRepo, "acme/widgets", nil, nil)
	done.State = run.StateSucceeded
	f.rr.seedStage(done.ID, 1, run.StageStateSucceeded)
	liveRun := seedPARun(f.rr.pendingAmendRunRepo, "acme/widgets", nil, nil)
	f.rr.seedStage(liveRun.ID, 1, run.StageStateRunning)
	cr := f.s.cfg.CampaignRepo.(*holdCampaignRepo)
	c := cr.seedCampaignWithItems("acme/widgets", "issue:20", []*campaign.Item{
		{IssueRef: "issue:21", State: campaign.ItemStatePending, Position: 0},
		{IssueRef: "issue:22", State: campaign.ItemStateSucceeded, RunID: idp(liveRun.ID), Position: 1},
		{IssueRef: "issue:23", State: campaign.ItemStateRunning, RunID: idp(done.ID), Position: 2},
	})
	for _, it := range statusItemsRaw(t, callCampaignStatusAs(f.s, c.ID, gateViewReadIdentity())) {
		if _, ok := it["awaiting"]; ok {
			t.Fatalf("item %s carries awaiting, want the key absent", it["issue_ref"])
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+f.c.ID.String()+"/items", nil)
	req.SetPathValue("campaign_id", f.c.ID.String())
	f.s.handleListCampaignItems(w, injectIdentity(req, gateViewReadIdentity()))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"awaiting"`) {
		t.Fatalf("items list = %d %s, want 200 with no awaiting key", w.Code, w.Body.String())
	}
}

func TestCampaignStatus_Awaiting_StageReadErrorOmitsBlock(t *testing.T) {
	f := newAwaitingFixture(t)
	f.rr.stagesErr = errors.New("stages down")
	items := statusItemsRaw(t, callCampaignStatusAs(f.s, f.c.ID, gateViewReadIdentity()))
	if len(items) != 3 {
		t.Fatalf("items = %d, want the status intact with 3 items", len(items))
	}
	for _, it := range items {
		if _, ok := it["awaiting"]; ok {
			t.Fatalf("item %s carries awaiting on a stage-read error, want it omitted", it["issue_ref"])
		}
	}
}

func TestCampaignStatus_Awaiting_RunReadErrorOmitsBlock(t *testing.T) {
	f := newAwaitingFixture(t)
	f.rr.getRunErr = errors.New("runs down")
	for _, it := range statusItemsRaw(t, callCampaignStatusAs(f.s, f.c.ID, gateViewReadIdentity())) {
		if _, ok := it["awaiting"]; ok {
			t.Fatalf("item %s carries awaiting on a run-read error, want it omitted", it["issue_ref"])
		}
	}
}

func TestCampaignStatus_Awaiting_MarkerReadErrorUnavailable(t *testing.T) {
	f := newAwaitingFixture(t)
	f.au.err = errors.New("audit down")
	got, _ := statusAwaitingByRef(t, callCampaignStatusAs(f.s, f.c.ID, gateViewReadIdentity()))
	gate := got["issue:3"]
	if gate == nil || !gate.DecisionUnavailable || gate.Decision != nil || gate.Reason != awaitingReasonGateApproval {
		t.Fatalf("gate awaiting = %+v, want decision_unavailable with no decision and the classification kept", gate)
	}
}

// With no audit repository wired, the decision source is unavailable, not
// absent.
func TestCampaignStatus_Awaiting_AuditUnwiredUnavailable(t *testing.T) {
	f := newAwaitingFixture(t)
	f.s.cfg.AuditRepo = nil
	got, _ := statusAwaitingByRef(t, callCampaignStatusAs(f.s, f.c.ID, gateViewReadIdentity()))
	if gate := got["issue:3"]; gate == nil || !gate.DecisionUnavailable || gate.Decision != nil {
		t.Fatalf("gate awaiting = %+v, want decision_unavailable", gate)
	}
}

// The terminal-run guard, isolated from the status handler's reconcile-on-read
// pass (which settles a running item whose run reached terminal before the
// block is built, masking the guard there).
func TestCampaignItemAwaitingForStatus_TerminalRunOmitted(t *testing.T) {
	f := newAwaitingFixture(t)
	done := seedPARun(f.rr.pendingAmendRunRepo, "acme/widgets", nil, nil)
	done.State = run.StateFailed
	f.rr.seedStage(done.ID, 1, run.StageStateRunning)
	it := &campaign.Item{IssueRef: "issue:9", State: campaign.ItemStateRunning, RunID: idp(done.ID)}
	if blk := f.s.campaignItemAwaitingForStatus(context.Background(), f.c, it, true, nil); blk != nil {
		t.Fatalf("awaiting = %+v for a terminal run, want nil", blk)
	}
	live := &campaign.Item{IssueRef: "issue:1", State: campaign.ItemStateRunning, RunID: idp(f.host.ID)}
	if blk := f.s.campaignItemAwaitingForStatus(context.Background(), f.c, live, true, nil); blk == nil {
		t.Fatal("awaiting = nil for a running run, want a block (the fixture must reach the guard)")
	}
}

// An unreadable or gate-less marker payload yields no current marker. Tested
// on currentAwaitingMarker directly: through the overlay, a zero-gate marker
// would also be dropped by the foreign-stage check, masking this guard.
func TestCurrentAwaitingMarker_UnreadablePayloadIsNone(t *testing.T) {
	req := campaigndriver.CategoryCampaignGateDecisionRequired
	for name, payload := range map[string]string{
		"truncated json": `{"gate":`,
		"no gate":        `{"decision_state":"human_quorum_required","action":"approve"}`,
	} {
		if m := currentAwaitingMarker([]*audit.Entry{{Sequence: 3, Category: req, Payload: json.RawMessage(payload)}}, nil); m != nil {
			t.Errorf("%s: marker = %+v, want none", name, m)
		}
	}
	good := awaitMarkerEntry(3, req, campaigndriver.GateRunLevel)
	if m := currentAwaitingMarker([]*audit.Entry{good}, nil); m == nil || m.Gate != campaigndriver.GateRunLevel || m.Sequence != 3 {
		t.Fatalf("well-formed marker = %+v, want gate run at sequence 3", m)
	}
}
