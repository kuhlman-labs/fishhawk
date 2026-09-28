package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
	workmgmtgitlab "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/gitlab"
	workmgmtjira "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/jira"
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
	// failIfNoInstallation mirrors the real github.Provider fail-closed
	// (#1092's installation_unavailable guard): File errors when the
	// resolved Target.Scope is still the zero scope.
	failIfNoInstallation bool
	// boardingError, when set, mirrors the github.Provider best-effort
	// boarding failure (#1107): File returns the created item with a nil
	// error, Boarded=false, and this string as BoardingError.
	boardingError string
}

func (f *fakeWorkProvider) Name() string { return f.name }

func (f *fakeWorkProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	f.called = true
	f.captured = req
	if f.failIfNoInstallation && req.Target.Scope.IsZero() {
		return nil, errors.New("installation_unavailable: provider needs an installation token")
	}
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	created := &workmgmt.CreatedItem{
		Provider:      f.name,
		Number:        4242,
		URL:           "https://github.com/kuhlman-labs/fishhawk/issues/4242",
		AppliedLabels: req.Item.Classification.Labels,
		Status:        req.Item.BoardPlacement.Status,
		BoardColumn:   req.Item.BoardPlacement.BoardColumn,
	}
	if f.boardingError != "" {
		created.BoardingError = f.boardingError
	} else {
		created.Boarded = true
	}
	return created, nil
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

	// The caller is the run's own run-bound agent token: the only
	// identity entitled to drive a work_item_filed audit onto this run.
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
		Relations: &workItemRelations{ParentEpic: "#1005"},
		RunID:     runID.String(),
	}, "mcp:run:"+runID.String())

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
	if got.Target.Scope != forge.FromGitHubInstallationID(inst) {
		t.Errorf("provider Target.Scope = %q, want scope for installation %d", got.Target.Scope.Ref(), inst)
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
		if e.ActorSubject == nil || *e.ActorSubject != "mcp:run:"+runID.String() {
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
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Tidy the workspace file",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
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

// TestFileWorkItem_DependsOn_ThreadsToProviderRequest asserts the HTTP
// relations.depends_on threads through the request -> filing.Relations ->
// Apply -> the dispatched ProviderRequest (the request->domain half).
func TestFileWorkItem_DependsOn_ThreadsToProviderRequest(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	s := New(Config{AuditRepo: newAuditFake()})
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "depends on siblings",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Relations: &workItemRelations{DependsOn: []string{"#41", "42"}},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	got := fp.captured.Item.Relations.DependsOn
	if len(got) != 2 || got[0] != "#41" || got[1] != "42" {
		t.Errorf("provider Item.Relations.DependsOn = %v, want [#41 42]", got)
	}
}

// TestFileWorkItem_DependsOn_MalformedRejected asserts a malformed
// depends_on entry is rejected with 422 work_item_invalid (the file-time
// format-validation branch in Apply, surfaced through the handler).
func TestFileWorkItem_DependsOn_MalformedRejected(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	s := New(Config{AuditRepo: newAuditFake()})
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "bad dep",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Relations: &workItemRelations{DependsOn: []string{"not-a-ref"}},
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "work_item_invalid") {
		t.Errorf("body should carry work_item_invalid: %s", rec.Body.String())
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
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Tidy after the run",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		RunID:     runID.String(),
	}, "mcp:run:"+runID.String())

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
	if fp.captured.Item.Title != "[ADR-036] Record the provider boundary" {
		t.Errorf("title = %q, want ADR-036 rendered", fp.captured.Item.Title)
	}
}

// TestFileWorkItem_NumberedType_EmptyExistingNumbers_Unprocessable is the
// #1265 cross-layer done-means: an adr filing with existing_numbers omitted
// returns 422 work_item_invalid (surfacing the numbered-type cause in
// details) instead of silently filing ADR-001. It exercises the full
// wire -> workItemRequest -> FilingRequest -> Apply -> allocateNumber ->
// work_item_invalid mapping; the provider is never dispatched.
func TestFileWorkItem_NumberedType_EmptyExistingNumbers_Unprocessable(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "Record the provider boundary",
		// existing_numbers omitted on purpose
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	if env.Error.Details["existing_numbers_required"] != true {
		t.Errorf("details.existing_numbers_required = %v, want true", env.Error.Details["existing_numbers_required"])
	}
	if fp.called {
		t.Error("provider dispatched despite a fail-closed numbered allocate")
	}
}

// fakeDiscoverProvider is a workmgmt.Provider that ALSO implements the
// optional workmgmt.NumberDiscoverer capability (#1269), so the handler's
// type-assert resolves it and runs server-side discovery before Apply. It
// records whether discovery was called and the request it received.
type fakeDiscoverProvider struct {
	fakeWorkProvider
	discovered     []int
	discoverErr    error
	discoverCalled bool
	discoverReq    workmgmt.DiscoverNumbersRequest
}

func (f *fakeDiscoverProvider) DiscoverNumbers(_ context.Context, req workmgmt.DiscoverNumbersRequest) ([]int, error) {
	f.discoverCalled = true
	f.discoverReq = req
	if f.discoverErr != nil {
		return nil, f.discoverErr
	}
	return f.discovered, nil
}

// registerFakeDiscoverProvider registers a discovery-capable fake under the
// default provider id so the handler's workmgmt.Get + NumberDiscoverer assert
// resolve it.
func registerFakeDiscoverProvider(t *testing.T, p *fakeDiscoverProvider) {
	t.Helper()
	if p.name == "" {
		p.name = workmgmt.Default().Provider
	}
	workmgmt.Register(p)
}

// TestFileWorkItem_NumberedTypeDiscoversNextNumber is the cross-boundary seam
// (handler -> NumberDiscoverer capability -> provider): an adr filing with
// existing_numbers omitted discovers the in-use numbers server-side and
// allocates max+1.
func TestFileWorkItem_NumberedTypeDiscoversNextNumber(t *testing.T) {
	fp := &fakeDiscoverProvider{discovered: []int{65, 70, 79}}
	registerFakeDiscoverProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "Record the discovery boundary",
		// existing_numbers omitted on purpose — discovery fills it.
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.discoverCalled {
		t.Fatal("discovery capability was not invoked")
	}
	if fp.discoverReq.Prefix != "ADR-" || fp.discoverReq.TitleFormat != "[ADR-{number}] {summary}" {
		t.Errorf("discover request = %+v, want adr prefix/format", fp.discoverReq)
	}
	// The handler must thread the item type's default_labels into the discovery
	// request so the provider can narrow discovery by a label: qualifier (#1522).
	// The adr type carries default_labels [adr].
	if len(fp.discoverReq.DefaultLabels) != 1 || fp.discoverReq.DefaultLabels[0] != "adr" {
		t.Errorf("discover request DefaultLabels = %v, want [adr] (threaded from the type's conventions)", fp.discoverReq.DefaultLabels)
	}
	if fp.captured.Number != 80 {
		t.Errorf("ProviderRequest.Number = %d, want 80 (max(65,70,79)+1)", fp.captured.Number)
	}
	if fp.captured.Item.Title != "[ADR-080] Record the discovery boundary" {
		t.Errorf("title = %q, want ADR-080", fp.captured.Item.Title)
	}
}

// TestFileWorkItem_NumberedTypeDiscoversFirstNumber asserts the empty-discovery
// seed path: no existing numbers -> the handler seeds [0] -> allocate yields 1
// (ADR-001), NOT a silent wrong number nor a crash.
func TestFileWorkItem_NumberedTypeDiscoversFirstNumber(t *testing.T) {
	fp := &fakeDiscoverProvider{discovered: nil}
	registerFakeDiscoverProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "The very first decision",
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.discoverCalled {
		t.Fatal("discovery capability was not invoked")
	}
	if fp.captured.Number != 1 {
		t.Errorf("ProviderRequest.Number = %d, want 1 (empty discovery -> seed [0] -> 1)", fp.captured.Number)
	}
	if fp.captured.Item.Title != "[ADR-001] The very first decision" {
		t.Errorf("title = %q, want ADR-001", fp.captured.Item.Title)
	}
}

// TestFileWorkItem_CallerExistingNumbersOverridesDiscovery asserts a
// caller-supplied existing_numbers short-circuits discovery — the discoverer is
// NOT called and the caller's list wins.
func TestFileWorkItem_CallerExistingNumbersOverridesDiscovery(t *testing.T) {
	fp := &fakeDiscoverProvider{discovered: []int{500}} // would yield 501 if discovery ran
	registerFakeDiscoverProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:            "kuhlman-labs/fishhawk",
		Type:            "adr",
		Summary:         "Caller knows best",
		ExistingNumbers: []int{34, 35},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.discoverCalled {
		t.Error("discovery ran despite a caller-supplied existing_numbers override")
	}
	if fp.captured.Number != 36 {
		t.Errorf("ProviderRequest.Number = %d, want 36 (caller list 34,35 -> 36)", fp.captured.Number)
	}
}

// TestFileWorkItem_DiscoveryErrorFailsClosed asserts a genuine discovery error
// fails the filing closed with 422 work_item_invalid carrying
// details.discovery_failed, and NO issue is created (File is never dispatched).
func TestFileWorkItem_DiscoveryErrorFailsClosed(t *testing.T) {
	fp := &fakeDiscoverProvider{discoverErr: errors.New("search API exploded")}
	registerFakeDiscoverProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "Discovery will fail",
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	if got, _ := env.Error.Details["discovery_failed"].(string); got == "" || !strings.Contains(got, "search API exploded") {
		t.Errorf("details.discovery_failed = %v, want it to carry the cause", env.Error.Details["discovery_failed"])
	}
	if fp.called {
		t.Error("provider File dispatched despite a fail-closed discovery error")
	}
}

// TestFileWorkItem_ProviderWithoutDiscovererFailsClosed asserts a provider that
// does NOT implement NumberDiscoverer + an omitted existing_numbers falls
// through to Apply's pre-existing #1265 fail-closed 422 (no silent ADR-001).
// Discovery never ran, so the 422 is NOT enriched with discovery_failed.
func TestFileWorkItem_ProviderWithoutDiscovererFailsClosed(t *testing.T) {
	fp := &fakeWorkProvider{} // File-only, no NumberDiscoverer capability
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "No discovery capability",
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	if env.Error.Details["existing_numbers_required"] != true {
		t.Errorf("details.existing_numbers_required = %v, want true (the #1265 guard)", env.Error.Details["existing_numbers_required"])
	}
	if _, present := env.Error.Details["discovery_failed"]; present {
		t.Errorf("details must NOT carry discovery_failed (no discovery ran): %v", env.Error.Details)
	}
	if fp.called {
		t.Error("provider File dispatched despite a fail-closed numbered allocate")
	}
}

// TestFileWorkItem_UnimplementedProvider_FailsClosed asserts an
// unregistered/unimplemented provider id returns a typed 501 naming the
// missing provider rather than panicking. It uses workItemProviderAbsentID, an
// id no production provider uses and no test registers: the registry is
// process-global, and "jira" and (since #3658's real-provider campaign and
// onboarding tests) "gitlab" are both registered by earlier tests in this
// package, so either would resolve depending on test order.
func TestFileWorkItem_UnimplementedProvider_FailsClosed(t *testing.T) {
	conv := workmgmt.Default()
	conv.Provider = workItemProviderAbsentID // never registered
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	s := New(Config{})
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Try an unimplemented provider",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
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
	if env.Error.Details["provider"] != workItemProviderAbsentID {
		t.Errorf("details.provider = %v, want %s", env.Error.Details["provider"], workItemProviderAbsentID)
	}
}

// TestFileWorkItem_Jira_EndToEnd is the #1094 cross-boundary seam: the
// Target.Jira field spans config-parse -> filing endpoint -> provider ->
// REST client. It injects a provider: jira conventions (with a jira block)
// through conventionsLoader and registers the REAL jira provider backed by
// a *jiraclient.Client pointed at a stubbed HTTP transport, then POSTs a
// file-issue request and asserts (a) the created Jira issue key/URL is
// returned and (b) the conventions-resolved title/body/labels reached the
// transport — the seam a per-layer unit (Target.Jira left unpopulated)
// would miss (cf. #618).
func TestFileWorkItem_Jira_EndToEnd(t *testing.T) {
	var createBody, linkBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rest/api/3/issue", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&createBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"10042","key":"FISH-42"}`)
	})
	// The post-create LinkParent PUT — capture its body so the test can
	// assert the provider->client seam emits the right wire shape per field.
	mux.HandleFunc("PUT /rest/api/3/issue/{key}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&linkBody)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /rest/api/3/issue/{key}/transitions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"To Backlog","to":{"name":"Backlog"}}]}`)
	})
	mux.HandleFunc("POST /rest/api/3/issue/{key}/transitions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	workmgmt.Register(workmgmtjira.New(jiraclient.New(srv.URL, "e@x.com", "tok")))

	conv := workmgmt.Default()
	conv.Provider = workmgmtjira.ProviderName
	conv.Jira = &workmgmt.JiraConnection{ProjectKey: "FISH"}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	// file runs one filing flow with the conventions' current jira block and
	// returns the decoded response; createBody/linkBody hold the captured
	// transport bodies for that flow.
	file := func(t *testing.T) workItemResponse {
		t.Helper()
		createBody, linkBody = nil, nil
		s := New(Config{})
		rec := fileWorkItem(t, s, workItemRequest{
			Repo:      "kuhlman-labs/fishhawk",
			Type:      "feature",
			Summary:   "Add the widget endpoint",
			TitleVars: map[string]string{"epic": "22", "n": "5"},
			Relations: &workItemRelations{ParentEpic: "FISH-100"},
		}, "github:operator")
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
		return decodeWorkItem(t, rec)
	}

	// Default (team-managed) flow: parent_field unset.
	resp := file(t)
	if resp.Provider != workmgmtjira.ProviderName {
		t.Errorf("provider = %q, want %q", resp.Provider, workmgmtjira.ProviderName)
	}
	if !resp.EpicLinked {
		t.Errorf("epic_linked = false, want true (parent FISH-100 linked post-create): %+v", resp)
	}
	// The created Jira issue key/URL is returned: Number is the key's numeric
	// suffix, URL the browse URL carrying the full key.
	if resp.Number != 42 {
		t.Errorf("number = %d, want 42 (suffix of FISH-42)", resp.Number)
	}
	if resp.URL != srv.URL+"/browse/FISH-42" {
		t.Errorf("url = %q, want the FISH-42 browse URL", resp.URL)
	}
	if !resp.Boarded {
		t.Errorf("boarded = false, want true (the Backlog transition succeeded): %+v", resp)
	}

	// Transport seam: the conventions-resolved title/body/labels reached the
	// Jira create call — and the parent is NOT linked at create time.
	if createBody == nil {
		t.Fatal("create transport was not hit")
	}
	fields, _ := createBody["fields"].(map[string]any)
	if fields == nil {
		t.Fatalf("create body missing fields: %v", createBody)
	}
	if got, _ := fields["summary"].(string); got != "[E22.5] Add the widget endpoint" {
		t.Errorf("transport summary = %q, want the rendered title", got)
	}
	if proj, _ := fields["project"].(map[string]any); proj["key"] != "FISH" {
		t.Errorf("transport project.key = %v, want FISH", proj["key"])
	}
	if it, _ := fields["issuetype"].(map[string]any); it["name"] != "Feature" {
		t.Errorf("transport issuetype.name = %v, want Feature", it["name"])
	}
	labels, _ := fields["labels"].([]any)
	if len(labels) == 0 || labels[0] != "type:feature" {
		t.Errorf("transport labels = %v, want the resolved default type:feature", labels)
	}
	if _, present := fields["parent"]; present {
		t.Errorf("create body carried parent; linking is now a post-create PUT: %v", fields)
	}
	if _, ok := fields["description"]; !ok {
		t.Errorf("transport body (description) not sent: %v", fields)
	}

	// Post-create link seam (team-managed): the PUT body emits the object
	// shape {"parent":{"key":"FISH-100"}}.
	if linkBody == nil {
		t.Fatal("link (PUT) transport was not hit for the default parent_field")
	}
	linkFields, _ := linkBody["fields"].(map[string]any)
	if parent, _ := linkFields["parent"].(map[string]any); parent["key"] != "FISH-100" {
		t.Errorf("link body parent.key = %v, want FISH-100 (team-managed object shape)", parent["key"])
	}

	// Classic flow: a configured epic-link custom field emits the bare-string
	// shape {"customfield_10014":"FISH-100"} at the same PUT seam.
	conv.Jira = &workmgmt.JiraConnection{ProjectKey: "FISH", ParentField: "customfield_10014"}
	resp = file(t)
	if !resp.EpicLinked {
		t.Errorf("epic_linked = false, want true (classic custom field linked): %+v", resp)
	}
	if linkBody == nil {
		t.Fatal("link (PUT) transport was not hit for the classic parent_field")
	}
	linkFields, _ = linkBody["fields"].(map[string]any)
	if got, _ := linkFields["customfield_10014"].(string); got != "FISH-100" {
		t.Errorf("link body customfield_10014 = %v, want bare string FISH-100 (classic shape)", linkFields["customfield_10014"])
	}
	if _, present := linkFields["parent"]; present {
		t.Errorf("classic link body carried a parent object: %v", linkFields)
	}
}

// TestSetConventionsLoader covers the deployment-level override seam
// (ADR-058 #1856): SetConventionsLoader replaces the process-wide resolver so
// serve.go can serve a FISHHAWKD_WORKMGMT_CONVENTIONS file for every repo. It
// asserts the installed loader is the one the handler resolves through.
func TestSetConventionsLoader(t *testing.T) {
	prev := conventionsLoader
	t.Cleanup(func() { conventionsLoader = prev })

	want := workmgmt.Default()
	want.Provider = workmgmtgitlab.ProviderName
	want.GitLab = &workmgmt.GitLabConnection{Project: "group/app"}
	SetConventionsLoader(func(context.Context, string) (workmgmt.Conventions, error) { return want, nil })

	got, err := conventionsLoader(context.Background(), "any/repo")
	if err != nil {
		t.Fatalf("conventionsLoader err = %v, want nil", err)
	}
	if got.Provider != workmgmtgitlab.ProviderName {
		t.Errorf("Provider = %q, want %q (the installed loader was not used)", got.Provider, workmgmtgitlab.ProviderName)
	}
	if got.GitLab == nil || got.GitLab.Project != "group/app" {
		t.Errorf("GitLab = %+v, want the installed override's block", got.GitLab)
	}
}

// TestFileWorkItem_GitLab_ForgeOptional pins the forge-optional gate
// (ADR-058 #1856): a provider: gitlab filing must NOT enter the GitHub
// installation-resolution branch, even when a GitHub client IS configured, so
// it never 502s on GitHub egress. The GitHub client's installation endpoint
// fails the test if hit; the fake gitlab provider must still be dispatched
// with Target.GitLab populated and Target.Scope left zero (gitlab carries its
// own server-side credentials).
func TestFileWorkItem_GitLab_ForgeOptional(t *testing.T) {
	fp := &fakeWorkProvider{name: workmgmtgitlab.ProviderName}
	workmgmt.Register(fp)

	// A GitHub client whose installation endpoint must NOT be hit for a gitlab
	// filing: the provider gate skips GitHub resolution entirely.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("GetRepoInstallation called for a gitlab filing; want the GitHub branch skipped")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	s := New(Config{GitHub: gh})

	conv := workmgmt.Default()
	conv.Provider = workmgmtgitlab.ProviderName
	conv.GitLab = &workmgmt.GitLabConnection{Project: "group/subgroup/app"}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "GitLab filing skips GitHub egress",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
		Relations: &workItemRelations{ParentEpic: "#100"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.called {
		t.Fatal("gitlab provider was not dispatched")
	}
	if !fp.captured.Target.Scope.IsZero() {
		t.Errorf("Target.Scope = %q, want zero (no GitHub installation resolved for a gitlab filing)", fp.captured.Target.Scope.Ref())
	}
	if fp.captured.Target.GitLab == nil || fp.captured.Target.GitLab.Project != "group/subgroup/app" {
		t.Errorf("Target.GitLab = %+v, want the conventions' gitlab block populated", fp.captured.Target.GitLab)
	}
}

// TestFileWorkItem_GitLab_EndToEnd is the ADR-058 #1856 cross-boundary seam:
// the Target.GitLab field spans conventions-parse -> filing endpoint ->
// provider -> REST client. It injects a provider: gitlab conventions through
// conventionsLoader and registers the REAL gitlab provider backed by a
// *gitlabclient.Client pointed at an httptest fake GitLab API, then POSTs a
// file-issue request and asserts (a) the created issue number/URL/labels are
// returned and boarded, and (b) the conventions-resolved project/labels
// reached the transport — the seam a per-layer unit (Target.GitLab left
// unpopulated) would miss (cf. #618). No GitHub client is configured, proving
// a gitlab filing needs no GitHub egress.
func TestFileWorkItem_GitLab_EndToEnd(t *testing.T) {
	var createLabels string
	var projectLookupPath string
	var linkHit bool
	// A single catch-all handler routing on the ESCAPED path: the namespaced
	// project path carries %2F, which a ServeMux `{path}` wildcard would
	// mis-split (net/http decodes r.URL.Path). Match on EscapedPath() so the
	// on-the-wire percent-encoding is observable, mirroring the gitlabclient
	// stub-Doer tests.
	handler := func(w http.ResponseWriter, r *http.Request) {
		esc := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(esc, "/api/v4/projects/") &&
			!strings.Contains(esc, "/issues"):
			// GET /api/v4/projects/:url-encoded-path -> {id, web_url}.
			projectLookupPath = strings.TrimPrefix(esc, "/api/v4/projects/")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":55,"web_url":"https://gitlab.example/group/subgroup/app"}`)
		case r.Method == http.MethodPost && esc == "/api/v4/projects/55/issues":
			// Issue create -> {iid, web_url}.
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			createLabels, _ = body["labels"].(string)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"iid":42,"web_url":"https://gitlab.example/group/subgroup/app/-/issues/42"}`)
		case r.Method == http.MethodGet && esc == "/api/v4/projects/55/issues/100/links":
			// The parent-epic CAPACITY probe (#3714): an explicit title_vars.n
			// skips {n} derivation, so guardParentEpicCapacity makes its own
			// EpicChildren read — on gitlab that is the epic issue's relates_to
			// links. An empty link set means no children, and gitlab declares no
			// ChildCap, so the guard is doubly inert and the filing proceeds.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && esc == "/api/v4/projects/55/issues/42/links":
			// Best-effort parent link.
			linkHit = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, esc)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	workmgmt.Register(workmgmtgitlab.New(gitlabclient.New(srv.URL, "glpat-tok")))

	conv := workmgmt.Default()
	conv.Provider = workmgmtgitlab.ProviderName
	conv.GitLab = &workmgmt.GitLabConnection{Project: "group/subgroup/app"}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	// No GitHub client configured: a gitlab filing must not need one.
	s := New(Config{})
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
		Relations: &workItemRelations{ParentEpic: "#100"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)

	// (a) The created GitLab issue iid/URL are returned.
	if resp.Provider != workmgmtgitlab.ProviderName {
		t.Errorf("provider = %q, want %q", resp.Provider, workmgmtgitlab.ProviderName)
	}
	if resp.Number != 42 {
		t.Errorf("number = %d, want 42 (the created issue iid)", resp.Number)
	}
	if resp.URL != "https://gitlab.example/group/subgroup/app/-/issues/42" {
		t.Errorf("url = %q, want the created issue web_url", resp.URL)
	}
	// Board placement rode the create as the status label -> Boarded true.
	if !resp.Boarded {
		t.Errorf("boarded = false, want true (the Backlog status label rode the create): %+v", resp)
	}
	if !resp.EpicLinked {
		t.Errorf("epic_linked = false, want true (#100 linked post-create): %+v", resp)
	}

	// (b) Transport seam: the conventions-resolved project path was
	// URL-encoded into the lookup and the resolved labels (including the
	// Backlog board-status label) reached the create call.
	if projectLookupPath != "group%2Fsubgroup%2Fapp" {
		t.Errorf("project lookup path = %q, want the URL-encoded namespaced path", projectLookupPath)
	}
	if !strings.Contains(createLabels, "type:feature") {
		t.Errorf("create labels = %q, want the resolved default type:feature", createLabels)
	}
	if !strings.Contains(createLabels, "Backlog") {
		t.Errorf("create labels = %q, want the Backlog board-status label (label-driven board placement)", createLabels)
	}
	if !linkHit {
		t.Error("parent link transport was not hit for #100")
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

// TestFileWorkItem_ProviderFileError_BadGateway asserts a genuinely fatal
// provider-side failure (CreateIssue / installation resolution — no durable
// issue exists) surfaces as 502 work_item_filing_failed. Post-#2587 the raw
// provider cause is REDACTED out of the 5xx body by the writeError chokepoint
// (it can carry storage / third-party-endpoint internals); the operator gets
// it from the server log keyed by error_ref instead. This test is the
// allow-list counterfactual vehicle: deleting redactErrorDetails in writeError
// makes details.error reappear here and this test go RED. (error_ref presence
// is proven by TestWriteError_5xxSetsErrorRefFromRequestID and the integration
// test, which drive the real requestID middleware; the fileWorkItem helper
// calls the handler directly, so no request id is in scope here.)
func TestFileWorkItem_ProviderFileError_BadGateway(t *testing.T) {
	fp := &fakeWorkProvider{fileErr: errors.New("github said no")}
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Will fail at the provider",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, "github:operator")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_filing_failed" {
		t.Errorf("code = %q, want work_item_filing_failed", env.Error.Code)
	}
	// The provider cause must NOT reach the caller: the writeError chokepoint
	// redacts the non-allow-listed "error" key out of the 5xx body (#2587).
	if got, ok := env.Error.Details["error"]; ok {
		t.Errorf("details.error must be redacted from a 5xx body, got %v", got)
	}
	// And the raw cause must not leak via any other channel of the body.
	if strings.Contains(rec.Body.String(), "github said no") {
		t.Errorf("raw provider cause leaked into the 5xx body: %s", rec.Body.String())
	}
}

// TestFileWorkItem_BoardingBestEffort_Created is the #1107 cross-boundary
// test: a best-effort board-placement failure must NOT 502 and orphan the
// created issue. The provider returns a CreatedItem with Boarded=false and
// a BoardingError set (the issue exists); the handler must return 201 with
// boarded:false and the cause echoed in boarding_error, exercising the
// provider-return -> handler -> wire-response seam (cf. #618).
func TestFileWorkItem_BoardingBestEffort_Created(t *testing.T) {
	fp := &fakeWorkProvider{boardingError: "workmgmt/github: status \"Backlog\" is not a Status option on the project"}
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Board placement will fail but the issue lands",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 not 502 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if resp.Number != 4242 || resp.URL == "" {
		t.Errorf("created issue not echoed: %+v", resp)
	}
	if resp.Boarded {
		t.Errorf("boarded = true, want false on a board-placement failure")
	}
	if !strings.Contains(resp.BoardingError, "is not a Status option") {
		t.Errorf("boarding_error should carry the cause, got %q", resp.BoardingError)
	}

	// The wire JSON the MCP FiledWorkItem mirror decodes must carry boarded
	// as a present field (always set; required). Decode the raw body into a
	// shape with the same json tag to prove the seam.
	var mirror struct {
		Boarded       bool   `json:"boarded"`
		BoardingError string `json:"boarding_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mirror); err != nil {
		t.Fatalf("decode mirror: %v", err)
	}
	if mirror.Boarded {
		t.Errorf("mirror boarded = true, want false")
	}
	if mirror.BoardingError == "" {
		t.Errorf("mirror boarding_error empty, want the cause")
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

// TestFileWorkItem_RunRepoMismatch_Forbidden asserts the #1005 fix-up
// run-to-repo consistency gate: even a caller holding the run's own
// run-bound token cannot file against — or borrow the installation of —
// a different repository than that run's, so a run_id whose run belongs
// to a different repo than the filing target is rejected 403 before any
// provider dispatch or audit write.
func TestFileWorkItem_RunRepoMismatch_Forbidden(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	au := newAuditFake()
	rr := newPromptRunRepo()
	runID := uuid.New()
	inst := int64(99)
	rr.getRuns[runID] = &run.Run{
		ID:             runID,
		Repo:           "someone-else/private-repo",
		State:          run.StateRunning,
		InstallationID: &inst,
	}
	s := New(Config{AuditRepo: au, RunRepo: rr})

	// Entitled caller (run-bound token for runID) but a cross-repo
	// filing target: the entitlement gate passes, the repo gate trips.
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Borrow another run's installation",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		RunID:     runID.String(),
	}, "mcp:run:"+runID.String())

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "run_repo_mismatch" {
		t.Errorf("code = %q, want run_repo_mismatch", env.Error.Code)
	}
	if fp.called {
		t.Error("provider dispatched despite repo mismatch")
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 0 {
		t.Errorf("appended %d audit entries on a mismatched run, want 0", len(au.appended))
	}
}

// TestFileWorkItem_UnauthorizedRun_Forbidden asserts the #1005 fix-up
// caller-to-run entitlement gate for the same-repo case: a caller that
// supplies an in-flight run's UUID in the SAME repo but is NOT that run's
// own run-bound agent token is rejected 403 run_not_entitled before any
// provider dispatch or audit write. This closes the cross-run audit-write
// surface — an authenticated caller cannot inject a work_item_filed entry
// onto a run's hash chain under their own actor_subject just by knowing
// the run UUID. Both an un-bound caller and a caller bound to a different
// run are covered.
func TestFileWorkItem_UnauthorizedRun_Forbidden(t *testing.T) {
	runID := uuid.New()
	newServer := func() (*Server, *fakeWorkProvider, *auditFake) {
		fp := &fakeWorkProvider{}
		registerFakeProvider(t, fp)
		au := newAuditFake()
		rr := newPromptRunRepo()
		inst := int64(99)
		rr.getRuns[runID] = &run.Run{
			ID:             runID,
			Repo:           "kuhlman-labs/fishhawk", // SAME repo as the filing target
			State:          run.StateRunning,
			InstallationID: &inst,
		}
		return New(Config{AuditRepo: au, RunRepo: rr}), fp, au
	}

	cases := []struct {
		name    string
		subject string
	}{
		{"not run-bound", "github:operator"},
		{"bound to a different run", "mcp:run:" + uuid.New().String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fp, au := newServer()
			rec := fileWorkItem(t, s, workItemRequest{
				Repo:      "kuhlman-labs/fishhawk",
				Type:      "chore",
				Summary:   "Inject an entry onto someone else's run",
				TitleVars: map[string]string{"epic": "22", "n": "7"},
				RunID:     runID.String(),
			}, tc.subject)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			var env errorEnvelope
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Code != "run_not_entitled" {
				t.Errorf("code = %q, want run_not_entitled", env.Error.Code)
			}
			if fp.called {
				t.Error("provider dispatched for an unentitled run_id")
			}
			au.mu.Lock()
			defer au.mu.Unlock()
			if len(au.appended) != 0 {
				t.Errorf("appended %d audit entries for an unentitled run, want 0", len(au.appended))
			}
		})
	}
}

// TestFileWorkItem_RunResolutionGuards covers the run-resolution and
// size-cap error branches that accepting both repo and run_id introduced.
func TestFileWorkItem_RunResolutionGuards(t *testing.T) {
	t.Run("invalid run_id UUID", func(t *testing.T) {
		fp := &fakeWorkProvider{}
		registerFakeProvider(t, fp)
		s := New(Config{RunRepo: newPromptRunRepo()})
		rec := fileWorkItem(t, s, workItemRequest{
			Repo: "kuhlman-labs/fishhawk", Type: "chore", Summary: "x", RunID: "not-a-uuid",
			TitleVars: map[string]string{"epic": "22", "n": "7"},
		}, "github:operator")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
		var env errorEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error.Code != "validation_failed" {
			t.Errorf("code = %q, want validation_failed", env.Error.Code)
		}
		if fp.called {
			t.Error("provider dispatched on invalid run_id")
		}
	})

	t.Run("run not found", func(t *testing.T) {
		fp := &fakeWorkProvider{}
		registerFakeProvider(t, fp)
		s := New(Config{RunRepo: newPromptRunRepo()}) // empty repo -> ErrNotFound
		// Run-bound caller for this run_id so the entitlement gate passes
		// and the not-found lookup branch is exercised.
		rid := uuid.New()
		rec := fileWorkItem(t, s, workItemRequest{
			Repo: "kuhlman-labs/fishhawk", Type: "chore", Summary: "x", RunID: rid.String(),
			TitleVars: map[string]string{"epic": "22", "n": "7"},
		}, "mcp:run:"+rid.String())
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
		var env errorEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error.Code != "run_not_found" {
			t.Errorf("code = %q, want run_not_found", env.Error.Code)
		}
	})

	t.Run("run lookup unconfigured", func(t *testing.T) {
		fp := &fakeWorkProvider{}
		registerFakeProvider(t, fp)
		s := New(Config{}) // no RunRepo
		// Run-bound caller for this run_id so the entitlement gate passes
		// and the unconfigured-RunRepo branch is exercised.
		rid := uuid.New()
		rec := fileWorkItem(t, s, workItemRequest{
			Repo: "kuhlman-labs/fishhawk", Type: "chore", Summary: "x", RunID: rid.String(),
			TitleVars: map[string]string{"epic": "22", "n": "7"},
		}, "mcp:run:"+rid.String())
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
		}
		var env errorEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error.Code != "run_lookup_unconfigured" {
			t.Errorf("code = %q, want run_lookup_unconfigured", env.Error.Code)
		}
	})

	t.Run("body too large", func(t *testing.T) {
		s := New(Config{})
		// Build a raw body that exceeds the cap without routing through the
		// typed marshal helper, so the size guard (not field validation) trips.
		oversize := bytes.Repeat([]byte("a"), maxWorkItemRequestBytes+1)
		raw, _ := json.Marshal(workItemRequest{
			Repo: "kuhlman-labs/fishhawk", Type: "chore", Summary: "x", Body: string(oversize),
		})
		req := httptest.NewRequest(http.MethodPost, "/v0/work-items", bytes.NewReader(raw))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: "github:operator"}))
		rec := httptest.NewRecorder()
		s.handleFileWorkItem(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 (body=%s)", rec.Code, rec.Body.String())
		}
		var env errorEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error.Code != "body_too_large" {
			t.Errorf("code = %q, want body_too_large", env.Error.Code)
		}
	})
}

// newInstallationGitHubClient builds a *githubclient.Client whose
// GET /repos/{owner}/{repo}/installation endpoint answers with the given
// installation id, or 404 (App-not-installed -> githubclient.ErrNotInstalled)
// when notInstalled is true. Mirrors the lineage_test.go stub pattern.
func newInstallationGitHubClient(t *testing.T, installID int64, notInstalled bool) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		if notInstalled {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d}`, installID)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// TestFileWorkItem_NoRun_ResolvesInstallation is the cross-boundary test
// for #1095: a run-absent operator filing must resolve the App's
// installation for the target repo so the provider receives a non-zero
// Target.Scope. It drives a real POST through handleFileWorkItem
// with a stub installation endpoint and asserts the fakeWorkProvider
// captured the resolved id — the handler -> GitHub-resolver -> provider
// seam a per-layer unit would miss.
func TestFileWorkItem_NoRun_ResolvesInstallation(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	const wantInst = int64(7788)
	gh := newInstallationGitHubClient(t, wantInst, false)
	s := New(Config{GitHub: gh})

	// Non-run-bound operator caller, no run and no run_id: the run-absent
	// ADR-040 follow-up filing path.
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Operator follow-up filing",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.called {
		t.Fatal("provider was not called")
	}
	if fp.captured.Target.Scope != forge.FromGitHubInstallationID(wantInst) {
		t.Errorf("provider Target.Scope = %q, want scope for installation %d (resolved from the stub installation endpoint)",
			fp.captured.Target.Scope.Ref(), wantInst)
	}
}

// TestFileWorkItem_NoRun_NoInstallation_FailsClosed pins the preserved
// fail-closed for the genuinely-unresolvable case: the App is not
// installed on the repo (404 -> githubclient.ErrNotInstalled), so the
// handler leaves InstallationID 0 and proceeds, and the provider fails
// closed -> 502 work_item_filing_failed at the handler boundary.
func TestFileWorkItem_NoRun_NoInstallation_FailsClosed(t *testing.T) {
	fp := &fakeWorkProvider{failIfNoInstallation: true}
	registerFakeProvider(t, fp)

	gh := newInstallationGitHubClient(t, 0, true) // 404 -> ErrNotInstalled
	s := New(Config{GitHub: gh})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "No installation on this repo",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, "github:operator")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_filing_failed" {
		t.Errorf("code = %q, want work_item_filing_failed", env.Error.Code)
	}
	if !fp.captured.Target.Scope.IsZero() {
		t.Errorf("Target.Scope = %q, want zero scope (left unresolved on ErrNotInstalled)", fp.captured.Target.Scope.Ref())
	}
}

// TestFileWorkItem_NoRun_ResolutionError_BadGateway pins the distinct
// handler-side resolution-error branch: a transient/non-ErrNotInstalled
// GetRepoInstallation failure (the installation endpoint returns a 5xx,
// which classifyStatus maps to a non-ErrNotInstalled error) is surfaced
// as 502 work_item_filing_failed by the handler ITSELF, before provider
// dispatch — not masked as the provider's "no installation" message.
// This is a different code path than TestFileWorkItem_NoRun_NoInstallation_FailsClosed,
// which reaches 502 through the ErrNotInstalled-leaves-0 path and the
// provider's own fail-closed. Assert the provider was NOT dispatched.
func TestFileWorkItem_NoRun_ResolutionError_BadGateway(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	// Installation endpoint returns 500 -> non-ErrNotInstalled error.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"server error"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	s := New(Config{GitHub: gh})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Installation lookup is transiently unavailable",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, "github:operator")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_filing_failed" {
		t.Errorf("code = %q, want work_item_filing_failed", env.Error.Code)
	}
	if fp.called {
		t.Error("provider dispatched despite a resolution error (want handler-side 502 before dispatch)")
	}
}

// TestFileWorkItem_RunBound_RunAbsent_Forbidden pins the binding authz
// condition for #1095: a run-bound agent token (mcp:run:<uuid> subject)
// that files run-absent (no run_id) MUST be rejected 403 before any
// GetRepoInstallation call or provider dispatch. The run-absent
// installation-resolution path is operator-only; a run-bound token must
// file run-scoped (supply its own repo-consistency-checked run_id) so it
// cannot resolve an installation for an arbitrary App-installed repo (the
// confused-deputy egress #1005 closed, via the run-absent door).
func TestFileWorkItem_RunBound_RunAbsent_Forbidden(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	// A GitHub client whose installation endpoint must NOT be hit: the
	// authz gate rejects before any resolution.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("GetRepoInstallation called; want rejected before installation resolution")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	s := New(Config{GitHub: gh})

	// Run-bound agent token but NO run_id supplied: it must not be able to
	// use the run-absent door.
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Sneak through the run-absent door",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, "mcp:run:"+uuid.New().String())

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "run_scoped_filing_required" {
		t.Errorf("code = %q, want run_scoped_filing_required", env.Error.Code)
	}
	if fp.called {
		t.Error("provider dispatched for a run-bound run-absent filing")
	}
}

// TestFileWorkItem_RunBound_RunAbsent_GitLab_Forbidden pins the run-bound
// rejection as PROVIDER-INDEPENDENT (ADR-058 #1856 fix-up): a run-bound agent
// token filing run-absent against a resolved provider: gitlab MUST be rejected
// 403 run_scoped_filing_required, exactly as it is for github_projects. The
// forge-optional gate resolves GitHub-specific installation only for
// github_projects, but the authz invariant — a run-scoped token may not widen
// its authority to make unscoped filings via the run-absent door — holds for
// every provider (gitlab/jira file with deployment-wide server-side
// credentials, so the same widening applies). No GitHub client is configured,
// proving the rejection does not depend on GitHub egress being wired.
func TestFileWorkItem_RunBound_RunAbsent_GitLab_Forbidden(t *testing.T) {
	fp := &fakeWorkProvider{name: workmgmtgitlab.ProviderName}
	workmgmt.Register(fp)

	conv := workmgmt.Default()
	conv.Provider = workmgmtgitlab.ProviderName
	conv.GitLab = &workmgmt.GitLabConnection{Project: "group/app"}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	s := New(Config{})

	// Run-bound agent token, NO run_id supplied: the run-absent door is
	// operator-only regardless of the resolved provider.
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Sneak an unscoped gitlab filing through the run-absent door",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, "mcp:run:"+uuid.New().String())

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "run_scoped_filing_required" {
		t.Errorf("code = %q, want run_scoped_filing_required", env.Error.Code)
	}
	if fp.called {
		t.Error("gitlab provider dispatched for a run-bound run-absent filing")
	}
}

// TestFileWorkItem_RunBound_RunScoped_GitLab_Dispatches pins the load-bearing
// POSITIVE run-scoped door (#2024 / E45.17): a run-bound agent token that
// supplies its own repo-consistency-checked run_id for a run carrying a nil
// InstallationID (every gitlab run) MUST dispatch successfully, even though
// Target.Scope stays zero. The gate is keyed on activeRun == nil, NOT on
// Target.Scope.IsZero() — this test proves it by configuring a GitHub client
// (whose installation endpoint must never be hit for a gitlab-provider
// filing, since the resolution branch is gated on
// conv.Provider == github_projects). A refactor that re-keys the gate onto
// Target.Scope.IsZero() would reject this filing and turn this test red.
func TestFileWorkItem_RunBound_RunScoped_GitLab_Dispatches(t *testing.T) {
	fp := &fakeWorkProvider{name: workmgmtgitlab.ProviderName}
	workmgmt.Register(fp)

	conv := workmgmt.Default()
	conv.Provider = workmgmtgitlab.ProviderName
	conv.GitLab = &workmgmt.GitLabConnection{Project: "group/app"}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	au := newAuditFake()
	rr := newPromptRunRepo()
	runID := uuid.New()
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning}

	// GitHub client configured so a refactor that re-keys the gate onto
	// Target.Scope.IsZero() (rather than activeRun == nil) is actually
	// exercised: with GitHub nil, the pre-diff predicate
	// (target.Scope.IsZero() && s.cfg.GitHub != nil) would already be false
	// and this test would pass for the wrong reason. Its /repos/ endpoint
	// must never be hit — installation resolution only runs for
	// conv.Provider == github_projects.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("GetRepoInstallation called; want no GitHub resolution for a gitlab-provider filing")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	s := New(Config{RunRepo: rr, AuditRepo: au, GitHub: gh})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "File a gitlab issue via the run-scoped door",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		RunID:     runID.String(),
	}, "mcp:run:"+runID.String())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.called {
		t.Fatal("gitlab provider was not called")
	}
	if !fp.captured.Target.Scope.IsZero() {
		t.Errorf("provider Target.Scope = %q, want zero (no GitHub resolveRepoScope for a gitlab-provider filing)", fp.captured.Target.Scope.Ref())
	}
	resp := decodeWorkItem(t, rec)
	if resp.Provider != workmgmtgitlab.ProviderName {
		t.Errorf("provider = %q, want %q", resp.Provider, workmgmtgitlab.ProviderName)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category == categoryWorkItemFiled {
			found = true
		}
	}
	if !found {
		t.Errorf("no work_item_filed audit entry; appended=%d", len(au.appended))
	}
}

// TestFileWorkItem_RunBound_RunScoped_GitHubProjects_NilInstallation_Dispatches
// is the github_projects twin of the gitlab test above: a run-bound token
// whose run carries a nil InstallationID (e.g. a github_projects run whose
// installation lookup never resolved) files run-scoped. The run-scoped door
// lets this filing proceed (activeRun != nil) and, because Target.Scope
// starts zero and the provider IS github_projects, it newly enters
// resolveRepoScope — exactly as the diff intends.
func TestFileWorkItem_RunBound_RunScoped_GitHubProjects_NilInstallation_Dispatches(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	au := newAuditFake()
	rr := newPromptRunRepo()
	runID := uuid.New()
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning}

	const wantInst = int64(5566)
	gh := newInstallationGitHubClient(t, wantInst, false)
	s := New(Config{RunRepo: rr, AuditRepo: au, GitHub: gh})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "File via the run-scoped door with a nil-installation run",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		RunID:     runID.String(),
	}, "mcp:run:"+runID.String())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.called {
		t.Fatal("provider was not called")
	}
	resp := decodeWorkItem(t, rec)
	if resp.Provider != workmgmtgithub.ProviderName {
		t.Errorf("provider = %q, want %q", resp.Provider, workmgmtgithub.ProviderName)
	}
	if fp.captured.Target.Scope != forge.FromGitHubInstallationID(wantInst) {
		t.Errorf("provider Target.Scope = %q, want scope for installation %d (resolved via resolveRepoScope)",
			fp.captured.Target.Scope.Ref(), wantInst)
	}
}

// newEpicGitHubClient builds a *githubclient.Client whose installation
// endpoint answers with installID and whose single-issue endpoint answers
// with epicTitle — the harness for the #1184 epic auto-derivation seam
// (handler -> GetRepoInstallation -> GetIssue -> title render).
func newEpicGitHubClient(t *testing.T, installID int64, epicTitle string) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d}`, installID)
	})
	mux.HandleFunc("GET /repos/{owner}/{name}/issues/{number}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{"number": 389, "title": epicTitle, "state": "open"})
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// TestFileWorkItem_EpicDerivedFromParent is the #1184 cross-boundary seam:
// a child filing supplies only {n} and parent_epic; the handler reads the
// parent epic issue's [E22] title via GetIssue and derives the {epic} var,
// so the rendered title is [E22.1]. Drives the full handler ->
// GetRepoInstallation -> GetIssue -> Apply.renderTitle -> provider chain.
func TestFileWorkItem_EpicDerivedFromParent(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	gh := newEpicGitHubClient(t, 7788, "[E22] The parent epic")
	s := New(Config{GitHub: gh})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "bug",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"}, // epic omitted, auto-derived
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if resp.Title != "[E22.1] Fix the widget" {
		t.Errorf("title = %q, want [E22.1] Fix the widget (epic derived from parent)", resp.Title)
	}
	if fp.captured.Item.Title != "[E22.1] Fix the widget" {
		t.Errorf("provider Item.Title = %q, want the derived title", fp.captured.Item.Title)
	}
}

// TestFileWorkItem_EpicDerived_TitleVarsOmitted is the binding condition (1)
// nil-map-guard path: title_vars omitted ENTIRELY with parent_epic set must
// derive {epic} into a freshly-allocated map, render the title, and not
// panic. Uses a conventions type whose title_format references only {epic}
// so the rendered title needs no {n}.
func TestFileWorkItem_EpicDerived_TitleVarsOmitted(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	conv := workmgmt.Conventions{
		Provider: workmgmt.Default().Provider, // github_projects -> the fake resolves
		Types: map[string]workmgmt.ItemType{
			"feature": {
				TitleFormat:   "[E{epic}] {summary}",
				BodySkeleton:  []string{"Summary"},
				DefaultLabels: []string{"type:feature"},
				DefaultFields: workmgmt.DefaultFields{Status: "Backlog", Complexity: "medium"},
				EpicLink:      "optional",
			},
		},
	}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	gh := newEpicGitHubClient(t, 7788, "[E22] The parent epic")
	s := New(Config{GitHub: gh})

	// title_vars omitted entirely (nil map): the nil-map guard must allocate.
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Ship it",
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, no panic (body=%s)", rec.Code, rec.Body.String())
	}
	if resp := decodeWorkItem(t, rec); resp.Title != "[E22] Ship it" {
		t.Errorf("title = %q, want [E22] Ship it", resp.Title)
	}
}

// TestFileWorkItem_EpicDerivation_FailsClosed pins binding condition (3):
// every epic-derivation failure mode leaves {epic} unset so the title fails
// closed with the structured missing-placeholder 422 — never a wrong title
// or a crash. Covers a GitHub client absent and a parent title with no
// [E<n>] token; both assert details.missing_placeholders includes "epic".
func TestFileWorkItem_EpicDerivation_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		gh   func(t *testing.T) *githubclient.Client
	}{
		{"github absent", func(*testing.T) *githubclient.Client { return nil }},
		{"parent title has no [E..] token", func(t *testing.T) *githubclient.Client {
			return newEpicGitHubClient(t, 7788, "A plain title with no epic token")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeWorkProvider{}
			registerFakeProvider(t, fp)
			s := New(Config{GitHub: tc.gh(t)})

			rec := fileWorkItem(t, s, workItemRequest{
				Repo:      "kuhlman-labs/fishhawk",
				Type:      "bug",
				Summary:   "Fix the widget",
				TitleVars: map[string]string{"n": "1"}, // epic cannot be derived
				Relations: &workItemRelations{ParentEpic: "#389"},
			}, "github:operator")

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
			}
			var env errorEnvelope
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Code != "work_item_invalid" {
				t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
			}
			missing, _ := env.Error.Details["missing_placeholders"].([]any)
			if !containsStr(missing, "epic") {
				t.Errorf("details.missing_placeholders = %v, want it to include epic", env.Error.Details["missing_placeholders"])
			}
			if fp.called {
				t.Error("provider dispatched despite a fail-closed title render")
			}
		})
	}
}

// TestFileWorkItem_OffSkeletonSection_Unprocessable pins binding condition
// (2): a sections key off the type's body skeleton fails loud with a 422
// work_item_invalid carrying details.unknown_sections — never a silent drop.
func TestFileWorkItem_OffSkeletonSection_Unprocessable(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Tidy up",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Sections: map[string]string{
			"Summary": "the real content",
			"Impact":  "off-skeleton content that must not be silently dropped",
		},
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	unknown, _ := env.Error.Details["unknown_sections"].([]any)
	if !containsStr(unknown, "Impact") {
		t.Errorf("details.unknown_sections = %v, want it to include Impact", env.Error.Details["unknown_sections"])
	}
	if _, ok := env.Error.Details["expected_sections"]; !ok {
		t.Errorf("details.expected_sections missing: %v", env.Error.Details)
	}
	if fp.called {
		t.Error("provider dispatched despite an off-skeleton section")
	}
}

// containsStr reports whether the JSON-decoded []any slice contains want.
func containsStr(xs []any, want string) bool {
	for _, x := range xs {
		if s, ok := x.(string); ok && s == want {
			return true
		}
	}
	return false
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

// fakeGithubAPI implements github.API so the depends_on end-to-end test can
// drive the REAL workmgmt/github.Provider — exercising its actual
// renderDependsOnMarker write at File time and parseDependsOnMarker read at
// EpicChildren time, not a re-implementation. createdBody captures the body
// the provider handed CreateIssue (where the depends_on marker is stamped);
// subIssues is what ListSubIssues returns for the EpicChildren readback.
type fakeGithubAPI struct {
	createdBody string
	subIssues   []githubclient.SubIssue
}

func (f *fakeGithubAPI) CreateIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	f.createdBody = p.Body
	return &githubclient.CreatedIssue{Number: 4242, NodeID: "CHILD_NODE", HTMLURL: "https://github.com/kuhlman-labs/fishhawk/issues/4242"}, nil
}

func (f *fakeGithubAPI) IssueNodeID(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ int) (string, error) {
	return "EPIC_NODE", nil
}

func (f *fakeGithubAPI) ProjectFields(_ context.Context, _ forge.CredentialScope, _ githubclient.ProjectCoord, _ string) (*githubclient.ProjectMeta, error) {
	return &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD", StatusOptions: map[string]string{"Backlog": "OPT"}}, nil
}

func (f *fakeGithubAPI) ProjectItemStatus(_ context.Context, _ forge.CredentialScope, _, _, _ string) (*githubclient.ProjectItemStatus, error) {
	return &githubclient.ProjectItemStatus{OnBoard: true, ItemID: "ITEM"}, nil
}

func (f *fakeGithubAPI) AddProjectItem(_ context.Context, _ forge.CredentialScope, _, _ string) (string, error) {
	return "ITEM", nil
}

func (f *fakeGithubAPI) SetProjectItemSingleSelect(_ context.Context, _ forge.CredentialScope, _, _, _, _ string) error {
	return nil
}

func (f *fakeGithubAPI) AddSubIssue(_ context.Context, _ forge.CredentialScope, _, _ string) error {
	return nil
}

func (f *fakeGithubAPI) ListSubIssues(_ context.Context, _ forge.CredentialScope, _ string) ([]githubclient.SubIssue, error) {
	return f.subIssues, nil
}

func (f *fakeGithubAPI) SearchIssuesByTitle(_ context.Context, _ forge.CredentialScope, _ string) ([]githubclient.IssueTitleResult, error) {
	return nil, nil
}

// GetIssue satisfies the widened github.API (#2051): the no-epic
// ResolveDependencies path reads each named issue via GetIssue. Minimal stub —
// these tests do not exercise the no-epic path — returning ErrNotFound.
func (f *fakeGithubAPI) GetIssue(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, _ int) (*githubclient.Issue, error) {
	return nil, githubclient.ErrNotFound
}

func (f *fakeGithubAPI) ProjectsTokenConfigured() bool { return true }

// ListRepoIssues satisfies the work-item read capability's slice of the
// workmgmt/github API interface (#2230). This fake exercises the FILING and
// board-sync paths, which never enumerate issues, so it is a mechanical stub.
func (f *fakeGithubAPI) ListRepoIssues(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ githubclient.ListRepoIssuesOptions) ([]githubclient.RepoIssue, error) {
	return nil, nil
}

// UpdateIssue satisfies the grooming-mutation capability's slice of the
// workmgmt/github API interface (E54.5 / #2237). This fake exercises the
// FILING and board-sync paths, which never edit an existing issue, so it is a
// mechanical stub.
func (f *fakeGithubAPI) UpdateIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ int, _ githubclient.UpdateIssueParams) (*githubclient.Issue, error) {
	return nil, nil
}

// TestFileWorkItem_DependsOn_EndToEnd is the binding-condition end-to-end
// test: a SERIALIZED work-item filing request carrying relations.depends_on
// flows request -> handleFileWorkItem/Apply -> the real github.Provider.File
// (which stamps the depends_on body marker) -> EpicChildren readback (which
// parses the marker back into edges). It integration-tests the
// request->domain->persist->read seam in one test rather than the two halves
// separately, and asserts a reference to a NON-child is dropped from the edge
// set.
func TestFileWorkItem_DependsOn_EndToEnd(t *testing.T) {
	api := &fakeGithubAPI{}
	provider := workmgmtgithub.New(api)
	workmgmt.Register(provider) // registers under "github_projects" (Default().Provider)

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

	// Serialized request: a child carrying depends_on among its epic's
	// siblings (#41, #42) plus a reference to a NON-child (#999) that the
	// readback must drop.
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "slice C depends on A and B",
		TitleVars: map[string]string{"epic": "22", "n": "3"},
		Relations: &workItemRelations{ParentEpic: "#1005", DependsOn: []string{"#41", "42", "#999"}},
		RunID:     runID.String(),
	}, "mcp:run:"+runID.String())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	// Persist: the provider stamped the depends_on marker into the created
	// issue body.
	if !strings.Contains(api.createdBody, "Depends on: #41, #42, #999") {
		t.Fatalf("created body missing depends_on marker:\n%s", api.createdBody)
	}

	// Read: EpicChildren parses the stamped body back. The children set is
	// {41, 42, 4242}; #999 is not a child and must be dropped.
	api.subIssues = []githubclient.SubIssue{
		{Number: 41, NodeID: "N41", Title: "slice A", Body: "## Summary\n\nslice A\n"},
		{Number: 42, NodeID: "N42", Title: "slice B", Body: "## Summary\n\nslice B\n"},
		{Number: 4242, NodeID: "CHILD_NODE", Title: "slice C", Body: api.createdBody},
	}
	res, err := provider.EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(inst), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#1005",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if len(res.Children) != 3 || res.Children[0].Number != 41 || res.Children[2].Number != 4242 {
		t.Fatalf("children = %+v, want ascending 41,42,4242", res.Children)
	}
	wantEdges := []workmgmt.DependsEdge{{From: 4242, To: 41}, {From: 4242, To: 42}}
	if len(res.Edges) != len(wantEdges) {
		t.Fatalf("edges = %+v, want %+v (the #999 non-child reference must be dropped)", res.Edges, wantEdges)
	}
	for i, e := range res.Edges {
		if e != wantEdges[i] {
			t.Fatalf("edge[%d] = %+v, want %+v", i, e, wantEdges[i])
		}
	}
}

// newLabeledEpicGitHubClient serves GetRepoInstallation + GetIssue where the
// parent epic issue carries the given labels (object form) — the stub for the
// area-derivation path (#1616). The title carries an [E22] token so {epic}
// derivation is unaffected; labels drive area derivation.
//
// It is a thin wrapper over newPerIssueLabeledGitHubClient (#3179), so the two
// #1616 area tests keep the exact stub behavior they were written against.
func newLabeledEpicGitHubClient(t *testing.T, installID int64, labels []string, getIssueErr bool) *githubclient.Client {
	t.Helper()
	return newPerIssueLabeledGitHubClient(t, installID, map[int][]string{389: labels}, getIssueErr)
}

// newPerIssueLabeledGitHubClient serves GetRepoInstallation + a PER-ISSUE-NUMBER
// GetIssue, so a test can give the parent epic and the originating run's
// triggering issue DIFFERENT labels — the precedence and fallback cases of the
// #3179 phase ladder need exactly that. An issue number absent from the map is
// served with NO labels (a real issue that simply carries none), not a 404, so
// a derivation that consults the wrong issue derives nothing rather than
// erroring for an unrelated reason.
//
// Every issue's title carries the [E22] token, so {epic} derivation is
// unaffected regardless of which issue a test points the ladder at.
func newPerIssueLabeledGitHubClient(t *testing.T, installID int64, byIssue map[int][]string, getIssueErr bool) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d}`, installID)
	})
	mux.HandleFunc("GET /repos/{owner}/{name}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		if getIssueErr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		labels := byIssue[number]
		labelObjs := make([]map[string]any, 0, len(labels))
		for _, l := range labels {
			labelObjs = append(labelObjs, map[string]any{"name": l})
		}
		body, _ := json.Marshal(map[string]any{"number": number, "title": "[E22] The parent epic", "state": "open", "labels": labelObjs})
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// TestFileWorkItem_LabelCompleteness_Integration is the cross-boundary seam
// for #1616 (verification 6): a feature filing with NO autonomy label and no
// area (no GitHub client to derive it) drives request -> conventions Apply ->
// provider -> response + audit. The response carries defaulted_labels
// [autonomy:medium] and missing_label_namespaces [area phase] (#3179 added the
// phase namespace and it is likewise underivable here); the filed item's
// labels include autonomy:medium; and the run-bound work_item_filed audit
// payload carries both fields.
func TestFileWorkItem_LabelCompleteness_Integration(t *testing.T) {
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
	}, "mcp:run:"+runID.String())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if strings.Join(resp.DefaultedLabels, ",") != "autonomy:medium" {
		t.Errorf("response defaulted_labels = %v, want [autonomy:medium]", resp.DefaultedLabels)
	}
	// Widened for #3179: phase joined the shipped default's required namespaces
	// and, with no GitHub client, is no more derivable than area.
	if strings.Join(resp.MissingLabelNamespaces, ",") != "area,phase" {
		t.Errorf("response missing_label_namespaces = %v, want [area phase]", resp.MissingLabelNamespaces)
	}
	// The filed item's labels (as the provider received them) include the default.
	if !containsString(fp.captured.Item.Classification.Labels, "autonomy:medium") {
		t.Errorf("provider Item labels %v missing autonomy:medium", fp.captured.Item.Classification.Labels)
	}

	// Audit seam: the work_item_filed payload carries both completeness fields.
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category != categoryWorkItemFiled {
			continue
		}
		found = true
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("audit payload: %v", err)
		}
		dl, _ := payload["defaulted_labels"].([]any)
		if !containsStr(dl, "autonomy:medium") {
			t.Errorf("audit defaulted_labels = %v, want it to include autonomy:medium", payload["defaulted_labels"])
		}
		mn, _ := payload["missing_label_namespaces"].([]any)
		if !containsStr(mn, "area") {
			t.Errorf("audit missing_label_namespaces = %v, want it to include area", payload["missing_label_namespaces"])
		}
	}
	if !found {
		t.Fatalf("no work_item_filed audit entry; appended=%d", len(au.appended))
	}
}

// TestFileWorkItem_AreaDerivedFromParentEpic is the area-derivation happy path
// (#1616, verification 7): with a stub GitHub client whose parent-epic
// GetIssue returns labels [epic, area:backend], the filed item's labels
// include area:backend, it appears in the response defaulted_labels, and
// missing_label_namespaces omits area.
func TestFileWorkItem_AreaDerivedFromParentEpic(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	gh := newLabeledEpicGitHubClient(t, 7788, []string{"epic", "area:backend"}, false)
	s := New(Config{GitHub: gh})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"}, // epic auto-derived too
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if !containsString(fp.captured.Item.Classification.Labels, "area:backend") {
		t.Errorf("filed item labels %v missing derived area:backend", fp.captured.Item.Classification.Labels)
	}
	if !containsString(resp.DefaultedLabels, "area:backend") {
		t.Errorf("response defaulted_labels = %v, want it to include area:backend", resp.DefaultedLabels)
	}
	for _, ns := range resp.MissingLabelNamespaces {
		if ns == "area" {
			t.Errorf("missing_label_namespaces = %v, must omit area (it was derived)", resp.MissingLabelNamespaces)
		}
	}
}

// TestFileWorkItem_AreaDerivation_FailsOpen is the area-derivation fail-open
// sibling (#1616, verification 7; widened for phase in #3179 per binding
// approval condition 1): a GetIssue error must NOT fail the filing — it
// succeeds with BOTH area and phase listed in missing_label_namespaces (each
// derivation derived nothing off the same failed fetch). Uses a feature type
// whose title_format needs no {epic}, so the GetIssue failure does not also
// fail title rendering.
func TestFileWorkItem_AreaDerivation_FailsOpen(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)

	// A conventions type that requires area + autonomy + phase but whose title
	// needs no {epic}, so a GetIssue error only affects label derivation.
	conv := workmgmt.Conventions{
		Provider: workmgmt.Default().Provider,
		Types: map[string]workmgmt.ItemType{
			"feature": {
				TitleFormat:             "{summary}",
				BodySkeleton:            []string{"Summary"},
				DefaultLabels:           []string{"type:feature"},
				LabelDefaults:           map[string]string{"autonomy": "autonomy:medium"},
				RequiredLabelNamespaces: []string{"area", "autonomy", "phase"},
				DefaultFields:           workmgmt.DefaultFields{Status: "Backlog", Complexity: "medium"},
				EpicLink:                "optional",
			},
		},
	}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	gh := newLabeledEpicGitHubClient(t, 7788, nil, true) // GetIssue 500s
	s := New(Config{GitHub: gh})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Ship it",
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — area derivation must fail OPEN (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if strings.Join(resp.MissingLabelNamespaces, ",") != "area,phase" {
		t.Errorf("missing_label_namespaces = %v, want [area phase] (both derivations failed open)", resp.MissingLabelNamespaces)
	}
}

// --- phase derivation (#3179) ---

// phaseDerivationServer wires the pieces every phase-derivation case needs: a
// registered capture provider, a per-issue-number GitHub stub, and (optionally)
// a run repository holding the originating run row the evidence-run fallback
// reads. Passing a nil runs map leaves s.cfg.RunRepo NIL — the nil-RunRepo
// fail-open branch.
func phaseDerivationServer(t *testing.T, byIssue map[int][]string, getIssueErr bool, runs map[uuid.UUID]*run.Run) (*Server, *fakeWorkProvider) {
	t.Helper()
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	gh := newPerIssueLabeledGitHubClient(t, 7788, byIssue, getIssueErr)
	cfg := Config{GitHub: gh}
	if runs != nil {
		rr := newPromptRunRepo()
		for id, row := range runs {
			rr.getRuns[id] = row
		}
		cfg.RunRepo = rr
	}
	return New(cfg), fp
}

// seedTriggerRun builds a run row for the evidence-run fallback: repo + an
// `issue:<n>` TriggerRef, the two fields originatingRunIssue reads.
func seedTriggerRun(id uuid.UUID, repo string, issueNumber int) *run.Run {
	ref := "issue:" + strconv.Itoa(issueNumber)
	inst := int64(7788)
	return &run.Run{
		ID:             id,
		Repo:           repo,
		State:          run.StateRunning,
		InstallationID: &inst,
		TriggerRef:     &ref,
	}
}

func missingNamespacesInclude(resp workItemResponse, ns string) bool {
	return containsString(resp.MissingLabelNamespaces, ns)
}

// TestFileWorkItem_PhaseDerivedFromParentEpic is the phase happy path and rung
// 2 of the #3179 ladder: the parent epic carries phase:alpha, so the filed
// item's labels include it, it appears in the response defaulted_labels (a
// system-added label the caller did not supply, so a wrong inherit is
// challengeable at filing time), and phase is absent from
// missing_label_namespaces.
func TestFileWorkItem_PhaseDerivedFromParentEpic(t *testing.T) {
	s, fp := phaseDerivationServer(t, map[int][]string{
		389: {"epic", "area:backend", "phase:alpha"},
	}, false, nil)

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if !containsString(fp.captured.Item.Classification.Labels, "phase:alpha") {
		t.Errorf("filed item labels %v missing derived phase:alpha", fp.captured.Item.Classification.Labels)
	}
	if !containsString(resp.DefaultedLabels, "phase:alpha") {
		t.Errorf("response defaulted_labels = %v, want it to include phase:alpha", resp.DefaultedLabels)
	}
	if missingNamespacesInclude(resp, "phase") {
		t.Errorf("missing_label_namespaces = %v, must omit phase (it was derived)", resp.MissingLabelNamespaces)
	}
}

// TestFileWorkItem_PhaseFallsBackToRunIssue is rung 3: the parent epic carries
// area but NO phase, so the ladder falls through to the originating run's
// triggering issue (#3100), which carries phase:alpha. This is the shape the
// epic_link:optional types (bug, chore — the defer-concern shape) file in, and
// is what makes the ladder more than a naive inherit-from-epic.
func TestFileWorkItem_PhaseFallsBackToRunIssue(t *testing.T) {
	runID := uuid.New()
	s, fp := phaseDerivationServer(t, map[int][]string{
		389:  {"epic", "area:backend"}, // no phase on the epic
		3100: {"phase:alpha"},          // the run's triggering issue
	}, false, map[uuid.UUID]*run.Run{
		runID: seedTriggerRun(runID, "kuhlman-labs/fishhawk", 3100),
	})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Relations: &workItemRelations{ParentEpic: "#389", EvidenceRuns: []string{runID.String()}},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if !containsString(fp.captured.Item.Classification.Labels, "phase:alpha") {
		t.Errorf("filed item labels %v missing phase:alpha derived via the evidence-run fallback", fp.captured.Item.Classification.Labels)
	}
	if !containsString(resp.DefaultedLabels, "phase:alpha") {
		t.Errorf("response defaulted_labels = %v, want it to include phase:alpha", resp.DefaultedLabels)
	}
	if missingNamespacesInclude(resp, "phase") {
		t.Errorf("missing_label_namespaces = %v, must omit phase (the fallback derived it)", resp.MissingLabelNamespaces)
	}
}

// TestFileWorkItem_PhaseEpicWinsOverRunIssue is the PRECEDENCE case #3179
// names: the epic carries phase:beta and the run's triggering issue carries
// phase:alpha, and the epic wins — phase:beta only, with phase:alpha nowhere on
// the item. phase:* describes WHEN an item is scheduled, which follows its
// scheduling home (the epic it rolls up to), not where it was discovered.
//
// The two issues carry DELIBERATELY DIFFERENT phase values by construction, so
// reversing the ladder lands the assertion on the behavioral comparison rather
// than on fixture setup.
func TestFileWorkItem_PhaseEpicWinsOverRunIssue(t *testing.T) {
	runID := uuid.New()
	s, fp := phaseDerivationServer(t, map[int][]string{
		389:  {"epic", "area:backend", "phase:beta"},
		3100: {"phase:alpha"},
	}, false, map[uuid.UUID]*run.Run{
		runID: seedTriggerRun(runID, "kuhlman-labs/fishhawk", 3100),
	})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Relations: &workItemRelations{ParentEpic: "#389", EvidenceRuns: []string{runID.String()}},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	labels := fp.captured.Item.Classification.Labels
	if !containsString(labels, "phase:beta") {
		t.Errorf("filed item labels %v missing the epic's phase:beta", labels)
	}
	if containsString(labels, "phase:alpha") {
		t.Errorf("filed item labels %v carry the RUN ISSUE's phase:alpha; the parent epic must win the ladder", labels)
	}
}

// TestFileWorkItem_CallerPhaseSuppressesDerivation is rung 1: a caller-supplied
// phase:beta is never rewritten, even though the parent epic carries
// phase:alpha. Exactly one phase label survives, it is the caller's, and phase
// is absent from defaulted_labels — the system added nothing.
//
// The caller's value and the epic's are DIFFERENT by construction, so deleting
// the suppression guard produces a visibly different label set rather than a
// byte-identical one.
func TestFileWorkItem_CallerPhaseSuppressesDerivation(t *testing.T) {
	s, fp := phaseDerivationServer(t, map[int][]string{
		389: {"epic", "area:backend", "phase:alpha"},
	}, false, nil)

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Labels:    []string{"phase:beta"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	var phases []string
	for _, l := range fp.captured.Item.Classification.Labels {
		if strings.HasPrefix(l, "phase:") {
			phases = append(phases, l)
		}
	}
	if strings.Join(phases, ",") != "phase:beta" {
		t.Errorf("filed item phase labels = %v, want exactly [phase:beta] — a caller-supplied phase is never rewritten or duplicated", phases)
	}
	for _, d := range resp.DefaultedLabels {
		if strings.HasPrefix(d, "phase:") {
			t.Errorf("response defaulted_labels = %v reports a phase the system did not add", resp.DefaultedLabels)
		}
	}
}

// TestFileWorkItem_PhaseDerivation_FailsOpen_GetIssueError is the fetch-error
// fail-open branch: GetIssue 500s, so nothing is derived and the filing STILL
// returns 201 with phase reported in missing_label_namespaces. A filing is
// never rejected on labels.
func TestFileWorkItem_PhaseDerivation_FailsOpen_GetIssueError(t *testing.T) {
	s, _ := phaseDerivationServer(t, map[int][]string{
		389: {"epic", "area:backend", "phase:alpha"},
	}, true /* GetIssue 500s */, nil)

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"epic": "22", "n": "1"}, // supplied: {epic} derivation also fails
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — phase derivation must fail OPEN (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if !missingNamespacesInclude(resp, "phase") {
		t.Errorf("missing_label_namespaces = %v, want it to include phase (derivation failed open)", resp.MissingLabelNamespaces)
	}
}

// TestFileWorkItem_PhaseDerivation_RefusesForeignRun is the SAME-REPO GUARD.
// Relations.EvidenceRuns is caller-supplied on POST /v0/work-items and is NOT
// entitlement-checked, so a run row belonging to a DIFFERENT repository must
// derive NOTHING — reading a foreign run's triggering issue number and applying
// its phase against this target repo would derive a wrong label from an unowned
// row.
//
// Seeded BY CONSTRUCTION: the run row's Repo is other-org/other-repo while the
// filing targets kuhlman-labs/fishhawk, and issue 3100 in the stub carries
// phase:alpha — so with the guard deleted the derivation SUCCEEDS and the
// assertion goes red on the behavioral outcome, not on a fetch failure.
func TestFileWorkItem_PhaseDerivation_RefusesForeignRun(t *testing.T) {
	runID := uuid.New()
	s, fp := phaseDerivationServer(t, map[int][]string{
		389:  {"epic", "area:backend"}, // no phase on the epic -> the ladder reaches rung 3
		3100: {"phase:alpha"},
	}, false, map[uuid.UUID]*run.Run{
		runID: seedTriggerRun(runID, "other-org/other-repo", 3100),
	})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Relations: &workItemRelations{ParentEpic: "#389", EvidenceRuns: []string{runID.String()}},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if containsString(fp.captured.Item.Classification.Labels, "phase:alpha") {
		t.Errorf("filed item labels %v derived a phase from a run in a DIFFERENT repository; the same-repo guard must refuse it", fp.captured.Item.Classification.Labels)
	}
	if !missingNamespacesInclude(resp, "phase") {
		t.Errorf("missing_label_namespaces = %v, want it to include phase (nothing was derivable)", resp.MissingLabelNamespaces)
	}
}

// TestFileWorkItem_PhaseDerivation_NoEvidenceRun is the no-fallback-signal
// branch: no evidence_runs and an epic carrying no phase, so both rungs yield
// nothing and phase is reported LOUDLY rather than silently absent.
func TestFileWorkItem_PhaseDerivation_NoEvidenceRun(t *testing.T) {
	s, _ := phaseDerivationServer(t, map[int][]string{
		389: {"epic", "area:backend"},
	}, false, map[uuid.UUID]*run.Run{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if !missingNamespacesInclude(resp, "phase") {
		t.Errorf("missing_label_namespaces = %v, want it to include phase", resp.MissingLabelNamespaces)
	}
}

// TestFileWorkItem_PhaseDerivation_NilRunRepo is the unwired-dependency
// fail-open branch: an evidence run IS supplied but s.cfg.RunRepo is nil, so
// the fallback cannot resolve the run and derives nothing — 201, with phase
// reported missing. Issue 3100 carries phase:alpha in the stub, so this cannot
// pass merely because there was no phase to find.
func TestFileWorkItem_PhaseDerivation_NilRunRepo(t *testing.T) {
	s, fp := phaseDerivationServer(t, map[int][]string{
		389:  {"epic", "area:backend"},
		3100: {"phase:alpha"},
	}, false, nil /* RunRepo left NIL */)

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Relations: &workItemRelations{ParentEpic: "#389", EvidenceRuns: []string{uuid.NewString()}},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a nil run repository must fail OPEN (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if containsString(fp.captured.Item.Classification.Labels, "phase:alpha") {
		t.Errorf("filed item labels %v derived a phase with no run repository wired", fp.captured.Item.Classification.Labels)
	}
	if !missingNamespacesInclude(resp, "phase") {
		t.Errorf("missing_label_namespaces = %v, want it to include phase", resp.MissingLabelNamespaces)
	}
}

// TestFileWorkItem_PhaseDerivation_UnparseableEvidenceRun covers the remaining
// two rung-3 refusals in one behavioral pass: an evidence_runs entry that is
// not a UUID, and a well-formed id naming a run the repository does not hold
// (GetRun error). Both derive nothing and both return 201 with phase reported.
func TestFileWorkItem_PhaseDerivation_UnparseableEvidenceRun(t *testing.T) {
	for name, ref := range map[string]string{
		"not_a_uuid":  "run-42",
		"unknown_run": uuid.NewString(),
	} {
		t.Run(name, func(t *testing.T) {
			s, fp := phaseDerivationServer(t, map[int][]string{
				389:  {"epic", "area:backend"},
				3100: {"phase:alpha"},
			}, false, map[uuid.UUID]*run.Run{})

			rec := fileWorkItem(t, s, workItemRequest{
				Repo:      "kuhlman-labs/fishhawk",
				Type:      "feature",
				Summary:   "Fix the widget",
				TitleVars: map[string]string{"n": "1"},
				Relations: &workItemRelations{ParentEpic: "#389", EvidenceRuns: []string{ref}},
			}, "github:operator")

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}
			resp := decodeWorkItem(t, rec)
			if containsString(fp.captured.Item.Classification.Labels, "phase:alpha") {
				t.Errorf("filed item labels %v derived a phase from an unresolvable evidence run", fp.captured.Item.Classification.Labels)
			}
			if !missingNamespacesInclude(resp, "phase") {
				t.Errorf("missing_label_namespaces = %v, want it to include phase", resp.MissingLabelNamespaces)
			}
		})
	}
}

// TestFileWorkItem_PhaseDerivation_NoTriggerIssue is the run-without-an-issue
// refusal: the run row is same-repo and resolvable but its TriggerRef is nil
// (a non-issue-triggered run), so originatingRunIssue reports not-ok and
// nothing is derived.
func TestFileWorkItem_PhaseDerivation_NoTriggerIssue(t *testing.T) {
	runID := uuid.New()
	row := seedTriggerRun(runID, "kuhlman-labs/fishhawk", 3100)
	row.TriggerRef = nil
	s, fp := phaseDerivationServer(t, map[int][]string{
		389:  {"epic", "area:backend"},
		3100: {"phase:alpha"},
	}, false, map[uuid.UUID]*run.Run{runID: row})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Fix the widget",
		TitleVars: map[string]string{"n": "1"},
		Relations: &workItemRelations{ParentEpic: "#389", EvidenceRuns: []string{runID.String()}},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if containsString(fp.captured.Item.Classification.Labels, "phase:alpha") {
		t.Errorf("filed item labels %v derived a phase from a run with no issue TriggerRef", fp.captured.Item.Classification.Labels)
	}
	if !missingNamespacesInclude(resp, "phase") {
		t.Errorf("missing_label_namespaces = %v, want it to include phase", resp.MissingLabelNamespaces)
	}
}

// TestFileWorkItem_PhaseNotDerivedForExemptType is the type-gate branch: the
// adr type declares NO phase namespace (neither required_label_namespaces nor
// label_defaults), so derivation must not run for it even when a phase:alpha
// label is in reach — the #1616 type-exemption posture, unchanged by #3179.
// The signal is put on the EVIDENCE-RUN rung, since the adr type refuses a
// parent-epic relation outright.
func TestFileWorkItem_PhaseNotDerivedForExemptType(t *testing.T) {
	runID := uuid.New()
	s, fp := phaseDerivationServer(t, map[int][]string{
		3100: {"phase:alpha"},
	}, false, map[uuid.UUID]*run.Run{
		runID: seedTriggerRun(runID, "kuhlman-labs/fishhawk", 3100),
	})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:            "kuhlman-labs/fishhawk",
		Type:            "adr",
		Summary:         "Choose the derivation ladder",
		ExistingNumbers: []int{0},
		Relations:       &workItemRelations{EvidenceRuns: []string{runID.String()}},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if containsString(fp.captured.Item.Classification.Labels, "phase:alpha") {
		t.Errorf("adr filing labels %v gained a derived phase; adr declares no phase namespace", fp.captured.Item.Classification.Labels)
	}
	if len(resp.MissingLabelNamespaces) != 0 {
		t.Errorf("missing_label_namespaces = %v, want none for an exempt type", resp.MissingLabelNamespaces)
	}
}

// --- child-number discovery (#1958) ---

// fakeChildNumberProvider is a workmgmt.Provider that ALSO implements the
// optional workmgmt.EpicChildrenQuerier capability (#1958), so the handler's
// child-number discovery seam (handler -> EpicChildrenQuerier -> NextChildNumber
// -> rendered title) is exercised end to end. It is concurrency-safe so the
// two-parallel-filings test can share one instance: when appendOnFile is set,
// File records the just-filed child title so a subsequent EpicChildren reflects
// it — the mechanism that lets serialized filings allocate distinct numbers.
type fakeChildNumberProvider struct {
	name string

	mu           sync.Mutex
	children     []workmgmt.EpicChild
	epicErr      error
	epicCalls    int
	epicReq      workmgmt.EpicChildrenRequest
	fileCalls    int
	lastFileReq  workmgmt.ProviderRequest
	filedTitles  []string
	appendOnFile bool
	number       int
	// childCap is the provider-declared hard child cap the fake reports on its
	// EpicChildrenResult (#3714). 0 (the default) means "no cap declared", which
	// keeps every pre-#3714 fixture's guard inert and its behaviour unchanged.
	childCap int

	// EpicChildren-read barrier (#1958, binding condition 1). When barrierN > 0,
	// each EpicChildren call reports arrival and blocks until barrierN calls have
	// arrived OR barrierWait elapses, BEFORE it snapshots f.children. This forces
	// concurrent omitted-n filings to observe the SAME child snapshot: an
	// implementation WITHOUT the per-epic lock overlaps both reads (both see
	// [E7.1],[E7.2] -> both allocate [E7.3] -> collision, the test fails),
	// whereas the correct locked implementation can never have two goroutines
	// inside EpicChildren at once, so the lone arrival times out and the reads
	// stay serialized (distinct [E7.3],[E7.4]) — the timeout is why the barrier
	// exercises the race without deadlocking under the very lock it guards.
	barrierN    int
	barrierWait time.Duration
	barrierArr  int
	barrierCh   chan struct{}
}

// awaitBarrier blocks until barrierN EpicChildren calls have arrived (all
// released together) or barrierWait elapses (a lone arrival proceeds). It is a
// no-op when barrierN <= 0. The channel wait happens WITHOUT f.mu held so the
// released goroutines contend for the snapshot read fairly.
func (f *fakeChildNumberProvider) awaitBarrier() {
	f.mu.Lock()
	if f.barrierN <= 0 {
		f.mu.Unlock()
		return
	}
	if f.barrierCh == nil {
		f.barrierCh = make(chan struct{})
	}
	ch := f.barrierCh
	wait := f.barrierWait
	f.barrierArr++
	if f.barrierArr >= f.barrierN {
		select {
		case <-ch: // already released
		default:
			close(ch)
		}
	}
	f.mu.Unlock()

	select {
	case <-ch:
	case <-time.After(wait):
	}
}

func (f *fakeChildNumberProvider) Name() string { return f.name }

func (f *fakeChildNumberProvider) EpicChildren(_ context.Context, req workmgmt.EpicChildrenRequest) (*workmgmt.EpicChildrenResult, error) {
	f.awaitBarrier() // force concurrent filings to snapshot the same children
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epicCalls++
	f.epicReq = req
	if f.epicErr != nil {
		return nil, f.epicErr
	}
	out := make([]workmgmt.EpicChild, len(f.children))
	copy(out, f.children)
	return &workmgmt.EpicChildrenResult{Children: out, ChildCap: f.childCap}, nil
}

func (f *fakeChildNumberProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileCalls++
	f.lastFileReq = req
	f.filedTitles = append(f.filedTitles, req.Item.Title)
	if f.appendOnFile {
		f.children = append(f.children, workmgmt.EpicChild{Title: req.Item.Title})
	}
	f.number++
	return &workmgmt.CreatedItem{
		Provider:      f.name,
		Number:        4242 + f.number,
		URL:           "https://github.com/kuhlman-labs/fishhawk/issues/4242",
		AppliedLabels: req.Item.Classification.Labels,
		Boarded:       true,
	}, nil
}

// registerFakeChildNumberProvider registers a child-number-capable fake under
// the default provider id so the handler's workmgmt.Get + EpicChildrenQuerier
// assert resolve it.
func registerFakeChildNumberProvider(t *testing.T, p *fakeChildNumberProvider) {
	t.Helper()
	if p.name == "" {
		p.name = workmgmt.Default().Provider
	}
	workmgmt.Register(p)
}

// TestFileWorkItem_ChildNumberDiscovered is the cross-boundary done-means seam
// (handler -> EpicChildrenQuerier -> NextChildNumber -> rendered title): a
// feature filing with parent_epic and no title_vars.n discovers the epic's
// existing children server-side and files the next number, [E7.3]. It also
// asserts EpicChildren was called with the right epic ref + target — the source
// swap guard from the plan's risk note.
func TestFileWorkItem_ChildNumberDiscovered(t *testing.T) {
	fp := &fakeChildNumberProvider{children: []workmgmt.EpicChild{
		{Number: 501, Title: "[E7.1] first child"},
		{Number: 502, Title: "[E7.2] second child"},
	}}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Discover my number",
		TitleVars: map[string]string{"epic": "7"}, // n omitted -> discovered
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.epicCalls != 1 {
		t.Fatalf("EpicChildren called %d times, want 1", fp.epicCalls)
	}
	if fp.epicReq.Epic != "#389" {
		t.Errorf("EpicChildren req.Epic = %q, want #389", fp.epicReq.Epic)
	}
	if fp.epicReq.Target.Repo.Owner != "kuhlman-labs" || fp.epicReq.Target.Repo.Name != "fishhawk" {
		t.Errorf("EpicChildren req.Target.Repo = %+v, want kuhlman-labs/fishhawk", fp.epicReq.Target.Repo)
	}
	if fp.lastFileReq.Item.Title != "[E7.3] Discover my number" {
		t.Errorf("filed title = %q, want [E7.3] Discover my number (max(1,2)+1)", fp.lastFileReq.Item.Title)
	}
	if resp := decodeWorkItem(t, rec); resp.Title != "[E7.3] Discover my number" {
		t.Errorf("response title = %q, want [E7.3] Discover my number", resp.Title)
	}
}

// TestFileWorkItem_ChildNumberExplicitOverride asserts a caller-supplied n
// short-circuits {n} discovery — NextChildNumber is NOT consulted and the
// caller's value is rendered verbatim (the override contract, mirroring
// existing_numbers). Since #3714 the single EpicChildren call on this path is
// the parent-epic CAPACITY probe, not discovery.
func TestFileWorkItem_ChildNumberExplicitOverride(t *testing.T) {
	fp := &fakeChildNumberProvider{children: []workmgmt.EpicChild{{Title: "[E7.9] would yield 10"}}}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Caller knows the number",
		TitleVars: map[string]string{"epic": "7", "n": "3"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	// ONE EpicChildren call, and it is the #3714 parent-epic CAPACITY probe, not
	// {n} discovery: the fixture's lone child is [E7.9], so a derivation that had
	// NOT been short-circuited would render [E7.10]. The title assertion below is
	// what proves the override; the count only pins that the guard probes exactly
	// once (the cap governs the sub-issue LINK, so an explicit n cannot bypass it).
	if fp.epicCalls != 1 {
		t.Errorf("EpicChildren called %d times, want 1 (the capacity probe only)", fp.epicCalls)
	}
	if fp.lastFileReq.Item.Title != "[E7.3] Caller knows the number" {
		t.Errorf("filed title = %q, want the caller's n rendered verbatim", fp.lastFileReq.Item.Title)
	}
}

// TestFileWorkItem_ChildNumberDiscoveryErrorFailsClosed asserts a genuine
// EpicChildren error fails the filing closed with 422 work_item_invalid
// carrying details.n_discovery_failed, and NO issue is created (File is never
// dispatched — no orphan issue).
func TestFileWorkItem_ChildNumberDiscoveryErrorFailsClosed(t *testing.T) {
	fp := &fakeChildNumberProvider{epicErr: errors.New("sub-issues API exploded")}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Discovery will fail",
		TitleVars: map[string]string{"epic": "7"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	if got, _ := env.Error.Details["n_discovery_failed"].(string); got == "" || !strings.Contains(got, "sub-issues API exploded") {
		t.Errorf("details.n_discovery_failed = %v, want it to carry the cause", env.Error.Details["n_discovery_failed"])
	}
	if fp.fileCalls != 0 {
		t.Error("provider File dispatched despite a fail-closed discovery error")
	}
}

// TestFileWorkItem_ChildNumberProviderWithoutCapability asserts a provider that
// does NOT implement EpicChildrenQuerier + an omitted n falls through to
// Apply's renderTitle missing-placeholder 422 unchanged (byte-compatible with
// the pre-#1958 contract), listing 'n' in missing_placeholders. Discovery never
// ran, so the 422 is NOT enriched with n_discovery_failed.
func TestFileWorkItem_ChildNumberProviderWithoutCapability(t *testing.T) {
	fp := &fakeWorkProvider{} // File-only, no EpicChildrenQuerier capability
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "No discovery capability",
		TitleVars: map[string]string{"epic": "7"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	missing, _ := env.Error.Details["missing_placeholders"].([]any)
	sawN := false
	for _, m := range missing {
		if str, _ := m.(string); str == "n" {
			sawN = true
		}
	}
	if !sawN {
		t.Errorf("missing_placeholders = %v, want it to list 'n'", env.Error.Details["missing_placeholders"])
	}
	if _, present := env.Error.Details["n_discovery_failed"]; present {
		t.Errorf("details must NOT carry n_discovery_failed (no discovery ran): %v", env.Error.Details)
	}
	if fp.called {
		t.Error("provider File dispatched despite an unresolved {n}")
	}
}

// TestFileWorkItem_ChildNumberFirstChild asserts the first-child path: an epic
// with no matching children yields [E7.1], NOT a crash nor a wrong number.
func TestFileWorkItem_ChildNumberFirstChild(t *testing.T) {
	fp := &fakeChildNumberProvider{children: nil}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "The first child",
		TitleVars: map[string]string{"epic": "7"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.lastFileReq.Item.Title != "[E7.1] The first child" {
		t.Errorf("filed title = %q, want [E7.1] (first child of the epic)", fp.lastFileReq.Item.Title)
	}
}

// TestFileWorkItem_ChildNumberZeroMatchFailsClosed is the #2101 fix: an epic
// whose children are NON-EMPTY but carry only unmatched [E22.X]-style
// placeholder titles (the #389 corpus shape) must fail the omitted-n filing
// closed — 422 work_item_invalid with details.n_discovery_failed naming the
// epic, and NO issue created — instead of silently allocating a colliding
// [E22.1]. This asserts the SHIPPED behavior, not merely that the branch exists.
func TestFileWorkItem_ChildNumberZeroMatchFailsClosed(t *testing.T) {
	fp := &fakeChildNumberProvider{children: []workmgmt.EpicChild{
		{Number: 601, Title: "[E22.X] placeholder one"},
		{Number: 602, Title: "[E22.X] placeholder two"},
	}}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Zero-match epic",
		TitleVars: map[string]string{"epic": "22"}, // n omitted -> discovery -> zero match
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	got, _ := env.Error.Details["n_discovery_failed"].(string)
	if got == "" || !strings.Contains(got, "#389") {
		t.Errorf("details.n_discovery_failed = %v, want it present and naming the epic #389", env.Error.Details["n_discovery_failed"])
	}
	if fp.fileCalls != 0 {
		t.Error("provider File dispatched despite a fail-closed zero-match discovery")
	}
}

// TestFileWorkItem_ChildNumberZeroMatchExplicitOverride pins acceptance
// criterion 3 against the EXACT zero-match scenario the #2101 fix names: an
// explicit title_vars.n supplied against an epic whose children are non-empty
// but carry only unmatched [E22.X] titles STILL succeeds (201, issue filed),
// because the explicit-n override short-circuits {n} DISCOVERY entirely —
// NextChildNumber never runs, so the #2101 fail-closed branch cannot fire. This
// proves the override path is unaffected by the fix. (Since #3714 ONE
// EpicChildren call still happens: the parent-epic capacity probe, which is
// inert here because the fake declares no ChildCap.)
func TestFileWorkItem_ChildNumberZeroMatchExplicitOverride(t *testing.T) {
	fp := &fakeChildNumberProvider{children: []workmgmt.EpicChild{
		{Number: 601, Title: "[E22.X] placeholder one"},
		{Number: 602, Title: "[E22.X] placeholder two"},
	}}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Caller knows the number",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	// The one EpicChildren call is the #3714 capacity probe, NOT discovery:
	// NextChildNumber never runs (the [E22.X] corpus would make it fail closed),
	// and the fake declares no ChildCap, so the guard is inert and the filing
	// still succeeds — the point this test exists to pin.
	if fp.epicCalls != 1 {
		t.Errorf("EpicChildren called %d times, want 1 (the capacity probe only; discovery is short-circuited)", fp.epicCalls)
	}
	if fp.fileCalls != 1 {
		t.Errorf("File called %d times, want 1 (the filing succeeds)", fp.fileCalls)
	}
	if fp.lastFileReq.Item.Title != "[E22.7] Caller knows the number" {
		t.Errorf("filed title = %q, want [E22.7] Caller knows the number (explicit n verbatim)", fp.lastFileReq.Item.Title)
	}
}

// TestFileWorkItem_ChildNumberConcurrentFilingsDistinct is binding condition (1):
// two parallel omitted-n filings against the SAME epic must serialize through
// the per-epic in-process lock and file DISTINCT consecutive numbers ([E7.3] and
// [E7.4]) — never a collision on [E7.3].
//
// The barrier (barrierN:2) makes the test load-bearing rather than probabilistic:
// EpicChildren blocks until BOTH filings arrive before either snapshots the
// children, so an implementation WITHOUT the per-epic lock — which can run both
// EpicChildren calls concurrently — deterministically has both read the same
// [E7.1],[E7.2] snapshot, both allocate [E7.3], and collide (the seen-set assert
// fails). The correct locked implementation can never have two goroutines inside
// EpicChildren at once, so the lone arrival trips barrierWait and the reads stay
// serialized — barrierWait (not a fixed sleep) is what lets the guarded lock pass
// without the barrier deadlocking on an arrival that can never come.
func TestFileWorkItem_ChildNumberConcurrentFilingsDistinct(t *testing.T) {
	fp := &fakeChildNumberProvider{
		children:     []workmgmt.EpicChild{{Title: "[E7.1] existing"}, {Title: "[E7.2] existing"}},
		appendOnFile: true,
		barrierN:     2,
		barrierWait:  2 * time.Second,
	}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := fileWorkItem(t, s, workItemRequest{
				Repo:      "kuhlman-labs/fishhawk",
				Type:      "feature",
				Summary:   "Concurrent",
				TitleVars: map[string]string{"epic": "7"},
				Relations: &workItemRelations{ParentEpic: "#389"},
			}, "github:operator")
			if rec.Code != http.StatusCreated {
				t.Errorf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()

	fp.mu.Lock()
	titles := append([]string(nil), fp.filedTitles...)
	fp.mu.Unlock()
	if len(titles) != 2 {
		t.Fatalf("filed %d titles, want 2: %v", len(titles), titles)
	}
	seen := map[string]bool{}
	for _, tl := range titles {
		seen[tl] = true
	}
	if !seen["[E7.3] Concurrent"] || !seen["[E7.4] Concurrent"] {
		t.Errorf("filed titles = %v, want the distinct consecutive [E7.3] and [E7.4]", titles)
	}
}

// TestFileWorkItem_NoIntakeObjectWhenHookProducesNothing pins the
// backward-compatibility half of the #2239 response change: the `intake` key
// is omitempty, so a deployment whose hook produced no Signals at all returns
// the pre-#2239 payload verbatim.
//
// It is the response-side twin of
// TestIntakeHook_DegradedFilingBodyIsUnchanged (which pins the BODY side). The
// two together bound this change's blast radius: a degraded hook is invisible
// to both the created issue and any existing response consumer.
//
// The bad state is seeded BY CONSTRUCTION — a nil signals value handed to the
// response struct — rather than by driving a degradation, so the assertion
// lands on the serialization rule itself.
func TestFileWorkItem_NoIntakeObjectWhenHookProducesNothing(t *testing.T) {
	raw, err := json.Marshal(workItemResponse{
		Type:     "chore",
		Title:    "[E22.7] Tidy the workspace file",
		Number:   4242,
		URL:      "https://example.test/4242",
		Provider: workmgmt.Default().Provider,
		Intake:   nil,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "\"intake\"") {
		t.Errorf("a nil intake serialized an intake key, breaking the pre-#2239 payload shape: %s", raw)
	}
}

// TestFileWorkItem_GitLab_ChildNumberAllocatedViaEpicChildren pins the
// newly-reachable consequence of #3658: the REAL gitlab provider now implements
// workmgmt.EpicChildrenQuerier, so a gitlab filing with a parent_epic and an
// [EX.n] title format with n omitted discovers the epic's existing children
// (the epic issue's relates_to links) and allocates the next number. Before
// #3658 the provider was File-only and could not allocate at all. #102 carries a
// Parent epic marker naming THIS epic (admitted); #103 names another epic and is
// excluded, so its [E7.9] must NOT drive the allocation to 10.
func TestFileWorkItem_GitLab_ChildNumberAllocatedViaEpicChildren(t *testing.T) {
	api := newFakeGitLabAPI(55)
	api.addIssue(100, "[E7] the epic", "", nil, []int{101, 102, 103})
	api.addIssue(101, "[E7.1] first child", "", nil, []int{100})
	api.addIssue(102, "[E7.2] second child", "Parent epic: #100", nil, []int{100})
	api.addIssue(103, "[E7.9] a foreign epic's child", "Parent epic: #400", nil, []int{100})
	registerRealGitLabProvider(t, api)
	s := New(Config{}) // no GitHub client: a gitlab filing needs none

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "group/app",
		Type:      "feature",
		Summary:   "Discover my number on gitlab",
		TitleVars: map[string]string{"epic": "7"}, // n omitted -> discovered
		Relations: &workItemRelations{ParentEpic: "#100"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if resp := decodeWorkItem(t, rec); resp.Title != "[E7.3] Discover my number on gitlab" {
		t.Errorf("response title = %q, want [E7.3] Discover my number on gitlab (max(1,2)+1 over the relates_to children)", resp.Title)
	}
}

// --- sequential-number allocation lock (#3704) ---

// fakeSequentialNumberProvider is a workmgmt.Provider that ALSO implements the
// optional workmgmt.NumberDiscoverer capability (#1269), like
// fakeDiscoverProvider, but is concurrency-safe so the two-parallel-filings
// test can share one instance (the AGENTS.md #3226 trap: a racy fake makes a
// -race counterfactual attributable to the FAKE rather than to the control).
// Every field is guarded by mu. When appendOnFile is set, File records the
// just-allocated number so a subsequent DiscoverNumbers reflects it — the
// mechanism that lets serialized filings allocate distinct numbers.
//
// fakeDiscoverProvider is deliberately NOT retrofitted: it is only ever driven
// serially by the existing single-filing tests, which stay untouched.
type fakeSequentialNumberProvider struct {
	name string

	mu             sync.Mutex
	discovered     []int
	discoverErr    error
	discoverCalls  int
	discoverReq    workmgmt.DiscoverNumbersRequest
	fileCalls      int
	filedTitles    []string
	filedNumbers   []int
	appendOnFile   bool
	createdCounter int

	// DiscoverNumbers arrival barrier (#1958's pattern, binding condition 1).
	// When barrierN > 0 each DiscoverNumbers call SNAPSHOTS the number set
	// first, THEN reports arrival and blocks until barrierN calls have arrived
	// OR barrierWait elapses, and only then returns that pre-barrier snapshot.
	// Snapshotting BEFORE the wait is what makes the counterfactual
	// deterministic: with the production lock removed both goroutines are past
	// the snapshot (each holding the same pre-filing set) before either is
	// released, so both allocate the same number and collide. The release is a
	// BOUNDED TIMEOUT (barrierWait, derived via timescale.D) rather than a
	// strict rendezvous, which is what lets the correctly-LOCKED implementation
	// — where two goroutines can never be inside DiscoverNumbers at once — pass
	// instead of deadlocking on an arrival that can never come.
	barrierN    int
	barrierWait time.Duration
	barrierArr  int
	barrierCh   chan struct{}
}

// awaitBarrier blocks until barrierN DiscoverNumbers calls have arrived (all
// released together) or barrierWait elapses (a lone arrival proceeds). It is a
// no-op when barrierN <= 0. The channel wait happens WITHOUT f.mu held so a
// blocked caller never wedges the fake's other methods.
func (f *fakeSequentialNumberProvider) awaitBarrier() {
	f.mu.Lock()
	if f.barrierN <= 0 {
		f.mu.Unlock()
		return
	}
	if f.barrierCh == nil {
		f.barrierCh = make(chan struct{})
	}
	ch := f.barrierCh
	wait := f.barrierWait
	f.barrierArr++
	if f.barrierArr >= f.barrierN {
		select {
		case <-ch: // already released
		default:
			close(ch)
		}
	}
	f.mu.Unlock()

	select {
	case <-ch:
	case <-time.After(wait):
	}
}

func (f *fakeSequentialNumberProvider) Name() string { return f.name }

func (f *fakeSequentialNumberProvider) DiscoverNumbers(_ context.Context, req workmgmt.DiscoverNumbersRequest) ([]int, error) {
	// Snapshot FIRST (binding condition 1), then wait on the arrival barrier,
	// then return that pre-barrier snapshot.
	f.mu.Lock()
	f.discoverCalls++
	f.discoverReq = req
	err := f.discoverErr
	snapshot := append([]int(nil), f.discovered...)
	f.mu.Unlock()

	f.awaitBarrier()

	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (f *fakeSequentialNumberProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileCalls++
	f.filedTitles = append(f.filedTitles, req.Item.Title)
	f.filedNumbers = append(f.filedNumbers, req.Number)
	if f.appendOnFile {
		f.discovered = append(f.discovered, req.Number)
	}
	f.createdCounter++
	return &workmgmt.CreatedItem{
		Provider:      f.name,
		Number:        7000 + f.createdCounter,
		URL:           "https://github.com/kuhlman-labs/fishhawk/issues/7000",
		AppliedLabels: req.Item.Classification.Labels,
		Boarded:       true,
	}, nil
}

// registerFakeSequentialNumberProvider registers the concurrency-safe
// discovery-capable fake under the default provider id.
func registerFakeSequentialNumberProvider(t *testing.T, p *fakeSequentialNumberProvider) {
	t.Helper()
	if p.name == "" {
		p.name = workmgmt.Default().Provider
	}
	workmgmt.Register(p)
}

// sequentialTestTarget is the target the handler builds for a
// "kuhlman-labs/fishhawk" filing, as far as sequentialNumberLockKey reads it
// (owner + name only), so a test can compute the very key the handler will use.
func sequentialTestTarget() workmgmt.Target {
	return workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}}
}

// TestFileWorkItem_SequentialNumberConcurrentFilingsDistinct is the done-means
// behavioural test for #3704: two parallel `adr` filings with existing_numbers
// omitted must serialize through the per-(repo, prefix) in-process lock and
// allocate DISTINCT consecutive numbers ([ADR-080] and [ADR-081]) — never a
// collision on [ADR-080], which is the duplicate [E78] observed on #3699/#3700.
//
// The barrier makes the test load-bearing rather than probabilistic. Each
// DiscoverNumbers snapshots the number set BEFORE waiting, so WITHOUT the lock
// both goroutines deterministically hold the same pre-filing {79} snapshot
// before either reaches File, both allocate 80, and the seen-set assertion
// fails. WITH the lock only one goroutine can be inside DiscoverNumbers at a
// time: the lone arrival trips the bounded barrierWait, files 80, File appends
// it, and the second call's snapshot is {79,80} -> 81.
func TestFileWorkItem_SequentialNumberConcurrentFilingsDistinct(t *testing.T) {
	fp := &fakeSequentialNumberProvider{
		discovered:   []int{79},
		appendOnFile: true,
		barrierN:     2,
		barrierWait:  timescale.D(2 * time.Second),
	}
	registerFakeSequentialNumberProvider(t, fp)
	s := New(Config{})

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := fileWorkItem(t, s, workItemRequest{
				Repo:    "kuhlman-labs/fishhawk",
				Type:    "adr",
				Summary: "Concurrent decision",
				// existing_numbers omitted on purpose — discovery fills it.
			}, "github:operator")
			if rec.Code != http.StatusCreated {
				t.Errorf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()

	fp.mu.Lock()
	titles := append([]string(nil), fp.filedTitles...)
	fp.mu.Unlock()
	if len(titles) != 2 {
		t.Fatalf("filed %d titles, want 2: %v", len(titles), titles)
	}
	seen := map[string]bool{}
	for _, tl := range titles {
		seen[tl] = true
	}
	if !seen["[ADR-080] Concurrent decision"] || !seen["[ADR-081] Concurrent decision"] {
		t.Errorf("filed titles = %v, want the distinct consecutive [ADR-080] and [ADR-081]", titles)
	}
}

// fileWorkItemWithin runs one filing on its own goroutine and fails the test if
// it has not returned within bound — the shape every held-lock case below uses
// to turn "the handler contended on a lock it should not have taken" into a
// bounded failure instead of a hang.
func fileWorkItemWithin(t *testing.T, s *Server, body workItemRequest, bound time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	type result struct{ rec *httptest.ResponseRecorder }
	done := make(chan result, 1)
	go func() { done <- result{fileWorkItem(t, s, body, "github:operator")} }()
	select {
	case r := <-done:
		return r.rec
	case <-time.After(bound):
		t.Fatalf("filing did not complete within %s — it contended on the held sequential-number lock", bound)
		return nil
	}
}

// TestFileWorkItem_SequentialExistingNumbersSkipsLock pins the
// explicit-override carve-out BEHAVIOURALLY: the test itself holds the filing's
// (repo, prefix) lock, and a caller-supplied existing_numbers filing must still
// complete — the lock is taken ONLY when discovery actually runs.
func TestFileWorkItem_SequentialExistingNumbersSkipsLock(t *testing.T) {
	fp := &fakeSequentialNumberProvider{discovered: []int{500}} // would yield 501 if discovery ran
	registerFakeSequentialNumberProvider(t, fp)
	s := New(Config{})

	unlock := lockSequentialNumberKey(sequentialNumberLockKey(sequentialTestTarget(), "ADR-"))
	defer unlock()

	rec := fileWorkItemWithin(t, s, workItemRequest{
		Repo:            "kuhlman-labs/fishhawk",
		Type:            "adr",
		Summary:         "Caller knows best",
		ExistingNumbers: []int{34, 35},
	}, timescale.D(2*time.Second))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	fp.mu.Lock()
	calls := fp.discoverCalls
	numbers := append([]int(nil), fp.filedNumbers...)
	fp.mu.Unlock()
	if calls != 0 {
		t.Errorf("discovery ran %d times despite a caller-supplied existing_numbers override", calls)
	}
	if len(numbers) != 1 || numbers[0] != 36 {
		t.Errorf("filed numbers = %v, want [36] (caller list 34,35 -> 36)", numbers)
	}
}

// TestFileWorkItem_SequentialNoDiscovererSkipsLock is the no-capability skip
// mode: with the (repo, prefix) lock held by the test, a File-only provider
// must still reach the pre-existing #1265 fail-closed 422 without contending.
func TestFileWorkItem_SequentialNoDiscovererSkipsLock(t *testing.T) {
	fp := &fakeWorkProvider{name: workmgmt.Default().Provider}
	workmgmt.Register(fp)
	s := New(Config{})

	unlock := lockSequentialNumberKey(sequentialNumberLockKey(sequentialTestTarget(), "ADR-"))
	defer unlock()

	rec := fileWorkItemWithin(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "No discoverer here",
	}, timescale.D(2*time.Second))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Details["existing_numbers_required"] != true {
		t.Errorf("details.existing_numbers_required = %v, want true (the unchanged #1265 fail-closed)", env.Error.Details["existing_numbers_required"])
	}
}

// TestFileWorkItem_SequentialDiscoveryErrorReleasesLock is the fail-closed
// mode: a DiscoverNumbers error still returns the unchanged 422
// work_item_invalid with details.discovery_failed, AND a second filing against
// the same key then completes — proving the error branch released the lock
// rather than wedging every later filing of that (repo, prefix).
func TestFileWorkItem_SequentialDiscoveryErrorReleasesLock(t *testing.T) {
	fp := &fakeSequentialNumberProvider{discoverErr: errors.New("search API exploded")}
	registerFakeSequentialNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItemWithin(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "Discovery will fail",
	}, timescale.D(2*time.Second))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	if got, _ := env.Error.Details["discovery_failed"].(string); got == "" || !strings.Contains(got, "search API exploded") {
		t.Errorf("details.discovery_failed = %v, want it to carry the cause", env.Error.Details["discovery_failed"])
	}

	// The lock must be free again: a healthy second filing against the SAME
	// (repo, prefix) key completes within the bound.
	fp.mu.Lock()
	fp.discoverErr = nil
	fp.discovered = []int{79}
	fp.mu.Unlock()

	rec2 := fileWorkItemWithin(t, s, workItemRequest{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "adr",
		Summary: "Discovery recovered",
	}, timescale.D(2*time.Second))
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second filing status = %d, want 201 — the discovery-error branch wedged the lock (body=%s)", rec2.Code, rec2.Body.String())
	}
}

// TestKeyedLocks_SameKeySerializes pins the shared helper's core contract: a
// second acquisition of the SAME key blocks until the first unlocks.
func TestKeyedLocks_SameKeySerializes(t *testing.T) {
	k := &keyedLocks{}
	unlock := k.lock("repo#ADR-")

	acquired := make(chan struct{})
	go func() {
		u := k.lock("repo#ADR-")
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
		t.Fatal("a second acquisition of the same key did not block while the first was held")
	case <-time.After(timescale.D(50 * time.Millisecond)):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(timescale.D(2 * time.Second)):
		t.Fatal("the second acquisition never completed after the first unlocked")
	}
}

// TestKeyedLocks_DistinctKeysDoNotContend pins that the helper keys its
// mutexes: two DIFFERENT keys are holdable simultaneously. A helper that
// ignored its key and serialized on one global mutex would leave the second
// acquisition blocked until the bound elapsed.
func TestKeyedLocks_DistinctKeysDoNotContend(t *testing.T) {
	k := &keyedLocks{}
	unlockA := k.lock("repo#ADR-")
	defer unlockA()

	acquired := make(chan struct{})
	go func() {
		u := k.lock("repo#E")
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
	case <-time.After(timescale.D(2 * time.Second)):
		t.Fatal("acquiring a DISTINCT key blocked while another key was held — the helper is serializing on one global mutex")
	}
}

// --- parent-epic child-cap refusal (#3714) ---

// numberedChildCorpus builds n well-formed numbered child titles
// [E<epic>.1..E<epic>.n]. Fixture discipline matters here (the plan's
// anti-MASKING note): the titles MUST be well-formed so NextChildNumber
// SUCCEEDS, otherwise the pre-existing #2101 zero-match guard would refuse the
// filing on its own and MASK the new cap control — the at-cap test would stay
// green with the cap check deleted.
func numberedChildCorpus(epic string, n int) []workmgmt.EpicChild {
	out := make([]workmgmt.EpicChild, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, workmgmt.EpicChild{
			Number: 500 + i,
			Title:  fmt.Sprintf("[E%s.%d] child %d", epic, i, i),
		})
	}
	return out
}

// capRefusal decodes the shared at-cap 422 and asserts every detail key the
// refusal contract names, so both refusal sites are checked against ONE
// expectation (they are built by one constructor, parentEpicFullError).
func assertParentEpicFullRefusal(t *testing.T, rec *httptest.ResponseRecorder, wantCount, wantCap int) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", env.Error.Code)
	}
	if full, _ := env.Error.Details["parent_epic_full"].(bool); !full {
		t.Errorf("details.parent_epic_full = %v, want true: %v", env.Error.Details["parent_epic_full"], env.Error.Details)
	}
	if got, _ := env.Error.Details["parent_epic"].(string); got != "#389" {
		t.Errorf("details.parent_epic = %v, want #389", env.Error.Details["parent_epic"])
	}
	if got, _ := env.Error.Details["parent_epic_child_count"].(float64); int(got) != wantCount {
		t.Errorf("details.parent_epic_child_count = %v, want %d", env.Error.Details["parent_epic_child_count"], wantCount)
	}
	if got, _ := env.Error.Details["parent_epic_child_cap"].(float64); int(got) != wantCap {
		t.Errorf("details.parent_epic_child_cap = %v, want %d", env.Error.Details["parent_epic_child_cap"], wantCap)
	}
	// The remedy must be named, and the non-bypass stated: "pass n explicitly"
	// is the escape hatch every OTHER 422 on this path offers, and it does NOT
	// work here.
	if !strings.Contains(env.Error.Message, "successor catch-all epic") ||
		!strings.Contains(env.Error.Message, "does not bypass the link cap") {
		t.Errorf("message does not name the remedy and the non-bypass: %q", env.Error.Message)
	}
}

// TestFileWorkItem_ParentEpicAtCapRefusedOnDerivedPath is the #3714 done-means
// on the DERIVED-{n} path: a parent epic already carrying the provider's hard
// cap of children refuses the filing 422 with details.parent_epic_full BEFORE
// anything is created, instead of freezing {n} at a number whose sub-issue link
// GitHub rejects and rendering the same [E<epic>.<n>] title on every later
// filing (E68 #2885 produced seven [E68.67] issues).
func TestFileWorkItem_ParentEpicAtCapRefusedOnDerivedPath(t *testing.T) {
	fp := &fakeChildNumberProvider{children: numberedChildCorpus("68", 100), childCap: 100}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Would freeze at 101",
		TitleVars: map[string]string{"epic": "68"}, // n omitted -> derivation path
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	assertParentEpicFullRefusal(t, rec, 100, 100)
	if fp.fileCalls != 0 {
		t.Errorf("provider File dispatched %d times; the refusal must precede Apply and File so NOTHING is created", fp.fileCalls)
	}
}

// TestFileWorkItem_ParentEpicBelowCapStillFiles is the BOUNDARY below the cap:
// 99 children of a 100-cap epic is NOT full, so the filing succeeds and the
// derived {n} is exactly 100 — the last allocatable number. This is the arm
// that discriminates `>= cap` from `> cap`.
func TestFileWorkItem_ParentEpicBelowCapStillFiles(t *testing.T) {
	fp := &fakeChildNumberProvider{children: numberedChildCorpus("68", 99), childCap: 100}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "The last allocatable child",
		TitleVars: map[string]string{"epic": "68"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (99 of 100 is not full) (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.lastFileReq.Item.Title != "[E68.100] The last allocatable child" {
		t.Errorf("filed title = %q, want [E68.100] (max(1..99)+1)", fp.lastFileReq.Item.Title)
	}
}

// TestFileWorkItem_ParentEpicAtCapRefusedOnExplicitNPath pins the cap on the
// NON-derivation path: an explicit title_vars.n short-circuits {n} discovery,
// but the cap governs the sub-issue LINK, not the number, so the filing is
// refused identically. Without this the filing would land an UNLINKED orphan
// (succeed-with-epic_link_error) — the defect's worse half.
func TestFileWorkItem_ParentEpicAtCapRefusedOnExplicitNPath(t *testing.T) {
	fp := &fakeChildNumberProvider{children: numberedChildCorpus("68", 100), childCap: 100}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Explicit n does not bypass the link cap",
		TitleVars: map[string]string{"epic": "68", "n": "150"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	assertParentEpicFullRefusal(t, rec, 100, 100)
	if fp.fileCalls != 0 {
		t.Errorf("provider File dispatched %d times; an explicit n must not bypass the cap", fp.fileCalls)
	}
}

// capConventions installs conventions carrying a type whose title_format has NO
// {n} placeholder but whose epic_link is optional, so a filing of it can carry
// a parent_epic and reach the non-derivation capacity guard. The shipped default
// conventions have no such type (adr/epic both carry epic_link: none), so this
// branch is only reachable through a deployment override (ADR-058 #1856).
func capConventions(t *testing.T) {
	t.Helper()
	conv := workmgmt.Default()
	flat := conv.Types["chore"]
	flat.TitleFormat = "{summary}" // no {n}: derivation short-circuits
	flat.EpicLink = "optional"
	types := make(map[string]workmgmt.ItemType, len(conv.Types)+1)
	for k, v := range conv.Types {
		types[k] = v
	}
	types["flatchore"] = flat
	conv.Types = types
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })
}

// TestFileWorkItem_ParentEpicAtCapRefusedForTypeWithoutChildNumber pins the
// widest arm: a type whose title_format carries NO {n} at all, filed against a
// full parent epic, is refused identically. The cap is a property of the LINK,
// so a filing that never needed a child number is still unlinkable.
func TestFileWorkItem_ParentEpicAtCapRefusedForTypeWithoutChildNumber(t *testing.T) {
	capConventions(t)
	fp := &fakeChildNumberProvider{children: numberedChildCorpus("68", 100), childCap: 100}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "flatchore",
		Summary:   "No child number at all",
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	assertParentEpicFullRefusal(t, rec, 100, 100)
	if fp.fileCalls != 0 {
		t.Errorf("provider File dispatched %d times; the cap governs the LINK, not the number", fp.fileCalls)
	}
}

// TestFileWorkItem_ParentEpicNoDeclaredCapIsInert is the provider-declares-no-cap
// arm: ChildCap 0 with 150 children still files. The guard must be inert for a
// provider whose Children set is not the structurally-capped set the link writes
// into — the stated gitlab residual.
func TestFileWorkItem_ParentEpicNoDeclaredCapIsInert(t *testing.T) {
	fp := &fakeChildNumberProvider{children: numberedChildCorpus("68", 150), childCap: 0}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "No cap declared",
		TitleVars: map[string]string{"epic": "68"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (ChildCap 0 declares no cap) (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.lastFileReq.Item.Title != "[E68.151] No cap declared" {
		t.Errorf("filed title = %q, want [E68.151] (allocation unaffected)", fp.lastFileReq.Item.Title)
	}
}

// TestFileWorkItem_ParentEpicCapProbeErrorFailsOpen pins the deliberate FAIL
// OPEN on the non-derivation path: an EpicChildren probe error lets the filing
// through exactly as it did before #3714, because "pass n explicitly" is the
// documented escape hatch from that very error and failing closed here would
// make the remedy unreachable. The residual — a probe error against a
// genuinely-full parent still files an unlinked issue — is real and stated.
//
// Note the CONTRAST this arm draws with
// TestFileWorkItem_ChildNumberDiscoveryErrorFailsClosed: the SAME probe error on
// the DERIVED-{n} path fails CLOSED, because there the error also means no
// number can be allocated.
func TestFileWorkItem_ParentEpicCapProbeErrorFailsOpen(t *testing.T) {
	fp := &fakeChildNumberProvider{epicErr: errors.New("sub-issues API exploded"), childCap: 100}
	registerFakeChildNumberProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "feature",
		Summary:   "Probe error must not wedge the escape hatch",
		TitleVars: map[string]string{"epic": "68", "n": "7"},
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (the capacity guard FAILS OPEN on a probe error) (body=%s)", rec.Code, rec.Body.String())
	}
	if fp.fileCalls != 1 {
		t.Errorf("File called %d times, want 1 (the escape hatch is preserved)", fp.fileCalls)
	}
	if fp.lastFileReq.Item.Title != "[E68.7] Probe error must not wedge the escape hatch" {
		t.Errorf("filed title = %q, want the explicit n rendered verbatim", fp.lastFileReq.Item.Title)
	}
}

// TestFileWorkItem_ParentEpicCapInertWithoutQuerierCapability pins the
// capability short-circuit: a provider that does not implement
// EpicChildrenQuerier cannot be probed, so a parent_epic filing of a type
// needing no {n} is unchanged by #3714.
func TestFileWorkItem_ParentEpicCapInertWithoutQuerierCapability(t *testing.T) {
	capConventions(t)
	fp := &fakeWorkProvider{} // File-only, no EpicChildrenQuerier capability
	registerFakeProvider(t, fp)
	s := New(Config{})

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "flatchore",
		Summary:   "No querier capability",
		Relations: &workItemRelations{ParentEpic: "#389"},
	}, "github:operator")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (no capability to probe) (body=%s)", rec.Code, rec.Body.String())
	}
	if !fp.called {
		t.Error("provider File was not dispatched; the guard must be inert without the capability")
	}
}

// TestParentEpicAtChildCap is the pure boundary unit for the predicate BOTH
// refusal sites share. It pins the `>= cap` semantics (at the cap the parent is
// full: the NEXT link is the rejected one), the no-cap-declared inertness, and
// the nil-result guard.
func TestParentEpicAtChildCap(t *testing.T) {
	children := func(n int) []workmgmt.EpicChild { return make([]workmgmt.EpicChild, n) }
	for _, tc := range []struct {
		name      string
		res       *workmgmt.EpicChildrenResult
		wantFull  bool
		wantCount int
		wantCap   int
	}{
		{"nil result", nil, false, 0, 0},
		{"no cap declared, many children", &workmgmt.EpicChildrenResult{Children: children(150)}, false, 150, 0},
		{"one below the cap", &workmgmt.EpicChildrenResult{Children: children(99), ChildCap: 100}, false, 99, 100},
		{"exactly at the cap", &workmgmt.EpicChildrenResult{Children: children(100), ChildCap: 100}, true, 100, 100},
		{"over the cap", &workmgmt.EpicChildrenResult{Children: children(101), ChildCap: 100}, true, 101, 100},
		{"empty epic with a cap", &workmgmt.EpicChildrenResult{ChildCap: 100}, false, 0, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full, count, childCap := parentEpicAtChildCap(tc.res)
			if full != tc.wantFull || count != tc.wantCount || childCap != tc.wantCap {
				t.Errorf("parentEpicAtChildCap = (%v, %d, %d), want (%v, %d, %d)",
					full, count, childCap, tc.wantFull, tc.wantCount, tc.wantCap)
			}
		})
	}
}
