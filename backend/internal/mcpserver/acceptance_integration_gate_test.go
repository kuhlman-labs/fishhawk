package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// --- pure fold ---

// acceptancePendingActions is the acceptance_pending arm's shape for a local
// run: the acceptance dispatch plus an unrelated action the hold must keep.
func acceptancePendingActions(runID string) *NextActions {
	return &NextActions{
		State: "acceptance_pending",
		Actions: []SuggestedAction{
			{Action: "fishhawk_dispatch_stage", Params: map[string]string{"run_id": runID, "stage": "acceptance"}},
			{Action: "fishhawk_run_stage", Params: map[string]string{"run_id": runID, "stage": "acceptance"}},
			{Action: "fishhawk_get_run_status", Params: map[string]string{"run_id": runID}},
		},
	}
}

// partialChildrenStatus is a decomposed parent with two succeeded children and
// a newest slices_integrated covering only the first.
func partialChildrenStatus() *ChildrenStatus {
	return &ChildrenStatus{
		IntegrationPhase: integrationPhaseReadyToIntegrate,
		Children: []ChildStatus{
			{RunID: "child-a", SliceIndex: 0, State: "succeeded"},
			{RunID: "child-b", SliceIndex: 1, State: "succeeded"},
		},
		Total:                   2,
		Succeeded:               2,
		IntegratedChildRunIDs:   []string{"child-a"},
		UnintegratedChildRunIDs: []string{"child-b"},
		fanInRecorded:           true,
	}
}

func TestOffersAcceptanceDispatch(t *testing.T) {
	cases := []struct {
		name string
		na   *NextActions
		want bool
	}{
		{"nil", nil, false},
		{"dispatch_stage acceptance", &NextActions{Actions: []SuggestedAction{{Action: "fishhawk_dispatch_stage", Params: map[string]string{"stage": "acceptance"}}}}, true},
		{"run_stage acceptance", &NextActions{Actions: []SuggestedAction{{Action: "fishhawk_run_stage", Params: map[string]string{"stage": "acceptance"}}}}, true},
		{"dispatch_stage implement", &NextActions{Actions: []SuggestedAction{{Action: "fishhawk_dispatch_stage", Params: map[string]string{"stage": "implement"}}}}, false},
		{"other verb naming acceptance", &NextActions{Actions: []SuggestedAction{{Action: "fishhawk_await_stage", Params: map[string]string{"stage": "acceptance"}}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := offersAcceptanceDispatch(c.na); got != c.want {
				t.Errorf("offersAcceptanceDispatch = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFoldAcceptanceIntegrationHold_PartialStripsAndPrependsAwait: every
// acceptance dispatch is removed, the state is set, fishhawk_await_children is
// FIRST, its reason names the uncovered child and the server's 409, and the
// unrelated action survives.
func TestFoldAcceptanceIntegrationHold_PartialStripsAndPrependsAwait(t *testing.T) {
	na := acceptancePendingActions("parent-1")
	foldAcceptanceIntegrationHold("parent-1", partialChildrenStatus(), "succeeded", nil, na)

	if na.State != acceptanceHeldIntegrationIncompleteState {
		t.Errorf("state = %q, want %s", na.State, acceptanceHeldIntegrationIncompleteState)
	}
	if offersAcceptanceDispatch(na) {
		t.Errorf("actions still offer an acceptance dispatch: %+v", na.Actions)
	}
	if len(na.Actions) != 2 {
		t.Fatalf("actions = %+v, want await + the unrelated get_run_status", na.Actions)
	}
	first := na.Actions[0]
	if first.Action != "fishhawk_await_children" || first.Params["run_id"] != "parent-1" {
		t.Errorf("first action = %+v, want fishhawk_await_children on the parent", first)
	}
	for _, want := range []string{"child-b", "409 acceptance_integration_incomplete"} {
		if !strings.Contains(first.Reason, want) {
			t.Errorf("reason %q missing %q", first.Reason, want)
		}
	}
	if na.Actions[1].Action != "fishhawk_get_run_status" {
		t.Errorf("the unrelated action was dropped: %+v", na.Actions)
	}
}

// TestFoldAcceptanceIntegrationHold_FailureReason: a newer integration failure
// is what the reason names.
func TestFoldAcceptanceIntegrationHold_FailureReason(t *testing.T) {
	cs := partialChildrenStatus()
	cs.IntegrationPhase = integrationPhaseFailed
	cs.IntegrationFailure = &integrationFailure{Cause: auditCategorySliceHeadMissing, ChildRunID: "child-b", Sequence: 9}
	na := acceptancePendingActions("p")
	foldAcceptanceIntegrationHold("p", cs, "succeeded", nil, na)
	if na.State != acceptanceHeldIntegrationIncompleteState {
		t.Fatalf("state = %q, want held", na.State)
	}
	if r := na.Actions[0].Reason; !strings.Contains(r, "slice_head_missing") || !strings.Contains(r, "child-b") {
		t.Errorf("reason %q must name the failure cause and child", r)
	}
	// Running children with no uncovered succeeded child: the generic reason.
	cs2 := &ChildrenStatus{IntegrationPhase: integrationPhaseRunningChildren, Children: []ChildStatus{{RunID: "x", State: "running"}}, fanInRecorded: true}
	na2 := acceptancePendingActions("p")
	foldAcceptanceIntegrationHold("p", cs2, "succeeded", nil, na2)
	if r := na2.Actions[0].Reason; !strings.Contains(r, "not every child has succeeded") {
		t.Errorf("generic reason = %q", r)
	}
}

// TestFoldAcceptanceIntegrationHold_NoOpArms pins every arm that must leave
// next_actions byte-identical.
func TestFoldAcceptanceIntegrationHold_NoOpArms(t *testing.T) {
	integrated := partialChildrenStatus()
	integrated.IntegrationPhase = integrationPhaseIntegrated
	integrated.UnintegratedChildRunIDs = nil
	// fanInRecorded: true isolates the zero-children guard — without it the C3
	// arm (no record + succeeded parent) would no-op the fold anyway and mask a
	// deleted zero-children check.
	noChildren := &ChildrenStatus{IntegrationPhase: integrationPhaseRunningChildren, fanInRecorded: true}
	// C3: no fan-in record of any kind + the parent's implement stage already
	// succeeded = no integration authority; the server stands down, so must the
	// display.
	noAuthority := partialChildrenStatus()
	noAuthority.fanInRecorded = false

	cases := []struct {
		name        string
		cs          *ChildrenStatus
		parentState string
		authority   *runSliceIntegration
		na          func() *NextActions
	}{
		{"integrated", integrated, "succeeded", nil, func() *NextActions { return acceptancePendingActions("p") }},
		{"nil children status", nil, "succeeded", nil, func() *NextActions { return acceptancePendingActions("p") }},
		{"zero children", noChildren, "succeeded", nil, func() *NextActions { return acceptancePendingActions("p") }},
		{"no acceptance dispatch offered", partialChildrenStatus(), "succeeded", nil, func() *NextActions {
			return &NextActions{State: "review_pending", Actions: []SuggestedAction{{Action: "fishhawk_await_review"}}}
		}},
		{"C3 no integration authority (inference fallback)", noAuthority, "succeeded", nil, func() *NextActions { return acceptancePendingActions("p") }},
		// #4165: the server's own predicate says no authority. A PARTIAL record
		// and a parent still awaiting_children would both keep the hold under
		// the inference, so only the authority branch makes this a no-op — the
		// server gate stands down here too.
		{"C3 authority unavailable with a partial record", partialChildrenStatus(), "awaiting_children",
			&runSliceIntegration{Available: false, Reason: "GitHub not configured"},
			func() *NextActions { return acceptancePendingActions("p") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			na := c.na()
			before, _ := json.Marshal(na)
			foldAcceptanceIntegrationHold("p", c.cs, c.parentState, c.authority, na)
			after, _ := json.Marshal(na)
			if string(before) != string(after) {
				t.Errorf("next_actions changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	// nil next_actions must not panic.
	foldAcceptanceIntegrationHold("p", partialChildrenStatus(), "succeeded", nil, nil)
}

// TestFoldAcceptanceIntegrationHold_FanInRecordLost pins the #4165 wedge: the
// server HAS slice-integration authority, the parent's implement stage
// succeeded, and NO fan-in record exists (the best-effort slices_integrated
// append was lost). The server refuses acceptance 409
// acceptance_integration_incomplete, so the display must HOLD and name the
// integrate-wave recovery. MECHANISM: this snapshot is exactly the C3
// inference's positive case, so without the authority signal the fold would
// no-op; and without the fanInRecordLost reason branch the generic "lacks the
// slices of" reason would appear instead of integrate-wave.
func TestFoldAcceptanceIntegrationHold_FanInRecordLost(t *testing.T) {
	cs := partialChildrenStatus()
	cs.fanInRecorded = false
	cs.IntegratedChildRunIDs = nil
	cs.UnintegratedChildRunIDs = []string{"child-a", "child-b"}
	na := acceptancePendingActions("parent-1")
	foldAcceptanceIntegrationHold("parent-1", cs, "succeeded", &runSliceIntegration{Available: true}, na)

	if na.State != acceptanceHeldIntegrationIncompleteState {
		t.Fatalf("state = %q, want %s", na.State, acceptanceHeldIntegrationIncompleteState)
	}
	if offersAcceptanceDispatch(na) {
		t.Errorf("actions still offer an acceptance dispatch: %+v", na.Actions)
	}
	first := na.Actions[0]
	if first.Action != "fishhawk_await_children" || first.Params["run_id"] != "parent-1" {
		t.Errorf("first action = %+v, want fishhawk_await_children on the parent", first)
	}
	for _, want := range []string{"POST /v0/runs/parent-1/integrate-wave", "acceptance_integration_incomplete", "not_awaiting_children", "slices_integrated append was lost"} {
		if !strings.Contains(first.Reason, want) {
			t.Errorf("reason %q missing %q", first.Reason, want)
		}
	}
}

// TestFanInRecordLost pins the wedge predicate's every conjunct: it needs a
// POSITIVE authority, no record, no failure, and a SUCCEEDED parent.
func TestFanInRecordLost(t *testing.T) {
	avail := &runSliceIntegration{Available: true}
	unavail := &runSliceIntegration{Available: false, Reason: "run has no installation_id"}
	none := &ChildrenStatus{}
	cases := []struct {
		name      string
		cs        *ChildrenStatus
		state     string
		authority *runSliceIntegration
		want      bool
	}{
		{"authority available + no record + succeeded", none, "succeeded", avail, true},
		{"nil authority (older backend)", none, "succeeded", nil, false},
		{"authority unavailable", none, "succeeded", unavail, false},
		{"record present", &ChildrenStatus{fanInRecorded: true}, "succeeded", avail, false},
		{"integration failure recorded", &ChildrenStatus{IntegrationFailure: &integrationFailure{Cause: auditCategorySliceHeadMissing}}, "succeeded", avail, false},
		{"parent still awaiting_children", none, "awaiting_children", avail, false},
		{"parent stage unreadable", none, "", avail, false},
		{"nil status", nil, "succeeded", avail, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fanInRecordLost(c.cs, c.state, c.authority); got != c.want {
				t.Errorf("fanInRecordLost = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNewestStageByType pins the newest-by-Sequence selection over BOTH slice
// orderings: older-first catches a first-match regression (it would return the
// failed seq-2 stage), newer-first catches a last-match one.
func TestNewestStageByType(t *testing.T) {
	plan := Stage{ID: "plan", Sequence: 1, Type: "plan", State: "succeeded"}
	old := Stage{ID: "impl-old", Sequence: 2, Type: "implement", State: "failed"}
	newer := Stage{ID: "impl-new", Sequence: 4, Type: "implement", State: "succeeded"}
	for name, stages := range map[string][]Stage{
		"older first": {plan, old, newer},
		"newer first": {newer, plan, old},
	} {
		t.Run(name, func(t *testing.T) {
			got := newestStageByType(stages, "implement")
			if got == nil || got.ID != "impl-new" {
				t.Fatalf("newestStageByType = %+v, want the seq-4 implement stage", got)
			}
		})
	}
	if got := newestStageByType([]Stage{plan}, "implement"); got != nil {
		t.Errorf("no implement stage: got %+v, want nil", got)
	}
	if got := newestStageByType(nil, "implement"); got != nil {
		t.Errorf("nil stages: got %+v, want nil", got)
	}
}

// TestSliceIntegrationOf pins the nil-safe accessor: a nil run, a run without
// a capabilities block (a list read), and a block without the key are all
// UNDECIDABLE (nil); a present key is returned as-is.
func TestSliceIntegrationOf(t *testing.T) {
	si := &runSliceIntegration{Available: true}
	cases := []struct {
		name string
		run  *Run
		want *runSliceIntegration
	}{
		{"nil run", nil, nil},
		{"no capabilities block", &Run{}, nil},
		{"block without the key", &Run{Capabilities: &runCapabilities{}}, nil},
		{"key present", &Run{Capabilities: &runCapabilities{SliceIntegration: si}}, si},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sliceIntegrationOf(c.run); got != c.want {
				t.Errorf("sliceIntegrationOf = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestIntegrationAuthorityAbsent pins C3 in both modes. With the server's
// capabilities.slice_integration present (#4165) the answer is
// !authority.Available, whatever the record or stage say — the server gate
// consults neither. With it absent (an older backend) it falls back to the
// inference: only NO fan-in record AND a SUCCEEDED parent implement stage read
// as authority-less; a record of any kind, a parent still awaiting_children,
// or an UNREADABLE parent stage keeps the hold.
func TestIntegrationAuthorityAbsent(t *testing.T) {
	none := &ChildrenStatus{}
	recorded := &ChildrenStatus{fanInRecorded: true}
	avail := &runSliceIntegration{Available: true}
	unavail := &runSliceIntegration{Available: false, Reason: "GitHub not configured"}
	cases := []struct {
		name      string
		cs        *ChildrenStatus
		state     string
		authority *runSliceIntegration
		want      bool
	}{
		{"no record + succeeded", none, "succeeded", nil, true},
		{"no record + awaiting_children", none, "awaiting_children", nil, false},
		{"no record + unreadable", none, "", nil, false},
		{"record + succeeded", recorded, "succeeded", nil, false},
		{"nil status", nil, "succeeded", nil, false},
		// #4165: the server's predicate wins over the inference. The first row
		// IS the inference's positive case, the second its negative case, so
		// only the authority branch flips them.
		{"authority available + no record + succeeded", none, "succeeded", avail, false},
		{"authority unavailable + record + awaiting_children", recorded, "awaiting_children", unavail, true},
		{"authority unavailable + nil status", nil, "succeeded", unavail, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := integrationAuthorityAbsent(c.cs, c.state, c.authority); got != c.want {
				t.Errorf("integrationAuthorityAbsent = %v, want %v", got, c.want)
			}
		})
	}
}

// --- resolver gate: cost + fail-open ---

// fanInReadCount sums the fake backend's category-filtered reads of the
// plan_decomposed probe and the four fan-in kinds.
func fanInReadCount(fb *fakeBackend) map[string]int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := map[string]int{}
	for _, c := range append([]string{"plan_decomposed"}, fanInCategories...) {
		out[c] = fb.perRunAuditCategoryReads[c]
	}
	return out
}

// TestGateAcceptanceOnIntegration_ZeroReadsWhenNotApplicable: no acceptance
// dispatch offered, or a decomposed CHILD run, makes zero reads.
func TestGateAcceptanceOnIntegration_ZeroReadsWhenNotApplicable(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	seedPlanDecomposed(fb, parent, []string{uuid.NewString()}, 0)
	run := &Run{ID: parent.String()}

	na := &NextActions{State: "review_pending", Actions: []SuggestedAction{{Action: "fishhawk_await_review"}}}
	r.gateAcceptanceOnIntegration(context.Background(), parent, run, nil, na)

	child := "p"
	r.gateAcceptanceOnIntegration(context.Background(), parent, &Run{ID: parent.String(), DecomposedFrom: &child}, nil, acceptancePendingActions(parent.String()))
	r.gateAcceptanceOnIntegration(context.Background(), parent, &Run{ID: parent.String(), ParentRunID: &child}, nil, acceptancePendingActions(parent.String()))
	r.gateAcceptanceOnIntegration(context.Background(), parent, nil, nil, acceptancePendingActions(parent.String()))

	for cat, n := range fanInReadCount(fb) {
		if n != 0 {
			t.Errorf("%s reads = %d, want 0 (the gate must not read when no top-level acceptance dispatch is offered)", cat, n)
		}
	}
}

// TestGateAcceptanceOnIntegration_FailsOpen: a plan_decomposed decode error,
// and a fan-in walk error, both leave next_actions unchanged — the server's
// 409 is the authority, the display never wedges on a read failure.
func TestGateAcceptanceOnIntegration_FailsOpen(t *testing.T) {
	t.Run("plan_decomposed decode error", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		parent := uuid.New()
		fb.mu.Lock()
		fb.perRunAuditByRun[parent] = []AuditEntry{{ID: uuid.NewString(), Sequence: 1, RunID: parent.String(), Category: "plan_decomposed"}}
		fb.mu.Unlock()
		na := acceptancePendingActions(parent.String())
		r.gateAcceptanceOnIntegration(context.Background(), parent, &Run{ID: parent.String()}, nil, na)
		if na.State != "acceptance_pending" || !offersAcceptanceDispatch(na) {
			t.Errorf("next_actions = %+v, want unchanged on a read error", na)
		}
	})
	t.Run("fan-in walk error", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		parent, a := uuid.New(), uuid.New()
		seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
		seedPlanDecomposed(fb, parent, []string{a.String()}, 0)
		fb.mu.Lock()
		fb.perRunAuditNextByRun[parent] = "stuck-cursor"
		fb.mu.Unlock()
		na := acceptancePendingActions(parent.String())
		r.gateAcceptanceOnIntegration(context.Background(), parent, &Run{ID: parent.String()}, nil, na)
		if na.State != "acceptance_pending" || !offersAcceptanceDispatch(na) {
			t.Errorf("next_actions = %+v, want unchanged on a fan-in read error", na)
		}
		if fanInReadCount(fb)[auditCategorySlicesIntegrated] == 0 {
			t.Error("the fan-in walk never ran — the fixture did not reach the error arm")
		}
	})
}

// TestGateAcceptanceOnIntegration_NewestImplementStage pins that the gate reads
// the parent's NEWEST implement stage by Sequence (#4165). MECHANISM: the
// older failed implement stage is listed FIRST, so a first-match lookup reads
// "failed", the nil-authority C3 inference refuses to fire, and the dispatch
// is held; the newest stage succeeded, so the fallback keeps it.
func TestGateAcceptanceOnIntegration_NewestImplementStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, a := uuid.New(), uuid.New()
	seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
	seedPlanDecomposed(fb, parent, []string{a.String()}, 0)
	stages := []Stage{
		{ID: "impl-old", Sequence: 2, Type: "implement", State: "failed"},
		{ID: "impl-new", Sequence: 4, Type: "implement", State: "succeeded"},
	}
	na := acceptancePendingActions(parent.String())
	r.gateAcceptanceOnIntegration(context.Background(), parent, &Run{ID: parent.String()}, stages, na)
	if na.State != "acceptance_pending" || !offersAcceptanceDispatch(na) {
		t.Errorf("next_actions = %+v, want acceptance_pending kept (newest implement succeeded, no record, no authority signal)", na)
	}
}

// TestGateAcceptanceOnIntegration_KeysOnRunCapabilities pins that the gate
// passes the run's own capabilities.slice_integration through (#4165): the
// same no-record + succeeded snapshot HOLDS with authority available and is a
// no-op without the key.
func TestGateAcceptanceOnIntegration_KeysOnRunCapabilities(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, a := uuid.New(), uuid.New()
	seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
	seedPlanDecomposed(fb, parent, []string{a.String()}, 0)
	stages := []Stage{{ID: "impl", Sequence: 2, Type: "implement", State: "succeeded"}}

	held := acceptancePendingActions(parent.String())
	run := &Run{ID: parent.String(), Capabilities: &runCapabilities{SliceIntegration: &runSliceIntegration{Available: true}}}
	r.gateAcceptanceOnIntegration(context.Background(), parent, run, stages, held)
	if held.State != acceptanceHeldIntegrationIncompleteState || !strings.Contains(held.Actions[0].Reason, "integrate-wave") {
		t.Errorf("authority present: next_actions = %+v, want held naming integrate-wave", held)
	}

	kept := acceptancePendingActions(parent.String())
	r.gateAcceptanceOnIntegration(context.Background(), parent, &Run{ID: parent.String()}, stages, kept)
	if kept.State != "acceptance_pending" || !offersAcceptanceDispatch(kept) {
		t.Errorf("no authority signal: next_actions = %+v, want the C3 fallback to keep the dispatch", kept)
	}
}

// --- cross-layer: MCP client ↔ real server handlers ↔ Postgres audit chain ---

// authorityOnlyGitHub is a non-nil orchestrator.GitHubAPI so the server's
// SliceIntegrationUnavailable reports integration authority present (the gate
// is live, not stood down per C3). Its methods are never called on the paths
// this test drives: the embedded nil interface would panic if one were.
type authorityOnlyGitHub struct{ orchestrator.GitHubAPI }

// TestAcceptanceIntegrationGate_CrossLayer_PartialThenFull is the done-means
// test end to end (#4080): it spans the real MCP client decode, the real
// server handlers, the Postgres audit chain and wavecoverage, against the
// shared pgtest Postgres (no container of its own).
//
// Partial coverage: fishhawk_await_children releases integration_pending naming
// child B through the REAL paginated audit endpoint, AND the real host-dispatch
// marker answers 409 acceptance_integration_incomplete with the acceptance
// stage unchanged on a repo read. After a covering slices_integrated is
// appended: await releases children_settled AND the marker admits.
func TestAcceptanceIntegrationGate_CrossLayer_PartialThenFull(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	const bearer = "fhk_accept_gate_e2e"
	o := &orchestrator.Orchestrator{Runs: runRepo, Audit: auditRepo, GitHub: authorityOnlyGitHub{}}
	srv := server.New(server.Config{
		RunRepo:      runRepo,
		AuditRepo:    auditRepo,
		Orchestrator: o,
		APITokenRepo: &stubMCPAPITokens{tok: &apitoken.Token{
			ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "write:runs"}, PlainText: bearer,
		}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: bearer}), getenv: func(string) string { return "" }, reviewPollInterval: time.Millisecond}

	inst := int64(42)
	parent, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: runpkg.TriggerCLI, InstallationID: &inst,
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	impl, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: parent.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	for _, to := range []runpkg.StageState{runpkg.StageStateAwaitingChildren, runpkg.StageStateSucceeded} {
		if _, err := runRepo.TransitionStage(ctx, impl.ID, to, nil); err != nil {
			t.Fatalf("transition implement to %s: %v", to, err)
		}
	}
	acc, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: parent.ID, Sequence: 2, Type: runpkg.StageTypeAcceptance,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create acceptance stage: %v", err)
	}
	if _, err := runRepo.TransitionStage(ctx, acc.ID, runpkg.StageStateAwaitingHostDispatch, nil); err != nil {
		t.Fatalf("park acceptance: %v", err)
	}

	newChild := func(idx int) *runpkg.Run {
		t.Helper()
		i := idx
		c, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
			TriggerSource: runpkg.TriggerCLI, DecomposedFrom: &parent.ID, ParentRunID: &parent.ID, SliceIndex: &i,
			InstallationID: &inst,
		})
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
		for _, to := range []runpkg.State{runpkg.StateRunning, runpkg.StateSucceeded} {
			if _, err := runRepo.TransitionRun(ctx, c.ID, to); err != nil {
				t.Fatalf("transition child to %s: %v", to, err)
			}
		}
		return c
	}
	childA, childB := newChild(0), newChild(1)

	appendAudit := func(category string, payload any) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s: %v", category, err)
		}
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: parent.ID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
		}); err != nil {
			t.Fatalf("append %s: %v", category, err)
		}
	}
	appendAudit("plan_decomposed", map[string]any{
		"child_run_ids":          []string{childA.ID.String(), childB.ID.String()},
		"effective_max_parallel": 0,
	})
	// The newest slices_integrated covers ONLY child A. Payload keys come from
	// the decoder struct, which TestSlicesIntegratedPayloadKeysMatchEmitter ties
	// to the real emitter.
	appendAudit(auditCategorySlicesIntegrated, slicesIntegratedPayload{
		ConsolidatedBranch: "fishhawk/run-" + parent.ID.String(), ChildRunIDs: []string{childA.ID.String()},
	})

	stageState := func() string {
		t.Helper()
		st, err := runRepo.GetStage(ctx, acc.ID)
		if err != nil {
			t.Fatalf("GetStage: %v", err)
		}
		return string(st.State)
	}

	// --- partial coverage ---
	_, out, err := r.awaitChildren(ctx, nil, AwaitChildrenInput{RunID: parent.ID.String(), TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("awaitChildren (partial): %v", err)
	}
	if out.Status != "integration_pending" {
		t.Fatalf("await status = %q, want integration_pending through the real audit endpoint", out.Status)
	}
	if len(out.UnintegratedChildRunIDs) != 1 || out.UnintegratedChildRunIDs[0] != childB.ID.String() {
		t.Errorf("unintegrated_child_run_ids = %v, want [%s]", out.UnintegratedChildRunIDs, childB.ID)
	}

	_, hdErr := r.api.HostDispatchStage(ctx, parent.ID, acc.ID)
	var ae *apiError
	if !errors.As(hdErr, &ae) || ae.Code != "acceptance_integration_incomplete" {
		t.Fatalf("HostDispatchStage err = %v, want a 409 acceptance_integration_incomplete apiError", hdErr)
	}
	if ae.StatusCode != 409 {
		t.Errorf("status = %d, want 409", ae.StatusCode)
	}
	if got := stageState(); got != string(runpkg.StageStateAwaitingHostDispatch) {
		t.Errorf("acceptance stage = %q after the refusal, want awaiting_host_dispatch (no state committed)", got)
	}

	// --- full coverage ---
	appendAudit(auditCategorySlicesIntegrated, slicesIntegratedPayload{
		ConsolidatedBranch: "fishhawk/run-" + parent.ID.String(),
		ChildRunIDs:        []string{childA.ID.String(), childB.ID.String()},
	})
	_, out2, err := r.awaitChildren(ctx, nil, AwaitChildrenInput{RunID: parent.ID.String(), TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("awaitChildren (full): %v", err)
	}
	if out2.Status != "children_settled" {
		t.Fatalf("await status = %q, want children_settled after full coverage", out2.Status)
	}
	res, err := r.api.HostDispatchStage(ctx, parent.ID, acc.ID)
	if err != nil {
		t.Fatalf("HostDispatchStage (full): %v", err)
	}
	if !res.Transitioned {
		t.Errorf("transitioned = false, want true once every child is integrated")
	}
	if got := stageState(); got != string(runpkg.StageStateDispatched) {
		t.Errorf("acceptance stage = %q, want dispatched", got)
	}
}
