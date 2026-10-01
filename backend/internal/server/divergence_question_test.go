package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// divergence_question_test.go pins the E75.5 / #3733 question surface:
// openDivergenceFor's selection (newest UNANSWERED entry), its rule-6
// run-bound guard, its shipped-default inertness, and each fail-quiet read
// mode — plus the two call sites (GET /v0/runs/{id} and the gate view).

func dqEnabledConfig() *precedent.DivergenceConfig {
	return &precedent.DivergenceConfig{Enabled: true, MinDecisions: 3, MinAgreement: 0.8, Window: 30 * 24 * time.Hour}
}

// dqSeedDivergence seeds a precedent_divergence entry BY CONSTRUCTION (not
// through the detection hook), at an explicit chain sequence.
func dqSeedDivergence(au *auditFake, runID, stageID uuid.UUID, seq int64, class, outcome string) {
	p := precedentDivergencePayload{
		DecisionClass:    class,
		StageID:          stageID.String(),
		StageKind:        "plan",
		DecisionSequence: seq - 1,
		Outcome:          outcome,
		ModalOutcome:     "approve",
		AgreementRatio:   1,
		HumanCount:       4,
		Threshold:        precedentDivergenceThreshold{MinDecisions: 3, MinAgreement: 0.8, WindowSeconds: 2592000, DoctrineVersion: "sha-1"},
		IndexVersion:     precedent.IndexVersion,
		Cited: []precedentDivergenceCitation{
			{SourceSequence: 101, SourceEntryHash: "h101", DecisionClass: class, Outcome: "approve"},
			{SourceSequence: 102, SourceEntryHash: "h102", DecisionClass: class, Outcome: "approve"},
		},
		CitedTotal: 4,
	}
	raw, _ := json.Marshal(p)
	rid, sid := runID, stageID
	au.seeded = append(au.seeded, &audit.Entry{
		ID: uuid.New(), Sequence: seq, RunID: &rid, StageID: &sid,
		Category: CategoryPrecedentDivergence, Payload: raw,
		EntryHash: fmt.Sprintf("dv-hash-%d", seq), Timestamp: time.Now().UTC(),
	})
}

// dqSeedAnswer seeds a precedent_divergence_answered entry citing seq.
func dqSeedAnswer(au *auditFake, runID uuid.UUID, seq int64) {
	raw, _ := json.Marshal(precedentDivergenceAnsweredPayload{DivergenceSequence: seq, Answer: divergenceAnswerOneOff})
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		ID: uuid.New(), Sequence: seq + 1000, RunID: &rid,
		Category: CategoryPrecedentDivergenceAnswered, Payload: raw,
	})
}

func dqServer(cfg *precedent.DivergenceConfig) (*Server, *auditFake, *run.Run) {
	au := newAuditFake()
	ru := &run.Run{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk", State: run.StateRunning}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, DivergenceConfig: cfg})
	return s, au, ru
}

func dqOperatorCtx() context.Context {
	return context.WithValue(context.Background(), ctxKeyIdentity, testOperatorIdentity())
}

// TestDivergenceQuestion_SurfacesNewestUnanswered: of three entries, the
// answered one is skipped and the NEWEST unanswered one is rendered, with the
// answer pointer, the closed option set and citations by sequence only.
func TestDivergenceQuestion_SurfacesNewestUnanswered(t *testing.T) {
	s, au, ru := dqServer(dqEnabledConfig())
	stage := uuid.New()
	dqSeedDivergence(au, ru.ID, stage, 10, "plan_approval", "reject")
	dqSeedDivergence(au, ru.ID, stage, 30, "concern_waive", "waived")
	dqSeedDivergence(au, ru.ID, stage, 20, "concern_defer", "deferred")
	dqSeedAnswer(au, ru.ID, 30)

	q := s.openDivergenceFor(dqOperatorCtx(), ru)
	if q == nil {
		t.Fatal("question = nil, want the newest unanswered (seq 20)")
	}
	if q.Sequence != 20 || q.DecisionClass != "concern_defer" || q.Outcome != "deferred" {
		t.Fatalf("question = %+v, want seq 20 concern_defer/deferred (seq 30 is answered)", q)
	}
	if q.OpenTotal != 2 {
		t.Errorf("open_total = %d, want 2", q.OpenTotal)
	}
	if want := "POST /v0/runs/" + ru.ID.String() + "/divergence/20/answer"; q.Answer.Endpoint != want || q.Answer.Tool != divergenceAnswerTool {
		t.Errorf("answer pointer = %+v, want %q / %s", q.Answer, want, divergenceAnswerTool)
	}
	if len(q.Options) != 2 || q.Options[0] != divergenceAnswerOneOff || q.Options[1] != divergenceAnswerDoctrineChange {
		t.Errorf("options = %v", q.Options)
	}
	if len(q.CitedSequences) != 2 || q.CitedSequences[0] != 101 || q.CitedTotal != 4 {
		t.Errorf("citations = %v / %d", q.CitedSequences, q.CitedTotal)
	}
	if q.ModalOutcome != "approve" || q.AgreementRatio != 1 || q.HumanCount != 4 || q.Question == "" {
		t.Errorf("question = %+v", q)
	}
}

// TestDivergenceQuestion_AnsweredIsNotSurfaced: the only entry is answered, so
// no question renders. Deleting the answered-skip makes it render.
func TestDivergenceQuestion_AnsweredIsNotSurfaced(t *testing.T) {
	s, au, ru := dqServer(dqEnabledConfig())
	dqSeedDivergence(au, ru.ID, uuid.New(), 10, "plan_approval", "reject")
	dqSeedAnswer(au, ru.ID, 10)
	if q := s.openDivergenceFor(dqOperatorCtx(), ru); q != nil {
		t.Fatalf("question = %+v, want nil: seq 10 is answered", q)
	}
}

// TestDivergenceQuestion_ShippedDefaultNoQuestionOnEveryAllowedClass is the
// approval-condition-3 done-means test for the question half: under a nil
// DivergenceConfig (the shipped default) and an explicit Enabled:false one,
// a chain that DOES carry an unanswered precedent_divergence entry for each
// allow-listed class (concern_waive, concern_defer, plan_approval reject)
// surfaces NO question on the helper, the gate view or GET /v0/runs/{id}, and
// the reads append no precedent_divergence entry. The enabled arm is the
// discriminator — same chain, question present.
func TestDivergenceQuestion_ShippedDefaultNoQuestionOnEveryAllowedClass(t *testing.T) {
	classes := []struct{ class, outcome string }{
		{"concern_waive", "waived"},
		{"concern_defer", "deferred"},
		{"plan_approval", "reject"},
	}
	for _, tc := range []struct {
		name string
		cfg  *precedent.DivergenceConfig
		want bool
	}{
		{"nil_shipped_default", nil, false},
		{"explicitly_disabled", &precedent.DivergenceConfig{MinDecisions: 3, MinAgreement: 0.8, Window: time.Hour}, false},
		{"enabled_discriminator", dqEnabledConfig(), true},
	} {
		for _, c := range classes {
			t.Run(tc.name+"/"+c.class, func(t *testing.T) {
				h := newDQHTTPHarness(tc.cfg)
				dqSeedDivergence(h.audit, h.runID, uuid.New(), 50, c.class, c.outcome)

				if got := h.s.openDivergenceFor(dqOperatorCtx(), &run.Run{ID: h.runID}) != nil; got != tc.want {
					t.Fatalf("helper question present = %v, want %v", got, tc.want)
				}
				for _, body := range []map[string]json.RawMessage{h.getRun(t, withAuth), h.gateView(t, withAuth)} {
					_, has := body["divergence"]
					if has != tc.want {
						t.Fatalf("response divergence key present = %v, want %v: %v", has, tc.want, keys(body))
					}
				}
				for _, ap := range h.audit.appended {
					if ap.Category == CategoryPrecedentDivergence || ap.Category == CategoryPrecedentDivergenceAnswered {
						t.Fatalf("a read appended %s", ap.Category)
					}
				}
			})
		}
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDivergenceQuestion_RunBoundIdentityGetsNoBlock pins ADR-082 rule 6 at
// the API surface: the run's OWN run-bound token reads its own run and gets no
// question (on the helper and on both HTTP surfaces), while the operator arm
// on the same chain does.
func TestDivergenceQuestion_RunBoundIdentityGetsNoBlock(t *testing.T) {
	h := newDQHTTPHarness(dqEnabledConfig())
	dqSeedDivergence(h.audit, h.runID, uuid.New(), 50, "plan_approval", "reject")
	runBound := func(r *http.Request) *http.Request {
		return withIdentity(r, Identity{Subject: "mcp:run:" + h.runID.String()})
	}
	ctx := context.WithValue(context.Background(), ctxKeyIdentity, Identity{Subject: "mcp:run:" + h.runID.String()})
	if q := h.s.openDivergenceFor(ctx, &run.Run{ID: h.runID}); q != nil {
		t.Fatalf("run-bound helper question = %+v, want nil", q)
	}
	for name, body := range map[string]map[string]json.RawMessage{
		"get_run": h.getRun(t, runBound), "gate_view": h.gateView(t, runBound),
	} {
		if _, has := body["divergence"]; has {
			t.Fatalf("%s: run-bound caller received the divergence block", name)
		}
	}
	if h.s.openDivergenceFor(dqOperatorCtx(), &run.Run{ID: h.runID}) == nil {
		t.Fatal("operator arm: question = nil, want present (the discriminator)")
	}
}

// TestDivergenceQuestion_FailQuietModes: one case per named fail-quiet branch.
// The answered-read case seeds an ANSWERED entry so that, were the read error
// ignored, the answered question would wrongly reappear as open.
func TestDivergenceQuestion_FailQuietModes(t *testing.T) {
	t.Run("nil_run", func(t *testing.T) {
		s, _, _ := dqServer(dqEnabledConfig())
		if q := s.openDivergenceFor(dqOperatorCtx(), nil); q != nil {
			t.Fatalf("q = %+v", q)
		}
	})
	t.Run("nil_audit_repo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", DivergenceConfig: dqEnabledConfig()})
		if q := s.openDivergenceFor(dqOperatorCtx(), &run.Run{ID: uuid.New()}); q != nil {
			t.Fatalf("q = %+v", q)
		}
	})
	t.Run("no_entries", func(t *testing.T) {
		s, _, ru := dqServer(dqEnabledConfig())
		if q := s.openDivergenceFor(dqOperatorCtx(), ru); q != nil {
			t.Fatalf("q = %+v", q)
		}
	})
	t.Run("divergence_read_fails", func(t *testing.T) {
		s, au, ru := dqServer(dqEnabledConfig())
		dqSeedDivergence(au, ru.ID, uuid.New(), 10, "plan_approval", "reject")
		au.listByCategoryErrCategory = CategoryPrecedentDivergence
		if q := s.openDivergenceFor(dqOperatorCtx(), ru); q != nil {
			t.Fatalf("q = %+v", q)
		}
	})
	t.Run("answered_read_fails", func(t *testing.T) {
		s, au, ru := dqServer(dqEnabledConfig())
		dqSeedDivergence(au, ru.ID, uuid.New(), 10, "plan_approval", "reject")
		dqSeedAnswer(au, ru.ID, 10)
		au.listByCategoryErrCategory = CategoryPrecedentDivergenceAnswered
		if q := s.openDivergenceFor(dqOperatorCtx(), ru); q != nil {
			t.Fatalf("q = %+v: a failed answered-read must not resurface an answered question", q)
		}
	})
	t.Run("malformed_payload_skipped", func(t *testing.T) {
		s, au, ru := dqServer(dqEnabledConfig())
		dqSeedDivergence(au, ru.ID, uuid.New(), 10, "plan_approval", "reject")
		rid := ru.ID
		au.seeded = append(au.seeded, &audit.Entry{Sequence: 40, RunID: &rid, Category: CategoryPrecedentDivergence, Payload: json.RawMessage(`{not json`)})
		q := s.openDivergenceFor(dqOperatorCtx(), ru)
		if q == nil || q.Sequence != 10 || q.OpenTotal != 1 {
			t.Fatalf("q = %+v, want seq 10 with open_total 1 (seq 40 undecodable)", q)
		}
	})
}

// dqHTTPHarness drives the two carrying handlers against fakes.
type dqHTTPHarness struct {
	s     *Server
	audit *auditFake
	runID uuid.UUID
}

func newDQHTTPHarness(cfg *precedent.DivergenceConfig) *dqHTTPHarness {
	repo := newApprovalRunRepo()
	au := newAuditFake()
	runID := uuid.New()
	repo.seedRun(&run.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au,
		ConcernRepo: newFakeConcernRepo(), DivergenceConfig: cfg})
	return &dqHTTPHarness{s: s, audit: au, runID: runID}
}

func (h *dqHTTPHarness) serve(t *testing.T, path string, handler http.HandlerFunc, auth func(*http.Request) *http.Request) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("run_id", h.runID.String())
	w := httptest.NewRecorder()
	handler(w, auth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", path, w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func (h *dqHTTPHarness) getRun(t *testing.T, auth func(*http.Request) *http.Request) map[string]json.RawMessage {
	return h.serve(t, "/v0/runs/"+h.runID.String(), h.s.handleGetRun, auth)
}

func (h *dqHTTPHarness) gateView(t *testing.T, auth func(*http.Request) *http.Request) map[string]json.RawMessage {
	return h.serve(t, "/v0/runs/"+h.runID.String()+"/gate-view", h.s.handleGetRunGateView, auth)
}

// TestDivergenceQuestion_BothSurfacesCarryTheSameBlock: GET /v0/runs/{id} and
// the gate view carry byte-identical divergence blocks (one helper).
func TestDivergenceQuestion_BothSurfacesCarryTheSameBlock(t *testing.T) {
	h := newDQHTTPHarness(dqEnabledConfig())
	dqSeedDivergence(h.audit, h.runID, uuid.New(), 50, "plan_approval", "reject")
	a, b := h.getRun(t, withAuth)["divergence"], h.gateView(t, withAuth)["divergence"]
	if len(a) == 0 || string(a) != string(b) {
		t.Fatalf("get_run divergence = %s\ngate_view divergence = %s", a, b)
	}
	if !strings.Contains(string(a), `"sequence":50`) {
		t.Errorf("block = %s, want sequence 50", a)
	}
}
