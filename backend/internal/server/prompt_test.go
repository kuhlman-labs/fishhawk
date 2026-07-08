package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// promptRunRepo is a run.Repository fake that supports GetStage +
// GetRun. Other methods panic to make accidental calls loud.
type promptRunRepo struct {
	stage    *run.Stage
	stageErr error
	runRow   *run.Run
	runErr   error
	// listRunsErr, when set, makes ListRuns return an error. The
	// decomposition-aware lineage ledger (#1038) enumerates child runs
	// via ListRuns and must treat a lookup error as an incomplete
	// ledger; this lets a test exercise that path.
	listRunsErr          error
	getStages            map[uuid.UUID]*run.Stage
	getRuns              map[uuid.UUID]*run.Run
	setPRURLCalls        []promptSetPRURLCall
	transitionStageCalls []promptTransitionStageCall
	// stagesByRunID backs ListStagesForRun. When non-nil, the map is
	// consulted; when nil the method returns an error so accidental
	// calls in tests that don't seed it stay loud.
	stagesByRunID map[uuid.UUID][]*run.Stage
	// addRunCostDeltas records every AddRunCost delta so the plan-review
	// cost-rollup seam test (#681) can assert the rollup was actually
	// driven with a non-zero delta rather than silently skipped.
	addRunCostDeltas []float64
	// onTransitionStage, when set, is invoked at the start of every
	// TransitionStage call (before the state mutation is recorded). The #1351
	// audit-visible-before-dispatch ordering test uses it to snapshot whether
	// the approval_submitted add_scope_files audit is already durable at the
	// instant the plan stage flips to succeeded — the dispatch-gating
	// transition.
	onTransitionStage func(id uuid.UUID, to run.StageState)
}

type promptTransitionStageCall struct {
	StageID    uuid.UUID
	To         run.StageState
	Completion *run.StageCompletion
}

type promptSetPRURLCall struct {
	RunID uuid.UUID
	URL   string
}

func newPromptRunRepo() *promptRunRepo {
	return &promptRunRepo{
		getStages: map[uuid.UUID]*run.Stage{},
		getRuns:   map[uuid.UUID]*run.Run{},
	}
}

func (r *promptRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.stageErr != nil {
		return nil, r.stageErr
	}
	if s, ok := r.getStages[id]; ok {
		return s, nil
	}
	if r.stage != nil && r.stage.ID == id {
		return r.stage, nil
	}
	return nil, run.ErrNotFound
}

func (r *promptRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.runErr != nil {
		return nil, r.runErr
	}
	if rn, ok := r.getRuns[id]; ok {
		return rn, nil
	}
	if r.runRow != nil && r.runRow.ID == id {
		return r.runRow, nil
	}
	return nil, run.ErrNotFound
}

// AddRunCost satisfies the trace handler's runCostRecorder optional
// capability (#681) so the plan-review cost-rollup seam test can assert the
// per-run total accumulates via a real AddRunCost call (not a vacuous skip).
func (r *promptRunRepo) AddRunCost(_ context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*run.Run, error) {
	r.addRunCostDeltas = append(r.addRunCostDeltas, deltaUSD)
	rn, ok := r.getRuns[id]
	if !ok {
		if r.runRow != nil && r.runRow.ID == id {
			rn = r.runRow
		} else {
			return nil, run.ErrNotFound
		}
	}
	rn.CostUSDTotal += deltaUSD
	if resolvedModel != "" {
		rn.ResolvedModel = resolvedModel
	}
	return rn, nil
}

// CreateRun mints a run row into getRuns, threading the decomposition-linkage
// fields (DecomposedFrom / ParentRunID / SliceIndex) and IssueContext so the
// campaign-mint → prompt cross-boundary test (#1721) can drive
// StartRunForCampaignIssue against this same fake the prompt handler reads.
func (r *promptRunRepo) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	runnerKind := p.RunnerKind
	if runnerKind == "" {
		runnerKind = run.RunnerKindGitHubActions
	}
	rn := &run.Run{
		ID:             uuid.New(),
		Repo:           p.Repo,
		WorkflowID:     p.WorkflowID,
		WorkflowSHA:    p.WorkflowSHA,
		TriggerSource:  p.TriggerSource,
		TriggerRef:     p.TriggerRef,
		InstallationID: p.InstallationID,
		IssueContext:   p.IssueContext,
		DecomposedFrom: p.DecomposedFrom,
		ParentRunID:    p.ParentRunID,
		SliceIndex:     p.SliceIndex,
		WorkflowSpec:   p.WorkflowSpec,
		RunnerKind:     runnerKind,
		State:          run.StatePending,
	}
	r.getRuns[rn.ID] = rn
	return rn, nil
}

// CreateStage appends a stage row into stagesByRunID + getStages so a run
// minted through CreateRun is later resolvable by the prompt handler.
func (r *promptRunRepo) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	st := &run.Stage{
		ID:           uuid.New(),
		RunID:        p.RunID,
		Sequence:     p.Sequence,
		Type:         p.Type,
		ExecutorKind: p.ExecutorKind,
		ExecutorRef:  p.ExecutorRef,
		State:        run.StageStatePending,
	}
	if r.stagesByRunID == nil {
		r.stagesByRunID = map[uuid.UUID][]*run.Stage{}
	}
	r.stagesByRunID[p.RunID] = append(r.stagesByRunID[p.RunID], st)
	r.getStages[st.ID] = st
	return st, nil
}
func (r *promptRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}

// ListRuns honors the DecomposedFrom filter (scanning getRuns) so lineage
// tests can register decomposition children (#1038). It mirrors the repo
// contract that Limit must be > 0 by returning nothing on a zero limit.
// Filters the fake doesn't model return empty, preserving existing call
// sites.
func (r *promptRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	if r.listRunsErr != nil {
		return nil, r.listRunsErr
	}
	var out []*run.Run
	if f.DecomposedFrom != nil && f.Limit > 0 {
		for _, rn := range r.getRuns {
			if rn.DecomposedFrom != nil && *rn.DecomposedFrom == *f.DecomposedFrom {
				out = append(out, rn)
				if len(out) >= f.Limit {
					break
				}
			}
		}
	}
	return out, nil
}
func (r *promptRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) SetRunPullRequestURL(_ context.Context, id uuid.UUID, url string) (*run.Run, error) {
	r.setPRURLCalls = append(r.setPRURLCalls, promptSetPRURLCall{RunID: id, URL: url})
	if rn, ok := r.getRuns[id]; ok {
		u := url
		rn.PullRequestURL = &u
		return rn, nil
	}
	if r.runRow != nil && r.runRow.ID == id {
		u := url
		r.runRow.PullRequestURL = &u
		return r.runRow, nil
	}
	// Run not seeded — return a synthetic row so the handler's
	// best-effort log path still works.
	return &run.Run{ID: id}, nil
}
func (r *promptRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.stagesByRunID == nil {
		return nil, errors.New("not used")
	}
	return r.stagesByRunID[runID], nil
}
func (r *promptRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *promptRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *promptRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *promptRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *promptRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if r.onTransitionStage != nil {
		r.onTransitionStage(id, to)
	}
	r.transitionStageCalls = append(r.transitionStageCalls, promptTransitionStageCall{
		StageID:    id,
		To:         to,
		Completion: c,
	})
	if st, ok := r.getStages[id]; ok {
		st.State = to
		return st, nil
	}
	return &run.Stage{ID: id, State: to}, nil
}

// stubIssueGetter records calls and returns canned issues.
type stubIssueGetter struct {
	called  bool
	issue   *githubclient.Issue
	getErr  error
	gotInst int64
	gotRepo githubclient.RepoRef
	gotNum  int

	// Comment-fetch seam (#621): canned thread + optional error, plus
	// a recorded-call flag and args so branch-2 tests can assert the
	// fetch happened with the right installation/repo/number.
	comments        []githubclient.FetchedIssueComment
	commentsErr     error
	commentsCalled  bool
	commentsGotInst int64
	commentsGotRepo githubclient.RepoRef
	commentsGotNum  int
}

func (s *stubIssueGetter) GetIssue(_ context.Context, installationID int64, repo githubclient.RepoRef, number int) (*githubclient.Issue, error) {
	s.called = true
	s.gotInst = installationID
	s.gotRepo = repo
	s.gotNum = number
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.issue, nil
}

func (s *stubIssueGetter) ListIssueComments(_ context.Context, installationID int64, repo githubclient.RepoRef, number int) ([]githubclient.FetchedIssueComment, error) {
	s.commentsCalled = true
	s.commentsGotInst = installationID
	s.commentsGotRepo = repo
	s.commentsGotNum = number
	if s.commentsErr != nil {
		return nil, s.commentsErr
	}
	return s.comments, nil
}

func newPromptServer(t *testing.T) (*Server, *promptRunRepo, *signingFake, *stubIssueGetter) {
	t.Helper()
	rr := newPromptRunRepo()
	sf := newSigningFake()
	gh := &stubIssueGetter{}
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     rr,
		SigningRepo: sf,
	})
	// Inject the stub by overriding the default issueGetter resolver
	// via a dedicated test-only field. promptIssueGetterOverride is
	// nil in production.
	s.promptIssueGetterOverride = gh
	return s, rr, sf, gh
}

func promptRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt", stageID), nil)
	if sigOverride != "" {
		req.Header.Set("X-Fishhawk-Signature", sigOverride)
	} else if priv != nil {
		sig := ed25519.Sign(priv, PromptCanonicalMessage(stageID))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	}
	_ = runID
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// promptRenderRequest exercises the unsigned /prompt-render preview endpoint,
// the sibling surface of promptRequest's signed /prompt dispatch endpoint.
func promptRenderRequest(t *testing.T, s *Server, stageID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestGetStagePrompt_HappyPath_ImplementWithIssue(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StageID != stageID.String() {
		t.Errorf("StageID = %q", resp.StageID)
	}
	if resp.StageType != "implement" {
		t.Errorf("StageType = %q", resp.StageType)
	}
	if resp.Prompt == "" || len(resp.PromptHash) != 64 {
		t.Errorf("empty/short Prompt or PromptHash: %+v", resp)
	}
	// Implement-stage prompt links the issue (#244): title + URL
	// appear, but the body is dropped — the agent is told to fetch.
	for _, want := range []string{
		"Add foo",
		"Triggering issue: #42 · Add foo",
		"https://github.com/kuhlman-labs/example/issues/42",
		"Fetch the issue body via your GitHub tooling",
		"kuhlman-labs/example",
	} {
		if !contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
	if contains(resp.Prompt, "Body text") {
		t.Errorf("implement prompt should not include the issue body verbatim:\n%s", resp.Prompt)
	}

	if !gh.called || gh.gotNum != 42 || gh.gotInst != installation {
		t.Errorf("github stub not called as expected: %+v", gh)
	}
	if gh.gotRepo.Owner != "kuhlman-labs" || gh.gotRepo.Name != "example" {
		t.Errorf("repo parsed wrong: %+v", gh.gotRepo)
	}
}

// makeFixupEntryWithModel builds a stage_fixup_triggered audit entry carrying
// the routed concerns AND (when includeModel) the #1164 pinned fix-up model, so
// the prompt-fetch read-back (fixupResolvedModelFromAudit) is exercisable. With
// includeModel=false the fixup_model key is ABSENT — the pre-#1164 entry shape.
func makeFixupEntryWithModel(runID, stageID uuid.UUID, concerns []planreview.Concern, model, source string, includeModel bool) *audit.Entry {
	fields := map[string]any{
		"stage_id":     stageID.String(),
		"concerns":     concerns,
		"pass_ordinal": 1,
	}
	if includeModel {
		fields["fixup_model"] = model
		fields["fixup_model_source"] = source
	}
	payload, _ := json.Marshal(fields)
	rid := runID
	sid := stageID
	return &audit.Entry{ID: uuid.New(), Category: CategoryStageFixupTriggered, RunID: &rid, StageID: &sid, Payload: payload}
}

// TestGetStagePrompt_Implement_FixupModelPin covers binding conditions 1 & 2
// (#1164): a fix-up dispatch honors the model PINNED on the newest
// stage_fixup_triggered entry at BOTH /prompt and /prompt-render —
//   - a present non-empty pin wins over the run's live spec resolution;
//   - a PRESENT-BUT-EMPTY pin yields an EMPTY implement_model (ok=true), DISTINCT
//     from the no-pin fall-through (which would surface the live spec model);
//   - a no-pin (pre-#1164) fix-up and a non-fix-up dispatch are byte-identical to
//     the live resolveImplementModelForRun result.
func TestGetStagePrompt_Implement_FixupModelPin(t *testing.T) {
	const liveModel = "claude-sonnet-4-6" // the run's live spec executor.model (Y)
	specYAML := []byte("workflows:\n" +
		"  feature_change:\n" +
		"    stages:\n" +
		"      - id: implement\n" +
		"        type: implement\n" +
		"        executor:\n" +
		"          agent: claudecode\n" +
		"          model: " + liveModel + "\n")
	concerns := []planreview.Concern{{Severity: planreview.SeverityLow, Category: "style", Note: "nit"}}

	// run both endpoints, returning the implement_model each carried.
	run2 := func(t *testing.T, entriesFor func(runID, stageID uuid.UUID) []*audit.Entry) (string, string) {
		t.Helper()
		s, rr, sf, _ := newPromptServer(t)
		runID := uuid.New()
		stageID := uuid.New()
		priv, _ := sf.issue(t, runID)
		rr.runRow = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI, WorkflowSpec: specYAML}
		rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
		s.cfg.AuditRepo = &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{runID: entriesFor(runID, stageID)}}

		w := promptRequest(t, s, runID, stageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("/prompt status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var signed promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &signed); err != nil {
			t.Fatalf("decode /prompt: %v", err)
		}
		rreq := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
		rw := httptest.NewRecorder()
		s.Handler().ServeHTTP(rw, rreq)
		if rw.Code != http.StatusOK {
			t.Fatalf("/prompt-render status = %d, want 200:\n%s", rw.Code, rw.Body.String())
		}
		var rendered promptResponse
		if err := json.Unmarshal(rw.Body.Bytes(), &rendered); err != nil {
			t.Fatalf("decode /prompt-render: %v", err)
		}
		return signed.ImplementModel, rendered.ImplementModel
	}

	t.Run("non-empty pin wins over live spec model", func(t *testing.T) {
		signed, rendered := run2(t, func(runID, stageID uuid.UUID) []*audit.Entry {
			return []*audit.Entry{makeFixupEntryWithModel(runID, stageID, concerns, "claude-haiku-4-5-20251001", "operator", true)}
		})
		if signed != "claude-haiku-4-5-20251001" || rendered != "claude-haiku-4-5-20251001" {
			t.Fatalf("implement_model = (/prompt %q, /prompt-render %q), want the pinned claude-haiku-4-5-20251001 on both", signed, rendered)
		}
	})
	t.Run("present-but-empty pin yields empty (distinct from fall-through)", func(t *testing.T) {
		signed, rendered := run2(t, func(runID, stageID uuid.UUID) []*audit.Entry {
			return []*audit.Entry{makeFixupEntryWithModel(runID, stageID, concerns, "", "", true)}
		})
		if signed != "" || rendered != "" {
			t.Fatalf("implement_model = (/prompt %q, /prompt-render %q), want EMPTY on both (present-but-empty pin honored, not the live %q)", signed, rendered, liveModel)
		}
	})
	t.Run("no-pin fix-up falls through to live resolution", func(t *testing.T) {
		signed, rendered := run2(t, func(runID, stageID uuid.UUID) []*audit.Entry {
			return []*audit.Entry{makeFixupEntryWithModel(runID, stageID, concerns, "", "", false)}
		})
		if signed != liveModel || rendered != liveModel {
			t.Fatalf("implement_model = (/prompt %q, /prompt-render %q), want the live %q on both (no-pin fall-through)", signed, rendered, liveModel)
		}
	})
	t.Run("non-fix-up dispatch uses live resolution", func(t *testing.T) {
		signed, rendered := run2(t, func(runID, stageID uuid.UUID) []*audit.Entry {
			return nil // no stage_fixup_triggered entry → not a fix-up
		})
		if signed != liveModel || rendered != liveModel {
			t.Fatalf("implement_model = (/prompt %q, /prompt-render %q), want the live %q on both (non-fix-up)", signed, rendered, liveModel)
		}
	})
}

// TestGetStagePrompt_Implement_CarriesResolvedModel asserts the implement-model
// producer side (#1013): a resolved model (here from the spec executor.model
// rung) is carried on the prompt response under the byte-identical
// `implement_model` json tag the runner decoder reads.
func TestGetStagePrompt_Implement_CarriesResolvedModel(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
		WorkflowSpec: []byte("workflows:\n" +
			"  feature_change:\n" +
			"    stages:\n" +
			"      - id: implement\n" +
			"        type: implement\n" +
			"        executor:\n" +
			"          agent: claudecode\n" +
			"          model: claude-opus-4-8\n"),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "b", State: "open"}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ImplementModel != "claude-opus-4-8" {
		t.Fatalf("ImplementModel = %q, want claude-opus-4-8", resp.ImplementModel)
	}
	// Pin the wire tag byte-identical to the runner decoder.
	if !contains(w.Body.String(), `"implement_model":"claude-opus-4-8"`) {
		t.Fatalf("response missing the implement_model json tag:\n%s", w.Body.String())
	}
}

// TestGetStagePrompt_Implement_EmptyModelOmitted asserts the byte-identical
// today's-spawn path (#1013): with no spec executor.model, no plan
// recommendation, no deployment default and no operator override, the resolved
// model is empty and the implement_model key is OMITTED from the response.
func TestGetStagePrompt_Implement_EmptyModelOmitted(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
		// No WorkflowSpec executor.model, no plan artifact, no default.
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "b", State: "open"}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ImplementModel != "" {
		t.Fatalf("ImplementModel = %q, want empty", resp.ImplementModel)
	}
	if contains(w.Body.String(), "implement_model") {
		t.Fatalf("implement_model key must be omitted when empty (byte-identical spawn):\n%s", w.Body.String())
	}
}

// TestGetStagePrompt_Plan_CarriesResolvedModel asserts the plan-model producer
// side (#1416): a spec-pinned plan executor.model (Scenario B) is carried on the
// plan-stage prompt response under the byte-identical `plan_model` json tag the
// runner decoder reads, and lands on both the signed /prompt and the SPA
// /prompt-render endpoints.
func TestGetStagePrompt_Plan_CarriesResolvedModel(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: "manual",
		WorkflowSpec: []byte("workflows:\n" +
			"  feature_change:\n" +
			"    stages:\n" +
			"      - id: plan\n" +
			"        type: plan\n" +
			"        executor:\n" +
			"          agent: claudecode\n" +
			"          model: claude-opus-4-8\n"),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PlanModel != "claude-opus-4-8" {
		t.Fatalf("PlanModel = %q, want claude-opus-4-8", resp.PlanModel)
	}
	// Pin the wire tag byte-identical to the runner decoder.
	if !contains(w.Body.String(), `"plan_model":"claude-opus-4-8"`) {
		t.Fatalf("response missing the plan_model json tag:\n%s", w.Body.String())
	}
	// The implement_model json field must NOT appear on a plan-stage response
	// (the prompt TEXT may mention implement_model in prose, so match the field).
	if contains(w.Body.String(), `"implement_model":`) {
		t.Fatalf("plan-stage response must not carry the implement_model field:\n%s", w.Body.String())
	}

	// The SPA-readable render path carries the same resolution.
	rreq := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, rreq)
	if rw.Code != http.StatusOK {
		t.Fatalf("render status = %d, want 200:\n%s", rw.Code, rw.Body.String())
	}
	var rendered promptResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &rendered); err != nil {
		t.Fatalf("decode render: %v", err)
	}
	if rendered.PlanModel != "claude-opus-4-8" {
		t.Fatalf("render PlanModel = %q, want claude-opus-4-8", rendered.PlanModel)
	}
}

// TestGetStagePrompt_Plan_EmptyModelOmitted asserts the byte-identical
// today's-spawn path (#1416): with no plan executor.model and no default, the
// resolved plan model is empty and the plan_model key is OMITTED from the
// response.
func TestGetStagePrompt_Plan_EmptyModelOmitted(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: "manual",
		// No WorkflowSpec plan executor.model, no default.
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PlanModel != "" {
		t.Fatalf("PlanModel = %q, want empty", resp.PlanModel)
	}
	if contains(w.Body.String(), "plan_model") {
		t.Fatalf("plan_model key must be omitted when empty (byte-identical spawn):\n%s", w.Body.String())
	}
}

func TestGetStagePrompt_Implement_CarriesScopeJustificationPath(t *testing.T) {
	// #1153: the implement handler populates trigger.ImplementRunID /
	// ImplementStageID, so the rendered prompt carries the run/stage-keyed scope
	// self-exempt sidecar path. Asserting the path proves the ids were threaded.
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantPath := "/tmp/fishhawk-scope-justifications-" + runID.String() + "-" + stageID.String() + ".json"
	if !contains(resp.Prompt, wantPath) {
		t.Errorf("implement prompt missing the keyed scope-justification path %q\n---\n%s", wantPath, resp.Prompt)
	}
	if !contains(resp.Prompt, "### Deliberately-unchanged declared scope files") {
		t.Errorf("implement prompt missing the scope self-exempt section:\n%s", resp.Prompt)
	}
}

func TestGetStagePrompt_PlanStage_DoesNotFetchIssue_WhenNotIssueTriggered(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: "manual",
		// TriggerRef nil → no issue fetch
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue called for non-issue trigger")
	}
}

func TestGetStagePrompt_GitHubFetchFails_StillReturns200(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.getErr = errors.New("github down")

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort issue fetch):\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// Number is preserved (parsed from TriggerRef) even when GitHub
	// fetch fails — the agent still knows which issue to look at,
	// it just doesn't have the body inline.
	if !contains(resp.Prompt, "Triggering issue: #42") {
		t.Errorf("prompt missing issue header:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "Title:") {
		t.Errorf("Title: header should be omitted when fetch failed:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_PrefersCachedIssueContext is the #415
// headline check: when the run row carries a cached IssueContext
// (operator-side `gh issue view` shipped the payload inline at
// run-create), the prompt builder reads from it and never calls
// GitHub. Local-runner runs that have no installation_id depend
// on this path; webhook flows that DO have installation_id should
// still prefer the cache when present (cheaper, no rate-limit
// pressure).
func TestGetStagePrompt_PrefersCachedIssueContext(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		// No InstallationID — the local-runner shape. The cache
		// MUST work without one.
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached title",
			Body:   "Cached body — operator's gh fetched this.",
			URL:    "https://github.com/kuhlman-labs/example/issues/42",
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue should NOT be called when IssueContext is cached on the run row")
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// Plan stage renders the issue body verbatim (unlike implement
	// which links only). Cached body should appear.
	if !contains(resp.Prompt, "Cached body — operator's gh fetched this.") {
		t.Errorf("prompt missing cached body:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Cached title") {
		t.Errorf("prompt missing cached title:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_CachedIssueContext_PreferredOverGitHubFetch
// guards the resolution-order invariant: even when InstallationID
// is set (webhook-dispatched run that ALSO happened to carry
// inline context — an unlikely cohabitation, but worth pinning),
// the cache wins and the GitHub fetch is skipped. Prevents a
// future "let's just always re-fetch" regression.
func TestGetStagePrompt_CachedIssueContext_PreferredOverGitHubFetch(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached",
			Body:   "Cached body",
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	// If the GitHub fetch wins (regression), this would clobber
	// the cache values; the assertions below catch it.
	gh.issue = &githubclient.Issue{Number: 42, Title: "FROM GITHUB", Body: "FROM GITHUB"}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue should NOT be called when cache is populated")
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "Cached body") {
		t.Errorf("cache should win; prompt missing cached body:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "FROM GITHUB") {
		t.Errorf("cache should win; prompt unexpectedly contained GitHub fetch:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_CachedIssueComments_MappedIntoTrigger is the
// #618 check: a cached IssueContext carrying comments renders the
// '### Issue comments' section in the plan-stage prompt with the
// commenter's login.
func TestGetStagePrompt_CachedIssueComments_MappedIntoTrigger(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached title",
			Body:   "Cached body.",
			URL:    "https://github.com/kuhlman-labs/example/issues/42",
			Comments: []run.IssueComment{
				{Author: "alice", Body: "Comment-borne refinement.", CreatedAt: "2026-05-01T10:00:00Z"},
			},
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if gh.called {
		t.Errorf("GetIssue should NOT be called when IssueContext is cached")
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "### Issue comments") {
		t.Errorf("prompt missing comments section:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Comment-borne refinement.") {
		t.Errorf("prompt missing comment body:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "@alice") {
		t.Errorf("prompt missing comment author:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_CachedIssueContext_NoComments guards the
// regression case: a cached IssueContext with no comments still
// renders the body-only plan prompt unchanged — no comments section.
func TestGetStagePrompt_CachedIssueContext_NoComments(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/example",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		IssueContext: &run.IssueContext{
			Number: 42,
			Title:  "Cached title",
			Body:   "Cached body.",
			URL:    "https://github.com/kuhlman-labs/example/issues/42",
		},
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if contains(resp.Prompt, "### Issue comments") {
		t.Errorf("no comments section expected when IssueContext has no comments:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Cached body.") {
		t.Errorf("body-only prompt should still render the body:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_WebhookFetchedComments_MappedIntoTrigger is the
// #621 headline check: a webhook-triggered run (InstallationID set, no
// cached IssueContext) fetches the issue comment thread via branch 2
// and renders it in the plan-stage prompt. It also proves the shared
// writeIssueComments bot-filter applies on this path — a [bot]-authored
// comment is dropped — exercising fetch -> server mapping -> render end
// to end.
func TestGetStagePrompt_WebhookFetchedComments_MappedIntoTrigger(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	var installation int64 = 555
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
		// No IssueContext — forces branch 2 (webhook fetch).
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}
	gh.comments = []githubclient.FetchedIssueComment{
		{Author: "alice", Body: "Comment-borne refinement.", CreatedAt: "2026-05-01T10:00:00Z"},
		{Author: "github-actions[bot]", Body: "CI failed on main.", CreatedAt: "2026-05-01T11:00:00Z"},
	}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if !gh.commentsCalled {
		t.Fatalf("ListIssueComments should be called on the webhook branch")
	}
	if gh.commentsGotInst != installation || gh.commentsGotNum != 42 ||
		gh.commentsGotRepo != (githubclient.RepoRef{Owner: "kuhlman-labs", Name: "example"}) {
		t.Errorf("ListIssueComments args = inst %d repo %+v num %d",
			gh.commentsGotInst, gh.commentsGotRepo, gh.commentsGotNum)
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "### Issue comments") {
		t.Errorf("prompt missing comments section:\n%s", resp.Prompt)
	}
	if !contains(resp.Prompt, "Comment-borne refinement.") || !contains(resp.Prompt, "@alice") {
		t.Errorf("prompt missing human comment:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "CI failed on main.") || contains(resp.Prompt, "github-actions[bot]") {
		t.Errorf("bot-authored comment should be filtered on the webhook path:\n%s", resp.Prompt)
	}
}

// TestGetStagePrompt_WebhookCommentsFetchError_DegradesToBody confirms a
// ListIssueComments failure on the webhook path is best-effort: the
// prompt still returns 200 with title+body and no comments section.
func TestGetStagePrompt_WebhookCommentsFetchError_DegradesToBody(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	var installation int64 = 555
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}
	gh.commentsErr = errors.New("boom")

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !contains(resp.Prompt, "Body text") {
		t.Errorf("title+body should still render on comment-fetch failure:\n%s", resp.Prompt)
	}
	if contains(resp.Prompt, "### Issue comments") {
		t.Errorf("no comments section expected when the fetch failed:\n%s", resp.Prompt)
	}
}

func TestGetStagePrompt_UnsupportedStageType(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageType("review")}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501:\n%s", w.Code, w.Body.String())
	}
}

func TestGetStagePrompt_StageNotFound(t *testing.T) {
	s, _, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetStagePrompt_SignatureMissing(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGetStagePrompt_SignatureBadHex(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, nil, "not-hex")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGetStagePrompt_SignatureWrongKey(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	otherRunID := uuid.New()
	stageID := uuid.New()
	// Issue a key for a DIFFERENT run; sign with that.
	otherPriv, _ := sf.issue(t, otherRunID)
	// But also issue one for the real run so the lookup hits a key
	// (otherwise we'd test ErrNotFound, not ErrSignatureInvalid).
	_, _ = sf.issue(t, runID)
	rr.runRow = &run.Run{ID: runID, Repo: "x/y"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	sig := ed25519.Sign(otherPriv, PromptCanonicalMessage(stageID))
	w := promptRequest(t, s, runID, stageID, nil, hex.EncodeToString(sig))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGetStagePrompt_BadStageUUID(t *testing.T) {
	s, _, _, _ := newPromptServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid/prompt", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetStagePrompt_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		"/v0/stages/"+uuid.New().String()+"/prompt", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestParseIssueRef(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"issue:42", 42, true},
		{"issue:0", 0, false},
		{"issue:-1", 0, false},
		{"pr:42", 0, false},
		{"42", 0, false},
		{"issue:", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := parseIssueRef(c.in)
			if got != c.want || ok != c.wantOK {
				t.Errorf("got (%d, %v), want (%d, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestParseRepoOwnerName(t *testing.T) {
	r, err := parseRepoOwnerName("owner/name")
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if r.Owner != "owner" || r.Name != "name" {
		t.Errorf("got %+v", r)
	}
	if _, err := parseRepoOwnerName("nope"); err == nil {
		t.Error("expected error for missing slash")
	}
	if _, err := parseRepoOwnerName("/x"); err == nil {
		t.Error("expected error for empty owner")
	}
	if _, err := parseRepoOwnerName("x/"); err == nil {
		t.Error("expected error for empty name")
	}
	if _, err := parseRepoOwnerName("a/b/c"); err == nil {
		t.Error("expected error for multi-slash")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestGetStagePromptRender_HappyPath_NoSignatureRequired confirms the
// SPA-side endpoint constructs the same prompt as the runner's path
// without requiring X-Fishhawk-Signature (#215). Auth tracks the
// existing stage/audit reads — no header check at the handler level.
func TestGetStagePromptRender_HappyPath_NoSignatureRequired(t *testing.T) {
	s, rr, _, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StageType != "implement" {
		t.Errorf("StageType = %q", resp.StageType)
	}
	// Implement-stage prompt links the issue (#244): title + URL
	// appear, but the body is dropped — the agent is told to fetch.
	for _, want := range []string{
		"Add foo",
		"Triggering issue: #42 · Add foo",
		"https://github.com/kuhlman-labs/example/issues/42",
		"Fetch the issue body via your GitHub tooling",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, resp.Prompt)
		}
	}
	if strings.Contains(resp.Prompt, "Body text") {
		t.Errorf("implement prompt should not include the issue body verbatim:\n%s", resp.Prompt)
	}
}

// TestGetStagePromptRender_MatchesSignatureAuthedPath asserts both
// endpoints produce byte-identical prompts for the same stage —
// they have to, because the audit story depends on the SPA showing
// the same text the runner saw.
func TestGetStagePromptRender_MatchesSignatureAuthedPath(t *testing.T) {
	s, rr, sf, gh := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	installation := int64(99)
	triggerRef := "issue:42"
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	gh.issue = &githubclient.Issue{Number: 42, Title: "T", Body: "B", State: "open"}

	signed := promptRequest(t, s, runID, stageID, priv, "")
	if signed.Code != http.StatusOK {
		t.Fatalf("signed status = %d", signed.Code)
	}
	rendered := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", stageID), nil)
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, rendered)
	if rw.Code != http.StatusOK {
		t.Fatalf("rendered status = %d", rw.Code)
	}

	var signedBody, renderedBody promptResponse
	_ = json.Unmarshal(signed.Body.Bytes(), &signedBody)
	_ = json.Unmarshal(rw.Body.Bytes(), &renderedBody)
	if signedBody.Prompt != renderedBody.Prompt {
		t.Errorf("prompt diverged between signed + rendered paths:\nsigned:\n%s\n---\nrendered:\n%s",
			signedBody.Prompt, renderedBody.Prompt)
	}
	if signedBody.PromptHash != renderedBody.PromptHash {
		t.Errorf("hash diverged: %q vs %q", signedBody.PromptHash, renderedBody.PromptHash)
	}
	// The fix-up wire flag (#784) is set in BOTH handlers; assert they stay
	// consistent so a change to one (runner-facing) handler that misses the
	// other (SPA render) is caught. The fixup=true path itself is covered by
	// TestGetStagePrompt_Implement_FixupConcerns_RenderedAndFolded on the
	// runner-facing handler; here both are false (plan stage, no fix-up entry),
	// which still guards against a structural divergence between the two.
	if signedBody.Fixup != renderedBody.Fixup || signedBody.FixupBranch != renderedBody.FixupBranch {
		t.Errorf("fix-up wire flag diverged: signed={%v,%q} rendered={%v,%q}",
			signedBody.Fixup, signedBody.FixupBranch, renderedBody.Fixup, renderedBody.FixupBranch)
	}
}

// planStageSpecYAML is a valid feature_change workflow spec with a
// workflow-level policy max_stage_runtime of 30m. No per-stage executor
// timeouts so both plan and implement resolve to the policy value.
const planStageSpecYAML30m = `version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "30m"
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
`

// planStageSpecYAML45mImpl is the same workflow but the implement stage
// declares executor.timeout: "45m", which overrides the 30m workflow policy.
const planStageSpecYAML45mImpl = `version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "30m"
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
          timeout: "45m"
        produces:
          - artifact: pull_request
`

// TestGetStagePrompt_PlanBudget_WorkflowPolicy exercises the three-level
// timeout precedence (stage executor > workflow policy > 15m default)
// through the full server path for a plan-stage prompt. Each case asserts
// on the "implement stage N minutes" text rendered into the prompt body.
func TestGetStagePrompt_PlanBudget_WorkflowPolicy(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(planStageSpecYAML30m),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Prompt, "implement stage 30 minutes") {
		t.Errorf("prompt missing 'implement stage 30 minutes' (workflow policy):\n%s", resp.Prompt)
	}
}

func TestGetStagePrompt_PlanBudget_StageExecutorOverridesPolicy(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(planStageSpecYAML45mImpl),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Prompt, "implement stage 45 minutes") {
		t.Errorf("prompt missing 'implement stage 45 minutes' (stage executor override):\n%s", resp.Prompt)
	}
}

func TestGetStagePrompt_PlanBudget_NilSpecFallsBackTo15m(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  nil,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Prompt, "implement stage 15 minutes") {
		t.Errorf("prompt missing 'implement stage 15 minutes' (nil spec default):\n%s", resp.Prompt)
	}
}

func TestGetStagePromptRender_StageNotFound(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	rr.stageErr = run.ErrNotFound
	req := httptest.NewRequest(http.MethodGet,
		"/v0/stages/"+uuid.New().String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetStagePromptRender_BadStageUUID(t *testing.T) {
	s, _, _, _ := newPromptServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetStagePromptRender_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet,
		"/v0/stages/"+uuid.New().String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetStagePrompt_DecomposedFromRunID_Present(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	parentRunID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	sliceIdx := 2
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		DecomposedFrom: &parentRunID,
		SliceIndex:     &sliceIdx,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DecomposedFromRunID != parentRunID.String() {
		t.Errorf("DecomposedFromRunID = %q, want %q", resp.DecomposedFromRunID, parentRunID.String())
	}
	// The persisted slice_index is surfaced so the runner can route the
	// child onto fishhawk/run-<parent>/slice-<n> (E24.1 / #1141).
	if resp.SliceIndex != 2 {
		t.Errorf("SliceIndex = %d, want 2", resp.SliceIndex)
	}
}

// TestGetStagePrompt_SliceIndex_Zero_ForSliceZeroChild covers the
// omitempty-drops-0 case: a decomposed child at slice 0 carries a non-nil
// *0 SliceIndex on the row; the response field is dropped on the wire by
// omitempty but the runner reads it as the zero value — the correct value
// for slice 0 (it only reads slice_index when decomposed_from_run_id is set).
func TestGetStagePrompt_SliceIndex_Zero_ForSliceZeroChild(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	parentRunID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	zero := 0
	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		DecomposedFrom: &parentRunID,
		SliceIndex:     &zero,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "slice_index") {
		t.Errorf("response body unexpectedly carries slice_index for slice 0 (omitempty should drop it):\n%s", w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SliceIndex != 0 {
		t.Errorf("SliceIndex = %d, want 0", resp.SliceIndex)
	}
}

func TestGetStagePrompt_DecomposedFromRunID_Absent_ForStandaloneRun(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		// DecomposedFrom nil → standalone run
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DecomposedFromRunID != "" {
		t.Errorf("DecomposedFromRunID = %q, want empty for standalone run", resp.DecomposedFromRunID)
	}
	// A standalone run has nil SliceIndex; the field is omitted on the wire
	// and decodes to 0 (the runner never reads it without decomposed_from).
	if strings.Contains(w.Body.String(), "slice_index") {
		t.Errorf("response body unexpectedly carries slice_index for standalone run:\n%s", w.Body.String())
	}
	if resp.SliceIndex != 0 {
		t.Errorf("SliceIndex = %d, want 0 for standalone run", resp.SliceIndex)
	}
}

// TestGetStagePrompt_StateGuard_* cover the 409 state guard on the
// runner-facing prompt endpoint. One test per non-runnable state, plus
// three no-regression checks for the runnable states.
func TestGetStagePrompt_StateGuard_AwaitingApproval(t *testing.T) {
	testPromptStateGuard(t, run.StageStateAwaitingApproval, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_AwaitingChildren(t *testing.T) {
	testPromptStateGuard(t, run.StageStateAwaitingChildren, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Succeeded(t *testing.T) {
	testPromptStateGuard(t, run.StageStateSucceeded, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Failed(t *testing.T) {
	testPromptStateGuard(t, run.StageStateFailed, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Cancelled(t *testing.T) {
	testPromptStateGuard(t, run.StageStateCancelled, http.StatusConflict)
}

func TestGetStagePrompt_StateGuard_Pending_Passes(t *testing.T) {
	testPromptStateGuard(t, run.StageStatePending, http.StatusOK)
}

func TestGetStagePrompt_StateGuard_Dispatched_Passes(t *testing.T) {
	testPromptStateGuard(t, run.StageStateDispatched, http.StatusOK)
}

func TestGetStagePrompt_StateGuard_Running_Passes(t *testing.T) {
	testPromptStateGuard(t, run.StageStateRunning, http.StatusOK)
}

func testPromptStateGuard(t *testing.T, state run.StageState, wantStatus int) {
	t.Helper()
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{ID: runID, Repo: "x/y", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement, State: state}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, wantStatus, w.Body.String())
	}
	if wantStatus == http.StatusConflict {
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.Error.Code != "stage_not_runnable" {
			t.Errorf("error.code = %q, want stage_not_runnable", env.Error.Code)
		}
		if env.Error.Details["current_state"] != string(state) {
			t.Errorf("current_state = %v, want %q", env.Error.Details["current_state"], string(state))
		}
		if env.Error.Details["stage_id"] != stageID.String() {
			t.Errorf("stage_id = %v, want %q", env.Error.Details["stage_id"], stageID.String())
		}
	}
}

// TestGetStagePromptRender_StateGuard_* cover the 409 state guard on
// the SPA-facing prompt-render endpoint.
func TestGetStagePromptRender_StateGuard_AwaitingApproval(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateAwaitingApproval, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_AwaitingChildren(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateAwaitingChildren, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Succeeded(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateSucceeded, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Failed(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateFailed, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Cancelled(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateCancelled, http.StatusConflict)
}

func TestGetStagePromptRender_StateGuard_Pending_Passes(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStatePending, http.StatusOK)
}

func TestGetStagePromptRender_StateGuard_Dispatched_Passes(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateDispatched, http.StatusOK)
}

func TestGetStagePromptRender_StateGuard_Running_Passes(t *testing.T) {
	testPromptRenderStateGuard(t, run.StageStateRunning, http.StatusOK)
}

func testPromptRenderStateGuard(t *testing.T, state run.StageState, wantStatus int) {
	t.Helper()
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{ID: runID, Repo: "x/y", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement, State: state}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, wantStatus, w.Body.String())
	}
	if wantStatus == http.StatusConflict {
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.Error.Code != "stage_not_runnable" {
			t.Errorf("error.code = %q, want stage_not_runnable", env.Error.Code)
		}
		if env.Error.Details["current_state"] != string(state) {
			t.Errorf("current_state = %v, want %q", env.Error.Details["current_state"], string(state))
		}
		if env.Error.Details["stage_id"] != stageID.String() {
			t.Errorf("stage_id = %v, want %q", env.Error.Details["stage_id"], stageID.String())
		}
	}
}

func TestGetStagePromptRender_DecomposedFromRunID_Present(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	parentRunID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		DecomposedFrom: &parentRunID,
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DecomposedFromRunID != parentRunID.String() {
		t.Errorf("DecomposedFromRunID = %q, want %q", resp.DecomposedFromRunID, parentRunID.String())
	}
}

// specWithVerifyYAML is a minimal feature_change spec where the implement
// stage declares executor.verify.command, .timeout, and .max_iterations.
const specWithVerifyYAML = `version: "0.3"
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
          verify:
            command: "scripts/test"
            timeout: "5m"
            max_iterations: 3
        produces:
          - artifact: pull_request
`

// TestGetStagePrompt_VerifyConfig_Present confirms that when the workflow
// spec declares executor.verify, the prompt response carries verify_command
// and verify_timeout_seconds.
func TestGetStagePrompt_VerifyConfig_Present(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(specWithVerifyYAML),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.VerifyCommand != "scripts/test" {
		t.Errorf("VerifyCommand = %q, want %q", resp.VerifyCommand, "scripts/test")
	}
	if resp.VerifyTimeoutSeconds != 300 {
		t.Errorf("VerifyTimeoutSeconds = %d, want 300 (5m)", resp.VerifyTimeoutSeconds)
	}
	if resp.VerifyMaxIterations != 3 {
		t.Errorf("VerifyMaxIterations = %d, want 3", resp.VerifyMaxIterations)
	}
}

// TestGetStagePrompt_VerifyMaxIterations_SignedEndpoint confirms the
// signed GET /v0/stages/{id}/prompt endpoint serves verify_max_iterations
// from executor.verify.max_iterations, mirroring the prompt-render path.
func TestGetStagePrompt_VerifyMaxIterations_SignedEndpoint(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(specWithVerifyYAML),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.VerifyMaxIterations != 3 {
		t.Errorf("VerifyMaxIterations = %d, want 3", resp.VerifyMaxIterations)
	}
}

// TestGetStagePrompt_VerifyConfig_Absent confirms that when the workflow
// spec declares no executor.verify block, both verify fields are omitted
// from the JSON response (omitempty).
func TestGetStagePrompt_VerifyConfig_Absent(t *testing.T) {
	s, rr, _, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()

	rr.runRow = &run.Run{
		ID:            runID,
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(planStageSpecYAML30m),
	}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String()+"/prompt-render", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// Assert on the raw JSON bytes so omitempty behaviour is visible.
	body := w.Body.String()
	if strings.Contains(body, "verify_command") {
		t.Errorf("response JSON should not contain verify_command when spec has none:\n%s", body)
	}
	if strings.Contains(body, "verify_timeout_seconds") {
		t.Errorf("response JSON should not contain verify_timeout_seconds when spec has none:\n%s", body)
	}
}

// --- loadPriorRejectionFeedback unit tests ---

// feedbackRunRepo wraps promptRunRepo to supply canned ListRuns results.
type feedbackRunRepo struct {
	*promptRunRepo
	listResult []*run.Run
	listErr    error
}

func (r *feedbackRunRepo) ListRuns(_ context.Context, _ run.ListRunsFilter) ([]*run.Run, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.listResult, nil
}

// feedbackAuditRepo is a minimal audit.Repository for loadPriorRejectionFeedback tests.
type feedbackAuditRepo struct {
	byRunID map[uuid.UUID][]*audit.Entry
	listErr error
}

func (f *feedbackAuditRepo) Append(_ context.Context, _ audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}
func (f *feedbackAuditRepo) AppendGlobalChained(_ context.Context, _ audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) Get(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) ListGlobal(_ context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *feedbackAuditRepo) LastForRun(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (f *feedbackAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Honour the category filter so callers that query the wrong constant
	// (e.g. a typo in CategoryStageFixupTriggered) see nothing — otherwise
	// the resolver tests would pass without pinning the constant.
	var out []*audit.Entry
	for _, e := range f.byRunID[runID] {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *feedbackAuditRepo) ListAll(_ context.Context, _ audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *feedbackAuditRepo) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}

// storingAuditRepo is an audit.Repository fake whose AppendChained actually
// PERSISTS the entry — preserving Category and Payload, keyed by RunID — into
// the embedded feedbackAuditRepo's byRunID, so a single in-process test can
// round-trip the approval-audit WRITE (handleSubmitApproval -> writeApprovalAudit
// -> AppendChained) through the implement prompt-fetch READ (handleGetStagePrompt
// -> loadApprovalAddScopeFiles -> ListForRunByCategory). feedbackAuditRepo's
// AppendChained is a no-op that discards the write — which is exactly why the
// existing fold test (TestGetStagePrompt_Implement_AddScopeFilesFoldedIntoScope)
// hand-seeds the read side and so cannot reproduce the #1351
// approve->dispatch->first-fetch seam. This fake closes that gap. The
// category-filtering ListForRunByCategory reader and every other interface method
// are inherited from feedbackAuditRepo.
type storingAuditRepo struct {
	*feedbackAuditRepo
}

func newStoringAuditRepo() *storingAuditRepo {
	return &storingAuditRepo{feedbackAuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{}}}
}

// AppendChained persists the entry so a later ListForRunByCategory on the same
// fake returns it. This is the only override; every other audit.Repository
// method is the embedded feedbackAuditRepo's.
func (a *storingAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.listErr != nil {
		return nil, a.listErr
	}
	rid := p.RunID
	e := &audit.Entry{ID: uuid.New(), RunID: &rid, Category: p.Category, Payload: p.Payload}
	a.byRunID[p.RunID] = append(a.byRunID[p.RunID], e)
	return e, nil
}

func newFeedbackServer(t *testing.T, runs []*run.Run, auditByRun map[uuid.UUID][]*audit.Entry) *Server {
	t.Helper()
	rr := &feedbackRunRepo{
		promptRunRepo: newPromptRunRepo(),
		listResult:    runs,
	}
	ar := &feedbackAuditRepo{byRunID: auditByRun}
	return New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   rr,
		AuditRepo: ar,
	})
}

func makeRejectionEntry(runID uuid.UUID, comment string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision":          "reject",
		"rejection_comment": comment,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

func makeApproveEntry(runID uuid.UUID) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"decision": "approve"})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

func TestLoadPriorRejectionFeedback_NoPriorRuns_ReturnsNil(t *testing.T) {
	s := newFeedbackServer(t, nil, nil)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", uuid.New())
	if got != nil {
		t.Errorf("got %q, want nil (no prior runs)", *got)
	}
}

func TestLoadPriorRejectionFeedback_PriorRunNoRejection_ReturnsNil(t *testing.T) {
	priorID := uuid.New()
	currentID := uuid.New()
	s := newFeedbackServer(t,
		[]*run.Run{{ID: priorID}},
		map[uuid.UUID][]*audit.Entry{priorID: {makeApproveEntry(priorID)}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got != nil {
		t.Errorf("got %q, want nil (no rejection in prior run)", *got)
	}
}

func TestLoadPriorRejectionFeedback_PriorRunRejectionEmptyComment_ReturnsNil(t *testing.T) {
	priorID := uuid.New()
	currentID := uuid.New()
	payload, _ := json.Marshal(map[string]any{"decision": "reject", "rejection_comment": ""})
	rid := priorID
	s := newFeedbackServer(t,
		[]*run.Run{{ID: priorID}},
		map[uuid.UUID][]*audit.Entry{priorID: {{ID: uuid.New(), RunID: &rid, Payload: payload}}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got != nil {
		t.Errorf("got %q, want nil (rejection with empty comment)", *got)
	}
}

func TestLoadPriorRejectionFeedback_PriorRunRejectionNonEmptyComment_ReturnsComment(t *testing.T) {
	priorID := uuid.New()
	currentID := uuid.New()
	s := newFeedbackServer(t,
		[]*run.Run{{ID: priorID}},
		map[uuid.UUID][]*audit.Entry{priorID: {makeRejectionEntry(priorID, "plan is too vague")}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got == nil {
		t.Fatal("got nil, want comment")
	}
	if *got != "plan is too vague" {
		t.Errorf("got %q, want 'plan is too vague'", *got)
	}
}

func TestLoadPriorRejectionFeedback_CurrentRunIDExcluded(t *testing.T) {
	currentID := uuid.New()
	// The only run in the list is the current one — must be skipped.
	s := newFeedbackServer(t,
		[]*run.Run{{ID: currentID}},
		map[uuid.UUID][]*audit.Entry{currentID: {makeRejectionEntry(currentID, "do not return this")}},
	)
	got := s.loadPriorRejectionFeedback(context.Background(), "x/y", "issue:42", currentID)
	if got != nil {
		t.Errorf("got %q, want nil (current run should be excluded)", *got)
	}
}

// --- loadPriorSchemaValidationError unit tests (#646) ---

func makeSchemaRetryEntry(runID uuid.UUID, validationErr string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"validation_error": validationErr,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "plan_schema_retry", RunID: &rid, Payload: payload}
}

func TestLoadPriorSchemaValidationError_NewestWins(t *testing.T) {
	runID := uuid.New()
	// Entries are returned ASC by ts; the newest (last) must win.
	s := newFeedbackServer(t, nil, map[uuid.UUID][]*audit.Entry{
		runID: {
			makeSchemaRetryEntry(runID, "first error"),
			makeSchemaRetryEntry(runID, "second error"),
		},
	})
	got := s.loadPriorSchemaValidationError(context.Background(), runID)
	if got == nil {
		t.Fatal("got nil, want newest validation_error")
	}
	if *got != "second error" {
		t.Errorf("got %q, want %q (newest entry wins)", *got, "second error")
	}
}

func TestLoadPriorSchemaValidationError_NoEntries_ReturnsNil(t *testing.T) {
	s := newFeedbackServer(t, nil, nil)
	if got := s.loadPriorSchemaValidationError(context.Background(), uuid.New()); got != nil {
		t.Errorf("got %q, want nil (no entries)", *got)
	}
}

func TestLoadPriorSchemaValidationError_ListError_ReturnsNil(t *testing.T) {
	rr := &feedbackRunRepo{promptRunRepo: newPromptRunRepo()}
	ar := &feedbackAuditRepo{listErr: errors.New("boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar})
	if got := s.loadPriorSchemaValidationError(context.Background(), uuid.New()); got != nil {
		t.Errorf("got %q, want nil (list error degrades to nil)", *got)
	}
}

// TestGetStagePrompt_Implement_EchoesScopeFiles verifies that the
// implement-stage prompt response echoes the approved plan's
// scope.files into the scope_files field, so the runner can bound the
// commit to exactly those declared paths (#581).
func TestGetStagePrompt_Implement_EchoesScopeFiles(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
				{Path: "docs/api/v0.md", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.ScopeFiles) != 2 {
		t.Fatalf("scope_files len = %d, want 2: %+v", len(resp.ScopeFiles), resp.ScopeFiles)
	}
	if resp.ScopeFiles[0].Path != "backend/internal/server/prompt.go" || resp.ScopeFiles[0].Operation != "modify" {
		t.Errorf("scope_files[0] = %+v", resp.ScopeFiles[0])
	}
	if resp.ScopeFiles[1].Path != "docs/api/v0.md" || resp.ScopeFiles[1].Operation != "modify" {
		t.Errorf("scope_files[1] = %+v", resp.ScopeFiles[1])
	}
}

// makeFixupEntry builds a stage_fixup_triggered audit entry bound to the
// given stage, carrying the resolved selected concerns the prompt renderer
// reads back (matching server/fixup.go's writeFixupAudit payload shape).
func makeFixupEntry(runID, stageID uuid.UUID, concerns []planreview.Concern) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"stage_id":         stageID.String(),
		"selected_indices": []int{0},
		"concerns":         concerns,
		"reason":           "operator routed concerns back",
		"pass_ordinal":     1,
	})
	rid := runID
	sid := stageID
	return &audit.Entry{ID: uuid.New(), Category: CategoryStageFixupTriggered, RunID: &rid, StageID: &sid, Payload: payload}
}

// makeReportedHeadEntry builds a reported-head ledger audit entry
// (pull_request_opened / child_pushed / fixup_pushed) carrying a head_sha
// at the given timestamp, so resolveFixupExpectedHeadSHA's newest-entry
// pick can be exercised (#967).
func makeReportedHeadEntry(runID, stageID uuid.UUID, category, headSHA string, ts time.Time) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"head_sha": headSHA})
	rid := runID
	sid := stageID
	return &audit.Entry{ID: uuid.New(), Category: category, RunID: &rid, StageID: &sid,
		Timestamp: ts, Payload: payload}
}

// TestGetStagePrompt_Implement_FixupConcerns_RetainsFullPlanScope confirms that
// when an implement stage carries a stage_fixup_triggered audit entry, the
// prompt renders the selected concerns as binding instructions and RETAINS the
// FULL approved plan scope as the effective fix-up scope (#1314), reversing the
// #1162 concern-surface narrowing. Mode 2 of effectiveFixupScope: a non-empty
// plan scope with no amendment/allow_create retains EVERY approved-plan file —
// including a file no routed concern names — so the agent's legitimate in-plan
// edits are committed rather than drift-excluded. The concern's prose no longer
// scrapes paths into scope, so a file referenced only by a concern Note (and not
// in plan scope) stays absent.
func TestGetStagePrompt_Implement_FixupConcerns_RetainsFullPlanScope(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	// Plan scope is {prompt.go, prompt_test.go}: prompt_test.go is prompt.go's
	// own coupled sibling, so the #1214 sibling fold derives nothing new and the
	// effective scope equals the plan scope exactly (clean count assertion).
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
				{Path: "backend/internal/server/prompt_test.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "coverage",
			Note: "add a test in backend/internal/run/fixup_test.go for the bound"},
	}
	// Seed the reported-head ledger: a PR-open head then a NEWER fixup_pushed
	// head, so the expected-head resolver must pick the newest entry across
	// categories rather than the first one it sees (#967).
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {
			makeFixupEntry(runID, implStageID, concerns),
			makeReportedHeadEntry(runID, implStageID, "pull_request_opened",
				"aaaa000000000000000000000000000000000000", time.Now().Add(-2*time.Hour)),
			makeReportedHeadEntry(runID, implStageID, "fixup_pushed",
				"bbbb111111111111111111111111111111111111", time.Now().Add(-1*time.Hour)),
		},
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The prompt renders the binding fix-up concerns section.
	for _, want := range []string{
		"### Fix-up concerns",
		"[high/coverage]",
		"add a test in backend/internal/run/fixup_test.go for the bound",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}

	// The effective scope RETAINS the FULL plan scope (#1314): BOTH approved-plan
	// files are present even though no concern names them, so the agent's in-plan
	// edits ship rather than being drift-excluded.
	paths := map[string]bool{}
	for _, f := range resp.ScopeFiles {
		paths[f.Path] = true
	}
	if !paths["backend/internal/server/prompt.go"] {
		t.Errorf("approved-plan file missing from scope_files: %+v", resp.ScopeFiles)
	}
	if !paths["backend/internal/server/prompt_test.go"] {
		t.Errorf("approved-plan file missing from scope_files: %+v", resp.ScopeFiles)
	}
	// Concern prose no longer scrapes paths into scope (#1314 removed the
	// concern-surface fold): a file named only by a concern Note, absent from
	// plan scope, stays out of scope.
	if paths["backend/internal/run/fixup_test.go"] {
		t.Errorf("concern-named file (not in plan scope) must NOT be folded into scope_files: %+v", resp.ScopeFiles)
	}
	if len(resp.ScopeFiles) != 2 {
		t.Errorf("effective scope must equal the full plan scope (2 files), got %+v", resp.ScopeFiles)
	}

	// Cross-boundary assertion (#784): the response carries the fix-up wire
	// flag the runner reads, and fixup_branch matches the runner's per-stage
	// branch formula byte-for-byte. A divergence here would re-create the
	// `checkout -b <existing branch>` already-exists failure.
	if !resp.Fixup {
		t.Errorf("fixup = false, want true for a stage with an unconsumed stage_fixup_triggered entry")
	}
	wantBranch := fmt.Sprintf("fishhawk/run-%s/stage-%s", runID.String()[:8], implStageID.String()[:8])
	if resp.FixupBranch != wantBranch {
		t.Errorf("fixup_branch = %q, want %q", resp.FixupBranch, wantBranch)
	}

	// The fix-up dispatch advertises the run's recorded head — the NEWEST
	// reported head across the lineage ledger categories (#967): here the
	// fixup_pushed head, not the older pull_request_opened one.
	if want := "bbbb111111111111111111111111111111111111"; resp.FixupExpectedHeadSHA != want {
		t.Errorf("fixup_expected_head_sha = %q, want %q (the newest reported head)",
			resp.FixupExpectedHeadSHA, want)
	}
}

// TestGetStagePrompt_Implement_FixupConcerns_FoldsCoupledTestSibling is the
// done-means test for #1214 under the #1314 full-retention base (mode 5): a
// fix-up whose plan scope contains a production .go file must land the agent's
// coupled *_test.go sibling in the same commit instead of having it stripped as
// scope_drift. It drives a real fix-up prompt dispatch and asserts the effective
// scope.files contains BOTH the plan-scope production file AND its <stem>_test.go
// sibling as operation=modify (behavioral, not presence-based). The subtests
// cover the branches the fold touches: (a) a production plan-scope file folds
// the sibling; (b) a plan scope of ONLY a *_test.go file folds nothing extra
// (coupledTestSiblings skips _test.go inputs). The empty-plan-scope path (no
// sibling fold of an empty set) is covered by
// TestGetStagePrompt_Implement_FixupConcerns_EmptyPlanScopeStaysEmpty.
func TestGetStagePrompt_Implement_FixupConcerns_FoldsCoupledTestSibling(t *testing.T) {
	// dispatchFixup builds a fix-up prompt dispatch whose routed concern carries
	// concernNote (prose only — no longer scraped into scope after #1314) and
	// whose plan scope is planScopePath, returning the effective scope.files from
	// the prompt response.
	dispatchFixup := func(t *testing.T, planScopePath, concernNote string) []scopeFile {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "scoped plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{
					{Path: planScopePath, Operation: plan.FileOpModify},
				},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
				{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
			},
		}
		rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		concerns := []planreview.Concern{
			{Severity: planreview.SeverityHigh, Category: "coverage", Note: concernNote},
		}
		auditByRun := map[uuid.UUID][]*audit.Entry{
			runID: {makeFixupEntry(runID, implStageID, concerns)},
		}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.Fixup {
			t.Fatalf("fixup = false, want true (an unconsumed stage_fixup_triggered entry)")
		}
		return resp.ScopeFiles
	}

	t.Run("production plan-scope file folds the coupled test sibling", func(t *testing.T) {
		// The plan scope names only main.go; main_test.go must be auto-folded into
		// the effective scope as operation=modify so the agent's coupled test edit
		// is committed, not stripped as scope_drift (#1214). The concern is prose.
		got := dispatchFixup(t,
			"runner/cmd/fishhawk-runner/main.go",
			"fix the off-by-one in the loop bound")

		op := map[string]string{}
		for _, f := range got {
			op[f.Path] = f.Operation
		}
		if op["runner/cmd/fishhawk-runner/main.go"] != "modify" {
			t.Errorf("plan-scope production file missing or not modify: %+v", got)
		}
		if op["runner/cmd/fishhawk-runner/main_test.go"] != "modify" {
			t.Errorf("coupled <stem>_test.go sibling must be folded as operation=modify: %+v", got)
		}
		if len(got) != 2 {
			t.Errorf("effective scope must be exactly {production, sibling test}, got %+v", got)
		}
	})

	t.Run("test-only plan scope folds no extra sibling", func(t *testing.T) {
		// coupledTestSiblings skips *_test.go inputs, so a plan scope of only a
		// test file does not spuriously derive a second sibling.
		got := dispatchFixup(t,
			"backend/internal/run/fixup_test.go",
			"strengthen the assertion")

		op := map[string]string{}
		for _, f := range got {
			op[f.Path] = f.Operation
		}
		if op["backend/internal/run/fixup_test.go"] != "modify" {
			t.Errorf("plan-scope test file missing from scope: %+v", got)
		}
		if len(got) != 1 {
			t.Errorf("a test-only plan scope must not fold a second sibling, got %+v", got)
		}
	})
}

// TestGetStagePrompt_Implement_FixupConcerns_EmptyPlanScopeStaysEmpty covers the
// empty-plan-scope path of effectiveFixupScope (mode 6): a fix-up whose approved
// plan carries NO scope.files (a plan_missing_for_implement fix-up) must keep an
// empty effective scope so the runner's `git add -A` fallback is preserved — we
// do not synthesize a scope. This is the one branch where effectiveFixupScope
// returns its input unchanged.
func TestGetStagePrompt_Implement_FixupConcerns_EmptyPlanScopeStaysEmpty(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	// Plan with an EMPTY scope.files — the plan_missing_for_implement shape.
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scopeless plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope:        plan.Scope{Files: []plan.ScopeFile{}},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	// A routed concern (so the fix-up path engages) plus an allow_create — neither
	// must synthesize a scope when the plan scope is empty.
	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "correctness",
			Note: "reconsider the overall approach; the current logic is incorrect"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {makeFixupEntryWithAllowCreate(runID, implStageID, concerns, []string{"backend/internal/server/new.go"})},
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.Fixup {
		t.Errorf("fixup = false, want true (an unconsumed stage_fixup_triggered entry)")
	}
	// An empty plan scope stays empty — the runner's git add -A fallback is
	// preserved and no allow_create/amendment fold synthesizes a scope.
	if len(resp.ScopeFiles) != 0 {
		t.Errorf("empty plan scope must stay empty (git add -A fallback), got %+v", resp.ScopeFiles)
	}
}

// TestGetStagePrompt_Implement_FixupConcerns_AmendmentIncluded covers the
// approved-scope-amendment fold of effectiveFixupScope under the #1314
// full-retention base (mode 3): a path approved via the mid-stage scope-amendment
// escape hatch is folded ON TOP of the full plan scope. The full plan scope is
// retained, the amendment widens it, and both the amendment's coupled test
// sibling and the plan file's sibling land in the same commit.
func TestGetStagePrompt_Implement_FixupConcerns_AmendmentIncluded(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()
	sa := newFakeScopeAmendmentRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "coverage",
			Note: "add a case in backend/internal/run/fixup_test.go"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {makeFixupEntry(runID, implStageID, concerns)},
	}

	// Approve a scope amendment for a path no concern references.
	a, err := sa.Create(context.Background(), scopeamendment.CreateParams{
		RunID: runID, StageID: implStageID,
		Paths:  []scopeamendment.PathEntry{{Path: "backend/internal/server/amended.go", Operation: scopeamendment.OperationModify}},
		Reason: "coupled seam",
	})
	if err != nil {
		t.Fatalf("create amendment: %v", err)
	}
	if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator",
	}); err != nil {
		t.Fatalf("approve amendment: %v", err)
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		SigningRepo:        sf,
		ArtifactRepo:       art,
		AuditRepo:          &feedbackAuditRepo{byRunID: auditByRun},
		ScopeAmendmentRepo: sa,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	paths := map[string]bool{}
	for _, f := range resp.ScopeFiles {
		paths[f.Path] = true
	}
	// The full plan scope is RETAINED (#1314): the plan file is present even
	// though no concern or amendment names it.
	if !paths["backend/internal/server/prompt.go"] {
		t.Errorf("approved-plan file missing from scope_files (full scope must be retained): %+v", resp.ScopeFiles)
	}
	// The approved amendment is folded ON TOP of the full plan scope.
	if !paths["backend/internal/server/amended.go"] {
		t.Errorf("approved scope-amendment path missing from scope_files: %+v", resp.ScopeFiles)
	}
	// Both the plan file and the amendment source are production .go files, so the
	// coupled-test-sibling fold (#1214) pulls in BOTH <stem>_test.go siblings —
	// the fold operates on the entire retained set (plan scope + amendment).
	if !paths["backend/internal/server/prompt_test.go"] {
		t.Errorf("coupled <stem>_test.go sibling of the plan file must be folded: %+v", resp.ScopeFiles)
	}
	if !paths["backend/internal/server/amended_test.go"] {
		t.Errorf("coupled <stem>_test.go sibling of the amendment source must be folded: %+v", resp.ScopeFiles)
	}
	// Concern prose no longer scrapes paths into scope (#1314): a file named only
	// by the concern Note, absent from plan scope and the amendment, stays out.
	if paths["backend/internal/run/fixup_test.go"] {
		t.Errorf("concern-named file (not in plan scope or amendment) must NOT be in scope_files: %+v", resp.ScopeFiles)
	}
	if len(resp.ScopeFiles) != 4 {
		t.Errorf("scope_files must be the full plan scope + amendment + both coupled test siblings (4 files), got %+v", resp.ScopeFiles)
	}
}

// TestGetStagePrompt_Implement_FixupConcerns_ProseConcernPlusAmendment is the
// #1314 regression test (mode 1): the exact silent-collapse the fix reverses. A
// fix-up dispatch whose approved plan scope has MULTIPLE files, whose routed
// concern is PROSE that scrapes to NO repo-relative path, AND with an approved
// mid-stage scope amendment for one out-of-plan file. Under the old #1162
// narrowing the concern scrape was empty, the amendment made the narrowed set
// non-empty (= just the amendment target), the empty-narrow fail-safe never
// fired, and the entire approved plan scope was dropped — collapsing the
// effective scope to ~1 file and drift-excluding the agent's in-plan edits. The
// fix retains the FULL plan scope: the effective scope must equal the full plan
// scope UNION the amendment target (plus coupled siblings), NOT collapse to just
// the amendment.
func TestGetStagePrompt_Implement_FixupConcerns_ProseConcernPlusAmendment(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()
	sa := newFakeScopeAmendmentRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	// A multi-file plan scope of *_test.go files so the #1214 sibling fold derives
	// nothing new — keeps the count assertion focused on retention-vs-collapse.
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "multi-file plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/a_test.go", Operation: plan.FileOpModify},
				{Path: "backend/internal/server/b_test.go", Operation: plan.FileOpModify},
				{Path: "backend/internal/server/c_test.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	// Prose concern that names NO repo-relative path (no slash+extension token).
	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "correctness",
			Note: "the bound is off; reconsider how the loop terminates"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {makeFixupEntry(runID, implStageID, concerns)},
	}

	// An approved amendment for a file OUTSIDE the plan scope — the one file the
	// old narrowing would have collapsed the scope down to. It is also a
	// *_test.go file so it derives no extra sibling.
	a, err := sa.Create(context.Background(), scopeamendment.CreateParams{
		RunID: runID, StageID: implStageID,
		Paths:  []scopeamendment.PathEntry{{Path: "backend/internal/server/amended_test.go", Operation: scopeamendment.OperationModify}},
		Reason: "coupled seam discovered mid-pass",
	})
	if err != nil {
		t.Fatalf("create amendment: %v", err)
	}
	if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator",
	}); err != nil {
		t.Fatalf("approve amendment: %v", err)
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		SigningRepo:        sf,
		ArtifactRepo:       art,
		AuditRepo:          &feedbackAuditRepo{byRunID: auditByRun},
		ScopeAmendmentRepo: sa,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	paths := map[string]bool{}
	for _, f := range resp.ScopeFiles {
		paths[f.Path] = true
	}
	// The #1314 assertion: the effective scope must be the FULL plan scope UNION
	// the amendment target — NOT collapsed to just the amendment. Every approved
	// plan file must be present so the agent's in-plan edits would be staged
	// rather than drift-excluded.
	for _, want := range []string{
		"backend/internal/server/a_test.go",
		"backend/internal/server/b_test.go",
		"backend/internal/server/c_test.go",
	} {
		if !paths[want] {
			t.Errorf("approved-plan file %q dropped from scope_files (the #1314 collapse): %+v", want, resp.ScopeFiles)
		}
	}
	if !paths["backend/internal/server/amended_test.go"] {
		t.Errorf("approved amendment target missing from scope_files: %+v", resp.ScopeFiles)
	}
	if len(resp.ScopeFiles) != 4 {
		t.Errorf("scope_files must be the full plan scope (3) UNION the amendment (1) = 4 files, got %+v", resp.ScopeFiles)
	}
}

// TestGetStagePrompt_Implement_NoFixup_OmitsWireFlag is the negative case for
// #784: a normal implement dispatch with no stage_fixup_triggered audit entry
// must leave fixup=false and fixup_branch empty so the runner's default
// per-stage branch routing (checkout -b) stays unchanged.
func TestGetStagePrompt_Implement_NoFixup_OmitsWireFlag(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{}, // no fix-up entry
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Fixup {
		t.Errorf("fixup = true, want false for a normal (non-fix-up) implement dispatch")
	}
	if resp.FixupBranch != "" {
		t.Errorf("fixup_branch = %q, want empty for a normal implement dispatch", resp.FixupBranch)
	}
	if resp.FixupExpectedHeadSHA != "" {
		t.Errorf("fixup_expected_head_sha = %q, want empty for a normal implement dispatch", resp.FixupExpectedHeadSHA)
	}
	// A normal implement dispatch never reaches effectiveFixupScope (gated on the
	// fix-up branch), so scope_files is the FULL plan scope unchanged.
	if len(resp.ScopeFiles) != 1 || resp.ScopeFiles[0].Path != "backend/internal/server/prompt.go" {
		t.Errorf("non-fix-up dispatch must carry the full plan scope, got %+v", resp.ScopeFiles)
	}
}

// TestResolveFixupExpectedHeadSHA_ReadErrorOmitsField: the expected-head
// resolver is best-effort — a ListForRunByCategory failure on the
// reported-head ledger must WARN and return "" (the runner then skips the
// SHA comparison) rather than failing the dispatch (#967).
func TestResolveFixupExpectedHeadSHA_ReadErrorOmitsField(t *testing.T) {
	s := New(Config{
		Addr:      "127.0.0.1:0",
		AuditRepo: &feedbackAuditRepo{listErr: errors.New("audit store unavailable")},
	})
	if got := s.resolveFixupExpectedHeadSHA(context.Background(), uuid.New(), uuid.New()); got != "" {
		t.Errorf("resolveFixupExpectedHeadSHA = %q, want empty on a ledger read error", got)
	}
}

// TestResolveAcceptanceExpectedHeadSHA_ReadErrorOmitsField: the E31.18
// merge-candidate resolver shares the best-effort posture — a
// ListForRunByCategory failure must WARN and return "" (the runner's
// identity gate then degrades to unverifiable-warn) rather than failing
// the acceptance dispatch.
func TestResolveAcceptanceExpectedHeadSHA_ReadErrorOmitsField(t *testing.T) {
	s := New(Config{
		Addr:      "127.0.0.1:0",
		AuditRepo: &feedbackAuditRepo{listErr: errors.New("audit store unavailable")},
	})
	if got := s.resolveAcceptanceExpectedHeadSHA(context.Background(), uuid.New(), uuid.New()); got != "" {
		t.Errorf("resolveAcceptanceExpectedHeadSHA = %q, want empty on a ledger read error", got)
	}
}

// TestGetStagePrompt_Implement_FixupDecomposedChild_SliceBranch covers the
// decomposed-child fix-up branch form after ADR-041 (#1246): a child's work
// lives on its per-slice sole-writer branch
// fishhawk/run-<shortID(parentRunID)>/slice-<n>, so a fix-up on such a child
// must derive that slice branch — NOT the pre-ADR-041 bare parent prefix
// fishhawk/run-<parent> (orphaned from both the slice work and the #1243
// consolidated head, and path-nesting with the slice refs). It covers BOTH
// derivation branches: a non-nil SliceIndex (here 2) routes onto slice-2, and
// a nil SliceIndex falls back to slice-0 (matching the runner's slice-0
// default).
func TestGetStagePrompt_Implement_FixupDecomposedChild_SliceBranch(t *testing.T) {
	// A decomposed child fix-up lands on its per-slice sole-writer branch
	// fishhawk/run-<parent>/slice-<n>. Post-#1721 the child resolves its slice
	// via the persisted SliceIndex against the parent plan's decomposition, so
	// the fixture carries a matching decomposition (slice 2 scoped). A
	// nil-SliceIndex decomposed child can no longer link to a slice and fails
	// closed at scope resolution before the fix-up branch is derived; the branch
	// helper's nil->0 default is unit-tested directly in
	// TestFixupBranchFor_DecomposedChild.
	sliceIdx := 2
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	parentRunID := uuid.New()
	childRunID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
		Decomposition: &plan.Decomposition{
			Rationale: "scope split",
			SubPlans: []plan.SubPlanSummary{
				{Title: "Part A", ScopeHint: "A", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
				{Title: "Part B", ScopeHint: "B", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/b/b.go", Operation: plan.FileOpModify}}}},
				{Title: "Part C", ScopeHint: "C", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify}}}},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		childRunID: {
			{ID: planStageID, RunID: childRunID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: childRunID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[childRunID] = &run.Run{
		ID:             childRunID,
		Repo:           "o/r",
		WorkflowID:     "feature_change",
		DecomposedFrom: &parentRunID,
		SliceIndex:     &sliceIdx,
	}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: childRunID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "coverage", Note: "tighten the bound"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		childRunID: {makeFixupEntry(childRunID, implStageID, concerns)},
	}

	priv, _ := sf.issue(t, childRunID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, childRunID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Fixup {
		t.Fatalf("fixup = false, want true")
	}
	wantBranch := "fishhawk/run-" + parentRunID.String()[:8] + "/slice-2"
	if resp.FixupBranch != wantBranch {
		t.Errorf("fixup_branch = %q, want slice branch %q", resp.FixupBranch, wantBranch)
	}
}

// TestFixupBranchFor_DecomposedChild pins the decomposed-child fix-up branch
// helper directly. A SliceIndex-set child lands on slice-<n>; a nil SliceIndex
// defaults to slice-0 (matching the runner's slice-0 default). The nil->0 path
// is exercised here rather than through the prompt handler because a
// nil-SliceIndex decomposed child now fails closed at scope resolution (#1721)
// before the fix-up branch is ever derived.
func TestFixupBranchFor_DecomposedChild(t *testing.T) {
	parentRunID := uuid.New()
	stage := &run.Stage{ID: uuid.New()}
	prefix := "fishhawk/run-" + parentRunID.String()[:8]

	idx := 2
	if got := fixupBranchFor(&run.Run{ID: uuid.New(), DecomposedFrom: &parentRunID, SliceIndex: &idx}, stage); got != prefix+"/slice-2" {
		t.Errorf("SliceIndex=2: fixupBranchFor = %q, want %q", got, prefix+"/slice-2")
	}
	if got := fixupBranchFor(&run.Run{ID: uuid.New(), DecomposedFrom: &parentRunID}, stage); got != prefix+"/slice-0" {
		t.Errorf("SliceIndex=nil: fixupBranchFor = %q, want %q", got, prefix+"/slice-0")
	}
}

// TestGetStagePrompt_Implement_FixupDecomposedParent_SharedBranch covers the
// decomposed-PARENT fix-up branch form (#1063): a parent has DecomposedFrom ==
// nil, so the per-stage form would target fishhawk/run-<parent>/stage-<stage>,
// NOT the consolidated PR head. When the parent has minted children, the fix-up
// must land on the shared consolidated branch fishhawk/run-<shortID(parent)>.
func TestGetStagePrompt_Implement_FixupDecomposedParent_SharedBranch(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	parentRunID := uuid.New()
	childRunID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		parentRunID: {
			{ID: planStageID, RunID: parentRunID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: parentRunID, Type: run.StageTypeImplement},
		},
	}
	// Parent run: DecomposedFrom == nil (it's the parent), and a child row is
	// registered so hasDecomposedChildren probes true.
	rr.getRuns[parentRunID] = &run.Run{
		ID:         parentRunID,
		Repo:       "o/r",
		WorkflowID: "feature_change",
	}
	rr.getRuns[childRunID] = &run.Run{
		ID:             childRunID,
		Repo:           "o/r",
		WorkflowID:     "feature_change",
		DecomposedFrom: &parentRunID,
	}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: parentRunID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "coverage", Note: "tighten the bound"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		parentRunID: {makeFixupEntry(parentRunID, implStageID, concerns)},
	}

	priv, _ := sf.issue(t, parentRunID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, parentRunID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Fixup {
		t.Fatalf("fixup = false, want true")
	}
	// Cross-package byte-match pin: assert against the SAME exported source of
	// truth production uses (orchestrator.ConsolidatedBranch), not a local
	// literal. A stale literal here is exactly what masked the #1245
	// divergence; this fails if fixupBranchForRun ever desyncs from the
	// orchestrator's consolidated-branch derivation on a future rename.
	wantBranch := orchestrator.ConsolidatedBranch(parentRunID)
	if resp.FixupBranch != wantBranch {
		t.Errorf("fixup_branch = %q, want shared consolidated branch %q", resp.FixupBranch, wantBranch)
	}
}

// TestResolveFixupConcerns covers the audit-payload reader directly: no
// trigger entry, a wrong-stage entry, the happy path, and a malformed payload.
// The reader now returns only the rendered concern lines — the joined-notes
// return that fed the removed #1162 concern-scrape was dropped in #1314.
func TestResolveFixupConcerns(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	concerns := []planreview.Concern{
		{Severity: planreview.SeverityMedium, Category: "security", Note: "check authz"},
		{Severity: planreview.SeverityLow, Category: "scope", Note: "touch pkg/a/file.go"},
	}

	t.Run("no entries returns nil", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{}})
		rendered := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if rendered != nil {
			t.Errorf("got %v, want nil", rendered)
		}
	})

	t.Run("entry for a different stage is ignored", func(t *testing.T) {
		other := uuid.New()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntry(runID, other, concerns)}},
		}})
		rendered := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if rendered != nil {
			t.Errorf("got %v, want nil (entry bound to a different stage)", rendered)
		}
	})

	t.Run("happy path renders concern lines as trusted FixupConcerns", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntry(runID, stageID, concerns)}},
		}})
		rendered := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if len(rendered) != 2 {
			t.Fatalf("rendered len = %d, want 2: %v", len(rendered), rendered)
		}
		if rendered[0].Text != "[medium/security] check authz" {
			t.Errorf("rendered[0].Text = %q", rendered[0].Text)
		}
		if rendered[1].Text != "[low/scope] touch pkg/a/file.go" {
			t.Errorf("rendered[1].Text = %q", rendered[1].Text)
		}
		// Concerns without Provenance (operator/reviewer-authored) stay trusted.
		for i, c := range rendered {
			if c.AcceptanceDerived {
				t.Errorf("rendered[%d].AcceptanceDerived = true, want false (no provenance marker)", i)
			}
		}
	})

	t.Run("acceptance-provenance concern decodes as AcceptanceDerived", func(t *testing.T) {
		// This is the synthesize -> stage_fixup_triggered audit payload ->
		// resolveFixupConcerns seam (ADR-050 / E31.8 / #1613): a persisted concern
		// carrying Provenance=acceptance must decode to AcceptanceDerived=true so
		// the prompt renderer quarantines it, while a sibling without the marker
		// stays trusted. Per-layer units miss this JSON round-trip.
		mixed := []planreview.Concern{
			{Severity: planreview.SeverityHigh, Category: "acceptance", Note: "criterion c1 failed", Provenance: planreview.ConcernProvenanceAcceptance},
			{Severity: planreview.SeverityMedium, Category: "scope", Note: "operator concern"},
		}
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntry(runID, stageID, mixed)}},
		}})
		rendered := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if len(rendered) != 2 {
			t.Fatalf("rendered len = %d, want 2: %v", len(rendered), rendered)
		}
		if !rendered[0].AcceptanceDerived {
			t.Errorf("rendered[0].AcceptanceDerived = false, want true (Provenance=acceptance)")
		}
		if rendered[1].AcceptanceDerived {
			t.Errorf("rendered[1].AcceptanceDerived = true, want false (no provenance marker)")
		}
	})

	t.Run("malformed payload is skipped", func(t *testing.T) {
		rid := runID
		sid := stageID
		bad := &audit.Entry{ID: uuid.New(), Category: CategoryStageFixupTriggered, RunID: &rid, StageID: &sid, Payload: []byte("{not json")}
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {bad}},
		}})
		rendered := s.resolveFixupConcerns(context.Background(), runID, stageID)
		if rendered != nil {
			t.Errorf("got %v, want nil (malformed payload)", rendered)
		}
	})
}

// makeFixupEntryWithAllowCreate builds a stage_fixup_triggered audit entry
// carrying both the selected concerns and the declared allow_create paths
// (#823), matching server/fixup.go's writeFixupAudit payload shape.
func makeFixupEntryWithAllowCreate(runID, stageID uuid.UUID, concerns []planreview.Concern, allowCreate []string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"stage_id":         stageID.String(),
		"selected_indices": []int{0},
		"concerns":         concerns,
		"allow_create":     allowCreate,
		"reason":           "operator declared a net-new file",
		"pass_ordinal":     1,
	})
	rid := runID
	sid := stageID
	return &audit.Entry{ID: uuid.New(), Category: CategoryStageFixupTriggered, RunID: &rid, StageID: &sid, Payload: payload}
}

func TestResolveFixupAllowCreate(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	concerns := []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "scope", Note: "needs a new file"}}

	t.Run("no entries returns nil", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{}})
		if got := s.resolveFixupAllowCreate(context.Background(), runID, stageID); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("entry for a different stage is ignored", func(t *testing.T) {
		other := uuid.New()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntryWithAllowCreate(runID, other, concerns, []string{"a/b.go"})}},
		}})
		if got := s.resolveFixupAllowCreate(context.Background(), runID, stageID); got != nil {
			t.Errorf("got %v, want nil (entry bound to a different stage)", got)
		}
	})

	t.Run("returns the newest entry's declared paths", func(t *testing.T) {
		old := makeFixupEntryWithAllowCreate(runID, stageID, concerns, []string{"old/path.go"})
		newest := makeFixupEntryWithAllowCreate(runID, stageID, concerns, []string{"new/a.go", "new/b.go"})
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {old, newest}},
		}})
		got := s.resolveFixupAllowCreate(context.Background(), runID, stageID)
		if len(got) != 2 || got[0] != "new/a.go" || got[1] != "new/b.go" {
			t.Errorf("got %v, want [new/a.go new/b.go] (newest entry)", got)
		}
	})

	t.Run("entry without allow_create returns nil", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeFixupEntry(runID, stageID, concerns)}},
		}})
		if got := s.resolveFixupAllowCreate(context.Background(), runID, stageID); got != nil {
			t.Errorf("got %v, want nil (no allow_create on the entry)", got)
		}
	})
}

// priorDiffTraceStore is a configurable tracestore.Storage for the #1163 fix-up
// prior-diff tests: Get returns the configured body (or getErr). Put/Stat/List
// are unused.
type priorDiffTraceStore struct {
	body   []byte
	getErr error
}

func (s *priorDiffTraceStore) Put(context.Context, tracestore.BundleRef, io.Reader) error { return nil }
func (s *priorDiffTraceStore) Get(_ context.Context, _ tracestore.BundleRef) (io.ReadCloser, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return io.NopCloser(bytes.NewReader(s.body)), nil
}
func (s *priorDiffTraceStore) Stat(context.Context, tracestore.BundleRef) (tracestore.Stat, error) {
	return tracestore.Stat{}, errors.New("priorDiffTraceStore: Stat not used")
}
func (s *priorDiffTraceStore) List(context.Context, uuid.UUID) ([]tracestore.BundleRef, error) {
	return nil, errors.New("priorDiffTraceStore: List not used")
}

// makeRedactedDiffBundle builds a gzipped JSONL trace bundle carrying a single
// git_diff event with the given patch + changed files — the shape
// bundle.ExtractDiff parses. The bytes are pre-redacted repo code only (no issue
// text), mirroring the runner's redacted variant.
func makeRedactedDiffBundle(t *testing.T, patch string, files []map[string]string) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	diffData, err := json.Marshal(map[string]any{
		"kind": "git_diff", "base_ref": "main", "files": files, "num_files": len(files), "patch": patch,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []line{
		{Seq: 1, Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, Kind: bundle.EventKindGitDiff, Data: diffData},
	}
	var raw bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// TestGetStagePrompt_Implement_FixupPriorDiff_Rendered is the cross-boundary
// end-to-end test for #1163: it seeds a TraceStore + a trace_uploaded audit
// entry pointing at a real redacted bundle whose git_diff event carries a known
// patch, triggers a fix-up (a stage_fixup_triggered concern), and asserts the
// rendered prompt contains the seeded diff hunks under the change-under-amendment
// section. This crosses the trace-store -> resolver -> Trigger field ->
// prompt-render seam end to end, not just per-layer units.
func TestGetStagePrompt_Implement_FixupPriorDiff_Rendered(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "correctness",
			Note: "fix the nil deref in backend/internal/server/prompt.go"},
	}
	hash := strings.Repeat("a", 64)
	const patch = "diff --git a/backend/internal/server/prompt.go b/backend/internal/server/prompt.go\n" +
		"@@ -1,2 +1,3 @@\n+SENTINEL_HUNK_LINE\n"
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {
			makeFixupEntry(runID, implStageID, concerns),
			makeTraceUploadedEntry(t, 1, runID, implStageID, "redacted", hash),
		},
	}

	ts := &priorDiffTraceStore{
		body: makeRedactedDiffBundle(t, patch, []map[string]string{
			{"path": "backend/internal/server/prompt.go", "status": "modified"},
		}),
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
		TraceStore:   ts,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, want := range []string{
		"### The change you are amending",
		"```diff",
		"SENTINEL_HUNK_LINE",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
}

// TestResolveFixupPriorDiff_NoTrace_ReturnsEmpty: no redacted trace for the
// stage → ("", "") and the bundle Get is never reached.
func TestResolveFixupPriorDiff_NoTrace_ReturnsEmpty(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	s := New(Config{
		Addr:       "127.0.0.1:0",
		AuditRepo:  &feedbackAuditRepo{}, // no trace_uploaded entries
		TraceStore: &priorDiffTraceStore{getErr: errors.New("must not be called")},
	})
	patch, fileList := s.resolveFixupPriorDiff(context.Background(), runID, stageID)
	if patch != "" || fileList != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\")", patch, fileList)
	}
}

// TestResolveFixupPriorDiff_TraceStoreError_ReturnsEmpty: a redacted trace
// exists but TraceStore.Get errors → ("", "") (best-effort WARN-and-proceed).
func TestResolveFixupPriorDiff_TraceStoreError_ReturnsEmpty(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	hash := strings.Repeat("a", 64)
	s := New(Config{
		Addr: "127.0.0.1:0",
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {makeTraceUploadedEntry(t, 1, runID, stageID, "redacted", hash)},
		}},
		TraceStore: &priorDiffTraceStore{getErr: errors.New("storage down")},
	})
	patch, fileList := s.resolveFixupPriorDiff(context.Background(), runID, stageID)
	if patch != "" || fileList != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\") on Get error", patch, fileList)
	}
}

// TestResolveFixupPriorDiff_Unconfigured_ReturnsEmpty: a nil AuditRepo or nil
// TraceStore short-circuits to ("", "").
func TestResolveFixupPriorDiff_Unconfigured_ReturnsEmpty(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	t.Run("nil AuditRepo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", TraceStore: &priorDiffTraceStore{}})
		patch, fileList := s.resolveFixupPriorDiff(context.Background(), runID, stageID)
		if patch != "" || fileList != "" {
			t.Errorf("got (%q, %q), want (\"\", \"\")", patch, fileList)
		}
	})

	t.Run("nil TraceStore", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{}})
		patch, fileList := s.resolveFixupPriorDiff(context.Background(), runID, stageID)
		if patch != "" || fileList != "" {
			t.Errorf("got (%q, %q), want (\"\", \"\")", patch, fileList)
		}
	})
}

// TestResolveFixupPriorDiff_AuditListError_ReturnsEmpty: a ListForRunByCategory
// error degrades to ("", "") (best-effort WARN-and-proceed).
func TestResolveFixupPriorDiff_AuditListError_ReturnsEmpty(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	s := New(Config{
		Addr:       "127.0.0.1:0",
		AuditRepo:  &feedbackAuditRepo{listErr: errors.New("audit down")},
		TraceStore: &priorDiffTraceStore{getErr: errors.New("must not be called")},
	})
	patch, fileList := s.resolveFixupPriorDiff(context.Background(), runID, stageID)
	if patch != "" || fileList != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\") on audit list error", patch, fileList)
	}
}

// TestResolveFixupPriorDiff_NoDiffEvent_ReturnsEmpty: a redacted bundle that
// parses but carries no git_diff event (bundle.ExtractDiff → ErrNoDiffEvent)
// degrades to ("", "").
func TestResolveFixupPriorDiff_NoDiffEvent_ReturnsEmpty(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	hash := strings.Repeat("a", 64)
	// A bundle with only a manifest line — no git_diff event.
	ts := &priorDiffTraceStore{body: makeRedactedDiffBundleManifestOnly(t)}
	s := New(Config{
		Addr: "127.0.0.1:0",
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {makeTraceUploadedEntry(t, 1, runID, stageID, "redacted", hash)},
		}},
		TraceStore: ts,
	})
	patch, fileList := s.resolveFixupPriorDiff(context.Background(), runID, stageID)
	if patch != "" || fileList != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\") on ErrNoDiffEvent", patch, fileList)
	}
}

// makeRedactedDiffBundleManifestOnly builds a gzipped JSONL bundle with a
// manifest line but no git_diff event, so bundle.ExtractDiff returns
// ErrNoDiffEvent.
func makeRedactedDiffBundleManifestOnly(t *testing.T) []byte {
	t.Helper()
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{"seq": 1, "kind": bundle.EventKindManifest, "data": json.RawMessage(mdata)})
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// TestResolveFixupPriorDiff_HappyPath_ReturnsPatchAndFileList: a redacted bundle
// with a git_diff event yields the patch and the rendered changed-file list.
func TestResolveFixupPriorDiff_HappyPath_ReturnsPatchAndFileList(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	hash := strings.Repeat("a", 64)
	const patch = "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n+changed\n"
	ts := &priorDiffTraceStore{
		body: makeRedactedDiffBundle(t, patch, []map[string]string{
			{"path": "x.go", "status": "modified"},
		}),
	}
	s := New(Config{
		Addr: "127.0.0.1:0",
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {makeTraceUploadedEntry(t, 1, runID, stageID, "redacted", hash)},
		}},
		TraceStore: ts,
	})
	gotPatch, gotFiles := s.resolveFixupPriorDiff(context.Background(), runID, stageID)
	if gotPatch != patch {
		t.Errorf("patch = %q, want %q", gotPatch, patch)
	}
	if !strings.Contains(gotFiles, "x.go") {
		t.Errorf("file list = %q, want it to contain %q", gotFiles, "x.go")
	}
}

// TestGetStagePrompt_Implement_FixupAllowCreate_Folded confirms an operator-
// declared net-new file (allow_create, #823) is folded into the effective
// scope.files — the exact set the runner's #818 created-out-of-scope gate diffs
// against — on TOP of the retained full plan scope (#1314, mode 4). The plan
// file is retained, the allow_create file is present, and an undeclared file is
// still absent so the #818 silent-strip hole stays closed.
func TestGetStagePrompt_Implement_FixupAllowCreate_Folded(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	concerns := []planreview.Concern{
		{Severity: planreview.SeverityMedium, Category: "scope", Note: "extract the helper into a new file"},
	}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {makeFixupEntryWithAllowCreate(runID, implStageID, concerns, []string{"backend/internal/server/helper.go"})},
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	paths := map[string]bool{}
	for _, f := range resp.ScopeFiles {
		paths[f.Path] = true
	}
	// The declared net-new file is folded into the effective scope (allow_create
	// surface).
	if !paths["backend/internal/server/helper.go"] {
		t.Errorf("allow_create file missing from scope_files: %+v", resp.ScopeFiles)
	}
	// The full plan scope is RETAINED (#1314): the plan file is present on TOP of
	// the allow_create fold.
	if !paths["backend/internal/server/prompt.go"] {
		t.Errorf("approved-plan file missing from scope_files (full scope must be retained): %+v", resp.ScopeFiles)
	}
	// An undeclared path is NOT in the effective scope — the #818 gate would
	// still trip for it (the silent-strip hole stays closed).
	if paths["backend/internal/server/undeclared.go"] {
		t.Errorf("undeclared path leaked into scope_files: %+v", resp.ScopeFiles)
	}
}

// TestGetStagePrompt_Implement_NoScopeFilesWhenPlanMissing confirms the
// scope_files field is omitted when no approved plan is available, so
// the runner falls back to `git add -A`.
func TestGetStagePrompt_Implement_NoScopeFilesWhenPlanMissing(t *testing.T) {
	s, rr, sf, _ := newPromptServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	priv, _ := sf.issue(t, runID)
	rr.runRow = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement}

	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ScopeFiles != nil {
		t.Errorf("scope_files = %+v, want nil/omitted when plan missing", resp.ScopeFiles)
	}
}

// TestGetStagePrompt_DecomposedChild_ScopeConstraintInjected verifies that
// when a child run has DecomposedFrom set and a matching IssueContext, the
// implement-stage prompt contains a SCOPE CONSTRAINT block with this child's
// scope_hint and the sibling's scope_hint (#541).
func TestGetStagePrompt_DecomposedChild_ScopeConstraintInjected(t *testing.T) {
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	parentRunID := uuid.New()
	childRunID := uuid.New()
	parentPlanStageID := uuid.New()
	childStageID := uuid.New()

	// Build a standard_v1 plan artifact with a two-sub-plan decomposition.
	parentPlan := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "parent plan",
		Verification: plan.Verification{
			TestStrategy: "ts",
			RollbackPlan: "rb",
		},
		Decomposition: &plan.Decomposition{
			Rationale: "scope split",
			SubPlans: []plan.SubPlanSummary{
				{Title: "Part A title", ScopeHint: "Implement Part A in pkg/a.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
				{Title: "Part B title", ScopeHint: "Implement Part B in pkg/b.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/b/b.go", Operation: plan.FileOpModify}}}},
			},
		},
	}
	planBytes, err := json.Marshal(parentPlan)
	if err != nil {
		t.Fatalf("marshal parent plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       parentPlanStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	// Seed parent run with a plan stage.
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		parentRunID: {
			{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan},
		},
	}
	rr.getRuns[parentRunID] = &run.Run{
		ID:   parentRunID,
		Repo: "o/r",
	}

	// Child run: DecomposedFrom=parentRunID, linked to Part A by SliceIndex=0
	// (the durable linkage, #1721) — the IssueContext.Body header is no longer
	// what binds the child to its sub-plan.
	sliceIdx := 0
	childBody := "## Part A title\n\nImplement Part A in pkg/a.\n\n---\n*Decomposed sub-plan.*"
	rr.getRuns[childRunID] = &run.Run{
		ID:             childRunID,
		Repo:           "o/r",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerCLI,
		ParentRunID:    &parentRunID,
		DecomposedFrom: &parentRunID,
		SliceIndex:     &sliceIdx,
		IssueContext: &run.IssueContext{
			Title: "Part A title",
			Body:  childBody,
		},
	}
	rr.getStages[childStageID] = &run.Stage{
		ID:    childStageID,
		RunID: childRunID,
		Type:  run.StageTypeImplement,
	}

	priv, _ := sf.issue(t, childRunID)

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, childRunID, childStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, want := range []string{
		"SCOPE CONSTRAINT",
		"Implement Part A in pkg/a.",
		"Implement Part B in pkg/b.",
		"do NOT modify code in sibling scope",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
}

// TestGetStagePrompt_DecomposedChild_ScopeFiles is the cross-boundary seam
// test for #676: it threads a per-sub-plan scope.files slice through schema
// validation -> backend plan domain type -> the prompt-response wire payload
// (the field the runner's scope_handoff/scope_drift consumer reads), and
// asserts the child's prompt response carries the MATCHED sub-plan's slice,
// not the parent's full union. The fail-loud subtest asserts a sub-plan
// without scope now returns 409 decomposed_scope_unresolved rather than
// silently inheriting the parent's full scope.files (#1721).
func TestGetStagePrompt_DecomposedChild_ScopeFiles(t *testing.T) {
	// Parent decomposed plan: full scope is the union (a.go, b.go), but each
	// sub-plan carries its own narrower slice. Part B intentionally omits
	// scope to exercise the fail-loud path.
	newParentPlan := func() *plan.Plan {
		return &plan.Plan{
			PlanVersion: "standard_v1",
			Summary:     "parent plan",
			Scope: plan.Scope{
				Files: []plan.ScopeFile{
					{Path: "pkg/a/a.go", Operation: plan.FileOpCreate},
					{Path: "pkg/b/b.go", Operation: plan.FileOpModify},
				},
			},
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Decomposition: &plan.Decomposition{
				Rationale: "scope split",
				SubPlans: []plan.SubPlanSummary{
					{
						Title:     "Part A title",
						ScopeHint: "Implement Part A in pkg/a.",
						Scope: &plan.Scope{
							Files: []plan.ScopeFile{
								{Path: "pkg/a/a.go", Operation: plan.FileOpCreate},
							},
						},
					},
					{
						Title:     "Part B title",
						ScopeHint: "Implement Part B in pkg/b.",
						// No Scope — must fall back to the parent's full scope.
					},
				},
			},
		}
	}

	// seedChildPrompt seeds a parent plan artifact + a decomposed child run
	// linked to sub-plan sliceIdx (the durable SliceIndex linkage, #1721),
	// then fetches the child's implement-stage prompt response recorder.
	seedChildPrompt := func(t *testing.T, sliceIdx int) *httptest.ResponseRecorder {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		parentRunID := uuid.New()
		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()

		planBytes, err := json.Marshal(newParentPlan())
		if err != nil {
			t.Fatalf("marshal parent plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       parentPlanStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			parentRunID: {
				{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan},
			},
		}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}

		idx := sliceIdx
		rr.getRuns[childRunID] = &run.Run{
			ID:             childRunID,
			Repo:           "o/r",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerCLI,
			ParentRunID:    &parentRunID,
			DecomposedFrom: &parentRunID,
			SliceIndex:     &idx,
		}
		rr.getStages[childStageID] = &run.Stage{
			ID:    childStageID,
			RunID: childRunID,
			Type:  run.StageTypeImplement,
		}

		priv, _ := sf.issue(t, childRunID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		return promptRequest(t, s, childRunID, childStageID, priv, "")
	}

	scopePaths := func(sfs []scopeFile) []string {
		out := make([]string, 0, len(sfs))
		for _, f := range sfs {
			out = append(out, f.Path)
		}
		return out
	}

	t.Run("sub-plan scope auto-includes coupled _test.go", func(t *testing.T) {
		// Part A's slice is {pkg/a/a.go (create)}; the fold (#1083) must add
		// pkg/a/a_test.go so the coupled unit tests are in-scope for the slice
		// that owns the code, while still excluding the parent union's pkg/b.
		w := seedChildPrompt(t, 0)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := scopePaths(resp.ScopeFiles)
		want := []string{"pkg/a/a.go", "pkg/a/a_test.go"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scope_files = %v, want the sub-plan's slice plus its coupled test sibling %v (NOT the parent union)", got, want)
		}
	})

	t.Run("sub-plan without scope fails closed with 409", func(t *testing.T) {
		// Part B (slice 1) carries no scope. Under #1721 the child no longer
		// inherits the parent's full scope — it fails closed at the dispatch
		// surface rather than fanning out an unscoped child.
		w := seedChildPrompt(t, 1)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertDecomposedScopeUnresolved(t, w, 1)
	})
}

// assertDecomposedScopeUnresolved asserts a prompt-endpoint recorder carries a
// 409 decomposed_scope_unresolved error naming a non-empty run_id, the given
// slice_index, and a parent_run_id detail (#1721).
func assertDecomposedScopeUnresolved(t *testing.T, w *httptest.ResponseRecorder, wantSlice int) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
	}
	if body.Error.Code != "decomposed_scope_unresolved" {
		t.Errorf("error.code = %q, want decomposed_scope_unresolved\n%s", body.Error.Code, w.Body.String())
	}
	if rid, _ := body.Error.Details["run_id"].(string); rid == "" {
		t.Errorf("error.details.run_id empty, want the child run id\n%s", w.Body.String())
	}
	if _, ok := body.Error.Details["parent_run_id"]; !ok {
		t.Errorf("error.details missing parent_run_id\n%s", w.Body.String())
	}
	// JSON numbers decode to float64.
	if got, _ := body.Error.Details["slice_index"].(float64); int(got) != wantSlice {
		t.Errorf("error.details.slice_index = %v, want %d\n%s", body.Error.Details["slice_index"], wantSlice, w.Body.String())
	}
}

// TestGetStagePrompt_DecomposedScope_FailLoud is the #1721 fail-loud guard: a
// decomposed child whose slice scope cannot be resolved returns 409
// decomposed_scope_unresolved at BOTH prompt endpoints rather than silently
// inheriting the parent's full scope. It also pins the campaign-minted shape
// (nil IssueContext, SliceIndex set) narrowing correctly, and a non-decomposed
// control keeping full scope with no 409.
func TestGetStagePrompt_DecomposedScope_FailLoud(t *testing.T) {
	// Parent plan: two scoped sub-plans (union a.go, b.go) plus a top-level
	// scope for the non-decomposed control.
	newParentPlan := func() *plan.Plan {
		return &plan.Plan{
			PlanVersion: "standard_v1",
			Summary:     "parent plan",
			Scope: plan.Scope{
				Files: []plan.ScopeFile{
					{Path: "pkg/a/a.go", Operation: plan.FileOpModify},
					{Path: "pkg/b/b.go", Operation: plan.FileOpModify},
				},
			},
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Decomposition: &plan.Decomposition{
				Rationale: "scope split",
				SubPlans: []plan.SubPlanSummary{
					{Title: "Part A title", ScopeHint: "Implement Part A.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
					{Title: "Part B title", ScopeHint: "Implement Part B.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/b/b.go", Operation: plan.FileOpModify}}}},
				},
			},
		}
	}

	// seed wires a parent plan artifact + a child run whose shape the caller
	// mutates, and returns the server + child stage id for hitting either
	// prompt endpoint. childMut receives the child run row and its parent id.
	seed := func(t *testing.T, childMut func(child *run.Run, parentRunID uuid.UUID)) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		parentRunID := uuid.New()
		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()

		planBytes, err := json.Marshal(newParentPlan())
		if err != nil {
			t.Fatalf("marshal parent plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       parentPlanStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			parentRunID: {{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan}},
		}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}

		child := &run.Run{
			ID:            childRunID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
			// ParentRunID lets loadApprovedPlanForRun walk up to the parent's
			// decomposed plan artifact (real fan-out children set both this and
			// DecomposedFrom). childMut supplies DecomposedFrom + SliceIndex.
			ParentRunID: &parentRunID,
		}
		childMut(child, parentRunID)
		rr.getRuns[childRunID] = child
		rr.getStages[childStageID] = &run.Stage{ID: childStageID, RunID: childRunID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, childRunID)
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: sf, ArtifactRepo: art})
		s.promptIssueGetterOverride = &stubIssueGetter{}
		return s, childRunID, childStageID, priv
	}

	scopePaths := func(sfs []scopeFile) []string {
		out := make([]string, 0, len(sfs))
		for _, f := range sfs {
			out = append(out, f.Path)
		}
		return out
	}

	t.Run("campaign-minted shape (nil IssueContext) narrows to its slice", func(t *testing.T) {
		// This is the exact regression shape (#1721): DecomposedFrom set,
		// SliceIndex set, IssueContext nil — which previously fell through to
		// the parent's full scope.
		s, _, childStageID, priv := seed(t, func(child *run.Run, parentRunID uuid.UUID) {
			idx := 0
			child.DecomposedFrom = &parentRunID
			child.SliceIndex = &idx
			child.IssueContext = nil
		})
		w := promptRequest(t, s, uuid.Nil, childStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := scopePaths(resp.ScopeFiles)
		want := []string{"pkg/a/a.go", "pkg/a/a_test.go"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scope_files = %v, want the slice-0 narrowing %v (NOT the parent union)", got, want)
		}
	})

	t.Run("out-of-range SliceIndex fails closed on /prompt", func(t *testing.T) {
		s, _, childStageID, priv := seed(t, func(child *run.Run, parentRunID uuid.UUID) {
			idx := 7 // only 2 sub-plans exist
			child.DecomposedFrom = &parentRunID
			child.SliceIndex = &idx
		})
		w := promptRequest(t, s, uuid.Nil, childStageID, priv, "")
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertDecomposedScopeUnresolved(t, w, 7)
	})

	t.Run("out-of-range SliceIndex fails closed on /prompt-render", func(t *testing.T) {
		s, _, childStageID, _ := seed(t, func(child *run.Run, parentRunID uuid.UUID) {
			idx := 7
			child.DecomposedFrom = &parentRunID
			child.SliceIndex = &idx
		})
		w := promptRenderRequest(t, s, childStageID)
		if w.Code != http.StatusConflict {
			t.Fatalf("render status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertDecomposedScopeUnresolved(t, w, 7)
	})

	t.Run("nil-scope slice fails closed on /prompt-render", func(t *testing.T) {
		// Slice 1 (Part B) is scoped in newParentPlan, so build a bespoke
		// parent whose slice-1 carries nil Scope to prove the render surface
		// also fails closed on an empty-scope match.
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()
		parentRunID := uuid.New()
		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()
		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "parent plan",
			Scope:        plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}},
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Decomposition: &plan.Decomposition{
				Rationale: "scope split",
				SubPlans: []plan.SubPlanSummary{
					{Title: "Part A title", ScopeHint: "A", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
					{Title: "Part B title", ScopeHint: "B"}, // nil Scope
				},
			},
		}
		planBytes, _ := json.Marshal(p)
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{StageID: parentPlanStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{parentRunID: {{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan}}}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}
		idx := 1
		rr.getRuns[childRunID] = &run.Run{ID: childRunID, Repo: "o/r", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI, ParentRunID: &parentRunID, DecomposedFrom: &parentRunID, SliceIndex: &idx}
		rr.getStages[childStageID] = &run.Stage{ID: childStageID, RunID: childRunID, Type: run.StageTypeImplement}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: sf, ArtifactRepo: art})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRenderRequest(t, s, childStageID)
		if w.Code != http.StatusConflict {
			t.Fatalf("render status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertDecomposedScopeUnresolved(t, w, 1)
	})

	t.Run("decomposed child of a decomposition-less plan fails closed at both endpoints", func(t *testing.T) {
		// A decomposed child (DecomposedFrom + SliceIndex set) whose loadable
		// parent plan carries NO decomposition. matchDecomposedSubPlan returns
		// (nil, -1) for the nil-decomposition plan, so the guard must fail closed
		// rather than fall through to the plan's top-level scope — the silent
		// full-scope fallback the approval condition required gone (#1721).
		seedNoDecomp := func(t *testing.T) (*Server, uuid.UUID, ed25519.PrivateKey) {
			t.Helper()
			rr := newPromptRunRepo()
			sf := newSigningFake()
			art := newFakeArtifactRepo()
			parentRunID := uuid.New()
			childRunID := uuid.New()
			parentPlanStageID := uuid.New()
			childStageID := uuid.New()
			// Parent plan with a top-level scope but NO Decomposition block.
			p := &plan.Plan{
				PlanVersion:  "standard_v1",
				Summary:      "parent plan without decomposition",
				Scope:        plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}, {Path: "pkg/b/b.go", Operation: plan.FileOpModify}}},
				Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			}
			planBytes, _ := json.Marshal(p)
			sv := "standard_v1"
			if _, err := art.Create(context.Background(), artifact.CreateParams{StageID: parentPlanStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes}); err != nil {
				t.Fatalf("seed plan artifact: %v", err)
			}
			rr.stagesByRunID = map[uuid.UUID][]*run.Stage{parentRunID: {{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan}}}
			rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}
			idx := 0
			rr.getRuns[childRunID] = &run.Run{ID: childRunID, Repo: "o/r", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI, ParentRunID: &parentRunID, DecomposedFrom: &parentRunID, SliceIndex: &idx}
			rr.getStages[childStageID] = &run.Stage{ID: childStageID, RunID: childRunID, Type: run.StageTypeImplement}
			priv, _ := sf.issue(t, childRunID)
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: sf, ArtifactRepo: art})
			s.promptIssueGetterOverride = &stubIssueGetter{}
			return s, childStageID, priv
		}

		s, childStageID, priv := seedNoDecomp(t)
		w := promptRequest(t, s, uuid.Nil, childStageID, priv, "")
		if w.Code != http.StatusConflict {
			t.Fatalf("/prompt status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertDecomposedScopeUnresolved(t, w, 0)

		s2, childStageID2, _ := seedNoDecomp(t)
		wr := promptRenderRequest(t, s2, childStageID2)
		if wr.Code != http.StatusConflict {
			t.Fatalf("/prompt-render status = %d, want 409:\n%s", wr.Code, wr.Body.String())
		}
		assertDecomposedScopeUnresolved(t, wr, 0)
	})

	t.Run("nil-SliceIndex child of a decomposed plan fails closed at both endpoints", func(t *testing.T) {
		// A decomposed child (DecomposedFrom set) with NO persisted SliceIndex
		// that resolves — via the parent walk — to a plan carrying a
		// Decomposition is a fan-out child that lost its slice link:
		// matchDecomposedSubPlan treats a nil index as unlinked, so the guard
		// fails closed (slice_index -1) rather than falling through to the
		// parent's full top-level scope — the silent full-scope fallback the
		// approval condition required gone, not merely bypassed (#1721). The
		// orchestrator stamps SliceIndex on every real fan-out child, so this
		// shape is unreachable in production, but the handler must fail closed if
		// it ever appears rather than reopen the #1669 full-scope binding. (A
		// nil-SliceIndex child whose resolved plan has NO decomposition is a
		// standalone child with its own top-level plan — see seed's ParentRunID
		// walk lands on a decomposed plan here — and keeps that scope instead.)
		s, _, childStageID, priv := seed(t, func(child *run.Run, parentRunID uuid.UUID) {
			child.DecomposedFrom = &parentRunID
			// SliceIndex intentionally left nil.
		})
		w := promptRequest(t, s, uuid.Nil, childStageID, priv, "")
		if w.Code != http.StatusConflict {
			t.Fatalf("/prompt status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertDecomposedScopeUnresolved(t, w, -1)

		s2, _, childStageID2, _ := seed(t, func(child *run.Run, parentRunID uuid.UUID) {
			child.DecomposedFrom = &parentRunID
		})
		wr := promptRenderRequest(t, s2, childStageID2)
		if wr.Code != http.StatusConflict {
			t.Fatalf("/prompt-render status = %d, want 409:\n%s", wr.Code, wr.Body.String())
		}
		assertDecomposedScopeUnresolved(t, wr, -1)
	})

	t.Run("non-decomposed control keeps full scope, no 409", func(t *testing.T) {
		s, _, childStageID, priv := seed(t, func(child *run.Run, _ uuid.UUID) {
			// DecomposedFrom nil → ordinary run; never touches the require path.
		})
		w := promptRequest(t, s, uuid.Nil, childStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := scopePaths(resp.ScopeFiles)
		want := []string{"pkg/a/a.go", "pkg/b/b.go"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scope_files = %v, want the parent's full scope %v", got, want)
		}
	})
}

// TestResolveDecomposedScope_DisjointSlices is the #1669 server-resolution
// confinement guard: given a decomposed parent plan whose sub-plans declare
// disjoint per-slice scope.files, each child's resolveDecomposedScopeFiles
// narrows to exactly its slice (plus the coupled _test.go sibling), the
// per-child narrowed sets are pairwise DISJOINT, the declared slice files
// union back to the parent scope, and resolveDecomposedScopeConstraint carries
// the same slice files. This is the primary regression that each fan-out child
// is confined to its slice — not the whole plan.
func TestResolveDecomposedScope_DisjointSlices(t *testing.T) {
	parentRunID := uuid.New()
	parentPlan := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "parent plan",
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "pkg/a/a.go", Operation: plan.FileOpModify},
				{Path: "pkg/b/b.go", Operation: plan.FileOpModify},
				{Path: "pkg/c/c.go", Operation: plan.FileOpModify},
			},
		},
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Decomposition: &plan.Decomposition{
			Rationale: "scope split",
			SubPlans: []plan.SubPlanSummary{
				{Title: "Part A title", ScopeHint: "Implement Part A.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
				{Title: "Part B title", ScopeHint: "Implement Part B.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/b/b.go", Operation: plan.FileOpModify}}}},
				{Title: "Part C title", ScopeHint: "Implement Part C.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/c/c.go", Operation: plan.FileOpModify}}}},
			},
		},
	}

	childFor := func(title string, idx int) *run.Run {
		sliceIdx := idx
		return &run.Run{
			ID:             uuid.New(),
			Repo:           "o/r",
			ParentRunID:    &parentRunID,
			DecomposedFrom: &parentRunID,
			SliceIndex:     &sliceIdx,
			IssueContext: &run.IssueContext{
				Title: title,
				Body:  "## " + title + "\n\nslice body\n\n---\n*Decomposed sub-plan.*",
			},
		}
	}

	s := New(Config{Addr: "127.0.0.1:0"})
	ctx := context.Background()

	type slice struct {
		title    string
		wantFile string
	}
	slices := []slice{
		{"Part A title", "pkg/a/a.go"},
		{"Part B title", "pkg/b/b.go"},
		{"Part C title", "pkg/c/c.go"},
	}

	narrowedSets := make([][]string, 0, len(slices))
	unionDeclared := map[string]struct{}{}
	for i, sl := range slices {
		child := childFor(sl.title, i)

		// (a) resolveDecomposedScopeFiles narrows to the slice + its coupled test.
		gotFiles := scopePathsList(s.resolveDecomposedScopeFiles(ctx, child, parentPlan))
		wantFiles := []string{sl.wantFile, coupledTestPath(sl.wantFile)}
		if !reflect.DeepEqual(gotFiles, wantFiles) {
			t.Errorf("%s: resolveDecomposedScopeFiles = %v, want its slice + coupled test %v", sl.title, gotFiles, wantFiles)
		}
		narrowedSets = append(narrowedSets, gotFiles)

		// (c) resolveDecomposedScopeConstraint carries the declared slice files.
		sc := s.resolveDecomposedScopeConstraint(ctx, child, parentPlan)
		if sc == nil {
			t.Fatalf("%s: resolveDecomposedScopeConstraint returned nil", sl.title)
		}
		if !reflect.DeepEqual(sc.ScopeFiles, []string{sl.wantFile}) {
			t.Errorf("%s: constraint.ScopeFiles = %v, want %v", sl.title, sc.ScopeFiles, []string{sl.wantFile})
		}
		for _, f := range sc.ScopeFiles {
			unionDeclared[f] = struct{}{}
		}
	}

	// (b) the per-child narrowed sets are pairwise disjoint.
	seen := map[string]string{}
	for i, set := range narrowedSets {
		for _, f := range set {
			if prev, dup := seen[f]; dup {
				t.Errorf("file %q appears in slice %q and slice %d — narrowed sets must be disjoint", f, prev, i)
			}
			seen[f] = slices[i].title
		}
	}

	// (c cont.) the declared slice files union back to the parent scope.
	wantUnion := map[string]struct{}{"pkg/a/a.go": {}, "pkg/b/b.go": {}, "pkg/c/c.go": {}}
	if !reflect.DeepEqual(unionDeclared, wantUnion) {
		t.Errorf("union of declared slice files = %v, want the parent scope %v", unionDeclared, wantUnion)
	}
}

// scopePathsList extracts the ordered paths from a []scopeFile.
func scopePathsList(sfs []scopeFile) []string {
	out := make([]string, 0, len(sfs))
	for _, f := range sfs {
		out = append(out, f.Path)
	}
	return out
}

// coupledTestPath returns the _test.go sibling of a non-test .go path.
func coupledTestPath(p string) string {
	return strings.TrimSuffix(p, ".go") + "_test.go"
}

// TestCoupledTestSiblings exercises the pure stem-sibling derivation behind
// the decomposed-child scope auto-fold (#1083): a non-test .go entry yields
// its _test.go sibling, a _test.go entry yields nothing, non-.go and
// delete-operation entries are skipped, nested dirs produce correct paths,
// and duplicate siblings collapse in first-seen order.
func TestCoupledTestSiblings(t *testing.T) {
	tests := []struct {
		name  string
		files []scopeFile
		want  []string
	}{
		{
			name:  "non-test go yields sibling",
			files: []scopeFile{{Path: "pkg/a/a.go", Operation: "create"}},
			want:  []string{"pkg/a/a_test.go"},
		},
		{
			name:  "test file yields nothing",
			files: []scopeFile{{Path: "pkg/a/a_test.go", Operation: "modify"}},
			want:  nil,
		},
		{
			name:  "non-go ignored",
			files: []scopeFile{{Path: "docs/x.yaml", Operation: "modify"}},
			want:  nil,
		},
		{
			name:  "delete operation skipped",
			files: []scopeFile{{Path: "pkg/a/a.go", Operation: "delete"}},
			want:  nil,
		},
		{
			name: "nested dirs and mixed packages",
			files: []scopeFile{
				{Path: "backend/internal/server/plan.go", Operation: "modify"},
				{Path: "backend/internal/run/run.go", Operation: "create"},
			},
			want: []string{
				"backend/internal/server/plan_test.go",
				"backend/internal/run/run_test.go",
			},
		},
		{
			name: "duplicate source files collapse",
			files: []scopeFile{
				{Path: "pkg/a/a.go", Operation: "create"},
				{Path: "pkg/a/a.go", Operation: "modify"},
			},
			want: []string{"pkg/a/a_test.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coupledTestSiblings(tt.files)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("coupledTestSiblings = %v, want %v", got, tt.want)
			}
		})
	}

	// dedup against an already-scoped sibling is foldScopePaths' job: a
	// _test.go already in scope must not be duplicated by the fold.
	t.Run("already-scoped sibling not duplicated", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		in := []scopeFile{
			{Path: "pkg/a/a.go", Operation: "create"},
			{Path: "pkg/a/a_test.go", Operation: "modify"},
		}
		got := s.foldScopePaths(context.Background(), in, coupledTestSiblings(in), "coupled-test-sibling")
		if len(got) != 2 {
			t.Fatalf("folded scope = %v, want no duplicate of the already-scoped _test.go", got)
		}
	})
}

// makeApproveWithCommentEntry builds an approval_submitted audit entry with
// decision=approve and a non-empty operator comment (the approve-with-
// conditions text loadApprovalConditions reads).
func makeApproveWithCommentEntry(runID uuid.UUID, comment string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision": "approve",
		"comment":  comment,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

// makePlanReusedFromEntry builds a plan_reused_from audit entry carrying the
// #1229 exempted_paths + resume reason — the durable record the recovery
// prompt builder reads back.
func makePlanReusedFromEntry(runID uuid.UUID, exempted []scopeExemption, reason string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"parent_run_id":           uuid.New().String(),
		"parent_failure_category": "B",
		"source":                  "operator_recovery",
		"exempted_paths":          exempted,
		"reason":                  reason,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: CategoryPlanReusedFrom, RunID: &rid, Payload: payload}
}

func TestResolveRecoveryScopeExemptions(t *testing.T) {
	runID := uuid.New()
	want := []scopeExemption{{Path: "backend/a.go", Reason: "unchanged on this slice"}}

	t.Run("returns the run's exempted_paths", func(t *testing.T) {
		s := newFeedbackServer(t, nil, map[uuid.UUID][]*audit.Entry{
			runID: {makePlanReusedFromEntry(runID, want, "recover")},
		})
		got := s.resolveRecoveryScopeExemptions(context.Background(), runID)
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("absent plan_reused_from returns nil", func(t *testing.T) {
		s := newFeedbackServer(t, nil, nil)
		if got := s.resolveRecoveryScopeExemptions(context.Background(), runID); got != nil {
			t.Errorf("got %+v, want nil (no plan_reused_from)", got)
		}
	})

	t.Run("plan_reused_from with no exemptions returns nil", func(t *testing.T) {
		s := newFeedbackServer(t, nil, map[uuid.UUID][]*audit.Entry{
			runID: {makePlanReusedFromEntry(runID, nil, "recover")},
		})
		if got := s.resolveRecoveryScopeExemptions(context.Background(), runID); got != nil {
			t.Errorf("got %+v, want nil (no exempted_paths)", got)
		}
	})
}

func TestLoadRecoveryResumeReason(t *testing.T) {
	runID := uuid.New()

	t.Run("returns the resume reason", func(t *testing.T) {
		s := newFeedbackServer(t, nil, map[uuid.UUID][]*audit.Entry{
			runID: {makePlanReusedFromEntry(runID, nil, "steer the recovery here")},
		})
		got := s.loadRecoveryResumeReason(context.Background(), runID)
		if got == nil || *got != "steer the recovery here" {
			t.Errorf("got %v, want the resume reason", got)
		}
	})

	t.Run("absent plan_reused_from returns nil", func(t *testing.T) {
		s := newFeedbackServer(t, nil, nil)
		if got := s.loadRecoveryResumeReason(context.Background(), runID); got != nil {
			t.Errorf("got %q, want nil", *got)
		}
	})

	t.Run("empty/whitespace reason returns nil", func(t *testing.T) {
		s := newFeedbackServer(t, nil, map[uuid.UUID][]*audit.Entry{
			runID: {makePlanReusedFromEntry(runID, nil, "   ")},
		})
		if got := s.loadRecoveryResumeReason(context.Background(), runID); got != nil {
			t.Errorf("got %q, want nil (whitespace reason)", *got)
		}
	})
}

func TestAppendRecoveryResumeReason(t *testing.T) {
	conditions := "Inherited approve-with-conditions text."
	reason := "Recover with the declared file unchanged."

	t.Run("nil reason leaves conditions unchanged", func(t *testing.T) {
		got := appendRecoveryResumeReason(&conditions, nil)
		if got != &conditions {
			t.Errorf("got %v, want the original conditions pointer unchanged", got)
		}
	})

	t.Run("nil conditions yields the labeled reason", func(t *testing.T) {
		got := appendRecoveryResumeReason(nil, &reason)
		if got == nil || *got != "Recovery directive (resume_run reason): "+reason {
			t.Errorf("got %v, want the labeled reason", got)
		}
	})

	t.Run("both combine conditions then reason", func(t *testing.T) {
		got := appendRecoveryResumeReason(&conditions, &reason)
		want := conditions + "\n\nRecovery directive (resume_run reason): " + reason
		if got == nil || *got != want {
			t.Errorf("got %v, want %q", got, want)
		}
	})
}

// TestGetStagePrompt_Implement_RecoveryScopeExemptionsAndReason crosses the
// plan_reused_from audit → implement prompt-response path (#1229) on BOTH
// build paths (the signed /prompt and the SPA /prompt-render): the recovery
// run's exempt_scope_files surface in resp.ScopeExemptions and the resume
// reason renders into the binding "Approval conditions" block (Part D). A
// control run with no plan_reused_from leaves both empty/absent.
func TestGetStagePrompt_Implement_RecoveryScopeExemptionsAndReason(t *testing.T) {
	const exemptPath = "backend/internal/server/handlers.go"
	const resumeReason = "Recover leaving the declared file unchanged on this slice."

	buildServer := func(t *testing.T, withReuse bool) (*Server, *promptRunRepo, *signingFake, uuid.UUID, uuid.UUID) {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "recoverable plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: exemptPath, Operation: plan.FileOpModify}},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
				{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
			},
		}
		rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		auditByRun := map[uuid.UUID][]*audit.Entry{}
		if withReuse {
			auditByRun[runID] = []*audit.Entry{
				makePlanReusedFromEntry(runID,
					[]scopeExemption{{Path: exemptPath, Reason: "declared but unchanged"}}, resumeReason),
			}
		}

		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}
		return s, rr, sf, runID, implStageID
	}

	decode := func(t *testing.T, w *httptest.ResponseRecorder) promptResponse {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode prompt: %v", err)
		}
		return resp
	}

	renderRequest := func(t *testing.T, s *Server, stageID uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/v0/stages/%s/prompt-render", stageID), nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}

	t.Run("recovery run carries exemptions + reason on /prompt", func(t *testing.T) {
		s, _, sf, runID, implStageID := buildServer(t, true)
		priv, _ := sf.issue(t, runID)
		resp := decode(t, promptRequest(t, s, runID, implStageID, priv, ""))
		if len(resp.ScopeExemptions) != 1 || resp.ScopeExemptions[0].Path != exemptPath ||
			resp.ScopeExemptions[0].Reason != "declared but unchanged" {
			t.Errorf("ScopeExemptions = %+v, want the one operator exemption", resp.ScopeExemptions)
		}
		if !strings.Contains(resp.Prompt, resumeReason) {
			t.Errorf("prompt missing the Part D resume reason %q\n---\n%s", resumeReason, resp.Prompt)
		}
	})

	t.Run("recovery run carries exemptions + reason on /prompt-render", func(t *testing.T) {
		s, _, _, _, implStageID := buildServer(t, true)
		resp := decode(t, renderRequest(t, s, implStageID))
		if len(resp.ScopeExemptions) != 1 || resp.ScopeExemptions[0].Path != exemptPath {
			t.Errorf("ScopeExemptions = %+v, want the one operator exemption", resp.ScopeExemptions)
		}
		if !strings.Contains(resp.Prompt, resumeReason) {
			t.Errorf("prompt-render missing the Part D resume reason %q\n---\n%s", resumeReason, resp.Prompt)
		}
	})

	t.Run("non-recovery run has empty ScopeExemptions + no reason", func(t *testing.T) {
		s, _, sf, runID, implStageID := buildServer(t, false)
		priv, _ := sf.issue(t, runID)
		resp := decode(t, promptRequest(t, s, runID, implStageID, priv, ""))
		if len(resp.ScopeExemptions) != 0 {
			t.Errorf("ScopeExemptions = %+v, want empty (no plan_reused_from)", resp.ScopeExemptions)
		}
		if strings.Contains(resp.Prompt, resumeReason) {
			t.Errorf("non-recovery prompt unexpectedly contains the resume reason\n---\n%s", resp.Prompt)
		}
	})
}

// TestGetStagePrompt_ApprovalConditions_DecompositionFallback is the
// integration test for #677: it crosses the audit-load -> handler ->
// rendered-prompt-text path and asserts the parent plan-gate's binding
// approve-with-conditions text propagates into a decomposed child's
// implement prompt (the #558 approval-note delivery, which a child with no
// plan stage of its own would otherwise silently drop). The standalone and
// no-conditions subtests guard the backward-compatible boundaries.
func TestGetStagePrompt_ApprovalConditions_DecompositionFallback(t *testing.T) {
	const parentCondition = "Use the orthogonal-lens reviewer; do NOT touch the legacy adapter."

	// parentPlan carries a two-sub-plan decomposition; the child links to a
	// sub-plan by SliceIndex (matchDecomposedSubPlan, #1721). Each sub-plan
	// declares its own scope so the child resolves rather than failing closed.
	newParentPlan := func() *plan.Plan {
		return &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "parent plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Decomposition: &plan.Decomposition{
				Rationale: "scope split",
				SubPlans: []plan.SubPlanSummary{
					{Title: "Part A title", ScopeHint: "Implement Part A in pkg/a.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
					{Title: "Part B title", ScopeHint: "Implement Part B in pkg/b.", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/b/b.go", Operation: plan.FileOpModify}}}},
				},
			},
		}
	}

	// seedDecomposedChild wires a parent plan artifact + a decomposed child run
	// linked to Part A by SliceIndex=0, with the supplied audit entries keyed
	// by run ID, and returns the child's implement-stage prompt response.
	seedDecomposedChild := func(t *testing.T, auditByRun map[uuid.UUID][]*audit.Entry, parentRunID uuid.UUID) promptResponse {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()

		planBytes, err := json.Marshal(newParentPlan())
		if err != nil {
			t.Fatalf("marshal parent plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       parentPlanStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			parentRunID: {
				{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan},
			},
		}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}

		sliceIdx := 0
		childBody := "## Part A title\n\nImplement Part A in pkg/a.\n\n---\n*Decomposed sub-plan.*"
		rr.getRuns[childRunID] = &run.Run{
			ID:             childRunID,
			Repo:           "o/r",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerCLI,
			ParentRunID:    &parentRunID,
			DecomposedFrom: &parentRunID,
			SliceIndex:     &sliceIdx,
			IssueContext: &run.IssueContext{
				Title: "Part A title",
				Body:  childBody,
			},
		}
		rr.getStages[childStageID] = &run.Stage{
			ID:    childStageID,
			RunID: childRunID,
			Type:  run.StageTypeImplement,
		}

		priv, _ := sf.issue(t, childRunID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo:    &feedbackAuditRepo{byRunID: auditByRun},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, childRunID, childStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	t.Run("child inherits parent approval conditions", func(t *testing.T) {
		parentRunID := uuid.New()
		resp := seedDecomposedChild(t, map[uuid.UUID][]*audit.Entry{
			parentRunID: {makeApproveWithCommentEntry(parentRunID, parentCondition)},
		}, parentRunID)
		for _, want := range []string{"### Approval conditions", parentCondition} {
			if !strings.Contains(resp.Prompt, want) {
				t.Errorf("child prompt missing %q\n---\n%s", want, resp.Prompt)
			}
		}
	})

	t.Run("child with no parent conditions renders no block", func(t *testing.T) {
		parentRunID := uuid.New()
		// Parent approved with an empty comment → no conditions.
		resp := seedDecomposedChild(t, map[uuid.UUID][]*audit.Entry{
			parentRunID: {makeApproveEntry(parentRunID)},
		}, parentRunID)
		if strings.Contains(resp.Prompt, "### Approval conditions") {
			t.Errorf("child prompt should carry no approval-conditions block:\n%s", resp.Prompt)
		}
	})

	t.Run("standalone run still renders its own conditions", func(t *testing.T) {
		const standaloneCondition = "Cap the retry budget at 2 and keep the timeout drift fix."
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		standalonePlan := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "standalone plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		}
		planBytes, err := json.Marshal(standalonePlan)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		// DecomposedFrom nil → standalone; the helper reads the run's OWN
		// approval_submitted entries with no parent fallback in play.
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{
			ID:    implStageID,
			RunID: runID,
			Type:  run.StageTypeImplement,
		}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithCommentEntry(runID, standaloneCondition)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, want := range []string{"### Approval conditions", standaloneCondition} {
			if !strings.Contains(resp.Prompt, want) {
				t.Errorf("standalone prompt missing %q\n---\n%s", want, resp.Prompt)
			}
		}
	})
}

// makeStageRetriedEntry builds a stage_retried audit entry (#1176 fixture)
// recording that the run's implement stage was re-dispatched in place. It does
// not feed the prompt renderer — loadApprovalConditions reads only the
// approval_submitted entry — and exists solely so the fixture faithfully models
// the post-retry_stage state the regression below guards.
func makeStageRetriedEntry(runID uuid.UUID) *audit.Entry {
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: CategoryStageRetried, RunID: &rid}
}

// TestGetStagePrompt_Implement_Retried_ReinjectsApprovalConditions is a
// REGRESSION GUARD, not a test of a retry-specific code branch. It locks the
// invariant that a retried implement stage's prompt is byte-equivalent to first
// dispatch with respect to the operator's #558 approval-condition amendments.
//
// Diagnostic finding (#1176): the issue's premise — that retry_stage DROPS the
// approval conditions and rebuilds the overruled plan — is contradicted by the
// code. The implement prompt-construction path re-injects approval conditions
// purely by runID on EVERY fetch: prompt.go sets
// trigger.ApprovalConditions = s.resolveApprovalConditions(runRow)
// unconditionally in the implement branch, with NO RetryAttempt/SelfRetryCount
// gating and NO first-dispatch prompt snapshot; loadApprovalConditions scans the
// run's approval_submitted audit entries newest-first by runID. Both render
// blocks fire purely on ApprovalConditions != nil. So nothing today consults
// SelfRetryCount when re-injecting — the fixture sets it only to model the
// post-retry state, and this test does not exercise a retry-only path. Evidence
// run 000ae761 predates the #1171/#1185 agent-disregard fix, so #1176 is largely
// a duplicate of #1171 (root cause fixed by #1185) and is converted here into a
// permanent regression guarantee. This test will FAIL if a future prompt-path
// refactor reintroduces retry-conditional gating or a first-dispatch snapshot
// that would let a retry diverge from first dispatch. Cross-reference
// #1171/#1185/#1176.
func TestGetStagePrompt_Implement_Retried_ReinjectsApprovalConditions(t *testing.T) {
	// A distinctive sentinel so its verbatim survival across the retry is
	// unambiguous in the rendered prompt body.
	const condition = "Best-effort POST-create parent linking; record EpicLinkError on failure but still return the created issue."

	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
	}
	rr.getRuns[runID] = &run.Run{
		ID:            runID,
		Repo:          "o/r",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
	}
	// Model a RETRIED implement stage: SelfRetryCount>0 plus a stage_retried
	// audit entry, i.e. the post-retry_stage state #1176 describes.
	rr.getStages[implStageID] = &run.Stage{
		ID:             implStageID,
		RunID:          runID,
		Type:           run.StageTypeImplement,
		SelfRetryCount: 1,
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {
				makeApproveWithCommentEntry(runID, condition),
				makeStageRetriedEntry(runID),
			},
		}},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Both blocks plus the verbatim sentinel must survive the retry.
	for _, want := range []string{
		"### Approval conditions",
		"### Binding conditions — confirm each in your PR Notes",
		condition,
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("retried implement prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}

	// Ordering: the pre-plan "### Approval conditions" block must precede the
	// #1185 tail reinforcement block, mirroring first-dispatch rendering.
	pre := strings.Index(resp.Prompt, "### Approval conditions")
	reinforce := strings.Index(resp.Prompt, "### Binding conditions — confirm each in your PR Notes")
	if pre < 0 || reinforce < 0 || pre >= reinforce {
		t.Errorf("approval-condition blocks out of order: pre-plan idx=%d, reinforcement idx=%d\n---\n%s", pre, reinforce, resp.Prompt)
	}
}

// makeApproveWithScopeFilesEntry builds an approval_submitted audit entry with
// decision=approve carrying the structured add_scope_files slice (#824) that
// loadApprovalAddScopeFiles reads back.
func makeApproveWithScopeFilesEntry(runID uuid.UUID, addScopeFiles []string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision":        "approve",
		"add_scope_files": addScopeFiles,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

// TestGetStagePrompt_Implement_AddScopeFilesFoldedIntoScope crosses the full
// #824 seam: persisted approval_submitted.add_scope_files ->
// resolveApprovalAddScopeFiles -> mergeStructuredScopeFiles ->
// promptResponse.ScopeFiles. An approved plan declares one scope file; the
// structured add_scope_files names three the prose fold cannot reach — a
// DIRECTORY (trailing slash), an extensionless ROOT file (go.work), and a
// slash-path with a dotted name — and all four must surface on the rendered
// implement-prompt scope. Subtests pin the empty-scope guard and decomposition-
// parent inheritance.
func TestGetStagePrompt_Implement_AddScopeFilesFoldedIntoScope(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"
	structured := []string{
		"backend/internal/agenteval/testdata/corpus/newcase/", // directory
		"go.work",                  // extensionless repo-root file
		"docs/spec/standard_v1.md", // slash-path with a dotted name
	}

	t.Run("structured paths folded into declared scope", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "scoped plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithScopeFilesEntry(runID, structured)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := make(map[string]bool, len(resp.ScopeFiles))
		for _, f := range resp.ScopeFiles {
			got[f.Path] = true
		}
		for _, want := range append([]string{plannedFile}, structured...) {
			if !got[want] {
				t.Errorf("resp.ScopeFiles missing %q; got %#v", want, resp.ScopeFiles)
			}
		}
	})

	t.Run("plan-missing run is not augmented", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		implStageID := uuid.New()

		// No plan artifact → empty scope. The structured fold must keep it
		// empty so the runner's git add -A fallback isn't silently narrowed.
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {}}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				runID: {makeApproveWithScopeFilesEntry(runID, structured)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ScopeFiles != nil {
			t.Errorf("plan-missing run must keep empty scope, got %#v", resp.ScopeFiles)
		}
	})

	t.Run("decomposed child inherits parent add_scope_files", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		parentRunID := uuid.New()
		childRunID := uuid.New()
		parentPlanStageID := uuid.New()
		childStageID := uuid.New()

		// Parent plan declares a decomposition whose slice-0 sub-plan scopes the
		// same file, so the child resolves its slice to a non-empty scope. Post
		// #1721 a decomposed child MUST link to a slice — a decomposition-less
		// parent plan now fails closed rather than inheriting the parent's
		// top-level scope. add_scope_files is keyed to the PARENT run, so the fold
		// must still walk up and land the inherited paths in the child's resolved
		// slice scope.
		parentPlan := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "parent plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
			},
			Decomposition: &plan.Decomposition{
				Rationale: "scope split",
				SubPlans: []plan.SubPlanSummary{
					{Title: "Part A", ScopeHint: "A", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}}}},
				},
			},
		}
		planBytes, err := json.Marshal(parentPlan)
		if err != nil {
			t.Fatalf("marshal parent plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       parentPlanStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			parentRunID: {{ID: parentPlanStageID, RunID: parentRunID, Type: run.StageTypePlan}},
		}
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r"}
		childSliceIdx := 0
		rr.getRuns[childRunID] = &run.Run{
			ID:             childRunID,
			Repo:           "o/r",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerCLI,
			ParentRunID:    &parentRunID,
			DecomposedFrom: &parentRunID,
			SliceIndex:     &childSliceIdx,
		}
		rr.getStages[childStageID] = &run.Stage{ID: childStageID, RunID: childRunID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, childRunID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			// add_scope_files is keyed to the PARENT run; the child has no
			// gate of its own, so resolveApprovalAddScopeFiles must walk up.
			AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
				parentRunID: {makeApproveWithScopeFilesEntry(parentRunID, structured)},
			}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, childRunID, childStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := make(map[string]bool, len(resp.ScopeFiles))
		for _, f := range resp.ScopeFiles {
			got[f.Path] = true
		}
		for _, want := range structured {
			if !got[want] {
				t.Errorf("child resp.ScopeFiles missing inherited %q; got %#v", want, resp.ScopeFiles)
			}
		}
	})
}

// TestGetStagePrompt_Implement_AddScopeFilesFoldedOnInitialDispatch is the
// #1351 regression test and the issue's done-means. Unlike
// TestGetStagePrompt_Implement_AddScopeFilesFoldedIntoScope — which hand-seeds
// the approval_submitted audit entry through a no-op AppendChained fake and so
// masks any write->persist->read seam — this test drives the REAL
// approve->advance->first-fetch sequence end to end:
//
//	(a) seed an approved plan artifact with one scope.file;
//	(b) call the REAL handleSubmitApproval on the plan stage with
//	    add_scope_files=[two paths] so writeApprovalAudit emits the
//	    approval_submitted entry exactly as production does, persisted through a
//	    STORING audit fake (storingAuditRepo) that genuinely round-trips
//	    AppendChained -> ListForRunByCategory;
//	(c) advance the plan stage (the handler does this);
//	(d) call handleGetStagePrompt for the implement stage exactly ONCE — the
//	    initial dispatch, with NO second render-poll;
//
// then assert resp.ScopeFiles == plan.Scope ∪ add_scope_files on that FIRST
// fetch. This is the exact write->persist->read seam the no-op AppendChained
// fake hid (#1351), and a cross-boundary integration test (#618): it crosses
// the approve-write -> approval_submitted persist -> implement prompt-read
// boundary that breaks in production while the per-helper unit tests each pass.
func TestGetStagePrompt_Implement_AddScopeFilesFoldedOnInitialDispatch(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"
	// The two paths from the originating run 03f6e28a/#1349.
	addScopeFiles := []string{
		"runner/cmd/fishhawk-runner/main_test.go",
		"runner/internal/agent/agent_test.go",
	}

	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()
	au := newStoringAuditRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	// The plan stage must be awaiting_approval so the approve transition
	// (awaiting_approval -> succeeded) is valid; it is also listed under the run
	// so loadApprovedPlanForRun resolves the artifact post-advance.
	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage}}
	rr.getStages[planStageID] = planStage
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}
	rr.getRuns[runID] = &run.Run{
		ID:            runID,
		Repo:          "o/r",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
	}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		ApprovalRepo: newFakeApprovalRepo(),
		AuditRepo:    au,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	// (b)+(c): the REAL approval WRITE path — writeApprovalAudit persists
	// approval_submitted carrying the two paths through the storing fake, and
	// advanceStage flips the plan stage to succeeded.
	body := fmt.Sprintf(`{"decision":"approve","add_scope_files":[%q,%q]}`, addScopeFiles[0], addScopeFiles[1])
	aw := submitApproval(t, s, planStageID, body)
	if aw.Code != http.StatusOK {
		t.Fatalf("approval status = %d, want 200:\n%s", aw.Code, aw.Body.String())
	}

	// (d): ONE implement prompt-fetch — the initial dispatch, no render-poll.
	priv, _ := sf.issue(t, runID)
	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make(map[string]bool, len(resp.ScopeFiles))
	for _, f := range resp.ScopeFiles {
		got[f.Path] = true
	}
	for _, want := range append([]string{plannedFile}, addScopeFiles...) {
		if !got[want] {
			t.Errorf("FIRST implement prompt-fetch scope_files missing %q; must be plan.Scope ∪ add_scope_files on the initial dispatch (#1351); got %#v", want, resp.ScopeFiles)
		}
	}
}

// TestGetStagePrompt_Implement_AddScopeFilesVisibleInPromptText is the #1406
// done-means and a cross-boundary integration test (#618): operator-added
// scope.files must be visible in the implement prompt TEXT, not only the
// enforced ScopeFiles list. #1351 folded them into the ENFORCED scope; this seam
// proves the complementary AGENT-VISIBILITY half — without it a defensive agent
// reads only the rendered plan scope, concludes the added paths are out of
// scope, and files a redundant mid-stage amendment for paths already folded
// (run 6434aae9). It drives the REAL approve-write -> approval_submitted persist
// -> implement prompt-read path (mirroring AddScopeFilesFoldedOnInitialDispatch)
// and asserts resp.Prompt carries the "Operator-added scope files" section AND
// each added path — a comment-only/no-op touch of prompt.go fails it where a
// presence-only gate would pass (#1169). The render (SPA-readable) handler is
// asserted to carry the identical text so the two prompt paths stay byte-aligned.
func TestGetStagePrompt_Implement_AddScopeFilesVisibleInPromptText(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"
	addScopeFiles := []string{
		"backend/internal/server/trace.go",
		"docs/issue-comment-surfaces.md",
	}

	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()
	au := newStoringAuditRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage}}
	rr.getStages[planStageID] = planStage
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}
	rr.getRuns[runID] = &run.Run{
		ID:            runID,
		Repo:          "o/r",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
	}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		ApprovalRepo: newFakeApprovalRepo(),
		AuditRepo:    au,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	// REAL approve-write: writeApprovalAudit persists approval_submitted with the
	// two added paths through the storing fake; advanceStage flips plan->succeeded.
	body := fmt.Sprintf(`{"decision":"approve","add_scope_files":[%q,%q]}`, addScopeFiles[0], addScopeFiles[1])
	aw := submitApproval(t, s, planStageID, body)
	if aw.Code != http.StatusOK {
		t.Fatalf("approval status = %d, want 200:\n%s", aw.Code, aw.Body.String())
	}

	// Runner-facing (signed) implement prompt.
	priv, _ := sf.issue(t, runID)
	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The shipped behavior: the section header AND each added path appear in the
	// prompt TEXT. (The enforced ScopeFiles fold is covered by #1351's tests.)
	if !contains(resp.Prompt, "Operator-added scope files (approved — in-scope, do NOT request an amendment)") {
		t.Errorf("implement prompt missing the operator-added-scope section:\n%s", resp.Prompt)
	}
	for _, path := range addScopeFiles {
		if !contains(resp.Prompt, path) {
			t.Errorf("implement prompt text missing operator-added path %q:\n%s", path, resp.Prompt)
		}
	}

	// Render (SPA-readable) handler must carry the identical section text so the
	// two prompt paths stay byte-aligned (the audit story depends on the SPA
	// showing what the runner saw).
	rendered := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", implStageID), nil)
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, rendered)
	if rw.Code != http.StatusOK {
		t.Fatalf("render status = %d, want 200:\n%s", rw.Code, rw.Body.String())
	}
	var renderResp promptResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &renderResp); err != nil {
		t.Fatalf("decode render: %v", err)
	}
	if renderResp.Prompt != resp.Prompt {
		t.Errorf("render-path prompt diverged from signed path:\nsigned:\n%s\n---\nrendered:\n%s",
			resp.Prompt, renderResp.Prompt)
	}
}

// TestSubmitApproval_AddScopeFiles_AuditVisibleBeforePlanStageAdvance pins the
// #1351 ROOT CAUSE (the contingency the in-process fold test confirmed): the
// approval_submitted audit carrying add_scope_files MUST be durably committed
// BEFORE the plan stage transitions to succeeded. That transition is the signal
// every dispatch path keys on — the synchronous Orchestrator.Advance AND an
// asynchronous reconciler that observes a succeeded plan stage with a still-
// pending implement stage. If the audit lands AFTER the transition (the pre-fix
// ordering: advanceStage then writeApprovalAudit), the initial implement
// prompt-fetch can race the write and lose — loadApprovalAddScopeFiles returns
// empty and the operator-declared paths are dropped from the enforced scope,
// forcing the redundant mid-stage amendment observed in runs 03f6e28a/#1349 and
// e6e379fd/#1352 (the merge logs fired only on the later render-poll, never at
// the initial dispatch fetch).
//
// The assertion is the happens-before: at the instant the plan stage flips to
// succeeded, the add_scope_files audit is already queryable. This FAILS on the
// pre-fix ordering and PASSES once writeApprovalAudit is moved ahead of
// advanceStage. No ArtifactRepo/SigningRepo is wired — the prompt is never
// fetched; the test isolates the write→transition ordering inside the approval
// handler.
func TestSubmitApproval_AddScopeFiles_AuditVisibleBeforePlanStageAdvance(t *testing.T) {
	addScopeFiles := []string{
		"runner/cmd/fishhawk-runner/main_test.go",
		"runner/internal/agent/agent_test.go",
	}

	rr := newPromptRunRepo()
	au := newStoringAuditRepo()

	runID := uuid.New()
	planStageID := uuid.New()

	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	rr.getStages[planStageID] = planStage
	rr.getRuns[runID] = &run.Run{
		ID:            runID,
		Repo:          "o/r",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
	}

	var transitionObserved, addScopeFilesVisibleAtTransition bool
	rr.onTransitionStage = func(id uuid.UUID, to run.StageState) {
		if id != planStageID || to != run.StageStateSucceeded {
			return
		}
		transitionObserved = true
		entries, _ := au.ListForRunByCategory(context.Background(), runID, "approval_submitted")
		for _, e := range entries {
			var pl struct {
				Decision      string   `json:"decision"`
				AddScopeFiles []string `json:"add_scope_files"`
			}
			if json.Unmarshal(e.Payload, &pl) == nil && pl.Decision == "approve" && len(pl.AddScopeFiles) > 0 {
				addScopeFilesVisibleAtTransition = true
			}
		}
	}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: newFakeApprovalRepo(),
		AuditRepo:    au,
	})

	body := fmt.Sprintf(`{"decision":"approve","add_scope_files":[%q,%q]}`, addScopeFiles[0], addScopeFiles[1])
	w := submitApproval(t, s, planStageID, body)
	if w.Code != http.StatusOK {
		t.Fatalf("approval status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	if !transitionObserved {
		t.Fatal("plan stage never transitioned to succeeded")
	}
	if !addScopeFilesVisibleAtTransition {
		t.Error("approval_submitted add_scope_files audit was NOT visible when the plan stage advanced to succeeded; the initial implement dispatch can race the write and drop the operator-declared scope (#1351)")
	}
}

// makeApproveWithCommentAndScopeFilesEntry builds an approval_submitted audit
// entry carrying BOTH a free-text operator comment (the #558 binding-conditions
// reason) AND the structured add_scope_files slice (#824). It lets the #1225
// regression test prove the two channels diverge: the comment prose is NOT
// folded into scope while the structured slice still is.
func makeApproveWithCommentAndScopeFilesEntry(runID uuid.UUID, comment string, addScopeFiles []string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision":        "approve",
		"comment":         comment,
		"add_scope_files": addScopeFiles,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

// TestGetStagePrompt_Implement_ReasonPathNotFoldedIntoScope is the #1225
// regression guard for the removed #730 approve-reason prose fold. It crosses
// the full audit-load -> handler -> promptResponse.ScopeFiles seam: an approved
// plan declares one scope file, the binding approve-with-conditions COMMENT
// names a repo-relative path (backend/go.mod — the exact E24.4/#1144 token an
// operator wrote into the reason to EXPLAIN no go.mod edit was needed), and the
// structured add_scope_files names a DIFFERENT path. The rendered implement
// prompt's scope_files must carry the plan file and the structured path but
// MUST NOT carry the comment-named path — proving reason prose no longer mutates
// scope (so it can never be folded as an unsatisfiable required-to-touch entry)
// while the structured fold still works. It fails on the pre-#1225 code (which
// scraped backend/go.mod out of the comment and folded it).
func TestGetStagePrompt_Implement_ReasonPathNotFoldedIntoScope(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"
	const reasonPath = "backend/go.mod"
	const structuredPath = "docs/api/v0.md"
	const comment = "Approved. No edit to `backend/go.mod` is needed — it is already correct."

	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
	}
	rr.getRuns[runID] = &run.Run{
		ID:            runID,
		Repo:          "o/r",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
	}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {makeApproveWithCommentAndScopeFilesEntry(runID, comment, []string{structuredPath})},
		}},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make(map[string]bool, len(resp.ScopeFiles))
	for _, f := range resp.ScopeFiles {
		got[f.Path] = true
	}
	// The plan file and the STRUCTURED add_scope_files path are in scope.
	for _, want := range []string{plannedFile, structuredPath} {
		if !got[want] {
			t.Errorf("resp.ScopeFiles missing %q; got %#v", want, resp.ScopeFiles)
		}
	}
	// The path named ONLY in the approval reason/comment must NOT be folded.
	if got[reasonPath] {
		t.Errorf("reason-prose path %q must NOT be folded into scope_files (#1225); got %#v", reasonPath, resp.ScopeFiles)
	}
	// The binding comment must still reach the agent as #558 conditions.
	if !strings.Contains(resp.Prompt, comment) {
		t.Errorf("approval comment must still be injected as binding conditions (#558); prompt missing %q", comment)
	}
}

// TestAddScopeFiles_DoesNotBypassForbiddenPathsGate is the #824 security
// assertion (added at approval): folding a path into the implement scope via
// add_scope_files must NOT let it slip past the forbidden_paths policy gate.
// The two layers are independent by construction — the structured fold only
// shapes promptResponse.ScopeFiles (the runner's staging set), while the
// category-B gate is policy.Evaluate(diff, constraints), which reads the
// PRODUCED diff and the spec's forbidden_paths and has no scope.files input at
// all. This test pins both halves: (1) the fold genuinely stages the forbidden
// path into scope, and (2) the same path in the produced diff is still a
// forbidden_paths violation, so it fails category-B regardless of the fold.
func TestAddScopeFiles_DoesNotBypassForbiddenPathsGate(t *testing.T) {
	const forbidden = ".github/workflows/x.yml"

	// (1) The structured fold stages the forbidden path into scope.
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify}},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change", TriggerSource: run.TriggerCLI}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {makeApproveWithScopeFilesEntry(runID, []string{forbidden})},
		}},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	folded := false
	for _, f := range resp.ScopeFiles {
		if f.Path == forbidden {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("precondition: add_scope_files did not fold %q into scope; got %#v", forbidden, resp.ScopeFiles)
	}

	// (2) The produced diff touching that same path still violates the
	// forbidden_paths gate — the fold gave it no special pass.
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: forbidden, Status: policy.StatusAdded}}}
	constraints := policy.Constraints{ForbiddenPaths: []string{".github/**"}}
	violations := policy.Evaluate(diff, constraints)
	if len(violations) == 0 {
		t.Fatalf("forbidden_paths gate did not fire on folded path %q — fold must NOT bypass the gate", forbidden)
	}
	if violations[0].Constraint != "forbidden_paths" {
		t.Errorf("violation = %q, want forbidden_paths", violations[0].Constraint)
	}
}

// TestGetStagePrompt_Implement_ApprovedScopeAmendmentsFolded crosses the
// prompt-side half of the #961 activation path: persisted scope_amendments
// rows -> resolveApprovedScopeAmendments -> mergeApprovedScopeAmendments ->
// promptResponse.ScopeFiles. Only APPROVED rows fold; pending and denied
// rows confer nothing; paths already in scope dedupe by path; an empty
// (plan-missing) scope stays empty.
func TestGetStagePrompt_Implement_ApprovedScopeAmendmentsFolded(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"

	seedAmendments := func(sa *fakeScopeAmendmentRepo, runID, stageID uuid.UUID) {
		approve := func(id uuid.UUID) {
			if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
				ID: id, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator",
			}); err != nil {
				t.Fatalf("approve: %v", err)
			}
		}
		// Approved: one net-new file, one modify, plus the already-
		// declared plan file (dedupe check).
		a, err := sa.Create(context.Background(), scopeamendment.CreateParams{
			RunID: runID, StageID: stageID,
			Paths: []scopeamendment.PathEntry{
				{Path: "backend/internal/server/newfile.go", Operation: scopeamendment.OperationCreate},
				{Path: "docs/extra.md", Operation: scopeamendment.OperationModify},
				{Path: plannedFile, Operation: scopeamendment.OperationModify},
			},
			Reason: "seam",
		})
		if err != nil {
			t.Fatal(err)
		}
		approve(a.ID)
		// Pending: must NOT fold.
		if _, err := sa.Create(context.Background(), scopeamendment.CreateParams{
			RunID: runID, StageID: stageID,
			Paths:  []scopeamendment.PathEntry{{Path: "pending/never.go", Operation: scopeamendment.OperationModify}},
			Reason: "pending",
		}); err != nil {
			t.Fatal(err)
		}
		// Denied: must NOT fold.
		d, err := sa.Create(context.Background(), scopeamendment.CreateParams{
			RunID: runID, StageID: stageID,
			Paths:  []scopeamendment.PathEntry{{Path: "denied/never.go", Operation: scopeamendment.OperationModify}},
			Reason: "denied",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
			ID: d.ID, Status: scopeamendment.StatusDenied, Reason: "no", DecidedBy: "github:operator",
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("approved paths folded, pending and denied excluded, deduped", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()
		sa := newFakeScopeAmendmentRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "scoped plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		seedAmendments(sa, runID, implStageID)

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:               "127.0.0.1:0",
			RunRepo:            rr,
			SigningRepo:        sf,
			ArtifactRepo:       art,
			ScopeAmendmentRepo: sa,
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		counts := map[string]int{}
		for _, f := range resp.ScopeFiles {
			counts[f.Path]++
		}
		for _, want := range []string{plannedFile, "backend/internal/server/newfile.go", "docs/extra.md"} {
			if counts[want] != 1 {
				t.Errorf("ScopeFiles[%q] count = %d, want exactly 1; got %#v", want, counts[want], resp.ScopeFiles)
			}
		}
		for _, never := range []string{"pending/never.go", "denied/never.go"} {
			if counts[never] != 0 {
				t.Errorf("ScopeFiles must not contain %q (undecided/denied)", never)
			}
		}
	})

	t.Run("plan-missing run is not augmented", func(t *testing.T) {
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()
		sa := newFakeScopeAmendmentRepo()

		runID := uuid.New()
		implStageID := uuid.New()

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {}}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		seedAmendments(sa, runID, implStageID)

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:               "127.0.0.1:0",
			RunRepo:            rr,
			SigningRepo:        sf,
			ArtifactRepo:       art,
			ScopeAmendmentRepo: sa,
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ScopeFiles != nil {
			t.Errorf("plan-missing run must keep empty scope (git add -A fallback), got %#v", resp.ScopeFiles)
		}
	})
}

// TestResolveApprovalConditions_ParentRunIDFallback covers the #978
// single-level ParentRunID fallback: a CI-retry / category-B recovery
// child (ParentRunID set, DecomposedFrom nil) has no plan gate of its
// own, so the parent's binding approve-with-conditions text must reach
// its implement prompt. Own-run entries win; the DecomposedFrom branch
// keeps precedence over ParentRunID; nil when neither yields anything.
func TestResolveApprovalConditions_ParentRunIDFallback(t *testing.T) {
	parentID := uuid.New()
	decompParentID := uuid.New()
	const ownCondition = "own: keep the adapter."
	const parentCondition = "parent: keep the recover endpoint idempotent."
	const decompCondition = "decomp: split per module."

	newSrv := func(byRun map[uuid.UUID][]*audit.Entry) *Server {
		return New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{byRunID: byRun}})
	}

	t.Run("own entries win over parent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			runID:    {makeApproveWithCommentEntry(runID, ownCondition)},
			parentID: {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if got == nil || *got != ownCondition {
			t.Errorf("got %v, want own condition", got)
		}
	})

	t.Run("parent inherited when own absent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			parentID: {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if got == nil || *got != parentCondition {
			t.Errorf("got %v, want parent condition", got)
		}
	})

	t.Run("DecomposedFrom precedence preserved", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			decompParentID: {makeApproveWithCommentEntry(decompParentID, decompCondition)},
			parentID:       {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got := s.resolveApprovalConditions(context.Background(), &run.Run{
			ID: runID, ParentRunID: &parentID, DecomposedFrom: &decompParentID,
		})
		if got == nil || *got != decompCondition {
			t.Errorf("got %v, want decomposition parent's condition", got)
		}
		// A decomposed child whose decomposition parent has no
		// conditions does NOT fall through to ParentRunID — the
		// decomposition branch terminates the lookup.
		s = newSrv(map[uuid.UUID][]*audit.Entry{
			parentID: {makeApproveWithCommentEntry(parentID, parentCondition)},
		})
		got = s.resolveApprovalConditions(context.Background(), &run.Run{
			ID: runID, ParentRunID: &parentID, DecomposedFrom: &decompParentID,
		})
		if got != nil {
			t.Errorf("got %v, want nil (decomposition branch terminates)", got)
		}
	})

	t.Run("nil when neither", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{})
		if got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID}); got != nil {
			t.Errorf("standalone run with no entries: got %v, want nil", got)
		}
		if got := s.resolveApprovalConditions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID}); got != nil {
			t.Errorf("parented run with no entries anywhere: got %v, want nil", got)
		}
	})
}

// TestResolveApprovalAddScopeFiles_ParentRunIDFallback mirrors the
// conditions fallback test for the #824 structured add_scope_files
// slice across the #978 ParentRunID boundary.
func TestResolveApprovalAddScopeFiles_ParentRunIDFallback(t *testing.T) {
	parentID := uuid.New()
	decompParentID := uuid.New()
	ownPaths := []string{"own/a.go"}
	parentPaths := []string{"parent/b.go", "parent/c.md"}
	decompPaths := []string{"decomp/d.go"}

	newSrv := func(byRun map[uuid.UUID][]*audit.Entry) *Server {
		return New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{byRunID: byRun}})
	}

	t.Run("own entries win over parent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			runID:    {makeApproveWithScopeFilesEntry(runID, ownPaths)},
			parentID: {makeApproveWithScopeFilesEntry(parentID, parentPaths)},
		})
		got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if !reflect.DeepEqual(got, ownPaths) {
			t.Errorf("got %v, want own paths %v", got, ownPaths)
		}
	})

	t.Run("parent inherited when own absent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			parentID: {makeApproveWithScopeFilesEntry(parentID, parentPaths)},
		})
		got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if !reflect.DeepEqual(got, parentPaths) {
			t.Errorf("got %v, want parent paths %v", got, parentPaths)
		}
	})

	t.Run("DecomposedFrom precedence preserved", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			decompParentID: {makeApproveWithScopeFilesEntry(decompParentID, decompPaths)},
			parentID:       {makeApproveWithScopeFilesEntry(parentID, parentPaths)},
		})
		got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{
			ID: runID, ParentRunID: &parentID, DecomposedFrom: &decompParentID,
		})
		if !reflect.DeepEqual(got, decompPaths) {
			t.Errorf("got %v, want decomposition parent's paths %v", got, decompPaths)
		}
	})

	t.Run("nil when neither", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{})
		if got := s.resolveApprovalAddScopeFiles(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

// makeApproveWithBindingAssertionsEntry builds an approval_submitted audit
// entry with decision=approve carrying the binding_assertions slice (#1171)
// that loadApprovalBindingAssertions reads back.
func makeApproveWithBindingAssertionsEntry(runID uuid.UUID, assertions []bindingAssertion) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"decision":           "approve",
		"binding_assertions": assertions,
	})
	rid := runID
	return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
}

// TestGetStagePrompt_Implement_BindingAssertionsEchoedOnResponse crosses the
// full #1171 read-back seam: persisted approval_submitted.binding_assertions ->
// resolveApprovalBindingAssertions -> promptResponse.BindingAssertions. The
// declared assertions must surface verbatim on the rendered implement-prompt
// response; a run with no declaration omits the field (byte-identical to today).
func TestGetStagePrompt_Implement_BindingAssertionsEchoedOnResponse(t *testing.T) {
	const plannedFile = "backend/internal/server/prompt.go"
	assertions := []bindingAssertion{
		{Type: "file_contains", Path: "backend/internal/yaml/pad.go", Literal: "pad: 3"},
		{Type: "test_asserts", Path: "backend/internal/yaml/pad_test.go", Literal: "TestPad"},
	}

	seed := func(t *testing.T, entries []*audit.Entry) promptResponse {
		t.Helper()
		rr := newPromptRunRepo()
		sf := newSigningFake()
		art := newFakeArtifactRepo()

		runID := uuid.New()
		planStageID := uuid.New()
		implStageID := uuid.New()

		p := &plan.Plan{
			PlanVersion:  "standard_v1",
			Summary:      "scoped plan",
			Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
			Scope: plan.Scope{
				Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
			},
		}
		planBytes, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		sv := "standard_v1"
		if _, err := art.Create(context.Background(), artifact.CreateParams{
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &sv,
			Content:       planBytes,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}

		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
			runID: {{ID: planStageID, RunID: runID, Type: run.StageTypePlan}},
		}
		rr.getRuns[runID] = &run.Run{
			ID:            runID,
			Repo:          "o/r",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
		}
		rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

		priv, _ := sf.issue(t, runID)
		s := New(Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      rr,
			SigningRepo:  sf,
			ArtifactRepo: art,
			AuditRepo:    &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{runID: entries}},
		})
		s.promptIssueGetterOverride = &stubIssueGetter{}

		w := promptRequest(t, s, runID, implStageID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	t.Run("declared assertions echoed on response", func(t *testing.T) {
		resp := seed(t, []*audit.Entry{makeApproveWithBindingAssertionsEntry(uuid.New(), assertions)})
		if !reflect.DeepEqual(resp.BindingAssertions, assertions) {
			t.Errorf("resp.BindingAssertions = %#v, want %#v", resp.BindingAssertions, assertions)
		}
	})

	t.Run("no declaration omits the field", func(t *testing.T) {
		// An approval_submitted entry with no binding_assertions key.
		entry := func() *audit.Entry {
			payload, _ := json.Marshal(map[string]any{"decision": "approve"})
			rid := uuid.New()
			return &audit.Entry{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}
		}()
		resp := seed(t, []*audit.Entry{entry})
		if resp.BindingAssertions != nil {
			t.Errorf("resp.BindingAssertions = %#v, want nil when none declared", resp.BindingAssertions)
		}
	})
}

// TestResolveApprovalBindingAssertions_ParentRunIDFallback mirrors the
// add_scope_files fallback test for the #1171 binding_assertions slice across
// the decomposition and #978 ParentRunID boundaries.
func TestResolveApprovalBindingAssertions_ParentRunIDFallback(t *testing.T) {
	parentID := uuid.New()
	decompParentID := uuid.New()
	own := []bindingAssertion{{Type: "file_contains", Path: "own/a.go", Literal: "x"}}
	parent := []bindingAssertion{{Type: "file_contains", Path: "parent/b.go", Literal: "y"}}
	decomp := []bindingAssertion{{Type: "test_asserts", Path: "decomp/d_test.go", Literal: "z"}}

	newSrv := func(byRun map[uuid.UUID][]*audit.Entry) *Server {
		return New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{byRunID: byRun}})
	}

	t.Run("own entries win over parent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			runID:    {makeApproveWithBindingAssertionsEntry(runID, own)},
			parentID: {makeApproveWithBindingAssertionsEntry(parentID, parent)},
		})
		got := s.resolveApprovalBindingAssertions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if !reflect.DeepEqual(got, own) {
			t.Errorf("got %v, want own %v", got, own)
		}
	})

	t.Run("parent inherited when own absent", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			parentID: {makeApproveWithBindingAssertionsEntry(parentID, parent)},
		})
		got := s.resolveApprovalBindingAssertions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID})
		if !reflect.DeepEqual(got, parent) {
			t.Errorf("got %v, want parent %v", got, parent)
		}
	})

	t.Run("DecomposedFrom precedence preserved", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{
			decompParentID: {makeApproveWithBindingAssertionsEntry(decompParentID, decomp)},
			parentID:       {makeApproveWithBindingAssertionsEntry(parentID, parent)},
		})
		got := s.resolveApprovalBindingAssertions(context.Background(), &run.Run{
			ID: runID, ParentRunID: &parentID, DecomposedFrom: &decompParentID,
		})
		if !reflect.DeepEqual(got, decomp) {
			t.Errorf("got %v, want decomposition parent's %v", got, decomp)
		}
	})

	t.Run("nil when neither", func(t *testing.T) {
		runID := uuid.New()
		s := newSrv(map[uuid.UUID][]*audit.Entry{})
		if got := s.resolveApprovalBindingAssertions(context.Background(), &run.Run{ID: runID, ParentRunID: &parentID}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

// seedFixupPromptServer wires a prompt server with one implement stage carrying
// a stage_fixup_triggered entry whose concerns are exactly `concerns`. Returns
// the decoded prompt response for the implement stage. Shared by the #1165
// apply-list serve tests.
func seedFixupPromptServer(t *testing.T, concerns []planreview.Concern) promptResponse {
	t.Helper()
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
		}},
	}
	planBytes, _ := json.Marshal(p)
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}
	auditByRun := map[uuid.UUID][]*audit.Entry{
		runID: {makeFixupEntry(runID, implStageID, concerns)},
	}
	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: sf, ArtifactRepo: art,
		AuditRepo: &feedbackAuditRepo{byRunID: auditByRun},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := promptRequest(t, s, runID, implStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// TestGetStagePrompt_FixupApplyPatches_ServedWhenAllPatched is the
// server-serves end of the #1165 seam: when EVERY routed concern carries a
// suggested_patch, the prompt response carries the apply-list in routing order.
func TestGetStagePrompt_FixupApplyPatches_ServedWhenAllPatched(t *testing.T) {
	resp := seedFixupPromptServer(t, []planreview.Concern{
		{Severity: planreview.SeverityMedium, Category: "scope", Note: "fix one", SuggestedPatch: "diff-one"},
		{Severity: planreview.SeverityLow, Category: "style", Note: "fix two", SuggestedPatch: "diff-two"},
	})
	if !resp.Fixup {
		t.Fatal("fixup = false, want true")
	}
	if len(resp.FixupApplyPatches) != 2 {
		t.Fatalf("FixupApplyPatches len = %d, want 2:\n%+v", len(resp.FixupApplyPatches), resp.FixupApplyPatches)
	}
	if resp.FixupApplyPatches[0].Patch != "diff-one" || resp.FixupApplyPatches[1].Patch != "diff-two" {
		t.Errorf("apply-list = %+v, want the two diffs in routing order", resp.FixupApplyPatches)
	}
}

// TestGetStagePrompt_FixupApplyPatches_OmittedWhenAnyMissing covers failure
// mode (d): a single patch-less routed concern makes the whole pass ineligible,
// so the apply-list is omitted and the runner takes the agent path.
func TestGetStagePrompt_FixupApplyPatches_OmittedWhenAnyMissing(t *testing.T) {
	resp := seedFixupPromptServer(t, []planreview.Concern{
		{Severity: planreview.SeverityMedium, Category: "scope", Note: "patched", SuggestedPatch: "diff-one"},
		{Severity: planreview.SeverityHigh, Category: "correctness", Note: "no patch"},
	})
	if !resp.Fixup {
		t.Fatal("fixup = false, want true (the trigger still routes concerns)")
	}
	if len(resp.FixupApplyPatches) != 0 {
		t.Errorf("FixupApplyPatches = %+v, want empty when a concern lacks a patch", resp.FixupApplyPatches)
	}
}

// acceptancePlanArtifactContent returns a standard_v1 plan JSON carrying two
// blocking acceptance criteria + an out_of_scope entry, used to seed the
// approved-plan artifact the acceptance-stage prompt reads.
func acceptancePlanArtifactContent(t *testing.T) json.RawMessage {
	t.Helper()
	blocking := true
	p := plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "Ship the widget endpoint.",
		Verification: plan.Verification{
			TestStrategy: "unit + integration",
			RollbackPlan: "revert the PR",
			AcceptanceCriteria: []plan.AcceptanceCriterion{
				{ID: "ac-create", Statement: "POST /widgets returns 201", Source: plan.CriterionSourceExplicit, SourceRef: "#1534", Blocking: &blocking},
				{ID: "ac-list", Statement: "GET /widgets lists widgets", Source: plan.CriterionSourceInferred, Rationale: "listing implied", Blocking: &blocking},
			},
			OutOfScope: []string{"deletion not covered"},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newAcceptancePromptServer wires a server for acceptance-stage prompt fetches:
// a plan stage + an acceptance stage on the run, an ArtifactRepo holding the
// approved plan artifact under the plan stage, the signing fake, and the issue
// stub. Returns the server, run id, acceptance stage id, the run's signing
// private key, and the issue stub.
func newAcceptancePromptServer(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey, *stubIssueGetter) {
	t.Helper()
	runID := uuid.New()
	planStageID := uuid.New()
	acceptanceStageID := uuid.New()

	rr := newPromptRunRepo()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	gh := &stubIssueGetter{issue: &githubclient.Issue{Number: 1534, Title: "Widget endpoint", Body: "we need a widget endpoint", State: "open"}}

	triggerRef := "issue:1534"
	installation := int64(7)
	runRow := &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateSucceeded}
	acceptanceStage := &run.Stage{ID: acceptanceStageID, RunID: runID, Type: run.StageTypeAcceptance, State: run.StageStateRunning}
	rr.getRuns[runID] = runRow
	rr.getStages[planStageID] = planStage
	rr.getStages[acceptanceStageID] = acceptanceStage
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage, acceptanceStage}}

	v := "standard_v1"
	ar.all = append(ar.all, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       acceptancePlanArtifactContent(t),
		CreatedAt:     time.Now().UTC(),
	})

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
	})
	s.promptIssueGetterOverride = gh
	priv, _ := sf.issue(t, runID)
	return s, runID, acceptanceStageID, priv, gh
}

// TestGetStagePrompt_Acceptance_PopulatesCriteria pins that the /prompt handler
// loads the approved plan's acceptance criteria for an acceptance stage and
// renders them, while withholding the diff / scope-files sections (ADR-049
// decision #4 independence).
func TestGetStagePrompt_Acceptance_PopulatesCriteria(t *testing.T) {
	s, runID, acceptanceStageID, priv, _ := newAcceptancePromptServer(t)
	w := promptRequest(t, s, runID, acceptanceStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StageType != "acceptance" {
		t.Errorf("StageType = %q, want acceptance", resp.StageType)
	}
	for _, want := range []string{"ac-create", "ac-list", "POST /widgets returns 201", "deletion not covered"} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("acceptance prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
	// Independence: no diff / scope-files sections.
	for _, banned := range []string{"Files in scope:", "### Diff under review"} {
		if strings.Contains(resp.Prompt, banned) {
			t.Errorf("acceptance prompt must not contain %q:\n%s", banned, resp.Prompt)
		}
	}
	// The E31.4 seam renders the not-declared line until #1532 lands.
	if !strings.Contains(resp.Prompt, "not declared in the workflow spec") {
		t.Errorf("acceptance prompt missing the target-URL not-declared line:\n%s", resp.Prompt)
	}
}

// TestGetStagePromptRender_Acceptance_PopulatesCriteria pins the SPA render
// handler carries the SAME acceptance criteria — the two handlers duplicate
// trigger construction, so the acceptance branch must land in both (or the SPA
// view silently diverges from the runner path).
func TestGetStagePromptRender_Acceptance_PopulatesCriteria(t *testing.T) {
	s, _, acceptanceStageID, _, _ := newAcceptancePromptServer(t)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", acceptanceStageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"ac-create", "ac-list", "not declared in the workflow spec"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered acceptance prompt missing %q\n---\n%s", want, body)
		}
	}
	if strings.Contains(body, "### Diff under review") {
		t.Errorf("rendered acceptance prompt must withhold the diff:\n%s", body)
	}
}

// TestGetStagePrompt_Acceptance_RendersDeclaredTargetHost pins the ACTIVATED
// E31.4/#1532 seam: a run whose workflow spec declares an acceptance-stage
// egress allowance renders the first target host in the prompt's Target
// instance section in full http(s) URL form (a schemeless host:port gains an
// http:// prefix, #1574), and the not-declared interim line is gone.
func TestGetStagePrompt_Acceptance_RendersDeclaredTargetHost(t *testing.T) {
	s, runID, acceptanceStageID, priv, _ := newAcceptancePromptServer(t)
	runRow, err := s.cfg.RunRepo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	runRow.WorkflowSpec = []byte(`
version: "1.3"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts:
            - staging.example.com:8443
            - second.example.com
`)
	w := promptRequest(t, s, runID, acceptanceStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Prompt, "Target instance URL: http://staging.example.com:8443") {
		t.Errorf("acceptance prompt missing the declared target host in http:// URL form:\n%s", resp.Prompt)
	}
	if strings.Contains(resp.Prompt, "not declared in the workflow spec") {
		t.Errorf("acceptance prompt still renders the not-declared line with a declared egress host:\n%s", resp.Prompt)
	}
}

// acceptanceEgressSpec is the workflow spec the E31.7 wire tests declare: an
// acceptance stage with a two-host egress allowance, so the FULL-list contract
// (vs the prompt text's first-host-only render) is distinguishable.
const acceptanceEgressSpec = `
version: "1.3"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts:
            - staging.example.com:8443
            - second.example.com
`

// TestGetStagePrompt_Acceptance_ServesEgressHostsAndCriteriaIDs pins the E31.7
// (#1535) acceptance-stage wire fields: egress_target_hosts carries ALL
// spec-declared hosts (the runner's ADR-050 proxy allow-list — not just the
// first host the prompt text renders) and acceptance_criteria_ids carries the
// approved plan's criterion ids in plan order (the runner's verdict join-key
// validation set). The raw-body checks pin the json tags byte-identical to the
// runner's upload.FetchedPrompt decoder.
func TestGetStagePrompt_Acceptance_ServesEgressHostsAndCriteriaIDs(t *testing.T) {
	s, runID, acceptanceStageID, priv, _ := newAcceptancePromptServer(t)
	runRow, err := s.cfg.RunRepo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	runRow.WorkflowSpec = []byte(acceptanceEgressSpec)
	w := promptRequest(t, s, runID, acceptanceStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := []string{"staging.example.com:8443", "second.example.com"}; !reflect.DeepEqual(resp.EgressTargetHosts, want) {
		t.Errorf("EgressTargetHosts = %v, want %v (ALL declared hosts)", resp.EgressTargetHosts, want)
	}
	if want := []string{"ac-create", "ac-list"}; !reflect.DeepEqual(resp.AcceptanceCriteriaIDs, want) {
		t.Errorf("AcceptanceCriteriaIDs = %v, want %v", resp.AcceptanceCriteriaIDs, want)
	}
	for _, tag := range []string{
		`"egress_target_hosts":["staging.example.com:8443","second.example.com"]`,
		`"acceptance_criteria_ids":["ac-create","ac-list"]`,
	} {
		if !strings.Contains(w.Body.String(), tag) {
			t.Errorf("response missing wire tag %s:\n%s", tag, w.Body.String())
		}
	}
}

// TestGetStagePrompt_Acceptance_NoEgressSpec_HostsOmitted pins the fail-closed
// posture for a spec with no egress block (or no spec at all): the
// egress_target_hosts field is OMITTED — the runner's proxy then admits only
// model + backend hosts — while acceptance_criteria_ids still serves the
// approved plan's ids.
func TestGetStagePrompt_Acceptance_NoEgressSpec_HostsOmitted(t *testing.T) {
	s, runID, acceptanceStageID, priv, _ := newAcceptancePromptServer(t)
	w := promptRequest(t, s, runID, acceptanceStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"egress_target_hosts":`) {
		t.Errorf("egress_target_hosts must be omitted for a no-egress spec:\n%s", w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := []string{"ac-create", "ac-list"}; !reflect.DeepEqual(resp.AcceptanceCriteriaIDs, want) {
		t.Errorf("AcceptanceCriteriaIDs = %v, want %v", resp.AcceptanceCriteriaIDs, want)
	}
}

// TestGetStagePrompt_Acceptance_ServesExpectedHeadSHA pins the E31.18 (#1569)
// merge-candidate wire field: an acceptance-stage prompt response carries
// acceptance_expected_head_sha resolved as the NEWEST head_sha across the
// run's reported-head ledger categories (here the fixup_pushed head, not the
// older pull_request_opened one — the same newest-entry pick as
// fixup_expected_head_sha, #967). The raw-body check pins the json tag
// byte-identical to the runner's upload.FetchedPrompt decoder.
func TestGetStagePrompt_Acceptance_ServesExpectedHeadSHA(t *testing.T) {
	s, runID, acceptanceStageID, priv, _ := newAcceptancePromptServer(t)
	au := s.cfg.AuditRepo.(*auditFake)
	au.seeded = append(au.seeded,
		makeReportedHeadEntry(runID, acceptanceStageID, "pull_request_opened",
			"aaaa000000000000000000000000000000000000", time.Now().Add(-2*time.Hour)),
		makeReportedHeadEntry(runID, acceptanceStageID, "fixup_pushed",
			"bbbb111111111111111111111111111111111111", time.Now().Add(-1*time.Hour)),
	)
	w := promptRequest(t, s, runID, acceptanceStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := "bbbb111111111111111111111111111111111111"; resp.AcceptanceExpectedHeadSHA != want {
		t.Errorf("acceptance_expected_head_sha = %q, want %q (the newest reported head)",
			resp.AcceptanceExpectedHeadSHA, want)
	}
	// Pin the wire tag byte-identical to the runner decoder.
	if want := `"acceptance_expected_head_sha":"bbbb111111111111111111111111111111111111"`; !strings.Contains(w.Body.String(), want) {
		t.Errorf("response missing wire tag %s:\n%s", want, w.Body.String())
	}
}

// TestGetStagePrompt_Acceptance_EmptyLedger_ExpectedHeadSHAOmitted pins the
// WARN-and-omit posture: an acceptance stage on a run with NO reported-head
// ledger entries omits acceptance_expected_head_sha entirely (omitempty), so
// the runner's identity gate degrades to unverifiable-warn rather than
// comparing against an empty expectation.
func TestGetStagePrompt_Acceptance_EmptyLedger_ExpectedHeadSHAOmitted(t *testing.T) {
	s, runID, acceptanceStageID, priv, _ := newAcceptancePromptServer(t)
	w := promptRequest(t, s, runID, acceptanceStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"acceptance_expected_head_sha":`) {
		t.Errorf("acceptance_expected_head_sha must be omitted on an empty reported-head ledger:\n%s", w.Body.String())
	}
}

// TestGetStagePromptRender_Acceptance_ServesExpectedHeadSHA pins the SPA
// render handler carries the SAME merge-candidate field — the two handlers
// duplicate response construction, so the E31.18 resolution must land in both
// (or the rendered view silently diverges from the runner path).
func TestGetStagePromptRender_Acceptance_ServesExpectedHeadSHA(t *testing.T) {
	s, runID, acceptanceStageID, _, _ := newAcceptancePromptServer(t)
	au := s.cfg.AuditRepo.(*auditFake)
	au.seeded = append(au.seeded,
		makeReportedHeadEntry(runID, acceptanceStageID, "pull_request_opened",
			"cccc222222222222222222222222222222222222", time.Now().Add(-1*time.Hour)),
	)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", acceptanceStageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if want := `"acceptance_expected_head_sha":"cccc222222222222222222222222222222222222"`; !strings.Contains(w.Body.String(), want) {
		t.Errorf("rendered response missing wire tag %s:\n%s", want, w.Body.String())
	}
}

// TestGetStagePrompt_NonAcceptanceStages_OmitAcceptanceFields pins the
// omitempty contract: plan and implement prompt responses carry NEITHER
// egress_target_hosts NOR acceptance_criteria_ids (nor the E31.18
// acceptance_expected_head_sha), so every pre-E31.7 response
// stays byte-identical.
func TestGetStagePrompt_NonAcceptanceStages_OmitAcceptanceFields(t *testing.T) {
	for _, stageType := range []run.StageType{run.StageTypePlan, run.StageTypeImplement} {
		t.Run(string(stageType), func(t *testing.T) {
			s, rr, sf, gh := newPromptServer(t)
			runID, stageID := uuid.New(), uuid.New()
			priv, _ := sf.issue(t, runID)
			installation := int64(99)
			triggerRef := "issue:42"
			rr.runRow = &run.Run{
				ID:             runID,
				Repo:           "kuhlman-labs/example",
				WorkflowID:     "feature_change",
				TriggerSource:  run.TriggerGitHubIssue,
				TriggerRef:     &triggerRef,
				InstallationID: &installation,
			}
			rr.stage = &run.Stage{ID: stageID, RunID: runID, Type: stageType}
			gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "Body text", State: "open"}
			w := promptRequest(t, s, runID, stageID, priv, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			for _, banned := range []string{`"egress_target_hosts":`, `"acceptance_criteria_ids":`, `"acceptance_expected_head_sha":`} {
				if strings.Contains(w.Body.String(), banned) {
					t.Errorf("%s response must not carry %s:\n%s", stageType, banned, w.Body.String())
				}
			}
		})
	}
}

// TestGetStagePromptRender_Acceptance_ServesEgressHostsAndCriteriaIDs pins the
// SPA render handler carries the SAME acceptance wire fields — the two
// handlers duplicate response construction, so the acceptance block must land
// in both (or the rendered view silently diverges from the runner path).
func TestGetStagePromptRender_Acceptance_ServesEgressHostsAndCriteriaIDs(t *testing.T) {
	s, runID, acceptanceStageID, _, _ := newAcceptancePromptServer(t)
	runRow, err := s.cfg.RunRepo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	runRow.WorkflowSpec = []byte(acceptanceEgressSpec)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", acceptanceStageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := []string{"staging.example.com:8443", "second.example.com"}; !reflect.DeepEqual(resp.EgressTargetHosts, want) {
		t.Errorf("EgressTargetHosts = %v, want %v", resp.EgressTargetHosts, want)
	}
	if want := []string{"ac-create", "ac-list"}; !reflect.DeepEqual(resp.AcceptanceCriteriaIDs, want) {
		t.Errorf("AcceptanceCriteriaIDs = %v, want %v", resp.AcceptanceCriteriaIDs, want)
	}
}
