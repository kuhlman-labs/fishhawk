package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fakeWorkProvider is a workmgmt.Provider test double: it records the
// fully-resolved ProviderRequest the handler dispatched (the canonical
// -> provider seam the cross-boundary test asserts) and returns a
// canned CreatedItem or a configured error.
type fakeWorkProvider struct {
	name     string
	called   bool
	captured workmgmt.ProviderRequest
	fileErr  error
}

func (f *fakeWorkProvider) Name() string { return f.name }

func (f *fakeWorkProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	f.called = true
	f.captured = req
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	return &workmgmt.CreatedItem{
		Provider:      f.name,
		Number:        4242,
		URL:           "https://github.com/kuhlman-labs/fishhawk/issues/4242",
		AppliedLabels: req.Item.Classification.Labels,
		Status:        req.Item.BoardPlacement.Status,
		BoardColumn:   req.Item.BoardPlacement.BoardColumn,
	}, nil
}

// registerFakeProvider registers p under the default conventions'
// provider id (github_projects) so the handler's workmgmt.Get resolves
// it. The registry is process-global with no deregister, so each test
// re-registers a fresh fake; the names never collide with the
// never-registered "jira" id the unimplemented-provider case uses.
func registerFakeProvider(t *testing.T, p *fakeWorkProvider) {
	t.Helper()
	if p.name == "" {
		p.name = workmgmt.Default().Provider
	}
	workmgmt.Register(p)
}

// fileWorkItem POSTs body to the handler with an authenticated identity
// and returns the recorder.
func fileWorkItem(t *testing.T, s *Server, body workItemRequest, subject string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/work-items", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: subject}))
	rec := httptest.NewRecorder()
	s.handleFileWorkItem(rec, req)
	return rec
}

func decodeWorkItem(t *testing.T, rec *httptest.ResponseRecorder) workItemResponse {
	t.Helper()
	var resp workItemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

// TestFileWorkItem_RunInFlight_AuditsAndApplies drives the full
// cross-boundary seam (#618): request -> conventions Apply -> registered
// fake provider -> work_item_filed audit. It asserts both that the
// provider received the conventions-resolved item and that an audit
// entry landed on the in-flight run.
func TestFileWorkItem_RunInFlight_AuditsAndApplies(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	au := newAuditFake()
	rr := newPromptRunRepo()
	runID := uuid.New()
	inst := int64(99)
	rr.getRuns[runID] = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/fishhawk",
		State:          run.StateRunning,
		InstallationID: &inst,
	}
	s := New(Config{AuditRepo: au, RunRepo: rr})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
		Relations: &workItemRelations{ParentEpic: "#1005"},
		RunID:     runID.String(),
	}, "fishhawk-operator-agent@local")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if resp.Title != "[E22.5] Add the widget endpoint" {
		t.Errorf("title = %q, want rendered from title_format", resp.Title)
	}
	if resp.Number != 4242 || resp.URL == "" {
		t.Errorf("created number/url not echoed: %+v", resp)
	}
	if resp.Provider != workmgmt.Default().Provider {
		t.Errorf("provider = %q, want %q", resp.Provider, workmgmt.Default().Provider)
	}
	if !resp.Audited {
		t.Error("audited = false, want true for an in-flight run")
	}

	// Provider seam: the conventions-resolved item reached the provider.
	if !fp.called {
		t.Fatal("provider was not called")
	}
	got := fp.captured
	if got.Item.Title != "[E22.5] Add the widget endpoint" {
		t.Errorf("provider Item.Title = %q", got.Item.Title)
	}
	if got.Item.Relations.ParentEpic != "#1005" {
		t.Errorf("provider Item.Relations.ParentEpic = %q", got.Item.Relations.ParentEpic)
	}
	if len(got.Item.Classification.Labels) == 0 || got.Item.Classification.Labels[0] != "type:feature" {
		t.Errorf("provider Item.Labels = %v, want default type:feature", got.Item.Classification.Labels)
	}
	if got.Target.InstallationID != inst {
		t.Errorf("provider Target.InstallationID = %d, want %d", got.Target.InstallationID, inst)
	}
	if got.Target.Repo.Owner != "kuhlman-labs" || got.Target.Repo.Name != "fishhawk" {
		t.Errorf("provider Target.Repo = %+v", got.Target.Repo)
	}

	// Audit seam: one work_item_filed entry on the run.
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category != categoryWorkItemFiled {
			continue
		}
		found = true
		if e.RunID != runID {
			t.Errorf("audit RunID = %s, want %s", e.RunID, runID)
		}
		if e.ActorSubject == nil || *e.ActorSubject != "fishhawk-operator-agent@local" {
			t.Errorf("audit ActorSubject = %v", e.ActorSubject)
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("audit payload: %v", err)
		}
		if payload["type"] != "feature" || payload["provider"] != workmgmt.Default().Provider {
			t.Errorf("audit payload missing fields: %v", payload)
		}
		if payload["created_url"] == "" || payload["created_url"] == nil {
			t.Errorf("audit payload created_url empty: %v", payload)
		}
	}
	if !found {
		t.Errorf("no work_item_filed audit entry; appended=%d", len(au.appended))
	}
}

// TestFileWorkItem_NoRun_FilesWithoutAudit asserts the run-absent branch:
// filing succeeds with no run in flight, and no audit entry is written.
func TestFileWorkItem_NoRun_FilesWithoutAudit(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	au := newAuditFake()
	s := New(Config{AuditRepo: au})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Tidy the workspace file",
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if resp.Audited {
		t.Error("audited = true, want false with no run in flight")
	}
	if !fp.called {
		t.Error("provider not called")
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 0 {
		t.Errorf("appended %d audit entries, want 0", len(au.appended))
	}
}

// TestFileWorkItem_TerminalRun_NoAudit asserts a run_id pointing at a
// terminal run does not get a work_item_filed entry (in-flight only),
// while the filing still succeeds.
func TestFileWorkItem_TerminalRun_NoAudit(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	au := newAuditFake()
	rr := newPromptRunRepo()
	runID := uuid.New()
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: run.StateSucceeded}
	s := New(Config{AuditRepo: au, RunRepo: rr})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Tidy after the run",
		RunID:   runID.String(),
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if decodeWorkItem(t, rec).Audited {
		t.Error("audited = true, want false for a terminal run")
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 0 {
		t.Errorf("appended %d audit entries, want 0 for terminal run", len(au.appended))
	}
}

// TestFileWorkItem_NumberedType_AllocatesAndDispatches confirms the ADR
// sequential numbering flows through Apply into the ProviderRequest.
func TestFileWorkItem_NumberedType_AllocatesAndDispatches(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:            "kuhlman-labs/fishhawk",
		Type:            "adr",
		Summary:         "Record the provider boundary",
		ExistingNumbers: []int{34, 35},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.captured.Number != 36 {
		t.Errorf("ProviderRequest.Number = %d, want 36", fp.captured.Number)
	}
	if fp.captured.Item.Title != "[ADR-36] Record the provider boundary" {
		t.Errorf("title = %q, want ADR-36 rendered", fp.captured.Item.Title)
	}
}

// TestFileWorkItem_UnimplementedProvider_FailsClosed asserts an
// unregistered/unimplemented provider id (jira is interface-only in v0)
// returns a typed 501 naming the missing provider rather than panicking.
func TestFileWorkItem_UnimplementedProvider_FailsClosed(t *testing.T) {
	conv := workmgmt.Default()
	conv.Provider = "jira" // never registered
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	s := New(Config{})
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Try the jira path",
	}, "github:operator")

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if env.Error.Code != "provider_unimplemented" {
		t.Errorf("code = %q, want provider_unimplemented", env.Error.Code)
	}
	if env.Error.Details["provider"] != "jira" {
		t.Errorf("details.provider = %v, want jira", env.Error.Details["provider"])
	}
}

// TestFileWorkItem_ApplyError_Unprocessable asserts a conventions
// violation (an unknown work-item type) returns 422 work_item_invalid.
func TestFileWorkItem_ApplyError_Unprocessable(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "not-a-type",
		Summary: "Bad type",
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.called {
		t.Error("provider should not be called on an apply error")
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
}

// TestFileWorkItem_ProviderFileError_BadGateway asserts a provider-side
// failure surfaces as 502 work_item_filing_failed.
func TestFileWorkItem_ProviderFileError_BadGateway(t *testing.T) {
	fp := &fakeWorkProvider{fileErr: errors.New("github said no")}
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Will fail at the provider",
	}, "github:operator")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_filing_failed" {
		t.Errorf("code = %q, want work_item_filing_failed", env.Error.Code)
	}
}

// TestFileWorkItem_BadRequests covers the validation guards.
func TestFileWorkItem_BadRequests(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	s := New(Config{})

	cases := []struct {
		name string
		body workItemRequest
	}{
		{"missing repo", workItemRequest{Type: "chore", Summary: "x"}},
		{"bad repo", workItemRequest{Repo: "no-slash", Type: "chore", Summary: "x"}},
		{"missing type", workItemRequest{Repo: "o/r", Summary: "x"}},
		{"missing summary", workItemRequest{Repo: "o/r", Type: "chore"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := fileWorkItem(t, s, tc.body, "github:operator")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestFileWorkItem_Anonymous_Unauthorized asserts an unauthenticated
// caller is rejected.
func TestFileWorkItem_Anonymous_Unauthorized(t *testing.T) {
	s := New(Config{})
	rec := fileWorkItem(t, s, workItemRequest{
		Repo: "o/r", Type: "chore", Summary: "x",
	}, "anonymous")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}
