package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- test-binary capability adapters ----------------------------------------

// noGuardedEdgeMarker is carried by a repository fixture that genuinely lacks the
// guarded edge, so the adapter below leaves it to the real assertion.
type noGuardedEdgeMarker interface{ noGuardedEdgeCapability() }

// init replaces the two capability vars once, before any test goroutine starts.
// They are SEAMS: every in-memory fixture reaches the ordinary and claimed edges
// through them. Postgres proves the atomicity they only imitate, and
// TestSupplementalStartAppender_OnlyTestBinaryAssigns and
// TestSupplementalGuardedEdge_OnlyTestBinaryAssigns fail on any production
// assignment. Only *auditFake and *crewRoutedAudit gain the start appender, so
// every other audit type takes the real assertion; acceptance_retirement_drop_test.go
// pins *auditFake's own fallback, which still does not carry the method.
func init() {
	realAppender := supplementalStartAppender
	supplementalStartAppender = func(repo audit.Repository) (audit.DedupedChainAppender, bool) {
		switch repo.(type) {
		case *auditFake, *crewRoutedAudit:
			return testDedupedAppender{repo}, true
		}
		return realAppender(repo)
	}
	realEdge := supplementalGuardedEdge
	supplementalGuardedEdge = func(s *Server) (run.GuardedStageTransitioner, bool) {
		if g, ok := realEdge(s); ok {
			return g, true
		}
		if s.cfg.RunRepo == nil {
			return nil, false
		}
		if _, absent := s.cfg.RunRepo.(noGuardedEdgeMarker); absent {
			return nil, false
		}
		return testGuardedEdge{s}, true
	}
}

// testStartAppendMu is the one mutex the start adapter runs its scan and append under.
var testStartAppendMu sync.Mutex

type testDedupedAppender struct{ repo audit.Repository }

func (a testDedupedAppender) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	testStartAppendMu.Lock()
	defer testStartAppendMu.Unlock()
	existing, err := a.repo.ListForRunByCategory(ctx, p.RunID, p.Category)
	if err != nil {
		return nil, err
	}
	if err := audit.CheckDedupeSpec(existing, &p, spec); err != nil {
		return nil, err
	}
	return a.repo.AppendChained(ctx, p)
}

// testGuardedEdgeLocks serializes the guarded adapter per stage, as the store's
// row lock does, so a fixture parked on one stage cannot stall a parallel test.
var (
	testGuardedEdgeLocksMu sync.Mutex
	testGuardedEdgeLocks   = map[uuid.UUID]*sync.Mutex{}
)

func testGuardedEdgeLock(id uuid.UUID) *sync.Mutex {
	testGuardedEdgeLocksMu.Lock()
	defer testGuardedEdgeLocksMu.Unlock()
	lock, ok := testGuardedEdgeLocks[id]
	if !ok {
		lock = &sync.Mutex{}
		testGuardedEdgeLocks[id] = lock
	}
	return lock
}

type testGuardedEdge struct{ s *Server }

func (e testGuardedEdge) TransitionStageFromAttemptGuarded(ctx context.Context, id uuid.UUID, from, to run.StageState, expected string, completion *run.StageCompletion, guard run.StageEdgeGuard) (*run.Stage, error) {
	if (expected == "" && !guard.LegacyIdentity) || len(guard.Categories) == 0 || guard.Decide == nil {
		return nil, run.ErrStageEdgeGuardInvalid
	}
	lock := testGuardedEdgeLock(id)
	lock.Lock()
	defer lock.Unlock()
	stage, err := e.s.cfg.RunRepo.GetStage(ctx, id)
	if err != nil {
		return nil, err
	}
	state, token := stage.State, run.StageAttemptToken(stage.DispatchedAt)
	if e.s.cfg.AuditRepo != nil {
		rows, err := e.s.listGuardRows(ctx, stage.RunID, guard.Categories)
		if err != nil {
			return nil, err
		}
		if err := guard.Decide(rows); err != nil {
			return nil, err
		}
	} else if err := guard.Decide(nil); err != nil {
		return nil, err
	}
	if state != from {
		return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: state}
	}
	if token != expected {
		return nil, run.StageAttemptChangedError{StageID: id, Expected: expected, Actual: token}
	}
	return e.s.cfg.RunRepo.TransitionStage(ctx, id, to, completion)
}

// --- rows -------------------------------------------------------------------

const (
	recoveryHead      = "1a2b3c4d5e6f70819aabbccddeeff00112233445"
	recoveryOtherHead = "ffffffffffffffffffffffffffffffffffffffff"
	recoveryAttempt   = "2026-11-05T12:00:00Z"
	recoveryOlder     = "2026-11-04T12:00:00Z"
	recoveryPath      = "backend/internal/foo/foo.go"
	recoveryPathB     = "backend/internal/foo/bar.go"
)

func recoveryPayload(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("setup: marshal payload: %v", err)
	}
	return b
}

// supplementalVerdict builds a gating supplemental verdict's payload.
func supplementalVerdict(head, attempt string, verdict planreview.Verdict) implementReviewedEvidence {
	return implementReviewedEvidence{
		ImplementReviewedPayload: planreview.ImplementReviewedPayload{
			ReviewerKind: "agent",
			Authority:    planreview.AuthorityGating,
			Verdict:      verdict,
			Origin:       planreview.OriginBaseRebaseReinvoke,
			HeadSHA:      head,
		},
		StageAttempt: attempt,
	}
}

func verdictRow(t *testing.T, stageID uuid.UUID, seq int64, head, attempt string, verdict planreview.Verdict) run.StageEdgeGuardRow {
	t.Helper()
	return evidenceRow(t, stageID, seq, supplementalVerdict(head, attempt, verdict))
}

func evidenceRow(t *testing.T, stageID uuid.UUID, seq int64, evidence implementReviewedEvidence) run.StageEdgeGuardRow {
	t.Helper()
	return run.StageEdgeGuardRow{Category: implementReviewedCategory, Sequence: seq, StageID: &stageID, Payload: recoveryPayload(t, evidence)}
}

func startRow(t *testing.T, stageID uuid.UUID, seq int64, head, attempt string, agents int, paths ...string) run.StageEdgeGuardRow {
	t.Helper()
	if paths == nil {
		paths = []string{}
	}
	return run.StageEdgeGuardRow{Category: CategorySupplementalReviewStarted, Sequence: seq, StageID: &stageID, Payload: recoveryPayload(t, supplementalReviewStartedPayload{
		StageAttempt: attempt, HeadSHA: head, ConfiguredAgents: agents, RequiredPaths: paths,
		Origin: planreview.OriginBaseRebaseReinvoke, PassKey: supplementalPassStartKey(attempt, head),
	})}
}

func requiredObligation(head, attempt string, paths ...string) *reviewObligation {
	set := make([]scopeExemption, 0, len(paths))
	for _, p := range paths {
		set = append(set, scopeExemption{Path: p, Reason: "unchanged"})
	}
	return &reviewObligation{HeadSHA: head, StageAttempt: attempt, SupplementalScopeExemptions: set}
}

func checkpointRow(t *testing.T, stageID uuid.UUID, seq int64, head, attempt string, paths ...string) run.StageEdgeGuardRow {
	t.Helper()
	return run.StageEdgeGuardRow{Category: CategoryReviewObligationCheckpoint, Sequence: seq, StageID: &stageID, Payload: recoveryPayload(t, map[string]any{
		"push_checkpoint": map[string]any{"review_obligation": requiredObligation(head, attempt, paths...)},
	})}
}

func parkedRow(t *testing.T, stageID uuid.UUID, seq int64, head, attempt string, paths ...string) run.StageEdgeGuardRow {
	t.Helper()
	return run.StageEdgeGuardRow{Category: CategoryScopeCompletenessParked, Sequence: seq, StageID: &stageID, Payload: recoveryPayload(t, map[string]any{
		"review_obligation": requiredObligation(head, attempt, paths...),
	})}
}

func rawRow(category string, seq int64, stageID uuid.UUID, payload string) run.StageEdgeGuardRow {
	return run.StageEdgeGuardRow{Category: category, Sequence: seq, StageID: &stageID, Payload: []byte(payload)}
}

// recoverySeam is one running implement stage over fake stores.
type recoverySeam struct {
	s       *Server
	au      *auditFake
	rr      *orchestratorRepo
	runRow  *run.Run
	stage   *run.Stage
	attempt string
}

func newRecoverySeam(t *testing.T) *recoverySeam {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()
	au.stampSequence = true
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStateRunning)
	stage.Type = run.StageTypeImplement
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})
	return &recoverySeam{s: s, au: au, rr: rr, runRow: runRow, stage: stage, attempt: run.StageAttemptToken(stage.DispatchedAt)}
}

func (f *recoverySeam) append(t *testing.T, category string, payload []byte) {
	t.Helper()
	stageID := f.stage.ID
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: f.runRow.ID, StageID: &stageID, Category: category, Payload: payload}); err != nil {
		t.Fatalf("setup: append %s: %v", category, err)
	}
}

func (f *recoverySeam) carrier(t *testing.T, head string, paths ...string) {
	t.Helper()
	f.s.recordReviewObligationCheckpoint(context.Background(), f.runRow.ID, f.stage.ID, resumeKindPROpen,
		pushCheckpointPayload{Branch: "b", HeadSHA: head, BaseSHA: "base", VerifiedTreeSHA: "tree"},
		requiredObligation(head, f.attempt, paths...), audit.ActorKind("system"), nil)
}

func (f *recoverySeam) start(t *testing.T, head, attempt string, agents int, paths ...string) {
	t.Helper()
	got, err := f.s.allocateSupplementalPass(context.Background(), f.runRow.ID, f.stage.ID, head, attempt, agents, paths)
	if err != nil || !got.won {
		t.Fatalf("setup: allocate start: %+v, %v", got, err)
	}
}

func (f *recoverySeam) verdict(t *testing.T, head, attempt string, verdict planreview.Verdict) {
	t.Helper()
	f.append(t, implementReviewedCategory, recoveryPayload(t, supplementalVerdict(head, attempt, verdict)))
}

func (f *recoverySeam) opened(t *testing.T, head string) {
	t.Helper()
	f.append(t, "pull_request_opened", recoveryPayload(t, map[string]any{"head_sha": head}))
}

// seedGuardRows seeds rows as already-committed entries.
func seedGuardRows(au *auditFake, runID uuid.UUID, rows []run.StageEdgeGuardRow, at time.Time) {
	for _, row := range rows {
		rid := runID
		au.seeded = append(au.seeded, &audit.Entry{RunID: &rid, StageID: row.StageID, Category: row.Category, Sequence: row.Sequence, Timestamp: at, Payload: row.Payload})
	}
}

// --- obligation state --------------------------------------------------------

func TestReviewObligationKnowledge_ThreeStates(t *testing.T) {
	if got := reviewObligationKnowledge(nil); got != obligationUnknown {
		t.Errorf("nil_pointer = %d, want unknown", got)
	}
	cases := []struct {
		name string
		wire string
		want reviewObligationState
	}{
		{"list_absent", `{"head_sha":"h","stage_attempt":"a"}`, obligationUnknown},
		{"list_null", `{"head_sha":"h","stage_attempt":"a","supplemental_scope_exemptions":null}`, obligationUnknown},
		{"list_empty", `{"head_sha":"h","stage_attempt":"a","supplemental_scope_exemptions":[]}`, obligationKnownNone},
		{"list_present", `{"head_sha":"h","stage_attempt":"a","supplemental_scope_exemptions":[{"path":"p","reason":"r"}]}`, obligationKnownRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var o reviewObligation
			if err := json.Unmarshal([]byte(tc.wire), &o); err != nil {
				t.Fatalf("setup: decode: %v", err)
			}
			if got := reviewObligationKnowledge(&o); got != tc.want {
				t.Fatalf("state = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("null_pointer_stays_unknown", func(t *testing.T) {
		var carrier struct {
			ReviewObligation *reviewObligation `json:"review_obligation"`
		}
		if err := json.Unmarshal([]byte(`{"review_obligation":null}`), &carrier); err != nil {
			t.Fatalf("setup: decode: %v", err)
		}
		if got := reviewObligationKnowledge(carrier.ReviewObligation); got != obligationUnknown {
			t.Fatalf("state = %d, want unknown", got)
		}
	})
}

func TestValidateReviewObligationReport_Modes(t *testing.T) {
	dispatched := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	stage := &run.Stage{ID: uuid.New(), DispatchedAt: &dispatched}
	cases := []struct {
		name    string
		report  *reviewObligation
		kind    string
		wantErr bool
	}{
		{name: "ok", report: requiredObligation(recoveryHead, recoveryAttempt, recoveryPath), kind: resumeKindPROpen},
		{name: "head_mismatch", report: requiredObligation(recoveryOtherHead, recoveryAttempt, recoveryPath), kind: resumeKindPROpen, wantErr: true},
		{name: "stale_attempt", report: requiredObligation(recoveryHead, recoveryOlder, recoveryPath), kind: resumeKindPROpen, wantErr: true},
		{name: "empty_path", report: requiredObligation(recoveryHead, recoveryAttempt, ""), kind: resumeKindPush, wantErr: true},
		{name: "reverify_nonempty", report: requiredObligation(recoveryHead, recoveryAttempt, recoveryPath), kind: resumeKindReverify, wantErr: true},
		{name: "reverify_empty", report: requiredObligation(recoveryHead, recoveryAttempt), kind: resumeKindReverify},
		{name: "nil_report", kind: resumeKindPROpen, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReviewObligationReport(tc.report, tc.kind, recoveryHead, stage)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestRecordReviewObligationCheckpoint_Modes(t *testing.T) {
	ctx := context.Background()
	coords := pushCheckpointPayload{Branch: "b", HeadSHA: recoveryHead, BaseSHA: "base", VerifiedTreeSHA: "tree", PRTitle: "t", PRBody: "body"}
	rows := func(t *testing.T, f *recoverySeam) []*audit.Entry {
		t.Helper()
		entries, err := f.au.ListForRunByCategory(ctx, f.runRow.ID, CategoryReviewObligationCheckpoint)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return entries
	}
	t.Run("known_required_writes_carrier", func(t *testing.T) {
		f := newRecoverySeam(t)
		subject := "sub"
		f.s.recordReviewObligationCheckpoint(ctx, f.runRow.ID, f.stage.ID, resumeKindPush, coords, requiredObligation(recoveryHead, f.attempt, recoveryPath), audit.ActorKind("system"), &subject)
		got := rows(t, f)
		if len(got) != 1 {
			t.Fatalf("rows = %d", len(got))
		}
		var payload struct {
			ResumeKind     string `json:"resume_kind"`
			PushCheckpoint struct {
				Branch           string            `json:"branch"`
				HeadSHA          string            `json:"head_sha"`
				BaseSHA          string            `json:"base_sha"`
				VerifiedTreeSHA  string            `json:"verified_tree_sha"`
				PRTitle          string            `json:"pr_title"`
				PRBody           string            `json:"pr_body"`
				ReviewObligation *reviewObligation `json:"review_obligation"`
			} `json:"push_checkpoint"`
		}
		if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		cp := payload.PushCheckpoint
		if payload.ResumeKind != resumeKindPush || cp.Branch != "b" || cp.HeadSHA != recoveryHead || cp.BaseSHA != "base" ||
			cp.VerifiedTreeSHA != "tree" || cp.PRTitle != "t" || cp.PRBody != "body" {
			t.Errorf("payload = %+v", payload)
		}
		if reviewObligationKnowledge(cp.ReviewObligation) != obligationKnownRequired {
			t.Errorf("obligation = %+v", cp.ReviewObligation)
		}
	})
	t.Run("text_keys_gated_on_non_empty", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.carrier(t, recoveryHead, recoveryPath)
		got := rows(t, f)
		if len(got) != 1 {
			t.Fatalf("rows = %d", len(got))
		}
		if bytes.Contains(got[0].Payload, []byte("pr_title")) || bytes.Contains(got[0].Payload, []byte("pr_body")) {
			t.Errorf("payload carries empty text keys: %s", got[0].Payload)
		}
	})
	for name, report := range map[string]*reviewObligation{
		"nil_report":     nil,
		"unknown_report": {HeadSHA: recoveryHead},
		"known_none":     {HeadSHA: recoveryHead, SupplementalScopeExemptions: []scopeExemption{}},
	} {
		t.Run(name+"_writes_nothing", func(t *testing.T) {
			f := newRecoverySeam(t)
			f.s.recordReviewObligationCheckpoint(ctx, f.runRow.ID, f.stage.ID, resumeKindPROpen, coords, report, audit.ActorKind("system"), nil)
			if got := rows(t, f); len(got) != 0 {
				t.Fatalf("rows = %d, want none", len(got))
			}
		})
	}
	t.Run("half_populated_coordinates_write_nothing", func(t *testing.T) {
		f := newRecoverySeam(t)
		report := requiredObligation(recoveryHead, f.attempt, recoveryPath)
		f.s.recordReviewObligationCheckpoint(ctx, f.runRow.ID, f.stage.ID, resumeKindPROpen, pushCheckpointPayload{HeadSHA: recoveryHead}, report, audit.ActorKind("system"), nil)
		f.s.recordReviewObligationCheckpoint(ctx, f.runRow.ID, f.stage.ID, resumeKindPROpen, pushCheckpointPayload{Branch: "b"}, report, audit.ActorKind("system"), nil)
		if got := rows(t, f); len(got) != 0 {
			t.Fatalf("rows = %d, want none", len(got))
		}
	})
	t.Run("append_error_is_best_effort", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.au.appendErrCategory = CategoryReviewObligationCheckpoint
		f.carrier(t, recoveryHead, recoveryPath)
		if got := rows(t, f); len(got) != 0 {
			t.Fatalf("rows = %d, want none", len(got))
		}
	})
}

// --- verdict wire ------------------------------------------------------------

// TestImplementReviewedEvidence_WireParity is the unit half of write and read
// parity; TestSupplementalVerdict_CarriesCapturedAttempt drives a real pass.
func TestImplementReviewedEvidence_WireParity(t *testing.T) {
	plain := planreview.ImplementReviewedPayload{
		ReviewerKind: "agent", Authority: planreview.AuthorityGating, Verdict: planreview.VerdictApprove,
		Origin: planreview.OriginBaseRebaseReinvoke, HeadSHA: recoveryHead,
	}
	plainBytes := recoveryPayload(t, plain)
	t.Run("empty_attempt_byte_identical", func(t *testing.T) {
		got := recoveryPayload(t, implementReviewedEvidence{ImplementReviewedPayload: plain})
		if !bytes.Equal(got, plainBytes) {
			t.Fatalf("empty attempt changed the bytes:\n got %s\nwant %s", got, plainBytes)
		}
	})
	marked := recoveryPayload(t, implementReviewedEvidence{ImplementReviewedPayload: plain, StageAttempt: recoveryAttempt})
	t.Run("existing_keys_unchanged", func(t *testing.T) {
		var got, want map[string]json.RawMessage
		if err := json.Unmarshal(marked, &got); err != nil {
			t.Fatalf("decode marked: %v", err)
		}
		if err := json.Unmarshal(plainBytes, &want); err != nil {
			t.Fatalf("decode plain: %v", err)
		}
		if string(got["stage_attempt"]) != `"`+recoveryAttempt+`"` {
			t.Errorf("stage_attempt = %s", got["stage_attempt"])
		}
		delete(got, "stage_attempt")
		if len(got) != len(want) {
			t.Fatalf("keys differ: %v vs %v", got, want)
		}
		for key, value := range want {
			if !bytes.Equal(got[key], value) {
				t.Errorf("key %s = %s, want %s", key, got[key], value)
			}
		}
	})
	t.Run("old_reader_ignores_attempt", func(t *testing.T) {
		var old planreview.ImplementReviewedPayload
		if err := json.Unmarshal(marked, &old); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if old.Verdict != planreview.VerdictApprove || old.HeadSHA != recoveryHead || old.Origin != planreview.OriginBaseRebaseReinvoke {
			t.Errorf("old reader = %+v", old)
		}
	})
	t.Run("decoded_by_prior_dedup", func(t *testing.T) {
		stageID := uuid.New()
		entries := []*audit.Entry{{StageID: &stageID, Payload: marked}}
		if !supplementalReinvokeReviewAlreadyRecorded(entries, stageID, recoveryHead) {
			t.Error("the advisory dedup must still match a marked row")
		}
	})
	t.Run("new_attempt_decode_count_parity", func(t *testing.T) {
		stageID := uuid.New()
		state, err := supplementalRoundFromRows([]run.StageEdgeGuardRow{rawRow(implementReviewedCategory, 1, stageID, string(marked))}, stageID, recoveryHead, recoveryAttempt)
		if err != nil || state.currentVerdicts != 1 {
			t.Fatalf("state = %+v, err %v", state, err)
		}
	})
}

// --- round state -------------------------------------------------------------

// TestSupplementalRoundState_Modes is Z12.
func TestSupplementalRoundState_Modes(t *testing.T) {
	stageID, otherStage := uuid.New(), uuid.New()
	otherOrigin := supplementalVerdict(recoveryHead, recoveryAttempt, planreview.VerdictApprove)
	otherOrigin.Origin = ""
	advisoryReject := supplementalVerdict(recoveryHead, recoveryOlder, planreview.VerdictReject)
	advisoryReject.Authority = planreview.AuthorityAdvisory
	badKey := startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 1)
	badKey.Payload = recoveryPayload(t, supplementalReviewStartedPayload{StageAttempt: recoveryAttempt, HeadSHA: recoveryHead, ConfiguredAgents: 1, PassKey: "wrong"})
	approve := func(seq int64, attempt string) run.StageEdgeGuardRow {
		return verdictRow(t, stageID, seq, recoveryHead, attempt, planreview.VerdictApprove)
	}
	cases := []struct {
		name      string
		attempt   string
		rows      []run.StageEdgeGuardRow
		listFault string
		want      supplementalRoundSnapshot
		wantErr   bool
	}{
		{name: "no_rows"},
		{name: "partial_current", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 2), approve(2, recoveryAttempt)},
			want: supplementalRoundSnapshot{currentVerdicts: 1, startedThisAttempt: true, startedAgents: 2}},
		{name: "complete_current", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 2), approve(2, recoveryAttempt), approve(3, recoveryAttempt)},
			want: supplementalRoundSnapshot{currentVerdicts: 2, startedThisAttempt: true, startedAgents: 2}},
		{name: "complete_prior_attempt_not_settled", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryOlder, 1), approve(2, recoveryOlder)}},
		{name: "partial_prior_attempt_not_settled", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryOlder, 2), approve(2, recoveryOlder)}},
		// Every seeded row's timestamp is after this attempt's dispatch, which a
		// timestamp rule would count; only the recorded token keeps it out.
		{name: "late_old_attempt_row_not_counted", rows: []run.StageEdgeGuardRow{approve(5, recoveryOlder)}},
		{name: "legacy_row_no_attempt_not_settled", rows: []run.StageEdgeGuardRow{approve(1, "")}},
		{name: "legacy_reject_binds", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryHead, "", planreview.VerdictReject)},
			want: supplementalRoundSnapshot{rejected: true}},
		{name: "reject_prior_attempt", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryHead, recoveryOlder, planreview.VerdictReject)},
			want: supplementalRoundSnapshot{rejected: true}},
		{name: "reject_current", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryHead, recoveryAttempt, planreview.VerdictReject)},
			want: supplementalRoundSnapshot{rejected: true, currentVerdicts: 1}},
		{name: "advisory_reject_not_rejected", rows: []run.StageEdgeGuardRow{evidenceRow(t, stageID, 1, advisoryReject)}},
		{name: "other_head", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryOtherHead, recoveryAttempt, planreview.VerdictReject)}},
		{name: "other_stage", rows: []run.StageEdgeGuardRow{verdictRow(t, otherStage, 1, recoveryHead, recoveryAttempt, planreview.VerdictReject), startRow(t, otherStage, 2, recoveryHead, recoveryAttempt, 1)}},
		{name: "other_origin", rows: []run.StageEdgeGuardRow{evidenceRow(t, stageID, 1, otherOrigin)}},
		{name: "started_this_attempt", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 3, recoveryPath)},
			want: supplementalRoundSnapshot{startedThisAttempt: true, startedAgents: 3}},
		{name: "started_other_attempt_ignored", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryOlder, 1)}},
		{name: "started_other_head_ignored", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryOtherHead, recoveryAttempt, 1)}},
		{name: "verdict_list_error", listFault: implementReviewedCategory, rows: []run.StageEdgeGuardRow{approve(1, recoveryAttempt)}, wantErr: true},
		{name: "started_list_error", listFault: CategorySupplementalReviewStarted, wantErr: true},
		{name: "verdict_row_undecodable", rows: []run.StageEdgeGuardRow{rawRow(implementReviewedCategory, 1, stageID, `{"verdict":7}`)}, wantErr: true},
		{name: "started_row_undecodable", rows: []run.StageEdgeGuardRow{rawRow(CategorySupplementalReviewStarted, 1, stageID, `{"configured_agents":"x"}`)}, wantErr: true},
		{name: "started_row_key_mismatch", rows: []run.StageEdgeGuardRow{badKey}, wantErr: true},
		{name: "started_row_agents_zero", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 0)}, wantErr: true},
		{name: "legacy_nil_dispatched_at", attempt: "-", rows: []run.StageEdgeGuardRow{approve(1, "")}, want: supplementalRoundSnapshot{currentVerdicts: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			au := newAuditFake()
			au.listByCategoryErrCategory = tc.listFault
			runID := uuid.New()
			dispatched, err := time.Parse(time.RFC3339Nano, recoveryAttempt)
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			seedGuardRows(au, runID, tc.rows, dispatched.Add(time.Hour))
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
			attempt := recoveryAttempt
			if tc.attempt == "-" {
				attempt = ""
			}
			state, err := s.supplementalRoundState(context.Background(), runID, stageID, recoveryHead, attempt)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if state != (supplementalRoundSnapshot{}) {
					t.Fatalf("an error must carry the empty state, got %+v", state)
				}
				return
			}
			if state != tc.want {
				t.Fatalf("state = %+v, want %+v", state, tc.want)
			}
		})
	}
}

// --- required obligation -----------------------------------------------------

// TestRequiredReviewObligationMissing_Modes is Z1. Carriers are written by the real
// recorder and starts by the real allocator.
func TestRequiredReviewObligationMissing_Modes(t *testing.T) {
	cases := []struct {
		name           string
		seed           func(t *testing.T, f *recoverySeam)
		body           []scopeExemption
		wantMissing    []string
		wantDischarged bool
		wantIncomplete bool
		wantErr        bool
	}{
		{name: "body_covers", seed: func(t *testing.T, f *recoverySeam) { f.carrier(t, recoveryHead, recoveryPath) }, body: []scopeExemption{{Path: recoveryPath}}},
		{name: "body_missing_path", seed: func(t *testing.T, f *recoverySeam) { f.carrier(t, recoveryHead, recoveryPath, recoveryPathB) },
			body: []scopeExemption{{Path: recoveryPath}}, wantMissing: []string{recoveryPathB}},
		{name: "different_head_not_reused", seed: func(t *testing.T, f *recoverySeam) { f.carrier(t, recoveryOtherHead, recoveryPath) }},
		{name: "opened_at_head_not_discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.carrier(t, recoveryHead, recoveryPath)
			f.opened(t, recoveryHead)
		}, wantMissing: []string{recoveryPath}},
		{name: "rearmed_after_opened_still_required", seed: func(t *testing.T, f *recoverySeam) {
			f.carrier(t, recoveryHead, recoveryPath)
			f.opened(t, recoveryHead)
			f.carrier(t, recoveryHead, recoveryPath, recoveryPathB)
		}, wantMissing: []string{recoveryPath, recoveryPathB}},
		{name: "parked_required", seed: func(t *testing.T, f *recoverySeam) {
			f.append(t, CategoryScopeCompletenessParked, recoveryPayload(t, map[string]any{"review_obligation": requiredObligation(recoveryHead, f.attempt, recoveryPath)}))
		}, wantMissing: []string{recoveryPath}},
		{name: "known_none_ignored", seed: func(t *testing.T, f *recoverySeam) {
			f.append(t, CategoryScopeCompletenessParked, recoveryPayload(t, map[string]any{"review_obligation": requiredObligation(recoveryHead, f.attempt)}))
			f.append(t, CategoryScopeCompletenessParked, []byte(`{"head_sha":"`+recoveryHead+`"}`))
		}},
		{name: "known_none_does_not_retire_required", seed: func(t *testing.T, f *recoverySeam) {
			f.carrier(t, recoveryHead, recoveryPath)
			f.append(t, CategoryScopeCompletenessParked, recoveryPayload(t, map[string]any{"review_obligation": requiredObligation(recoveryHead, f.attempt)}))
		}, wantMissing: []string{recoveryPath}},
		{name: "list_error", seed: func(t *testing.T, f *recoverySeam) {
			f.au.listByCategoryErrCategory = CategoryReviewObligationCheckpoint
		}, wantErr: true},
		{name: "newer_artifact_not_discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.carrier(t, recoveryHead, recoveryPath)
			art := newFakeArtifactRepo()
			seedBudgetPlanArtifact(t, art, f.stage.ID, &plan.Plan{PlanVersion: "standard_v1", Summary: "newer"})
			f.s.cfg.ArtifactRepo = art
		}, wantMissing: []string{recoveryPath}},
		{name: "started_row_no_carrier_required", seed: func(t *testing.T, f *recoverySeam) { f.start(t, recoveryHead, f.attempt, 2, recoveryPath) },
			wantMissing: []string{recoveryPath}, wantIncomplete: true},
		{name: "current_complete_discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.start(t, recoveryHead, f.attempt, 2, recoveryPath)
			f.verdict(t, recoveryHead, f.attempt, planreview.VerdictApprove)
			f.verdict(t, recoveryHead, f.attempt, planreview.VerdictApprove)
		}, wantMissing: []string{recoveryPath}, wantDischarged: true},
		{name: "current_partial_not_discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.start(t, recoveryHead, f.attempt, 2, recoveryPath)
			f.verdict(t, recoveryHead, f.attempt, planreview.VerdictApprove)
		}, wantMissing: []string{recoveryPath}, wantIncomplete: true},
		{name: "current_reject_not_discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.start(t, recoveryHead, f.attempt, 2, recoveryPath)
			f.verdict(t, recoveryHead, f.attempt, planreview.VerdictApprove)
			f.verdict(t, recoveryHead, f.attempt, planreview.VerdictReject)
		}, wantMissing: []string{recoveryPath}, wantIncomplete: true},
		{name: "prior_attempt_complete_not_discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.start(t, recoveryHead, recoveryOlder, 1, recoveryPath)
			f.verdict(t, recoveryHead, recoveryOlder, planreview.VerdictApprove)
		}, wantMissing: []string{recoveryPath}},
		{name: "verdicts_without_start_not_discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.carrier(t, recoveryHead, recoveryPath)
			f.verdict(t, recoveryHead, f.attempt, planreview.VerdictApprove)
		}, wantMissing: []string{recoveryPath}},
		{name: "round_state_error", seed: func(t *testing.T, f *recoverySeam) {
			f.carrier(t, recoveryHead, recoveryPath)
			f.append(t, implementReviewedCategory, []byte(`{"verdict":7}`))
		}, wantErr: true},
		{name: "carrier_row_undecodable", seed: func(t *testing.T, f *recoverySeam) {
			f.append(t, CategoryReviewObligationCheckpoint, []byte(`{"push_checkpoint":7}`))
		}, wantErr: true},
		{name: "parked_row_undecodable", seed: func(t *testing.T, f *recoverySeam) {
			f.append(t, CategoryScopeCompletenessParked, []byte(`{"review_obligation":7}`))
		}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoverySeam(t)
			tc.seed(t, f)
			check, err := f.s.requiredReviewObligationMissing(context.Background(), f.runRow.ID, f.stage, recoveryHead, tc.body)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !slices.Equal(check.missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", check.missing, tc.wantMissing)
			}
			if check.discharged != tc.wantDischarged {
				t.Errorf("discharged = %v, want %v", check.discharged, tc.wantDischarged)
			}
			if check.startedIncomplete != tc.wantIncomplete {
				t.Errorf("startedIncomplete = %v, want %v", check.startedIncomplete, tc.wantIncomplete)
			}
		})
	}
}

// failingGetRunRepo and failingListArtifactRepo are read faults, kept here rather
// than borrowing review_shutdown_test.go's, whose file is unix-only.
type failingGetRunRepo struct{ run.Repository }

func (failingGetRunRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("run read fault")
}

type failingListArtifactRepo struct{ artifact.Repository }

func (failingListArtifactRepo) ListForStage(context.Context, uuid.UUID) ([]*artifact.Artifact, error) {
	return nil, errors.New("artifact read fault")
}

func TestSupplementalPassLost_Modes(t *testing.T) {
	gating, advisory := specImplementGatingReviewers, specImplementAdvisoryReviewers
	body := &pullRequestBody{HeadSHA: recoveryHead, SupplementalScopeExemptions: []scopeExemption{{Path: recoveryPath, Reason: "r"}}}
	start := func(t *testing.T, stageID uuid.UUID, seq int64, attempt string) run.StageEdgeGuardRow {
		t.Helper()
		return startRow(t, stageID, seq, recoveryHead, attempt, 1, recoveryPath)
	}
	cases := []struct {
		name string
		spec []byte
		body *pullRequestBody
		edit func(t *testing.T, s *Server, au *auditFake, stage *run.Stage, attempt string) []run.StageEdgeGuardRow
		want bool
	}{
		{name: "no_exemptions", spec: gating, body: &pullRequestBody{HeadSHA: recoveryHead}},
		{name: "not_running", spec: gating, edit: func(_ *testing.T, _ *Server, _ *auditFake, stage *run.Stage, _ string) []run.StageEdgeGuardRow {
			stage.State = run.StageStateAwaitingApproval
			return nil
		}},
		{name: "not_implement", spec: gating, edit: func(_ *testing.T, _ *Server, _ *auditFake, stage *run.Stage, _ string) []run.StageEdgeGuardRow {
			stage.Type = run.StageTypePlan
			return nil
		}},
		{name: "no_approved_plan_not_lost", spec: gating, edit: func(_ *testing.T, s *Server, _ *auditFake, _ *run.Stage, _ string) []run.StageEdgeGuardRow {
			s.cfg.ArtifactRepo = newFakeArtifactRepo()
			return nil
		}},
		{name: "no_reviewers_not_lost", spec: specNoReviewers},
		{name: "no_backend_not_lost", spec: gating, edit: func(_ *testing.T, s *Server, _ *auditFake, _ *run.Stage, _ string) []run.StageEdgeGuardRow {
			s.cfg.PlanReviewers = nil
			return nil
		}},
		{name: "advisory_not_lost", spec: advisory},
		{name: "run_read_error_lost", spec: gating, want: true, edit: func(_ *testing.T, s *Server, _ *auditFake, _ *run.Stage, _ string) []run.StageEdgeGuardRow {
			s.cfg.RunRepo = failingGetRunRepo{s.cfg.RunRepo}
			return nil
		}},
		{name: "plan_read_error_lost", spec: gating, want: true, edit: func(_ *testing.T, s *Server, _ *auditFake, _ *run.Stage, _ string) []run.StageEdgeGuardRow {
			s.cfg.ArtifactRepo = failingListArtifactRepo{s.cfg.ArtifactRepo}
			return nil
		}},
		{name: "already_settled_current_attempt_not_lost", spec: gating, edit: func(t *testing.T, _ *Server, _ *auditFake, stage *run.Stage, attempt string) []run.StageEdgeGuardRow {
			return []run.StageEdgeGuardRow{start(t, stage.ID, 1, attempt), verdictRow(t, stage.ID, 2, recoveryHead, attempt, planreview.VerdictApprove)}
		}},
		{name: "partial_current_lost", spec: gating, want: true, edit: func(t *testing.T, _ *Server, _ *auditFake, stage *run.Stage, attempt string) []run.StageEdgeGuardRow {
			return []run.StageEdgeGuardRow{startRow(t, stage.ID, 1, recoveryHead, attempt, 2, recoveryPath), verdictRow(t, stage.ID, 2, recoveryHead, attempt, planreview.VerdictApprove)}
		}},
		{name: "complete_prior_attempt_lost", spec: gating, want: true, edit: func(t *testing.T, _ *Server, _ *auditFake, stage *run.Stage, _ string) []run.StageEdgeGuardRow {
			return []run.StageEdgeGuardRow{start(t, stage.ID, 1, recoveryOlder), verdictRow(t, stage.ID, 2, recoveryHead, recoveryOlder, planreview.VerdictApprove)}
		}},
		{name: "late_earlier_attempt_approval_lost", spec: gating, want: true, edit: func(t *testing.T, _ *Server, _ *auditFake, stage *run.Stage, attempt string) []run.StageEdgeGuardRow {
			return []run.StageEdgeGuardRow{start(t, stage.ID, 1, attempt), verdictRow(t, stage.ID, 9, recoveryHead, recoveryOlder, planreview.VerdictApprove)}
		}},
		{name: "round_state_read_error_lost", spec: gating, want: true, edit: func(_ *testing.T, _ *Server, au *auditFake, _ *run.Stage, _ string) []run.StageEdgeGuardRow {
			au.listByCategoryErrCategory = implementReviewedCategory
			return nil
		}},
		{name: "reject_any_attempt_lost", spec: gating, want: true, edit: func(t *testing.T, _ *Server, _ *auditFake, stage *run.Stage, attempt string) []run.StageEdgeGuardRow {
			return []run.StageEdgeGuardRow{
				start(t, stage.ID, 1, attempt),
				verdictRow(t, stage.ID, 2, recoveryHead, attempt, planreview.VerdictApprove),
				verdictRow(t, stage.ID, 3, recoveryHead, recoveryOlder, planreview.VerdictReject),
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, au, _, runRow, stage := newImplementReviewServer(t, &fakePlanReviewer{}, tc.spec)
			stage.State = run.StageStateRunning
			if tc.edit != nil {
				seedGuardRows(au, runRow.ID, tc.edit(t, s, au, stage, run.StageAttemptToken(stage.DispatchedAt)), time.Now())
			}
			upload := tc.body
			if upload == nil {
				upload = body
			}
			if got := s.supplementalPassLost(context.Background(), runRow.ID, stage, upload); got != tc.want {
				t.Fatalf("lost = %v, want %v", got, tc.want)
			}
		})
	}
	// A zero round state already reads as lost, so only the log separates the round
	// read's own branch; the fault must not pass silently.
	t.Run("round_state_read_error_warns", func(t *testing.T) {
		s, _, au, _, runRow, stage := newImplementReviewServer(t, &fakePlanReviewer{}, gating)
		stage.State = run.StageStateRunning
		au.listByCategoryErrCategory = implementReviewedCategory
		var logs bytes.Buffer
		s.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		if !s.supplementalPassLost(context.Background(), runRow.ID, stage, body) {
			t.Fatal("a round read fault must read as lost")
		}
		if !strings.Contains(logs.String(), "treating the pass as lost") {
			t.Fatalf("the fault passed silently: %q", logs.String())
		}
	})
}

// --- allocation --------------------------------------------------------------

// plainAuditOnly embeds the interface, so it carries no atomic start and no adapter
// applies to it.
type plainAuditOnly struct{ audit.Repository }

// survivingStartAudit answers every allocation with a scripted survivor.
type survivingStartAudit struct {
	audit.Repository
	survivor *audit.Entry
}

func (a survivingStartAudit) AppendChainedDeduped(context.Context, audit.ChainAppendParams, audit.DedupeSpec) (*audit.Entry, error) {
	return nil, &audit.DedupedDuplicateError{Existing: a.survivor}
}

func TestAllocateSupplementalPass_Modes(t *testing.T) {
	ctx := context.Background()
	rows := func(t *testing.T, f *recoverySeam) []*audit.Entry {
		t.Helper()
		entries, err := f.au.ListForRunByCategory(ctx, f.runRow.ID, CategorySupplementalReviewStarted)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return entries
	}
	allocate := func(f *recoverySeam, stageID uuid.UUID, head, attempt string) (supplementalPassAllocation, error) {
		return f.s.allocateSupplementalPass(ctx, f.runRow.ID, stageID, head, attempt, 2, []string{recoveryPath})
	}
	t.Run("first_allocates", func(t *testing.T) {
		f := newRecoverySeam(t)
		got, err := allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
		if err != nil || !got.won {
			t.Fatalf("allocation = %+v, err %v", got, err)
		}
		entries := rows(t, f)
		if len(entries) != 1 {
			t.Fatalf("rows = %d", len(entries))
		}
		if entries[0].StageID == nil || *entries[0].StageID != f.stage.ID {
			t.Errorf("row stage = %v", entries[0].StageID)
		}
		var payload supplementalReviewStartedPayload
		if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := supplementalReviewStartedPayload{
			RunID: f.runRow.ID.String(), StageID: f.stage.ID.String(), StageAttempt: recoveryAttempt, HeadSHA: recoveryHead,
			ConfiguredAgents: 2, RequiredPaths: []string{recoveryPath}, Origin: planreview.OriginBaseRebaseReinvoke,
			PassKey: supplementalPassStartKey(recoveryAttempt, recoveryHead),
		}
		if !reflect.DeepEqual(payload, want) {
			t.Errorf("payload = %+v, want %+v", payload, want)
		}
	})
	t.Run("duplicate_returns_survivor", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.append(t, "pull_request_opened", []byte(`{}`)) // the survivor's sequence is not 1
		if got, err := allocate(f, f.stage.ID, recoveryHead, recoveryAttempt); err != nil || !got.won {
			t.Fatalf("first = %+v, %v", got, err)
		}
		got, err := allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
		if err != nil || got.won {
			t.Fatalf("duplicate = %+v, err %v", got, err)
		}
		entries := rows(t, f)
		if len(entries) != 1 {
			t.Fatalf("rows = %d", len(entries))
		}
		if got.sequence != entries[0].Sequence || got.sequence == 0 {
			t.Errorf("sequence = %d, want survivor's %d", got.sequence, entries[0].Sequence)
		}
	})
	t.Run("other_attempt_same_head_allocates", func(t *testing.T) {
		f := newRecoverySeam(t)
		_, _ = allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
		got, err := allocate(f, f.stage.ID, recoveryHead, recoveryOlder)
		if err != nil || !got.won || len(rows(t, f)) != 2 {
			t.Fatalf("allocation = %+v, err %v, rows %d", got, err, len(rows(t, f)))
		}
	})
	t.Run("other_head_same_attempt_allocates", func(t *testing.T) {
		f := newRecoverySeam(t)
		_, _ = allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
		got, err := allocate(f, f.stage.ID, recoveryOtherHead, recoveryAttempt)
		if err != nil || !got.won || len(rows(t, f)) != 2 {
			t.Fatalf("allocation = %+v, err %v, rows %d", got, err, len(rows(t, f)))
		}
	})
	t.Run("other_stage_same_key_allocates", func(t *testing.T) {
		f := newRecoverySeam(t)
		_, _ = allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
		got, err := allocate(f, uuid.New(), recoveryHead, recoveryAttempt)
		if err != nil || !got.won || len(rows(t, f)) != 2 {
			t.Fatalf("allocation = %+v, err %v, rows %d", got, err, len(rows(t, f)))
		}
	})
	t.Run("capability_absent_no_append", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.s.cfg.AuditRepo = plainAuditOnly{f.au}
		if _, err := allocate(f, f.stage.ID, recoveryHead, recoveryAttempt); !errors.Is(err, errSupplementalStartNotAtomic) {
			t.Fatalf("err = %v", err)
		}
		if got := rows(t, f); len(got) != 0 {
			t.Fatalf("rows = %d, want none", len(got))
		}
	})
	t.Run("empty_attempt_no_append", func(t *testing.T) {
		f := newRecoverySeam(t)
		if _, err := allocate(f, f.stage.ID, recoveryHead, ""); !errors.Is(err, errSupplementalPassIdentityUnknown) {
			t.Fatalf("err = %v", err)
		}
		if got := rows(t, f); len(got) != 0 {
			t.Fatalf("rows = %d, want none", len(got))
		}
	})
	t.Run("empty_head_no_append", func(t *testing.T) {
		f := newRecoverySeam(t)
		if _, err := allocate(f, f.stage.ID, "", recoveryAttempt); !errors.Is(err, errSupplementalPassIdentityUnknown) {
			t.Fatalf("err = %v", err)
		}
		if got := rows(t, f); len(got) != 0 {
			t.Fatalf("rows = %d, want none", len(got))
		}
	})
	t.Run("append_error", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.au.appendErrCategory = CategorySupplementalReviewStarted
		got, err := allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
		if err == nil || got.won {
			t.Fatalf("allocation = %+v, err %v", got, err)
		}
	})
	t.Run("survivor_undecodable", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.s.cfg.AuditRepo = survivingStartAudit{f.au, &audit.Entry{Sequence: 9, Payload: []byte(`{"stage_attempt":7}`)}}
		got, err := allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
		if err == nil || got.won {
			t.Fatalf("allocation = %+v, err %v", got, err)
		}
	})
	t.Run("survivor_identity_mismatch", func(t *testing.T) {
		for name, survivor := range map[string]supplementalReviewStartedPayload{
			"attempt": {StageAttempt: recoveryOlder, HeadSHA: recoveryHead},
			"head":    {StageAttempt: recoveryAttempt, HeadSHA: recoveryOtherHead},
		} {
			t.Run(name, func(t *testing.T) {
				f := newRecoverySeam(t)
				f.s.cfg.AuditRepo = survivingStartAudit{f.au, &audit.Entry{Sequence: 9, Payload: recoveryPayload(t, survivor)}}
				got, err := allocate(f, f.stage.ID, recoveryHead, recoveryAttempt)
				if err == nil || got.won {
					t.Fatalf("allocation = %+v, err %v", got, err)
				}
			})
		}
	})
}

func TestExemptionPaths(t *testing.T) {
	got := exemptionPaths([]prompt.GateScopeExemption{{Path: recoveryPath, Reason: "r"}, {Path: recoveryPathB}})
	if !slices.Equal(got, []string{recoveryPath, recoveryPathB}) {
		t.Errorf("paths = %v", got)
	}
	if empty := exemptionPaths(nil); empty == nil || len(empty) != 0 {
		t.Errorf("an empty set must be [], never null: %#v", empty)
	}
}

// TestAllocateSupplementalPass_RealStore_PG releases 16 goroutines on one key
// through the real store: real-store component evidence, not split-mode proof.
func TestAllocateSupplementalPass_RealStore_PG(t *testing.T) {
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	runID, stageID := seedPGReviewRun(t, runRepo)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: auditRepo, RunRepo: runRepo})

	const racers = 16
	release := make(chan struct{})
	results := make([]supplementalPassAllocation, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			<-release
			results[i], errs[i] = s.allocateSupplementalPass(context.Background(), runID, stageID, recoveryHead, recoveryAttempt, 2, []string{recoveryPath})
		})
	}
	close(release)
	wg.Wait()

	entries, err := auditRepo.ListForRunByCategory(context.Background(), runID, CategorySupplementalReviewStarted)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("start rows = %d, want exactly one", len(entries))
	}
	winners := 0
	for i := range racers {
		if errs[i] != nil {
			t.Errorf("racer %d: %v", i, errs[i])
			continue
		}
		if results[i].won {
			winners++
		} else if results[i].sequence != entries[0].Sequence {
			t.Errorf("racer %d named sequence %d, want survivor %d", i, results[i].sequence, entries[0].Sequence)
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly one", winners)
	}
}

// assertVarOnlyDeclared fails on any production assignment to a capability var.
// Its RED is this guard's own finding.
func assertVarOnlyDeclared(t *testing.T, name string) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	declared := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		declared += countPackageVarDecl(file, name)
		for _, position := range varAssignments(file, name) {
			t.Errorf("%s: production code assigns %s; only the test binary may", fset.Position(position), name)
		}
	}
	if declared != 1 {
		t.Fatalf("%s declarations = %d, want one (a vacuous scan proves nothing)", name, declared)
	}
}

func countPackageVarDecl(file *ast.File, name string) int {
	n := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, ident := range value.Names {
				if ident.Name == name {
					n++
				}
			}
		}
	}
	return n
}

func varAssignments(file *ast.File, name string) []token.Pos {
	var found []token.Pos
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			if ident, ok := lhs.(*ast.Ident); ok && ident.Name == name {
				found = append(found, assign.Pos())
			}
		}
		return true
	})
	return found
}

// TestVarAssignments_DetectsPlantedAssignment keeps the two guards below from
// passing vacuously.
func TestVarAssignments_DetectsPlantedAssignment(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package p\nvar seam = 1\nfunc f() { seam = 2 }\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := varAssignments(file, "seam"); len(got) != 1 {
		t.Fatalf("assignments = %d, want the planted one", len(got))
	}
	if got := countPackageVarDecl(file, "seam"); got != 1 {
		t.Fatalf("declarations = %d", got)
	}
}

// TestSupplementalStartAppender_OnlyTestBinaryAssigns is Z42.
func TestSupplementalStartAppender_OnlyTestBinaryAssigns(t *testing.T) {
	assertVarOnlyDeclared(t, "supplementalStartAppender")
}

// TestSupplementalGuardedEdge_OnlyTestBinaryAssigns is Z52.
func TestSupplementalGuardedEdge_OnlyTestBinaryAssigns(t *testing.T) {
	assertVarOnlyDeclared(t, "supplementalGuardedEdge")
}

// markerRepo carries the marker, so the guarded adapter leaves it alone.
type markerRepo struct{ run.Repository }

func (markerRepo) noGuardedEdgeCapability() {}

// TestRecoveryTestAdapters_Modes pins the seams every in-memory route test relies on.
func TestRecoveryTestAdapters_Modes(t *testing.T) {
	ctx := context.Background()
	guard := func(err error) run.StageEdgeGuard {
		return run.StageEdgeGuard{Categories: []string{implementReviewedCategory}, Decide: func([]run.StageEdgeGuardRow) error { return err }}
	}
	t.Run("advances_on_identity", func(t *testing.T) {
		f := newRecoverySeam(t)
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, f.attempt, run.StageStateAwaitingApproval, guard(nil)); err != nil {
			t.Fatalf("err = %v", err)
		}
		if f.stage.State != run.StageStateAwaitingApproval {
			t.Fatalf("state = %s", f.stage.State)
		}
	})
	t.Run("decide_reads_rows_and_refuses_unwrapped", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.verdict(t, recoveryHead, f.attempt, planreview.VerdictReject)
		err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, f.attempt, run.StageStateAwaitingApproval, supplementalHeadRejectGuard(f.stage.ID, recoveryHead))
		var rejected *supplementalHeadRejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("err = %v", err)
		}
		if f.stage.State != run.StageStateRunning {
			t.Fatalf("state = %s", f.stage.State)
		}
	})
	t.Run("list_fault_refuses", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.au.listByCategoryErrCategory = implementReviewedCategory
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, f.attempt, run.StageStateAwaitingApproval, guard(nil)); err == nil {
			t.Fatal("a list fault must refuse the edge")
		}
		if f.stage.State != run.StageStateRunning {
			t.Fatalf("state = %s", f.stage.State)
		}
	})
	t.Run("nil_audit_decides_over_no_rows", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.s.cfg.AuditRepo = nil
		refused := errors.New("refused")
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, f.attempt, run.StageStateAwaitingApproval, guard(refused)); !errors.Is(err, refused) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("state_drift", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.stage.State = run.StageStateFailed
		var changed run.StageStateChangedError
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, f.attempt, run.StageStateAwaitingApproval, guard(nil)); !errors.As(err, &changed) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("attempt_drift", func(t *testing.T) {
		f := newRecoverySeam(t)
		var changed run.StageAttemptChangedError
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, recoveryOlder, run.StageStateAwaitingApproval, guard(nil)); !errors.As(err, &changed) {
			t.Fatalf("err = %v", err)
		}
		if f.stage.State != run.StageStateRunning {
			t.Fatalf("state = %s", f.stage.State)
		}
	})
	t.Run("legacy_identity", func(t *testing.T) {
		legacy := guard(nil)
		legacy.LegacyIdentity = true
		f := newRecoverySeam(t)
		var changed run.StageAttemptChangedError
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, "", run.StageStateAwaitingApproval, legacy); !errors.As(err, &changed) {
			t.Fatalf("an anchored row must refuse a legacy identity: %v", err)
		}
		f.stage.DispatchedAt = nil
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, "", run.StageStateAwaitingApproval, legacy); err != nil {
			t.Fatalf("a nil-dispatched row must match it: %v", err)
		}
	})
	t.Run("invalid_guard", func(t *testing.T) {
		f := newRecoverySeam(t)
		if err := f.s.transitionStageAtAttemptGuarded(ctx, f.stage.ID, "", run.StageStateAwaitingApproval, guard(nil)); !errors.Is(err, run.ErrStageEdgeGuardInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing_stage", func(t *testing.T) {
		f := newRecoverySeam(t)
		if err := f.s.transitionStageAtAttemptGuarded(ctx, uuid.New(), f.attempt, run.StageStateAwaitingApproval, guard(nil)); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("marker_and_nil_repo_stay_absent", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.s.cfg.RunRepo = markerRepo{f.rr}
		if _, ok := supplementalGuardedEdge(f.s); ok {
			t.Error("a marked repository must stay capability-absent")
		}
		f.s.cfg.RunRepo = nil
		if _, ok := supplementalGuardedEdge(f.s); ok {
			t.Error("a nil repository must stay capability-absent")
		}
	})
	t.Run("start_list_fault", func(t *testing.T) {
		f := newRecoverySeam(t)
		f.au.listByCategoryErrCategory = CategorySupplementalReviewStarted
		if _, err := f.s.allocateSupplementalPass(ctx, f.runRow.ID, f.stage.ID, recoveryHead, recoveryAttempt, 1, nil); err == nil {
			t.Fatal("a list fault must refuse the allocation")
		}
	})
	t.Run("crew_routed_audit_adapted", func(t *testing.T) {
		if _, ok := supplementalStartAppender(&crewRoutedAudit{auditFake: newAuditFake()}); !ok {
			t.Error("the crew-routed fake is adapted")
		}
		if _, ok := supplementalStartAppender(plainAuditOnly{newAuditFake()}); ok {
			t.Error("every other type takes the real assertion")
		}
	})
}

// --- guards ------------------------------------------------------------------

// TestSupplementalHeadRejectGuard_Modes is Z19.
func TestSupplementalHeadRejectGuard_Modes(t *testing.T) {
	stageID, otherStage := uuid.New(), uuid.New()
	verdictAs := func(authority planreview.AuthorityMode, origin string) run.StageEdgeGuardRow {
		evidence := supplementalVerdict(recoveryHead, recoveryAttempt, planreview.VerdictReject)
		evidence.Authority, evidence.Origin = authority, origin
		return evidenceRow(t, stageID, 1, evidence)
	}
	cases := []struct {
		name       string
		rows       []run.StageEdgeGuardRow
		wantReject bool
		wantFault  bool
	}{
		{name: "reject_current", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryHead, recoveryAttempt, planreview.VerdictReject)}, wantReject: true},
		{name: "reject_prior_attempt", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryHead, recoveryOlder, planreview.VerdictReject)}, wantReject: true},
		{name: "legacy_reject_no_attempt", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryHead, "", planreview.VerdictReject)}, wantReject: true},
		{name: "approve_only", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryHead, recoveryAttempt, planreview.VerdictApprove)}},
		{name: "no_rows"},
		{name: "other_head", rows: []run.StageEdgeGuardRow{verdictRow(t, stageID, 1, recoveryOtherHead, recoveryAttempt, planreview.VerdictReject)}},
		{name: "other_stage", rows: []run.StageEdgeGuardRow{verdictRow(t, otherStage, 1, recoveryHead, recoveryAttempt, planreview.VerdictReject)}},
		{name: "other_origin", rows: []run.StageEdgeGuardRow{verdictAs(planreview.AuthorityGating, "")}},
		{name: "row_undecodable", rows: []run.StageEdgeGuardRow{rawRow(implementReviewedCategory, 1, stageID, `{"verdict":7}`)}, wantFault: true},
		{name: "advisory_reject_not_binding", rows: []run.StageEdgeGuardRow{verdictAs(planreview.AuthorityAdvisory, planreview.OriginBaseRebaseReinvoke)}},
		{name: "gateless_reject_not_binding", rows: []run.StageEdgeGuardRow{verdictAs(planreview.AuthorityGateless, planreview.OriginBaseRebaseReinvoke)}},
		{name: "no_authority_reject_binds", rows: []run.StageEdgeGuardRow{verdictAs("", planreview.OriginBaseRebaseReinvoke)}, wantReject: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := supplementalHeadRejectGuard(stageID, recoveryHead)
			if !slices.Equal(guard.Categories, []string{implementReviewedCategory}) {
				t.Fatalf("categories = %v", guard.Categories)
			}
			err := guard.Decide(tc.rows)
			var rejected *supplementalHeadRejectedError
			if got := errors.As(err, &rejected); got != tc.wantReject {
				t.Fatalf("rejected = %v (err %v), want %v", got, err, tc.wantReject)
			}
			if !tc.wantReject && (err != nil) != tc.wantFault {
				t.Fatalf("err = %v, wantFault %v", err, tc.wantFault)
			}
		})
	}
}

// edgeOutcome names which refusal an edge guard returned.
func edgeOutcome(err error) string {
	var rejected *supplementalHeadRejectedError
	var unmet *reviewObligationUnmetError
	var incomplete *supplementalPassIncompleteError
	switch {
	case err == nil:
		return "pass"
	case errors.As(err, &rejected):
		return "rejected"
	case errors.As(err, &unmet):
		return "unmet"
	case errors.As(err, &incomplete):
		return "incomplete"
	}
	return "fault"
}

func TestSupplementalEdgeGuard_ObligationModes(t *testing.T) {
	stageID := uuid.New()
	full := []scopeExemption{{Path: recoveryPath}}
	namedRow := func(category string) run.StageEdgeGuardRow {
		row := verdictRow(t, stageID, 2, recoveryHead, recoveryAttempt, "")
		row.Category = category
		return row
	}
	cases := []struct {
		name string
		rows []run.StageEdgeGuardRow
		body []scopeExemption
		want string
	}{
		{name: "body_covers", rows: []run.StageEdgeGuardRow{checkpointRow(t, stageID, 1, recoveryHead, recoveryAttempt, recoveryPath)}, body: full, want: "pass"},
		{name: "start_missing_path_unmet", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 1, recoveryPath)}, want: "unmet"},
		{name: "discharged_passes", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 1, recoveryPath), verdictRow(t, stageID, 2, recoveryHead, recoveryAttempt, planreview.VerdictApprove)}, want: "pass"},
		{name: "partial_unmet", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 2, recoveryPath), verdictRow(t, stageID, 2, recoveryHead, recoveryAttempt, planreview.VerdictApprove)}, want: "unmet"},
		{name: "prior_attempt_complete_unmet", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryOlder, 1, recoveryPath), verdictRow(t, stageID, 2, recoveryHead, recoveryOlder, planreview.VerdictApprove)}, want: "unmet"},
		{name: "carrier_required_unmet", rows: []run.StageEdgeGuardRow{checkpointRow(t, stageID, 1, recoveryHead, recoveryAttempt, recoveryPath)}, want: "unmet"},
		{name: "parked_required_unmet", rows: []run.StageEdgeGuardRow{parkedRow(t, stageID, 1, recoveryHead, recoveryAttempt, recoveryPath)}, want: "unmet"},
		{name: "known_none_ignored", rows: []run.StageEdgeGuardRow{parkedRow(t, stageID, 1, recoveryHead, recoveryAttempt)}, want: "pass"},
		{name: "other_head_ignored", rows: []run.StageEdgeGuardRow{checkpointRow(t, stageID, 1, recoveryOtherHead, recoveryAttempt, recoveryPath)}, want: "pass"},
		{name: "other_stage_ignored", rows: []run.StageEdgeGuardRow{checkpointRow(t, uuid.New(), 1, recoveryHead, recoveryAttempt, recoveryPath)}, want: "pass"},
		{name: "row_undecodable", rows: []run.StageEdgeGuardRow{rawRow(CategoryReviewObligationCheckpoint, 1, stageID, `{"push_checkpoint":7}`)}, want: "fault"},
		{name: "verdict_undecodable", rows: []run.StageEdgeGuardRow{rawRow(implementReviewedCategory, 1, stageID, `{"verdict":7}`)}, want: "fault"},
		{name: "start_undecodable", rows: []run.StageEdgeGuardRow{rawRow(CategorySupplementalReviewStarted, 1, stageID, `{"configured_agents":"x"}`)}, want: "fault"},
		{name: "started_incomplete_full_body", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 2, recoveryPath)}, body: full, want: "incomplete"},
		{name: "started_complete_full_body", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 1, recoveryPath), verdictRow(t, stageID, 2, recoveryHead, recoveryAttempt, planreview.VerdictApprove)}, body: full, want: "pass"},
		{name: "named_row_not_counted", rows: []run.StageEdgeGuardRow{
			startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 1, recoveryPath),
			namedRow("implement_review_failed"), namedRow("implement_review_skipped"),
		}, body: full, want: "incomplete"},
		{name: "prior_attempt_start_ignored", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryOlder, 1, recoveryPath)}, body: full, want: "pass"},
		{name: "other_head_start_ignored", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryOtherHead, recoveryAttempt, 1, recoveryPath)}, want: "pass"},
		{name: "reject_precedes_every_other_predicate", rows: []run.StageEdgeGuardRow{startRow(t, stageID, 1, recoveryHead, recoveryAttempt, 2, recoveryPath), verdictRow(t, stageID, 2, recoveryHead, recoveryOlder, planreview.VerdictReject)}, want: "rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := supplementalEdgeGuard(stageID, recoveryHead, recoveryAttempt, tc.body).Decide(tc.rows)
			if got := edgeOutcome(err); got != tc.want {
				t.Fatalf("outcome = %s (err %v), want %s", got, err, tc.want)
			}
		})
	}
	t.Run("reads_exactly_the_evidence_categories", func(t *testing.T) {
		want := []string{implementReviewedCategory, CategorySupplementalReviewStarted, CategoryReviewObligationCheckpoint, CategoryScopeCompletenessParked}
		if got := supplementalEdgeGuard(stageID, recoveryHead, recoveryAttempt, nil).Categories; !slices.Equal(got, want) {
			t.Fatalf("categories = %v, want %v", got, want)
		}
	})
	t.Run("unmet_names_the_missing_paths", func(t *testing.T) {
		err := supplementalEdgeGuard(stageID, recoveryHead, recoveryAttempt, nil).Decide([]run.StageEdgeGuardRow{checkpointRow(t, stageID, 1, recoveryHead, recoveryAttempt, recoveryPath, recoveryPathB)})
		var unmet *reviewObligationUnmetError
		if !errors.As(err, &unmet) || !slices.Equal(unmet.missing, []string{recoveryPath, recoveryPathB}) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("errors_describe_themselves", func(t *testing.T) {
		for _, err := range []error{&supplementalHeadRejectedError{headSHA: "h"}, &reviewObligationUnmetError{missing: []string{"p"}}, &supplementalPassIncompleteError{headSHA: "h"}} {
			if !strings.HasPrefix(err.Error(), "supplemental review: ") {
				t.Errorf("message = %q", err.Error())
			}
		}
	})
}

// TestObligationReducer_ReadAndEdgeAgree drives one row set through both callers.
func TestObligationReducer_ReadAndEdgeAgree(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, f *recoverySeam)
		body []scopeExemption
		want string
	}{
		{name: "clean", seed: func(*testing.T, *recoverySeam) {}, want: "pass"},
		{name: "unmet", seed: func(t *testing.T, f *recoverySeam) { f.carrier(t, recoveryHead, recoveryPath) }, want: "unmet"},
		{name: "discharged", seed: func(t *testing.T, f *recoverySeam) {
			f.start(t, recoveryHead, f.attempt, 1, recoveryPath)
			f.verdict(t, recoveryHead, f.attempt, planreview.VerdictApprove)
		}, want: "pass"},
		{name: "incomplete", seed: func(t *testing.T, f *recoverySeam) { f.start(t, recoveryHead, f.attempt, 1, recoveryPath) },
			body: []scopeExemption{{Path: recoveryPath}}, want: "incomplete"},
		{name: "fault", seed: func(t *testing.T, f *recoverySeam) { f.append(t, implementReviewedCategory, []byte(`{"verdict":7}`)) }, want: "fault"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRecoverySeam(t)
			tc.seed(t, f)
			check, readErr := f.s.requiredReviewObligationMissing(ctx, f.runRow.ID, f.stage, recoveryHead, tc.body)
			read := "pass"
			switch {
			case readErr != nil:
				read = "fault"
			case len(check.missing) > 0 && !check.discharged:
				read = "unmet"
			case check.startedIncomplete:
				read = "incomplete"
			}
			rows, err := f.s.listGuardRows(ctx, f.runRow.ID, supplementalEdgeCategories)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			edge := edgeOutcome(supplementalEdgeGuard(f.stage.ID, recoveryHead, f.attempt, tc.body).Decide(rows))
			if read != tc.want || edge != tc.want {
				t.Fatalf("read = %s, edge = %s, want %s", read, edge, tc.want)
			}
		})
	}
}

// --- outcome-mode proof ------------------------------------------------------

func TestVariantModeProved_Modes(t *testing.T) {
	stageID, otherStage := uuid.New(), uuid.New()
	liveFixup := `{"concerns":[{"severity":"HIGH","category":"c","note":"n"}]}`
	liveConflict := `{"branch":"b","base_ref":"main","expected_head_sha":"h"}`
	fixup := rawRow(CategoryStageFixupTriggered, 1, stageID, liveFixup)
	conflict := rawRow(CategoryStageConflictResolutionTriggered, 1, stageID, liveConflict)
	// A later row's payload would itself prove the mode, so only the newest-row rule
	// keeps a consumed trigger from proving anything.
	later := func(category string) run.StageEdgeGuardRow {
		return rawRow(category, 2, stageID, `{"concerns":[{"note":"n"}],"branch":"b","base_ref":"main","expected_head_sha":"h"}`)
	}
	cases := []struct {
		name    string
		outcome string
		isChild bool
		rows    []run.StageEdgeGuardRow
		want    bool
		wantErr bool
	}{
		{name: "child_pushed", outcome: "pushed", isChild: true, want: true},
		{name: "not_child_pushed", outcome: "pushed", rows: []run.StageEdgeGuardRow{fixup}},
		{name: "live_fixup", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{fixup}, want: true},
		{name: "live_fixup_no_changes", outcome: "fixup_no_changes", rows: []run.StageEdgeGuardRow{fixup}, want: true},
		{name: "retried_live_fixup", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{fixup, later("stage_retried"), rawRow(implementReviewedCategory, 3, stageID, `{}`)}, want: true},
		{name: "consumed_by_pushed", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{fixup, later(CategoryFixupPushed)}},
		{name: "consumed_by_no_changes", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{fixup, later(CategoryFixupNoChanges)}},
		{name: "consumed_by_recovered", outcome: "fixup_no_changes", rows: []run.StageEdgeGuardRow{fixup, later(CategoryStageFixupRecovered)}},
		{name: "superseded_marker", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{fixup, later(CategoryStageFixupSuperseded)}},
		{name: "empty_concerns", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{rawRow(CategoryStageFixupTriggered, 1, stageID, `{"concerns":[]}`)}},
		{name: "other_stage_trigger", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{rawRow(CategoryStageFixupTriggered, 1, otherStage, liveFixup)}},
		{name: "live_conflict", outcome: "conflict_resolution_pushed", rows: []run.StageEdgeGuardRow{conflict}, want: true},
		{name: "conflict_consumed_pushed", outcome: "conflict_resolution_pushed", rows: []run.StageEdgeGuardRow{conflict, later(CategoryConflictResolutionPushed)}},
		{name: "conflict_consumed_failed", outcome: "conflict_resolution_pushed", rows: []run.StageEdgeGuardRow{conflict, later(CategoryStageConflictResolutionFailed)}},
		{name: "conflict_unpopulated", outcome: "conflict_resolution_pushed", rows: []run.StageEdgeGuardRow{rawRow(CategoryStageConflictResolutionTriggered, 1, stageID, `{"branch":"b"}`)}},
		{name: "fixup_spelled_on_live_conflict", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{conflict}},
		{name: "later_merge_candidate", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{fixup, later(CategoryStageMergeCandidateVerifyTriggered)}},
		{name: "undecodable", outcome: "fixup_pushed", rows: []run.StageEdgeGuardRow{rawRow(CategoryStageFixupTriggered, 1, stageID, `{"concerns":"x"}`)}, wantErr: true},
		{name: "conflict_undecodable", outcome: "conflict_resolution_pushed", rows: []run.StageEdgeGuardRow{rawRow(CategoryStageConflictResolutionTriggered, 1, stageID, `{"branch":7}`)}, wantErr: true},
		{name: "no_rows", outcome: "fixup_pushed"},
		{name: "unknown_outcome", outcome: "merge_candidate_verified", isChild: true, rows: []run.StageEdgeGuardRow{fixup}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := variantModeProved(tc.outcome, tc.rows, stageID, tc.isChild)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("proved = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVariantEdgeGuard_Modes(t *testing.T) {
	stageID := uuid.New()
	live := rawRow(CategoryStageFixupTriggered, 1, stageID, `{"concerns":[{"note":"n"}]}`)
	reject := func(head string) run.StageEdgeGuardRow {
		return verdictRow(t, stageID, 5, head, recoveryOlder, planreview.VerdictReject)
	}
	advisory := supplementalVerdict(recoveryHead, recoveryAttempt, planreview.VerdictReject)
	advisory.Authority = planreview.AuthorityAdvisory
	firstReview := supplementalVerdict(recoveryHead, recoveryAttempt, planreview.VerdictReject)
	firstReview.Origin = ""
	cases := []struct {
		name    string
		outcome string
		head    string
		isChild bool
		rows    []run.StageEdgeGuardRow
		want    string
	}{
		{name: "proved_new_head_old_reject_passes", outcome: "fixup_pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{live, reject(recoveryHead)}, want: "pass"},
		{name: "proved_same_head_reject_refuses", outcome: "fixup_pushed", head: recoveryHead, rows: []run.StageEdgeGuardRow{live, reject(recoveryHead)}, want: "rejected"},
		{name: "proved_same_head_unmet_refuses", outcome: "fixup_pushed", head: recoveryHead, rows: []run.StageEdgeGuardRow{live, checkpointRow(t, stageID, 2, recoveryHead, recoveryAttempt, recoveryPath)}, want: "unmet"},
		{name: "proved_no_head_passes", outcome: "fixup_no_changes", rows: []run.StageEdgeGuardRow{live, reject(recoveryHead), reject("")}, want: "pass"},
		{name: "proved_child_pushed_new_head_passes", outcome: "pushed", head: recoveryOtherHead, isChild: true, rows: []run.StageEdgeGuardRow{reject(recoveryHead)}, want: "pass"},
		{name: "unproved_other_head_reject_refuses", outcome: "fixup_pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{reject(recoveryHead)}, want: "rejected"},
		{name: "unproved_no_head_reject_refuses", outcome: "fixup_no_changes", rows: []run.StageEdgeGuardRow{reject(recoveryHead)}, want: "rejected"},
		{name: "unproved_empty_head_reject_refuses", outcome: "fixup_no_changes", rows: []run.StageEdgeGuardRow{reject("")}, want: "rejected"},
		{name: "unproved_unmet_refuses", outcome: "pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{checkpointRow(t, stageID, 2, recoveryHead, recoveryAttempt, recoveryPath)}, want: "unmet"},
		{name: "unproved_start_refuses", outcome: "conflict_resolution_pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{startRow(t, stageID, 2, recoveryHead, recoveryAttempt, 1, recoveryPath)}, want: "unmet"},
		{name: "unproved_nothing_recorded_passes", outcome: "pushed", head: recoveryHead, want: "pass"},
		{name: "unproved_other_stage_reject_passes", outcome: "pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{rawRow(implementReviewedCategory, 1, uuid.New(), `{"verdict":7}`)}, want: "pass"},
		{name: "unproved_first_review_reject_passes", outcome: "pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{evidenceRow(t, stageID, 5, firstReview)}, want: "pass"},
		{name: "unproved_known_none_passes", outcome: "pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{parkedRow(t, stageID, 2, recoveryHead, recoveryAttempt)}, want: "pass"},
		{name: "non_gating_reject_passes", outcome: "pushed", head: recoveryOtherHead, rows: []run.StageEdgeGuardRow{evidenceRow(t, stageID, 5, advisory)}, want: "pass"},
		{name: "proved_started_incomplete_refuses", outcome: "fixup_pushed", head: recoveryHead, rows: []run.StageEdgeGuardRow{live, startRow(t, stageID, 2, recoveryHead, recoveryAttempt, 1)}, want: "incomplete"},
		{name: "proof_row_undecodable", outcome: "fixup_pushed", head: recoveryHead, rows: []run.StageEdgeGuardRow{rawRow(CategoryStageFixupTriggered, 1, stageID, `{"concerns":"x"}`)}, want: "fault"},
		{name: "unproved_verdict_undecodable", outcome: "pushed", head: recoveryHead, rows: []run.StageEdgeGuardRow{rawRow(implementReviewedCategory, 1, stageID, `{"origin":7}`)}, want: "fault"},
		{name: "unproved_carrier_undecodable", outcome: "pushed", head: recoveryHead, rows: []run.StageEdgeGuardRow{rawRow(CategoryScopeCompletenessParked, 1, stageID, `{"review_obligation":7}`)}, want: "fault"},
		{name: "unproved_start_undecodable", outcome: "pushed", head: recoveryHead, rows: []run.StageEdgeGuardRow{rawRow(CategorySupplementalReviewStarted, 1, stageID, `{"head_sha":7}`)}, want: "fault"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := variantEdgeGuard(tc.outcome, stageID, tc.head, recoveryAttempt, tc.isChild)
			if got := edgeOutcome(guard.Decide(tc.rows)); got != tc.want {
				t.Fatalf("outcome = %s (err %v), want %s", got, guard.Decide(tc.rows), tc.want)
			}
		})
	}
	t.Run("reads_mode_and_evidence_categories", func(t *testing.T) {
		guard := variantEdgeGuard("pushed", stageID, recoveryHead, recoveryAttempt, false)
		for _, category := range append(slices.Clone(supplementalEdgeCategories), variantModeCategories...) {
			if !slices.Contains(guard.Categories, category) {
				t.Errorf("guard does not read %s", category)
			}
		}
		if len(supplementalEdgeCategories) != 4 {
			t.Errorf("shared categories were mutated: %v", supplementalEdgeCategories)
		}
	})
	t.Run("legacy_identity_only_on_empty_attempt", func(t *testing.T) {
		if variantEdgeGuard("pushed", stageID, recoveryHead, recoveryAttempt, false).LegacyIdentity {
			t.Error("an anchored attempt must not take the legacy identity")
		}
		if !variantEdgeGuard("pushed", stageID, recoveryHead, "", false).LegacyIdentity {
			t.Error("an empty attempt must take the legacy identity")
		}
	})
}

// --- categories --------------------------------------------------------------

// recoveryPayloadViolation enforces the interim allow-lists ADR-106's registry flag
// will rely on (#4599): exact key sets, naming only this run.
func recoveryPayloadViolation(category string, payload []byte, runID, stageID uuid.UUID) string {
	allowed := map[string][]string{
		CategorySupplementalReviewStarted:  {"run_id", "stage_id", "stage_attempt", "head_sha", "configured_agents", "required_paths", "origin", "pass_key"},
		CategoryReviewObligationCheckpoint: {"run_id", "stage_id", "resume_kind", "push_checkpoint"},
	}[category]
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return "undecodable: " + err.Error()
	}
	for key := range top {
		if !slices.Contains(allowed, key) {
			return "field outside the allow-list: " + key
		}
	}
	if string(top["run_id"]) != `"`+runID.String()+`"` {
		return "run_id names another run"
	}
	if string(top["stage_id"]) != `"`+stageID.String()+`"` {
		return "stage_id names another stage"
	}
	if raw, ok := top["push_checkpoint"]; ok {
		var checkpoint map[string]json.RawMessage
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return "undecodable checkpoint: " + err.Error()
		}
		for key := range checkpoint {
			if !slices.Contains([]string{"branch", "head_sha", "base_sha", "verified_tree_sha", "pr_title", "pr_body", "review_obligation"}, key) {
				return "checkpoint field outside the allow-list: " + key
			}
		}
	}
	return ""
}

// TestRecoveryCategories_RegisteredAndRunBoundReadable is Z41. It is INTERIM, not
// ADR-106 registry option B: #4599 owns the mandatory flag.
func TestRecoveryCategories_RegisteredAndRunBoundReadable(t *testing.T) {
	ctx := context.Background()
	f := newRecoverySeam(t)
	f.carrier(t, recoveryHead, recoveryPath)
	f.start(t, recoveryHead, f.attempt, 1, recoveryPath)
	categories := []string{CategoryReviewObligationCheckpoint, CategorySupplementalReviewStarted}
	if categories[0] != "review_obligation_checkpoint" || categories[1] != "supplemental_review_started" {
		t.Fatalf("wire values drifted: %v", categories)
	}
	var entries []*audit.Entry
	for _, category := range categories {
		if !audit.IsKnownCategory(category) {
			t.Errorf("%s is not registered", category)
		}
		if issuecomment.RendersActivity(category) {
			t.Errorf("%s must stay a resolver hint, not an issue-comment activity line", category)
		}
		got, err := f.au.ListForRunByCategory(ctx, f.runRow.ID, category)
		if err != nil || len(got) != 1 {
			t.Fatalf("list %s: %d rows, %v", category, len(got), err)
		}
		// auditFake's listed entry may not stamp Category on every path.
		got[0].Category = category
		entries = append(entries, got[0])
		if violation := recoveryPayloadViolation(category, got[0].Payload, f.runRow.ID, f.stage.ID); violation != "" {
			t.Errorf("%s: %s", category, violation)
		}
	}
	kept := withholdScopeOverlapFromRunBound(entries, "mcp:run:"+f.runRow.ID.String())
	if len(kept) != len(entries) {
		t.Errorf("run-bound read kept %d of %d rows", len(kept), len(entries))
	}
	t.Run("field_naming_another_run_fails", func(t *testing.T) {
		other := uuid.New()
		plantedField := []byte(`{"run_id":"` + f.runRow.ID.String() + `","stage_id":"` + f.stage.ID.String() + `","other_run_id":"` + other.String() + `"}`)
		if recoveryPayloadViolation(CategorySupplementalReviewStarted, plantedField, f.runRow.ID, f.stage.ID) == "" {
			t.Error("a foreign field must fail the allow-list")
		}
		foreignRun := []byte(`{"run_id":"` + other.String() + `","stage_id":"` + f.stage.ID.String() + `"}`)
		if recoveryPayloadViolation(CategorySupplementalReviewStarted, foreignRun, f.runRow.ID, f.stage.ID) == "" {
			t.Error("another run's id must fail")
		}
		foreignStage := []byte(`{"run_id":"` + f.runRow.ID.String() + `","stage_id":"` + other.String() + `"}`)
		if recoveryPayloadViolation(CategorySupplementalReviewStarted, foreignStage, f.runRow.ID, f.stage.ID) == "" {
			t.Error("another stage's id must fail")
		}
		plantedNested := []byte(`{"run_id":"` + f.runRow.ID.String() + `","stage_id":"` + f.stage.ID.String() + `","push_checkpoint":{"parent_run_id":"` + other.String() + `"}}`)
		if recoveryPayloadViolation(CategoryReviewObligationCheckpoint, plantedNested, f.runRow.ID, f.stage.ID) == "" {
			t.Error("a foreign checkpoint field must fail")
		}
	})
}

// --- golden ------------------------------------------------------------------

// reviewObligationWireSections is testdata/wire/review_obligation_wire.json: one
// contract, one fixture. Each section is a compact message compared byte for byte
// by the slice that owns its surface; no other slice may edit the file.
type reviewObligationWireSections struct {
	FailureReport       json.RawMessage `json:"failure_report"`
	PromptFields        json.RawMessage `json:"prompt_fields"`
	ResumedArtifact     json.RawMessage `json:"resumed_artifact"`
	KnownRequiredPark   json.RawMessage `json:"known_required_park"`
	ReviewRoundInFlight json.RawMessage `json:"review_round_in_flight"`
}

// loadReviewObligationWire reads the shared golden, failing closed on any read or
// decode error and on an unknown section.
func loadReviewObligationWire(t *testing.T) reviewObligationWireSections {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "testdata", "wire", "review_obligation_wire.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var sections reviewObligationWireSections
	if err := decoder.Decode(&sections); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return sections
}

// goldenFailureBody mirrors the runner's pullRequestFailureBody field order with
// review_obligation LAST, after the held-commit text. The runner's own test pins
// the real struct to these bytes.
type goldenFailureBody struct {
	Outcome          string            `json:"outcome"`
	Category         string            `json:"category"`
	Reason           string            `json:"reason"`
	Branch           string            `json:"branch,omitempty"`
	HeadSHA          string            `json:"head_sha,omitempty"`
	BaseSHA          string            `json:"base_sha,omitempty"`
	VerifiedTreeSHA  string            `json:"verified_tree_sha,omitempty"`
	ResumeKind       string            `json:"resume_kind,omitempty"`
	Title            string            `json:"title,omitempty"`
	Body             string            `json:"body,omitempty"`
	ReviewObligation *reviewObligation `json:"review_obligation,omitempty"`
}

// TestReviewObligationGolden_Sections pins the fixture against the backend-side
// marshalers this slice can reach; S2, S3 and S4 pin their real writers.
func TestReviewObligationGolden_Sections(t *testing.T) {
	sections := loadReviewObligationWire(t)
	named := map[string]json.RawMessage{
		"failure_report": sections.FailureReport, "prompt_fields": sections.PromptFields,
		"resumed_artifact": sections.ResumedArtifact, "known_required_park": sections.KnownRequiredPark,
		"review_round_in_flight": sections.ReviewRoundInFlight,
	}
	for name, section := range named {
		t.Run(name+"_compact", func(t *testing.T) {
			if len(section) == 0 {
				t.Fatal("section is absent")
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, section); err != nil {
				t.Fatalf("compact: %v", err)
			}
			if !bytes.Equal(compact.Bytes(), section) {
				t.Fatal("section is not a compact single-line message")
			}
		})
	}

	var failure goldenFailureBody
	if err := json.Unmarshal(sections.FailureReport, &failure); err != nil {
		t.Fatalf("decode failure_report: %v", err)
	}
	t.Run("failure_report", func(t *testing.T) {
		got, err := json.Marshal(failure)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Equal(got, sections.FailureReport) {
			t.Fatalf("key order drifted:\n got %s\nwant %s", got, sections.FailureReport)
		}
		if failure.Outcome != "failed" || failure.ResumeKind != "" || failure.Branch == "" || failure.BaseSHA == "" {
			t.Errorf("not a pr_open failure report: %+v", failure)
		}
		if reviewObligationKnowledge(failure.ReviewObligation) != obligationKnownRequired {
			t.Errorf("obligation = %+v", failure.ReviewObligation)
		}
		dispatched, err := time.Parse(time.RFC3339Nano, failure.ReviewObligation.StageAttempt)
		if err != nil {
			t.Fatalf("stage_attempt is not a token: %v", err)
		}
		stage := &run.Stage{DispatchedAt: &dispatched}
		if run.StageAttemptToken(stage.DispatchedAt) != failure.ReviewObligation.StageAttempt {
			t.Errorf("stage_attempt does not round-trip through StageAttemptToken")
		}
		if err := validateReviewObligationReport(failure.ReviewObligation, resumeKindPROpen, failure.HeadSHA, stage); err != nil {
			t.Errorf("the golden report does not validate: %v", err)
		}
	})
	t.Run("prompt_fields", func(t *testing.T) {
		got, err := json.Marshal(struct {
			HeldCommitReviewObligation     *reviewObligation `json:"held_commit_review_obligation,omitempty"`
			SupportsReviewObligationResume bool              `json:"supports_review_obligation_resume,omitempty"`
		}{failure.ReviewObligation, true})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Equal(got, sections.PromptFields) {
			t.Fatalf("prompt fields:\n got %s\nwant %s", got, sections.PromptFields)
		}
	})
	t.Run("resumed_artifact", func(t *testing.T) {
		wire, err := json.Marshal(failure.ReviewObligation.SupplementalScopeExemptions)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		key := `,"supplemental_scope_exemptions":` + string(wire)
		if !bytes.Contains(sections.ResumedArtifact, []byte(key)) {
			t.Fatalf("artifact does not carry the served set %s", key)
		}
		existing, err := os.ReadFile(filepath.Join(repoRoot(t), "testdata", "wire", "held_commit_pr_artifact.json"))
		if err != nil {
			t.Fatalf("read existing artifact: %v", err)
		}
		without := bytes.Replace(sections.ResumedArtifact, []byte(key), nil, 1)
		if !bytes.Equal(without, bytes.TrimSuffix(existing, []byte("\n"))) {
			t.Fatalf("only the supplemental key may differ from held_commit_pr_artifact.json:\n got %s\nwant %s", without, existing)
		}
		// Its sorted position: the runner marshals a map.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(sections.ResumedArtifact, &fields); err != nil {
			t.Fatalf("decode: %v", err)
		}
		before, after := bytes.Index(sections.ResumedArtifact, []byte(`"pr_url"`)), bytes.Index(sections.ResumedArtifact, []byte(`"supplemental_scope_exemptions"`))
		if before < 0 || after < before || after > bytes.Index(sections.ResumedArtifact, []byte(`"title"`)) {
			t.Errorf("key is not at its sorted position: %s", sections.ResumedArtifact)
		}
		var artifactHead struct {
			HeadSHA string `json:"head_sha"`
			BaseSHA string `json:"base_sha"`
		}
		if err := json.Unmarshal(sections.ResumedArtifact, &artifactHead); err != nil {
			t.Fatalf("decode head: %v", err)
		}
		if artifactHead.HeadSHA != failure.HeadSHA || artifactHead.BaseSHA != failure.BaseSHA {
			t.Errorf("artifact coordinates %+v differ from the failure report", artifactHead)
		}
	})
	t.Run("known_required_park", func(t *testing.T) {
		var park struct {
			ParkRow       json.RawMessage `json:"park_row"`
			ParkedPayload json.RawMessage `json:"parked_payload"`
		}
		decoder := json.NewDecoder(bytes.NewReader(sections.KnownRequiredPark))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&park); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var row run.ScopeCompletenessPark
		if err := json.Unmarshal(park.ParkRow, &row); err != nil {
			t.Fatalf("decode park row: %v", err)
		}
		if got, err := json.Marshal(row); err != nil || !bytes.Equal(got, park.ParkRow) {
			t.Fatalf("park row is not the real struct's bytes (%v):\n got %s\nwant %s", err, got, park.ParkRow)
		}
		if row.BaseSHA != "" || bytes.Contains(park.ParkRow, []byte(`"base_sha"`)) {
			t.Errorf("a known-required park row must withhold its base: %s", park.ParkRow)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(park.ParkedPayload, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if got, err := json.Marshal(payload); err != nil || !bytes.Equal(got, park.ParkedPayload) {
			t.Fatalf("payload is not a sorted map's bytes (%v):\n got %s\nwant %s", err, got, park.ParkedPayload)
		}
		if _, leaked := payload["base_sha"]; leaked {
			t.Errorf("a known-required payload must withhold base_sha: %s", park.ParkedPayload)
		}
		if string(payload["required_review_base_sha"]) != `"`+failure.BaseSHA+`"` {
			t.Errorf("required_review_base_sha = %s", payload["required_review_base_sha"])
		}
		for _, key := range []string{"run_id", "stage_id"} {
			var id uuid.UUID
			if err := json.Unmarshal(payload[key], &id); err != nil {
				t.Errorf("%s is not a uuid: %v", key, err)
			}
		}
		var obligation reviewObligation
		if err := json.Unmarshal(payload["review_obligation"], &obligation); err != nil {
			t.Fatalf("decode obligation: %v", err)
		}
		if reviewObligationKnowledge(&obligation) != obligationKnownRequired || obligation.HeadSHA != row.HeldCommitSHA {
			t.Errorf("obligation = %+v", obligation)
		}
	})
	t.Run("review_round_in_flight", func(t *testing.T) {
		var flight struct {
			Status   int               `json:"status"`
			Headers  map[string]string `json:"headers"`
			Envelope json.RawMessage   `json:"envelope"`
		}
		decoder := json.NewDecoder(bytes.NewReader(sections.ReviewRoundInFlight))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&flight); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var envelope struct {
			Error struct {
				Details struct {
					RunID   uuid.UUID `json:"run_id"`
					StageID uuid.UUID `json:"stage_id"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(flight.Envelope, &envelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		runID, stageID := envelope.Error.Details.RunID, envelope.Error.Details.StageID
		s := New(Config{Addr: "127.0.0.1:0"})
		var w *httptest.ResponseRecorder
		// The header is rounded up from a live clock; a stalled host retries, the
		// bytes never loosen.
		for range 3 {
			w = httptest.NewRecorder()
			s.writeSupplementalPassBusy(w, httptest.NewRequest(http.MethodPost, "/", nil), runID, stageID, time.Now().Add(90*time.Second-10*time.Millisecond))
			if w.Header().Get(reviewPassRemainingHeader) == flight.Headers[reviewPassRemainingHeader] {
				break
			}
		}
		if w.Code != flight.Status {
			t.Errorf("status = %d, want %d", w.Code, flight.Status)
		}
		if len(flight.Headers) != 2 {
			t.Errorf("headers = %v, want Retry-After and the remaining header only", flight.Headers)
		}
		for name, want := range flight.Headers {
			if got := w.Header().Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if got := bytes.TrimSuffix(w.Body.Bytes(), []byte("\n")); !bytes.Equal(got, flight.Envelope) {
			t.Fatalf("envelope:\n got %s\nwant %s", got, flight.Envelope)
		}
	})
}
