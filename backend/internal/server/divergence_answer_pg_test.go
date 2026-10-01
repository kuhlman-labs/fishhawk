package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// divergence_answer_pg_test.go is the E75.5 / #3733 ask/answer half's
// cross-boundary walk (the #618 rule): with the feature ENABLED, a contrary
// plan reject through the REAL approvals handler records a precedent_divergence
// entry on the REAL chain (gate_divergence_pg_test.go stops there); this file
// continues — GET /v0/runs/{id} and the gate view surface the question keyed
// by that entry's REAL chain sequence, the answer endpoint files a
// doctrine_change item and appends precedent_divergence_answered citing it,
// and the next GET no longer carries the question. The ranked set crosses the
// decision index, the chain and two HTTP surfaces, which no per-layer fake can
// stand in for.

func TestDivergenceAnswerPG_EndToEndDoctrineChange(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	gh, forge := newDAForge(t)

	f := newGPPG(t)
	dvPGSeedApproves(t, f, 4)
	runID, planStage := dvPGGateRun(t, f, "sha-gp")
	s := New(Config{Addr: "127.0.0.1:0",
		RunRepo:          run.NewPostgresRepository(f.pool),
		ConcernRepo:      concern.NewPostgresRepository(f.pool),
		ApprovalRepo:     approval.NewPostgresRepository(f.pool),
		AuditRepo:        f.audit,
		PrecedentIndex:   decisionindex.NewStore(f.pool),
		IdentityProvider: &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true},
		GitHub:           gh,
		DivergenceConfig: dvPGConfig(),
	})

	dvPGReject(t, s, planStage)

	// The detection half landed exactly one entry; its REAL sequence is the
	// question's key.
	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryPrecedentDivergence)
	if err != nil || len(entries) != 1 {
		t.Fatalf("precedent_divergence entries = %d (%v), want 1", len(entries), err)
	}
	seq := entries[0].Sequence

	get := func(handler http.HandlerFunc, path string) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetPathValue("run_id", runID.String())
		w := httptest.NewRecorder()
		handler(w, withAuth(req))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d:\n%s", path, w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}
	runPath := "/v0/runs/" + runID.String()

	var q divergenceQuestion
	if err := json.Unmarshal(get(s.handleGetRun, runPath)["divergence"], &q); err != nil {
		t.Fatalf("GET run carried no decodable divergence block: %v", err)
	}
	if q.Sequence != seq || q.DecisionClass != "plan_approval" || q.Outcome != "reject" ||
		q.ModalOutcome != "approve" || q.HumanCount != 4 || q.StageID != planStage.String() || len(q.CitedSequences) == 0 {
		t.Fatalf("question = %+v, want seq %d plan_approval reject vs approve", q, seq)
	}
	if gv := get(s.handleGetRunGateView, runPath+"/gate-view")["divergence"]; len(gv) == 0 {
		t.Fatal("gate view carried no divergence block")
	}

	// Answer through the REAL route shape the question pointed at.
	body := `{"answer":"doctrine_change","note":"reject generated-file edits","parent_epic":"#389","n":"5"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/divergence/"+strconv.FormatInt(seq, 10)+"/answer", strings.NewReader(body))
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("sequence", strconv.FormatInt(seq, 10))
	w := httptest.NewRecorder()
	s.handleAnswerDivergence(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("answer = %d:\n%s", w.Code, w.Body.String())
	}
	if !fp.called || !containsStr2(fp.captured.Item.Classification.Labels, "autonomy:low") {
		t.Fatalf("filed = %v labels = %v, want one autonomy:low item", fp.called, fp.captured.Item.Classification.Labels)
	}
	if wr := forge.writes(); len(wr) != 0 {
		t.Fatalf("forge writes = %v, want none", wr)
	}

	answered, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryPrecedentDivergenceAnswered)
	if err != nil || len(answered) != 1 {
		t.Fatalf("answered entries = %d (%v), want 1", len(answered), err)
	}
	var ap precedentDivergenceAnsweredPayload
	if err := json.Unmarshal(answered[0].Payload, &ap); err != nil {
		t.Fatal(err)
	}
	if ap.DivergenceSequence != seq || ap.Answer != "doctrine_change" || ap.IssueNumber != 4242 ||
		answered[0].StageID == nil || *answered[0].StageID != planStage || answered[0].Sequence <= seq {
		t.Fatalf("answer entry = %+v (stage %v seq %d)", ap, answered[0].StageID, answered[0].Sequence)
	}

	if _, has := get(s.handleGetRun, runPath)["divergence"]; has {
		t.Fatal("the answered question still surfaces on GET /v0/runs/{id}")
	}
	if _, has := get(s.handleGetRunGateView, runPath+"/gate-view")["divergence"]; has {
		t.Fatal("the answered question still surfaces on the gate view")
	}
}
