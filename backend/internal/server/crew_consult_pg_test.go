package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// The consult round-trip suite (E77.5 / #3739): HTTP send -> budget +
// registry -> mailbox -> audit chain (stage-stamped) -> derived row ->
// detached responder -> RespondToCrewMessage threaded reply -> ?wait render,
// over a real pgtest database. Handlers are driven directly with an injected
// identity, as the E77.3 suite does.

// consultDoc is a response_required consult on anchor.
func consultDoc(recipient, anchor, payload string) string {
	return `{"schema_version":"crew-message-v1","type":"consult","recipient_role":"` + recipient +
		`","anchor":` + anchor + `,"payload":` + payload + `,"response_required":true}`
}

// withResponders installs reg and drains every detached consult goroutine at
// test end, before the pool closes (Cleanup is LIFO; the pool's is older).
func (f *crewPG) withResponders(t *testing.T, reg map[crewmessage.Role]CrewResponder) {
	t.Helper()
	g, err := NewCrewResponderRegistry(reg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	f.srv.cfg.CrewResponders = g
	t.Cleanup(f.srv.waitBackgroundReviews)
}

func (f *crewPG) stageOf(t *testing.T, runID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM stages WHERE run_id = $1`, runID).Scan(&id); err != nil {
		t.Fatalf("stage of run: %v", err)
	}
	return id
}

// awaitState polls the derived row until it leaves open or bound elapses.
func (f *crewPG) awaitState(t *testing.T, seq int64, bound time.Duration) crewmessage.Row {
	t.Helper()
	end := time.Now().Add(bound)
	for {
		row, err := f.mailbox.Store().Get(context.Background(), seq)
		if err != nil {
			t.Fatalf("get row: %v", err)
		}
		if row.State != crewmessage.StateOpen || time.Now().After(end) {
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// disposedReason reads the reason recorded on seq's disposition chain entry.
func (f *crewPG) disposedReason(t *testing.T, seq int64) (string, string) {
	t.Helper()
	var disp, reason string
	if err := f.pool.QueryRow(context.Background(), `SELECT payload->>'disposition', coalesce(payload->>'reason','')
		FROM audit_entries WHERE category = $1 AND payload->>'sent_sequence' = $2`,
		crewmessage.CategoryDisposed, jsonInt(seq)).Scan(&disp, &reason); err != nil {
		t.Fatalf("disposition entry of %d: %v", seq, err)
	}
	return disp, reason
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func shortenConsultWindow(t *testing.T, d time.Duration) {
	t.Helper()
	orig := crewConsultWindow
	crewConsultWindow = d
	t.Cleanup(func() { crewConsultWindow = orig })
}

const consultAnswerLiteral = "IBIS-5518-historian-answer"

func TestCrewConsult_EndToEnd_DeterministicResponder(t *testing.T) {
	f := newCrewPG(t)
	seen := make(chan CrewConsultRequest, 1)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{crewmessage.RoleHistorian: &fakeResponder{
		answer: CrewConsultAnswer{Summary: consultAnswerLiteral, Detail: "### heading\n" + crewEnd,
			Evidence: []crewmessage.EvidenceReference{{Kind: crewmessage.EvidenceDecisionRecord, Ref: "ADR-081"}}},
		seen: seen,
	}})
	runA := f.seedRun(t, run.StageTypePlan)
	stageA := f.stageOf(t, runA)
	tok := runBound(runA, "mcp:read", scopeWriteMessages)

	before := time.Now()
	sent := f.send(t, consultDoc("historian", runAnchor(runA), hostileConsultPayload()), tok)
	if sent.ConsultDeadline == nil || sent.ConsultDeadline.Before(before) || sent.ConsultDeadline.After(time.Now().Add(crewConsultWindow)) {
		t.Fatalf("consult_deadline = %v, want within now+window", sent.ConsultDeadline)
	}
	if sent.ConsultBudgetRemaining == nil || *sent.ConsultBudgetRemaining != maxCrewConsultsPerStage-1 {
		t.Fatalf("consult_budget_remaining = %v, want %d", sent.ConsultBudgetRemaining, maxCrewConsultsPerStage-1)
	}
	req := <-seen
	if req.SentSequence != sent.SentSequence || req.StageID != stageA || req.SenderRole != crewmessage.RolePlanner ||
		!strings.Contains(req.Question, crewHostile) {
		t.Fatalf("responder saw %+v", req)
	}

	done, _ := f.waitAsync(t, sent.SentSequence, 10, tok)
	w := <-done
	got := decodeCrewGet(t, w)
	if !got.Answered || got.Answer == nil || got.Answer.SenderRole != string(crewmessage.RoleHistorian) {
		t.Fatalf("answered = %v, answer = %+v", got.Answered, got.Answer)
	}
	requireOnlyInsideEnvelope(t, got.Answer.Rendered, consultAnswerLiteral)
	requireNoRawOutsideRendered(t, w.Body.Bytes(), consultAnswerLiteral)
	if !strings.Contains(got.Answer.Rendered, "decision_record:ADR-081") {
		t.Errorf("answer evidence not rendered: %s", got.Answer.Rendered)
	}

	row := f.awaitState(t, sent.SentSequence, timescale.D(5*time.Second))
	if row.State != crewmessage.StateAccepted {
		t.Fatalf("consult state = %q, want accepted", row.State)
	}
	if n := f.count(t, crewmessage.CategorySent); n != 2 {
		t.Fatalf("crew_message_sent entries = %d, want consult + threaded answer", n)
	}
	if n := f.count(t, crewmessage.CategoryDisposed); n != 1 {
		t.Fatalf("crew_message_disposed entries = %d, want 1", n)
	}
	if disp, _ := f.disposedReason(t, sent.SentSequence); disp != string(crewmessage.DispositionAccepted) {
		t.Fatalf("disposition = %q, want accepted", disp)
	}
}

// (1) + C1: the (cap+1)th consult on one stage is refused BEFORE the append.
func TestCrewConsult_BudgetExhausted(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: "a"}},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	stageA := f.stageOf(t, runA)
	// Seed the budget BY CONSTRUCTION: two consults already stamped with THIS
	// stage, recorded straight through the mailbox (no dispatch).
	for i := 0; i < maxCrewConsultsPerStage; i++ {
		doc, err := crewmessage.WithSenderRole([]byte(consultDoc("historian", runAnchor(runA), crewConsultPayload)), crewmessage.RolePlanner)
		if err != nil {
			t.Fatalf("seed doc: %v", err)
		}
		if _, err := f.mailbox.Send(context.Background(), crewmessage.SendParams{RawMessage: doc, StageID: &stageA}); err != nil {
			t.Fatalf("seed consult: %v", err)
		}
	}
	w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
		consultDoc("historian", runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "crew_consult_budget_exhausted")
	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if d := body.Error.Details; d["max"] != float64(maxCrewConsultsPerStage) || d["used"] != float64(maxCrewConsultsPerStage) {
		t.Fatalf("details = %v, want max=used=%d", d, maxCrewConsultsPerStage)
	}
	// Refuse-BEFORE-append, read from the chain.
	if n := f.count(t, crewmessage.CategorySent); n != maxCrewConsultsPerStage {
		t.Fatalf("crew_message_sent entries = %d after a refused consult, want %d", n, maxCrewConsultsPerStage)
	}
}

// The HTTP consult branch stamps the executing stage on the chain entry, so
// consults sent over the wire are what the cap counts: cap sends succeed with
// a decreasing budget, the next is refused, and no refused entry is appended.
func TestCrewConsult_HTTPConsultsConsumeTheStageBudget(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: "a"}},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	tok := runBound(runA, "mcp:read", scopeWriteMessages)
	for i := 0; i < maxCrewConsultsPerStage; i++ {
		sent := f.send(t, consultDoc("historian", runAnchor(runA), crewConsultPayload), tok)
		want := maxCrewConsultsPerStage - i - 1
		if sent.ConsultBudgetRemaining == nil {
			t.Fatalf("consult %d carries no consult_budget_remaining", i)
		}
		if got := *sent.ConsultBudgetRemaining; got != want {
			t.Fatalf("consult %d budget_remaining = %d, want %d", i, got, want)
		}
	}
	w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
		consultDoc("historian", runAnchor(runA), crewConsultPayload), tok)
	requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "crew_consult_budget_exhausted")
}

// A threaded reply and a consult on ANOTHER stage never consume the budget,
// and a non-response_required consult is not a budgeted consult.
func TestCrewConsult_BudgetCountsOnlyThisStagesConsultRoots(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: "a"}},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	stageA := f.stageOf(t, runA)
	otherStage := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 2, 'plan', 'agent', 'claude-code', 'failed')`, otherStage, runA); err != nil {
		t.Fatalf("seed stage: %v", err)
	}
	mk := func(raw string) []byte {
		doc, err := crewmessage.WithSenderRole([]byte(raw), crewmessage.RolePlanner)
		if err != nil {
			t.Fatalf("doc: %v", err)
		}
		return doc
	}
	ctx := context.Background()
	root, err := f.mailbox.Send(ctx, crewmessage.SendParams{RawMessage: mk(consultDoc("historian", runAnchor(runA), crewConsultPayload)), StageID: &otherStage})
	if err != nil {
		t.Fatalf("other-stage consult: %v", err)
	}
	rootSeq := root.SentSequence
	for _, p := range []crewmessage.SendParams{
		// A threaded response_required CONSULT stamped with THIS stage: only
		// the thread-root filter keeps it out of the count.
		{RawMessage: mk(consultDoc("historian", runAnchor(runA), crewConsultPayload)), StageID: &stageA, ThreadRootSequence: &rootSeq},
		{RawMessage: mk(crewDoc("consult", "", "historian", runAnchor(runA), crewConsultPayload)), StageID: &stageA},
		{RawMessage: mk(crewDoc("consult", "", "historian", runAnchor(runA), crewConsultPayload)), StageID: &stageA},
	} {
		if _, err := f.mailbox.Send(ctx, p); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	used, err := f.srv.crewConsultBudgetUsed(ctx, runA, stageA)
	if err != nil || used != 0 {
		t.Fatalf("used = %d, %v; want 0", used, err)
	}
}

// (2) + C2: a legal, addressable, non-implement role with NO registered
// responder is refused — the registry holds only historian, so the miss is
// the only thing in the path.
func TestCrewConsult_ResponderUnavailable(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: "a"}},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
		consultDoc("security", runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "crew_responder_unavailable")
	if !strings.Contains(w.Body.String(), `"recipient_role":"security"`) {
		t.Fatalf("details missing recipient_role: %s", w.Body.String())
	}
	if n := f.count(t, crewmessage.CategorySent); n != 0 {
		t.Fatalf("crew_message_sent entries = %d after a refused consult, want 0", n)
	}
}

// (3) regression pin over E77.1's CanReceive, at the HTTP layer.
func TestCrewConsult_ImplementRecipientRefused(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: "a"}},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
		consultDoc("implementer", runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "recipient_not_addressable")
	if n := f.count(t, crewmessage.CategorySent); n != 0 {
		t.Fatalf("crew_message_sent entries = %d, want 0", n)
	}
}

type stageSnapshot struct {
	state      string
	startedAt  *time.Time
	selfRetry  int
	nonCrew    int
	stageScope int
}

func (f *crewPG) snapshotStage(t *testing.T, runID, stageID uuid.UUID) stageSnapshot {
	t.Helper()
	ctx := context.Background()
	var s stageSnapshot
	if err := f.pool.QueryRow(ctx, `SELECT state, started_at, self_retry_count FROM stages WHERE id = $1`, stageID).
		Scan(&s.state, &s.startedAt, &s.selfRetry); err != nil {
		t.Fatalf("read stage: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries WHERE run_id = $1 AND category NOT LIKE 'crew_message_%'`, runID).
		Scan(&s.nonCrew); err != nil {
		t.Fatalf("count non-crew entries: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries WHERE stage_id = $1 AND category <> $2`, stageID, crewmessage.CategorySent).
		Scan(&s.stageScope); err != nil {
		t.Fatalf("count stage entries: %v", err)
	}
	return s
}

// (4) + C4 + approval condition 4: a responder that never returns leaves the
// consult EXPIRED at the shortened window with a reason on the disposition
// entry, and the stage's COMMITTED STATE is unchanged.
func TestCrewConsult_ExpiresAtDeadline(t *testing.T) {
	f := newCrewPG(t)
	gate := make(chan struct{})
	f.withResponders(t, map[crewmessage.Role]CrewResponder{crewmessage.RoleHistorian: &fakeResponder{gate: gate}})
	t.Cleanup(func() { close(gate) }) // LIFO: runs before the drain above.
	window := timescale.D(300 * time.Millisecond)
	shortenConsultWindow(t, window)
	runA := f.seedRun(t, run.StageTypePlan)
	stageA := f.stageOf(t, runA)
	before := f.snapshotStage(t, runA, stageA)

	sent := f.send(t, consultDoc("historian", runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
	row := f.awaitState(t, sent.SentSequence, window+timescale.D(5*time.Second))
	if row.State != crewmessage.StateExpired {
		t.Fatalf("consult state = %q past its deadline, want expired", row.State)
	}
	if row.ReasonSequence == nil {
		t.Fatal("expired consult carries no reason entry")
	}
	disp, reason := f.disposedReason(t, sent.SentSequence)
	if disp != string(crewmessage.DispositionExpired) || !strings.Contains(reason, "did not answer before the consult deadline") {
		t.Fatalf("disposition = %q reason = %q", disp, reason)
	}
	if n := f.count(t, crewmessage.CategorySent); n != 1 {
		t.Fatalf("crew_message_sent entries = %d, want only the consult (no answer)", n)
	}
	after := f.snapshotStage(t, runA, stageA)
	if after.state != before.state || after.selfRetry != before.selfRetry ||
		(after.startedAt == nil) != (before.startedAt == nil) ||
		(after.startedAt != nil && !after.startedAt.Equal(*before.startedAt)) {
		t.Fatalf("stage row changed by an expired consult: before %+v after %+v", before, after)
	}
	if after.nonCrew != 0 || after.stageScope != 0 {
		t.Fatalf("an expired consult wrote non-crew or stage-scoped entries: %+v", after)
	}
}

// (5) a responder error disposes EXPIRED, not accepted.
func TestCrewConsult_ResponderErrorExpires(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{err: errors.New("precedent index unavailable")},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	sent := f.send(t, consultDoc("historian", runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
	row := f.awaitState(t, sent.SentSequence, timescale.D(5*time.Second))
	if row.State != crewmessage.StateExpired {
		t.Fatalf("state = %q, want expired", row.State)
	}
	if _, reason := f.disposedReason(t, sent.SentSequence); !strings.Contains(reason, "precedent index unavailable") {
		t.Fatalf("reason = %q, want the responder error", reason)
	}
}

// A blank answer is no answer.
func TestCrewConsult_BlankAnswerExpires(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: "  "}},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	sent := f.send(t, consultDoc("historian", runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
	if row := f.awaitState(t, sent.SentSequence, timescale.D(5*time.Second)); row.State != crewmessage.StateExpired {
		t.Fatalf("state = %q, want expired", row.State)
	}
	if _, reason := f.disposedReason(t, sent.SentSequence); !strings.Contains(reason, "blank answer") {
		t.Fatalf("reason = %q", reason)
	}
}

// An answer the contract rejects (an empty evidence ref) is not recorded and
// the consult expires with the cause.
func TestCrewConsult_InvalidAnswerExpires(t *testing.T) {
	f := newCrewPG(t)
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: "ok",
			Evidence: []crewmessage.EvidenceReference{{Kind: crewmessage.EvidenceRun, Ref: ""}}}},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	sent := f.send(t, consultDoc("historian", runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
	if row := f.awaitState(t, sent.SentSequence, timescale.D(5*time.Second)); row.State != crewmessage.StateExpired {
		t.Fatalf("state = %q, want expired", row.State)
	}
	if _, reason := f.disposedReason(t, sent.SentSequence); !strings.Contains(reason, "could not be recorded") {
		t.Fatalf("reason = %q", reason)
	}
	if n := f.count(t, crewmessage.CategorySent); n != 1 {
		t.Fatalf("crew_message_sent entries = %d, want only the consult", n)
	}
}

// (6) the production (empty) registry refuses every consult by name.
func TestCrewConsult_UnconfiguredRegistryRefusesSend(t *testing.T) {
	f := newCrewPG(t)
	runA := f.seedRun(t, run.StageTypePlan)
	for _, role := range []string{"historian", "architect", "security", "reviewer", "captain"} {
		w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
			consultDoc(role, runAnchor(runA), crewConsultPayload), runBound(runA, "mcp:read", scopeWriteMessages))
		requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "crew_responder_unavailable")
	}
	if n := f.count(t, crewmessage.CategorySent); n != 0 {
		t.Fatalf("crew_message_sent entries = %d, want 0", n)
	}
}

// The consult outlives the sender's HTTP connection: cancelling the request
// context right after the 201 does not stop the answer landing.
func TestCrewConsult_SurvivesSenderDisconnect(t *testing.T) {
	f := newCrewPG(t)
	gate := make(chan struct{})
	f.withResponders(t, map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian: &fakeResponder{answer: CrewConsultAnswer{Summary: consultAnswerLiteral}, gate: gate},
	})
	runA := f.seedRun(t, run.StageTypePlan)
	reqCtx, cancel := context.WithCancel(context.Background())
	tok := runBound(runA, "mcp:read", scopeWriteMessages)
	w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
		consultDoc("historian", runAnchor(runA), crewConsultPayload),
		func(r *http.Request) *http.Request { return tok(r.WithContext(reqCtx)) })
	cancel()
	close(gate)
	if w.Code != http.StatusCreated {
		t.Fatalf("send = %d %s", w.Code, w.Body.String())
	}
	var sent crewMessageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &sent)
	if row := f.awaitState(t, sent.SentSequence, timescale.D(5*time.Second)); row.State != crewmessage.StateAccepted {
		t.Fatalf("state = %q after sender disconnect, want accepted", row.State)
	}
	if n := f.count(t, crewmessage.CategorySent); n != 2 {
		t.Fatalf("crew_message_sent entries = %d, want consult + answer", n)
	}
}
