package server

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/dispatcherheartbeat"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
)

var (
	hdhNow     = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	hdhRunID   = "11111111-2222-4333-8444-555555555555"
	hdhStageID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	hdhOther   = "99999999-8888-4777-8666-555555555555"
)

// hdhRec builds a fresh-by-default record received age before hdhNow.
func hdhRec(host string, age time.Duration, mut func(*hostdispatcher.Heartbeat)) dispatcherheartbeat.Record {
	hb := hostdispatcher.Heartbeat{
		IntervalSeconds: 30, StartedAt: hdhNow.Add(-time.Hour), LastTickAt: hdhNow.Add(-age),
		RunRefusals: []hostdispatcher.RunRefusal{},
	}
	if mut != nil {
		mut(&hb)
	}
	return dispatcherheartbeat.Record{HostLabel: host, Heartbeat: hb, ReceivedAt: hdhNow.Add(-age)}
}

// hdhEntry appends a run_refusals entry for hdhRunID.
func hdhEntry(code string, disp hostdispatcher.Disposition, stageID, budgetSource, detail string) func(*hostdispatcher.Heartbeat) {
	return func(hb *hostdispatcher.Heartbeat) {
		hb.RunRefusals = append(hb.RunRefusals, hostdispatcher.RunRefusal{
			RunID: hdhRunID, StageID: stageID, Code: code, Disposition: disp, BudgetSource: budgetSource,
			Detail: detail, FirstAt: hdhNow.Add(-10 * time.Minute), LastAt: hdhNow.Add(-time.Minute),
		})
	}
}

func hdhText(reason, state, budgetSource string) string {
	return hostDispatcherNextSteps[hostDispatcherTextKey{Reason: reason, State: state, BudgetSource: budgetSource}]
}

func TestClassifyHostDispatcherHeartbeat(t *testing.T) {
	paused := func(hb *hostdispatcher.Heartbeat) { hb.Paused = true }
	failing := func(hb *hostdispatcher.Heartbeat) { hb.TickFailureCode = "ledger_read_failed" }
	both := func(fs ...func(*hostdispatcher.Heartbeat)) func(*hostdispatcher.Heartbeat) {
		return func(hb *hostdispatcher.Heartbeat) {
			for _, f := range fs {
				f(hb)
			}
		}
	}
	capped := func(day string) func(*hostdispatcher.Heartbeat) {
		return func(hb *hostdispatcher.Heartbeat) { hb.DailyCap, hb.SpawnsToday, hb.SpawnsDay = 3, 3, day }
	}
	midnight := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		recs     []dispatcherheartbeat.Record
		kind     string
		reason   string
		state    string
		host     string
		nextStep string
		check    func(t *testing.T, v hostDispatcherVerdict)
	}{
		{name: "absent", kind: awaitingKindHuman, reason: awaitingReasonHostDispatcherAbsent, state: hostDispatcherStateAbsent,
			nextStep: hdhText(awaitingReasonHostDispatcherAbsent, hostDispatcherStateAbsent, "")},
		{name: "stale after three intervals", recs: []dispatcherheartbeat.Record{hdhRec("a", 4*30*time.Second, nil)},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatcherAbsent, state: hostDispatcherStateStale, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatcherAbsent, hostDispatcherStateStale, ""),
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if !v.Observation.LastHeartbeatAt.Equal(hdhNow.Add(-2 * time.Minute)) {
					t.Errorf("last_heartbeat_at = %v", v.Observation.LastHeartbeatAt)
				}
			}},
		{name: "huge interval is clamped", recs: []dispatcherheartbeat.Record{hdhRec("a", 16*time.Minute, func(hb *hostdispatcher.Heartbeat) { hb.IntervalSeconds = 1e9 })},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatcherAbsent, state: hostDispatcherStateStale, host: "a"},
		{name: "future received_at reads stale", recs: []dispatcherheartbeat.Record{hdhRec("a", -2*time.Minute, nil)},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatcherAbsent, state: hostDispatcherStateStale, host: "a"},
		{name: "fresh with no entry is live", recs: []dispatcherheartbeat.Record{hdhRec("a", 30*time.Second, nil)},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateLive, host: "a",
			nextStep: awaitingNextSteps[awaitingReasonHostDispatch]},
		{name: "paused", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, paused)},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchPaused, state: hostDispatcherStatePaused, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatchPaused, hostDispatcherStatePaused, "")},
		{name: "tick failure", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, failing)},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchRefused, state: hostDispatcherStateRefusing, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatchRefused, hostDispatcherStateRefusing, ""),
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if v.Observation.TickFailureCode != "ledger_read_failed" {
					t.Errorf("tick_failure_code = %q", v.Observation.TickFailureCode)
				}
			}},
		{name: "paused wins over a waiting entry", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, both(paused, hdhEntry("concurrency_slot_queued", hostdispatcher.DispositionWaiting, hdhStageID, "", "")))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchPaused, state: hostDispatcherStatePaused, host: "a"},
		{name: "capped today", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, capped("2026-11-24"))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchCapped, state: hostDispatcherStateCapped, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatchCapped, hostDispatcherStateCapped, ""),
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if !v.Observation.CapResetsAt.Equal(midnight) || v.Observation.DailyCap != 3 || v.Observation.SpawnsToday != 3 {
					t.Errorf("observation = %+v, want cap_resets_at %v", v.Observation, midnight)
				}
			}},
		{name: "yesterday's cap count reads live", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, capped("2026-11-23"))},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateLive, host: "a"},
		{name: "waiting entry", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("concurrency_slot_queued", hostdispatcher.DispositionWaiting, hdhStageID, "", ""))},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateWaiting, host: "a"},
		// #5489 condition H2: the dispatcher maps the review-round set to
		// waiting, so such an entry stays host_dispatcher (the MCP overlay
		// refines it), never system/in_flight.
		{name: "review-round entry stays host_dispatcher", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("implement_review_in_flight", hostdispatcher.DispositionWaiting, hdhStageID, "", ""))},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateWaiting, host: "a"},
		{name: "in_flight entry", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("already_dispatched", hostdispatcher.DispositionInFlight, hdhStageID, "", ""))},
			kind: awaitingKindSystem, reason: awaitingReasonInFlight, state: hostDispatcherStateInFlight, host: "a"},
		{name: "held entry", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("campaign_dispatch_held", hostdispatcher.DispositionHeld, hdhStageID, "", ""))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchRefused, state: hostDispatcherStateHeld, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatchRefused, hostDispatcherStateHeld, ""),
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if v.Observation.RefusalCode != "campaign_dispatch_held" || !v.Observation.RefusalFirstAt.Equal(hdhNow.Add(-10*time.Minute)) {
					t.Errorf("observation = %+v", v.Observation)
				}
			}},
		{name: "refusing entry", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("path_outside_allowed_roots", hostdispatcher.DispositionRefusing, hdhStageID, "", "working_dir /x is outside"))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchRefused, state: hostDispatcherStateRefusing, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatchRefused, hostDispatcherStateRefusing, ""),
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if v.Observation.RefusalCode != "path_outside_allowed_roots" || v.Observation.RefusalDetail != "working_dir /x is outside" {
					t.Errorf("observation = %+v", v.Observation)
				}
				if strings.Contains(v.NextStep, "outside") {
					t.Errorf("next_step %q carries dispatcher-authored text", v.NextStep)
				}
			}},
		{name: "paused entry", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("paused", hostdispatcher.DispositionPaused, hdhStageID, "", ""))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchPaused, state: hostDispatcherStatePaused, host: "a"},
		{name: "capped entry", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("cap_reached:daily", hostdispatcher.DispositionCapped, hdhStageID, "", ""))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchCapped, state: hostDispatcherStateCapped, host: "a",
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if !v.Observation.CapResetsAt.Equal(midnight) {
					t.Errorf("cap_resets_at = %v", v.Observation.CapResetsAt)
				}
			}},
		{name: "budget_blocked live_spec", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("budget_blocked", hostdispatcher.DispositionBudgetBlocked, hdhStageID, hostdispatcher.BudgetSourceLiveSpec, ""))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchBudgetBlocked, state: hostDispatcherStateHeld, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatchBudgetBlocked, hostDispatcherStateHeld, hostdispatcher.BudgetSourceLiveSpec),
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if v.Observation.BudgetSource != hostdispatcher.BudgetSourceLiveSpec || !strings.Contains(v.NextStep, "limit_usd") {
					t.Errorf("verdict = %+v", v)
				}
			}},
		{name: "budget_blocked admission_spec", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("budget_blocked", hostdispatcher.DispositionBudgetBlocked, hdhStageID, hostdispatcher.BudgetSourceAdmissionSpec, ""))},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchBudgetBlocked, state: hostDispatcherStateHeld, host: "a",
			nextStep: hdhText(awaitingReasonHostDispatchBudgetBlocked, hostDispatcherStateHeld, ""),
			check: func(t *testing.T, v hostDispatcherVerdict) {
				if v.Observation.BudgetSource != hostdispatcher.BudgetSourceAdmissionSpec {
					t.Errorf("budget_source = %q", v.Observation.BudgetSource)
				}
				for _, bad := range []string{"limit_usd", "default branch", "default-branch", "advisory"} {
					if strings.Contains(v.NextStep, bad) {
						t.Errorf("admission next_step %q names %q", v.NextStep, bad)
					}
				}
				if !strings.Contains(v.NextStep, "budget_override") {
					t.Errorf("admission next_step %q does not name budget_override", v.NextStep)
				}
			}},
		{name: "stage-mismatched entry is discarded", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, hdhEntry("path_outside_allowed_roots", hostdispatcher.DispositionRefusing, hdhOther, "", ""))},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateLive, host: "a"},
		{name: "another run's entry is ignored", recs: []dispatcherheartbeat.Record{hdhRec("a", 0, func(hb *hostdispatcher.Heartbeat) {
			hb.RunRefusals = append(hb.RunRefusals, hostdispatcher.RunRefusal{RunID: hdhOther, Code: "x", Disposition: hostdispatcher.DispositionRefusing})
		})}, kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateLive, host: "a"},
		// Multi-dispatcher rules (#5489 step 5d).
		{name: "m1 stale paused plus fresh live reads live", recs: []dispatcherheartbeat.Record{hdhRec("a", 10*time.Minute, paused), hdhRec("b", 0, nil)},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateLive, host: "b"},
		{name: "m2 one host refusing plus another live reads live", recs: []dispatcherheartbeat.Record{
			hdhRec("a", 0, hdhEntry("path_outside_allowed_roots", hostdispatcher.DispositionRefusing, hdhStageID, "", "")), hdhRec("b", 5*time.Second, nil)},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateLive, host: "b"},
		{name: "m3 waiting plus refusing reads waiting", recs: []dispatcherheartbeat.Record{
			hdhRec("a", 5*time.Second, hdhEntry("concurrency_slot_queued", hostdispatcher.DispositionWaiting, hdhStageID, "", "")),
			hdhRec("b", 0, hdhEntry("path_outside_allowed_roots", hostdispatcher.DispositionRefusing, hdhStageID, "", ""))},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateWaiting, host: "a"},
		{name: "m4 held plus live reads held", recs: []dispatcherheartbeat.Record{
			hdhRec("a", 5*time.Second, hdhEntry("run_dispatch_held", hostdispatcher.DispositionHeld, hdhStageID, "", "")), hdhRec("b", 0, nil)},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchRefused, state: hostDispatcherStateHeld, host: "a"},
		{name: "m5 held plus in_flight reads in_flight", recs: []dispatcherheartbeat.Record{
			hdhRec("a", 0, hdhEntry("run_dispatch_held", hostdispatcher.DispositionHeld, hdhStageID, "", "")),
			hdhRec("b", 5*time.Second, hdhEntry("already_dispatched", hostdispatcher.DispositionInFlight, hdhStageID, "", ""))},
			kind: awaitingKindSystem, reason: awaitingReasonInFlight, state: hostDispatcherStateInFlight, host: "b"},
		{name: "m6 paused record with a mismatched entry plus live reads live", recs: []dispatcherheartbeat.Record{
			hdhRec("a", 0, both(paused, hdhEntry("concurrency_slot_queued", hostdispatcher.DispositionWaiting, hdhOther, "", ""))), hdhRec("b", 5*time.Second, nil)},
			kind: awaitingKindHostDispatcher, reason: awaitingReasonHostDispatch, state: hostDispatcherStateLive, host: "b"},
		{name: "m7 host blocks: newer received_at decides", recs: []dispatcherheartbeat.Record{hdhRec("a", 10*time.Second, paused), hdhRec("b", 5*time.Second, failing)},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchRefused, state: hostDispatcherStateRefusing, host: "b"},
		{name: "m7 host blocks: equal received_at goes to the lower host", recs: []dispatcherheartbeat.Record{hdhRec("b", 5*time.Second, failing), hdhRec("a", 5*time.Second, paused)},
			kind: awaitingKindHuman, reason: awaitingReasonHostDispatchPaused, state: hostDispatcherStatePaused, host: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := classifyHostDispatcherHeartbeat(tc.recs, hdhRunID, hdhStageID, hdhNow)
			if v.Kind != tc.kind || v.Reason != tc.reason || v.Observation.State != tc.state || v.Observation.Host != tc.host {
				t.Fatalf("verdict = %s/%s state %q host %q, want %s/%s state %q host %q",
					v.Kind, v.Reason, v.Observation.State, v.Observation.Host, tc.kind, tc.reason, tc.state, tc.host)
			}
			if v.NextStep == "" {
				t.Fatal("next_step is empty")
			}
			if tc.nextStep != "" && v.NextStep != tc.nextStep {
				t.Fatalf("next_step = %q, want %q", v.NextStep, tc.nextStep)
			}
			if tc.check != nil {
				tc.check(t, v)
			}
		})
	}
}

// TestAwaitingReasons_DocumentedAndGuided is the done-means pin: every
// awaiting reason has server-constant guidance and is in the OpenAPI reason
// enum, every heartbeat text is non-empty, and the host_dispatcher schema's
// properties equal hostDispatcherObservation's json tags.
func TestAwaitingReasons_DocumentedAndGuided(t *testing.T) {
	reasons := []string{
		awaitingReasonHostDispatch, awaitingReasonOperatorDispatch, awaitingReasonStageRetry,
		awaitingReasonGateApproval, awaitingReasonDeployApproval, awaitingReasonInput,
		awaitingReasonScopeDecision, awaitingReasonScopeAmendmentPending, awaitingReasonRunDecision,
		awaitingReasonInFlight, awaitingReasonChildren, awaitingReasonNoActiveStage, awaitingReasonDispatchHeld,
		awaitingReasonHostDispatchPaused, awaitingReasonHostDispatchCapped, awaitingReasonHostDispatchRefused,
		awaitingReasonHostDispatchBudgetBlocked, awaitingReasonHostDispatcherAbsent,
	}
	if len(awaitingNextSteps) != len(reasons) {
		t.Errorf("awaitingNextSteps holds %d reasons, the list here %d: add the new reason to this test and the OpenAPI enum", len(awaitingNextSteps), len(reasons))
	}
	for _, r := range reasons {
		if awaitingNextSteps[r] == "" {
			t.Errorf("reason %q has no awaitingNextSteps text", r)
		}
	}
	for k, text := range hostDispatcherNextSteps {
		if text == "" {
			t.Errorf("hostDispatcherNextSteps%+v is empty", k)
		}
	}
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}
	enum := append([]string(nil), doc.Components.Schemas["CampaignItemAwaiting"].Properties["reason"].Enum...)
	want := append([]string(nil), reasons...)
	sort.Strings(enum)
	sort.Strings(want)
	if !reflect.DeepEqual(enum, want) {
		t.Errorf("CampaignItemAwaiting.reason enum = %v, want %v (docs/api/v0.openapi.yaml)", enum, want)
	}
	if _, ok := doc.Components.Schemas["CampaignItemAwaiting"].Properties["host_dispatcher"]; !ok {
		t.Error("CampaignItemAwaiting.host_dispatcher is undocumented")
	}
	var props []string
	for k := range doc.Components.Schemas["CampaignItemAwaitingHostDispatcher"].Properties {
		props = append(props, k)
	}
	sort.Strings(props)
	tags := hostDispatcherObservationKeys()
	if !reflect.DeepEqual(props, tags) {
		t.Errorf("CampaignItemAwaitingHostDispatcher properties = %v, want hostDispatcherObservation's json tags %v", props, tags)
	}
	states := doc.Components.Schemas["CampaignItemAwaitingHostDispatcher"].Properties["state"].Enum
	sort.Strings(states)
	wantStates := []string{hostDispatcherStateAbsent, hostDispatcherStateCapped, hostDispatcherStateHeld, hostDispatcherStateInFlight,
		hostDispatcherStateLive, hostDispatcherStatePaused, hostDispatcherStateRefusing, hostDispatcherStateStale, hostDispatcherStateWaiting}
	sort.Strings(wantStates)
	if !reflect.DeepEqual(states, wantStates) {
		t.Errorf("CampaignItemAwaitingHostDispatcher.state enum = %v, want %v", states, wantStates)
	}
}

// hostDispatcherObservationKeys returns hostDispatcherObservation's json
// keys, sorted.
func hostDispatcherObservationKeys() []string {
	var keys []string
	rt := reflect.TypeOf(hostDispatcherObservation{})
	for i := range rt.NumField() {
		keys = append(keys, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(keys)
	return keys
}
