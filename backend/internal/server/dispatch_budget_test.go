package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// dbBudget is one budget line of a test spec.
type dbBudget struct {
	period      string
	limit       float64
	enforcement string
}

// dbSpec builds a feature_change workflow spec carrying budgets.
func dbSpec(budgets ...dbBudget) string {
	var b strings.Builder
	b.WriteString("version: \"0.4\"\nworkflows:\n  feature_change:\n    description: \"x\"\n")
	if len(budgets) > 0 {
		b.WriteString("    budgets:\n")
		for _, bb := range budgets {
			fmt.Fprintf(&b, "      - period: %s\n        limit_usd: %g\n        enforcement: %s\n", bb.period, bb.limit, bb.enforcement)
		}
	}
	b.WriteString("    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n")
	return b.String()
}

func dbBlocking(limit float64) dbBudget { return dbBudget{"weekly", limit, "blocking"} }
func dbAdvisory(limit float64) dbBudget { return dbBudget{"weekly", limit, "advisory"} }

// dbRunRepo is the in-memory fakeRepo plus a cost summer keyed by the
// [from,to) range and a seeded stage list.
type dbRunRepo struct {
	*fakeRepo
	mu       sync.Mutex
	spend    func(from, to time.Time) float64
	sumErr   error
	sumCalls int
	stages   map[uuid.UUID][]*run.Stage
	// getRunErr injects a GetRun error per run id (an ancestor read failure).
	getRunErr map[uuid.UUID]error
}

func (r *dbRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	err := r.getRunErr[id]
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fakeRepo.GetRun(ctx, id)
}

func (r *dbRunRepo) SumWorkflowCostInRange(_ context.Context, _, _ string, from, to time.Time) (float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sumCalls++
	if r.sumErr != nil {
		return 0, r.sumErr
	}
	if r.spend == nil {
		return 0, nil
	}
	return r.spend(from, to), nil
}

func (r *dbRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stages[runID], nil
}

// dbFixture is a server with a GitHub stub serving the live spec, the
// range-keyed summer and the audit fake.
type dbFixture struct {
	s    *Server
	repo *dbRunRepo
	au   *auditFake
	gh   *fakeGitHubForRuns
}

type dbOpts struct {
	noGitHub      bool
	limitOverride float64
}

func newDBFixture(t *testing.T, liveSpec string, o dbOpts) *dbFixture {
	t.Helper()
	f := &dbFixture{
		repo: &dbRunRepo{fakeRepo: newFakeRepo(), stages: map[uuid.UUID][]*run.Stage{}},
		au:   newAuditFake(),
		gh:   newFakeGitHubForRuns(liveSpec),
	}
	cfg := Config{Addr: "127.0.0.1:0", RunRepo: f.repo, AuditRepo: f.au, BudgetLimitOverrideUSD: o.limitOverride}
	if !o.noGitHub {
		ghSrv := f.gh.server(t)
		cfg.GitHub = &githubclient.Client{
			BaseURL: ghSrv.URL,
			Tokens:  &ghTokensStub{tok: "ghs_test"},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
			AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
		}
	}
	f.s = New(cfg)
	return f
}

// setLive swaps the stub's live default-branch spec.
func (f *dbFixture) setLive(specYAML string) {
	enc := base64.StdEncoding.EncodeToString([]byte(specYAML))
	f.gh.specStatus = http.StatusOK
	f.gh.specBody = `{"path":".fishhawk/workflows.yaml","sha":"spec_sha","content":"` + enc + `","encoding":"base64","type":"file"}`
}

func (f *dbFixture) spent(v float64) {
	f.repo.spend = func(_, _ time.Time) float64 { return v }
}

// seedRun stores a run with the cached admission spec. mutate may set
// lineage, forge or installation fields.
func (f *dbFixture) seedRun(admission string, mutate func(*run.Run)) *run.Run {
	r := &run.Run{
		ID:            uuid.New(),
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		State:         run.StateRunning,
		WorkflowSpec:  []byte(admission),
		CreatedAt:     time.Now().UTC(),
	}
	if mutate != nil {
		mutate(r)
	}
	f.repo.mu.Lock()
	f.repo.runs[r.ID] = r
	f.repo.mu.Unlock()
	return r
}

// bind seeds a run_budget_override_bound row on runID naming repo/workflow.
func (f *dbFixture) bind(runID uuid.UUID, repo, workflowID string) {
	payload, _ := json.Marshal(budgetOverrideBoundPayload{WorkflowID: workflowID, Repo: repo, Period: "weekly", LimitUSD: 10, Spent: 15, AdmittedVia: "create"})
	rid := runID
	f.au.mu.Lock()
	f.au.seeded = append(f.au.seeded, &audit.Entry{ID: uuid.New(), RunID: &rid, Category: CategoryRunBudgetOverrideBound, Payload: payload})
	f.au.mu.Unlock()
}

// seedVerifyPass gives runID a pending implement stage with a live
// merge-candidate verify trigger (consumed=false) and returns its id.
func (f *dbFixture) seedVerifyPass(runID uuid.UUID, consumed bool) uuid.UUID {
	st := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStatePending}
	f.repo.mu.Lock()
	f.repo.stages[runID] = append(f.repo.stages[runID], st)
	f.repo.mu.Unlock()
	trig, _ := json.Marshal(mergeCandidateVerifyTrigger{Branch: "b", BaseRef: "main", ExpectedHeadSHA: "abc", Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: "scripts/test verify"})
	rid, sid := runID, st.ID
	f.au.mu.Lock()
	f.au.seeded = append(f.au.seeded, &audit.Entry{ID: uuid.New(), RunID: &rid, StageID: &sid, Sequence: 10, Category: CategoryStageMergeCandidateVerifyTriggered, Payload: trig})
	if consumed {
		f.au.seeded = append(f.au.seeded, &audit.Entry{ID: uuid.New(), RunID: &rid, StageID: &sid, Sequence: 11, Category: CategoryMergeCandidateVerified, Payload: []byte(`{}`)})
	}
	f.au.mu.Unlock()
	return st.ID
}

var dbNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func (f *dbFixture) verdict(t *testing.T, r *run.Run) *dispatchBudgetPayload {
	t.Helper()
	out, err := f.s.dispatchBudgetFor(context.Background(), r, dbNow)
	if err != nil {
		t.Fatalf("dispatchBudgetFor: unexpected error %v", err)
	}
	return out
}

func wantStatus(t *testing.T, got *dispatchBudgetPayload, status, source string) {
	t.Helper()
	if got.Status != status || got.BudgetSource != source {
		t.Fatalf("verdict = {status %q, source %q}, want {%q, %q}: %+v", got.Status, got.BudgetSource, status, source, got)
	}
}

// TestDispatchBudget_LiveSpec_LimitRaiseAndAdvisorySwitchReadmit (A1, ruling
// C1): the cached spec stays blocking $10 throughout; only the live spec
// changes, and the verdict follows it.
func TestDispatchBudget_LiveSpec_LimitRaiseAndAdvisorySwitchReadmit(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)

	got := f.verdict(t, r)
	wantStatus(t, got, dispatchBudgetBlocked, dispatchBudgetSourceLive)
	if got.Budget == nil || got.Budget.LimitUSD != 10 || got.Budget.SpentUSD != 15 || got.Budget.Period != "weekly" {
		t.Fatalf("blocked detail = %+v, want weekly 15 of 10", got.Budget)
	}

	f.setLive(dbSpec(dbBlocking(20)))
	wantStatus(t, f.verdict(t, r), dispatchBudgetClear, dispatchBudgetSourceLive)

	f.setLive(dbSpec(dbAdvisory(10)))
	wantStatus(t, f.verdict(t, r), dispatchBudgetClear, dispatchBudgetSourceLive)

	// The live spec turns a cached-advisory budget blocking.
	r2 := f.seedRun(dbSpec(dbAdvisory(10)), nil)
	f.setLive(dbSpec(dbBlocking(10)))
	wantStatus(t, f.verdict(t, r2), dispatchBudgetBlocked, dispatchBudgetSourceLive)
}

// TestDispatchBudget_LiveSpecUnreadable_FailsClosed (A2, reworked for D2):
// every live-read failure mode falls back to the admission spec. A blocking
// admission spec fails closed with the mode's reason; an advisory-only
// admission spec clears with source admission_spec_fallback.
func TestDispatchBudget_LiveSpecUnreadable_FailsClosed(t *testing.T) {
	modes := []struct {
		name   string
		break_ func(f *dbFixture)
		reason string
	}{
		{"installation_500", func(f *dbFixture) { f.gh.installationStatus = http.StatusInternalServerError }, budgetCheckLiveInstallationUnresolved},
		{"contents_500", func(f *dbFixture) {
			f.gh.specStatus = http.StatusInternalServerError
			f.gh.specBody = `{"message":"boom"}`
		}, budgetCheckLiveUnavailable},
		{"contents_404", func(f *dbFixture) { f.gh.specStatus = http.StatusNotFound; f.gh.specBody = `{"message":"Not Found"}` }, budgetCheckLiveNotFound},
		{"unparseable_yaml", func(f *dbFixture) { f.setLive("version: [\n") }, budgetCheckLiveInvalid},
		{"workflow_absent", func(f *dbFixture) {
			f.setLive(strings.Replace(dbSpec(dbBlocking(10)), "feature_change", "other_flow", 1))
		}, budgetCheckWorkflowAbsent},
	}
	for _, m := range modes {
		t.Run(m.name+"/blocking_admission_fails_closed", func(t *testing.T) {
			// Under its limit: a fall-through to the cached spec would clear.
			f := newDBFixture(t, dbSpec(dbBlocking(100)), dbOpts{})
			f.spent(15)
			m.break_(f)
			r := f.seedRun(dbSpec(dbBlocking(100)), nil)
			_, err := f.s.dispatchBudgetFor(context.Background(), r, dbNow)
			var bce *budgetCheckError
			if !errors.As(err, &bce) || bce.Reason != m.reason {
				t.Fatalf("err = %v, want *budgetCheckError reason %s", err, m.reason)
			}
			w := getRunAsWithQuery(t, f.s, testOperatorIdentity(), r.ID, "include=dispatch_budget")
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET status = %d, want 503:\n%s", w.Code, w.Body.String())
			}
			env := decodeErrEnvelope(t, w)
			if env.Error.Code != budgetCheckFailedCode || env.Error.Details["reason"] != m.reason {
				t.Fatalf("503 = %+v, want budget_check_failed reason %s", env.Error, m.reason)
			}
		})
		t.Run(m.name+"/advisory_admission_clears", func(t *testing.T) {
			f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
			f.spent(15)
			m.break_(f)
			r := f.seedRun(dbSpec(dbAdvisory(10)), nil)
			wantStatus(t, f.verdict(t, r), dispatchBudgetClear, dispatchBudgetSourceAdmissionFallback)
		})
	}
}

// TestDispatchBudget_UndecidableAdmissionSpec_LiveReadFailed_Clears pins the
// D2 verdict when the live read fails AND the admission spec cannot be
// evaluated: an empty runs.workflow_spec (a legacy row), an unparseable one,
// or one without the run's workflow. admissionSpecBudgets yields no budgets,
// so the admission spec "declares no blocking budget" and the verdict CLEARS
// with source admission_spec_fallback, exactly as for an advisory-only
// admission spec (D2: fail closed ONLY when the admission spec declares a
// blocking budget). The live spec here is blocking and over its limit, so a
// fail-closed reading of D2 would answer 503 budget_check_failed instead;
// that alternative is an operator ruling, not this test's.
func TestDispatchBudget_UndecidableAdmissionSpec_LiveReadFailed_Clears(t *testing.T) {
	for _, tc := range []struct{ name, admission string }{
		{"empty_admission_spec", ""},
		{"unparseable_admission_spec", "version: [\n"},
		{"workflow_absent_from_admission_spec", strings.Replace(dbSpec(dbBlocking(10)), "feature_change", "other_flow", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
			f.spent(15)
			f.gh.specStatus = http.StatusInternalServerError
			f.gh.specBody = `{"message":"boom"}`
			r := f.seedRun(tc.admission, nil)
			wantStatus(t, f.verdict(t, r), dispatchBudgetClear, dispatchBudgetSourceAdmissionFallback)
			if f.gh.specCalls == 0 {
				t.Fatal("live spec was never read; the row does not exercise the read-failure fallback")
			}
			w := getRunAsWithQuery(t, f.s, testOperatorIdentity(), r.ID, "include=dispatch_budget")
			if w.Code != http.StatusOK {
				t.Fatalf("GET status = %d, want 200 clear:\n%s", w.Code, w.Body.String())
			}
		})
	}
}

type errEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeErrEnvelope(t *testing.T, w *httptest.ResponseRecorder) errEnvelope {
	t.Helper()
	var env errEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	return env
}

// TestDispatchBudget_NoLiveSource_EvaluatesAdmissionSpec (A3): no GitHub
// client, or a GitLab run, evaluates the cached spec and never calls the
// stub.
func TestDispatchBudget_NoLiveSource_EvaluatesAdmissionSpec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noGitHub bool
		mutate   func(*run.Run)
	}{
		{"nil_github_client", true, nil},
		{"gitlab_run", false, func(r *run.Run) { ref := "gitlab:42"; r.InstallationRef = &ref }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFixture(t, dbSpec(dbAdvisory(10)), dbOpts{noGitHub: tc.noGitHub})
			f.spent(15)
			r := f.seedRun(dbSpec(dbBlocking(10)), tc.mutate)
			wantStatus(t, f.verdict(t, r), dispatchBudgetBlocked, dispatchBudgetSourceAdmission)
			if f.gh.specCalls != 0 {
				t.Errorf("spec stub called %d times, want 0", f.gh.specCalls)
			}
		})
	}
}

// TestDispatchBudget_EveryBudgetEvaluated (B1): Budgets[0] advisory over,
// Budgets[1] blocking monthly over; a Budgets[0]-only evaluation clears.
func TestDispatchBudget_EveryBudgetEvaluated(t *testing.T) {
	live := dbSpec(dbBudget{"weekly", 10, "advisory"}, dbBudget{"monthly", 10, "blocking"})
	f := newDBFixture(t, live, dbOpts{})
	f.spent(15)
	r := f.seedRun(live, nil)
	got := f.verdict(t, r)
	wantStatus(t, got, dispatchBudgetBlocked, dispatchBudgetSourceLive)
	if got.Budget.Period != "monthly" {
		t.Errorf("offending period = %q, want monthly", got.Budget.Period)
	}
}

// TestDispatchBudget_VerdictMatchesBudgetTier (B2, C1 parity): the dispatch
// verdict and GET /v0/runs/{id}/budget agree on blocked-ness and limit.
func TestDispatchBudget_VerdictMatchesBudgetTier(t *testing.T) {
	for _, tc := range []struct {
		name     string
		live     string
		override float64
	}{
		{"live_raise", dbSpec(dbBlocking(20)), 0},
		{"advisory_switch", dbSpec(dbAdvisory(10)), 0},
		{"second_budget_blocking_over", dbSpec(dbBudget{"weekly", 100, "advisory"}, dbBudget{"monthly", 10, "blocking"}), 0},
		{"limit_override_100", dbSpec(dbBlocking(10)), 100},
		{"blocking_over", dbSpec(dbBlocking(10)), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFixture(t, tc.live, dbOpts{limitOverride: tc.override})
			f.spent(15)
			r := f.seedRun(dbSpec(dbBlocking(10)), nil)
			v, err := f.s.dispatchBudgetFor(context.Background(), r, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			st, err := f.s.runBudgetStatus(context.Background(), r.ID, true)
			if err != nil || st == nil {
				t.Fatalf("runBudgetStatus = %+v, %v", st, err)
			}
			tierBlocks := st.Enforcement == "blocking" && (st.Tier == "over" || st.Tier == "ack_required" || st.Tier == "page")
			if (v.Status == dispatchBudgetBlocked) != tierBlocks {
				t.Fatalf("verdict %q vs budget tier %q/%s disagree", v.Status, st.Tier, st.Enforcement)
			}
			if v.Budget != nil && v.Budget.LimitUSD != st.LimitUSD {
				t.Errorf("limit_usd: verdict %v, budget %v", v.Budget.LimitUSD, st.LimitUSD)
			}
		})
	}
}

// TestDispatchBudget_AdvisoryOverLimit_Clear (B4): the summer is never called.
func TestDispatchBudget_AdvisoryOverLimit_Clear(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbAdvisory(10)), dbOpts{})
	f.spent(999)
	r := f.seedRun(dbSpec(dbAdvisory(10)), nil)
	wantStatus(t, f.verdict(t, r), dispatchBudgetClear, dispatchBudgetSourceLive)
	if f.repo.sumCalls != 0 {
		t.Errorf("summer called %d times, want 0", f.repo.sumCalls)
	}
}

// TestDispatchBudget_BlockingUnderLimit_Clear (B5).
func TestDispatchBudget_BlockingUnderLimit_Clear(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(5)
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)
	wantStatus(t, f.verdict(t, r), dispatchBudgetClear, dispatchBudgetSourceLive)
}

// TestDispatchBudget_PeriodRollover_Clear (B6): spend lands in week P only.
func TestDispatchBudget_PeriodRollover_Clear(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.repo.spend = func(from, to time.Time) float64 {
		if !dbNow.Before(from) && dbNow.Before(to) {
			return 15
		}
		return 0
	}
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)
	wantStatus(t, f.verdict(t, r), dispatchBudgetBlocked, dispatchBudgetSourceLive)
	next, err := f.s.dispatchBudgetFor(context.Background(), r, dbNow.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, next, dispatchBudgetClear, dispatchBudgetSourceLive)
}

// TestDispatchBudget_OverrideBoundRunIsExempt (C1).
func TestDispatchBudget_OverrideBoundRunIsExempt(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)
	f.bind(r.ID, r.Repo, r.WorkflowID)
	got := f.verdict(t, r)
	if got.Status != dispatchBudgetExempt || got.ExemptReason != dispatchBudgetExemptOverride || got.OverrideRunID != r.ID.String() {
		t.Fatalf("verdict = %+v, want exempt budget_override %s", got, r.ID)
	}
}

// TestDispatchBudget_OverrideBoundDecompositionDescendantIsExempt (C2).
func TestDispatchBudget_OverrideBoundDecompositionDescendantIsExempt(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	root := f.seedRun(dbSpec(dbBlocking(10)), nil)
	f.bind(root.ID, root.Repo, root.WorkflowID)
	child := f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { id := root.ID; r.DecomposedFrom = &id })
	grand := f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { id := child.ID; r.DecomposedFrom = &id })
	for _, r := range []*run.Run{child, grand} {
		got := f.verdict(t, r)
		if got.Status != dispatchBudgetExempt || got.OverrideRunID != root.ID.String() {
			t.Fatalf("descendant %s verdict = %+v, want exempt via %s", r.ID, got, root.ID)
		}
	}
}

// TestDispatchBudget_BindingMustMatchRepoAndWorkflow (D5(d)): a binding row on
// an ancestor whose grant names another repo or workflow does not exempt.
func TestDispatchBudget_BindingMustMatchRepoAndWorkflow(t *testing.T) {
	for _, tc := range []struct{ name, repo, wf string }{
		{"other_repo", "x/other", "feature_change"},
		{"other_workflow", "x/y", "other_flow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
			f.spent(15)
			root := f.seedRun(dbSpec(dbBlocking(10)), nil)
			f.bind(root.ID, tc.repo, tc.wf)
			child := f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { id := root.ID; r.DecomposedFrom = &id })
			wantStatus(t, f.verdict(t, child), dispatchBudgetBlocked, dispatchBudgetSourceLive)
		})
	}
}

// TestDispatchBudget_ParentRunIDChildIsNotExempt (C3, ruling C2).
func TestDispatchBudget_ParentRunIDChildIsNotExempt(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	bound := f.seedRun(dbSpec(dbBlocking(10)), nil)
	f.bind(bound.ID, bound.Repo, bound.WorkflowID)
	retry := f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { id := bound.ID; r.ParentRunID = &id })
	wantStatus(t, f.verdict(t, retry), dispatchBudgetBlocked, dispatchBudgetSourceLive)
}

// TestDispatchBudget_BrokenAncestry_NoExemption (C4): the bound root is never
// reached, so each reads blocked.
func TestDispatchBudget_BrokenAncestry_NoExemption(t *testing.T) {
	t.Run("missing_parent", func(t *testing.T) {
		f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
		f.spent(15)
		r := f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { id := uuid.New(); r.DecomposedFrom = &id })
		wantStatus(t, f.verdict(t, r), dispatchBudgetBlocked, dispatchBudgetSourceLive)
	})
	t.Run("cycle", func(t *testing.T) {
		f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
		f.spent(15)
		a := f.seedRun(dbSpec(dbBlocking(10)), nil)
		b := f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { id := a.ID; r.DecomposedFrom = &id })
		bid := b.ID
		a.DecomposedFrom = &bid
		wantStatus(t, f.verdict(t, b), dispatchBudgetBlocked, dispatchBudgetSourceLive)
	})
	t.Run("depth_9", func(t *testing.T) {
		f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
		f.spent(15)
		root := f.seedRun(dbSpec(dbBlocking(10)), nil)
		f.bind(root.ID, root.Repo, root.WorkflowID)
		cur := root
		for i := 0; i < maxCampaignAncestorDepth+1; i++ {
			parent := cur.ID
			cur = f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { r.DecomposedFrom = &parent })
		}
		wantStatus(t, f.verdict(t, cur), dispatchBudgetBlocked, dispatchBudgetSourceLive)
	})
}

// TestDispatchBudget_EightHopAncestorIsExempt: the walk reaches a bound
// ancestor exactly maxCampaignAncestorDepth hops up; only the ninth hop is
// refused (depth_9 above).
func TestDispatchBudget_EightHopAncestorIsExempt(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	root := f.seedRun(dbSpec(dbBlocking(10)), nil)
	f.bind(root.ID, root.Repo, root.WorkflowID)
	cur := root
	for i := 0; i < maxCampaignAncestorDepth; i++ {
		parent := cur.ID
		cur = f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { r.DecomposedFrom = &parent })
	}
	got := f.verdict(t, cur)
	if got.Status != dispatchBudgetExempt || got.OverrideRunID != root.ID.String() {
		t.Fatalf("%d-hop descendant verdict = %+v, want exempt via %s", maxCampaignAncestorDepth, got, root.ID)
	}
}

// TestDispatchBudget_AncestorReadError_FailsClosed: an ancestor GetRun error
// other than run.ErrNotFound is a binding read error, not a missing parent. A
// verdict that would be blocked is undecided (override_binding_unreadable),
// never a guessed blocked or exempt; a clear verdict still clears.
func TestDispatchBudget_AncestorReadError_FailsClosed(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	root := f.seedRun(dbSpec(dbBlocking(10)), nil)
	f.bind(root.ID, root.Repo, root.WorkflowID)
	child := f.seedRun(dbSpec(dbBlocking(10)), func(r *run.Run) { id := root.ID; r.DecomposedFrom = &id })
	f.repo.getRunErr = map[uuid.UUID]error{root.ID: errors.New("pg: connection reset")}
	_, err := f.s.dispatchBudgetFor(context.Background(), child, dbNow)
	var bce *budgetCheckError
	if !errors.As(err, &bce) || bce.Reason != budgetCheckBindingUnreadable {
		t.Fatalf("err = %v, want override_binding_unreadable", err)
	}
	f.spent(1)
	wantStatus(t, f.verdict(t, child), dispatchBudgetClear, dispatchBudgetSourceLive)
}

// TestDispatchBudget_BindingReadError_FailsClosed (C5).
func TestDispatchBudget_BindingReadError_FailsClosed(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	f.au.listByCategoryErrCategory = CategoryRunBudgetOverrideBound
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)
	_, err := f.s.dispatchBudgetFor(context.Background(), r, dbNow)
	var bce *budgetCheckError
	if !errors.As(err, &bce) || bce.Reason != budgetCheckBindingUnreadable {
		t.Fatalf("err = %v, want override_binding_unreadable", err)
	}
	// An unreadable binding never stalls a clear verdict.
	f.spent(1)
	wantStatus(t, f.verdict(t, r), dispatchBudgetClear, dispatchBudgetSourceLive)
}

// TestDispatchBudget_ExemptionsDecidedBeforeLiveRead (approval condition D1):
// with the live read failing and a blocking admission spec, an override-bound
// run is exempt (no forge read at all) and a verify-pass stage is still
// named on the undecided verdict.
func TestDispatchBudget_ExemptionsDecidedBeforeLiveRead(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.spent(15)
	f.gh.specStatus = http.StatusInternalServerError
	f.gh.specBody = `{"message":"boom"}`

	bound := f.seedRun(dbSpec(dbBlocking(10)), nil)
	f.bind(bound.ID, bound.Repo, bound.WorkflowID)
	got := f.verdict(t, bound)
	if got.Status != dispatchBudgetExempt {
		t.Fatalf("bound run under a failed live read = %+v, want exempt", got)
	}
	if f.gh.specCalls != 0 || f.gh.installationCalls != 0 {
		t.Errorf("exempt verdict read the forge (spec %d, installation %d), want none", f.gh.specCalls, f.gh.installationCalls)
	}

	vp := f.seedRun(dbSpec(dbBlocking(10)), nil)
	stageID := f.seedVerifyPass(vp.ID, false)
	_, err := f.s.dispatchBudgetFor(context.Background(), vp, dbNow)
	var bce *budgetCheckError
	if !errors.As(err, &bce) || bce.Reason != budgetCheckLiveUnavailable {
		t.Fatalf("err = %v, want live_spec_unavailable", err)
	}
	if len(bce.VerifyPassStageIDs) != 1 || bce.VerifyPassStageIDs[0] != stageID.String() {
		t.Fatalf("undecided verdict verify-pass stages = %v, want [%s]", bce.VerifyPassStageIDs, stageID)
	}
}

// TestDispatchBudget_VerifyPassStageListed (E1).
func TestDispatchBudget_VerifyPassStageListed(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.au.stampSequence = true
	f.spent(15)
	live := f.seedRun(dbSpec(dbBlocking(10)), nil)
	sid := f.seedVerifyPass(live.ID, false)
	got := f.verdict(t, live)
	wantStatus(t, got, dispatchBudgetBlocked, dispatchBudgetSourceLive)
	if len(got.VerifyPassStageIDs) != 1 || got.VerifyPassStageIDs[0] != sid.String() {
		t.Fatalf("verify_pass_stage_ids = %v, want [%s]", got.VerifyPassStageIDs, sid)
	}
	consumed := f.seedRun(dbSpec(dbBlocking(10)), nil)
	f.seedVerifyPass(consumed.ID, true)
	if got := f.verdict(t, consumed); len(got.VerifyPassStageIDs) != 0 {
		t.Fatalf("consumed trigger listed: %v", got.VerifyPassStageIDs)
	}
}

// --- handler: GET /v0/runs/{id}?include=dispatch_budget ---

// TestGetRun_IncludeDispatchBudget_Served (E2).
func TestGetRun_IncludeDispatchBudget_Served(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{noGitHub: true})
	f.spent(15)
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)
	w := getRunAsWithQuery(t, f.s, testOperatorIdentity(), r.ID, "include=dispatch_budget")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var body struct {
		DispatchBudget *dispatchBudgetPayload `json:"dispatch_budget"`
		CampaignLink   json.RawMessage        `json:"campaign_link"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.DispatchBudget == nil || body.DispatchBudget.Status != dispatchBudgetBlocked || body.DispatchBudget.BudgetSource != dispatchBudgetSourceAdmission {
		t.Fatalf("dispatch_budget = %+v, want blocked admission_spec:\n%s", body.DispatchBudget, w.Body.String())
	}
	if w := getRunAsWithQuery(t, f.s, testOperatorIdentity(), r.ID, ""); strings.Contains(w.Body.String(), `"dispatch_budget"`) {
		t.Errorf("dispatch_budget served without the include:\n%s", w.Body.String())
	}
}

// TestGetRun_IncludeDispatchBudget_SumErrorIs503 (E2, D5(c)): the body names
// the enumerated reason and never the raw cause.
func TestGetRun_IncludeDispatchBudget_SumErrorIs503(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{noGitHub: true})
	f.repo.sumErr = errors.New("pg: secret-host connection refused")
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)
	w := getRunAsWithQuery(t, f.s, testOperatorIdentity(), r.ID, "include=dispatch_budget")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	env := decodeErrEnvelope(t, w)
	if env.Error.Code != budgetCheckFailedCode || env.Error.Details["reason"] != budgetCheckSpendSumFailed {
		t.Fatalf("503 = %+v", env.Error)
	}
	if strings.Contains(w.Body.String(), "secret-host") {
		t.Errorf("503 body leaked the raw cause:\n%s", w.Body.String())
	}
}

// TestGetRun_IncludeDispatchBudget_503CarriesVerifyPassStageIDs (approval
// condition D1, amendments 0f18ebd4 / a09939e2): an undecided verdict's 503
// carries the verify-pass stage ids THROUGH writeError's 5xx default-deny
// redactor, so the dispatcher can still spawn a verify-only pass while the
// live read errors. The raw cause never reaches the body (D5(c)), and a run
// with no verify pass carries no key.
func TestGetRun_IncludeDispatchBudget_503CarriesVerifyPassStageIDs(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{})
	f.au.stampSequence = true
	f.spent(15)
	f.gh.specStatus = http.StatusInternalServerError
	f.gh.specBody = `{"message":"secret-forge-cause"}`

	vp := f.seedRun(dbSpec(dbBlocking(10)), nil)
	sid := f.seedVerifyPass(vp.ID, false)
	w := getRunAsWithQuery(t, f.s, testOperatorIdentity(), vp.ID, "include=dispatch_budget")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	env := decodeErrEnvelope(t, w)
	if env.Error.Code != budgetCheckFailedCode || env.Error.Details["reason"] != budgetCheckLiveUnavailable {
		t.Fatalf("503 = %+v, want budget_check_failed reason %s", env.Error, budgetCheckLiveUnavailable)
	}
	ids, _ := env.Error.Details["verify_pass_stage_ids"].([]any)
	if len(ids) != 1 || ids[0] != sid.String() {
		t.Fatalf("503 details.verify_pass_stage_ids = %v, want [%s]:\n%s", env.Error.Details["verify_pass_stage_ids"], sid, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-forge-cause") {
		t.Errorf("503 body leaked the raw cause:\n%s", w.Body.String())
	}

	plain := f.seedRun(dbSpec(dbBlocking(10)), nil)
	w = getRunAsWithQuery(t, f.s, testOperatorIdentity(), plain.ID, "include=dispatch_budget")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("plain run status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if _, ok := decodeErrEnvelope(t, w).Error.Details["verify_pass_stage_ids"]; ok {
		t.Errorf("503 of a run with no verify pass carries verify_pass_stage_ids:\n%s", w.Body.String())
	}
}

// TestGetRun_IncludeDispatchBudget_RunTokenForbidden (E2).
func TestGetRun_IncludeDispatchBudget_RunTokenForbidden(t *testing.T) {
	f := newDBFixture(t, dbSpec(dbBlocking(10)), dbOpts{noGitHub: true})
	r := f.seedRun(dbSpec(dbBlocking(10)), nil)
	runToken := Identity{Subject: "mcp:run:" + r.ID.String(), TokenID: "mcp-tok", Scopes: []string{"read:runs"}, AccountID: testOperatorAccountID}
	w := getRunAsWithQuery(t, f.s, runToken, r.ID, "include=dispatch_budget")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"run_token_forbidden"`) || !strings.Contains(w.Body.String(), "include=dispatch_budget") {
		t.Fatalf("status = %d, want 403 run_token_forbidden naming dispatch_budget:\n%s", w.Code, w.Body.String())
	}
}

// TestGetRun_Include_UnknownValueListsBoth (E2).
func TestGetRun_Include_UnknownValueListsBoth(t *testing.T) {
	f := newDBFixture(t, dbSpec(), dbOpts{noGitHub: true})
	r := f.seedRun(dbSpec(), nil)
	w := getRunAsWithQuery(t, f.s, testOperatorIdentity(), r.ID, "include=dispatch_budget,bogus")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	env := decodeErrEnvelope(t, w)
	if !strings.Contains(env.Error.Message, "campaign_link") || !strings.Contains(env.Error.Message, "dispatch_budget") {
		t.Errorf("message %q must name both values", env.Error.Message)
	}
	allowed, _ := env.Error.Details["allowed"].([]any)
	if len(allowed) != 2 || allowed[0] != runIncludeCampaignLink || allowed[1] != runIncludeDispatchBudget {
		t.Errorf("details.allowed = %v, want both", env.Error.Details["allowed"])
	}
}

// TestRunBudgetOverrideBound_WithheldFromRunBoundAuditRead (D4, D3).
func TestRunBudgetOverrideBound_WithheldFromRunBoundAuditRead(t *testing.T) {
	f := newDBFixture(t, dbSpec(), dbOpts{noGitHub: true})
	r := f.seedRun(dbSpec(), nil)
	f.bind(r.ID, r.Repo, r.WorkflowID)
	read := func(id Identity) string {
		req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+r.ID.String()+"/audit", nil)
		req.SetPathValue("run_id", r.ID.String())
		w := httptest.NewRecorder()
		f.s.handleListRunAudit(w, withIdentity(req, id))
		if w.Code != http.StatusOK {
			t.Fatalf("audit read status = %d:\n%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	runToken := Identity{Subject: "mcp:run:" + r.ID.String(), TokenID: "mcp-tok", Scopes: []string{"read:audit"}, AccountID: testOperatorAccountID}
	if body := read(runToken); strings.Contains(body, CategoryRunBudgetOverrideBound) {
		t.Errorf("run-bound audit read carried %s:\n%s", CategoryRunBudgetOverrideBound, body)
	}
	if body := read(testOperatorIdentity()); !strings.Contains(body, CategoryRunBudgetOverrideBound) {
		t.Errorf("operator audit read omitted %s:\n%s", CategoryRunBudgetOverrideBound, body)
	}
}
