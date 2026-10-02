package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Persona concern operator surfaces (E55.10 / #3755, slice 2). The ingest
// controls (persona_concern_ingest.go) stamp reviewer_role / quote_unverified /
// severity_clamped_from on the persisted concern row; these tests pin that the
// operator-facing surfaces CARRY them: the gate view, the run-status concern
// block, the fix-up routing payload (stage_fixup_triggered `concerns`) and the
// fix-up prompt line rendered from it.

const (
	surfacesPersonaNote  = "persona: the authorization check is bypassable"
	surfacesStandardNote = "standard: missing nil check on the response"
)

// TestPersonaConcernSurfaces_CrossBoundary is the CROSS-BOUNDARY test: it
// drives ONE implement-review round through the REAL loop (runImplementReviews)
// with a persona fake emitting a high concern whose quoted passage has one
// word altered, and a standard fake emitting a high unquoted concern, then
// follows both concerns across every layer —
//
//	verdict ingest → concern persistence (the concern fake)
//	→ GET /v0/runs/{run_id}/gate-view   (handleGetRunGateView)
//	→ GET /v0/runs/{run_id}             (handleGetRun, concerns block)
//	→ POST /v0/stages/{stage_id}/fixup  (handleFixupStage, concern_ids)
//	→ stage_fixup_triggered payload     (the routed concerns)
//	→ fix-up prompt lines               (resolveFixupConcerns)
//
// The persona concern must surface reviewer_role <persona>, quote_unverified,
// severity low and severity_clamped_from high everywhere; the standard concern
// in the SAME round surfaces reviewer_role standard, no quote marker, and the
// byte-identical "[severity/category] note" fix-up line.
//
// Counterfactuals (each run): drop the ReviewerRole/QuoteUnverified copies from
// gateViewOpenConcern — the gate-view assertions go RED; from
// buildRunConcernsPayload — the run-status assertions go RED; from
// resolveConcernsByID — the routed payload AND the prompt line go RED (the
// prompt renders from the routed payload); make fixupConcernLine ignore the
// role — the persona prompt line goes RED.
func TestPersonaConcernSurfaces_CrossBoundary(t *testing.T) {
	std := verdictFake(planreview.VerdictApproveWithConcerns, personaStdModel,
		planreview.Concern{Severity: planreview.SeverityHigh, Category: "correctness", Note: surfacesStandardNote})
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel,
		quotedConcern(surfacesPersonaNote, personaRemitAlteredQuote, personaRemitPath))
	s, _, runRow, implStage, _ := personaImplRun(t, personaSpec(personaSpecOpts{attachOn: "implement"}), std, persona, "codex/"+personaAgentModel)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, reviewInjectionDiff(), nil, "head-surfaces", nil)

	// Layer 1: persistence (the precondition every later layer reads).
	rows := rowsByNote(t, cr, runRow.ID)
	personaRow, stdRow := rows[surfacesPersonaNote], rows[surfacesStandardNote]
	if personaRow == nil || stdRow == nil {
		t.Fatalf("persisted rows = %+v, want both the persona and the standard concern", rows)
	}
	if personaRow.ReviewerRole != personaTestName || !personaRow.QuoteUnverified || personaRow.Severity != "low" || personaRow.SeverityClampedFrom != "high" {
		t.Fatalf("persona row = %+v, want role %s, quote_unverified, low clamped from high (ingest precondition)", personaRow, personaTestName)
	}
	if stdRow.ReviewerRole != concern.ReviewerRoleStandard || stdRow.QuoteUnverified || stdRow.Severity != "high" {
		t.Fatalf("standard row = %+v, want role standard, unmarked, high (ingest precondition)", stdRow)
	}

	// Layer 2: the gate view.
	gv := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
	gvByNote := map[string]gateViewConcern{}
	for _, c := range gv.Open {
		gvByNote[c.Note] = c
	}
	if c, ok := gvByNote[surfacesPersonaNote]; !ok || c.ReviewerRole != personaTestName || !c.QuoteUnverified || c.Severity != "low" || c.SeverityClampedFrom != "high" {
		t.Errorf("gate-view persona concern = %+v (present %v), want reviewer_role %s, quote_unverified, low, severity_clamped_from high", c, ok, personaTestName)
	}
	if c, ok := gvByNote[surfacesStandardNote]; !ok || c.ReviewerRole != concern.ReviewerRoleStandard || c.QuoteUnverified || c.SeverityClampedFrom != "" {
		t.Errorf("gate-view standard concern = %+v (present %v), want reviewer_role standard, no quote/clamp marker", c, ok)
	}

	// Layer 3: the run-status concerns block, read as RAW wire bytes so a
	// mistyped json tag cannot round-trip through the same Go struct.
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runRow.ID.String(), nil)
	req.SetPathValue("run_id", runRow.ID.String())
	w := httptest.NewRecorder()
	s.handleGetRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET run status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var status struct {
		Concerns *struct {
			Items []map[string]any `json:"items"`
		} `json:"concerns"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode run status: %v", err)
	}
	if status.Concerns == nil {
		t.Fatalf("run status carries no concerns block:\n%s", w.Body.String())
	}
	itemsByID := map[string]map[string]any{}
	for _, it := range status.Concerns.Items {
		id, _ := it["id"].(string)
		itemsByID[id] = it
	}
	if it := itemsByID[personaRow.ID.String()]; it["reviewer_role"] != personaTestName || it["quote_unverified"] != true || it["severity"] != "low" {
		t.Errorf("run-status persona item = %v, want reviewer_role %s, quote_unverified true, severity low", it, personaTestName)
	}
	if it := itemsByID[stdRow.ID.String()]; it["reviewer_role"] != concern.ReviewerRoleStandard {
		t.Errorf("run-status standard item = %v, want reviewer_role standard", it)
	} else if _, has := it["quote_unverified"]; has {
		t.Errorf("run-status standard item carries quote_unverified = %v, want the key ABSENT (omitempty)", it["quote_unverified"])
	}

	// Layer 4: fix-up routing by concern_ids. The fix-up server shares the SAME
	// concern store; its stage row carries the persona run's ids at the
	// implement gate so resolveConcernsByID's run/stage ownership check passes.
	fs, repo, fau, _ := fixupServerWithConcerns(t)
	fs.cfg.ConcernRepo = cr
	repo.seedRun(&run.Run{ID: runRow.ID, State: run.StateRunning})
	repo.mu.Lock()
	repo.stages[implStage.ID] = &run.Stage{
		ID: implStage.ID, RunID: runRow.ID, Sequence: 1, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateAwaitingApproval,
	}
	repo.mu.Unlock()
	fw := postFixup(t, fs, implStage.ID, fixupRequest{
		ConcernIDs: []string{personaRow.ID.String(), stdRow.ID.String()},
		Reason:     "address both reviewers' concerns",
	})
	if fw.Code != http.StatusOK {
		t.Fatalf("fixup status = %d, want 200:\n%s", fw.Code, fw.Body.String())
	}
	triggers := auditFakeEntries(fau, CategoryStageFixupTriggered)
	if len(triggers) != 1 {
		t.Fatalf("%s entries = %d, want 1", CategoryStageFixupTriggered, len(triggers))
	}
	var trig struct {
		Concerns []map[string]any `json:"concerns"`
	}
	if err := json.Unmarshal(triggers[0].Payload, &trig); err != nil {
		t.Fatalf("decode trigger payload: %v", err)
	}
	if len(trig.Concerns) != 2 {
		t.Fatalf("routed concerns = %v, want 2", trig.Concerns)
	}
	if c := trig.Concerns[0]; c["note"] != surfacesPersonaNote || c["reviewer_role"] != personaTestName || c["quote_unverified"] != true {
		t.Errorf("routed persona concern = %v, want reviewer_role %s + quote_unverified true", c, personaTestName)
	}
	if c := trig.Concerns[1]; c["note"] != surfacesStandardNote || c["reviewer_role"] != concern.ReviewerRoleStandard {
		t.Errorf("routed standard concern = %v, want reviewer_role standard", c)
	}

	// Layer 5: the fix-up prompt lines served from that trigger.
	lines := fs.resolveFixupConcerns(context.Background(), runRow.ID, implStage.ID)
	if len(lines) != 2 {
		t.Fatalf("fix-up prompt concerns = %+v, want 2", lines)
	}
	if want := "[low/security · persona " + personaTestName + " · quote unverified] " + surfacesPersonaNote; lines[0].Text != want {
		t.Errorf("persona fix-up line = %q, want %q", lines[0].Text, want)
	}
	if want := "[high/correctness] " + surfacesStandardNote; lines[1].Text != want {
		t.Errorf("standard fix-up line = %q, want the byte-identical pre-#3755 shape %q", lines[1].Text, want)
	}
}

// TestFixupConcernLine pins each branch of the fix-up prompt line renderer:
// a standard or unattributed (legacy) concern keeps the byte-identical
// "[severity/category] note" line, a persona concern names its persona, a
// whitespace-only role reads as unattributed, and the quote-unverified marker
// renders independently of the persona label.
func TestFixupConcernLine(t *testing.T) {
	base := planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "edited an out-of-scope file"}
	with := func(role string, unverified bool) planreview.Concern {
		c := base
		c.ReviewerRole, c.QuoteUnverified = role, unverified
		return c
	}
	for _, tc := range []struct {
		name string
		c    planreview.Concern
		want string
	}{
		{"legacy unattributed", with("", false), "[medium/scope] edited an out-of-scope file"},
		{"standard reviewer", with(concern.ReviewerRoleStandard, false), "[medium/scope] edited an out-of-scope file"},
		{"whitespace role reads as unattributed", with("  ", false), "[medium/scope] edited an out-of-scope file"},
		{"persona", with("security", false), "[medium/scope · persona security] edited an out-of-scope file"},
		{"persona with unverified quote", with("security", true), "[medium/scope · persona security · quote unverified] edited an out-of-scope file"},
		{"unverified quote without a persona", with("", true), "[medium/scope · quote unverified] edited an out-of-scope file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fixupConcernLine(tc.c); got != tc.want {
				t.Errorf("fixupConcernLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPersonaConcernSurfaces_SettledLedgerAndLegacyRows drives the gate-view
// handler over three rows and reads the response as RAW JSON (an absent key
// and an empty value are distinct states):
//
//   - a persona-attributed, quote-unverified, clamped row that was then WAIVED
//     must keep reviewer_role / quote_unverified / severity_clamped_from on its
//     settled-ledger row (the cross-boundary test above covers only open[]);
//   - a legacy row minted before migration 0092 (reviewer_role ”, unmarked),
//     open and settled, must serialize with NONE of the three keys, so its
//     payload is byte-identical to pre-#3755.
//
// The run-status item for the legacy row is held to the same absent-key rule.
//
// Counterfactual (run): drop the three marker copies from the settled-ledger
// construction in handleGetRunGateView — the waived persona row's keys vanish:
// RED.
func TestPersonaConcernSurfaces_SettledLedgerAndLegacyRows(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	ctx := context.Background()

	personaRows, err := cr.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: runID, StageID: stageID, StageKind: concern.StageKindImplement,
		ReviewerModel: personaAgentModel, ReviewerRole: personaTestName, OriginReviewSequence: 7,
		Concerns: []concern.RaisedConcern{{Severity: "low", Category: "security", Note: "settled persona",
			QuoteUnverified: true, SeverityClampedFrom: "high"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cr.ApplyResolution(ctx, personaRows[0].ID, concern.StateWaived, "operator waived"); err != nil {
		t.Fatal(err)
	}
	legacyOpen := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 8, "medium", "scope", "legacy open", "")
	legacySettled := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 9, "medium", "scope", "legacy settled", "")
	if _, err := cr.ApplyResolution(ctx, legacySettled.ID, concern.StateWaived, "operator waived"); err != nil {
		t.Fatal(err)
	}

	w := getGateView(t, s, runID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("gate-view status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var raw struct {
		Open    []map[string]any `json:"open"`
		Settled []map[string]any `json:"settled"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode gate-view: %v", err)
	}
	byNote := map[string]map[string]any{}
	for _, c := range append(raw.Open, raw.Settled...) {
		n, _ := c["note"].(string)
		byNote[n] = c
	}

	if c := byNote["settled persona"]; c["state"] != string(concern.StateWaived) || c["reviewer_role"] != personaTestName ||
		c["quote_unverified"] != true || c["severity_clamped_from"] != "high" {
		t.Errorf("settled persona ledger row = %v, want waived with reviewer_role %s, quote_unverified true, severity_clamped_from high", c, personaTestName)
	}
	for _, note := range []string{"legacy open", "legacy settled"} {
		c, ok := byNote[note]
		if !ok {
			t.Fatalf("gate view carries no %q row: %s", note, w.Body.String())
		}
		for _, key := range []string{"reviewer_role", "quote_unverified", "severity_clamped_from"} {
			if v, has := c[key]; has {
				t.Errorf("%s row carries %s = %v, want the key ABSENT for a legacy row", note, key, v)
			}
		}
	}

	item, err := json.Marshal(buildRunConcernsPayload([]*concern.Concern{legacyOpen}, nil).Items[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"reviewer_role"`, `"quote_unverified"`} {
		if strings.Contains(string(item), key) {
			t.Errorf("run-status legacy item carries %s, want the key absent: %s", key, item)
		}
	}
}
