package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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

// daPGPostAnswer POSTs a doctrine_change answer for (runID, seq) to s.
func daPGPostAnswer(t *testing.T, s *Server, runID uuid.UUID, seq int64) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"answer":"doctrine_change","parent_epic":"#389","n":"5"}`
	path := "/v0/runs/" + runID.String() + "/divergence/" + strconv.FormatInt(seq, 10) + "/answer"
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("sequence", strconv.FormatInt(seq, 10))
	w := httptest.NewRecorder()
	s.handleAnswerDivergence(w, withAuth(req))
	return w
}

// TestDivergenceAnswerPG_ConcurrentDoctrineChangeFilesOnce (#3891): two
// SERVERS (two replicas, each with its own connection pool and the REAL
// run.NewPostgresRepository, so each probes the production
// DivergenceAnswerLocker) answer ONE real divergence question with
// doctrine_change concurrently. Exactly one work item is filed, exactly one
// precedent_divergence_answered entry lands on the REAL chain, and the
// duplicate gets 409 divergence_already_answered.
//
// The overlap is FORCED and PROVEN, never timed (the
// TestFileRefinementSession_ConcurrentFilesOnce tracer pattern): the winner's
// answer is pinned inside its first File, holding the answer lock; the loser's
// answer is then launched, and the barrier waits until the loser's pool has
// COMPLETED at least two pg_try_advisory_lock queries. pglock re-tries only a
// refused lock, so a second try proves the loser reached the lock, was refused
// while the winner held it, and is polling. Only then is the winner released.
// The barrier's bound FAILS the test; it never releases the winner. With the
// server's lock call removed the loser completes no try and files a second
// item inside the winner's section, RED.
func TestDivergenceAnswerPG_ConcurrentDoctrineChangeFilesOnce(t *testing.T) {
	bp := newDABlockingProvider()
	gh, _ := newDAForge(t)

	dbURL := pgtest.NewURL(t)
	pool := newFilingTestPool(t, dbURL, nil)
	loserTries := &filingTryLockTracer{}
	loserPool := newFilingTestPool(t, dbURL, loserTries)
	f := &gpPG{pool: pool, audit: audit.NewPostgresRepository(pool)}
	dvPGSeedApproves(t, f, 4)
	runID, planStage := dvPGGateRun(t, f, "sha-gp")
	winner := New(Config{Addr: "127.0.0.1:0",
		RunRepo:          run.NewPostgresRepository(pool),
		ConcernRepo:      concern.NewPostgresRepository(pool),
		ApprovalRepo:     approval.NewPostgresRepository(pool),
		AuditRepo:        f.audit,
		PrecedentIndex:   decisionindex.NewStore(pool),
		IdentityProvider: &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true},
		GitHub:           gh,
		DivergenceConfig: dvPGConfig(),
	})
	loser := New(Config{Addr: "127.0.0.1:0",
		RunRepo:          run.NewPostgresRepository(loserPool),
		AuditRepo:        audit.NewPostgresRepository(loserPool),
		GitHub:           gh,
		DivergenceConfig: dvPGConfig(),
	})

	dvPGReject(t, winner, planStage)
	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryPrecedentDivergence)
	if err != nil || len(entries) != 1 {
		t.Fatalf("precedent_divergence entries = %d (%v), want 1", len(entries), err)
	}
	seq := entries[0].Sequence

	// Registered after the pools, so it runs BEFORE they close: release the
	// pinned winner and wait for both requests to return their connections.
	var inflight sync.WaitGroup
	t.Cleanup(func() {
		bp.releaseFirst()
		inflight.Wait()
	})
	results := make(chan *httptest.ResponseRecorder, 2)
	post := func(s *Server) {
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			results <- daPGPostAnswer(t, s, runID, seq)
		}()
	}

	post(winner)
	select {
	case <-bp.entered:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("the winner never reached File under the divergence answer lock")
	}

	post(loser)
	deadline := time.After(timescale.D(10 * time.Second))
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
barrier:
	for loserTries.tries.Load() < 2 {
		select {
		case n := <-bp.entered:
			t.Errorf("File call %d began while the winner held the divergence answer lock: the loser filed inside the critical section", n)
			break barrier
		case <-poll.C:
		case <-deadline:
			t.Fatalf("the loser completed %d pg_try_advisory_lock queries and filed nothing within the bound: it never contended on the divergence answer lock", loserTries.tries.Load())
		}
	}
	if got := loserTries.tries.Load(); got < 2 {
		t.Errorf("loser completed %d pg_try_advisory_lock queries while the winner held the lock, want >= 2 (refused, then polling): the loser never contended", got)
	}
	bp.releaseFirst()

	codes, recs := daAwaitResponses(t, results, 2)
	daAssertOneFilingOneAnswer(t, codes, recs, bp.count())
	answered, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryPrecedentDivergenceAnswered)
	if err != nil || len(answered) != 1 {
		t.Fatalf("precedent_divergence_answered entries on the real chain = %d (%v), want exactly 1", len(answered), err)
	}
}
