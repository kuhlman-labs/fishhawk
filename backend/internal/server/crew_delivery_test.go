package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Deferred crew delivery (E77.7 / #3741): the OPEN findings/notices addressed
// to the reading stage's role reach the next plan or review render inside the
// crew quarantine envelope — never an implement render — and the signed serve
// (never the preview) records crew_message_delivered.

// seedCrewMail records one run-anchored message BY CONSTRUCTION (straight
// through the mailbox, never via the HTTP send) and returns its sent sequence.
// stageID, when non-nil, stamps the chain entry. summary is the sentinel.
func seedCrewMail(t *testing.T, f *crewPG, runID uuid.UUID, msgType crewmessage.MessageType, recipient crewmessage.Role, summary string) int64 {
	t.Helper()
	payload := `{"summary":"` + summary + `"}`
	extra := ""
	switch msgType {
	case crewmessage.TypeConsult:
		payload = `{"question":"` + summary + `"}`
		extra = `,"response_required":true`
	case crewmessage.TypeEscalation:
		payload = `{"summary":"` + summary + `","recommended_default":"keep","tradeoffs":"none"}`
	}
	row, err := f.mailbox.Send(context.Background(), crewmessage.SendParams{
		RawMessage: []byte(`{"schema_version":"crew-message-v1","type":"` + string(msgType) +
			`","sender_role":"architect","recipient_role":"` + string(recipient) + `","anchor":` +
			runAnchor(runID) + `,"payload":` + payload + extra + `}`),
		Actor: crewmessage.Actor{Kind: audit.ActorSystem, Subject: "test:architect"},
	})
	if err != nil {
		t.Fatalf("seed %s: %v", msgType, err)
	}
	return row.SentSequence
}

// newDeliveryPlanRun seeds a plan run whose prompt the signed handler serves,
// returning the server, the run, its plan stage and the stage signing key.
func newDeliveryPlanRun(t *testing.T, f *crewPG, ar audit.Repository) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
	t.Helper()
	s, sf := newConsultFoldServer(t, f, ar)
	runID := f.seedRun(t, run.StageTypePlan)
	stageID := f.stageOf(t, runID)
	priv, _ := sf.issue(t, runID)
	if _, err := f.pool.Exec(context.Background(), `UPDATE runs SET requires_charter = false WHERE id = $1`, runID); err != nil {
		t.Fatalf("record charter determination: %v", err)
	}
	return s, runID, stageID, priv
}

// servePrompt GETs the signed prompt (priv non-nil) or the preview (nil).
func servePrompt(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey) string {
	t.Helper()
	w := promptRenderRequest(t, s, stageID)
	if priv != nil {
		w = promptRequest(t, s, runID, stageID, priv, "")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	return resp.Prompt
}

// deliveredEntries reads the crew_message_delivered payloads for runID from
// the chain (committed state, not a return value).
func deliveredEntries(t *testing.T, f *crewPG, runID uuid.UUID) []crewMessageDeliveredPayload {
	t.Helper()
	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryCrewMessageDelivered)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	out := make([]crewMessageDeliveredPayload, 0, len(entries))
	for _, e := range entries {
		var p crewMessageDeliveredPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode delivery: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// recordDeliveryOn appends a crew_message_delivered entry stamped with stageID
// BY CONSTRUCTION, for the once-per-other-stage filter's fixtures.
func recordDeliveryOn(t *testing.T, f *crewPG, runID, stageID uuid.UUID, seqs ...int64) {
	t.Helper()
	payload, _ := json.Marshal(crewMessageDeliveredPayload{SentSequences: seqs, StageType: "plan", RecipientRole: "planner", Render: "plan"})
	kind := audit.ActorSystem
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Category: CategoryCrewMessageDelivered, ActorKind: &kind, Payload: payload,
	}); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
}

func countEnvelopes(rendered string) (begins, ends int) {
	return strings.Count(rendered, "\n"+crewBegin+"\n"), strings.Count(rendered, "\n"+crewEnd+"\n")
}

// TestCrewDelivery_PlanPromptRendersEnvelopedFinding (done-means): the served
// plan prompt carries the finding addressed to the planner inside exactly one
// balanced crew envelope.
func TestCrewDelivery_PlanPromptRendersEnvelopedFinding(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "DELIV-SENTINEL-finding")

	got := servePrompt(t, s, runID, stageID, priv)
	if b, e := countEnvelopes(got); b != 1 || e != 1 {
		t.Fatalf("crew envelopes = %d begin / %d end, want exactly one balanced pair:\n%s", b, e, got)
	}
	requireInsideCrewEnvelopes(t, got, "DELIV-SENTINEL-finding")
}

// TestCrewDelivery_DeliveredAuditEntryRecorded (done-means): the signed serve
// appends ONE crew_message_delivered entry naming the delivered sequences, the
// reading stage and the recipient role, stamped with the stage id.
func TestCrewDelivery_DeliveredAuditEntryRecorded(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	finding := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "REC-finding")
	notice := seedCrewMail(t, f, runID, crewmessage.TypeNotice, crewmessage.RolePlanner, "REC-notice")

	servePrompt(t, s, runID, stageID, priv)
	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryCrewMessageDelivered)
	if err != nil || len(entries) != 1 {
		t.Fatalf("crew_message_delivered entries = %d (err %v), want 1", len(entries), err)
	}
	if entries[0].StageID == nil || *entries[0].StageID != stageID {
		t.Errorf("delivery stamped with stage %v, want %s", entries[0].StageID, stageID)
	}
	var p crewMessageDeliveredPayload
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Equal(p.SentSequences, []int64{finding, notice}) || p.StageType != "plan" || p.RecipientRole != "planner" || p.Render != "plan" {
		t.Errorf("delivery payload = %+v, want sequences [%d %d] for plan/planner", p, finding, notice)
	}
}

// TestCrewDelivery_EndToEnd_SendThenPlanPrompt is the cross-boundary test for
// this slice: an operator POSTs a finding over HTTP (handler -> mailbox ->
// chain + derived row), the signed plan prompt carries it enveloped (resolver
// -> renderer), the chain records the delivery, and a later DIFFERENT plan
// stage is not handed it again.
func TestCrewDelivery_EndToEnd_SendThenPlanPrompt(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	sent := f.send(t, crewDoc("finding", "", "planner", runAnchor(runID), `{"summary":"E2E-SENTINEL-finding","severity":"high"}`),
		operator("write:stages"))

	requireInsideCrewEnvelopes(t, servePrompt(t, s, runID, stageID, priv), "E2E-SENTINEL-finding")
	if got := deliveredEntries(t, f, runID); len(got) != 1 || !slices.Equal(got[0].SentSequences, []int64{sent.SentSequence}) {
		t.Fatalf("deliveries = %+v, want one naming %d", got, sent.SentSequence)
	}
	later := f.seedStage(t, runID, 2, run.StageTypePlan)
	if got := s.resolveDeliverableCrewMessages(context.Background(), runID, later, run.StageTypePlan); len(got.Messages) != 0 {
		t.Fatalf("a later stage was re-handed %d delivered message(s)", len(got.Messages))
	}
}

// TestCrewDelivery_ImplementPromptCarriesNoCrewMessages (C1): an OPEN finding
// addressed to the planner AND one to the reviewer exist, and neither the
// implement stage's served prompt nor its resolver carries them.
func TestCrewDelivery_ImplementPromptCarriesNoCrewMessages(t *testing.T) {
	f := newCrewPG(t)
	s, sf := newConsultFoldServer(t, f, f.audit)
	runID := f.seedRun(t, run.StageTypeImplement)
	stageID := f.stageOf(t, runID)
	priv, _ := sf.issue(t, runID)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "IMPL-SENTINEL-planner")
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RoleReviewer, "IMPL-SENTINEL-reviewer")

	for _, st := range []run.StageType{run.StageTypeImplement, run.StageTypeAcceptance, run.StageTypeDeploy} {
		if got := s.resolveDeliverableCrewMessages(context.Background(), runID, stageID, st); len(got.Messages) != 0 {
			t.Errorf("%s resolver delivered %d message(s), want 0", st, len(got.Messages))
		}
	}
	for name, p := range map[string]ed25519.PrivateKey{"prompt": priv, "prompt-render": nil} {
		got := servePrompt(t, s, runID, stageID, p)
		for _, leaked := range []string{"IMPL-SENTINEL-planner", "IMPL-SENTINEL-reviewer", crewBegin} {
			if strings.Contains(got, leaked) {
				t.Errorf("%s: implement prompt carries %q", name, leaked)
			}
		}
	}
	if got := deliveredEntries(t, f, runID); len(got) != 0 {
		t.Errorf("implement serve recorded %d deliveries, want 0", len(got))
	}
}

// TestCrewDelivery_RoleGate (C2): a finding to the reviewer is not handed to
// the planner, and vice versa.
func TestCrewDelivery_RoleGate(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RoleReviewer, "ROLE-SENTINEL-reviewer")
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "ROLE-SENTINEL-planner")
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RoleSecurity, "ROLE-SENTINEL-security")

	got := servePrompt(t, s, runID, stageID, priv)
	requireInsideCrewEnvelopes(t, got, "ROLE-SENTINEL-planner")
	for _, leaked := range []string{"ROLE-SENTINEL-reviewer", "ROLE-SENTINEL-security"} {
		if strings.Contains(got, leaked) {
			t.Errorf("plan prompt carries %q, addressed to another role", leaked)
		}
	}
	review := crewMessagesText(s.resolveDeliverableCrewMessages(context.Background(), runID, stageID, run.StageTypeReview).Messages)
	if !strings.Contains(review, "ROLE-SENTINEL-reviewer") || strings.Contains(review, "ROLE-SENTINEL-planner") {
		t.Errorf("review delivery = %q, want only the reviewer's finding", review)
	}
}

// TestCrewDelivery_DisposedFindingNotDelivered (C3): a finding disposed
// accepted, otherwise identical to a delivered one, is not delivered.
func TestCrewDelivery_DisposedFindingNotDelivered(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	disposed := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "STATE-SENTINEL-disposed")
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "STATE-SENTINEL-open")
	if _, err := f.mailbox.Dispose(context.Background(), crewmessage.DisposeParams{
		SentSequence: disposed, Disposition: crewmessage.DispositionAccepted,
		Actor: crewmessage.Actor{Kind: audit.ActorSystem, Subject: "test:captain"},
	}); err != nil {
		t.Fatalf("dispose: %v", err)
	}

	got := servePrompt(t, s, runID, stageID, priv)
	requireInsideCrewEnvelopes(t, got, "STATE-SENTINEL-open")
	if strings.Contains(got, "STATE-SENTINEL-disposed") {
		t.Error("a disposed finding was delivered")
	}
}

// TestCrewDelivery_TypeGate (C4): an escalation and a consult addressed to the
// planner, each otherwise deliverable, are not delivered; nor is a threaded
// REPLY notice (only thread roots are delivered).
func TestCrewDelivery_TypeGate(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	seedCrewMail(t, f, runID, crewmessage.TypeEscalation, crewmessage.RolePlanner, "TYPE-SENTINEL-escalation")
	consult := seedCrewMail(t, f, runID, crewmessage.TypeConsult, crewmessage.RolePlanner, "TYPE-SENTINEL-consult")
	if _, err := f.mailbox.Send(context.Background(), crewmessage.SendParams{
		RawMessage: []byte(`{"schema_version":"crew-message-v1","type":"notice","sender_role":"architect","recipient_role":"planner","anchor":` +
			runAnchor(runID) + `,"payload":{"summary":"TYPE-SENTINEL-reply"}}`),
		Actor:              crewmessage.Actor{Kind: audit.ActorSystem, Subject: "test:architect"},
		ThreadRootSequence: &consult,
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	seedCrewMail(t, f, runID, crewmessage.TypeNotice, crewmessage.RolePlanner, "TYPE-SENTINEL-notice")

	got := servePrompt(t, s, runID, stageID, priv)
	requireInsideCrewEnvelopes(t, got, "TYPE-SENTINEL-notice")
	for _, leaked := range []string{"TYPE-SENTINEL-escalation", "TYPE-SENTINEL-consult", "TYPE-SENTINEL-reply"} {
		if strings.Contains(got, leaked) {
			t.Errorf("plan prompt carries %q, which deferred delivery must not carry", leaked)
		}
	}
}

// TestCrewDelivery_NotRedeliveredToADifferentStage (C5): a delivery already
// recorded against stage A is not handed to stage B.
func TestCrewDelivery_NotRedeliveredToADifferentStage(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageA, _ := newDeliveryPlanRun(t, f, f.audit)
	stageB := f.seedStage(t, runID, 2, run.StageTypePlan)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "ONCE-SENTINEL")
	recordDeliveryOn(t, f, runID, stageA, seq)

	got := servePrompt(t, s, runID, stageB, nil)
	if strings.Contains(got, "ONCE-SENTINEL") {
		t.Fatal("a finding delivered to stage A was re-delivered to stage B")
	}
}

// TestCrewDelivery_RedeliveredOnSameStageRetry is C5's ACCEPT arm: a delivery
// recorded against the SAME stage re-renders on that stage's retry, so a
// filter that delivered nothing at all cannot pass C5.
func TestCrewDelivery_RedeliveredOnSameStageRetry(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageA, priv := newDeliveryPlanRun(t, f, f.audit)
	seq := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "RETRY-DELIV-SENTINEL")
	recordDeliveryOn(t, f, runID, stageA, seq)

	requireInsideCrewEnvelopes(t, servePrompt(t, s, runID, stageA, priv), "RETRY-DELIV-SENTINEL")
}

// TestCrewDelivery_RenderPreviewRecordsNoDelivery (C6): the preview renders the
// same delivery but records nothing; only the signed serve records.
func TestCrewDelivery_RenderPreviewRecordsNoDelivery(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "PREVIEW-SENTINEL")

	for range 2 {
		requireInsideCrewEnvelopes(t, servePrompt(t, s, runID, stageID, nil), "PREVIEW-SENTINEL")
	}
	if n := f.count(t, CategoryCrewMessageDelivered); n != 0 {
		t.Fatalf("preview recorded %d crew_message_delivered entries, want 0", n)
	}
	servePrompt(t, s, runID, stageID, priv)
	if n := f.count(t, CategoryCrewMessageDelivered); n != 1 {
		t.Fatalf("signed serve recorded %d crew_message_delivered entries, want 1", n)
	}
}

// TestCrewDelivery_CapSelectsNewest (C7): of five open findings exactly the
// three NEWEST render, in ascending order.
func TestCrewDelivery_CapSelectsNewest(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	names := []string{"CAP-SENTINEL-1", "CAP-SENTINEL-2", "CAP-SENTINEL-3", "CAP-SENTINEL-4", "CAP-SENTINEL-5"}
	var seqs []int64
	for _, n := range names {
		seqs = append(seqs, seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, n))
	}

	got := servePrompt(t, s, runID, stageID, priv)
	for _, n := range names[2:] {
		requireInsideCrewEnvelopes(t, got, n)
	}
	for _, n := range names[:2] {
		if strings.Contains(got, n) {
			t.Errorf("%s (one of the two oldest) rendered past the %d-message cap", n, maxCrewDeliveriesPerPrompt)
		}
	}
	if i3, i5 := strings.Index(got, "CAP-SENTINEL-3"), strings.Index(got, "CAP-SENTINEL-5"); i3 > i5 {
		t.Errorf("deliveries not ascending: CAP-SENTINEL-3 at %d after CAP-SENTINEL-5 at %d", i3, i5)
	}
	if d := deliveredEntries(t, f, runID); len(d) != 1 || !slices.Equal(d[0].SentSequences, seqs[2:]) {
		t.Errorf("recorded deliveries = %+v, want the three newest %v", d, seqs[2:])
	}
}

// TestCrewDelivery_DeliveriesPrecedeConsultFoldIn: the deliveries are rendered
// AHEAD of the stage's own answered consult, so the renderer's front-dropping
// block cap elides them first.
func TestCrewDelivery_DeliveriesPrecedeConsultFoldIn(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	seedStageConsult(t, s, f, runID, stageID, "ORDER-Q-consult", "ORDER-A-answer")
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "ORDER-SENTINEL-finding")

	got := servePrompt(t, s, runID, stageID, priv)
	fi, qi := strings.Index(got, "ORDER-SENTINEL-finding"), strings.Index(got, "ORDER-Q-consult")
	if fi < 0 || qi < 0 || fi > qi {
		t.Fatalf("finding at %d, consult at %d: the delivery must precede the consult fold-in", fi, qi)
	}
}

// TestCrewDelivery_OpenFindingIsNotAnOpenConcern (C9, an INVARIANT, not a
// counterfactual vehicle): a delivered open finding is a crew_messages row and
// never a concern, so the merge gate's open-concern read stays empty.
func TestCrewDelivery_OpenFindingIsNotAnOpenConcern(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	f.send(t, crewDoc("finding", "", "planner", runAnchor(runID), `{"summary":"INVARIANT-finding","severity":"high"}`),
		operator("write:stages"))
	requireInsideCrewEnvelopes(t, servePrompt(t, s, runID, stageID, priv), "INVARIANT-finding")
	open, err := concern.NewPostgresRepository(f.pool).ListOpenByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list open concerns: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("an open crew finding produced %d open concern row(s); a finding must never block the merge gate", len(open))
	}
}

// deliveryAuditRepo wraps the real audit repository to inject the resolver's
// and recorder's degrade branches.
type deliveryAuditRepo struct {
	audit.Repository
	deliveredListErr error
	appendErr        error
	tamper           map[int64]bool
}

func (a *deliveryAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if a.deliveredListErr != nil && category == CategoryCrewMessageDelivered {
		return nil, a.deliveredListErr
	}
	entries, err := a.Repository.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return nil, err
	}
	out := make([]*audit.Entry, 0, len(entries))
	for _, e := range entries {
		if a.tamper[e.Sequence] {
			c := *e
			c.EntryHash = "tampered-" + c.EntryHash
			e = &c
		}
		out = append(out, e)
	}
	return out, nil
}

func (a *deliveryAuditRepo) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.appendErr != nil && p.Category == CategoryCrewMessageDelivered {
		return nil, a.appendErr
	}
	return a.Repository.AppendChained(ctx, p)
}

// TestCrewDelivery_Unconfigured: no mailbox or no audit repository resolves an
// empty delivery, and recording is a no-op.
func TestCrewDelivery_Unconfigured(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "UNCONF")
	for name, s := range map[string]*Server{
		"no mailbox": New(Config{Addr: "127.0.0.1:0", AuditRepo: f.audit}),
		"no audit":   New(Config{Addr: "127.0.0.1:0", CrewMailbox: f.mailbox}),
	} {
		if got := s.resolveDeliverableCrewMessages(context.Background(), runID, uuid.New(), run.StageTypePlan); len(got.Messages) != 0 || len(got.Sequences) != 0 {
			t.Errorf("%s: delivery = %+v, want empty", name, got)
		}
	}
	New(Config{Addr: "127.0.0.1:0"}).recordCrewMessagesDelivered(context.Background(), runID, uuid.New(), run.StageTypePlan, "plan", []int64{1})
	f.srv.recordCrewMessagesDelivered(context.Background(), runID, f.stageOf(t, runID), run.StageTypePlan, "plan", nil)
	if n := f.count(t, CategoryCrewMessageDelivered); n != 0 {
		t.Errorf("an unconfigured or empty record appended %d entries", n)
	}
}

// TestCrewDelivery_RowListErrorDegrades: a failing derived-row listing (the
// table renamed away in this test's own database) delivers nothing and the
// plan prompt still builds.
func TestCrewDelivery_RowListErrorDegrades(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, f.audit)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "ROWERR-DELIV")
	if got := s.resolveDeliverableCrewMessages(context.Background(), runID, stageID, run.StageTypePlan); len(got.Messages) != 1 {
		t.Fatalf("control: delivered %d, want 1", len(got.Messages))
	}
	if _, err := f.pool.Exec(context.Background(), `ALTER TABLE crew_messages RENAME TO crew_messages_gone`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := s.resolveDeliverableCrewMessages(context.Background(), runID, stageID, run.StageTypePlan); len(got.Messages) != 0 {
		t.Fatalf("row-list-error delivery = %+v, want empty", got)
	}
	if got := servePrompt(t, s, runID, stageID, priv); strings.Contains(got, "ROWERR-DELIV") {
		t.Error("a delivery rendered despite the row-list failure")
	}
}

// TestCrewDelivery_DeliveredListErrorDegrades: when the prior-delivery record
// cannot be read the once-per-other-stage filter cannot be applied, so nothing
// is delivered (never an unfiltered re-delivery).
func TestCrewDelivery_DeliveredListErrorDegrades(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	stageID := f.stageOf(t, runID)
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "DLISTERR")
	s, _ := newConsultFoldServer(t, f, &deliveryAuditRepo{Repository: f.audit, deliveredListErr: errors.New("chain unavailable")})
	if got := s.resolveDeliverableCrewMessages(context.Background(), runID, stageID, run.StageTypePlan); len(got.Messages) != 0 {
		t.Fatalf("delivered-list-error delivery = %+v, want empty", got)
	}
}

// TestCrewDelivery_SkipsUnresolvableDocument: one message whose chain entry
// fails the citation check is skipped (and not recorded) while its siblings
// are delivered.
func TestCrewDelivery_SkipsUnresolvableDocument(t *testing.T) {
	f := newCrewPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	stageID := f.stageOf(t, runID)
	bad := seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "SKIPDOC-bad")
	good := seedCrewMail(t, f, runID, crewmessage.TypeNotice, crewmessage.RolePlanner, "SKIPDOC-good")
	s, _ := newConsultFoldServer(t, f, &deliveryAuditRepo{Repository: f.audit, tamper: map[int64]bool{bad: true}})

	got := s.resolveDeliverableCrewMessages(context.Background(), runID, stageID, run.StageTypePlan)
	text := crewMessagesText(got.Messages)
	if !strings.Contains(text, "SKIPDOC-good") || strings.Contains(text, "SKIPDOC-bad") {
		t.Fatalf("delivery = %q, want only the resolvable sibling", text)
	}
	if !slices.Equal(got.Sequences, []int64{good}) {
		t.Errorf("sequences = %v, want [%d] (the skipped message must not be recorded)", got.Sequences, good)
	}
}

// TestCrewDelivery_RecordFailureOffersAgain: a failed crew_message_delivered
// append still serves the prompt, and — nothing recorded — the message is
// offered again to the NEXT stage (the safe direction).
func TestCrewDelivery_RecordFailureOffersAgain(t *testing.T) {
	f := newCrewPG(t)
	s, runID, stageID, priv := newDeliveryPlanRun(t, f, &deliveryAuditRepo{Repository: f.audit, appendErr: errors.New("append refused")})
	seedCrewMail(t, f, runID, crewmessage.TypeFinding, crewmessage.RolePlanner, "RECFAIL-SENTINEL")

	requireInsideCrewEnvelopes(t, servePrompt(t, s, runID, stageID, priv), "RECFAIL-SENTINEL")
	if n := f.count(t, CategoryCrewMessageDelivered); n != 0 {
		t.Fatalf("recorded %d deliveries through a failing append", n)
	}
	next := f.seedStage(t, runID, 2, run.StageTypePlan)
	if got := crewMessagesText(s.resolveDeliverableCrewMessages(context.Background(), runID, next, run.StageTypePlan).Messages); !strings.Contains(got, "RECFAIL-SENTINEL") {
		t.Fatalf("the unrecorded delivery was not offered to the next stage: %q", got)
	}
}

// crewRoutedAudit composes a review-path test's audit fake with a real
// pgtest-backed audit repository: every crew_* category (the mailbox's chain
// entries and crew_message_delivered) reads and appends through crew, and
// everything else through the fake the review harness asserts on. This is
// what lets the fake-repo review harnesses exercise the pg-only mailbox.
type crewRoutedAudit struct {
	*auditFake
	crew audit.Repository
}

func (a *crewRoutedAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if strings.HasPrefix(category, "crew_") {
		return a.crew.ListForRunByCategory(ctx, runID, category)
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

func (a *crewRoutedAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if strings.HasPrefix(p.Category, "crew_") {
		return a.crew.AppendChained(ctx, p)
	}
	return a.auditFake.AppendChained(ctx, p)
}

// wireCrewIntoReviewServer points s at f's mailbox through a crewRoutedAudit
// over au, seeding a pg run (and stage) row with runID/stageID so the
// mailbox's and the chain's foreign keys hold.
func wireCrewIntoReviewServer(t *testing.T, s *Server, f *crewPG, au *auditFake, runID, stageID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		VALUES ($1, 'kuhlman-labs/fishhawk', 'feature_change', 'sha', 'cli', 'running', 'local')`, runID); err != nil {
		t.Fatalf("seed pg run: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 1, 'plan', 'agent', 'claude-code', 'running')`, stageID, runID); err != nil {
		t.Fatalf("seed pg stage: %v", err)
	}
	s.cfg.CrewMailbox = f.mailbox
	s.cfg.AuditRepo = &crewRoutedAudit{auditFake: au, crew: f.audit}
}
