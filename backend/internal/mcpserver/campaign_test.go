package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// --- fishhawk_start_campaign (E25.8 / #1447) ---

// TestStartCampaign_HappyPath_PostsBodyReturnsCampaign drives the whole
// tool→client→wire→decode chain in one test: the input struct's
// repo/epic_ref/pause_policy reach the backend request body, and the created
// Campaign decodes back out.
func TestStartCampaign_HappyPath_PostsBodyReturnsCampaign(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:        "kuhlman-labs/fishhawk",
		EpicRef:     "#25",
		PausePolicy: "pause_item",
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	if fb.createCampaignBody.Repo != "kuhlman-labs/fishhawk" ||
		fb.createCampaignBody.EpicRef != "#25" ||
		fb.createCampaignBody.PausePolicy != "pause_item" {
		t.Errorf("backend got body = %+v", fb.createCampaignBody)
	}
	if out.Campaign.ID == "" {
		t.Errorf("Campaign.ID empty; expected the fake to allocate one")
	}
	if out.Campaign.Repo != "kuhlman-labs/fishhawk" || out.Campaign.EpicRef != "#25" {
		t.Errorf("decoded Campaign = %+v", out.Campaign)
	}
	if out.Campaign.PausePolicy != "pause_item" {
		t.Errorf("Campaign.PausePolicy = %q, want pause_item", out.Campaign.PausePolicy)
	}
}

// TestStartCampaign_ItemsSubset_PostsItemsInBody pins the optional subset
// filter (#2003): a non-empty items list travels in the POST body verbatim so
// the backend scopes the campaign to just the named children.
func TestStartCampaign_ItemsSubset_PostsItemsInBody(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:    "kuhlman-labs/fishhawk",
		EpicRef: "#25",
		Items:   []string{"issue:101", "issue:102"},
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	if got := fb.createCampaignBody.Items; len(got) != 2 || got[0] != "issue:101" || got[1] != "issue:102" {
		t.Errorf("backend got items = %v, want [issue:101 issue:102]", got)
	}
}

// TestStartCampaign_OmittedItems_LeavesBodyEmpty pins the backward-compatible
// default: omitting items sends no items field (nil), so the backend sweeps
// every child.
func TestStartCampaign_OmittedItems_LeavesBodyEmpty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:    "x/y",
		EpicRef: "#1",
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	if len(fb.createCampaignBody.Items) != 0 {
		t.Errorf("items = %v, want empty (omit sweeps every child)", fb.createCampaignBody.Items)
	}
}

// TestStartCampaign_ItemNotChild_MapsActionableError covers the new
// campaign_item_not_child wire code: a requested item that is not a child of
// the epic maps to an actionable client-side message naming the code and epic.
func TestStartCampaign_ItemNotChild_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"campaign_item_not_child","message":"issue:999 is not a child of the epic"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo: "x/y", EpicRef: "#25", Items: []string{"issue:999"},
	})
	if err == nil {
		t.Fatal("err = nil, want campaign_item_not_child mapping")
	}
	for _, want := range []string{"campaign_item_not_child", "child", "#25"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestStartCampaign_OmittedPausePolicy_LeavesBodyEmpty pins the optional
// pause_policy: omitting it sends an empty value (the backend normalizes it).
func TestStartCampaign_OmittedPausePolicy_LeavesBodyEmpty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:    "x/y",
		EpicRef: "#1",
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	if fb.createCampaignBody.PausePolicy != "" {
		t.Errorf("pause_policy = %q, want empty (omit takes the server default)", fb.createCampaignBody.PausePolicy)
	}
}

// TestStartCampaign_OperatorAgentOverride_CarriedAndReturned proves the OPTIONAL
// campaign-level operator_agent override (E25.12 / #1451) travels in the POST
// body as opaque JSON AND round-trips back on the created Campaign mirror — the
// surface that lets the rollup show the contract governing every issue-run.
func TestStartCampaign_OperatorAgentOverride_CarriedAndReturned(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	override := map[string]any{"may_approve": "always", "must_page_human": []any{"deploy"}}
	fb.createCampaignResp = Campaign{
		ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "pending", PausePolicy: "pause_campaign",
		OperatorAgent: override,
	}
	r := newResolver(srv, nil)

	_, out, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:          "x/y",
		EpicRef:       "#25",
		OperatorAgent: override,
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	// The request body carried the override as a JSON object.
	if len(fb.createCampaignBody.OperatorAgent) == 0 {
		t.Fatalf("operator_agent absent from POST body: %+v", fb.createCampaignBody)
	}
	var sent map[string]any
	if err := json.Unmarshal(fb.createCampaignBody.OperatorAgent, &sent); err != nil {
		t.Fatalf("operator_agent body not valid JSON: %v", err)
	}
	if sent["may_approve"] != "always" {
		t.Errorf("sent operator_agent.may_approve = %v, want always", sent["may_approve"])
	}
	// The response round-tripped the block back onto the Campaign mirror.
	if out.Campaign.OperatorAgent["may_approve"] != "always" {
		t.Errorf("returned Campaign.OperatorAgent = %+v", out.Campaign.OperatorAgent)
	}
}

// TestStartCampaign_OmittedOperatorAgent_LeavesBodyEmpty pins the optional
// operator_agent: omitting it sends NO operator_agent key (the byte-identical
// default — each issue-run inherits its workflow contract).
func TestStartCampaign_OmittedOperatorAgent_LeavesBodyEmpty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#1"})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	if len(fb.createCampaignBody.OperatorAgent) != 0 {
		t.Errorf("operator_agent present when not supplied: %s", fb.createCampaignBody.OperatorAgent)
	}
}

// TestStartCampaign_EmptyOperatorAgent_CarriedAsEmptyObject exercises the
// JSON-UNMARSHAL boundary for the omitted-vs-explicit-{} discriminator. It
// unmarshals two distinct tool-input JSON payloads into StartCampaignInput and
// asserts that (a) an omitted operator_agent leaves the field as a nil map and
// (b) an explicit "operator_agent":{} produces a non-nil empty map — and that
// startCampaign forwards the empty map to the REST layer as the JSON object
// "{}" while dropping the omitted case. This guards the regression path where
// `len(...) > 0` incorrectly collapses both cases.
func TestStartCampaign_EmptyOperatorAgent_CarriedAsEmptyObject(t *testing.T) {
	// Part (a): omitted operator_agent → nil map → no operator_agent key in body.
	{
		var omittedIn StartCampaignInput
		if err := json.Unmarshal([]byte(`{"repo":"x/y","epic_ref":"#1"}`), &omittedIn); err != nil {
			t.Fatalf("unmarshal omitted: %v", err)
		}
		if omittedIn.OperatorAgent != nil {
			t.Errorf("omitted operator_agent: got non-nil map %v, want nil", omittedIn.OperatorAgent)
		}

		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		_, _, err := r.startCampaign(context.Background(), nil, omittedIn)
		if err != nil {
			t.Fatalf("startCampaign (omitted): %v", err)
		}
		if len(fb.createCampaignBody.OperatorAgent) != 0 {
			t.Errorf("omitted operator_agent: body has operator_agent %s, want absent", fb.createCampaignBody.OperatorAgent)
		}
	}

	// Part (b): explicit "operator_agent":{} → non-nil empty map → body carries "{}".
	{
		var emptyIn StartCampaignInput
		if err := json.Unmarshal([]byte(`{"repo":"x/y","epic_ref":"#1","operator_agent":{}}`), &emptyIn); err != nil {
			t.Fatalf("unmarshal empty {}: %v", err)
		}
		if emptyIn.OperatorAgent == nil {
			t.Fatal("explicit {}: got nil map, want non-nil empty map")
		}
		if len(emptyIn.OperatorAgent) != 0 {
			t.Errorf("explicit {}: map should be empty, got %v", emptyIn.OperatorAgent)
		}

		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		_, _, err := r.startCampaign(context.Background(), nil, emptyIn)
		if err != nil {
			t.Fatalf("startCampaign (empty {}): %v", err)
		}
		if len(fb.createCampaignBody.OperatorAgent) == 0 {
			t.Fatal("explicit {}: operator_agent absent from POST body, want {}")
		}
		var decoded map[string]any
		if err := json.Unmarshal(fb.createCampaignBody.OperatorAgent, &decoded); err != nil {
			t.Fatalf("operator_agent body not valid JSON: %v", err)
		}
		if decoded == nil || len(decoded) != 0 {
			t.Errorf("explicit {}: body operator_agent decoded to %v, want non-nil empty map", decoded)
		}
		if got := string(fb.createCampaignBody.OperatorAgent); got != "{}" {
			t.Errorf("explicit {}: body operator_agent bytes = %q, want {}", got)
		}
	}
}

// TestStartCampaign_MissingRepo_FailsLocally proves the empty-repo guard
// rejects before any HTTP call.
func TestStartCampaign_MissingRepo_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{EpicRef: "#1"})
	if err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Fatalf("err = %v, want local repo-required validation", err)
	}
	if fb.createCampaignBody.EpicRef != "" {
		t.Errorf("backend was called despite missing repo: %+v", fb.createCampaignBody)
	}
}

// TestStartCampaign_NeitherEpicRefNorItems_FailsLocally proves the
// neither-provided guard (#2051) rejects before any HTTP call: with repo set but
// NEITHER epic_ref NOR items, the tool fails locally with the actionable
// one-of-epic_ref-or-items message. (An items-only call is accepted — see
// TestStartCampaign_ItemsOnly_NoEpicRef_Forwarded.)
func TestStartCampaign_NeitherEpicRefNorItems_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y"})
	if err == nil || !strings.Contains(err.Error(), "one of epic_ref or items is required") {
		t.Fatalf("err = %v, want local one-of-epic_ref-or-items validation", err)
	}
	if fb.createCampaignBody.Repo != "" {
		t.Errorf("backend was called despite neither epic_ref nor items: %+v", fb.createCampaignBody)
	}
}

// TestStartCampaign_ItemsOnly_NoEpicRef_Forwarded proves the no-epic variant
// (#2051) is accepted at the tool layer: items present with epic_ref ABSENT
// passes local validation and forwards to the backend with an EMPTY epic_ref
// (no omitempty on the wire) and the items list — the server owns the branch.
func TestStartCampaign_ItemsOnly_NoEpicRef_Forwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:  "kuhlman-labs/fishhawk",
		Items: []string{"issue:101", "issue:102"},
	})
	if err != nil {
		t.Fatalf("startCampaign (items-only): %v", err)
	}
	if fb.createCampaignBody.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("backend repo = %q, want the request repo", fb.createCampaignBody.Repo)
	}
	if fb.createCampaignBody.EpicRef != "" {
		t.Errorf("backend epic_ref = %q, want empty (no-epic variant)", fb.createCampaignBody.EpicRef)
	}
	if got := fb.createCampaignBody.Items; len(got) != 2 || got[0] != "issue:101" || got[1] != "issue:102" {
		t.Errorf("backend items = %v, want [issue:101 issue:102]", got)
	}
}

func TestStartCampaign_RepoNotInstalled_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"repo_not_installed","message":"GitHub App is not installed on the target repository"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#1"})
	if err == nil {
		t.Fatal("err = nil, want repo_not_installed mapping")
	}
	for _, want := range []string{"repo_not_installed", "install the Fishhawk GitHub App", "x/y"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestStartCampaign_DanglingDependency_MapsActionableError is the fallback
// branch: an error WITHOUT the #2120 category details (an older backend) keeps
// the pre-existing "not a fellow child" fix-the-edges wording.
func TestStartCampaign_DanglingDependency_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"campaign_dangling_dependency","message":"child #27 depends on #99 which is not a fellow child"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#25"})
	if err == nil {
		t.Fatal("err = nil, want campaign_dangling_dependency mapping")
	}
	for _, want := range []string{"campaign_dangling_dependency", "depends_on", "not a fellow child", "#25"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
	// No excluded-incomplete remedy leaks into the pure not_child fallback.
	if strings.Contains(err.Error(), "include it in items") {
		t.Errorf("fallback err %q must not name the excluded-incomplete remedy", err.Error())
	}
}

// TestStartCampaign_DanglingNotChildDetails_KeepsNotChildWording is the #2120
// not_child branch: details carrying only dangling_not_child render the
// unchanged "not a fellow child" wording, not the excluded-incomplete remedy.
func TestStartCampaign_DanglingNotChildDetails_KeepsNotChildWording(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"campaign_dangling_dependency","message":"dangling edge","details":{"epic_ref":"#25","dangling_not_child":["issue:27->issue:999"]}}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#25"})
	if err == nil {
		t.Fatal("err = nil, want mapping")
	}
	for _, want := range []string{"campaign_dangling_dependency", "not a fellow child", "#25"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "include it in items") {
		t.Errorf("not_child-only err %q must not name the excluded-incomplete remedy", err.Error())
	}
}

// TestStartCampaign_DanglingExcludedIncompleteDetails_NamesIncludeOmitRemedy is
// the #2120 excluded-incomplete branch: details carrying only
// dangling_excluded_incomplete render the include-in-items / omit-items remedy
// naming the real cause, NOT the generic fix-the-edges wording.
func TestStartCampaign_DanglingExcludedIncompleteDetails_NamesIncludeOmitRemedy(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"campaign_dangling_dependency","message":"dangling edge","details":{"epic_ref":"#25","dangling_excluded_incomplete":["issue:101->issue:100"]}}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#25"})
	if err == nil {
		t.Fatal("err = nil, want mapping")
	}
	for _, want := range []string{"campaign_dangling_dependency", "include it in items", "omit items"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "fix the epic's dependency edges") {
		t.Errorf("excluded-incomplete-only err %q must not name the generic fix-the-edges remedy", err.Error())
	}
}

// TestStartCampaign_DanglingBothDetails_RendersBothCauses is the #2120
// both-present branch: details carrying BOTH category keys render both the
// not_child and the excluded-incomplete remedies.
func TestStartCampaign_DanglingBothDetails_RendersBothCauses(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"campaign_dangling_dependency","message":"dangling edges","details":{"epic_ref":"#25","dangling_not_child":["issue:27->issue:999"],"dangling_excluded_incomplete":["issue:101->issue:100"]}}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#25"})
	if err == nil {
		t.Fatal("err = nil, want mapping")
	}
	for _, want := range []string{"campaign_dangling_dependency", "not a fellow child", "include it in items", "omit items", "#25"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestStartCampaign_DanglingDetails_SourceToConsumer closes the serialization
// seam binding condition 1(b) names: it threads a REAL categorized
// campaign.DanglingDependencyError through the REAL server details-map builder
// (server.DanglingDependencyDetails) into the REAL MCP operator-message render
// in ONE flow — so the error-categorization → details-keys → operator-message
// path is covered source-to-consumer, not split across a server-only test that
// asserts the keys and an MCP-only test fed hand-written details JSON (#2120).
//
// A key rename on only one side of the seam now fails this test: the details map
// is produced by the server (not a literal), so the MCP consumer's key lookups
// must agree with what the server actually emits for the remedy to render.
func TestStartCampaign_DanglingDetails_SourceToConsumer(t *testing.T) {
	// A real categorized error: one out-of-epic edge (NotChild) and one
	// included→excluded-incomplete edge, exactly as campaign.Assemble returns.
	de := &campaign.DanglingDependencyError{
		NotChild:           []workmgmt.DependsEdge{{From: 27, To: 999}},
		ExcludedIncomplete: []workmgmt.DependsEdge{{From: 101, To: 100}},
	}
	// Build the 422 body with the REAL server-side details map + message, then
	// serialize the same error envelope the server writes on the wire.
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    "campaign_dangling_dependency",
			"message": de.Error(),
			"details": server.DanglingDependencyDetails(de, "#25"),
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = string(body)
	r := newResolver(srv, nil)

	_, _, gotErr := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#25"})
	if gotErr == nil {
		t.Fatal("err = nil, want campaign_dangling_dependency mapping")
	}
	// Both category keys are present in the real details map, so the operator
	// message must name BOTH remedies (not_child fix-the-edges + include/omit).
	for _, want := range []string{"campaign_dangling_dependency", "not a fellow child", "include it in items", "omit items", "#25"} {
		if !strings.Contains(gotErr.Error(), want) {
			t.Errorf("err %q missing %q", gotErr.Error(), want)
		}
	}
}

func TestStartCampaign_RepoUnconfigured_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusServiceUnavailable
	fb.createCampaignErr = `{"error":{"code":"campaign_repo_unconfigured","message":"campaigns endpoint requires a configured campaign repository"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#1"})
	if err == nil || !strings.Contains(err.Error(), "campaign_repo_unconfigured") {
		t.Fatalf("err = %v, want campaign_repo_unconfigured mapping", err)
	}
}

// TestStartCampaign_ForbiddenScope_SurfacesError proves a runner-bound token's
// 403 (no write:campaigns) surfaces as a tool error rather than a silent
// success — the auth path the plan notes is covered by an error-mapping test.
func TestStartCampaign_ForbiddenScope_SurfacesError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusForbidden
	fb.createCampaignErr = `{"error":{"code":"insufficient_scope","message":"token lacks write:campaigns"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#1"})
	if err == nil || !strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("err = %v, want the 403 insufficient_scope surfaced", err)
	}
}

// --- fishhawk_get_campaign_status (E25.8 / #1447) ---

// TestGetCampaignStatus_HappyPath_ReturnsRollupAndNextActions drives the chain
// end-to-end: the path id round-trips, and the rollup + next_action + the
// embedded next_actions classification all decode back.
func TestGetCampaignStatus_HappyPath_ReturnsRollupAndNextActions(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.campaignStatusByID[id] = CampaignStatus{
		Campaign: Campaign{ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "running", PausePolicy: "pause_campaign"},
		Items: []CampaignItem{
			{ID: uuid.NewString(), IssueRef: "#26", DependsOn: []string{}, State: "eligible"},
		},
		Rollup:     CampaignRollup{Eligible: []string{"#26"}, Blocked: []string{}, Running: []string{}, Done: []string{}, Failed: []string{}, Cancelled: []string{}, Paused: []string{}},
		NextAction: CampaignNextAction{Action: "start_run", IssueRef: "#26", Detail: "this item's dependencies are satisfied"},
	}
	r := newResolver(srv, nil)

	_, out, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: id.String()})
	if err != nil {
		t.Fatalf("getCampaignStatus: %v", err)
	}
	if fb.getCampaignStatusID != id {
		t.Errorf("backend got campaign id %s, want %s", fb.getCampaignStatusID, id)
	}
	if len(out.Rollup.Eligible) != 1 || out.Rollup.Eligible[0] != "#26" {
		t.Errorf("Rollup.Eligible = %+v", out.Rollup.Eligible)
	}
	if out.NextAction.Action != "start_run" || out.NextAction.IssueRef != "#26" {
		t.Errorf("NextAction = %+v", out.NextAction)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatalf("NextActions should be a non-empty classification, got %+v", out.NextActions)
	}
	if out.NextActions.Actions[0].Action != "fishhawk_start_run" {
		t.Errorf("classified action = %q, want fishhawk_start_run", out.NextActions.Actions[0].Action)
	}
	if got := out.NextActions.Actions[0].Params["trigger_ref"]; got != "#26" {
		t.Errorf("classified start_run trigger_ref = %q, want #26", got)
	}
}

// TestGetCampaignStatus_OperatorAgentOverride_Returned proves a campaign-level
// operator_agent override decodes back onto the status surface's Campaign mirror
// (E25.12 / #1451) so the rollup can display the contract governing every
// issue-run wholesale.
func TestGetCampaignStatus_OperatorAgentOverride_Returned(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.campaignStatusByID[id] = CampaignStatus{
		Campaign: Campaign{
			ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "running", PausePolicy: "pause_campaign",
			OperatorAgent: map[string]any{"may_retry": "infra_flake"},
		},
		Items:      []CampaignItem{},
		Rollup:     CampaignRollup{Eligible: []string{}, Blocked: []string{}, Running: []string{}, Done: []string{}, Failed: []string{}, Cancelled: []string{}, Paused: []string{}},
		NextAction: CampaignNextAction{Action: "wait", Detail: "items are running or blocked"},
	}
	r := newResolver(srv, nil)

	_, out, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: id.String()})
	if err != nil {
		t.Fatalf("getCampaignStatus: %v", err)
	}
	if out.Campaign.OperatorAgent["may_retry"] != "infra_flake" {
		t.Errorf("status Campaign.OperatorAgent = %+v", out.Campaign.OperatorAgent)
	}
}

func TestGetCampaignStatus_InvalidUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestGetCampaignStatus_NotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.campaignStatusStatus = http.StatusNotFound
	fb.campaignStatusErr = `{"error":{"code":"campaign_not_found","message":"no campaign with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "campaign_not_found") {
		t.Fatalf("err = %v, want campaign_not_found mapping", err)
	}
}

// --- fishhawk_resume_campaign (E25.8 / #1447) ---

// TestResumeCampaign_HappyPath_PostsToResumePath drives the chain: the path id
// round-trips and the updated (resumed) campaign decodes back.
func TestResumeCampaign_HappyPath_PostsToResumePath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.resumeCampaignResp = Campaign{ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "running", PausePolicy: "pause_campaign"}
	r := newResolver(srv, nil)

	_, out, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: id.String()})
	if err != nil {
		t.Fatalf("resumeCampaign: %v", err)
	}
	if fb.resumeCampaignID != id {
		t.Errorf("backend got resume id %s, want %s", fb.resumeCampaignID, id)
	}
	if out.Campaign.State != "running" {
		t.Errorf("Campaign.State = %q, want running", out.Campaign.State)
	}
}

func TestResumeCampaign_InvalidUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: "nope"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestResumeCampaign_NotPaused_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.resumeCampaignStatus = http.StatusConflict
	fb.resumeCampaignErr = `{"error":{"code":"campaign_not_paused","message":"campaign has no paused state to resume"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want campaign_not_paused mapping")
	}
	for _, want := range []string{"campaign_not_paused", "nothing to resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

func TestResumeCampaign_NotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.resumeCampaignStatus = http.StatusNotFound
	fb.resumeCampaignErr = `{"error":{"code":"campaign_not_found","message":"no campaign with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "campaign_not_found") {
		t.Fatalf("err = %v, want campaign_not_found mapping", err)
	}
}

// --- fishhawk_start_campaign_item_run (E26.2 / #1481) ---

// TestStartCampaignItemRun_HappyPath_PostsBodyDecodesRunItem drives the whole
// tool→client→wire→decode chain: the input's issue_ref/workflow_id/workflow_ref/
// runner_kind reach the POST body (at the campaign-id path), and the {run,item}
// response decodes back out.
func TestStartCampaignItemRun_HappyPath_PostsBodyDecodesRunItem(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	runID := uuid.NewString()
	fb.startCampaignItemRunResp = StartCampaignItemRunResult{
		Run:  Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: "pending", RunnerKind: "local"},
		Item: CampaignItem{ID: uuid.NewString(), IssueRef: "issue:100", State: "running", RunID: runID},
	}
	r := newResolver(srv, nil)

	_, out, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID:  id.String(),
		IssueRef:    "issue:100",
		WorkflowID:  "feature_change",
		WorkflowRef: "main",
		RunnerKind:  "local",
	})
	if err != nil {
		t.Fatalf("startCampaignItemRun: %v", err)
	}
	if fb.startCampaignItemRunID != id {
		t.Errorf("path campaign_id = %s, want %s", fb.startCampaignItemRunID, id)
	}
	if fb.startCampaignItemRunBody.IssueRef != "issue:100" ||
		fb.startCampaignItemRunBody.WorkflowID != "feature_change" ||
		fb.startCampaignItemRunBody.WorkflowRef != "main" ||
		fb.startCampaignItemRunBody.RunnerKind != "local" {
		t.Errorf("backend got body = %+v", fb.startCampaignItemRunBody)
	}
	if out.Run.ID != runID || out.Run.RunnerKind != "local" {
		t.Errorf("decoded run = %+v, want id=%s runner_kind=local", out.Run, runID)
	}
	if out.Item.IssueRef != "issue:100" || out.Item.State != "running" || out.Item.RunID != runID {
		t.Errorf("decoded item = %+v, want issue:100 running linked to %s", out.Item, runID)
	}
}

// TestStartCampaignItemRun_OmittedOptionalFields_LeavesBodyEmpty pins that
// omitting workflow_ref/runner_kind sends empty values (the backend defaults
// them: default branch + github_actions).
func TestStartCampaignItemRun_OmittedOptionalFields_LeavesBodyEmpty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(),
		IssueRef:   "issue:1",
		WorkflowID: "feature_change",
	})
	if err != nil {
		t.Fatalf("startCampaignItemRun: %v", err)
	}
	if fb.startCampaignItemRunBody.WorkflowRef != "" || fb.startCampaignItemRunBody.RunnerKind != "" {
		t.Errorf("optional fields not empty: %+v", fb.startCampaignItemRunBody)
	}
}

// TestStartCampaignItemRun_BadCampaignID_FailsLocally proves the UUID guard
// rejects before any HTTP call.
func TestStartCampaignItemRun_BadCampaignID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: "not-a-uuid", IssueRef: "issue:1", WorkflowID: "feature_change",
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation", err)
	}
	if fb.startCampaignItemRunBody.IssueRef != "" {
		t.Errorf("backend was called despite bad campaign_id: %+v", fb.startCampaignItemRunBody)
	}
}

// TestStartCampaignItemRun_MissingIssueRef_FailsLocally proves the empty
// issue_ref guard rejects before any HTTP call.
func TestStartCampaignItemRun_MissingIssueRef_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(), WorkflowID: "feature_change",
	})
	if err == nil || !strings.Contains(err.Error(), "issue_ref is required") {
		t.Fatalf("err = %v, want local issue_ref-required validation", err)
	}
	if fb.startCampaignItemRunID != uuid.Nil {
		t.Errorf("backend was called despite missing issue_ref")
	}
}

// TestStartCampaignItemRun_MissingWorkflowID_FailsLocally proves the empty
// workflow_id guard rejects before any HTTP call.
func TestStartCampaignItemRun_MissingWorkflowID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(), IssueRef: "issue:1",
	})
	if err == nil || !strings.Contains(err.Error(), "workflow_id is required") {
		t.Fatalf("err = %v, want local workflow_id-required validation", err)
	}
}

// TestStartCampaignItemRun_ItemNotEligible_MapsActionableError maps the DAG-gate
// refusal onto an operator-actionable error naming the surface to poll.
func TestStartCampaignItemRun_ItemNotEligible_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startCampaignItemRunStatus = http.StatusConflict
	fb.startCampaignItemRunErr = `{"error":{"code":"item_not_eligible","message":"blocked on dependency issue:99; it must succeed before this item can start"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(), IssueRef: "issue:100", WorkflowID: "feature_change",
	})
	if err == nil {
		t.Fatal("err = nil, want item_not_eligible mapping")
	}
	for _, want := range []string{"item_not_eligible", "issue:99", "fishhawk_get_campaign_status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestStartCampaignItemRun_ItemHumanLed_MapsActionableError maps the DISTINCT
// human-led refusal (#1697): a deps-satisfied autonomy:low item yields
// item_human_led, and the wrapper routes to out-of-band handling + re-poll
// WITHOUT the "start the ref" suffix the item_not_eligible case keeps. The fake
// backend returns the byte-real error JSON the server writes (status + code +
// the human-led detail shape), so the decode/wrap path runs against the true
// wire shape rather than a hand-built apiError.
func TestStartCampaignItemRun_ItemHumanLed_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startCampaignItemRunStatus = http.StatusConflict
	fb.startCampaignItemRunErr = `{"error":{"code":"item_human_led","message":"item is deps-satisfied but autonomy:low (human-led); a human must lead it — do not start an agent run (next_action: attend_human_led)"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(), IssueRef: "issue:101", WorkflowID: "feature_change",
	})
	if err == nil {
		t.Fatal("err = nil, want item_human_led mapping")
	}
	for _, want := range []string{"item_human_led", "human-led", "fishhawk_get_campaign_status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "start the ref") || strings.Contains(err.Error(), "next_action names") {
		t.Errorf("err must NOT tell the caller to start a ref: %q", err.Error())
	}
}

// TestStartCampaignItemRun_CampaignNotStartable_MapsActionableError maps the
// paused/terminal-campaign refusal.
func TestStartCampaignItemRun_CampaignNotStartable_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startCampaignItemRunStatus = http.StatusConflict
	fb.startCampaignItemRunErr = `{"error":{"code":"campaign_not_startable","message":"campaign is not in a state that can start an item run"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(), IssueRef: "issue:1", WorkflowID: "feature_change",
	})
	if err == nil || !strings.Contains(err.Error(), "campaign_not_startable") {
		t.Fatalf("err = %v, want campaign_not_startable mapping", err)
	}
	if !strings.Contains(err.Error(), "fishhawk_resume_campaign") {
		t.Errorf("err %q should point at the resume tool", err.Error())
	}
}

// TestStartCampaignItemRun_ItemNotFound_MapsActionableError maps the
// unknown-issue_ref refusal.
func TestStartCampaignItemRun_ItemNotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startCampaignItemRunStatus = http.StatusNotFound
	fb.startCampaignItemRunErr = `{"error":{"code":"campaign_item_not_found","message":"no campaign item with that issue_ref"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(), IssueRef: "issue:nope", WorkflowID: "feature_change",
	})
	if err == nil || !strings.Contains(err.Error(), "campaign_item_not_found") {
		t.Fatalf("err = %v, want campaign_item_not_found mapping", err)
	}
}

// --- no-epic (items-without-epic_ref) TRUE end-to-end test (E48.36 / #2051,
// binding condition 1) ---

// fakeMCPResolverProvider is a workmgmt.Provider that implements
// IssueSetDependencyResolver, so the E2E test can drive the REAL server no-epic
// branch without any GitHub. It returns a canned result over the named issues.
type fakeMCPResolverProvider struct {
	name     string
	result   *workmgmt.EpicChildrenResult
	captured workmgmt.IssueSetRequest
}

func (f *fakeMCPResolverProvider) Name() string { return f.name }

func (f *fakeMCPResolverProvider) File(_ context.Context, _ workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	return &workmgmt.CreatedItem{Provider: f.name}, nil
}

func (f *fakeMCPResolverProvider) ResolveDependencies(_ context.Context, req workmgmt.IssueSetRequest) (*workmgmt.EpicChildrenResult, error) {
	f.captured = req
	return f.result, nil
}

// stubMCPAPITokens is a minimal apitoken.Repository that resolves exactly one
// bearer string to a canned token (with write:campaigns), so the E2E test can
// drive the REAL server auth middleware. Every other method is nil via the
// embedded interface — an accidental call panics loudly.
type stubMCPAPITokens struct {
	apitoken.Repository
	tok *apitoken.Token
}

func (s *stubMCPAPITokens) Authenticate(_ context.Context, plaintext string) (*apitoken.Token, error) {
	if s.tok != nil && plaintext == s.tok.PlainText {
		return s.tok, nil
	}
	return nil, apitoken.ErrNotFound
}

// TestStartCampaign_NoEpic_E2E_ThroughRealServer is the binding-condition-1 TRUE
// end-to-end test: it drives fishhawk_start_campaign with items present and
// epic_ref ABSENT through the REAL MCP client request-build/serialization → an
// httptest server wrapping the REAL server.Handler() (so handleCreateCampaign
// runs, including its bearerAuth + requireWriteScope) → the no-epic branch → a
// fake IssueSetDependencyResolver → campaign.Assemble → Persist (real Postgres),
// and asserts the campaign assembles over EXACTLY the named issues. This proves
// epic_ref is emitted as "" and the server treats "" as ABSENT (routing to the
// no-epic branch) AND that the branch wiring reaches ResolveDependencies — so a
// serialization change (epic_ref dropped/renamed) or a branch-decision
// regression turns this test RED.
func TestStartCampaign_NoEpic_E2E_ThroughRealServer(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	// A no-epic resolver over exactly {101, 102}: 102 depends on 101.
	prov := &fakeMCPResolverProvider{
		name: workmgmt.Default().Provider,
		result: &workmgmt.EpicChildrenResult{
			Children: []workmgmt.EpicChild{{Number: 101, Title: "first"}, {Number: 102, Title: "second"}},
			Edges:    []workmgmt.DependsEdge{{From: 102, To: 101}},
		},
	}
	workmgmt.Register(prov)

	const bearer = "fhk_noepic_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"write:campaigns"}, PlainText: bearer,
	}}
	// GitHub nil: the runless install resolution is skipped, so scope stays zero
	// and the fake resolver (which ignores scope) still runs.
	s := server.New(server.Config{CampaignRepo: repo, APITokenRepo: tokRepo})
	// Interpose a capturing middleware so the test can inspect the RAW request
	// body the real MCP client serialized. Routing to the no-epic branch alone is
	// vacuous for the load-bearing serialization claim: the server trims and
	// branches identically whether epic_ref is "" or absent, so only the raw wire
	// body proves the client emitted epic_ref as "" (non-omitempty) rather than
	// dropping the key. If a future refactor re-adds omitempty and drops the key,
	// this assertion — not the routing check — turns the test RED.
	handler := s.Handler()
	var capturedBody []byte
	capture := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/v0/campaigns" {
			b, rerr := io.ReadAll(req.Body)
			if rerr != nil {
				t.Errorf("read captured request body: %v", rerr)
			}
			capturedBody = b
			req.Body = io.NopCloser(bytes.NewReader(b))
		}
		handler.ServeHTTP(w, req)
	})
	httpSrv := httptest.NewServer(capture)
	t.Cleanup(httpSrv.Close)

	r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})}

	// items present, epic_ref ABSENT — the load-bearing no-epic serialization.
	_, out, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:  "kuhlman-labs/fishhawk",
		Items: []string{"issue:101", "issue:102"},
	})
	if err != nil {
		t.Fatalf("startCampaign (no-epic E2E): %v", err)
	}
	// The REAL client emitted epic_ref as "" on the wire (not omitted): the
	// load-bearing no-epic serialization binding condition 1 requires. This is the
	// non-vacuous assertion — routing works whether the key is "" or absent, so the
	// raw body is the only proof the client kept the key and serialized "".
	if !bytes.Contains(capturedBody, []byte(`"epic_ref":""`)) {
		t.Errorf("request body = %s, want it to carry \"epic_ref\":\"\" (client must serialize the no-epic sentinel, not drop the key)", capturedBody)
	}
	// The server routed to the no-epic branch and forwarded the named items.
	if got := prov.captured.Items; len(got) != 2 || got[0] != "issue:101" || got[1] != "issue:102" {
		t.Fatalf("resolver got items = %v, want [issue:101 issue:102] (proves epic_ref==\"\" routed to the no-epic branch)", got)
	}
	if out.Campaign.EpicRef != "" {
		t.Errorf("created epic_ref = %q, want empty (no-epic variant)", out.Campaign.EpicRef)
	}

	// The campaign assembled + persisted over EXACTLY the named issues.
	id, perr := uuid.Parse(out.Campaign.ID)
	if perr != nil {
		t.Fatalf("parse created id %q: %v", out.Campaign.ID, perr)
	}
	items, lerr := repo.ListCampaignItemsForCampaign(context.Background(), id)
	if lerr != nil {
		t.Fatalf("list items: %v", lerr)
	}
	if len(items) != 2 {
		t.Fatalf("persisted items = %+v, want exactly {101,102}", items)
	}
	byRef := map[string][]string{}
	for _, it := range items {
		byRef[it.IssueRef] = it.DependsOn
	}
	if _, ok := byRef["issue:101"]; !ok {
		t.Errorf("missing issue:101; items = %+v", items)
	}
	if deps, ok := byRef["issue:102"]; !ok || len(deps) != 1 || deps[0] != "issue:101" {
		t.Errorf("issue:102 depends_on = %v, want [issue:101]", deps)
	}
}
