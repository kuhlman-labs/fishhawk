package server

// Tests for the on-approval grooming apply hook (E54.19 / #2822).
//
// THE HARNESS IS LOCAL BY DESIGN (binding condition C2). approvals_test.go's
// newApprovalServer / newApprovalServerWithIdentity harness is shared by eight-
// plus tests and wires no ArtifactRepo, which this hook requires. Rather than
// widen it, newGroomingApplyFixture below CALLS the existing fakes
// (newFakeApprovalRepo, newApprovalRunRepo, newApprovalAuditFake,
// groomingSourceArtifactRepo) without modifying any of them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const groomingApplyRepo = "kuhlman-labs/fishhawk"

// groomingApplySpec declares the four backlog-grooming action classes at the
// modes this repository's own workflow declares — hygiene auto, the destructive
// three gated/report. It is what makes TestApplyApprovedGrooming_ReportModeClassNotDispatched
// a real threading assertion rather than a defaulted-away one.
const groomingApplySpec = `
version: "2"
workflows:
  backlog_grooming:
    description: Test grooming workflow
    autonomy: low
    actions:
      hygiene:
        mode: auto
        when: objective_reversible
      ordering:
        mode: gated
      dedup:
        mode: gated
      scoping:
        mode: report
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
        gates:
          - type: approval
            sla: 24_hours
            approvals:
              count: 1
              members: [kuhlman-labs]
`

// groomingApplySpecHygieneReport is groomingApplySpec with HYGIENE moved to
// report mode, so the report-mode short-circuit is exercised on the one class
// this hook decides.
var groomingApplySpecHygieneReport = strings.Replace(groomingApplySpec,
	"      hygiene:\n        mode: auto\n        when: objective_reversible\n",
	"      hygiene:\n        mode: report\n", 1)

func groomingApplyRef(n int) plan.ItemRef {
	id := fmt.Sprintf("%s#%d", groomingApplyRepo, n)
	return plan.ItemRef{
		Type: "github_issue",
		ID:   id,
		URL:  fmt.Sprintf("https://github.com/%s/issues/%d", groomingApplyRepo, n),
	}
}

// groomingApplyEntryIDs names the report's six entry ids — one per class — so a
// test asserts against the id it means rather than re-deriving it inline.
type groomingApplyEntryIDs struct {
	hygiene       string
	dependency    string
	ordering      string
	duplicate     string
	decomposition string
	visionDrift   string
}

// groomingApplyFullReport builds a schema-valid grooming_report carrying ONE
// entry of EVERY class. One-of-each is the point: the hygiene filter's whole
// job is to separate two of these six from the other four.
func groomingApplyFullReport() (*plan.GroomingReport, groomingApplyEntryIDs) {
	h := groomingApplyRef(11)
	dFrom, dTo := groomingApplyRef(12), groomingApplyRef(13)
	o := groomingApplyRef(14)
	pa, pb := groomingApplyRef(15), groomingApplyRef(16)
	dc := groomingApplyRef(17)
	vd := groomingApplyRef(18)

	ids := groomingApplyEntryIDs{
		hygiene:       plan.GroomingEntryID(plan.GroomingClassHygiene, "missing_label_namespace", h),
		dependency:    plan.GroomingEntryID(plan.GroomingClassDependency, "", dFrom, dTo),
		ordering:      plan.GroomingEntryID(plan.GroomingClassOrdering, "", o),
		duplicate:     plan.GroomingEntryID(plan.GroomingClassDuplicate, "", pa, pb),
		decomposition: plan.GroomingEntryID(plan.GroomingClassDecomposition, "", dc),
		visionDrift:   plan.GroomingEntryID(plan.GroomingClassVisionDrift, "V3", vd),
	}

	return &plan.GroomingReport{
		Kind:          "grooming_report",
		ReportVersion: "grooming_report_v1",
		TicketReference: plan.TicketReference{
			Type: plan.TicketType("github_issue"), ID: groomingApplyRepo + "#2822",
			URL: "https://github.com/" + groomingApplyRepo + "/issues/2822",
		},
		GeneratedBy: plan.GeneratedBy{
			Agent: "test", Model: "test-model",
			Timestamp: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC),
		},
		Summary: "one entry of every class",
		Ordering: []plan.OrderingEntry{{
			ID: ids.ordering, ItemRef: o, Rank: 1, Score: 90,
			RubricCitations: []plan.RubricCitation{{RubricID: "V1"}},
		}},
		Duplicates: []plan.DuplicateCandidate{{
			ID: ids.duplicate, Pair: []plan.ItemRef{pa, pb},
			Basis: "same defect", Confidence: "high",
		}},
		HygieneDefects: []plan.HygieneDefect{{
			ID: ids.hygiene, ItemRef: h, Defect: "missing_label_namespace",
			Detail: "no area: label",
			// The prose stays prose; the apply path reads `fix` and only
			// `fix` (#2847), so the fixture must carry the structured member
			// or this entry would skip and the dispatch assertions below
			// would be vacuously green.
			SuggestedFix: "Please attach the ownership marking for backend duties",
			Fix:          &plan.HygieneFix{Labels: []string{"area:api"}},
		}},
		DependencyEdges: []plan.DependencyEdge{{
			ID: ids.dependency, From: dFrom, To: dTo, Basis: "shared seam", Kind: "depends_on",
		}},
		VisionDrift: []plan.VisionDriftFlag{{
			ID: ids.visionDrift, ItemRef: vd, Basis: "non_goal",
			CharterRefID: "V3", Detail: "advances a charter non-goal",
		}},
		DecompositionSuggestions: []plan.DecompositionSuggestion{{
			ID: ids.decomposition, ItemRef: dc, Rationale: "too large",
			ProposedChildren: []plan.DecompositionChild{
				{Title: "a", ScopeHint: "x"}, {Title: "b", ScopeHint: "y"},
			},
		}},
	}, ids
}

func groomingApplyReportJSON(t *testing.T, report *plan.GroomingReport) []byte {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal grooming report: %v", err)
	}
	// Parse it back through the production parser so a fixture that the schema
	// would reject fails HERE, in the fixture, rather than as a mysterious
	// degrade inside the hook under test.
	if _, perr := plan.ParseGroomingReport(b); perr != nil {
		t.Fatalf("fixture report is not schema-valid: %v", perr)
	}
	return b
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// groomingApplyMutator records EVERY dispatch. Tests assert on this call log
// rather than on a returned error: the hook is best-effort and returns nothing,
// so what the provider was ASKED to do is the only discriminating observation.
type groomingApplyMutator struct {
	mu    sync.Mutex
	calls []workmgmt.GroomingMutationRequest
	err   error
	// block makes the first dispatch wait for the context (or the duration),
	// which is how the bounded-context test wedges a forge.
	block time.Duration
}

func (m *groomingApplyMutator) ApplyGroomingMutation(ctx context.Context, req workmgmt.GroomingMutationRequest) (*workmgmt.GroomingMutationResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, req)
	block, err := m.block, m.err
	m.mu.Unlock()
	if block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(block):
		}
	}
	if err != nil {
		return nil, err
	}
	return &workmgmt.GroomingMutationResult{Applied: true, ProviderResponse: "applied " + string(req.Kind)}, nil
}

func (m *groomingApplyMutator) dialed() []workmgmt.GroomingMutationRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]workmgmt.GroomingMutationRequest(nil), m.calls...)
}

func (m *groomingApplyMutator) dialedEntryIDs() []string {
	var out []string
	for _, c := range m.dialed() {
		out = append(out, c.EntryID)
	}
	return out
}

// groomingApplyReader serves a canned open issue with no labels, so a proposed
// label set is never already-applied.
type groomingApplyReader struct {
	mu    sync.Mutex
	reads []string
	err   error
}

func (r *groomingApplyReader) ReadWorkItem(_ context.Context, req workmgmt.ReadWorkItemRequest) (*workmgmt.WorkItemRecord, error) {
	r.mu.Lock()
	r.reads = append(r.reads, req.Ref)
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &workmgmt.WorkItemRecord{State: "open", OnBoard: true, BoardColumn: "Backlog", BoardState: "backlog"}, nil
}

func (r *groomingApplyReader) ListWorkItems(context.Context, workmgmt.ListWorkItemsRequest) (*workmgmt.WorkItemPage, error) {
	return nil, errors.New("not used by the apply layer")
}

// ---------------------------------------------------------------------------
// Local harness (binding condition C2)
// ---------------------------------------------------------------------------

// groomingApplyApprovalRepo COMPOSES approvals_test.go's fakeApprovalRepo
// rather than modifying it (binding condition C2): it delegates every method
// and adds only an injectable ListForStage error, which the shared fake has no
// field for and which the re-ratification read's own error branch needs.
type groomingApplyApprovalRepo struct {
	*fakeApprovalRepo
	errMu   sync.Mutex
	listErr error
}

func (r *groomingApplyApprovalRepo) setListErr(err error) {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	r.listErr = err
}

func (r *groomingApplyApprovalRepo) ListForStage(ctx context.Context, stageID uuid.UUID) ([]*approval.Approval, error) {
	r.errMu.Lock()
	err := r.listErr
	r.errMu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fakeApprovalRepo.ListForStage(ctx, stageID)
}

type groomingApplyOpts struct {
	// specYAML overrides the run's cached workflow spec.
	specYAML string
	// omitReport ships a plan stage with NO grooming_report artifact.
	omitReport bool
	// badReport ships a grooming_report artifact whose bytes do not parse.
	badReport bool
	// omitRun leaves the run row unseeded, so GetRun fails.
	omitRun bool
	// conventionsErr / mutatorErr / readerErr force each resolution rung to
	// degrade.
	conventionsErr error
	mutatorErr     error
	readerErr      error
	// mutatorFailure makes every dispatch return an error.
	mutatorFailure error
	// mutatorBlock wedges each dispatch on the context.
	mutatorBlock time.Duration
}

type groomingApplyFixture struct {
	server    *Server
	runs      *approvalRunRepo
	approvals *groomingApplyApprovalRepo
	audit     *approvalAuditFake
	artifacts *groomingSourceArtifactRepo
	mutator   *groomingApplyMutator
	reader    *groomingApplyReader
	stage     *run.Stage
	run       *run.Run
	report    *plan.GroomingReport
	ids       groomingApplyEntryIDs
}

func newGroomingApplyFixture(t *testing.T, opts groomingApplyOpts) *groomingApplyFixture {
	t.Helper()

	ar := &groomingApplyApprovalRepo{fakeApprovalRepo: newFakeApprovalRepo()}
	rr := newApprovalRunRepo()
	au := newApprovalAuditFake()
	arts := &groomingSourceArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{}}

	stage := rr.seedStage(run.StageStateAwaitingApproval)
	specYAML := opts.specYAML
	if specYAML == "" {
		specYAML = groomingApplySpec
	}
	install := int64(4242)
	rn := &run.Run{
		ID: stage.RunID, Repo: groomingApplyRepo,
		WorkflowID: "backlog_grooming", WorkflowSpec: []byte(specYAML),
		InstallationID: &install,
		CreatedAt:      time.Now().UTC(),
	}
	if !opts.omitRun {
		rr.seedRun(rn)
	}

	report, ids := groomingApplyFullReport()
	if !opts.omitReport {
		content := groomingApplyReportJSON(t, report)
		if opts.badReport {
			content = []byte(`{"kind":"grooming_report","not":"parseable"`)
		}
		arts.byStage[stage.ID] = []*artifact.Artifact{{
			ID: uuid.New(), StageID: stage.ID, Kind: artifact.KindGroomingReport,
			Content: content, CreatedAt: time.Now().UTC(),
		}}
	}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: ar,
		RunRepo:      rr,
		AuditRepo:    au,
		ArtifactRepo: arts,
	})

	mut := &groomingApplyMutator{err: opts.mutatorFailure, block: opts.mutatorBlock}
	rdr := &groomingApplyReader{}

	prevConv := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		if opts.conventionsErr != nil {
			return workmgmt.Conventions{}, opts.conventionsErr
		}
		return workmgmt.Conventions{
			Provider: "github",
			States: map[string]string{
				workmgmt.CanonicalStateBacklog:    "Backlog",
				workmgmt.CanonicalStateUpNext:     "Up Next",
				workmgmt.CanonicalStateInProgress: "In Progress",
				workmgmt.CanonicalStateDone:       "Done",
			},
		}, nil
	}
	t.Cleanup(func() { conventionsLoader = prevConv })

	prevMut := groomingMutatorFor
	groomingMutatorFor = func(string) (workmgmt.GroomingMutator, error) {
		if opts.mutatorErr != nil {
			return nil, opts.mutatorErr
		}
		return mut, nil
	}
	t.Cleanup(func() { groomingMutatorFor = prevMut })

	prevRdr := groomingReaderFor
	groomingReaderFor = func(string) (workmgmt.WorkItemReader, error) {
		if opts.readerErr != nil {
			return nil, opts.readerErr
		}
		return rdr, nil
	}
	t.Cleanup(func() { groomingReaderFor = prevRdr })

	return &groomingApplyFixture{
		server: s, runs: rr, approvals: ar, audit: au, artifacts: arts,
		mutator: mut, reader: rdr, stage: stage, run: rn, report: report, ids: ids,
	}
}

// seedApproval inserts an approval row DIRECTLY through the repo. Bad state is
// seeded BY CONSTRUCTION rather than by driving the handler, so a counterfactual
// RED lands on the behavioural assertion and never on a fixture-setup guard.
func (f *groomingApplyFixture) seedApproval(t *testing.T, subject string, decision approval.Decision) {
	t.Helper()
	if _, err := f.approvals.Submit(context.Background(), approval.SubmitParams{
		StageID: f.stage.ID, ApproverSubject: subject, Decision: decision, Surface: "test",
	}); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
}

// groomingAudit returns the appended audit rows of the two grooming categories.
func (f *groomingApplyFixture) groomingAudit() []audit.ChainAppendParams {
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range f.audit.appended {
		if p.Category == workmgmt.GroomingMutationAppliedCategory ||
			p.Category == workmgmt.GroomingApplyCompletedCategory {
			out = append(out, p)
		}
	}
	return out
}

// groomingAuditCategories renders the grooming audit trail compactly, so a
// counterfactual RED reads as a list of categories rather than a page of raw
// payload bytes.
func (f *groomingApplyFixture) groomingAuditCategories() []string {
	var out []string
	for _, p := range f.groomingAudit() {
		out = append(out, p.Category)
	}
	return out
}

func (f *groomingApplyFixture) rowsOfCategory(category string) []audit.ChainAppendParams {
	var out []audit.ChainAppendParams
	for _, p := range f.groomingAudit() {
		if p.Category == category {
			out = append(out, p)
		}
	}
	return out
}

// mutationRecords decodes the grooming_mutation_applied payloads BARE, exactly
// as priorGroomingDispositions does — so a wrapped payload fails here too.
func (f *groomingApplyFixture) mutationRecords(t *testing.T) map[string]workmgmt.GroomingMutationRecord {
	t.Helper()
	out := map[string]workmgmt.GroomingMutationRecord{}
	for _, p := range f.rowsOfCategory(workmgmt.GroomingMutationAppliedCategory) {
		var rec workmgmt.GroomingMutationRecord
		if err := json.Unmarshal(p.Payload, &rec); err != nil {
			t.Fatalf("decode grooming_mutation_applied payload: %v (%s)", err, p.Payload)
		}
		out[rec.EntryID] = rec
	}
	return out
}

func (f *groomingApplyFixture) degradeReason(t *testing.T) string {
	t.Helper()
	rows := f.rowsOfCategory(workmgmt.GroomingApplyCompletedCategory)
	if len(rows) != 1 {
		t.Fatalf("grooming_apply_completed rows = %d, want exactly 1", len(rows))
	}
	var got groomingApplyDegradePayload
	if err := json.Unmarshal(rows[0].Payload, &got); err != nil {
		t.Fatalf("decode grooming_apply_completed payload: %v", err)
	}
	if !got.Degraded {
		t.Errorf("payload = %s, want degraded:true", rows[0].Payload)
	}
	return got.DegradeReason
}

// ---------------------------------------------------------------------------
// The hygiene class filter (COUNTERFACTUAL 3)
// ---------------------------------------------------------------------------

// TestHygieneOnlyGroomingDecisions_ExcludesDestructiveClasses is the pure
// class-boundary assertion. This is the control that keeps `dedup` and
// `scoping` from closing or iceboxing real issues on a single gate approval.
//
// COUNTERFACTUAL: widen the filter (drop the GroomingActionClassFor ==
// ActionGroomHygiene test, or add the other four arrays to the loop) and this
// reddens on the exact-set comparison.
func TestHygieneOnlyGroomingDecisions_ExcludesDestructiveClasses(t *testing.T) {
	report, ids := groomingApplyFullReport()

	got := hygieneOnlyGroomingDecisions(report)
	want := map[string]bool{ids.hygiene: true, ids.dependency: true}

	if len(got) != len(want) {
		t.Fatalf("decisions = %d (%+v), want exactly %d (the hygiene defect and the dependency edge)",
			len(got), got, len(want))
	}
	for _, d := range got {
		if !want[d.EntryID] {
			t.Errorf("decision for %q is not a hygiene-class entry; the filter has widened", d.EntryID)
		}
		if d.Verdict != workmgmt.GroomingApproved {
			t.Errorf("verdict for %q = %q, want approved", d.EntryID, d.Verdict)
		}
		if d.CloseTarget != "" {
			t.Errorf("decision for %q carries CloseTarget %q; no hygiene kind is a duplicate close",
				d.EntryID, d.CloseTarget)
		}
	}
	for _, forbidden := range []string{ids.ordering, ids.duplicate, ids.decomposition, ids.visionDrift} {
		if want[forbidden] {
			t.Fatalf("fixture bug: %q is both a destructive id and an expected hygiene id", forbidden)
		}
	}
}

// TestHygieneOnlyGroomingDecisions_EdgeCases pins the two defensive branches of
// the synthesizer: a nil report, and an entry with an empty id (dropped rather
// than decided, because an empty id would fail the apply layer's join check and
// refuse the WHOLE apply).
func TestHygieneOnlyGroomingDecisions_EdgeCases(t *testing.T) {
	if got := hygieneOnlyGroomingDecisions(nil); got != nil {
		t.Errorf("nil report yielded %+v, want nil", got)
	}
	report, ids := groomingApplyFullReport()
	report.HygieneDefects[0].ID = "   "
	got := hygieneOnlyGroomingDecisions(report)
	if len(got) != 1 || got[0].EntryID != ids.dependency {
		t.Errorf("decisions = %+v, want only the dependency edge (the blank-id hygiene entry is dropped)", got)
	}
}

// ---------------------------------------------------------------------------
// C1 — the decision guard (COUNTERFACTUAL 1). Issue AC4.
// ---------------------------------------------------------------------------

// TestApplyApprovedGrooming_RejectDispatchesNothing is C1's counterfactual
// vehicle: a REJECT decision applies nothing.
//
// THE STAGE IS RATIFIED BY CONSTRUCTION — one approve row, no rejections — so
// C3 (the re-ratification predicate) PASSES and C1 is the only thing standing
// between this call and a full hygiene dispatch. That construction is
// deliberate: on a plain reject submission C3 would also refuse, and a
// zero-dial assertion would stay green with C1 deleted, which is the masking
// trap (a) the counterfactual rules name.
//
// COUNTERFACTUAL: delete `if decision != approval.DecisionApprove { return }`
// and the hygiene mutations dispatch on the reject path.
func TestApplyApprovedGrooming_RejectDispatchesNothing(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionReject)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v on a REJECT; a rejected report must apply nothing", dialed)
	}
	if rows := f.groomingAudit(); len(rows) != 0 {
		t.Errorf("grooming audit rows = %d (%v), want 0 — a reject must not even enter the apply",
			len(rows), f.groomingAuditCategories())
	}
}

// TestSubmitApproval_RejectOnGroomStageAppliesNothing proves the CALL SITE
// passes the decision through: the reject travels the real
// POST /v0/stages/{stage_id}/approvals handler into finishApprovalAdvance's
// type-only plan block. Deleting the `p.Decision` argument (or moving the call
// into the approve-only block) does not redden this — that is C1's own test
// above — but deleting the call site entirely does.
func TestSubmitApproval_RejectOnGroomStageAppliesNothing(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})

	w := submitApproval(t, f.server, f.stage.ID, `{"decision":"reject","comment":"re-rank first"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v on a rejected gate", dialed)
	}
	if rows := f.groomingAudit(); len(rows) != 0 {
		t.Errorf("grooming audit rows = %d, want 0", len(rows))
	}
}

// ---------------------------------------------------------------------------
// C3 — the re-ratification predicate (COUNTERFACTUAL 2)
// ---------------------------------------------------------------------------

// TestApplyApprovedGrooming_ContestedGateDispatchesNothing is C3's
// counterfactual vehicle: a gate carrying a rejection alongside a grant applies
// nothing, even though THIS submission is an approve.
//
// THE ASSERTION READS COMMITTED STATE, not a returned error — the hook returns
// nothing, and a control whose effect is a write is only observable in what was
// written. Bad state is seeded by construction (the reject row is inserted
// directly through the repo, not produced by driving the handler).
//
// COUNTERFACTUAL: delete the `rejections > 0` half of the predicate and the
// hygiene mutations dispatch on a contested gate.
func TestApplyApprovedGrooming_ContestedGateDispatchesNothing(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)
	f.seedApproval(t, "someone-else", approval.DecisionReject)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v on a CONTESTED gate; a contested gate must apply nothing", dialed)
	}
	if got := f.degradeReason(t); got != groomingApplyNotRatified {
		t.Errorf("degrade_reason = %q, want %q", got, groomingApplyNotRatified)
	}
	if n := len(f.rowsOfCategory(workmgmt.GroomingMutationAppliedCategory)); n != 0 {
		t.Errorf("grooming_mutation_applied rows = %d, want 0 on a refused apply", n)
	}
}

// TestApplyApprovedGrooming_UngrantedGateDispatchesNothing is the other half of
// the same predicate: ZERO grants (the gate rows could not be read back, or the
// submission was recorded-but-not-counted) applies nothing.
func TestApplyApprovedGrooming_UngrantedGateDispatchesNothing(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	// No approval rows at all: grants == 0.

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v on an ungranted gate", dialed)
	}
	if got := f.degradeReason(t); got != groomingApplyNotRatified {
		t.Errorf("degrade_reason = %q, want %q", got, groomingApplyNotRatified)
	}
}

// TestApplyApprovedGrooming_ApprovalRepoErrorDispatchesNothing pins the
// ratification read's own error branch: an unreadable approval list is NOT a
// ratified gate.
func TestApplyApprovedGrooming_ApprovalRepoErrorDispatchesNothing(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.approvals.setListErr(errors.New("approvals unavailable"))

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v with an unreadable approval list", dialed)
	}
	if got := f.degradeReason(t); got != groomingApplyNotRatified {
		t.Errorf("degrade_reason = %q, want %q", got, groomingApplyNotRatified)
	}
}

// ---------------------------------------------------------------------------
// C2 — the report-absent early-out. NOT A CONTROL, stated rather than implied.
// ---------------------------------------------------------------------------

// TestApplyApprovedGrooming_OrdinaryPlanStageNoOps is a REGRESSION PIN, not a
// counterfactual vehicle: the grooming_report-absent early-out is
// behaviour-PRESERVING under deletion (with no report the loader finds nothing
// and the hook returns having written nothing either way). It is named as a pin
// that an ordinary, non-grooming plan approval writes no grooming audit row and
// dials no provider.
func TestApplyApprovedGrooming_OrdinaryPlanStageNoOps(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{omitReport: true})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v on an ordinary plan stage", dialed)
	}
	if rows := f.groomingAudit(); len(rows) != 0 {
		t.Errorf("grooming audit rows = %d (%v), want 0 on an ordinary plan approval",
			len(rows), f.groomingAuditCategories())
	}
}

// TestApplyApprovedGrooming_ArtifactListErrorIsSilent pins the loader's own
// fail-safe branch: an unreadable artifact list is indistinguishable from "this
// stage never had a report", so the hook stays silent rather than degrading
// every ordinary plan approval into a grooming audit row.
func TestApplyApprovedGrooming_ArtifactListErrorIsSilent(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.artifacts.err = errors.New("artifacts unavailable")
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v with an unreadable artifact list", dialed)
	}
	if rows := f.groomingAudit(); len(rows) != 0 {
		t.Errorf("grooming audit rows = %d, want 0", len(rows))
	}
}

// ---------------------------------------------------------------------------
// Degrade modes — ONE TEST PER NAMED FAILURE MODE
// ---------------------------------------------------------------------------

// TestApplyApprovedGrooming_DegradeModes drives one case per degrade constant
// and asserts THAT branch's observable behaviour: zero mutator dials, zero
// grooming_mutation_applied rows, and exactly one grooming_apply_completed row
// carrying that reason.
func TestApplyApprovedGrooming_DegradeModes(t *testing.T) {
	cases := []struct {
		name string
		opts groomingApplyOpts
		want string
	}{
		{"report_unparseable", groomingApplyOpts{badReport: true}, groomingApplyReportUnparseable},
		{"run_unreadable", groomingApplyOpts{omitRun: true}, groomingApplyRunUnreadable},
		{"conventions_unavailable", groomingApplyOpts{conventionsErr: errors.New("no conventions")}, groomingApplyConventionsUnavailable},
		{"mutator_unavailable", groomingApplyOpts{mutatorErr: &workmgmt.UnavailableError{
			Provider: "gitlab", Capability: workmgmt.GroomingCapability, Reason: workmgmt.ReasonNotImplemented,
		}}, groomingApplyMutatorUnavailable},
		{"reader_unavailable", groomingApplyOpts{readerErr: &workmgmt.UnavailableError{
			Provider: "gitlab", Capability: workmgmt.ReaderCapability, Reason: workmgmt.ReasonNotImplemented,
		}}, groomingApplyReaderUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroomingApplyFixture(t, tc.opts)
			f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

			f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

			if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
				t.Errorf("mutator dialed %v on the %s degrade path", dialed, tc.name)
			}
			if n := len(f.rowsOfCategory(workmgmt.GroomingMutationAppliedCategory)); n != 0 {
				t.Errorf("grooming_mutation_applied rows = %d, want 0 — a degrade must leave the churn baseline untouched", n)
			}
			if got := f.degradeReason(t); got != tc.want {
				t.Errorf("degrade_reason = %q, want %q", got, tc.want)
			}
			// The approval row is still present and nothing unwound it.
			rows, err := f.approvals.ListForStage(context.Background(), f.stage.ID)
			if err != nil || len(rows) != 1 {
				t.Errorf("approvals after degrade = %d (err %v), want the grant intact", len(rows), err)
			}
		})
	}
}

// TestApplyApprovedGrooming_RepoUnresolvableDegrades covers the remaining named
// reason: a run whose Repo is not owner/name yields no Target and applies
// nothing.
func TestApplyApprovedGrooming_RepoUnresolvableDegrades(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.run.Repo = "not-a-full-name"
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v with an unresolvable repo", dialed)
	}
	if got := f.degradeReason(t); got != groomingApplyRepoUnresolvable {
		t.Errorf("degrade_reason = %q, want %q", got, groomingApplyRepoUnresolvable)
	}
}

// ---------------------------------------------------------------------------
// Outcome tests
// ---------------------------------------------------------------------------

// TestApplyApprovedGrooming_NonHygieneEntriesRecordedNoDecision is the hygiene
// filter's OBSERVABLE consequence, one layer out from the pure unit: the
// destructive entries reach the apply layer with no decision, are recorded
// skipped with `no_decision`, and no destructive kind is ever dialed.
func TestApplyApprovedGrooming_NonHygieneEntriesRecordedNoDecision(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	recs := f.mutationRecords(t)
	for _, id := range []string{f.ids.ordering, f.ids.duplicate, f.ids.decomposition} {
		rec, ok := recs[id]
		if !ok {
			t.Fatalf("no grooming_mutation_applied row for %q; every candidate must be audited", id)
		}
		if rec.Outcome != workmgmt.GroomingOutcomeSkipped {
			t.Errorf("%q outcome = %q, want skipped", id, rec.Outcome)
		}
		if rec.SkipReason != workmgmt.GroomingSkipNoDecision {
			t.Errorf("%q skip_reason = %q, want %q", id, rec.SkipReason, workmgmt.GroomingSkipNoDecision)
		}
	}
	// The vision-drift flag derives no mutation at all, so it settles earlier
	// in the ladder — as a finding, never as an undecided candidate.
	if rec, ok := recs[f.ids.visionDrift]; !ok || rec.SkipReason != workmgmt.GroomingSkipFindingOnly {
		t.Errorf("vision drift record = %+v, want skip_reason %q", rec, workmgmt.GroomingSkipFindingOnly)
	}
	for _, c := range f.mutator.dialed() {
		if c.Kind.Destructive() {
			t.Errorf("mutator dialed a DESTRUCTIVE kind %q for entry %q", c.Kind, c.EntryID)
		}
		if c.Kind == workmgmt.GroomingKindRankSet {
			t.Errorf("mutator dialed rank_set for entry %q; ordering is proposal-only", c.EntryID)
		}
	}
	// And the hygiene pair DID dispatch, so the test is not vacuously green.
	dialed := f.mutator.dialedEntryIDs()
	if len(dialed) != 2 {
		t.Fatalf("dialed = %v, want exactly the hygiene defect and the dependency edge", dialed)
	}
}

// TestApplyApprovedGrooming_ReportModeClassNotDispatched proves the RESOLVED
// autonomy matrix is really threaded through rather than defaulted away: with
// hygiene at `mode: report`, the proposals are surfaced and nothing dispatches.
func TestApplyApprovedGrooming_ReportModeClassNotDispatched(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{specYAML: groomingApplySpecHygieneReport})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v under mode: report; report mode surfaces and acts on nothing", dialed)
	}
	recs := f.mutationRecords(t)
	for _, id := range []string{f.ids.hygiene, f.ids.dependency} {
		rec, ok := recs[id]
		if !ok {
			t.Fatalf("no record for %q", id)
		}
		if rec.SkipReason != workmgmt.GroomingSkipReportMode {
			t.Errorf("%q skip_reason = %q, want %q", id, rec.SkipReason, workmgmt.GroomingSkipReportMode)
		}
	}
}

// TestApplyApprovedGrooming_UnreadableSpecResolvesGated pins the fail-closed
// projection: an unparseable workflow spec yields an EMPTY mode map, every
// class resolves gated, and no destructive kind is dialed. The hygiene pair
// still settles — gated is not report, and hygiene is not destructive — so the
// degenerate path is fail-closed where it matters, not fail-silent everywhere.
func TestApplyApprovedGrooming_UnreadableSpecResolvesGated(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{specYAML: "version: \"2\"\nnot: valid yaml at all\n  - x\n"})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	for _, c := range f.mutator.dialed() {
		if c.Kind.Destructive() {
			t.Errorf("mutator dialed a DESTRUCTIVE kind %q with an unreadable spec", c.Kind)
		}
	}
	recs := f.mutationRecords(t)
	if rec, ok := recs[f.ids.duplicate]; !ok || rec.Outcome != workmgmt.GroomingOutcomeSkipped {
		t.Errorf("duplicate record = %+v, want skipped", rec)
	}
}

// TestApplyApprovedGrooming_ProviderFailureRecordedFailed: a provider error
// records outcome failed for that entry, the loop CONTINUES to the next, and
// the approval is never unwound.
func TestApplyApprovedGrooming_ProviderFailureRecordedFailed(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{mutatorFailure: errors.New("projects token unset")})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 2 {
		t.Errorf("dialed = %v, want 2 — continue-and-report must not abort on the first failure", dialed)
	}
	recs := f.mutationRecords(t)
	for _, id := range []string{f.ids.hygiene, f.ids.dependency} {
		rec, ok := recs[id]
		if !ok {
			t.Fatalf("no record for %q", id)
		}
		if rec.Outcome != workmgmt.GroomingOutcomeFailed {
			t.Errorf("%q outcome = %q, want failed", id, rec.Outcome)
		}
		if !strings.Contains(rec.Error, "projects token unset") {
			t.Errorf("%q error = %q, want the provider message", id, rec.Error)
		}
	}
	rows, err := f.approvals.ListForStage(context.Background(), f.stage.ID)
	if err != nil || len(rows) != 1 || rows[0].Decision != approval.DecisionApprove {
		t.Errorf("approvals = %+v (err %v), want the grant intact — a provider failure never unwinds the gate", rows, err)
	}
}

// TestApplyApprovedGrooming_BoundedContext: a wedged forge cannot hold the
// operator's approve request open. Every deadline-competing duration is derived
// through timescale.D so the discrimination ratio holds at any factor; no raw
// elapsed upper bound is asserted.
func TestApplyApprovedGrooming_BoundedContext(t *testing.T) {
	budget := timescale.D(150 * time.Millisecond)
	prev := groomingApplyBudget
	groomingApplyBudget = budget
	t.Cleanup(func() { groomingApplyBudget = prev })

	// The wedge is an order of magnitude past the budget, so the budget — not
	// the sleep — is what releases the call.
	f := newGroomingApplyFixture(t, groomingApplyOpts{mutatorBlock: budget * 10})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	done := make(chan struct{})
	go func() {
		f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(budget * 8):
		t.Fatal("applyApprovedGrooming did not return within 8x its budget; a wedged forge is holding the approve request open")
	}

	// The wedged candidates are RECORDED, not silently dropped.
	recs := f.mutationRecords(t)
	for _, id := range []string{f.ids.hygiene, f.ids.dependency} {
		if _, ok := recs[id]; !ok {
			t.Errorf("no grooming_mutation_applied row for %q; a budget expiry must still audit every candidate", id)
		}
	}
	if n := len(f.rowsOfCategory(workmgmt.GroomingApplyCompletedCategory)); n != 1 {
		t.Errorf("grooming_apply_completed rows = %d, want 1", n)
	}
}

// TestApplyApprovedGrooming_DetachedFromRequestCancellation is the other half of
// the context construction: cancelling the CALLER's context (an operator's
// client disconnecting mid-approve) must not strand a half-applied report.
// context.WithoutCancel is what makes the apply run to completion.
func TestApplyApprovedGrooming_DetachedFromRequestCancellation(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the hook runs

	f.server.applyApprovedGrooming(ctx, f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 2 {
		t.Errorf("dialed = %v, want 2 — a cancelled request context must not abort the apply", dialed)
	}
}

// TestApplyApprovedGrooming_AuditSinkErrorSurfaced: a failing AuditRepo yields a
// *workmgmt.GroomingAuditError from ApplyGrooming. The hook logs it rather than
// swallowing it, and — the observable that matters — it still dispatched
// everything, because continue-and-report holds through an audit failure.
func TestApplyApprovedGrooming_AuditSinkErrorSurfaced(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)
	f.audit.mu.Lock()
	f.audit.appendErr = errors.New("audit chain unavailable")
	f.audit.mu.Unlock()

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 2 {
		t.Errorf("dialed = %v, want 2 — an audit-sink failure must not abort the apply", dialed)
	}
	if rows := f.groomingAudit(); len(rows) != 0 {
		t.Errorf("grooming audit rows = %d, want 0 — the sink was failing throughout", len(rows))
	}
}

// TestGroomingApplyAuditSink_MarshalsBare is the payload-shape pin (#2813 /
// binding condition C6): both sink methods marshal the workmgmt record BARE,
// with its own json tags, NOT wrapped in a run/stage envelope. The run and
// stage identity ride the audit row's own columns.
func TestGroomingApplyAuditSink_MarshalsBare(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	rows := f.rowsOfCategory(workmgmt.GroomingMutationAppliedCategory)
	if len(rows) == 0 {
		t.Fatal("no grooming_mutation_applied rows")
	}
	var raw map[string]any
	if err := json.Unmarshal(rows[0].Payload, &raw); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, ok := raw["entry_id"]; !ok {
		t.Errorf("payload = %s, want entry_id at the TOP level (priorGroomingDispositions reads it there)", rows[0].Payload)
	}
	for _, forbidden := range []string{"run_id", "stage_id", "record", "mutation"} {
		if _, wrapped := raw[forbidden]; wrapped {
			t.Errorf("payload carries an envelope key %q; the churn guard decodes the record BARE", forbidden)
		}
	}
	if rows[0].StageID == nil || *rows[0].StageID != f.stage.ID {
		t.Errorf("audit row StageID = %v, want the groom stage id on the COLUMN", rows[0].StageID)
	}
	if rows[0].RunID != f.stage.RunID {
		t.Errorf("audit row RunID = %v, want %v", rows[0].RunID, f.stage.RunID)
	}
}

// ---------------------------------------------------------------------------
// CROSS-BOUNDARY INTEGRATION
// ---------------------------------------------------------------------------

// TestApproveGroomStage_AppliesHygieneAndAuditsEndToEnd drives the whole seam:
// a real Postgres run/stage/artifact/approval/audit stack, a real
// POST /v0/stages/{stage_id}/approvals approve, the real apply layer, and then
// — the assertion per-layer units cannot make — the resulting audit rows fed
// back through the UNCHANGED priorGroomingDispositions.
//
// That round trip is what proves the payload the sink WRITES is the payload the
// churn guard READS (#2813 / binding condition C6). A per-layer unit can pin
// each side's shape; only this can pin that they are the same shape.
//
// It is also this change's DONE-MEANS test: it asserts the shipped observable —
// an approved report's hygiene mutations reaching the provider and being
// audited — so a comment-only touch of the scoped files still fails here.
func TestApproveGroomStage_AppliesHygieneAndAuditsEndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	apprRepo := approval.NewPostgresRepository(pool)

	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: groomingApplyRepo, WorkflowID: "backlog_grooming", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI, WorkflowSpec: []byte(groomingApplySpec),
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
	if _, err := runRepo.TransitionStage(ctx, stage.ID, run.StageStateAwaitingApproval, nil); err != nil {
		t.Fatalf("park stage at awaiting_approval: %v", err)
	}

	report, ids := groomingApplyFullReport()
	sv := "grooming_report_v1"
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindGroomingReport,
		SchemaVersion: &sv, Content: groomingApplyReportJSON(t, report),
	}); err != nil {
		t.Fatalf("create grooming_report artifact: %v", err)
	}

	mut := &groomingApplyMutator{}
	rdr := &groomingApplyReader{}
	prevConv, prevMut, prevRdr := conventionsLoader, groomingMutatorFor, groomingReaderFor
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		return workmgmt.Conventions{Provider: "github", States: map[string]string{
			workmgmt.CanonicalStateBacklog: "Backlog", workmgmt.CanonicalStateUpNext: "Up Next",
		}}, nil
	}
	groomingMutatorFor = func(string) (workmgmt.GroomingMutator, error) { return mut, nil }
	groomingReaderFor = func(string) (workmgmt.WorkItemReader, error) { return rdr, nil }
	t.Cleanup(func() {
		conventionsLoader, groomingMutatorFor, groomingReaderFor = prevConv, prevMut, prevRdr
	})

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: runRepo, ArtifactRepo: artRepo,
		AuditRepo: auditRepo, ApprovalRepo: apprRepo,
	})

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"hygiene looks right"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// (1) The provider was dialed for EXACTLY the hygiene-class entry ids.
	dialed := mut.dialedEntryIDs()
	want := map[string]bool{ids.hygiene: true, ids.dependency: true}
	if len(dialed) != len(want) {
		t.Fatalf("dialed = %v, want exactly the hygiene defect and the dependency edge", dialed)
	}
	for _, id := range dialed {
		if !want[id] {
			t.Errorf("provider dialed %q, which is not a hygiene-class entry", id)
		}
	}

	// (2) One grooming_mutation_applied row per SETTLED candidate, and exactly
	// one grooming_apply_completed summary.
	applied, err := auditRepo.ListForRunByCategory(ctx, rn.ID, workmgmt.GroomingMutationAppliedCategory)
	if err != nil {
		t.Fatalf("list mutation rows: %v", err)
	}
	if len(applied) != 6 {
		t.Errorf("grooming_mutation_applied rows = %d, want 6 (one per report entry)", len(applied))
	}
	completed, err := auditRepo.ListForRunByCategory(ctx, rn.ID, workmgmt.GroomingApplyCompletedCategory)
	if err != nil {
		t.Fatalf("list summary rows: %v", err)
	}
	if len(completed) != 1 {
		t.Fatalf("grooming_apply_completed rows = %d, want exactly 1", len(completed))
	}
	var summary workmgmt.GroomingApplySummary
	if err := json.Unmarshal(completed[0].Payload, &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Applied != 2 || summary.Skipped != 4 || summary.Failed != 0 {
		t.Errorf("summary = %+v, want applied 2 / skipped 4 / failed 0", summary)
	}

	// (3) THE SEAM: feed those rows back through the UNCHANGED
	// priorGroomingDispositions and confirm the applied set round-trips with no
	// spurious rejections.
	decisions, appliedResult, derr := s.priorGroomingDispositions(ctx, rn.ID)
	if derr != nil {
		t.Fatalf("priorGroomingDispositions: %v", derr)
	}
	if len(decisions) != 0 {
		t.Errorf("dispositions carry %d rejections (%+v), want 0 — nothing was rejected", len(decisions), decisions)
	}
	got := map[string]bool{}
	for _, rec := range appliedResult.Applied {
		got[rec.EntryID] = true
	}
	if len(got) != 2 || !got[ids.hygiene] || !got[ids.dependency] {
		t.Errorf("round-tripped applied set = %v, want exactly {%q, %q} — the payload the sink writes is not the payload the churn guard reads",
			got, ids.hygiene, ids.dependency)
	}
}
