package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Crew work requests (E77.7 / #3741, ADR-081 rule 2): a work_request send
// files exactly ONE work item through the shared applyAndFileWorkItem core and
// never creates a run; a filing failure degrades the 201 and keeps the
// recorded message.

// countingWorkProvider counts File calls under a mutex (the #3226 rule) and
// answers EpicChildren with no children, so {n} derives to 1.
type countingWorkProvider struct {
	mu       sync.Mutex
	files    int
	captured workmgmt.ProviderRequest
	fileErr  error
}

func (p *countingWorkProvider) Name() string { return workmgmt.Default().Provider }

func (p *countingWorkProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.files++
	p.captured = req
	if p.fileErr != nil {
		return nil, p.fileErr
	}
	return &workmgmt.CreatedItem{
		Provider: p.Name(), Number: 4242, URL: "https://github.com/kuhlman-labs/fishhawk/issues/4242",
		AppliedLabels: req.Item.Classification.Labels, Boarded: true,
	}, nil
}

func (p *countingWorkProvider) EpicChildren(context.Context, workmgmt.EpicChildrenRequest) (*workmgmt.EpicChildrenResult, error) {
	return &workmgmt.EpicChildrenResult{}, nil
}

func (p *countingWorkProvider) fileCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.files
}

// countingRunRepo counts CreateRun: a work request must never create a run.
type countingRunRepo struct {
	run.Repository
	mu      sync.Mutex
	creates int
	getErr  error
}

func (r *countingRunRepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	r.mu.Lock()
	r.creates++
	r.mu.Unlock()
	return r.Repository.CreateRun(ctx, p)
}

func (r *countingRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.Repository.GetRun(ctx, id)
}

// workRequestAudit fails only the crew_work_request_filed append.
type workRequestAudit struct {
	audit.Repository
	failFiled bool
}

func (a *workRequestAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.failFiled && p.Category == CategoryCrewWorkRequestFiled {
		return nil, errors.New("append refused")
	}
	return a.Repository.AppendChained(ctx, p)
}

type workRequestFixture struct {
	*crewPG
	provider *countingWorkProvider
	runs     *countingRunRepo
}

// newWorkRequestFixture wires f.srv with a counting provider, a counting run
// repository and the epic-deriving GitHub client the {epic} title var needs.
func newWorkRequestFixture(t *testing.T) *workRequestFixture {
	t.Helper()
	f := newCrewPG(t)
	fp := &countingWorkProvider{}
	workmgmt.Register(fp)
	rr := &countingRunRepo{Repository: run.NewPostgresRepository(f.pool)}
	f.srv = New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     rr,
		AuditRepo:   f.audit,
		CrewMailbox: f.mailbox,
		GitHub:      newEpicGitHubClient(t, 7788, "[E22] The parent epic"),
	})
	return &workRequestFixture{crewPG: f, provider: fp, runs: rr}
}

const workRequestPayload = `{"title":"WR-SENTINEL add a retry budget","summary":"the poller spins without a bound","rationale":"it burned a stage budget"}`

func workRequestDoc(anchor string) string {
	return `{"schema_version":"crew-message-v1","type":"work_request","recipient_role":"captain","anchor":` + anchor +
		`,"payload":` + workRequestPayload + `,"evidence":[{"kind":"issue","ref":"389"},{"kind":"url","ref":"https://example.test/x"}]}`
}

// parseWorkRequest parses doc with the sender role the send path would derive.
func parseWorkRequest(doc string) (*crewmessage.Message, error) {
	withRole, err := crewmessage.WithSenderRole([]byte(doc), crewmessage.RolePlanner)
	if err != nil {
		return nil, err
	}
	return crewmessage.Parse(withRole)
}

func sendWorkRequest(t *testing.T, s *Server, body string, decorate func(*http.Request) *http.Request) (crewMessageResponse, map[string]json.RawMessage) {
	t.Helper()
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body, decorate)
	if w.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var resp crewMessageResponse
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	return resp, raw
}

// TestCrewWorkRequest_FilesOneIssueAndNoRun (C10, done-means): a run-bound
// planner's work_request files exactly one conventions-complete item citing
// the message and its evidence, records crew_work_request_filed on the run's
// chain, and creates no run.
func TestCrewWorkRequest_FilesOneIssueAndNoRun(t *testing.T) {
	f := newWorkRequestFixture(t)
	runID := f.seedRun(t, run.StageTypePlan)
	resp, _ := sendWorkRequest(t, f.srv, workRequestDoc(runAnchor(runID)), runBound(runID, scopeWriteMessages))

	if n := f.provider.fileCount(); n != 1 {
		t.Fatalf("provider.File calls = %d, want exactly 1", n)
	}
	if f.runs.creates != 0 {
		t.Fatalf("CreateRun calls = %d, want 0: a work request never dispatches a run", f.runs.creates)
	}
	if resp.WorkItemFilingError != nil {
		t.Fatalf("work_item_filing_error = %+v, want nil", resp.WorkItemFilingError)
	}
	if resp.WorkItem == nil || resp.WorkItem.Number != 4242 {
		t.Fatalf("work_item = %+v, want #4242", resp.WorkItem)
	}
	if resp.WorkItem.Title != "[E22.1] WR-SENTINEL add a retry budget" {
		t.Errorf("title = %q, want the conventions-rendered [E22.1] title", resp.WorkItem.Title)
	}
	item := f.provider.captured.Item
	for _, want := range []string{"the poller spins without a bound", "it burned a stage budget",
		"crew_message:" + strconv.FormatInt(resp.SentSequence, 10), runID.String(), "url:https://example.test/x"} {
		if !strings.Contains(item.Body, want) {
			t.Errorf("filed body missing %q:\n%s", want, item.Body)
		}
	}
	if item.Relations.ParentEpic != "#389" {
		t.Errorf("parent epic = %q, want #389 (from the issue evidence ref)", item.Relations.ParentEpic)
	}
	if f.provider.captured.Target.Scope.IsZero() {
		t.Error("the filing target carried no installation scope")
	}

	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryCrewWorkRequestFiled)
	if err != nil || len(entries) != 1 {
		t.Fatalf("crew_work_request_filed entries = %d (err %v), want 1", len(entries), err)
	}
	var p struct {
		SentSequence int64  `json:"sent_sequence"`
		IssueNumber  int    `json:"issue_number"`
		IssueURL     string `json:"issue_url"`
	}
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatalf("decode filed payload: %v", err)
	}
	if p.SentSequence != resp.SentSequence || p.IssueNumber != 4242 || !strings.HasSuffix(p.IssueURL, "/issues/4242") {
		t.Errorf("filed payload = %+v, want sequence %d and #4242", p, resp.SentSequence)
	}
}

// TestCrewWorkRequest_RunBoundRequiresRunScope (C11): a run-bound identity on
// the run-ABSENT branch is refused run_scoped_filing_required with nothing
// filed, while the SAME row filed by an operator lands — so it is the guard,
// not the missing run, that refuses. The run-anchored ACCEPT arm is
// TestCrewWorkRequest_FilesOneIssueAndNoRun.
func TestCrewWorkRequest_RunBoundRequiresRunScope(t *testing.T) {
	f := newWorkRequestFixture(t)
	msg, err := parseWorkRequest(workRequestDoc(`{"issue_ref":"kuhlman-labs/fishhawk#12"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	row := &crewmessage.Row{SentSequence: 991, IssueRef: "kuhlman-labs/fishhawk#12", SenderRole: crewmessage.RolePlanner}
	ctx := context.Background()

	item, ferr := f.srv.fileCrewWorkRequest(ctx, Identity{Subject: "mcp:run:" + uuid.NewString()}, msg, row)
	if item != nil || ferr == nil || ferr.Code != "run_scoped_filing_required" {
		t.Fatalf("run-bound run-less filing = (%+v, %+v), want run_scoped_filing_required", item, ferr)
	}
	if n := f.provider.fileCount(); n != 0 {
		t.Fatalf("provider.File calls = %d after the refusal, want 0", n)
	}

	item, ferr = f.srv.fileCrewWorkRequest(ctx, Identity{Subject: "github:operator"}, msg, row)
	if ferr != nil || item == nil {
		t.Fatalf("operator run-less filing = (%+v, %+v), want a filed item", item, ferr)
	}
	if got := f.provider.captured.Target.Repo; got.Owner != "kuhlman-labs" || got.Name != "fishhawk" {
		t.Errorf("filing repo = %+v, want the issue_ref's kuhlman-labs/fishhawk", got)
	}
}

// TestCrewWorkRequest_FilingFailureKeepsChainEntry (C12): the provider fails;
// the send is still 201 with work_item_filing_error, and the message's
// crew_message_sent entry is READ BACK from the chain (committed state).
func TestCrewWorkRequest_FilingFailureKeepsChainEntry(t *testing.T) {
	f := newWorkRequestFixture(t)
	f.provider.fileErr = errors.New("forge unavailable")
	runID := f.seedRun(t, run.StageTypePlan)
	resp, raw := sendWorkRequest(t, f.srv, workRequestDoc(runAnchor(runID)), runBound(runID, scopeWriteMessages))

	if resp.WorkItem != nil {
		t.Fatalf("work_item = %+v, want nil on a failed filing", resp.WorkItem)
	}
	if resp.WorkItemFilingError == nil || resp.WorkItemFilingError.Code != "work_item_filing_failed" {
		t.Fatalf("work_item_filing_error = %+v, want work_item_filing_failed", resp.WorkItemFilingError)
	}
	if _, ok := raw["work_item"]; ok {
		t.Error("a failed filing still serialised a work_item member")
	}
	sent, err := f.audit.ListForRunByCategory(context.Background(), runID, crewmessage.CategorySent)
	if err != nil {
		t.Fatalf("list sent: %v", err)
	}
	found := false
	for _, e := range sent {
		found = found || e.Sequence == resp.SentSequence
	}
	if !found {
		t.Fatalf("the crew_message_sent entry %d is not on the chain after a failed filing", resp.SentSequence)
	}
	if n := f.count(t, CategoryCrewWorkRequestFiled); n != 0 {
		t.Errorf("crew_work_request_filed recorded %d entries for a filing that failed", n)
	}
}

// TestCrewWorkRequest_OtherTypesFileNothing: a finding send never reaches the
// provider and its response carries neither work-item member.
func TestCrewWorkRequest_OtherTypesFileNothing(t *testing.T) {
	f := newWorkRequestFixture(t)
	runID := f.seedRun(t, run.StageTypePlan)
	_, raw := sendWorkRequest(t, f.srv, crewDoc("finding", "", "reviewer", runAnchor(runID), `{"summary":"not a request"}`),
		runBound(runID, scopeWriteMessages))
	if n := f.provider.fileCount(); n != 0 {
		t.Fatalf("a finding send filed %d work items", n)
	}
	for _, k := range []string{"work_item", "work_item_filing_error"} {
		if _, ok := raw[k]; ok {
			t.Errorf("a finding send response carries %q", k)
		}
	}
}

// TestCrewWorkRequest_DegradeBranches: each remaining defensive branch returns
// its named filing error with nothing filed.
func TestCrewWorkRequest_DegradeBranches(t *testing.T) {
	f := newWorkRequestFixture(t)
	runID := f.seedRun(t, run.StageTypePlan)
	msg, err := parseWorkRequest(workRequestDoc(runAnchor(runID)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := context.Background()
	op := Identity{Subject: "github:operator"}

	t.Run("no repository", func(t *testing.T) {
		row := &crewmessage.Row{SentSequence: 1, DecisionRecordID: "dr-1"}
		if _, ferr := f.srv.fileCrewWorkRequest(ctx, op, msg, row); ferr == nil || ferr.Code != "crew_work_request_no_repository" {
			t.Fatalf("filing error = %+v, want crew_work_request_no_repository", ferr)
		}
	})
	t.Run("anchor run lookup fails", func(t *testing.T) {
		f.runs.getErr = errors.New("db down")
		defer func() { f.runs.getErr = nil }()
		if _, ferr := f.srv.fileCrewWorkRequest(ctx, op, msg, &crewmessage.Row{SentSequence: 1, RunID: &runID}); ferr == nil || ferr.Code != "internal_error" {
			t.Fatalf("filing error = %+v, want internal_error", ferr)
		}
	})
	t.Run("conventions load fails", func(t *testing.T) {
		prev := conventionsLoader
		conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
			return workmgmt.Conventions{}, errors.New("no conventions")
		}
		defer func() { conventionsLoader = prev }()
		if _, ferr := f.srv.fileCrewWorkRequest(ctx, op, msg, &crewmessage.Row{SentSequence: 1, RunID: &runID}); ferr == nil || ferr.Code != "internal_error" {
			t.Fatalf("filing error = %+v, want internal_error", ferr)
		}
	})
	t.Run("installation resolution fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		prev := f.srv.cfg.GitHub
		f.srv.cfg.GitHub = &githubclient.Client{BaseURL: srv.URL, Tokens: &fakeTokenProvider{tok: "t"},
			HTTP: &http.Client{Timeout: 5 * time.Second}, AppJWT: func() (string, error) { return "jwt", nil }}
		defer func() { f.srv.cfg.GitHub = prev }()
		if _, ferr := f.srv.fileCrewWorkRequest(ctx, op, msg, &crewmessage.Row{SentSequence: 1, RunID: &runID}); ferr == nil || ferr.Code != "work_item_filing_failed" {
			t.Fatalf("filing error = %+v, want work_item_filing_failed", ferr)
		}
	})
	if n := f.provider.fileCount(); n != 0 {
		t.Fatalf("provider.File calls = %d across the degrade branches, want 0", n)
	}
}

// TestCrewWorkRequest_RecordFailureStillReturnsItem: a failed
// crew_work_request_filed append is warn-only — the landed filing is still
// reported.
func TestCrewWorkRequest_RecordFailureStillReturnsItem(t *testing.T) {
	f := newWorkRequestFixture(t)
	f.srv.cfg.AuditRepo = &workRequestAudit{Repository: f.audit, failFiled: true}
	runID := f.seedRun(t, run.StageTypePlan)
	resp, _ := sendWorkRequest(t, f.srv, workRequestDoc(runAnchor(runID)), runBound(runID, scopeWriteMessages))
	if resp.WorkItem == nil || resp.WorkItemFilingError != nil {
		t.Fatalf("response = %+v / %+v, want the filed item despite the record failure", resp.WorkItem, resp.WorkItemFilingError)
	}
	if n := f.count(t, CategoryCrewWorkRequestFiled); n != 0 {
		t.Errorf("recorded %d entries through a failing append", n)
	}
}

// TestCrewWorkRequest_OperatorRunLessRecordsOnGlobalChain: an operator's
// issue_ref-anchored request files and records its fact on the run-less chain.
func TestCrewWorkRequest_OperatorRunLessRecordsOnGlobalChain(t *testing.T) {
	f := newWorkRequestFixture(t)
	resp, _ := sendWorkRequest(t, f.srv, workRequestDoc(`{"issue_ref":"kuhlman-labs/fishhawk#12"}`), operator("write:stages"))
	if resp.WorkItem == nil {
		t.Fatalf("operator run-less work request filed nothing: %+v", resp.WorkItemFilingError)
	}
	if n := f.count(t, CategoryCrewWorkRequestFiled); n != 1 {
		t.Errorf("crew_work_request_filed entries = %d, want 1 on the run-less chain", n)
	}
}

func TestCrewWorkRequestHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"kuhlman-labs/fishhawk#12": "kuhlman-labs/fishhawk", "#12": "", "fishhawk": "", "": "",
	} {
		if got := crewIssueRefRepo(in); got != want {
			t.Errorf("crewIssueRefRepo(%q) = %q, want %q", in, got, want)
		}
	}
	msg := &crewmessage.Message{Evidence: []crewmessage.EvidenceReference{
		{Kind: crewmessage.EvidenceRun, Ref: "r"}, {Kind: crewmessage.EvidenceIssue, Ref: "#7"}, {Kind: crewmessage.EvidenceIssue, Ref: "8"},
	}}
	if got := crewWorkRequestParentEpic(msg); got != "#7" {
		t.Errorf("parent epic = %q, want the first issue ref #7", got)
	}
	if got := crewWorkRequestParentEpic(&crewmessage.Message{}); got != "" {
		t.Errorf("parent epic with no issue evidence = %q, want empty", got)
	}
	long := strings.Repeat("x", deferSummaryMaxLen+10)
	for _, tc := range []struct {
		p    crewmessage.Payload
		want string
	}{
		{crewmessage.Payload{Title: "T\nsecond", Summary: "S"}, "T"},
		{crewmessage.Payload{Summary: "S only"}, "S only"},
		{crewmessage.Payload{}, "Crew work request"},
		{crewmessage.Payload{Title: long}, strings.Repeat("x", deferSummaryMaxLen) + "…"},
	} {
		if got := crewWorkRequestSummary(&crewmessage.Message{Payload: tc.p}); got != tc.want {
			t.Errorf("summary(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}
