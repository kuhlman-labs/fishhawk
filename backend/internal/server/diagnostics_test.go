package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diagnostics"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

func newDiagServer(t *testing.T, stored *run.Run, stages []*run.Stage, af *scAuditFake) *Server {
	t.Helper()
	return newDiagServerWith(t, stored, stages, af, func(*Config) {})
}

// newDiagServerWith is newDiagServer plus a Config hook, so the wedge
// cases can wire (or deliberately NOT wire) the StageCheckRepo and
// CampaignRepo the wedge assembly reads.
func newDiagServerWith(t *testing.T, stored *run.Run, stages []*run.Stage, af *scAuditFake, tweak func(*Config)) *Server {
	t.Helper()
	cfg := Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   &statusCommentRunRepo{stored: stored, stages: stages},
		AuditRepo: af,
	}
	tweak(&cfg)
	return New(cfg)
}

// diagCampaignRepo is a campaign.Repository fake for the wedge reads.
// Only the two list methods the wedge assembly calls are overridden;
// BaseFake supplies the rest.
type diagCampaignRepo struct {
	campaign.BaseFake
	forRun      []*campaign.Item
	forRunErr   error
	forCampaign []*campaign.Item
	forCampErr  error
}

func (r *diagCampaignRepo) ListCampaignItemsForRun(context.Context, uuid.UUID) ([]*campaign.Item, error) {
	return r.forRun, r.forRunErr
}

func (r *diagCampaignRepo) ListCampaignItemsForCampaign(context.Context, uuid.UUID) ([]*campaign.Item, error) {
	return r.forCampaign, r.forCampErr
}

// diagWedgeFixture builds a run wedged in every shape at once: a review
// stage with a red required check, a campaign item in `failed` with one
// blocked dependent, and a slice_integration_conflict audit entry.
// diagWedgeSentinel is the free-text token diagWedgeFixture seeds into
// every free-text-bearing field of the wedged run (a stage FailureReason
// and the fan-in audit payload). It exists so a "must not leak" assertion
// against this fixture can actually FAIL: an assertion that the egress
// lacks a token the fixture never carried is evidence of nothing.
const diagWedgeSentinel = "SENSITIVE-WEDGE-FREE-TEXT"

func diagWedgeFixture(t *testing.T) (*run.Run, []*run.Stage, *scAuditFake, *stageCheckRepoFake, *diagCampaignRepo) {
	t.Helper()
	runID := uuid.New()
	reviewStageID := uuid.New()
	runRow := &run.Run{
		ID:          runID,
		Repo:        "kuhlman-labs/fishhawk",
		WorkflowID:  "feature_change",
		RunnerKind:  run.RunnerKindGitHubActions,
		State:       run.StateFailed,
		WorkflowSHA: "specsha",
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{
			Contexts: []string{"CI Pass", "CodeQL"},
			Sources:  []string{"branch_protection"},
		},
	}
	stages := []*run.Stage{
		{ID: uuid.New(), Sequence: 0, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		// The wedged review stage carries a free-text FailureReason
		// holding diagWedgeSentinel BY CONSTRUCTION. Nothing in the wedge
		// derivation reads it, which is exactly the point: every
		// consumer of this fixture that asserts the sentinel is ABSENT
		// from an egress is then asserting something real. Seeding it
		// only where the wedge code already looks would make those
		// assertions unfalsifiable (#1737 implement review).
		{ID: reviewStageID, Sequence: 1, Type: run.StageTypeReview, State: run.StageStateRunning,
			FailureReason: strPtr("merge refused: " + diagWedgeSentinel)},
	}
	af := &scAuditFake{allEntries: []*audit.Entry{
		{Sequence: 5, Category: "stage_dispatched"},
		// The conflict entry's PAYLOAD is the other place free text
		// plausibly rides in (it names branches and conflict detail);
		// the derivation reads the CATEGORY only.
		{Sequence: 6, Category: "slice_integration_conflict",
			Payload: []byte(`{"detail":"` + diagWedgeSentinel + `"}`)},
	}}

	checks := newStageCheckRepoFake()
	checks.seed(reviewStageID, &stagecheck.Check{Name: "CI Pass", State: stagecheck.StateFail})
	checks.seed(reviewStageID, &stagecheck.Check{Name: "CodeQL", State: stagecheck.StatePass})

	itemID := uuid.New()
	campaignID := uuid.New()
	camp := &diagCampaignRepo{
		forRun: []*campaign.Item{{
			ID: itemID, CampaignID: campaignID, IssueRef: "issue:1737",
			State: campaign.ItemStateFailed, RunID: &runID,
		}},
		forCampaign: []*campaign.Item{
			{ID: itemID, CampaignID: campaignID, IssueRef: "issue:1737", State: campaign.ItemStateFailed},
			// One blocked dependent...
			{ID: uuid.New(), CampaignID: campaignID, IssueRef: "issue:1738",
				State: campaign.ItemStateBlocked, DependsOn: []string{"issue:1737"}},
			// ...one blocked item that does NOT depend on this one, so
			// the count is dependents, not "every blocked sibling".
			{ID: uuid.New(), CampaignID: campaignID, IssueRef: "issue:1739",
				State: campaign.ItemStateBlocked, DependsOn: []string{"issue:9999"}},
			// ...and one pending dependent, which is not blocked.
			{ID: uuid.New(), CampaignID: campaignID, IssueRef: "issue:1740",
				State: campaign.ItemStatePending, DependsOn: []string{"issue:1737"}},
		},
	}
	return runRow, stages, af, checks, camp
}

// fetchDiagBundle drives GET /v0/runs/{id}/diagnostics and decodes it,
// failing the test on any non-200.
func fetchDiagBundle(t *testing.T, s *Server, runID uuid.UUID) diagnostics.DiagnosticBundle {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", runID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var b diagnostics.DiagnosticBundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return b
}

func TestGetRunDiagnostics_HappyPath(t *testing.T) {
	runID := uuid.New()
	failID := uuid.New()
	runRow := &run.Run{
		ID:          runID,
		Repo:        "kuhlman-labs/fishhawk",
		WorkflowID:  "feature_change",
		WorkflowSHA: "specsha123",
		RunnerKind:  run.RunnerKindLocal,
		State:       run.StateFailed,
	}
	stages := []*run.Stage{
		{ID: uuid.New(), Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{
			ID:              failID,
			Sequence:        1,
			Type:            run.StageTypeImplement,
			State:           run.StageStateFailed,
			FailureCategory: failureCat(run.FailureA),
			FailureReason:   strPtr("SENSITIVE free text that must not leak"),
		},
	}
	af := &scAuditFake{allEntries: []*audit.Entry{
		{Sequence: 100, Category: "stage_dispatched"},
		{Sequence: 101, StageID: &failID, Category: "agent_failed"},
	}}
	s := newDiagServer(t, runRow, stages, af)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", runID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	var b diagnostics.DiagnosticBundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.RunID != runID.String() {
		t.Errorf("run_id = %q", b.RunID)
	}
	if b.WorkflowSpecHash != "specsha123" || b.RunnerKind != run.RunnerKindLocal {
		t.Errorf("spec/runner = %q/%q", b.WorkflowSpecHash, b.RunnerKind)
	}
	if len(b.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(b.Stages))
	}
	if b.FailingStage == nil || b.FailingStage.FailureCategory != "A" {
		t.Fatalf("failing stage = %+v", b.FailingStage)
	}
	if b.FailingStage.FailureSurface != "agent_failed" {
		t.Errorf("failure_surface = %q, want agent_failed", b.FailingStage.FailureSurface)
	}
	if b.AuditSequenceRange == nil || b.AuditSequenceRange.Min != 100 || b.AuditSequenceRange.Max != 101 {
		t.Errorf("audit range = %+v", b.AuditSequenceRange)
	}
	// Versions are stamped from internal/version. In dev/test the values
	// are the literals; assert they are carried (non-empty), not the
	// specific value.
	if b.Versions.Fishhawkd.Version == "" {
		t.Errorf("fishhawkd version empty")
	}

	// The redaction boundary: no free text crosses by default.
	if strings.Contains(w.Body.String(), "SENSITIVE") || strings.Contains(w.Body.String(), "must not leak") {
		t.Errorf("bundle leaked free text: %s", w.Body.String())
	}
}

func TestGetRunDiagnostics_NotFound(t *testing.T) {
	s := newDiagServer(t, nil, nil, &scAuditFake{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", uuid.New()), nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetRunDiagnostics_BadUUID(t *testing.T) {
	s := newDiagServer(t, nil, nil, &scAuditFake{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/v0/runs/not-a-uuid/diagnostics", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetRunDiagnostics_NilRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &scAuditFake{}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", uuid.New()), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetRunDiagnostics_NilAuditRepo(t *testing.T) {
	runID := uuid.New()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &statusCommentRunRepo{stored: &run.Run{ID: runID}}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", runID), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- wedge context (#1737) ---

// TestGetRunDiagnostics_WedgeContext is the read-path proof: a run
// wedged in every shape at once surfaces all four wedge facts through
// the handler.
func TestGetRunDiagnostics_WedgeContext(t *testing.T) {
	runRow, stages, af, checks, camp := diagWedgeFixture(t)
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
		c.CampaignRepo = camp
	})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want populated")
	}
	wc := b.WedgeContext
	if got, want := strings.Join(wc.BlockingChecks, ","), "CI Pass"; got != want {
		t.Errorf("blocking_checks = %q, want %q (only the RED context)", got, want)
	}
	if wc.CampaignItemState != "failed" {
		t.Errorf("campaign_item_state = %q, want failed", wc.CampaignItemState)
	}
	if wc.BlockedDependents != 1 {
		t.Errorf("blocked_dependents = %d, want 1 (blocked siblings that DEPEND on this item)", wc.BlockedDependents)
	}
	if wc.IntegrateWaveError != "slice_integration_conflict" {
		t.Errorf("integrate_wave_error = %q", wc.IntegrateWaveError)
	}
}

// TestGetRunDiagnostics_NoWedgeShape_OmitsBlock is failure mode m1 at
// the handler: a run with no wedge shape carries no wedge_context key
// at all — the anti-noise half of the block.
func TestGetRunDiagnostics_NoWedgeShape_OmitsBlock(t *testing.T) {
	runID := uuid.New()
	runRow := &run.Run{ID: runID, WorkflowID: "feature_change", State: run.StateSucceeded}
	stages := []*run.Stage{{ID: uuid.New(), Sequence: 0, Type: run.StageTypeReview, State: run.StageStateSucceeded}}
	af := &scAuditFake{allEntries: []*audit.Entry{{Sequence: 1, Category: "stage_dispatched"}}}
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = newStageCheckRepoFake()
		c.CampaignRepo = &diagCampaignRepo{}
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", runID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "wedge_context") {
		t.Errorf("healthy run carries a wedge_context key: %s", w.Body.String())
	}
}

// TestGetRunDiagnostics_NilChecksSnapshot_NoBlockingChecks is failure
// mode m2: every local dogfood run has a nil RequiredChecksSnapshot
// (#2497), so BlockingChecks must be empty rather than over-claimed —
// even with a StageCheckRepo holding a red row for the review stage.
func TestGetRunDiagnostics_NilChecksSnapshot_NoBlockingChecks(t *testing.T) {
	runRow, stages, af, checks, _ := diagWedgeFixture(t)
	runRow.RequiredChecksSnapshot = nil
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
	})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want populated by the fan-in marker")
	}
	if len(b.WedgeContext.BlockingChecks) != 0 {
		t.Errorf("blocking_checks = %v, want empty on a nil snapshot", b.WedgeContext.BlockingChecks)
	}
}

// TestGetRunDiagnostics_UnwiredStageCheckRepo is failure mode m3: no
// StageCheckRepo means no check names and a still-200 read.
func TestGetRunDiagnostics_UnwiredStageCheckRepo(t *testing.T) {
	runRow, stages, af, _, _ := diagWedgeFixture(t)
	s := newDiagServerWith(t, runRow, stages, af, func(*Config) {})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want populated by the fan-in marker")
	}
	if len(b.WedgeContext.BlockingChecks) != 0 {
		t.Errorf("blocking_checks = %v, want empty with no StageCheckRepo", b.WedgeContext.BlockingChecks)
	}
}

// TestGetRunDiagnostics_NoReviewStage_NoBlockingChecks covers the
// review-stage lookup miss: a run that never reached review (all the
// decomposition-child runs) carries no check names and still reads 200.
func TestGetRunDiagnostics_NoReviewStage_NoBlockingChecks(t *testing.T) {
	runRow, _, af, checks, _ := diagWedgeFixture(t)
	stages := []*run.Stage{{ID: uuid.New(), Sequence: 0, Type: run.StageTypePlan, State: run.StageStateFailed}}
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
	})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want populated by the fan-in marker")
	}
	if len(b.WedgeContext.BlockingChecks) != 0 {
		t.Errorf("blocking_checks = %v, want empty with no review stage", b.WedgeContext.BlockingChecks)
	}
}

// TestGetRunDiagnostics_CampaignReadError_Still200 is failure mode m4:
// a ListCampaignItemsForRun error degrades the campaign facts and must
// NOT turn the read into a 500.
func TestGetRunDiagnostics_CampaignReadError_Still200(t *testing.T) {
	runRow, stages, af, checks, camp := diagWedgeFixture(t)
	camp.forRunErr = errors.New("campaign list blew up")
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
		c.CampaignRepo = camp
	})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil, want the non-campaign facts to survive")
	}
	if b.WedgeContext.CampaignItemState != "" || b.WedgeContext.BlockedDependents != 0 {
		t.Errorf("campaign facts survived a read error: %+v", b.WedgeContext)
	}
	// The rest of the wedge block still lands.
	if len(b.WedgeContext.BlockingChecks) != 1 {
		t.Errorf("blocking_checks = %v, want the red check to survive", b.WedgeContext.BlockingChecks)
	}
}

// TestGetRunDiagnostics_CampaignSiblingReadError_Still200 covers the
// SECOND campaign degrade branch, separate from m4: the item lookup
// succeeded but the sibling list failed, so the item state is carried
// and the dependent count is not.
func TestGetRunDiagnostics_CampaignSiblingReadError_Still200(t *testing.T) {
	runRow, stages, af, checks, camp := diagWedgeFixture(t)
	camp.forCampErr = errors.New("sibling list blew up")
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
		c.CampaignRepo = camp
	})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil")
	}
	if b.WedgeContext.CampaignItemState != "failed" {
		t.Errorf("campaign_item_state = %q, want failed", b.WedgeContext.CampaignItemState)
	}
	if b.WedgeContext.BlockedDependents != 0 {
		t.Errorf("blocked_dependents = %d, want 0 after a sibling read error", b.WedgeContext.BlockedDependents)
	}
}

// TestGetRunDiagnostics_NilCampaignRepo is failure mode m5: no
// CampaignRepo wired at all — no panic, no campaign facts, 200.
func TestGetRunDiagnostics_NilCampaignRepo(t *testing.T) {
	runRow, stages, af, checks, _ := diagWedgeFixture(t)
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
	})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil")
	}
	if b.WedgeContext.CampaignItemState != "" || b.WedgeContext.BlockedDependents != 0 {
		t.Errorf("campaign facts present with no CampaignRepo: %+v", b.WedgeContext)
	}
}

// TestGetRunDiagnostics_NoCampaignLinkage covers the empty-list degrade:
// a run that belongs to no campaign carries no campaign facts.
func TestGetRunDiagnostics_NoCampaignLinkage(t *testing.T) {
	runRow, stages, af, checks, _ := diagWedgeFixture(t)
	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
		c.CampaignRepo = &diagCampaignRepo{}
	})

	b := fetchDiagBundle(t, s, runRow.ID)
	if b.WedgeContext == nil {
		t.Fatal("wedge_context = nil")
	}
	if b.WedgeContext.CampaignItemState != "" {
		t.Errorf("campaign_item_state = %q, want empty with no linkage", b.WedgeContext.CampaignItemState)
	}
}

// TestGetRunDiagnostics_WedgeNeverCarriesFreeText is the redaction
// counterfactual at the handler. The sentinel is seeded BY CONSTRUCTION
// in the drive advance audit payload, the failing stage's FailureReason
// and the campaign item's state string; none of it may reach the wire.
//
// Counterfactual: bypass normalizeCampaignItemState in
// diagnostics.buildWedgeContext and this goes RED.
func TestGetRunDiagnostics_WedgeNeverCarriesFreeText(t *testing.T) {
	const sentinel = "SENTINEL_HANDLER_FREE_TEXT"

	runRow, stages, af, checks, camp := diagWedgeFixture(t)
	failID := uuid.New()
	stages = append(stages, &run.Stage{
		ID: failID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStateFailed,
		FailureCategory: failureCat(run.FailureB),
		FailureReason:   strPtr("cannot integrate slice: " + sentinel),
	})
	af.allEntries = append(af.allEntries,
		&audit.Entry{Sequence: 7, Category: "run_auto_advanced",
			Payload: []byte(`{"event":"` + sentinel + `"}`)})
	camp.forRun[0].State = campaign.ItemState("failed after " + sentinel)

	s := newDiagServerWith(t, runRow, stages, af, func(c *Config) {
		c.StageCheckRepo = checks
		c.CampaignRepo = camp
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", runRow.ID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), sentinel) {
		t.Errorf("diagnostics leaked free text: %s", w.Body.String())
	}
	var b diagnostics.DiagnosticBundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.WedgeContext == nil || b.WedgeContext.CampaignItemState != "" {
		t.Errorf("campaign_item_state survived an unrecognized state: %+v", b.WedgeContext)
	}
}

// TestCollectWedgeFacts_DefensiveNils covers the defensive branches the
// HTTP path cannot reach: a nil run row (the handler never passes one,
// but the sibling product-report caller shares this helper) and a
// campaign list whose entries are nil.
func TestCollectWedgeFacts_DefensiveNils(t *testing.T) {
	runRow, stages, _, checks, camp := diagWedgeFixture(t)
	s := New(Config{Addr: "127.0.0.1:0", StageCheckRepo: checks, CampaignRepo: camp})

	if w := s.collectWedgeFacts(context.Background(), nil, stages); w == nil ||
		len(w.BlockingChecks) != 0 || w.CampaignItemState != "" {
		t.Errorf("nil run row yielded facts: %+v", w)
	}

	// A nil first item is skipped rather than dereferenced.
	camp.forRun = []*campaign.Item{nil}
	if w := s.collectWedgeFacts(context.Background(), runRow, stages); w.CampaignItemState != "" {
		t.Errorf("nil campaign item yielded a state: %+v", w)
	}

	// A nil sibling is skipped rather than dereferenced.
	camp.forRun = []*campaign.Item{{ID: uuid.New(), IssueRef: "issue:1737", State: campaign.ItemStateFailed}}
	camp.forCampaign = []*campaign.Item{nil}
	if w := s.collectWedgeFacts(context.Background(), runRow, stages); w.BlockedDependents != 0 {
		t.Errorf("nil sibling counted: %+v", w)
	}
}
