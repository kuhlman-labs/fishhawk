package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

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

// stubRuns is a tiny in-memory run.Repository covering only the
// methods Dispatcher.Handle uses (CreateRun).
type stubRuns struct {
	mu        sync.Mutex
	created   []*run.Run
	createErr error
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
		State:         run.StatePending,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	s.created = append(s.created, r)
	return r, nil
}

func (s *stubRuns) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) GetStage(context.Context, uuid.UUID) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (s *stubRuns) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
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
	return &audit.Entry{ID: uuid.New(), RunID: p.RunID}, nil
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
}

func TestHandle_DispatchError_AuditsFailureCategory(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
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
