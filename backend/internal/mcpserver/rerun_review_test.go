package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// rerunReviewStub answers POST /v0/runs/{run_id}/reviews/rerun with
// status/body, recording each request line.
type rerunReviewStub struct {
	requests []string
	status   int
	body     string
}

func (b *rerunReviewStub) resolver(t *testing.T) *runResolver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return newResolver(ts, nil)
}

// TestRerunReview_PassesThrough: the tool POSTs the run-scoped rerun route,
// decodes every field of the backend's 200 body (a tag drift would zero one)
// and names fishhawk_await_review on the implement stage as the follow-up.
func TestRerunReview_PassesThrough(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.NewString()
	stub := &rerunReviewStub{body: `{"run_id":"` + runID.String() + `","stage":"implement","stage_id":"` + stageID +
		`","rerun_of_sequence":41,"new_round_sequence":57,"configured_agents":2}`}
	r := stub.resolver(t)

	_, out, err := r.rerunReview(context.Background(), nil, RerunReviewInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("rerunReview: %v", err)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "POST /v0/runs/"+runID.String()+"/reviews/rerun" {
		t.Fatalf("requests = %v, want one POST to the rerun route", stub.requests)
	}
	want := RerunReviewResult{RunID: runID.String(), Stage: "implement", StageID: stageID, RerunOfSequence: 41, NewRoundSequence: 57, ConfiguredAgents: 2}
	if out.Result != want {
		t.Errorf("result = %+v, want %+v", out.Result, want)
	}
	for _, s := range []string{"round 41", "round 57", "2 configured reviewer", "fishhawk_await_review"} {
		if !strings.Contains(out.Message, s) {
			t.Errorf("message missing %q: %q", s, out.Message)
		}
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_await_review" || na.Params["run_id"] != runID.String() || na.Params["stage"] != "implement" {
		t.Errorf("next_action = %+v, want fishhawk_await_review{run_id, stage=implement}", na)
	}
}

// TestRerunReview_EmptyRunIDFallsBackToInput: a body without run_id (an older
// or partial response) still reports the requested run, so next_action never
// carries an empty run_id.
func TestRerunReview_EmptyRunIDFallsBackToInput(t *testing.T) {
	runID := uuid.New()
	stub := &rerunReviewStub{body: `{"stage":"implement","rerun_of_sequence":3,"new_round_sequence":9,"configured_agents":1}`}
	r := stub.resolver(t)

	_, out, err := r.rerunReview(context.Background(), nil, RerunReviewInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("rerunReview: %v", err)
	}
	if out.Result.RunID != runID.String() {
		t.Errorf("result.run_id = %q, want the input run %s", out.Result.RunID, runID)
	}
	if out.NextAction == nil || out.NextAction.Params["run_id"] != runID.String() {
		t.Errorf("next_action = %+v, want run_id %s", out.NextAction, runID)
	}
}

// TestRerunReview_BadUUIDRefusedLocally: an invalid run id never reaches the
// backend.
func TestRerunReview_BadUUIDRefusedLocally(t *testing.T) {
	stub := &rerunReviewStub{body: `{}`}
	r := stub.resolver(t)

	_, _, err := r.rerunReview(context.Background(), nil, RerunReviewInput{RunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want the local UUID refusal", err)
	}
	if len(stub.requests) != 0 {
		t.Errorf("the malformed id reached the backend: %v", stub.requests)
	}
}

// TestRerunReview_RefusalsSurfaceVerbatimWithHint: every backend refusal is a
// tool error carrying the backend code and details verbatim — never a false
// success — plus the remedy hint for the codes that have one.
func TestRerunReview_RefusalsSurfaceVerbatimWithHint(t *testing.T) {
	cases := []struct {
		status int
		code   string
		detail string
		hint   string
	}{
		{http.StatusConflict, "no_review_round", "", "nothing to re-run"},
		{http.StatusConflict, "review_round_in_flight", `"reason":"round_unsettled"`, "fishhawk_reconcile_reviews"},
		{http.StatusConflict, "review_round_has_verdict", `"verdicts":1`, "needs no re-run"},
		{http.StatusConflict, "review_rerun_ineligible", `"reason":"gating_authority"`, "details.reason names why"},
		{http.StatusBadGateway, "review_rerun_not_started", `"reason":"forge_compare_unavailable"`, "call fishhawk_rerun_review again"},
		{http.StatusForbidden, "operator_agent_forbidden", "", ""},
		{http.StatusNotFound, "run_not_found", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			details := "{}"
			if tc.detail != "" {
				details = "{" + tc.detail + "}"
			}
			stub := &rerunReviewStub{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused","details":` + details + `}}`}
			r := stub.resolver(t)

			_, out, err := r.rerunReview(context.Background(), nil, RerunReviewInput{RunID: uuid.NewString()})
			if err == nil {
				t.Fatalf("want a tool error for %s, got output %+v", tc.code, out)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.code) {
				t.Errorf("error = %q, want the backend code %s verbatim", msg, tc.code)
			}
			if tc.detail != "" && !strings.Contains(msg, tc.detail) {
				t.Errorf("error = %q, want the details %s verbatim", msg, tc.detail)
			}
			if tc.hint != "" && !strings.Contains(msg, tc.hint) {
				t.Errorf("error = %q, want the remedy hint %q", msg, tc.hint)
			}
			if tc.hint == "" && strings.Contains(msg, " — ") {
				t.Errorf("error = %q, want no remedy hint for %s", msg, tc.code)
			}
			if out.NextAction != nil {
				t.Errorf("next_action = %+v on a refusal, want none", out.NextAction)
			}
		})
	}
}

// TestRerunReview_TransportErrorSurfaces: a non-apiError failure (the backend
// unreachable) is still a tool error, with no hint.
func TestRerunReview_TransportErrorSurfaces(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := newResolver(ts, nil)
	ts.Close()

	_, _, err := r.rerunReview(context.Background(), nil, RerunReviewInput{RunID: uuid.NewString()})
	if err == nil || !strings.HasPrefix(err.Error(), "rerun review: ") {
		t.Fatalf("err = %v, want a wrapped transport error", err)
	}
}

// TestRerunReviewRefusalHint pins the pure hint table, including the empty
// default.
func TestRerunReviewRefusalHint(t *testing.T) {
	for _, code := range []string{"no_review_round", "review_round_in_flight", "review_round_has_verdict", "review_rerun_ineligible", "review_rerun_not_started"} {
		if rerunReviewRefusalHint(code) == "" {
			t.Errorf("hint(%s) is empty, want a remedy", code)
		}
	}
	if got := rerunReviewRefusalHint("insufficient_scope"); got != "" {
		t.Errorf("hint(insufficient_scope) = %q, want empty", got)
	}
}
