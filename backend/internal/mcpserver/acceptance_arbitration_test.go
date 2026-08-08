package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_arbitrate_acceptance (E66.37 / #2474) ---

// arbitrateFakeBackend is a self-contained backend stub for the
// arbitrate-acceptance tool: it serves only
// POST /v0/runs/{run_id}/acceptance-arbitration. body captures the last decoded
// request so tests assert reason/acknowledgement threading; status drives the
// HTTP status (default 200); errBody, when set, is written verbatim for the
// error-path tests; calledByID counts calls per run id.
type arbitrateFakeBackend struct {
	mu         sync.Mutex
	body       acceptanceArbitrationRequest
	resp       map[uuid.UUID]ArbitrateAcceptanceResult
	status     int
	errBody    string
	calledByID map[uuid.UUID]int
}

func newArbitrateFakeBackend(t *testing.T) (*arbitrateFakeBackend, *httptest.Server) {
	fb := &arbitrateFakeBackend{
		resp:       map[uuid.UUID]ArbitrateAcceptanceResult{},
		status:     http.StatusOK,
		calledByID: map[uuid.UUID]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/acceptance-arbitration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body acceptanceArbitrationRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.calledByID[id]++
		fb.body = body
		status := fb.status
		errBody := fb.errBody
		resp, ok := fb.resp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = ArbitrateAcceptanceResult{
				RunID:               id.String(),
				AcceptanceGateState: "acceptance_arbitrated",
				OutcomeSequence:     60,
				ArbitrationSequence: 62,
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestArbitrateAcceptance_HappyPath_ThreadsReasonAndAcknowledgement(t *testing.T) {
	fb, srv := newArbitrateFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	_, out, err := r.arbitrateAcceptance(context.Background(), nil, ArbitrateAcceptanceInput{
		RunID:                     runID.String(),
		Reason:                    "  every criterion is externally unvalidatable  ",
		AcknowledgeFailedCriteria: true,
	})
	if err != nil {
		t.Fatalf("arbitrateAcceptance: %v", err)
	}
	if out.Result.AcceptanceGateState != "acceptance_arbitrated" {
		t.Errorf("acceptance_gate_state = %q", out.Result.AcceptanceGateState)
	}
	if out.Result.OutcomeSequence != 60 {
		t.Errorf("outcome_sequence = %d, want 60 (the binding surfaced back to the operator)", out.Result.OutcomeSequence)
	}
	if fb.calledByID[runID] != 1 {
		t.Errorf("backend called %d times, want 1", fb.calledByID[runID])
	}
	if fb.body.Reason != "every criterion is externally unvalidatable" {
		t.Errorf("body reason = %q, want the trimmed reason", fb.body.Reason)
	}
	if !fb.body.AcknowledgeFailedCriteria {
		t.Error("body acknowledge_failed_criteria = false, want true")
	}
}

// TestArbitrateAcceptance_AcknowledgementDefaultsFalse: the flag is a deliberate
// operator act, so it must never be sent true by default — the backend's
// requires_acknowledgement guard depends on the client not fabricating it.
func TestArbitrateAcceptance_AcknowledgementDefaultsFalse(t *testing.T) {
	fb, srv := newArbitrateFakeBackend(t)
	r := newResolver(srv, nil)

	if _, _, err := r.arbitrateAcceptance(context.Background(), nil, ArbitrateAcceptanceInput{
		RunID: uuid.New().String(), Reason: "class-5 all-skip",
	}); err != nil {
		t.Fatalf("arbitrateAcceptance: %v", err)
	}
	if fb.body.AcknowledgeFailedCriteria {
		t.Error("acknowledge_failed_criteria = true without the operator setting it")
	}
}

func TestArbitrateAcceptance_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newArbitrateFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.arbitrateAcceptance(context.Background(), nil, ArbitrateAcceptanceInput{
		RunID: "not-a-uuid", Reason: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID parse error", err)
	}
	if len(fb.calledByID) != 0 {
		t.Errorf("backend called %d times, want 0 (local validation precedes the HTTP hop)", len(fb.calledByID))
	}
}

func TestArbitrateAcceptance_EmptyReason_FailsLocally(t *testing.T) {
	fb, srv := newArbitrateFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.arbitrateAcceptance(context.Background(), nil, ArbitrateAcceptanceInput{
		RunID: uuid.New().String(), Reason: "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("err = %v, want the empty-reason error", err)
	}
	if len(fb.calledByID) != 0 {
		t.Errorf("backend called %d times, want 0", len(fb.calledByID))
	}
}

// TestArbitrateAcceptance_BackendErrorsSurface: every 409 the backend's guards
// can return must reach the operator with its code intact — a swallowed or
// re-labelled refusal would leave them guessing which guard fired.
func TestArbitrateAcceptance_BackendErrorsSurface(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"not applicable", http.StatusConflict, "acceptance_arbitration_not_applicable"},
		{"requires acknowledgement", http.StatusConflict, "acceptance_arbitration_requires_acknowledgement"},
		{"outcome superseded", http.StatusConflict, "acceptance_outcome_superseded"},
		{"run token forbidden", http.StatusForbidden, "run_token_forbidden"},
		{"insufficient scope", http.StatusForbidden, "insufficient_scope"},
		{"unconfigured", http.StatusServiceUnavailable, "acceptance_arbitration_unconfigured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newArbitrateFakeBackend(t)
			fb.status = tc.status
			fb.errBody = `{"error":{"code":"` + tc.code + `","message":"refused"}}`
			r := newResolver(srv, nil)

			_, _, err := r.arbitrateAcceptance(context.Background(), nil, ArbitrateAcceptanceInput{
				RunID: uuid.New().String(), Reason: "x",
			})
			if err == nil {
				t.Fatal("err = nil, want the backend refusal surfaced")
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("err = %v, want it to name %q", err, tc.code)
			}
		})
	}
}
