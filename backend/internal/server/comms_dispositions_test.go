package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// comms_dispositions_test.go pins POST/GET /v0/runs/{run_id}/comms-dispositions
// (#4016) against fakes: one test per rung or failure mode, each asserting the
// status, the code AND the committed comms_disposition_recorded row count read
// AFTER the call (a refusal and a rolled-back write return identical errors).
//
// The fixture records its gather and its comms_report_recorded row through the
// PRODUCTION writers (recordCommsScanGathered, commsRecordedPayload +
// appendCommsRecorded), so the recorded row carries its full real key set
// (approval condition 1). The plain fake drives the handler's FALLBACK append
// path; cmdAtomicAudit carries audit.FamilyWindowAppender and returns the
// typed comms refusals (approval condition 2). The atomic path against real
// Postgres is comms_dispositions_pg_test.go.

// The fixture report's drafts.
const (
	cmdD1 = "draft:UR-issue-12"
	cmdD2 = "draft:UR-issue-30+UR-issue-31"
	cmdD3 = "draft:UR-issue-50"
	// cmdD2Comment is D2's comment-report variant (cmdFixtureOpts.commentD2).
	cmdD2Comment = "draft:UR-comment-30-7+UR-issue-31"
)

// cmdFixtureOpts varies the fixture.
type cmdFixtureOpts struct {
	// commentD2 swaps D2's UR-issue-30 for the comment report UR-comment-30-7
	// (issue 30, comment 7).
	commentD2 bool
	// legacyGather records the gather WITHOUT the suggested_clusters key, as a
	// row written before #4016.
	legacyGather bool
}

// cmdReportBody is the fixture comms_report: D1, D2, D3, one n_drift over
// UR-issue-41 and UR-issue-40 not_drafted; UR-issue-99 is cited nowhere.
func cmdReportBody(t *testing.T, opts cmdFixtureOpts) []byte {
	t.Helper()
	d2Sources := []any{"UR-issue-30", "UR-issue-31"}
	d2ID := cmdD2
	if opts.commentD2 {
		d2Sources = []any{"UR-comment-30-7", "UR-issue-31"}
		d2ID = cmdD2Comment
	}
	draft := func(id string, sources []any, title string) map[string]any {
		return map[string]any{
			"id": id, "source_report_ids": sources,
			"rubric_citations": []any{map[string]any{"rubric_id": "U1"}},
			"proposed_issue": map[string]any{
				"type": "bug", "title": title,
				"body":   "Users report " + strings.ToLower(title) + ".",
				"labels": []any{"area:backend", "type:bug"},
			},
		}
	}
	return commsExampleMutated(t, func(m map[string]any) {
		m["drafts"] = []any{
			draft(cmdD1, []any{"UR-issue-12"}, "CSV export writes no rows"),
			draft(d2ID, d2Sources, "Dashboard times out on large runs"),
			draft(cmdD3, []any{"UR-issue-50"}, "Login page rejects valid tokens"),
		}
		m["n_drift"] = []any{map[string]any{
			"id": "ndrift:N2:UR-issue-41", "non_goal_id": "N2",
			"source_report_ids": []any{"UR-issue-41"}, "note": "Requests a hosted offering.",
		}}
		m["not_drafted"] = []any{map[string]any{"report_id": "UR-issue-40", "reason": "question"}}
	})
}

// cmdGather is the gather the fixture report was validated against, with two
// recorded clusters: {30, 31} (kept whole in D2) and {12, 40} (split between
// D1 and not_drafted).
func cmdGather(opts cmdFixtureOpts) commsScanGatheredPayload {
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	shown := func(id string, issue int, comment int64) commsShownReport {
		kind := "issue"
		if comment != 0 {
			kind = "comment"
		}
		return commsShownReport{ID: id, Kind: kind, IssueNumber: issue, CommentID: comment,
			ContentHash: strings.Repeat("c", 64), UpdatedAt: at}
	}
	first := shown("UR-issue-30", 30, 0)
	if opts.commentD2 {
		first = shown("UR-comment-30-7", 30, 7)
	}
	return commsScanGatheredPayload{
		StageAttempt: "1", Repo: upkeepTestRepo,
		Shown: []commsShownReport{
			shown("UR-issue-12", 12, 0), first, shown("UR-issue-31", 31, 0),
			shown("UR-issue-40", 40, 0), shown("UR-issue-41", 41, 0),
			shown("UR-issue-50", 50, 0), shown("UR-issue-99", 99, 0),
		},
		Charter: commsCharterRecord{Path: "docs/CHARTER.md", ContentHash: strings.Repeat("d", 64),
			RubricIDs: []string{"U1", "U2"}, NonGoalIDs: []string{"N1", "N2"}},
		SuggestedClusters: []commsRecordedCluster{
			{ReportIDs: []string{first.ID, "UR-issue-31"}, Score: 0.9},
			{ReportIDs: []string{"UR-issue-12", "UR-issue-40"}, Score: 0.8},
		},
	}
}

// cmdPreviews is the recorded preview set: D1 rendered (its body carrying the
// comms marker, so `<` is in play), D2 a preview error, D3 skipped.
func cmdPreviews(d2ID string) commsPreviewSet {
	return commsPreviewSet{
		Previews: []commsDraftPreview{
			{DraftID: cmdD1, FilingBodyDigest: strings.Repeat("1", 64), CommsRenderedPreview: &CommsRenderedPreview{
				Title: "CSV export writes no rows", Labels: []string{"area:backend", "type:bug"},
				DefaultedLabels: []string{"autonomy:medium"}, MissingLabelNamespaces: []string{},
				Body:       "Users report csv export writes no rows.\n\n---\n### Comms provenance (server-rendered)\n- UR-issue-12 (#12) & more\n<!-- fishhawk-comms:v1 UR-issue-12 cccc -->",
				SourceRefs: []string{"#12"},
			}},
			{DraftID: d2ID, FilingBodyDigest: strings.Repeat("2", 64),
				Error: &commsPreviewError{Code: "work_item_invalid", Message: "labels: unknown area"}},
			{DraftID: cmdD3, FilingBodyDigest: strings.Repeat("3", 64), Skipped: commsPreviewSkipBudgetExhausted},
		},
		CharterText: commsCharterTextRendered,
	}
}

type cmdFixture struct {
	s        *Server
	runID    uuid.UUID
	stageID  uuid.UUID
	arts     *ukArtifactRepo
	runs     *ukRunRepo
	au       *ukAudit
	art      *artifact.Artifact
	opts     cmdFixtureOpts
	gather   *audit.Entry
	recorded *audit.Entry
}

// newCMDFixture wires a server whose run carries ONE recorded comms_report.
func newCMDFixture(t *testing.T, opts cmdFixtureOpts) *cmdFixture {
	t.Helper()
	f := &cmdFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit(), opts: opts}
	f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
	f.runs = &ukRunRepo{runID: f.runID}
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
	f.gather = f.recordGather(t, cmdGather(opts), opts.legacyGather)
	f.art, f.recorded = f.addReport(t, cmdReportBody(t, opts), true)
	return f
}

// recordGather records the gather through the production writer, or — for a
// legacy row — as raw JSON without the suggested_clusters key.
func (f *cmdFixture) recordGather(t *testing.T, p commsScanGatheredPayload, legacy bool) *audit.Entry {
	t.Helper()
	ctx := context.Background()
	if !legacy {
		e, _, err := f.s.recordCommsScanGathered(ctx, f.runID, f.stageID, p)
		if err != nil {
			t.Fatalf("record gather: %v", err)
		}
		return e
	}
	p = p.normalized()
	p.StageID = f.stageID
	p.GatherDigest = commsGatherDigest(p)
	var m map[string]any
	raw, _ := json.Marshal(p)
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "suggested_clusters")
	raw, _ = json.Marshal(m)
	sid := f.stageID
	e, err := f.au.AppendChained(ctx, audit.ChainAppendParams{
		RunID: f.runID, StageID: &sid, Timestamp: time.Now().UTC(), Category: CategoryCommsScanGathered, Payload: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if commsGatheredClustersRecorded(e.Payload) {
		t.Fatal("legacy gather row still carries suggested_clusters")
	}
	return e
}

// addReport stores a comms_report artifact; with record it also appends the
// comms_report_recorded row through the production writer, bound to f.gather.
func (f *cmdFixture) addReport(t *testing.T, body []byte, record bool) (*artifact.Artifact, *audit.Entry) {
	t.Helper()
	a := &artifact.Artifact{
		ID: uuid.New(), StageID: f.stageID, Kind: artifact.KindCommsReport,
		Content: body, ContentHash: sha256Hex(body), CreatedAt: time.Now().UTC(),
	}
	f.arts.byID[a.ID] = a
	if !record {
		return a, nil
	}
	report, err := plan.ParseCommsReport(body)
	if err != nil {
		t.Fatalf("fixture report does not parse: %v", err)
	}
	gathered, err := decodeCommsScanGathered(f.gather.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if ref := checkCommsCharterRefs(report, gathered); ref != nil {
		t.Fatalf("fixture report fails the ingest's charter checks: %+v", ref)
	}
	d2 := cmdD2
	if f.opts.commentD2 {
		d2 = cmdD2Comment
	}
	payload := commsRecordedPayload(f.runID, f.stageID, a.ID.String(), a.ContentHash, len(body), report,
		commsBoundGather{entry: f.gather, payload: gathered}, cmdPreviews(d2))
	if err := f.s.appendCommsRecorded(context.Background(), f.runID, f.stageID, payload); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.au.ListForRunByCategory(context.Background(), f.runID, CategoryCommsReportRecorded)
	return a, rows[len(rows)-1]
}

// appendRaw appends a raw row of category to the run's chain.
func (f *cmdFixture) appendRaw(t *testing.T, category string, payload any) *audit.Entry {
	t.Helper()
	raw, ok := payload.([]byte)
	if !ok {
		raw, _ = json.Marshal(payload)
	}
	sid := f.stageID
	e, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, StageID: &sid, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func postCMD(t *testing.T, s *Server, runID string, raw string, withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/comms-dispositions", strings.NewReader(raw))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	s.handleRecordCommsDispositions(w, withID(req))
	return w
}

func getCMD(t *testing.T, s *Server, runID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID+"/comms-dispositions", nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	s.handleListCommsDispositions(w, req)
	return w
}

func cmdBatch(entries ...string) string {
	return `{"dispositions":[` + strings.Join(entries, ",") + `]}`
}

func cmdEntry(draftID, verdict string) string {
	return fmt.Sprintf(`{"draft_id":%q,"verdict":%q}`, draftID, verdict)
}

func cmdEntryEpic(draftID, verdict, epic string) string {
	return fmt.Sprintf(`{"draft_id":%q,"verdict":%q,"parent_epic":%q}`, draftID, verdict, epic)
}

func decodeCMD(t *testing.T, w *httptest.ResponseRecorder) commsDispositionsResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var out commsDispositionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func (f *cmdFixture) rows() int { return f.au.count(CategoryCommsDispositionRecorded) }

// requireCMDRefused asserts status + code AND zero committed disposition rows.
func requireCMDRefused(t *testing.T, f *cmdFixture, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	requireGDError(t, w, status, code)
	if n := f.rows(); n != 0 {
		t.Fatalf("committed comms_disposition_recorded rows = %d, want 0 on a %s refusal", n, code)
	}
}

// cmdErrorCode reads the error envelope's code ("" for a non-error body).
func cmdErrorCode(w *httptest.ResponseRecorder) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return env.Error.Code
}

// cmdAtomicAudit carries audit.FamilyWindowAppender so the handler takes the
// ATOMIC path. batchErr (a typed comms refusal or any error) fails the whole
// batch with nothing appended; otherwise every param lands in one call.
type cmdAtomicAudit struct {
	*ukAudit
	batchErr error
	families []string
}

func (a *cmdAtomicAudit) AppendChainedFamilyDispositionBatch(ctx context.Context, family, _ string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	a.families = append(a.families, family)
	if family != audit.WindowFamilyComms {
		return nil, &audit.UnknownWindowFamilyError{Family: family}
	}
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

func (a *cmdAtomicAudit) AppendChainedFamilyWindowClose(context.Context, string, audit.ChainAppendParams, string) (*audit.Entry, []*audit.Entry, error) {
	return nil, nil, errors.New("not used by the capture")
}

var _ audit.FamilyWindowAppender = (*cmdAtomicAudit)(nil)

// cmdListFailAudit fails ListForRunByCategory for one category.
type cmdListFailAudit struct {
	*ukAudit
	cat string
}

func (a *cmdListFailAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.cat {
		return nil, errors.New("list boom")
	}
	return a.ukAudit.ListForRunByCategory(ctx, runID, category)
}

// cmdAtomicListFailAudit commits through the atomic batch, then fails its
// read-back list for one category.
type cmdAtomicListFailAudit struct{ *cmdListFailAudit }

func (a *cmdAtomicListFailAudit) AppendChainedFamilyDispositionBatch(ctx context.Context, family, artifactID string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	return (&cmdAtomicAudit{ukAudit: a.ukAudit}).AppendChainedFamilyDispositionBatch(ctx, family, artifactID, ps)
}

func (a *cmdAtomicListFailAudit) AppendChainedFamilyWindowClose(context.Context, string, audit.ChainAppendParams, string) (*audit.Entry, []*audit.Entry, error) {
	return nil, nil, errors.New("not used by the capture")
}

// cmdFailingAudit fails the failOn-th disposition append (1-based).
type cmdFailingAudit struct {
	*ukAudit
	failOn int
	calls  int
}

func (a *cmdFailingAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategoryCommsDispositionRecorded {
		a.calls++
		if a.calls == a.failOn {
			return nil, errors.New("append boom")
		}
	}
	return a.ukAudit.AppendChained(ctx, p)
}

// --- C0..C6 ------------------------------------------------------------------

func TestCommsDispositions_Unconfigured(t *testing.T) {
	s := New(Config{})
	requireGDError(t, postCMD(t, s, uuid.NewString(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
		http.StatusServiceUnavailable, "comms_dispositions_unconfigured")
	requireGDError(t, getCMD(t, s, uuid.NewString()), http.StatusServiceUnavailable, "comms_dispositions_unconfigured")
}

// TestCommsDispositions_OperatorOnly: anonymous, a run-bound agent token (even
// for its OWN run), a delegated operator-agent token and a token missing
// write:approvals are each refused with NOTHING recorded.
//
// COUNTERFACTUAL (the requireOperatorCapture call): delete it — the fixture
// batch is otherwise valid, so each case returns 200 with one row.
func TestCommsDispositions_OperatorOnly(t *testing.T) {
	body := cmdBatch(cmdEntry(cmdD1, "approved"))
	cases := []struct {
		name   string
		id     func(*cmdFixture) Identity
		status int
		code   string
	}{
		{"Anonymous", func(*cmdFixture) Identity { return Identity{} }, http.StatusUnauthorized, "authentication_required"},
		{"RunTokenForbidden", func(f *cmdFixture) Identity {
			return Identity{Subject: "mcp:run:" + f.runID.String(), TokenID: "t", Scopes: []string{"write:approvals"}}
		}, http.StatusForbidden, "run_token_forbidden"},
		{"OperatorAgentForbidden", func(*cmdFixture) Identity {
			return Identity{Subject: operatorrole.TokenSubjectPrefix + "captain", TokenID: "t", Scopes: []string{"write:approvals"}}
		}, http.StatusForbidden, "operator_agent_forbidden"},
		{"InsufficientScope", func(*cmdFixture) Identity {
			return Identity{Subject: "github:ops", TokenID: "t", Scopes: []string{"read:runs"}}
		}, http.StatusForbidden, "insufficient_scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCMDFixture(t, cmdFixtureOpts{})
			requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), body, ukWithIdentity(tc.id(f))), tc.status, tc.code)
		})
	}
}

func TestCommsDispositions_BadRunID(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	requireCMDRefused(t, f, postCMD(t, f.s, "not-a-uuid", cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
		http.StatusBadRequest, "validation_failed")
	requireGDError(t, getCMD(t, f.s, "not-a-uuid"), http.StatusBadRequest, "validation_failed")
}

func TestCommsDispositions_UnknownRun(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	other := uuid.NewString()
	requireCMDRefused(t, f, postCMD(t, f.s, other, cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
		http.StatusNotFound, "run_not_found")
	requireGDError(t, getCMD(t, f.s, other), http.StatusNotFound, "run_not_found")
	f.runs.getErr = errors.New("db down")
	requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
		http.StatusInternalServerError, "internal_error")
}

// --- C7 / C8 -----------------------------------------------------------------

// TestCommsDispositions_UnknownKeyRefused: a misspelled `parent_epics` on an
// otherwise-valid approved entry is 400 naming the key, zero rows.
//
// COUNTERFACTUAL (the strict decode): swap to a lenient decode — the key is
// dropped and the entry records (200, one row).
func TestCommsDispositions_UnknownKeyRefused(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(`{"draft_id":"`+cmdD1+`","verdict":"approved","parent_epics":"#5"}`), ukOperator)
	requireCMDRefused(t, f, w, http.StatusBadRequest, "validation_failed")
	if !strings.Contains(w.Body.String(), "parent_epics") {
		t.Errorf("400 does not name the unknown key: %s", w.Body.String())
	}
}

// TestCommsDispositions_Validation: every remaining C7 body refusal, each on an
// otherwise-valid body, with zero rows.
func TestCommsDispositions_Validation(t *testing.T) {
	cases := map[string]string{
		"unparseable":       `{"dispositions":[`,
		"top-level key":     `{"dispositions":[` + cmdEntry(cmdD1, "approved") + `],"dry_run":true}`,
		"second entry key":  cmdBatch(cmdEntry(cmdD3, "approved"), `{"draft_id":"`+cmdD1+`","verdict":"approved","note":"x"}`),
		"wrong type":        `{"dispositions":[{"draft_id":12,"verdict":"approved"}]}`,
		"trailing content":  cmdBatch(cmdEntry(cmdD1, "approved")) + `{"dispositions":[]}`,
		"empty batch":       `{"dispositions":[]}`,
		"empty body":        ``,
		"empty draft_id":    cmdBatch(cmdEntry("  ", "approved")),
		"parent_epic array": `{"dispositions":[{"draft_id":"` + cmdD1 + `","verdict":"approved","parent_epic":[5]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCMDFixture(t, cmdFixtureOpts{})
			requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), body, ukOperator), http.StatusBadRequest, "validation_failed")
		})
	}
}

// TestCommsDispositions_BatchCapEnforced: 26 entries is 400 validation_failed
// with details.max=25.
//
// MASKING GUARD: a report holds at most 25 drafts, so with the cap deleted the
// batch falls to 422 comms_draft_unknown and STILL writes nothing — the row
// count cannot discriminate, so this arm pins error IDENTITY (code + max).
func TestCommsDispositions_BatchCapEnforced(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	entries := make([]string, 0, 26)
	for i := 0; i < 26; i++ {
		entries = append(entries, cmdEntry(fmt.Sprintf("draft:UR-issue-%d", 100+i), "approved"))
	}
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(entries...), ukOperator)
	requireCMDRefused(t, f, w, http.StatusBadRequest, "validation_failed")
	if d := ukErrorDetails(t, w); d["max"] != float64(25) || d["got"] != float64(26) {
		t.Errorf("details max/got = %v/%v, want 25/26; body %s", d["max"], d["got"], w.Body.String())
	}
}

// COUNTERFACTUAL (the duplicate check): delete it — 200 and two rows.
func TestCommsDispositions_DuplicateDraftIDRefused(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(),
		cmdBatch(cmdEntry(cmdD1, "approved"), cmdEntry(cmdD1, "rejected")), ukOperator),
		http.StatusBadRequest, "validation_failed")
}

// COUNTERFACTUAL (the C8 check): delete it — a row with verdict maybe lands.
func TestCommsDispositions_VerdictInvalid(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "maybe")), ukOperator)
	requireCMDRefused(t, f, w, http.StatusBadRequest, "comms_verdict_invalid")
	if d := ukErrorDetails(t, w); !reflect.DeepEqual(d["allowed"], []any{"approved", "rejected"}) {
		t.Errorf("details.allowed = %v", d["allowed"])
	}
}

// TestCommsDispositions_ParentEpicInvalid: a parent_epic the ONE parser
// (plan.CommsParentEpicNumber) cannot read is 400 with zero rows — and on a run
// with NO recorded report it is still that 400, not 409 comms_report_absent.
//
// MASKING GUARD: on the full fixture the C11 rung re-parses parent_epic and
// also refuses (approval condition 4), so deleting the C7 parse leaves those
// cases 400. The report-less arm ISOLATES C7: C11 runs after the report
// lookup, so with C7 deleted it answers 409 comms_report_absent.
//
// COUNTERFACTUAL (the C7 parse): delete it — the report-less arm is 409.
func TestCommsDispositions_ParentEpicInvalid(t *testing.T) {
	for _, v := range []string{"0", "#", " 5", "", "-3", "#1#"} {
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			f := newCMDFixture(t, cmdFixtureOpts{})
			requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntryEpic(cmdD1, "approved", v)), ukOperator),
				http.StatusBadRequest, "validation_failed")
		})
	}
	t.Run("report-less run", func(t *testing.T) {
		f := &cmdFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit()}
		f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
		f.runs = &ukRunRepo{runID: f.runID}
		f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
		w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntryEpic(cmdD1, "approved", "0")), ukOperator)
		requireCMDRefused(t, f, w, http.StatusBadRequest, "validation_failed")
		if d := ukErrorDetails(t, w); d["field"] != "dispositions[0].parent_epic" {
			t.Errorf("details = %v, want the parent_epic field", d)
		}
	})
}

// COUNTERFACTUAL (the rejected-verdict check): delete it — a rejected row with
// parent_epic lands.
func TestCommsDispositions_ParentEpicOnRejected(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntryEpic(cmdD1, "rejected", "#389")), ukOperator),
		http.StatusBadRequest, "validation_failed")
}

// TestCommsDispositions_VerdictBeforeReportLookup: on a run with NO recorded
// report an out-of-set verdict is still 400 comms_verdict_invalid — the body
// rungs run before the C9 lookup.
func TestCommsDispositions_VerdictBeforeReportLookup(t *testing.T) {
	f := &cmdFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit()}
	f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
	f.runs = &ukRunRepo{runID: f.runID}
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
	requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "maybe")), ukOperator),
		http.StatusBadRequest, "comms_verdict_invalid")
	requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
		http.StatusConflict, "comms_report_absent")
}

// --- C9 ----------------------------------------------------------------------

// TestCommsDispositions_ReportAbsent: a run with no comms_report_recorded row
// (an ORPHAN artifact exists) is 409 comms_report_absent on both verbs.
//
// COUNTERFACTUAL (the absent branch): route it to the 500 — RED on the code.
func TestCommsDispositions_ReportAbsent(t *testing.T) {
	f := &cmdFixture{runID: uuid.New(), stageID: uuid.New(), au: newUKAudit()}
	f.arts = &ukArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}}
	f.runs = &ukRunRepo{runID: f.runID}
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: f.au})
	f.gather = f.recordGather(t, cmdGather(cmdFixtureOpts{}), false)
	f.addReport(t, cmdReportBody(t, cmdFixtureOpts{}), false)
	requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
		http.StatusConflict, "comms_report_absent")
	requireGDError(t, getCMD(t, f.s, f.runID.String()), http.StatusConflict, "comms_report_absent")
}

// TestCommsDispositions_ReportUnresolvable500: every way the newest recorded
// row, its artifact or its gather can fail is a 500 on BOTH verbs with zero
// rows — never a fallback to an older report, never an empty view. Each case
// appends a NEWER recorded row (so the good one is not the binding).
func TestCommsDispositions_ReportUnresolvable500(t *testing.T) {
	full := func(f *cmdFixture, mutate func(m map[string]any)) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(f.recorded.Payload, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		return m
	}
	cases := map[string]func(t *testing.T, f *cmdFixture){
		"ReportUnreadable": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["artifact_id"] = uuid.NewString() }))
		},
		"artifact id not a uuid": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["artifact_id"] = "nope" }))
		},
		"row not JSON object": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, []byte(`["x"]`))
		},
		"wrong kind": func(t *testing.T, f *cmdFixture) {
			a, _ := f.addReport(t, cmdReportBody(t, f.opts), false)
			a.Kind = artifact.KindUpkeepReport
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["artifact_id"] = a.ID.String() }))
		},
		"artifact does not parse": func(t *testing.T, f *cmdFixture) {
			a, _ := f.addReport(t, []byte(`{"kind":"comms_report"}`), false)
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["artifact_id"] = a.ID.String() }))
		},
		"no previews": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { delete(m, "previews") }))
		},
		"previews not an array": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["previews"] = map[string]any{} }))
		},
		"previews null": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["previews"] = nil }))
		},
		"no unaccounted_report_ids": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { delete(m, "unaccounted_report_ids") }))
		},
		"no gather_digest": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { delete(m, "gather_digest") }))
		},
		"GatherUnloadable": func(t *testing.T, f *cmdFixture) {
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["gather_digest"] = strings.Repeat("f", 64) }))
		},
		"gather undecodable": func(t *testing.T, f *cmdFixture) {
			var g map[string]any
			_ = json.Unmarshal(f.gather.Payload, &g)
			g["gather_digest"] = strings.Repeat("e", 64)
			g["unexpected"] = true // the strict gather decode refuses it
			f.appendRaw(t, CategoryCommsScanGathered, g)
			f.appendRaw(t, CategoryCommsReportRecorded, full(f, func(m map[string]any) { m["gather_digest"] = strings.Repeat("e", 64) }))
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCMDFixture(t, cmdFixtureOpts{})
			seed(t, f)
			requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
				http.StatusInternalServerError, "internal_error")
			requireGDError(t, getCMD(t, f.s, f.runID.String()), http.StatusInternalServerError, "internal_error")
		})
	}
}

// TestDecodeCommsRecordedRow_RequiresKeys isolates decodeCommsRecordedRow's
// required-key checks. Through the handler, a missing gather_digest is also
// refused downstream (commsScanGatheredByDigest never matches ""), which masks
// this rung; called directly, each missing key is its own error naming the key.
//
// COUNTERFACTUAL (each check): delete it — that case decodes without error.
func TestDecodeCommsRecordedRow_RequiresKeys(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	if _, err := decodeCommsRecordedRow(f.recorded); err != nil {
		t.Fatalf("production row: %v", err)
	}
	for key, mutate := range map[string]func(m map[string]any){
		"gather_digest":          func(m map[string]any) { m["gather_digest"] = "" },
		"previews":               func(m map[string]any) { m["previews"] = "nope" },
		"unaccounted_report_ids": func(m map[string]any) { delete(m, "unaccounted_report_ids") },
	} {
		t.Run(key, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(f.recorded.Payload, &m)
			mutate(m)
			raw, _ := json.Marshal(m)
			_, err := decodeCommsRecordedRow(&audit.Entry{Sequence: 7, Payload: raw})
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want a refusal naming %s", err, key)
			}
		})
	}
}

// TestCommsDispositions_RecordedRowDecodeIsLenient (approval condition 1): the
// fixture row is the PRODUCTION writer's (run_id, schema_version, entry_counts,
// gather_sequence, … — keys commsRecordedRow does not declare), plus a newer
// row carrying an extra future key; both resolve with 200.
//
// COUNTERFACTUAL (decodeCommsRecordedRow's plain json.Unmarshal): switch to
// DisallowUnknownFields — every real-shaped row is a 500.
func TestCommsDispositions_RecordedRowDecodeIsLenient(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	var m map[string]any
	_ = json.Unmarshal(f.recorded.Payload, &m)
	for _, k := range []string{"run_id", "schema_version", "entry_counts", "gather_sequence", "charter_content_hash", "size_bytes"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("production recorded row lacks %q; the leniency proof needs undeclared keys: %v", k, m)
		}
	}
	decodeCMD(t, getCMD(t, f.s, f.runID.String()))
	m["suppressions"] = []any{map[string]any{"id": "UR-issue-12"}}
	f.appendRaw(t, CategoryCommsReportRecorded, m)
	decodeCMD(t, getCMD(t, f.s, f.runID.String()))
}

// TestCommsDispositions_BindsToRecordedRowNotNewestArtifact: an ORPHAN newer
// comms_report artifact (no recorded row) carrying a draft the recorded report
// lacks. The binding stays on the recorded artifact, and the orphan-only draft
// is 422.
//
// COUNTERFACTUAL (bind by newest artifact): artifact_id flips and the orphan
// draft records.
func TestCommsDispositions_BindsToRecordedRowNotNewestArtifact(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	orphan := commsExampleMutated(t, func(m map[string]any) {
		var base map[string]any
		_ = json.Unmarshal(cmdReportBody(t, cmdFixtureOpts{}), &base)
		for k, v := range base {
			m[k] = v
		}
		drafts := m["drafts"].([]any)
		extra := map[string]any{}
		b, _ := json.Marshal(drafts[2])
		_ = json.Unmarshal(b, &extra)
		extra["id"], extra["source_report_ids"] = "draft:UR-issue-99", []any{"UR-issue-99"}
		m["drafts"] = append(drafts, extra)
	})
	a, _ := f.addReport(t, orphan, false)
	a.CreatedAt = time.Now().Add(time.Hour)

	got := decodeCMD(t, getCMD(t, f.s, f.runID.String()))
	if got.ArtifactID != f.art.ID.String() {
		t.Fatalf("bound to %s, want the recorded %s (orphan %s)", got.ArtifactID, f.art.ID, a.ID)
	}
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry("draft:UR-issue-99", "approved")), ukOperator)
	requireCMDRefused(t, f, w, http.StatusUnprocessableEntity, "comms_draft_unknown")
}

// --- C10 / C11 ---------------------------------------------------------------

// TestCommsDispositions_UnknownDraftWholeBatch: one known and one unknown
// draft — 422 naming only the unknown one, and the KNOWN one is NOT recorded.
//
// COUNTERFACTUAL (whole-batch ordering): move the check after the append loop
// — D1's row lands.
func TestCommsDispositions_UnknownDraftWholeBatch(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved"), cmdEntry("draft:UR-issue-99", "approved")), ukOperator)
	requireCMDRefused(t, f, w, http.StatusUnprocessableEntity, "comms_draft_unknown")
	d := ukErrorDetails(t, w)
	if !reflect.DeepEqual(d["unknown_draft_ids"], []any{"draft:UR-issue-99"}) || d["artifact_id"] != f.art.ID.String() {
		t.Errorf("details = %v", d)
	}
}

// TestCommsDispositions_ParentEpicIsSource: an approved draft whose
// parent_epic is the issue a cited report lives on — #12 for D1, and 30 for
// D2's comment-report variant (UR-comment-30-7 lives on issue 30) — is 400
// reason parent_epic_is_source with zero rows. A parent_epic naming an
// unrelated issue records.
//
// COUNTERFACTUAL (the C11 call): delete it — a row lands.
func TestCommsDispositions_ParentEpicIsSource(t *testing.T) {
	cases := []struct {
		name  string
		opts  cmdFixtureOpts
		draft string
		epic  string
	}{
		{"issue report", cmdFixtureOpts{}, cmdD1, "#12"},
		{"comment report", cmdFixtureOpts{commentD2: true}, cmdD2Comment, "30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCMDFixture(t, tc.opts)
			w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD3, "approved"), cmdEntryEpic(tc.draft, "approved", tc.epic)), ukOperator)
			requireCMDRefused(t, f, w, http.StatusBadRequest, "validation_failed")
			d := ukErrorDetails(t, w)
			if d["reason"] != "parent_epic_is_source" || d["field"] != "dispositions[1].parent_epic" || d["draft_id"] != tc.draft || d["parent_epic"] != tc.epic {
				t.Errorf("details = %v", d)
			}
		})
	}
	f := newCMDFixture(t, cmdFixtureOpts{})
	decodeCMD(t, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntryEpic(cmdD1, "approved", "#13")), ukOperator))
	if f.rows() != 1 {
		t.Fatalf("rows = %d, want 1 for an unrelated parent_epic", f.rows())
	}
}

// TestCommsParentEpicIsSource_UnparseableRefused (approval condition 4): the
// C11 decision parses through plan.CommsParentEpicNumber and REFUSES a value it
// cannot parse rather than skipping the equality check. C7 refuses such a value
// first, so this leg is pinned on the pure function.
//
// COUNTERFACTUAL: make the !ok branch `continue` — nil is returned.
func TestCommsParentEpicIsSource_UnparseableRefused(t *testing.T) {
	report, err := plan.ParseCommsReport(cmdReportBody(t, cmdFixtureOpts{}))
	if err != nil {
		t.Fatal(err)
	}
	g := cmdGather(cmdFixtureOpts{})
	bad := " 12"
	ref := commsParentEpicIsSource(report, &g, []commsDispositionInput{{DraftID: cmdD1, Verdict: "approved", ParentEpic: &bad}})
	if ref == nil || ref.Details["field"] != "dispositions[0].parent_epic" || ref.Details["got"] != bad {
		t.Fatalf("refusal = %+v, want an unparseable-parent_epic refusal", ref)
	}
	rejected := "#12"
	if ref := commsParentEpicIsSource(report, &g, []commsDispositionInput{{DraftID: cmdD1, Verdict: "rejected", ParentEpic: &rejected}}); ref != nil {
		t.Errorf("a rejected entry is not an approved filing: %+v", ref)
	}
}

// --- C12 and the append paths ------------------------------------------------

// TestCommsDispositions_WindowClosedFallback: a comms_apply_window_closed
// watermark for the bound artifact refuses the capture with 409 and zero rows;
// a watermark of ANOTHER artifact does not. The read-back reports the window.
//
// COUNTERFACTUAL (the fallback window check): delete it — the fake has no
// in-transaction check, so a row is appended.
func TestCommsDispositions_WindowClosedFallback(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	f.appendRaw(t, audit.CommsApplyWindowClosedCategory, map[string]any{"artifact_id": uuid.NewString(), "settlement": "approved"})
	wm := f.appendRaw(t, audit.CommsApplyWindowClosedCategory, map[string]any{"artifact_id": f.art.ID.String(), "settlement": "rejected"})
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator)
	requireCMDRefused(t, f, w, http.StatusConflict, "comms_window_closed")
	if d := ukErrorDetails(t, w); d["watermark_sequence"] != float64(wm.Sequence) || d["settlement"] != "rejected" || d["artifact_id"] != f.art.ID.String() {
		t.Errorf("409 details = %v", d)
	}
	got := decodeCMD(t, getCMD(t, f.s, f.runID.String()))
	if !got.WindowClosed || got.Settlement == nil || got.Settlement.AuditSequence != wm.Sequence {
		t.Errorf("read-back window = %v %+v, want closed at %d", got.WindowClosed, got.Settlement, wm.Sequence)
	}
}

// TestCommsDispositions_AtomicRefusals (approval condition 2): the in-memory
// FamilyWindowAppender returns the typed comms refusals; the handler maps each
// to its 409, and nothing is appended. The batch reaches the appender under
// the comms family.
//
// COUNTERFACTUAL (each errors.As case): delete it — the refusal falls through
// to the 500.
func TestCommsDispositions_AtomicRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  func(f *cmdFixture) error
		code string
		want map[string]any
	}{
		{"superseded", func(f *cmdFixture) error {
			return &audit.ReportSupersededError{Family: audit.WindowFamilyComms, ArtifactID: f.art.ID.String(), CurrentArtifactID: "next", CurrentSequence: 41}
		}, "comms_report_superseded", map[string]any{"current_artifact_id": "next", "current_sequence": float64(41)}},
		{"window closed", func(f *cmdFixture) error {
			return &audit.WindowClosedError{Family: audit.WindowFamilyComms, ArtifactID: f.art.ID.String(), Settlement: "approved", Sequence: 17}
		}, "comms_window_closed", map[string]any{"settlement": "approved", "watermark_sequence": float64(17)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCMDFixture(t, cmdFixtureOpts{})
			at := &cmdAtomicAudit{ukAudit: f.au, batchErr: tc.err(f)}
			f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: at})
			w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator)
			requireCMDRefused(t, f, w, http.StatusConflict, tc.code)
			d := ukErrorDetails(t, w)
			if d["artifact_id"] != f.art.ID.String() {
				t.Errorf("details.artifact_id = %v", d["artifact_id"])
			}
			for k, v := range tc.want {
				if d[k] != v {
					t.Errorf("details.%s = %v, want %v", k, d[k], v)
				}
			}
			if !reflect.DeepEqual(at.families, []string{audit.WindowFamilyComms}) {
				t.Errorf("appender families = %v, want [comms]", at.families)
			}
		})
	}
}

// TestCommsDispositions_AtomicHappyPath: the atomic path commits the batch.
func TestCommsDispositions_AtomicHappyPath(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &cmdAtomicAudit{ukAudit: f.au}})
	got := decodeCMD(t, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved"), cmdEntry(cmdD3, "rejected")), ukOperator))
	if f.rows() != 2 || len(got.Dispositions) != 2 {
		t.Fatalf("rows = %d, dispositions = %+v", f.rows(), got.Dispositions)
	}
}

// TestCommsDispositions_AtomicFailureRecordsNothing: an atomic batch failure is
// a 500 with details.recorded=0 of requested=2 and no row.
//
// COUNTERFACTUAL: report recorded=len(params) — RED on details.recorded.
func TestCommsDispositions_AtomicFailureRecordsNothing(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &cmdAtomicAudit{ukAudit: f.au, batchErr: errors.New("tx boom")}})
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved"), cmdEntry(cmdD3, "approved")), ukOperator)
	requireCMDRefused(t, f, w, http.StatusInternalServerError, "internal_error")
	if d := ukErrorDetails(t, w); d["recorded"] != float64(0) || d["requested"] != float64(2) {
		t.Errorf("details recorded/requested = %v/%v, want 0/2", d["recorded"], d["requested"])
	}
}

// TestCommsDispositions_ReadBackFailureAfterCommit: the atomic batch COMMITS,
// then a read-back list fails — the 500 says the batch landed
// (recorded = requested = 2), and the GET recovers once the list heals.
//
// COUNTERFACTUAL (respondCommsDispositions' committed branch): pass 0 from the
// POST — the 500 carries no recorded/requested though two rows are durable.
func TestCommsDispositions_ReadBackFailureAfterCommit(t *testing.T) {
	for _, cat := range []string{CategoryCommsDispositionRecorded, audit.CommsApplyWindowClosedCategory} {
		t.Run(cat, func(t *testing.T) {
			f := newCMDFixture(t, cmdFixtureOpts{})
			flaky := &cmdListFailAudit{ukAudit: f.au, cat: cat}
			f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &cmdAtomicListFailAudit{cmdListFailAudit: flaky}})
			w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved"), cmdEntry(cmdD3, "rejected")), ukOperator)
			requireGDError(t, w, http.StatusInternalServerError, "internal_error")
			if n := f.rows(); n != 2 {
				t.Fatalf("durable rows = %d, want 2", n)
			}
			if d := ukErrorDetails(t, w); d["recorded"] != float64(2) || d["requested"] != float64(2) {
				t.Errorf("details recorded/requested = %v/%v, want 2/2; body %s", d["recorded"], d["requested"], w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "WAS recorded") {
				t.Errorf("500 message does not say the batch was recorded: %s", w.Body.String())
			}
			flaky.cat = ""
			if got := decodeCMD(t, getCMD(t, f.s, f.runID.String())); len(got.Dispositions) != 2 {
				t.Errorf("healed read-back = %+v", got.Dispositions)
			}
		})
	}
}

// TestCommsDispositions_FallbackPartialAppendFailure: the fallback's second
// append fails — 500 with recorded=1 of requested=2, and exactly one row is
// durable.
func TestCommsDispositions_FallbackPartialAppendFailure(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &cmdFailingAudit{ukAudit: f.au, failOn: 2}})
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved"), cmdEntry(cmdD3, "approved")), ukOperator)
	requireGDError(t, w, http.StatusInternalServerError, "internal_error")
	if d := ukErrorDetails(t, w); d["recorded"] != float64(1) || d["requested"] != float64(2) {
		t.Errorf("details recorded/requested = %v/%v, want 1/2", d["recorded"], d["requested"])
	}
	if n := f.rows(); n != 1 {
		t.Errorf("durable rows = %d, want 1", n)
	}
}

// TestCommsDispositions_ListErrors: read failures on the recorded-row list, the
// gather list, the disposition list and the window scan are 500s, never an
// empty 200, and a POST hitting one before its append writes nothing.
func TestCommsDispositions_ListErrors(t *testing.T) {
	for _, cat := range []string{CategoryCommsReportRecorded, CategoryCommsScanGathered, CategoryCommsDispositionRecorded, audit.CommsApplyWindowClosedCategory} {
		t.Run(cat, func(t *testing.T) {
			f := newCMDFixture(t, cmdFixtureOpts{})
			f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &cmdListFailAudit{ukAudit: f.au, cat: cat}})
			requireGDError(t, getCMD(t, f.s, f.runID.String()), http.StatusInternalServerError, "internal_error")
			if cat == CategoryCommsDispositionRecorded {
				return // the POST appends before this list: ..._ReadBackFailureAfterCommit
			}
			requireCMDRefused(t, f, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator),
				http.StatusInternalServerError, "internal_error")
		})
	}
}

// --- the 200 ------------------------------------------------------------------

// TestCommsDispositions_HappyPath: two entries → exactly two rows carrying
// every payload key, actor user + subject; the POST echo equals the GET.
func TestCommsDispositions_HappyPath(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	w := postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntryEpic(cmdD1, "approved", "#389"), cmdEntry(cmdD3, "rejected")), ukOperator)
	post := decodeCMD(t, w)
	if n := f.rows(); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	rows, _ := f.au.ListForRunByCategory(context.Background(), f.runID, CategoryCommsDispositionRecorded)
	for _, e := range rows {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"run_id", "stage_id", "artifact_id", "content_hash", "draft_id", "verdict"} {
			if _, ok := m[k]; !ok {
				t.Errorf("row %d payload missing %q: %v", e.Sequence, k, m)
			}
		}
		if m["artifact_id"] != f.art.ID.String() || m["stage_id"] != f.stageID.String() || m["content_hash"] != f.art.ContentHash {
			t.Errorf("row bound to %v", m)
		}
		if _, has := m["parent_epic"]; has != (m["draft_id"] == cmdD1) {
			t.Errorf("parent_epic presence wrong on %v", m)
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorUser || e.ActorSubject == nil || *e.ActorSubject != "github:ops" {
			t.Errorf("actor = %v/%v, want user/github:ops", e.ActorKind, e.ActorSubject)
		}
	}
	if post.ArtifactID != f.art.ID.String() || post.StageID != f.stageID.String() || post.WindowClosed || len(post.Dispositions) != 2 {
		t.Fatalf("echo = %+v", post)
	}
	if post.GatherDigest != commsEntryDigest(f.gather) || post.CharterText != commsCharterTextRendered || post.PreviewDegraded {
		t.Errorf("echo gather/charter/degraded = %s/%s/%v", post.GatherDigest, post.CharterText, post.PreviewDegraded)
	}
	if d := post.Dispositions[0]; d.DraftID != cmdD1 || d.Verdict != "approved" || d.ParentEpic != "#389" || d.RecordedBy != "github:ops" {
		t.Errorf("dispositions[0] = %+v", d)
	}
	if !reflect.DeepEqual(post.UnaccountedReportIDs, []string{"UR-issue-99"}) {
		t.Errorf("unaccounted = %v, want [UR-issue-99]", post.UnaccountedReportIDs)
	}
	if g := getCMD(t, f.s, f.runID.String()); g.Body.String() != w.Body.String() {
		t.Errorf("GET body differs from POST echo:\nGET  %s\nPOST %s", g.Body.String(), w.Body.String())
	}
}

// TestCommsDispositions_LastWins: D1 approved, then D1 rejected — the GET shows
// rejected at the higher sequence, and both rows remain.
//
// COUNTERFACTUAL (the sequence comparison): flip it — approved is shown.
func TestCommsDispositions_LastWins(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	first := decodeCMD(t, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator))
	decodeCMD(t, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "rejected")), ukOperator))
	got := decodeCMD(t, getCMD(t, f.s, f.runID.String()))
	if len(got.Dispositions) != 1 || got.Dispositions[0].Verdict != "rejected" || got.Dispositions[0].AuditSequence <= first.Dispositions[0].AuditSequence {
		t.Fatalf("dispositions = %+v, want D1 rejected at a higher sequence than %d", got.Dispositions, first.Dispositions[0].AuditSequence)
	}
	if f.rows() != 2 {
		t.Errorf("rows = %d, want both kept", f.rows())
	}
}

// TestCommsDispositions_ProjectionSkipsJunk: an undecodable row, a row without
// a draft id and a row of another artifact never manufacture a verdict.
func TestCommsDispositions_ProjectionSkipsJunk(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	f.appendRaw(t, CategoryCommsDispositionRecorded, []byte(`"junk"`))
	f.appendRaw(t, CategoryCommsDispositionRecorded, map[string]any{"artifact_id": f.art.ID.String(), "verdict": "approved"})
	f.appendRaw(t, CategoryCommsDispositionRecorded, map[string]any{"artifact_id": uuid.NewString(), "draft_id": cmdD1, "verdict": "approved"})
	got := decodeCMD(t, getCMD(t, f.s, f.runID.String()))
	if len(got.Dispositions) != 0 || len(got.UndecidedDraftIDs) != 3 {
		t.Errorf("dispositions = %+v undecided = %v, want none decided", got.Dispositions, got.UndecidedDraftIDs)
	}
}

// TestCommsDispositions_PreviewsVerbatim (approval condition 3): the GET's
// previews are BYTE-equal to the recorded row's previews value — one rendered
// (whose body carries `<` and `&`), one error, one skipped — and
// undecided_draft_ids lists D2 and D3, in report order, once only D1 is decided.
//
// COUNTERFACTUAL: decode previews into a typed shape (or omit them), or list
// decided drafts as undecided — RED.
//
// The bound row is the production row with ONE extra key inside the first
// preview (a field a later ingest might add): a typed decode-and-re-encode
// would drop it, so byte equality discriminates "verbatim" from "same shape".
func TestCommsDispositions_PreviewsVerbatim(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	var full map[string]any
	if err := json.Unmarshal(f.recorded.Payload, &full); err != nil {
		t.Fatal(err)
	}
	full["previews"].([]any)[0].(map[string]any)["future_field"] = "kept <verbatim> & whole"
	newer := f.appendRaw(t, CategoryCommsReportRecorded, full)
	var row map[string]json.RawMessage
	if err := json.Unmarshal(newer.Payload, &row); err != nil {
		t.Fatal(err)
	}
	decodeCMD(t, postCMD(t, f.s, f.runID.String(), cmdBatch(cmdEntry(cmdD1, "approved")), ukOperator))
	g := getCMD(t, f.s, f.runID.String())
	var body map[string]json.RawMessage
	if err := json.Unmarshal(g.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body["previews"], row["previews"]) {
		t.Fatalf("previews are not verbatim:\nGET %s\nrow %s", body["previews"], row["previews"])
	}
	for _, want := range []string{`"error":{"code":"work_item_invalid"`, `"skipped":"budget_exhausted"`, `fishhawk-comms:v1`, `"future_field":`} {
		if !strings.Contains(string(body["previews"]), want) {
			t.Errorf("previews lack %s: %s", want, body["previews"])
		}
	}
	got := decodeCMD(t, g)
	if !reflect.DeepEqual(got.UndecidedDraftIDs, []string{cmdD2, cmdD3}) {
		t.Errorf("undecided = %v, want [%s %s]", got.UndecidedDraftIDs, cmdD2, cmdD3)
	}
}

// TestCommsDispositions_ClusterSplits: the gather recorded {30, 31} (both in
// D2: kept whole, NOT listed) and {12, 40} (12 in D1, 40 not_drafted: SPLIT).
//
// COUNTERFACTUAL (the same-placement filter): delete it — the first cluster is
// listed too.
func TestCommsDispositions_ClusterSplits(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{})
	got := decodeCMD(t, getCMD(t, f.s, f.runID.String()))
	want := []commsClusterSplit{{
		ReportIDs: []string{"UR-issue-12", "UR-issue-40"}, Score: 0.8,
		Placements: []commsClusterPlacement{
			{ReportID: "UR-issue-12", Kind: "draft", EntryID: cmdD1},
			{ReportID: "UR-issue-40", Kind: "not_drafted", EntryID: "question"},
		},
	}}
	if !got.ClustersRecorded || !reflect.DeepEqual(got.ClusterSplits, want) {
		t.Fatalf("clusters_recorded=%v splits=%+v, want true %+v", got.ClustersRecorded, got.ClusterSplits, want)
	}
}

// TestCommsClusterSplits_Placements: every placement kind, including an id the
// report cites nowhere (unaccounted) and an n_drift member.
func TestCommsClusterSplits_Placements(t *testing.T) {
	report, err := plan.ParseCommsReport(cmdReportBody(t, cmdFixtureOpts{}))
	if err != nil {
		t.Fatal(err)
	}
	got := commsClusterSplits(report, []commsRecordedCluster{
		{ReportIDs: []string{"UR-issue-41", "UR-issue-99", "UR-issue-50"}, Score: 0.7},
		{ReportIDs: []string{"UR-issue-99", "UR-issue-98"}, Score: 0.6},
	})
	want := []commsClusterSplit{{
		ReportIDs: []string{"UR-issue-41", "UR-issue-99", "UR-issue-50"}, Score: 0.7,
		Placements: []commsClusterPlacement{
			{ReportID: "UR-issue-41", Kind: "n_drift", EntryID: "ndrift:N2:UR-issue-41"},
			{ReportID: "UR-issue-99", Kind: "unaccounted"},
			{ReportID: "UR-issue-50", Kind: "draft", EntryID: cmdD3},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splits = %+v, want %+v (two unaccounted members share a placement: not split)", got, want)
	}
	if got := commsClusterSplits(report, nil); got == nil || len(got) != 0 {
		t.Errorf("no clusters = %#v, want []", got)
	}
}

// TestCommsDispositions_ClustersNotRecorded: a gather recorded before #4016
// carries no suggested_clusters key — clusters_recorded=false and splits [].
//
// COUNTERFACTUAL: hard-code clusters_recorded true — RED.
func TestCommsDispositions_ClustersNotRecorded(t *testing.T) {
	f := newCMDFixture(t, cmdFixtureOpts{legacyGather: true})
	g := getCMD(t, f.s, f.runID.String())
	got := decodeCMD(t, g)
	if got.ClustersRecorded || got.ClusterSplits == nil || len(got.ClusterSplits) != 0 {
		t.Fatalf("clusters_recorded=%v splits=%#v, want false []", got.ClustersRecorded, got.ClusterSplits)
	}
	if !strings.Contains(g.Body.String(), `"cluster_splits":[]`) || !strings.Contains(g.Body.String(), `"unaccounted_report_ids":["UR-issue-99"]`) {
		t.Errorf("arrays must serialize as arrays: %s", g.Body.String())
	}
}

// --- the code inventory --------------------------------------------------------

// TestCommsDispositions_EveryCodeReachable drives one case per code and
// requires the OBSERVED code set to equal commsDispositionsCodes — a code added
// to the list with no reachable case, or answered but missing from the list,
// is RED. Every case runs in memory (approval condition 2).
func TestCommsDispositions_EveryCodeReachable(t *testing.T) {
	ok := cmdBatch(cmdEntry(cmdD1, "approved"))
	withAtomic := func(err error) func(*cmdFixture) {
		return func(f *cmdFixture) {
			f.s = New(Config{RunRepo: f.runs, ArtifactRepo: f.arts, AuditRepo: &cmdAtomicAudit{ukAudit: f.au, batchErr: err}})
		}
	}
	type kase struct {
		setup func(*cmdFixture)
		runID func(*cmdFixture) string
		body  string
		ident func(*cmdFixture) func(*http.Request) *http.Request
	}
	op := func(*cmdFixture) func(*http.Request) *http.Request { return ukOperator }
	asID := func(id Identity) func(*cmdFixture) func(*http.Request) *http.Request {
		return func(*cmdFixture) func(*http.Request) *http.Request { return ukWithIdentity(id) }
	}
	cases := []kase{
		{setup: func(f *cmdFixture) { f.s = New(Config{}) }, body: ok, ident: op},
		{body: ok, ident: asID(Identity{})},
		{body: ok, ident: func(f *cmdFixture) func(*http.Request) *http.Request {
			return ukWithIdentity(Identity{Subject: "mcp:run:" + f.runID.String(), TokenID: "t", Scopes: []string{"write:approvals"}})
		}},
		{body: ok, ident: asID(Identity{Subject: operatorrole.TokenSubjectPrefix + "c", TokenID: "t", Scopes: []string{"write:approvals"}})},
		{body: ok, ident: asID(Identity{Subject: "github:ops", TokenID: "t", Scopes: []string{"read:runs"}})},
		{body: `{"dispositions":[]}`, ident: op},
		{runID: func(*cmdFixture) string { return uuid.NewString() }, body: ok, ident: op},
		{body: cmdBatch(cmdEntry(cmdD1, "maybe")), ident: op},
		{setup: func(f *cmdFixture) {
			f.au.entries = nil // drop every row: no recorded report
		}, body: ok, ident: op},
		{body: cmdBatch(cmdEntry("draft:UR-issue-99", "approved")), ident: op},
		{setup: withAtomic(&audit.WindowClosedError{Family: audit.WindowFamilyComms}), body: ok, ident: op},
		{setup: withAtomic(&audit.ReportSupersededError{Family: audit.WindowFamilyComms}), body: ok, ident: op},
		{setup: withAtomic(errors.New("tx boom")), body: ok, ident: op},
		{body: ok, ident: op}, // the 200, which contributes no code
	}
	observed := map[string]bool{}
	for i, c := range cases {
		f := newCMDFixture(t, cmdFixtureOpts{})
		if c.setup != nil {
			c.setup(f)
		}
		runID := f.runID.String()
		if c.runID != nil {
			runID = c.runID(f)
		}
		w := postCMD(t, f.s, runID, c.body, c.ident(f))
		code := cmdErrorCode(w)
		if code == "" && w.Code != http.StatusOK {
			t.Fatalf("case %d: status %d with no error code: %s", i, w.Code, w.Body.String())
		}
		if code != "" {
			observed[code] = true
		}
	}
	got := make([]string, 0, len(observed))
	for c := range observed {
		got = append(got, c)
	}
	want := append([]string(nil), commsDispositionsCodes...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observed codes = %v\nwant commsDispositionsCodes = %v", got, want)
	}
}
