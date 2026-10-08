package server

// Cross-boundary flow tests for the comms scan's last phase (#4017, slice 2).
//
// Each case drives the REAL seams end to end, with nothing called directly
// that a production request would reach through a route:
//
//	signed GET /v0/stages/{id}/prompt (promptRequest, the real router)
//	  → the real userreport.Scan + Classify over a fake reader → the
//	    suppression memory (the audit chain's comms_apply_completed rows)
//	    → a comms_scan_gathered row
//	signed POST /v0/runs/{id}/plan (shipPlanRequest, the real router)
//	  → comms_report ingest → previews through the registered provider
//	  → comms_report_recorded
//	POST /v0/runs/{id}/comms-dispositions (handleRecordCommsDispositions)
//	POST /v0/stages/{id}/approvals (handleSubmitApproval)
//	  → finishApprovalAdvance → applyApprovedComms (the approvals.go call)
//	  → settlement → the detached filing loop → applyAndFileWorkItem
//	  → the registered provider → the cursor store
//	a NEW run's signed GET /prompt over the same cursor store and chain
//
// and every assertion reads the fakes' durable state after the calls return.
//
// COUNTERFACTUAL (run, not reasoned — see the PR notes): delete the
// s.applyApprovedComms call in approvals.go → zero filings, no watermark and
// no suppressions → every test here goes red.
//
// The audit fake does NOT implement audit.FamilyWindowAppender, so the
// settlement and the capture's window check take their read-then-append
// fallback; the atomic production transaction is pinned against real
// Postgres by comms_apply_pg_test.go. The fakes compose existing ones without
// modifying them. These tests swap package vars (conventionsLoader,
// commsUserReportReaderFor, commsScanNow) and are NON-parallel.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// cfAudit is cmaAudit (sequence-assigning chain, newest-first ListAll) with
// ListForRun served from the same rows, so every reader on the approval path
// sees what the gather, the ingest and the capture wrote.
type cfAudit struct {
	*cmaAudit
}

func (a *cfAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for i, p := range a.appended {
		if p.RunID != runID {
			continue
		}
		rid := p.RunID
		out = append(out, &audit.Entry{Sequence: int64(i + 1), RunID: &rid, StageID: p.StageID,
			Category: p.Category, Payload: p.Payload, Timestamp: p.Timestamp})
	}
	return out, nil
}

// cfBase is the flow clock; cfAt(n) is n minutes after it.
var cfBase = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

func cfAt(n int) time.Time { return cfBase.Add(time.Duration(n) * time.Minute) }

// cfDraft is one draft of a flow report: its cited issue numbers and title.
type cfDraft struct {
	issues []int
	title  string
	epic   string
}

func cfID(n int) string { return fmt.Sprintf("UR-issue-%d", n) }

func (d cfDraft) sources() []string {
	out := make([]string, len(d.issues))
	for i, n := range d.issues {
		out[i] = cfID(n)
	}
	return out
}

func (d cfDraft) id() string { return plan.CommsDraftID(d.sources()) }

// cfReport is one flow report's placements.
type cfReport struct {
	drafts     []cfDraft
	nDrift     []int // each its own N2 entry
	notDrafted []int
}

// body renders r as a comms_report citing csCharterDoc's ids (rubric V1,
// non-goal N2); a shown report it names nowhere is unaccounted.
func (r cfReport) body(t *testing.T) []byte {
	t.Helper()
	return commsExampleMutated(t, func(m map[string]any) {
		ds := []any{}
		for _, d := range r.drafts {
			src := []any{}
			for _, s := range d.sources() {
				src = append(src, s)
			}
			issue := map[string]any{"type": "bug", "title": d.title,
				"body": "Users report " + strings.ToLower(d.title) + ".", "labels": []any{"area:backend", "type:bug"}}
			if d.epic != "" {
				issue["parent_epic"] = d.epic
			}
			ds = append(ds, map[string]any{"id": d.id(), "source_report_ids": src,
				"rubric_citations": []any{map[string]any{"rubric_id": "V1"}}, "proposed_issue": issue})
		}
		m["drafts"] = ds
		nd := []any{}
		for _, n := range r.nDrift {
			nd = append(nd, map[string]any{"id": "ndrift:N2:" + cfID(n), "non_goal_id": "N2",
				"source_report_ids": []any{cfID(n)}, "note": "Requests a hosted offering."})
		}
		m["n_drift"] = nd
		nds := []any{}
		for _, n := range r.notDrafted {
			nds = append(nds, map[string]any{"report_id": cfID(n), "reason": "question"})
		}
		m["not_drafted"] = nds
	})
}

type cfFixture struct {
	s        *Server
	rr       *ukFlowRunRepo // CreateRun-counting: the apply must never create a run
	au       *cfAudit
	sf       *signingFake
	reader   *csReader
	cursors  *csCursors
	provider *cmaProvider
	now      time.Time
}

// newCFFixture wires the real prompt, ingest, capture and approval routes over
// shared fakes: a reader of external-author issues, a charter (csCharterDoc),
// conventions naming a File-only recording provider (the intake hook degrades
// reader_unavailable with no findings) and KEEPING the defaulted autonomy
// tier, and a cursor store whose issues cursor starts at cfAt(-60).
func newCFFixture(t *testing.T, issues map[int]int) *cfFixture {
	t.Helper()
	f := &cfFixture{
		rr: &ukFlowRunRepo{upkeepRunRepo: newUpkeepRunRepo()},
		au: &cfAudit{cmaAudit: &cmaAudit{ukApplyAudit: &ukApplyAudit{groomingApplyAuditFake: &groomingApplyAuditFake{
			approvalAuditFake: newApprovalAuditFake()}}}},
		sf:       newSigningFake(),
		reader:   &csReader{next: cfAt(300)},
		cursors:  &csCursors{vals: map[string]time.Time{csCursorKey(cfKey(userreport.SourceIssues)): cfAt(-60)}},
		provider: &cmaProvider{name: "comms-flow-fake-" + uuid.NewString()},
		now:      cfAt(400),
	}
	for n, minute := range issues {
		f.reader.items = append(f.reader.items, csIssue(n, fmt.Sprintf("Report %d", n),
			fmt.Sprintf("Something is wrong in area %d.", n), cfAt(minute), commsExternalAuthor))
	}
	workmgmt.Register(f.provider)
	conv := ukApplyConventions(f.provider.name, true)
	conv.Charter = &workmgmt.Charter{Path: igCharterPath}
	installConventions(t, conv, nil)

	prevReader, prevNow := commsUserReportReaderFor, commsScanNow
	commsUserReportReaderFor = func(string) (workmgmt.UserReportReader, error) { return f.reader, nil }
	commsScanNow = func() time.Time { return f.now }
	t.Cleanup(func() { commsUserReportReaderFor, commsScanNow = prevReader, prevNow })

	c := igCharterConfigWith(&igFetcher{content: csCharterDoc})
	c.Addr = "127.0.0.1:0"
	c.RunRepo, c.AuditRepo, c.ArtifactRepo = f.rr, f.au, newFakeArtifactRepo()
	c.ApprovalRepo, c.SigningRepo, c.UserReportCursors = newFakeApprovalRepo(), f.sf, f.cursors
	c.Logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	f.s = New(c)
	f.s.promptIssueGetterOverride = &stubIssueGetter{}
	return f
}

func cfKey(src userreport.Source) userreport.Key {
	return userreport.Key{Repo: commsTestRepo, Source: src}
}

// cfRun is one user-report-scan run with a running plan stage.
type cfRun struct {
	run   *run.Run
	stage *run.Stage
	priv  ed25519.PrivateKey
	shown []string
}

// newRun seeds an untenanted user-report-scan run (the route middleware
// 403s a tenanted run for the identity-less caller).
func (f *cfFixture) newRun(t *testing.T) *cfRun {
	t.Helper()
	inst := int64(42)
	rn := &run.Run{ID: uuid.New(), Repo: commsTestRepo, WorkflowID: "user_report_scan", WorkflowSpec: userReportScanSpec(t),
		State: run.StateRunning, InstallationID: &inst}
	f.rr.seedRun(rn)
	dispatched := f.now
	st := &run.Stage{ID: uuid.New(), RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		State: run.StageStateRunning, RequiresApproval: true, DispatchedAt: &dispatched}
	f.rr.getStages[st.ID] = st
	if f.rr.stagesByRunID == nil {
		f.rr.stagesByRunID = map[uuid.UUID][]*run.Stage{}
	}
	f.rr.stagesByRunID[rn.ID] = []*run.Stage{st}
	r := &cfRun{run: rn, stage: st}
	r.priv, _ = f.sf.issue(t, rn.ID)
	return r
}

// gather serves the SIGNED /prompt for r's plan stage (recording its gather)
// and returns the shown report ids the recorded row carries. The scan cache
// clock moves first, so every gather is fresh.
func (f *cfFixture) gather(t *testing.T, r *cfRun) []string {
	t.Helper()
	f.now = f.now.Add(2 * commsScanCacheTTL)
	if w := promptRequest(t, f.s, r.run.ID, r.stage.ID, r.priv, ""); w.Code != http.StatusOK {
		t.Fatalf("signed /prompt status = %d:\n%s", w.Code, w.Body.String())
	}
	_, p, err := f.s.latestCommsScanGathered(context.Background(), r.run.ID, r.stage.ID)
	if err != nil {
		t.Fatalf("latestCommsScanGathered: %v", err)
	}
	r.shown = nil
	for _, s := range p.Shown {
		r.shown = append(r.shown, s.ID)
	}
	sort.Strings(r.shown)
	return r.shown
}

// ingest ships report through the signed /plan route.
func (f *cfFixture) ingest(t *testing.T, r *cfRun, report cfReport) {
	t.Helper()
	if w := shipPlanRequest(t, f.s, r.run.ID, r.stage.ID, r.priv, report.body(t), ""); w.Code != http.StatusCreated {
		t.Fatalf("ingest status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
}

// dispose records the captain's verdicts through the real capture handler.
func (f *cfFixture) dispose(t *testing.T, r *cfRun, entries ...string) {
	t.Helper()
	decodeCMD(t, postCMD(t, f.s, r.run.ID.String(), cmdBatch(entries...), ukOperator))
}

// decide submits the gate decision through the real approval route, then
// waits on the detached apply.
func (f *cfFixture) decide(t *testing.T, r *cfRun, decision string) {
	t.Helper()
	w := submitApproval(t, f.s, r.stage.ID, fmt.Sprintf(`{"decision":%q,"comment":"comms gate"}`, decision))
	if w.Code != http.StatusOK {
		t.Fatalf("approval status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	f.s.waitReportApply()
}

func (f *cfFixture) payloads(t *testing.T, r *cfRun, category string) []json.RawMessage {
	t.Helper()
	rows, err := f.au.ListForRunByCategory(context.Background(), r.run.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	out := []json.RawMessage{}
	for _, e := range rows {
		out = append(out, e.Payload)
	}
	return out
}

func (f *cfFixture) completed(t *testing.T, r *cfRun) commsApplyCompletedPayload {
	t.Helper()
	rows := f.payloads(t, r, CategoryCommsApplyCompleted)
	if len(rows) != 1 {
		t.Fatalf("comms_apply_completed rows = %d, want 1", len(rows))
	}
	var c commsApplyCompletedPayload
	if err := json.Unmarshal(rows[0], &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *cfFixture) skips(t *testing.T, r *cfRun) map[string]commsDraftSkippedPayload {
	t.Helper()
	out := map[string]commsDraftSkippedPayload{}
	for _, raw := range f.payloads(t, r, CategoryCommsDraftSkipped) {
		var p commsDraftSkippedPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		out[p.DraftID] = p
	}
	return out
}

func (f *cfFixture) filed(t *testing.T, r *cfRun) map[string]commsDraftFiledPayload {
	t.Helper()
	out := map[string]commsDraftFiledPayload{}
	for _, raw := range f.payloads(t, r, CategoryCommsDraftFiled) {
		var p commsDraftFiledPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		out[p.DraftID] = p
	}
	return out
}

// previews decodes r's recorded previews by draft id.
func (f *cfFixture) previews(t *testing.T, r *cfRun) map[string]commsDraftPreview {
	t.Helper()
	rows := f.payloads(t, r, CategoryCommsReportRecorded)
	if len(rows) != 1 {
		t.Fatalf("comms_report_recorded rows = %d, want 1", len(rows))
	}
	var rec struct {
		Previews []commsDraftPreview `json:"previews"`
	}
	if err := json.Unmarshal(rows[0], &rec); err != nil {
		t.Fatal(err)
	}
	out := map[string]commsDraftPreview{}
	for _, p := range rec.Previews {
		out[p.DraftID] = p
	}
	return out
}

func (f *cfFixture) fileTitles() []string {
	out := []string{}
	for _, r := range f.provider.requests() {
		out = append(out, r.Item.Title)
	}
	sort.Strings(out)
	return out
}

func (f *cfFixture) cursor() time.Time {
	v, _, _ := f.cursors.Get(context.Background(), cfKey(userreport.SourceIssues))
	return v
}

func cfIDs(ns ...int) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, cfID(n))
	}
	sort.Strings(out)
	return out
}

// The main flow's reports and their update times (minutes after cfBase):
//
//	40  not_drafted              @1  (before the earliest held report)
//	60  D4 undecided             @2  (the EARLIEST held report)
//	12  D1 approved              @3
//	30, 31  D2 approved, epic #77 @4, @5
//	41  n_drift (N2)             @6
//	50  D3 rejected              @7
//	99  unaccounted              @8
var (
	cfD1 = cfDraft{issues: []int{12}, title: "CSV export writes no rows"}
	cfD2 = cfDraft{issues: []int{30, 31}, title: "Dashboard times out on large runs", epic: "#3726"}
	cfD3 = cfDraft{issues: []int{50}, title: "Login page rejects valid tokens"}
	cfD4 = cfDraft{issues: []int{60}, title: "Search ignores quoted phrases"}

	cfMainIssues = map[int]int{40: 1, 60: 2, 12: 3, 30: 4, 31: 5, 41: 6, 50: 7, 99: 8}
	cfMainReport = cfReport{drafts: []cfDraft{cfD1, cfD2, cfD3, cfD4}, nDrift: []int{41}, notDrafted: []int{40}}
)

// TestCommsFlow_GatherIngestDispositionsApproveFilesExactlyApproved: the
// approved drafts (D1, and D2 under the captain's #77 override) reach File
// with the body the captain reviewed and no autonomy label; the rejected (D3)
// and undecided (D4) drafts do not; nothing creates a run or touches a source;
// the completion row carries the suppressions and the held-back cursor; a
// later capture is refused 409; and a NEW run's gather through the real
// userreport.Scan shows the undecided draft's source and the unaccounted
// report again while the filed, rejected and n_drift sources stay suppressed.
func TestCommsFlow_GatherIngestDispositionsApproveFilesExactlyApproved(t *testing.T) {
	f := newCFFixture(t, cfMainIssues)
	r := f.newRun(t)
	if got := f.gather(t, r); !reflect.DeepEqual(got, cfIDs(12, 30, 31, 40, 41, 50, 60, 99)) {
		t.Fatalf("first gather shown = %v", got)
	}
	f.ingest(t, r, cfMainReport)
	f.dispose(t, r, cmdEntry(cfD1.id(), commsVerdictApproved), cmdEntryEpic(cfD2.id(), commsVerdictApproved, "#77"),
		cmdEntry(cfD3.id(), commsVerdictRejected))
	f.decide(t, r, "approve")

	if got := f.fileTitles(); !reflect.DeepEqual(got, []string{cfD1.title, cfD2.title}) {
		t.Fatalf("filed titles = %v, want exactly D1 and D2", got)
	}
	previews := f.previews(t, r)
	for _, req := range f.provider.requests() {
		d := cfD1
		if req.Item.Title == cfD2.title {
			d = cfD2
		}
		pv := previews[d.id()]
		if pv.CommsRenderedPreview == nil || req.Item.Body != pv.Body {
			t.Errorf("%s filed body differs from its recorded preview body:\nfiled   %q\npreview %+v", d.title, req.Item.Body, pv.CommsRenderedPreview)
		}
		for _, l := range req.Item.Classification.Labels {
			if commsIsAutonomyLabel(l) {
				t.Errorf("%s filed with %s", d.title, l)
			}
		}
	}
	filed := f.filed(t, r)
	if pe := filed[cfD2.id()].ParentEpic; pe != "#77" {
		t.Errorf("D2 parent epic = %q, want the captain's #77 override", pe)
	}
	if pe := filed[cfD1.id()].ParentEpic; pe != "" {
		t.Errorf("D1 parent epic = %q, want none", pe)
	}
	skips := f.skips(t, r)
	if skips[cfD3.id()].SkipReason != commsSkipRejected || skips[cfD4.id()].SkipReason != commsSkipUndecided || len(skips) != 2 {
		t.Errorf("skips = %+v, want D3 rejected and D4 undecided", skips)
	}

	c := f.completed(t, r)
	if c.Degraded || c.Counts != (commsApplyCounts{Drafts: 4, Filed: 2, Skipped: 2}) {
		t.Errorf("completion = degraded %v counts %+v", c.Degraded, c.Counts)
	}
	wantSups := []string{
		cfID(12) + "/filed/" + cfD1.id(), cfID(30) + "/filed/" + cfD2.id(), cfID(31) + "/filed/" + cfD2.id(),
		cfID(41) + "/n_drift/ndrift:N2:" + cfID(41), cfID(50) + "/rejected/" + cfD3.id(),
	}
	sort.Strings(wantSups)
	if got := supKeys(c.Suppressions); !reflect.DeepEqual(got, wantSups) {
		t.Errorf("suppressions = %v, want %v", got, wantSups)
	}
	if !c.Cursor.Advanced || !reflect.DeepEqual(c.Cursor.HeldReportIDs, cfIDs(60, 99)) {
		t.Errorf("cursor outcome = %+v, want advanced holding 60 and 99", c.Cursor)
	}
	if got := f.cursor(); !got.Equal(cfAt(2)) {
		t.Errorf("issues cursor = %v, want held back to the undecided source's %v", got, cfAt(2))
	}
	if n := f.rr.creates.Load(); n != 0 {
		t.Errorf("CreateRun calls = %d, want 0", n)
	}

	// The window is closed: a later capture is refused and records nothing.
	requireGDError(t, postCMD(t, f.s, r.run.ID.String(), cmdBatch(cmdEntry(cfD4.id(), commsVerdictApproved)), ukOperator),
		http.StatusConflict, "comms_window_closed")

	// The rescan: a NEW run over the same chain and cursor store.
	if got := f.gather(t, f.newRun(t)); !reflect.DeepEqual(got, cfIDs(60, 99)) {
		t.Errorf("rescan shown = %v, want only the undecided source and the unaccounted report %v", got, cfIDs(60, 99))
	}
}

// TestCommsFlow_RescanProposesNothingUnlessReportChanged: when every report
// is accounted for, the apply advances the cursor to the gather's pending
// value and the rescan shows nothing; editing one FILED report's body changes
// its content hash and updated_at, so it re-enters.
func TestCommsFlow_RescanProposesNothingUnlessReportChanged(t *testing.T) {
	f := newCFFixture(t, map[int]int{12: 3, 30: 4, 31: 5})
	r := f.newRun(t)
	f.gather(t, r)
	f.ingest(t, r, cfReport{drafts: []cfDraft{cfD1, cfD2}})
	f.dispose(t, r, cmdEntry(cfD1.id(), commsVerdictApproved), cmdEntry(cfD2.id(), commsVerdictApproved))
	f.decide(t, r, "approve")
	if c := f.completed(t, r); !c.Cursor.Advanced || len(c.Cursor.HeldReportIDs) != 0 || c.Counts.Filed != 2 {
		t.Fatalf("completion = %+v, want two filed and the cursor advanced with nothing held", c)
	}
	if got := f.cursor(); !got.Equal(cfAt(300)) {
		t.Fatalf("issues cursor = %v, want the gather's pending %v", got, cfAt(300))
	}
	if got := f.gather(t, f.newRun(t)); len(got) != 0 {
		t.Fatalf("rescan shown = %v, want nothing", got)
	}

	// The reporter edits issue 12 after the cursor.
	f.reader.mu.Lock()
	for i := range f.reader.items {
		if f.reader.items[i].IssueNumber == 12 {
			f.reader.items[i].Body = "Still broken after the last release."
			f.reader.items[i].UpdatedAt = cfAt(301)
		}
	}
	f.reader.next = cfAt(310)
	f.reader.mu.Unlock()
	if got := f.gather(t, f.newRun(t)); !reflect.DeepEqual(got, cfIDs(12)) {
		t.Errorf("rescan after an edit shown = %v, want the edited report %v", got, cfIDs(12))
	}
}

// TestCommsFlow_GateRejectRescan: a gate REJECT through the approval route
// files nothing and never moves the cursor. With per-draft dispositions only
// the rejected draft's sources are suppressed on the rescan; with no
// disposition at all (a whole-gate reject) every draft's sources are.
func TestCommsFlow_GateRejectRescan(t *testing.T) {
	cases := map[string]struct {
		entries []string
		want    []string
	}{
		// D3 rejected: 50 suppressed; D1, D2, D4 re-proposed with the rest.
		"per-draft": {
			entries: []string{cmdEntry(cfD1.id(), commsVerdictApproved), cmdEntry(cfD3.id(), commsVerdictRejected)},
			want:    cfIDs(12, 30, 31, 40, 41, 60, 99),
		},
		// No disposition: every draft's sources suppressed; the n_drift,
		// not_drafted and unaccounted reports are re-proposed.
		"whole-gate": {want: cfIDs(40, 41, 99)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCFFixture(t, cfMainIssues)
			r := f.newRun(t)
			f.gather(t, r)
			f.ingest(t, r, cfMainReport)
			if len(tc.entries) > 0 {
				f.dispose(t, r, tc.entries...)
			}
			before := f.cursor()
			f.decide(t, r, "reject")

			if n := len(f.provider.requests()); n != 0 {
				t.Errorf("File calls = %d on a rejected gate, want 0", n)
			}
			c := f.completed(t, r)
			if c.Decision != commsApplyDecisionReject || c.Cursor.Advanced || c.Cursor.Reason != commsCursorReasonDecisionReject {
				t.Errorf("completion = %+v, want the reject row with the cursor untouched", c)
			}
			if got := f.cursor(); !got.Equal(before) {
				t.Errorf("issues cursor moved on a reject: %v -> %v", before, got)
			}
			if got := f.gather(t, f.newRun(t)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rescan shown = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCommsFlow_OverlappingScansPartialClusters (#4014 obligation 2): runs 1
// and 2 both gather before either applies. Run 1 files {R1, R2} and leaves R3
// unaccounted; run 2's {R1, R2, R3} draft is skipped filed_elsewhere naming
// R1 and R2, and nothing is filed twice. The rescan shows R3 and not R1/R2.
func TestCommsFlow_OverlappingScansPartialClusters(t *testing.T) {
	// R3 (issue 3) is the EARLIEST, so the rescan re-reads R1 and R2 and the
	// suppression memory, not the cursor, is what keeps them out.
	f := newCFFixture(t, map[int]int{3: 1, 1: 2, 2: 3})
	r1, r2 := f.newRun(t), f.newRun(t)
	f.gather(t, r1)
	f.gather(t, r2)
	d12 := cfDraft{issues: []int{1, 2}, title: "Sync drops edits"}
	d123 := cfDraft{issues: []int{1, 2, 3}, title: "Sync drops edits and attachments"}
	f.ingest(t, r1, cfReport{drafts: []cfDraft{d12}})
	f.ingest(t, r2, cfReport{drafts: []cfDraft{d123}})
	f.dispose(t, r1, cmdEntry(d12.id(), commsVerdictApproved))
	f.dispose(t, r2, cmdEntry(d123.id(), commsVerdictApproved))
	f.decide(t, r1, "approve")
	f.decide(t, r2, "approve")

	if got := f.fileTitles(); !reflect.DeepEqual(got, []string{d12.title}) {
		t.Fatalf("filed titles = %v, want only run 1's draft", got)
	}
	sk := f.skips(t, r2)[d123.id()]
	var conflicting []string
	for _, c := range sk.ConflictingReports {
		conflicting = append(conflicting, c.ID)
	}
	sort.Strings(conflicting)
	if sk.SkipReason != commsSkipFiledElsewhere || !reflect.DeepEqual(conflicting, cfIDs(1, 2)) {
		t.Errorf("run 2 skip = %+v, want filed_elsewhere naming R1 and R2", sk)
	}
	if got := f.gather(t, f.newRun(t)); !reflect.DeepEqual(got, cfIDs(3)) {
		t.Errorf("rescan shown = %v, want only R3", got)
	}
}
