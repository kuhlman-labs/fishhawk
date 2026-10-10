package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- fishhawk_draft_epic (E34.4 / #1595) ---

const testSessionID = "11111111-1111-1111-1111-111111111111"

// refineFakeBackend is a stateful stub for the refinement session endpoints. It
// records each request (method, path, Authorization, raw body) so the per-arm
// wire tests assert the exact contract, and it holds an in-memory session so
// the end-to-end loop test drives real state transitions. overrideStatus /
// overrideBody, when set, short-circuit the NEXT response for a route (the
// failure-mode + drift tests inject a canned status+body) — the request is
// still recorded first.
type refineFakeBackend struct {
	mu         sync.Mutex
	calls      int
	lastMethod string
	lastPath   string
	lastAuth   string
	lastBody   []byte

	overrideStatus int
	overrideBody   string

	// in-memory session state (single session; tests use one)
	state         string
	drifted       bool
	revisionCount int
	latestOrigin  string
	latestDraft   json.RawMessage
	decisions     []map[string]any

	// flaggedOrdinal, when non-zero, marks that 1-based child as
	// needs_attention in criteria_precheck (the E34.5 advisory flag); 0 means a
	// clean, checked-and-clean pre-check over two children.
	flaggedOrdinal int

	// filing, when non-nil, is the session view's `filing` block (#4153). The
	// file route sets it in_progress (the detached launch); completeFiling
	// flips it to filed, as the backend's detached goroutine would.
	filing map[string]any
}

// completeFiling simulates the backend's detached filing finishing: the
// filing block goes filed with the epic + both children recorded, and the
// session's top-level state becomes filed.
func (fb *refineFakeBackend) completeFiling(repo string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.state = "filed"
	fb.filing = map[string]any{
		"state": "filed", "repo": repo, "in_flight": false, "child_count": 2, "filed_count": 3,
		"epic": map[string]any{"number": 2000, "url": "https://github.com/" + repo + "/issues/2000"},
		"children": []any{
			map[string]any{"ordinal": 1, "number": 2001, "url": "https://github.com/" + repo + "/issues/2001"},
			map[string]any{"ordinal": 2, "number": 2002, "url": "https://github.com/" + repo + "/issues/2002"},
		},
		"started_at":   "2026-10-09T00:00:00Z",
		"completed_at": "2026-10-09T00:01:00Z",
	}
}

func defaultDraftJSON() json.RawMessage {
	return json.RawMessage(`{
		"epic": {"summary": "E: widgets", "scope": "in scope", "out_of_scope": "out of scope"},
		"children": [
			{"summary": "c1", "proposal": "p1", "done_means": "d1", "acceptance_criteria": ["a1"], "labels": ["area:backend"], "depends_on": []},
			{"summary": "c2", "proposal": "p2", "done_means": "d2", "acceptance_criteria": ["a2"], "labels": ["area:backend"], "depends_on": [1]}
		]
	}`)
}

func (fb *refineFakeBackend) sessionViewJSON() []byte {
	m := map[string]any{
		"session_id":     testSessionID,
		"state":          fb.state,
		"revision_count": fb.revisionCount,
		"latest_origin":  fb.latestOrigin,
		"latest_draft":   fb.latestDraft,
		"preview": []any{
			map[string]any{"kind": "epic", "title": "E: widgets"},
			map[string]any{"kind": "child", "title": "c1"},
		},
		"waves":             []any{[]any{1}, []any{2}},
		"criteria_precheck": fb.criteriaPrecheckJSON(),
		"decisions":         fb.decisions,
	}
	if fb.drifted {
		m["drifted"] = true
	}
	if fb.filing != nil {
		m["filing"] = fb.filing
	}
	b, _ := json.Marshal(m)
	return b
}

// criteriaPrecheckJSON builds the criteria_precheck sub-object for the session
// view over two children, marking fb.flaggedOrdinal (when non-zero) as
// needs_attention with a no_blocking_criterion finding.
func (fb *refineFakeBackend) criteriaPrecheckJSON() map[string]any {
	children := make([]any, 0, 2)
	for _, ord := range []int{1, 2} {
		child := map[string]any{"ordinal": ord, "findings": []any{}}
		if ord == fb.flaggedOrdinal {
			child["needs_attention"] = true
			child["findings"] = []any{
				map[string]any{"rule": "no_blocking_criterion", "detail": "no blocking acceptance criterion"},
			}
		}
		children = append(children, child)
	}
	return map[string]any{
		"needs_attention": fb.flaggedOrdinal != 0,
		"children":        children,
	}
}

func (fb *refineFakeBackend) record(r *http.Request) []byte {
	body, _ := readAllBody(r)
	fb.calls++
	fb.lastMethod = r.Method
	fb.lastPath = r.URL.Path
	fb.lastAuth = r.Header.Get("Authorization")
	fb.lastBody = body
	return body
}

// maybeOverride writes the canned status+body when set and reports whether it
// handled the response.
func (fb *refineFakeBackend) maybeOverride(w http.ResponseWriter) bool {
	if fb.overrideStatus == 0 {
		return false
	}
	w.WriteHeader(fb.overrideStatus)
	_, _ = w.Write([]byte(fb.overrideBody))
	return true
}

func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	var buf strings.Builder
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	buf.Write(raw)
	return []byte(buf.String()), nil
}

func newRefineFakeBackend(t *testing.T) (*refineFakeBackend, *httptest.Server) {
	fb := &refineFakeBackend{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v0/refinement/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.record(r)
		if fb.maybeOverride(w) {
			return
		}
		fb.state = "awaiting_approval"
		fb.drifted = false
		fb.revisionCount = 1
		fb.latestOrigin = "brief"
		fb.latestDraft = defaultDraftJSON()
		fb.decisions = nil
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(fb.sessionViewJSON())
	})

	mux.HandleFunc("GET /v0/refinement/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.record(r)
		if fb.maybeOverride(w) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fb.sessionViewJSON())
	})

	mux.HandleFunc("PATCH /v0/refinement/sessions/{id}/draft", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		defer fb.mu.Unlock()
		body := fb.record(r)
		if fb.maybeOverride(w) {
			return
		}
		var req struct {
			BriefAmendment string          `json:"brief_amendment"`
			Draft          json.RawMessage `json:"draft"`
		}
		_ = json.Unmarshal(body, &req)
		fb.revisionCount++
		fb.state = "awaiting_approval"
		fb.drifted = false
		fb.decisions = nil // an edit re-gates the session
		if req.Draft != nil {
			fb.latestOrigin = "edit"
			fb.latestDraft = req.Draft // echo the submitted draft
		} else {
			fb.latestOrigin = "amendment"
			fb.latestDraft = defaultDraftJSON()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fb.sessionViewJSON())
	})

	mux.HandleFunc("POST /v0/refinement/sessions/{id}/decision", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		defer fb.mu.Unlock()
		body := fb.record(r)
		if fb.maybeOverride(w) {
			return
		}
		var req struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		_ = json.Unmarshal(body, &req)
		fb.state = req.Decision
		fb.decisions = append(fb.decisions, map[string]any{
			"decision":           req.Decision,
			"reason":             req.Reason,
			"draft_id":           "22222222-2222-2222-2222-222222222222",
			"draft_content_hash": "sha256:abc",
			"created_at":         "2026-07-03T00:00:00Z",
		})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fb.sessionViewJSON())
	})

	mux.HandleFunc("POST /v0/refinement/sessions/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		defer fb.mu.Unlock()
		body := fb.record(r)
		if fb.maybeOverride(w) {
			return
		}
		var req struct {
			Repo string `json:"repo"`
		}
		_ = json.Unmarshal(body, &req)
		// The detached file arm (#4153): the launch returns 202 at once with
		// nothing filed yet, and progress moves onto the session view.
		fb.filing = map[string]any{
			"state": "in_progress", "repo": req.Repo, "in_flight": true,
			"child_count": 2, "filed_count": 0, "children": []any{},
			"started_at": "2026-10-09T00:00:00Z",
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{
			"status": "filing_in_progress",
			"session_id": "` + testSessionID + `",
			"draft_id": "22222222-2222-2222-2222-222222222222",
			"repo": "` + req.Repo + `",
			"child_count": 2,
			"budget_seconds": 300,
			"already_in_progress": false,
			"children": [],
			"resumed": false,
			"already_completed": false,
			"verified": false
		}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

// --- per-arm wire tests (criterion arms-map-one-to-one) ---

func TestDraftEpic_OpenArm_WiresCreate(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{Brief: "build widgets"})
	if err != nil {
		t.Fatalf("draftEpic open: %v", err)
	}
	if fb.lastMethod != http.MethodPost || fb.lastPath != "/v0/refinement/sessions" {
		t.Errorf("wire = %s %s, want POST /v0/refinement/sessions", fb.lastMethod, fb.lastPath)
	}
	var reqBody map[string]any
	_ = json.Unmarshal(fb.lastBody, &reqBody)
	if reqBody["brief"] != "build widgets" {
		t.Errorf("request brief = %v, want 'build widgets'", reqBody["brief"])
	}
	if out.Session == nil || out.Session.SessionID != testSessionID {
		t.Fatalf("out.Session = %+v, want session_id %s", out.Session, testSessionID)
	}
	if out.Session.State != "awaiting_approval" || out.Session.LatestOrigin != "brief" {
		t.Errorf("state/origin = %q/%q", out.Session.State, out.Session.LatestOrigin)
	}
	// The RefinementSession fields round-trip into the output.
	if out.Session.RevisionCount != 1 || len(out.Session.Preview) != 2 || len(out.Session.Waves) != 2 {
		t.Errorf("revision_count/preview/waves = %d/%d/%d", out.Session.RevisionCount, len(out.Session.Preview), len(out.Session.Waves))
	}
	if len(out.Session.LatestDraft.Children) != 2 {
		t.Errorf("latest_draft children = %d, want 2", len(out.Session.LatestDraft.Children))
	}
	if got := out.Session.LatestDraft.Children[1].DependsOn; len(got) != 1 || got[0] != 1 {
		t.Errorf("child[1].depends_on = %v, want [1]", got)
	}
	// criteria_precheck round-trips into the mirror; a clean draft is
	// checked-and-clean (needs_attention false, per-child findings []).
	if out.Session.CriteriaPrecheck.NeedsAttention {
		t.Errorf("clean draft criteria_precheck.needs_attention = true, want false")
	}
	if len(out.Session.CriteriaPrecheck.Children) != 2 {
		t.Errorf("criteria_precheck children = %d, want 2", len(out.Session.CriteriaPrecheck.Children))
	}
	if len(out.SessionGuidance) == 0 || out.SessionGuidance[0].Arm != "decide" {
		t.Errorf("guidance = %+v, want first arm 'decide'", out.SessionGuidance)
	}
}

// TestDraftEpic_CriteriaPrecheck_GuidanceNamesFlaggedOrdinal round-trips a
// flagged view through the tool: criteria_precheck decodes into the mirror and
// the awaiting_approval decide guidance names the flagged child ordinal while
// confirming approval remains legal.
func TestDraftEpic_CriteriaPrecheck_GuidanceNamesFlaggedOrdinal(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.flaggedOrdinal = 2 // child 2 flagged no_blocking_criterion
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{Brief: "build widgets"})
	if err != nil {
		t.Fatalf("draftEpic open: %v", err)
	}
	if !out.Session.CriteriaPrecheck.NeedsAttention {
		t.Fatalf("criteria_precheck.needs_attention must decode true; got %+v", out.Session.CriteriaPrecheck)
	}
	flagged := out.Session.CriteriaPrecheck.Children[1]
	if flagged.Ordinal != 2 || !flagged.NeedsAttention {
		t.Fatalf("child 2 must be flagged in the mirror; got %+v", flagged)
	}
	if len(out.SessionGuidance) == 0 || out.SessionGuidance[0].Arm != "decide" {
		t.Fatalf("first guidance arm = %+v, want decide", out.SessionGuidance)
	}
	reason := out.SessionGuidance[0].Reason
	if !strings.Contains(reason, "child 2") {
		t.Errorf("decide guidance reason must name the flagged child 2; got %q", reason)
	}
	if !strings.Contains(reason, "still legal") {
		t.Errorf("decide guidance reason must state approval is still legal (advisory); got %q", reason)
	}
}

func TestDraftEpic_PreviewArm_WiresGet(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.state = "awaiting_approval"
	fb.revisionCount = 1
	fb.latestOrigin = "brief"
	fb.latestDraft = defaultDraftJSON()
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID})
	if err != nil {
		t.Fatalf("draftEpic preview: %v", err)
	}
	if fb.lastMethod != http.MethodGet || fb.lastPath != "/v0/refinement/sessions/"+testSessionID {
		t.Errorf("wire = %s %s, want GET /v0/refinement/sessions/{id}", fb.lastMethod, fb.lastPath)
	}
	if len(fb.lastBody) != 0 {
		t.Errorf("GET carried a body: %s", fb.lastBody)
	}
	if out.Session == nil {
		t.Fatal("out.Session is nil")
	}
}

func TestDraftEpic_EditArm_BriefAmendment_WiresPatch(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{
		SessionID:      testSessionID,
		BriefAmendment: "also add a metrics child",
	})
	if err != nil {
		t.Fatalf("draftEpic edit(amendment): %v", err)
	}
	if fb.lastMethod != http.MethodPatch || fb.lastPath != "/v0/refinement/sessions/"+testSessionID+"/draft" {
		t.Errorf("wire = %s %s, want PATCH /v0/refinement/sessions/{id}/draft", fb.lastMethod, fb.lastPath)
	}
	var reqBody map[string]any
	_ = json.Unmarshal(fb.lastBody, &reqBody)
	if reqBody["brief_amendment"] != "also add a metrics child" {
		t.Errorf("brief_amendment = %v", reqBody["brief_amendment"])
	}
	// XOR contract: only the amendment arm is serialized, never draft.
	if _, present := reqBody["draft"]; present {
		t.Errorf("PATCH body carried both arms: %s", fb.lastBody)
	}
	if out.Session == nil || out.Session.LatestOrigin != "amendment" {
		t.Errorf("latest_origin = %+v, want amendment", out.Session)
	}
}

func TestDraftEpic_EditArm_Draft_RoundTripsDependsOn(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	draft := &EpicDraft{
		Epic: EpicDraftEpic{Summary: "E", Scope: "s", OutOfScope: "o"},
		Children: []EpicDraftChild{
			{Summary: "one", Proposal: "p", DoneMeans: "d", AcceptanceCriteria: []string{"a"}, Labels: []string{"area:backend"}, DependsOn: []int{}},
			{Summary: "two", Proposal: "p", DoneMeans: "d", AcceptanceCriteria: []string{"a"}, Labels: []string{"area:backend"}, DependsOn: []int{1, 2}},
		},
	}
	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, Draft: draft})
	if err != nil {
		t.Fatalf("draftEpic edit(draft): %v", err)
	}
	if fb.lastMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", fb.lastMethod)
	}
	// The full EpicDraft serialized under the draft arm, XOR the amendment arm.
	var reqBody struct {
		BriefAmendment string     `json:"brief_amendment"`
		Draft          *EpicDraft `json:"draft"`
	}
	if err := json.Unmarshal(fb.lastBody, &reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if reqBody.BriefAmendment != "" {
		t.Errorf("brief_amendment set on a draft edit: %q", reqBody.BriefAmendment)
	}
	if reqBody.Draft == nil || len(reqBody.Draft.Children) != 2 {
		t.Fatalf("draft not serialized: %s", fb.lastBody)
	}
	// depends_on ordinals survive the serialize->wire->decode round trip both
	// on the request and back through the echoed latest_draft (struct-drift guard
	// against the strict-decoding server).
	if got := reqBody.Draft.Children[1].DependsOn; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("request child[1].depends_on = %v, want [1 2]", got)
	}
	if out.Session == nil || out.Session.LatestOrigin != "edit" {
		t.Fatalf("latest_origin = %+v, want edit", out.Session)
	}
	if got := out.Session.LatestDraft.Children[1].DependsOn; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("echoed latest_draft child[1].depends_on = %v, want [1 2]", got)
	}
}

func TestDraftEpic_DecideArm_WiresDecision(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{
		SessionID: testSessionID,
		Decision:  "approved",
		Reason:    "scope looks right",
	})
	if err != nil {
		t.Fatalf("draftEpic decide: %v", err)
	}
	if fb.lastMethod != http.MethodPost || fb.lastPath != "/v0/refinement/sessions/"+testSessionID+"/decision" {
		t.Errorf("wire = %s %s, want POST .../decision", fb.lastMethod, fb.lastPath)
	}
	var reqBody map[string]any
	_ = json.Unmarshal(fb.lastBody, &reqBody)
	if reqBody["decision"] != "approved" || reqBody["reason"] != "scope looks right" {
		t.Errorf("decision/reason = %v/%v", reqBody["decision"], reqBody["reason"])
	}
	if out.Session == nil || out.Session.State != "approved" {
		t.Fatalf("state = %+v, want approved", out.Session)
	}
	if len(out.SessionGuidance) == 0 || out.SessionGuidance[0].Arm != "file" {
		t.Errorf("guidance = %+v, want first arm 'file'", out.SessionGuidance)
	}
}

func TestDraftEpic_FileArm_WiresFile(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{
		SessionID: testSessionID,
		Repo:      "kuhlman-labs/fishhawk",
	})
	if err != nil {
		t.Fatalf("draftEpic file: %v", err)
	}
	if fb.lastMethod != http.MethodPost || fb.lastPath != "/v0/refinement/sessions/"+testSessionID+"/file" {
		t.Errorf("wire = %s %s, want POST .../file", fb.lastMethod, fb.lastPath)
	}
	var reqBody map[string]any
	_ = json.Unmarshal(fb.lastBody, &reqBody)
	if reqBody["repo"] != "kuhlman-labs/fishhawk" {
		t.Errorf("repo = %v", reqBody["repo"])
	}
	if out.Filing == nil {
		t.Fatal("out.Filing is nil")
	}
	// The 202 launch body decodes into the mirror: status, child count and
	// budget present, nothing filed yet (no epic, no children).
	if out.Filing.Status != "filing_in_progress" || out.Filing.AlreadyInProgress {
		t.Errorf("status/already_in_progress = %q/%v, want filing_in_progress/false", out.Filing.Status, out.Filing.AlreadyInProgress)
	}
	if out.Filing.ChildCount != 2 || out.Filing.BudgetSeconds != 300 {
		t.Errorf("child_count/budget_seconds = %d/%d, want 2/300", out.Filing.ChildCount, out.Filing.BudgetSeconds)
	}
	if out.Filing.Epic != nil || len(out.Filing.Children) != 0 {
		t.Errorf("a fresh launch has filed nothing yet; filing = %+v", out.Filing)
	}
	// The filing is detached: the next move is the preview arm, never terminal.
	if len(out.SessionGuidance) != 1 {
		t.Fatalf("guidance = %+v, want exactly one entry", out.SessionGuidance)
	}
	g := out.SessionGuidance[0]
	if g.Arm != "preview" || g.State != "filing_in_progress" || g.Arguments["session_id"] != testSessionID {
		t.Errorf("guidance = %+v, want filing_in_progress -> preview(session_id)", g)
	}
	if !strings.Contains(g.Reason, "kuhlman-labs/fishhawk") || !strings.Contains(g.Reason, "300s") {
		t.Errorf("guidance reason = %q, want the repo and the budget named", g.Reason)
	}
}

// TestDraftEpic_FileArm_AlreadyInProgress_GuidancePreview: a concurrent file
// call (202 already_in_progress) launched nothing, so the next move is the
// preview arm. Counterfactual: delete the in-progress branch of
// guidanceForFiling -> terminal guidance -> RED.
func TestDraftEpic_FileArm_AlreadyInProgress_GuidancePreview(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusAccepted
	fb.overrideBody = `{
		"status": "already_in_progress",
		"session_id": "` + testSessionID + `",
		"draft_id": "22222222-2222-2222-2222-222222222222",
		"repo": "kuhlman-labs/fishhawk",
		"child_count": 2,
		"budget_seconds": 300,
		"already_in_progress": true,
		"epic": {"number": 2000, "url": "https://github.com/kuhlman-labs/fishhawk/issues/2000"},
		"children": [],
		"resumed": true,
		"already_completed": false,
		"verified": false
	}`
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("file(already_in_progress): %v", err)
	}
	if out.Filing == nil || !out.Filing.AlreadyInProgress || out.Filing.Status != "already_in_progress" {
		t.Fatalf("filing = %+v, want already_in_progress", out.Filing)
	}
	if out.Filing.Epic == nil || out.Filing.Epic.Number != 2000 {
		t.Errorf("the filed-so-far epic must decode; got %+v", out.Filing.Epic)
	}
	if len(out.SessionGuidance) != 1 || out.SessionGuidance[0].Arm != "preview" || out.SessionGuidance[0].Arguments["session_id"] != testSessionID {
		t.Fatalf("guidance = %+v, want preview(session_id)", out.SessionGuidance)
	}
	if !strings.Contains(out.SessionGuidance[0].Reason, "already running") {
		t.Errorf("guidance reason = %q, want it to say a filing is already running", out.SessionGuidance[0].Reason)
	}
}

// TestDraftEpic_FileArm_StatuslessBody_Terminal pins the rollback posture: a
// pre-#4153 backend's synchronous 200 carries no status and means the session
// filed, so the guidance stays terminal (the plan's rollback note).
func TestDraftEpic_FileArm_StatuslessBody_Terminal(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusOK
	fb.overrideBody = `{
		"session_id": "` + testSessionID + `",
		"draft_id": "22222222-2222-2222-2222-222222222222",
		"repo": "kuhlman-labs/fishhawk",
		"epic": {"number": 2000, "url": "https://github.com/kuhlman-labs/fishhawk/issues/2000"},
		"children": [{"ordinal": 1, "number": 2001, "url": "u1"}],
		"resumed": false,
		"already_completed": false,
		"verified": true
	}`
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("file(statusless 200): %v", err)
	}
	if len(out.SessionGuidance) != 1 || out.SessionGuidance[0].Arm != "terminal" {
		t.Fatalf("guidance = %+v, want terminal", out.SessionGuidance)
	}
	args := out.SessionGuidance[0].Arguments
	if args["epic"] != "#2000 https://github.com/kuhlman-labs/fishhawk/issues/2000" || args["child_1"] != "#2001 u1" {
		t.Errorf("terminal arguments = %v, want the filed coordinates", args)
	}
}

// --- done-means behavioral test: the stateful session loop with guidance
// asserted at EVERY transition (criterion session-guidance-correct-arm) ---

func TestDraftEpic_SessionLoop(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)
	ctx := context.Background()

	firstArm := func(out DraftEpicOutput) string {
		if len(out.SessionGuidance) == 0 {
			return ""
		}
		return out.SessionGuidance[0].Arm
	}

	// 1. open -> awaiting_approval -> guidance: decide
	_, out, err := r.draftEpic(ctx, nil, DraftEpicInput{Brief: "build widgets"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sid := out.Session.SessionID
	if out.Session.State != "awaiting_approval" || firstArm(out) != "decide" {
		t.Fatalf("after open: state=%q arm=%q, want awaiting_approval/decide", out.Session.State, firstArm(out))
	}

	// 2. preview -> awaiting_approval -> guidance: decide
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sid})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if firstArm(out) != "decide" {
		t.Fatalf("after preview: arm=%q, want decide", firstArm(out))
	}

	// 3. direct draft edit -> re-gates to awaiting_approval -> guidance: decide
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{
		SessionID: sid,
		Draft: &EpicDraft{
			Epic:     EpicDraftEpic{Summary: "E", Scope: "s", OutOfScope: "o"},
			Children: []EpicDraftChild{{Summary: "c", Proposal: "p", DoneMeans: "d", AcceptanceCriteria: []string{"a"}, Labels: []string{"x"}, DependsOn: []int{}}},
		},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if out.Session.LatestOrigin != "edit" || firstArm(out) != "decide" {
		t.Fatalf("after edit: origin=%q arm=%q, want edit/decide", out.Session.LatestOrigin, firstArm(out))
	}

	// 4. reject -> rejected -> guidance: edit (re-draft)
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sid, Decision: "rejected", Reason: "too broad"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if out.Session.State != "rejected" || firstArm(out) != "edit" {
		t.Fatalf("after reject: state=%q arm=%q, want rejected/edit", out.Session.State, firstArm(out))
	}

	// 5. brief_amendment re-draft -> re-gates to awaiting_approval -> guidance: decide
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sid, BriefAmendment: "narrow the scope"})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if out.Session.LatestOrigin != "amendment" || out.Session.State != "awaiting_approval" || firstArm(out) != "decide" {
		t.Fatalf("after amend: origin=%q state=%q arm=%q", out.Session.LatestOrigin, out.Session.State, firstArm(out))
	}

	// 6. approve -> approved -> guidance: file
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sid, Decision: "approved", Reason: "good now"})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out.Session.State != "approved" || firstArm(out) != "file" {
		t.Fatalf("after approve: state=%q arm=%q, want approved/file", out.Session.State, firstArm(out))
	}

	// 7. file -> 202 filing_in_progress -> guidance: preview (detached, #4153)
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sid, Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if out.Filing == nil || out.Filing.Status != "filing_in_progress" || firstArm(out) != "preview" {
		t.Fatalf("after file: filing=%+v arm=%q, want filing_in_progress/preview", out.Filing, firstArm(out))
	}

	// 8. preview while the filing runs -> filing in_progress -> guidance: preview
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sid})
	if err != nil {
		t.Fatalf("preview(in progress): %v", err)
	}
	if out.Session.Filing == nil || out.Session.Filing.State != "in_progress" || firstArm(out) != "preview" {
		t.Fatalf("while filing: filing=%+v arm=%q, want in_progress/preview", out.Session.Filing, firstArm(out))
	}

	// 9. the detached filing completes -> preview -> state filed -> terminal
	fb.completeFiling("kuhlman-labs/fishhawk")
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sid})
	if err != nil {
		t.Fatalf("preview(filed): %v", err)
	}
	if out.Session.State != "filed" || firstArm(out) != "terminal" {
		t.Fatalf("after completion: state=%q arm=%q, want filed/terminal", out.Session.State, firstArm(out))
	}
}

// --- arm-dispatch fail-closed tests (criterion arm-dispatch-fails-closed) ---

func TestDraftEpic_ZeroArms_FailsClosedNoHTTP(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{})
	if err == nil {
		t.Fatal("want a fail-closed error on zero arms")
	}
	if !strings.Contains(err.Error(), "exactly one arm") {
		t.Errorf("err = %v, want the legal-arms enumeration", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestDraftEpic_IllegalCombo_BriefAndDecision_FailsClosedNoHTTP(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{Brief: "x", Decision: "approved", Reason: "y"})
	if err == nil || !strings.Contains(err.Error(), "open arm") {
		t.Fatalf("err = %v, want illegal-open-combo error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestDraftEpic_EditBothArms_FailsClosedNoHTTP(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{
		SessionID:      testSessionID,
		BriefAmendment: "a",
		Draft:          &EpicDraft{Epic: EpicDraftEpic{Summary: "E"}},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one of brief_amendment or draft") {
		t.Fatalf("err = %v, want both-arms error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestDraftEpic_MultipleSubArms_FailsClosedNoHTTP(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	// decide + file arms both populated alongside session_id.
	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{
		SessionID: testSessionID,
		Decision:  "approved",
		Reason:    "y",
		Repo:      "o/n",
	})
	if err == nil || !strings.Contains(err.Error(), "only ONE of") {
		t.Fatalf("err = %v, want single-sub-arm error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestDraftEpic_DecideArm_InvalidDecision_FailsClosedNoHTTP(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{
		SessionID: testSessionID,
		Decision:  "maybe",
		Reason:    "y",
	})
	if err == nil || !strings.Contains(err.Error(), "approved or rejected") {
		t.Fatalf("err = %v, want invalid-decision error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestDraftEpic_DecideArm_MissingReason_FailsClosedNoHTTP(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{
		SessionID: testSessionID,
		Decision:  "approved",
	})
	if err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("err = %v, want missing-reason error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestDraftEpic_InvalidSessionID_FailsClosedNoHTTP(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want invalid-UUID error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

// --- per-failure-mode tests (one behavioral assertion per mode) ---

func TestDraftEpic_DecisionAlreadyRecorded_SurfacesRegateGuidance(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusConflict
	fb.overrideBody = `{"error":{"code":"decision_already_recorded","message":"the latest revision already carries a decision"}}`
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, Decision: "approved", Reason: "y"})
	if err == nil {
		t.Fatal("want an error on 409 decision_already_recorded")
	}
	// verbatim code + re-gate-by-editing guidance.
	if !strings.Contains(err.Error(), "decision_already_recorded") || !strings.Contains(err.Error(), "re-gate by EDITING") {
		t.Errorf("err = %v, want verbatim code + re-gate guidance", err)
	}
}

func TestDraftEpic_AmendmentBudgetExhausted_SurfacesDirectEditAlternative(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusConflict
	fb.overrideBody = `{"error":{"code":"amendment_budget_exhausted","message":"the per-session brief-amendment budget is spent"}}`
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, BriefAmendment: "one more"})
	if err == nil {
		t.Fatal("want an error on 409 amendment_budget_exhausted")
	}
	if !strings.Contains(err.Error(), "amendment_budget_exhausted") || !strings.Contains(err.Error(), "direct draft edit") {
		t.Errorf("err = %v, want verbatim code + direct-draft-edit alternative", err)
	}
}

func TestDraftEpic_RefinementNotApproved_PrematureFile(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusConflict
	fb.overrideBody = `{"error":{"code":"refinement_not_approved","message":"the draft is not approved"}}`
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, Repo: "o/n"})
	if err == nil {
		t.Fatal("want an error on 409 refinement_not_approved")
	}
	if !strings.Contains(err.Error(), "refinement_not_approved") || !strings.Contains(err.Error(), "approve the latest revision first") {
		t.Errorf("err = %v, want verbatim code + approve-first guidance", err)
	}
}

func TestDraftEpic_Drifted_GuidanceReDecide(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusOK
	fb.overrideBody = `{
		"session_id": "` + testSessionID + `",
		"state": "awaiting_approval",
		"drifted": true,
		"revision_count": 2,
		"latest_origin": "edit",
		"latest_draft": {"epic":{"summary":"E","scope":"s","out_of_scope":"o"},"children":[{"summary":"c","proposal":"p","done_means":"d","acceptance_criteria":["a"],"labels":["x"],"depends_on":[]}]},
		"preview": [{"kind":"epic"}],
		"waves": [[1]],
		"decisions": [{"decision":"approved","reason":"stale","draft_id":"22222222-2222-2222-2222-222222222222","draft_content_hash":"sha256:old","created_at":"2026-07-03T00:00:00Z"}]
	}`
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID})
	if err != nil {
		t.Fatalf("preview(drifted): %v", err)
	}
	if out.Session == nil || !out.Session.Drifted {
		t.Fatalf("out.Session drifted = %+v, want drifted true", out.Session)
	}
	if len(out.SessionGuidance) == 0 || out.SessionGuidance[0].State != "drifted" || out.SessionGuidance[0].Arm != "decide" {
		t.Fatalf("guidance = %+v, want drifted->decide", out.SessionGuidance)
	}
	if !strings.Contains(out.SessionGuidance[0].Reason, "re-decide the latest revision") {
		t.Errorf("guidance reason = %q, want re-decide-the-latest-revision", out.SessionGuidance[0].Reason)
	}
}

func TestDraftEpic_FileAlreadyCompleted_TerminalGuidance(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusOK
	fb.overrideBody = `{
		"session_id": "` + testSessionID + `",
		"draft_id": "22222222-2222-2222-2222-222222222222",
		"repo": "kuhlman-labs/fishhawk",
		"epic": {"number": 2000, "url": "https://github.com/kuhlman-labs/fishhawk/issues/2000"},
		"children": [{"ordinal": 1, "number": 2001, "url": "u"}],
		"resumed": false,
		"already_completed": true,
		"verified": false
	}`
	r := newResolver(srv, nil)

	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("file(already_completed): %v", err)
	}
	if out.Filing == nil || !out.Filing.AlreadyCompleted {
		t.Fatalf("filing = %+v, want already_completed", out.Filing)
	}
	if len(out.SessionGuidance) == 0 || out.SessionGuidance[0].Arm != "terminal" {
		t.Fatalf("guidance = %+v, want terminal", out.SessionGuidance)
	}
	// No re-file suggestion on a completed replay.
	if strings.Contains(strings.ToLower(out.SessionGuidance[0].Reason), "re-invoke") {
		t.Errorf("guidance reason suggests re-file on a completed replay: %q", out.SessionGuidance[0].Reason)
	}
}

// TestDraftEpic_FilingLaunchRefused_ReinvokeSameRepo: since the file arm
// detached (#4153) a 502 refinement_filing_failed is a synchronous refusal to
// START (the installation could not be resolved) — nothing was launched — so
// the error says so and names the re-invoke with the same repo, surfacing the
// backend code and details verbatim.
func TestDraftEpic_FilingLaunchRefused_ReinvokeSameRepo(t *testing.T) {
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusBadGateway
	fb.overrideBody = `{"error":{"code":"refinement_filing_failed","message":"could not resolve the GitHub App installation for the target repo","details":{"error_ref":"ref-123"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID, Repo: "kuhlman-labs/fishhawk"})
	if err == nil {
		t.Fatal("want an error on 502 refinement_filing_failed")
	}
	if !strings.Contains(err.Error(), "refinement_filing_failed") || !strings.Contains(err.Error(), "ref-123") {
		t.Errorf("err = %v, want the verbatim code and details", err)
	}
	if !strings.Contains(err.Error(), "nothing was launched") {
		t.Errorf("err = %v, want it to say nothing was launched", err)
	}
	if !strings.Contains(err.Error(), "re-invoke the file arm with the SAME repo") || !strings.Contains(err.Error(), "kuhlman-labs/fishhawk") {
		t.Errorf("err = %v, want re-invoke-same-repo guidance naming the repo", err)
	}
}

// --- preview-arm guidance over the filing block (#4153): one test per
// branch, each a deletion counterfactual of its case ---

// approvedSessionWithFiling builds a preview body for an approved session
// carrying filing block `filing` (raw JSON), and optionally drifted.
func approvedSessionWithFiling(state, filing string, drifted bool) string {
	d := ""
	if drifted {
		d = `"drifted": true,`
	}
	return `{
		"session_id": "` + testSessionID + `",
		"state": "` + state + `",
		` + d + `
		"revision_count": 1,
		"latest_origin": "brief",
		"latest_draft": ` + string(defaultDraftJSON()) + `,
		"preview": [],
		"waves": [[1],[2]],
		"criteria_precheck": {"needs_attention": false, "children": []},
		"decisions": [],
		"filing": ` + filing + `
	}`
}

func previewWith(t *testing.T, body string) DraftEpicOutput {
	t.Helper()
	fb, srv := newRefineFakeBackend(t)
	fb.overrideStatus = http.StatusOK
	fb.overrideBody = body
	r := newResolver(srv, nil)
	_, out, err := r.draftEpic(context.Background(), nil, DraftEpicInput{SessionID: testSessionID})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(out.SessionGuidance) != 1 {
		t.Fatalf("guidance = %+v, want exactly one entry", out.SessionGuidance)
	}
	return out
}

// TestDraftEpic_Preview_FilingInProgress_GuidancePreview: a running detached
// filing routes to the preview arm, not the file arm the approved state
// would otherwise name. Counterfactual: delete the in_progress case -> the
// approved -> file guidance -> RED.
func TestDraftEpic_Preview_FilingInProgress_GuidancePreview(t *testing.T) {
	out := previewWith(t, approvedSessionWithFiling("approved", `{
		"state": "in_progress", "repo": "kuhlman-labs/fishhawk", "in_flight": true,
		"child_count": 2, "filed_count": 2,
		"epic": {"number": 2000, "url": "https://github.com/kuhlman-labs/fishhawk/issues/2000"},
		"children": [{"ordinal": 1, "number": 2001, "url": "u1"}],
		"started_at": "2026-10-09T00:00:00Z"
	}`, false))
	f := out.Session.Filing
	if f == nil || f.State != "in_progress" || !f.InFlight || f.FiledCount != 2 || f.ChildCount != 2 {
		t.Fatalf("filing block must decode into the mirror; got %+v", f)
	}
	if f.Epic == nil || f.Epic.Number != 2000 || len(f.Children) != 1 || f.Children[0].Number != 2001 || f.StartedAt == nil {
		t.Errorf("filed-so-far items must decode; got %+v", f)
	}
	g := out.SessionGuidance[0]
	if g.State != "filing_in_progress" || g.Arm != "preview" || g.Arguments["session_id"] != testSessionID {
		t.Fatalf("guidance = %+v, want filing_in_progress -> preview(session_id)", g)
	}
	if !strings.Contains(g.Reason, "2 of 3 items") {
		t.Errorf("guidance reason = %q, want the progress (2 of 3 items)", g.Reason)
	}
}

// TestDraftEpic_Preview_FilingFailed_GuidanceFileWithPinnedRepo: a failed
// filing routes to the file arm carrying the PINNED repo, with the failure
// named. Counterfactual: delete the failed case -> the approved -> file
// guidance carries the placeholder owner/name, not the pinned repo -> RED.
func TestDraftEpic_Preview_FilingFailed_GuidanceFileWithPinnedRepo(t *testing.T) {
	out := previewWith(t, approvedSessionWithFiling("approved", `{
		"state": "failed", "repo": "kuhlman-labs/fishhawk", "in_flight": false,
		"child_count": 2, "filed_count": 2, "children": [{"ordinal": 1, "number": 2001, "url": "u1"}],
		"last_error": "link child #2001 to epic #2000: context deadline exceeded",
		"failed_ordinal": 1, "step": "link"
	}`, false))
	f := out.Session.Filing
	if f == nil || f.State != "failed" || f.FailedOrdinal == nil || *f.FailedOrdinal != 1 || f.Step != "link" || f.LastError == "" {
		t.Fatalf("failure fields must decode into the mirror; got %+v", f)
	}
	g := out.SessionGuidance[0]
	if g.State != "filing_failed" || g.Arm != "file" {
		t.Fatalf("guidance = %+v, want filing_failed -> file", g)
	}
	if g.Arguments["repo"] != "kuhlman-labs/fishhawk" || g.Arguments["session_id"] != testSessionID {
		t.Errorf("guidance arguments = %v, want the pinned repo + session_id", g.Arguments)
	}
	for _, want := range []string{"context deadline exceeded", "step link", "ordinal 1", "SAME repo"} {
		if !strings.Contains(g.Reason, want) {
			t.Errorf("guidance reason = %q, want it to contain %q", g.Reason, want)
		}
	}
}

// TestDraftEpic_Preview_FilingIncomplete_GuidanceFileWithPinnedRepo: an open
// filing session with nothing running (a restart) routes to the file arm with
// the pinned repo. Counterfactual: delete the incomplete case -> the approved
// -> file guidance carries owner/name -> RED.
func TestDraftEpic_Preview_FilingIncomplete_GuidanceFileWithPinnedRepo(t *testing.T) {
	out := previewWith(t, approvedSessionWithFiling("approved", `{
		"state": "incomplete", "repo": "kuhlman-labs/fishhawk", "in_flight": false,
		"child_count": 2, "filed_count": 1, "children": []
	}`, false))
	g := out.SessionGuidance[0]
	if g.State != "filing_incomplete" || g.Arm != "file" || g.Arguments["repo"] != "kuhlman-labs/fishhawk" {
		t.Fatalf("guidance = %+v, want filing_incomplete -> file(pinned repo)", g)
	}
	if !strings.Contains(g.Reason, "1 of 3 items") || !strings.Contains(g.Reason, "nothing is running") {
		t.Errorf("guidance reason = %q, want the progress and the nothing-running cause", g.Reason)
	}
}

// TestDraftEpic_Preview_Filed_Terminal: a completed filing (top-level state
// filed) is terminal and names the filed coordinates. Counterfactual: delete
// the filed branch of guidanceForSession -> the unknown state falls to the
// awaiting_approval default (decide) -> RED.
func TestDraftEpic_Preview_Filed_Terminal(t *testing.T) {
	out := previewWith(t, approvedSessionWithFiling("filed", `{
		"state": "filed", "repo": "kuhlman-labs/fishhawk", "in_flight": false,
		"child_count": 1, "filed_count": 2,
		"epic": {"number": 2000, "url": "e"},
		"children": [{"ordinal": 1, "number": 2001, "url": "c1"}],
		"completed_at": "2026-10-09T00:01:00Z"
	}`, false))
	if out.Session.State != "filed" || out.Session.Filing.CompletedAt == nil {
		t.Fatalf("session = %+v, want state filed with completed_at", out.Session)
	}
	g := out.SessionGuidance[0]
	if g.State != "filed" || g.Arm != "terminal" {
		t.Fatalf("guidance = %+v, want filed -> terminal", g)
	}
	if g.Arguments["epic"] != "#2000 e" || g.Arguments["child_1"] != "#2001 c1" {
		t.Errorf("terminal arguments = %v, want the filed coordinates", g.Arguments)
	}
}

// TestDraftEpic_Preview_DriftedBeatsFailedFiling: a drifted revision must be
// re-decided before any re-file (the file arm would refuse it 409), so drift
// takes precedence over a failed filing block. Counterfactual: evaluate the
// filing block before drift -> file guidance -> RED.
func TestDraftEpic_Preview_DriftedBeatsFailedFiling(t *testing.T) {
	out := previewWith(t, approvedSessionWithFiling("awaiting_approval", `{
		"state": "failed", "repo": "kuhlman-labs/fishhawk", "in_flight": false,
		"child_count": 2, "filed_count": 1, "children": [], "last_error": "boom"
	}`, true))
	if g := out.SessionGuidance[0]; g.State != "drifted" || g.Arm != "decide" {
		t.Fatalf("guidance = %+v, want drifted -> decide", g)
	}
}

// TestGuidanceForFilingProgress_Fallbacks pins the pure helper's edges: a
// failed block with no pinned repo names the owner/name placeholder and no
// failure point; an unknown state yields no filing-specific move (the caller
// falls back to the approval-state guidance); a nil block yields nothing.
func TestGuidanceForFilingProgress_Fallbacks(t *testing.T) {
	g := guidanceForFilingProgress(testSessionID, &RefinementFilingProgress{State: "failed", LastError: "boom"})
	if len(g) != 1 || g[0].Arguments["repo"] != "owner/name" {
		t.Fatalf("failed without repo = %+v, want the owner/name placeholder", g)
	}
	if !strings.Contains(g[0].Reason, "the last filing stopped: boom") {
		t.Errorf("reason = %q, want no failure point when neither step nor ordinal is named", g[0].Reason)
	}
	if got := guidanceForFilingProgress(testSessionID, &RefinementFilingProgress{State: "something_new"}); got != nil {
		t.Errorf("unknown state = %+v, want nil (fall through)", got)
	}
	if got := guidanceForFilingProgress(testSessionID, nil); got != nil {
		t.Errorf("nil block = %+v, want nil", got)
	}
	// An unknown filing state on an approved session falls through to the
	// approval-state guidance (file).
	out := guidanceForSession(&RefinementSession{SessionID: testSessionID, State: "approved", Filing: &RefinementFilingProgress{State: "something_new"}})
	if len(out) != 1 || out[0].Arm != "file" {
		t.Errorf("approved + unknown filing state = %+v, want the approved -> file guidance", out)
	}
	// A filed session with no filing block still terminates (no coordinates).
	out = guidanceForSession(&RefinementSession{SessionID: testSessionID, State: "filed"})
	if len(out) != 1 || out[0].Arm != "terminal" || out[0].Arguments != nil {
		t.Errorf("filed without a block = %+v, want terminal with no arguments", out)
	}
}

// TestDraftEpic_AuthHeaderForwardedOnEveryArm proves each of the five client
// methods routes through the shared authenticated do path — a method that
// hand-built its request and bypassed do would drop the bearer.
func TestDraftEpic_AuthHeaderForwardedOnEveryArm(t *testing.T) {
	cases := []struct {
		name string
		in   DraftEpicInput
	}{
		{"open", DraftEpicInput{Brief: "x"}},
		{"preview", DraftEpicInput{SessionID: testSessionID}},
		{"edit", DraftEpicInput{SessionID: testSessionID, BriefAmendment: "a"}},
		{"decide", DraftEpicInput{SessionID: testSessionID, Decision: "approved", Reason: "y"}},
		{"file", DraftEpicInput{SessionID: testSessionID, Repo: "o/n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newRefineFakeBackend(t)
			r := newResolver(srv, nil)
			if _, _, err := r.draftEpic(context.Background(), nil, tc.in); err != nil {
				t.Fatalf("draftEpic %s: %v", tc.name, err)
			}
			if fb.lastAuth != "Bearer tok-test" {
				t.Errorf("Authorization = %q, want 'Bearer tok-test'", fb.lastAuth)
			}
		})
	}
}

// newSleepingRefineBackend stands up a refinement backend whose every route
// sleeps `delay` before responding 2xx. The routing test uses it to prove which
// arms tolerate a slow drafting-agent-shaped response.
func newSleepingRefineBackend(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	sessionView := []byte(`{"session_id":"` + testSessionID + `","state":"awaiting_approval","revision_count":1,"latest_origin":"brief","latest_draft":` + string(defaultDraftJSON()) + `,"preview":[],"waves":[],"criteria_precheck":{"needs_attention":false,"children":[]},"decisions":[]}`)
	sleepJSON := func(status int, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/refinement/sessions", sleepJSON(http.StatusCreated, sessionView))
	mux.HandleFunc("GET /v0/refinement/sessions/{id}", sleepJSON(http.StatusOK, sessionView))
	mux.HandleFunc("PATCH /v0/refinement/sessions/{id}/draft", sleepJSON(http.StatusOK, sessionView))
	mux.HandleFunc("POST /v0/refinement/sessions/{id}/decision", sleepJSON(http.StatusOK, sessionView))
	mux.HandleFunc("POST /v0/refinement/sessions/{id}/file", sleepJSON(http.StatusOK, []byte(`{"session_id":"`+testSessionID+`","draft_id":"22222222-2222-2222-2222-222222222222","repo":"o/n","epic":{"number":1,"url":"u"},"children":[],"resumed":false,"already_completed":false,"verified":true}`)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRefinement_ClientRouting_AgentArmsUseLongClient proves the per-arm client
// routing: the two agent-backed arms (open, brief_amendment) go through the long
// client and tolerate a multi-hundred-ms response, while the four fast arms
// (preview, decide, file, direct draft edit) stay on the short client and time
// out. Both timeouts are injected small (short=20ms, long=3s) against a server
// that sleeps 120ms, so the test is fast and deterministic without a real 30s
// wait.
func TestRefinement_ClientRouting_AgentArmsUseLongClient(t *testing.T) {
	srv := newSleepingRefineBackend(t, 120*time.Millisecond)
	api := newAPIClient(config{backendURL: srv.URL, apiToken: "tok"})
	// In-package override of the injected timeouts: short aborts before the 120ms
	// server delay; long tolerates it.
	api.http.Timeout = 20 * time.Millisecond
	api.httpLong.Timeout = 3 * time.Second

	sid := uuid.MustParse(testSessionID)
	ctx := context.Background()

	// --- long-client arms SUCCEED (routed through httpLong) ---
	if _, err := api.CreateRefinementSession(ctx, "build widgets"); err != nil {
		t.Errorf("open arm should use the long client and tolerate the delay, got: %v", err)
	}
	if _, err := api.EditRefinementDraft(ctx, sid, "add a child", nil); err != nil {
		t.Errorf("brief_amendment arm should use the long client and tolerate the delay, got: %v", err)
	}

	// --- short-client arms TIME OUT (routed through the 20ms short client) ---
	assertTimeout := func(name string, err error) {
		if err == nil {
			t.Errorf("%s arm should time out on the short client, got nil error", name)
			return
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s arm error = %v, want a deadline-exceeded timeout", name, err)
		}
	}
	_, err := api.GetRefinementSession(ctx, sid)
	assertTimeout("preview", err)
	_, err = api.DecideRefinementSession(ctx, sid, "approved", "ok")
	assertTimeout("decide", err)
	_, err = api.FileRefinementSession(ctx, sid, "o/n")
	assertTimeout("file", err)
	// The direct `draft` edit is a fast, no-agent arm and MUST stay on the short
	// client — briefAmendment empty, draft non-nil.
	_, err = api.EditRefinementDraft(ctx, sid, "", &EpicDraft{})
	assertTimeout("direct-edit", err)
}
