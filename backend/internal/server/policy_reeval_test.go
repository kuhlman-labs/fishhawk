package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// makeCheckRunPayloadWithRepo extends makeCheckRunPayload (which is
// scoped to ingestCheckRun's reads) with the top-level repository
// field reevaluateCIPolicy needs to construct the PR URL.
func makeCheckRunPayloadWithRepo(repoFullName, action, checkName, headSHA string, conclusion *string, prNumbers []int) []byte {
	body := map[string]any{
		"action": action,
		"check_run": map[string]any{
			"id":           int64(999),
			"name":         checkName,
			"head_sha":     headSHA,
			"status":       "completed",
			"conclusion":   conclusion,
			"completed_at": "2026-05-16T12:00:00Z",
			"pull_requests": func() []map[string]any {
				out := make([]map[string]any, 0, len(prNumbers))
				for _, n := range prNumbers {
					out = append(out, map[string]any{"number": n})
				}
				return out
			}(),
		},
		"repository": map[string]any{
			"full_name": repoFullName,
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// reevalRunRepo is a focused run.Repository fake for the policy
// reeval tests. Only the three methods the path calls are
// implemented; the rest fail with "not used" so a future change
// that broadens the surface fails loudly.
type reevalRunRepo struct {
	runs          []*run.Run
	stagesByRunID map[uuid.UUID][]*run.Stage
	listErr       error
}

func newReevalRunRepo() *reevalRunRepo {
	return &reevalRunRepo{stagesByRunID: map[uuid.UUID][]*run.Stage{}}
}

func (r *reevalRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := []*run.Run{}
	for _, rn := range r.runs {
		if f.PullRequestURL != nil {
			if rn.PullRequestURL == nil || *rn.PullRequestURL != *f.PullRequestURL {
				continue
			}
		}
		out = append(out, rn)
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (r *reevalRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	return r.stagesByRunID[runID], nil
}

func (r *reevalRunRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *reevalRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *reevalRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *reevalRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *reevalRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *reevalRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *reevalRunRepo) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *reevalRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *reevalRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (r *reevalRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (r *reevalRunRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *reevalRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// reevalAuditRepo records AppendChained calls and lets tests seed
// prior policy_evaluated rows.
type reevalAuditRepo struct {
	audit.Repository
	mu       sync.Mutex
	preSeeds []*audit.Entry
	appended []audit.ChainAppendParams
}

func newReevalAuditRepo() *reevalAuditRepo { return &reevalAuditRepo{} }

func (r *reevalAuditRepo) seedPolicyEvaluated(runID, stageID uuid.UUID, payload policy.EvaluationPayload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, _ := json.Marshal(payload)
	rID := runID
	sID := stageID
	r.preSeeds = append(r.preSeeds, &audit.Entry{
		ID:       uuid.New(),
		RunID:    &rID,
		StageID:  &sID,
		Category: policy.CategoryPolicyEvaluated,
		Payload:  body,
	})
}

func (r *reevalAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range r.preSeeds {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	for _, p := range r.appended {
		if p.RunID != runID || p.Category != category {
			continue
		}
		stageID := p.StageID
		out = append(out, &audit.Entry{
			ID:       uuid.New(),
			RunID:    &p.RunID,
			StageID:  stageID,
			Category: p.Category,
			Payload:  p.Payload,
		})
	}
	return out, nil
}

func (r *reevalAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appended = append(r.appended, p)
	return &audit.Entry{ID: uuid.New(), RunID: &p.RunID}, nil
}

// reevalFixture wires the three repos + a *Server with sensible
// defaults. Each test mutates the seeds it needs.
type reevalFixture struct {
	srv     *Server
	runs    *reevalRunRepo
	audit   *reevalAuditRepo
	checks  *stageCheckRepoFake
	runID   uuid.UUID
	stageID uuid.UUID
	prURL   string
}

func newReevalFixture(t *testing.T, requiredContexts []string) *reevalFixture {
	t.Helper()
	runID := uuid.New()
	stageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"

	runs := newReevalRunRepo()
	pr := prURL
	runs.runs = []*run.Run{{
		ID:             runID,
		Repo:           "x/y",
		PullRequestURL: &pr,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{
			Contexts: requiredContexts,
			Sources:  []string{"branch_protection"},
		},
	}}
	runs.stagesByRunID[runID] = []*run.Stage{
		{ID: stageID, RunID: runID, Type: run.StageTypeImplement, State: run.StageStateRunning},
	}

	aud := newReevalAuditRepo()
	// Seed a prior policy_evaluated row with no CIGreen (the
	// trace-upload-time row #297 produces).
	aud.seedPolicyEvaluated(runID, stageID, policy.EvaluationPayload{
		StageType: "implement",
		Diff: []policy.DiffEntry{
			{Path: "x.go", Status: policy.Status("modified")},
		},
		Applied: policy.Constraints{
			RequiredOutcomes: []string{"ci_green"},
			// CIGreen nil — pre-#300 deferred posture.
		},
		Passed:           true,
		DeferredOutcomes: []string{"ci_green"},
	})

	checks := newStageCheckRepoFake()

	srv := New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        runs,
		AuditRepo:      aud,
		StageCheckRepo: checks,
	})

	return &reevalFixture{
		srv:     srv,
		runs:    runs,
		audit:   aud,
		checks:  checks,
		runID:   runID,
		stageID: stageID,
		prURL:   prURL,
	}
}

// latestPolicyEvaluatedAppend returns the most-recent appended
// policy_evaluated payload from the audit fake, decoded. Returns
// nil when nothing has been written.
func (f *reevalFixture) latestPolicyEvaluatedAppend(t *testing.T) *policy.EvaluationPayload {
	t.Helper()
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	for i := len(f.audit.appended) - 1; i >= 0; i-- {
		p := f.audit.appended[i]
		if p.Category != policy.CategoryPolicyEvaluated {
			continue
		}
		var pl policy.EvaluationPayload
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			t.Fatalf("decode appended policy_evaluated: %v", err)
		}
		return &pl
	}
	return nil
}

// seedCheck adds a stage_checks row for the implement stage.
func (f *reevalFixture) seedCheck(name, status string, conclusion *string) {
	f.checks.seed(f.stageID, &stagecheck.Check{
		StageID:    f.stageID,
		Name:       name,
		Status:     status,
		Conclusion: conclusion,
		Timestamp:  time.Now().UTC(),
	})
}

func TestReevaluateCIPolicy_SingleRequired_FlipsToTrue(t *testing.T) {
	fx := newReevalFixture(t, []string{"ci_pass"})
	fx.seedCheck("ci_pass", "completed", ptrStr("success"))

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	got := fx.latestPolicyEvaluatedAppend(t)
	if got == nil {
		t.Fatal("expected a new policy_evaluated audit row; got none")
	}
	if got.Applied.CIGreen == nil || !*got.Applied.CIGreen {
		t.Errorf("Applied.CIGreen = %v, want &true", got.Applied.CIGreen)
	}
	if len(got.DeferredOutcomes) != 0 {
		t.Errorf("DeferredOutcomes = %v, want empty (ci_green has a signal now)", got.DeferredOutcomes)
	}
	if len(got.Violations) != 0 {
		t.Errorf("Violations = %v, want empty (ci_green passed)", got.Violations)
	}
}

func TestReevaluateCIPolicy_SingleRequired_TerminatesFailure_FlipsToFalse(t *testing.T) {
	fx := newReevalFixture(t, []string{"ci_pass"})
	fx.seedCheck("ci_pass", "completed", ptrStr("failure"))

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("failure"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	got := fx.latestPolicyEvaluatedAppend(t)
	if got == nil {
		t.Fatal("expected a new policy_evaluated audit row; got none")
	}
	if got.Applied.CIGreen == nil || *got.Applied.CIGreen {
		t.Errorf("Applied.CIGreen = %v, want &false", got.Applied.CIGreen)
	}
	if len(got.Violations) == 0 {
		t.Errorf("expected ci_green violation; got none")
	}
	if got.Passed {
		t.Errorf("Passed = true, want false (ci_green failed)")
	}
}

func TestReevaluateCIPolicy_MultiRequired_FirstGreen_DedupSkip(t *testing.T) {
	// Two required checks; only one has reported terminally.
	// Aggregate is still nil (some pending). The prior row's
	// CIGreen is also nil. Dedup: skip.
	fx := newReevalFixture(t, []string{"ci_pass", "lint"})
	fx.seedCheck("ci_pass", "completed", ptrStr("success"))

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	if got := fx.latestPolicyEvaluatedAppend(t); got != nil {
		t.Errorf("expected no new audit row (dedup); got %+v", got)
	}
}

func TestReevaluateCIPolicy_MultiRequired_SecondGreen_FlipsToTrue(t *testing.T) {
	// Both required checks now in pass bucket → aggregate is true → new row.
	fx := newReevalFixture(t, []string{"ci_pass", "lint"})
	fx.seedCheck("ci_pass", "completed", ptrStr("success"))
	fx.seedCheck("lint", "completed", ptrStr("success"))

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "lint", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	got := fx.latestPolicyEvaluatedAppend(t)
	if got == nil {
		t.Fatal("expected new audit row when quorum lands")
	}
	if got.Applied.CIGreen == nil || !*got.Applied.CIGreen {
		t.Errorf("Applied.CIGreen = %v, want &true", got.Applied.CIGreen)
	}
}

func TestReevaluateCIPolicy_MultiRequired_OneFails_FlipsImmediately(t *testing.T) {
	// One required check fails while another is still pending.
	// Failure is decisive — we don't wait for the sibling.
	fx := newReevalFixture(t, []string{"ci_pass", "lint"})
	fx.seedCheck("lint", "completed", ptrStr("failure"))

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "lint", "deadbeef", ptrStr("failure"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	got := fx.latestPolicyEvaluatedAppend(t)
	if got == nil {
		t.Fatal("expected new audit row on first failure")
	}
	if got.Applied.CIGreen == nil || *got.Applied.CIGreen {
		t.Errorf("Applied.CIGreen = %v, want &false", got.Applied.CIGreen)
	}
}

func TestReevaluateCIPolicy_Dedup_RedeliveryOfSameEvent_NoNewRow(t *testing.T) {
	// First call lands the green-flip row; second call sees the
	// same aggregate and skips.
	fx := newReevalFixture(t, []string{"ci_pass"})
	fx.seedCheck("ci_pass", "completed", ptrStr("success"))

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	rows := 0
	for _, p := range fx.audit.appended {
		if p.Category == policy.CategoryPolicyEvaluated {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("expected exactly 1 new policy_evaluated row; got %d", rows)
	}
}

func TestReevaluateCIPolicy_NotFishhawkManaged_Skips(t *testing.T) {
	// PR URL has no Fishhawk run pointing at it.
	fx := newReevalFixture(t, []string{"ci_pass"})
	fx.runs.runs = nil // wipe — no matching run

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	if got := fx.latestPolicyEvaluatedAppend(t); got != nil {
		t.Errorf("unmanaged PR should not produce a row; got %+v", got)
	}
}

func TestReevaluateCIPolicy_CheckNotRequired_Skips(t *testing.T) {
	// Required snapshot lists ci_pass; the event is for "smoke",
	// which isn't required. Skip without reading further state.
	fx := newReevalFixture(t, []string{"ci_pass"})

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "smoke", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	if got := fx.latestPolicyEvaluatedAppend(t); got != nil {
		t.Errorf("non-required check should not produce a row; got %+v", got)
	}
}

func TestReevaluateCIPolicy_PriorIsSkip_NoNewRow(t *testing.T) {
	// Prior evaluation was skipped (e.g. no diff). The CI signal
	// can't unblock a missing spec / diff — leave the chain alone.
	fx := newReevalFixture(t, []string{"ci_pass"})
	// Override the seeded prior row with a skip variant.
	fx.audit.preSeeds = nil
	fx.audit.seedPolicyEvaluated(fx.runID, fx.stageID, policy.EvaluationPayload{
		StageType:  "implement",
		SkipReason: policy.SkipNoDiffInBundle,
		Passed:     true,
	})
	fx.seedCheck("ci_pass", "completed", ptrStr("success"))

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	if got := fx.latestPolicyEvaluatedAppend(t); got != nil {
		t.Errorf("skip-state row should stay sticky; got %+v", got)
	}
}

func TestReevaluateCIPolicy_AuditCompleteCheck_ExcludedFromAggregate(t *testing.T) {
	// `fishhawk_audit_complete` is in the required-checks snapshot
	// (customers configure it per #231), but it's our own derived
	// check (#229). Excluded from ci_green aggregation. Without
	// exclusion the aggregate would be stuck at "pending" forever
	// because audit-complete depends on the policy eval that we're
	// about to write — a circular dep.
	fx := newReevalFixture(t, []string{"ci_pass", "fishhawk_audit_complete"})
	fx.seedCheck("ci_pass", "completed", ptrStr("success"))
	// Audit-complete is intentionally NOT seeded — would normally
	// be pending while it derives.

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	got := fx.latestPolicyEvaluatedAppend(t)
	if got == nil {
		t.Fatal("expected new audit row; audit-complete should not block aggregation")
	}
	if got.Applied.CIGreen == nil || !*got.Applied.CIGreen {
		t.Errorf("Applied.CIGreen = %v, want &true (audit-complete excluded)", got.Applied.CIGreen)
	}
}

func TestReevaluateCIPolicy_NoImplementStage_Skips(t *testing.T) {
	fx := newReevalFixture(t, []string{"ci_pass"})
	// Replace stages with a plan-only set — no implement.
	fx.runs.stagesByRunID[fx.runID] = []*run.Stage{
		{ID: uuid.New(), RunID: fx.runID, Type: run.StageTypePlan, State: run.StageStateSucceeded},
	}

	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	if got := fx.latestPolicyEvaluatedAppend(t); got != nil {
		t.Errorf("run without implement stage should be skipped; got %+v", got)
	}
}

func TestReevaluateCIPolicy_NonTerminalAction_Skips(t *testing.T) {
	fx := newReevalFixture(t, []string{"ci_pass"})
	// action=created — non-terminal.
	body := makeCheckRunPayloadWithRepo("x/y", "created", "ci_pass", "deadbeef", nil, []int{42})
	fx.srv.reevaluateCIPolicy(context.Background(), body)

	if got := fx.latestPolicyEvaluatedAppend(t); got != nil {
		t.Errorf("non-terminal action should not produce a row; got %+v", got)
	}
}

func TestReevaluateCIPolicy_NoRepos_NoOp(t *testing.T) {
	// Defensive: the helper bails when any of the three required
	// repos isn't wired. Tests the empty-config posture (legacy
	// deployments) without needing repos.
	s := New(Config{Addr: "127.0.0.1:0"})
	body := makeCheckRunPayloadWithRepo("x/y", "completed", "ci_pass", "deadbeef", ptrStr("success"), []int{42})
	s.reevaluateCIPolicy(context.Background(), body) // must not panic
}
