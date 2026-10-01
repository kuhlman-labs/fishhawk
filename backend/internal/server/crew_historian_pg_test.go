package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// The CROSS-BOUNDARY end-to-end suite for the historian (E77.8 / #3742): a
// REAL consult POSTed over HTTP, answered by the REAL responder reading REAL
// decision_index rows, rendered through the REAL crew-message envelope, and
// then read back on the REAL gate view. Per-layer units cannot show the
// sentinel is absent from what a planner would SEE, nor that the gate view
// reports the same cited entry ids.

// historianPG extends the crew fixture with the decision index and the
// concern repository the gate view needs.
type historianPG struct {
	*crewPG
	index *decisionindex.Store
}

func newHistorianPG(t *testing.T) *historianPG {
	t.Helper()
	f := newCrewPG(t)
	idx := decisionindex.NewStore(f.pool)
	f.srv.cfg.ConcernRepo = concern.NewPostgresRepository(f.pool)
	f.srv.cfg.PrecedentIndex = idx
	h, err := NewHistorianResponder(idx)
	if err != nil {
		t.Fatalf("NewHistorianResponder: %v", err)
	}
	f.withResponders(t, map[crewmessage.Role]CrewResponder{crewmessage.RoleHistorian: h})
	return &historianPG{crewPG: f, index: idx}
}

// seedDecision seeds one prior decision: a chain entry carrying the REASON
// PROSE (the sentinel), plus the decision_index row that CITES it by sequence.
// The split is the point — the prose lives on the chain, and the historian
// holds no audit repository with which to read it.
func (f *historianPG) seedDecision(t *testing.T, acct *uuid.UUID, class decisionindex.DecisionClass,
	outcome string, paths []string,
) decisionindex.Row {
	t.Helper()
	ctx := context.Background()
	priorRun := f.seedRun(t, run.StageTypePlan)
	if acct != nil {
		if _, err := f.pool.Exec(ctx, `UPDATE runs SET account_id = $2 WHERE id = $1`, priorRun, *acct); err != nil {
			t.Fatalf("tenant the prior run: %v", err)
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"decision": outcome,
		"reason":   historianSentinel + " — the operator's full prose, which must never reach a planner.",
	})
	entry, err := f.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID: priorRun, Timestamp: time.Now().UTC(), Category: "approval_submitted", Payload: payload,
	})
	if err != nil {
		t.Fatalf("seed chain entry: %v", err)
	}
	row := decisionindex.Row{
		SourceSequence:  entry.Sequence,
		SourceEntryHash: entry.EntryHash,
		RunID:           priorRun,
		AccountID:       acct,
		Repo:            "kuhlman-labs/fishhawk",
		WorkflowID:      "feature_change",
		DoctrineVersion: "sha",
		DecisionClass:   class,
		StageKind:       "plan",
		Outcome:         outcome,
		TouchedPaths:    paths,
		EscalationKeys:  []string{},
		ActorKind:       "user",
		ActorSubject:    "github:op",
		DecidedAt:       time.Now().UTC(),
		ReasonSequence:  entry.Sequence,
		// The index NEVER copies the prose (ADR-082 rule 1) — only the key it
		// lives under. Planting the sentinel here too proves the responder's
		// projection drops even that.
		ReasonKey: historianSentinel,
	}
	if err := f.index.Upsert(ctx, row); err != nil {
		t.Fatalf("seed decision index row: %v", err)
	}
	return row
}

func (f *historianPG) gateView(t *testing.T, runID uuid.UUID, query string) gateViewResponse {
	t.Helper()
	return decodeGateView(t, callGateView(f.srv, runID, query, gateViewReadIdentity()))
}

const historianConsultQuestion = "Has a plan touching backend/internal/server/crew_historian.go already been decided? " +
	"See also backend/internal/precedent/precedent.go."

func historianConsultPayload() string {
	b, _ := json.Marshal(map[string]string{
		"question":         historianConsultQuestion,
		"what_i_can_infer": "the package exists and has prior review history",
		"context":          "paths: backend/internal/server/crew_historian.go",
	})
	return string(b)
}

func TestCrewConsult_HistorianEndToEnd_Pg(t *testing.T) {
	f := newHistorianPG(t)
	paths := []string{"backend/internal/server/crew_historian.go", "backend/internal/server/crew_consult.go"}
	reject := f.seedDecision(t, nil, decisionindex.ClassPlanApproval, "reject", paths)
	waive := f.seedDecision(t, nil, decisionindex.ClassConcernWaive, "waived", paths)

	runA := f.seedRun(t, run.StageTypePlan)
	stageA := f.stageOf(t, runA)
	tok := runBound(runA, "mcp:read", scopeWriteMessages)

	// THE FACT THE WHOLE PATH DESIGN RESTS ON, asserted rather than assumed
	// (binding approval condition 2): mid-plan-stage there is NO plan
	// artifact, so decisionindex.GateContext derives an EMPTY touched-path
	// list and the prose-path union is load-bearing, not decorative.
	gc, err := f.index.GateContext(context.Background(), decisionindex.GateRef{RunID: runA, StageID: &stageA})
	if err != nil {
		t.Fatalf("GateContext: %v", err)
	}
	if len(gc.TouchedPaths) != 0 {
		t.Fatalf("plan-derived TouchedPaths = %v at consult time, want EMPTY — the prose-path union would be "+
			"redundant and the risks note wrong", gc.TouchedPaths)
	}

	sent := f.send(t, consultDoc("historian", runAnchor(runA), historianConsultPayload()), tok)
	done, _ := f.waitAsync(t, sent.SentSequence, 10, tok)
	got := decodeCrewGet(t, <-done)
	if !got.Answered || got.Answer == nil {
		t.Fatalf("answered = %v, answer = %+v", got.Answered, got.Answer)
	}
	out := got.Answer.Rendered

	// Each prior decision is cited by sequence AND entry hash prefix, with its
	// matched path prefixes.
	for _, row := range []decisionindex.Row{reject, waive} {
		cite := fmt.Sprintf("e=%d@%s", row.SourceSequence, row.SourceEntryHash[:historianHashBytes])
		if !strings.Contains(out, cite) {
			t.Fatalf("citation %q missing from the rendered answer:\n%s", cite, out)
		}
	}
	if !strings.Contains(out, "p=backend/internal") {
		t.Fatalf("no matched path prefix reached the answer:\n%s", out)
	}
	// AND THE SENTINEL IS NOWHERE — not in the envelope, not in the whole body.
	if strings.Contains(out, historianSentinel) {
		t.Fatalf("another run's reason prose reached the planner:\n%s", out)
	}

	// The gate view reports the consult with the SAME cited entry ids.
	gv := f.gateView(t, runA, "stage_kind=plan")
	if len(gv.Consults) != 1 {
		t.Fatalf("gate-view consults = %+v, want exactly the one consult", gv.Consults)
	}
	c := gv.Consults[0]
	if c.SentSequence != sent.SentSequence || !c.Answered || c.State != string(crewmessage.StateAccepted) {
		t.Fatalf("consult block = %+v", c)
	}
	if c.RecipientRole != string(crewmessage.RoleHistorian) || c.SenderRole != string(crewmessage.RolePlanner) {
		t.Fatalf("consult roles = %s -> %s", c.SenderRole, c.RecipientRole)
	}
	if c.StageKind != string(run.StageTypePlan) || c.StageID != stageA.String() {
		t.Fatalf("consult stage = %s/%s, want the plan stage %s", c.StageKind, c.StageID, stageA)
	}
	for _, row := range []decisionindex.Row{reject, waive} {
		want := "audit_entry:" + fmt.Sprint(row.SourceSequence)
		found := false
		for _, ref := range c.CitedEntryRefs {
			if ref == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("cited_entry_refs = %v, missing %q", c.CitedEntryRefs, want)
		}
	}
	if strings.Contains(c.AnswerSummary, historianSentinel) || strings.Contains(c.Question, historianSentinel) {
		t.Fatalf("the sentinel reached the gate view: %+v", c)
	}
	if gv.HistoryIncomplete {
		t.Fatalf("history_incomplete on a healthy read; gaps = %v", gv.HistoryGaps)
	}

	// (C10) The stage_kind filter matches the CONSULT's own stage: this plan
	// consult is absent from an implement-scoped view, and present in the
	// unfiltered one.
	if got := f.gateView(t, runA, "stage_kind=implement").Consults; len(got) != 0 {
		t.Fatalf("implement-scoped gate view returned the PLAN consult: %+v", got)
	}
	if got := f.gateView(t, runA, "").Consults; len(got) != 1 {
		t.Fatalf("unfiltered gate view consults = %+v, want the one consult", got)
	}
}

// (C2, pg sibling) A row belonging to ANOTHER account is absent from the
// answer. The consult here carries NO account, so ListFilter.AccountScoped is
// what confines it to the untenanted rows; without that flag a nil account
// matches every account's rows and the foreign row appears.
func TestHistorian_ForeignAccountRowsAbsent_Pg(t *testing.T) {
	f := newHistorianPG(t)
	paths := []string{"backend/internal/server/crew_historian.go"}
	foreignAcct := uuid.New()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`,
		foreignAcct, "other-"+foreignAcct.String()[:8]); err != nil {
		t.Fatalf("seed foreign account: %v", err)
	}
	ours := f.seedDecision(t, nil, decisionindex.ClassPlanApproval, "reject", paths)
	theirs := f.seedDecision(t, &foreignAcct, decisionindex.ClassPlanApproval, "approve", paths)

	runA := f.seedRun(t, run.StageTypePlan)
	tok := runBound(runA, "mcp:read", scopeWriteMessages)
	sent := f.send(t, consultDoc("historian", runAnchor(runA), historianConsultPayload()), tok)
	done, _ := f.waitAsync(t, sent.SentSequence, 10, tok)
	got := decodeCrewGet(t, <-done)
	if !got.Answered || got.Answer == nil {
		t.Fatalf("answered = %v", got.Answered)
	}
	out := got.Answer.Rendered
	if !strings.Contains(out, fmt.Sprintf("e=%d@", ours.SourceSequence)) {
		t.Fatalf("our own untenanted row is missing:\n%s", out)
	}
	if strings.Contains(out, fmt.Sprintf("e=%d@", theirs.SourceSequence)) {
		t.Fatalf("another account's decision reached the answer:\n%s", out)
	}
}

// (C6, pg sibling) An unresolvable run EXPIRES the consult with a reason
// naming the historian — it is never answered with a fabricated no-precedent.
func TestCrewConsult_HistorianRunMissingExpires_Pg(t *testing.T) {
	f := newHistorianPG(t)
	runA := f.seedRun(t, run.StageTypePlan)
	tok := runBound(runA, "mcp:read", scopeWriteMessages)
	// A consult sent under an account the RUN does not belong to: GateContext
	// narrows the resolve by account, so the run reads as missing. BOTH sides
	// must be tenanted — an UNTENANTED run matches every account by design
	// (the #1830 single-tenant window), so tenanting only the caller would
	// resolve fine and this arm would prove nothing.
	acct, runAcct := uuid.New(), uuid.New()
	for _, a := range []uuid.UUID{acct, runAcct} {
		if _, err := f.pool.Exec(context.Background(),
			`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, a, "acc-"+a.String()[:8]); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE runs SET account_id = $2 WHERE id = $1`, runA, runAcct); err != nil {
		t.Fatalf("tenant the run: %v", err)
	}
	withAcct := func(r *http.Request) *http.Request {
		r = tok(r)
		id := IdentityFrom(r.Context())
		id.AccountID = acct.String()
		return injectIdentity(r, id)
	}
	sent := f.send(t, consultDoc("historian", runAnchor(runA), historianConsultPayload()), withAcct)
	row := f.awaitState(t, sent.SentSequence, timescale.D(10*time.Second))
	if row.State != crewmessage.StateExpired {
		t.Fatalf("consult state = %q, want expired", row.State)
	}
	disp, reason := f.disposedReason(t, sent.SentSequence)
	if disp != string(crewmessage.DispositionExpired) {
		t.Fatalf("disposition = %q, want expired", disp)
	}
	if !strings.Contains(reason, "historian") {
		t.Fatalf("expiry reason = %q, want it to name the historian", reason)
	}
}
