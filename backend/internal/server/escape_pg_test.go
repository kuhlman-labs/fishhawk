package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// escape_pg_test.go drives POST /v0/runs/{run_id}/escapes against the
// PRODUCTION run, audit and concern repositories on a real Postgres (E94.3 /
// #4694): the closure's atomicity lives in audit.AppendChainedDedupedTx's
// run-row lock + in-transaction scan + BeforeAppend hook, which no fake can
// exercise.

type escapePGFixture struct {
	pool     *pgxpool.Pool
	mux      *http.ServeMux
	runs     run.Repository
	concerns concern.Repository
	runID    uuid.UUID
	stageID  uuid.UUID
}

func newEscapePGFixture(t *testing.T) *escapePGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	concernRepo := concern.NewPostgresRepository(pool)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, ConcernRepo: concernRepo})
	fx := &escapePGFixture{pool: pool, mux: http.NewServeMux(), runs: runRepo, concerns: concernRepo}
	s.registerRoutes(fx.mux)
	fx.runID, fx.stageID = fx.createRun(t, ctx)
	return fx
}

func (fx *escapePGFixture) createRun(t *testing.T, ctx context.Context) (uuid.UUID, uuid.UUID) {
	t.Helper()
	r, err := fx.runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "acme/widgets", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	st, err := fx.runs.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return r.ID, st.ID
}

// do is t-FREE so concurrent workers can call it.
func (fx *escapePGFixture) do(runID uuid.UUID, id Identity, body any) *httptest.ResponseRecorder {
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/escapes", strings.NewReader(string(buf)))
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, withIdentity(req, id))
	return rec
}

func (fx *escapePGFixture) countRows(t *testing.T, runID uuid.UUID, recordKind string) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries WHERE run_id = $1 AND category = $2 AND payload->>'record_kind' = $3`,
		runID, CategoryEscapeRecorded, recordKind).Scan(&n); err != nil {
		t.Fatalf("count escape rows: %v", err)
	}
	return n
}

func pgBirth(t *testing.T, fx *escapePGFixture, id Identity, key string) int64 {
	t.Helper()
	rec := fx.do(fx.runID, id, humanFindingBody(key))
	return decodeEscapeResponse(t, rec).AuditSequence
}

func wantPGCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	if got := decodeErrorEnvelope(t, rec).Code; got != code {
		t.Fatalf("code = %q, want %q", got, code)
	}
}

// The production audit repository carries the atomic capability the closure
// relies on, and a birth persists through it.
func TestRecordEscape_PG_CapabilityWired(t *testing.T) {
	fx := newEscapePGFixture(t)
	if _, ok := audit.NewPostgresRepository(fx.pool).(audit.DedupedChainAppender); !ok {
		t.Fatal("the production audit repository does not implement audit.DedupedChainAppender")
	}
	seq := pgBirth(t, fx, escapeStaticID(), "k1")
	entries, err := audit.NewPostgresRepository(fx.pool).ListForRunByCategory(context.Background(), fx.runID, CategoryEscapeRecorded)
	if err != nil || len(entries) != 1 || entries[0].Sequence != seq || entries[0].StageID != nil {
		t.Fatalf("persisted escape rows = %+v (err %v), want one stage-less row at %d", entries, err, seq)
	}
	wantPGCode(t, fx.do(fx.runID, escapeSessionID(), humanFindingBody("k1")), http.StatusConflict, "escape_already_recorded")
	if n := fx.countRows(t, fx.runID, escapeRecordKindBirth); n != 1 {
		t.Errorf("births = %d, want 1", n)
	}
}

// CF-B7: the birth row still reads pending, so ONLY the supersedes_sequence
// dedupe refuses the second close.
func TestRecordEscape_PG_ReClosureRefused(t *testing.T) {
	fx := newEscapePGFixture(t)
	seq := pgBirth(t, fx, escapeStaticID(), "k1")
	decodeEscapeResponse(t, fx.do(fx.runID, escapeSessionID(), closeBody(seq, "defect")))
	wantPGCode(t, fx.do(fx.runID, escapeSessionID(), closeBody(seq, "no_defect")), http.StatusConflict, "escape_already_closed")
	if n := fx.countRows(t, fx.runID, escapeRecordKindClosure); n != 1 {
		t.Errorf("closure rows = %d, want exactly 1", n)
	}
}

func TestRecordEscape_PG_ConcurrentClosesExactlyOneSuccessor(t *testing.T) {
	fx := newEscapePGFixture(t)
	seq := pgBirth(t, fx, escapeStaticID(), "k1")
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = fx.do(fx.runID, escapeSessionID(), closeBody(seq, "defect")).Code
		}(i)
	}
	wg.Wait()
	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d from a concurrent close", c)
		}
	}
	if created != 1 || conflicts != n-1 {
		t.Errorf("created = %d, conflicts = %d, want 1 and %d", created, conflicts, n-1)
	}
	if rows := fx.countRows(t, fx.runID, escapeRecordKindClosure); rows != 1 {
		t.Errorf("closure rows = %d, want exactly 1", rows)
	}
}

func TestRecordEscape_PG_ConcurrentBirthsSameKeyExactlyOne(t *testing.T) {
	fx := newEscapePGFixture(t)
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = fx.do(fx.runID, escapeSessionID(), humanFindingBody("same-key")).Code
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else if c != http.StatusConflict {
			t.Errorf("unexpected status %d from a concurrent birth", c)
		}
	}
	if created != 1 {
		t.Errorf("created = %d, want exactly 1", created)
	}
	if rows := fx.countRows(t, fx.runID, escapeRecordKindBirth); rows != 1 {
		t.Errorf("birth rows = %d, want exactly 1", rows)
	}
}

// The target is read on the closure's own transaction: a counted birth, an
// unknown sequence and another run's escape are each refused 422, writing
// nothing.
func TestRecordEscape_PG_TargetCheckedInTransaction(t *testing.T) {
	fx := newEscapePGFixture(t)
	counted := pgBirth(t, fx, escapeSessionID(), "counted")
	wantPGCode(t, fx.do(fx.runID, escapeSessionID(), closeBody(counted, "defect")), http.StatusUnprocessableEntity, "escape_target_not_pending")
	wantPGCode(t, fx.do(fx.runID, escapeSessionID(), closeBody(counted+1000, "defect")), http.StatusUnprocessableEntity, "escape_target_not_found")
	otherRun, _ := fx.createRun(t, context.Background())
	pending := pgBirth(t, fx, escapeStaticID(), "pending")
	wantPGCode(t, fx.do(otherRun, escapeSessionID(), closeBody(pending, "defect")), http.StatusUnprocessableEntity, "escape_target_not_found")
	if n := fx.countRows(t, fx.runID, escapeRecordKindClosure) + fx.countRows(t, otherRun, escapeRecordKindClosure); n != 0 {
		t.Errorf("closure rows = %d, want 0", n)
	}
}

// R5 / CF-B12 on the real repository: GetByIDs returns a WRAPPED
// concern.ErrNotFound for a missing id, mapped to 422; a real concern records.
func TestRecordEscape_PG_MissingConcernIs422(t *testing.T) {
	fx := newEscapePGFixture(t)
	wantPGCode(t, fx.do(fx.runID, escapeSessionID(), laterReviewerBody(uuid.New())), http.StatusUnprocessableEntity, "escape_concern_not_found")
	rows, err := fx.concerns.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: fx.runID, StageID: fx.stageID, StageKind: concern.StageKindImplement,
		ReviewerModel: "claude-fable-5-1", ReviewerRole: concern.ReviewerRoleStandard, OriginReviewSequence: 7,
		Concerns: []concern.RaisedConcern{{Severity: "high", Category: "correctness", Note: "a"}, {Severity: "low", Category: "correctness", Note: "b"}},
	})
	if err != nil {
		t.Fatalf("InsertRaised: %v", err)
	}
	// Two concerns of ONE verdict share OriginReviewSequence 7; each records.
	for _, c := range rows {
		resp := decodeEscapeResponse(t, fx.do(fx.runID, escapeSessionID(), laterReviewerBody(c.ID)))
		if resp.SourceEventKey != "concern:"+c.ID.String() {
			t.Errorf("source_event_key = %q, want concern:%s", resp.SourceEventKey, c.ID)
		}
	}
	if n := fx.countRows(t, fx.runID, escapeRecordKindBirth); n != 2 {
		t.Errorf("births = %d, want 2", n)
	}
}
