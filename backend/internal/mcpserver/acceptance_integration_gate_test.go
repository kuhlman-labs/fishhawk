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
	foldAcceptanceIntegrationHold("parent-1", partialChildrenStatus(), "succeeded", na)

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
	foldAcceptanceIntegrationHold("p", cs, "succeeded", na)
	if na.State != acceptanceHeldIntegrationIncompleteState {
		t.Fatalf("state = %q, want held", na.State)
	}
	if r := na.Actions[0].Reason; !strings.Contains(r, "slice_head_missing") || !strings.Contains(r, "child-b") {
		t.Errorf("reason %q must name the failure cause and child", r)
	}
	// Running children with no uncovered succeeded child: the generic reason.
	cs2 := &ChildrenStatus{IntegrationPhase: integrationPhaseRunningChildren, Children: []ChildStatus{{RunID: "x", State: "running"}}, fanInRecorded: true}
	na2 := acceptancePendingActions("p")
	foldAcceptanceIntegrationHold("p", cs2, "succeeded", na2)
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
		na          func() *NextActions
	}{
		{"integrated", integrated, "succeeded", func() *NextActions { return acceptancePendingActions("p") }},
		{"nil children status", nil, "succeeded", func() *NextActions { return acceptancePendingActions("p") }},
		{"zero children", noChildren, "succeeded", func() *NextActions { return acceptancePendingActions("p") }},
		{"no acceptance dispatch offered", partialChildrenStatus(), "succeeded", func() *NextActions {
			return &NextActions{State: "review_pending", Actions: []SuggestedAction{{Action: "fishhawk_await_review"}}}
		}},
		{"C3 no integration authority", noAuthority, "succeeded", func() *NextActions { return acceptancePendingActions("p") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			na := c.na()
			before, _ := json.Marshal(na)
			foldAcceptanceIntegrationHold("p", c.cs, c.parentState, na)
			after, _ := json.Marshal(na)
			if string(before) != string(after) {
				t.Errorf("next_actions changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	// nil next_actions must not panic.
	foldAcceptanceIntegrationHold("p", partialChildrenStatus(), "succeeded", nil)
}

// TestIntegrationAuthorityAbsent pins the C3 inference: only NO fan-in record
// AND a SUCCEEDED parent implement stage read as authority-less. A record of
// any kind, a parent still awaiting_children, or an UNREADABLE parent stage
// keeps the hold.
func TestIntegrationAuthorityAbsent(t *testing.T) {
	none := &ChildrenStatus{}
	recorded := &ChildrenStatus{fanInRecorded: true}
	cases := []struct {
		name  string
		cs    *ChildrenStatus
		state string
		want  bool
	}{
		{"no record + succeeded", none, "succeeded", true},
		{"no record + awaiting_children", none, "awaiting_children", false},
		{"no record + unreadable", none, "", false},
		{"record + succeeded", recorded, "succeeded", false},
		{"nil status", nil, "succeeded", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := integrationAuthorityAbsent(c.cs, c.state); got != c.want {
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
