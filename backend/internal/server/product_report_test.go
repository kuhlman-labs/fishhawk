package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fakeFeedbackProvider is a workmgmt.FeedbackProvider double: it records
// what the handler dispatched (the conventions -> provider seam) and
// returns a configured search hit / canned create result.
type fakeFeedbackProvider struct {
	name string

	searchHit *workmgmt.ExistingReport
	searchErr error

	filed       bool
	filedReport workmgmt.FeedbackReport
	fileErr     error

	occurrenceNumber int
	occurrenceNote   string
	appendErr        error
}

func (f *fakeFeedbackProvider) Name() string { return f.name }

func (f *fakeFeedbackProvider) SearchOpenByFingerprint(_ context.Context, _ workmgmt.Target, _ string) (*workmgmt.ExistingReport, error) {
	return f.searchHit, f.searchErr
}

func (f *fakeFeedbackProvider) File(_ context.Context, _ workmgmt.Target, report workmgmt.FeedbackReport) (*workmgmt.CreatedItem, error) {
	f.filed = true
	f.filedReport = report
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	return &workmgmt.CreatedItem{
		Provider: f.name,
		Number:   4242,
		URL:      "https://github.com/kuhlman-labs/fishhawk/issues/4242",
	}, nil
}

func (f *fakeFeedbackProvider) AppendOccurrence(_ context.Context, _ workmgmt.Target, number int, note string) error {
	f.occurrenceNumber = number
	f.occurrenceNote = note
	return f.appendErr
}

func registerFakeFeedback(t *testing.T, p *fakeFeedbackProvider) {
	t.Helper()
	if p.name == "" {
		p.name = workmgmt.Default().Provider
	}
	workmgmt.RegisterFeedback(p)
}

// productReportFixture builds a server + run with a failing implement
// stage and a couple of audit entries, and returns the run id.
func productReportFixture(t *testing.T, fp *fakeFeedbackProvider, af *scAuditFake) (*Server, uuid.UUID) {
	t.Helper()
	registerFakeFeedback(t, fp)
	runID := uuid.New()
	failID := uuid.New()
	inst := int64(99)
	stored := &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "specsha123",
		RunnerKind:     run.RunnerKindLocal,
		State:          run.StateFailed,
		InstallationID: &inst,
	}
	stages := []*run.Stage{
		{ID: uuid.New(), Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{
			ID:              failID,
			Sequence:        1,
			Type:            run.StageTypeImplement,
			State:           run.StageStateFailed,
			FailureCategory: failureCat(run.FailureB),
			FailureReason:   strPtr("SENSITIVE free text that must not leak"),
		},
	}
	if af.allEntries == nil {
		af.allEntries = []*audit.Entry{
			{Sequence: 100, Category: "stage_dispatched"},
			{Sequence: 101, StageID: &failID, Category: "policy_evaluated"},
		}
	}
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   &statusCommentRunRepo{stored: stored, stages: stages},
		AuditRepo: af,
	})
	return s, runID
}

func postProductReport(s *Server, runID uuid.UUID, subject, body string) *httptest.ResponseRecorder {
	return postProductReportAs(s, runID, Identity{Subject: subject}, body)
}

// postProductReportAs drives the handler with the FULL caller identity
// (Subject + TokenID + Scopes), so the per-arm entitlement tests can model
// a run-bound agent token, an operator/operator-agent bearer, or a cookie
// session. postProductReport re-expresses the common Subject-only case in
// terms of it so the existing happy-path tests keep compiling unchanged.
func postProductReportAs(s *Server, runID uuid.UUID, id Identity, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/product-reports", bytes.NewReader([]byte(body)))
	req.SetPathValue("run_id", runID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	rec := httptest.NewRecorder()
	s.handleFileProductReport(rec, req)
	return rec
}

// TestProductReport_DedupMiss_CreatesMarkedReport drives the full egress
// seam on a dedup miss: collect -> fingerprint -> search (miss) -> File ->
// product_report_filed audit. It also asserts the redaction boundary:
// the failing stage's free text never reaches the provider.
func TestProductReport_DedupMiss_CreatesMarkedReport(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp productReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Action != "created" || resp.Number != 4242 {
		t.Errorf("response = %+v, want action=created number=4242", resp)
	}
	if resp.Fingerprint == "" {
		t.Error("response missing fingerprint")
	}
	if resp.Destination != productRepo {
		t.Errorf("destination = %q, want %q", resp.Destination, productRepo)
	}

	if !fp.filed {
		t.Fatal("provider.File was not called on a dedup miss")
	}
	if fp.filedReport.Fingerprint != resp.Fingerprint {
		t.Errorf("filed fingerprint %q != response %q", fp.filedReport.Fingerprint, resp.Fingerprint)
	}
	// Redaction boundary: no free text crosses by default.
	if strings.Contains(fp.filedReport.Body, "SENSITIVE") || strings.Contains(fp.filedReport.Title, "SENSITIVE") {
		t.Errorf("free text leaked into the report: title=%q body=%q", fp.filedReport.Title, fp.filedReport.Body)
	}

	// Audit seam: a product_report_filed entry naming what left the boundary.
	assertProductReportAudit(t, af, runID, resp.Fingerprint, "created")
}

// TestProductReport_DedupHit_AppendsOccurrence asserts a fingerprint hit
// appends an occurrence comment and creates nothing.
func TestProductReport_DedupHit_AppendsOccurrence(t *testing.T) {
	fp := &fakeFeedbackProvider{searchHit: &workmgmt.ExistingReport{
		Number: 7, URL: "https://github.com/kuhlman-labs/fishhawk/issues/7",
	}}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp productReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Action != "occurrence" || resp.Number != 7 {
		t.Errorf("response = %+v, want action=occurrence number=7", resp)
	}
	if fp.filed {
		t.Error("provider.File was called on a dedup hit; want nothing created")
	}
	if fp.occurrenceNumber != 7 {
		t.Errorf("occurrence appended to #%d, want #7", fp.occurrenceNumber)
	}
	if strings.Contains(fp.occurrenceNote, "SENSITIVE") {
		t.Errorf("free text leaked into occurrence comment: %q", fp.occurrenceNote)
	}
	assertProductReportAudit(t, af, runID, resp.Fingerprint, "occurrence")
}

// TestProductReport_KillSwitch_Returns403 asserts the per-repo kill-switch
// returns 403 and files nothing.
func TestProductReport_KillSwitch_Returns403(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) {
		c := workmgmt.Default()
		c.ProductFeedback = &workmgmt.ProductFeedback{Enabled: false}
		return c, nil
	}
	defer func() { conventionsLoader = prev }()

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "product_feedback_disabled") {
		t.Errorf("body = %s, want product_feedback_disabled", rec.Body.String())
	}
	if fp.filed || fp.occurrenceNumber != 0 {
		t.Error("kill-switch must file nothing")
	}
	for _, p := range af.appendedParams {
		if p.Category == categoryProductReportFiled {
			t.Error("kill-switch must not write a product_report_filed audit")
		}
	}
}

// TestProductReport_OwnRunBoundToken_NoWriteRuns_Created is the REGRESSION
// test for #1274 (binding condition 1): the run's OWN run-bound token, with
// TokenID set and scopes WITHOUT write:runs (run-bound tokens carry
// mcp:read, never write:runs), filing for its own run must be admitted
// (201) — NOT rejected as insufficient_scope. This is the exact branch the
// rejected fall-through gate broke. The run-bound arm is terminal, so the
// write:runs check never runs for it.
func TestProductReport_OwnRunBoundToken_NoWriteRuns_Created(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	id := Identity{
		Subject: "mcp:run:" + runID.String(),
		TokenID: "fhm_token_id",
		Scopes:  []string{"mcp:read"},
	}
	rec := postProductReportAs(s, runID, id, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Error("the run's own run-bound token must reach the provider")
	}
}

// TestProductReport_ForeignRunBoundToken_Returns403 asserts a run-bound
// token bound to a DIFFERENT run cannot drive an egress on this run's chain
// (run_not_entitled), even when the run-bound arm does not require
// write:runs.
func TestProductReport_ForeignRunBoundToken_Returns403(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	id := Identity{
		Subject: "mcp:run:" + uuid.New().String(),
		TokenID: "fhm_token_id",
		Scopes:  []string{"mcp:read"},
	}
	rec := postProductReportAs(s, runID, id, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "run_not_entitled") {
		t.Errorf("body = %s, want run_not_entitled", rec.Body.String())
	}
	if fp.filed {
		t.Error("a foreign run-bound caller must not reach the provider")
	}
}

// TestProductReport_OperatorAgentToken_AnyRun_Created asserts an
// operator-agent bearer token (operatorrole subject prefix, write:runs,
// TokenID set) may file for the run (operator scope = deployment), and the
// product_report_filed audit records actor_kind=agent for that subject
// (binding condition 2) — with no actor.go change.
func TestProductReport_OperatorAgentToken_AnyRun_Created(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	id := Identity{
		Subject: operatorrole.TokenSubjectPrefix + "operator-role-v0",
		TokenID: "uat_operator_agent",
		Scopes:  []string{"write:runs"},
	}
	rec := postProductReportAs(s, runID, id, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Fatal("operator-agent token must reach the provider")
	}
	// Audit names the operator-agent actor as actor_kind=agent.
	var found bool
	for _, p := range af.appendedParams {
		if p.Category != categoryProductReportFiled {
			continue
		}
		found = true
		if p.ActorKind == nil || *p.ActorKind != audit.ActorAgent {
			t.Errorf("audit ActorKind = %v, want %v", p.ActorKind, audit.ActorAgent)
		}
		if p.ActorSubject == nil || *p.ActorSubject != id.Subject {
			t.Errorf("audit ActorSubject = %v, want %s", p.ActorSubject, id.Subject)
		}
	}
	if !found {
		t.Error("no product_report_filed audit entry for the operator-agent caller")
	}
}

// TestProductReport_OperatorSession_Created asserts a cookie-session
// operator (empty TokenID) is admitted (201) by the default switch arm.
func TestProductReport_OperatorSession_Created(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	id := Identity{Subject: "github:operator"} // empty TokenID -> cookie session
	rec := postProductReportAs(s, runID, id, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Error("a cookie-session operator must reach the provider")
	}
}

// TestProductReport_OperatorBearer_MissingWriteRuns_Returns403 asserts a
// non-run-bound bearer token without write:runs is rejected with
// insufficient_scope and a required_scope detail.
func TestProductReport_OperatorBearer_MissingWriteRuns_Returns403(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	id := Identity{
		Subject: "github:operator",
		TokenID: "uat_no_write_runs",
		Scopes:  []string{}, // no write:runs
	}
	rec := postProductReportAs(s, runID, id, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_scope") {
		t.Errorf("body = %s, want insufficient_scope", rec.Body.String())
	}
	var resp struct {
		Error struct {
			Details struct {
				RequiredScope string `json:"required_scope"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if resp.Error.Details.RequiredScope != "write:runs" {
		t.Errorf("required_scope = %q, want write:runs", resp.Error.Details.RequiredScope)
	}
	if fp.filed {
		t.Error("a bearer without write:runs must not reach the provider")
	}
}

// TestProductReport_Anonymous_Returns401 asserts an unauthenticated caller
// is rejected before the entitlement switch.
func TestProductReport_Anonymous_Returns401(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	rec := postProductReportAs(s, runID, Identity{}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required", rec.Body.String())
	}
	if fp.filed {
		t.Error("an anonymous caller must not reach the provider")
	}
}

// TestProductReport_OperatorDedupHit_AppendsOccurrence asserts an operator
// caller on a fingerprint hit appends an occurrence (201) and creates
// nothing — caller identity does not change the dedup behavior.
func TestProductReport_OperatorDedupHit_AppendsOccurrence(t *testing.T) {
	fp := &fakeFeedbackProvider{searchHit: &workmgmt.ExistingReport{
		Number: 11, URL: "https://github.com/kuhlman-labs/fishhawk/issues/11",
	}}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	id := Identity{
		Subject: operatorrole.TokenSubjectPrefix + "operator-role-v0",
		TokenID: "uat_operator_agent",
		Scopes:  []string{"write:runs"},
	}
	rec := postProductReportAs(s, runID, id, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp productReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Action != "occurrence" || resp.Number != 11 {
		t.Errorf("response = %+v, want action=occurrence number=11", resp)
	}
	if fp.filed {
		t.Error("provider.File was called on a dedup hit; want nothing created")
	}
	if fp.occurrenceNumber != 11 {
		t.Errorf("occurrence appended to #%d, want #11", fp.occurrenceNumber)
	}
}

// TestProductReport_FreeText_RedactedOnConsent is the cross-boundary
// assertion for binding condition (5): a consented free-text description
// reaches the real handler, is run through the shared redaction module, and
// crosses into the filed report — with embedded secrets scrubbed. This is
// the seam the per-layer MCP/CLI tests cannot prove: their fake backends
// decode without DisallowUnknownFields, so they accept the body the real
// server would otherwise reject.
func TestProductReport_FreeText_RedactedOnConsent(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	// A description carrying a real GitHub PAT shape (36 chars after ghp_)
	// plus prose. With consent, the prose must cross and the token must be
	// redacted.
	secret := "ghp_" + strings.Repeat("A", 36)
	body := `{"include_free_text":true,"description":"Tried to file an issue but it hung; my token was ` + secret + `"}`
	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Fatal("provider.File was not called")
	}
	// The consented prose crossed the boundary.
	if !strings.Contains(fp.filedReport.Body, "Tried to file an issue but it hung") {
		t.Errorf("consented free text did not cross into the report body: %q", fp.filedReport.Body)
	}
	// The embedded secret was redacted server-side before it crossed.
	if strings.Contains(fp.filedReport.Body, secret) {
		t.Errorf("raw secret leaked into the report body: %q", fp.filedReport.Body)
	}
	if !strings.Contains(fp.filedReport.Body, "[REDACTED:github-pat-classic]") {
		t.Errorf("expected the secret to be replaced by a redaction marker; body=%q", fp.filedReport.Body)
	}
}

// TestProductReport_FreeText_AbsentWithoutConsent asserts that a
// description supplied WITHOUT include_free_text never crosses the
// boundary — the default is product facts only (binding conditions 1 & 2).
func TestProductReport_FreeText_AbsentWithoutConsent(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	body := `{"description":"this prose must NOT cross without consent"}`
	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Fatal("provider.File was not called")
	}
	if strings.Contains(fp.filedReport.Body, "this prose must NOT cross") {
		t.Errorf("free text crossed the boundary without consent: %q", fp.filedReport.Body)
	}
}

func assertProductReportAudit(t *testing.T, af *scAuditFake, runID uuid.UUID, fingerprint, action string) {
	t.Helper()
	var found bool
	for _, p := range af.appendedParams {
		if p.Category != categoryProductReportFiled {
			continue
		}
		found = true
		if p.RunID != runID {
			t.Errorf("audit RunID = %s, want %s", p.RunID, runID)
		}
		var payload map[string]any
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			t.Fatalf("audit payload: %v", err)
		}
		if payload["fingerprint"] != fingerprint {
			t.Errorf("audit fingerprint = %v, want %s", payload["fingerprint"], fingerprint)
		}
		if payload["destination"] != productRepo {
			t.Errorf("audit destination = %v, want %s", payload["destination"], productRepo)
		}
		if payload["action"] != action {
			t.Errorf("audit action = %v, want %s", payload["action"], action)
		}
	}
	if !found {
		t.Errorf("no product_report_filed audit entry; appended=%d", len(af.appendedParams))
	}
}

// TestReportLabels_IncludeAutonomyDefault is the reportLabels unit assertion
// for #1616 (verification 9): the product-report path bypasses workmgmt.Apply,
// so the autonomy:medium default is applied at the label source here — for BOTH
// kinds — so no product report is ever filed autonomy-unset.
func TestReportLabels_IncludeAutonomyDefault(t *testing.T) {
	for _, kind := range []string{"feature", "bug"} {
		labels := reportLabels(kind)
		var hasType, hasAutonomy bool
		for _, l := range labels {
			if l == "type:"+kind {
				hasType = true
			}
			if l == "autonomy:medium" {
				hasAutonomy = true
			}
		}
		if !hasType {
			t.Errorf("reportLabels(%q) = %v, want it to include type:%s", kind, labels, kind)
		}
		if !hasAutonomy {
			t.Errorf("reportLabels(%q) = %v, want it to include autonomy:medium", kind, labels)
		}
	}
}

// TestProductReport_FiledLabelsIncludeAutonomy is the provider-capture
// assertion for #1616 (verification 9): the labels that actually reach the
// feedback provider on a dedup-miss File include autonomy:medium, for both a
// default (bug) and an explicit feature report.
func TestProductReport_FiledLabelsIncludeAutonomy(t *testing.T) {
	cases := map[string]string{
		"bug default": "",
		"feature":     `{"kind":"feature"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fp := &fakeFeedbackProvider{}
			af := &scAuditFake{}
			s, runID := productReportFixture(t, fp, af)

			rec := postProductReport(s, runID, "mcp:run:"+runID.String(), body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}
			if !fp.filed {
				t.Fatal("provider.File was not called on a dedup miss")
			}
			var hasAutonomy bool
			for _, l := range fp.filedReport.Labels {
				if l == "autonomy:medium" {
					hasAutonomy = true
				}
			}
			if !hasAutonomy {
				t.Errorf("filed report labels = %v, want it to include autonomy:medium", fp.filedReport.Labels)
			}
		})
	}
}
