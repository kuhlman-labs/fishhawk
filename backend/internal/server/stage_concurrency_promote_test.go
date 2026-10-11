package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The cross-layer tests for the operator promote of a queued concurrency-group
// waiter (#4191): the REAL handler over the newSCPG harness (pgtest Postgres
// run + audit repositories and the Postgres concurrency store). Every verdict
// is read back from committed state — the slot row's order_key, the
// stage_concurrency_promoted audit rows, the stage read surface and the
// host-dispatch marker — never from the response alone.

var promoteOperator = Identity{Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:runs"}}

// promote POSTs the promote verb for stageID with the raw body as id
// (nil = anonymous), straight to the handler.
func (f *scPG) promote(stageID, body string, id *Identity) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stageID+"/concurrency/promote", strings.NewReader(body))
	req.SetPathValue("stage_id", stageID)
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	w := httptest.NewRecorder()
	f.srv.handlePromoteQueuedStage(w, req)
	return w
}

func promoteBody(reason string) string {
	raw, _ := json.Marshal(map[string]string{"reason": reason})
	return string(raw)
}

func decodePromote(t *testing.T, w *httptest.ResponseRecorder) promoteQueuedStageResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("promote = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promoteQueuedStageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode promote: %v", err)
	}
	return resp
}

// promoteErrorCode returns the error envelope's code and details.
func promoteErrorCode(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	return env.Error.Code, env.Error.Details
}

// orderKey reads the slot row's order_key (nil = NULL, never promoted).
func (f *scPG) orderKey(id uuid.UUID) *time.Time {
	f.t.Helper()
	var k *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT order_key FROM stage_concurrency_slots WHERE stage_id = $1`, id).Scan(&k); err != nil {
		f.t.Fatalf("read order_key: %v", err)
	}
	return k
}

type promotedRow struct {
	sequence     int64
	actorKind    string
	actorSubject string
	stageID      *uuid.UUID
	payload      map[string]any
}

// promotedRows reads the stage_concurrency_promoted rows across EVERY run.
func (f *scPG) promotedRows() []promotedRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT sequence, actor_kind, actor_subject, stage_id, payload FROM audit_entries WHERE category = $1 ORDER BY id`,
		CategoryStageConcurrencyPromoted)
	if err != nil {
		f.t.Fatalf("read promoted rows: %v", err)
	}
	defer rows.Close()
	var out []promotedRow
	for rows.Next() {
		var r promotedRow
		var kind, subject *string
		var raw []byte
		if err := rows.Scan(&r.sequence, &kind, &subject, &r.stageID, &raw); err != nil {
			f.t.Fatalf("scan promoted row: %v", err)
		}
		if kind != nil {
			r.actorKind = *kind
		}
		if subject != nil {
			r.actorSubject = *subject
		}
		_ = json.Unmarshal(raw, &r.payload)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate promoted rows: %v", err)
	}
	return out
}

// holderAndWaiters admits H on h1 and queues each of waiters behind it, in
// order.
func (f *scPG) holderAndWaiters(n int) (*run.Stage, []*run.Stage) {
	f.t.Helper()
	h := f.impl()
	f.admitted(h, hostBody("h1"))
	ws := make([]*run.Stage, n)
	for i := range ws {
		ws[i] = f.impl()
		f.queued(ws[i], hostBody("h1"))
	}
	return h, ws
}

// unchanged asserts a refused promote of st wrote nothing: order_key NULL and
// no stage_concurrency_promoted row anywhere.
func (f *scPG) unchanged(st *run.Stage) {
	f.t.Helper()
	if k := f.orderKey(st.ID); k != nil {
		f.t.Fatalf("order_key = %v after a refused promote, want NULL", *k)
	}
	if rows := f.promotedRows(); len(rows) != 0 {
		f.t.Fatalf("promoted audit rows = %+v after a refused promote, want none", rows)
	}
}

// TestPromoteQueuedStage_CrossLayer: handler -> store -> Postgres -> audit ->
// stage read surface -> host-dispatch marker. C, queued third behind holder
// H, is promoted to first; the stage reads show the new order and
// promoted:true; exactly one audit row carries the payload; and once H
// settles the marker queues A behind C and admits C.
func TestPromoteQueuedStage_CrossLayer(t *testing.T) {
	f := newSCPG(t, pgStore)
	h, ws := f.holderAndWaiters(3)
	a, b, c := ws[0], ws[1], ws[2]

	resp := decodePromote(t, f.promote(c.ID.String(), promoteBody("  C unblocks the others  "), &promoteOperator))
	if !resp.Promoted || resp.OldPosition != 3 || resp.NewPosition != 1 || resp.StageID != c.ID || resp.RunID != c.RunID ||
		resp.Group != "local-implement:h1" || resp.Limit != 1 || !sameStageIDs(holderIDs(resp.Holders), h.ID) || resp.AuditSequence == nil {
		t.Fatalf("promote response = %+v, want promoted 3 -> 1 behind holder H with an audit sequence", resp)
	}
	if f.orderKey(c.ID) == nil {
		t.Fatal("C order_key is NULL after a 200 promote")
	}

	for name, blk := range f.blocks(c) {
		if blk == nil || blk.Position != 1 || !blk.Promoted {
			t.Errorf("%s: C block = %+v, want position 1 promoted:true", name, blk)
		}
	}
	for st, want := range map[*run.Stage]int{a: 2, b: 3} {
		if blk := f.blocks(st)["get"]; blk == nil || blk.Position != want || blk.Promoted {
			t.Errorf("stage %s block = %+v, want position %d promoted:false", st.ID, blk, want)
		}
	}
	if blk := f.blocks(h)["get"]; blk == nil || blk.Status != "holding" || blk.Promoted {
		t.Errorf("holder block = %+v, want holding and never promoted", blk)
	}

	rows := f.promotedRows()
	if len(rows) != 1 {
		t.Fatalf("promoted audit rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.sequence != *resp.AuditSequence || r.stageID == nil || *r.stageID != c.ID ||
		r.actorKind != string(audit.ActorUser) || r.actorSubject != "github:ops" {
		t.Errorf("promoted row = %+v, want sequence %d on stage C by user github:ops", r, *resp.AuditSequence)
	}
	for k, want := range map[string]any{
		"stage_id": c.ID.String(), "run_id": c.RunID.String(), "group": "local-implement:h1",
		"limit": float64(1), "old_position": float64(3), "new_position": float64(1),
		"reason": "C unblocks the others", "actor": "github:ops",
	} {
		if r.payload[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, r.payload[k], want)
		}
	}
	if !audit.IsKnownCategory(CategoryStageConcurrencyPromoted) {
		t.Errorf("%s is not registered in audit/categories.go", CategoryStageConcurrencyPromoted)
	}

	f.to(h.ID, run.StageStateRunning, run.StageStateSucceeded)
	if d, _, _ := f.queued(a, hostBody("h1")); d.Position != 2 {
		t.Errorf("A after H settled = position %d, want 2 (behind promoted C)", d.Position)
	}
	f.admitted(c, hostBody("h1"))
}

// TestPromoteQueuedStage_AuthAndBodyRefused pins every rung decided before the
// store: each answers its code and writes NOTHING (order_key read back NULL,
// zero audit rows). Each fixture is otherwise promotable (C is third), so
// deleting a rung turns its case into a 200.
func TestPromoteQueuedStage_AuthAndBodyRefused(t *testing.T) {
	f := newSCPG(t, pgStore)
	_, ws := f.holderAndWaiters(3)
	c := ws[2]
	ok := promoteBody("unblock")
	runBound := Identity{Subject: "mcp:run:" + c.RunID.String(), TokenID: "tok-run", Scopes: []string{"write:runs", "write:retries"}}
	noScope := Identity{Subject: "github:ops", TokenID: "tok-ro", Scopes: []string{"read:runs"}}

	cases := []struct {
		name, stageID, body string
		id                  *Identity
		status              int
		code, field         string
	}{
		{"anonymous", c.ID.String(), ok, nil, http.StatusUnauthorized, "authentication_required", ""},
		{"run-bound token with write:runs", c.ID.String(), ok, &runBound, http.StatusForbidden, "run_token_forbidden", ""},
		{"token without write:runs", c.ID.String(), ok, &noScope, http.StatusForbidden, "insufficient_scope", ""},
		{"non-UUID stage id", "not-a-uuid", ok, &promoteOperator, http.StatusBadRequest, "validation_failed", "stage_id"},
		{"whitespace-only reason", c.ID.String(), promoteBody(" \t\n "), &promoteOperator, http.StatusBadRequest, "validation_failed", "reason"},
		{"absent reason", c.ID.String(), `{}`, &promoteOperator, http.StatusBadRequest, "validation_failed", "reason"},
		{"empty body", c.ID.String(), ``, &promoteOperator, http.StatusBadRequest, "validation_failed", "reason"},
		{"reason over 1000 bytes", c.ID.String(), promoteBody(strings.Repeat("r", promoteReasonMax+1)), &promoteOperator, http.StatusBadRequest, "validation_failed", "reason"},
		{"unknown field", c.ID.String(), `{"reason":"x","unexpected":true}`, &promoteOperator, http.StatusBadRequest, "validation_failed", "body"},
		{"trailing document", c.ID.String(), `{"reason":"x"} {"reason":"y"}`, &promoteOperator, http.StatusBadRequest, "validation_failed", "body"},
		{"non-object body", c.ID.String(), `["x"]`, &promoteOperator, http.StatusBadRequest, "validation_failed", "body"},
		{"non-string reason", c.ID.String(), `{"reason":5}`, &promoteOperator, http.StatusBadRequest, "validation_failed", "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.promote(tc.stageID, tc.body, tc.id)
			code, details := promoteErrorCode(t, w)
			if w.Code != tc.status || code != tc.code {
				t.Fatalf("promote = %d %s, want %d %s:\n%s", w.Code, code, tc.status, tc.code, w.Body.String())
			}
			if tc.field != "" && details["field"] != tc.field {
				t.Errorf("details.field = %v, want %s", details["field"], tc.field)
			}
			f.unchanged(c)
		})
	}

	// A reason of exactly the bound is accepted (the bound is not off by one).
	resp := decodePromote(t, f.promote(c.ID.String(), promoteBody(strings.Repeat("r", promoteReasonMax)), &promoteOperator))
	if !resp.Promoted {
		t.Fatalf("1000-byte reason = %+v, want promoted", resp)
	}
}

// TestPromoteQueuedStage_StoreRefusalsMapped: one case per store refusal, each
// read back as nothing written.
func TestPromoteQueuedStage_StoreRefusalsMapped(t *testing.T) {
	t.Run("holding stage is never preempted", func(t *testing.T) {
		f := newSCPG(t, pgStore)
		h, _ := f.holderAndWaiters(1)
		before, _ := f.slot(h.ID)
		w := f.promote(h.ID.String(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, w); w.Code != http.StatusConflict || code != promoteCodeSlotHeld {
			t.Fatalf("promote holder = %d %s, want 409 %s", w.Code, code, promoteCodeSlotHeld)
		}
		after, _ := f.slot(h.ID)
		if after.state != "held" || !after.enqueuedAt.Equal(before.enqueuedAt) || after.acquiredAt == nil ||
			!after.acquiredAt.Equal(*before.acquiredAt) {
			t.Errorf("holder row %+v changed from %+v", after, before)
		}
		f.unchanged(h)
	})
	t.Run("stage with no slot row", func(t *testing.T) {
		f := newSCPG(t, pgStore)
		st := f.impl()
		w := f.promote(st.ID.String(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, w); w.Code != http.StatusConflict || code != promoteCodeNotQueued {
			t.Fatalf("promote unqueued = %d %s, want 409 %s", w.Code, code, promoteCodeNotQueued)
		}
		if _, ok := f.slot(st.ID); ok {
			t.Error("a refused promote created a slot row")
		}
		if rows := f.promotedRows(); len(rows) != 0 {
			t.Errorf("promoted rows = %+v, want none", rows)
		}
	})
	t.Run("stale waiter", func(t *testing.T) {
		f := newSCPG(t, pgStore)
		_, ws := f.holderAndWaiters(2)
		c := ws[1]
		f.exec(`UPDATE stage_concurrency_slots SET last_seen_at = clock_timestamp() - interval '61 seconds' WHERE stage_id = $1`, c.ID)
		w := f.promote(c.ID.String(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, w); w.Code != http.StatusConflict || code != promoteCodeNotLive {
			t.Fatalf("promote stale = %d %s, want 409 %s", w.Code, code, promoteCodeNotLive)
		}
		f.unchanged(c)
	})
	t.Run("unknown stage", func(t *testing.T) {
		f := newSCPG(t, pgStore)
		w := f.promote(uuid.NewString(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, w); w.Code != http.StatusNotFound || code != "stage_not_found" {
			t.Fatalf("promote unknown = %d %s, want 404 stage_not_found", w.Code, code)
		}
	})
	t.Run("slot changed group", func(t *testing.T) {
		f := newSCPG(t, func(*pgxpool.Pool) concurrency.Store { return promoteErrStore{err: concurrency.ErrSlotChanged} })
		w := f.promote(uuid.NewString(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, w); w.Code != http.StatusConflict || code != promoteCodeSlotChanged {
			t.Fatalf("promote changed = %d %s, want 409 %s", w.Code, code, promoteCodeSlotChanged)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		f := newSCPG(t, func(*pgxpool.Pool) concurrency.Store { return errStore{} })
		w := f.promote(uuid.NewString(), promoteBody("x"), &promoteOperator)
		code, details := promoteErrorCode(t, w)
		if w.Code != http.StatusInternalServerError || code != promoteCodeFailed {
			t.Fatalf("promote store error = %d %s, want 500 %s", w.Code, code, promoteCodeFailed)
		}
		if strings.Contains(w.Body.String(), "db down") || details[internalCauseKey] != nil {
			t.Errorf("500 body leaks the raw cause: %s", w.Body.String())
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		f := newSCPG(t, nil)
		w := f.promote(uuid.NewString(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, w); w.Code != http.StatusServiceUnavailable || code != promoteCodeUnconfigured {
			t.Fatalf("promote nil store = %d %s, want 503 %s", w.Code, code, promoteCodeUnconfigured)
		}
	})
}

// promoteErrStore wraps a real store shape but answers Promote with err.
type promoteErrStore struct {
	errStore
	err error
}

func (p promoteErrStore) Promote(context.Context, concurrency.PromoteRequest) (concurrency.Promotion, error) {
	return concurrency.Promotion{}, p.err
}

// TestPromoteQueuedStage_AuditAppendFailureRollsBack: an append failure INSIDE
// the promotion transaction answers 500 and the promotion is rolled back — the
// order_key READ BACK is NULL (committed state, not the error identity).
func TestPromoteQueuedStage_AuditAppendFailureRollsBack(t *testing.T) {
	f := newSCPG(t, pgStore)
	_, ws := f.holderAndWaiters(2)
	c := ws[1]
	calls := 0
	f.srv.promoteAuditAppendOverride = func(context.Context, pgx.Tx, audit.ChainAppendParams) (*audit.Entry, error) {
		calls++
		return nil, errors.New("chain append refused")
	}
	w := f.promote(c.ID.String(), promoteBody("x"), &promoteOperator)
	if code, _ := promoteErrorCode(t, w); w.Code != http.StatusInternalServerError || code != promoteCodeFailed {
		t.Fatalf("promote = %d %s, want 500 %s:\n%s", w.Code, code, promoteCodeFailed, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("append seam called %d times, want 1 (the failure must be injected inside the promotion)", calls)
	}
	f.unchanged(c)
	if blk := f.blocks(c)["get"]; blk == nil || blk.Position != 2 || blk.Promoted {
		t.Errorf("C block after rollback = %+v, want position 2 promoted:false", blk)
	}
}

// TestPromoteQueuedStage_OperatorAgentAdmitted pins the admitted-by-design
// rung (approval condition 6): a delegated operator-agent token with
// write:runs promotes, and the audit row records actor_kind agent. Unlike
// review_rerun.go this verb does not refuse operator agents; a later
// tightening must change this test deliberately.
func TestPromoteQueuedStage_OperatorAgentAdmitted(t *testing.T) {
	f := newSCPG(t, pgStore)
	_, ws := f.holderAndWaiters(2)
	c := ws[1]
	agent := Identity{Subject: "operator-agent/operator-role-v0", TokenID: "tok-agent", Scopes: []string{"write:runs"}}
	resp := decodePromote(t, f.promote(c.ID.String(), promoteBody("overseer asked"), &agent))
	if !resp.Promoted || resp.NewPosition != 1 {
		t.Fatalf("operator-agent promote = %+v, want promoted to 1", resp)
	}
	rows := f.promotedRows()
	if len(rows) != 1 || rows[0].actorKind != string(audit.ActorAgent) || rows[0].actorSubject != agent.Subject ||
		rows[0].payload["actor"] != agent.Subject {
		t.Fatalf("promoted rows = %+v, want one agent row by %s", rows, agent.Subject)
	}
}

// TestPromoteQueuedStage_AlreadyFirstIsNoop: the head waiter answers 200
// promoted:false with no audit_sequence, and nothing is written.
func TestPromoteQueuedStage_AlreadyFirstIsNoop(t *testing.T) {
	f := newSCPG(t, pgStore)
	_, ws := f.holderAndWaiters(2)
	a := ws[0]
	w := f.promote(a.ID.String(), promoteBody("x"), &promoteOperator)
	resp := decodePromote(t, w)
	if resp.Promoted || resp.OldPosition != 1 || resp.NewPosition != 1 || resp.AuditSequence != nil ||
		strings.Contains(w.Body.String(), "audit_sequence") {
		t.Fatalf("no-op promote = %s, want promoted:false 1 -> 1 without audit_sequence", w.Body.String())
	}
	f.unchanged(a)
}

// TestPromoteQueuedStage_RouteRegistered drives the verb through the server's
// mux and auth middleware (not the handler directly): an anonymous POST
// reaches the handler's 401, where an unregistered route would 404/405.
func TestPromoteQueuedStage_RouteRegistered(t *testing.T) {
	f := newSCPG(t, pgStore)
	_, ws := f.holderAndWaiters(2)
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+ws[1].ID.String()+"/concurrency/promote", strings.NewReader(promoteBody("x")))
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if code, _ := promoteErrorCode(t, w); w.Code != http.StatusUnauthorized || code != "authentication_required" {
		t.Fatalf("anonymous POST through the mux = %d %s, want 401 authentication_required:\n%s", w.Code, code, w.Body.String())
	}
	f.unchanged(ws[1])
}

// TestOpenAPIDocumentsConcurrencyPromote pins the API contract doc to the
// promote surface (approval condition 3: this assertion lives here, not in
// stage_concurrency_test.go's TestOpenAPIDocumentsConcurrency).
func TestOpenAPIDocumentsConcurrencyPromote(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"/v0/stages/{stage_id}/concurrency/promote:",
		"PromoteQueuedStageRequest:", "PromoteQueuedStageResponse:",
		promoteCodeUnconfigured, promoteCodeFailed, promoteCodeNotQueued, promoteCodeSlotHeld,
		promoteCodeNotLive, promoteCodeSlotChanged, "run_token_forbidden", "audit_sequence",
		CategoryStageConcurrencyPromoted,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml does not document %q", want)
		}
	}
	// `promoted` must be a property of the StageConcurrency schema itself:
	// scan that schema's lines up to the next 4-space-indented schema key.
	const header = "\n    StageConcurrency:\n"
	start := strings.Index(doc, header)
	if start < 0 {
		t.Fatal("docs/api/v0.openapi.yaml has no StageConcurrency schema")
	}
	declared := false
	for _, line := range strings.Split(doc[start+len(header):], "\n") {
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") {
			break
		}
		if strings.HasPrefix(line, "        promoted:") {
			declared = true
		}
	}
	if !declared {
		t.Error("the StageConcurrency schema does not declare `promoted`")
	}
}
