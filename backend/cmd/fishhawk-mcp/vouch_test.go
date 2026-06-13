package main

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

// --- fishhawk_vouch_commit (ADR-035 remediation, #1044) ---

// vouchFakeBackend is a self-contained backend stub for the vouch-commit
// tool: it serves only POST /v0/runs/{run_id}/vouch-commit. vouchBody
// captures the last decoded body so tests assert sha/reason threading.
// vouchStatus drives the HTTP status (default 200); vouchErrBody, when set,
// is written verbatim for the error-path tests. vouchCalledByID counts
// calls per run id.
type vouchFakeBackend struct {
	mu              sync.Mutex
	vouchBody       vouchCommitRequest
	vouchResp       map[uuid.UUID]VouchCommitResult
	vouchStatus     int
	vouchErrBody    string
	vouchCalledByID map[uuid.UUID]int
}

func newVouchFakeBackend(t *testing.T) (*vouchFakeBackend, *httptest.Server) {
	fb := &vouchFakeBackend{
		vouchResp:       map[uuid.UUID]VouchCommitResult{},
		vouchStatus:     http.StatusOK,
		vouchCalledByID: map[uuid.UUID]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/vouch-commit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body vouchCommitRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.vouchCalledByID[id]++
		fb.vouchBody = body
		status := fb.vouchStatus
		errBody := fb.vouchErrBody
		resp, ok := fb.vouchResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = VouchCommitResult{RunID: id.String(), VouchedSHA: body.SHA, Reason: body.Reason}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestVouchCommit_HappyPath_ThreadsSHAAndReason(t *testing.T) {
	fb, srv := newVouchFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	_, out, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
		RunID:  runID.String(),
		SHA:    "891e084aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Reason: "sync-schemas remediation",
	})
	if err != nil {
		t.Fatalf("vouchCommit: %v", err)
	}
	if out.Result.VouchedSHA != "891e084aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("VouchedSHA = %q", out.Result.VouchedSHA)
	}
	if fb.vouchCalledByID[runID] != 1 {
		t.Errorf("vouch called %d times, want 1", fb.vouchCalledByID[runID])
	}
	if fb.vouchBody.SHA != "891e084aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("body sha = %q", fb.vouchBody.SHA)
	}
	if fb.vouchBody.Reason != "sync-schemas remediation" {
		t.Errorf("body reason = %q", fb.vouchBody.Reason)
	}
}

func TestVouchCommit_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newVouchFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
		RunID: "not-a-uuid", SHA: "abc", Reason: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID parse error", err)
	}
	if len(fb.vouchCalledByID) != 0 {
		t.Errorf("backend vouch called %d times, want 0", len(fb.vouchCalledByID))
	}
}

func TestVouchCommit_EmptySHA_FailsLocally(t *testing.T) {
	fb, srv := newVouchFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
		RunID: uuid.NewString(), SHA: "  ", Reason: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "sha is required") {
		t.Fatalf("err = %v, want sha-required error", err)
	}
	if len(fb.vouchCalledByID) != 0 {
		t.Errorf("backend vouch called %d times, want 0", len(fb.vouchCalledByID))
	}
}

func TestVouchCommit_EmptyReason_FailsLocally(t *testing.T) {
	fb, srv := newVouchFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
		RunID: uuid.NewString(), SHA: "abc", Reason: "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("err = %v, want reason-required error", err)
	}
	if len(fb.vouchCalledByID) != 0 {
		t.Errorf("backend vouch called %d times, want 0", len(fb.vouchCalledByID))
	}
}

func TestVouchCommit_RunTokenForbidden_PropagatesAs403(t *testing.T) {
	fb, srv := newVouchFakeBackend(t)
	fb.vouchStatus = http.StatusForbidden
	fb.vouchErrBody = `{"error":{"code":"run_token_forbidden","message":"a run-bound agent token may not vouch a commit"}}`
	r := newResolver(srv, nil)

	_, _, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
		RunID: uuid.NewString(), SHA: "abc", Reason: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "run_token_forbidden") {
		t.Fatalf("err = %v, want run_token_forbidden", err)
	}
}
