package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// verify_run_seam_test.go crosses the filtered-versus-complete seam (#5065)
// the always-complete fake backend cannot see: the REAL server.Handler
// (handleListRunAudit's withholding, its run-bound chain=true refusal and its
// pageOffset pagination) behind httptest, the REAL apiClient HTTP decode, and
// the real verifyRun. The in-memory repository keeps the Docker-free tests the
// counterfactual vehicles; the Postgres test pins the real ChainsByParent SQL.

// verifySeamAuditRepo is an in-memory hash-chained audit repository. Each
// run's chain is independently rooted (genesis prev_hash nil); sequence is
// one global counter, as in Postgres.
type verifySeamAuditRepo struct {
	audit.BaseFake
	mu      sync.Mutex
	seq     int64
	ts      time.Time
	entries []*audit.Entry
	parent  map[uuid.UUID]uuid.UUID // child run -> parent run
}

func newVerifySeamAuditRepo() *verifySeamAuditRepo {
	return &verifySeamAuditRepo{
		ts:     time.Now().UTC().Truncate(time.Microsecond),
		parent: map[uuid.UUID]uuid.UUID{},
	}
}

// add appends one chained entry for runID, linking it to that run's last
// entry and hashing it with the production audit.ComputeEntryHash.
func (f *verifySeamAuditRepo) add(t *testing.T, runID uuid.UUID, category string) *audit.Entry {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var prev *string
	for i := len(f.entries) - 1; i >= 0; i-- {
		if e := f.entries[i]; e.RunID != nil && *e.RunID == runID {
			h := e.EntryHash
			prev = &h
			break
		}
	}
	f.seq++
	rid := runID
	payload := json.RawMessage(fmt.Sprintf(`{"n":%d}`, f.seq))
	e := &audit.Entry{
		ID:        uuid.New(),
		Sequence:  f.seq,
		RunID:     &rid,
		Timestamp: f.ts.Add(time.Duration(f.seq) * time.Second),
		Category:  category,
		Payload:   payload,
		PrevHash:  prev,
	}
	f.rehash(t, e)
	f.entries = append(f.entries, e)
	return e
}

// rehash recomputes e.EntryHash from its current fields.
func (f *verifySeamAuditRepo) rehash(t *testing.T, e *audit.Entry) {
	t.Helper()
	h, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID: e.RunID, StageID: e.StageID, Timestamp: e.Timestamp, Category: e.Category,
		ActorKind: e.ActorKind, ActorSubject: e.ActorSubject, Payload: e.Payload, PrevHash: e.PrevHash,
	})
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	e.EntryHash = h
}

// cloneSeamEntries returns copies so the handler's in-place filters never alias the store.
func cloneSeamEntries(in []*audit.Entry) []*audit.Entry {
	out := make([]*audit.Entry, 0, len(in))
	for _, e := range in {
		c := *e
		out = append(out, &c)
	}
	return out
}

func (f *verifySeamAuditRepo) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*audit.Entry
	for _, e := range f.entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return cloneSeamEntries(out), nil
}

func (f *verifySeamAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	all, _ := f.ListForRun(ctx, runID)
	var out []*audit.Entry
	for _, e := range all {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

// ChainsByParent models ListAuditEntriesForRunChain: the run plus every run
// recursively linked to it by parent_run_id, ordered by global sequence.
func (f *verifySeamAuditRepo) ChainsByParent(_ context.Context, parentRunID uuid.UUID, _ bool) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := map[uuid.UUID]bool{parentRunID: true}
	for changed := true; changed; {
		changed = false
		for child, parent := range f.parent {
			if in[parent] && !in[child] {
				in[child] = true
				changed = true
			}
		}
	}
	var out []*audit.Entry
	for _, e := range f.entries {
		if e.RunID != nil && in[*e.RunID] {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return cloneSeamEntries(out), nil
}

// auditRequestLog records every GET /v0/runs/{id}/audit raw query the real
// server received.
type auditRequestLog struct {
	mu      sync.Mutex
	queries []string
}

func (l *auditRequestLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.queries...)
}

// newVerifySeam wires the REAL server.Handler over repo behind httptest with a
// stub API token of the given subject, and a REAL runResolver/apiClient,
// the construction acceptance_integration_gate_test.go uses.
func newVerifySeam(t *testing.T, repo audit.Repository, runRepo runpkg.Repository, subject string) (*runResolver, *auditRequestLog) {
	t.Helper()
	const bearer = "fhk_verify_run_seam"
	cfg := server.Config{
		AuditRepo: repo,
		APITokenRepo: &stubMCPAPITokens{tok: &apitoken.Token{
			ID: uuid.New(), Subject: subject, Scopes: []string{"read:runs", "read:audit"}, PlainText: bearer,
		}},
	}
	if runRepo != nil {
		cfg.RunRepo = runRepo
	}
	h := server.New(cfg).Handler()
	log := &auditRequestLog{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v0/runs/") && strings.HasSuffix(r.URL.Path, "/audit") {
			log.mu.Lock()
			log.queries = append(log.queries, r.URL.RawQuery)
			log.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: bearer}), getenv: func(string) string { return "" }}, log
}

// ordinaryWalk pages the ORDINARY feed (Chain false) to exhaustion through the
// same apiClient — the positive control that the server's withholding is live.
func ordinaryWalk(t *testing.T, r *runResolver, runID uuid.UUID) []AuditEntry {
	t.Helper()
	var all []AuditEntry
	cursor := ""
	for {
		items, next, err := r.api.ListRunAudit(context.Background(), runID, ListRunAuditFilter{Limit: listAuditLimitMax, Cursor: cursor})
		if err != nil {
			t.Fatalf("ordinary ListRunAudit: %v", err)
		}
		all = append(all, items...)
		if next == "" {
			return all
		}
		cursor = next
	}
}

func countCategory(entries []AuditEntry, category string) int {
	n := 0
	for _, e := range entries {
		if e.Category == category {
			n++
		}
	}
	return n
}

// TestVerifyRun_Seam_HiddenShadowRowsAcrossPages: two delegation_shadow rows
// the ordinary feed withholds, one at chain-feed index 199 with its successor
// at index 200, so the link crosses the 200-row page boundary.
func TestVerifyRun_Seam_HiddenShadowRowsAcrossPages(t *testing.T) {
	repo := newVerifySeamAuditRepo()
	runID := uuid.New()
	const total = 230
	shadowAt := map[int]bool{57: true, 199: true}
	for i := 0; i < total; i++ {
		cat := "stage_completed"
		if shadowAt[i] {
			cat = server.CategoryDelegationShadowEvaluated
		}
		repo.add(t, runID, cat)
	}
	r, log := newVerifySeam(t, repo, nil, "github:op")

	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if !out.Verified {
		t.Fatalf("Verified = false, want true on an intact chain with withheld shadow rows; failure_reason = %q", out.FailureReason)
	}
	if out.ChainLength != total {
		t.Errorf("ChainLength = %d, want %d (shadow rows included)", out.ChainLength, total)
	}
	if got := out.CategoryCounts[server.CategoryDelegationShadowEvaluated]; got != 2 {
		t.Errorf("CategoryCounts[delegation_shadow_evaluated] = %d, want 2", got)
	}
	verifyQueries := log.snapshot()
	if len(verifyQueries) < 2 {
		t.Errorf("audit-list requests = %d, want at least 2 (the walk must cross a page boundary)", len(verifyQueries))
	}
	for _, raw := range verifyQueries {
		if q, _ := url.ParseQuery(raw); q.Get("chain") != "true" {
			t.Errorf("verify_run request %q lacks chain=true", raw)
		}
	}

	// POSITIVE CONTROL: the same client's ordinary walk sees NO shadow row,
	// and its requests carry no chain param, so the fixture genuinely
	// exercises the server's withholding.
	before := len(verifyQueries)
	ordinary := ordinaryWalk(t, r, runID)
	if n := countCategory(ordinary, server.CategoryDelegationShadowEvaluated); n != 0 {
		t.Fatalf("ordinary feed returned %d delegation_shadow_evaluated rows, want 0 (withholding not exercised)", n)
	}
	if len(ordinary) != total-2 {
		t.Errorf("ordinary feed rows = %d, want %d", len(ordinary), total-2)
	}
	for _, raw := range log.snapshot()[before:] {
		q, _ := url.ParseQuery(raw)
		if _, ok := q["chain"]; ok {
			t.Errorf("ordinary ListRunAudit request %q carries a chain key, want none", raw)
		}
	}
}

// TestVerifyRun_Seam_RetryDescendantNotSpliced: the chain=true read returns a
// CI-retry descendant's independently rooted chain interleaved by sequence.
func TestVerifyRun_Seam_RetryDescendantNotSpliced(t *testing.T) {
	repo := newVerifySeamAuditRepo()
	parent, child := uuid.New(), uuid.New()
	repo.parent[child] = parent
	parentRows, childRows := 0, 0
	for i := 0; i < 12; i++ {
		repo.add(t, parent, "stage_completed")
		parentRows++
		if i%3 == 1 {
			repo.add(t, child, "stage_completed")
			childRows++
		}
	}
	r, _ := newVerifySeam(t, repo, nil, "github:op")

	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: parent.String()})
	if err != nil {
		t.Fatalf("verifyRun(parent): %v", err)
	}
	if !out.Verified {
		t.Fatalf("parent Verified = false, want true; failure_reason = %q", out.FailureReason)
	}
	if out.ChainLength != parentRows {
		t.Errorf("parent ChainLength = %d, want %d (parent rows only)", out.ChainLength, parentRows)
	}
	if out.DescendantEntriesExcluded != childRows {
		t.Errorf("DescendantEntriesExcluded = %d, want %d", out.DescendantEntriesExcluded, childRows)
	}
	if want := []string{child.String()}; !reflect.DeepEqual(out.DescendantRunIDs, want) {
		t.Errorf("DescendantRunIDs = %v, want %v", out.DescendantRunIDs, want)
	}

	_, cout, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: child.String()})
	if err != nil {
		t.Fatalf("verifyRun(child): %v", err)
	}
	if !cout.Verified || cout.ChainLength != childRows {
		t.Errorf("child: Verified=%v ChainLength=%d failure=%q, want true/%d", cout.Verified, cout.ChainLength, cout.FailureReason, childRows)
	}
	if cout.DescendantEntriesExcluded != 0 || len(cout.DescendantRunIDs) != 0 {
		t.Errorf("child: descendants = %d %v, want none", cout.DescendantEntriesExcluded, cout.DescendantRunIDs)
	}
}

// TestVerifyRun_Seam_TamperedStoredRowDetected: real corruption of a stored
// row still fails through the real handler and client.
func TestVerifyRun_Seam_TamperedStoredRowDetected(t *testing.T) {
	build := func() (*verifySeamAuditRepo, uuid.UUID, []*audit.Entry) {
		repo := newVerifySeamAuditRepo()
		runID := uuid.New()
		var rows []*audit.Entry
		for i := 0; i < 5; i++ {
			rows = append(rows, repo.add(t, runID, "stage_completed"))
		}
		return repo, runID, rows
	}

	t.Run("payload", func(t *testing.T) {
		repo, runID, rows := build()
		rows[2].Payload = json.RawMessage(`{"n":"tampered"}`)
		r, _ := newVerifySeam(t, repo, nil, "github:op")
		_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("verifyRun: %v", err)
		}
		if out.Verified || !strings.HasPrefix(out.FailureReason, "hash_mismatch") {
			t.Errorf("Verified=%v failure=%q, want false/hash_mismatch", out.Verified, out.FailureReason)
		}
	})

	t.Run("prev_hash", func(t *testing.T) {
		repo, runID, rows := build()
		wrong := rows[0].EntryHash // re-point entry 3 two links back
		rows[3].PrevHash = &wrong
		repo.rehash(t, rows[3])
		r, _ := newVerifySeam(t, repo, nil, "github:op")
		_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("verifyRun: %v", err)
		}
		if out.Verified || !strings.HasPrefix(out.FailureReason, "chain_broken") {
			t.Errorf("Verified=%v failure=%q, want false/chain_broken", out.Verified, out.FailureReason)
		}
	})
}

// TestVerifyRun_Seam_RunBoundTokenRefusedExplicitly: the real server answers a
// run-bound chain=true read 403 run_token_forbidden; the tool surfaces it as
// an explicit not-verified error, never a verified:false / chain_broken result.
func TestVerifyRun_Seam_RunBoundTokenRefusedExplicitly(t *testing.T) {
	repo := newVerifySeamAuditRepo()
	runID := uuid.New()
	repo.add(t, runID, "stage_completed")
	repo.add(t, runID, server.CategoryDelegationShadowEvaluated)
	repo.add(t, runID, "stage_completed")
	r, _ := newVerifySeam(t, repo, nil, "mcp:run:"+runID.String())

	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err == nil {
		t.Fatalf("verifyRun returned no error under a run-bound token; output = %+v", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "audit chain was NOT verified") {
		t.Errorf("error = %q, want it to say the audit chain was NOT verified", msg)
	}
	for _, want := range []string{"not a chain break", "operator token", "fishhawk export --run " + runID.String(), "fishhawk-verify", "run_token_forbidden"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to contain %q", msg, want)
		}
	}
	if out.Verified || out.FailureReason != "" || out.ChainLength != 0 {
		t.Errorf("output = %+v, want the zero value alongside the error", out)
	}
}

// TestVerifyRun_CrossLayer_Postgres_CompleteChainAndDescendant pins the real
// recursive-CTE ChainsByParent SQL and its ordering: a parent and a CI-retry
// descendant (ParentRunID) with more than one 200-row page of interleaved
// AppendChained rows, shadow rows on the parent with one straddling the page
// boundary. Skips without Docker, so it is never a counterfactual vehicle.
func TestVerifyRun_CrossLayer_Postgres_CompleteChainAndDescendant(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	inst := int64(42)
	parent, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/verify-run", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: runpkg.TriggerCLI, InstallationID: &inst,
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/verify-run", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: runpkg.TriggerCLI, InstallationID: &inst, ParentRunID: &parent.ID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	// Rows already on the runs (CreateRun may stamp its own) count too.
	feedIndex := 0
	if existing, err := auditRepo.ChainsByParent(ctx, parent.ID, false); err == nil {
		feedIndex = len(existing)
	}
	appendRow := func(runID uuid.UUID, cat string) {
		t.Helper()
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: runID, Timestamp: time.Now().UTC(), Category: cat,
			Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, feedIndex)),
		}); err != nil {
			t.Fatalf("AppendChained: %v", err)
		}
		feedIndex++
	}
	for feedIndex < 240 {
		switch {
		case feedIndex == 199 || feedIndex == 60:
			appendRow(parent.ID, server.CategoryDelegationShadowEvaluated)
		case feedIndex%5 == 2:
			appendRow(child.ID, "stage_completed")
		default:
			appendRow(parent.ID, "stage_completed")
		}
	}

	chain, err := auditRepo.ChainsByParent(ctx, parent.ID, false)
	if err != nil {
		t.Fatalf("ChainsByParent: %v", err)
	}
	if len(chain) <= listAuditLimitMax {
		t.Fatalf("chain rows = %d, want more than one %d-row page", len(chain), listAuditLimitMax)
	}
	parentRows, childRows, shadow := 0, 0, 0
	for _, e := range chain {
		switch *e.RunID {
		case parent.ID:
			parentRows++
			if e.Category == server.CategoryDelegationShadowEvaluated {
				shadow++
			}
		case child.ID:
			childRows++
		}
	}
	if shadow != 2 {
		t.Fatalf("parent shadow rows = %d, want 2", shadow)
	}

	r, _ := newVerifySeam(t, auditRepo, runRepo, "github:op")
	_, out, err := r.verifyRun(ctx, nil, VerifyRunInput{RunID: parent.ID.String()})
	if err != nil {
		t.Fatalf("verifyRun(parent): %v", err)
	}
	if !out.Verified || out.ChainLength != parentRows {
		t.Fatalf("parent: Verified=%v ChainLength=%d failure=%q, want true/%d", out.Verified, out.ChainLength, out.FailureReason, parentRows)
	}
	if out.DescendantEntriesExcluded != childRows || !reflect.DeepEqual(out.DescendantRunIDs, []string{child.ID.String()}) {
		t.Errorf("parent descendants = %d %v, want %d [%s]", out.DescendantEntriesExcluded, out.DescendantRunIDs, childRows, child.ID)
	}
	if got := out.CategoryCounts[server.CategoryDelegationShadowEvaluated]; got != 2 {
		t.Errorf("parent CategoryCounts[delegation_shadow_evaluated] = %d, want 2", got)
	}

	_, cout, err := r.verifyRun(ctx, nil, VerifyRunInput{RunID: child.ID.String()})
	if err != nil {
		t.Fatalf("verifyRun(child): %v", err)
	}
	if !cout.Verified || cout.ChainLength != childRows {
		t.Errorf("child: Verified=%v ChainLength=%d failure=%q, want true/%d", cout.Verified, cout.ChainLength, cout.FailureReason, childRows)
	}

	if n := countCategory(ordinaryWalk(t, r, parent.ID), server.CategoryDelegationShadowEvaluated); n != 0 {
		t.Errorf("ordinary feed returned %d delegation_shadow_evaluated rows, want 0", n)
	}
}
