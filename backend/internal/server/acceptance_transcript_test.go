package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// validTranscript returns a two-criterion transcript: crit-a FAILED whose
// LAST request is a 200 with a mismatching body (the Done-means case — an
// assertion failure on a 2xx), and crit-b passed.
func validTranscript() acceptanceTranscriptBody {
	return acceptanceTranscriptBody{
		Criteria: []acceptanceTranscriptCriterion{
			{
				ID: "crit-a", Seed: "acceptance-dispatched",
				Requests: []acceptanceTranscriptRequest{
					{Method: "POST", Path: "/v0/dev/fixtures", RequestBody: `{"scenario":"acceptance-dispatched"}`, Status: 201, ResponseBody: `{"ok":true}`, ElapsedMs: 12},
					{Method: "GET", Path: "/v0/runs/abc/audit?category=acceptance_outcome_recorded", Status: 200, ResponseBody: `{"items":[]}`, ElapsedMs: 3},
				},
				Assertion: "newest acceptance_outcome_recorded carries transcript.artifact_id",
				Outcome:   "failed", WallMs: 20,
			},
			{
				ID: "scenario:issue-12/crit-b",
				Requests: []acceptanceTranscriptRequest{
					{Method: "GET", Path: "/healthz", Status: 200, ElapsedMs: 1},
				},
				Assertion: "200", Outcome: "passed", WallMs: 2,
			},
		},
		TargetURL: "http://localhost:8090",
	}
}

func transcriptBytes(t *testing.T, tr acceptanceTranscriptBody) []byte {
	t.Helper()
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func shipTranscriptRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/acceptance/transcript?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sigOverride != "" {
		req.Header.Set("X-Fishhawk-Signature", sigOverride)
	} else if priv != nil {
		sig := ed25519.Sign(priv, signing.ComputeMessage(body))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// TestAcceptanceTranscriptCapValue pins the plain-const bound (#3106 rule).
func TestAcceptanceTranscriptCapValue(t *testing.T) {
	if maxAcceptanceTranscriptBytes != 256*1024 {
		t.Fatalf("maxAcceptanceTranscriptBytes = %d, want 262144", maxAcceptanceTranscriptBytes)
	}
}

// --- handler ---------------------------------------------------------------

func TestShipAcceptanceTranscript_Create201(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := transcriptBytes(t, validTranscript())
	w := shipTranscriptRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Idempotent || resp.ContentHash != sha256Hex(body) || resp.StageID != stageID {
		t.Errorf("response = %+v", resp)
	}
	if len(ar.all) != 1 || ar.all[0].Kind != artifact.KindAcceptanceTranscript || string(ar.all[0].Content) != string(body) {
		t.Fatalf("artifact not persisted verbatim as acceptance_transcript: %+v", ar.all)
	}
	if ar.all[0].SchemaVersion != nil {
		t.Errorf("schema_version = %v, want nil", *ar.all[0].SchemaVersion)
	}
}

func TestShipAcceptanceTranscript_Idempotent200(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := transcriptBytes(t, validTranscript())
	if w := shipTranscriptRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first: %d", w.Code)
	}
	w := shipTranscriptRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceTranscriptResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Idempotent || resp.ID != ar.all[0].ID {
		t.Errorf("response = %+v", resp)
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
}

func TestShipAcceptanceTranscript_TooLarge413(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := bytes.Repeat([]byte("x"), maxAcceptanceTranscriptBytes+1)
	w := shipTranscriptRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "body_too_large") {
		t.Fatalf("status = %d, want 413:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

func TestShipAcceptanceTranscript_NonAcceptanceStage400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newAcceptanceServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	priv, _ := sf.issue(t, runID)
	w := shipTranscriptRequest(t, s, runID, stageID, priv, transcriptBytes(t, validTranscript()), "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "acceptance stage") {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

func TestShipAcceptanceTranscript_StageMismatch400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newAcceptanceServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: uuid.New(), Type: run.StageTypeAcceptance}
	priv, _ := sf.issue(t, runID)
	w := shipTranscriptRequest(t, s, runID, stageID, priv, transcriptBytes(t, validTranscript()), "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "does not belong") {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

func TestShipAcceptanceTranscript_UnknownStage404(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newAcceptanceServer(t, runID, stageID)
	delete(rr.getStages, stageID)
	priv, _ := sf.issue(t, runID)
	w := shipTranscriptRequest(t, s, runID, stageID, priv, transcriptBytes(t, validTranscript()), "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "stage_not_found") {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

func TestShipAcceptanceTranscript_BadSignature401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	sf.issue(t, runID)
	body := transcriptBytes(t, validTranscript())
	w := shipTranscriptRequest(t, s, runID, stageID, nil, body, hex.EncodeToString(bytes.Repeat([]byte{1}, 64)))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "signature_invalid") {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0", len(ar.all))
	}
}

func TestShipAcceptanceTranscript_BadRunID400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newAcceptanceServer(t, runID, stageID)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/acceptance/transcript?stage_id="+stageID.String(), bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/acceptance/transcript?stage_id=nope", bytes.NewReader([]byte(`{}`)))
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

func TestShipAcceptanceTranscript_GetByHashError500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	ar.getByHashErr = fmt.Errorf("db down")
	priv, _ := sf.issue(t, runID)
	w := shipTranscriptRequest(t, s, runID, stageID, priv, transcriptBytes(t, validTranscript()), "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
}

func TestShipAcceptanceTranscript_CreateError500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	ar.createErr = fmt.Errorf("db down")
	priv, _ := sf.issue(t, runID)
	w := shipTranscriptRequest(t, s, runID, stageID, priv, transcriptBytes(t, validTranscript()), "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
}

// TestShipAcceptanceTranscript_Validation400 has one 400 case PER named
// validation rule; each asserts the field name lands in details.error and
// nothing is persisted. Malformed inputs are self-paired (crit-a vs crit-a)
// so a byte-exact comparison cannot reject them for another reason.
func TestShipAcceptanceTranscript_Validation400(t *testing.T) {
	mut := func(f func(tr *acceptanceTranscriptBody)) []byte {
		tr := validTranscript()
		f(&tr)
		b, _ := json.Marshal(tr)
		return b
	}
	req := func(method, path string, status int) acceptanceTranscriptRequest {
		return acceptanceTranscriptRequest{Method: method, Path: path, Status: status}
	}
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"unknown top-level field", []byte(`{"criteria":[],"extra":1}`), "unknown field"},
		{"trailing object", append(transcriptBytes(t, validTranscript()), []byte(`{}`)...), "single JSON object"},
		{"empty criteria", []byte(`{"criteria":[]}`), "criteria: required"},
		{"101 criteria", mut(func(tr *acceptanceTranscriptBody) {
			tr.Criteria = nil
			for i := 0; i < 101; i++ {
				tr.Criteria = append(tr.Criteria, acceptanceTranscriptCriterion{ID: fmt.Sprintf("c-%d", i), Outcome: "passed"})
			}
		}), "criteria: 101 entries"},
		{"empty id", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].ID = "" }), "criteria[0].id: required"},
		{"duplicate id", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[1].ID = "crit-a" }), "criteria[1].id: duplicate"},
		{"id with a space", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].ID = "crit a" }), "criteria[0].id: must match"},
		{"id with uppercase", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].ID = "Crit-A" }), "criteria[0].id: must match"},
		{"id of 129 bytes", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].ID = strings.Repeat("a", 129) }), "criteria[0].id: 129 bytes"},
		{"scenario id without issue segment", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].ID = "scenario:crit-a" }), "criteria[0].id: must match"},
		{"seed with a space", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Seed = "plan gate" }), "criteria[0].seed: must match"},
		{"seed of 201 bytes", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Seed = strings.Repeat("s", 201) }), "criteria[0].seed: 201 bytes"},
		{"bad outcome", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Outcome = "ok" }), "criteria[0].outcome"},
		{"method FETCH", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("FETCH", "/x", 200) }), "requests[0].method"},
		{"path with a space", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("GET", "/v0/runs/a b", 200) }), "requests[0].path: must match"},
		{"path with a backtick", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("GET", "/a`b", 200) }), "requests[0].path: must match"},
		{"path with a pipe", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("GET", "/a|b", 200) }), "requests[0].path: must match"},
		{"path with a newline", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("GET", "/a\nb", 200) }), "requests[0].path: must match"},
		{"path without leading slash", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("GET", "healthz", 200) }), "requests[0].path: must match"},
		{"path of 513 bytes", mut(func(tr *acceptanceTranscriptBody) {
			tr.Criteria[0].Requests[0] = req("GET", "/"+strings.Repeat("a", 512), 200)
		}), "requests[0].path: 513 bytes"},
		{"status 99", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("GET", "/x", 99) }), "requests[0].status: 99"},
		{"status 600", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0] = req("GET", "/x", 600) }), "requests[0].status: 600"},
		{"request_body 4097 bytes", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0].RequestBody = strings.Repeat("b", 4097) }), "requests[0].request_body: 4097"},
		{"response_body 4097 bytes", mut(func(tr *acceptanceTranscriptBody) {
			tr.Criteria[0].Requests[0].ResponseBody = strings.Repeat("b", 4097)
		}), "requests[0].response_body: 4097"},
		{"negative elapsed_ms", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Requests[0].ElapsedMs = -1 }), "requests[0].elapsed_ms"},
		{"201 requests", mut(func(tr *acceptanceTranscriptBody) {
			tr.Criteria[0].Requests = nil
			for i := 0; i < 201; i++ {
				tr.Criteria[0].Requests = append(tr.Criteria[0].Requests, req("GET", "/x", 200))
			}
		}), "criteria[0].requests: 201 entries"},
		{"assertion 2001 bytes", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].Assertion = strings.Repeat("a", 2001) }), "criteria[0].assertion: 2001"},
		{"negative wall_ms", mut(func(tr *acceptanceTranscriptBody) { tr.Criteria[0].WallMs = -1 }), "criteria[0].wall_ms"},
		{"target_url not http", mut(func(tr *acceptanceTranscriptBody) { tr.TargetURL = "ftp://x" }), "target_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, _, _ := newAcceptanceServer(t, runID, stageID)
			priv, _ := sf.issue(t, runID)
			w := shipTranscriptRequest(t, s, runID, stageID, priv, tc.body, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "acceptance_transcript_invalid") || !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body missing acceptance_transcript_invalid / %q:\n%s", tc.want, w.Body.String())
			}
			if len(ar.all) != 0 {
				t.Errorf("artifacts = %d, want 0 (rejected, never truncated)", len(ar.all))
			}
		})
	}
}

// TestAcceptanceTranscriptValidate_Accepts pins the accept side of every
// boundary the rejection table probes: the scenario-prefixed id, a 128-byte
// id, a 512-byte path, status 100 and 599, 4096-byte bodies, 100 criteria,
// 200 requests, 2000-byte assertion, every method, every outcome.
func TestAcceptanceTranscriptValidate_Accepts(t *testing.T) {
	tr := validTranscript()
	tr.Criteria[0].ID = strings.Repeat("a", 128)
	tr.Criteria[0].Requests = nil
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		tr.Criteria[0].Requests = append(tr.Criteria[0].Requests, acceptanceTranscriptRequest{Method: m, Path: "/" + strings.Repeat("p", 511), Status: 100, RequestBody: strings.Repeat("b", 4096), ResponseBody: strings.Repeat("b", 4096)})
	}
	for len(tr.Criteria[0].Requests) < 200 {
		tr.Criteria[0].Requests = append(tr.Criteria[0].Requests, acceptanceTranscriptRequest{Method: "GET", Path: "/a/b?c=d&e=f%20g:@!$'()*+,;=~._-", Status: 599})
	}
	tr.Criteria[0].Assertion = strings.Repeat("a", 2000)
	tr.Criteria[0].Seed = strings.Repeat("s", 200)
	for i, o := range []string{"passed", "failed", "skipped", "undecidable"} {
		tr.Criteria = append(tr.Criteria, acceptanceTranscriptCriterion{ID: fmt.Sprintf("scenario:issue-%d/c-%d", i, i), Outcome: o})
	}
	for len(tr.Criteria) < 100 {
		tr.Criteria = append(tr.Criteria, acceptanceTranscriptCriterion{ID: fmt.Sprintf("fill-%d", len(tr.Criteria)), Outcome: "skipped"})
	}
	if _, err := decodeAcceptanceTranscript(transcriptBytes(t, tr)); err != nil {
		t.Fatalf("boundary transcript rejected: %v", err)
	}
}

// TestAcceptanceTranscriptValidate_IDGrammar and _PathCharset / _StatusRange
// are the named counterfactual vehicles: deleting the id regexp, the path
// regexp or the status range check reddens them.
func TestAcceptanceTranscriptValidate_IDGrammar(t *testing.T) {
	for _, id := range []string{"crit a", "Crit-A", strings.Repeat("a", 129), "scenario:crit-a", "-lead", "a_b", "a\tb"} {
		tr := validTranscript()
		tr.Criteria[0].ID = id
		if _, err := decodeAcceptanceTranscript(transcriptBytes(t, tr)); err == nil || !strings.Contains(err.Error(), "criteria[0].id") {
			t.Errorf("id %q: err = %v, want a criteria[0].id rejection", id, err)
		}
	}
	for _, id := range []string{"crit-a", "scenario:issue-12/crit-a", "0", strings.Repeat("a", 128)} {
		tr := validTranscript()
		tr.Criteria[0].ID = id
		if _, err := decodeAcceptanceTranscript(transcriptBytes(t, tr)); err != nil {
			t.Errorf("id %q: unexpected rejection %v", id, err)
		}
	}
}

func TestAcceptanceTranscriptValidate_PathCharset(t *testing.T) {
	for _, p := range []string{"/v0/runs/a b", "/a`b", "/a|b", "/a\nb", "/a\x00b", "/a#b", "/a\"b", "/a<b>", "/a\\b", "/a{b}", "/ä"} {
		tr := validTranscript()
		tr.Criteria[0].Requests[0].Path = p
		if _, err := decodeAcceptanceTranscript(transcriptBytes(t, tr)); err == nil || !strings.Contains(err.Error(), "requests[0].path") {
			t.Errorf("path %q: err = %v, want a requests[0].path rejection", p, err)
		}
	}
}

func TestAcceptanceTranscriptValidate_StatusRange(t *testing.T) {
	for st, ok := range map[int]bool{99: false, 100: true, 599: true, 600: false, 0: false, -1: false} {
		tr := validTranscript()
		tr.Criteria[0].Requests[0].Status = st
		_, err := decodeAcceptanceTranscript(transcriptBytes(t, tr))
		if ok && err != nil {
			t.Errorf("status %d: unexpected rejection %v", st, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "requests[0].status")) {
			t.Errorf("status %d: err = %v, want a status rejection", st, err)
		}
	}
}

// TestAcceptanceTranscriptValidate_RejectsInjectionCorpusPayloads is the
// STRUCTURAL-EXCLUSION test (approval condition 3): every committed agenteval
// injection payload — body, each comment, each verify_output field, each
// containment probe — is fed to the backend validator as criteria[0].id AND
// as requests[0].path and must be REJECTED, because a payload that survives
// ingest is the one that could reach a render (no envelope wraps this block).
//
// PREMISE, CHECKED not asserted: the test also feeds each text with a
// grammar-legal PREFIX (`x-` for the id, `/` for the path) so the leading-
// character rule cannot mask the charset rule — a raw body starting with an
// uppercase letter would be rejected for the wrong reason. If a corpus entry
// ever fits the charset, this test FAILS naming it; the remedy is to exclude
// that entry at render, never to weaken the grammar or drop the entry.
func TestAcceptanceTranscriptValidate_RejectsInjectionCorpusPayloads(t *testing.T) {
	cases, err := agenteval.LoadInjectionCorpus("../agenteval/testdata/injection-corpus")
	if err != nil {
		t.Fatalf("load injection corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("injection corpus is empty — the exclusion test would pass vacuously")
	}
	probes := 0
	for _, c := range cases {
		texts := map[string]string{"body": c.Case.Body}
		for i, cm := range c.Case.Comments {
			texts[fmt.Sprintf("comment[%d]", i)] = cm.Body
		}
		if vo := c.Case.VerifyOutput; vo != nil {
			texts["verify_output.parent_tail"] = vo.ParentTail
			texts["verify_output.parent_summary_detail"] = vo.ParentSummaryDetail
			texts["verify_output.slice_tail"] = vo.SliceTail
			texts["verify_output.slice_summary_detail"] = vo.SliceSummaryDetail
		}
		for i, p := range c.Case.ContainmentProbes {
			texts[fmt.Sprintf("probe[%d]", i)] = p.Text
		}
		for name, text := range texts {
			if text == "" {
				continue
			}
			probes++
			for _, id := range []string{text, "x-" + text} {
				tr := validTranscript()
				tr.Criteria[0].ID = id
				if _, err := decodeAcceptanceTranscript(transcriptBytes(t, tr)); err == nil {
					t.Errorf("%s/%s FITS the id grammar as %q — do NOT weaken the grammar or drop the entry: exclude it at render and record the finding", c.Name, name, id)
				}
			}
			for _, p := range []string{text, "/" + text} {
				tr := validTranscript()
				tr.Criteria[0].Requests[0].Path = p
				if _, err := decodeAcceptanceTranscript(transcriptBytes(t, tr)); err == nil {
					t.Errorf("%s/%s FITS the path charset as %q — do NOT weaken the grammar or drop the entry: exclude it at render and record the finding", c.Name, name, p)
				}
			}
		}
	}
	if probes < 20 {
		t.Fatalf("only %d corpus texts probed; expected the six-class corpus to yield >= 20", probes)
	}
}

// --- summarizer ------------------------------------------------------------

func summaryOf(t *testing.T, tr acceptanceTranscriptBody) []acceptanceTranscriptCriterionSummary {
	t.Helper()
	rows, err := summarizeAcceptanceTranscript(transcriptBytes(t, tr))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return rows
}

// TestSummarizeAcceptanceTranscript_FailedLastRequest200 is the Done-means
// case: an assertion failure on a 200 names its request.
func TestSummarizeAcceptanceTranscript_FailedLastRequest200(t *testing.T) {
	rows := summaryOf(t, validTranscript())
	if len(rows) != 2 || rows[0].ID != "crit-a" || rows[0].Outcome != "failed" || rows[0].RequestCount != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	fr := rows[0].FailingRequest
	if fr == nil || fr.Method != "GET" || fr.Path != "/v0/runs/abc/audit?category=acceptance_outcome_recorded" || fr.Status != 200 {
		t.Fatalf("failing_request = %+v, want the LAST request (GET …/audit -> 200)", fr)
	}
}

func TestSummarizeAcceptanceTranscript_FailedLastRequest500(t *testing.T) {
	tr := validTranscript()
	tr.Criteria[0].Requests = append(tr.Criteria[0].Requests, acceptanceTranscriptRequest{Method: "DELETE", Path: "/boom", Status: 500})
	rows := summaryOf(t, tr)
	if fr := rows[0].FailingRequest; fr == nil || fr.Method != "DELETE" || fr.Path != "/boom" || fr.Status != 500 || rows[0].RequestCount != 3 {
		t.Fatalf("failing_request = %+v", fr)
	}
}

func TestSummarizeAcceptanceTranscript_FailedZeroRequestsNull(t *testing.T) {
	tr := validTranscript()
	tr.Criteria[0].Requests = nil
	rows := summaryOf(t, tr)
	if rows[0].FailingRequest != nil || rows[0].RequestCount != 0 {
		t.Fatalf("row = %+v, want null failing_request", rows[0])
	}
	b, _ := json.Marshal(rows[0])
	if !strings.Contains(string(b), `"failing_request":null`) {
		t.Errorf("explicit null expected: %s", b)
	}
}

func TestSummarizeAcceptanceTranscript_NonFailedNull(t *testing.T) {
	for _, o := range []string{"passed", "skipped", "undecidable"} {
		tr := validTranscript()
		tr.Criteria[0].Outcome = o
		rows := summaryOf(t, tr)
		if rows[0].FailingRequest != nil {
			t.Errorf("outcome %s: failing_request = %+v, want null", o, rows[0].FailingRequest)
		}
	}
}

func TestSummarizeAcceptanceTranscript_InvalidStoredContent(t *testing.T) {
	if _, err := summarizeAcceptanceTranscript([]byte(`{"criteria":[{"id":"Bad Id","outcome":"passed"}]}`)); err == nil {
		t.Fatal("invalid stored content summarized without error")
	}
}

// --- agreement (approval condition 1) -------------------------------------

func TestAcceptanceTranscriptAgreement(t *testing.T) {
	rows := summaryOf(t, validTranscript()) // crit-a failed, scenario:issue-12/crit-b passed
	agree := []acceptanceCriterionResult{{ID: "crit-a", Result: "failed"}, {ID: "scenario:issue-12/crit-b", Result: "passed"}, {ID: "extra", Result: "passed"}}
	if reason, ids := acceptanceTranscriptAgreement(rows, agree); reason != "" || ids != nil {
		t.Errorf("agreeing: reason=%q ids=%v", reason, ids)
	}
	disagree := []acceptanceCriterionResult{{ID: "crit-a", Result: "passed"}, {ID: "scenario:issue-12/crit-b", Result: "passed"}}
	if reason, ids := acceptanceTranscriptAgreement(rows, disagree); reason != transcriptSuppressedOutcomeDisagrees || len(ids) != 1 || ids[0] != "crit-a" {
		t.Errorf("disagreeing: reason=%q ids=%v", reason, ids)
	}
	missing := []acceptanceCriterionResult{{ID: "crit-a", Result: "failed"}}
	if reason, ids := acceptanceTranscriptAgreement(rows, missing); reason != transcriptSuppressedNotInVerdict || len(ids) != 1 || ids[0] != "scenario:issue-12/crit-b" {
		t.Errorf("missing: reason=%q ids=%v", reason, ids)
	}
	if reason, _ := acceptanceTranscriptAgreement(rows, nil); reason != transcriptSuppressedNotInVerdict {
		t.Errorf("un-itemized verdict: reason=%q, want not-in-verdict", reason)
	}
}

// --- resolveAcceptanceTranscriptRef ----------------------------------------

func seedTranscriptArtifact(t *testing.T, ar *fakeArtifactRepo, stageID uuid.UUID, kind artifact.Kind) *artifact.Artifact {
	t.Helper()
	body := transcriptBytes(t, validTranscript())
	a, err := ar.Create(context.Background(), artifact.CreateParams{StageID: stageID, Kind: kind, Content: body, ContentHash: sha256Hex(body)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestResolveAcceptanceTranscriptRef(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, _, _ := newAcceptanceServer(t, runID, stageID)
	ctx := context.Background()
	good := seedTranscriptArtifact(t, ar, stageID, artifact.KindAcceptanceTranscript)
	other := seedTranscriptArtifact(t, ar, uuid.New(), artifact.KindAcceptanceTranscript)
	wrongKind := seedTranscriptArtifact(t, ar, stageID, artifact.KindAcceptance)

	if got, err := s.resolveAcceptanceTranscriptRef(ctx, stageID, acceptanceTranscriptRef{ArtifactID: good.ID.String(), ContentHash: good.ContentHash}); err != nil || got.ID != good.ID {
		t.Fatalf("match: got=%v err=%v", got, err)
	}
	cases := []struct {
		name string
		ref  acceptanceTranscriptRef
		want string
	}{
		{"not a uuid", acceptanceTranscriptRef{ArtifactID: "nope", ContentHash: good.ContentHash}, transcriptRefInvalid},
		{"hash not hex64", acceptanceTranscriptRef{ArtifactID: good.ID.String(), ContentHash: "ABC"}, transcriptRefInvalid},
		{"not found", acceptanceTranscriptRef{ArtifactID: uuid.New().String(), ContentHash: good.ContentHash}, transcriptArtifactNotFound},
		{"kind mismatch", acceptanceTranscriptRef{ArtifactID: wrongKind.ID.String(), ContentHash: wrongKind.ContentHash}, transcriptArtifactKindMismatch},
		{"stage mismatch", acceptanceTranscriptRef{ArtifactID: other.ID.String(), ContentHash: other.ContentHash}, transcriptArtifactStageMismatch},
		{"hash mismatch", acceptanceTranscriptRef{ArtifactID: good.ID.String(), ContentHash: strings.Repeat("0", 64)}, transcriptContentHashMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.resolveAcceptanceTranscriptRef(ctx, stageID, tc.ref)
			if err == nil || got != nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("got=%v err=%v, want %s", got, err, tc.want)
			}
		})
	}
}

// --- end-to-end seam (the real router, both POSTs, both GETs) --------------

// TestAcceptanceTranscriptSeam_EndToEnd drives the real router: signed POST
// transcript → 201; signed POST verdict carrying the injected ref → 201;
// GET /v0/runs/{id}/audit newest acceptance_outcome_recorded carries
// transcript.artifact_id == the created id and criteria[0].failing_request ==
// the transcript's LAST request (a 200); GET /v0/artifacts/{id} returns kind
// acceptance_transcript with the verbatim content.
//
// PRODUCER → CONSUMER (approval condition 2): it then reads the SAME store
// through latestAcceptanceOutcome — the one consumer symbol every downstream
// reader (the merge gate, arbitration, and slice 1's implement-review
// gate-evidence stamping) goes through — and asserts acceptanceOutcome.
// Transcript is decoded from the ENDPOINT-PRODUCED payload, not a seeded
// fixture. It is representative because it is the single serialization
// boundary between the recorded payload and every in-process consumer.
func TestAcceptanceTranscriptSeam_EndToEnd(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)
	seedValidatedHead(seam.au, seam.runID, seam.acceptanceID)

	// 1) transcript
	trBody := transcriptBytes(t, validTranscript())
	w := shipTranscriptRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, trBody, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ship transcript status = %d:\n%s", w.Code, w.Body.String())
	}
	var trResp acceptanceTranscriptResponse
	_ = json.NewDecoder(w.Body).Decode(&trResp)

	// 2) verdict carrying the ref, rows AGREEING with the transcript.
	verdict, _ := json.Marshal(acceptanceBody{
		Verdict: "failed", FailureMode: "assertion_fail",
		Criteria: critRaw(
			acceptanceCriterionResult{ID: "crit-a", Result: "failed", Observed: "items empty"},
			acceptanceCriterionResult{ID: "scenario:issue-12/crit-b", Result: "passed"},
		),
		Transcript: &acceptanceTranscriptRef{ArtifactID: trResp.ID.String(), ContentHash: trResp.ContentHash},
	})
	w = shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, verdict, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ship verdict status = %d:\n%s", w.Code, w.Body.String())
	}

	// 3) GET audit through the router.
	items := auditFeedItems(t, seam.s, seam.runID, CategoryAcceptanceOutcomeRecorded)
	if len(items) == 0 {
		t.Fatal("no acceptance_outcome_recorded entries")
	}
	var payload struct {
		Verdict    string                      `json:"verdict"`
		Transcript acceptanceTranscriptSummary `json:"transcript"`
	}
	if err := json.Unmarshal(items[len(items)-1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Verdict != "failed" {
		t.Errorf("verdict = %q, want failed (the transcript never changes the verdict)", payload.Verdict)
	}
	if payload.Transcript.ArtifactID != trResp.ID.String() || payload.Transcript.ContentHash != trResp.ContentHash {
		t.Errorf("transcript ref = %+v, want %s/%s", payload.Transcript, trResp.ID, trResp.ContentHash)
	}
	if payload.Transcript.SummarySuppressed != "" || len(payload.Transcript.Criteria) != 2 {
		t.Fatalf("transcript block = %+v", payload.Transcript)
	}
	fr := payload.Transcript.Criteria[0].FailingRequest
	if fr == nil || fr.Method != "GET" || fr.Status != 200 || fr.Path != "/v0/runs/abc/audit?category=acceptance_outcome_recorded" {
		t.Errorf("criteria[0].failing_request = %+v, want the transcript's LAST request", fr)
	}
	if payload.Transcript.Criteria[1].FailingRequest != nil {
		t.Errorf("criteria[1].failing_request = %+v, want null on a passed row", payload.Transcript.Criteria[1].FailingRequest)
	}

	// 4) GET artifact through the router: kind + verbatim content.
	req := httptest.NewRequest(http.MethodGet, "/v0/artifacts/"+trResp.ID.String(), nil)
	rec := httptest.NewRecorder()
	seam.s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET artifact status = %d:\n%s", rec.Code, rec.Body.String())
	}
	var art artifactResponse
	_ = json.NewDecoder(rec.Body).Decode(&art)
	if art.Kind != string(artifact.KindAcceptanceTranscript) || string(art.Content) != string(trBody) {
		t.Errorf("artifact = kind %q content %s, want acceptance_transcript with verbatim content", art.Kind, art.Content)
	}

	// 5) Consumer: latestAcceptanceOutcome decodes the endpoint-produced block.
	out, err := seam.s.latestAcceptanceOutcome(context.Background(), seam.runID)
	if err != nil || !out.Recorded || out.Transcript == nil {
		t.Fatalf("latestAcceptanceOutcome = %+v err=%v, want a decoded Transcript", out, err)
	}
	if out.Transcript.ArtifactID != trResp.ID.String() || len(out.Transcript.Criteria) != 2 || out.Transcript.Criteria[0].FailingRequest == nil || out.Transcript.Criteria[0].FailingRequest.Status != 200 {
		t.Errorf("consumer-decoded Transcript = %+v", out.Transcript)
	}
}

// TestAcceptanceTranscriptSeam_DisagreementSuppressesSummaryNotVerdict pins
// approval condition 1 end to end: a PASSED verdict shipped with a transcript
// whose crit-a is FAILED records verdict passed (the ladder runs over the
// verdict's own rows, untouched), keeps the artifact ref, and carries
// criteria:null + summary_suppressed naming the disagreeing id.
func TestAcceptanceTranscriptSeam_DisagreementSuppressesSummaryNotVerdict(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)
	seedValidatedHead(seam.au, seam.runID, seam.acceptanceID)
	w := shipTranscriptRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, transcriptBytes(t, validTranscript()), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ship transcript: %d", w.Code)
	}
	var trResp acceptanceTranscriptResponse
	_ = json.NewDecoder(w.Body).Decode(&trResp)
	verdict, _ := json.Marshal(acceptanceBody{
		Verdict: "passed",
		Criteria: critRaw(
			acceptanceCriterionResult{ID: "crit-a", Result: "passed"},
			acceptanceCriterionResult{ID: "scenario:issue-12/crit-b", Result: "passed"},
		),
		Transcript: &acceptanceTranscriptRef{ArtifactID: trResp.ID.String(), ContentHash: trResp.ContentHash},
	})
	w = shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, verdict, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ship verdict status = %d:\n%s", w.Code, w.Body.String())
	}
	out, err := seam.s.latestAcceptanceOutcome(context.Background(), seam.runID)
	if err != nil || out.Verdict != "passed" {
		t.Fatalf("verdict = %q err=%v, want passed — the transcript must never change the verdict", out.Verdict, err)
	}
	if out.Transcript == nil || out.Transcript.ArtifactID != trResp.ID.String() {
		t.Fatalf("transcript ref dropped: %+v", out.Transcript)
	}
	if out.Transcript.Criteria != nil || out.Transcript.SummarySuppressed != transcriptSuppressedOutcomeDisagrees || len(out.Transcript.DisagreeingIDs) != 1 || out.Transcript.DisagreeingIDs[0] != "crit-a" {
		t.Errorf("summary not suppressed: %+v", out.Transcript)
	}
	raw := lastAppendedByCategory(t, seam.au, CategoryAcceptanceOutcomeRecorded).Payload
	if !strings.Contains(string(raw), `"criteria":null`) || !strings.Contains(string(raw), `"summary_suppressed":"criterion_outcome_disagrees"`) {
		t.Errorf("payload = %s", raw)
	}
}

// TestDecodeAcceptanceTranscriptSummary pins the consumer-side decode: nil on
// absent / null / undecodable / ref-less; populated otherwise.
func TestDecodeAcceptanceTranscriptSummary(t *testing.T) {
	for _, raw := range []string{"", "null", "{", "[]", `{"content_hash":"x"}`, "42"} {
		if got := decodeAcceptanceTranscriptSummary(json.RawMessage(raw)); got != nil {
			t.Errorf("%q: got %+v, want nil", raw, got)
		}
	}
	got := decodeAcceptanceTranscriptSummary(json.RawMessage(`{"artifact_id":"a","content_hash":"h","criteria":[{"id":"c","outcome":"failed","request_count":1,"failing_request":{"method":"GET","path":"/x","status":200}}]}`))
	if got == nil || got.ArtifactID != "a" || len(got.Criteria) != 1 || got.Criteria[0].FailingRequest == nil || got.Criteria[0].FailingRequest.Path != "/x" {
		t.Fatalf("got %+v", got)
	}
}
