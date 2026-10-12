package specialistjob

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Every time fixture here is seeded by SQL relative to the DATABASE clock
// (clock_timestamp() - interval), never a Go time.Now(): the staleness the
// store judges is on the database clock (AGENTS.md cross-clock trap #3048).

type storeFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	repo  run.Repository
	store *Store
	slots *concurrency.PostgresStore
}

func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	return &storeFixture{t: t, pool: pool, repo: run.NewPostgresRepository(pool), store: NewStore(pool), slots: concurrency.NewPostgresStore(pool)}
}

var operator = Requester{Seat: SeatOperator, ActorKind: "user", Subject: "github:captain"}

func (f *storeFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *storeFixture) account() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "sj-"+id.String())
	return id
}

// newRun creates a run, in acct when non-nil.
func (f *storeFixture) newRun(acct *uuid.UUID) uuid.UUID {
	f.t.Helper()
	r, err := f.repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	if acct != nil {
		f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, *acct, r.ID)
	}
	return r.ID
}

// parse builds and parses a request of kind k whose subject names runID
// ("" = no run).
func parse(t *testing.T, k Kind, runID string) *Request {
	t.Helper()
	doc := requestDoc(k, validInputs(k))
	subject := map[string]any{"head_sha": sha1}
	if runID != "" {
		subject["run_id"] = runID
	}
	doc["subject"] = subject
	req, err := ParseRequest(mustJSON(t, doc))
	if err != nil {
		t.Fatalf("ParseRequest(%s): %v", k, err)
	}
	return req
}

func (f *storeFixture) create(acct *uuid.UUID, k Kind, runID string) *Job {
	f.t.Helper()
	job, err := f.store.Create(context.Background(), CreateParams{AccountID: acct, Request: parse(f.t, k, runID), Requester: operator})
	if err != nil {
		f.t.Fatalf("Create(%s): %v", k, err)
	}
	return job
}

func (f *storeFixture) admit(acct *uuid.UUID, id uuid.UUID, nonce string) (*Admission, error) {
	return f.store.AdmitJob(context.Background(), AdmitParams{AccountID: acct, JobID: id, Host: "h1", Nonce: nonce, Actor: Actor{Kind: audit.ActorUser, Subject: "github:captain"}})
}

func (f *storeFixture) mustAdmit(acct *uuid.UUID, id uuid.UUID, nonce string) *Admission {
	f.t.Helper()
	a, err := f.admit(acct, id, nonce)
	if err != nil {
		f.t.Fatalf("AdmitJob: %v", err)
	}
	if !a.Admitted {
		f.t.Fatalf("AdmitJob = queued at %d (contended %v), want admitted", a.Position, a.Contended)
	}
	return a
}

func (f *storeFixture) mustQueue(acct *uuid.UUID, id uuid.UUID, nonce string) *Admission {
	f.t.Helper()
	a, err := f.admit(acct, id, nonce)
	if err != nil {
		f.t.Fatalf("AdmitJob: %v", err)
	}
	if a.Admitted {
		f.t.Fatalf("AdmitJob admitted, want queued")
	}
	return a
}

// jobState reads the row's committed state back.
func (f *storeFixture) jobState(id uuid.UUID) State {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM specialist_jobs WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("read job state: %v", err)
	}
	return State(s)
}

type auditRow struct {
	category, actorKind, actorSubject string
	account                           *uuid.UUID
	payload                           map[string]any
	sequence                          int64
}

// entries reads the specialist_job_* entries of one run chain (runID set) or
// of the global chain (runID nil), in order.
func (f *storeFixture) entries(runID *uuid.UUID) []auditRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT category, COALESCE(actor_kind, ''), COALESCE(actor_subject, ''), account_id, payload, sequence
FROM audit_entries WHERE run_id IS NOT DISTINCT FROM $1 AND category LIKE 'specialist_job_%' ORDER BY sequence`, runID)
	if err != nil {
		f.t.Fatalf("read audit: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (auditRow, error) {
		var a auditRow
		var raw []byte
		if err := r.Scan(&a.category, &a.actorKind, &a.actorSubject, &a.account, &raw, &a.sequence); err != nil {
			return a, err
		}
		return a, json.Unmarshal(raw, &a.payload)
	})
	if err != nil {
		f.t.Fatalf("scan audit: %v", err)
	}
	return out
}

func asJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func completion(t *testing.T, k Kind, nonce string) *Completion {
	t.Helper()
	doc := completionDoc(k, "pass", validEvidence(k))
	doc["execution_nonce"] = nonce
	c, err := ParseCompletion(mustJSON(t, doc), k)
	if err != nil {
		t.Fatalf("ParseCompletion: %v", err)
	}
	return c
}

// seedStale backdates a job's heartbeat past JobHeartbeatTTL on the DB clock.
func (f *storeFixture) seedStale(id uuid.UUID) {
	f.exec(`UPDATE specialist_jobs SET last_seen_at = clock_timestamp() - make_interval(secs => $2) - interval '30 seconds' WHERE id = $1`,
		id, concurrency.JobHeartbeatTTL.Seconds())
}

// verifyStage parks a stage at awaiting_host_dispatch and returns the
// local-verify:h1 admission request for it.
func (f *storeFixture) verifyStage() concurrency.Request {
	f.t.Helper()
	ctx := context.Background()
	st, err := f.repo.CreateStage(ctx, run.CreateStageParams{
		RunID: f.newRun(nil), Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		f.t.Fatalf("create stage: %v", err)
	}
	if _, err := f.repo.TransitionStage(ctx, st.ID, run.StageStateAwaitingHostDispatch, nil); err != nil {
		f.t.Fatalf("park stage: %v", err)
	}
	return concurrency.Request{StageID: st.ID, RunID: st.RunID, From: run.StageStateAwaitingHostDispatch,
		GroupKey: concurrency.VerifyGroupKey("h1"), Limit: concurrency.DefaultVerifyLimit, Host: "h1"}
}

func (f *storeFixture) admitStage(r concurrency.Request) concurrency.Admission {
	f.t.Helper()
	a, err := f.slots.Admit(context.Background(), r)
	if err != nil {
		f.t.Fatalf("stage Admit: %v", err)
	}
	return a
}

func TestStore_CreateRecordsInputsAndRequester(t *testing.T) {
	f := newStoreFixture(t)
	runID := f.newRun(nil)
	req := parse(t, KindOverlapScan, runID.String())
	job, err := f.store.Create(context.Background(), CreateParams{Request: req, Requester: operator})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if job.State != StateQueued || job.Kind != KindOverlapScan || job.Requester != operator || job.Subject.RunID != runID.String() {
		t.Fatalf("job = %+v, want queued overlap_scan with the operator requester on the run", job)
	}
	got := f.entries(&runID)
	if len(got) != 1 || got[0].category != CategoryRequested {
		t.Fatalf("run-chain entries = %+v, want exactly one %s", got, CategoryRequested)
	}
	e := got[0]
	if job.RequestedSequence != e.sequence {
		t.Errorf("requested_sequence = %d, want the entry's %d", job.RequestedSequence, e.sequence)
	}
	// Semantic JSON equality: JSONB normalizes key order and whitespace.
	wantInputs := asJSON(t, mustJSON(t, validInputs(KindOverlapScan)))
	if !reflect.DeepEqual(e.payload["inputs"], wantInputs) {
		t.Errorf("payload.inputs = %v, want the request inputs %v", e.payload["inputs"], wantInputs)
	}
	wantRequester := map[string]any{"seat": "operator", "actor_kind": "user", "subject": "github:captain"}
	if !reflect.DeepEqual(e.payload["requester"], wantRequester) {
		t.Errorf("payload.requester = %v, want %v", e.payload["requester"], wantRequester)
	}
	if e.actorKind != "user" || e.actorSubject != "github:captain" {
		t.Errorf("entry actor = %s/%s, want user/github:captain", e.actorKind, e.actorSubject)
	}
	if e.payload["job_id"] != job.ID.String() || e.payload["kind"] != "overlap_scan" {
		t.Errorf("payload job_id/kind = %v/%v", e.payload["job_id"], e.payload["kind"])
	}
	if !reflect.DeepEqual(asJSON(t, job.Inputs), wantInputs) {
		t.Errorf("row inputs = %s, want the request inputs", job.Inputs)
	}
}

func TestStore_CreateWithoutRunUsesGlobalChain(t *testing.T) {
	f := newStoreFixture(t)
	acct := f.account()
	job := f.create(&acct, KindInventory, "")
	var rows []auditRow
	for _, e := range f.entries(nil) {
		if e.payload["job_id"] == job.ID.String() {
			rows = append(rows, e)
		}
	}
	if len(rows) != 1 || rows[0].category != CategoryRequested || rows[0].account == nil || *rows[0].account != acct {
		t.Fatalf("global entries for the job = %+v, want one %s in account %s", rows, CategoryRequested, acct)
	}
	if job.AccountID == nil || *job.AccountID != acct {
		t.Fatalf("job account = %v, want %s", job.AccountID, acct)
	}
}

func TestStore_CreateRefusesForeignAccountRun(t *testing.T) {
	f := newStoreFixture(t)
	mine, theirs := f.account(), f.account()
	runID := f.newRun(&theirs)
	_, err := f.store.Create(context.Background(), CreateParams{AccountID: &mine, Request: parse(t, KindInventory, runID.String()), Requester: operator})
	if !errors.Is(err, ErrSubjectRunNotFound) {
		t.Fatalf("Create on a foreign-account run = %v, want ErrSubjectRunNotFound", err)
	}
	if got := f.entries(&runID); len(got) != 0 {
		t.Fatalf("foreign run chain gained %d specialist entries, want 0", len(got))
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM specialist_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("job rows = %d (%v), want 0", n, err)
	}
}

func TestStore_CreateParamRefusals(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	inv := parse(t, KindInventory, "")
	cases := map[string]func() error{
		"draft via Create": func() error {
			_, err := f.store.Create(ctx, CreateParams{Request: parse(t, KindDraft, ""), Requester: operator})
			return err
		},
		"runner kind via CreateCompleted": func() error {
			_, err := f.store.CreateCompleted(ctx, CreateCompletedParams{Request: inv, Requester: operator, Text: "x"})
			return err
		},
		"nil request": func() error { _, err := f.store.Create(ctx, CreateParams{Requester: operator}); return err },
		"empty inputs": func() error {
			_, err := f.store.Create(ctx, CreateParams{Request: &Request{Kind: KindInventory}, Requester: operator})
			return err
		},
		"bad seat": func() error {
			_, err := f.store.Create(ctx, CreateParams{Request: inv, Requester: Requester{Seat: "x", ActorKind: "user", Subject: "s"}})
			return err
		},
		"empty subject": func() error {
			_, err := f.store.Create(ctx, CreateParams{Request: inv, Requester: Requester{Seat: SeatOperator, ActorKind: "user"}})
			return err
		},
		"bad run id": func() error {
			r := parse(t, KindInventory, "")
			r.Subject.RunID = "not-a-uuid"
			_, err := f.store.Create(ctx, CreateParams{Request: r, Requester: operator})
			return err
		},
		"admit nil id":    func() error { _, err := f.store.AdmitJob(ctx, AdmitParams{Nonce: "n"}); return err },
		"admit no nonce":  func() error { _, err := f.store.AdmitJob(ctx, AdmitParams{JobID: uuid.New()}); return err },
		"complete nil id": func() error { _, err := f.store.Complete(ctx, CompleteParams{Completion: &Completion{}}); return err },
		"complete nil completion": func() error {
			_, err := f.store.Complete(ctx, CompleteParams{JobID: uuid.New()})
			return err
		},
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: err = %v, want ErrInvalidParams", name, err)
		}
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM specialist_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("job rows after refusals = %d (%v), want 0", n, err)
	}
}

func TestStore_CreateCompletedDraft(t *testing.T) {
	f := newStoreFixture(t)
	runID := f.newRun(nil)
	req := parse(t, KindDraft, runID.String())
	job, err := f.store.CreateCompleted(context.Background(), CreateCompletedParams{Request: req, Requester: operator, Text: "# Specialist jobs\n"})
	if err != nil {
		t.Fatalf("CreateCompleted: %v", err)
	}
	if job.State != StateCompleted || job.Verdict != VerdictPass || job.CompletedAt == nil || job.CompletedSequence == nil {
		t.Fatalf("draft job = %+v, want completed pass", job)
	}
	got := f.entries(&runID)
	if len(got) != 2 || got[0].category != CategoryRequested || got[1].category != CategoryCompleted {
		t.Fatalf("entries = %+v, want exactly requested then completed", got)
	}
	if ev, _ := got[1].payload["evidence"].(map[string]any); ev["text"] != "# Specialist jobs\n" {
		t.Errorf("completed evidence = %v, want the rendered text", got[1].payload["evidence"])
	}
	if *job.CompletedSequence != got[1].sequence {
		t.Errorf("completed_sequence = %d, want %d", *job.CompletedSequence, got[1].sequence)
	}
}

func TestStore_CompleteRefusesNotRunning(t *testing.T) {
	f := newStoreFixture(t)
	runID := f.newRun(nil)
	job := f.create(nil, KindInventory, runID.String())
	_, err := f.store.Complete(context.Background(), CompleteParams{JobID: job.ID, Completion: completion(t, KindInventory, "n1")})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Complete(queued) = %v, want ErrNotRunning", err)
	}
	if s := f.jobState(job.ID); s != StateQueued {
		t.Fatalf("row state after refusal = %s, want queued", s)
	}
	if got := f.entries(&runID); len(got) != 1 {
		t.Fatalf("entries = %+v, want only the requested entry", got)
	}
}

func TestStore_CompleteRefusesNonceMismatch(t *testing.T) {
	f := newStoreFixture(t)
	runID := f.newRun(nil)
	job := f.create(nil, KindInventory, runID.String())
	f.mustAdmit(nil, job.ID, "n1")
	_, err := f.store.Complete(context.Background(), CompleteParams{JobID: job.ID, Completion: completion(t, KindInventory, "n2")})
	if !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("Complete(wrong nonce) = %v, want ErrNonceMismatch", err)
	}
	if s := f.jobState(job.ID); s != StateRunning {
		t.Fatalf("row state after refusal = %s, want running", s)
	}
	if got := f.entries(&runID); len(got) != 1 {
		t.Fatalf("entries = %+v, want only the requested entry", got)
	}
}

func TestStore_CompleteRefusesKindMismatch(t *testing.T) {
	f := newStoreFixture(t)
	runID := f.newRun(nil)
	job := f.create(nil, KindInventory, runID.String())
	f.mustAdmit(nil, job.ID, "n1")
	_, err := f.store.Complete(context.Background(), CompleteParams{JobID: job.ID, Completion: completion(t, KindOverlapScan, "n1")})
	if !errors.Is(err, ErrKindMismatch) {
		t.Fatalf("Complete(overlap on inventory) = %v, want ErrKindMismatch", err)
	}
	if s := f.jobState(job.ID); s != StateRunning {
		t.Fatalf("row state after refusal = %s, want running", s)
	}
}

func TestStore_CompleteRecordsTypedResultWithRequester(t *testing.T) {
	f := newStoreFixture(t)
	runID := f.newRun(nil)
	job := f.create(nil, KindInventory, runID.String())
	f.mustAdmit(nil, job.ID, "n1")
	runner := Actor{Kind: audit.ActorAgent, Subject: "runner:someone-else"}
	done, err := f.store.Complete(context.Background(), CompleteParams{JobID: job.ID, Completion: completion(t, KindInventory, "n1"), Actor: runner})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.State != StateCompleted || done.Verdict != VerdictPass {
		t.Fatalf("job = %+v, want completed pass", done)
	}
	got := f.entries(&runID)
	if len(got) != 2 || got[1].category != CategoryCompleted {
		t.Fatalf("entries = %+v, want requested then completed", got)
	}
	e := got[1]
	wantRequester := map[string]any{"seat": "operator", "actor_kind": "user", "subject": "github:captain"}
	if !reflect.DeepEqual(e.payload["requester"], wantRequester) {
		t.Errorf("payload.requester = %v, want the job row's %v (never the completing identity)", e.payload["requester"], wantRequester)
	}
	if !reflect.DeepEqual(e.payload["charged_to"], map[string]any{"seat": "operator", "subject": "github:captain"}) {
		t.Errorf("payload.charged_to = %v", e.payload["charged_to"])
	}
	if e.payload["provenance"] != ProvenanceBearer || e.payload["verdict"] != "pass" {
		t.Errorf("payload provenance/verdict = %v/%v, want bearer/pass", e.payload["provenance"], e.payload["verdict"])
	}
	if e.payload["requested_sequence"] != float64(job.RequestedSequence) {
		t.Errorf("payload.requested_sequence = %v, want %d", e.payload["requested_sequence"], job.RequestedSequence)
	}
	if e.actorKind != "agent" || e.actorSubject != "runner:someone-else" {
		t.Errorf("entry actor = %s/%s, want the completing identity", e.actorKind, e.actorSubject)
	}
	if !reflect.DeepEqual(e.payload["evidence"], asJSON(t, mustJSON(t, validEvidence(KindInventory)))) {
		t.Errorf("payload.evidence = %v", e.payload["evidence"])
	}
	if err := ValidateResult(done.Result); err != nil {
		t.Errorf("stored result is off-contract: %v", err)
	}
	var res Result
	if err := json.Unmarshal(done.Result, &res); err != nil || res.Requester != operator || res.JobID != job.ID.String() {
		t.Errorf("stored result = %+v (%v), want the row requester", res, err)
	}
}

func TestAdmitJob_RefusesDraftAndCompleted(t *testing.T) {
	f := newStoreFixture(t)
	draft, err := f.store.CreateCompleted(context.Background(), CreateCompletedParams{Request: parse(t, KindDraft, ""), Requester: operator, Text: "x"})
	if err != nil {
		t.Fatalf("CreateCompleted: %v", err)
	}
	// A draft row seeded back to queued by construction isolates the draft
	// refusal from the completed refusal.
	f.exec(`UPDATE specialist_jobs SET state = 'queued', verdict = NULL, result = NULL, completed_at = NULL WHERE id = $1`, draft.ID)
	if _, err := f.admit(nil, draft.ID, "n1"); !errors.Is(err, ErrNotAdmissible) {
		t.Fatalf("AdmitJob(draft) = %v, want ErrNotAdmissible", err)
	}
	if s := f.jobState(draft.ID); s != StateQueued {
		t.Fatalf("draft state after refusal = %s, want queued (unadmitted)", s)
	}
	done := f.create(nil, KindInventory, "")
	f.mustAdmit(nil, done.ID, "n1")
	if _, err := f.store.Complete(context.Background(), CompleteParams{JobID: done.ID, Completion: completion(t, KindInventory, "n1")}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := f.admit(nil, done.ID, "n1"); !errors.Is(err, ErrNotAdmissible) {
		t.Fatalf("AdmitJob(completed) = %v, want ErrNotAdmissible", err)
	}
	if _, err := f.admit(nil, uuid.New(), "n1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AdmitJob(unknown) = %v, want ErrNotFound", err)
	}
}

func TestAdmitJob_QueuesBehindVerifyStageHolder(t *testing.T) {
	f := newStoreFixture(t)
	if a := f.admitStage(f.verifyStage()); !a.Admitted {
		t.Fatalf("verify stage not admitted: %+v", a)
	}
	job := f.create(nil, KindInventory, "")
	q := f.mustQueue(nil, job.ID, "n1")
	if q.Contended || q.Position != 1 {
		t.Fatalf("queued = %+v, want uncontended position 1", q)
	}
	if s := f.jobState(job.ID); s != StateQueued {
		t.Fatalf("job state = %s, want queued behind the verify stage holder", s)
	}
	if q.Job.AdmissionGroup != "local-verify:h1" || q.Job.Host != "h1" {
		t.Fatalf("queued job group/host = %q/%q, want local-verify:h1/h1", q.Job.AdmissionGroup, q.Job.Host)
	}
}

func TestAdmitJob_ImplementHolderDoesNotBlock(t *testing.T) {
	f := newStoreFixture(t)
	r := f.verifyStage()
	r.GroupKey = concurrency.DefaultGroupKey("h1")
	if a := f.admitStage(r); !a.Admitted {
		t.Fatalf("implement stage not admitted: %+v", a)
	}
	job := f.create(nil, KindInventory, "")
	f.mustAdmit(nil, job.ID, "n1")
}

func TestAdmitJob_YieldsToFreshStageWaiter(t *testing.T) {
	f := newStoreFixture(t)
	holder, waiter := f.verifyStage(), f.verifyStage()
	if a := f.admitStage(holder); !a.Admitted {
		t.Fatalf("holder not admitted")
	}
	if a := f.admitStage(waiter); a.Admitted {
		t.Fatalf("waiter admitted, want queued")
	}
	// Release the holder: its stage settles, so only the fresh waiter is in
	// the group's path.
	if _, err := f.repo.TransitionStage(context.Background(), holder.StageID, run.StageStateRunning, nil); err != nil {
		t.Fatalf("holder running: %v", err)
	}
	if _, err := f.repo.TransitionStage(context.Background(), holder.StageID, run.StageStateSucceeded, nil); err != nil {
		t.Fatalf("holder succeeded: %v", err)
	}
	job := f.create(nil, KindInventory, "")
	q := f.mustQueue(nil, job.ID, "n1")
	if q.Position != 2 {
		t.Fatalf("job position = %d, want 2 (behind the fresh stage waiter)", q.Position)
	}
	if s := f.jobState(job.ID); s != StateQueued {
		t.Fatalf("job state = %s, want queued", s)
	}
}

func TestAdmitJob_FIFOAmongJobs(t *testing.T) {
	f := newStoreFixture(t)
	holder := f.verifyStage()
	f.admitStage(holder)
	older, newer := f.create(nil, KindInventory, ""), f.create(nil, KindInventory, "")
	f.mustQueue(nil, older.ID, "a")
	if q := f.mustQueue(nil, newer.ID, "b"); q.Position != 2 {
		t.Fatalf("newer position = %d, want 2", q.Position)
	}
	f.exec(`UPDATE stages SET state = 'succeeded' WHERE id = $1`, holder.StageID)
	if q := f.mustQueue(nil, newer.ID, "b"); q.Position != 2 {
		t.Fatalf("newer position after release = %d, want 2 (older job still ahead)", q.Position)
	}
	f.mustAdmit(nil, older.ID, "a")
	if q := f.mustQueue(nil, newer.ID, "b"); q.Position != 1 {
		t.Fatalf("newer position behind the running older job = %d, want 1", q.Position)
	}
}

func TestAdmitJob_ContendedQueuesWithoutDeciding(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if hit, err := concurrency.TryGroupLockTx(ctx, tx, nil, concurrency.VerifyGroupKey("h1")); err != nil || !hit {
		t.Fatalf("hold group lock: %v %v", hit, err)
	}
	job := f.create(nil, KindInventory, "")
	q := f.mustQueue(nil, job.ID, "n1")
	if !q.Contended {
		t.Fatalf("admission = %+v, want contended", q)
	}
}

func TestAdmitJob_HeartbeatSameNonceIdempotent_DifferentNonceRefused(t *testing.T) {
	f := newStoreFixture(t)
	job := f.create(nil, KindInventory, "")
	first := f.mustAdmit(nil, job.ID, "n1")
	f.exec(`UPDATE specialist_jobs SET last_seen_at = clock_timestamp() - interval '30 seconds' WHERE id = $1`, job.ID)
	beat := f.mustAdmit(nil, job.ID, "n1")
	if beat.ExecutionNonce != "n1" || beat.TakenOver || !beat.Job.LastSeenAt.After(first.Job.LastSeenAt.Add(-31e9)) {
		t.Fatalf("heartbeat = %+v, want admitted under n1 with a refreshed last_seen_at", beat)
	}
	if !beat.Job.StartedAt.Equal(*first.Job.StartedAt) {
		t.Fatalf("heartbeat moved started_at %v -> %v", first.Job.StartedAt, beat.Job.StartedAt)
	}
	if _, err := f.admit(nil, job.ID, "n2"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("AdmitJob(other nonce, fresh) = %v, want ErrAlreadyRunning", err)
	}
}

// TestAdmitJob_StaleRunningTakenOverUnderNewNonce is approval condition C1.
func TestAdmitJob_StaleRunningTakenOverUnderNewNonce(t *testing.T) {
	f := newStoreFixture(t)
	runID := f.newRun(nil)
	job := f.create(nil, KindInventory, runID.String())
	f.mustAdmit(nil, job.ID, "n1")
	f.seedStale(job.ID)
	took := f.mustAdmit(nil, job.ID, "n2")
	if !took.TakenOver || took.ExecutionNonce != "n2" || took.Job.ExecutionNonce != "n2" {
		t.Fatalf("takeover = %+v, want taken over under n2", took)
	}
	got := f.entries(&runID)
	if len(got) != 2 || got[1].category != CategoryTakenOver || got[1].payload["job_id"] != job.ID.String() {
		t.Fatalf("entries = %+v, want requested then %s", got, CategoryTakenOver)
	}
	if _, ok := got[1].payload["execution_nonce"]; ok {
		t.Errorf("takeover payload discloses a nonce: %v", got[1].payload)
	}
	if _, err := f.store.Complete(context.Background(), CompleteParams{JobID: job.ID, Completion: completion(t, KindInventory, "n1")}); !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("Complete under the superseded nonce = %v, want ErrNonceMismatch", err)
	}
	if _, err := f.store.Complete(context.Background(), CompleteParams{JobID: job.ID, Completion: completion(t, KindInventory, "n2")}); err != nil {
		t.Fatalf("Complete under the new nonce: %v", err)
	}
}

// A stale running job whose takeover is refused by occupancy is left
// untouched: refreshing it would make it count as a live holder again.
func TestAdmitJob_StaleTakeoverQueuedLeavesRowUntouched(t *testing.T) {
	f := newStoreFixture(t)
	job := f.create(nil, KindInventory, "")
	f.mustAdmit(nil, job.ID, "n1")
	f.seedStale(job.ID)
	if a := f.admitStage(f.verifyStage()); !a.Admitted {
		t.Fatalf("verify stage not admitted beside a stale job: %+v", a)
	}
	q := f.mustQueue(nil, job.ID, "n2")
	if q.TakenOver {
		t.Fatalf("queued takeover reported TakenOver")
	}
	var nonce string
	var stale bool
	if err := f.pool.QueryRow(context.Background(), `SELECT execution_nonce, last_seen_at <= clock_timestamp() - make_interval(secs => $2)
FROM specialist_jobs WHERE id = $1`, job.ID, concurrency.JobHeartbeatTTL.Seconds()).Scan(&nonce, &stale); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if nonce != "n1" || !stale {
		t.Fatalf("row after refused takeover: nonce %q stale %v, want n1 and still stale", nonce, stale)
	}
}

func TestStore_GetForeignAccountNotFound(t *testing.T) {
	f := newStoreFixture(t)
	mine, theirs := f.account(), f.account()
	job := f.create(&theirs, KindInventory, "")
	if _, err := f.store.Get(context.Background(), &mine, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(foreign) = %v, want ErrNotFound", err)
	}
	if _, err := f.store.Get(context.Background(), nil, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(untenanted) = %v, want ErrNotFound", err)
	}
	got, err := f.store.Get(context.Background(), &theirs, job.ID)
	if err != nil || got.ID != job.ID {
		t.Fatalf("Get(own) = %+v, %v", got, err)
	}
	if _, err := f.store.Complete(context.Background(), CompleteParams{AccountID: &mine, JobID: job.ID, Completion: completion(t, KindInventory, "n")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Complete(foreign) = %v, want ErrNotFound", err)
	}
}
