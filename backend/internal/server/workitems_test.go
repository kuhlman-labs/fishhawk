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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
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
	// resolved Target.InstallationID is still 0.
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
	if f.failIfNoInstallation && req.Target.InstallationID == 0 {
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
// missing provider rather than panicking. jira is now a real provider, so
// this uses a genuinely-never-registered placeholder ("gitlab") — the
// registry is process-global, and the end-to-end jira test below registers
// the jira provider, so a stale "jira" id here would resolve.
func TestFileWorkItem_UnimplementedProvider_FailsClosed(t *testing.T) {
	conv := workmgmt.Default()
	conv.Provider = "gitlab" // never registered
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return conv, nil }
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
	if env.Error.Details["provider"] != "gitlab" {
		t.Errorf("details.provider = %v, want gitlab", env.Error.Details["provider"])
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
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return conv, nil }
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
// issue exists) surfaces as 502 work_item_filing_failed with the provider
// cause in details.error.
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
	// The provider cause is surfaced in details.error (the apiError.Details
	// precedent the MCP tool reads to render it).
	if got, _ := env.Error.Details["error"].(string); !strings.Contains(got, "github said no") {
		t.Errorf("details.error should carry the provider cause, got %v", env.Error.Details["error"])
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
// Target.InstallationID. It drives a real POST through handleFileWorkItem
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
	if fp.captured.Target.InstallationID != wantInst {
		t.Errorf("provider Target.InstallationID = %d, want %d (resolved from the stub installation endpoint)",
			fp.captured.Target.InstallationID, wantInst)
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
	if fp.captured.Target.InstallationID != 0 {
		t.Errorf("Target.InstallationID = %d, want 0 (left unresolved on ErrNotInstalled)", fp.captured.Target.InstallationID)
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
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return conv, nil }
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
