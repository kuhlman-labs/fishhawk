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
	"github.com/kuhlman-labs/fishhawk/backend/internal/diagnostics"
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

	// Board-placement doubles (#1737). filedTarget records the Target the
	// handler built so the Project wiring is assertable; boardErr /
	// boardOK drive the two placement arms.
	filedTarget workmgmt.Target
	boardErr    string
	boardOK     bool
}

func (f *fakeFeedbackProvider) Name() string { return f.name }

func (f *fakeFeedbackProvider) SearchOpenByFingerprint(_ context.Context, _ workmgmt.Target, _ string) (*workmgmt.ExistingReport, error) {
	return f.searchHit, f.searchErr
}

func (f *fakeFeedbackProvider) File(_ context.Context, target workmgmt.Target, report workmgmt.FeedbackReport) (*workmgmt.CreatedItem, error) {
	f.filed = true
	f.filedReport = report
	f.filedTarget = target
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	// Mirror the real provider's nil-project contract: with no project on
	// the Target there is nothing to place, so Boarded is false whatever
	// the double was configured with. BoardingError is left alone so a
	// test can still model a provider that records a stale cause it had
	// no business recording.
	boarded := f.boardOK && target.Project != nil
	return &workmgmt.CreatedItem{
		Provider:      f.name,
		Number:        4242,
		URL:           "https://github.com/kuhlman-labs/fishhawk/issues/4242",
		Status:        report.BoardPlacement.Status,
		Boarded:       boarded,
		BoardingError: f.boardErr,
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

// TestProductReport_UnknownFeedbackProvider_501Redacts drives the
// provider_unimplemented branch (product_report.go): the repo's conventions
// name a feedback provider that is not registered, so workmgmt.GetFeedback
// returns an *UnknownProviderError. Post-#2587 the branch passes a STATIC
// literal message (never unk.Error(), a raw-cause syntax) while the
// product-owned facts ride the allow-listed provider/registered detail keys.
// Asserts the 501 code, the static message, the allow-listed details, and that
// no raw cause string leaks.
func TestProductReport_UnknownFeedbackProvider_501Redacts(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		c := workmgmt.Default()
		c.Provider = "no-such-feedback-provider"
		c.ProductFeedback = &workmgmt.ProductFeedback{Enabled: true}
		return c, nil
	}
	defer func() { conventionsLoader = prev }()

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "provider_unimplemented" {
		t.Errorf("code = %q, want provider_unimplemented", env.Error.Code)
	}
	if env.Error.Message != "the resolved feedback provider is not implemented" {
		t.Errorf("message = %q, want the static literal", env.Error.Message)
	}
	if env.Error.Details["provider"] != "no-such-feedback-provider" {
		t.Errorf("details.provider = %v, want the allow-listed provider id", env.Error.Details["provider"])
	}
	if _, ok := env.Error.Details["registered"]; !ok {
		t.Errorf("details missing the allow-listed registered key: %v", env.Error.Details)
	}
	// No raw provider-error text (e.g. "unknown provider") may ride the message.
	if strings.Contains(env.Error.Message, "unknown") {
		t.Errorf("static message must not embed the raw provider error: %q", env.Error.Message)
	}
	if fp.filed {
		t.Error("provider_unimplemented must file nothing")
	}
}

// TestProductReport_KillSwitch_Returns403 asserts the per-repo kill-switch
// returns 403 and files nothing.
func TestProductReport_KillSwitch_Returns403(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
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

// dedupFeedbackProvider is a fingerprint-AWARE feedback double: unlike
// fakeFeedbackProvider (which returns a fixed search result), it models a
// real dedup store — File records the fingerprint -> issue number, and
// SearchOpenByFingerprint returns a hit only for a fingerprint already
// filed. This lets one provider instance be driven across several POSTs
// to observe created-vs-occurrence keyed on the actual fingerprint.
type dedupFeedbackProvider struct {
	name  string
	filed map[string]int // fingerprint -> upstream number
	next  int
}

func (f *dedupFeedbackProvider) Name() string { return f.name }

func (f *dedupFeedbackProvider) SearchOpenByFingerprint(_ context.Context, _ workmgmt.Target, fingerprint string) (*workmgmt.ExistingReport, error) {
	if num, ok := f.filed[fingerprint]; ok {
		return &workmgmt.ExistingReport{Number: num, URL: "https://github.com/kuhlman-labs/fishhawk/issues/" + fingerprint}, nil
	}
	return nil, nil
}

func (f *dedupFeedbackProvider) File(_ context.Context, _ workmgmt.Target, report workmgmt.FeedbackReport) (*workmgmt.CreatedItem, error) {
	if f.filed == nil {
		f.filed = map[string]int{}
	}
	f.next++
	f.filed[report.Fingerprint] = f.next
	return &workmgmt.CreatedItem{Provider: f.name, Number: f.next, URL: "https://github.com/kuhlman-labs/fishhawk/issues/x"}, nil
}

func (f *dedupFeedbackProvider) AppendOccurrence(_ context.Context, _ workmgmt.Target, _ int, _ string) error {
	return nil
}

// fingerprintFixture builds a server + failing run whose failing stage
// carries the given category, free-text reason, and audit surface, so a
// test can vary exactly the fingerprint inputs (#1962). It does NOT
// register a provider — the caller registers a shared dedup provider so a
// single store is observed across POSTs.
func fingerprintFixture(t *testing.T, af *scAuditFake, category run.FailureCategory, reason, surface string) (*Server, uuid.UUID) {
	t.Helper()
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
			FailureCategory: failureCat(category),
			FailureReason:   strPtr(reason),
		},
	}
	af.allEntries = []*audit.Entry{
		{Sequence: 100, Category: "stage_dispatched"},
		{Sequence: 101, StageID: &failID, Category: surface},
	}
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   &statusCommentRunRepo{stored: stored, stages: stages},
		AuditRepo: af,
	})
	return s, runID
}

// TestProductReport_DetailClass_SplitsConflatedSurface is the #1962/#1933
// vs #1932 regression at the handler layer: two runs whose failing stage
// shares stage type, category C, and the fixup_base_checkout surface but
// whose FailureReason texts carry the credential-401 shape vs the bad-ref
// shape produce DIFFERENT fingerprints and BOTH take the created path
// (dedup miss against each other). A third run repeating the first run's
// reason CLASS (a different auth-401 text) reproduces the first fingerprint
// and takes the occurrence path. This exercises Collect ->
// ClassifyFailureDetail -> Fingerprint -> dedup search -> render end to end.
func TestProductReport_DetailClass_SplitsConflatedSurface(t *testing.T) {
	dp := &dedupFeedbackProvider{name: workmgmt.Default().Provider}
	// Save and restore the prior registration so this global mutation does
	// not leak into a product-report test that runs afterward in this
	// package and relies on its own previously-registered provider.
	if prev, err := workmgmt.GetFeedback(dp.name); err == nil {
		t.Cleanup(func() { workmgmt.RegisterFeedback(prev) })
	}
	workmgmt.RegisterFeedback(dp)

	const surface = "fixup_base_checkout"

	// Run 1: credential-401 shape -> auth-401.
	af1 := &scAuditFake{}
	s1, run1 := fingerprintFixture(t, af1, run.FailureC,
		"fatal: unable to access 'https://github.com/kuhlman-labs/fishhawk/': The requested URL returned error: 401",
		surface)
	rec1 := postProductReport(s1, run1, "mcp:run:"+run1.String(), "")
	resp1 := decodeProductReportResp(t, rec1)
	if resp1.Action != "created" {
		t.Fatalf("run1 action = %q, want created (body=%s)", resp1.Action, rec1.Body.String())
	}

	// Run 2: bad-ref shape -> bad-object-ref. Same surface + category, so
	// pre-#1962 it would have deduped onto run1; now it must split.
	af2 := &scAuditFake{}
	s2, run2 := fingerprintFixture(t, af2, run.FailureC,
		"fatal: couldn't find remote ref refs/heads/fishhawk/run-xyz",
		surface)
	rec2 := postProductReport(s2, run2, "mcp:run:"+run2.String(), "")
	resp2 := decodeProductReportResp(t, rec2)
	if resp2.Action != "created" {
		t.Fatalf("run2 action = %q, want created (body=%s)", resp2.Action, rec2.Body.String())
	}
	if resp1.Fingerprint == resp2.Fingerprint {
		t.Fatalf("distinct root causes on a shared surface conflated: both %q", resp1.Fingerprint)
	}

	// Run 3: a DIFFERENT auth-401 text -> same class as run1 -> same
	// fingerprint -> occurrence (dedup hit), proving the class (not the raw
	// text) is what keys the fingerprint.
	af3 := &scAuditFake{}
	s3, run3 := fingerprintFixture(t, af3, run.FailureC,
		"remote: Authentication failed for 'https://github.com/kuhlman-labs/fishhawk/'",
		surface)
	rec3 := postProductReport(s3, run3, "mcp:run:"+run3.String(), "")
	resp3 := decodeProductReportResp(t, rec3)
	if resp3.Fingerprint != resp1.Fingerprint {
		t.Errorf("same detail class must reproduce the fingerprint: run3 %q != run1 %q", resp3.Fingerprint, resp1.Fingerprint)
	}
	if resp3.Action != "occurrence" {
		t.Errorf("run3 action = %q, want occurrence (dedup hit on run1's class)", resp3.Action)
	}
}

func decodeProductReportResp(t *testing.T, rec *httptest.ResponseRecorder) productReportResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp productReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
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

// --- wedge context + board placement (#1737) ---

// wedgedProductReportServer builds a server over the SAME wedged run
// shape diagnostics_test.go pins (red required check, campaign item in
// `failed` with one blocked dependent, slice_integration_conflict audit
// entry), wired for the product-report egress. tweak customizes the
// conventions so the no-project / with-project boarding arms can differ.
func wedgedProductReportServer(t *testing.T, fp *fakeFeedbackProvider, project *workmgmt.Project) (*Server, uuid.UUID) {
	t.Helper()
	return wedgedProductReportServerForRepo(t, fp, project, "")
}

// wedgedProductReportServerForRepo is wedgedProductReportServer with the
// SOURCE run's repo overridden, so the egress-authorization arm can drive
// a run whose repo is NOT the fixed product tracker. An empty sourceRepo
// keeps the fixture's own repo (which IS the product tracker).
func wedgedProductReportServerForRepo(t *testing.T, fp *fakeFeedbackProvider, project *workmgmt.Project, sourceRepo string) (*Server, uuid.UUID) {
	t.Helper()
	registerFakeFeedback(t, fp)
	runRow, stages, af, checks, camp := diagWedgeFixture(t)
	if sourceRepo != "" {
		runRow.Repo = sourceRepo
	}

	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		conv := workmgmt.Default()
		conv.Project = project
		return conv, nil
	}
	t.Cleanup(func() { conventionsLoader = prev })

	s := New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        &statusCommentRunRepo{stored: runRow, stages: stages},
		AuditRepo:      af,
		StageCheckRepo: checks,
		CampaignRepo:   camp,
	})
	return s, runRow.ID
}

func decodeProductReportResponse(t *testing.T, rec *httptest.ResponseRecorder) productReportResponse {
	t.Helper()
	var got productReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return got
}

// TestProductReport_WedgeEvidenceInBody is verification item (2): the
// cross-boundary handler -> diagnostics -> workmgmt assertion. A wedged
// run's FILED body carries the wedge lines, and the response reports the
// report boarded.
func TestProductReport_WedgeEvidenceInBody(t *testing.T) {
	fp := &fakeFeedbackProvider{boardOK: true}
	s, runID := wedgedProductReportServer(t, fp,
		&workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7})

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{"kind":"bug"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Fatal("provider.File was not called")
	}
	body := fp.filedReport.Body
	for _, want := range []string{
		"## Wedge context",
		"blocking required checks: `CI Pass`",
		"campaign item state: `failed`",
		"blocked dependents: `1`",
		"fan-in error: `slice_integration_conflict`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filed body missing %q\n---\n%s", want, body)
		}
	}
	// The redaction contract still holds across the EGRESS boundary: the
	// fixture seeds diagWedgeSentinel BY CONSTRUCTION into the wedged
	// review stage's FailureReason and into the fan-in audit payload, so
	// this assertion can genuinely fail if the wedge block ever starts
	// copying either through.
	//
	// Vacuity guard: if the fixture ever stops seeding the sentinel this
	// FAILS LOUD rather than degrading into an assertion that cannot fail.
	_, fixtureStages, fixtureAudit, _, _ := diagWedgeFixture(t)
	seededReason := ""
	if fr := fixtureStages[1].FailureReason; fr != nil {
		seededReason = *fr
	}
	if !strings.Contains(seededReason, diagWedgeSentinel) ||
		!strings.Contains(string(fixtureAudit.allEntries[1].Payload), diagWedgeSentinel) {
		t.Fatal("diagWedgeFixture no longer seeds diagWedgeSentinel into a stage FailureReason " +
			"AND the fan-in audit payload; the leak assertion below would be vacuous")
	}
	if strings.Contains(body, diagWedgeSentinel) {
		t.Errorf("filed body leaked free text:\n%s", body)
	}

	// Board placement requested Backlog against the configured project.
	if fp.filedReport.BoardPlacement.Status != "Backlog" {
		t.Errorf("board status = %q, want Backlog", fp.filedReport.BoardPlacement.Status)
	}
	if fp.filedTarget.Project == nil || fp.filedTarget.Project.Number != 7 {
		t.Errorf("filed target project = %+v, want the conventions project", fp.filedTarget.Project)
	}
	// Labels are still conventions-complete.
	if got := strings.Join(fp.filedReport.Labels, ","); got != "type:bug,autonomy:medium" {
		t.Errorf("labels = %q, want type:bug,autonomy:medium", got)
	}

	got := decodeProductReportResponse(t, rec)
	if !got.Boarded || got.BoardingStatus != workmgmt.BoardingStatusBoarded || got.BoardingError != "" {
		t.Errorf("boarding echo = %+v, want boarded/true/no-error", got)
	}
	if got.Number != 4242 {
		t.Errorf("number = %d, want 4242", got.Number)
	}
}

// TestProductReport_OccurrenceCommentCarriesWedgeSummary is verification
// item (5)'s occurrence half: the dedup-hit comment carries the one-line
// wedge summary, and the response reports that nothing was created to
// board.
func TestProductReport_OccurrenceCommentCarriesWedgeSummary(t *testing.T) {
	fp := &fakeFeedbackProvider{
		searchHit: &workmgmt.ExistingReport{Number: 11, URL: "https://example.test/11"},
	}
	s, runID := wedgedProductReportServer(t, fp,
		&workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7})

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if fp.filed {
		t.Fatal("a dedup hit filed a new report")
	}
	for _, want := range []string{
		"- wedge: ",
		"blocking checks `CI Pass`",
		"campaign item `failed`",
		"`1` blocked dependents",
		"fan-in `slice_integration_conflict`",
	} {
		if !strings.Contains(fp.occurrenceNote, want) {
			t.Errorf("occurrence note missing %q\n---\n%s", want, fp.occurrenceNote)
		}
	}
	got := decodeProductReportResponse(t, rec)
	if got.Boarded || got.BoardingStatus != workmgmt.BoardingStatusNotAttemptedNoReport {
		t.Errorf("boarding echo = %+v, want not_attempted_no_report", got)
	}
}

// TestProductReport_BoardingFailureStillFiles is counterfactual (9) and
// failure mode m7: placement fails, the report still returns 201 with the
// created number/URL, boarded=false, and a NON-EMPTY boarding_error
// naming which step failed.
func TestProductReport_BoardingFailureStillFiles(t *testing.T) {
	fp := &fakeFeedbackProvider{boardErr: "workmgmt/github: add project item: 403"}
	s, runID := wedgedProductReportServer(t, fp,
		&workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7})

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 even when boarding failed (body %s)", rec.Code, rec.Body.String())
	}
	got := decodeProductReportResponse(t, rec)
	if got.Boarded {
		t.Error("boarded = true after a placement failure")
	}
	if got.BoardingStatus != workmgmt.BoardingStatusFailed {
		t.Errorf("boarding_status = %q, want failed", got.BoardingStatus)
	}
	if got.BoardingError != workmgmt.BoardingCauseAddItemFailed {
		t.Errorf("boarding_error = %q, want the closed literal %q",
			got.BoardingError, workmgmt.BoardingCauseAddItemFailed)
	}
	if got.Number != 4242 || got.URL == "" {
		t.Errorf("created issue lost: number=%d url=%q", got.Number, got.URL)
	}
}

// TestProductReport_BoardingErrorIsCallerSafe is the disclosure control.
// The 201 body is a SUCCESS response, so writeError's default-deny detail
// allow-list never runs on it; without safeBoardingCause the raw provider
// error crosses to the caller. The worst instance is the unknown-status
// path, whose message enumerates the project's Status field OPTION
// NAMES — private board metadata reachable only through the operator's
// privileged projects token.
//
// The sentinels are seeded BY CONSTRUCTION into the raw cause the
// provider records, and every one of them is asserted absent from the
// wire while the closed literal is asserted present.
//
// Counterfactual: assign created.BoardingError straight to
// resp.BoardingError (drop the safeBoardingCause call) and this goes RED.
func TestProductReport_BoardingErrorIsCallerSafe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		wantCause string
		sentinels []string
	}{
		{
			name: "unknown status leaks the project's option names",
			raw: `workmgmt/github: status "Backlog" is not a Status option on the project; ` +
				`available: Embargoed-Legal, Security-Incident-Queue, Up Next`,
			wantCause: workmgmt.BoardingCauseStatusOptionUnknown,
			sentinels: []string{"Embargoed-Legal", "Security-Incident-Queue", "available:"},
		},
		{
			name:      "add item leaks third-party API text",
			raw:       "workmgmt/github: add project item: POST https://api.internal.example.com/graphql: 403 SAML enforcement",
			wantCause: workmgmt.BoardingCauseAddItemFailed,
			sentinels: []string{"api.internal.example.com", "SAML enforcement"},
		},
		{
			name:      "resolve fields",
			raw:       "workmgmt/github: resolve project fields: token TOPSECRET-PAT rejected",
			wantCause: workmgmt.BoardingCauseProjectFieldsUnavailable,
			sentinels: []string{"TOPSECRET-PAT"},
		},
		{
			name:      "set status",
			raw:       "workmgmt/github: set status field: node MDEwOlNlY3JldE5vZGU= denied",
			wantCause: workmgmt.BoardingCauseSetStatusFailed,
			sentinels: []string{"MDEwOlNlY3JldE5vZGU="},
		},
		{
			// The fail-safe default arm: an error this table does not
			// recognize must degrade to a vaguer literal, never to an echo.
			name:      "unrecognized cause degrades, never echoes",
			raw:       "some future provider error mentioning INTERNAL-HOSTNAME",
			wantCause: workmgmt.BoardingCauseUnclassified,
			sentinels: []string{"INTERNAL-HOSTNAME", "future provider error"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeFeedbackProvider{boardErr: tc.raw}
			s, runID := wedgedProductReportServer(t, fp,
				&workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7})

			rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
			}
			got := decodeProductReportResponse(t, rec)
			if got.BoardingStatus != workmgmt.BoardingStatusFailed {
				t.Fatalf("boarding_status = %q, want failed", got.BoardingStatus)
			}
			if got.BoardingError != tc.wantCause {
				t.Errorf("boarding_error = %q, want the closed literal %q", got.BoardingError, tc.wantCause)
			}
			for _, sentinel := range tc.sentinels {
				if strings.Contains(rec.Body.String(), sentinel) {
					t.Errorf("201 body leaked %q from the raw placement error: %s",
						sentinel, rec.Body.String())
				}
			}
		})
	}
}

// TestProductReport_ForeignRepoBoardNotAuthorized is the egress-
// authorization control. A product report always lands in the FIXED
// product tracker, but the board coordinates come from the SOURCE run's
// repo conventions. Without the binding, any repo Fishhawk runs against
// could name an arbitrary Projects-v2 board and have this server write to
// it under a privileged credential — the operator's static projects token
// for a user-owned board.
//
// Counterfactual: pass conv.Project straight into the Target (drop
// productReportBoard) and this goes RED on the filedTarget assertion.
func TestProductReport_ForeignRepoBoardNotAuthorized(t *testing.T) {
	foreign := &workmgmt.Project{Owner: "someone-else", OwnerType: "user", Number: 99}
	fp := &fakeFeedbackProvider{boardOK: true}
	s, runID := wedgedProductReportServerForRepo(t, fp, foreign, "attacker-org/not-fishhawk")

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Fatal("provider.File was not called")
	}
	if fp.filedTarget.Project != nil {
		t.Errorf("unauthorized board coordinates reached the provider: %+v", fp.filedTarget.Project)
	}
	got := decodeProductReportResponse(t, rec)
	if got.Boarded {
		t.Error("boarded = true against an unauthorized board")
	}
	if got.BoardingStatus != workmgmt.BoardingStatusNotAttemptedProjectNotAuthorized {
		t.Errorf("boarding_status = %q, want not_attempted_project_not_authorized", got.BoardingStatus)
	}
	if got.BoardingError != "" {
		t.Errorf("boarding_error = %q; a refused destination is a configuration state, not a failure",
			got.BoardingError)
	}

	// Control arm: the SAME project coordinates, from the destination
	// repo's own conventions, DO reach the provider. Pairing the refused
	// input with ITSELF is what makes this discrimination rather than a
	// comparison of two different values.
	fp2 := &fakeFeedbackProvider{boardOK: true}
	s2, runID2 := wedgedProductReportServerForRepo(t, fp2, foreign, productRepo)
	rec2 := postProductReport(s2, runID2, "mcp:run:"+runID2.String(), `{}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("control status = %d, want 201 (body %s)", rec2.Code, rec2.Body.String())
	}
	if fp2.filedTarget.Project == nil || fp2.filedTarget.Project.Number != 99 {
		t.Errorf("authorized board coordinates were dropped: %+v", fp2.filedTarget.Project)
	}
	if got2 := decodeProductReportResponse(t, rec2); got2.BoardingStatus != workmgmt.BoardingStatusBoarded {
		t.Errorf("control boarding_status = %q, want boarded", got2.BoardingStatus)
	}
}

// TestProductReport_FailedStatusAlwaysCarriesACause is the other half of
// TestProductReport_NotAttemptedStatusIsAlwaysCauseFree, and closes the
// contract seam the implement review found: BoardingStatusOf's defensive
// default arm (a project IS configured, and the provider neither boarded
// nor recorded a cause) returns `failed`, while the OpenAPI schema and
// the workmgmt README both state `failed` is the status that CARRIES a
// cause. An operator reading status=failed with no cause sees a state the
// docs say cannot happen — and the WARN log line carried an empty error
// string too.
//
// Counterfactual: delete safeBoardingCause's empty-input arm (return ""
// instead of BoardingCauseUnreported) and this goes RED.
func TestProductReport_FailedStatusAlwaysCarriesACause(t *testing.T) {
	// Neither boarded nor a cause, with a project configured.
	fp := &fakeFeedbackProvider{boardOK: false, boardErr: ""}
	s, runID := wedgedProductReportServer(t, fp,
		&workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7})

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	got := decodeProductReportResponse(t, rec)
	if got.BoardingStatus != workmgmt.BoardingStatusFailed {
		t.Fatalf("boarding_status = %q, want failed", got.BoardingStatus)
	}
	if got.BoardingError != workmgmt.BoardingCauseUnreported {
		t.Errorf("boarding_error = %q, want the synthesized %q — a `failed` with no cause "+
			"is a state the schema and the README both say cannot happen",
			got.BoardingError, workmgmt.BoardingCauseUnreported)
	}
	if !strings.Contains(rec.Body.String(), `"boarding_error"`) {
		t.Errorf("wire body omits boarding_error on a failed status: %s", rec.Body.String())
	}
}

// TestProductReport_NoProjectNotAttempted is failure mode m6 and the
// Condition-1 contract at the RESPONSE surface: no project configured
// reports boarded=false with the distinct not_attempted_no_project status
// and NO boarding_error, so an operator can tell it apart from a real
// placement failure with no extra lookup.
func TestProductReport_NoProjectNotAttempted(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	s, runID := wedgedProductReportServer(t, fp, nil)

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if fp.filedTarget.Project != nil {
		t.Errorf("target project = %+v, want nil when the conventions declare none", fp.filedTarget.Project)
	}
	got := decodeProductReportResponse(t, rec)
	if got.Boarded {
		t.Error("boarded = true with no project configured")
	}
	if got.BoardingStatus != workmgmt.BoardingStatusNotAttemptedNoProject {
		t.Errorf("boarding_status = %q, want not_attempted_no_project", got.BoardingStatus)
	}
	if got.BoardingError != "" {
		t.Errorf("boarding_error = %q, want empty — not attempting is not an error", got.BoardingError)
	}
	// The raw wire shape carries the field, so a client that reads the
	// JSON (not the Go struct) can distinguish the arms too.
	if !strings.Contains(rec.Body.String(), `"boarding_status":"not_attempted_no_project"`) {
		t.Errorf("wire body missing the status field: %s", rec.Body.String())
	}
}

// TestProductReport_HealthyRunOmitsWedgeBlock is failure mode m1 at the
// egress: a run with no wedge shape files a body with no wedge heading —
// the anti-noise counterfactual that keeps the block from becoming
// unconditional decoration.
func TestProductReport_HealthyRunOmitsWedgeBlock(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(fp.filedReport.Body, "Wedge context") {
		t.Errorf("un-wedged run rendered a wedge block:\n%s", fp.filedReport.Body)
	}
}

// TestProductReport_FingerprintStableAcrossWedge is regression pin (11):
// the dedup fingerprint must be UNCHANGED by the presence of wedge
// context, so every currently-open deduped report keeps matching its
// recurrences after this change.
func TestProductReport_FingerprintStableAcrossWedge(t *testing.T) {
	base := diagnostics.DiagnosticBundle{
		RunID:      uuid.New().String(),
		WorkflowID: "feature_change",
		RunState:   "failed",
		FailingStage: &diagnostics.FailingStage{
			Type:               "implement",
			FailureCategory:    "B",
			FailureSurface:     "scope_violation",
			FailureDetailClass: "undeclared_file",
		},
		Versions: diagnostics.VersionFacts{
			Fishhawkd: diagnostics.Component{Version: "0.4.2"},
		},
	}
	withWedge := base
	withWedge.WedgeContext = &diagnostics.WedgeContext{
		BlockingChecks:     []string{"CI Pass"},
		CampaignItemState:  "failed",
		BlockedDependents:  3,
		IntegrateWaveError: "slice_integration_conflict",
	}
	if a, b := bundleFingerprint(base), bundleFingerprint(withWedge); a != b {
		t.Fatalf("fingerprint drifted with wedge context: %q vs %q — every open deduped report would stop matching", a, b)
	}
}

// TestProductReport_NotAttemptedStatusIsAlwaysCauseFree defends the
// cause-free invariant on a not-attempted status, which the no-project
// happy path does NOT reach: a provider that records a BoardingError
// while no project is configured must still surface a cause-free
// not_attempted_no_project, because a cause on a "not attempted" status
// is exactly the ambiguity Condition 1 exists to remove. Found by running
// the counterfactual — deleting the guard left every other test green.
//
// HONEST SCOPE (observed, not reasoned — #1737 fix-up pass). The handler
// defends this invariant with a REDUNDANT PAIR: the `== failed` gate
// around the BoardingError assignment, and the trailing
// `!= failed -> BoardingError = ""` re-assertion. Deleting EITHER ONE
// alone leaves this test GREEN, because the survivor still clears or
// still withholds the cause; the test goes RED only when BOTH are
// removed (observed: boarding_error = "placement_failed" on a
// not_attempted_no_project body). So this test pins the INVARIANT, not
// either individual guard, and it is not a single-deletion
// counterfactual vehicle for either one. That redundancy is deliberate
// (see the handler comment calling the trailing clear a positive
// re-assertion) and the pairing is what makes a future edit to one arm
// non-fatal — but a reader must not read this test as proof that either
// arm is independently load-bearing.
func TestProductReport_NotAttemptedStatusIsAlwaysCauseFree(t *testing.T) {
	// The provider reports a cause even though nothing was boardable.
	fp := &fakeFeedbackProvider{boardErr: "stale cause from a provider that should not have tried"}
	s, runID := wedgedProductReportServer(t, fp, nil)

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	got := decodeProductReportResponse(t, rec)
	if got.BoardingStatus != workmgmt.BoardingStatusNotAttemptedNoProject {
		t.Fatalf("boarding_status = %q, want not_attempted_no_project", got.BoardingStatus)
	}
	if got.BoardingError != "" {
		t.Errorf("boarding_error = %q on a not-attempted status; a cause here reads as "+
			"'boarding was tried and failed', which is the ambiguity this contract removes",
			got.BoardingError)
	}
	if strings.Contains(rec.Body.String(), "boarding_error") {
		t.Errorf("wire body carries boarding_error on a not-attempted status: %s", rec.Body.String())
	}
}
