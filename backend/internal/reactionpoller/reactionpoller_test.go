package reactionpoller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// -----------------------------------------------------------------
// Fakes
// -----------------------------------------------------------------

type fakeRunRepo struct {
	mu      sync.Mutex
	stages  []*run.Stage
	runs    map[uuid.UUID]*run.Run
	listErr error
}

func newFakeRunRepo() *fakeRunRepo {
	return &fakeRunRepo{runs: map[uuid.UUID]*run.Run{}}
}

func (f *fakeRunRepo) ListStagesAwaitingChildren(_ context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (f *fakeRunRepo) ListStagesAwaitingApproval(_ context.Context) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.stages, nil
}

func (f *fakeRunRepo) ListReviewStagesAwaitingApproval(_ context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (f *fakeRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, run.ErrNotFound
}

func (f *fakeRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (f *fakeRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (f *fakeRunRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

type fakeAudit struct {
	audit.Repository
	mu        sync.Mutex
	entries   []*audit.Entry
	appended  []audit.ChainAppendParams
	listErr   error
	appendErr error
}

func (a *fakeAudit) seed(e *audit.Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *fakeAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	out := []*audit.Entry{}
	for _, e := range a.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	// Also serve already-appended rows.
	for _, p := range a.appended {
		if p.RunID != runID || p.Category != category {
			continue
		}
		stageID := p.StageID
		out = append(out, &audit.Entry{
			ID: uuid.New(), RunID: &p.RunID, StageID: stageID,
			Category: p.Category, Payload: p.Payload, Timestamp: p.Timestamp,
		})
	}
	return out, nil
}

func (a *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.appended = append(a.appended, p)
	return &audit.Entry{ID: uuid.New(), RunID: &p.RunID}, nil
}

type fakeReactions struct {
	mu        sync.Mutex
	calls     int
	byComment map[int64][]githubclient.IssueCommentReaction
	listErr   error
}

func (f *fakeReactions) ListIssueCommentReactionsScoped(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, commentID int64) ([]githubclient.IssueCommentReaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byComment[commentID], nil
}

type fakeApprovals struct {
	mu    sync.Mutex
	calls []webhook.ApprovalCommandParams
	err   error
}

func (f *fakeApprovals) HandleApprovalCommand(_ context.Context, p webhook.ApprovalCommandParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	return f.err
}

// -----------------------------------------------------------------
// Fixture
// -----------------------------------------------------------------

type fixture struct {
	t         *testing.T
	runs      *fakeRunRepo
	audit     *fakeAudit
	reactions *fakeReactions
	approvals *fakeApprovals
	ticker    *Ticker
	now       time.Time
	commentID int64
	commentAt time.Time
	stageID   uuid.UUID
	runID     uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	commentAt := now.Add(-5 * time.Minute) // fast tier by default
	stageID := uuid.New()
	runID := uuid.New()
	commentID := int64(4242)

	runs := newFakeRunRepo()
	triggerRef := "issue:42"
	installID := int64(99)
	runs.runs[runID] = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installID,
	}
	runs.stages = []*run.Stage{
		{ID: stageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval},
	}

	aud := &fakeAudit{}
	seedAnchorAndPlan(aud, runID, stageID, commentID, commentAt)

	reactions := &fakeReactions{byComment: map[int64][]githubclient.IssueCommentReaction{}}
	approvals := &fakeApprovals{}

	ticker := &Ticker{
		Runs:         runs,
		Audit:        aud,
		Reactions:    reactions,
		Approvals:    approvals,
		FastInterval: 30 * time.Second,
		SlowInterval: 5 * time.Minute,
		AgeThreshold: 10 * time.Minute,
		Now:          func() time.Time { return now },
	}

	return &fixture{
		t:         t,
		runs:      runs,
		audit:     aud,
		reactions: reactions,
		approvals: approvals,
		ticker:    ticker,
		now:       now,
		commentID: commentID,
		commentAt: commentAt,
		stageID:   stageID,
		runID:     runID,
	}
}

// seedAnchorAndPlan seeds the two audit facts the poller now reads
// (#1054): a `status_comment_posted` row carrying the anchor comment
// id, and a `plan_generated` row marking plan existence (also the
// cadence age origin + approval cutoff). planExistsAt is used for both
// the anchor posted-at and the plan-existence instant in the default
// fixture; the binding-condition-2 tests seed a distinct plan-existence
// time directly.
func seedAnchorAndPlan(aud *fakeAudit, runID, stageID uuid.UUID, commentID int64, planExistsAt time.Time) {
	seedAnchor(aud, runID, stageID, commentID, planExistsAt.Add(-time.Minute))
	seedPlanGenerated(aud, runID, stageID, planExistsAt)
}

func seedAnchor(aud *fakeAudit, runID, stageID uuid.UUID, commentID int64, postedAt time.Time) {
	r := runID
	s := stageID
	payload, _ := json.Marshal(map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": commentID,
	})
	aud.seed(&audit.Entry{
		ID: uuid.New(), RunID: &r, StageID: &s,
		Category:  issuecomment.CategoryStatusCommentPosted,
		Payload:   payload,
		Timestamp: postedAt,
	})
}

func seedPlanGenerated(aud *fakeAudit, runID, stageID uuid.UUID, at time.Time) {
	r := runID
	s := stageID
	payload, _ := json.Marshal(map[string]any{
		"run_id":         runID.String(),
		"stage_id":       stageID.String(),
		"schema_version": "standard_v1",
	})
	aud.seed(&audit.Entry{
		ID: uuid.New(), RunID: &r, StageID: &s,
		Category:  planGeneratedCategory,
		Payload:   payload,
		Timestamp: at,
	})
}

func reaction(id int64, content githubclient.IssueCommentReactKind, login string) githubclient.IssueCommentReaction {
	r := githubclient.IssueCommentReaction{ID: id, Content: content}
	r.User.Login = login
	return r
}

// reactionAt is reaction with an explicit placement time, used by the
// plan-existence cutoff tests (#1054, binding condition 2).
func reactionAt(id int64, content githubclient.IssueCommentReactKind, login string, createdAt time.Time) githubclient.IssueCommentReaction {
	r := reaction(id, content, login)
	r.CreatedAt = createdAt
	return r
}

func (fx *fixture) seedReactions(reactions ...githubclient.IssueCommentReaction) {
	fx.reactions.byComment[fx.commentID] = reactions
}

func (fx *fixture) advance(d time.Duration) {
	fx.now = fx.now.Add(d)
	fx.ticker.Now = func() time.Time { return fx.now }
}

func (fx *fixture) reactionObservedCount() int {
	fx.audit.mu.Lock()
	defer fx.audit.mu.Unlock()
	n := 0
	for _, p := range fx.audit.appended {
		if p.Category == CategoryPlanReactionObserved {
			n++
		}
	}
	return n
}

// -----------------------------------------------------------------
// Tests
// -----------------------------------------------------------------

func TestTick_HappyPath_ApprovalReactionForwarded(t *testing.T) {
	fx := newFixture(t)
	fx.seedReactions(reaction(1, githubclient.ReactPlusOne, "alice"))

	fx.ticker.Tick(context.Background())

	if got := fx.reactions.calls; got != 1 {
		t.Errorf("expected 1 GitHub reactions call; got %d", got)
	}
	if got := fx.reactionObservedCount(); got != 1 {
		t.Errorf("expected 1 audit row; got %d", got)
	}
	if got := len(fx.approvals.calls); got != 1 {
		t.Fatalf("expected 1 approval forward; got %d", got)
	}
	call := fx.approvals.calls[0]
	if call.Source != webhook.ApprovalSourceReactionEmoji {
		t.Errorf("Source = %q, want %q", call.Source, webhook.ApprovalSourceReactionEmoji)
	}
	if call.Decision != webhook.MatchActionApprove {
		t.Errorf("Decision = %q, want approve", call.Decision)
	}
	if call.SenderLogin != "alice" {
		t.Errorf("SenderLogin = %q, want alice", call.SenderLogin)
	}
	if call.IssueNumber != 42 {
		t.Errorf("IssueNumber = %d, want 42", call.IssueNumber)
	}
}

func TestTick_NonApprovalReaction_AuditOnlyNoForward(t *testing.T) {
	// 👎 / -1 is recorded in the audit chain (visible to operators)
	// but does NOT forward to approval — v0 has no reject-by-emoji
	// surface (the rationale-bearing slash command owns reject).
	fx := newFixture(t)
	fx.seedReactions(reaction(2, githubclient.ReactMinusOne, "bob"))

	fx.ticker.Tick(context.Background())

	if got := fx.reactionObservedCount(); got != 1 {
		t.Errorf("expected 1 audit row; got %d", got)
	}
	if got := len(fx.approvals.calls); got != 0 {
		t.Errorf("expected 0 approval forwards; got %d", got)
	}
}

func TestTick_Dedup_RepeatedTicksDoNotRefireApprovals(t *testing.T) {
	fx := newFixture(t)
	fx.seedReactions(reaction(7, githubclient.ReactPlusOne, "alice"))

	fx.ticker.Tick(context.Background())
	fx.advance(31 * time.Second) // past the fast cadence
	fx.ticker.Tick(context.Background())
	fx.advance(31 * time.Second)
	fx.ticker.Tick(context.Background())

	if got := fx.reactions.calls; got != 3 {
		t.Errorf("expected 3 GitHub calls (one per tick); got %d", got)
	}
	if got := fx.reactionObservedCount(); got != 1 {
		t.Errorf("expected 1 audit row after dedup; got %d", got)
	}
	if got := len(fx.approvals.calls); got != 1 {
		t.Errorf("expected 1 approval forward after dedup; got %d", got)
	}
}

func TestTick_CadenceSwitch_FastThenSlow(t *testing.T) {
	fx := newFixture(t)

	// T=0: comment is 5min old (fast tier). First tick polls.
	fx.ticker.Tick(context.Background())
	if fx.reactions.calls != 1 {
		t.Fatalf("expected initial poll; got %d", fx.reactions.calls)
	}

	// T=31s: comment is 5min31s old (still fast tier).
	// 31s since last poll ≥ 30s fast cadence → polls.
	fx.advance(31 * time.Second)
	fx.ticker.Tick(context.Background())
	if fx.reactions.calls != 2 {
		t.Fatalf("expected second fast-tier poll; got %d", fx.reactions.calls)
	}

	// T=5min32s: comment is 10min32s old (slow tier crossed).
	// 5min1s since last poll ≥ 5min slow cadence → polls once more.
	fx.advance(5*time.Minute + time.Second)
	fx.ticker.Tick(context.Background())
	if fx.reactions.calls != 3 {
		t.Fatalf("expected slow-tier poll once 5min cadence elapsed; got %d", fx.reactions.calls)
	}

	// T=5min33s: only 1s since last poll. Slow cadence is 5min.
	// 1s < 5min → SKIP. This is the gate the slow tier provides.
	fx.advance(time.Second)
	fx.ticker.Tick(context.Background())
	if fx.reactions.calls != 3 {
		t.Errorf("slow tier should skip below cadence; got %d", fx.reactions.calls)
	}
}

func TestTick_NonPlanStage_Ignored(t *testing.T) {
	// An awaiting-approval stage that isn't a plan stage (e.g.
	// a review stage) is out of scope — review-stage approvals
	// don't have plan comments to poll reactions on.
	fx := newFixture(t)
	fx.runs.stages[0].Type = run.StageTypeReview

	fx.ticker.Tick(context.Background())

	if fx.reactions.calls != 0 {
		t.Errorf("non-plan stage should not produce a GitHub call; got %d", fx.reactions.calls)
	}
}

// TestTick_DeployStageAwaitingDeployApproval_Ignored pins #1390 binding
// condition 1: ListStagesAwaitingApproval was broadened to also return deploy
// stages parked at awaiting_deploy_approval (the SLA ticker's second consumer
// is THIS reaction poller). The broadening is safe ONLY because the poller
// skips any stage whose Type != plan. This asserts the deploy gate row the
// broadening newly surfaces is filtered out before any GitHub call — the
// poller must never treat a deploy approval as a plan-comment reaction target.
func TestTick_DeployStageAwaitingDeployApproval_Ignored(t *testing.T) {
	fx := newFixture(t)
	fx.runs.stages[0].Type = run.StageTypeDeploy
	fx.runs.stages[0].State = run.StageStateAwaitingDeployApproval

	fx.ticker.Tick(context.Background())

	if fx.reactions.calls != 0 {
		t.Errorf("deploy stage at awaiting_deploy_approval must not produce a GitHub call; got %d", fx.reactions.calls)
	}
}

func TestTick_NoPlanCommentYet_Skips(t *testing.T) {
	fx := newFixture(t)
	// Wipe the seeded anchor + plan_generated audit rows.
	fx.audit.entries = nil

	fx.ticker.Tick(context.Background())

	if fx.reactions.calls != 0 {
		t.Errorf("missing anchor/plan should skip the poll; got %d calls", fx.reactions.calls)
	}
}

func TestTick_NoPlanGeneratedYet_Skips(t *testing.T) {
	// The anchor exists from run creation (#1054), but no plan has been
	// generated. There is no approvable plan, so the poller must skip —
	// and binding condition 2 means a reaction now would predate any
	// future plan anyway.
	fx := newFixture(t)
	fx.audit.entries = nil
	seedAnchor(fx.audit, fx.runID, fx.stageID, fx.commentID, fx.commentAt)

	fx.ticker.Tick(context.Background())

	if fx.reactions.calls != 0 {
		t.Errorf("no plan_generated should skip the poll; got %d calls", fx.reactions.calls)
	}
}

func TestTick_ReactionBeforePlanExistence_NotAdmitted(t *testing.T) {
	// Binding condition 2 (#1054): the anchor comment exists from run
	// creation, so a 👍 placed BEFORE the plan was generated is not a
	// plan approval. The dedup audit row is still written (so it isn't
	// re-evaluated forever) but it must NOT forward to the approval
	// handler.
	fx := newFixture(t)
	planExistsAt := fx.commentAt // the fixture's plan_generated timestamp
	fx.seedReactions(reactionAt(11, githubclient.ReactPlusOne, "alice", planExistsAt.Add(-time.Minute)))

	fx.ticker.Tick(context.Background())

	if got := fx.reactionObservedCount(); got != 1 {
		t.Errorf("pre-plan reaction should still be recorded for dedup; got %d audit rows", got)
	}
	if got := len(fx.approvals.calls); got != 0 {
		t.Fatalf("pre-plan reaction must NOT forward as a plan approval; got %d forwards", got)
	}
}

func TestTick_ReactionAfterPlanExistence_Admitted(t *testing.T) {
	// The mirror of the above: a 👍 placed AFTER the plan exists IS a
	// plan approval and forwards.
	fx := newFixture(t)
	planExistsAt := fx.commentAt
	fx.seedReactions(reactionAt(12, githubclient.ReactPlusOne, "alice", planExistsAt.Add(time.Minute)))

	fx.ticker.Tick(context.Background())

	if got := len(fx.approvals.calls); got != 1 {
		t.Fatalf("post-plan reaction should forward as a plan approval; got %d forwards", got)
	}
}

func TestTick_AnchorCommentIdRepoint(t *testing.T) {
	// The poller resolves the comment to poll from the anchor's
	// status_comment_posted id (#1054), not the deleted plan_full rows.
	// Reactions are keyed by that comment id in the fake.
	fx := newFixture(t)
	fx.seedReactions(reaction(21, githubclient.ReactRocket, "carol"))

	fx.ticker.Tick(context.Background())

	if fx.reactions.calls != 1 {
		t.Fatalf("expected the poller to poll the anchor comment id; got %d calls", fx.reactions.calls)
	}
	if got := len(fx.approvals.calls); got != 1 {
		t.Errorf("expected the anchor-id-keyed reaction to forward; got %d", got)
	}
}

func TestTick_NonIssueTrigger_Skips(t *testing.T) {
	fx := newFixture(t)
	r := fx.runs.runs[fx.runID]
	cli := "cli:operator"
	r.TriggerRef = &cli // not "issue:<n>"

	fx.ticker.Tick(context.Background())

	if got := len(fx.approvals.calls); got != 0 {
		t.Errorf("non-issue trigger should not forward to approval; got %d", got)
	}
}

func TestTick_MissingInstallationID_Skips(t *testing.T) {
	fx := newFixture(t)
	fx.runs.runs[fx.runID].InstallationID = nil

	fx.ticker.Tick(context.Background())

	if fx.reactions.calls != 0 {
		t.Errorf("missing installation_id should skip the poll; got %d", fx.reactions.calls)
	}
}

func TestTick_ListReactionsError_LogsAndContinues(t *testing.T) {
	fx := newFixture(t)
	fx.reactions.listErr = errors.New("rate limited")

	// Must not panic; per-stage failure is per the worker doc.
	fx.ticker.Tick(context.Background())

	if got := len(fx.approvals.calls); got != 0 {
		t.Errorf("approval should not fire on GitHub error; got %d", got)
	}
}

func TestTick_MultipleApprovalShapedReactions_AllForwarded(t *testing.T) {
	fx := newFixture(t)
	fx.seedReactions(
		reaction(1, githubclient.ReactPlusOne, "alice"),
		reaction(2, githubclient.ReactRocket, "bob"),
		reaction(3, githubclient.ReactHeart, "carol"),
		reaction(4, githubclient.ReactEyes, "dave"), // not approval
	)

	fx.ticker.Tick(context.Background())

	if got := fx.reactionObservedCount(); got != 4 {
		t.Errorf("expected 4 audit rows (every reaction recorded); got %d", got)
	}
	if got := len(fx.approvals.calls); got != 3 {
		t.Errorf("expected 3 approval forwards (the four reactions less eyes); got %d", got)
	}
}

func TestTick_LegacyPlanUpdatedRow_IgnoredAnchorIdWins(t *testing.T) {
	// Legacy runs may still carry KindPlanUpdated rows on the
	// issue_commented category (#377), but the poller now resolves the
	// comment to poll from the anchor's status_comment_posted id
	// (#1054), NOT those rows. A stale legacy row carrying a DIFFERENT
	// comment id must be ignored — the anchor id is canonical.
	fx := newFixture(t)
	r := fx.runID
	s := fx.stageID
	payload, _ := json.Marshal(map[string]any{
		"kind":              string(issuecomment.KindPlanUpdated),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": int64(9999), // a different, stale id
	})
	fx.audit.seed(&audit.Entry{
		ID: uuid.New(), RunID: &r, StageID: &s,
		Category: issuecomment.CategoryIssueCommented, Payload: payload,
		Timestamp: fx.commentAt.Add(time.Minute),
	})
	// Reactions live only under the anchor comment id, not the stale id.
	fx.seedReactions(reaction(11, githubclient.ReactHooray, "eve"))

	fx.ticker.Tick(context.Background())

	if got := len(fx.approvals.calls); got != 1 {
		t.Errorf("expected the anchor-id reaction forwarded (legacy row ignored); got %d", got)
	}
}

func TestRun_RejectsMissingDeps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Ticker)
		want   string
	}{
		{"missing runs", func(t *Ticker) { t.Runs = nil }, "Runs"},
		{"missing audit", func(t *Ticker) { t.Audit = nil }, "Audit"},
		{"missing reactions", func(t *Ticker) { t.Reactions = nil }, "Reactions"},
		{"missing approvals", func(t *Ticker) { t.Approvals = nil }, "Approvals"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			tc.mutate(fx.ticker)
			err := fx.ticker.Run(context.Background())
			if err == nil {
				t.Fatalf("Run with missing %s should error; got nil", tc.want)
			}
		})
	}
}
