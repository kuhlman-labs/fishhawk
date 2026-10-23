package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// gateViewNote96Plus is deliberately longer than the compaction levers'
// 96-byte auditPayloadStringCap so a byte-identical round-trip through the
// client -> tool -> output seam proves none of compact.go's levers apply to
// this surface.
const gateViewNote96Plus = "The reviewer's full concern prose is intentionally longer than ninety-six bytes so any truncation or elision on the new gate-view surface would visibly alter the round-tripped note here."

// gateViewProbe records what the fake backend saw so a test can assert the
// stage_kind query forwarded correctly.
type gateViewProbe struct {
	mu            sync.Mutex
	calls         int
	lastRunID     string
	lastStageKind string
}

// newGateViewBackend serves GET /v0/runs/{run_id}/gate-view with the given
// status + payload, recording the request into the returned probe.
func newGateViewBackend(t *testing.T, status int, payload any) (*httptest.Server, *gateViewProbe) {
	t.Helper()
	probe := &gateViewProbe{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/gate-view", func(w http.ResponseWriter, r *http.Request) {
		probe.mu.Lock()
		probe.calls++
		probe.lastRunID = r.PathValue("run_id")
		probe.lastStageKind = r.URL.Query().Get("stage_kind")
		probe.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, probe
}

// callGateView drives fishhawk_get_gate_view end-to-end through a real MCP
// CallTool over an in-memory transport against srv.
func callGateView(t *testing.T, srv *httptest.Server, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	resolver := newResolver(srv, nil)

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerGetGateView(server, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_gate_view",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

// TestGetGateView_FullNoteByteIdentical (binding condition 2) drives the tool
// end-to-end and asserts a >96-byte concern note arrives byte-identical through
// the client -> tool -> output seam, proving stripReviewProse and the 96-byte
// auditPayloadStringCap do NOT apply to this surface.
func TestGetGateView_FullNoteByteIdentical(t *testing.T) {
	runID := uuid.New()
	payload := GateView{
		RunID: runID.String(),
		Open: []GateViewConcern{{
			ID:                   uuid.New().String(),
			StageKind:            "implement",
			Round:                2,
			OriginReviewSequence: 30,
			ReviewerModel:        "claude-opus-4-8",
			Severity:             "high",
			Category:             "correctness",
			State:                "reopened",
			Note:                 gateViewNote96Plus,
			Fixups: []GateViewFixup{{
				Sequence: 20, Reason: "route it back", Outcome: "pushed", ApplyPath: "applied", HeadSHA: "abc123",
			}},
		}},
		Settled:                 []GateViewSettledConcern{},
		SuppressedRelitigations: []GateViewSuppressedRelitig{},
	}
	srv, _ := newGateViewBackend(t, http.StatusOK, payload)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent is nil; the typed output did not serialize")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	// Byte-identical proof at the wire: the full note is present verbatim.
	if !strings.Contains(string(raw), gateViewNote96Plus) {
		t.Fatalf("full note did not survive the seam byte-identical:\n%s", string(raw))
	}
	var out GetGateViewOutput
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode GetGateViewOutput from wire: %v", uerr)
	}
	if out.GateView == nil || len(out.GateView.Open) != 1 {
		t.Fatalf("gate_view.open did not round-trip: %+v", out.GateView)
	}
	if got := out.GateView.Open[0].Note; got != gateViewNote96Plus {
		t.Errorf("note = %q, want the full %d-byte prose byte-identical", got, len(gateViewNote96Plus))
	}
	// The fix-up join fields survive too (full decision context, not just the note).
	fx := out.GateView.Open[0].Fixups
	if len(fx) != 1 || fx[0].Outcome != "pushed" || fx[0].ApplyPath != "applied" || fx[0].HeadSHA != "abc123" {
		t.Errorf("fixup join did not round-trip: %+v", fx)
	}
}

// TestGetGateView_StageKindForwarding asserts the optional stage_kind reaches
// the backend as a query parameter.
func TestGetGateView_StageKindForwarding(t *testing.T) {
	runID := uuid.New()
	srv, probe := newGateViewBackend(t, http.StatusOK, GateView{
		RunID:                   runID.String(),
		Open:                    []GateViewConcern{},
		Settled:                 []GateViewSettledConcern{},
		SuppressedRelitigations: []GateViewSuppressedRelitig{},
	})

	res := callGateView(t, srv, map[string]any{"run_id": runID.String(), "stage_kind": "implement"})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.lastStageKind != "implement" {
		t.Errorf("forwarded stage_kind = %q, want implement", probe.lastStageKind)
	}
	if probe.lastRunID != runID.String() {
		t.Errorf("forwarded run_id = %q, want %q", probe.lastRunID, runID.String())
	}
}

// TestGetGateView_ErrorMapping asserts a backend 404 / 503 surfaces as a tool
// error rather than a bogus empty success.
func TestGetGateView_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
	}{
		{"not found", http.StatusNotFound, "run_not_found"},
		{"unconfigured", http.StatusServiceUnavailable, "gate_view_unconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			envelope := map[string]any{"error": map[string]any{"code": tc.code, "message": tc.code}}
			srv, _ := newGateViewBackend(t, tc.status, envelope)

			res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
			if !res.IsError {
				t.Fatalf("CallTool should surface a tool error for HTTP %d; got success: %+v", tc.status, res.StructuredContent)
			}
			var sb strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					sb.WriteString(tc.Text)
				}
			}
			if !strings.Contains(sb.String(), tc.code) {
				t.Errorf("tool error should mention %q; got %q", tc.code, sb.String())
			}
		})
	}
}

// TestGetGateView_InvalidStageKind rejects a bad stage_kind locally before any
// backend round-trip.
func TestGetGateView_InvalidStageKind(t *testing.T) {
	runID := uuid.New()
	srv, probe := newGateViewBackend(t, http.StatusOK, GateView{})

	res := callGateView(t, srv, map[string]any{"run_id": runID.String(), "stage_kind": "deploy"})
	if !res.IsError {
		t.Fatalf("CallTool should reject an invalid stage_kind locally; got success")
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.calls != 0 {
		t.Errorf("backend was called %d times; a bad stage_kind must fail before the round-trip", probe.calls)
	}
}

// TestGetGateView_LiveValidationRoundTrips (#2045, E48.35) drives the tool
// end-to-end over the in-memory MCP transport and asserts the live_validation
// block survives the client -> tool -> output seam. Two shapes: a healthy linked
// walk (walk_ref present) and a stranded intent-only marker (filing_failed +
// filing_incomplete, no walk_ref). A backend that omits the block leaves the
// mirrored field nil (the mixed-version degrade), asserted in the third case.
func TestGetGateView_LiveValidationRoundTrips(t *testing.T) {
	newPayload := func(runID uuid.UUID, lv *RunLiveValidation) GateView {
		return GateView{
			RunID:                   runID.String(),
			Open:                    []GateViewConcern{},
			Settled:                 []GateViewSettledConcern{},
			SuppressedRelitigations: []GateViewSuppressedRelitig{},
			LiveValidation:          lv,
		}
	}

	decode := func(t *testing.T, res *mcp.CallToolResult) *GateView {
		t.Helper()
		if res.IsError {
			t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
		}
		if res.StructuredContent == nil {
			t.Fatal("StructuredContent is nil; the typed output did not serialize")
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal StructuredContent: %v", err)
		}
		var out GetGateViewOutput
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			t.Fatalf("decode GetGateViewOutput from wire: %v", uerr)
		}
		if out.GateView == nil {
			t.Fatal("gate_view did not round-trip")
		}
		return out.GateView
	}

	t.Run("healthy linked walk survives the seam", func(t *testing.T) {
		runID := uuid.New()
		srv, _ := newGateViewBackend(t, http.StatusOK, newPayload(runID, &RunLiveValidation{
			PendingCriteriaCount: 3, WalkRef: "#123", FilingFailed: false,
		}))
		gv := decode(t, callGateView(t, srv, map[string]any{"run_id": runID.String()}))
		if gv.LiveValidation == nil {
			t.Fatal("gate_view.live_validation did not round-trip through the seam")
		}
		if lv := gv.LiveValidation; lv.PendingCriteriaCount != 3 || lv.WalkRef != "#123" || lv.FilingFailed || lv.FilingIncomplete {
			t.Errorf("live_validation = %+v, want {3 #123 false false}", *lv)
		}
	})

	t.Run("stranded intent-only marker survives the seam", func(t *testing.T) {
		runID := uuid.New()
		srv, _ := newGateViewBackend(t, http.StatusOK, newPayload(runID, &RunLiveValidation{
			PendingCriteriaCount: 1, FilingFailed: true, FilingIncomplete: true,
		}))
		gv := decode(t, callGateView(t, srv, map[string]any{"run_id": runID.String()}))
		if gv.LiveValidation == nil {
			t.Fatal("gate_view.live_validation did not round-trip through the seam")
		}
		if lv := gv.LiveValidation; lv.PendingCriteriaCount != 1 || lv.WalkRef != "" || !lv.FilingFailed || !lv.FilingIncomplete {
			t.Errorf("live_validation = %+v, want {1 \"\" true true}", *lv)
		}
	})

	t.Run("omitted block leaves the mirror nil", func(t *testing.T) {
		runID := uuid.New()
		srv, _ := newGateViewBackend(t, http.StatusOK, newPayload(runID, nil))
		gv := decode(t, callGateView(t, srv, map[string]any{"run_id": runID.String()}))
		if gv.LiveValidation != nil {
			t.Errorf("gate_view.live_validation = %+v, want nil when the backend omits it", gv.LiveValidation)
		}
	})
}

// TestGetGateView_ConcernEvidenceWireDecode pins the cross-boundary wire seam
// for #2353: the client's GateViewConcern / GateViewSettledConcern json tags
// must byte-match the server's gateViewConcern / gateViewSettledConcern. A
// mismatched tag is silent — it yields an empty field, not a decode error — so
// the backend body here is RAW JSON with the server's exact key spelling rather
// than a marshalled client struct, which would make a typo self-cancelling.
func TestGetGateView_ConcernEvidenceWireDecode(t *testing.T) {
	runID := uuid.New()
	const openEvidence = "the concern row is minted without new_evidence; see backend/internal/server/trace.go:4557"
	const settledEvidence = "waived on the strength of this evidence, which the ledger then dropped"
	openRef := uuid.New().String()
	settledRef := uuid.New().String()

	var body any
	if err := json.Unmarshal([]byte(`{
	  "run_id": "`+runID.String()+`",
	  "open": [{
	    "id": "`+uuid.New().String()+`",
	    "stage_kind": "implement",
	    "severity": "high",
	    "category": "correctness",
	    "state": "raised",
	    "note": "`+gateViewNote96Plus+`",
	    "new_evidence": "`+openEvidence+`",
	    "settled_ref": "`+openRef+`",
	    "has_suggested_patch": false
	  }],
	  "settled": [{
	    "id": "`+uuid.New().String()+`",
	    "stage_kind": "implement",
	    "state": "waived",
	    "severity": "medium",
	    "category": "scope",
	    "note": "settled row",
	    "new_evidence": "`+settledEvidence+`",
	    "settled_ref": "`+settledRef+`"
	  }],
	  "suppressed_relitigations": [],
	  "history_incomplete": false
	}`), &body); err != nil {
		t.Fatalf("build backend body: %v", err)
	}
	srv, _ := newGateViewBackend(t, http.StatusOK, body)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out GetGateViewOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tool output: %v\n%s", err, raw)
	}
	if len(out.GateView.Open) != 1 || len(out.GateView.Settled) != 1 {
		t.Fatalf("open=%d settled=%d, want 1 each", len(out.GateView.Open), len(out.GateView.Settled))
	}
	if got := out.GateView.Open[0].NewEvidence; got != openEvidence {
		t.Errorf("open concern new_evidence = %q, want %q verbatim", got, openEvidence)
	}
	if got := out.GateView.Open[0].SettledRef; got != openRef {
		t.Errorf("open concern settled_ref = %q, want %q", got, openRef)
	}
	if got := out.GateView.Settled[0].NewEvidence; got != settledEvidence {
		t.Errorf("settled concern new_evidence = %q, want %q verbatim", got, settledEvidence)
	}
	if got := out.GateView.Settled[0].SettledRef; got != settledRef {
		t.Errorf("settled concern settled_ref = %q, want %q", got, settledRef)
	}
}

// TestGetGateView_DisputesDecodeFromBackendShape is the json-tag byte-match
// guard for the dispute fields (E48.103 / #2551): the payload is authored as
// RAW backend-shaped JSON (the server's key names, not this package's Go
// struct), so a tag typo on GateViewConcern.Disputed / .Disputes or on any
// gateViewDispute field decodes to a zero value and reddens this test.
func TestGetGateView_DisputesDecodeFromBackendShape(t *testing.T) {
	runID := uuid.New()
	concernID := uuid.New().String()
	raw := json.RawMessage(`{
      "run_id": "` + runID.String() + `",
      "open": [{
        "id": "` + concernID + `",
        "stage_kind": "implement",
        "origin_review_sequence": 30,
        "reviewer_model": "gpt-5.6-sol",
        "severity": "high",
        "category": "authz",
        "state": "addressed_pending",
        "note": "` + gateViewNote96Plus + `",
        "has_suggested_patch": false,
        "disputed": true,
        "disputes": [{
          "sequence": 41,
          "round": 2,
          "veto_reason": "raiser_rejected_same_round",
          "resolution": "confirmed",
          "confirming_reviewer_model": "fable-5",
          "raising_reviewer_model": "gpt-5.6-sol",
          "note": "reads fixed to me"
        }]
      }],
      "settled": [],
      "suppressed_relitigations": [],
      "history_incomplete": false
    }`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	if decoded.GateView == nil || len(decoded.GateView.Open) != 1 {
		t.Fatalf("open did not round-trip: %+v", decoded.GateView)
	}
	got := decoded.GateView.Open[0]
	if !got.Disputed {
		t.Error("disputed = false, want true (a json-tag mismatch silently zeroes it)")
	}
	if len(got.Disputes) != 1 {
		t.Fatalf("disputes = %+v, want exactly one", got.Disputes)
	}
	d := got.Disputes[0]
	if d.VetoReason != "raiser_rejected_same_round" || d.ConfirmingReviewerModel != "fable-5" ||
		d.RaisingReviewerModel != "gpt-5.6-sol" || d.Resolution != "confirmed" ||
		d.Sequence != 41 || d.Round != 2 || d.Note != "reads fixed to me" {
		t.Errorf("dispute = %+v, want every field decoded from the backend-shaped payload", d)
	}
}

// TestGetGateView_NoDisputes_RendersFalse is the negative control: a
// backend-shaped payload with no dispute keys decodes to disputed=false with
// no disputes, so the assertion above discriminates.
func TestGetGateView_NoDisputes_RendersFalse(t *testing.T) {
	runID := uuid.New()
	raw := json.RawMessage(`{
      "run_id": "` + runID.String() + `",
      "open": [{
        "id": "` + uuid.New().String() + `",
        "stage_kind": "implement",
        "origin_review_sequence": 30,
        "severity": "high",
        "category": "authz",
        "state": "raised",
        "note": "plain open concern",
        "has_suggested_patch": false,
        "disputed": false
      }],
      "settled": [],
      "suppressed_relitigations": [],
      "history_incomplete": false
    }`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	got := decoded.GateView.Open[0]
	if got.Disputed || len(got.Disputes) != 0 {
		t.Errorf("disputed = %v disputes = %+v, want a clean undisputed render", got.Disputed, got.Disputes)
	}
}

// TestGetGateView_ReviewDiffTruncated_PassesThrough is the json-tag byte-match
// guard for the #2875 review_diff_truncated field: the backend body is RAW
// server-shaped JSON (the server's exact key spelling), so a tag typo on
// GateView.ReviewDiffTruncated or any gateViewReviewDiffTruncated field decodes
// to a zero value and reddens this test — the client returns it unmodified.
func TestGetGateView_ReviewDiffTruncated_PassesThrough(t *testing.T) {
	runID := uuid.New()
	raw := json.RawMessage(`{
      "run_id": "` + runID.String() + `",
      "open": [],
      "settled": [],
      "suppressed_relitigations": [],
      "history_incomplete": false,
      "review_diff_truncated": {
        "reason": "runner_patch_cap",
        "changed_file_count": 210,
        "omitted_file_count": 205,
        "omitted_files": ["pkg/one.go (no hunks shown)", "pkg/two.go (may be cut — its tail may be missing)"],
        "omitted_files_residual": 5,
        "delta_re_review": true,
        "best_effort": true
      }
    }`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	rdt := decoded.GateView.ReviewDiffTruncated
	if rdt == nil {
		t.Fatal("review_diff_truncated did not decode through the seam (json tag mismatch?)")
	}
	if rdt.Reason != "runner_patch_cap" || rdt.ChangedFileCount != 210 || rdt.OmittedFileCount != 205 {
		t.Errorf("review_diff_truncated scalars = %+v, want reason/counts intact", rdt)
	}
	if len(rdt.OmittedFiles) != 2 || rdt.OmittedFiles[0] != "pkg/one.go (no hunks shown)" {
		t.Errorf("omitted_files did not pass through: %+v", rdt.OmittedFiles)
	}
	if rdt.OmittedFilesResidual != 5 || !rdt.DeltaReReview || !rdt.BestEffort {
		t.Errorf("residual/delta/best_effort did not pass through: %+v", rdt)
	}
}

// TestGetGateView_ReviewDiffTruncated_OmittedLeavesMirrorNil: a backend body that
// omits the block leaves the client mirror nil (the mixed-version degrade).
func TestGetGateView_ReviewDiffTruncated_OmittedLeavesMirrorNil(t *testing.T) {
	runID := uuid.New()
	raw := json.RawMessage(`{
      "run_id": "` + runID.String() + `",
      "open": [],
      "settled": [],
      "suppressed_relitigations": [],
      "history_incomplete": false
    }`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	if decoded.GateView.ReviewDiffTruncated != nil {
		t.Errorf("review_diff_truncated = %+v, want nil when the backend omits it", decoded.GateView.ReviewDiffTruncated)
	}
}

// TestGetGateView_PrecedentPassesThrough (E75.4 / #3732): the gate view's
// precedent block decodes through the MCP seam with every field intact —
// including the reason excerpt, since this surface applies none of the
// compaction levers — and names the full query.
func TestGetGateView_PrecedentPassesThrough(t *testing.T) {
	runID := uuid.New()
	raw := json.RawMessage(`{"run_id":"` + runID.String() + `","open":[],"settled":[],` +
		`"suppressed_relitigations":[],"history_incomplete":false,"precedent":` + drivePrecedentBlock + `}`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	p := decoded.GateView.Precedent
	if p == nil {
		t.Fatal("precedent did not decode through the seam (json tag mismatch?)")
	}
	if p.DecisionClass != "plan_approval" || p.IndexVersion != "precedent-rank-v1" || p.Fingerprint != "fp1" {
		t.Errorf("precedent scalars = %+v, want class/index_version/fingerprint intact", p)
	}
	if len(p.Items) != 1 || p.Items[0].SourceEntryHash != "h41" || p.Items[0].ReasonExcerpt != "looks right" ||
		p.Items[0].Score.Total != 0.5 {
		t.Errorf("precedent items = %+v, want the cited item with its excerpt and score", p.Items)
	}
	if p.Summary.ModalOutcome != "approved" || p.FullQuery.Tool != "fishhawk_precedent" || p.FullQuery.Repo != "x/y" {
		t.Errorf("precedent summary/full_query = %+v / %+v", p.Summary, p.FullQuery)
	}
}

// TestGetGateView_PrecedentOmittedLeavesMirrorNil: no block on the wire (no
// open gate, nothing indexed, an older backend) leaves the mirror nil.
func TestGetGateView_PrecedentOmittedLeavesMirrorNil(t *testing.T) {
	runID := uuid.New()
	raw := json.RawMessage(`{"run_id":"` + runID.String() + `","open":[],"settled":[],` +
		`"suppressed_relitigations":[],"history_incomplete":false}`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)
	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	if decoded.GateView.Precedent != nil {
		t.Errorf("precedent = %+v, want nil when the backend omits it", decoded.GateView.Precedent)
	}
}

// TestGetGateView_ConsultsPassThrough (E77.8 / #3742): the consults block
// survives the MCP seam intact — the tool applies no compaction lever to it,
// so the captain reads the same cited entry ids the backend reported.
func TestGetGateView_ConsultsPassThrough(t *testing.T) {
	runID := uuid.New()
	raw := json.RawMessage(`{"run_id":"` + runID.String() + `","open":[],"settled":[],` +
		`"suppressed_relitigations":[],"history_incomplete":false,"consults":[` +
		`{"sent_sequence":77,"stage_kind":"plan","sender_role":"planner","recipient_role":"historian",` +
		`"state":"accepted","answered":true,"question":"has this been decided?",` +
		`"answer_summary":"Prior decisions in x/y: plan_approval 1","cited_entry_refs":["audit_entry:9"],` +
		`"asked_at":"2026-09-30T12:00:00Z"}]}`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	cs := decoded.GateView.Consults
	if len(cs) != 1 {
		t.Fatalf("consults = %+v, want one through the seam (json tag mismatch?)", cs)
	}
	if cs[0].SentSequence != 77 || cs[0].RecipientRole != "historian" || !cs[0].Answered {
		t.Errorf("consult = %+v, want the historian consult intact", cs[0])
	}
	if len(cs[0].CitedEntryRefs) != 1 || cs[0].CitedEntryRefs[0] != "audit_entry:9" {
		t.Errorf("cited_entry_refs = %v, want the backend's list verbatim", cs[0].CitedEntryRefs)
	}
	if cs[0].Question != "has this been decided?" {
		t.Errorf("question = %q, want it uncompacted", cs[0].Question)
	}
}

// TestGetGateView_MergeReadinessAndResolutionBasisDecode: both #4086 gate-view
// additions survive the MCP seam from a backend-shaped body — the
// merge_readiness block (blockers with details, undetermined) and the settled
// row's resolution_basis — and an older backend's body leaves both empty.
func TestGetGateView_MergeReadinessAndResolutionBasisDecode(t *testing.T) {
	runID := uuid.New()
	raw := json.RawMessage(`{"run_id":"` + runID.String() + `","open":[],"suppressed_relitigations":[],"history_incomplete":false,` +
		`"settled":[{"id":"` + uuid.NewString() + `","stage_kind":"implement","state":"addressed","severity":"high","category":"correctness",` +
		`"note":"n","state_reason":"operator evidence: re-ran it","resolution_basis":"operator_evidence"}],` +
		`"merge_readiness":{"blockers":[{"code":"approval_dismissed","message":"no approval is live",` +
		`"details":{"dismissing_commit":"cccc","dismissing_cause":"vouch_commit","next_step":"approve_pr"}}],` +
		`"undetermined":["acceptance staleness: the acceptance history could not be read"]}}`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)
	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	mr := decoded.GateView.MergeReadiness
	if mr == nil || len(mr.Blockers) != 1 || len(mr.Undetermined) != 1 {
		t.Fatalf("merge_readiness = %+v, want one blocker and one undetermined through the seam", mr)
	}
	b := mr.Blockers[0]
	if b.Code != "approval_dismissed" || b.Message != "no approval is live" ||
		b.Details["dismissing_cause"] != "vouch_commit" || b.Details["next_step"] != "approve_pr" {
		t.Errorf("blocker = %+v, want the backend's code/message/details verbatim", b)
	}
	if len(decoded.GateView.Settled) != 1 || decoded.GateView.Settled[0].ResolutionBasis != "operator_evidence" {
		t.Errorf("settled = %+v, want resolution_basis operator_evidence", decoded.GateView.Settled)
	}

	older := json.RawMessage(`{"run_id":"` + runID.String() + `","open":[],"settled":[{"id":"` + uuid.NewString() +
		`","stage_kind":"implement","state":"addressed","severity":"low","category":"style","note":"n"}],` +
		`"suppressed_relitigations":[],"history_incomplete":false}`)
	srv2, _ := newGateViewBackend(t, http.StatusOK, older)
	res2 := callGateView(t, srv2, map[string]any{"run_id": runID.String()})
	out2, _ := json.Marshal(res2.StructuredContent)
	var decoded2 GetGateViewOutput
	if uerr := json.Unmarshal(out2, &decoded2); uerr != nil {
		t.Fatalf("decode older body: %v", uerr)
	}
	if decoded2.GateView.MergeReadiness != nil || decoded2.GateView.Settled[0].ResolutionBasis != "" {
		t.Errorf("older backend: merge_readiness=%+v basis=%q, want nil / empty", decoded2.GateView.MergeReadiness, decoded2.GateView.Settled[0].ResolutionBasis)
	}
}

// TestGateViewMergeReadiness_WireShape pins the hand-maintained
// gateViewMergeReadiness / gateViewMergeBlocker mirrors and the
// GateViewSettledConcern.ResolutionBasis field against the REAL backend
// (#4086, the #371 trap): a run with a pull request and a re-opened acceptance
// stage on the REAL chain, and a concern resolved through the REAL
// POST /concerns/resolve route. (a) Every key the SERVER emits in the
// merge_readiness block and its blocker is a mirror field; (b) the block
// decodes through GetGateView with the acceptance_stale blocker and the
// no-GitHub-client undetermined reason; (c) the resolved concern decodes as
// addressed with resolution_basis operator_evidence.
func TestGateViewMergeReadiness_WireShape(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	concernRepo := concern.NewPostgresRepository(pool)

	row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.SetRunPullRequestURL(ctx, row.ID, "https://github.com/x/y/pull/7"); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	acc, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: row.ID, Sequence: 2, Type: runpkg.StageTypeAcceptance, ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "acceptance",
	})
	if err != nil {
		t.Fatalf("create acceptance stage: %v", err)
	}
	const h1, h2 = "aaaa000000000000000000000000000000000001", "bbbb000000000000000000000000000000000002"
	for _, e := range []struct {
		category string
		payload  string
	}{
		{"acceptance_outcome_recorded", `{"verdict":"passed","head_sha":"` + h1 + `"}`},
		{"acceptance_reopened", `{"head_sha":"` + h2 + `"}`},
	} {
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: row.ID, StageID: &acc.ID, Timestamp: time.Now().UTC(), Category: e.category, Payload: json.RawMessage(e.payload),
		}); err != nil {
			t.Fatalf("seed %s: %v", e.category, err)
		}
	}

	impl, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: row.ID, Sequence: 1, Type: runpkg.StageTypeImplement, ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "implement",
	})
	if err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	raised, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: row.ID, StageID: impl.ID, StageKind: concern.StageKindImplement, ReviewerModel: "m", OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{{Severity: "high", Category: "correctness", Note: "nil deref"}},
	})
	if err != nil {
		t.Fatalf("insert concern: %v", err)
	}
	if err := concernRepo.MarkAddressedPending(ctx, []uuid.UUID{raised[0].ID}, "routed by fix-up"); err != nil {
		t.Fatalf("route concern: %v", err)
	}

	const bearer = "fhk_merge_readiness_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit", "write:stages"}, PlainText: bearer,
	}}
	s := server.New(server.Config{RunRepo: runRepo, ConcernRepo: concernRepo, AuditRepo: auditRepo, APITokenRepo: tokRepo})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	client := newAPIClient(config{backendURL: ts.URL, apiToken: bearer})
	if res, err := client.ResolveConcerns(ctx, row.ID, []string{raised[0].ID.String()}, "re-ran the reproduction"); err != nil || res.Resolved != 1 {
		t.Fatalf("ResolveConcerns = %+v, %v; want one resolved", res, err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v0/runs/"+row.ID.String()+"/gate-view", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET gate-view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET gate-view = %d: %s", resp.StatusCode, body)
	}
	var block map[string]json.RawMessage
	if err := json.Unmarshal(top["merge_readiness"], &block); err != nil {
		t.Fatalf("server emitted no merge_readiness block:\n%s", body)
	}
	fields := jsonFieldNames(reflect.TypeOf(gateViewMergeReadiness{}))
	for k := range block {
		if !fields[k] {
			t.Errorf("server merge_readiness key %q has no field on the gateViewMergeReadiness mirror (tag drift)", k)
		}
	}
	var blockers []map[string]json.RawMessage
	_ = json.Unmarshal(block["blockers"], &blockers)
	if len(blockers) == 0 {
		t.Fatalf("server emitted no blocker:\n%s", body)
	}
	bFields := jsonFieldNames(reflect.TypeOf(gateViewMergeBlocker{}))
	for k := range blockers[0] {
		if !bFields[k] {
			t.Errorf("server blocker key %q has no field on the gateViewMergeBlocker mirror (tag drift)", k)
		}
	}
	var settled []map[string]json.RawMessage
	_ = json.Unmarshal(top["settled"], &settled)
	sFields := jsonFieldNames(reflect.TypeOf(GateViewSettledConcern{}))
	for _, sc := range settled {
		for k := range sc {
			if !sFields[k] {
				t.Errorf("server settled key %q has no field on the GateViewSettledConcern mirror (tag drift)", k)
			}
		}
	}

	gv, err := client.GetGateView(ctx, row.ID, "")
	if err != nil {
		t.Fatalf("GetGateView: %v", err)
	}
	mr := gv.MergeReadiness
	if mr == nil || len(mr.Blockers) != 1 || mr.Blockers[0].Code != "acceptance_stale" {
		t.Fatalf("merge_readiness = %+v, want the acceptance_stale blocker", mr)
	}
	if d := mr.Blockers[0].Details; d["verified_head_sha"] != h1 || d["current_head_sha"] != h2 || d["acceptance_stage_id"] != acc.ID.String() {
		t.Errorf("acceptance_stale details = %+v, want H1/H2 and the stage id", d)
	}
	if len(mr.Undetermined) != 1 || !strings.Contains(mr.Undetermined[0], "no GitHub client") {
		t.Errorf("undetermined = %v, want the approval check's no-GitHub-client reason", mr.Undetermined)
	}
	if len(gv.Settled) != 1 || gv.Settled[0].State != "addressed" || gv.Settled[0].ResolutionBasis != "operator_evidence" {
		t.Errorf("settled = %+v, want the resolved concern addressed with resolution_basis operator_evidence", gv.Settled)
	}
}

// TestMergeCandidateQueue_WireShape pins the hand-maintained
// runMergeCandidateQueue mirror against the REAL backend (ADR-092 / #4200,
// the #371 trap): three runs enqueued on one base through the REAL
// mergequeue.PostgresStore (A admitted live, B and C held), served by the REAL
// server. (a) Every key the SERVER emits in the run read's
// merge_candidate_queue block is a mirror field; (b) C's block decodes through
// GetRun AND GetGateView as held at position 2 behind A, anchored at its head;
// (c) it survives into fishhawk_get_run_status's GetRunStatusOutput.Run;
// (d) the live run decodes as live with its phase. Counterfactual: renaming a
// mirror tag (e.g. position) fails (a) and (b) → RED.
func TestMergeCandidateQueue_WireShape(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	queue := mergequeue.NewPostgresStore(pool)

	ids := make([]uuid.UUID, 3)
	for i, head := range []string{"a1", "b1", "c1"} {
		row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		ids[i] = row.ID
		if _, err := queue.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: row.ID, BaseRef: "main", HeadSHA: head}); err != nil {
			t.Fatalf("enqueue %s: %v", head, err)
		}
		if i == 0 {
			adm, err := queue.AdmitNext(ctx, mergequeue.Base{Repo: "x/y", BaseRef: "main"})
			if err != nil || !adm.Admitted || adm.Entry.RunID != row.ID {
				t.Fatalf("admit A = %+v, %v; want A admitted", adm, err)
			}
		}
	}
	a, c := ids[0], ids[2]

	const bearer = "fhk_merge_queue_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit", "write:stages"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ConcernRepo: concern.NewPostgresRepository(pool), AuditRepo: audit.NewPostgresRepository(pool),
		APITokenRepo: tokRepo, MergeQueue: queue,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := newAPIClient(config{backendURL: ts.URL, apiToken: bearer})

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v0/runs/"+c.String(), nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run = %d: %s", resp.StatusCode, body)
	}
	var block map[string]json.RawMessage
	if err := json.Unmarshal(top["merge_candidate_queue"], &block); err != nil {
		t.Fatalf("server emitted no merge_candidate_queue block:\n%s", body)
	}
	fields := jsonFieldNames(reflect.TypeOf(runMergeCandidateQueue{}))
	for k := range block {
		if !fields[k] {
			t.Errorf("server merge_candidate_queue key %q has no field on the runMergeCandidateQueue mirror (tag drift)", k)
		}
	}

	check := func(surface string, q *runMergeCandidateQueue) {
		t.Helper()
		if q == nil {
			t.Fatalf("%s: merge_candidate_queue decoded to nil", surface)
		}
		if q.State != "held" || q.Position != 2 || q.LiveRunID != a.String() || q.AnchoredHeadSHA != "c1" ||
			q.BaseRef != "main" || q.Repo != "x/y" || q.EnqueuedAt.IsZero() || q.Note == "" {
			t.Errorf("%s: merge_candidate_queue = %+v, want held at position 2 behind %s anchored at c1", surface, q, a)
		}
	}
	r, err := client.GetRun(ctx, c)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	check("run", r.MergeCandidateQueue)
	gv, err := client.GetGateView(ctx, c, "")
	if err != nil {
		t.Fatalf("GetGateView: %v", err)
	}
	check("gate view", gv.MergeCandidateQueue)

	resolver := &runResolver{api: client, getenv: envFuncFromMap(nil)}
	_, out, err := resolver.getRunStatus(ctx, nil, GetRunStatusInput{RunID: c.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	check("get_run_status", out.Run.MergeCandidateQueue)

	live, err := client.GetRun(ctx, a)
	if err != nil {
		t.Fatalf("GetRun A: %v", err)
	}
	if q := live.MergeCandidateQueue; q == nil || q.State != "live" || q.Phase != string(mergequeue.PhaseAdmitted) ||
		q.Position != 0 || q.LiveRunID != a.String() || q.AdmittedAt == nil {
		t.Errorf("A merge_candidate_queue = %+v, want live/admitted with no position", q)
	}
}

// mcvWireSpecYAML declares an implement verify command, so the backend's
// merge-candidate verify state classifies a base-advance head.
const mcvWireSpecYAML = `version: "0.5"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
`

// TestMergeCandidateVerdict_WireShape pins the hand-maintained
// gateViewMergeCandidateVerdict mirror against the REAL backend (E87.2 /
// #4293, approval criterion 5890f97c; the #371 trap). An ENFORCE-mode server
// over Postgres carries a base-advance head whose verdict was TRANSFERRED, a
// refused transfer and a shadow would_transfer whose pass failed. (a) Every
// key the SERVER emits in the block, its last_transfer_refusal and its shadow
// entries is a mirror field; (b) GetGateView and the fishhawk_get_gate_view
// handler decode source transferred with transferred_from = the prior head,
// the refusal's reason_code, and the false transfer. Counterfactual: renaming
// a mirror tag (e.g. transferred_from) fails (a) and (b) → RED.
func TestMergeCandidateVerdict_WireShape(t *testing.T) {
	const (
		root   = "3333333333333333333333333333333333333333"
		head   = "1111111111111111111111111111111111111111"
		other  = "2222222222222222222222222222222222222222"
		bearer = "fhk_merge_candidate_verdict_e2e"
	)
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec: []byte(mcvWireSpecYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	seed := func(category string, payload map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(payload)
		kind := audit.ActorSystem
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: row.ID, Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, Payload: raw,
		}); err != nil {
			t.Fatalf("seed %s: %v", category, err)
		}
	}
	seed(server.CategoryBranchRebased, map[string]any{"new_head_sha": head, "merge_commit_sha": head})
	seed(server.CategoryMergeCandidateVerdictTransferred, map[string]any{
		"from_head": root, "to_head": head, "root_head": root, "reason": "independent",
		"reason_code": "independent", "reason_untrusted": true, "mode": "enforce"})
	seed(server.CategoryMergeCandidateVerdictTransferRefused, map[string]any{
		"from_head": root, "to_head": other, "reason": "reach_overlap", "reason_code": "reach_overlap",
		"reason_detail": "a", "reason_untrusted": true, "mode": "enforce"})
	seed(server.CategoryMergeCandidateVerdictTransferShadow, map[string]any{
		"from_head": root, "to_head": other, "reason_code": "independent", "reason_untrusted": true,
		"would_transfer": true, "duration_ms": 7, "mode": "shadow"})
	seed(server.CategoryMergeCandidateVerified, map[string]any{"head_sha": other, "result": "failed"})

	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ConcernRepo: concern.NewPostgresRepository(pool), AuditRepo: auditRepo,
		APITokenRepo:           tokRepo,
		MergeCandidateTransfer: server.MergeCandidateTransferConfig{Mode: server.MergeCandidateTransferEnforce},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := newAPIClient(config{backendURL: ts.URL, apiToken: bearer})

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v0/runs/"+row.ID.String()+"/gate-view", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET gate-view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET gate-view = %d: %s", resp.StatusCode, body)
	}
	var block map[string]json.RawMessage
	if err := json.Unmarshal(top["merge_candidate_verdict"], &block); err != nil {
		t.Fatalf("server emitted no merge_candidate_verdict block:\n%s", body)
	}
	var refusal map[string]json.RawMessage
	var shadows []map[string]json.RawMessage
	if json.Unmarshal(block["last_transfer_refusal"], &refusal) != nil || json.Unmarshal(block["shadow"], &shadows) != nil || len(shadows) != 1 {
		t.Fatalf("block lacks last_transfer_refusal or one shadow entry: %s", top["merge_candidate_verdict"])
	}
	for _, c := range []struct {
		name string
		keys map[string]json.RawMessage
		typ  reflect.Type
	}{
		{"merge_candidate_verdict", block, reflect.TypeOf(gateViewMergeCandidateVerdict{})},
		{"last_transfer_refusal", refusal, reflect.TypeOf(gateViewTransferRefusal{})},
		{"shadow[]", shadows[0], reflect.TypeOf(gateViewTransferShadow{})},
	} {
		fields := jsonFieldNames(c.typ)
		for k := range c.keys {
			if !fields[k] {
				t.Errorf("server %s key %q has no field on the %s mirror (tag drift)", c.name, k, c.typ.Name())
			}
		}
	}

	check := func(surface string, v *gateViewMergeCandidateVerdict) {
		t.Helper()
		if v == nil {
			t.Fatalf("%s: merge_candidate_verdict decoded to nil", surface)
		}
		if v.TransferMode != "enforce" || v.HeadSHA != head || v.State != "passed" || v.Cause != "base_advance" ||
			v.Source != "transferred" || v.TransferredFrom != root || v.RootHead != root {
			t.Errorf("%s: block = %+v, want %s passed, source transferred from %s", surface, v, head, root)
		}
		if r := v.LastTransferRefusal; r == nil || r.ReasonCode != "reach_overlap" || r.ReasonDetail != "a" ||
			!r.ReasonUntrusted || r.ToHead != other || r.Sequence == 0 {
			t.Errorf("%s: last_transfer_refusal = %+v, want reach_overlap for %s", surface, r, other)
		}
		if len(v.Shadow) != 1 || !v.Shadow[0].WouldTransfer || v.Shadow[0].PassResult != "failed" ||
			!v.Shadow[0].FalseTransfer || v.Shadow[0].DurationMS != 7 || len(v.FalseTransfers) != 1 || v.FalseTransfers[0] != other {
			t.Errorf("%s: shadow = %+v, false_transfers = %v; want one false transfer to %s", surface, v.Shadow, v.FalseTransfers, other)
		}
	}
	gv, err := client.GetGateView(ctx, row.ID, "")
	if err != nil {
		t.Fatalf("GetGateView: %v", err)
	}
	check("client", gv.MergeCandidateVerdict)
	resolver := &runResolver{api: client, getenv: envFuncFromMap(nil)}
	_, out, err := resolver.getGateView(ctx, nil, GetGateViewInput{RunID: row.ID.String()})
	if err != nil {
		t.Fatalf("getGateView: %v", err)
	}
	check("fishhawk_get_gate_view", out.GateView.MergeCandidateVerdict)
}

// TestFanInConflict_WireShape (#4343) pins the hand-maintained
// gateViewFanInConflict mirror against a REAL server response (the
// TestMergeCandidateVerdict_WireShape pattern): a decomposed parent whose
// newest slices_integrated row recorded a base-fold conflict is served by the
// real handler over Postgres. Every key the server emits must be a mirror
// field and every mirror field must be emitted (both directions), and every
// member must decode NON-ZERO through the client and through
// fishhawk_get_gate_view — a tag renamed on either side leaves the block nil
// or a field zero, and the test fails.
func TestFanInConflict_WireShape(t *testing.T) {
	const bearer = "fhk_fan_in_conflict_e2e"
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec: []byte(mcvWireSpecYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	child := uuid.New()
	raw, _ := json.Marshal(map[string]any{
		"child_run_ids": []string{child.String()}, "consolidated_branch": "fishhawk/run-x-consolidated",
		"slice_count": 1, "integration_commit_shas": []string{"s1"},
		"base_ref": "main", "base_sha": "basesha2", "base_fold_outcome": "conflict",
		"base_fold_conflict": map[string]any{
			"conflicting_slice_index": 1, "conflicting_child_run_id": child.String(),
			"conflicting_base_paths": []string{"c.go"}, "base_attribution": "paths_overlap", "compare_truncated": true,
		},
	})
	kind := audit.ActorSystem
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: row.ID, Timestamp: time.Now().UTC(), Category: "slices_integrated", ActorKind: &kind, Payload: raw,
	}); err != nil {
		t.Fatalf("seed slices_integrated: %v", err)
	}

	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ConcernRepo: concern.NewPostgresRepository(pool), AuditRepo: auditRepo,
		APITokenRepo: tokRepo,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := newAPIClient(config{backendURL: ts.URL, apiToken: bearer})

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v0/runs/"+row.ID.String()+"/gate-view", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET gate-view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET gate-view = %d: %s", resp.StatusCode, body)
	}
	var block map[string]json.RawMessage
	if err := json.Unmarshal(top["fan_in_conflict"], &block); err != nil {
		t.Fatalf("server emitted no fan_in_conflict block:\n%s", body)
	}
	fields := jsonFieldNames(reflect.TypeOf(gateViewFanInConflict{}))
	for k := range block {
		if !fields[k] {
			t.Errorf("server fan_in_conflict key %q has no field on the mirror (tag drift)", k)
		}
	}
	for k := range fields {
		if _, ok := block[k]; !ok {
			t.Errorf("mirror field %q is not emitted by the server's fan_in_conflict block (tag drift)", k)
		}
	}

	check := func(surface string, v *gateViewFanInConflict) {
		t.Helper()
		if v == nil {
			t.Fatalf("%s: fan_in_conflict decoded to nil", surface)
		}
		if v.Kind != "base_fold_conflict" || v.Sequence == 0 || v.BaseRef != "main" || v.BaseSHA != "basesha2" ||
			v.ConflictingSliceIndex == nil || *v.ConflictingSliceIndex != 1 || v.ConflictingChildRunID != child.String() ||
			v.BaseAttribution != "paths_overlap" || len(v.ConflictingBasePaths) != 1 || v.ConflictingBasePaths[0] != "c.go" ||
			!v.CompareTruncated || !strings.Contains(v.Remedy, "fishhawk_rebase_run_branch") {
			t.Errorf("%s: block = %+v, want every member decoded non-zero", surface, v)
		}
	}
	gv, err := client.GetGateView(ctx, row.ID, "")
	if err != nil {
		t.Fatalf("GetGateView: %v", err)
	}
	check("client", gv.FanInConflict)
	resolver := &runResolver{api: client, getenv: envFuncFromMap(nil)}
	_, out, err := resolver.getGateView(ctx, nil, GetGateViewInput{RunID: row.ID.String()})
	if err != nil {
		t.Fatalf("getGateView: %v", err)
	}
	check("fishhawk_get_gate_view", out.GateView.FanInConflict)
}

// TestMergeCandidateStaleness_WireShape pins the hand-maintained
// runMergeCandidateStaleness mirror against the REAL backend (E85.4 / #4147;
// the #371 trap). A real server.New over Postgres, with GitHub a githubclient
// over an httptest fake, carries two open decomposed parents (installation,
// PR URL, one child each): one whose PR head the fake reports 250 commits
// behind the base tip (stale, lower-bounded) and one whose PR read fails
// (undetermined). (a) The UNION of the keys the server emits across the two
// gate-view blocks equals the mirror's json tags in BOTH directions; (b) every
// member decodes non-zero through GetGateView, GetRun, the
// fishhawk_get_gate_view handler and fishhawk_get_run_status; (c)
// nextActionsFor over the decoded Run, and the run-status tool's own
// next_actions, name fishhawk_rebase_run_branch. Counterfactual: renaming a
// mirror tag (e.g. base_sha) fails (a) and (b).
func TestMergeCandidateStaleness_WireShape(t *testing.T) {
	const (
		bearer   = "fhk_merge_candidate_staleness_e2e"
		prHead   = "1111111111111111111111111111111111111111"
		baseTip  = "2222222222222222222222222222222222222222"
		stalePR  = 7
		brokenPR = 8
	)
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	newParent := func(pr int) uuid.UUID {
		t.Helper()
		inst := int64(55)
		row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
			InstallationID: &inst,
		})
		if err != nil {
			t.Fatalf("create parent: %v", err)
		}
		if _, err := runRepo.SetRunPullRequestURL(ctx, row.ID, fmt.Sprintf("https://github.com/x/y/pull/%d", pr)); err != nil {
			t.Fatalf("SetRunPullRequestURL: %v", err)
		}
		if _, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
			DecomposedFrom: &row.ID,
		}); err != nil {
			t.Fatalf("create child: %v", err)
		}
		return row.ID
	}
	staleID, brokenID := newParent(stalePR), newParent(brokenPR)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/x/y/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("number") != fmt.Sprint(stalePR) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","merged":false,"head":{"sha":%q,"ref":"fishhawk/run-x-consolidated"},"base":{"ref":"main"}}`, prHead)
	})
	mux.HandleFunc("GET /repos/x/y/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":{"sha":%q}}`, baseTip)
	})
	mux.HandleFunc("GET /repos/x/y/compare/{basehead...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("basehead") != prHead+"..."+baseTip {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		commits := make([]map[string]string, 250)
		for i := range commits {
			commits[i] = map[string]string{"sha": fmt.Sprintf("c%039d", i)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"commits": commits})
	})
	ghSrv := httptest.NewServer(mux)
	t.Cleanup(ghSrv.Close)

	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ConcernRepo: concern.NewPostgresRepository(pool), AuditRepo: auditRepo,
		APITokenRepo: tokRepo,
		GitHub: &githubclient.Client{
			BaseURL: ghSrv.URL,
			Tokens:  rebaseE2ETokens{},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
			AppJWT:  func() (string, error) { return "ghs_jwt", nil },
		},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := newAPIClient(config{backendURL: ts.URL, apiToken: bearer})

	rawBlock := func(id uuid.UUID) map[string]json.RawMessage {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v0/runs/"+id.String()+"/gate-view", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET gate-view: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET gate-view = %d: %s", resp.StatusCode, body)
		}
		var block map[string]json.RawMessage
		if err := json.Unmarshal(top["merge_candidate_staleness"], &block); err != nil {
			t.Fatalf("server emitted no merge_candidate_staleness block:\n%s", body)
		}
		return block
	}
	union := map[string]bool{}
	for _, id := range []uuid.UUID{staleID, brokenID} {
		for k := range rawBlock(id) {
			union[k] = true
		}
	}
	fields := jsonFieldNames(reflect.TypeOf(runMergeCandidateStaleness{}))
	for k := range union {
		if !fields[k] {
			t.Errorf("server merge_candidate_staleness key %q has no field on the mirror (tag drift)", k)
		}
	}
	for k := range fields {
		if !union[k] {
			t.Errorf("mirror field %q is not emitted by the server's merge_candidate_staleness block (tag drift)", k)
		}
	}

	checkStale := func(surface string, v *runMergeCandidateStaleness) {
		t.Helper()
		if v == nil {
			t.Fatalf("%s: merge_candidate_staleness decoded to nil", surface)
		}
		if v.State != "stale" || v.HeadSHA != prHead || v.BaseRef != "main" || v.BaseSHA != baseTip ||
			v.BehindBy == nil || *v.BehindBy != 250 || !v.BehindByLowerBound || v.NextStep != "fishhawk_rebase_run_branch" ||
			v.CheckedAt.IsZero() || v.Note == "" || v.Reason != "" {
			t.Errorf("%s: block = %+v, want stale 250+ behind %s with every stale member decoded non-zero", surface, v, baseTip)
		}
	}
	checkUndetermined := func(surface string, v *runMergeCandidateStaleness) {
		t.Helper()
		if v == nil {
			t.Fatalf("%s: merge_candidate_staleness decoded to nil", surface)
		}
		if v.State != "undetermined" || v.Reason != "pr_read_failed" || v.CheckedAt.IsZero() || v.Note == "" ||
			v.BehindBy != nil || v.NextStep != "" {
			t.Errorf("%s: block = %+v, want undetermined pr_read_failed with no behind_by or next_step", surface, v)
		}
	}

	resolver := &runResolver{api: client, getenv: envFuncFromMap(nil)}
	for _, tc := range []struct {
		id    uuid.UUID
		check func(string, *runMergeCandidateStaleness)
	}{{staleID, checkStale}, {brokenID, checkUndetermined}} {
		gv, err := client.GetGateView(ctx, tc.id, "")
		if err != nil {
			t.Fatalf("GetGateView: %v", err)
		}
		tc.check("gate view", gv.MergeCandidateStaleness)
		_, out, err := resolver.getGateView(ctx, nil, GetGateViewInput{RunID: tc.id.String()})
		if err != nil {
			t.Fatalf("getGateView: %v", err)
		}
		tc.check("fishhawk_get_gate_view", out.GateView.MergeCandidateStaleness)
		r, err := client.GetRun(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		tc.check("run", r.MergeCandidateStaleness)
	}

	r, err := client.GetRun(ctx, staleID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	hasRebase := func(na *NextActions) bool {
		if na == nil {
			return false
		}
		for _, a := range na.Actions {
			if a.Action == "fishhawk_rebase_run_branch" && a.Params["run_id"] == staleID.String() && a.Params["confirm"] == "true" {
				return true
			}
		}
		return false
	}
	if na := nextActionsFor(r, nil, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{}); !hasRebase(na) {
		t.Errorf("nextActionsFor over the decoded Run = %v, want fishhawk_rebase_run_branch", actionNames(na))
	}
	_, out, err := resolver.getRunStatus(ctx, nil, GetRunStatusInput{RunID: staleID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	checkStale("get_run_status", out.Run.MergeCandidateStaleness)
	if !hasRebase(out.NextActions) {
		t.Errorf("get_run_status next_actions = %v, want fishhawk_rebase_run_branch", actionNames(out.NextActions))
	}
}

// TestGetGateView_ScopeOverlapPassThrough (E83.60 / #4118): the backend's
// scope_overlap block survives the MCP seam intact through the client mirror,
// and a body without it (a run-bound caller, a failed compute, an older
// backend) leaves the mirror nil.
func TestGetGateView_ScopeOverlapPassThrough(t *testing.T) {
	runID := uuid.New()
	other := uuid.NewString()
	parent := uuid.NewString()
	raw := json.RawMessage(`{"run_id":"` + runID.String() + `","open":[],"settled":[],` +
		`"suppressed_relitigations":[],"history_incomplete":false,"scope_overlap":{"run_id":"` + runID.String() +
		`","self_scope_resolved":true,"overlaps":[{"run_id":"` + other + `","issue_ref":"x/y#7","decomposed_from":"` + parent +
		`","stage":"implement","stage_state":"running","paths":["a.go","b.go"]}],"scanned_runs":4,"unscoped_runs":2,"truncated":true}}`)
	srv, _ := newGateViewBackend(t, http.StatusOK, raw)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	out, _ := json.Marshal(res.StructuredContent)
	var decoded GetGateViewOutput
	if uerr := json.Unmarshal(out, &decoded); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	ov := decoded.GateView.ScopeOverlap
	if ov == nil {
		t.Fatal("scope_overlap did not decode through the seam (json tag mismatch?)")
	}
	if ov.RunID != runID.String() || !ov.SelfScopeResolved || ov.ScannedRuns != 4 || ov.UnscopedRuns != 2 || !ov.Truncated {
		t.Errorf("scope_overlap scalars = %+v, want every scalar intact", ov)
	}
	if len(ov.Overlaps) != 1 {
		t.Fatalf("overlaps = %+v, want one", ov.Overlaps)
	}
	o := ov.Overlaps[0]
	if o.RunID != other || o.IssueRef != "x/y#7" || o.DecomposedFrom != parent || o.Stage != "implement" ||
		o.StageState != "running" || len(o.Paths) != 2 || o.Paths[0] != "a.go" || o.Paths[1] != "b.go" {
		t.Errorf("overlap = %+v, want every member intact", o)
	}

	bare := json.RawMessage(`{"run_id":"` + runID.String() + `","open":[],"settled":[],` +
		`"suppressed_relitigations":[],"history_incomplete":false}`)
	srv2, _ := newGateViewBackend(t, http.StatusOK, bare)
	res2 := callGateView(t, srv2, map[string]any{"run_id": runID.String()})
	out2, _ := json.Marshal(res2.StructuredContent)
	var decoded2 GetGateViewOutput
	if uerr := json.Unmarshal(out2, &decoded2); uerr != nil {
		t.Fatalf("decode GetGateViewOutput: %v", uerr)
	}
	if decoded2.GateView.ScopeOverlap != nil {
		t.Errorf("scope_overlap = %+v, want nil when the backend omits it", decoded2.GateView.ScopeOverlap)
	}
}

// wireRawGET reads path from the real server with bearer and decodes the top
// level into raw json.
func wireRawGET(t *testing.T, ctx context.Context, base, path, bearer string) map[string]json.RawMessage {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
	}
	return top
}

// requireMirrorKeys fails on any key of block with no field on mirror.
func requireMirrorKeys(t *testing.T, surface string, block json.RawMessage, mirror reflect.Type) {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(block, &keys); err != nil || len(keys) == 0 {
		t.Fatalf("%s: server emitted no block: %s", surface, block)
	}
	fields := jsonFieldNames(mirror)
	for k := range keys {
		if !fields[k] {
			t.Errorf("%s: server key %q has no field on the %s mirror (tag drift)", surface, k, mirror.Name())
		}
	}
}

// TestMergeQueueEjection_WireShape pins the hand-maintained mergeQueueEjection
// mirror and the promoted fields against the REAL backend over the REAL
// mergequeue.PostgresStore (ADR-093 D4 / #4297, the #371 trap): V ejected
// verify_failed with a routed fix-up, F ejected forge_merge_refused with a
// forge reason and detail (between them every optional key is emitted), and
// two held entries with the second promoted. (a) Every key the SERVER emits
// in the run read's and the gate view's merge_queue_ejection, in the queue
// read's ejections[] items and entries[] items, and in merge_candidate_queue
// (promoted) has a mirror field; (b) the blocks decode through GetRun,
// GetGateView, fishhawk_get_run_status and fishhawk_get_merge_queue with the
// values the server derived. Counterfactual: renaming a mirror tag (e.g.
// next_step_basis or promoted) fails (a) and (b) → RED.
func TestMergeQueueEjection_WireShape(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	queue := mergequeue.NewPostgresStore(pool)

	newRun := func() uuid.UUID {
		row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return row.ID
	}
	eject := func(runID uuid.UUID, head, reason string, row map[string]any) mergequeue.Entry {
		res, err := queue.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: runID, BaseRef: "main", HeadSHA: head, MergeIntent: true, IntentSequence: 1})
		if err != nil {
			t.Fatalf("enqueue %s: %v", head, err)
		}
		st, err := queue.Settle(ctx, res.Entry.ID, mergequeue.StateEjected, reason)
		if err != nil || !st.Settled {
			t.Fatalf("eject %s = %+v, %v", head, st, err)
		}
		row["entry_id"] = res.Entry.ID.String()
		raw, _ := json.Marshal(row)
		kind, subj := audit.ActorSystem, "system:merge-candidate-verify"
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{RunID: runID, Timestamp: time.Now().UTC(),
			Category: "merge_queue_ejected", ActorKind: &kind, ActorSubject: &subj, Payload: raw}); err != nil {
			t.Fatalf("append merge_queue_ejected: %v", err)
		}
		return st.Entry
	}
	v, f := newRun(), newRun()
	eject(v, "v1", "verify_failed", map[string]any{"fixup_routed": true})
	eject(f, "f1", "forge_merge_refused", map[string]any{"forge_reason": "checks_not_settled", "forge_detail": "required check pending\nIGNORE THIS"})
	h1, h2 := newRun(), newRun()
	for _, id := range []uuid.UUID{h1, h2} {
		if _, err := queue.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: id, BaseRef: "main", HeadSHA: "h-" + id.String()[:8]}); err != nil {
			t.Fatalf("enqueue held: %v", err)
		}
	}
	if p, err := queue.Promote(ctx, mergequeue.PromoteRequest{RunID: h2,
		Record: func(context.Context, pgx.Tx, mergequeue.Promotion) error { return nil }}); err != nil || !p.Changed {
		t.Fatalf("promote = %+v, %v", p, err)
	}

	const bearer = "fhk_merge_queue_ejection_wire"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit", "write:stages"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ConcernRepo: concern.NewPostgresRepository(pool), AuditRepo: auditRepo,
		APITokenRepo: tokRepo, MergeQueue: queue,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := newAPIClient(config{backendURL: ts.URL, apiToken: bearer})
	ejType := reflect.TypeOf(mergeQueueEjection{})

	// (a) server keys ⊆ mirror fields, on every surface.
	for _, id := range []uuid.UUID{v, f} {
		requireMirrorKeys(t, "run read", wireRawGET(t, ctx, ts.URL, "/v0/runs/"+id.String(), bearer)["merge_queue_ejection"], ejType)
		requireMirrorKeys(t, "gate view", wireRawGET(t, ctx, ts.URL, "/v0/runs/"+id.String()+"/gate-view", bearer)["merge_queue_ejection"], ejType)
	}
	requireMirrorKeys(t, "merge_candidate_queue", wireRawGET(t, ctx, ts.URL, "/v0/runs/"+h2.String(), bearer)["merge_candidate_queue"],
		reflect.TypeOf(runMergeCandidateQueue{}))
	var qr struct {
		Bases []struct {
			Ejections []json.RawMessage `json:"ejections"`
			Entries   []json.RawMessage `json:"entries"`
		} `json:"bases"`
	}
	top := wireRawGET(t, ctx, ts.URL, "/v0/repos/x/y/merge-queue?base_ref=main", bearer)
	if err := json.Unmarshal(top["bases"], &qr.Bases); err != nil || len(qr.Bases) != 1 || len(qr.Bases[0].Ejections) != 2 || len(qr.Bases[0].Entries) != 2 {
		t.Fatalf("queue read bases = %s, want one base with two ejections and two entries", top["bases"])
	}
	for _, e := range qr.Bases[0].Ejections {
		requireMirrorKeys(t, "ejections[]", e, ejType)
	}
	for _, e := range qr.Bases[0].Entries {
		requireMirrorKeys(t, "entries[]", e, reflect.TypeOf(mergeQueueEntry{}))
	}

	// (b) decoded values through every client surface.
	checkV := func(surface string, ej *mergeQueueEjection) {
		t.Helper()
		if ej == nil || ej.Reason != "verify_failed" || ej.NextStep != "await_delegated_fixup" || ej.NextStepBasis != "recorded" ||
			ej.FixupRouted == nil || !*ej.FixupRouted || ej.Reentry != "automatic_after_delegated_fixup" || !ej.MergeIntent ||
			ej.AnchoredHeadSHA != "v1" || ej.Remedy == "" || ej.EjectedAt == nil || ej.RunID != v.String() || ej.BaseRef != "main" || ej.Repo != "x/y" {
			t.Errorf("%s: merge_queue_ejection = %+v, want the routed verify_failed ejection of %s", surface, ej, v)
		}
	}
	r, err := client.GetRun(ctx, v)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	checkV("run", r.MergeQueueEjection)
	gv, err := client.GetGateView(ctx, v, "")
	if err != nil {
		t.Fatalf("GetGateView: %v", err)
	}
	checkV("gate view", gv.MergeQueueEjection)
	resolver := &runResolver{api: client, getenv: envFuncFromMap(nil)}
	_, out, err := resolver.getRunStatus(ctx, nil, GetRunStatusInput{RunID: v.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	checkV("get_run_status", out.Run.MergeQueueEjection)
	if q, err := client.GetRun(ctx, h2); err != nil || q.MergeCandidateQueue == nil || !q.MergeCandidateQueue.Promoted || q.MergeQueueEjection != nil {
		t.Errorf("held promoted run = %+v, %v; want merge_candidate_queue.promoted true and no ejection", q, err)
	}

	_, mq, err := resolver.getMergeQueue(ctx, nil, getMergeQueueInput{Repo: "x/y", BaseRef: "main"})
	if err != nil || len(mq.Bases) != 1 {
		t.Fatalf("getMergeQueue = %+v, %v", mq, err)
	}
	b := mq.Bases[0]
	if len(b.Ejections) != 2 || b.Ejections[0].RunID != f.String() || b.Ejections[1].RunID != v.String() {
		t.Fatalf("ejections = %+v, want F then V (newest first)", b.Ejections)
	}
	fe := b.Ejections[0]
	if fe.Reason != "forge_merge_refused" || fe.NextStep != "fishhawk_merge_run" || fe.ForgeReason != "checks_not_settled" ||
		fe.ForgeDetail != "required check pending" || fe.FixupRouted != nil || fe.Reentry != "operator_merge_run" {
		t.Errorf("F ejection = %+v, want forge_merge_refused / merge_run / checks_not_settled with the sanitised detail", fe)
	}
	checkV("get_merge_queue", &b.Ejections[1])
	if len(b.Entries) != 2 || b.Entries[0].RunID != h2.String() || !b.Entries[0].Promoted || b.Entries[1].Promoted {
		t.Errorf("entries = %+v, want promoted H2 first, then unpromoted H1", b.Entries)
	}
}

// TestGetMergeQueue_OlderBackendEjectionsRenderEmpty: a backend that omits
// ejections (pre-#4297) renders [] per base through the tool, never null.
// Counterfactual: deleting the per-base normalisation → nil (RED).
func TestGetMergeQueue_OlderBackendEjectionsRenderEmpty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"repo":"x/y","bases":[{"base_ref":"main","entries":[],"eta_basis":{"samples":0,"note":"n"}}]}`)
	}))
	t.Cleanup(ts.Close)
	resolver := &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: "fhk_old"}), getenv: envFuncFromMap(nil)}
	_, out, err := resolver.getMergeQueue(context.Background(), nil, getMergeQueueInput{Repo: "x/y"})
	if err != nil || len(out.Bases) != 1 {
		t.Fatalf("getMergeQueue = %+v, %v", out, err)
	}
	if out.Bases[0].Ejections == nil {
		t.Fatal("an older backend's omitted ejections decoded to nil, want []")
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"ejections":[]`) {
		t.Errorf("tool output = %s, want ejections []", raw)
	}
}
