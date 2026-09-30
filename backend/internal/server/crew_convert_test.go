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

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Finding -> concern conversion (E77.7 / #3741): the captain-only route that
// promotes an advisory crew finding into a binding concern. Every test reads
// COMMITTED STATE — the concern table, the crew_messages row and the audit
// chain — because a conversion that fires and is rolled back returns a
// byte-identical error.

// failingConcernRepo wraps a real concern repository and fails (or empties)
// InsertRaised, counting calls under a mutex (#3226).
type failingConcernRepo struct {
	concern.Repository
	mu        sync.Mutex
	insertErr error
	empty     bool
	calls     int
}

func (r *failingConcernRepo) InsertRaised(ctx context.Context, p concern.InsertRaisedParams) ([]*concern.Concern, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if r.insertErr != nil {
		return nil, r.insertErr
	}
	if r.empty {
		return nil, nil
	}
	return r.Repository.InsertRaised(ctx, p)
}

// failingStageRunRepo fails ListStagesForRun.
type failingStageRunRepo struct {
	run.Repository
	err error
}

func (r *failingStageRunRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, r.err
}

// convertAuditRepo fails AppendChained for the conversion categories only.
type convertAuditRepo struct {
	audit.Repository
	appendErr error
}

func (a *convertAuditRepo) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.appendErr != nil && (p.Category == CategoryCrewFindingConverted || p.Category == CategoryCrewFindingConvertFailed) {
		return nil, a.appendErr
	}
	return a.Repository.AppendChained(ctx, p)
}

// newConvertServer wires the conversion route over f's pg stores and a real
// concern repository.
func newConvertServer(f *crewPG, cr concern.Repository) *Server {
	return New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     run.NewPostgresRepository(f.pool),
		AuditRepo:   f.audit,
		ConcernRepo: cr,
		CrewMailbox: f.mailbox,
	})
}

func convertFinding(t *testing.T, s *Server, seq int64, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	q := strconv.FormatInt(seq, 10)
	return crewCall(t, s.handleConvertCrewFindingToConcern, http.MethodPost,
		"/v0/crew-messages/"+q+"/convert-to-concern", q, body, decorate)
}

func decodeConvert(t *testing.T, w *httptest.ResponseRecorder) crewConvertResponse {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("convert status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var resp crewConvertResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode convert: %v", err)
	}
	return resp
}

// concernsOn reads the run's concern rows (committed state).
func concernsOn(t *testing.T, f *crewPG, runID uuid.UUID) []*concern.Concern {
	t.Helper()
	rows, err := concern.NewPostgresRepository(f.pool).ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list concerns: %v", err)
	}
	return rows
}

func crewRowState(t *testing.T, f *crewPG, seq int64) crewmessage.State {
	t.Helper()
	row, err := f.mailbox.Store().Get(context.Background(), seq)
	if err != nil {
		t.Fatalf("get row %d: %v", seq, err)
	}
	return row.State
}

// requireNothingConverted asserts committed state after a refusal: the
// finding is still open, no concern exists, no conversion fact was appended.
func requireNothingConverted(t *testing.T, f *crewPG, runID uuid.UUID, seq int64) {
	t.Helper()
	if st := crewRowState(t, f, seq); st != crewmessage.StateOpen {
		t.Errorf("finding state = %s after a refusal, want open", st)
	}
	if got := concernsOn(t, f, runID); len(got) != 0 {
		t.Errorf("a refused conversion left %d concern row(s)", len(got))
	}
	if n := f.count(t, CategoryCrewFindingConverted) + f.count(t, CategoryCrewFindingConvertFailed); n != 0 {
		t.Errorf("a refused conversion appended %d conversion entr(ies)", n)
	}
}

// TestCrewConvert_ConvertsFindingIntoConcern (done-means + condition 1): an
// open run-anchored finding converts into ONE raised concern citing it, the
// finding is disposed accepted, and the chain carries exactly one
// crew_finding_converted entry naming that concern and no corrective entry.
func TestCrewConvert_ConvertsFindingIntoConcern(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	stageID := f.stageOf(t, runID)
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
	sent := f.send(t, crewDoc("finding", "", "planner", runAnchor(runID),
		`{"summary":"CONVERT-SENTINEL the cache key omits the tenant","severity":"high"}`), operator("write:stages"))

	resp := decodeConvert(t, convertFinding(t, s, sent.SentSequence, `{"reason":"captain adopts it"}`, operator("write:stages")))
	if resp.CrewMessage.State != string(crewmessage.StateAccepted) || resp.CrewMessage.SentSequence != sent.SentSequence {
		t.Errorf("crew_message = %+v, want the finding accepted", resp.CrewMessage)
	}

	rows := concernsOn(t, f, runID)
	if len(rows) != 1 {
		t.Fatalf("concern rows = %d, want exactly 1", len(rows))
	}
	c := rows[0]
	if c.ID != resp.Concern.ID || c.StageID != stageID || c.StageKind != concern.StageKindPlan ||
		c.OriginReviewSequence != sent.SentSequence || c.Severity != "high" ||
		c.Category != crewConvertedConcernCategory || c.State != concern.StateRaised {
		t.Errorf("concern row = %+v, response = %+v", c, resp.Concern)
	}
	if !strings.Contains(c.Note, "CONVERT-SENTINEL") || !strings.Contains(c.Note, "[crew_message:"+strconv.FormatInt(sent.SentSequence, 10)+"]") {
		t.Errorf("note does not quote the finding and cite its sequence: %q", c.Note)
	}
	open, err := concern.NewPostgresRepository(f.pool).ListOpenByRun(context.Background(), runID)
	if err != nil || len(open) != 1 {
		t.Errorf("open concerns = %d (err %v), want the converted concern to be open", len(open), err)
	}
	if st := crewRowState(t, f, sent.SentSequence); st != crewmessage.StateAccepted {
		t.Errorf("finding state = %s, want accepted", st)
	}

	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryCrewFindingConverted)
	if err != nil || len(entries) != 1 {
		t.Fatalf("crew_finding_converted entries = %d (err %v), want 1", len(entries), err)
	}
	var p crewFindingConvertedPayload
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.SentSequence != sent.SentSequence || p.ConcernID != c.ID.String() || p.StageKind != "plan" ||
		p.Severity != "high" || p.DispositionSequence == nil {
		t.Errorf("converted payload = %+v", p)
	}
	if entries[0].StageID == nil || *entries[0].StageID != stageID {
		t.Errorf("converted entry stamped with stage %v, want %s", entries[0].StageID, stageID)
	}
	if n := f.count(t, CategoryCrewFindingConvertFailed); n != 0 {
		t.Errorf("crew_finding_convert_failed entries = %d on a landed conversion, want 0", n)
	}
}

// TestCrewConvert_DefaultSeverityAndImplementStage: a finding with no
// severity converts at medium, attached to the run's NEWEST plan/implement
// stage (implement here), never to a review stage.
func TestCrewConvert_DefaultSeverityAndImplementStage(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	implID := f.seedStage(t, runID, 2, run.StageTypeImplement)
	f.seedStage(t, runID, 3, run.StageTypeReview)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RoleReviewer, "NOSEV")
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))

	resp := decodeConvert(t, convertFinding(t, s, seq, "", operator("write:stages")))
	if resp.Concern.Severity != crewConvertDefaultSeverity || resp.Concern.StageID != implID || resp.Concern.StageKind != concern.StageKindImplement {
		t.Errorf("concern = %+v, want medium on the implement stage %s", resp.Concern, implID)
	}
}

// TestCrewConvert_InsertFailureAppendsCorrective (condition 1): the finding is
// disposed, then the concern insert fails — the corrective
// crew_finding_convert_failed entry is appended, NO crew_finding_converted,
// no concern row, and the response is 500.
func TestCrewConvert_InsertFailureAppendsCorrective(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo func(concern.Repository) *failingConcernRepo
	}{
		{"insert error", func(r concern.Repository) *failingConcernRepo {
			return &failingConcernRepo{Repository: r, insertErr: errors.New("db down")}
		}},
		{"insert returned no row", func(r concern.Repository) *failingConcernRepo {
			return &failingConcernRepo{Repository: r, empty: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrewPG(t)
			runID := f.seedRun(t, run.StageTypePlan)
			seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "FAILCONV")
			cr := tc.repo(concern.NewPostgresRepository(f.pool))
			s := newConvertServer(f, cr)

			requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("write:stages")),
				http.StatusInternalServerError, "crew_finding_convert_failed")
			if cr.calls != 1 {
				t.Fatalf("InsertRaised calls = %d, want 1", cr.calls)
			}
			entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryCrewFindingConvertFailed)
			if err != nil || len(entries) != 1 {
				t.Fatalf("crew_finding_convert_failed entries = %d (err %v), want 1", len(entries), err)
			}
			var p crewFindingConvertFailedPayload
			if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.SentSequence != seq || p.ActualState != string(crewmessage.StateAccepted) || p.Error == "" || p.DispositionSequence == nil {
				t.Errorf("corrective payload = %+v", p)
			}
			if n := f.count(t, CategoryCrewFindingConverted); n != 0 {
				t.Errorf("crew_finding_converted entries = %d for a conversion that did not land, want 0", n)
			}
			if got := concernsOn(t, f, runID); len(got) != 0 {
				t.Errorf("concern rows = %d, want 0", len(got))
			}
			if st := crewRowState(t, f, seq); st != crewmessage.StateAccepted {
				t.Errorf("finding state = %s, want accepted (dispose runs FIRST)", st)
			}
		})
	}
}

// TestCrewConvert_AuditAppendFailureStillConverts: the conversion fact is
// best-effort — a failed append leaves the landed concern and a 201.
func TestCrewConvert_AuditAppendFailureStillConverts(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "APPENDFAIL")
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
	s.cfg.AuditRepo = &convertAuditRepo{Repository: f.audit, appendErr: errors.New("chain down")}

	decodeConvert(t, convertFinding(t, s, seq, "", operator("write:stages")))
	if got := concernsOn(t, f, runID); len(got) != 1 {
		t.Errorf("concern rows = %d, want 1", len(got))
	}
}

// TestCrewConvert_RefusesNonFinding (C13): each fixture is OTHERWISE
// convertible — open, in the caller's account — and differs only in the
// property the gate checks.
func TestCrewConvert_RefusesNonFinding(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
	ctx := context.Background()

	notice := seedCrewMail(t, f, runID, crewmessage.TypeNotice, crewmessage.RolePlanner, "NOTICE")
	root := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "ROOT")
	reply, err := f.mailbox.Send(ctx, crewmessage.SendParams{
		RawMessage:         []byte(crewDoc("finding", "architect", "planner", runAnchor(runID), `{"summary":"REPLY"}`)),
		Actor:              crewmessage.Actor{Kind: audit.ActorSystem, Subject: "test:architect"},
		ThreadRootSequence: &root,
	})
	if err != nil {
		t.Fatalf("seed reply: %v", err)
	}
	runLess, err := f.mailbox.Send(ctx, crewmessage.SendParams{
		RawMessage: []byte(crewDoc("finding", "architect", "planner", `{"issue_ref":"kuhlman-labs/fishhawk#1"}`, `{"summary":"RUNLESS"}`)),
		Actor:      crewmessage.Actor{Kind: audit.ActorSystem, Subject: "test:architect"},
	})
	if err != nil {
		t.Fatalf("seed run-less: %v", err)
	}

	for _, tc := range []struct {
		name, reason string
		seq          int64
	}{
		{"notice", "not_a_finding", notice},
		{"reply", "not_a_thread_root", reply.SentSequence},
		{"run-less", "run_less_anchor", runLess.SentSequence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := convertFinding(t, s, tc.seq, "", operator("write:stages"))
			requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "crew_message_not_finding")
			if !strings.Contains(w.Body.String(), tc.reason) {
				t.Errorf("refusal does not name %q: %s", tc.reason, w.Body.String())
			}
			if st := crewRowState(t, f, tc.seq); st != crewmessage.StateOpen {
				t.Errorf("state = %s, want open", st)
			}
		})
	}
	if got := concernsOn(t, f, runID); len(got) != 0 {
		t.Fatalf("a non-finding converted into %d concern(s)", len(got))
	}
	if n := f.count(t, CategoryCrewFindingConverted); n != 0 {
		t.Fatalf("crew_finding_converted entries = %d, want 0", n)
	}
}

// TestCrewConvert_RefusesRunBoundToken (C14): an mcp:run token on its OWN run
// holding write:stages — so neither a cross-run guard nor the scope check
// masks it — is refused self_decision and nothing is recorded.
func TestCrewConvert_RefusesRunBoundToken(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "RUNBOUND")
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))

	requireCrewRefusal(t, convertFinding(t, s, seq, "", runBound(runID, "write:stages", scopeWriteMessages)),
		http.StatusForbidden, "self_decision")
	requireNothingConverted(t, f, runID, seq)
}

// TestCrewConvert_SecondConversionRefused (C16): two sequential conversions —
// the second is 409 and exactly ONE concern row exists (committed state).
func TestCrewConvert_SecondConversionRefused(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "TWICE")
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))

	decodeConvert(t, convertFinding(t, s, seq, "", operator("write:stages")))
	requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("write:stages")),
		http.StatusConflict, "crew_message_already_disposed")
	if got := concernsOn(t, f, runID); len(got) != 1 {
		t.Fatalf("concern rows after two conversions = %d, want exactly 1", len(got))
	}
	if n := f.count(t, CategoryCrewFindingConverted); n != 1 {
		t.Fatalf("crew_finding_converted entries = %d, want 1", n)
	}
}

// TestCrewConvert_DisposedRefusedBeforeAnyRunRead isolates the row-state
// pre-check from Dispose's own ErrAlreadyDisposed (which would otherwise mask
// it with the same 409): the run has NO plan/implement stage, so without the
// pre-check the disposed finding would reach the stage read and answer 422
// crew_finding_no_concern_stage instead.
func TestCrewConvert_DisposedRefusedBeforeAnyRunRead(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypeReview)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RoleReviewer, "DISPOSED-EARLY")
	if _, err := f.mailbox.Dispose(context.Background(), crewmessage.DisposeParams{
		SentSequence: seq, Disposition: crewmessage.DispositionRejected,
		Actor: crewmessage.Actor{Kind: audit.ActorUser, Subject: "github:op"},
	}); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
	requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("write:stages")),
		http.StatusConflict, "crew_message_already_disposed")
}

// TestCrewConvert_DisposeFirstIsTheExactlyOnceGate isolates the ORDERING
// control from the row-state pre-check: the fixture is a racing converter's
// view — the chain already records a disposition while the derived row still
// reads open (constructed by resetting the projection) — so the pre-check
// passes and Dispose's refuse-before-append is the only thing in the path.
// It must refuse BEFORE any concern is inserted.
func TestCrewConvert_DisposeFirstIsTheExactlyOnceGate(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "RACE")
	ctx := context.Background()
	if _, err := f.mailbox.Dispose(ctx, crewmessage.DisposeParams{
		SentSequence: seq, Disposition: crewmessage.DispositionAccepted,
		Actor: crewmessage.Actor{Kind: audit.ActorUser, Subject: "github:other-captain"},
	}); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE crew_messages SET state = 'open', disposition_sequence = NULL,
		reason_sequence = NULL, disposed_at = NULL, round = 0, last_applied_sequence = sent_sequence
		WHERE sent_sequence = $1`, seq); err != nil {
		t.Fatalf("reset projection: %v", err)
	}
	if st := crewRowState(t, f, seq); st != crewmessage.StateOpen {
		t.Fatalf("fixture: row state = %s, want the stale open projection", st)
	}
	cr := &failingConcernRepo{Repository: concern.NewPostgresRepository(f.pool)}
	s := newConvertServer(f, cr)

	requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("write:stages")),
		http.StatusConflict, "crew_message_already_disposed")
	if cr.calls != 0 {
		t.Errorf("InsertRaised called %d time(s) for an already-disposed finding", cr.calls)
	}
	if got := concernsOn(t, f, runID); len(got) != 0 {
		t.Fatalf("concern rows = %d, want 0 — dispose must run FIRST", len(got))
	}
}

// TestCrewConvert_RefusalLadder covers the remaining named refusals, each
// asserting that nothing converted.
func TestCrewConvert_RefusalLadder(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "LADDER")
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))

	t.Run("anonymous", func(t *testing.T) {
		requireCrewRefusal(t, convertFinding(t, s, seq, "", nil), http.StatusUnauthorized, "authentication_required")
	})
	t.Run("operator without write:stages", func(t *testing.T) {
		requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("read:audit")), http.StatusForbidden, "insufficient_scope")
	})
	t.Run("bad sequence", func(t *testing.T) {
		w := crewCall(t, s.handleConvertCrewFindingToConcern, http.MethodPost, "/v0/crew-messages/x/convert-to-concern", "x", "", operator("write:stages"))
		requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
	})
	t.Run("malformed body", func(t *testing.T) {
		requireCrewRefusal(t, convertFinding(t, s, seq, `{"reason":1}`, operator("write:stages")), http.StatusBadRequest, "validation_failed")
	})
	t.Run("unknown body field", func(t *testing.T) {
		requireCrewRefusal(t, convertFinding(t, s, seq, `{"severity":"high"}`, operator("write:stages")), http.StatusBadRequest, "validation_failed")
	})
	t.Run("unknown sequence", func(t *testing.T) {
		requireCrewRefusal(t, convertFinding(t, s, seq+100000, "", operator("write:stages")), http.StatusNotFound, "crew_message_not_found")
	})
	requireNothingConverted(t, f, runID, seq)
}

// TestCrewConvert_AccountGate: a run-less finding in account A is not found
// for an identity in account B — and reaches the type gate for account A.
func TestCrewConvert_AccountGate(t *testing.T) {
	f := newCrewPG(t)
	ctx := context.Background()
	acctA, acctB := uuid.New(), uuid.New()
	for i, a := range []uuid.UUID{acctA, acctB} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, a, "crew-convert-"+strconv.Itoa(i)+"-"+a.String()); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	row, err := f.mailbox.Send(ctx, crewmessage.SendParams{
		RawMessage: []byte(crewDoc("finding", "architect", "planner", `{"issue_ref":"kuhlman-labs/fishhawk#2"}`, `{"summary":"ACCT"}`)),
		Actor:      crewmessage.Actor{Kind: audit.ActorSystem, Subject: "test:architect"},
		AccountID:  &acctA,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
	as := func(acct uuid.UUID) func(*http.Request) *http.Request {
		return func(r *http.Request) *http.Request {
			return injectIdentity(r, Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"write:stages"}, AccountID: acct.String()})
		}
	}
	requireCrewRefusal(t, convertFinding(t, s, row.SentSequence, "", as(acctB)), http.StatusNotFound, "crew_message_not_found")
	requireCrewRefusal(t, convertFinding(t, s, row.SentSequence, "", as(acctA)), http.StatusUnprocessableEntity, "crew_message_not_finding")
}

// TestCrewConvert_NoConcernStage: a run with no plan or implement stage has
// nothing a concern row may reference; refused before the dispose.
func TestCrewConvert_NoConcernStage(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypeReview)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RoleReviewer, "NOSTAGE")
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))

	requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("write:stages")),
		http.StatusUnprocessableEntity, "crew_finding_no_concern_stage")
	requireNothingConverted(t, f, runID, seq)
}

// TestCrewConvert_ReadFailuresRefuseBeforeDispose: a stage-list failure and an
// unresolvable document each answer 500 with the finding still open.
func TestCrewConvert_ReadFailuresRefuseBeforeDispose(t *testing.T) {
	t.Run("stage list", func(t *testing.T) {
		f := newCrewPG(t)
		runID := f.seedRun(t, run.StageTypePlan)
		seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "STAGEERR")
		s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
		s.cfg.RunRepo = &failingStageRunRepo{Repository: s.cfg.RunRepo, err: errors.New("db down")}
		requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("write:stages")), http.StatusInternalServerError, "internal_error")
		requireNothingConverted(t, f, runID, seq)
	})
	t.Run("unresolvable document", func(t *testing.T) {
		f := newCrewPG(t)
		runID := f.seedRun(t, run.StageTypePlan)
		seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "DOCERR")
		s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
		s.cfg.AuditRepo = &deliveryAuditRepo{Repository: f.audit, tamper: map[int64]bool{seq: true}}
		requireCrewRefusal(t, convertFinding(t, s, seq, "", operator("write:stages")), http.StatusInternalServerError, "internal_error")
		requireNothingConverted(t, f, runID, seq)
	})
}

// TestCrewConvert_Unconfigured: no mailbox, or no concern repository, is 503.
func TestCrewConvert_Unconfigured(t *testing.T) {
	noMailbox := New(Config{Addr: "127.0.0.1:0", RunRepo: newOrchestratorRepo(), AuditRepo: &auditCapture{}, ConcernRepo: newFakeConcernRepo()})
	requireCrewRefusal(t, convertFinding(t, noMailbox, 1, "", operator("write:stages")), http.StatusServiceUnavailable, "crew_message_unconfigured")
	noConcerns := New(Config{Addr: "127.0.0.1:0", RunRepo: newOrchestratorRepo(), AuditRepo: &auditCapture{}, CrewMailbox: crewmessage.NewMailbox(nil, 0)})
	requireCrewRefusal(t, convertFinding(t, noConcerns, 1, "", operator("write:stages")), http.StatusServiceUnavailable, "crew_message_unconfigured")
}

// TestCrewConvert_ConvertedFindingStopsDelivering (C15): after conversion the
// plan prompt no longer carries the finding and the gate view's crew block no
// longer lists it — the dispose is what drops it from both open-state reads.
func TestCrewConvert_ConvertedFindingStopsDelivering(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	s.cfg.ConcernRepo = concern.NewPostgresRepository(f.pool)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "STOPS-SENTINEL")
	requireInsideCrewEnvelopes(t, servePrompt(t, s, runID, stageID, priv), "STOPS-SENTINEL")

	decodeConvert(t, convertFinding(t, s, seq, "", operator("write:stages")))
	if got := servePrompt(t, s, runID, stageID, priv); strings.Contains(got, "STOPS-SENTINEL") {
		t.Fatalf("a converted finding is still delivered to the plan prompt")
	}
	if gv := decodeGateView(t, getGateView(t, s, runID, "")); len(gv.CrewMessages) != 0 {
		t.Fatalf("a converted finding is still in the gate view's crew block: %+v", gv.CrewMessages)
	}
}

// TestCrewConvert_ConvertedConcernRoundIsDerivedFromFindingSequence pins the
// stated risk: OriginReviewSequence is the finding's sent sequence, so an
// implement concern's gate-view round counts the fix-up triggers BELOW the
// finding's send — not the conversion.
func TestCrewConvert_ConvertedConcernRoundIsDerivedFromFindingSequence(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypeImplement)
	stageID := f.stageOf(t, runID)
	kind := audit.ActorSystem
	trigger := func() {
		if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: runID, StageID: &stageID, Category: CategoryStageFixupTriggered, ActorKind: &kind, Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("seed trigger: %v", err)
		}
	}
	trigger()
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RoleReviewer, "ROUND")
	trigger()
	s := newConvertServer(f, concern.NewPostgresRepository(f.pool))
	resp := decodeConvert(t, convertFinding(t, s, seq, "", operator("write:stages")))

	gv := decodeGateView(t, getGateView(t, s, runID, ""))
	for _, c := range gv.Open {
		if c.ID == resp.Concern.ID {
			if c.Round != 2 || c.OriginReviewSequence != seq {
				t.Fatalf("converted concern round = %d origin = %d, want round 2 derived from the finding's sequence %d", c.Round, c.OriginReviewSequence, seq)
			}
			return
		}
	}
	t.Fatalf("converted concern %s not in the gate view's open list: %+v", resp.Concern.ID, gv.Open)
}

// TestCrewDelivery_EndToEnd_SendThenPlanPromptThenGateViewThenConvert is the
// cross-boundary test: a finding POSTed over HTTP reaches the signed plan
// prompt and the gate view, converts into a concern over HTTP, and then leaves
// both crew surfaces while appearing as an open concern — one run, every seam.
func TestCrewDelivery_EndToEnd_SendThenPlanPromptThenGateViewThenConvert(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	s.cfg.ConcernRepo = concern.NewPostgresRepository(f.pool)
	sent := f.send(t, crewDoc("finding", "", "planner", runAnchor(runID),
		`{"summary":"E2E-CONVERT-SENTINEL","severity":"low"}`), operator("write:stages"))

	requireInsideCrewEnvelopes(t, servePrompt(t, s, runID, stageID, priv), "E2E-CONVERT-SENTINEL")
	before := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(before.CrewMessages) != 1 || before.CrewMessages[0].SentSequence != sent.SentSequence || len(before.Open) != 0 {
		t.Fatalf("gate view before conversion: crew = %+v, open = %d", before.CrewMessages, len(before.Open))
	}

	resp := decodeConvert(t, convertFinding(t, s, sent.SentSequence, "", operator("write:stages")))

	if got := servePrompt(t, s, runID, stageID, priv); strings.Contains(got, "E2E-CONVERT-SENTINEL") {
		t.Fatalf("the converted finding still reaches the plan prompt")
	}
	after := decodeGateView(t, getGateView(t, s, runID, ""))
	if len(after.CrewMessages) != 0 {
		t.Fatalf("the converted finding is still in the crew block: %+v", after.CrewMessages)
	}
	if len(after.Open) != 1 || after.Open[0].ID != resp.Concern.ID || after.Open[0].Category != crewConvertedConcernCategory {
		t.Fatalf("gate view open after conversion = %+v, want the converted concern", after.Open)
	}
}

func TestCrewConcernStage(t *testing.T) {
	plan := &run.Stage{ID: uuid.New(), Sequence: 1, Type: run.StageTypePlan}
	impl := &run.Stage{ID: uuid.New(), Sequence: 2, Type: run.StageTypeImplement}
	review := &run.Stage{ID: uuid.New(), Sequence: 3, Type: run.StageTypeReview}
	// plan listed FIRST, so a first-match pick (not newest-by-sequence) fails.
	if st, kind, ok := crewConcernStage([]*run.Stage{plan, nil, review, impl}); !ok || st != impl || kind != concern.StageKindImplement {
		t.Errorf("newest = %v %q %v, want implement", st, kind, ok)
	}
	if st, kind, ok := crewConcernStage([]*run.Stage{plan, review}); !ok || st != plan || kind != concern.StageKindPlan {
		t.Errorf("plan only = %v %q %v", st, kind, ok)
	}
	if _, _, ok := crewConcernStage([]*run.Stage{review}); ok {
		t.Error("a review-only run yielded a concern stage")
	}
}

func TestCrewConvertedNote(t *testing.T) {
	msg := &crewmessage.Message{SenderRole: crewmessage.RoleArchitect, Payload: crewmessage.Payload{Summary: "  x  "}}
	if got := crewConvertedNote(msg, 7); got != `Converted from crew finding (architect): "x" [crew_message:7]` {
		t.Errorf("note = %q", got)
	}
	msg.Payload.Summary = ""
	if got := crewConvertedNote(msg, 7); !strings.Contains(got, "no summary") {
		t.Errorf("blank-summary note = %q", got)
	}
}
