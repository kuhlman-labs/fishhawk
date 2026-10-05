package server

// Tests for the on-approval upkeep apply hook (#3924).
//
// THE HARNESS IS LOCAL (binding condition C6): every fake below is new to this
// file or COMPOSES an existing one (groomingApplyAuditFake,
// groomingApplyApprovalRepo, approvalRunRepo) without modifying it. Every test
// synchronizes through s.waitUpkeepApply() (a WaitGroup barrier) or through
// the parked provider's unbuffered started/release channels — never a
// sleep-poll. Tests that swap package vars (budgets, conventionsLoader) do not
// call t.Parallel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// ukApplyArtifactRepo serves artifacts by id and by stage.
type ukApplyArtifactRepo struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]*artifact.Artifact
	byStage map[uuid.UUID][]*artifact.Artifact
	listErr error
}

func (r *ukApplyArtifactRepo) Create(context.Context, artifact.CreateParams) (*artifact.Artifact, error) {
	return nil, errors.New("ukApplyArtifactRepo: Create not implemented")
}

func (r *ukApplyArtifactRepo) GetByHash(context.Context, uuid.UUID, string) (*artifact.Artifact, error) {
	return nil, artifact.ErrNotFound
}

func (r *ukApplyArtifactRepo) Get(_ context.Context, id uuid.UUID) (*artifact.Artifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.byID[id]; ok {
		return a, nil
	}
	return nil, artifact.ErrNotFound
}

func (r *ukApplyArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.byStage[stageID], nil
}

func (r *ukApplyArtifactRepo) add(a *artifact.Artifact) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[a.ID] = a
	r.byStage[a.StageID] = append(r.byStage[a.StageID], a)
}

// ukApplyAudit composes groomingApplyAuditFake (sequence-assigning,
// ctx-honouring, per-category append failure / wedge) and adds a PER-CATEGORY
// list failure. It deliberately does NOT implement audit.UpkeepWindowAppender,
// so settlement takes the in-memory fallback.
type ukApplyAudit struct {
	*groomingApplyAuditFake
	listErrCategories map[string]error
}

func (a *ukApplyAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if err := a.listErrCategories[category]; err != nil {
		return nil, err
	}
	return a.groomingApplyAuditFake.ListForRunByCategory(ctx, runID, category)
}

// ukApplyRunRepo counts CreateRun: the apply must NEVER create a run.
type ukApplyRunRepo struct {
	*approvalRunRepo
	creates atomic.Int32
}

func (r *ukApplyRunRepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	r.creates.Add(1)
	return r.approvalRunRepo.CreateRun(ctx, p)
}

// ukApplyProvider records every File request. failSummary fails the filing
// whose rendered title equals it; started/release park File (started is
// closed on the first entry, File then waits for release).
type ukApplyProvider struct {
	name        string
	mu          sync.Mutex
	reqs        []workmgmt.ProviderRequest
	failSummary string
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
}

func (p *ukApplyProvider) Name() string { return p.name }

func (p *ukApplyProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	if p.started != nil {
		p.startOnce.Do(func() { close(p.started) })
	}
	if p.release != nil {
		<-p.release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if p.failSummary != "" && req.Item.Title == p.failSummary {
		return nil, errors.New("ukApplyProvider: injected filing failure")
	}
	n := 7000 + len(p.reqs)
	return &workmgmt.CreatedItem{
		Provider: p.name, Number: n,
		URL:           fmt.Sprintf("https://example.test/issues/%d", n),
		AppliedLabels: req.Item.Classification.Labels,
	}, nil
}

func (p *ukApplyProvider) requests() []workmgmt.ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]workmgmt.ProviderRequest(nil), p.reqs...)
}

func (p *ukApplyProvider) byTitle() map[string]workmgmt.ProviderRequest {
	out := map[string]workmgmt.ProviderRequest{}
	for _, r := range p.requests() {
		out[r.Item.Title] = r
	}
	return out
}

// ukApplyConventions is workmgmt.Default() with a unique fake provider and a
// `{summary}` title format (the default's "[E{epic}.{n}]" needs a forge read
// this harness does not wire). When keepAutonomyDefault is false the types'
// autonomy label default is removed too, so a default cannot mask the strip.
func ukApplyConventions(provider string, keepAutonomyDefault bool) workmgmt.Conventions {
	conv := workmgmt.Default()
	conv.Provider = provider
	types := make(map[string]workmgmt.ItemType, len(conv.Types))
	for k, v := range conv.Types {
		if v.Numbering == nil {
			v.TitleFormat = "{summary}"
		}
		defaults := map[string]string{}
		for ns, lbl := range v.LabelDefaults {
			if ns == "autonomy" && !keepAutonomyDefault {
				continue
			}
			defaults[ns] = lbl
		}
		v.LabelDefaults = defaults
		types[k] = v
	}
	conv.Types = types
	return conv
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const ukApplyRepo = "kuhlman-labs/fishhawk"

// ukApplyFinding describes one deprecation-sourced finding.
type ukApplyFinding struct {
	subject    string
	title      string
	typ        string
	labels     []string
	parentEpic string
}

func (f ukApplyFinding) id() string { return plan.UpkeepFindingID("deprecation", f.subject) }

var ukApplyDefaultFindings = []ukApplyFinding{
	{subject: "pkg/a", title: "Replace deprecated pkg/a", typ: "chore", labels: []string{"area:build"}},
	{subject: "pkg/b", title: "Replace deprecated pkg/b", typ: "chore", labels: []string{"area:build"}, parentEpic: "#3726"},
	{subject: "pkg/c", title: "Replace deprecated pkg/c", typ: "bug", labels: []string{"area:backend", "type:bug"}},
}

type ukApplyOpts struct {
	findings        []ukApplyFinding
	badBody         bool
	omitRecordedRow bool
	omitRun         bool
	repo            string
	conventionsErr  error
	autonomyDefault bool
	duplicates      []upkeep.Duplicate
	// rawDuplicates, when non-nil, replaces the recorded row's duplicates value.
	rawDuplicates json.RawMessage
	// reportBody, when non-nil, replaces the fixture's report body (#3750:
	// the advisory example).
	reportBody []byte
	// covered, when non-nil, is the recorded row's `covered` value; nil
	// leaves the key ABSENT (a pre-#3750 row).
	covered json.RawMessage
}

type ukApplyFixture struct {
	s         *Server
	runs      *ukApplyRunRepo
	approvals *groomingApplyApprovalRepo
	au        *ukApplyAudit
	arts      *ukApplyArtifactRepo
	provider  *ukApplyProvider
	stage     *run.Stage
	run       *run.Run
	art       *artifact.Artifact
	findings  []ukApplyFinding
}

func ukApplyReportBody(t *testing.T, findings []ukApplyFinding) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(upkeepExampleBody(t), &doc); err != nil {
		t.Fatal(err)
	}
	var out []any
	for _, f := range findings {
		issue := map[string]any{
			"title": f.title, "body": "Replace " + f.subject + ".", "type": f.typ, "labels": f.labels,
		}
		if f.labels == nil {
			issue["labels"] = []string{}
		}
		if f.parentEpic != "" {
			issue["parent_epic"] = f.parentEpic
		}
		out = append(out, map[string]any{
			"id": f.id(), "source": "deprecation", "subject": f.subject,
			"evidence": []any{map[string]any{
				"kind": "file", "path": "backend/" + f.subject + ".go", "line": 1, "value": f.subject,
			}},
			"proposed_issue": issue,
		})
	}
	doc["findings"] = out
	doc["sources_scanned"] = []string{"deprecation"}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := plan.ParseUpkeepReport(b); perr != nil {
		t.Fatalf("fixture upkeep report is not valid: %v", perr)
	}
	return b
}

func newUkApplyFixture(t *testing.T, opts ukApplyOpts) *ukApplyFixture {
	t.Helper()
	findings := opts.findings
	if findings == nil {
		findings = ukApplyDefaultFindings
	}
	rr := &ukApplyRunRepo{approvalRunRepo: newApprovalRunRepo()}
	ar := &groomingApplyApprovalRepo{fakeApprovalRepo: newFakeApprovalRepo()}
	au := &ukApplyAudit{groomingApplyAuditFake: &groomingApplyAuditFake{approvalAuditFake: newApprovalAuditFake()}}
	arts := &ukApplyArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}, byStage: map[uuid.UUID][]*artifact.Artifact{}}

	stage := rr.seedStage(run.StageStateAwaitingApproval)
	repo := opts.repo
	if repo == "" {
		repo = ukApplyRepo
	}
	install := int64(4242)
	rn := &run.Run{ID: stage.RunID, Repo: repo, InstallationID: &install, CreatedAt: time.Now().UTC()}
	if !opts.omitRun {
		rr.seedRun(rn)
	}

	body := ukApplyReportBody(t, findings)
	if opts.reportBody != nil {
		body = opts.reportBody
	}
	if opts.badBody {
		body = []byte(`{"kind":"upkeep_report","not":"parseable"`)
	}
	art := &artifact.Artifact{
		ID: uuid.New(), StageID: stage.ID, Kind: artifact.KindUpkeepReport,
		Content: body, ContentHash: sha256Hex(body), CreatedAt: time.Now().UTC(),
	}
	arts.add(art)

	s := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr, AuditRepo: au, ArtifactRepo: arts})
	f := &ukApplyFixture{
		s: s, runs: rr, approvals: ar, au: au, arts: arts, stage: stage, run: rn, art: art, findings: findings,
		provider: &ukApplyProvider{name: "upkeep-apply-fake-" + uuid.NewString()},
	}
	workmgmt.Register(f.provider)
	installConventions(t, ukApplyConventions(f.provider.name, opts.autonomyDefault), opts.conventionsErr)

	if !opts.omitRecordedRow {
		dups := opts.rawDuplicates
		if dups == nil {
			d := opts.duplicates
			if d == nil {
				d = []upkeep.Duplicate{}
			}
			dups, _ = json.Marshal(d)
		}
		row := map[string]any{"run_id": stage.RunID.String(), "artifact_id": art.ID.String(), "duplicates": dups}
		if opts.covered != nil {
			row["covered"] = opts.covered
		}
		f.recordRow(t, row)
	}
	return f
}

func (f *ukApplyFixture) appendRow(t *testing.T, category string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	stageID := f.stage.ID
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.stage.RunID, StageID: &stageID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	}); err != nil {
		t.Fatalf("seed %s row: %v", category, err)
	}
}

func (f *ukApplyFixture) recordRow(t *testing.T, payload any) {
	t.Helper()
	f.appendRow(t, CategoryUpkeepReportRecorded, payload)
}

func (f *ukApplyFixture) seedApproval(t *testing.T, subject string, d approval.Decision) {
	t.Helper()
	if _, err := f.approvals.Submit(context.Background(), approval.SubmitParams{
		StageID: f.stage.ID, ApproverSubject: subject, Decision: d, Surface: "test",
	}); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
}

func (f *ukApplyFixture) grant(t *testing.T) {
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)
}

// dispose seeds one upkeep_disposition_recorded row BY CONSTRUCTION.
func (f *ukApplyFixture) dispose(t *testing.T, findingID, verdict string, tier bool, epic string) {
	t.Helper()
	f.appendRow(t, CategoryUpkeepDispositionRecorded, upkeepDispositionPayload{
		RunID: f.stage.RunID.String(), StageID: f.stage.ID.String(), ArtifactID: f.art.ID.String(),
		FindingID: findingID, Source: "deprecation", Verdict: verdict,
		AuthorizeDelegationTier: tier, ParentEpic: epic,
	})
}

// approveAll grants the gate and approves every finding.
func (f *ukApplyFixture) approveAll(t *testing.T) {
	t.Helper()
	f.grant(t)
	for _, fd := range f.findings {
		f.dispose(t, fd.id(), upkeepVerdictApproved, false, "")
	}
}

func (f *ukApplyFixture) apply(t *testing.T, d approval.Decision) {
	t.Helper()
	f.s.applyApprovedUpkeep(context.Background(), f.stage, d)
	f.s.waitUpkeepApply()
}

func (f *ukApplyFixture) rows(t *testing.T, category string) []*audit.Entry {
	t.Helper()
	rows, err := f.au.groomingApplyAuditFake.ListForRunByCategory(context.Background(), f.stage.RunID, category)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (f *ukApplyFixture) watermarks(t *testing.T) []groomingWindowPayload {
	t.Helper()
	var out []groomingWindowPayload
	for _, e := range f.rows(t, audit.UpkeepApplyWindowClosedCategory) {
		var p groomingWindowPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (f *ukApplyFixture) completed(t *testing.T) []upkeepApplyCompletedPayload {
	t.Helper()
	var out []upkeepApplyCompletedPayload
	for _, e := range f.rows(t, CategoryUpkeepApplyCompleted) {
		var p upkeepApplyCompletedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (f *ukApplyFixture) skips(t *testing.T) map[string]upkeepFindingSkippedPayload {
	t.Helper()
	out := map[string]upkeepFindingSkippedPayload{}
	for _, e := range f.rows(t, CategoryUpkeepFindingSkipped) {
		var p upkeepFindingSkippedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.FindingID] = p
	}
	return out
}

func (f *ukApplyFixture) filed(t *testing.T) map[string]upkeepFindingFiledPayload {
	t.Helper()
	out := map[string]upkeepFindingFiledPayload{}
	for _, e := range f.rows(t, CategoryUpkeepFindingFiled) {
		var p upkeepFindingFiledPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.FindingID] = p
	}
	return out
}

// applyRowCount counts every row this hook can write.
func (f *ukApplyFixture) applyRowCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, c := range []string{audit.UpkeepApplyWindowClosedCategory, CategoryUpkeepApplyCompleted,
		CategoryUpkeepFindingFiled, CategoryUpkeepFindingSkipped} {
		n += len(f.rows(t, c))
	}
	return n
}

// degradeReason asserts exactly one degraded completed row and returns its reason.
func (f *ukApplyFixture) degradeReason(t *testing.T) string {
	t.Helper()
	c := f.completed(t)
	if len(c) != 1 || !c[0].Degraded {
		t.Fatalf("upkeep_apply_completed rows = %+v, want exactly one degraded row", c)
	}
	return c[0].DegradeReason
}

func ukSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// E0: the early-out
// ---------------------------------------------------------------------------

// TestApplyApprovedUpkeep_OrdinaryPlanStageNoOps: a stage with no upkeep_report
// artifact (an ordinary plan stage), an unreadable artifact list, or a server
// with no artifact repo writes NOTHING on approve or reject.
func TestApplyApprovedUpkeep_OrdinaryPlanStageNoOps(t *testing.T) {
	cases := map[string]struct {
		opts   ukApplyOpts
		mutate func(*testing.T, *ukApplyFixture)
	}{
		"no_upkeep_artifact": {mutate: func(_ *testing.T, f *ukApplyFixture) {
			f.arts.byStage = map[uuid.UUID][]*artifact.Artifact{}
		}},
		"artifact_list_error": {mutate: func(_ *testing.T, f *ukApplyFixture) {
			f.arts.listErr = errors.New("artifact store down")
		}},
		"nil_artifact_repo": {mutate: func(_ *testing.T, f *ukApplyFixture) { f.s.cfg.ArtifactRepo = nil }},
		"no_recorded_row":   {opts: ukApplyOpts{omitRecordedRow: true}},
		"recorded_report_on_other_stage": {mutate: func(t *testing.T, f *ukApplyFixture) {
			other := &artifact.Artifact{ID: uuid.New(), StageID: uuid.New(), Kind: artifact.KindUpkeepReport}
			f.arts.add(other)
			f.recordRow(t, map[string]any{"artifact_id": other.ID.String(), "duplicates": []any{}})
		}},
	}
	for name, tc := range cases {
		for _, d := range []approval.Decision{approval.DecisionApprove, approval.DecisionReject} {
			t.Run(name+"/"+string(d), func(t *testing.T) {
				f := newUkApplyFixture(t, tc.opts)
				f.approveAll(t)
				if tc.mutate != nil {
					tc.mutate(t, f)
				}
				f.apply(t, d)
				if n := f.applyRowCount(t); n != 0 {
					t.Errorf("upkeep apply rows = %d, want 0 (an ordinary plan decision settles nothing)", n)
				}
				if n := len(f.provider.requests()); n != 0 {
					t.Errorf("File calls = %d, want 0", n)
				}
			})
		}
	}
	// A nil stage or nil audit repo is tolerated without a panic.
	f := newUkApplyFixture(t, ukApplyOpts{})
	f.s.applyApprovedUpkeep(context.Background(), nil, approval.DecisionApprove)
	f.s.cfg.AuditRepo = nil
	f.s.applyApprovedUpkeep(context.Background(), f.stage, approval.DecisionApprove)
	f.s.waitUpkeepApply()
}

// ---------------------------------------------------------------------------
// C1 / C2: the reject path
// ---------------------------------------------------------------------------

// TestApplyApprovedUpkeep_RejectSettlesRejectedFilesNothing: the fixture seeds a
// GRANT row and approved dispositions BY CONSTRUCTION, so with the decision
// branch deleted C3 would pass and the loop would file — the deletion is
// observable. A reject files nothing and settles the window `rejected`.
func TestApplyApprovedUpkeep_RejectSettlesRejectedFilesNothing(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{})
	f.approveAll(t)
	f.apply(t, approval.DecisionReject)

	if n := len(f.provider.requests()); n != 0 {
		t.Fatalf("File calls = %d on a REJECT, want 0", n)
	}
	wm := f.watermarks(t)
	if len(wm) != 1 || wm[0].Settlement != "rejected" || wm[0].ArtifactID != f.art.ID.String() {
		t.Fatalf("watermarks = %+v, want one 'rejected' for artifact %s", wm, f.art.ID)
	}
	if n := len(f.rows(t, CategoryUpkeepFindingFiled)) + len(f.rows(t, CategoryUpkeepApplyCompleted)); n != 0 {
		t.Errorf("filed+completed rows = %d on a reject, want 0", n)
	}
	// The window is now closed: a later capture is refused.
	resp := postUK(t, f.s, f.stage.RunID.String(),
		`{"dispositions":[{"finding_id":"`+f.findings[0].id()+`","verdict":"approved"}]}`, ukOperator)
	if resp.Code != 409 || !strings.Contains(resp.Body.String(), "upkeep_window_closed") {
		t.Errorf("post-reject capture = %d %s, want 409 upkeep_window_closed", resp.Code, resp.Body.String())
	}
}

// TestApplyApprovedUpkeep_RejectClosesWindowEvenWhenBodyUnreadable (binding
// condition C2): the reject settles from the recorded ROW's artifact id before
// any report-body read, so an unparseable body cannot keep the window open.
func TestApplyApprovedUpkeep_RejectClosesWindowEvenWhenBodyUnreadable(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{badBody: true})
	f.grant(t)
	f.apply(t, approval.DecisionReject)
	wm := f.watermarks(t)
	if len(wm) != 1 || wm[0].Settlement != "rejected" {
		t.Fatalf("watermarks = %+v, want one 'rejected' despite the unreadable body", wm)
	}
	if n := len(f.completed(t)); n != 0 {
		t.Errorf("completed rows = %d, want 0 (a reject is not a degrade)", n)
	}
}

// TestApplyApprovedUpkeep_RejectSettlementFailureLogged: a reject whose
// watermark cannot be appended files nothing and writes nothing else.
func TestApplyApprovedUpkeep_RejectSettlementFailureLogged(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{})
	f.approveAll(t)
	f.au.failCategories = map[string]bool{audit.UpkeepApplyWindowClosedCategory: true}
	f.apply(t, approval.DecisionReject)
	if n := f.applyRowCount(t); n != 0 || len(f.provider.requests()) != 0 {
		t.Errorf("rows=%d files=%d, want 0/0", n, len(f.provider.requests()))
	}
}

// ---------------------------------------------------------------------------
// Pre-ratification degrades (window stays OPEN)
// ---------------------------------------------------------------------------

// TestApplyApprovedUpkeep_ReportUnreadableDegrades: on approve, an unparseable
// body or an undecodable recorded row degrades report_unreadable and does NOT
// close the window.
func TestApplyApprovedUpkeep_ReportUnreadableDegrades(t *testing.T) {
	cases := map[string]func(t *testing.T) *ukApplyFixture{
		"unparseable_body": func(t *testing.T) *ukApplyFixture {
			return newUkApplyFixture(t, ukApplyOpts{badBody: true})
		},
		"undecodable_recorded_row": func(t *testing.T) *ukApplyFixture {
			f := newUkApplyFixture(t, ukApplyOpts{})
			f.recordRow(t, []string{"not", "an", "object"})
			return f
		},
		"recorded_row_bad_artifact_id": func(t *testing.T) *ukApplyFixture {
			f := newUkApplyFixture(t, ukApplyOpts{})
			f.recordRow(t, map[string]any{"artifact_id": "not-a-uuid"})
			return f
		},
		"recorded_rows_unlistable": func(t *testing.T) *ukApplyFixture {
			f := newUkApplyFixture(t, ukApplyOpts{})
			f.au.listErrCategories = map[string]error{CategoryUpkeepReportRecorded: errors.New("audit down")}
			return f
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			f := build(t)
			f.grant(t)
			f.apply(t, approval.DecisionApprove)
			if got := f.degradeReason(t); got != upkeepApplyReportUnreadable {
				t.Errorf("degrade_reason = %q, want %q", got, upkeepApplyReportUnreadable)
			}
			if wm := f.watermarks(t); len(wm) != 0 {
				t.Errorf("watermarks = %+v, want none (pre-ratification degrade leaves the window open)", wm)
			}
			if n := len(f.provider.requests()); n != 0 {
				t.Errorf("File calls = %d, want 0", n)
			}
		})
	}
}

// TestApplyApprovedUpkeep_ContestedGateFilesNothingWindowOpen (C3): a contested
// gate, an ungranted gate, or unreadable approval rows file nothing, degrade
// not_ratified, and leave the window open. COUNTERFACTUAL: delete C3 → the
// contested case files → red.
func TestApplyApprovedUpkeep_ContestedGateFilesNothingWindowOpen(t *testing.T) {
	cases := map[string]func(*testing.T, *ukApplyFixture){
		"contested": func(t *testing.T, f *ukApplyFixture) {
			f.grant(t)
			f.seedApproval(t, "someone-else", approval.DecisionReject)
		},
		"ungranted":    func(*testing.T, *ukApplyFixture) {},
		"list_error":   func(t *testing.T, f *ukApplyFixture) { f.grant(t); f.approvals.setListErr(errors.New("down")) },
		"no_approvals": func(t *testing.T, f *ukApplyFixture) { f.grant(t); f.s.cfg.ApprovalRepo = nil },
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUkApplyFixture(t, ukApplyOpts{})
			for _, fd := range f.findings {
				f.dispose(t, fd.id(), upkeepVerdictApproved, false, "")
			}
			seed(t, f)
			f.apply(t, approval.DecisionApprove)
			if n := len(f.provider.requests()); n != 0 {
				t.Fatalf("File calls = %d on an unratified gate, want 0", n)
			}
			if got := f.degradeReason(t); got != upkeepApplyNotRatified {
				t.Errorf("degrade_reason = %q, want %q", got, upkeepApplyNotRatified)
			}
			if wm := f.watermarks(t); len(wm) != 0 {
				t.Errorf("watermarks = %+v, want none", wm)
			}
		})
	}
}

// TestApplyApprovedUpkeep_WindowUnsettledDegrades: the `approved` watermark
// cannot be appended → degraded window_unsettled, nothing filed.
func TestApplyApprovedUpkeep_WindowUnsettledDegrades(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{})
	f.approveAll(t)
	f.au.failCategories = map[string]bool{audit.UpkeepApplyWindowClosedCategory: true}
	f.apply(t, approval.DecisionApprove)
	if got := f.degradeReason(t); got != upkeepApplyWindowUnsettled {
		t.Errorf("degrade_reason = %q, want %q", got, upkeepApplyWindowUnsettled)
	}
	if n := len(f.provider.requests()); n != 0 {
		t.Errorf("File calls = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Post-ratification degrades (window stays CLOSED)
// ---------------------------------------------------------------------------

// TestApplyApprovedUpkeep_PostRatificationDegrades: each resolution rung's
// failure names its own reason, files nothing, and leaves the window settled
// `approved`.
func TestApplyApprovedUpkeep_PostRatificationDegrades(t *testing.T) {
	cases := []struct {
		name   string
		opts   ukApplyOpts
		mutate func(*testing.T, *ukApplyFixture)
		want   string
	}{
		{name: "duplicates_not_an_array", opts: ukApplyOpts{rawDuplicates: json.RawMessage(`"nope"`)}, want: upkeepApplyDuplicatesUnreadable},
		{name: "duplicates_absent", opts: ukApplyOpts{rawDuplicates: json.RawMessage(`null`)}, want: upkeepApplyDuplicatesUnreadable},
		{name: "prior_filings_unlistable", mutate: func(_ *testing.T, f *ukApplyFixture) {
			f.au.listErrCategories = map[string]error{CategoryUpkeepFindingFiled: errors.New("audit down")}
		}, want: upkeepApplyPriorFilingsUnreadable},
		{name: "prior_filing_unattributable", mutate: func(t *testing.T, f *ukApplyFixture) {
			f.appendRow(t, CategoryUpkeepFindingFiled, map[string]any{"artifact_id": f.art.ID.String()})
		}, want: upkeepApplyPriorFilingsUnreadable},
		{name: "run_unreadable", opts: ukApplyOpts{omitRun: true}, want: upkeepApplyRunUnreadable},
		{name: "no_run_repo", mutate: func(_ *testing.T, f *ukApplyFixture) { f.s.cfg.RunRepo = nil }, want: upkeepApplyRunUnreadable},
		{name: "repo_unresolvable", opts: ukApplyOpts{repo: "no-slash"}, want: upkeepApplyRepoUnresolvable},
		{name: "conventions_unavailable", opts: ukApplyOpts{conventionsErr: errors.New("no conventions")}, want: upkeepApplyConventionsUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUkApplyFixture(t, tc.opts)
			f.approveAll(t)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			f.apply(t, approval.DecisionApprove)
			if got := f.degradeReason(t); got != tc.want {
				t.Errorf("degrade_reason = %q, want %q", got, tc.want)
			}
			if n := len(f.provider.requests()); n != 0 {
				t.Errorf("File calls = %d, want 0", n)
			}
			wm := f.watermarks(t)
			if len(wm) != 1 || wm[0].Settlement != "approved" {
				t.Errorf("watermarks = %+v, want one 'approved' (post-ratification degrade leaves it closed)", wm)
			}
		})
	}
}

// TestApplyApprovedUpkeep_PrelaunchTimeoutDegradesNamed: a stalled audit store
// on the settlement append expires the shrunken prelaunch budget → the reason
// is prelaunch_timeout, nothing is filed, and the hook returns promptly.
func TestApplyApprovedUpkeep_PrelaunchTimeoutDegradesNamed(t *testing.T) {
	budget := timescale.D(50 * time.Millisecond)
	prev := upkeepApplyPrelaunchBudget
	upkeepApplyPrelaunchBudget = budget
	t.Cleanup(func() { upkeepApplyPrelaunchBudget = prev })

	f := newUkApplyFixture(t, ukApplyOpts{})
	f.approveAll(t)
	f.au.wedgeCategories = map[string]bool{audit.UpkeepApplyWindowClosedCategory: true}

	start := time.Now()
	f.apply(t, approval.DecisionApprove)
	if el := time.Since(start); el > 20*budget {
		t.Errorf("hook took %v, want < %v (20x the prelaunch budget)", el, 20*budget)
	}
	if got := f.degradeReason(t); got != upkeepApplyPrelaunchTimeout {
		t.Errorf("degrade_reason = %q, want %q", got, upkeepApplyPrelaunchTimeout)
	}
	if n := len(f.provider.requests()); n != 0 {
		t.Errorf("File calls = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// The per-finding loop
// ---------------------------------------------------------------------------

// TestApplyApprovedUpkeep_FilesApprovedFindingsWithMarkerAndKey: approved
// findings file in report order, each body carrying the finding marker and the
// minted idempotency key; the completed row reports the counts; no run is
// created.
func TestApplyApprovedUpkeep_FilesApprovedFindingsWithMarkerAndKey(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{})
	f.approveAll(t)
	f.apply(t, approval.DecisionApprove)

	reqs := f.provider.requests()
	if len(reqs) != len(f.findings) {
		t.Fatalf("File calls = %d, want %d", len(reqs), len(f.findings))
	}
	filed := f.filed(t)
	for i, fd := range f.findings {
		if reqs[i].Item.Title != fd.title {
			t.Errorf("filing %d title = %q, want %q (report order)", i, reqs[i].Item.Title, fd.title)
		}
		key := workmgmt.MintIdempotencyKey(upkeepIdempotencyNamespace, f.stage.RunID.String(), f.art.ID.String(), fd.id())
		if !workmgmt.BodyHasIdempotencyKey(reqs[i].Item.Body, key) {
			t.Errorf("filing %d body lacks the idempotency key %s:\n%s", i, key, reqs[i].Item.Body)
		}
		if !strings.Contains(reqs[i].Item.Body, upkeep.FindingMarker(fd.id())) {
			t.Errorf("filing %d body lacks the finding marker:\n%s", i, reqs[i].Item.Body)
		}
		row, ok := filed[fd.id()]
		if !ok {
			t.Fatalf("no upkeep_finding_filed row for %s", fd.id())
		}
		if row.IdempotencyKey != key || row.IssueNumber == 0 || row.Provider != f.provider.name || row.ArtifactID != f.art.ID.String() {
			t.Errorf("filed row = %+v", row)
		}
	}
	c := f.completed(t)
	if len(c) != 1 || c[0].Degraded || c[0].Filed != 3 || c[0].Skipped != 0 || c[0].Findings != 3 {
		t.Errorf("completed = %+v, want one {filed:3 skipped:0 findings:3}", c)
	}
	if n := f.runs.creates.Load(); n != 0 {
		t.Errorf("CreateRun calls = %d, want 0 (the upkeep apply never creates a run)", n)
	}
	wm := f.watermarks(t)
	if len(wm) != 1 || wm[0].Settlement != "approved" {
		t.Errorf("watermarks = %+v, want one 'approved'", wm)
	}
}

// TestApplyApprovedUpkeep_SkipsNotApproved: an undispositioned finding and a
// rejected one are skipped not_approved. COUNTERFACTUAL: delete rule 1 → the
// undispositioned (non-duplicate, unfiled) finding is filed → red.
func TestApplyApprovedUpkeep_SkipsNotApproved(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{})
	f.grant(t)
	a, b, c := f.findings[0], f.findings[1], f.findings[2]
	f.dispose(t, a.id(), upkeepVerdictApproved, false, "")
	f.dispose(t, b.id(), upkeepVerdictRejected, false, "")
	// c: undispositioned.
	f.apply(t, approval.DecisionApprove)

	got := f.provider.byTitle()
	if _, ok := got[a.title]; !ok || len(got) != 1 {
		t.Fatalf("filed titles = %v, want only %q", ukSortedKeys(got), a.title)
	}
	sk := f.skips(t)
	for _, id := range []string{b.id(), c.id()} {
		if sk[id].SkipReason != upkeepSkipNotApproved {
			t.Errorf("skip[%s] = %+v, want not_approved", id, sk[id])
		}
	}
	if cp := f.completed(t); len(cp) != 1 || cp[0].Filed != 1 || cp[0].Skipped != 2 {
		t.Errorf("completed = %+v, want {filed:1 skipped:2}", cp)
	}
}

// TestApplyApprovedUpkeep_LastWinsDisposition: a later rejected verdict
// supersedes an earlier approval for the same finding.
func TestApplyApprovedUpkeep_LastWinsDisposition(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{findings: ukApplyDefaultFindings[:1]})
	f.grant(t)
	id := f.findings[0].id()
	f.dispose(t, id, upkeepVerdictApproved, false, "")
	f.dispose(t, id, upkeepVerdictRejected, false, "")
	f.apply(t, approval.DecisionApprove)
	if n := len(f.provider.requests()); n != 0 {
		t.Errorf("File calls = %d, want 0 (the later rejected verdict wins)", n)
	}
}

// TestApplyApprovedUpkeep_SkipsDuplicateOfOpenIssue: an APPROVED, never-filed
// finding the ingest marked as a duplicate is skipped, and the row names the
// issue and basis. Rule 1 cannot mask it (approved) and rule 3 cannot (never
// filed). COUNTERFACTUAL: delete rule 2 → it is filed → red.
//
// It also PINS the marker-provenance decision (plan test s): a marker-basis
// mark whose issue carries NO Fishhawk provenance (nothing in this run filed
// it) still suppresses filing, and the skip row makes the planted issue
// visible by number and basis.
func TestApplyApprovedUpkeep_SkipsDuplicateOfOpenIssue(t *testing.T) {
	a, b, c := ukApplyDefaultFindings[0], ukApplyDefaultFindings[1], ukApplyDefaultFindings[2]
	f := newUkApplyFixture(t, ukApplyOpts{duplicates: []upkeep.Duplicate{
		{FindingID: a.id(), IssueNumber: 901, IssueURL: "https://example.test/issues/901", Basis: upkeep.BasisMarker},
		{FindingID: b.id(), IssueNumber: 902, Basis: upkeep.BasisSimilarity, Score: 0.9},
	}})
	f.approveAll(t)
	f.apply(t, approval.DecisionApprove)

	got := f.provider.byTitle()
	if _, ok := got[c.title]; !ok || len(got) != 1 {
		t.Fatalf("filed titles = %v, want only %q", ukSortedKeys(got), c.title)
	}
	sk := f.skips(t)
	if s := sk[a.id()]; s.SkipReason != upkeepSkipDuplicate || s.DuplicateIssueNumber != 901 ||
		s.DuplicateBasis != string(upkeep.BasisMarker) || s.DuplicateIssueURL == "" {
		t.Errorf("marker skip = %+v, want duplicate_of_open_issue #901 basis=marker", s)
	}
	if s := sk[b.id()]; s.SkipReason != upkeepSkipDuplicate || s.DuplicateIssueNumber != 902 || s.DuplicateBasis != string(upkeep.BasisSimilarity) {
		t.Errorf("similarity skip = %+v, want duplicate_of_open_issue #902 basis=similarity", s)
	}
}

// TestApplyApprovedUpkeep_SecondApplyAlreadyFiled: a second apply over the
// same settled window files nothing and records already_filed with the prior
// issue number. COUNTERFACTUAL: delete rule 3 → the permanent watermark hands
// back the same approvals and the non-deduping fake re-files → red.
func TestApplyApprovedUpkeep_SecondApplyAlreadyFiled(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{findings: ukApplyDefaultFindings[:1]})
	f.approveAll(t)
	f.apply(t, approval.DecisionApprove)
	if n := len(f.provider.requests()); n != 1 {
		t.Fatalf("first apply File calls = %d, want 1", n)
	}
	first := f.filed(t)[f.findings[0].id()].IssueNumber

	f.apply(t, approval.DecisionApprove)
	if n := len(f.provider.requests()); n != 1 {
		t.Fatalf("File calls after the second apply = %d, want still 1", n)
	}
	s := f.skips(t)[f.findings[0].id()]
	if s.SkipReason != upkeepSkipAlreadyFiled || s.PriorIssueNumber != first {
		t.Errorf("second-pass skip = %+v, want already_filed prior #%d", s, first)
	}
	if wm := f.watermarks(t); len(wm) != 1 {
		t.Errorf("watermarks = %d, want 1 (permanence)", len(wm))
	}
}

// TestApplyApprovedUpkeep_AutonomyStrippedUnlessAuthorized: with conventions
// carrying NO autonomy default (so a default cannot mask the assertion), a
// proposed autonomy:high is absent from the filed labels and listed in
// stripped_labels unless the captain authorized it. COUNTERFACTUAL: delete the
// strip → autonomy:high present on the unauthorized filing → red.
func TestApplyApprovedUpkeep_AutonomyStrippedUnlessAuthorized(t *testing.T) {
	findings := []ukApplyFinding{
		{subject: "pkg/x", title: "Unauthorized tier", typ: "chore", labels: []string{"area:build", "autonomy:high"}},
		{subject: "pkg/y", title: "Authorized tier", typ: "chore", labels: []string{"area:build", "Autonomy:High"}},
	}
	f := newUkApplyFixture(t, ukApplyOpts{findings: findings})
	f.grant(t)
	f.dispose(t, findings[0].id(), upkeepVerdictApproved, false, "")
	f.dispose(t, findings[1].id(), upkeepVerdictApproved, true, "")
	f.apply(t, approval.DecisionApprove)

	got := f.provider.byTitle()
	if hasLabelFold(got["Unauthorized tier"].Item.Classification.Labels, "autonomy:high") {
		t.Errorf("unauthorized filing labels = %v, want no autonomy:high", got["Unauthorized tier"].Item.Classification.Labels)
	}
	if !hasLabelFold(got["Authorized tier"].Item.Classification.Labels, "autonomy:high") {
		t.Errorf("authorized filing labels = %v, want autonomy:high", got["Authorized tier"].Item.Classification.Labels)
	}
	filed := f.filed(t)
	if s := filed[findings[0].id()].StrippedLabels; len(s) != 1 || s[0] != "autonomy:high" {
		t.Errorf("stripped_labels = %v, want [autonomy:high]", s)
	}
	if s := filed[findings[1].id()].StrippedLabels; len(s) != 0 {
		t.Errorf("authorized stripped_labels = %v, want []", s)
	}
}

// TestApplyApprovedUpkeep_DefaultConventionsAutonomyTier (binding condition
// C4): with workmgmt.Default()'s autonomy:medium label default, an
// UNAUTHORIZED proposed autonomy:high files as the conventions default
// autonomy:medium — never the proposed tier — and an authorized one files as
// autonomy:high. applied_labels is the post-Apply set actually filed.
func TestApplyApprovedUpkeep_DefaultConventionsAutonomyTier(t *testing.T) {
	findings := []ukApplyFinding{
		{subject: "pkg/m", title: "Unauthorized high", typ: "chore", labels: []string{"area:build", "autonomy:high"}},
		{subject: "pkg/n", title: "Authorized high", typ: "chore", labels: []string{"area:build", "autonomy:high"}},
	}
	f := newUkApplyFixture(t, ukApplyOpts{findings: findings, autonomyDefault: true})
	f.grant(t)
	f.dispose(t, findings[0].id(), upkeepVerdictApproved, false, "")
	f.dispose(t, findings[1].id(), upkeepVerdictApproved, true, "")
	f.apply(t, approval.DecisionApprove)

	got := f.provider.byTitle()
	un := got["Unauthorized high"].Item.Classification.Labels
	if hasLabelFold(un, "autonomy:high") || !hasLabelFold(un, "autonomy:medium") {
		t.Errorf("unauthorized labels = %v, want the conventions default autonomy:medium and no autonomy:high", un)
	}
	au := got["Authorized high"].Item.Classification.Labels
	if !hasLabelFold(au, "autonomy:high") || hasLabelFold(au, "autonomy:medium") {
		t.Errorf("authorized labels = %v, want autonomy:high only", au)
	}
	filed := f.filed(t)
	if a := filed[findings[0].id()].AppliedLabels; !hasLabelFold(a, "autonomy:medium") || hasLabelFold(a, "autonomy:high") {
		t.Errorf("applied_labels = %v, want the post-Apply set (autonomy:medium, no autonomy:high)", a)
	}
	if a := filed[findings[1].id()].AppliedLabels; !hasLabelFold(a, "autonomy:high") {
		t.Errorf("authorized applied_labels = %v, want autonomy:high", a)
	}
}

func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// TestApplyApprovedUpkeep_ParentEpicOverrideElseProposal: the disposition's
// parent_epic override wins over the proposal's, the proposal's is used when
// there is no override, and both normalize to `#N`.
func TestApplyApprovedUpkeep_ParentEpicOverrideElseProposal(t *testing.T) {
	findings := []ukApplyFinding{
		{subject: "pkg/o", title: "Override wins", typ: "chore", parentEpic: "#3726"},
		{subject: "pkg/p", title: "Proposal used", typ: "chore", parentEpic: "3726"},
		{subject: "pkg/q", title: "No epic", typ: "chore"},
	}
	f := newUkApplyFixture(t, ukApplyOpts{findings: findings})
	f.grant(t)
	f.dispose(t, findings[0].id(), upkeepVerdictApproved, false, "77")
	f.dispose(t, findings[1].id(), upkeepVerdictApproved, false, "")
	f.dispose(t, findings[2].id(), upkeepVerdictApproved, false, "")
	f.apply(t, approval.DecisionApprove)

	got := f.provider.byTitle()
	for title, want := range map[string]string{"Override wins": "#77", "Proposal used": "#3726", "No epic": ""} {
		if pe := got[title].Item.Relations.ParentEpic; pe != want {
			t.Errorf("%s parent epic = %q, want %q", title, pe, want)
		}
	}
	if pe := f.filed(t)[findings[0].id()].ParentEpic; pe != "#77" {
		t.Errorf("filed row parent_epic = %q, want #77", pe)
	}
}

// TestApplyApprovedUpkeep_FilingFailedContinues: a provider failure on one
// finding records filing_failed with the core's code and the next finding
// still files.
func TestApplyApprovedUpkeep_FilingFailedContinues(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{})
	f.provider.failSummary = f.findings[0].title
	f.approveAll(t)
	f.apply(t, approval.DecisionApprove)

	s := f.skips(t)[f.findings[0].id()]
	if s.SkipReason != upkeepSkipFilingFailed || s.Code != "work_item_filing_failed" || s.Message == "" {
		t.Errorf("failed finding skip = %+v, want filing_failed/work_item_filing_failed", s)
	}
	if n := len(f.filed(t)); n != 2 {
		t.Errorf("filed rows = %d, want 2 (the loop continues past a failure)", n)
	}
	if c := f.completed(t); len(c) != 1 || c[0].Failed != 1 || c[0].Filed != 2 {
		t.Errorf("completed = %+v, want {filed:2 failed:1}", c)
	}
}

// TestApplyApprovedUpkeep_BudgetExhaustedRecorded: a zero detached budget
// records every approved finding apply_budget_exhausted rather than dialing a
// dead context.
func TestApplyApprovedUpkeep_BudgetExhaustedRecorded(t *testing.T) {
	prevB, prevP := upkeepApplyBudget, upkeepApplyPerFindingBudget
	upkeepApplyBudget, upkeepApplyPerFindingBudget = 0, 0
	t.Cleanup(func() { upkeepApplyBudget, upkeepApplyPerFindingBudget = prevB, prevP })

	f := newUkApplyFixture(t, ukApplyOpts{})
	f.approveAll(t)
	f.apply(t, approval.DecisionApprove)
	if n := len(f.provider.requests()); n != 0 {
		t.Fatalf("File calls = %d under a zero budget, want 0", n)
	}
	sk := f.skips(t)
	for _, fd := range f.findings {
		if sk[fd.id()].SkipReason != upkeepSkipBudgetExhausted {
			t.Errorf("skip[%s] = %+v, want apply_budget_exhausted", fd.id(), sk[fd.id()])
		}
	}
	if c := f.completed(t); len(c) != 1 || c[0].BudgetExhausted != 3 {
		t.Errorf("completed = %+v, want budget_exhausted:3", c)
	}
}

// TestUpkeepApplyBudgetFor pins the floor/scale rule.
func TestUpkeepApplyBudgetFor(t *testing.T) {
	if got := upkeepApplyBudgetFor(0); got != upkeepApplyBudget {
		t.Errorf("budget(0) = %v, want the floor %v", got, upkeepApplyBudget)
	}
	if got := upkeepApplyBudgetFor(200); got != 200*upkeepApplyPerFindingBudget {
		t.Errorf("budget(200) = %v, want %v", got, 200*upkeepApplyPerFindingBudget)
	}
}

// TestApplyApprovedUpkeep_DetachedAndShutdownDrains: the hook returns while
// the provider is parked, and Shutdown blocks until it is released.
// COUNTERFACTUAL: drop bgUpkeepApply.Wait() from Shutdown → Shutdown returns
// while parked → red.
func TestApplyApprovedUpkeep_DetachedAndShutdownDrains(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{findings: ukApplyDefaultFindings[:1]})
	f.provider.started = make(chan struct{})
	f.provider.release = make(chan struct{})
	f.approveAll(t)
	f.s.cfg.ShutdownTimeout = timescale.D(10 * time.Second)

	f.s.applyApprovedUpkeep(context.Background(), f.stage, approval.DecisionApprove)
	select {
	case <-f.provider.started:
	case <-time.After(timescale.D(5 * time.Second)):
		close(f.provider.release)
		t.Fatal("the detached loop never reached the provider")
	}

	shut := make(chan error, 1)
	go func() { shut <- f.s.Shutdown(context.Background()) }()
	select {
	case err := <-shut:
		close(f.provider.release)
		t.Fatalf("Shutdown returned (%v) while the detached upkeep apply was parked; it must drain bgUpkeepApply", err)
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(f.provider.release)
	select {
	case <-shut:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown did not return after the provider was released")
	}
	if n := len(f.completed(t)); n != 1 {
		t.Errorf("completed rows = %d after Shutdown, want 1", n)
	}
}

// TestApplyApprovedUpkeep_DetachedFromRequestCancellation: a cancelled caller
// context strands neither the settlement nor the filing.
func TestApplyApprovedUpkeep_DetachedFromRequestCancellation(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{findings: ukApplyDefaultFindings[:1]})
	f.approveAll(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.s.applyApprovedUpkeep(ctx, f.stage, approval.DecisionApprove)
	f.s.waitUpkeepApply()
	if n := len(f.provider.requests()); n != 1 {
		t.Errorf("File calls = %d on a cancelled request, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestUpkeepFilingHelpers(t *testing.T) {
	if got := normalizeUpkeepEpicRef(" 389 "); got != "#389" {
		t.Errorf("normalize(389) = %q", got)
	}
	if got := normalizeUpkeepEpicRef("#389"); got != "#389" {
		t.Errorf("normalize(#389) = %q", got)
	}
	if got := normalizeUpkeepEpicRef(""); got != "" {
		t.Errorf("normalize('') = %q", got)
	}
	m := upkeep.FindingMarker("deprecation:x")
	if got := upkeepFilingBody("", "deprecation:x"); got != m {
		t.Errorf("empty body = %q, want the marker alone", got)
	}
	once := upkeepFilingBody("body\n", "deprecation:x")
	if once != "body\n\n"+m || upkeepFilingBody(once, "deprecation:x") != once {
		t.Errorf("body marker not appended exactly once: %q", once)
	}
	kept, stripped := upkeepFilingLabels([]string{"area:x", "autonomy:low"}, false)
	if len(kept) != 1 || len(stripped) != 1 {
		t.Errorf("strip = %v / %v", kept, stripped)
	}
}

// TestCollapseUpkeepConsumed_SkipsAndLastWins: junk rows and other artifacts'
// rows never manufacture a verdict; the higher sequence wins per finding.
func TestCollapseUpkeepConsumed_SkipsAndLastWins(t *testing.T) {
	art := uuid.NewString()
	row := func(seq int64, p any) *audit.Entry {
		raw, _ := json.Marshal(p)
		return &audit.Entry{Sequence: seq, Payload: raw}
	}
	got := collapseUpkeepConsumed([]*audit.Entry{
		nil,
		{Sequence: 1, Payload: []byte(`not json`)},
		row(2, upkeepDispositionPayload{ArtifactID: uuid.NewString(), FindingID: "f", Verdict: "approved"}),
		row(5, upkeepDispositionPayload{ArtifactID: art, FindingID: "f", Verdict: "rejected"}),
		row(3, upkeepDispositionPayload{ArtifactID: art, FindingID: "f", Verdict: "approved", ParentEpic: "#1"}),
		row(4, upkeepDispositionPayload{ArtifactID: art, FindingID: "", Verdict: "approved"}),
	}, art)
	if len(got) != 1 || got["f"].Verdict != "rejected" {
		t.Errorf("collapsed = %+v, want f→rejected only", got)
	}
}

// TestSubmitApproval_RejectOnUpkeepStageSettlesWindow drives a reject through
// the real POST /v0/stages/{stage_id}/approvals handler into
// finishApprovalAdvance's type-only plan block: the window settles `rejected`
// and nothing is filed. COUNTERFACTUAL: delete the applyApprovedUpkeep call in
// approvals.go → no watermark → red. (The approve-path flow through the same
// route is upkeep_flow_test.go's.)
func TestSubmitApproval_RejectOnUpkeepStageSettlesWindow(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{})
	for _, fd := range f.findings {
		f.dispose(t, fd.id(), upkeepVerdictApproved, false, "")
	}
	w := submitApproval(t, f.s, f.stage.ID, `{"decision":"reject","comment":"not now"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	f.s.waitUpkeepApply()
	if wm := f.watermarks(t); len(wm) != 1 || wm[0].Settlement != "rejected" {
		t.Fatalf("watermarks = %+v, want one 'rejected' through the approval route", wm)
	}
	if n := len(f.provider.requests()); n != 0 {
		t.Errorf("File calls = %d on a rejected gate, want 0", n)
	}
}

// ukApplyAppenderAudit adds the production capability audit.UpkeepWindowAppender
// over ukApplyAudit, so settlement takes the ATOMIC path: it records the
// artifact it was asked to settle and returns every disposition row as
// consumed (or closeErr).
type ukApplyAppenderAudit struct {
	*ukApplyAudit
	closeErr    error
	closedFor   []string
	settlements []string
}

func (a *ukApplyAppenderAudit) AppendChainedUpkeepDispositionBatch(context.Context, string, []audit.ChainAppendParams) ([]*audit.Entry, error) {
	return nil, errors.New("ukApplyAppenderAudit: batch not used")
}

func (a *ukApplyAppenderAudit) AppendChainedUpkeepWindowClose(ctx context.Context, p audit.ChainAppendParams, artifactID string) (*audit.Entry, []*audit.Entry, error) {
	if a.closeErr != nil {
		return nil, nil, a.closeErr
	}
	var wp groomingWindowPayload
	_ = json.Unmarshal(p.Payload, &wp)
	a.closedFor = append(a.closedFor, artifactID)
	a.settlements = append(a.settlements, wp.Settlement)
	wm, err := a.AppendChained(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	consumed, err := a.ListForRunByCategory(ctx, p.RunID, CategoryUpkeepDispositionRecorded)
	return wm, consumed, err
}

var _ audit.UpkeepWindowAppender = (*ukApplyAppenderAudit)(nil)

// TestApplyApprovedUpkeep_AtomicSettlementPath: with the production
// capability present the hook settles through AppendChainedUpkeepWindowClose
// for the bound artifact and files from its consumed set; a capability error
// degrades window_unsettled with nothing filed.
func TestApplyApprovedUpkeep_AtomicSettlementPath(t *testing.T) {
	t.Run("settles_and_files", func(t *testing.T) {
		f := newUkApplyFixture(t, ukApplyOpts{})
		app := &ukApplyAppenderAudit{ukApplyAudit: f.au}
		f.s.cfg.AuditRepo = app
		f.approveAll(t)
		f.apply(t, approval.DecisionApprove)
		if len(app.closedFor) != 1 || app.closedFor[0] != f.art.ID.String() || app.settlements[0] != "approved" {
			t.Fatalf("window closes = %v %v, want one 'approved' for %s", app.closedFor, app.settlements, f.art.ID)
		}
		if n := len(f.provider.requests()); n != len(f.findings) {
			t.Errorf("File calls = %d, want %d", n, len(f.findings))
		}
	})
	t.Run("close_error_degrades", func(t *testing.T) {
		f := newUkApplyFixture(t, ukApplyOpts{})
		f.s.cfg.AuditRepo = &ukApplyAppenderAudit{ukApplyAudit: f.au, closeErr: errors.New("tx aborted")}
		f.approveAll(t)
		f.apply(t, approval.DecisionApprove)
		if got := f.degradeReason(t); got != upkeepApplyWindowUnsettled {
			t.Errorf("degrade_reason = %q, want %q", got, upkeepApplyWindowUnsettled)
		}
		if n := len(f.provider.requests()); n != 0 {
			t.Errorf("File calls = %d, want 0", n)
		}
	})
}

// ---------------------------------------------------------------------------
// Advisory findings (#3750): Dependabot coverage and server-rendered filings
// ---------------------------------------------------------------------------

// ukAdvisoryBody is the shipped advisory example with mutate applied to each
// finding's proposed_issue (keyed by finding id), re-validated.
func ukAdvisoryBody(t *testing.T, mutate map[string]func(issue map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(upkeepAdvisoryExampleBody(t), &doc); err != nil {
		t.Fatal(err)
	}
	for _, raw := range doc["findings"].([]any) {
		fd := raw.(map[string]any)
		if m := mutate[fd["id"].(string)]; m != nil {
			m(fd["proposed_issue"].(map[string]any))
		}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := plan.ParseUpkeepReport(b); perr != nil {
		t.Fatalf("mutated advisory report is not valid: %v", perr)
	}
	return b
}

// ukCoveredRaw marks ukAdvNet covered by Dependabot pull request #3823.
func ukCoveredRaw(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal([]upkeep.Covered{{FindingID: ukAdvNet, Package: "golang.org/x/net",
		Pulls: []upkeep.CoveringPull{{Number: 3823, URL: "https://github.com/kuhlman-labs/fishhawk/pull/3823",
			Directory: "backend", BumpsTo: "0.23.0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestApplyApprovedUpkeep_SkipsCoveredByDependabotPR: finding X (ukAdvNet) is
// APPROVED, not a duplicate and never filed, and the recorded row marks it
// covered. It is skipped covered_by_dependabot_pr carrying the pull request,
// and nothing is filed. X is approved and non-duplicate, so the covered case
// is the only thing between it and filing: delete it and X is filed → red.
func TestApplyApprovedUpkeep_SkipsCoveredByDependabotPR(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{reportBody: upkeepAdvisoryExampleBody(t), covered: ukCoveredRaw(t)})
	f.grant(t)
	f.dispose(t, ukAdvNet, upkeepVerdictApproved, false, "")
	f.apply(t, approval.DecisionApprove)

	if n := len(f.provider.requests()); n != 0 {
		t.Fatalf("File calls = %d, want 0 (the only approved finding is covered)", n)
	}
	s := f.skips(t)[ukAdvNet]
	if s.SkipReason != upkeepSkipCovered || len(s.CoveringPRNumbers) != 1 || s.CoveringPRNumbers[0] != 3823 ||
		len(s.CoveringPRURLs) != 1 || s.CoveringPRURLs[0] != "https://github.com/kuhlman-labs/fishhawk/pull/3823" {
		t.Errorf("skip = %+v, want covered_by_dependabot_pr by #3823", s)
	}
	if s.Source != plan.UpkeepSourceAdvisory {
		t.Errorf("skip source = %q, want advisory", s.Source)
	}
	if c := f.completed(t); len(c) != 1 || c[0].Degraded || c[0].Filed != 0 || c[0].Skipped != 3 || c[0].Findings != 3 {
		t.Errorf("completed = %+v, want {findings:3 filed:0 skipped:3}", c)
	}
}

// TestApplyApprovedUpkeep_LegacyRowWithoutCoveredFiles: a recorded row with NO
// `covered` key (recorded before #3750) reads as no marks, so approved X is
// filed. Treating the absent key as unreadable degrades and files nothing →
// red.
func TestApplyApprovedUpkeep_LegacyRowWithoutCoveredFiles(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{reportBody: upkeepAdvisoryExampleBody(t)})
	f.grant(t)
	f.dispose(t, ukAdvNet, upkeepVerdictApproved, false, "")
	f.apply(t, approval.DecisionApprove)

	if n := len(f.provider.requests()); n != 1 {
		t.Fatalf("File calls = %d, want 1", n)
	}
	if _, ok := f.filed(t)[ukAdvNet]; !ok {
		t.Errorf("no upkeep_finding_filed row for %s", ukAdvNet)
	}
	if c := f.completed(t); len(c) != 1 || c[0].Degraded || c[0].Filed != 1 {
		t.Errorf("completed = %+v, want one non-degraded {filed:1}", c)
	}
}

// TestApplyApprovedUpkeep_CoverageUnreadableDegrades: a `covered` key that is
// present but null, not an array, or an undecodable array fails closed —
// upkeep_apply_coverage_unreadable, nothing filed, the window left settled
// `approved`. X is approved, so a lenient decode (null as empty) lets it file
// → red.
func TestApplyApprovedUpkeep_CoverageUnreadableDegrades(t *testing.T) {
	for name, raw := range map[string]string{
		"null": `null`, "string": `"nope"`, "object": `{"finding_id":"x"}`, "array_of_numbers": `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newUkApplyFixture(t, ukApplyOpts{reportBody: upkeepAdvisoryExampleBody(t), covered: json.RawMessage(raw)})
			f.grant(t)
			f.dispose(t, ukAdvNet, upkeepVerdictApproved, false, "")
			f.apply(t, approval.DecisionApprove)
			if got := f.degradeReason(t); got != upkeepApplyCoverageUnreadable {
				t.Errorf("degrade_reason = %q, want %q", got, upkeepApplyCoverageUnreadable)
			}
			if n := len(f.provider.requests()); n != 0 {
				t.Errorf("File calls = %d, want 0", n)
			}
			if wm := f.watermarks(t); len(wm) != 1 || wm[0].Settlement != "approved" {
				t.Errorf("watermarks = %+v, want one 'approved' (post-ratification)", wm)
			}
		})
	}
}

// TestApplyApprovedUpkeep_AdvisoryFilingIsServerRendered (approval condition
// 1): an approved advisory finding whose AGENT title and body quote the call
// path through the repository's own code — the caller frame's function,
// qualified name and filename — files a SERVER-RENDERED title and body that
// contain none of them, nor any of the agent's prose. Filing the agent title
// or body instead → red.
func TestApplyApprovedUpkeep_AdvisoryFilingIsServerRendered(t *testing.T) {
	const (
		agentTitle = "Fix serveH2 in internal/server/serve.go (GO-2024-2687)"
		agentBody  = "govulncheck traced github.com/kuhlman-labs/fishhawk/backend/internal/server.serveH2 " +
			"at internal/server/serve.go:88 into Framer.ReadFrame."
	)
	body := ukAdvisoryBody(t, map[string]func(map[string]any){ukAdvNet: func(issue map[string]any) {
		issue["title"] = agentTitle
		issue["body"] = agentBody
	}})
	f := newUkApplyFixture(t, ukApplyOpts{reportBody: body})
	f.grant(t)
	f.dispose(t, ukAdvNet, upkeepVerdictApproved, false, "")
	f.apply(t, approval.DecisionApprove)

	reqs := f.provider.requests()
	if len(reqs) != 1 {
		t.Fatalf("File calls = %d, want 1", len(reqs))
	}
	title, filedBody := reqs[0].Item.Title, reqs[0].Item.Body
	if want := "GO-2024-2687: golang.org/x/net v0.22.0 (high severity)"; title != want {
		t.Errorf("filed title = %q, want the server-rendered %q", title, want)
	}
	for _, leak := range []string{agentTitle, agentBody, "serveH2", "internal/server/serve.go",
		"github.com/kuhlman-labs/fishhawk/backend/internal/server.serveH2", "govulncheck traced"} {
		if strings.Contains(title, leak) || strings.Contains(filedBody, leak) {
			t.Errorf("filing quotes %q:\ntitle: %s\nbody:\n%s", leak, title, filedBody)
		}
	}
	for _, want := range []string{"### Advisory facts (server-rendered)",
		"- Advisory IDs: `GO-2024-2687`, `CVE-2023-45288`, `GHSA-4v7x-pqxf-cx7m`",
		"- Fixed version: `v0.23.0`", "- Reachability: `called`", "- Manifests: `backend/go.mod`\n",
		upkeep.FindingMarker(ukAdvNet)} {
		if !strings.Contains(filedBody, want) {
			t.Errorf("filed body lacks %q:\n%s", want, filedBody)
		}
	}
	row := f.filed(t)[ukAdvNet]
	if !row.ServerRendered || row.Title != title || row.Source != plan.UpkeepSourceAdvisory {
		t.Errorf("filed row = %+v, want server_rendered true and the rendered title", row)
	}
}

// TestApplyApprovedUpkeep_AdvisoryFactsBlock: an approved advisory finding
// with no published fix, whose agent body carries NONE of the facts, files a
// body carrying every server-rendered facts line, with `no fix published`.
// Drop the facts from the filing and the agent body carries none of these
// lines → red.
func TestApplyApprovedUpkeep_AdvisoryFactsBlock(t *testing.T) {
	body := ukAdvisoryBody(t, map[string]func(map[string]any){ukAdvGHSA: func(issue map[string]any) {
		issue["body"] = "See the weekly scan."
	}})
	f := newUkApplyFixture(t, ukApplyOpts{reportBody: body, covered: json.RawMessage(`[]`)})
	f.grant(t)
	f.dispose(t, ukAdvGHSA, upkeepVerdictApproved, false, "")
	f.apply(t, approval.DecisionApprove)

	reqs := f.provider.requests()
	if len(reqs) != 1 {
		t.Fatalf("File calls = %d, want 1", len(reqs))
	}
	got := reqs[0].Item.Body
	for _, want := range []string{
		"### Advisory facts (server-rendered)\n",
		"- Advisory IDs: `GHSA-q8v2-3m4c-7x9p`\n",
		"- Ecosystem: `npm`\n",
		"- Package: `yaml-front-parser`\n",
		"- In-use version: `2.1.0`\n",
		"- Fixed version: no fix published\n",
		"- Reachability: `unanalyzed`\n",
		"- Severity: `medium`\n",
		"- Manifests: `site/pnpm-lock.yaml`\n",
		upkeep.FindingMarker(ukAdvGHSA),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("filed body lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "See the weekly scan.") {
		t.Errorf("filed body carries the agent's prose:\n%s", got)
	}
	key := workmgmt.MintIdempotencyKey(upkeepIdempotencyNamespace, f.stage.RunID.String(), f.art.ID.String(), ukAdvGHSA)
	if !workmgmt.BodyHasIdempotencyKey(got, key) {
		t.Errorf("filed body lacks the idempotency key %s", key)
	}
}

// TestUpkeepFilingProse: a non-advisory finding files its proposed title and
// body byte-identically; an advisory finding never does, even one whose
// advisory object is missing (unreachable past rule (l), and still rendered
// from structured fields only).
func TestUpkeepFilingProse(t *testing.T) {
	dep := plan.UpkeepFinding{Source: plan.UpkeepSourceDeprecation,
		ProposedIssue: plan.UpkeepProposedIssue{Title: "Replace pkg/a", Body: "Replace pkg/a.\n"}}
	if title, body, rendered := upkeepFilingProse(dep); title != "Replace pkg/a" || body != "Replace pkg/a.\n" || rendered {
		t.Errorf("deprecation prose = (%q, %q, %v), want the proposal unchanged", title, body, rendered)
	}
	if got := upkeepFilingBody("Replace pkg/a.\n", "deprecation:pkg/a"); got != "Replace pkg/a.\n\n"+upkeep.FindingMarker("deprecation:pkg/a") {
		t.Errorf("non-advisory filing body = %q, want body + marker", got)
	}
	bare := plan.UpkeepFinding{Source: plan.UpkeepSourceAdvisory,
		ProposedIssue: plan.UpkeepProposedIssue{Title: "agent title", Body: "agent body"}}
	title, body, rendered := upkeepFilingProse(bare)
	if !rendered || strings.Contains(title, "agent") || strings.Contains(body, "agent") {
		t.Errorf("advisory prose = (%q, %q, %v), want server-rendered with no agent prose", title, body, rendered)
	}
}

// TestUpkeepAdvisoryManifestPaths: only manifest file refs are listed — never
// a call-site source file, a run ref, an absolute or root-escaping path, or a
// repeat.
func TestUpkeepAdvisoryManifestPaths(t *testing.T) {
	f := &plan.UpkeepFinding{Evidence: []plan.UpkeepEvidenceRef{
		{Kind: plan.UpkeepEvidenceKindFile, Path: "backend/go.mod"},
		{Kind: plan.UpkeepEvidenceKindFile, Path: "backend/internal/server/serve.go"},
		{Kind: plan.UpkeepEvidenceKindRun, Path: "runner/go.mod"},
		{Kind: plan.UpkeepEvidenceKindFile, Path: "../go.mod"},
		{Kind: plan.UpkeepEvidenceKindFile, Path: "/etc/go.mod"},
		{Kind: plan.UpkeepEvidenceKindFile, Path: ""},
		{Kind: plan.UpkeepEvidenceKindFile, Path: "site/pnpm-lock.yaml"},
		{Kind: plan.UpkeepEvidenceKindFile, Path: "backend/go.mod"},
		{Kind: plan.UpkeepEvidenceKindFile, Path: "go.mod"},
	}}
	got := upkeepAdvisoryManifestPaths(f)
	want := []string{"backend/go.mod", "site/pnpm-lock.yaml", "go.mod"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("manifest paths = %v, want %v", got, want)
	}
}

// TestUpkeepRecordedCoverage_ThreeStates pins the raw-JSON decision directly:
// absent → no marks, an array → marks by finding id (an id-less mark is
// dropped), anything else → unreadable.
func TestUpkeepRecordedCoverage_ThreeStates(t *testing.T) {
	entry := func(payload string) *audit.Entry { return &audit.Entry{Sequence: 7, Payload: json.RawMessage(payload)} }

	if got, err := upkeepRecordedCoverage(entry(`{"artifact_id":"a","duplicates":[]}`)); err != nil || got == nil || len(got) != 0 {
		t.Errorf("absent key = (%v, %v), want empty marks and no error", got, err)
	}
	got, err := upkeepRecordedCoverage(entry(`{"covered":[{"finding_id":"advisory:X:p","package":"p","pulls":[{"number":4,"directory":"backend","bumps_to":"1.0.0"}]},{"package":"q"}]}`))
	if err != nil || len(got) != 1 || got["advisory:X:p"].Pulls[0].Number != 4 {
		t.Errorf("array = (%+v, %v), want one mark for advisory:X:p", got, err)
	}
	long := `"` + strings.Repeat("x", 100) + `"`
	for _, payload := range []string{`{"covered":null}`, `{"covered":"x"}`, `{"covered":` + long + `}`, `{"covered":[1]}`, `[]`} {
		if got, err := upkeepRecordedCoverage(entry(payload)); err == nil {
			t.Errorf("payload %s = %v, want an unreadable error", payload, got)
		}
	}
	if s := upkeepTruncateRaw(json.RawMessage(long)); !strings.HasSuffix(s, "...[truncated]") || len(s) > 64+len("...[truncated]") {
		t.Errorf("truncated = %q, want 64 bytes plus the marker", s)
	}
}

// TestUpkeepRecordedRow_SelectsHighestSequenceForArtifact: the ONE row both
// the duplicates and the coverage marks are read from is the highest-sequence
// recorded row naming the artifact; rows naming another artifact or not
// decoding are passed over, and no matching row or a list failure is an error.
func TestUpkeepRecordedRow_SelectsHighestSequenceForArtifact(t *testing.T) {
	f := newUkApplyFixture(t, ukApplyOpts{omitRecordedRow: true})
	ctx := context.Background()
	art := f.art.ID.String()
	if _, err := f.s.upkeepRecordedRow(ctx, f.stage.RunID, art); err == nil {
		t.Error("no recorded row: want an error")
	}
	f.recordRow(t, map[string]any{"artifact_id": art, "duplicates": []any{}, "marker": "first"})
	f.recordRow(t, map[string]any{"artifact_id": uuid.NewString(), "duplicates": []any{}})
	f.appendRow(t, CategoryUpkeepReportRecorded, "not an object")
	f.recordRow(t, map[string]any{"artifact_id": art, "duplicates": []any{}, "marker": "newest"})
	f.recordRow(t, map[string]any{"artifact_id": uuid.NewString(), "duplicates": []any{}})
	e, err := f.s.upkeepRecordedRow(ctx, f.stage.RunID, art)
	if err != nil || !strings.Contains(string(e.Payload), `"newest"`) {
		t.Fatalf("selected row = %v (err %v), want the newest row naming %s", e, err, art)
	}
	f.au.listErrCategories = map[string]error{CategoryUpkeepReportRecorded: errors.New("audit down")}
	if _, err := f.s.upkeepRecordedRow(ctx, f.stage.RunID, art); err == nil {
		t.Error("list failure: want an error")
	}
}
