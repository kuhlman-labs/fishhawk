package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errServerWithLog builds a Server whose logger writes JSON records to the
// returned buffer, so the 5xx-logging test can read the emitted record.
func errServerWithLog(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(Config{Logger: logger}), &buf
}

// driveWriteError runs a single writeError call through the REAL requestID
// middleware so error_ref is minted on the path that ships. The client-supplied
// X-Request-ID is honored by the middleware, so the test can assert on a fixed
// correlation value rather than a random one.
func driveWriteError(t *testing.T, s *Server, reqID string, status int, code, msg string, details map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, status, code, msg, details)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v0/anything", nil)
	if reqID != "" {
		req.Header.Set("X-Request-ID", reqID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWriteError_5xxDropsNonAllowlistedDetails is counterfactual (1): the 5xx
// allow-list drops every non-allow-listed detail key. The bad state is seeded
// BY CONSTRUCTION — a literal details map — not by calling the control in setup.
// Deleting the redactErrorDetails call in writeError makes "error" reappear and
// this test go RED.
func TestWriteError_5xxDropsNonAllowlistedDetails(t *testing.T) {
	s, _ := errServerWithLog(t)
	rec := driveWriteError(t, s, "corr-1", http.StatusInternalServerError, "internal_error",
		"boom", map[string]any{"run_id": "r1", "error": "secret DB cause 42P01"})

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if _, ok := env.Error.Details["error"]; ok {
		t.Errorf("non-allow-listed \"error\" key survived redaction: %v", env.Error.Details)
	}
	if env.Error.Details["run_id"] != "r1" {
		t.Errorf("allow-listed run_id dropped: %v", env.Error.Details)
	}
	if strings.Contains(rec.Body.String(), "secret DB cause 42P01") {
		t.Errorf("raw cause leaked into the 5xx body: %s", rec.Body.String())
	}
}

// TestWriteError_4xxPassesDetailsVerbatimAndSetsNoErrorRef is counterfactual
// (2): the byte-identical negative criterion. A 4xx response is byte-for-byte
// the pre-#2587 shape — no allow-list, no error_ref, details verbatim. Deleting
// the `status >= 500` gate (making redaction+error_ref unconditional) drops the
// non-allow-listed key AND adds error_ref, so the byte comparison goes RED.
func TestWriteError_4xxPassesDetailsVerbatimAndSetsNoErrorRef(t *testing.T) {
	s, _ := errServerWithLog(t)
	rec := driveWriteError(t, s, "corr-2", http.StatusBadRequest, "validation_failed",
		"bad", map[string]any{"field": "repo", "zzz_extra": "kept verbatim on 4xx"})

	// json.Encoder sorts map keys, so the body is deterministic. error_ref is
	// omitted on 4xx (omitempty), so it does not appear despite a live ref.
	const want = `{"error":{"code":"validation_failed","message":"bad","details":{"field":"repo","zzz_extra":"kept verbatim on 4xx"}}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("4xx body not byte-identical:\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(rec.Body.String(), "error_ref") {
		t.Errorf("error_ref must be absent on a 4xx body: %s", rec.Body.String())
	}
}

// TestWriteError_5xxSetsErrorRefFromRequestID is counterfactual (3): a 5xx body
// carries error_ref equal to the request id (and the X-Request-ID response
// header). Driven through the real middleware, not a bare handler call.
// Deleting the `bodyRef = ref` assignment in writeError makes error_ref empty
// and this test go RED.
func TestWriteError_5xxSetsErrorRefFromRequestID(t *testing.T) {
	s, _ := errServerWithLog(t)
	rec := driveWriteError(t, s, "corr-3", http.StatusBadGateway, "work_item_filing_failed",
		"provider could not file the work item", nil)

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.ErrorRef != "corr-3" {
		t.Errorf("error_ref = %q, want corr-3", env.Error.ErrorRef)
	}
	if hdr := rec.Header().Get("X-Request-ID"); hdr != env.Error.ErrorRef {
		t.Errorf("error_ref %q != X-Request-ID %q", env.Error.ErrorRef, hdr)
	}
}

// TestWriteError_5xxLogsFullCauseKeyedByErrorRef is counterfactual (4): the
// operator keeps the full cause. The control's effect is EMITTED STATE, so the
// test READS the captured slog record after the call returns — an error-identity
// assertion could not distinguish it. The cause arrives through the
// internalCauseKey channel (CONDITION 1), NOT via a plain details key, and the
// test asserts the ONE record joins the ref and that cause while the response
// body discloses neither. Deleting the `slog.String("cause", cause)` append
// makes the log carry no cause and this test go RED.
func TestWriteError_5xxLogsFullCauseKeyedByErrorRef(t *testing.T) {
	s, buf := errServerWithLog(t)
	const planted = "workmgmt/github: create issue: https://ghe.internal.example.com 500 42P01"
	rec := driveWriteError(t, s, "corr-4", http.StatusInternalServerError, "internal_error",
		"could not record the operator-named scope amendment",
		map[string]any{internalCauseKey: planted, "run_id": "r9"})

	// The caller sees neither the cause nor the internal channel key.
	if strings.Contains(rec.Body.String(), planted) {
		t.Errorf("cause leaked into the response body: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), internalCauseKey) {
		t.Errorf("internal cause key leaked into the response body: %s", rec.Body.String())
	}

	// One record joins error_ref and the full cause.
	rec4 := findLogRecord(t, buf, "http error response")
	if rec4["error_ref"] != "corr-4" {
		t.Errorf("log error_ref = %v, want corr-4", rec4["error_ref"])
	}
	if cause, _ := rec4["cause"].(string); cause != planted {
		t.Errorf("log cause = %v, want the full planted cause", rec4["cause"])
	}
}

// TestWriteError_AlwaysStripsInternalCauseKey pins the strip-always channel
// (CONDITION 1's own new-disclosure guard): internalCauseKey is removed from
// the body UNCONDITIONALLY — here on a 4xx, where the 5xx allow-list does NOT
// run — so the channel can never leak even if someone forgets the allow-list.
// Deleting the splitInternalCause call in writeError (passing details straight
// through) makes the cause and the "__cause" key appear in the 4xx body and this
// test go RED.
func TestWriteError_AlwaysStripsInternalCauseKey(t *testing.T) {
	s, _ := errServerWithLog(t)
	const planted = "secret cause that must never ship"
	rec := driveWriteError(t, s, "corr-5", http.StatusBadRequest, "validation_failed",
		"bad", map[string]any{"field": "repo", internalCauseKey: planted})

	if strings.Contains(rec.Body.String(), planted) {
		t.Errorf("internalCauseKey cause leaked on a 4xx: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), internalCauseKey) {
		t.Errorf("internal cause key leaked on a 4xx: %s", rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Details["field"] != "repo" {
		t.Errorf("legitimate 4xx detail dropped: %v", env.Error.Details)
	}
}

// TestRedactableDetailKeys_ExcludesErrorAndAdmitsSurveyedKeys pins the SHIPPED
// allow-list table's observable effect (DONE-MEANS): "error" is dropped and
// every surveyed key survives a redaction round-trip. A comment-only or no-op
// touch of errors.go that a presence gate would pass fails this test.
func TestRedactableDetailKeys_ExcludesErrorAndAdmitsSurveyedKeys(t *testing.T) {
	if _, dropped := redactErrorDetails(map[string]any{"error": "x"})["error"]; dropped {
		t.Fatal("\"error\" must never be admitted by the allow-list")
	}
	// Every surveyed key must survive.
	surveyed := []string{
		"run_id", "stage_id", "provider", "action", "item_id", "retryable",
		"next_actions", "workflow_id", "artifact_id", "reason", "registered",
		"state", "repo", "stage_type", "predicate", "ref", "got", "issue_ref",
		"verdict_sequence", "installation_id", "filed_issue", "field",
		"concern_id", "epic_number", "failed_ordinal", "filed", "stage",
	}
	for _, k := range surveyed {
		out := redactErrorDetails(map[string]any{k: "v"})
		if out[k] != "v" {
			t.Errorf("surveyed key %q was dropped by the allow-list", k)
		}
	}
	// A never-listed key is dropped.
	if out := redactErrorDetails(map[string]any{"totally_new_key": "v"}); out != nil {
		t.Errorf("a non-allow-listed key survived: %v", out)
	}
}

// TestSplitInternalCause covers the cause-channel split directly, including the
// branch where the ONLY key is internalCauseKey so the returned client map is
// emptied to nil (errors.go's len(client)==0 → nil), and the absent-key
// fast-path that returns details unchanged.
func TestSplitInternalCause(t *testing.T) {
	// Cause present alongside a real key: cause extracted, key retained.
	cause, client := splitInternalCause(map[string]any{internalCauseKey: "boom", "run_id": "r1"})
	if cause != "boom" {
		t.Errorf("cause = %q, want boom", cause)
	}
	if client["run_id"] != "r1" {
		t.Errorf("client dropped run_id: %v", client)
	}
	if _, ok := client[internalCauseKey]; ok {
		t.Errorf("client still carries the internal cause key: %v", client)
	}

	// Cause is the ONLY key: client collapses to nil so the omitempty shape holds.
	cause, client = splitInternalCause(map[string]any{internalCauseKey: "only"})
	if cause != "only" {
		t.Errorf("cause = %q, want only", cause)
	}
	if client != nil {
		t.Errorf("client = %v, want nil when the cause was the only key", client)
	}

	// No cause key: details returned unchanged, empty cause.
	in := map[string]any{"field": "repo"}
	cause, client = splitInternalCause(in)
	if cause != "" {
		t.Errorf("cause = %q, want empty when no internal cause key", cause)
	}
	if client["field"] != "repo" {
		t.Errorf("client = %v, want the input passed through", client)
	}

	// Empty input: nothing to split.
	if c, cl := splitInternalCause(nil); c != "" || cl != nil {
		t.Errorf("splitInternalCause(nil) = (%q, %v), want (\"\", nil)", c, cl)
	}
}

// findLogRecord scans the JSON log buffer for the first record whose msg field
// matches want and returns it decoded.
func findLogRecord(t *testing.T, buf *bytes.Buffer, want string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == want {
			return rec
		}
	}
	t.Fatalf("no log record with msg=%q in:\n%s", want, buf.String())
	return nil
}
