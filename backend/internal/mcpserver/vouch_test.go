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

// vouchFullSHA is a well-formed 40-hex commit id for fixtures that must reach a
// branch AFTER the full-sha guard (#4435).
const vouchFullSHA = "891e084aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
		RunID: "not-a-uuid", SHA: vouchFullSHA, Reason: "x",
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
		RunID: uuid.NewString(), SHA: vouchFullSHA, Reason: "   ",
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
		RunID: uuid.NewString(), SHA: vouchFullSHA, Reason: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "run_token_forbidden") {
		t.Fatalf("err = %v, want run_token_forbidden", err)
	}
}

// TestVouchCommit_NonFullSHA_FailsLocally pins the #4435 fast fail: an
// abbreviated or otherwise non-full sha is refused locally, naming the
// full-sha requirement, and the backend is never called. The fake backend
// accepts ANY body with a 200, so with the guard deleted the tool would
// succeed and record one backend call — the zero-call assertion goes RED.
//
// COUNTERFACTUAL: deleting the `!isFullObjectName(sha)` guard in vouchCommit
// turns every subtest RED (no error, one backend call).
func TestVouchCommit_NonFullSHA_FailsLocally(t *testing.T) {
	cases := []struct{ name, sha string }{
		{"8-char prefix", vouchFullSHA[:8]},
		{"40-char non-hex", strings.Repeat("g", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newVouchFakeBackend(t)
			r := newResolver(srv, nil)

			_, _, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
				RunID: uuid.NewString(), SHA: tc.sha, Reason: "x",
			})
			if err == nil || !strings.Contains(err.Error(), "not a full commit id") {
				t.Fatalf("err = %v, want the full-sha refusal", err)
			}
			for _, want := range []string{"40 hex", "64", "git rev-parse"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err should name %q: %v", want, err)
				}
			}
			if len(fb.vouchCalledByID) != 0 {
				t.Errorf("backend vouch called %d times, want 0", len(fb.vouchCalledByID))
			}
		})
	}
}

// TestVouchCommit_FullSHA_ReachesBackend guards against over-tightening: a
// 64-hex (SHA-256) id and an upper-case 40-hex id both pass the local guard and
// reach the backend, which owns the lowercase canonicalization.
func TestVouchCommit_FullSHA_ReachesBackend(t *testing.T) {
	cases := []struct{ name, sha string }{
		{"64-hex", strings.Repeat("ab12", 16)},
		{"upper-case 40-hex", strings.ToUpper(vouchFullSHA)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newVouchFakeBackend(t)
			r := newResolver(srv, nil)
			runID := uuid.New()

			if _, _, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
				RunID: runID.String(), SHA: tc.sha, Reason: "x",
			}); err != nil {
				t.Fatalf("vouchCommit: %v", err)
			}
			if fb.vouchCalledByID[runID] != 1 {
				t.Errorf("vouch called %d times, want 1", fb.vouchCalledByID[runID])
			}
			if fb.vouchBody.SHA != tc.sha {
				t.Errorf("body sha = %q, want %q", fb.vouchBody.SHA, tc.sha)
			}
		})
	}
}

// TestFullObjectName_ShaAcceptanceParity runs the SAME inputs as the backend's
// TestVouchCommit_ShaAcceptanceParity (server/vouch_test.go) through this
// package's duplicated fast-fail predicate, so it cannot drift from the
// authoritative backend one (#4435). Keep the two tables identical.
func TestFullObjectName_ShaAcceptanceParity(t *testing.T) {
	cases := []struct {
		name string
		sha  string
		want bool
	}{
		{"7 hex", strings.Repeat("a", 7), false},
		{"8 hex", strings.Repeat("a", 8), false},
		{"39 hex", strings.Repeat("a", 39), false},
		{"40 hex", strings.Repeat("a", 40), true},
		{"41 hex", strings.Repeat("a", 41), false},
		{"63 hex", strings.Repeat("a", 63), false},
		{"64 hex", strings.Repeat("a", 64), true},
		{"40 non-hex", strings.Repeat("g", 40), false},
		{"40 upper-case hex", strings.Repeat("A", 40), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFullObjectName(tc.sha); got != tc.want {
				t.Errorf("isFullObjectName(%q) = %v, want %v", tc.sha, got, tc.want)
			}
		})
	}
}
