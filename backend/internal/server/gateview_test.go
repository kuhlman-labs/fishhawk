package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gateViewLongNote is deliberately longer than the MCP compaction levers'
// 96-byte auditPayloadStringCap so a byte-identical round-trip proves the new
// surface elides nothing.
const gateViewLongNote = "The reviewer's full concern prose is intentionally longer than ninety-six bytes so a truncation or elision no-op would visibly change the round-tripped note text here."

// gateViewServer wires a Server with the run, audit, and concern fakes the
// gate-view handler reads.
func gateViewServer(t *testing.T) (*Server, *fakeRepo, *auditFake, *fakeConcernRepo) {
	t.Helper()
	repo := newFakeRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr})
	return s, repo, au, cr
}

// seedGateRun creates a run so GetRun resolves (an unknown run is the 404 case).
func seedGateRun(t *testing.T, repo *fakeRepo) uuid.UUID {
	t.Helper()
	got, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "s", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return got.ID
}

// seedGateConcern inserts one concern row with the given fields, returning it so
// callers can key audit joins on its stable ID (and mutate State/StateReason to
// model a settled or reopened row).
func seedGateConcern(t *testing.T, cr *fakeConcernRepo, runID, stageID uuid.UUID, stageKind, model string, seq int64, sev, cat, note, patch string) *concern.Concern {
	t.Helper()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            stageKind,
		ReviewerModel:        model,
		OriginReviewSequence: seq,
		Concerns:             []concern.RaisedConcern{{Severity: sev, Category: cat, Note: note, SuggestedPatch: patch}},
	})
	if err != nil {
		t.Fatalf("seed concern: %v", err)
	}
	return rows[0]
}

// gateViewReadIdentity is an operator token carrying the read scope the
// gate-view read requires (#1960). getGateView injects it by default so the
// behavioral tests exercise an authorized caller; the auth-guard tests inject
// their own identities.
func gateViewReadIdentity() Identity {
	return Identity{Subject: "github:op", TokenID: "tok-op", Scopes: []string{scopeGateViewRead}}
}

// getGateView drives handleGetRunGateView directly (not through s.Handler()):
// the auth middleware re-derives identity from the request and would clobber an
// injected context identity, so the default authorized-caller identity is
// injected and the handler invoked directly, mirroring TestGateView_CrossRunGuard.
func getGateView(t *testing.T, s *Server, runID uuid.UUID, query string) *httptest.ResponseRecorder {
	t.Helper()
	return callGateView(s, runID, query, gateViewReadIdentity())
}

// callGateView invokes the handler directly with the given identity, setting the
// run_id path value the mux would otherwise supply.
func callGateView(s *Server, runID uuid.UUID, query string, id Identity) *httptest.ResponseRecorder {
	path := "/v0/runs/" + runID.String() + "/gate-view"
	if query != "" {
		path += "?" + query
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("run_id", runID.String())
	req = injectIdentity(req, id)
	s.handleGetRunGateView(w, req)
	return w
}

func decodeGateView(t *testing.T, w *httptest.ResponseRecorder) gateViewResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp gateViewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode gate-view: %v\n%s", err, w.Body.String())
	}
	return resp
}

// --- failure modes -------------------------------------------------------

func TestGateView_ConcernRepoUnconfigured_503(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo}) // no ConcernRepo
	runID := seedGateRun(t, repo)
	w := getGateView(t, s, runID, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bodyHasCode(w, "gate_view_unconfigured") {
		t.Errorf("want gate_view_unconfigured code, got %s", w.Body.String())
	}
}

func TestGateView_UnknownRun_404(t *testing.T) {
	s, _, _, _ := gateViewServer(t)
	w := getGateView(t, s, uuid.New(), "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bodyHasCode(w, "run_not_found") {
		t.Errorf("want run_not_found code, got %s", w.Body.String())
	}
}

func TestGateView_InvalidStageKind_400(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	w := getGateView(t, s, runID, "stage_kind=deploy")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bodyHasCode(w, "validation_failed") {
		t.Errorf("want validation_failed code, got %s", w.Body.String())
	}
}

// TestGateView_CrossRunGuard mirrors handleFixupStage: an mcp:run:<uuid> token
// may only read its own run's gate view; a matching subject passes.
func TestGateView_CrossRunGuard(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)

	call := func(subject string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String()+"/gate-view", nil)
		req.SetPathValue("run_id", runID.String())
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: subject}))
		s.handleGetRunGateView(w, req)
		return w
	}

	// A token bound to a DIFFERENT run is refused.
	if w := call("mcp:run:" + uuid.New().String()); w.Code != http.StatusForbidden || !bodyHasCode(w, "cross_run_gate_view") {
		t.Fatalf("cross-run: status = %d body = %s, want 403 cross_run_gate_view", w.Code, w.Body.String())
	}
	// A token bound to THIS run passes.
	if w := call("mcp:run:" + runID.String()); w.Code != http.StatusOK {
		t.Fatalf("same-run: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// TestGateView_ReadScope enforces the read-scope posture (#1960 authz): a
// non-mcp caller must hold scopeGateViewRead. Anonymous -> 401, a token
// missing the scope -> 403, and a cookie-session operator (no scope list)
// passes. Full reviewer prose must not be anonymously readable.
func TestGateView_ReadScope(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 10, "high", "correctness", gateViewLongNote, "")

	// Anonymous -> 401.
	if w := callGateView(s, runID, "", anonIdentity()); w.Code != http.StatusUnauthorized || !bodyHasCode(w, "authentication_required") {
		t.Errorf("anonymous: status = %d body = %s, want 401 authentication_required", w.Code, w.Body.String())
	}
	// Authenticated token missing the read scope -> 403.
	noScope := Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"write:runs"}}
	if w := callGateView(s, runID, "", noScope); w.Code != http.StatusForbidden || !bodyHasCode(w, "insufficient_scope") {
		t.Errorf("missing-scope: status = %d body = %s, want 403 insufficient_scope", w.Code, w.Body.String())
	}
	// Cookie-session operator (no scope list) bypasses scope enforcement -> 200.
	cookie := Identity{Subject: "github:op", UserID: "u1", SessionID: "sess-1"}
	if w := callGateView(s, runID, "", cookie); w.Code != http.StatusOK {
		t.Errorf("cookie session: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// TestGateView_RunRepoUnconfigured_503 covers the RunRepo-nil guard that sits
// before the existence check: a configured ConcernRepo but no RunRepo -> 503.
func TestGateView_RunRepoUnconfigured_503(t *testing.T) {
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", ConcernRepo: cr}) // no RunRepo
	w := getGateView(t, s, uuid.New(), "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bodyHasCode(w, "run_repo_unconfigured") {
		t.Errorf("want run_repo_unconfigured code, got %s", w.Body.String())
	}
}

// TestGateView_ListByRunError_500 covers the ConcernRepo.ListByRun error branch
// -> 500 internal_error.
func TestGateView_ListByRunError_500(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	cr.listErr = errors.New("injected list-by-run error")
	w := getGateView(t, s, runID, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !bodyHasCode(w, "internal_error") {
		t.Errorf("want internal_error code, got %s", w.Body.String())
	}
}

// TestGateView_MalformedMCPSubject_401 covers a malformed mcp:run:<garbage>
// subject whose trailing text is not a UUID -> 401 authentication_required.
func TestGateView_MalformedMCPSubject_401(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	w := callGateView(s, runID, "", Identity{Subject: "mcp:run:not-a-uuid", TokenID: "tok-mcp", Scopes: []string{"mcp:read"}})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !bodyHasCode(w, "authentication_required") {
		t.Errorf("want authentication_required code, got %s", w.Body.String())
	}
}

func TestGateView_NilAuditRepo_HistoryIncomplete(t *testing.T) {
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: cr}) // no AuditRepo
	runID := seedGateRun(t, repo)
	seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "claude-opus-4-8", 10, "high", "correctness", gateViewLongNote, "")

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if !resp.HistoryIncomplete {
		t.Errorf("HistoryIncomplete = false, want true when AuditRepo is nil")
	}
	if len(resp.Open) != 1 {
		t.Fatalf("Open = %d, want 1 (concerns intact under degradation)", len(resp.Open))
	}
	// Every history category should be named as a gap.
	for _, cat := range gateViewHistoryCategories {
		if !containsString(resp.HistoryGaps, cat) {
			t.Errorf("HistoryGaps missing %q: %v", cat, resp.HistoryGaps)
		}
	}
}

// TestGateView_SingleCategoryError_HistoryGap injects an error on ONE category
// and asserts only that category is named in the gap while the others' joins
// stay intact.
func TestGateView_SingleCategoryError_HistoryGap(t *testing.T) {
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	base := newAuditFake()
	au := &oneCategoryErrAudit{auditFake: base, failCategory: "implement_reviewed"}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr})
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	c := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "claude-opus-4-8", 10, "high", "correctness", "note", "")
	// A trigger in a HEALTHY category still joins even though implement_reviewed errors.
	seedHeadEntry(base, runID, &stageID, CategoryStageFixupTriggered, 20, map[string]any{
		"concern_ids": []string{c.ID.String()}, "reason": "route it",
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if !resp.HistoryIncomplete {
		t.Errorf("HistoryIncomplete = false, want true")
	}
	if !containsString(resp.HistoryGaps, "implement_reviewed") {
		t.Errorf("HistoryGaps should name implement_reviewed: %v", resp.HistoryGaps)
	}
	if containsString(resp.HistoryGaps, CategoryStageFixupTriggered) {
		t.Errorf("HistoryGaps should NOT name the healthy stage_fixup_triggered: %v", resp.HistoryGaps)
	}
	if len(resp.Open) != 1 || len(resp.Open[0].Fixups) != 1 {
		t.Fatalf("healthy fixup join should survive; open=%+v", resp.Open)
	}
	if resp.Open[0].Fixups[0].Reason != "route it" {
		t.Errorf("fixup reason = %q, want %q", resp.Open[0].Fixups[0].Reason, "route it")
	}
}

// TestGateView_MalformedPayload_SkippedWarnOnly seeds one malformed trigger and
// one valid trigger for the same concern; the valid one still joins.
func TestGateView_MalformedPayload_SkippedWarnOnly(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	c := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 10, "high", "correctness", "note", "")
	// Malformed: concern_ids is a string, not an array -> unmarshal error, skipped.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &runID, StageID: &stageID, Category: CategoryStageFixupTriggered, Sequence: 20,
		Payload: json.RawMessage(`{"concern_ids": "not-an-array", "reason": "bad"}`),
	})
	// Valid sibling.
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 30, map[string]any{
		"concern_ids": []string{c.ID.String()}, "reason": "good",
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Open) != 1 || len(resp.Open[0].Fixups) != 1 {
		t.Fatalf("malformed entry should be skipped, sibling should join; open=%+v", resp.Open)
	}
	if resp.Open[0].Fixups[0].Reason != "good" {
		t.Errorf("fixup reason = %q, want %q", resp.Open[0].Fixups[0].Reason, "good")
	}
}

// --- behavioral done-means ----------------------------------------------

// TestGateView_FullNoteByteIdentical proves no elision: a >96-byte note round-
// trips byte-identical on both an OPEN and a SETTLED concern.
func TestGateView_FullNoteByteIdentical(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 10, "high", "correctness", gateViewLongNote, "")
	settled := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 11, "low", "style", gateViewLongNote, "")
	settled.State = concern.StateWaived
	settled.StateReason = "operator waived: not blocking"

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Open) != 1 || resp.Open[0].Note != gateViewLongNote {
		t.Fatalf("open note not byte-identical: %q", firstNote(resp.Open))
	}
	if len(resp.Settled) != 1 || resp.Settled[0].Note != gateViewLongNote {
		t.Fatalf("settled note not byte-identical: %+v", resp.Settled)
	}
}

// TestGateView_RoundBoundaryDerivation asserts concerns straddling a
// stage_fixup_triggered sequence get rounds 1 and 2 and a plan concern omits
// round. Round derivation COUNTS same-stage triggers below a sequence, so it is
// order-independent — the load-bearing defensive-sort coverage (observable
// fixup ordering) lives in TestGateView_FixupJoin, not here; the descending
// seed order below only documents that round counting tolerates any order.
func TestGateView_RoundBoundaryDerivation(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()

	before := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 10, "high", "correctness", "raised before any fixup", "")
	after := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 30, "high", "correctness", "raised after one fixup", "")
	planC := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindPlan, "m", 15, "medium", "scope", "plan concern", "")

	// Seed triggers in DESCENDING (shuffled) order to prove the defensive sort:
	// two same-stage triggers below `after`'s origin sequence.
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 25, map[string]any{"concern_ids": []string{}, "reason": "second-ish"})
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 20, map[string]any{"concern_ids": []string{}, "reason": "first"})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	byID := indexOpen(resp.Open)
	if got := byID[before.ID.String()].Round; got != 1 {
		t.Errorf("before-fixup concern round = %d, want 1", got)
	}
	// `after` (seq 30) sits above BOTH triggers (20, 25) -> round 3.
	if got := byID[after.ID.String()].Round; got != 3 {
		t.Errorf("after-fixup concern round = %d, want 3 (two triggers below its origin seq)", got)
	}
	if got := byID[planC.ID.String()].Round; got != 0 {
		t.Errorf("plan concern round = %d, want 0 (omitted)", got)
	}
	// The plan concern must not carry a round key on the wire.
	if bytesContains(t, resp, planC.ID.String(), `"round"`) {
		t.Errorf("plan concern should omit the round field on the wire")
	}
}

// TestGateView_FixupJoin covers all three outcomes: pushed (apply_path+head_sha),
// no_changes, and pending (a trigger with no following outcome). The three
// triggers are seeded in SHUFFLED (non-ascending) order so the assertions are
// load-bearing for the defensive Sequence sort: without it the fixups would
// emerge in repo/seed order and the positional + ascending-order checks fail.
func TestGateView_FixupJoin(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	c := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 5, "high", "correctness", "note", "")
	id := c.ID.String()

	// Triggers seeded 60 -> 20 -> 40 (shuffled). The outcome entries sit
	// between them by Sequence; earliestOutcomeAfter must still pair each
	// trigger with its own following outcome once the entries are sorted.
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 60, map[string]any{"concern_ids": []string{id}, "reason": "pass3"})
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 20, map[string]any{"concern_ids": []string{id}, "reason": "pass1"})
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 40, map[string]any{"concern_ids": []string{id}, "reason": "pass2"})
	seedHeadEntry(au, runID, &stageID, "fixup_no_changes", 45, map[string]any{})
	seedHeadEntry(au, runID, &stageID, "fixup_pushed", 25, map[string]any{"head_sha": "deadbeef", "apply_path": "applied"})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Open) != 1 {
		t.Fatalf("Open = %d, want 1", len(resp.Open))
	}
	fx := resp.Open[0].Fixups
	if len(fx) != 3 {
		t.Fatalf("Fixups = %d, want 3: %+v", len(fx), fx)
	}
	// Defensive-sort load-bearing check: the fixups must surface in ascending
	// Sequence order regardless of the shuffled seed order above.
	if fx[0].Sequence != 20 || fx[1].Sequence != 40 || fx[2].Sequence != 60 {
		t.Fatalf("fixups not in ascending Sequence order (defensive sort dropped?): %d, %d, %d",
			fx[0].Sequence, fx[1].Sequence, fx[2].Sequence)
	}
	if fx[0].Outcome != "pushed" || fx[0].ApplyPath != "applied" || fx[0].HeadSHA != "deadbeef" {
		t.Errorf("fixup[0] = %+v, want pushed/applied/deadbeef", fx[0])
	}
	if fx[1].Outcome != "no_changes" {
		t.Errorf("fixup[1] outcome = %q, want no_changes", fx[1].Outcome)
	}
	if fx[2].Outcome != "pending" {
		t.Errorf("fixup[2] outcome = %q, want pending", fx[2].Outcome)
	}
}

// TestGateView_NilStageLegacyJoin covers the legacy nil-stage-id join
// (sameStage's nil-nil match inside earliestOutcomeAfter, gateview.go:521) and
// gateViewRound's nil-stageID trigger skip (gateview.go:488): an audit entry
// recorded before stage ids were threaded onto fix-up audit entries carries a
// nil StageID, and neither derivation may silently drop it (the join) or
// silently fold it into an unrelated concrete-stage concern's round count
// (the skip).
func TestGateView_NilStageLegacyJoin(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)

	// (a) nil-nil outcome join: a trigger and its outcome both recorded with no
	// stage id (a legacy entry) must still pair up via sameStage's nil-nil match.
	legacy := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 5, "high", "correctness", "legacy note", "")
	seedHeadEntry(au, runID, nil, CategoryStageFixupTriggered, 20, map[string]any{
		"concern_ids": []string{legacy.ID.String()}, "reason": "legacy pass",
	})
	seedHeadEntry(au, runID, nil, "fixup_pushed", 25, map[string]any{
		"head_sha": "legacyhead", "apply_path": "legacyapplied",
	})

	// (b) round-skip: a nil-stageID trigger below a CONCRETE-stage concern's
	// origin sequence must not count toward that concern's round (gateViewRound
	// only counts SAME-stage triggers; nil never matches a concrete stage id).
	stageID := uuid.New()
	roundConcern := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 50, "high", "correctness", "round note", "")
	seedHeadEntry(au, runID, nil, CategoryStageFixupTriggered, 10, map[string]any{
		"concern_ids": []string{}, "reason": "unrelated legacy trigger",
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	byID := indexOpen(resp.Open)

	lc := byID[legacy.ID.String()]
	if len(lc.Fixups) != 1 {
		t.Fatalf("legacy concern Fixups = %d, want 1: %+v", len(lc.Fixups), lc.Fixups)
	}
	if lc.Fixups[0].Outcome != "pushed" || lc.Fixups[0].HeadSHA != "legacyhead" || lc.Fixups[0].ApplyPath != "legacyapplied" {
		t.Errorf("legacy nil-stage fixup did not join its nil-stage outcome: %+v", lc.Fixups[0])
	}

	if got := byID[roundConcern.ID.String()].Round; got != 1 {
		t.Errorf("round-skip concern Round = %d, want 1 (nil-stageID trigger must not bump an unrelated concrete stage's round)", got)
	}
}

// TestGateView_ResolutionJoin_StateReasonOverwrite proves the original fix-up
// routing reason still surfaces from the audit join even though the concern's
// state_reason was overwritten with the re-review note.
func TestGateView_ResolutionJoin_StateReasonOverwrite(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	c := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 10, "high", "correctness", "note", "")
	// Model the overwrite: state_reason now holds the re-review note, not the
	// original routing reason.
	c.State = concern.StateReopened
	c.StateReason = "re-review: still not fixed"

	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 20, map[string]any{
		"concern_ids": []string{c.ID.String()}, "reason": "original routing reason",
	})
	seedHeadEntry(au, runID, &stageID, "implement_reviewed", 30, map[string]any{
		"concern_resolutions": []map[string]any{
			{"id": c.ID.String(), "resolution": "reopened", "note": "re-review note text"},
		},
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Open) != 1 {
		t.Fatalf("Open = %d, want 1", len(resp.Open))
	}
	oc := resp.Open[0]
	if oc.StateReason != "re-review: still not fixed" {
		t.Errorf("state_reason = %q, want the overwritten re-review reason", oc.StateReason)
	}
	if len(oc.Fixups) != 1 || oc.Fixups[0].Reason != "original routing reason" {
		t.Fatalf("original routing reason must survive from the audit join: %+v", oc.Fixups)
	}
	if len(oc.Resolutions) != 1 || oc.Resolutions[0].Resolution != "reopened" || oc.Resolutions[0].Note != "re-review note text" {
		t.Fatalf("resolution join wrong: %+v", oc.Resolutions)
	}
	if oc.Resolutions[0].Round != 2 {
		t.Errorf("resolution round = %d, want 2 (one trigger below the review seq)", oc.Resolutions[0].Round)
	}
}

// TestGateView_SettledSection carries all four settled states each with its
// state_reason and full note.
func TestGateView_SettledSection(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	states := map[concern.State]string{
		concern.StateAddressed:  "confirmed by re-review",
		concern.StateWaived:     "operator waived",
		concern.StateSuperseded: "overtaken by other change",
		concern.StateDeferred:   "filed follow-up #123",
	}
	seq := int64(1)
	for st, reason := range states {
		row := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", seq, "medium", "scope", gateViewLongNote, "")
		row.State = st
		row.StateReason = reason
		seq++
	}

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Settled) != 4 {
		t.Fatalf("Settled = %d, want 4: %+v", len(resp.Settled), resp.Settled)
	}
	seen := map[string]string{}
	for _, sc := range resp.Settled {
		seen[sc.State] = sc.StateReason
		if sc.Note != gateViewLongNote {
			t.Errorf("settled %s note not byte-identical", sc.State)
		}
	}
	for st, reason := range states {
		if seen[string(st)] != reason {
			t.Errorf("settled state %s: state_reason = %q, want %q", st, seen[string(st)], reason)
		}
	}
	if len(resp.Open) != 0 {
		t.Errorf("Open = %d, want 0 (all concerns settled)", len(resp.Open))
	}
}

// TestGateView_AddressedByConditionInSettledLedger pins the #1956 done-means
// surface: a plan-stage concern resolved to the terminal addressed_by_condition
// state renders in the settled ledger (with its lineage state_reason), NOT in
// the open section — so the merge gate sees it as settled and no hand waive is
// demanded.
func TestGateView_AddressedByConditionInSettledLedger(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	row := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindPlan, "claude-opus-4-8", 5, "high", "correctness", gateViewLongNote, "")
	const reason = "binding approval condition (approval sequence 42) confirmed delivered by implement review sequence 200"
	row.State = concern.StateAddressedByCondition
	row.StateReason = reason

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Open) != 0 {
		t.Errorf("Open = %d, want 0 (an addressed_by_condition concern is settled, not open)", len(resp.Open))
	}
	if len(resp.Settled) != 1 {
		t.Fatalf("Settled = %d, want 1: %+v", len(resp.Settled), resp.Settled)
	}
	sc := resp.Settled[0]
	if sc.State != string(concern.StateAddressedByCondition) {
		t.Errorf("settled state = %q, want addressed_by_condition", sc.State)
	}
	if sc.StateReason != reason {
		t.Errorf("settled state_reason = %q, want the lineage reason", sc.StateReason)
	}
	if sc.ID != row.ID {
		t.Errorf("settled id = %s, want %s", sc.ID, row.ID)
	}
}

// TestGateView_SuppressedRelitigations (binding condition 1) populates the
// suppressed_relitigations section from concern_relitigation_suppressed entries.
func TestGateView_SuppressedRelitigations(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	// A concern so the response is non-trivial; the suppression is run-level.
	seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 10, "high", "correctness", "note", "")
	seedHeadEntry(au, runID, nil, concernRelitigationSuppressedCategory, 40, map[string]any{
		"settled_ref":            "concern-abc",
		"settled_state":          "waived",
		"severity":               "medium",
		"category":               "style",
		"note":                   gateViewLongNote,
		"reviewer_model":         "gpt-5.5",
		"origin_review_sequence": 39,
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.SuppressedRelitigations) != 1 {
		t.Fatalf("SuppressedRelitigations = %d, want 1: %+v", len(resp.SuppressedRelitigations), resp.SuppressedRelitigations)
	}
	sr := resp.SuppressedRelitigations[0]
	if sr.SettledRef != "concern-abc" || sr.SettledState != "waived" || sr.ReviewerModel != "gpt-5.5" || sr.OriginReviewSequence != 39 {
		t.Errorf("suppressed relitigation fields wrong: %+v", sr)
	}
	if sr.Note != gateViewLongNote {
		t.Errorf("suppressed note not byte-identical")
	}
	if resp.HistoryIncomplete {
		t.Errorf("HistoryIncomplete = true, want false (all categories readable)")
	}
}

// TestGateView_StageKindFilterScoping (binding condition 1) scopes the concerns
// to a single stage kind and echoes the filter.
func TestGateView_StageKindFilterScoping(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	planC := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindPlan, "m", 10, "medium", "scope", "plan concern", "")
	implC := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 20, "high", "correctness", "implement concern", "")

	resp := decodeGateView(t, getGateView(t, s, runID, "stage_kind=implement"))
	if resp.StageKind != "implement" {
		t.Errorf("StageKind echo = %q, want implement", resp.StageKind)
	}
	if len(resp.Open) != 1 || resp.Open[0].ID.String() != implC.ID.String() {
		t.Fatalf("stage_kind=implement should scope to the implement concern only: %+v", resp.Open)
	}
	for _, oc := range resp.Open {
		if oc.ID.String() == planC.ID.String() {
			t.Errorf("plan concern leaked past the implement filter")
		}
	}
}

// --- helpers -------------------------------------------------------------

// oneCategoryErrAudit wraps auditFake to fail ListForRunByCategory for exactly
// one category, so the single-category degradation path is testable while the
// other categories' joins stay intact.
type oneCategoryErrAudit struct {
	*auditFake
	failCategory string
}

func (a *oneCategoryErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.failCategory {
		return nil, errors.New("injected list-by-category error for " + category)
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

func bodyHasCode(w *httptest.ResponseRecorder, code string) bool {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		return false
	}
	return env.Error.Code == code
}

func indexOpen(open []gateViewConcern) map[string]gateViewConcern {
	m := make(map[string]gateViewConcern, len(open))
	for _, c := range open {
		m[c.ID.String()] = c
	}
	return m
}

func firstNote(open []gateViewConcern) string {
	if len(open) == 0 {
		return "<none>"
	}
	return open[0].Note
}

// bytesContains re-marshals the response and reports whether the JSON object for
// the concern with the given id contains needle — used to assert an omitempty
// field is absent on the wire.
func bytesContains(t *testing.T, resp gateViewResponse, id, needle string) bool {
	t.Helper()
	for _, c := range resp.Open {
		if c.ID.String() != id {
			continue
		}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal concern: %v", err)
		}
		return strings.Contains(string(b), needle)
	}
	return false
}
