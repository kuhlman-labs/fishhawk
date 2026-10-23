package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// The cross-layer tests for the operator promote of a held merge-queue entry
// (ADR-093 D2/D7, E87.6 / #4297): the REAL handler over the mcq fixture (the
// pgtest Postgres mergequeue store, run and audit repositories). Every
// verdict is read back from committed state — the queue order
// (ListForBase), the entry's order_key and the merge_queue_promoted rows —
// never from the response alone.

// mqpWorld is A live (it holds the line, so the detached pump after a
// promote admits nothing), then B, C, D held in that order.
type mqpWorld struct {
	f          *mcqFixture
	a, b, c, d *mcqRun
}

func newMQPWorld(t *testing.T) *mqpWorld {
	t.Helper()
	f := newMCQFixture(t)
	w := &mqpWorld{f: f}
	ctx := context.Background()
	runs := make([]*mcqRun, 0, 4)
	for _, name := range []string{"a", "b", "c", "d"} {
		r := f.seed(name, name+"0", []byte(mcvUndelegatedSpecYAML))
		if _, err := f.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: name + "0"}); err != nil {
			t.Fatalf("enqueue %s: %v", name, err)
		}
		runs = append(runs, r)
	}
	w.a, w.b, w.c, w.d = runs[0], runs[1], runs[2], runs[3]
	adm, err := f.store.AdmitNext(ctx, mqpBase())
	if err != nil || !adm.Admitted || adm.Entry == nil || adm.Entry.RunID != w.a.id {
		t.Fatalf("admit A = %+v, %v; want A admitted live", adm, err)
	}
	return w
}

func mqpBase() mergequeue.Base { return mergequeue.Base{Repo: "x/y", BaseRef: mcqBase} }

// promote POSTs the verb for runID with the raw body as id (nil =
// anonymous), straight to the handler.
func mqpPromote(s *Server, runID, body string, id *Identity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/merge-queue/promote", strings.NewReader(body))
	req.SetPathValue("run_id", runID)
	if id != nil {
		req = withIdentity(req, *id)
	}
	w := httptest.NewRecorder()
	s.handleMergeQueuePromote(w, req)
	return w
}

func decodeMQPromote(t *testing.T, w *httptest.ResponseRecorder) mergeQueuePromoteResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("merge-queue promote = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp mergeQueuePromoteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode promote: %v", err)
	}
	return resp
}

// order reads the base's active entries' runs in queue order.
func (w *mqpWorld) order() []uuid.UUID {
	w.f.t.Helper()
	es, err := w.f.store.ListForBase(context.Background(), mqpBase())
	if err != nil {
		w.f.t.Fatalf("list for base: %v", err)
	}
	out := make([]uuid.UUID, 0, len(es))
	for _, e := range es {
		out = append(out, e.RunID)
	}
	return out
}

func (w *mqpWorld) requireOrder(want ...*mcqRun) {
	w.f.t.Helper()
	got := w.order()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = got[i] == want[i].id
	}
	if !ok {
		names := map[uuid.UUID]string{w.a.id: "A", w.b.id: "B", w.c.id: "C", w.d.id: "D"}
		gotNames := make([]string, 0, len(got))
		for _, id := range got {
			gotNames = append(gotNames, names[id])
		}
		w.f.t.Fatalf("queue order = %v, want %d entries in the requested order", gotNames, len(want))
	}
}

// orderKey reads the run's newest entry's order_key (nil = NULL).
func (w *mqpWorld) orderKey(r *mcqRun) *time.Time {
	w.f.t.Helper()
	var k *time.Time
	if err := w.f.pool.QueryRow(context.Background(),
		`SELECT order_key FROM merge_candidate_queue_entries WHERE id = $1`, w.f.entry(r).ID).Scan(&k); err != nil {
		w.f.t.Fatalf("read order_key: %v", err)
	}
	return k
}

// promotedRows reads every merge_queue_promoted row across every run.
func (w *mqpWorld) promotedRows() []promotedRow {
	w.f.t.Helper()
	rows, err := w.f.pool.Query(context.Background(),
		`SELECT sequence, actor_kind, actor_subject, stage_id, payload FROM audit_entries WHERE category = $1 ORDER BY id`,
		CategoryMergeQueuePromoted)
	if err != nil {
		w.f.t.Fatalf("read promoted rows: %v", err)
	}
	defer rows.Close()
	var out []promotedRow
	for rows.Next() {
		var r promotedRow
		var kind, subject *string
		var raw []byte
		if err := rows.Scan(&r.sequence, &kind, &subject, &r.stageID, &raw); err != nil {
			w.f.t.Fatalf("scan promoted row: %v", err)
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
		w.f.t.Fatalf("iterate promoted rows: %v", err)
	}
	return out
}

// unchanged asserts the queue is still A live, B, C, D with no order key on
// any held entry and no merge_queue_promoted row.
func (w *mqpWorld) unchanged() {
	w.f.t.Helper()
	w.requireOrder(w.a, w.b, w.c, w.d)
	for _, r := range []*mcqRun{w.b, w.c, w.d} {
		if k := w.orderKey(r); k != nil {
			w.f.t.Fatalf("run %s order_key = %v, want NULL (nothing promoted)", r.id, *k)
		}
	}
	if rows := w.promotedRows(); len(rows) != 0 {
		w.f.t.Fatalf("merge_queue_promoted rows = %+v, want none", rows)
	}
}

// TestMergeQueuePromote_MovesHeldEntryToHeadAndRecords is the 200 path: D is
// promoted from held position 3 to 1, the live A keeps the line, the order is
// read back A, D, B, C, D carries an order key, and exactly ONE
// merge_queue_promoted row names the entry, run, positions, reason and actor,
// with its sequence on the response.
func TestMergeQueuePromote_MovesHeldEntryToHeadAndRecords(t *testing.T) {
	w := newMQPWorld(t)
	rec := mqpPromote(w.f.srv, w.d.id.String(), promoteBody("unblocks B and C"), &promoteOperator)
	resp := decodeMQPromote(t, rec)
	w.f.srv.bgMergeQueuePump.Wait()
	dEntry := w.f.entry(w.d)
	if !resp.Promoted || resp.OldPosition != 3 || resp.NewPosition != 1 || resp.RunID != w.d.id.String() ||
		resp.EntryID != dEntry.ID.String() || resp.Repo != "x/y" || resp.BaseRef != mcqBase ||
		resp.LiveRunID != w.a.id.String() || resp.AuditSequence == nil {
		t.Fatalf("promote = %s, want D promoted 3 -> 1 behind live A with an audit_sequence", rec.Body.String())
	}
	w.requireOrder(w.a, w.d, w.b, w.c)
	if w.orderKey(w.d) == nil {
		t.Fatal("D order_key is NULL after a promote")
	}
	rows := w.promotedRows()
	if len(rows) != 1 {
		t.Fatalf("merge_queue_promoted rows = %d, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.sequence != *resp.AuditSequence || r.actorSubject != promoteOperator.Subject || r.actorKind != string(audit.ActorUser) ||
		r.payload["entry_id"] != dEntry.ID.String() || r.payload["run_id"] != w.d.id.String() ||
		r.payload["repo"] != "x/y" || r.payload["base_ref"] != mcqBase ||
		r.payload["old_position"] != float64(3) || r.payload["new_position"] != float64(1) ||
		r.payload["reason"] != "unblocks B and C" || r.payload["actor"] != promoteOperator.Subject {
		t.Fatalf("merge_queue_promoted row = %+v, want D's entry, 3 -> 1, the reason and the operator (sequence %d)", r, *resp.AuditSequence)
	}
	// The live entry was never preempted.
	if st, found, err := w.f.store.ActiveForRun(context.Background(), w.a.id); err != nil || !found || st.Entry.State != mergequeue.StateLive {
		t.Fatalf("A after promote = %+v found=%v err=%v, want still live", st.Entry, found, err)
	}
}

// TestMergeQueuePromote_AlreadyFirstIsNoop: B, first among the held entries,
// answers 200 promoted:false with no audit_sequence, and nothing is written.
func TestMergeQueuePromote_AlreadyFirstIsNoop(t *testing.T) {
	w := newMQPWorld(t)
	rec := mqpPromote(w.f.srv, w.b.id.String(), promoteBody("x"), &promoteOperator)
	resp := decodeMQPromote(t, rec)
	if resp.Promoted || resp.OldPosition != 1 || resp.NewPosition != 1 || resp.AuditSequence != nil ||
		strings.Contains(rec.Body.String(), "audit_sequence") {
		t.Fatalf("no-op promote = %s, want promoted:false 1 -> 1 without audit_sequence", rec.Body.String())
	}
	w.unchanged()
}

// TestMergeQueuePromote_AuthAndBodyRefused: one case per pre-store rung, each
// read back as nothing written.
func TestMergeQueuePromote_AuthAndBodyRefused(t *testing.T) {
	w := newMQPWorld(t)
	d := w.d.id.String()
	ok := promoteBody("unblock")
	runBound := Identity{Subject: "mcp:run:" + d, TokenID: "tok-run", Scopes: []string{"write:runs", "write:retries"}}
	noScope := Identity{Subject: "github:ops", TokenID: "tok-ro", Scopes: []string{"read:runs"}}
	cases := []struct {
		name, runID, body string
		id                *Identity
		status            int
		code, field       string
	}{
		{"anonymous", d, ok, nil, http.StatusUnauthorized, "authentication_required", ""},
		{"run-bound token with write:runs", d, ok, &runBound, http.StatusForbidden, "run_token_forbidden", ""},
		{"token without write:runs", d, ok, &noScope, http.StatusForbidden, "insufficient_scope", ""},
		{"non-UUID run id", "not-a-uuid", ok, &promoteOperator, http.StatusBadRequest, "validation_failed", "run_id"},
		{"empty reason", d, promoteBody(""), &promoteOperator, http.StatusBadRequest, "validation_failed", "reason"},
		{"whitespace-only reason", d, promoteBody(" \t\n "), &promoteOperator, http.StatusBadRequest, "validation_failed", "reason"},
		{"reason over 1000 bytes", d, promoteBody(strings.Repeat("r", promoteReasonMax+1)), &promoteOperator, http.StatusBadRequest, "validation_failed", "reason"},
		{"unknown field", d, `{"reason":"x","unexpected":true}`, &promoteOperator, http.StatusBadRequest, "validation_failed", "body"},
		{"trailing document", d, `{"reason":"x"} {"reason":"y"}`, &promoteOperator, http.StatusBadRequest, "validation_failed", "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := mqpPromote(w.f.srv, tc.runID, tc.body, tc.id)
			code, details := promoteErrorCode(t, rec)
			if rec.Code != tc.status || code != tc.code {
				t.Fatalf("promote = %d %s, want %d %s:\n%s", rec.Code, code, tc.status, tc.code, rec.Body.String())
			}
			if tc.field != "" && details["field"] != tc.field {
				t.Errorf("details.field = %v, want %s", details["field"], tc.field)
			}
			w.unchanged()
		})
	}
}

// TestMergeQueuePromote_UnconfiguredAfterAuth: 503 merge_queue_unconfigured
// for an authorised operator when no merge queue is wired, and when the
// wired store lacks the Promote capability (a nil-embedded fake: approval
// condition M1, degrade instead of panic) — but an anonymous caller still
// reads 401, because the 503 rung sits after the auth rungs.
func TestMergeQueuePromote_UnconfiguredAfterAuth(t *testing.T) {
	runID := uuid.NewString()
	for _, tc := range []struct {
		name  string
		store mergequeue.Store
	}{
		{"no merge queue", nil},
		{"store without Promote", &mcqFaultStore{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: tc.store})
			rec := mqpPromote(srv, runID, promoteBody("x"), nil)
			if code, _ := promoteErrorCode(t, rec); rec.Code != http.StatusUnauthorized || code != "authentication_required" {
				t.Fatalf("anonymous = %d %s, want 401 authentication_required:\n%s", rec.Code, code, rec.Body.String())
			}
			rec = mqpPromote(srv, runID, promoteBody("x"), &promoteOperator)
			if code, _ := promoteErrorCode(t, rec); rec.Code != http.StatusServiceUnavailable || code != mqPromoteCodeUnconfigured {
				t.Fatalf("operator = %d %s, want 503 %s:\n%s", rec.Code, code, mqPromoteCodeUnconfigured, rec.Body.String())
			}
		})
	}
}

// TestMergeQueuePromote_StoreRefusalsMapped: one case per store refusal, each
// read back as nothing written.
func TestMergeQueuePromote_StoreRefusalsMapped(t *testing.T) {
	t.Run("live entry", func(t *testing.T) {
		w := newMQPWorld(t)
		rec := mqpPromote(w.f.srv, w.a.id.String(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, rec); rec.Code != http.StatusConflict || code != mqPromoteCodeEntryLive {
			t.Fatalf("promote live A = %d %s, want 409 %s:\n%s", rec.Code, code, mqPromoteCodeEntryLive, rec.Body.String())
		}
		w.unchanged()
	})
	t.Run("not queued", func(t *testing.T) {
		w := newMQPWorld(t)
		e := w.f.seed("e", "e0", []byte(mcvUndelegatedSpecYAML))
		rec := mqpPromote(w.f.srv, e.id.String(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, rec); rec.Code != http.StatusConflict || code != mqPromoteCodeNotQueued {
			t.Fatalf("promote unqueued run = %d %s, want 409 %s:\n%s", rec.Code, code, mqPromoteCodeNotQueued, rec.Body.String())
		}
		w.unchanged()
	})
	t.Run("unknown run", func(t *testing.T) {
		w := newMQPWorld(t)
		rec := mqpPromote(w.f.srv, uuid.NewString(), promoteBody("x"), &promoteOperator)
		if code, _ := promoteErrorCode(t, rec); rec.Code != http.StatusNotFound || code != "run_not_found" {
			t.Fatalf("promote unknown run = %d %s, want 404 run_not_found:\n%s", rec.Code, code, rec.Body.String())
		}
		w.unchanged()
	})
	t.Run("yielded entry names its next_step", func(t *testing.T) {
		w := newMQPWorld(t)
		w.f.markYielded(w.d, mergeCandidateQueueReasonForgeApprovalAbsent, "no approving review", 0, 0)
		rec := mqpPromote(w.f.srv, w.d.id.String(), promoteBody("x"), &promoteOperator)
		code, details := promoteErrorCode(t, rec)
		if rec.Code != http.StatusConflict || code != mqPromoteCodeEntryYielded {
			t.Fatalf("promote yielded D = %d %s, want 409 %s:\n%s", rec.Code, code, mqPromoteCodeEntryYielded, rec.Body.String())
		}
		if details["next_step"] != forgeApprovalNextApprovePR || details["yield_reason"] != mergeCandidateQueueReasonForgeApprovalAbsent {
			t.Fatalf("details = %v, want next_step %s and yield_reason %s", details, forgeApprovalNextApprovePR, mergeCandidateQueueReasonForgeApprovalAbsent)
		}
		if k := w.orderKey(w.d); k != nil {
			t.Fatalf("yielded D order_key = %v, want NULL", *k)
		}
		if rows := w.promotedRows(); len(rows) != 0 {
			t.Fatalf("merge_queue_promoted rows = %+v, want none", rows)
		}
	})
}

// TestMergeQueuePromote_AuditAppendFailureRollsBack: the merge_queue_promoted
// append fails INSIDE the promotion transaction, so the verb answers 500 and
// the committed order is read back UNCHANGED (A, B, C, D, no order key).
func TestMergeQueuePromote_AuditAppendFailureRollsBack(t *testing.T) {
	w := newMQPWorld(t)
	calls := 0
	w.f.srv.promoteAuditAppendOverride = func(_ context.Context, _ pgx.Tx, p audit.ChainAppendParams) (*audit.Entry, error) {
		calls++
		if p.Category != CategoryMergeQueuePromoted {
			t.Errorf("append category = %s, want %s", p.Category, CategoryMergeQueuePromoted)
		}
		return nil, errors.New("chain append refused")
	}
	rec := mqpPromote(w.f.srv, w.d.id.String(), promoteBody("x"), &promoteOperator)
	if code, _ := promoteErrorCode(t, rec); rec.Code != http.StatusInternalServerError || code != mqPromoteCodeFailed {
		t.Fatalf("promote = %d %s, want 500 %s:\n%s", rec.Code, code, mqPromoteCodeFailed, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("append seam called %d times, want 1 (the failure must be injected inside the promotion)", calls)
	}
	w.unchanged()
}

// TestMergeQueuePromote_RouteRegistered drives the verb through the server's
// mux and auth middleware: an anonymous POST reaches the handler's 401, where
// an unregistered route would 404/405.
func TestMergeQueuePromote_RouteRegistered(t *testing.T) {
	w := newMQPWorld(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+w.d.id.String()+"/merge-queue/promote", strings.NewReader(promoteBody("x")))
	rec := httptest.NewRecorder()
	w.f.srv.Handler().ServeHTTP(rec, req)
	if code, _ := promoteErrorCode(t, rec); rec.Code != http.StatusUnauthorized || code != "authentication_required" {
		t.Fatalf("anonymous POST through the mux = %d %s, want 401 authentication_required:\n%s", rec.Code, code, rec.Body.String())
	}
	w.unchanged()
}

// TestMergeQueuePromote_PumpsBaseAfterChange: with NO live entry, a promote
// that changed the order hands the base to the pump, which admits the
// promoted entry FIRST — the first merge_candidate_queue_admitted row is D's,
// not B's (the entry FIFO would have admitted).
func TestMergeQueuePromote_PumpsBaseAfterChange(t *testing.T) {
	f := newMCQFixture(t)
	ctx := context.Background()
	runs := make([]*mcqRun, 0, 3)
	for _, name := range []string{"b", "c", "d"} {
		r := f.seed(name, name+"0", []byte(mcvUndelegatedSpecYAML))
		if _, err := f.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: name + "0"}); err != nil {
			t.Fatalf("enqueue %s: %v", name, err)
		}
		runs = append(runs, r)
	}
	d := runs[2]
	resp := decodeMQPromote(t, mqpPromote(f.srv, d.id.String(), promoteBody("x"), &promoteOperator))
	if !resp.Promoted || resp.OldPosition != 3 || resp.NewPosition != 1 || resp.LiveRunID != "" {
		t.Fatalf("promote = %+v, want D promoted 3 -> 1 on a free line", resp)
	}
	f.srv.bgMergeQueuePump.Wait()
	var first string
	if err := f.pool.QueryRow(ctx, `SELECT run_id::text FROM audit_entries WHERE category = $1 ORDER BY id LIMIT 1`,
		CategoryMergeCandidateQueueAdmitted).Scan(&first); err != nil {
		t.Fatalf("read the first admission row (none means the base was never pumped): %v", err)
	}
	if first != d.id.String() {
		t.Fatalf("first admitted run = %s, want the promoted D %s", first, d.id)
	}
}
