package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// upkeep_dispositions_test.go pins POST/GET /v0/runs/{run_id}/upkeep-dispositions
// (#3923) against fakes: one test per named rung, each asserting the status,
// the code AND that no upkeep_disposition_recorded row was appended (committed
// state, not just the error). Bad-state fixtures are seeded BY CONSTRUCTION.
//
// The fake audit repo does NOT carry audit.UpkeepWindowAppender, so these
// tests drive the handler's FALLBACK append path; the atomic path is pinned
// against real Postgres in upkeep_dispositions_pg_test.go.

// The example report's three finding ids (docs/spec/examples).
const (
	ukFlake  = "flake:TestWidgetSync"
	ukDrift  = "toolchain_drift:go"
	ukDeprec = "deprecation:io/ioutil"
)

// --- fakes ------------------------------------------------------------------

// ukRunRepo knows exactly one run.
type ukRunRepo struct {
	run.BaseFake
	runID  uuid.UUID
	getErr error
}

func (f *ukRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if id != f.runID {
		return nil, run.ErrNotFound
	}
	return &run.Run{ID: id}, nil
}

// ukArtifactRepo serves artifacts by id.
type ukArtifactRepo struct {
	gdArtifactRepo
	byID map[uuid.UUID]*artifact.Artifact
}

func (f *ukArtifactRepo) Get(_ context.Context, id uuid.UUID) (*artifact.Artifact, error) {
	if a, ok := f.byID[id]; ok {
		return a, nil
	}
	return nil, artifact.ErrNotFound
}

// ukAudit is a sequence-assigning in-memory chain (the shared auditFake does
// not assign sequences, and the binding rule and last-wins key on them).
type ukAudit struct {
	*auditFake
	mu      sync.Mutex
	entries []*audit.Entry
	next    int64
}

func newUKAudit() *ukAudit { return &ukAudit{auditFake: &auditFake{}} }

func (a *ukAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	rid := p.RunID
	e := &audit.Entry{
		ID: uuid.New(), RunID: &rid, StageID: p.StageID, Sequence: a.next,
		Timestamp: p.Timestamp, Category: p.Category, ActorKind: p.ActorKind,
		ActorSubject: p.ActorSubject, Payload: p.Payload,
	}
	a.entries = append(a.entries, e)
	return e, nil
}

func (a *ukAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *ukAudit) count(category string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.entries {
		if e.Category == category {
			n++
		}
	}
	return n
}

// --- harness ----------------------------------------------------------------

// ukReportBody renders the example report restricted to the given finding ids.
func ukReportBody(t *testing.T, ids ...string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(upkeepExampleBody(t), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var kept []any
	for _, f := range doc["findings"].([]any) {
		if want[f.(map[string]any)["id"].(string)] {
			kept = append(kept, f)
		}
	}
	doc["findings"] = kept
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type ukFixture struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
	arts    *ukArtifactRepo
	runs    *ukRunRepo
	au      *ukAudit
	art     *artifact.Artifact
}

// addReport stores an upkeep_report artifact with the given findings; when
// record is true it also appends the upkeep_report_recorded row naming it.
func (f *ukFixture) addReport(t *testing.T, record bool, ids ...string) *artifact.Artifact {
	t.Helper()
	body := ukReportBody(t, ids...)
	a := &artifact.Artifact{
		ID: uuid.New(), StageID: f.stageID, Kind: artifact.KindUpkeepReport,
		Content: body, ContentHash: sha256Hex(body), CreatedAt: time.Now().UTC(),
	}
	f.arts.byID[a.ID] = a
	if record {
		f.recordRow(t, map[string]any{"run_id": f.runID.String(), "artifact_id": a.ID.String()})
	}
	return a
}

func (f *ukFixture) recordRow(t *testing.T, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(), Category: CategoryUpkeepReportRecorded, Payload: raw,
	}); err != nil {
		t.Fatal(err)
	}
}

// newUKFixture wires a server whose run carries ONE recorded three-finding
// report.
func newUKFixture(t *testing.T) *ukFixture {
	t.Helper()
	f := &ukFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit()}
	f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
	f.runs = &ukRunRepo{runID: f.runID}
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
	f.art = f.addReport(t, true, ukFlake, ukDrift, ukDeprec)
	return f
}

func ukOperator(req *http.Request) *http.Request { return gdOperator(req) }

func ukWithIdentity(id Identity) func(*http.Request) *http.Request {
	return func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}
}

func postUK(t *testing.T, s *Server, runID string, raw string, withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/upkeep-dispositions", strings.NewReader(raw))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	s.handleRecordUpkeepDispositions(w, withID(req))
	return w
}

func getUK(t *testing.T, s *Server, runID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID+"/upkeep-dispositions", nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	s.handleListUpkeepDispositions(w, req)
	return w
}

func ukBatch(entries ...string) string {
	return `{"dispositions":[` + strings.Join(entries, ",") + `]}`
}

func ukEntry(findingID, verdict string) string {
	return fmt.Sprintf(`{"finding_id":%q,"verdict":%q}`, findingID, verdict)
}

func decodeUK(t *testing.T, w *httptest.ResponseRecorder) upkeepDispositionsResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var out upkeepDispositionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// requireUKRefused asserts status + code AND zero committed disposition rows.
func requireUKRefused(t *testing.T, f *ukFixture, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	requireGDError(t, w, status, code)
	if n := f.au.count(CategoryUpkeepDispositionRecorded); n != 0 {
		t.Fatalf("committed upkeep_disposition_recorded rows = %d, want 0 on a %s refusal", n, code)
	}
}

// --- U0..U11 ----------------------------------------------------------------

func TestUpkeepDispositions_U0_Unconfigured(t *testing.T) {
	s := New(Config{})
	w := postUK(t, s, uuid.NewString(), ukBatch(ukEntry(ukFlake, "approved")), ukOperator)
	requireGDError(t, w, http.StatusServiceUnavailable, "upkeep_dispositions_unconfigured")
	g := getUK(t, s, uuid.NewString())
	requireGDError(t, g, http.StatusServiceUnavailable, "upkeep_dispositions_unconfigured")
}

// TestUpkeepDispositions_U1toU4_OperatorOnly: anonymous, a run-bound agent
// token (even for its OWN run), a delegated operator-agent token, and a token
// missing write:approvals are each refused with NOTHING recorded.
//
// COUNTERFACTUAL (each rung in requireOperatorCapture): drop the check — the
// injected identity reaches the append and the request returns 200.
func TestUpkeepDispositions_U1toU4_OperatorOnly(t *testing.T) {
	body := ukBatch(ukEntry(ukFlake, "approved"))
	cases := []struct {
		name   string
		id     func(*ukFixture) Identity
		status int
		code   string
	}{
		{"U1 anonymous", func(*ukFixture) Identity { return Identity{} }, http.StatusUnauthorized, "authentication_required"},
		{"U2 run-bound token", func(f *ukFixture) Identity {
			return Identity{Subject: "mcp:run:" + f.runID.String(), TokenID: "t", Scopes: []string{"write:approvals"}}
		}, http.StatusForbidden, "run_token_forbidden"},
		{"U3 operator-agent token", func(*ukFixture) Identity {
			return Identity{Subject: operatorrole.TokenSubjectPrefix + "captain", TokenID: "t", Scopes: []string{"write:approvals"}}
		}, http.StatusForbidden, "operator_agent_forbidden"},
		{"U4 missing write:approvals", func(*ukFixture) Identity {
			return Identity{Subject: "github:ops", TokenID: "t", Scopes: []string{"read:runs"}}
		}, http.StatusForbidden, "insufficient_scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUKFixture(t)
			w := postUK(t, f.s, f.runID.String(), body, ukWithIdentity(tc.id(f)))
			requireUKRefused(t, f, w, tc.status, tc.code)
		})
	}
}

func TestUpkeepDispositions_U5_BadRunID(t *testing.T) {
	f := newUKFixture(t)
	requireUKRefused(t, f, postUK(t, f.s, "not-a-uuid", ukBatch(ukEntry(ukFlake, "approved")), ukOperator),
		http.StatusBadRequest, "validation_failed")
	requireGDError(t, getUK(t, f.s, "not-a-uuid"), http.StatusBadRequest, "validation_failed")
}

// TestUpkeepDispositions_U6_UnknownRun: an unknown run is 404 run_not_found on
// BOTH verbs, and a run read failure is a 500.
//
// COUNTERFACTUAL (upkeepRequireRun): drop the GetRun check — the unknown
// run's empty chain falls through to 409 upkeep_report_absent.
func TestUpkeepDispositions_U6_UnknownRun(t *testing.T) {
	f := newUKFixture(t)
	other := uuid.NewString()
	requireUKRefused(t, f, postUK(t, f.s, other, ukBatch(ukEntry(ukFlake, "approved")), ukOperator),
		http.StatusNotFound, "run_not_found")
	requireGDError(t, getUK(t, f.s, other), http.StatusNotFound, "run_not_found")

	f.runs.getErr = errors.New("db down")
	requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved")), ukOperator),
		http.StatusInternalServerError, "internal_error")
}

// TestUpkeepDispositions_U7_Validation: every body-shape refusal records
// nothing. The rejected-verdict rows pair the override with the SAME valid
// finding id the happy path accepts, so only the override rule refuses.
//
// COUNTERFACTUAL (the rejected-override rule): drop it — the request records
// and returns 200.
func TestUpkeepDispositions_U7_Validation(t *testing.T) {
	over := make([]string, upkeepMaxDispositions+1)
	for i := range over {
		over[i] = ukEntry(fmt.Sprintf("flake:T%d", i), "approved")
	}
	cases := map[string]string{
		"malformed body":        `{"dispositions":[`,
		"trailing document":     ukBatch(ukEntry(ukFlake, "approved")) + ukBatch(ukEntry(ukDrift, "approved")),
		"empty batch":           `{"dispositions":[]}`,
		"empty body":            ``,
		"201 entries":           ukBatch(over...),
		"empty finding_id":      ukBatch(ukEntry("  ", "approved")),
		"duplicate finding_id":  ukBatch(ukEntry(ukFlake, "approved"), ukEntry(ukFlake, "rejected")),
		"invalid parent_epic":   ukBatch(`{"finding_id":"` + ukFlake + `","verdict":"approved","parent_epic":"+12"}`),
		"empty parent_epic":     ukBatch(`{"finding_id":"` + ukFlake + `","verdict":"approved","parent_epic":""}`),
		"parent_epic on reject": ukBatch(`{"finding_id":"` + ukFlake + `","verdict":"rejected","parent_epic":"#389"}`),
		"tier on reject":        ukBatch(`{"finding_id":"` + ukFlake + `","verdict":"rejected","authorize_delegation_tier":true}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUKFixture(t)
			requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator),
				http.StatusBadRequest, "validation_failed")
		})
	}
}

// TestUpkeepDispositions_U8_VerdictInvalid: a verdict outside the closed set
// is 400 upkeep_verdict_invalid with details.allowed.
//
// COUNTERFACTUAL (the closed-set check): drop it — the fake append accepts
// "amended" and the request returns 200.
func TestUpkeepDispositions_U8_VerdictInvalid(t *testing.T) {
	f := newUKFixture(t)
	w := postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "amended")), ukOperator)
	requireUKRefused(t, f, w, http.StatusBadRequest, "upkeep_verdict_invalid")
	if !strings.Contains(w.Body.String(), `"allowed":["approved","rejected"]`) {
		t.Errorf("details.allowed missing: %s", w.Body.String())
	}
}

// TestUpkeepDispositions_U9_ReportAbsentOrUnreadable: no recorded row is 409
// (even with an ORPHAN artifact present — the binding is the recorded row,
// never the newest artifact); a recorded row naming an unreadable, wrong-kind,
// unparseable or undecodable target is 500, never a fallback.
func TestUpkeepDispositions_U9_ReportAbsentOrUnreadable(t *testing.T) {
	newBare := func(t *testing.T) *ukFixture {
		f := &ukFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit()}
		f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
		f.runs = &ukRunRepo{runID: f.runID}
		f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
		return f
	}
	body := ukBatch(ukEntry(ukFlake, "approved"))

	t.Run("no recorded row, orphan artifact present", func(t *testing.T) {
		f := newBare(t)
		f.addReport(t, false, ukFlake) // orphan: Create succeeded, row never landed
		requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator), http.StatusConflict, "upkeep_report_absent")
		requireGDError(t, getUK(t, f.s, f.runID.String()), http.StatusConflict, "upkeep_report_absent")
	})
	t.Run("recorded row names a missing artifact", func(t *testing.T) {
		f := newBare(t)
		f.recordRow(t, map[string]any{"artifact_id": uuid.NewString()})
		requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator), http.StatusInternalServerError, "internal_error")
	})
	t.Run("recorded row artifact_id not a uuid", func(t *testing.T) {
		f := newBare(t)
		f.recordRow(t, map[string]any{"artifact_id": "nope"})
		requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator), http.StatusInternalServerError, "internal_error")
	})
	t.Run("recorded row payload undecodable", func(t *testing.T) {
		f := newBare(t)
		f.recordRow(t, map[string]any{"artifact_id": 7})
		requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator), http.StatusInternalServerError, "internal_error")
	})
	t.Run("named artifact has the wrong kind", func(t *testing.T) {
		f := newBare(t)
		a := f.addReport(t, true, ukFlake)
		a.Kind = artifact.KindGroomingReport
		requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator), http.StatusInternalServerError, "internal_error")
	})
	t.Run("named artifact does not parse", func(t *testing.T) {
		f := newBare(t)
		a := f.addReport(t, true, ukFlake)
		a.Content = []byte(`{"kind":"upkeep_report"}`)
		requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator), http.StatusInternalServerError, "internal_error")
		requireGDError(t, getUK(t, f.s, f.runID.String()), http.StatusInternalServerError, "internal_error")
	})
}

// TestUpkeepDispositions_U10_FindingUnknown: [known, unknown] is 422 naming
// ONLY the unknown id, and NOTHING lands — not even the known entry.
//
// COUNTERFACTUAL (the whole-batch pre-check): delete it or move it into the
// append loop — the fake append accepts any id, so the known row (or both)
// lands.
func TestUpkeepDispositions_U10_FindingUnknown(t *testing.T) {
	f := newUKFixture(t)
	unknown := "flake:" + uuid.NewString()
	w := postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved"), ukEntry(unknown, "approved")), ukOperator)
	requireUKRefused(t, f, w, http.StatusUnprocessableEntity, "upkeep_finding_unknown")
	var env struct {
		Error struct {
			Details struct {
				Unknown []string `json:"unknown_finding_ids"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if got := env.Error.Details.Unknown; len(got) != 1 || got[0] != unknown {
		t.Errorf("unknown_finding_ids = %v, want [%s]", got, unknown)
	}
}

// TestUpkeepDispositions_U11_WindowClosedFallback: an artifact-bound
// upkeep_apply_window_closed row (seeded by construction) refuses the capture
// on the fallback path with 409 and zero rows; the read-back reports it.
//
// COUNTERFACTUAL (the fallback-path watermark check): delete it — the fake has
// no in-transaction check, so the request returns 200 and appends.
func TestUpkeepDispositions_U11_WindowClosedFallback(t *testing.T) {
	f := newUKFixture(t)
	// A watermark bound to ANOTHER artifact must not close this window.
	raw, _ := json.Marshal(map[string]any{"artifact_id": uuid.NewString(), "settlement": "approved"})
	_, _ = f.au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: f.runID, Category: audit.UpkeepApplyWindowClosedCategory, Payload: raw})
	raw, _ = json.Marshal(map[string]any{"artifact_id": f.art.ID.String(), "settlement": "rejected"})
	wm, _ := f.au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: f.runID, Category: audit.UpkeepApplyWindowClosedCategory, Payload: raw})

	w := postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved")), ukOperator)
	requireUKRefused(t, f, w, http.StatusConflict, "upkeep_window_closed")
	if !strings.Contains(w.Body.String(), fmt.Sprintf(`"watermark_sequence":%d`, wm.Sequence)) || !strings.Contains(w.Body.String(), `"settlement":"rejected"`) {
		t.Errorf("409 details missing watermark facts: %s", w.Body.String())
	}
	got := decodeUK(t, getUK(t, f.s, f.runID.String()))
	if !got.WindowClosed || got.Settlement == nil || got.Settlement.AuditSequence != wm.Sequence {
		t.Errorf("read-back window = %v %+v, want closed at %d", got.WindowClosed, got.Settlement, wm.Sequence)
	}
}

// --- happy path, last-wins, binding ------------------------------------------

// TestUpkeepDispositions_HappyPath: two entries → exactly two rows, every
// payload key present, actor user + subject; the POST echo equals the GET.
func TestUpkeepDispositions_HappyPath(t *testing.T) {
	f := newUKFixture(t)
	body := ukBatch(
		`{"finding_id":"`+ukFlake+`","verdict":"approved","authorize_delegation_tier":true,"parent_epic":"#389"}`,
		ukEntry(ukDeprec, "rejected"),
	)
	w := postUK(t, f.s, f.runID.String(), body, ukOperator)
	post := decodeUK(t, w)
	if n := f.au.count(CategoryUpkeepDispositionRecorded); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	rows, _ := f.au.ListForRunByCategory(context.Background(), f.runID, CategoryUpkeepDispositionRecorded)
	for _, e := range rows {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"run_id", "stage_id", "artifact_id", "content_hash", "finding_id", "source", "verdict", "authorize_delegation_tier"} {
			if _, ok := m[k]; !ok {
				t.Errorf("row %d payload missing %q: %v", e.Sequence, k, m)
			}
		}
		if m["artifact_id"] != f.art.ID.String() || m["stage_id"] != f.stageID.String() {
			t.Errorf("row bound to %v / %v, want %s / %s", m["artifact_id"], m["stage_id"], f.art.ID, f.stageID)
		}
		if _, has := m["parent_epic"]; has != (m["finding_id"] == ukFlake) {
			t.Errorf("parent_epic presence wrong on %v", m)
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorUser || e.ActorSubject == nil || *e.ActorSubject != "github:ops" {
			t.Errorf("actor = %v/%v, want user/github:ops", e.ActorKind, e.ActorSubject)
		}
	}
	if post.ArtifactID != f.art.ID.String() || post.WindowClosed || len(post.Dispositions) != 2 {
		t.Fatalf("echo = %+v", post)
	}
	d0, d1 := post.Dispositions[0], post.Dispositions[1]
	if d0.FindingID != ukDeprec || d0.Verdict != "rejected" || d0.AuthorizeDelegationTier || d0.Source != "deprecation" {
		t.Errorf("dispositions[0] = %+v", d0)
	}
	if d1.FindingID != ukFlake || !d1.AuthorizeDelegationTier || d1.ParentEpic != "#389" || d1.RecordedBy != "github:ops" {
		t.Errorf("dispositions[1] = %+v", d1)
	}
	if g := getUK(t, f.s, f.runID.String()); g.Body.String() != w.Body.String() {
		t.Errorf("GET body differs from POST echo:\nGET  %s\nPOST %s", g.Body.String(), w.Body.String())
	}
}

// TestUpkeepDispositions_LastWins: a second POST flips one verdict; the
// read-back shows the newer verdict and BOTH rows remain.
func TestUpkeepDispositions_LastWins(t *testing.T) {
	f := newUKFixture(t)
	decodeUK(t, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukDrift, "approved")), ukOperator))
	got := decodeUK(t, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukDrift, "rejected")), ukOperator))
	if len(got.Dispositions) != 1 || got.Dispositions[0].Verdict != "rejected" {
		t.Fatalf("read-back = %+v, want one rejected", got.Dispositions)
	}
	if n := f.au.count(CategoryUpkeepDispositionRecorded); n != 2 {
		t.Errorf("rows = %d, want 2 (supersession is auditable)", n)
	}
}

// TestUpkeepDispositions_BindsToHighestSequenceRecordedRow is the carried
// #3921 multiple-reports item: A {x, y} recorded at a lower sequence, B {y, z}
// at a higher one, both on ONE stage. Captures bind to B.
//
// COUNTERFACTUAL (latestUpkeepReport): pick the LOWEST-sequence row — POST z
// returns 422 and the artifact becomes A.
func TestUpkeepDispositions_BindsToHighestSequenceRecordedRow(t *testing.T) {
	f := &ukFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit()}
	f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
	f.runs = &ukRunRepo{runID: f.runID}
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
	a := f.addReport(t, true, ukFlake, ukDrift)
	// A disposition recorded against A BEFORE B lands (by construction).
	raw, _ := json.Marshal(upkeepDispositionPayload{ArtifactID: a.ID.String(), FindingID: ukDrift, Verdict: "approved"})
	_, _ = f.au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: f.runID, Category: CategoryUpkeepDispositionRecorded, Payload: raw})
	b := f.addReport(t, true, ukDrift, ukDeprec)

	got := decodeUK(t, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukDeprec, "approved")), ukOperator))
	if got.ArtifactID != b.ID.String() {
		t.Fatalf("artifact_id = %s, want B %s", got.ArtifactID, b.ID)
	}
	if len(got.Dispositions) != 1 || got.Dispositions[0].FindingID != ukDeprec {
		t.Errorf("B's read-back = %+v, want only %s (A's disposition must not leak)", got.Dispositions, ukDeprec)
	}
	requireGDError(t, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved")), ukOperator),
		http.StatusUnprocessableEntity, "upkeep_finding_unknown")
}

// TestUpkeepDispositions_ProjectionSkipsJunk: undecodable and id-less rows
// contribute no disposition; an older row never overrides a newer one.
func TestUpkeepDispositions_ProjectionSkipsJunk(t *testing.T) {
	art := uuid.NewString()
	mk := func(seq int64, payload string) *audit.Entry {
		return &audit.Entry{Sequence: seq, Payload: []byte(payload)}
	}
	got := projectUpkeepDispositions([]*audit.Entry{
		nil,
		mk(1, `not json`),
		mk(2, `{"artifact_id":"`+art+`","verdict":"approved"}`),
		mk(5, `{"artifact_id":"`+art+`","finding_id":"flake:a","verdict":"rejected"}`),
		mk(4, `{"artifact_id":"`+art+`","finding_id":"flake:a","verdict":"approved"}`),
	}, art)
	if len(got) != 1 || got[0].Verdict != "rejected" || got[0].AuditSequence != 5 {
		t.Fatalf("projection = %+v, want one rejected@5", got)
	}
}

// TestUpkeepDispositions_FallbackPartialAppendFailure: on the NON-atomic
// fallback, a three-entry batch whose SECOND append fails is a 500 whose
// details say exactly what survived — recorded=1 of requested=3 — and the
// chain carries exactly that one durable row (committed state, not just the
// error envelope).
func TestUpkeepDispositions_FallbackPartialAppendFailure(t *testing.T) {
	f := newUKFixture(t)
	failing := &ukFailingAudit{ukAudit: f.au, failOn: 2}
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: failing})
	w := postUK(t, f.s, f.runID.String(), ukBatch(
		ukEntry(ukFlake, "approved"), ukEntry(ukDrift, "approved"), ukEntry(ukDeprec, "rejected"),
	), ukOperator)
	requireGDError(t, w, http.StatusInternalServerError, "internal_error")
	d := ukErrorDetails(t, w)
	if d["recorded"] != float64(1) || d["requested"] != float64(3) {
		t.Errorf("details recorded/requested = %v/%v, want 1/3; body %s", d["recorded"], d["requested"], w.Body.String())
	}
	if n := f.au.count(CategoryUpkeepDispositionRecorded); n != 1 {
		t.Errorf("durable upkeep_disposition_recorded rows = %d, want exactly 1 (the append before the failure)", n)
	}
	if got := failing.calls; got != 2 {
		t.Errorf("disposition appends attempted = %d, want 2 (the loop stops at the failure)", got)
	}
}

// ukFailingAudit fails the failOn-th disposition append (1-based) and passes
// every other append through to the sequence-assigning chain.
type ukFailingAudit struct {
	*ukAudit
	failOn int
	calls  int
}

func (a *ukFailingAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategoryUpkeepDispositionRecorded {
		a.calls++
		if a.calls == a.failOn {
			return nil, errors.New("append boom")
		}
	}
	return a.ukAudit.AppendChained(ctx, p)
}

// ukErrorDetails decodes the error envelope's details object.
func ukErrorDetails(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
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

// --- strict decode, rung ordering, the two 500s (#3924 carried 13-16) --------

// TestUpkeepDispositions_UnknownFieldRefused: a misspelled
// authorise_delegation_tier on an otherwise-valid APPROVED entry is 400
// validation_failed naming the key, and records NOTHING.
//
// COUNTERFACTUAL (decodeUpkeepDispositionsBody's DisallowUnknownFields): drop
// it — the misspelled key is silently discarded, and the batch is otherwise
// valid against the recorded report, so nothing downstream refuses it: a 200
// lands one row with authorize_delegation_tier false.
func TestUpkeepDispositions_UnknownFieldRefused(t *testing.T) {
	f := newUKFixture(t)
	w := postUK(t, f.s, f.runID.String(),
		ukBatch(`{"finding_id":"`+ukFlake+`","verdict":"approved","authorise_delegation_tier":true}`), ukOperator)
	requireUKRefused(t, f, w, http.StatusBadRequest, "validation_failed")
	if !strings.Contains(w.Body.String(), "authorise_delegation_tier") {
		t.Errorf("400 does not name the unknown key: %s", w.Body.String())
	}
}

// TestUpkeepDispositions_UnknownNestedFieldRefused: the strict decode applies
// at EVERY depth — an undeclared key inside an entry, inside a LATER entry of
// an otherwise-valid batch (so the valid first entry does not land either),
// and at the top level are each 400 validation_failed with zero rows. Each
// body is otherwise valid.
func TestUpkeepDispositions_UnknownNestedFieldRefused(t *testing.T) {
	cases := map[string]string{
		"entry key":     ukBatch(`{"finding_id":"` + ukFlake + `","verdict":"approved","note":"looks right"}`),
		"top-level key": `{"dispositions":[` + ukEntry(ukFlake, "approved") + `],"dry_run":true}`,
		"second entry":  ukBatch(ukEntry(ukDrift, "approved"), `{"finding_id":"`+ukFlake+`","verdict":"approved","parentEpic":"#389"}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUKFixture(t)
			requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator),
				http.StatusBadRequest, "validation_failed")
		})
	}
}

// TestUpkeepDispositions_NonBooleanTierRefused: authorize_delegation_tier is a
// BOOLEAN; a string or number is 400 validation_failed with zero rows, never
// coerced. (A type mismatch fails the decode with or without the strict
// option, so this pins the contract rather than DisallowUnknownFields.)
func TestUpkeepDispositions_NonBooleanTierRefused(t *testing.T) {
	for name, v := range map[string]string{"string": `"true"`, "number": `1`} {
		t.Run(name, func(t *testing.T) {
			f := newUKFixture(t)
			body := ukBatch(`{"finding_id":"` + ukFlake + `","verdict":"approved","authorize_delegation_tier":` + v + `}`)
			requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), body, ukOperator),
				http.StatusBadRequest, "validation_failed")
		})
	}
}

// TestUpkeepDispositions_AmendedVerdictBeforeReportLookup: on an EXISTING run
// whose chain carries NO upkeep_report_recorded row, an out-of-set verdict is
// 400 upkeep_verdict_invalid — the U8 body rung runs BEFORE the U9 report
// lookup, so the caller learns its request is malformed rather than being sent
// to wait for a report that would still refuse it.
//
// COUNTERFACTUAL (rung order): move the latestUpkeepReport resolution above the
// per-entry loop — the empty chain answers first with 409 upkeep_report_absent.
func TestUpkeepDispositions_AmendedVerdictBeforeReportLookup(t *testing.T) {
	f := &ukFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit()}
	f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
	f.runs = &ukRunRepo{runID: f.runID}
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
	if n := f.au.count(CategoryUpkeepReportRecorded); n != 0 {
		t.Fatalf("fixture carries %d upkeep_report_recorded rows, want 0", n)
	}
	requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "amended")), ukOperator),
		http.StatusBadRequest, "upkeep_verdict_invalid")
	// The same run with a VALID verdict reaches U9 — proving the fixture's
	// chain really is report-less, so the 400 above is the ordering, not luck.
	requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved")), ukOperator),
		http.StatusConflict, "upkeep_report_absent")
}

// ukAtomicAudit carries audit.UpkeepWindowAppender so the handler takes the
// ATOMIC path: batchErr fails the whole batch with nothing appended; otherwise
// every param lands on the chain in one call.
type ukAtomicAudit struct {
	*ukAudit
	batchErr error
}

func (a *ukAtomicAudit) AppendChainedUpkeepDispositionBatch(ctx context.Context, _ string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	if a.batchErr != nil {
		return nil, a.batchErr
	}
	out := make([]*audit.Entry, 0, len(ps))
	for _, p := range ps {
		e, err := a.AppendChained(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (a *ukAtomicAudit) AppendChainedUpkeepWindowClose(context.Context, audit.ChainAppendParams, string) (*audit.Entry, []*audit.Entry, error) {
	return nil, nil, errors.New("not used by the capture")
}

var _ audit.UpkeepWindowAppender = (*ukAtomicAudit)(nil)

// TestUpkeepDispositions_AtomicFailureRecordsNothing: an atomic batch failure
// is a 500 with details.recorded=0 of requested=2, and the chain carries no
// row.
func TestUpkeepDispositions_AtomicFailureRecordsNothing(t *testing.T) {
	f := newUKFixture(t)
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &ukAtomicAudit{ukAudit: f.au, batchErr: errors.New("tx boom")}})
	w := postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved"), ukEntry(ukDrift, "approved")), ukOperator)
	requireUKRefused(t, f, w, http.StatusInternalServerError, "internal_error")
	if d := ukErrorDetails(t, w); d["recorded"] != float64(0) || d["requested"] != float64(2) {
		t.Errorf("details recorded/requested = %v/%v, want 0/2 on an atomic failure; body %s", d["recorded"], d["requested"], w.Body.String())
	}
}

// TestUpkeepDispositions_ReadBackFailureAfterCommit: the atomic batch COMMITS,
// then the read-back list fails. The 500 must say the batch landed
// (details.recorded = requested = 2, and the message), because the rows ARE
// durable —
// reporting it like the atomic failure would tell the captain nothing was
// recorded when it was. A repeat POST is then safe (last-wins), and the
// read-back GET recovers once the list heals.
//
// COUNTERFACTUAL (respondUpkeepDispositions' committed branch): pass 0 from
// the POST — the 500 carries no recorded/requested and reads "listing recorded
// dispositions failed", though two rows are durable.
func TestUpkeepDispositions_ReadBackFailureAfterCommit(t *testing.T) {
	for _, cat := range []string{CategoryUpkeepDispositionRecorded, audit.UpkeepApplyWindowClosedCategory} {
		t.Run(cat, func(t *testing.T) {
			f := newUKFixture(t)
			flaky := &ukListFailAudit{ukAudit: f.au, cat: cat}
			f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &ukAtomicListFailAudit{ukListFailAudit: flaky}})
			w := postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved"), ukEntry(ukDrift, "rejected")), ukOperator)
			requireGDError(t, w, http.StatusInternalServerError, "internal_error")
			if n := f.au.count(CategoryUpkeepDispositionRecorded); n != 2 {
				t.Fatalf("durable rows = %d, want 2 (the batch committed before the read-back)", n)
			}
			if d := ukErrorDetails(t, w); d["recorded"] != float64(2) || d["requested"] != float64(2) {
				t.Errorf("details recorded/requested = %v/%v, want 2/2 after a committed batch; body %s", d["recorded"], d["requested"], w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "WAS recorded") {
				t.Errorf("500 message does not say the batch was recorded: %s", w.Body.String())
			}
			// The list heals: GET reads back exactly the committed batch.
			flaky.cat = ""
			if got := decodeUK(t, getUK(t, f.s, f.runID.String())); len(got.Dispositions) != 2 {
				t.Errorf("healed read-back = %+v, want the 2 committed dispositions", got.Dispositions)
			}
		})
	}
}

// ukAtomicListFailAudit is ukListFailAudit plus the atomic capability, so the
// POST commits through the batch and then fails its read-back list.
type ukAtomicListFailAudit struct{ *ukListFailAudit }

func (a *ukAtomicListFailAudit) AppendChainedUpkeepDispositionBatch(ctx context.Context, artifactID string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	return (&ukAtomicAudit{ukAudit: a.ukAudit}).AppendChainedUpkeepDispositionBatch(ctx, artifactID, ps)
}

func (a *ukAtomicListFailAudit) AppendChainedUpkeepWindowClose(context.Context, audit.ChainAppendParams, string) (*audit.Entry, []*audit.Entry, error) {
	return nil, nil, errors.New("not used by the capture")
}

// TestUpkeepDispositions_ListErrors: read failures on the disposition list and
// the window scan are 500s, never an empty 200.
func TestUpkeepDispositions_ListErrors(t *testing.T) {
	for _, cat := range []string{CategoryUpkeepDispositionRecorded, audit.UpkeepApplyWindowClosedCategory, CategoryUpkeepReportRecorded} {
		t.Run(cat, func(t *testing.T) {
			f := newUKFixture(t)
			f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &ukListFailAudit{ukAudit: f.au, cat: cat}})
			requireGDError(t, getUK(t, f.s, f.runID.String()), http.StatusInternalServerError, "internal_error")
			if cat == CategoryUpkeepDispositionRecorded {
				// The POST appends before its read-back list: a failure there is
				// the post-commit 500, pinned by ..._ReadBackFailureAfterCommit.
				return
			}
			requireUKRefused(t, f, postUK(t, f.s, f.runID.String(), ukBatch(ukEntry(ukFlake, "approved")), ukOperator),
				http.StatusInternalServerError, "internal_error")
		})
	}
}

type ukListFailAudit struct {
	*ukAudit
	cat string
}

func (a *ukListFailAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.cat {
		return nil, errors.New("list boom")
	}
	return a.ukAudit.ListForRunByCategory(ctx, runID, category)
}

// TestUpkeepReportRecordedCategory_AuditMirror: the audit-layer binding
// re-check reads audit.UpkeepReportRecordedCategory, the ingest writes
// CategoryUpkeepReportRecorded — they must be one value.
func TestUpkeepReportRecordedCategory_AuditMirror(t *testing.T) {
	if audit.UpkeepReportRecordedCategory != CategoryUpkeepReportRecorded {
		t.Fatalf("audit.UpkeepReportRecordedCategory = %q, server writes %q", audit.UpkeepReportRecordedCategory, CategoryUpkeepReportRecorded)
	}
}
