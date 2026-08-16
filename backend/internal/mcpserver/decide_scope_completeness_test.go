package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
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
// 500 refusal reaching the operator by CODE, so a backend guard cannot fire
// while the tool reports something generic.
func TestDecideScopeCompleteness_AmendBackendRefusalsSurfaced(t *testing.T) {
	for _, code := range []string{
		"amend_incomplete_coverage",
		"amend_refused_owner_slice_active",
		"amend_owner_attribution_unresolved",
		"amend_budget_exhausted",
		"amend_unrecorded",
		"amend_resume_failed",
	} {
		t.Run(code, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			fb.decideScopeCompletenessStatus = http.StatusConflict
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
