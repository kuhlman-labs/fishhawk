package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// escape_test.go drives POST /v0/runs/{run_id}/escapes (E94.3 / #4694, ADR-117
// D3 + A1) through registerRoutes' full wrapper chain against in-memory fakes
// on fixture repository acme/widgets (ADR-110). Every refusal asserts status,
// code AND that no escape row was appended (read after the call).

const (
	escapeUUIDKey  = "3f2b9c1e-8d4a-4b6e-9f1a-2c3d4e5f6a7b"
	escapeAdvisory = "GHSA-jfh8-c2jp-5v3q"
)

var escapeHeadSHA = strings.Repeat("a", 40)

// escapeAuditFake stamps each appended entry with its 1-based append index
// as Sequence, both on the returned entry and on ListForRunByCategory.
type escapeAuditFake struct{ *auditFake }

func newEscapeAuditFake() *escapeAuditFake {
	a := newAuditFake()
	a.stampSequence = true
	return &escapeAuditFake{a}
}

func (f *escapeAuditFake) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	e, err := f.auditFake.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	e.Sequence = int64(len(f.appended))
	f.mu.Unlock()
	return e, nil
}

// escapeRows returns every appended escape_recorded row.
func (f *escapeAuditFake) escapeRows() []audit.ChainAppendParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range f.appended {
		if p.Category == CategoryEscapeRecorded {
			out = append(out, p)
		}
	}
	return out
}

type escapeFixture struct {
	s        *Server
	mux      *http.ServeMux
	runs     *fakeRepo
	audit    *escapeAuditFake
	concerns *fakeConcernRepo
	run      *run.Run
}

func newEscapeRun(account string) *run.Run {
	url := "https://github.com/acme/widgets/pull/7"
	return &run.Run{
		ID: uuid.New(), Repo: "acme/widgets", WorkflowID: "feature_change", AccountID: account,
		State: run.StateRunning, PullRequestURL: &url, CreatedAt: time.Now().UTC(),
	}
}

func newEscapeFixture(t *testing.T) *escapeFixture {
	t.Helper()
	fx := &escapeFixture{runs: newFakeRepo(), audit: newEscapeAuditFake(), concerns: newFakeConcernRepo()}
	fx.run = newEscapeRun(testOperatorAccountID)
	fx.runs.runs[fx.run.ID] = fx.run
	fx.s = New(Config{Addr: "127.0.0.1:0", RunRepo: fx.runs, AuditRepo: fx.audit, ConcernRepo: fx.concerns})
	fx.mux = http.NewServeMux()
	fx.s.registerRoutes(fx.mux)
	return fx
}

func (fx *escapeFixture) post(t *testing.T, id Identity, body any) *httptest.ResponseRecorder {
	t.Helper()
	return fx.postTo(t, id, fx.run.ID.String(), body)
}

func (fx *escapeFixture) postTo(t *testing.T, id Identity, runID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw string
	switch b := body.(type) {
	case string:
		raw = b
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		raw = string(buf)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/escapes", strings.NewReader(raw))
	req = withIdentity(req, id)
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

// addConcern seeds an implement-stage concern on runID.
func (fx *escapeFixture) addConcern(runID uuid.UUID, originSeq int64) *concern.Concern {
	model := "claude-fable-5-1"
	c := &concern.Concern{
		ID: uuid.New(), RunID: runID, StageID: uuid.New(), StageKind: concern.StageKindImplement,
		OriginReviewSequence: originSeq, ReviewerModel: &model, ReviewerRole: concern.ReviewerRoleStandard,
		Severity: "high", Note: "n", State: concern.StateRaised,
	}
	fx.concerns.rows = append(fx.concerns.rows, c)
	return c
}

// testReviewedHead is a recorded reviewed_head built from slice 0's type.
func testReviewedHead() *planreview.ReviewedHead {
	return &planreview.ReviewedHead{
		Status: planreview.ReviewedHeadRecorded, Kind: planreview.ReviewedHeadKindWipTree, Origin: "trace",
		SHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), ChangeID: "chg-1",
		DiffMode: "full", PatchRendered: true, Truncated: true, ChangedFileCount: 3,
		Files: []string{"a.go"}, PartialFiles: []string{"b.go"}, OmittedFiles: []string{"c.go"},
	}
}

// seedRound seeds the concern's round: an implement_review_started row at
// startedSeq (carrying head when non-nil) and an implement_reviewed verdict at
// verdictSeq naming round sequence roundSeq.
func (fx *escapeFixture) seedRound(t *testing.T, runID uuid.UUID, startedSeq, verdictSeq, roundSeq int64, head *planreview.ReviewedHead) {
	t.Helper()
	started, err := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 2, ReviewedHead: head})
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := json.Marshal(planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove, ReviewRoundSequence: roundSeq})
	if err != nil {
		t.Fatal(err)
	}
	rid := runID
	fx.audit.seeded = append(fx.audit.seeded,
		&audit.Entry{RunID: &rid, Sequence: startedSeq, Category: "implement_review_started", Payload: started},
		&audit.Entry{RunID: &rid, Sequence: verdictSeq, Category: "implement_reviewed", Payload: verdict},
	)
}

func escapeStaticID() Identity {
	return Identity{Subject: "github:alice", TokenID: "tok-static", Scopes: []string{"write:runs"}, AuthMethod: "static", AccountID: testOperatorAccountID}
}

func escapeOAuthID() Identity {
	return Identity{Subject: "github:alice", TokenID: "tok-oauth", Scopes: []string{"write:runs"}, AuthMethod: "oauth", AccountID: testOperatorAccountID}
}

func escapeSessionID() Identity {
	return Identity{Subject: "github:alice", UserID: "u-1", SessionID: "sess-1", AccountID: testOperatorAccountID}
}

func escapeAgentID() Identity {
	return Identity{Subject: "operator-agent/v1", TokenID: "tok-agent", Scopes: []string{"write:runs"}, AuthMethod: "oauth", AccountID: testOperatorAccountID}
}

func humanFindingBody(key string) map[string]any {
	return map[string]any{
		"source": "human_finding", "severity": "high", "category": "correctness",
		"description": "nil deref on an empty input", "files": []string{"backend/internal/x/x.go"},
		"head_sha": escapeHeadSHA, "idempotency_key": key,
	}
}

func securityBody() map[string]any {
	return map[string]any{
		"source": "human_finding", "severity": "high", "category": "security",
		"security_class": "authz_bypass", "advisory_id": escapeAdvisory,
		"head_sha": escapeHeadSHA, "idempotency_key": escapeUUIDKey,
	}
}

func laterReviewerBody(concernID uuid.UUID) map[string]any {
	return map[string]any{
		"source": "later_reviewer", "severity": "medium", "category": "correctness",
		"description": "the reviewer missed a race", "files": []string{"backend/internal/y/y.go"},
		"head_sha": escapeHeadSHA, "concern_id": concernID.String(),
	}
}

func securityLaterReviewerBody(concernID uuid.UUID) map[string]any {
	return map[string]any{
		"source": "later_reviewer", "severity": "high", "category": "security",
		"security_class": "authz_bypass", "advisory_id": escapeAdvisory,
		"head_sha": escapeHeadSHA, "concern_id": concernID.String(),
	}
}

func closeBody(seq int64, judgement string) map[string]any {
	return map[string]any{"supersedes_sequence": seq, "attestation": map[string]any{"judgement": judgement}}
}

// wantRefused asserts status + code and that the escape row count is
// unchanged from before.
func wantRefused(t *testing.T, fx *escapeFixture, rec *httptest.ResponseRecorder, before, status int, code string) errorBody {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	eb := decodeErrorEnvelope(t, rec)
	if eb.Code != code {
		t.Fatalf("code = %q, want %q; body %s", eb.Code, code, rec.Body.String())
	}
	if n := len(fx.audit.escapeRows()); n != before {
		t.Fatalf("escape rows = %d after a refusal, want %d (a refusal appends nothing)", n, before)
	}
	return eb
}

func decodeEscapeResponse(t *testing.T, rec *httptest.ResponseRecorder) escapeResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	var out escapeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode 201: %v", err)
	}
	return out
}

// lastEscapePayload decodes the most recent escape row as a generic map.
func lastEscapePayload(t *testing.T, fx *escapeFixture) map[string]any {
	t.Helper()
	rows := fx.audit.escapeRows()
	if len(rows) == 0 {
		t.Fatal("no escape row appended")
	}
	var m map[string]any
	if err := json.Unmarshal(rows[len(rows)-1].Payload, &m); err != nil {
		t.Fatalf("decode escape payload: %v", err)
	}
	return m
}

func escObj(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	esc, ok := m["escape"].(map[string]any)
	if !ok {
		t.Fatalf("payload carries no escape object: %v", m)
	}
	return esc
}

// recordBirth records a human_finding with the given identity and returns its
// audit sequence.
func recordBirth(t *testing.T, fx *escapeFixture, id Identity, key string) int64 {
	t.Helper()
	return decodeEscapeResponse(t, fx.post(t, id, humanFindingBody(key))).AuditSequence
}

// CF-B16: dropping escapeRequireAuthenticated answers an anonymous caller
// 403 account_forbidden on an account-owned run.
func TestRecordEscape_AnonymousRefused401(t *testing.T) {
	fx := newEscapeFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+fx.run.ID.String()+"/escapes",
		strings.NewReader(`{"source":"human_finding"}`))
	rec := httptest.NewRecorder()
	fx.s.Handler().ServeHTTP(rec, req)
	wantRefused(t, fx, rec, 0, http.StatusUnauthorized, "authentication_required")
	// The in-handler repeat holds when the outer wrapper is bypassed.
	rec = httptest.NewRecorder()
	fx.s.handleRecordEscape(rec, withIdentity(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")), Identity{Subject: "anonymous"}))
	wantRefused(t, fx, rec, 0, http.StatusUnauthorized, "authentication_required")
}

// CF-B1: the mcp:run: identity HOLDS write:runs, so only the run-bound
// refusal stops it.
func TestRecordEscape_RunBoundTokenRefused(t *testing.T) {
	fx := newEscapeFixture(t)
	for name, subject := range map[string]string{
		"own_run":   "mcp:run:" + fx.run.ID.String(),
		"other_run": "mcp:run:" + uuid.NewString(),
		"malformed": "mcp:run:garbage",
	} {
		t.Run(name, func(t *testing.T) {
			id := Identity{Subject: subject, TokenID: "tok-run", Scopes: []string{"write:runs"}, AccountID: testOperatorAccountID}
			wantRefused(t, fx, fx.post(t, id, humanFindingBody("k-"+name)), 0, http.StatusForbidden, "run_token_forbidden")
		})
	}
}

func TestRecordEscape_MissingScopeRefused(t *testing.T) {
	fx := newEscapeFixture(t)
	id := escapeStaticID()
	id.Scopes = []string{"read:runs"}
	wantRefused(t, fx, fx.post(t, id, humanFindingBody("k1")), 0, http.StatusForbidden, "insufficient_scope")
}

func TestRecordEscape_Unconfigured503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	rec := httptest.NewRecorder()
	s.handleRecordEscape(rec, withIdentity(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")), escapeStaticID()))
	if rec.Code != http.StatusServiceUnavailable || decodeErrorEnvelope(t, rec).Code != "escape_unconfigured" {
		t.Fatalf("no repos: %d %s, want 503 escape_unconfigured", rec.Code, rec.Body.String())
	}
	// A later_reviewer escape also needs the concern repository.
	fx := newEscapeFixture(t)
	fx.s.cfg.ConcernRepo = nil
	wantRefused(t, fx, fx.post(t, escapeStaticID(), laterReviewerBody(uuid.New())), 0, http.StatusServiceUnavailable, "escape_unconfigured")
}

func TestRecordEscape_ValidationFailures(t *testing.T) {
	fx := newEscapeFixture(t)
	with := func(base map[string]any, k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range base {
			out[kk] = vv
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	hf := humanFindingBody("k1")
	many := make([]string, escapeMaxFiles+1)
	for i := range many {
		many[i] = "f" + strconv.Itoa(i) + ".go"
	}
	commits := make([]string, escapeMaxIntroducingCommits+1)
	for i := range commits {
		commits[i] = escapeHeadSHA
	}
	cases := []struct {
		name  string
		body  any
		field string
	}{
		{"empty_body", "", "body"},
		{"not_json", "{", "body"},
		{"unknown_field", with(hf, "bogus", "x"), "bogus"},
		{"trailing_content", `{"source":"human_finding"} {}`, "body"},
		{"bad_source", with(hf, "source", "acceptance"), "source"},
		{"bad_severity", with(hf, "severity", "critical"), "severity"},
		{"bad_category", with(hf, "category", "style"), "category"},
		{"missing_description", with(hf, "description", nil), "description"},
		{"blank_description", with(hf, "description", "  "), "description"},
		{"long_description", with(hf, "description", strings.Repeat("x", escapeMaxDescriptionRunes+1)), "description"},
		{"missing_files", with(hf, "files", nil), "files"},
		{"too_many_files", with(hf, "files", many), "files"},
		{"absolute_file", with(hf, "files", []string{"/etc/passwd"}), "files"},
		{"traversal_file", with(hf, "files", []string{"a/../../b"}), "files"},
		{"unclean_file", with(hf, "files", []string{"a//b"}), "files"},
		{"control_char_file", with(hf, "files", []string{"a\nb"}), "files"},
		{"backslash_file", with(hf, "files", []string{`a\b`}), "files"},
		{"bad_head_sha", with(hf, "head_sha", "ABC"), "head_sha"},
		{"too_many_commits", with(hf, "introducing_commits", commits), "introducing_commits"},
		{"bad_commit", with(hf, "introducing_commits", []string{"xyz"}), "introducing_commits"},
		{"missing_key", with(hf, "idempotency_key", nil), "idempotency_key"},
		{"bad_key", with(hf, "idempotency_key", "-leading"), "idempotency_key"},
		{"concern_on_other_source", with(hf, "concern_id", uuid.NewString()), "concern_id"},
		{"class_on_non_security", with(hf, "security_class", "authz_bypass"), "security_class"},
		{"advisory_on_non_security", with(hf, "advisory_id", escapeAdvisory), "advisory_id"},
		{"later_reviewer_key_refused", with(laterReviewerBody(uuid.New()), "idempotency_key", "k"), "idempotency_key"},
		{"later_reviewer_missing_concern", with(laterReviewerBody(uuid.New()), "concern_id", nil), "concern_id"},
		{"later_reviewer_bad_concern", with(laterReviewerBody(uuid.New()), "concern_id", "nope"), "concern_id"},
		{"security_missing_class", with(securityBody(), "security_class", nil), "security_class"},
		{"security_bad_class", with(securityBody(), "security_class", "Authz-Bypass"), "security_class"},
		{"security_missing_advisory", with(securityBody(), "advisory_id", nil), "advisory_id"},
		{"close_zero_sequence", map[string]any{"supersedes_sequence": 0, "attestation": map[string]any{"judgement": "defect"}}, "supersedes_sequence"},
		{"close_missing_attestation", map[string]any{"supersedes_sequence": 1}, "attestation"},
		{"close_bad_judgement", closeBody(1, "maybe"), "attestation.judgement"},
		{"close_long_note", map[string]any{"supersedes_sequence": 1, "attestation": map[string]any{"judgement": "defect", "note": strings.Repeat("n", escapeMaxNoteRunes+1)}}, "attestation.note"},
		{"close_extra_field", with(closeBody(1, "defect"), "source", "human_finding"), "source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), tc.body), 0, http.StatusBadRequest, "validation_failed")
			if got := eb.Details["field"]; got != tc.field {
				t.Errorf("details.field = %v, want %q", got, tc.field)
			}
		})
	}
	t.Run("bad_run_id", func(t *testing.T) {
		eb := wantRefused(t, fx, fx.postTo(t, escapeSessionID(), "not-a-uuid", humanFindingBody("k1")), 0, http.StatusBadRequest, "validation_failed")
		if eb.Details["field"] != "run_id" {
			t.Errorf("details.field = %v, want run_id", eb.Details["field"])
		}
	})
	t.Run("over_size_body", func(t *testing.T) {
		big := `{"description":"` + strings.Repeat("x", escapeMaxBodyBytes) + `"}`
		wantRefused(t, fx, fx.post(t, escapeSessionID(), big), 0, http.StatusBadRequest, "validation_failed")
	})
}

// R2 / CF-B15: an otherwise-valid security record plus security_finding_id is
// refused ONLY by strict decoding.
func TestRecordEscape_SecurityFindingIDRefused(t *testing.T) {
	fx := newEscapeFixture(t)
	body := securityBody()
	body["security_finding_id"] = "SF-1"
	eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusBadRequest, "validation_failed")
	if eb.Details["field"] != "security_finding_id" {
		t.Errorf("details.field = %v, want security_finding_id", eb.Details["field"])
	}
}

// R9b / CF-B13: sec-key-1 passes the general key charset and the record is
// otherwise valid; only the UUID rule refuses it.
func TestRecordEscape_SecurityIdempotencyKeyMustBeUUID(t *testing.T) {
	fx := newEscapeFixture(t)
	body := securityBody()
	body["idempotency_key"] = "sec-key-1"
	eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusBadRequest, "validation_failed")
	if eb.Details["field"] != "idempotency_key" {
		t.Errorf("details.field = %v, want idempotency_key", eb.Details["field"])
	}
	// An uppercase UUID is not canonical.
	body["idempotency_key"] = strings.ToUpper(escapeUUIDKey)
	wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusBadRequest, "validation_failed")
	// The same non-UUID key is accepted on a non-security escape.
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), humanFindingBody("sec-key-1")))
	// A canonical UUID is accepted on the security escape.
	body["idempotency_key"] = escapeUUIDKey
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), body))
}

// C1: a security non-later_reviewer record without a canonical UUID key is
// 400 validation_failed, field idempotency_key.
func TestRecordEscape_SecurityNonLaterReviewerRequiresUUIDKey(t *testing.T) {
	fx := newEscapeFixture(t)
	for name, key := range map[string]any{"absent": nil, "slug": "operator-1", "braced": "{" + escapeUUIDKey + "}"} {
		t.Run(name, func(t *testing.T) {
			body := securityBody()
			body["source"] = "operator_battery"
			if key == nil {
				delete(body, "idempotency_key")
			} else {
				body["idempotency_key"] = key
			}
			eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusBadRequest, "validation_failed")
			if eb.Details["field"] != "idempotency_key" {
				t.Errorf("details.field = %v, want idempotency_key", eb.Details["field"])
			}
		})
	}
}

func TestRecordEscape_RunNotFound404(t *testing.T) {
	fx := newEscapeFixture(t)
	wantRefused(t, fx, fx.postTo(t, escapeSessionID(), uuid.NewString(), humanFindingBody("k1")), 0, http.StatusNotFound, "run_not_found")
	wantRefused(t, fx, fx.postTo(t, escapeSessionID(), uuid.NewString(), closeBody(1, "defect")), 0, http.StatusNotFound, "run_not_found")
	fx.runs.getErr = errors.New("db down")
	wantRefused(t, fx, fx.post(t, escapeSessionID(), humanFindingBody("k1")), 0, http.StatusInternalServerError, "internal_error")
}

// CF-B5: a human static recorder of a human_finding is otherwise counted.
func TestRecordEscape_StaticTokenBirthPending(t *testing.T) {
	fx := newEscapeFixture(t)
	resp := decodeEscapeResponse(t, fx.post(t, escapeStaticID(), humanFindingBody("k1")))
	if resp.State != escapeStatePendingJudgement || resp.PendingReason != "credential_not_forge_verified" {
		t.Fatalf("response = %+v, want pending_judgement / credential_not_forge_verified", resp)
	}
	m := lastEscapePayload(t, fx)
	if m["state"] != escapeStatePendingJudgement || m["pending_reason"] != "credential_not_forge_verified" || m["agent_recorded"] != false {
		t.Errorf("payload = %v, want a pending, non-agent birth", m)
	}
	if rec := m["recorder"].(map[string]any); rec["credential_kind"] != "static" {
		t.Errorf("recorder = %v, want credential_kind static", rec)
	}
}

func TestRecordEscape_ForgeVerifiedBirthCounted(t *testing.T) {
	for name, id := range map[string]Identity{"session": escapeSessionID(), "oauth": escapeOAuthID()} {
		t.Run(name, func(t *testing.T) {
			fx := newEscapeFixture(t)
			resp := decodeEscapeResponse(t, fx.post(t, id, humanFindingBody("k1")))
			if resp.State != escapeStateCounted || resp.JudgementBasis != escapeBasisNotRequired || resp.PendingReason != "" {
				t.Fatalf("response = %+v, want counted / not_required", resp)
			}
			if resp.RecordKind != escapeRecordKindBirth || resp.SourceEventKey != "idempotency_key:k1" || resp.RunID != fx.run.ID.String() || resp.AuditSequence != 1 {
				t.Errorf("response = %+v, want a birth keyed idempotency_key:k1 at sequence 1", resp)
			}
			m := lastEscapePayload(t, fx)
			if m["escape_version"] != float64(1) || m["record_kind"] != "birth" || m["source_event_key"] != "idempotency_key:k1" {
				t.Errorf("payload = %v", m)
			}
			esc := escObj(t, m)
			if esc["description"] != "nil deref on an empty input" || esc["idempotency_key"] != "k1" || esc["reviewed_head"] != nil {
				t.Errorf("escape = %v, want the recorded fields and no reviewed_head", esc)
			}
			sub := esc["subject"].(map[string]any)
			if sub["pull_request_url"] != "https://github.com/acme/widgets/pull/7" || sub["head_sha"] != escapeHeadSHA || sub["pull_request_merged"] != false {
				t.Errorf("subject = %v", sub)
			}
		})
	}
}

func TestRecordEscape_AgentRecordedBirthPending(t *testing.T) {
	fx := newEscapeFixture(t)
	resp := decodeEscapeResponse(t, fx.post(t, escapeAgentID(), humanFindingBody("k1")))
	if resp.State != escapeStatePendingJudgement || resp.PendingReason != "agent_recorded" {
		t.Fatalf("agent subject: %+v, want pending / agent_recorded", resp)
	}
	// A delegated request from a forge-verified human is agent-recorded too.
	body := humanFindingBody("k2")
	body["delegated"] = true
	resp = decodeEscapeResponse(t, fx.post(t, escapeSessionID(), body))
	if resp.State != escapeStatePendingJudgement || resp.PendingReason != "agent_recorded" {
		t.Fatalf("delegated: %+v, want pending / agent_recorded", resp)
	}
	m := lastEscapePayload(t, fx)
	if m["agent_recorded"] != true || m["recorder"].(map[string]any)["delegated"] != true {
		t.Errorf("payload = %v, want agent_recorded + recorder.delegated", m)
	}
}

// CF-B6: a forge-verified human session with a valid concern is otherwise
// counted.
func TestRecordEscape_LaterReviewerBirthPending(t *testing.T) {
	fx := newEscapeFixture(t)
	c := fx.addConcern(fx.run.ID, 50)
	resp := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)))
	if resp.State != escapeStatePendingJudgement || resp.PendingReason != "review_finding_source" {
		t.Fatalf("response = %+v, want pending / review_finding_source", resp)
	}
	if resp.SourceEventKey != "concern:"+c.ID.String() {
		t.Errorf("source_event_key = %q, want concern:%s", resp.SourceEventKey, c.ID)
	}
	esc := escObj(t, lastEscapePayload(t, fx))
	ref := esc["concern"].(map[string]any)
	if ref["id"] != c.ID.String() || ref["run_id"] != fx.run.ID.String() || ref["stage_kind"] != "implement" ||
		ref["origin_review_sequence"] != float64(50) || ref["reviewer_model"] != "claude-fable-5-1" || ref["reviewer_role"] != concern.ReviewerRoleStandard {
		t.Errorf("concern binding = %v", ref)
	}
	if _, ok := esc["idempotency_key"]; ok {
		t.Errorf("a later_reviewer escape carries an idempotency_key: %v", esc)
	}
}

// R1 / CF-B11: two concerns of ONE verdict share an OriginReviewSequence, as
// persistReviewConcernsAs mints them; each is its own birth.
func TestRecordEscape_LaterReviewerDistinctConcernsSameRound(t *testing.T) {
	fx := newEscapeFixture(t)
	c1 := fx.addConcern(fx.run.ID, 50)
	c2 := fx.addConcern(fx.run.ID, 50)
	r1 := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c1.ID)))
	r2 := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c2.ID)))
	if r1.SourceEventKey == r2.SourceEventKey {
		t.Fatalf("both births keyed %q, want distinct concern:<id> keys", r1.SourceEventKey)
	}
	if n := len(fx.audit.escapeRows()); n != 2 {
		t.Errorf("escape rows = %d, want 2", n)
	}
}

func TestRecordEscape_LaterReviewerReplayDeduplicated(t *testing.T) {
	fx := newEscapeFixture(t)
	c := fx.addConcern(fx.run.ID, 50)
	first := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)))
	eb := wantRefused(t, fx, fx.post(t, escapeStaticID(), laterReviewerBody(c.ID)), 1, http.StatusConflict, "escape_already_recorded")
	if eb.Details["existing_sequence"] != float64(first.AuditSequence) {
		t.Errorf("existing_sequence = %v, want %d", eb.Details["existing_sequence"], first.AuditSequence)
	}
}

// R5 / CF-B12: the fake returns the WRAPPED concern.ErrNotFound like
// production.
func TestRecordEscape_ConcernFailClosed(t *testing.T) {
	t.Run("missing_concern", func(t *testing.T) {
		fx := newEscapeFixture(t)
		wantRefused(t, fx, fx.post(t, escapeSessionID(), laterReviewerBody(uuid.New())), 0, http.StatusUnprocessableEntity, "escape_concern_not_found")
	})
	t.Run("lookup_error", func(t *testing.T) {
		fx := newEscapeFixture(t)
		fx.concerns.getByIDsErr = errors.New("db down")
		wantRefused(t, fx, fx.post(t, escapeSessionID(), laterReviewerBody(uuid.New())), 0, http.StatusInternalServerError, "internal_error")
	})
	t.Run("missing_run", func(t *testing.T) {
		fx := newEscapeFixture(t)
		c := fx.addConcern(uuid.New(), 50)
		wantRefused(t, fx, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)), 0, http.StatusUnprocessableEntity, "escape_concern_not_found")
	})
	t.Run("cross_account", func(t *testing.T) {
		fx := newEscapeFixture(t)
		other := newEscapeRun("00000000-0000-0000-0000-0000000000bb")
		fx.runs.runs[other.ID] = other
		c := fx.addConcern(other.ID, 50)
		wantRefused(t, fx, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)), 0, http.StatusUnprocessableEntity, "escape_concern_not_found")
	})
	t.Run("same_account_other_run_accepted", func(t *testing.T) {
		fx := newEscapeFixture(t)
		other := newEscapeRun(testOperatorAccountID)
		fx.runs.runs[other.ID] = other
		c := fx.addConcern(other.ID, 50)
		decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)))
	})
}

// R3 / CF-B14: the concern's round carries a recorded reviewed_head, copied
// JSON-equal onto the birth and again onto its closure.
func TestRecordEscape_LaterReviewerCarriesRoundReviewedHead(t *testing.T) {
	fx := newEscapeFixture(t)
	c := fx.addConcern(fx.run.ID, 50)
	head := testReviewedHead()
	fx.seedRound(t, fx.run.ID, 40, 50, 40, head)
	birth := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)))
	want, _ := json.Marshal(head)
	var wantM any
	_ = json.Unmarshal(want, &wantM)
	esc := escObj(t, lastEscapePayload(t, fx))
	if !reflect.DeepEqual(esc["reviewed_head"], wantM) {
		t.Fatalf("reviewed_head = %v, want %v (JSON-equal)", esc["reviewed_head"], wantM)
	}
	if esc["concern"].(map[string]any)["review_round_sequence"] != float64(40) {
		t.Errorf("concern.review_round_sequence = %v, want 40", esc["concern"])
	}
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), closeBody(birth.AuditSequence, "defect")))
	if got := escObj(t, lastEscapePayload(t, fx))["reviewed_head"]; !reflect.DeepEqual(got, wantM) {
		t.Errorf("closure reviewed_head = %v, want the birth's %v", got, wantM)
	}
}

func TestRecordEscape_LaterReviewerReviewedHeadAbsent(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, fx *escapeFixture, c *concern.Concern)
	}{
		{"plan_round", func(t *testing.T, fx *escapeFixture, c *concern.Concern) {
			c.StageKind = concern.StageKindPlan
			fx.seedRound(t, fx.run.ID, 40, 50, 40, testReviewedHead())
		}},
		{"missing_verdict", func(t *testing.T, fx *escapeFixture, c *concern.Concern) {
			fx.seedRound(t, fx.run.ID, 40, 51, 40, testReviewedHead())
		}},
		{"zero_round_sequence", func(t *testing.T, fx *escapeFixture, c *concern.Concern) {
			fx.seedRound(t, fx.run.ID, 40, 50, 0, testReviewedHead())
		}},
		{"missing_started_row", func(t *testing.T, fx *escapeFixture, c *concern.Concern) {
			fx.seedRound(t, fx.run.ID, 41, 50, 40, testReviewedHead())
		}},
		{"pre_a17_started_row", func(t *testing.T, fx *escapeFixture, c *concern.Concern) {
			fx.seedRound(t, fx.run.ID, 40, 50, 40, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEscapeFixture(t)
			c := fx.addConcern(fx.run.ID, 50)
			tc.seed(t, fx, c)
			decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)))
			if esc := escObj(t, lastEscapePayload(t, fx)); esc["reviewed_head"] != nil {
				t.Errorf("reviewed_head = %v, want absent", esc["reviewed_head"])
			}
		})
	}
}

func TestRecordEscape_LaterReviewerReviewedHeadReadError500(t *testing.T) {
	for _, cat := range []string{"implement_reviewed", "implement_review_started"} {
		t.Run(cat, func(t *testing.T) {
			fx := newEscapeFixture(t)
			c := fx.addConcern(fx.run.ID, 50)
			fx.seedRound(t, fx.run.ID, 40, 50, 40, testReviewedHead())
			fx.audit.listByCategoryErrCategory = cat
			wantRefused(t, fx, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)), 0, http.StatusInternalServerError, "internal_error")
		})
	}
}

func TestRecordEscape_NonReviewSourceHasNoReviewedHead(t *testing.T) {
	fx := newEscapeFixture(t)
	fx.seedRound(t, fx.run.ID, 40, 50, 40, testReviewedHead())
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), humanFindingBody("k1")))
	esc := escObj(t, lastEscapePayload(t, fx))
	if esc["reviewed_head"] != nil || esc["concern"] != nil {
		t.Errorf("escape = %v, want no reviewed_head and no concern binding", esc)
	}
}

// C1: a security later_reviewer record carries no idempotency key and is
// born with reviewed_head; C3: only its scalar identity fields.
func TestRecordEscape_SecurityLaterReviewerBornWithReviewedHead(t *testing.T) {
	fx := newEscapeFixture(t)
	c := fx.addConcern(fx.run.ID, 50)
	fx.seedRound(t, fx.run.ID, 40, 50, 40, testReviewedHead())
	resp := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), securityLaterReviewerBody(c.ID)))
	if resp.SourceEventKey != "concern:"+c.ID.String() {
		t.Errorf("source_event_key = %q, want concern:%s", resp.SourceEventKey, c.ID)
	}
	esc := escObj(t, lastEscapePayload(t, fx))
	rh, ok := esc["reviewed_head"].(map[string]any)
	if !ok {
		t.Fatalf("security later_reviewer escape carries no reviewed_head: %v", esc)
	}
	h := testReviewedHead()
	want := map[string]any{"status": h.Status, "kind": h.Kind, "sha": h.SHA, "tree_sha": h.TreeSHA, "change_id": h.ChangeID, "origin": h.Origin}
	if !reflect.DeepEqual(rh, want) {
		t.Errorf("security reviewed_head = %v, want ONLY the scalar identity fields %v (C3)", rh, want)
	}
}

// C3: a NON-security later_reviewer escape keeps the full reviewed_head,
// path sets included.
func TestRecordEscape_NonSecurityLaterReviewerKeepsFullReviewedHead(t *testing.T) {
	fx := newEscapeFixture(t)
	c := fx.addConcern(fx.run.ID, 50)
	fx.seedRound(t, fx.run.ID, 40, 50, 40, testReviewedHead())
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), laterReviewerBody(c.ID)))
	rh := escObj(t, lastEscapePayload(t, fx))["reviewed_head"].(map[string]any)
	for _, k := range []string{"files", "partial_files", "omitted_files", "diff_mode"} {
		if _, ok := rh[k]; !ok {
			t.Errorf("non-security reviewed_head lacks %q: %v", k, rh)
		}
	}
}

func TestEscapeSecurityReviewedHead_Undecodable(t *testing.T) {
	if got := escapeSecurityReviewedHead(json.RawMessage(`[1]`)); got != nil {
		t.Errorf("undecodable head = %s, want dropped", got)
	}
	if got := escapeSecurityReviewedHead(nil); got != nil {
		t.Errorf("nil head = %s, want nil", got)
	}
}

func TestRecordEscape_SecurityLaterReviewerReplayDeduplicated(t *testing.T) {
	fx := newEscapeFixture(t)
	c := fx.addConcern(fx.run.ID, 50)
	first := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), securityLaterReviewerBody(c.ID)))
	eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), securityLaterReviewerBody(c.ID)), 1, http.StatusConflict, "escape_already_recorded")
	if eb.Details["existing_sequence"] != float64(first.AuditSequence) {
		t.Errorf("existing_sequence = %v, want %d", eb.Details["existing_sequence"], first.AuditSequence)
	}
}

func TestRecordEscape_BirthReplayDeduplicated(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeSessionID(), "k1")
	eb := wantRefused(t, fx, fx.post(t, escapeStaticID(), humanFindingBody("k1")), 1, http.StatusConflict, "escape_already_recorded")
	if eb.Details["existing_sequence"] != float64(seq) {
		t.Errorf("existing_sequence = %v, want %d", eb.Details["existing_sequence"], seq)
	}
	// A different key records.
	recordBirth(t, fx, escapeSessionID(), "k2")
}

func TestRecordEscape_SubjectUnresolved(t *testing.T) {
	fx := newEscapeFixture(t)
	body := humanFindingBody("k1")
	delete(body, "head_sha")
	wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusUnprocessableEntity, "escape_subject_unresolved")
	// The merge-state read failing is 500.
	fx.audit.listByCategoryErrCategory = CategoryPRMerged
	wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusInternalServerError, "internal_error")
	// An observably merged PR resolves the subject without a head.
	fx.audit.listByCategoryErrCategory = ""
	rid := fx.run.ID
	fx.audit.seeded = append(fx.audit.seeded, &audit.Entry{RunID: &rid, Sequence: 900, Category: CategoryPRMerged, Payload: json.RawMessage(`{}`)})
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), body))
	if sub := escObj(t, lastEscapePayload(t, fx))["subject"].(map[string]any); sub["pull_request_merged"] != true {
		t.Errorf("subject = %v, want pull_request_merged true", sub)
	}
}

// CF-B9: each subcase is a valid security record plus ONE free-text field.
func TestRecordEscape_SecurityClassOnly(t *testing.T) {
	fx := newEscapeFixture(t)
	for field, v := range map[string]any{
		"description":         "the token leaks",
		"files":               []string{"backend/internal/auth/x.go"},
		"introducing_commits": []string{escapeHeadSHA},
	} {
		t.Run(field, func(t *testing.T) {
			body := securityBody()
			body[field] = v
			eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusUnprocessableEntity, "escape_security_free_text_refused")
			if eb.Details["field"] != field {
				t.Errorf("details.field = %v, want %q", eb.Details["field"], field)
			}
		})
	}
	t.Run("empty_description_present", func(t *testing.T) {
		body := securityBody()
		body["description"] = ""
		wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 0, http.StatusUnprocessableEntity, "escape_security_free_text_refused")
	})
	t.Run("valid_security_record", func(t *testing.T) {
		decodeEscapeResponse(t, fx.post(t, escapeSessionID(), securityBody()))
		esc := escObj(t, lastEscapePayload(t, fx))
		keys := make([]string, 0, len(esc))
		for k := range esc {
			keys = append(keys, k)
		}
		for _, k := range keys {
			switch k {
			case "source", "severity", "category", "security", "subject", "idempotency_key":
			default:
				t.Errorf("security escape carries %q: %v", k, esc)
			}
		}
		if sec := esc["security"].(map[string]any); sec["class"] != "authz_bypass" || sec["advisory_id"] != escapeAdvisory {
			t.Errorf("security = %v", sec)
		}
	})
}

func TestRecordEscape_SecurityAdvisoryValidation(t *testing.T) {
	cases := []struct {
		name, runnerRef, advisory string
		ok                        bool
	}{
		{"github_real_ghsa", "", escapeAdvisory, true},
		{"github_lowercase_prefix", "", "ghsa-jfh8-c2jp-5v3q", false},
		{"github_cve_refused", "", "CVE-2026-0001", false},
		{"github_short_group", "", "GHSA-jfh8-c2jp-5v3", false},
		{"gitlab_opaque_accepted", "gitlab:5", "acme/widgets#12", true},
		{"gitlab_space_refused", "gitlab:5", "bad id", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEscapeFixture(t)
			if tc.runnerRef != "" {
				ref := tc.runnerRef
				fx.run.InstallationRef = &ref
			}
			body := securityBody()
			body["advisory_id"] = tc.advisory
			body["idempotency_key"] = uuid.NewString()
			rec := fx.post(t, escapeSessionID(), body)
			if tc.ok {
				decodeEscapeResponse(t, rec)
				return
			}
			eb := wantRefused(t, fx, rec, 0, http.StatusBadRequest, "validation_failed")
			if eb.Details["field"] != "advisory_id" {
				t.Errorf("case %d details.field = %v, want advisory_id", i, eb.Details["field"])
			}
		})
	}
}

// CF-B2: an operator-agent subject with an oauth credential, not delegated —
// only the agent arm refuses.
func TestRecordEscape_CloseRefusesAgentSubject(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeStaticID(), "k1")
	for name, id := range map[string]Identity{
		"operator_agent": escapeAgentID(),
		"first_officer":  {Subject: "first_officer/seat-1", TokenID: "tok-fo", Scopes: []string{"write:runs"}, AuthMethod: "oauth", AccountID: testOperatorAccountID},
	} {
		t.Run(name, func(t *testing.T) {
			eb := wantRefused(t, fx, fx.post(t, id, closeBody(seq, "defect")), 1, http.StatusForbidden, "escape_attestation_requires_human")
			if eb.Details["refused_actor"] != "agent_token" {
				t.Errorf("refused_actor = %v, want agent_token", eb.Details["refused_actor"])
			}
		})
	}
}

// CF-B3: a forge-verified human session marked delegated.
func TestRecordEscape_CloseRefusesDelegated(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeStaticID(), "k1")
	body := closeBody(seq, "defect")
	body["delegated"] = true
	eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), body), 1, http.StatusForbidden, "escape_attestation_requires_human")
	if eb.Details["refused_actor"] != "delegated" {
		t.Errorf("refused_actor = %v, want delegated", eb.Details["refused_actor"])
	}
}

// CF-B4: a human static token, not delegated.
func TestRecordEscape_CloseRefusesNonForgeCredential(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeStaticID(), "k1")
	for name, method := range map[string]string{"static": "static", "unrecorded": "", "unknown": "weird"} {
		t.Run(name, func(t *testing.T) {
			id := escapeStaticID()
			id.AuthMethod = method
			eb := wantRefused(t, fx, fx.post(t, id, closeBody(seq, "defect")), 1, http.StatusForbidden, "escape_attestation_requires_forge_credential")
			if eb.Details["credential_kind"] == nil || eb.Details["forge_verified_kinds"] == nil {
				t.Errorf("details = %v, want credential_kind + forge_verified_kinds", eb.Details)
			}
		})
	}
}

func TestRecordEscape_CloseCountsAndCopiesEscapeFields(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeStaticID(), "k1")
	birthEsc := escObj(t, lastEscapePayload(t, fx))
	body := closeBody(seq, "defect")
	body["attestation"].(map[string]any)["note"] = "confirmed on main"
	resp := decodeEscapeResponse(t, fx.post(t, escapeOAuthID(), body))
	if resp.State != escapeStateCounted || resp.RecordKind != escapeRecordKindClosure || resp.JudgementBasis != escapeBasisOperatorAttestation ||
		resp.SupersedesSequence != strconv.FormatInt(seq, 10) || resp.SourceEventKey != "" {
		t.Fatalf("response = %+v, want a counted closure superseding %d", resp, seq)
	}
	m := lastEscapePayload(t, fx)
	if m["supersedes_sequence"] != strconv.FormatInt(seq, 10) || m["record_kind"] != "closure" || m["state"] != "counted" {
		t.Errorf("closure payload = %v", m)
	}
	if !reflect.DeepEqual(escObj(t, m), birthEsc) {
		t.Errorf("closure escape = %v, want the birth's %v copied", escObj(t, m), birthEsc)
	}
	att := m["attestation"].(map[string]any)
	if att["judgement"] != "defect" || att["note"] != "confirmed on main" || att["actor_subject"] != "github:alice" || att["credential_kind"] != "oauth" {
		t.Errorf("attestation = %v", att)
	}
	if _, err := uuid.Parse(att["judgement_id"].(string)); err != nil {
		t.Errorf("judgement_id = %v, want a server UUID", att["judgement_id"])
	}
}

func TestRecordEscape_CloseDismissed(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeStaticID(), "k1")
	resp := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), closeBody(seq, "no_defect")))
	if resp.State != escapeStateDismissed {
		t.Fatalf("state = %q, want dismissed", resp.State)
	}
}

// CF-B8: a COUNTED birth with no successor is refused only by the pending
// check.
func TestRecordEscape_CloseTargetRefusals(t *testing.T) {
	t.Run("unknown_target", func(t *testing.T) {
		fx := newEscapeFixture(t)
		wantRefused(t, fx, fx.post(t, escapeSessionID(), closeBody(99, "defect")), 0, http.StatusUnprocessableEntity, "escape_target_not_found")
	})
	t.Run("cross_run_target", func(t *testing.T) {
		fx := newEscapeFixture(t)
		other := newEscapeRun(testOperatorAccountID)
		fx.runs.runs[other.ID] = other
		seq := recordBirth(t, fx, escapeStaticID(), "k1")
		wantRefused(t, fx, fx.postTo(t, escapeSessionID(), other.ID.String(), closeBody(seq, "defect")), 1, http.StatusUnprocessableEntity, "escape_target_not_found")
	})
	t.Run("counted_birth", func(t *testing.T) {
		fx := newEscapeFixture(t)
		seq := recordBirth(t, fx, escapeSessionID(), "k1") // counted at birth
		wantRefused(t, fx, fx.post(t, escapeSessionID(), closeBody(seq, "defect")), 1, http.StatusUnprocessableEntity, "escape_target_not_pending")
	})
	t.Run("closure_row_target", func(t *testing.T) {
		fx := newEscapeFixture(t)
		seq := recordBirth(t, fx, escapeStaticID(), "k1")
		closure := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), closeBody(seq, "defect")))
		wantRefused(t, fx, fx.post(t, escapeSessionID(), closeBody(closure.AuditSequence, "defect")), 2, http.StatusUnprocessableEntity, "escape_target_not_pending")
	})
	t.Run("list_error", func(t *testing.T) {
		fx := newEscapeFixture(t)
		seq := recordBirth(t, fx, escapeStaticID(), "k1")
		fx.audit.listByCategoryErrCategory = CategoryEscapeRecorded
		wantRefused(t, fx, fx.post(t, escapeSessionID(), closeBody(seq, "defect")), 1, http.StatusInternalServerError, "internal_error")
	})
	t.Run("append_error", func(t *testing.T) {
		fx := newEscapeFixture(t)
		seq := recordBirth(t, fx, escapeStaticID(), "k1")
		fx.audit.appendErrCategory = CategoryEscapeRecorded
		wantRefused(t, fx, fx.post(t, escapeSessionID(), closeBody(seq, "defect")), 1, http.StatusInternalServerError, "internal_error")
		wantRefused(t, fx, fx.post(t, escapeSessionID(), humanFindingBody("k2")), 1, http.StatusInternalServerError, "internal_error")
	})
}

func TestRecordEscape_CloseAlreadyClosed409(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeStaticID(), "k1")
	closure := decodeEscapeResponse(t, fx.post(t, escapeSessionID(), closeBody(seq, "defect")))
	eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), closeBody(seq, "no_defect")), 2, http.StatusConflict, "escape_already_closed")
	if eb.Details["successor_sequence"] != float64(closure.AuditSequence) {
		t.Errorf("successor_sequence = %v, want %d", eb.Details["successor_sequence"], closure.AuditSequence)
	}
}

func TestRecordEscape_SecurityCloseNoteRefused(t *testing.T) {
	fx := newEscapeFixture(t)
	body := securityBody()
	seq := decodeEscapeResponse(t, fx.post(t, escapeStaticID(), body)).AuditSequence
	cb := closeBody(seq, "defect")
	cb["attestation"].(map[string]any)["note"] = "the leak is in x.go"
	eb := wantRefused(t, fx, fx.post(t, escapeSessionID(), cb), 1, http.StatusUnprocessableEntity, "escape_security_free_text_refused")
	if eb.Details["field"] != "attestation.note" {
		t.Errorf("details.field = %v, want attestation.note", eb.Details["field"])
	}
	// Without the note the security close lands.
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), closeBody(seq, "defect")))
}

// Run state never gates a close (A1.2).
func TestRecordEscape_CloseOnTerminalRun(t *testing.T) {
	fx := newEscapeFixture(t)
	seq := recordBirth(t, fx, escapeStaticID(), "k1")
	fx.run.State = run.StateSucceeded
	decodeEscapeResponse(t, fx.post(t, escapeSessionID(), closeBody(seq, "defect")))
	fx.run.State = run.StateFailed
	recordBirth(t, fx, escapeStaticID(), "k2")
}

func TestRecordEscape_AuditActorFields(t *testing.T) {
	fx := newEscapeFixture(t)
	recordBirth(t, fx, escapeAgentID(), "k1")
	recordBirth(t, fx, escapeSessionID(), "k2")
	rows := fx.audit.escapeRows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for i, want := range []struct{ kind, subject string }{{"agent", "operator-agent/v1"}, {"user", "github:alice"}} {
		p := rows[i]
		if p.StageID != nil {
			t.Errorf("row %d StageID = %v, want nil (stage-less)", i, p.StageID)
		}
		if p.ActorKind == nil || string(*p.ActorKind) != want.kind || p.ActorSubject == nil || *p.ActorSubject != want.subject {
			t.Errorf("row %d actor = %v/%v, want %s/%s", i, p.ActorKind, p.ActorSubject, want.kind, want.subject)
		}
		if p.RunID != fx.run.ID {
			t.Errorf("row %d RunID = %s, want %s", i, p.RunID, fx.run.ID)
		}
	}
}

func TestEscapeValidRepoPath(t *testing.T) {
	for p, want := range map[string]bool{
		"a/b.go": true, "README.md": true, "": false, ".": false, "a/./b": false, "../a": false,
		"a/": false, strings.Repeat("a", escapeMaxPathBytes+1): false,
	} {
		if got := escapeValidRepoPath(p); got != want {
			t.Errorf("escapeValidRepoPath(%q) = %v, want %v", p, got, want)
		}
	}
}
