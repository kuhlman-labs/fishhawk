package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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

// seedGateConcernWithEvidence seeds one concern carrying the #2353 evidence
// fields. A separate helper rather than two more positional parameters on the
// already-11-argument seedGateConcern, whose call sites are unrelated to this
// change.
func seedGateConcernWithEvidence(t *testing.T, cr *fakeConcernRepo, runID, stageID uuid.UUID,
	seq int64, note, evidence, settledRef string) *concern.Concern {
	t.Helper()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            concern.StageKindImplement,
		ReviewerModel:        "m",
		OriginReviewSequence: seq,
		Concerns: []concern.RaisedConcern{{
			Severity: "high", Category: "correctness", Note: note,
			NewEvidence: evidence, SettledRef: settledRef,
		}},
	})
	if err != nil {
		t.Fatalf("seed concern with evidence: %v", err)
	}
	return rows[0]
}

// TestGateView_RendersConcernEvidence is the done-means for the gate-view leg
// of #2353: fishhawk_get_gate_view is the surface the operator reads when
// deciding whether a concern is worth blocking on, and until now it rendered
// `note` alone, so a concern whose substance sat in new_evidence read as an
// unsupported assertion. Both the OPEN list and the SETTLED ledger must carry
// new_evidence AND settled_ref verbatim.
func TestGateView_RendersConcernEvidence(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	const openEvidence = "backend/internal/server/gateview.go:441 maps SuggestedPatch but not NewEvidence; the field is dropped at the render boundary"
	const settledEvidence = "reproduced on run 90e0ea6a: the concern minted with new_evidence='' despite the verdict carrying it"
	openRef := uuid.New().String()
	settledRef := uuid.New().String()

	seedGateConcernWithEvidence(t, cr, runID, uuid.New(), 10, "evidenced open concern", openEvidence, openRef)
	settled := seedGateConcernWithEvidence(t, cr, runID, uuid.New(), 11, "evidenced settled concern", settledEvidence, settledRef)
	settled.State = concern.StateWaived
	settled.StateReason = "operator waived"

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Open) != 1 {
		t.Fatalf("Open = %d, want 1: %+v", len(resp.Open), resp.Open)
	}
	if resp.Open[0].NewEvidence != openEvidence {
		t.Errorf("Open[0].new_evidence = %q, want %q verbatim", resp.Open[0].NewEvidence, openEvidence)
	}
	if resp.Open[0].SettledRef != openRef {
		t.Errorf("Open[0].settled_ref = %q, want %q", resp.Open[0].SettledRef, openRef)
	}
	if len(resp.Settled) != 1 {
		t.Fatalf("Settled = %d, want 1: %+v", len(resp.Settled), resp.Settled)
	}
	if resp.Settled[0].NewEvidence != settledEvidence {
		t.Errorf("Settled[0].new_evidence = %q, want %q verbatim", resp.Settled[0].NewEvidence, settledEvidence)
	}
	if resp.Settled[0].SettledRef != settledRef {
		t.Errorf("Settled[0].settled_ref = %q, want %q", resp.Settled[0].SettledRef, settledRef)
	}

	// The evidence must be in the raw JSON body under the wire names the MCP
	// client decodes — the struct-level assertions above would still pass with a
	// mistyped json tag, which is the seam that silently yields an empty field.
	body := getGateView(t, s, runID, "").Body.String()
	for _, want := range []string{`"new_evidence":"` + openEvidence + `"`, `"settled_ref":"` + openRef + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("gate-view body missing %s\nbody: %s", want, body)
		}
	}
}

// TestGateView_OmitsEmptyConcernEvidence is the suppression counterpart: a
// concern with neither field must omit BOTH keys entirely rather than emitting
// `"new_evidence":""`. An empty labelled field reads as "the reviewer supplied
// no evidence" when the truth is that this concern never had any, and it also
// keeps the payload byte-identical for the common no-evidence concern.
func TestGateView_OmitsEmptyConcernEvidence(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	seedGateConcernWithEvidence(t, cr, runID, uuid.New(), 10, "bare open concern", "", "")
	settled := seedGateConcernWithEvidence(t, cr, runID, uuid.New(), 11, "bare settled concern", "", "")
	settled.State = concern.StateWaived

	body := getGateView(t, s, runID, "").Body.String()
	for _, absent := range []string{"new_evidence", "settled_ref"} {
		if strings.Contains(body, absent) {
			t.Errorf("gate-view body carries %q for concerns with no evidence — omitempty is not applied\nbody: %s", absent, body)
		}
	}
}

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

// TestGateView_FixupJoin_RecoveredOutcome pins the #3395 third outcome: a
// trigger followed only by a stage_fixup_recovered entry reads "recovered"
// (before this it read "pending" forever), while a trigger followed by
// fixup_pushed and THEN stage_fixup_recovered still reads "pushed" because
// earliestOutcomeAfter picks the earliest following outcome. The recovered
// entry's payload is deliberately malformed to pin that nothing is decoded
// from it — a bad payload cannot hide a recovered pass.
func TestGateView_FixupJoin_RecoveredOutcome(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	c := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 5, "high", "correctness", "note", "")
	id := c.ID.String()

	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 20, map[string]any{"concern_ids": []string{id}, "reason": "pass1"})
	seedHeadEntry(au, runID, &stageID, "fixup_pushed", 25, map[string]any{"head_sha": "deadbeef", "apply_path": "agent"})
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupRecovered, 27, map[string]any{"delivered_nothing": false})
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 40, map[string]any{"concern_ids": []string{id}, "reason": "pass2"})
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &stageID, Category: CategoryStageFixupRecovered, Sequence: 45, Payload: []byte(`{not json`),
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(resp.Open) != 1 {
		t.Fatalf("Open = %d, want 1", len(resp.Open))
	}
	fx := resp.Open[0].Fixups
	if len(fx) != 2 {
		t.Fatalf("Fixups = %d, want 2: %+v", len(fx), fx)
	}
	if fx[0].Outcome != "pushed" || fx[0].HeadSHA != "deadbeef" {
		t.Errorf("fixup[0] = %+v, want pushed/deadbeef (a pushed-then-recovered pass reads pushed)", fx[0])
	}
	if fx[1].Outcome != "recovered" {
		t.Errorf("fixup[1] outcome = %q, want recovered", fx[1].Outcome)
	}
	if fx[1].HeadSHA != "" || fx[1].ApplyPath != "" {
		t.Errorf("fixup[1] = %+v, want no head_sha/apply_path on a recovered outcome", fx[1])
	}
	if resp.HistoryIncomplete {
		t.Errorf("history_incomplete = true, want false (a malformed recovered payload is not decoded, so it is not a gap)")
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
	// only counts SAME-stage triggers; nil never matches a concrete stage id). A
	// same-stage trigger at sequence 30 is seeded alongside it, both below the
	// concern's origin (50), so the assertion actually distinguishes the skip:
	// if the nil-stage trigger were wrongly counted the round would be 3 (not
	// 2), and if the same-stage trigger were wrongly SKIPPED the round would
	// stay 1 — either regression moves the observed value away from 2, so the
	// nil-stageID skip path is exercised rather than trivially unreachable.
	stageID := uuid.New()
	roundConcern := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "m", 50, "high", "correctness", "round note", "")
	seedHeadEntry(au, runID, nil, CategoryStageFixupTriggered, 10, map[string]any{
		"concern_ids": []string{}, "reason": "unrelated legacy trigger",
	})
	seedHeadEntry(au, runID, &stageID, CategoryStageFixupTriggered, 30, map[string]any{
		"concern_ids": []string{}, "reason": "same-stage trigger",
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

	if got := byID[roundConcern.ID.String()].Round; got != 2 {
		t.Errorf("round-skip concern Round = %d, want 2 (the same-stage trigger at seq 30 must count but the nil-stageID trigger at seq 10 must not)", got)
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

// TestGateView_OperatorConcern_RendersMintedConcern (#2623, binding condition 2)
// drives a REAL operator-concern-only fix-up through handleFixupStage, then reads
// the GATE VIEW consumer — not just the single-run concerns block — and asserts
// the minted operator concern renders in the OPEN section with its full verbatim
// note (no elision) and a fix-up join keyed off the trigger payload's concern_ids
// / operator_concern_id. A defect in the gate-view consumer that dropped the
// operator concern would pass a run-concerns-block-only test but fail here.
func TestGateView_OperatorConcern_RendersMintedConcern(t *testing.T) {
	// fixupServerWithConcerns wires the approvalRunRepo (which GetRun resolves for
	// the gate view's existence check) plus the audit + concern fakes the gate
	// view reads — the same *Server drives both the fix-up and the gate view.
	s, repo, _, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)

	const text = "CodeQL high alert: the gate view must surface this operator instruction as an OPEN concern until it is addressed."
	if w := postFixup(t, s, stage.ID, fixupRequest{OperatorConcern: text, Reason: "required CodeQL gate"}); w.Code != http.StatusOK {
		t.Fatalf("fix-up status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	minted := operatorConcernRows(cr)
	if len(minted) != 1 {
		t.Fatalf("operator concern rows = %d, want 1", len(minted))
	}

	resp := decodeGateView(t, callGateView(s, stage.RunID, "", gateViewReadIdentity()))
	if len(resp.Open) != 1 {
		t.Fatalf("gate-view Open = %d, want 1 (the minted operator concern must be visible)", len(resp.Open))
	}
	oc := resp.Open[0]
	if oc.ID != minted[0].ID {
		t.Errorf("gate-view open id = %s, want the minted id %s", oc.ID, minted[0].ID)
	}
	if oc.Category != operatorConcernCategory || oc.Severity != string(operatorConcernSeverity) {
		t.Errorf("gate-view open category/severity = %s/%s, want %s/%s", oc.Category, oc.Severity, operatorConcernCategory, operatorConcernSeverity)
	}
	if oc.State != string(concern.StateAddressedPending) {
		t.Errorf("gate-view open state = %q, want addressed_pending", oc.State)
	}
	if oc.Note != text {
		t.Errorf("gate-view open note = %q, want the full verbatim text (no elision)", oc.Note)
	}
	// The fix-up join proves the trigger payload's concern_ids / operator_concern_id
	// wiring reaches the gate-view consumer: the minted id matched a trigger.
	if len(oc.Fixups) != 1 {
		t.Fatalf("gate-view open Fixups = %d, want 1 (the operator concern's routing pass)", len(oc.Fixups))
	}
	if oc.Fixups[0].Reason != "required CodeQL gate" {
		t.Errorf("gate-view fixup reason = %q, want the operator's routing reason", oc.Fixups[0].Reason)
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

// --- live-validation walk surface (#2045, E48.35) ------------------------

// TestGateView_SurfacesLiveValidation: the gate view mirrors the run-status
// live-validation surface, so the gate view answers "is a live-validation walk
// still pending at this gate" in the one call.
func TestGateView_SurfacesLiveValidation(t *testing.T) {
	s, repo, au, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	seedLiveValidationMarker(au, runID, liveValidationWalkLinkedKind, liveValidationWalkMarker{
		Phase: "linked", PendingCriteriaCount: 2, CriterionIDs: []string{"ac1", "ac2"}, WalkRef: "#4300",
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if resp.LiveValidation == nil {
		t.Fatalf("live_validation = nil, want the pending walk")
	}
	if resp.LiveValidation.WalkRef != "#4300" || resp.LiveValidation.PendingCriteriaCount != 2 ||
		resp.LiveValidation.FilingFailed || resp.LiveValidation.FilingIncomplete {
		t.Errorf("live_validation = %+v, want count 2, walk #4300, healthy", resp.LiveValidation)
	}
}

// TestGateView_LiveValidation_StrandedIntent: a bare intent marker surfaces the
// file-manually incomplete variant on the gate view (binding condition A(1)).
func TestGateView_LiveValidation_StrandedIntent(t *testing.T) {
	s, repo, au, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	seedLiveValidationMarker(au, runID, liveValidationWalkIntentKind, liveValidationWalkMarker{
		Phase: "intent", PendingCriteriaCount: 1, CriterionIDs: []string{"ac1"},
	})

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if resp.LiveValidation == nil || !resp.LiveValidation.FilingFailed ||
		!resp.LiveValidation.FilingIncomplete || resp.LiveValidation.WalkRef != "" {
		t.Errorf("live_validation = %+v, want file-manually incomplete", resp.LiveValidation)
	}
}

// TestGateView_NoLiveValidation_OmitsBlock: no marker → the field is omitted.
func TestGateView_NoLiveValidation_OmitsBlock(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if resp.LiveValidation != nil {
		t.Errorf("live_validation = %+v, want nil when no marker landed", resp.LiveValidation)
	}
}

// --- disputes (E48.103 / #2551) ---------------------------------------------

// openConcernByID finds one open concern in a gate-view response.
func openConcernByID(t *testing.T, resp gateViewResponse, id uuid.UUID) gateViewConcern {
	t.Helper()
	for _, c := range resp.Open {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("concern %s is not in the open set (%d open, %d settled)", id, len(resp.Open), len(resp.Settled))
	return gateViewConcern{}
}

// runSplitRound drives the REAL implement-review loop for the observed
// split: the reviewer that RAISED the concern rejects, a DIFFERENT reviewer
// confirms it in the same round.
func runSplitRound(s *Server, runID, stageID uuid.UUID, concernID string) {
	raiser := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject}, model: "gpt-5.6-sol"}
	peer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:            planreview.VerdictApprove,
			ConcernResolutions: []planreview.ConcernResolution{{ID: concernID, Resolution: "confirmed", Note: "reads fixed to me"}},
		},
		model: "fable-5",
	}
	s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: raiser}, {reviewer: peer}},
		planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget, "", 0)
}

// TestGateView_SplitRound_RendersDispute is THE cross-layer test (E48.103 /
// #2551, operator condition 3): the same-round raiser-reject / peer-confirm
// split from run 9bba554d, driven through the REAL review loop into the REAL
// concern store and read back over the gate-view HTTP surface. The merge gate
// must show the concern OPEN and DISPUTED, naming both reviewers.
func TestGateView_SplitRound_RendersDispute(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", gateViewLongNote, "routed: fix the authz check")

	runSplitRound(s, runID, stageID, row.ID.String())

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	got := openConcernByID(t, resp, row.ID)
	if got.State != string(concern.StateAddressedPending) {
		t.Fatalf("state = %q, want addressed_pending (the peer confirm must not have settled it)", got.State)
	}
	if !got.Disputed {
		t.Error("disputed = false, want true (a split resolve/reject must be visible at the merge gate)")
	}
	if len(got.Disputes) != 1 {
		t.Fatalf("disputes = %+v, want exactly one", got.Disputes)
	}
	d := got.Disputes[0]
	if d.VetoReason != vetoRaiserRejectedSameRound {
		t.Errorf("veto_reason = %q, want %q", d.VetoReason, vetoRaiserRejectedSameRound)
	}
	if d.ConfirmingReviewerModel != "fable-5" || d.RaisingReviewerModel != "gpt-5.6-sol" {
		t.Errorf("dispute models = {confirming %q, raising %q}, want {fable-5, gpt-5.6-sol}", d.ConfirmingReviewerModel, d.RaisingReviewerModel)
	}
	if d.Resolution != "confirmed" {
		t.Errorf("resolution = %q, want confirmed", d.Resolution)
	}
}

// TestGateView_VetoAppendFailed_StillDisputed is operator condition 1: the
// concern_resolution_vetoed append is best-effort, so the dispute must NOT be
// visible only when that append happens to succeed. With the append FAILING,
// the gate view must still refuse to render a clean no-dispute view — the
// disputed flag is derived from the durable concern row (still open) plus the
// authoritative implement_reviewed payload that carries the confirmation.
func TestGateView_VetoAppendFailed_StillDisputed(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", gateViewLongNote, "routed: fix the authz check")

	// Fail ONLY the veto append; implement_reviewed still lands.
	au.appendErrCategory = concernResolutionVetoedCategory
	runSplitRound(s, runID, stageID, row.ID.String())

	au.mu.Lock()
	for _, ap := range au.appended {
		if ap.Category == concernResolutionVetoedCategory {
			au.mu.Unlock()
			t.Fatal("a concern_resolution_vetoed entry landed; the injected append failure did not take effect")
		}
	}
	au.mu.Unlock()

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	got := openConcernByID(t, resp, row.ID)
	if got.State != string(concern.StateAddressedPending) {
		t.Fatalf("state = %q, want addressed_pending (the veto stands even when its bookkeeping fails)", got.State)
	}
	if !got.Disputed {
		t.Error("disputed = false with the veto append failed, want true (the gate must not report a confident absence of dispute)")
	}
	if len(got.Disputes) != 0 {
		t.Errorf("disputes = %+v, want none (the enrichment entry never landed)", got.Disputes)
	}
}

// TestGateView_VetoCategoryReadError_DegradesVisibly: an unreadable
// concern_resolution_vetoed category names itself in history_gaps with
// history_incomplete=true, and the derived dispute survives the gap.
func TestGateView_VetoCategoryReadError_DegradesVisibly(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", gateViewLongNote, "routed")

	runSplitRound(s, runID, stageID, row.ID.String())
	s.cfg.AuditRepo = &categoryErrAuditRepo{
		auditFake:   au,
		errCategory: concernResolutionVetoedCategory,
		err:         errors.New("audit store unavailable"),
	}

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if !resp.HistoryIncomplete {
		t.Error("history_incomplete = false, want true (an unreadable dispute category is a visible gap)")
	}
	var named bool
	for _, g := range resp.HistoryGaps {
		if g == concernResolutionVetoedCategory {
			named = true
		}
	}
	if !named {
		t.Errorf("history_gaps = %v, want it to name %s", resp.HistoryGaps, concernResolutionVetoedCategory)
	}
	if got := openConcernByID(t, resp, row.ID); !got.Disputed {
		t.Error("disputed = false, want true (the derived dispute survives an unreadable enrichment category)")
	}
}

// TestGateView_UndisputedConcern_NotDisputed is the negative control: an open
// concern with no recorded confirmation renders disputed=false with no
// disputes, so the flag discriminates.
func TestGateView_UndisputedConcern_NotDisputed(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", gateViewLongNote, "routed")

	// Same round shape, but the peer reopens instead of confirming.
	peer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:            planreview.VerdictReject,
			ConcernResolutions: []planreview.ConcernResolution{{ID: row.ID.String(), Resolution: "reopened", Note: "still broken"}},
		},
		model: "fable-5",
	}
	s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: peer}},
		planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget, "", 0)

	got := openConcernByID(t, decodeGateView(t, getGateView(t, s, runID, "")), row.ID)
	if got.Disputed || len(got.Disputes) != 0 {
		t.Errorf("disputed = %v disputes = %+v, want a clean undisputed render", got.Disputed, got.Disputes)
	}
}

// TestGateView_BlankNoteConcernRendersPointer: the gate view is the surface an
// operator decides from, and it renders each open concern's FULL note. A LEGACY
// blank-note row — seeded by construction, as such rows already sit in the
// store — must render a non-blank stand-in naming its originating review, not
// an empty string whose only available disposition is a waive of unknown
// content (#2555).
func TestGateView_BlankNoteConcernRendersPointer(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	blank := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "gpt-5-codex", 88,
		"medium", "coverage", "", "")
	authored := seedGateConcern(t, cr, runID, stageID, concern.StageKindImplement, "gpt-5-codex", 89,
		"low", "style", "the helper shadows the package-level logger", "")

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	byID := map[uuid.UUID]string{}
	for _, c := range resp.Open {
		byID[c.ID] = c.Note
	}
	got := byID[blank.ID]
	if strings.TrimSpace(got) == "" {
		t.Fatal("gate view rendered an EMPTY note for a blank-note open concern")
	}
	for _, want := range []string{concern.MissingNoteMarker, "88", "gpt-5-codex"} {
		if !strings.Contains(got, want) {
			t.Errorf("gate-view note = %q, want it to name %q", got, want)
		}
	}
	// The counterfactual half: an authored note is rendered verbatim, so the
	// chokepoint cannot be satisfied by decorating every note.
	if byID[authored.ID] != "the helper shadows the package-level logger" {
		t.Errorf("authored note = %q, want it rendered verbatim", byID[authored.ID])
	}
}

// --- diff-truncation surface (#2875) ------------------------------------

// seedReviewDiffTruncatedMarker appends an implement_review_diff_truncated audit
// entry (or a raw payload when rawPayload != nil, for the undecodable case).
func seedReviewDiffTruncatedMarker(au *auditFake, runID uuid.UUID, p reviewDiffTruncatedPayload, rawPayload []byte) {
	payload := rawPayload
	if payload == nil {
		payload, _ = json.Marshal(p)
	}
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Category: reviewDiffTruncatedCategory, Payload: payload, Timestamp: time.Now().UTC(),
	})
}

// TestBuildGateView_ReviewDiffTruncated_Present: an emitted entry surfaces as
// review_diff_truncated with reason + the COMPLETE omitted list, and the NEWEST
// entry wins across two rounds. COUNTERFACTUAL: delete the reviewDiffTruncatedForRun
// call from buildGateView → the field is nil and this goes RED.
func TestBuildGateView_ReviewDiffTruncated_Present(t *testing.T) {
	s, repo, au, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	// Round 1 (older), then round 2 (newer) wins.
	seedReviewDiffTruncatedMarker(au, runID, reviewDiffTruncatedPayload{
		Reason: "runner_patch_cap", ChangedFileCount: 3, OmittedFileCount: 1,
		OmittedFiles: []string{"old.go (no hunks shown)"},
	}, nil)
	seedReviewDiffTruncatedMarker(au, runID, reviewDiffTruncatedPayload{
		Reason: "GitHub capped compare", ChangedFileCount: 9, OmittedFileCount: 2,
		OmittedFiles:         []string{"a.go (no hunks shown)", "b.go (may be cut — its tail may be missing)"},
		OmittedFilesResidual: 5, DeltaReReview: true, BestEffort: true,
	}, nil)

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	rdt := resp.ReviewDiffTruncated
	if rdt == nil {
		t.Fatal("review_diff_truncated = nil, want the newest entry")
	}
	if rdt.Reason != "GitHub capped compare" || rdt.ChangedFileCount != 9 || rdt.OmittedFileCount != 2 {
		t.Errorf("review_diff_truncated = %+v, want the newest (round 2) entry", rdt)
	}
	if len(rdt.OmittedFiles) != 2 || !rdt.BestEffort || !rdt.DeltaReReview || rdt.OmittedFilesResidual != 5 {
		t.Errorf("review_diff_truncated fields = %+v, want the complete list + residual 5 + best-effort + delta", rdt)
	}
}

// TestBuildGateView_ReviewDiffTruncated_Degrades: each degrade yields nil, never
// a partially-populated field and never a failed gate view.
func TestBuildGateView_ReviewDiffTruncated_Degrades(t *testing.T) {
	t.Run("nil AuditRepo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if got := s.reviewDiffTruncatedForRun(context.Background(), uuid.New()); got != nil {
			t.Errorf("nil AuditRepo must yield nil; got %+v", got)
		}
	})
	t.Run("list error", func(t *testing.T) {
		s, repo, base, _ := gateViewServer(t)
		s.cfg.AuditRepo = &oneCategoryErrAudit{auditFake: base, failCategory: reviewDiffTruncatedCategory}
		runID := seedGateRun(t, repo)
		if got := s.reviewDiffTruncatedForRun(context.Background(), runID); got != nil {
			t.Errorf("list error must yield nil; got %+v", got)
		}
	})
	t.Run("no entry", func(t *testing.T) {
		s, repo, _, _ := gateViewServer(t)
		runID := seedGateRun(t, repo)
		if got := s.reviewDiffTruncatedForRun(context.Background(), runID); got != nil {
			t.Errorf("no entry must yield nil; got %+v", got)
		}
	})
	t.Run("undecodable payload", func(t *testing.T) {
		s, repo, au, _ := gateViewServer(t)
		runID := seedGateRun(t, repo)
		seedReviewDiffTruncatedMarker(au, runID, reviewDiffTruncatedPayload{}, []byte("{not json"))
		if got := s.reviewDiffTruncatedForRun(context.Background(), runID); got != nil {
			t.Errorf("undecodable payload must yield nil; got %+v", got)
		}
	})
}

// TestHandleGetRunGateView_ReviewDiffTruncated_EndToEnd crosses handler -> wire:
// a seeded implement_review_diff_truncated entry is served through the real
// GET /v0/runs/{run_id}/gate-view handler and the decoded response body asserts
// review_diff_truncated.omitted_files AND the residual round-trip intact.
func TestHandleGetRunGateView_ReviewDiffTruncated_EndToEnd(t *testing.T) {
	s, repo, au, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	seedReviewDiffTruncatedMarker(au, runID, reviewDiffTruncatedPayload{
		Reason: "runner_patch_cap", ChangedFileCount: 210, OmittedFileCount: 205,
		OmittedFiles:         []string{"pkg/one.go (no hunks shown)", "pkg/two.go (may be cut — its tail may be missing)"},
		OmittedFilesResidual: 5,
	}, nil)

	w := getGateView(t, s, runID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Decode the raw HTTP body (not a re-marshal) so a json-tag drift is caught.
	var body struct {
		ReviewDiffTruncated *struct {
			Reason               string   `json:"reason"`
			OmittedFiles         []string `json:"omitted_files"`
			OmittedFilesResidual int      `json:"omitted_files_residual"`
		} `json:"review_diff_truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ReviewDiffTruncated == nil {
		t.Fatal("review_diff_truncated absent from the wire body")
	}
	if len(body.ReviewDiffTruncated.OmittedFiles) != 2 ||
		body.ReviewDiffTruncated.OmittedFiles[0] != "pkg/one.go (no hunks shown)" {
		t.Errorf("omitted_files did not round-trip: %+v", body.ReviewDiffTruncated.OmittedFiles)
	}
	if body.ReviewDiffTruncated.OmittedFilesResidual != 5 {
		t.Errorf("omitted_files_residual = %d, want 5 (residual must round-trip end to end)",
			body.ReviewDiffTruncated.OmittedFilesResidual)
	}
}

// --- stale-review surface (#3655) ----------------------------------------

// seedReviewHeadMismatch appends a review_head_mismatch audit entry at the
// given sequence (or a raw payload when rawPayload != nil, for the undecodable
// case).
func seedReviewHeadMismatch(au *auditFake, runID, stageID uuid.UUID, seq int64, p reviewHeadMismatchPayload, rawPayload []byte) {
	payload := rawPayload
	if payload == nil {
		payload, _ = json.Marshal(p)
	}
	rid, sid := runID, stageID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Sequence: seq, Category: CategoryReviewHeadMismatch,
		Payload: payload, Timestamp: time.Now().UTC(),
	})
}

// seedRHMReviewStarted appends an implement_review_started row at seq.
func seedRHMReviewStarted(au *auditFake, runID, stageID uuid.UUID, seq int64) {
	rid, sid := runID, stageID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Sequence: seq, Category: "implement_review_started",
		Payload: []byte(`{"tree_sha":"t"}`), Timestamp: time.Now().UTC(),
	})
}

// Two distinct literal trees / heads, definitionally unequal (seeded by
// construction).
const (
	rhmReviewedTree = "1111111111111111111111111111111111111111"
	rhmPushedTree   = "2222222222222222222222222222222222222222"
	rhmReviewedHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rhmPushedHead   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func rhmPayload(runID, stageID uuid.UUID, reviewedTree string) reviewHeadMismatchPayload {
	return reviewHeadMismatchPayload{
		RunID: runID.String(), StageID: stageID.String(),
		ReviewedTreeSHA: reviewedTree, PushedTreeSHA: rhmPushedTree, ReviewRoundSequence: 7,
		ReviewedHeadSHA: rhmReviewedHead, PushedHeadSHA: rhmPushedHead,
	}
}

// TestBuildGateView_ReviewHeadMismatch_Present (G1): a recorded mismatch
// surfaces with both trees and both heads, and the NEWEST entry wins.
// COUNTERFACTUAL: delete the reviewHeadMismatchForRun call from buildGateView
// → the field is nil and this goes RED.
func TestBuildGateView_ReviewHeadMismatch_Present(t *testing.T) {
	s, repo, au, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	seedRHMReviewStarted(au, runID, stageID, 7)
	seedReviewHeadMismatch(au, runID, stageID, 8, rhmPayload(runID, stageID, "0000000000000000000000000000000000000000"), nil)
	seedReviewHeadMismatch(au, runID, stageID, 9, rhmPayload(runID, stageID, rhmReviewedTree), nil)

	w := getGateView(t, s, runID, "")
	resp := decodeGateView(t, w)
	got := resp.ReviewHeadMismatch
	if got == nil {
		t.Fatalf("review_head_mismatch = nil, want the newest entry; body=%s", w.Body.String())
	}
	want := gateViewReviewHeadMismatch{
		StageID: stageID.String(), ReviewedTreeSHA: rhmReviewedTree, PushedTreeSHA: rhmPushedTree,
		ReviewRoundSequence: 7, ReviewedHeadSHA: rhmReviewedHead, PushedHeadSHA: rhmPushedHead,
	}
	if *got != want {
		t.Errorf("review_head_mismatch = %+v, want %+v", *got, want)
	}
	// Decode the raw HTTP body (not the Go struct) so a json-tag drift is caught.
	var raw struct {
		RHM map[string]any `json:"review_head_mismatch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, k := range []string{"stage_id", "reviewed_tree_sha", "pushed_tree_sha", "review_round_sequence", "reviewed_head_sha", "pushed_head_sha"} {
		if _, ok := raw.RHM[k]; !ok {
			t.Errorf("wire key review_head_mismatch.%s absent; got %v", k, raw.RHM)
		}
	}
}

// TestBuildGateView_ReviewHeadMismatch_AbsentKey (G2): with NO mismatch row the
// key is ABSENT from the marshalled JSON, not null.
func TestBuildGateView_ReviewHeadMismatch_AbsentKey(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	w := getGateView(t, s, runID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if v, ok := raw["review_head_mismatch"]; ok {
		t.Errorf("review_head_mismatch present (%s), want the key absent", v)
	}
}

// TestBuildGateView_ReviewHeadMismatch_Degrades (G3): each degrade yields nil,
// never a partially-populated field and never a failed gate view — and the
// concerns stay intact.
func TestBuildGateView_ReviewHeadMismatch_Degrades(t *testing.T) {
	t.Run("nil AuditRepo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if got := s.reviewHeadMismatchForRun(context.Background(), uuid.New()); got != nil {
			t.Errorf("nil AuditRepo must yield nil; got %+v", got)
		}
	})
	t.Run("list error", func(t *testing.T) {
		s, repo, base, cr := gateViewServer(t)
		runID := seedGateRun(t, repo)
		stageID := uuid.New()
		seedReviewHeadMismatch(base, runID, stageID, 9, rhmPayload(runID, stageID, rhmReviewedTree), nil)
		row := seedRoutedConcern(t, cr, runID, stageID, "fable-5", "a note", "routed")
		s.cfg.AuditRepo = &oneCategoryErrAudit{auditFake: base, failCategory: CategoryReviewHeadMismatch}
		if got := s.reviewHeadMismatchForRun(context.Background(), runID); got != nil {
			t.Errorf("list error must yield nil; got %+v", got)
		}
		resp := decodeGateView(t, getGateView(t, s, runID, ""))
		if resp.ReviewHeadMismatch != nil {
			t.Errorf("gate view review_head_mismatch = %+v, want nil on list error", resp.ReviewHeadMismatch)
		}
		found := false
		for _, c := range resp.Open {
			if c.ID == row.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("open concerns lost on the degrade: %+v", resp.Open)
		}
	})
	t.Run("no entry", func(t *testing.T) {
		s, repo, _, _ := gateViewServer(t)
		runID := seedGateRun(t, repo)
		if got := s.reviewHeadMismatchForRun(context.Background(), runID); got != nil {
			t.Errorf("no entry must yield nil; got %+v", got)
		}
	})
	t.Run("undecodable payload", func(t *testing.T) {
		s, repo, au, _ := gateViewServer(t)
		runID := seedGateRun(t, repo)
		seedReviewHeadMismatch(au, runID, uuid.New(), 9, reviewHeadMismatchPayload{}, []byte("{not json"))
		if got := s.reviewHeadMismatchForRun(context.Background(), runID); got != nil {
			t.Errorf("undecodable payload must yield nil; got %+v", got)
		}
	})
}

// TestReviewHeadMismatchForRun_Supersession: a same-stage
// implement_review_started round recorded AFTER the mismatch supersedes it (a
// fix-up forced a fresh round); an OLDER round, a DIFFERENT stage's round, or a
// failed supersession read keeps it. COUNTERFACTUAL: delete the supersession
// loop → "newer same-stage round" goes RED; drop the stage match → "other
// stage" goes RED.
func TestReviewHeadMismatchForRun_Supersession(t *testing.T) {
	cases := []struct {
		name      string
		seed      func(au *auditFake, runID, stageID uuid.UUID)
		failStart bool
		wantNil   bool
	}{
		{"newer same-stage round supersedes", func(au *auditFake, runID, stageID uuid.UUID) {
			seedRHMReviewStarted(au, runID, stageID, 10)
		}, false, true},
		{"older same-stage round keeps", func(au *auditFake, runID, stageID uuid.UUID) {
			seedRHMReviewStarted(au, runID, stageID, 7)
		}, false, false},
		{"newer other-stage round keeps", func(au *auditFake, runID, _ uuid.UUID) {
			seedRHMReviewStarted(au, runID, uuid.New(), 10)
		}, false, false},
		{"supersession read error keeps", func(au *auditFake, runID, stageID uuid.UUID) {
			seedRHMReviewStarted(au, runID, stageID, 10)
		}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au, _ := gateViewServer(t)
			runID := seedGateRun(t, repo)
			stageID := uuid.New()
			seedReviewHeadMismatch(au, runID, stageID, 9, rhmPayload(runID, stageID, rhmReviewedTree), nil)
			tc.seed(au, runID, stageID)
			if tc.failStart {
				s.cfg.AuditRepo = &oneCategoryErrAudit{auditFake: au, failCategory: "implement_review_started"}
			}
			got := s.reviewHeadMismatchForRun(context.Background(), runID)
			if tc.wantNil && got != nil {
				t.Errorf("want superseded (nil); got %+v", got)
			}
			if !tc.wantNil && got == nil {
				t.Error("want the mismatch kept; got nil")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// #3319 — `disputed` is SCOPED to refused CONFIRMS.
// ---------------------------------------------------------------------------

// TestGateView_ReopenVeto_NotDisputedButSurfacedAsDetail: a concern carrying
// ONLY a refused `reopened` reports disputed=false with one disputes[] row. A
// refused reopen leaves the concern addressed_pending, so calling it `disputed`
// would tell the operator a confirmation failed to settle it — the opposite of
// what happened. The refusal still surfaces as detail.
func TestGateView_ReopenVeto_NotDisputedButSurfacedAsDetail(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", gateViewLongNote, "routed: fix the authz check")

	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:            planreview.VerdictReject,
			ConcernResolutions: []planreview.ConcernResolution{{ID: row.ID.String(), Resolution: "reopened"}},
		},
		model: "fable-5",
	}
	s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: reviewer}},
		planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget, "", 0)

	got := openConcernByID(t, decodeGateView(t, getGateView(t, s, runID, "")), row.ID)
	if got.State != string(concern.StateAddressedPending) {
		t.Fatalf("state = %q, want addressed_pending", got.State)
	}
	if got.Disputed {
		t.Error("disputed = true on a refused REOPEN, want false — no confirmation failed to settle this concern")
	}
	if len(got.Disputes) != 1 {
		t.Fatalf("disputes = %+v, want exactly one row carrying the refusal as detail", got.Disputes)
	}
	if got.Disputes[0].VetoReason != vetoReopenWithoutNamedConcern || got.Disputes[0].Resolution != "reopened" {
		t.Errorf("dispute = %+v, want veto_reason=%s resolution=reopened", got.Disputes[0], vetoReopenWithoutNamedConcern)
	}
}

// TestGateView_ConfirmVeto_StillDisputed: the pre-#3319 behaviour is unchanged
// for a refused `confirmed`.
func TestGateView_ConfirmVeto_StillDisputed(t *testing.T) {
	s, repo, _, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", gateViewLongNote, "routed")

	runSplitRound(s, runID, stageID, row.ID.String())

	got := openConcernByID(t, decodeGateView(t, getGateView(t, s, runID, "")), row.ID)
	if !got.Disputed {
		t.Error("disputed = false on a refused CONFIRM, want true")
	}
	if len(got.Disputes) != 1 || got.Disputes[0].Resolution != "confirmed" {
		t.Errorf("disputes = %+v, want one refused confirm", got.Disputes)
	}
}

// TestGateView_LegacyVetoPayload_EmptyResolution_StillDisputed: a veto payload
// recorded before #3319 carries no `resolution` string. Those were ALWAYS
// confirm vetoes, so an empty resolution must still set disputed.
func TestGateView_LegacyVetoPayload_EmptyResolution_StillDisputed(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	stageID := uuid.New()
	row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", gateViewLongNote, "routed")

	// A legacy entry BY CONSTRUCTION: veto_reason set, resolution absent.
	seedStageAuditEntry(t, au, runID, stageID, 11, concernResolutionVetoedCategory, map[string]any{
		"concern_id":      row.ID.String(),
		"veto_reason":     vetoRaiserRejectedSameRound,
		"review_sequence": 11,
	})

	got := openConcernByID(t, decodeGateView(t, getGateView(t, s, runID, "")), row.ID)
	if !got.Disputed {
		t.Error("disputed = false on a legacy veto payload with an empty resolution, want true — pre-#3319 vetoes were always confirms")
	}
	if len(got.Disputes) != 1 || got.Disputes[0].Resolution != "" {
		t.Errorf("disputes = %+v, want one row with an empty resolution", got.Disputes)
	}
}

// --- captain block (E76.3 / #3766, ADR-083 rule 4) -------------------------

// TestGateView_CaptainBlockOmittedWhenUnwired: with no CaptainStore the
// response carries no captain key and records no captain gap — the pre-E76.3
// gate view, byte-for-byte.
func TestGateView_CaptainBlockOmittedWhenUnwired(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	w := getGateView(t, s, runID, "")
	resp := decodeGateView(t, w)
	if strings.Contains(w.Body.String(), `"captain"`) || resp.Captain != nil {
		t.Errorf("unwired captain store rendered a captain block:\n%s", w.Body.String())
	}
	if resp.HistoryIncomplete || len(resp.HistoryGaps) != 0 {
		t.Errorf("unwired captain store recorded gaps %v (incomplete=%v), want none", resp.HistoryGaps, resp.HistoryIncomplete)
	}
}

// TestGateView_CaptainBlock_NonUUIDRunAccountIsGap: a run whose account id is
// not a UUID resolves UNAVAILABLE (never the untenanted partition): the block
// is omitted and the gap is named. The store is never read on this path, so
// a pool-less store is enough to wire it.
func TestGateView_CaptainBlock_NonUUIDRunAccountIsGap(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	s.cfg.CaptainStore = captain.NewStore(nil)
	runID := seedGateRun(t, repo)
	repo.mu.Lock()
	repo.runs[runID].AccountID = "not-a-uuid"
	repo.mu.Unlock()
	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if resp.Captain != nil {
		t.Errorf("captain = %+v, want omitted on an unparseable run account", resp.Captain)
	}
	if !resp.HistoryIncomplete || !slices.Contains(resp.HistoryGaps, gateViewGapCaptainRecord) {
		t.Errorf("history gaps = %v (incomplete=%v), want %q", resp.HistoryGaps, resp.HistoryIncomplete, gateViewGapCaptainRecord)
	}
}

// --- E77.7 (#3741): the captain's crew_messages block ---

// crewGateServer wires a gate-view server over f's pg run/audit/mailbox (the
// audit repository optionally wrapped) with an empty concern fake.
func crewGateServer(f *crewPG, ar audit.Repository) *Server {
	return New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     run.NewPostgresRepository(f.pool),
		AuditRepo:   ar,
		ConcernRepo: newFakeConcernRepo(),
		CrewMailbox: f.mailbox,
	})
}

// TestGateView_CrewMessagesRendered (done-means): every OPEN finding/notice
// thread root on the run is listed whatever its recipient, with rendered
// byte-equal to prompt.RenderCrewMessages over that one message; consults,
// escalations and disposed findings are not, and none of it is a concern.
func TestGateView_CrewMessagesRendered(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	ctx := context.Background()
	finding := f.send(t, crewDoc("finding", "", "architect", runAnchor(runID),
		`{"summary":"GATE-SENTINEL-finding","severity":"high"}`), operator("write:stages")).SentSequence
	notice := seedCrewMail(t, f, runID, crewmessage.TypeNotice, crewmessage.RoleReviewer, "GATE-SENTINEL-notice")
	seedCrewMail(t, f, runID, crewmessage.TypeConsult, crewmessage.RolePlanner, "GATE-EXCLUDED-consult")
	seedCrewMail(t, f, runID, crewmessage.TypeEscalation, crewmessage.RoleCaptain, "GATE-EXCLUDED-escalation")
	disposed := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "GATE-EXCLUDED-disposed")
	if _, err := f.mailbox.Dispose(ctx, crewmessage.DisposeParams{
		SentSequence: disposed, Disposition: crewmessage.DispositionAccepted,
		Actor: crewmessage.Actor{Kind: audit.ActorUser, Subject: "github:op"},
	}); err != nil {
		t.Fatalf("dispose: %v", err)
	}

	s := crewGateServer(f, f.audit)
	w := getGateView(t, s, runID, "")
	resp := decodeGateView(t, w)
	if len(resp.CrewMessages) != 2 {
		t.Fatalf("crew_messages = %+v, want exactly the open finding and notice", resp.CrewMessages)
	}
	for i, want := range []int64{finding, notice} {
		got := resp.CrewMessages[i]
		if got.SentSequence != want || got.State != string(crewmessage.StateOpen) {
			t.Fatalf("crew_messages[%d] = %+v, want open sequence %d", i, got, want)
		}
		row, err := f.mailbox.Store().Get(ctx, want)
		if err != nil {
			t.Fatalf("get row: %v", err)
		}
		expect, err := s.renderCrewRow(ctx, row)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got.Rendered != expect {
			t.Errorf("crew_messages[%d].rendered is not prompt.RenderCrewMessages' output:\n got %q\nwant %q", i, got.Rendered, expect)
		}
	}
	if f0 := resp.CrewMessages[0]; f0.MessageType != "finding" || f0.RecipientRole != "architect" || f0.SenderRole != "captain" || f0.Severity != "high" {
		t.Errorf("finding metadata = %+v", f0)
	}
	requireInsideCrewEnvelopes(t, resp.CrewMessages[0].Rendered, "GATE-SENTINEL-finding")
	body := w.Body.String()
	if strings.Contains(body, "GATE-EXCLUDED") {
		t.Errorf("an excluded message reached the gate view:\n%s", body)
	}
	if len(resp.Open) != 0 || resp.HistoryIncomplete {
		t.Errorf("open = %d, history_incomplete = %v; an open finding must not be a concern or a gap", len(resp.Open), resp.HistoryIncomplete)
	}
}

// TestGateView_RunBoundTokenSeesNoCrewMessages (C8): the identity is
// mcp:run:<the run's OWN id>, so the cross-run guard PASSES and the omission
// is the only thing between the token and the crew text. The operator control
// proves the fixture carries a block to omit.
func TestGateView_RunBoundTokenSeesNoCrewMessages(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypeImplement)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "RUNBOUND-GATE-SENTINEL")
	s := crewGateServer(f, f.audit)

	if got := decodeGateView(t, getGateView(t, s, runID, "")); len(got.CrewMessages) != 1 {
		t.Fatalf("control: operator crew_messages = %d, want 1", len(got.CrewMessages))
	}
	w := callGateView(s, runID, "", Identity{Subject: "mcp:run:" + runID.String(), TokenID: "tok-run",
		Scopes: []string{"mcp:read", "write:scope-amendments"}})
	resp := decodeGateView(t, w)
	if resp.CrewMessages != nil {
		t.Fatalf("a run-bound token read crew_messages: %+v", resp.CrewMessages)
	}
	if strings.Contains(w.Body.String(), "RUNBOUND-GATE-SENTINEL") || strings.Contains(w.Body.String(), "crew_messages") {
		t.Fatalf("crew text reached a run-bound gate-view read:\n%s", w.Body.String())
	}
}

// TestGateView_CrewMessagesUnconfigured: no mailbox is the pre-E77.7
// response — no block and no gap.
func TestGateView_CrewMessagesUnconfigured(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "UNWIRED")
	s := crewGateServer(f, f.audit)
	s.cfg.CrewMailbox = nil
	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if resp.CrewMessages != nil || slices.Contains(resp.HistoryGaps, gateViewGapCrewMessages) {
		t.Fatalf("unwired mailbox: crew_messages = %+v, gaps = %v; want neither", resp.CrewMessages, resp.HistoryGaps)
	}
}

// TestGateView_CrewReadFailureRecordsGap: a failed row list AND a failed
// per-message document read each omit the block with a crew_messages gap and
// history_incomplete — a partial read is never rendered as absence.
func TestGateView_CrewReadFailureRecordsGap(t *testing.T) {
	requireGap := func(t *testing.T, resp gateViewResponse) {
		t.Helper()
		if resp.CrewMessages != nil {
			t.Fatalf("crew_messages = %+v, want nil on a failed read", resp.CrewMessages)
		}
		if !resp.HistoryIncomplete || !slices.Contains(resp.HistoryGaps, gateViewGapCrewMessages) {
			t.Fatalf("history_incomplete = %v, gaps = %v; want a crew_messages gap", resp.HistoryIncomplete, resp.HistoryGaps)
		}
	}
	t.Run("document", func(t *testing.T) {
		f := newCrewPG(t)
		runID := f.seedRun(t, run.StageTypePlan)
		seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "DOCGAP-good")
		bad := seedCrewMail(t, f, runID, crewmessage.TypeNotice, crewmessage.RolePlanner, "DOCGAP-bad")
		s := crewGateServer(f, &deliveryAuditRepo{Repository: f.audit, tamper: map[int64]bool{bad: true}})
		requireGap(t, decodeGateView(t, getGateView(t, s, runID, "")))
	})
	t.Run("rows", func(t *testing.T) {
		f := newCrewPG(t)
		runID := f.seedRun(t, run.StageTypePlan)
		seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "ROWGAP")
		if _, err := f.pool.Exec(context.Background(), `ALTER TABLE crew_messages RENAME TO crew_messages_gone`); err != nil {
			t.Fatalf("rename: %v", err)
		}
		requireGap(t, decodeGateView(t, getGateView(t, crewGateServer(f, f.audit), runID, "")))
	})
}

// --- E77.8 (#3742): the crew-consult block ---------------------------------

// TestGateView_ConsultsDegradeVisibly is the counterfactual vehicle for the
// consults block's fail-visible branch: the crew_message_sent read fails, and
// the response must carry history_incomplete + the NAMED gap with the block
// EMPTY rather than partially built. Deleting the gap record leaves
// history_incomplete false and the operator reads "this run asked nothing".
//
// Only the crew_message_sent category is failed, so every other history join
// still builds and the degradation is attributable to this read.
func TestGateView_ConsultsDegradeVisibly(t *testing.T) {
	s, repo, au, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	au.listByCategoryErr = errors.New("injected crew read failure")
	au.listByCategoryErrCategory = "crew_message_sent"

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if !resp.HistoryIncomplete {
		t.Fatal("history_incomplete = false after the crew-consult read failed")
	}
	if !slices.Contains(resp.HistoryGaps, "crew_consults") {
		t.Fatalf("history_gaps = %v, want it to name crew_consults", resp.HistoryGaps)
	}
	if resp.Consults == nil || len(resp.Consults) != 0 {
		t.Fatalf("consults = %+v, want an EMPTY array (never nil, never partial)", resp.Consults)
	}
}

// The healthy control: no consults on the run yields an EMPTY array, NO gap
// and history_incomplete false — so the degradation above is not what a quiet
// run looks like.
func TestGateView_ConsultsEmptyWithoutGap(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if resp.Consults == nil {
		t.Fatal("consults is nil; the block is always present (the SuppressedRelitigations posture)")
	}
	if len(resp.Consults) != 0 {
		t.Fatalf("consults = %+v, want empty", resp.Consults)
	}
	if slices.Contains(resp.HistoryGaps, "crew_consults") {
		t.Fatalf("a quiet run recorded a crew_consults gap: %v", resp.HistoryGaps)
	}
}

// --- gate isolation (E51.2 / #2135) ----------------------------------------

// TestGateView_GateIsolation_EndToEnd is the cross-layer pin: a bundle packed
// around the SHARED runner golden is POSTed through the real trace handler,
// then GET /v0/runs/{id}/gate-view is read through the real handler — bundle
// decode → audit persistence → gate-view render on the same fixed bytes the
// runner's half marshals.
func TestGateView_GateIsolation_EndToEnd(t *testing.T) {
	repo := newFakeRepo()
	runID := seedGateRun(t, repo)
	s, sf, _, _ := newTraceServer(t)
	priv, _ := sf.issue(t, runID)
	stageID := uuid.New()
	body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "fallback"))
	if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("trace POST status = %d:\n%s", w.Code, w.Body.String())
	}
	s.cfg.RunRepo = repo
	s.cfg.ConcernRepo = newFakeConcernRepo()

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	gi := resp.GateIsolation
	if gi == nil {
		t.Fatalf("gate_isolation block absent after an evidence-bearing upload; gaps=%v", resp.HistoryGaps)
	}
	if gi.Class != "fallback" || gi.Path != "clone" || gi.StageID != stageID.String() ||
		!strings.Contains(gi.ContainerUnavailable, "FISHHAWK_GATE_IMAGE is empty") {
		t.Errorf("block = %+v, want class fallback / path clone / stage %s / container_unavailable naming the empty image", gi, stageID)
	}
	if gi.WorstClass != "fallback" || gi.WorstStageID != stageID.String() {
		t.Errorf("worst = (%q, %q), want the single row's own class + stage", gi.WorstClass, gi.WorstStageID)
	}
}

// TestGateView_GateIsolationWorstClassSurvivesNewerContainer pins condition 3:
// the top-level fields are the NEWEST row, while worst_class / worst_stage_id
// keep naming an earlier fallback or refusal a later container stage would
// otherwise mask.
func TestGateView_GateIsolationWorstClassSurvivesNewerContainer(t *testing.T) {
	type row struct {
		seq   int64
		class string
	}
	cases := []struct {
		name      string
		rows      []row
		worst     string
		worstSeq  int64
		newestCls string
	}{
		{"fallback then container", []row{{10, "fallback"}, {20, "container"}}, "fallback", 10, "container"},
		{"refused then fallback then container", []row{{10, "refused"}, {20, "fallback"}, {30, "container"}}, "refused", 10, "container"},
		{"all container", []row{{10, "container"}, {20, "container"}}, "container", 20, "container"},
		{"two fallbacks: newest wins the tie", []row{{10, "fallback"}, {20, "fallback"}, {30, "container"}}, "fallback", 20, "container"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au, _ := gateViewServer(t)
			runID := seedGateRun(t, repo)
			stages := map[int64]uuid.UUID{}
			for _, r := range tc.rows {
				stages[r.seq] = uuid.New()
				seedGateIsolationRow(t, au, runID, stages[r.seq], r.seq, gateIsolationRowPayload(t, stages[r.seq], r.class))
			}
			newest := tc.rows[len(tc.rows)-1]
			gi := decodeGateView(t, getGateView(t, s, runID, "")).GateIsolation
			if gi == nil {
				t.Fatal("gate_isolation block absent")
			}
			if gi.Class != tc.newestCls || gi.Sequence != newest.seq || gi.StageID != stages[newest.seq].String() {
				t.Errorf("newest = (%q, %d, %s), want (%q, %d, %s)", gi.Class, gi.Sequence, gi.StageID, tc.newestCls, newest.seq, stages[newest.seq])
			}
			if gi.WorstClass != tc.worst || gi.WorstSequence != tc.worstSeq || gi.WorstStageID != stages[tc.worstSeq].String() {
				t.Errorf("worst = (%q, %d, %s), want (%q, %d, %s)", gi.WorstClass, gi.WorstSequence, gi.WorstStageID, tc.worst, tc.worstSeq, stages[tc.worstSeq])
			}
		})
	}
}

func TestGateView_GateIsolationAbsentKeyOmitted(t *testing.T) {
	s, repo, _, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	w := getGateView(t, s, runID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"gate_isolation"`) {
		t.Errorf("a run with no gate_isolation_recorded row carried the key:\n%s", w.Body.String())
	}
	resp := decodeGateView(t, w)
	if slices.Contains(resp.HistoryGaps, CategoryGateIsolationRecorded) {
		t.Errorf("a quiet run recorded a gate_isolation gap: %v", resp.HistoryGaps)
	}
}

func TestGateView_GateIsolationListErrorIsGap(t *testing.T) {
	s, repo, au, _ := gateViewServer(t)
	runID := seedGateRun(t, repo)
	au.listByCategoryErrCategory = CategoryGateIsolationRecorded
	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	if resp.GateIsolation != nil {
		t.Errorf("block = %+v, want nil on a read failure", resp.GateIsolation)
	}
	if !resp.HistoryIncomplete || !slices.Contains(resp.HistoryGaps, CategoryGateIsolationRecorded) {
		t.Errorf("history_incomplete=%v gaps=%v, want a gate_isolation_recorded gap", resp.HistoryIncomplete, resp.HistoryGaps)
	}
}

// TestGateView_GateIsolationUndecodableIsGap pins the wholesale degrade: an
// undecodable row — newest OR older — omits the block with a gap, because a
// worst_class built around a row it could not read could hide a refusal.
func TestGateView_GateIsolationUndecodableIsGap(t *testing.T) {
	for _, undecodableNewest := range []bool{true, false} {
		name := "older row undecodable"
		if undecodableNewest {
			name = "newest row undecodable"
		}
		t.Run(name, func(t *testing.T) {
			s, repo, au, _ := gateViewServer(t)
			runID := seedGateRun(t, repo)
			bad := json.RawMessage(`{"stage_id":"x","class":7}`)
			good := gateIsolationRowPayload(t, uuid.New(), "container")
			first, second := bad, good
			if undecodableNewest {
				first, second = good, bad
			}
			seedGateIsolationRow(t, au, runID, uuid.New(), 10, first)
			seedGateIsolationRow(t, au, runID, uuid.New(), 20, second)
			resp := decodeGateView(t, getGateView(t, s, runID, ""))
			if resp.GateIsolation != nil {
				t.Errorf("block = %+v, want nil when a row does not decode", resp.GateIsolation)
			}
			if !resp.HistoryIncomplete || !slices.Contains(resp.HistoryGaps, CategoryGateIsolationRecorded) {
				t.Errorf("history_incomplete=%v gaps=%v, want a gate_isolation_recorded gap", resp.HistoryIncomplete, resp.HistoryGaps)
			}
		})
	}
}

// --- merge readiness (#4086) ---------------------------------------------

// failStagesRepo fails ListStagesForRun so the merge-readiness block's stages
// read degrades visibly.
type failStagesRepo struct{ *fakeRepo }

func (r *failStagesRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("injected list-stages error")
}

// readinessGateViewServer is gateViewServer over driveE2ERepo, whose
// ListStagesForRun serves the seeded stagesByRun (the bare fakeRepo does not
// implement it).
func readinessGateViewServer(t *testing.T) (*Server, *fakeRepo, *auditFake, *fakeConcernRepo) {
	t.Helper()
	repo := &driveE2ERepo{fakeRepo: newFakeRepo()}
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr})
	return s, repo.fakeRepo, au, cr
}

// seedMergeReadinessRun stores a run row carrying a pull request url and the
// given stages directly in the fake repo.
func seedMergeReadinessRun(repo *fakeRepo, state run.State, prURL string, stages func(uuid.UUID) []*run.Stage) *run.Run {
	runRow := &run.Run{ID: uuid.New(), Repo: "x/y", InstallationID: instID(42), State: state, WorkflowID: "feature_change"}
	if prURL != "" {
		runRow.PullRequestURL = &prURL
	}
	repo.mu.Lock()
	repo.runs[runRow.ID] = runRow
	if stages != nil {
		repo.stagesByRun[runRow.ID] = stages(runRow.ID)
	}
	repo.mu.Unlock()
	return runRow
}

func pendingAcceptanceStages(runID uuid.UUID) []*run.Stage {
	return acceptanceMergeStages(runID, run.StageStatePending)
}

// seedStaleAcceptance seeds a `passed` outcome at H1 then a stage-scoped
// re-open at H2 on the run's acceptance stage — the state
// reopenAcceptanceOnFixupPush leaves.
func seedStaleAcceptance(au *auditFake, repo *fakeRepo, runID uuid.UUID, h1, h2 string) uuid.UUID {
	repo.mu.Lock()
	accID := repo.stagesByRun[runID][2].ID
	repo.mu.Unlock()
	seedReadinessEntry(au, runID, &accID, CategoryAcceptanceOutcomeRecorded, 5, map[string]any{"verdict": "passed", "head_sha": h1})
	seedReadinessEntry(au, runID, &accID, CategoryAcceptanceReopened, 8, map[string]any{"head_sha": h2})
	return accID
}

func mergeBlockerByCode(mr *gateViewMergeReadiness, code string) *gateViewMergeBlocker {
	for i := range mr.Blockers {
		if mr.Blockers[i].Code == code {
			return &mr.Blockers[i]
		}
	}
	return nil
}

func undeterminedContains(mr *gateViewMergeReadiness, sub string) bool {
	for _, u := range mr.Undetermined {
		if strings.Contains(u, sub) {
			return true
		}
	}
	return false
}

// TestGateView_MergeReadiness pins the gate view's merge_readiness block: the
// same two helpers the merge endpoint calls, rendered as blockers, with every
// fail-open reason and the acceptance-history read failure surfaced instead of
// a silent empty block.
func TestGateView_MergeReadiness(t *testing.T) {
	const h1, h2 = "aaaa000000000000000000000000000000000001", "bbbb000000000000000000000000000000000002"

	t.Run("acceptance_stale blocker and no GitHub client undetermined", func(t *testing.T) {
		s, repo, au, _ := readinessGateViewServer(t)
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, pendingAcceptanceStages)
		accID := seedStaleAcceptance(au, repo, runRow.ID, h1, h2)

		resp := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
		mr := resp.MergeReadiness
		if mr == nil {
			t.Fatal("merge_readiness = nil, want the block for a run with a pull request")
		}
		b := mergeBlockerByCode(mr, mergeCodeAcceptanceStale)
		if b == nil {
			t.Fatalf("blockers = %+v, want acceptance_stale", mr.Blockers)
		}
		if b.Details["verified_head_sha"] != h1 || b.Details["current_head_sha"] != h2 ||
			b.Details["next_step"] != "fishhawk_dispatch_stage" || b.Details["acceptance_stage_id"] != accID.String() {
			t.Errorf("acceptance_stale details = %+v, want H1/H2/dispatch/stage id", b.Details)
		}
		if !strings.Contains(b.Message, h1) || !strings.Contains(b.Message, h2) {
			t.Errorf("acceptance_stale message %q does not name both heads", b.Message)
		}
		if !undeterminedContains(mr, "no GitHub client") {
			t.Errorf("undetermined = %v, want the approval check's 'no GitHub client' reason", mr.Undetermined)
		}
		if resp.HistoryIncomplete || containsString(resp.HistoryGaps, gateViewGapMergeReadiness) {
			t.Errorf("history_incomplete=%v gaps=%v, want no merge_readiness gap on a healthy read", resp.HistoryIncomplete, resp.HistoryGaps)
		}
	})

	t.Run("approval_dismissed blocker via the forge", func(t *testing.T) {
		s, repo, au, _ := readinessGateViewServer(t)
		gh := blockedDismissed()
		s.cfg.GitHub = newApprovalGitHubClient(t, gh)
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, nil)
		seedReadinessEntry(au, runRow.ID, nil, CategoryOperatorCommitVouched, 3, map[string]any{lineageVouchedSHAField: adHead})

		resp := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
		mr := resp.MergeReadiness
		if mr == nil {
			t.Fatal("merge_readiness = nil, want the block")
		}
		b := mergeBlockerByCode(mr, mergeCodeApprovalDismissed)
		if b == nil {
			t.Fatalf("blockers = %+v, want approval_dismissed", mr.Blockers)
		}
		if b.Details["dismissing_commit"] != adHead || b.Details["approved_commit"] != adApproved ||
			b.Details["dismissing_cause"] != dismissingCauseVouchCommit || b.Details["next_step"] != "approve_pr" {
			t.Errorf("approval_dismissed details = %+v", b.Details)
		}
		if len(mr.Undetermined) != 0 {
			t.Errorf("undetermined = %v, want none (both checks determined)", mr.Undetermined)
		}
		if mergeBlockerByCode(mr, mergeCodeAcceptanceStale) != nil {
			t.Errorf("blockers = %+v, want no acceptance_stale on a run without an acceptance stage", mr.Blockers)
		}
	})

	t.Run("no blocker on a clean determined read", func(t *testing.T) {
		s, repo, _, _ := readinessGateViewServer(t)
		s.cfg.GitHub = newApprovalGitHubClient(t, &approvalGitHub{mergeableState: "blocked", headSHA: adHead,
			reviews: []map[string]any{review(1, "alice", "APPROVED", adHead)}})
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, pendingAcceptanceStages)

		mr := decodeGateView(t, getGateView(t, s, runRow.ID, "")).MergeReadiness
		if mr == nil || len(mr.Blockers) != 0 || len(mr.Undetermined) != 0 {
			t.Fatalf("merge_readiness = %+v, want an empty, fully-determined block", mr)
		}
		// Always-present arrays: the wire carries [] rather than null.
		raw := getGateView(t, s, runRow.ID, "").Body.String()
		if !strings.Contains(raw, `"merge_readiness":{"blockers":[],"undetermined":[]}`) {
			t.Errorf("merge_readiness wire = %s, want both arrays present and empty", raw)
		}
	})

	t.Run("omitted without a pull request or on a failed/cancelled run", func(t *testing.T) {
		s, repo, au, _ := readinessGateViewServer(t)
		for name, row := range map[string]*run.Run{
			"no PR url": seedMergeReadinessRun(repo, run.StateRunning, "", pendingAcceptanceStages),
			"failed":    seedMergeReadinessRun(repo, run.StateFailed, mergePR, pendingAcceptanceStages),
			"cancelled": seedMergeReadinessRun(repo, run.StateCancelled, mergePR, pendingAcceptanceStages),
		} {
			seedStaleAcceptance(au, repo, row.ID, h1, h2)
			w := getGateView(t, s, row.ID, "")
			if resp := decodeGateView(t, w); resp.MergeReadiness != nil {
				t.Errorf("%s: merge_readiness = %+v, want omitted", name, resp.MergeReadiness)
			}
			if strings.Contains(w.Body.String(), "merge_readiness") {
				t.Errorf("%s: wire carries merge_readiness, want the key omitted", name)
			}
		}
	})

	t.Run("acceptance history read error is a visible gap", func(t *testing.T) {
		s, repo, au, _ := readinessGateViewServer(t)
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, pendingAcceptanceStages)
		seedStaleAcceptance(au, repo, runRow.ID, h1, h2)
		au.listByCategoryErrCategory = CategoryAcceptanceReopened

		resp := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
		if !resp.HistoryIncomplete || !containsString(resp.HistoryGaps, gateViewGapMergeReadiness) {
			t.Errorf("history_incomplete=%v gaps=%v, want a merge_readiness gap", resp.HistoryIncomplete, resp.HistoryGaps)
		}
		mr := resp.MergeReadiness
		if mr == nil {
			t.Fatal("merge_readiness = nil, want the block with the failure named")
		}
		if !undeterminedContains(mr, "acceptance staleness: the acceptance history could not be read") {
			t.Errorf("undetermined = %v, want the acceptance read failure named", mr.Undetermined)
		}
		if mergeBlockerByCode(mr, mergeCodeAcceptanceStale) != nil {
			t.Errorf("blockers = %+v, want no acceptance_stale built from a failed read", mr.Blockers)
		}
	})

	t.Run("stages read error is a visible gap", func(t *testing.T) {
		base := newFakeRepo()
		repo := &failStagesRepo{fakeRepo: base}
		au := newAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: newFakeConcernRepo()})
		runRow := seedMergeReadinessRun(base, run.StateRunning, mergePR, nil)

		resp := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
		if !resp.HistoryIncomplete || !containsString(resp.HistoryGaps, gateViewGapMergeReadiness) {
			t.Errorf("history_incomplete=%v gaps=%v, want a merge_readiness gap", resp.HistoryIncomplete, resp.HistoryGaps)
		}
		if resp.MergeReadiness == nil || !undeterminedContains(resp.MergeReadiness, "the run's stages could not be read") {
			t.Errorf("merge_readiness = %+v, want the stages read failure named", resp.MergeReadiness)
		}
	})

	t.Run("buildGateView never computes it (attention-scan cost)", func(t *testing.T) {
		s, repo, au, cr := readinessGateViewServer(t)
		gh := blockedDismissed()
		s.cfg.GitHub = newApprovalGitHubClient(t, gh)
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, pendingAcceptanceStages)
		seedStaleAcceptance(au, repo, runRow.ID, h1, h2)
		rows, _ := cr.ListByRun(context.Background(), runRow.ID)

		resp := s.buildGateView(context.Background(), runRow.ID, "", rows)
		if resp.MergeReadiness != nil {
			t.Errorf("buildGateView merge_readiness = %+v, want nil: only the gate-view handler pays the forge reads", resp.MergeReadiness)
		}
		gh.mu.Lock()
		calls := gh.reviewsCalls
		gh.mu.Unlock()
		if calls != 0 {
			t.Errorf("reviews reads = %d, want 0 from buildGateView", calls)
		}
	})
}

// --- settled resolution_basis (#4086) -------------------------------------

// TestGateView_SettledResolutionBasis: a concern resolved through the REAL
// resolve handler reads back in the settled ledger as state=addressed with
// resolution_basis=operator_evidence, while a reviewer-confirmed addressed
// row, a row whose operator resolution a later review overtook, a waived row
// and a prefix-only row without a chain record carry no basis.
func TestGateView_SettledResolutionBasis(t *testing.T) {
	s, repo, au, cr := gateViewServer(t)
	runID := seedGateRun(t, repo)
	ctx := context.Background()

	resolved := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 1, "high", "correctness", "resolved by operator", "")
	if err := cr.MarkAddressedPending(ctx, []uuid.UUID{resolved.ID}, "routed by fix-up"); err != nil {
		t.Fatalf("route: %v", err)
	}
	const evidence = "re-ran the reproduction at the fix-up head"
	w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{resolved.ID.String()}, Evidence: evidence})
	if w.Code != http.StatusOK {
		t.Fatalf("resolve status = %d:\n%s", w.Code, w.Body.String())
	}

	reviewer := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 2, "high", "correctness", "reviewer confirmed", "")
	reviewer.State = concern.StateAddressed
	reviewer.StateReason = "confirmed by re-review"

	// Operator-resolved, then reopened and re-confirmed by a review: the intent
	// entry exists, but the review overwrote the state_reason.
	overtaken := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 3, "high", "correctness", "overtaken", "")
	overtaken.State = concern.StateAddressed
	overtaken.StateReason = "confirmed by a later re-review"
	seedHeadEntry(au, runID, &overtaken.StageID, CategoryConcernResolvedWithEvidence, 50, map[string]any{"concern_id": overtaken.ID.String()})

	// Waived with an intent entry and the prefix: the state alone excludes it.
	waived := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 4, "low", "style", "waived", "")
	waived.State = concern.StateWaived
	waived.StateReason = resolveConcernsReasonPrefix + "not a resolve"
	seedHeadEntry(au, runID, &waived.StageID, CategoryConcernResolvedWithEvidence, 51, map[string]any{"concern_id": waived.ID.String()})

	// The prefix with no chain record: the join alone excludes it.
	prefixOnly := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "m", 5, "low", "style", "prefix only", "")
	prefixOnly.State = concern.StateAddressed
	prefixOnly.StateReason = resolveConcernsReasonPrefix + "no chain record"

	resp := decodeGateView(t, getGateView(t, s, runID, ""))
	byID := map[uuid.UUID]gateViewSettledConcern{}
	for _, sc := range resp.Settled {
		byID[sc.ID] = sc
	}
	if got := byID[resolved.ID]; got.State != string(concern.StateAddressed) || got.ResolutionBasis != gateViewResolutionBasisOperatorEvidence {
		t.Errorf("resolved row = state %q basis %q, want addressed/operator_evidence", got.State, got.ResolutionBasis)
	}
	for name, id := range map[string]uuid.UUID{"reviewer-confirmed": reviewer.ID, "overtaken": overtaken.ID, "waived": waived.ID, "prefix-only": prefixOnly.ID} {
		if got, ok := byID[id]; !ok || got.ResolutionBasis != "" {
			t.Errorf("%s row = %+v (present %v), want settled with no resolution_basis", name, got, ok)
		}
	}

	// A failed concern_resolved_with_evidence read is a named gap and the
	// basis is withheld rather than guessed.
	au.listByCategoryErrCategory = CategoryConcernResolvedWithEvidence
	resp = decodeGateView(t, getGateView(t, s, runID, ""))
	if !resp.HistoryIncomplete || !containsString(resp.HistoryGaps, CategoryConcernResolvedWithEvidence) {
		t.Errorf("history_incomplete=%v gaps=%v, want a concern_resolved_with_evidence gap", resp.HistoryIncomplete, resp.HistoryGaps)
	}
	for _, sc := range resp.Settled {
		if sc.ResolutionBasis != "" {
			t.Errorf("row %s basis = %q under a failed read, want none", sc.ID, sc.ResolutionBasis)
		}
	}
}
