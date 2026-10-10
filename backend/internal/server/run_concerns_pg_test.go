package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// run_concerns_pg_test.go drives GET /v0/runs/{run_id}/concerns (#4101) END TO
// END against REAL Postgres. Idiom (the upkeep_dispositions_pg_test.go shape):
// every request goes through the REGISTERED route via s.Handler().ServeHTTP
// with a real "Authorization: Bearer" token that the auth middleware resolves
// through APITokenRepo — never withIdentity/injectIdentity on a directly
// invoked handler — so the mux pattern (and its coexistence with POST
// /v0/runs/{run_id}/concerns/waive), requireRunAccount and the read-scope
// check are all on the path. It crosses persistence (InsertRaised /
// ApplyResolution / ListRuns by DecomposedFrom) -> handler -> wire shape.

const (
	rcPGReadBearer   = "fhk_rc_operator_read"
	rcPGNoReadBearer = "fhk_rc_operator_noread"
)

// multiTokenRepo authenticates any of a fixed set of plaintext bearers.
type multiTokenRepo struct {
	apitoken.Repository
	byPlain map[string]*apitoken.Token
}

func (m *multiTokenRepo) Authenticate(_ context.Context, plaintext string) (*apitoken.Token, error) {
	if tok, ok := m.byPlain[plaintext]; ok {
		return tok, nil
	}
	return nil, apitoken.ErrNotFound
}

type rcPGFixture struct {
	s        *Server
	runs     run.Repository
	concerns concern.Repository
}

func newRCPGFixture(t *testing.T) *rcPGFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &rcPGFixture{runs: run.NewPostgresRepository(pool), concerns: concern.NewPostgresRepository(pool)}
	tokens := &multiTokenRepo{byPlain: map[string]*apitoken.Token{
		rcPGReadBearer: {
			ID: uuid.New(), Subject: "github:ops", Scopes: []string{"read:runs", scopeGateViewRead}, PlainText: rcPGReadBearer,
		},
		rcPGNoReadBearer: {
			ID: uuid.New(), Subject: "github:ops", Scopes: []string{"read:runs", "write:runs"}, PlainText: rcPGNoReadBearer,
		},
	}}
	f.s = New(Config{RunRepo: f.runs, ConcernRepo: f.concerns, APITokenRepo: tokens})
	return f
}

// seedRun creates a run (optionally a child) with one implement stage, the
// stage a review_concerns row must reference.
func (f *rcPGFixture) seedRun(t *testing.T, parentRunID, decomposedFrom *uuid.UUID, slice *int) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	rn, err := f.runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
		ParentRunID: parentRunID, DecomposedFrom: decomposedFrom, SliceIndex: slice,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	st, err := f.runs.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 1, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return rn.ID, st.ID
}

func (f *rcPGFixture) raise(t *testing.T, runID, stageID uuid.UUID, note string) *concern.Concern {
	t.Helper()
	rows, err := f.concerns.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: runID, StageID: stageID, StageKind: concern.StageKindImplement, ReviewerModel: "model-pg",
		OriginReviewSequence: 3, Concerns: []concern.RaisedConcern{{Severity: "high", Category: "correctness", Note: note}},
	})
	if err != nil {
		t.Fatalf("insert raised: %v", err)
	}
	return rows[0]
}

func (f *rcPGFixture) get(t *testing.T, runID uuid.UUID, query, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v0/runs/" + runID.String() + "/concerns"
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	return w
}

func rcPGDecode(t *testing.T, w *httptest.ResponseRecorder) runConcernListResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runConcernListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return resp
}

func rcSortedIDs(items []runConcernListItem) []string {
	ids := rcItemIDs(items)
	slices.Sort(ids)
	return ids
}

// TestRunConcernsPG_IncludeChildren_EndToEnd: a parent, one decomposition child
// (DecomposedFrom = parent, slice 0) and a recovery child (ParentRunID =
// parent, DecomposedFrom NULL), each with an open concern, plus one WAIVED
// parent concern. include_children=true lists exactly the parent's open
// concern and the decomposition child's (stamped with child_run_id/slice 0);
// state=all adds the waived row; the recovery child's never appears.
func TestRunConcernsPG_IncludeChildren_EndToEnd(t *testing.T) {
	f := newRCPGFixture(t)
	parent, parentStage := f.seedRun(t, nil, nil, nil)
	slice0 := 0
	child, childStage := f.seedRun(t, &parent, &parent, &slice0)
	recovery, recoveryStage := f.seedRun(t, &parent, nil, nil)

	parentOpen := f.raise(t, parent, parentStage, "parent open concern")
	parentWaived := f.raise(t, parent, parentStage, "parent waived concern")
	if _, err := f.concerns.ApplyResolution(context.Background(), parentWaived.ID, concern.StateWaived, "operator waive"); err != nil {
		t.Fatalf("waive: %v", err)
	}
	childOpen := f.raise(t, child, childStage, "child open concern")
	f.raise(t, recovery, recoveryStage, "recovery child concern")

	open := rcPGDecode(t, f.get(t, parent, "include_children=true", rcPGReadBearer))
	want := []string{parentOpen.ID.String(), childOpen.ID.String()}
	slices.Sort(want)
	if got := rcSortedIDs(open.Items); !slices.Equal(got, want) {
		t.Fatalf("open items = %v, want exactly parent-open + child-open %v", got, want)
	}
	for _, it := range open.Items {
		switch it.ID {
		case childOpen.ID.String():
			if it.ChildRunID != child.String() || it.SliceIndex == nil || *it.SliceIndex != 0 || it.RunID != child.String() {
				t.Errorf("child item = %+v, want child_run_id=%s slice_index=0", it, child)
			}
		case parentOpen.ID.String():
			if it.ChildRunID != "" || it.SliceIndex != nil || it.RunID != parent.String() {
				t.Errorf("parent item = %+v, want no child stamp", it)
			}
		}
	}
	if open.Children == nil || len(*open.Children) != 1 || (*open.Children)[0].RunID != child.String() {
		t.Errorf("children = %+v, want only the decomposition child", open.Children)
	}

	all := rcPGDecode(t, f.get(t, parent, "include_children=true&state=all", rcPGReadBearer))
	wantAll := []string{parentOpen.ID.String(), parentWaived.ID.String(), childOpen.ID.String()}
	slices.Sort(wantAll)
	if got := rcSortedIDs(all.Items); !slices.Equal(got, wantAll) {
		t.Fatalf("state=all items = %v, want %v (waived added, recovery still excluded)", got, wantAll)
	}
}

// TestRunConcernsPG_Authz is the C4 observable on the real route: no bearer ->
// 401, a token lacking read:audit -> 403 insufficient_scope.
func TestRunConcernsPG_Authz(t *testing.T) {
	f := newRCPGFixture(t)
	parent, stage := f.seedRun(t, nil, nil, nil)
	f.raise(t, parent, stage, "c")

	if w := f.get(t, parent, "", ""); w.Code != http.StatusUnauthorized || !bodyHasCode(w, "authentication_required") {
		t.Errorf("no bearer: status = %d body = %s, want 401 authentication_required", w.Code, w.Body.String())
	}
	if w := f.get(t, parent, "", rcPGNoReadBearer); w.Code != http.StatusForbidden || !bodyHasCode(w, "insufficient_scope") {
		t.Errorf("no read:audit: status = %d body = %s, want 403 insufficient_scope", w.Code, w.Body.String())
	}
	if w := f.get(t, uuid.New(), "", rcPGReadBearer); w.Code != http.StatusNotFound || !bodyHasCode(w, "run_not_found") {
		t.Errorf("unknown run: status = %d body = %s, want 404 run_not_found", w.Code, w.Body.String())
	}
}
