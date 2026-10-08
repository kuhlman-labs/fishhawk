package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Known-value redaction of the acceptance evidence surfaces (E72.41 / #3793).
// Every test here drives the real run() through acceptanceStageSetup with the
// acceptanceKnownValueSource seam overridden, and reads the bytes the
// fakeUploader captured at the ship boundary. The bound value is
// knownTestValue(): runtime-built and shaped to match NO DefaultPatterns
// regex, so only the known-value pass can remove it from a surface.

const knownMarker = "[REDACTED:credential:ACC_TOKEN]"

// withKnownValues overrides the acceptance known-value seam for one test.
func withKnownValues(t *testing.T, bindings ...redaction.KnownValue) {
	t.Helper()
	orig := acceptanceKnownValueSource
	acceptanceKnownValueSource = func() []redaction.KnownValue { return bindings }
	t.Cleanup(func() { acceptanceKnownValueSource = orig })
}

// jsonPayload marshals v into an event payload.
func jsonPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	return json.RawMessage(mustJSON(t, v))
}

// shippedBundles gunzips both ShipTrace variants, keyed by variant name, and
// fails unless exactly the raw and the redacted variant shipped.
func shippedBundles(t *testing.T, fu *fakeUploader) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, call := range fu.gotShipCalls {
		plain, err := gunzip(call.Bundle)
		if err != nil {
			t.Fatalf("gunzip %s bundle: %v", call.Variant, err)
		}
		out[call.Variant] = string(plain)
	}
	if len(fu.gotShipCalls) != 2 || out["raw"] == "" || out["redacted"] == "" {
		t.Fatalf("ShipTrace variants = %d %v, want raw + redacted", len(fu.gotShipCalls), keysOf(out))
	}
	return out
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// knownVerdict is a served-criteria-valid verdict for crit-a/crit-b whose
// crit-a `observed` prose carries observed.
func knownVerdict(t *testing.T, observed string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"verdict": "passed",
		"criteria": []map[string]any{
			{"id": "crit-a", "result": "passed", "observed": observed},
			{"id": "crit-b", "result": "passed"},
		},
	})
}

// knownRunSetup is acceptanceStageSetup with the transcript-grammar criteria
// ids and a fake agent returning res; transcript, when non-nil, is written to
// the keyed sidecar during the invocation (after the pre-invoke sweep).
func knownRunSetup(t *testing.T, res agent.Result, transcript []byte) (*fakeUploader, []string) {
	t.Helper()
	_, fu, args := acceptanceStageSetup(t)
	fu.promptResp.AcceptanceCriteriaIDs = []string{"crit-a", "crit-b"}
	inv := &fakeInvoker{canned: res}
	if transcript != nil {
		inv.onInvoke = func(int, agent.Invocation) {
			mustWriteTranscript(t, acceptanceTranscriptPath(acceptanceTestRunID, acceptanceTestStageID), transcript)
		}
	}
	withFakeInvoker(t, inv)
	return fu, args
}

// TestRun_AcceptanceStage_KnownValueScrubbedFromEverySurface: a bound value
// planted in the verdict prose, the transcript (a response body and the
// assertion), an agent trace event (raw) and a second event (URL-escaped) is
// absent, in both forms, from the shipped verdict, the shipped transcript and
// BOTH bundle variants, each of which carries the binding's marker instead.
// The agent-event plants are the isolating fixture for the bundles: the
// acceptance_evidence event is already verdict-scrubbed, so only the bundle
// scrub can remove the value from the agent events.
func TestRun_AcceptanceStage_KnownValueScrubbedFromEverySurface(t *testing.T) {
	v := knownTestValue()
	escaped := url.QueryEscape(v)
	if strings.Contains(escaped, v) {
		t.Fatalf("fixture: the escaped form must not contain the raw value: %q", escaped)
	}
	withKnownValues(t, redaction.KnownValue{Name: "ACC_TOKEN", Value: v})
	transcript := mustJSON(t, upload.AcceptanceTranscript{Criteria: []upload.AcceptanceTranscriptCriterion{
		{ID: "crit-a", Requests: []upload.AcceptanceTranscriptRequest{{Method: "GET", Path: "/healthz", Status: 200, ResponseBody: "echo " + v}}, Assertion: "body echoes " + v, Outcome: "passed"},
		{ID: "crit-b", Requests: []upload.AcceptanceTranscriptRequest{{Method: "GET", Path: "/v0/runs", Status: 200}}, Assertion: "200", Outcome: "passed"},
	}})
	fu, args := knownRunSetup(t, agent.Result{
		OK:               true,
		StructuredOutput: knownVerdict(t, "instance echoed "+v),
		Events: []agent.Event{
			{Kind: "raw", Payload: jsonPayload(t, map[string]string{"text": "agent saw " + v})},
			{Kind: "raw", Payload: jsonPayload(t, map[string]string{"text": "GET https://target.example/x?t=" + escaped})},
		},
	}, transcript)

	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	surfaces := map[string]string{}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("ShipAcceptance not called")
	}
	surfaces["shipped verdict"] = string(fu.gotAcceptanceArgs.Body)
	if fu.gotTranscriptArgs == nil {
		t.Fatalf("ShipAcceptanceTranscript not called:\n%s", stderr.String())
	}
	surfaces["shipped transcript"] = string(fu.gotTranscriptArgs.Body)
	for variant, plain := range shippedBundles(t, fu) {
		surfaces[variant+" bundle"] = plain
	}
	for name, body := range surfaces {
		if strings.Contains(body, v) {
			t.Errorf("bound value survived into the %s", name)
		}
		if strings.Contains(body, escaped) {
			t.Errorf("URL-escaped bound value survived into the %s", name)
		}
		if !strings.Contains(body, knownMarker) {
			t.Errorf("%s carries no %s marker", name, knownMarker)
		}
	}
	out := stderr.String()
	if !strings.Contains(out, `"event":"acceptance_known_values_loaded"`) || !strings.Contains(out, `"count":1`) {
		t.Errorf("missing acceptance_known_values_loaded count=1:\n%s", out)
	}
	if strings.Contains(out, v) {
		t.Errorf("runner log carries the bound value:\n%s", out)
	}
	// The bundle scrub's hits fold into the existing trace_redacted line.
	if line := logLine(out, "trace_redacted"); !strings.Contains(line, `"Pattern":"credential:ACC_TOKEN"`) {
		t.Errorf("trace_redacted does not report the known-value hits: %q", line)
	}
}

// logLine returns the last runner-log line carrying event, or "".
func logLine(log, event string) string {
	var line string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, `"event":"`+event+`"`) {
			line = l
		}
	}
	return line
}

// TestRun_AcceptanceStage_KnownValueScrubbedFromFailureReason: a category-A
// agent failure whose reason carries the bound value ships both bundle
// manifests with the marker in place of the value. RedactDefault alone does
// not match the value, so the redacted manifest proves the known scrub too.
func TestRun_AcceptanceStage_KnownValueScrubbedFromFailureReason(t *testing.T) {
	v := knownTestValue()
	withKnownValues(t, redaction.KnownValue{Name: "ACC_TOKEN", Value: v})
	fu, args := knownRunSetup(t, agent.Result{
		OK:              false,
		FailureCategory: "A",
		FailureReason:   "agent crashed after printing " + v,
	}, nil)

	var stderr strings.Builder
	if got := run(args, &stderr); got == exitOK {
		t.Fatalf("run = exitOK, want a failure exit:\n%s", stderr.String())
	}
	for variant, plain := range shippedBundles(t, fu) {
		if strings.Contains(plain, v) {
			t.Errorf("bound value survived into the %s bundle manifest", variant)
		}
		if !strings.Contains(plain, "agent crashed after printing "+knownMarker) {
			t.Errorf("%s bundle manifest lacks the scrubbed failure reason:\n%s", variant, plain)
		}
	}
	if line := logLine(stderr.String(), "trace_redacted"); !strings.Contains(line, `"Pattern":"credential:ACC_TOKEN"`) {
		t.Errorf("trace_redacted does not report the failure-reason hit: %q", line)
	}
}

// TestRun_AcceptanceStage_KnownValueInTranscriptPath_DropsTranscript: a bound
// value only in a transcript request PATH becomes a `[`-bearing marker the
// path grammar refuses, so the post-redaction re-validation DROPS the
// transcript (acceptance_transcript_invalid) rather than shipping a body the
// backend refuses; the verdict still ships and the value is nowhere.
func TestRun_AcceptanceStage_KnownValueInTranscriptPath_DropsTranscript(t *testing.T) {
	v := knownTestValue()
	withKnownValues(t, redaction.KnownValue{Name: "ACC_TOKEN", Value: v})
	path := "/items/" + v
	if !acceptanceTranscriptPathRe.MatchString(path) {
		t.Fatalf("fixture: the raw path must pass the path grammar so only the post-redaction check can drop it: %q", path)
	}
	transcript := mustJSON(t, upload.AcceptanceTranscript{Criteria: []upload.AcceptanceTranscriptCriterion{
		{ID: "crit-a", Requests: []upload.AcceptanceTranscriptRequest{{Method: "GET", Path: path, Status: 200}}, Assertion: "200", Outcome: "passed"},
	}})
	fu, args := knownRunSetup(t, agent.Result{OK: true, StructuredOutput: knownVerdict(t, "ok")}, transcript)

	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (a dropped transcript never fails the stage):\n%s", got, stderr.String())
	}
	if fu.gotTranscriptArgs != nil {
		t.Errorf("a transcript whose path carried a bound value must be dropped, not shipped: %s", fu.gotTranscriptArgs.Body)
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("the verdict must still ship")
	}
	out := stderr.String()
	if !strings.Contains(out, `"event":"acceptance_transcript_invalid"`) || !strings.Contains(out, "post-redaction") {
		t.Errorf("missing post-redaction acceptance_transcript_invalid:\n%s", out)
	}
	if strings.Contains(string(fu.gotAcceptanceArgs.Body), v) || strings.Contains(out, v) {
		t.Error("bound value reached the verdict or the runner log")
	}
	for variant, plain := range shippedBundles(t, fu) {
		if strings.Contains(plain, v) {
			t.Errorf("bound value survived into the %s bundle", variant)
		}
	}
}

// TestRun_AcceptanceStage_KnownValueBelowFloor_LoggedByName: a binding whose
// value is under redaction.MinKnownValueBytes is not redacted, and the runner
// log names the BINDING (never the value) in
// acceptance_known_value_below_floor; no acceptance_known_values_loaded line
// is emitted because nothing was retained.
func TestRun_AcceptanceStage_KnownValueBelowFloor_LoggedByName(t *testing.T) {
	short := "pin" + strings.Repeat("7", 4)
	if len(short) != redaction.MinKnownValueBytes-1 {
		t.Fatalf("fixture: want a value one byte under the floor, got %d bytes", len(short))
	}
	withKnownValues(t, redaction.KnownValue{Name: "SHORT_PIN", Value: short})
	fu, args := knownRunSetup(t, agent.Result{OK: true, StructuredOutput: knownVerdict(t, "ok")}, nil)

	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("ShipAcceptance not called")
	}
	line := logLine(stderr.String(), "acceptance_known_value_below_floor")
	if line == "" {
		t.Fatalf("missing acceptance_known_value_below_floor:\n%s", stderr.String())
	}
	for _, want := range []string{`"binding":"SHORT_PIN"`, `"floor_bytes":8`, `"run_id":"` + acceptanceTestRunID + `"`} {
		if !strings.Contains(line, want) {
			t.Errorf("below-floor line missing %s: %s", want, line)
		}
	}
	if strings.Contains(stderr.String(), short) {
		t.Errorf("runner log carries the below-floor value:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "acceptance_known_values_loaded") {
		t.Errorf("no value was retained, so nothing may be reported loaded:\n%s", stderr.String())
	}
}

// TestRun_AcceptanceStage_NoKnownValues_LogUnchanged: with the production
// seam (nil until #3795) the acceptance stage emits neither known-value log
// line, so today's runner log is byte-identical.
func TestRun_AcceptanceStage_NoKnownValues_LogUnchanged(t *testing.T) {
	if got := acceptanceKnownValueSource(); got != nil {
		t.Fatalf("production seam must return nil until #3795, got %d bindings", len(got))
	}
	fu, args := knownRunSetup(t, agent.Result{OK: true, StructuredOutput: knownVerdict(t, "ok")}, nil)
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("ShipAcceptance not called")
	}
	if strings.Contains(stderr.String(), "acceptance_known_value") {
		t.Errorf("no binding: no known-value log line may appear:\n%s", stderr.String())
	}
}

// TestRedactAcceptanceVerdict_KnownValueInTargetURL pins what happens to a
// bound value inside the grammar-validated target_url (userinfo password,
// userinfo-escaped as a real URL carries it): redaction runs AFTER
// validation, so the shipped target_url becomes
// `https://user:[REDACTED:credential:ACC_TOKEN]@target.example`. It keeps
// the http(s) prefix, so the runner twin of the backend validator still
// accepts the redacted verdict; a stricter backend URL grammar could refuse
// it (the documented exposure, runner README § "Known-value redaction").
func TestRedactAcceptanceVerdict_KnownValueInTargetURL(t *testing.T) {
	v := knownTestValue()
	kv := knownTestSet(t, v)
	userinfo := strings.TrimPrefix(url.UserPassword("", v).String(), ":")
	if userinfo == v {
		t.Fatalf("fixture: the userinfo-escaped form must differ from the raw value")
	}
	raw := mustJSON(t, map[string]any{
		"verdict":    "passed",
		"target_url": "https://user:" + userinfo + "@target.example",
		"criteria":   []map[string]any{{"id": "AC1", "result": "passed"}},
	})
	validated, err := validateAcceptanceVerdict(raw, []string{"AC1"}, nil)
	if err != nil {
		t.Fatalf("fixture verdict must validate before redaction: %v", err)
	}
	red, hits := redactAcceptanceVerdict(validated, kv)
	var got struct {
		TargetURL string `json:"target_url"`
	}
	if err := json.Unmarshal(red, &got); err != nil {
		t.Fatalf("redacted verdict is not JSON: %v\n%s", err, red)
	}
	if want := "https://user:" + knownMarker + "@target.example"; got.TargetURL != want {
		t.Errorf("target_url = %q, want %q", got.TargetURL, want)
	}
	if findHitCount(hits, "credential:ACC_TOKEN") != 1 {
		t.Errorf("hits = %+v", hits)
	}
	if _, err := validateAcceptanceVerdict(red, []string{"AC1"}, nil); err != nil {
		t.Errorf("the redacted verdict no longer passes the runner twin validator: %v", err)
	}
}
