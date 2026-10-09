package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// --- #4077 boot re-dispatch fixtures ---------------------------------------

// specRedispatchAdvisory: a 2-agent advisory plan review and a 1-agent
// advisory implement review.
var specRedispatchAdvisory = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 2
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 1
`)

// specRedispatchNoReviewers is the same workflow with the reviewers removed:
// a dispatcher running under it starts no round.
var specRedispatchNoReviewers = []byte(`version: "0.3"
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
`)

// blockingReviewer stands in for a reviewer still running when the daemon
// dies: Review blocks until release is closed (or its context ends).
type blockingReviewer struct {
	release chan struct{}
	once    sync.Once
}

func newBlockingReviewer() *blockingReviewer {
	return &blockingReviewer{release: make(chan struct{})}
}

func (b *blockingReviewer) Review(ctx context.Context, _ string) (*planreview.ReviewVerdict, string, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	return &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, "claude-opus-4-8", nil
}

func (b *blockingReviewer) unblock() { b.once.Do(func() { close(b.release) }) }

// redispatchTraceStore stores every Put by (variant, content hash) and serves
// it back on Get — the round trip a trace-origin re-dispatch needs.
type redispatchTraceStore struct {
	mu     sync.Mutex
	bodies map[string][]byte
}

func newRedispatchTraceStore() *redispatchTraceStore {
	return &redispatchTraceStore{bodies: map[string][]byte{}}
}

func (s *redispatchTraceStore) put(variant tracestore.Variant, hash string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[string(variant)+"/"+hash] = b
}

func (s *redispatchTraceStore) Put(_ context.Context, ref tracestore.BundleRef, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.put(ref.Variant, ref.ContentHash, b)
	return nil
}

func (s *redispatchTraceStore) Get(_ context.Context, ref tracestore.BundleRef) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bodies[string(ref.Variant)+"/"+ref.ContentHash]
	if !ok {
		return nil, errors.New("redispatchTraceStore: no bundle for " + ref.ContentHash)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *redispatchTraceStore) Stat(context.Context, tracestore.BundleRef) (tracestore.Stat, error) {
	return tracestore.Stat{}, errors.New("redispatchTraceStore: Stat not used")
}

func (s *redispatchTraceStore) List(context.Context, uuid.UUID) ([]tracestore.BundleRef, error) {
	return nil, errors.New("redispatchTraceStore: List not used")
}

// recordingCompareClient is a githubclient.Client whose compare endpoint
// returns body and records every compare path it served.
func recordingCompareClient(t *testing.T, body string) (*githubclient.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/compare/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// redispatchFixtureOpts varies ONE input of an otherwise ELIGIBLE fixture, so
// every per-mode test isolates the control it names.
type redispatchFixtureOpts struct {
	spec            []byte
	reviewerUnwired bool
	noArtifactRepo  bool
	traceStore      tracestore.Storage
	github          *githubclient.Client
	planStageState  run.StageState
}

type redispatchFixture struct {
	s         *Server
	au        *auditFake
	rr        *orchestratorRepo
	art       *fakeArtifactRepo
	runRow    *run.Run
	planStage *run.Stage
	implStage *run.Stage
	reviewer  *fakePlanReviewer
}

// newRedispatchFixture builds the RESTARTED server (processStart = bootMarker)
// over a run with an awaiting_approval plan stage, a running implement stage,
// an artifact repo and a completing reviewer: by default a seeded advisory
// round is eligible. The audit fake stamps real sequences.
func newRedispatchFixture(t *testing.T, opts redispatchFixtureOpts) *redispatchFixture {
	t.Helper()
	f := &redispatchFixture{
		au:       newAuditFake(),
		rr:       newOrchestratorRepo(),
		art:      newFakeArtifactRepo(),
		reviewer: &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"},
	}
	f.au.stampSequence = true
	spec := opts.spec
	if spec == nil {
		spec = specRedispatchAdvisory
	}
	f.runRow = f.rr.seedRun()
	f.runRow.WorkflowID = "feature_change"
	f.runRow.WorkflowSpec = spec
	f.runRow.Repo = "kuhlman-labs/example"
	inst := int64(55)
	f.runRow.InstallationID = &inst
	planState := opts.planStageState
	if planState == "" {
		planState = run.StageStateAwaitingApproval
	}
	f.planStage = f.rr.seedStage(f.runRow.ID, 0, planState)
	f.planStage.RequiresApproval = true
	f.implStage = f.rr.seedStage(f.runRow.ID, 1, run.StageStateRunning)
	f.implStage.Type = run.StageTypeImplement
	f.implStage.RequiresApproval = true

	cfg := Config{
		Addr:         "127.0.0.1:0",
		AuditRepo:    f.au,
		RunRepo:      f.rr,
		ProcessStart: bootMarker,
	}
	if !opts.noArtifactRepo {
		cfg.ArtifactRepo = f.art
	}
	if !opts.reviewerUnwired {
		cfg.PlanReviewers = singleReviewerSet{f.reviewer}
	}
	if opts.traceStore != nil {
		cfg.TraceStore = opts.traceStore
	}
	if opts.github != nil {
		cfg.GitHub = opts.github
	}
	f.s = New(cfg)
	return f
}

// appendAt appends an entry through the fake's chained append (so later
// re-dispatch appends sequence after it) and returns its sequence.
func (f *redispatchFixture) appendAt(t *testing.T, stageID uuid.UUID, ts time.Time, category string, payload any) int64 {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", category, err)
	}
	sid := stageID
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runRow.ID, StageID: &sid, Timestamp: ts, Category: category, Payload: b,
	}); err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	return int64(len(f.au.appended))
}

// seedPlanArtifact stores content as the plan stage's artifact under hash and
// records the plan_generated naming it.
func (f *redispatchFixture) seedPlanArtifact(t *testing.T, content []byte, hash string, storeArtifact bool) {
	t.Helper()
	if storeArtifact {
		sv := "standard_v1"
		if _, err := f.art.Create(context.Background(), artifact.CreateParams{
			StageID: f.planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv,
			Content: content, ContentHash: hash,
		}); err != nil {
			t.Fatalf("seed plan artifact: %v", err)
		}
	}
	f.appendAt(t, f.planStage.ID, beforeBoot, "plan_generated", map[string]any{"content_hash": hash})
}

// seedEligiblePlanRound seeds a stored plan, its plan_generated, a scope
// pre-check between them, and an orphaned advisory 2-agent round; returns the
// round's sequence.
func (f *redispatchFixture) seedEligiblePlanRound(t *testing.T, started planreview.ReviewStartedPayload) int64 {
	t.Helper()
	f.seedPlanArtifact(t, validPlanBytes(t), "plan-hash", true)
	f.appendAt(t, f.planStage.ID, beforeBoot, categoryPlanScopePrecheck, ScopePrecheckPayload{})
	return f.appendAt(t, f.planStage.ID, beforeBoot, "plan_review_started", started)
}

func advisoryPlanStarted() planreview.ReviewStartedPayload {
	return planreview.ReviewStartedPayload{ConfiguredAgents: 2, Authority: planreview.AuthorityAdvisory}
}

func advisoryImplementStarted(origin, base, head string) planreview.ReviewStartedPayload {
	return planreview.ReviewStartedPayload{
		ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory,
		RoundOrigin: origin, RoundBaseSHA: base, HeadSHA: head,
	}
}

// entries returns the run's category entries (sequence-stamped).
func redispatchEntries(t *testing.T, au *auditFake, runID uuid.UUID, category string) []*audit.Entry {
	t.Helper()
	got, err := au.ListForRunByCategory(context.Background(), runID, category)
	if err != nil {
		t.Fatalf("list %s: %v", category, err)
	}
	return got
}

// failedReasonsAfter decodes the reasons of the category's *_review_failed
// entries sequenced after seq.
func failedReasonsAfter(t *testing.T, au *auditFake, runID uuid.UUID, category string, seq int64) []string {
	t.Helper()
	var out []string
	for _, e := range redispatchEntries(t, au, runID, category) {
		if e.Sequence <= seq {
			continue
		}
		var p planreview.ReviewFailedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", category, err)
		}
		out = append(out, p.Reason)
	}
	return out
}

// redispatchedRows decodes every review_round_redispatched payload.
func redispatchedRows(t *testing.T, au *auditFake, runID uuid.UUID) []reviewRoundRedispatchedPayload {
	t.Helper()
	var out []reviewRoundRedispatchedPayload
	for _, e := range redispatchEntries(t, au, runID, categoryReviewRoundRedispatched) {
		var p reviewRoundRedispatchedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", categoryReviewRoundRedispatched, err)
		}
		out = append(out, p)
	}
	return out
}

// startedAfter returns the category's started payloads sequenced after seq,
// with their sequences.
func startedAfter(t *testing.T, au *auditFake, runID uuid.UUID, category string, seq int64) ([]planreview.ReviewStartedPayload, []int64) {
	t.Helper()
	var out []planreview.ReviewStartedPayload
	var seqs []int64
	for _, e := range redispatchEntries(t, au, runID, category) {
		if e.Sequence <= seq {
			continue
		}
		var p planreview.ReviewStartedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", category, err)
		}
		out = append(out, p)
		seqs = append(seqs, e.Sequence)
	}
	return out, seqs
}

func countAfter(t *testing.T, au *auditFake, runID uuid.UUID, category string, seq int64) int {
	t.Helper()
	n := 0
	for _, e := range redispatchEntries(t, au, runID, category) {
		if e.Sequence > seq {
			n++
		}
	}
	return n
}

func bootSweep(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	s.waitBackgroundReviews()
}

// assertRedispatchedRound asserts exactly one review_round_redispatched names
// orphanedSeq, a newer started round records the lineage (depth 1), wantVerdicts
// verdicts landed after it, and no *_review_failed landed after the orphan.
func assertRedispatchedRound(t *testing.T, au *auditFake, runID uuid.UUID, stage orphanedReviewStageKind, orphanedSeq int64, wantVerdicts int) planreview.ReviewStartedPayload {
	t.Helper()
	rows := redispatchedRows(t, au, runID)
	if len(rows) != 1 || rows[0].OrphanedRoundSequence != orphanedSeq || rows[0].Stage != stage.label || rows[0].RedispatchDepth != 1 {
		t.Fatalf("%s rows = %+v, want one naming stage %s sequence %d depth 1", categoryReviewRoundRedispatched, rows, stage.label, orphanedSeq)
	}
	if n := len(failedReasonsAfter(t, au, runID, stage.failed, orphanedSeq)); n != 0 {
		t.Fatalf("%s count = %d, want 0 (the round was re-dispatched, not failed)", stage.failed, n)
	}
	started, seqs := startedAfter(t, au, runID, stage.started, orphanedSeq)
	if len(started) != 1 {
		t.Fatalf("newer %s rounds = %d, want 1", stage.started, len(started))
	}
	if started[0].RedispatchOf != orphanedSeq || started[0].RedispatchDepth != 1 {
		t.Errorf("re-dispatched round lineage = of %d depth %d, want of %d depth 1", started[0].RedispatchOf, started[0].RedispatchDepth, orphanedSeq)
	}
	verdict := strings.TrimSuffix(stage.started, "_review_started") + "_reviewed"
	if n := countAfter(t, au, runID, verdict, seqs[0]); n != wantVerdicts {
		t.Errorf("%s after the re-dispatched round = %d, want %d", verdict, n, wantVerdicts)
	}
	if reviewRedispatchPending(runID, stage.label, orphanedSeq) {
		t.Error("the orphaned round is still in the pending-redispatch set after the re-dispatch settled")
	}
	return started[0]
}

// --- Cross-boundary restart tests (the done-means) -------------------------

// TestReviewRedispatch_DaemonRestart_PlanRoundCompletes is the done-means
// restart test: server A takes a REAL plan upload through the HTTP handler and
// dies with its advisory 2-agent round in flight (its reviewer blocks); server
// B, over the SAME run/audit/artifact fakes with a later boot marker, runs its
// boot sweep. The round is re-dispatched against the same plan and completes:
// one review_round_redispatched, a newer plan_review_started carrying the
// lineage, two plan_reviewed, ZERO plan_review_failed.
//
// COUNTERFACTUAL C1: replace the boot-mode redispatch branch with the
// synthesize path → plan_review_failed count = 2, want 0 → RED.
func TestReviewRedispatch_DaemonRestart_PlanRoundCompletes(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()
	au.stampSequence = true
	sf := newSigningFake()
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specRedispatchAdvisory
	runRow.Repo = "kuhlman-labs/example"
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true

	blocked := newBlockingReviewer()
	a := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, AuditRepo: au, RunRepo: rr, ArtifactRepo: art, PlanReviewers: singleReviewerSet{blocked}})
	t.Cleanup(func() { blocked.unblock(); a.waitBackgroundReviews() })
	priv, _ := sf.issue(t, runRow.ID)
	if w := shipPlanRequest(t, a, runRow.ID, planStage.ID, priv, validPlanBytes(t), ""); w.Code != http.StatusCreated {
		t.Fatalf("server A plan upload status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	orphaned := redispatchEntries(t, au, runRow.ID, "plan_review_started")
	if len(orphaned) != 1 {
		t.Fatalf("server A plan_review_started = %d, want 1", len(orphaned))
	}

	completing := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	b := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr, ArtifactRepo: art, PlanReviewers: singleReviewerSet{completing}, ProcessStart: time.Now().Add(time.Millisecond)})
	bootSweep(t, b)

	got := assertRedispatchedRound(t, au, runRow.ID, orphanedReviewStages[0], orphaned[0].Sequence, 2)
	if got.ConfiguredAgents != 2 || got.Authority != planreview.AuthorityAdvisory {
		t.Errorf("re-dispatched plan round = %+v, want 2 advisory agents", got)
	}
	completing.mu.Lock()
	defer completing.mu.Unlock()
	if len(completing.calls) != 2 {
		t.Fatalf("server B reviewer invocations = %d, want 2", len(completing.calls))
	}
	if !strings.Contains(completing.calls[0], planfixtureSummary(t)) {
		t.Error("the re-dispatched prompt does not carry the orphaned round's plan")
	}
}

// planfixtureSummary is the summary of the plan validPlanBytes encodes, so the
// restart test can prove the re-dispatched prompt reviews the SAME plan.
func planfixtureSummary(t *testing.T) string {
	t.Helper()
	var p struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(validPlanBytes(t), &p); err != nil || p.Summary == "" {
		t.Fatalf("decode plan fixture summary: %v", err)
	}
	return p.Summary
}

// TestReviewRedispatch_DaemonRestart_ImplementTraceRoundCompletes is the
// implement twin over a REAL trace upload: server A's raw upload dispatches
// the advisory round (its reviewer blocks) and the redacted variant lands in
// the trace store; server B's boot sweep re-dispatches the round from the
// stored redacted bundle against the SAME head and it completes. Re-dispatching
// the same head proves the #797 dedup bypass end to end.
//
// COUNTERFACTUAL C6: delete the marker-gated dedup skip in
// runImplementReviewsForTree → the dedup finds the orphaned round's head, no
// new round starts, and the fallback synthesizes implement_review_failed → RED.
func TestReviewRedispatch_DaemonRestart_ImplementTraceRoundCompletes(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()
	au.stampSequence = true
	sf := newSigningFake()
	ts := newRedispatchTraceStore()
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specRedispatchAdvisory
	runRow.Repo = "kuhlman-labs/example"
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: validPlanBytes(t), ContentHash: "approved-plan",
	}); err != nil {
		t.Fatalf("seed approved plan: %v", err)
	}
	implStage := rr.seedStage(runRow.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	blocked := newBlockingReviewer()
	a := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, TraceStore: ts, AuditRepo: au, RunRepo: rr, ArtifactRepo: art, PlanReviewers: singleReviewerSet{blocked}})
	t.Cleanup(func() { blocked.unblock(); a.waitBackgroundReviews() })
	priv, _ := sf.issue(t, runRow.ID)
	const head = "restart-head"
	bundleBytes := implementPushGatedBundleWithHeadSHA(t, 2, head)
	for _, variant := range []string{"raw", "redacted"} {
		if w := shipRequest(t, a, runRow.ID, implStage.ID, variant, priv, bundleBytes, ""); w.Code != http.StatusAccepted {
			t.Fatalf("server A %s upload status = %d, want 202:\n%s", variant, w.Code, w.Body.String())
		}
	}
	orphaned := redispatchEntries(t, au, runRow.ID, "implement_review_started")
	if len(orphaned) != 1 {
		t.Fatalf("server A implement_review_started = %d, want 1", len(orphaned))
	}

	completing := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	b := New(Config{Addr: "127.0.0.1:0", TraceStore: ts, AuditRepo: au, RunRepo: rr, ArtifactRepo: art, PlanReviewers: singleReviewerSet{completing}, ProcessStart: time.Now().Add(time.Millisecond)})
	bootSweep(t, b)

	got := assertRedispatchedRound(t, au, runRow.ID, orphanedReviewStages[1], orphaned[0].Sequence, 1)
	if got.HeadSHA != head || got.RoundOrigin != reviewRoundOriginTrace {
		t.Errorf("re-dispatched implement round = head %q origin %q, want %q / trace", got.HeadSHA, got.RoundOrigin, head)
	}
	completing.mu.Lock()
	defer completing.mu.Unlock()
	if len(completing.calls) != 1 || !strings.Contains(completing.calls[0], "file1.go") {
		t.Errorf("server B reviewer calls = %d, want 1 reviewing the stored bundle's diff", len(completing.calls))
	}
}

// TestReviewRedispatch_CompareOrigins: a fixup_push or consolidated round is
// rebuilt as ComparePatch(recorded base, recorded head) and completes, with the
// new round re-stamped with the same source.
func TestReviewRedispatch_CompareOrigins(t *testing.T) {
	cases := []struct {
		name, origin, base, head string
	}{
		{name: "fixup_push", origin: reviewRoundOriginFixupPush, base: "base-pass", head: "head-pushed"},
		{name: "consolidated", origin: reviewRoundOriginConsolidated, base: "consolidated-base", head: "headsha1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh, paths := recordingCompareClient(t, cannedCompareOneFile)
			f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh})
			// The implement review renders the run's approved plan.
			f.seedPlanArtifact(t, validPlanBytes(t), "approved-plan", true)
			seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(tc.origin, tc.base, tc.head))

			bootSweep(t, f.s)

			got := assertRedispatchedRound(t, f.au, f.runRow.ID, orphanedReviewStages[1], seq, 1)
			if got.RoundOrigin != tc.origin || got.RoundBaseSHA != tc.base || got.HeadSHA != tc.head {
				t.Errorf("re-dispatched round source = %+v, want origin %s base %s head %s", got, tc.origin, tc.base, tc.head)
			}
			p := paths()
			if len(p) != 1 || !strings.HasSuffix(p[0], "/compare/"+tc.base+"..."+tc.head) {
				t.Errorf("compare paths = %v, want one ComparePatch(%s, %s)", p, tc.base, tc.head)
			}
		})
	}
}

// --- Per-mode tests --------------------------------------------------------

// TestReviewRedispatch_IneligibleModes: one row per ineligibility slug. Each
// fixture is otherwise ELIGIBLE (reviewer, artifact repo, awaiting_approval
// plan stage, input source wired), so the slug's own check is the only thing
// in the path. Each asserts the round is synthesized failed with a reason
// naming the slug and that NO review_round_redispatched was appended.
//
// COUNTERFACTUALS: C2 (authority check permissive) → gating_authority RED;
// C3 (already-redispatched lookup returns false) → already_redispatched RED;
// C4 (depth check permissive) → depth_cap_reached RED; C5 (each superseded /
// plan-state check permissive) → its row RED.
func TestReviewRedispatch_IneligibleModes(t *testing.T) {
	cases := []struct {
		name  string
		opts  func() redispatchFixtureOpts
		seed  func(t *testing.T, f *redispatchFixture) (orphanedSeq int64)
		stage orphanedReviewStageKind
		slug  string
	}{
		{
			name: "gating authority", stage: orphanedReviewStages[0], slug: redispatchSlugGatingAuthority,
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				p := advisoryPlanStarted()
				p.Authority = planreview.AuthorityGating
				return f.seedEligiblePlanRound(t, p)
			},
		},
		{
			name: "reviewer backend unwired", stage: orphanedReviewStages[0], slug: redispatchSlugReviewerUnwired,
			opts: func() redispatchFixtureOpts { return redispatchFixtureOpts{reviewerUnwired: true} },
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.seedEligiblePlanRound(t, advisoryPlanStarted())
			},
		},
		{
			name: "already re-dispatched once", stage: orphanedReviewStages[0], slug: redispatchSlugAlreadyRedispatched,
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
				f.appendAt(t, f.planStage.ID, beforeBoot, categoryReviewRoundRedispatched,
					reviewRoundRedispatchedPayload{Stage: "plan", StageID: f.planStage.ID.String(), OrphanedRoundSequence: seq, ConfiguredAgents: 2, RedispatchDepth: 1})
				return seq
			},
		},
		{
			name: "depth cap reached", stage: orphanedReviewStages[0], slug: redispatchSlugDepthCapReached,
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				p := advisoryPlanStarted()
				p.RedispatchOf, p.RedispatchDepth = 1, maxReviewRoundRedispatches
				return f.seedEligiblePlanRound(t, p)
			},
		},
		{
			name: "plan stage no longer awaiting approval", stage: orphanedReviewStages[0], slug: redispatchSlugPlanStageNotAwaiting,
			opts: func() redispatchFixtureOpts { return redispatchFixtureOpts{planStageState: run.StageStateSucceeded} },
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.seedEligiblePlanRound(t, advisoryPlanStarted())
			},
		},
		{
			name: "superseded by a newer plan", stage: orphanedReviewStages[0], slug: redispatchSlugSupersededByNewPlan,
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
				f.appendAt(t, f.planStage.ID, beforeBoot, "plan_generated", map[string]any{"content_hash": "newer-plan"})
				return seq
			},
		},
		{
			name: "artifact repo unwired", stage: orphanedReviewStages[0], slug: redispatchSlugPlanArtifactUnavail,
			opts: func() redispatchFixtureOpts { return redispatchFixtureOpts{noArtifactRepo: true} },
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.seedEligiblePlanRound(t, advisoryPlanStarted())
			},
		},
		{
			name: "unknown round source (legacy implement row)", stage: orphanedReviewStages[1], slug: redispatchSlugUnknownRoundSource,
			opts: func() redispatchFixtureOpts { return redispatchFixtureOpts{traceStore: newRedispatchTraceStore()} },
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted("", "", "h1"))
			},
		},
		{
			name: "compare round missing its base", stage: orphanedReviewStages[1], slug: redispatchSlugUnknownRoundSource,
			opts: func() redispatchFixtureOpts {
				return redispatchFixtureOpts{github: cannedComparePatchClient(t, cannedCompareOneFile)}
			},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginFixupPush, "", "h1"))
			},
		},
		{
			name: "superseded by a fix-up", stage: orphanedReviewStages[1], slug: redispatchSlugSupersededByFixup,
			opts: func() redispatchFixtureOpts { return redispatchFixtureOpts{traceStore: newRedispatchTraceStore()} },
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", "h1"))
				f.appendAt(t, f.implStage.ID, beforeBoot, CategoryStageFixupTriggered, map[string]any{"stage_id": f.implStage.ID.String()})
				return seq
			},
		},
		{
			name: "trace store unwired", stage: orphanedReviewStages[1], slug: redispatchSlugTraceBundleUnavail,
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", "h1"))
			},
		},
		{
			name: "forge compare unwired", stage: orphanedReviewStages[1], slug: redispatchSlugForgeCompareUnavail,
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginFixupPush, "b1", "h1"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts redispatchFixtureOpts
			if tc.opts != nil {
				opts = tc.opts()
			}
			f := newRedispatchFixture(t, opts)
			seq := tc.seed(t, f)

			bootSweep(t, f.s)

			// The already_redispatched fixture seeds the one prior row itself.
			wantRows := 0
			if tc.slug == redispatchSlugAlreadyRedispatched {
				wantRows = 1
			}
			if rows := redispatchedRows(t, f.au, f.runRow.ID); len(rows) != wantRows {
				t.Fatalf("%s rows = %+v, want %d (none appended by the sweep)", categoryReviewRoundRedispatched, rows, wantRows)
			}
			want := orphanedReviewNotRedispatchedReason(tc.slug)
			reasons := failedReasonsAfter(t, f.au, f.runRow.ID, tc.stage.failed, seq)
			wantN := 2
			if tc.stage.isImplement {
				wantN = 1
			}
			if len(reasons) != wantN {
				t.Fatalf("%s count = %d (%v), want %d", tc.stage.failed, len(reasons), reasons, wantN)
			}
			for _, r := range reasons {
				if r != want {
					t.Errorf("synthesized reason = %q, want %q", r, want)
				}
			}
			if started, _ := startedAfter(t, f.au, f.runRow.ID, tc.stage.started, seq); len(started) != 0 {
				t.Errorf("newer %s rounds = %d, want 0", tc.stage.started, len(started))
			}
		})
	}
}

// TestReviewRedispatch_FallbackModes: one row per fallback failure. The round
// is ELIGIBLE, so the sweep audits review_round_redispatched, but the input
// rebuild (or the dispatcher) starts no new round; the fallback then closes the
// orphaned round failed with a reason naming the slug.
//
// COUNTERFACTUAL C8: delete the post-dispatch fallback call → the
// round_not_started row reads 0 failures (the round would stay pending
// forever) → RED, asserted by reading the audit after the call.
func TestReviewRedispatch_FallbackModes(t *testing.T) {
	cases := []struct {
		name  string
		opts  func(t *testing.T) redispatchFixtureOpts
		seed  func(t *testing.T, f *redispatchFixture) int64
		stage orphanedReviewStageKind
		slug  string
	}{
		{
			name: "plan artifact missing", stage: orphanedReviewStages[0], slug: redispatchSlugPlanArtifactUnavail,
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				f.seedPlanArtifact(t, validPlanBytes(t), "never-stored", false)
				return f.appendAt(t, f.planStage.ID, beforeBoot, "plan_review_started", advisoryPlanStarted())
			},
		},
		{
			name: "no redacted bundle with the round's head", stage: orphanedReviewStages[1], slug: redispatchSlugTraceBundleUnavail,
			opts: func(t *testing.T) redispatchFixtureOpts {
				return redispatchFixtureOpts{traceStore: newRedispatchTraceStore()}
			},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", "round-head"))
				// A redacted bundle exists for the stage, but for another head.
				other := implementPushGatedBundleWithHeadSHA(t, 1, "other-head")
				f.seedRedactedBundle(t, other)
				return seq
			},
		},
		{
			name: "compare failed", stage: orphanedReviewStages[1], slug: redispatchSlugCompareFailed,
			opts: func(t *testing.T) redispatchFixtureOpts {
				return redispatchFixtureOpts{github: erroringComparePatchClient(t)}
			},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginFixupPush, "b1", "h1"))
			},
		},
		{
			name: "dispatcher started no round (reviewers removed)", stage: orphanedReviewStages[0], slug: redispatchSlugRoundNotStarted,
			opts: func(t *testing.T) redispatchFixtureOpts {
				return redispatchFixtureOpts{spec: specRedispatchNoReviewers}
			},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.seedEligiblePlanRound(t, advisoryPlanStarted())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts redispatchFixtureOpts
			if tc.opts != nil {
				opts = tc.opts(t)
			}
			f := newRedispatchFixture(t, opts)
			seq := tc.seed(t, f)

			bootSweep(t, f.s)

			rows := redispatchedRows(t, f.au, f.runRow.ID)
			if len(rows) != 1 || rows[0].OrphanedRoundSequence != seq {
				t.Fatalf("%s rows = %+v, want one naming %d", categoryReviewRoundRedispatched, rows, seq)
			}
			wantN := 2
			if tc.stage.isImplement {
				wantN = 1
			}
			reasons := failedReasonsAfter(t, f.au, f.runRow.ID, tc.stage.failed, seq)
			if len(reasons) != wantN {
				t.Fatalf("%s count = %d (%v), want %d", tc.stage.failed, len(reasons), reasons, wantN)
			}
			want := orphanedReviewRedispatchFallbackReason(tc.slug)
			for _, r := range reasons {
				if r != want {
					t.Errorf("fallback reason = %q, want %q", r, want)
				}
			}
			if started, _ := startedAfter(t, f.au, f.runRow.ID, tc.stage.started, seq); len(started) != 0 {
				t.Errorf("newer %s rounds = %d, want 0", tc.stage.started, len(started))
			}
			if reviewRedispatchPending(f.runRow.ID, tc.stage.label, seq) {
				t.Error("the orphaned round is still pending after the fallback")
			}
		})
	}
}

// seedRedactedBundle stores b as the implement stage's redacted bundle and
// records its trace_uploaded.
func (f *redispatchFixture) seedRedactedBundle(t *testing.T, b []byte) {
	t.Helper()
	ts, ok := f.s.cfg.TraceStore.(*redispatchTraceStore)
	if !ok {
		t.Fatal("fixture has no redispatchTraceStore")
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	ts.put(tracestore.VariantRedacted, hash, b)
	f.appendAt(t, f.implStage.ID, beforeBoot, "trace_uploaded", map[string]any{"variant": string(tracestore.VariantRedacted), "content_hash": hash})
}

// TestReviewRedispatch_FallbackRecountsUnderLock is approval condition C2: a
// dispatcher that fails BEFORE its own *_review_started still appends a
// *_review_failed sequenced after the orphaned round (here the plan-parse
// branch of runPlanReviews, the same shape as document_injection_failed). The
// fallback re-counts under the stripe lock and synthesizes only the remainder,
// so the round settles at exactly ConfiguredAgents.
//
// COUNTERFACTUAL: synthesize from the handoff-time landed count instead of the
// recount → 3 plan_review_failed for a 2-agent round → RED.
func TestReviewRedispatch_FallbackRecountsUnderLock(t *testing.T) {
	f := newRedispatchFixture(t, redispatchFixtureOpts{})
	f.seedPlanArtifact(t, []byte(`{"plan_version":"not-a-plan"}`), "bad-plan", true)
	seq := f.appendAt(t, f.planStage.ID, beforeBoot, "plan_review_started", advisoryPlanStarted())

	bootSweep(t, f.s)

	reasons := failedReasonsAfter(t, f.au, f.runRow.ID, "plan_review_failed", seq)
	if len(reasons) != 2 {
		t.Fatalf("plan_review_failed after the orphaned round = %d (%v), want 2 (one from the dispatcher, one synthesized)", len(reasons), reasons)
	}
	fallback := 0
	for _, r := range reasons {
		if r == orphanedReviewRedispatchFallbackReason(redispatchSlugRoundNotStarted) {
			fallback++
		}
	}
	if fallback != 1 {
		t.Errorf("fallback-synthesized failures = %d, want 1 (the dispatcher's own failure already counts)", fallback)
	}
}

// TestReviewRedispatch_RedispatchAuditAppendFails: without the
// review_round_redispatched record a crash could re-dispatch the round on
// every boot, so an append failure closes the round failed instead.
func TestReviewRedispatch_RedispatchAuditAppendFails(t *testing.T) {
	f := newRedispatchFixture(t, redispatchFixtureOpts{})
	seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
	f.au.appendErrCategory = categoryReviewRoundRedispatched

	bootSweep(t, f.s)

	reasons := failedReasonsAfter(t, f.au, f.runRow.ID, "plan_review_failed", seq)
	if len(reasons) != 2 || reasons[0] != orphanedReviewNotRedispatchedReason(redispatchSlugRedispatchAuditFailed) {
		t.Fatalf("reasons = %v, want 2 x %q", reasons, orphanedReviewNotRedispatchedReason(redispatchSlugRedispatchAuditFailed))
	}
	if started, _ := startedAfter(t, f.au, f.runRow.ID, "plan_review_started", seq); len(started) != 0 {
		t.Errorf("a round was dispatched with no %s record", categoryReviewRoundRedispatched)
	}
}

// TestReviewRedispatch_EligibilityReadErrorClosesFailed: an undecidable round
// falls back to the #1781 closure (named eligibility_check_failed) instead of
// staying pending, and the predicate surfaces the read error to its caller
// (restart-blockers reports check_failed on it).
func TestReviewRedispatch_EligibilityReadErrorClosesFailed(t *testing.T) {
	f := newRedispatchFixture(t, redispatchFixtureOpts{})
	seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
	f.au.listByCategoryErrCategory = categoryReviewRoundRedispatched

	latest, payload, ok, err := f.s.latestReviewStarted(context.Background(), f.runRow.ID, orphanedReviewStages[0])
	if err != nil || !ok {
		t.Fatalf("latestReviewStarted: ok=%v err=%v", ok, err)
	}
	if _, _, eerr := f.s.redispatchEligibility(context.Background(), f.runRow.ID, orphanedReviewStages[0], latest, payload); eerr == nil {
		t.Fatal("redispatchEligibility swallowed the audit read error, want it returned")
	}

	bootSweep(t, f.s)

	reasons := failedReasonsAfter(t, f.au, f.runRow.ID, "plan_review_failed", seq)
	if len(reasons) != 2 || reasons[0] != orphanedReviewNotRedispatchedReason(redispatchSlugEligibilityCheckFailed) {
		t.Fatalf("reasons = %v, want 2 x eligibility_check_failed", reasons)
	}
}

// TestReviewRedispatch_EligiblePredicate pins the predicate's positive answer
// on each origin the eligible fixtures use, so the per-mode rows above are
// known to isolate their one control.
func TestReviewRedispatch_EligiblePredicate(t *testing.T) {
	cases := []struct {
		name  string
		opts  redispatchFixtureOpts
		stage orphanedReviewStageKind
		seed  func(t *testing.T, f *redispatchFixture) int64
	}{
		{name: "plan", stage: orphanedReviewStages[0], seed: func(t *testing.T, f *redispatchFixture) int64 {
			return f.seedEligiblePlanRound(t, advisoryPlanStarted())
		}},
		{name: "implement trace", stage: orphanedReviewStages[1], opts: redispatchFixtureOpts{traceStore: newRedispatchTraceStore()}, seed: func(t *testing.T, f *redispatchFixture) int64 {
			return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", "h1"))
		}},
		{name: "implement fixup_push", stage: orphanedReviewStages[1], opts: redispatchFixtureOpts{github: cannedComparePatchClient(t, cannedCompareOneFile)}, seed: func(t *testing.T, f *redispatchFixture) int64 {
			return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginFixupPush, "b1", "h1"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedispatchFixture(t, tc.opts)
			tc.seed(t, f)
			latest, payload, ok, err := f.s.latestReviewStarted(context.Background(), f.runRow.ID, tc.stage)
			if err != nil || !ok {
				t.Fatalf("latestReviewStarted: ok=%v err=%v", ok, err)
			}
			eligible, slug, err := f.s.redispatchEligibility(context.Background(), f.runRow.ID, tc.stage, latest, payload)
			if err != nil || !eligible {
				t.Fatalf("redispatchEligibility = (%v, %q, %v), want eligible", eligible, slug, err)
			}
		})
	}
}

// TestReviewRedispatch_GoroutineRechecksBeforeAndAfter covers the re-dispatch
// goroutine's two lock-held re-checks: a round superseded by a newer round, or
// already settled, since the handoff is not dispatched; and the fallback does
// not close a round once a newer round started.
func TestReviewRedispatch_GoroutineRechecksBeforeAndAfter(t *testing.T) {
	stage := orphanedReviewStages[0]
	ctx := context.Background()

	t.Run("newer round started", func(t *testing.T) {
		f := newRedispatchFixture(t, redispatchFixtureOpts{})
		seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
		f.appendAt(t, f.planStage.ID, afterBoot, "plan_review_started", advisoryPlanStarted())
		if f.s.orphanedRoundStillOpen(ctx, f.runRow.ID, stage, seq, 2) {
			t.Error("orphanedRoundStillOpen = true for a superseded round, want false")
		}
		f.s.closeUnstartedRedispatch(ctx, f.runRow.ID, stage, seq, advisoryPlanStarted(), redispatchSlugRoundNotStarted)
		if n := len(failedReasonsAfter(t, f.au, f.runRow.ID, stage.failed, seq)); n != 0 {
			t.Errorf("fallback closed a superseded round: %d failures, want 0", n)
		}
	})
	t.Run("round already settled", func(t *testing.T) {
		f := newRedispatchFixture(t, redispatchFixtureOpts{})
		seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
		f.appendAt(t, f.planStage.ID, afterBoot, "plan_reviewed", map[string]any{"verdict": "approve"})
		f.appendAt(t, f.planStage.ID, afterBoot, "plan_reviewed", map[string]any{"verdict": "approve"})
		if f.s.orphanedRoundStillOpen(ctx, f.runRow.ID, stage, seq, 2) {
			t.Error("orphanedRoundStillOpen = true for a settled round, want false")
		}
		f.s.runOrphanedRoundRedispatch(ctx, f.runRow.ID, stage, seq, advisoryPlanStarted())
		f.s.waitBackgroundReviews()
		if started, _ := startedAfter(t, f.au, f.runRow.ID, stage.started, seq); len(started) != 0 {
			t.Errorf("a settled round was re-dispatched: %d newer rounds", len(started))
		}
	})
	t.Run("open round", func(t *testing.T) {
		f := newRedispatchFixture(t, redispatchFixtureOpts{})
		seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
		if !f.s.orphanedRoundStillOpen(ctx, f.runRow.ID, stage, seq, 2) {
			t.Error("orphanedRoundStillOpen = false for an open round, want true")
		}
	})
}

// TestReviewRedispatchMarker_SetOnlyByBootRedispatch is approval condition C4's
// structural half (the behavioural half is TestTraceUpload_CannotCarryRedispatchMarker):
// withReviewRedispatch, the only way to set the marker that bypasses the #797
// same-head dedup, is called by exactly ONE production site — the boot
// re-dispatch goroutine. A handler, MCP tool or request-derived call site
// added anywhere in the package turns this RED.
func TestReviewRedispatchMarker_SetOnlyByBootRedispatch(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var sites []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "withReviewRedispatch" {
					sites = append(sites, name+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	want := "review_redispatch.go:runOrphanedRoundRedispatch"
	if len(sites) != 1 || sites[0] != want {
		t.Fatalf("withReviewRedispatch production call sites = %v, want exactly [%s] (the marker is server-internal: approval condition C4 of #4077)", sites, want)
	}
}
