package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// captureDispatcherLogs wires a JSON slog handler into d so tests
// can assert on the structured lines Handle emits.
func captureDispatcherLogs(d *Dispatcher) *bytes.Buffer {
	var buf bytes.Buffer
	d.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// --- MatchEvent table tests ---

func TestMatchEvent_BotSenderSkips(t *testing.T) {
	ev := Event{
		Type: "issues", Action: "labeled",
		Sender: "dependabot[bot]", SenderType: "Bot",
		InstallationID: 42, RawBody: []byte(`{}`),
	}
	got := MatchEvent(ev)
	if !got.Skip || !strings.Contains(got.Reason, "bot") {
		t.Errorf("got = %+v, want skip with bot reason", got)
	}
}

func TestMatchEvent_NoInstallationSkips(t *testing.T) {
	ev := Event{Type: "issues", Action: "labeled", InstallationID: 0}
	got := MatchEvent(ev)
	if !got.Skip || !strings.Contains(got.Reason, "installation") {
		t.Errorf("got = %+v, want skip with installation reason", got)
	}
}

func TestMatchEvent_UnrecognizedTypeSkips(t *testing.T) {
	ev := Event{Type: "deployment_status", InstallationID: 1, RawBody: []byte(`{}`)}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip", got)
	}
}

func TestMatchEvent_IssuesLabeled_FishhawkLabel(t *testing.T) {
	body := []byte(`{
		"issue": {"number": 1247},
		"label": {"name": "fishhawk"}
	}`)
	ev := Event{
		Type: "issues", Action: "labeled",
		InstallationID: 42, RawBody: body,
	}
	got := MatchEvent(ev)
	if got.Skip {
		t.Fatalf("got skip, want dispatch: %+v", got)
	}
	if got.WorkflowID != DefaultWorkflowID {
		t.Errorf("WorkflowID = %q", got.WorkflowID)
	}
	if got.TriggerSource != run.TriggerGitHubIssue {
		t.Errorf("TriggerSource = %q", got.TriggerSource)
	}
	if got.TriggerRef != "issue:1247" {
		t.Errorf("TriggerRef = %q", got.TriggerRef)
	}
	if got.IssueRef == nil || got.IssueRef.Number != 1247 {
		t.Errorf("IssueRef = %+v", got.IssueRef)
	}
}

func TestMatchEvent_IssuesLabeled_LabelMatchIsCaseInsensitive(t *testing.T) {
	body := []byte(`{"issue":{"number":1},"label":{"name":"FishHawk"}}`)
	ev := Event{Type: "issues", Action: "labeled", InstallationID: 1, RawBody: body}
	if MatchEvent(ev).Skip {
		t.Error("expected case-insensitive label match to dispatch")
	}
}

func TestMatchEvent_IssuesLabeled_OtherLabelSkips(t *testing.T) {
	body := []byte(`{"issue":{"number":1},"label":{"name":"bug"}}`)
	ev := Event{Type: "issues", Action: "labeled", InstallationID: 1, RawBody: body}
	got := MatchEvent(ev)
	if !got.Skip || !strings.Contains(got.Reason, "fishhawk") {
		t.Errorf("got = %+v", got)
	}
}

func TestMatchEvent_IssuesNonLabeledActionSkips(t *testing.T) {
	body := []byte(`{"issue":{"number":1},"label":{"name":"fishhawk"}}`)
	ev := Event{Type: "issues", Action: "opened", InstallationID: 1, RawBody: body}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip on non-labeled action", got)
	}
}

func TestMatchEvent_IssuesPayloadParseFailureSkips(t *testing.T) {
	ev := Event{Type: "issues", Action: "labeled", InstallationID: 1, RawBody: []byte("{not json")}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v", got)
	}
}

func TestMatchEvent_IssuesMissingNumberSkips(t *testing.T) {
	body := []byte(`{"label":{"name":"fishhawk"}}`)
	ev := Event{Type: "issues", Action: "labeled", InstallationID: 1, RawBody: body}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v", got)
	}
}

func TestMatchEvent_IssueComment_Created_TriggerCommand(t *testing.T) {
	body := []byte(`{
		"comment": {"body": "/fishhawk run\n\nplease"},
		"issue":   {"number": 1247}
	}`)
	ev := Event{Type: "issue_comment", Action: "created", InstallationID: 42, RawBody: body}
	got := MatchEvent(ev)
	if got.Skip {
		t.Fatalf("got = %+v, want dispatch", got)
	}
	if got.TriggerRef != "issue:1247" {
		t.Errorf("TriggerRef = %q", got.TriggerRef)
	}
	if got.IssueRef == nil || got.IssueRef.Number != 1247 {
		t.Errorf("IssueRef = %+v", got.IssueRef)
	}
}

func TestMatchEvent_IssueComment_NoTriggerCommandSkips(t *testing.T) {
	body := []byte(`{"comment":{"body":"just chatting"},"issue":{"number":1}}`)
	ev := Event{Type: "issue_comment", Action: "created", InstallationID: 1, RawBody: body}
	got := MatchEvent(ev)
	if !got.Skip || !strings.Contains(got.Reason, "/fishhawk run") {
		t.Errorf("got = %+v", got)
	}
}

func TestMatchEvent_IssueComment_EditedSkips(t *testing.T) {
	body := []byte(`{"comment":{"body":"/fishhawk run"},"issue":{"number":1}}`)
	ev := Event{Type: "issue_comment", Action: "edited", InstallationID: 1, RawBody: body}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip on edit", got)
	}
}

func TestMatchEvent_IssueComment_ApproveCommand(t *testing.T) {
	body := []byte(`{"comment":{"body":"/fishhawk approve"},"issue":{"number":1247}}`)
	ev := Event{Type: "issue_comment", Action: "created", InstallationID: 42, RawBody: body}
	got := MatchEvent(ev)
	if got.Skip {
		t.Fatalf("got = %+v, want match", got)
	}
	if got.Action != MatchActionApprove {
		t.Errorf("Action = %q, want approve", got.Action)
	}
	if got.IssueRef == nil || got.IssueRef.Number != 1247 {
		t.Errorf("IssueRef = %+v", got.IssueRef)
	}
	if got.CommentBody != "" {
		t.Errorf("CommentBody = %q, want empty", got.CommentBody)
	}
	if got.WorkflowID != "" {
		t.Errorf("WorkflowID should be empty for approve action; got %q", got.WorkflowID)
	}
}

func TestMatchEvent_IssueComment_RejectCommand_WithReason(t *testing.T) {
	body := []byte(`{"comment":{"body":"/fishhawk reject\n\nthe scope is too wide"},"issue":{"number":1247}}`)
	ev := Event{Type: "issue_comment", Action: "created", InstallationID: 42, RawBody: body}
	got := MatchEvent(ev)
	if got.Skip {
		t.Fatalf("got = %+v, want match", got)
	}
	if got.Action != MatchActionReject {
		t.Errorf("Action = %q, want reject", got.Action)
	}
	if got.CommentBody != "the scope is too wide" {
		t.Errorf("CommentBody = %q", got.CommentBody)
	}
}

func TestMatchEvent_IssueComment_RunCommand_TaggedAction(t *testing.T) {
	body := []byte(`{"comment":{"body":"/fishhawk run"},"issue":{"number":1247}}`)
	ev := Event{Type: "issue_comment", Action: "created", InstallationID: 42, RawBody: body}
	got := MatchEvent(ev)
	if got.Skip {
		t.Fatalf("got = %+v, want match", got)
	}
	if got.Action != MatchActionRun {
		t.Errorf("Action = %q, want run", got.Action)
	}
	if got.WorkflowID != DefaultWorkflowID {
		t.Errorf("WorkflowID = %q", got.WorkflowID)
	}
}

func TestMatchEvent_IssueComment_NonCommandBodyMentioningCommand_Skips(t *testing.T) {
	// Quoted-reply guard: "/fishhawk approve" inside prose must not
	// classify as a command. Only commands at the start of the
	// trimmed body fire.
	body := []byte(`{"comment":{"body":"Should I /fishhawk approve here?"},"issue":{"number":1}}`)
	ev := Event{Type: "issue_comment", Action: "created", InstallationID: 1, RawBody: body}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip", got)
	}
}

func TestMatchEvent_IssueComment_NotMistakenForLongerCommandPrefix(t *testing.T) {
	// "/fishhawk runner" is not "/fishhawk run" — the next byte
	// after the command word must be whitespace or end-of-string.
	body := []byte(`{"comment":{"body":"/fishhawk runner"},"issue":{"number":1}}`)
	ev := Event{Type: "issue_comment", Action: "created", InstallationID: 1, RawBody: body}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip", got)
	}
}

// --- MatchEvent: workflow_run ---

func workflowRunBody(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	defaults := map[string]any{
		"id":          int64(987654321),
		"path":        ".github/workflows/fishhawk.yml",
		"conclusion":  "failure",
		"status":      "completed",
		"event":       "workflow_dispatch",
		"head_branch": "main",
		"head_sha":    "abc123",
	}
	for k, v := range fields {
		defaults[k] = v
	}
	body, _ := json.Marshal(map[string]any{"workflow_run": defaults})
	return body
}

func TestMatchEvent_WorkflowRun_FailureCompleted_Matches(t *testing.T) {
	ev := Event{
		Type: "workflow_run", Action: "completed", InstallationID: 42,
		RawBody: workflowRunBody(t, nil),
	}
	got := MatchEvent(ev)
	if got.Skip {
		t.Fatalf("got = %+v, want match", got)
	}
	if got.Action != MatchActionRunnerActionFailed {
		t.Errorf("Action = %q, want runner_action_failed", got.Action)
	}
	if got.WorkflowRunID != 987654321 {
		t.Errorf("WorkflowRunID = %d", got.WorkflowRunID)
	}
	if got.WorkflowRunConclusion != "failure" {
		t.Errorf("WorkflowRunConclusion = %q", got.WorkflowRunConclusion)
	}
}

func TestMatchEvent_WorkflowRun_NonTerminalAction_Skips(t *testing.T) {
	ev := Event{
		Type: "workflow_run", Action: "in_progress", InstallationID: 42,
		RawBody: workflowRunBody(t, nil),
	}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip on non-terminal action", got)
	}
}

func TestMatchEvent_WorkflowRun_SuccessConclusion_Skips(t *testing.T) {
	ev := Event{
		Type: "workflow_run", Action: "completed", InstallationID: 42,
		RawBody: workflowRunBody(t, map[string]any{"conclusion": "success"}),
	}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip on successful run", got)
	}
}

func TestMatchEvent_WorkflowRun_OtherConclusionsSkip(t *testing.T) {
	for _, c := range []string{"neutral", "skipped", "", "unknown_future_value"} {
		t.Run(c, func(t *testing.T) {
			ev := Event{
				Type: "workflow_run", Action: "completed", InstallationID: 42,
				RawBody: workflowRunBody(t, map[string]any{"conclusion": c}),
			}
			got := MatchEvent(ev)
			if !got.Skip {
				t.Errorf("conclusion=%q got = %+v, want skip", c, got)
			}
		})
	}
}

func TestMatchEvent_WorkflowRun_FailedConclusionsMatch(t *testing.T) {
	for _, c := range []string{"failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale"} {
		t.Run(c, func(t *testing.T) {
			ev := Event{
				Type: "workflow_run", Action: "completed", InstallationID: 42,
				RawBody: workflowRunBody(t, map[string]any{"conclusion": c}),
			}
			got := MatchEvent(ev)
			if got.Skip {
				t.Errorf("conclusion=%q should match; got skip: %s", c, got.Reason)
			}
			if got.WorkflowRunConclusion != c {
				t.Errorf("WorkflowRunConclusion = %q, want %q", got.WorkflowRunConclusion, c)
			}
		})
	}
}

func TestMatchEvent_WorkflowRun_OtherWorkflowFile_Skips(t *testing.T) {
	// A failing CI workflow on the same repo isn't Fishhawk's
	// concern — only the runner action's failures route here.
	ev := Event{
		Type: "workflow_run", Action: "completed", InstallationID: 42,
		RawBody: workflowRunBody(t, map[string]any{"path": ".github/workflows/ci.yml"}),
	}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip for non-fishhawk workflow", got)
	}
}

func TestMatchEvent_WorkflowRun_NonDispatchEvent_Skips(t *testing.T) {
	// A `push`-triggered run of fishhawk.yml (e.g. the customer
	// committed something that triggered the workflow on their
	// own) isn't ours to react to.
	ev := Event{
		Type: "workflow_run", Action: "completed", InstallationID: 42,
		RawBody: workflowRunBody(t, map[string]any{"event": "push"}),
	}
	got := MatchEvent(ev)
	if !got.Skip {
		t.Errorf("got = %+v, want skip for non-dispatch event", got)
	}
}

// --- Dispatcher.Handle tests with stubs ---

// stubGitHub is a minimal GitHubAPI for handler tests. Each call
// returns the configured response or err; the recorder fields let
// tests assert on the request shape.
type stubGitHub struct {
	mu sync.Mutex

	specContent  []byte
	specSHA      string
	specErr      error
	dispatchErr  error
	dispatchCall struct {
		repo githubclient.RepoRef
		file string
		ref  string
		args githubclient.DispatchInputs
	}
	specCalls     int
	dispatchCalls int

	// workflowRun is what GetWorkflowRun returns; nil + non-nil err
	// when the lookup should fail.
	workflowRun     *githubclient.WorkflowRun
	workflowRunErr  error
	workflowRunCall struct {
		runID int64
	}
	workflowRunCalls int
}

func (s *stubGitHub) GetWorkflowSpec(_ context.Context, _ int64,
	_ githubclient.RepoRef, _ string) (*githubclient.FileContent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specCalls++
	if s.specErr != nil {
		return nil, s.specErr
	}
	return &githubclient.FileContent{Content: s.specContent, SHA: s.specSHA}, nil
}

func (s *stubGitHub) DispatchWorkflow(_ context.Context, _ int64,
	repo githubclient.RepoRef, file, ref string, args githubclient.DispatchInputs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatchCalls++
	s.dispatchCall.repo = repo
	s.dispatchCall.file = file
	s.dispatchCall.ref = ref
	s.dispatchCall.args = args
	return s.dispatchErr
}

func (s *stubGitHub) GetWorkflowRun(_ context.Context, _ int64,
	_ githubclient.RepoRef, runID int64) (*githubclient.WorkflowRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflowRunCalls++
	s.workflowRunCall.runID = runID
	if s.workflowRunErr != nil {
		return nil, s.workflowRunErr
	}
	return s.workflowRun, nil
}

// stubRuns is a tiny in-memory run.Repository covering the
// methods Dispatcher.Handle uses: CreateRun, CreateStage,
// TransitionStage. Other methods stay "not used" so accidental
// reads in the dispatcher path are loud.
type stubRuns struct {
	mu             sync.Mutex
	created        []*run.Run
	createdStages  []*run.Stage
	transitions    []stubStageTransition
	createErr      error
	createStageErr error
	transitionErr  error
}

type stubStageTransition struct {
	StageID uuid.UUID
	To      run.StageState
}

func (s *stubRuns) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return nil, s.createErr
	}
	r := &run.Run{
		ID:            uuid.New(),
		Repo:          p.Repo,
		WorkflowID:    p.WorkflowID,
		WorkflowSHA:   p.WorkflowSHA,
		TriggerSource: p.TriggerSource,
		TriggerRef:    p.TriggerRef,
		ParentRunID:   p.ParentRunID,
		State:         run.StatePending,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	s.created = append(s.created, r)
	return r, nil
}

func (s *stubRuns) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createStageErr != nil {
		return nil, s.createStageErr
	}
	st := &run.Stage{
		ID:               uuid.New(),
		RunID:            p.RunID,
		Sequence:         p.Sequence,
		Type:             p.Type,
		ExecutorKind:     p.ExecutorKind,
		ExecutorRef:      p.ExecutorRef,
		State:            run.StageStatePending,
		GateSLA:          p.GateSLA,
		RequiresApproval: p.RequiresApproval,
		Gate:             p.Gate,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	s.createdStages = append(s.createdStages, st)
	return st, nil
}

func (s *stubRuns) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	s.transitions = append(s.transitions, stubStageTransition{StageID: id, To: to})
	for _, st := range s.createdStages {
		if st.ID == id {
			st.State = to
			return st, nil
		}
	}
	return nil, run.ErrNotFound
}

func (s *stubRuns) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (s *stubRuns) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*run.Run, 0, len(s.created))
	// Mirror the SQL: created_at DESC tiebreak by id DESC. The
	// dispatcher's parent-finder reads the first non-terminal row,
	// so newest-first ordering matters. Tests append in order so
	// reverse iteration is enough.
	for i := len(s.created) - 1; i >= 0; i-- {
		r := s.created[i]
		if f.Repo != "" && r.Repo != f.Repo {
			continue
		}
		if f.TriggerRef != nil {
			if r.TriggerRef == nil || *r.TriggerRef != *f.TriggerRef {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}
func (s *stubRuns) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.createdStages {
		if st.ID == id {
			return st, nil
		}
	}
	return nil, run.ErrNotFound
}
func (s *stubRuns) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (s *stubRuns) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (s *stubRuns) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// stubAudit captures every AppendChained call so tests can assert
// audit-entry shape and category.
type stubAudit struct {
	mu        sync.Mutex
	appended  []audit.ChainAppendParams
	appendErr error
}

func (s *stubAudit) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (s *stubAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return nil, s.appendErr
	}
	s.appended = append(s.appended, p)
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}

func (s *stubAudit) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (s *stubAudit) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (s *stubAudit) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (s *stubAudit) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (s *stubAudit) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (s *stubAudit) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (s *stubAudit) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}

// validSpec is the canonical workflow YAML used in dispatcher
// tests. Mirrors MVP_SPEC §4.2 in shape but with minimal content.
const validSpec = `version: "0.1"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Test workflow
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            sla: 4_business_hours
`

func newDispatcherWithStubs(t *testing.T) (*Dispatcher, *stubGitHub, *stubRuns, *stubAudit) {
	t.Helper()
	gh := &stubGitHub{
		specContent: []byte(validSpec),
		specSHA:     "feedf00d",
	}
	runs := &stubRuns{}
	au := &stubAudit{}
	d := &Dispatcher{
		GitHub: gh,
		Runs:   runs,
		Audit:  au,
		Now:    func() time.Time { return time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC) },
	}
	return d, gh, runs, au
}

func issueLabeledEvent(t *testing.T) Event {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"action": "labeled",
		"issue":  map[string]any{"number": 1247},
		"label":  map[string]any{"name": "fishhawk"},
		"repository": map[string]any{
			"full_name": "kuhlman-labs/fishhawk",
		},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": "alice", "type": "User"},
	})
	ev, err := ParseEvent("issues", "deliv-1", body)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestHandle_HappyPath_CreatesRunAndDispatches(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	logs := captureDispatcherLogs(d)

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gh.specCalls != 1 {
		t.Errorf("spec calls = %d, want 1", gh.specCalls)
	}
	if gh.dispatchCalls != 1 {
		t.Errorf("dispatch calls = %d, want 1", gh.dispatchCalls)
	}
	if gh.dispatchCall.file != DefaultActionsWorkflowFile {
		t.Errorf("workflow file = %q", gh.dispatchCall.file)
	}
	if gh.dispatchCall.ref != "main" {
		t.Errorf("ref = %q, want main (default)", gh.dispatchCall.ref)
	}
	if gh.dispatchCall.repo.Owner != "kuhlman-labs" || gh.dispatchCall.repo.Name != "fishhawk" {
		t.Errorf("repo = %+v", gh.dispatchCall.repo)
	}
	if gh.dispatchCall.args["workflow_id"] != "feature_change" {
		t.Errorf("inputs.workflow_id = %q", gh.dispatchCall.args["workflow_id"])
	}
	if _, err := uuid.Parse(gh.dispatchCall.args["run_id"]); err != nil {
		t.Errorf("inputs.run_id not UUID: %v", err)
	}

	if len(runs.created) != 1 {
		t.Fatalf("runs created = %d, want 1", len(runs.created))
	}
	got := runs.created[0]
	if got.WorkflowSHA != "feedf00d" {
		t.Errorf("WorkflowSHA = %q", got.WorkflowSHA)
	}
	if got.TriggerSource != run.TriggerGitHubIssue {
		t.Errorf("TriggerSource = %q", got.TriggerSource)
	}
	if got.TriggerRef == nil || *got.TriggerRef != "issue:1247" {
		t.Errorf("TriggerRef = %v", got.TriggerRef)
	}

	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if au.appended[0].Category != "run_dispatched" {
		t.Errorf("audit category = %q", au.appended[0].Category)
	}
	if !strings.Contains(string(au.appended[0].Payload), `"outcome":"dispatched"`) {
		t.Errorf("audit payload outcome wrong: %s", au.appended[0].Payload)
	}

	out := logs.String()
	for _, want := range []string{
		`"level":"INFO"`,
		`"msg":"webhook dispatched"`,
		`"event":"issues"`,
		`"action":"labeled"`,
		`"delivery_id":"deliv-1"`,
		`"repo":"kuhlman-labs/fishhawk"`,
		`"workflow_id":"feature_change"`,
		`"run_id":"` + got.ID.String() + `"`,
		`"stage_id":"` + runs.createdStages[0].ID.String() + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("success log missing %s:\n%s", want, out)
		}
	}
}

func TestHandle_HappyPath_CreatesStagesAndDispatchesFirst(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// One stage created (the validSpec defines plan only).
	if len(runs.createdStages) != 1 {
		t.Fatalf("stages = %d, want 1", len(runs.createdStages))
	}
	st := runs.createdStages[0]
	if st.Sequence != 0 {
		t.Errorf("Sequence = %d, want 0", st.Sequence)
	}
	if st.Type != run.StageTypePlan {
		t.Errorf("Type = %q, want plan", st.Type)
	}
	if st.ExecutorKind != run.ExecutorAgent || st.ExecutorRef != "claude-code" {
		t.Errorf("Executor = %q/%q", st.ExecutorKind, st.ExecutorRef)
	}

	// dispatch_inputs carry the stage UUID.
	if gh.dispatchCall.args["stage_id"] != st.ID.String() {
		t.Errorf("stage_id input = %q, want %q",
			gh.dispatchCall.args["stage_id"], st.ID)
	}
	if gh.dispatchCall.args["run_id"] == "" {
		t.Error("run_id input missing")
	}

	// The first stage transitioned to dispatched after workflow_dispatch.
	if len(runs.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(runs.transitions))
	}
	tr := runs.transitions[0]
	if tr.StageID != st.ID {
		t.Errorf("transitioned stage = %s, want %s", tr.StageID, st.ID)
	}
	if tr.To != run.StageStateDispatched {
		t.Errorf("transition to = %q, want dispatched", tr.To)
	}
}

func TestHandle_MultiStageSpec_OnlyFirstDispatched(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.specContent = []byte(`version: "0.1"
roles:
  tech_lead:
    members: ["@x"]
workflows:
  feature_change:
    description: multi-stage
    stages:
      - id: plan
        type: plan
        executor: {agent: claude-code}
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers: {any_of: [tech_lead]}
            sla: 4_business_hours
      - id: implement
        type: implement
        executor: {agent: claude-code}
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
        constraints:
          - max_files_changed: 30
      - id: review
        type: review
        executor: {human: true}
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvers: {any_of: [tech_lead]}
`)

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// All three stages exist.
	if len(runs.createdStages) != 3 {
		t.Fatalf("stages = %d, want 3", len(runs.createdStages))
	}
	// Sequence 0 is plan (agent), 1 is implement (agent), 2 is review (human).
	want := []struct {
		seq      int
		typ      run.StageType
		execKind run.ExecutorKind
	}{
		{0, run.StageTypePlan, run.ExecutorAgent},
		{1, run.StageTypeImplement, run.ExecutorAgent},
		{2, run.StageTypeReview, run.ExecutorHuman},
	}
	for i, w := range want {
		got := runs.createdStages[i]
		if got.Sequence != w.seq || got.Type != w.typ || got.ExecutorKind != w.execKind {
			t.Errorf("stage %d: got (seq=%d type=%q kind=%q), want (seq=%d type=%q kind=%q)",
				i, got.Sequence, got.Type, got.ExecutorKind, w.seq, w.typ, w.execKind)
		}
	}

	// Only the FIRST stage was transitioned.
	if len(runs.transitions) != 1 {
		t.Errorf("transitions = %d, want 1 (only first stage)", len(runs.transitions))
	}
	if runs.transitions[0].StageID != runs.createdStages[0].ID {
		t.Errorf("transitioned stage = %s, want first stage %s",
			runs.transitions[0].StageID, runs.createdStages[0].ID)
	}
}

func TestHandle_HumanStage_ExecutorRefIsConventional(t *testing.T) {
	// Two-stage spec: implement (agent) → review (human). The review
	// stage's from_stage reference must resolve, so we keep both
	// stages and assert the human one's executor mapping.
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.specContent = []byte(`version: "0.1"
roles:
  tech_lead:
    members: ["@x"]
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor: {agent: claude-code}
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: pull_request
        constraints:
          - max_files_changed: 30
      - id: review
        type: review
        executor: {human: true}
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvers: {any_of: [tech_lead]}
`)
	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.createdStages) != 2 {
		t.Fatalf("stages = %d, want 2", len(runs.createdStages))
	}
	human := runs.createdStages[1]
	if human.ExecutorKind != run.ExecutorHuman || human.ExecutorRef != "human" {
		t.Errorf("human stage executor = %q/%q, want human/human",
			human.ExecutorKind, human.ExecutorRef)
	}
}

func TestHandle_StageCreateError_ReturnsErrorForRetry(t *testing.T) {
	d, _, runs, au := newDispatcherWithStubs(t)
	runs.createStageErr = errors.New("db down")

	err := d.Handle(context.Background(), issueLabeledEvent(t))
	if err == nil {
		t.Fatal("expected error on stage create failure")
	}
	// Run was created but stages weren't — that's a transient state
	// the next retry will fix (CreateStage is idempotent enough at
	// the SQL layer that the retry repopulates).
	if len(runs.created) != 1 {
		t.Errorf("run created = %d, want 1", len(runs.created))
	}
	// No audit row because we didn't reach the dispatch step.
	if len(au.appended) != 0 {
		t.Errorf("audit entries = %d, want 0", len(au.appended))
	}
}

func TestHandle_TransitionFailure_DoesntFailDispatch(t *testing.T) {
	// The dispatch already fired; failing to transition the stage
	// to dispatched is a non-fatal state-machine issue we log but
	// don't unwind on. The runner picks the stage up either way.
	d, gh, runs, au := newDispatcherWithStubs(t)
	runs.transitionErr = errors.New("state machine refusal")

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gh.dispatchCalls != 1 {
		t.Errorf("dispatch calls = %d, want 1 (transition error mustn't unwind)", gh.dispatchCalls)
	}
	// Audit still records the dispatch outcome.
	if len(au.appended) != 1 {
		t.Errorf("audit entries = %d, want 1", len(au.appended))
	}
}

func TestHandle_EmptyStagesSpec_NoRun(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	// A workflow with no stages — schema requires at least one,
	// but defense-in-depth: the dispatcher refuses rather than
	// dispatching with no work to do.
	gh.specContent = []byte(`version: "0.1"
workflows:
  feature_change:
    description: empty
    stages: []
`)
	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("created run despite empty stages: %d", len(runs.created))
	}
}

func TestHandle_DispatchError_AuditsFailureCategory(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	logs := captureDispatcherLogs(d)
	gh.dispatchErr = errors.New("github 422: bad ref")

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 1 {
		t.Errorf("run still created on dispatch failure (correct): %d", len(runs.created))
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit = %d, want 1", len(au.appended))
	}
	if !strings.Contains(string(au.appended[0].Payload), `"outcome":"dispatch_failed"`) {
		t.Errorf("payload missing failure outcome: %s", au.appended[0].Payload)
	}

	out := logs.String()
	for _, want := range []string{
		`"level":"WARN"`,
		`"msg":"webhook dispatch failed"`,
		`"event":"issues"`,
		`"action":"labeled"`,
		`"delivery_id":"deliv-1"`,
		`"repo":"kuhlman-labs/fishhawk"`,
		`"workflow_id":"feature_change"`,
		`"run_id":"` + runs.created[0].ID.String() + `"`,
		`"stage_id":"` + runs.createdStages[0].ID.String() + `"`,
		`"error":"github 422: bad ref"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("failure log missing %s:\n%s", want, out)
		}
	}
}

func TestHandle_SkipDoesntCreateRunOrAudit(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)

	// "issues.opened" — not a trigger action.
	body, _ := json.Marshal(map[string]any{
		"action":       "opened",
		"installation": map[string]any{"id": 42},
		"repository":   map[string]any{"full_name": "x/y"},
		"sender":       map[string]any{"login": "alice", "type": "User"},
	})
	ev, _ := ParseEvent("issues", "deliv-2", body)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if gh.specCalls != 0 || gh.dispatchCalls != 0 {
		t.Errorf("unexpected GitHub calls: spec=%d dispatch=%d", gh.specCalls, gh.dispatchCalls)
	}
	if len(runs.created) != 0 {
		t.Errorf("runs created on skip: %d", len(runs.created))
	}
	if len(au.appended) != 0 {
		t.Errorf("audit on skip: %d", len(au.appended))
	}
}

func TestHandle_SpecForbidden_NoRunNoAudit(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	gh.specErr = githubclient.ErrForbidden

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle returned err on Forbidden, want nil: %v", err)
	}
	if len(runs.created) != 0 || len(au.appended) != 0 {
		t.Errorf("created run or audit on forbidden: runs=%d audit=%d",
			len(runs.created), len(au.appended))
	}
}

func TestHandle_SpecNotFound_NoRunNoAudit(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.specErr = githubclient.ErrNotFound

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("run created on spec not-found: %d", len(runs.created))
	}
}

func TestHandle_SpecTransientError_ReturnsErrorForRetry(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.specErr = errors.New("upstream timeout")

	err := d.Handle(context.Background(), issueLabeledEvent(t))
	if err == nil {
		t.Fatal("expected non-nil error on transient spec failure")
	}
	if len(runs.created) != 0 {
		t.Errorf("run created despite transient err: %d", len(runs.created))
	}
}

func TestHandle_SpecParseError_NoRun(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	gh.specContent = []byte("not valid yaml: : :")
	gh.specSHA = "deadbeef"

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Refused to dispatch — no run, no audit (rejection logs only).
	if len(runs.created) != 0 || len(au.appended) != 0 {
		t.Errorf("created on parse error: runs=%d audit=%d",
			len(runs.created), len(au.appended))
	}
}

func TestHandle_WorkflowIDNotInSpec_NoRun(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	// Spec parses, but doesn't contain "feature_change".
	gh.specContent = []byte(`version: "0.1"
roles:
  tech_lead:
    members: ["@x"]
workflows:
  hotfix:
    description: only one
    stages:
      - id: plan
        type: plan
        executor: {agent: claude-code}
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers: {any_of: [tech_lead]}
            sla: 4_business_hours
`)
	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("created run when workflow_id missing: %d", len(runs.created))
	}
}

func TestHandle_RunCreateError_ReturnsError(t *testing.T) {
	d, _, runs, _ := newDispatcherWithStubs(t)
	runs.createErr = errors.New("db down")

	err := d.Handle(context.Background(), issueLabeledEvent(t))
	if err == nil {
		t.Fatal("expected error on run create failure")
	}
}

func TestHandle_AuditAppendError_DoesntFailDispatch(t *testing.T) {
	// Dispatcher already fired workflow_dispatch; logging the
	// audit failure is enough — we don't unwind.
	d, gh, runs, au := newDispatcherWithStubs(t)
	au.appendErr = errors.New("audit table down")

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 1 || gh.dispatchCalls != 1 {
		t.Errorf("audit failure aborted dispatch: runs=%d dispatch=%d",
			len(runs.created), gh.dispatchCalls)
	}
}

func TestHandle_MalformedRepoSkips(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	body, _ := json.Marshal(map[string]any{
		"action":       "labeled",
		"installation": map[string]any{"id": 42},
		"repository":   map[string]any{"full_name": "no-slash"}, // malformed
		"sender":       map[string]any{"login": "alice", "type": "User"},
		"issue":        map[string]any{"number": 1},
		"label":        map[string]any{"name": "fishhawk"},
	})
	ev, _ := ParseEvent("issues", "d", body)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 || gh.specCalls != 0 {
		t.Errorf("acted on malformed repo: runs=%d spec=%d",
			len(runs.created), gh.specCalls)
	}
}

func TestHandle_DefaultsApplied(t *testing.T) {
	d, gh, _, _ := newDispatcherWithStubs(t)
	d.DefaultRef = ""
	d.ActionsWorkflowFile = ""

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatal(err)
	}
	if gh.dispatchCall.ref != "main" {
		t.Errorf("default ref = %q, want main", gh.dispatchCall.ref)
	}
	if gh.dispatchCall.file != DefaultActionsWorkflowFile {
		t.Errorf("default file = %q, want %s", gh.dispatchCall.file, DefaultActionsWorkflowFile)
	}
}

func TestParseRepo(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		owner string
		name  string
	}{
		{"x/y", true, "x", "y"},
		{"kuhlman-labs/fishhawk", true, "kuhlman-labs", "fishhawk"},
		{"no-slash", false, "", ""},
		{"/y", false, "", ""},
		{"x/", false, "", ""},
		{"", false, "", ""},
	}
	for _, c := range cases {
		got, err := parseRepo(c.in)
		if c.ok != (err == nil) {
			t.Errorf("parseRepo(%q): err=%v, want ok=%v", c.in, err, c.ok)
		}
		if c.ok && (got.Owner != c.owner || got.Name != c.name) {
			t.Errorf("parseRepo(%q) = %+v", c.in, got)
		}
	}
}

func TestStringPtr(t *testing.T) {
	p := stringPtr("hello")
	if p == nil || *p != "hello" {
		t.Errorf("stringPtr broken: %v", p)
	}
}

func TestHandle_PersistsRequiresApprovalPerStageGate(t *testing.T) {
	// Per #207: stages with an approval-typed gate must have
	// RequiresApproval=true at create time so the trace upload
	// handler picks the right post-upload transition. Stages
	// without an approval gate must have RequiresApproval=false.
	const multiStageSpec = `version: "0.1"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Test workflow with mixed gating
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [founder]
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        # NO gates — implement is a pure agent stage.
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvers:
              any_of: [founder]
`
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.specContent = []byte(multiStageSpec)

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.createdStages) != 3 {
		t.Fatalf("createdStages = %d, want 3 (plan/implement/review)", len(runs.createdStages))
	}

	want := []struct {
		stageType        string
		requiresApproval bool
	}{
		{"plan", true},       // approval gate
		{"implement", false}, // no gate
		{"review", true},     // approval gate
	}
	for i, w := range want {
		got := runs.createdStages[i]
		if string(got.Type) != w.stageType {
			t.Errorf("stage %d type = %q, want %q", i, got.Type, w.stageType)
		}
		if got.RequiresApproval != w.requiresApproval {
			t.Errorf("stage %d (%s) RequiresApproval = %v, want %v",
				i, w.stageType, got.RequiresApproval, w.requiresApproval)
		}
	}
}

func TestHandle_PersistsGateShapePerStage(t *testing.T) {
	// Per #213: the dispatcher writes the *primary* gate's shape
	// (type + blocking_checks + approvers) onto each stages row so
	// the review-stage UI can render it without re-parsing the spec.
	// Primary = first approval gate, else first check gate, else
	// nil. v0 stages don't usually have multiple gates, but the
	// review-stage gate is the canonical case where blocking_checks
	// matter.
	const spec = `version: "0.1"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: review gate carries blocking_checks
    stages:
      - id: plan
        type: plan
        executor: { agent: claude-code }
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers: { any_of: [founder] }
      - id: implement
        type: implement
        executor: { agent: claude-code }
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor: { human: true }
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvers: { any_of: [founder] }
            blocking_checks:
              - ci_pass
              - fishhawk_audit_complete
`
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.specContent = []byte(spec)

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.createdStages) != 3 {
		t.Fatalf("createdStages = %d, want 3", len(runs.createdStages))
	}

	plan, implement, review := runs.createdStages[0], runs.createdStages[1], runs.createdStages[2]

	if plan.Gate == nil || plan.Gate.Kind != run.GateKindApproval {
		t.Errorf("plan.Gate = %+v, want approval gate", plan.Gate)
	}
	if plan.Gate != nil && plan.Gate.Approvers == nil {
		t.Errorf("plan.Gate.Approvers = nil, want any_of:[founder]")
	}
	if plan.Gate != nil && len(plan.Gate.BlockingChecks) != 0 {
		t.Errorf("plan.Gate.BlockingChecks = %v, want empty (none in spec)", plan.Gate.BlockingChecks)
	}

	// implement stage has no gates: Gate must be nil.
	if implement.Gate != nil {
		t.Errorf("implement.Gate = %+v, want nil (no gates in spec)", implement.Gate)
	}

	// review stage carries the blocking_checks the UI needs to render.
	if review.Gate == nil {
		t.Fatal("review.Gate = nil, want approval gate with blocking_checks")
	}
	if review.Gate.Kind != run.GateKindApproval {
		t.Errorf("review.Gate.Kind = %q, want approval", review.Gate.Kind)
	}
	if got := review.Gate.BlockingChecks; len(got) != 2 || got[0] != "ci_pass" || got[1] != "fishhawk_audit_complete" {
		t.Errorf("review.Gate.BlockingChecks = %v, want [ci_pass, fishhawk_audit_complete]", got)
	}
	if review.Gate.Approvers == nil || len(review.Gate.Approvers.AnyOf) != 1 || review.Gate.Approvers.AnyOf[0] != "founder" {
		t.Errorf("review.Gate.Approvers = %+v, want any_of:[founder]", review.Gate.Approvers)
	}
}

// stubIssueNotifier captures NotifyPickup invocations so the
// dispatcher's pickup-ack hook can be asserted on without standing
// up the full issuecomment package wiring.
type stubIssueNotifier struct {
	mu    sync.Mutex
	calls []stubNotifyCall
	err   error
}

type stubNotifyCall struct {
	runID  uuid.UUID
	sender string
}

func (s *stubIssueNotifier) NotifyPickup(_ context.Context, runID uuid.UUID, sender string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubNotifyCall{runID: runID, sender: sender})
	return s.err
}

func TestHandle_IssueTrigger_FiresPickupNotification(t *testing.T) {
	d, _, runs, _ := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 NotifyPickup call; got %d", len(notifier.calls))
	}
	created := runs.created[0]
	if notifier.calls[0].runID != created.ID {
		t.Errorf("NotifyPickup runID = %s, want %s", notifier.calls[0].runID, created.ID)
	}
	if notifier.calls[0].sender != "alice" {
		t.Errorf("NotifyPickup sender = %q, want alice", notifier.calls[0].sender)
	}
}

func TestHandle_DispatchFailure_SuppressesPickupNotification(t *testing.T) {
	d, gh, _, _ := newDispatcherWithStubs(t)
	gh.dispatchErr = errors.New("422 invalid ref")
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("expected 0 NotifyPickup calls when dispatch failed; got %d", len(notifier.calls))
	}
}

func TestHandle_NotifyPickupError_DoesntFailDispatch(t *testing.T) {
	d, _, _, _ := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{err: errors.New("403 forbidden")}
	d.IssueNotifier = notifier

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Errorf("Handle should swallow notifier errors; got %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Errorf("notifier should still be called once; got %d", len(notifier.calls))
	}
}

// stubApprovalHandler captures HandleApprovalCommand invocations
// so dispatcher routing tests can assert the handler is reached
// with the right params.
type stubApprovalHandler struct {
	mu    sync.Mutex
	calls []ApprovalCommandParams
	err   error
}

func (s *stubApprovalHandler) HandleApprovalCommand(_ context.Context, p ApprovalCommandParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, p)
	return s.err
}

func issueApproveCommentEvent(t *testing.T, body string) Event {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"action":  "created",
		"comment": map[string]any{"body": body},
		"issue":   map[string]any{"number": 1247},
		"repository": map[string]any{
			"full_name": "kuhlman-labs/fishhawk",
		},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": "alice", "type": "User"},
	})
	ev, err := ParseEvent("issue_comment", "deliv-approve", raw)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestHandle_SlashApprove_RoutesToApprovalHandler(t *testing.T) {
	d, gh, _, _ := newDispatcherWithStubs(t)
	approver := &stubApprovalHandler{}
	d.ApprovalHandler = approver

	if err := d.Handle(context.Background(), issueApproveCommentEvent(t, "/fishhawk approve\n\nlooks good")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gh.dispatchCalls != 0 {
		t.Errorf("approve command should not fire workflow_dispatch; got %d", gh.dispatchCalls)
	}
	if gh.specCalls != 0 {
		t.Errorf("approve command should not fetch the spec; got %d", gh.specCalls)
	}
	if len(approver.calls) != 1 {
		t.Fatalf("expected 1 approval handler call; got %d", len(approver.calls))
	}
	c := approver.calls[0]
	if c.Decision != MatchActionApprove {
		t.Errorf("decision = %q", c.Decision)
	}
	if c.SenderLogin != "alice" {
		t.Errorf("sender = %q", c.SenderLogin)
	}
	if c.IssueNumber != 1247 {
		t.Errorf("issue = %d", c.IssueNumber)
	}
	if c.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("repo = %q", c.Repo)
	}
	if c.Comment != "looks good" {
		t.Errorf("comment = %q", c.Comment)
	}
}

func TestHandle_SlashReject_RoutesToApprovalHandler(t *testing.T) {
	d, _, _, _ := newDispatcherWithStubs(t)
	approver := &stubApprovalHandler{}
	d.ApprovalHandler = approver

	if err := d.Handle(context.Background(), issueApproveCommentEvent(t, "/fishhawk reject")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(approver.calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(approver.calls))
	}
	if approver.calls[0].Decision != MatchActionReject {
		t.Errorf("decision = %q want reject", approver.calls[0].Decision)
	}
}

func TestHandle_SlashApprove_NoHandler_LogsAndSkips(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	// Leave d.ApprovalHandler unset.

	if err := d.Handle(context.Background(), issueApproveCommentEvent(t, "/fishhawk approve")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gh.dispatchCalls != 0 || gh.specCalls != 0 {
		t.Errorf("approve command should not fire workflow_dispatch or fetch spec without handler")
	}
	if len(runs.created) != 0 {
		t.Errorf("approve command should not create a run; got %+v", runs.created)
	}
}

func TestHandle_SlashApprove_HandlerError_DoesntFailWebhook(t *testing.T) {
	d, _, _, _ := newDispatcherWithStubs(t)
	approver := &stubApprovalHandler{err: errors.New("forbidden")}
	d.ApprovalHandler = approver

	if err := d.Handle(context.Background(), issueApproveCommentEvent(t, "/fishhawk approve")); err != nil {
		t.Errorf("handler errors should be swallowed; got %v", err)
	}
	if len(approver.calls) != 1 {
		t.Errorf("handler should still be called; got %d", len(approver.calls))
	}
}

func TestHandle_ThreadsFollowupAsParent_WhenPriorRunIsActive(t *testing.T) {
	d, _, runs, _ := newDispatcherWithStubs(t)

	// Pre-seed an existing non-terminal run on the same trigger_ref.
	// The dispatcher's findParentRunID reads ListRuns; the stub
	// returns rows newest-first, so the most-recent active run wins.
	tr := "issue:1247"
	priorID := uuid.New()
	runs.created = append(runs.created, &run.Run{
		ID:            priorID,
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    DefaultWorkflowID,
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &tr,
		State:         run.StateRunning,
		CreatedAt:     time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
	})

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(runs.created) != 2 {
		t.Fatalf("expected 2 runs (prior + new); got %d", len(runs.created))
	}
	newest := runs.created[len(runs.created)-1]
	if newest.ParentRunID == nil {
		t.Fatalf("ParentRunID should be set when a prior active run exists")
	}
	if *newest.ParentRunID != priorID {
		t.Errorf("ParentRunID = %s, want %s", newest.ParentRunID, priorID)
	}
}

func TestHandle_DoesNotThread_WhenPriorRunIsTerminal(t *testing.T) {
	d, _, runs, _ := newDispatcherWithStubs(t)

	// Pre-seed an existing terminal run; the dispatcher should
	// treat the new trigger as a fresh root rather than threading
	// it under the closed predecessor.
	tr := "issue:1247"
	runs.created = append(runs.created, &run.Run{
		ID:            uuid.New(),
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    DefaultWorkflowID,
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &tr,
		State:         run.StateSucceeded,
		CreatedAt:     time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
	})

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	newest := runs.created[len(runs.created)-1]
	if newest.ParentRunID != nil {
		t.Errorf("ParentRunID should be nil when prior run is terminal; got %v", newest.ParentRunID)
	}
}

func TestHandle_DoesNotThread_WhenNoPriorRun(t *testing.T) {
	d, _, runs, _ := newDispatcherWithStubs(t)
	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 1 {
		t.Fatalf("expected 1 run; got %d", len(runs.created))
	}
	if runs.created[0].ParentRunID != nil {
		t.Errorf("ParentRunID should be nil when there's no prior run; got %v", runs.created[0].ParentRunID)
	}
}

// --- workflow_run.failure routing ---

func workflowRunFailedEvent(t *testing.T) Event {
	t.Helper()
	body := workflowRunBody(t, nil) // failure conclusion, dispatch event, fishhawk.yml
	raw, _ := json.Marshal(map[string]any{
		"action": "completed",
		"workflow_run": map[string]any{
			"id":          int64(987654321),
			"path":        ".github/workflows/fishhawk.yml",
			"conclusion":  "failure",
			"status":      "completed",
			"event":       "workflow_dispatch",
			"head_branch": "main",
			"head_sha":    "abc123",
		},
		"repository": map[string]any{
			"full_name": "kuhlman-labs/fishhawk",
		},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": "fishhawk-app[bot]", "type": "User"},
	})
	_ = body // produced for the matcher unit tests; we rebuild a richer event here
	ev, err := ParseEvent("workflow_run", "deliv-wfrun", raw)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// seedDispatchedStage adds a fake stage in the `dispatched` state to
// the stub's createdStages so FailStage can walk it through running →
// failed. Returns the stage id for assertion-side use.
func seedDispatchedStage(t *testing.T, runs *stubRuns) uuid.UUID {
	t.Helper()
	id := uuid.New()
	runs.mu.Lock()
	runs.createdStages = append(runs.createdStages, &run.Stage{
		ID: id, RunID: uuid.New(), Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		State: run.StageStateDispatched,
	})
	runs.mu.Unlock()
	return id
}

func TestHandle_RunnerActionFailed_TransitionsStageToFailedC(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	stageID := seedDispatchedStage(t, runs)
	gh.workflowRun = &githubclient.WorkflowRun{
		ID:         987654321,
		Conclusion: "failure",
		Status:     "completed",
		Event:      "workflow_dispatch",
		Inputs: map[string]string{
			"run_id":      uuid.New().String(),
			"stage_id":    stageID.String(),
			"workflow_id": "feature_change",
		},
		HTMLURL: "https://github.com/kuhlman-labs/fishhawk/actions/runs/987654321",
	}

	if err := d.Handle(context.Background(), workflowRunFailedEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gh.workflowRunCalls != 1 {
		t.Errorf("expected 1 GetWorkflowRun call; got %d", gh.workflowRunCalls)
	}
	if gh.workflowRunCall.runID != 987654321 {
		t.Errorf("GetWorkflowRun runID = %d", gh.workflowRunCall.runID)
	}

	// FailStage walks dispatched -> running -> failed, so we expect
	// two transition records on the stub.
	runs.mu.Lock()
	defer runs.mu.Unlock()
	if len(runs.transitions) != 2 {
		t.Fatalf("expected 2 transitions (running, failed); got %d: %+v",
			len(runs.transitions), runs.transitions)
	}
	if runs.transitions[0].StageID != stageID || runs.transitions[0].To != run.StageStateRunning {
		t.Errorf("first transition = %+v, want (stageID, running)", runs.transitions[0])
	}
	if runs.transitions[1].StageID != stageID || runs.transitions[1].To != run.StageStateFailed {
		t.Errorf("second transition = %+v, want (stageID, failed)", runs.transitions[1])
	}
}

func TestHandle_RunnerActionFailed_NotADispatchedRun_Skips(t *testing.T) {
	// A workflow_run.completed with no stage_id input — e.g. a
	// run fired by something other than Fishhawk's dispatcher
	// (manual trigger, scheduled cron). We can't match it; just
	// log and move on.
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.workflowRun = &githubclient.WorkflowRun{
		ID:     987654321,
		Inputs: map[string]string{},
	}

	if err := d.Handle(context.Background(), workflowRunFailedEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.transitions) != 0 {
		t.Errorf("no stage_id input should mean no transition; got %+v", runs.transitions)
	}
}

func TestHandle_RunnerActionFailed_StageAlreadyTerminal_Idempotent(t *testing.T) {
	// The trace upload may have landed before the workflow_run
	// webhook arrived (e.g., trace POST then runner exit). The
	// stage is already in awaiting_approval; FailStage's
	// transition to failed-from-awaiting_approval succeeds.
	// Confirms that the handler doesn't 5xx and that re-deliveries
	// don't break.
	d, gh, runs, _ := newDispatcherWithStubs(t)
	stageID := uuid.New()
	runs.mu.Lock()
	runs.createdStages = append(runs.createdStages, &run.Stage{
		ID: stageID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		State:        run.StageStateAwaitingApproval,
	})
	runs.mu.Unlock()
	gh.workflowRun = &githubclient.WorkflowRun{
		ID:     987654321,
		Inputs: map[string]string{"stage_id": stageID.String()},
	}

	if err := d.Handle(context.Background(), workflowRunFailedEvent(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// awaiting_approval -> failed is one transition (no running step).
	runs.mu.Lock()
	defer runs.mu.Unlock()
	if len(runs.transitions) != 1 || runs.transitions[0].To != run.StageStateFailed {
		t.Errorf("expected 1 transition to failed; got %+v", runs.transitions)
	}
}

func TestHandle_RunnerActionFailed_GetWorkflowRunErrors_LogsAndReturns(t *testing.T) {
	d, gh, runs, _ := newDispatcherWithStubs(t)
	gh.workflowRunErr = errors.New("github 5xx")

	// Should not surface as a webhook error. We log + return nil so
	// GitHub doesn't retry the delivery — the watchdog will still
	// time out the stage.
	if err := d.Handle(context.Background(), workflowRunFailedEvent(t)); err != nil {
		t.Errorf("Handle should swallow lookup errors; got %v", err)
	}
	if len(runs.transitions) != 0 {
		t.Errorf("no transition expected on lookup failure; got %+v", runs.transitions)
	}
}

func TestPickPrimaryGate(t *testing.T) {
	// Approval wins over check, in any order, even if the check
	// gate appears first in the spec — the review-stage UI's
	// approval-vs-check decision depends on the right pick.
	checkGate := spec.Gate{Type: spec.GateTypeCheck, BlockingChecks: []string{"ci_pass"}}
	approvalGate := spec.Gate{Type: spec.GateTypeApproval, Approvers: &spec.Approvers{AnyOf: []string{"founder"}}}

	if got := pickPrimaryGate([]spec.Gate{checkGate, approvalGate}); got == nil || got.Type != spec.GateTypeApproval {
		t.Errorf("approval-after-check: got %+v, want approval", got)
	}
	if got := pickPrimaryGate([]spec.Gate{approvalGate, checkGate}); got == nil || got.Type != spec.GateTypeApproval {
		t.Errorf("approval-before-check: got %+v, want approval", got)
	}
	if got := pickPrimaryGate([]spec.Gate{checkGate}); got == nil || got.Type != spec.GateTypeCheck {
		t.Errorf("check-only: got %+v, want check", got)
	}
	if got := pickPrimaryGate(nil); got != nil {
		t.Errorf("empty gates: got %+v, want nil", got)
	}
}
