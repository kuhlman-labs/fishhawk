package server

// Tests for the non-mutating intake preview (#3774 / E81.4) and the
// prepareWorkItem lock-release contract the preview depends on.
//
// The organising claim is that a preview is the filing MINUS provider.File:
// it renders the same title/body/labels/number/intake a filing would send,
// and it creates, writes and holds nothing. Every preview-test fake therefore
// carries a tripwire (pvGuard) that t.Fatal's on File and on every other
// mutating capability the fake implements, so a preview that ever reached one
// fails at the call site rather than merely leaving a counter non-zero.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ---------------------------------------------------------------------------
// Tripwire fakes
// ---------------------------------------------------------------------------

// pvGuard is the "a preview creates nothing" tripwire. File is legal only once
// a test has explicitly armed a real filing (fileOK); Transition and
// ApplyGroomingMutation — the capability seams every label, board-placement,
// epic-link and close mutation is dispatched through — are never legal on
// either path. (workmgmt exposes no separate comment capability on a filing
// provider: the GitHub provider's post-create writes all happen inside File.)
type pvGuard struct {
	t      *testing.T
	fileOK atomic.Bool
}

func (g *pvGuard) file() {
	if !g.fileOK.Load() {
		g.t.Fatal("provider.File was dialed while previewing; a preview must create nothing")
	}
}

func (g *pvGuard) never(capability string) {
	g.t.Fatalf("the mutating capability %s was dialed; neither a preview nor a filing may act on the tracker this way", capability)
}

// pvReadProvider is a reader-capable, number-discovering fake behind the
// tripwire.
type pvReadProvider struct {
	igReadProvider
	guard      *pvGuard
	discovered []int
}

var (
	_ workmgmt.WorkItemReader   = (*pvReadProvider)(nil)
	_ workmgmt.NumberDiscoverer = (*pvReadProvider)(nil)
	_ workmgmt.Transitioner     = (*pvReadProvider)(nil)
	_ workmgmt.GroomingMutator  = (*pvReadProvider)(nil)
)

func (p *pvReadProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.guard.file()
	return p.fakeWorkProvider.File(ctx, req)
}

func (p *pvReadProvider) Transition(context.Context, workmgmt.TransitionRequest) (*workmgmt.TransitionResult, error) {
	p.guard.never("Transition")
	return nil, errors.New("unreachable")
}

func (p *pvReadProvider) ApplyGroomingMutation(_ context.Context, req workmgmt.GroomingMutationRequest) (*workmgmt.GroomingMutationResult, error) {
	p.guard.never("ApplyGroomingMutation(" + string(req.Kind) + ")")
	return nil, errors.New("unreachable")
}

func (p *pvReadProvider) DiscoverNumbers(context.Context, workmgmt.DiscoverNumbersRequest) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.discovered...), nil
}

func (p *pvReadProvider) lists() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listCalls
}

// pvRegisterRead registers a tripwired reader whose window is items.
func pvRegisterRead(t *testing.T, items []workmgmt.WorkItemRecord) *pvReadProvider {
	t.Helper()
	p := &pvReadProvider{guard: &pvGuard{t: t}}
	p.items = items
	p.name = workmgmt.Default().Provider
	workmgmt.Register(p)
	return p
}

// pvHealthyItems is igHealthyProvider's window: #1234 duplicates the standard
// draft, #22 is an epic candidate.
func pvHealthyItems() []workmgmt.WorkItemRecord {
	return []workmgmt.WorkItemRecord{
		{Number: 1234, Title: "[E22.4] Add the widget endpoint", URL: "https://example.test/1234", Labels: []string{"type:chore", "area:backend"}},
		{Number: 22, Title: "[E22] Widget platform", URL: "https://example.test/22", Labels: []string{"epic", "area:backend"}},
		{Number: 9, Title: "Something entirely unrelated about invoices", URL: "https://example.test/9"},
	}
}

// pvFileOnlyProvider is a File-only (no reader) fake behind the tripwire, so
// the intake hook degrades reader_unavailable.
type pvFileOnlyProvider struct {
	fakeWorkProvider
	guard *pvGuard
}

func (p *pvFileOnlyProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.guard.file()
	return p.fakeWorkProvider.File(ctx, req)
}

func (p *pvFileOnlyProvider) Transition(context.Context, workmgmt.TransitionRequest) (*workmgmt.TransitionResult, error) {
	p.guard.never("Transition")
	return nil, errors.New("unreachable")
}

func (p *pvFileOnlyProvider) ApplyGroomingMutation(_ context.Context, req workmgmt.GroomingMutationRequest) (*workmgmt.GroomingMutationResult, error) {
	p.guard.never("ApplyGroomingMutation(" + string(req.Kind) + ")")
	return nil, errors.New("unreachable")
}

// pvChildProvider is the child-number-capable fake (takes the per-epic lock)
// behind the tripwire.
type pvChildProvider struct {
	fakeChildNumberProvider
	guard *pvGuard
}

func (p *pvChildProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.guard.file()
	return p.fakeChildNumberProvider.File(ctx, req)
}

func (p *pvChildProvider) Transition(context.Context, workmgmt.TransitionRequest) (*workmgmt.TransitionResult, error) {
	p.guard.never("Transition")
	return nil, errors.New("unreachable")
}

func (p *pvChildProvider) ApplyGroomingMutation(_ context.Context, req workmgmt.GroomingMutationRequest) (*workmgmt.GroomingMutationResult, error) {
	p.guard.never("ApplyGroomingMutation(" + string(req.Kind) + ")")
	return nil, errors.New("unreachable")
}

// pvSeqProvider is the sequential-number-capable fake (takes the per-(repo,
// prefix) lock) behind the tripwire.
type pvSeqProvider struct {
	fakeSequentialNumberProvider
	guard *pvGuard
}

func (p *pvSeqProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.guard.file()
	return p.fakeSequentialNumberProvider.File(ctx, req)
}

func (p *pvSeqProvider) Transition(context.Context, workmgmt.TransitionRequest) (*workmgmt.TransitionResult, error) {
	p.guard.never("Transition")
	return nil, errors.New("unreachable")
}

func (p *pvSeqProvider) ApplyGroomingMutation(_ context.Context, req workmgmt.GroomingMutationRequest) (*workmgmt.GroomingMutationResult, error) {
	p.guard.never("ApplyGroomingMutation(" + string(req.Kind) + ")")
	return nil, errors.New("unreachable")
}

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

// previewWorkItemRec POSTs body to the preview handler as id.
func previewWorkItemRec(t *testing.T, s *Server, body workItemRequest, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/work-items/preview", bytes.NewReader(raw))
	req = withIdentity(req, id)
	rec := httptest.NewRecorder()
	s.handlePreviewWorkItem(rec, req)
	return rec
}

func decodeWorkItemPreview(t *testing.T, rec *httptest.ResponseRecorder) workItemPreviewResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp workItemPreviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preview: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

func pvDecodeErr(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Error
}

// pvOperator is a non-run-bound operator identity.
func pvOperator() Identity { return Identity{Subject: "github:operator"} }

// pvStandardDraft is the standard chore draft the intake fixtures duplicate.
func pvStandardDraft() workItemRequest {
	return workItemRequest{
		Repo:      "kuhlman-labs/fishhawk",
		Type:      "chore",
		Summary:   "Add the widget endpoint",
		TitleVars: map[string]string{"epic": "22", "n": "5"},
	}
}

// durationRE matches the ONE wall-clock byte run a preview and a later filing
// legitimately differ in: the hook's measured duration inside the hidden marker.
var durationRE = regexp.MustCompile(`"duration_ms":\d+`)

func normalizeDuration(body string) string {
	return durationRE.ReplaceAllString(body, `"duration_ms":0`)
}

// signalsJSON renders signals with the measured duration zeroed, for an
// equality comparison that ignores only wall clock.
func signalsJSON(t *testing.T, s intakegroom.Signals) string {
	t.Helper()
	s.DurationMS = 0
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal signals: %v", err)
	}
	return string(raw)
}

func sortedCopy(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Done-means: a preview is the filing minus File
// ---------------------------------------------------------------------------

// TestPreviewWorkItem_MatchesSubsequentFiling is the cross-boundary done-means:
// request -> shared prelude -> prepareWorkItem -> intake hook (fake reader seam
// + repodoc charter seam) -> 200, then the SAME draft filed through POST
// /v0/work-items. The preview must equal what the provider received — title,
// body (modulo the marker's duration_ms), labels (order-insensitive), number —
// and its intake must equal the 201's, while the preview itself creates
// nothing and writes no audit. The numbered (adr) subcase makes number parity
// non-vacuous.
func TestPreviewWorkItem_MatchesSubsequentFiling(t *testing.T) {
	cases := map[string]struct {
		draft      workItemRequest
		wantNumber int
	}{
		"chore": {draft: pvStandardDraft(), wantNumber: 0},
		"numbered adr": {draft: workItemRequest{
			Repo:    "kuhlman-labs/fishhawk",
			Type:    "adr",
			Summary: "Add the widget endpoint",
		}, wantNumber: 80},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := pvRegisterRead(t, pvHealthyItems())
			p.discovered = []int{79}
			igInstallCharterConventions(t)

			au := newAuditFake()
			rr := newPromptRunRepo()
			runID := uuid.New()
			inst := int64(99)
			rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning, InstallationID: &inst}
			cfg := igCharterConfig(igCharterDoc, false)
			cfg.AuditRepo = au
			cfg.RunRepo = rr
			s := New(cfg)

			auditCount := func() int {
				au.mu.Lock()
				defer au.mu.Unlock()
				return len(au.appended)
			}
			before := auditCount()

			pv := decodeWorkItemPreview(t, previewWorkItemRec(t, s, tc.draft, pvOperator()))
			if p.called {
				t.Fatal("the preview reached provider.File")
			}
			if got := auditCount(); got != before {
				t.Fatalf("audit entries = %d after the preview, want %d unchanged — a preview writes no audit", got, before)
			}
			if !pv.Intake.HasFindings() || !strings.Contains(pv.Body, intakegroom.SectionHeading) {
				t.Fatalf("the preview carries no intake findings, so the equality below would be vacuous: %+v", pv.Intake)
			}

			// Now FILE the same draft (run-scoped, so the filing is audited).
			p.guard.fileOK.Store(true)
			filed := tc.draft
			filed.RunID = runID.String()
			frec := fileWorkItem(t, s, filed, "mcp:run:"+runID.String())
			if frec.Code != http.StatusCreated {
				t.Fatalf("filing status = %d, want 201 (body=%s)", frec.Code, frec.Body.String())
			}
			if !p.called {
				t.Fatal("the filing did not reach provider.File")
			}
			if got := auditCount(); got != before+1 {
				t.Errorf("audit entries = %d after the filing, want %d (grows only on the filing)", got, before+1)
			}

			got := p.captured
			if pv.Title != got.Item.Title {
				t.Errorf("preview title = %q, filed title = %q", pv.Title, got.Item.Title)
			}
			if normalizeDuration(pv.Body) != normalizeDuration(got.Item.Body) {
				t.Errorf("preview body differs from the filed body beyond duration_ms:\n--- preview\n%s\n--- filed\n%s", pv.Body, got.Item.Body)
			}
			if a, b := sortedCopy(pv.Labels), sortedCopy(got.Item.Classification.Labels); strings.Join(a, ",") != strings.Join(b, ",") {
				t.Errorf("preview labels = %v, filed labels = %v", a, b)
			}
			if pv.Number != got.Number || pv.Number != tc.wantNumber {
				t.Errorf("preview number = %d, filed number = %d, want both %d", pv.Number, got.Number, tc.wantNumber)
			}
			resp := decodeWorkItem(t, frec)
			if resp.Intake == nil {
				t.Fatalf("the 201 carries no intake (body=%s)", frec.Body.String())
			}
			if a, b := signalsJSON(t, pv.Intake), signalsJSON(t, *resp.Intake); a != b {
				t.Errorf("preview intake differs from the filing's beyond duration_ms:\n--- preview\n%s\n--- filed\n%s", a, b)
			}
			if pv.Provider != workmgmt.Default().Provider {
				t.Errorf("preview provider = %q, want %q", pv.Provider, workmgmt.Default().Provider)
			}
		})
	}
}

// TestPreviewWorkItem_SourceRefsExcludedNearDuplicateStillFlagged: #1234 is the
// draft's declared source (near-identical title, DIFFERENT body, so the
// byte-identical self-match guard cannot be what excludes it) and #1240 an
// unrelated near-duplicate. The source is excluded and reported as
// derives_from; the unrelated one is still flagged.
func TestPreviewWorkItem_SourceRefsExcludedNearDuplicateStillFlagged(t *testing.T) {
	p := pvRegisterRead(t, []workmgmt.WorkItemRecord{
		{Number: 1234, Title: "[E22.4] Add the widget endpoint", Body: "the source item's own body", URL: "https://example.test/1234", Labels: []string{"type:chore", "area:backend"}},
		{Number: 1240, Title: "Widget endpoint pagination", URL: "https://example.test/1240", Labels: []string{"type:chore"}},
		{Number: 22, Title: "[E22] Widget platform", URL: "https://example.test/22", Labels: []string{"epic", "area:backend"}},
	})
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))

	draft := pvStandardDraft()
	draft.Body = "A draft written from #1234."
	draft.SourceRefs = []string{"#1234"}
	pv := decodeWorkItemPreview(t, previewWorkItemRec(t, s, draft, pvOperator()))

	for _, d := range pv.Intake.Duplicates {
		if d.Number == 1234 {
			t.Fatalf("the declared source #1234 was reported as a duplicate: %+v", pv.Intake.Duplicates)
		}
	}
	if len(pv.Intake.Duplicates) == 0 || pv.Intake.Duplicates[0].Number != 1240 {
		t.Errorf("the unrelated near-duplicate #1240 must still be flagged, got %+v", pv.Intake.Duplicates)
	}
	want := intakegroom.SourceItem{Number: 1234, Title: "[E22.4] Add the widget endpoint", URL: "https://example.test/1234", InWindow: true}
	if len(pv.Intake.DerivesFrom) != 1 || pv.Intake.DerivesFrom[0] != want {
		t.Errorf("derives_from = %+v, want [%+v]", pv.Intake.DerivesFrom, want)
	}
	if !strings.Contains(pv.Body, "**Derives from**") {
		t.Errorf("the previewed body does not render the Derives-from block:\n%s", pv.Body)
	}
	if p.called {
		t.Error("the preview reached provider.File")
	}
}

// TestPreviewWorkItem_DegradedIntakeReported: a degraded hook is still a 200
// preview, reports the typed reason, WARN-logs it, and leaves the body free of
// any advisory section (exactly as a degraded filing's body is unchanged).
func TestPreviewWorkItem_DegradedIntakeReported(t *testing.T) {
	cases := map[string]struct {
		register func(t *testing.T) func() bool
		want     intakegroom.DegradeReason
	}{
		"reader_unavailable": {
			register: func(t *testing.T) func() bool {
				p := &pvFileOnlyProvider{guard: &pvGuard{t: t}}
				p.name = workmgmt.Default().Provider
				workmgmt.Register(p)
				return func() bool { return p.called }
			},
			want: intakegroom.DegradeReasonReaderUnavailable,
		},
		"reader_error": {
			register: func(t *testing.T) func() bool {
				p := pvRegisterRead(t, nil)
				p.listErr = errors.New("the forge refused the read")
				return func() bool { return p.called }
			},
			want: intakegroom.DegradeReasonReaderError,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			called := tc.register(t)
			igInstallCharterConventions(t)
			cfg := igCharterConfig(igCharterDoc, false)
			sink := igCaptureDegrades(&cfg)
			s := New(cfg)

			pv := decodeWorkItemPreview(t, previewWorkItemRec(t, s, pvStandardDraft(), pvOperator()))
			if !pv.Intake.Degraded || pv.Intake.DegradeReason != tc.want {
				t.Fatalf("intake = degraded %v / %q, want %q", pv.Intake.Degraded, pv.Intake.DegradeReason, tc.want)
			}
			igAssertDegradeLogged(t, sink, tc.want)
			if strings.Contains(pv.Body, intakegroom.SectionHeading) || strings.Contains(pv.Body, intakegroom.MarkerPrefix) {
				t.Errorf("a degraded preview's body carries an advisory section:\n%s", pv.Body)
			}
			if called() {
				t.Error("the preview reached provider.File")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Prelude refusals (preview mode)
// ---------------------------------------------------------------------------

// TestPreviewWorkItem_RunBoundTokenRefused: a run-bound agent token is refused
// 403 preview_operator_only, with or without its own entitled run_id, and no
// tracker read happens. The CODE is the discriminator: with the refusal
// removed, the run-absent arm answers 403 run_scoped_filing_required and the
// run-id arm answers the 400 run_id refusal.
func TestPreviewWorkItem_RunBoundTokenRefused(t *testing.T) {
	for _, withRunID := range []bool{false, true} {
		name := "no run_id"
		if withRunID {
			name = "own entitled run_id"
		}
		t.Run(name, func(t *testing.T) {
			p := pvRegisterRead(t, pvHealthyItems())
			igInstallCharterConventions(t)
			rr := newPromptRunRepo()
			runID := uuid.New()
			rr.getRuns[runID] = &run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning}
			cfg := igCharterConfig(igCharterDoc, false)
			cfg.RunRepo = rr
			s := New(cfg)

			draft := pvStandardDraft()
			if withRunID {
				draft.RunID = runID.String()
			}
			rec := previewWorkItemRec(t, s, draft, Identity{Subject: "mcp:run:" + runID.String()})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			if e := pvDecodeErr(t, rec); e.Code != "preview_operator_only" {
				t.Errorf("code = %q, want preview_operator_only", e.Code)
			}
			if n := p.lists(); n != 0 {
				t.Errorf("tracker read %d times for a refused run-bound preview, want 0", n)
			}
			if p.called {
				t.Error("a refused preview reached provider.File")
			}
		})
	}
}

// TestPreviewWorkItem_RunIDRefused: an operator supplying a run_id is 400
// validation_failed {field: run_id}. With the refusal removed, the existing
// entitlement check answers 403 run_not_entitled instead.
func TestPreviewWorkItem_RunIDRefused(t *testing.T) {
	p := pvRegisterRead(t, pvHealthyItems())
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))

	draft := pvStandardDraft()
	draft.RunID = uuid.NewString()
	rec := previewWorkItemRec(t, s, draft, pvOperator())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	e := pvDecodeErr(t, rec)
	if e.Code != "validation_failed" || e.Details["field"] != "run_id" {
		t.Errorf("error = %+v, want validation_failed {field: run_id}", e)
	}
	if n := p.lists(); n != 0 {
		t.Errorf("tracker read %d times, want 0", n)
	}
}

// TestPreviewWorkItem_InvisibleRepoIs403: a cookie-session member whose
// visibility covers only other/repo is DENIED 403 repo_forbidden (the
// point-read posture), with no tracker read. The visible arm proves the
// fixture does not deny everything. With the enforceRepoVisibility call
// removed, the invisible arm proceeds to a 200 carrying the repo's titles.
func TestPreviewWorkItem_InvisibleRepoIs403(t *testing.T) {
	cases := map[string]struct {
		visible  map[string]bool
		wantCode int
	}{
		"invisible repo": {visible: map[string]bool{"other/repo": true}, wantCode: http.StatusForbidden},
		"visible repo":   {visible: map[string]bool{"kuhlman-labs/fishhawk": true}, wantCode: http.StatusOK},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := pvRegisterRead(t, pvHealthyItems())
			igInstallCharterConventions(t)
			cfg := igCharterConfig(igCharterDoc, false)
			cfg.AccountRoles = fakeAccountRoles{role: account.RoleMember}
			cfg.RepoVisibility = newFakeRepoVisibility(tc.visible)
			s := New(cfg)

			rec := previewWorkItemRec(t, s, pvStandardDraft(), memberIdentity())
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode != http.StatusForbidden {
				return
			}
			if e := pvDecodeErr(t, rec); e.Code != "repo_forbidden" {
				t.Errorf("code = %q, want repo_forbidden", e.Code)
			}
			if n := p.lists(); n != 0 {
				t.Errorf("tracker read %d times for an invisible repo, want 0", n)
			}
		})
	}
}

// TestPreviewWorkItem_MalformedSourceRefsIs400: the prelude refuses a
// malformed ref 400 validation_failed {field: source_refs, got: <ref>} before
// any forge round-trip. With the prelude check removed, the request reaches
// the core check and answers 422 instead.
func TestPreviewWorkItem_MalformedSourceRefsIs400(t *testing.T) {
	p := pvRegisterRead(t, pvHealthyItems())
	calls := installConventions(t, workmgmt.Default(), nil)
	s := New(Config{})

	draft := pvStandardDraft()
	draft.SourceRefs = []string{"#12", "abc"}
	rec := previewWorkItemRec(t, s, draft, pvOperator())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	e := pvDecodeErr(t, rec)
	if e.Code != "validation_failed" || e.Details["field"] != "source_refs" || e.Details["got"] != "abc" {
		t.Errorf("error = %+v, want validation_failed {field: source_refs, got: abc}", e)
	}
	if *calls != 0 {
		t.Errorf("conventions loaded %d times, want 0 — a malformed ref costs no forge round-trip", *calls)
	}
	if n := p.lists(); n != 0 || p.called {
		t.Errorf("tracker read %d times / provider called %v, want neither", n, p.called)
	}
}

// TestPreviewWorkItem_CoreRefusesMalformedSourceRefs drives the server-internal
// entry points DIRECTLY (no HTTP prelude): a malformed ref is refused 422
// work_item_invalid with details.source_refs_invalid by prepareWorkItem, for
// both the preview and the filing core. With the core check removed,
// intakeFilingFor silently drops the list and both calls succeed. The tracker
// is read back afterwards: nothing was read, nothing was filed.
func TestPreviewWorkItem_CoreRefusesMalformedSourceRefs(t *testing.T) {
	p := pvRegisterRead(t, pvHealthyItems())
	s := New(igCharterConfig(igCharterDoc, false))
	conv := workmgmt.Default()
	conv.Charter = &workmgmt.Charter{Path: igCharterPath}
	target := workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}, Project: conv.Project}
	filing := workmgmt.FilingRequest{
		Type:       "chore",
		Summary:    "Add the widget endpoint",
		TitleVars:  map[string]string{"epic": "22", "n": "5"},
		SourceRefs: []string{"not-a-ref"},
	}

	assert422 := func(t *testing.T, werr *workItemError) {
		t.Helper()
		if werr == nil {
			t.Fatal("a malformed source ref was accepted by the core")
		}
		if werr.status != http.StatusUnprocessableEntity || werr.code != "work_item_invalid" {
			t.Errorf("error = %d %s, want 422 work_item_invalid", werr.status, werr.code)
		}
		if got, _ := werr.details["source_refs_invalid"].(string); !strings.Contains(got, "not-a-ref") {
			t.Errorf("details.source_refs_invalid = %v, want it to name the ref", werr.details["source_refs_invalid"])
		}
	}

	pv, werr := s.previewWorkItem(context.Background(), filing, conv, target, "kuhlman-labs", "fishhawk")
	if pv != nil {
		t.Errorf("preview returned %+v alongside a refusal", pv)
	}
	assert422(t, werr)

	p.guard.fileOK.Store(true) // the filing core may legally File; it must not here
	_, created, ferr, _ := s.applyAndFileWorkItemWithIntake(context.Background(), filing, conv, target, "kuhlman-labs", "fishhawk")
	if created != nil {
		t.Errorf("the filing core created %+v for a malformed ref", created)
	}
	assert422(t, ferr)

	if n := p.lists(); n != 0 || p.called {
		t.Errorf("tracker read %d times / provider called %v, want neither", n, p.called)
	}
}

// TestPreviewWorkItemRoute_Registered: the route is on the real mux. An
// anonymous POST reaches the PREVIEW handler's own auth gate (its message
// names previewing), rather than a 404/405 or the filing handler.
func TestPreviewWorkItemRoute_Registered(t *testing.T) {
	h := New(Config{Addr: "127.0.0.1:0"}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v0/work-items/preview", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST /v0/work-items/preview = %d, want 401 (route registered, auth gate reached):\n%s", w.Code, w.Body.String())
	}
	if e := pvDecodeErr(t, w); !strings.Contains(e.Message, "previewing") {
		t.Errorf("message = %q, want the preview handler's own auth refusal", e.Message)
	}
}

// ---------------------------------------------------------------------------
// prepareWorkItem lock-release contract (binding approval condition 1)
// ---------------------------------------------------------------------------

// pvLockBound is the fileWorkItemWithin bound the lock tests use.
func pvLockBound() time.Duration { return timescale.D(2 * time.Second) }

// TestPrepareWorkItem_ErrorAfterLockSelfReleases (condition 1a): a prepare
// error raised AFTER a lock was taken must not leave the lock held — a
// same-epic / same-prefix filing right after completes within the bound.
//
// The two Apply-error arms are the counterfactual vehicles: the lock was
// handed to prepareWorkItem and then Apply refused an off-skeleton section, so
// ONLY prepareWorkItem's internal self-release frees it; delete that release
// and the follow-up filing times out. The capacity-refusal arm pins the
// derived-path cap refusal too, but deriveChildNumberTitleVar releases before
// it returns there, so that arm stays green under the same deletion. (A
// provider-resolution failure cannot follow a lock: both locks are taken only
// after workmgmt.Get succeeded.) Each arm uses its own repo, so a regression
// cannot wedge an unrelated test's lock key.
func TestPrepareWorkItem_ErrorAfterLockSelfReleases(t *testing.T) {
	t.Run("apply error after the child-number lock", func(t *testing.T) {
		fp := &pvChildProvider{guard: &pvGuard{t: t}}
		fp.children = []workmgmt.EpicChild{{Number: 501, Title: "[E93.1] first"}}
		registerFakeChildNumberProvider(t, &fp.fakeChildNumberProvider)
		workmgmt.Register(fp)
		fp.guard.fileOK.Store(true)
		s := New(Config{})

		bad := workItemRequest{
			Repo: "kuhlman-labs/lock-child", Type: "feature", Summary: "Refused after lock",
			TitleVars: map[string]string{"epic": "93"}, Relations: &workItemRelations{ParentEpic: "#9301"},
			Sections: map[string]string{"Summary": "s", "Impact": "off-skeleton"},
		}
		rec := fileWorkItemWithin(t, s, bad, pvLockBound())
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
		if fp.epicCalls != 1 {
			t.Fatalf("EpicChildren called %d times, want 1 — the child-number lock was never taken, so this arm proves nothing", fp.epicCalls)
		}

		good := bad
		good.Sections = nil
		if rec := fileWorkItemWithin(t, s, good, pvLockBound()); rec.Code != http.StatusCreated {
			t.Fatalf("same-epic follow-up status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("apply error after the sequential-number lock", func(t *testing.T) {
		fp := &pvSeqProvider{guard: &pvGuard{t: t}}
		fp.discovered = []int{79}
		registerFakeSequentialNumberProvider(t, &fp.fakeSequentialNumberProvider)
		workmgmt.Register(fp)
		fp.guard.fileOK.Store(true)
		s := New(Config{})

		bad := workItemRequest{
			Repo: "kuhlman-labs/lock-seq", Type: "adr", Summary: "Refused after lock",
			Sections: map[string]string{"Context": "c", "Impact": "off-skeleton"},
		}
		rec := fileWorkItemWithin(t, s, bad, pvLockBound())
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
		if fp.discoverCalls != 1 {
			t.Fatalf("DiscoverNumbers called %d times, want 1 — the sequential lock was never taken", fp.discoverCalls)
		}

		good := bad
		good.Sections = nil
		if rec := fileWorkItemWithin(t, s, good, pvLockBound()); rec.Code != http.StatusCreated {
			t.Fatalf("same-prefix follow-up status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("capacity refusal on the derived path", func(t *testing.T) {
		fp := &pvChildProvider{guard: &pvGuard{t: t}}
		fp.children = numberedChildCorpus("94", 3)
		fp.childCap = 3
		registerFakeChildNumberProvider(t, &fp.fakeChildNumberProvider)
		workmgmt.Register(fp)
		fp.guard.fileOK.Store(true)
		s := New(Config{})

		req := workItemRequest{
			Repo: "kuhlman-labs/lock-cap", Type: "feature", Summary: "Over the cap",
			TitleVars: map[string]string{"epic": "94"}, Relations: &workItemRelations{ParentEpic: "#9401"},
		}
		if rec := fileWorkItemWithin(t, s, req, pvLockBound()); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want the 422 cap refusal (body=%s)", rec.Code, rec.Body.String())
		}
		fp.mu.Lock()
		fp.childCap = 0
		fp.mu.Unlock()
		if rec := fileWorkItemWithin(t, s, req, pvLockBound()); rec.Code != http.StatusCreated {
			t.Fatalf("same-epic follow-up status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

// pvPanicDiscoverProvider takes the child-number lock (EpicChildren) and then
// PANICS in DiscoverNumbers, for a custom type that is both child-numbered and
// sequentially numbered — the one way to reach a panic in prepareWorkItem's own
// frame with a lock already handed to it.
type pvPanicDiscoverProvider struct {
	pvChildProvider
}

func (p *pvPanicDiscoverProvider) DiscoverNumbers(context.Context, workmgmt.DiscoverNumbersRequest) ([]int, error) {
	panic("discovery exploded")
}

// TestPrepareWorkItem_PanicAfterLockSelfReleases (condition 1, panic arm): a
// panic unwinding prepareWorkItem after it was handed the child-number lock
// still releases it, so a same-epic filing completes. With the deferred
// self-release deleted, the follow-up times out.
func TestPrepareWorkItem_PanicAfterLockSelfReleases(t *testing.T) {
	// Copy the types map: Default() hands back the process-wide value, and a
	// write through its map would leak this type into every later test.
	conv := workmgmt.Default()
	types := make(map[string]workmgmt.ItemType, len(conv.Types)+1)
	for k, v := range conv.Types {
		types[k] = v
	}
	conv.Types = types
	conv.Types["numchild"] = workmgmt.ItemType{
		TitleFormat:  "[E{epic}.{n}] [X-{number}] {summary}",
		BodySkeleton: []string{"Summary"},
		Numbering:    &workmgmt.Numbering{Scheme: "sequential", Prefix: "X-"},
	}
	installConventions(t, conv, nil)

	// No existing children: the first child is {n}=1, so derivation succeeds
	// (and hands its lock over) without depending on how NextChildNumber
	// parses this custom title_format.
	fp := &pvPanicDiscoverProvider{pvChildProvider{guard: &pvGuard{t: t}}}
	fp.name = workmgmt.Default().Provider
	workmgmt.Register(fp)
	s := New(Config{})
	target := workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "lock-panic"}}
	filing := workmgmt.FilingRequest{
		Type: "numchild", Summary: "Panics after lock",
		TitleVars: map[string]string{"epic": "95"},
		Relations: workmgmt.Relations{ParentEpic: "#9501"},
	}

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Fatal("prepareWorkItem did not panic; the fixture is not exercising the panic path")
			}
		}()
		_, _, _ = s.prepareWorkItem(context.Background(), filing, conv, target, "kuhlman-labs", "lock-panic")
	}()
	if fp.epicCalls != 1 {
		t.Fatalf("EpicChildren called %d times, want 1 — the child-number lock was never taken", fp.epicCalls)
	}

	done := make(chan struct{})
	go func() {
		unlock := lockChildNumberKey(childNumberLockKey(target, "#9501"))
		unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(pvLockBound()):
		t.Fatal("the child-number lock is still held after prepareWorkItem panicked — the self-release did not run on the panic path")
	}
}

// TestPreviewWorkItem_LockTakingDraftReleasesLocks (condition 1b): a preview of
// a draft that TAKES a lock — an epic child with an auto-derived {n}, and a
// numbered type with discovered numbers — must release it, so a same-epic /
// same-prefix filing right after completes. With previewWorkItem's release()
// deleted, the follow-up filing times out.
func TestPreviewWorkItem_LockTakingDraftReleasesLocks(t *testing.T) {
	t.Run("epic child with derived n", func(t *testing.T) {
		fp := &pvChildProvider{guard: &pvGuard{t: t}}
		fp.children = []workmgmt.EpicChild{{Number: 501, Title: "[E96.1] first"}}
		registerFakeChildNumberProvider(t, &fp.fakeChildNumberProvider)
		workmgmt.Register(fp)
		s := New(Config{})

		draft := workItemRequest{
			Repo: "kuhlman-labs/preview-child", Type: "feature", Summary: "Derived child",
			TitleVars: map[string]string{"epic": "96"}, Relations: &workItemRelations{ParentEpic: "#9601"},
		}
		pv := decodeWorkItemPreview(t, previewWorkItemRec(t, s, draft, pvOperator()))
		if pv.Title != "[E96.2] Derived child" {
			t.Fatalf("preview title = %q, want the derived [E96.2] (the lock-taking path)", pv.Title)
		}
		if fp.fileCalls != 0 {
			t.Fatal("the preview reached provider.File")
		}
		fp.guard.fileOK.Store(true)
		rec := fileWorkItemWithin(t, s, draft, pvLockBound())
		if rec.Code != http.StatusCreated {
			t.Fatalf("same-epic filing status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("numbered type with discovery", func(t *testing.T) {
		fp := &pvSeqProvider{guard: &pvGuard{t: t}}
		fp.discovered = []int{79}
		registerFakeSequentialNumberProvider(t, &fp.fakeSequentialNumberProvider)
		workmgmt.Register(fp)
		s := New(Config{})

		draft := workItemRequest{Repo: "kuhlman-labs/preview-seq", Type: "adr", Summary: "Numbered preview"}
		pv := decodeWorkItemPreview(t, previewWorkItemRec(t, s, draft, pvOperator()))
		if pv.Number != 80 {
			t.Fatalf("preview number = %d, want 80 (the discovery path)", pv.Number)
		}
		if fp.fileCalls != 0 {
			t.Fatal("the preview reached provider.File")
		}
		fp.guard.fileOK.Store(true)
		rec := fileWorkItemWithin(t, s, draft, pvLockBound())
		if rec.Code != http.StatusCreated {
			t.Fatalf("same-prefix filing status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestPrepareWorkItem_ReleaseIsIdempotentAndSafeWithoutLocks: on success the
// release is non-nil, safe to call twice (sync.Once — a second raw unlock of a
// sync.Mutex is a fatal runtime error, not a recoverable panic) and safe when
// no lock was taken; and after it runs the lock is genuinely free.
func TestPrepareWorkItem_ReleaseIsIdempotentAndSafeWithoutLocks(t *testing.T) {
	fp := &pvChildProvider{guard: &pvGuard{t: t}}
	fp.children = []workmgmt.EpicChild{{Number: 501, Title: "[E97.1] first"}}
	registerFakeChildNumberProvider(t, &fp.fakeChildNumberProvider)
	workmgmt.Register(fp)
	s := New(Config{})
	conv := workmgmt.Default()
	target := workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "release-once"}}

	// No lock taken: an explicit n short-circuits derivation.
	_, release, werr := s.prepareWorkItem(context.Background(), workmgmt.FilingRequest{
		Type: "chore", Summary: "No lock", TitleVars: map[string]string{"epic": "97", "n": "4"},
	}, conv, target, "kuhlman-labs", "release-once")
	if werr != nil || release == nil {
		t.Fatalf("prepare = (release nil %v, %+v), want a non-nil release and no error", release == nil, werr)
	}
	release()
	release()

	// Lock taken: derived n.
	_, release, werr = s.prepareWorkItem(context.Background(), workmgmt.FilingRequest{
		Type: "feature", Summary: "Locked", TitleVars: map[string]string{"epic": "97"},
		Relations: workmgmt.Relations{ParentEpic: "#9701"},
	}, conv, target, "kuhlman-labs", "release-once")
	if werr != nil || release == nil {
		t.Fatalf("prepare = (release nil %v, %+v), want a non-nil release and no error", release == nil, werr)
	}
	release()
	release()
	done := make(chan struct{})
	go func() {
		lockChildNumberKey(childNumberLockKey(target, "#9701"))()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(pvLockBound()):
		t.Fatal("the child-number lock is still held after release()")
	}
}

// TestPreviewWorkItem_ConventionsRefusalMapsLikeFiling: a refusal from the
// shared core (an off-skeleton section) surfaces through the PREVIEW handler
// with the filing handler's exact status, code and details, and nothing is
// read from the tracker for intake (the refusal precedes the hook).
func TestPreviewWorkItem_ConventionsRefusalMapsLikeFiling(t *testing.T) {
	p := pvRegisterRead(t, pvHealthyItems())
	s := New(Config{})

	draft := pvStandardDraft()
	draft.Sections = map[string]string{"Summary": "s", "Impact": "off-skeleton"}
	rec := previewWorkItemRec(t, s, draft, pvOperator())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	e := pvDecodeErr(t, rec)
	if e.Code != "work_item_invalid" {
		t.Errorf("code = %q, want work_item_invalid", e.Code)
	}
	if unknown, _ := e.Details["unknown_sections"].([]any); !containsStr(unknown, "Impact") {
		t.Errorf("details.unknown_sections = %v, want it to include Impact", e.Details["unknown_sections"])
	}
	if n := p.lists(); n != 0 || p.called {
		t.Errorf("tracker read %d times / provider called %v, want neither", n, p.called)
	}
}
