package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_retrigger_ci (E83.49 / #4082) ---

// retriggerFakeBackend serves only POST /v0/runs/{run_id}/retrigger-ci. status
// and body are written verbatim; calls counts requests per run id.
type retriggerFakeBackend struct {
	mu     sync.Mutex
	status int
	body   string
	calls  map[string]int
}

func newRetriggerFakeBackend(t *testing.T, status int, body string) (*retriggerFakeBackend, *httptest.Server) {
	t.Helper()
	fb := &retriggerFakeBackend{status: status, body: body, calls: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/retrigger-ci", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.calls[r.PathValue("run_id")]++
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.status)
		_, _ = w.Write([]byte(fb.body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestRetriggerCI_SuccessPassthrough(t *testing.T) {
	runID := uuid.New()
	fb, srv := newRetriggerFakeBackend(t, http.StatusOK, `{"run_id":"`+runID.String()+`","pr_url":"https://github.com/x/y/pull/7","head_sha":"aaa",
		"rerun":[{"id":11,"event":"pull_request","status":"completed","conclusion":"failure"}],
		"skipped":[{"id":14,"event":"workflow_dispatch","reason":"event_excluded"}],
		"failed":[{"id":12,"event":"push","error":"forbidden"}]}`)
	r := newResolver(srv, nil)

	_, out, err := r.retriggerCI(context.Background(), nil, retriggerCIInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("retriggerCI: %v", err)
	}
	if fb.calls[runID.String()] != 1 {
		t.Errorf("backend called %d times, want 1", fb.calls[runID.String()])
	}
	res := out.Result
	if res.RunID != runID.String() || res.PRURL != "https://github.com/x/y/pull/7" || res.HeadSHA != "aaa" {
		t.Errorf("result header = %+v", res)
	}
	if len(res.Rerun) != 1 || res.Rerun[0].ID != 11 || res.Rerun[0].Conclusion != "failure" {
		t.Errorf("rerun = %+v", res.Rerun)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != "event_excluded" {
		t.Errorf("skipped = %+v", res.Skipped)
	}
	if len(res.Failed) != 1 || res.Failed[0].Error != "forbidden" {
		t.Errorf("failed = %+v", res.Failed)
	}
}

// TestRetriggerCI_ConflictSurfacedVerbatim: a backend 409 reaches the caller
// with its error code and message intact.
func TestRetriggerCI_ConflictSurfacedVerbatim(t *testing.T) {
	_, srv := newRetriggerFakeBackend(t, http.StatusConflict,
		`{"error":{"code":"pull_request_not_open","message":"the run's pull request is not open; never close a run's PR"}}`)
	r := newResolver(srv, nil)

	_, _, err := r.retriggerCI(context.Background(), nil, retriggerCIInput{RunID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want the backend 409")
	}
	if !strings.Contains(err.Error(), "pull_request_not_open") || !strings.Contains(err.Error(), "never close a run's PR") {
		t.Errorf("err = %v, want the code and message surfaced verbatim", err)
	}
}

func TestRetriggerCI_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newRetriggerFakeBackend(t, http.StatusOK, `{}`)
	r := newResolver(srv, nil)

	_, _, err := r.retriggerCI(context.Background(), nil, retriggerCIInput{RunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID parse error", err)
	}
	if len(fb.calls) != 0 {
		t.Errorf("backend called %d times, want 0", len(fb.calls))
	}
}
