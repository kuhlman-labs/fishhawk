package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// --- fixtures for the grooming-source campaign path (E54.6 / #2238) ---
//
// These fakes are LOCAL to this file deliberately: the package's existing
// fakeApprovalRepo / fakeArtifactRepo live in unscoped test files, and the
// grooming ladder needs COUNTING behaviour those do not provide. Nothing shared
// is extended.

// groomingSourceRunRepo serves the run rows and stage lists the grooming ladder and
// the supersession scan read. It counts calls so the read-once boundary (AC3)
// can be asserted rather than assumed.
type groomingSourceRunRepo struct {
	run.BaseFake
	mu sync.Mutex

	runs   map[uuid.UUID]*run.Run
	stages map[uuid.UUID][]*run.Stage
	// listRuns, when set, overrides the default paging response so the
	// supersession scan's page-cap and error branches are reachable.
	listRuns    func(f run.ListRunsFilter) ([]*run.Run, error)
	getRunErr   error
	stagesErr   error
	stageCalls  int
	listRunCall int
}

func (f *groomingSourceRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getRunErr != nil {
		return nil, f.getRunErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

func (f *groomingSourceRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageCalls++
	if f.stagesErr != nil {
		return nil, f.stagesErr
	}
	return f.stages[id], nil
}

func (f *groomingSourceRunRepo) ListRuns(_ context.Context, filter run.ListRunsFilter) ([]*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listRunCall++
	if f.listRuns != nil {
		return f.listRuns(filter)
	}
	var out []*run.Run
	for _, r := range f.runs {
		if r.Repo == filter.Repo && r.WorkflowID == filter.WorkflowID && r.AccountID == filter.AccountID {
			out = append(out, r)
		}
	}
	// One short page: the scan can prove it reached the end of the list.
	if filter.Offset > 0 {
		return nil, nil
	}
	return out, nil
}

// groomingSourceArtifactRepo serves stage artifacts and COUNTS the grooming-report
// reads, which is what makes the read-once assertion behavioural.
type groomingSourceArtifactRepo struct {
	mu           sync.Mutex
	byStage      map[uuid.UUID][]*artifact.Artifact
	err          error
	reportsRead  int
	listForStage int
}

func (f *groomingSourceArtifactRepo) Create(context.Context, artifact.CreateParams) (*artifact.Artifact, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *groomingSourceArtifactRepo) Get(context.Context, uuid.UUID) (*artifact.Artifact, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *groomingSourceArtifactRepo) GetByHash(context.Context, uuid.UUID, string) (*artifact.Artifact, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *groomingSourceArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listForStage++
	if f.err != nil {
		return nil, f.err
	}
	arts := f.byStage[stageID]
	for _, a := range arts {
		if a.Kind == artifact.KindGroomingReport {
			f.reportsRead++
		}
	}
	return arts, nil
}

// groomingSourceApprovalRepo serves the gate decisions the ratification check reads.
type groomingSourceApprovalRepo struct {
	mu      sync.Mutex
	byStage map[uuid.UUID][]*approval.Approval
	err     error
	calls   int
}

func (f *groomingSourceApprovalRepo) Submit(context.Context, approval.SubmitParams) (*approval.SubmitResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *groomingSourceApprovalRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*approval.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byStage[stageID], nil
}

// groomingSourceReportJSON builds a schema-valid grooming_report whose Ordering ranks
// the given issue numbers in the order they are passed — so a caller can hand
// ranks that are the REVERSE of numeric order.
func groomingSourceReportJSON(t *testing.T, owner, name string, rankedNumbers ...int) []byte {
	t.Helper()
	type itemRef struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		URL  string `json:"url"`
	}
	type citation struct {
		RubricID string `json:"rubric_id"`
	}
	type entry struct {
		ID              string     `json:"id"`
		ItemRef         itemRef    `json:"item_ref"`
		Rank            int        `json:"rank"`
		Score           float64    `json:"score"`
		RubricCitations []citation `json:"rubric_citations"`
	}
	ordering := make([]entry, 0, len(rankedNumbers))
	for i, n := range rankedNumbers {
		id := fmt.Sprintf("%s/%s#%d", owner, name, n)
		ordering = append(ordering, entry{
			// The entry id is DERIVED, not minted: the validator recomposes it
			// through plan.GroomingEntryID and rejects a report whose ids do not
			// round-trip.
			ID:              plan.GroomingEntryID(plan.GroomingClassOrdering, "", plan.ItemRef{Type: "github_issue", ID: id}),
			ItemRef:         itemRef{Type: "github_issue", ID: id, URL: "https://github.test/" + id},
			Rank:            i + 1,
			Score:           float64(100 - i),
			RubricCitations: []citation{{RubricID: "V1"}},
		})
	}
	doc := map[string]any{
		"kind":           "grooming_report",
		"report_version": "grooming_report_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"id":   owner + "/" + name + "#2238",
			"url":  "https://github.test/" + owner + "/" + name + "/issues/2238",
		},
		"generated_by": map[string]any{
			"agent": "test", "model": "test-model", "timestamp": "2026-08-22T00:00:00Z",
		},
		"summary":  "test ordering",
		"ordering": ordering,
		// Every entry array is REQUIRED by the schema even when empty.
		"duplicates": []any{}, "hygiene_defects": []any{}, "dependency_edges": []any{},
		"vision_drift": []any{}, "decomposition_suggestions": []any{},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal grooming report: %v", err)
	}
	return b
}

// groomingSourceFixture wires a server whose grooming run ships an APPROVED report
// ranking the given issue numbers in the given order.
type groomingSourceFixture struct {
	server    *Server
	campaigns *fakeCampaignRepo
	runs      *groomingSourceRunRepo
	artifacts *groomingSourceArtifactRepo
	approvals *groomingSourceApprovalRepo
	// audit captures the GLOBAL-chain appends so the
	// campaign_grooming_source_resolved EMISSION is asserted behaviourally
	// rather than inferred from its category registration. It reuses this
	// package's campaignAuditRecorder (campaigns_test.go) — nothing shared is
	// extended, and every fixture wires it, so deleting the emit reddens the
	// audit tests instead of leaving them green.
	audit    *campaignAuditRecorder
	provider *fakeIssueSetProvider
	runID    uuid.UUID
	stageID  uuid.UUID
	reportID uuid.UUID
}

type groomingSourceOpts struct {
	approvals []approval.Decision
	// omitReport ships a run with a plan stage but no grooming_report artifact.
	omitReport bool
	// badReport ships an unparseable report artifact.
	badReport bool
	// repo overrides the grooming run's repo (for the mismatch case).
	repo string
	// accountID sets the grooming run's tenant account. Empty means "the
	// caller's own account" (the ordinary same-tenant fixture); use
	// untenantedRun to build a run with NO account at all.
	accountID string
	// untenantedRun ships a source run whose AccountID is EMPTY, the case a
	// NULL-allow tenancy check would wave through for any authenticated caller.
	untenantedRun bool
	// children is the resolver result; nil builds one from rankedNumbers.
	children *workmgmt.EpicChildrenResult
}

func newGroomingSourceFixture(t *testing.T, opts groomingSourceOpts, rankedNumbers ...int) *groomingSourceFixture {
	t.Helper()
	const owner, name = "kuhlman-labs", "fishhawk"
	repoFull := owner + "/" + name

	f := &groomingSourceFixture{
		audit:     &campaignAuditRecorder{},
		campaigns: newFakeCampaignRepo(),
		runs:      &groomingSourceRunRepo{runs: map[uuid.UUID]*run.Run{}, stages: map[uuid.UUID][]*run.Stage{}},
		artifacts: &groomingSourceArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{}},
		approvals: &groomingSourceApprovalRepo{byStage: map[uuid.UUID][]*approval.Approval{}},
		runID:     uuid.New(), stageID: uuid.New(), reportID: uuid.New(),
	}

	runRepo := opts.repo
	if runRepo == "" {
		runRepo = repoFull
	}
	// The default source run belongs to the CALLER's account: the tenancy match
	// is exact, so an untenanted run is not visible to the tenanted test
	// operator (see TestCreateCampaign_GroomingSource_UntenantedRunNotFound).
	runAccount := opts.accountID
	if runAccount == "" && !opts.untenantedRun {
		runAccount = testOperatorAccountID
	}
	if opts.untenantedRun {
		runAccount = ""
	}
	f.runs.runs[f.runID] = &run.Run{
		ID: f.runID, Repo: runRepo, WorkflowID: "backlog_grooming",
		AccountID: runAccount, CreatedAt: time.Now().Add(-time.Hour),
	}
	f.runs.stages[f.runID] = []*run.Stage{{ID: f.stageID, RunID: f.runID, Type: run.StageTypePlan}}
	if !opts.omitReport {
		content := groomingSourceReportJSON(t, owner, name, rankedNumbers...)
		if opts.badReport {
			content = []byte(`{"kind":"grooming_report","report_version":"nope"}`)
		}
		f.artifacts.byStage[f.stageID] = []*artifact.Artifact{{
			ID: f.reportID, StageID: f.stageID, Kind: artifact.KindGroomingReport,
			Content: content, ContentHash: "sha256:test-report-hash", CreatedAt: time.Now(),
		}}
	}
	for i, d := range opts.approvals {
		f.approvals.byStage[f.stageID] = append(f.approvals.byStage[f.stageID], &approval.Approval{
			ID: uuid.New(), StageID: f.stageID, Decision: d,
			ApproverSubject: fmt.Sprintf("reviewer-%d", i),
		})
	}

	children := opts.children
	if children == nil {
		children = &workmgmt.EpicChildrenResult{}
		// The provider returns children in ASCENDING numeric order — the real
		// GitHub behaviour — so a handler that skipped the permutation would
		// persist ascending order and fail the rank assertions.
		sorted := append([]int(nil), rankedNumbers...)
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j] < sorted[i] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		for _, n := range sorted {
			children.Children = append(children.Children, workmgmt.EpicChild{Number: n})
		}
	}
	f.provider = &fakeIssueSetProvider{result: children}
	registerIssueSetProvider(t, f.provider)

	f.server = New(Config{
		CampaignRepo: f.campaigns, RunRepo: f.runs,
		ArtifactRepo: f.artifacts, ApprovalRepo: f.approvals,
		AuditRepo: f.audit,
	})
	return f
}

// groomingSourceBody renders a create body naming this fixture's grooming run.
func groomingSourceBody(runID uuid.UUID, extra string) string {
	return fmt.Sprintf(`{"repo":"kuhlman-labs/fishhawk","grooming_source":{"run_id":%q%s}}`, runID.String(), extra)
}

func decodeGroomingErr(t *testing.T, body []byte) (string, map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope %s: %v", body, err)
	}
	return env.Error.Code, env.Error.Details
}

// TestCreateCampaign_GroomingSource_EndToEnd is the DONE-MEANS behavioural
// assertion for AC1 + AC5, crossing every layer the change spans: request
// payload -> run/artifact/approval reads -> provider seam -> assembly ->
// PERSISTED ITEM ORDER -> response -> audit.
//
// The report ranks the issues in the REVERSE of their numeric order and the
// provider returns them ASCENDING, so an implementation that merely handed the
// issue set to the existing no-epic path FAILS here. It asserts COMMITTED state
// (the persisted items read back), because a scrambled order still returns 201.
func TestCreateCampaign_GroomingSource_EndToEnd(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 303, 101, 202)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}

	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// (a) THE QUEUE IS IN RATIFIED RANK ORDER — committed state, not the response.
	items, err := f.campaigns.ListCampaignItemsForCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var gotRefs []string
	for i, it := range items {
		gotRefs = append(gotRefs, it.IssueRef)
		if it.Position != i {
			t.Errorf("item %s Position = %d, want %d", it.IssueRef, it.Position, i)
		}
	}
	want := []string{"issue:303", "issue:101", "issue:202"}
	if fmt.Sprint(gotRefs) != fmt.Sprint(want) {
		t.Fatalf("persisted queue order = %v, want %v (ratified rank order, NOT ascending issue number)", gotRefs, want)
	}

	// (b) THE PROVENANCE IS DURABLE — read off the persisted campaign row, not
	// merely echoed in the response.
	stored, err := f.campaigns.GetCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if len(stored.GroomingSource) == 0 {
		t.Fatal("the persisted campaign row carries NO grooming_source: a created campaign must never be unprovenanced (K3)")
	}
	var prov campaignGroomingSourcePayload
	if err := json.Unmarshal(stored.GroomingSource, &prov); err != nil {
		t.Fatalf("decode persisted provenance: %v", err)
	}
	if prov.SourceRunID != f.runID || prov.SourceStageID != f.stageID || prov.ReportArtifactID != f.reportID {
		t.Errorf("persisted provenance ids = %+v, want run=%s stage=%s artifact=%s", prov, f.runID, f.stageID, f.reportID)
	}
	if prov.ReportContentHash != "sha256:test-report-hash" {
		t.Errorf("persisted report_content_hash = %q, want the artifact's hash", prov.ReportContentHash)
	}
	if fmt.Sprint(prov.OrderedRefs) != fmt.Sprint(want) {
		t.Errorf("persisted ordered_refs = %v, want %v", prov.OrderedRefs, want)
	}

	// (c) The 201 response names the same source.
	if len(created.GroomingSource) == 0 {
		t.Fatal("the 201 response carries no grooming_source block")
	}
	var respProv campaignGroomingSourcePayload
	if err := json.Unmarshal(created.GroomingSource, &respProv); err != nil {
		t.Fatalf("decode response provenance: %v", err)
	}
	if respProv.SourceRunID != prov.SourceRunID || respProv.ReportContentHash != prov.ReportContentHash {
		t.Errorf("response provenance %+v disagrees with the persisted row %+v", respProv, prov)
	}

	// (d) THE AUDIT LAYER FIRED. The chain's second copy of the provenance is
	// best-effort by design, but it IS shipped behaviour (an operator can
	// fishhawk_await_audit on the category), so the emission is asserted rather
	// than inferred from the category registration: exactly one entry, naming
	// the campaign AND carrying the same identifiers the row does.
	entries := f.audit.entriesFor(auditCampaignGroomingSourceResolved)
	if len(entries) != 1 {
		t.Fatalf("%s audit entries = %d, want exactly 1", auditCampaignGroomingSourceResolved, len(entries))
	}
	aud := decodeGroomingAudit(t, entries[0])
	if aud.CampaignID != created.ID.String() {
		t.Errorf("audit campaign_id = %q, want %s", aud.CampaignID, created.ID)
	}
	if aud.Repo != created.Repo {
		t.Errorf("audit repo = %q, want %q", aud.Repo, created.Repo)
	}
	if aud.GroomingSource.SourceRunID != f.runID || aud.GroomingSource.SourceStageID != f.stageID ||
		aud.GroomingSource.ReportArtifactID != f.reportID {
		t.Errorf("audit provenance ids = %+v, want run=%s stage=%s artifact=%s",
			aud.GroomingSource, f.runID, f.stageID, f.reportID)
	}
	if aud.GroomingSource.ReportContentHash != "sha256:test-report-hash" {
		t.Errorf("audit report_content_hash = %q, want the artifact's hash", aud.GroomingSource.ReportContentHash)
	}
	if fmt.Sprint(aud.GroomingSource.OrderedRefs) != fmt.Sprint(want) || aud.GroomingSource.OrderedCount != len(want) {
		t.Errorf("audit ordered_refs = %v (count %d), want %v", aud.GroomingSource.OrderedRefs, aud.GroomingSource.OrderedCount, want)
	}
}

// groomingAuditEntry is the decoded campaign_grooming_source_resolved payload:
// the campaign it names plus the SAME provenance type the row and the response
// carry, so a drift between the three cannot pass unnoticed.
type groomingAuditEntry struct {
	CampaignID     string                        `json:"campaign_id"`
	Repo           string                        `json:"repo"`
	GroomingSource campaignGroomingSourcePayload `json:"grooming_source"`
}

func decodeGroomingAudit(t *testing.T, p audit.GlobalChainAppendParams) groomingAuditEntry {
	t.Helper()
	var out groomingAuditEntry
	if err := json.Unmarshal(p.Payload, &out); err != nil {
		t.Fatalf("decode %s payload %s: %v", p.Category, p.Payload, err)
	}
	return out
}

// entriesFor returns every recorded append in the given category, so a test can
// assert the ENTRY (not merely a count).
func (a *campaignAuditRecorder) entriesFor(category string) []audit.GlobalChainAppendParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.GlobalChainAppendParams
	for _, e := range a.entries {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out
}

// TestCreateCampaign_GroomingSource_AuditCarriesTheFullProvenance covers the
// audit payload fields the happy path cannot reach: the applied limit and its
// omitted count, a NAMED exclusion, and an acknowledged supersession. Together
// with the end-to-end test's ids/hash/ordering assertions, every field of the
// emitted entry is pinned — and deleting the emitCampaignAudit call reddens both.
func TestCreateCampaign_GroomingSource_AuditCarriesTheFullProvenance(t *testing.T) {
	// Rank 1 is an issue in ANOTHER repo (an exclusion), then three in-repo
	// issues of which the limit takes two — so one convertible entry is dropped
	// by the cap and one entry is excluded, and the two counts stay distinct.
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		children:  &workmgmt.EpicChildrenResult{Children: []workmgmt.EpicChild{{Number: 101}, {Number: 303}}},
	}, 303, 101, 202)
	spliceForeignOrderingEntry(t, f, "other/repo#77")
	newerID := f.seedNewerApprovedGroomingRun(t, time.Now())

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, `,"limit":2,"allow_superseded":true`))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	entries := f.audit.entriesFor(auditCampaignGroomingSourceResolved)
	if len(entries) != 1 {
		t.Fatalf("%s audit entries = %d, want exactly 1", auditCampaignGroomingSourceResolved, len(entries))
	}
	aud := decodeGroomingAudit(t, entries[0])
	if aud.CampaignID != created.ID.String() {
		t.Fatalf("audit campaign_id = %q, want %s", aud.CampaignID, created.ID)
	}
	gsrc := aud.GroomingSource
	if gsrc.Limit != 2 || gsrc.OmittedByLimit != 1 {
		t.Errorf("audit limit=%d omitted_by_limit=%d, want 2 and 1", gsrc.Limit, gsrc.OmittedByLimit)
	}
	if len(gsrc.Excluded) != 1 || gsrc.Excluded[0].Ref != "other/repo#77" ||
		gsrc.Excluded[0].Reason != campaign.ExclusionOtherRepo {
		t.Errorf("audit excluded = %+v, want exactly one other_repo exclusion naming other/repo#77", gsrc.Excluded)
	}
	if gsrc.SupersededBy == nil || *gsrc.SupersededBy != newerID {
		t.Errorf("audit superseded_by = %v, want %s — the acknowledged supersession rides the chain copy too", gsrc.SupersededBy, newerID)
	}
	// The chain copy agrees with the system of record.
	stored, err := f.campaigns.GetCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	var row campaignGroomingSourcePayload
	if err := json.Unmarshal(stored.GroomingSource, &row); err != nil {
		t.Fatalf("decode persisted provenance: %v", err)
	}
	if fmt.Sprint(row) != fmt.Sprint(gsrc) {
		t.Fatalf("audit provenance %+v disagrees with the persisted row %+v", gsrc, row)
	}
}

// TestCreateCampaign_GroomingSource_UntenantedRunNotFound is the dedicated
// counterfactual vehicle for the EXACT tenancy match. The source run carries NO
// account — the case a NULL-allow check waves through for any authenticated
// caller — and the run id here is CALLER-SUPPLIED, so allowing it would let any
// tenant consume an untenanted run's ratified order. It must be reported
// identically to a missing run, and nothing may be persisted from it.
func TestCreateCampaign_GroomingSource_UntenantedRunNotFound(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals:     []approval.Decision{approval.DecisionApprove},
		untenantedRun: true,
	}, 20, 10)
	if got := f.runs.runs[f.runID].AccountID; got != "" {
		t.Fatalf("fixture source run AccountID = %q, want empty — this test needs an UNTENANTED run", got)
	}
	if testOperatorAccountID == "" {
		t.Fatal("the test operator identity carries no account; this case would be a same-account match, not a cross-tenant one")
	}

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
	if code, _ := decodeGroomingErr(t, w.Body.Bytes()); code != codeGroomingRunNotFound {
		t.Fatalf("code = %q, want %q — an UNTENANTED run must be indistinguishable from a missing one",
			code, codeGroomingRunNotFound)
	}
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted from an untenanted run's order, want 0", n)
	}
	if f.provider.resolveCalled {
		t.Error("the provider was dialed for a run outside the caller's account")
	}
}

// TestCreateCampaign_GroomingSource_UndeterminedIsNotAcknowledgeable is K2's
// second half and the counterfactual vehicle for the UNCONDITIONAL undetermined
// refusal: allow_superseded acknowledges a NAMED newer run, and an incomplete
// scan names none. A flag the caller controls must not stand in for a check that
// never finished, so the create is refused even WITH the acknowledgement.
func TestCreateCampaign_GroomingSource_UndeterminedIsNotAcknowledgeable(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
	// Every page comes back FULL, so the scan never reaches the end of the list
	// within its page budget. Seeded BY CONSTRUCTION — nothing here calls the
	// control — so the RED lands on the behavioural assertion.
	f.runs.listRuns = func(run.ListRunsFilter) ([]*run.Run, error) {
		out := make([]*run.Run, groomingSupersessionPageSize)
		for i := range out {
			out[i] = &run.Run{ID: uuid.New(), CreatedAt: time.Now().Add(-2 * time.Hour)}
		}
		return out, nil
	}

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, `,"allow_superseded":true`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s) — allow_superseded must NOT bypass an undetermined scan", w.Code, w.Body.String())
	}
	if code, _ := decodeGroomingErr(t, w.Body.Bytes()); code != codeGroomingSupersessionUndetermined {
		t.Fatalf("code = %q, want %q", code, codeGroomingSupersessionUndetermined)
	}
	// COMMITTED STATE: the refusal is what matters, and a bypassed control
	// returns a 201 with a campaign behind it.
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted from an order whose currency was never established, want 0", n)
	}
	if n := len(f.audit.entriesFor(auditCampaignGroomingSourceResolved)); n != 0 {
		t.Fatalf("%d grooming-source audit entries behind the refusal, want 0", n)
	}
}

// spliceForeignOrderingEntry appends an ordering entry for an issue in ANOTHER
// repo, ranked last, so the derived order carries exactly one NAMED exclusion.
func spliceForeignOrderingEntry(t *testing.T, f *groomingSourceFixture, ref string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(f.artifacts.byStage[f.stageID][0].Content, &doc); err != nil {
		t.Fatalf("decode fixture report: %v", err)
	}
	ordering := doc["ordering"].([]any)
	ordering = append(ordering, map[string]any{
		"id":    plan.GroomingEntryID(plan.GroomingClassOrdering, "", plan.ItemRef{Type: "github_issue", ID: ref}),
		"rank":  len(ordering) + 1,
		"score": 1.0,
		"item_ref": map[string]any{
			"type": "github_issue", "id": ref, "url": "https://github.test/" + ref,
		},
		"rubric_citations": []any{map[string]any{"rubric_id": "V1"}},
	})
	doc["ordering"] = ordering
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f.artifacts.byStage[f.stageID][0].Content = raw
}

// TestCreateCampaign_GroomingSource_ReadOnce pins AC3 behaviourally: exactly
// ONE grooming-report artifact read and ONE approval list per create, and ZERO
// further reads across a subsequent status read.
func TestCreateCampaign_GroomingSource_ReadOnce(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	reportsAfterCreate := f.artifacts.reportsRead
	approvalsAfterCreate := f.approvals.calls
	if reportsAfterCreate != 1 {
		t.Fatalf("grooming-report reads during create = %d, want exactly 1", reportsAfterCreate)
	}
	if approvalsAfterCreate != 1 {
		t.Fatalf("approval lists during create = %d, want exactly 1", approvalsAfterCreate)
	}

	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A full status read drives reconcile + the engine partition. Neither may
	// re-read the order.
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+created.ID.String()+"/status", nil)
	req.SetPathValue("campaign_id", created.ID.String())
	rec := httptest.NewRecorder()
	f.server.handleGetCampaignStatus(rec, withAuth(req))

	if f.artifacts.reportsRead != reportsAfterCreate {
		t.Errorf("grooming-report reads went %d -> %d across a status read; the ratified order is read EXACTLY ONCE, at assembly (AC3)",
			reportsAfterCreate, f.artifacts.reportsRead)
	}
	if f.approvals.calls != approvalsAfterCreate {
		t.Errorf("approval lists went %d -> %d across a status read", approvalsAfterCreate, f.approvals.calls)
	}
}

// TestCreateCampaign_GroomingSource_Refusals is the PER-FAILURE-MODE table:
// one case per named refusal branch, each asserting the OBSERVABLE output
// (status + code) AND that ZERO campaigns were persisted — so a control that
// fired and was then bypassed cannot pass on the error identity alone.
func TestCreateCampaign_GroomingSource_Refusals(t *testing.T) {
	tests := []struct {
		name       string
		opts       groomingSourceOpts
		body       func(f *groomingSourceFixture) string
		mutate     func(f *groomingSourceFixture)
		wantStatus int
		wantCode   string
	}{
		{
			name: "run id is not a UUID",
			opts: groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}},
			body: func(*groomingSourceFixture) string {
				return `{"repo":"kuhlman-labs/fishhawk","grooming_source":{"run_id":"not-a-uuid"}}`
			},
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			name:       "unknown run",
			opts:       groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}},
			body:       func(*groomingSourceFixture) string { return groomingSourceBody(uuid.New(), "") },
			wantStatus: http.StatusNotFound, wantCode: codeGroomingRunNotFound,
		},
		{
			// A run in ANOTHER account must be INDISTINGUISHABLE from a missing
			// one — same status, same code — or the id space is an oracle.
			name:       "cross-tenant run is indistinguishable from missing",
			opts:       groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}, accountID: uuid.NewString()},
			wantStatus: http.StatusNotFound, wantCode: codeGroomingRunNotFound,
		},
		{
			name:       "grooming run groomed a different repo",
			opts:       groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}, repo: "other/repo"},
			wantStatus: http.StatusUnprocessableEntity, wantCode: codeGroomingRepoMismatch,
		},
		{
			name:       "run shipped no grooming report",
			opts:       groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}, omitReport: true},
			wantStatus: http.StatusUnprocessableEntity, wantCode: codeGroomingOrderAbsent,
		},
		{
			name:       "report was never approved",
			opts:       groomingSourceOpts{},
			wantStatus: http.StatusUnprocessableEntity, wantCode: codeGroomingNotApproved,
		},
		{
			// A rejection ALONGSIDE an approval is a CONTESTED gate, not a
			// ratified one. A guard that only counted approvals would pass this.
			name:       "a rejection alongside an approval is not ratified",
			opts:       groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove, approval.DecisionReject}},
			wantStatus: http.StatusUnprocessableEntity, wantCode: codeGroomingNotApproved,
		},
		{
			name:       "report does not parse",
			opts:       groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}, badReport: true},
			wantStatus: http.StatusUnprocessableEntity, wantCode: codeGroomingOrderInvalid,
		},
		{
			name: "stage list unreadable",
			opts: groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}},
			mutate: func(f *groomingSourceFixture) {
				f.runs.stagesErr = errInjected
			},
			wantStatus: http.StatusBadGateway, wantCode: codeGroomingSupersessionUnreadable,
		},
		{
			name: "approval list unreadable",
			opts: groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}},
			mutate: func(f *groomingSourceFixture) {
				f.approvals.err = errInjected
			},
			wantStatus: http.StatusBadGateway, wantCode: codeGroomingSupersessionUnreadable,
		},
		{
			// K2: the scan cannot be RUN. allow_superseded deliberately does NOT
			// bypass this — nobody can acknowledge a read that did not happen.
			name: "supersession scan cannot read its candidates",
			opts: groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}},
			mutate: func(f *groomingSourceFixture) {
				f.runs.listRuns = func(run.ListRunsFilter) ([]*run.Run, error) { return nil, errInjected }
			},
			body: func(f *groomingSourceFixture) string {
				return groomingSourceBody(f.runID, `,"allow_superseded":true`)
			},
			wantStatus: http.StatusBadGateway, wantCode: codeGroomingSupersessionUnreadable,
		},
		{
			// K2: the bounded scan could not PROVE absence. Reporting "the scan
			// was capped" on a successful create would relabel the silence, so it
			// REFUSES — unconditionally; see
			// TestCreateCampaign_GroomingSource_UndeterminedIsNotAcknowledgeable
			// for the allow_superseded half.
			name: "supersession scan cannot prove absence",
			opts: groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}},
			mutate: func(f *groomingSourceFixture) {
				// Every page comes back FULL, so the scan never reaches the end of
				// the list within its page budget.
				f.runs.listRuns = func(run.ListRunsFilter) ([]*run.Run, error) {
					out := make([]*run.Run, groomingSupersessionPageSize)
					for i := range out {
						out[i] = &run.Run{ID: uuid.New(), CreatedAt: time.Now().Add(-2 * time.Hour)}
					}
					return out, nil
				}
			},
			wantStatus: http.StatusUnprocessableEntity, wantCode: codeGroomingSupersessionUndetermined,
		},
		{
			name: "the order names no issue in the target repo",
			opts: groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}},
			mutate: func(f *groomingSourceFixture) {
				// Re-point the report at another repo's issues: every entry is
				// excluded, so nothing is campaignable.
				f.artifacts.byStage[f.stageID][0].Content = groomingSourceReportJSON(t, "other", "repo", 1, 2)
			},
			wantStatus: http.StatusUnprocessableEntity, wantCode: codeGroomingOrderEmpty,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroomingSourceFixture(t, tc.opts, 20, 10)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			body := groomingSourceBody(f.runID, "")
			if tc.body != nil {
				body = tc.body(f)
			}
			w := postCampaign(t, f.server, body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			code, _ := decodeGroomingErr(t, w.Body.Bytes())
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q (body=%s)", code, tc.wantCode, w.Body.String())
			}
			// COMMITTED STATE: no campaign may exist behind a refusal.
			if n := f.campaigns.countCampaigns(); n != 0 {
				t.Fatalf("%d campaign(s) persisted behind a refusal, want 0", n)
			}
		})
	}
}

// TestCreateCampaign_GroomingSource_SupersededRefused is K2's headline: a NEWER
// approved grooming run of the same workflow refuses the create by default, and
// the refusal NAMES the newer run so the operator can act on it. The newer run
// is seeded BY CONSTRUCTION (a second run stamped strictly later with its own
// approved report), never by calling the control in the test's own setup.
func TestCreateCampaign_GroomingSource_SupersededRefused(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
	newerID := f.seedNewerApprovedGroomingRun(t, time.Now())

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	code, details := decodeGroomingErr(t, w.Body.Bytes())
	if code != codeGroomingSuperseded {
		t.Fatalf("code = %q, want %q", code, codeGroomingSuperseded)
	}
	if details["superseded_by"] != newerID.String() {
		t.Errorf("details.superseded_by = %v, want %s", details["superseded_by"], newerID)
	}
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted behind the supersession refusal, want 0", n)
	}
}

// TestCreateCampaign_GroomingSource_AllowSupersededRecordsTheChoice: the
// acknowledgement converts the refusal into an EXPLICIT record — never a silent
// use of a stale order. The durable provenance must name the run that superseded
// it, so a later reader can see the choice was deliberate.
func TestCreateCampaign_GroomingSource_AllowSupersededRecordsTheChoice(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
	newerID := f.seedNewerApprovedGroomingRun(t, time.Now())

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, `,"allow_superseded":true`))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stored, err := f.campaigns.GetCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	var prov campaignGroomingSourcePayload
	if err := json.Unmarshal(stored.GroomingSource, &prov); err != nil {
		t.Fatalf("decode provenance: %v", err)
	}
	if prov.SupersededBy == nil || *prov.SupersededBy != newerID {
		t.Fatalf("persisted superseded_by = %v, want %s — an acknowledged stale order must be recorded, not silent", prov.SupersededBy, newerID)
	}
}

// TestCreateCampaign_GroomingSource_UnapprovedReportRefused is the dedicated
// counterfactual vehicle for the RATIFICATION check. The bad state is seeded BY
// CONSTRUCTION — a run whose plan stage carries no approval row at all — so the
// RED lands on the behavioural assertion rather than on fixture setup.
func TestCreateCampaign_GroomingSource_UnapprovedReportRefused(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{ /* no approvals seeded */ }, 20, 10)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	if code, _ := decodeGroomingErr(t, w.Body.Bytes()); code != codeGroomingNotApproved {
		t.Fatalf("code = %q, want %q", code, codeGroomingNotApproved)
	}
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted from an UNRATIFIED order, want 0", n)
	}
}

// TestCreateCampaign_GroomingSource_CrossTenantRunNotFound is the dedicated
// counterfactual vehicle for the tenant scoping, asserting both halves: the
// refusal fires AND no campaign is persisted from another account's order.
func TestCreateCampaign_GroomingSource_CrossTenantRunNotFound(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		accountID: uuid.NewString(),
	}, 20, 10)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
	if code, _ := decodeGroomingErr(t, w.Body.Bytes()); code != codeGroomingRunNotFound {
		t.Fatalf("code = %q, want %q — a cross-tenant run must be indistinguishable from a missing one", code, codeGroomingRunNotFound)
	}
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted from another account's order, want 0", n)
	}
}

// TestCreateCampaign_GroomingSource_MutualExclusivity is the counterfactual
// vehicle for the three-source guard. Both conflicting combinations get their
// own case, and each asserts the provider was NEVER dialed — the point of
// refusing before the forge round-trip.
func TestCreateCampaign_GroomingSource_MutualExclusivity(t *testing.T) {
	tests := []struct {
		name  string
		extra string
	}{
		{"grooming_source + epic_ref", `"epic_ref":"issue:1","`},
		{"grooming_source + items", `"items":["issue:1"],"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
			body := fmt.Sprintf(`{"repo":"kuhlman-labs/fishhawk",%sgrooming_source":{"run_id":%q}}`, tc.extra, f.runID.String())
			w := postCampaign(t, f.server, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
			if code, _ := decodeGroomingErr(t, w.Body.Bytes()); code != "validation_failed" {
				t.Fatalf("code = %q, want validation_failed", code)
			}
			if f.provider.resolveCalled {
				t.Error("the provider was dialed despite a body-shape refusal")
			}
			if n := f.campaigns.countCampaigns(); n != 0 {
				t.Fatalf("%d campaign(s) persisted behind the refusal, want 0", n)
			}
		})
	}
}

// TestCreateCampaign_GroomingSource_NegativeLimitRefused pins the remaining
// body-shape guard.
func TestCreateCampaign_GroomingSource_NegativeLimitRefused(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
	w := postCampaign(t, f.server, groomingSourceBody(f.runID, `,"limit":-1`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if f.provider.resolveCalled {
		t.Error("the provider was dialed despite a body-shape refusal")
	}
}

// TestCreateCampaign_GroomingSource_LimitReportsOmitted pins K4 end to end: a
// capped batch reports HOW MANY ranked issues the cap dropped, so a truncated
// campaign is never silent — and the count is distinct from the exclusions.
func TestCreateCampaign_GroomingSource_LimitReportsOmitted(t *testing.T) {
	// The resolver only ever sees the CAPPED set, so build its result from the
	// top two by rank.
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		children: &workmgmt.EpicChildrenResult{Children: []workmgmt.EpicChild{
			{Number: 101}, {Number: 303},
		}},
	}, 303, 101, 202, 404)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, `,"limit":2`))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var prov campaignGroomingSourcePayload
	if err := json.Unmarshal(created.GroomingSource, &prov); err != nil {
		t.Fatalf("decode provenance: %v", err)
	}
	if prov.Limit != 2 || prov.OmittedByLimit != 2 {
		t.Fatalf("limit=%d omitted_by_limit=%d, want 2 and 2", prov.Limit, prov.OmittedByLimit)
	}
	if fmt.Sprint(prov.OrderedRefs) != "[issue:303 issue:101]" {
		t.Fatalf("ordered_refs = %v, want the top 2 by rank", prov.OrderedRefs)
	}
	// The resolver only ever saw the capped set.
	if fmt.Sprint(f.provider.captured.Items) != "[issue:303 issue:101]" {
		t.Fatalf("resolver got items = %v, want only the capped set", f.provider.captured.Items)
	}
}

// TestCreateCampaign_GroomingSource_ExclusionsReported pins that a
// non-convertible ordering entry is REPORTED in the durable provenance with its
// named reason rather than silently dropped (AC7's sibling: a truncated batch
// an operator cannot see).
func TestCreateCampaign_GroomingSource_ExclusionsReported(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		children:  &workmgmt.EpicChildrenResult{Children: []workmgmt.EpicChild{{Number: 10}}},
	}, 10)
	// Splice in an ordering entry for ANOTHER repo.
	spliceForeignOrderingEntry(t, f, "other/repo#77")

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var prov campaignGroomingSourcePayload
	if err := json.Unmarshal(created.GroomingSource, &prov); err != nil {
		t.Fatalf("decode provenance: %v", err)
	}
	if len(prov.Excluded) != 1 || prov.Excluded[0].Reason != campaign.ExclusionOtherRepo {
		t.Fatalf("excluded = %+v, want exactly one other_repo exclusion NAMED in the provenance", prov.Excluded)
	}
	if prov.Excluded[0].Ref != "other/repo#77" {
		t.Errorf("excluded ref = %q, want other/repo#77", prov.Excluded[0].Ref)
	}
}

// TestCreateCampaign_GroomingSource_DanglingNamesProvenance is AC7: an ordering
// whose depends_on targets an issue OUTSIDE the selected set fails closed, and
// the 422 names both the offending edge and the batch's grooming provenance so
// the operator message can offer the widen-or-drop remedy.
func TestCreateCampaign_GroomingSource_DanglingNamesProvenance(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		children: &workmgmt.EpicChildrenResult{
			Children:     []workmgmt.EpicChild{{Number: 20}, {Number: 10}},
			DroppedEdges: []workmgmt.DependsEdge{{From: 10, To: 999, Reason: workmgmt.DropNotChild}},
		},
	}, 20, 10)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, `,"limit":2`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	code, details := decodeGroomingErr(t, w.Body.Bytes())
	if code != "campaign_dangling_dependency" {
		t.Fatalf("code = %q, want campaign_dangling_dependency", code)
	}
	if details[danglingSourceKey] != danglingSourceGroomingOrder {
		t.Errorf("details.%s = %v, want %q", danglingSourceKey, details[danglingSourceKey], danglingSourceGroomingOrder)
	}
	if details[danglingGroomingRunKey] != f.runID.String() {
		t.Errorf("details.%s = %v, want %s", danglingGroomingRunKey, details[danglingGroomingRunKey], f.runID)
	}
	edges, _ := details["dangling_not_child"].([]any)
	if len(edges) != 1 || edges[0] != "issue:10->issue:999" {
		t.Errorf("details.dangling_not_child = %v, want the offending edge named", details["dangling_not_child"])
	}
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted behind the dangling refusal, want 0", n)
	}
}

// TestCreateCampaign_GroomingSource_ClosedTargetElided_201 is the #2953
// reproducer at the grooming source: a grooming-ordered batch whose depends_on
// targets are OUT-OF-ORDER and closed-and-completed now assembles SUCCESSFULLY
// (201) with the edges elided, where before #2953 it 422'd
// campaign_dangling_dependency with unactionable widen-the-limit advice. The
// response surfaces the elision and no dangling refusal is emitted.
func TestCreateCampaign_GroomingSource_ClosedTargetElided_201(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		// #2032 and #2801 are in the batch; each depends on an out-of-batch target
		// (#1639/#2822) the provider already classified closed-and-completed.
		children: &workmgmt.EpicChildrenResult{
			Children: []workmgmt.EpicChild{{Number: 2032}, {Number: 2801}},
			SatisfiedEdges: []workmgmt.SatisfiedEdge{
				{From: 2032, To: 1639, State: "closed", StateReason: "completed"},
				{From: 2801, To: 2822, State: "closed", StateReason: "completed"},
			},
		},
	}, 2032, 2801)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created campaign: %v", err)
	}
	if len(created.SatisfiedDependencies) != 2 {
		t.Fatalf("satisfied_dependencies = %+v, want 2 elided edges", created.SatisfiedDependencies)
	}
	if n := f.audit.count(auditCampaignDependencyElided); n != 1 {
		t.Errorf("%s audit entries = %d, want exactly 1", auditCampaignDependencyElided, n)
	}
}

// TestCreateCampaign_GroomingSource_SetMismatchIs500 pins the ReorderByPriority
// failure at the handler: a provider that resolved a set the order did not name
// is a BUG, not operator input, so it is a 500 and nothing is persisted.
func TestCreateCampaign_GroomingSource_SetMismatchIs500(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		// The resolver returns an issue the ratified order never named.
		children: &workmgmt.EpicChildrenResult{Children: []workmgmt.EpicChild{{Number: 20}, {Number: 10}, {Number: 777}}},
	}, 20, 10)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted behind the set-mismatch refusal, want 0", n)
	}
}

// TestCreateCampaign_GroomingSource_DependencyLandsInLaterWave is AC4: a
// resolved depends_on edge between two ORDERED issues still puts the dependent
// item in a later wave — the permutation changes the queue order, never the DAG.
func TestCreateCampaign_GroomingSource_DependencyLandsInLaterWave(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{
		approvals: []approval.Decision{approval.DecisionApprove},
		children: &workmgmt.EpicChildrenResult{
			Children: []workmgmt.EpicChild{{Number: 10}, {Number: 20}},
			// 20 depends on 10, while the ratified order ranks 20 FIRST.
			Edges: []workmgmt.DependsEdge{{From: 20, To: 10}},
		},
	}, 20, 10)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, err := f.campaigns.ListCampaignItemsForCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	byRef := map[string]*campaign.Item{}
	for _, it := range items {
		byRef[it.IssueRef] = it
	}
	// The QUEUE order follows the rank...
	if items[0].IssueRef != "issue:20" {
		t.Fatalf("queue order = %s first, want issue:20 (the ratified rank)", items[0].IssueRef)
	}
	// ...while the DAG edge survives, so the dependent item still waits.
	dep := byRef["issue:20"]
	if dep == nil || len(dep.DependsOn) != 1 || dep.DependsOn[0] != "issue:10" {
		t.Fatalf("issue:20 depends_on = %v, want [issue:10] — the permutation must not perturb the DAG", dep)
	}
	if root := byRef["issue:10"]; root == nil || len(root.DependsOn) != 0 {
		t.Fatalf("issue:10 depends_on = %v, want empty", root)
	}
}

// countCampaigns reports how many campaigns the fake persisted — the
// COMMITTED-STATE half of every refusal assertion, so a control that fired and
// was then bypassed cannot pass on error identity alone.
func (f *fakeCampaignRepo) countCampaigns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.campaigns)
}

// seedNewerApprovedGroomingRun adds a SECOND grooming run of the same workflow,
// stamped strictly later than the source and carrying its own APPROVED grooming
// report. Seeded BY CONSTRUCTION: nothing in this helper calls the supersession
// control, so a test built on it fails on the behavioural assertion rather than
// on fixture setup.
func (f *groomingSourceFixture) seedNewerApprovedGroomingRun(t *testing.T, at time.Time) uuid.UUID {
	t.Helper()
	newerRun, newerStage := uuid.New(), uuid.New()
	src := f.runs.runs[f.runID]
	f.runs.runs[newerRun] = &run.Run{
		ID: newerRun, Repo: src.Repo, WorkflowID: src.WorkflowID,
		AccountID: src.AccountID, CreatedAt: at,
	}
	f.runs.stages[newerRun] = []*run.Stage{{ID: newerStage, RunID: newerRun, Type: run.StageTypePlan}}
	f.artifacts.byStage[newerStage] = []*artifact.Artifact{{
		ID: uuid.New(), StageID: newerStage, Kind: artifact.KindGroomingReport,
		Content:     groomingSourceReportJSON(t, "kuhlman-labs", "fishhawk", 10, 20),
		ContentHash: "sha256:newer-report", CreatedAt: at,
	}}
	f.approvals.byStage[newerStage] = []*approval.Approval{{
		ID: uuid.New(), StageID: newerStage, Decision: approval.DecisionApprove, ApproverSubject: "op",
	}}
	return newerRun
}

// TestCreateCampaign_GroomingSource_BoardSweepFiresOverTheRatifiedQueue is AC2:
// a grooming-sourced campaign's pending -> running edge fires the EXISTING
// #1816 campaign_started -> Up Next sweep once per still-queued item, over the
// ratified queue, with NO board write path added by this change.
//
// It drives the real deriveCampaignAfterChange against the campaign this
// change's own create handler persisted, so the sweep is exercised over a
// genuinely grooming-sourced campaign rather than a hand-seeded one.
func TestCreateCampaign_GroomingSource_BoardSweepFiresOverTheRatifiedQueue(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 303, 101, 202)

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, err := f.campaigns.ListCampaignItemsForCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}

	// Now swap the registry for a board Transitioner and drive the pending ->
	// running derivation with the FIRST item (the rank-1 issue) dispatched.
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{Moved: true, From: "Backlog", To: "Up Next"}}
	registerTransitionProvider(t, fp)
	boardServer, _ := campaignBoardServer(t)
	boardServer.cfg.CampaignRepo = f.campaigns

	items[0].State = campaign.ItemStateRunning
	c, err := f.campaigns.GetCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	boardServer.deriveCampaignAfterChange(context.Background(), c, items)

	// One move per STILL-QUEUED item (the dispatched rank-1 item is naturally
	// excluded), each to Up Next under the existing campaign_started trigger.
	if len(fp.calls) != 2 {
		t.Fatalf("board moves = %d, want 2 (one per still-queued item)", len(fp.calls))
	}
	var gotNumbers []int
	for _, call := range fp.calls {
		if call.Trigger != lifecycleCampaignStarted {
			t.Errorf("trigger = %q, want %q — this change adds no board write path of its own", call.Trigger, lifecycleCampaignStarted)
		}
		if call.CanonicalState != workmgmt.CanonicalStateUpNext {
			t.Errorf("canonical state = %q, want up_next", call.CanonicalState)
		}
		gotNumbers = append(gotNumbers, call.IssueNumber)
	}
	// The sweep walks the queue in its persisted (ratified) order.
	if fmt.Sprint(gotNumbers) != "[101 202]" {
		t.Fatalf("swept issue numbers = %v, want [101 202] in ratified rank order", gotNumbers)
	}
}

// TestCreateCampaign_GroomingSource_RepoCauseNeverReachesTheCaller is the
// call-site behavioural proof that the grooming ladder's REPOSITORY errors ride
// the non-client-facing internalCauseKey channel (E67.15 / #2587) rather than a
// plain `error` detail key. A repository error renders storage, query and
// infrastructure text; an agent-facing authenticated caller has no business
// reading it, while the operator must keep all of it.
//
// It drives the REAL handleCreateCampaign once per edited call site — GetRun,
// the source run's stage/artifact/approval read, and the supersession scan's
// ListRuns — and asserts both halves of the join:
//
//   - the client body carries the static message and error_ref and NEITHER the
//     cause text NOR the literal "__cause" channel key;
//   - ONE operator log record carries that SAME error_ref together with the
//     full cause.
//
// COUNTERFACTUAL NOTE: the body half alone would NOT discriminate. writeError's
// 5xx allow-list drops a non-admitted `error` key anyway, so reverting the call
// sites leaves the body assertions green — the LOG-cause assertion is the one
// that goes red, which is exactly the hole the key-based allow-list leaves.
func TestCreateCampaign_GroomingSource_RepoCauseNeverReachesTheCaller(t *testing.T) {
	tests := []struct {
		name       string
		sentinel   string
		mutate     func(f *groomingSourceFixture, injected error)
		wantStatus int
		wantMsg    string
	}{
		{
			name:     "get run",
			sentinel: "pgx: SQLSTATE 08006 host=grooming-db-01 dial failed",
			mutate: func(f *groomingSourceFixture, injected error) {
				f.runs.getRunErr = injected
			},
			wantStatus: http.StatusInternalServerError,
			wantMsg:    "could not read the grooming run",
		},
		{
			name:     "source run stage read",
			sentinel: "pgx: SQLSTATE 42P01 relation \"stages\" does not exist",
			mutate: func(f *groomingSourceFixture, injected error) {
				f.runs.stagesErr = injected
			},
			wantStatus: http.StatusBadGateway,
			wantMsg:    "could not read the grooming run's stages, artifacts or approvals",
		},
		{
			name:     "supersession scan candidate list",
			sentinel: "pgx: SQLSTATE 53300 too many connections for role \"fishhawk\"",
			mutate: func(f *groomingSourceFixture, injected error) {
				f.runs.listRuns = func(run.ListRunsFilter) ([]*run.Run, error) { return nil, injected }
			},
			wantStatus: http.StatusBadGateway,
			wantMsg:    "could not determine whether a newer approved grooming run supersedes this order",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const reqID = "grooming-cause-ref"
			f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
			tc.mutate(f, fmt.Errorf("%s", tc.sentinel))

			var logBuf bytes.Buffer
			f.server.cfg.Logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			req := httptest.NewRequest(http.MethodPost, "/v0/campaigns",
				strings.NewReader(groomingSourceBody(f.runID, "")))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(context.WithValue(req.Context(), ctxKeyRequestID, reqID))
			w := httptest.NewRecorder()
			f.server.handleCreateCampaign(w, withAuth(req))

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.wantStatus, w.Body.String())
			}

			// (a) The client body: no repository cause, no internal channel key.
			body := w.Body.String()
			if strings.Contains(body, tc.sentinel) {
				t.Errorf("the repository cause leaked into the client body: %s", body)
			}
			if strings.Contains(body, internalCauseKey) {
				t.Errorf("the internal cause key leaked into the client body: %s", body)
			}
			var env errorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode body: %v (%s)", err, body)
			}
			if _, present := env.Error.Details[internalCauseKey]; present {
				t.Errorf("details still carry %q: %v", internalCauseKey, env.Error.Details)
			}
			if env.Error.Message != tc.wantMsg {
				t.Errorf("message = %q, want the static literal %q", env.Error.Message, tc.wantMsg)
			}
			if env.Error.ErrorRef != reqID {
				t.Fatalf("body error_ref = %q, want %q — the caller's only correlation handle", env.Error.ErrorRef, reqID)
			}

			// (b) ONE operator log record joins the SAME ref with the full cause.
			rec := soleLogRecord(t, &logBuf, "http error response")
			if rec["error_ref"] != reqID {
				t.Errorf("log error_ref = %v, want %q (must equal the body ref)", rec["error_ref"], reqID)
			}
			cause, _ := rec["cause"].(string)
			if !strings.Contains(cause, tc.sentinel) {
				t.Errorf("log cause = %v, want it to carry the repository cause %q joined to the ref", rec["cause"], tc.sentinel)
			}

			if n := f.campaigns.countCampaigns(); n != 0 {
				t.Fatalf("%d campaign(s) persisted behind the read failure, want 0", n)
			}
		})
	}
}

// TestCreateCampaign_GroomingSource_SupersededFoundOnALaterPage covers the
// scan's SUCCESSFUL multi-page path, which neither the short-first-page fixture
// nor the perpetually-full-page cap case reaches: a FULL first page of
// irrelevant runs, then a later SHORT page carrying the superseding approved
// run. Without it, broken offset handling or a scan that only ever inspected
// its first page would pass while missing a known superseding run.
//
// The two failure modes discriminate in opposite directions, which is what
// makes this a real vehicle: a scan that never advanced its offset would read
// the FULL page forever and refuse UNDETERMINED, while a scan that stopped
// after page one would find no superseding run and return 201.
func TestCreateCampaign_GroomingSource_SupersededFoundOnALaterPage(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
	newerID := f.seedNewerApprovedGroomingRun(t, time.Now())
	src := f.runs.runs[f.runID]
	newer := f.runs.runs[newerID]

	// Page 0 is FULL and carries only runs OLDER than the source, so nothing on
	// it can supersede and the scan must keep paging. Page 1 is SHORT — the
	// end-of-list proof — and is where the superseding run actually lives.
	firstPage := make([]*run.Run, groomingSupersessionPageSize)
	for i := range firstPage {
		firstPage[i] = &run.Run{
			ID: uuid.New(), Repo: src.Repo, WorkflowID: src.WorkflowID,
			AccountID: src.AccountID, CreatedAt: src.CreatedAt.Add(-time.Hour),
		}
	}
	var offsets []int
	f.runs.listRuns = func(filter run.ListRunsFilter) ([]*run.Run, error) {
		offsets = append(offsets, filter.Offset)
		switch filter.Offset {
		case 0:
			return firstPage, nil
		case groomingSupersessionPageSize:
			return []*run.Run{newer}, nil
		default:
			return nil, fmt.Errorf("scan requested offset %d; it must walk pages in order", filter.Offset)
		}
	}

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 — the superseding run lives on the SECOND page and must still be found (body=%s)",
			w.Code, w.Body.String())
	}
	code, details := decodeGroomingErr(t, w.Body.Bytes())
	if code != codeGroomingSuperseded {
		t.Fatalf("code = %q, want %q (body=%s)", code, codeGroomingSuperseded, w.Body.String())
	}
	if details["superseded_by"] != newerID.String() {
		t.Errorf("details.superseded_by = %v, want %s", details["superseded_by"], newerID)
	}
	// The scan walked BOTH pages, in order, and stopped at the short one.
	if fmt.Sprint(offsets) != fmt.Sprint([]int{0, groomingSupersessionPageSize}) {
		t.Errorf("scan offsets = %v, want [0 %d] — one full page then the short page that ends the list",
			offsets, groomingSupersessionPageSize)
	}
	if n := f.campaigns.countCampaigns(); n != 0 {
		t.Fatalf("%d campaign(s) persisted from an order a later page proved superseded, want 0", n)
	}
}

// TestCreateCampaign_GroomingSource_ZeroTimestampedReportIsNotAbsent pins the
// latest-report selection against a report artifact whose CreatedAt is the ZERO
// time. time.Time{}.UnixNano() is a large NEGATIVE number, so a numeric-floor
// sentinel treated such an artifact as absent and a run that DID ship a report
// surfaced as 422 grooming_order_absent. The selection must key on FIRST HIT,
// not on a wall-clock floor.
func TestCreateCampaign_GroomingSource_ZeroTimestampedReportIsNotAbsent(t *testing.T) {
	f := newGroomingSourceFixture(t, groomingSourceOpts{approvals: []approval.Decision{approval.DecisionApprove}}, 20, 10)
	// Seeded BY CONSTRUCTION: the artifact is stamped with the zero time before
	// any request runs, so the RED lands on the behavioural assertion.
	f.artifacts.byStage[f.stageID][0].CreatedAt = time.Time{}

	w := postCampaign(t, f.server, groomingSourceBody(f.runID, ""))
	if w.Code != http.StatusCreated {
		code, _ := decodeGroomingErr(t, w.Body.Bytes())
		t.Fatalf("status = %d code = %q, want 201 — a zero-timestamped report artifact is PRESENT, not absent (body=%s)",
			w.Code, code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// COMMITTED STATE: the order this report carried is the queue that landed.
	items, err := f.campaigns.ListCampaignItemsForCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var gotRefs []string
	for _, it := range items {
		gotRefs = append(gotRefs, it.IssueRef)
	}
	if fmt.Sprint(gotRefs) != "[issue:20 issue:10]" {
		t.Fatalf("persisted queue order = %v, want [issue:20 issue:10]", gotRefs)
	}
}
