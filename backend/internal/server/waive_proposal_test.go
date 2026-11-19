package server

// Waive proposals (E80.28 / #5229, ADR-143 A1): the chain fold, the
// recorded-head derivation, applyConcernWaiveLinked and the propose handler.
//
// Every test seeds state BY CONSTRUCTION (a fake concern row with provenance
// server_check, a pull_request_opened head row, fixup_pushed rows and
// concern_waive_proposed rows on the guardedAuditWrapper's chain) and reads
// committed state back after the call: the row state and the audit rows per
// category. A response code alone never decides a test.
//
// COUNTERFACTUALS (hand-run; each mutation restored byte-identically):
//
//   - H6 (V2 failed-intent reopen, waive_proposal.go): the concern_waive_failed
//     case's `st.live[p.ConcernID] = p` replaced by `_ = p` in pass 2. Fixture:
//     intent concern_waived{proposal_sequence:P} then
//     concern_waive_failed{intent_sequence: that intent}. Observed RED:
//     TestWaiveProposalFold_FailedIntentLeavesLive "live proposal = <nil>, want P"
//     and TestWaiveProposalFold_LegacyFailedMatchesNearestIntent.
//   - H7 (acknowledged proposal keeps its place, waive_proposal.go): the reopen's
//     `st.live[p.ConcernID] == nil` replaced by `true`. Fixture: P, intent(P),
//     Q, concern_waive_failed(intent). Observed RED:
//     TestWaiveProposalFold_ProposalDuringFailedIntentStaysLive "live proposal =
//     P (seq 2), want Q (seq 4)".
//   - run-bound refusal (handleProposeWaive): the runBoundTokenRunID branch made
//     unreachable. The token names the concern's OWN run and holds write:fixups,
//     so nothing else refuses: _RunBoundTokenRefused RED (201, one proposed row).
//   - in-lock one-live check (propose guard): `return waiveProposalExistsError{...}`
//     replaced by nil. The hook inserts a competitor after the pre-check:
//     _ConcurrentProposerRefused RED (201, two proposed rows).
//   - in-lock head check (propose guard): `cur != head` replaced by false. The
//     hook records fixup_pushed{B} after the pre-check: _HeadMovedUnderLockRefused
//     RED (201, one proposed row).
//   - credential scan: `len(hits) > 0` replaced by false: _CredentialInTextRefused
//     RED on all three fields (201, proposed row carrying the bytes).
//   - fail closed without the capability: the propose `!ok` branch made
//     unreachable: _NoGuardedAppenderFailsClosed RED (nil-interface panic
//     recovered as a test failure). In applyConcernWaiveLinked, the `!ok` return
//     removed: _NoCapabilityTypedError RED (panic on the nil appender).
//   - guard refusal pass-through (applyConcernWaiveLinked): the `return nil, aerr`
//     refusal branch removed: _GuardRefusalWritesNoIntentNoCorrective RED (the
//     refusal wrapped as concernWaiveAuditAppendError; errors.As on the typed
//     sentinel fails).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// guardedAuditWrapper embeds the audit fake and adds audit.GuardedChainAppender
// under a mutex (guard against the inner fake, then the inner append). Every
// append returns its 1-based chain sequence, and ListForRunByCategory returns
// the actor fields and sequences, the way the real chain does. A one-shot
// beforeAppend hook fires on the first append of a target category through
// EITHER AppendChained or AppendChainedGuarded, AFTER the handler's pre-check
// and BEFORE the guard, so a push or a competing row can be injected into that
// window. Slices 3 and 4 reuse it.
type guardedAuditWrapper struct {
	*auditFake
	gmu sync.Mutex

	hmu          sync.Mutex
	hookCategory string
	hook         func()
	hookFired    bool
}

func newGuardedAuditWrapper() *guardedAuditWrapper {
	inner := newAuditFake()
	inner.stampSequence = true
	return &guardedAuditWrapper{auditFake: inner}
}

// beforeAppend arms the one-shot hook for category.
func (w *guardedAuditWrapper) beforeAppend(category string, hook func()) {
	w.hmu.Lock()
	defer w.hmu.Unlock()
	w.hookCategory, w.hook, w.hookFired = category, hook, false
}

func (w *guardedAuditWrapper) fireHook(category string) {
	w.hmu.Lock()
	var h func()
	if w.hook != nil && !w.hookFired && category == w.hookCategory {
		w.hookFired = true
		h = w.hook
	}
	w.hmu.Unlock()
	if h != nil {
		h()
	}
}

func (w *guardedAuditWrapper) appendLocked(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	e, err := w.auditFake.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	e.Sequence = int64(len(w.appended))
	w.mu.Unlock()
	e.Category, e.Payload, e.ActorKind, e.ActorSubject = p.Category, p.Payload, p.ActorKind, p.ActorSubject
	return e, nil
}

func (w *guardedAuditWrapper) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	w.fireHook(p.Category)
	w.gmu.Lock()
	defer w.gmu.Unlock()
	return w.appendLocked(ctx, p)
}

// wrapperTxReader is the guard's reader, scoped to the locked run like the
// production tx reader.
type wrapperTxReader struct {
	w     *guardedAuditWrapper
	runID uuid.UUID
}

func (r wrapperTxReader) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if runID != r.runID {
		return nil, audit.ErrGuardReaderRunScope
	}
	return r.w.ListForRunByCategory(ctx, runID, category)
}

func (w *guardedAuditWrapper) AppendChainedGuarded(ctx context.Context, p audit.ChainAppendParams, guard audit.AppendGuard) (*audit.Entry, error) {
	if guard == nil {
		return nil, audit.ErrNilAppendGuard
	}
	w.fireHook(p.Category)
	w.gmu.Lock()
	defer w.gmu.Unlock()
	if err := guard(ctx, wrapperTxReader{w: w, runID: p.RunID}); err != nil {
		return nil, err
	}
	return w.appendLocked(ctx, p)
}

// ListForRunByCategory returns appended entries with sequence and actor fields.
func (w *guardedAuditWrapper) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a := w.auditFake
	if a.listByCategoryErr != nil {
		return nil, a.listByCategoryErr
	}
	if a.listByCategoryErrCategory != "" && category == a.listByCategoryErrCategory {
		return nil, errors.New("guardedAuditWrapper: injected ListForRunByCategory error for " + category)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for i, ap := range a.appended {
		if ap.RunID != runID || ap.Category != category {
			continue
		}
		rid := ap.RunID
		out = append(out, &audit.Entry{
			RunID: &rid, StageID: ap.StageID, Timestamp: ap.Timestamp, Category: ap.Category,
			Payload: ap.Payload, ActorKind: ap.ActorKind, ActorSubject: ap.ActorSubject, Sequence: int64(i + 1),
		})
	}
	return out, nil
}

// seed appends a row directly (no hook), returning it with its sequence.
func (w *guardedAuditWrapper) seed(t *testing.T, runID uuid.UUID, category string, fields map[string]any) *audit.Entry {
	t.Helper()
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("seed %s: marshal: %v", category, err)
	}
	w.gmu.Lock()
	defer w.gmu.Unlock()
	e, err := w.appendLocked(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: category, Payload: payload,
	})
	if err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
	return e
}

// payloads returns the decoded payloads of category, in append order.
func (w *guardedAuditWrapper) payloads(t *testing.T, category string) []map[string]any {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, ap := range w.appended {
		if ap.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ap.Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		out = append(out, m)
	}
	return out
}

func (w *guardedAuditWrapper) count(category string) int {
	return len(auditEntriesByCategory(w.auditFake, category))
}

const (
	proposalHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	proposalHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type proposeFixture struct {
	s       *Server
	au      *guardedAuditWrapper
	cr      *fakeConcernRepo
	runID   uuid.UUID
	stageID uuid.UUID
	row     *concern.Concern
}

// newProposeFixture seeds a raised high server_check row and a
// pull_request_opened row naming head A.
func newProposeFixture(t *testing.T) *proposeFixture {
	t.Helper()
	au := newGuardedAuditWrapper()
	cr := newFakeConcernRepo()
	f := &proposeFixture{
		s: New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr}), au: au, cr: cr,
		runID: uuid.New(), stageID: uuid.New(),
	}
	f.row = seedServerCheckRow(t, cr, f.runID, f.stageID, "high")
	au.seed(t, f.runID, "pull_request_opened", map[string]any{"head_sha": proposalHeadA})
	return f
}

// seedProposal appends a concern_waive_proposed row for concernID at head.
func (f *proposeFixture) seedProposal(t *testing.T, concernID uuid.UUID, head string) *audit.Entry {
	t.Helper()
	return f.au.seed(t, f.runID, CategoryConcernWaiveProposed, map[string]any{
		"concern_id": concernID.String(), "reason": "seeded", "evidence": "seeded", "judgement": "no_defect", "head_sha": head,
	})
}

func validProposeBody() map[string]any {
	return map[string]any{
		"reason":    "the matched string is a documented placeholder, not a live credential",
		"evidence":  "config/settings.go:2 assigns the README's example token; no forge accepts it",
		"judgement": "no_defect",
	}
}

func postPropose(t *testing.T, s *Server, concernID string, body any, auth func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+concernID+"/waive-proposal", strings.NewReader(string(raw)))
	req.SetPathValue("concern_id", concernID)
	w := httptest.NewRecorder()
	s.requireConcernAccount(memberWrite, s.handleProposeWaive)(w, auth(req))
	return w
}

func assertErrCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) errorBody {
	t.Helper()
	if w.Code != status {
		t.Errorf("status = %d, want %d %s:\n%s", w.Code, status, code, w.Body.String())
		return errorBody{}
	}
	env := decodeErrorEnvelope(t, w)
	if env.Code != code {
		t.Errorf("code = %q, want %q:\n%s", env.Code, code, w.Body.String())
	}
	return env
}

func foldState(t *testing.T, f *proposeFixture) *waiveProposalState {
	t.Helper()
	st, err := loadWaiveProposalState(context.Background(), f.au, f.runID)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	return st
}

// ---- Fold units ----

func TestWaiveProposalFold_FirstLiveWins(t *testing.T) {
	f := newProposeFixture(t)
	p1 := f.seedProposal(t, f.row.ID, proposalHeadA)
	f.seedProposal(t, f.row.ID, proposalHeadA)
	st := foldState(t, f)
	if live := st.Live(f.row.ID); live == nil || live.Sequence != p1.Sequence {
		t.Fatalf("live = %+v, want the first proposal (seq %d)", live, p1.Sequence)
	}
	if st.inert[f.row.ID] != 1 {
		t.Errorf("inert count = %d, want 1", st.inert[f.row.ID])
	}
}

func TestWaiveProposalFold_RejectedClosesLive(t *testing.T) {
	testFoldCloser(t, CategoryConcernWaiveProposalRejected)
}

func TestWaiveProposalFold_DroppedClosesLive(t *testing.T) {
	testFoldCloser(t, CategoryConcernWaiveProposalDropped)
}

func testFoldCloser(t *testing.T, category string) {
	t.Helper()
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	// A closer naming another sequence leaves it live.
	f.au.seed(t, f.runID, category, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence + 100})
	if foldState(t, f).Live(f.row.ID) == nil {
		t.Fatalf("%s naming another sequence closed the live proposal", category)
	}
	f.au.seed(t, f.runID, category, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence})
	if live := foldState(t, f).Live(f.row.ID); live != nil {
		t.Errorf("%s naming the live sequence left it live: %+v", category, live)
	}
}

func TestWaiveProposalFold_LinkedWaiveClosesLive(t *testing.T) {
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	// An UNLINKED waive does not close it.
	f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{"concern_id": f.row.ID.String()})
	if foldState(t, f).Live(f.row.ID) == nil {
		t.Fatal("an unlinked concern_waived closed the proposal")
	}
	f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence})
	if live := foldState(t, f).Live(f.row.ID); live != nil {
		t.Errorf("a linked concern_waived left the proposal live: %+v", live)
	}
}

// TestWaiveProposalFold_FailedIntentLeavesLive is V2: an intent whose
// disposition failed (concern_waive_failed naming it) does not close the
// proposal. H6 counterfactual target.
func TestWaiveProposalFold_FailedIntentLeavesLive(t *testing.T) {
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	intent := f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence})
	f.au.seed(t, f.runID, CategoryConcernWaiveFailed, map[string]any{"concern_id": f.row.ID.String(), "intent_sequence": intent.Sequence})
	if live := foldState(t, f).Live(f.row.ID); live == nil || live.Sequence != p.Sequence {
		t.Fatalf("live proposal = %+v, want seq %d (a failed intent must leave the proposal live)", live, p.Sequence)
	}
}

// TestWaiveProposalFold_ProposalDuringFailedIntentStaysLive: a proposal Q
// recorded between P's linked intent and the corrective row naming that intent
// keeps its place; P stays closed and Q never turns inert. A rejection of Q
// before the corrective row lets P reopen.
func TestWaiveProposalFold_ProposalDuringFailedIntentStaysLive(t *testing.T) {
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	intent := f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence})
	q := f.seedProposal(t, f.row.ID, proposalHeadA)
	f.au.seed(t, f.runID, CategoryConcernWaiveFailed, map[string]any{"concern_id": f.row.ID.String(), "intent_sequence": intent.Sequence})
	st := foldState(t, f)
	if live := st.Live(f.row.ID); live == nil || live.Sequence != q.Sequence {
		t.Fatalf("live proposal = %+v, want Q (seq %d), not P (seq %d)", live, q.Sequence, p.Sequence)
	}
	if n := st.inert[f.row.ID]; n != 0 {
		t.Errorf("inert = %d, want 0", n)
	}

	g := newProposeFixture(t)
	p2 := g.seedProposal(t, g.row.ID, proposalHeadA)
	intent2 := g.au.seed(t, g.runID, CategoryConcernWaived, map[string]any{"concern_id": g.row.ID.String(), "proposal_sequence": p2.Sequence})
	q2 := g.seedProposal(t, g.row.ID, proposalHeadA)
	g.au.seed(t, g.runID, CategoryConcernWaiveProposalRejected, map[string]any{"concern_id": g.row.ID.String(), "proposal_sequence": q2.Sequence})
	g.au.seed(t, g.runID, CategoryConcernWaiveFailed, map[string]any{"concern_id": g.row.ID.String(), "intent_sequence": intent2.Sequence})
	if live := foldState(t, g).Live(g.row.ID); live == nil || live.Sequence != p2.Sequence {
		t.Fatalf("live proposal = %+v, want P reopened (seq %d) once Q was rejected", live, p2.Sequence)
	}
}

func TestWaiveProposalFold_FailedOtherIntentDoesNotReopen(t *testing.T) {
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	other := f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{"concern_id": f.row.ID.String()})
	f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence})
	// The failure names the OTHER (unlinked) intent, so the linked one stands.
	f.au.seed(t, f.runID, CategoryConcernWaiveFailed, map[string]any{"concern_id": f.row.ID.String(), "intent_sequence": other.Sequence})
	if live := foldState(t, f).Live(f.row.ID); live != nil {
		t.Errorf("a failure naming another intent kept the proposal live: %+v", live)
	}
}

func TestWaiveProposalFold_LegacyFailedMatchesNearestIntent(t *testing.T) {
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence})
	// Legacy corrective row: no intent_sequence key.
	f.au.seed(t, f.runID, CategoryConcernWaiveFailed, map[string]any{"concern_id": f.row.ID.String(), "actual_state": "raised"})
	if live := foldState(t, f).Live(f.row.ID); live == nil {
		t.Fatal("a legacy concern_waive_failed did not match the nearest preceding intent; the proposal was closed")
	}
}

func TestWaiveProposalFold_OtherConcernAndUndecodableSkipped(t *testing.T) {
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	other := uuid.New()
	f.au.seed(t, f.runID, CategoryConcernWaiveProposalRejected, map[string]any{"concern_id": other.String(), "proposal_sequence": p.Sequence})
	f.au.seed(t, f.runID, CategoryConcernWaiveProposalRejected, map[string]any{"concern_id": "not-a-uuid", "proposal_sequence": p.Sequence})
	// An undecodable payload (proposal_sequence of the wrong type).
	f.au.seed(t, f.runID, CategoryConcernWaiveProposalRejected, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": "x"})
	st := foldState(t, f)
	if live := st.Live(f.row.ID); live == nil || live.Sequence != p.Sequence {
		t.Fatalf("live = %+v, want seq %d (other-concern and undecodable rows must be skipped)", live, p.Sequence)
	}
	if st.Live(other) != nil {
		t.Error("the other concern has a live proposal")
	}
}

func TestRecordedRunHead_SourcesAndRebaseRules(t *testing.T) {
	ctx := context.Background()
	f := newProposeFixture(t)
	if h, _ := recordedRunHead(ctx, f.au, f.runID); h != proposalHeadA {
		t.Fatalf("head = %q, want A from pull_request_opened", h)
	}
	f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	if h, _ := recordedRunHead(ctx, f.au, f.runID); h != proposalHeadB {
		t.Fatalf("head = %q, want B from the newer fixup_pushed", h)
	}
	f.au.seed(t, f.runID, CategoryBranchRebased, map[string]any{"already_up_to_date": true, "new_head_sha": "cccc"})
	if h, _ := recordedRunHead(ctx, f.au, f.runID); h != proposalHeadB {
		t.Errorf("head = %q, want B (an already_up_to_date rebase moves no head)", h)
	}
	f.au.seed(t, f.runID, CategoryBranchRebased, map[string]any{"merge_commit_sha": "dddd"})
	if h, _ := recordedRunHead(ctx, f.au, f.runID); h != "dddd" {
		t.Errorf("head = %q, want dddd (merge_commit_sha fallback)", h)
	}
	if h, _ := recordedRunHead(ctx, f.au, uuid.New()); h != "" {
		t.Errorf("head of an unrecorded run = %q, want empty", h)
	}
}

// ---- applyConcernWaiveLinked ----

func TestApplyConcernWaiveLinked_LinkKeysOnlyWhenLinked(t *testing.T) {
	f := newProposeFixture(t)
	link := &waiveProposalLink{ProposalSequence: 7, ProposedBy: operatorAgentSubject, ProposedByActorKind: "agent", ProposalHeadSHA: proposalHeadA}
	admit := func(context.Context, audit.ChainReader) error { return nil }
	if _, err := f.s.applyConcernWaiveLinked(context.Background(), f.row, "human reason", "github:alice", audit.ActorUser, "", false, false,
		concern.JudgementNoDefect, "", link, admit); err != nil {
		t.Fatalf("linked waive: %v", err)
	}
	other := seedServerCheckRow(t, f.cr, f.runID, f.stageID, "high")
	if _, err := f.s.applyConcernWaive(context.Background(), other, "direct", "github:alice", audit.ActorUser, "", false, false,
		concern.JudgementNoDefect, ""); err != nil {
		t.Fatalf("direct waive: %v", err)
	}
	ps := f.au.payloads(t, CategoryConcernWaived)
	if len(ps) != 2 {
		t.Fatalf("concern_waived rows = %d, want 2", len(ps))
	}
	want := map[string]any{"proposal_sequence": float64(7), "proposed_by": operatorAgentSubject, "proposed_by_actor_kind": "agent", "proposal_head_sha": proposalHeadA}
	for k, v := range want {
		if ps[0][k] != v {
			t.Errorf("linked payload %s = %v, want %v", k, ps[0][k], v)
		}
		if _, ok := ps[1][k]; ok {
			t.Errorf("direct payload carries link key %s", k)
		}
	}
	assertState(t, f.cr, f.row.ID, concern.StateWaived)
}

type sentinelGuardRefusal struct{}

func (sentinelGuardRefusal) Error() string { return "sentinel guard refusal" }

func TestApplyConcernWaiveLinked_GuardRefusalWritesNoIntentNoCorrective(t *testing.T) {
	f := newProposeFixture(t)
	refuse := func(context.Context, audit.ChainReader) error { return sentinelGuardRefusal{} }
	_, err := f.s.applyConcernWaiveLinked(context.Background(), f.row, "human reason", "github:alice", audit.ActorUser, "", false, false,
		concern.JudgementNoDefect, "", &waiveProposalLink{ProposalSequence: 1}, refuse)
	var got sentinelGuardRefusal
	if !errors.As(err, &got) {
		t.Errorf("err = %v (%T), want the guard's typed refusal unwrapped", err, err)
	}
	var appendErr concernWaiveAuditAppendError
	if errors.As(err, &appendErr) {
		t.Errorf("err = %v is an audit-append failure; a guard refusal must not be", err)
	}
	if n := f.au.count(CategoryConcernWaived); n != 0 {
		t.Errorf("concern_waived rows = %d, want 0", n)
	}
	if n := f.au.count(CategoryConcernWaiveFailed); n != 0 {
		t.Errorf("concern_waive_failed rows = %d, want 0 (no intent, nothing to correct)", n)
	}
	assertState(t, f.cr, f.row.ID, concern.StateRaised) // ApplyHumanDisposition never ran
}

func TestApplyConcernWaiveLinked_NoCapabilityTypedError(t *testing.T) {
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	row := seedServerCheckRow(t, cr, uuid.New(), uuid.New(), "high")
	admit := func(context.Context, audit.ChainReader) error { return nil }
	_, err := s.applyConcernWaiveLinked(context.Background(), row, "human reason", "github:alice", audit.ActorUser, "", false, false,
		concern.JudgementNoDefect, "", &waiveProposalLink{ProposalSequence: 1}, admit)
	if !errors.Is(err, errGuardedAppendUnavailable) {
		t.Errorf("err = %v, want errGuardedAppendUnavailable", err)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived rows = %d, want 0 (no non-atomic fallback)", n)
	}
	assertState(t, cr, row.ID, concern.StateRaised)
}

// TestWaiveConcern_DirectPayloadUnchanged: a direct waive carries none of the
// link keys, and its corrective row names the intent it corrects.
func TestWaiveConcern_DirectPayloadUnchanged(t *testing.T) {
	f := newProposeFixture(t)
	reviewer, err := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.runID, StageID: f.stageID, StageKind: concern.StageKindImplement, OriginReviewSequence: 1,
		ReviewerModel: "m", Concerns: []concern.RaisedConcern{{Severity: "low", Category: "style", Note: "n"}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := postWaiveAs(t, f.s, reviewer[0].ID.String(), waiveConcernRequest{Reason: "not a defect"}, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("direct waive status = %d: %s", w.Code, w.Body.String())
	}
	ps := f.au.payloads(t, CategoryConcernWaived)
	for _, k := range []string{"proposal_sequence", "proposed_by", "proposed_by_actor_kind", "proposal_head_sha"} {
		if _, ok := ps[0][k]; ok {
			t.Errorf("direct waive payload carries link key %s", k)
		}
	}
	// A failing disposition: the corrective row carries intent_sequence.
	f.cr.applyHumanDispositionErr = errors.New("transient store failure")
	second, _ := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.runID, StageID: f.stageID, StageKind: concern.StageKindImplement, OriginReviewSequence: 1,
		ReviewerModel: "m", Concerns: []concern.RaisedConcern{{Severity: "low", Category: "style", Note: "n2"}},
	})
	if w := postWaiveAs(t, f.s, second[0].ID.String(), waiveConcernRequest{Reason: "not a defect"}, withAuth); w.Code == http.StatusOK {
		t.Fatalf("waive with a failing store succeeded: %s", w.Body.String())
	}
	intents := auditEntriesByCategory(f.au.auditFake, CategoryConcernWaived)
	failed := f.au.payloads(t, CategoryConcernWaiveFailed)
	if len(failed) != 1 || len(intents) != 2 {
		t.Fatalf("failed rows = %d, intents = %d; want 1 and 2", len(failed), len(intents))
	}
	if got, want := failed[0]["intent_sequence"], float64(intents[1]+1); got != want {
		t.Errorf("corrective intent_sequence = %v, want %v (the second intent's sequence)", got, want)
	}
}

// ---- Propose ----

func TestWaiveProposalPropose_RecordsPendingProposal(t *testing.T) {
	f := newProposeFixture(t)
	w := postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp proposeWaiveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ProposedBy != operatorAgentSubject || resp.CredentialKind == "" || !strings.Contains(resp.NextStep, "Web UI") {
		t.Errorf("echo = %q/%q next_step %q", resp.ProposedBy, resp.CredentialKind, resp.NextStep)
	}
	if !resp.Proposal.AgentAuthored || resp.Proposal.HeadSHA != proposalHeadA {
		t.Errorf("proposal view = %+v", resp.Proposal)
	}
	if n := f.au.count(CategoryConcernWaiveProposed); n != 1 {
		t.Fatalf("proposed rows = %d, want 1", n)
	}
	idx := auditEntriesByCategory(f.au.auditFake, CategoryConcernWaiveProposed)[0]
	ap := f.au.appended[idx]
	if ap.ActorKind == nil || *ap.ActorKind != audit.ActorAgent || ap.ActorSubject == nil || *ap.ActorSubject != operatorAgentSubject {
		t.Errorf("actor = %v/%v, want agent/%s", ap.ActorKind, ap.ActorSubject, operatorAgentSubject)
	}
	p := f.au.payloads(t, CategoryConcernWaiveProposed)[0]
	if p["head_sha"] != proposalHeadA || p["check_key"] != serverCheckKey || p["category"] != f.row.Category || p["provenance"] != concern.ProvenanceServerCheck {
		t.Errorf("payload = %v", p)
	}
	if resp.Proposal.ProposalSequence != int64(idx+1) {
		t.Errorf("proposal_sequence = %d, want %d", resp.Proposal.ProposalSequence, idx+1)
	}
	assertState(t, f.cr, f.row.ID, concern.StateRaised)
	if n := f.au.count(CategoryConcernWaived); n != 0 {
		t.Errorf("concern_waived rows = %d, want 0 (a proposal clears nothing)", n)
	}
	if live := foldState(t, f).Live(f.row.ID); live == nil || live.ProposedBy != operatorAgentSubject {
		t.Errorf("fold live = %+v, want the new proposal by the operator agent", live)
	}
}

func TestWaiveProposalPropose_MarkerConcernProposable(t *testing.T) {
	f := newProposeFixture(t)
	rows, err := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.runID, StageID: f.stageID, StageKind: concern.StageKindImplement, Provenance: concern.ProvenanceServerCheck,
		OriginReviewSequence: 9,
		Concerns: []concern.RaisedConcern{{Severity: "low", Category: "diff_secrets_marker_suppressed",
			Note: "allowlist marker suppressed a match", CheckKey: "diff_secrets_marker|config/x.go"}},
	})
	if err != nil {
		t.Fatalf("seed marker row: %v", err)
	}
	w := postPropose(t, f.s, rows[0].ID.String(), validProposeBody(), withOperatorAgentAuth)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	if p := f.au.payloads(t, CategoryConcernWaiveProposed); len(p) != 1 || p[0]["category"] != "diff_secrets_marker_suppressed" {
		t.Errorf("proposed payloads = %v", p)
	}
	assertState(t, f.cr, rows[0].ID, concern.StateRaised)
}

// assertNoProposal checks the refusal left no proposal and the row raised.
func assertNoProposal(t *testing.T, f *proposeFixture) {
	t.Helper()
	if n := f.au.count(CategoryConcernWaiveProposed); n != 0 {
		t.Errorf("proposed rows = %d, want 0", n)
	}
	assertState(t, f.cr, f.row.ID, concern.StateRaised)
}

func TestWaiveProposalPropose_BodyRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"ReasonRequired", func(b map[string]any) { b["reason"] = "  " }},
		{"EvidenceRequired", func(b map[string]any) { delete(b, "evidence") }},
		{"ReasonTooLong", func(b map[string]any) { b["reason"] = strings.Repeat("r", maxOperatorConcernBytes+1) }},
		{"EvidenceTooLong", func(b map[string]any) { b["evidence"] = strings.Repeat("e", maxOperatorConcernBytes+1) }},
		{"JudgementRequired", func(b map[string]any) { delete(b, "judgement") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProposeFixture(t)
			b := validProposeBody()
			tc.mutate(b)
			assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), b, withOperatorAgentAuth), http.StatusBadRequest, "validation_failed")
			assertNoProposal(t, f)
		})
	}
}

func TestWaiveProposalPropose_TooLongBoundaryAccepted(t *testing.T) {
	f := newProposeFixture(t)
	b := validProposeBody()
	b["reason"] = strings.Repeat("r", maxOperatorConcernBytes)
	b["evidence"] = strings.Repeat("e", maxOperatorConcernBytes)
	if w := postPropose(t, f.s, f.row.ID.String(), b, withOperatorAgentAuth); w.Code != http.StatusCreated {
		t.Fatalf("status = %d at exactly %d bytes, want 201: %s", w.Code, maxOperatorConcernBytes, w.Body.String())
	}
}

// generatedPAT returns a fresh string matching the github-pat-classic pattern.
func generatedPAT(t *testing.T) string {
	t.Helper()
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var sb strings.Builder
	sb.WriteString("ghp_")
	for i := 0; i < 36; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alnum))))
		if err != nil {
			t.Fatalf("rand: %v", err)
		}
		sb.WriteByte(alnum[n.Int64()])
	}
	return sb.String()
}

func TestWaiveProposalPropose_CredentialInTextRefused(t *testing.T) {
	for _, field := range []string{"reason", "evidence", "tradeoff_cost"} {
		t.Run(field, func(t *testing.T) {
			f := newProposeFixture(t)
			secret := generatedPAT(t)
			b := validProposeBody()
			if field == "tradeoff_cost" {
				b["judgement"] = "tradeoff"
			}
			b[field] = "the value " + secret + " is a fixture"
			w := postPropose(t, f.s, f.row.ID.String(), b, withOperatorAgentAuth)
			env := assertErrCode(t, w, http.StatusBadRequest, "waive_proposal_contains_credential")
			if env.Details["field"] != field {
				t.Errorf("details.field = %v, want %s", env.Details["field"], field)
			}
			if strings.Contains(w.Body.String(), secret) {
				t.Error("the response echoes the credential-shaped bytes")
			}
			assertNoProposal(t, f)
		})
	}
}

func TestWaiveProposalPropose_RunBoundTokenRefused(t *testing.T) {
	f := newProposeFixture(t)
	assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withRunBoundAuth(f.runID)),
		http.StatusForbidden, "run_token_forbidden")
	// An unknown concern id: refused before the handler's concern read, not 404.
	assertErrCode(t, postPropose(t, f.s, uuid.New().String(), validProposeBody(), withRunBoundAuth(f.runID)),
		http.StatusForbidden, "run_token_forbidden")
	assertNoProposal(t, f)
}

// TestWaiveProposalPropose_CrossAccountRefused (X5(a)): requireConcernAccount
// answers 403 account_forbidden before the handler for an operator agent and a
// run-bound token from account B on an account-A run; the same-account control
// proves the fixture reaches the handler.
func TestWaiveProposalPropose_CrossAccountRefused(t *testing.T) {
	repo := newFakeRepo()
	au := newGuardedAuditWrapper()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr})
	runID := seedGateRun(t, repo)
	repo.mu.Lock()
	repo.runs[runID].AccountID = authzAcctA
	repo.mu.Unlock()
	row := seedServerCheckRow(t, cr, runID, uuid.New(), "high")
	au.seed(t, runID, "pull_request_opened", map[string]any{"head_sha": proposalHeadA})

	agentB := operatorAgentIdentity()
	agentB.AccountID = authzAcctB
	runBoundB := Identity{Subject: "mcp:run:" + uuid.New().String(), TokenID: "tok-b", AccountID: authzAcctB, Scopes: []string{"write:fixups"}}
	for name, id := range map[string]Identity{"operator-agent": agentB, "run-bound": runBoundB} {
		id := id
		w := postPropose(t, s, row.ID.String(), validProposeBody(), func(r *http.Request) *http.Request { return withIdentity(r, id) })
		if w.Code != http.StatusForbidden || !bodyHasCode(w, "account_forbidden") {
			t.Errorf("%s: status = %d, want 403 account_forbidden: %s", name, w.Code, w.Body.String())
		}
	}
	if n := au.count(CategoryConcernWaiveProposed); n != 0 {
		t.Fatalf("proposed rows = %d after cross-account calls, want 0", n)
	}
	agentA := operatorAgentIdentity()
	agentA.AccountID = authzAcctA
	if w := postPropose(t, s, row.ID.String(), validProposeBody(), func(r *http.Request) *http.Request { return withIdentity(r, agentA) }); w.Code != http.StatusCreated {
		t.Errorf("same-account control status = %d, want 201: %s", w.Code, w.Body.String())
	}
}

func TestWaiveProposalPropose_ScopeRequired(t *testing.T) {
	f := newProposeFixture(t)
	id := operatorAgentIdentity()
	id.Scopes = []string{"read:runs"}
	assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), validProposeBody(), func(r *http.Request) *http.Request { return withIdentity(r, id) }),
		http.StatusForbidden, "insufficient_scope")
	assertNoProposal(t, f)
}

func TestWaiveProposalPropose_ReviewerConcernRefused(t *testing.T) {
	f := newProposeFixture(t)
	rows, _ := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.runID, StageID: f.stageID, StageKind: concern.StageKindImplement, OriginReviewSequence: 1,
		ReviewerModel: "m", Concerns: []concern.RaisedConcern{{Severity: "high", Category: "security", Note: "n"}},
	})
	assertErrCode(t, postPropose(t, f.s, rows[0].ID.String(), validProposeBody(), withOperatorAgentAuth),
		http.StatusUnprocessableEntity, "concern_not_server_check")
	assertNoProposal(t, f)
}

func TestWaiveProposalPropose_NotOpenRefused(t *testing.T) {
	f := newProposeFixture(t)
	if _, err := f.cr.ApplyHumanDisposition(context.Background(), f.row.ID, concern.StateWaived, "human waived", concern.HumanDisposition{
		Outcome: concern.OutcomeWaived, Judgement: concern.JudgementNoDefect, ActorKind: string(audit.ActorUser),
	}); err != nil {
		t.Fatalf("seed waived row: %v", err)
	}
	assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth), http.StatusConflict, "concern_not_open")
	if n := f.au.count(CategoryConcernWaiveProposed); n != 0 {
		t.Errorf("proposed rows = %d, want 0", n)
	}
}

func TestWaiveProposalPropose_HeadUnresolvedRefused(t *testing.T) {
	au := newGuardedAuditWrapper()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	row := seedServerCheckRow(t, cr, uuid.New(), uuid.New(), "high")
	assertErrCode(t, postPropose(t, s, row.ID.String(), validProposeBody(), withOperatorAgentAuth), http.StatusConflict, "waive_proposal_head_unresolved")
	if n := au.count(CategoryConcernWaiveProposed); n != 0 {
		t.Errorf("proposed rows = %d, want 0", n)
	}
}

func TestWaiveProposalPropose_ExpectedHeadMismatchRefused(t *testing.T) {
	f := newProposeFixture(t)
	b := validProposeBody()
	b["expected_head_sha"] = proposalHeadB
	assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), b, withOperatorAgentAuth), http.StatusConflict, "waive_proposal_head_moved")
	assertNoProposal(t, f)
}

func TestWaiveProposalPropose_HeadReadErrorFailsClosed(t *testing.T) {
	f := newProposeFixture(t)
	f.au.listByCategoryErrCategory = "pull_request_opened"
	assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth), http.StatusInternalServerError, "internal_error")
	assertNoProposal(t, f)
}

func TestWaiveProposalPropose_LiveProposalExistsRefused(t *testing.T) {
	f := newProposeFixture(t)
	p := f.seedProposal(t, f.row.ID, proposalHeadA)
	env := assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth), http.StatusConflict, "waive_proposal_exists")
	if env.Details["proposal_sequence"] != float64(p.Sequence) {
		t.Errorf("details.proposal_sequence = %v, want %d", env.Details["proposal_sequence"], p.Sequence)
	}
	if n := f.au.count(CategoryConcernWaiveProposed); n != 1 {
		t.Errorf("proposed rows = %d, want exactly 1", n)
	}
}

// TestWaiveProposalPropose_ConcurrentProposerRefused: a competitor lands after
// the pre-check; only the in-lock one-live check refuses.
func TestWaiveProposalPropose_ConcurrentProposerRefused(t *testing.T) {
	f := newProposeFixture(t)
	var competitor *audit.Entry
	f.au.beforeAppend(CategoryConcernWaiveProposed, func() { competitor = f.seedProposal(t, f.row.ID, proposalHeadA) })
	env := assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth), http.StatusConflict, "waive_proposal_exists")
	if competitor == nil {
		t.Fatal("the hook did not fire")
	}
	if env.Details["proposal_sequence"] != float64(competitor.Sequence) {
		t.Errorf("details.proposal_sequence = %v, want the competitor's %d", env.Details["proposal_sequence"], competitor.Sequence)
	}
	if n := f.au.count(CategoryConcernWaiveProposed); n != 1 {
		t.Errorf("proposed rows = %d, want exactly 1 (the competitor)", n)
	}
}

// TestWaiveProposalPropose_HeadMovedUnderLockRefused: a push lands after the
// pre-check; only the in-lock head re-read refuses.
func TestWaiveProposalPropose_HeadMovedUnderLockRefused(t *testing.T) {
	f := newProposeFixture(t)
	f.au.beforeAppend(CategoryConcernWaiveProposed, func() {
		f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	})
	env := assertErrCode(t, postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth), http.StatusConflict, "waive_proposal_head_moved")
	if env.Details["current_head_sha"] != proposalHeadB {
		t.Errorf("details.current_head_sha = %v, want B", env.Details["current_head_sha"])
	}
	assertNoProposal(t, f)
}

func TestWaiveProposalPropose_NoGuardedAppenderFailsClosed(t *testing.T) {
	au := newAuditFake()
	au.stampSequence = true
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	row := seedServerCheckRow(t, cr, uuid.New(), uuid.New(), "high")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: row.RunID, Category: "pull_request_opened",
		Payload: []byte(`{"head_sha":"` + proposalHeadA + `"}`)}); err != nil {
		t.Fatalf("seed head: %v", err)
	}
	assertErrCode(t, postPropose(t, s, row.ID.String(), validProposeBody(), withOperatorAgentAuth),
		http.StatusServiceUnavailable, "waive_proposal_atomic_append_unavailable")
	if n := len(auditEntriesByCategory(au, CategoryConcernWaiveProposed)); n != 0 {
		t.Errorf("proposed rows = %d, want 0", n)
	}
}

// TestWaiveProposalPropose_StaleLiveReplaced: a live proposal at A with the
// head moved to B is dropped (system actor), then the new proposal binds to B.
func TestWaiveProposalPropose_StaleLiveReplaced(t *testing.T) {
	f := newProposeFixture(t)
	old := f.seedProposal(t, f.row.ID, proposalHeadA)
	f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	w := postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	drops := auditEntriesByCategory(f.au.auditFake, CategoryConcernWaiveProposalDropped)
	if len(drops) != 1 {
		t.Fatalf("dropped rows = %d, want 1", len(drops))
	}
	ap := f.au.appended[drops[0]]
	if ap.ActorKind == nil || *ap.ActorKind != audit.ActorSystem {
		t.Errorf("drop actor = %v, want system", ap.ActorKind)
	}
	dp := f.au.payloads(t, CategoryConcernWaiveProposalDropped)[0]
	if dp["proposal_sequence"] != float64(old.Sequence) || dp["cause"] != "head_moved" ||
		dp["proposal_head_sha"] != proposalHeadA || dp["current_head_sha"] != proposalHeadB {
		t.Errorf("drop payload = %v", dp)
	}
	live := foldState(t, f).Live(f.row.ID)
	if live == nil || live.HeadSHA != proposalHeadB || live.Sequence == old.Sequence {
		t.Errorf("live = %+v, want the new proposal at B", live)
	}
}

// TestPendingWaiveProposals_OnlyAtCurrentHead: the read filter shows a live
// proposal only at the current recorded head and only on an open
// server_check row.
func TestPendingWaiveProposals_OnlyAtCurrentHead(t *testing.T) {
	f := newProposeFixture(t)
	f.seedProposal(t, f.row.ID, proposalHeadA)
	got, err := f.s.pendingWaiveProposals(context.Background(), f.runID, []*concern.Concern{f.row})
	if err != nil || got[f.row.ID] == nil {
		t.Fatalf("pending = %v, err %v; want the proposal at A", got, err)
	}
	f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	got, _ = f.s.pendingWaiveProposals(context.Background(), f.runID, []*concern.Concern{f.row})
	if got[f.row.ID] != nil {
		t.Errorf("a stale proposal is pending: %+v", got[f.row.ID])
	}
}

// TestWaiveProposalPropose_StaleDropSkippedWhenClosedUnderLock: another actor
// closes the stale proposal after the pre-check; the drop guard skips the drop
// (no dropped row) and the new proposal still lands at B.
func TestWaiveProposalPropose_StaleDropSkippedWhenClosedUnderLock(t *testing.T) {
	f := newProposeFixture(t)
	old := f.seedProposal(t, f.row.ID, proposalHeadA)
	f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	f.au.beforeAppend(CategoryConcernWaiveProposalDropped, func() {
		f.au.seed(t, f.runID, CategoryConcernWaiveProposalRejected, map[string]any{"concern_id": f.row.ID.String(), "proposal_sequence": old.Sequence})
	})
	w := postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	if n := f.au.count(CategoryConcernWaiveProposalDropped); n != 0 {
		t.Errorf("dropped rows = %d, want 0 (the proposal was already closed under the lock)", n)
	}
	if live := foldState(t, f).Live(f.row.ID); live == nil || live.HeadSHA != proposalHeadB {
		t.Errorf("live = %+v, want the new proposal at B", live)
	}
}
