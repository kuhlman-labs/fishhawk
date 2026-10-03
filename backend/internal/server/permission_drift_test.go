package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/permdrift"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The permission-drift check (E80.4 / #3761), server half. Every arm drives
// the REAL githubclient against an httptest GitHub (driftGH) serving /compare
// and /contents?ref=, and reads COMMITTED STATE back — concern rows from the
// concern store, entries from the audit fake — never only a log line.
//
// COUNTERFACTUAL CONVENTION (approval condition 6): every arm guarding a
// control names the mutation that removes it and the fixture state that makes
// the removal observable.

const (
	driftWFPath  = ".github/workflows/ci.yml"
	driftSpecRef = ".fishhawk/workflows.yaml"
	driftBase    = "base0000"
	driftHead    = "head1111"
)

// driftWF renders a workflow whose top-level permissions block is perms (a
// YAML map body, two-space indented under `permissions:`).
func driftWF(perms string) string {
	return "name: ci\non: push\npermissions:\n" + perms + "jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps: []\n"
}

var (
	wfBase     = driftWF("  contents: read\n  issues: write\n")
	wfWidened  = driftWF("  contents: write\n  issues: write\n")
	wfNarrowed = driftWF("  contents: read\n")
	wfReorder  = driftWF("  issues: write\n  contents: read\n")
)

// wfWideningKey is the check key of wfBase → wfWidened.
var wfWideningKey = permdrift.CheckKey("gha-workflow-permissions", driftWFPath, "jobs.build.contents", "write")

// driftSpec is a minimal workflow-v2 spec with the given forbidden_paths.
func driftSpec(forbidden string) string {
	return `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        constraints:
          forbidden_paths: ` + forbidden + "\n"
}

// driftGH is a fake GitHub for the check: compare lists `changed`, contents
// serves files[ref][path] (absent → 404). Bookkeeping is mutex-guarded because
// the check runs on a background goroutine (#3226).
type driftGH struct {
	mu            sync.Mutex
	files         map[string]map[string]string
	changed       []string
	renamed       bool              // every changed file is "renamed" with no previous_filename
	renamedFrom   map[string]string // changed path → previous_filename (a "renamed" row)
	headSHA       string            // the compare's head commit ("" → driftHead)
	truncated     bool
	compareStatus int
	contentStatus map[string]int // "ref|path" → status
	calls         int
}

func newDriftGH() *driftGH {
	return &driftGH{files: map[string]map[string]string{}, contentStatus: map[string]int{}}
}

func (g *driftGH) put(ref, path, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.files[ref] == nil {
		g.files[ref] = map[string]string{}
	}
	g.files[ref][path] = content
}

func (g *driftGH) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *driftGH) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	if i := strings.Index(p, "/compare/"); i >= 0 {
		if g.compareStatus != 0 {
			w.WriteHeader(g.compareStatus)
			_, _ = w.Write([]byte(`{"message":"injected"}`))
			return
		}
		files := []map[string]any{}
		for _, c := range g.changed {
			row := map[string]any{"filename": c, "status": "modified", "changes": 2, "patch": "@@ -1 +1 @@\n-a\n+b"}
			if g.renamed {
				row["status"] = "renamed"
			}
			if from, ok := g.renamedFrom[c]; ok {
				row["status"], row["previous_filename"] = "renamed", from
			}
			files = append(files, row)
		}
		if g.truncated { // a changed file with an omitted patch body → Truncated
			files = append(files, map[string]any{"filename": "big.bin", "status": "modified", "changes": 9})
		}
		head := g.headSHA
		if head == "" {
			head = driftHead
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_commits": 1, "commits": []map[string]string{{"sha": head}}, "files": files,
		})
		return
	}
	if i := strings.Index(p, "/contents/"); i >= 0 {
		path := p[i+len("/contents/"):]
		ref := r.URL.Query().Get("ref")
		if st := g.contentStatus[ref+"|"+path]; st != 0 {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"message":"injected"}`))
			return
		}
		c, ok := g.files[ref][path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": path, "sha": "blob", "type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(c)),
		})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (g *driftGH) client(t *testing.T) *githubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  driftTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// driftTokens is a stateless (so race-free) token provider: the check's
// goroutine and a review goroutine may share one client.
type driftTokens struct{}

func (driftTokens) Token(context.Context, int64) (string, error) { return "ghs_t", nil }

// driftSeqAudit stamps a monotonically increasing Sequence on every append, so
// the origin-sequence link between the detected entry and the concerns is
// observable (auditFake returns Sequence 0 for every entry).
type driftSeqAudit struct {
	*auditFake
	seqMu sync.Mutex
	seq   int64
	byCat map[string][]int64
}

func (a *driftSeqAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	e, err := a.auditFake.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	a.seqMu.Lock()
	defer a.seqMu.Unlock()
	a.seq++
	e.Sequence = a.seq
	a.byCat[p.Category] = append(a.byCat[p.Category], a.seq)
	return e, nil
}

// flakyConcernRepo fails the first `failures` InsertRaised calls.
type flakyConcernRepo struct {
	*fakeConcernRepo
	flakyMu  sync.Mutex
	failures int
	calls    int
}

func (f *flakyConcernRepo) InsertRaised(ctx context.Context, p concern.InsertRaisedParams) ([]*concern.Concern, error) {
	f.flakyMu.Lock()
	f.calls++
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	f.flakyMu.Unlock()
	if fail {
		return nil, errors.New("concern store unavailable")
	}
	return f.fakeConcernRepo.InsertRaised(ctx, p)
}

type driftFixture struct {
	s      *Server
	gh     *driftGH
	au     *driftSeqAudit
	cr     *flakyConcernRepo
	rr     *orchestratorRepo
	sf     *signingFake
	runRow *run.Run
	stage  *run.Stage
}

func newDriftFixture(t *testing.T) *driftFixture {
	t.Helper()
	gh := newDriftGH()
	rr := newOrchestratorRepo()
	au := &driftSeqAudit{auditFake: newAuditFake(), byCat: map[string][]int64{}}
	cr := &flakyConcernRepo{fakeConcernRepo: newFakeConcernRepo()}
	sf := newSigningFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: newFakeArtifactRepo(),
		AuditRepo:    au,
		RunRepo:      rr,
		ConcernRepo:  cr,
		GitHub:       gh.client(t),
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	runRow := rr.seedRun()
	runRow.Repo = "acme/widgets"
	inst := int64(55)
	runRow.InstallationID = &inst
	stage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	stage.Type = run.StageTypeImplement
	stage.RequiresApproval = true
	return &driftFixture{s: s, gh: gh, au: au, cr: cr, rr: rr, sf: sf, runRow: runRow, stage: stage}
}

// workflowChange seeds ci.yml at driftBase/driftHead and lists it as changed.
func (f *driftFixture) workflowChange(base, head string) {
	if base != "" {
		f.gh.put(driftBase, driftWFPath, base)
	}
	if head != "" {
		f.gh.put(driftHead, driftWFPath, head)
	}
	f.gh.changed = []string{driftWFPath}
}

func (f *driftFixture) req(trigger string) permissionDriftRequest {
	return permissionDriftRequest{RunID: f.runRow.ID, StageID: f.stage.ID, Base: driftBase, Head: driftHead, Trigger: trigger}
}

// run executes the check synchronously.
func (f *driftFixture) run(req permissionDriftRequest) {
	f.s.runPermissionDriftCheck(context.Background(), req)
}

func (f *driftFixture) rows(t *testing.T) []*concern.Concern {
	t.Helper()
	return serverCheckRows(t, f.cr.fakeConcernRepo, f.runRow.ID)
}

func (f *driftFixture) detected(t *testing.T) []permissionDriftDetectedPayload {
	t.Helper()
	var out []permissionDriftDetectedPayload
	for _, ap := range driftEntries(f.au.auditFake, permissionDriftDetectedCategory) {
		var p permissionDriftDetectedPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", permissionDriftDetectedCategory, err)
		}
		out = append(out, p)
	}
	return out
}

func (f *driftFixture) count(category string) int {
	return len(driftEntries(f.au.auditFake, category))
}

// unevaluableReasons returns the reason of every unevaluable item across all
// detected entries, keyed by surface.
func (f *driftFixture) unevaluable(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range f.detected(t) {
		for _, u := range p.Unevaluable {
			out[u.Surface] = u.Reason
		}
	}
	return out
}

// driftEntries returns the appended entries of one category.
func driftEntries(au *auditFake, category string) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, e := range au.appended {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out
}

func rowWithKey(rows []*concern.Concern, key string) *concern.Concern {
	for _, r := range rows {
		if r.CheckKey == key {
			return r
		}
	}
	return nil
}

// TestPermissionDrift_PullRequestReportToHumanWaive is the cross-boundary end
// to end: the real /pull-request PR-opened handler → background check → forge
// compare + contents → concern store → audit log → waive handler.
//
// COUNTERFACTUAL (refuseNonHumanServerCheckClear's body mutated to `return
// false`): the fixture's concern is server_check and the caller holds an
// operator-agent token, so without the guard the waive returns 200 and the
// row READ BACK after the call is waived → RED. COUNTERFACTUAL (the
// checkPermissionDrift call in the PR-opened path deleted): the fixture's only
// changed file widens contents read→write, so with no call no row exists → RED.
func TestPermissionDrift_PullRequestReportToHumanWaive(t *testing.T) {
	f := newDriftFixture(t)
	f.workflowChange(wfBase, wfWidened)

	body, _ := json.Marshal(pullRequestBody{
		PRNumber: 7, PRURL: "https://github.com/acme/widgets/pull/7", Branch: "fishhawk/run-x",
		HeadSHA: driftHead, BaseSHA: driftBase, Title: "t", FilesChangedCount: 1,
	})
	priv, _ := f.sf.issue(t, f.runRow.ID)
	if w := shipPRRequest(t, f.s, f.runRow.ID, f.stage.ID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d:\n%s", w.Code, w.Body.String())
	}
	f.s.waitBackgroundReviews()

	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("server_check rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Severity != "high" || row.Category != "security" || row.CheckKey != wfWideningKey {
		t.Errorf("row severity/category/check_key = %q/%q/%q, want high/security/%q", row.Severity, row.Category, row.CheckKey, wfWideningKey)
	}
	for _, want := range []string{"jobs.build.contents", "from read to write", driftWFPath, "only a human"} {
		if !strings.Contains(strings.ToLower(row.Note), strings.ToLower(want)) {
			t.Errorf("note missing %q:\n%s", want, row.Note)
		}
	}
	payloads := f.detected(t)
	if len(payloads) != 1 {
		t.Fatalf("%s entries = %d, want 1", permissionDriftDetectedCategory, len(payloads))
	}
	p := payloads[0]
	if p.Trigger != permissionDriftTriggerPROpened || p.BaseSHA != driftBase || p.HeadSHA != driftHead || p.SurfacesVersion != permdrift.SurfacesVersion {
		t.Errorf("payload trigger/base/head/version = %q/%q/%q/%d", p.Trigger, p.BaseSHA, p.HeadSHA, p.SurfacesVersion)
	}
	if len(p.Widenings) != 1 || p.Widenings[0].Before != "read" || p.Widenings[0].After != "write" || p.Widenings[0].CheckKey != wfWideningKey {
		t.Errorf("payload widenings = %+v", p.Widenings)
	}
	f.au.seqMu.Lock()
	seqs := f.au.byCat[permissionDriftDetectedCategory]
	f.au.seqMu.Unlock()
	if len(seqs) != 1 || row.OriginReviewSequence != seqs[0] {
		t.Errorf("origin_review_sequence = %d, want the detected entry's sequence %v", row.OriginReviewSequence, seqs)
	}

	if w := postWaiveAs(t, f.s, row.ID.String(), waiveConcernRequest{Reason: "agent says intended"}, withOperatorAgentAuth); w.Code != http.StatusForbidden {
		t.Errorf("agent waive status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	assertState(t, f.cr.fakeConcernRepo, row.ID, concern.StateRaised)

	if w := postWaiveAs(t, f.s, row.ID.String(), waiveConcernRequest{Reason: "intended permission grant"}, withAuth); w.Code != http.StatusOK {
		t.Fatalf("human waive status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertState(t, f.cr.fakeConcernRepo, row.ID, concern.StateWaived)
}

// TestPermissionDrift_FixupPushedRaises: the fixup_pushed report checks the
// pass delta (previous head → pushed head).
//
// COUNTERFACTUAL (the checkPermissionDrift call in succeedFixupPushStage
// deleted): the pass delta widens contents, so no row exists → RED.
func TestPermissionDrift_FixupPushedRaises(t *testing.T) {
	f := newDriftFixture(t)
	f.workflowChange(wfBase, wfWidened)
	body, _ := json.Marshal(map[string]any{
		"outcome": "fixup_pushed", "branch": "fishhawk/run-x", "head_sha": driftHead, "base_sha": driftBase, "files_changed_count": 1,
	})
	priv, _ := f.sf.issue(t, f.runRow.ID)
	if w := shipPRRequest(t, f.s, f.runRow.ID, f.stage.ID, priv, body, ""); w.Code != http.StatusOK {
		t.Fatalf("ship status = %d:\n%s", w.Code, w.Body.String())
	}
	f.s.waitBackgroundReviews()
	if rows := f.rows(t); len(rows) != 1 || rows[0].CheckKey != wfWideningKey {
		t.Fatalf("rows = %d, want 1 with %q", len(rows), wfWideningKey)
	}
	if p := f.detected(t); len(p) != 1 || p[0].Trigger != permissionDriftTriggerFixupPushed {
		t.Errorf("detected = %+v, want one fixup_pushed entry", p)
	}
}

// TestPermissionDrift_ConsolidatedReviewRaises: a decomposed parent's
// consolidated review checks parent base → consolidated head.
//
// COUNTERFACTUAL (the checkPermissionDrift call in DispatchConsolidatedReview
// deleted): the consolidated delta widens contents, so no row exists → RED.
func TestPermissionDrift_ConsolidatedReviewRaises(t *testing.T) {
	f := newDriftFixture(t)
	f.workflowChange(wfBase, wfWidened)
	parent, implStage := seedConsolidatedParent(t, f.rr, newFakeArtifactRepo(), specImplementGatingReviewers)
	parent.Repo = "acme/widgets"

	f.s.DispatchConsolidatedReview(context.Background(), parent.ID, driftBase, driftHead)
	f.s.waitBackgroundReviews()

	rows := serverCheckRows(t, f.cr.fakeConcernRepo, parent.ID)
	if len(rows) != 1 || rows[0].CheckKey != wfWideningKey || rows[0].StageID != implStage.ID {
		t.Fatalf("rows = %+v, want one %q row on the parent implement stage", rows, wfWideningKey)
	}
}

// seedConflictTrigger records the stage's conflict-resolution trigger naming
// the base branch.
func (f *driftFixture) seedConflictTrigger(t *testing.T, baseRef string) {
	t.Helper()
	payload, _ := json.Marshal(conflictResolutionTrigger{Branch: "fishhawk/run-x", BaseRef: baseRef, ExpectedHeadSHA: driftBase, Pass: 1})
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runRow.ID, StageID: &f.stage.ID, Timestamp: time.Now().UTC(),
		Category: CategoryStageConflictResolutionTriggered, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPermissionDrift_ConflictResolutionIntersects (approval condition 2): a
// conflict-resolution push is checked, and only widenings present in BOTH
// previous-head → pushed-head AND base-branch-tip → pushed-head raise.
//
// COUNTERFACTUAL (drop the intersection: the `if req.IntersectRef != ""` block
// in runPermissionDriftCheck mutated to never run): the main-introduced arm's
// fixture has main's tip ALREADY at contents: write, so only the first
// comparison sees the widening; without the intersection it raises one row → RED.
// The resolution-introduced arm is the control that the intersection keeps a
// real widening (main still at read, so both comparisons see it).
func TestPermissionDrift_ConflictResolutionIntersects(t *testing.T) {
	const mainRef = "main"
	t.Run("main-introduced widening is dropped", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.put(mainRef, driftWFPath, wfWidened)
		f.seedConflictTrigger(t, mainRef)
		pr := &pullRequestBody{BaseSHA: driftBase, HeadSHA: driftHead}
		f.run(f.s.conflictResolutionDriftRequest(context.Background(), f.runRow.ID, f.stage.ID, pr))
		if rows := f.rows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0 (the base branch introduced the widening)", len(rows))
		}
	})
	t.Run("resolution-introduced widening raises", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.put(mainRef, driftWFPath, wfBase)
		f.seedConflictTrigger(t, mainRef)
		pr := &pullRequestBody{BaseSHA: driftBase, HeadSHA: driftHead}
		f.run(f.s.conflictResolutionDriftRequest(context.Background(), f.runRow.ID, f.stage.ID, pr))
		if rows := f.rows(t); len(rows) != 1 || rows[0].CheckKey != wfWideningKey {
			t.Fatalf("rows = %d, want 1 (the resolution introduced the widening)", len(rows))
		}
		if p := f.detected(t); len(p) != 1 || p[0].IntersectRef != mainRef {
			t.Errorf("detected = %+v, want intersect_ref %q", p, mainRef)
		}
	})
	// The base branch cannot be named (no trigger entry): the single comparison
	// runs and records intersect_unresolved — base-branch widenings raise too
	// (noise, never a miss). COUNTERFACTUAL (return without checking when the
	// trigger is unreadable): the fixture's widening then raises nothing → RED.
	t.Run("unresolved base branch checks the full delta", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.put(mainRef, driftWFPath, wfWidened)
		pr := &pullRequestBody{BaseSHA: driftBase, HeadSHA: driftHead}
		f.run(f.s.conflictResolutionDriftRequest(context.Background(), f.runRow.ID, f.stage.ID, pr))
		if rows := f.rows(t); len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if p := f.detected(t); len(p) != 1 || !p[0].IntersectUnresolved {
			t.Errorf("detected = %+v, want intersect_unresolved", p)
		}
	})
	// The base-branch side unreadable → the pair fails CLOSED.
	t.Run("base branch fetch failure is unevaluable", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.contentStatus[mainRef+"|"+driftWFPath] = http.StatusInternalServerError
		f.seedConflictTrigger(t, mainRef)
		pr := &pullRequestBody{BaseSHA: driftBase, HeadSHA: driftHead}
		f.run(f.s.conflictResolutionDriftRequest(context.Background(), f.runRow.ID, f.stage.ID, pr))
		if got := f.unevaluable(t)["gha-workflow-permissions"]; got != permdrift.ReasonFetchFailed {
			t.Errorf("unevaluable = %q, want fetch_failed", got)
		}
	})
	// COUNTERFACTUAL (the checkPermissionDrift call in
	// succeedConflictResolutionPushStage deleted): the push widens contents
	// against an unchanged main, so no row exists → RED.
	t.Run("handler call site", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.put(mainRef, driftWFPath, wfBase)
		f.seedConflictTrigger(t, mainRef)
		body, _ := json.Marshal(map[string]any{
			"outcome": "conflict_resolution_pushed", "branch": "fishhawk/run-x", "head_sha": driftHead, "base_sha": driftBase,
		})
		priv, _ := f.sf.issue(t, f.runRow.ID)
		if w := shipPRRequest(t, f.s, f.runRow.ID, f.stage.ID, priv, body, ""); w.Code != http.StatusOK {
			t.Fatalf("ship status = %d:\n%s", w.Code, w.Body.String())
		}
		f.s.waitBackgroundReviews()
		if rows := f.rows(t); len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if p := f.detected(t); len(p) != 1 || p[0].Trigger != permissionDriftTriggerConflictResolution {
			t.Errorf("detected = %+v", p)
		}
	})
}

// TestPermissionDrift_NarrowingOnlyIsNotice: a removed permission is a
// narrowing — one permission_narrowing_noticed entry, ZERO concerns.
//
// COUNTERFACTUAL (route narrowings into the raise list): the fixture's only
// change is a removed permission, so any concern row is the defect → RED.
func TestPermissionDrift_NarrowingOnlyIsNotice(t *testing.T) {
	f := newDriftFixture(t)
	f.workflowChange(wfBase, wfNarrowed)
	f.run(f.req(permissionDriftTriggerPROpened))
	if rows := f.rows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
	if n := f.count(permissionNarrowingNoticedCategory); n != 1 {
		t.Errorf("%s entries = %d, want 1", permissionNarrowingNoticedCategory, n)
	}
	if n := f.count(permissionDriftDetectedCategory); n != 0 {
		t.Errorf("%s entries = %d, want 0", permissionDriftDetectedCategory, n)
	}
}

// TestPermissionDrift_ReorderIsSilent: a reordered permissions block appends
// nothing at all.
func TestPermissionDrift_ReorderIsSilent(t *testing.T) {
	f := newDriftFixture(t)
	f.workflowChange(wfBase, wfReorder)
	f.run(f.req(permissionDriftTriggerPROpened))
	f.au.mu.Lock()
	n := len(f.au.appended)
	f.au.mu.Unlock()
	if n != 0 || len(f.rows(t)) != 0 {
		t.Errorf("entries = %d rows = %d, want 0/0", n, len(f.rows(t)))
	}
}

// TestPermissionDrift_DedupeOnStage: a second check of the same widening on
// the same stage inserts nothing, while the row is open OR waived.
//
// COUNTERFACTUAL (delete the `!recorded[w.CheckKey]` filter): the fixture
// already holds one row with the same key on the same stage, so the read-back
// count becomes 2 → RED.
func TestPermissionDrift_DedupeOnStage(t *testing.T) {
	f := newDriftFixture(t)
	f.workflowChange(wfBase, wfWidened)
	f.run(f.req(permissionDriftTriggerPROpened))
	f.run(f.req(permissionDriftTriggerFixupPushed))
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows after two checks = %d, want 1", len(rows))
	}
	if n := f.count(permissionDriftDetectedCategory); n != 1 {
		t.Errorf("detected entries = %d, want 1 (a fully de-duplicated check appends nothing)", n)
	}
	f.cr.mu.Lock()
	rows[0].State = concern.StateWaived
	f.cr.mu.Unlock()
	f.run(f.req(permissionDriftTriggerFixupPushed))
	if n := len(f.rows(t)); n != 1 {
		t.Errorf("rows after a waive + recheck = %d, want 1", n)
	}
	f.cr.mu.Lock()
	rows[0].State = concern.StateSuperseded
	f.cr.mu.Unlock()
	f.run(f.req(permissionDriftTriggerFixupPushed))
	if n := len(f.rows(t)); n != 2 {
		t.Errorf("rows after a supersede + recheck = %d, want 2 (superseded does not suppress)", n)
	}
}

// TestPermissionDrift_ExtensionReadAtBase: the repository's surface extension
// is read at BASE only, so a change cannot delete its own surface.
//
// COUNTERFACTUAL (fetch the extension at req.Head): the head-side extension no
// longer declares infra/app.json, so its widening is never evaluated → the
// infra-app row is missing → RED.
func TestPermissionDrift_ExtensionReadAtBase(t *testing.T) {
	f := newDriftFixture(t)
	ext := "version: 1\nsurfaces:\n  - id: infra-app\n    kind: github_app_permissions_json\n    paths: [\"infra/app.json\"]\n"
	f.gh.put(driftBase, permdrift.RepoSurfacesPath, ext)
	f.gh.put(driftHead, permdrift.RepoSurfacesPath, "version: 1\nsurfaces: []\n")
	f.gh.put(driftBase, "infra/app.json", `{"default_permissions":{"contents":"read"}}`)
	f.gh.put(driftHead, "infra/app.json", `{"default_permissions":{"contents":"write"}}`)
	f.gh.changed = []string{permdrift.RepoSurfacesPath, "infra/app.json"}
	f.run(f.req(permissionDriftTriggerPROpened))

	rows := f.rows(t)
	var infra, decl bool
	for _, r := range rows {
		infra = infra || strings.HasPrefix(r.CheckKey, "permission_drift|infra-app|infra/app.json|")
		decl = decl || strings.HasPrefix(r.CheckKey, "permission_drift|"+permdrift.SurfaceIDSurfaceDeclarations+"|")
	}
	if !infra || !decl {
		t.Errorf("rows = %d (infra-app %v, declarations %v), want both", len(rows), infra, decl)
	}
}

// TestPermissionDrift_FailureModes: one arm per named failure mode.
func TestPermissionDrift_FailureModes(t *testing.T) {
	ctx := context.Background()

	t.Run("nil ConcernRepo is a no-op", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.s.cfg.ConcernRepo = nil
		f.s.checkPermissionDrift(ctx, f.req(permissionDriftTriggerPROpened))
		f.s.waitBackgroundReviews()
		if f.gh.callCount() != 0 || f.count(permissionDriftDetectedCategory) != 0 {
			t.Errorf("calls = %d detected = %d, want 0/0", f.gh.callCount(), f.count(permissionDriftDetectedCategory))
		}
	})
	t.Run("nil AuditRepo is a no-op", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.s.cfg.AuditRepo = nil
		f.s.checkPermissionDrift(ctx, f.req(permissionDriftTriggerPROpened))
		f.s.waitBackgroundReviews()
		if f.gh.callCount() != 0 || len(f.rows(t)) != 0 {
			t.Errorf("calls = %d rows = %d, want 0/0", f.gh.callCount(), len(f.rows(t)))
		}
	})
	// Item 9. COUNTERFACTUAL (the commit_missing raise in
	// runPermissionDriftCheck mutated back to a bare `return`): the request
	// carries no head, so nothing else can mint a row → zero rows → RED. The
	// raise happens before any forge call, so the forge sees zero requests.
	t.Run("missing head raises commit_missing", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		r := f.req(permissionDriftTriggerPROpened)
		r.Head = ""
		f.s.checkPermissionDrift(ctx, r)
		f.s.waitBackgroundReviews()
		if f.gh.callCount() != 0 {
			t.Errorf("calls = %d, want 0", f.gh.callCount())
		}
		rows := f.rows(t)
		if len(rows) != 1 || rows[0].CheckKey != permdrift.UnevaluableKey(permissionDriftAllSurfaces.ID, driftBase+"..", "") {
			t.Fatalf("rows = %+v, want one commit_missing row", rows)
		}
		if got := f.unevaluable(t)[permissionDriftAllSurfaces.ID]; got != permdrift.ReasonCommitMissing {
			t.Errorf("reason = %q, want commit_missing", got)
		}
	})
	t.Run("missing base raises commit_missing", func(t *testing.T) {
		f := newDriftFixture(t)
		r := f.req(permissionDriftTriggerFixupPushed)
		r.Base = ""
		f.run(r)
		if got := f.unevaluable(t)[permissionDriftAllSurfaces.ID]; got != permdrift.ReasonCommitMissing || f.gh.callCount() != 0 {
			t.Errorf("reason = %q calls = %d, want commit_missing/0", got, f.gh.callCount())
		}
	})
	t.Run("get run failure is skipped", func(t *testing.T) {
		f := newDriftFixture(t)
		r := f.req(permissionDriftTriggerPROpened)
		r.RunID = uuid.New()
		f.run(r)
		if f.gh.callCount() != 0 {
			t.Errorf("calls = %d, want 0", f.gh.callCount())
		}
	})
	t.Run("forge not wired appends nothing", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.s.cfg.GitHub = nil
		f.run(f.req(permissionDriftTriggerPROpened))
		if f.count(permissionDriftDetectedCategory) != 0 || len(f.rows(t)) != 0 {
			t.Error("a check without a forge appended or raised")
		}
	})
	t.Run("forge without file fetch appends nothing", func(t *testing.T) {
		f := newDriftFixture(t)
		ref := "gitlab:5"
		f.runRow.InstallationRef = &ref
		f.s.cfg.ForgeResolver = func(string) (forge.Forge, error) {
			return &fakeCompareForge{name: "gitlab", result: oneFileCompareResult()}, nil
		}
		f.run(f.req(permissionDriftTriggerPROpened))
		if f.count(permissionDriftDetectedCategory) != 0 || len(f.rows(t)) != 0 {
			t.Error("a check without a file fetcher appended or raised")
		}
	})
	// COUNTERFACTUAL (return early on a compare error instead of raising): no
	// row exists although the change was never evaluated → RED.
	t.Run("compare error raises compare_failed", func(t *testing.T) {
		f := newDriftFixture(t)
		f.gh.compareStatus = http.StatusInternalServerError
		f.run(f.req(permissionDriftTriggerPROpened))
		rows := f.rows(t)
		if len(rows) != 1 || rows[0].CheckKey != permdrift.UnevaluableKey(permissionDriftAllSurfaces.ID, driftBase+".."+driftHead, driftHead) {
			t.Fatalf("rows = %+v, want one unevaluable row", rows)
		}
		if got := f.unevaluable(t)[permissionDriftAllSurfaces.ID]; got != permdrift.ReasonCompareFailed {
			t.Errorf("reason = %q, want compare_failed", got)
		}
	})
	// COUNTERFACTUAL (map a non-404 fetch error to an absent side): an absent
	// head reads as narrowing and raises nothing → RED.
	t.Run("head fetch 500 raises fetch_failed", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.contentStatus[driftHead+"|"+driftWFPath] = http.StatusInternalServerError
		f.run(f.req(permissionDriftTriggerPROpened))
		if got := f.unevaluable(t)["gha-workflow-permissions"]; got != permdrift.ReasonFetchFailed {
			t.Errorf("reason = %q, want fetch_failed", got)
		}
		if rows := f.rows(t); len(rows) != 1 || rows[0].CheckKey != permdrift.UnevaluableKey("gha-workflow-permissions", driftWFPath, driftHead) {
			t.Errorf("rows = %+v", rows)
		}
	})
	t.Run("base 404 raises the added file's grants", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange("", wfWidened)
		f.run(f.req(permissionDriftTriggerPROpened))
		if rowWithKey(f.rows(t), wfWideningKey) == nil {
			t.Errorf("rows = %+v, want the added file's contents: write raised", f.rows(t))
		}
	})
	t.Run("head 404 on the spec widens forbidden_paths", func(t *testing.T) {
		f := newDriftFixture(t)
		f.gh.put(driftBase, driftSpecRef, driftSpec(`[".github/workflows/**"]`))
		f.gh.changed = []string{driftSpecRef}
		f.run(f.req(permissionDriftTriggerPROpened))
		var found bool
		for _, r := range f.rows(t) {
			found = found || strings.HasPrefix(r.CheckKey, "permission_drift|"+permdrift.SurfaceIDForbiddenPaths+"|")
		}
		if !found {
			t.Errorf("rows = %+v, want a forbidden_paths widening", f.rows(t))
		}
	})
	t.Run("unparseable head raises parse_error", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, "jobs: [unclosed\n")
		f.run(f.req(permissionDriftTriggerPROpened))
		if got := f.unevaluable(t)["gha-workflow-permissions"]; got != permdrift.ReasonParseError {
			t.Errorf("reason = %q, want parse_error", got)
		}
	})
	// COUNTERFACTUAL (skip the exact-path probe on truncation): the truncated
	// listing does not name the spec, so its forbidden_paths widening is never
	// evaluated → RED. The glob surface gets one compare_truncated row.
	t.Run("truncated compare probes exact paths and flags glob surfaces", func(t *testing.T) {
		f := newDriftFixture(t)
		f.gh.truncated = true
		f.gh.put(driftBase, driftSpecRef, driftSpec(`[".github/workflows/**", ".fishhawk/**"]`))
		f.gh.put(driftHead, driftSpecRef, driftSpec(`[".github/workflows/**"]`))
		f.run(f.req(permissionDriftTriggerPROpened))
		var fp bool
		for _, r := range f.rows(t) {
			fp = fp || strings.HasPrefix(r.CheckKey, "permission_drift|"+permdrift.SurfaceIDForbiddenPaths+"|")
		}
		if !fp {
			t.Errorf("rows = %+v, want the probed forbidden_paths widening", f.rows(t))
		}
		if got := f.unevaluable(t)["gha-workflow-permissions"]; got != permdrift.ReasonCompareTruncated {
			t.Errorf("glob surface reason = %q, want compare_truncated", got)
		}
		if p := f.detected(t); len(p) != 1 || !p[0].CompareTruncated {
			t.Errorf("detected = %+v, want compare_truncated recorded", p)
		}
	})
	// A renamed file's source path is not in the listing, so exact-path
	// surfaces are probed. COUNTERFACTUAL (drop `|| renamed`): the listing names
	// only the rename target, so the spec's widening is missed → RED.
	t.Run("rename probes exact paths", func(t *testing.T) {
		f := newDriftFixture(t)
		f.gh.renamed = true
		f.gh.changed = []string{"docs/moved.md"}
		f.gh.put(driftBase, driftSpecRef, driftSpec(`[".github/workflows/**", ".fishhawk/**"]`))
		f.gh.put(driftHead, driftSpecRef, driftSpec(`[".github/workflows/**"]`))
		f.run(f.req(permissionDriftTriggerPROpened))
		if len(f.rows(t)) == 0 {
			t.Error("rows = 0, want the probed forbidden_paths widening")
		}
	})
	// COUNTERFACTUAL (the append-error `return` deleted): the fixture fails ONLY
	// the detected append, so InsertRaised would run → a row reads back → RED.
	t.Run("audit append failure raises nothing", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.au.appendErrCategory = permissionDriftDetectedCategory
		f.run(f.req(permissionDriftTriggerPROpened))
		if n := len(f.rows(t)); n != 0 {
			t.Errorf("rows = %d, want 0 (no concern without its origin entry)", n)
		}
	})
	// COUNTERFACTUAL (delete the retry): the store fails exactly once, so
	// without the retry no row reads back → RED.
	t.Run("insert failure is retried once", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.cr.failures = 1
		f.run(f.req(permissionDriftTriggerPROpened))
		if n := len(f.rows(t)); n != 1 || f.cr.calls != 2 {
			t.Errorf("rows = %d calls = %d, want 1/2", n, f.cr.calls)
		}
		if n := f.count(permissionDriftRaiseFailedCategory); n != 0 {
			t.Errorf("raise_failed entries = %d, want 0", n)
		}
	})
	// COUNTERFACTUAL (skip the raise_failed append): the store fails twice, so
	// the gap is recorded only by that entry → RED.
	t.Run("insert failure twice records raise_failed", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.cr.failures = 2
		f.run(f.req(permissionDriftTriggerPROpened))
		if n := len(f.rows(t)); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
		if n := f.count(permissionDriftDetectedCategory); n != 1 {
			t.Errorf("detected entries = %d, want 1 (the entry stands)", n)
		}
		failed := driftEntries(f.au.auditFake, permissionDriftRaiseFailedCategory)
		if len(failed) != 1 || !strings.Contains(string(failed[0].Payload), wfWideningKey) {
			t.Fatalf("raise_failed entries = %+v, want one naming %q", failed, wfWideningKey)
		}
	})
	// COUNTERFACTUAL (a list error returns instead of raising): the fixture's
	// list errors while InsertRaised works, so zero rows read back → RED.
	t.Run("list error fails open", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.cr.listErr = errors.New("store unavailable")
		f.run(f.req(permissionDriftTriggerPROpened))
		f.cr.listErr = nil
		if n := len(f.rows(t)); n != 1 {
			t.Errorf("rows = %d, want 1", n)
		}
	})
	// Approval condition 2. COUNTERFACTUAL (the unevaluable append in
	// permissionDriftSurfaces mutated out): the change touches only ci.yml, so
	// the only row left is the product widening → the declarations row is
	// missing → RED.
	t.Run("unparseable extension keeps product surfaces and fails closed", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.put(driftBase, permdrift.RepoSurfacesPath, "version: [\n")
		f.run(f.req(permissionDriftTriggerPROpened))
		p := f.detected(t)
		if len(p) != 1 || p[0].ExtensionError != permdrift.ReasonParseError || len(p[0].Widenings) != 1 {
			t.Errorf("detected = %+v, want the product widening with extension_error parse_error", p)
		}
		rows := f.rows(t)
		if len(rows) != 2 || rowWithKey(rows, wfWideningKey) == nil ||
			rowWithKey(rows, permdrift.UnevaluableKey(permdrift.SurfaceIDSurfaceDeclarations, permdrift.RepoSurfacesPath, driftHead)) == nil {
			t.Fatalf("rows = %+v, want the product widening AND the declarations unevaluable row", rows)
		}
		if got := f.unevaluable(t)[permdrift.SurfaceIDSurfaceDeclarations]; got != permdrift.ReasonExtensionParseError {
			t.Errorf("reason = %q, want extension_parse_error", got)
		}
	})
	// Item 8. COUNTERFACTUAL (the unevaluable append in permissionDriftSurfaces
	// mutated out): the change touches only ci.yml, so the only row left is the
	// product widening → the declarations fetch_failed row is missing → RED.
	t.Run("extension fetch failure keeps product surfaces and fails closed", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.contentStatus[driftBase+"|"+permdrift.RepoSurfacesPath] = http.StatusInternalServerError
		f.run(f.req(permissionDriftTriggerPROpened))
		p := f.detected(t)
		if len(p) != 1 || p[0].ExtensionError != permdrift.ReasonFetchFailed || len(p[0].Widenings) != 1 {
			t.Errorf("detected = %+v, want the product widening with extension_error fetch_failed", p)
		}
		rows := f.rows(t)
		if len(rows) != 2 || rowWithKey(rows, wfWideningKey) == nil ||
			rowWithKey(rows, permdrift.UnevaluableKey(permdrift.SurfaceIDSurfaceDeclarations, permdrift.RepoSurfacesPath, driftHead)) == nil {
			t.Fatalf("rows = %+v, want the product widening AND the declarations fetch_failed row", rows)
		}
		if got := f.unevaluable(t)[permdrift.SurfaceIDSurfaceDeclarations]; got != permdrift.ReasonFetchFailed {
			t.Errorf("reason = %q, want fetch_failed", got)
		}
	})
	t.Run("rejected extension entry is named", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		f.gh.put(driftBase, permdrift.RepoSurfacesPath,
			"version: 1\nsurfaces:\n  - id: gha-workflow-permissions\n    kind: gha_workflow_permissions\n    paths: [\"x.yml\"]\n")
		f.run(f.req(permissionDriftTriggerPROpened))
		p := f.detected(t)
		if len(p) != 1 || len(p[0].ExtensionRejected) != 1 || p[0].ExtensionRejected[0].Reason != permdrift.RejectProductIDCollision {
			t.Errorf("detected = %+v, want one product_id_collision rejection", p)
		}
	})
}

// TestFileFetcherFor mirrors the forgeCompareFor ladder.
func TestFileFetcherFor(t *testing.T) {
	inst := int64(55)
	githubRun := func(repo string, id *int64) *run.Run { return &run.Run{ID: uuid.New(), Repo: repo, InstallationID: id} }
	gh := newDriftGH()
	gh.put("r", "a.txt", "hello")
	client := gh.client(t)

	t.Run("github wired fetches through GetFile", func(t *testing.T) {
		s := New(Config{GitHub: client, ForgeResolver: func(string) (forge.Forge, error) {
			t.Fatal("github-family run must NOT consult the resolver")
			return nil, nil
		}})
		ff, scope, repo, reason := s.fileFetcherFor(githubRun("acme/widgets", &inst))
		if reason != "" {
			t.Fatalf("reason = %q", reason)
		}
		fc, err := ff.FetchFile(context.Background(), scope, repo, "a.txt", "r")
		if err != nil || string(fc.Content) != "hello" {
			t.Fatalf("FetchFile = %v, %v", fc, err)
		}
		if _, err := ff.FetchFile(context.Background(), scope, repo, "missing", "r"); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("missing file err = %v, want forge.ErrNotFound", err)
		}
	})
	for name, tc := range map[string]struct {
		cfg  Config
		row  *run.Run
		want string
	}{
		"github unwired":         {Config{}, githubRun("acme/widgets", &inst), "github client not wired"},
		"github no installation": {Config{GitHub: client}, githubRun("acme/widgets", nil), "no installation id"},
		"gitlab unresolved": {Config{ForgeResolver: func(string) (forge.Forge, error) { return nil, errors.New("boom") }},
			gitlabRunRow("acme/widgets"), "forge gitlab unresolved"},
		"gitlab not a fetcher": {Config{ForgeResolver: func(string) (forge.Forge, error) { return &fakeCompareForge{name: "gitlab"}, nil }},
			gitlabRunRow("acme/widgets"), "forge gitlab cannot fetch files"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, reason := New(tc.cfg).fileFetcherFor(tc.row); reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
	t.Run("github bad repo", func(t *testing.T) {
		if _, _, _, reason := New(Config{GitHub: client}).fileFetcherFor(githubRun("noslash", &inst)); !strings.HasPrefix(reason, "parse repo:") {
			t.Errorf("reason = %q", reason)
		}
	})
	t.Run("gitlab fetcher resolves", func(t *testing.T) {
		fake := &fetchingForge{fakeCompareForge: fakeCompareForge{name: "gitlab"}}
		s := New(Config{ForgeResolver: func(string) (forge.Forge, error) { return fake, nil }})
		ff, scope, repo, reason := s.fileFetcherFor(gitlabRunRow("group/sub/project"))
		if reason != "" || ff != forge.FileFetcher(fake) || scope.Ref() != "gitlab:5" || repo.Name != "project" {
			t.Errorf("fetcher/scope/repo/reason = %v/%q/%+v/%q", ff, scope.Ref(), repo, reason)
		}
	})
	t.Run("gitlab unsplittable repo", func(t *testing.T) {
		fake := &fetchingForge{fakeCompareForge: fakeCompareForge{name: "gitlab"}}
		s := New(Config{ForgeResolver: func(string) (forge.Forge, error) { return fake, nil }})
		if _, _, _, reason := s.fileFetcherFor(gitlabRunRow("noslash")); reason != "parse repo" {
			t.Errorf("reason = %q", reason)
		}
	})
}

// fetchingForge is a fakeCompareForge that also satisfies forge.FileFetcher.
type fetchingForge struct{ fakeCompareForge }

func (f *fetchingForge) FetchFile(context.Context, forge.CredentialScope, forge.RepoRef, string, string) (*forge.FileContent, error) {
	return nil, forge.ErrNotFound
}

// TestIntersectDrift (item 2): the conflict-resolution intersection matches
// widenings against widenings and narrowings against narrowings.
//
// COUNTERFACTUAL (intersectDrift's two sets mutated back into one union of
// second.Widened and second.Narrowed): first widens K (absent)→read while
// second NARROWS K write→read — both share CheckKey(…, K, "read") — so the
// union keeps first's widening → RED on "want zero widenings". The second arm
// (second also widens K to read) is the control that a same-direction change
// survives.
func TestIntersectDrift(t *testing.T) {
	sf := permdrift.DefaultSurfaces()[0]
	widen := permdrift.Change{Key: "jobs.build.contents", Before: permdrift.Absent, After: "read", Direction: permdrift.Widened}
	narrow := permdrift.Change{Key: "jobs.build.contents", Before: "write", After: "read", Direction: permdrift.Narrowed}
	first := permdrift.Result{Widened: []permdrift.Change{widen}}

	if got := intersectDrift(sf, driftWFPath, first, permdrift.Result{Narrowed: []permdrift.Change{narrow}}); len(got.Widened) != 0 {
		t.Errorf("opposite direction: widenings = %+v, want zero", got.Widened)
	}
	if got := intersectDrift(sf, driftWFPath, first, permdrift.Result{Widened: []permdrift.Change{widen}}); len(got.Widened) != 1 {
		t.Errorf("same direction: widenings = %+v, want one", got.Widened)
	}
	firstN := permdrift.Result{Narrowed: []permdrift.Change{narrow}}
	if got := intersectDrift(sf, driftWFPath, firstN, permdrift.Result{Widened: []permdrift.Change{widen}}); len(got.Narrowed) != 0 {
		t.Errorf("opposite direction: narrowings = %+v, want zero", got.Narrowed)
	}
	if got := intersectDrift(sf, driftWFPath, first, permdrift.Result{Unevaluable: permdrift.ReasonFetchFailed}); got.Unevaluable != permdrift.ReasonFetchFailed {
		t.Errorf("unevaluable second = %+v, want it to fail the pair closed", got)
	}
}

// TestPermissionDrift_RenameSource (item 3, server half) drives the REAL
// githubclient decode of previous_filename through the pairing into the
// concern store.
func TestPermissionDrift_RenameSource(t *testing.T) {
	const src, dst = "infra/specs/a.yaml", "docs/a.yaml"
	ext := "version: 1\nsurfaces:\n  - id: infra-specs\n    kind: fishhawk_spec_forbidden_paths\n    paths: [\"infra/specs/*.yaml\"]\n"

	// COUNTERFACTUAL (the PreviousPath pair addition mutated out): the
	// destination matches no surface and the source surface is glob-only, so
	// the exact-path probe skips it → no infra-specs row → RED.
	t.Run("rename out of a glob surface is evaluated at the source", func(t *testing.T) {
		f := newDriftFixture(t)
		f.gh.put(driftBase, permdrift.RepoSurfacesPath, ext)
		f.gh.put(driftHead, permdrift.RepoSurfacesPath, ext)
		f.gh.put(driftBase, src, driftSpec(`[".github/workflows/**"]`))
		f.gh.put(driftHead, dst, driftSpec(`[".github/workflows/**"]`))
		f.gh.changed = []string{dst}
		f.gh.renamedFrom = map[string]string{dst: src}
		f.run(f.req(permissionDriftTriggerPROpened))
		var found bool
		for _, r := range f.rows(t) {
			found = found || strings.HasPrefix(r.CheckKey, "permission_drift|infra-specs|"+src+"|")
		}
		if !found {
			t.Fatalf("rows = %+v, want the source's forbidden_paths widening on infra-specs", f.rows(t))
		}
		if got := f.unevaluable(t)["gha-workflow-permissions"]; got != "" {
			t.Errorf("a named rename source raised %q on a glob surface, want nothing", got)
		}
	})
	// COUNTERFACTUAL (the rename_source_unknown raise mutated out): the listed
	// destination matches no surface and the exact-path probes find nothing,
	// so zero rows exist → RED.
	t.Run("rename without a source raises rename_source_unknown per glob surface", func(t *testing.T) {
		f := newDriftFixture(t)
		f.gh.put(driftBase, permdrift.RepoSurfacesPath, ext)
		f.gh.put(driftHead, permdrift.RepoSurfacesPath, ext)
		f.gh.changed = []string{dst}
		f.gh.renamed = true
		f.run(f.req(permissionDriftTriggerPROpened))
		rows := f.rows(t)
		for _, id := range []string{"gha-workflow-permissions", "infra-specs"} {
			if rowWithKey(rows, permdrift.UnevaluableKey(id, dst, driftHead)) == nil {
				t.Errorf("rows = %+v, want a rename_source_unknown row on %s", rows, id)
			}
			if got := f.unevaluable(t)[id]; got != permdrift.ReasonRenameSourceUnknown {
				t.Errorf("%s reason = %q, want rename_source_unknown", id, got)
			}
		}
		if len(rows) != 2 {
			t.Errorf("rows = %d, want exactly one per glob surface (exact-path surfaces are probed)", len(rows))
		}
	})
}

// TestPermissionDrift_UnevaluablePerHead (item 5, approval condition 1): an
// unevaluable key carries the resolved head commit, so a waived row suppresses
// a repeat at the same commit but never a result at a new one, and an
// unresolved head is suppressed by an OPEN row only.
func TestPermissionDrift_UnevaluablePerHead(t *testing.T) {
	waiveAll := func(f *driftFixture) {
		f.cr.mu.Lock()
		defer f.cr.mu.Unlock()
		for _, r := range f.cr.rows {
			r.State = concern.StateWaived
		}
	}

	// COUNTERFACTUAL (the head dropped from UnevaluableKey's key — the body
	// mutated to ignore its head argument): head A's waived row then shares
	// head B's key and suppresses it → rows = 1 → RED on "want 2".
	t.Run("push trigger: waived at head A does not suppress head B", func(t *testing.T) {
		f := newDriftFixture(t)
		f.workflowChange(wfBase, wfWidened)
		const headB = "head2222"
		f.gh.put(headB, driftWFPath, wfWidened)
		f.gh.contentStatus[driftHead+"|"+driftWFPath] = http.StatusInternalServerError
		f.gh.contentStatus[headB+"|"+driftWFPath] = http.StatusInternalServerError
		f.run(f.req(permissionDriftTriggerPROpened))
		if n := len(f.rows(t)); n != 1 {
			t.Fatalf("rows after head A = %d, want 1", n)
		}
		waiveAll(f)
		f.run(f.req(permissionDriftTriggerFixupPushed))
		if n := len(f.rows(t)); n != 1 {
			t.Fatalf("rows after a repeat at head A = %d, want 1 (waived at the same head suppresses)", n)
		}
		r := f.req(permissionDriftTriggerFixupPushed)
		r.Head = headB
		f.run(r)
		rows := f.rows(t)
		if len(rows) != 2 || rowWithKey(rows, permdrift.UnevaluableKey("gha-workflow-permissions", driftWFPath, headB)) == nil {
			t.Fatalf("rows after head B = %+v, want 2 with a head-B row", rows)
		}
	})

	// Approval condition 1(a). COUNTERFACTUAL (permissionDriftKeyHead mutated
	// to return req.Head for the consolidated trigger — keying on the BRANCH
	// name): both checks name the same branch, so commit A's waived row
	// suppresses commit B's → rows = 1 → RED on "want 2".
	t.Run("consolidated trigger: waived at commit A does not suppress commit B", func(t *testing.T) {
		f := newDriftFixture(t)
		const branch = "fishhawk/run-x-consolidated"
		f.gh.changed = []string{driftWFPath}
		f.gh.put(driftBase, driftWFPath, wfBase)
		f.gh.contentStatus[branch+"|"+driftWFPath] = http.StatusInternalServerError
		req := permissionDriftRequest{RunID: f.runRow.ID, StageID: f.stage.ID, Base: driftBase, Head: branch, Trigger: permissionDriftTriggerConsolidated}
		f.gh.headSHA = "aaaa1111"
		f.run(req)
		rows := f.rows(t)
		if len(rows) != 1 || rows[0].CheckKey != permdrift.UnevaluableKey("gha-workflow-permissions", driftWFPath, "aaaa1111") {
			t.Fatalf("rows after commit A = %+v, want one row keyed by commit A", rows)
		}
		waiveAll(f)
		f.run(req)
		if n := len(f.rows(t)); n != 1 {
			t.Fatalf("rows after a repeat at commit A = %d, want 1", n)
		}
		f.gh.headSHA = "bbbb2222"
		f.run(req)
		if rows := f.rows(t); len(rows) != 2 || rowWithKey(rows, permdrift.UnevaluableKey("gha-workflow-permissions", driftWFPath, "bbbb2222")) == nil {
			t.Fatalf("rows after commit B = %+v, want 2 with a commit-B row", rows)
		}
	})

	// The consolidated compare failed, so its head commit is resolved from the
	// newest integration_commit_recorded merge_sha. COUNTERFACTUAL (the ledger
	// fallback in permissionDriftKeyHead mutated out): the key carries "" and
	// the assertion on the merge-sha key goes RED.
	t.Run("consolidated trigger: compare failure keys by the integration ledger", func(t *testing.T) {
		f := newDriftFixture(t)
		const branch = "fishhawk/run-x-consolidated"
		f.gh.compareStatus = http.StatusInternalServerError
		seed := func(seq int64, sha string) {
			rid := f.runRow.ID
			payload, _ := json.Marshal(map[string]string{"merge_sha": sha})
			f.au.mu.Lock()
			f.au.seeded = append(f.au.seeded, &audit.Entry{RunID: &rid, Sequence: seq, Category: lineageIntegrationCommitCategory, Payload: payload})
			f.au.mu.Unlock()
		}
		seed(1, "merge1111")
		seed(2, "merge2222")
		req := permissionDriftRequest{RunID: f.runRow.ID, StageID: f.stage.ID, Base: "main", Head: branch, Trigger: permissionDriftTriggerConsolidated}
		f.run(req)
		if rows := f.rows(t); len(rows) != 1 || rows[0].CheckKey != permdrift.UnevaluableKey(permissionDriftAllSurfaces.ID, "main.."+branch, "merge2222") {
			t.Fatalf("rows = %+v, want one compare_failed row keyed by the newest merge sha", rows)
		}
	})

	// Approval condition 1(b). COUNTERFACTUAL (raisePermissionDrift's
	// `if u.head == "" { suppress = open }` mutated out): the waived row then
	// suppresses the repeat → rows = 1 → RED on "want 2". The open-row arm is
	// the control that an OPEN row still suppresses an unresolved head.
	t.Run("empty head: a waived prior row does not suppress", func(t *testing.T) {
		f := newDriftFixture(t)
		r := f.req(permissionDriftTriggerPROpened)
		r.Head = ""
		f.run(r)
		f.run(r)
		if n := len(f.rows(t)); n != 1 {
			t.Fatalf("rows after two checks with an OPEN row = %d, want 1 (an open row suppresses)", n)
		}
		waiveAll(f)
		f.run(r)
		if n := len(f.rows(t)); n != 2 {
			t.Fatalf("rows after a waive + recheck = %d, want 2 (a waived row never suppresses an unresolved head)", n)
		}
	})
	t.Run("unresolvable consolidated head: a waived prior row does not suppress", func(t *testing.T) {
		f := newDriftFixture(t)
		f.gh.compareStatus = http.StatusInternalServerError
		req := permissionDriftRequest{RunID: f.runRow.ID, StageID: f.stage.ID, Base: "main", Head: "fishhawk/run-x-consolidated", Trigger: permissionDriftTriggerConsolidated}
		f.run(req)
		rows := f.rows(t)
		if len(rows) != 1 || rows[0].CheckKey != permdrift.UnevaluableKey(permissionDriftAllSurfaces.ID, "main..fishhawk/run-x-consolidated", "") {
			t.Fatalf("rows = %+v, want one row keyed by the unresolved head", rows)
		}
		waiveAll(f)
		f.run(req)
		if n := len(f.rows(t)); n != 2 {
			t.Fatalf("rows after a waive + recheck = %d, want 2", n)
		}
	})
}

// TestPermissionDrift_PayloadAndNoteAreSanitized (item 10, approval
// conditions 3 and 4): a forbidden_paths glob is a bracketed key value the
// extractors do not escape, so a spec can put a newline, a bidi override and
// 10 KB into a widening key.
//
// COUNTERFACTUAL (the permdrift.Display calls on the widening's payload fields
// mutated out): the payload key carries the newline and is unbounded → RED.
// COUNTERFACTUAL (escapeKeyField's control-rune clause mutated out): check_key
// carries the newline → RED.
func TestPermissionDrift_PayloadAndNoteAreSanitized(t *testing.T) {
	f := newDriftFixture(t)
	evil := "\"x\\n## injected\\u202e\"" // YAML double-quoted escapes: a newline and a bidi override
	long := `"` + strings.Repeat("y", 10*1024) + `"`
	f.gh.put(driftBase, driftSpecRef, driftSpec(`[`+evil+`, `+long+`]`))
	f.gh.put(driftHead, driftSpecRef, driftSpec(`["docs/**"]`))
	f.gh.changed = []string{driftSpecRef}
	f.run(f.req(permissionDriftTriggerPROpened))

	p := f.detected(t)
	if len(p) != 1 || len(p[0].Widenings) < 2 {
		t.Fatalf("detected = %+v, want the two forbidden_paths widenings", p)
	}
	var sawNewlineKey bool
	for _, w := range p[0].Widenings {
		for _, field := range []string{w.Path, w.Key, w.Before, w.After} {
			if strings.ContainsFunc(field, unicode.IsControl) || strings.ContainsRune(field, '\u202e') || len(field) > 256 {
				t.Errorf("payload field %q is not control-free and bounded", field)
			}
		}
		if strings.ContainsFunc(w.CheckKey, unicode.IsControl) || strings.ContainsRune(w.CheckKey, '\u202e') || len(w.CheckKey) > 4*300 {
			t.Errorf("check_key %q carries a control rune or is unbounded", w.CheckKey)
		}
		sawNewlineKey = sawNewlineKey || strings.Contains(w.CheckKey, "%0A")
	}
	if !sawNewlineKey {
		t.Errorf("no check_key carries the escaped newline: %+v", p[0].Widenings)
	}
	for _, r := range f.rows(t) {
		if strings.ContainsFunc(r.Note, unicode.IsControl) || strings.ContainsRune(r.Note, '\u202e') || len(r.Note) > 2000 {
			t.Errorf("note carries a control rune or is unbounded (%d bytes)", len(r.Note))
		}
		if strings.ContainsFunc(r.CheckKey, unicode.IsControl) {
			t.Errorf("stored check_key %q carries a control rune", r.CheckKey)
		}
	}
}

// TestPermissionDrift_ConsolidatedReviewNoReviewers (item 7): the drift hook
// in DispatchConsolidatedReview runs for a mergeable parent even when no
// reviewer is declared or wired (no PlanReviewers/PlanReviewer in the
// fixture) — no reviewer-dependent guard precedes it.
//
// COUNTERFACTUAL (the runPermissionDriftCheck call in
// DispatchConsolidatedReview temporarily removed, then restored with no diff):
// no reviewer runs, so the drift hook is the only path that can mint a row →
// zero rows → RED.
func TestPermissionDrift_ConsolidatedReviewNoReviewers(t *testing.T) {
	f := newDriftFixture(t)
	f.workflowChange(wfBase, wfWidened)
	// specImplementNoReviewers (diff_secrets_test.go): no implement reviewers.
	parent, implStage := seedConsolidatedParent(t, f.rr, newFakeArtifactRepo(), specImplementNoReviewers)
	parent.Repo = "acme/widgets"
	if f.s.cfg.PlanReviewers != nil || f.s.cfg.PlanReviewer != nil {
		t.Fatal("fixture wires a reviewer; this arm needs none")
	}

	f.s.DispatchConsolidatedReview(context.Background(), parent.ID, driftBase, driftHead)
	f.s.waitBackgroundReviews()

	rows := serverCheckRows(t, f.cr.fakeConcernRepo, parent.ID)
	if len(rows) != 1 || rows[0].CheckKey != wfWideningKey || rows[0].StageID != implStage.ID {
		t.Fatalf("rows = %+v, want one %q row on the parent implement stage", rows, wfWideningKey)
	}
}
