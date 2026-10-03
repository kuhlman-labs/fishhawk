package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// Upkeep scan evidence gather tests (#3922, slice 2). NONE of these tests is
// parallel: they swap the package-var seams (newUpkeepPinSource,
// upkeepListWorkflowDir) and bounds (upkeepFlakeBundleCap, …), restoring them
// in t.Cleanup.
//
// The flake fixture's ListRuns DELIBERATELY ignores its Repo / AccountID
// filter, so the Go-side ownership predicate, window cutoff and self-skip are
// the only things that can exclude a run — each is a real control here.

// upkeepMarker appears only in buildUpkeepScan's output.
const upkeepMarker = "You are producing an upkeep scan report"

const upkeepTestCommit = "0123456789abcdef0123456789abcdef01234567"

// --- fakes -----------------------------------------------------------------

// fakeUpkeepPinSource serves files from a map; an unlisted path is
// forge.ErrNotFound.
type fakeUpkeepPinSource struct {
	mu        sync.Mutex
	files     map[string]string
	errs      map[string]error
	listed    []string
	listErr   error
	block     time.Duration // > 0: FetchFile blocks until ctx is done (or this wedge)
	fetched   []string
	listCalls int
}

func (f *fakeUpkeepPinSource) FetchFile(ctx context.Context, p string) ([]byte, error) {
	f.mu.Lock()
	f.fetched = append(f.fetched, p)
	block := f.block
	f.mu.Unlock()
	if block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(block):
			return nil, errors.New("fake pin source: wedge elapsed without a ctx cancel")
		}
	}
	if err := f.errs[p]; err != nil {
		return nil, err
	}
	c, ok := f.files[p]
	if !ok {
		return nil, forge.ErrNotFound
	}
	return []byte(c), nil
}

func (f *fakeUpkeepPinSource) ListWorkflowFiles(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.listed, f.listErr
}

func (f *fakeUpkeepPinSource) wasFetched(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.fetched {
		if q == p {
			return true
		}
	}
	return false
}

// installUpkeepPinSource swaps the newUpkeepPinSource seam for src and
// returns a counter of seam calls.
func installUpkeepPinSource(t *testing.T, src upkeepPinSource) *int {
	t.Helper()
	calls := new(int)
	prev := newUpkeepPinSource
	newUpkeepPinSource = func(context.Context, *Server, *run.Run) (upkeepPinSource, string) {
		*calls++
		return src, ""
	}
	t.Cleanup(func() { newUpkeepPinSource = prev })
	return calls
}

// setUpkeepBound swaps one package-var bound for the test.
func setUpkeepBound[T any](t *testing.T, p *T, v T) {
	t.Helper()
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}

// upkeepEvidenceRunRepo serves every seeded run from ListRuns, IGNORING the
// Repo / AccountID filter, ordered created_at DESC, id DESC like the real
// query, with offset/limit applied.
type upkeepEvidenceRunRepo struct {
	*upkeepRunRepo

	listRunsErr   error
	listRunsCalls int
}

func (r *upkeepEvidenceRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	r.listRunsCalls++
	err := r.listRunsErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	all := make([]*run.Run, 0, len(r.getRuns))
	for _, rn := range r.getRuns {
		all = append(all, rn)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID.String() > all[j].ID.String()
	})
	if f.Offset >= len(all) {
		return nil, nil
	}
	all = all[f.Offset:]
	if len(all) > f.Limit {
		all = all[:f.Limit]
	}
	return all, nil
}

func (r *upkeepEvidenceRunRepo) runListCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listRunsCalls
}

// upkeepTraceStore serves bundles by content hash and counts Gets.
type upkeepTraceStore struct {
	mu     sync.Mutex
	bodies map[string][]byte
	gets   int
	getErr error
	block  time.Duration // > 0: Get blocks until ctx is done (or this wedge)
}

func (s *upkeepTraceStore) Put(context.Context, tracestore.BundleRef, io.Reader) error { return nil }
func (s *upkeepTraceStore) Get(ctx context.Context, ref tracestore.BundleRef) (io.ReadCloser, error) {
	s.mu.Lock()
	s.gets++
	block, err := s.block, s.getErr
	b, ok := s.bodies[ref.ContentHash]
	s.mu.Unlock()
	if block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(block):
			return nil, errors.New("upkeepTraceStore: wedge elapsed without a ctx cancel")
		}
	}
	if err != nil {
		return nil, err
	}
	if !ok || ref.Variant != tracestore.VariantRedacted {
		return nil, errors.New("upkeepTraceStore: no redacted body for " + ref.ContentHash)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (s *upkeepTraceStore) Stat(context.Context, tracestore.BundleRef) (tracestore.Stat, error) {
	return tracestore.Stat{}, errors.New("upkeepTraceStore: Stat not used")
}
func (s *upkeepTraceStore) List(context.Context, uuid.UUID) ([]tracestore.BundleRef, error) {
	return nil, errors.New("upkeepTraceStore: List not used")
}

func (s *upkeepTraceStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// --- fixture ---------------------------------------------------------------

type upkeepEvidenceFixture struct {
	s         *Server
	rr        *upkeepEvidenceRunRepo
	au        *auditFake
	ts        *upkeepTraceStore
	sf        *signingFake
	runRow    *run.Run
	planStage *run.Stage
	implStage *run.Stage
	logs      *bytes.Buffer
	seq       int64
	nHash     int
}

// newUpkeepEvidenceFixture seeds the SCANNING run (untenanted, so the router's
// ownership middleware serves the identity-less test caller) with the given
// cached spec, a recorded document base commit, and its plan + implement
// stages.
func newUpkeepEvidenceFixture(t *testing.T, wfSpec []byte, workflowID string) *upkeepEvidenceFixture {
	t.Helper()
	rr := &upkeepEvidenceRunRepo{upkeepRunRepo: newUpkeepRunRepo()}
	commit := upkeepTestCommit
	runRow := &run.Run{
		ID: uuid.New(), Repo: upkeepTestRepo, WorkflowID: workflowID, WorkflowSpec: wfSpec,
		State: run.StateRunning, CreatedAt: time.Now(), DocumentBaseCommit: &commit,
	}
	rr.seedRun(runRow)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Sequence: 0, Type: run.StageTypePlan, State: run.StageStateRunning}
	implStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Sequence: 1, Type: run.StageTypeImplement, State: run.StageStatePending}
	rr.getStages[planStage.ID] = planStage
	rr.getStages[implStage.ID] = implStage
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runRow.ID: {implStage, planStage}}

	f := &upkeepEvidenceFixture{
		rr: rr, au: newAuditFake(), ts: &upkeepTraceStore{bodies: map[string][]byte{}}, sf: newSigningFake(),
		runRow: runRow, planStage: planStage, implStage: implStage, logs: &bytes.Buffer{},
	}
	f.s = New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: f.sf, AuditRepo: f.au, TraceStore: f.ts,
		Logger: slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	f.s.promptIssueGetterOverride = &stubIssueGetter{}
	return f
}

// seedFlakeRun seeds a run of repo/account created `age` ago with one
// implement stage. A non-empty gateJSON also seeds that stage's
// trace_uploaded row and redacted bundle.
func (f *upkeepEvidenceFixture) seedFlakeRun(t *testing.T, repo, account string, age time.Duration, gateJSON string) (*run.Run, *run.Stage) {
	t.Helper()
	rn := &run.Run{ID: uuid.New(), Repo: repo, AccountID: account, State: run.StateSucceeded, CreatedAt: time.Now().Add(-age)}
	f.rr.seedRun(rn)
	st := f.addImplStage(rn)
	if gateJSON != "" {
		f.seedTrace(t, rn.ID, st.ID, makeRedactedGateEvidenceBundle(t, gateJSON))
	}
	return rn, st
}

func (f *upkeepEvidenceFixture) addImplStage(rn *run.Run) *run.Stage {
	st := &run.Stage{ID: uuid.New(), RunID: rn.ID, Sequence: len(f.rr.stagesByRunID[rn.ID]) + 1, Type: run.StageTypeImplement, State: run.StageStateSucceeded}
	f.rr.getStages[st.ID] = st
	f.rr.stagesByRunID[rn.ID] = append(f.rr.stagesByRunID[rn.ID], st)
	return st
}

func (f *upkeepEvidenceFixture) seedTrace(t *testing.T, runID, stageID uuid.UUID, body []byte) {
	t.Helper()
	f.nHash++
	f.seq++
	hash := fmt.Sprintf("%064x", f.nHash)
	f.au.mu.Lock()
	f.au.seeded = append(f.au.seeded, makeTraceUploadedEntry(t, f.seq, runID, stageID, "redacted", hash))
	f.au.mu.Unlock()
	f.ts.mu.Lock()
	f.ts.bodies[hash] = body
	f.ts.mu.Unlock()
}

func (f *upkeepEvidenceFixture) resolve(t *testing.T) *prompt.UpkeepScanContext {
	t.Helper()
	u, err := f.s.resolveUpkeepScanContext(context.Background(), f.runRow, f.planStage)
	if err != nil {
		t.Fatalf("resolveUpkeepScanContext: %v", err)
	}
	if u == nil {
		t.Fatal("resolveUpkeepScanContext = nil for an upkeep-declaring stage")
	}
	return u
}

// flakeGate is gate evidence for one verify that FAILED with tail on tree,
// then PASSED on the same tree.
func flakeGate(tail, tree string) string {
	b, _ := json.Marshal(map[string]any{"verify_runs": []map[string]any{
		{"command": "scripts/test verify", "exit_code": 1, "outcome": "failed", "tree_sha": tree, "output_tail": tail},
		{"command": "scripts/test verify", "exit_code": 0, "outcome": "passed", "tree_sha": tree},
	}})
	return string(b)
}

func failTail(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "--- FAIL: %s (0.01s)\n", n)
	}
	return b.String() + "FAIL\n"
}

// subjects maps flake subject → fact.
func subjects(u *prompt.UpkeepScanContext) map[string]prompt.UpkeepFlakeFact {
	out := map[string]prompt.UpkeepFlakeFact{}
	for _, f := range u.Flakes {
		out[f.Subject] = f
	}
	return out
}

func degradeCount(u *prompt.UpkeepScanContext, source, reason string) int {
	for _, d := range u.Degrades {
		if d.Source == source && d.Reason == reason {
			return d.Count
		}
	}
	return 0
}

func citesRun(f prompt.UpkeepFlakeFact, runID uuid.UUID) bool {
	for _, r := range f.Refs {
		if r.RunID == runID.String() {
			return true
		}
	}
	return false
}

// --- pin gather: the core ------------------------------------------------------

func TestGatherUpkeepPinFiles_ReadsFixedSetWorkspaceAndWorkflows(t *testing.T) {
	src := &fakeUpkeepPinSource{
		files: map[string]string{
			"go.work":                      "go 1.25.0\n\nuse (\n\t./backend\n\t./cli // the CLI\n\t.\n)\nuse ./runner\n",
			"backend/go.mod":               "module x\n\ngo 1.25.0\n",
			"cli/go.mod":                   "module y\n\ngo 1.25\n",
			"go.mod":                       "module root\n\ngo 1.25\n",
			".golangci.yaml":               "version: \"2\"\nrun:\n  go: \"1.25\"\n",
			".github/workflows/ci.yml":     "x\n",
			".github/workflows/rel.yaml":   "y\n",
			".github/workflows/README.md":  "not yaml\n",
			"AGENTS.md":                    "agents\n",
			"docs/api/README.md":           "api\n",
			".github/workflows/nested/a.y": "nested\n",
		},
		listed: []string{".github/workflows/rel.yaml", ".github/workflows/ci.yml", ".github/workflows/README.md", ".github/workflows/nested/a.yml"},
	}
	files, reasons := gatherUpkeepPinFiles(context.Background(), src)
	if len(reasons) != 0 {
		t.Errorf("reasons = %v, want none (absent files are not degrades)", reasons)
	}
	want := []string{
		".github/workflows/ci.yml", ".github/workflows/rel.yaml", ".golangci.yaml", "AGENTS.md",
		"backend/go.mod", "cli/go.mod", "docs/api/README.md", "go.mod", "go.work",
	}
	got := make([]string, 0, len(files))
	for p := range files {
		got = append(got, p)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("files read = %v, want %v", got, want)
	}
	// runner/go.mod is `use`d but absent: fetched, not a degrade. The
	// .golangci.yml and docs/api/v0.md are absent too.
	for _, p := range []string{"runner/go.mod", ".golangci.yml", "docs/api/v0.md"} {
		if !src.wasFetched(p) {
			t.Errorf("%s was not fetched", p)
		}
	}
	if src.wasFetched(".github/workflows/README.md") || src.wasFetched(".github/workflows/nested/a.yml") {
		t.Errorf("a non-YAML or nested workflow entry was fetched: %v", src.fetched)
	}
}

func TestGoWorkUseDirs(t *testing.T) {
	work := "go 1.25.0\nuse ./a\nuse (\n  ./c\n  \"./b\"\n  ../escape\n  /abs\n  .\n)\nuse(./d)\nuser ./nope\n"
	if got := strings.Join(goWorkUseDirs(work), ","); got != "a,b,c,d" {
		t.Errorf("goWorkUseDirs = %q, want a,b,c,d", got)
	}
}

// One test per pin degrade the core can produce.
func TestGatherUpkeepPinFiles_Degrades(t *testing.T) {
	t.Run("pin_fetch_failed", func(t *testing.T) {
		src := &fakeUpkeepPinSource{files: map[string]string{"AGENTS.md": "x"}, errs: map[string]error{"AGENTS.md": errors.New("502")}}
		files, reasons := gatherUpkeepPinFiles(context.Background(), src)
		if reasons[upkeepDegradePinFetchFailed] != 1 || len(files) != 0 {
			t.Errorf("reasons = %v files = %v, want one pin_fetch_failed and nothing read", reasons, files)
		}
	})
	t.Run("pin_file_too_large", func(t *testing.T) {
		setUpkeepBound(t, &upkeepPinFileMaxBytes, 8)
		src := &fakeUpkeepPinSource{files: map[string]string{"AGENTS.md": "123456789", "go.mod": "go 1.25"}}
		files, reasons := gatherUpkeepPinFiles(context.Background(), src)
		if reasons[upkeepDegradePinFileTooLarge] != 1 {
			t.Errorf("reasons = %v, want one pin_file_too_large", reasons)
		}
		if _, ok := files["AGENTS.md"]; ok {
			t.Error("the oversized file was kept")
		}
		if _, ok := files["go.mod"]; !ok {
			t.Error("the in-bound file was dropped")
		}
	})
	t.Run("workflow_listing_unavailable", func(t *testing.T) {
		src := &fakeUpkeepPinSource{listErr: errUpkeepWorkflowListingUnavailable}
		_, reasons := gatherUpkeepPinFiles(context.Background(), src)
		if reasons[upkeepDegradeWorkflowListingUnavailable] != 1 || len(reasons) != 1 {
			t.Errorf("reasons = %v, want only workflow_listing_unavailable", reasons)
		}
	})
	t.Run("workflow_listing_failed", func(t *testing.T) {
		src := &fakeUpkeepPinSource{listErr: errors.New("500")}
		_, reasons := gatherUpkeepPinFiles(context.Background(), src)
		if reasons[upkeepDegradeWorkflowListingFailed] != 1 || len(reasons) != 1 {
			t.Errorf("reasons = %v, want only workflow_listing_failed", reasons)
		}
	})
	t.Run("absent workflow directory is not a degrade", func(t *testing.T) {
		src := &fakeUpkeepPinSource{listErr: fmt.Errorf("list: %w", forge.ErrNotFound)}
		if _, reasons := gatherUpkeepPinFiles(context.Background(), src); len(reasons) != 0 {
			t.Errorf("reasons = %v, want none", reasons)
		}
	})
	t.Run("workspace_dirs_capped", func(t *testing.T) {
		setUpkeepBound(t, &upkeepPinMaxWorkspaceDirs, 2)
		src := &fakeUpkeepPinSource{files: map[string]string{"go.work": "use (\n./a\n./b\n./c\n)\n"}}
		_, reasons := gatherUpkeepPinFiles(context.Background(), src)
		if reasons[upkeepDegradeWorkspaceDirsCapped] != 1 {
			t.Errorf("reasons = %v, want workspace_dirs_capped = 1", reasons)
		}
		if !src.wasFetched("b/go.mod") || src.wasFetched("c/go.mod") {
			t.Errorf("fetched = %v, want a/ and b/ read and c/ cut", src.fetched)
		}
	})
	t.Run("workflow_files_capped", func(t *testing.T) {
		setUpkeepBound(t, &upkeepPinMaxWorkflowFiles, 1)
		src := &fakeUpkeepPinSource{listed: []string{".github/workflows/b.yml", ".github/workflows/a.yml", ".github/workflows/c.yaml"}}
		_, reasons := gatherUpkeepPinFiles(context.Background(), src)
		if reasons[upkeepDegradeWorkflowFilesCapped] != 2 {
			t.Errorf("reasons = %v, want workflow_files_capped = 2", reasons)
		}
		if !src.wasFetched(".github/workflows/a.yml") || src.wasFetched(".github/workflows/b.yml") {
			t.Errorf("fetched = %v, want the first sorted workflow only", src.fetched)
		}
	})
	t.Run("budget_exceeded", func(t *testing.T) {
		wedge := timescale.D(5 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(50*time.Millisecond))
		defer cancel()
		src := &fakeUpkeepPinSource{block: wedge}
		start := time.Now()
		_, reasons := gatherUpkeepPinFiles(ctx, src)
		if el := time.Since(start); el >= wedge {
			t.Fatalf("gather took %v, not cut by the budget (wedge %v)", el, wedge)
		}
		if reasons[upkeepDegradeBudgetExceeded] == 0 {
			t.Errorf("reasons = %v, want budget_exceeded", reasons)
		}
		if len(src.fetched) != 1 || src.listCalls != 0 {
			t.Errorf("fetched = %v list calls = %d, want only the first read attempted", src.fetched, src.listCalls)
		}
	})
}

// --- pin gather: the production binding --------------------------------------

// upkeepRefFetcher is a forge file fetcher that asserts the ref it is asked
// for and serves files from a map.
type upkeepRefFetcher struct {
	t       *testing.T
	wantRef string
	never   bool
	files   map[string]string
	refs    []string
}

func (f *upkeepRefFetcher) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, p, ref string) (*forge.FileContent, error) {
	if f.never {
		f.t.Fatalf("pin fetcher called (path %q, ref %q) on a run with no recorded base commit", p, ref)
	}
	f.refs = append(f.refs, ref)
	if ref != f.wantRef {
		f.t.Errorf("pin fetch of %s at ref %q, want the recorded commit %q", p, ref, f.wantRef)
	}
	c, ok := f.files[p]
	if !ok {
		return nil, forge.ErrNotFound
	}
	return &forge.FileContent{Path: p, Content: []byte(c)}, nil
}

func TestUpkeepForgePinSource_Binding(t *testing.T) {
	newRun := func() *run.Run {
		c := upkeepTestCommit
		return &run.Run{ID: uuid.New(), Repo: upkeepTestRepo, DocumentBaseCommit: &c}
	}

	t.Run("pin_reader_unwired", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if src, reason := s.upkeepForgePinSource(context.Background(), newRun()); src != nil || reason != upkeepDegradePinReaderUnwired {
			t.Errorf("= (%v, %q), want pin_reader_unwired", src, reason)
		}
	})
	t.Run("base_commit_unrecorded never fetches", func(t *testing.T) {
		never := &upkeepRefFetcher{t: t, never: true}
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		f.s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: never}
		f.runRow.DocumentBaseCommit = nil
		u := f.resolve(t)
		if degradeCount(u, plan.UpkeepSourceToolchainDrift, upkeepDegradeBaseCommitUnrecorded) != 1 {
			t.Errorf("degrades = %+v, want base_commit_unrecorded", u.Degrades)
		}
		if u.BaseCommit != "" || u.PinFilesScanned != 0 {
			t.Errorf("BaseCommit = %q scanned = %d, want none", u.BaseCommit, u.PinFilesScanned)
		}
	})
	t.Run("repo_malformed", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: &upkeepRefFetcher{t: t, never: true}}
		rn := newRun()
		rn.Repo = "no-slash"
		if _, reason := s.upkeepForgePinSource(context.Background(), rn); reason != upkeepDegradeRepoMalformed {
			t.Errorf("reason = %q, want repo_malformed", reason)
		}
	})
	t.Run("scope_unavailable", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: &upkeepRefFetcher{t: t, never: true}}
		s.cfg.DocumentScope = func(context.Context, forge.RepoRef) (forge.CredentialScope, error) {
			return forge.CredentialScope{}, errors.New("no installation")
		}
		if _, reason := s.upkeepForgePinSource(context.Background(), newRun()); reason != upkeepDegradeScopeUnavailable {
			t.Errorf("reason = %q, want scope_unavailable", reason)
		}
	})
	t.Run("reads at the recorded commit and lists workflows there", func(t *testing.T) {
		fetcher := &upkeepRefFetcher{t: t, wantRef: upkeepTestCommit, files: map[string]string{
			".github/workflows/a.yml": "curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.8.0/install.sh | sh\n",
			".github/workflows/b.yml": "curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.9.0/install.sh | sh\n",
		}}
		var listRef string
		prev := upkeepListWorkflowDir
		upkeepListWorkflowDir = func(_ context.Context, _ *Server, _ forge.CredentialScope, _ forge.RepoRef, ref string) ([]string, bool, error) {
			listRef = ref
			return []string{".github/workflows/a.yml", ".github/workflows/b.yml"}, true, nil
		}
		t.Cleanup(func() { upkeepListWorkflowDir = prev })
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		f.s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: fetcher}
		u := f.resolve(t)
		if listRef != upkeepTestCommit {
			t.Errorf("workflow listing ref = %q, want %q", listRef, upkeepTestCommit)
		}
		if len(fetcher.refs) == 0 {
			t.Fatal("the production fetcher was never called")
		}
		if u.BaseCommit != upkeepTestCommit || u.PinFilesScanned != 2 {
			t.Errorf("BaseCommit = %q scanned = %d, want %q and 2", u.BaseCommit, u.PinFilesScanned, upkeepTestCommit)
		}
		if len(u.PinDrift) != 1 || u.PinDrift[0].Family != upkeep.PinFamilyGolangciLint {
			t.Errorf("drift = %+v, want one golangci-lint drift", u.PinDrift)
		}
	})
	t.Run("gitlab run: workflow_listing_unavailable", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		f.s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: &upkeepRefFetcher{t: t, wantRef: upkeepTestCommit}}
		ref := "gitlab:5"
		f.runRow.InstallationRef = &ref
		// A listing seam that WOULD answer, so only the forge check can make
		// the listing unavailable.
		prev := upkeepListWorkflowDir
		upkeepListWorkflowDir = func(context.Context, *Server, forge.CredentialScope, forge.RepoRef, string) ([]string, bool, error) {
			t.Error("the GitHub workflow listing was called for a GitLab run")
			return nil, true, nil
		}
		t.Cleanup(func() { upkeepListWorkflowDir = prev })
		u := f.resolve(t)
		if degradeCount(u, plan.UpkeepSourceToolchainDrift, upkeepDegradeWorkflowListingUnavailable) != 1 {
			t.Errorf("degrades = %+v, want workflow_listing_unavailable", u.Degrades)
		}
	})
	t.Run("github run without a GitHub client: workflow_listing_unavailable", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		f.s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: &upkeepRefFetcher{t: t, wantRef: upkeepTestCommit}}
		u := f.resolve(t)
		if degradeCount(u, plan.UpkeepSourceToolchainDrift, upkeepDegradeWorkflowListingUnavailable) != 1 {
			t.Errorf("degrades = %+v, want workflow_listing_unavailable", u.Degrades)
		}
	})
}

// The production listing reads the Contents API through cfg.GitHub and keeps
// file entries only; a failing listing surfaces its error.
func TestUpkeepListWorkflowDir_ContentsAPI(t *testing.T) {
	cf := &contentsFake{dirs: map[string][]string{upkeepWorkflowDir: {"a.yml", "b.yaml"}}}
	s := New(Config{Addr: "127.0.0.1:0"})
	s.cfg.GitHub = newTestSweepGitHub(t, cf)
	repo := forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}
	paths, ok, err := upkeepListWorkflowDir(context.Background(), s, forge.FromGitHubInstallationID(7), repo, upkeepTestCommit)
	if err != nil || !ok || strings.Join(paths, ",") != ".github/workflows/a.yml,.github/workflows/b.yaml" {
		t.Fatalf("= (%v, %v, %v), want the two workflow paths", paths, ok, err)
	}
	cf.status = http.StatusInternalServerError
	if _, ok, err := upkeepListWorkflowDir(context.Background(), s, forge.FromGitHubInstallationID(7), repo, upkeepTestCommit); err == nil || !ok {
		t.Errorf("ok=%v err=%v, want an available listing that failed", ok, err)
	}
}

// --- pin facts through the renderer (approval conditions 1, 3, 4) ------------

// Condition 1: real-repo-shaped QUOTED go pins go through DetectPinDrift into
// the renderer and render un-withheld. Condition 3: the root go.mod and the
// .golangci.yaml spelling are read, and an unnormalizable go-version literal
// ('stable', '1.25.x', a matrix list) contributes no occurrence.
func TestUpkeepScan_QuotedAndUnnormalizableGoPins(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	installUpkeepPinSource(t, &fakeUpkeepPinSource{
		files: map[string]string{
			"go.mod":         "module root\n\ngo 1.25.0\n",
			".golangci.yaml": "version: \"2\"\nrun:\n  go: \"1.22\"\n",
			".github/workflows/ci.yml": "jobs:\n  t:\n    steps:\n      - uses: actions/setup-go@v5\n        with:\n          go-version: '1.25'\n" +
				"      - with:\n          go-version: stable\n      - with:\n          go-version: '1.25.x'\n      - with:\n          go-version: [1.24, 1.26]\n",
		},
		listed: []string{".github/workflows/ci.yml"},
	})
	u := f.resolve(t)
	if len(u.PinDrift) != 1 || u.PinDrift[0].Family != upkeep.PinFamilyGo {
		t.Fatalf("drift = %+v, want one go drift", u.PinDrift)
	}
	var vals []string
	for _, o := range u.PinDrift[0].Occurrences {
		vals = append(vals, o.Path+":"+strconv.Itoa(o.Line)+"="+o.Value)
	}
	// Exactly the three normalizable pins; stable / 1.25.x / the list are skipped.
	want := ".github/workflows/ci.yml:6=1.25,.golangci.yaml:3=1.22,go.mod:3=1.25.0"
	if got := strings.Join(vals, ","); got != want {
		t.Errorf("occurrences = %s, want %s", got, want)
	}
	text, err := prompt.Build("plan", prompt.Trigger{Repo: upkeepTestRepo, Upkeep: u})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	line := factLine(t, text, "- family go:")
	for _, frag := range []string{".github/workflows/ci.yml:6 = 1.25", ".golangci.yaml:3 = 1.22", "go.mod:3 = 1.25.0"} {
		if !strings.Contains(line, frag) {
			t.Errorf("go drift line %q lacks %q", line, frag)
		}
	}
	if strings.Contains(line, prompt.UpkeepFactWithheld) {
		t.Errorf("a quoted pin rendered withheld: %q", line)
	}
}

// With only unnormalizable selectors disagreeing, there is no go drift.
func TestUpkeepScan_UnnormalizableGoVersionIsNotAPin(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	installUpkeepPinSource(t, &fakeUpkeepPinSource{
		files: map[string]string{
			"go.work":                  "go 1.25.0\n",
			".github/workflows/ci.yml": "      go-version: stable\n      go-version: '1.24.x'\n      go-version: [1.23, 1.24]\n",
		},
		listed: []string{".github/workflows/ci.yml"},
	})
	u := f.resolve(t)
	if len(u.PinDrift) != 0 {
		t.Errorf("drift = %+v, want none: an unnormalizable go-version is not a pin", u.PinDrift)
	}
}

// Condition 4: the detector's per-family occurrence cap and the flake-subject
// cap are threaded into the prompt's omitted-count lines.
func TestUpkeepScan_DetectorCapsReachThePrompt(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	files := map[string]string{}
	var listed []string
	n := upkeep.PinMaxOccurrences + 3
	for i := 0; i < n; i++ {
		p := fmt.Sprintf(".github/workflows/w%02d.yml", i)
		files[p] = fmt.Sprintf("      go-version: '1.%d'\n", 20+i%2)
		listed = append(listed, p)
	}
	installUpkeepPinSource(t, &fakeUpkeepPinSource{files: files, listed: listed})
	names := make([]string, upkeep.FlakeMaxSubjects+2)
	for i := range names {
		names[i] = fmt.Sprintf("TestCap%02d", i)
	}
	f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, flakeGate(failTail(names...), "tree-cap"))

	u := f.resolve(t)
	if len(u.PinDrift) != 1 || u.PinDrift[0].OmittedOccurrences != 3 {
		t.Fatalf("drift = %+v, want one family with 3 omitted occurrences", u.PinDrift)
	}
	if u.OmittedFlakes != 2 || len(u.Flakes) != upkeep.FlakeMaxSubjects {
		t.Fatalf("flakes = %d omitted = %d, want %d and 2", len(u.Flakes), u.OmittedFlakes, upkeep.FlakeMaxSubjects)
	}
	text, err := prompt.Build("plan", prompt.Trigger{Repo: upkeepTestRepo, Upkeep: u})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{"; 3 further occurrences omitted", "- 2 further flake subjects omitted"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

// --- flake gather ----------------------------------------------------------------

// The acceptance shape plus the tenancy, window and self controls. Every
// excluded run carries its OWN distinct flake, and ListRuns ignores the
// filter, so only the Go-side check can keep each out.
func TestGatherUpkeepFlakes_TenancyWindowAndSelf(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	installUpkeepPinSource(t, &fakeUpkeepPinSource{})
	f.runRow.AccountID = "acct-1"

	good, goodStage := f.seedFlakeRun(t, upkeepTestRepo, "acct-1", time.Hour, flakeGate(failTail("TestX"), "tree-good"))
	// Account-less same-repo run: the ownership predicate compares the account
	// only when both carry one, so the ingest would accept it.
	acctless, _ := f.seedFlakeRun(t, strings.ToUpper(upkeepTestRepo), "", 2*time.Hour, flakeGate(failTail("TestAcctless"), "tree-a"))
	foreignRepo, _ := f.seedFlakeRun(t, "other/repo", "acct-1", 3*time.Hour, flakeGate(failTail("TestForeignRepo"), "tree-r"))
	foreignAcct, _ := f.seedFlakeRun(t, upkeepTestRepo, "acct-2", 4*time.Hour, flakeGate(failTail("TestForeignAcct"), "tree-f"))
	old, _ := f.seedFlakeRun(t, upkeepTestRepo, "acct-1", upkeepFlakeWindow+time.Hour, flakeGate(failTail("TestOld"), "tree-o"))
	// The scanning run's own implement stage carries a flake too.
	f.seedTrace(t, f.runRow.ID, f.implStage.ID, makeRedactedGateEvidenceBundle(t, flakeGate(failTail("TestSelf"), "tree-s")))

	u := f.resolve(t)
	got := subjects(u)
	x, ok := got["TestX"]
	if !ok || !citesRun(x, good.ID) || len(x.Refs) != 1 || x.Refs[0].StageID != goodStage.ID.String() || x.Occurrences != 1 {
		t.Fatalf("TestX = %+v (present %v), want one occurrence citing run %s stage %s", x, ok, good.ID, goodStage.ID)
	}
	if a, ok := got["TestAcctless"]; !ok || !citesRun(a, acctless.ID) {
		t.Errorf("account-less same-repo run not cited: %+v", got)
	}
	for subj, rn := range map[string]*run.Run{
		"TestForeignRepo": foreignRepo, "TestForeignAcct": foreignAcct, "TestOld": old, "TestSelf": f.runRow,
	} {
		if fl, ok := got[subj]; ok {
			t.Errorf("%s cited (run %s): %+v — the run must be excluded", subj, rn.ID, fl)
		}
	}
	if u.FlakeRunsScanned != 2 || u.FlakeStagesScanned != 2 || u.FlakeWindowDays != 14 {
		t.Errorf("runs = %d stages = %d window = %d, want 2, 2, 14", u.FlakeRunsScanned, u.FlakeStagesScanned, u.FlakeWindowDays)
	}
	if len(u.Degrades) != 0 {
		t.Errorf("degrades = %+v, want none", u.Degrades)
	}
}

func TestGatherUpkeepFlakes_MissingTraceAndNoGateEvidenceAreNotDegrades(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	installUpkeepPinSource(t, &fakeUpkeepPinSource{})
	f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, "")            // no trace
	rn, _ := f.seedFlakeRun(t, upkeepTestRepo, "", 2*time.Hour, "") // trace without gate evidence
	f.seedTrace(t, rn.ID, f.rr.stagesByRunID[rn.ID][0].ID, makeRedactedGateEvidenceBundle(t, ""))
	u := f.resolve(t)
	if len(u.Degrades) != 0 || len(u.Flakes) != 0 || u.FlakeRunsScanned != 2 || u.FlakeStagesScanned != 0 {
		t.Errorf("ctx = %+v, want 2 runs scanned, 0 stages, no flakes, no degrades", u)
	}
	if f.ts.getCount() != 1 {
		t.Errorf("trace Gets = %d, want 1 (the stage with no trace is never fetched)", f.ts.getCount())
	}
}

// One test per flake degrade.
func TestGatherUpkeepFlakes_Degrades(t *testing.T) {
	src := plan.UpkeepSourceFlake
	gate := flakeGate(failTail("TestX"), "tree-1")

	t.Run("trace_store_unwired", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.s.cfg.TraceStore = nil
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, gate)
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeTraceStoreUnwired) != 1 || f.rr.runListCalls() != 0 {
			t.Errorf("degrades = %+v ListRuns calls = %d, want trace_store_unwired and no listing", u.Degrades, f.rr.runListCalls())
		}
	})
	t.Run("run_list_failed", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.rr.listRunsErr = errors.New("db down")
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeRunListFailed) != 1 {
			t.Errorf("degrades = %+v, want run_list_failed", u.Degrades)
		}
	})
	t.Run("run_scan_capped", func(t *testing.T) {
		setUpkeepBound(t, &upkeepFlakeRunScanCap, 1)
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		newer, _ := f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, flakeGate(failTail("TestNewer"), "t1"))
		f.seedFlakeRun(t, upkeepTestRepo, "", 2*time.Hour, flakeGate(failTail("TestOlder"), "t2"))
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeRunScanCapped) != 1 || u.FlakeRunsScanned != 1 {
			t.Errorf("degrades = %+v runs = %d, want run_scan_capped and 1 run", u.Degrades, u.FlakeRunsScanned)
		}
		got := subjects(u)
		if fl, ok := got["TestNewer"]; !ok || !citesRun(fl, newer.ID) {
			t.Errorf("newest run not scanned: %+v", got)
		}
		if _, ok := got["TestOlder"]; ok {
			t.Error("a run past the scan cap was read")
		}
	})
	t.Run("stage_list_failed", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		other, _ := f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, gate)
		failing := &stageListFailOnce{upkeepEvidenceRunRepo: f.rr, failFor: other.ID}
		f.s.cfg.RunRepo = failing
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeStageListFailed) != 1 || len(u.Flakes) != 0 {
			t.Errorf("degrades = %+v flakes = %+v, want stage_list_failed and none", u.Degrades, u.Flakes)
		}
	})
	t.Run("trace_list_failed", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, gate)
		f.au.listByCategoryErrCategory = "trace_uploaded"
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeTraceListFailed) != 1 || f.ts.getCount() != 0 {
			t.Errorf("degrades = %+v gets = %d, want trace_list_failed and no Get", u.Degrades, f.ts.getCount())
		}
	})
	t.Run("trace_fetch_failed", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, gate)
		f.ts.getErr = errors.New("s3 503")
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeTraceFetchFailed) != 1 || len(u.Flakes) != 0 {
			t.Errorf("degrades = %+v, want trace_fetch_failed", u.Degrades)
		}
	})
	t.Run("bundle_too_large", func(t *testing.T) {
		setUpkeepBound(t, &upkeepBundleMaxBytes, int64(16))
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, gate)
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeBundleTooLarge) != 1 || len(u.Flakes) != 0 {
			t.Errorf("degrades = %+v, want bundle_too_large", u.Degrades)
		}
	})
	t.Run("gate_evidence_parse_failed", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, `[1,2]`)
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeGateEvidenceParseFailed) != 1 {
			t.Errorf("degrades = %+v, want gate_evidence_parse_failed", u.Degrades)
		}
	})
	t.Run("bundle_cap_reached", func(t *testing.T) {
		setUpkeepBound(t, &upkeepFlakeBundleCap, 1)
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, flakeGate(failTail("TestFirst"), "t1"))
		f.seedFlakeRun(t, upkeepTestRepo, "", 2*time.Hour, flakeGate(failTail("TestSecond"), "t2"))
		u := f.resolve(t)
		if degradeCount(u, src, upkeepDegradeBundleCapReached) != 1 {
			t.Errorf("degrades = %+v, want bundle_cap_reached", u.Degrades)
		}
		if f.ts.getCount() != 1 {
			t.Errorf("trace Gets = %d, want exactly 1 under a cap of 1", f.ts.getCount())
		}
		if _, ok := subjects(u)["TestSecond"]; ok {
			t.Error("a bundle past the cap was read")
		}
	})
	t.Run("budget_exceeded", func(t *testing.T) {
		setUpkeepBound(t, &upkeepEvidenceBudget, timescale.D(100*time.Millisecond))
		wedge := timescale.D(5 * time.Second)
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, gate)
		f.ts.block = wedge
		start := time.Now()
		u := f.resolve(t)
		if el := time.Since(start); el >= wedge {
			t.Fatalf("gather took %v, not cut by the budget (wedge %v)", el, wedge)
		}
		if degradeCount(u, src, upkeepDegradeBudgetExceeded) != 1 {
			t.Errorf("degrades = %+v, want flake budget_exceeded", u.Degrades)
		}
		if degradeCount(u, plan.UpkeepSourceToolchainDrift, upkeepDegradeBudgetExceeded) != 0 {
			t.Errorf("degrades = %+v: the pin gather finished before the budget and must not report it", u.Degrades)
		}
	})
	t.Run("pin budget_exceeded", func(t *testing.T) {
		setUpkeepBound(t, &upkeepEvidenceBudget, timescale.D(50*time.Millisecond))
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{block: timescale.D(5 * time.Second)})
		u := f.resolve(t)
		if degradeCount(u, plan.UpkeepSourceToolchainDrift, upkeepDegradeBudgetExceeded) == 0 {
			t.Errorf("degrades = %+v, want toolchain_drift budget_exceeded", u.Degrades)
		}
	})
	t.Run("degrades are WARN-logged with the run id", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.rr.listRunsErr = errors.New("db down")
		f.resolve(t)
		l := f.logs.String()
		if !strings.Contains(l, `"reason":"run_list_failed"`) || !strings.Contains(l, f.runRow.ID.String()) || !strings.Contains(l, `"level":"WARN"`) {
			t.Errorf("log = %s, want a WARN naming run_list_failed and the run id", l)
		}
	})
}

// stageListFailOnce fails ListStagesForRun for one run only, so the scanning
// run's own binding still resolves.
type stageListFailOnce struct {
	*upkeepEvidenceRunRepo
	failFor uuid.UUID
}

func (r *stageListFailOnce) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if runID == r.failFor {
		return nil, errors.New("stage list blew up")
	}
	return r.upkeepEvidenceRunRepo.ListStagesForRun(ctx, runID)
}

// --- scope of the gather ----------------------------------------------------------

func TestResolveUpkeepScanContext_OnlyDeclaringPlanStages(t *testing.T) {
	t.Run("ordinary workflow: nil, zero pin and trace reads", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, []byte(appliesToSpec("")), "guarded")
		calls := installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, flakeGate(failTail("TestX"), "t"))
		u, err := f.s.resolveUpkeepScanContext(context.Background(), f.runRow, f.planStage)
		if err != nil || u != nil {
			t.Fatalf("= (%+v, %v), want (nil, nil)", u, err)
		}
		if *calls != 0 || f.ts.getCount() != 0 || f.rr.runListCalls() != 0 {
			t.Errorf("pin source builds = %d, trace Gets = %d, ListRuns = %d; want all 0", *calls, f.ts.getCount(), f.rr.runListCalls())
		}
	})
	t.Run("non-plan stage: nil with no read at all", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		calls := installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		u, err := f.s.resolveUpkeepScanContext(context.Background(), f.runRow, f.implStage)
		if err != nil || u != nil || *calls != 0 || f.rr.listCalls() != 0 {
			t.Errorf("= (%+v, %v) builds = %d stage lists = %d, want nil and no reads", u, err, *calls, f.rr.listCalls())
		}
	})
	t.Run("binding transport error is returned", func(t *testing.T) {
		f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
		installUpkeepPinSource(t, &fakeUpkeepPinSource{})
		f.rr.listStagesErr = errors.New("db down")
		if _, err := f.s.resolveUpkeepScanContext(context.Background(), f.runRow, f.planStage); err == nil || !strings.Contains(err.Error(), "db down") {
			t.Errorf("err = %v, want the binding's list error", err)
		}
	})
}

// --- handler-level, cross-boundary ---------------------------------------------

// serve drives the signed /prompt and the unsigned /prompt-render.
func (f *upkeepEvidenceFixture) serve(t *testing.T, stageID uuid.UUID) (signed, preview string) {
	t.Helper()
	priv, _ := f.sf.issue(t, f.runRow.ID)
	sw := promptRequest(t, f.s, f.runRow.ID, stageID, priv, "")
	if sw.Code != http.StatusOK {
		t.Fatalf("/prompt status = %d:\n%s", sw.Code, sw.Body.String())
	}
	pw := promptRenderRequest(t, f.s, stageID)
	if pw.Code != http.StatusOK {
		t.Fatalf("/prompt-render status = %d:\n%s", pw.Code, pw.Body.String())
	}
	var a, b promptResponse
	if err := json.Unmarshal(sw.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode /prompt: %v", err)
	}
	if err := json.Unmarshal(pw.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode /prompt-render: %v", err)
	}
	return a.Prompt, b.Prompt
}

// driftingWorkflows is two workflows pinning different golangci-lint tags.
func driftingWorkflows() *fakeUpkeepPinSource {
	return &fakeUpkeepPinSource{
		files: map[string]string{
			".github/workflows/a.yml": "      - run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.8.0/install.sh | sh -s -- -b bin v2.8.0\n",
			".github/workflows/b.yml": "      - run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.9.0/install.sh | sh -s -- -b bin v2.9.0\n",
		},
		listed: []string{".github/workflows/a.yml", ".github/workflows/b.yml"},
	}
}

// (a) The shipped upkeep-scan spec, served through BOTH prompt endpoints:
// spec binding → pin source + run/audit/trace fakes → detectors → prompt.Build
// → HTTP response.
func TestUpkeepScanPrompt_ServedWithEvidence(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	installUpkeepPinSource(t, driftingWorkflows())
	flaky, flakyStage := f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, flakeGate(failTail("TestX"), "tree-1"))

	signed, preview := f.serve(t, f.planStage.ID)
	if signed != preview {
		t.Errorf("/prompt and /prompt-render disagree:\n--- prompt ---\n%s\n--- render ---\n%s", signed, preview)
	}
	drift := factLine(t, signed, "- family golangci-lint:")
	for _, want := range []string{".github/workflows/a.yml:1 = v2.8.0", ".github/workflows/b.yml:1 = v2.9.0"} {
		if !strings.Contains(drift, want) {
			t.Errorf("drift line %q lacks %q", drift, want)
		}
	}
	wantFlake := fmt.Sprintf("- subject TestX: occurrences 1; runs: run_id %s stage_id %s", flaky.ID, flakyStage.ID)
	for _, want := range []string{upkeepMarker, plan.UpkeepReportVersion, wantFlake, "Base commit (pin files read at): " + upkeepTestCommit} {
		if !strings.Contains(signed, want) {
			t.Errorf("served prompt lacks %q", want)
		}
	}
}

// (b) An ordinary plan stage is served the plain plan prompt and the gather
// makes zero reads.
func TestUpkeepScanPrompt_OrdinaryPlanStageUntouched(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, []byte(appliesToSpec("")), "guarded")
	calls := installUpkeepPinSource(t, driftingWorkflows())
	f.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, flakeGate(failTail("TestX"), "tree-1"))
	signed, preview := f.serve(t, f.planStage.ID)
	for name, text := range map[string]string{"prompt": signed, "prompt-render": preview} {
		if strings.Contains(text, upkeepMarker) || strings.Contains(text, "Evidence gathered by the server") {
			t.Errorf("%s: ordinary plan stage served upkeep content", name)
		}
	}
	if *calls != 0 || f.ts.getCount() != 0 || f.rr.runListCalls() != 0 {
		t.Errorf("pin source builds = %d, trace Gets = %d, ListRuns = %d; want all 0", *calls, f.ts.getCount(), f.rr.runListCalls())
	}
}

// A binding transport error is a 500 on both endpoints.
func TestUpkeepScanPrompt_BindingErrorIs500(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	installUpkeepPinSource(t, &fakeUpkeepPinSource{})
	f.rr.listStagesErr = errors.New("db down")
	priv, _ := f.sf.issue(t, f.runRow.ID)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"prompt":        promptRequest(t, f.s, f.runRow.ID, f.planStage.ID, priv, ""),
		"prompt-render": promptRenderRequest(t, f.s, f.planStage.ID),
	} {
		// A 500's details are redacted to an error_ref; the message names the
		// failed resolution.
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "resolve the stage's upkeep_report declaration failed") {
			t.Errorf("%s = %d %s, want 500 naming the upkeep declaration", name, w.Code, w.Body.String())
		}
	}
}

// (c) A plan_schema_retry row carrying the guard's malformed-body message
// renders in the SERVED upkeep prompt under the upkeep heading: audit →
// loadPriorSchemaValidationError → Trigger → buildUpkeepScan.
func TestUpkeepScanPrompt_SchemaRetryFeedbackUnderUpkeepHeading(t *testing.T) {
	f := newUpkeepEvidenceFixture(t, upkeepScanSpec(t), "upkeep_scan")
	installUpkeepPinSource(t, &fakeUpkeepPinSource{})
	guardMsg := "stage x declares produces: upkeep_report; the body is not a parseable upkeep_report (unexpected end of JSON input); " +
		"it may ship only an upkeep_report (" + plan.UpkeepReportVersion + ", kind \"upkeep_report\") or a clarification_request"
	payload, _ := json.Marshal(map[string]any{"validation_error": guardMsg})
	rid, sid := f.runRow.ID, f.planStage.ID
	f.au.seeded = append(f.au.seeded, &audit.Entry{ID: uuid.New(), Sequence: 1, RunID: &rid, StageID: &sid,
		Timestamp: time.Now(), Category: "plan_schema_retry", Payload: payload})

	signed, _ := f.serve(t, f.planStage.ID)
	head := "### Prior upkeep-scan schema validation failure"
	i := strings.Index(signed, head)
	if i < 0 {
		t.Fatalf("served prompt lacks %q", head)
	}
	section := signed[i:]
	if j := strings.Index(section[len(head):], "### "); j >= 0 {
		section = section[:len(head)+j]
	}
	for _, want := range []string{plan.UpkeepReportVersion, "unexpected end of JSON input"} {
		if !strings.Contains(section, want) {
			t.Errorf("schema-retry section lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(signed, "standard_v1 validation") {
		t.Error("served upkeep prompt carries the plan schema's retry heading")
	}
}

// --- facts → ingest round trip (approval condition 2) ---------------------------

var (
	upkeepFactRunRE   = regexp.MustCompile(`run_id (\S+) stage_id ([^\s,;]+)`)
	upkeepFactOccurRE = regexp.MustCompile(`^(\S+):(\d+) = (\S+)$`)
)

// factLine returns the single rendered facts line starting with prefix.
func factLine(t *testing.T, text, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no facts line starts with %q in:\n%s", prefix, text)
	return ""
}

// TestUpkeepFacts_RoundTripThroughIngest builds an upkeep_report whose every
// finding id is `<source>:<subject>` for a gathered family or flake subject —
// '@redocly/cli', 'verify-gate:unnamed' and 'verify-gate:infra' included — with
// evidence refs parsed VERBATIM from the RENDERED facts block, and posts it
// through the #3921 ingest, which must accept it.
func TestUpkeepFacts_RoundTripThroughIngest(t *testing.T) {
	var ers *upkeepEvidenceRunRepo
	f := newUpkeepIngestFixtureWith(t, upkeepScanSpec(t), "upkeep_scan", nil, func(rr *upkeepRunRepo) run.Repository {
		ers = &upkeepEvidenceRunRepo{upkeepRunRepo: rr}
		return ers
	})
	ts := &upkeepTraceStore{bodies: map[string][]byte{}}
	f.s.cfg.TraceStore = ts
	ev := &upkeepEvidenceFixture{s: f.s, rr: ers, au: f.au, ts: ts, runRow: f.runRow, planStage: f.planStage, implStage: f.implStage}
	f.runRow.CreatedAt = time.Now()

	installUpkeepPinSource(t, &fakeUpkeepPinSource{
		files: map[string]string{
			"go.work":       "go 1.25.0\n",
			".golangci.yml": "run:\n  go: '1.22'\n",
			".github/workflows/a.yml": "      - run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.8.0/install.sh | sh\n" +
				"      - run: npx -y @redocly/cli@2.31.5 lint\n",
			".github/workflows/b.yml": "      - run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.9.0/install.sh | sh\n",
			"AGENTS.md":               "npx -y @redocly/cli@2.30.0 lint docs/api/v0.openapi.yaml\n",
		},
		listed: []string{".github/workflows/a.yml", ".github/workflows/b.yml"},
	})
	ev.seedFlakeRun(t, upkeepTestRepo, "", time.Hour, flakeGate(failTail("TestX"), "t1"))
	ev.seedFlakeRun(t, upkeepTestRepo, "", 2*time.Hour, flakeGate("panic: boom\n", "t2"))
	infra, _ := ev.seedFlakeRun(t, upkeepTestRepo, "", 3*time.Hour, "")
	ev.seedTrace(t, infra.ID, ers.stagesByRunID[infra.ID][0].ID, makeRedactedGateEvidenceBundle(t, `{"flake_retries":2}`))

	u := ev.resolve(t)
	text, err := prompt.Build("plan", prompt.Trigger{Repo: upkeepTestRepo, Upkeep: u})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	type finding = map[string]any
	var findings []finding
	issue := func(subject string) map[string]any {
		return map[string]any{"title": "Upkeep: " + subject, "body": "From the upkeep scan.", "type": "chore", "labels": []string{"area:build"}}
	}
	for _, fam := range []string{upkeep.PinFamilyGo, upkeep.PinFamilyGolangciLint, upkeep.PinFamilyRedocly} {
		line := factLine(t, text, "- family "+fam+":")
		var evidence []map[string]any
		for _, occ := range strings.Split(strings.TrimPrefix(line, "- family "+fam+":"), ";") {
			m := upkeepFactOccurRE.FindStringSubmatch(strings.TrimSpace(occ))
			if m == nil {
				t.Fatalf("unparseable occurrence %q in %q", occ, line)
			}
			n, _ := strconv.Atoi(m[2])
			evidence = append(evidence, map[string]any{"kind": "file", "path": m[1], "line": n, "value": m[3]})
		}
		findings = append(findings, finding{
			"id": plan.UpkeepSourceToolchainDrift + ":" + fam, "source": plan.UpkeepSourceToolchainDrift,
			"subject": fam, "evidence": evidence, "proposed_issue": issue(fam),
		})
	}
	for _, subj := range []string{"TestX", upkeep.FlakeSubjectUnnamed, upkeep.FlakeSubjectInfra} {
		line := factLine(t, text, "- subject "+subj+":")
		var evidence []map[string]any
		for _, m := range upkeepFactRunRE.FindAllStringSubmatch(line, -1) {
			evidence = append(evidence, map[string]any{"kind": "run", "run_id": m[1], "stage_id": m[2]})
		}
		if len(evidence) == 0 {
			t.Fatalf("no run refs in %q", line)
		}
		findings = append(findings, finding{
			"id": plan.UpkeepSourceFlake + ":" + subj, "source": plan.UpkeepSourceFlake,
			"subject": subj, "evidence": evidence, "proposed_issue": issue(subj),
		})
	}
	report, _ := json.Marshal(map[string]any{
		"kind":           "upkeep_report",
		"report_version": plan.UpkeepReportVersion,
		// The unanchored-scan ticket_reference fallback the prompt instructs.
		"ticket_reference": map[string]any{"type": "github_issue", "url": "https://github.com/" + upkeepTestRepo + "/issues", "id": upkeepTestRepo},
		"generated_by":     map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-10-03T09:00:00Z"},
		"summary":          "Unanchored scheduled scan: no triggering issue.",
		"sources_scanned":  []string{plan.UpkeepSourceFlake, plan.UpkeepSourceToolchainDrift},
		"findings":         findings,
	})

	code, resp := f.post(t, f.planStage.ID, report)
	if code != http.StatusCreated {
		t.Fatalf("ingest status = %d, want 201: %v\nreport: %s", code, resp, report)
	}
	if len(findings) != 6 {
		t.Errorf("findings = %d, want 6", len(findings))
	}
}
