package server

// Tests for the on-approval comms apply hook (#4017).
//
// THE HARNESS COMPOSES existing fakes (ukApplyRunRepo, ukApplyArtifactRepo,
// ukApplyAudit, groomingApplyApprovalRepo, ukApplyConventions, igCharterConfig)
// without modifying them. Every fixture records its gather and its
// comms_report_recorded row through the PRODUCTION writers
// (recordCommsScanGathered, commsPreviewDrafts, commsRecordedPayload +
// appendCommsRecorded), so the recorded previews and digests are the ones the
// ingest would have written. Tests synchronize through s.waitReportApply(),
// the provider's park channels or the apply's test hooks. Tests that swap
// package vars (budgets, conventionsLoader, hooks, the memory cap) do not call
// t.Parallel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// cmaAudit adds ListAll (the suppression-memory and draft-filed reads) over
// ukApplyAudit's sequence-assigning chain: newest first, the Category filter
// and the row Limit honoured, with a per-category ListAll failure. It does
// NOT implement audit.FamilyWindowAppender, so settlement takes the fallback.
type cmaAudit struct {
	*ukApplyAudit
	listAllErr map[string]error
}

func (a *cmaAudit) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	if p.Category != nil {
		if err := a.listAllErr[*p.Category]; err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for i := len(a.appended) - 1; i >= 0; i-- {
		ap := a.appended[i]
		if p.Category != nil && ap.Category != *p.Category {
			continue
		}
		rid := ap.RunID
		out = append(out, &audit.Entry{
			Sequence: int64(i + 1), RunID: &rid, StageID: ap.StageID,
			Category: ap.Category, Payload: ap.Payload, Timestamp: ap.Timestamp,
		})
		if p.Limit > 0 && len(out) >= p.Limit {
			break
		}
	}
	return out, nil
}

// cmaRunRepo can fail GetRun.
type cmaRunRepo struct {
	*ukApplyRunRepo
	getErr error
}

func (r *cmaRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.ukApplyRunRepo.GetRun(ctx, id)
}

// cmaProvider records every File request. failTitle fails the filing with that
// title; park (when non-nil) makes EVERY File signal entered and wait for
// release; waitCtx makes File return only once its context is done.
type cmaProvider struct {
	name      string
	mu        sync.Mutex
	reqs      []workmgmt.ProviderRequest
	failTitle string
	entered   chan struct{}
	release   chan struct{}
	waitCtx   bool
}

func (p *cmaProvider) Name() string { return p.name }

func (p *cmaProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	if p.entered != nil {
		p.entered <- struct{}{}
		<-p.release
	}
	if p.waitCtx {
		<-ctx.Done()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if p.failTitle != "" && req.Item.Title == p.failTitle {
		return nil, errors.New("cmaProvider: injected filing failure")
	}
	n := 8000 + len(p.reqs)
	return &workmgmt.CreatedItem{
		Provider: p.name, Number: n, URL: fmt.Sprintf("https://example.test/issues/%d", n),
		AppliedLabels: req.Item.Classification.Labels,
	}, nil
}

func (p *cmaProvider) requests() []workmgmt.ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]workmgmt.ProviderRequest(nil), p.reqs...)
}

// cmaAdvanceCall is one recorded cursor Advance.
type cmaAdvanceCall struct {
	Source userreport.Source
	To     time.Time
}

// cmaCursorStore is a monotonic in-memory cursor store that records every
// Advance in call order; failSource fails Advance for that source.
type cmaCursorStore struct {
	mu         sync.Mutex
	at         map[userreport.Source]time.Time
	calls      []cmaAdvanceCall
	failSource userreport.Source
}

func newCMACursorStore() *cmaCursorStore {
	return &cmaCursorStore{at: map[userreport.Source]time.Time{}}
}

func (c *cmaCursorStore) Get(_ context.Context, k userreport.Key) (time.Time, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.at[k.Source]
	return at, ok, nil
}

func (c *cmaCursorStore) Init(_ context.Context, k userreport.Key, initial time.Time) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.at[k.Source]; ok {
		return at, nil
	}
	c.at[k.Source] = initial
	return initial, nil
}

func (c *cmaCursorStore) Advance(_ context.Context, k userreport.Key, to time.Time) (time.Time, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, cmaAdvanceCall{Source: k.Source, To: to})
	if c.failSource == k.Source {
		return time.Time{}, false, errors.New("cmaCursorStore: injected advance failure")
	}
	if cur, ok := c.at[k.Source]; ok && !cur.Before(to) {
		return cur, false, nil
	}
	c.at[k.Source] = to
	return to, true, nil
}

func (c *cmaCursorStore) advances() []cmaAdvanceCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cmaAdvanceCall(nil), c.calls...)
}

func (c *cmaCursorStore) value(src userreport.Source) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at[src]
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const cmaRepo = "kuhlman-labs/fishhawk"

// cmaT is the fixture clock: report n was updated at cmaT(n).
func cmaT(n int) time.Time { return time.Date(2026, 10, 6, n, 0, 0, 0, time.UTC) }

// cmaShown is the fixture gather's shown set: id → (issue, updated hour).
// UR-issue-50 is the EARLIEST report; UR-issue-99 is cited nowhere.
var cmaShown = []struct {
	id    string
	issue int
	hour  int
}{
	{"UR-issue-12", 12, 3}, {"UR-issue-30", 30, 4}, {"UR-issue-31", 31, 4},
	{"UR-issue-40", 40, 4}, {"UR-issue-41", 41, 4}, {"UR-issue-50", 50, 1},
	{"UR-issue-99", 99, 5},
}

// cmaHash is a report's gathered content hash; salt varies it.
func cmaHash(id, salt string) string { return sha256Hex([]byte(id + "@" + salt)) }

// cmaDraft is one fixture draft.
type cmaDraft struct {
	sources []string
	title   string
	epic    string
}

func (d cmaDraft) id() string { return plan.CommsDraftID(d.sources) }

var (
	cmaD1 = cmaDraft{sources: []string{"UR-issue-12"}, title: "CSV export writes no rows"}
	cmaD2 = cmaDraft{sources: []string{"UR-issue-30", "UR-issue-31"}, title: "Dashboard times out on large runs"}
	cmaD3 = cmaDraft{sources: []string{"UR-issue-50"}, title: "Login page rejects valid tokens"}
)

const cmaNDriftID = "ndrift:N2:UR-issue-41"

type cmaOpts struct {
	drafts       []cmaDraft
	repo         string
	account      string
	omit99       bool   // UR-issue-99 is not_drafted: no unaccounted report
	noPending    bool   // the scan did not complete
	hashSalt     string // varies every gathered hash
	charterHash  string // the gathered charter hash ("" = a fixed fake)
	legacyGather bool
	// previews mutates the recorded preview set before it is recorded.
	previews func(t *testing.T, set *commsPreviewSet, report *plan.CommsReport, g *commsScanGatheredPayload)
	// omitRecorded leaves the comms_report_recorded row out.
	omitRecorded bool
}

type cmaFixture struct {
	s         *Server
	runs      *cmaRunRepo
	approvals *groomingApplyApprovalRepo
	au        *cmaAudit
	arts      *ukApplyArtifactRepo
	provider  *cmaProvider
	cursors   *cmaCursorStore
	conv      workmgmt.Conventions
}

// cmaRun is one run carrying a recorded comms_report.
type cmaRun struct {
	stage  *run.Stage
	run    *run.Run
	art    *artifact.Artifact
	gather *audit.Entry
	gp     *commsScanGatheredPayload
	report *plan.CommsReport
}

func newCMAFixture(t *testing.T) *cmaFixture {
	t.Helper()
	f := &cmaFixture{
		runs:      &cmaRunRepo{ukApplyRunRepo: &ukApplyRunRepo{approvalRunRepo: newApprovalRunRepo()}},
		approvals: &groomingApplyApprovalRepo{fakeApprovalRepo: newFakeApprovalRepo()},
		au: &cmaAudit{ukApplyAudit: &ukApplyAudit{groomingApplyAuditFake: &groomingApplyAuditFake{
			approvalAuditFake: newApprovalAuditFake()}}},
		arts:     &ukApplyArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}, byStage: map[uuid.UUID][]*artifact.Artifact{}},
		provider: &cmaProvider{name: "comms-apply-fake-" + uuid.NewString()},
		cursors:  newCMACursorStore(),
	}
	f.s = New(Config{Addr: "127.0.0.1:0", ApprovalRepo: f.approvals, RunRepo: f.runs, AuditRepo: f.au,
		ArtifactRepo: f.arts, UserReportCursors: f.cursors})
	workmgmt.Register(f.provider)
	// The conventions KEEP their defaulted autonomy tier (production's shape),
	// so the apply's suppression of it is always in play.
	f.conv = ukApplyConventions(f.provider.name, true)
	installConventions(t, f.conv, nil)
	return f
}

// withCharter wires the igCharterDoc document seams and a charter-declaring
// conventions, returning the fetcher and the charter's content hash.
func (f *cmaFixture) withCharter(t *testing.T) (*igFetcher, string) {
	t.Helper()
	fetcher := &igFetcher{content: igCharterDoc}
	cfg := igCharterConfigWith(fetcher)
	f.s.cfg.DocumentResolver, f.s.cfg.DocumentBaseRef = cfg.DocumentResolver, cfg.DocumentBaseRef
	f.conv.Charter = &workmgmt.Charter{Path: igCharterPath}
	installConventions(t, f.conv, nil)
	ch, reason, detail := f.s.resolveCharterDocument(context.Background(), f.conv,
		workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}})
	if reason != "" || ch.ContentHash == "" {
		t.Fatalf("fixture charter read = (%q, %q, %s)", ch.ContentHash, reason, detail)
	}
	return fetcher, ch.ContentHash
}

func cmaGather(opts cmaOpts) commsScanGatheredPayload {
	repo := opts.repo
	if repo == "" {
		repo = cmaRepo
	}
	salt := opts.hashSalt
	if salt == "" {
		salt = "v1"
	}
	var shown []commsShownReport
	for _, r := range cmaShown {
		shown = append(shown, commsShownReport{ID: r.id, Kind: "issue", IssueNumber: r.issue,
			ContentHash: cmaHash(r.id, salt), UpdatedAt: cmaT(r.hour)})
	}
	charterHash := opts.charterHash
	if charterHash == "" {
		charterHash = strings.Repeat("d", 64)
	}
	g := commsScanGatheredPayload{
		StageAttempt: "1", Repo: repo, Shown: shown,
		Charter: commsCharterRecord{Path: igCharterPath, ContentHash: charterHash,
			RubricIDs: []string{"S2", "S4", "U4", "V1"}, NonGoalIDs: []string{"N1", "N2"}},
	}
	if !opts.noPending {
		g.PendingCursor = &commsPendingCursor{Since: cmaT(0), NoteSince: cmaT(0), Cursor: cmaT(8), NoteCursor: cmaT(8)}
	}
	return g
}

func cmaReportBody(t *testing.T, drafts []cmaDraft, omit99 bool) []byte {
	t.Helper()
	return commsExampleMutated(t, func(m map[string]any) {
		var ds []any
		for _, d := range drafts {
			src := make([]any, len(d.sources))
			for i, s := range d.sources {
				src[i] = s
			}
			issue := map[string]any{
				"type": "bug", "title": d.title,
				"body":   "Users report " + strings.ToLower(d.title) + " & more.",
				"labels": []any{"area:backend", "type:bug"},
			}
			if d.epic != "" {
				issue["parent_epic"] = d.epic
			}
			ds = append(ds, map[string]any{
				"id": d.id(), "source_report_ids": src,
				"rubric_citations": []any{map[string]any{"rubric_id": "S2"}},
				"proposed_issue":   issue,
			})
		}
		m["drafts"] = ds
		m["n_drift"] = []any{map[string]any{
			"id": cmaNDriftID, "non_goal_id": "N2",
			"source_report_ids": []any{"UR-issue-41"}, "note": "Requests a hosted offering.",
		}}
		nd := []any{map[string]any{"report_id": "UR-issue-40", "reason": "question"}}
		if omit99 {
			nd = append(nd, map[string]any{"report_id": "UR-issue-99", "reason": "noise"})
		}
		m["not_drafted"] = nd
	})
}

// addRun seeds one run whose plan stage carries a recorded comms_report.
func (f *cmaFixture) addRun(t *testing.T, opts cmaOpts) *cmaRun {
	t.Helper()
	ctx := context.Background()
	drafts := opts.drafts
	if drafts == nil {
		drafts = []cmaDraft{cmaD1, cmaD2, cmaD3}
	}
	stage := f.runs.seedStage(run.StageStateAwaitingApproval)
	install := int64(4242)
	repo := opts.repo
	if repo == "" {
		repo = cmaRepo
	}
	rn := &run.Run{ID: stage.RunID, Repo: repo, AccountID: opts.account, InstallationID: &install, CreatedAt: time.Now().UTC()}
	f.runs.seedRun(rn)
	r := &cmaRun{stage: stage, run: rn}

	gp := cmaGather(opts)
	r.gather = f.recordGather(t, rn.ID, stage.ID, gp, opts.legacyGather)
	gathered, err := decodeCommsScanGathered(r.gather.Payload)
	if err != nil {
		t.Fatal(err)
	}
	r.gp = gathered

	body := cmaReportBody(t, drafts, opts.omit99)
	report, err := plan.ParseCommsReport(body)
	if err != nil {
		t.Fatalf("fixture report does not parse: %v", err)
	}
	if ref := checkCommsCharterRefs(report, gathered); ref != nil {
		t.Fatalf("fixture report fails the ingest's charter checks: %+v", ref)
	}
	r.report = report
	r.art = &artifact.Artifact{ID: uuid.New(), StageID: stage.ID, Kind: artifact.KindCommsReport,
		Content: body, ContentHash: sha256Hex(body), CreatedAt: time.Now().UTC()}
	f.arts.add(r.art)
	if opts.omitRecorded {
		return r
	}
	previews := f.s.commsPreviewDrafts(ctx, rn, report, gathered)
	if opts.previews != nil {
		opts.previews(t, &previews, report, gathered)
	}
	payload := commsRecordedPayload(rn.ID, stage.ID, r.art.ID.String(), r.art.ContentHash, len(body), report,
		commsBoundGather{entry: r.gather, payload: gathered}, previews)
	if err := f.s.appendCommsRecorded(ctx, rn.ID, stage.ID, payload); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *cmaFixture) recordGather(t *testing.T, runID, stageID uuid.UUID, p commsScanGatheredPayload, legacy bool) *audit.Entry {
	t.Helper()
	if !legacy {
		e, _, err := f.s.recordCommsScanGathered(context.Background(), runID, stageID, p)
		if err != nil {
			t.Fatalf("record gather: %v", err)
		}
		return e
	}
	// A row written before #4016: no suggested_clusters key, digest stamped
	// over the legacy shape at record time.
	p = p.normalized()
	p.StageID = stageID
	var m map[string]any
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &m)
	delete(m, "suggested_clusters")
	m["gather_digest"] = strings.Repeat("e", 64)
	raw, _ = json.Marshal(m)
	return f.appendRow(t, runID, stageID, CategoryCommsScanGathered, raw)
}

func (f *cmaFixture) appendRow(t *testing.T, runID, stageID uuid.UUID, category string, payload any) *audit.Entry {
	t.Helper()
	raw, ok := payload.([]byte)
	if !ok {
		raw, _ = json.Marshal(payload)
	}
	e, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	})
	if err != nil {
		t.Fatalf("seed %s row: %v", category, err)
	}
	return e
}

func (f *cmaFixture) seedApproval(t *testing.T, r *cmaRun, d approval.Decision) {
	t.Helper()
	if _, err := f.approvals.Submit(context.Background(), approval.SubmitParams{
		StageID: r.stage.ID, ApproverSubject: "kuhlman-labs-" + uuid.NewString(), Decision: d, Surface: "test",
	}); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
}

// dispose seeds one comms_disposition_recorded row BY CONSTRUCTION (bypassing
// the capture's own checks, e.g. C11).
func (f *cmaFixture) dispose(t *testing.T, r *cmaRun, draftID, verdict, epic string) {
	t.Helper()
	f.appendRow(t, r.run.ID, r.stage.ID, CategoryCommsDispositionRecorded, commsDispositionPayload{
		RunID: r.run.ID.String(), StageID: r.stage.ID.String(), ArtifactID: r.art.ID.String(),
		ContentHash: r.art.ContentHash, DraftID: draftID, Verdict: verdict, ParentEpic: epic,
	})
}

func (f *cmaFixture) apply(r *cmaRun, d approval.Decision) {
	f.s.applyApprovedComms(context.Background(), r.stage, d)
	f.s.waitReportApply()
}

func (f *cmaFixture) rows(t *testing.T, r *cmaRun, category string) []*audit.Entry {
	t.Helper()
	rows, err := f.au.groomingApplyAuditFake.ListForRunByCategory(context.Background(), r.run.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (f *cmaFixture) watermarks(t *testing.T, r *cmaRun) []string {
	t.Helper()
	var out []string
	for _, e := range f.rows(t, r, audit.CommsApplyWindowClosedCategory) {
		var p groomingWindowPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Settlement)
	}
	return out
}

func (f *cmaFixture) completed(t *testing.T, r *cmaRun) []commsApplyCompletedPayload {
	t.Helper()
	var out []commsApplyCompletedPayload
	for _, e := range f.rows(t, r, CategoryCommsApplyCompleted) {
		var p commsApplyCompletedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// only asserts exactly one completion row and returns it.
func (f *cmaFixture) only(t *testing.T, r *cmaRun) commsApplyCompletedPayload {
	t.Helper()
	c := f.completed(t, r)
	if len(c) != 1 {
		t.Fatalf("comms_apply_completed rows = %+v, want exactly one", c)
	}
	return c[0]
}

func (f *cmaFixture) skips(t *testing.T, r *cmaRun) map[string]commsDraftSkippedPayload {
	t.Helper()
	out := map[string]commsDraftSkippedPayload{}
	for _, e := range f.rows(t, r, CategoryCommsDraftSkipped) {
		var p commsDraftSkippedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.DraftID] = p
	}
	return out
}

func (f *cmaFixture) filed(t *testing.T, r *cmaRun) map[string]commsDraftFiledPayload {
	t.Helper()
	out := map[string]commsDraftFiledPayload{}
	for _, e := range f.rows(t, r, CategoryCommsDraftFiled) {
		var p commsDraftFiledPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.DraftID] = p
	}
	return out
}

func (f *cmaFixture) applyRowCount(t *testing.T, r *cmaRun) int {
	t.Helper()
	n := 0
	for _, c := range []string{audit.CommsApplyWindowClosedCategory, CategoryCommsApplyCompleted,
		CategoryCommsDraftFiled, CategoryCommsDraftSkipped} {
		n += len(f.rows(t, r, c))
	}
	return n
}

// requireDegraded asserts one degraded completion row with reason, an empty
// suppressions array, the repo stamped, and the cursor untouched.
func (f *cmaFixture) requireDegraded(t *testing.T, r *cmaRun, reason string) commsApplyCompletedPayload {
	t.Helper()
	c := f.only(t, r)
	if !c.Degraded || c.DegradeReason != reason {
		t.Fatalf("completion = degraded %v reason %q, want degraded %q", c.Degraded, c.DegradeReason, reason)
	}
	if c.Suppressions == nil || len(c.Suppressions) != 0 {
		t.Errorf("degraded row suppressions = %#v, want an empty array", c.Suppressions)
	}
	if c.Cursor.Advanced || len(f.cursors.advances()) != 0 {
		t.Errorf("cursor advanced on a degraded apply: %+v / %v", c.Cursor, f.cursors.advances())
	}
	return c
}

func (f *cmaFixture) fileCount() int { return len(f.provider.requests()) }

// supKeys renders suppressions as sorted "id/basis/entry" strings.
func supKeys(sups []commsApplySuppression) []string {
	out := []string{}
	for _, s := range sups {
		out = append(out, s.ID+"/"+s.Basis+"/"+s.EntryID)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// E0: the early-out
// ---------------------------------------------------------------------------

// TestApplyApprovedComms_OrdinaryPlanStageNoOps: a stage with no comms_report
// artifact, an unreadable list, no artifact repo, no recorded row, or a
// recorded report on ANOTHER stage writes nothing on approve or reject.
func TestApplyApprovedComms_OrdinaryPlanStageNoOps(t *testing.T) {
	cases := map[string]struct {
		opts   cmaOpts
		mutate func(*testing.T, *cmaFixture, *cmaRun)
	}{
		"no_comms_artifact": {mutate: func(_ *testing.T, f *cmaFixture, _ *cmaRun) {
			f.arts.byStage = map[uuid.UUID][]*artifact.Artifact{}
		}},
		"artifact_list_error": {mutate: func(_ *testing.T, f *cmaFixture, _ *cmaRun) {
			f.arts.listErr = errors.New("artifact store down")
		}},
		"nil_artifact_repo": {mutate: func(_ *testing.T, f *cmaFixture, _ *cmaRun) { f.s.cfg.ArtifactRepo = nil }},
		"no_recorded_row":   {opts: cmaOpts{omitRecorded: true}},
		"recorded_report_on_other_stage": {mutate: func(t *testing.T, f *cmaFixture, r *cmaRun) {
			other := &artifact.Artifact{ID: uuid.New(), StageID: uuid.New(), Kind: artifact.KindCommsReport}
			f.arts.add(other)
			f.appendRow(t, r.run.ID, r.stage.ID, CategoryCommsReportRecorded, map[string]any{"artifact_id": other.ID.String()})
		}},
	}
	for name, tc := range cases {
		for _, d := range []approval.Decision{approval.DecisionApprove, approval.DecisionReject} {
			t.Run(name+"/"+string(d), func(t *testing.T) {
				f := newCMAFixture(t)
				r := f.addRun(t, tc.opts)
				f.seedApproval(t, r, approval.DecisionApprove)
				f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
				if tc.mutate != nil {
					tc.mutate(t, f, r)
				}
				f.apply(r, d)
				if n := f.applyRowCount(t, r); n != 0 {
					t.Errorf("apply rows = %d, want 0 on an ordinary plan stage", n)
				}
				if n := f.fileCount(); n != 0 {
					t.Errorf("File calls = %d, want 0", n)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// C1: the reject path
// ---------------------------------------------------------------------------

// TestApplyApprovedComms_RejectSettlesRejectedFilesNothing: the fixture
// carries a grant AND an approved disposition, so deleting the decision guard
// would file. COUNTERFACTUAL: delete the `decision != approve` branch → File
// is called → red.
func TestApplyApprovedComms_RejectSettlesRejectedFilesNothing(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionReject)

	if n := f.fileCount(); n != 0 {
		t.Fatalf("File calls = %d on a rejected gate, want 0", n)
	}
	if wm := f.watermarks(t, r); !reflect.DeepEqual(wm, []string{"rejected"}) {
		t.Errorf("watermarks = %v, want [rejected]", wm)
	}
	c := f.only(t, r)
	if c.Decision != commsApplyDecisionReject || c.Degraded || c.Repo != cmaRepo {
		t.Errorf("completion = %+v, want a non-degraded reject row for %s", c, cmaRepo)
	}
	if c.Cursor.Advanced || c.Cursor.Reason != commsCursorReasonDecisionReject || len(f.cursors.advances()) != 0 {
		t.Errorf("cursor = %+v (calls %v), want untouched with reason decision_reject", c.Cursor, f.cursors.advances())
	}
	if n := len(f.rows(t, r, CategoryCommsDraftFiled)) + len(f.rows(t, r, CategoryCommsDraftSkipped)); n != 0 {
		t.Errorf("per-draft rows = %d on reject, want 0", n)
	}
}

// TestApplyApprovedComms_GateRejectPerDraftRejections: D1 rejected and D2
// approved on a rejected gate → `rejected` suppressions for D1's sources only.
func TestApplyApprovedComms_GateRejectPerDraftRejections(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.dispose(t, r, cmaD1.id(), commsVerdictRejected, "")
	f.dispose(t, r, cmaD2.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionReject)

	c := f.only(t, r)
	want := []string{"UR-issue-12/rejected/" + cmaD1.id()}
	if got := supKeys(c.Suppressions); !reflect.DeepEqual(got, want) {
		t.Errorf("suppressions = %v, want %v", got, want)
	}
	if c.Suppressions[0].Hash != cmaHash("UR-issue-12", "v1") {
		t.Errorf("suppression hash = %q, want the gathered hash", c.Suppressions[0].Hash)
	}
}

// TestApplyApprovedComms_WholeGateRejectSuppressesAllDrafts: no disposition at
// all → every draft's cited reports get `rejected`. COUNTERFACTUAL: return
// before writing the reject-path completion row → no suppressions → red.
func TestApplyApprovedComms_WholeGateRejectSuppressesAllDrafts(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.apply(r, approval.DecisionReject)

	c := f.only(t, r)
	want := []string{
		"UR-issue-12/rejected/" + cmaD1.id(), "UR-issue-30/rejected/" + cmaD2.id(),
		"UR-issue-31/rejected/" + cmaD2.id(), "UR-issue-50/rejected/" + cmaD3.id(),
	}
	sort.Strings(want)
	if got := supKeys(c.Suppressions); !reflect.DeepEqual(got, want) {
		t.Errorf("suppressions = %v, want %v", got, want)
	}
}

// TestApplyApprovedComms_RejectSettlementFailureNeverSuppresses (approval
// condition 1): the `rejected` watermark append fails while an APPROVED
// disposition exists. The consumed set is UNKNOWN, so the apply writes ONE
// degraded reject row with NO suppressions. COUNTERFACTUAL: treat the failed
// settlement's nil consumed set as empty (whole-gate) → every draft
// suppressed → red.
func TestApplyApprovedComms_RejectSettlementFailureNeverSuppresses(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.au.failCategories = map[string]bool{audit.CommsApplyWindowClosedCategory: true}
	f.apply(r, approval.DecisionReject)

	for _, row := range f.completed(t, r) {
		if len(row.Suppressions) != 0 {
			t.Fatalf("a failed reject settlement wrote suppressions %+v; the consumed set is unknown, not empty", row.Suppressions)
		}
	}
	c := f.requireDegraded(t, r, commsApplyWindowUnsettled)
	if c.Decision != commsApplyDecisionReject || c.Cursor.Reason != commsCursorReasonDecisionReject {
		t.Errorf("completion = %+v, want decision reject with cursor reason decision_reject", c)
	}
	if c.Repo != cmaRepo {
		t.Errorf("degraded row repo = %q, want %q", c.Repo, cmaRepo)
	}
	if n := f.fileCount(); n != 0 {
		t.Errorf("File calls = %d, want 0", n)
	}
}

// TestApplyApprovedComms_RejectClosesWindowEvenWhenBodyUnreadable: the window
// settles `rejected` before any body read; the unreadable body then degrades
// the reject row with no suppressions.
func TestApplyApprovedComms_RejectClosesWindowEvenWhenBodyUnreadable(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	r.art.Content = []byte(`{"kind":"comms_report","not":"parseable"`)
	f.apply(r, approval.DecisionReject)

	if wm := f.watermarks(t, r); !reflect.DeepEqual(wm, []string{"rejected"}) {
		t.Fatalf("watermarks = %v, want [rejected] even with an unreadable body", wm)
	}
	c := f.requireDegraded(t, r, commsApplyReportUnreadable)
	if c.Decision != commsApplyDecisionReject {
		t.Errorf("decision = %q, want reject", c.Decision)
	}
}

// ---------------------------------------------------------------------------
// C3 and the pre-/post-ratification degrades
// ---------------------------------------------------------------------------

// TestApplyApprovedComms_ContestedGateFilesNothingWindowOpen: a grant AND a
// rejection → not ratified, nothing filed, window left OPEN, cursor
// untouched. COUNTERFACTUAL: make the ratify check always OK → files → red.
func TestApplyApprovedComms_ContestedGateFilesNothingWindowOpen(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.seedApproval(t, r, approval.DecisionReject)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)

	f.requireDegraded(t, r, commsApplyNotRatified)
	if n := f.fileCount(); n != 0 {
		t.Errorf("File calls = %d on a contested gate, want 0", n)
	}
	if wm := f.watermarks(t, r); len(wm) != 0 {
		t.Errorf("watermarks = %v, want the window left open", wm)
	}
}

// TestApplyApprovedComms_NoApprovalRepoNotRatified: no approval repository
// cannot ratify.
func TestApplyApprovedComms_NoApprovalRepoNotRatified(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.s.cfg.ApprovalRepo = nil
	f.apply(r, approval.DecisionApprove)
	f.requireDegraded(t, r, commsApplyNotRatified)
}

// TestApplyApprovedComms_WindowUnsettledDegrades: the `approved` watermark
// does not land → degraded, nothing filed.
func TestApplyApprovedComms_WindowUnsettledDegrades(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.au.failCategories = map[string]bool{audit.CommsApplyWindowClosedCategory: true}
	f.apply(r, approval.DecisionApprove)

	f.requireDegraded(t, r, commsApplyWindowUnsettled)
	if n := f.fileCount(); n != 0 {
		t.Errorf("File calls = %d, want 0", n)
	}
}

// TestApplyApprovedComms_ReportUnreadableDegrades: an unreadable body or an
// unreadable recorded row degrades before ratification, window open.
func TestApplyApprovedComms_ReportUnreadableDegrades(t *testing.T) {
	t.Run("body", func(t *testing.T) {
		f := newCMAFixture(t)
		r := f.addRun(t, cmaOpts{})
		f.seedApproval(t, r, approval.DecisionApprove)
		r.art.Content = []byte(`not json`)
		f.apply(r, approval.DecisionApprove)
		f.requireDegraded(t, r, commsApplyReportUnreadable)
		if wm := f.watermarks(t, r); len(wm) != 0 {
			t.Errorf("watermarks = %v, want the window left open", wm)
		}
	})
	t.Run("recorded_row", func(t *testing.T) {
		f := newCMAFixture(t)
		r := f.addRun(t, cmaOpts{})
		f.seedApproval(t, r, approval.DecisionApprove)
		f.appendRow(t, r.run.ID, r.stage.ID, CategoryCommsReportRecorded, []byte(`{"artifact_id":"not-a-uuid"}`))
		f.apply(r, approval.DecisionApprove)
		f.requireDegraded(t, r, commsApplyReportUnreadable)
	})
}

// TestApplyApprovedComms_PostRatificationDegrades: each post-ratification
// input that cannot be produced names its own degrade_reason, leaves the
// window CLOSED, files nothing and leaves the cursor untouched.
func TestApplyApprovedComms_PostRatificationDegrades(t *testing.T) {
	cases := map[string]struct {
		opts   cmaOpts
		mutate func(*testing.T, *cmaFixture, *cmaRun)
		want   string
	}{
		"run_unreadable": {want: commsApplyRunUnreadable, mutate: func(_ *testing.T, f *cmaFixture, _ *cmaRun) {
			f.runs.getErr = errors.New("run store down")
		}},
		"account_unparseable": {want: commsApplyAccountUnparseable, opts: cmaOpts{account: "not-a-uuid"}},
		"repo_unresolvable":   {want: commsApplyRepoUnresolvable, opts: cmaOpts{repo: "no-slash"}},
		"conventions_unavailable": {want: commsApplyConventionsUnavailable, mutate: func(t *testing.T, f *cmaFixture, _ *cmaRun) {
			installConventions(t, workmgmt.Conventions{}, errors.New("conventions unreadable"))
		}},
		"prior_filings_unreadable": {want: commsApplyPriorFilingsUnreadable, mutate: func(t *testing.T, f *cmaFixture, r *cmaRun) {
			f.appendRow(t, r.run.ID, r.stage.ID, CategoryCommsDraftFiled, []byte(`{"draft_id":""}`))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCMAFixture(t)
			r := f.addRun(t, tc.opts)
			f.seedApproval(t, r, approval.DecisionApprove)
			f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
			if tc.mutate != nil {
				tc.mutate(t, f, r)
			}
			f.apply(r, approval.DecisionApprove)
			f.requireDegraded(t, r, tc.want)
			if wm := f.watermarks(t, r); !reflect.DeepEqual(wm, []string{"approved"}) {
				t.Errorf("watermarks = %v, want the window CLOSED approved", wm)
			}
			if n := f.fileCount(); n != 0 {
				t.Errorf("File calls = %d, want 0", n)
			}
		})
	}
}

// TestApplyApprovedComms_PrelaunchTimeoutDegradesNamed: a stalled store past
// the prelaunch budget is recorded as comms_apply_prelaunch_timeout.
func TestApplyApprovedComms_PrelaunchTimeoutDegradesNamed(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	orig := commsApplyPrelaunchBudget
	commsApplyPrelaunchBudget = timescale.D(100 * time.Millisecond)
	t.Cleanup(func() { commsApplyPrelaunchBudget = orig })
	f.au.wedgeCategories = map[string]bool{audit.CommsApplyWindowClosedCategory: true}
	f.apply(r, approval.DecisionApprove)
	f.requireDegraded(t, r, commsApplyPrelaunchTimeout)
}

// TestApplyApprovedComms_GuardUnavailableFilesNothing: the double-filing guard
// cannot be read → comms_apply_guard_unavailable, nothing filed.
// COUNTERFACTUAL: make the guard-error branch continue → File → red.
func TestApplyApprovedComms_GuardUnavailableFilesNothing(t *testing.T) {
	for _, cat := range []string{CategoryCommsApplyCompleted, CategoryCommsDraftFiled} {
		t.Run(cat, func(t *testing.T) {
			f := newCMAFixture(t)
			r := f.addRun(t, cmaOpts{})
			f.seedApproval(t, r, approval.DecisionApprove)
			f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
			f.au.listAllErr = map[string]error{cat: errors.New("audit listing down")}
			f.apply(r, approval.DecisionApprove)
			f.requireDegraded(t, r, commsApplyGuardUnavailable)
			if n := f.fileCount(); n != 0 {
				t.Errorf("File calls = %d, want 0 with the guard unreadable", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The per-draft loop
// ---------------------------------------------------------------------------

// TestApplyApprovedComms_FilesApprovedDraftsExactly: approved / rejected /
// undecided → File only for the approved draft, through the work-item core,
// with the read-contract keys and the GATHERED hashes; no run is created.
func TestApplyApprovedComms_FilesApprovedDraftsExactly(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.dispose(t, r, cmaD2.id(), commsVerdictRejected, "")
	f.apply(r, approval.DecisionApprove)

	reqs := f.provider.requests()
	if len(reqs) != 1 || reqs[0].Item.Title != cmaD1.title {
		t.Fatalf("File requests = %+v, want exactly D1", reqs)
	}
	if !strings.Contains(reqs[0].Item.Body, userreport.DraftMarker([]userreport.MarkedReport{{ID: "UR-issue-12", ContentHash: cmaHash("UR-issue-12", "v1")}})) {
		t.Errorf("filed body carries no comms draft marker over the gathered hash:\n%s", reqs[0].Item.Body)
	}
	fd, ok := f.filed(t, r)[cmaD1.id()]
	if !ok {
		t.Fatal("no comms_draft_filed row for D1")
	}
	wantReports := []userreport.MarkedReport{{ID: "UR-issue-12", ContentHash: cmaHash("UR-issue-12", "v1")}}
	if fd.Repo != cmaRepo || fd.IssueNumber == 0 || fd.CommentID != 0 || !reflect.DeepEqual(fd.Reports, wantReports) {
		t.Errorf("filed row = repo %q issue %d comment %d reports %v, want %s / >0 / 0 / %v",
			fd.Repo, fd.IssueNumber, fd.CommentID, fd.Reports, cmaRepo, wantReports)
	}
	skips := f.skips(t, r)
	if skips[cmaD2.id()].SkipReason != commsSkipRejected || skips[cmaD3.id()].SkipReason != commsSkipUndecided {
		t.Errorf("skips = %+v, want D2 rejected and D3 undecided", skips)
	}
	c := f.only(t, r)
	if c.Degraded || c.Counts != (commsApplyCounts{Drafts: 3, Filed: 1, Skipped: 2}) {
		t.Errorf("completion counts = %+v degraded %v", c.Counts, c.Degraded)
	}
	if n := f.runs.creates.Load(); n != 0 {
		t.Errorf("CreateRun calls = %d, want 0: the comms apply never creates a run", n)
	}
}

// TestCommsApply_SourceNeverReferencesRunCreationOrSourceMutation pins the
// structural half of "never creates a run, never touches a source": the
// production file names no run-creation, mutation, transition or comment
// path.
func TestCommsApply_SourceNeverReferencesRunCreationOrSourceMutation(t *testing.T) {
	src, err := os.ReadFile("comms_apply.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	for _, banned := range []string{"CreateRun(", "createRun", "GroomingMutator", "Transitioner", "ApplyGrooming",
		"CreateComment", "AddLabels", "EditIssue", "CloseIssue"} {
		if strings.Contains(code, banned) {
			t.Errorf("comms_apply.go references %q", banned)
		}
	}
}

// TestApplyApprovedComms_LastWinsDisposition: the higher-sequence verdict per
// draft wins.
func TestApplyApprovedComms_LastWinsDisposition(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.dispose(t, r, cmaD1.id(), commsVerdictRejected, "")
	f.dispose(t, r, cmaD2.id(), commsVerdictRejected, "")
	f.dispose(t, r, cmaD2.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)

	if _, ok := f.filed(t, r)[cmaD2.id()]; !ok {
		t.Error("D2 (rejected then approved) was not filed")
	}
	if s := f.skips(t, r)[cmaD1.id()]; s.SkipReason != commsSkipRejected {
		t.Errorf("D1 (approved then rejected) skip = %q, want rejected", s.SkipReason)
	}
}

// TestApplyApprovedComms_SecondApplyAlreadyFiled: a second apply of the same
// artifact files nothing again. COUNTERFACTUAL: delete the already_filed case
// → the second apply files D1 again → red.
func TestApplyApprovedComms_SecondApplyAlreadyFiled(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)
	first := f.filed(t, r)[cmaD1.id()].IssueNumber
	f.apply(r, approval.DecisionApprove)

	if n := f.fileCount(); n != 1 {
		t.Fatalf("File calls = %d across two applies, want 1", n)
	}
	s := f.skips(t, r)[cmaD1.id()]
	if s.SkipReason != commsSkipAlreadyFiled || s.PriorIssueNumber != first {
		t.Errorf("second-apply skip = %+v, want already_filed naming #%d", s, first)
	}
	if wm := f.watermarks(t, r); !reflect.DeepEqual(wm, []string{"approved"}) {
		t.Errorf("watermarks = %v, want ONE approved (the window is permanent)", wm)
	}
}

// otherRunRow seeds a row of category on a DIFFERENT run (another apply).
func (f *cmaFixture) otherRunRow(t *testing.T, category string, payload any) {
	t.Helper()
	f.appendRow(t, uuid.New(), uuid.New(), category, payload)
}

// TestApplyApprovedComms_FiledElsewhereSkipsPerReport: one cited report
// already filed elsewhere makes the whole draft filed_elsewhere, via a prior
// apply's `filed` suppression or via another artifact's comms_draft_filed row
// alone (the window before that apply's completion row lands).
// COUNTERFACTUAL: delete the filed_elsewhere case → D2 files → red.
func TestApplyApprovedComms_FiledElsewhereSkipsPerReport(t *testing.T) {
	h30 := cmaHash("UR-issue-30", "v1")
	cases := map[string]struct {
		seed      func(*testing.T, *cmaFixture)
		wantBasis string
	}{
		"filed_suppression": {wantBasis: commsSuppressionBasisFiled, seed: func(t *testing.T, f *cmaFixture) {
			f.otherRunRow(t, CategoryCommsApplyCompleted, commsApplyCompletedPayload{Repo: cmaRepo,
				Suppressions: []commsApplySuppression{{ID: "UR-issue-30", Hash: h30, Basis: commsSuppressionBasisFiled, EntryID: "x"}}})
		}},
		"draft_filed_row_only": {wantBasis: commsConflictBasisDraftFiled, seed: func(t *testing.T, f *cmaFixture) {
			f.otherRunRow(t, CategoryCommsDraftFiled, commsDraftFiledPayload{Repo: cmaRepo, IssueNumber: 4242,
				Reports: []userreport.MarkedReport{{ID: "UR-issue-30", ContentHash: h30}}, DraftID: "draft:other"})
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCMAFixture(t)
			r := f.addRun(t, cmaOpts{})
			tc.seed(t, f)
			f.seedApproval(t, r, approval.DecisionApprove)
			f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
			f.dispose(t, r, cmaD2.id(), commsVerdictApproved, "")
			f.apply(r, approval.DecisionApprove)

			s := f.skips(t, r)[cmaD2.id()]
			if s.SkipReason != commsSkipFiledElsewhere || len(s.ConflictingReports) != 1 ||
				s.ConflictingReports[0].ID != "UR-issue-30" || s.ConflictingReports[0].Basis != tc.wantBasis {
				t.Fatalf("D2 skip = %+v, want filed_elsewhere naming UR-issue-30 (%s)", s, tc.wantBasis)
			}
			if n := f.fileCount(); n != 1 {
				t.Errorf("File calls = %d, want 1 (D1 only)", n)
			}
			// D2's sources are approved-but-unfiled: they hold the cursor.
			if c := f.only(t, r); !containsAll(c.Cursor.HeldReportIDs, "UR-issue-30", "UR-issue-31") {
				t.Errorf("held = %v, want D2's sources held", c.Cursor.HeldReportIDs)
			}
		})
	}
}

func containsAll(xs []string, want ...string) bool {
	set := map[string]bool{}
	for _, x := range xs {
		set[x] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestApplyApprovedComms_FiledElsewhereIsHashSensitive: a prior filing of
// UR-issue-30 at a DIFFERENT content hash (the report was edited since) does
// not block the draft.
func TestApplyApprovedComms_FiledElsewhereIsHashSensitive(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.otherRunRow(t, CategoryCommsApplyCompleted, commsApplyCompletedPayload{Repo: cmaRepo,
		Suppressions: []commsApplySuppression{{ID: "UR-issue-30", Hash: cmaHash("UR-issue-30", "old"), Basis: commsSuppressionBasisFiled}}})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD2.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)
	if _, ok := f.filed(t, r)[cmaD2.id()]; !ok {
		t.Errorf("D2 not filed; skips = %+v", f.skips(t, r))
	}
}

// TestApplyApprovedComms_PriorRejectedReportFiledAndFlagged: a cited report
// carrying a prior `rejected` suppression from another apply is FILED and
// flagged in prior_suppressions.
func TestApplyApprovedComms_PriorRejectedReportFiledAndFlagged(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	h12 := cmaHash("UR-issue-12", "v1")
	f.otherRunRow(t, CategoryCommsApplyCompleted, commsApplyCompletedPayload{Repo: cmaRepo,
		Suppressions: []commsApplySuppression{{ID: "UR-issue-12", Hash: h12, Basis: commsSuppressionBasisRejected}}})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)

	fd, ok := f.filed(t, r)[cmaD1.id()]
	if !ok {
		t.Fatalf("D1 not filed; skips = %+v", f.skips(t, r))
	}
	want := []commsSuppression{{ID: "UR-issue-12", Hash: h12, Basis: commsSuppressionBasisRejected}}
	if !reflect.DeepEqual(fd.PriorSuppressions, want) {
		t.Errorf("prior_suppressions = %+v, want %+v", fd.PriorSuppressions, want)
	}
}

// TestApplyApprovedComms_ParentEpicIsSourceSkips: an override parent_epic
// naming the issue the draft's own report lives on (appended directly,
// bypassing capture C11) is skipped. COUNTERFACTUAL: delete the case → D1
// files → red.
func TestApplyApprovedComms_ParentEpicIsSourceSkips(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "#12")
	f.apply(r, approval.DecisionApprove)

	if s := f.skips(t, r)[cmaD1.id()]; s.SkipReason != commsSkipParentEpicIsSource {
		t.Fatalf("D1 skip = %+v, want parent_epic_is_source", s)
	}
	if n := f.fileCount(); n != 0 {
		t.Errorf("File calls = %d, want 0", n)
	}
}

// TestApplyApprovedComms_ParentEpicOverrideElseProposal: the captain's
// override wins; otherwise the proposal's epic is used.
func TestApplyApprovedComms_ParentEpicOverrideElseProposal(t *testing.T) {
	f := newCMAFixture(t)
	d2 := cmaD2
	d2.epic = "#3775"
	r := f.addRun(t, cmaOpts{drafts: []cmaDraft{cmaD1, d2, cmaD3}})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "4000")
	f.dispose(t, r, d2.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)

	got := map[string]string{}
	for _, req := range f.provider.requests() {
		got[req.Item.Title] = req.Item.Relations.ParentEpic
	}
	if got[cmaD1.title] != "#4000" || got[d2.title] != "#3775" {
		t.Errorf("filed parent epics = %v, want D1 #4000 (override) and D2 #3775 (proposal)", got)
	}
}

// TestApplyApprovedComms_FilingBodyDriftSkips: a recorded digest of a
// DIFFERENT body is never filed. COUNTERFACTUAL: delete the digest comparison
// → D1 files → red.
func TestApplyApprovedComms_FilingBodyDriftSkips(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{previews: func(_ *testing.T, set *commsPreviewSet, _ *plan.CommsReport, _ *commsScanGatheredPayload) {
		set.Previews[0].FilingBodyDigest = commsFilingBodyDigest("a body the captain never reviewed")
	}})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.dispose(t, r, cmaD2.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)

	if s := f.skips(t, r)[cmaD1.id()]; s.SkipReason != commsSkipFilingFailed || s.Code != commsFilingBodyDrift {
		t.Fatalf("D1 skip = %+v, want filing_failed filing_body_drift", s)
	}
	if n := f.fileCount(); n != 1 {
		t.Errorf("File calls = %d, want 1 (D2 only)", n)
	}
}

// TestApplyApprovedComms_FilingBodyDigestAbsentSkips: no recorded digest →
// filing_failed filing_body_digest_absent.
func TestApplyApprovedComms_FilingBodyDigestAbsentSkips(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{previews: func(_ *testing.T, set *commsPreviewSet, _ *plan.CommsReport, _ *commsScanGatheredPayload) {
		set.Previews = set.Previews[1:]
	}})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)
	if s := f.skips(t, r)[cmaD1.id()]; s.SkipReason != commsSkipFilingFailed || s.Code != commsFilingDigestAbsent {
		t.Fatalf("D1 skip = %+v, want filing_failed filing_body_digest_absent", s)
	}
}

// TestApplyApprovedComms_RendersPerRecordedCharterText: the ingest recorded
// `changed` (ids alone) while the charter now matches the gather (`rendered`
// now). The apply renders per the RECORDED state, so the ids-only body
// matches its digest and files. COUNTERFACTUAL: render per the current state
// → the rubric text renders → filing_body_drift → red.
func TestApplyApprovedComms_RendersPerRecordedCharterText(t *testing.T) {
	f := newCMAFixture(t)
	_, hash := f.withCharter(t)
	r := f.addRun(t, cmaOpts{charterHash: hash, previews: func(t *testing.T, set *commsPreviewSet, report *plan.CommsReport, g *commsScanGatheredPayload) {
		if set.CharterText != commsCharterTextRendered {
			t.Fatalf("fixture precondition: ingest charter_text = %q, want rendered", set.CharterText)
		}
		set.CharterText = commsCharterTextChanged
		for i, d := range report.Drafts {
			set.Previews[i].FilingBodyDigest = commsFilingBodyDigest(commsFilingRequest(commsFilingInput{Draft: d, Gathered: g}).Body)
		}
	}})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)
	if _, ok := f.filed(t, r)[cmaD1.id()]; !ok {
		t.Fatalf("D1 not filed; skips = %+v", f.skips(t, r))
	}
}

// TestApplyApprovedComms_CharterUnavailableCodeDistinct (approval condition
// 6): the ingest rendered the rubric text and the charter read fails
// TRANSIENTLY at apply → filing_failed code charter_unavailable, distinct from
// filing_body_drift; a charter that CHANGED since ingest is filing_body_drift.
func TestApplyApprovedComms_CharterUnavailableCodeDistinct(t *testing.T) {
	cases := map[string]struct {
		mutate func(*igFetcher)
		want   string
	}{
		"transient_read_failure": {want: commsFilingCharterUnavailable, mutate: func(fe *igFetcher) {
			fe.mu.Lock()
			fe.err = errors.New("forge read: connection reset")
			fe.mu.Unlock()
		}},
		"charter_changed": {want: commsFilingBodyDrift, mutate: func(fe *igFetcher) {
			fe.mu.Lock()
			fe.content = strings.Replace(igCharterDoc, "already tracked", "tracked anywhere", 1)
			fe.mu.Unlock()
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCMAFixture(t)
			fetcher, hash := f.withCharter(t)
			r := f.addRun(t, cmaOpts{charterHash: hash, previews: func(t *testing.T, set *commsPreviewSet, _ *plan.CommsReport, _ *commsScanGatheredPayload) {
				if set.CharterText != commsCharterTextRendered {
					t.Fatalf("fixture precondition: ingest charter_text = %q, want rendered", set.CharterText)
				}
			}})
			tc.mutate(fetcher)
			f.seedApproval(t, r, approval.DecisionApprove)
			f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
			f.apply(r, approval.DecisionApprove)
			if s := f.skips(t, r)[cmaD1.id()]; s.SkipReason != commsSkipFilingFailed || s.Code != tc.want {
				t.Fatalf("D1 skip = %+v, want filing_failed %s", s, tc.want)
			}
			if n := f.fileCount(); n != 0 {
				t.Errorf("File calls = %d, want 0", n)
			}
		})
	}
}

// TestApplyApprovedComms_DefaultedAutonomySuppressed (approval condition 4):
// the conventions default an autonomy tier; the recorded preview shows it as
// defaulted, the filing carries NO autonomy:* label, the filed row's
// suppressed_default_labels equals the preview's defaulted autonomy label,
// and the shared conventions are not mutated. COUNTERFACTUAL: file with the
// loaded conventions instead of the clone → autonomy:medium filed → red.
func TestApplyApprovedComms_DefaultedAutonomySuppressed(t *testing.T) {
	f := newCMAFixture(t)
	var previewDefaulted []string
	r := f.addRun(t, cmaOpts{previews: func(_ *testing.T, set *commsPreviewSet, _ *plan.CommsReport, _ *commsScanGatheredPayload) {
		if pv := set.Previews[0].CommsRenderedPreview; pv != nil {
			for _, l := range pv.DefaultedLabels {
				if commsIsAutonomyLabel(l) {
					previewDefaulted = append(previewDefaulted, l)
				}
			}
		}
	}})
	if len(previewDefaulted) == 0 {
		t.Fatal("fixture precondition: the recorded preview defaulted no autonomy label")
	}
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.apply(r, approval.DecisionApprove)

	reqs := f.provider.requests()
	if len(reqs) != 1 {
		t.Fatalf("File calls = %d, want 1", len(reqs))
	}
	for _, l := range reqs[0].Item.Classification.Labels {
		if commsIsAutonomyLabel(l) {
			t.Errorf("filed label %q: the defaulted autonomy tier reached the tracker", l)
		}
	}
	fd := f.filed(t, r)[cmaD1.id()]
	if !reflect.DeepEqual(fd.SuppressedDefaultLabels, previewDefaulted) {
		t.Errorf("suppressed_default_labels = %v, want the preview's defaulted %v", fd.SuppressedDefaultLabels, previewDefaulted)
	}
	if f.conv.Types["bug"].LabelDefaults["autonomy"] == "" {
		t.Error("the loaded conventions lost their autonomy default: the apply mutated shared state")
	}
}

// TestCommsFilingConventions: the clone strips both autonomy forms from the
// filed type only and leaves the input untouched.
func TestCommsFilingConventions(t *testing.T) {
	conv := workmgmt.Conventions{Types: map[string]workmgmt.ItemType{
		"bug":  {LabelDefaults: map[string]string{"autonomy": "autonomy:medium", "area": "area:x"}, DefaultLabels: []string{"autonomy:low", "type:bug"}},
		"epic": {LabelDefaults: map[string]string{"autonomy": "autonomy:high"}},
	}}
	got, suppressed := commsFilingConventions(conv, "bug")
	if !reflect.DeepEqual(suppressed, []string{"autonomy:low", "autonomy:medium"}) {
		t.Errorf("suppressed = %v", suppressed)
	}
	if _, ok := got.Types["bug"].LabelDefaults["autonomy"]; ok || !reflect.DeepEqual(got.Types["bug"].DefaultLabels, []string{"type:bug"}) {
		t.Errorf("clone bug type = %+v", got.Types["bug"])
	}
	if got.Types["epic"].LabelDefaults["autonomy"] != "autonomy:high" {
		t.Error("an unfiled type was changed")
	}
	if conv.Types["bug"].LabelDefaults["autonomy"] != "autonomy:medium" || len(conv.Types["bug"].DefaultLabels) != 2 {
		t.Error("the input conventions were mutated")
	}
	if _, s := commsFilingConventions(conv, "absent"); len(s) != 0 {
		t.Errorf("unknown type suppressed %v", s)
	}
}

// TestApplyApprovedComms_ProposedAutonomyStrippedAgain: a stored draft label
// autonomy:high (refused by rule (h), so seeded by construction straight into
// the filing step) never reaches File.
func TestApplyApprovedComms_ProposedAutonomyStrippedAgain(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	d := r.report.Drafts[0]
	d.ProposedIssue.Labels = append([]string{"autonomy:high"}, d.ProposedIssue.Labels...)
	env, _, degrade, _ := f.s.commsPreviewSetup(context.Background(), r.run, r.gp)
	if degrade != "" {
		t.Fatalf("preview setup degraded: %s", degrade)
	}
	body := commsFilingRequest(commsFilingInput{Draft: d, Gathered: r.gp}).Body
	job := &commsFilingJob{runID: r.run.ID, stageID: r.stage.ID, artifactID: r.art.ID.String(), repo: cmaRepo,
		gather: r.gp, digests: map[string]string{d.ID: commsFilingBodyDigest(body)}, env: env}
	filed, code, msg := f.s.fileCommsDraft(context.Background(), job, d, 0, false, commsShownHashes(r.gp), commsApplyGuard{})
	if filed == nil {
		t.Fatalf("filing failed: %s %s", code, msg)
	}
	for _, req := range f.provider.requests() {
		for _, l := range req.Item.Classification.Labels {
			if commsIsAutonomyLabel(l) {
				t.Errorf("filed label %q", l)
			}
		}
	}
	if !reflect.DeepEqual(filed.StrippedLabels, []string{"autonomy:high"}) {
		t.Errorf("stripped_labels = %v, want [autonomy:high]", filed.StrippedLabels)
	}
	if got := commsStripAutonomy([]string{" Autonomy:low", "area:x"}); !reflect.DeepEqual(got, []string{"area:x"}) {
		t.Errorf("commsStripAutonomy = %v", got)
	}
}

// TestApplyApprovedComms_FilingFailedContinuesAndHolds: a refused filing is
// recorded, the loop continues, and the draft's sources hold the cursor.
func TestApplyApprovedComms_FilingFailedContinuesAndHolds(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.provider.failTitle = cmaD3.title
	f.seedApproval(t, r, approval.DecisionApprove)
	for _, d := range []cmaDraft{cmaD1, cmaD2, cmaD3} {
		f.dispose(t, r, d.id(), commsVerdictApproved, "")
	}
	f.apply(r, approval.DecisionApprove)

	if s := f.skips(t, r)[cmaD3.id()]; s.SkipReason != commsSkipFilingFailed || s.Code == "" {
		t.Fatalf("D3 skip = %+v, want filing_failed with the core's code", s)
	}
	c := f.only(t, r)
	if c.Counts != (commsApplyCounts{Drafts: 3, Filed: 2, Failed: 1}) {
		t.Errorf("counts = %+v", c.Counts)
	}
	// UR-issue-50 (D3's source) is the earliest report: the cursor holds there.
	if got := f.cursors.value(userreport.SourceIssues); !got.Equal(cmaT(1)) {
		t.Errorf("cursor = %v, want held at %v by the unfiled draft", got, cmaT(1))
	}
}

// TestApplyApprovedComms_BudgetExhaustedRecordedAndHolds: drafts after the
// budget expired are recorded apply_budget_exhausted and hold the cursor.
func TestApplyApprovedComms_BudgetExhaustedRecordedAndHolds(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	origB, origP := commsApplyBudget, commsApplyPerDraftBudget
	commsApplyBudget, commsApplyPerDraftBudget = timescale.D(500*time.Millisecond), 0
	t.Cleanup(func() { commsApplyBudget, commsApplyPerDraftBudget = origB, origP })
	f.provider.waitCtx = true // D1's File returns only once the budget expired
	f.seedApproval(t, r, approval.DecisionApprove)
	for _, d := range []cmaDraft{cmaD1, cmaD2, cmaD3} {
		f.dispose(t, r, d.id(), commsVerdictApproved, "")
	}
	f.apply(r, approval.DecisionApprove)

	skips := f.skips(t, r)
	for _, d := range []cmaDraft{cmaD2, cmaD3} {
		if skips[d.id()].SkipReason != commsSkipBudgetExhausted {
			t.Errorf("%s skip = %+v, want apply_budget_exhausted", d.id(), skips[d.id()])
		}
	}
	c := f.only(t, r)
	if c.Counts.BudgetExhausted != 2 || c.Degraded {
		t.Errorf("completion = %+v, want 2 budget_exhausted, not degraded", c)
	}
	if !containsAll(c.Cursor.HeldReportIDs, "UR-issue-30", "UR-issue-31", "UR-issue-50") {
		t.Errorf("held = %v, want the exhausted drafts' sources", c.Cursor.HeldReportIDs)
	}
}

// TestCommsApplyBudgetFor: the floor, or the per-draft scale.
func TestCommsApplyBudgetFor(t *testing.T) {
	if got := commsApplyBudgetFor(1); got != commsApplyBudget {
		t.Errorf("budget(1) = %v, want the floor %v", got, commsApplyBudget)
	}
	n := int(commsApplyBudget/commsApplyPerDraftBudget) + 10
	if got := commsApplyBudgetFor(n); got != time.Duration(n)*commsApplyPerDraftBudget {
		t.Errorf("budget(%d) = %v, want the scaled budget", n, got)
	}
}

// ---------------------------------------------------------------------------
// The cursor
// ---------------------------------------------------------------------------

// approveCursorCase approves the listed drafts, rejects the others named in
// rejected, applies, and returns the run.
func (f *cmaFixture) approveAndApply(t *testing.T, r *cmaRun, approved, rejected []cmaDraft) {
	t.Helper()
	f.seedApproval(t, r, approval.DecisionApprove)
	for _, d := range approved {
		f.dispose(t, r, d.id(), commsVerdictApproved, "")
	}
	for _, d := range rejected {
		f.dispose(t, r, d.id(), commsVerdictRejected, "")
	}
	f.apply(r, approval.DecisionApprove)
}

// TestApplyApprovedComms_UndecidedDraftHoldsCursor (binding obligation 1): the
// undecided D3's source was updated at T1, before the pending cursor T8, so
// the cursor holds at T1. COUNTERFACTUAL: omit undecided sources from the
// held set → the cursor advances to T5 (the unaccounted report) → red.
func TestApplyApprovedComms_UndecidedDraftHoldsCursor(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.approveAndApply(t, r, []cmaDraft{cmaD1, cmaD2}, nil)
	if got := f.cursors.value(userreport.SourceIssues); !got.Equal(cmaT(1)) {
		t.Errorf("cursor = %v, want held at %v by the undecided draft", got, cmaT(1))
	}
	if c := f.only(t, r); !c.Cursor.Advanced || !containsAll(c.Cursor.HeldReportIDs, "UR-issue-50", "UR-issue-99") {
		t.Errorf("cursor outcome = %+v", c.Cursor)
	}
}

// TestApplyApprovedComms_UnfiledApprovedDraftHoldsCursor: an approved draft
// that did not file (digest absent) holds its sources.
func TestApplyApprovedComms_UnfiledApprovedDraftHoldsCursor(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{previews: func(_ *testing.T, set *commsPreviewSet, _ *plan.CommsReport, _ *commsScanGatheredPayload) {
		set.Previews[2].FilingBodyDigest = ""
	}})
	f.approveAndApply(t, r, []cmaDraft{cmaD1, cmaD2, cmaD3}, nil)
	if got := f.cursors.value(userreport.SourceIssues); !got.Equal(cmaT(1)) {
		t.Errorf("cursor = %v, want held at %v by the unfiled approved draft", got, cmaT(1))
	}
}

// TestApplyApprovedComms_UnaccountedReportHoldsCursor: every draft decided,
// the unaccounted UR-issue-99 (T5) holds the cursor. COUNTERFACTUAL: drop the
// recorded unaccounted ids from the held set → T8 → red.
func TestApplyApprovedComms_UnaccountedReportHoldsCursor(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.approveAndApply(t, r, []cmaDraft{cmaD1, cmaD2}, []cmaDraft{cmaD3})
	if got := f.cursors.value(userreport.SourceIssues); !got.Equal(cmaT(5)) {
		t.Errorf("cursor = %v, want held at %v by the unaccounted report", got, cmaT(5))
	}
}

// TestApplyApprovedComms_AllAccountedAdvancesToPending: nothing held → the
// cursor and note floor advance to the gather's pending values.
func TestApplyApprovedComms_AllAccountedAdvancesToPending(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{omit99: true})
	f.approveAndApply(t, r, []cmaDraft{cmaD1, cmaD2}, []cmaDraft{cmaD3})
	if got := f.cursors.value(userreport.SourceIssues); !got.Equal(cmaT(8)) {
		t.Errorf("cursor = %v, want the pending %v", got, cmaT(8))
	}
	if got := f.cursors.value(userreport.SourceIssueNotes); !got.Equal(cmaT(8)) {
		t.Errorf("note floor = %v, want the pending %v", got, cmaT(8))
	}
	c := f.only(t, r)
	if !c.Cursor.Advanced || c.Cursor.Cursor == nil || !c.Cursor.Cursor.Equal(cmaT(8)) || len(c.Cursor.HeldReportIDs) != 0 {
		t.Errorf("cursor outcome = %+v", c.Cursor)
	}
}

// TestApplyApprovedComms_AdvancesNoteFloorThenCursor: the note floor moves
// FIRST. COUNTERFACTUAL: swap the two Advance calls → order wrong → red.
func TestApplyApprovedComms_AdvancesNoteFloorThenCursor(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{omit99: true})
	f.approveAndApply(t, r, []cmaDraft{cmaD1}, nil)
	calls := f.cursors.advances()
	if len(calls) != 2 || calls[0].Source != userreport.SourceIssueNotes || calls[1].Source != userreport.SourceIssues {
		t.Errorf("Advance calls = %+v, want issue_notes then issues", calls)
	}
}

// TestApplyApprovedComms_IncompleteScanNoAdvance: no pending cursor → the
// filings stand and the cursor never moves (scan_incomplete).
func TestApplyApprovedComms_IncompleteScanNoAdvance(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{noPending: true})
	f.approveAndApply(t, r, []cmaDraft{cmaD1}, nil)
	c := f.only(t, r)
	if c.Cursor.Advanced || c.Cursor.Reason != commsCursorReasonScanIncomplete || len(f.cursors.advances()) != 0 {
		t.Errorf("cursor = %+v (calls %v), want scan_incomplete and no Advance", c.Cursor, f.cursors.advances())
	}
	if c.Counts.Filed != 1 {
		t.Errorf("filed = %d, want 1", c.Counts.Filed)
	}
}

// TestApplyApprovedComms_CursorStoreUnwiredStillFiles: no cursor store →
// cursor_store_unwired; the filings stand.
func TestApplyApprovedComms_CursorStoreUnwiredStillFiles(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.s.cfg.UserReportCursors = nil
	f.approveAndApply(t, r, []cmaDraft{cmaD1}, nil)
	c := f.only(t, r)
	if c.Cursor.Reason != commsCursorReasonStoreUnwired || c.Counts.Filed != 1 {
		t.Errorf("completion = %+v, want cursor_store_unwired with D1 filed", c)
	}
}

// TestApplyApprovedComms_CursorAdvanceFailureRecorded: a failed Advance is
// recorded (advance_failed + error) and the filings stand; a note-floor
// failure leaves the cursor unmoved.
func TestApplyApprovedComms_CursorAdvanceFailureRecorded(t *testing.T) {
	for _, src := range []userreport.Source{userreport.SourceIssueNotes, userreport.SourceIssues} {
		t.Run(string(src), func(t *testing.T) {
			f := newCMAFixture(t)
			r := f.addRun(t, cmaOpts{})
			f.cursors.failSource = src
			f.approveAndApply(t, r, []cmaDraft{cmaD1}, nil)
			c := f.only(t, r)
			if c.Cursor.Advanced || c.Cursor.Reason != commsCursorReasonAdvanceFailed || c.Cursor.Error == "" || c.Counts.Filed != 1 {
				t.Errorf("completion = %+v, want advance_failed with an error and D1 filed", c)
			}
			if src == userreport.SourceIssueNotes && len(f.cursors.advances()) != 1 {
				t.Errorf("Advance calls = %v, want the cursor NOT advanced after a note-floor failure", f.cursors.advances())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Suppressions, guard, binding
// ---------------------------------------------------------------------------

// TestApplyApprovedComms_SuppressionsReadableByGatherMemory: the writer and
// the merged #4014 reader agree — after an approve, loadCommsSuppressionMemory
// returns every written suppression (filed, rejected, n_drift).
// COUNTERFACTUAL: rename the JSON keys to report_id / content_hash → the
// memory is empty → red.
func TestApplyApprovedComms_SuppressionsReadableByGatherMemory(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.approveAndApply(t, r, []cmaDraft{cmaD1}, []cmaDraft{cmaD2})

	mem, err := f.s.loadCommsSuppressionMemory(context.Background(), nil, cmaRepo)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"UR-issue-12": commsSuppressionBasisFiled, "UR-issue-30": commsSuppressionBasisRejected,
		"UR-issue-31": commsSuppressionBasisRejected, "UR-issue-41": commsSuppressionBasisNDrift,
	}
	for id, basis := range want {
		sup, ok := mem.lookup(id, cmaHash(id, "v1"))
		if !ok || sup.Basis != basis {
			t.Errorf("memory[%s] = %+v (found %v), want basis %s", id, sup, ok, basis)
		}
	}
	if len(mem) != len(want) {
		t.Errorf("memory holds %d suppressions, want %d (undecided D3 and unaccounted reports are NOT suppressed)", len(mem), len(want))
	}
	if sups := f.only(t, r).Suppressions; !containsSup(sups, "UR-issue-41", commsSuppressionBasisNDrift, cmaNDriftID) {
		t.Errorf("suppressions = %+v, want the n_drift entry", sups)
	}
}

func containsSup(sups []commsApplySuppression, id, basis, entry string) bool {
	for _, s := range sups {
		if s.ID == id && s.Basis == basis && s.EntryID == entry {
			return true
		}
	}
	return false
}

// TestApplyApprovedComms_GuardTruncationRecorded: a guard read that hit the
// row cap proceeds on the newest rows and records guard_truncated.
func TestApplyApprovedComms_GuardTruncationRecorded(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	orig := commsMemoryMaxRows
	commsMemoryMaxRows = 1
	t.Cleanup(func() { commsMemoryMaxRows = orig })
	for i := 0; i < 2; i++ {
		f.otherRunRow(t, CategoryCommsApplyCompleted, commsApplyCompletedPayload{Repo: cmaRepo, Suppressions: []commsApplySuppression{}})
	}
	f.approveAndApply(t, r, []cmaDraft{cmaD1}, nil)
	c := f.only(t, r)
	if !c.GuardTruncated || c.Degraded || c.Counts.Filed != 1 {
		t.Errorf("completion = %+v, want guard_truncated with D1 filed", c)
	}
}

// TestApplyApprovedComms_BindsExactGatheredRow (binding obligation 3): a
// SECOND, later gather for the same stage with other hashes and another
// pending cursor is recorded after ingest; the apply binds the row the report
// names. COUNTERFACTUAL: bind latestCommsScanGathered instead → the later
// hashes → red.
func TestApplyApprovedComms_BindsExactGatheredRow(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{omit99: true})
	later := cmaGather(cmaOpts{omit99: true, hashSalt: "v2"})
	later.PendingCursor.Cursor, later.PendingCursor.NoteCursor = cmaT(9), cmaT(9)
	f.recordGather(t, r.run.ID, r.stage.ID, later, false)
	f.approveAndApply(t, r, []cmaDraft{cmaD1, cmaD2}, []cmaDraft{cmaD3})

	fd := f.filed(t, r)[cmaD1.id()]
	if len(fd.Reports) != 1 || fd.Reports[0].ContentHash != cmaHash("UR-issue-12", "v1") {
		t.Errorf("filed reports = %+v, want the ORIGINAL gathered hash", fd.Reports)
	}
	if got := f.cursors.value(userreport.SourceIssues); !got.Equal(cmaT(8)) {
		t.Errorf("cursor = %v, want the bound gather's pending %v", got, cmaT(8))
	}
}

// TestApplyApprovedComms_LegacyGatherRowLoadsByStoredDigest: a pre-#4016
// gather row (no suggested_clusters key) is bound by its STORED digest and
// files; a recompute-and-compare would refuse it.
func TestApplyApprovedComms_LegacyGatherRowLoadsByStoredDigest(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{legacyGather: true})
	if commsGatheredClustersRecorded(r.gather.Payload) {
		t.Fatal("fixture precondition: the legacy gather carries suggested_clusters")
	}
	f.approveAndApply(t, r, []cmaDraft{cmaD1}, nil)
	if _, ok := f.filed(t, r)[cmaD1.id()]; !ok {
		t.Errorf("D1 not filed on a legacy gather; completion = %+v", f.completed(t, r))
	}
}

// ---------------------------------------------------------------------------
// Serialization, the lock wait, detachment
// ---------------------------------------------------------------------------

// setHook installs a test hook for the test's duration.
func setHook(t *testing.T, hook *func(uuid.UUID), fn func(uuid.UUID)) {
	t.Helper()
	orig := *hook
	*hook = fn
	t.Cleanup(func() { *hook = orig })
}

// TestApplyApprovedComms_ConcurrentAppliesSerializePerRepo: runs A and B both
// approve a draft over UR-issue-12. A's File is parked; B reaches the lock;
// A is released → B skips filed_elsewhere and exactly ONE File happened.
// COUNTERFACTUAL: delete the lock acquisition → B reads the guard while A is
// inside File (A's draft-filed row not yet written) and files too → red.
func TestApplyApprovedComms_ConcurrentAppliesSerializePerRepo(t *testing.T) {
	f := newCMAFixture(t)
	a := f.addRun(t, cmaOpts{drafts: []cmaDraft{cmaD1}})
	b := f.addRun(t, cmaOpts{drafts: []cmaDraft{cmaD1}})
	for _, r := range []*cmaRun{a, b} {
		f.seedApproval(t, r, approval.DecisionApprove)
		f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	}
	f.provider.entered = make(chan struct{}, 4)
	f.provider.release = make(chan struct{})
	bQueued := make(chan struct{}, 1)
	setHook(t, &commsApplyBeforeLock, func(id uuid.UUID) {
		if id == b.run.ID {
			bQueued <- struct{}{}
		}
	})

	f.s.applyApprovedComms(context.Background(), a.stage, approval.DecisionApprove)
	select {
	case <-f.provider.entered:
	case <-time.After(timescale.D(5 * time.Second)):
		close(f.provider.release)
		t.Fatal("run A never reached File")
	}
	f.s.applyApprovedComms(context.Background(), b.stage, approval.DecisionApprove)
	select {
	case <-bQueued:
	case <-time.After(timescale.D(5 * time.Second)):
		close(f.provider.release)
		t.Fatal("run B never reached the lock")
	}
	// A window in which an UNSERIALIZED B would read the guard and enter File.
	select {
	case <-f.provider.entered:
		close(f.provider.release)
		f.s.waitReportApply()
		t.Fatal("run B entered File while run A held the repository lock")
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(f.provider.release)
	f.s.waitReportApply()

	if n := f.fileCount(); n != 1 {
		t.Fatalf("File calls = %d, want exactly 1 across the two applies", n)
	}
	if s := f.skips(t, b)[cmaD1.id()]; s.SkipReason != commsSkipFiledElsewhere {
		t.Errorf("run B skip = %+v, want filed_elsewhere", s)
	}
}

// TestApplyApprovedComms_LockWaitBoundedByBudget (approval condition 3): B
// queues behind a parked A with a tiny budget; it writes its
// apply_budget_exhausted rows and ONE degraded completion row WITHOUT waiting
// for A's release, and advances nothing. COUNTERFACTUAL: acquire with a
// context-blind wait → B blocks until A is released → red (timeout).
func TestApplyApprovedComms_LockWaitBoundedByBudget(t *testing.T) {
	f := newCMAFixture(t)
	a := f.addRun(t, cmaOpts{drafts: []cmaDraft{cmaD1}})
	b := f.addRun(t, cmaOpts{})
	for _, r := range []*cmaRun{a, b} {
		f.seedApproval(t, r, approval.DecisionApprove)
		f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	}
	f.provider.entered = make(chan struct{}, 4)
	f.provider.release = make(chan struct{})
	bDone := make(chan struct{}, 1)
	setHook(t, &commsApplyAfterLoop, func(id uuid.UUID) {
		if id == b.run.ID {
			bDone <- struct{}{}
		}
	})
	origB, origP := commsApplyBudget, commsApplyPerDraftBudget
	t.Cleanup(func() { commsApplyBudget, commsApplyPerDraftBudget = origB, origP })

	f.s.applyApprovedComms(context.Background(), a.stage, approval.DecisionApprove)
	select {
	case <-f.provider.entered:
	case <-time.After(timescale.D(5 * time.Second)):
		close(f.provider.release)
		t.Fatal("run A never reached File")
	}
	commsApplyBudget, commsApplyPerDraftBudget = timescale.D(200*time.Millisecond), 0
	f.s.applyApprovedComms(context.Background(), b.stage, approval.DecisionApprove)
	select {
	case <-bDone:
	case <-time.After(timescale.D(5 * time.Second)):
		close(f.provider.release)
		f.s.waitReportApply()
		t.Fatal("run B waited on the lock past its apply budget")
	}

	c := f.requireDegraded(t, b, commsApplyLockWaitExhausted)
	if c.Counts != (commsApplyCounts{Drafts: 3, BudgetExhausted: 3}) {
		t.Errorf("counts = %+v, want every draft budget-exhausted", c.Counts)
	}
	skips := f.skips(t, b)
	for _, d := range []cmaDraft{cmaD1, cmaD2, cmaD3} {
		if skips[d.id()].SkipReason != commsSkipBudgetExhausted {
			t.Errorf("%s skip = %+v, want apply_budget_exhausted", d.id(), skips[d.id()])
		}
	}
	if !containsAll(c.Cursor.HeldReportIDs, "UR-issue-12", "UR-issue-50", "UR-issue-99") {
		t.Errorf("held = %v, want every source and the unaccounted report held", c.Cursor.HeldReportIDs)
	}
	close(f.provider.release)
	f.s.waitReportApply()
}

// TestAcquireCommsApplyLock_ExpiredContextNeverAcquires: an expired budget
// never takes even a free lock.
func TestAcquireCommsApplyLock_ExpiredContextNeverAcquires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rel, err := acquireCommsApplyLock(ctx, commsApplyLockKey{repo: "x/" + uuid.NewString()}); err == nil {
		rel()
		t.Fatal("acquired with an expired context")
	}
}

// TestApplyApprovedComms_DetachedAndShutdownDrains: Shutdown waits for a
// parked detached apply.
func TestApplyApprovedComms_DetachedAndShutdownDrains(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{drafts: []cmaDraft{cmaD1}})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	f.provider.entered = make(chan struct{}, 1)
	f.provider.release = make(chan struct{})
	f.s.cfg.ShutdownTimeout = timescale.D(10 * time.Second)

	f.s.applyApprovedComms(context.Background(), r.stage, approval.DecisionApprove)
	select {
	case <-f.provider.entered:
	case <-time.After(timescale.D(5 * time.Second)):
		close(f.provider.release)
		t.Fatal("the detached loop never reached the provider")
	}
	shut := make(chan error, 1)
	go func() { shut <- f.s.Shutdown(context.Background()) }()
	select {
	case err := <-shut:
		close(f.provider.release)
		t.Fatalf("Shutdown returned (%v) while the detached comms apply was parked", err)
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(f.provider.release)
	select {
	case <-shut:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown did not return after the provider was released")
	}
	if n := len(f.completed(t, r)); n != 1 {
		t.Errorf("completed rows = %d after Shutdown, want 1", n)
	}
}

// TestApplyApprovedComms_DetachedFromRequestCancellation: a cancelled caller
// context strands neither the settlement nor the filing.
func TestApplyApprovedComms_DetachedFromRequestCancellation(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.seedApproval(t, r, approval.DecisionApprove)
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.s.applyApprovedComms(ctx, r.stage, approval.DecisionApprove)
	f.s.waitReportApply()
	if n := f.fileCount(); n != 1 {
		t.Errorf("File calls = %d on a cancelled request, want 1", n)
	}
}

// TestCollapseCommsConsumed_SkipsAndLastWins: junk rows and other artifacts'
// rows never manufacture a verdict; the higher sequence wins per draft.
func TestCollapseCommsConsumed_SkipsAndLastWins(t *testing.T) {
	art := uuid.NewString()
	row := func(seq int64, p any) *audit.Entry {
		raw, _ := json.Marshal(p)
		return &audit.Entry{Sequence: seq, Payload: raw}
	}
	got := collapseCommsConsumed([]*audit.Entry{
		nil,
		{Sequence: 1, Payload: []byte(`not json`)},
		row(2, commsDispositionPayload{ArtifactID: uuid.NewString(), DraftID: "d", Verdict: "approved"}),
		row(5, commsDispositionPayload{ArtifactID: art, DraftID: "d", Verdict: "approved", ParentEpic: "#9"}),
		row(3, commsDispositionPayload{ArtifactID: art, DraftID: "d", Verdict: "rejected"}),
	}, art)
	if len(got) != 1 || got["d"] != (commsConsumedDisposition{Verdict: "approved", ParentEpic: "#9"}) {
		t.Errorf("collapsed = %+v", got)
	}
}

// TestCommsRecordedDigests_Lenient: junk elements and id-less ones are
// skipped; a non-array decodes to no digests.
func TestCommsRecordedDigests_Lenient(t *testing.T) {
	got := commsRecordedDigests(json.RawMessage(`[{"draft_id":"a","filing_body_digest":"x","extra":1}, 7, {"filing_body_digest":"y"}]`))
	if !reflect.DeepEqual(got, map[string]string{"a": "x"}) {
		t.Errorf("digests = %v", got)
	}
	if got := commsRecordedDigests(json.RawMessage(`{}`)); len(got) != 0 {
		t.Errorf("non-array digests = %v", got)
	}
}

// ---------------------------------------------------------------------------
// Through the approval route
// ---------------------------------------------------------------------------

// TestSubmitApproval_RejectOnCommsStageSettlesWindow drives a reject through
// the real POST /v0/stages/{stage_id}/approvals handler into
// finishApprovalAdvance's type-only plan block. COUNTERFACTUAL: delete the
// applyApprovedComms call in approvals.go → no watermark → red.
func TestSubmitApproval_RejectOnCommsStageSettlesWindow(t *testing.T) {
	f := newCMAFixture(t)
	r := f.addRun(t, cmaOpts{})
	f.dispose(t, r, cmaD1.id(), commsVerdictApproved, "")
	w := submitApproval(t, f.s, r.stage.ID, `{"decision":"reject","comment":"not now"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	f.s.waitReportApply()
	if wm := f.watermarks(t, r); !reflect.DeepEqual(wm, []string{"rejected"}) {
		t.Fatalf("watermarks = %v, want one 'rejected' through the approval route", wm)
	}
	if n := f.fileCount(); n != 0 {
		t.Errorf("File calls = %d on a rejected gate, want 0", n)
	}
	if c := f.only(t, r); c.Decision != commsApplyDecisionReject {
		t.Errorf("completion = %+v, want the reject row", c)
	}
}
