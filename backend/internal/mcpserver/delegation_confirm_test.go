package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
)

const delegationConfirmTestRepo = "kuhlman-labs/confirm-mcp"

// delegationConfirmStub answers every GET with getBody and every POST with
// postStatus/postBody, recording each request line and body.
type delegationConfirmStub struct {
	requests   []string
	bodies     []string
	getBody    string
	postStatus int
	postBody   string
}

func (b *delegationConfirmStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		b.bodies = append(b.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, b.getBody)
			return
		}
		if b.postStatus != 0 {
			w.WriteHeader(b.postStatus)
		}
		_, _ = io.WriteString(w, b.postBody)
	}))
	t.Cleanup(ts.Close)
	return ts
}

const delegationConfirmReadBody = `{"repo":"` + delegationConfirmTestRepo + `","source":"run_cache","workflow_sha":"abc","captain":"github:alice","seat_sequence":7,` +
	`"workflows":[{"workflow":"ship","status":"confirmed","current_content_hash":"h1","confirmation":{"subject":"github:alice","content_hash":"h1","sequence":9,"entry_hash":"e9","at":"2026-09-01T00:00:00Z"}},` +
	`{"workflow":"guarded","status":"unconfirmed","reason":"handover","current_content_hash":"h2"}],` +
	`"unconfirmed_workflows":["guarded"],"skipped_entries":0,"ignored_entries":1}`

const delegationConfirmVerbBody = `{"repo":"` + delegationConfirmTestRepo + `","workflow":{"workflow":"ship","status":"confirmed","current_content_hash":"h1"},` +
	`"event":{"sequence":10,"entry_hash":"e10","category":"delegation_confirmed","at":"2026-09-01T00:00:00Z","payload":{"workflow":"ship"}}}`

// TestDelegationConfirmTool_ActionSetIsClosed is the schema test the issue
// asks for: the advertised action enum is EXACTLY {read, confirm, lower} (and
// equals delegationconfirm.Actions), and both tier-bearing fields are
// restricted to {low, medium} so the schema cannot name the top tier.
func TestDelegationConfirmTool_ActionSetIsClosed(t *testing.T) {
	raw, err := json.Marshal(delegationConfirmInputSchema())
	if err != nil {
		t.Fatal(err)
	}
	type prop struct {
		Enum       []string        `json:"enum"`
		Properties map[string]prop `json:"properties"`
	}
	var s struct {
		Properties map[string]prop `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Properties["action"].Enum, ","); got != "read,confirm,lower" {
		t.Errorf("action enum = %q, want read,confirm,lower", got)
	}
	if got := strings.Join(delegationconfirm.Actions(), ","); got != "read,confirm,lower" {
		t.Errorf("delegationconfirm.Actions() = %q, want read,confirm,lower", got)
	}
	if got := strings.Join(s.Properties["proposed_tier"].Enum, ","); got != "low,medium" {
		t.Errorf("proposed_tier enum = %q, want low,medium", got)
	}
	if got := strings.Join(s.Properties["proposed_escalation"].Properties["max_autonomy"].Enum, ","); got != "low,medium" {
		t.Errorf("proposed_escalation.max_autonomy enum = %q, want low,medium", got)
	}
	for name := range s.Properties {
		if strings.Contains(name, "raise") || strings.Contains(name, "approval") {
			t.Errorf("input schema carries a raise-shaped property %q", name)
		}
	}
}

// TestDelegationConfirmTool_ReadGETsConfirmation: the default action GETs the
// confirmation route with source/ref as query and returns the read only.
func TestDelegationConfirmTool_ReadGETsConfirmation(t *testing.T) {
	b := &delegationConfirmStub{getBody: delegationConfirmReadBody}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.delegationConfirm(context.Background(), nil, delegationConfirmInput{
		Repo: delegationConfirmTestRepo, Source: "run_cache", Ref: "main"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "GET /v0/repos/kuhlman-labs/confirm-mcp/delegation/confirmation?ref=main&source=run_cache"
	if len(b.requests) != 1 || b.requests[0] != want {
		t.Errorf("requests = %v, want [%s]", b.requests, want)
	}
	if out.Confirmation == nil || out.Result != nil || out.Elisions != nil {
		t.Fatalf("output = %+v, want the read only, unbounded", out)
	}
	if strings.Join(out.Confirmation.UnconfirmedWorkflows, ",") != "guarded" || out.Confirmation.IgnoredEntries != 1 ||
		len(out.Confirmation.Workflows) != 2 || out.Confirmation.Workflows[0].Confirmation == nil {
		t.Errorf("confirmation = %+v", out.Confirmation)
	}
}

// TestDelegationConfirmTool_ConfirmPOSTsBoundHash: confirm POSTs the exact
// workflow + content_hash and never sets delegated.
func TestDelegationConfirmTool_ConfirmPOSTsBoundHash(t *testing.T) {
	b := &delegationConfirmStub{postBody: delegationConfirmVerbBody}
	r := digestResolver(b.serve(t).URL, map[string]string{"GITHUB_REPOSITORY": delegationConfirmTestRepo})
	_, out, err := r.delegationConfirm(context.Background(), nil, delegationConfirmInput{
		Action: "confirm", Workflow: " ship ", ContentHash: "h1", Source: "run_cache"})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(b.requests) != 1 || b.requests[0] != "POST /v0/repos/kuhlman-labs/confirm-mcp/delegation/confirm" {
		t.Fatalf("requests = %v", b.requests)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(b.bodies[0]), &body)
	if body["workflow"] != "ship" || body["content_hash"] != "h1" || body["source"] != "run_cache" {
		t.Errorf("body = %v", body)
	}
	if _, ok := body["delegated"]; ok {
		t.Errorf("body sets delegated: %v", body)
	}
	if out.Result == nil || out.Result.Event.Category != delegationconfirm.CategoryDelegationConfirmed || out.Confirmation != nil {
		t.Errorf("output = %+v", out)
	}
}

// TestDelegationConfirmTool_LowerPOSTsProposal: lower POSTs every proposal
// field and surfaces the filed work item.
func TestDelegationConfirmTool_LowerPOSTsProposal(t *testing.T) {
	b := &delegationConfirmStub{postBody: `{"repo":"` + delegationConfirmTestRepo + `","workflow":{"workflow":"ship","status":"unconfirmed","reason":"handover"},` +
		`"event":{"sequence":11,"entry_hash":"e11","category":"delegation_lower_proposed","at":"2026-09-01T00:00:00Z"},` +
		`"filed":{"number":42,"url":"https://example.test/42","title":"lower","applied_labels":["autonomy:low"]}}`}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.delegationConfirm(context.Background(), nil, delegationConfirmInput{
		Repo: delegationConfirmTestRepo, Action: "lower", Workflow: "ship", ProposedTier: "medium",
		ProposedEscalation: &delegationconfirm.Escalation{Paths: []string{"docs/**"}, MaxAutonomy: "low"},
		Reason:             "new captain", TitleVars: map[string]string{"epic": "76"}, Labels: []string{"area:backend"}})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if len(b.requests) != 1 || b.requests[0] != "POST /v0/repos/kuhlman-labs/confirm-mcp/delegation/lower" {
		t.Fatalf("requests = %v", b.requests)
	}
	for _, want := range []string{`"proposed_tier":"medium"`, `"max_autonomy":"low"`, `"reason":"new captain"`, `"epic":"76"`, `"area:backend"`} {
		if !strings.Contains(b.bodies[0], want) {
			t.Errorf("body %s lacks %s", b.bodies[0], want)
		}
	}
	if out.Result == nil || out.Result.Filed == nil || out.Result.Filed.Number != 42 {
		t.Errorf("output = %+v, want the filed item", out)
	}
}

// TestDelegationConfirmTool_LocalRefusals: every local refusal fires BEFORE
// any request — one case per branch.
func TestDelegationConfirmTool_LocalRefusals(t *testing.T) {
	esc := &delegationconfirm.Escalation{Paths: []string{"x"}, MaxAutonomy: "low"}
	for _, tc := range []struct {
		name string
		in   delegationConfirmInput
		want string
	}{
		{"no repo", delegationConfirmInput{}, "repo is required"},
		{"raise action", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "raise"}, "unknown action"},
		{"workflow on read", delegationConfirmInput{Repo: delegationConfirmTestRepo, Workflow: "ship"}, "workflow applies only"},
		{"content_hash on read", delegationConfirmInput{Repo: delegationConfirmTestRepo, ContentHash: "h"}, "content_hash applies only to action=confirm"},
		{"reason on read", delegationConfirmInput{Repo: delegationConfirmTestRepo, Reason: "r"}, "reason applies only to action=lower"},
		{"tier on confirm", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "confirm", Workflow: "ship", ContentHash: "h", ProposedTier: "low"}, "proposed_tier applies only to action=lower"},
		{"escalation on confirm", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "confirm", Workflow: "ship", ContentHash: "h", ProposedEscalation: esc}, "proposed_escalation applies only"},
		{"labels on confirm", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "confirm", Workflow: "ship", Labels: []string{"a"}}, "labels applies only"},
		{"title_vars on confirm", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "confirm", Workflow: "ship", TitleVars: map[string]string{"n": "1"}}, "title_vars applies only"},
		{"parent_epic on confirm", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "confirm", Workflow: "ship", ParentEpic: "#1"}, "parent_epic applies only"},
		{"hash on lower", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "lower", Workflow: "ship", ContentHash: "h", Reason: "r"}, "content_hash applies only to action=confirm"},
		{"no workflow on confirm", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "confirm", ContentHash: "h"}, "workflow is required for action=confirm"},
		{"no workflow on lower", delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "lower", Reason: "r"}, "workflow is required for action=lower"},
		{"malformed repo", delegationConfirmInput{Repo: "noslash"}, "repo must be owner/name"},
		{"malformed repo on a write", delegationConfirmInput{Repo: "noslash", Action: "confirm", Workflow: "ship", ContentHash: "h"}, "repo must be owner/name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &delegationConfirmStub{}
			r := digestResolver(b.serve(t).URL, nil)
			_, _, err := r.delegationConfirm(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(b.requests) != 0 {
				t.Errorf("requests = %v, want none (refused locally)", b.requests)
			}
		})
	}
}

// TestDelegationConfirmTool_BackendRefusalIsToolError: a backend refusal
// surfaces as a tool error carrying its code — never an empty result that
// reads as success — on both write actions and the read.
func TestDelegationConfirmTool_BackendRefusalIsToolError(t *testing.T) {
	for _, tc := range []struct {
		action, code string
		status       int
	}{
		{"confirm", "delegation_agent_identity_refused", http.StatusForbidden},
		{"confirm", "delegation_hash_stale", http.StatusConflict},
		{"lower", "delegation_raise_refused", http.StatusBadRequest},
	} {
		b := &delegationConfirmStub{postStatus: tc.status,
			postBody: fmt.Sprintf(`{"error":{"code":%q,"message":"refused"}}`, tc.code)}
		r := digestResolver(b.serve(t).URL, nil)
		in := delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: tc.action, Workflow: "ship", ContentHash: "h"}
		if tc.action == "lower" {
			in = delegationConfirmInput{Repo: delegationConfirmTestRepo, Action: "lower", Workflow: "ship", ProposedTier: "medium", Reason: "r"}
		}
		_, out, err := r.delegationConfirm(context.Background(), nil, in)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != tc.code || ae.StatusCode != tc.status {
			t.Errorf("%s: err = %v, want *apiError %d %s", tc.action, err, tc.status, tc.code)
		}
		if out.Result != nil || out.Confirmation != nil {
			t.Errorf("%s: output = %+v, want empty on refusal", tc.action, out)
		}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = io.WriteString(w, `{"error":{"code":"delegation_confirm_unconfigured","message":"x"}}`)
	}))
	t.Cleanup(ts.Close)
	_, _, err := digestResolver(ts.URL, nil).delegationConfirm(context.Background(), nil, delegationConfirmInput{Repo: delegationConfirmTestRepo})
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "delegation_confirm_unconfigured" {
		t.Errorf("read err = %v, want delegation_confirm_unconfigured", err)
	}
}

// bigConfirmationRead is a read with n workflows, each carrying a confirmation
// detail block, all unconfirmed ids listed.
func bigConfirmationRead(n int) *delegationConfirmationState {
	st := &delegationConfirmationState{Repo: delegationConfirmTestRepo, Source: "ref", SeatSequence: 3}
	for i := 0; i < n; i++ {
		id := "workflow-" + strconv.Itoa(i) + strings.Repeat("x", 40)
		st.Workflows = append(st.Workflows, delegationconfirm.WorkflowStatus{
			Workflow: id, Status: delegationconfirm.StatusUnconfirmed, Reason: delegationconfirm.ReasonHashStale,
			CurrentContentHash: strings.Repeat("c", 64),
			Confirmation:       &delegationconfirm.Confirmation{Subject: "github:alice", ContentHash: strings.Repeat("d", 64), EntryHash: strings.Repeat("e", 64)},
		})
		st.UnconfirmedWorkflows = append(st.UnconfirmedWorkflows, id)
	}
	return st
}

// TestDelegationConfirmTool_BoundsEveryTier: each ladder tier fits the budget
// AND marks its truncation, and B1/B2 keep unconfirmed_workflows whole.
func TestDelegationConfirmTool_BoundsEveryTier(t *testing.T) {
	budget := responseBudget{bytes: mcpConvergenceFloorBytes, source: "configured"}

	// B1: detail blocks alone push it over.
	b1 := bigConfirmationRead(12)
	out, err := boundDelegationConfirmOutput(delegationConfirmOutput{Confirmation: b1}, budget)
	if err != nil {
		t.Fatal(err)
	}
	assertDelegationConfirmBounded(t, "B1", out, budget.bytes)
	if out.Elisions.Tier != "B1" || len(out.Confirmation.UnconfirmedWorkflows) != 12 || len(out.Confirmation.Workflows) != 12 {
		t.Errorf("B1 = tier %s, %d ids, %d workflows; want B1 keeping all 12", out.Elisions.Tier, len(out.Confirmation.UnconfirmedWorkflows), len(out.Confirmation.Workflows))
	}
	for _, ws := range out.Confirmation.Workflows {
		if ws.Confirmation != nil || ws.Status == "" {
			t.Errorf("B1 workflow = %+v, want detail dropped and status kept", ws)
		}
	}

	// B2: the workflow list itself is too long.
	out, err = boundDelegationConfirmOutput(delegationConfirmOutput{Confirmation: bigConfirmationRead(30)}, budget)
	if err != nil {
		t.Fatal(err)
	}
	assertDelegationConfirmBounded(t, "B2", out, budget.bytes)
	if out.Elisions.Tier != "B2" || len(out.Confirmation.UnconfirmedWorkflows) != 30 || len(out.Confirmation.Workflows) >= 30 {
		t.Errorf("B2 = tier %s, %d ids, %d workflows", out.Elisions.Tier, len(out.Confirmation.UnconfirmedWorkflows), len(out.Confirmation.Workflows))
	}

	// FLOOR: the unconfirmed id list alone exceeds the budget.
	out, err = boundDelegationConfirmOutput(delegationConfirmOutput{Confirmation: bigConfirmationRead(400)}, budget)
	if err != nil {
		t.Fatal(err)
	}
	assertDelegationConfirmBounded(t, "floor", out, budget.bytes)
	if out.Elisions.Tier != floorTierName || len(out.Confirmation.Workflows) != 0 || len(out.Confirmation.UnconfirmedWorkflows) >= 400 {
		t.Errorf("floor = tier %s, %d ids, %d workflows", out.Elisions.Tier, len(out.Confirmation.UnconfirmedWorkflows), len(out.Confirmation.Workflows))
	}

	// A verb result with an oversized event payload.
	res := &delegationConfirmVerbResult{Repo: delegationConfirmTestRepo,
		Workflow: delegationconfirm.WorkflowStatus{Workflow: "ship", Status: "unconfirmed", Reason: "handover"},
		Event:    delegationConfirmEvent{Sequence: 4, Category: delegationconfirm.CategoryDelegationLowerProposed, Payload: map[string]any{"reason": strings.Repeat("r", 8000)}},
		Filed:    &delegationConfirmFiled{Number: 1, URL: "u", Title: "t"}}
	out, err = boundDelegationConfirmOutput(delegationConfirmOutput{Result: res}, budget)
	if err != nil {
		t.Fatal(err)
	}
	assertDelegationConfirmBounded(t, "verb", out, budget.bytes)
	if out.Result.Event.Payload != nil || out.Result.Event.Sequence != 4 || out.Result.Workflow.Status != "unconfirmed" || out.Result.Filed == nil {
		t.Errorf("bounded verb = %+v, want the payload dropped and the identity kept", out.Result)
	}
}

func assertDelegationConfirmBounded(t *testing.T, tier string, out delegationConfirmOutput, budget int) {
	t.Helper()
	n, err := marshalledLen(out)
	if err != nil {
		t.Fatal(err)
	}
	if n > budget {
		t.Errorf("%s: %d bytes > budget %d", tier, n, budget)
	}
	if out.Elisions == nil || out.Elisions.Tier == "" || len(out.Elisions.Fields) == 0 {
		t.Fatalf("%s: truncation not marked: %+v", tier, out.Elisions)
	}
}

// TestDelegationConfirmTool_RegisteredSchemaRefusesRaise drives the
// REGISTERED tool over a real MCP session: a well-formed read succeeds and
// returns structured output, while action=raise and proposed_tier=high are
// refused by the advertised schema before the handler runs.
func TestDelegationConfirmTool_RegisteredSchemaRefusesRaise(t *testing.T) {
	b := &delegationConfirmStub{getBody: delegationConfirmReadBody}
	url := b.serve(t).URL
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerDelegationConfirm(server, digestResolver(url, nil))
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ok, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_delegation_confirm",
		Arguments: map[string]any{"repo": delegationConfirmTestRepo}})
	if err != nil || ok.IsError || ok.StructuredContent == nil {
		t.Fatalf("read call = %+v, %v; want structured success", ok, err)
	}
	for _, args := range []map[string]any{
		{"repo": delegationConfirmTestRepo, "action": "raise"},
		{"repo": delegationConfirmTestRepo, "action": "lower", "workflow": "ship", "reason": "r", "proposed_tier": "high"},
		{"repo": delegationConfirmTestRepo, "action": "lower", "workflow": "ship", "reason": "r",
			"proposed_escalation": map[string]any{"paths": []string{"x"}, "max_autonomy": "high"}},
	} {
		before := len(b.requests)
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_delegation_confirm", Arguments: args})
		if err == nil && (res == nil || !res.IsError) {
			t.Errorf("args %v were accepted; want a schema refusal", args)
		}
		if len(b.requests) != before {
			t.Errorf("args %v reached the backend: %v", args, b.requests[before:])
		}
	}
}
