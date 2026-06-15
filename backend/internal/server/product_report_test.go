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
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/product-reports", bytes.NewReader([]byte(body)))
	req.SetPathValue("run_id", runID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: subject}))
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

// TestProductReport_ForeignToken_Returns403 asserts a token bound to a
// different run (or unbound) cannot drive an egress on this run's chain.
func TestProductReport_ForeignToken_Returns403(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	// Operator token: not run-bound at all.
	if rec := postProductReport(s, runID, "github:operator", ""); rec.Code != http.StatusForbidden {
		t.Errorf("operator token status = %d, want 403", rec.Code)
	}
	// A different run's run-bound token.
	if rec := postProductReport(s, runID, "mcp:run:"+uuid.New().String(), ""); rec.Code != http.StatusForbidden {
		t.Errorf("foreign run-bound token status = %d, want 403", rec.Code)
	}
	if fp.filed {
		t.Error("an unentitled caller must not reach the provider")
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
