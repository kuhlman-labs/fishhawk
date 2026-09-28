package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodash"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Repo dashboard rollups (E40.3 / #1714). Fakes the seam tests need live here
// (approval condition 5): dashRunRepo wraps the shared fakeRepo adding stage
// rows and a cost summer; dashAuditFake serves a per-run audit chain.

// dashNow is a Wednesday; the default 12-week window ends Monday 2026-10-05.
var dashNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

type dashRunRepo struct {
	*fakeRepo
	stages map[uuid.UUID][]*run.Stage
	spent  float64
}

func (r *dashRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	return r.stages[id], nil
}

func (r *dashRunRepo) SumWorkflowCostInRange(context.Context, string, string, time.Time, time.Time) (float64, error) {
	return r.spent, nil
}

type dashAuditFake struct {
	audit.BaseFake
	byRun map[uuid.UUID][]*audit.Entry
}

func (f *dashAuditFake) ListForRun(_ context.Context, id uuid.UUID) ([]*audit.Entry, error) {
	return f.byRun[id], nil
}

type dashFixture struct {
	runs  *dashRunRepo
	audit *dashAuditFake
}

func newDashFixture() *dashFixture {
	return &dashFixture{
		runs:  &dashRunRepo{fakeRepo: newFakeRepo(), stages: map[uuid.UUID][]*run.Stage{}},
		audit: &dashAuditFake{byRun: map[uuid.UUID][]*audit.Entry{}},
	}
}

func (f *dashFixture) server() *Server {
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs, AuditRepo: f.audit})
	s.nowFunc = func() time.Time { return dashNow }
	return s
}

func (f *dashFixture) seed(id uuid.UUID, repo string, created time.Time, specYAML string) *run.Run {
	rn := &run.Run{
		ID: id, Repo: repo, WorkflowID: "feature_change", WorkflowSHA: "abc123",
		TriggerSource: run.TriggerCLI, State: run.StateSucceeded,
		CreatedAt: created, UpdatedAt: created, WorkflowSpec: []byte(specYAML),
	}
	f.runs.mu.Lock()
	f.runs.runs[id] = rn
	f.runs.mu.Unlock()
	return rn
}

func (f *dashFixture) add(runID uuid.UUID, stageID *uuid.UUID, cat string, at time.Time, payload map[string]any) {
	p, _ := json.Marshal(payload)
	rid := runID
	f.audit.byRun[runID] = append(f.audit.byRun[runID], &audit.Entry{
		RunID: &rid, StageID: stageID, Category: cat, Timestamp: at, Payload: p,
	})
}

func dashID(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)) }

const dashSpec = `version: "0.4"
roles:
  tech_lead:
    members: ["@acme/leads"]
workflows:
  feature_change:
    budgets:
      - period: weekly
        limit_usd: 50
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-5-5
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          max_tokens: 1000
        produces:
          - artifact: pull_request
`

// seedGolden seeds one fully-populated merged run (the wire-golden fixture)
// plus one older unmerged run, with fixed ids and timestamps.
func (f *dashFixture) seedGolden() {
	created := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	rn := f.seed(dashID(1), "acme/app", created, dashSpec)
	pr := "https://github.com/acme/app/pull/7"
	rn.PullRequestURL = &pr
	cat := run.FailureA
	planID, implID := dashID(11), dashID(12)
	f.runs.stages[rn.ID] = []*run.Stage{
		{ID: planID, RunID: rn.ID, Type: run.StageType("plan")},
		{ID: implID, RunID: rn.ID, Type: run.StageType("implement"), FailureCategory: &cat},
	}
	f.add(rn.ID, &planID, "plan_generated", created.Add(10*time.Minute), map[string]any{})
	f.add(rn.ID, &planID, "approval_submitted", created.Add(40*time.Minute), map[string]any{"stage_id": planID.String(), "decision": "approve"})
	f.add(rn.ID, &implID, "cost_recorded", created.Add(time.Hour), map[string]any{
		"usd": 3.0, "model": "unpriced-test-model", "input_tokens": 100, "cache_read_input_tokens": 300, "cache_write_input_tokens": 100, "output_tokens": 50,
	})
	f.add(rn.ID, &implID, "stage_fixup_triggered", created.Add(2*time.Hour), map[string]any{})
	f.add(rn.ID, &implID, "cost_recorded", created.Add(3*time.Hour), map[string]any{"usd": 1.0, "source": "implement_review"})
	f.add(rn.ID, nil, "acceptance_outcome_recorded", created.Add(4*time.Hour), map[string]any{"verdict": "passed"})
	f.add(rn.ID, nil, "pr_merged", created.Add(5*time.Hour), map[string]any{})
	f.add(rn.ID, nil, "post_merge_observed", created.Add(6*time.Hour), map[string]any{})

	// An unmerged run two weeks earlier: counts for health only.
	f.seed(dashID(2), "acme/app", time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC), dashSpec)
	f.runs.spent = 12.5
}

func dashGET(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func dashDecode(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	return m
}

func dashGoldenPath(t *testing.T, endpoint string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "wire", "repodash_"+endpoint+".json")
}

// TestRepoDash_WireGoldens drives each endpoint through the REAL mux over the
// populated fixture and compares the indented body byte-for-byte against
// testdata/wire/repodash_<endpoint>.json — the files the SPA's panel tests
// render (approval condition 3). A json-tag rename fails here. Regenerate
// deliberately with FISHHAWK_UPDATE_WIRE_GOLDEN=1.
func TestRepoDash_WireGoldens(t *testing.T) {
	for _, endpoint := range []string{"throughput", "health", "economics", "posture"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newDashFixture()
			f.seedGolden()
			rec := dashGET(t, f.server(), "/v0/repos/acme/app/"+endpoint+"?weeks=4")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, bytes.TrimSpace(rec.Body.Bytes()), "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			path := dashGoldenPath(t, endpoint)
			if os.Getenv("FISHHAWK_UPDATE_WIRE_GOLDEN") == "1" {
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read wire golden %s: %v", path, err)
			}
			if !bytes.Equal(pretty.Bytes(), golden) {
				t.Fatalf("%s body drifted from the shared wire golden %s (the SPA renders that file — update both sides deliberately):\n--- got ---\n%s\n--- golden ---\n%s", endpoint, path, pretty.String(), golden)
			}
		})
	}
}

// TestRepoDash_PopulatedValues asserts the load-bearing numbers of the golden
// fixture directly, so the golden cannot be regenerated around a wrong fold.
func TestRepoDash_PopulatedValues(t *testing.T) {
	f := newDashFixture()
	f.seedGolden()
	s := f.server()

	var th repoThroughputResponse
	dashJSON(t, dashGET(t, s, "/v0/repos/acme/app/throughput?weeks=4"), &th)
	if th.MergedChanges != 1 || th.MedianCycleTimeSeconds != 5*3600 || th.CycleTimeSamples != 1 {
		t.Errorf("throughput = %+v, want 1 merged change, 5h cycle", th)
	}
	if th.WaitOnHuman == nil || th.WaitOnHuman.Runs != 1 || th.WaitOnHuman.Gates[0].Gate != "plan_approval" || th.WaitOnHuman.Gates[0].MedianWaitSeconds != 1800 {
		t.Errorf("wait_on_human = %+v, want one 30m plan_approval wait", th.WaitOnHuman)
	}

	var h repoHealthResponse
	dashJSON(t, dashGET(t, s, "/v0/repos/acme/app/health?weeks=4"), &h)
	if h.RunsConsidered != 2 || h.PlanFirstShotApprovalRate != 1 || h.FixupRate != 0.5 || h.AcceptancePassRate != 1 || h.FailureCategories.A != 1 {
		t.Errorf("health = %+v", h)
	}

	var e repoEconomicsResponse
	dashJSON(t, dashGET(t, s, "/v0/repos/acme/app/economics?weeks=4"), &e)
	if e.TotalCostUSD != 4 || e.CostPerMergedChangeUSD != 4 || len(e.Budgets) != 1 || e.Budgets[0].SpentUSD != 12.5 || e.Budgets[0].LimitUSD != 50 {
		t.Errorf("economics = %+v", e)
	}

	var p repoPostureResponse
	dashJSON(t, dashGET(t, s, "/v0/repos/acme/app/posture"), &p)
	if p.Version != "0.4" || p.SchemaMajor != 0 || !p.SchemaSupported || !p.SpecValid || p.SchemaHash == "" || len(p.Workflows) != 1 || len(p.Workflows[0].Stages) != 2 {
		t.Errorf("posture = %+v", p)
	}
}

func dashJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// (a) unconfigured repositories answer 503 with a named code.
func TestRepoDash_Unconfigured503(t *testing.T) {
	for _, ep := range []string{"throughput", "health", "economics", "posture"} {
		rec := dashGET(t, New(Config{Addr: "127.0.0.1:0"}), "/v0/repos/acme/app/"+ep)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", ep, rec.Code)
		}
		assertErrorCode(t, rec, "run_repo_unconfigured")
	}
	f := newDashFixture()
	rec := dashGET(t, New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs}), "/v0/repos/acme/app/throughput")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no audit repo: status = %d, want 503", rec.Code)
	}
	assertErrorCode(t, rec, "audit_repo_unconfigured")
}

// (b)(c) the weeks bound: the fixture repo HAS runs, so a deleted bound check
// would serve 200.
func TestRepoDash_WeeksBound400(t *testing.T) {
	f := newDashFixture()
	f.seedGolden()
	s := f.server()
	for _, weeks := range []string{"0", "53", "abc", "-1"} {
		for _, ep := range []string{"throughput", "health", "economics"} {
			rec := dashGET(t, s, "/v0/repos/acme/app/"+ep+"?weeks="+weeks)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s weeks=%s: status = %d, want 400; body %s", ep, weeks, rec.Code, rec.Body.String())
			}
			assertErrorCode(t, rec, "validation_failed")
			if !strings.Contains(rec.Body.String(), `"field":"weeks"`) {
				t.Fatalf("%s weeks=%s: body does not name the field: %s", ep, weeks, rec.Body.String())
			}
		}
	}
	for _, weeks := range []string{"1", "52"} {
		if rec := dashGET(t, s, "/v0/repos/acme/app/throughput?weeks="+weeks); rec.Code != http.StatusOK {
			t.Fatalf("weeks=%s: status = %d, want 200", weeks, rec.Code)
		}
	}
}

// (d) a repo the visibility filter denies answers 403 on every endpoint. The
// fixture HOLDS populated rows for the repo, so with the gate's call site
// removed each handler would reach a 200 body instead.
func TestRepoDash_RepoForbidden403(t *testing.T) {
	f := newDashFixture()
	f.seedGolden()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs, AuditRepo: f.audit,
		AccountRoles:   fakeAccountRoles{role: account.RoleMember},
		RepoVisibility: newFakeRepoVisibility(map[string]bool{"other/repo": true})})
	s.nowFunc = func() time.Time { return dashNow }
	handlers := map[string]http.HandlerFunc{
		"throughput": s.handleGetRepoThroughput, "health": s.handleGetRepoHealth,
		"economics": s.handleGetRepoEconomics, "posture": s.handleGetRepoPosture,
	}
	for ep, h := range handlers {
		req := httptest.NewRequest(http.MethodGet, "/v0/repos/acme/app/"+ep, nil)
		req.SetPathValue("owner", "acme")
		req.SetPathValue("name", "app")
		rec := httptest.NewRecorder()
		h(rec, withIdentity(req, memberIdentity()))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403; body %s", ep, rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "repo_forbidden")
	}
}

// (e) a repo with no runs answers 200 with zero-valued rollups and no NaN.
func TestRepoDash_NoRunsZeroRollups(t *testing.T) {
	s := newDashFixture().server()
	th := dashDecode(t, dashGET(t, s, "/v0/repos/acme/app/throughput"))
	if string(th["merged_changes"]) != "0" || string(th["median_cycle_time_seconds"]) != "0" || string(th["truncated"]) != "false" {
		t.Fatalf("throughput = %v", th)
	}
	var weeks []repodash.WeekCount
	_ = json.Unmarshal(th["weeks"], &weeks)
	if len(weeks) != repodash.DefaultWeeks {
		t.Fatalf("weeks = %d buckets, want the default %d", len(weeks), repodash.DefaultWeeks)
	}
	h := dashDecode(t, dashGET(t, s, "/v0/repos/acme/app/health"))
	for _, k := range []string{"plan_first_shot_approval_rate", "fixup_rate", "acceptance_pass_rate", "runs_considered"} {
		if string(h[k]) != "0" {
			t.Errorf("health %s = %s, want 0", k, h[k])
		}
	}
}

// (f) economics with no cost_recorded rows answers 200 `{}`.
func TestRepoDash_EconomicsNoCostEmptyObject(t *testing.T) {
	f := newDashFixture()
	f.seed(dashID(1), "acme/app", dashNow.Add(-time.Hour), dashSpec)
	rec := dashGET(t, f.server(), "/v0/repos/acme/app/economics")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("status = %d body = %q, want 200 {}", rec.Code, rec.Body.String())
	}
}

// (g) posture with no cached spec (and with no run at all) answers 200 `{}`.
func TestRepoDash_PostureNoSpecEmptyObject(t *testing.T) {
	f := newDashFixture()
	if rec := dashGET(t, f.server(), "/v0/repos/acme/app/posture"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("no run: status = %d body = %q", rec.Code, rec.Body.String())
	}
	f.seed(dashID(1), "acme/app", dashNow.Add(-time.Hour), "")
	if rec := dashGET(t, f.server(), "/v0/repos/acme/app/posture"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("no spec: status = %d body = %q", rec.Code, rec.Body.String())
	}
}

// (h) + approval condition 4: supported+valid, unsupported major, invalid spec.
func TestRepoDash_PostureValidity(t *testing.T) {
	cases := []struct {
		name          string
		spec          string
		wantVersion   string
		wantMajor     int
		wantSupported bool
		wantValid     bool
	}{
		{"supported and valid", dashSpec, "0.4", 0, true, true},
		{"unsupported major", "version: \"9.0\"\nworkflows: {}\n", "9.0", 9, false, false},
		{"invalid spec", "version: \"0.4\"\nworkflows:\n  feature_change:\n    stages: 7\n", "0.4", 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDashFixture()
			f.seed(dashID(1), "acme/app", dashNow.Add(-time.Hour), tc.spec)
			var p repoPostureResponse
			dashJSON(t, dashGET(t, f.server(), "/v0/repos/acme/app/posture"), &p)
			if p.Version != tc.wantVersion || p.SchemaMajor != tc.wantMajor || p.SchemaSupported != tc.wantSupported || p.SpecValid != tc.wantValid {
				t.Fatalf("posture = %+v", p)
			}
			if tc.wantSupported == (p.SchemaHash == "") {
				t.Errorf("schema_hash = %q, want present iff supported", p.SchemaHash)
			}
			if tc.wantValid != (p.SpecError == "") {
				t.Errorf("spec_error = %q, want present iff invalid", p.SpecError)
			}
		})
	}
}

// (i) throughput over runs with NO gate markers: the raw JSON has no
// wait_on_human key (a decoded zero block and an absent key are
// indistinguishable after unmarshalling, so assert on the bytes).
func TestRepoDash_ThroughputOmitsWaitOnHuman(t *testing.T) {
	f := newDashFixture()
	rn := f.seed(dashID(1), "acme/app", time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC), dashSpec)
	f.add(rn.ID, nil, "pr_merged", rn.CreatedAt.Add(time.Hour), map[string]any{})
	rec := dashGET(t, f.server(), "/v0/repos/acme/app/throughput")
	raw := dashDecode(t, rec)
	if _, ok := raw["wait_on_human"]; ok {
		t.Fatalf("wait_on_human present with no gate markers: %s", rec.Body.String())
	}
	if string(raw["merged_changes"]) != "1" {
		t.Fatalf("merged_changes = %s, want 1 (fixture must be populated)", raw["merged_changes"])
	}
}

// Approval condition 1: a run created 5 days BEFORE the window and merged
// inside it is counted (the look-back keeps it in the scan).
func TestRepoDash_LookBackCountsPreWindowRun(t *testing.T) {
	f := newDashFixture()
	win := repodash.NewWindow(dashNow, 1)
	rn := f.seed(dashID(1), "acme/app", win.Start.Add(-5*24*time.Hour), dashSpec)
	f.add(rn.ID, nil, "pr_merged", win.Start.Add(time.Hour), map[string]any{})
	var th repoThroughputResponse
	dashJSON(t, dashGET(t, f.server(), "/v0/repos/acme/app/throughput?weeks=1"), &th)
	if th.MergedChanges != 1 || th.CycleTimeSamples != 1 {
		t.Fatalf("throughput = %+v, want the pre-window run's merge counted", th)
	}
	if th.Truncated {
		t.Fatalf("truncated = true on a complete scan")
	}
}

// Approval condition 1: ceiling+N runs all inside the look-back set
// truncated:true on every rollup; exactly-ceiling runs do not.
func TestRepoDash_CeilingSetsTruncated(t *testing.T) {
	f := newDashFixture()
	for i := 0; i < repodash.MaxRunsScanned+5; i++ {
		f.seed(dashID(1000+i), "acme/app", dashNow.Add(-time.Duration(i)*time.Second), "")
	}
	s := f.server()
	for _, ep := range []string{"throughput", "health", "economics"} {
		rec := dashGET(t, s, "/v0/repos/acme/app/"+ep)
		raw := dashDecode(t, rec)
		if string(raw["truncated"]) != "true" {
			t.Fatalf("%s: truncated = %s, want true; body %s", ep, raw["truncated"], rec.Body.String())
		}
	}

	g := newDashFixture()
	for i := 0; i < repodash.MaxRunsScanned; i++ {
		g.seed(dashID(5000+i), "acme/app", dashNow.Add(-time.Duration(i)*time.Second), "")
	}
	raw := dashDecode(t, dashGET(t, g.server(), "/v0/repos/acme/app/throughput"))
	if string(raw["truncated"]) != "false" {
		t.Fatalf("exactly-ceiling scan: truncated = %s, want false", raw["truncated"])
	}
}

// TestRepoDash_SchemaHashTableCoversEveryEmbeddedMajor derives the set of
// majors the spec parser routes (an unrouted major fails with "is not
// recognized") and requires embeddedWorkflowSchemaHashes to match it, so a new
// workflow major cannot land with posture reporting it schema_supported:false.
func TestRepoDash_SchemaHashTableCoversEveryEmbeddedMajor(t *testing.T) {
	for major := 0; major <= 20; major++ {
		_, err := spec.ParseBytes([]byte(fmt.Sprintf("version: \"%d.0\"\nworkflows: {}\n", major)))
		routed := err == nil || !strings.Contains(err.Error(), "is not recognized")
		if _, ok := embeddedWorkflowSchemaHashes[major]; ok != routed {
			t.Fatalf("workflow major %d: parser routes it = %v, embeddedWorkflowSchemaHashes has it = %v — "+
				"add/remove its row in backend/internal/server/repodash.go (and see AGENTS.md § Schema change checklist step 3)", major, routed, ok)
		}
	}
}
