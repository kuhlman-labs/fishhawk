package mergeoutcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// tickNow is the fixture clock's start.
var tickNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// tickAudit is memAudit plus ListAll (category filter), the CI ticker's scan.
type tickAudit struct {
	memAudit
	listAllErr error
	appendErr  error
}

func (a *tickAudit) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	if a.listAllErr != nil {
		return nil, a.listAllErr
	}
	var out []*audit.Entry
	for _, e := range a.snapshot() {
		if p.Category == nil || e.Category == *p.Category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *tickAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.appendErr != nil && p.Category == CategoryRunMergeCIObserved {
		return nil, a.appendErr
	}
	return a.memAudit.AppendChained(ctx, p)
}

// seed appends a chain row at ts.
func (a *tickAudit) seed(runID uuid.UUID, category string, ts time.Time, payload string) {
	r := runID
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, &audit.Entry{ID: uuid.New(), RunID: &r, Timestamp: ts, Category: category, Payload: json.RawMessage(payload)})
}

type tickRuns struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*run.Run
	err  error
}

func (r *tickRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	got, ok := r.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return got, nil
}

// tickForge serves PRs by number and check runs by SHA, counting calls.
type tickForge struct {
	mu         sync.Mutex
	prs        map[int]*forge.PullRequest
	prErr      map[int]error // persistent per-PR error
	checks     map[string][]githubclient.CheckRunSummary
	truncated  bool
	checksErr  error
	getCalls   map[int]int
	checkCalls int
}

func (f *tickForge) GetPullRequest(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, number int) (*forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getCalls == nil {
		f.getCalls = map[int]int{}
	}
	f.getCalls[number]++
	if err := f.prErr[number]; err != nil {
		return nil, err
	}
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("%w: get pr", forge.ErrNotFound)
	}
	return pr, nil
}

func (f *tickForge) ListCheckRunsForRef(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, ref string) ([]githubclient.CheckRunSummary, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	if f.checksErr != nil {
		return nil, false, f.checksErr
	}
	return f.checks[ref], f.truncated, nil
}

func (f *tickForge) gets(number int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls[number]
}

func (f *tickForge) totalGets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.getCalls {
		n += c
	}
	return n
}

type tickFixture struct {
	t      *testing.T
	ticker *CITicker
	audit  *tickAudit
	runs   *tickRuns
	forge  *tickForge
	now    time.Time
	byPR   map[int]uuid.UUID
}

func newTickFixture(t *testing.T) *tickFixture {
	t.Helper()
	f := &tickFixture{
		t:     t,
		audit: &tickAudit{},
		runs:  &tickRuns{runs: map[uuid.UUID]*run.Run{}},
		forge: &tickForge{prs: map[int]*forge.PullRequest{}, prErr: map[int]error{}, checks: map[string][]githubclient.CheckRunSummary{}},
		now:   tickNow,
		byPR:  map[int]uuid.UUID{},
	}
	f.ticker = &CITicker{
		Runs: f.runs, Audit: f.audit, Forge: f.forge,
		Logger:       slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		Interval:     10 * time.Minute,
		Window:       6 * time.Hour,
		RetryBackoff: time.Millisecond,
		Now:          func() time.Time { return f.now },
	}
	return f
}

func shaFor(n int) string { return fmt.Sprintf("%040d", n) }

func prURLFor(n int) string { return fmt.Sprintf("https://github.com/acme/widgets/pull/%d", n) }

// addMergedRun seeds a github run on PR n, a pr_merged row at mergedAt, the
// forge's merged PR, and a green CI Pass on the merge commit.
func (f *tickFixture) addMergedRun(n int, mergedAt time.Time) uuid.UUID {
	id := uuid.New()
	inst := int64(9)
	u := prURLFor(n)
	f.runs.runs[id] = &run.Run{
		ID: id, Repo: "acme/widgets", PullRequestURL: &u, InstallationID: &inst,
		RequiredChecksSnapshot: snap("CI Pass", auditcheckpublisher.CheckName),
		CreatedAt:              mergedAt.Add(-time.Hour), UpdatedAt: mergedAt,
	}
	f.audit.seed(id, "pr_merged", mergedAt, `{}`)
	m := mergedAt
	f.forge.prs[n] = &forge.PullRequest{NodeID: fmt.Sprintf("PR_%d", n), Merged: true, State: "closed", MergeCommitSHA: shaFor(n), MergedAt: &m}
	done := mergedAt.Add(30 * time.Minute)
	f.forge.checks[shaFor(n)] = []githubclient.CheckRunSummary{{ID: int64(n), Name: "CI Pass", Status: "completed",
		Conclusion: "success", StartedAt: &mergedAt, CompletedAt: &done, CheckSuiteID: 1}}
	f.byPR[n] = id
	return id
}

func (f *tickFixture) ciRows(id uuid.UUID) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, e := range f.audit.rows(id, CategoryRunMergeCIObserved) {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			f.t.Fatalf("payload: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func (f *tickFixture) tick() CITickSummary { return f.ticker.Tick(context.Background()) }

func TestTick_RecordsGreenAtMaturity(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	sum := f.tick()
	rows := f.ciRows(id)
	if len(rows) != 1 || sum.Recorded != 1 || sum.Evaluated != 1 {
		t.Fatalf("rows = %v sum = %+v, want one row", rows, sum)
	}
	r := rows[0]
	want := map[string]any{
		"conclusion": "green", "merge_commit_sha": shaFor(42), "pull_request_number": float64(42),
		"pull_request_url": prURLFor(42), "repo": "acme/widgets", "source": "github_check_runs_poll",
		"matures_at": "2026-09-10T11:00:00Z", "observed_at": "2026-09-10T12:00:00Z",
		"merged_at": "2026-09-10T05:00:00Z", "merge_evidence_at": "2026-09-10T05:00:00Z",
		"window_seconds": float64(21600), "missing_reason": "", "observation_error_class": "",
		"required_checks_source": "run_snapshot", "checks_truncated": false, "checks_observed": float64(1),
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s = %v, want %v", k, r[k], v)
		}
	}
	if fmt.Sprint(r["excluded_checks"]) != "[fishhawk_audit_complete]" || fmt.Sprint(r["passed_checks"]) != "[CI Pass]" {
		t.Errorf("excluded = %v passed = %v", r["excluded_checks"], r["passed_checks"])
	}
	e := f.audit.rows(id, CategoryRunMergeCIObserved)[0]
	if e.ActorSubject == nil || *e.ActorSubject != ActorSubject || e.ActorKind == nil || *e.ActorKind != audit.ActorSystem {
		t.Errorf("actor = %v/%v, want system/%s", e.ActorKind, e.ActorSubject, ActorSubject)
	}
}

// No agent text as input: the recorded payload's key set equals the
// forge-attested allow-list, with no message/body/title/reason key.
func TestRunMergeCIObserved_PayloadKeyAllowList(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.tick()
	rows := f.ciRows(id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var keys []string
	for k := range rows[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"checks_observed", "checks_truncated", "conclusion", "excluded_checks", "failed_checks",
		"matures_at", "merge_commit_sha", "merge_evidence_at", "merged_at", "missing_check_reasons", "missing_checks",
		"missing_reason", "observation_error_class", "observed_at", "passed_checks", "pull_request_number",
		"pull_request_url", "repo", "required_checks", "required_checks_source", "source", "window_seconds"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("payload keys = %v\nwant           %v", keys, want)
	}
}

func TestTick_BeforeMaturity_RecordsNothingAndSkipsForge(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-time.Hour))
	sum := f.tick()
	if rows := f.ciRows(id); len(rows) != 0 || f.forge.totalGets() != 0 || f.forge.checkCalls != 0 || sum.Candidates != 0 {
		t.Fatalf("rows = %v gets = %d checks = %d sum = %+v, want nothing before maturity", rows, f.forge.totalGets(), f.forge.checkCalls, sum)
	}
}

// The chain's evidence is mature but the forge's merged_at is not: the run is
// held until the forge maturity, with no failure counted and no row.
func TestTick_ForgeMergedAtNotMature_HeldWithoutFailure(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	late := tickNow.Add(-time.Hour)
	f.forge.prs[42].MergedAt = &late
	sum := f.tick()
	if rows := f.ciRows(id); len(rows) != 0 || sum.Failed != 0 || f.forge.checkCalls != 0 {
		t.Fatalf("rows = %v sum = %+v checks = %d, want held without a failure", rows, sum, f.forge.checkCalls)
	}
	f.now = tickNow.Add(time.Hour)
	if sum := f.tick(); sum.Deferred != 1 || f.forge.gets(42) != 1 {
		t.Fatalf("sum = %+v gets = %d, want held (no forge call) before forge maturity", sum, f.forge.gets(42))
	}
	f.now = tickNow.Add(5*time.Hour + time.Minute)
	f.tick()
	rows := f.ciRows(id)
	if len(rows) != 1 || rows[0]["matures_at"] != "2026-09-10T17:00:00Z" {
		t.Fatalf("rows = %v, want one row matured at the forge merged_at + window", rows)
	}
}

func TestTick_AlreadyObserved_NoForgeCalls(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.audit.seed(id, CategoryRunMergeCIObserved, tickNow.Add(-time.Minute), fmt.Sprintf(`{"merge_commit_sha":%q}`, shaFor(42)))
	f.tick()
	if f.forge.totalGets() != 0 || len(f.ciRows(id)) != 1 {
		t.Fatalf("gets = %d rows = %d, want 0 forge calls and the seeded row only", f.forge.totalGets(), len(f.ciRows(id)))
	}
}

func TestTick_MaxPerTick_DrainsOverTicks(t *testing.T) {
	f := newTickFixture(t)
	f.ticker.MaxPerTick = 2
	for n := 1; n <= 3; n++ {
		f.addMergedRun(n, tickNow.Add(-time.Duration(6+n)*time.Hour))
	}
	count := func() int {
		total := 0
		for _, id := range f.byPR {
			total += len(f.ciRows(id))
		}
		return total
	}
	if sum := f.tick(); count() != 2 || sum.Evaluated != 2 {
		t.Fatalf("after tick 1: rows = %d sum = %+v, want 2", count(), sum)
	}
	f.tick()
	if count() != 3 {
		t.Fatalf("after tick 2: rows = %d, want 3", count())
	}
}

func TestTick_GitLabRun_Skipped(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	ref := "gitlab:5"
	f.runs.runs[id].InstallationRef = &ref
	f.tick()
	f.tick()
	if f.forge.totalGets() != 0 || len(f.ciRows(id)) != 0 {
		t.Fatalf("gets = %d rows = %d, want a non-github run skipped", f.forge.totalGets(), len(f.ciRows(id)))
	}
}

func TestTick_NoMergeEvidence_Skipped(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.audit.mu.Lock()
	f.audit.entries = nil // drop the pr_merged row
	f.audit.mu.Unlock()
	f.audit.seed(id, "stage_completed", tickNow.Add(-7*time.Hour), `{}`)
	if sum := f.tick(); sum.Candidates != 0 || f.forge.totalGets() != 0 {
		t.Fatalf("sum = %+v gets = %d, want no candidate without merge evidence", sum, f.forge.totalGets())
	}
}

// Every merge-evidence category counts, and the EARLIEST row is the run's
// merge-evidence time.
func TestTick_MergeEvidenceCategories_EarliestWins(t *testing.T) {
	for _, cat := range []string{"post_merge_observed", "merge_observation_recorded"} {
		f := newTickFixture(t)
		id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
		f.audit.mu.Lock()
		f.audit.entries = nil
		f.audit.mu.Unlock()
		f.audit.seed(id, cat, tickNow.Add(-2*time.Hour), `{}`)
		f.audit.seed(id, cat, tickNow.Add(-8*time.Hour), `{}`)
		f.tick()
		rows := f.ciRows(id)
		if len(rows) != 1 || rows[0]["merge_evidence_at"] != "2026-09-10T04:00:00Z" {
			t.Fatalf("%s: rows = %v, want one row keyed on the earliest evidence", cat, rows)
		}
	}
}

func TestTick_ForgeError_RetriesNextTick(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.forge.prErr[42] = fmt.Errorf("%w: get pr", forge.ErrNotFound)
	if sum := f.tick(); sum.Failed != 1 || len(f.ciRows(id)) != 0 {
		t.Fatalf("sum = %+v rows = %d, want a failure and no row", sum, len(f.ciRows(id)))
	}
	delete(f.forge.prErr, 42)
	f.now = tickNow.Add(10 * time.Minute)
	f.tick()
	if len(f.ciRows(id)) != 1 {
		t.Fatalf("rows = %d, want the run recorded once the forge recovers", len(f.ciRows(id)))
	}
}

// Operator condition 4: eligibility keys on MERGE-EVIDENCE time, so a run
// created long before it merged is evaluated.
func TestTick_OldCreatedRecentlyMerged_Evaluated(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.runs.runs[id].CreatedAt = tickNow.Add(-60 * 24 * time.Hour)
	f.runs.runs[id].UpdatedAt = tickNow.Add(-60 * 24 * time.Hour)
	f.tick()
	if len(f.ciRows(id)) != 1 {
		t.Fatalf("rows = %d, want the old-created, recently-merged run recorded", len(f.ciRows(id)))
	}
}

func TestTick_MergeEvidenceOutsideLookback_Skipped(t *testing.T) {
	f := newTickFixture(t)
	f.addMergedRun(42, tickNow.Add(-30*24*time.Hour))
	if sum := f.tick(); sum.Candidates != 0 || f.forge.totalGets() != 0 {
		t.Fatalf("sum = %+v gets = %d, want evidence past the lookback skipped", sum, f.forge.totalGets())
	}
}

func TestCITicker_LookbackNeverBelowTerminalDeadline(t *testing.T) {
	tk := &CITicker{Lookback: time.Hour, Window: 6 * time.Hour}
	if got, want := tk.lookback(), 6*time.Hour+DefaultCITerminalAfter+24*time.Hour; got != want {
		t.Fatalf("lookback = %v, want %v", got, want)
	}
	if got := (&CITicker{}).lookback(); got != DefaultCILookback {
		t.Fatalf("default lookback = %v, want %v", got, DefaultCILookback)
	}
	if got := (&CITicker{Lookback: 30 * 24 * time.Hour}).lookback(); got != 30*24*time.Hour {
		t.Fatalf("wide lookback = %v, want it kept", got)
	}
}

// Operator condition 5: oldest-maturity-first — persistently failing NEWER
// runs never prevent an older mature run from being recorded.
func TestTick_OldestMaturityFirst_NewerFailuresDoNotStarve(t *testing.T) {
	f := newTickFixture(t)
	f.ticker.MaxPerTick = 2
	old := f.addMergedRun(1, tickNow.Add(-20*time.Hour))
	for n := 2; n <= 4; n++ {
		f.addMergedRun(n, tickNow.Add(-time.Duration(6+n)*time.Hour))
		f.forge.prErr[n] = fmt.Errorf("%w: get pr", forge.ErrForbidden)
	}
	f.tick()
	if len(f.ciRows(old)) != 1 {
		t.Fatalf("older run rows = %d after tick 1, want 1 (oldest maturity first)", len(f.ciRows(old)))
	}
}

// Operator condition 5: a failing run enters backoff and stops consuming the
// per-tick cap, so OLDER persistently failing runs free it within a few ticks.
func TestTick_FailingRunsBackOff_FreeTheCap(t *testing.T) {
	f := newTickFixture(t)
	f.ticker.MaxPerTick = 2
	for n := 1; n <= 2; n++ {
		f.addMergedRun(n, tickNow.Add(-time.Duration(20+n)*time.Hour))
		f.forge.prErr[n] = fmt.Errorf("%w: get pr", forge.ErrForbidden)
	}
	newer := f.addMergedRun(3, tickNow.Add(-7*time.Hour))
	f.tick()
	if len(f.ciRows(newer)) != 0 {
		t.Fatal("newer run recorded on tick 1; the cap should have gone to the older runs")
	}
	f.now = tickNow.Add(time.Minute)
	sum := f.tick()
	if len(f.ciRows(newer)) != 1 || sum.Deferred != 2 {
		t.Fatalf("tick 2: newer rows = %d sum = %+v, want the newer run recorded while the failing ones back off", len(f.ciRows(newer)), sum)
	}
	if f.forge.gets(1) != 1 || f.forge.gets(2) != 1 {
		t.Fatalf("gets = %d/%d, want each failing run tried once", f.forge.gets(1), f.forge.gets(2))
	}
}

func TestCITicker_BackoffDoublesAndCaps(t *testing.T) {
	tk := &CITicker{Interval: 10 * time.Minute, MaxBackoff: time.Hour}
	id := uuid.New()
	var got []time.Duration
	for i := 0; i < 5; i++ {
		st := tk.noteFailure(id, tickNow)
		got = append(got, st.nextAttempt.Sub(tickNow))
	}
	want := []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("backoff = %v, want %v", got, want)
	}
	if !tk.inBackoff(id, tickNow.Add(59*time.Minute)) || tk.inBackoff(id, tickNow.Add(time.Hour)) {
		t.Fatal("inBackoff disagrees with nextAttempt")
	}
	tk.clear(id)
	if tk.inBackoff(id, tickNow) {
		t.Fatal("clear left the run in backoff")
	}
	if got := (&CITicker{}).maxBackoff(); got != defaultCIMaxBackoff {
		t.Fatalf("default max backoff = %v", got)
	}
}

// Operator condition 5: a run still unobservable 7 days after maturity records
// a terminal missing/observation_failed row naming the last error class, and
// leaves the queue.
func TestTick_UnobservableSevenDaysAfterMaturity_RecordsTerminalMissing(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-8*24*time.Hour))
	f.forge.prErr[42] = fmt.Errorf("%w: get pr", forge.ErrForbidden)
	sum := f.tick()
	rows := f.ciRows(id)
	if len(rows) != 1 || sum.Terminal != 1 {
		t.Fatalf("rows = %v sum = %+v, want one terminal row", rows, sum)
	}
	r := rows[0]
	if r["conclusion"] != "missing" || r["missing_reason"] != "observation_failed" || r["observation_error_class"] != "forbidden" ||
		r["merge_commit_sha"] != "" || r["merged_at"] != nil || r["matures_at"] != "2026-09-02T18:00:00Z" {
		t.Fatalf("terminal row = %v", r)
	}
	f.now = tickNow.Add(24 * time.Hour)
	f.tick()
	if f.forge.gets(42) != 1 || len(f.ciRows(id)) != 1 {
		t.Fatalf("gets = %d rows = %d, want the run out of the queue", f.forge.gets(42), len(f.ciRows(id)))
	}
}

// A check-run listing failure past the deadline keeps the forge merge facts.
func TestTick_TerminalAfterCheckRunsFailure_CarriesMergeFacts(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-8*24*time.Hour))
	f.forge.checksErr = errors.New("githubclient: list check runs: 503: unavailable")
	f.tick()
	rows := f.ciRows(id)
	if len(rows) != 1 || rows[0]["observation_error_class"] != "transient" || rows[0]["merge_commit_sha"] != shaFor(42) ||
		rows[0]["merged_at"] == nil || rows[0]["missing_reason"] != "observation_failed" {
		t.Fatalf("rows = %v, want a terminal row carrying the merge facts", rows)
	}
	if f.forge.checkCalls != forgeAttempts {
		t.Fatalf("check calls = %d, want %d (a 5xx is retried)", f.forge.checkCalls, forgeAttempts)
	}
}

func TestTick_FailureBeforeTerminalDeadline_NoTerminalRow(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-2*24*time.Hour))
	f.forge.prErr[42] = fmt.Errorf("%w: get pr", forge.ErrForbidden)
	if sum := f.tick(); sum.Failed != 1 || len(f.ciRows(id)) != 0 {
		t.Fatalf("sum = %+v rows = %d, want a failure but no terminal row yet", sum, len(f.ciRows(id)))
	}
}

func TestTick_UnmergedOrMissingFacts_Fail(t *testing.T) {
	for name, mutate := range map[string]func(*forge.PullRequest){
		"unmerged":       func(p *forge.PullRequest) { p.Merged = false },
		"no merge sha":   func(p *forge.PullRequest) { p.MergeCommitSHA = "" },
		"no merged_at":   func(p *forge.PullRequest) { p.MergedAt = nil },
		"terminal class": func(p *forge.PullRequest) { p.Merged = false },
	} {
		f := newTickFixture(t)
		id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
		mutate(f.forge.prs[42])
		if name == "terminal class" {
			f.now = tickNow.Add(8 * 24 * time.Hour)
		}
		sum := f.tick()
		if name == "terminal class" {
			if rows := f.ciRows(id); len(rows) != 1 || rows[0]["observation_error_class"] != "pr_not_merged" {
				t.Fatalf("%s: rows = %v", name, rows)
			}
			continue
		}
		if sum.Failed != 1 || len(f.ciRows(id)) != 0 || f.forge.checkCalls != 0 {
			t.Fatalf("%s: sum = %+v rows = %d checks = %d, want a failure before listing checks", name, sum, len(f.ciRows(id)), f.forge.checkCalls)
		}
	}
}

func TestTick_RedAndTruncatedVerdictsRecorded(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.forge.checks[shaFor(42)][0].Conclusion = "failure"
	f.tick()
	if rows := f.ciRows(id); len(rows) != 1 || rows[0]["conclusion"] != "red" || fmt.Sprint(rows[0]["failed_checks"]) != "[CI Pass]" {
		t.Fatalf("rows = %v, want red", rows)
	}
	g := newTickFixture(t)
	gid := g.addMergedRun(43, tickNow.Add(-7*time.Hour))
	g.forge.truncated = true
	g.tick()
	if rows := g.ciRows(gid); len(rows) != 1 || rows[0]["conclusion"] != "missing" || rows[0]["checks_truncated"] != true {
		t.Fatalf("rows = %v, want missing/truncated", rows)
	}
}

func TestTick_AuditAppendError_Fails(t *testing.T) {
	f := newTickFixture(t)
	f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.audit.appendErr = errors.New("db down")
	if sum := f.tick(); sum.Failed != 1 || sum.Recorded != 0 {
		t.Fatalf("sum = %+v, want the append failure counted", sum)
	}
	// Past the terminal deadline the terminal append fails too; nothing lands
	// and the run stays queued.
	f.now = tickNow.Add(8 * 24 * time.Hour)
	if sum := f.tick(); sum.Terminal != 0 || sum.Failed != 1 {
		t.Fatalf("sum = %+v, want no terminal row while the store refuses", sum)
	}
}

func TestTick_AuditListError_TickSkipped(t *testing.T) {
	f := newTickFixture(t)
	f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.audit.listAllErr = errors.New("db down")
	if sum := f.tick(); sum.Candidates != 0 || f.forge.totalGets() != 0 {
		t.Fatalf("sum = %+v gets = %d, want the tick skipped", sum, f.forge.totalGets())
	}
}

// Only the OBSERVED listing fails: proceeding without it would re-evaluate an
// already-observed run, so the tick is skipped instead.
func TestTick_ObservedListError_TickSkipped(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.audit.seed(id, CategoryRunMergeCIObserved, tickNow.Add(-time.Minute), fmt.Sprintf(`{"merge_commit_sha":%q}`, shaFor(42)))
	f.ticker.Audit = &categoryErrAudit{tickAudit: f.audit, failing: CategoryRunMergeCIObserved}
	if sum := f.tick(); sum.Candidates != 0 || f.forge.totalGets() != 0 {
		t.Fatalf("sum = %+v gets = %d, want the tick skipped", sum, f.forge.totalGets())
	}
}

// Only ONE merge-evidence listing fails while another category still names
// the run: a partial evidence view would understate the earliest merge time,
// so the tick is skipped instead.
func TestTick_EvidenceListError_TickSkipped(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.audit.seed(id, "post_merge_observed", tickNow.Add(-7*time.Hour), `{}`)
	f.ticker.Audit = &categoryErrAudit{tickAudit: f.audit, failing: "pr_merged"}
	if sum := f.tick(); sum.Candidates != 0 || f.forge.totalGets() != 0 {
		t.Fatalf("sum = %+v gets = %d, want the tick skipped", sum, f.forge.totalGets())
	}
}

// categoryErrAudit fails ListAll for one category only.
type categoryErrAudit struct {
	*tickAudit
	failing string
}

func (e *categoryErrAudit) ListAll(ctx context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	if p.Category != nil && *p.Category == e.failing {
		return nil, errors.New("db down")
	}
	return e.tickAudit.ListAll(ctx, p)
}

func TestTick_RunLookupErrors(t *testing.T) {
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	f.runs.err = errors.New("db down")
	f.tick()
	if f.ticker.isSkipped(id) || f.forge.totalGets() != 0 {
		t.Fatal("a transient run-load error must not skip the run permanently")
	}
	f.runs.err = nil
	delete(f.runs.runs, id)
	f.tick()
	if !f.ticker.isSkipped(id) {
		t.Fatal("a deleted run should be skipped")
	}
}

func TestTick_ContextCancelled_StopsWithoutFailure(t *testing.T) {
	f := newTickFixture(t)
	f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sum := f.ticker.Tick(ctx); sum.Evaluated != 0 || sum.Failed != 0 {
		t.Fatalf("sum = %+v, want nothing evaluated on a cancelled context", sum)
	}
}

func TestCITargetFor(t *testing.T) {
	inst := int64(9)
	zero := int64(0)
	u := prURLFor(42)
	bad := "https://github.com/acme/widgets/issues/42"
	other := "https://github.com/acme/gadgets/pull/42"
	empty := ""
	gh, gl := "github:9", "gitlab:5"
	cases := []struct {
		name string
		r    run.Run
		want string
	}{
		{"ok", run.Run{Repo: "acme/widgets", PullRequestURL: &u, InstallationID: &inst}, ""},
		{"ok github ref", run.Run{Repo: "acme/widgets", PullRequestURL: &u, InstallationID: &inst, InstallationRef: &gh}, ""},
		{"no pr url", run.Run{Repo: "acme/widgets", InstallationID: &inst}, ciSkipNoPRURL},
		{"empty pr url", run.Run{Repo: "acme/widgets", PullRequestURL: &empty, InstallationID: &inst}, ciSkipNoPRURL},
		{"gitlab", run.Run{Repo: "acme/widgets", PullRequestURL: &u, InstallationID: &inst, InstallationRef: &gl}, ciSkipNonGitHub},
		{"no installation", run.Run{Repo: "acme/widgets", PullRequestURL: &u}, ciSkipNoInstallation},
		{"zero installation", run.Run{Repo: "acme/widgets", PullRequestURL: &u, InstallationID: &zero}, ciSkipNoInstallation},
		{"malformed", run.Run{Repo: "acme/widgets", PullRequestURL: &bad, InstallationID: &inst}, ciSkipMalformedPRURL},
		{"repo mismatch", run.Run{Repo: "acme/widgets", PullRequestURL: &other, InstallationID: &inst}, ciSkipPRRepoMismatch},
	}
	for _, tc := range cases {
		tgt, reason := ciTargetFor(&tc.r)
		if reason != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, reason, tc.want)
		}
		if tc.want == "" && (tgt.number != 42 || tgt.repo.Owner != "acme" || tgt.scope.Ref() != "9") {
			t.Errorf("%s: target = %+v", tc.name, tgt)
		}
	}
}

func TestParseGitHubPRURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://github.com/acme/widgets/pull/7":  true,
		"https://github.com/acme/widgets/pull/7/": true,
		"https://github.com/acme/widgets/pull/0":  false,
		"https://github.com/acme/widgets/pull/x":  false,
		"https://github.com/acme/pull/7":          false,
		"/acme/widgets/pull/7":                    false,
		"https://github.com//widgets/pull/7":      false,
		"://bad":                                  false,
	} {
		if _, _, got := parseGitHubPRURL(raw); got != ok {
			t.Errorf("%q: ok = %v, want %v", raw, got, ok)
		}
	}
}

func TestClassifyCIError(t *testing.T) {
	ctx := context.Background()
	cases := map[error]string{
		fmt.Errorf("%w: x", forge.ErrNotFound):     errClassNotFound,
		fmt.Errorf("%w: x", forge.ErrForbidden):    errClassForbidden,
		fmt.Errorf("%w: x", forge.ErrNotInstalled): errClassNotInstalled,
		fmt.Errorf("%w: x", forge.ErrValidation):   errClassValidation,
		errors.New("githubclient: get pr: 502: x"): errClassTransient,
		errors.New("githubclient: get pr: 409: x"): errClassForge,
	}
	for err, want := range cases {
		if got := classifyCIError(ctx, err); got != want {
			t.Errorf("%v: class = %q, want %q", err, got, want)
		}
	}
}

// Operator condition 6: the flag-to-ticker constructor names why it is not
// started, and catches a nil GitHub client before the interface wrap.
func TestNewCITicker(t *testing.T) {
	runs := &tickRuns{}
	au := &tickAudit{}
	gh := githubclient.New(staticTokens{})
	cases := []struct {
		name string
		cfg  CITickerConfig
		want string
	}{
		{"disabled", CITickerConfig{Runs: runs, Audit: au, GitHub: gh}, "disabled"},
		{"no runs", CITickerConfig{Enabled: true, Audit: au, GitHub: gh}, "RunRepo unconfigured"},
		{"no audit", CITickerConfig{Enabled: true, Runs: runs, GitHub: gh}, "AuditRepo unconfigured"},
		{"nil github", CITickerConfig{Enabled: true, Runs: runs, Audit: au, GitHub: nil}, "GitHub client unconfigured (no app id?)"},
	}
	for _, tc := range cases {
		tk, reason := NewCITicker(tc.cfg)
		if tk != nil || reason != tc.want {
			t.Errorf("%s: ticker = %v reason = %q, want nil/%q", tc.name, tk, reason, tc.want)
		}
	}
	tk, reason := NewCITicker(CITickerConfig{Enabled: true, Runs: runs, Audit: au, GitHub: gh,
		Interval: time.Minute, Window: time.Hour})
	if tk == nil || reason != "" || tk.Forge == nil || tk.interval() != time.Minute || tk.window() != time.Hour {
		t.Fatalf("enabled: ticker = %+v reason = %q, want a wired ticker", tk, reason)
	}
}

func TestCITicker_Defaults(t *testing.T) {
	tk := &CITicker{}
	if tk.interval() != DefaultCIInterval || tk.window() != DefaultCIWindow || tk.maxPerTick() != DefaultCIMaxPerTick ||
		tk.terminalAfter() != DefaultCITerminalAfter || tk.logger() == nil || tk.now().IsZero() {
		t.Fatal("defaults not applied")
	}
}

func TestCITicker_Run(t *testing.T) {
	if err := (&CITicker{}).Run(context.Background()); err == nil {
		t.Fatal("Run without dependencies: err = nil")
	}
	f := newTickFixture(t)
	id := f.addMergedRun(42, tickNow.Add(-7*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	f.ticker.Interval = time.Millisecond
	go func() { done <- f.ticker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.ciRows(id)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || len(f.ciRows(id)) != 1 {
		t.Fatalf("Run: err = %v rows = %d, want nil and one row", err, len(f.ciRows(id)))
	}
}

// Operator condition 6: ONE pg-backed Tick through a REAL githubclient
// against an httptest mux, asserting the COMMITTED run_merge_ci_observed row.
func TestTick_PG_RealClient_RecordsCommittedRow(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	inst := int64(9)
	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "acme/widgets", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
		InstallationID:         &inst,
		RequiredChecksSnapshot: snap("CI Pass", auditcheckpublisher.CheckName),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.SetRunPullRequestURL(ctx, r.ID, prURLFor(42)); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	merged := now.Add(-7 * time.Hour)
	kind := audit.ActorSystem
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{RunID: r.ID, Timestamp: merged, Category: "pr_merged",
		ActorKind: &kind, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("seed pr_merged: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/42", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"node_id":"PR_42","state":"closed","merged":true,"merge_commit_sha":%q,"merged_at":%q}`,
			shaFor(42), merged.Format(time.RFC3339))
	})
	mux.HandleFunc("/repos/acme/widgets/commits/"+shaFor(42)+"/check-runs", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("filter") != "all" {
			http.Error(w, "want filter=all", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[
			{"id":1,"name":"CI Pass","status":"completed","conclusion":"failure","started_at":%q,"completed_at":%q,"check_suite":{"id":5}},
			{"id":2,"name":"CI Pass","status":"completed","conclusion":"success","started_at":%q,"completed_at":%q,"check_suite":{"id":5}}]}`,
			merged.Format(time.RFC3339), merged.Add(20*time.Minute).Format(time.RFC3339),
			merged.Add(time.Hour).Format(time.RFC3339), merged.Add(90*time.Minute).Format(time.RFC3339))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := githubclient.New(staticTokens{})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	tk, reason := NewCITicker(CITickerConfig{Enabled: true, Runs: runRepo, Audit: auditRepo, GitHub: c})
	if tk == nil {
		t.Fatalf("NewCITicker: %s", reason)
	}
	tk.Now = func() time.Time { return now }
	tk.RetryBackoff = time.Millisecond
	if sum := tk.Tick(ctx); sum.Recorded != 1 {
		t.Fatalf("tick = %+v, want one row recorded", sum)
	}
	if sum := tk.Tick(ctx); sum.Candidates != 0 {
		t.Fatalf("second tick = %+v, want the observed run out of the scan", sum)
	}
	rows, err := auditRepo.ListForRunByCategory(ctx, r.ID, CategoryRunMergeCIObserved)
	if err != nil || len(rows) != 1 {
		t.Fatalf("committed rows = %d err = %v, want 1", len(rows), err)
	}
	var p map[string]any
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p["conclusion"] != "green" || p["merge_commit_sha"] != shaFor(42) || fmt.Sprint(p["excluded_checks"]) != "[fishhawk_audit_complete]" {
		t.Fatalf("committed payload = %v", p)
	}
}
