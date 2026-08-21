package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

// TestStartCampaign_ThreadsWorkingDirThroughRealClient drives the PRODUCTION
// path — the tool handler → the real apiClient → the wire — and asserts the
// campaign-level binding (E48.87 / #2527) arrives in the POST body. It is
// deliberately NOT a body the test constructed and handed to the client: a test
// that marshals campaignCreateRequest itself cannot detect
// StartCampaignInput.WorkingDir never being threaded into the CreateCampaign
// call, which is exactly the seam that breaks. An un-cleaned absolute value is
// used so the Clean is pinned too, and the omitted case proves an unbound
// campaign sends no working_dir key at all.
//
// Counterfactual: drop `workingDir` from the r.api.CreateCampaign argument list
// (pass "") → the POST body carries no binding → RED.
func TestStartCampaign_ThreadsWorkingDirThroughRealClient(t *testing.T) {
	t.Run("bound", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)

		base := t.TempDir()
		uncleaned := base + "/./" // absolute but not cleaned
		_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
			Repo:       "kuhlman-labs/fishhawk",
			EpicRef:    "#25",
			WorkingDir: uncleaned,
		})
		if err != nil {
			t.Fatalf("startCampaign: %v", err)
		}
		if got := fb.createCampaignBody.WorkingDir; got != filepath.Clean(uncleaned) {
			t.Errorf("POST body working_dir = %q, want the cleaned %q — the tool must thread StartCampaignInput.WorkingDir into the client call", got, filepath.Clean(uncleaned))
		}
	})

	t.Run("omitted_sends_no_key", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)

		_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
			Repo: "kuhlman-labs/fishhawk", EpicRef: "#25",
		})
		if err != nil {
			t.Fatalf("startCampaign: %v", err)
		}
		if got := fb.createCampaignBody.WorkingDir; got != "" {
			t.Errorf("POST body working_dir = %q, want empty (an unbound campaign sends no binding)", got)
		}
	})
}

// TestStartCampaign_RelativeWorkingDir_Refused is counterfactual C5: the MCP
// start_campaign syntactic guard (E48.87 / #2527). A relative binding would be
// inherited by every item run in the batch, so it is refused BEFORE the backend
// is dialed — asserted through the explicit never-dialed seam
// (createCampaignCalls == 0) against a REACHABLE in-test server, which
// distinguishes a local refusal from a backend rejection or a connection error.
// Refused for any campaign, exactly as the per-item guard is refused for any
// runner_kind: a relative path is unresolvable no matter who consumes it.
//
// Counterfactual: delete the `!filepath.IsAbs(workingDir)` guard in startCampaign
// → the tool dials the backend and returns no error → RED on both the error
// assertion and the zero-request assertion.
func TestStartCampaign_RelativeWorkingDir_Refused(t *testing.T) {
	// A REACHABLE in-test server that counts every request it receives. Pointing
	// at an unreachable address instead would make a connection error
	// indistinguishable from the guard firing.
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","repo":"kuhlman-labs/fishhawk","epic_ref":"#25","state":"pending","pause_policy":"pause_campaign"}`))
	}))
	t.Cleanup(srv.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: srv.URL, apiToken: "fhk_test"})}

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:       "kuhlman-labs/fishhawk",
		EpicRef:    "#25",
		WorkingDir: "./sub", // relative
	})
	if err == nil || !strings.Contains(err.Error(), "working_dir") || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want a refusal naming working_dir and absolute", err)
	}
	if n := atomic.LoadInt32(&requests); n != 0 {
		t.Errorf("backend saw %d requests; a relative-path refusal must never dial the backend", n)
	}

	// Negative control: the SAME server, an ABSOLUTE working_dir → the guard does
	// not fire and the request goes through. Without this the guard could pass by
	// refusing every campaign.
	if _, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:       "kuhlman-labs/fishhawk",
		EpicRef:    "#25",
		WorkingDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("absolute working_dir must be accepted, got %v", err)
	}
	if n := atomic.LoadInt32(&requests); n != 1 {
		t.Errorf("backend saw %d requests after the absolute call, want exactly 1", n)
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

// TestGetCampaignStatus_AcceptanceSlot_EndToEnd drives the REAL getCampaignStatus
// handler across the three seams this feature spans (E48.71 / #2503): the
// campaign-status read, the per-run stage read, and the live network probe. One
// item's acceptance stage is running (the holder), another's is
// awaiting_host_dispatch (a waiter), and a live httptest /healthz serves a git_sha
// reached via a FISHHAWK_PREVIEW_ADDR env override on newResolver. Per-layer units
// would each pass while the seam between them broke, so this asserts the shipped
// values: the held_by issue ref, the waiting issue ref, the held state, and the
// serving git_sha the probe target actually served — a scope-satisfying no-op edit
// fails it.
func TestGetCampaignStatus_AcceptanceSlot_EndToEnd(t *testing.T) {
	shrinkSlotProbeTimeout(t, 300*time.Millisecond)
	fb, srv := newFakeBackend(t)
	hz := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)

	holder, holderRun := seedItemWithAcceptanceStage(t, fb, "#26", "running", "running")
	waiter, _ := seedItemWithAcceptanceStage(t, fb, "#27", "running", "awaiting_host_dispatch")

	id := uuid.New()
	fb.campaignStatusByID[id] = CampaignStatus{
		Campaign:   Campaign{ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "running", PausePolicy: "pause_campaign"},
		Items:      []CampaignItem{holder, waiter},
		Rollup:     CampaignRollup{Running: []string{"#26", "#27"}, Eligible: []string{}, Blocked: []string{}, Done: []string{}, Failed: []string{}, Cancelled: []string{}, Paused: []string{}},
		NextAction: CampaignNextAction{Action: "wait", Detail: "items are running"},
	}
	r := newResolver(srv, map[string]string{acceptancePreviewAddrEnv: hostOf(hz.URL)})

	_, out, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: id.String()})
	if err != nil {
		t.Fatalf("getCampaignStatus: %v", err)
	}
	if out.AcceptanceSlot == nil {
		t.Fatal("acceptance_slot should be present when an item is at the acceptance gate")
	}
	slot := out.AcceptanceSlot
	if !slot.Shared {
		t.Error("Shared should be true (acceptance validates against ONE shared slot)")
	}
	if slot.TargetHost != hostOf(hz.URL) || slot.TargetHostSource != "env" {
		t.Errorf("TargetHost,Source = %q,%q; want %q,env", slot.TargetHost, slot.TargetHostSource, hostOf(hz.URL))
	}
	if slot.State != "held" || slot.ServingGitSHA != "abc1234def" {
		t.Errorf("State,ServingGitSHA = %q,%q; want held,abc1234def", slot.State, slot.ServingGitSHA)
	}
	if slot.HeldBy == nil || slot.HeldBy.IssueRef != "#26" || slot.HeldBy.RunID != holderRun.String() {
		t.Errorf("HeldBy = %+v, want issue #26 run %s", slot.HeldBy, holderRun)
	}
	if len(slot.Waiting) != 1 || slot.Waiting[0].IssueRef != "#27" {
		t.Errorf("Waiting = %+v, want one entry for #27", slot.Waiting)
	}
}

// TestGetCampaignStatus_NoAcceptanceParticipant_OmitsSlot is the companion
// negative (E48.71 / #2503): a campaign with no acceptance-participating item
// returns AcceptanceSlot == nil and leaves the pre-existing rollup / next_actions
// assertions intact.
func TestGetCampaignStatus_NoAcceptanceParticipant_OmitsSlot(t *testing.T) {
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
	if out.AcceptanceSlot != nil {
		t.Errorf("acceptance_slot should be omitted when no item is at the acceptance gate, got %+v", out.AcceptanceSlot)
	}
	// Pre-existing assertions still hold.
	if len(out.Rollup.Eligible) != 1 || out.Rollup.Eligible[0] != "#26" {
		t.Errorf("Rollup.Eligible = %+v", out.Rollup.Eligible)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatalf("NextActions should be a non-empty classification, got %+v", out.NextActions)
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

// --- fishhawk_cancel_campaign (#2355) ---

// TestCancelCampaignTool_RoundTrip drives the tool→client→wire→decode chain: the
// path id round-trips to the backend and the cancelled campaign decodes back.
func TestCancelCampaignTool_RoundTrip(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.cancelCampaignResp = Campaign{ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "cancelled", PausePolicy: "pause_campaign"}
	r := newResolver(srv, nil)

	_, out, err := r.cancelCampaign(context.Background(), nil, CancelCampaignInput{CampaignID: id.String()})
	if err != nil {
		t.Fatalf("cancelCampaign: %v", err)
	}
	if fb.cancelCampaignID != id {
		t.Errorf("backend got cancel id %s, want %s", fb.cancelCampaignID, id)
	}
	if out.Campaign.State != "cancelled" {
		t.Errorf("Campaign.State = %q, want cancelled", out.Campaign.State)
	}
}

func TestCancelCampaign_InvalidUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.cancelCampaign(context.Background(), nil, CancelCampaignInput{CampaignID: "nope"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestCancelCampaign_NotCancellable_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.cancelCampaignStatus = http.StatusConflict
	fb.cancelCampaignErr = `{"error":{"code":"campaign_not_cancellable","message":"campaign is already in a terminal state and cannot be cancelled"}}`
	r := newResolver(srv, nil)

	_, _, err := r.cancelCampaign(context.Background(), nil, CancelCampaignInput{CampaignID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want campaign_not_cancellable mapping")
	}
	for _, want := range []string{"campaign_not_cancellable", "already terminal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

func TestCancelCampaign_NotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.cancelCampaignStatus = http.StatusNotFound
	fb.cancelCampaignErr = `{"error":{"code":"campaign_not_found","message":"no campaign with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.cancelCampaign(context.Background(), nil, CancelCampaignInput{CampaignID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "campaign_not_found") {
		t.Fatalf("err = %v, want campaign_not_found mapping", err)
	}
}

// --- fishhawk_start_campaign_item_run (E26.2 / #1481) ---

// TestStartCampaignItemRun_HappyPath_PostsBodyDecodesRunItem drives the whole
// tool→client→wire→decode chain: the input's issue_ref/workflow_id/workflow_ref/
// runner_kind/working_dir reach the POST body (at the campaign-id path), and the
// {run,item} response — including the run's echoed working_dir binding (E48.69 /
// #2498) — decodes back out (E2 of the plan's cross-boundary strategy).
func TestStartCampaignItemRun_HappyPath_PostsBodyDecodesRunItem(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	runID := uuid.NewString()
	wd := t.TempDir() // an absolute checkout path
	fb.startCampaignItemRunResp = StartCampaignItemRunResult{
		Run:  Run{ID: runID, Repo: "kuhlman-labs/fishhawk", State: "pending", RunnerKind: "local", WorkingDir: wd},
		Item: CampaignItem{ID: uuid.NewString(), IssueRef: "issue:100", State: "running", RunID: runID},
	}
	r := newResolver(srv, nil)

	_, out, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID:  id.String(),
		IssueRef:    "issue:100",
		WorkflowID:  "feature_change",
		WorkflowRef: "main",
		RunnerKind:  "local",
		WorkingDir:  wd,
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
	if fb.startCampaignItemRunBody.WorkingDir != wd {
		t.Errorf("working_dir did not reach the POST body: got %q, want %q", fb.startCampaignItemRunBody.WorkingDir, wd)
	}
	if out.Run.ID != runID || out.Run.RunnerKind != "local" {
		t.Errorf("decoded run = %+v, want id=%s runner_kind=local", out.Run, runID)
	}
	if out.Run.WorkingDir != wd {
		t.Errorf("decoded run working_dir = %q, want the bound %q", out.Run.WorkingDir, wd)
	}
	if out.Item.IssueRef != "issue:100" || out.Item.State != "running" || out.Item.RunID != runID {
		t.Errorf("decoded item = %+v, want issue:100 running linked to %s", out.Item, runID)
	}
}

// TestStartCampaignItemRun_OmittedOptionalFields_LeavesBodyEmpty pins that
// omitting workflow_ref/runner_kind/working_dir sends empty values (the backend
// defaults them: default branch + github_actions, unbound run). runner_kind is
// omitted here (the github_actions path), so the local-requires-working_dir
// guard does not fire.
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
	if fb.startCampaignItemRunBody.WorkflowRef != "" || fb.startCampaignItemRunBody.RunnerKind != "" ||
		fb.startCampaignItemRunBody.WorkingDir != "" {
		t.Errorf("optional fields not empty: %+v", fb.startCampaignItemRunBody)
	}
}

// TestStartCampaignItemRun_LocalOmittedWorkingDirReachesBackend pins the E48.87
// / #2527 RELOCATION of #2498's M1 refusal. local + omitted working_dir is no
// longer refused in the MCP tool, because an omitted value is no longer
// necessarily missing: the CAMPAIGN may carry a binding this run inherits, and
// only the backend can see it. So the call must now REACH the backend (exactly
// once) rather than being refused locally — the backend decides, and refuses
// working_dir_required when the campaign is unbound too
// (TestStartCampaignItemRun_WorkingDirRequired_MapsRemedy asserts that mapping,
// TestStartCampaignItemRun_LocalUnboundCampaign_Refused in the server package
// asserts the refusal itself). github_actions + omitted still succeeds, the
// unchanged negative control.
//
// Counterfactual: restoring the removed local+omitted guard refuses locally and
// dials nothing → RED on the reached-the-backend assertion.
func TestStartCampaignItemRun_LocalOmittedWorkingDirReachesBackend(t *testing.T) {
	t.Run("local_no_working_dir_reaches_backend", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)

		_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
			CampaignID: uuid.NewString(),
			IssueRef:   "issue:1",
			WorkflowID: "feature_change",
			RunnerKind: "local",
		})
		if err != nil {
			t.Fatalf("local with no working_dir must reach the backend (the campaign may bind it), got %v", err)
		}
		if fb.startCampaignItemRunCalls != 1 {
			t.Errorf("backend dialed %d times, want exactly 1 — the local+omitted refusal moved server-side (#2527), so the MCP tool must not short-circuit it", fb.startCampaignItemRunCalls)
		}
		// Nothing is invented on the way: an omitted value stays omitted, so the
		// backend sees "no per-item override" and applies the campaign binding.
		if fb.startCampaignItemRunBody.WorkingDir != "" {
			t.Errorf("POST body working_dir = %q, want empty (an omitted per-item value must not be back-filled by the tool)", fb.startCampaignItemRunBody.WorkingDir)
		}
	})

	t.Run("github_actions_no_working_dir_succeeds", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)

		_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
			CampaignID: uuid.NewString(),
			IssueRef:   "issue:1",
			WorkflowID: "feature_change",
			RunnerKind: "github_actions",
		})
		if err != nil {
			t.Fatalf("github_actions with no working_dir must succeed, got %v", err)
		}
		if fb.startCampaignItemRunCalls != 1 {
			t.Errorf("backend dialed %d times, want exactly 1 (the negative control proves the guard does not refuse everything)", fb.startCampaignItemRunCalls)
		}
	})
}

// TestStartCampaignItemRun_RelativeWorkingDirRefusedAnyRunnerKind pins the M3
// refusal (E48.69 / #2498): a relative working_dir is refused for BOTH
// runner_kind local AND github_actions, naming working_dir and absolute, and the
// backend is never dialed. Deleting the IsAbs guard lets both through → RED.
func TestStartCampaignItemRun_RelativeWorkingDirRefusedAnyRunnerKind(t *testing.T) {
	for _, rk := range []string{"local", "github_actions"} {
		t.Run(rk, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)

			_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
				CampaignID: uuid.NewString(),
				IssueRef:   "issue:1",
				WorkflowID: "feature_change",
				RunnerKind: rk,
				WorkingDir: "./sub", // relative
			})
			if err == nil || !strings.Contains(err.Error(), "working_dir") || !strings.Contains(err.Error(), "absolute") {
				t.Fatalf("err = %v, want a refusal naming working_dir and absolute", err)
			}
			if fb.startCampaignItemRunCalls != 0 {
				t.Errorf("backend was dialed %d times; a relative-path refusal must never dial the backend", fb.startCampaignItemRunCalls)
			}
		})
	}
}

// TestStartCampaignItemRun_AbsoluteWorkingDirPassedThroughCleaned pins that an
// absolute (but un-cleaned) working_dir is filepath.Clean'd and passed through to
// the POST body (E48.69 / #2498) rather than refused.
func TestStartCampaignItemRun_AbsoluteWorkingDirPassedThroughCleaned(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	base := t.TempDir()
	uncleaned := base + "/./" // absolute but not cleaned
	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(),
		IssueRef:   "issue:1",
		WorkflowID: "feature_change",
		RunnerKind: "local",
		WorkingDir: uncleaned,
	})
	if err != nil {
		t.Fatalf("absolute working_dir must pass through, got %v", err)
	}
	if fb.startCampaignItemRunBody.WorkingDir != filepath.Clean(uncleaned) {
		t.Errorf("POST body working_dir = %q, want the cleaned %q", fb.startCampaignItemRunBody.WorkingDir, filepath.Clean(uncleaned))
	}
}

// TestStartCampaignItemRun_RefusalIsTransportIndependent pins the M5 requirement
// (E48.69 / #2498, the issue's proposal 3): the working_dir guards fire on BOTH
// the HTTP transport AND stdio. On stdio the pre-change behavior was a silent
// fall-through to the client's cwd, so this test goes red against today's code
// and against any implementation that copies start_run's HTTP-only guard.
func TestStartCampaignItemRun_RefusalIsTransportIndependent(t *testing.T) {
	type refusalCase struct {
		name string
		in   StartCampaignItemRunInput
	}
	// The M3 (relative) refusal inputs. The M1 (local + omitted) case is NO
	// LONGER an MCP-layer refusal as of E48.87 / #2527 — it moved server-side,
	// where the campaign's binding is visible — so it is pinned by
	// TestStartCampaignItemRun_LocalOmittedWorkingDirReachesBackend and the
	// server package's TestStartCampaignItemRun_LocalUnboundCampaign_Refused
	// instead. The relative refusal, which needs no I/O, stays here and stays
	// transport-independent.
	cases := []refusalCase{
		{"relative_local", StartCampaignItemRunInput{IssueRef: "issue:1", WorkflowID: "feature_change", RunnerKind: "local", WorkingDir: "./sub"}},
		{"relative_gha", StartCampaignItemRunInput{IssueRef: "issue:1", WorkflowID: "feature_change", RunnerKind: "github_actions", WorkingDir: "./sub"}},
	}
	for _, httpTransport := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("http=%v/%s", httpTransport, tc.name), func(t *testing.T) {
				fb, srv := newFakeBackend(t)
				r := newResolver(srv, nil)
				r.httpTransport = httpTransport
				in := tc.in
				in.CampaignID = uuid.NewString()

				_, _, err := r.startCampaignItemRun(context.Background(), nil, in)
				if err == nil || !strings.Contains(err.Error(), "working_dir") {
					t.Fatalf("err = %v, want a working_dir refusal on transport http=%v", err, httpTransport)
				}
				if fb.startCampaignItemRunCalls != 0 {
					t.Errorf("backend dialed %d times on transport http=%v; the refusal must never dial the backend", fb.startCampaignItemRunCalls, httpTransport)
				}
			})
		}
	}
}

// TestStartCampaignItemRun_BindsWorkingDirInheritedByResolver is the DIRECT test
// of the inheritance leg the operator's binding condition 1 requires (E48.69 /
// #2498): mint a campaign item run carrying an absolute working_dir, then assert
// the run-aware resolver resolveWorkingDirForRun — the seam the stage verbs
// (run_stage/dispatch_stage/run_children/drive_run) call — returns that bound
// value for an OMITTED input on that run id. This observes the issue's "resolve
// by inheritance with no per-call repetition" clause directly rather than by
// composition. The fake backend persists the minted run (POST body.working_dir →
// getRunByID) so the resolver's GET reads it back.
//
// Counterfactual: remove the `WorkingDir: workingDir` binding from the
// r.api.StartCampaignItemRun call — the minted run then carries no binding, and
// resolveWorkingDirForRun over HTTP falls through to resolveWorkingDir("") which
// refuses, so the inherit assertion goes RED (an error instead of the bound dir).
func TestStartCampaignItemRun_BindsWorkingDirInheritedByResolver(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true // the inheriting verbs run over HTTP; stdio would fall back to cwd

	wd := t.TempDir()
	_, out, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(),
		IssueRef:   "issue:1",
		WorkflowID: "feature_change",
		RunnerKind: "local",
		WorkingDir: wd,
	})
	if err != nil {
		t.Fatalf("startCampaignItemRun: %v", err)
	}
	mintedRunID, err := uuid.Parse(out.Run.ID)
	if err != nil {
		t.Fatalf("minted run id %q not a UUID: %v", out.Run.ID, err)
	}

	// Inheritance: an OMITTED input on the minted run resolves to the bound dir
	// with no per-call repetition.
	got, err := r.resolveWorkingDirForRun(context.Background(), mintedRunID, "")
	if err != nil {
		t.Fatalf("resolveWorkingDirForRun on the minted run with an omitted input must inherit the binding, got %v", err)
	}
	if got != filepath.Clean(wd) {
		t.Errorf("inherited working_dir = %q, want the bound %q", got, filepath.Clean(wd))
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

// TestStartCampaignItemRun_WorkingDirRequired_MapsRemedy pins the MCP rendering
// of the RELOCATED rung-3 refusal (E48.87 / #2527). The backend owns the
// decision — only it can see whether the campaign carries a binding — so the
// tool's job is to turn the working_dir_required code into an operator-actionable
// remedy. It must name BOTH ways out (bind it once at start_campaign, or pass it
// for this item); naming only one would send the operator back into the per-item
// repetition this issue removes.
//
// The stub's backend message is deliberately TERSE and carries NONE of the
// remedy wording. Without that, the generic `start campaign item run: %w`
// fallback renders the code AND the backend message, so a rich server message
// would satisfy every substring assertion with the mapping deleted — the test
// would be green against a control that is not there (observed: this exact test
// passed under the deletion until the fixture was made terse). Every assertion
// below is therefore on wording the TOOL adds.
func TestStartCampaignItemRun_WorkingDirRequired_MapsRemedy(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startCampaignItemRunStatus = http.StatusBadRequest
	fb.startCampaignItemRunErr = `{"error":{"code":"working_dir_required","message":"no checkout bound"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
		CampaignID: uuid.NewString(), IssueRef: "issue:100", WorkflowID: "feature_change", RunnerKind: "local",
	})
	if err == nil {
		t.Fatal("err = nil, want a working_dir_required mapping")
	}
	// Remedy 1 (bind once at the campaign), remedy 2 (pass it for this item),
	// and the resolve-your-own-checkout instruction — none of which appear in
	// the backend message above.
	for _, want := range []string{
		"bind working_dir ONCE on the campaign at fishhawk_start_campaign",
		"pass an absolute working_dir for this item",
		"resolve your own checkout",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q — the remedy must name both ways out", err.Error(), want)
		}
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

// TestStartCampaignItemRun_OversizedRow_BoundedThroughHandler drives the verb
// that concretely broke on 2026-08-07 at 75,963 characters (#2510). It is a
// MUTATING verb — the run is minted and the item linked by the time this
// response renders — so the row is reduced rather than the response rejected,
// and the elisions block must survive the SDK's in-handler output-schema
// validation.
func TestStartCampaignItemRun_OversizedRow_BoundedThroughHandler(t *testing.T) {
	fb, srv := newFakeBackend(t)
	campaignID := uuid.New()
	runID := uuid.NewString()
	fb.startCampaignItemRunResp = StartCampaignItemRunResult{
		Run:  worstCaseIssueRun(runID),
		Item: CampaignItem{ID: uuid.NewString(), IssueRef: "issue:100", State: "running", RunID: runID},
	}

	raw := callBoundedToolOverSDK(t, srv, nil, registerStartCampaignItemRun, "fishhawk_start_campaign_item_run",
		map[string]any{
			"campaign_id": campaignID.String(),
			"issue_ref":   "issue:100",
			"workflow_id": "feature_change",
			"runner_kind": "github_actions",
		})

	var out StartCampaignItemRunOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertBoundedWithElisions(t, "fishhawk_start_campaign_item_run", raw, out.Elisions, mcpResponseByteBudgetDefault)
	if out.Run.ID != runID {
		t.Errorf("the minted run id was lost to the bound: %+v", out.Run)
	}
	if out.Item.IssueRef != "issue:100" {
		t.Errorf("the linked item was lost to the bound: %+v", out.Item)
	}
}

// TestGetCampaignStatus_Closed_E2E_ThroughRealServer is the cross-boundary
// done-means for the #2681 `closed` next_action: it drives a REAL
// Postgres-backed campaign through the REAL server.Handler() over httptest, the
// REAL MCP apiClient JSON decode, and campaignNextActionsFor rendering in ONE
// test — so a json-tag rename or an enum-name drift between the server's
// campaignNextActionPayload and this package's CampaignNextAction turns it RED
// rather than passing per-layer.
//
// It asserts BOTH terminal arms across the boundary:
//   - a CANCELLED campaign with an unfinished item decodes as `closed` +
//     NextActions.State campaign_closed carrying the single standalone
//     fishhawk_start_run;
//   - a terminal-FAILED campaign WITH a restartable item still decodes as
//     `start_run` with the reopen-flavoured detail.
func TestGetCampaignStatus_Closed_E2E_ThroughRealServer(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	const bearer = "fhk_closed_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"write:campaigns", "read:runs"}, PlainText: bearer,
	}}
	s := server.New(server.Config{CampaignRepo: repo, APITokenRepo: tokRepo})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})}

	// seed builds a campaign in cState whose items are driven to the given
	// states, and returns its id.
	seed := func(t *testing.T, cState campaign.State, items map[string]campaign.ItemState) uuid.UUID {
		t.Helper()
		c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{Repo: "kuhlman-labs/fishhawk", EpicRef: "issue:99"})
		if err != nil {
			t.Fatalf("create campaign: %v", err)
		}
		for ref, st := range items {
			it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: ref})
			if err != nil {
				t.Fatalf("create item %s: %v", ref, err)
			}
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, st); err != nil {
				t.Fatalf("item %s →%s: %v", ref, st, err)
			}
		}
		if _, err := repo.TransitionCampaign(ctx, c.ID, cState); err != nil {
			t.Fatalf("campaign →%s: %v", cState, err)
		}
		return c.ID
	}

	// --- arm 1: a CANCELLED campaign with an unfinished item -> closed ---
	closedID := seed(t, campaign.StateCancelled, map[string]campaign.ItemState{
		"issue:100": campaign.ItemStateSucceeded,
		"issue:101": campaign.ItemStateFailed,
	})
	_, out, err := r.getCampaignStatus(ctx, nil, GetCampaignStatusInput{CampaignID: closedID.String()})
	if err != nil {
		t.Fatalf("getCampaignStatus (closed): %v", err)
	}
	if out.NextAction.Action != "closed" {
		t.Fatalf("decoded next_action = %+v, want action \"closed\" (the server's terminal filter must survive the wire)", out.NextAction)
	}
	if out.NextAction.IssueRef != "issue:101" {
		t.Errorf("decoded issue_ref = %q, want issue:101 (the stranded ref is carried)", out.NextAction.IssueRef)
	}
	// The DETAIL arrived intact — not merely a non-empty string.
	if !strings.Contains(out.NextAction.Detail, "cancelled") || !strings.Contains(out.NextAction.Detail, "fishhawk_start_run") {
		t.Errorf("decoded detail = %q, want it to name the terminal state and the standalone verb", out.NextAction.Detail)
	}
	if out.NextActions == nil || out.NextActions.State != "campaign_closed" {
		t.Fatalf("next_actions = %+v, want State campaign_closed", out.NextActions)
	}
	if len(out.NextActions.Actions) != 1 || out.NextActions.Actions[0].Action != "fishhawk_start_run" {
		t.Errorf("next_actions actions = %+v, want exactly one fishhawk_start_run", out.NextActions.Actions)
	}
	if out.NextActions.Actions[0].Params["trigger_ref"] != "issue:101" {
		t.Errorf("next_actions params = %v, want trigger_ref issue:101", out.NextActions.Actions[0].Params)
	}

	// --- arm 2: a terminal-FAILED campaign WITH a restartable item -> start_run ---
	failedID := seed(t, campaign.StateFailed, map[string]campaign.ItemState{
		"issue:200": campaign.ItemStateSucceeded,
		"issue:201": campaign.ItemStateFailed,
	})
	_, out2, err := r.getCampaignStatus(ctx, nil, GetCampaignStatusInput{CampaignID: failedID.String()})
	if err != nil {
		t.Fatalf("getCampaignStatus (failed+restartable): %v", err)
	}
	if out2.NextAction.Action != "start_run" || out2.NextAction.IssueRef != "issue:201" {
		t.Fatalf("decoded next_action = %+v, want start_run issue:201 (the reopen keeps this arm legal)", out2.NextAction)
	}
	if !strings.Contains(out2.NextAction.Detail, "REOPENS") {
		t.Errorf("decoded detail = %q, want the reopen-flavoured detail", out2.NextAction.Detail)
	}
	// The restartable item is folded into the wire cancelled slice, so the
	// classifier routes it to the campaign-item restart verb.
	if out2.NextActions == nil || out2.NextActions.State != "campaign_start_run" {
		t.Fatalf("next_actions = %+v, want State campaign_start_run", out2.NextActions)
	}
	if len(out2.NextActions.Actions) != 1 || out2.NextActions.Actions[0].Action != "fishhawk_start_campaign_item_run" {
		t.Errorf("next_actions actions = %+v, want exactly one fishhawk_start_campaign_item_run", out2.NextActions.Actions)
	}

	// --- arm 3 (E32.11 / #1737): a stranded item WITH a linked run carries the
	// operator-gated filing suggestion LAST. This is the cross-boundary proof
	// that the campaign's ITEMS survive the wire into campaignNextActionsFor —
	// the seam the #1737 signature widening introduces. A per-layer unit passes
	// on a hand-built []CampaignItem while a dropped `run_id` json tag or an
	// items list that never reaches the classifier would leave the operator with
	// no filing move at all.
	linkedRun, err := runpkg.NewPostgresRepository(pool).CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create linked run: %v", err)
	}
	linkedID := seed(t, campaign.StateCancelled, map[string]campaign.ItemState{
		"issue:300": campaign.ItemStateSucceeded,
		"issue:301": campaign.ItemStateFailed,
	})
	items, err := repo.ListCampaignItemsForCampaign(ctx, linkedID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.IssueRef == "issue:301" {
			if _, err := repo.SetCampaignItemRun(ctx, it.ID, &linkedRun.ID); err != nil {
				t.Fatalf("link run to item: %v", err)
			}
		}
	}
	_, out3, err := r.getCampaignStatus(ctx, nil, GetCampaignStatusInput{CampaignID: linkedID.String()})
	if err != nil {
		t.Fatalf("getCampaignStatus (closed+linked run): %v", err)
	}
	if out3.NextActions == nil || out3.NextActions.State != "campaign_closed" {
		t.Fatalf("next_actions = %+v, want State campaign_closed", out3.NextActions)
	}
	acts := out3.NextActions.Actions
	if len(acts) != 2 || acts[0].Action != "fishhawk_start_run" || acts[1].Action != "fishhawk_report_product_issue" {
		t.Fatalf("next_actions actions = %+v, want [fishhawk_start_run fishhawk_report_product_issue] — the recovery move leads, filing is LAST", acts)
	}
	filing := acts[1]
	if filing.Params["run_id"] != linkedRun.ID.String() {
		t.Errorf("filing run_id = %q, want the STUCK ITEM'S own run id %q (not the campaign's)", filing.Params["run_id"], linkedRun.ID)
	}
	if filing.Params["kind"] != "bug" || filing.Consumes != consumesNone {
		t.Errorf("filing action = %+v, want kind=bug consumes=none", filing)
	}
	if !strings.Contains(filing.Reason, "issue:301") || !strings.Contains(filing.Reason, "failed") {
		t.Errorf("filing reason = %q, want it to name the stranded item and its state", filing.Reason)
	}
}

// TestStartCampaignItemRun_FloorTier_BoundedThroughHandler is the cross-boundary
// test (#2576 step 9): it drives fishhawk_start_campaign_item_run through the
// fakeBackend seam AND the MCP SDK in-memory transport at a floor-clamped
// budget, with an oversized issue_context and a multi-element depends_on, so the
// floor's per-field elisions block must pass the reflected output-schema
// validation INSIDE the handler and carry the campaign-items pointer built from
// the caller's campaign_id. Per-layer units would each pass while that seam
// broke.
func TestStartCampaignItemRun_FloorTier_BoundedThroughHandler(t *testing.T) {
	ctx := context.Background()
	fb, srv := newFakeBackend(t)
	campaignID := uuid.New()
	runID := uuid.NewString()

	// Seed a run whose issue_context is oversized AND whose retained strings are
	// inflated (floorForcingRun), so the within-row tiers cannot get it under the
	// convergence floor and the ladder is driven INTO the floor tier — where the
	// three-element depends_on is truncated and itemised.
	fb.startCampaignItemRunResp = StartCampaignItemRunResult{
		Run: floorForcingRun(runID),
		Item: CampaignItem{
			ID: uuid.NewString(), IssueRef: "kuhlman-labs/fishhawk#2576",
			DependsOn: []string{"kuhlman-labs/fishhawk#2508", "kuhlman-labs/fishhawk#2510", "kuhlman-labs/fishhawk#2546"},
			RunID:     runID, State: "running",
		},
	}

	cfg := config{backendURL: srv.URL, apiToken: "tok-test"}
	srvMCP := buildServer(cfg)
	registerTools(srvMCP, &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(map[string]string{runStatusBudgetEnvVar: "1"})})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srvMCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_start_campaign_item_run",
		Arguments: map[string]any{
			"campaign_id": campaignID.String(),
			"issue_ref":   "kuhlman-labs/fishhawk#2576",
			"workflow_id": "feature_change",
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "validating tool output") {
			t.Fatalf("the floor elisions block failed the SDK's output-schema validation: %v", err)
		}
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}

	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out StartCampaignItemRunOutput
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatalf("decode tool output %s: %v", structured, err)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("did not reach the floor tier through the handler: %s", structured)
	}
	if err := validateWireElisions(out.Elisions); err != nil {
		t.Errorf("round-tripped floor elisions violate their own classification: %v", err)
	}

	var saw bool
	want := "GET /v0/campaigns/" + campaignID.String() + "/items"
	for _, f := range out.Elisions.Fields {
		if f.Field == "item.depends_on" {
			saw = true
			if f.Class != string(classStored) || f.Pointer != want {
				t.Errorf("item.depends_on entry = {class %q pointer %q}, want stored + %q built from the caller's campaign_id", f.Class, f.Pointer, want)
			}
		}
	}
	if !saw {
		t.Errorf("no item.depends_on floor entry crossed the handler boundary: %s", structured)
	}
}
