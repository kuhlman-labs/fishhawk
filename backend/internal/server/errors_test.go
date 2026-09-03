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

// writeErrorNoMiddleware calls s.writeError DIRECTLY on a bare
// httptest.NewRequest, with NO requestID wrapper. Bypassing the middleware IS
// the point: requestID mints an id whenever X-Request-ID is empty or
// over-length (middleware.go), so no input to driveWriteError can make
// RequestIDFrom(r.Context()) return "". A request context that never passed
// through the middleware is the only way to reach the empty-ref edge, and it
// seeds that state BY CONSTRUCTION — the test never calls RequestIDFrom in its
// own setup to establish it. The state is unreachable in production today
// (every route is wrapped by requestID), so the tests below guard a future
// refactor that drops or reorders that wrapper.
func writeErrorNoMiddleware(t *testing.T, s *Server, status int, code, msg string, details map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/anything", nil)
	rec := httptest.NewRecorder()
	s.writeError(rec, req, status, code, msg, details)
	return rec
}

// TestWriteError_5xxWithoutRequestIDOmitsErrorRef pins the no-request-id edge
// of writeError (E67.30 / #2636): when RequestIDFrom returns "", the 5xx body
// OMITS the error_ref member entirely rather than shipping it present-and-empty
// — the claim errorBody.ErrorRef's doc comment makes and that
// TestWriteError_5xxSetsErrorRefFromRequestID cannot reach, since it drives
// every call through the real middleware. Assertions are on the SHIPPED bytes,
// so a comment-only touch of errors.go cannot satisfy them. Counterfactuals:
// deleting `,omitempty` from errorBody.ErrorRef's json tag renders
// `"error_ref":""` and (1) deleting `bodyRef = ref` in favor of a non-empty
// placeholder fabricates a ref on this path — each fails both the byte-exact
// comparison and the raw-substring guard.
func TestWriteError_5xxWithoutRequestIDOmitsErrorRef(t *testing.T) {
	tests := []struct {
		name    string
		details map[string]any
		want    string
	}{
		{
			name:    "nil details",
			details: nil,
			want:    `{"error":{"code":"internal_error","message":"boom"}}` + "\n",
		},
		{
			// An allow-listed key proves the 5xx redaction path still ran and
			// that ONLY error_ref is missing from the envelope.
			name:    "allow-listed detail survives",
			details: map[string]any{"run_id": "r1"},
			want:    `{"error":{"code":"internal_error","message":"boom","details":{"run_id":"r1"}}}` + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, buf := errServerWithLog(t)
			rec := writeErrorNoMiddleware(t, s, http.StatusInternalServerError,
				"internal_error", "boom", tc.details)

			if got := rec.Body.String(); got != tc.want {
				t.Errorf("5xx body without a request id is not byte-exact:\n got: %q\nwant: %q", got, tc.want)
			}
			// Guard on the key NAME too, so a present-but-empty `"error_ref":""`
			// fails on the substring alone and not only on byte equality.
			if strings.Contains(rec.Body.String(), "error_ref") {
				t.Errorf("error_ref must be omitted when no request id is present: %s", rec.Body.String())
			}

			// The operator-side record is still emitted, degraded to an
			// un-correlatable-but-present line rather than suppressed.
			logged := soleLogRecord(t, buf, "http error response")
			if ref, ok := logged["error_ref"].(string); !ok || ref != "" {
				t.Errorf("log error_ref = %v, want the empty string", logged["error_ref"])
			}
		})
	}
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
	rec4 := soleLogRecord(t, buf, "http error response")
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

// TestWriteError_4xxLogsInternalCauseToOperator pins the fold of the
// internalCauseKey channel into the operator log at a NON-5xx status
// (E67.31 / #2637). Before this change the `cause` attribute was appended only
// inside writeError's `status >= 500` branch while the body strip was already
// unconditional, so a 4xx call site handing a cause through the channel lost it
// entirely — neither the caller nor the operator saw it. The assertions read
// EMITTED STATE (the captured slog record) and SHIPPED BYTES, never a comment:
// a comment-only touch of errors.go fails the log-attribute assertion.
//
// The asymmetry this test encodes: at 4xx the LOG RECORD carries error_ref (the
// request id, from writeError's unconditional attr slice) while the BODY omits
// it, so correlation is operator-side only. Case (d) is the 5xx control — the
// pre-existing behavior (cause AND pre-redaction details in the record,
// error_ref in the body) must be unregressed by widening the append.
//
// Every planted cause is a literal in the table, seeded BY CONSTRUCTION: the
// test never calls splitInternalCause or writeError in setup to establish it,
// so a counterfactual RED lands on the behavioral assertion, not on fixtures.
func TestWriteError_4xxLogsInternalCauseToOperator(t *testing.T) {
	const planted4xx = "storage: update scope amendment: pgx: SQLSTATE 42P01 at ghe.internal.example.com"
	const planted5xx = "workmgmt/github: create issue: https://ghe.internal.example.com 500 42P01"

	tests := []struct {
		name    string
		reqID   string
		status  int
		code    string
		msg     string
		details map[string]any
		// wantBody is the byte-exact shipped response body.
		wantBody string
		// wantCause is the expected `cause` log attribute; "" means the key
		// must be ABSENT from the record entirely (not present-and-empty).
		wantCause string
		// wantBodyRef is the expected error_ref member of the body ("" = omitted).
		wantBodyRef string
		// wantLoggedDetails names a key that must appear under the record's
		// pre-redaction `details` attribute; "" means no `details` attribute.
		wantLoggedDetails string
	}{
		{
			// (a) The whole point: 4xx WITH a cause alongside a legitimate
			// detail key. Body verbatim and cause-free; log carries the cause.
			name:    "4xx with cause: stripped from body, folded into the log",
			reqID:   "corr-4xx-a",
			status:  http.StatusBadRequest,
			code:    "validation_failed",
			msg:     "bad",
			details: map[string]any{"field": "repo", internalCauseKey: planted4xx},
			wantBody: `{"error":{"code":"validation_failed","message":"bad",` +
				`"details":{"field":"repo"}}}` + "\n",
			wantCause:   planted4xx,
			wantBodyRef: "",
		},
		{
			// (b) No internalCauseKey at all: the record must carry NO `cause`
			// key, so the common-path record is unchanged by this widening.
			name:      "4xx with no cause key: no cause attribute",
			reqID:     "corr-4xx-b",
			status:    http.StatusBadRequest,
			code:      "validation_failed",
			msg:       "bad",
			details:   map[string]any{"field": "repo"},
			wantBody:  `{"error":{"code":"validation_failed","message":"bad","details":{"field":"repo"}}}` + "\n",
			wantCause: "",
		},
		{
			// (c) internalCauseKey PRESENT but EMPTY: the retained `cause != ""`
			// guard must suppress the attribute entirely. An empty attribute
			// (`"cause":""`) is a distinct regression from a missing one, so
			// this asserts ABSENCE of the key, not an empty value.
			name:      "4xx with empty cause value: still no cause attribute",
			reqID:     "corr-4xx-c",
			status:    http.StatusBadRequest,
			code:      "validation_failed",
			msg:       "bad",
			details:   map[string]any{"field": "repo", internalCauseKey: ""},
			wantBody:  `{"error":{"code":"validation_failed","message":"bad","details":{"field":"repo"}}}` + "\n",
			wantCause: "",
		},
		{
			// (d) 5xx CONTROL — unregressed: the record still carries BOTH the
			// cause AND the pre-redaction details, and the body still carries
			// error_ref. `details` remains deliberately 5xx-gated.
			name:    "5xx control: cause, pre-redaction details, and a body error_ref",
			reqID:   "corr-5xx-d",
			status:  http.StatusInternalServerError,
			code:    "internal_error",
			msg:     "boom",
			details: map[string]any{"run_id": "r9", internalCauseKey: planted5xx},
			wantBody: `{"error":{"code":"internal_error","message":"boom",` +
				`"details":{"run_id":"r9"},"error_ref":"corr-5xx-d"}}` + "\n",
			wantCause:         planted5xx,
			wantBodyRef:       "corr-5xx-d",
			wantLoggedDetails: "run_id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, buf := errServerWithLog(t)
			rec := driveWriteError(t, s, tc.reqID, tc.status, tc.code, tc.msg, tc.details)

			// SHIPPED BYTES: byte-exact, so a stray error_ref, a surviving
			// "__cause" member, or a leaked cause all fail here.
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body not byte-exact:\n got: %q\nwant: %q", got, tc.wantBody)
			}
			if strings.Contains(rec.Body.String(), internalCauseKey) {
				t.Errorf("internal cause key leaked into the body: %s", rec.Body.String())
			}
			if tc.wantCause != "" && strings.Contains(rec.Body.String(), tc.wantCause) {
				t.Errorf("cause leaked into the body: %s", rec.Body.String())
			}
			if tc.wantBodyRef == "" && strings.Contains(rec.Body.String(), "error_ref") {
				t.Errorf("error_ref must be absent from a 4xx body: %s", rec.Body.String())
			}

			// EMITTED STATE: exactly one record, carrying error_ref at EVERY
			// status (it is unconditional) and the cause per the branch.
			logged := soleLogRecord(t, buf, "http error response")
			if logged["error_ref"] != tc.reqID {
				t.Errorf("log error_ref = %v, want %q", logged["error_ref"], tc.reqID)
			}
			if tc.wantCause == "" {
				if _, present := logged["cause"]; present {
					t.Errorf("log record must carry NO cause key, got %#v", logged["cause"])
				}
			} else {
				if cause, _ := logged["cause"].(string); cause != tc.wantCause {
					t.Errorf("log cause = %#v, want the full planted cause %q", logged["cause"], tc.wantCause)
				}
			}

			// `details` stays 5xx-gated: present with the pre-redaction keys on
			// the control, absent on every 4xx case.
			if tc.wantLoggedDetails == "" {
				if _, present := logged["details"]; present {
					t.Errorf("4xx record must carry no details attribute, got %#v", logged["details"])
				}
			} else {
				d, ok := logged["details"].(map[string]any)
				if !ok {
					t.Fatalf("5xx record details = %#v, want a map", logged["details"])
				}
				if _, present := d[tc.wantLoggedDetails]; !present {
					t.Errorf("5xx record details missing %q: %#v", tc.wantLoggedDetails, d)
				}
			}
		})
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

// TestRedactableDetailKeys_AdmitsIssueSetTimeoutCounts pins the four keys the
// 504 issue_set_resolution_timeout refusal is MADE OF (E54.59 / #3113). A 504
// is a 5xx, so the default-deny redactor runs on it: without these entries the
// operator receives a refusal with no numbers in it at all, which is the empty
// body the counts exist to prevent.
//
// This is the direct counterfactual vehicle for the allow-list additions —
// deleting any of the four entries from errors.go reddens the matching case
// here AND the campaigns_test 504 detail assertions, which go through the real
// writeError.
func TestRedactableDetailKeys_AdmitsIssueSetTimeoutCounts(t *testing.T) {
	for _, k := range []string{"resolved", "items_total", "suggested_grooming_order_limit", "budget_seconds"} {
		out := redactErrorDetails(map[string]any{k: 7})
		if out[k] != 7 {
			t.Errorf("issue-set timeout count key %q was dropped by the 5xx allow-list; the 504 refusal would ship with no numbers", k)
		}
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

// soleLogRecord scans the JSON log buffer for records whose msg field matches
// want and returns the single match decoded. It asserts the ONE-record
// invariant every caller relies on rather than returning the first hit: the
// chokepoint's contract is that a 5xx emits EXACTLY ONE "http error response"
// record joining error_ref to the full cause, so an implementation that emitted
// the record twice (once redacted for the client path, once with the cause)
// would satisfy a first-match helper while splitting the correlation across two
// lines and re-disclosing the cause on an extra record. Counting here makes
// that failure mode observable at every call site at once.
func soleLogRecord(t *testing.T, buf *bytes.Buffer, want string) map[string]any {
	t.Helper()
	var matches []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == want {
			matches = append(matches, rec)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want EXACTLY ONE log record with msg=%q, got %d in:\n%s", want, len(matches), buf.String())
	}
	return matches[0]
}
