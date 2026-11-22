package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// holdAudit is the dispatch-hold tests' audit repository: an auditFake that
// stamps every appended entry with its 1-based append index as Sequence (on
// the append's return and on every category read), the way the real chain
// orders entries, and can fail the hold categories' reads in isolation.
// holdListFailAfter > 0 lets that many hold-category reads succeed and fails
// every later one (the verb's post-act re-read). beforeAppend runs before
// each verb append; appends are serialized so a concurrent append's Sequence
// is its own.
type holdAudit struct {
	*auditFake
	holdListErr       error
	holdListFailAfter int
	holdReads         int
	beforeAppend      func()
	appendMu          sync.Mutex
}

func newHoldAudit() *holdAudit { return &holdAudit{auditFake: &auditFake{stampSequence: true}} }

func (h *holdAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if h.beforeAppend != nil {
		h.beforeAppend()
	}
	h.appendMu.Lock()
	defer h.appendMu.Unlock()
	e, err := h.auditFake.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	e.Sequence = int64(len(h.appended))
	h.mu.Unlock()
	return e, nil
}

func (h *holdAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == CategoryRunDispatchHoldSet || category == CategoryRunDispatchHoldCleared {
		if h.holdListErr != nil {
			return nil, h.holdListErr
		}
		h.mu.Lock()
		h.holdReads++
		n := h.holdReads
		h.mu.Unlock()
		if h.holdListFailAfter > 0 && n > h.holdListFailAfter {
			return nil, errors.New("holdAudit: injected hold read error after the own-state read")
		}
	}
	return h.auditFake.ListForRunByCategory(ctx, runID, category)
}

// holdRows is the COMMITTED chain state: runID's hold rows, in append order.
func (h *holdAudit) holdRows(runID uuid.UUID) []audit.ChainAppendParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range h.appended {
		if p.RunID == runID && (p.Category == CategoryRunDispatchHoldSet || p.Category == CategoryRunDispatchHoldCleared) {
			out = append(out, p)
		}
	}
	return out
}

// seed appends one hold row for runID BY CONSTRUCTION (never through the verb
// under test).
func (h *holdAudit) seed(t *testing.T, runID uuid.UUID, category, reason string) {
	t.Helper()
	action := runDispatchHoldActionSet
	if category == CategoryRunDispatchHoldCleared {
		action = runDispatchHoldActionClear
	}
	raw, _ := json.Marshal(runDispatchHoldAuditPayload{Action: action, Reason: reason, Actor: "github:seed"})
	if _, err := h.auditFake.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	}); err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
}

type runHoldFixture struct {
	s   *Server
	rr  *orchestratorRepo
	aud *holdAudit
}

func newRunHoldFixture(t *testing.T) *runHoldFixture {
	t.Helper()
	f := &runHoldFixture{rr: newOrchestratorRepo(), aud: newHoldAudit()}
	f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr, AuditRepo: f.aud})
	return f
}

// childOf seeds a run decomposed from parent.
func (f *runHoldFixture) childOf(parent uuid.UUID) *run.Run {
	r := f.rr.seedRun()
	f.rr.mu.Lock()
	r.DecomposedFrom = &parent
	f.rr.mu.Unlock()
	return r
}

func (f *runHoldFixture) followUpOf(parent uuid.UUID) *run.Run {
	r := f.rr.seedRun()
	f.rr.mu.Lock()
	r.ParentRunID = &parent
	f.rr.mu.Unlock()
	return r
}

func (f *runHoldFixture) stage(runID uuid.UUID, typ run.StageType, state run.StageState) *run.Stage {
	st := f.rr.seedStage(runID, 0, state)
	f.rr.mu.Lock()
	st.Type = typ
	f.rr.mu.Unlock()
	return st
}

func (f *runHoldFixture) stageState(t *testing.T, id uuid.UUID) run.StageState {
	t.Helper()
	st, err := f.rr.GetStage(context.Background(), id)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	return st.State
}

func holdOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:runs"},
	}))
}

// postHold calls the verb's handler directly with withID's identity.
func (f *runHoldFixture) postHold(t *testing.T, runID, body string, withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/dispatch-hold", strings.NewReader(body))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	if withID != nil {
		req = withID(req)
	}
	f.s.handleRunDispatchHold(w, req)
	return w
}

func decodeHoldResponse(t *testing.T, w *httptest.ResponseRecorder) runDispatchHoldResponse {
	t.Helper()
	var resp runDispatchHoldResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return resp
}

// TestRunDispatchHold_SetAndClearAudited: set then clear each answer 200
// changed:true with an audit_sequence, and the chain READ after each call
// holds exactly the expected rows, reason and actor included.
func TestRunDispatchHold_SetAndClearAudited(t *testing.T) {
	f := newRunHoldFixture(t)
	r := f.rr.seedRun()

	w := f.postHold(t, r.ID.String(), `{"action":"set","reason":"wave 2 must wait for #5486"}`, holdOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("set = %d %s", w.Code, w.Body.String())
	}
	resp := decodeHoldResponse(t, w)
	if !resp.Changed || resp.AuditSequence == 0 || resp.DispatchHold == nil || resp.DispatchHold.Inherited ||
		resp.DispatchHold.HeldRunID != r.ID.String() || resp.DispatchHold.Reason != "wave 2 must wait for #5486" {
		t.Fatalf("set response = %+v / %+v", resp, resp.DispatchHold)
	}
	rows := f.aud.holdRows(r.ID)
	if len(rows) != 1 || rows[0].Category != CategoryRunDispatchHoldSet {
		t.Fatalf("chain after set = %+v, want one run_dispatch_hold_set", rows)
	}
	var p runDispatchHoldAuditPayload
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil || p.Reason != "wave 2 must wait for #5486" || p.Actor != "github:ops" || p.Action != "set" {
		t.Fatalf("set payload = %+v (%v)", p, err)
	}
	if rows[0].ActorSubject == nil || *rows[0].ActorSubject != "github:ops" || rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorUser {
		t.Fatalf("set actor = %v / %v", rows[0].ActorSubject, rows[0].ActorKind)
	}

	w = f.postHold(t, r.ID.String(), `{"action":"clear","reason":"predecessor integrated"}`, holdOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", w.Code, w.Body.String())
	}
	resp = decodeHoldResponse(t, w)
	if !resp.Changed || resp.AuditSequence == 0 || resp.DispatchHold != nil {
		t.Fatalf("clear response = %+v", resp)
	}
	rows = f.aud.holdRows(r.ID)
	if len(rows) != 2 || rows[1].Category != CategoryRunDispatchHoldCleared {
		t.Fatalf("chain after clear = %+v, want set then cleared", rows)
	}
	if err := json.Unmarshal(rows[1].Payload, &p); err != nil || p.Reason != "predecessor integrated" || p.Action != "clear" {
		t.Fatalf("clear payload = %+v (%v)", p, err)
	}
}

// TestRunDispatchHold_Refusals: one row per rung; each asserts status, code
// and that the committed chain gained no row.
func TestRunDispatchHold_Refusals(t *testing.T) {
	withIdent := func(id Identity) func(*http.Request) *http.Request {
		return func(req *http.Request) *http.Request {
			return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
		}
	}
	long := strings.Repeat("x", promoteReasonMax+1)
	type row struct {
		name     string
		body     string
		runID    func(f *runHoldFixture, r *run.Run) string
		withID   func(*http.Request) *http.Request
		setup    func(f *runHoldFixture, r *run.Run)
		noAudit  bool
		wantCode int
		wantErr  string
	}
	ok := `{"action":"set","reason":"hold it"}`
	self := func(_ *runHoldFixture, r *run.Run) string { return r.ID.String() }
	rows := []row{
		{name: "anonymous", body: ok, withID: nil, wantCode: http.StatusUnauthorized, wantErr: "authentication_required"},
		// The run-bound token carries write:runs, so no later rung refuses it.
		{name: "run-bound token", body: ok, wantCode: http.StatusForbidden, wantErr: "run_token_forbidden"},
		{name: "no write:runs", body: ok, withID: withIdent(Identity{Subject: "github:ro", TokenID: "tok-ro", Scopes: []string{"read:runs"}}),
			wantCode: http.StatusForbidden, wantErr: "insufficient_scope"},
		{name: "non-uuid run_id", body: ok, runID: func(*runHoldFixture, *run.Run) string { return "not-a-uuid" },
			wantCode: http.StatusBadRequest, wantErr: "validation_failed"},
		{name: "unknown action", body: `{"action":"pause","reason":"x"}`, wantCode: http.StatusBadRequest, wantErr: "validation_failed"},
		{name: "empty reason", body: `{"action":"set","reason":"   "}`, wantCode: http.StatusBadRequest, wantErr: "validation_failed"},
		{name: "over-1000-byte reason", body: `{"action":"set","reason":"` + long + `"}`, wantCode: http.StatusBadRequest, wantErr: "validation_failed"},
		{name: "unknown field", body: `{"action":"set","reason":"x","force":true}`, wantCode: http.StatusBadRequest, wantErr: "validation_failed"},
		{name: "trailing content", body: ok + `{}`, wantCode: http.StatusBadRequest, wantErr: "validation_failed"},
		{name: "missing run", body: ok, runID: func(*runHoldFixture, *run.Run) string { return uuid.NewString() },
			wantCode: http.StatusNotFound, wantErr: "run_not_found"},
		{name: "set on a terminal run", body: ok, setup: func(f *runHoldFixture, r *run.Run) {
			f.rr.mu.Lock()
			f.rr.runs[r.ID].State = run.StateCancelled
			f.rr.mu.Unlock()
		}, wantCode: http.StatusConflict, wantErr: "run_terminal"},
		{name: "nil audit repo", body: ok, noAudit: true, wantCode: http.StatusServiceUnavailable, wantErr: "dispatch_hold_unavailable"},
		{name: "failing append", body: ok, setup: func(f *runHoldFixture, _ *run.Run) { f.aud.appendErr = errors.New("chain down") },
			wantCode: http.StatusInternalServerError, wantErr: "dispatch_hold_failed"},
		{name: "own-state read error", body: ok, setup: func(f *runHoldFixture, _ *run.Run) { f.aud.holdListErr = errors.New("read down") },
			wantCode: http.StatusInternalServerError, wantErr: "dispatch_hold_failed"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunHoldFixture(t)
			if tc.noAudit {
				f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr})
			}
			r := f.rr.seedRun()
			if tc.setup != nil {
				tc.setup(f, r)
			}
			withID := tc.withID
			switch tc.name {
			case "run-bound token":
				withID = withIdent(Identity{Subject: "mcp:run:" + r.ID.String(), TokenID: "tok-run", Scopes: []string{"write:runs"}})
			case "anonymous":
				withID = nil
			default:
				if withID == nil {
					withID = holdOperator
				}
			}
			rid := self
			if tc.runID != nil {
				rid = tc.runID
			}
			w := f.postHold(t, rid(f, r), tc.body, withID)
			code, _ := decodeCampaignErrorDetails(t, w)
			if w.Code != tc.wantCode || code != tc.wantErr {
				t.Fatalf("= %d %s, want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantErr)
			}
			if rows := f.aud.holdRows(r.ID); len(rows) != 0 {
				t.Fatalf("a refusal appended %d hold rows: %+v", len(rows), rows)
			}
		})
	}
}

// TestRunDispatchHold_IdempotentNoop: a second set and a clear-when-unheld
// answer changed:false and leave the COMMITTED chain row count unchanged.
func TestRunDispatchHold_IdempotentNoop(t *testing.T) {
	f := newRunHoldFixture(t)
	r := f.rr.seedRun()
	if w := f.postHold(t, r.ID.String(), `{"reason":"first"}`, holdOperator); w.Code != http.StatusOK || !decodeHoldResponse(t, w).Changed {
		t.Fatalf("first set (default action) = %d %s", w.Code, w.Body.String())
	}
	w := f.postHold(t, r.ID.String(), `{"action":"set","reason":"again"}`, holdOperator)
	resp := decodeHoldResponse(t, w)
	if w.Code != http.StatusOK || resp.Changed || resp.AuditSequence != 0 || resp.DispatchHold == nil || resp.DispatchHold.Reason != "first" {
		t.Fatalf("second set = %d %+v", w.Code, resp)
	}
	if n := len(f.aud.holdRows(r.ID)); n != 1 {
		t.Fatalf("chain rows after a second set = %d, want 1", n)
	}

	other := f.rr.seedRun()
	w = f.postHold(t, other.ID.String(), `{"action":"clear","reason":"nothing held"}`, holdOperator)
	resp = decodeHoldResponse(t, w)
	if w.Code != http.StatusOK || resp.Changed || resp.DispatchHold != nil {
		t.Fatalf("clear-when-unheld = %d %+v", w.Code, resp)
	}
	if n := len(f.aud.holdRows(other.ID)); n != 0 {
		t.Fatalf("chain rows after a clear-when-unheld = %d, want 0", n)
	}
}

// TestRunDispatchHold_ClearInheritedIsNoop: clearing a child that inherits a
// parent's hold changes nothing and says which run holds it.
func TestRunDispatchHold_ClearInheritedIsNoop(t *testing.T) {
	f := newRunHoldFixture(t)
	parent := f.rr.seedRun()
	child := f.childOf(parent.ID)
	f.aud.seed(t, parent.ID, CategoryRunDispatchHoldSet, "parent hold")
	w := f.postHold(t, child.ID.String(), `{"action":"clear","reason":"try the child"}`, holdOperator)
	resp := decodeHoldResponse(t, w)
	if w.Code != http.StatusOK || resp.Changed || resp.DispatchHold == nil || !resp.DispatchHold.Inherited ||
		resp.DispatchHold.HeldRunID != parent.ID.String() {
		t.Fatalf("clear child = %d %+v / %+v", w.Code, resp, resp.DispatchHold)
	}
	if n := len(f.aud.holdRows(child.ID)); n != 0 {
		t.Fatalf("child chain rows = %d, want 0", n)
	}
}

// TestRunDispatchHold_PostActReadFailureReportsUnavailable: the append
// committed but the effective-hold re-read failed. The verb answers 200
// changed:true with the row's audit_sequence and dispatch_hold_unavailable,
// never a 500 (which the client reads as "nothing recorded"), and exactly
// one row is committed.
func TestRunDispatchHold_PostActReadFailureReportsUnavailable(t *testing.T) {
	check := func(t *testing.T, f *runHoldFixture, runID uuid.UUID, body string, before int, wantCategory string) {
		t.Helper()
		w := f.postHold(t, runID.String(), body, holdOperator)
		if w.Code != http.StatusOK {
			t.Fatalf("= %d %s, want 200", w.Code, w.Body.String())
		}
		resp := decodeHoldResponse(t, w)
		if !resp.Changed || resp.AuditSequence == 0 || !resp.DispatchHoldUnavailable || resp.DispatchHold != nil {
			t.Fatalf("response = %+v; want changed, a non-zero audit_sequence, dispatch_hold_unavailable and no dispatch_hold", resp)
		}
		rows := f.aud.holdRows(runID)
		if len(rows) != before+1 || rows[len(rows)-1].Category != wantCategory {
			t.Fatalf("chain = %+v; want exactly one new %s row", rows, wantCategory)
		}
	}
	t.Run("set, hold re-read fails", func(t *testing.T) {
		f := newRunHoldFixture(t)
		r := f.rr.seedRun()
		f.aud.holdListFailAfter = 2 // the own-state pre-check's two category reads
		check(t, f, r.ID, `{"action":"set","reason":"hold"}`, 0, CategoryRunDispatchHoldSet)
	})
	t.Run("clear on a child, ancestor re-read fails", func(t *testing.T) {
		f := newRunHoldFixture(t)
		p := f.rr.seedRun()
		c := f.childOf(p.ID)
		f.aud.seed(t, c.ID, CategoryRunDispatchHoldSet, "child hold")
		f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: &holdGetRunErrRepo{orchestratorRepo: f.rr, failID: p.ID}, AuditRepo: f.aud})
		check(t, f, c.ID, `{"action":"clear","reason":"release"}`, 1, CategoryRunDispatchHoldCleared)
	})
}

// TestRunDispatchHold_ConcurrentSetsLatestRowDecides: two simultaneous sets
// both pass the own-state pre-check before either appends (a barrier on the
// append), so the chain gains a redundant second set row, the accepted
// pre-check-then-append race. The hold stays active and the LATEST row
// decides: the effective hold is the higher-sequence row, with its reason. A
// later clear then releases it.
func TestRunDispatchHold_ConcurrentSetsLatestRowDecides(t *testing.T) {
	f := newRunHoldFixture(t)
	r := f.rr.seedRun()
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	f.aud.beforeAppend = func() {
		arrived.Done()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}
	var wg sync.WaitGroup
	got := make([]*httptest.ResponseRecorder, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = f.postHold(t, r.ID.String(), fmt.Sprintf(`{"action":"set","reason":"set %d"}`, i), holdOperator)
		}()
	}
	wg.Wait()
	f.aud.beforeAppend = nil
	for i, w := range got {
		if w.Code != http.StatusOK || !decodeHoldResponse(t, w).Changed {
			t.Fatalf("set %d = %d %s; want 200 changed:true (both passed the pre-check)", i, w.Code, w.Body.String())
		}
	}
	rows := f.aud.holdRows(r.ID)
	if len(rows) != 2 || rows[0].Category != CategoryRunDispatchHoldSet || rows[1].Category != CategoryRunDispatchHoldSet {
		t.Fatalf("chain = %+v; want two set rows", rows)
	}
	var latest runDispatchHoldAuditPayload
	if err := json.Unmarshal(rows[1].Payload, &latest); err != nil {
		t.Fatal(err)
	}
	hold, err := f.s.resolveRunDispatchHold(context.Background(), r)
	if err != nil || hold == nil || hold.AuditSequence != 2 || hold.Reason != latest.Reason {
		t.Fatalf("effective hold = %+v (%v); want active at sequence 2 with the latest row's reason %q", hold, err, latest.Reason)
	}
	if w := f.postHold(t, r.ID.String(), `{"action":"clear","reason":"release"}`, holdOperator); w.Code != http.StatusOK || !decodeHoldResponse(t, w).Changed {
		t.Fatalf("clear = %d %s", w.Code, w.Body.String())
	}
	if hold, err := f.s.resolveRunDispatchHold(context.Background(), r); err != nil || hold != nil {
		t.Fatalf("after clear = %+v (%v); want not held", hold, err)
	}
}

// TestRunDispatchHold_ClearAllowedOnTerminalRun: clear is always allowed, so
// a hold left on a run that went terminal can still be released.
func TestRunDispatchHold_ClearAllowedOnTerminalRun(t *testing.T) {
	f := newRunHoldFixture(t)
	r := f.rr.seedRun()
	f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "held")
	f.rr.mu.Lock()
	f.rr.runs[r.ID].State = run.StateCancelled
	f.rr.mu.Unlock()
	w := f.postHold(t, r.ID.String(), `{"action":"clear","reason":"release"}`, holdOperator)
	if w.Code != http.StatusOK || !decodeHoldResponse(t, w).Changed {
		t.Fatalf("clear on terminal = %d %s", w.Code, w.Body.String())
	}
}

// TestResolveRunDispatchHold_Ancestry: one row per resolver mode.
func TestResolveRunDispatchHold_Ancestry(t *testing.T) {
	ctx := context.Background()
	type result struct {
		held      bool
		inherited bool
		heldRun   uuid.UUID
		err       bool
	}
	check := func(t *testing.T, f *runHoldFixture, target *run.Run, want result) {
		t.Helper()
		got, err := f.s.resolveRunDispatchHold(ctx, target)
		if (err != nil) != want.err {
			t.Fatalf("err = %v, want err %v", err, want.err)
		}
		if (got != nil) != want.held {
			t.Fatalf("hold = %+v, want held %v", got, want.held)
		}
		if got != nil && (got.Inherited != want.inherited || got.HeldRunID != want.heldRun.String()) {
			t.Fatalf("hold = %+v, want inherited %v held_run_id %s", got, want.inherited, want.heldRun)
		}
	}
	t.Run("own hold", func(t *testing.T) {
		f := newRunHoldFixture(t)
		r := f.rr.seedRun()
		f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "own")
		check(t, f, r, result{held: true, heldRun: r.ID})
	})
	t.Run("inherited through DecomposedFrom", func(t *testing.T) {
		f := newRunHoldFixture(t)
		p := f.rr.seedRun()
		c := f.childOf(p.ID)
		f.aud.seed(t, p.ID, CategoryRunDispatchHoldSet, "parent")
		check(t, f, c, result{held: true, inherited: true, heldRun: p.ID})
	})
	t.Run("inherited through ParentRunID", func(t *testing.T) {
		f := newRunHoldFixture(t)
		p := f.rr.seedRun()
		c := f.followUpOf(p.ID)
		f.aud.seed(t, p.ID, CategoryRunDispatchHoldSet, "predecessor")
		check(t, f, c, result{held: true, inherited: true, heldRun: p.ID})
	})
	t.Run("latest wins: set clear set is held", func(t *testing.T) {
		f := newRunHoldFixture(t)
		r := f.rr.seedRun()
		f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "1")
		f.aud.seed(t, r.ID, CategoryRunDispatchHoldCleared, "2")
		f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "3")
		check(t, f, r, result{held: true, heldRun: r.ID})
	})
	t.Run("latest wins: set clear is not held", func(t *testing.T) {
		f := newRunHoldFixture(t)
		r := f.rr.seedRun()
		f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "1")
		f.aud.seed(t, r.ID, CategoryRunDispatchHoldCleared, "2")
		check(t, f, r, result{})
	})
	t.Run("missing parent decides on the runs read", func(t *testing.T) {
		f := newRunHoldFixture(t)
		c := f.childOf(uuid.New())
		check(t, f, c, result{})
		f.aud.seed(t, c.ID, CategoryRunDispatchHoldSet, "own")
		check(t, f, c, result{held: true, heldRun: c.ID})
	})
	t.Run("cycle stops", func(t *testing.T) {
		f := newRunHoldFixture(t)
		a := f.rr.seedRun()
		b := f.childOf(a.ID)
		f.rr.mu.Lock()
		f.rr.runs[a.ID].DecomposedFrom = &b.ID
		f.rr.mu.Unlock()
		check(t, f, b, result{})
	})
	t.Run("depth: 8 hops held, 9 hops not", func(t *testing.T) {
		f := newRunHoldFixture(t)
		chain := []*run.Run{f.rr.seedRun()}
		for range maxCampaignAncestorDepth + 1 {
			chain = append(chain, f.childOf(chain[len(chain)-1].ID))
		}
		leaf := chain[len(chain)-1] // 9 hops below chain[0]
		f.aud.seed(t, chain[0].ID, CategoryRunDispatchHoldSet, "root")
		check(t, f, leaf, result{})
		check(t, f, chain[maxCampaignAncestorDepth], result{held: true, inherited: true, heldRun: chain[0].ID}) // exactly 8 hops
	})
	t.Run("audit read error is returned", func(t *testing.T) {
		f := newRunHoldFixture(t)
		r := f.rr.seedRun()
		f.aud.holdListErr = errors.New("read down")
		check(t, f, r, result{err: true})
	})
	t.Run("ancestor read error is returned", func(t *testing.T) {
		f := newRunHoldFixture(t)
		p := f.rr.seedRun()
		c := f.childOf(p.ID)
		f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: &holdGetRunErrRepo{orchestratorRepo: f.rr, failID: p.ID}, AuditRepo: f.aud})
		check(t, f, c, result{err: true})
	})
	t.Run("nil audit repo is inert", func(t *testing.T) {
		f := newRunHoldFixture(t)
		f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr})
		check(t, f, f.rr.seedRun(), result{})
	})
}

// TestHostDispatch_RunDispatchHoldRefusesMarker: one row per stage type. A
// held run's marker answers 409 run_dispatch_held with the held run and
// reason in details, the stage re-reads its pre-call state and the slot store
// saw no Admit. Without the guard the fixture's CAS admits (dispatched).
func TestHostDispatch_RunDispatchHoldRefusesMarker(t *testing.T) {
	for _, typ := range []run.StageType{run.StageTypePlan, run.StageTypeImplement, run.StageTypeAcceptance} {
		t.Run(string(typ), func(t *testing.T) {
			f := newRunHoldFixture(t)
			store := &countingAdmitStore{}
			f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr, AuditRepo: f.aud, Concurrency: store})
			r := f.rr.seedRun()
			st := f.stage(r.ID, typ, run.StageStateAwaitingHostDispatch)
			f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "operator says wait")
			w := postHostDispatch(t, f.s, r.ID, st.ID, withHostDispatchOperator)
			code, details := decodeCampaignErrorDetails(t, w)
			if w.Code != http.StatusConflict || code != "run_dispatch_held" {
				t.Fatalf("marker = %d %s, want 409 run_dispatch_held", w.Code, w.Body.String())
			}
			if details["held_run_id"] != r.ID.String() || details["inherited"] != false || details["reason"] != "operator says wait" ||
				details["stage_id"] != st.ID.String() {
				t.Fatalf("details = %v", details)
			}
			if strings.Contains(decodeCampaignErrorMessage(t, w), "operator says wait") {
				t.Fatal("the refusal message embeds the operator's free-text reason; it must stay a data field")
			}
			if got := f.stageState(t, st.ID); got != run.StageStateAwaitingHostDispatch {
				t.Fatalf("stage = %s after refusal, want awaiting_host_dispatch", got)
			}
			if n := store.count(); n != 0 {
				t.Fatalf("slot Admit calls = %d, want 0", n)
			}
		})
	}
}

// TestHostDispatch_RunDispatchHoldInheritedByDecomposedChild: a wave-2 child
// stage of a held parent is refused with inherited true. The set row lives
// ONLY on the parent's chain.
func TestHostDispatch_RunDispatchHoldInheritedByDecomposedChild(t *testing.T) {
	f := newRunHoldFixture(t)
	parent := f.rr.seedRun()
	child := f.childOf(parent.ID)
	st := f.stage(child.ID, run.StageTypePlan, run.StageStatePending)
	f.aud.seed(t, parent.ID, CategoryRunDispatchHoldSet, "parent hold")
	w := postHostDispatch(t, f.s, child.ID, st.ID, withHostDispatchOperator)
	code, details := decodeCampaignErrorDetails(t, w)
	if w.Code != http.StatusConflict || code != "run_dispatch_held" || details["inherited"] != true ||
		details["held_run_id"] != parent.ID.String() || details["run_id"] != child.ID.String() {
		t.Fatalf("child marker = %d %s", w.Code, w.Body.String())
	}
	if got := f.stageState(t, st.ID); got != run.StageStatePending {
		t.Fatalf("child stage = %s, want pending", got)
	}
}

// TestHostDispatch_RunDispatchHoldClearReadmits: after a clear the same
// marker answers 200 transitioned:true and the stage reads dispatched.
func TestHostDispatch_RunDispatchHoldClearReadmits(t *testing.T) {
	f := newRunHoldFixture(t)
	r := f.rr.seedRun()
	st := f.stage(r.ID, run.StageTypePlan, run.StageStatePending)
	f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "hold")
	if w := postHostDispatch(t, f.s, r.ID, st.ID, withHostDispatchOperator); w.Code != http.StatusConflict {
		t.Fatalf("held marker = %d %s, want 409", w.Code, w.Body.String())
	}
	f.aud.seed(t, r.ID, CategoryRunDispatchHoldCleared, "release")
	w := postHostDispatch(t, f.s, r.ID, st.ID, withHostDispatchOperator)
	if w.Code != http.StatusOK || !decodeHostDispatch(t, w).Transitioned {
		t.Fatalf("marker after clear = %d %s, want 200 transitioned", w.Code, w.Body.String())
	}
	if got := f.stageState(t, st.ID); got != run.StageStateDispatched {
		t.Fatalf("stage = %s after clear, want dispatched", got)
	}
}

// TestHostDispatch_RunDispatchHoldReadFailsClosed: a hold read error answers
// 500 dependency_check_failed and the stage is unchanged.
func TestHostDispatch_RunDispatchHoldReadFailsClosed(t *testing.T) {
	f := newRunHoldFixture(t)
	r := f.rr.seedRun()
	st := f.stage(r.ID, run.StageTypePlan, run.StageStatePending)
	f.aud.holdListErr = errors.New("chain read down")
	w := postHostDispatch(t, f.s, r.ID, st.ID, withHostDispatchOperator)
	code, _ := decodeCampaignErrorDetails(t, w)
	if w.Code != http.StatusInternalServerError || code != "dependency_check_failed" {
		t.Fatalf("marker = %d %s, want 500 dependency_check_failed", w.Code, w.Body.String())
	}
	if got := f.stageState(t, st.ID); got != run.StageStatePending {
		t.Fatalf("stage = %s, want pending", got)
	}
}

// TestHostDispatch_RunDispatchHoldQueuedWaiterRePostRefused (#4826): an
// implement stage left at awaiting_host_dispatch by a queued slot (the queue
// row never moves the stage) is refused on its re-POST BEFORE Admit. The
// unheld control proves the fixture reaches Admit.
func TestHostDispatch_RunDispatchHoldQueuedWaiterRePostRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		held       bool
		wantCode   int
		wantAdmits int
	}{
		{"held", true, http.StatusConflict, 0},
		{"not held", false, http.StatusInternalServerError, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunHoldFixture(t)
			store := &countingAdmitStore{}
			f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr, AuditRepo: f.aud, Concurrency: store})
			r := f.rr.seedRun()
			st := f.stage(r.ID, run.StageTypeImplement, run.StageStateAwaitingHostDispatch)
			if tc.held {
				f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "hold the waiter")
			}
			w := postHostDispatch(t, f.s, r.ID, st.ID, withHostDispatchOperator)
			if w.Code != tc.wantCode {
				t.Fatalf("re-POST = %d %s, want %d", w.Code, w.Body.String(), tc.wantCode)
			}
			if n := store.count(); n != tc.wantAdmits {
				t.Fatalf("Admit calls = %d, want %d", n, tc.wantAdmits)
			}
		})
	}
}

// TestHostDispatch_RunDispatchHoldDispatchedArmUnaffected: the idempotent
// dispatched arm still answers 200 transitioned:false under a hold.
func TestHostDispatch_RunDispatchHoldDispatchedArmUnaffected(t *testing.T) {
	f := newRunHoldFixture(t)
	r := f.rr.seedRun()
	st := f.stage(r.ID, run.StageTypePlan, run.StageStateDispatched)
	f.aud.seed(t, r.ID, CategoryRunDispatchHoldSet, "hold")
	w := postHostDispatch(t, f.s, r.ID, st.ID, withHostDispatchOperator)
	if w.Code != http.StatusOK || decodeHostDispatch(t, w).Transitioned {
		t.Fatalf("dispatched arm = %d %s, want 200 transitioned:false", w.Code, w.Body.String())
	}
}

// TestRunDispatchHold_EndToEndParentHoldsChildMarker drives server.New over
// HTTP with real bearer tokens: request -> handler -> audit persistence ->
// resolver -> marker -> GET projection, then the clear readmits.
func TestRunDispatchHold_EndToEndParentHoldsChildMarker(t *testing.T) {
	rr := newOrchestratorRepo()
	aud := newHoldAudit()
	tokens := newFakeTokenRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: aud, APITokenRepo: tokens})
	tok, err := tokens.Issue(context.Background(), "github:ops", []string{"read:runs", "write:runs"})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok.PlainText)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}
	parent := rr.seedRun()
	child := rr.seedRun()
	rr.mu.Lock()
	child.DecomposedFrom = &parent.ID
	rr.mu.Unlock()
	wave2 := rr.seedStage(child.ID, 0, run.StageStatePending)
	marker := "/v0/runs/" + child.ID.String() + "/stages/" + wave2.ID.String() + "/host-dispatch"

	if w := do(http.MethodPost, "/v0/runs/"+parent.ID.String()+"/dispatch-hold", `{"action":"set","reason":"park wave 2"}`); w.Code != http.StatusOK {
		t.Fatalf("set over HTTP = %d %s", w.Code, w.Body.String())
	}
	w := do(http.MethodGet, "/v0/runs/"+child.ID.String(), "")
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET child = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	if got.DispatchHold == nil || !got.DispatchHold.Inherited || got.DispatchHold.HeldRunID != parent.ID.String() || got.DispatchHold.Reason != "park wave 2" {
		t.Fatalf("GET child dispatch_hold = %+v", got.DispatchHold)
	}
	w = do(http.MethodPost, marker, "")
	if code, _ := decodeCampaignErrorDetails(t, w); w.Code != http.StatusConflict || code != "run_dispatch_held" {
		t.Fatalf("held child marker = %d %s", w.Code, w.Body.String())
	}
	if st, _ := rr.GetStage(context.Background(), wave2.ID); st.State != run.StageStatePending {
		t.Fatalf("held child stage = %s, want pending", st.State)
	}
	if rows := aud.holdRows(parent.ID); len(rows) != 1 || rows[0].Category != CategoryRunDispatchHoldSet {
		t.Fatalf("parent chain = %+v, want one set row", rows)
	}
	if rows := aud.holdRows(child.ID); len(rows) != 0 {
		t.Fatalf("child chain = %+v, want none", rows)
	}
	if w := do(http.MethodPost, "/v0/runs/"+parent.ID.String()+"/dispatch-hold", `{"action":"clear","reason":"wave 1 integrated"}`); w.Code != http.StatusOK {
		t.Fatalf("clear over HTTP = %d %s", w.Code, w.Body.String())
	}
	w = do(http.MethodPost, marker, "")
	if w.Code != http.StatusOK || !decodeHostDispatch(t, w).Transitioned {
		t.Fatalf("marker after clear = %d %s, want 200 transitioned", w.Code, w.Body.String())
	}
	if st, _ := rr.GetStage(context.Background(), wave2.ID); st.State != run.StageStateDispatched {
		t.Fatalf("child stage after clear = %s, want dispatched", st.State)
	}
}

// runDispatchHoldDocumentedCodes is every code the verb and the marker guard
// answer; the openapi operation must name each.
var runDispatchHoldDocumentedCodes = []string{
	"authentication_required", "run_token_forbidden", "insufficient_scope", "validation_failed",
	runDispatchHoldCodeUnavailable, "run_not_found", runDispatchHoldCodeTerminal, runDispatchHoldCodeFailed,
}

// TestOpenAPI_RunDispatchHoldRouteDocumented pins the route, its refusal
// codes, the RunDispatchHold schema, the run fields, the awaiting block field
// and reason, and run_dispatch_held on the host-dispatch marker.
func TestOpenAPI_RunDispatchHoldRouteDocumented(t *testing.T) {
	doc, _ := escapeOpenAPIDoc(t)
	op := escapeMapAt(t, doc, "paths", "/v0/runs/{run_id}/dispatch-hold", "post")
	if op["operationId"] != "setRunDispatchHold" {
		t.Errorf("docs/api/v0.openapi.yaml: dispatch-hold operationId = %v, want setRunDispatchHold", op["operationId"])
	}
	opYAML, _ := yaml.Marshal(op)
	for _, code := range runDispatchHoldDocumentedCodes {
		if !strings.Contains(string(opYAML), code) {
			t.Errorf("docs/api/v0.openapi.yaml POST /v0/runs/{run_id}/dispatch-hold does not name code %q", code)
		}
	}
	schemas := escapeMapAt(t, doc, "components", "schemas")
	hold := escapeMapAt(t, schemas, "RunDispatchHold", "properties")
	for _, field := range []string{"held_run_id", "inherited", "reason", "held_by", "held_at", "audit_sequence"} {
		if _, ok := hold[field]; !ok {
			t.Errorf("docs/api/v0.openapi.yaml RunDispatchHold is missing %q (runDispatchHoldPayload)", field)
		}
	}
	req := escapeMapAt(t, schemas, "RunDispatchHoldRequest")
	if req["additionalProperties"] != false {
		t.Error("docs/api/v0.openapi.yaml RunDispatchHoldRequest must set additionalProperties: false (strict decode)")
	}
	runProps := escapeMapAt(t, schemas, "Run", "properties")
	for _, field := range []string{"dispatch_hold", "dispatch_hold_unavailable"} {
		if _, ok := runProps[field]; !ok {
			t.Errorf("docs/api/v0.openapi.yaml Run is missing %q (runResponse)", field)
		}
	}
	awaiting := escapeMapAt(t, schemas, "CampaignItemAwaiting")
	if _, ok := escapeMapAt(t, awaiting, "properties")["dispatch_hold"]; !ok {
		t.Error("docs/api/v0.openapi.yaml CampaignItemAwaiting is missing dispatch_hold")
	}
	awaitingYAML, _ := yaml.Marshal(awaiting)
	if !strings.Contains(string(awaitingYAML), awaitingReasonDispatchHeld) {
		t.Errorf("docs/api/v0.openapi.yaml CampaignItemAwaiting does not name reason %q", awaitingReasonDispatchHeld)
	}
	marker := escapeMapAt(t, doc, "paths", "/v0/runs/{run_id}/stages/{stage_id}/host-dispatch", "post")
	markerYAML, _ := yaml.Marshal(marker)
	if !strings.Contains(string(markerYAML), runDispatchHoldCodeHeld) {
		t.Errorf("docs/api/v0.openapi.yaml host-dispatch marker does not name %q", runDispatchHoldCodeHeld)
	}
}
