package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// promoteStub answers POST /v0/stages/{stage_id}/concurrency/promote with
// status/body and GET /v0/runs/{run_id}/stages with stagesStatus/stagesBody,
// recording each request line and the promote request body.
type promoteStub struct {
	requests     []string
	bodies       []string
	status       int
	body         string
	stagesStatus int
	stagesBody   string
}

func (b *promoteStub) resolver(t *testing.T) *runResolver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if b.stagesStatus != 0 {
				w.WriteHeader(b.stagesStatus)
			}
			_, _ = io.WriteString(w, b.stagesBody)
			return
		}
		b.bodies = append(b.bodies, string(raw))
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return newResolver(ts, nil)
}

// promotedBody is a server-shaped 200 body (stage_concurrency_promote.go's
// promoteQueuedStageResponse field set, byte-for-byte tags).
func promotedBody(stageID, runID, holderRun, holderStage string, since time.Time) string {
	return `{"stage_id":"` + stageID + `","run_id":"` + runID + `","group":"local-implement:h1","limit":1,"promoted":true,` +
		`"old_position":3,"new_position":1,"holders":[{"run_id":"` + holderRun + `","stage_id":"` + holderStage + `","since":"` +
		since.Format(time.RFC3339Nano) + `"}],"audit_sequence":42}`
}

// TestPromoteQueuedStage_PassesThrough: the tool POSTs the stage-scoped
// promote route with exactly {"reason": <trimmed>}, decodes every field of the
// backend's 200 body (a tag drift would zero one) and names
// fishhawk_await_stage on the promoted stage, with its type read from the run.
func TestPromoteQueuedStage_PassesThrough(t *testing.T) {
	stageID, runID := uuid.NewString(), uuid.NewString()
	holderRun, holderStage := uuid.NewString(), uuid.NewString()
	since := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	stub := &promoteStub{
		body:       promotedBody(stageID, runID, holderRun, holderStage, since),
		stagesBody: `{"items":[{"id":"` + uuid.NewString() + `","type":"plan"},{"id":"` + stageID + `","type":"implement"}]}`,
	}
	r := stub.resolver(t)

	_, out, err := r.promoteQueuedStage(context.Background(), nil, PromoteQueuedStageInput{StageID: stageID, Reason: "  C fixes the shared flake  "})
	if err != nil {
		t.Fatalf("promoteQueuedStage: %v", err)
	}
	if len(stub.requests) != 2 || stub.requests[0] != "POST /v0/stages/"+stageID+"/concurrency/promote" ||
		stub.requests[1] != "GET /v0/runs/"+runID+"/stages" {
		t.Fatalf("requests = %v, want the promote POST then the run's stage read", stub.requests)
	}
	if len(stub.bodies) != 1 || stub.bodies[0] != `{"reason":"C fixes the shared flake"}` {
		t.Fatalf("promote body = %v, want exactly {\"reason\":<trimmed>}", stub.bodies)
	}
	res := out.Result
	if res.StageID != stageID || res.RunID != runID || res.Group != "local-implement:h1" || res.Limit != 1 || !res.Promoted ||
		res.OldPosition != 3 || res.NewPosition != 1 || res.AuditSequence == nil || *res.AuditSequence != 42 {
		t.Errorf("result = %+v, want every field of the server body", res)
	}
	if len(res.Holders) != 1 || res.Holders[0].RunID != holderRun || res.Holders[0].StageID != holderStage || !res.Holders[0].Since.Equal(since) {
		t.Errorf("holders = %+v, want the one server holder", res.Holders)
	}
	for _, s := range []string{"from position 3 to 1", "audit sequence 42", `"local-implement:h1"`, "fishhawk_await_stage"} {
		if !strings.Contains(out.Message, s) {
			t.Errorf("message missing %q: %q", s, out.Message)
		}
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_await_stage" || na.Params["run_id"] != runID || na.Params["stage_id"] != stageID ||
		na.Params["stage"] != "implement" || na.Consumes != consumesNone {
		t.Errorf("next_action = %+v, want fishhawk_await_stage{run_id, stage_id, stage=implement}", na)
	}
}

// TestPromoteQueuedStage_NoopAndUnreadableStageType: an already-first stage
// answers promoted:false, and the message says nothing was recorded; when the
// stage-type read fails, next_action omits stage and its reason says so.
func TestPromoteQueuedStage_NoopAndUnreadableStageType(t *testing.T) {
	stageID, runID := uuid.NewString(), uuid.NewString()
	stub := &promoteStub{
		body: `{"stage_id":"` + stageID + `","run_id":"` + runID + `","group":"spec:o/r:db","limit":2,"promoted":false,` +
			`"old_position":1,"new_position":1,"holders":[]}`,
		stagesStatus: http.StatusInternalServerError,
		stagesBody:   `{"error":{"code":"internal","message":"boom"}}`,
	}
	r := stub.resolver(t)

	_, out, err := r.promoteQueuedStage(context.Background(), nil, PromoteQueuedStageInput{StageID: stageID, Reason: "unblocks"})
	if err != nil {
		t.Fatalf("promoteQueuedStage: %v", err)
	}
	if out.Result.Promoted || out.Result.AuditSequence != nil {
		t.Errorf("result = %+v, want the no-op (promoted false, no audit sequence)", out.Result)
	}
	if !strings.Contains(out.Message, "already first") || !strings.Contains(out.Message, "nothing was changed or recorded") {
		t.Errorf("message = %q, want the no-op wording", out.Message)
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_await_stage" || na.Params["stage_id"] != stageID {
		t.Fatalf("next_action = %+v, want fishhawk_await_stage on the stage", na)
	}
	if _, ok := na.Params["stage"]; ok {
		t.Errorf("next_action params = %v, want no stage type after a failed read", na.Params)
	}
	if !strings.Contains(na.Reason, "stage type could not be read") {
		t.Errorf("next_action reason = %q, want the unreadable-type note", na.Reason)
	}
}

// TestPromoteQueuedStage_StageIDFallbackAndNoRunID: a body without stage_id
// still reports the requested stage, and one without run_id names no
// next_action (an await needs the run) and reads no stages.
func TestPromoteQueuedStage_StageIDFallbackAndNoRunID(t *testing.T) {
	stageID := uuid.NewString()
	stub := &promoteStub{body: `{"group":"g","limit":1,"promoted":true,"old_position":2,"new_position":1,"holders":[]}`}
	r := stub.resolver(t)

	_, out, err := r.promoteQueuedStage(context.Background(), nil, PromoteQueuedStageInput{StageID: stageID, Reason: "x"})
	if err != nil {
		t.Fatalf("promoteQueuedStage: %v", err)
	}
	if out.Result.StageID != stageID {
		t.Errorf("result.stage_id = %q, want the input stage %s", out.Result.StageID, stageID)
	}
	if !strings.Contains(out.Message, "audit sequence unreported") {
		t.Errorf("message = %q, want the unreported-sequence wording", out.Message)
	}
	if out.NextAction != nil {
		t.Errorf("next_action = %+v, want none without a run_id", out.NextAction)
	}
	if len(stub.requests) != 1 {
		t.Errorf("requests = %v, want only the promote POST", stub.requests)
	}
}

// TestPromoteQueuedStage_LocalRefusals: an invalid stage id and a blank
// (whitespace-only) reason are refused before the HTTP hop — the fake sees
// ZERO requests.
func TestPromoteQueuedStage_LocalRefusals(t *testing.T) {
	cases := []struct {
		name string
		in   PromoteQueuedStageInput
		want string
	}{
		{"bad uuid", PromoteQueuedStageInput{StageID: "not-a-uuid", Reason: "x"}, "not a valid UUID"},
		{"blank reason", PromoteQueuedStageInput{StageID: uuid.NewString(), Reason: " \t\n "}, "reason is required"},
		{"empty reason", PromoteQueuedStageInput{StageID: uuid.NewString()}, "reason is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &promoteStub{body: `{}`}
			r := stub.resolver(t)
			_, _, err := r.promoteQueuedStage(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want the local refusal %q", err, tc.want)
			}
			if len(stub.requests) != 0 {
				t.Errorf("the refused input reached the backend: %v", stub.requests)
			}
		})
	}
}

// TestPromoteQueuedStage_RefusalsSurfaceVerbatimWithHint: every backend
// refusal is a tool error carrying the backend code and details verbatim —
// never a false success — plus the remedy hint for the codes that have one.
func TestPromoteQueuedStage_RefusalsSurfaceVerbatimWithHint(t *testing.T) {
	cases := []struct {
		status int
		code   string
		hint   string
	}{
		{http.StatusConflict, "concurrency_slot_held", "never preempted"},
		{http.StatusConflict, "concurrency_slot_not_queued", "#4297"},
		{http.StatusConflict, "concurrency_waiter_not_live", "re-dispatch the stage with fishhawk_dispatch_stage"},
		{http.StatusConflict, "concurrency_slot_changed", "re-read the stage's concurrency block"},
		{http.StatusForbidden, "run_token_forbidden", "promote from the operator session"},
		{http.StatusForbidden, "insufficient_scope", "carries write:runs"},
		{http.StatusNotFound, "stage_not_found", ""},
		{http.StatusInternalServerError, "concurrency_promote_failed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			stub := &promoteStub{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused","details":{"stage_id":"s-1"}}}`}
			r := stub.resolver(t)

			_, out, err := r.promoteQueuedStage(context.Background(), nil, PromoteQueuedStageInput{StageID: uuid.NewString(), Reason: "x"})
			if err == nil {
				t.Fatalf("want a tool error for %s, got output %+v", tc.code, out)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.code) || !strings.Contains(msg, `"stage_id":"s-1"`) {
				t.Errorf("error = %q, want the backend code %s and details verbatim", msg, tc.code)
			}
			if tc.hint != "" && !strings.Contains(msg, tc.hint) {
				t.Errorf("error = %q, want the hint containing %q", msg, tc.hint)
			}
			if got := promoteQueuedStageRefusalHint(tc.code); (got != "") != (tc.hint != "") {
				t.Errorf("promoteQueuedStageRefusalHint(%q) = %q, want a hint iff the table names one", tc.code, got)
			}
			if len(stub.requests) != 1 {
				t.Errorf("requests = %v, want exactly the promote POST (no stage read on a refusal)", stub.requests)
			}
		})
	}
}

// TestPromoteQueuedStage_TransportError: an unreachable backend is a tool
// error, never a success.
func TestPromoteQueuedStage_TransportError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := newResolver(ts, nil)
	ts.Close()
	if _, _, err := r.promoteQueuedStage(context.Background(), nil, PromoteQueuedStageInput{StageID: uuid.NewString(), Reason: "x"}); err == nil ||
		!strings.Contains(err.Error(), "promote queued stage") {
		t.Fatalf("err = %v, want a promote transport error", err)
	}
}

// TestStageConcurrency_DecodesPromoted pins the client mirror of the stage
// block's promoted flag (#4191) against a server-shaped block: a tag drift
// decodes it to false.
func TestStageConcurrency_DecodesPromoted(t *testing.T) {
	var c StageConcurrency
	if err := json.Unmarshal([]byte(`{"status":"awaiting_concurrency_slot","group":"local-implement:h1","limit":1,"position":1,"promoted":true,"waiter_live":true}`), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !c.Promoted || c.Position != 1 {
		t.Errorf("decoded = %+v, want promoted:true at position 1", c)
	}
	raw, err := json.Marshal(StageConcurrency{Status: concurrencyStatusAwaitingSlot, Position: 2})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(raw), "promoted") {
		t.Errorf("encoded = %s, want promoted omitted when false", raw)
	}
}
