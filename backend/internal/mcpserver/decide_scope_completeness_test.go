package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	planpkg "github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	runmodel "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// --- fishhawk_decide_scope_completeness (#1231) ---

func TestDecideScopeCompleteness_ExemptThreadsBody(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.decideScopeCompletenessResp[runID] = ScopeCompletenessDecisionResult{
		RunID:          runID.String(),
		StageID:        uuid.New().String(),
		Decision:       "exempt",
		State:          "running",
		HeldCommitSHA:  "abc123",
		RunBranch:      "fishhawk/run-x",
		MissingPaths:   []string{"pkg/declared_test.go"},
		PullRequestURL: "https://github.com/x/y/pull/7",
	}

	_, out, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    runID.String(),
		Decision: "exempt",
		Reason:   "the declared test sibling is genuinely unchanged this slice",
	})
	if err != nil {
		t.Fatalf("decideScopeCompleteness: %v", err)
	}
	if out.Result.Decision != "exempt" || out.Result.HeldCommitSHA != "abc123" {
		t.Errorf("result = %+v, want exempt with held SHA abc123", out.Result)
	}
	if out.Result.PullRequestURL != "https://github.com/x/y/pull/7" {
		t.Errorf("PullRequestURL = %q, want the held commit's opened PR", out.Result.PullRequestURL)
	}
	if fb.decideScopeCompletenessCalled[runID] != 1 {
		t.Errorf("decision called %d times, want exactly 1", fb.decideScopeCompletenessCalled[runID])
	}
	if fb.decideScopeCompletenessBody.Decision != "exempt" ||
		fb.decideScopeCompletenessBody.Reason != "the declared test sibling is genuinely unchanged this slice" {
		t.Errorf("body = %+v, want the threaded decision + reason", fb.decideScopeCompletenessBody)
	}
}

func TestDecideScopeCompleteness_FailThreadsBody(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.decideScopeCompletenessResp[runID] = ScopeCompletenessDecisionResult{
		RunID:    runID.String(),
		StageID:  uuid.New().String(),
		Decision: "fail",
		State:    "failed",
	}

	_, out, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    runID.String(),
		Decision: "fail",
		Reason:   "the missing file really must be written; route to category-B",
	})
	if err != nil {
		t.Fatalf("decideScopeCompleteness: %v", err)
	}
	if out.Result.Decision != "fail" || out.Result.State != "failed" {
		t.Errorf("result = %+v, want fail -> failed (category-B)", out.Result)
	}
	if out.Result.PullRequestURL != "" {
		t.Errorf("PullRequestURL = %q, want empty on a fail decision (no PR opened)", out.Result.PullRequestURL)
	}
	if fb.decideScopeCompletenessBody.Decision != "fail" {
		t.Errorf("body = %+v, want decision=fail threaded", fb.decideScopeCompletenessBody)
	}
}

func TestDecideScopeCompleteness_InvalidUUIDBeforeHTTP(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    "not-a-uuid",
		Decision: "exempt",
		Reason:   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID validation error before the HTTP hop", err)
	}
	if len(fb.decideScopeCompletenessCalled) != 0 {
		t.Errorf("backend hit despite invalid UUID")
	}
}

func TestDecideScopeCompleteness_RejectsBadDecisionBeforeHTTP(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    runID.String(),
		Decision: "approve", // valid for amendments, NOT for scope completeness
		Reason:   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "exempt") {
		t.Errorf("err = %v, want decision validation error naming exempt/fail", err)
	}
	if fb.decideScopeCompletenessCalled[runID] != 0 {
		t.Errorf("backend hit despite invalid decision")
	}
}

func TestDecideScopeCompleteness_RejectsEmptyReasonBeforeHTTP(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    runID.String(),
		Decision: "exempt",
		Reason:   "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("err = %v, want empty-reason validation error before the HTTP hop", err)
	}
	if fb.decideScopeCompletenessCalled[runID] != 0 {
		t.Errorf("backend hit despite empty reason")
	}
}

func TestDecideScopeCompleteness_BackendErrorsSurfaced(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		errBody string
		wantSub string
	}{
		{"run token forbidden", http.StatusForbidden,
			`{"error":{"code":"run_token_forbidden","message":"a run-bound agent token may not decide a scope-completeness park"}}`,
			"run_token_forbidden"},
		{"not parked", http.StatusConflict,
			`{"error":{"code":"scope_completeness_not_parked","message":"the implement stage is not parked in awaiting_scope_decision"}}`,
			"scope_completeness_not_parked"},
		{"validation failed", http.StatusBadRequest,
			`{"error":{"code":"validation_failed","message":"decision must be exempt or fail"}}`,
			"validation_failed"},
		{"not found", http.StatusNotFound,
			`{"error":{"code":"run_not_found","message":"no run with that id"}}`,
			"run_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			fb.decideScopeCompletenessStatus = tc.status
			fb.decideScopeCompletenessErr = tc.errBody
			r := newResolver(srv, nil)

			_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
				RunID:    uuid.New().String(),
				Decision: "exempt",
				Reason:   "forced",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want %q surfaced", err, tc.wantSub)
			}
		})
	}
}

// TestDecideScopeCompleteness_AssertionClassParkResult pins the #2501 second
// shortfall class at the MCP surface: an exempt decision on a park whose
// shortfall is an unsatisfied binding assertion (empty MissingPaths) threads
// through unchanged and surfaces the pending-then-dispatch state the backend
// now returns.
func TestDecideScopeCompleteness_AssertionClassParkResult(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.decideScopeCompletenessResp[runID] = ScopeCompletenessDecisionResult{
		RunID:         runID.String(),
		StageID:       uuid.New().String(),
		Decision:      "exempt",
		State:         "pending",
		HeldCommitSHA: "abc123",
		RunBranch:     "fishhawk/run-x",
		// No MissingPaths: the shortfall was an unsatisfied binding assertion.
	}

	_, out, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    runID.String(),
		Decision: "exempt",
		Reason:   "the operator-authored literal was wrong, not the implement output",
	})
	if err != nil {
		t.Fatalf("decideScopeCompleteness: %v", err)
	}
	if out.Result.Decision != "exempt" || out.Result.HeldCommitSHA != "abc123" {
		t.Errorf("result = %+v, want exempt with the held SHA", out.Result)
	}
	if len(out.Result.MissingPaths) != 0 {
		t.Errorf("an assertion-class park must carry no missing paths, got %v", out.Result.MissingPaths)
	}
	if out.Result.State != "pending" {
		t.Errorf("State = %q, want pending (#2501: exempt resumes in place for an agent-free re-dispatch)", out.Result.State)
	}
}

// --- fishhawk_decide_scope_completeness: the `amend` decision (#2591) ---

// TestDecideScopeCompleteness_AmendThreadsBody pins the wire threading: the
// decision AND the paths array reach the backend with each entry's declared
// operation intact (an operation dropped in transit would silently downgrade a
// `create` to a `modify` and re-fail the #818/#825 created-file gate), and the
// amend-only response fields decode.
func TestDecideScopeCompleteness_AmendThreadsBody(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.decideScopeCompletenessResp[runID] = ScopeCompletenessDecisionResult{
		RunID:    runID.String(),
		StageID:  uuid.New().String(),
		Decision: "amend",
		State:    "pending",
		AmendedPaths: []ScopeAmendmentPathEntry{
			{Path: "backend/internal/server/prompt.go", Operation: "modify"},
			{Path: "backend/internal/server/new.go", Operation: "create"},
		},
		AmendmentID:                   "8f14e45f-ceea-467a-9b2e-1f0d0b2b0000",
		AgentAmendmentBudgetRemaining: 1,
	}

	_, out, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    runID.String(),
		Decision: "amend",
		Reason:   "the coupled endpoint file is build-required by this slice",
		Paths: []ScopeAmendmentPathEntry{
			{Path: "backend/internal/server/prompt.go", Operation: "modify"},
			{Path: "backend/internal/server/new.go", Operation: "create"},
		},
	})
	if err != nil {
		t.Fatalf("decideScopeCompleteness: %v", err)
	}
	if fb.decideScopeCompletenessBody.Decision != "amend" {
		t.Errorf("body = %+v, want decision=amend threaded", fb.decideScopeCompletenessBody)
	}
	got := fb.decideScopeCompletenessBody.Paths
	if len(got) != 2 {
		t.Fatalf("body paths = %+v, want both entries threaded", got)
	}
	if got[0].Path != "backend/internal/server/prompt.go" || got[0].Operation != "modify" {
		t.Errorf("body paths[0] = %+v, want the modify entry with its operation intact", got[0])
	}
	if got[1].Path != "backend/internal/server/new.go" || got[1].Operation != "create" {
		t.Errorf("body paths[1] = %+v, want the create entry with its operation intact", got[1])
	}
	if out.Result.AmendmentID != "8f14e45f-ceea-467a-9b2e-1f0d0b2b0000" {
		t.Errorf("AmendmentID = %q, want the pre-approved row id decoded", out.Result.AmendmentID)
	}
	if out.Result.AgentAmendmentBudgetRemaining != 1 {
		t.Errorf("AgentAmendmentBudgetRemaining = %d, want the surfaced cost of the operator amend",
			out.Result.AgentAmendmentBudgetRemaining)
	}
	if len(out.Result.AmendedPaths) != 2 {
		t.Errorf("AmendedPaths = %+v, want both amended entries decoded", out.Result.AmendedPaths)
	}
}

// TestDecideScopeCompleteness_AmendThreadsAcknowledgement pins the
// acknowledge_owner_unresolved flag reaching the backend and the audited
// acknowledgement decoding back — the flag is the operator's explicit
// risk-taking, so a silently dropped `true` would turn a knowing decision into
// a 409 the operator cannot clear.
func TestDecideScopeCompleteness_AmendThreadsAcknowledgement(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.decideScopeCompletenessResp[runID] = ScopeCompletenessDecisionResult{
		RunID:                       runID.String(),
		StageID:                     uuid.New().String(),
		Decision:                    "amend",
		State:                       "pending",
		OwnerAttributionUnresolved:  true,
		AcknowledgedOwnerUnresolved: true,
		OwnerUnresolvedPaths: []ScopeCompletenessUnresolvedOwner{
			{Path: "a/b.go", Reason: "no_owning_slice_entry"},
		},
	}

	_, out, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:                      runID.String(),
		Decision:                   "amend",
		Reason:                     "attribution degraded; accepting the sibling-conflict risk",
		Paths:                      []ScopeAmendmentPathEntry{{Path: "a/b.go", Operation: "modify"}},
		AcknowledgeOwnerUnresolved: true,
	})
	if err != nil {
		t.Fatalf("decideScopeCompleteness: %v", err)
	}
	if !fb.decideScopeCompletenessBody.AcknowledgeOwnerUnresolved {
		t.Errorf("body = %+v, want acknowledge_owner_unresolved threaded true", fb.decideScopeCompletenessBody)
	}
	if !out.Result.AcknowledgedOwnerUnresolved || !out.Result.OwnerAttributionUnresolved {
		t.Errorf("result = %+v, want the audited acknowledgement decoded on BOTH flags", out.Result)
	}
	// The per-path detail is what makes the relaxation legible: WHICH boundary
	// is unprovable and WHY, not merely that one is.
	if len(out.Result.OwnerUnresolvedPaths) != 1 ||
		out.Result.OwnerUnresolvedPaths[0].Path != "a/b.go" ||
		out.Result.OwnerUnresolvedPaths[0].Reason != "no_owning_slice_entry" {
		t.Errorf("result owner_unresolved_paths = %+v, want the acknowledged path AND its reason decoded",
			out.Result.OwnerUnresolvedPaths)
	}
	// Decoded from a HANDLER-shaped literal, not the seeded struct: the hop
	// above round-trips one type through itself, so it cannot catch a json tag
	// that has drifted from `backend/internal/server`'s response shape.
	var wire ScopeCompletenessDecisionResult
	if err := json.Unmarshal([]byte(
		`{"owner_unresolved_paths":[{"path":"c/d.go","reason":"owning_slice_index_absent"}]}`), &wire); err != nil {
		t.Fatalf("unmarshal handler-shaped body: %v", err)
	}
	if len(wire.OwnerUnresolvedPaths) != 1 || wire.OwnerUnresolvedPaths[0].Path != "c/d.go" ||
		wire.OwnerUnresolvedPaths[0].Reason != "owning_slice_index_absent" {
		t.Errorf("handler-shaped body decoded to %+v, want the {path, reason} entry — the json tags must mirror the handler",
			wire.OwnerUnresolvedPaths)
	}
}

// TestDecideScopeCompleteness_RejectsAmendWithoutPathsBeforeHTTP: an amend with
// nothing to fold is a no-op. Asserted through the NEVER-DIALED httptest seam
// (the backend handler's per-run counter stays empty) rather than only on the
// error string, so a rejection that actually reached the wire cannot pass.
func TestDecideScopeCompleteness_RejectsAmendWithoutPathsBeforeHTTP(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    runID.String(),
		Decision: "amend",
		Reason:   "widen the slice",
	})
	if err == nil || !strings.Contains(err.Error(), "requires paths") {
		t.Errorf("err = %v, want an amend-without-paths rejection before the HTTP hop", err)
	}
	if len(fb.decideScopeCompletenessCalled) != 0 {
		t.Errorf("backend DIALED despite the pre-HTTP rejection: %v", fb.decideScopeCompletenessCalled)
	}
}

// TestDecideScopeCompleteness_RejectsPathsOnNonAmendBeforeHTTP: paths on an
// exempt/fail decision would silently go nowhere. Same never-dialed seam.
func TestDecideScopeCompleteness_RejectsPathsOnNonAmendBeforeHTTP(t *testing.T) {
	for _, decision := range []string{"exempt", "fail"} {
		t.Run(decision, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)

			_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
				RunID:    uuid.New().String(),
				Decision: decision,
				Reason:   "r",
				Paths:    []ScopeAmendmentPathEntry{{Path: "a/b.go", Operation: "modify"}},
			})
			if err == nil || !strings.Contains(err.Error(), "only be supplied with decision") {
				t.Errorf("err = %v, want a paths-on-non-amend rejection before the HTTP hop", err)
			}
			if len(fb.decideScopeCompletenessCalled) != 0 {
				t.Errorf("backend DIALED despite the pre-HTTP rejection: %v", fb.decideScopeCompletenessCalled)
			}
		})
	}
}

// TestDecideScopeCompleteness_RejectsAcknowledgementOnNonAmendBeforeHTTP: the
// acknowledgement is amend-only — only the amend arm runs an owner-slice guard
// — so on exempt/fail it would be forwarded, silently ignored, and recorded on
// no audit entry, leaving the operator believing they had taken an AUDITED
// risk. Same never-dialed seam: the flag must not reach the wire at all.
func TestDecideScopeCompleteness_RejectsAcknowledgementOnNonAmendBeforeHTTP(t *testing.T) {
	for _, decision := range []string{"exempt", "fail"} {
		t.Run(decision, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)

			_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
				RunID:                      uuid.New().String(),
				Decision:                   decision,
				Reason:                     "r",
				AcknowledgeOwnerUnresolved: true,
			})
			if err == nil || !strings.Contains(err.Error(), "acknowledge_owner_unresolved may only be supplied") {
				t.Errorf("err = %v, want an acknowledgement-on-non-amend rejection before the HTTP hop", err)
			}
			if len(fb.decideScopeCompletenessCalled) != 0 {
				t.Errorf("backend DIALED despite the pre-HTTP rejection: %v", fb.decideScopeCompletenessCalled)
			}
		})
	}
}

// TestDecideScopeCompleteness_RejectsBadDecisionNamesAmend keeps the enum
// message honest now that a third decision exists.
func TestDecideScopeCompleteness_RejectsBadDecisionNamesAmend(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
		RunID:    uuid.New().String(),
		Decision: "approve",
		Reason:   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "amend") {
		t.Errorf("err = %v, want the enum error to name amend", err)
	}
}

// TestDecideScopeCompleteness_AmendBackendRefusalsSurfaced pins each new 409 /
// 500 refusal reaching the operator by CODE — replayed at the status the
// backend REALLY emits for it — so a backend guard cannot fire while the tool
// reports something generic.
func TestDecideScopeCompleteness_AmendBackendRefusalsSurfaced(t *testing.T) {
	// Each row carries the status the backend ACTUALLY emits for that code: the
	// two persistence failures are 500s, and replaying them at 409 would leave
	// the 5xx-class path (a client-side retry or error-envelope branch keyed on
	// the status rather than the code) untested.
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"amend_incomplete_coverage", http.StatusConflict},
		{"amend_refused_owner_slice_active", http.StatusConflict},
		{"amend_owner_attribution_unresolved", http.StatusConflict},
		{"amend_budget_exhausted", http.StatusConflict},
		{"amend_unrecorded", http.StatusInternalServerError},
		{"amend_resume_failed", http.StatusInternalServerError},
		{"amendment_path_owned_by_sibling_slice", http.StatusConflict},
		{"slice_ownership_check_failed", http.StatusInternalServerError},
	} {
		code := tc.code
		t.Run(code, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			fb.decideScopeCompletenessStatus = tc.status
			fb.decideScopeCompletenessErr = `{"error":{"code":"` + code + `","message":"refused"}}`
			r := newResolver(srv, nil)

			_, _, err := r.decideScopeCompleteness(context.Background(), nil, DecideScopeCompletenessInput{
				RunID:    uuid.New().String(),
				Decision: "amend",
				Reason:   "forced",
				Paths:    []ScopeAmendmentPathEntry{{Path: "a/b.go", Operation: "modify"}},
			})
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Errorf("err = %v, want %q surfaced", err, code)
			}
		})
	}
}

// amendOwnershipE2E is a real-Postgres decomposition with the requester parked at
// slice 0 depends_on [2], and slice 1 owning a path with no edge.
type amendOwnershipE2E struct {
	parent  *runmodel.Run
	child   *runmodel.Run
	stageID uuid.UUID
	sibling map[int]*runmodel.Run
	impl    map[int]*runmodel.Stage
}

// parkRepo is the optional capability the runner-report handler type-asserts.
type parkRepo interface {
	ParkScopeCompletenessAndAppend(ctx context.Context, stageID uuid.UUID, park runmodel.ScopeCompletenessPark, p audit.ChainAppendParams) (*runmodel.Stage, bool, error)
}

func seedAmendOwnershipE2E(t *testing.T, ctx context.Context, runRepo runmodel.Repository, artRepo artifact.Repository) amendOwnershipE2E {
	t.Helper()
	mint := func(parent *uuid.UUID, idx *int) *runmodel.Run {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, runmodel.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runmodel.TriggerCLI,
			RunnerKind: runmodel.RunnerKindLocal, ParentRunID: parent, DecomposedFrom: parent, SliceIndex: idx,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return r
	}
	newStage := func(runID uuid.UUID, seq int, typ runmodel.StageType) *runmodel.Stage {
		t.Helper()
		st, err := runRepo.CreateStage(ctx, runmodel.CreateStageParams{
			RunID: runID, Sequence: seq, Type: typ, ExecutorKind: runmodel.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("create stage: %v", err)
		}
		return st
	}
	parent := mint(nil, nil)
	locker, ok := any(runRepo).(interface {
		ResolveRunnerKind(context.Context, uuid.UUID, string) (runmodel.RunnerKindResolution, error)
	})
	if !ok {
		t.Fatal("run repository cannot lock runner_kind")
	}
	if _, err := locker.ResolveRunnerKind(ctx, parent.ID, runmodel.RunnerKindLocal); err != nil {
		t.Fatalf("lock parent: %v", err)
	}
	planStage := newStage(parent.ID, 0, runmodel.StageTypePlan)
	scope := func(path string) *planpkg.Scope {
		return &planpkg.Scope{Files: []planpkg.ScopeFile{{Path: path, Operation: planpkg.FileOpModify}}}
	}
	body, err := json.Marshal(planpkg.Plan{
		PlanVersion: "standard_v1", Summary: "s",
		Verification: planpkg.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Decomposition: &planpkg.Decomposition{Rationale: "split", SubPlans: []planpkg.SubPlanSummary{
			{Title: "own", ScopeHint: "own", Scope: scope("own/"), DependsOn: []int{2}},
			{Title: "stranger", ScopeHint: "docs", Scope: scope("docs/b.md")},
			{Title: "ancestor", ScopeHint: "docs", Scope: scope("docs/a.md")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sv := "standard_v1"
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: body,
		ContentHash: uuid.NewString() + uuid.NewString(),
	}); err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}
	out := amendOwnershipE2E{parent: parent, sibling: map[int]*runmodel.Run{}, impl: map[int]*runmodel.Stage{}}
	pid := parent.ID
	for i := 0; i < 3; i++ {
		idx := i
		c := mint(&pid, &idx)
		st := newStage(c.ID, 1, runmodel.StageTypeImplement)
		if i == 0 {
			out.child, out.stageID = c, st.ID
			continue
		}
		out.sibling[i], out.impl[i] = c, st
	}
	for _, to := range []runmodel.StageState{runmodel.StageStateDispatched, runmodel.StageStateRunning} {
		if _, err := runRepo.TransitionStage(ctx, out.stageID, to, nil); err != nil {
			t.Fatalf("transition requester: %v", err)
		}
	}
	parker, ok := any(runRepo).(parkRepo)
	if !ok {
		t.Fatal("run repository cannot park")
	}
	stageID := out.stageID
	actor := audit.ActorSystem
	subject := "runner"
	if _, _, err := parker.ParkScopeCompletenessAndAppend(ctx, out.stageID, runmodel.ScopeCompletenessPark{
		HeldCommitSHA: strings.Repeat("1", 40), RunBranch: "fishhawk/run-x", VerifiedTreeSHA: strings.Repeat("2", 40),
		BaseSHA: strings.Repeat("3", 40), MissingPaths: []string{"docs/never-edited.md"},
	}, audit.ChainAppendParams{
		RunID: out.child.ID, StageID: &stageID, Timestamp: time.Now().UTC(), Category: "scope_completeness_parked",
		ActorKind: &actor, ActorSubject: &subject, Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	return out
}

// deliver walks the ancestor to succeeded; integrate records it on the parent's
// real chain.
func (e amendOwnershipE2E) deliver(t *testing.T, ctx context.Context, runRepo runmodel.Repository, idx int) {
	t.Helper()
	for _, to := range []runmodel.StageState{runmodel.StageStateDispatched, runmodel.StageStateRunning, runmodel.StageStateSucceeded} {
		if _, err := runRepo.TransitionStage(ctx, e.impl[idx].ID, to, nil); err != nil {
			t.Fatalf("transition implement: %v", err)
		}
	}
	for _, to := range []runmodel.State{runmodel.StateRunning, runmodel.StateSucceeded} {
		if _, err := runRepo.TransitionRun(ctx, e.sibling[idx].ID, to); err != nil {
			t.Fatalf("transition run: %v", err)
		}
	}
}

func (e amendOwnershipE2E) integrate(t *testing.T, ctx context.Context, auditRepo audit.Repository, idx int) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"consolidated_branch": "fishhawk/run-parent", "child_run_ids": []string{e.sibling[idx].ID.String()}})
	actor := audit.ActorSystem
	subject := "test"
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: e.parent.ID, Timestamp: time.Now().UTC(), Category: "slices_integrated",
		ActorKind: &actor, ActorSubject: &subject, Payload: payload,
	}); err != nil {
		t.Fatalf("append slices_integrated: %v", err)
	}
}

// TestDecideScopeCompleteness_AmendOwnershipRefusal_EndToEnd crosses the MCP tool,
// the real API client, the real HTTP handler and real Postgres repositories. The
// delivered-ancestor control goes through identical wiring, so the refusals cannot
// be a 401/403 masking them.
func TestDecideScopeCompleteness_AmendOwnershipRefusal_EndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runmodel.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	amendRepo := scopeamendment.NewPostgresRepository(pool)

	const bearer = "fhk_amend_ownership_e2e"
	srv := server.New(server.Config{
		RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo, ScopeAmendmentRepo: amendRepo,
		SigningRepo: signing.NewPostgresRepository(pool),
		APITokenRepo: &stubMCPAPITokens{tok: &apitoken.Token{
			ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit", "write:runs", "write:stages"}, PlainText: bearer,
		}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: bearer}), getenv: envFuncFromMap(nil)}

	amend := func(child uuid.UUID, path string, ack bool) error {
		_, _, err := r.decideScopeCompleteness(ctx, nil, DecideScopeCompletenessInput{
			RunID: child.String(), Decision: "amend", Reason: "widen", AcknowledgeOwnerUnresolved: ack,
			Paths: []ScopeAmendmentPathEntry{{Path: path, Operation: "modify"}},
		})
		return err
	}
	apiErrOf := func(t *testing.T, err error) *apiError {
		t.Helper()
		var ae *apiError
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v, want an *apiError through the chain", err)
		}
		return ae
	}
	assertParked := func(t *testing.T, seeded amendOwnershipE2E) {
		t.Helper()
		rows, err := amendRepo.ListByRun(ctx, seeded.child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("%d amendment rows, want 0", len(rows))
		}
		st, err := runRepo.GetStage(ctx, seeded.stageID)
		if err != nil {
			t.Fatal(err)
		}
		if st.State != runmodel.StageStateAwaitingScopeDecision {
			t.Errorf("stage state = %q, want parked", st.State)
		}
	}

	t.Run("non_ancestor", func(t *testing.T) {
		seeded := seedAmendOwnershipE2E(t, ctx, runRepo, artRepo)
		ae := apiErrOf(t, amend(seeded.child.ID, "docs/b.md", false))
		if ae.Code != "amendment_path_owned_by_sibling_slice" || ae.StatusCode != http.StatusConflict {
			t.Fatalf("got %d %q: %v", ae.StatusCode, ae.Code, ae)
		}
		conflicts, _ := ae.Details["conflicts"].([]any)
		if len(conflicts) == 0 {
			t.Fatalf("details.conflicts empty: %v", ae.Details)
		}
		first, _ := conflicts[0].(map[string]any)
		if first["owning_slice"] != float64(1) || first["owning_child_run_id"] != seeded.sibling[1].ID.String() {
			t.Errorf("conflicts[0] = %v, want slice 1 and its child", first)
		}
		if remedy, _ := ae.Details["remedy"].(string); !strings.Contains(remedy, "corrected decomposition") {
			t.Errorf("details.remedy = %q", remedy)
		}
		if ae.Details["retryable"] != false {
			t.Errorf("details.retryable = %v, want false", ae.Details["retryable"])
		}
		assertParked(t, seeded)
	})

	t.Run("unproven_ancestor_acknowledged", func(t *testing.T) {
		seeded := seedAmendOwnershipE2E(t, ctx, runRepo, artRepo)
		seeded.deliver(t, ctx, runRepo, 2)
		ae := apiErrOf(t, amend(seeded.child.ID, "docs/a.md", true))
		if ae.Code != "amend_refused_owner_slice_active" || ae.StatusCode != http.StatusConflict {
			t.Fatalf("got %d %q: %v", ae.StatusCode, ae.Code, ae)
		}
		if ae.Details["reason"] != "ancestor_not_integrated" || ae.Details["retryable"] != true {
			t.Errorf("details = %v, want ancestor_not_integrated retryable", ae.Details)
		}
		if ae.Details["sibling_run_id"] != seeded.sibling[2].ID.String() {
			t.Errorf("sibling_run_id = %v, want %s", ae.Details["sibling_run_id"], seeded.sibling[2].ID)
		}
		if remedy, _ := ae.Details["remedy"].(string); remedy == "" {
			t.Errorf("details.remedy empty: %v", ae.Details)
		}
		assertParked(t, seeded)
	})

	t.Run("delivered_ancestor_control", func(t *testing.T) {
		seeded := seedAmendOwnershipE2E(t, ctx, runRepo, artRepo)
		seeded.deliver(t, ctx, runRepo, 2)
		seeded.integrate(t, ctx, auditRepo, 2)
		if err := amend(seeded.child.ID, "docs/a.md", false); err != nil {
			t.Fatalf("amend naming a delivered ancestor's path: %v", err)
		}
		st, err := runRepo.GetStage(ctx, seeded.stageID)
		if err != nil {
			t.Fatal(err)
		}
		if st.State != runmodel.StageStatePending {
			t.Errorf("stage state = %q, want pending", st.State)
		}
	})
}
