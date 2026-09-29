package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakePrecedentIndex records the ListFilter it RECEIVES and honours its Repo,
// DecisionClass and StageKind arms.
//
// Recording the filter is what makes the repository-isolation case a real
// control (#3731 binding condition 3): the assertion is on filter.Repo, so
// deleting the handler's `Repo:` assignment is RED even though precedent.Rank's
// own defensive re-assertion would ALSO have dropped the foreign row. Rank's
// check is defence in depth and has its own separate test.
type fakePrecedentIndex struct {
	rows []decisionindex.Row

	lastFilter  decisionindex.ListFilter
	filterCalls int
	listErr     error

	gate    decisionindex.GateContext
	gateRef decisionindex.GateRef
	gateErr error
}

func (f *fakePrecedentIndex) List(_ context.Context, filter decisionindex.ListFilter) ([]decisionindex.Row, error) {
	f.lastFilter = filter
	f.filterCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []decisionindex.Row
	for _, r := range f.rows {
		if filter.Repo != "" && r.Repo != filter.Repo {
			continue
		}
		if filter.DecisionClass != "" && r.DecisionClass != filter.DecisionClass {
			continue
		}
		if filter.StageKind != "" && r.StageKind != filter.StageKind {
			continue
		}
		out = append(out, r)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *fakePrecedentIndex) GateContext(_ context.Context, ref decisionindex.GateRef) (decisionindex.GateContext, error) {
	f.gateRef = ref
	if f.gateErr != nil {
		return decisionindex.GateContext{}, f.gateErr
	}
	return f.gate, nil
}

// precedentAuditFake serves ListForRun only, counting the per-run reads so the
// bounded-N+1 claim is asserted rather than argued.
type precedentAuditFake struct {
	audit.BaseFake
	byRun    map[uuid.UUID][]*audit.Entry
	errFor   map[uuid.UUID]error
	runCalls map[uuid.UUID]int
}

func (f *precedentAuditFake) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	if f.runCalls == nil {
		f.runCalls = map[uuid.UUID]int{}
	}
	f.runCalls[runID]++
	if err, bad := f.errFor[runID]; bad {
		return nil, err
	}
	return f.byRun[runID], nil
}

func precedentEntry(runID uuid.UUID, seq int64, payload map[string]any) *audit.Entry {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	rid := runID
	return &audit.Entry{ID: uuid.New(), RunID: &rid, Sequence: seq,
		Category: "approval_submitted", Payload: raw}
}

func precedentRow(runID uuid.UUID, seq int64, repo string, mutate func(*decisionindex.Row)) decisionindex.Row {
	r := decisionindex.Row{
		SourceSequence:  seq,
		SourceEntryHash: "hash-" + repo,
		RunID:           runID,
		Repo:            repo,
		WorkflowID:      "feature_change",
		DoctrineVersion: "sha-1",
		DecisionClass:   decisionindex.ClassPlanApproval,
		StageKind:       "plan",
		Outcome:         "approve",
		TouchedPaths:    []string{"backend/a.go"},
		EscalationKeys:  []string{},
		DecidedAt:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		ReasonSequence:  seq,
		ReasonKey:       "reason",
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

// precedentGET drives the handler DIRECTLY with an injected identity, exactly as
// getAttention does: the auth middleware would otherwise re-derive identity from
// the (credential-less) request and the account narrowing under test would have
// nothing to narrow by. Route REGISTRATION is pinned separately, by
// TestPrecedent_RouteIsRegistered driving the real mux.
func precedentGET(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/precedent"+query, nil), memberIdentity())
	rec := httptest.NewRecorder()
	s.handleGetPrecedent(rec, req)
	return rec
}

// TestPrecedent_RouteIsRegistered pins the mux entry: without it the router
// answers 404 rather than the handler's own 503.
func TestPrecedent_RouteIsRegistered(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet, "/v0/precedent?repo=a/b&decision_class=plan_approval", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /v0/precedent is not registered (404)")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the handler's own 503 (body %s)", rec.Code, rec.Body.String())
	}
}

func decodePrecedent(t *testing.T, rec *httptest.ResponseRecorder) precedentResponse {
	t.Helper()
	var out precedentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func hasDegradation(resp precedentResponse, reason string) bool {
	for _, d := range resp.Degraded {
		if d.Reason == reason {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Fail-closed / defensive branches, one test each
// ---------------------------------------------------------------------------

// TestPrecedent_UnconfiguredIndexReturns503: nil Config.PrecedentIndex.
// COUNTERFACTUAL: delete the nil guard → the handler dereferences a nil
// interface on List and panics (500), RED on the 503 assertion.
func TestPrecedent_UnconfiguredIndexReturns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "precedent_unconfigured") {
		t.Errorf("body = %s, want the named precedent_unconfigured code", rec.Body.String())
	}
}

// TestPrecedent_MissingRepoReturns400.
//
// MECHANISM: the fake holds rows for TWO repositories, so deleting the
// required-repo check yields 200 with CROSS-REPOSITORY items rather than 400 —
// RED on both the status and the body.
func TestPrecedent_MissingRepoReturns400(t *testing.T) {
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(uuid.New(), 1, "acme/widgets", nil),
		precedentRow(uuid.New(), 2, "other/repo", nil),
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?decision_class=plan_approval")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"field":"repo"`) {
		t.Errorf("body = %s, want the offending field named", rec.Body.String())
	}
}

// TestPrecedent_UnknownDecisionClassReturns400.
//
// MECHANISM: the fake holds rows of a REAL class, so deleting the closed-set
// validation yields 200 with an empty result set instead of 400 — RED on status.
func TestPrecedent_UnknownDecisionClassReturns400(t *testing.T) {
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(uuid.New(), 1, "acme/widgets", nil),
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	for _, q := range []string{
		"?repo=acme/widgets&decision_class=not_a_class",
		"?repo=acme/widgets",
	} {
		rec := precedentGET(t, s, q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", q, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "plan_approval") || !strings.Contains(body, "merge_verdict") {
			t.Errorf("%s: body = %s, want the accepted classes named", q, body)
		}
	}
}

// TestPrecedent_RepositoryIsolation is the #3731 binding-condition-3 control:
// the assertion is on the ListFilter the STORE RECEIVED, not merely on the items
// returned. COUNTERFACTUAL: delete `Repo: pctx.Repo` from the ListFilter build →
// the recorded filter.Repo is empty and this is RED, before Rank's defensive
// re-assertion can mask it.
func TestPrecedent_RepositoryIsolation(t *testing.T) {
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(uuid.New(), 1, "acme/widgets", nil),
		precedentRow(uuid.New(), 2, "other/repo", nil),
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if idx.lastFilter.Repo != "acme/widgets" {
		t.Errorf("the store received filter.Repo = %q, want acme/widgets — the isolation is enforced in the QUERY", idx.lastFilter.Repo)
	}
	if idx.lastFilter.DecisionClass != decisionindex.ClassPlanApproval {
		t.Errorf("filter.DecisionClass = %q, want plan_approval", idx.lastFilter.DecisionClass)
	}
	if !idx.lastFilter.Newest || idx.lastFilter.Limit != precedentCandidateWindow {
		t.Errorf("filter window = newest:%v limit:%d, want newest:true limit:%d",
			idx.lastFilter.Newest, idx.lastFilter.Limit, precedentCandidateWindow)
	}
	if idx.lastFilter.AccountID == nil || idx.lastFilter.AccountID.String() != testOperatorAccountID {
		t.Errorf("filter.AccountID = %v, want the caller's account %s", idx.lastFilter.AccountID, testOperatorAccountID)
	}
	resp := decodePrecedent(t, rec)
	for _, it := range resp.Results {
		if it.Repo != "acme/widgets" {
			t.Errorf("result from %q leaked into an acme/widgets query", it.Repo)
		}
	}
}

// TestPrecedent_EmptyResultReturnsReasonNotError.
func TestPrecedent_EmptyResultReturnsReasonNotError(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: &fakePrecedentIndex{}})
	rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodePrecedent(t, rec)
	if len(resp.Results) != 0 {
		t.Errorf("results = %d, want 0", len(resp.Results))
	}
	if resp.Results == nil && !strings.Contains(rec.Body.String(), `"results":[]`) {
		t.Errorf("body = %s, want results rendered as an empty array", rec.Body.String())
	}
	if !hasDegradation(resp, precedentDegradedNoIndexedDecisions) {
		t.Errorf("degraded = %+v, want the named %s reason", resp.Degraded, precedentDegradedNoIndexedDecisions)
	}
}

// TestPrecedent_CandidateWindowTruncationIsMarked seeds EXACTLY
// precedentCandidateWindow rows. COUNTERFACTUAL: delete the full-window check →
// truncated stays false and the named degradation is absent, RED on both.
func TestPrecedent_CandidateWindowTruncationIsMarked(t *testing.T) {
	idx := &fakePrecedentIndex{}
	for i := 0; i < precedentCandidateWindow; i++ {
		idx.rows = append(idx.rows, precedentRow(uuid.New(), int64(i+1), "acme/widgets", nil))
	}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval")
	resp := decodePrecedent(t, rec)
	if !resp.Truncated {
		t.Error("truncated = false on a FULL candidate window")
	}
	if !hasDegradation(resp, precedentDegradedWindowTruncated) {
		t.Errorf("degraded = %+v, want %s", resp.Degraded, precedentDegradedWindowTruncated)
	}
}

// TestPrecedent_ReasonExcerptCapped. COUNTERFACTUAL: delete the capExcerpt call
// → the whole 5000-byte reason is echoed and the length assertion is RED.
func TestPrecedent_ReasonExcerptCapped(t *testing.T) {
	runID := uuid.New()
	long := strings.Repeat("x", 5000)
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{precedentRow(runID, 7, "acme/widgets", nil)}}
	af := &precedentAuditFake{byRun: map[uuid.UUID][]*audit.Entry{
		runID: {precedentEntry(runID, 7, map[string]any{"reason": long})},
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx, AuditRepo: af})
	resp := decodePrecedent(t, precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval"))
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Results))
	}
	got := resp.Results[0].ReasonExcerpt
	if len(got) > precedentReasonExcerptCap {
		t.Errorf("excerpt is %d bytes, want <= %d", len(got), precedentReasonExcerptCap)
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("excerpt = %q, want a truncation marker so the cut is not silent", got)
	}
}

// TestPrecedent_UnreadableReasonEntryDegrades: the audit fake errors for ONE run
// only, so the OTHER item keeps its excerpt.
// COUNTERFACTUAL: delete the degradation append → the omission is silent and the
// test is RED on the missing named reason.
func TestPrecedent_UnreadableReasonEntryDegrades(t *testing.T) {
	bad, good := uuid.New(), uuid.New()
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(bad, 1, "acme/widgets", nil),
		precedentRow(good, 2, "acme/widgets", nil),
	}}
	af := &precedentAuditFake{
		byRun:  map[uuid.UUID][]*audit.Entry{good: {precedentEntry(good, 2, map[string]any{"reason": "kept"})}},
		errFor: map[uuid.UUID]error{bad: errors.New("chain read failed")},
	}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx, AuditRepo: af})
	resp := decodePrecedent(t, precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval"))
	if !hasDegradation(resp, precedentDegradedReasonUnreadable) {
		t.Errorf("degraded = %+v, want the named %s reason", resp.Degraded, precedentDegradedReasonUnreadable)
	}
	for _, it := range resp.Results {
		switch it.RunID {
		case bad.String():
			if it.ReasonExcerpt != "" {
				t.Errorf("unreadable run carried an excerpt %q, want none", it.ReasonExcerpt)
			}
		case good.String():
			if it.ReasonExcerpt != "kept" {
				t.Errorf("readable run's excerpt = %q, want 'kept'", it.ReasonExcerpt)
			}
		}
	}
}

// TestPrecedent_MissingReasonEntryOrKeyDegrades covers the other two excerpt
// failure modes: no chain entry at the cited sequence, and an entry whose payload
// carries no STRING value at the named key.
func TestPrecedent_MissingReasonEntryOrKeyDegrades(t *testing.T) {
	noEntry, wrongType := uuid.New(), uuid.New()
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(noEntry, 1, "acme/widgets", nil),
		precedentRow(wrongType, 2, "acme/widgets", nil),
	}}
	af := &precedentAuditFake{byRun: map[uuid.UUID][]*audit.Entry{
		noEntry:   {precedentEntry(noEntry, 999, map[string]any{"reason": "elsewhere"})},
		wrongType: {precedentEntry(wrongType, 2, map[string]any{"reason": 42})},
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx, AuditRepo: af})
	resp := decodePrecedent(t, precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval"))
	missing := 0
	for _, d := range resp.Degraded {
		if d.Reason == precedentDegradedReasonMissing {
			missing++
		}
	}
	if missing != 2 {
		t.Errorf("%s degradations = %d, want 2 (missing entry + non-string value); got %+v",
			precedentDegradedReasonMissing, missing, resp.Degraded)
	}
	for _, it := range resp.Results {
		if it.ReasonExcerpt != "" {
			t.Errorf("item %d fabricated an excerpt %q", it.SourceSequence, it.ReasonExcerpt)
		}
	}
}

// TestPrecedent_NilAuditRepoDegrades: excerpts need the chain; no chain is a
// named degradation, not a failed request.
func TestPrecedent_NilAuditRepoDegrades(t *testing.T) {
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{precedentRow(uuid.New(), 1, "acme/widgets", nil)}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !hasDegradation(decodePrecedent(t, rec), precedentDegradedAuditRepoUnavailable) {
		t.Errorf("body = %s, want the named %s reason", rec.Body.String(), precedentDegradedAuditRepoUnavailable)
	}
}

// TestPrecedent_ChainReadOncePerDistinctRun pins the bounded N+1: three items
// across two runs cost TWO chain reads, not three.
func TestPrecedent_ChainReadOncePerDistinctRun(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(a, 1, "acme/widgets", nil),
		precedentRow(a, 2, "acme/widgets", nil),
		precedentRow(b, 3, "acme/widgets", nil),
	}}
	af := &precedentAuditFake{byRun: map[uuid.UUID][]*audit.Entry{
		a: {precedentEntry(a, 1, map[string]any{"reason": "r1"}), precedentEntry(a, 2, map[string]any{"reason": "r2"})},
		b: {precedentEntry(b, 3, map[string]any{"reason": "r3"})},
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx, AuditRepo: af})
	if rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := af.runCalls[a]; got != 1 {
		t.Errorf("run A chain reads = %d, want exactly 1 for its 2 items", got)
	}
	if got := af.runCalls[b]; got != 1 {
		t.Errorf("run B chain reads = %d, want 1", got)
	}
}

// TestPrecedent_RepeatedCallsByteIdentical: the same request twice against the
// same fake yields byte-identical bodies (determinism across the whole render,
// including the degradation list, which is assembled from a map iteration).
func TestPrecedent_RepeatedCallsByteIdentical(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(a, 1, "acme/widgets", nil),
		precedentRow(b, 2, "acme/widgets", func(r *decisionindex.Row) { r.TouchedPaths = []string{"backend/b.go"} }),
		precedentRow(c, 3, "acme/widgets", nil),
	}}
	af := &precedentAuditFake{
		byRun:  map[uuid.UUID][]*audit.Entry{a: {precedentEntry(a, 1, map[string]any{"reason": "r1"})}},
		errFor: map[uuid.UUID]error{b: errors.New("boom"), c: errors.New("boom")},
	}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx, AuditRepo: af})
	q := "?repo=acme/widgets&decision_class=plan_approval&paths=backend/a.go"
	first := precedentGET(t, s, q).Body.String()
	for i := 0; i < 5; i++ {
		if got := precedentGET(t, s, q).Body.String(); got != first {
			t.Fatalf("body differs on repeat %d:\n first = %s\nsecond = %s", i, first, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Gate reference (binding condition 1)
// ---------------------------------------------------------------------------

// TestPrecedent_GateReferenceResolvesAndEchoes: the derived context is used for
// the ranking and echoed back, and the account travels INTO the resolve.
func TestPrecedent_GateReferenceResolvesAndEchoes(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	idx := &fakePrecedentIndex{
		gate: decisionindex.GateContext{
			Repo: "acme/widgets", WorkflowID: "feature_change", DoctrineVersion: "sha-1",
			StageKind: "implement", TouchedPaths: []string{"backend/a.go"},
			EscalationKeys: []string{"scope_cap_exceeded"},
		},
		rows: []decisionindex.Row{
			precedentRow(uuid.New(), 1, "acme/widgets", func(r *decisionindex.Row) {
				r.StageKind = "implement"
				r.EscalationKeys = []string{"scope_cap_exceeded"}
			}),
		},
	}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?decision_class=plan_approval&run_id="+runID.String()+"&stage_id="+stageID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if idx.gateRef.RunID != runID {
		t.Errorf("resolved run = %s, want %s", idx.gateRef.RunID, runID)
	}
	if idx.gateRef.StageID == nil || *idx.gateRef.StageID != stageID {
		t.Errorf("resolved stage = %v, want %s", idx.gateRef.StageID, stageID)
	}
	if idx.gateRef.AccountID == nil || idx.gateRef.AccountID.String() != testOperatorAccountID {
		t.Fatalf("the gate resolve carried AccountID = %v, want the caller's account %s — the read MUST be account-scoped",
			idx.gateRef.AccountID, testOperatorAccountID)
	}
	resp := decodePrecedent(t, rec)
	rc := resp.ResolvedContext
	if rc.Repo != "acme/widgets" || rc.StageKind != "implement" {
		t.Errorf("resolved context = %+v, want the DERIVED repo and stage kind", rc)
	}
	if len(rc.TouchedPaths) != 1 || rc.TouchedPaths[0] != "backend/a.go" {
		t.Errorf("echoed paths = %v, want the derived [backend/a.go]", rc.TouchedPaths)
	}
	if len(rc.EscalationKeys) != 1 || rc.EscalationKeys[0] != "scope_cap_exceeded" {
		t.Errorf("echoed keys = %v, want the derived [scope_cap_exceeded]", rc.EscalationKeys)
	}
	if len(resp.Results) != 1 || resp.Results[0].Score.EscalationKeys == 0 {
		t.Errorf("the derived keys were not used for RANKING: %+v", resp.Results)
	}
}

// TestPrecedent_GateReferenceForeignAccountIs404 is the #3731 binding-condition-1
// control. The store answers ErrRunMissing for a run outside the caller's
// account (the account is part of the reference), and the handler must return
// the SAME 404 as for a nonexistent run while echoing NO resolved context.
//
// COUNTERFACTUAL: drop AccountID from the GateRef the handler builds → the fake
// (like the real store) resolves the run and the response carries the repository
// and the derived paths and keys, RED on the 404 AND on the body assertions.
func TestPrecedent_GateReferenceForeignAccountIs404(t *testing.T) {
	idx := &fakePrecedentIndex{gateErr: decisionindex.ErrRunMissing}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?decision_class=plan_approval&run_id="+uuid.New().String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"acme/widgets", "resolved_context", "touched_paths", "escalation_keys", "results"} {
		if strings.Contains(body, leak) {
			t.Errorf("the 404 body leaks %q: %s", leak, body)
		}
	}
	if idx.filterCalls != 0 {
		t.Errorf("the candidate window was queried %d times after an unresolvable gate reference, want 0", idx.filterCalls)
	}
}

// TestPrecedent_GateReferenceMalformedIdsAre400.
func TestPrecedent_GateReferenceMalformedIdsAre400(t *testing.T) {
	idx := &fakePrecedentIndex{}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	for _, q := range []string{
		"?decision_class=plan_approval&run_id=not-a-uuid",
		"?decision_class=plan_approval&run_id=" + uuid.New().String() + "&stage_id=not-a-uuid",
	} {
		if rec := precedentGET(t, s, q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

// TestPrecedent_ExplicitFieldsOverrideDerived.
func TestPrecedent_ExplicitFieldsOverrideDerived(t *testing.T) {
	idx := &fakePrecedentIndex{
		gate: decisionindex.GateContext{Repo: "derived/repo", StageKind: "plan",
			TouchedPaths: []string{"derived.go"}, EscalationKeys: []string{"derived_key"}},
		rows: []decisionindex.Row{precedentRow(uuid.New(), 1, "explicit/repo", nil)},
	}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?decision_class=plan_approval&run_id="+uuid.New().String()+
		"&repo=explicit/repo&stage_kind=plan&paths=explicit.go&escalation_keys=explicit_key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if idx.lastFilter.Repo != "explicit/repo" {
		t.Errorf("filter.Repo = %q, want the EXPLICIT override", idx.lastFilter.Repo)
	}
	rc := decodePrecedent(t, rec).ResolvedContext
	if len(rc.TouchedPaths) != 1 || rc.TouchedPaths[0] != "explicit.go" {
		t.Errorf("echoed paths = %v, want the explicit override", rc.TouchedPaths)
	}
	if len(rc.EscalationKeys) != 1 || rc.EscalationKeys[0] != "explicit_key" {
		t.Errorf("echoed keys = %v, want the explicit override", rc.EscalationKeys)
	}
}

// TestPrecedent_ResolvedContextListsCapped is the server half of #3731 binding
// condition 2: 10,000 supplied paths do not make the echo a function of the
// REQUEST. COUNTERFACTUAL: delete capResolvedList's cap → the echo carries all
// 10,000 and both assertions are RED.
func TestPrecedent_ResolvedContextListsCapped(t *testing.T) {
	idx := &fakePrecedentIndex{}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	// 5,000 paths, not 10,000: net/url's OWN query-parameter limit is 10,000 and
	// r.URL.Query() swallows the resulting error, so a request carrying more
	// than that parses to an EMPTY query and the handler fails CLOSED on the
	// now-missing decision_class (asserted at the end). The cap under test here
	// is the one this change adds, so the fixture stays under that ceiling.
	const supplied = 5000
	var q strings.Builder
	q.WriteString("?repo=acme/widgets&decision_class=plan_approval")
	for i := 0; i < supplied; i++ {
		q.WriteString("&paths=pkg/f")
		q.WriteString(uuid.New().String())
		q.WriteString(".go")
	}
	rec := precedentGET(t, s, q.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	rc := decodePrecedent(t, rec).ResolvedContext
	if len(rc.TouchedPaths) != precedentResolvedContextListCap {
		t.Errorf("echoed paths = %d, want the cap %d", len(rc.TouchedPaths), precedentResolvedContextListCap)
	}
	if rc.TouchedPathsTotal != supplied || !rc.TouchedPathsTruncated {
		t.Errorf("total/truncated = %d/%v, want %d/true", rc.TouchedPathsTotal, rc.TouchedPathsTruncated, supplied)
	}

	// The platform ceiling itself: past net/url's limit the whole query parses
	// empty, so the request is REFUSED rather than answered against a silently
	// emptied context.
	q.WriteString(strings.Repeat("&paths=x.go", 6000))
	if over := precedentGET(t, s, q.String()); over.Code != http.StatusBadRequest {
		t.Errorf("status past net/url's query-parameter limit = %d, want 400 (fail closed)", over.Code)
	}
}

// TestPrecedent_ConcernCategoryNormalized: a raw reviewer spelling matches the
// canonical value the index stored. COUNTERFACTUAL: delete the
// NormalizeConcernCategory call → "Bug" never equals "correctness", the category
// component is 0, and the test is RED.
func TestPrecedent_ConcernCategoryNormalized(t *testing.T) {
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(uuid.New(), 1, "acme/widgets", func(r *decisionindex.Row) {
			r.DecisionClass = decisionindex.ClassConcernWaive
			r.ConcernCategory = decisionindex.CorrectnessConcernCategory
		}),
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=concern_waive&concern_category=Bug")
	resp := decodePrecedent(t, rec)
	if resp.ResolvedContext.ConcernCategory != decisionindex.CorrectnessConcernCategory {
		t.Errorf("echoed category = %q, want the canonical correctness", resp.ResolvedContext.ConcernCategory)
	}
	if len(resp.Results) != 1 || resp.Results[0].Score.ConcernCategory == 0 {
		t.Errorf("the normalized category did not match the indexed row: %+v", resp.Results)
	}
}

// TestPrecedent_LimitValidation: over-max is refused rather than silently
// clamped, so the per-request chain-read bound is a real bound.
func TestPrecedent_LimitValidation(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: &fakePrecedentIndex{}})
	rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval&limit=51")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a limit above %d", rec.Code, precedentMaxLimit)
	}
}

// TestPrecedent_ListErrorIs500: a store fault is a failed request, never a 200
// with a silently short ranking.
func TestPrecedent_ListErrorIs500(t *testing.T) {
	idx := &fakePrecedentIndex{listErr: errors.New("db down")}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	if rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval"); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestPrecedent_GateContextErrorIs500: a non-ErrRunMissing resolve fault is a
// 500, NOT the 404 the missing/foreign/wrong-stage cases share — the two failure
// classes stay separated.
func TestPrecedent_GateContextErrorIs500(t *testing.T) {
	idx := &fakePrecedentIndex{gateErr: errors.New("db down")}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	rec := precedentGET(t, s, "?decision_class=plan_approval&run_id="+uuid.New().String())
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestPrecedent_RepoNotVisibleReturns403 covers the point-read repo-visibility
// DENY. The other precedent tests wire NO RepoVisibility mirror, which makes the
// check inert — so deleting it would be unobservable on their fixtures (the
// masking case). This one wires a mirror that denies the repository, which is
// the only fixture on which the check is the thing in the path.
//
// COUNTERFACTUAL: delete the enforceRepoVisibility call → 200 with the other
// repository's decision reasons, RED on the status.
func TestPrecedent_RepoNotVisibleReturns403(t *testing.T) {
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(uuid.New(), 1, "acme/secret", nil),
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx,
		RepoVisibility: newFakeRepoVisibility(map[string]bool{"acme/visible": true})})

	rec := precedentGET(t, s, "?repo=acme/secret&decision_class=plan_approval")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a repository the caller cannot read (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "repo_forbidden") {
		t.Errorf("body = %s, want the named repo_forbidden code", rec.Body.String())
	}
	if idx.filterCalls != 0 {
		t.Errorf("the index was queried %d times for an invisible repository, want 0", idx.filterCalls)
	}

	// Positive control: a VISIBLE repository proceeds, so the 403 above is the
	// check biting rather than a broken fixture.
	if ok := precedentGET(t, s, "?repo=acme/visible&decision_class=plan_approval"); ok.Code != http.StatusOK {
		t.Errorf("visible repository = %d, want 200 (body %s)", ok.Code, ok.Body.String())
	}
}

// TestPrecedent_RepoVisibilityFaultIs503: a filter that cannot function fails the
// WHOLE request closed, never a 200 with an unfiltered answer.
func TestPrecedent_RepoVisibilityFaultIs503(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{"acme/widgets": true})
	vis.err = errors.New("mirror down")
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: &fakePrecedentIndex{}, RepoVisibility: vis})
	if rec := precedentGET(t, s, "?repo=acme/widgets&decision_class=plan_approval"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 on a visibility-filter fault", rec.Code)
	}
}

// TestPrecedent_UnparseableSessionAccountIs500 covers callerAccountUUID's
// fail-closed branch: a non-empty Identity.AccountID that is not a UUID is a
// corrupted sessions-row invariant, and the request FAILS rather than widening
// the read to every account.
//
// COUNTERFACTUAL: make the parse error return (nil, true) instead → the request
// answers 200 with an unbounded (nil-account) filter, RED on the status AND on
// the recorded filter.
func TestPrecedent_UnparseableSessionAccountIs500(t *testing.T) {
	idx := &fakePrecedentIndex{rows: []decisionindex.Row{
		precedentRow(uuid.New(), 1, "acme/widgets", nil),
	}}
	s := New(Config{Addr: "127.0.0.1:0", PrecedentIndex: idx})
	id := memberIdentity()
	id.AccountID = "not-a-uuid"
	req := withIdentity(httptest.NewRequest(http.MethodGet,
		"/v0/precedent?repo=acme/widgets&decision_class=plan_approval", nil), id)
	rec := httptest.NewRecorder()
	s.handleGetPrecedent(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	if idx.filterCalls != 0 {
		t.Errorf("the index was queried %d times under an unresolvable account, want 0 — never a widened read", idx.filterCalls)
	}
}
