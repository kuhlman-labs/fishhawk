package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge/stub"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- fixtures -----------------------------------------------------------

var (
	sweepRunID   = uuid.MustParse("11111111-aaaa-4bbb-8ccc-000000000001")
	sweepStageA  = uuid.MustParse("22222222-aaaa-4bbb-8ccc-000000000002")
	sweepStageB  = uuid.MustParse("33333333-aaaa-4bbb-8ccc-000000000003")
	sweepBranchA = "fishhawk/run-11111111/stage-22222222"
	sweepBranchB = "fishhawk/run-11111111/stage-33333333"
	sweepConsol  = "fishhawk/run-11111111-consolidated"
	sweepSlice0  = "fishhawk/run-11111111/slice-0"
	sweepSlice1  = "fishhawk/run-11111111/slice-1"
)

type headBase struct{ head, base string }

// fakeRunBranchForge is the in-memory sweep forge. The recorded delete set is
// read back AFTER a sweep returns, so the preservation gates are asserted on
// committed state, not on a returned error. Guarded by mu: the cancel-path
// sweep runs on a goroutine.
type fakeRunBranchForge struct {
	mu          sync.Mutex
	openHead    map[string]bool    // head -> an open PR exists (any base)
	openPair    map[headBase]bool  // (head, base) -> an open PR exists
	listErr     map[string]error   // head -> error on the head-only (base "") list
	pairErr     map[headBase]error // (head, base) -> error on the base-keyed list
	deleteErr   map[string]error
	absent      map[string]bool
	deleted     []string
	listCalls   int
	block       chan struct{} // when non-nil, every forge call waits on it
	enteredOnce sync.Once
	entered     chan struct{}
}

func newFakeRunBranchForge() *fakeRunBranchForge {
	return &fakeRunBranchForge{
		openHead:  map[string]bool{},
		openPair:  map[headBase]bool{},
		listErr:   map[string]error{},
		pairErr:   map[headBase]error{},
		deleteErr: map[string]error{},
		absent:    map[string]bool{},
		entered:   make(chan struct{}),
	}
}

func (f *fakeRunBranchForge) wait(ctx context.Context) error {
	f.enteredOnce.Do(func() { close(f.entered) })
	if f.block == nil {
		return nil
	}
	select {
	case <-f.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeRunBranchForge) GetBranchSHA(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef, branch string) (string, bool, error) {
	if err := f.wait(ctx); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.absent[branch] {
		return "", false, nil
	}
	return "sha", true, nil
}

func (f *fakeRunBranchForge) ListOpenPullRequestsByHead(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef, head, base string) ([]forge.PullRequest, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if err := f.listErr[head]; err != nil && base == "" {
		return nil, err
	}
	if err := f.pairErr[headBase{head, base}]; err != nil {
		return nil, err
	}
	open := false
	if base == "" {
		open = f.openHead[head]
		for k := range f.openPair {
			if k.head == head {
				open = true
			}
		}
	} else {
		open = f.openPair[headBase{head, base}]
	}
	if open {
		return []forge.PullRequest{{Number: 7, State: "open"}}, nil
	}
	return nil, nil
}

func (f *fakeRunBranchForge) DeleteRef(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef, branch string) error {
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteErr[branch]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, branch)
	f.absent[branch] = true
	return nil
}

func (f *fakeRunBranchForge) deletedSet() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.deleted...)
	sort.Strings(out)
	return out
}

// deleteOnlyForge implements forge.RefDeleter but not the open-PR read.
type deleteOnlyForge struct{ calls int }

func (d *deleteOnlyForge) DeleteRef(context.Context, forge.CredentialScope, forge.RepoRef, string) error {
	d.calls++
	return nil
}

func sweepRun(state run.State) *run.Run {
	inst := int64(42)
	return &run.Run{ID: sweepRunID, Repo: "x/y", State: state, InstallationID: &inst}
}

func sweepRunRepo(r *run.Run, children []*run.Run) *prEventsRunRepo {
	return &prEventsRunRepo{
		listResult: []*run.Run{r},
		stages: map[uuid.UUID][]*run.Stage{r.ID: {
			{ID: sweepStageA, RunID: r.ID, Type: run.StageTypePlan},
			{ID: sweepStageB, RunID: r.ID, Type: run.StageTypeImplement},
		}},
		decomposedResult: children,
	}
}

func sweepServer(rr *prEventsRunRepo, ar *prEventsAuditRepo, f forge.RefDeleter) *Server {
	cfg := Config{Addr: "127.0.0.1:0", RunRepo: rr, RunBranchDeleter: f}
	if ar != nil {
		cfg.AuditRepo = ar
	}
	return New(cfg)
}

func sweptRows(ar *prEventsAuditRepo) []runBranchesSweptPayload {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	var out []runBranchesSweptPayload
	for _, p := range ar.appended {
		if p.Category != RunBranchesSweptCategory {
			continue
		}
		var body runBranchesSweptPayload
		_ = json.Unmarshal(p.Payload, &body)
		out = append(out, body)
	}
	return out
}

func oneSweptRow(t *testing.T, ar *prEventsAuditRepo) runBranchesSweptPayload {
	t.Helper()
	rows := sweptRows(ar)
	if len(rows) != 1 {
		t.Fatalf("run_branches_swept rows = %d, want 1", len(rows))
	}
	return rows[0]
}

func childRun(idx *int) *run.Run {
	parent := sweepRunID
	return &run.Run{ID: uuid.New(), DecomposedFrom: &parent, SliceIndex: idx}
}

func intp(i int) *int { return &i }

// --- derivation ---------------------------------------------------------

// TestRunSweepCandidates_DerivesBranchNames is the done-means pin: the
// candidate set is asserted BYTE-EXACTLY for known UUIDs, and cross-checked
// against the exported orchestrator helpers the derivation must call.
func TestRunSweepCandidates_DerivesBranchNames(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	stages := []*run.Stage{{ID: sweepStageA}, {ID: sweepStageB}, nil}

	if got, want := runSweepCandidates(r, stages, nil, false), []string{sweepBranchA, sweepBranchB}; !reflect.DeepEqual(got, want) {
		t.Errorf("ordinary run candidates = %v, want %v", got, want)
	}

	children := []*run.Run{childRun(intp(1)), childRun(nil), nil}
	got := runSweepCandidates(r, stages, children, true)
	want := []string{sweepConsol, sweepSlice0, sweepSlice1, sweepBranchA, sweepBranchB}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decomposed parent candidates = %v, want %v", got, want)
	}
	if orchestrator.ConsolidatedBranch(sweepRunID) != sweepConsol ||
		orchestrator.SliceBranch(sweepRunID, 1) != sweepSlice1 {
		t.Errorf("orchestrator helpers diverged from the pinned literals")
	}

	if got := runSweepCandidates(childRun(intp(0)), stages, nil, true); len(got) != 0 {
		t.Errorf("decomposed child candidates = %v, want none", got)
	}
	if got := runSweepCandidates(nil, stages, nil, false); got != nil {
		t.Errorf("nil run candidates = %v, want nil", got)
	}
}

// TestSweepRunBranches_RefusesNonFishhawkRef pins the namespace guard every
// candidate passes through: a non-Fishhawk name never survives it.
func TestSweepRunBranches_RefusesNonFishhawkRef(t *testing.T) {
	set := map[string]struct{}{
		"main": {}, "release/v1": {}, "fishhawk/other": {}, "fishhawk/run-abc/stage-x": {},
	}
	got := namespaceGuardBranches(set)
	if want := []string{"fishhawk/run-abc/stage-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("guarded = %v, want %v", got, want)
	}
}

// --- per-failure-mode ---------------------------------------------------

// TestSweepRunBranches_OpenPullRequestBranchSurvives: the open PR is seeded by
// construction in the fake's PR list against a NON-default base, and the
// assertion reads the recorded delete set after the call returns.
func TestSweepRunBranches_OpenPullRequestBranchSurvives(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	f := newFakeRunBranchForge()
	f.openPair[headBase{sweepBranchA, "fishhawk/some-integration-branch"}] = true
	ar := &prEventsAuditRepo{}
	s := sweepServer(sweepRunRepo(r, nil), ar, f)

	s.sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)

	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepBranchB}) {
		t.Fatalf("deleted = %v, want only %s (the open-PR head must survive)", got, sweepBranchB)
	}
	row := oneSweptRow(t, ar)
	if len(row.SkippedOpenPR) != 1 || row.SkippedOpenPR[0] != (sweepBranchSkip{sweepBranchA, sweepSkipOpenPRHead}) {
		t.Errorf("skipped_open_pr = %+v", row.SkippedOpenPR)
	}
	if row.Trigger != sweepTriggerPRMerged {
		t.Errorf("trigger = %q", row.Trigger)
	}
}

func TestSweepRunBranches_PRListErrorPreservesBranch(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	f := newFakeRunBranchForge()
	f.listErr[sweepBranchA] = errors.New("boom 502")
	ar := &prEventsAuditRepo{}
	s := sweepServer(sweepRunRepo(r, nil), ar, f)

	s.sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)

	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepBranchB}) {
		t.Fatalf("deleted = %v, want only %s", got, sweepBranchB)
	}
	row := oneSweptRow(t, ar)
	if len(row.SkippedPRReadError) != 1 || row.SkippedPRReadError[0].Branch != sweepBranchA ||
		!strings.Contains(row.SkippedPRReadError[0].Error, "boom 502") {
		t.Errorf("skipped_pr_read_error = %+v", row.SkippedPRReadError)
	}
}

func TestSweepRunBranches_PerBranchDeleteErrorContinues(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	f := newFakeRunBranchForge()
	f.deleteErr[sweepBranchA] = forge.ErrForbidden
	ar := &prEventsAuditRepo{}
	s := sweepServer(sweepRunRepo(r, nil), ar, f)

	s.sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)

	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepBranchB}) {
		t.Fatalf("deleted = %v, want %s still deleted past the 403", got, sweepBranchB)
	}
	row := oneSweptRow(t, ar)
	if len(row.Errors) != 1 || row.Errors[0].Branch != sweepBranchA {
		t.Errorf("errors = %+v", row.Errors)
	}
	if r.State != run.StateSucceeded {
		t.Errorf("run state = %q, the sweep must never touch it", r.State)
	}
}

func TestSweepRunBranches_NilDeleterIsNoOpWithoutAuditRow(t *testing.T) {
	cases := map[string]Config{
		"github family, nil cfg.GitHub": {},
		"other family, resolver error": {ForgeResolver: func(string) (forge.Forge, error) {
			return nil, errors.New("no forge")
		}},
		"seam lacks the open-PR read": {RunBranchDeleter: &deleteOnlyForge{}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			r := sweepRun(run.StateSucceeded)
			if name == "other family, resolver error" {
				ref := "gitlab:5"
				r.InstallationRef = &ref
			}
			ar := &prEventsAuditRepo{}
			cfg.Addr, cfg.RunRepo, cfg.AuditRepo = "127.0.0.1:0", sweepRunRepo(r, nil), ar
			s := New(cfg)
			s.sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
			if n := len(sweptRows(ar)); n != 0 {
				t.Errorf("run_branches_swept rows = %d, want 0", n)
			}
			if d, ok := cfg.RunBranchDeleter.(*deleteOnlyForge); ok && d.calls != 0 {
				t.Errorf("delete-only seam called %d times, want 0", d.calls)
			}
		})
	}
}

func TestSweepRunBranches_ZeroCredentialScopeIsNoOp(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	r.InstallationID = nil
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{}
	sweepServer(sweepRunRepo(r, nil), ar, f).sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	if len(f.deletedSet()) != 0 || f.listCalls != 0 || len(sweptRows(ar)) != 0 {
		t.Errorf("zero scope: deleted=%v listCalls=%d rows=%d, want all zero", f.deletedSet(), f.listCalls, len(sweptRows(ar)))
	}
}

func TestSweepRunBranches_UnparseableRepoIsNoOp(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	r.Repo = "not-owner-name"
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{}
	sweepServer(sweepRunRepo(r, nil), ar, f).sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	if len(f.deletedSet()) != 0 || len(sweptRows(ar)) != 0 {
		t.Errorf("unparseable repo: deleted=%v rows=%d, want none", f.deletedSet(), len(sweptRows(ar)))
	}
}

func TestSweepRunBranches_NilAuditRepoStillDeletes(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	f := newFakeRunBranchForge()
	sweepServer(sweepRunRepo(r, nil), nil, f).sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepBranchA, sweepBranchB}) {
		t.Errorf("deleted = %v, want both stage branches", got)
	}
}

func TestSweepRunBranches_AuditAppendErrorStillDeletes(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{err: errors.New("db down")}
	sweepServer(sweepRunRepo(r, nil), ar, f).sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepBranchA, sweepBranchB}) {
		t.Errorf("deleted = %v, want both stage branches", got)
	}
}

func TestSweepRunBranches_AlreadyAbsentBranchClassifiedAbsent(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	f := newFakeRunBranchForge()
	f.absent[sweepBranchA] = true
	ar := &prEventsAuditRepo{}
	sweepServer(sweepRunRepo(r, nil), ar, f).sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	row := oneSweptRow(t, ar)
	if !reflect.DeepEqual(row.AlreadyAbsent, []string{sweepBranchA}) || len(row.Errors) != 0 ||
		!reflect.DeepEqual(row.Deleted, []string{sweepBranchB}) {
		t.Errorf("row = %+v, want A already_absent, B deleted, no errors", row)
	}
}

func TestSweepRunBranches_ChildrenEnumerationErrorDegradesToParentOnly(t *testing.T) {
	r := sweepRun(run.StateCancelled)
	rr := sweepRunRepo(r, nil)
	rr.listErr = errors.New("list children failed")
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{}
	sweepServer(rr, ar, f).sweepRunBranches(context.Background(), r, sweepTriggerCancelled)
	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepConsol, sweepBranchA, sweepBranchB}) {
		t.Errorf("deleted = %v, want consolidated + the parent's stage branches", got)
	}
	if row := oneSweptRow(t, ar); !strings.Contains(row.ChildrenUnenumerated, "list children failed") {
		t.Errorf("children_unenumerated = %q", row.ChildrenUnenumerated)
	}
}

func TestSweepRunBranches_StagesEnumerationErrorRecorded(t *testing.T) {
	r := sweepRun(run.StateCancelled)
	rr := sweepRunRepo(r, []*run.Run{childRun(intp(0))})
	rr.stagesErr = errors.New("list stages failed")
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{}
	sweepServer(rr, ar, f).sweepRunBranches(context.Background(), r, sweepTriggerCancelled)
	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepConsol, sweepSlice0}) {
		t.Errorf("deleted = %v, want consolidated + slice-0", got)
	}
	if row := oneSweptRow(t, ar); !strings.Contains(row.StagesUnenumerated, "list stages failed") {
		t.Errorf("stages_unenumerated = %q", row.StagesUnenumerated)
	}
}

func TestSweepRunBranches_DecomposedChildContributesNoCandidates(t *testing.T) {
	c := childRun(intp(0))
	inst := int64(42)
	c.Repo, c.InstallationID, c.State = "x/y", &inst, run.StateSucceeded
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{}
	sweepServer(sweepRunRepo(c, nil), ar, f).sweepRunBranches(context.Background(), c, sweepTriggerPRMerged)
	if len(f.deletedSet()) != 0 || f.listCalls != 0 || len(sweptRows(ar)) != 0 {
		t.Errorf("child: deleted=%v listCalls=%d rows=%d, want none", f.deletedSet(), f.listCalls, len(sweptRows(ar)))
	}
}

// TestSweepRunBranches_SkipsTerminalFailedRun: revive resumes a failed run on
// exactly these branches, so the sweep must not touch them (#3678).
func TestSweepRunBranches_SkipsTerminalFailedRun(t *testing.T) {
	r := sweepRun(run.StateFailed)
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{}
	sweepServer(sweepRunRepo(r, nil), ar, f).sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	if got := f.deletedSet(); len(got) != 0 {
		t.Errorf("deleted = %v on a terminal-failed run, want none", got)
	}
	if n := len(sweptRows(ar)); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

func TestSweepRunBranches_EmptyCandidateSetWritesNoRow(t *testing.T) {
	r := sweepRun(run.StateSucceeded)
	rr := sweepRunRepo(r, nil)
	rr.stages = nil
	f := newFakeRunBranchForge()
	ar := &prEventsAuditRepo{}
	sweepServer(rr, ar, f).sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	if n := len(sweptRows(ar)); n != 0 {
		t.Errorf("rows = %d, want 0 for an empty candidate set", n)
	}
}

// --- approval condition 1: the BASE-branch gate -------------------------

func cancelRun(t *testing.T, s *Server, runID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", runID), nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	return w
}

// TestSweepRunBranches_CancelledParentKeepsConsolidatedBaseOfOpenSlicePR: a
// cancelled decomposed parent whose consolidated branch is the BASE of an open
// slice PR keeps it, and the row names it skipped with the open_pr_base reason.
func TestSweepRunBranches_CancelledParentKeepsConsolidatedBaseOfOpenSlicePR(t *testing.T) {
	r := sweepRun(run.StateRunning)
	rr := sweepRunRepo(r, []*run.Run{childRun(intp(0)), childRun(intp(1))})
	f := newFakeRunBranchForge()
	f.openPair[headBase{sweepSlice0, sweepConsol}] = true
	ar := &prEventsAuditRepo{}
	s := sweepServer(rr, ar, f)

	if w := cancelRun(t, s, r.ID); w.Code != http.StatusOK {
		t.Fatalf("cancel status = %d: %s", w.Code, w.Body.String())
	}
	s.waitBranchSweeps()

	got := f.deletedSet()
	for _, b := range got {
		if b == sweepConsol || b == sweepSlice0 {
			t.Fatalf("deleted = %v: %s must survive", got, b)
		}
	}
	if want := []string{sweepSlice1, sweepBranchA, sweepBranchB}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}
	row := oneSweptRow(t, ar)
	skips := map[string]string{}
	for _, sk := range row.SkippedOpenPR {
		skips[sk.Branch] = sk.Reason
	}
	if skips[sweepConsol] != sweepSkipOpenPRBase || skips[sweepSlice0] != sweepSkipOpenPRHead {
		t.Errorf("skipped_open_pr = %+v, want consolidated=open_pr_base slice-0=open_pr_head", row.SkippedOpenPR)
	}
	if row.Trigger != sweepTriggerCancelled {
		t.Errorf("trigger = %q", row.Trigger)
	}
}

// TestSweepRunBranches_BaseGateReadErrorPreservesBranch: when the head whose
// PR might target the consolidated branch cannot be read, the consolidated
// branch is preserved (skipped_pr_read_error), never deleted.
func TestSweepRunBranches_BaseGateReadErrorPreservesBranch(t *testing.T) {
	r := sweepRun(run.StateCancelled)
	rr := sweepRunRepo(r, []*run.Run{childRun(intp(0))})
	f := newFakeRunBranchForge()
	f.listErr[sweepSlice0] = errors.New("pr list 500")
	f.pairErr[headBase{sweepSlice0, sweepConsol}] = errors.New("pr list 500")
	ar := &prEventsAuditRepo{}
	sweepServer(rr, ar, f).sweepRunBranches(context.Background(), r, sweepTriggerCancelled)
	for _, b := range f.deletedSet() {
		if b == sweepConsol {
			t.Fatalf("consolidated branch deleted while the base gate was undecidable")
		}
	}
	row := oneSweptRow(t, ar)
	var got []string
	for _, e := range row.SkippedPRReadError {
		got = append(got, e.Branch)
	}
	sort.Strings(got)
	if want := []string{sweepConsol, sweepSlice0}; !reflect.DeepEqual(got, want) {
		t.Errorf("skipped_pr_read_error branches = %v, want %v", got, want)
	}
}

// --- approval condition 3: the cancel response never waits on the sweep --

func TestCancelRun_ResponseDoesNotWaitOnSlowForge(t *testing.T) {
	r := sweepRun(run.StateRunning)
	f := newFakeRunBranchForge()
	f.block = make(chan struct{})
	ar := &prEventsAuditRepo{}
	s := sweepServer(sweepRunRepo(r, nil), ar, f)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- cancelRun(t, s, r.ID) }()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("cancel status = %d", w.Code)
		}
	case <-time.After(5 * time.Second):
		close(f.block)
		t.Fatal("cancel response blocked on the forge round-trips")
	}
	// The sweep is genuinely in flight and blocked on the forge.
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep never reached the forge")
	}
	if n := len(sweptRows(ar)); n != 0 {
		t.Fatalf("row written while the forge was still blocked")
	}
	close(f.block)
	s.waitBranchSweeps()
	if row := oneSweptRow(t, ar); len(row.Deleted) != 2 {
		t.Errorf("deleted = %v, want both stage branches after the forge unblocks", row.Deleted)
	}
}

// --- end-to-end GitHub stub ---------------------------------------------

// githubSweepStub is an httptest GitHub serving the three endpoints the sweep
// crosses (ref probe, PR list, ref delete) with real branch state, so the
// test drives githubclient -> forgegithub -> sweep -> audit end to end.
type githubSweepStub struct {
	mu       sync.Mutex
	branches map[string]bool
	openPRs  map[headBase]bool // (head, base) of every open PR
	deletes  []string          // escaped request paths of every DELETE
}

func newGitHubSweepStub(t *testing.T, branches ...string) (*githubSweepStub, *githubclient.Client) {
	t.Helper()
	st := &githubSweepStub{branches: map[string]bool{}, openPRs: map[headBase]bool{}}
	for _, b := range branches {
		st.branches[b] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(st.serve))
	t.Cleanup(srv.Close)
	return st, &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stub.StaticTokens{Value: "tok"},
		HTTP:    srv.Client(),
	}
}

func (st *githubSweepStub) serve(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()
	path := r.URL.EscapedPath()
	const refProbe, refDelete, pulls = "/repos/x/y/git/ref/heads/", "/repos/x/y/git/refs/heads/", "/repos/x/y/pulls"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, refProbe):
		if !st.branches[strings.TrimPrefix(path, refProbe)] {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"object":{"sha":"abc123"}}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, refDelete):
		st.deletes = append(st.deletes, path)
		b := strings.TrimPrefix(path, refDelete)
		if !st.branches[b] {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Reference does not exist"}`))
			return
		}
		delete(st.branches, b)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == pulls:
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "x:")
		base := r.URL.Query().Get("base")
		var out []map[string]any
		for k := range st.openPRs {
			if k.head == head && (base == "" || k.base == base) {
				out = append(out, map[string]any{"number": 9, "state": "open", "html_url": "https://github.com/x/y/pull/9"})
			}
		}
		if out == nil {
			out = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

func (st *githubSweepStub) deletePaths() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := append([]string(nil), st.deletes...)
	sort.Strings(out)
	return out
}

// sweptAuditRow decodes the single persisted run_branches_swept row.
func sweptAuditRow(t *testing.T, rows []audit.ChainAppendParams) runBranchesSweptPayload {
	t.Helper()
	var out []runBranchesSweptPayload
	for _, p := range rows {
		if p.Category == RunBranchesSweptCategory {
			var body runBranchesSweptPayload
			if err := json.Unmarshal(p.Payload, &body); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			out = append(out, body)
		}
	}
	if len(out) != 1 {
		t.Fatalf("run_branches_swept rows = %d, want 1", len(out))
	}
	return out[0]
}

// resolvedSweepForge is a registry-resolved forge.Forge carrying the sweep
// capability (the non-GitHub rung of runBranchForgeFor).
type resolvedSweepForge struct {
	forge.Forge
	f *fakeRunBranchForge
}

func (r resolvedSweepForge) DeleteRef(ctx context.Context, s forge.CredentialScope, repo forge.RepoRef, b string) error {
	return r.f.DeleteRef(ctx, s, repo, b)
}

func (r resolvedSweepForge) ListOpenPullRequestsByHead(ctx context.Context, s forge.CredentialScope, repo forge.RepoRef, h, b string) ([]forge.PullRequest, error) {
	return r.f.ListOpenPullRequestsByHead(ctx, s, repo, h, b)
}

func (r resolvedSweepForge) GetBranchSHA(ctx context.Context, s forge.CredentialScope, repo forge.RepoRef, b string) (string, bool, error) {
	return r.f.GetBranchSHA(ctx, s, repo, b)
}

// noDeleteForge is a resolved forge.Forge WITHOUT forge.RefDeleter.
type noDeleteForge struct{ forge.Forge }

func TestRunBranchForgeFor_NonGitHubFamilyLadder(t *testing.T) {
	f := newFakeRunBranchForge()
	var typedNil *noDeleteForge
	cases := []struct {
		name    string
		got     forge.Forge
		wantNil bool
	}{
		{"capable forge resolves", resolvedSweepForge{f: f}, false},
		{"forge without RefDeleter is nil", noDeleteForge{}, true},
		{"typed-nil forge is nil", typedNil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Addr: "127.0.0.1:0", ForgeResolver: func(id string) (forge.Forge, error) {
				if id != "gitlab" {
					t.Errorf("resolver id = %q, want gitlab", id)
				}
				return tc.got, nil
			}})
			if got := s.runBranchForgeFor("gitlab"); (got == nil) != tc.wantNil {
				t.Errorf("runBranchForgeFor nil = %v, want %v", got == nil, tc.wantNil)
			}
		})
	}

	// End to end through the resolved rung: a gitlab-family run sweeps.
	r := sweepRun(run.StateSucceeded)
	ref := "gitlab:5"
	r.InstallationRef = &ref
	ar := &prEventsAuditRepo{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: sweepRunRepo(r, nil), AuditRepo: ar,
		ForgeResolver: func(string) (forge.Forge, error) { return resolvedSweepForge{f: f}, nil }})
	s.sweepRunBranches(context.Background(), r, sweepTriggerPRMerged)
	if got := f.deletedSet(); !reflect.DeepEqual(got, []string{sweepBranchA, sweepBranchB}) {
		t.Errorf("deleted = %v, want both stage branches via the resolved forge", got)
	}
}

func TestTruncateSweepError_CapsLength(t *testing.T) {
	long := strings.Repeat("x", sweepErrorMaxLen+50)
	if got := truncateSweepError(errors.New(long)); len(got) != sweepErrorMaxLen {
		t.Errorf("len = %d, want %d", len(got), sweepErrorMaxLen)
	}
	if got := truncateSweepError(errors.New("short")); got != "short" {
		t.Errorf("got %q", got)
	}
}
