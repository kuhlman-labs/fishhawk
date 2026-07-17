package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
)

// --- fakes ---------------------------------------------------------------

// codeScanRunRepo is a minimal run.Repository for the code_scanning_alert
// ingest tests. ListRuns records the PR-URL filter (so a test can assert
// run-matching) and returns a configured result; the stage/artifact
// surface is stubbed so loadApprovedPlanForRun can resolve scope.files.
type codeScanRunRepo struct {
	run.Repository
	mu         sync.Mutex
	listURLs   []string
	listResult []*run.Run
	listErr    error
	runsByID   map[uuid.UUID]*run.Run
	stages     map[uuid.UUID][]*run.Stage
}

func (r *codeScanRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.PullRequestURL != nil {
		r.listURLs = append(r.listURLs, *f.PullRequestURL)
	}
	return r.listResult, r.listErr
}
func (r *codeScanRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if rn, ok := r.runsByID[id]; ok {
		return rn, nil
	}
	return &run.Run{ID: id}, nil
}
func (r *codeScanRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	return r.stages[id], nil
}
func (r *codeScanRunRepo) listCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.listURLs)
}

type codeScanArtifactRepo struct {
	artifact.Repository
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (r *codeScanArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	return r.byStage[stageID], nil
}

// codeScanAuditRepo records AppendChained calls and replays them back
// through ListForRun, so the idempotency dedup (which reads the run's
// prior securityscan entries) is exercised across deliveries.
type codeScanAuditRepo struct {
	audit.Repository
	mu        sync.Mutex
	seeded    []*audit.Entry
	seq       int64
	appends   []audit.ChainAppendParams
	listErr   error
	appendErr error
}

func (r *codeScanAuditRepo) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]*audit.Entry, len(r.seeded))
	copy(out, r.seeded)
	return out, nil
}
func (r *codeScanAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.appendErr != nil {
		return nil, r.appendErr
	}
	r.appends = append(r.appends, p)
	r.seq++
	r.seeded = append(r.seeded, &audit.Entry{
		Sequence: r.seq,
		Category: p.Category,
		Payload:  p.Payload,
		RunID:    &p.RunID,
	})
	return &audit.Entry{Sequence: r.seq}, nil
}
func (r *codeScanAuditRepo) securityScanEntries() []audit.ChainAppendParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, a := range r.appends {
		if a.Category == securityscan.AuditCategorySecurityFindings {
			out = append(out, a)
		}
	}
	return out
}

// seedFixup appends a stage_fixup_triggered entry so the floor advances.
func (r *codeScanAuditRepo) seedFixup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.seeded = append(r.seeded, &audit.Entry{Sequence: r.seq, Category: CategoryStageFixupTriggered})
}

type fakeCodeScanningLister struct {
	mu       sync.Mutex
	findings []securityscan.Finding
	err      error
	calls    int
}

func (l *fakeCodeScanningLister) ListCodeScanningAlerts(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ string) ([]securityscan.Finding, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return l.findings, nil
}
func (l *fakeCodeScanningLister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// --- helpers -------------------------------------------------------------

// seedRunWithPlan wires a matchable run whose plan declares the given
// scope.files paths, so the diff-intersection filter has a real diff.
func seedRunWithPlan(t *testing.T, rr *codeScanRunRepo, ar *codeScanArtifactRepo, scopePaths []string) *run.Run {
	t.Helper()
	runID := uuid.New()
	planStageID := uuid.New()
	installID := int64(99)
	rn := &run.Run{ID: runID, Repo: "octo/app", InstallationID: &installID}
	rr.listResult = []*run.Run{rn}
	if rr.runsByID == nil {
		rr.runsByID = map[uuid.UUID]*run.Run{}
	}
	rr.runsByID[runID] = rn
	if rr.stages == nil {
		rr.stages = map[uuid.UUID][]*run.Stage{}
	}
	rr.stages[runID] = []*run.Stage{{ID: planStageID, RunID: runID, Type: run.StageTypePlan}}

	files := make([]plan.ScopeFile, 0, len(scopePaths))
	for _, p := range scopePaths {
		files = append(files, plan.ScopeFile{Path: p, Operation: plan.FileOpModify})
	}
	content, err := json.Marshal(plan.Plan{Scope: plan.Scope{Files: files}})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	ver := "standard_v1"
	if ar.byStage == nil {
		ar.byStage = map[uuid.UUID][]*artifact.Artifact{}
	}
	ar.byStage[planStageID] = []*artifact.Artifact{{
		ID:            uuid.New(),
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &ver,
		Content:       content,
	}}
	return rn
}

// codeScanPayload builds a code_scanning_alert webhook body for a PR ref.
func codeScanPayload(prNumber int, headSHA string) []byte {
	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"ref":        refForPR(prNumber),
		"commit_oid": headSHA,
		"alert":      map[string]any{"number": 1},
		"repository": map[string]any{
			"full_name": "octo/app",
			"name":      "app",
			"owner":     map[string]any{"login": "octo"},
		},
		"installation": map[string]any{"id": 99},
	})
	return body
}

func refForPR(n int) string {
	return "refs/pull/" + itoa(n) + "/merge"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func decodeScanPayload(t *testing.T, raw json.RawMessage) securityScanAuditPayload {
	t.Helper()
	var p securityScanAuditPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode securityscan payload: %v", err)
	}
	return p
}

func highFinding(path string, number int) securityscan.Finding {
	return securityscan.Finding{
		Number:   number,
		RuleID:   "js/sql-injection",
		Severity: securityscan.SeverityHigh,
		State:    "open",
		Path:     path,
		Tool:     "CodeQL",
	}
}

// --- tests ---------------------------------------------------------------

func TestIngestCodeScanningAlert_MatchedRun_RecordsFinding(t *testing.T) {
	// A high-severity finding on a file in the run's implement diff is
	// matched to the run and recorded as ONE securityscan audit entry.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{}
	rn := seedRunWithPlan(t, rr, ar, []string{"backend/internal/server/codescanning.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	lister := &fakeCodeScanningLister{findings: []securityscan.Finding{
		highFinding("backend/internal/server/codescanning.go", 1),
	}}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "deadbeef"), lister)

	if rr.listCallCount() != 1 || rr.listURLs[0] != "https://github.com/octo/app/pull/42" {
		t.Fatalf("run lookup = %+v, want one PR-url lookup", rr.listURLs)
	}
	entries := au.securityScanEntries()
	if len(entries) != 1 {
		t.Fatalf("securityscan entries = %d, want 1", len(entries))
	}
	got := decodeScanPayload(t, entries[0].Payload)
	if len(got.Findings) != 1 || got.Findings[0].Number != 1 {
		t.Errorf("recorded findings = %+v, want the high finding #1", got.Findings)
	}
	if got.HeadSHA != "deadbeef" || got.PRNumber != 42 {
		t.Errorf("payload head/pr = %q/%d, want deadbeef/42", got.HeadSHA, got.PRNumber)
	}
	if entries[0].RunID != rn.ID {
		t.Errorf("entry run id = %v, want %v", entries[0].RunID, rn.ID)
	}
}

func TestIngestCodeScanningAlert_FindingOffDiff_NoEntry(t *testing.T) {
	// A high-severity finding on a file NOT in the implement diff is
	// pre-existing repo debt, not introduced here: it must not gate, so
	// no entry is recorded.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{}
	seedRunWithPlan(t, rr, ar, []string{"backend/internal/server/codescanning.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	lister := &fakeCodeScanningLister{findings: []securityscan.Finding{
		highFinding("some/other/untouched.go", 7),
	}}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "deadbeef"), lister)

	if n := len(au.securityScanEntries()); n != 0 {
		t.Fatalf("securityscan entries = %d, want 0 (finding off-diff)", n)
	}
}

func TestIngestCodeScanningAlert_UnmatchedRun_NoOp(t *testing.T) {
	// PR not managed by Fishhawk: ListRuns returns empty. The handler
	// short-circuits before fetching alerts or recording anything.
	rr := &codeScanRunRepo{listResult: nil}
	au := &codeScanAuditRepo{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: &codeScanArtifactRepo{}, AuditRepo: au})

	lister := &fakeCodeScanningLister{findings: []securityscan.Finding{highFinding("a.go", 1)}}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "deadbeef"), lister)

	if rr.listCallCount() != 1 {
		t.Errorf("expected one run lookup; got %d", rr.listCallCount())
	}
	if lister.callCount() != 0 {
		t.Errorf("unmatched run must not fetch alerts; got %d fetches", lister.callCount())
	}
	if n := len(au.securityScanEntries()); n != 0 {
		t.Errorf("securityscan entries = %d, want 0", n)
	}
}

func TestIngestCodeScanningAlert_NonPullRef_NoOp(t *testing.T) {
	// A branch-ref alert (e.g. a post-merge default-branch scan) maps to
	// no PR: no run lookup, no fetch, no entry.
	rr := &codeScanRunRepo{}
	au := &codeScanAuditRepo{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: &codeScanArtifactRepo{}, AuditRepo: au})

	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"ref":        "refs/heads/main",
		"commit_oid": "deadbeef",
		"repository": map[string]any{"full_name": "octo/app", "name": "app", "owner": map[string]any{"login": "octo"}},
	})
	lister := &fakeCodeScanningLister{}
	s.ingestCodeScanningAlertWith(context.Background(), body, lister)

	if rr.listCallCount() != 0 {
		t.Errorf("branch-ref alert must not trigger a run lookup; got %d", rr.listCallCount())
	}
	if lister.callCount() != 0 {
		t.Errorf("branch-ref alert must not fetch alerts; got %d", lister.callCount())
	}
}

func TestIngestCodeScanningAlert_Idempotent_DuplicateDelivery(t *testing.T) {
	// GitHub delivers one code_scanning_alert per alert; every delivery
	// re-fetches the same set. A second identical delivery must NOT record
	// a second entry.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{}
	seedRunWithPlan(t, rr, ar, []string{"a.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	lister := &fakeCodeScanningLister{findings: []securityscan.Finding{highFinding("a.go", 1)}}
	body := codeScanPayload(42, "deadbeef")
	s.ingestCodeScanningAlertWith(context.Background(), body, lister)
	s.ingestCodeScanningAlertWith(context.Background(), body, lister)

	if n := len(au.securityScanEntries()); n != 1 {
		t.Fatalf("securityscan entries after duplicate delivery = %d, want 1", n)
	}
}

func TestIngestCodeScanningAlert_CleanRescanAfterFixup_NoNoiseEntry(t *testing.T) {
	// After a fix-up, the floor advances past the prior dirty entry. A
	// clean re-scan records nothing new (the gate clears via the floor —
	// the dirty entry is now below it). The dirty entry remains for the
	// audit trail; no spurious clean entry is appended.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{}
	seedRunWithPlan(t, rr, ar, []string{"a.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	// First delivery: a high finding → one dirty entry.
	dirty := &fakeCodeScanningLister{findings: []securityscan.Finding{highFinding("a.go", 1)}}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "sha1"), dirty)
	if n := len(au.securityScanEntries()); n != 1 {
		t.Fatalf("after first delivery securityscan entries = %d, want 1", n)
	}

	// A fix-up runs; the floor advances.
	au.seedFixup()

	// Clean re-scan on the fixed head: no high findings on the diff.
	clean := &fakeCodeScanningLister{findings: nil}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "sha2"), clean)

	if n := len(au.securityScanEntries()); n != 1 {
		t.Fatalf("after clean re-scan securityscan entries = %d, want 1 (no spurious clean entry)", n)
	}
}

func TestIngestCodeScanningAlert_FetchError_NoEntry(t *testing.T) {
	// A REST fetch error is best-effort: no entry this delivery, no panic.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{}
	seedRunWithPlan(t, rr, ar, []string{"a.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	lister := &fakeCodeScanningLister{err: errors.New("boom")}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "deadbeef"), lister)

	if n := len(au.securityScanEntries()); n != 0 {
		t.Errorf("securityscan entries after fetch error = %d, want 0", n)
	}
}

func TestIngestCodeScanningAlert_NoClient_NoFetch(t *testing.T) {
	// Dev posture: no GitHub client wired. The matched run is found but no
	// fetch happens and nothing is recorded.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{}
	seedRunWithPlan(t, rr, ar, []string{"a.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "deadbeef"), nil)

	if n := len(au.securityScanEntries()); n != 0 {
		t.Errorf("securityscan entries with no client = %d, want 0", n)
	}
}

func TestIngestCodeScanningAlert_MalformedPayload_NoCrash(t *testing.T) {
	rr := &codeScanRunRepo{}
	au := &codeScanAuditRepo{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: &codeScanArtifactRepo{}, AuditRepo: au})

	lister := &fakeCodeScanningLister{}
	s.ingestCodeScanningAlertWith(context.Background(), []byte(`{not json`), lister)

	if rr.listCallCount() != 0 {
		t.Errorf("malformed payload must not trigger a run lookup; got %d", rr.listCallCount())
	}
	if lister.callCount() != 0 {
		t.Errorf("malformed payload must not fetch; got %d", lister.callCount())
	}
}

func TestIngestCodeScanningAlert_EmptyRepoFullName_NoOp(t *testing.T) {
	// A valid PR ref but no repository full_name can't build a PR URL: no
	// run lookup, no fetch.
	rr := &codeScanRunRepo{}
	au := &codeScanAuditRepo{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: &codeScanArtifactRepo{}, AuditRepo: au})

	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"ref":        "refs/pull/42/merge",
		"commit_oid": "deadbeef",
		"repository": map[string]any{}, // no full_name
	})
	lister := &fakeCodeScanningLister{}
	s.ingestCodeScanningAlertWith(context.Background(), body, lister)

	if rr.listCallCount() != 0 {
		t.Errorf("empty full_name must not trigger a run lookup; got %d", rr.listCallCount())
	}
	if lister.callCount() != 0 {
		t.Errorf("empty full_name must not fetch; got %d", lister.callCount())
	}
}

func TestIngestCodeScanningAlert_AuditReadError_NoEntry(t *testing.T) {
	// A read error reading the run's prior audit entries is best-effort:
	// the record step bails without appending and without panicking.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{listErr: errors.New("db down")}
	seedRunWithPlan(t, rr, ar, []string{"a.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	lister := &fakeCodeScanningLister{findings: []securityscan.Finding{highFinding("a.go", 1)}}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "deadbeef"), lister)

	if n := len(au.securityScanEntries()); n != 0 {
		t.Errorf("securityscan entries after audit read error = %d, want 0", n)
	}
}

func TestIngestCodeScanningAlert_AuditAppendError_NoPanic(t *testing.T) {
	// An append failure is logged and swallowed (best-effort) — no entry
	// lands and the handler returns cleanly.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codeScanAuditRepo{appendErr: errors.New("append failed")}
	seedRunWithPlan(t, rr, ar, []string{"a.go"})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	lister := &fakeCodeScanningLister{findings: []securityscan.Finding{highFinding("a.go", 1)}}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(42, "deadbeef"), lister)

	if n := len(au.securityScanEntries()); n != 0 {
		t.Errorf("securityscan entries after append error = %d, want 0", n)
	}
}

func TestIngestCodeScanningAlert_MissingDeps_NoOp(t *testing.T) {
	// Unconfigured server (no RunRepo / AuditRepo) tolerates the call.
	s := New(Config{Addr: "127.0.0.1:0"})
	lister := &fakeCodeScanningLister{}
	s.ingestCodeScanningAlertWith(context.Background(), codeScanPayload(1, "abc"), lister)
	if lister.callCount() != 0 {
		t.Errorf("missing deps must not fetch; got %d", lister.callCount())
	}
}

func TestPullNumberFromRef(t *testing.T) {
	cases := []struct {
		ref    string
		want   int
		wantOK bool
	}{
		{"refs/pull/42/merge", 42, true},
		{"refs/pull/7/head", 7, true},
		{"refs/heads/main", 0, false},
		{"refs/pull//merge", 0, false},
		{"refs/pull/abc/merge", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := pullNumberFromRef(c.ref)
		if got != c.want || ok != c.wantOK {
			t.Errorf("pullNumberFromRef(%q) = (%d,%v), want (%d,%v)", c.ref, got, ok, c.want, c.wantOK)
		}
	}
}
