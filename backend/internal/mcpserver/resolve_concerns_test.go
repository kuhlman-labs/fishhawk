package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// resolveConcernsStub answers POST /v0/runs/{run_id}/concerns/resolve with
// status/body, recording each request line and body.
type resolveConcernsStub struct {
	requests []string
	bodies   []string
	status   int
	body     string
}

func (b *resolveConcernsStub) resolver(t *testing.T) *runResolver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		b.bodies = append(b.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return newResolver(ts, nil)
}

// TestResolveConcerns_PassesThrough: the tool POSTs the run-scoped route with
// the ids and evidence verbatim and returns the backend's per-item result.
func TestResolveConcerns_PassesThrough(t *testing.T) {
	runID := uuid.New()
	a, b := uuid.NewString(), uuid.NewString()
	stub := &resolveConcernsStub{body: `{"run_id":"` + runID.String() + `","evidence":"re-ran it","resolved":1,"failed":1,"results":[` +
		`{"concern_id":"` + a + `","applied":true,"state":"addressed","state_reason":"operator evidence: re-ran it"},` +
		`{"concern_id":"` + b + `","applied":false,"error_code":"concern_resolve_conflict","error":"raced"}]}`}
	r := stub.resolver(t)

	_, out, err := r.resolveConcerns(context.Background(), nil, ResolveConcernsInput{
		RunID: runID.String(), ConcernIDs: []string{a, b}, Evidence: "re-ran it",
	})
	if err != nil {
		t.Fatalf("resolveConcerns: %v", err)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "POST /v0/runs/"+runID.String()+"/concerns/resolve" {
		t.Fatalf("requests = %v, want one POST to the resolve route", stub.requests)
	}
	var sent resolveConcernsRequestBody
	if err := json.Unmarshal([]byte(stub.bodies[0]), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.Evidence != "re-ran it" || len(sent.ConcernIDs) != 2 || sent.ConcernIDs[0] != a || sent.ConcernIDs[1] != b {
		t.Errorf("sent body = %+v, want the ids and evidence verbatim", sent)
	}
	res := out.Result
	if res.Resolved != 1 || res.Failed != 1 || len(res.Results) != 2 {
		t.Fatalf("result = %+v, want 1 resolved / 1 failed", res)
	}
	if !res.Results[0].Applied || res.Results[0].State != "addressed" || res.Results[0].StateReason != "operator evidence: re-ran it" {
		t.Errorf("results[0] = %+v, want the applied addressed row", res.Results[0])
	}
	if res.Results[1].Applied || res.Results[1].ErrorCode != "concern_resolve_conflict" {
		t.Errorf("results[1] = %+v, want the failed item's error_code", res.Results[1])
	}
}

// TestResolveConcerns_LocalValidation: malformed input fails before the HTTP
// hop — the backend sees zero requests.
func TestResolveConcerns_LocalValidation(t *testing.T) {
	good := uuid.NewString()
	for _, tc := range []struct {
		name string
		in   ResolveConcernsInput
		want string
	}{
		{"bad run id", ResolveConcernsInput{RunID: "nope", ConcernIDs: []string{good}, Evidence: "e"}, "run_id"},
		{"empty ids", ResolveConcernsInput{RunID: uuid.NewString(), Evidence: "e"}, "at least one concern"},
		{"bad concern id", ResolveConcernsInput{RunID: uuid.NewString(), ConcernIDs: []string{good, "bad"}, Evidence: "e"}, `concern_ids entry "bad"`},
		{"blank evidence", ResolveConcernsInput{RunID: uuid.NewString(), ConcernIDs: []string{good}, Evidence: "  \n"}, "evidence is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &resolveConcernsStub{body: `{}`}
			r := stub.resolver(t)
			_, _, err := r.resolveConcerns(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(stub.requests) != 0 {
				t.Errorf("backend requests = %v, want 0 (local validation short-circuits)", stub.requests)
			}
		})
	}
}

// TestResolveConcerns_BackendRefusal_Surfaced: a backend refusal (here the
// human-only 403) surfaces as a tool error carrying the code.
func TestResolveConcerns_BackendRefusal_Surfaced(t *testing.T) {
	stub := &resolveConcernsStub{
		status: http.StatusForbidden,
		body:   `{"error":{"code":"resolve_requires_human","message":"an agent token cannot resolve it"}}`,
	}
	r := stub.resolver(t)
	_, _, err := r.resolveConcerns(context.Background(), nil, ResolveConcernsInput{
		RunID: uuid.NewString(), ConcernIDs: []string{uuid.NewString()}, Evidence: "e",
	})
	if err == nil || !strings.Contains(err.Error(), "resolve_requires_human") {
		t.Errorf("err = %v, want the backend refusal code surfaced", err)
	}
}
