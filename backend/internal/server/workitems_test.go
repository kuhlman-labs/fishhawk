package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
	// failIfNoInstallation mirrors the real github.Provider fail-closed
	// (#1092's installation_unavailable guard): File errors when the
	// resolved Target.InstallationID is still 0.
	failIfNoInstallation bool
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
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Borrow another run's installation",
		RunID:   runID.String(),
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
				Repo:    "kuhlman-labs/fishhawk",
				Type:    "chore",
				Summary: "Inject an entry onto someone else's run",
				RunID:   runID.String(),
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
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Operator follow-up filing",
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
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "No installation on this repo",
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
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Installation lookup is transiently unavailable",
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
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "chore",
		Summary: "Sneak through the run-absent door",
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
