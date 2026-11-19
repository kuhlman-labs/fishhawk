package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// proposeWaiveStub answers POST /v0/concerns/{concern_id}/waive-proposal with
// status/body, recording each request line and body.
type proposeWaiveStub struct {
	requests []string
	bodies   []string
	status   int
	body     string
}

func (b *proposeWaiveStub) resolver(t *testing.T) *runResolver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		b.bodies = append(b.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return newResolver(ts, nil)
}

// proposeWaive201 is a server-shaped 201 body (proposeWaiveResponse) for id.
func proposeWaive201(id uuid.UUID) string {
	return `{"proposal":{"proposal_sequence":42,"concern_id":"` + id.String() + `","stage_id":"stage-1",` +
		`"proposed_by":"operator-agent/1.0","proposed_by_actor_kind":"agent","proposed_at":"2026-11-18T12:00:00Z",` +
		`"judgement":"tradeoff","tradeoff_cost":"a fixture key stays in the diff","reason":"test fixture","evidence":"testdata/key.pem is a generated fixture",` +
		`"head_sha":"aaaa1111","check_key":"diff_secrets:testdata/key.pem:private_key","category":"diff_secrets_detected","severity":"high","agent_authored":true},` +
		`"proposed_by":"operator-agent/1.0","credential_kind":"operator_agent",` +
		`"next_step":"The proposal is pending and clears nothing. A human signed in to the Fishhawk Web UI confirms or rejects it from the attention queue's decision panel. There is no CLI or MCP confirm."}`
}

// TestProposeWaive_PostsExactPathAndBody: the tool POSTs the concern-scoped
// waive-proposal route with reason, evidence, judgement and the optional
// tradeoff_cost / expected_head_sha verbatim, and decodes every field of the
// backend's 201 body, including next_step and the acting-identity echo.
func TestProposeWaive_PostsExactPathAndBody(t *testing.T) {
	id := uuid.New()
	stub := &proposeWaiveStub{status: http.StatusCreated, body: proposeWaive201(id)}
	r := stub.resolver(t)

	_, out, err := r.proposeWaive(context.Background(), nil, ProposeWaiveInput{
		ConcernID: id.String(), Reason: "test fixture", Evidence: "testdata/key.pem is a generated fixture",
		Judgement: "tradeoff", TradeoffCost: "a fixture key stays in the diff", ExpectedHeadSHA: "aaaa1111",
	})
	if err != nil {
		t.Fatalf("proposeWaive: %v", err)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "POST /v0/concerns/"+id.String()+"/waive-proposal" {
		t.Fatalf("requests = %v, want one POST to the waive-proposal route", stub.requests)
	}
	if want := `{"reason":"test fixture","evidence":"testdata/key.pem is a generated fixture","judgement":"tradeoff",` +
		`"tradeoff_cost":"a fixture key stays in the diff","expected_head_sha":"aaaa1111"}`; stub.bodies[0] != want {
		t.Errorf("sent body = %s, want %s", stub.bodies[0], want)
	}
	want := WaiveProposal{
		ProposalSequence: 42, ConcernID: id.String(), StageID: "stage-1",
		ProposedBy: "operator-agent/1.0", ProposedByActorKind: "agent",
		ProposedAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
		Judgement:  "tradeoff", TradeoffCost: "a fixture key stays in the diff",
		Reason: "test fixture", Evidence: "testdata/key.pem is a generated fixture",
		HeadSHA: "aaaa1111", CheckKey: "diff_secrets:testdata/key.pem:private_key",
		Category: "diff_secrets_detected", Severity: "high", AgentAuthored: true,
	}
	if out.Proposal != want {
		t.Errorf("decoded proposal = %+v, want %+v", out.Proposal, want)
	}
	if out.ProposedBy != "operator-agent/1.0" || out.CredentialKind != "operator_agent" {
		t.Errorf("identity echo = (%q, %q), want the backend's proposed_by and credential_kind", out.ProposedBy, out.CredentialKind)
	}
	for _, needle := range []string{"clears nothing", "Fishhawk Web UI", "no CLI or MCP confirm"} {
		if !strings.Contains(out.NextStep, needle) {
			t.Errorf("next_step %q does not carry %q", out.NextStep, needle)
		}
	}
}

// TestProposeWaive_OmitsBlankOptionalKeys: blank tradeoff_cost and
// expected_head_sha never reach the backend as sent-but-empty keys, while a
// blank judgement IS sent so the backend, not the tool, refuses it.
func TestProposeWaive_OmitsBlankOptionalKeys(t *testing.T) {
	id := uuid.New()
	stub := &proposeWaiveStub{status: http.StatusCreated, body: proposeWaive201(id)}
	r := stub.resolver(t)
	if _, _, err := r.proposeWaive(context.Background(), nil, ProposeWaiveInput{
		ConcernID: id.String(), Reason: "r", Evidence: "e",
	}); err != nil {
		t.Fatalf("proposeWaive: %v", err)
	}
	if want := `{"reason":"r","evidence":"e","judgement":""}`; len(stub.bodies) != 1 || stub.bodies[0] != want {
		t.Errorf("sent bodies = %v, want exactly %s", stub.bodies, want)
	}
}

// TestProposeWaive_PassesRefusalCodesThrough: every backend refusal reaches the
// caller with its code, and the tool adds no local pre-check beyond the UUID
// (each case is exactly one backend request).
func TestProposeWaive_PassesRefusalCodesThrough(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusForbidden, "run_token_forbidden"},
		{http.StatusBadRequest, "waive_proposal_contains_credential"},
		{http.StatusConflict, "waive_proposal_exists"},
		{http.StatusConflict, "waive_proposal_head_moved"},
		{http.StatusUnprocessableEntity, "concern_not_server_check"},
		{http.StatusServiceUnavailable, "waive_proposal_atomic_append_unavailable"},
		{http.StatusBadRequest, "validation_failed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			stub := &proposeWaiveStub{
				status: tc.status,
				body:   `{"error":{"code":"` + tc.code + `","message":"refused by the backend"}}`,
			}
			r := stub.resolver(t)
			_, _, err := r.proposeWaive(context.Background(), nil, ProposeWaiveInput{
				ConcernID: uuid.NewString(), Reason: "r", Evidence: "e", Judgement: "no_defect",
			})
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Errorf("err = %v, want the backend's %s surfaced", err, tc.code)
			}
			if len(stub.requests) != 1 {
				t.Errorf("backend requests = %d, want 1 (no local pre-check)", len(stub.requests))
			}
		})
	}
}

// TestProposeWaive_InvalidConcernIDNoHop: a malformed concern_id is refused
// before the HTTP hop.
func TestProposeWaive_InvalidConcernIDNoHop(t *testing.T) {
	stub := &proposeWaiveStub{status: http.StatusCreated, body: proposeWaive201(uuid.New())}
	r := stub.resolver(t)
	_, _, err := r.proposeWaive(context.Background(), nil, ProposeWaiveInput{ConcernID: "not-a-uuid", Reason: "r", Evidence: "e"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want the UUID refusal", err)
	}
	if len(stub.requests) != 0 {
		t.Errorf("backend requests = %d, want 0", len(stub.requests))
	}
}

// TestProposeWaive_SchemaRequiredFields: concern_id, reason and evidence are
// schema-required; judgement and tradeoff_cost stay omitempty (the backend is
// the single refusal authority, as for every concern judgement verb), and
// expected_head_sha is optional.
func TestProposeWaive_SchemaRequiredFields(t *testing.T) {
	schema, err := jsonschema.For[ProposeWaiveInput](nil)
	if err != nil {
		t.Fatalf("infer schema: %v", err)
	}
	required := map[string]bool{}
	for _, k := range schema.Required {
		required[k] = true
	}
	for _, k := range []string{"concern_id", "reason", "evidence"} {
		if !required[k] {
			t.Errorf("%q not in the required list %v", k, schema.Required)
		}
	}
	for _, k := range []string{"judgement", "tradeoff_cost", "expected_head_sha"} {
		if required[k] {
			t.Errorf("%q is schema-required (%v); it must stay optional", k, schema.Required)
		}
		if schema.Properties[k] == nil {
			t.Errorf("schema has no %q property", k)
		}
	}
}

// TestProposeWaive_NoConfirmOrRejectTool pins ADR-143 A1's channel rule on the
// wire: fishhawk_propose_waive is registered with a description stating the
// contract, and NO tool confirms or rejects a waive proposal (both are Web UI
// session-only D2 verbs).
func TestProposeWaive_NoConfirmOrRejectTool(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()
	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var desc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_propose_waive" {
			desc = tool.Description
		}
		if strings.Contains(tool.Name, "waive_proposal") || strings.Contains(tool.Name, "confirm_waive") || strings.Contains(tool.Name, "reject_waive") {
			t.Errorf("tool %q decides a waive proposal; confirm and reject are Web UI session-only (ADR-143 A1)", tool.Name)
		}
	}
	if desc == "" {
		t.Fatal("fishhawk_propose_waive is not registered")
	}
	flat := strings.Join(strings.Fields(desc), " ")
	for _, needle := range []string{
		"It clears nothing",
		"NEVER paste the matched bytes",
		"waive_proposal_contains_credential",
		"One live proposal per concern",
		"A moved head drops it",
		"run_token_forbidden",
		"Only a human signed in to the Fishhawk Web UI confirms or rejects it",
		"There is no MCP or CLI confirm",
		"agent_alternative",
	} {
		if !strings.Contains(flat, needle) {
			t.Errorf("fishhawk_propose_waive description missing %q:\n%s", needle, desc)
		}
	}
}

// TestWaiveProposalMirrors_WireShape pins the two client mirrors of the
// backend's read surfaces against raw server-shaped bytes: the gate view's
// open[].waive_proposal block (waiveProposalView) and the run read's
// waive_proposals list (runWaiveProposalItem). A json tag that drifts from the
// backend decodes to its zero value silently; this catches it.
func TestWaiveProposalMirrors_WireShape(t *testing.T) {
	var gv GateViewConcern
	if err := json.Unmarshal([]byte(`{"id":"c1","state":"raised","judgement":"unlabelled","waive_proposal":{"proposal_sequence":7,"concern_id":"c1","stage_id":"s1",`+
		`"proposed_by":"operator-agent/1.0","proposed_by_actor_kind":"agent","proposed_at":"2026-11-18T12:00:00Z","judgement":"no_defect",`+
		`"reason":"r","evidence":"e","head_sha":"abc","check_key":"k","category":"diff_secrets_detected","severity":"high","agent_authored":true}}`), &gv); err != nil {
		t.Fatalf("decode gate-view concern: %v", err)
	}
	p := gv.WaiveProposal
	if p == nil || p.ProposalSequence != 7 || p.ConcernID != "c1" || p.StageID != "s1" || p.ProposedBy != "operator-agent/1.0" ||
		p.ProposedByActorKind != "agent" || p.ProposedAt.IsZero() || p.Judgement != "no_defect" || p.Reason != "r" || p.Evidence != "e" ||
		p.HeadSHA != "abc" || p.CheckKey != "k" || p.Category != "diff_secrets_detected" || p.Severity != "high" || !p.AgentAuthored {
		t.Errorf("decoded waive_proposal = %+v, want every server field", p)
	}

	var run Run
	if err := json.Unmarshal([]byte(`{"id":"r1","waive_proposals":[{"concern_id":"c1","stage_id":"s1","proposal_sequence":7,"proposed_by":"operator-agent/1.0","judgement":"no_defect","head_sha":"abc"}]}`), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	want := runWaiveProposal{ConcernID: "c1", StageID: "s1", ProposalSequence: 7, ProposedBy: "operator-agent/1.0", Judgement: "no_defect", HeadSHA: "abc"}
	if len(run.WaiveProposals) != 1 || run.WaiveProposals[0] != want {
		t.Errorf("decoded waive_proposals = %+v, want [%+v]", run.WaiveProposals, want)
	}
}
