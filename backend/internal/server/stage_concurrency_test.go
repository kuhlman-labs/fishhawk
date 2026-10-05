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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The cross-layer tests for local stage concurrency groups (#3964 /
// ADR-087): the REAL host-dispatch handler over a pgtest Postgres run
// repository, audit repository and the Postgres concurrency store. Every
// verdict is read back from committed state (stage rows, slot rows, audit
// rows), never from the response alone. Time fixtures are set by SQL against
// the database clock, and every dispatched_at backdate moves the slot row's
// held_dispatched_at with it so the holder stays the SAME attempt the row
// admitted (approval condition 5).

type scPG struct {
	t     *testing.T
	pool  *pgxpool.Pool
	repo  run.Repository
	audit audit.Repository
	srv   *Server
}

func newSCPG(t *testing.T, store func(*pgxpool.Pool) concurrency.Store) *scPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &scPG{t: t, pool: pool, repo: run.NewPostgresRepository(pool), audit: audit.NewPostgresRepository(pool)}
	cfg := Config{Addr: "127.0.0.1:0", RunRepo: f.repo, AuditRepo: f.audit}
	if store != nil {
		cfg.Concurrency = store(pool)
	}
	f.srv = New(cfg)
	return f
}

func pgStore(pool *pgxpool.Pool) concurrency.Store { return concurrency.NewPostgresStore(pool) }

// stage creates a run for repo with one agent stage of typ parked at
// awaiting_host_dispatch.
func (f *scPG) stage(repo string, typ run.StageType) *run.Stage {
	f.t.Helper()
	ctx := context.Background()
	r, err := f.repo.CreateRun(ctx, run.CreateRunParams{
		Repo: repo, WorkflowID: "wf", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	st, err := f.repo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: typ, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		f.t.Fatalf("create stage: %v", err)
	}
	st, err = f.repo.TransitionStage(ctx, st.ID, run.StageStateAwaitingHostDispatch, nil)
	if err != nil {
		f.t.Fatalf("park stage: %v", err)
	}
	return st
}

func (f *scPG) impl() *run.Stage { return f.stage("kuhlman-labs/fishhawk", run.StageTypeImplement) }

func (f *scPG) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *scPG) setSpec(st *run.Stage, doc string) {
	f.t.Helper()
	f.exec(`UPDATE runs SET workflow_spec = $1 WHERE id = $2`, []byte(doc), st.RunID)
}

func (f *scPG) state(id uuid.UUID) run.StageState {
	f.t.Helper()
	st, err := f.repo.GetStage(context.Background(), id)
	if err != nil {
		f.t.Fatalf("get stage: %v", err)
	}
	return st.State
}

func (f *scPG) to(id uuid.UUID, states ...run.StageState) {
	f.t.Helper()
	for _, s := range states {
		var c *run.StageCompletion
		if s == run.StageStateFailed {
			cat, reason := run.FailureC, "runner died"
			c = &run.StageCompletion{FailureCategory: &cat, FailureReason: &reason}
		}
		if _, err := f.repo.TransitionStage(context.Background(), id, s, c); err != nil {
			f.t.Fatalf("transition %s -> %s: %v", id, s, err)
		}
	}
}

type scSlotRow struct {
	state      string
	enqueuedAt time.Time
	acquiredAt *time.Time
}

func (f *scPG) slot(id uuid.UUID) (scSlotRow, bool) {
	f.t.Helper()
	var r scSlotRow
	err := f.pool.QueryRow(context.Background(),
		`SELECT state, enqueued_at, acquired_at FROM stage_concurrency_slots WHERE stage_id = $1`, id).
		Scan(&r.state, &r.enqueuedAt, &r.acquiredAt)
	if err != nil {
		return scSlotRow{}, false
	}
	return r, true
}

// backdateHolder moves a holder's dispatched_at back AND keeps the slot row's
// held_dispatched_at on the same attempt (approval condition 5).
func (f *scPG) backdateHolder(id uuid.UUID, ago string) {
	f.t.Helper()
	f.exec(`UPDATE stages SET dispatched_at = now() - $2::interval WHERE id = $1`, id, ago)
	f.exec(`UPDATE stage_concurrency_slots s SET held_dispatched_at = st.dispatched_at FROM stages st WHERE st.id = s.stage_id AND s.stage_id = $1`, id)
}

// mark POSTs the host-dispatch marker as an operator write:runs token with
// the given raw body ("" = no body).
func (f *scPG) mark(st *run.Stage, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var req *http.Request
	target := "/v0/runs/" + st.RunID.String() + "/stages/" + st.ID.String() + "/host-dispatch"
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, target, nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	}
	req.SetPathValue("run_id", st.RunID.String())
	req.SetPathValue("stage_id", st.ID.String())
	w := httptest.NewRecorder()
	f.srv.handleHostDispatchStage(w, withHostDispatchOperator(req))
	return w
}

func hostBody(h string) string { return `{"host":"` + h + `"}` }

// admitted asserts a 200 transitioned:true grouped admission.
func (f *scPG) admitted(st *run.Stage, body string) hostDispatchResponse {
	f.t.Helper()
	w := f.mark(st, body)
	if w.Code != http.StatusOK {
		f.t.Fatalf("marker %s = %d, want 200:\n%s", st.ID, w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(f.t, w)
	if !resp.Transitioned || resp.StageState != string(run.StageStateDispatched) {
		f.t.Fatalf("marker %s = %+v, want transitioned:true dispatched", st.ID, resp)
	}
	if got := f.state(st.ID); got != run.StageStateDispatched {
		f.t.Fatalf("stage %s reads %s after admission, want dispatched", st.ID, got)
	}
	return resp
}

type queuedDetails struct {
	StageID             string              `json:"stage_id"`
	Group               string              `json:"group"`
	Limit               int                 `json:"limit"`
	Position            int                 `json:"position"`
	Holders             []concurrencyHolder `json:"holders"`
	EnqueuedAt          time.Time           `json:"enqueued_at"`
	Contended           bool                `json:"contended"`
	QueueTTLSeconds     int                 `json:"queue_ttl_seconds"`
	PollIntervalSeconds int                 `json:"poll_interval_seconds"`
}

// queued asserts a 409 concurrency_slot_queued with the stage READ BACK still
// awaiting_host_dispatch, and returns the decoded details + raw body.
func (f *scPG) queued(st *run.Stage, body string) (queuedDetails, string, string) {
	f.t.Helper()
	w := f.mark(st, body)
	if w.Code != http.StatusConflict {
		f.t.Fatalf("marker %s = %d, want 409:\n%s", st.ID, w.Code, w.Body.String())
	}
	var env struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		f.t.Fatalf("decode 409: %v", err)
	}
	if env.Error.Code != "concurrency_slot_queued" {
		f.t.Fatalf("409 code = %q, want concurrency_slot_queued:\n%s", env.Error.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		f.t.Errorf("Retry-After = %q, want 5", got)
	}
	var d queuedDetails
	if err := json.Unmarshal(env.Error.Details, &d); err != nil {
		f.t.Fatalf("decode details: %v", err)
	}
	if got := f.state(st.ID); got != run.StageStateAwaitingHostDispatch {
		f.t.Fatalf("queued stage %s reads %s, want awaiting_host_dispatch (untouched)", st.ID, got)
	}
	return d, env.Error.Message, string(env.Error.Details)
}

func holderIDs(hs []concurrencyHolder) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.StageID)
	}
	return out
}

func sameStageIDs(got []uuid.UUID, want ...uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestHostDispatch_SecondLocalImplementQueued is the issue's core
// counterfactual: a second local implement on the same host is QUEUED (409,
// stage untouched) behind the first, not admitted.
func TestHostDispatch_SecondLocalImplementQueued(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b := f.impl(), f.impl()

	resp := f.admitted(a, hostBody("h1"))
	if resp.Concurrency == nil || resp.Concurrency.Group != "local-implement:h1" || resp.Concurrency.Limit != 1 || resp.Concurrency.QueuedBefore {
		t.Fatalf("admitted concurrency block = %+v, want {local-implement:h1, 1, queued_before:false}", resp.Concurrency)
	}

	d, msg, _ := f.queued(b, hostBody("h1"))
	if d.Position != 1 || !sameStageIDs(holderIDs(d.Holders), a.ID) || d.Holders[0].RunID != a.RunID {
		t.Fatalf("queued details = %+v, want position 1 behind holder A", d)
	}
	if d.Group != "local-implement:h1" || d.Limit != 1 || d.QueueTTLSeconds != 60 || d.PollIntervalSeconds != 5 || d.StageID != b.ID.String() {
		t.Errorf("queued details = %+v, want group/limit/ttl/poll/stage_id", d)
	}
	if !strings.Contains(msg, "held by run "+a.RunID.String()) {
		t.Errorf("409 message %q does not name holder A", msg)
	}
	if row, ok := f.slot(b.ID); !ok || row.state != "queued" {
		t.Fatalf("B slot row = %+v (present %v), want queued", row, ok)
	}
}

// TestHostDispatch_StageBlocksVisible pins the `concurrency` block on all
// three stage reads (list, get, run-stage wait envelope), for a queued stage
// and a holder, and its release once the holder settles.
func TestHostDispatch_StageBlocksVisible(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b := f.impl(), f.impl()
	f.admitted(a, hostBody("h1"))
	f.queued(b, hostBody("h1"))

	for name, blk := range f.blocks(b) {
		if blk == nil {
			t.Fatalf("%s: B has no concurrency block", name)
		}
		if blk.Status != "awaiting_concurrency_slot" || blk.Position != 1 || !blk.WaiterLive ||
			!sameStageIDs(holderIDs(blk.Holders), a.ID) || blk.Group != "local-implement:h1" || blk.Limit != 1 {
			t.Errorf("%s: B block = %+v, want queued at 1 behind A with a live waiter", name, blk)
		}
	}
	aStage, _ := f.repo.GetStage(context.Background(), a.ID)
	for name, blk := range f.blocks(a) {
		if blk == nil || blk.Status != "holding" || blk.Host != "h1" || blk.AcquiredAt == nil ||
			blk.HeldDispatchedAt == nil || !blk.HeldDispatchedAt.Equal(*aStage.DispatchedAt) {
			t.Errorf("%s: A block = %+v, want holding on h1 for the admitted attempt", name, blk)
		}
	}

	// A waiter that stopped refreshing reads waiter_live:false.
	f.exec(`UPDATE stage_concurrency_slots SET last_seen_at = clock_timestamp() - interval '61 seconds' WHERE stage_id = $1`, b.ID)
	if blk := f.blocks(b)["get"]; blk == nil || blk.WaiterLive {
		t.Errorf("stale B block = %+v, want waiter_live:false", blk)
	}

	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.admitted(b, hostBody("h1"))
	for name, blk := range f.blocks(b) {
		if blk == nil || blk.Status != "holding" || len(blk.Holders) != 1 {
			t.Errorf("%s: B block after admission = %+v, want holding", name, blk)
		}
	}
	for name, blk := range f.blocks(a) {
		if blk != nil {
			t.Errorf("%s: settled A still carries a block %+v", name, blk)
		}
	}
}

// blocks reads the stage's concurrency block through all three read handlers.
func (f *scPG) blocks(st *run.Stage) map[string]*stageConcurrency {
	f.t.Helper()
	out := map[string]*stageConcurrency{}

	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+st.RunID.String()+"/stages", nil)
	req.SetPathValue("run_id", st.RunID.String())
	w := httptest.NewRecorder()
	f.srv.handleListRunStages(w, req)
	var list struct {
		Items []stageResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		f.t.Fatalf("list stages = %d %v:\n%s", w.Code, err, w.Body.String())
	}
	for i := range list.Items {
		if list.Items[i].ID == st.ID {
			out["list"] = list.Items[i].Concurrency
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/stages/"+st.ID.String(), nil)
	req.SetPathValue("stage_id", st.ID.String())
	w = httptest.NewRecorder()
	f.srv.handleGetStage(w, req)
	var one stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil || w.Code != http.StatusOK {
		f.t.Fatalf("get stage = %d %v:\n%s", w.Code, err, w.Body.String())
	}
	out["get"] = one.Concurrency

	req = httptest.NewRequest(http.MethodGet, "/v0/runs/"+st.RunID.String()+"/stages/"+st.ID.String(), nil)
	req.SetPathValue("run_id", st.RunID.String())
	req.SetPathValue("stage_id", st.ID.String())
	w = httptest.NewRecorder()
	f.srv.handleGetRunStage(w, withHostDispatchOperator(req))
	var wait runStageWaitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &wait); err != nil || w.Code != http.StatusOK {
		f.t.Fatalf("get run stage = %d %v:\n%s", w.Code, err, w.Body.String())
	}
	out["wait"] = wait.Concurrency
	return out
}

// TestHostDispatch_EmptyHoldersRendered is approval condition 2's rendering
// half: a stage queued behind an earlier WAITER (no live holder) renders
// holders as [] (never null) in the 409 details and the stage block, and the
// message names the empty case instead of an empty "held by".
func TestHostDispatch_EmptyHoldersRendered(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b, c := f.impl(), f.impl(), f.impl()
	f.admitted(a, hostBody("h1"))
	f.queued(b, hostBody("h1"))
	f.queued(c, hostBody("h1"))
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)

	d, msg, raw := f.queued(c, hostBody("h1")) // B is ahead and fresh; no holder
	if d.Position != 2 || !strings.Contains(raw, `"holders":[]`) {
		t.Fatalf("details = %s, want position 2 with holders []", raw)
	}
	if !strings.Contains(msg, "no live holders; queued behind earlier waiters") || strings.Contains(msg, "held by") {
		t.Errorf("message = %q, want the empty-holders wording", msg)
	}
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+c.ID.String(), nil)
	req.SetPathValue("stage_id", c.ID.String())
	rec := httptest.NewRecorder()
	f.srv.handleGetStage(rec, req)
	if !strings.Contains(rec.Body.String(), `"holders":[]`) {
		t.Errorf("stage block = %s, want holders []", rec.Body.String())
	}
}

func TestConcurrencyQueuedMessage_Arms(t *testing.T) {
	h := concurrency.Holder{RunID: uuid.New(), StageID: uuid.New()}
	for _, tc := range []struct {
		name string
		adm  concurrency.Admission
		want string
	}{
		{"held", concurrency.Admission{Position: 1, Holders: []concurrency.Holder{h}}, "held by run " + h.RunID.String() + " stage " + h.StageID.String()},
		{"contended", concurrency.Admission{Position: 1, Holders: []concurrency.Holder{}, Contended: true}, "no live holders; another admission held the group lock"},
		{"waiters", concurrency.Admission{Position: 2, Holders: []concurrency.Holder{}}, "no live holders; queued behind earlier waiters"},
	} {
		if got := concurrencyQueuedMessage("g", 1, tc.adm); !strings.Contains(got, tc.want) {
			t.Errorf("%s: message %q missing %q", tc.name, got, tc.want)
		}
	}
	if got := toConcurrencyHolders(nil); got == nil || len(got) != 0 {
		t.Errorf("toConcurrencyHolders(nil) = %#v, want a non-nil empty slice", got)
	}
}

// TestHostDispatch_CrashReleaseViaReapFailure: the existing reap machinery
// moves a crashed holder out of running, which releases its slot at once.
func TestHostDispatch_CrashReleaseViaReapFailure(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b := f.impl(), f.impl()
	f.admitted(a, hostBody("h1"))
	f.to(a.ID, run.StageStateRunning)
	f.queued(b, hostBody("h1"))

	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+a.RunID.String()+"/stages/"+a.ID.String()+"/reap-failure",
		strings.NewReader(`{"category":"C","reason":"runner died","expected_state":"running"}`))
	req.SetPathValue("run_id", a.RunID.String())
	req.SetPathValue("stage_id", a.ID.String())
	w := httptest.NewRecorder()
	f.srv.handleReapStageFailure(w, withHostDispatchOperator(req))
	if w.Code != http.StatusOK {
		t.Fatalf("reap-failure = %d:\n%s", w.Code, w.Body.String())
	}
	f.admitted(b, hostBody("h1"))
}

// TestHostDispatch_HeartbeatKeepsHolderLive is approval condition 5: a
// RUNNING holder whose dispatched_at is past RunningStaleAfter (the slot row
// kept on the same attempt) stays live ONLY because the REAL progress
// handler stamped a fresh progress.reported_at.
func TestHostDispatch_HeartbeatKeepsHolderLive(t *testing.T) {
	setup := func(t *testing.T) (*scPG, *run.Stage, *run.Stage) {
		f := newSCPG(t, pgStore)
		a, b := f.impl(), f.impl()
		f.admitted(a, hostBody("h1"))
		f.to(a.ID, run.StageStateRunning)
		f.backdateHolder(a.ID, "50 minutes")
		return f, a, b
	}
	t.Run("no heartbeat: past the backstop, B is admitted", func(t *testing.T) {
		f, _, b := setup(t)
		f.admitted(b, hostBody("h1"))
	})
	t.Run("fresh heartbeat: A still holds", func(t *testing.T) {
		f, a, b := setup(t)
		w := postProgressDirect(t, f.srv, a.RunID.String(), a.ID.String(), progressBody(t, stageProgressReport{
			LastEvent: "assistant", TurnsThisAttempt: 3, TokensThisAttempt: 100,
		}))
		if w.Code/100 != 2 {
			t.Fatalf("progress POST = %d:\n%s", w.Code, w.Body.String())
		}
		d, _, _ := f.queued(b, hostBody("h1"))
		if !sameStageIDs(holderIDs(d.Holders), a.ID) {
			t.Fatalf("holders = %+v, want A kept live by its heartbeat", d.Holders)
		}
	})
}

// TestHostDispatch_ReopenedStageQueuesBehindLiveHolder: a stage re-opened by
// the retry or fix-up path comes back through the marker as a NEW episode —
// its previous-episode held row restarts as queued at the tail behind the
// current holder and is not counted as a holder itself.
func TestHostDispatch_ReopenedStageQueuesBehindLiveHolder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		park   []run.StageState
		reopen func(f *scPG, id uuid.UUID) error
	}{
		{"retry", []run.StageState{run.StageStateRunning, run.StageStateFailed}, func(f *scPG, id uuid.UUID) error {
			_, err := run.RetryStage(context.Background(), f.repo, id, run.RetryOptions{})
			return err
		}},
		{"fixup", []run.StageState{run.StageStateRunning, run.StageStateAwaitingApproval}, func(f *scPG, id uuid.UUID) error {
			_, err := run.FixupStage(context.Background(), f.repo, id, run.FixupOptions{MaxPasses: 1, HardCeiling: 3})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSCPG(t, pgStore)
			a, b, c := f.impl(), f.impl(), f.impl()
			f.admitted(a, hostBody("h1"))
			f.to(a.ID, tc.park...)
			old, _ := f.slot(a.ID)
			f.admitted(b, hostBody("h1"))
			if err := tc.reopen(f, a.ID); err != nil {
				t.Fatalf("re-open: %v", err)
			}
			if got := f.state(a.ID); got != run.StageStatePending {
				t.Fatalf("re-opened A reads %s, want pending", got)
			}
			// Orchestrator.Advance parks a host-dispatched stage at
			// awaiting_host_dispatch before the marker sees it.
			f.to(a.ID, run.StageStateAwaitingHostDispatch)
			d, _, _ := f.queued(a, hostBody("h1"))
			if d.Position != 1 || !sameStageIDs(holderIDs(d.Holders), b.ID) {
				t.Fatalf("re-opened A details = %+v, want position 1 behind B", d)
			}
			row, _ := f.slot(a.ID)
			if row.state != "queued" || old.acquiredAt == nil || !row.enqueuedAt.After(*old.acquiredAt) {
				t.Fatalf("A slot row = %+v (old %+v), want queued with a restarted enqueued_at", row, old)
			}
			dc, _, _ := f.queued(c, hostBody("h1"))
			if !sameStageIDs(holderIDs(dc.Holders), b.ID) || dc.Position != 2 {
				t.Fatalf("C details = %+v, want holders [B] only and A ahead", dc)
			}
		})
	}
}

// TestHostDispatch_DefaultGroupKeyAndLimitShipped (done-means): the default
// group is per host at limit 1, and an absent body lands in
// local-implement:unknown.
func TestHostDispatch_DefaultGroupKeyAndLimitShipped(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b, c, d, e := f.impl(), f.impl(), f.impl(), f.impl(), f.impl()
	f.admitted(a, hostBody("h1"))
	if q, _, _ := f.queued(b, hostBody("h1")); q.Group != "local-implement:h1" || q.Limit != 1 {
		t.Fatalf("h1 queue = %+v, want local-implement:h1 limit 1", q)
	}
	if resp := f.admitted(c, hostBody("h2")); resp.Concurrency.Group != "local-implement:h2" {
		t.Fatalf("h2 admission group = %q", resp.Concurrency.Group)
	}
	if resp := f.admitted(d, ""); resp.Concurrency.Group != "local-implement:unknown" {
		t.Fatalf("no-body admission group = %q, want local-implement:unknown", resp.Concurrency.Group)
	}
	if q, _, _ := f.queued(e, `{"host":""}`); q.Group != "local-implement:unknown" {
		t.Fatalf("empty-host queue group = %q, want local-implement:unknown", q.Group)
	}
}

// TestHostDispatch_InvalidHostRejected: every malformed body answers 400
// validation_failed with the stage untouched and no slot row.
func TestHostDispatch_InvalidHostRejected(t *testing.T) {
	f := newSCPG(t, pgStore)
	for name, body := range map[string]string{
		"too long":       hostBody(strings.Repeat("h", 300)),
		"slash":          hostBody("a/b"),
		"unknown key":    `{"host":"h1","x":1}`,
		"malformed json": `{"host":`,
		"oversized":      `{"host":"h1","pad":"` + strings.Repeat("x", 5000) + `"}`,
		"nonce too long": `{"host":"h1","admission_nonce":"` + strings.Repeat("n", 65) + `"}`,
		"nonce bad char": `{"host":"h1","admission_nonce":"n/1"}`,
		"nonce no host":  `{"admission_nonce":"n.1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := f.impl()
			w := f.mark(st, body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "validation_failed") {
				t.Fatalf("marker = %d, want 400 validation_failed:\n%s", w.Code, w.Body.String())
			}
			if got := f.state(st.ID); got != run.StageStateAwaitingHostDispatch {
				t.Fatalf("stage reads %s, want awaiting_host_dispatch", got)
			}
			if _, ok := f.slot(st.ID); ok {
				t.Fatal("a rejected marker wrote a slot row")
			}
		})
	}
}

// TestHostDispatch_AdmissionNonceRecordedAndEchoed (approval condition 3): an
// admission records the request's admission_nonce on the held slot and echoes
// it in the 200 block; every stage read's holding block carries it; a queued
// stage's block carries none; an admission without a nonce carries none.
func TestHostDispatch_AdmissionNonceRecordedAndEchoed(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b := f.impl(), f.impl()
	resp := f.admitted(a, `{"host":"h1","admission_nonce":"nonce-a-1"}`)
	if resp.Concurrency == nil || resp.Concurrency.AdmissionNonce != "nonce-a-1" {
		t.Fatalf("admission block = %+v, want admission_nonce echoed", resp.Concurrency)
	}
	f.queued(b, `{"host":"h1","admission_nonce":"nonce-b-1"}`)
	for name, blk := range f.blocks(a) {
		if blk == nil || blk.Status != "holding" || blk.AdmissionNonce != "nonce-a-1" {
			t.Errorf("%s: A block = %+v, want holding with admission_nonce nonce-a-1", name, blk)
		}
	}
	for name, blk := range f.blocks(b) {
		if blk == nil || blk.AdmissionNonce != "" {
			t.Errorf("%s: queued B block = %+v, want no admission_nonce", name, blk)
		}
	}
	// B is admitted by a nonce-less marker (a direct dispatch): no nonce.
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	if resp := f.admitted(b, hostBody("h1")); resp.Concurrency == nil || resp.Concurrency.AdmissionNonce != "" {
		t.Fatalf("nonce-less admission block = %+v, want no admission_nonce", resp.Concurrency)
	}
	if blk := f.blocks(b)["get"]; blk == nil || blk.Status != "holding" || blk.AdmissionNonce != "" {
		t.Fatalf("B block = %+v, want holding with no admission_nonce", blk)
	}
}

// TestHostDispatch_TerminalRunRefused: a stage whose run is succeeded, failed
// or cancelled is never host-spawned — 409 dispatch_not_admissible naming the
// run state, the stage READ BACK untouched and no slot row — with or without a
// slot store, and on the already-dispatched idempotent arm too.
func TestHostDispatch_TerminalRunRefused(t *testing.T) {
	paths := map[run.State][]run.State{
		run.StateSucceeded: {run.StateRunning, run.StateSucceeded},
		run.StateFailed:    {run.StateFailed},
		run.StateCancelled: {run.StateCancelled},
	}
	for name, store := range map[string]func(*pgxpool.Pool) concurrency.Store{"slot store": pgStore, "no store": nil} {
		for terminal, path := range paths {
			for _, dispatched := range []bool{false, true} {
				t.Run(name+"/"+string(terminal)+map[bool]string{false: "/parked", true: "/dispatched"}[dispatched], func(t *testing.T) {
					f := newSCPG(t, store)
					st := f.impl()
					want := run.StageStateAwaitingHostDispatch
					if dispatched {
						f.to(st.ID, run.StageStateDispatched)
						want = run.StageStateDispatched
					}
					for _, to := range path {
						if _, err := f.repo.TransitionRun(context.Background(), st.RunID, to); err != nil {
							t.Fatalf("run -> %s: %v", to, err)
						}
					}
					w := f.mark(st, hostBody("h1"))
					if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "dispatch_not_admissible") ||
						!strings.Contains(w.Body.String(), `"run_state":"`+string(terminal)+`"`) {
						t.Fatalf("marker = %d, want 409 dispatch_not_admissible naming run_state %s:\n%s", w.Code, terminal, w.Body.String())
					}
					if got := f.state(st.ID); got != want {
						t.Fatalf("stage reads %s, want %s (untouched)", got, want)
					}
					if _, ok := f.slot(st.ID); ok {
						t.Fatal("a refused marker wrote a slot row")
					}
				})
			}
		}
	}
}

// TestHostDispatch_PlanStageNotGrouped: an undeclared non-implement stage is
// ungrouped — admitted beside an implement holder, no block, no row.
func TestHostDispatch_PlanStageNotGrouped(t *testing.T) {
	f := newSCPG(t, pgStore)
	a := f.impl()
	p := f.stage("kuhlman-labs/fishhawk", run.StageTypePlan)
	f.admitted(a, hostBody("h1"))
	resp := f.admitted(p, hostBody("h1"))
	if resp.Concurrency != nil {
		t.Fatalf("plan admission carries a concurrency block %+v", resp.Concurrency)
	}
	if _, ok := f.slot(p.ID); ok {
		t.Fatal("ungrouped plan stage wrote a slot row")
	}
}

func concurrencySpec(block string) string {
	doc := `version: "2"
workflows:
  wf:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	if block != "" {
		doc += "        concurrency:" + block + "\n"
	}
	return doc
}

// TestHostDispatch_SpecLimitTwoAdmitsBoth (done-means): a declared limit 2
// on the default group admits two and queues the third.
func TestHostDispatch_SpecLimitTwoAdmitsBoth(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b, c := f.impl(), f.impl(), f.impl()
	for _, st := range []*run.Stage{a, b, c} {
		f.setSpec(st, concurrencySpec("\n          limit: 2"))
	}
	f.admitted(a, hostBody("h1"))
	if resp := f.admitted(b, hostBody("h1")); resp.Concurrency.Limit != 2 || resp.Concurrency.Group != "local-implement:h1" {
		t.Fatalf("B admission block = %+v, want local-implement:h1 limit 2", resp.Concurrency)
	}
	if d, _, _ := f.queued(c, hostBody("h1")); d.Limit != 2 || len(d.Holders) != 2 {
		t.Fatalf("C details = %+v, want limit 2 behind two holders", d)
	}
}

// TestHostDispatch_SpecNamedGroupRepoScoped: a named group is repository
// scoped — queued behind a same-repo holder, admitted beside another repo's.
func TestHostDispatch_SpecNamedGroupRepoScoped(t *testing.T) {
	f := newSCPG(t, pgStore)
	a := f.stage("org/one", run.StageTypeImplement)
	b := f.stage("org/one", run.StageTypeImplement)
	c := f.stage("org/two", run.StageTypeImplement)
	for _, st := range []*run.Stage{a, b, c} {
		f.setSpec(st, concurrencySpec("\n          group: deploy-target"))
	}
	if resp := f.admitted(a, hostBody("h1")); resp.Concurrency.Group != "spec:org/one:deploy-target" {
		t.Fatalf("A group = %q", resp.Concurrency.Group)
	}
	if d, _, _ := f.queued(b, hostBody("h2")); d.Group != "spec:org/one:deploy-target" || !sameStageIDs(holderIDs(d.Holders), a.ID) {
		t.Fatalf("B details = %+v, want queued behind A in the repo group (host-independent)", d)
	}
	f.admitted(c, hostBody("h1"))
}

// TestHostDispatch_SpecParseFailureFallsBackToDefault: an unparseable spec
// falls back to the restrictive default (implement limit 1).
func TestHostDispatch_SpecParseFailureFallsBackToDefault(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b := f.impl(), f.impl()
	for _, st := range []*run.Stage{a, b} {
		f.setSpec(st, "version: [not a spec")
	}
	f.admitted(a, hostBody("h1"))
	if d, _, _ := f.queued(b, hostBody("h1")); d.Limit != 1 || d.Group != "local-implement:h1" {
		t.Fatalf("details = %+v, want the default group at limit 1", d)
	}
}

// TestHostDispatch_AlreadyDispatchedArmUnchanged: the idempotent arm is
// byte-identical — transitioned:false, no block, no slot row.
func TestHostDispatch_AlreadyDispatchedArmUnchanged(t *testing.T) {
	f := newSCPG(t, pgStore)
	a := f.impl()
	f.to(a.ID, run.StageStateDispatched)
	w := f.mark(a, hostBody("h1"))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "concurrency") {
		t.Fatalf("marker = %d %s, want 200 without a concurrency block", w.Code, w.Body.String())
	}
	if resp := decodeHostDispatch(t, w); resp.Transitioned {
		t.Fatalf("resp = %+v, want transitioned:false", resp)
	}
	if _, ok := f.slot(a.ID); ok {
		t.Fatal("idempotent arm wrote a slot row")
	}
}

// TestHostDispatch_NilStoreUnchanged: no slot store = today's marker (both
// admitted, body ignored, no block).
func TestHostDispatch_NilStoreUnchanged(t *testing.T) {
	f := newSCPG(t, nil)
	a, b := f.impl(), f.impl()
	for _, st := range []*run.Stage{a, b} {
		w := f.mark(st, hostBody("not/valid"))
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "concurrency") {
			t.Fatalf("nil-store marker = %d %s, want a plain 200", w.Code, w.Body.String())
		}
		if got := f.state(st.ID); got != run.StageStateDispatched {
			t.Fatalf("stage reads %s, want dispatched", got)
		}
	}
	if blk := f.blocks(a)["get"]; blk != nil {
		t.Fatalf("nil store rendered a block %+v", blk)
	}
}

// driftStore moves the stage under the marker (after its load, before the
// admission's row lock) and then delegates to the real store, so the
// admission's drift check fires on committed state.
type driftStore struct {
	concurrency.Store
	repo run.Repository
	to   []run.StageState
}

func (d driftStore) Admit(ctx context.Context, req concurrency.Request) (concurrency.Admission, error) {
	for _, s := range d.to {
		if _, err := d.repo.TransitionStage(ctx, req.StageID, s, nil); err != nil {
			return concurrency.Admission{}, err
		}
	}
	return d.Store.Admit(ctx, req)
}

// TestHostDispatch_DriftDuringAdmissionReclassified: a stage moved between
// the marker's load and the admission is reclassified exactly as the bare
// CAS's drift — cancelled → 409 dispatch_not_admissible, dispatched by
// another writer → 200 transitioned:false — and no slot row is written.
func TestHostDispatch_DriftDuringAdmissionReclassified(t *testing.T) {
	for _, tc := range []struct {
		name     string
		to       []run.StageState
		wantCode int
		wantBody string
	}{
		{"cancelled", []run.StageState{run.StageStateCancelled}, http.StatusConflict, "dispatch_not_admissible"},
		{"dispatched by another writer", []run.StageState{run.StageStateDispatched}, http.StatusOK, `"transitioned":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSCPG(t, func(pool *pgxpool.Pool) concurrency.Store {
				return driftStore{Store: concurrency.NewPostgresStore(pool), repo: run.NewPostgresRepository(pool), to: tc.to}
			})
			a := f.impl()
			w := f.mark(a, hostBody("h1"))
			if w.Code != tc.wantCode || !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Fatalf("marker = %d %s, want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantBody)
			}
			if _, ok := f.slot(a.ID); ok {
				t.Fatal("a drifted admission wrote a slot row")
			}
		})
	}
}

// errStore fails every call.
type errStore struct{}

func (errStore) Admit(context.Context, concurrency.Request) (concurrency.Admission, error) {
	return concurrency.Admission{}, errors.New("db down")
}

func (errStore) StatusForStages(context.Context, []uuid.UUID) (map[uuid.UUID]concurrency.Status, error) {
	return nil, errors.New("db down")
}

// TestHostDispatch_StoreErrorFailsClosed: a non-drift admission error answers
// 500 and leaves the stage parked (nothing spawns on a 5xx), and a status
// read error omits the block rather than failing the read.
func TestHostDispatch_StoreErrorFailsClosed(t *testing.T) {
	f := newSCPG(t, func(*pgxpool.Pool) concurrency.Store { return errStore{} })
	a := f.impl()
	w := f.mark(a, hostBody("h1"))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "internal_error") {
		t.Fatalf("marker = %d %s, want 500 internal_error", w.Code, w.Body.String())
	}
	if got := f.state(a.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("stage reads %s after a store error, want awaiting_host_dispatch", got)
	}
	for name, blk := range f.blocks(a) {
		if blk != nil {
			t.Errorf("%s: block %+v rendered on a status read error", name, blk)
		}
	}
}

// TestHostDispatch_ConcurrencyAuditRows: stage_concurrency_queued is written
// once per queue episode (not per poll) and stage_concurrency_admitted on
// every grouped admission.
func TestHostDispatch_ConcurrencyAuditRows(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b := f.impl(), f.impl()
	f.admitted(a, hostBody("h1"))
	f.queued(b, hostBody("h1"))
	f.queued(b, hostBody("h1"))
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.admitted(b, hostBody("h1"))

	rows := f.auditRows(b.RunID)
	var queued, admitted []map[string]any
	for _, r := range rows {
		switch r.category {
		case CategoryStageConcurrencyQueued:
			queued = append(queued, r.payload)
		case CategoryStageConcurrencyAdmitted:
			admitted = append(admitted, r.payload)
		}
	}
	if len(queued) != 1 || queued[0]["group"] != "local-implement:h1" || queued[0]["host"] != "h1" || queued[0]["position"] != float64(1) {
		t.Fatalf("queued rows = %+v, want exactly one for the episode", queued)
	}
	if len(admitted) != 1 || admitted[0]["queued_before"] != true {
		t.Fatalf("admitted rows = %+v, want one with queued_before:true", admitted)
	}
	if aRows := f.auditRows(a.RunID); len(aRows) != 1 || aRows[0].category != CategoryStageConcurrencyAdmitted || aRows[0].payload["queued_before"] != false {
		t.Fatalf("A audit rows = %+v, want one admitted row with queued_before:false", aRows)
	}
	for _, c := range []string{CategoryStageConcurrencyQueued, CategoryStageConcurrencyAdmitted} {
		if !audit.IsKnownCategory(c) {
			t.Errorf("%s is not registered in audit/categories.go", c)
		}
	}
}

type scAuditRow struct {
	category string
	payload  map[string]any
}

func (f *scPG) auditRows(runID uuid.UUID) []scAuditRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT category, payload FROM audit_entries WHERE run_id = $1 AND category LIKE 'stage_concurrency_%' ORDER BY sequence`, runID)
	if err != nil {
		f.t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []scAuditRow
	for rows.Next() {
		var r scAuditRow
		var raw []byte
		if err := rows.Scan(&r.category, &raw); err != nil {
			f.t.Fatalf("scan audit: %v", err)
		}
		_ = json.Unmarshal(raw, &r.payload)
		out = append(out, r)
	}
	return out
}

// TestHostDispatch_OtherAccountHolderNotDisclosed is approval condition 6 at
// the handler: a holder in another account is neither counted (the other
// account's stage is admitted) nor disclosed in the 409 details or the stage
// block. The pgtest role is a superuser and BYPASSES RLS — the production
// runtime posture today — so the scoping under test is the store's explicit
// runs.account_id match, not RLS.
func TestHostDispatch_OtherAccountHolderNotDisclosed(t *testing.T) {
	f := newSCPG(t, pgStore)
	acct1, acct2 := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{acct1, acct2} {
		f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "cc-"+id.String())
	}
	a, b, c := f.impl(), f.impl(), f.impl()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct1, a.RunID)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct2, b.RunID)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct1, c.RunID)
	f.admitted(a, hostBody("h1"))
	f.admitted(b, hostBody("h1")) // same host label, other account
	if blk := f.blocks(b)["get"]; blk == nil || !sameStageIDs(holderIDs(blk.Holders), b.ID) {
		t.Fatalf("account-2 block = %+v, want holders [B] only", blk)
	}
	d, _, raw := f.queued(c, hostBody("h1"))
	if !sameStageIDs(holderIDs(d.Holders), a.ID) || strings.Contains(raw, b.ID.String()) {
		t.Fatalf("account-1 details = %s, want holders [A] and no trace of B", raw)
	}
}

// TestOpenAPIDocumentsConcurrency pins the API contract doc to the surface:
// the optional body, the 409 code and the Stage concurrency schema.
func TestOpenAPIDocumentsConcurrency(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{"concurrency_slot_queued", "StageConcurrency:", "HostDispatchRequest:", "awaiting_concurrency_slot", "held_dispatched_at", "admission_nonce"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml does not document %q", want)
		}
	}
}
