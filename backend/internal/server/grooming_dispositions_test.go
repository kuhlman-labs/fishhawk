package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// grooming_dispositions_test.go pins POST/GET
// /v0/runs/{run_id}/grooming-dispositions (E54.30 / #2843).
//
// FIXTURE DISCIPLINE. Every bad-state fixture is seeded BY CONSTRUCTION, never
// by calling the control inside the test's own setup:
//
//   - the unknown entry id is a freshly generated random string, definitionally
//     not a derived id — the test never asks GroomingEntryIDs what is unknown;
//   - the operator-agent subject is spelled with the exported
//     operatorrole.TokenSubjectPrefix constant, not by asking IsTokenSubject;
//   - the run-bound subject is a literal "mcp:run:" + a uuid, not by asking
//     runBoundTokenRunID.
//
// So a RED lands on the behavioral assertion, not on fixture setup.

// --- fakes ------------------------------------------------------------------

// gdRunRepo serves the run's stage list.
type gdRunRepo struct {
	run.BaseFake
	stages    []*run.Stage
	stagesErr error
}

func (f *gdRunRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	if f.stagesErr != nil {
		return nil, f.stagesErr
	}
	return f.stages, nil
}

// gdArtifactRepo serves stage artifacts.
type gdArtifactRepo struct {
	byStage map[uuid.UUID][]*artifact.Artifact
	listErr error
}

func (f *gdArtifactRepo) Create(context.Context, artifact.CreateParams) (*artifact.Artifact, error) {
	return nil, errors.New("not implemented")
}
func (f *gdArtifactRepo) Get(context.Context, uuid.UUID) (*artifact.Artifact, error) {
	return nil, errors.New("not implemented")
}
func (f *gdArtifactRepo) GetByHash(context.Context, uuid.UUID, string) (*artifact.Artifact, error) {
	return nil, errors.New("not implemented")
}
func (f *gdArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byStage[stageID], nil
}

// --- harness ----------------------------------------------------------------

// gdFixture is a wired server whose run carries ONE grooming_report artifact
// with one entry of every class.
type gdFixture struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
	art     *artifact.Artifact
	ids     groomingApplyEntryIDs
	au      *auditFake
	// unscopedBearer is a real token plaintext resolving to an identity with
	// read:runs and NOT write:approvals — the G4 credential.
	unscopedBearer string
}

// newGDFixture wires the happy-path server. reportBody nil uses the all-class
// fixture; a non-nil body is used verbatim (the unparseable-report case).
func newGDFixture(t *testing.T, reportBody []byte) *gdFixture {
	t.Helper()
	runID, stageID := uuid.New(), uuid.New()
	report, ids := groomingApplyFullReport()
	body := reportBody
	if body == nil {
		body = groomingApplyReportJSON(t, report)
	}
	art := &artifact.Artifact{
		ID: uuid.New(), StageID: stageID, Kind: artifact.KindGroomingReport,
		Content: body, ContentHash: sha256Hex(body),
		CreatedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
	}
	au := &auditFake{}
	// A REAL bearer token carrying read:runs but NOT write:approvals, wired at
	// construction so the auth middleware can resolve it. The through-the-mux
	// layering test uses it; the direct-handler tests inject an identity.
	tokens := &stubAPITokenRepo{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:ops",
		Scopes: []string{"read:runs"}, PlainText: "fhk_gd_unscoped",
	}}
	s := New(Config{
		RunRepo: &gdRunRepo{stages: []*run.Stage{
			{ID: stageID, RunID: runID, Type: run.StageTypePlan},
		}},
		ArtifactRepo: &gdArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{stageID: {art}}},
		AuditRepo:    au,
		APITokenRepo: tokens,
	})
	return &gdFixture{s: s, runID: runID, stageID: stageID, art: art, ids: ids, au: au, unscopedBearer: tokens.tok.PlainText}
}

// gdOperator is the credential the capture verb accepts: an operator bearer
// token carrying write:approvals.
func gdOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:approvals"},
	}))
}

// postGD posts a disposition batch with the given identity mutator.
func postGD(t *testing.T, s *Server, runID uuid.UUID, raw string,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/grooming-dispositions", strings.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleRecordGroomingDispositions(w, withID(req))
	return w
}

// getGD reads back the recorded dispositions.
func getGD(t *testing.T, s *Server, runID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+runID.String()+"/grooming-dispositions", nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleListGroomingDispositions(w, gdOperator(req))
	return w
}

// gdBatch renders a one-entry request body.
func gdBatch(entryID, verdict string) string {
	return fmt.Sprintf(`{"dispositions":[{"entry_id":%q,"verdict":%q}]}`, entryID, verdict)
}

// requireGDError asserts BOTH the status AND the error code. Status alone would
// green on an unrelated failure that happens to share the status.
func requireGDError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, wantStatus, w.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, w.Body.String())
	}
	if env.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q; body: %s", env.Error.Code, wantCode, w.Body.String())
	}
}

// gdRows returns every appended grooming_disposition_recorded param.
func gdRows(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryGroomingDispositionRecorded {
			out = append(out, au.appended[i])
		}
	}
	return out
}

// --- G0..G8: one behavioral test per named rung ------------------------------

// G0. TestGroomingDispositionsUnconfigured: the repositories are not wired.
func TestGroomingDispositionsUnconfigured(t *testing.T) {
	s := New(Config{})
	w := postGD(t, s, uuid.New(), `{"dispositions":[{"entry_id":"x","verdict":"approved"}]}`, gdOperator)
	requireGDError(t, w, http.StatusServiceUnavailable, "grooming_dispositions_unconfigured")
}

// G1. TestGroomingDispositionsAnonymousRejected.
func TestGroomingDispositionsAnonymousRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"),
		func(req *http.Request) *http.Request { return req })
	requireGDError(t, w, http.StatusUnauthorized, "authentication_required")
	if n := len(gdRows(f.au)); n != 0 {
		t.Errorf("appended %d rows on an anonymous request, want 0", n)
	}
}

// G2. TestGroomingDispositionsRunBoundTokenRejected asserts the refusal for the
// token's OWN run — so the test proves the refusal is UNCONDITIONAL, not merely
// a cross-run guard. The subject is a literal "mcp:run:"+uuid, seeded by
// construction rather than by asking runBoundTokenRunID.
func TestGroomingDispositionsRunBoundTokenRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"),
		func(req *http.Request) *http.Request {
			return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
				Subject: "mcp:run:" + f.runID.String(), TokenID: "tok-run",
				Scopes: []string{"write:approvals"},
			}))
		})
	requireGDError(t, w, http.StatusForbidden, "run_token_forbidden")
	if n := len(gdRows(f.au)); n != 0 {
		t.Errorf("appended %d rows for a run-bound token, want 0", n)
	}
}

// G3. TestGroomingDispositionsOperatorAgentRejected. The subject is spelled with
// the exported operatorrole.TokenSubjectPrefix constant, seeded by construction.
func TestGroomingDispositionsOperatorAgentRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"),
		func(req *http.Request) *http.Request {
			return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
				Subject: operatorrole.TokenSubjectPrefix + "operator-role-v0",
				TokenID: "tok-delegated", Scopes: []string{"write:approvals"},
			}))
		})
	requireGDError(t, w, http.StatusForbidden, "operator_agent_forbidden")
	if n := len(gdRows(f.au)); n != 0 {
		t.Errorf("appended %d rows for a delegated operator-agent token, want 0", n)
	}
}

// G4. TestGroomingDispositionsMissingScopeRejected: an operator token WITHOUT
// write:approvals.
//
// REACHABILITY (binding condition 2). The route is wrapped by
// requireRunAccount(memberWrite, …), whose cookie role-bounding applies only to
// a resolved OAuth cookie (SessionID != "" && TokenID == ""). This identity
// carries a TokenID, so the wrapper's write-tier branch returns early on
// ownership alone and the handler-local G4 rung is what refuses — which is what
// TestGroomingDispositionsScopeRungIsReachableThroughTheMux proves through the
// REAL mux rather than by direct handler call.
func TestGroomingDispositionsMissingScopeRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"),
		func(req *http.Request) *http.Request {
			return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
				Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"read:runs"},
			}))
		})
	requireGDError(t, w, http.StatusForbidden, "insufficient_scope")
	if n := len(gdRows(f.au)); n != 0 {
		t.Errorf("appended %d rows for an unscoped token, want 0", n)
	}
}

// TestGroomingDispositionsScopeRungIsReachableThroughTheMux is the layering
// proof binding condition 2 demands: the request goes through the REAL
// Handler(), so requireRunAccount(memberWrite, …) runs FIRST. If that wrapper
// refused with a different code, this test would report that code — and the
// handler-local G4 assertion above would be unreachable, i.e. pinned in
// appearance only. It asserts the shipped ordering yields exactly
// insufficient_scope.
func TestGroomingDispositionsScopeRungIsReachableThroughTheMux(t *testing.T) {
	f := newGDFixture(t, nil)
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+f.runID.String()+"/grooming-dispositions",
		strings.NewReader(gdBatch(f.ids.ordering, "approved")))
	req.Header.Set("Authorization", "Bearer "+f.unscopedBearer)
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	requireGDError(t, w, http.StatusForbidden, "insufficient_scope")
}

// G5a. TestGroomingDispositionsEmptyBatchRejected.
func TestGroomingDispositionsEmptyBatchRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	for _, body := range []string{`{"dispositions":[]}`, `{}`} {
		w := postGD(t, f.s, f.runID, body, gdOperator)
		requireGDError(t, w, http.StatusBadRequest, "validation_failed")
	}
}

// G5b. TestGroomingDispositionsIntraBatchDuplicateRejected. The duplicate is
// SELF-PAIRED — the same entry id twice — so the refusal cannot be produced by
// a byte-exact comparison against some other value.
func TestGroomingDispositionsIntraBatchDuplicateRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	body := fmt.Sprintf(
		`{"dispositions":[{"entry_id":%q,"verdict":"approved"},{"entry_id":%q,"verdict":"rejected"}]}`,
		f.ids.ordering, f.ids.ordering)
	w := postGD(t, f.s, f.runID, body, gdOperator)
	requireGDError(t, w, http.StatusBadRequest, "validation_failed")
	if n := len(gdRows(f.au)); n != 0 {
		t.Errorf("appended %d rows for an intra-batch duplicate, want 0", n)
	}
}

// G5c. An empty entry_id.
func TestGroomingDispositionsEmptyEntryIDRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	w := postGD(t, f.s, f.runID, `{"dispositions":[{"entry_id":"  ","verdict":"approved"}]}`, gdOperator)
	requireGDError(t, w, http.StatusBadRequest, "validation_failed")
}

// G5d. An unparseable body.
func TestGroomingDispositionsMalformedBodyRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	w := postGD(t, f.s, f.runID, `{"dispositions":`, gdOperator)
	requireGDError(t, w, http.StatusBadRequest, "validation_failed")
}

// G5e. TestGroomingDispositionsTrailingContentRejected pins the
// single-JSON-document rule.
//
// The case that matters is TWO CONCATENATED BATCHES: json.Decoder stops after
// the first value, so before the G5' check that body decoded the first batch,
// silently discarded the second, and returned 200 — a success response for a
// capture that recorded half of what the operator sent.
//
// The assertion is on COMMITTED STATE as well as identity: each refusal is
// followed by a read of the appended rows asserting ZERO, which is what proves
// the refusal precedes the write rather than merely renaming the response.
//
// TRAILING WHITESPACE MUST STILL BE ACCEPTED — a trailing newline is what every
// curl heredoc sends, and rejecting it would break every real caller to fix a
// synthetic one. That row is the regression this table most needs.
func TestGroomingDispositionsTrailingContentRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	one := gdBatch(f.ids.ordering, "approved")
	two := gdBatch(f.ids.hygiene, "rejected")

	for _, tc := range []struct {
		name     string
		body     string
		wantCode int
	}{
		// The silent-drop case: two valid batches, the second discarded.
		{"second valid object", one + two, http.StatusBadRequest},
		{"trailing garbage", one + " garbage-not-json", http.StatusBadRequest},
		{"trailing array", one + `[1,2,3]`, http.StatusBadRequest},
		// Accepted: whitespace only.
		{"trailing newline", one + "\n", http.StatusOK},
		{"trailing whitespace", one + " \t\r\n ", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(gdRows(f.au))
			w := postGD(t, f.s, f.runID, tc.body, gdOperator)
			if tc.wantCode != http.StatusOK {
				requireGDError(t, w, tc.wantCode, "validation_failed")
				// COMMITTED STATE: a refused body records nothing. The 400
				// bytes alone would not distinguish "refused before the write"
				// from "refused after one row leaked".
				if n := len(gdRows(f.au)) - before; n != 0 {
					t.Errorf("a refused body appended %d rows, want 0 — the refusal must precede the write", n)
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (trailing whitespace must be ACCEPTED); body: %s",
					w.Code, w.Body.String())
			}
			if n := len(gdRows(f.au)) - before; n != 1 {
				t.Errorf("an accepted body appended %d rows, want 1", n)
			}
		})
	}
}

// G6. TestGroomingDispositionsInvalidVerdictRejected.
func TestGroomingDispositionsInvalidVerdictRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	for _, v := range []string{"maybe", "", "APPROVED", "applied"} {
		t.Run(v, func(t *testing.T) {
			w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, v), gdOperator)
			requireGDError(t, w, http.StatusBadRequest, "grooming_verdict_invalid")
		})
	}
	if n := len(gdRows(f.au)); n != 0 {
		t.Errorf("appended %d rows for an invalid verdict, want 0", n)
	}
}

// TestGroomingDispositionsVerdictSetIsTheWorkmgmtDomain pins that the accepted
// set is exactly the three workmgmt verdicts — so capture and the (future,
// #2991) apply cannot drift on vocabulary.
func TestGroomingDispositionsVerdictSetIsTheWorkmgmtDomain(t *testing.T) {
	f := newGDFixture(t, nil)
	for _, v := range []workmgmt.GroomingVerdict{
		workmgmt.GroomingApproved, workmgmt.GroomingRejected, workmgmt.GroomingAmended,
	} {
		w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, string(v)), gdOperator)
		if w.Code != http.StatusOK {
			t.Errorf("verdict %q status = %d, want 200; body: %s", v, w.Code, w.Body.String())
		}
	}
}

// G7a. TestGroomingDispositionsNoReportRejected: the run genuinely carries no
// grooming_report artifact.
func TestGroomingDispositionsNoReportRejected(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s := New(Config{
		RunRepo:      &gdRunRepo{stages: []*run.Stage{{ID: stageID, RunID: runID, Type: run.StageTypePlan}}},
		ArtifactRepo: &gdArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{}},
		AuditRepo:    &auditFake{},
	})
	w := postGD(t, s, runID, gdBatch("ordering:github/acme/app#1", "approved"), gdOperator)
	requireGDError(t, w, http.StatusConflict, "grooming_report_absent")
}

// G7b. TestGroomingDispositionsReportUnparseableIs500 pins the absent-vs-
// unreadable DISTINCTION: a report that exists but cannot be parsed is a 500,
// not the 409 that says "there is nothing to disposition". Collapsing the two
// is how a capture would silently attach to the wrong report.
func TestGroomingDispositionsReportUnparseableIs500(t *testing.T) {
	f := newGDFixture(t, []byte(`{"kind":"grooming_report","report_version":"nope"}`))
	w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"), gdOperator)
	requireGDError(t, w, http.StatusInternalServerError, "internal_error")
	if strings.Contains(w.Body.String(), "grooming_report_absent") {
		t.Error("an UNREADABLE report was reported as ABSENT; the two states must stay distinct")
	}
}

// G7c. A stage-listing failure is a 500, not a 409.
func TestGroomingDispositionsStageListErrorIs500(t *testing.T) {
	s := New(Config{
		RunRepo:      &gdRunRepo{stagesErr: errors.New("stage list boom")},
		ArtifactRepo: &gdArtifactRepo{},
		AuditRepo:    &auditFake{},
	})
	w := postGD(t, s, uuid.New(), gdBatch("ordering:github/acme/app#1", "approved"), gdOperator)
	requireGDError(t, w, http.StatusInternalServerError, "internal_error")
}

// G7d. TestGroomingDispositionsArtifactListErrorIs500 pins the artifact-LIST
// failure branch: a stage whose artifact list cannot be read is a 500, never
// the 409 that says the run carries no report. An unreadable list is not an
// absent report, and collapsing the two would hand an operator "there is
// nothing to disposition" while the report sits unread (#2993).
//
// The injected error is a test-constructed sentinel, never obtained from the
// resolver, so a RED lands on the behavioral assertion and not on setup.
func TestGroomingDispositionsArtifactListErrorIs500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	au := &auditFake{}
	s := New(Config{
		RunRepo: &gdRunRepo{stages: []*run.Stage{
			{ID: stageID, RunID: runID, Type: run.StageTypePlan},
		}},
		ArtifactRepo: &gdArtifactRepo{listErr: errors.New("artifact list boom")},
		AuditRepo:    au,
	})

	w := postGD(t, s, runID, gdBatch("ordering:github/acme/app#1", "approved"), gdOperator)
	requireGDError(t, w, http.StatusInternalServerError, "internal_error")
	if strings.Contains(w.Body.String(), "grooming_report_absent") {
		t.Error("an UNREADABLE artifact list was reported as an ABSENT report; the two states must stay distinct")
	}
	// Committed state, read after the call returns: a regression that fell
	// through to the write path is caught here even if the error identity
	// happened to match.
	if rows := gdRows(au); len(rows) != 0 {
		t.Errorf("appended %d disposition rows on an unreadable artifact list, want 0: %+v", len(rows), rows)
	}

	// BOTH verbs resolve through newestGroomingReportArtifact. The read-back is
	// where a collapsed error would report the report as absent when it is
	// merely unreadable, so it gets its own assertion.
	g := getGD(t, s, runID)
	requireGDError(t, g, http.StatusInternalServerError, "internal_error")
	if strings.Contains(g.Body.String(), "grooming_report_absent") {
		t.Error("GET reported an UNREADABLE artifact list as an ABSENT report")
	}
}

// gdTieBreakFixture builds two grooming_report artifacts with BYTE-EQUAL
// CreatedAt and different ids. The artifact with the lexicographically GREATER
// id string is the one newerGroomingArtifact selects, so it carries the report
// that DROPS the Duplicates class — making WHICH report won observable through
// the API rather than through internals.
//
// The ordering is COMPUTED from the two id strings, never assumed from
// generation order: uuid.New() gives no ordering guarantee, and assuming one
// makes the test pass and fail at random.
type gdTieBreakFixture struct {
	winner, loser *artifact.Artifact
	winnerStage   uuid.UUID
	loserStage    uuid.UUID
	ids           groomingApplyEntryIDs
	byStage       map[uuid.UUID][]*artifact.Artifact
}

func newGDTieBreakFixture(t *testing.T) *gdTieBreakFixture {
	t.Helper()
	// Equal to the byte: one shared time value, not two constructions of it.
	created := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	full, ids := groomingApplyFullReport()
	fullBody := groomingApplyReportJSON(t, full)

	trimmed, _ := groomingApplyFullReport()
	trimmed.Duplicates = []plan.DuplicateCandidate{}
	trimmedBody := groomingApplyReportJSON(t, trimmed)

	stageA, stageB := uuid.New(), uuid.New()
	idA, idB := uuid.New(), uuid.New()
	for idA.String() == idB.String() {
		idB = uuid.New()
	}
	// The GREATER id string wins (grooming_dispositions.go: the equal-CreatedAt
	// branch is `candidate.ID.String() > incumbent.ID.String()`).
	winnerID, winnerStage, loserID, loserStage := idA, stageA, idB, stageB
	if idB.String() > idA.String() {
		winnerID, winnerStage, loserID, loserStage = idB, stageB, idA, stageA
	}

	// The winner drops the Duplicates class; the loser keeps it. So the
	// duplicate entry id exists ONLY on the LOSER.
	winner := &artifact.Artifact{
		ID: winnerID, StageID: winnerStage, Kind: artifact.KindGroomingReport,
		Content: trimmedBody, ContentHash: sha256Hex(trimmedBody), CreatedAt: created,
	}
	loser := &artifact.Artifact{
		ID: loserID, StageID: loserStage, Kind: artifact.KindGroomingReport,
		Content: fullBody, ContentHash: sha256Hex(fullBody), CreatedAt: created,
	}
	return &gdTieBreakFixture{
		winner: winner, loser: loser,
		winnerStage: winnerStage, loserStage: loserStage, ids: ids,
		byStage: map[uuid.UUID][]*artifact.Artifact{
			winnerStage: {winner}, loserStage: {loser},
		},
	}
}

// TestGroomingDispositionsEqualCreatedAtTieBreakIsTotal pins the id-string
// tiebreak in newerGroomingArtifact: two reports written in the same clock tick
// must still order stably, and the SAME one must win regardless of the order
// the repository returns the stages in (#2994).
//
// The reversed-stage-order arm is what makes this a totality test rather than
// one lucky arrangement: a resolver whose selection depended on traversal order
// passes one arm and fails the other.
func TestGroomingDispositionsEqualCreatedAtTieBreakIsTotal(t *testing.T) {
	f := newGDTieBreakFixture(t)

	arms := []struct {
		name   string
		stages []uuid.UUID
	}{
		{"winner_first", []uuid.UUID{f.winnerStage, f.loserStage}},
		{"winner_second", []uuid.UUID{f.loserStage, f.winnerStage}},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			runID := uuid.New()
			s := New(Config{
				RunRepo: &gdRunRepo{stages: []*run.Stage{
					{ID: arm.stages[0], RunID: runID, Type: run.StageTypePlan},
					{ID: arm.stages[1], RunID: runID, Type: run.StageTypePlan},
				}},
				ArtifactRepo: &gdArtifactRepo{byStage: f.byStage},
				AuditRepo:    &auditFake{},
			})

			// The duplicate entry exists ONLY on the LOSING report, so a
			// capture that attached to the loser would 200 here. It must 422.
			w := postGD(t, s, runID, gdBatch(f.ids.duplicate, "approved"), gdOperator)
			requireGDError(t, w, http.StatusUnprocessableEntity, "grooming_entry_unknown")

			// An entry on the WINNING report is accepted and the response names
			// the greater-id artifact.
			w = postGD(t, s, runID, gdBatch(f.ids.ordering, "approved"), gdOperator)
			out := decodeGDResponse(t, w)
			if out.ArtifactID != f.winner.ID.String() {
				t.Errorf("capture attached to artifact %s, want the GREATER-id %s (loser %s)",
					out.ArtifactID, f.winner.ID, f.loser.ID)
			}
			if out.StageID != f.winnerStage.String() {
				t.Errorf("capture stage = %s, want the winning report's stage %s", out.StageID, f.winnerStage)
			}

			// The read-back resolves through the same function and must agree.
			got := decodeGDResponse(t, getGD(t, s, runID))
			if got.ArtifactID != f.winner.ID.String() {
				t.Errorf("read-back artifact = %s, want the GREATER-id %s", got.ArtifactID, f.winner.ID)
			}
		})
	}
}

// G8. TestGroomingDispositionsUnknownEntryRejected. The unknown id is a freshly
// generated random string — definitionally not a derived id — so the fixture
// never consults the control it is testing.
func TestGroomingDispositionsUnknownEntryRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	unknown := "ordering:github/acme/app#" + uuid.NewString()
	w := postGD(t, f.s, f.runID, gdBatch(unknown, "approved"), gdOperator)
	requireGDError(t, w, http.StatusUnprocessableEntity, "grooming_entry_unknown")
	if !strings.Contains(w.Body.String(), unknown) {
		t.Errorf("details do not name the unknown id %q; body: %s", unknown, w.Body.String())
	}
}

// --- happy path + projection ------------------------------------------------

func decodeGDResponse(t *testing.T, w *httptest.ResponseRecorder) groomingDispositionsResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var out groomingDispositionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, w.Body.String())
	}
	return out
}

// TestGroomingDispositionAuditCategoryDistinct is the DONE-MEANS distinctness
// test: the appended row's category string is literally
// "grooming_disposition_recorded", that string IS registered in
// audit.KnownCategories, and it is NOT grooming_mutation_applied. A no-op edit
// to audit/categories.go fails the registry half here, where a scope-presence
// gate would pass.
func TestGroomingDispositionAuditCategoryDistinct(t *testing.T) {
	f := newGDFixture(t, nil)
	w := postGD(t, f.s, f.runID, gdBatch(f.ids.hygiene, "approved"), gdOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	rows := gdRows(f.au)
	if len(rows) != 1 {
		t.Fatalf("appended %d rows, want 1", len(rows))
	}
	if rows[0].Category != "grooming_disposition_recorded" {
		t.Errorf("category = %q, want the literal %q", rows[0].Category, "grooming_disposition_recorded")
	}
	if rows[0].Category == workmgmt.GroomingMutationAppliedCategory {
		t.Error("a disposition was recorded under the APPLY category; what the operator DECIDED and what was APPLIED are distinct facts")
	}
	if !audit.IsKnownCategory(CategoryGroomingDispositionRecorded) {
		t.Errorf("%q is absent from audit.KnownCategories; the stream would 400 on ?category=", CategoryGroomingDispositionRecorded)
	}
	// No apply-family row was written by capture.
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	for _, a := range f.au.appended {
		if a.Category == workmgmt.GroomingMutationAppliedCategory {
			t.Error("capture wrote a grooming_mutation_applied row; capture applies nothing")
		}
	}
}

// TestGroomingDispositionsPayloadCarriesEntryClassAndArtifact pins the recorded
// payload shape: the class comes from the report's own routing, and the
// resolved artifact id + content hash ride along so a later consumer (#2991)
// can tell WHICH report a capture attached to.
func TestGroomingDispositionsPayloadCarriesEntryClassAndArtifact(t *testing.T) {
	f := newGDFixture(t, nil)
	body := fmt.Sprintf(
		`{"dispositions":[{"entry_id":%q,"verdict":"rejected","close_target":"acme/app#12"}]}`,
		f.ids.duplicate)
	w := postGD(t, f.s, f.runID, body, gdOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	rows := gdRows(f.au)
	if len(rows) != 1 {
		t.Fatalf("appended %d rows, want 1", len(rows))
	}
	var got groomingDispositionPayload
	if err := json.Unmarshal(rows[0].Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.EntryID != f.ids.duplicate {
		t.Errorf("entry_id = %q, want %q", got.EntryID, f.ids.duplicate)
	}
	if got.EntryClass != plan.GroomingClassDuplicate {
		t.Errorf("entry_class = %q, want %q", got.EntryClass, plan.GroomingClassDuplicate)
	}
	if got.Verdict != string(workmgmt.GroomingRejected) {
		t.Errorf("verdict = %q, want %q", got.Verdict, workmgmt.GroomingRejected)
	}
	if got.CloseTarget != "acme/app#12" {
		t.Errorf("close_target = %q, want acme/app#12", got.CloseTarget)
	}
	if got.ArtifactID != f.art.ID.String() || got.ContentHash != f.art.ContentHash {
		t.Errorf("payload artifact = (%s,%s), want (%s,%s)",
			got.ArtifactID, got.ContentHash, f.art.ID, f.art.ContentHash)
	}
	if rows[0].StageID == nil || *rows[0].StageID != f.stageID {
		t.Errorf("row stage_id = %v, want the report stage %s", rows[0].StageID, f.stageID)
	}
	if rows[0].ActorSubject == nil || *rows[0].ActorSubject != "github:ops" {
		t.Errorf("actor_subject = %v, want github:ops", rows[0].ActorSubject)
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorUser {
		t.Errorf("actor_kind = %v, want user", rows[0].ActorKind)
	}
}

// TestGroomingDispositionsNewestArtifactWins seeds TWO grooming_report
// artifacts with different CreatedAt on the same run and asserts BOTH capture
// and read-back attach to the NEWER one. This is the falsifier for the
// newest-artifact design decision.
func TestGroomingDispositionsNewestArtifactWins(t *testing.T) {
	runID, oldStage, newStage := uuid.New(), uuid.New(), uuid.New()
	report, ids := groomingApplyFullReport()
	oldBody := groomingApplyReportJSON(t, report)

	// The newer report drops one class, so "which report" is observable: its id
	// set differs from the older one's.
	newReport, _ := groomingApplyFullReport()
	newReport.Duplicates = []plan.DuplicateCandidate{}
	newBody := groomingApplyReportJSON(t, newReport)

	oldArt := &artifact.Artifact{
		ID: uuid.New(), StageID: oldStage, Kind: artifact.KindGroomingReport,
		Content: oldBody, ContentHash: sha256Hex(oldBody),
		CreatedAt: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
	}
	newArt := &artifact.Artifact{
		ID: uuid.New(), StageID: newStage, Kind: artifact.KindGroomingReport,
		Content: newBody, ContentHash: sha256Hex(newBody),
		CreatedAt: time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC),
	}
	au := &auditFake{}
	s := New(Config{
		RunRepo: &gdRunRepo{stages: []*run.Stage{
			{ID: oldStage, RunID: runID, Type: run.StageTypePlan},
			{ID: newStage, RunID: runID, Type: run.StageTypePlan},
		}},
		ArtifactRepo: &gdArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{
			oldStage: {oldArt}, newStage: {newArt},
		}},
		AuditRepo: au,
	})

	// The duplicate entry exists ONLY on the OLDER report, so a capture that
	// attached to the older artifact would 200 here. It must 422.
	w := postGD(t, s, runID, gdBatch(ids.duplicate, "approved"), gdOperator)
	requireGDError(t, w, http.StatusUnprocessableEntity, "grooming_entry_unknown")

	// An entry present on the NEWER report is accepted, and the response names
	// the newer artifact.
	w = postGD(t, s, runID, gdBatch(ids.ordering, "approved"), gdOperator)
	out := decodeGDResponse(t, w)
	if out.ArtifactID != newArt.ID.String() {
		t.Errorf("capture attached to artifact %s, want the NEWER %s", out.ArtifactID, newArt.ID)
	}
	if out.StageID != newStage.String() {
		t.Errorf("capture stage = %s, want the newer report's stage %s", out.StageID, newStage)
	}
	// The read-back agrees by construction (same resolver).
	got := decodeGDResponse(t, getGD(t, s, runID))
	if got.ArtifactID != newArt.ID.String() {
		t.Errorf("read-back artifact = %s, want the NEWER %s", got.ArtifactID, newArt.ID)
	}
}

// TestProjectGroomingDispositions_SkipsUndecodableAndForeignRows pins the two
// tolerant-projection branches directly: an undecodable payload contributes NO
// disposition (the fail-safe direction), and a row recorded against a DIFFERENT
// artifact never leaks into this artifact's read-back.
func TestProjectGroomingDispositions_SkipsUndecodableAndForeignRows(t *testing.T) {
	const artID = "11111111-1111-1111-1111-111111111111"
	seq := int64(0)
	entry := func(payload string) *audit.Entry {
		seq++
		return &audit.Entry{Sequence: seq, Payload: json.RawMessage(payload), Timestamp: time.Now().UTC()}
	}
	entries := []*audit.Entry{
		entry(`not json at all`),
		entry(`{"artifact_id":"` + artID + `","entry_id":"","verdict":"approved"}`),
		// The foreign row carries a DISTINCT entry_id on purpose: sharing
		// "ordering:a" would let the last-wins collapse absorb it, and the case
		// would stay green with the artifact filter deleted.
		entry(`{"artifact_id":"22222222-2222-2222-2222-222222222222","entry_id":"duplicate:foreign","verdict":"approved"}`),
		entry(`{"artifact_id":"` + artID + `","entry_id":"ordering:a","entry_class":"ordering","verdict":"rejected"}`),
		nil,
	}
	got := projectGroomingDispositions(entries, artID, map[string]string{"ordering:a": "ordering"})
	if len(got) != 1 {
		t.Fatalf("projected %d dispositions %+v, want exactly 1", len(got), got)
	}
	if got[0].EntryID != "ordering:a" || got[0].Verdict != "rejected" {
		t.Errorf("projected %+v, want the one decodable, same-artifact row", got[0])
	}
	for _, d := range got {
		if d.EntryID == "duplicate:foreign" {
			t.Error("a disposition recorded against a DIFFERENT artifact leaked into this artifact's read-back")
		}
	}
}

// --- POSTGRES-BACKED cross-boundary + committed-state tests ------------------

// gdPGFixture is a real Postgres run + plan stage + ingested grooming_report.
type gdPGFixture struct {
	s         *Server
	runID     uuid.UUID
	stageID   uuid.UUID
	artID     uuid.UUID
	ids       groomingApplyEntryIDs
	auditRepo audit.Repository
	// runRepo / artRepo are exposed so a test can re-wire a SECOND server over
	// the same Postgres state with a decorated audit repository (the mid-batch
	// append-failure injection below).
	runRepo run.Repository
	artRepo artifact.Repository
}

// newGDPGFixture wires real repositories against a shared testcontainers
// Postgres, seeds a run + plan stage, and ingests a genuine grooming_report
// artifact. reports lets a caller seed additional artifacts.
func newGDPGFixture(t *testing.T) *gdPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: groomingApplyRepo, WorkflowID: "backlog_grooming", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create groom stage: %v", err)
	}
	report, ids := groomingApplyFullReport()
	body := groomingApplyReportJSON(t, report)
	sv := plan.GroomingReportVersion
	art, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindGroomingReport,
		SchemaVersion: &sv, Content: body, ContentHash: sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("create grooming_report artifact: %v", err)
	}
	s := New(Config{RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo})
	return &gdPGFixture{
		s: s, runID: rn.ID, stageID: stage.ID, artID: art.ID, ids: ids,
		auditRepo: auditRepo, runRepo: runRepo, artRepo: artRepo,
	}
}

// pgDispositionRows reads the persisted grooming_disposition_recorded rows.
func (f *gdPGFixture) pgDispositionRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := f.auditRepo.ListForRunByCategory(context.Background(), f.runID, CategoryGroomingDispositionRecorded)
	if err != nil {
		t.Fatalf("list disposition rows: %v", err)
	}
	return rows
}

// TestRecordGroomingDispositionsEndToEnd is the CROSS-BOUNDARY done-means: a
// real Postgres run/stage/artifact/audit stack, a multi-entry POST over HTTP,
// and assertions on PERSISTED STATE — not on the response body alone.
func TestRecordGroomingDispositionsEndToEnd(t *testing.T) {
	f := newGDPGFixture(t)
	body := fmt.Sprintf(`{"dispositions":[
	  {"entry_id":%q,"verdict":"approved"},
	  {"entry_id":%q,"verdict":"rejected","close_target":"kuhlman-labs/fishhawk#16"},
	  {"entry_id":%q,"verdict":"amended"}
	]}`, f.ids.hygiene, f.ids.duplicate, f.ids.ordering)

	w := postGD(t, f.s, f.runID, body, gdOperator)
	out := decodeGDResponse(t, w)
	if out.ArtifactID != f.artID.String() || out.StageID != f.stageID.String() {
		t.Errorf("response names artifact %s / stage %s, want %s / %s",
			out.ArtifactID, out.StageID, f.artID, f.stageID)
	}

	// (1) Exactly three chained rows landed with the expected payloads.
	rows := f.pgDispositionRows(t)
	if len(rows) != 3 {
		t.Fatalf("persisted %d grooming_disposition_recorded rows, want 3", len(rows))
	}
	want := map[string]groomingDispositionPayload{
		f.ids.hygiene:   {EntryID: f.ids.hygiene, EntryClass: plan.GroomingClassHygiene, Verdict: "approved"},
		f.ids.duplicate: {EntryID: f.ids.duplicate, EntryClass: plan.GroomingClassDuplicate, Verdict: "rejected", CloseTarget: "kuhlman-labs/fishhawk#16"},
		f.ids.ordering:  {EntryID: f.ids.ordering, EntryClass: plan.GroomingClassOrdering, Verdict: "amended"},
	}
	for _, row := range rows {
		var got groomingDispositionPayload
		if err := json.Unmarshal(row.Payload, &got); err != nil {
			t.Fatalf("decode persisted payload: %v", err)
		}
		exp, ok := want[got.EntryID]
		if !ok {
			t.Errorf("persisted an unexpected entry_id %q", got.EntryID)
			continue
		}
		if got.EntryClass != exp.EntryClass || got.Verdict != exp.Verdict || got.CloseTarget != exp.CloseTarget {
			t.Errorf("persisted %+v, want class %q verdict %q close_target %q",
				got, exp.EntryClass, exp.Verdict, exp.CloseTarget)
		}
		if got.ArtifactID != f.artID.String() {
			t.Errorf("persisted artifact_id = %q, want %q", got.ArtifactID, f.artID)
		}
		delete(want, got.EntryID)
	}
	if len(want) != 0 {
		t.Errorf("these entries never persisted: %v", want)
	}

	// (2) ZERO grooming_mutation_applied rows — the categories are distinct
	// facts and capture applies NOTHING (done-means).
	applied, err := f.auditRepo.ListForRunByCategory(context.Background(), f.runID, workmgmt.GroomingMutationAppliedCategory)
	if err != nil {
		t.Fatalf("list apply rows: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("capture wrote %d grooming_mutation_applied rows, want 0", len(applied))
	}

	// (3) The GET read-back returns the same set.
	got := decodeGDResponse(t, getGD(t, f.s, f.runID))
	if len(got.Dispositions) != 3 {
		t.Fatalf("read-back returned %d dispositions, want 3: %+v", len(got.Dispositions), got.Dispositions)
	}
	byEntry := map[string]recordedGroomingDisposition{}
	for _, d := range got.Dispositions {
		byEntry[d.EntryID] = d
	}
	if byEntry[f.ids.duplicate].Verdict != "rejected" ||
		byEntry[f.ids.duplicate].CloseTarget != "kuhlman-labs/fishhawk#16" ||
		byEntry[f.ids.duplicate].EntryClass != plan.GroomingClassDuplicate {
		t.Errorf("read-back duplicate row = %+v, want the rejected close", byEntry[f.ids.duplicate])
	}
	if byEntry[f.ids.hygiene].RecordedBy != "github:ops" {
		t.Errorf("read-back recorded_by = %q, want github:ops", byEntry[f.ids.hygiene].RecordedBy)
	}
}

// TestGroomingDispositionsReadBackLastWins pins the supersession rule: a
// re-captured entry reads back with the LATER verdict while BOTH audit rows
// remain in the chain, so the supersession is itself auditable.
func TestGroomingDispositionsReadBackLastWins(t *testing.T) {
	f := newGDPGFixture(t)
	if w := postGD(t, f.s, f.runID, gdBatch(f.ids.hygiene, "approved"), gdOperator); w.Code != http.StatusOK {
		t.Fatalf("first capture status = %d: %s", w.Code, w.Body.String())
	}
	if w := postGD(t, f.s, f.runID, gdBatch(f.ids.hygiene, "rejected"), gdOperator); w.Code != http.StatusOK {
		t.Fatalf("second capture status = %d: %s", w.Code, w.Body.String())
	}
	if rows := f.pgDispositionRows(t); len(rows) != 2 {
		t.Fatalf("chain holds %d rows, want BOTH captures (2) — the supersession must stay auditable", len(rows))
	}
	got := decodeGDResponse(t, getGD(t, f.s, f.runID))
	if len(got.Dispositions) != 1 {
		t.Fatalf("read-back returned %d dispositions, want 1 collapsed row: %+v", len(got.Dispositions), got.Dispositions)
	}
	if got.Dispositions[0].Verdict != "rejected" {
		t.Errorf("read-back verdict = %q, want the LATER %q", got.Dispositions[0].Verdict, "rejected")
	}
}

// TestGroomingDispositionsUnknownEntryLeavesNoRows is the COMMITTED-STATE
// counterfactual vehicle for the batch-atomic validation loop.
//
// It is deliberately NOT an error-identity test: the 422 bytes are IDENTICAL
// whether or not the first disposition leaked into the chain, so an
// error-identity assertion would stay green with the validate-all-before-append
// ordering deleted. This reads the persisted rows AFTER the call returns and
// asserts ZERO.
func TestGroomingDispositionsUnknownEntryLeavesNoRows(t *testing.T) {
	f := newGDPGFixture(t)
	unknown := "ordering:github/acme/app#" + uuid.NewString()
	body := fmt.Sprintf(
		`{"dispositions":[{"entry_id":%q,"verdict":"approved"},{"entry_id":%q,"verdict":"approved"}]}`,
		f.ids.hygiene, unknown)
	w := postGD(t, f.s, f.runID, body, gdOperator)
	requireGDError(t, w, http.StatusUnprocessableEntity, "grooming_entry_unknown")

	rows := f.pgDispositionRows(t)
	if len(rows) != 0 {
		t.Fatalf("a refused batch left %d rows in the chain, want 0 — validation must complete for the WHOLE batch before ANY append", len(rows))
	}
}

// gdFailingAudit fails the k-th grooming_disposition_recorded AppendChained and
// PASSES EVERY OTHER CALL THROUGH to the real repository, so the rows before
// the failure genuinely land in Postgres. That pass-through is the point: the
// branch under test is the one place the change knowingly weakens its own
// "a partially-recorded capture is unreachable" claim, and only durable rows
// can prove the reported count against what actually persisted.
type gdFailingAudit struct {
	audit.Repository
	failAt int // 1-based index among disposition appends; 0 never fails
	calls  int
}

func (f *gdFailingAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategoryGroomingDispositionRecorded {
		f.calls++
		if f.calls == f.failAt {
			return nil, fmt.Errorf("gdFailingAudit: injected append failure on disposition %d", f.calls)
		}
	}
	return f.Repository.AppendChained(ctx, p)
}

// gdErrorDetails decodes the error envelope's details map.
func gdErrorDetails(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, w.Body.String())
	}
	return env.Error.Details
}

// TestGroomingDispositionsMidBatchAppendFailure pins the ONE branch where the
// batch-atomic guarantee is knowingly weakened: an AppendChained failure PART
// WAY through the batch, after earlier rows have durably landed.
//
// Three things are asserted, and the third is the one that matters:
//
//  1. the response is 500 internal_error;
//  2. details.recorded / details.requested carry the counts;
//  3. details.recorded EQUALS the number of rows that actually persisted —
//     a body claiming 2 while 3 landed is worse than the failure itself, and
//     that count is the operator's only evidence of what survived.
//
// Then the DOCUMENTED RECOVERY is exercised: the same batch is re-POSTed
// against a healthy server and the read-back resolves last-wins to the retried
// verdicts, which is what makes the error message's "a repeat POST is safe"
// claim true rather than aspirational.
func TestGroomingDispositionsMidBatchAppendFailure(t *testing.T) {
	f := newGDPGFixture(t)
	// Fail the THIRD disposition: two rows land, one does not.
	failing := &gdFailingAudit{Repository: f.auditRepo, failAt: 3}
	sick := New(Config{RunRepo: f.runRepo, ArtifactRepo: f.artRepo, AuditRepo: failing})

	body := fmt.Sprintf(`{"dispositions":[
	  {"entry_id":%q,"verdict":"approved"},
	  {"entry_id":%q,"verdict":"approved"},
	  {"entry_id":%q,"verdict":"approved"}
	]}`, f.ids.hygiene, f.ids.duplicate, f.ids.ordering)

	w := postGD(t, sick, f.runID, body, gdOperator)
	requireGDError(t, w, http.StatusInternalServerError, "internal_error")

	details := gdErrorDetails(t, w)
	recorded, okR := details["recorded"].(float64)
	requested, okQ := details["requested"].(float64)
	if !okR || !okQ {
		t.Fatalf("details missing recorded/requested counts: %v", details)
	}
	if int(requested) != 3 {
		t.Errorf("details.requested = %v, want 3", requested)
	}
	if int(recorded) != 2 {
		t.Errorf("details.recorded = %v, want 2 (the two appends that preceded the injected failure)", recorded)
	}

	// (3) The reported count must match DURABLE STATE, not the loop counter's
	// intent. Read the rows back from Postgres.
	rows := f.pgDispositionRows(t)
	if len(rows) != int(recorded) {
		t.Fatalf("details.recorded = %d but %d rows actually persisted; the reported count is the operator's only evidence of what survived",
			int(recorded), len(rows))
	}
	landed := map[string]string{}
	for _, row := range rows {
		var got groomingDispositionPayload
		if err := json.Unmarshal(row.Payload, &got); err != nil {
			t.Fatalf("decode persisted payload: %v", err)
		}
		landed[got.EntryID] = got.Verdict
	}
	if _, ok := landed[f.ids.ordering]; ok {
		t.Error("the disposition whose append FAILED persisted anyway")
	}
	for _, id := range []string{f.ids.hygiene, f.ids.duplicate} {
		if landed[id] != "approved" {
			t.Errorf("entry %q did not durably land before the failure (got %q)", id, landed[id])
		}
	}

	// RECOVERY: re-POST the whole batch with CORRECTING verdicts against a
	// healthy server. Capture is last-wins, so the retry resolves cleanly and
	// the partial capture leaves no stuck state.
	retry := fmt.Sprintf(`{"dispositions":[
	  {"entry_id":%q,"verdict":"rejected"},
	  {"entry_id":%q,"verdict":"rejected"},
	  {"entry_id":%q,"verdict":"rejected"}
	]}`, f.ids.hygiene, f.ids.duplicate, f.ids.ordering)
	if w := postGD(t, f.s, f.runID, retry, gdOperator); w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if n := len(f.pgDispositionRows(t)); n != 5 {
		t.Errorf("chain holds %d rows, want 5 (2 partial + 3 retried) — every capture stays auditable", n)
	}
	got := decodeGDResponse(t, getGD(t, f.s, f.runID))
	if len(got.Dispositions) != 3 {
		t.Fatalf("read-back returned %d dispositions, want 3: %+v", len(got.Dispositions), got.Dispositions)
	}
	for _, d := range got.Dispositions {
		if d.Verdict != "rejected" {
			t.Errorf("entry %q reads back as %q, want the RETRIED %q", d.EntryID, d.Verdict, "rejected")
		}
	}
}

// TestGroomingDispositionsReadBackListErrorIs500 pins the
// ListForRunByCategory failure branch in respondGroomingDispositions — the
// read that BOTH verbs share. The append succeeds; only the projection read
// fails.
func TestGroomingDispositionsReadBackListErrorIs500(t *testing.T) {
	f := newGDFixture(t, nil)
	f.au.listByCategoryErr = errors.New("chain read boom")
	w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"), gdOperator)
	requireGDError(t, w, http.StatusInternalServerError, "internal_error")
	// The capture itself LANDED — the failure is on the read-back, and the
	// distinction matters to an operator deciding whether to retry.
	if n := len(gdRows(f.au)); n != 1 {
		t.Errorf("appended %d rows, want 1 — the append precedes the failing read-back", n)
	}
	requireGDError(t, getGD(t, f.s, f.runID), http.StatusInternalServerError, "internal_error")
}

// TestGroomingDispositionsGetInvalidRunIDRejected pins the GET handler's
// run_id parse. The POST's parse is covered by the ladder; this one had no
// case at all.
func TestGroomingDispositionsGetInvalidRunIDRejected(t *testing.T) {
	f := newGDFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/not-a-uuid/grooming-dispositions", nil)
	req.SetPathValue("run_id", "not-a-uuid")
	w := httptest.NewRecorder()
	f.s.handleListGroomingDispositions(w, gdOperator(req))
	requireGDError(t, w, http.StatusBadRequest, "validation_failed")
}

// --- route registration -----------------------------------------------------

// TestGroomingDispositionRoutesRegistered guards the route table: both verbs
// must reach their handlers through the mux. With no repositories wired each
// handler returns 503 grooming_dispositions_unconfigured — an UNregistered
// route would 404 with the default not-found body, so a 503 here proves the
// wiring in handlers.go.
func TestGroomingDispositionRoutesRegistered(t *testing.T) {
	s := New(Config{})
	path := "/v0/runs/00000000-0000-0000-0000-000000000000/grooming-dispositions"
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, path, bytes.NewReader([]byte(`{}`)))
			s.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s returned 404 — route not registered in handlers.go", method, path)
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (routed, repositories unwired); body: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "grooming_dispositions_unconfigured") {
				t.Errorf("body = %s, want grooming_dispositions_unconfigured", rec.Body.String())
			}
		})
	}
}
