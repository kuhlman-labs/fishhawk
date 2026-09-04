package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// --- #3132 childSliceVerifyEvidence fixtures ------------------------------

// hashKeyedTraceStore serves a DIFFERENT bundle body per content hash, which the
// shared single-body priorDiffTraceStore cannot do — the multi-child cases need
// each slice to resolve its OWN evidence, or a per-slice assertion would pass
// vacuously against one shared body.
type hashKeyedTraceStore struct {
	bodies map[string][]byte
}

func (s *hashKeyedTraceStore) Put(context.Context, tracestore.BundleRef, io.Reader) error { return nil }
func (s *hashKeyedTraceStore) Get(_ context.Context, ref tracestore.BundleRef) (io.ReadCloser, error) {
	b, ok := s.bodies[ref.ContentHash]
	if !ok {
		return nil, errors.New("hashKeyedTraceStore: no body for hash " + ref.ContentHash)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (s *hashKeyedTraceStore) Stat(context.Context, tracestore.BundleRef) (tracestore.Stat, error) {
	return tracestore.Stat{}, errors.New("hashKeyedTraceStore: Stat not used")
}
func (s *hashKeyedTraceStore) List(context.Context, uuid.UUID) ([]tracestore.BundleRef, error) {
	return nil, errors.New("hashKeyedTraceStore: List not used")
}

// stageListFailingRepo wraps orchestratorRepo and fails ListStagesForRun for the
// named child run only. Embedding + override keeps the shared fake untouched
// while seeding the bad state BY CONSTRUCTION, so a counterfactual RED lands on
// the behavioral assertion rather than on fixture setup.
type stageListFailingRepo struct {
	*orchestratorRepo
	failFor uuid.UUID
}

func (r *stageListFailingRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if runID == r.failFor {
		return nil, errors.New("stage list blew up")
	}
	return r.orchestratorRepo.ListStagesForRun(ctx, runID)
}

// sliceVerifyFixture accumulates children, their implement stages, their
// trace_uploaded audit rows and their bundle bodies.
type sliceVerifyFixture struct {
	t     *testing.T
	rr    *orchestratorRepo
	ar    *feedbackAuditRepo
	ts    *hashKeyedTraceStore
	seq   int64
	nHash int
}

func newSliceVerifyFixture(t *testing.T) *sliceVerifyFixture {
	t.Helper()
	rr := newOrchestratorRepo()
	rr.seedRun()
	return &sliceVerifyFixture{
		t:  t,
		rr: rr,
		ar: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{}},
		ts: &hashKeyedTraceStore{bodies: map[string][]byte{}},
	}
}

func (f *sliceVerifyFixture) server() *Server {
	return New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr, AuditRepo: f.ar, TraceStore: f.ts})
}

// seedImplementStage inserts an implement-typed stage on runID. seedStage always
// builds a PLAN stage, so this writes the fake's maps directly rather than
// extending the shared helper.
func (f *sliceVerifyFixture) seedImplementStage(runID uuid.UUID, state run.StageState) *run.Stage {
	st := &run.Stage{
		ID: uuid.New(), RunID: runID, Sequence: 2,
		Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		State: state, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	f.rr.mu.Lock()
	f.rr.stagesByID[st.ID] = st
	f.rr.stagesByRunID[runID] = append(f.rr.stagesByRunID[runID], st)
	f.rr.mu.Unlock()
	return st
}

// seedTrace records a trace_uploaded audit row for (runID, stageID) and stores
// a redacted bundle carrying gateJSON under a fresh content hash.
func (f *sliceVerifyFixture) seedTrace(runID, stageID uuid.UUID, gateJSON string) {
	f.t.Helper()
	f.nHash++
	// Unique per seeded bundle and exactly 64 hex chars, which is what
	// pickRedactedTraceHash requires of a content hash.
	hash := fmt.Sprintf("%064x", f.nHash)
	f.seq++
	f.ar.byRunID[runID] = append(f.ar.byRunID[runID],
		makeTraceUploadedEntry(f.t, f.seq, runID, stageID, "redacted", hash))
	f.ts.bodies[hash] = makeRedactedGateEvidenceBundle(f.t, gateJSON)
}

// seedSlice is the happy-path child builder: a decomposed child with an
// implement stage, a trace row and a bundle.
func (f *sliceVerifyFixture) seedSlice(parent uuid.UUID, idx int, gateJSON string) *run.Run {
	f.t.Helper()
	child := f.rr.seedDecomposedChild(parent, idx, run.StateSucceeded)
	st := f.seedImplementStage(child.ID, run.StageStateSucceeded)
	f.seedTrace(child.ID, st.ID, gateJSON)
	return child
}

const passingSliceGate = `{"verify_runs":[{"command":"scripts/test verify","exit_code":0,"outcome":"passed",` +
	`"head_sha":"HEADSHA_PASSING","output_tail":"PASSED_TAIL_SENTINEL"}],` +
	`"verify_summary":{"outcome":"passed","iterations":1,"max_iterations":3}}`

const failingSliceGate = `{"verify_runs":[{"command":"scripts/test verify","exit_code":1,"outcome":"failed",` +
	`"head_sha":"HEADSHA_FAILING","output_tail":"FAILED_TAIL_SENTINEL"}],` +
	`"verify_summary":{"outcome":"failed","iterations":3,"max_iterations":3}}`

// --- guards ---------------------------------------------------------------

// An ordinary run has no decomposition children: nil, no allocation, and the
// caller leaves the prompt byte-identical.
func TestChildSliceVerifyEvidence_OrdinaryRunReturnsNil(t *testing.T) {
	f := newSliceVerifyFixture(t)
	got, omitted := f.server().childSliceVerifyEvidence(context.Background(), uuid.New())
	if got != nil || omitted != 0 {
		t.Errorf("ordinary run: got %v / omitted %d, want nil / 0", got, omitted)
	}
}

func TestChildSliceVerifyEvidence_NilRepoReturnsNil(t *testing.T) {
	f := newSliceVerifyFixture(t)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f.ar, TraceStore: f.ts})
	if got, omitted := s.childSliceVerifyEvidence(context.Background(), uuid.New()); got != nil || omitted != 0 {
		t.Errorf("nil RunRepo: got %v / %d, want nil / 0", got, omitted)
	}
}

func TestChildSliceVerifyEvidence_NilAuditRepoReturnsNil(t *testing.T) {
	f := newSliceVerifyFixture(t)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr, TraceStore: f.ts})
	if got, omitted := s.childSliceVerifyEvidence(context.Background(), uuid.New()); got != nil || omitted != 0 {
		t.Errorf("nil AuditRepo: got %v / %d, want nil / 0", got, omitted)
	}
}

func TestChildSliceVerifyEvidence_NilTraceStoreReturnsNil(t *testing.T) {
	f := newSliceVerifyFixture(t)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr, AuditRepo: f.ar})
	if got, omitted := s.childSliceVerifyEvidence(context.Background(), uuid.New()); got != nil || omitted != 0 {
		t.Errorf("nil TraceStore: got %v / %d, want nil / 0", got, omitted)
	}
}

// A listAllDecomposedChildren error degrades to nil — never blocks the review.
func TestChildSliceVerifyEvidence_ListChildrenErrorReturnsNil(t *testing.T) {
	f := newSliceVerifyFixture(t)
	f.rr.listRunsErr = errors.New("boom")
	if got, omitted := f.server().childSliceVerifyEvidence(context.Background(), uuid.New()); got != nil || omitted != 0 {
		t.Errorf("list-children error: got %v / %d, want nil / 0", got, omitted)
	}
}

// --- resolution -----------------------------------------------------------

// Two children resolve to two rows in slice-index order, each carrying the
// commands, outcomes, exit codes and head SHA actually read from ITS OWN bundle.
func TestChildSliceVerifyEvidence_TwoChildrenOrderedBySliceIndex(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	// Seed out of index order so the assertion is on the SORT, not on insertion.
	c1 := f.seedSlice(parent, 1, `{"verify_runs":[{"command":"go build ./...","exit_code":0,"outcome":"passed",`+
		`"head_sha":"SHA_SLICE_ONE"}],"verify_summary":{"outcome":"passed","iterations":2,"max_iterations":3}}`)
	c0 := f.seedSlice(parent, 0, passingSliceGate)

	got, omitted := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 2 || omitted != 0 {
		t.Fatalf("got %d rows / omitted %d, want 2 / 0: %+v", len(got), omitted, got)
	}
	if got[0].SliceIndex == nil || *got[0].SliceIndex != 0 || got[0].ChildRunID != c0.ID.String() {
		t.Errorf("row 0 = %+v, want slice 0 / child %s", got[0], c0.ID)
	}
	if got[1].SliceIndex == nil || *got[1].SliceIndex != 1 || got[1].ChildRunID != c1.ID.String() {
		t.Errorf("row 1 = %+v, want slice 1 / child %s", got[1], c1.ID)
	}
	if got[0].UnavailableReason != "" || got[1].UnavailableReason != "" {
		t.Errorf("resolved rows must carry no reason: %+v", got)
	}
	if got[0].ChildStageState != string(run.StageStateSucceeded) {
		t.Errorf("row 0 stage state = %q", got[0].ChildStageState)
	}
	if len(got[0].VerifyRuns) != 1 || got[0].VerifyRuns[0].Command != "scripts/test verify" ||
		got[0].VerifyRuns[0].Outcome != "passed" || got[0].VerifyRuns[0].ExitCode != 0 {
		t.Errorf("row 0 verify runs = %+v", got[0].VerifyRuns)
	}
	if got[0].VerifiedHeadSHA != "HEADSHA_PASSING" {
		t.Errorf("row 0 verified head = %q, want HEADSHA_PASSING", got[0].VerifiedHeadSHA)
	}
	if len(got[1].VerifyRuns) != 1 || got[1].VerifyRuns[0].Command != "go build ./..." {
		t.Errorf("row 1 verify runs = %+v (must come from ITS OWN bundle)", got[1].VerifyRuns)
	}
	if got[1].VerifiedHeadSHA != "SHA_SLICE_ONE" {
		t.Errorf("row 1 verified head = %q, want SHA_SLICE_ONE", got[1].VerifiedHeadSHA)
	}
	if got[0].VerifySummary == nil || got[0].VerifySummary.Outcome != "passed" || got[0].VerifySummary.Iterations != 1 {
		t.Errorf("row 0 summary = %+v", got[0].VerifySummary)
	}
}

// --- named absences (one case per branch) ---------------------------------

// A child with no implement stage emits a NAMED-REASON row, not a dropped slice.
func TestChildSliceVerifyEvidence_ChildWithoutImplementStageEmitsNamedReason(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	child := f.rr.seedDecomposedChild(parent, 0, run.StateSucceeded)
	f.rr.seedStage(child.ID, 1, run.StageStateSucceeded) // a PLAN stage only

	got, _ := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (the slice must NOT be dropped): %+v", len(got), got)
	}
	if got[0].UnavailableReason != "child_has_no_implement_stage" {
		t.Errorf("reason = %q, want child_has_no_implement_stage", got[0].UnavailableReason)
	}
	if got[0].ChildRunID != child.ID.String() {
		t.Errorf("child run id = %q, want %q", got[0].ChildRunID, child.ID)
	}
}

// A child whose ListStagesForRun errors emits its named row AND its healthy
// sibling still resolves — best-effort per child, never all-or-nothing.
func TestChildSliceVerifyEvidence_ChildStageListErrorEmitsNamedReasonAndSiblingsSurvive(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	failing := f.seedSlice(parent, 0, passingSliceGate)
	sibling := f.seedSlice(parent, 1, passingSliceGate)
	s := New(Config{Addr: "127.0.0.1:0",
		RunRepo:    &stageListFailingRepo{orchestratorRepo: f.rr, failFor: failing.ID},
		AuditRepo:  f.ar,
		TraceStore: f.ts})

	got, _ := s.childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (failing row + surviving sibling): %+v", len(got), got)
	}
	byChild := map[string]prompt.GateSliceVerify{}
	for _, r := range got {
		byChild[r.ChildRunID] = r
	}
	if r := byChild[failing.ID.String()]; r.UnavailableReason != "child_stage_list_failed" {
		t.Errorf("failing child reason = %q, want child_stage_list_failed", r.UnavailableReason)
	}
	sib := byChild[sibling.ID.String()]
	if sib.UnavailableReason != "" || len(sib.VerifyRuns) != 1 {
		t.Errorf("sibling must still resolve fully: %+v", sib)
	}
}

// A child with no trace_uploaded row passes resolveStageGateEvidence's literal
// through onto the row.
func TestChildSliceVerifyEvidence_MissingTraceEmitsNoRedactedTraceForStage(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	child := f.rr.seedDecomposedChild(parent, 0, run.StateSucceeded)
	f.seedImplementStage(child.ID, run.StageStateSucceeded) // no trace seeded

	got, _ := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 1 || got[0].UnavailableReason != "no_redacted_trace_for_stage" {
		t.Fatalf("got %+v, want one row reasoned no_redacted_trace_for_stage", got)
	}
}

// A bundle that parses but carries no verify run draws the partial literal.
func TestChildSliceVerifyEvidence_BundleWithoutVerifyRunsEmitsNoVerifyRunsInGateEvidence(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	f.seedSlice(parent, 0, `{"scope_facts":{"declared_files":2}}`)

	got, _ := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 1 || got[0].UnavailableReason != "no_verify_runs_in_gate_evidence" {
		t.Fatalf("got %+v, want one row reasoned no_verify_runs_in_gate_evidence", got)
	}
}

// --- per-run filtering ----------------------------------------------------

// Superseded runs are dropped (an absorbed iteration is not the slice's
// committed-tree result), while the head SHA is still read from the raw list.
func TestChildSliceVerifyEvidence_SupersededRunsDropped(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	f.seedSlice(parent, 0, `{"verify_runs":[`+
		`{"command":"stale iteration","exit_code":1,"outcome":"failed","head_sha":"SHA_FROM_SUPERSEDED","superseded":true},`+
		`{"command":"scripts/test verify","exit_code":0,"outcome":"passed"}],`+
		`"verify_summary":{"outcome":"passed","iterations":2,"max_iterations":3}}`)

	got, _ := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	if len(got[0].VerifyRuns) != 1 || got[0].VerifyRuns[0].Command != "scripts/test verify" {
		t.Errorf("superseded run must be dropped, got %+v", got[0].VerifyRuns)
	}
	if got[0].VerifiedHeadSHA != "SHA_FROM_SUPERSEDED" {
		t.Errorf("head SHA must be read from the RAW run list, got %q", got[0].VerifiedHeadSHA)
	}
}

// A `passed` run's output tail is blanked (bounding: a green tail carries
// nothing a reviewer needs); a FAILED run's tail is kept.
func TestChildSliceVerifyEvidence_PassedRunTailBlanked(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	f.seedSlice(parent, 0, passingSliceGate)
	f.seedSlice(parent, 1, failingSliceGate)

	got, _ := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	var passed, failed prompt.GateSliceVerify
	for _, r := range got {
		if r.VerifySummary != nil && r.VerifySummary.Outcome == "passed" {
			passed = r
		} else {
			failed = r
		}
	}
	if len(passed.VerifyRuns) != 1 || passed.VerifyRuns[0].OutputTail != "" {
		t.Errorf("passed run tail must be blanked, got %q", passed.VerifyRuns[0].OutputTail)
	}
	if len(failed.VerifyRuns) != 1 || failed.VerifyRuns[0].OutputTail != "FAILED_TAIL_SENTINEL" {
		t.Errorf("failed run tail must be KEPT, got %+v", failed.VerifyRuns)
	}
}

// The per-slice run bound keeps the LAST N — the terminal run is authoritative.
func TestChildSliceVerifyEvidence_CapsRunsPerSlice(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	var runs []string
	for i := 0; i < prompt.MaxSliceVerifyRunsPerSlice+2; i++ {
		runs = append(runs, fmt.Sprintf(`{"command":"cmd-%d","exit_code":1,"outcome":"failed"}`, i))
	}
	f.seedSlice(parent, 0, `{"verify_runs":[`+strings.Join(runs, ",")+`]}`)

	got, _ := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != 1 || len(got[0].VerifyRuns) != prompt.MaxSliceVerifyRunsPerSlice {
		t.Fatalf("got %d runs, want %d: %+v", len(got[0].VerifyRuns), prompt.MaxSliceVerifyRunsPerSlice, got)
	}
	last := got[0].VerifyRuns[len(got[0].VerifyRuns)-1]
	if last.Command != fmt.Sprintf("cmd-%d", prompt.MaxSliceVerifyRunsPerSlice+1) {
		t.Errorf("the TERMINAL run must survive the bound, got %q", last.Command)
	}
}

// --- the entry bound ------------------------------------------------------

func TestChildSliceVerifyEvidence_CapsAtMaxSliceVerifyEntries(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	for i := 0; i < prompt.MaxSliceVerifyEntries+3; i++ {
		f.seedSlice(parent, i, passingSliceGate)
	}
	got, omitted := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != prompt.MaxSliceVerifyEntries || omitted != 3 {
		t.Fatalf("got %d rows / omitted %d, want %d / 3", len(got), omitted, prompt.MaxSliceVerifyEntries)
	}
}

// OPERATOR BINDING CONDITION 1. A FAILED slice and an UNRESOLVED slice both sit
// BEYOND the bound in slice-index order. Both MUST survive, and everything the
// bound dropped must be passing — under a plain index ordering the one row whose
// absence endangers a merge is exactly the row the bound silently eats.
func TestChildSliceVerifyEvidence_CapNeverDropsANonPassingSlice(t *testing.T) {
	f := newSliceVerifyFixture(t)
	parent := uuid.New()
	// Indices 0..cap-1 pass; the two non-passing slices sit at cap and cap+1.
	for i := 0; i < prompt.MaxSliceVerifyEntries; i++ {
		f.seedSlice(parent, i, passingSliceGate)
	}
	failedChild := f.seedSlice(parent, prompt.MaxSliceVerifyEntries, failingSliceGate)
	unresolvedChild := f.rr.seedDecomposedChild(parent, prompt.MaxSliceVerifyEntries+1, run.StateSucceeded)
	f.rr.seedStage(unresolvedChild.ID, 1, run.StageStateSucceeded) // plan stage only → unresolvable

	got, omitted := f.server().childSliceVerifyEvidence(context.Background(), parent)
	if len(got) != prompt.MaxSliceVerifyEntries || omitted != 2 {
		t.Fatalf("got %d rows / omitted %d, want %d / 2", len(got), omitted, prompt.MaxSliceVerifyEntries)
	}
	var sawFailed, sawUnresolved bool
	for _, r := range got {
		switch r.ChildRunID {
		case failedChild.ID.String():
			sawFailed = true
			if r.VerifySummary == nil || r.VerifySummary.Outcome != "failed" {
				t.Errorf("failed slice row lost its failed summary: %+v", r)
			}
		case unresolvedChild.ID.String():
			sawUnresolved = true
			if r.UnavailableReason != "child_has_no_implement_stage" {
				t.Errorf("unresolved slice row reason = %q", r.UnavailableReason)
			}
		}
	}
	if !sawFailed {
		t.Errorf("the FAILED slice beyond the bound was dropped — the cap must never drop a non-passing slice")
	}
	if !sawUnresolved {
		t.Errorf("the UNRESOLVED slice beyond the bound was dropped — the cap must never drop a non-passing slice")
	}
	// Everything that DID survive besides those two must be passing, i.e. the
	// bound only ever ate passing rows.
	for _, r := range got {
		if r.ChildRunID == failedChild.ID.String() || r.ChildRunID == unresolvedChild.ID.String() {
			continue
		}
		if r.UnavailableReason != "" || r.VerifySummary == nil || r.VerifySummary.Outcome != "passed" {
			t.Errorf("unexpected non-passing survivor: %+v", r)
		}
	}
}
