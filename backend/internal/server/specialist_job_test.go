package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
)

// The specialist-job HTTP suite (E85.9 / #4300): handler -> specialistjob
// store -> specialist_jobs + audit chain over a real pgtest database, with the
// runner's completions taken from the shared golden
// testdata/wire/specialist_job.json.

const (
	sjSHA1  = "1111111111111111111111111111111111111111"
	sjSHA2  = "2222222222222222222222222222222222222222"
	sjHost  = "sj-test-host"
	sjOther = "0b0e3c1e-5f4a-4d55-9a3c-6f1f2a9d0c02"
)

type sjPG struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *Server
}

func newSJPG(t *testing.T) *sjPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	return &sjPG{t: t, pool: pool, srv: New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        run.NewPostgresRepository(pool),
		AuditRepo:      audit.NewPostgresRepository(pool),
		CampaignRepo:   campaign.NewPostgresRepository(pool),
		SpecialistJobs: specialistjob.NewStore(pool),
	})}
}

func (f *sjPG) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedRun seeds a running run with one running implement stage, in acct when
// non-nil.
func (f *sjPG) seedRun(acct *uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
		VALUES ($1, 'kuhlman-labs/fishhawk', 'feature_change', 'sha', 'cli', 'running', 'local', $2)`, id, acct)
	f.exec(`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 1, 'implement', 'agent', 'claude-code', 'running')`, uuid.New(), id)
	return id
}

func (f *sjPG) account() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "sj-http-"+id.String())
	return id
}

// runSnapshot is the run state plus every stage state, for the
// result-touches-no-run-row assertions.
func (f *sjPG) runSnapshot(runID uuid.UUID) []string {
	f.t.Helper()
	var out []string
	var state string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		f.t.Fatalf("read run: %v", err)
	}
	out = append(out, "run="+state)
	rows, err := f.pool.Query(context.Background(), `SELECT id::text || '=' || state FROM stages WHERE run_id = $1 ORDER BY sequence`, runID)
	if err != nil {
		f.t.Fatalf("read stages: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatalf("scan stage: %v", err)
		}
		out = append(out, s)
	}
	return out
}

type sjEntry struct {
	category, actorKind, actorSubject string
	payload                           map[string]any
}

// runEntries reads the run's chain entries of the specialist-job categories.
func (f *sjPG) runEntries(runID uuid.UUID) []sjEntry {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT category, COALESCE(actor_kind, ''), COALESCE(actor_subject, ''), payload
		FROM audit_entries WHERE run_id = $1 AND category LIKE 'specialist_job_%' ORDER BY sequence`, runID)
	if err != nil {
		f.t.Fatalf("read entries: %v", err)
	}
	defer rows.Close()
	var out []sjEntry
	for rows.Next() {
		var e sjEntry
		var raw []byte
		if err := rows.Scan(&e.category, &e.actorKind, &e.actorSubject, &raw); err != nil {
			f.t.Fatalf("scan entry: %v", err)
		}
		if err := json.Unmarshal(raw, &e.payload); err != nil {
			f.t.Fatalf("decode payload: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func (f *sjPG) jobCount() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM specialist_jobs`).Scan(&n); err != nil {
		f.t.Fatalf("count jobs: %v", err)
	}
	return n
}

func sjCall(t *testing.T, h http.HandlerFunc, method, target, jobID, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if jobID != "" {
		req.SetPathValue("job_id", jobID)
	}
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func sjIdentity(id Identity) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
	}
}

// sjOperator is a human operator token holding both specialist-job scopes.
var sjOperator = operator("write:stages", "read:runs")

func sjInputs(k specialistjob.Kind) map[string]any {
	ref := func(id, head, base string) map[string]any {
		return map[string]any{"run_id": id, "head_sha": head, "base_sha": base}
	}
	switch k {
	case specialistjob.KindCounterfactualBattery:
		return map[string]any{"mutations": []any{map[string]any{
			"control_path": "backend/internal/x/x.go",
			"mutation":     map[string]any{"find": "return err", "replace": "return nil"},
			"tests":        map[string]any{"run": "^TestX$", "package": "./backend/internal/x"},
			"expect":       "red",
		}}}
	case specialistjob.KindPlanCrosscheck:
		return map[string]any{"focus_files": []any{map[string]any{"path": "backend/x.go", "expect": "present"}}}
	case specialistjob.KindOverlapScan:
		return map[string]any{"runs": []any{ref(sjOther, sjSHA1, sjSHA2), ref(uuid.NewString(), sjSHA2, sjSHA1)}}
	case specialistjob.KindFaninBuild:
		return map[string]any{"slices": []any{map[string]any{"name": "slice-1", "head_sha": sjSHA1}}, "packages": []any{"./backend/internal/x/..."}}
	case specialistjob.KindInventory:
		return map[string]any{"command": "git_log", "args": map[string]any{"paths": []any{"docs"}, "max_count": 20}}
	case specialistjob.KindDraft:
		return map[string]any{"document_kind": "adr", "title": "Specialist jobs ADR", "brief": "Why we need them.",
			"outline": []any{map[string]any{"heading": "Context", "body": "the context"}}}
	}
	panic("unknown kind " + string(k))
}

func sjRequestBody(t *testing.T, k specialistjob.Kind, subject map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"schema_version": specialistjob.SchemaVersion, "kind": string(k), "subject": subject, "inputs": sjInputs(k)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(b)
}

func decodeSJ[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, status, w.Body.String())
	}
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body = %s", err, w.Body.String())
	}
	return out
}

func (f *sjPG) request(k specialistjob.Kind, subject map[string]any, decorate func(*http.Request) *http.Request) specialistJobResponse {
	f.t.Helper()
	w := sjCall(f.t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", sjRequestBody(f.t, k, subject), decorate)
	return decodeSJ[specialistJobResponse](f.t, w, http.StatusCreated)
}

func (f *sjPG) admit(jobID, nonce string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	body := `{"host":"` + sjHost + `","nonce":"` + nonce + `"}`
	return sjCall(f.t, f.srv.handleAdmitSpecialistJob, http.MethodPost, "/v0/specialist-jobs/"+jobID+"/admission", jobID, body, decorate)
}

func (f *sjPG) result(jobID, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return sjCall(f.t, f.srv.handleSpecialistJobResult, http.MethodPost, "/v0/specialist-jobs/"+jobID+"/result", jobID, body, sjOperator)
}

func (f *sjPG) get(jobID string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	return sjCall(f.t, f.srv.handleGetSpecialistJob, http.MethodGet, "/v0/specialist-jobs/"+jobID, jobID, "", decorate)
}

// sjGolden reads the shared golden's completions and admission nonce.
func sjGolden(t *testing.T) (map[string]json.RawMessage, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "specialist_job.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var doc struct {
		Admission struct {
			ExecutionNonce string `json:"execution_nonce"`
		} `json:"admission"`
		Completions map[string]json.RawMessage `json:"completions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return doc.Completions, doc.Admission.ExecutionNonce
}

func semanticJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// TestSpecialistJob_EndToEnd_PerKind is the cross-boundary test: for each
// runner-executed kind, request -> admission -> result (the golden
// completion) -> GET, asserting the typed verdict and evidence, the
// server-derived requester, both audit rows on the run chain, and that the
// run and its stages are untouched.
func TestSpecialistJob_EndToEnd_PerKind(t *testing.T) {
	completions, nonce := sjGolden(t)
	cases := []struct {
		kind   specialistjob.Kind
		golden string
	}{
		{specialistjob.KindCounterfactualBattery, "counterfactual_battery"},
		{specialistjob.KindFaninBuild, "fanin_build"},
		{specialistjob.KindOverlapScan, "overlap_scan"},
		{specialistjob.KindPlanCrosscheck, "plan_crosscheck"},
		{specialistjob.KindInventory, "inventory"},
		{specialistjob.KindFaninBuild, "inconclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			f := newSJPG(t)
			runID := f.seedRun(nil)
			before := f.runSnapshot(runID)
			job := f.request(tc.kind, map[string]any{"run_id": runID.String(), "head_sha": sjSHA1}, sjOperator)
			if job.State != "queued" {
				t.Fatalf("state = %q, want queued", job.State)
			}
			adm := decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, nonce, sjOperator), http.StatusOK)
			if !adm.Admitted || adm.ExecutionNonce != nonce || adm.Job.AdmissionGroup != concurrency.VerifyGroupKey(sjHost) {
				t.Fatalf("admission = %+v", adm)
			}
			// C1: the read surface never renders the execution nonce.
			if w := f.get(job.JobID, sjOperator); strings.Contains(w.Body.String(), "execution_nonce") || strings.Contains(w.Body.String(), nonce) {
				t.Fatalf("GET leaked the execution nonce: %s", w.Body.String())
			}
			decodeSJ[specialistJobResponse](t, f.result(job.JobID, string(completions[tc.golden])), http.StatusOK)

			got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK)
			var want specialistjob.Completion
			if err := json.Unmarshal(completions[tc.golden], &want); err != nil {
				t.Fatalf("decode golden completion: %v", err)
			}
			if got.State != "completed" || got.Verdict != string(want.Verdict) {
				t.Fatalf("state/verdict = %s/%s, want completed/%s", got.State, got.Verdict, want.Verdict)
			}
			wantReq := specialistjob.Requester{Seat: specialistjob.SeatOperator, ActorKind: "user", Subject: "github:operator"}
			if got.Requester != wantReq {
				t.Fatalf("requester = %+v, want %+v", got.Requester, wantReq)
			}
			var res specialistjob.Result
			if err := json.Unmarshal(got.Result, &res); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if len(want.Evidence) > 0 && !reflect.DeepEqual(semanticJSON(t, res.Evidence), semanticJSON(t, want.Evidence)) {
				t.Fatalf("result evidence = %s, want %s", res.Evidence, want.Evidence)
			}
			if res.Requester != wantReq || res.ChargedTo.Seat != specialistjob.SeatOperator {
				t.Fatalf("result requester/charge = %+v / %+v", res.Requester, res.ChargedTo)
			}

			entries := f.runEntries(runID)
			if len(entries) != 2 || entries[0].category != specialistjob.CategoryRequested || entries[1].category != specialistjob.CategoryCompleted {
				t.Fatalf("run chain entries = %+v, want requested then completed", entries)
			}
			if entries[0].actorKind != "user" || entries[0].actorSubject != "github:operator" {
				t.Fatalf("requested actor = %s/%s", entries[0].actorKind, entries[0].actorSubject)
			}
			done := entries[1].payload
			if done["verdict"] != string(want.Verdict) || done["provenance"] != specialistjob.ProvenanceBearer {
				t.Fatalf("completed payload = %+v", done)
			}
			req, _ := done["requester"].(map[string]any)
			charged, _ := done["charged_to"].(map[string]any)
			if req["subject"] != "github:operator" || charged["seat"] != "operator" || charged["subject"] != "github:operator" {
				t.Fatalf("completed requester/charged_to = %+v / %+v", req, charged)
			}
			if after := f.runSnapshot(runID); !reflect.DeepEqual(before, after) {
				t.Fatalf("run/stage state changed: %v -> %v", before, after)
			}
		})
	}
}

func TestSpecialistJob_DraftCompletesInProcess(t *testing.T) {
	f := newSJPG(t)
	runID := f.seedRun(nil)
	before := f.runSnapshot(runID)
	job := f.request(specialistjob.KindDraft, map[string]any{"run_id": runID.String()}, sjOperator)
	if job.State != "completed" || job.Verdict != "pass" {
		t.Fatalf("draft = %s/%s, want completed/pass", job.State, job.Verdict)
	}
	var res specialistjob.Result
	if err := json.Unmarshal(job.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var ev specialistjob.DraftEvidence
	if err := json.Unmarshal(res.Evidence, &ev); err != nil {
		t.Fatalf("decode draft evidence: %v", err)
	}
	for _, want := range []string{"# Specialist jobs ADR", "## Context", "## Decision"} {
		if !strings.Contains(ev.Text, want) {
			t.Errorf("draft text missing %q:\n%s", want, ev.Text)
		}
	}
	if entries := f.runEntries(runID); len(entries) != 2 {
		t.Fatalf("draft wrote %d specialist-job audit rows, want exactly 2", len(entries))
	}
	if after := f.runSnapshot(runID); !reflect.DeepEqual(before, after) {
		t.Fatalf("run/stage state changed: %v -> %v", before, after)
	}
	w := f.admit(job.JobID, "n-1", sjOperator)
	requireCrewRefusal(t, w, http.StatusConflict, "specialist_job_not_admissible")
}

// C4: the requester's actor kind and subject come from the authenticated
// identity; a delegated operator-agent token is actor_kind=agent.
func TestSpecialistJobRequest_RequesterFromIdentity(t *testing.T) {
	cases := []struct {
		name     string
		id       Identity
		wantKind string
	}{
		{"human token", Identity{Subject: "github:octocat", TokenID: "t1", Scopes: []string{"write:stages"}}, "user"},
		{"operator-agent token", Identity{Subject: operatorAgentSubject, TokenID: "t2", Scopes: []string{"write:stages"}}, "agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSJPG(t)
			runID := f.seedRun(nil)
			job := f.request(specialistjob.KindInventory, map[string]any{"run_id": runID.String(), "head_sha": sjSHA1}, sjIdentity(tc.id))
			want := specialistjob.Requester{Seat: specialistjob.SeatOperator, ActorKind: tc.wantKind, Subject: tc.id.Subject}
			if job.Requester != want {
				t.Fatalf("requester = %+v, want %+v", job.Requester, want)
			}
			entries := f.runEntries(runID)
			if len(entries) != 1 || entries[0].actorKind != tc.wantKind || entries[0].actorSubject != tc.id.Subject {
				t.Fatalf("requested entry = %+v, want actor %s/%s", entries, tc.wantKind, tc.id.Subject)
			}
		})
	}
}

func TestSpecialistJobRequest_BodyTooLarge(t *testing.T) {
	f := newSJPG(t)
	// A VALID document padded with insignificant whitespace past the cap, so
	// only the byte cap can refuse it.
	body := sjRequestBody(t, specialistjob.KindDraft, map[string]any{"repo": "kuhlman-labs/fishhawk"})
	body = body[:len(body)-1] + strings.Repeat(" ", specialistJobRequestMaxBytes) + "}"
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, sjOperator)
	requireCrewRefusal(t, w, http.StatusRequestEntityTooLarge, "request_too_large")
	if n := f.jobCount(); n != 0 {
		t.Fatalf("an oversized request created %d jobs", n)
	}
}

// The fixture identity is run-bound yet CARRIES write:stages, so the scope
// gate passes and the seat check is the only refusal.
func TestSpecialistJobRequest_RunBoundTokenRefused(t *testing.T) {
	f := newSJPG(t)
	runID := f.seedRun(nil)
	body := sjRequestBody(t, specialistjob.KindDraft, map[string]any{"run_id": runID.String()})
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, runBound(runID, "write:stages", "read:runs"))
	requireCrewRefusal(t, w, http.StatusForbidden, "specialist_job_requester_not_a_seat")
	if n := f.jobCount(); n != 0 {
		t.Fatalf("a run-bound request created %d jobs", n)
	}
}

func TestSpecialistJobRequest_MissingScope403(t *testing.T) {
	f := newSJPG(t)
	body := sjRequestBody(t, specialistjob.KindDraft, map[string]any{"repo": "kuhlman-labs/fishhawk"})
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, operator("read:runs"))
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
}

func TestSpecialistJobRequest_UnknownMember400(t *testing.T) {
	f := newSJPG(t)
	body := `{"schema_version":"specialist-job-v1","kind":"draft","subject":{"repo":"a/b"},"inputs":{"document_kind":"adr","title":"t"},` +
		`"requester":{"seat":"first_officer","actor_kind":"user","subject":"forged"}}`
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, sjOperator)
	requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
	if !strings.Contains(w.Body.String(), "requester") {
		t.Fatalf("refusal does not name the forged member: %s", w.Body.String())
	}
	if n := f.jobCount(); n != 0 {
		t.Fatalf("a refused request created %d jobs", n)
	}
}

func TestSpecialistJobRequest_ForeignAccountRun404(t *testing.T) {
	f := newSJPG(t)
	acct := f.account()
	runID := f.seedRun(&acct)
	body := sjRequestBody(t, specialistjob.KindInventory, map[string]any{"run_id": runID.String(), "head_sha": sjSHA1})
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, sjOperator)
	requireCrewRefusal(t, w, http.StatusNotFound, "run_not_found")
}

func TestSpecialistJobRequest_UnknownCampaign404(t *testing.T) {
	f := newSJPG(t)
	body := sjRequestBody(t, specialistjob.KindInventory, map[string]any{"campaign_id": uuid.NewString(), "head_sha": sjSHA1})
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, sjOperator)
	requireCrewRefusal(t, w, http.StatusNotFound, "campaign_not_found")
}

// sjCampaignRepo answers GetCampaignAccountID from a fixed map.
type sjCampaignRepo struct {
	campaign.BaseFake
	accounts map[uuid.UUID]string
	err      error
}

func (r sjCampaignRepo) GetCampaignAccountID(_ context.Context, id uuid.UUID) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	acct, ok := r.accounts[id]
	if !ok {
		return "", campaign.ErrNotFound
	}
	return acct, nil
}

func TestSpecialistJobRequest_CampaignAccountScoping(t *testing.T) {
	f := newSJPG(t)
	own, foreign := uuid.New(), uuid.New()
	f.srv.cfg.CampaignRepo = sjCampaignRepo{accounts: map[uuid.UUID]string{own: "", foreign: uuid.NewString()}}
	call := func(id uuid.UUID) *httptest.ResponseRecorder {
		body := sjRequestBody(t, specialistjob.KindInventory, map[string]any{"campaign_id": id.String(), "head_sha": sjSHA1})
		return sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, sjOperator)
	}
	requireCrewRefusal(t, call(foreign), http.StatusNotFound, "campaign_not_found")
	if w := call(own); w.Code != http.StatusCreated {
		t.Fatalf("own campaign: status %d; body %s", w.Code, w.Body.String())
	}
	f.srv.cfg.CampaignRepo = sjCampaignRepo{err: errors.New("db down")}
	if w := call(own); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("campaign probe error: status %d, want 503 (fail closed)", w.Code)
	}
	f.srv.cfg.CampaignRepo = nil
	requireCrewRefusal(t, call(own), http.StatusNotFound, "campaign_not_found")
}

func TestSpecialistJobGet_ForeignAccount404(t *testing.T) {
	f := newSJPG(t)
	acct := f.account()
	tenant := sjIdentity(Identity{Subject: "github:tenant", TokenID: "t", Scopes: []string{"write:stages", "read:runs"}, AccountID: acct.String()})
	job := f.request(specialistjob.KindInventory, map[string]any{"repo": "kuhlman-labs/fishhawk", "head_sha": sjSHA1}, tenant)
	if w := f.get(job.JobID, tenant); w.Code != http.StatusOK {
		t.Fatalf("own-account GET: %d %s", w.Code, w.Body.String())
	}
	requireCrewRefusal(t, f.get(job.JobID, sjOperator), http.StatusNotFound, "specialist_job_not_found")
	requireCrewRefusal(t, f.admit(job.JobID, "n", sjOperator), http.StatusNotFound, "specialist_job_not_found")
	requireCrewRefusal(t, f.result(job.JobID, `{}`), http.StatusNotFound, "specialist_job_not_found")
}

func TestSpecialistJobGet_ScopeAndBadID(t *testing.T) {
	f := newSJPG(t)
	requireCrewRefusal(t, f.get("not-a-uuid", sjOperator), http.StatusBadRequest, "validation_failed")
	requireCrewRefusal(t, f.get(uuid.NewString(), operator("write:stages")), http.StatusForbidden, "insufficient_scope")
	requireCrewRefusal(t, f.get(uuid.NewString(), nil), http.StatusUnauthorized, "authentication_required")
}

func (f *sjPG) queuedJob(k specialistjob.Kind) (specialistJobResponse, uuid.UUID) {
	f.t.Helper()
	runID := f.seedRun(nil)
	return f.request(k, map[string]any{"run_id": runID.String(), "head_sha": sjSHA1}, sjOperator), runID
}

func TestSpecialistJobAdmission_AlreadyRunning409(t *testing.T) {
	f := newSJPG(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, "nonce-a", sjOperator), http.StatusOK)
	requireCrewRefusal(t, f.admit(job.JobID, "nonce-b", sjOperator), http.StatusConflict, "specialist_job_already_running")
	// The same nonce is the idempotent heartbeat.
	if hb := decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, "nonce-a", sjOperator), http.StatusOK); !hb.Admitted {
		t.Fatalf("same-nonce heartbeat not admitted: %+v", hb)
	}
}

// C1 at the HTTP surface: a running job whose heartbeat is stale past
// JobHeartbeatTTL is taken over under a NEW nonce, the takeover is on the
// chain, and the old nonce can no longer ship a result.
func TestSpecialistJobAdmission_StaleRunningJobTakenOver(t *testing.T) {
	f := newSJPG(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, "nonce-old", sjOperator), http.StatusOK)
	f.exec(`UPDATE specialist_jobs SET last_seen_at = clock_timestamp() - make_interval(secs => $2) WHERE id = $1`,
		job.JobID, concurrency.JobHeartbeatTTL.Seconds()+30)
	stale := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK)
	if !stale.Stale {
		t.Fatalf("a running job unseen past JobHeartbeatTTL must read stale: %+v", stale)
	}
	adm := decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, "nonce-new", sjOperator), http.StatusOK)
	if !adm.Admitted || !adm.TakenOver || adm.ExecutionNonce != "nonce-new" {
		t.Fatalf("takeover admission = %+v", adm)
	}
	var taken int
	for _, e := range f.runEntries(runID) {
		if e.category == specialistjob.CategoryTakenOver {
			taken++
		}
	}
	if taken != 1 {
		t.Fatalf("takeover entries = %d, want 1", taken)
	}
	completions, _ := sjGolden(t)
	old := strings.Replace(string(completions["inventory"]), `"golden-execution-nonce"`, `"nonce-old"`, 1)
	requireCrewRefusal(t, f.result(job.JobID, old), http.StatusConflict, "specialist_job_nonce_mismatch")
}

func TestSpecialistJobAdmission_BadBody400(t *testing.T) {
	f := newSJPG(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	for name, body := range map[string]string{
		"unknown member": `{"host":"h","nonce":"n","state":"running"}`,
		"empty host":     `{"host":"","nonce":"n"}`,
		"empty nonce":    `{"host":"h","nonce":""}`,
		"not json":       `nope`,
	} {
		t.Run(name, func(t *testing.T) {
			w := sjCall(t, f.srv.handleAdmitSpecialistJob, http.MethodPost, "/", job.JobID, body, sjOperator)
			requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
		})
	}
	if got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK); got.State != "queued" || got.AdmissionGroup != "" {
		t.Fatalf("a refused admission changed the job: %+v", got)
	}
}

func TestSpecialistJobResult_NotRunning409(t *testing.T) {
	f := newSJPG(t)
	completions, _ := sjGolden(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	requireCrewRefusal(t, f.result(job.JobID, string(completions["inventory"])), http.StatusConflict, "specialist_job_not_running")
	if got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK); got.State != "queued" {
		t.Fatalf("state = %s, want queued", got.State)
	}
}

func TestSpecialistJobResult_NonceMismatch409(t *testing.T) {
	f := newSJPG(t)
	completions, _ := sjGolden(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, "another-nonce", sjOperator), http.StatusOK)
	requireCrewRefusal(t, f.result(job.JobID, string(completions["inventory"])), http.StatusConflict, "specialist_job_nonce_mismatch")
	if got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK); got.State != "running" {
		t.Fatalf("state = %s, want running", got.State)
	}
}

func TestSpecialistJobResult_KindMismatch400(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, _ := f.queuedJob(specialistjob.KindOverlapScan)
	decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, nonce, sjOperator), http.StatusOK)
	w := f.result(job.JobID, string(completions["counterfactual_battery"]))
	requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
	if !strings.Contains(w.Body.String(), "kind_mismatch") {
		t.Fatalf("refusal does not name the kind mismatch: %s", w.Body.String())
	}
	if got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK); got.State != "running" {
		t.Fatalf("state = %s, want running", got.State)
	}
}

func TestSpecialistJobResult_SecondResult409(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, nonce, sjOperator), http.StatusOK)
	decodeSJ[specialistJobResponse](t, f.result(job.JobID, string(completions["inventory"])), http.StatusOK)
	requireCrewRefusal(t, f.result(job.JobID, string(completions["inventory"])), http.StatusConflict, "specialist_job_not_running")
	if n := len(f.runEntries(runID)); n != 2 {
		t.Fatalf("entries = %d, want exactly requested + one completed", n)
	}
}

func TestSpecialistJobResult_BodyTooLarge413(t *testing.T) {
	f := newSJPG(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	body := `{"x":"` + strings.Repeat("a", specialistJobResultMaxBytes) + `"}`
	requireCrewRefusal(t, f.result(job.JobID, body), http.StatusRequestEntityTooLarge, "request_too_large")
}

// sjFailingStore fails every call, for the 500 arm.
type sjFailingStore struct{}

var errSJStore = errors.New("store down")

func (sjFailingStore) Create(context.Context, specialistjob.CreateParams) (*specialistjob.Job, error) {
	return nil, errSJStore
}
func (sjFailingStore) CreateCompleted(context.Context, specialistjob.CreateCompletedParams) (*specialistjob.Job, error) {
	return nil, errSJStore
}
func (sjFailingStore) Get(context.Context, *uuid.UUID, uuid.UUID) (*specialistjob.Job, error) {
	return nil, errSJStore
}
func (sjFailingStore) AdmitJob(context.Context, specialistjob.AdmitParams) (*specialistjob.Admission, error) {
	return nil, errSJStore
}
func (sjFailingStore) Complete(context.Context, specialistjob.CompleteParams) (*specialistjob.Job, error) {
	return nil, errSJStore
}
func (sjFailingStore) ListCompletedForRun(context.Context, *uuid.UUID, uuid.UUID) ([]*specialistjob.Job, error) {
	return nil, errSJStore
}

func TestSpecialistJob_StoreErrorIs500(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", SpecialistJobs: sjFailingStore{}})
	id := uuid.NewString()
	body := sjRequestBody(t, specialistjob.KindDraft, map[string]any{"repo": "a/b"})
	for name, w := range map[string]*httptest.ResponseRecorder{
		"request": sjCall(t, s.handleRequestSpecialistJob, http.MethodPost, "/", "", body, sjOperator),
		"get":     sjCall(t, s.handleGetSpecialistJob, http.MethodGet, "/", id, "", sjOperator),
		"admit":   sjCall(t, s.handleAdmitSpecialistJob, http.MethodPost, "/", id, `{"host":"h","nonce":"n"}`, sjOperator),
		"result":  sjCall(t, s.handleSpecialistJobResult, http.MethodPost, "/", id, `{}`, sjOperator),
	} {
		t.Run(name, func(t *testing.T) { requireCrewRefusal(t, w, http.StatusInternalServerError, "internal_error") })
	}
}

func TestSpecialistJob_Unconfigured503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	id := uuid.NewString()
	for name, w := range map[string]*httptest.ResponseRecorder{
		"request": sjCall(t, s.handleRequestSpecialistJob, http.MethodPost, "/", "", "{}", sjOperator),
		"get":     sjCall(t, s.handleGetSpecialistJob, http.MethodGet, "/", id, "", sjOperator),
		"admit":   sjCall(t, s.handleAdmitSpecialistJob, http.MethodPost, "/", id, "{}", sjOperator),
		"result":  sjCall(t, s.handleSpecialistJobResult, http.MethodPost, "/", id, "{}", sjOperator),
	} {
		t.Run(name, func(t *testing.T) {
			requireCrewRefusal(t, w, http.StatusServiceUnavailable, "specialist_jobs_unconfigured")
		})
	}
}

// TestSpecialistJob_GoldenCompletionsParse: every golden completion parses
// against its kind on the backend side (the runner round-trips the same
// bytes in upload_test.go).
func TestSpecialistJob_GoldenCompletionsParse(t *testing.T) {
	completions, _ := sjGolden(t)
	kinds := map[string]specialistjob.Kind{
		"counterfactual_battery": specialistjob.KindCounterfactualBattery, "fanin_build": specialistjob.KindFaninBuild,
		"overlap_scan": specialistjob.KindOverlapScan, "plan_crosscheck": specialistjob.KindPlanCrosscheck,
		"inventory": specialistjob.KindInventory, "inconclusive": specialistjob.KindFaninBuild,
	}
	if len(completions) != len(kinds) {
		t.Fatalf("golden carries %d completions, want %d", len(completions), len(kinds))
	}
	for name, k := range kinds {
		if _, err := specialistjob.ParseCompletion(completions[name], k); err != nil {
			t.Errorf("golden completion %s does not parse as %s: %v", name, k, err)
		}
	}
}

func TestSpecialistJob_RoutesRegistered(t *testing.T) {
	rec := &patternRecorder{}
	(&Server{}).registerRoutes(rec)
	for _, want := range []string{
		"POST /v0/specialist-jobs", "GET /v0/specialist-jobs/{job_id}",
		"POST /v0/specialist-jobs/{job_id}/admission", "POST /v0/specialist-jobs/{job_id}/result",
	} {
		if !rec.has(want) {
			t.Errorf("registerRoutes does not register %q", want)
		}
	}
}

type patternRecorder struct{ patterns []string }

func (p *patternRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	p.patterns = append(p.patterns, pattern)
}

func (p *patternRecorder) has(pattern string) bool {
	for _, got := range p.patterns {
		if got == pattern {
			return true
		}
	}
	return false
}

// TestOpenAPI_SpecialistJobRoutesDocumented: the four routes, their schemas
// and every error code are in the OpenAPI source of truth.
func TestOpenAPI_SpecialistJobRoutesDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"\n  /v0/specialist-jobs:\n", "\n  /v0/specialist-jobs/{job_id}:\n",
		"\n  /v0/specialist-jobs/{job_id}/admission:\n", "\n  /v0/specialist-jobs/{job_id}/result:\n",
		"operationId: requestSpecialistJob", "operationId: getSpecialistJob",
		"operationId: admitSpecialistJob", "operationId: completeSpecialistJob",
		"\n    SpecialistJob:\n", "\n    SpecialistJobAdmission:\n",
		"specialist_jobs_unconfigured", "specialist_job_requester_not_a_seat", "request_too_large",
		"specialist_job_not_found", "specialist_job_not_admissible", "specialist_job_already_running",
		"specialist_job_not_running", "specialist_job_nonce_mismatch", "campaign_not_found",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}

// A schema-valid draft whose render exceeds specialistjob.MaxDraftBytes (five
// 8000-byte outline bodies, inside the 64 KiB request cap) is refused 400 and
// files no job.
func TestSpecialistJobRequest_DraftRenderTooLarge400(t *testing.T) {
	f := newSJPG(t)
	var outline []any
	for i := 0; i < 5; i++ {
		outline = append(outline, map[string]any{"heading": "Section", "body": strings.Repeat("b", 8000)})
	}
	b, err := json.Marshal(map[string]any{"schema_version": specialistjob.SchemaVersion, "kind": "draft",
		"subject": map[string]any{"repo": "kuhlman-labs/fishhawk"},
		"inputs":  map[string]any{"document_kind": "issue", "title": "Big", "outline": outline}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", string(b), sjOperator)
	requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
	if !strings.Contains(w.Body.String(), "draft could not be rendered") {
		t.Fatalf("refusal is not the render bound: %s", w.Body.String())
	}
	if n := f.jobCount(); n != 0 {
		t.Fatalf("an over-bound draft created %d jobs", n)
	}
}

// --- E85.15 / #4363: runner-attested result provenance ---

// sjAttesterID is a runner credential: an explicit API token holding only the
// dedicated attest scope.
var sjAttesterID = Identity{Subject: "specialist-runner/sj-test-host", TokenID: "tok-runner", Scopes: []string{specialistjob.AttestScope}}

// sjBearerID is a plain write:stages bearer (no attest scope).
var sjBearerID = Identity{Subject: "github:bearer", TokenID: "tok-bearer", Scopes: []string{"write:stages", "read:runs"}}

// sjCookieID is a cookie session: no TokenID and no scope list.
var sjCookieID = Identity{Subject: "github:cookie-operator", SessionID: "sess-1"}

// sjSigned decorates a request with id and, when sig is non-empty, the
// result signature header.
func sjSigned(id Identity, sig string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		if sig != "" {
			r.Header.Set(specialistjob.ResultSignatureHeader, sig)
		}
		return sjIdentity(id)(r)
	}
}

func (f *sjPG) resultAs(jobID, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	return sjCall(f.t, f.srv.handleSpecialistJobResult, http.MethodPost, "/v0/specialist-jobs/"+jobID+"/result", jobID, body, decorate)
}

// attestedAdmission admits job as sjAttesterID under nonce and returns the
// delivered private key, failing unless a 64-byte key was delivered.
func (f *sjPG) attestedAdmission(jobID, nonce string) ed25519.PrivateKey {
	f.t.Helper()
	adm := decodeSJ[specialistJobAdmissionResponse](f.t, f.admit(jobID, nonce, sjIdentity(sjAttesterID)), http.StatusOK)
	if !adm.Admitted || adm.AttestationKey == "" || adm.AttestationExpiresAt == nil {
		f.t.Fatalf("attester admission delivered no key: %+v", adm)
	}
	raw, err := base64.StdEncoding.DecodeString(adm.AttestationKey)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		f.t.Fatalf("attestation_key = %q (decode err %v, %d bytes), want a base64 64-byte key", adm.AttestationKey, err, len(raw))
	}
	return ed25519.PrivateKey(raw)
}

func sjSign(t *testing.T, key ed25519.PrivateKey, jobID, body string) string {
	t.Helper()
	return hex.EncodeToString(ed25519.Sign(key, specialistjob.ResultMessage(uuid.MustParse(jobID), []byte(body))))
}

// requireAttestationRefusal asserts 401 specialist_job_attestation_invalid
// with the given details.reason.
func requireAttestationRefusal(t *testing.T, w *httptest.ResponseRecorder, reason string) {
	t.Helper()
	requireCrewRefusal(t, w, http.StatusUnauthorized, "specialist_job_attestation_invalid")
	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if got := body.Error.Details["reason"]; got != reason {
		t.Fatalf("details.reason = %v, want %q; body = %s", got, reason, w.Body.String())
	}
}

// requireStillRunning reads the job back and requires it running with no
// result and no provenance: a refused result wrote nothing.
func (f *sjPG) requireStillRunning(jobID string) {
	f.t.Helper()
	got := decodeSJ[specialistJobResponse](f.t, f.get(jobID, sjOperator), http.StatusOK)
	if got.State != "running" || len(got.Result) != 0 || got.Provenance != "" || got.BlockingEligible || got.CompletedSequence != nil {
		f.t.Fatalf("a refused result changed the job: %+v", got)
	}
}

func (f *sjPG) completedPayloads(runID uuid.UUID) []sjEntry {
	f.t.Helper()
	var out []sjEntry
	for _, e := range f.runEntries(runID) {
		if e.category == specialistjob.CategoryCompleted {
			out = append(out, e)
		}
	}
	return out
}

// TestSpecialistJobResult_RunnerAttestedEndToEnd is the cross-boundary test:
// HTTP admission under an attester identity -> the delivered key signs
// ResultMessage over the golden completion bytes -> the signed result under
// the same identity -> GET and the run chain's specialist_job_completed
// payload carry runner_attested + blocking_eligible, the key is consumed, and
// the run and its stages are untouched.
func TestSpecialistJobResult_RunnerAttestedEndToEnd(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	before := f.runSnapshot(runID)
	key := f.attestedAdmission(job.JobID, nonce)
	body := string(completions["inventory"])

	done := decodeSJ[specialistJobResponse](t, f.resultAs(job.JobID, body, sjSigned(sjAttesterID, sjSign(t, key, job.JobID, body))), http.StatusOK)
	if done.State != "completed" || done.Provenance != specialistjob.ProvenanceRunnerAttested || !done.BlockingEligible {
		t.Fatalf("result answer = %s/%q/%v, want completed/runner_attested/true", done.State, done.Provenance, done.BlockingEligible)
	}
	got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjIdentity(sjAttesterID)), http.StatusOK)
	if got.Provenance != specialistjob.ProvenanceRunnerAttested || !got.BlockingEligible {
		t.Fatalf("GET provenance/blocking_eligible = %q/%v", got.Provenance, got.BlockingEligible)
	}
	entries := f.completedPayloads(runID)
	if len(entries) != 1 {
		t.Fatalf("completed entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.payload["provenance"] != specialistjob.ProvenanceRunnerAttested || e.payload["blocking_eligible"] != true ||
		e.payload["attested_by"] != sjAttesterID.Subject {
		t.Fatalf("completed payload = %+v", e.payload)
	}
	if e.actorKind != string(audit.ActorSystem) || e.actorSubject != sjAttesterID.Subject {
		t.Fatalf("completed actor = %s/%s, want system/%s", e.actorKind, e.actorSubject, sjAttesterID.Subject)
	}
	var keyNull bool
	if err := f.pool.QueryRow(context.Background(), `SELECT attestation_public_key IS NULL FROM specialist_jobs WHERE id = $1`, job.JobID).Scan(&keyNull); err != nil {
		t.Fatalf("read key column: %v", err)
	}
	if !keyNull {
		t.Fatal("an attested completion left the attestation key in place, want it consumed")
	}
	if after := f.runSnapshot(runID); !reflect.DeepEqual(before, after) {
		t.Fatalf("run/stage state changed: %v -> %v", before, after)
	}
}

// A plain write:stages bearer's result records provenance bearer and is never
// blocking-eligible, on the result answer, the read and the audit payload.
func TestSpecialistJobResult_BearerProvenanceNeverBlockingEligible(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, nonce, sjIdentity(sjBearerID)), http.StatusOK)
	done := decodeSJ[specialistJobResponse](t, f.resultAs(job.JobID, string(completions["inventory"]), sjIdentity(sjBearerID)), http.StatusOK)
	got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjIdentity(sjBearerID)), http.StatusOK)
	for name, j := range map[string]specialistJobResponse{"result": done, "get": got} {
		if j.Provenance != specialistjob.ProvenanceBearer || j.BlockingEligible {
			t.Fatalf("%s provenance/blocking_eligible = %q/%v, want bearer/false", name, j.Provenance, j.BlockingEligible)
		}
	}
	entries := f.completedPayloads(runID)
	if len(entries) != 1 || entries[0].payload["provenance"] != specialistjob.ProvenanceBearer ||
		entries[0].payload["blocking_eligible"] != false {
		t.Fatalf("completed payload = %+v", entries)
	}
	if _, ok := entries[0].payload["attested_by"]; ok {
		t.Fatalf("a bearer completion carries attested_by: %+v", entries[0].payload)
	}
	if entries[0].actorKind != "user" || entries[0].actorSubject != sjBearerID.Subject {
		t.Fatalf("bearer completion actor = %s/%s, want user/%s", entries[0].actorKind, entries[0].actorSubject, sjBearerID.Subject)
	}
}

// A token holding write:stages but NOT the attest scope is admitted with no
// attestation key delivered and none stored.
func TestSpecialistJobAdmission_PlainBearerGetsNoAttestationKey(t *testing.T) {
	f := newSJPG(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	plain := Identity{Subject: "github:plain", TokenID: "tok-plain", Scopes: []string{"write:stages"}}
	w := f.admit(job.JobID, "nonce-plain", sjIdentity(plain))
	adm := decodeSJ[specialistJobAdmissionResponse](t, w, http.StatusOK)
	if !adm.Admitted {
		t.Fatalf("plain bearer not admitted: %+v", adm)
	}
	if strings.Contains(w.Body.String(), "attestation_key") || strings.Contains(w.Body.String(), "attestation_expires_at") {
		t.Fatalf("a plain write:stages bearer was delivered an attestation key: %s", w.Body.String())
	}
	var keyNull bool
	if err := f.pool.QueryRow(context.Background(), `SELECT attestation_public_key IS NULL FROM specialist_jobs WHERE id = $1`, job.JobID).Scan(&keyNull); err != nil {
		t.Fatalf("read key column: %v", err)
	}
	if !keyNull {
		t.Fatal("a plain bearer admission stored an attestation key")
	}
}

// Each row isolates one arm of attesterFor's guard.
func TestAttesterFor_Table(t *testing.T) {
	runSubject := "mcp:run:" + uuid.NewString()
	cases := []struct {
		name string
		id   Identity
		want string
	}{
		{"token with the attest scope", Identity{Subject: "specialist-runner/h", TokenID: "t", Scopes: []string{specialistjob.AttestScope}}, "specialist-runner/h"},
		{"cookie session carrying the attest scope", Identity{Subject: "github:cookie", SessionID: "s", Scopes: []string{specialistjob.AttestScope}}, ""},
		{"run-bound token carrying the attest scope", Identity{Subject: runSubject, TokenID: "t", Scopes: []string{specialistjob.AttestScope}}, ""},
		{"token without the attest scope", Identity{Subject: "github:op", TokenID: "t", Scopes: []string{"write:stages"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attesterFor(tc.id); got != tc.want {
				t.Fatalf("attesterFor = %q, want %q", got, tc.want)
			}
		})
	}
	if a := specialistJobActor(cases[0].id); a.Kind != audit.ActorSystem || a.Subject != "specialist-runner/h" {
		t.Fatalf("attester actor = %+v, want system", a)
	}
	if a := specialistJobActor(cases[3].id); a.Kind != audit.ActorUser {
		t.Fatalf("non-attester actor = %+v, want user", a)
	}
}

// A signature header from a caller that is not an attester is refused
// caller_not_attester and nothing is written. The fixture admits with the
// SAME plain bearer, so no key exists: without the handler branch the store
// would refuse no_attestation_key instead, and with the header ignored the
// result would land bearer.
func TestSpecialistJobResult_SignatureFromNonAttesterRefused(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	decodeSJ[specialistJobAdmissionResponse](t, f.admit(job.JobID, nonce, sjIdentity(sjBearerID)), http.StatusOK)
	sig := hex.EncodeToString(make([]byte, ed25519.SignatureSize))
	requireAttestationRefusal(t, f.resultAs(job.JobID, string(completions["inventory"]), sjSigned(sjBearerID, sig)), specialistjob.ReasonCallerNotAttester)
	f.requireStillRunning(job.JobID)
	if n := len(f.completedPayloads(runID)); n != 0 {
		t.Fatalf("a refused result appended %d completed entries", n)
	}
}

// A signature over body A posted with body B is refused signature_invalid.
func TestSpecialistJobResult_TamperedBodyRefused(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	key := f.attestedAdmission(job.JobID, nonce)
	signed := string(completions["inventory"])
	tampered := strings.Replace(signed, "1234", "9999", 1)
	if tampered == signed {
		t.Fatal("fixture: the tamper did not change the body")
	}
	requireAttestationRefusal(t, f.resultAs(job.JobID, tampered, sjSigned(sjAttesterID, sjSign(t, key, job.JobID, signed))), specialistjob.ReasonSignatureInvalid)
	f.requireStillRunning(job.JobID)
	if n := len(f.completedPayloads(runID)); n != 0 {
		t.Fatalf("a refused result appended %d completed entries", n)
	}
}

// The key's expiry is decided on the DB clock: a key seeded expired by SQL
// refuses attestation_expired (the store's reason reaches details.reason).
func TestSpecialistJobResult_ExpiredAttestationRefused(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	key := f.attestedAdmission(job.JobID, nonce)
	f.exec(`UPDATE specialist_jobs SET attestation_expires_at = clock_timestamp() - interval '1 minute' WHERE id = $1`, job.JobID)
	body := string(completions["inventory"])
	requireAttestationRefusal(t, f.resultAs(job.JobID, body, sjSigned(sjAttesterID, sjSign(t, key, job.JobID, body))), specialistjob.ReasonAttestationExpired)
	f.requireStillRunning(job.JobID)
}

// Replaying the identical signed POST after an attested completion is 409
// specialist_job_not_running and changes nothing.
func TestSpecialistJobResult_ReplayRefused(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	key := f.attestedAdmission(job.JobID, nonce)
	body := string(completions["inventory"])
	signed := sjSigned(sjAttesterID, sjSign(t, key, job.JobID, body))
	first := decodeSJ[specialistJobResponse](t, f.resultAs(job.JobID, body, signed), http.StatusOK)
	requireCrewRefusal(t, f.resultAs(job.JobID, body, signed), http.StatusConflict, "specialist_job_not_running")
	got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK)
	if got.Provenance != first.Provenance || got.CompletedSequence == nil || first.CompletedSequence == nil ||
		*got.CompletedSequence != *first.CompletedSequence {
		t.Fatalf("replay changed the job: first %+v, now %+v", first, got)
	}
	if n := len(f.completedPayloads(runID)); n != 1 {
		t.Fatalf("completed entries = %d, want 1", n)
	}
}

// A repeated, empty or non-hex signature header is refused
// signature_malformed: repeated and non-hex by the handler, empty by the
// store's length check. The "hex prefix then non-hex" row decodes 64 bytes
// before the error, so only the handler's decode-error arm refuses it (the
// store would answer signature_invalid).
func TestSpecialistJobResult_MalformedSignatureHeader401(t *testing.T) {
	f := newSJPG(t)
	completions, nonce := sjGolden(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	f.attestedAdmission(job.JobID, nonce)
	good := hex.EncodeToString(make([]byte, ed25519.SignatureSize))
	for name, headers := range map[string][]string{
		"not hex":                 {"zz-not-hex"},
		"hex prefix then non-hex": {good + "zz"},
		"empty":                   {""},
		"repeated":                {good, good},
	} {
		t.Run(name, func(t *testing.T) {
			w := f.resultAs(job.JobID, string(completions["inventory"]), func(r *http.Request) *http.Request {
				for _, h := range headers {
					r.Header.Add(specialistjob.ResultSignatureHeader, h)
				}
				return sjIdentity(sjAttesterID)(r)
			})
			requireAttestationRefusal(t, w, specialistjob.ReasonSignatureMalformed)
		})
	}
	f.requireStillRunning(job.JobID)
}

// An attest-only runner credential can never REQUEST a job.
func TestSpecialistJobRequest_AttestScopeCannotRequest(t *testing.T) {
	f := newSJPG(t)
	body := sjRequestBody(t, specialistjob.KindDraft, map[string]any{"repo": "kuhlman-labs/fishhawk"})
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, sjIdentity(sjAttesterID))
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
	if n := f.jobCount(); n != 0 {
		t.Fatalf("an attest-only token created %d jobs", n)
	}
}

func TestSpecialistJobGet_AttestScopeMayRead(t *testing.T) {
	f := newSJPG(t)
	job, _ := f.queuedJob(specialistjob.KindInventory)
	got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjIdentity(sjAttesterID)), http.StatusOK)
	if got.JobID != job.JobID || got.Provenance != "" || got.BlockingEligible {
		t.Fatalf("attest-scope GET = %+v", got)
	}
}

// TestSpecialistJobRoutes_ScopeMatrix pins requireSpecialistJobScope on the
// three runner routes (approval condition C1): the route scope OR the attest
// scope passes, neither is 403, anonymous is 401, and a cookie session
// (TokenID "") keeps requireWriteScope's bypass — its pre-change outcome
// (GET 200, admission 200 with no key, result 200 bearer).
func TestSpecialistJobRoutes_ScopeMatrix(t *testing.T) {
	completions, nonce := sjGolden(t)
	neither := Identity{Subject: "github:none", TokenID: "tok-none", Scopes: []string{"read:audit"}}
	type route struct {
		name  string
		scope string
		call  func(f *sjPG, jobID string, id func(*http.Request) *http.Request) *httptest.ResponseRecorder
	}
	routes := []route{
		{"get", "read:runs", func(f *sjPG, jobID string, id func(*http.Request) *http.Request) *httptest.ResponseRecorder {
			return f.get(jobID, id)
		}},
		{"admission", "write:stages", func(f *sjPG, jobID string, id func(*http.Request) *http.Request) *httptest.ResponseRecorder {
			return f.admit(jobID, nonce, id)
		}},
		{"result", "write:stages", func(f *sjPG, jobID string, id func(*http.Request) *http.Request) *httptest.ResponseRecorder {
			decodeSJ[specialistJobAdmissionResponse](f.t, f.admit(jobID, nonce, sjOperator), http.StatusOK)
			return f.resultAs(jobID, string(completions["inventory"]), id)
		}},
	}
	for _, rt := range routes {
		t.Run(rt.name+"/cookie session", func(t *testing.T) {
			f := newSJPG(t)
			job, _ := f.queuedJob(specialistjob.KindInventory)
			w := rt.call(f, job.JobID, sjIdentity(sjCookieID))
			if w.Code != http.StatusOK {
				t.Fatalf("cookie session: status %d, want 200 (requireWriteScope's bypass); body %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "attestation_key") || strings.Contains(w.Body.String(), specialistjob.ProvenanceRunnerAttested) {
				t.Fatalf("a cookie session obtained attestation: %s", w.Body.String())
			}
		})
		t.Run(rt.name+"/route scope only", func(t *testing.T) {
			f := newSJPG(t)
			job, _ := f.queuedJob(specialistjob.KindInventory)
			id := Identity{Subject: "github:route", TokenID: "tok-route", Scopes: []string{rt.scope}}
			if w := rt.call(f, job.JobID, sjIdentity(id)); w.Code != http.StatusOK {
				t.Fatalf("route scope %s: status %d; body %s", rt.scope, w.Code, w.Body.String())
			}
		})
		t.Run(rt.name+"/attest scope only", func(t *testing.T) {
			f := newSJPG(t)
			job, _ := f.queuedJob(specialistjob.KindInventory)
			if w := rt.call(f, job.JobID, sjIdentity(sjAttesterID)); w.Code != http.StatusOK {
				t.Fatalf("attest scope: status %d; body %s", w.Code, w.Body.String())
			}
		})
		t.Run(rt.name+"/neither scope", func(t *testing.T) {
			f := newSJPG(t)
			job, _ := f.queuedJob(specialistjob.KindInventory)
			requireCrewRefusal(t, rt.call(f, job.JobID, sjIdentity(neither)), http.StatusForbidden, "insufficient_scope")
		})
		t.Run(rt.name+"/anonymous", func(t *testing.T) {
			f := newSJPG(t)
			job, _ := f.queuedJob(specialistjob.KindInventory)
			requireCrewRefusal(t, rt.call(f, job.JobID, nil), http.StatusUnauthorized, "authentication_required")
		})
	}
}

// --- First-officer seat requests (E85.6 / #4148). Counterfactuals run by
// hand (control mutated, test run, RED observed, file restored):
//   - C2 seat profile: adding write:stages to firstofficer.SeatScopes turned
//     TestSpecialistJobRequest_FirstOfficerSeatCannotAdmitOrResult RED (the
//     seat's admission answered 200 admitted).
//   - C5 requester seat: deriving SeatOperator for a seat turned
//     TestSpecialistJobRequest_FirstOfficerSeatRequester RED.
//   - C6 scope narrowness: accepting RequestScope from any identity turned
//     TestSpecialistJobRequest_RequestScopeIsSeatOnly RED (201).
//   - C7 subject confinement: deleting the specialistJobSubjectInSeat call
//     turned TestSpecialistJobRequest_FirstOfficerSubjectConfinement RED (the
//     request naming the unlinked run answered 201).

func sjSeat(subject string, scopes ...string) func(*http.Request) *http.Request {
	if scopes == nil {
		scopes = firstofficer.SeatScopes()
	}
	return sjIdentity(Identity{Subject: subject, TokenID: "seat-token", Scopes: scopes})
}

func TestSpecialistJobRequest_FirstOfficerSeatRequester(t *testing.T) {
	f := newSJPG(t)
	runID := f.seedRun(nil)
	subj := runSeat(runID)
	job := f.request(specialistjob.KindInventory, map[string]any{"run_id": runID.String(), "head_sha": sjSHA1}, sjSeat(subj))
	want := specialistjob.Requester{Seat: specialistjob.SeatFirstOfficer, ActorKind: "agent", Subject: subj}
	if job.Requester != want {
		t.Fatalf("requester = %+v, want %+v", job.Requester, want)
	}
	entries := f.runEntries(runID)
	if len(entries) != 1 || entries[0].actorKind != "agent" || entries[0].actorSubject != subj {
		t.Fatalf("requested entry = %+v, want actor agent/%s", entries, subj)
	}
	// A draft from the seat completes in-process like an operator's.
	draft := f.request(specialistjob.KindDraft, map[string]any{"run_id": runID.String()}, sjSeat(subj))
	if draft.State != string(specialistjob.StateCompleted) || draft.Requester.Seat != specialistjob.SeatFirstOfficer {
		t.Fatalf("seat draft = %+v", draft)
	}
}

// The seat profile carries neither write:stages nor the attest scope, so a
// seat can request but never admit or ship a result.
func TestSpecialistJobRequest_FirstOfficerSeatCannotAdmitOrResult(t *testing.T) {
	f := newSJPG(t)
	job, runID := f.queuedJob(specialistjob.KindInventory)
	seat := sjSeat(runSeat(runID))
	requireCrewRefusal(t, f.admit(job.JobID, "n", seat), http.StatusForbidden, "insufficient_scope")
	w := sjCall(t, f.srv.handleSpecialistJobResult, http.MethodPost, "/v0/specialist-jobs/"+job.JobID+"/result", job.JobID, `{}`, seat)
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
	if got := decodeSJ[specialistJobResponse](t, f.get(job.JobID, sjOperator), http.StatusOK); got.State != string(specialistjob.StateQueued) {
		t.Fatalf("job state after seat admission attempt = %q, want queued", got.State)
	}
}

func TestSpecialistJobRequest_RequestScopeIsSeatOnly(t *testing.T) {
	f := newSJPG(t)
	runID := f.seedRun(nil)
	body := sjRequestBody(t, specialistjob.KindInventory, map[string]any{"run_id": runID.String(), "head_sha": sjSHA1})
	w := sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, operator(specialistjob.RequestScope))
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
	// A seat WITHOUT the request scope is refused too.
	w = sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, sjSeat(runSeat(runID), "read:runs", "write:stages"))
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
	if n := f.jobCount(); n != 0 {
		t.Fatalf("refused requests created %d jobs", n)
	}
}

func TestSpecialistJobRequest_FirstOfficerSubjectConfinement(t *testing.T) {
	f := newSJPG(t)
	camps := newFOCampaignRepo()
	f.srv.cfg.CampaignRepo = camps
	cid, otherCID := uuid.New(), uuid.New()
	camps.campaigns[cid] = &campaign.Campaign{ID: cid, State: campaign.StateRunning}
	camps.accounts[cid], camps.accounts[otherCID] = "", ""
	linked, unlinked := f.seedRun(nil), f.seedRun(nil)
	camps.byRun[linked] = []*campaign.Item{{ID: uuid.New(), CampaignID: cid, RunID: &linked}}
	camps.byRun[unlinked] = []*campaign.Item{{ID: uuid.New(), CampaignID: otherCID, RunID: &unlinked}}

	call := func(seat func(*http.Request) *http.Request, subject map[string]any) *httptest.ResponseRecorder {
		body := sjRequestBody(t, specialistjob.KindInventory, subject)
		return sjCall(t, f.srv.handleRequestSpecialistJob, http.MethodPost, "/v0/specialist-jobs", "", body, seat)
	}
	cseat := sjSeat(campaignSeat(cid))
	rseat := sjSeat(runSeat(linked))

	refused := []struct {
		name    string
		seat    func(*http.Request) *http.Request
		subject map[string]any
	}{
		{"campaign seat, run outside the campaign", cseat, map[string]any{"run_id": unlinked.String(), "head_sha": sjSHA1}},
		{"campaign seat, another campaign", cseat, map[string]any{"campaign_id": otherCID.String(), "head_sha": sjSHA1}},
		{"campaign seat, own campaign but foreign run", cseat, map[string]any{"campaign_id": cid.String(), "run_id": unlinked.String(), "head_sha": sjSHA1}},
		{"campaign seat, run-less and campaign-less subject", cseat, map[string]any{"repo": "kuhlman-labs/fishhawk", "head_sha": sjSHA1}},
		{"run seat, another run", rseat, map[string]any{"run_id": unlinked.String(), "head_sha": sjSHA1}},
		{"run seat, a campaign subject", rseat, map[string]any{"campaign_id": cid.String(), "head_sha": sjSHA1}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			requireCrewRefusal(t, call(tc.seat, tc.subject), http.StatusForbidden, "first_officer_subject_out_of_seat")
		})
	}
	if n := f.jobCount(); n != 0 {
		t.Fatalf("out-of-seat requests created %d jobs", n)
	}

	for name, tc := range map[string]struct {
		seat    func(*http.Request) *http.Request
		subject map[string]any
	}{
		"campaign seat, linked run":   {cseat, map[string]any{"run_id": linked.String(), "head_sha": sjSHA1}},
		"campaign seat, own campaign": {cseat, map[string]any{"campaign_id": cid.String(), "head_sha": sjSHA1}},
		"run seat, own run":           {rseat, map[string]any{"run_id": linked.String(), "head_sha": sjSHA1}},
	} {
		if w := call(tc.seat, tc.subject); w.Code != http.StatusCreated {
			t.Fatalf("%s: status %d, body %s", name, w.Code, w.Body.String())
		}
	}

	// A seat-membership read failure fails closed.
	before := f.jobCount()
	camps.listErr = errors.New("db down")
	if w := call(cseat, map[string]any{"run_id": linked.String(), "head_sha": sjSHA1}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("membership read error: status %d, want 503", w.Code)
	}
	if n := f.jobCount(); n != before {
		t.Fatal("a membership read error created a job")
	}
	// A malformed seat subject (unreachable past the middleware) is refused.
	w := call(sjSeat("first_officer/run/not-a-uuid"), map[string]any{"run_id": linked.String(), "head_sha": sjSHA1})
	requireCrewRefusal(t, w, http.StatusUnauthorized, "first_officer_seat_invalid")
}
