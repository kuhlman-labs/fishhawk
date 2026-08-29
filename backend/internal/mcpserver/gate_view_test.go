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
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
