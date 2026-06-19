package main

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
