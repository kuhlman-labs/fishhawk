package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// seedAcmeDeferRun seeds a running run on the non-Fishhawk repository
// acme/widgets (ADR-110) with the installation and PR URL the defer verb needs.
func seedAcmeDeferRun(repo *approvalRunRepo, runID uuid.UUID) {
	inst := int64(7788)
	prURL := "https://github.com/acme/widgets/pull/12"
	repo.seedRun(&run.Run{
		ID:             runID,
		Repo:           "acme/widgets",
		State:          run.StateRunning,
		InstallationID: &inst,
		PullRequestURL: &prURL,
	})
}

// tradeoffBody renders a JSON object of base plus judgement tradeoff with the
// given cost.
func tradeoffBody(t *testing.T, base map[string]any, cost string) string {
	t.Helper()
	body := map[string]any{"judgement": "tradeoff", "tradeoff_cost": cost}
	for k, v := range base {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(raw)
}

// assertTradeoffCostTooLong asserts the 400 validation_failed the cap returns:
// details {field: tradeoff_cost, rule: tradeoff_cost_too_long, bytes, max}.
func assertTradeoffCostTooLong(t *testing.T, w *httptest.ResponseRecorder, bytes int) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 tradeoff_cost_too_long:\n%s", w.Code, w.Body.String())
		return
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" || details["field"] != "tradeoff_cost" || details["rule"] != concern.JudgementRuleTradeoffCostTooLong {
		t.Errorf("error = %s %v, want validation_failed field=tradeoff_cost rule=%s", code, details, concern.JudgementRuleTradeoffCostTooLong)
	}
	if details["bytes"] != float64(bytes) || details["max"] != float64(maxOperatorConcernBytes) {
		t.Errorf("details bytes=%v max=%v, want %d / %d", details["bytes"], details["max"], bytes, maxOperatorConcernBytes)
	}
}

// TestTradeoffCostBound_RefusedOnEveryHumanVerb (E94.8 / #4841, the carried
// Phase-2 hardening; operator acceptance criterion
// tradeoff-cost-bounded-all-verbs): a tradeoff_cost over maxOperatorConcernBytes
// is refused 400 tradeoff_cost_too_long on waive, bulk waive, resolve and
// defer, with the concern unchanged, no verb audit row and, for defer, no work
// item filed. A tradeoff judgement with a non-blank cost satisfies every other
// judgement rule, so with the cap deleted each request COMMITS (C6).
func TestTradeoffCostBound_RefusedOnEveryHumanVerb(t *testing.T) {
	overCap := strings.Repeat("c", maxOperatorConcernBytes+1)
	t.Run("waive", func(t *testing.T) {
		s, au, cr := waiveServer(t)
		row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "cold start")
		w := postWaiveRaw(t, s, row.ID.String(), tradeoffBody(t, map[string]any{"reason": "accepted"}, overCap), withAuth)
		assertTradeoffCostTooLong(t, w, len(overCap))
		assertUnjudged(t, cr, concern.StateRaised, row.ID)
		assertNoAuditRows(t, au, CategoryConcernWaived, CategoryConcernWaiveFailed)
	})
	t.Run("bulk waive", func(t *testing.T) {
		s, au, cr := waiveServer(t)
		runID := uuid.New()
		a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
		b := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 2, "b")
		w := postBulkWaiveRaw(t, s, runID.String(), bulkWaiveBody(t, "accepted", map[string]any{"judgement": "tradeoff", "tradeoff_cost": overCap}, a.ID, b.ID), withAuth)
		assertTradeoffCostTooLong(t, w, len(overCap))
		assertUnjudged(t, cr, concern.StateRaised, a.ID, b.ID)
		assertNoAuditRows(t, au, CategoryConcernWaived, CategoryConcernWaiveFailed)
	})
	t.Run("resolve", func(t *testing.T) {
		s, au, cr := resolveServer(t)
		runID := uuid.New()
		row := seedResolvableConcern(t, cr, runID, "a")
		w := postResolve(t, s, runID.String(), resolveBody(t, "fixed with a cache", map[string]any{"judgement": "tradeoff", "tradeoff_cost": overCap}, row.ID))
		assertTradeoffCostTooLong(t, w, len(overCap))
		assertUnjudged(t, cr, concern.StateAddressedPending, row.ID)
		assertNoAuditRows(t, au, CategoryConcernResolvedWithEvidence, CategoryConcernResolveFailed)
	})
	t.Run("defer", func(t *testing.T) {
		s, repo, au, cr, fp := deferServer(t)
		runID := uuid.New()
		seedAcmeDeferRun(repo, runID)
		row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "retry loop has no backoff")
		w := postDeferRaw(t, s, row.ID.String(), tradeoffBody(t, map[string]any{"parent_epic": "#389", "n": "3"}, overCap), withAuth)
		assertTradeoffCostTooLong(t, w, len(overCap))
		if fp.called {
			t.Error("provider File was called; a refused tradeoff_cost must file no work item")
		}
		assertUnjudged(t, cr, concern.StateRaised, row.ID)
		assertNoAuditRows(t, au, CategoryConcernDeferred, CategoryConcernDeferFailed)
	})
}

// TestTradeoffCostBound_ExactlyMaxAccepted: the cap is inclusive, so a cost of
// exactly maxOperatorConcernBytes commits and is stored verbatim.
func TestTradeoffCostBound_ExactlyMaxAccepted(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "cold start")
	atCap := strings.Repeat("c", maxOperatorConcernBytes)
	w := postWaiveRaw(t, s, row.ID.String(), tradeoffBody(t, map[string]any{"reason": "accepted"}, atCap), withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 at exactly %d bytes:\n%s", w.Code, maxOperatorConcernBytes, w.Body.String())
	}
	if row.State != concern.StateWaived || row.Judgement != concern.JudgementTradeoff || row.TradeoffCost != atCap {
		t.Errorf("committed state %s judgement %q cost %d bytes, want waived / tradeoff / the %d-byte cost", row.State, row.Judgement, len(row.TradeoffCost), len(atCap))
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 1 {
		t.Errorf("concern_waived entries = %d, want 1", n)
	}
}
