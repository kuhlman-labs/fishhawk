package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// run_concerns_test.go pins GET /v0/runs/{run_id}/concerns (#4101) over the
// package fakes, one test per branch of handleListRunConcerns. The handler is
// invoked directly with an injected identity (the gate-view idiom), because the
// auth middleware would re-derive identity from the request; the registered
// route through s.Handler() is exercised by run_concerns_pg_test.go and by the
// cross-account case at the bottom of this file.

// childErrConcernRepo wraps fakeConcernRepo and fails ListByRun for ONE run
// id only, so a test can prove a single child's read failure fails the whole
// listing closed while the parent's read succeeds.
type childErrConcernRepo struct {
	*fakeConcernRepo
	failRunID uuid.UUID
}

func (c *childErrConcernRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	if runID == c.failRunID {
		return nil, errors.New("injected child list-by-run error")
	}
	return c.fakeConcernRepo.ListByRun(ctx, runID)
}

func runConcernsServer(t *testing.T) (*Server, *fakeRepo, *fakeConcernRepo) {
	t.Helper()
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: cr})
	return s, repo, cr
}

// seedDecompChild creates a decomposition child of parent (DecomposedFrom AND
// ParentRunID set, as run.ChildParamsFrom does) with the given slice index and
// an explicit CreatedAt, so ordering tests are deterministic.
func seedRCDecompChild(t *testing.T, repo *fakeRepo, parent uuid.UUID, slice int, createdAt time.Time) uuid.UUID {
	t.Helper()
	p := parent
	idx := slice
	child, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "s", TriggerSource: run.TriggerCLI,
		ParentRunID: &p, DecomposedFrom: &p, SliceIndex: &idx,
	})
	if err != nil {
		t.Fatalf("seed decomposition child: %v", err)
	}
	repo.mu.Lock()
	repo.runs[child.ID].CreatedAt = createdAt
	repo.mu.Unlock()
	return child.ID
}

// seedRecoveryChild creates a recovery child: ParentRunID = parent but NO
// DecomposedFrom. It must never appear on an include_children listing.
func seedRCRecoveryChild(t *testing.T, repo *fakeRepo, parent uuid.UUID) uuid.UUID {
	t.Helper()
	p := parent
	child, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "s", TriggerSource: run.TriggerCLI,
		ParentRunID: &p,
	})
	if err != nil {
		t.Fatalf("seed recovery child: %v", err)
	}
	return child.ID
}

func seedRCConcern(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID, note string) *concern.Concern {
	t.Helper()
	return seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindImplement, "model-x", 7, "high", "correctness", note, "")
}

func callRunConcerns(s *Server, runID string, query string, id Identity) *httptest.ResponseRecorder {
	path := "/v0/runs/" + runID + "/concerns"
	if query != "" {
		path += "?" + query
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("run_id", runID)
	req = injectIdentity(req, id)
	s.handleListRunConcerns(w, req)
	return w
}

func decodeRunConcerns(t *testing.T, w *httptest.ResponseRecorder) runConcernListResponse {
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

func rcItemIDs(items []runConcernListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// --- input / configuration failure modes ----------------------------------

func TestListRunConcerns_Unconfigured_503(t *testing.T) {
	repo := newFakeRepo()
	for name, s := range map[string]*Server{
		"no concern repo": New(Config{Addr: "127.0.0.1:0", RunRepo: repo}),
		"no run repo":     New(Config{Addr: "127.0.0.1:0", ConcernRepo: newFakeConcernRepo()}),
	} {
		w := callRunConcerns(s, uuid.NewString(), "", gateViewReadIdentity())
		if w.Code != http.StatusServiceUnavailable || !bodyHasCode(w, "concern_store_unconfigured") {
			t.Errorf("%s: status = %d body = %s, want 503 concern_store_unconfigured", name, w.Code, w.Body.String())
		}
	}
}

func TestListRunConcerns_BadInputs_400(t *testing.T) {
	s, repo, _ := runConcernsServer(t)
	runID := seedGateRun(t, repo).String()
	cases := []struct {
		name, runID, query, field string
	}{
		{"bad run_id", "not-a-uuid", "", "run_id"},
		{"bad state", runID, "state=opne", "state"},
		{"bad include_children", runID, "include_children=maybe", "include_children"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := callRunConcerns(s, tc.runID, tc.query, gateViewReadIdentity())
			if w.Code != http.StatusBadRequest || !bodyHasCode(w, "validation_failed") {
				t.Fatalf("status = %d body = %s, want 400 validation_failed", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"field":"`+tc.field+`"`) {
				t.Errorf("error should name field %q: %s", tc.field, w.Body.String())
			}
		})
	}
}

func TestListRunConcerns_UnknownRun_404(t *testing.T) {
	s, _, _ := runConcernsServer(t)
	w := callRunConcerns(s, uuid.NewString(), "", gateViewReadIdentity())
	if w.Code != http.StatusNotFound || !bodyHasCode(w, "run_not_found") {
		t.Fatalf("status = %d body = %s, want 404 run_not_found JSON error", w.Code, w.Body.String())
	}
}

func TestListRunConcerns_GetRunError_500(t *testing.T) {
	s, repo, _ := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	repo.getErr = errors.New("injected get-run error")
	w := callRunConcerns(s, runID.String(), "", gateViewReadIdentity())
	if w.Code != http.StatusInternalServerError || !bodyHasCode(w, "internal_error") {
		t.Fatalf("status = %d body = %s, want 500 internal_error", w.Code, w.Body.String())
	}
}

// --- state filter ---------------------------------------------------------

// TestListRunConcerns_StateFilter: state=open (and the default) excludes a
// waived row; state=all includes it.
func TestListRunConcerns_StateFilter(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	raised := seedRCConcern(t, cr, runID, "raised concern")
	waived := seedRCConcern(t, cr, runID, "waived concern")
	waived.State = concern.StateWaived

	for _, q := range []string{"", "state=open"} {
		got := decodeRunConcerns(t, callRunConcerns(s, runID.String(), q, gateViewReadIdentity()))
		if ids := rcItemIDs(got.Items); len(ids) != 1 || ids[0] != raised.ID.String() {
			t.Errorf("query %q: items = %v, want only the raised concern %s", q, ids, raised.ID)
		}
		if got.State != runConcernsStateOpen || got.Count != 1 {
			t.Errorf("query %q: state = %q count = %d, want open/1", q, got.State, got.Count)
		}
	}

	all := decodeRunConcerns(t, callRunConcerns(s, runID.String(), "state=all", gateViewReadIdentity()))
	if ids := rcItemIDs(all.Items); len(ids) != 2 || ids[0] != raised.ID.String() || ids[1] != waived.ID.String() {
		t.Errorf("state=all: items = %v, want [raised, waived]", ids)
	}
	if all.State != runConcernsStateAll || all.Count != 2 {
		t.Errorf("state=all: state = %q count = %d, want all/2", all.State, all.Count)
	}
}

// TestListRunConcerns_ItemShape pins the wire fields of one item, including
// the bounded short_summary label (never the full note).
func TestListRunConcerns_ItemShape(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	c := seedGateConcern(t, cr, runID, uuid.New(), concern.StageKindPlan, "model-y", 42, "medium", "scope", gateViewLongNote, "--- a\n+++ b\n")
	c.ReviewerRole = "standard"
	c.Provenance = concern.ProvenanceServerCheck

	w := callRunConcerns(s, runID.String(), "", gateViewReadIdentity())
	got := decodeRunConcerns(t, w)
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want 1", got.Items)
	}
	it := got.Items[0]
	if it.ID != c.ID.String() || it.RunID != runID.String() || it.StageID != c.StageID.String() ||
		it.StageKind != concern.StageKindPlan || it.Severity != "medium" || it.Category != "scope" ||
		it.State != string(concern.StateRaised) || it.ReviewerModel != "model-y" || it.ReviewerRole != "standard" ||
		it.OriginReviewSequence != 42 || !it.HasSuggestedPatch || it.Provenance != concern.ProvenanceServerCheck {
		t.Errorf("item = %+v", it)
	}
	if it.ShortSummary != concernShortSummary(gateViewLongNote) || it.ShortSummary == gateViewLongNote {
		t.Errorf("short_summary = %q, want the bounded label", it.ShortSummary)
	}
	if it.ChildRunID != "" || it.SliceIndex != nil {
		t.Errorf("parent row must not carry child_run_id/slice_index: %+v", it)
	}
	if strings.Contains(w.Body.String(), `"children"`) {
		t.Errorf("children must be absent when include_children=false: %s", w.Body.String())
	}
}

// --- include_children -----------------------------------------------------

// TestListRunConcerns_IncludeChildrenFalse_ParentOnly: a decomposed parent
// without include_children lists only its own rows.
func TestListRunConcerns_IncludeChildrenFalse_ParentOnly(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	parent := seedGateRun(t, repo)
	child := seedRCDecompChild(t, repo, parent, 0, time.Now().UTC())
	pc := seedRCConcern(t, cr, parent, "parent concern")
	seedRCConcern(t, cr, child, "child concern")

	for _, q := range []string{"", "include_children=false"} {
		got := decodeRunConcerns(t, callRunConcerns(s, parent.String(), q, gateViewReadIdentity()))
		if ids := rcItemIDs(got.Items); len(ids) != 1 || ids[0] != pc.ID.String() {
			t.Errorf("query %q: items = %v, want only the parent concern", q, ids)
		}
		if got.IncludeChildren || got.Children != nil {
			t.Errorf("query %q: include_children = %v children = %v, want false/absent", q, got.IncludeChildren, got.Children)
		}
	}
}

// TestListRunConcerns_IncludeChildrenTrue_StampsChildRows: parent rows first,
// then the decomposition child's, with child_run_id + slice_index stamped on
// the child row only, and the child summarized in children[].
func TestListRunConcerns_IncludeChildrenTrue_StampsChildRows(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	parent := seedGateRun(t, repo)
	child := seedRCDecompChild(t, repo, parent, 0, time.Now().UTC())
	pc := seedRCConcern(t, cr, parent, "parent concern")
	cc := seedRCConcern(t, cr, child, "child concern")

	got := decodeRunConcerns(t, callRunConcerns(s, parent.String(), "include_children=true", gateViewReadIdentity()))
	if ids := rcItemIDs(got.Items); len(ids) != 2 || ids[0] != pc.ID.String() || ids[1] != cc.ID.String() {
		t.Fatalf("items = %v, want [parent, child]", ids)
	}
	if p := got.Items[0]; p.ChildRunID != "" || p.SliceIndex != nil || p.RunID != parent.String() {
		t.Errorf("parent row = %+v, want no child stamp", p)
	}
	c := got.Items[1]
	if c.ChildRunID != child.String() || c.SliceIndex == nil || *c.SliceIndex != 0 || c.RunID != child.String() {
		t.Errorf("child row = %+v, want child_run_id=%s slice_index=0", c, child)
	}
	if !got.IncludeChildren || got.Count != 2 {
		t.Errorf("include_children = %v count = %d, want true/2", got.IncludeChildren, got.Count)
	}
	if got.Children == nil || len(*got.Children) != 1 {
		t.Fatalf("children = %v, want one child summary", got.Children)
	}
	if sum := (*got.Children)[0]; sum.RunID != child.String() || sum.SliceIndex == nil || *sum.SliceIndex != 0 ||
		sum.ItemCount != 1 || sum.State != string(run.StatePending) {
		t.Errorf("child summary = %+v", sum)
	}
}

// TestListRunConcerns_RecoveryChildExcluded: a recovery child (ParentRunID set,
// DecomposedFrom nil) is not a decomposition child, so its open concern never
// appears. Paging children by ParentRunID would list it.
func TestListRunConcerns_RecoveryChildExcluded(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	parent := seedGateRun(t, repo)
	recovery := seedRCRecoveryChild(t, repo, parent)
	pc := seedRCConcern(t, cr, parent, "parent concern")
	seedRCConcern(t, cr, recovery, "recovery child concern")

	got := decodeRunConcerns(t, callRunConcerns(s, parent.String(), "include_children=true", gateViewReadIdentity()))
	if ids := rcItemIDs(got.Items); len(ids) != 1 || ids[0] != pc.ID.String() {
		t.Errorf("items = %v, want only the parent concern (recovery child excluded)", ids)
	}
	if got.Children == nil || len(*got.Children) != 0 {
		t.Errorf("children = %v, want [] (a recovery child is not a decomposition child)", got.Children)
	}
}

// TestListRunConcerns_ChildOrdering: children are listed by slice index, not
// store order. The store pages children created_at DESC, so seeding slice 0
// EARLIER than slice 1 makes the unsorted order [1, 0] — only
// sortDecomposedChildren yields [0, 1].
func TestListRunConcerns_ChildOrdering(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	parent := seedGateRun(t, repo)
	base := time.Now().UTC()
	child0 := seedRCDecompChild(t, repo, parent, 0, base)
	child1 := seedRCDecompChild(t, repo, parent, 1, base.Add(time.Second))
	c0 := seedRCConcern(t, cr, child0, "slice 0 concern")
	c1 := seedRCConcern(t, cr, child1, "slice 1 concern")

	got := decodeRunConcerns(t, callRunConcerns(s, parent.String(), "include_children=true", gateViewReadIdentity()))
	if ids := rcItemIDs(got.Items); len(ids) != 2 || ids[0] != c0.ID.String() || ids[1] != c1.ID.String() {
		t.Errorf("items = %v, want [slice 0, slice 1]", ids)
	}
	if got.Children == nil || len(*got.Children) != 2 ||
		(*got.Children)[0].RunID != child0.String() || (*got.Children)[1].RunID != child1.String() {
		t.Errorf("children = %+v, want [slice 0, slice 1]", got.Children)
	}
}

// TestListRunConcerns_NonDecomposed_ChildrenEmptyArray: include_children=true
// on an ordinary run reads children and reports a non-nil [] (present), so a
// caller can tell "read, none" from "not requested".
func TestListRunConcerns_NonDecomposed_ChildrenEmptyArray(t *testing.T) {
	s, repo, _ := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	w := callRunConcerns(s, runID.String(), "include_children=true", gateViewReadIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"children":[]`) || !strings.Contains(body, `"items":[]`) {
		t.Errorf("body = %s, want children:[] and items:[]", body)
	}
}

// --- authorization ----------------------------------------------------------

func TestListRunConcerns_Anonymous_401(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	seedRCConcern(t, cr, runID, "c")
	w := callRunConcerns(s, runID.String(), "", anonIdentity())
	if w.Code != http.StatusUnauthorized || !bodyHasCode(w, "authentication_required") {
		t.Fatalf("status = %d body = %s, want 401 authentication_required", w.Code, w.Body.String())
	}
}

func TestListRunConcerns_MissingReadScope_403(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	seedRCConcern(t, cr, runID, "c")
	noScope := Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"read:runs", "write:runs"}}
	w := callRunConcerns(s, runID.String(), "", noScope)
	if w.Code != http.StatusForbidden || !bodyHasCode(w, "insufficient_scope") {
		t.Fatalf("status = %d body = %s, want 403 insufficient_scope", w.Code, w.Body.String())
	}
}

func TestListRunConcerns_RunBoundCrossRun_403(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	runA := seedGateRun(t, repo)
	runB := seedGateRun(t, repo)
	seedRCConcern(t, cr, runA, "a")
	seedRCConcern(t, cr, runB, "b")
	w := callRunConcerns(s, runB.String(), "", Identity{Subject: "mcp:run:" + runA.String(), TokenID: "tok-mcp"})
	if w.Code != http.StatusForbidden || !bodyHasCode(w, "cross_run_concerns") {
		t.Fatalf("status = %d body = %s, want 403 cross_run_concerns", w.Code, w.Body.String())
	}
}

func TestListRunConcerns_RunBoundOwnRun_200(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	c := seedRCConcern(t, cr, runID, "own")
	got := decodeRunConcerns(t, callRunConcerns(s, runID.String(), "", Identity{Subject: "mcp:run:" + runID.String(), TokenID: "tok-mcp"}))
	if ids := rcItemIDs(got.Items); len(ids) != 1 || ids[0] != c.ID.String() {
		t.Errorf("items = %v, want the run's own concern", ids)
	}
}

func TestListRunConcerns_RunBoundIncludeChildren_403(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	parent := seedGateRun(t, repo)
	child := seedRCDecompChild(t, repo, parent, 0, time.Now().UTC())
	seedRCConcern(t, cr, parent, "parent")
	seedRCConcern(t, cr, child, "child")
	w := callRunConcerns(s, parent.String(), "include_children=true", Identity{Subject: "mcp:run:" + parent.String(), TokenID: "tok-mcp"})
	if w.Code != http.StatusForbidden || !bodyHasCode(w, "cross_run_concerns") {
		t.Fatalf("status = %d body = %s, want 403 cross_run_concerns", w.Code, w.Body.String())
	}
}

func TestListRunConcerns_MalformedMCPSubject_401(t *testing.T) {
	s, repo, _ := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	w := callRunConcerns(s, runID.String(), "", Identity{Subject: "mcp:run:not-a-uuid", TokenID: "tok-mcp"})
	if w.Code != http.StatusUnauthorized || !bodyHasCode(w, "authentication_required") {
		t.Fatalf("status = %d body = %s, want 401 authentication_required", w.Code, w.Body.String())
	}
}

// --- fail-closed store reads ------------------------------------------------

func TestListRunConcerns_ParentListError_500(t *testing.T) {
	s, repo, cr := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	cr.listErr = errors.New("injected list-by-run error")
	w := callRunConcerns(s, runID.String(), "", gateViewReadIdentity())
	if w.Code != http.StatusInternalServerError || !bodyHasCode(w, "internal_error") {
		t.Fatalf("status = %d body = %s, want 500 internal_error", w.Code, w.Body.String())
	}
}

func TestListRunConcerns_ChildrenListError_500(t *testing.T) {
	s, repo, _ := runConcernsServer(t)
	runID := seedGateRun(t, repo)
	repo.listErr = errors.New("injected list-runs error")
	w := callRunConcerns(s, runID.String(), "include_children=true", gateViewReadIdentity())
	if w.Code != http.StatusInternalServerError || !bodyHasCode(w, "internal_error") {
		t.Fatalf("status = %d body = %s, want 500 internal_error", w.Code, w.Body.String())
	}
}

// TestListRunConcerns_ChildOnlyListError_500: the parent's read succeeds and
// one child's fails — the listing fails CLOSED naming that child rather than
// returning the parent's rows alone.
func TestListRunConcerns_ChildOnlyListError_500(t *testing.T) {
	repo := newFakeRepo()
	base := newFakeConcernRepo()
	parent := seedGateRun(t, repo)
	child := seedRCDecompChild(t, repo, parent, 0, time.Now().UTC())
	seedRCConcern(t, base, parent, "parent")
	seedRCConcern(t, base, child, "child")
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: &childErrConcernRepo{fakeConcernRepo: base, failRunID: child}})

	w := callRunConcerns(s, parent.String(), "include_children=true", gateViewReadIdentity())
	if w.Code != http.StatusInternalServerError || !bodyHasCode(w, "internal_error") {
		t.Fatalf("status = %d body = %s, want 500 internal_error", w.Code, w.Body.String())
	}
	// The 5xx redactor strips non-allow-listed detail keys, so the failing
	// child is named in the message.
	if !strings.Contains(w.Body.String(), "decomposition child run "+child.String()) {
		t.Errorf("500 should name the failing child run: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "parent") || strings.Contains(w.Body.String(), `"items"`) {
		t.Errorf("500 must not carry a partial listing: %s", w.Body.String())
	}
}

// TestOpenAPI_RunConcernsRouteDocumented pins the route in the OpenAPI source
// of truth (docs/api/v0.openapi.yaml).
func TestOpenAPI_RunConcernsRouteDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "\n  /v0/runs/{run_id}/concerns:\n")
	if start < 0 {
		t.Fatal("docs/api/v0.openapi.yaml is missing the /v0/runs/{run_id}/concerns path item")
	}
	// Bound the search to this path item so a parameter of another route
	// cannot satisfy it.
	item := doc[start+1:]
	if end := strings.Index(item[1:], "\n  /"); end >= 0 {
		item = item[:end+1]
	}
	for _, want := range []string{
		"operationId: listRunConcerns", "name: state", "name: include_children",
		"enum: [open, all]", "cross_run_concerns", "concern_store_unconfigured",
	} {
		if !strings.Contains(item, want) {
			t.Errorf("the /v0/runs/{run_id}/concerns path item is missing %q", want)
		}
	}
	for _, want := range []string{"\n    RunConcernList:\n", "\n    RunConcernListItem:\n"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing schema %q", strings.TrimSpace(want))
		}
	}
}

// --- cross-account isolation on the REGISTERED route ----------------------

// TestListRunConcerns_Route_CrossAccount_Forbidden pins the account-ownership
// wrapper on GET /v0/runs/{run_id}/concerns (ADR-057 / #1829): the route is
// registered as requireRunAccount(readAccess, handleListRunConcerns), and a
// bearer token bound to account A reading a run owned by account B is 403
// account_forbidden, with and without include_children. It drives the
// REGISTERED route through s.Handler().ServeHTTP with a real bearer the auth
// middleware resolves (the TestHandler_WrapsRunRoute_EnforcesAccount idiom),
// never a directly invoked handler, so the wrapper at the registration site is
// on the path.
//
// include_children has NO account check of its own: children are listed by
// DecomposedFrom under the parent's wrapper decision, so the include_children
// case pins that the one wrapper also gates the child fan-out.
//
// Isolation: both tokens carry read:audit, so insufficient_scope cannot mask
// the deletion, and the same-account control proves the fixture reaches the
// handler (200 with both ids) — with the wrapper gone the cross-account token
// would get that same 200.
//
// Counterfactual (run, fix-up for the operator high concern): replacing
// `s.requireRunAccount(readAccess, s.handleListRunConcerns)` with
// `s.handleListRunConcerns` at the GET /v0/runs/{run_id}/concerns
// registration in handlers.go made this test RED on BOTH cases:
//
//	--- FAIL: TestListRunConcerns_Route_CrossAccount_Forbidden/include_children
//	    status = 200, want 403 account_forbidden; body {..."count":2,"items":[
//	    ...the account-B parent AND child concern ids...]}
//	--- FAIL: TestListRunConcerns_Route_CrossAccount_Forbidden/parent_only
//	    status = 200, want 403 account_forbidden; body {..."count":1,"items":[
//	    ...the account-B parent concern id...]}
//
// handlers.go was then restored byte-identically (cmp against the saved copy
// clean, git diff empty).
func TestListRunConcerns_Route_CrossAccount_Forbidden(t *testing.T) {
	const (
		crossBearer = "fhk_rc_account_a"
		ownerBearer = "fhk_rc_account_b"
	)
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	tokens := &multiTokenRepo{byPlain: map[string]*apitoken.Token{
		crossBearer: {ID: uuid.New(), Subject: "github:op", AccountID: authzAcctA, Scopes: []string{scopeGateViewRead}, PlainText: crossBearer},
		ownerBearer: {ID: uuid.New(), Subject: "github:op", AccountID: authzAcctB, Scopes: []string{scopeGateViewRead}, PlainText: ownerBearer},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: cr, APITokenRepo: tokens})

	parent := seedGateRun(t, repo)
	child := seedRCDecompChild(t, repo, parent, 0, time.Now().UTC())
	repo.mu.Lock()
	repo.runs[parent].AccountID = authzAcctB
	repo.runs[child].AccountID = authzAcctB
	repo.mu.Unlock()
	parentConcern := seedRCConcern(t, cr, parent, "account B parent concern")
	childConcern := seedRCConcern(t, cr, child, "account B child concern")

	get := func(query, bearer string) *httptest.ResponseRecorder {
		path := "/v0/runs/" + parent.String() + "/concerns"
		if query != "" {
			path += "?" + query
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}

	// Positive control: the owning account's token reaches the handler.
	owner := decodeRunConcerns(t, get("include_children=true", ownerBearer))
	if got := rcItemIDs(owner.Items); len(got) != 2 {
		t.Fatalf("owner items = %v, want parent + child concern (fixture must reach the handler)", got)
	}

	for name, query := range map[string]string{
		"parent_only":      "",
		"include_children": "include_children=true",
	} {
		t.Run(name, func(t *testing.T) {
			w := get(query, crossBearer)
			if w.Code != http.StatusForbidden || !bodyHasCode(w, "account_forbidden") {
				t.Fatalf("status = %d, want 403 account_forbidden; body %s", w.Code, w.Body.String())
			}
			for _, id := range []string{parentConcern.ID.String(), childConcern.ID.String()} {
				if strings.Contains(w.Body.String(), id) {
					t.Errorf("403 body leaks concern id %s: %s", id, w.Body.String())
				}
			}
		})
	}
}
