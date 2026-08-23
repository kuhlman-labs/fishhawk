package server

// Tests for the intake-groom hook (#2239 / E54.7).
//
// The organising claim under test is NOT "the hook produces good signals" —
// that is the pure package's job (backend/internal/intakegroom). It is that
// the hook CANNOT HURT THE FILING: every enumerated failure mode returns HTTP
// 201, still creates the issue, and reports a typed degrade reason. There is
// one test per branch, and each asserts observable behaviour rather than an
// internal state.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const igCharterPath = "docs/charter.md"
const igCommit = "0123456789abcdef0123456789abcdef01234567"
const igBranch = "main"

// igCharterDoc is a charter carrying the shipped §4 rubric table shape. It
// declares S2, S4 and U4 so every structural rule has a citable id.
const igCharterDoc = `# Charter

## 4. Rubric

| id | line |
| --- | --- |
| **V1** | Does it move the current phase's goal? |
| **S2** | Is the item distinct from what is already tracked? |
| **S4** | Does the item carry the structure a reviewer needs? |
| **U4** | Does anything depend on this item landing? |
`

// igCharterNoRubric resolves fine but carries no parsable rubric rows — the
// bad state seeded BY CONSTRUCTION, not by calling the control under test.
const igCharterNoRubric = "# Charter\n\nProse only. No rubric table here.\n"

// igFetcher serves one charter document at any ref.
type igFetcher struct {
	content string
	missing bool
}

func (f *igFetcher) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, p, _ string) (*forge.FileContent, error) {
	if f.missing {
		return nil, forge.ErrNotFound
	}
	return &forge.FileContent{Path: p, Content: []byte(f.content), SHA: "blob"}, nil
}

type igCommits struct{}

func (igCommits) GetBranchSHA(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, _ string) (string, bool, error) {
	return igCommit, true, nil
}

// igCharterConfig returns the document seams wired against content.
func igCharterConfig(content string, missing bool) Config {
	return Config{
		DocumentResolver: &repodoc.Resolver{Fetcher: &igFetcher{content: content, missing: missing}, Commits: igCommits{}},
		DocumentBaseRef: func(context.Context, forge.RepoRef) (string, error) {
			return igBranch, nil
		},
	}
}

// igInstallCharterConventions points the process-wide conventions loader at a
// charter declaration.
func igInstallCharterConventions(t *testing.T) {
	t.Helper()
	conv := workmgmt.Default()
	conv.Charter = &workmgmt.Charter{Path: igCharterPath}
	installConventions(t, conv, nil)
}

// igReadProvider is a fakeWorkProvider that ALSO implements
// workmgmt.WorkItemReader, plus the never-dialed destructive seams the
// nothing-was-acted-on assertion reads after the call returns.
type igReadProvider struct {
	fakeWorkProvider

	mu sync.Mutex

	items     []workmgmt.WorkItemRecord
	truncated bool
	listErr   error
	// panicOnList makes ListWorkItems panic synchronously — the reachable
	// panic case (the hook starts no goroutine of its own).
	panicOnList bool
	// wedge, when > 0, makes ListWorkItems block until the CONTEXT is done or
	// wedge elapses, whichever comes first, then return ctx.Err(). It is
	// deliberately cancellation-COOPERATIVE: see the L1 note on
	// TestIntakeHook_WedgedReaderDegradesWithinBudget.
	wedge time.Duration

	gotRequest workmgmt.ListWorkItemsRequest
	listCalls  int

	// Never-dialed destructive seams. Nothing in the intake path may touch
	// them; the assertion READS them after the call returns, because a
	// control that fired and rolled back would return a byte-identical
	// response.
	transitionCalls   int
	closeCalls        int
	labelMutateCalls  int
	groomingApplyCall int
}

func (p *igReadProvider) ReadWorkItem(context.Context, workmgmt.ReadWorkItemRequest) (*workmgmt.WorkItemRecord, error) {
	return nil, errors.New("not used by the intake hook")
}

func (p *igReadProvider) ListWorkItems(ctx context.Context, req workmgmt.ListWorkItemsRequest) (*workmgmt.WorkItemPage, error) {
	p.mu.Lock()
	p.listCalls++
	p.gotRequest = req
	panicOnList, wedge, listErr := p.panicOnList, p.wedge, p.listErr
	items, truncated := p.items, p.truncated
	p.mu.Unlock()

	if panicOnList {
		panic("intake reader exploded")
	}
	if wedge > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wedge):
			// The wedge outlasted the hook's deadline. Reaching here means the
			// deadline never fired, which is exactly the counterfactual RED.
			return nil, errors.New("wedge elapsed without the hook's deadline firing")
		}
	}
	if listErr != nil {
		return nil, listErr
	}
	return &workmgmt.WorkItemPage{Items: items, Truncated: truncated}, nil
}

// Transition / Close / MutateLabels / ApplyGroomingMutation are the
// never-dialed seams. They are plain methods (not capability implementations)
// so this fake cannot accidentally satisfy a mutation interface — the point is
// that NOTHING calls them at all.
func (p *igReadProvider) Transition() { p.mu.Lock(); p.transitionCalls++; p.mu.Unlock() }
func (p *igReadProvider) Close()      { p.mu.Lock(); p.closeCalls++; p.mu.Unlock() }
func (p *igReadProvider) MutateLabels() {
	p.mu.Lock()
	p.labelMutateCalls++
	p.mu.Unlock()
}
func (p *igReadProvider) ApplyGrooming() {
	p.mu.Lock()
	p.groomingApplyCall++
	p.mu.Unlock()
}

// igRegisterReadProvider registers p under the default provider id.
func igRegisterReadProvider(t *testing.T, p *igReadProvider) {
	t.Helper()
	p.name = workmgmt.Default().Provider
	workmgmt.Register(p)
}

// igHealthyProvider is a reader whose window contains an obvious duplicate of
// the filing the tests below make, plus an epic candidate.
func igHealthyProvider(t *testing.T) *igReadProvider {
	t.Helper()
	p := &igReadProvider{
		items: []workmgmt.WorkItemRecord{
			{Number: 1234, Title: "[E22.4] Add the widget endpoint", URL: "https://example.test/1234", Labels: []string{"type:chore", "area:backend"}},
			{Number: 22, Title: "[E22] Widget platform", URL: "https://example.test/22", Labels: []string{"epic", "area:backend"}},
			{Number: 9, Title: "Something entirely unrelated about invoices", URL: "https://example.test/9"},
		},
	}
	igRegisterReadProvider(t, p)
	return p
}

// ---------------------------------------------------------------------------
// Happy path (cross-boundary end-to-end)
// ---------------------------------------------------------------------------

// TestFileWorkItem_IntakeSignalsEndToEnd is the CROSS-BOUNDARY test: one
// request drives the real handler -> shared filing core -> the intake hook ->
// the reader seam and the repodoc charter seam -> the rendered body the
// provider receives -> the 201 payload -> the MCP decoding path.
//
// Binding approval condition L3 is satisfied inside this test: the handler's
// ACTUAL response bytes are fed through mcpserver's decoding shape, rather
// than a second, independently-maintained fixture that could drift from the
// handler's serialization while both stayed individually valid.
func TestFileWorkItem_IntakeSignalsEndToEnd(t *testing.T) {
	p := igHealthyProvider(t)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
	}, "github:operator")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	// (a) The body the PROVIDER received carries the rendered advisory section
	// AND a parseable hidden marker.
	if !p.called {
		t.Fatal("provider was not called")
	}
	body := p.captured.Item.Body
	if !strings.Contains(body, intakegroom.SectionHeading) {
		t.Errorf("provider body carries no advisory section:\n%s", body)
	}
	parsed, ok := intakegroom.ParseBody(body)
	if !ok {
		t.Fatalf("hidden marker did not round-trip out of the filed body:\n%s", body)
	}
	if len(parsed.Duplicates) == 0 || parsed.Duplicates[0].Number != 1234 {
		t.Errorf("marker duplicates = %+v, want #1234 first", parsed.Duplicates)
	}

	// (b) The 201 carries the intake object with the expected candidate and
	// the cited rubric ids.
	resp := decodeWorkItem(t, rec)
	if resp.Intake == nil {
		t.Fatalf("201 carries no intake object (body=%s)", rec.Body.String())
	}
	if resp.Intake.Degraded {
		t.Errorf("intake degraded = true (%s), want a healthy hook", resp.Intake.DegradeReason)
	}
	if len(resp.Intake.Duplicates) == 0 || resp.Intake.Duplicates[0].Number != 1234 {
		t.Errorf("response duplicates = %+v, want #1234 first", resp.Intake.Duplicates)
	}
	if resp.Intake.Score.Unscored {
		t.Errorf("score unscored with a rubric-bearing charter: %+v", resp.Intake.Score)
	}
	cited := map[string]bool{}
	for _, c := range resp.Intake.Score.Citations {
		cited[c.RubricID] = true
		if c.Quote == "" {
			t.Errorf("citation %s carries no charter quote", c.RubricID)
		}
	}
	// The filing declares no parent epic and no depends_on, and has a
	// medium-or-better duplicate: all three structural rules fire.
	for _, want := range []string{"S2", "S4", "U4"} {
		if !cited[want] {
			t.Errorf("rubric id %s not cited; cited=%v", want, cited)
		}
	}
	if resp.Intake.ScannedItems != 3 {
		t.Errorf("scanned_items = %d, want 3", resp.Intake.ScannedItems)
	}

	// The read was pushed down as a bounded, newest-first window.
	p.mu.Lock()
	got := p.gotRequest
	p.mu.Unlock()
	if !got.Newest || got.MaxScanned != intakegroom.DefaultMaxScanned || !got.IncludeClosed || got.ResolveBoardState {
		t.Errorf("ListWorkItems request = %+v, want Newest+MaxScanned=%d+IncludeClosed, ResolveBoardState=false",
			got, intakegroom.DefaultMaxScanned)
	}

	// (c) L3: the handler's REAL bytes decode through the MCP path.
	assertMCPDecodesIntake(t, rec.Body.Bytes())

	// NOTHING-DESTRUCTIVE, read as COMMITTED STATE after the call returned: a
	// control that fired and rolled back would return a byte-identical
	// response, so this reads the counters rather than an error identity.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.transitionCalls != 0 || p.closeCalls != 0 || p.labelMutateCalls != 0 || p.groomingApplyCall != 0 {
		t.Errorf("intake grooming dialed a destructive seam: transition=%d close=%d label=%d grooming=%d",
			p.transitionCalls, p.closeCalls, p.labelMutateCalls, p.groomingApplyCall)
	}
	// The duplicate candidate's own record is unchanged.
	if p.items[0].Number != 1234 || p.items[0].Title != "[E22.4] Add the widget endpoint" {
		t.Errorf("the duplicate candidate's record was mutated: %+v", p.items[0])
	}
}

// assertMCPDecodesIntake feeds the handler's ACTUAL 201 bytes through the MCP
// decoding path (binding approval condition L3).
//
// It decodes into mcpserver.FiledWorkItem — the very struct
// fishhawk_file_issue returns to the agent — rather than into a separately
// maintained fixture "described as identical". Those two can drift apart
// silently: a fixture stays valid while the handler's serialization moves and
// nothing reddens. There is exactly ONE payload here, produced by the real
// handler, and the boundary is exercised rather than asserted twice.
//
// mcpserver's structs are deliberately LOCAL decode-only mirrors (ADR-064
// keeps workmgmt out of that package), which is precisely why a drift check is
// needed: nothing in the type system couples them to the server's response.
func assertMCPDecodesIntake(t *testing.T, responseBytes []byte) {
	t.Helper()
	var filed mcpserver.FiledWorkItem
	dec := json.NewDecoder(bytes.NewReader(responseBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&filed); err != nil {
		t.Fatalf("the handler's 201 does not decode through the MCP path: %v\nbody=%s", err, responseBytes)
	}
	if filed.Intake == nil {
		t.Fatalf("MCP decode dropped the intake object; the local mirror has drifted from the handler payload\nbody=%s", responseBytes)
	}
	if len(filed.Intake.Duplicates) == 0 || filed.Intake.Duplicates[0].Number != 1234 {
		t.Errorf("MCP-decoded duplicates = %+v, want #1234 first", filed.Intake.Duplicates)
	}
	if filed.Intake.Duplicates[0].Confidence == "" || filed.Intake.Duplicates[0].Basis == "" {
		t.Errorf("MCP-decoded duplicate lost its confidence band / basis: %+v", filed.Intake.Duplicates[0])
	}
	if len(filed.Intake.Score.Citations) == 0 {
		t.Errorf("MCP-decoded score lost its citations: %+v", filed.Intake.Score)
	}
	for _, c := range filed.Intake.Score.Citations {
		if c.RubricID == "" || c.Quote == "" {
			t.Errorf("MCP-decoded citation is missing fields: %+v", c)
		}
	}
	if filed.Intake.ScannedItems == 0 {
		t.Error("MCP-decoded scanned_items = 0, want the real scan count")
	}
}

// ---------------------------------------------------------------------------
// Per-failure-mode: one test per enumerated degradation branch.
// Each asserts 201 + the issue still created + the exact typed reason.
// ---------------------------------------------------------------------------

// igAssertDegraded files a work item and asserts the filing SUCCEEDED and the
// hook degraded with exactly want.
func igAssertDegraded(t *testing.T, s *Server, filed func() bool, want intakegroom.DegradeReason) {
	t.Helper()
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
	}, "github:operator")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a degraded hook must never fail a filing (body=%s)",
			rec.Code, rec.Body.String())
	}
	if !filed() {
		t.Fatal("the work item was not filed; a degraded hook must still create the issue")
	}
	resp := decodeWorkItem(t, rec)
	if resp.Number == 0 || resp.URL == "" {
		t.Errorf("no created issue echoed on the 201: %+v", resp)
	}
	if resp.Intake == nil {
		t.Fatalf("no intake object on the 201 (body=%s)", rec.Body.String())
	}
	if !resp.Intake.Degraded {
		t.Fatalf("intake degraded = false, want a degradation with reason %q", want)
	}
	if resp.Intake.DegradeReason != want {
		t.Errorf("degrade_reason = %q, want %q", resp.Intake.DegradeReason, want)
	}
}

// TestIntakeHook_ReaderUnavailableStillFiles: a File-only provider, so
// ReaderFor returns *UnavailableError{ReasonNotImplemented}.
func TestIntakeHook_ReaderUnavailableStillFiles(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))
	igAssertDegraded(t, s, func() bool { return fp.called }, intakegroom.DegradeReasonReaderUnavailable)
}

// TestIntakeHook_ReaderErrorStillFiles: the reader is present and refuses.
// This is also COUNTERFACTUAL (3): replace the degrade path with an error
// return and this goes 502 instead of 201.
func TestIntakeHook_ReaderErrorStillFiles(t *testing.T) {
	p := &igReadProvider{listErr: &workmgmt.UnavailableError{
		Provider:   workmgmt.Default().Provider,
		Capability: workmgmt.ReaderCapability,
		Reason:     workmgmt.ReasonForbidden,
		Detail:     "the forge refused the read",
	}}
	igRegisterReadProvider(t, p)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonReaderError)
}

// TestIntakeHook_CharterUndeclaredStillFiles: conv.Charter is nil.
//
// This is the branch binding approval condition L4 is about: everywhere else
// in E54 this REFUSES. Here it degrades, and the item is filed.
func TestIntakeHook_CharterUndeclaredStillFiles(t *testing.T) {
	p := igHealthyProvider(t)
	installConventions(t, chConventionsWithoutCharter(), nil)
	s := New(igCharterConfig(igCharterDoc, false))
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonCharterUndeclared)
}

// TestIntakeHook_CharterUnresolvedStillFiles: the declared path does not exist
// at the pinned commit (repodoc.ErrMissingDocument).
func TestIntakeHook_CharterUnresolvedStillFiles(t *testing.T) {
	p := igHealthyProvider(t)
	igInstallCharterConventions(t)
	s := New(igCharterConfig("", true))
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonCharterUnresolved)
}

// TestIntakeHook_CharterRubricUnparsedStillFiles: the charter resolves but
// carries no rubric table.
func TestIntakeHook_CharterRubricUnparsedStillFiles(t *testing.T) {
	p := igHealthyProvider(t)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterNoRubric, false))
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonCharterRubricUnparsed)
}

// TestIntakeHook_SeamUnwiredStillFiles: a deployment with no document
// resolver / base-ref resolver. This is also the documented no-revert kill
// switch for the whole feature.
func TestIntakeHook_SeamUnwiredStillFiles(t *testing.T) {
	p := igHealthyProvider(t)
	igInstallCharterConventions(t)
	s := New(Config{})
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonSeamUnwired)
}

// TestIntakeHook_BaseRefErrorStillFiles: the base-ref resolver is wired but
// fails. Distinct branch from an unwired seam, and it maps to
// charter_unresolved because the charter is declared and simply could not be
// pinned.
func TestIntakeHook_BaseRefErrorStillFiles(t *testing.T) {
	p := igHealthyProvider(t)
	igInstallCharterConventions(t)
	cfg := igCharterConfig(igCharterDoc, false)
	cfg.DocumentBaseRef = func(context.Context, forge.RepoRef) (string, error) {
		return "", errors.New("forge unreachable")
	}
	s := New(cfg)
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonCharterUnresolved)
}

// TestIntakeHook_DocumentScopeErrorStillFiles: the credential-scope resolver
// is wired and fails. Its own branch, same degrade reason.
func TestIntakeHook_DocumentScopeErrorStillFiles(t *testing.T) {
	p := igHealthyProvider(t)
	igInstallCharterConventions(t)
	cfg := igCharterConfig(igCharterDoc, false)
	cfg.DocumentScope = func(context.Context, forge.RepoRef) (forge.CredentialScope, error) {
		return forge.CredentialScope{}, errors.New("no installation for this repo")
	}
	s := New(cfg)
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonCharterUnresolved)
}

// TestIntakeHook_NilPageStillFiles: a reader that violates its own contract
// by returning (nil, nil). The hook treats it as a reader error rather than
// dereferencing it.
func TestIntakeHook_NilPageStillFiles(t *testing.T) {
	p := &igNilPageProvider{}
	p.name = workmgmt.Default().Provider
	workmgmt.Register(p)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonReaderError)
}

// igNilPageProvider returns the contract-violating (nil, nil).
type igNilPageProvider struct{ fakeWorkProvider }

func (p *igNilPageProvider) ReadWorkItem(context.Context, workmgmt.ReadWorkItemRequest) (*workmgmt.WorkItemRecord, error) {
	return nil, errors.New("not used")
}

func (p *igNilPageProvider) ListWorkItems(context.Context, workmgmt.ListWorkItemsRequest) (*workmgmt.WorkItemPage, error) {
	return nil, nil //nolint:nilnil // deliberately the contract violation under test
}

// TestIntakeHook_PanickingReaderStillFiles: a reader that panics
// SYNCHRONOUSLY — the reachable panic case, since the hook starts no goroutine
// of its own.
//
// COUNTERFACTUAL (2): delete the deferred recover() in runIntakeGroom and this
// test dies with the panic instead of asserting a 201.
func TestIntakeHook_PanickingReaderStillFiles(t *testing.T) {
	p := &igReadProvider{panicOnList: true}
	igRegisterReadProvider(t, p)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))
	igAssertDegraded(t, s, func() bool { return p.called }, intakegroom.DegradeReasonHookPanic)
}

// TestIntakeHook_WedgedReaderDegradesWithinBudget is the BUDGET branch and
// COUNTERFACTUAL (1) for the hook's own context.WithTimeout.
//
// L1, STATED HONESTLY. The fake is deliberately CANCELLATION-COOPERATIVE: it
// selects on ctx.Done(). That is not a weaker test dressed up as a strong one
// — it is the only honest one available, because a context deadline cannot
// preempt a callee that never consults the context, so a fake that blocked
// unconditionally would be asserting a property this mechanism does not have.
// What this pins is the PLUMBING: that the hook derives its own deadline from
// intakegroom.DefaultDeadline, hands that context (not the caller's) to the
// reader, and maps the resulting cancellation onto budget_exceeded.
//
// The counterfactual is real rather than a hang: with the WithTimeout deleted,
// the reader's ctx is the caller's request context, which is never cancelled,
// so the fake falls through to its wedge timer and returns a NON-deadline
// error — the reason becomes reader_error and the elapsed bound is blown.
// Both assertions go red.
func TestIntakeHook_WedgedReaderDegradesWithinBudget(t *testing.T) {
	// The wedge must outlast the hook's fixed 3s deadline by a wide margin on a
	// loaded runner, so it is timescale-derived while the deadline is not (it
	// is a shipped constant). elapsedBound sits strictly between them.
	wedge := timescale.D(20 * time.Second)
	elapsedBound := intakegroom.DefaultDeadline + timescale.D(2*time.Second)
	if elapsedBound >= wedge {
		t.Fatalf("test is not discriminating: elapsedBound %s >= wedge %s", elapsedBound, wedge)
	}

	p := &igReadProvider{wedge: wedge}
	igRegisterReadProvider(t, p)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))

	start := time.Now()
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
	}, "github:operator")
	elapsed := time.Since(start)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if resp.Intake == nil || !resp.Intake.Degraded {
		t.Fatalf("intake = %+v, want a degradation", resp.Intake)
	}
	if resp.Intake.DegradeReason != intakegroom.DegradeReasonBudgetExceeded {
		t.Errorf("degrade_reason = %q, want %q", resp.Intake.DegradeReason, intakegroom.DegradeReasonBudgetExceeded)
	}
	if elapsed > elapsedBound {
		t.Errorf("filing took %s, want <= %s (the hook's deadline did not bound the read)", elapsed, elapsedBound)
	}
	t.Logf("MEASURED wedged-reader filing: elapsed=%s reported duration_ms=%d bound=%s wedge=%s",
		elapsed, resp.Intake.DurationMS, elapsedBound, wedge)
}

// TestIntakeHook_MeasuredAddedLatencyWithinBudget reports the healthy-path
// added latency rather than merely asserting it (acceptance criterion 6). The
// wedged half is measured and logged by the test above.
func TestIntakeHook_MeasuredAddedLatencyWithinBudget(t *testing.T) {
	p := igHealthyProvider(t)
	_ = p
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))

	start := time.Now()
	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
	}, "github:operator")
	elapsed := time.Since(start)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeWorkItem(t, rec)
	if resp.Intake == nil {
		t.Fatal("no intake object")
	}
	bound := intakegroom.DefaultDeadline + timescale.D(2*time.Second)
	if elapsed > bound {
		t.Errorf("healthy filing took %s, want <= %s", elapsed, bound)
	}
	if resp.Intake.DurationMS < 0 {
		t.Errorf("duration_ms = %d, want a non-negative measurement", resp.Intake.DurationMS)
	}
	t.Logf("MEASURED healthy filing: elapsed=%s reported duration_ms=%d bound=%s",
		elapsed, resp.Intake.DurationMS, bound)
}

// TestIntakeHook_ProductionReadPathIsCancellationCooperative is the source-level
// assertion L1 requires: the bound above is conditional on a
// cancellation-respecting reader, so the claim that the PRODUCTION path is one
// must itself be pinned rather than asserted in prose.
//
// The GitHub work-item reader reaches the forge through githubclient, whose
// requests are built with http.NewRequestWithContext — Go's HTTP client returns
// at a context deadline. If a future edit built a request without the context,
// this fails and the honest bound above would silently stop holding.
func TestIntakeHook_ProductionReadPathIsCancellationCooperative(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "githubclient", "graphql.go"))
	if err != nil {
		// Locate the file that actually issues the GraphQL request.
		entries, derr := os.ReadDir(filepath.Join("..", "githubclient"))
		if derr != nil {
			t.Fatalf("read githubclient dir: %v", derr)
		}
		var found bool
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, rerr := os.ReadFile(filepath.Join("..", "githubclient", e.Name()))
			if rerr != nil {
				continue
			}
			if strings.Contains(string(b), "http.NewRequestWithContext") {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("no githubclient source builds a request with http.NewRequestWithContext; " +
				"the intake hook's conditional latency bound depends on the production read path honouring ctx")
		}
		return
	}
	if !strings.Contains(string(src), "http.NewRequestWithContext") {
		t.Error("githubclient/graphql.go no longer builds its request with http.NewRequestWithContext; " +
			"the intake hook's latency bound is conditional on a cancellation-respecting reader")
	}
}

// TestIntakeHook_DegradedFilingBodyIsUnchanged pins the blast-radius claim: a
// degraded hook leaves the filed body BYTE-IDENTICAL to what it would have been
// without the feature. It is what keeps every existing body-asserting test
// green, and it is asserted rather than assumed.
func TestIntakeHook_DegradedFilingBodyIsUnchanged(t *testing.T) {
	// Baseline: a File-only provider with the seams unwired — grooming cannot
	// run at all.
	base := &fakeWorkProvider{}
	registerFakeProvider(t, base)
	installConventions(t, chConventionsWithoutCharter(), nil)
	s := New(Config{})
	if rec := igFileFeatureRec(t, s); rec.Code != http.StatusCreated {
		t.Fatalf("baseline status = %d", rec.Code)
	}
	baselineBody := base.captured.Item.Body

	// Same filing, with the reader wired but erroring: still degraded, so the
	// body must not have moved.
	p := &igReadProvider{listErr: errors.New("forge refused")}
	igRegisterReadProvider(t, p)
	igInstallCharterConventions(t)
	s2 := New(igCharterConfig(igCharterDoc, false))
	if rec := igFileFeatureRec(t, s2); rec.Code != http.StatusCreated {
		t.Fatalf("degraded status = %d", rec.Code)
	}
	if p.captured.Item.Body != baselineBody {
		t.Errorf("a degraded hook changed the filed body:\n got: %q\nwant: %q", p.captured.Item.Body, baselineBody)
	}
	if strings.Contains(p.captured.Item.Body, intakegroom.MarkerPrefix) {
		t.Error("a degraded filing carries an intake marker; the no-op render did not hold")
	}
}

// igFileFeatureRec POSTs the standard filing used across these tests.
func igFileFeatureRec(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	return fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
	}, "github:operator")
}

// TestIntakeHook_AuditCarriesIntakeSummary pins the audit surface: the compact
// intake summary rides inside the EXISTING work_item_filed payload rather than
// a new category.
func TestIntakeHook_AuditCarriesIntakeSummary(t *testing.T) {
	igHealthyProvider(t)
	igInstallCharterConventions(t)

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
	cfg := igCharterConfig(igCharterDoc, false)
	cfg.AuditRepo = au
	cfg.RunRepo = rr
	s := New(cfg)

	rec := fileWorkItem(t, s, workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
		RunID:     runID.String(),
	}, "mcp:run:"+runID.String())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	var payload map[string]any
	var found bool
	for _, e := range au.appended {
		if e.Category != categoryWorkItemFiled {
			continue
		}
		found = true
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode audit payload: %v", err)
		}
	}
	if !found {
		t.Fatal("no work_item_filed audit entry")
	}
	intake, ok := payload["intake"].(map[string]any)
	if !ok {
		t.Fatalf("audit payload carries no intake summary: %v", payload)
	}
	if intake["duplicate_count"] == nil || intake["top_duplicate_number"] == nil {
		t.Errorf("intake summary missing duplicate fields: %v", intake)
	}
	if intake["cited_rubric_ids"] == nil {
		t.Errorf("intake summary missing cited_rubric_ids: %v", intake)
	}
}

// TestDeferConcern_IntakeSignalsRenderedThroughSharedCore is binding approval
// condition L5: the SECONDARY (non-HTTP-filing) call sites get intake grooming
// for free by sharing applyAndFileWorkItem's core, and they discard the
// returned Signals.
//
// Without this, that "for free" claim is unpinned — a later change that
// special-cased one site (running the hook only on the HTTP handler, say)
// would redden nothing at all. So this drives the DEFER-CONCERN path end to
// end under a healthy reader and asserts the OBSERVABLE consequence: the
// follow-up issue that path filed carries the rendered advisory section and a
// parseable marker, exactly as an HTTP filing does.
//
// It reads the body the provider received rather than the response, because
// the defer path never surfaces the signals — the shared-core claim is about
// what LANDS on the issue, not about what any caller returns.
func TestDeferConcern_IntakeSignalsRenderedThroughSharedCore(t *testing.T) {
	s, repo, _, cr, _ := deferServer(t)

	// Swap in a reader-capable provider so the hook can enumerate a window.
	// The candidate duplicates the concern's own follow-up title.
	p := &igReadProvider{items: []workmgmt.WorkItemRecord{
		{Number: 4711, Title: "[E22.9] Flush the buffer on shutdown", URL: "https://example.test/4711"},
	}}
	igRegisterReadProvider(t, p)
	igInstallCharterConventions(t)
	s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: &igFetcher{content: igCharterDoc}, Commits: igCommits{}}
	s.cfg.DocumentBaseRef = func(context.Context, forge.RepoRef) (string, error) { return igBranch, nil }

	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "flush the buffer on shutdown")

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{
		ParentEpic: "#389",
		N:          "9",
	}, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	if !p.called {
		t.Fatal("the defer path did not reach the provider")
	}
	body := p.captured.Item.Body
	if !strings.Contains(body, intakegroom.SectionHeading) {
		t.Fatalf("the defer-filed follow-up carries no advisory section; the shared-core claim does not hold:\n%s", body)
	}
	parsed, ok := intakegroom.ParseBody(body)
	if !ok {
		t.Fatalf("the defer-filed follow-up carries no parseable intake marker:\n%s", body)
	}
	if len(parsed.Duplicates) == 0 || parsed.Duplicates[0].Number != 4711 {
		t.Errorf("defer-path duplicates = %+v, want #4711", parsed.Duplicates)
	}
	if parsed.Degraded {
		t.Errorf("defer-path grooming degraded (%s), want a healthy hook", parsed.DegradeReason)
	}
}
