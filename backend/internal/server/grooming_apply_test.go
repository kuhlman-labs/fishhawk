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
	// refuse makes every dispatch return a REFUSAL carrying this reason.
	refuse string
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
	m.mu.Lock()
	refuse := m.refuse
	m.mu.Unlock()
	if refuse != "" {
		return &workmgmt.GroomingMutationResult{Refused: true, RefuseReason: refuse,
			ProviderResponse: "refused " + string(req.Kind)}, nil
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

// groomingApplyAuditFake decorates the shared approvalAuditFake (unmodified,
// binding condition C2) with a FUNCTIONAL ListForRunByCategory and a SEQUENCED
// AppendChained, which the #2991 window settlement's non-atomic fallback needs
// (the base fake's ListForRunByCategory returns an error and its AppendChained
// returns a zero Sequence). It does NOT implement audit.GroomingWindowAppender,
// so the apply-hook unit tests exercise the fallback; the atomic capability path
// is covered by the pgtest suites.
type groomingApplyAuditFake struct {
	*approvalAuditFake
	// failCategories fails AppendChained for exactly these categories, sparing
	// every other (notably the window watermark), so a test can model a SINK
	// outage that must not abort dispatch while the settlement still lands.
	failCategories map[string]bool
}

func (a *groomingApplyAuditFake) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.failCategories[p.Category] {
		return nil, errors.New("groomingApplyAuditFake: injected append failure for " + p.Category)
	}
	e, err := a.approvalAuditFake.AppendChained(ctx, p)
	if err != nil || e == nil {
		return e, err
	}
	a.mu.Lock()
	// This row was just appended last, so its 1-based position is len(appended).
	e.Sequence = int64(len(a.appended))
	e.Payload, e.Category, e.Timestamp, e.StageID = p.Payload, p.Category, p.Timestamp, p.StageID
	a.mu.Unlock()
	return e, nil
}

func (a *groomingApplyAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for i := range a.appended {
		p := a.appended[i]
		if p.RunID != runID || p.Category != category {
			continue
		}
		rid := p.RunID
		out = append(out, &audit.Entry{
			Sequence: int64(i + 1), RunID: &rid, StageID: p.StageID,
			Category: p.Category, Payload: p.Payload, Timestamp: p.Timestamp,
		})
	}
	return out, nil
}

// seedDisposition appends a grooming_disposition_recorded row DIRECTLY, so an
// apply-hook test can hand the settlement a recorded verdict to consume. Bad/
// good state is seeded by construction rather than by driving the capture verb.
func (f *groomingApplyFixture) seedDisposition(t *testing.T, artifactID, entryID, entryClass, verdict, closeTarget string) {
	t.Helper()
	payload, err := json.Marshal(groomingDispositionPayload{
		RunID: f.stage.RunID.String(), StageID: f.stage.ID.String(),
		ArtifactID: artifactID, EntryID: entryID, EntryClass: entryClass,
		Verdict: verdict, CloseTarget: closeTarget,
	})
	if err != nil {
		t.Fatalf("marshal disposition: %v", err)
	}
	stageID := f.stage.ID
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.stage.RunID, StageID: &stageID, Timestamp: time.Now().UTC(),
		Category: CategoryGroomingDispositionRecorded, Payload: payload,
	}); err != nil {
		t.Fatalf("seed disposition: %v", err)
	}
}

// windowRows returns the grooming_apply_window_closed rows for the run.
func (f *groomingApplyFixture) windowRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.stage.RunID, audit.GroomingApplyWindowClosedCategory)
	if err != nil {
		t.Fatalf("list window rows: %v", err)
	}
	return rows
}

// reportArtifactID returns the id of the seeded grooming_report artifact.
func (f *groomingApplyFixture) reportArtifactID(t *testing.T) string {
	t.Helper()
	arts := f.artifacts.byStage[f.stage.ID]
	if len(arts) == 0 {
		t.Fatal("fixture has no grooming_report artifact")
	}
	return arts[0].ID.String()
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
	// mutatorRefusal makes every dispatch return a provider REFUSAL carrying
	// this reason — the third result state (#2860).
	mutatorRefusal string
}

type groomingApplyFixture struct {
	server    *Server
	runs      *approvalRunRepo
	approvals *groomingApplyApprovalRepo
	audit     *groomingApplyAuditFake
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
	au := &groomingApplyAuditFake{approvalAuditFake: newApprovalAuditFake()}
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

	mut := &groomingApplyMutator{err: opts.mutatorFailure, block: opts.mutatorBlock, refuse: opts.mutatorRefusal}
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

// TestGroomingModesForRun_GatedClassesNeverAuto asserts the delegation boundary
// did not widen (#2991): over this repo's shipped-shape grooming spec, the hook's
// own projection resolves ordering/dedup to gated and scoping to report — NEVER
// auto — so consuming dispositions cannot silently make a destructive class
// self-dispatch. The parse-time refusals
// (spec.TestValidateAutonomy_GroomingNonDelegableClassesRefuseAuto,
// spec.TestShippedGroomingExample_MatrixDefaults) keep `mode: auto` unwritable on
// these classes and stay green and unedited.
func TestGroomingModesForRun_GatedClassesNeverAuto(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	modes := f.server.groomingModesForRun(context.Background(), f.run)

	if modes["ordering"] != workmgmt.GroomingModeGated {
		t.Errorf("ordering mode = %q, want gated", modes["ordering"])
	}
	if modes["dedup"] != workmgmt.GroomingModeGated {
		t.Errorf("dedup mode = %q, want gated", modes["dedup"])
	}
	if modes["scoping"] != workmgmt.GroomingModeReport {
		t.Errorf("scoping mode = %q, want report", modes["scoping"])
	}
	for _, c := range []string{"ordering", "dedup", "scoping"} {
		if modes[c] == workmgmt.GroomingModeAuto {
			t.Errorf("action class %q resolved auto; the delegation boundary has widened", c)
		}
	}
}

// ---------------------------------------------------------------------------
// mergeGroomingDecisions — the merge policy (#2991)
// ---------------------------------------------------------------------------

// decisionByID indexes a decision slice for assertions.
func decisionByID(ds []workmgmt.GroomingDecision) map[string]workmgmt.GroomingDecision {
	out := map[string]workmgmt.GroomingDecision{}
	for _, d := range ds {
		out[d.EntryID] = d
	}
	return out
}

// TestMergeGroomingDecisions_UndispositionedHygieneApprovedNoGate is the base
// layer + the SynthesizedHygieneDoesNotGrantGateApproval control: with NO
// dispositions, the hygiene defect and dependency edge get a synthesized
// `approved` decision, and GateApproved is populated for NEITHER.
//
// COUNTERFACTUAL (hygiene-action-class filter): widen the base layer (drop the
// GroomingActionClassFor == ActionGroomHygiene test, or add the other arrays)
// and the exact-set assertion reddens. COUNTERFACTUAL (explicit-approved-only on
// GateApproved): populate the gate map from a synthesized approval and the
// gate-nil assertion reddens.
func TestMergeGroomingDecisions_UndispositionedHygieneApprovedNoGate(t *testing.T) {
	report, ids := groomingApplyFullReport()

	got, gate := mergeGroomingDecisions(report, nil)
	byID := decisionByID(got)

	want := map[string]bool{ids.hygiene: true, ids.dependency: true}
	if len(got) != len(want) {
		t.Fatalf("decisions = %d (%+v), want exactly %d (hygiene defect + dependency edge)", len(got), got, len(want))
	}
	for id := range want {
		d, ok := byID[id]
		if !ok {
			t.Errorf("hygiene-class entry %q missing a decision", id)
			continue
		}
		if d.Verdict != workmgmt.GroomingApproved {
			t.Errorf("verdict for %q = %q, want approved", id, d.Verdict)
		}
	}
	// A synthesized hygiene approval must NOT grant a per-entry gate approval.
	if len(gate) != 0 {
		t.Errorf("GateApproved = %v, want empty/nil: an undispositioned hygiene default must not unlock a destructive class", gate)
	}
}

// TestMergeGroomingDecisions_UndispositionedGatedEntryGetsNoDecision is the
// COUNTERFACTUAL for the hygiene-action-class filter's exclusion half: an
// undispositioned DEDUP entry is absent from Decisions entirely, so ApplyGrooming
// records it no_decision.
func TestMergeGroomingDecisions_UndispositionedGatedEntryGetsNoDecision(t *testing.T) {
	report, ids := groomingApplyFullReport()
	got, _ := mergeGroomingDecisions(report, nil)
	if _, ok := decisionByID(got)[ids.duplicate]; ok {
		t.Errorf("undispositioned dedup entry %q got a decision; a gated class must reach rule 2 with no decision", ids.duplicate)
	}
}

// TestMergeGroomingDecisions_RecordedApprovedDedupGrantsGate pins that an
// explicit `approved` dedup disposition produces an approved decision WITH
// GateApproved true and the close_target threaded — the destructive unlock.
func TestMergeGroomingDecisions_RecordedApprovedDedupGrantsGate(t *testing.T) {
	report, ids := groomingApplyFullReport()
	got, gate := mergeGroomingDecisions(report, map[string]consumedDisposition{
		ids.duplicate: {Verdict: "approved", CloseTarget: "kuhlman-labs/fishhawk#16"},
	})
	d, ok := decisionByID(got)[ids.duplicate]
	if !ok {
		t.Fatalf("approved dedup disposition produced no decision; got %+v", got)
	}
	if d.Verdict != workmgmt.GroomingApproved || d.CloseTarget != "kuhlman-labs/fishhawk#16" {
		t.Errorf("decision = %+v, want approved with close_target threaded", d)
	}
	if !gate[ids.duplicate] {
		t.Errorf("GateApproved[%q] = false, want true — an explicit approved disposition unlocks the gated class", ids.duplicate)
	}
}

// TestMergeGroomingDecisions_RecordedRejectedHygieneOverrides pins that a
// recorded `rejected` on a hygiene entry OVERRIDES the synthesized approval and
// grants no gate approval.
func TestMergeGroomingDecisions_RecordedRejectedHygieneOverrides(t *testing.T) {
	report, ids := groomingApplyFullReport()
	got, gate := mergeGroomingDecisions(report, map[string]consumedDisposition{
		ids.hygiene: {Verdict: "rejected"},
	})
	d := decisionByID(got)[ids.hygiene]
	if d.Verdict != workmgmt.GroomingRejected {
		t.Errorf("verdict for %q = %q, want rejected (the recorded verdict overrides the synthesized approval)", ids.hygiene, d.Verdict)
	}
	if gate[ids.hygiene] {
		t.Errorf("GateApproved[%q] = true, want false — a rejection grants nothing", ids.hygiene)
	}
}

// TestMergeGroomingDecisions_AmendedDoesNotGrantGateApproval is the
// COUNTERFACTUAL for the explicit-approved-only condition on GateApproved: a
// recorded `amended` becomes an amended decision and grants NO gate approval, so
// amended is not an approval path.
//
// The disposition is on a HYGIENE entry so the byte-identical clean pairing is
// avoided: the base layer would synthesize `approved` for it, so the amended
// overlay must both change the verdict AND leave the gate empty.
func TestMergeGroomingDecisions_AmendedDoesNotGrantGateApproval(t *testing.T) {
	report, ids := groomingApplyFullReport()
	got, gate := mergeGroomingDecisions(report, map[string]consumedDisposition{
		ids.hygiene: {Verdict: "amended"},
	})
	if d := decisionByID(got)[ids.hygiene]; d.Verdict != workmgmt.GroomingAmended {
		t.Errorf("verdict for %q = %q, want amended", ids.hygiene, d.Verdict)
	}
	if len(gate) != 0 {
		t.Errorf("GateApproved = %v, want empty: amended must not grant a gate approval", gate)
	}
}

// TestMergeGroomingDecisions_DropsUnjoinableDisposition is the COUNTERFACTUAL for
// the report-declares-this-entry overlay filter: a disposition naming an entry
// the report does NOT declare is dropped, so it never reaches ApplyGrooming's
// rule-1 join (which would refuse the WHOLE apply on an unjoined id).
func TestMergeGroomingDecisions_DropsUnjoinableDisposition(t *testing.T) {
	report, _ := groomingApplyFullReport()
	unknown := "ordering:github/acme/app#" + uuid.NewString()
	got, gate := mergeGroomingDecisions(report, map[string]consumedDisposition{
		unknown: {Verdict: "approved"},
	})
	if _, ok := decisionByID(got)[unknown]; ok {
		t.Errorf("an unjoinable disposition %q produced a decision; it must be dropped", unknown)
	}
	if gate[unknown] {
		t.Errorf("GateApproved[%q] = true; an unjoinable disposition must grant nothing", unknown)
	}
}

// TestMergeGroomingDecisions_NilReport pins the nil-report guard.
func TestMergeGroomingDecisions_NilReport(t *testing.T) {
	got, gate := mergeGroomingDecisions(nil, map[string]consumedDisposition{"x": {Verdict: "approved"}})
	if got != nil || gate != nil {
		t.Errorf("nil report yielded (%+v, %+v), want (nil, nil)", got, gate)
	}
}

// TestMergeGroomingDecisions_BlankHygieneIDDropped pins the empty-id fail-safe:
// a schema-validated report always carries an id, but a blank one would fail the
// apply layer's join and refuse the WHOLE apply, so it is dropped from the base.
func TestMergeGroomingDecisions_BlankHygieneIDDropped(t *testing.T) {
	report, ids := groomingApplyFullReport()
	report.HygieneDefects[0].ID = "   "
	got, _ := mergeGroomingDecisions(report, nil)
	if _, ok := decisionByID(got)[ids.dependency]; !ok {
		t.Errorf("dependency edge missing; got %+v", got)
	}
	if len(got) != 1 {
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
		t.Errorf("grooming audit rows = %d (%v), want 0 — a reject dispatches nothing and writes no apply/mutation row",
			len(rows), f.groomingAuditCategories())
	}
	// #2991: the reject now SETTLES the window (a grooming_apply_window_closed
	// row), which is not part of the apply/mutation audit family above.
	if rows := f.windowRows(t); len(rows) != 1 {
		t.Errorf("window rows = %d, want 1 — a reject settles the capture window", len(rows))
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

// TestApplyApprovedGrooming_RejectClosesWindowAndAppliesNothing is the
// counterfactual vehicle for the C1 REJECT-PATH SETTLEMENT (#2991): a reject
// applies nothing AND settles the window `rejected`, as decisively as approval
// closes it.
//
// THE STAGE IS RATIFIED BY CONSTRUCTION (one approve row, no rejections), so C3
// would pass — C1 is the only thing standing between this call and both a
// dispatch and an `approved` settlement.
//
// COUNTERFACTUAL: delete the settleGroomingWindow call in C1 and the watermark
// assertion reddens (no window row). Delete the whole C1 return and the
// zero-dispatch assertion reddens (hygiene dispatches on the reject path).
func TestApplyApprovedGrooming_RejectClosesWindowAndAppliesNothing(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionReject)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v on a REJECT; a rejected report must apply nothing", dialed)
	}
	rows := f.windowRows(t)
	if len(rows) != 1 {
		t.Fatalf("window rows = %d, want 1 — a reject must settle the window", len(rows))
	}
	var wp groomingWindowPayload
	if err := json.Unmarshal(rows[0].Payload, &wp); err != nil {
		t.Fatalf("decode window payload: %v", err)
	}
	if wp.Settlement != "rejected" || wp.ArtifactID != f.reportArtifactID(t) {
		t.Errorf("watermark = %+v, want settlement=rejected artifact=%s", wp, f.reportArtifactID(t))
	}
}

// TestApplyApprovedGrooming_RejectSettlementFailureLoggedAppliesNothing exercises
// the reject-path settlement ERROR branch: the watermark append fails, so
// settleGroomingWindow returns an error the hook logs and swallows — nothing is
// dispatched and no watermark lands. (The failCategories map fails ONLY the
// window category, seeding the outage by construction.)
func TestApplyApprovedGrooming_RejectSettlementFailureLoggedAppliesNothing(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.audit.failCategories = map[string]bool{audit.GroomingApplyWindowClosedCategory: true}

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionReject)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("mutator dialed %v on a REJECT with a failed settlement", dialed)
	}
	if rows := f.windowRows(t); len(rows) != 0 {
		t.Errorf("window rows = %d, want 0 — the watermark append failed", len(rows))
	}
}

// TestApplyApprovedGrooming_ApprovedDedupDispatchesWithGate is the merge+ladder
// integration on the fake harness: an explicitly approved DEDUP disposition
// dispatches its close (dedup is gated in the fixture spec), while an
// undispositioned dedup would not.
func TestApplyApprovedGrooming_ApprovedDedupDispatchesWithGate(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)
	// Record an approved dedup disposition against the report artifact, with a
	// close target that is one of the pair (issue #16).
	f.seedDisposition(t, f.reportArtifactID(t), f.ids.duplicate, plan.GroomingClassDuplicate,
		"approved", groomingApplyRepo+"#16")

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	dialed := f.mutator.dialedEntryIDs()
	sawDup := false
	for _, id := range dialed {
		if id == f.ids.duplicate {
			sawDup = true
		}
	}
	if !sawDup {
		t.Errorf("dialed = %v, want it to include the approved dedup entry %q — an explicit approved disposition unlocks the gated class", dialed, f.ids.duplicate)
	}
	// And the window is settled `approved`.
	if rows := f.windowRows(t); len(rows) != 1 {
		t.Errorf("window rows = %d, want 1 (settled on approve)", len(rows))
	}
}

// TestApplyApprovedGrooming_PostRatificationDegradeLeavesWindowClosed pins the
// asymmetry: a POST-ratification degrade (mutator unavailable) leaves the window
// CLOSED — the dispositions were already consumed.
func TestApplyApprovedGrooming_PostRatificationDegradeLeavesWindowClosed(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{mutatorErr: errors.New("mutator down")})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if got := f.degradeReason(t); got != groomingApplyMutatorUnavailable {
		t.Errorf("degrade_reason = %q, want %q", got, groomingApplyMutatorUnavailable)
	}
	if rows := f.windowRows(t); len(rows) != 1 {
		t.Errorf("window rows = %d, want 1 — a post-ratification degrade leaves the window CLOSED", len(rows))
	}
}

// TestApplyApprovedGrooming_PreRatificationDegradeLeavesWindowOpen pins the other
// half: a PRE-ratification degrade (ungranted gate) does NOT close the window —
// nothing was decided, so a legitimate later capture must still be accepted.
func TestApplyApprovedGrooming_PreRatificationDegradeLeavesWindowOpen(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	// No approval rows: C3 fails ungranted BEFORE settlement.

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if got := f.degradeReason(t); got != groomingApplyNotRatified {
		t.Errorf("degrade_reason = %q, want %q", got, groomingApplyNotRatified)
	}
	if rows := f.windowRows(t); len(rows) != 0 {
		t.Errorf("window rows = %d, want 0 — a pre-ratification degrade must NOT close the window", len(rows))
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
	// Fail ONLY the per-mutation sink categories, so the window settlement's
	// watermark append still lands and the apply proceeds — the audit-SINK
	// failure is what must not abort dispatch. A watermark append failure is a
	// different case (settlement can't record the watermark, so nothing is
	// consumed): see TestApplyApprovedGrooming_WindowUnsettledDegrade.
	f.audit.failCategories = map[string]bool{
		workmgmt.GroomingMutationAppliedCategory: true,
		workmgmt.GroomingApplyCompletedCategory:  true,
	}

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 2 {
		t.Errorf("dialed = %v, want 2 — an audit-sink failure must not abort the apply", dialed)
	}
	if rows := f.groomingAudit(); len(rows) != 0 {
		t.Errorf("grooming audit rows = %d, want 0 — the sink was failing throughout", len(rows))
	}
}

// TestApplyApprovedGrooming_WindowUnsettledDegrade pins the POST-RATIFICATION
// settlement-failure branch (#2991): C3 passed but the watermark append fails, so
// nothing is consumed, nothing dispatches, and the degrade names
// grooming_apply_window_unsettled.
//
// COUNTERFACTUAL / defensive branch: the watermark AppendChained fails, so the
// settlement cannot record it. Only the window category is failed, so the
// completed-summary row STILL lands and names the reason — which is what makes the
// degrade-reason assertion below non-vacuous (a blanket outage would leave no row
// to read).
func TestApplyApprovedGrooming_WindowUnsettledDegrade(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)
	// Fail ONLY the watermark append; the settlement cannot record it, but the
	// completed-summary row is writable so the named degrade reason is observable.
	f.audit.failCategories = map[string]bool{
		audit.GroomingApplyWindowClosedCategory: true,
	}

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
		t.Errorf("dialed = %v, want 0 — a failed window settlement must not dispatch", dialed)
	}
	if rows := f.windowRows(t); len(rows) != 0 {
		t.Errorf("window rows = %d, want 0 — the watermark did not land, so the window stays open", len(rows))
	}
	if got := f.degradeReason(t); got != groomingApplyWindowUnsettled {
		t.Errorf("degrade_reason = %q, want %q", got, groomingApplyWindowUnsettled)
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

// groomingApplyTierReport returns the one-of-every-class report with a SECOND
// hygiene defect appended, proposing a delegation tier. Two hygiene entries,
// identical in every way except the label set, is what makes the dispatch
// assertion discriminating: a control that refused both, or neither, fails.
func groomingApplyTierReport() (*plan.GroomingReport, groomingApplyEntryIDs, string) {
	report, ids := groomingApplyFullReport()
	tierRef := groomingApplyRef(19)
	tierID := plan.GroomingEntryID(plan.GroomingClassHygiene, "missing_label_namespace", tierRef)
	report.HygieneDefects = append(report.HygieneDefects, plan.HygieneDefect{
		ID: tierID, ItemRef: tierRef, Defect: "missing_label_namespace",
		Detail:       "no area: label and the tier is unset",
		SuggestedFix: "Please attach the ownership marking and the delegation posture",
		Fix:          &plan.HygieneFix{Labels: []string{"area:backend", "autonomy:low"}},
	})
	return report, ids, tierID
}

// TestApproveGroomStage_DelegationTierLabelNotApplied is the CROSS-BOUNDARY
// end-to-end for containment rule 8 (E54.34 / #2855): a real Postgres
// run/stage/artifact/approval/audit stack, a real approve, the real apply layer,
// and the resulting audit rows fed back through the UNCHANGED
// priorGroomingDispositions.
//
// This is the seam the per-layer units cannot cover. Four things have to agree
// for the refusal to be real: workmgmt's derivation must MARK the entry, the
// server's apply hook must keep passing GateApproved nil, the bare audit payload
// must carry the named skip reason, and the churn guard's decoder must read that
// payload back as a skipped disposition. Each layer's unit pins its own side;
// only this pins that they are the same shape.
//
// It asserts on COMMITTED state — the mutator's dispatch log and the persisted
// audit rows — because the hook returns nothing.
func TestApproveGroomStage_DelegationTierLabelNotApplied(t *testing.T) {
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

	report, ids, tierID := groomingApplyTierReport()
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

	// (a) The provider was NOT dialed for the tier entry — and WAS for the
	// clerical hygiene entry beside it, so this is a refusal and not a
	// whole-apply degrade.
	dialed := mut.dialedEntryIDs()
	for _, id := range dialed {
		if id == tierID {
			t.Fatalf("provider dialed the delegation-tier entry %q; a whole-report approval must not write an autonomy: label", tierID)
		}
	}
	want := map[string]bool{ids.hygiene: true, ids.dependency: true}
	if len(dialed) != len(want) {
		t.Fatalf("dialed = %v, want exactly the clerical hygiene defect and the dependency edge", dialed)
	}
	for _, id := range dialed {
		if !want[id] {
			t.Errorf("provider dialed %q, which is neither the clerical hygiene entry nor the dependency edge", id)
		}
	}

	// (b) The refusal is AUDITED with the named reason, and the proposal stays
	// visible on the row — the operator can still see the tier that was proposed.
	rows, err := auditRepo.ListForRunByCategory(ctx, rn.ID, workmgmt.GroomingMutationAppliedCategory)
	if err != nil {
		t.Fatalf("list mutation rows: %v", err)
	}
	var tierRec *workmgmt.GroomingMutationRecord
	for _, row := range rows {
		var rec workmgmt.GroomingMutationRecord
		if uerr := json.Unmarshal(row.Payload, &rec); uerr != nil {
			t.Fatalf("decode grooming_mutation_applied payload: %v (%s)", uerr, row.Payload)
		}
		if rec.EntryID == tierID {
			cp := rec
			tierRec = &cp
		}
	}
	if tierRec == nil {
		t.Fatalf("no grooming_mutation_applied row for the delegation-tier entry %q; a refusal that is not audited is invisible", tierID)
	}
	if tierRec.Outcome != workmgmt.GroomingOutcomeSkipped ||
		tierRec.SkipReason != workmgmt.GroomingSkipDelegationTierNotAuthorized {
		t.Errorf("tier row = %+v, want skipped/%s", *tierRec, workmgmt.GroomingSkipDelegationTierNotAuthorized)
	}
	if !containsString(tierRec.After.List, "autonomy:low") {
		t.Errorf("tier row After = %+v, want the proposed tier surfaced so the suggestion stays visible", tierRec.After)
	}

	// (c) THE SEAM: the UNCHANGED churn-guard decoder reads that row back into
	// the third state — ABSENT from the baseline (neither applied, nor an
	// already_applied suppression, nor a rejected/amended verdict). Absence is
	// what makes the entry RESURFACE next run for the human to decide, which is
	// the disposition a containment refusal must produce. A decoder that
	// classified it as applied or as already_applied would suppress it forever.
	decisions, appliedResult, derr := s.priorGroomingDispositions(ctx, rn.ID)
	if derr != nil {
		t.Fatalf("priorGroomingDispositions: %v", derr)
	}
	for _, rec := range appliedResult.Applied {
		if rec.EntryID == tierID {
			t.Errorf("the refused tier entry round-tripped as APPLIED into the churn baseline; it would be suppressed instead of resurfacing")
		}
	}
	for _, rec := range appliedResult.Skipped {
		if rec.EntryID == tierID {
			t.Errorf("the refused tier entry round-tripped as an already_applied SUPPRESSION; it must stay absent from the baseline so it resurfaces")
		}
	}
	for _, d := range decisions {
		if d.EntryID == tierID {
			t.Errorf("the refused tier entry round-tripped as a %q verdict; the operator rejected nothing — containment refused it", d.Verdict)
		}
	}
	// The CLERICAL hygiene entry beside it DID round-trip as applied, so the
	// absence above is a per-entry outcome and not a decoder that read nothing.
	var clericalApplied bool
	for _, rec := range appliedResult.Applied {
		if rec.EntryID == ids.hygiene {
			clericalApplied = true
		}
	}
	if !clericalApplied {
		t.Errorf("the clerical hygiene entry %q did not round-trip as applied; the tier entry's absence proves nothing if the decoder read nothing", ids.hygiene)
	}
}

// ---------------------------------------------------------------------------
// The refused outcome, through the SHIPPED serialization (#2860)
// ---------------------------------------------------------------------------

// TestApplyApprovedGrooming_RefusedIDsReachTheCompletedPayload is operator
// approval condition 2, and it asserts the RENDERED JSON rather than the Go
// struct — a fact no compiler enforces.
//
// A COUNT proves bucketing happened; it does not prove the entry ID survived
// into the payload an operator actually reads. Not naming WHICH edges were
// refused is precisely why an 0/8 apply rate went unnoticed across three
// grooming walks, so the assertion is on `refused_ids` in the serialized
// grooming_apply_completed row, decoded through the SAME bare decode
// priorGroomingDispositions uses.
func TestApplyApprovedGrooming_RefusedIDsReachTheCompletedPayload(t *testing.T) {
	f := newGroomingApplyFixture(t, groomingApplyOpts{mutatorRefusal: "not on board"})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	rows := f.rowsOfCategory(workmgmt.GroomingApplyCompletedCategory)
	if len(rows) != 1 {
		t.Fatalf("grooming_apply_completed rows = %d, want exactly 1", len(rows))
	}
	// The RAW bytes must carry both keys — a struct-only assertion would still
	// pass if a json tag were renamed or dropped.
	raw := string(rows[0].Payload)
	for _, key := range []string{`"refused"`, `"refused_ids"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("serialized payload is missing %s: %s", key, raw)
		}
	}
	var summary workmgmt.GroomingApplySummary
	if err := json.Unmarshal(rows[0].Payload, &summary); err != nil {
		t.Fatalf("decode grooming_apply_completed payload: %v (%s)", err, raw)
	}
	if summary.Refused == 0 {
		t.Fatalf("summary refused = 0, want the dispatched candidates counted: %s", raw)
	}
	if summary.Applied != 0 {
		t.Errorf("summary applied = %d, want 0 — every dispatch was refused: %s", summary.Applied, raw)
	}
	// The IDS, not just the count: every dispatched entry is NAMED.
	if len(summary.RefusedIDs) != summary.Refused {
		t.Errorf("refused_ids = %v but refused = %d — the count and the ids disagree", summary.RefusedIDs, summary.Refused)
	}
	named := map[string]bool{}
	for _, id := range summary.RefusedIDs {
		named[id] = true
	}
	for _, id := range f.mutator.dialedEntryIDs() {
		if !named[id] {
			t.Errorf("entry %q was dispatched and refused but is absent from refused_ids %v", id, summary.RefusedIDs)
		}
	}

	// The PER-MUTATION row carries the refusal as its own audit fact, with an
	// EMPTY skip_reason so a reader cannot mistake it for an idempotent no-op.
	recs := f.mutationRecords(t)
	for _, id := range f.mutator.dialedEntryIDs() {
		rec, ok := recs[id]
		if !ok {
			t.Fatalf("no grooming_mutation_applied row for refused entry %q", id)
		}
		if rec.Outcome != workmgmt.GroomingOutcomeRefused {
			t.Errorf("entry %q outcome = %q, want %q", id, rec.Outcome, workmgmt.GroomingOutcomeRefused)
		}
		if rec.RefuseReason != "not on board" {
			t.Errorf("entry %q refuse_reason = %q, want %q", id, rec.RefuseReason, "not on board")
		}
		if rec.SkipReason != "" {
			t.Errorf("entry %q skip_reason = %q, want empty — a refusal is not a skip", id, rec.SkipReason)
		}
	}
}

// TestApplyApprovedGrooming_DegradePayloadCarriesRefusedZero pins the DEGRADE
// half of the same key. groomingApplyDegradePayload is a strict superset of
// GroomingApplySummary's count fields so ONE category filter returns both
// shapes; `refused` must therefore serialize on a degrade too, as its zero
// value — which is the correct fact, since nothing was dispatched.
func TestApplyApprovedGrooming_DegradePayloadCarriesRefusedZero(t *testing.T) {
	// An unparseable report degrades before anything is dispatched. (A MISSING
	// artifact is silent by design — it means "not a grooming stage" — and
	// writes no row at all.)
	f := newGroomingApplyFixture(t, groomingApplyOpts{badReport: true})
	f.seedApproval(t, "kuhlman-labs", approval.DecisionApprove)

	f.server.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	rows := f.rowsOfCategory(workmgmt.GroomingApplyCompletedCategory)
	if len(rows) != 1 {
		t.Fatalf("grooming_apply_completed rows = %d, want exactly 1", len(rows))
	}
	raw := string(rows[0].Payload)
	if !strings.Contains(raw, `"refused":0`) {
		t.Errorf("degrade payload does not carry `\"refused\":0`: %s", raw)
	}
	var got groomingApplyDegradePayload
	if err := json.Unmarshal(rows[0].Payload, &got); err != nil {
		t.Fatalf("decode degrade payload: %v (%s)", err, raw)
	}
	if !got.Degraded {
		t.Errorf("payload = %s, want degraded:true", raw)
	}
	if got.Refused != 0 {
		t.Errorf("degrade payload refused = %d, want 0 — nothing was dispatched", got.Refused)
	}
	// One filter, both shapes: the summary decode must also succeed on this row.
	var summary workmgmt.GroomingApplySummary
	if err := json.Unmarshal(rows[0].Payload, &summary); err != nil {
		t.Fatalf("the degrade row is not decodable as a summary: %v (%s)", err, raw)
	}
	if summary.Refused != 0 {
		t.Errorf("summary-decoded refused = %d, want 0", summary.Refused)
	}
}

// TestCollapseGroomingConsumed_SkipsAndLastWins covers the defensive branches of
// the pure collapse: a nil entry, a malformed/empty-entry-id payload, and the
// last-wins ordering when a lower-sequence row arrives AFTER a higher one.
func TestCollapseGroomingConsumed_SkipsAndLastWins(t *testing.T) {
	mk := func(entryID, verdict string, seq int64) *audit.Entry {
		p, _ := json.Marshal(groomingDispositionPayload{EntryID: entryID, Verdict: verdict})
		return &audit.Entry{Sequence: seq, Payload: p}
	}
	// Highest sequence first, then an older (lower-sequence) row for the same id:
	// the older one must NOT overwrite the newer verdict (the cur.s > e.Sequence skip).
	high := mk("e1", "approved", 5)
	low := mk("e1", "rejected", 2)
	bad := &audit.Entry{Sequence: 3, Payload: []byte(`{"entry_id":""}`)} // empty id -> skipped
	got := collapseGroomingConsumed([]*audit.Entry{nil, high, low, bad})

	if len(got) != 1 {
		t.Fatalf("collapsed = %v, want exactly one entry (e1)", got)
	}
	if got["e1"].Verdict != "approved" {
		t.Errorf("e1 verdict = %q, want approved (last-wins keeps the higher sequence)", got["e1"].Verdict)
	}
}
