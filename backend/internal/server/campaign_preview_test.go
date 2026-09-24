package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// --- POST /v0/campaigns/preview (#3647) ---

// postPreview POSTs a preview body to handlePreviewCampaign with an operator
// identity (scope bypass), mirroring postCampaign.
func postPreview(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handlePreviewCampaign(w, withAuth(req))
	return w
}

// decodePreview decodes a 200 preview report, failing the test on any other
// status so a refusal never silently decodes into a zero-valued report.
func decodePreview(t *testing.T, w *httptest.ResponseRecorder) campaignPreviewResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var rep campaignPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode preview report: %v (body=%s)", err, w.Body.String())
	}
	return rep
}

// previewItemRefs renders a report's items as issue refs in report order.
func previewItemRefs(rep campaignPreviewResponse) []string {
	refs := make([]string, 0, len(rep.Items))
	for _, it := range rep.Items {
		refs = append(refs, it.IssueRef)
	}
	return refs
}

// danglingReasons maps a report's dangling edges to "from->to" keyed by reason,
// so a per-failure-mode test asserts on the SHIPPED JSON rather than an
// internal type.
func danglingReasons(rep campaignPreviewResponse) map[string]string {
	out := map[string]string{}
	for _, d := range rep.Dangling {
		out[d.From+"->"+d.To] = d.Reason
	}
	return out
}

// TestPreviewCampaign_RouteReachesPreviewHandler is the ROUTER test (operator
// condition 1, kept in this file rather than handlers_test.go). It drives the
// REAL mux through s.Handler() with a write:campaigns bearer and asserts the
// response is a PREVIEW REPORT — a body only handlePreviewCampaign can emit.
// Asserting on the report rather than on a status code is what discriminates:
// a 404 (pattern never registered), a 405, or a dispatch to any other campaign
// handler all fail here, and none of them can produce valid/waves/items.
func TestPreviewCampaign_RouteReachesPreviewHandler(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	tokens := stubToken("write:campaigns")
	s := New(Config{CampaignRepo: newFakeCampaignRepo(), APITokenRepo: tokens})

	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/preview",
		strings.NewReader(`{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokens.tok.PlainText)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	rep := decodePreview(t, w)
	if !rep.Valid {
		t.Fatalf("valid = false through the router, want true (body=%s)", w.Body.String())
	}
	if got := previewItemRefs(rep); len(got) != 2 {
		t.Fatalf("items = %v, want the two smallDAG items", got)
	}
	if rep.WaveCount == nil || *rep.WaveCount != 2 {
		t.Errorf("wave_count = %v, want 2", rep.WaveCount)
	}
}

// TestPreviewCampaign_ValidSet_Report is the happy path: a closed epic set
// reports valid:true with the wave-ordered DAG.
func TestPreviewCampaign_ValidSet_Report(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
	if !rep.Valid {
		t.Fatalf("valid = false, want true: %+v", rep)
	}
	if rep.Repo != "kuhlman-labs/fishhawk" || rep.EpicRef != "issue:99" {
		t.Errorf("repo/epic_ref = %q/%q", rep.Repo, rep.EpicRef)
	}
	if rep.ItemCount != 2 {
		t.Errorf("item_count = %d, want 2", rep.ItemCount)
	}
	if len(rep.Waves) != 2 || len(rep.Waves[0]) != 1 || rep.Waves[0][0] != "issue:100" || rep.Waves[1][0] != "issue:101" {
		t.Fatalf("waves = %v, want [[issue:100] [issue:101]]", rep.Waves)
	}
	byRef := map[string]campaignPreviewItem{}
	for _, it := range rep.Items {
		byRef[it.IssueRef] = it
	}
	first, second := byRef["issue:100"], byRef["issue:101"]
	if first.Wave == nil || *first.Wave != 0 || len(first.DependsOn) != 0 {
		t.Errorf("issue:100 = %+v, want wave 0 with no depends_on", first)
	}
	if second.Wave == nil || *second.Wave != 1 || len(second.DependsOn) != 1 || second.DependsOn[0] != "issue:100" {
		t.Errorf("issue:101 = %+v, want wave 1 depending on issue:100", second)
	}
	if len(rep.Dangling) != 0 || len(rep.ClosureCandidates) != 0 || rep.Cycle != "" {
		t.Errorf("valid report carries failure fields: %+v", rep)
	}
}

// TestPreviewCampaign_CreatesNothingAndWritesNoAudit is the DONE-MEANS
// no-write test (operator condition 3). It reads COMMITTED STATE after the
// call returns — the campaign list and the audit chain — rather than only the
// response body, because the property under test is the ABSENCE of a write and
// a write that happened would leave the response byte-identical.
//
// It runs on BOTH the valid and the INVALID path, since a report-building
// branch could plausibly audit the refusal it reports.
func TestPreviewCampaign_CreatesNothingAndWritesNoAudit(t *testing.T) {
	cases := []struct {
		name      string
		result    *workmgmt.EpicChildrenResult
		wantValid bool
	}{
		{"valid", smallDAG(), true},
		{"invalid", &workmgmt.EpicChildrenResult{
			Children:     []workmgmt.EpicChild{{Number: 100, Title: "a"}},
			DroppedEdges: []workmgmt.DependsEdge{{From: 100, To: 999, Reason: workmgmt.DropNotChild}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeEpicProvider{result: tc.result}
			registerEpicProvider(t, fp)
			repo := newFakeCampaignRepo()
			aud := &campaignAuditRecorder{}
			s := New(Config{CampaignRepo: repo, AuditRepo: aud})

			before, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Limit: 100})
			if err != nil {
				t.Fatalf("list campaigns before: %v", err)
			}

			rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
			if rep.Valid != tc.wantValid {
				t.Fatalf("valid = %v, want %v", rep.Valid, tc.wantValid)
			}

			// (a) no campaign row exists, read back through the repository the
			// handler would have written to.
			after, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Limit: 100})
			if err != nil {
				t.Fatalf("list campaigns after: %v", err)
			}
			if len(after) != len(before) {
				t.Errorf("campaign count %d -> %d: preview created a campaign", len(before), len(after))
			}
			if len(after) != 0 {
				t.Errorf("campaigns after preview = %d, want none", len(after))
			}

			// (b) the audit chain is unchanged: no entry of ANY category was
			// appended for this preview (operator condition 3).
			aud.mu.Lock()
			appended := len(aud.entries)
			categories := make([]string, 0, appended)
			for _, e := range aud.entries {
				categories = append(categories, e.Category)
			}
			aud.mu.Unlock()
			if appended != 0 {
				t.Errorf("preview appended %d audit entries, want 0 (categories: %v)", appended, categories)
			}
		})
	}
}

// TestPreviewCampaign_DanglingNotChild_ReportsPartialGraph is the central
// behaviour: an unassemblable set is a 200 REPORT, not the 422 create refuses
// with. It also pins operator condition 2 — an INVALID report carries the items
// that DID resolve WITH their in-set edges, so the operator sees the partial
// graph alongside the dangling edges. waves may be absent; items and edges may
// not.
func TestPreviewCampaign_DanglingNotChild_ReportsPartialGraph(t *testing.T) {
	fp := &fakeIssueSetProvider{result: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 101, Title: "a", Autonomy: "medium"},
			{Number: 102, Title: "b"},
		},
		// One IN-SET edge (102 depends on 101) and one DANGLING edge.
		Edges:        []workmgmt.DependsEdge{{From: 102, To: 101}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 101, To: 999, Reason: workmgmt.DropNotChild}},
	}}
	registerIssueSetProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","items":["issue:101","issue:102"]}`))
	if rep.Valid {
		t.Fatalf("valid = true on a dangling set: %+v", rep)
	}
	// The partial graph (condition 2): both resolved items, with the in-set edge.
	if got := previewItemRefs(rep); len(got) != 2 || got[0] != "issue:101" || got[1] != "issue:102" {
		t.Fatalf("items = %v, want both resolved items on the INVALID path", got)
	}
	if rep.ItemCount != 2 {
		t.Errorf("item_count = %d, want 2", rep.ItemCount)
	}
	byRef := map[string]campaignPreviewItem{}
	for _, it := range rep.Items {
		byRef[it.IssueRef] = it
	}
	if deps := byRef["issue:102"].DependsOn; len(deps) != 1 || deps[0] != "issue:101" {
		t.Errorf("issue:102 depends_on = %v, want the in-set edge [issue:101]", deps)
	}
	if deps := byRef["issue:101"].DependsOn; len(deps) != 0 {
		t.Errorf("issue:101 depends_on = %v, want empty (its only edge is dangling, reported separately)", deps)
	}
	if byRef["issue:101"].Autonomy != "medium" {
		t.Errorf("autonomy = %q, want medium carried onto the invalid report", byRef["issue:101"].Autonomy)
	}
	// waves MAY be omitted on an invalid set — and are.
	if len(rep.Waves) != 0 || rep.WaveCount != nil {
		t.Errorf("waves/wave_count present on an invalid report: %v / %v", rep.Waves, rep.WaveCount)
	}
	// The dangling edge, as DATA.
	if got, want := danglingReasons(rep), map[string]string{"issue:101->issue:999": "not_child"}; len(got) != 1 || got["issue:101->issue:999"] != want["issue:101->issue:999"] {
		t.Fatalf("dangling = %v, want the not_child edge", got)
	}
	if rep.Dangling[0].Remedy == "" {
		t.Error("dangling entry carries no remedy")
	}
	if rep.Message == "" {
		t.Error("invalid report carries no assembler message")
	}
	// not_child IS widenable, so its target is a closure candidate.
	if len(rep.ClosureCandidates) != 1 || rep.ClosureCandidates[0] != "issue:999" {
		t.Errorf("closure_candidates = %v, want [issue:999]", rep.ClosureCandidates)
	}
}

// TestPreviewCampaign_ExcludedIncomplete_InClosureCandidates: the second
// WIDENABLE cause — an excluded-but-incomplete sibling — contributes a closure
// candidate, since including it in items IS the remedy.
func TestPreviewCampaign_ExcludedIncomplete_InClosureCandidates(t *testing.T) {
	fp := &fakeEpicProvider{result: &workmgmt.EpicChildrenResult{
		Children:     []workmgmt.EpicChild{{Number: 100, Title: "a"}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 100, To: 101, Reason: workmgmt.DropExcludedIncomplete}},
	}}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
	if rep.Valid {
		t.Fatalf("valid = true, want false: %+v", rep)
	}
	if got := danglingReasons(rep)["issue:100->issue:101"]; got != "excluded_incomplete" {
		t.Errorf("reason = %q, want excluded_incomplete", got)
	}
	if len(rep.ClosureCandidates) != 1 || rep.ClosureCandidates[0] != "issue:101" {
		t.Errorf("closure_candidates = %v, want [issue:101]", rep.ClosureCandidates)
	}
}

// TestPreviewCampaign_UnwidenableCauses_AbsentFromClosureCandidates is the
// COUNTERFACTUAL VEHICLE for the closure_candidates category filter: a
// closed-but-incomplete target and an unreadable target are both REPORTED as
// dangling, yet neither is offered as something to add to items — adding it
// cannot satisfy the edge. Deleting the filter puts them in the list and reddens
// this test.
func TestPreviewCampaign_UnwidenableCauses_AbsentFromClosureCandidates(t *testing.T) {
	cases := []struct {
		name       string
		reason     workmgmt.DropReason
		target     int
		wantReason string
	}{
		{"closed_incomplete", workmgmt.DropTargetClosedIncomplete, 1641, "target_closed_incomplete"},
		{"state_unreadable", workmgmt.DropTargetStateUnreadable, 1700, "target_state_unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeEpicProvider{result: &workmgmt.EpicChildrenResult{
				Children:     []workmgmt.EpicChild{{Number: 100, Title: "a"}},
				DroppedEdges: []workmgmt.DependsEdge{{From: 100, To: tc.target, Reason: tc.reason}},
			}}
			registerEpicProvider(t, fp)
			s := New(Config{CampaignRepo: newFakeCampaignRepo()})

			rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
			if rep.Valid {
				t.Fatalf("valid = true, want false: %+v", rep)
			}
			key := "issue:100->issue:" + strconv.Itoa(tc.target)
			if got := danglingReasons(rep)[key]; got != tc.wantReason {
				t.Fatalf("dangling %s reason = %q, want %q (all: %v)", key, got, tc.wantReason, danglingReasons(rep))
			}
			if len(rep.ClosureCandidates) != 0 {
				t.Errorf("closure_candidates = %v, want EMPTY: adding an unwidenable target cannot satisfy the edge",
					rep.ClosureCandidates)
			}
		})
	}
}

// TestPreviewCampaign_MixedCauses_OnlyWidenableInClosureCandidates pairs the
// widenable and unwidenable causes in ONE report, so the filter is proven to
// PARTITION rather than to be all-or-nothing.
func TestPreviewCampaign_MixedCauses_OnlyWidenableInClosureCandidates(t *testing.T) {
	fp := &fakeEpicProvider{result: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 100, Title: "a"}, {Number: 101, Title: "b"}},
		DroppedEdges: []workmgmt.DependsEdge{
			{From: 100, To: 900, Reason: workmgmt.DropNotChild},
			{From: 101, To: 901, Reason: workmgmt.DropTargetClosedIncomplete},
		},
	}}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
	if len(rep.Dangling) != 2 {
		t.Fatalf("dangling = %+v, want both edges reported", rep.Dangling)
	}
	if len(rep.ClosureCandidates) != 1 || rep.ClosureCandidates[0] != "issue:900" {
		t.Errorf("closure_candidates = %v, want only the widenable [issue:900]", rep.ClosureCandidates)
	}
}

// TestPreviewCampaign_Cycle_ReportsInvalid: a cycle is a 200 report too, with
// the assembler's message and NO dangling list.
func TestPreviewCampaign_Cycle_ReportsInvalid(t *testing.T) {
	fp := &fakeIssueSetProvider{result: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 101, Title: "a"}, {Number: 102, Title: "b"}},
		Edges:    []workmgmt.DependsEdge{{From: 101, To: 102}, {From: 102, To: 101}},
	}}
	registerIssueSetProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","items":["issue:101","issue:102"]}`))
	if rep.Valid {
		t.Fatalf("valid = true on a cycle: %+v", rep)
	}
	if rep.Cycle == "" || !strings.Contains(rep.Cycle, "cycle") {
		t.Errorf("cycle = %q, want the assembler's cycle message", rep.Cycle)
	}
	if len(rep.Dangling) != 0 {
		t.Errorf("dangling = %+v, want empty on a cycle", rep.Dangling)
	}
	// Condition 2 holds on the cycle path too: the resolved items and their
	// in-set edges are still reported.
	if len(rep.Items) != 2 {
		t.Fatalf("items = %v, want both resolved items", previewItemRefs(rep))
	}
	byRef := map[string]campaignPreviewItem{}
	for _, it := range rep.Items {
		byRef[it.IssueRef] = it
	}
	if deps := byRef["issue:101"].DependsOn; len(deps) != 1 || deps[0] != "issue:102" {
		t.Errorf("issue:101 depends_on = %v, want the in-set edge [issue:102]", deps)
	}
}

// TestPreviewCampaign_WrappedDanglingNoTypedForm is the DEFENSIVE branch: an
// assembly error that wraps ErrDanglingDependency WITHOUT the typed
// *campaign.DanglingDependencyError form (Assemble's non-child-edge invariant)
// must still report valid:false with the assembler's message and an EMPTY
// dangling list — never valid:true.
func TestPreviewCampaign_WrappedDanglingNoTypedForm(t *testing.T) {
	fp := &fakeIssueSetProvider{result: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 101, Title: "a"}},
		// An edge whose target is NOT a child, surfaced as a live Edge rather
		// than a DroppedEdge: Assemble's defensive fmt.Errorf path.
		Edges: []workmgmt.DependsEdge{{From: 101, To: 999}},
	}}
	registerIssueSetProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","items":["issue:101"]}`))
	if rep.Valid {
		t.Fatalf("valid = true on a defensively-wrapped dangling error: %+v", rep)
	}
	if len(rep.Dangling) != 0 {
		t.Errorf("dangling = %+v, want empty (no typed form to categorize)", rep.Dangling)
	}
	if len(rep.ClosureCandidates) != 0 {
		t.Errorf("closure_candidates = %v, want empty", rep.ClosureCandidates)
	}
	if rep.Message == "" {
		t.Error("message empty: the assembler's reason must still reach the operator")
	}
}

// TestPreviewCampaign_SatisfiedDependencies_Valid: an already-satisfied
// out-of-set target is elided into satisfied_dependencies, so the set previews
// VALID rather than dangling.
func TestPreviewCampaign_SatisfiedDependencies_Valid(t *testing.T) {
	fp := &fakeEpicProvider{result: satisfiedDepDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	rep := decodePreview(t, postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
	if !rep.Valid {
		t.Fatalf("valid = false, want true: %+v", rep)
	}
	if len(rep.SatisfiedDependencies) != 1 || rep.SatisfiedDependencies[0].To != 1639 {
		t.Errorf("satisfied_dependencies = %+v, want the elided edge to 1639", rep.SatisfiedDependencies)
	}
}

// --- preview refusals: identical to create's, by construction ---

// TestPreviewCampaign_Refusals covers every REQUEST-shaped failure mode. Each
// one is a refusal rather than a valid:false report because it is a property of
// the REQUEST, not of the resolved graph — and each is the byte-identical code
// create answers with, which is what the shared resolver buys.
func TestPreviewCampaign_Refusals(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T) *Server
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "neither epic_ref nor items nor grooming_source",
			setup:      func(*testing.T) *Server { return New(Config{CampaignRepo: newFakeCampaignRepo()}) },
			body:       `{"repo":"kuhlman-labs/fishhawk"}`,
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			name:       "grooming_source combined with items",
			setup:      func(*testing.T) *Server { return New(Config{CampaignRepo: newFakeCampaignRepo()}) },
			body:       `{"repo":"kuhlman-labs/fishhawk","items":["issue:1"],"grooming_source":{"run_id":"11111111-1111-1111-1111-111111111111"}}`,
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			name:       "repo not owner/name",
			setup:      func(*testing.T) *Server { return New(Config{CampaignRepo: newFakeCampaignRepo()}) },
			body:       `{"repo":"nope","epic_ref":"issue:1"}`,
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			name:       "body is not valid JSON",
			setup:      func(*testing.T) *Server { return New(Config{CampaignRepo: newFakeCampaignRepo()}) },
			body:       `{`,
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			// The create-only knobs have no preview meaning, so they are
			// UNKNOWN FIELDS rather than silently-ignored ones.
			name:       "create-only pause_policy rejected as an unknown field",
			setup:      func(*testing.T) *Server { return New(Config{CampaignRepo: newFakeCampaignRepo()}) },
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:1","pause_policy":"pause_item"}`,
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			name:       "create-only working_dir rejected as an unknown field",
			setup:      func(*testing.T) *Server { return New(Config{CampaignRepo: newFakeCampaignRepo()}) },
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:1","working_dir":"/tmp/x"}`,
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			name:       "create-only operator_agent rejected as an unknown field",
			setup:      func(*testing.T) *Server { return New(Config{CampaignRepo: newFakeCampaignRepo()}) },
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:1","operator_agent":{}}`,
			wantStatus: http.StatusBadRequest, wantCode: "validation_failed",
		},
		{
			name: "malformed items ref",
			setup: func(t *testing.T) *Server {
				registerEpicProvider(t, &fakeEpicProvider{result: smallDAG()})
				return New(Config{CampaignRepo: newFakeCampaignRepo()})
			},
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","items":["not-a-ref"]}`,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "campaign_item_ref_invalid",
		},
		{
			name: "non-child subset ref",
			setup: func(t *testing.T) *Server {
				registerEpicProvider(t, &fakeEpicProvider{result: smallDAG()})
				return New(Config{CampaignRepo: newFakeCampaignRepo()})
			},
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","items":["issue:999"]}`,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "campaign_item_not_child",
		},
		{
			name: "provider cannot resolve an issue set",
			setup: func(t *testing.T) *Server {
				registerEpicProvider(t, &fakeEpicProvider{result: smallDAG()}) // querier only
				return New(Config{CampaignRepo: newFakeCampaignRepo()})
			},
			body:       `{"repo":"kuhlman-labs/fishhawk","items":["issue:101"]}`,
			wantStatus: http.StatusNotImplemented, wantCode: "issue_set_resolution_unsupported",
		},
		{
			name: "provider cannot query epic children",
			setup: func(t *testing.T) *Server {
				registerIssueSetProvider(t, &fakeIssueSetProvider{result: noEpicDAG()}) // resolver only
				return New(Config{CampaignRepo: newFakeCampaignRepo()})
			},
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`,
			wantStatus: http.StatusNotImplemented, wantCode: "epic_children_unsupported",
		},
		{
			name: "GitHub App not installed",
			setup: func(t *testing.T) *Server {
				registerEpicProvider(t, &fakeEpicProvider{result: smallDAG()})
				return New(Config{CampaignRepo: newFakeCampaignRepo(), GitHub: newInstallationGitHubClient(t, 0, true)})
			},
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "repo_not_installed",
		},
		{
			name:       "campaign repository unconfigured",
			setup:      func(*testing.T) *Server { return New(Config{}) },
			body:       `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`,
			wantStatus: http.StatusServiceUnavailable, wantCode: "campaign_repo_unconfigured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.setup(t)
			w := postPreview(t, s, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if code := decodeCampaignError(t, w); code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// TestPreviewCampaign_RequireWriteScope_403 is the COUNTERFACTUAL VEHICLE for
// the scope guard. The read-only token is minted BY CONSTRUCTION (a scope set
// that simply lacks write:campaigns) rather than by calling the guard in the
// test's own setup, so deleting requireWriteScope from handlePreviewCampaign
// lands the failure on the 200-instead-of-403 assertion.
func TestPreviewCampaign_RequireWriteScope_403(t *testing.T) {
	registerEpicProvider(t, &fakeEpicProvider{result: smallDAG()})
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	id := Identity{Subject: "github:op", TokenID: "tok_read_only", Scopes: []string{"read:runs", "write:runs"}}
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/preview",
		strings.NewReader(`{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handlePreviewCampaign(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", code)
	}
}

// TestPreviewCampaign_MatchesCreate is the CROSS-BOUNDARY parity test: the
// load-bearing claim of this endpoint is that a preview is what create WOULD
// do, and that is only a tested claim if one fixture is driven through BOTH
// surfaces on the same server.
//
// It asserts two directions:
//   - a VALID fixture: the preview's items (ref, depends_on, position) equal the
//     created campaign's persisted items exactly, and the preview's waves
//     partition those same refs with each item's reported wave equal to the
//     index of the wave holding it (the persisted rows carry no wave column, so
//     the wave assignment is checked against the DAG the same rows encode);
//   - a DANGLING fixture: the edge set the preview reports as data is exactly
//     the edge set create refuses 422 campaign_dangling_dependency over.
func TestPreviewCampaign_MatchesCreate(t *testing.T) {
	t.Run("valid fixture", func(t *testing.T) {
		registerEpicProvider(t, &fakeEpicProvider{result: threeChildDAG()})
		repo := newFakeCampaignRepo()
		s := New(Config{CampaignRepo: repo})
		const body = `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`

		rep := decodePreview(t, postPreview(t, s, body))
		if !rep.Valid {
			t.Fatalf("preview reported invalid for a fixture create accepts: %+v", rep)
		}

		w := postCampaign(t, s, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("create status = %d, want 201 (body=%s)", w.Code, w.Body.String())
		}
		var created campaignResponse
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode created campaign: %v", err)
		}
		items, err := repo.ListCampaignItemsForCampaign(context.Background(), created.ID)
		if err != nil {
			t.Fatalf("list items: %v", err)
		}
		if len(items) != len(rep.Items) {
			t.Fatalf("preview item count %d != created item count %d", len(rep.Items), len(items))
		}
		for i, it := range items {
			p := rep.Items[i]
			if p.IssueRef != it.IssueRef {
				t.Errorf("item %d: preview ref %q != created ref %q", i, p.IssueRef, it.IssueRef)
			}
			if p.Position != it.Position {
				t.Errorf("item %d (%s): preview position %d != created position %d", i, it.IssueRef, p.Position, it.Position)
			}
			if strings.Join(p.DependsOn, ",") != strings.Join(it.DependsOn, ",") {
				t.Errorf("item %d (%s): preview depends_on %v != created depends_on %v", i, it.IssueRef, p.DependsOn, it.DependsOn)
			}
		}
		// The reported wave assignment agrees with the reported waves, and the
		// waves partition exactly the persisted refs.
		seen := map[string]bool{}
		for w, wave := range rep.Waves {
			for _, ref := range wave {
				seen[ref] = true
				for _, p := range rep.Items {
					if p.IssueRef == ref && (p.Wave == nil || *p.Wave != w) {
						t.Errorf("%s reported wave %v but appears in waves[%d]", ref, p.Wave, w)
					}
				}
			}
		}
		for _, it := range items {
			if !seen[it.IssueRef] {
				t.Errorf("created item %s is absent from the preview's waves", it.IssueRef)
			}
		}
	})

	t.Run("dangling fixture", func(t *testing.T) {
		result := &workmgmt.EpicChildrenResult{
			Children: []workmgmt.EpicChild{{Number: 100, Title: "a"}, {Number: 101, Title: "b"}},
			DroppedEdges: []workmgmt.DependsEdge{
				{From: 100, To: 900, Reason: workmgmt.DropNotChild},
				{From: 101, To: 901, Reason: workmgmt.DropTargetClosedIncomplete},
			},
		}
		registerEpicProvider(t, &fakeEpicProvider{result: result})
		repo := newFakeCampaignRepo()
		s := New(Config{CampaignRepo: repo})
		const body = `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`

		rep := decodePreview(t, postPreview(t, s, body))
		if rep.Valid {
			t.Fatalf("preview reported valid for a fixture create refuses: %+v", rep)
		}

		w := postCampaign(t, s, body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create status = %d, want 422 (body=%s)", w.Code, w.Body.String())
		}
		code, details := decodeCampaignErrorDetails(t, w)
		if code != "campaign_dangling_dependency" {
			t.Fatalf("create code = %q, want campaign_dangling_dependency", code)
		}
		// The SAME edges, in both renderings.
		refusalEdges := map[string]bool{}
		for _, key := range []string{"dangling_not_child", "dangling_closed_incomplete"} {
			list, _ := details[key].([]any)
			for _, e := range list {
				refusalEdges[e.(string)] = true
			}
		}
		previewEdges := map[string]bool{}
		for _, d := range rep.Dangling {
			previewEdges[d.From+"->"+d.To] = true
		}
		if len(refusalEdges) != len(previewEdges) {
			t.Fatalf("preview edges %v != create refusal edges %v", previewEdges, refusalEdges)
		}
		for e := range refusalEdges {
			if !previewEdges[e] {
				t.Errorf("edge %q in create's refusal but not in the preview report", e)
			}
		}
	})
}

// TestPreviewCampaign_ExplicitProviderUnregistered_Rejects pins that the
// OPTIONAL `provider` selector (#3645) is validated on the preview path by the
// SAME shared helper the create path uses, with the same 400 and the same
// details keys. A preview whose selector were silently ignored would report a
// DAG resolved against a different issue tracker than the create it is the
// pre-flight for.
//
// COUNTERFACTUAL VEHICLE: the epic provider is seeded to SUCCEED, so deleting
// the validateCampaignProviderSelector call from handlePreviewCampaign lands the
// red on a 200 report instead of a missing fixture.
func TestPreviewCampaign_ExplicitProviderUnregistered_Rejects(t *testing.T) {
	stubRegisteredWorkItemProviders(t, []string{"github_projects", "gitlab"})
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","provider":"never_registered"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	code, details := decodeCampaignErrorDetails(t, w)
	if code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", code)
	}
	if details["field"] != "provider" {
		t.Errorf("details.field = %v, want provider", details["field"])
	}
	if details["got"] != "never_registered" {
		t.Errorf("details.got = %v, want never_registered", details["got"])
	}
	reg, _ := details["registered"].([]any)
	if len(reg) != 2 || reg[0] != "github_projects" || reg[1] != "gitlab" {
		t.Errorf("details.registered = %v, want the live registered set", details["registered"])
	}
	// A body check: it must cost no provider dispatch.
	if fp.called {
		t.Error("the provider was dispatched despite an unregistered explicit provider")
	}
}

// TestPreviewCampaign_ExplicitProviderOverridesConventions proves the selector
// is THREADED into the shared resolver rather than merely validated: the
// previewed DAG must come from the EXPLICIT provider's fake, not the
// conventions-resolved one.
//
// COUNTERFACTUAL VEHICLE: drop Provider from the createCampaignRequest
// handlePreviewCampaign builds and the conventions fake is dispatched, yielding
// smallDAG's {100,101} instead of noEpicDAG's {101,102}.
func TestPreviewCampaign_ExplicitProviderOverridesConventions(t *testing.T) {
	const explicitID = "explicit_preview_provider"
	stubConventionsProvider(t, workmgmt.Conventions{Provider: workmgmt.Default().Provider})
	stubRegisteredWorkItemProviders(t, []string{workmgmt.Default().Provider, explicitID})

	conventionsFake := &fakeEpicProvider{name: workmgmt.Default().Provider, result: smallDAG()}
	explicitFake := &fakeEpicProvider{name: explicitID, result: noEpicDAG()}
	byID := map[string]workmgmt.Provider{
		workmgmt.Default().Provider: conventionsFake,
		explicitID:                  explicitFake,
	}
	var dispatched string
	stubWorkItemProviderLookup(t, func(id string) (workmgmt.Provider, error) {
		dispatched = id
		p, ok := byID[id]
		if !ok {
			return nil, &workmgmt.UnknownProviderError{ID: id, Known: []string{workmgmt.Default().Provider, explicitID}}
		}
		return p, nil
	})

	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	w := postPreview(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","provider":"`+explicitID+`"}`)
	rep := decodePreview(t, w)

	if dispatched != explicitID {
		t.Errorf("resolved provider = %q, want the EXPLICIT %q", dispatched, explicitID)
	}
	if conventionsFake.called {
		t.Error("the CONVENTIONS provider was dispatched despite an explicit provider on the request")
	}
	if got := previewItemRefs(rep); len(got) != 2 || got[0] != "issue:101" || got[1] != "issue:102" {
		t.Errorf("items = %v, want the EXPLICIT provider's noEpicDAG set [issue:101 issue:102]", got)
	}
}
