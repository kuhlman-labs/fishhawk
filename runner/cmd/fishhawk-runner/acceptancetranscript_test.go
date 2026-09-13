package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// transcriptGoldenBytes reads the SHARED golden testdata/wire/
// acceptance_transcript.json — the same bytes the backend ingests through its
// real router (backend/internal/server/trace_test.go's producer-to-consumer
// test), so the two validators are proven to accept identical bytes.
func transcriptGoldenBytes(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "wire", "acceptance_transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// transcriptGoldenServedIDs is the served-id set the golden's two criteria
// (one plan id, one replayed-scenario id) must be members of.
var transcriptGoldenServedIDs = []string{"crit-a", "scenario:issue-12/crit-b"}

type warnRecorder struct{ events []string }

func (w *warnRecorder) warn(event, detail string) { w.events = append(w.events, event+": "+detail) }

func (w *warnRecorder) has(event string) bool {
	for _, e := range w.events {
		if strings.HasPrefix(e, event+":") {
			return true
		}
	}
	return false
}

func withTranscriptPath(t *testing.T) string {
	t.Helper()
	orig := acceptanceVerdictDir
	acceptanceVerdictDir = t.TempDir()
	t.Cleanup(func() { acceptanceVerdictDir = orig })
	return acceptanceTranscriptPath("r", "s")
}

func mustWriteTranscript(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func minimalTranscript() upload.AcceptanceTranscript {
	return upload.AcceptanceTranscript{Criteria: []upload.AcceptanceTranscriptCriterion{{
		ID: "crit-a", Requests: []upload.AcceptanceTranscriptRequest{{Method: "GET", Path: "/healthz", Status: 200}},
		Assertion: "200", Outcome: "passed",
	}}}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAcceptanceTranscriptPath_PinnedFormat pins the keyed path byte-for-byte
// against the format prompt.AcceptanceTranscriptPath renders (the backend side
// pins the same literal), and asserts it matches the `fishhawk-acceptance-*-*.json`
// sweep glob.
func TestAcceptanceTranscriptPath_PinnedFormat(t *testing.T) {
	orig := acceptanceVerdictDir
	acceptanceVerdictDir = "/tmp"
	t.Cleanup(func() { acceptanceVerdictDir = orig })
	got := acceptanceTranscriptPath("RUN", "STAGE")
	if got != "/tmp/fishhawk-acceptance-transcript-RUN-STAGE.json" {
		t.Fatalf("acceptanceTranscriptPath = %q", got)
	}
	if ok, _ := filepath.Match("/tmp/fishhawk-acceptance-*-*.json", got); !ok {
		t.Errorf("path %q must match the scripts/dev sweep glob", got)
	}
}

// TestAcceptanceTranscriptShipMaxBytesValue pins the plain-const ship bound.
func TestAcceptanceTranscriptShipMaxBytesValue(t *testing.T) {
	if acceptanceTranscriptShipMaxBytes != 256*1024 {
		t.Fatalf("acceptanceTranscriptShipMaxBytes = %d, want 262144", acceptanceTranscriptShipMaxBytes)
	}
}

func TestCaptureAcceptanceTranscript_Missing(t *testing.T) {
	path := withTranscriptPath(t)
	var w warnRecorder
	if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil {
		t.Fatalf("got %s, want nil", got)
	}
	if !w.has("acceptance_transcript_missing") {
		t.Errorf("want acceptance_transcript_missing, got %v", w.events)
	}
}

func TestCaptureAcceptanceTranscript_Empty(t *testing.T) {
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, nil)
	var w warnRecorder
	if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil || !w.has("acceptance_transcript_missing") {
		t.Fatalf("got %s events %v", got, w.events)
	}
}

// TestCaptureAcceptanceTranscript_Oversize: a file one byte over
// readSidecarBounded's ceiling is dropped with acceptance_transcript_oversize
// and REMOVED (the #3142 checked-removal shape).
func TestCaptureAcceptanceTranscript_Oversize(t *testing.T) {
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, make([]byte, maxSidecarBytes+1))
	var w warnRecorder
	if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil {
		t.Fatalf("got %d bytes, want nil", len(got))
	}
	if !w.has("acceptance_transcript_oversize") {
		t.Errorf("want acceptance_transcript_oversize, got %v", w.events)
	}
	if w.has("acceptance_transcript_unremovable") {
		t.Errorf("removable file must not emit unremovable: %v", w.events)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("oversize transcript must be removed, stat err = %v", err)
	}
}

// TestCaptureAcceptanceTranscript_Oversize_Unremovable: when os.Remove fails
// (read-only parent dir) the unremovable event is emitted BESIDE the oversize
// one, never instead of it.
func TestCaptureAcceptanceTranscript_Oversize_Unremovable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write bits")
	}
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, make([]byte, maxSidecarBytes+1))
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var w warnRecorder
	if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil {
		t.Fatalf("got %d bytes, want nil", len(got))
	}
	if !w.has("acceptance_transcript_oversize") || !w.has("acceptance_transcript_unremovable") {
		t.Errorf("want oversize AND unremovable, got %v", w.events)
	}
}

// TestCaptureAcceptanceTranscript_Invalid: one case per named rule, each
// dropped with acceptance_transcript_invalid naming the field.
func TestCaptureAcceptanceTranscript_Invalid(t *testing.T) {
	served := []string{"crit-a"}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown field", `{"criteria":[],"bogus":1}`, "bogus"},
		{"trailing object", `{"criteria":[{"id":"crit-a","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}{}`, "single JSON object"},
		{"empty criteria", `{"criteria":[]}`, "criteria: required"},
		{"empty id", `{"criteria":[{"id":"","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".id: required"},
		{"id with space", `{"criteria":[{"id":"crit a","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".id: must match"},
		{"id uppercase", `{"criteria":[{"id":"Crit-A","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".id: must match"},
		{"id 129 bytes", `{"criteria":[{"id":"` + strings.Repeat("a", 129) + `","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, "exceeds the 128 cap"},
		{"scenario prefix without issue segment", `{"criteria":[{"id":"scenario:crit-a","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".id: must match"},
		{"id outside served set", `{"criteria":[{"id":"crit-z","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, "not a served criterion id"},
		{"duplicate id", `{"criteria":[{"id":"crit-a","requests":[],"assertion":"","outcome":"passed","wall_ms":0},{"id":"crit-a","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, "duplicate id"},
		{"seed with space", `{"criteria":[{"id":"crit-a","seed":"a b","requests":[],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".seed: must match"},
		{"bad outcome", `{"criteria":[{"id":"crit-a","requests":[],"assertion":"","outcome":"maybe","wall_ms":0}]}`, ".outcome:"},
		{"method FETCH", `{"criteria":[{"id":"crit-a","requests":[{"method":"FETCH","path":"/x","status":200,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".method:"},
		{"path with space", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/a b","status":200,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".path: must match"},
		{"path with backtick", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/a` + "`" + `b","status":200,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".path: must match"},
		{"path with pipe", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/a|b","status":200,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".path: must match"},
		{"path with newline", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/a\nb","status":200,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".path: must match"},
		{"path 513 bytes", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/` + strings.Repeat("a", 512) + `","status":200,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, "exceeds the 512 cap"},
		{"status 99", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/x","status":99,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, "outside 100..599"},
		{"status 600", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/x","status":600,"elapsed_ms":0}],"assertion":"","outcome":"passed","wall_ms":0}]}`, "outside 100..599"},
		{"negative elapsed", `{"criteria":[{"id":"crit-a","requests":[{"method":"GET","path":"/x","status":200,"elapsed_ms":-1}],"assertion":"","outcome":"passed","wall_ms":0}]}`, ".elapsed_ms"},
		{"negative wall", `{"criteria":[{"id":"crit-a","requests":[],"assertion":"","outcome":"passed","wall_ms":-1}]}`, ".wall_ms"},
		{"assertion 2001 bytes", `{"criteria":[{"id":"crit-a","requests":[],"assertion":"` + strings.Repeat("a", 2001) + `","outcome":"passed","wall_ms":0}]}`, "exceeds the 2000 cap"},
		{"target_url not http", `{"criteria":[{"id":"crit-a","requests":[],"assertion":"","outcome":"passed","wall_ms":0}],"target_url":"localhost:8090"}`, "target_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := withTranscriptPath(t)
			mustWriteTranscript(t, path, []byte(tc.body))
			var w warnRecorder
			if got := captureAcceptanceTranscript(path, served, w.warn); got != nil {
				t.Fatalf("got %s, want nil", got)
			}
			if !w.has("acceptance_transcript_invalid") {
				t.Fatalf("want acceptance_transcript_invalid, got %v", w.events)
			}
			if !strings.Contains(strings.Join(w.events, "\n"), tc.want) {
				t.Errorf("event must name %q, got %v", tc.want, w.events)
			}
		})
	}
}

// TestCaptureAcceptanceTranscript_CapCounts pins the 101-criteria and
// 201-request caps (built programmatically rather than as literals).
func TestCaptureAcceptanceTranscript_CapCounts(t *testing.T) {
	t.Run("101 criteria", func(t *testing.T) {
		var tr upload.AcceptanceTranscript
		for i := 0; i < 101; i++ {
			tr.Criteria = append(tr.Criteria, upload.AcceptanceTranscriptCriterion{ID: "c" + strings.Repeat("a", i+1), Requests: []upload.AcceptanceTranscriptRequest{}, Outcome: "passed"})
		}
		path := withTranscriptPath(t)
		mustWriteTranscript(t, path, mustJSON(t, tr))
		var w warnRecorder
		if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil || !strings.Contains(strings.Join(w.events, "\n"), "exceeds the 100 cap") {
			t.Fatalf("got %d bytes events %v", len(got), w.events)
		}
	})
	t.Run("201 requests", func(t *testing.T) {
		tr := minimalTranscript()
		for i := 0; i < 200; i++ {
			tr.Criteria[0].Requests = append(tr.Criteria[0].Requests, upload.AcceptanceTranscriptRequest{Method: "GET", Path: "/x", Status: 200})
		}
		path := withTranscriptPath(t)
		mustWriteTranscript(t, path, mustJSON(t, tr))
		var w warnRecorder
		if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil || !strings.Contains(strings.Join(w.events, "\n"), "exceeds the 200 cap") {
			t.Fatalf("got %d bytes events %v", len(got), w.events)
		}
	})
}

// TestCaptureAcceptanceTranscript_UnknownCriterionID_Dropped is the served-id
// membership counterfactual vehicle: an id that is grammar-valid but not
// served is dropped; the same id with an EMPTY served set is accepted (so the
// rejection is the membership check, not the grammar).
func TestCaptureAcceptanceTranscript_UnknownCriterionID_Dropped(t *testing.T) {
	tr := minimalTranscript()
	tr.Criteria[0].ID = "crit-unknown"
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, mustJSON(t, tr))
	var w warnRecorder
	if got := captureAcceptanceTranscript(path, []string{"crit-a", "crit-b"}, w.warn); got != nil {
		t.Fatalf("unknown id must be dropped, got %s", got)
	}
	if !strings.Contains(strings.Join(w.events, "\n"), `"crit-unknown" is not a served criterion id`) {
		t.Errorf("events = %v", w.events)
	}
	mustWriteTranscript(t, path, mustJSON(t, tr))
	if got := captureAcceptanceTranscript(path, nil, nil); got == nil {
		t.Fatal("empty served set must skip membership")
	}
}

// TestCaptureAcceptanceTranscript_BodyTruncationMarker: an honest oversize
// response body is BOUNDED with a marker, not dropped; ids/paths are never
// truncated (the Invalid table above pins their rejection).
func TestCaptureAcceptanceTranscript_BodyTruncationMarker(t *testing.T) {
	tr := minimalTranscript()
	tr.Criteria[0].Requests[0].ResponseBody = strings.Repeat("x", 10000)
	tr.Criteria[0].Requests[0].RequestBody = strings.Repeat("y", 5000)
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, mustJSON(t, tr))
	var w warnRecorder
	got := captureAcceptanceTranscript(path, nil, w.warn)
	if got == nil {
		t.Fatalf("oversize bodies must be bounded not dropped: %v", w.events)
	}
	var out upload.AcceptanceTranscript
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	rb := out.Criteria[0].Requests[0].ResponseBody
	if len(rb) > acceptanceTranscriptMaxBodyBytes || !strings.HasSuffix(rb, "...[truncated 5935 bytes]") {
		t.Errorf("response_body len=%d suffix=%q", len(rb), rb[len(rb)-40:])
	}
	qb := out.Criteria[0].Requests[0].RequestBody
	if len(qb) > acceptanceTranscriptMaxBodyBytes || !strings.Contains(qb, "...[truncated ") {
		t.Errorf("request_body len=%d", len(qb))
	}
	if !w.has("acceptance_transcript_bodies_truncated") {
		t.Errorf("events = %v", w.events)
	}
}

// TestCaptureAcceptanceTranscript_Redacts: a bearer-looking credential in a
// response body is replaced by the redaction placeholder in the SHIPPED bytes
// and acceptance_transcript_redacted is emitted.
func TestCaptureAcceptanceTranscript_Redacts(t *testing.T) {
	tr := minimalTranscript()
	tr.Criteria[0].Requests[0].ResponseBody = "Authorization: Bearer ghp_secretsecretsecret"
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, mustJSON(t, tr))
	var w warnRecorder
	got := captureAcceptanceTranscript(path, nil, w.warn)
	if got == nil {
		t.Fatalf("events = %v", w.events)
	}
	if strings.Contains(string(got), "ghp_secretsecretsecret") || !strings.Contains(string(got), "[REDACTED:") {
		t.Errorf("shipped bytes must carry the placeholder, not the secret: %s", got)
	}
	if !w.has("acceptance_transcript_redacted") {
		t.Errorf("events = %v", w.events)
	}
}

// TestCaptureAcceptanceTranscript_PostRedactionRevalidate: a placeholder longer
// than the secret it replaces pushes a body past the cap the backend enforces;
// the redacted bytes are re-validated and dropped rather than shipped to a 400.
func TestCaptureAcceptanceTranscript_PostRedactionRevalidate(t *testing.T) {
	tr := minimalTranscript()
	// 4076 filler + 20-byte AKIA key = 4096 (at the cap pre-redaction); the
	// 28-byte placeholder makes it 4104 post-redaction.
	tr.Criteria[0].Requests[0].ResponseBody = strings.Repeat("x", 4076) + "AKIAABCDEFGHIJKLMNOP"
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, mustJSON(t, tr))
	var w warnRecorder
	if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil {
		t.Fatalf("post-redaction over-cap body must be dropped, got %d bytes", len(got))
	}
	if !strings.Contains(strings.Join(w.events, "\n"), "post-redaction:") {
		t.Errorf("events = %v", w.events)
	}
}

// TestCaptureAcceptanceTranscript_PostRedactionBound: a valid transcript whose
// bytes exceed the 256 KiB ship bound (but sit under the 1 MiB read ceiling)
// is dropped with acceptance_transcript_oversize.
func TestCaptureAcceptanceTranscript_PostRedactionBound(t *testing.T) {
	var tr upload.AcceptanceTranscript
	for i := 0; i < 80; i++ {
		tr.Criteria = append(tr.Criteria, upload.AcceptanceTranscriptCriterion{
			ID: "c" + strings.Repeat("a", i+1),
			Requests: []upload.AcceptanceTranscriptRequest{{Method: "GET", Path: "/x", Status: 200,
				ResponseBody: strings.Repeat("z", 4000)}},
			Outcome: "passed",
		})
	}
	raw := mustJSON(t, tr)
	if len(raw) <= acceptanceTranscriptShipMaxBytes || int64(len(raw)) > maxSidecarBytes {
		t.Fatalf("fixture must sit between the ship bound and the read ceiling, got %d", len(raw))
	}
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, raw)
	var w warnRecorder
	if got := captureAcceptanceTranscript(path, nil, w.warn); got != nil {
		t.Fatalf("got %d bytes, want nil", len(got))
	}
	if !strings.Contains(strings.Join(w.events, "\n"), "over the 262144 ship bound") {
		t.Errorf("events = %v", w.events)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the ship-bound branch must NOT remove the file (only the read-ceiling branch does): %v", err)
	}
}

// TestCaptureAcceptanceTranscript_GoldenRoundTrip: the shared golden validates
// under the runner twin with the served ids it names, and the captured bytes
// are JSON-equivalent to the golden (no redaction hits, no truncation).
func TestCaptureAcceptanceTranscript_GoldenRoundTrip(t *testing.T) {
	golden := transcriptGoldenBytes(t)
	path := withTranscriptPath(t)
	mustWriteTranscript(t, path, golden)
	var w warnRecorder
	got := captureAcceptanceTranscript(path, transcriptGoldenServedIDs, w.warn)
	if got == nil {
		t.Fatalf("golden must capture: %v", w.events)
	}
	var want, have any
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(t, want)) != string(mustJSON(t, have)) {
		t.Errorf("captured bytes diverge from the golden:\n%s\n%s", got, golden)
	}
	if w.has("acceptance_transcript_redacted") || w.has("acceptance_transcript_bodies_truncated") {
		t.Errorf("golden must not redact or truncate: %v", w.events)
	}
	// The failing criterion's LAST request is the 200 (the Done-means case).
	var tr upload.AcceptanceTranscript
	_ = json.Unmarshal(got, &tr)
	last := tr.Criteria[0].Requests[len(tr.Criteria[0].Requests)-1]
	if tr.Criteria[0].Outcome != "failed" || last.Status != 200 {
		t.Errorf("golden shape drifted: outcome=%s last status=%d", tr.Criteria[0].Outcome, last.Status)
	}
}

// ---- run() end-to-end ----

// transcriptRunSetup builds an acceptance-stage run whose served criteria ids
// are grammar-valid lowercase ids and whose fake agent WRITES the given
// transcript sidecar during invocation (after the pre-invoke sweep, as the
// real agent does).
func transcriptRunSetup(t *testing.T, transcript []byte) (*fakeUploader, []string) {
	t.Helper()
	_, fu, args := acceptanceStageSetup(t)
	fu.promptResp.AcceptanceCriteriaIDs = []string{"crit-a", "crit-b"}
	inv := &fakeInvoker{canned: agent.Result{
		OK:               true,
		StructuredOutput: []byte(`{"verdict":"passed","criteria":[{"id":"crit-a","result":"passed"},{"id":"crit-b","result":"passed"}]}`),
	}}
	if transcript != nil {
		inv.onInvoke = func(int, agent.Invocation) {
			mustWriteTranscript(t, acceptanceTranscriptPath(acceptanceTestRunID, acceptanceTestStageID), transcript)
		}
	}
	withFakeInvoker(t, inv)
	return fu, args
}

func twoCriteriaTranscript(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, upload.AcceptanceTranscript{Criteria: []upload.AcceptanceTranscriptCriterion{
		{ID: "crit-a", Requests: []upload.AcceptanceTranscriptRequest{{Method: "GET", Path: "/healthz", Status: 200}}, Assertion: "200", Outcome: "passed"},
		{ID: "crit-b", Requests: []upload.AcceptanceTranscriptRequest{{Method: "GET", Path: "/v0/runs", Status: 200}}, Assertion: "200", Outcome: "passed"},
	}})
}

// TestRun_Acceptance_TranscriptShippedBeforeVerdict: the transcript ships
// BEFORE the verdict and the shipped verdict carries the backend-minted ref.
func TestRun_Acceptance_TranscriptShippedBeforeVerdict(t *testing.T) {
	fu, args := transcriptRunSetup(t, twoCriteriaTranscript(t))
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if strings.Join(fu.shipCallOrder, ",") != "transcript,acceptance" {
		t.Fatalf("ship order = %v, want transcript BEFORE acceptance", fu.shipCallOrder)
	}
	if fu.gotTranscriptArgs == nil || len(fu.gotTranscriptArgs.Body) == 0 {
		t.Fatal("ShipAcceptanceTranscript body empty")
	}
	var shipped struct {
		Verdict    string                          `json:"verdict"`
		Transcript *upload.AcceptanceTranscriptRef `json:"transcript"`
	}
	if err := json.Unmarshal(fu.gotAcceptanceArgs.Body, &shipped); err != nil {
		t.Fatal(err)
	}
	if shipped.Transcript == nil || shipped.Transcript.ArtifactID != "00000000-0000-0000-0000-000000000ddd" ||
		shipped.Transcript.ContentHash != "cafef00dcafef00dcafef00dcafef00dcafef00dcafef00dcafef00dcafef00d" {
		t.Errorf("verdict transcript ref = %+v", shipped.Transcript)
	}
	out := stderr.String()
	for _, want := range []string{`"event":"acceptance_transcript_captured"`, `"event":"acceptance_transcript_shipped"`, `"artifact_id":"00000000-0000-0000-0000-000000000ddd"`} {
		if !strings.Contains(out, want) {
			t.Errorf("runner log missing %s:\n%s", want, out)
		}
	}
}

// TestRun_Acceptance_TranscriptShipFailureStillDeliversVerdict: a transcript
// ship failure logs acceptance_transcript_upload_failed, the verdict still
// ships with NO transcript field, and the stage succeeds.
func TestRun_Acceptance_TranscriptShipFailureStillDeliversVerdict(t *testing.T) {
	fu, args := transcriptRunSetup(t, twoCriteriaTranscript(t))
	fu.transcriptErr = errors.New("upload: ship acceptance transcript exhausted retries: 503")
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (transcript failure never fails the stage):\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("verdict must still ship")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(fu.gotAcceptanceArgs.Body, &fields)
	if _, present := fields["transcript"]; present {
		t.Errorf("verdict must carry NO transcript ref after a ship failure: %s", fu.gotAcceptanceArgs.Body)
	}
	out := stderr.String()
	if !strings.Contains(out, `"event":"acceptance_transcript_upload_failed"`) || !strings.Contains(out, `"outcome":"ok"`) {
		t.Errorf("log:\n%s", out)
	}
}

// TestRun_Acceptance_NoTranscriptSidecar: with no sidecar,
// ShipAcceptanceTranscript is never called and the verdict bytes are unchanged
// (no transcript field), with acceptance_transcript_missing logged.
func TestRun_Acceptance_NoTranscriptSidecar(t *testing.T) {
	fu, args := transcriptRunSetup(t, nil)
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if fu.gotTranscriptArgs != nil {
		t.Fatal("ShipAcceptanceTranscript must not be called without a sidecar")
	}
	if strings.Join(fu.shipCallOrder, ",") != "acceptance" {
		t.Errorf("ship order = %v", fu.shipCallOrder)
	}
	if strings.Contains(string(fu.gotAcceptanceArgs.Body), `"transcript"`) {
		t.Errorf("verdict must be unchanged: %s", fu.gotAcceptanceArgs.Body)
	}
	if !strings.Contains(stderr.String(), `"event":"acceptance_transcript_missing"`) {
		t.Errorf("log:\n%s", stderr.String())
	}
}

// TestRun_Acceptance_TranscriptInvalidStillDeliversVerdict: a transcript that
// names a criterion the plan never served is dropped (second-layer membership)
// and the verdict ships ref-less; the stage succeeds.
func TestRun_Acceptance_TranscriptInvalidStillDeliversVerdict(t *testing.T) {
	bad := mustJSON(t, upload.AcceptanceTranscript{Criteria: []upload.AcceptanceTranscriptCriterion{
		{ID: "crit-zzz", Requests: []upload.AcceptanceTranscriptRequest{}, Outcome: "passed"},
	}})
	fu, args := transcriptRunSetup(t, bad)
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if fu.gotTranscriptArgs != nil {
		t.Fatal("an invalid transcript must not ship")
	}
	if !strings.Contains(stderr.String(), `"event":"acceptance_transcript_invalid"`) {
		t.Errorf("log:\n%s", stderr.String())
	}
}
