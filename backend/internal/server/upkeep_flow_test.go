package server

// Cross-boundary flow tests for the upkeep scan's last phase (#3924, slice 4).
//
// Each case drives the REAL seams end to end, with nothing called directly
// that a production request would reach through a route:
//
//	signed POST /v0/runs/{id}/plan (shipPlanRequest, the real router)
//	  → upkeep_report ingest → dedupe through a registered fake reader whose
//	    window carries one open issue with finding B's hidden marker and one
//	    whose title is finding F's → upkeep_report_recorded {duplicates:[B,F]}
//	POST /v0/runs/{id}/upkeep-dispositions (handleRecordUpkeepDispositions)
//	  → upkeep_disposition_recorded rows inside the open capture window
//	POST /v0/stages/{id}/approvals (handleSubmitApproval)
//	  → finishApprovalAdvance → applyApprovedUpkeep (the approvals.go call)
//	  → settlement → the detached per-finding loop → applyAndFileWorkItem
//	  → the SAME registered fake provider, now as the filer
//	a later disposition POST → 409 upkeep_window_closed
//
// and every assertion reads the fakes' durable state after the calls return.
//
// The fixture carries ALL six findings A–F the dispositions name (binding
// condition C7):
//
//	A approved, parent_epic override #77      → FILED, parent epic #77
//	B approved, open issue carries B's marker → skipped duplicate_of_open_issue
//	C rejected                                → skipped not_approved
//	D left undispositioned                    → skipped not_approved
//	E approved, proposes autonomy:high, tier
//	  NOT authorized, proposal's epic #3726   → FILED without autonomy:high,
//	                                            parent epic #3726
//	F approved, open issue titled like F      → skipped duplicate_of_open_issue
//
// COUNTERFACTUALS (run, not reasoned — see the PR notes):
//   - delete the `case isDup:` skip in upkeep_apply.go → B and F (approved,
//     marked at ingest) are filed → TestUpkeepFlow_IngestDispositionsApproveFilesExactlyApproved red.
//   - delete the s.applyApprovedUpkeep call in approvals.go → zero filings on
//     approve AND no `rejected` watermark on reject → BOTH tests red.
//
// The audit fake does NOT implement audit.UpkeepWindowAppender, so the
// settlement and the capture's window check both take their in-memory
// read-then-append fallback; the atomic production transaction is pinned
// against real Postgres by upkeep_dispositions_pg_test.go. The fakes are local
// to this file or compose existing ones without modifying them (C6).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// ukFlowProvider is ONE registered provider that the ingest dedupe reads
// through (igReadProvider's ListWorkItems window) and the apply files through
// (ukApplyProvider's recording File), so both halves of the flow resolve the
// same conventions-named provider exactly as production does.
type ukFlowProvider struct {
	*igReadProvider
	filer *ukApplyProvider
}

func (p *ukFlowProvider) Name() string { return p.filer.Name() }

func (p *ukFlowProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	return p.filer.File(ctx, req)
}

// ukFlowRunRepo is the ingest's multi-run wrapper plus a CreateRun counter:
// the upkeep apply must NEVER create a run.
type ukFlowRunRepo struct {
	*upkeepRunRepo
	creates atomic.Int32
}

func (r *ukFlowRunRepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	r.creates.Add(1)
	return r.upkeepRunRepo.CreateRun(ctx, p)
}

// ukFlowAudit is the sequence-assigning chain (ukAudit) with ListForRun
// served from the same entries, so every reader on the approval path sees
// the rows the ingest and the capture wrote.
type ukFlowAudit struct {
	*ukAudit
}

func (a *ukFlowAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// The six findings. Titles share no content words, so the only similarity
// mark the ingest can produce is F's (asserted on the recorded row below).
var (
	ukFlowA = ukApplyFinding{subject: "flow/csvlib", title: "Migrate the invoice exporter off csvlib",
		typ: "chore", labels: []string{"area:backend"}, parentEpic: "#3726"}
	ukFlowB = ukApplyFinding{subject: "flow/webhooksig", title: "Retire the legacy webhook signer helper",
		typ: "chore", labels: []string{"area:backend"}}
	ukFlowC = ukApplyFinding{subject: "flow/yamlshim", title: "Drop the vendored yaml shim from config loading",
		typ: "chore", labels: []string{"area:backend"}}
	ukFlowD = ukApplyFinding{subject: "flow/ioutilread", title: "Swap ioutil reads in the artifact uploader",
		typ: "chore", labels: []string{"area:backend"}}
	ukFlowE = ukApplyFinding{subject: "flow/protoruntime", title: "Upgrade the protobuf runtime behind trace encoding",
		typ: "chore", labels: []string{"area:backend", "autonomy:high"}, parentEpic: "#3726"}
	ukFlowF = ukApplyFinding{subject: "flow/poolwrap", title: "Remove the sync.Pool wrapper in the metrics buffer",
		typ: "chore", labels: []string{"area:backend"}}

	ukFlowFindings = []ukApplyFinding{ukFlowA, ukFlowB, ukFlowC, ukFlowD, ukFlowE, ukFlowF}
)

// The two OPEN issues the reader serves.
const (
	ukFlowMarkerIssue  = 501 // carries finding B's hidden marker under an unrelated title
	ukFlowSimilarIssue = 502 // titled exactly like finding F
)

type ukFlowFixture struct {
	s         *Server
	rr        *ukFlowRunRepo
	au        *ukFlowAudit
	ar        *fakeArtifactRepo
	provider  *ukFlowProvider
	runRow    *run.Run
	planStage *run.Stage
	priv      ed25519.PrivateKey
}

func newUkFlowFixture(t *testing.T) *ukFlowFixture {
	t.Helper()
	filer := &ukApplyProvider{name: "upkeep-flow-fake-" + uuid.NewString()}
	reader := &igReadProvider{items: []workmgmt.WorkItemRecord{
		{Number: ukFlowMarkerIssue, Title: "Quarterly tracker hygiene",
			Body: "planted\n" + upkeep.FindingMarker(ukFlowB.id()), URL: "https://example.test/501"},
		{Number: ukFlowSimilarIssue, Title: ukFlowF.title, URL: "https://example.test/502"},
	}}
	reader.name = filer.name
	p := &ukFlowProvider{igReadProvider: reader, filer: filer}
	workmgmt.Register(p)
	// The default conventions with a `{summary}` title format (no numbering
	// read) and the autonomy:medium label default KEPT — production's shape.
	installConventions(t, ukApplyConventions(filer.name, true), nil)

	base, runRow, planStage, _ := newUpkeepIngestRepo(upkeepScanSpec(t), "upkeep_scan", uuid.New(), uuid.New())
	f := &ukFlowFixture{
		rr: &ukFlowRunRepo{upkeepRunRepo: base}, au: &ukFlowAudit{ukAudit: newUKAudit()},
		ar: newFakeArtifactRepo(), provider: p, runRow: runRow, planStage: planStage,
	}
	sf := newSigningFake()
	f.s = New(Config{
		Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: f.ar, AuditRepo: f.au, RunRepo: f.rr,
		ApprovalRepo: newFakeApprovalRepo(),
		Logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	f.priv, _ = sf.issue(t, runRow.ID)
	return f
}

// ingest ships the six-finding report through the signed route and returns
// the bound artifact id, asserting the dedupe marked exactly B (marker) and F
// (similarity) — the precondition every later assertion leans on.
func (f *ukFlowFixture) ingest(t *testing.T) string {
	t.Helper()
	w := shipPlanRequest(t, f.s, f.runRow.ID, f.planStage.ID, f.priv, ukApplyReportBody(t, ukFlowFindings), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ingest status = %d, want 201: %s", w.Code, w.Body.String())
	}
	rows, err := f.au.ListForRunByCategory(context.Background(), f.runRow.ID, CategoryUpkeepReportRecorded)
	if err != nil || len(rows) != 1 {
		t.Fatalf("upkeep_report_recorded rows = %d (err %v), want 1", len(rows), err)
	}
	var rec struct {
		ArtifactID string             `json:"artifact_id"`
		Duplicates []upkeep.Duplicate `json:"duplicates"`
	}
	if err := json.Unmarshal(rows[0].Payload, &rec); err != nil {
		t.Fatalf("decode recorded row: %v", err)
	}
	got := map[string]upkeep.Duplicate{}
	for _, d := range rec.Duplicates {
		got[d.FindingID] = d
	}
	if d := got[ukFlowB.id()]; d.IssueNumber != ukFlowMarkerIssue || d.Basis != upkeep.BasisMarker {
		t.Fatalf("B duplicate = %+v, want #%d by marker", d, ukFlowMarkerIssue)
	}
	if d := got[ukFlowF.id()]; d.IssueNumber != ukFlowSimilarIssue || d.Basis != upkeep.BasisSimilarity {
		t.Fatalf("F duplicate = %+v, want #%d by similarity", d, ukFlowSimilarIssue)
	}
	if len(got) != 2 {
		t.Fatalf("recorded duplicates = %+v, want exactly B and F", rec.Duplicates)
	}
	if st := f.rr.getStages[f.planStage.ID].State; st != run.StageStateAwaitingApproval {
		t.Fatalf("plan stage = %q after ingest, want awaiting_approval", st)
	}
	return rec.ArtifactID
}

// dispose records the captain's verdicts through the real capture handler:
// A (override #77), B, E (tier NOT authorized) and F approved, C rejected,
// D left undispositioned.
func (f *ukFlowFixture) dispose(t *testing.T) {
	t.Helper()
	body := fmt.Sprintf(`{"dispositions":[
		{"finding_id":%q,"verdict":"approved","parent_epic":"#77"},
		{"finding_id":%q,"verdict":"approved"},
		{"finding_id":%q,"verdict":"rejected"},
		{"finding_id":%q,"verdict":"approved","authorize_delegation_tier":false},
		{"finding_id":%q,"verdict":"approved"}]}`,
		ukFlowA.id(), ukFlowB.id(), ukFlowC.id(), ukFlowE.id(), ukFlowF.id())
	w := postUK(t, f.s, f.runRow.ID.String(), body, ukOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("disposition capture status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if n := f.au.count(CategoryUpkeepDispositionRecorded); n != 5 {
		t.Fatalf("upkeep_disposition_recorded rows = %d, want 5", n)
	}
}

// decide submits the gate decision through the real approval route, then
// waits on the detached apply's WaitGroup barrier.
func (f *ukFlowFixture) decide(t *testing.T, body string) {
	t.Helper()
	w := submitApproval(t, f.s, f.planStage.ID, body)
	if w.Code != http.StatusOK {
		t.Fatalf("approval status = %d, want 200: %s", w.Code, w.Body.String())
	}
	f.s.waitUpkeepApply()
}

// assertWindowClosedOnLaterCapture: a capture after settlement is refused 409
// upkeep_window_closed and records nothing — proof the watermark the hook
// wrote is the one the capture route reads.
func (f *ukFlowFixture) assertWindowClosedOnLaterCapture(t *testing.T) {
	t.Helper()
	before := f.au.count(CategoryUpkeepDispositionRecorded)
	w := postUK(t, f.s, f.runRow.ID.String(),
		fmt.Sprintf(`{"dispositions":[{"finding_id":%q,"verdict":"approved"}]}`, ukFlowD.id()), ukOperator)
	requireGDError(t, w, http.StatusConflict, "upkeep_window_closed")
	if after := f.au.count(CategoryUpkeepDispositionRecorded); after != before {
		t.Errorf("upkeep_disposition_recorded rows %d → %d across a refused capture, want unchanged", before, after)
	}
}

func (f *ukFlowFixture) payloads(t *testing.T, category string) []json.RawMessage {
	t.Helper()
	rows, err := f.au.ListForRunByCategory(context.Background(), f.runRow.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]json.RawMessage, 0, len(rows))
	for _, e := range rows {
		out = append(out, e.Payload)
	}
	return out
}

func (f *ukFlowFixture) watermarks(t *testing.T) []groomingWindowPayload {
	t.Helper()
	var out []groomingWindowPayload
	for _, raw := range f.payloads(t, audit.UpkeepApplyWindowClosedCategory) {
		var p groomingWindowPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestUpkeepFlow_IngestDispositionsApproveFilesExactlyApproved: approve
// through the real route files EXACTLY the approved non-duplicate findings
// {A, E} — never B or F (marked duplicates at ingest), never C (rejected) or D
// (undispositioned) — and creates no run.
func TestUpkeepFlow_IngestDispositionsApproveFilesExactlyApproved(t *testing.T) {
	f := newUkFlowFixture(t)
	artifactID := f.ingest(t)
	f.dispose(t)
	f.decide(t, `{"decision":"approve","comment":"file the approved upkeep findings"}`)

	// The provider filed exactly A and E.
	reqs := f.provider.filer.byTitle()
	if len(reqs) != 2 || len(f.provider.filer.requests()) != 2 {
		t.Fatalf("filed titles = %v, want exactly {%q, %q}", ukSortedKeys(reqs), ukFlowA.title, ukFlowE.title)
	}
	for _, c := range []struct {
		fd       ukApplyFinding
		wantEpic string
	}{{ukFlowA, "#77"}, {ukFlowE, "#3726"}} {
		fd, wantEpic := c.fd, c.wantEpic
		req, ok := reqs[fd.title]
		if !ok {
			t.Fatalf("no filing for %q; filed %v", fd.title, ukSortedKeys(reqs))
		}
		// C7: the disposition override where given (A), else the proposal's (E).
		if pe := req.Item.Relations.ParentEpic; pe != wantEpic {
			t.Errorf("%q parent epic = %q, want %q", fd.title, pe, wantEpic)
		}
		key := workmgmt.MintIdempotencyKey(upkeepIdempotencyNamespace, f.runRow.ID.String(), artifactID, fd.id())
		if !workmgmt.BodyHasIdempotencyKey(req.Item.Body, key) {
			t.Errorf("%q body lacks the idempotency key %s:\n%s", fd.title, key, req.Item.Body)
		}
		if !strings.Contains(req.Item.Body, upkeep.FindingMarker(fd.id())) {
			t.Errorf("%q body lacks the finding marker:\n%s", fd.title, req.Item.Body)
		}
	}
	// E proposed autonomy:high without an authorized tier: the PROPOSED tier
	// is not applied (the conventions' own default may be).
	if labels := reqs[ukFlowE.title].Item.Classification.Labels; hasLabelFold(labels, "autonomy:high") {
		t.Errorf("E filed labels = %v, want no autonomy:high (tier not authorized)", labels)
	}

	// Audit: filed rows for A and E only, with the parent epic and the strip.
	filed := map[string]upkeepFindingFiledPayload{}
	for _, raw := range f.payloads(t, CategoryUpkeepFindingFiled) {
		var p upkeepFindingFiledPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		filed[p.FindingID] = p
	}
	if got := ukSortedKeys(filed); len(got) != 2 || filed[ukFlowA.id()].FindingID == "" || filed[ukFlowE.id()].FindingID == "" {
		t.Fatalf("upkeep_finding_filed findings = %v, want exactly A and E", got)
	}
	if pe := filed[ukFlowA.id()].ParentEpic; pe != "#77" {
		t.Errorf("A filed row parent_epic = %q, want #77", pe)
	}
	if pe := filed[ukFlowE.id()].ParentEpic; pe != "#3726" {
		t.Errorf("E filed row parent_epic = %q, want #3726", pe)
	}
	if s := filed[ukFlowE.id()].StrippedLabels; !hasLabelFold(s, "autonomy:high") {
		t.Errorf("E stripped_labels = %v, want autonomy:high", s)
	}
	if a := filed[ukFlowE.id()].AppliedLabels; hasLabelFold(a, "autonomy:high") {
		t.Errorf("E applied_labels = %v, want no autonomy:high", a)
	}
	for id, row := range filed {
		if row.ArtifactID != artifactID || row.IssueNumber == 0 || row.Provider != f.provider.Name() {
			t.Errorf("filed row %s = %+v, want artifact %s, an issue number and provider %s", id, row, artifactID, f.provider.Name())
		}
	}

	// Skips: B and F as duplicates (number + basis visible), C and D not_approved.
	skips := map[string]upkeepFindingSkippedPayload{}
	for _, raw := range f.payloads(t, CategoryUpkeepFindingSkipped) {
		var p upkeepFindingSkippedPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		skips[p.FindingID] = p
	}
	want := map[string]upkeepFindingSkippedPayload{
		ukFlowB.id(): {SkipReason: upkeepSkipDuplicate, DuplicateIssueNumber: ukFlowMarkerIssue, DuplicateBasis: string(upkeep.BasisMarker)},
		ukFlowF.id(): {SkipReason: upkeepSkipDuplicate, DuplicateIssueNumber: ukFlowSimilarIssue, DuplicateBasis: string(upkeep.BasisSimilarity)},
		ukFlowC.id(): {SkipReason: upkeepSkipNotApproved},
		ukFlowD.id(): {SkipReason: upkeepSkipNotApproved},
	}
	if len(skips) != len(want) {
		t.Errorf("upkeep_finding_skipped findings = %v, want %v", ukSortedKeys(skips), ukSortedKeys(want))
	}
	for id, w := range want {
		g := skips[id]
		if g.SkipReason != w.SkipReason || g.DuplicateIssueNumber != w.DuplicateIssueNumber || g.DuplicateBasis != w.DuplicateBasis {
			t.Errorf("skip %s = {%s #%d %s}, want {%s #%d %s}", id,
				g.SkipReason, g.DuplicateIssueNumber, g.DuplicateBasis, w.SkipReason, w.DuplicateIssueNumber, w.DuplicateBasis)
		}
	}

	// One `approved` watermark, one non-degraded summary.
	if wm := f.watermarks(t); len(wm) != 1 || wm[0].Settlement != "approved" || wm[0].ArtifactID != artifactID {
		t.Errorf("watermarks = %+v, want one 'approved' for %s", wm, artifactID)
	}
	completed := f.payloads(t, CategoryUpkeepApplyCompleted)
	if len(completed) != 1 {
		t.Fatalf("upkeep_apply_completed rows = %d, want 1", len(completed))
	}
	var sum upkeepApplyCompletedPayload
	if err := json.Unmarshal(completed[0], &sum); err != nil {
		t.Fatal(err)
	}
	if sum != (upkeepApplyCompletedPayload{ArtifactID: artifactID, Findings: 6, Filed: 2, Skipped: 4}) {
		t.Errorf("upkeep_apply_completed = %+v, want {findings:6 filed:2 skipped:4 failed:0 budget_exhausted:0 degraded:false}", sum)
	}

	if n := f.rr.creates.Load(); n != 0 {
		t.Errorf("CreateRun calls = %d, want 0 (the upkeep apply never creates a run)", n)
	}
	f.assertWindowClosedOnLaterCapture(t)
}

// TestUpkeepFlow_RejectThroughApprovalRouteSettlesRejected (binding condition
// C3): a REJECT through the real approval route closes the capture window
// `rejected`, files nothing, writes no per-finding or summary row, creates no
// run, and a later capture is refused 409 upkeep_window_closed — although
// four findings carry approved dispositions.
func TestUpkeepFlow_RejectThroughApprovalRouteSettlesRejected(t *testing.T) {
	f := newUkFlowFixture(t)
	artifactID := f.ingest(t)
	f.dispose(t)
	f.decide(t, `{"decision":"reject","comment":"not this week"}`)

	if wm := f.watermarks(t); len(wm) != 1 || wm[0].Settlement != "rejected" || wm[0].ArtifactID != artifactID {
		t.Fatalf("watermarks = %+v, want one 'rejected' for %s", wm, artifactID)
	}
	if n := len(f.provider.filer.requests()); n != 0 {
		t.Errorf("File calls = %d on a rejected gate, want 0", n)
	}
	for _, c := range []string{CategoryUpkeepFindingFiled, CategoryUpkeepFindingSkipped, CategoryUpkeepApplyCompleted} {
		if n := len(f.payloads(t, c)); n != 0 {
			t.Errorf("%s rows = %d on a rejected gate, want 0", c, n)
		}
	}
	if n := f.rr.creates.Load(); n != 0 {
		t.Errorf("CreateRun calls = %d, want 0", n)
	}
	f.assertWindowClosedOnLaterCapture(t)
}

// TestUpkeepFlow_FixtureCarriesSixDistinctFindings pins that the six-finding fixture is a valid report with
// six distinct finding ids (a collapsed id would silently shrink the flow).
func TestUpkeepFlow_FixtureCarriesSixDistinctFindings(t *testing.T) {
	r, err := plan.ParseUpkeepReport(ukApplyReportBody(t, ukFlowFindings))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	seen := map[string]bool{}
	for _, fd := range r.Findings {
		seen[fd.ID] = true
	}
	if len(r.Findings) != 6 || len(seen) != 6 {
		t.Fatalf("findings = %d (distinct %d), want 6 distinct A–F", len(r.Findings), len(seen))
	}
}
