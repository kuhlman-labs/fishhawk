package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
)

// First-officer seat end to end over real Postgres (E85.6 / #4148): server.New
// over HTTP against the real api_tokens, audit chain, crew mailbox,
// review_concerns, specialist_jobs, approvals, runs, campaigns and captain
// stores. It crosses request payload -> middleware/identity -> token
// persistence -> specialist-job store -> crew mailbox -> concern rows ->
// gate/merge refusal -> gate-view render, and reads back the three
// first_officer_* audit categories from the chain (approval condition 4).

type foPG struct {
	t      *testing.T
	pool   *pgxpool.Pool
	srv    *Server
	tokens apitoken.Repository
}

func newFOPG(t *testing.T) *foPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &foPG{t: t, pool: pool, tokens: apitoken.NewPostgresRepository(pool)}
	f.srv = New(Config{
		Addr:           "127.0.0.1:0",
		APITokenRepo:   f.tokens,
		RunRepo:        run.NewPostgresRepository(pool),
		AuditRepo:      audit.NewPostgresRepository(pool),
		CampaignRepo:   campaign.NewPostgresRepository(pool),
		CrewMailbox:    crewmessage.NewMailbox(pool, 0),
		ConcernRepo:    concern.NewPostgresRepository(pool),
		ApprovalRepo:   approval.NewPostgresRepository(pool),
		SpecialistJobs: specialistjob.NewStore(pool),
		CaptainStore:   captain.NewStore(pool),
	})
	return f
}

func (f *foPG) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedRun seeds a running run with a pull request, a succeeded plan stage and
// a succeeded implement stage; it returns the run and plan stage ids.
func (f *foPG) seedRun(repo string) (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	runID, planID := uuid.New(), uuid.New()
	f.exec(`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, pull_request_url)
		VALUES ($1, $2, 'feature_change', 'sha', 'cli', 'running', 'local', 'https://github.com/x/y/pull/7')`, runID, repo)
	f.exec(`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 1, 'plan', 'agent', 'claude-code', 'succeeded')`, planID, runID)
	f.exec(`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 2, 'implement', 'agent', 'claude-code', 'succeeded')`, uuid.New(), runID)
	return runID, planID
}

func (f *foPG) issue(subject string, scopes ...string) string {
	f.t.Helper()
	tok, err := f.tokens.Issue(context.Background(), subject, scopes)
	if err != nil {
		f.t.Fatalf("issue %s: %v", subject, err)
	}
	return tok.PlainText
}

func (f *foPG) do(method, path, bearer, body string, header ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

func (f *foPG) count(category string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_entries WHERE category = $1`, category).Scan(&n); err != nil {
		f.t.Fatalf("count %s: %v", category, err)
	}
	return n
}

func (f *foPG) activeTokens(subject string) int {
	f.t.Helper()
	toks, err := f.tokens.ListForSubject(context.Background(), subject)
	if err != nil {
		f.t.Fatalf("list tokens: %v", err)
	}
	return len(toks)
}

func requireCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("status = %d, want %d %s:\n%s", w.Code, status, code, w.Body.String())
	}
}

func TestFirstOfficerSeat_EndToEndPostgres(t *testing.T) {
	f := newFOPG(t)
	ctx := context.Background()
	runID, planID := f.seedRun("kuhlman-labs/fishhawk")
	human := f.issue("github:alice", "write:approvals", "write:runs", "write:stages", "read:runs", "read:audit")

	// (a) attach: the 201 token's scopes equal SeatScopes; the row reads back.
	w := f.do(http.MethodPost, "/v0/first-officer-seats", human, `{"run_id":"`+runID.String()+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("attach = %d:\n%s", w.Code, w.Body.String())
	}
	var att firstOfficerSeatAttachResponse
	_ = json.Unmarshal(w.Body.Bytes(), &att)
	if strings.Join(att.Scopes, ",") != strings.Join(firstofficer.SeatScopes(), ",") || att.SeatSubject != firstofficer.SubjectFor(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID}) {
		t.Fatalf("attach = %+v", att)
	}
	if n := f.count(auditFirstOfficerSeatAttached); n != 1 {
		t.Fatalf("first_officer_seat_attached rows = %d, want 1", n)
	}
	seat := att.Token

	// (b) prohibited verbs: 403 and one first_officer_action_refused row each.
	for _, c := range []struct{ path, body string }{
		{"/v0/stages/" + planID.String() + "/approvals", `{"decision":"approve"}`},
		{"/v0/runs/" + runID.String() + "/stages/" + planID.String() + "/host-dispatch", `{}`},
		{"/v0/runs/" + runID.String() + "/merge", `{"verdict":"lgtm"}`},
		{"/v0/runs/" + runID.String() + "/stages/" + planID.String() + "/reap-failure", `{}`},
	} {
		if w := f.do(http.MethodPost, c.path, seat, c.body); w.Code != http.StatusForbidden {
			t.Fatalf("seat POST %s = %d, want 403:\n%s", c.path, w.Code, w.Body.String())
		}
	}
	if n := f.count(auditFirstOfficerActionRefused); n != 4 {
		t.Fatalf("first_officer_action_refused rows = %d, want 4", n)
	}

	// (c) the seat requests a draft job; admission and result are refused.
	req := map[string]any{"schema_version": specialistjob.SchemaVersion, "kind": string(specialistjob.KindOverlapScan),
		"subject": map[string]any{"run_id": runID.String(), "head_sha": sjSHA1}, "inputs": sjInputs(specialistjob.KindOverlapScan)}
	reqBody, _ := json.Marshal(req)
	w = f.do(http.MethodPost, "/v0/specialist-jobs", seat, string(reqBody))
	if w.Code != http.StatusCreated {
		t.Fatalf("seat job request = %d:\n%s", w.Code, w.Body.String())
	}
	var job specialistJobResponse
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	if job.Requester.Seat != "first_officer" {
		t.Fatalf("requester = %+v, want seat first_officer", job.Requester)
	}
	completions, nonce := sjGolden(t)
	admit := `{"host":"` + sjHost + `","nonce":"` + nonce + `"}`
	if w := f.do(http.MethodPost, "/v0/specialist-jobs/"+job.JobID+"/admission", seat, admit); w.Code != http.StatusForbidden {
		t.Fatalf("seat admission = %d, want 403", w.Code)
	}
	result := string(completions["overlap_scan"])
	if w := f.do(http.MethodPost, "/v0/specialist-jobs/"+job.JobID+"/result", seat, result); w.Code != http.StatusForbidden {
		t.Fatalf("seat result = %d, want 403", w.Code)
	}

	// (d) a runner_attested FAILING completion, BY CONSTRUCTION through the
	// attester's admission key and an Ed25519 signature over ResultMessage.
	attester := f.issue("specialist-runner/fo-host", specialistjob.AttestScope)
	w = f.do(http.MethodPost, "/v0/specialist-jobs/"+job.JobID+"/admission", attester, admit)
	var adm specialistJobAdmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &adm); err != nil || w.Code != http.StatusOK || adm.AttestationKey == "" {
		t.Fatalf("attester admission = %d %+v:\n%s", w.Code, adm, w.Body.String())
	}
	rawKey, _ := base64.StdEncoding.DecodeString(adm.AttestationKey)
	sig := sjSign(t, ed25519.PrivateKey(rawKey), job.JobID, result)
	w = f.do(http.MethodPost, "/v0/specialist-jobs/"+job.JobID+"/result", attester, result, specialistjob.ResultSignatureHeader, sig)
	var done specialistJobResponse
	_ = json.Unmarshal(w.Body.Bytes(), &done)
	if w.Code != http.StatusOK || done.Verdict != "fail" || done.Provenance != specialistjob.ProvenanceRunnerAttested {
		t.Fatalf("attested result = %d %s/%s:\n%s", w.Code, done.Verdict, done.Provenance, w.Body.String())
	}
	var completedSeq int64
	if err := f.pool.QueryRow(ctx, `SELECT sequence FROM audit_entries WHERE run_id = $1 AND category = $2`,
		runID, specialistjob.CategoryCompleted).Scan(&completedSeq); err != nil {
		t.Fatalf("read completed sequence: %v", err)
	}

	// (e) the seat's finding citing it converts to a first_officer concern.
	finding := `{"schema_version":"crew-message-v1","type":"finding","recipient_role":"captain","anchor":{"run_id":"` + runID.String() +
		`"},"payload":{"summary":"overlap scan failed","severity":"high"},"evidence":[{"kind":"audit_entry","ref":"` + strconv.FormatInt(completedSeq, 10) + `"}]}`
	w = f.do(http.MethodPost, "/v0/crew-messages", seat, finding)
	if w.Code != http.StatusCreated {
		t.Fatalf("seat finding = %d:\n%s", w.Code, w.Body.String())
	}
	var sent struct {
		BlockingConcernID string `json:"blocking_concern_id"`
		SenderRole        string `json:"sender_role"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sent)
	if sent.BlockingConcernID == "" {
		t.Fatalf("send response carries no blocking_concern_id:\n%s", w.Body.String())
	}
	var provenance string
	if err := f.pool.QueryRow(ctx, `SELECT provenance FROM review_concerns WHERE id = $1`, sent.BlockingConcernID).Scan(&provenance); err != nil {
		t.Fatalf("read concern: %v", err)
	}
	if provenance != concern.ProvenanceFirstOfficer {
		t.Fatalf("concern provenance = %q, want first_officer", provenance)
	}

	// (e2) by-role delivery (ADR-089 D7): the captain addresses
	// recipient_role first_officer, and the seat lists it under read:audit.
	// Counterfactual: dropping read:audit from firstofficer.SeatScopes turned
	// this step RED (seat list by role = 403).
	notice := `{"schema_version":"crew-message-v1","type":"notice","recipient_role":"first_officer","anchor":{"run_id":"` +
		runID.String() + `"},"payload":{"summary":"re-scan after the rebase"}}`
	w = f.do(http.MethodPost, "/v0/crew-messages", human, notice)
	if w.Code != http.StatusCreated {
		t.Fatalf("captain notice = %d:\n%s", w.Code, w.Body.String())
	}
	var toFO crewMessageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &toFO)
	w = f.do(http.MethodGet, "/v0/crew-messages?recipient_role=first_officer", seat, "")
	if w.Code != http.StatusOK {
		t.Fatalf("seat list by role = %d:\n%s", w.Code, w.Body.String())
	}
	var listed crewMessageListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &listed)
	found := false
	for _, m := range listed.Items {
		if m.RecipientRole != string(crewmessage.RoleFirstOfficer) {
			t.Fatalf("by-role list carries recipient_role %q:\n%s", m.RecipientRole, w.Body.String())
		}
		found = found || (m.SentSequence == toFO.SentSequence && m.SenderRole == string(crewmessage.RoleCaptain))
	}
	if toFO.SentSequence == 0 || !found {
		t.Fatalf("seat's by-role list lacks the captain's notice %d:\n%s", toFO.SentSequence, w.Body.String())
	}

	// (f) merge refuses; the gate view lists the concern and the blocker.
	requireCode(t, f.do(http.MethodPost, "/v0/runs/"+runID.String()+"/merge", human, `{"verdict":"lgtm"}`), http.StatusConflict, codeFirstOfficerFindingOpen)
	gv := f.do(http.MethodGet, "/v0/runs/"+runID.String()+"/gate-view", human, "")
	if gv.Code != http.StatusOK {
		t.Fatalf("gate-view = %d:\n%s", gv.Code, gv.Body.String())
	}
	resp := decodeGateView(t, gv)
	if resp.MergeReadiness == nil || mergeBlockerByCode(resp.MergeReadiness, codeFirstOfficerFindingOpen) == nil {
		t.Fatalf("merge_readiness = %+v, want the first_officer_finding_open blocker", resp.MergeReadiness)
	}
	if !strings.Contains(gv.Body.String(), sent.BlockingConcernID) {
		t.Fatalf("gate view does not list concern %s", sent.BlockingConcernID)
	}

	// (g) waive needs a reason and a judgement (E94.7 / #4840); once waived
	// the hold clears and the committed row carries the judgement record.
	if w := f.do(http.MethodPost, "/v0/concerns/"+sent.BlockingConcernID+"/waive", human, `{"reason":"","judgement":"tradeoff","tradeoff_cost":"two runs touch one file"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty-reason waive = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if w := f.do(http.MethodPost, "/v0/concerns/"+sent.BlockingConcernID+"/waive", human, `{"reason":"overlap is intended"}`); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), `"field":"judgement"`) {
		t.Fatalf("judgement-less waive = %d, want 400 validation_failed field=judgement:\n%s", w.Code, w.Body.String())
	}
	if w := f.do(http.MethodPost, "/v0/concerns/"+sent.BlockingConcernID+"/waive", human, `{"reason":"overlap is intended","judgement":"tradeoff","tradeoff_cost":"two runs touch one file"}`); w.Code != http.StatusOK {
		t.Fatalf("waive = %d:\n%s", w.Code, w.Body.String())
	}
	var (
		judgement, actorKind, outcome, tradeoffCost string
		delegated, bulk, backfill                   bool
	)
	if err := f.pool.QueryRow(ctx, `SELECT judgement, judgement_actor_kind, judgement_delegated, judgement_bulk, judgement_backfill, outcome, tradeoff_cost
		FROM review_concerns WHERE id = $1`, sent.BlockingConcernID).Scan(&judgement, &actorKind, &delegated, &bulk, &backfill, &outcome, &tradeoffCost); err != nil {
		t.Fatalf("read the waived concern row: %v", err)
	}
	if judgement != "tradeoff" || actorKind != "user" || delegated || bulk || backfill || outcome != "waived" || tradeoffCost != "two runs touch one file" {
		t.Fatalf("committed row: judgement %q actor %q delegated %v bulk %v backfill %v outcome %q cost %q; want tradeoff/user/false/false/false/waived/the cost",
			judgement, actorKind, delegated, bulk, backfill, outcome, tradeoffCost)
	}
	if w := f.do(http.MethodPost, "/v0/runs/"+runID.String()+"/merge", human, `{"verdict":"lgtm"}`); strings.Contains(w.Body.String(), codeFirstOfficerFindingOpen) {
		t.Fatalf("merge still held after waive:\n%s", w.Body.String())
	}

	// (h) the anchor turns terminal: the seat's next request retires it.
	f.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, runID)
	requireCode(t, f.do(http.MethodGet, "/v0/runs/"+runID.String(), seat, ""), http.StatusUnauthorized, "first_officer_seat_retired")
	if n := f.activeTokens(att.SeatSubject); n != 0 {
		t.Fatalf("active seat tokens after retirement = %d, want 0", n)
	}
	var retired int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries WHERE run_id = $1 AND category = $2`,
		runID, auditFirstOfficerSeatRetired).Scan(&retired); err != nil || retired != 1 {
		t.Fatalf("run-chain first_officer_seat_retired rows = %d (err %v), want 1", retired, err)
	}
}

// TestFirstOfficerSeatAttach_CaptainGatePostgres pins C23 over the real
// captain store: while a captain sits, only the captain attaches or retires;
// the attach row records captain_basis captain.
func TestFirstOfficerSeatAttach_CaptainGatePostgres(t *testing.T) {
	f := newFOPG(t)
	repo := "acme/captained"
	runID, _ := f.seedRun(repo)
	raw, _ := json.Marshal(map[string]any{"repo": repo, "subject": "github:bob", "identity_verified": captain.IdentityVerified("github:bob")})
	kind, subj := audit.ActorUser, "github:bob"
	if _, err := audit.NewPostgresRepository(f.pool).AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Category: captain.CategoryAssigned, ActorKind: &kind, ActorSubject: &subj, Payload: raw,
	}); err != nil {
		t.Fatalf("seed captain: %v", err)
	}
	alice := f.issue("github:alice", "write:approvals")
	bob := f.issue("github:bob", "write:approvals")
	body := `{"run_id":"` + runID.String() + `"}`
	requireCode(t, f.do(http.MethodPost, "/v0/first-officer-seats", alice, body), http.StatusForbidden, "first_officer_attach_not_captain")
	if n := f.activeTokens(firstofficer.SubjectFor(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID})); n != 0 {
		t.Fatalf("non-captain attach minted %d seat tokens", n)
	}
	if w := f.do(http.MethodPost, "/v0/first-officer-seats", bob, body); w.Code != http.StatusCreated {
		t.Fatalf("captain attach = %d:\n%s", w.Code, w.Body.String())
	}
	var basis string
	if err := f.pool.QueryRow(context.Background(), `SELECT payload->>'captain_basis' FROM audit_entries WHERE category = $1`,
		auditFirstOfficerSeatAttached).Scan(&basis); err != nil || basis != captainBasisCaptain {
		t.Fatalf("captain_basis = %q (err %v), want captain", basis, err)
	}
	requireCode(t, f.do(http.MethodPost, "/v0/first-officer-seats/retire", alice, `{"run_id":"`+runID.String()+`","reason":"x"}`),
		http.StatusForbidden, "first_officer_attach_not_captain")
}
